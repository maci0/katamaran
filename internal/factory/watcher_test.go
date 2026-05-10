package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
	if err := os.WriteFile(filepath.Join(sandboxDir, migrationMetaFile), []byte(`{"id":`), 0o600); err != nil {
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
	if err := os.WriteFile(filepath.Join(sandboxDir, migrationMetaFile), data, 0o600); err != nil {
		t.Fatalf("write migration metadata: %v", err)
	}
}
