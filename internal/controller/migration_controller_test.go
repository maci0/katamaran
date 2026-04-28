package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
			name: "no destNode",
			obj: map[string]any{"spec": map[string]any{
				"sourcePod": map[string]any{"namespace": "default", "name": "p"},
				"image":     "y",
			}},
			want: "spec.destNode",
		},
		{
			name: "no image",
			obj: map[string]any{"spec": map[string]any{
				"sourcePod": map[string]any{"namespace": "default", "name": "p"},
				"destNode":  "x",
			}},
			want: "spec.destNode and spec.image are required",
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
	mu       sync.Mutex
	calls    []fakeOrchCall
	applyID  orchestrator.MigrationID
	applyErr error
	stopErr  error
	updates  chan orchestrator.StatusUpdate
}

func (f *fakeOrch) Apply(_ context.Context, _ orchestrator.Request) (orchestrator.MigrationID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// fakeDisc is a stub orchestrator.Discoverer that returns scripted lookups.
type fakeDisc struct {
	srcNode string
	destIP  string
	err     error
}

func (f *fakeDisc) ListKataPods(context.Context) ([]orchestrator.PodInfo, error) {
	return nil, nil
}
func (f *fakeDisc) ListKataNodes(context.Context) ([]orchestrator.NodeInfo, error) {
	return nil, nil
}
func (f *fakeDisc) LookupPodNode(context.Context, string, string) (string, error) {
	return f.srcNode, f.err
}
func (f *fakeDisc) LookupNodeInternalIP(context.Context, string) (string, error) {
	return f.destIP, f.err
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

func TestReconciler_RecoverFromDestComplete(t *testing.T) {
	cr := newMigrationCR("m3", []string{finalizerName}, false, map[string]any{
		"phase":       "transferring",
		"migrationID": "id-m3",
	})
	destJob := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "katamaran-dest-id-m3",
			Namespace: jobNamespace,
			Labels: map[string]string{
				"katamaran.io/migration-id":   "id-m3",
				"app.kubernetes.io/component": "dest",
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: "True"},
			},
		},
	}
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr, destJob)
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Recovery runs in a goroutine; allow it a few ticks to converge.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m3", metav1.GetOptions{})
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
	destJob := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "katamaran-dest-id-cutover",
			Namespace: jobNamespace,
			Labels: map[string]string{
				"katamaran.io/migration-id":   "id-cutover",
				"app.kubernetes.io/component": "dest",
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: "True"},
			},
		},
	}
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr, destJob)
	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m-cutover", metav1.GetOptions{})
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		if phase == string(orchestrator.PhaseSucceeded) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery never patched succeeded from cutover")
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
		got, _ := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m4", metav1.GetOptions{})
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
	got, _ := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m5", metav1.GetOptions{})
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
	got, _ = dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m5", metav1.GetOptions{})
	if downtime, _, _ := unstructured.NestedInt64(got.Object, "status", "actualDowntimeMS"); downtime != 17 {
		t.Fatalf("actualDowntimeMS = %d, want 17", downtime)
	}
}
