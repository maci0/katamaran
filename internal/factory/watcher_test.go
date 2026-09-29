package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/migration"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestWatcherScanOffersMetadataAndConfigOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv := NewServer()
	watcher := NewWatcher(dir, srv)
	state := MigrationState{
		ID:              "mig-1",
		QEMUPid:         1234,
		VirtiofsdPid:    5678,
		HypervisorState: json.RawMessage(`{"pid":1234}`),
		CPU:             8,
		Memory:          4096,
		VMConfig:        json.RawMessage(`{"HypervisorType":"qemu"}`),
		AgentConfig:     json.RawMessage(`{"Debug":true}`),
	}
	writeMigrationMeta(t, dir, "sandbox-a", state)

	watcher.scan()
	assertStatus(t, srv, []wantVMStatus{{pid: 1234, cpu: 8, memory: 4096}})

	cfg, err := srv.Config(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Config after scan: %v", err)
	}
	if !bytes.Equal(cfg.Data, state.VMConfig) {
		t.Fatalf("Config.Data = %s, want %s", cfg.Data, state.VMConfig)
	}
	if !bytes.Equal(cfg.AgentConfig, state.AgentConfig) {
		t.Fatalf("Config.AgentConfig = %s, want %s", cfg.AgentConfig, state.AgentConfig)
	}

	got, err := srv.GetBaseVM(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetBaseVM after scan: %v", err)
	}
	assertVM(t, got, state)

	watcher.scan()
	assertStatus(t, srv, nil)
}

func TestWatcherScanIgnoresInvalidMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv := NewServer()
	watcher := NewWatcher(dir, srv)
	sandboxDir := filepath.Join(dir, "sandbox-b")
	if err := os.Mkdir(sandboxDir, 0o755); err != nil {
		t.Fatalf("mkdir sandbox: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandboxDir, migration.MigrationMetaFile), []byte(`{"id":`), 0o600); err != nil {
		t.Fatalf("write invalid metadata: %v", err)
	}

	watcher.scan()
	assertStatus(t, srv, nil)
}

func TestWatcherScanProcessesRecreatedSandboxPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv := NewServer()
	watcher := NewWatcher(dir, srv)

	first := MigrationState{ID: "first", QEMUPid: 1, VirtiofsdPid: 11, HypervisorState: json.RawMessage(`{"id":"first"}`)}
	second := MigrationState{ID: "second", QEMUPid: 2, VirtiofsdPid: 22, HypervisorState: json.RawMessage(`{"id":"second"}`)}
	writeMigrationMeta(t, dir, "sandbox-c", first)
	watcher.scan()
	assertStatus(t, srv, []wantVMStatus{{pid: 1}})

	got, err := srv.GetBaseVM(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetBaseVM first: %v", err)
	}
	assertVM(t, got, first)

	if err := os.RemoveAll(filepath.Join(dir, "sandbox-c")); err != nil {
		t.Fatalf("remove sandbox: %v", err)
	}
	watcher.scan()

	writeMigrationMeta(t, dir, "sandbox-c", second)
	watcher.scan()
	assertStatus(t, srv, []wantVMStatus{{pid: 2}})
	got, err = srv.GetBaseVM(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetBaseVM second: %v", err)
	}
	assertVM(t, got, second)
}

// A sandbox whose migration-meta.json cannot be read fails on every poll
// for as long as it stays broken. The scan must not re-log the failure at
// WARN on every tick (that noise trains operators to ignore warnings), and
// the escalation counter must reset once the offending path is gone.
func TestWatcherScanThrottlesRepeatedReadFailures(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer()
	watcher := NewWatcher(dir, srv)

	// A directory where a file is expected makes os.ReadFile fail with EISDIR
	// for every user, including root, so the test does not depend on
	// permission bits being enforced.
	broken := filepath.Join(dir, "sandbox-broken", migration.MigrationMetaFile)
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", broken, err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	for i := 1; i <= 9; i++ {
		watcher.scan()
		if got, want := watcher.consecutiveErrors, i; got != want {
			t.Fatalf("consecutiveErrors after %d scans = %d, want %d", i, got, want)
		}
	}
	if got := logs.String(); strings.Contains(got, "level=WARN") {
		t.Fatalf("scan failures escalated to WARN before the 10th occurrence:\n%s", got)
	}

	watcher.scan()
	if got, want := watcher.consecutiveErrors, 10; got != want {
		t.Fatalf("consecutiveErrors = %d, want %d", got, want)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("10th consecutive scan failure did not log at WARN:\n%s", logs.String())
	}

	if err := os.RemoveAll(filepath.Join(dir, "sandbox-broken")); err != nil {
		t.Fatalf("remove broken sandbox: %v", err)
	}
	watcher.scan()
	if got := watcher.consecutiveErrors; got != 0 {
		t.Fatalf("consecutiveErrors after a clean scan = %d, want 0", got)
	}
}

func writeMigrationMeta(t *testing.T, root, sandbox string, state MigrationState) {
	t.Helper()

	sandboxDir := filepath.Join(root, sandbox)
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sandboxDir, err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal migration state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandboxDir, migration.MigrationMetaFile), data, 0o600); err != nil {
		t.Fatalf("write migration metadata: %v", err)
	}
}
