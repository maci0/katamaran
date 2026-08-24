package factory

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/maci0/katamaran/internal/migration"
)

// LoadVMConfigFromNode populates the VMConfig the Config RPC returns by
// reading any existing Kata sandbox persist.json under sbsDir (the node-wide
// Kata sbs root). If no sandbox is present yet, it starts a background
// poller that retries until one appears or ctx is cancelled (shutdown).
func LoadVMConfigFromNode(ctx context.Context, srv *Server, sbsDir string) {
	if tryLoadFromSandbox(srv, sbsDir) {
		return
	}

	slog.Info("VMConfig not yet available; starting background poller", "sandbox_dir", sbsDir, "interval", "2s")
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if tryLoadFromSandboxSafe(srv, sbsDir) {
					return
				}
			}
		}
	}()
}

// tryLoadFromSandboxSafe wraps tryLoadFromSandbox with per-attempt panic
// recovery. A recover at goroutine scope would let a single panic kill the
// poller permanently, leaving VMConfig unavailable forever and every
// subsequent adoption falling back to cold start with no further signal.
// Same containment approach as Watcher.safeScan.
func tryLoadFromSandboxSafe(srv *Server, sbsDir string) (loaded bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("VMConfig poller panic", "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	return tryLoadFromSandbox(srv, sbsDir)
}

func tryLoadFromSandbox(srv *Server, sbsDir string) bool {
	persist := migration.FindSandboxPersist(sbsDir)
	if persist == nil {
		return false
	}

	vmCfg := migration.MarshalVMConfig(persist.Config.HypervisorType, persist.Config.HypervisorConfig, persist.Config.KataAgentConfig)
	srv.SetConfig(vmCfg, persist.Config.KataAgentConfig)
	slog.Info("VMConfig loaded from sandbox", "path", persist.Path, "size", len(vmCfg))
	return true
}
