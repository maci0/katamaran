package dashboard

import (
	"context"
	"sync"
	"time"
)

type PingData struct {
	Time    string  `json:"time"`
	Latency float64 `json:"latency"`
	Error   string  `json:"error,omitempty"`
}

type StatusResponse struct {
	Version                 string     `json:"version"`
	UptimeSeconds           int64      `json:"uptime_seconds"`
	Migrating               bool       `json:"migrating"`
	MigrationID             string     `json:"migration_id,omitempty"`
	MigrationElapsedSeconds int64      `json:"migration_elapsed_seconds,omitempty"`
	LastMigrationResult     string     `json:"last_migration_result,omitempty"`
	LastMigrationError      string     `json:"last_migration_error,omitempty"`
	MigrationsStarted       int64      `json:"migrations_started"`
	MigrationsSucceeded     int64      `json:"migrations_succeeded"`
	MigrationsFailed        int64      `json:"migrations_failed"`
	LoadgenRunning          bool       `json:"loadgen_running"`
	LoadgenType             string     `json:"loadgen_type,omitempty"`
	Logs                    []string   `json:"logs"`
	Pings                   []PingData `json:"pings"`
}

type App struct {
	allowedImage string

	// orch is the orchestrator handleMigrate submits to. Set by the
	// production main() to NewNative() (or kubeconfig fallback). Tests
	// inject a fakeOrchestrator. handleMigrate fails 503 if nil.
	// readyz also returns 503 until orch is set.
	orch any // typed as orchestrator.Orchestrator at use site to avoid import cycle in this file

	startTime time.Time

	migrationOutput  []string
	migrationMutex   sync.Mutex
	isMigrating      bool
	logBufferWrapped bool // true once buffer wrapping has been logged for this migration
	migrationID      string
	migrationStart   time.Time // when the current migration began
	migrationCancel  context.CancelFunc

	lastMigrationResult string // "success", "error", or "" (no migration run yet)
	lastMigrationError  string // error message from the last failed migration

	// Lifetime counters for observability.
	migrationsStarted   int64
	migrationsSucceeded int64
	migrationsFailed    int64

	pingLog        []PingData
	loadgenMutex   sync.Mutex
	loadgenRunning bool
	loadgenType    string // "ping" or "http"; empty when not running
	loadgenCancel  context.CancelFunc
}

// PodInfo and NodeInfo are defined in the orchestrator package and re-exported
// from discovery.go via type aliases. Kept out of this file so the wire format
// has a single source of truth.
