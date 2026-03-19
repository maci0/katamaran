package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestValidTarget(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"10.0.0.1", true},
		{"example.com", true},
		{"localhost", false},
		{"127.0.0.1", false},
		{"169.254.169.254", false},
		{"-c1", false},
		{"10.0.0.1; rm -rf /", false},
		{"evil.com@internal:8080", false},
		{"host#fragment", false},
		{"host%00encoded", false},
	}
	for _, tt := range tests {
		if got := validTarget(tt.target); got != tt.want {
			t.Errorf("validTarget(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
}

func TestValidFormValue(t *testing.T) {
	allowed := []string{"tap0", "10.0.0.1", "/run/vc/vm/sock", "katamaran:dev"}
	for _, v := range allowed {
		if !validFormValue(v) {
			t.Errorf("validFormValue(%q) = false, want true", v)
		}
	}
	rejected := []string{
		"tap0;ls",       // semicolon
		"val|cat",       // pipe
		"val&bg",        // ampersand
		"val$(cmd)",     // dollar
		"val`cmd`",      // backtick
		"val with space", // space
		"val\nnewline",  // newline
	}
	for _, v := range rejected {
		if validFormValue(v) {
			t.Errorf("validFormValue(%q) = true, want false", v)
		}
	}
}

func TestHandleMigrate_DowntimeInjection(t *testing.T) {
	// Regression test: downtime must be a strict integer to prevent
	// command injection via migrate.sh → envsubst → /bin/sh -c.
	payloads := []string{
		"25;rm -rf /",        // shell command separator
		"25|cat /etc/passwd", // pipe
		"25$(whoami)",        // command substitution
		"25`id`",             // backtick substitution
		"abc",                // non-numeric
		"25.5",               // float
		"0",                  // zero (must be positive)
		"-1",                 // negative
	}

	for _, payload := range payloads {
		t.Run("downtime="+payload, func(t *testing.T) {
			app := &App{}
			form := url.Values{}
			form.Add("source_node", "node1")
			form.Add("dest_node", "node2")
			form.Add("downtime", payload)
			req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			app.handleMigrate(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("downtime=%q: got status %d, want 400", payload, w.Code)
			}
			// Ensure no migration was started.
			app.migrationMutex.Lock()
			started := app.isMigrating
			app.migrationMutex.Unlock()
			if started {
				t.Errorf("downtime=%q: migration should not have started", payload)
			}
		})
	}
}

func TestApp_API(t *testing.T) {
	app := &App{}

	// Test handleStatus
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %v", w.Code)
	}

	var status StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Errorf("failed to unmarshal status: %v", err)
	}

	// Test appendLog and addPing
	app.appendLog("test log")
	app.addPing(1.5, "")

	w = httptest.NewRecorder()
	app.handleStatus(w, req)
	json.Unmarshal(w.Body.Bytes(), &status)
	if len(status.Logs) != 1 || status.Logs[0] != "test log" {
		t.Errorf("unexpected logs: %v", status.Logs)
	}
	if len(status.Pings) != 1 || status.Pings[0].Latency != 1.5 {
		t.Errorf("unexpected pings: %v", status.Pings)
	}

	// Test handlePingStart with invalid target
	req = httptest.NewRequest(http.MethodPost, "/api/ping?target=-invalid", nil)
	w = httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid target, got %v", w.Code)
	}

	// Test handlePingStart with valid target
	req = httptest.NewRequest(http.MethodPost, "/api/ping?target=1.1.1.1", nil)
	w = httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %v", w.Code)
	}
	app.handlePingStop(w, req)

	// Test handleHTTPStart with valid target
	req = httptest.NewRequest(http.MethodPost, "/api/httpgen?target=1.1.1.1", nil)
	w = httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %v", w.Code)
	}
	app.stopLoadgen()

	// Test handleMigrate
	form := url.Values{}
	form.Add("source_node", "node1")
	form.Add("dest_node", "node2")
	req = httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %v", w.Code)
	}

	app.handleMigrateStop(w, req)
}
