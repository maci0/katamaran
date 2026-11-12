package orchestrator

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// The source Job's Create can succeed on the API server and still report
// failure here: the response is lost and the retry sees AlreadyExists. That
// is the same submission, not a conflict, so Apply must keep both Jobs.
func TestNative_Apply_SourceAlreadyExistsKeepsDestJob(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	n.ids = func() MigrationID { return MigrationID("fixedid") }
	ctx := context.Background()

	// Only the dest Job exists up front: the duplicate-pod guard keys on the
	// source Job, so a pre-created source Job would make Apply join the
	// running migration and never reach the source Create this test covers.
	// The reactor lets the dest Create through (the guard finds nothing) and
	// reports AlreadyExists for the source Create, the response a retry sees
	// after the API server persisted the first attempt.
	cs.PrependReactor("create", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		create := action.(clienttesting.CreateAction)
		job := create.GetObject().(*batchv1.Job)
		if job.Labels["app.kubernetes.io/component"] != "source" {
			return false, nil, nil
		}
		persisted := job.DeepCopy()
		persisted.Namespace = DefaultJobNamespace
		if err := cs.Tracker().Add(persisted); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(batchv1.Resource("jobs"), job.Name)
	})

	id, err := n.Apply(ctx, validRequest())
	if err != nil {
		t.Fatalf("Apply with source job already present: %v", err)
	}
	if id != MigrationID("fixedid") {
		t.Fatalf("Apply id = %q, want fixedid", id)
	}
	for _, name := range []string{SourceJobName(id), DestJobName(id)} {
		if _, err := cs.BatchV1().Jobs(DefaultJobNamespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Fatalf("job %s missing after retried Apply: %v", name, err)
		}
	}
}

// A genuine source-create failure must still remove the dest Job, which is
// already running and would otherwise listen until activeDeadlineSeconds.
func TestNative_Apply_SourceCreateFailureRemovesDestJob(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		create := action.(clienttesting.CreateAction)
		job := create.GetObject().(*batchv1.Job)
		if job.Labels["app.kubernetes.io/component"] == "source" {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	n := newFromClient(cs)
	ctx := context.Background()

	if _, err := n.Apply(ctx, validRequest()); err == nil {
		t.Fatal("Apply must fail when the source job cannot be created")
	}
	jobs, err := cs.BatchV1().Jobs(DefaultJobNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("failed source create left %d job(s) behind: %+v", len(jobs.Items), jobs.Items)
	}
}
