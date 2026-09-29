// Package dashboard implements the katamaran web UI HTTP server.
//
// It owns the transport layer, request validation, in-memory UI state,
// load-generation endpoints, and the wiring to the orchestrator package.
// Migration orchestration rules remain in internal/orchestrator so the
// dashboard, controller, and structured CLI share one application boundary.
package dashboard

import (
	"bytes"
	"context"
	"embed"
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

// assetsFS holds the vendored third-party JS bundles (Tailwind CSS runtime,
// Chart.js). Vendoring instead of loading them from a CDN removes unverified
// remote script execution from the dashboard origin and keeps the UI working
// on air-gapped or egress-restricted clusters.
//
// assets/ also holds ATTRIBUTION.md and SHA256SUMS, the provenance record for
// the bundles. newMux serves each file by explicit name, so neither manifest
// is reachable over HTTP.
//
//go:embed assets
var assetsFS embed.FS

const (
	maxLogLines  = 1000
	maxPingLines = 500

	// HTTP server timeouts.
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 10 * time.Second
	httpWriteTimeout      = 30 * time.Second
	httpIdleTimeout       = 60 * time.Second
	shutdownTimeout       = 30 * time.Second

	// maxBodySize is the maximum request body size (1 MB), used by
	// MaxBytesReader on form POSTs.
	maxBodySize = 1 << 20

	// maxHeaderBytes caps the total size of HTTP request headers. Smaller
	// than maxBodySize because legitimate requests do not need megabytes of
	// headers, and a tight cap mitigates header-bomb DoS.
	maxHeaderBytes = 64 * 1024

	// Scanner buffer sizes for subprocess output reading.
	scannerInitBuf = 64 * 1024   // Initial buffer allocation.
	scannerMaxSize = 1024 * 1024 // Maximum line size.
	maxLogLineSize = 8 * 1024    // Maximum stored dashboard log line size.

	// Load generator intervals.
	httpLoadInterval  = 200 * time.Millisecond
	pingInterval      = 200 * time.Millisecond
	httpClientTimeout = 2 * time.Second

	// maxResponseDiscard is the maximum response body bytes to consume
	// and discard from HTTP load generator responses (for connection reuse).
	maxResponseDiscard = 1 << 20
)

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `katamaran-dashboard: Web dashboard for Kata Containers live migration

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
  KATAMARAN_MIGRATION_IMAGE       Required trusted image for /api/migrate; all other images are rejected
  KATAMARAN_ALLOWED_NAMESPACES    Comma-separated namespaces the dashboard may read pods
                                  from and migrate (default: every namespace in the cluster)

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
	var common buildinfo.CommonFlags
	common.Register(fs)

	fs.Usage = func() { printUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if code, handled := common.Act(fs, "katamaran-dashboard", stdout, stderr, printUsage); handled {
		return code
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
		fmt.Fprintf(stderr, "Error: KATAMARAN_MIGRATION_IMAGE contains invalid characters\n\n")
		printUsage(stderr)
		return 2
	}
	if allowedImage == "" {
		fmt.Fprintln(stderr, "Error: KATAMARAN_MIGRATION_IMAGE is required; set it to a trusted migration image")
		return 2
	}

	namespaces, err := newNamespaceScope(os.Getenv("KATAMARAN_ALLOWED_NAMESPACES"))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n\n", err)
		printUsage(stderr)
		return 2
	}
	if len(namespaces.allowed) == 0 {
		slog.Warn("KATAMARAN_ALLOWED_NAMESPACES is unset: /api/pods and /api/migrate reach every namespace in the cluster")
	} else {
		slog.Info("Namespace allowlist active", "namespaces", namespaces.names())
	}

	app := &App{startTime: time.Now(), allowedImage: allowedImage, namespaces: namespaces}

	// The dashboard needs a Kubernetes connection: try in-cluster
	// service-account creds first, then a kubeconfig-loaded client (handy
	// when running on a developer laptop).
	if nat, err := orchestrator.New(""); err == nil {
		app.orch = nat
		if disc, derr := orchestrator.NewDiscoverer(""); derr == nil {
			app.discoverer = disc
		} else {
			slog.Warn("Discoverer unavailable: /api/pods and /api/nodes will return 503", "error", derr)
		}
		slog.Info("Migration: orchestrator using client-go")
	} else {
		// No Kubernetes API reachable. Keep the dashboard up so /healthz,
		// the static UI, and the loadgen endpoints still work; migration
		// + discovery handlers return 503 until app.orch is wired. Real
		// deployments hit the branch above; this is the fallback for unit
		// tests + a developer-laptop dry run.
		slog.Warn("Kubernetes API unreachable: migration handlers will return 503 until in-cluster config or KUBECONFIG is available", "error", err)
	}

	publishExpvars(app)

	mux := app.newMux(*enableDebug)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           requestLogger(recoverMiddleware(securityHeaders(csrfCheck(mux)))),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("HTTP listen error", "error", err)
		return 1
	}
	slog.Info("Katamaran Dashboard listening", "version", buildinfo.Version, "addr", *addr, "pid", os.Getpid())
	if err := app.serve(ctx, srv, listener); err != nil {
		slog.Error("HTTP server error", "error", err)
		return 1
	}
	return 0
}

func (a *App) serve(ctx context.Context, srv *http.Server, listener net.Listener) error {
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	select {
	case err := <-serveDone:
		return err
	case <-ctx.Done():
	}
	a.draining.Store(true)
	slog.Info("Shutting down", "addr", srv.Addr)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if err != nil {
		err = errors.Join(err, srv.Close())
	}
	serveErr := <-serveDone
	if !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, serveErr)
	}
	return err
}

// apiRoutes is the complete list of /api endpoints. newMux registers each
// entry and derives the 405 Allow values from the same patterns, so a new
// endpoint cannot be routed without also being advertised to clients that
// probe it with the wrong method.
func (a *App) apiRoutes() []apiRoute {
	return []apiRoute{
		{"POST /api/migrate", a.handleMigrate},
		{"POST /api/migrate/stop", a.handleMigrateStop},
		{"GET /api/pods", a.handleListPods},
		{"GET /api/nodes", a.handleListNodes},
		{"GET /api/status", a.handleStatus},
		{"GET /api/history", a.handleHistory},
		{"POST /api/ping", a.handlePingStart},
		{"POST /api/ping/stop", a.handleLoadgenStop},
		{"POST /api/httpgen", a.handleHTTPStart},
		{"POST /api/httpgen/stop", a.handleLoadgenStop},
	}
}

// apiRoute pairs a Go 1.22 method-aware ServeMux pattern with its handler.
type apiRoute struct {
	pattern string
	handler http.HandlerFunc
}

// apiAllowedMethods maps each registered /api path to the value of the Allow
// header returned with 405. A "GET" pattern also serves HEAD, so the Allow
// value advertises both, matching what net/http actually dispatches.
func (a *App) apiAllowedMethods() map[string]string {
	routes := a.apiRoutes()
	allowed := make(map[string]string, len(routes))
	for _, rt := range routes {
		method, path, found := strings.Cut(rt.pattern, " ")
		if !found {
			continue
		}
		allow := method
		if method == http.MethodGet {
			allow = method + ", " + http.MethodHead
		}
		allowed[path] = allow
	}
	return allowed
}

// newMux creates the HTTP route table. Extracted so tests can use the same
// routing as production without duplicating pattern strings.
func (a *App) newMux(enableDebug bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	mux.HandleFunc("GET /metrics", serveDashboardMetrics)
	mux.HandleFunc("GET /{$}", a.serveHome)
	mux.HandleFunc("GET /assets/tailwind-3.4.17.js", serveAsset("tailwind-3.4.17.js"))
	mux.HandleFunc("GET /assets/tailwind-3.4.17.LICENSE.txt", serveAsset("tailwind-3.4.17.LICENSE.txt"))
	mux.HandleFunc("GET /assets/chart-4.5.1.min.js", serveAsset("chart-4.5.1.min.js"))
	mux.HandleFunc("GET /assets/chart-4.5.1.LICENSE.txt", serveAsset("chart-4.5.1.LICENSE.txt"))
	fallback := handleAPIFallback(a.apiAllowedMethods())
	mux.HandleFunc("/api", fallback)
	mux.HandleFunc("/api/", fallback)
	for _, rt := range a.apiRoutes() {
		mux.HandleFunc(rt.pattern, rt.handler)
	}
	if enableDebug {
		// Runtime diagnostics: pprof (goroutine dumps, heap profiles, CPU profiles)
		// and expvar (version, goroutine count, memstats). Zero overhead until accessed.
		mux.Handle("/debug/pprof/", http.DefaultServeMux)
		mux.Handle("GET /debug/vars", expvar.Handler())
	}
	return mux
}

func handleAPIFallback(allowed map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if allow, ok := allowed[r.URL.Path]; ok {
			w.Header().Set("Allow", allow)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
				"error": fmt.Sprintf("Method %s not allowed", r.Method),
				"allow": allow,
			})
			return
		}
		jsonError(w, "Not found", http.StatusNotFound)
	}
}

// Static probe response bodies, hoisted so each Kubernetes probe doesn't
// allocate a fresh []byte conversion on the hot path.
var (
	probeOKBody       = []byte("ok\n")
	probeNotReadyBody = []byte("orchestrator not wired\n")
)

// handleHealthz is a lightweight health check endpoint for Kubernetes probes.
// Unlike /api/status, it avoids mutex acquisition and JSON serialization.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(probeOKBody)
}

// handleReadyz is a readiness probe for Kubernetes. Unlike the lightweight
// /healthz liveness check, it verifies the dashboard can actually serve
// migration requests by confirming the orchestrator is wired.
func (a *App) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	if a.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "shutting down\n")
		return
	}
	if a.orch != nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(probeOKBody)
		return
	}
	dashboardReadinessFailuresTotal.Add(1)
	slog.Debug("Readiness check failed: orchestrator not wired")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(probeNotReadyBody)
}

// counterSnapshot returns the current values of the lifetime migration
// counters under a single lock acquisition.
func (a *App) counterSnapshot() (started, succeeded, failed int64) {
	a.migrationMutex.Lock()
	defer a.migrationMutex.Unlock()
	return a.migrationsStarted, a.migrationsSucceeded, a.migrationsFailed
}

// serveHome serves the dashboard's embedded index.html. Embedding removes
// the CWD dependency that http.ServeFile would otherwise impose. The charset
// is set explicitly instead of left to mime.TypeByExtension, which drops it
// when the pod image has no /etc/mime.types entry for .html and leaves the
// browser to guess the encoding of a document full of non-ASCII UI strings.
func (a *App) serveHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", a.startTime, bytes.NewReader(indexHTML))
}

// serveAsset serves one vendored asset from the embedded assetsFS by name.
// Asset filenames carry their upstream version, so responses are immutable
// and cacheable forever; a bump means editing the filename in both places
// (index.html and newMux).
func serveAsset(name string) http.HandlerFunc {
	body, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		// The file is embedded at build time via go:embed, so a miss is a
		// programming error. Panic during mux construction (fail fast at
		// startup) rather than 500-ing every request later.
		panic("dashboard: missing embedded asset " + name + ": " + err.Error())
	}
	contentType := "text/javascript; charset=utf-8"
	license := ""
	if strings.HasSuffix(name, ".LICENSE.txt") {
		contentType = "text/plain; charset=utf-8"
	} else {
		license = strings.TrimSuffix(strings.TrimSuffix(name, ".js"), ".min") + ".LICENSE.txt"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if license != "" {
			w.Header().Set("Link", "</assets/"+license+">; rel=\"license\"")
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	}
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
	writeJSON(w, http.StatusOK, a.namespaces.filterPods(pods))
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
	// The list is serialized here, at the boundary: a nil slice would reach
	// the client as JSON null, which breaks callers that iterate the result.
	if nodes == nil {
		nodes = []orchestrator.NodeInfo{}
	}
	writeJSON(w, http.StatusOK, nodes)
}

// handleHistory returns completed migrations, newest first.
func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.historySnapshot())
}

// handleStatus returns the current dashboard state, including active
// migrations, loadgen samples, and incremental log/ping cursors.
func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rawLogs := q.Get("logs_after")
	rawPings := q.Get("pings_after")
	logsAfter, logsDelta := parseStatusCursor(rawLogs)
	pingsAfter, pingsDelta := parseStatusCursor(rawPings)
	// Surface malformed cursors so client bugs are diagnosable; behavior
	// stays unchanged (a bad cursor falls back to the full snapshot).
	if rawLogs != "" && !logsDelta {
		slog.Debug("Ignoring malformed logs_after cursor", "logs_after", rawLogs, "request_id", requestIDFromContext(r.Context()))
	}
	if rawPings != "" && !pingsDelta {
		slog.Debug("Ignoring malformed pings_after cursor", "pings_after", rawPings, "request_id", requestIDFromContext(r.Context()))
	}

	a.migrationMutex.Lock()
	logView, logsNext, logsReset := cursorWindow(a.migrationOutput, a.migrationLogSeq, logsAfter, logsDelta)
	logs := make([]string, len(logView))
	copy(logs, logView)
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
	hist := a.historySnapshot()

	var elapsedSeconds int64
	if status && !migrationStart.IsZero() {
		elapsedSeconds = int64(time.Since(migrationStart).Seconds())
	}

	a.loadgenMutex.Lock()
	pingView, pingsNext, pingsReset := cursorWindow(a.pingLog, a.pingSeq, pingsAfter, pingsDelta)
	pings := make([]PingData, len(pingView))
	copy(pings, pingView)
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
		History:                 hist,
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

// cursorWindow returns the portion of buf visible to a client resuming from
// sequence number after. seq is buf's head sequence number; delta reports
// whether the client sent a cursor at all. When the cursor is missing or
// outside [start..seq] the full buffer is returned and reset is true, telling
// the client to discard its view. Shared by the logs and pings windows in
// handleStatus so their index arithmetic cannot drift.
func cursorWindow[T any](buf []T, seq, after int64, delta bool) (view []T, next int64, reset bool) {
	start := seq - int64(len(buf))
	view = buf
	if delta {
		switch {
		case after < start || after > seq:
			reset = true
		case after > start:
			view = buf[after-start:]
		}
	}
	return view, seq, reset
}
