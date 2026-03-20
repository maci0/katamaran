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
	t.Parallel()
	tests := []struct {
		target string
		want   bool
	}{
		{"10.0.0.1", true},
		{"localhost", false},       // loopback
		{"127.0.0.1", false},       // loopback
		{"169.254.169.254", false}, // cloud metadata
		{"-c1", false},             // flag injection
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

func TestValidTarget_UnresolvableHostname(t *testing.T) {
	t.Parallel()
	// Unresolvable hostnames must be rejected (fail closed) to prevent
	// SSRF bypass via names that resolve differently at connect time.
	// RFC 2606: .invalid TLD is guaranteed to never resolve.
	if validTarget("nonexistent-host.invalid") {
		t.Error("validTarget should reject unresolvable hostnames")
	}
}

func TestValidFormValue(t *testing.T) {
	t.Parallel()
	allowed := []string{"tap0", "10.0.0.1", "/run/vc/vm/sock", "katamaran:dev"}
	for _, v := range allowed {
		if !validFormValue(v) {
			t.Errorf("validFormValue(%q) = false, want true", v)
		}
	}
	rejected := []string{
		"tap0;ls",        // semicolon
		"val|cat",        // pipe
		"val&bg",         // ampersand
		"val$(cmd)",      // dollar
		"val`cmd`",       // backtick
		"val with space", // space
		"val\nnewline",   // newline
	}
	for _, v := range rejected {
		if validFormValue(v) {
			t.Errorf("validFormValue(%q) = true, want false", v)
		}
	}
}

func TestHandleMigrate_DowntimeInjection(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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

func TestHandleStatus_ReturnsJSON(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %v", w.Code)
	}

	var status StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}
	if status.Migrating {
		t.Fatal("expected migrating=false for fresh app")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
	}
}

func TestHandleMigrate_TapNetnsValidated(t *testing.T) {
	t.Parallel()
	app := &App{}
	form := url.Values{}
	form.Add("source_node", "node1")
	form.Add("dest_node", "node2")
	form.Add("tap_netns", "/proc/123/ns/net;evil")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for shell metachar in tap_netns, got %v", w.Code)
	}
}

func TestHandleStatus_IncludesLogsAndPings(t *testing.T) {
	t.Parallel()
	app := &App{}
	app.appendLog("test log")
	app.addPing(1.5, "")

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)

	var status StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}
	if len(status.Logs) != 1 || status.Logs[0] != "test log" {
		t.Fatalf("unexpected logs: %v", status.Logs)
	}
	if len(status.Pings) != 1 || status.Pings[0].Latency != 1.5 {
		t.Fatalf("unexpected pings: %v", status.Pings)
	}
}

func TestHandleStatus_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %v", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("expected Allow: GET, got %q", allow)
	}
}

func TestHandlePingStart_InvalidTarget(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/ping?target=-invalid", nil)
	w := httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid target, got %v", w.Code)
	}
}

func TestHandlePingStart_MissingTarget(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/ping", nil)
	w := httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing target, got %v", w.Code)
	}
}

func TestHandlePingStart_ValidTarget(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(app.stopLoadgen)
	req := httptest.NewRequest(http.MethodPost, "/api/ping?target=1.1.1.1", nil)
	w := httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}
}

func TestHandlePingStart_AlreadyRunning(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(app.stopLoadgen)
	// Start first loadgen.
	req := httptest.NewRequest(http.MethodPost, "/api/ping?target=1.1.1.1", nil)
	w := httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first start: expected 202, got %v", w.Code)
	}

	// Second start should be rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/api/ping?target=1.1.1.1", nil)
	w2 := httptest.NewRecorder()
	app.handlePingStart(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate start: expected 409, got %v", w2.Code)
	}
}

func TestHandleHTTPStart_AlreadyRunning(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(app.stopLoadgen)
	// Start first loadgen.
	req := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=1.1.1.1", nil)
	w := httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first start: expected 202, got %v", w.Code)
	}

	// Second start should be rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=1.1.1.1", nil)
	w2 := httptest.NewRecorder()
	app.handleHTTPStart(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate start: expected 409, got %v", w2.Code)
	}
}

func TestHandleHTTPStart_InvalidTarget(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=-evil", nil)
	w := httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid target, got %v", w.Code)
	}
}

func TestHandleHTTPStart_MissingTarget(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/httpgen", nil)
	w := httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing target, got %v", w.Code)
	}
}

func TestHandleHTTPStart_ValidTarget(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(app.stopLoadgen)
	req := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=1.1.1.1", nil)
	w := httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}
}

func TestHandleMigrate_AcceptsValidRequest(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(func() {
		app.migrationMutex.Lock()
		if app.migrationCancel != nil {
			app.migrationCancel()
		}
		app.migrationMutex.Unlock()
	})
	form := url.Values{}
	form.Add("source_node", "node1")
	form.Add("dest_node", "node2")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}
}

func TestHandleMigrate_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/api/migrate", nil)
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %v", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("expected Allow: POST, got %q", allow)
	}
}

func TestHandleMigrate_RejectsShellMetachars(t *testing.T) {
	t.Parallel()
	app := &App{}
	form := url.Values{}
	form.Add("source_node", "node1;evil")
	form.Add("dest_node", "node2")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for shell metachar in form value, got %v", w.Code)
	}
}

func TestHandleMigrate_DuplicatePrevented(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(func() {
		app.migrationMutex.Lock()
		if app.migrationCancel != nil {
			app.migrationCancel()
		}
		app.migrationMutex.Unlock()
	})

	// Start first migration.
	form := url.Values{}
	form.Add("source_node", "node1")
	form.Add("dest_node", "node2")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first migration: expected 202, got %v", w.Code)
	}

	// Attempt second migration while first is running.
	req2 := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	app.handleMigrate(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate migration: expected 409, got %v", w2.Code)
	}
}

func TestHandleMigrateStop_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/api/migrate/stop", nil)
	w := httptest.NewRecorder()
	app.handleMigrateStop(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %v", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("expected Allow: POST, got %q", allow)
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := securityHeaders(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	checks := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
		"X-XSS-Protection":       "0",
	}
	for header, want := range checks {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("Content-Security-Policy header missing")
	}
}

func TestAppendLog_Overflow(t *testing.T) {
	t.Parallel()
	app := &App{}
	for i := 0; i < maxLogLines+100; i++ {
		app.appendLog("line")
	}
	app.migrationMutex.Lock()
	count := len(app.migrationOutput)
	app.migrationMutex.Unlock()
	if count != maxLogLines {
		t.Fatalf("expected log capped at %d, got %d", maxLogLines, count)
	}
}

func TestAddPing_Overflow(t *testing.T) {
	t.Parallel()
	app := &App{}
	for i := 0; i < maxPingLines+100; i++ {
		app.addPing(1.0, "")
	}
	app.loadgenMutex.Lock()
	count := len(app.pingLog)
	app.loadgenMutex.Unlock()
	if count != maxPingLines {
		t.Fatalf("expected pings capped at %d, got %d", maxPingLines, count)
	}
}

func TestHandleHealthz(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handleHealthz(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("expected Content-Type: text/plain, got %q", ct)
	}
	if body := w.Body.String(); body != "ok\n" {
		t.Fatalf("expected body %q, got %q", "ok\n", body)
	}
}

func TestHandleHealthz_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	w := httptest.NewRecorder()
	handleHealthz(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %v", w.Code)
	}
}

func TestRecoverMiddleware(t *testing.T) {
	t.Parallel()
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	handler := recoverMiddleware(panicking)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %v", w.Code)
	}
}

func TestRequestLogger(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	handler := requestLogger(inner)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %v", w.Code)
	}
}

func TestHandleLoadgenStop_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/api/ping/stop", nil)
	w := httptest.NewRecorder()
	app.handleLoadgenStop(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %v", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("expected Allow: POST, got %q", allow)
	}
}
