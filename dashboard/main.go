package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	migrationOutput []string
	migrationMutex  sync.Mutex
	isMigrating     bool
	pingLog         []PingData
	pingMutex       sync.Mutex
	isPinging       bool
)

type PingData struct {
	Time    string  `json:"time"`
	Latency float64 `json:"latency"`
	Error   string  `json:"error,omitempty"`
}

func main() {
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/api/migrate", handleMigrate)
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/ping", handlePingStart)
	http.HandleFunc("/api/ping/stop", handlePingStop)
	http.HandleFunc("/api/httpgen", handleHttpStart)

	fmt.Println("Starting Katamaran Dashboard on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func handleMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()

	migrationMutex.Lock()
	if isMigrating {
		migrationMutex.Unlock()
		http.Error(w, "Migration already running", http.StatusConflict)
		return
	}
	isMigrating = true
	migrationOutput = []string{}
	migrationMutex.Unlock()

	args := []string{
		"../deploy/migrate.sh",
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

	go runCommand(args)

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("Migration started"))
}

func runCommand(args []string) {
	cmd := exec.Command(args[0], args[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		appendLog("Error creating stdout pipe: " + err.Error())
		setMigrating(false)
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		appendLog("Error starting command: " + err.Error())
		setMigrating(false)
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		appendLog(scanner.Text())
	}

	if err := cmd.Wait(); err != nil {
		appendLog("Command finished with error: " + err.Error())
	} else {
		appendLog("Command finished successfully.")
	}
	setMigrating(false)
}

func appendLog(msg string) {
	migrationMutex.Lock()
	defer migrationMutex.Unlock()
	migrationOutput = append(migrationOutput, msg)
	if len(migrationOutput) > 1000 {
		migrationOutput = migrationOutput[len(migrationOutput)-1000:]
	}
}

func setMigrating(status bool) {
	migrationMutex.Lock()
	defer migrationMutex.Unlock()
	isMigrating = status
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	migrationMutex.Lock()
	logs := make([]string, len(migrationOutput))
	copy(logs, migrationOutput)
	status := isMigrating
	migrationMutex.Unlock()

	pingMutex.Lock()
	pings := make([]PingData, len(pingLog))
	copy(pings, pingLog)
	pingMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"migrating": status,
		"logs":      logs,
		"pings":     pings,
	})
}

var pingRe = regexp.MustCompile(`time=([0-9\.]+) ms`)

func handlePingStart(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "Target required", http.StatusBadRequest)
		return
	}

	pingMutex.Lock()
	if isPinging {
		pingMutex.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	isPinging = true
	pingLog = []PingData{}
	pingMutex.Unlock()

	go func() {
		cmd := exec.Command("ping", "-i", "0.2", target)
		stdout, _ := cmd.StdoutPipe()
		cmd.Start()

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			pingMutex.Lock()
			if !isPinging {
				cmd.Process.Kill()
				pingMutex.Unlock()
				return
			}
			pingMutex.Unlock()

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
		cmd.Wait()

		pingMutex.Lock()
		isPinging = false
		pingMutex.Unlock()
	}()

	w.WriteHeader(http.StatusOK)
}

func handlePingStop(w http.ResponseWriter, r *http.Request) {
	pingMutex.Lock()
	isPinging = false
	pingMutex.Unlock()
	w.WriteHeader(http.StatusOK)
}

func addPing(lat float64, errStr string) {
	pingMutex.Lock()
	defer pingMutex.Unlock()
	pingLog = append(pingLog, PingData{
		Time:    time.Now().Format("15:04:05.000"),
		Latency: lat,
		Error:   errStr,
	})
	if len(pingLog) > 100 {
		pingLog = pingLog[len(pingLog)-100:]
	}
}

func handleHttpStart(w http.ResponseWriter, r *http.Request) {
	// Simple HTTP Loadgen (sends requests and logs latency)
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "Target required", http.StatusBadRequest)
		return
	}

	// Similar to ping, but doing http.Get
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		for i := 0; i < 50; i++ {
			pingMutex.Lock()
			if !isPinging {
				pingMutex.Unlock()
				break
			}
			pingMutex.Unlock()

			start := time.Now()
			resp, err := client.Get("http://" + target)
			lat := time.Since(start).Seconds() * 1000

			if err != nil {
				addPing(0, "HTTP Error")
			} else {
				resp.Body.Close()
				addPing(lat, "")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	w.WriteHeader(http.StatusOK)
}
