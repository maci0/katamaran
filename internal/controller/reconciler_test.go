package controller

import (
	"bytes"
	"context"
	"errors"
	"maps"
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
	clienttesting "k8s.io/client-go/testing"

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
	req, err := specToRequest(obj, "default")
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
	req, err := specToRequest(obj, "default")
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
			_, err := specToRequest(tc.obj, "default")
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
	req, err := specToRequest(obj, "default")
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
	req, err := specToRequest(obj, "default")
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
	podNodeErr    error
	nodeIPErr     error
	schedErr      error
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
	return f.podNode, f.podNodeErr
}

func (f *fakeDiscoverer) LookupNodeInternalIP(_ context.Context, name string) (string, error) {
	f.node = name
	return f.nodeIP, f.nodeIPErr
}

func (f *fakeDiscoverer) LookupPodScheduling(_ context.Context, namespace, name string) (orchestrator.PodScheduling, error) {
	return f.podScheduling, f.schedErr
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

// deletedPodsSnapshot returns a locked copy of deletedPods. Recovery runs in
// a goroutine, so test assertions must not read the slice unsynchronized.
func (f *fakeDiscoverer) deletedPodsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletedPods...)
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

// TestResolveSourcePodDiscovery_FailuresAndSelectorMerge pins two contracts
// dispatch depends on. First, every resolution failure must return an error
// AND leave a Failed status on the CR naming the failed step — that status is
// what a kubectl describe shows the operator, and without the early return
// dispatch would Apply a half-resolved Request (empty SourceNode/DestIP).
// Second, in auto-select mode the source pod's nodeSelector merges with any
// spec.destNodeSelector, with the CRD-level value winning key conflicts
// (the user's explicit constraint must not be silently overridden by the
// source pod's scheduling).
func TestResolveSourcePodDiscovery_FailuresAndSelectorMerge(t *testing.T) {
	t.Parallel()
	key := types.NamespacedName{Namespace: "default", Name: "m-disc"}
	newReq := func(t *testing.T, cr *unstructured.Unstructured) orchestrator.Request {
		t.Helper()
		req, err := specToRequest(cr.Object, key.Namespace)
		if err != nil {
			t.Fatalf("specToRequest: %v", err)
		}
		return req
	}

	tests := []struct {
		name           string
		mutateCR       func(*unstructured.Unstructured)
		disc           *fakeDiscoverer
		wantErr        string
		wantMsg        string // expected status.message on the patched CR
		verify         func(*testing.T, orchestrator.Request)
		skipStatusWant bool // merge/success cases do not write Failed status
	}{
		{
			name:    "no discoverer configured",
			wantErr: "discoverer unavailable",
			wantMsg: "resolve migration",
		},
		{
			name:    "source pod lookup fails",
			disc:    &fakeDiscoverer{podNodeErr: errors.New("apiserver unreachable")},
			wantErr: "apiserver unreachable",
			wantMsg: "resolve source pod node",
		},
		{
			name:    "source pod has no node yet",
			disc:    &fakeDiscoverer{podNode: ""},
			wantErr: "source pod node is empty",
			wantMsg: "resolve source pod node",
		},
		{
			name: "auto-select scheduling lookup fails",
			disc: &fakeDiscoverer{podNode: "worker-a", schedErr: errors.New("scheduling lookup boom")},
			mutateCR: func(cr *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(cr.Object, nil, "spec", "destNode")
			},
			wantErr: "scheduling lookup boom",
			wantMsg: "resolve source pod scheduling",
		},
		{
			name: "auto-select merges nodeSelectors CRD-wins",
			disc: &fakeDiscoverer{
				podNode: "worker-a",
				podScheduling: orchestrator.PodScheduling{
					NodeSelector: map[string]string{"katamaran.io/enabled": "true", "zone": "us-east-1a"},
					Tolerations: []corev1.Toleration{{
						Key:      "katamaran",
						Operator: corev1.TolerationOpExists,
					}},
				},
			},
			mutateCR: func(cr *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(cr.Object, nil, "spec", "destNode")
				_ = unstructured.SetNestedMap(cr.Object, map[string]any{"zone": "eu-west-1b", "gpu": "true"}, "spec", "destNodeSelector")
			},
			wantErr:        "",
			skipStatusWant: true,
			verify: func(t *testing.T, req orchestrator.Request) {
				t.Helper()
				// CRD-level values win conflicts; source-pod-only keys are added.
				want := map[string]string{"zone": "eu-west-1b", "gpu": "true", "katamaran.io/enabled": "true"}
				if !maps.Equal(req.DestNodeSelector, want) {
					t.Fatalf("DestNodeSelector = %v, want %v", req.DestNodeSelector, want)
				}
				if len(req.DestTolerations) != 1 || req.DestTolerations[0].Key != "katamaran" {
					t.Fatalf("DestTolerations = %+v, want source toleration copied", req.DestTolerations)
				}
				if req.SourceNode != "worker-a" {
					t.Fatalf("SourceNode = %q, want worker-a", req.SourceNode)
				}
			},
		},
		{
			name:    "dest node IP lookup fails",
			disc:    &fakeDiscoverer{podNode: "worker-a", nodeIPErr: errors.New("node gone")},
			wantErr: "node gone",
			wantMsg: "resolve dest node IP",
		},
		{
			name:    "dest node has no InternalIP",
			disc:    &fakeDiscoverer{podNode: "worker-a", nodeIP: ""},
			wantErr: "destination node InternalIP is empty",
			wantMsg: "resolve dest node IP",
		},
		{
			name:    "same-node migration rejected",
			disc:    &fakeDiscoverer{podNode: "worker-b", nodeIP: "10.0.0.20"},
			wantErr: "already runs on destNode",
			wantMsg: "invalid spec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cr := newMigrationCR("m-disc", []string{finalizerName}, false, nil)
			if tt.mutateCR != nil {
				tt.mutateCR(cr)
			}
			rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr)
			if tt.disc != nil {
				rec.Discoverer = tt.disc
			}

			req := newReq(t, cr)
			err := rec.resolveSourcePodDiscovery(context.Background(), key, &req)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
			}
			if tt.verify != nil {
				tt.verify(t, req)
			}

			got, gerr := dyn.Resource(MigrationGVR).Namespace(key.Namespace).Get(context.Background(), key.Name, metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get CR: %v", gerr)
			}
			phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
			message, _, _ := unstructured.NestedString(got.Object, "status", "message")
			errField, _, _ := unstructured.NestedString(got.Object, "status", "error")
			if tt.skipStatusWant {
				if phase != "" {
					t.Fatalf("status.phase = %q, want untouched on success path", phase)
				}
				return
			}
			if phase != string(orchestrator.PhaseFailed) {
				t.Fatalf("status.phase = %q, want %q (failure must be persisted for the operator)", phase, orchestrator.PhaseFailed)
			}
			if !strings.Contains(message, tt.wantMsg) {
				t.Fatalf("status.message = %q, want containing %q", message, tt.wantMsg)
			}
			if !strings.Contains(errField, tt.wantErr) {
				t.Fatalf("status.error = %q, want containing %q", errField, tt.wantErr)
			}
		})
	}
}

func TestReconciler_DispatchCoalescesProgressUpdates(t *testing.T) {
	cr := newMigrationCR("m-coalesce", []string{finalizerName}, false, nil)
	updates := make(chan orchestrator.StatusUpdate, 16)
	updates <- orchestrator.StatusUpdate{ID: "id-coalesce", Phase: orchestrator.PhaseDestStarting}
	// Rapid-fire transferring updates: only RAM counters differ, exactly the
	// shape tailProgress emits per KATAMARAN_PROGRESS marker during bulk RAM
	// transfer.
	for i := int64(1); i <= 8; i++ {
		updates <- orchestrator.StatusUpdate{ID: "id-coalesce", Phase: orchestrator.PhaseTransferring, RAMTransferred: i * 1000, RAMTotal: 8000}
	}
	updates <- orchestrator.StatusUpdate{ID: "id-coalesce", Phase: orchestrator.PhaseSucceeded, RAMTransferred: 8000, DowntimeMS: 12}
	close(updates)
	orch := &fakeOrch{applyID: "id-coalesce", updates: updates}
	rec, dyn, _ := newReconcilerWithCR(t, orch, cr)
	rec.Discoverer = &fakeDiscoverer{podNode: "worker-a", nodeIP: "10.0.0.20"}

	rec.dispatch(context.Background(), types.NamespacedName{Namespace: "default", Name: "m-coalesce"}, cr)

	var statusPatches int
	var sawSucceededPatch bool
	for _, a := range dyn.Actions() {
		if a.GetVerb() != "patch" || a.GetSubresource() != "status" {
			continue
		}
		statusPatches++
		if bytes.Contains(a.(clienttesting.PatchAction).GetPatch(), []byte(string(orchestrator.PhaseSucceeded))) {
			sawSucceededPatch = true
		}
	}
	// Exactly four patches must land: submitted (pre-watch), dest-starting
	// (first watch update), transferring (phase transition), succeeded
	// (terminal). The seven intermediate RAM-only refreshes are coalesced
	// away instead of each becoming an API write.
	if statusPatches != 4 {
		t.Fatalf("status patch count = %d, want 4 (intermediate RAM-only updates must be coalesced)", statusPatches)
	}
	if !sawSucceededPatch {
		t.Fatal("terminal succeeded update was never patched")
	}

	got, err := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m-coalesce", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get m-coalesce: %v", err)
	}
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	if phase != string(orchestrator.PhaseSucceeded) {
		t.Fatalf("final status.phase = %q, want %q", phase, orchestrator.PhaseSucceeded)
	}
	downtime, _, _ := unstructured.NestedInt64(got.Object, "status", "actualDowntimeMS")
	if downtime != 12 {
		t.Fatalf("final status.actualDowntimeMS = %d, want 12 (terminal facts must survive coalescing)", downtime)
	}
}

func TestShouldPatchStatusUpdate(t *testing.T) {
	t.Parallel()
	now := time.Now()
	recent := now.Add(-time.Second)
	old := now.Add(-2 * progressPatchInterval)
	cases := []struct {
		name        string
		u           orchestrator.StatusUpdate
		lastPatched orchestrator.StatusPhase
		lastPatchAt time.Time
		want        bool
	}{
		{"first ever", orchestrator.StatusUpdate{Phase: orchestrator.PhaseTransferring}, "", time.Time{}, true},
		{"phase transition", orchestrator.StatusUpdate{Phase: orchestrator.PhaseCutover}, orchestrator.PhaseTransferring, recent, true},
		{"terminal", orchestrator.StatusUpdate{Phase: orchestrator.PhaseFailed}, orchestrator.PhaseFailed, recent, true},
		{
			name:        "ram refresh too soon",
			u:           orchestrator.StatusUpdate{Phase: orchestrator.PhaseTransferring, RAMTransferred: 5},
			lastPatched: orchestrator.PhaseTransferring,
			lastPatchAt: recent,
			want:        false,
		},
		{
			name:        "ram refresh after interval",
			u:           orchestrator.StatusUpdate{Phase: orchestrator.PhaseTransferring, RAMTransferred: 5},
			lastPatched: orchestrator.PhaseTransferring,
			lastPatchAt: old,
			want:        true,
		},
		{
			name:        "error bearing",
			u:           orchestrator.StatusUpdate{Phase: orchestrator.PhaseTransferring, Error: errors.New("boom")},
			lastPatched: orchestrator.PhaseTransferring,
			lastPatchAt: recent,
			want:        true,
		},
		{
			name:        "applied downtime fact",
			u:           orchestrator.StatusUpdate{Phase: orchestrator.PhaseTransferring, AppliedDowntimeMS: 30},
			lastPatched: orchestrator.PhaseTransferring,
			lastPatchAt: recent,
			want:        true,
		},
		{
			name:        "measured downtime fact",
			u:           orchestrator.StatusUpdate{Phase: orchestrator.PhaseTransferring, DowntimeMS: 7},
			lastPatched: orchestrator.PhaseTransferring,
			lastPatchAt: recent,
			want:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldPatchStatusUpdate(tc.u, tc.lastPatched, tc.lastPatchAt); got != tc.want {
				t.Fatalf("shouldPatchStatusUpdate = %v, want %v", got, tc.want)
			}
		})
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

// TestReconciler_DeletionDefersUntilCancelRegistered pins the closed
// claim/cancel window: a tracking entry whose worker has not yet reached
// setTrackCancel must NOT be finalizer-released by handleDeletion, or the
// unstoppable worker would keep creating Jobs for a CR that is already gone.
// The next deletion pass (after the cancel is registered) completes cleanup.
func TestReconciler_DeletionDefersUntilCancelRegistered(t *testing.T) {
	cr := newMigrationCR("m-deldefer", []string{finalizerName}, false, nil)
	orch := &fakeOrch{}
	rec, dyn, _ := newReconcilerWithCR(t, orch, cr)

	key := types.NamespacedName{Namespace: "default", Name: "m-deldefer"}
	if !rec.markTracking(key) {
		t.Fatal("markTracking: expected first claim")
	}

	// Simulate dispatch between markTracking and setTrackCancel.
	rec.handleDeletion(context.Background(), key, newMigrationCR("m-deldefer", []string{finalizerName}, true, nil))

	if calls := orch.callsFor("Stop"); len(calls) != 0 {
		t.Fatalf("Stop called while worker cancel was unregistered: %v", calls)
	}
	got, err := dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m-deldefer", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR after deferred deletion: %v", err)
	}
	if !hasFinalizer(got) {
		t.Fatal("finalizer removed before the worker's cancel func was registered; dispatch could create Jobs for a deleted Migration")
	}
	rec.mu.Lock()
	_, stillTracked := rec.tracking[key]
	rec.mu.Unlock()
	if !stillTracked {
		t.Fatal("tracking entry dropped during deferred deletion; worker would run untracked")
	}

	// Now the worker registers its cancel; the next deletion pass must
	// complete cleanup: cancel invoked + finalizer removed + untracked.
	cancelled := make(chan struct{})
	rec.setTrackCancel(key, func() { close(cancelled) })
	rec.handleDeletion(context.Background(), key, newMigrationCR("m-deldefer", []string{finalizerName}, true, nil))

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handleDeletion did not invoke the registered cancel func")
	}
	got, err = dyn.Resource(MigrationGVR).Namespace("default").Get(context.Background(), "m-deldefer", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR after completed deletion: %v", err)
	}
	if hasFinalizer(got) {
		t.Fatalf("finalizer still present after completed deletion: %v", got.GetFinalizers())
	}
	rec.mu.Lock()
	_, tracked := rec.tracking[key]
	rec.mu.Unlock()
	if tracked {
		t.Fatal("tracking entry survived completed deletion")
	}
}

// TestReconciler_DeletionCancelsRecoveryLoop verifies that recover registers
// a cancellable context with its tracking entry, so CR deletion stops the
// recovery loop instead of leaving it polling (and running post-success side
// effects) for up to StatusTimeout after the Migration was deleted.
func TestReconciler_DeletionCancelsRecoveryLoop(t *testing.T) {
	cr := newMigrationCR("m-delrec", []string{finalizerName}, false, map[string]any{
		"phase":       "transferring",
		"migrationID": "id-delrec",
	})
	// Source job alive and never terminal; dest absent: recover loops and
	// retries Resume every tick until cancelled.
	srcJob := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "katamaran-source-id-delrec",
			Namespace: orchestrator.DefaultJobNamespace,
			Labels: map[string]string{
				orchestrator.MigrationIDLabel: "id-delrec",
				"app.kubernetes.io/component": "source",
			},
		},
	}
	orch := &fakeOrch{}
	rec, _, _ := newReconcilerWithCR(t, orch, cr, srcJob)
	rec.StatusTimeout = 10 * time.Minute

	if err := rec.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}

	key := types.NamespacedName{Namespace: "default", Name: "m-delrec"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		tk, ok := rec.tracking[key]
		registered := ok && tk.cancel != nil
		rec.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recover goroutine never registered its tracking cancel func")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give recover at least one tick so the test would fail without the fix
	// (it would keep looping forever after deletion).
	if waits := len(orch.callsFor("Resume")); waits == 0 {
		t.Log("note: Resume had not been called yet when deletion fired; loop cancellation is still exercised")
	}

	rec.handleDeletion(context.Background(), key, newMigrationCR("m-delrec", []string{finalizerName}, true, nil))

	deadline = time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		_, tracked := rec.tracking[key]
		rec.mu.Unlock()
		if !tracked {
			return // recovery loop observed the cancellation and exited
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery loop kept running after CR deletion")
		}
		time.Sleep(5 * time.Millisecond)
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

// TestTickOnce_SurvivesPanicAndListErrors pins tickOnce's recovery
// contract: Run drives tickOnce on a ticker for the life of the leader,
// so a panicking reconcile pass must be absorbed (counted in
// mWorkerPanics) and a List failure must surface as a counted
// mReconcileErrors bump — neither may kill the goroutine, which would
// silently halt all reconciliation while the pod stays "healthy".
func TestTickOnce_SurvivesPanicAndListErrors(t *testing.T) {
	// Not parallel: asserts deltas on process-global expvar counters.
	cr := newMigrationCR("m-tick", nil, false, nil)
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr)

	startPanics := mWorkerPanics.Value()
	dyn.PrependReactor("*", "*", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		panic("synthetic reconcile panic")
	})
	rec.tickOnce(context.Background()) // must not panic out of the test
	if delta := mWorkerPanics.Value() - startPanics; delta < 1 {
		t.Fatalf("mWorkerPanics delta = %d, want >= 1 after panicking tick", delta)
	}

	// A prepended non-panicking reaction shadows the panic one.
	startErrors := mReconcileErrors.Value()
	dyn.PrependReactor("*", "*", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("synthetic list failure")
	})
	rec.tickOnce(context.Background())
	if delta := mReconcileErrors.Value() - startErrors; delta < 1 {
		t.Fatalf("mReconcileErrors delta = %d, want >= 1 after failed tick", delta)
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
	req, err := specToRequest(obj, "default")
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
	req, err := specToRequest(obj, "default")
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

// When the source pod is already gone (lookup fails), the adoption-pending
// mark must be skipped without panicking and the migration still reports
// success. Pins the failure path so it stays a logged no-op rather than a
// silent or crashing one.
func TestReconciler_AdoptVM_MissingSourcePodSkipsPendingMark(t *testing.T) {
	const rsUID types.UID = "rs-uid-missing"
	updates := make(chan orchestrator.StatusUpdate, 1)
	updates <- orchestrator.StatusUpdate{ID: "id-missing", Phase: orchestrator.PhaseSucceeded}
	close(updates)
	orch := &fakeOrch{applyID: "id-missing", updates: updates}
	cr := adoptCR("m-missing", "none")
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "Migration"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "MigrationList"}, &unstructured.UnstructuredList{})
	dyn := fakedyn.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		MigrationGVR: "MigrationList",
	}, cr)
	// No pod seeded: the source-pod Get in handleMigrationOutcome fails.
	kube := fakekube.NewSimpleClientset()
	rec := NewReconciler(dyn, kube, orch, nil)
	rec.PollInterval = 10 * time.Millisecond
	rec.StatusTimeout = 1 * time.Second
	rec.Discoverer = &fakeDiscoverer{podNode: "worker-a", nodeIP: "10.0.0.20"}

	rec.dispatch(context.Background(), types.NamespacedName{Namespace: "default", Name: "m-missing"}, cr)

	if got := rec.pending.MigrationFor(rsUID); got != "" {
		t.Fatalf("pending mark for %s = %q after failed source-pod lookup, want unmarked", rsUID, got)
	}
}

// sourceCleanup=delete deletes the source pod before createAdoptionPod
// runs, so the adoption pod's label/owner inheritance must come from the
// pre-cleanup lookup in handleMigrationOutcome. Regression: inheritance
// used to re-fetch the pod after deletion, always hit NotFound on this
// path, and silently produced adoption pods the ReplicaSet did not own
// (defeating Strategy A part 1 for every delete/orphan migration).
func TestReconciler_AdoptVM_DeleteCleanupInheritsLabels(t *testing.T) {
	const rsUID types.UID = "rs-uid-inherit"
	const migID = "id-inherit"
	updates := make(chan orchestrator.StatusUpdate, 1)
	updates <- orchestrator.StatusUpdate{ID: migID, Phase: orchestrator.PhaseSucceeded}
	close(updates)
	orch := &fakeOrch{applyID: migID, updates: updates}

	pod := sourcePodOwnedBy("ReplicaSet", rsUID)
	pod.Labels = map[string]string{"app": "kata-demo", "pod-template-hash": "abc123"}
	disc := &fakeDiscoverer{podNode: "worker-a", nodeIP: "10.0.0.20"}
	cr := adoptCR("m-inherit", "delete")
	// Pin an explicit dest node so the adoption path skips auto-select
	// resolution (this test exercises inheritance, not node discovery).
	_ = unstructured.SetNestedField(cr.Object, "worker-b", "spec", "destNode")
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "Migration"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "katamaran.io", Version: "v1alpha1", Kind: "MigrationList"}, &unstructured.UnstructuredList{})
	dyn := fakedyn.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		MigrationGVR: "MigrationList",
		{Group: "", Version: "v1", Resource: "pods"}: "PodList",
	}, cr)
	kube := fakekube.NewSimpleClientset(pod)
	rec := NewReconciler(dyn, kube, orch, disc)
	rec.PollInterval = 10 * time.Millisecond
	rec.StatusTimeout = 1 * time.Second

	savedDelay := adoptionVMConfigDelay
	adoptionVMConfigDelay = time.Millisecond
	t.Cleanup(func() { adoptionVMConfigDelay = savedDelay })

	rec.dispatch(context.Background(), types.NamespacedName{Namespace: "default", Name: "m-inherit"}, cr)

	if deleted := disc.deletedPodsSnapshot(); len(deleted) != 1 || deleted[0] != "default/kata-demo" {
		t.Fatalf("deleted pods = %v, want [default/kata-demo]; cleanup must run before adoption for this regression", deleted)
	}
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
	for _, want := range []string{"app", "pod-template-hash"} {
		if got, _, _ := unstructured.NestedString(adopted.Object, "metadata", "labels", want); got == "" {
			t.Fatalf("adoption pod missing inherited label %q; metadata=%v", want, adopted.Object["metadata"])
		}
	}
	refs, found, _ := unstructured.NestedSlice(adopted.Object, "metadata", "ownerReferences")
	if !found || len(refs) == 0 {
		t.Fatalf("adoption pod has no ownerReferences; metadata=%v", adopted.Object["metadata"])
	}
}

// patchStatusRetry is the crash-recovery anchor for phase=Submitted: a lost
// write there re-dispatches duplicate Jobs against the same VM after a
// controller restart. It must retry transient failures to success, and give
// up with an error (not panic, not loop forever) once the budget runs out.
func TestPatchStatusRetry_RetriesTransientFailures(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "m-retry"}
	cr := newMigrationCR("m-retry", nil, false, nil)
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr)

	attempts := 0
	dyn.PrependReactor("*", "*", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts < 3 {
			return true, nil, errors.New("synthetic patch failure")
		}
		return false, nil, nil // let the real client handle it
	})
	if err := rec.patchStatusRetry(context.Background(), key, "id-x", string(orchestrator.PhaseSubmitted), "msg", ""); err != nil {
		t.Fatalf("patchStatusRetry = %v, want success on attempt 3", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestPatchStatusRetry_ExhaustsBudget(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "m-retry2"}
	cr := newMigrationCR("m-retry2", nil, false, nil)
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr)

	savedBackoff := submittedPatchBackoff
	submittedPatchBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { submittedPatchBackoff = savedBackoff })

	attempts := 0
	dyn.PrependReactor("*", "*", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, errors.New("synthetic patch failure")
	})
	if err := rec.patchStatusRetry(context.Background(), key, "id-y", string(orchestrator.PhaseSubmitted), "msg", ""); err == nil {
		t.Fatal("patchStatusRetry = nil, want error after exhausting the retry budget")
	}
	if attempts != submittedPatchAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, submittedPatchAttempts)
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
		deleted := disc.deletedPodsSnapshot()
		if len(deleted) == 1 && deleted[0] == "default/kata-demo" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery deleted pods %v, want [default/kata-demo]", disc.deletedPodsSnapshot())
}
