package dashboard

import (
	"context"
	"sync"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

type PingData struct {
	Time    string  `json:"time"`
	Latency float64 `json:"latency"`
	Error   string  `json:"error,omitempty"`
}

type MigrationProgress struct {
	Phase          string `json:"phase"`
	RAMTransferred int64  `json:"ram_transferred"`
	RAMTotal       int64  `json:"ram_total"`
	DowntimeMS     int64  `json:"downtime_ms"`
}

type StatusResponse struct {
	Version                 string             `json:"version"`
	UptimeSeconds           int64              `json:"uptime_seconds"`
	Migrating               bool               `json:"migrating"`
	MigrationID             string             `json:"migration_id,omitempty"`
	MigrationElapsedSeconds int64              `json:"migration_elapsed_seconds,omitempty"`
	MigrationProgress       *MigrationProgress `json:"migration_progress,omitempty"`
	LastMigrationResult     string             `json:"last_migration_result,omitempty"`
	LastMigrationError      string             `json:"last_migration_error,omitempty"`
	MigrationsStarted       int64              `json:"migrations_started"`
	MigrationsSucceeded     int64              `json:"migrations_succeeded"`
	MigrationsFailed        int64              `json:"migrations_failed"`
	LoadgenRunning          bool               `json:"loadgen_running"`
	LoadgenType             string             `json:"loadgen_type,omitempty"`
	Logs                    []string           `json:"logs"`
	LogsNext                int64              `json:"logs_next"`
	LogsReset               bool               `json:"logs_reset"`
	Pings                   []PingData         `json:"pings"`
	PingsNext               int64              `json:"pings_next"`
	PingsReset              bool               `json:"pings_reset"`
}

type App struct {
	allowedImage string

	// orch is the orchestrator handleMigrate submits to. Set by the
	// production main() to New() (or kubeconfig fallback). Tests
	// inject a fakeOrchestrator. handleMigrate fails 503 if nil.
	// readyz also returns 503 until orch is set.
	orch orchestrator.Orchestrator

	// discoverer backs pod/node dropdowns and pod-mode request resolution.
	// Nil means pod-picker endpoints and pod-mode request resolution are unavailable.
	discoverer orchestrator.Discoverer

	startTime time.Time

	migrationOutput  []string
	migrationLogSeq  int64
	migrationMutex   sync.Mutex
	isMigrating      bool
	logBufferWrapped bool // true once buffer wrapping has been logged for this migration
	migrationID      string
	migrationStart   time.Time // when the current migration began
	migrationCancel  context.CancelFunc

	lastMigrationResult string // "success", "error", or "" (no migration run yet)
	lastMigrationError  string // error message from the last failed migration

	// latestProgress is the most recent StatusUpdate's structured progress
	// data, surfaced to /api/status so the UI can render a progress bar.
	// Reset when a new migration starts; persists after completion so the
	// final transferred / downtime values stay visible until next run.
	latestProgress *MigrationProgress

	// Lifetime counters for observability.
	migrationsStarted   int64
	migrationsSucceeded int64
	migrationsFailed    int64

	pingLog        []PingData
	pingSeq        int64
	loadgenMutex   sync.Mutex
	loadgenRunning bool
	loadgenType    string // "ping" or "http"; empty when not running
	loadgenCancel  context.CancelFunc
}
