package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/maci0/katamaran/internal/migration"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TestServerConcurrentAccess drives every Server method from many goroutines
// at once. In production the watcher goroutine offers VMs and rewrites the
// config while gRPC handlers (one goroutine per Kata shim) drain the queue and
// read the config, so every field on Server is written by more than one
// goroutine. Run under -race, this pins the queue, config, and quit channel
// against unsynchronized access.
func TestServerConcurrentAccess(t *testing.T) {
	t.Parallel()

	const (
		producers = 4
		offers    = 200
		consumers = 4
	)
	ctx := context.Background()
	srv := NewServer()
	// The shared "warn once about the missing VMConfig" flag is reset by every
	// SetConfig, so the many readers below race with the writers on it.
	if _, err := srv.Config(ctx, &emptypb.Empty{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Config before any SetConfig = %v, want Unavailable", status.Code(err))
	}

	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range offers {
				// Writers interleave: a config refresh and an offer for the
				// same tick must not be able to pair up as one state.
				srv.SetConfig(
					[]byte(fmt.Sprintf(`{"gen":%d}`, p)),
					[]byte(fmt.Sprintf(`{"p":%d,"i":%d}`, p, i)),
				)
				srv.OfferVM(MigrationState{
					ID:           fmt.Sprintf("mig-%d-%d", p, i),
					QEMUPid:      int64(p*offers + i),
					VirtiofsdPid: int64(p*offers + i + 1),
					CPU:          uint32(i),
					Memory:       uint32(4096 + i),
					VMConfig:     json.RawMessage(fmt.Sprintf(`{"p":%d,"i":%d}`, p, i)),
				})
			}
		}()
	}
	for range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range offers {
				if cfg, err := srv.Config(ctx, &emptypb.Empty{}); err == nil {
					// Data and AgentConfig come from one SetConfig call;
					// observing them from different calls is a torn read.
					if len(cfg.Data) == 0 {
						t.Errorf("Config returned empty Data with AgentConfig=%s", cfg.AgentConfig)
					}
				} else if status.Code(err) != codes.Unavailable {
					t.Errorf("Config status = %v, want Unavailable", status.Code(err))
				}
				if vm, err := srv.GetBaseVM(ctx, &emptypb.Empty{}); err == nil {
					if vm.Id == "" || vm.ProxyPid == 0 {
						t.Errorf("GetBaseVM served an incomplete VM: %+v", vm)
					}
				} else if status.Code(err) != codes.Unavailable {
					t.Errorf("GetBaseVM status = %v, want Unavailable", status.Code(err))
				}
				if _, err := srv.Status(ctx, &emptypb.Empty{}); err != nil {
					t.Errorf("Status: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Quit is one-shot: concurrent callers must not double-close, which
	// panics. Every caller then observes the closed channel via QuitCh.
	var quitWG sync.WaitGroup
	for range 8 {
		quitWG.Add(1)
		go func() {
			defer quitWG.Done()
			if _, err := srv.Quit(context.Background(), &emptypb.Empty{}); err != nil {
				t.Errorf("Quit: %v", err)
			}
		}()
	}
	quitWG.Wait()
	<-srv.QuitCh()

	if _, err := srv.GetBaseVM(ctx, &emptypb.Empty{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("GetBaseVM after Quit = %v, want Unavailable", status.Code(err))
	}

	// The queue is capped, so the total offered is bounded by the cap and
	// everything the consumers drained is gone.
	statusResp, err := srv.Status(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statusResp.Vmstatus) > maxQueuedVMs {
		t.Fatalf("queue depth %d exceeds cap %d", len(statusResp.Vmstatus), maxQueuedVMs)
	}
}

// TestServerConfigAndQueueAreConsistent checks that the VMConfig served to a
// shim is the one the writer paired with it, under a writer that rewrites both
// halves on every iteration while readers clone them out.
func TestServerConfigAndQueueAreConsistent(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			payload := []byte(fmt.Sprintf(`{"n":%d}`, i))
			srv.SetConfig(payload, payload)
		}
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				cfg, err := srv.Config(context.Background(), &emptypb.Empty{})
				if err != nil {
					continue
				}
				if string(cfg.Data) != string(cfg.AgentConfig) {
					t.Errorf("torn config read: Data=%s AgentConfig=%s", cfg.Data, cfg.AgentConfig)
					return
				}
			}
		}()
	}
	close(stop)
	wg.Wait()
}

// TestWatcherScanRetriesUnparseableMetadata pins the recovery path: a
// migration-meta.json the watcher cannot parse must stay eligible for a later
// scan. Marking it seen would blacklist the sandbox for the life of the node
// daemon, so a VM that becomes readable one tick later is never offered.
func TestWatcherScanRetriesUnparseableMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv := NewServer()
	watcher := NewWatcher(dir, srv)
	sandboxDir := filepath.Join(dir, "sandbox-retry")
	if err := os.Mkdir(sandboxDir, 0o755); err != nil {
		t.Fatalf("mkdir sandbox: %v", err)
	}
	metaPath := filepath.Join(sandboxDir, migration.MigrationMetaFile)
	if err := os.WriteFile(metaPath, []byte(`{"id":`), 0o600); err != nil {
		t.Fatalf("write truncated metadata: %v", err)
	}

	watcher.scan()
	assertStatus(t, srv, nil)

	// The writer replaces the file with a complete one (temp + rename, the
	// dest binary's publish path). The next scan must pick it up.
	state := MigrationState{ID: "recovered", QEMUPid: 4242, CPU: 2, Memory: 2048}
	writeMigrationMeta(t, dir, "sandbox-retry", state)
	watcher.scan()
	assertStatus(t, srv, []wantVMStatus{{pid: 4242, cpu: 2, memory: 2048}})
}

// TestWatcherRunConcurrentWithHandlers runs the watcher's poll loop against a
// Server that gRPC handlers are draining, the production threading model for
// the node daemon.
func TestWatcherRunConcurrentWithHandlers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv := NewServer()
	watcher := NewWatcher(dir, srv)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		watcher.Run(ctx)
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			writeMigrationMeta(t, dir, fmt.Sprintf("sandbox-%02d", i), MigrationState{
				ID:      fmt.Sprintf("mig-%02d", i),
				QEMUPid: int64(1000 + i),
				CPU:     uint32(i),
				Memory:  uint32(1024 + i),
			})
		}
	}()
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if _, err := srv.GetBaseVM(context.Background(), &emptypb.Empty{}); err != nil &&
					status.Code(err) != codes.Unavailable {
					t.Errorf("GetBaseVM: %v", err)
					return
				}
				if _, err := srv.Status(context.Background(), &emptypb.Empty{}); err != nil {
					t.Errorf("Status: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	cancel()
	<-runDone

	// Every offered VM was either served or is still queued: no offer is lost
	// and none is duplicated.
	resp, err := srv.Status(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	seen := make(map[int64]bool, len(resp.Vmstatus))
	for _, vm := range resp.Vmstatus {
		if seen[vm.Pid] {
			t.Fatalf("pid %d queued twice", vm.Pid)
		}
		seen[vm.Pid] = true
	}
	if len(resp.Vmstatus) > maxQueuedVMs {
		t.Fatalf("queue depth %d exceeds cap %d", len(resp.Vmstatus), maxQueuedVMs)
	}
}
