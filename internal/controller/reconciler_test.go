package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	fakedyn "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"

	"github.com/maci0/katamaran/internal/orchestrator"
)

func TestSpecToRequest_Minimal(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod": map[string]any{
				"namespace": "default",
				"name":      "kata-demo",
			},
			"destNode": "worker-b",
			"image":    "localhost/katamaran:dev",
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatalf("specToRequest: %v", err)
	}
	if req.SourcePod == nil || req.SourcePod.Name != "kata-demo" || req.SourcePod.Namespace != "default" {
		t.Errorf("SourcePod = %+v", req.SourcePod)
	}
	if req.DestNode != "worker-b" || req.Image != "localhost/katamaran:dev" {
		t.Errorf("DestNode/Image not set: %+v", req)
	}
	if req.SourceNode != "" || req.DestIP != "" {
		t.Errorf("SourceNode/DestIP must be left empty for the reconciler to fill via Discoverer; got %+v", req)
	}
}

func TestSpecToRequest_AllFields(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod": map[string]any{
				"namespace": "default",
				"name":      "kata-demo",
			},
			"destPod": map[string]any{
				"namespace": "default",
				"name":      "kata-dest",
			},
			"destNode":        "worker-b",
			"image":           "localhost/katamaran:dev",
			"sharedStorage":   true,
			"replayCmdline":   true,
			"tunnelMode":      "ipip",
			"downtimeMS":      int64(50),
			"autoDowntime":    true,
			"multifdChannels": int64(4),
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatalf("specToRequest: %v", err)
	}
	if !req.SharedStorage || !req.ReplayCmdline || !req.AutoDowntime {
		t.Errorf("bool fields not threaded: %+v", req)
	}
	if req.DowntimeMS != 50 || req.MultifdChannels != 4 || req.TunnelMode != "ipip" {
		t.Errorf("numeric/string fields not threaded: %+v", req)
	}
	if req.DestPod == nil || req.DestPod.Name != "kata-dest" {
		t.Errorf("DestPod not threaded: %+v", req.DestPod)
	}
}

func TestSpecToRequest_MissingRequired(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want string
	}{
		{
			name: "no sourcePod",
			obj: map[string]any{"spec": map[string]any{
				"destNode": "x", "image": "y",
			}},
			want: "spec.sourcePod",
		},
		{
			name: "no image",
			obj: map[string]any{"spec": map[string]any{
				"sourcePod": map[string]any{"namespace": "default", "name": "p"},
				"destNode":  "x",
			}},
			want: "spec.image is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := specToRequest(tc.obj)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSpecToRequest_OptionalDestNode(t *testing.T) {
	// destNode is now optional — specToRequest should succeed without it.
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod": map[string]any{"namespace": "default", "name": "p"},
			"image":     "localhost/katamaran:dev",
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatalf("specToRequest: %v", err)
	}
	if req.DestNode != "" {
		t.Fatalf("DestNode = %q, want empty", req.DestNode)
	}
}

func TestSpecToRequest_DestNodeSelector(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod":        map[string]any{"namespace": "default", "name": "p"},
			"image":            "localhost/katamaran:dev",
			"destNodeSelector": map[string]any{"gpu": "true", "zone": "us-east-1a"},
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatalf("specToRequest: %v", err)
	}
	if len(req.DestNodeSelector) != 2 {
		t.Fatalf("DestNodeSelector = %v, want 2 entries", req.DestNodeSelector)
	}
	if req.DestNodeSelector["gpu"] != "true" {
		t.Fatalf("DestNodeSelector[gpu] = %q, want true", req.DestNodeSelector["gpu"])
	}
}

// Compile-time check: Discoverer is the right shape — keeps drift between
// the orchestrator package's interface and what Reconciler.dispatch calls
// from showing up at runtime.
var _ orchestrator.Discoverer = (orchestrator.Discoverer)(nil)

// ---- Reconciler-level tests with fake clients ----------------------------

// fakeOrch is a stub orchestrator.Orchestrator that records calls and
// returns scripted results. Tests only exercise Apply/Watch/Stop here.

type fakeOrchCall struct {
	op string // "Apply" | "Watch" | "Stop"
	id string
}

type fakeOrch struct {
	mu            sync.Mutex
	calls         []fakeOrchCall
	lastReq       orchestrator.Request
	applyID       orchestrator.MigrationID
	applyErr      error
	stopErr       error
	resumeErr     error
	resumeCreated bool
	updates       chan orchestrator.StatusUpdate
}

func (f *fakeOrch) Apply(_ context.Context, req orchestrator.Request) (orchestrator.MigrationID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
	f.calls = append(f.calls, fakeOrchCall{op: "Apply", id: string(f.applyID)})
	return f.applyID, f.applyErr
}
func (f *fakeOrch) Watch(_ context.Context, id orchestrator.MigrationID) (<-chan orchestrator.StatusUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeOrchCall{op: "Watch", id: string(id)})
	if f.updates == nil {
		ch := make(chan orchestrator.StatusUpdate)
		close(ch)
		return ch, nil
	}
	return f.updates, nil
}
func (f *fakeOrch) Stop(_ context.Context, id orchestrator.MigrationID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeOrchCall{op: "Stop", id: string(id)})
	return f.stopErr
}
func (f *fakeOrch) Resume(_ context.Context, id orchestrator.MigrationID, req orchestrator.Request) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeOrchCall{op: "Resume", id: string(id)})
	f.lastReq = req
	return f.resumeCreated, f.resumeErr
}
func (f *fakeOrch) callsFor(op string) []fakeOrchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeOrchCall
	for _, c := range f.calls {
		if c.op == op {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeOrch) lastRequest() orchestrator.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

type fakeDiscoverer struct {
	mu            sync.Mutex
	podNode       string
	nodeIP        string
	podNS         string
	podName       string
	node          string
	podScheduling orchestrator.PodScheduling
	deletedPods   []string
	orphanedPods  []string
}

func (f *fakeDiscoverer) ListKataPods(context.Context) ([]orchestrator.PodInfo, error) {
	return nil, nil
}

func (f *fakeDiscoverer) ListKataNodes(context.Context) ([]orchestrator.NodeInfo, error) {
	return nil, nil
}

func (f *fakeDiscoverer) LookupPodNode(_ context.Context, namespace, name string) (string, error) {
	f.podNS = namespace
	f.podName = name
	return f.podNode, nil
}

func (f *fakeDiscoverer) LookupNodeInternalIP(_ context.Context, name string) (string, error) {
	f.node = name
	return f.nodeIP, nil
}

func (f *fakeDiscoverer) LookupPodScheduling(_ context.Context, namespace, name string) (orchestrator.PodScheduling, error) {
	return f.podScheduling, nil
}

func (f *fakeDiscoverer) DeletePod(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedPods = append(f.deletedPods, namespace+"/"+name)
	return nil
}

func (f *fakeDiscoverer) OrphanAndDeletePod(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orphanedPods = append(f.orphanedPods, namespace+"/"+name)
	return nil
}

func newMigrationCR(name string, finalizers []string, withDeletion bool, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "katamaran.io/v1alpha1",
		"kind":       "Migration",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "default",
		},
		"spec": map[string]any{
			"sourcePod": map[string]any{"namespace": "default", "name": "kata-demo"},
			"destNode":  "worker-b",
			"image":     "localhost/katamaran:dev",
		},
	}
	if status != nil {
		obj["status"] = status
	}
	u := &unstructured.Unstructured{Object: obj}
	if len(finalizers) > 0 {
		u.SetFinalizers(finalizers)
	}
	if withDeletion {
		now := metav1.Now()
		u.SetDeletionTimestamp(&now)
	}
	return u
}

// completedDestJob returns the dest Job for a migration id in the Completed state.
func completedDestJob(id string) batchv1.Job {
	return batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "katamaran-dest-" + id,
			Namespace: orchestrator.DefaultJobNamespace,
			Labels: map[string]string{
				orchestrator.MigrationIDLabel: id,
				"app.kubernetes.io/component": "dest",
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: "True"},
			},
		},
	}
}

func newReconcilerWithCR(t *testing.T, orch orchestrator.Orchestrator, cr *unstructured.Unstructured, jobs ...batchv1.Job) (*Reconciler, *fakedyn.FakeDynamicClient, *fakekube.Clientset) {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "Migration"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "MigrationList"}, &unstructured.UnstructuredList{})
	dyn := fakedyn.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		MigrationGVR: "MigrationList",
	}, cr)
	kubeObjs := make([]runtime.Object, len(jobs))
	for i := range jobs {
		j := jobs[i]
		kubeObjs[i] = &j
	}
	kube := fakekube.NewSimpleClientset(kubeObjs...)
	rec := NewReconciler(dyn, kube, orch, nil)
	rec.PollInterval = 10 * time.Millisecond
	rec.StatusTimeout = 1 * time.Second
	return rec, dyn, kube
}

func TestReconciler_AddsFinalizerOnNewCR(t *testing.T) {
	cr := newMigrationCR("m1", nil, false, nil)
	orch := &fakeOrch{applyID: "id-m1"}
	rec, dyn, _ := newReconcilerWithCR(t, orch, cr)
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, _ := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m1", metav1.GetOptions{})
	if !hasFinalizer(got) {
		t.Fatalf("finalizer missing: %v", got.GetFinalizers())
	}
}

func TestReconciler_DispatchResolvesPodRequest(t *testing.T) {
	cr := newMigrationCR("m-resolve", []string{finalizerName}, false, nil)
	updates := make(chan orchestrator.StatusUpdate, 1)
	updates <- orchestrator.StatusUpdate{ID: "id-resolve", Phase: orchestrator.PhaseSucceeded}
	close(updates)
	orch := &fakeOrch{applyID: "id-resolve", updates: updates}
	rec, _, _ := newReconcilerWithCR(t, orch, cr)
	disc := &fakeDiscoverer{podNode: "worker-a", nodeIP: "10.0.0.20"}
	rec.Discoverer = disc

	key := types.NamespacedName{Namespace: "default", Name: "m-resolve"}
	rec.dispatch(context.Background(), key, cr)

	if disc.podNS != "default" || disc.podName != "kata-demo" {
		t.Fatalf("LookupPodNode called with %s/%s, want default/kata-demo", disc.podNS, disc.podName)
	}
	if disc.node != "worker-b" {
		t.Fatalf("LookupNodeInternalIP called with %q, want worker-b", disc.node)
	}
	got := orch.lastRequest()
	if got.SourcePod == nil || got.SourcePod.Namespace != "default" || got.SourcePod.Name != "kata-demo" {
		t.Fatalf("SourcePod = %+v, want default/kata-demo", got.SourcePod)
	}
	if got.SourceNode != "worker-a" {
		t.Fatalf("SourceNode = %q, want worker-a", got.SourceNode)
	}
	if got.DestNode != "worker-b" {
		t.Fatalf("DestNode = %q, want worker-b", got.DestNode)
	}
	if got.DestIP != "10.0.0.20" {
		t.Fatalf("DestIP = %q, want 10.0.0.20", got.DestIP)
	}
}

func TestReconciler_DeletionCallsStopAndRemovesFinalizer(t *testing.T) {
	cr := newMigrationCR("m2", []string{finalizerName}, true, map[string]any{
		"phase":       "transferring",
		"migrationID": "id-m2",
	})
	orch := &fakeOrch{}
	rec, dyn, _ := newReconcilerWithCR(t, orch, cr)
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	stops := orch.callsFor("Stop")
	if len(stops) != 1 || stops[0].id != "id-m2" {
		t.Fatalf("Stop calls = %v, want one with id-m2", stops)
	}
	got, _ := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m2", metav1.GetOptions{})
	if hasFinalizer(got) {
		t.Fatalf("finalizer still present: %v", got.GetFinalizers())
	}
}

// blockingApplyOrch blocks inside Apply until its context is cancelled,
// letting tests hold dispatch mid-submission.
type blockingApplyOrch struct {
	started chan struct{}
	done    chan struct{}
}

func (b *blockingApplyOrch) Apply(ctx context.Context, _ orchestrator.Request) (orchestrator.MigrationID, error) {
	close(b.started)
	<-ctx.Done()
	close(b.done)
	return "", ctx.Err()
}

func (b *blockingApplyOrch) Watch(_ context.Context, _ orchestrator.MigrationID) (<-chan orchestrator.StatusUpdate, error) {
	ch := make(chan orchestrator.StatusUpdate)
	close(ch)
	return ch, nil
}

func (b *blockingApplyOrch) Stop(_ context.Context, _ orchestrator.MigrationID) error { return nil }

func (b *blockingApplyOrch) Resume(_ context.Context, _ orchestrator.MigrationID, _ orchestrator.Request) (bool, error) {
	return false, nil
}

func TestReconciler_DeletionCancelsInFlightDispatch(t *testing.T) {
	cr := newMigrationCR("m-delrace", []string{finalizerName}, false, nil)
	orch := &blockingApplyOrch{started: make(chan struct{}), done: make(chan struct{})}
	rec, _, _ := newReconcilerWithCR(t, orch, cr)
	rec.Discoverer = &fakeDiscoverer{podNode: "worker-a", nodeIP: "10.0.0.20"}
	// Long budget: without the early setTrackCancel registration, nothing
	// else can unblock Apply within this window.
	rec.StatusTimeout = 10 * time.Minute

	key := types.NamespacedName{Namespace: "default", Name: "m-delrace"}
	if !rec.markTracking(key) {
		t.Fatal("markTracking: expected first claim")
	}
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		rec.dispatch(context.Background(), key, cr)
	}()

	select {
	case <-orch.started:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch never reached Apply")
	}

	delCR := newMigrationCR("m-delrace", []string{finalizerName}, true, nil)
	rec.handleDeletion(context.Background(), key, delCR)

	select {
	case <-dispatchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handleDeletion did not cancel in-flight dispatch; deleting a brand-new Migration would still create Jobs afterwards")
	}
}

func TestReconciler_RecoverFromDestComplete(t *testing.T) {
	cr := newMigrationCR("m3", []string{finalizerName}, false, map[string]any{
		"phase":       "transferring",
		"migrationID": "id-m3",
	})
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr, completedDestJob("id-m3"))
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Recovery runs in a goroutine; allow it a few ticks to converge.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m3", metav1.GetOptions{})
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		if phase == string(orchestrator.PhaseSucceeded) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery never patched succeeded")
}

func TestReconciler_RecoverFromAnyNonTerminalPhase(t *testing.T) {
	cr := newMigrationCR("m-cutover", []string{finalizerName}, false, map[string]any{
		"phase":       "cutover",
		"migrationID": "id-cutover",
	})
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr, completedDestJob("id-cutover"))
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m-cutover", metav1.GetOptions{})
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		if phase == string(orchestrator.PhaseSucceeded) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery never patched succeeded from cutover")
}

// TestReconciler_RecoverCallsResumeWhenSourceRunningDestMissing covers
// the mid-flight controller-restart case: source job is running, dest
// job was never created (orchestrator goroutine died with the previous
// mgr leader). Recovery must call Orchestrator.Resume so the dest job
// gets created and the migration completes.
func TestReconciler_RecoverCallsResumeWhenSourceRunningDestMissing(t *testing.T) {
	cr := newMigrationCR("m-resume", []string{finalizerName}, false, map[string]any{
		"phase":       "submitted",
		"migrationID": "id-resume",
	})
	srcJob := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "katamaran-source-id-resume",
			Namespace: orchestrator.DefaultJobNamespace,
			Labels: map[string]string{
				orchestrator.MigrationIDLabel: "id-resume",
				"app.kubernetes.io/component": "source",
			},
		},
		// No terminal condition: still running.
	}
	orch := &fakeOrch{resumeCreated: true}
	rec, _, _ := newReconcilerWithCR(t, orch, cr, srcJob)
	startResumed := mResumed.Value()
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls := orch.callsFor("Resume"); len(calls) > 0 {
			if calls[0].id != "id-resume" {
				t.Fatalf("Resume called with id %q, want id-resume", calls[0].id)
			}
			if got := mResumed.Value() - startResumed; got < 1 {
				t.Fatalf("mResumed delta = %d, want >= 1 (created=true should bump counter)", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery never called Resume; calls=%v", orch.calls)
}

func TestReconciler_RecoverFromMissingJobs(t *testing.T) {
	cr := newMigrationCR("m4", []string{finalizerName}, false, map[string]any{
		"phase":       "submitted",
		"migrationID": "id-m4",
	})
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr) // no jobs
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m4", metav1.GetOptions{})
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		errMsg, _, _ := unstructured.NestedString(got.Object, "status", "message")
		if phase == string(orchestrator.PhaseFailed) && strings.Contains(errMsg, "disappeared") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery never patched failed/disappeared")
}

func TestMarkTracking_SingleClaim(t *testing.T) {
	rec := &Reconciler{tracking: map[types.NamespacedName]*track{}}
	key := types.NamespacedName{Namespace: "default", Name: "m"}
	if !rec.markTracking(key) {
		t.Fatal("first markTracking should succeed")
	}
	if rec.markTracking(key) {
		t.Fatal("second markTracking should fail until untrack")
	}
	rec.untrack(key)
	if !rec.markTracking(key) {
		t.Fatal("after untrack, markTracking should succeed again")
	}
}

// TestUpdateProgressMetricsAndSnapshot pins the contract behind the
// /metrics and debug exports: entries are keyed by migration ID, empty
// IDs are ignored, phase always tracks the latest update while zero
// numeric fields keep their previous value (high-water semantics), and
// MigrationProgressSnapshot returns a copy that can be mutated without
// corrupting the tracked state.
func TestUpdateProgressMetricsAndSnapshot(t *testing.T) {
	// Not parallel: mutates the process-global migrationProgress map.
	const id = "id-progress-test"
	t.Cleanup(func() { migrationProgress.Delete(id) })

	updateProgressMetrics(orchestrator.StatusUpdate{
		ID:             id,
		Phase:          orchestrator.PhaseTransferring,
		RAMTransferred: 100,
		RAMTotal:       200,
	})
	snap := MigrationProgressSnapshot()
	e, ok := snap[id]
	if !ok {
		t.Fatalf("snapshot missing entry for %q: %v", id, snap)
	}
	if e.Phase != string(orchestrator.PhaseTransferring) || e.RAMTransferred != 100 || e.RAMTotal != 200 {
		t.Fatalf("entry = %+v, want transferring with ram 100/200", e)
	}

	// Zero-valued fields must not clobber the previously recorded numbers;
	// the new downtime must still land.
	updateProgressMetrics(orchestrator.StatusUpdate{
		ID:         id,
		Phase:      orchestrator.PhaseCutover,
		DowntimeMS: 42,
	})
	e = MigrationProgressSnapshot()[id]
	if e.Phase != string(orchestrator.PhaseCutover) {
		t.Fatalf("phase = %q, want %q", e.Phase, orchestrator.PhaseCutover)
	}
	if e.RAMTransferred != 100 || e.RAMTotal != 200 {
		t.Fatalf("zero-valued fields overwrote progress: %+v", e)
	}
	if e.DowntimeMS != 42 {
		t.Fatalf("DowntimeMS = %d, want 42", e.DowntimeMS)
	}

	// A snapshot is a copy: mutating it must not affect tracked state.
	snap = MigrationProgressSnapshot()
	snap[id] = MigrationProgressEntry{Phase: "tampered"}
	if got := MigrationProgressSnapshot()[id].Phase; got == "tampered" {
		t.Fatal("MigrationProgressSnapshot leaked internal state: mutating the snapshot changed tracked state")
	}

	// Empty-ID updates must be dropped, not stored under "".
	updateProgressMetrics(orchestrator.StatusUpdate{Phase: orchestrator.PhaseSucceeded})
	if _, ok := MigrationProgressSnapshot()[""]; ok {
		t.Fatal("empty migration ID was stored in progress map")
	}
}

func TestPatchStatusUpdate_PersistsProgressAndClearsStaleFields(t *testing.T) {
	cr := newMigrationCR("m5", []string{finalizerName}, false, map[string]any{
		"phase":   "submitted",
		"message": "old message",
		"error":   "old error",
	})
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr)
	err := rec.patchStatusUpdate(context.Background(), types.NamespacedName{Namespace: "default", Name: "m5"}, orchestrator.StatusUpdate{
		ID:             "id-m5",
		Phase:          orchestrator.PhaseTransferring,
		RAMTransferred: 123,
		RAMTotal:       456,
	}, "")
	if err != nil {
		t.Fatalf("patchStatusUpdate: %v", err)
	}
	got, err := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m5", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get m5 after first patch: %v", err)
	}
	if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != string(orchestrator.PhaseTransferring) {
		t.Fatalf("phase = %q, want transferring", phase)
	}
	if xfer, _, _ := unstructured.NestedInt64(got.Object, "status", "ramTransferred"); xfer != 123 {
		t.Fatalf("ramTransferred = %d, want 123", xfer)
	}
	if total, _, _ := unstructured.NestedInt64(got.Object, "status", "ramTotal"); total != 456 {
		t.Fatalf("ramTotal = %d, want 456", total)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "message"); found {
		t.Fatalf("stale message was not cleared")
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "error"); found {
		t.Fatalf("stale error was not cleared")
	}

	err = rec.patchStatusUpdate(context.Background(), types.NamespacedName{Namespace: "default", Name: "m5"}, orchestrator.StatusUpdate{
		ID:         "id-m5",
		Phase:      orchestrator.PhaseSucceeded,
		DowntimeMS: 17,
	}, "")
	if err != nil {
		t.Fatalf("patchStatusUpdate succeeded: %v", err)
	}
	got, err = dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m5", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get m5 after second patch: %v", err)
	}
	if downtime, _, _ := unstructured.NestedInt64(got.Object, "status", "actualDowntimeMS"); downtime != 17 {
		t.Fatalf("actualDowntimeMS = %d, want 17", downtime)
	}
}

func TestSpecToRequest_AdoptVM(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod": map[string]any{"namespace": "default", "name": "src"},
			"image":     "test:latest",
			"adoptVM":   true,
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !req.AdoptVM {
		t.Fatal("expected AdoptVM=true")
	}
}

func TestSpecToRequest_AdoptVM_DefaultFalse(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod": map[string]any{"namespace": "default", "name": "src"},
			"image":     "test:latest",
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatal(err)
	}
	if req.AdoptVM {
		t.Fatal("expected AdoptVM=false by default")
	}
}

// adoptCR builds a Migration CR with adoptVM and the given sourceCleanup,
// auto-scheduled (no destNode) so dispatch skips the slow adoption-pod
// path while still exercising the source-controller pending mark.
func adoptCR(name, sourceCleanup string) *unstructured.Unstructured {
	spec := map[string]any{
		"sourcePod": map[string]any{"namespace": "default", "name": "kata-demo"},
		"image":     "localhost/katamaran:dev",
		"adoptVM":   true,
	}
	if sourceCleanup != "" {
		spec["sourceCleanup"] = sourceCleanup
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "katamaran.io/v1alpha1",
		"kind":       "Migration",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       spec,
	}}
	u.SetFinalizers([]string{finalizerName})
	return u
}

// sourcePodOwnedBy returns the source pod carrying a controller
// ownerReference of the given kind/uid, as the webhook keys off.
func sourcePodOwnedBy(kind string, uid types.UID) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "kata-demo",
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       kind,
				Name:       "kata-demo-owner",
				UID:        uid,
				Controller: &ctrl,
			}},
		},
	}
}

func newAdoptReconciler(t *testing.T, orch orchestrator.Orchestrator, cr *unstructured.Unstructured, pod *corev1.Pod) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "Migration"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "MigrationList"}, &unstructured.UnstructuredList{})
	dyn := fakedyn.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		MigrationGVR: "MigrationList",
		// createAdoptionPod writes Pods through the dynamic client; the
		// pod list kind must be registered so tests can List them back.
		{Group: "", Version: "v1", Resource: "pods"}: "PodList",
	}, cr)
	kube := fakekube.NewSimpleClientset(pod)
	rec := NewReconciler(dyn, kube, orch, nil)
	rec.PollInterval = 10 * time.Millisecond
	rec.StatusTimeout = 1 * time.Second
	rec.Discoverer = &fakeDiscoverer{podNode: "worker-a", nodeIP: "10.0.0.20"}
	return rec
}

// On migration success with adoptVM, the source-pod controller must be
// marked pending so the webhook denies replacement pods. This mark must
// happen even when sourceCleanup=none — the source container is killed
// when QEMU pauses, so the controller will otherwise spawn a replacement
// to refill its replica count. Regression: the mark used to live inside
// the sourceCleanup!=none branch and never ran for sourceCleanup=none.
func TestReconciler_MarksPendingWhenSourceCleanupNone(t *testing.T) {
	const rsUID types.UID = "rs-uid-none"
	for _, cleanup := range []string{"none", ""} {
		t.Run("cleanup="+cleanup, func(t *testing.T) {
			updates := make(chan orchestrator.StatusUpdate, 1)
			updates <- orchestrator.StatusUpdate{ID: "id-none", Phase: orchestrator.PhaseSucceeded}
			close(updates)
			orch := &fakeOrch{applyID: "id-none", updates: updates}
			rec := newAdoptReconciler(t, orch, adoptCR("m-none", cleanup), sourcePodOwnedBy("ReplicaSet", rsUID))

			rec.dispatch(context.Background(), types.NamespacedName{Namespace: "default", Name: "m-none"}, adoptCR("m-none", cleanup))

			if got := rec.pending.MigrationFor(rsUID); got != "id-none" {
				t.Fatalf("pending mark for %s = %q, want id-none (mark must run independently of sourceCleanup)", rsUID, got)
			}
		})
	}
}

// Non-managed owner kinds (e.g. a bare ReplicationController, or no
// controller at all) must NOT be marked — only built-in workload
// controllers that auto-refill replicas are denied.
func TestReconciler_DoesNotMarkUnmanagedOwner(t *testing.T) {
	const uid types.UID = "rc-uid"
	updates := make(chan orchestrator.StatusUpdate, 1)
	updates <- orchestrator.StatusUpdate{ID: "id-rc", Phase: orchestrator.PhaseSucceeded}
	close(updates)
	orch := &fakeOrch{applyID: "id-rc", updates: updates}
	rec := newAdoptReconciler(t, orch, adoptCR("m-rc", "none"), sourcePodOwnedBy("ReplicationController", uid))

	rec.dispatch(context.Background(), types.NamespacedName{Namespace: "default", Name: "m-rc"}, adoptCR("m-rc", "none"))

	if got := rec.pending.MigrationFor(uid); got != "" {
		t.Fatalf("unmanaged owner %s was marked %q, want unmarked", uid, got)
	}
}

// With adoptVM and no spec.destNode (auto-select), the adoption pod must
// still be created: the dest node is resolved from the dest Job's
// scheduled pod. Regression: the node resolution inside native.Apply only
// mutates its local Request copy, so the reconciler used to see an empty
// DestNode and silently skip adoption while keeping the webhook's
// pending mark active — shrinking the workload by one replica.
func TestReconciler_AdoptVM_AutoSelectResolvesDestNode(t *testing.T) {
	const rsUID types.UID = "rs-uid-autoselect"
	const migID = "id-adopt1"
	updates := make(chan orchestrator.StatusUpdate, 1)
	updates <- orchestrator.StatusUpdate{ID: migID, Phase: orchestrator.PhaseSucceeded}
	close(updates)
	orch := &fakeOrch{applyID: migID, updates: updates}
	rec := newAdoptReconciler(t, orch, adoptCR("m-auto", ""), sourcePodOwnedBy("ReplicaSet", rsUID))

	destPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: orchestrator.DefaultJobNamespace,
			Name:      "katamaran-dest-" + migID + "-abcde",
			Labels:    map[string]string{"batch.kubernetes.io/job-name": orchestrator.DestJobName(migID)},
		},
		Spec: corev1.PodSpec{NodeName: "worker-b"},
	}
	if _, err := rec.Kube.CoreV1().Pods(orchestrator.DefaultJobNamespace).Create(context.Background(), destPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed dest job pod: %v", err)
	}

	savedDelay := adoptionVMConfigDelay
	adoptionVMConfigDelay = time.Millisecond
	t.Cleanup(func() { adoptionVMConfigDelay = savedDelay })

	cr := adoptCR("m-auto", "")
	rec.dispatch(context.Background(), types.NamespacedName{Namespace: "default", Name: "m-auto"}, cr)

	if got := rec.pending.MigrationFor(rsUID); got != migID {
		t.Fatalf("pending mark for %s = %q, want %q", rsUID, got, migID)
	}
	// createAdoptionPod goes through the dynamic client, so list its
	// objects from there (the fake kube clientset is a separate store).
	podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	list, err := rec.Dynamic.Resource(podGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list adoption pods: %v", err)
	}
	var adopted *unstructured.Unstructured
	for i := range list.Items {
		if strings.HasPrefix(list.Items[i].GetName(), "adopted-") {
			adopted = &list.Items[i]
			break
		}
	}
	if adopted == nil {
		t.Fatalf("no adoption pod created; pods=%d", len(list.Items))
	}
	node, _, _ := unstructured.NestedString(adopted.Object, "spec", "nodeName")
	if node != "worker-b" {
		t.Fatalf("adoption pod scheduled on %q, want worker-b", node)
	}
}

// Recovery after a controller restart must run the documented post-success
// side effects too: sourceCleanup=delete must delete the source pod even
// though the success was observed via Job inspection rather than the watch.
func TestReconciler_RecoverFromDestComplete_RunsSourceCleanup(t *testing.T) {
	cr := newMigrationCR("m3-cleanup", []string{finalizerName}, false, map[string]any{
		"phase":       "transferring",
		"migrationID": "id-m3c",
	})
	// Patch in sourceCleanup=delete on top of the base CR spec.
	_ = unstructured.SetNestedField(cr.Object, "delete", "spec", "sourceCleanup")

	disc := &fakeDiscoverer{}
	rec, _, _ := func() (*Reconciler, dynamic.Interface, *fakekube.Clientset) {
		scheme := runtime.NewScheme()
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "Migration"}, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "MigrationList"}, &unstructured.UnstructuredList{})
		dyn := fakedyn.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
			MigrationGVR: "MigrationList",
		}, cr)
		destJob := completedDestJob("id-m3c")
		kube := fakekube.NewSimpleClientset(&destJob)
		rec := NewReconciler(dyn, kube, &fakeOrch{}, disc)
		rec.PollInterval = 10 * time.Millisecond
		rec.StatusTimeout = 1 * time.Second
		return rec, dyn, kube
	}()

	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Recovery runs in a goroutine; allow it a few ticks to converge.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := len(disc.deletedPods); got == 1 && disc.deletedPods[0] == "default/kata-demo" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery deleted pods %v, want [default/kata-demo]", disc.deletedPods)
}
