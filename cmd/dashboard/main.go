package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxLogLines  = 1000
	maxPingLines = 500
)

type PingData struct {
	Time    string  `json:"time"`
	Latency float64 `json:"latency"`
	Error   string  `json:"error,omitempty"`
}

type StatusResponse struct {
	Migrating      bool       `json:"migrating"`
	LoadgenRunning bool       `json:"loadgen_running"`
	Logs           []string   `json:"logs"`
	Pings          []PingData `json:"pings"`
}

type App struct {
	migrationOutput []string
	migrationMutex  sync.Mutex
	isMigrating     bool
	migrationCancel context.CancelFunc

	pingLog        []PingData
	loadgenMutex   sync.Mutex
	loadgenRunning bool
	loadgenCancel  context.CancelFunc
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	app := &App{}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/", app.serveHome)
	mux.HandleFunc("/api/migrate", app.handleMigrate)
	mux.HandleFunc("/api/migrate/stop", app.handleMigrateStop)
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/ping", app.handlePingStart)
	mux.HandleFunc("/api/ping/stop", app.handleLoadgenStop)
	mux.HandleFunc("/api/httpgen", app.handleHTTPStart)
	mux.HandleFunc("/api/httpgen/stop", app.handleLoadgenStop)

	srv := &http.Server{
		Addr:           ":8080",
		Handler:        recoverMiddleware(requestLogger(securityHeaders(mux))),
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		slog.Info("Shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	slog.Info("Katamaran Dashboard listening", "addr", ":8080", "pid", os.Getpid())
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("HTTP server error", "error", err)
		os.Exit(1)
	}
}

// handleHealthz is a lightweight health check endpoint for Kubernetes probes.
// Unlike /api/status, it avoids mutex acquisition and JSON serialization.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// requestLogger wraps an http.Handler to log each request at completion.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		// Skip logging health checks to avoid noise.
		if r.URL.Path == "/healthz" {
			return
		}
		slog.Info("HTTP request", "method", r.Method, "path", r.URL.Path, "status", rw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.status = code
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

// recoverMiddleware catches panics in HTTP handlers, logs them, and returns 500.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("HTTP handler panic", "method", r.Method, "path", r.URL.Path, "panic", rec)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders wraps an http.Handler to set standard security headers
// on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com https://cdn.jsdelivr.net; "+
				"style-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"object-src 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func migrateScriptPath() string {
	paths := []string{
		"deploy/migrate.sh",
		"/usr/local/bin/migrate.sh",
		"./migrate.sh",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	slog.Warn("migrate.sh not found in expected locations, falling back to PATH resolution", "checked", paths)
	return "migrate.sh"
}

// validTarget checks that the target is a plausible IP or hostname for
// ping/HTTP probing. Rejects loopback, link-local, cloud metadata
// addresses, and unresolvable hostnames to prevent SSRF.
//
// Limitation: DNS rebinding could bypass this check — the hostname may
// resolve to a safe IP here but rebind to an internal IP at connect time.
// Accepted risk: this dashboard is a cluster-internal monitoring tool,
// not a public-facing API.
func validTarget(target string) bool {
	if strings.HasPrefix(target, "-") {
		return false
	}
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	if host == "" {
		return false
	}
	// Reject shell metacharacters that could escape into arguments.
	if strings.ContainsAny(host, ";|&$`\\\"'<>(){}!\n\r\t @#%") {
		return false
	}
	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		// Fail closed: reject unresolvable hostnames to prevent SSRF bypass
		// via names that the Go resolver cannot resolve but the target process
		// (ping, HTTP client) might resolve differently.
		return false
	}
	if ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() {
		return false
	}
	// Block cloud metadata endpoint (169.254.169.254).
	if ip.IP.Equal(net.ParseIP("169.254.169.254")) {
		return false
	}
	return true
}

// formValueRe is the allowlist for form values passed to migrate.sh.
// Aligned with migrate.sh's shell_safe_re to ensure defence-in-depth
// rejects the same characters at both layers.
var formValueRe = regexp.MustCompile(`^[a-zA-Z0-9_./:@=\-]+$`)

// validFormValue checks that a form value contains only shell-safe characters.
// Uses a whitelist aligned with migrate.sh's shell_safe_re regex, rejecting
// any characters that could be misinterpreted by envsubst or /bin/sh -c.
func validFormValue(v string) bool {
	return formValueRe.MatchString(v)
}

func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB max form body
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Validate all form values against shell metacharacters.
	formKeys := []string{"source_node", "dest_node", "qmp_source", "qmp_dest", "tap", "tap_netns", "dest_ip", "vm_ip", "image"}
	for _, key := range formKeys {
		if v := r.FormValue(key); v != "" && !validFormValue(v) {
			http.Error(w, fmt.Sprintf("Invalid value for %s", key), http.StatusBadRequest)
			return
		}
	}

	a.migrationMutex.Lock()
	if a.isMigrating {
		a.migrationMutex.Unlock()
		http.Error(w, "Migration already running", http.StatusConflict)
		return
	}
	a.isMigrating = true
	a.migrationOutput = nil
	// Use context.Background() so the migration process survives after
	// the HTTP response is sent (r.Context() cancels on response write).
	ctx, cancel := context.WithCancel(context.Background())
	a.migrationCancel = cancel
	a.migrationMutex.Unlock()

	args := []string{
		migrateScriptPath(),
		"--source-node", r.FormValue("source_node"),
		"--dest-node", r.FormValue("dest_node"),
		"--qmp-source", r.FormValue("qmp_source"),
		"--qmp-dest", r.FormValue("qmp_dest"),
		"--tap", r.FormValue("tap"),
		"--dest-ip", r.FormValue("dest_ip"),
		"--vm-ip", r.FormValue("vm_ip"),
		"--image", r.FormValue("image"),
	}

	if v := r.FormValue("tap_netns"); v != "" {
		args = append(args, "--tap-netns", v)
	}

	if r.FormValue("shared_storage") == "true" {
		args = append(args, "--shared-storage")
	}
	if dt := r.FormValue("downtime"); dt != "" {
		d, err := strconv.Atoi(dt)
		if err != nil || d <= 0 {
			a.migrationMutex.Lock()
			a.isMigrating = false
			a.migrationCancel = nil
			a.migrationMutex.Unlock()
			cancel()
			http.Error(w, "Invalid downtime value (must be a positive integer)", http.StatusBadRequest)
			return
		}
		args = append(args, "--downtime", strconv.Itoa(d))
	}

	go a.runCommand(ctx, args)

	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte("Migration started")); err != nil {
		slog.Warn("Failed to write response", "error", err)
	}
}

func (a *App) handleMigrateStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.migrationMutex.Lock()
	if a.migrationCancel != nil {
		a.migrationCancel()
	}
	a.migrationMutex.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (a *App) runCommand(ctx context.Context, args []string) {
	defer func() {
		a.migrationMutex.Lock()
		a.isMigrating = false
		a.migrationCancel = nil
		a.migrationMutex.Unlock()
	}()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.appendLog("Error: " + err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		a.appendLog("Error starting: " + err.Error())
		return
	}

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		a.appendLog(scanner.Text())
	}

	if err := cmd.Wait(); err != nil {
		a.appendLog("Finished with error: " + err.Error())
	} else {
		a.appendLog("Finished successfully.")
	}
}

func (a *App) appendLog(msg string) {
	a.migrationMutex.Lock()
	defer a.migrationMutex.Unlock()
	a.migrationOutput = append(a.migrationOutput, msg)
	if len(a.migrationOutput) > maxLogLines {
		n := len(a.migrationOutput) - maxLogLines
		clear(a.migrationOutput[:n]) // Release old strings for GC.
		a.migrationOutput = a.migrationOutput[n:]
	}
}

func (a *App) handleLoadgenStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.stopLoadgen()
	w.WriteHeader(http.StatusOK)
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.migrationMutex.Lock()
	logs := make([]string, len(a.migrationOutput))
	copy(logs, a.migrationOutput)
	status := a.isMigrating
	a.migrationMutex.Unlock()

	a.loadgenMutex.Lock()
	pings := make([]PingData, len(a.pingLog))
	copy(pings, a.pingLog)
	loadgenRunning := a.loadgenRunning
	a.loadgenMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(StatusResponse{
		Migrating:      status,
		LoadgenRunning: loadgenRunning,
		Logs:           logs,
		Pings:          pings,
	}); err != nil {
		slog.Error("Failed to encode status JSON", "error", err)
	}
}

var pingRe = regexp.MustCompile(`time=([0-9.]+) ms`)

func (a *App) stopLoadgen() {
	a.loadgenMutex.Lock()
	defer a.loadgenMutex.Unlock()
	if a.loadgenCancel != nil {
		a.loadgenCancel()
	}
}

func (a *App) handlePingStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "Target required", http.StatusBadRequest)
		return
	}
	if !validTarget(target) {
		http.Error(w, "Invalid target", http.StatusBadRequest)
		return
	}

	a.loadgenMutex.Lock()
	if a.loadgenRunning {
		a.loadgenMutex.Unlock()
		http.Error(w, "Load generator already running", http.StatusConflict)
		return
	}
	a.loadgenRunning = true
	a.pingLog = a.pingLog[:0]
	// Use context.Background() so the ping process survives after
	// the HTTP response is sent.
	ctx, cancel := context.WithCancel(context.Background())
	a.loadgenCancel = cancel
	a.loadgenMutex.Unlock()

	go func() {
		defer func() {
			a.loadgenMutex.Lock()
			a.loadgenRunning = false
			a.loadgenCancel = nil
			a.loadgenMutex.Unlock()
		}()

		cmd := exec.CommandContext(ctx, "ping", "-i", "0.2", target)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error("Failed to create ping stdout pipe", "target", target, "error", err)
			return
		}
		if err := cmd.Start(); err != nil {
			slog.Error("Failed to start ping process", "target", target, "error", err)
			return
		}

		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			matches := pingRe.FindStringSubmatch(line)
			if len(matches) > 1 {
				lat, _ := strconv.ParseFloat(matches[1], 64)
				a.addPing(lat, "")
			} else if strings.Contains(line, "Unreachable") || strings.Contains(line, "timeout") {
				a.addPing(0, "Timeout/Unreachable")
			}
		}
		if err := cmd.Wait(); err != nil {
			slog.Warn("Ping command finished with error", "target", target, "error", err)
		}
	}()

	w.WriteHeader(http.StatusAccepted)
}

func (a *App) addPing(lat float64, errStr string) {
	a.loadgenMutex.Lock()
	defer a.loadgenMutex.Unlock()
	a.pingLog = append(a.pingLog, PingData{
		Time:    time.Now().Format("15:04:05.000"),
		Latency: lat,
		Error:   errStr,
	})
	if len(a.pingLog) > maxPingLines {
		n := len(a.pingLog) - maxPingLines
		clear(a.pingLog[:n]) // Release old entries for GC.
		a.pingLog = a.pingLog[n:]
	}
}

func (a *App) handleHTTPStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "Target required", http.StatusBadRequest)
		return
	}
	if !validTarget(target) {
		http.Error(w, "Invalid target", http.StatusBadRequest)
		return
	}

	a.loadgenMutex.Lock()
	if a.loadgenRunning {
		a.loadgenMutex.Unlock()
		http.Error(w, "Load generator already running", http.StatusConflict)
		return
	}
	a.loadgenRunning = true
	a.pingLog = a.pingLog[:0]
	// Use context.Background() so the HTTP load generator survives after
	// the HTTP response is sent.
	ctx, cancel := context.WithCancel(context.Background())
	a.loadgenCancel = cancel
	a.loadgenMutex.Unlock()

	go func() {
		defer func() {
			a.loadgenMutex.Lock()
			a.loadgenRunning = false
			a.loadgenCancel = nil
			a.loadgenMutex.Unlock()
		}()

		client := &http.Client{Timeout: 2 * time.Second}
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()

		targetURL := "http://" + target
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
			if err != nil {
				a.addPing(0, "HTTP Error")
				return
			}
			resp, err := client.Do(req)
			lat := float64(time.Since(start).Milliseconds())

			if err != nil {
				if ctx.Err() != nil {
					return
				}
				a.addPing(0, "HTTP Error")
			} else {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				a.addPing(lat, "")
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	w.WriteHeader(http.StatusAccepted)
}
