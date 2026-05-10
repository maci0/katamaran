package factory

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

const (
	// migrationMetaFile is the filename the destination katamaran process
	// writes after a successful incoming live migration.
	migrationMetaFile = "migration-meta.json"

	// defaultPollInterval is the directory scan interval. Polling is
	// simpler and sufficient for the expected event rate (one migration
	// every few seconds at most).
	defaultPollInterval = 2 * time.Second
)

// Watcher polls a directory tree for new migration-meta.json files
// and offers each discovered VM to the factory Server.
type Watcher struct {
	dir    string
	server *Server
	seen   map[string]struct{}
}

// NewWatcher returns a Watcher that scans dir for sandbox
// subdirectories containing migration-meta.json.
func NewWatcher(dir string, server *Server) *Watcher {
	return &Watcher{
		dir:    dir,
		server: server,
		seen:   make(map[string]struct{}),
	}
}

// Run polls the watch directory every 2 seconds until ctx is
// cancelled. It is intended to be called in its own goroutine.
func (w *Watcher) Run(ctx context.Context) {
	slog.Info("Watcher started", "dir", w.dir)
	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()

	// Do an immediate scan before the first tick.
	w.safeScan()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Watcher stopped")
			return
		case <-ticker.C:
			w.safeScan()
		}
	}
}

// safeScan invokes scan with panic recovery. A panic on a malformed entry
// would otherwise kill the watcher goroutine and the factory would go
// blind to all subsequent migrations with no log signal.
func (w *Watcher) safeScan() {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("Watcher scan panic", "dir", w.dir, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	w.scan()
}

// scan walks the watch directory looking for sandbox subdirectories
// that contain a migration-meta.json file we haven't processed yet.
func (w *Watcher) scan() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		// The directory might not exist yet (no VMs running) — that
		// case is expected and stays silent. Other errors (permissions,
		// I/O) mean we are blind to migrations and need to be visible
		// to operators.
		if !os.IsNotExist(err) {
			slog.Warn("Watcher scan failed", "dir", w.dir, "error", err)
		}
		return
	}

	current := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := filepath.Join(w.dir, e.Name(), migrationMetaFile)
		current[metaPath] = struct{}{}
		if _, ok := w.seen[metaPath]; ok {
			continue
		}

		data, err := os.ReadFile(metaPath)
		if err != nil {
			// File doesn't exist (yet) in this sandbox dir — expected.
			// Other errors (permissions, I/O) mean we are silently blind
			// to a real migration; surface them.
			if !os.IsNotExist(err) {
				slog.Warn("Watcher failed to read migration metadata", "path", metaPath, "error", err)
			}
			continue
		}

		var state MigrationState
		if err := json.Unmarshal(data, &state); err != nil {
			slog.Warn("Failed to parse migration metadata", "path", metaPath, "error", err)
			w.seen[metaPath] = struct{}{}
			continue
		}

		slog.Info("Discovered migration metadata", "path", metaPath, "id", state.ID)
		w.seen[metaPath] = struct{}{}
		if len(state.VMConfig) > 0 {
			w.server.SetConfig(state.VMConfig, state.AgentConfig)
			slog.Info("VMConfig set from migration metadata", "id", state.ID)
		}
		w.server.OfferVM(state)
	}
	for metaPath := range w.seen {
		if _, ok := current[metaPath]; !ok {
			delete(w.seen, metaPath)
		}
	}
}
