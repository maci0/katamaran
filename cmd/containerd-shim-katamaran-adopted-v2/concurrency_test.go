package main

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
)

// TestAdoptedTaskServiceStartArmsWatcherOnce races concurrent Start calls,
// which is what containerd's concurrent ttrpc dispatch produces. watchStarted
// is the only thing keeping a duplicate Start from leaking a second
// /proc-polling watchExit goroutine per RPC.
func TestAdoptedTaskServiceStartArmsWatcherOnce(t *testing.T) {
	t.Parallel()

	svc := newAdoptedTaskService(func(string, ...any) {})

	// A pid that is already gone: watchExit observes the exit and returns
	// instead of polling for the life of the test.
	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Skipf("cannot spawn helper process: %v", err)
	}
	deadPID := child.Process.Pid
	_ = child.Wait()

	svc.mu.Lock()
	svc.qemuPid = deadPID
	svc.mu.Unlock()

	const starts = 32
	var wg sync.WaitGroup
	for range starts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Start(context.Background(), &taskAPI.StartRequest{}); err != nil {
				t.Errorf("Start: %v", err)
			}
		}()
	}
	wg.Wait()

	svc.mu.Lock()
	watchStarted := svc.watchStarted
	svc.mu.Unlock()
	if !watchStarted {
		t.Fatal("watchStarted is false after concurrent Start calls")
	}
	// The armed watcher must have run to completion and recorded the exit.
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.mu.Lock()
		done, armed := svc.exited, svc.watchStarted
		svc.mu.Unlock()
		if done {
			// watchStarted is latched: a later Start must not arm a
			// second watcher for the same task.
			if !armed {
				t.Fatal("watchStarted was cleared; a later Start would arm a second watcher")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the armed watchExit never recorded the exit")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAdoptedTaskServiceWaitConcurrentHandOff exercises the waiter hand-off:
// many Wait calls register under the lock, the exit swaps the slice out and
// closes every channel, and callers that give up delete their own channel from
// the slice. All of it happens on the goroutines containerd dispatches RPCs on.
func TestAdoptedTaskServiceWaitConcurrentHandOff(t *testing.T) {
	t.Parallel()

	svc := newAdoptedTaskService(func(string, ...any) {})
	svc.mu.Lock()
	svc.id = "task-1"
	svc.qemuPid = os.Getpid()
	svc.startedAt = time.Now()
	svc.mu.Unlock()

	ctx, cancelWaits := context.WithCancel(context.Background())
	defer cancelWaits()

	const waiters = 8
	exited := make(chan struct{}, waiters)
	var wg sync.WaitGroup
	for i := range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Wait(ctx, &taskAPI.WaitRequest{}); err == nil {
				exited <- struct{}{}
			}
		}()
		waitForWaiters(t, svc, i+1)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if _, err := svc.State(context.Background(), &taskAPI.StateRequest{}); err != nil {
					t.Errorf("State: %v", err)
					return
				}
				if _, err := svc.Pids(context.Background(), &taskAPI.PidsRequest{}); err != nil {
					t.Errorf("Pids: %v", err)
					return
				}
				if _, err := svc.Connect(context.Background(), &taskAPI.ConnectRequest{}); err != nil {
					t.Errorf("Connect: %v", err)
					return
				}
				if _, err := svc.Delete(context.Background(), &taskAPI.DeleteRequest{}); err != nil {
					t.Errorf("Delete: %v", err)
					return
				}
			}
		}()
	}
	// Concurrent Shutdown calls must not double-close, which panics.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 16 {
			if _, err := svc.Shutdown(context.Background(), &taskAPI.ShutdownRequest{}); err != nil {
				t.Errorf("Shutdown: %v", err)
				return
			}
		}
	}()

	// Record the exit the way watchExit does: swap the slice out under the
	// lock, close the channels outside it.
	svc.mu.Lock()
	pending := svc.waiters
	svc.waiters = nil
	svc.exited = true
	svc.exitedAt = time.Now()
	svc.exitCode = 7
	svc.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cancelWaits()
		t.Fatal("handlers did not return; a waiter was stranded on a channel nobody closes")
	}
	if n := len(exited); n != waiters {
		t.Fatalf("%d of %d waiters observed the exit, want %d", n, waiters, waiters)
	}

	// A Wait after the exit returns immediately with the recorded state.
	resp, err := svc.Wait(context.Background(), &taskAPI.WaitRequest{})
	if err != nil {
		t.Fatalf("Wait after exit: %v", err)
	}
	if resp.ExitStatus != 7 {
		t.Fatalf("Wait after exit returned status %d, want 7", resp.ExitStatus)
	}
}

// TestAdoptedTaskServiceWaitCancelDropsChannel pins the caller-gave-up path:
// a Wait whose context is cancelled removes its channel from the slice, so a
// later exit does not close (or retain) a channel nobody is listening on.
func TestAdoptedTaskServiceWaitCancelDropsChannel(t *testing.T) {
	t.Parallel()

	svc := newAdoptedTaskService(func(string, ...any) {})
	svc.mu.Lock()
	svc.qemuPid = os.Getpid()
	svc.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := svc.Wait(ctx, &taskAPI.WaitRequest{}); err == nil {
			t.Error("Wait on a cancelled context returned success")
		}
	}()
	waitForWaiters(t, svc, 1)
	cancel()
	<-done

	svc.mu.Lock()
	n := len(svc.waiters)
	svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d waiter channels left registered after every Wait gave up", n)
	}
}

// waitForWaiters blocks until the service has registered at least n waiters.
func waitForWaiters(t *testing.T, svc *adoptedTaskService, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.mu.Lock()
		got := len(svc.waiters)
		svc.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d waiters registered", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}
