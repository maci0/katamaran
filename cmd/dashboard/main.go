package main

import (
	"context"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/logutil"
)

const (
	maxLogLines  = 1000
	maxPingLines = 500

	// HTTP server timeouts.
	httpReadTimeout  = 10 * time.Second
	httpWriteTimeout = 30 * time.Second
	httpIdleTimeout  = 60 * time.Second
	shutdownTimeout  = 5 * time.Second

	// maxBodySize is the maximum request body size (1 MB), used for both
	// MaxBytesReader on form POSTs and MaxHeaderBytes on the server.
	maxBodySize = 1 << 20

	// Scanner buffer sizes for subprocess output reading.
	scannerInitBuf = 64 * 1024   // Initial buffer allocation.
	scannerMaxSize = 1024 * 1024 // Maximum line size.

	// Load generator intervals.
	httpLoadInterval  = 200 * time.Millisecond
	httpClientTimeout = 2 * time.Second

	// maxResponseDiscard is the maximum response body bytes to consume
	// and discard from HTTP load generator responses (for connection reuse).
	maxResponseDiscard = 1 << 20
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

// writeJSON sends a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("Failed to encode JSON response", "error", err, "status", status)
	}
}

// jsonError sends a JSON error response: {"error": "..."}.
func jsonError(w http.ResponseWriter, msg string, status int) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type App struct {
	// migrateScript overrides automatic migrate.sh discovery when set.
	// Used in tests to inject a dummy script.
	migrateScript string

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

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `katamaran-dashboard — Web dashboard for Kata Containers live migration

Usage:
  katamaran-dashboard [flags]
  katamaran-dashboard --version
  katamaran-dashboard --help

Flags:
  --addr string          HTTP listen address (default ":8080")
  --enable-debug         Enable /debug/pprof/ and /debug/vars endpoints
  --log-format string    Log output format: 'text' or 'json' (default "json")
  --log-level string     Log level: 'debug', 'info', 'warn', or 'error' (default "info")

Other:
  --version, -v          Show version and exit
  --help, -h             Show this help and exit

Examples:
  # Start on default port
  katamaran-dashboard

  # Custom address and text logging
  katamaran-dashboard --addr 0.0.0.0:9090 --log-format text
`)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // A second signal will now force exit
	}()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run contains all CLI logic: flag parsing, validation, and server startup.
// Extracted from main() so the CLI validation paths can be tested without os.Exit.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("katamaran-dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)

	addr := fs.String("addr", ":8080", "HTTP listen address (e.g. :8080, 0.0.0.0:9090)")
	enableDebug := fs.Bool("enable-debug", false, "Enable /debug/pprof/ and /debug/vars endpoints")
	logLevel := fs.String("log-level", "info", "Log level: 'debug', 'info', 'warn', or 'error'")
	logFormat := fs.String("log-format", "json", "Log output format: 'text' or 'json'")
	showVersion := fs.Bool("version", false, "Show version and exit")
	showVersionShort := fs.Bool("v", false, "")
	helpFlag := fs.Bool("help", false, "")
	helpFlagShort := fs.Bool("h", false, "")

	fs.Usage = func() { printUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *helpFlag || *helpFlagShort {
		printUsage(stdout)
		return 0
	}

	if *showVersion || *showVersionShort {
		fmt.Fprintf(stdout, "katamaran-dashboard %s\n", buildinfo.Version)
		return 0
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "Error: unexpected arguments: %s\n\n", strings.Join(fs.Args(), " "))
		printUsage(stderr)
		return 2
	}

	// Normalize enum flags for case-insensitive matching.
	*logFormat = strings.ToLower(*logFormat)
	*logLevel = strings.ToLower(*logLevel)

	if err := logutil.SetupLogger(stderr, *logFormat, *logLevel, "dashboard"); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	app := &App{startTime: time.Now()}

	expvar.NewString("version").Set(buildinfo.Version)
	expvar.Publish("migrations_started", expvar.Func(func() any { return app.getCounter("started") }))
	expvar.Publish("migrations_succeeded", expvar.Func(func() any { return app.getCounter("succeeded") }))
	expvar.Publish("migrations_failed", expvar.Func(func() any { return app.getCounter("failed") }))

	mux := app.newMux(*enableDebug)

	srv := &http.Server{
		Addr:           *addr,
		Handler:        requestLogger(recoverMiddleware(securityHeaders(csrfCheck(mux)))),
		ReadTimeout:    httpReadTimeout,
		WriteTimeout:   httpWriteTimeout,
		IdleTimeout:    httpIdleTimeout,
		MaxHeaderBytes: maxBodySize,
	}

	go func() {
		<-ctx.Done()
		slog.Info("Shutting down", "addr", *addr)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP server shutdown error", "error", err)
		} else {
			slog.Info("HTTP server stopped gracefully")
		}
	}()

	slog.Info("Katamaran Dashboard listening", "version", buildinfo.Version, "addr", *addr, "pid", os.Getpid())
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("HTTP server error", "error", err)
		return 1
	}
	return 0
}

// newMux creates the HTTP route table. Extracted so tests can use the same
// routing as production without duplicating pattern strings.
func (a *App) newMux(enableDebug bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	mux.HandleFunc("GET /{$}", a.serveHome)
	mux.HandleFunc("POST /api/migrate", a.handleMigrate)
	mux.HandleFunc("POST /api/migrate/stop", a.handleMigrateStop)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("POST /api/ping", a.handlePingStart)
	mux.HandleFunc("POST /api/ping/stop", a.handleLoadgenStop)
	mux.HandleFunc("POST /api/httpgen", a.handleHTTPStart)
	mux.HandleFunc("POST /api/httpgen/stop", a.handleLoadgenStop)
	if enableDebug {
		// Runtime diagnostics: pprof (goroutine dumps, heap profiles, CPU profiles)
		// and expvar (version, goroutine count, memstats). Zero overhead until accessed.
		mux.Handle("/debug/pprof/", http.DefaultServeMux)
		mux.Handle("GET /debug/vars", expvar.Handler())
	}
	return mux
}

// handleHealthz is a lightweight health check endpoint for Kubernetes probes.
// Unlike /api/status, it avoids mutex acquisition and JSON serialization.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// handleReadyz is a readiness probe for Kubernetes. Unlike the lightweight
// /healthz liveness check, it verifies the dashboard can actually serve
// migration requests by checking that the migrate.sh script is findable.
func (a *App) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	if a.migrateScript != "" {
		// Test override — always ready.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
		return
	}
	if _, err := migrateScriptPath(); err != nil {
		slog.Warn("Readiness check failed: migrate.sh not found", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("migrate.sh not found\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

func (a *App) getCounter(name string) int64 {
	a.migrationMutex.Lock()
	defer a.migrationMutex.Unlock()
	switch name {
	case "started":
		return a.migrationsStarted
	case "succeeded":
		return a.migrationsSucceeded
	case "failed":
		return a.migrationsFailed
	}
	return 0
}

func (a *App) serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.migrationMutex.Lock()
	logs := make([]string, len(a.migrationOutput))
	copy(logs, a.migrationOutput)
	status := a.isMigrating
	migrationID := a.migrationID
	migrationStart := a.migrationStart
	lastResult := a.lastMigrationResult
	lastError := a.lastMigrationError
	started := a.migrationsStarted
	succeeded := a.migrationsSucceeded
	failed := a.migrationsFailed
	a.migrationMutex.Unlock()

	var elapsedSeconds int64
	if status && !migrationStart.IsZero() {
		elapsedSeconds = int64(time.Since(migrationStart).Seconds())
	}

	a.loadgenMutex.Lock()
	pings := make([]PingData, len(a.pingLog))
	copy(pings, a.pingLog)
	loadgenRunning := a.loadgenRunning
	loadgenType := a.loadgenType
	a.loadgenMutex.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, StatusResponse{
		Version:                 buildinfo.Version,
		UptimeSeconds:           int64(time.Since(a.startTime).Seconds()),
		Migrating:               status,
		MigrationID:             migrationID,
		MigrationElapsedSeconds: elapsedSeconds,
		LastMigrationResult:     lastResult,
		LastMigrationError:      lastError,
		MigrationsStarted:       started,
		MigrationsSucceeded:     succeeded,
		MigrationsFailed:        failed,
		LoadgenRunning:          loadgenRunning,
		LoadgenType:             loadgenType,
		Logs:                    logs,
		Pings:                   pings,
	})
}
