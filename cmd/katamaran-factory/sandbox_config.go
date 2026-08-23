package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/maci0/katamaran/internal/factory"
	"github.com/maci0/katamaran/internal/migration"
)

// loadVMConfig populates the VMConfig the Config RPC returns by reading any
// existing Kata sandbox persist.json on the node. If no sandbox is present yet,
// it starts a background poller that retries until one appears.
func loadVMConfig(srv *factory.Server, watchDir string) {
	sbsDir := filepath.Join(filepath.Dir(strings.TrimRight(watchDir, "/")), "sbs")
	if tryLoadFromSandbox(srv, sbsDir) {
		return
	}

	slog.Info("VMConfig not yet available; starting background poller", "sandbox_dir", sbsDir, "interval", "2s")
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if tryLoadFromSandboxSafe(srv, sbsDir) {
				return
			}
		}
	}()
}

// tryLoadFromSandboxSafe wraps tryLoadFromSandbox with per-attempt panic
// recovery. A recover at goroutine scope would let a single panic kill the
// poller permanently, leaving VMConfig unavailable forever and every
// subsequent adoption falling back to cold start with no further signal.
// Mirrors factory.Watcher.safeScan.
func tryLoadFromSandboxSafe(srv *factory.Server, sbsDir string) (loaded bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("VMConfig poller panic", "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	return tryLoadFromSandbox(srv, sbsDir)
}

func tryLoadFromSandbox(srv *factory.Server, sbsDir string) bool {
	entries, err := os.ReadDir(sbsDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		persistPath := filepath.Join(sbsDir, entry.Name(), "persist.json")
		raw, err := os.ReadFile(persistPath)
		if err != nil {
			continue
		}
		var persist struct {
			Config migration.KataPersistConfig `json:"Config"`
		}
		if err := json.Unmarshal(raw, &persist); err != nil {
			slog.Warn("Failed to parse Kata persist.json; skipping", "path", persistPath, "error", err)
			continue
		}

		vmCfg := migration.MarshalVMConfig(persist.Config.HypervisorType, persist.Config.HypervisorConfig, persist.Config.KataAgentConfig)
		srv.SetConfig(vmCfg, persist.Config.KataAgentConfig)
		slog.Info("VMConfig loaded from sandbox", "sandbox", entry.Name(), "size", len(vmCfg))
		return true
	}
	return false
}
