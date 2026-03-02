package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
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

var (
	migrationOutput []string
	migrationMutex  sync.Mutex
	isMigrating     bool
	migrationCancel context.CancelFunc

	pingLog        []PingData
	loadgenMutex   sync.Mutex
	loadgenRunning bool
	loadgenCancel  context.CancelFunc
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

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", serveHome)
	mux.HandleFunc("/api/migrate", handleMigrate)
	mux.HandleFunc("/api/migrate/stop", handleMigrateStop)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/ping", handlePingStart)
	mux.HandleFunc("/api/ping/stop", handlePingStop)
	mux.HandleFunc("/api/httpgen", handleHTTPStart)

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

func serveHome(w http.ResponseWriter, r *http.Request) {
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

func handleMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	migrationMutex.Lock()
	if isMigrating {
		migrationMutex.Unlock()
		http.Error(w, "Migration already running", http.StatusConflict)
		return
	}
	isMigrating = true
	migrationOutput = migrationOutput[:0]
	ctx, cancel := context.WithCancel(r.Context())
	migrationCancel = cancel
	migrationMutex.Unlock()

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
			migrationMutex.Lock()
			isMigrating = false
			migrationCancel = nil
			migrationMutex.Unlock()
			http.Error(w, "Invalid downtime value (must be a positive integer)", http.StatusBadRequest)
			return
		}
		args = append(args, "--downtime", dt)
	}

	go runCommand(ctx, args)

	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte("Migration started")); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}

func handleMigrateStop(w http.ResponseWriter, r *http.Request) {
	migrationMutex.Lock()
	if migrationCancel != nil {
		migrationCancel()
	}
	migrationMutex.Unlock()
	w.WriteHeader(http.StatusOK)
}

func runCommand(ctx context.Context, args []string) {
	defer func() {
		migrationMutex.Lock()
		isMigrating = false
		migrationCancel = nil
		migrationMutex.Unlock()
	}()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		appendLog("Error: " + err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		appendLog("Error starting: " + err.Error())
		return
	}

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		appendLog(scanner.Text())
	}

	if err := cmd.Wait(); err != nil {
		appendLog("Finished with error: " + err.Error())
	} else {
		appendLog("Finished successfully.")
	}
}

func appendLog(msg string) {
	migrationMutex.Lock()
	defer migrationMutex.Unlock()
	migrationOutput = append(migrationOutput, msg)
	if len(migrationOutput) > maxLogLines {
		migrationOutput = migrationOutput[len(migrationOutput)-maxLogLines:]
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	migrationMutex.Lock()
	logs := make([]string, len(migrationOutput))
	copy(logs, migrationOutput)
	status := isMigrating
	migrationMutex.Unlock()

	loadgenMutex.Lock()
	pings := make([]PingData, len(pingLog))
	copy(pings, pingLog)
	loadgenMutex.Unlock()

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

func stopLoadgen() {
	loadgenMutex.Lock()
	defer loadgenMutex.Unlock()
	loadgenRunning = false
	if loadgenCancel != nil {
		loadgenCancel()
		loadgenCancel = nil
	}
}

func handlePingStart(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "Target required", http.StatusBadRequest)
		return
	}

	loadgenMutex.Lock()
	if loadgenRunning {
		loadgenMutex.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	loadgenRunning = true
	pingLog = pingLog[:0]
	ctx, cancel := context.WithCancel(r.Context())
	loadgenCancel = cancel
	loadgenMutex.Unlock()

	go func() {
		defer func() {
			loadgenMutex.Lock()
			loadgenRunning = false
			loadgenMutex.Unlock()
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
				addPing(lat, "")
			} else if strings.Contains(line, "Unreachable") || strings.Contains(line, "timeout") {
				addPing(0, "Timeout/Unreachable")
			}
		}
		if err := cmd.Wait(); err != nil {
			log.Printf("Ping command finished with error: %v", err)
		}
	}()

	w.WriteHeader(http.StatusOK)
}

func handlePingStop(w http.ResponseWriter, r *http.Request) {
	stopLoadgen()
	w.WriteHeader(http.StatusOK)
}

func addPing(lat float64, errStr string) {
	loadgenMutex.Lock()
	defer loadgenMutex.Unlock()
	pingLog = append(pingLog, PingData{
		Time:    time.Now().Format("15:04:05.000"),
		Latency: lat,
		Error:   errStr,
	})
	if len(pingLog) > maxPingLines {
		pingLog = pingLog[len(pingLog)-maxPingLines:]
	}
}

func handleHTTPStart(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "Target required", http.StatusBadRequest)
		return
	}

	loadgenMutex.Lock()
	if loadgenRunning {
		loadgenMutex.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	loadgenRunning = true
	pingLog = pingLog[:0]
	ctx, cancel := context.WithCancel(r.Context())
	loadgenCancel = cancel
	loadgenMutex.Unlock()

	go func() {
		defer func() {
			loadgenMutex.Lock()
			loadgenRunning = false
			loadgenMutex.Unlock()
		}()

		client := &http.Client{Timeout: 2 * time.Second}
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
				addPing(0, "HTTP Error")
			} else {
				if err := resp.Body.Close(); err != nil {
					log.Printf("Failed to close response body: %v", err)
				}
				addPing(lat, "")
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()

	w.WriteHeader(http.StatusOK)
}
