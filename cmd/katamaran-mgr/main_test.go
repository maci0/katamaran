package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/controller"
)

func TestMigrationImageStartupValidation(t *testing.T) {
	if os.Getenv("KATAMARAN_TEST_IMAGE_STARTUP") == "1" {
		os.Args = []string{"katamaran-mgr"}
		main()
		return
	}
	for _, image := range []string{"", "invalid image"} {
		t.Run(image, func(t *testing.T) {
			t.Setenv("KATAMARAN_TEST_IMAGE_STARTUP", "1")
			t.Setenv("KATAMARAN_MIGRATION_IMAGE", image)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationImageStartupValidation$")
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("exit = %v, want 2; output: %s", err, output)
			}
			if !strings.Contains(string(output), "KATAMARAN_MIGRATION_IMAGE") {
				t.Fatalf("missing image configuration error: %s", output)
			}
		})
	}
}

func TestDebugReadinessDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := debugMux(ctx, nil)
	for _, draining := range []bool{false, true} {
		if draining {
			cancel()
		}
		for _, path := range []string{"/healthz", "/readyz"} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			want := http.StatusOK
			if draining && path == "/readyz" {
				want = http.StatusServiceUnavailable
			}
			if w.Code != want {
				t.Fatalf("%s draining=%v: status=%d, want %d", path, draining, w.Code, want)
			}
		}
	}
}

// TestDebugReadinessChecksDependency pins the split between the two probes:
// /readyz reports 503 while a dependency is down, /healthz keeps returning
// 200 so a dependency outage removes the pod from the webhook Service's
// endpoints without restarting the controller.
func TestDebugReadinessChecksDependency(t *testing.T) {
	ctx := context.Background()
	down := errors.New("apiserver unreachable")
	mux := debugMux(ctx, func(context.Context) error { return down })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d, want 503", w.Code)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz status=%d, want 200 during a dependency outage", w.Code)
	}
}

func TestAPIServerReachableDisabledWithoutClient(t *testing.T) {
	t.Parallel()
	if got := apiServerReachable(nil); got != nil {
		t.Fatalf("apiServerReachable(nil) = %v, want nil", got)
	}
}

func TestServeHTTPDrainsRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "drained")
	})}
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, srv, func() error { return srv.Serve(listener) }) }()
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: time.Second * 5}
		resp, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			defer resp.Body.Close()
			var body []byte
			body, err = io.ReadAll(resp.Body)
			if err == nil && string(body) != "drained" {
				err = errors.New("response was not drained")
			}
		}
		response <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("server returned before request completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case err := <-response:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not drain")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServeHTTPListenFailure(t *testing.T) {
	want := errors.New("listen failed")
	if err := serveHTTP(context.Background(), &http.Server{}, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
}

func TestValidListenAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		want bool
	}{
		{":8081", true},
		{"0.0.0.0:8081", true},
		{"[::1]:8081", true},
		{"localhost:8081", true},
		{":0", true},
		{":http", true},
		{"localhost", false},
		{":65536", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			if got := validListenAddr(tt.addr); got != tt.want {
				t.Fatalf("validListenAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestResolvePodWaitTimeout(t *testing.T) {
	const defaultTimeout = 60 * time.Second
	tests := []struct {
		name    string
		env     string
		args    []string
		want    time.Duration
		wantErr string
	}{
		{name: "unset keeps the flag default", want: defaultTimeout},
		{name: "env overrides the default", env: "5m", want: 5 * time.Minute},
		{name: "explicit flag beats env", env: "5m", args: []string{"--pod-wait-timeout=90s"}, want: 90 * time.Second},
		{name: "explicit flag wins over a malformed env", env: "not-a-duration", args: []string{"--pod-wait-timeout=90s"}, want: 90 * time.Second},
		{name: "malformed env is an error", env: "not-a-duration", wantErr: "KATAMARAN_POD_WAIT_TIMEOUT"},
		{name: "unitless env is an error", env: "300", wantErr: "KATAMARAN_POD_WAIT_TIMEOUT"},
		{name: "zero env is an error", env: "0s", wantErr: "must be greater than 0"},
		{name: "negative env is an error", env: "-1m", wantErr: "must be greater than 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KATAMARAN_POD_WAIT_TIMEOUT", tt.env)
			fs := flag.NewFlagSet("katamaran-mgr", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			value := fs.Duration("pod-wait-timeout", defaultTimeout, "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("Parse(%v): %v", tt.args, err)
			}
			got, err := resolvePodWaitTimeout(fs, *value)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("resolvePodWaitTimeout = %v, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePodWaitTimeout: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolvePodWaitTimeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteProgressMetricsOrder(t *testing.T) {
	t.Parallel()
	snap := map[string]controller.MigrationProgressEntry{
		"mig-b": {Phase: "Transferring", RAMTransferred: 2, RAMTotal: 10, DowntimeMS: 20, AppliedDowntimeMS: 200, RTTMS: 2},
		"mig-c": {Phase: "Succeeded"},
		"mig-a": {Phase: "Failed", RAMTransferred: 1, RAMTotal: 10, DowntimeMS: 10, AppliedDowntimeMS: 100, RTTMS: 1},
	}
	var first, second bytes.Buffer
	writeProgressMetrics(&first, snap)
	writeProgressMetrics(&second, snap)
	if first.String() != second.String() {
		t.Fatalf("repeated scrapes of the same snapshot differ:\n%s\n---\n%s", first.String(), second.String())
	}
	want := []string{
		"katamaran_migration_ram_transferred_bytes{migration_id=\"mig-a\"} 1\n",
		"katamaran_migration_ram_transferred_bytes{migration_id=\"mig-b\"} 2\n",
		"katamaran_migration_ram_transferred_bytes{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_ram_total_bytes{migration_id=\"mig-a\"} 10\n",
		"katamaran_migration_ram_total_bytes{migration_id=\"mig-b\"} 10\n",
		"katamaran_migration_ram_total_bytes{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_phase{migration_id=\"mig-a\",phase=\"Failed\"} 1\n",
		"katamaran_migration_phase{migration_id=\"mig-b\",phase=\"Transferring\"} 1\n",
		"katamaran_migration_phase{migration_id=\"mig-c\",phase=\"Succeeded\"} 1\n",
		"katamaran_migration_downtime_ms{migration_id=\"mig-a\"} 10\n",
		"katamaran_migration_downtime_ms{migration_id=\"mig-b\"} 20\n",
		"katamaran_migration_downtime_ms{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_applied_downtime_ms{migration_id=\"mig-a\"} 100\n",
		"katamaran_migration_applied_downtime_ms{migration_id=\"mig-b\"} 200\n",
		"katamaran_migration_applied_downtime_ms{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_rtt_ms{migration_id=\"mig-a\"} 1\n",
		"katamaran_migration_rtt_ms{migration_id=\"mig-b\"} 2\n",
		"katamaran_migration_rtt_ms{migration_id=\"mig-c\"} 0\n",
	}
	var samples []string
	for line := range strings.SplitSeq(first.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			samples = append(samples, line+"\n")
		}
	}
	if got := strings.Join(samples, ""); got != strings.Join(want, "") {
		t.Fatalf("metric samples differ:\ngot:\n%swant:\n%s", got, strings.Join(want, ""))
	}
}
