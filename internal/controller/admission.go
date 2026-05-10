package controller

import (
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// pendingAdoption tracks ReplicaSet UIDs whose source pod was just
// deleted as part of a Migration with adoptVM=true. While the entry is
// present, the validating webhook denies any new Pod whose
// controllerRef points at this RS — preventing the RS from spawning
// a fresh cold replacement during the brief window between source-pod
// delete and adoption-pod create.
//
// Entries auto-expire after pendingAdoptionTTL so a crashing
// reconciler that fails to clear an entry doesn't block the RS
// indefinitely.
type pendingAdoption struct {
	rsUID       types.UID
	migrationID string
	expiresAt   time.Time
}

// pendingAdoptionTTL bounds how long a pending-adoption entry blocks
// RS replacements. Must be larger than the controller's adoption
// staging delay (5s) plus a safety margin for slow apiservers, but
// small enough that a stuck reconciler doesn't wedge the RS for hours.
const pendingAdoptionTTL = 60 * time.Second

// pendingAdoptionRegistry is the data store the webhook consults. It
// is keyed by RS UID so the per-Pod webhook check is O(1).
type pendingAdoptionRegistry struct {
	mu      sync.Mutex
	entries map[types.UID]pendingAdoption
}

func newPendingAdoptionRegistry() *pendingAdoptionRegistry {
	return &pendingAdoptionRegistry{entries: map[types.UID]pendingAdoption{}}
}

// Mark records that the RS identified by uid is in the source-deleted-
// adoption-pending window for the named migration.
func (p *pendingAdoptionRegistry) Mark(uid types.UID, migrationID string) {
	if uid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[uid] = pendingAdoption{
		rsUID:       uid,
		migrationID: migrationID,
		expiresAt:   time.Now().Add(pendingAdoptionTTL),
	}
}

// Clear removes the entry for uid (if any). Called after the adoption
// pod has been successfully created.
func (p *pendingAdoptionRegistry) Clear(uid types.UID) {
	if uid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, uid)
}

// MigrationFor returns the migration ID this RS is pending adoption
// for, or empty if no entry / expired. Expired entries are pruned
// here so the registry doesn't accumulate stale state.
func (p *pendingAdoptionRegistry) MigrationFor(uid types.UID) string {
	if uid == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return ""
	}
	if time.Now().After(e.expiresAt) {
		delete(p.entries, uid)
		return ""
	}
	return e.migrationID
}

// ShouldDenyPodCreate is the webhook's decision function. Returns a
// non-empty reason string when the pod must be denied, or "" when it
// should be allowed.
//
// Denies pods whose controllerRef points at a ReplicaSet currently
// flagged in pendingAdoption. The adopted pod itself bypasses because
// the controller creates it directly via the apiserver after clearing
// the registry entry; the webhook only sees RS-driven creates during
// the race window.
func (r *Reconciler) ShouldDenyPodCreate(pod *corev1.Pod) string {
	if r == nil || r.pending == nil || pod == nil {
		return ""
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == "ReplicaSet" {
			if migID := r.pending.MigrationFor(owner.UID); migID != "" {
				return "katamaran migration " + migID + " in progress; ReplicaSet pod replacement denied (adoption pod pending)"
			}
		}
	}
	return ""
}
