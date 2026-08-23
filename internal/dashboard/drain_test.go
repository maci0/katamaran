package dashboard

import (
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// TestDrainInBackground_UnblocksBlockedProducer pins the contract the
// runOrchestrator abandonment defer relies on: once the primary consumer is
// gone, drainInBackground keeps receiving until the channel closes so a
// producer blocked on send can always finish and run its cleanup.
func TestDrainInBackground_UnblocksBlockedProducer(t *testing.T) {
	t.Parallel()
	ch := make(chan orchestrator.StatusUpdate)
	produced := make(chan int, 1)
	go func() {
		defer close(ch)
		n := 0
		for i := 0; i < 3; i++ {
			// Blocking send: with no receiver these would wedge forever.
			ch <- orchestrator.StatusUpdate{ID: "drain-test"}
			n++
		}
		produced <- n
	}()

	// Let the producer block on its first send before draining.
	time.Sleep(10 * time.Millisecond)
	drainInBackground(ch)

	select {
	case n := <-produced:
		if n != 3 {
			t.Fatalf("producer delivered %d updates, want 3", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer still blocked; drainer did not consume")
	}
}

// TestDrainInBackground_ClosedChannelNoOp verifies draining an already-closed
// channel returns immediately rather than blocking or panicking.
func TestDrainInBackground_ClosedChannelNoOp(t *testing.T) {
	t.Parallel()
	ch := make(chan orchestrator.StatusUpdate, 2)
	ch <- orchestrator.StatusUpdate{ID: "a"}
	close(ch)
	done := make(chan struct{})
	go func() {
		drainInBackground(ch)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainInBackground did not return on closed channel")
	}
}
