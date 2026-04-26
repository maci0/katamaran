package migration

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProc is a stub implementation of the procFS interface used by the
// resolveSandbox tests. PIDForSandbox and NetnsHasIP are driven by simple
// per-uuid maps so each test case can declare exactly which sandboxes match.
type fakeProc struct {
	pids     map[string]int   // sandbox uuid -> pid (errors if absent)
	hasIP    map[int]bool     // pid -> whether netns contains the queried IP
	pidErr   map[string]error // optional per-uuid PIDForSandbox error
	netnsErr map[int]error    // optional per-pid NetnsHasIP error
}

func (f *fakeProc) PIDForSandbox(uuid string) (int, error) {
	if f.pidErr != nil {
		if err, ok := f.pidErr[uuid]; ok {
			return 0, err
		}
	}
	pid, ok := f.pids[uuid]
	if !ok {
		return 0, errors.New("no pid for sandbox " + uuid)
	}
	return pid, nil
}

func (f *fakeProc) NetnsHasIP(pid int, ip string) (bool, error) {
	if f.netnsErr != nil {
		if err, ok := f.netnsErr[pid]; ok {
			return false, err
		}
	}
	_ = ip
	return f.hasIP[pid], nil
}

// makeSandboxRoot creates a temp directory with empty subdirectories named
// for each provided sandbox uuid. Returns the temp directory path.
func makeSandboxRoot(t *testing.T, uuids ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, uuid := range uuids {
		if err := os.Mkdir(filepath.Join(root, uuid), 0o755); err != nil {
			t.Fatalf("mkdir sandbox %s: %v", uuid, err)
		}
	}
	return root
}

func TestResolveSandboxByPodIP_SingleMatch(t *testing.T) {
	t.Parallel()

	root := makeSandboxRoot(t, "sb-aaa", "sb-bbb", "sb-ccc")
	fp := &fakeProc{
		pids: map[string]int{
			"sb-aaa": 1001,
			"sb-bbb": 1002,
			"sb-ccc": 1003,
		},
		hasIP: map[int]bool{
			1001: false,
			1002: true, // only this one matches
			1003: false,
		},
	}

	got, err := resolveSandbox(root, fp, "10.0.0.5")
	if err != nil {
		t.Fatalf("resolveSandbox returned error: %v", err)
	}
	if got.Sandbox != "sb-bbb" {
		t.Errorf("Sandbox = %q, want %q", got.Sandbox, "sb-bbb")
	}
	if got.PID != 1002 {
		t.Errorf("PID = %d, want %d", got.PID, 1002)
	}
}

func TestResolveSandboxByPodIP_NoMatch(t *testing.T) {
	t.Parallel()

	root := makeSandboxRoot(t, "sb-x", "sb-y")
	fp := &fakeProc{
		pids:  map[string]int{"sb-x": 11, "sb-y": 22},
		hasIP: map[int]bool{11: false, 22: false},
	}

	_, err := resolveSandbox(root, fp, "10.0.0.42")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "10.0.0.42") {
		t.Errorf("error should mention pod IP; got: %v", err)
	}
}

func TestResolveSandboxByPodIP_AmbiguousMatch(t *testing.T) {
	t.Parallel()

	root := makeSandboxRoot(t, "sb-1", "sb-2", "sb-3")
	fp := &fakeProc{
		pids: map[string]int{
			"sb-1": 100,
			"sb-2": 200,
			"sb-3": 300,
		},
		hasIP: map[int]bool{
			100: true,
			200: true, // ambiguous: two matches
			300: false,
		},
	}

	_, err := resolveSandbox(root, fp, "10.0.0.7")
	if err == nil {
		t.Fatal("expected ambiguous error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention 'ambiguous'; got: %v", err)
	}
}

// writeFile is a small helper that writes content to path or fails the test.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// setupAPIServer starts an httptest TLS server and wires the package-level
// LookupPodIP injection points to point at it. Returns the server (for caller
// to close) and the cleanup function. The token is fixed to "test-token";
// the handler asserts that the Authorization header carries it.
func setupAPIServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	// Encode the test server's CA into a temp dir alongside a fake token
	// and namespace, then point the package-level paths at it.
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	caFile := filepath.Join(dir, "ca.crt")
	nsFile := filepath.Join(dir, "namespace")
	writeFile(t, tokenFile, []byte("test-token"))
	writeFile(t, nsFile, []byte("default"))

	// httptest.NewTLSServer.Certificate() returns the leaf cert; PEM-encode it
	// so the production code path (which expects a CA bundle) can parse it.
	cert := srv.Certificate()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	writeFile(t, caFile, pemBytes)

	// Sanity: ensure cert parses as expected (catches PEM/DER mistakes early).
	if _, err := x509.ParseCertificate(cert.Raw); err != nil {
		t.Fatalf("parse server cert: %v", err)
	}

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv URL: %v", err)
	}

	prevToken, prevCA, prevHost, prevPort := tokenPath, caPath, apiserverHost, apiserverPort
	prevB1, prevB2, prevB3 := lookupBackoff1, lookupBackoff2, lookupBackoff3
	tokenPath = tokenFile
	caPath = caFile
	apiserverHost = u.Hostname()
	apiserverPort = u.Port()
	lookupBackoff1 = time.Millisecond
	lookupBackoff2 = time.Millisecond
	lookupBackoff3 = time.Millisecond
	t.Cleanup(func() {
		tokenPath = prevToken
		caPath = prevCA
		apiserverHost = prevHost
		apiserverPort = prevPort
		lookupBackoff1 = prevB1
		lookupBackoff2 = prevB2
		lookupBackoff3 = prevB3
	})

	return srv
}

func TestLookupPodIP_RetryUntilSuccess(t *testing.T) {
	// Not t.Parallel(): this test mutates package-level vars (tokenPath,
	// caPath, apiserverHost, lookupBackoff*) via setupAPIServer, and the
	// other LookupPodIP test does the same. Running them in parallel races.

	var calls atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-token")
		}
		wantPath := "/api/v1/namespaces/myns/pods/mypod"
		if r.URL.Path != wantPath {
			t.Errorf("URL path = %q, want %q", r.URL.Path, wantPath)
		}
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		var payload map[string]any
		if n < 3 {
			payload = map[string]any{"status": map[string]any{"podIP": ""}}
		} else {
			payload = map[string]any{"status": map[string]any{"podIP": "10.244.0.17"}}
		}
		_ = json.NewEncoder(w).Encode(payload)
	}
	setupAPIServer(t, handler)

	ip, err := LookupPodIP(context.Background(), "myns", "mypod")
	if err != nil {
		t.Fatalf("LookupPodIP returned error: %v", err)
	}
	if ip != "10.244.0.17" {
		t.Errorf("ip = %q, want %q", ip, "10.244.0.17")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
}

func TestLookupPodIP_FailureAfterRetries(t *testing.T) {
	// Not t.Parallel(): see TestLookupPodIP_RetryUntilSuccess.

	var calls atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"podIP": ""},
		})
	}
	setupAPIServer(t, handler)

	_, err := LookupPodIP(context.Background(), "ns1", "podX")
	if err == nil {
		t.Fatal("expected error after retries, got nil")
	}
	if !strings.Contains(err.Error(), "ns1/podX") {
		t.Errorf("error should mention pod identity; got: %v", err)
	}
	if !strings.Contains(err.Error(), "no IP") {
		t.Errorf("error should mention 'no IP'; got: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
}
