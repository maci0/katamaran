package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maci0/katamaran/internal/factory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestTryLoadFromSandboxSetsConfigFromPersistJSON(t *testing.T) {
	t.Parallel()

	sbsDir := t.TempDir()
	persist := []byte(`{
		"Config": {
			"HypervisorType": "qemu",
			"HypervisorConfig": {"path": "/opt/kata/bin/qemu-system-x86_64"},
			"KataAgentConfig": {"debug": true}
		}
	}`)
	writePersistJSON(t, sbsDir, "sandbox-a", persist)

	srv := factory.NewServer()
	if !tryLoadFromSandbox(srv, sbsDir) {
		t.Fatal("tryLoadFromSandbox returned false, want true")
	}

	got, err := srv.Config(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Config after tryLoadFromSandbox: %v", err)
	}
	var vmConfig struct {
		HypervisorType   string          `json:"HypervisorType"`
		HypervisorConfig json.RawMessage `json:"HypervisorConfig"`
		AgentConfig      json.RawMessage `json:"AgentConfig"`
	}
	if err := json.Unmarshal(got.Data, &vmConfig); err != nil {
		t.Fatalf("unmarshal Config.Data: %v; raw=%s", err, got.Data)
	}
	if vmConfig.HypervisorType != "qemu" {
		t.Fatalf("HypervisorType = %q, want qemu", vmConfig.HypervisorType)
	}
	if !bytes.Contains(vmConfig.HypervisorConfig, []byte(`/opt/kata/bin/qemu-system-x86_64`)) {
		t.Fatalf("HypervisorConfig = %s, want qemu path", vmConfig.HypervisorConfig)
	}
	if !bytes.Equal(got.AgentConfig, []byte(`{"debug": true}`)) {
		t.Fatalf("AgentConfig = %s, want persisted KataAgentConfig", got.AgentConfig)
	}
	if !bytes.Equal(compactJSON(t, vmConfig.AgentConfig), compactJSON(t, got.AgentConfig)) {
		t.Fatalf("Config.Data AgentConfig = %s, want %s", vmConfig.AgentConfig, got.AgentConfig)
	}
}

func TestTryLoadFromSandboxReturnsFalseWithoutValidPersistJSON(t *testing.T) {
	t.Parallel()

	sbsDir := t.TempDir()
	writePersistJSON(t, sbsDir, "sandbox-a", []byte(`{"Config":`))

	srv := factory.NewServer()
	if tryLoadFromSandbox(srv, sbsDir) {
		t.Fatal("tryLoadFromSandbox returned true for invalid persist.json")
	}
	if _, err := srv.Config(context.Background(), &emptypb.Empty{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Config status = %v, want %v; err=%v", status.Code(err), codes.Unavailable, err)
	}
}

func writePersistJSON(t *testing.T, sbsDir, sandbox string, data []byte) {
	t.Helper()

	dir := filepath.Join(sbsDir, sandbox)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sandbox dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "persist.json"), data, 0o600); err != nil {
		t.Fatalf("write persist.json: %v", err)
	}
}

func compactJSON(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		t.Fatalf("compact JSON %s: %v", data, err)
	}
	return buf.Bytes()
}
