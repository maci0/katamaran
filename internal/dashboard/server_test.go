package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/buildinfo"
)

// dummyMigrateScript creates a no-op migrate.sh in a temp directory and
// returns its path. The script exits immediately with code 0.
func dummyMigrateScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "migrate.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// slowMigrateScript creates a migrate.sh that stays alive long enough for
// tests that need to observe an in-progress migration.
func slowMigrateScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "migrate.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 30\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// failingMigrateScript creates a migrate.sh that prints an error and exits 1.
func failingMigrateScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "migrate.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'QEMU connection refused'\necho 'migration failed: host unreachable'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// waitMigrationDone polls until the migration goroutine finishes.
func waitMigrationDone(t *testing.T, app *App, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		app.migrationMutex.Lock()
		done := !app.isMigrating
		app.migrationMutex.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("migration did not complete within timeout")
}

func TestRun_Help(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	// "Usage:" distinguishes the help banner from the version line, which also
	// contains "katamaran-dashboard".
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("expected usage output containing 'Usage:', got: %s", stdout.String())
	}
}

func TestRun_HelpShort(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("expected usage output containing 'Usage:', got: %s", stdout.String())
	}
}

func TestRun_Version(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), buildinfo.Version) {
		t.Fatalf("expected version %q in output, got: %s", buildinfo.Version, stdout.String())
	}
}

func TestRun_VersionShort(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"-v"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), buildinfo.Version) {
		t.Fatalf("expected version %q in output, got: %s", buildinfo.Version, stdout.String())
	}
}

func TestRun_UnexpectedArgs(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"foo", "bar"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Fatalf("expected unexpected args error, got: %s", stderr.String())
	}
}

func TestRun_UnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--nonexistent-flag"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "nonexistent-flag") {
		t.Fatalf("expected error mentioning unknown flag, got: %s", stderr.String())
	}
}

func TestRun_InvalidLogFormat(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--log-format", "yaml"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid log format") {
		t.Fatalf("expected log format error, got: %s", stderr.String())
	}
}

func TestRun_CaseInsensitiveLogFlags(t *testing.T) {
	// Not parallel: SetupLogger calls slog.SetDefault.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Pre-cancel so the server shuts down immediately.
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"--log-level", "INFO", "--log-format", "TEXT"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
}

// validMigrateForm returns url.Values with all required fields set to valid
// values. Tests can modify the returned form to inject specific bad values.
func validMigrateForm() url.Values {
	form := url.Values{}
	form.Set("source_node", "node1")
	form.Set("dest_node", "node2")
	form.Set("qmp_source", "/run/vc/vm/abc/qmp.sock")
	form.Set("qmp_dest", "/run/vc/vm/def/qmp.sock")
	form.Set("dest_ip", "10.0.0.2")
	form.Set("vm_ip", "10.244.1.5")
	form.Set("tap", "tap0_kata")
	form.Set("image", "katamaran:dev")
	return form
}

func TestValidTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target string
		want   bool
	}{
		{"10.0.0.1", true},
		{"10.0.0.1:8080", true},
		{"[2001:db8::1]", true},
		{"[2001:db8::1]:8080", true},
		{"localhost", false},       // loopback
		{"127.0.0.1", false},       // loopback
		{"169.254.169.254", false}, // cloud metadata
		{"-c1", false},             // flag injection
		{"10.0.0.1:0", false},      // invalid port
		{"10.0.0.1:65536", false},  // invalid port
		{"10.0.0.1:http", false},   // invalid port
		{"10.0.0.1; rm -rf /", false},
		{"evil.com@internal:8080", false},
		{"host#fragment", false},
		{"host%00encoded", false},
		{"10.0.0.1:8080/admin", false},               // path injection SSRF
		{strings.Repeat("a", maxTargetLen+1), false}, // exceeds length limit
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
		"tap0;ls",                              // semicolon
		"val|cat",                              // pipe
		"val&bg",                               // ampersand
		"val$(cmd)",                            // dollar
		"val`cmd`",                             // backtick
		"val with space",                       // space
		"val\nnewline",                         // newline
		strings.Repeat("a", maxFormValueLen+1), // exceeds length limit
	}
	for _, v := range rejected {
		if validFormValue(v) {
			t.Errorf("validFormValue(%q) = true, want false", v)
		}
	}
	// Value at exactly the limit should be accepted.
	if !validFormValue(strings.Repeat("a", maxFormValueLen)) {
		t.Error("validFormValue at maxFormValueLen should be accepted")
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
		"60001",              // exceeds upper bound
	}

	for _, payload := range payloads {
		t.Run("downtime="+payload, func(t *testing.T) {
			t.Parallel()
			app := &App{}
			form := validMigrateForm()
			form.Set("downtime", payload)
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
	form := validMigrateForm()
	form.Set("tap_netns", "/proc/123/ns/net;evil")
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

func TestMux_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	app := &App{}
	mux := app.newMux(false)

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/status"},
		{http.MethodGet, "/api/ping"},
		{http.MethodGet, "/api/httpgen"},
		{http.MethodGet, "/api/migrate"},
		{http.MethodGet, "/api/migrate/stop"},
		{http.MethodGet, "/api/ping/stop"},
		{http.MethodGet, "/api/httpgen/stop"},
		{http.MethodPost, "/healthz"},
		{http.MethodPost, "/"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("expected 405, got %v", w.Code)
			}
			if allow := w.Header().Get("Allow"); allow == "" {
				t.Fatal("expected Allow header to be set")
			}
			if strings.HasPrefix(tt.path, "/api/") {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Fatalf("expected JSON Content-Type for API 405, got %q", ct)
				}
				var resp map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal API 405 response: %v", err)
				}
				if resp["error"] == "" {
					t.Fatal("expected API 405 response to include error")
				}
			}
		})
	}
}

func TestMux_NotFound(t *testing.T) {
	t.Parallel()
	app := &App{}
	mux := app.newMux(false)

	for _, path := range []string{"/api/nonexistent", "/nonexistent"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %v", w.Code)
			}
			if strings.HasPrefix(path, "/api/") {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Fatalf("expected JSON Content-Type for API 404, got %q", ct)
				}
				var resp map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal API 404 response: %v", err)
				}
				if resp["error"] == "" {
					t.Fatal("expected API 404 response to include error")
				}
			}
		})
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
	req := httptest.NewRequest(http.MethodPost, "/api/ping?target=192.0.2.1", nil)
	w := httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	var pingResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &pingResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if pingResp["message"] != "Ping started" {
		t.Errorf("message = %q, want %q", pingResp["message"], "Ping started")
	}
}

func TestHandlePingStart_StripsPort(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(app.stopLoadgen)
	req := httptest.NewRequest(http.MethodPost, "/api/ping?target=192.0.2.1:8080", nil)
	w := httptest.NewRecorder()
	app.handlePingStart(w, req)
	// Should accept host:port and start successfully (port is stripped for ping).
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for target with port, got %v", w.Code)
	}
}

func TestHandlePingStart_AlreadyRunning(t *testing.T) {
	t.Parallel()
	app := &App{}
	t.Cleanup(app.stopLoadgen)
	// Start first loadgen.
	req := httptest.NewRequest(http.MethodPost, "/api/ping?target=192.0.2.1", nil)
	w := httptest.NewRecorder()
	app.handlePingStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first start: expected 202, got %v", w.Code)
	}

	// Second start should be rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/api/ping?target=192.0.2.1", nil)
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
	req := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=192.0.2.1", nil)
	w := httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first start: expected 202, got %v", w.Code)
	}

	// Second start should be rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=192.0.2.1", nil)
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

func TestHandleHTTPStart_InvalidPort(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=192.0.2.1:70000", nil)
	w := httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid target port, got %v", w.Code)
	}
}

func TestSafeDialContext_BlocksUnsafeAddress(t *testing.T) {
	t.Parallel()
	_, err := safeDialContext(context.Background(), "tcp", "127.0.0.1:80")
	if !errors.Is(err, errUnsafeTargetIP) {
		t.Fatalf("expected unsafe target error, got %v", err)
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
	req := httptest.NewRequest(http.MethodPost, "/api/httpgen?target=192.0.2.1", nil)
	w := httptest.NewRecorder()
	app.handleHTTPStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	var httpResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &httpResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if httpResp["message"] != "HTTP load generator started" {
		t.Errorf("message = %q, want %q", httpResp["message"], "HTTP load generator started")
	}
}

func TestHandleMigrate_AcceptsValidRequest(t *testing.T) {
	t.Parallel()
	app := &App{migrateScript: dummyMigrateScript(t)}
	t.Cleanup(func() {
		app.migrationMutex.Lock()
		if app.migrationCancel != nil {
			app.migrationCancel()
		}
		app.migrationMutex.Unlock()
	})
	form := validMigrateForm()
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}
}

func TestHandleMigrate_RejectsShellMetachars(t *testing.T) {
	t.Parallel()
	app := &App{}
	form := validMigrateForm()
	form.Set("source_node", "node1;evil")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for shell metachar in form value, got %v", w.Code)
	}
}

func TestHandleMigrate_RejectsDisallowedImage(t *testing.T) {
	t.Parallel()
	app := &App{allowedImage: "localhost/katamaran:dev"}
	form := validMigrateForm()
	form.Set("image", "evil.example/katamaran:dev")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for disallowed image, got %v", w.Code)
	}
	app.migrationMutex.Lock()
	started := app.isMigrating
	app.migrationMutex.Unlock()
	if started {
		t.Fatal("migration should not start with a disallowed image")
	}
}

func TestHandleMigrate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	app := &App{}
	form := url.Values{}
	form.Add("source_node", "node1")
	form.Add("dest_node", "node2")
	// Missing qmp_source, qmp_dest, dest_ip.
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing required field, got %v", w.Code)
	}
}

func TestHandleMigrate_DuplicatePrevented(t *testing.T) {
	t.Parallel()
	app := &App{migrateScript: slowMigrateScript(t)}
	t.Cleanup(func() {
		app.migrationMutex.Lock()
		if app.migrationCancel != nil {
			app.migrationCancel()
		}
		app.migrationMutex.Unlock()
	})

	// Start first migration.
	form := validMigrateForm()
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
	app.migrationMutex.Lock()
	cancel := app.migrationCancel
	app.migrationMutex.Unlock()
	if cancel != nil {
		cancel()
	}
	waitMigrationDone(t, app, 5*time.Second)
}

func TestHandleMigrateStop_ReturnsBody(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate/stop", nil)
	w := httptest.NewRecorder()
	app.handleMigrateStop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	var migStopResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &migStopResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if migStopResp["message"] != "Migration stop requested" {
		t.Errorf("message = %q, want %q", migStopResp["message"], "Migration stop requested")
	}
	if migStopResp["stopped"] != false {
		t.Errorf("stopped = %v, want false (no migration was running)", migStopResp["stopped"])
	}
}

func TestHandleLoadgenStop_ReturnsBody(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodPost, "/api/ping/stop", nil)
	w := httptest.NewRecorder()
	app.handleLoadgenStop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	var loadStopResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &loadStopResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if loadStopResp["message"] != "Load generator stop requested" {
		t.Errorf("message = %q, want %q", loadStopResp["message"], "Load generator stop requested")
	}
	if loadStopResp["stopped"] != false {
		t.Errorf("stopped = %v, want false (no loadgen was running)", loadStopResp["stopped"])
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
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"Permissions-Policy":        "camera=(), microphone=(), geolocation=()",
		"X-XSS-Protection":          "0",
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

func TestAppendLog_TruncatesLongLines(t *testing.T) {
	t.Parallel()
	app := &App{}
	app.appendLog(strings.Repeat("x", maxLogLineSize+100))
	app.migrationMutex.Lock()
	got := app.migrationOutput[0]
	app.migrationMutex.Unlock()
	if len(got) > maxLogLineSize+len(" ... [truncated]") {
		t.Fatalf("log line was not truncated, length=%d", len(got))
	}
	if !strings.HasSuffix(got, " ... [truncated]") {
		t.Fatalf("truncated log line missing suffix: %q", got)
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
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
	}
}

func TestHandleHealthz_HEAD(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	w := httptest.NewRecorder()
	handleHealthz(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for HEAD, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("expected Content-Type: text/plain, got %q", ct)
	}
}

func TestHandleStatus_HEAD(t *testing.T) {
	t.Parallel()
	app := &App{}
	req := httptest.NewRequest(http.MethodHead, "/api/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for HEAD, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type: application/json, got %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
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

func TestCSRFCheck_BlocksCrossOrigin(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)

	req := httptest.NewRequest(http.MethodPost, "/api/migrate", nil)
	req.Host = "dashboard.local:8080"
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin POST, got %v", w.Code)
	}
}

func TestCSRFCheck_AllowsSameOrigin(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)

	req := httptest.NewRequest(http.MethodPost, "/api/migrate", nil)
	req.Host = "dashboard.local:8080"
	req.Header.Set("Origin", "http://dashboard.local:8080")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for same-origin POST, got %v", w.Code)
	}
}

func TestCSRFCheck_AllowsNoOrigin(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)

	// POST without Origin header (curl, scripts) should be allowed.
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for POST without Origin, got %v", w.Code)
	}
}

func TestCSRFCheck_SkipsSafeMethods(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/api/status", nil)
		req.Host = "dashboard.local:8080"
		req.Header.Set("Origin", "https://evil.com")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s with cross-origin: expected 200, got %v", method, w.Code)
		}
	}
}

func TestCSRFCheck_RejectsMalformedOrigin(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)

	req := httptest.NewRequest(http.MethodPost, "/api/migrate", nil)
	req.Host = "dashboard.local:8080"
	req.Header.Set("Origin", "://not-a-url")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for malformed Origin, got %v", w.Code)
	}
}

func TestCSRFCheck_BlocksCrossOriginReferer(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)

	// POST without Origin but with cross-origin Referer should be rejected.
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", nil)
	req.Host = "dashboard.local:8080"
	req.Header.Set("Referer", "https://evil.com/attack")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin Referer, got %v", w.Code)
	}
}

func TestCSRFCheck_AllowsSameOriginReferer(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)

	// POST without Origin but with same-origin Referer should be allowed.
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", nil)
	req.Host = "dashboard.local:8080"
	req.Header.Set("Referer", "http://dashboard.local:8080/")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for same-origin Referer, got %v", w.Code)
	}
}

func TestPingRegex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line    string
		wantLat string
	}{
		{"64 bytes from 10.0.0.1: icmp_seq=1 ttl=64 time=0.123 ms", "0.123"},
		{"64 bytes from 10.0.0.1: icmp_seq=2 ttl=64 time=42.5 ms", "42.5"},
		{"64 bytes from 10.0.0.1: icmp_seq=3 ttl=64 time=1000 ms", "1000"},
		{"Request timeout for icmp_seq 0", ""},
		{"PING 10.0.0.1 (10.0.0.1) 56(84) bytes of data.", ""},
	}
	for _, tt := range tests {
		matches := pingRe.FindStringSubmatch(tt.line)
		if tt.wantLat == "" {
			if len(matches) > 1 {
				t.Errorf("line %q: expected no match, got %q", tt.line, matches[1])
			}
		} else {
			if len(matches) < 2 || matches[1] != tt.wantLat {
				t.Errorf("line %q: expected lat %q, got matches %v", tt.line, tt.wantLat, matches)
			}
		}
	}
}

func TestHandleMigrate_SharedStorageFlag(t *testing.T) {
	t.Parallel()
	app := &App{migrateScript: dummyMigrateScript(t)}
	t.Cleanup(func() {
		app.migrationMutex.Lock()
		if app.migrationCancel != nil {
			app.migrationCancel()
		}
		app.migrationMutex.Unlock()
	})
	form := validMigrateForm()
	form.Set("shared_storage", "true")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 with shared_storage=true, got %v", w.Code)
	}
}

func TestHandleMigrate_InvalidSharedStorage(t *testing.T) {
	t.Parallel()
	app := &App{}
	form := validMigrateForm()
	form.Set("shared_storage", "yes")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for shared_storage=yes, got %v", w.Code)
	}
}

func TestHandleMigrate_RejectsJSONContentType(t *testing.T) {
	t.Parallel()
	app := &App{}
	body := `{"source_node":"node1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for JSON content type, got %v", w.Code)
	}
}

func TestHandleMigrate_IgnoresQueryParams(t *testing.T) {
	t.Parallel()
	app := &App{}
	// Send required fields via query string only — PostFormValue should ignore them.
	req := httptest.NewRequest(http.MethodPost, "/api/migrate?source_node=node1&dest_node=node2&qmp_source=s&qmp_dest=d&dest_ip=10.0.0.2&vm_ip=10.244.1.5&tap=tap0&image=img", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when params are in query string only, got %v", w.Code)
	}
}

func TestHandleStatus_LoadgenRunning(t *testing.T) {
	t.Parallel()
	app := &App{}
	app.loadgenMutex.Lock()
	app.loadgenRunning = true
	app.loadgenType = "ping"
	app.loadgenMutex.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)

	var status StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}
	if !status.LoadgenRunning {
		t.Fatal("expected loadgen_running=true")
	}
	if status.LoadgenType != "ping" {
		t.Errorf("loadgen_type = %q, want %q", status.LoadgenType, "ping")
	}
}

func TestValidRequestID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id   string
		want bool
	}{
		{"abc-123", true},
		{"a", true},
		{strings.Repeat("x", 128), true},
		{"", false},
		{strings.Repeat("x", 129), false},
		{"has\nnewline", false},
		{"has\ttab", false},
		{"has\x00null", false},
		{"has\x7fDEL", false},
	}
	for _, tt := range tests {
		if got := validRequestID(tt.id); got != tt.want {
			t.Errorf("validRequestID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestResponseWriter_CapturesBytes(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
		w.Write([]byte(" world"))
	})
	handler := requestLogger(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != "hello world" {
		t.Fatalf("expected body %q, got %q", "hello world", w.Body.String())
	}
}

func TestRecoverMiddleware_APIPanic(t *testing.T) {
	t.Parallel()
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("api panic")
	})
	handler := recoverMiddleware(panicking)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected JSON Content-Type for /api/ panic, got %q", ct)
	}
}

func TestCSRFCheck_NonAPIPath(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfCheck(inner)
	req := httptest.NewRequest(http.MethodPost, "/non-api", nil)
	req.Host = "dashboard.local:8080"
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin POST on non-API path, got %v", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "json") {
		t.Fatalf("non-API CSRF rejection should use plain text, got Content-Type %q", ct)
	}
}

func TestSetMigrationResult(t *testing.T) {
	t.Parallel()
	app := &App{}

	app.setMigrationResult("success", "")
	app.migrationMutex.Lock()
	if app.lastMigrationResult != "success" {
		t.Errorf("lastMigrationResult = %q, want %q", app.lastMigrationResult, "success")
	}
	if app.migrationsSucceeded != 1 {
		t.Errorf("migrationsSucceeded = %d, want 1", app.migrationsSucceeded)
	}
	if app.migrationsFailed != 0 {
		t.Errorf("migrationsFailed = %d, want 0", app.migrationsFailed)
	}
	app.migrationMutex.Unlock()

	app.setMigrationResult("error", "something broke")
	app.migrationMutex.Lock()
	if app.lastMigrationResult != "error" {
		t.Errorf("lastMigrationResult = %q, want %q", app.lastMigrationResult, "error")
	}
	if app.lastMigrationError != "something broke" {
		t.Errorf("lastMigrationError = %q, want %q", app.lastMigrationError, "something broke")
	}
	if app.migrationsFailed != 1 {
		t.Errorf("migrationsFailed = %d, want 1", app.migrationsFailed)
	}
	if app.migrationsSucceeded != 1 {
		t.Errorf("migrationsSucceeded should still be 1, got %d", app.migrationsSucceeded)
	}
	app.migrationMutex.Unlock()
}

func TestHandleReadyz_WithScript(t *testing.T) {
	t.Parallel()
	app := &App{migrateScript: "/some/path"}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	app.handleReadyz(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with migrateScript set, got %v", w.Code)
	}
	if body := w.Body.String(); body != "ok\n" {
		t.Fatalf("expected body %q, got %q", "ok\n", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type = %q, want %q", ct, "text/plain")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
	}
}

func TestHandleReadyz_NoScript(t *testing.T) {
	t.Parallel()
	// No migrateScript set and migrate.sh not in expected locations.
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	app.handleReadyz(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when migrate.sh not found, got %v", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "not found") {
		t.Fatalf("expected 'not found' in body, got %q", body)
	}
}

func TestHandleMigrateStop_WithRunningMigration(t *testing.T) {
	t.Parallel()
	app := &App{migrateScript: slowMigrateScript(t)}
	// Start a migration so there's something to stop.
	form := validMigrateForm()
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start migration: expected 202, got %v", w.Code)
	}

	// Now stop it.
	stopReq := httptest.NewRequest(http.MethodPost, "/api/migrate/stop", nil)
	stopW := httptest.NewRecorder()
	app.handleMigrateStop(stopW, stopReq)
	if stopW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %v", stopW.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(stopW.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp["stopped"] != true {
		t.Errorf("stopped = %v, want true", resp["stopped"])
	}
	waitMigrationDone(t, app, 5*time.Second)
}

func TestGenerateID(t *testing.T) {
	t.Parallel()
	id := generateID()
	if len(id) != 16 {
		t.Fatalf("expected 16-char hex ID, got %d chars: %q", len(id), id)
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("expected lowercase hex, got char %q in %q", string(c), id)
		}
	}

	// IDs should be unique.
	id2 := generateID()
	if id == id2 {
		t.Fatalf("expected unique IDs, got %q twice", id)
	}
}

func TestRequestIDFromContext(t *testing.T) {
	t.Parallel()
	// Empty context returns empty string.
	if got := requestIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty string from empty context, got %q", got)
	}

	// Context with request ID returns it.
	ctx := context.WithValue(context.Background(), requestIDKey, "test-id-123")
	if got := requestIDFromContext(ctx); got != "test-id-123" {
		t.Fatalf("expected %q, got %q", "test-id-123", got)
	}
}

func TestHandleMigrate_BodyTooLarge(t *testing.T) {
	t.Parallel()
	app := &App{}
	// Create a body larger than maxBodySize (1 MB).
	largeBody := strings.Repeat("x="+strings.Repeat("y", 1024)+"&", 1100)
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(largeBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %v", w.Code)
	}
}

func TestRunCommand_ScriptFailure(t *testing.T) {
	t.Parallel()
	app := &App{migrateScript: failingMigrateScript(t)}
	form := validMigrateForm()
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}

	waitMigrationDone(t, app, 5*time.Second)

	app.migrationMutex.Lock()
	result := app.lastMigrationResult
	errMsg := app.lastMigrationError
	failed := app.migrationsFailed
	app.migrationMutex.Unlock()

	if result != "error" {
		t.Fatalf("expected lastMigrationResult='error', got %q", result)
	}
	if errMsg == "" {
		t.Fatal("expected non-empty lastMigrationError")
	}
	if !strings.Contains(errMsg, "host unreachable") {
		t.Fatalf("expected error to contain script output, got %q", errMsg)
	}
	if failed != 1 {
		t.Fatalf("expected migrationsFailed=1, got %d", failed)
	}
}

func TestPodsAndNodesEndpoints(t *testing.T) {
	// Stub kubectl: branches on $1 (pods/nodes) to return appropriate JSON.
	dir := t.TempDir()
	stub := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
case "$1" in
  get)
    case "$2" in
      pods)
        cat <<'EOF'
{"items":[{"metadata":{"namespace":"default","name":"vm-a"},"spec":{"runtimeClassName":"kata-qemu","nodeName":"n1"},"status":{"podIP":"10.0.0.5"}}]}
EOF
        ;;
      nodes)
        cat <<'EOF'
{"items":[{"metadata":{"name":"n1"},"status":{"addresses":[{"type":"InternalIP","address":"192.168.1.10"}]}}]}
EOF
        ;;
    esac
    ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	app := &App{}
	mux := app.newMux(false)

	tests := []struct {
		name string
		path string
	}{
		{"pods", "/api/pods"},
		{"nodes", "/api/nodes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json prefix", ct)
			}
			var arr []map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
				t.Fatalf("decode JSON array: %v; body=%s", err, w.Body.String())
			}
			if len(arr) == 0 {
				t.Fatalf("expected non-empty array, got %v", arr)
			}
		})
	}
}

func TestRunCommand_ScriptSuccess(t *testing.T) {
	t.Parallel()
	app := &App{migrateScript: dummyMigrateScript(t)}
	form := validMigrateForm()
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v", w.Code)
	}

	waitMigrationDone(t, app, 5*time.Second)

	app.migrationMutex.Lock()
	result := app.lastMigrationResult
	succeeded := app.migrationsSucceeded
	isMigrating := app.isMigrating
	app.migrationMutex.Unlock()

	if result != "success" {
		t.Fatalf("expected lastMigrationResult='success', got %q", result)
	}
	if succeeded != 1 {
		t.Fatalf("expected migrationsSucceeded=1, got %d", succeeded)
	}
	if isMigrating {
		t.Fatal("expected isMigrating=false after completion")
	}
}

func TestMigrateHandler_PodPickerMode(t *testing.T) {
	// Stub kubectl: dispatches on `get pod` vs `get node` to return the
	// nodeName (for pod lookup) or InternalIP (for node lookup).
	dir := t.TempDir()
	stub := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
# Args may be: -n <ns> get pod <name> -o jsonpath=...
#         or:  get node <name> -o jsonpath=...
for a in "$@"; do
  case "$a" in
    pod) kind=pod ;;
    node) kind=node ;;
  esac
done
case "$kind" in
  pod) printf 'src-node' ;;
  node) printf '192.168.1.20' ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// Migrate script writes its argv (one per line) to a file we can inspect.
	scriptDir := t.TempDir()
	argFile := filepath.Join(scriptDir, "args.txt")
	scriptPath := filepath.Join(scriptDir, "migrate.sh")
	scriptBody := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argFile + "\nexit 0\n"
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}

	app := &App{migrateScript: scriptPath}
	t.Cleanup(func() {
		app.migrationMutex.Lock()
		if app.migrationCancel != nil {
			app.migrationCancel()
		}
		app.migrationMutex.Unlock()
	})

	form := url.Values{}
	form.Set("source_pod_namespace", "default")
	form.Set("source_pod_name", "vm-a")
	form.Set("dest_node", "dst-node")
	form.Set("image", "katamaran:dev")

	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %v: %s", w.Code, w.Body.String())
	}

	waitMigrationDone(t, app, 5*time.Second)

	raw, err := os.ReadFile(argFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	// Helper: assert flag is present with the given value.
	hasFlag := func(flag, val string) bool {
		for i := 0; i < len(argv)-1; i++ {
			if argv[i] == flag && argv[i+1] == val {
				return true
			}
		}
		return false
	}

	expects := []struct{ flag, val string }{
		{"--pod-name", "vm-a"},
		{"--pod-namespace", "default"},
		{"--source-node", "src-node"},
		{"--dest-node", "dst-node"},
		{"--dest-ip", "192.168.1.20"},
		{"--image", "katamaran:dev"},
	}
	for _, e := range expects {
		if !hasFlag(e.flag, e.val) {
			t.Errorf("argv missing %s %s; argv=%v", e.flag, e.val, argv)
		}
	}
}

func TestMigrateHandler_PodPickerSameNodeRejected(t *testing.T) {
	// Stub kubectl so the source pod resolves to the same node as dest.
	dir := t.TempDir()
	stub := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
for a in "$@"; do
  case "$a" in
    pod) kind=pod ;;
    node) kind=node ;;
  esac
done
case "$kind" in
  pod) printf 'same-node' ;;
  node) printf '10.0.0.1' ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	app := &App{migrateScript: dummyMigrateScript(t)}
	form := url.Values{}
	form.Set("source_pod_namespace", "default")
	form.Set("source_pod_name", "vm-a")
	form.Set("dest_node", "same-node")
	form.Set("image", "katamaran:dev")

	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when source==dest, got %v: %s", w.Code, w.Body.String())
	}
}
