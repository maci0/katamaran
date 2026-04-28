package orchestrator

import (
	"context"
	"errors"
	"strings"
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
	hasSrc, hasDest := false, false
	for n := range names {
		if strings.HasPrefix(n, "katamaran-source-") {
			hasSrc = true
		}
		if strings.HasPrefix(n, "katamaran-dest-") {
			hasDest = true
		}
	}
	if !hasSrc || !hasDest {
		t.Fatalf("expected katamaran-source-<id> and katamaran-dest-<id>, got %v", names)
	}
}

func TestNative_Watch_TerminalSucceeded(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	// React to Get on either katamaran-{source,dest} by returning a Job
	// whose conditions include JobComplete=True. fake.Clientset doesn't
	// auto-update Job status from controller activity — we have to
	// fabricate it. The Native.poll loop reports PhaseSucceeded as soon as
	// the DEST Job is Complete (regardless of source's exit status).
	cs.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		name := ga.GetName()
		if !strings.HasPrefix(name, "katamaran-source-") && !strings.HasPrefix(name, "katamaran-dest-") {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
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
