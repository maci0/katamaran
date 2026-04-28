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
	n := NewFromClient(cs)
	req := validRequest()
	req.ReplayCmdline = true
	if _, err := n.Apply(context.Background(), req); !errors.Is(err, ErrReplayCmdlineNotSupported) {
		t.Fatalf("want ErrReplayCmdlineNotSupported, got %v", err)
	}
}

func TestNative_Apply_CreatesBothJobs(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := NewFromClient(cs)
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
	if len(jobs.Items) != 2 {
		t.Fatalf("expected exactly 2 jobs, got %d: %+v", len(jobs.Items), jobs.Items)
	}
	byComponent := map[string]batchv1.Job{}
	for _, j := range jobs.Items {
		component := j.Labels["app.kubernetes.io/component"]
		byComponent[component] = j
		if j.Labels[MigrationIDLabel] != string(id) {
			t.Fatalf("job %s migration label = %q, want %q", j.Name, j.Labels[MigrationIDLabel], id)
		}
	}
	src, hasSrc := byComponent["source"]
	dest, hasDest := byComponent["dest"]
	if !hasSrc || !hasDest {
		t.Fatalf("expected source and dest jobs, got components %v", byComponent)
	}
	if !strings.HasPrefix(src.Name, "katamaran-source-") || !strings.HasPrefix(dest.Name, "katamaran-dest-") {
		t.Fatalf("unexpected job names: source=%q dest=%q", src.Name, dest.Name)
	}
	if src.Spec.Template.Spec.NodeName != "n1" || dest.Spec.Template.Spec.NodeName != "n2" {
		t.Fatalf("jobs scheduled to wrong nodes: source=%q dest=%q", src.Spec.Template.Spec.NodeName, dest.Spec.Template.Spec.NodeName)
	}

	sourceCmd := jobCommand(t, src)
	for _, want := range []string{
		"--mode source",
		"--dest-ip 10.0.0.20",
		"--pod-name vm-a",
		"--pod-namespace default",
		"--multifd-channels 0",
	} {
		if !strings.Contains(sourceCmd, want) {
			t.Fatalf("source command missing %q: %s", want, sourceCmd)
		}
	}
	destCmd := jobCommand(t, dest)
	for _, want := range []string{"--mode dest", "--multifd-channels 0"} {
		if !strings.Contains(destCmd, want) {
			t.Fatalf("dest command missing %q: %s", want, destCmd)
		}
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

	n := NewFromClient(cs)
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
	n := NewFromClient(cs)
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

func jobCommand(t *testing.T, job batchv1.Job) string {
	t.Helper()
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("job %s containers = %d, want 1", job.Name, len(job.Spec.Template.Spec.Containers))
	}
	return strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
}
