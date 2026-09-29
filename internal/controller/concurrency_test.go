package controller

import (
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// managedPod builds a replacement Pod owned by a managed controller, the shape
// ShouldDenyPodCreate is asked about on every admission request.
func managedPod(ownerUID types.UID, kind, name string) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			OwnerReferences: []metav1.OwnerReference{{
				UID:        ownerUID,
				Kind:       kind,
				Controller: &controller,
			}},
		},
	}
}

// TestPendingAdoptionRegistryConcurrent hammers Mark and MigrationFor from
// many goroutines. Mark also sweeps expired entries, so it mutates the whole
// map while readers look entries up: the sweep must not run against a
// concurrent reader, and Mark's expiry-driven delete must not race one.
func TestPendingAdoptionRegistryConcurrent(t *testing.T) {
	t.Parallel()

	const (
		writers  = 4
		readers  = 4
		perLoop  = 300
		ownerCnt = 16
	)
	reg := newPendingAdoptionRegistry()

	owners := make([]types.UID, ownerCnt)
	for i := range owners {
		owners[i] = types.UID(fmt.Sprintf("uid-%02d", i))
	}

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perLoop {
				reg.Mark(owners[(w*perLoop+i)%ownerCnt], fmt.Sprintf("mig-%d-%d", w, i))
			}
		}()
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perLoop {
				for _, uid := range owners {
					// The specific id depends on Mark ordering, so assert
					// the invariant instead: a hit is always a well-formed
					// migration id, and the lookup never panics on a map
					// being swept underneath it.
					if id := reg.MigrationFor(uid); id != "" && id[:4] != "mig-" {
						t.Errorf("MigrationFor(%s) = %q, want a mig-* id or empty", uid, id)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	// Nothing expired, so every owner is still readable.
	for _, uid := range owners {
		if reg.MigrationFor(uid) == "" {
			t.Errorf("owner %s lost its entry once the writers finished", uid)
		}
	}
}

// TestPendingAdoptionRegistryConcurrentExpiry drives Mark's expiry sweep from
// a clock the test advances mid-run, so entries are deleted while other
// goroutines are reading the same map.
func TestPendingAdoptionRegistryConcurrentExpiry(t *testing.T) {
	t.Parallel()

	reg := newPendingAdoptionRegistry()
	clockMu := sync.Mutex{}
	now := time.Now()
	reg.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	owners := make([]types.UID, 8)
	for i := range owners {
		owners[i] = types.UID(fmt.Sprintf("uid-%d", i))
	}

	stop := make(chan struct{})
	var bg sync.WaitGroup
	// Rewrites every entry, so the sweep always has work to do.
	bg.Add(1)
	go func() {
		defer bg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			reg.Mark(owners[i%len(owners)], "mig")
		}
	}()
	// Pushes the clock past every entry's expiry, forcing the delete path
	// in Mark to run concurrently with the readers below.
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			advance(pendingAdoptionTTL)
		}
	}()

	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 2000 {
				for _, uid := range owners {
					if id := reg.MigrationFor(uid); id != "" && id != "mig" {
						t.Errorf("MigrationFor(%s) = %q, want \"mig\" or empty", uid, id)
						return
					}
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	bg.Wait()
}

// TestShouldDenyPodCreateConcurrent runs the webhook decision function
// alongside the reconciler goroutines that Mark entries. The webhook serves
// admission on one goroutine per request while dispatch runs in parallel, so
// the registry map is read and written concurrently in production.
func TestShouldDenyPodCreateConcurrent(t *testing.T) {
	t.Parallel()

	r := &Reconciler{pending: newPendingAdoptionRegistry()}
	uid := types.UID("rs-uid")
	pod := managedPod(uid, "ReplicaSet", "replacement")
	if reason := r.ShouldDenyPodCreate(pod); reason != "" {
		t.Fatalf("ShouldDenyPodCreate before Mark = %q, want allowed", reason)
	}
	r.pending.Mark(uid, "mig-1")
	if reason := r.ShouldDenyPodCreate(pod); reason == "" {
		t.Fatal("ShouldDenyPodCreate after Mark = allowed, want denied")
	}

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				r.pending.Mark(uid, fmt.Sprintf("mig-%d-%d", w, i))
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_ = r.ShouldDenyPodCreate(pod)
				_ = r.ShouldDenyPodCreate(managedPod(uid, "ReplicaSet", "replacement"))
				_ = r.ShouldDenyPodCreate(managedPod(uid, "ConfigMap", "unmanaged"))
			}
		}()
	}
	wg.Wait()
}
