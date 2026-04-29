package dashboard

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/logging"
	"github.com/maci0/katamaran/internal/orchestrator"
)

//go:embed index.html
var indexHTML []byte

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

Exit codes:
  0   Clean shutdown (signal received)
  1   Runtime error (port already in use, Kubernetes unreachable)
  2   Argument or configuration error

Environment variables:
  KATAMARAN_MIGRATION_IMAGE   Allowlist image for /api/migrate; unset means any image is accepted

Examples:
  # Start on default port
  katamaran-dashboard

  # Custom address and text logging
  katamaran-dashboard --addr 0.0.0.0:9090 --log-format text
`)
}

func validListenAddr(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return false
	}
	_, err = net.LookupPort("tcp", port)
	return err == nil
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
	if !validListenAddr(*addr) {
		fmt.Fprintf(stderr, "Error: invalid --addr %q (expected host:port, for example :8080 or 0.0.0.0:8080)\n\n", *addr)
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
	if allowedImage == "" {
		// /api/migrate has no built-in authentication; without an image
		// allowlist any caller able to reach it can launch arbitrary
		// privileged container images on cluster nodes via the rendered
		// source/dest Jobs. Warn loudly so operators set the allowlist
		// (or deploy the dashboard behind external auth).
		slog.Warn("KATAMARAN_MIGRATION_IMAGE is unset: any image submitted to /api/migrate will be accepted; set this env var to pin migrations to a single trusted image")
	}

	app := &App{startTime: time.Now(), allowedImage: allowedImage}

	// The dashboard requires a working Kubernetes connection: in-cluster
	// service-account creds first, then a kubeconfig-loaded client (handy
	// when running on a developer laptop). The Script (migrate.sh)
	// orchestrator is not used here; it is available standalone via the
	// bin/katamaran-orchestrator CLI.
	if nat, err := orchestrator.New(); err == nil {
		app.orch = nat
		if disc, err := orchestrator.NewDiscoverer(); err == nil {
			app.discoverer = disc
		}
		slog.Info("Migration: orchestrator using in-cluster client-go")
	} else if nat, err2 := orchestrator.NewFromKubeconfig("", ""); err2 == nil {
		app.orch = nat
		if disc, err := orchestrator.NewDiscovererFromKubeconfig("", ""); err == nil {
			app.discoverer = disc
		}
		slog.Info("Migration: orchestrator using kubeconfig", "in_cluster_err", err)
	} else {
		// No Kubernetes API reachable. Keep the dashboard up so /healthz,
		// the static UI, and the loadgen endpoints still work; migration
		// + discovery handlers return 503 until app.orch is wired. Real
		// deployments will hit one of the branches above; this is the
		// fallback for unit tests + a developer-laptop dry run.
		slog.Warn("Kubernetes API unreachable: migration handlers will return 503 until in-cluster config or KUBECONFIG is available", "in_cluster_err", err, "kubeconfig_err", err2)
	}

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
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	mux.HandleFunc("GET /api/pods", a.handleListPods)
	mux.HandleFunc("GET /api/nodes", a.handleListNodes)
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
	"/api/pods":         http.MethodGet + ", " + http.MethodHead,
	"/api/nodes":        http.MethodGet + ", " + http.MethodHead,
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
// migration requests by confirming the orchestrator is wired.
func (a *App) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	if a.orch != nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	dashboardReadinessFailuresTotal.Add(1)
	slog.Debug("Readiness check failed: orchestrator not wired")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("orchestrator not wired\n"))
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

// serveHome serves the dashboard's embedded index.html. Embedding removes
// the CWD dependency that http.ServeFile would otherwise impose.
func (a *App) serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeContent(w, r, "index.html", a.startTime, bytes.NewReader(indexHTML))
}

// handleListPods returns kata-runtime pods discovered from Kubernetes.
func (a *App) handleListPods(w http.ResponseWriter, r *http.Request) {
	disc := a.discoverer
	if disc == nil {
		slog.Warn("List pods failed: discoverer not configured", "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Discoverer not configured (no in-cluster config or KUBECONFIG)", http.StatusServiceUnavailable)
		return
	}
	pods, err := disc.ListKataPods(r.Context())
	if err != nil {
		slog.Warn("list kata pods failed", "error", err, "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Failed to list pods", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, pods)
}

// handleListNodes returns nodes labeled for the kata runtime.
func (a *App) handleListNodes(w http.ResponseWriter, r *http.Request) {
	disc := a.discoverer
	if disc == nil {
		slog.Warn("List nodes failed: discoverer not configured", "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Discoverer not configured (no in-cluster config or KUBECONFIG)", http.StatusServiceUnavailable)
		return
	}
	nodes, err := disc.ListKataNodes(r.Context())
	if err != nil {
		slog.Warn("list kata nodes failed", "error", err, "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Failed to list nodes", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

// handleStatus returns the current state of the dashboard, including active migrations and loadgen logs.
func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	logsAfter, logsDelta := parseStatusCursor(r.URL.Query().Get("logs_after"))
	pingsAfter, pingsDelta := parseStatusCursor(r.URL.Query().Get("pings_after"))

	a.migrationMutex.Lock()
	logStart := a.migrationLogSeq - int64(len(a.migrationOutput))
	logSrc := a.migrationOutput
	logsReset := false
	if logsDelta {
		switch {
		case logsAfter < logStart || logsAfter > a.migrationLogSeq:
			logsReset = true
		case logsAfter > logStart:
			logSrc = logSrc[logsAfter-logStart:]
		}
	}
	logs := make([]string, len(logSrc))
	copy(logs, logSrc)
	logsNext := a.migrationLogSeq
	status := a.isMigrating
	migrationID := a.migrationID
	migrationStart := a.migrationStart
	lastResult := a.lastMigrationResult
	lastError := a.lastMigrationError
	started := a.migrationsStarted
	succeeded := a.migrationsSucceeded
	failed := a.migrationsFailed
	var progress *MigrationProgress
	if a.latestProgress != nil {
		p := *a.latestProgress // copy under lock so caller mutation is safe
		progress = &p
	}
	a.migrationMutex.Unlock()

	var elapsedSeconds int64
	if status && !migrationStart.IsZero() {
		elapsedSeconds = int64(time.Since(migrationStart).Seconds())
	}

	a.loadgenMutex.Lock()
	pingStart := a.pingSeq - int64(len(a.pingLog))
	pingSrc := a.pingLog
	pingsReset := false
	if pingsDelta {
		switch {
		case pingsAfter < pingStart || pingsAfter > a.pingSeq:
			pingsReset = true
		case pingsAfter > pingStart:
			pingSrc = pingSrc[pingsAfter-pingStart:]
		}
	}
	pings := make([]PingData, len(pingSrc))
	copy(pings, pingSrc)
	pingsNext := a.pingSeq
	loadgenRunning := a.loadgenRunning
	loadgenType := a.loadgenType
	a.loadgenMutex.Unlock()

	writeJSON(w, http.StatusOK, StatusResponse{
		Version:                 buildinfo.Version,
		UptimeSeconds:           int64(time.Since(a.startTime).Seconds()),
		Migrating:               status,
		MigrationID:             migrationID,
		MigrationElapsedSeconds: elapsedSeconds,
		MigrationProgress:       progress,
		LastMigrationResult:     lastResult,
		LastMigrationError:      lastError,
		MigrationsStarted:       started,
		MigrationsSucceeded:     succeeded,
		MigrationsFailed:        failed,
		LoadgenRunning:          loadgenRunning,
		LoadgenType:             loadgenType,
		Logs:                    logs,
		LogsNext:                logsNext,
		LogsReset:               logsReset,
		Pings:                   pings,
		PingsNext:               pingsNext,
		PingsReset:              pingsReset,
	})
}

func parseStatusCursor(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
