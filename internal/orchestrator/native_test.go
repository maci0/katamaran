package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func validRequest() Request {
	return Request{
		SourceNode: "n1",
		DestNode:   "n2",
		DestIP:     "10.0.0.20",
		Image:      "katamaran:dev",
		SourcePod:  &PodRef{Namespace: "default", Name: "vm-a"},
	}
}

func TestNative_Apply_RejectsReplayCmdline(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := NewNativeFromClient(cs)
	req := validRequest()
	req.ReplayCmdline = true
	if _, err := n.Apply(context.Background(), req); !errors.Is(err, ErrReplayCmdlineNotSupported) {
		t.Fatalf("want ErrReplayCmdlineNotSupported, got %v", err)
	}
}

func TestNative_Apply_CreatesBothJobs(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := NewNativeFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if id == "" {
		t.Fatal("expected migration id")
	}
	jobs, err := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	names := map[string]bool{}
	for _, j := range jobs.Items {
		names[j.Name] = true
	}
	if !names["katamaran-source"] || !names["katamaran-dest"] {
		t.Fatalf("expected both jobs, got %v", names)
	}
}

func TestNative_Watch_TerminalSucceeded(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	// React to Get on katamaran-source by returning a Job whose conditions
	// include JobComplete=True. fake.Clientset doesn't auto-update Job status
	// from controller activity — we have to fabricate it.
	cs.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || ga.GetName() != "katamaran-source" {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "katamaran-source", Namespace: "kube-system"},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: "True"},
				},
			},
		}, nil
	})

	n := NewNativeFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	updates, err := n.Watch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got := drain(updates, 10*time.Second)
	if len(got) < 2 {
		t.Fatalf("want >=2 updates, got %d: %+v", len(got), got)
	}
	if got[0].Phase != PhaseSubmitted {
		t.Errorf("first phase = %s, want %s", got[0].Phase, PhaseSubmitted)
	}
	if got[len(got)-1].Phase != PhaseSucceeded {
		t.Errorf("last phase = %s, want %s", got[len(got)-1].Phase, PhaseSucceeded)
	}
}

func TestNative_Stop_DeletesJobs(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := NewNativeFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	jobs, _ := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatalf("expected jobs deleted, got %v", jobs.Items)
	}
}

func TestExpandShellVars(t *testing.T) {
	t.Parallel()
	got := expandShellVars("hello ${A} ${B}!", map[string]string{"A": "world", "B": "x"})
	if got != "hello world x!" {
		t.Fatalf("got %q", got)
	}
	if expandShellVars("${unknown}", nil) != "" {
		t.Fatal("expected unknown var to expand to empty")
	}
}
