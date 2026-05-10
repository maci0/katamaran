package controller

import (
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
	if got[:8] != "katamara" {
		t.Fatalf("deny reason looks wrong: %q", got)
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

func TestShouldDenyPodCreate_NonRSOwnerAllowed(t *testing.T) {
	t.Parallel()
	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	r.pending.Mark("rs-1", "mig-abc")
	ctrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", UID: "rs-1", Controller: &ctrl},
			},
		},
	}
	if got := r.ShouldDenyPodCreate(pod); got != "" {
		t.Fatalf("ShouldDenyPodCreate for DaemonSet = %q, want empty", got)
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
