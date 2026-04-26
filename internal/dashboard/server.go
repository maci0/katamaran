package dashboard

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/logging"
)

const (
	maxLogLines  = 1000
	maxPingLines = 500

	// HTTP server timeouts.
	httpReadTimeout  = 10 * time.Second
	httpWriteTimeout = 30 * time.Second
	httpIdleTimeout  = 60 * time.Second
	shutdownTimeout  = 5 * time.Second

	// maxBodySize is the maximum request body size (1 MB), used by
	// MaxBytesReader on form POSTs.
	maxBodySize = 1 << 20

	// maxHeaderBytes caps the total size of HTTP request headers. Smaller
	// than maxBodySize because legitimate requests do not need megabytes of
	// headers, and a large limit aids header-bomb DoS.
	maxHeaderBytes = 64 * 1024

	// Scanner buffer sizes for subprocess output reading.
	scannerInitBuf = 64 * 1024   // Initial buffer allocation.
	scannerMaxSize = 1024 * 1024 // Maximum line size.
	maxLogLineSize = 8 * 1024    // Maximum stored dashboard log line size.

	// Load generator intervals.
	httpLoadInterval  = 200 * time.Millisecond
	httpClientTimeout = 2 * time.Second

	// maxResponseDiscard is the maximum response body bytes to consume
	// and discard from HTTP load generator responses (for connection reuse).
	maxResponseDiscard = 1 << 20
)

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `katamaran-dashboard — Web dashboard for Kata Containers live migration

Usage:
  katamaran-dashboard [flags]
  katamaran-dashboard --version
  katamaran-dashboard --help

Flags:
  --addr string          HTTP listen address (default ":8080")
  --enable-debug         Enable /debug/pprof/ and /debug/vars endpoints
  --log-format string    Log output format: 'text' or 'json' (default "text")
  --log-level string     Log level: 'debug', 'info', 'warn', or 'error' (default "info")

Other:
  -v, --version          Show version and exit
  -h, --help             Show this help and exit

Examples:
  # Start on default port
  katamaran-dashboard

  # Custom address and text logging
  katamaran-dashboard --addr 0.0.0.0:9090 --log-format text
`)
}

// Run contains all CLI logic: flag parsing, validation, and server startup.
// Extracted from main() so the CLI validation paths can be tested without os.Exit.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("katamaran-dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)

	addr := fs.String("addr", ":8080", "HTTP listen address")
	enableDebug := fs.Bool("enable-debug", false, "Enable /debug/pprof/ and /debug/vars endpoints")
	logLevel := fs.String("log-level", "info", "Log level: 'debug', 'info', 'warn', or 'error'")
	logFormat := fs.String("log-format", "text", "Log output format: 'text' or 'json'")
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

	if err := logging.SetupLogger(stderr, *logFormat, *logLevel, "dashboard"); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n\n", err)
		printUsage(stderr)
		return 2
	}

	allowedImage := os.Getenv("KATAMARAN_MIGRATION_IMAGE")
	if allowedImage != "" && !validFormValue(allowedImage) {
		fmt.Fprintf(stderr, "Error: KATAMARAN_MIGRATION_IMAGE contains invalid characters\n")
		return 1
	}

	app := &App{startTime: time.Now(), allowedImage: allowedImage}

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
		MaxHeaderBytes: maxHeaderBytes,
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
	mux.HandleFunc("/api", handleAPIFallback)
	mux.HandleFunc("/api/", handleAPIFallback)
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

var apiAllowedMethods = map[string]string{
	"/api/migrate":      http.MethodPost,
	"/api/migrate/stop": http.MethodPost,
	"/api/status":       http.MethodGet + ", " + http.MethodHead,
	"/api/ping":         http.MethodPost,
	"/api/ping/stop":    http.MethodPost,
	"/api/httpgen":      http.MethodPost,
	"/api/httpgen/stop": http.MethodPost,
}

func handleAPIFallback(w http.ResponseWriter, r *http.Request) {
	if allow, ok := apiAllowedMethods[r.URL.Path]; ok {
		w.Header().Set("Allow", allow)
		jsonError(w, fmt.Sprintf("Method %s not allowed", r.Method), http.StatusMethodNotAllowed)
		return
	}
	jsonError(w, "Not found", http.StatusNotFound)
}

// handleHealthz is a lightweight health check endpoint for Kubernetes probes.
// Unlike /api/status, it avoids mutex acquisition and JSON serialization.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
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
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if _, err := migrateScriptPath(); err != nil {
		dashboardReadinessFailuresTotal.Add(1)
		slog.Warn("Readiness check failed: migrate.sh not found", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("migrate.sh not found\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// getCounter returns the current value of the specified migration counter.
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

// serveHome serves the dashboard's index.html file.
func (a *App) serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

// handleStatus returns the current state of the dashboard, including active migrations and loadgen logs.
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
