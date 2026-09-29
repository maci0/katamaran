package orchestrator

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// replayPoll runs one scripted poll to completion and returns every
// StatusUpdate it emitted, When included.
//
// The script is keyed to poll-tick count rather than to elapsed time: the
// source Job reports Active on its first tick (PhaseTransferring), the dest
// Job reports Complete on its second (PhaseSucceeded), and the source pod
// log carries no KATAMARAN_RESULT marker, so the terminal update carries no
// downtime figures. Time itself is virtualized by the synctest bubble.
func replayPoll(t *testing.T) []StatusUpdate {
	t.Helper()
	const id = MigrationID("aaaa0000bbbb1111")
	srcJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SourceJobName(id),
			Namespace: DefaultJobNamespace,
			Labels:    map[string]string{MigrationIDLabel: string(id)},
		},
	}
	destJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DestJobName(id),
			Namespace: DefaultJobNamespace,
			Labels:    map[string]string{MigrationIDLabel: string(id)},
		},
	}
	srcPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "katamaran-source-pod",
			Namespace: DefaultJobNamespace,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": SourceJobName(id)},
		},
	}

	cs := fake.NewSimpleClientset(srcJob, destJob, srcPod)
	destGets := 0
	cs.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		switch a.(clienttesting.GetAction).GetName() {
		case SourceJobName(id):
			out := srcJob.DeepCopy()
			out.Status.Active = 1
			return true, out, nil
		case DestJobName(id):
			destGets++
			out := destJob.DeepCopy()
			if destGets >= 2 {
				out.Status.Conditions = []batchv1.JobCondition{{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(metav1.Now().Time),
				}}
			}
			return true, out, nil
		}
		return false, nil, nil
	})

	var got []StatusUpdate
	synctest.Test(t, func(t *testing.T) {
		n := newFromClient(cs)
		ctx, cancel := context.WithCancel(context.Background())
		run := &nativeRun{
			srcJob:   SourceJobName(id),
			destJob:  DestJobName(id),
			updates:  make(chan StatusUpdate, 16),
			cancel:   cancel,
			finished: make(chan struct{}),
		}
		n.poll(ctx, id, run)
		for u := range run.updates {
			got = append(got, u)
		}
	})
	return got
}

// TestNativePoll_ReplayIsByteIdentical is the determinism gate for the
// migration state machine: the same script run twice must emit the same
// StatusUpdate sequence, When fields included. Any new unseeded input on the
// poll path (wall clock outside a synctest bubble, map iteration order,
// goroutine wakeup order) shows up here as a diff.
//
// n.ids is injected rather than read from crypto/rand for the same reason:
// a migration ID changes every Job name the run touches, so an unseeded ID
// makes replay impossible no matter how well time is virtualized.
func TestNativePoll_ReplayIsByteIdentical(t *testing.T) {
	t.Parallel()
	first := replayPoll(t)
	second := replayPoll(t)
	if len(first) == 0 {
		t.Fatal("scripted poll produced no updates")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay diverged:\n first: %+v\nsecond: %+v", first, second)
	}
	want := []StatusPhase{PhaseTransferring, PhaseSucceeded}
	if len(first) != len(want) {
		t.Fatalf("update count = %d (%+v), want %d", len(first), first, len(want))
	}
	for i, p := range want {
		if first[i].Phase != p {
			t.Errorf("update %d phase = %s, want %s", i, first[i].Phase, p)
		}
	}
}

// TestApply_UsesInjectedIDSource pins the seam: with a scripted ID source,
// Apply mints exactly that ID and names both Jobs after it, so two replays
// touch the same cluster objects.
func TestApply_UsesInjectedIDSource(t *testing.T) {
	t.Parallel()
	const scripted = MigrationID("0123456789abcdef")
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	n.ids = func() MigrationID { return scripted }

	id, err := n.Apply(t.Context(), validRequest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if id != scripted {
		t.Fatalf("migration ID = %q, want %q", id, scripted)
	}
	jobs, err := cs.BatchV1().Jobs(DefaultJobNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, j := range jobs.Items {
		want := SourceJobName(scripted)
		if j.Labels["app.kubernetes.io/component"] == "dest" {
			want = DestJobName(scripted)
		}
		if j.Name != want {
			t.Errorf("job name = %q, want %q", j.Name, want)
		}
	}
}
