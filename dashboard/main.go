package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
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
	Migrating bool       `json:"migrating"`
	Logs      []string   `json:"logs"`
	Pings     []PingData `json:"pings"`
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
	app := &App{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.serveHome)
	mux.HandleFunc("/api/migrate", app.handleMigrate)
	mux.HandleFunc("/api/migrate/stop", app.handleMigrateStop)
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/ping", app.handlePingStart)
	mux.HandleFunc("/api/ping/stop", app.handlePingStop)
	mux.HandleFunc("/api/httpgen", app.handleHTTPStart)

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Println("Katamaran Dashboard listening on :8080")
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}
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
	return "migrate.sh"
}

// validTarget checks that the target is a plausible IP or hostname for
// ping/HTTP probing. Rejects loopback, link-local, and cloud metadata
// addresses to prevent SSRF against internal services.
//
// Limitation: DNS rebinding could bypass this check — the hostname may
// resolve to a safe IP here but rebind to an internal IP at connect time.
// Accepted risk: this dashboard is a cluster-internal monitoring tool,
// not a public-facing API. For ping targets, there is no way to hook
// DNS resolution of an external process.
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
		// Unresolvable hostname — allow it through; ping will fail gracefully.
		return true
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

// validFormValue checks that a form value contains no shell metacharacters
// or control characters that could be misinterpreted by migrate.sh.
func validFormValue(v string) bool {
	return !strings.ContainsAny(v, " ;|&$`\\\"'<>(){}!\n\r\t")
}

func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Validate all form values against shell metacharacters.
	formKeys := []string{"source_node", "dest_node", "qmp_source", "qmp_dest", "tap", "dest_ip", "vm_ip", "image"}
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

	if r.FormValue("shared_storage") == "true" {
		args = append(args, "--shared-storage")
	}
	if dt := r.FormValue("downtime"); dt != "" {
		var d int
		if _, err := fmt.Sscanf(dt, "%d", &d); err != nil || d <= 0 {
			a.migrationMutex.Lock()
			a.isMigrating = false
			a.migrationCancel = nil
			a.migrationMutex.Unlock()
			cancel()
			http.Error(w, "Invalid downtime value (must be a positive integer)", http.StatusBadRequest)
			return
		}
		args = append(args, "--downtime", dt)
	}

	go a.runCommand(ctx, args)

	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte("Migration started")); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}

func (a *App) handleMigrateStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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
		a.migrationOutput = a.migrationOutput[len(a.migrationOutput)-maxLogLines:]
	}
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.migrationMutex.Lock()
	logs := make([]string, len(a.migrationOutput))
	copy(logs, a.migrationOutput)
	status := a.isMigrating
	a.migrationMutex.Unlock()

	a.loadgenMutex.Lock()
	pings := make([]PingData, len(a.pingLog))
	copy(pings, a.pingLog)
	a.loadgenMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(StatusResponse{
		Migrating: status,
		Logs:      logs,
		Pings:     pings,
	}); err != nil {
		log.Printf("Failed to encode status JSON: %v", err)
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
		w.WriteHeader(http.StatusOK)
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
			return
		}
		if err := cmd.Start(); err != nil {
			return
		}

		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			matches := pingRe.FindStringSubmatch(line)
			if len(matches) > 1 {
				var lat float64
				fmt.Sscanf(matches[1], "%f", &lat)
				a.addPing(lat, "")
			} else if strings.Contains(line, "Unreachable") || strings.Contains(line, "timeout") {
				a.addPing(0, "Timeout/Unreachable")
			}
		}
		if err := cmd.Wait(); err != nil {
			log.Printf("Ping command finished with error: %v", err)
		}
	}()

	w.WriteHeader(http.StatusOK)
}

func (a *App) handlePingStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.stopLoadgen()
	w.WriteHeader(http.StatusOK)
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
		a.pingLog = a.pingLog[len(a.pingLog)-maxPingLines:]
	}
}

func (a *App) handleHTTPStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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
		w.WriteHeader(http.StatusOK)
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

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			start := time.Now()
			resp, err := client.Get("http://" + target)
			lat := float64(time.Since(start).Milliseconds())

			if err != nil {
				a.addPing(0, "HTTP Error")
			} else {
				if err := resp.Body.Close(); err != nil {
					log.Printf("Failed to close response body: %v", err)
				}
				a.addPing(lat, "")
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	w.WriteHeader(http.StatusOK)
}
