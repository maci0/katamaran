package dashboard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// fakeOrchestrator is a deterministic in-memory orchestrator.Orchestrator
// for the dashboard tests. Each instance is configured with a behaviour
// (success / fail / hang-until-stop) and records the most-recent Request.
//
// Replaces the previous migrate.sh shell-stubs: the dashboard calls Apply
// + Watch the same way it would a real orchestrator, so we exercise the
// real handler code path without spawning any child processes.
type fakeOrchestrator struct {
	mu          sync.Mutex
	lastRequest orchestrator.Request

	// behaviour controls the StatusUpdate stream sent on Watch.
	//   "success" — emit submitted + transferring + succeeded, then close.
	//   "fail"    — emit submitted + failed (with err), then close.
	//   "slow"    — emit submitted, hold the channel open until Stop is
	//               called or the test cleanup cancels the run.
	behaviour string

	// per-run state
	runs map[orchestrator.MigrationID]*fakeRun
}

type fakeRun struct {
	updates chan orchestrator.StatusUpdate
	stop    chan struct{}
}

func newFakeOrchestrator(behaviour string) *fakeOrchestrator {
	return &fakeOrchestrator{behaviour: behaviour, runs: map[orchestrator.MigrationID]*fakeRun{}}
}

func dummyOrchestrator(t *testing.T) *fakeOrchestrator {
	t.Helper()
	return newFakeOrchestrator("success")
}

func slowOrchestrator(t *testing.T) *fakeOrchestrator {
	t.Helper()
	return newFakeOrchestrator("slow")
}

func failingOrchestrator(t *testing.T) *fakeOrchestrator {
	t.Helper()
	return newFakeOrchestrator("fail")
}

// LastRequest returns a snapshot of the Request the orchestrator was last
// asked to Apply. Tests that previously asserted on migrate.sh argv use
// this to assert on the structured Request shape instead.
func (f *fakeOrchestrator) LastRequest() orchestrator.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequest
}

func (f *fakeOrchestrator) Apply(ctx context.Context, req orchestrator.Request) (orchestrator.MigrationID, error) {
	f.mu.Lock()
	f.lastRequest = req
	id := orchestrator.MigrationID(req.SourceNode + "-" + req.DestNode)
	if id == "-" {
		id = "fake-id"
	}
	run := &fakeRun{updates: make(chan orchestrator.StatusUpdate, 8), stop: make(chan struct{})}
	f.runs[id] = run
	f.mu.Unlock()

	go func() {
		defer close(run.updates)
		run.updates <- orchestrator.StatusUpdate{ID: id, Phase: orchestrator.PhaseSubmitted, When: time.Now()}
		switch f.behaviour {
		case "success":
			run.updates <- orchestrator.StatusUpdate{ID: id, Phase: orchestrator.PhaseTransferring, When: time.Now()}
			run.updates <- orchestrator.StatusUpdate{ID: id, Phase: orchestrator.PhaseSucceeded, When: time.Now()}
		case "fail":
			run.updates <- orchestrator.StatusUpdate{ID: id, Phase: orchestrator.PhaseFailed, When: time.Now(), Error: errors.New("synthetic failure")}
		case "slow":
			// Block until Stop() or context cancel.
			select {
			case <-run.stop:
				run.updates <- orchestrator.StatusUpdate{ID: id, Phase: orchestrator.PhaseFailed, When: time.Now(), Error: errors.New("stopped")}
			case <-ctx.Done():
				run.updates <- orchestrator.StatusUpdate{ID: id, Phase: orchestrator.PhaseFailed, When: time.Now(), Error: ctx.Err()}
			}
		}
	}()
	return id, nil
}

func (f *fakeOrchestrator) Watch(_ context.Context, id orchestrator.MigrationID) (<-chan orchestrator.StatusUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[id]
	if !ok {
		return nil, orchestrator.ErrUnknownID
	}
	return run.updates, nil
}

func (f *fakeOrchestrator) Stop(_ context.Context, id orchestrator.MigrationID) error {
	f.mu.Lock()
	run, ok := f.runs[id]
	f.mu.Unlock()
	if !ok {
		return orchestrator.ErrUnknownID
	}
	select {
	case <-run.stop:
	default:
		close(run.stop)
	}
	return nil
}
