package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPendingAdoption_MarkAndExpire(t *testing.T) {
	t.Parallel()
	reg := newPendingAdoptionRegistry()
	uid := types.UID("rs-1")
	reg.Mark(uid, "mig-abc")
	if got := reg.MigrationFor(uid); got != "mig-abc" {
		t.Fatalf("MigrationFor = %q, want mig-abc", got)
	}
	// Force-expire by rewinding the entry's expiresAt.
	reg.mu.Lock()
	e := reg.entries[uid]
	e.expiresAt = time.Now().Add(-time.Second)
	reg.entries[uid] = e
	reg.mu.Unlock()
	if got := reg.MigrationFor(uid); got != "" {
		t.Fatalf("MigrationFor after expiry = %q, want empty", got)
	}
}

// A later Mark must sweep entries that expired but were never re-queried
// (the leak path: a reconciler that Marks then crashes before Clear, whose
// controller never creates another pod the webhook checks). MigrationFor
// only prunes the single UID it reads, so without the sweep in Mark such
// entries would accumulate unboundedly.
func TestPendingAdoption_MarkPrunesExpiredEntries(t *testing.T) {
	t.Parallel()
	reg := newPendingAdoptionRegistry()
	stale := types.UID("rs-stale")
	reg.Mark(stale, "mig-stale")
	// Force-expire the stale entry without ever reading it back.
	reg.mu.Lock()
	e := reg.entries[stale]
	e.expiresAt = time.Now().Add(-time.Second)
	reg.entries[stale] = e
	reg.mu.Unlock()

	// A Mark for a different UID must garbage-collect the expired entry.
	reg.Mark("rs-fresh", "mig-fresh")

	reg.mu.Lock()
	_, stillThere := reg.entries[stale]
	count := len(reg.entries)
	reg.mu.Unlock()
	if stillThere {
		t.Fatal("expired entry survived a later Mark; sweep did not run")
	}
	if count != 1 {
		t.Fatalf("registry holds %d entries, want 1 (only the fresh mark)", count)
	}
}

func TestPendingAdoption_Clear(t *testing.T) {
	t.Parallel()
	reg := newPendingAdoptionRegistry()
	reg.Mark("rs-2", "mig-xyz")
	reg.Clear("rs-2")
	if got := reg.MigrationFor("rs-2"); got != "" {
		t.Fatalf("MigrationFor after Clear = %q, want empty", got)
	}
}

func TestPendingAdoption_EmptyUIDIgnored(t *testing.T) {
	t.Parallel()
	reg := newPendingAdoptionRegistry()
	reg.Mark("", "mig-x")
	reg.Clear("")
	if got := reg.MigrationFor(""); got != "" {
		t.Fatalf("MigrationFor empty UID = %q, want empty", got)
	}
}

func TestShouldDenyPodCreate_RSReplacementDuringPending(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	rsUID := types.UID("rs-1")
	r.pending.Mark(rsUID, "mig-abc")
	ctrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", UID: rsUID, Controller: &ctrl},
			},
		},
	}
	got := r.ShouldDenyPodCreate(pod)
	if got == "" {
		t.Fatal("ShouldDenyPodCreate should deny RS-driven pod for pending adoption RS")
	}
	// The deny reason must identify the blocking migration and the owner kind
	// so operators can trace why a replacement pod was rejected.
	if !strings.HasPrefix(got, "katamaran migration ") {
		t.Fatalf("deny reason missing katamaran prefix: %q", got)
	}
	if !strings.Contains(got, "mig-abc") {
		t.Fatalf("deny reason omits migration ID: %q", got)
	}
	if !strings.Contains(got, "ReplicaSet") {
		t.Fatalf("deny reason omits owner kind: %q", got)
	}
}

func TestShouldDenyPodCreate_NoPendingAllowsAll(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	ctrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", UID: "rs-other", Controller: &ctrl},
			},
		},
	}
	if got := r.ShouldDenyPodCreate(pod); got != "" {
		t.Fatalf("ShouldDenyPodCreate without pending entry = %q, want empty", got)
	}
}

// TestShouldDenyPodCreate_StatefulSetOwnerDenied / DaemonSetOwnerDenied
// confirm Strategy A's Mark applies to all built-in pod controllers
// that auto-create replacement pods, not just ReplicaSet. Live e2e on
// the 3-node minikube cluster verified this end-to-end against a
// StatefulSet workload after the kind switch.
func TestShouldDenyPodCreate_StatefulSetOwnerDenied(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	r.pending.Mark("sts-1", "mig-sts")
	ctrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", UID: "sts-1", Controller: &ctrl},
			},
		},
	}
	if got := r.ShouldDenyPodCreate(pod); got == "" {
		t.Fatal("StatefulSet pod with marked owner UID must be denied")
	}
}

func TestShouldDenyPodCreate_DaemonSetOwnerDenied(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	r.pending.Mark("ds-1", "mig-ds")
	ctrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", UID: "ds-1", Controller: &ctrl},
			},
		},
	}
	if got := r.ShouldDenyPodCreate(pod); got == "" {
		t.Fatal("DaemonSet pod with marked owner UID must be denied")
	}
}

func TestShouldDenyPodCreate_UnmanagedKindAllowed(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	r.pending.Mark("foo-1", "mig-x")
	ctrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "FooController", UID: "foo-1", Controller: &ctrl},
			},
		},
	}
	if got := r.ShouldDenyPodCreate(pod); got != "" {
		t.Fatalf("ShouldDenyPodCreate for unmanaged-kind owner = %q, want empty", got)
	}
}

func TestShouldDenyPodCreate_NoControllerRefAllowed(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	r.pending.Mark("rs-1", "mig-abc")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", UID: "rs-1"}, // no Controller pointer
			},
		},
	}
	if got := r.ShouldDenyPodCreate(pod); got != "" {
		t.Fatalf("ShouldDenyPodCreate for non-controller owner = %q, want empty", got)
	}
}

// TestShouldDenyPodCreate_AdoptionPodBypassesEvenWithMatchingMark
// locks the bypass: the controller's own adoption pod inherits the
// source pod's ownerReferences (Strategy A part 1) so without the
// bypass the webhook would deny the pod it's supposed to let through.
// Live regression test for the T3b run where the first adoption-pod
// create attempt was rejected by its own webhook.
func TestShouldDenyPodCreate_AdoptionPodBypassesEvenWithMatchingMark(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	rsUID := types.UID("rs-1")
	r.pending.Mark(rsUID, "mig-abc")
	ctrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/name":      "katamaran",
				"app.kubernetes.io/component": "adopted-vm",
				"katamaran.io/source-pod":     "src-foo",
				"app":                         "kata-deploy-test", // inherited from source
			},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", UID: rsUID, Controller: &ctrl},
			},
		},
	}
	if got := r.ShouldDenyPodCreate(pod); got != "" {
		t.Fatalf("adoption pod with matching pending RSUID must bypass webhook; got deny reason %q", got)
	}
}

func TestShouldDenyPodCreate_NilSafe(t *testing.T) {
	t.Parallel()
	var r *Reconciler
	if got := r.ShouldDenyPodCreate(&corev1.Pod{}); got != "" {
		t.Fatalf("nil reconciler should return empty; got %q", got)
	}
	r = &Reconciler{}
	if got := r.ShouldDenyPodCreate(nil); got != "" {
		t.Fatalf("nil pod should return empty; got %q", got)
	}
}
