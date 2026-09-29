package migration

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProc is a stub implementation of the procFS interface used by the
// resolveSandbox tests. PIDsForSandboxes and NetnsHasIP are driven by simple
// per-uuid maps so each test case can declare exactly which sandboxes match.
type fakeProc struct {
	pids     map[string]int // sandbox uuid -> pid (absent = no process found)
	hasIP    map[int]bool   // pid -> whether netns contains the queried IP
	netnsErr map[int]error  // optional per-pid NetnsHasIP error
}

func (f *fakeProc) PIDsForSandboxes(uuids []string) map[string]int {
	out := make(map[string]int, len(uuids))
	for _, uuid := range uuids {
		if pid, ok := f.pids[uuid]; ok {
			out[uuid] = pid
		}
	}
	return out
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

// TestRealProcPIDsForSandboxes_FindsSpawnedProcess exercises the production
// /proc scanner, which every resolveSandbox test bypasses via fakeProc. A
// regression in the real scanner (sandboxUUIDRe gate, literal substring match
// against the raw NUL-separated cmdline) would otherwise only surface at
// migration time on a live node as "no sandbox contains pod IP".
func TestRealProcPIDsForSandboxes_FindsSpawnedProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux /proc")
	}
	t.Parallel()

	const uuid = "11111111-2222-3333-4444-555555555555"
	// Helper whose argv carries sandbox-<uuid>: the interpreter loops so the
	// process and its cmdline stay observable until we kill it. PIDsForSandboxes
	// matches the needle literally against /proc/<pid>/cmdline.
	script := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	cmd := exec.Command(script, "sandbox-"+uuid)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	pidPath := fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(pidPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper pid %d never appeared under /proc", cmd.Process.Pid)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := (realProc{}).PIDsForSandboxes([]string{uuid})
	pid, ok := got[uuid]
	if !ok {
		t.Fatalf("sandbox %s not resolved; got %v", uuid, got)
	}
	if pid != cmd.Process.Pid {
		t.Fatalf("sandbox %s resolved to pid %d, want %d", uuid, pid, cmd.Process.Pid)
	}

	// Unknown identifiers must be absent; invalid ones (regex gate) skipped.
	empty := (realProc{}).PIDsForSandboxes([]string{"no-such-sandbox-ffffffff", "../evil"})
	if len(empty) != 0 {
		t.Fatalf("unexpected resolutions for unknown/invalid uuids: %v", empty)
	}
}

// TestRecordSandboxMatches_MatchesNaiveNeedleSearch pins recordSandboxMatches
// to the literal-substring semantics it replaced. The scanner anchors on the
// shared "sandbox-" prefix and prefix-compares the following token; this table
// asserts the result is identical to testing bytes.Contains(raw, "sandbox-"+uuid)
// for every uuid, including the cases where the two could plausibly diverge:
// repeated anchors for one sandbox, a uuid that is a prefix of another, and
// token terminators.
func TestRecordSandboxMatches_MatchesNaiveNeedleSearch(t *testing.T) {
	t.Parallel()

	uuids := []string{"abc", "abcd", "a", "x-1.2_3", "zzz"}
	// A QEMU-like cmdline: one sandbox referenced by several args, a second
	// sandbox, and a "sandbox-" with no identifier behind it.
	raw := []byte("qemu\x00-smp\x001\x00-object\x00memory-backend-file,id=mem0,mem-path=/dev/shm/sandbox-abc/page\x00" +
		"-qmp\x00unix:path=/run/vc/vm/sandbox-abc/qmp.sock,server=on\x00" +
		"drive\x00file=/run/kata/x-x-1.2_3.img\x00" +
		"-chardev\x00socket,path=/run/vc/vm/sandbox-/x\x00" +
		"-name\x00sandbox-abcd-extra\x00")

	// Reference: what the previous per-uuid bytes.Contains loop returned.
	wantMatched := make(map[string]bool, len(uuids))
	for _, uuid := range uuids {
		if bytes.Contains(raw, []byte("sandbox-"+uuid)) {
			wantMatched[uuid] = true
		}
	}

	wanted := make(map[string]*procWant, len(uuids))
	for _, uuid := range uuids {
		wanted[uuid] = &procWant{uuid: []byte(uuid)}
	}
	var multi []string
	recordSandboxMatches(raw, wanted, 4242, 1, &multi)

	// Exactly one process was scanned, so every resolved uuid must carry that
	// PID and nothing may be reported as a multi-process match.
	for uuid, w := range wanted {
		switch {
		case wantMatched[uuid] && w.best != 4242:
			t.Errorf("uuid %q: expected a match, got best=%d", uuid, w.best)
		case !wantMatched[uuid] && w.best != 0:
			t.Errorf("uuid %q: unexpected match at pid %d", uuid, w.best)
		}
	}
	if len(multi) != 0 {
		t.Errorf("single-process scan reported multi-process matches: %v", multi)
	}

	// A second, higher-numbered process matching the same uuids must be
	// reported once each and must not displace the lower PID already recorded.
	recordSandboxMatches(raw, wanted, 5000, 2, &multi)
	for uuid, w := range wanted {
		if wantMatched[uuid] && w.best != 4242 {
			t.Errorf("uuid %q: lowest PID not kept, got %d", uuid, w.best)
		}
	}
	for _, uuid := range multi {
		if !wantMatched[uuid] {
			t.Errorf("multi-match reported for unmatched uuid %q", uuid)
		}
	}
	if want := len(wantMatched); len(multi) != want {
		t.Errorf("multi-match list = %v, want one entry per matched uuid (%d)", multi, want)
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
	prevBackoffs := lookupBackoffs
	tokenPath = tokenFile
	caPath = caFile
	apiserverHost = u.Hostname()
	apiserverPort = u.Port()
	lookupBackoffs = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() {
		tokenPath = prevToken
		caPath = prevCA
		apiserverHost = prevHost
		apiserverPort = prevPort
		lookupBackoffs = prevBackoffs
	})

	return srv
}

func TestLookupPodIP_RetryUntilSuccess(t *testing.T) {
	// Not t.Parallel(): this test mutates package-level vars (tokenPath,
	// caPath, apiserverHost, lookupBackoffs) via setupAPIServer, and the
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

func TestResolveAPIServerHostPort(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		port    string
		want    string
		wantErr string
	}{
		{name: "in-cluster defaults", host: "10.96.0.1", port: "443", want: "443"},
		{name: "non-numeric port is rejected", host: "10.96.0.1", port: "https", wantErr: "not a port number"},
		{name: "empty port", host: "10.96.0.1", port: "", wantErr: "not set"},
		{name: "empty host", host: "", port: "443", wantErr: "not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", tt.host)
			t.Setenv("KUBERNETES_SERVICE_PORT", tt.port)
			apiserverHost, apiserverPort = "", ""
			host, port, err := resolveAPIServerHostPort()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveAPIServerHostPort = %q/%q, want error containing %q", host, port, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAPIServerHostPort: %v", err)
			}
			if host != tt.host || port != tt.want {
				t.Fatalf("resolveAPIServerHostPort = %q/%q, want %q/%q", host, port, tt.host, tt.want)
			}
		})
	}
}
