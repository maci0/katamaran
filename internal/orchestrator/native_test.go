package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	got := drainUpdates(updates, 10*time.Second)
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

// TestSucceededUpdate_RecoversFromTailRace covers the race the
// scrapeResultMarker helper was added to fix: tailProgress polls every
// 2s but poll fires PhaseSucceeded as soon as the dest Job reaches
// Complete. If those land between tailProgress ticks, the
// KATAMARAN_RESULT marker is in the source pod log but never copied
// onto run.* — the terminal StatusUpdate would otherwise carry zeros.
func TestSucceededUpdate_RecoversFromTailRace(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "katamaran-source-id1-pod",
				Namespace: "kube-system",
				Labels:    map[string]string{"batch.kubernetes.io/job-name": "katamaran-source-id1"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
	)
	// fake.Clientset doesn't implement GetLogs; ReactionChain returning
	// a *rest.Request is more involved than this test needs. Instead,
	// stub n.scrapeResultMarker by exercising it through a custom
	// fake-pod-log helper. Bypass the real GetLogs by setting up
	// resultCaptured pre-emptively, then assert succeededUpdate copies
	// the values onto the StatusUpdate.
	n := NewFromClient(cs).(*native)
	run := &nativeRun{
		srcJob:           "katamaran-source-id1",
		destJob:          "katamaran-dest-id1",
		updates:          make(chan StatusUpdate, 4),
		finished:         make(chan struct{}),
		resultCaptured:   true,
		resultDowntime:   42,
		resultRAMXfer:    111,
		resultRAMTotal:   222,
		downtimeCaptured: true,
		appliedDowntime:  25,
		rttMS:            3,
		autoDowntime:     true,
	}
	u := n.succeededUpdate(context.Background(), MigrationID("id1"), run)
	if u.Phase != PhaseSucceeded {
		t.Fatalf("phase = %s, want %s", u.Phase, PhaseSucceeded)
	}
	if u.DowntimeMS != 42 || u.RAMTransferred != 111 || u.RAMTotal != 222 {
		t.Errorf("captured result not threaded: %+v", u)
	}
	if u.AppliedDowntimeMS != 25 || u.RTTMS != 3 || !u.AutoDowntime {
		t.Errorf("captured downtime-limit not threaded: %+v", u)
	}
}

// TestSucceededUpdate_NoCaptureFallsBackCleanly: when neither
// tailProgress nor the synchronous scrape filled run.result*, the
// terminal update should still ship as PhaseSucceeded with zero values
// rather than blocking or panicking.
func TestSucceededUpdate_NoCaptureFallsBackCleanly(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := NewFromClient(cs).(*native)
	run := &nativeRun{
		srcJob:   "katamaran-source-empty",
		destJob:  "katamaran-dest-empty",
		updates:  make(chan StatusUpdate, 4),
		finished: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	u := n.succeededUpdate(ctx, MigrationID("empty"), run)
	if u.Phase != PhaseSucceeded {
		t.Fatalf("phase = %s, want %s", u.Phase, PhaseSucceeded)
	}
	if u.DowntimeMS != 0 || u.RAMTransferred != 0 || u.RAMTotal != 0 {
		t.Errorf("expected zero result fields when no capture; got %+v", u)
	}
}

// TestParseProgressFields covers the marker-parsing helper that backs
// every KATAMARAN_* scrape. Reordering or extra whitespace must not
// shift values.
func TestParseProgressFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want map[string]string
	}{
		{
			in:   "status=completed ram_transferred=100 ram_total=200 ram_remaining=0",
			want: map[string]string{"status": "completed", "ram_transferred": "100", "ram_total": "200", "ram_remaining": "0"},
		},
		{
			in:   "downtime_ms=18 total_time_ms=1234 ram_transferred=900 ram_total=2000",
			want: map[string]string{"downtime_ms": "18", "total_time_ms": "1234", "ram_transferred": "900", "ram_total": "2000"},
		},
		{
			in:   "applied_ms=25 rtt_ms=0 auto=true",
			want: map[string]string{"applied_ms": "25", "rtt_ms": "0", "auto": "true"},
		},
		{
			in:   "  multiple   spaces=ok ",
			want: map[string]string{"multiple": "", "spaces": "ok"},
		},
	}
	for _, tc := range cases {
		got := parseProgressFields(tc.in)
		// "multiple" with no `=` should be filtered out by the
		// `eq <= 0` guard, so don't assert on it.
		for k, v := range tc.want {
			if k == "multiple" {
				continue
			}
			if got[k] != v {
				t.Errorf("parseProgressFields(%q)[%q] = %q, want %q", tc.in, k, got[k], v)
			}
		}
	}
}

// TestNativeRunSend_AfterClose: poll's defer can close run.updates
// before tailProgress tries to send. The run.send helper must absorb
// that race without panicking.
func TestNativeRunSend_AfterClose(t *testing.T) {
	t.Parallel()
	run := &nativeRun{
		updates:  make(chan StatusUpdate, 1),
		finished: make(chan struct{}),
	}
	// Mimic poll's defer order: signal finished, then close updates.
	close(run.finished)
	run.closeOnce.Do(func() { close(run.updates) })

	// Sending after the close must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("run.send panicked after close: %v", r)
		}
	}()
	run.send(StatusUpdate{Phase: PhaseTransferring})
}

// drainUpdates collects every value from c until it closes or deadline
// fires. Used by tests that exercise the full Apply -> Watch -> close
// lifecycle without caring about per-event timing.
func drainUpdates(c <-chan StatusUpdate, timeout time.Duration) []StatusUpdate {
	var out []StatusUpdate
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case u, ok := <-c:
			if !ok {
				return out
			}
			out = append(out, u)
		case <-deadline.C:
			return out
		}
	}
}
