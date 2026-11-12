package signalctx

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestNotify pins the contract the commands rely on: the first SIGTERM
// cancels the context, and the stop func that runs on that cancellation
// releases the signal so a later one is delivered to whoever asks for it
// instead of being swallowed by the handler Notify installed.
func TestNotify(t *testing.T) {
	released := make(chan struct{})
	ctx, stop := notify(func() { close(released) })
	defer stop()

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled by SIGTERM")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the signal handler was not released after cancellation")
	}

	// The handler is gone, so a fresh one receives the next signal.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	defer signal.Stop(ch)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("second signal: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the later SIGTERM was swallowed; the handler was not released")
	}
}
