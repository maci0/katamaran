package orchestrator

import (
	"testing"
	"time"
)

// TestDrainInBackground_UnblocksBlockedProducer pins the contract the
// Watch-stream abandonment defers rely on: once the primary consumer is
// gone, DrainInBackground keeps receiving until the channel closes so a
// producer blocked on send can always finish and run its cleanup.
func TestDrainInBackground_UnblocksBlockedProducer(t *testing.T) {
	t.Parallel()
	ch := make(chan StatusUpdate)
	produced := make(chan int, 1)
	go func() {
		defer close(ch)
		n := 0
		for i := 0; i < 3; i++ {
			// Blocking send: with no receiver these would wedge forever.
			ch <- StatusUpdate{ID: "drain-test"}
			n++
		}
		produced <- n
	}()

	// Let the producer block on its first send before draining.
	time.Sleep(10 * time.Millisecond)
	DrainInBackground(ch)

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
	ch := make(chan StatusUpdate, 2)
	ch <- StatusUpdate{ID: "a"}
	close(ch)
	done := make(chan struct{})
	go func() {
		DrainInBackground(ch)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DrainInBackground did not return on closed channel")
	}
}
