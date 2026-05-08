package factory

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	// migrationMetaFile is the filename the destination QEMU process
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
	w.scan()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Watcher stopped")
			return
		case <-ticker.C:
			w.scan()
		}
	}
}

// scan walks the watch directory looking for sandbox subdirectories
// that contain a migration-meta.json file we haven't processed yet.
func (w *Watcher) scan() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		// The directory might not exist yet (no VMs running).
		if !os.IsNotExist(err) {
			slog.Debug("Watcher scan failed", "dir", w.dir, "error", err)
		}
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := filepath.Join(w.dir, e.Name(), migrationMetaFile)
		if _, ok := w.seen[metaPath]; ok {
			continue
		}

		data, err := os.ReadFile(metaPath)
		if err != nil {
			// File doesn't exist (yet) in this sandbox dir — expected.
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
}
