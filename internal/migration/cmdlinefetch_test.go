package migration

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParsePodRef(t *testing.T) {
	t.Parallel()

	ns, pod, err := parsePodRef("default/kata-demo")
	if err != nil {
		t.Fatalf("parsePodRef valid ref: %v", err)
	}
	if ns != "default" || pod != "kata-demo" {
		t.Fatalf("parsePodRef = %q/%q, want default/kata-demo", ns, pod)
	}

	for _, ref := range []string{"", "default", "/kata-demo", "default/"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			if _, _, err := parsePodRef(ref); err == nil {
				t.Fatalf("parsePodRef(%q) succeeded, want error", ref)
			}
		})
	}
}

func TestScanPodLogMarkersFindsMarkers(t *testing.T) {
	t.Parallel()

	const token = "test-token"
	const vmMarker = "KATAMARAN_VMCONFIG_B64="
	const agentMarker = "KATAMARAN_AGENTCONFIG_B64="
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want Bearer %s", got, token)
		}
		_, _ = fmt.Fprintln(w, "ordinary log line")
		_, _ = fmt.Fprintf(w, "prefix %s%s\n", vmMarker, "vm-config")
		_, _ = fmt.Fprintf(w, "%s%s   \n", agentMarker, "agent-config")
	}))
	t.Cleanup(srv.Close)

	found, scanned, err := scanPodLogMarkers(context.Background(), srv.Client(), srv.URL, token, vmMarker, agentMarker)
	if err != nil {
		t.Fatalf("scanPodLogMarkers: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanPodLogMarkers scanned 0 bytes, want log body bytes counted")
	}
	if got := found[vmMarker]; got != "vm-config" {
		t.Fatalf("VMConfig marker = %q, want vm-config", got)
	}
	if got := found[agentMarker]; got != "agent-config" {
		t.Fatalf("AgentConfig marker = %q, want agent-config", got)
	}
}

func TestScanPodLogMarkersReturnsHTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	found, scanned, err := scanPodLogMarkers(context.Background(), srv.Client(), srv.URL, "token", "KATAMARAN_CMDLINE_B64=")
	if err == nil {
		t.Fatal("scanPodLogMarkers succeeded, want HTTP status error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("scanPodLogMarkers error = %v, want status 503", err)
	}
	if found != nil || scanned != 0 {
		t.Fatalf("scanPodLogMarkers returned found=%v scanned=%d on HTTP error, want nil/0", found, scanned)
	}
}

func TestFetchCmdlineFromPodLogWritesDecodedCmdline(t *testing.T) {
	cmdline := []byte("/usr/bin/qemu-system-x86_64\x00-name\x00sandbox-demo")
	setupAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		assertPodLogRequest(t, r, "myns", "mypod")
		_, _ = fmt.Fprintf(w, "noise\nKATAMARAN_CMDLINE_B64=%s\n", base64.StdEncoding.EncodeToString(cmdline))
	})

	path, err := fetchCmdlineFromPodLog(context.Background(), "myns/mypod")
	if err != nil {
		t.Fatalf("fetchCmdlineFromPodLog: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read decoded cmdline: %v", err)
	}
	if !bytes.Equal(got, cmdline) {
		t.Fatalf("decoded cmdline = %q, want %q", got, cmdline)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat decoded cmdline: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("decoded cmdline mode = %v, want 0600", got)
	}
}

func TestFetchVMConfigFromPodLogReadsMarkers(t *testing.T) {
	vmConfig := []byte(`{"HypervisorType":"qemu"}`)
	agentConfig := []byte(`{"Debug":true}`)
	setupAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		assertPodLogRequest(t, r, "myns", "mypod")
		_, _ = fmt.Fprintf(w, "KATAMARAN_AGENTCONFIG_B64=%s\n", base64.StdEncoding.EncodeToString(agentConfig))
		_, _ = fmt.Fprintf(w, "KATAMARAN_VMCONFIG_B64=%s\n", base64.StdEncoding.EncodeToString(vmConfig))
	})

	gotVMConfig, gotAgentConfig := fetchVMConfigFromPodLog(context.Background(), "myns/mypod")
	if !bytes.Equal(gotVMConfig, vmConfig) {
		t.Fatalf("VMConfig = %s, want %s", gotVMConfig, vmConfig)
	}
	if !bytes.Equal(gotAgentConfig, agentConfig) {
		t.Fatalf("AgentConfig = %s, want %s", gotAgentConfig, agentConfig)
	}
}

func assertPodLogRequest(t *testing.T, r *http.Request, ns, pod string) {
	t.Helper()

	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization header = %q, want Bearer test-token", got)
	}
	if got, want := r.URL.Path, "/api/v1/namespaces/"+ns+"/pods/"+pod+"/log"; got != want {
		t.Errorf("URL path = %q, want %q", got, want)
	}
	if got := r.URL.Query().Get("container"); got != "katamaran" {
		t.Errorf("container query = %q, want katamaran", got)
	}
}
