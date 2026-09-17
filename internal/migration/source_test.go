package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/icmp"

	"github.com/maci0/katamaran/internal/qmp"
	"github.com/maci0/katamaran/internal/qmptest"
)

// Common test addresses used across source migration tests.
var (
	testDestIP = netip.MustParseAddr("10.0.0.1")
	testVMIP   = netip.MustParseAddr("10.244.1.15")
)

func TestErrMigrationFailed_Exists(t *testing.T) {
	t.Parallel()
	// errors.Is(x, x) is reflexively true for any non-nil error, so it can't
	// detect a regression. Assert non-nil + non-empty message + distinctness.
	if errMigrationFailed == nil || errMigrationFailed.Error() == "" {
		t.Fatal("errMigrationFailed should be a non-nil sentinel with a message")
	}
	if errMigrationCancelled == nil || errMigrationCancelled.Error() == "" {
		t.Fatal("errMigrationCancelled should be a non-nil sentinel with a message")
	}
	if errors.Is(errMigrationFailed, errMigrationCancelled) {
		t.Fatal("errMigrationFailed and errMigrationCancelled should be distinct sentinels")
	}
}

func TestRunSource_Failures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		sharedStorage bool
		tunnelMode    TunnelMode
	}{
		{"BadQMPSocket", false, TunnelModeIPIP},
		{"SharedStorage_BadQMPSocket", true, TunnelModeIPIP},
		{"NonShared_BadQMPSocket", false, TunnelModeGRE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunSource(context.Background(), SourceConfig{
				QMPSocket:       "/nonexistent/qmp.sock",
				DestIP:          testDestIP,
				VMIP:            testVMIP,
				DriveIDs:        []string{"drive-virtio-disk0"},
				SharedStorage:   tt.sharedStorage,
				TunnelMode:      tt.tunnelMode,
				DowntimeLimitMS: 25,
			})
			if err == nil {
				t.Fatal("expected error for nonexistent QMP socket")
			}
			if !strings.Contains(err.Error(), "QMP") {
				t.Fatalf("expected QMP-related error, got: %v", err)
			}
		})
	}
}

func TestRunSource_ConfigValidation(t *testing.T) {
	t.Parallel()
	base := SourceConfig{
		QMPSocket:       "/nonexistent/qmp.sock",
		DestIP:          testDestIP,
		VMIP:            testVMIP,
		DriveIDs:        []string{"drive-virtio-disk0"},
		SharedStorage:   true,
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
	}
	tests := []struct {
		name string
		cfg  SourceConfig
		want string
	}{
		{"InvalidDestIP", func() SourceConfig { c := base; c.DestIP = netip.Addr{}; return c }(), "invalid destination address"},
		{"InvalidVMIP", func() SourceConfig { c := base; c.VMIP = netip.Addr{}; return c }(), "invalid VM address"},
		{"FamilyMismatch", func() SourceConfig { c := base; c.VMIP = netip.MustParseAddr("fd00::1"); return c }(), "address families must match"},
		{"InvalidTunnelMode", func() SourceConfig { c := base; c.TunnelMode = TunnelMode("vxlan"); return c }(), "invalid tunnel mode"},
		{"NegativeMultifd", func() SourceConfig { c := base; c.MultifdChannels = -1; return c }(), "multifd channels must be non-negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunSource(context.Background(), tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RunSource error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// QMP test helpers (StartFakeQMP, QMPHandshake, ConsumeCommand) are in internal/qmptest.

func TestWaitForStorageSync_JobReady(t *testing.T) {
	t.Parallel()
	var polls atomic.Int32
	sock, rec := startRecordingQMP(t, func(_ net.Conn, cmd recordedQMPCommand) string {
		if cmd.Execute != "query-block-jobs" {
			t.Errorf("execute = %q, want query-block-jobs", cmd.Execute)
			return `{"error":{"class":"CommandNotFound","desc":"unexpected command"}}`
		}
		if polls.Add(1) == 1 {
			return `{"return":[{"device":"mirror-drive0","len":1000,"offset":500,"ready":false,"status":"running","type":"mirror"}]}`
		}
		return `{"return":[{"device":"mirror-drive0","len":1000,"offset":1000,"ready":true,"status":"running","type":"mirror"}]}`
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForStorageSync(ctx, client, "mirror-drive0")
	if err != nil {
		t.Fatalf("waitForStorageSync: %v", err)
	}
	if got := len(rec.Commands()); got != 2 {
		t.Fatalf("block job queries = %d, want 2 (running then ready)", got)
	}
}

func TestWaitForStorageSync_JobDisappears(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// First poll: job present.
		qmptest.ConsumeCommand(conn)
		jobs := []qmp.BlockJobInfo{{Device: "mirror-drive0", Len: 1000, Offset: 500, Status: "running", Type: "mirror"}}
		b, _ := json.Marshal(jobs)
		conn.Write([]byte(`{"return":` + string(b) + "}\n"))
		// Second poll: job gone.
		qmptest.ConsumeCommand(conn)
		conn.Write([]byte(`{"return":[]}` + "\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForStorageSync(ctx, client, "mirror-drive0")
	if err == nil {
		t.Fatal("expected error when job disappears")
	}
	if !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("expected 'disappeared' in error, got: %v", err)
	}
}

func TestWaitForStorageSync_JobNeverAppears(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var polls atomic.Int32
	done := make(chan struct{})
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		defer close(done)
		qmptest.QMPHandshake(conn)
		decoder := json.NewDecoder(conn)
		for {
			var cmd recordedQMPCommand
			if err := decoder.Decode(&cmd); err != nil {
				return
			}
			if cmd.Execute != "query-block-jobs" {
				t.Errorf("execute = %q, want query-block-jobs", cmd.Execute)
				return
			}
			if polls.Add(1) == 2 {
				cancel()
			}
			if _, err := conn.Write([]byte(`{"return":[]}` + "\n")); err != nil {
				return
			}
		}
	})

	client, err := qmp.NewClient(context.Background(), sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("QMP server did not exit after client closed")
		}
	}()

	err = waitForStorageSync(ctx, client, "mirror-drive0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForStorageSync error = %v, want context canceled", err)
	}
	if got := polls.Load(); got != 2 {
		t.Fatalf("block job queries = %d, want 2 before cancellation", got)
	}
}

func TestWaitForStorageSync_JobFailed(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		qmptest.ConsumeCommand(conn)
		jobs := []qmp.BlockJobInfo{{Device: "mirror-drive0", Len: 1000, Offset: 0, Status: "concluded", Type: "mirror"}}
		b, _ := json.Marshal(jobs)
		conn.Write([]byte(`{"return":` + string(b) + "}\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForStorageSync(ctx, client, "mirror-drive0")
	if err == nil {
		t.Fatal("expected error for concluded job")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected 'failed' in error, got: %v", err)
	}
}

func TestWaitForStorageSync_ReadyTerminalJob(t *testing.T) {
	t.Parallel()
	for _, status := range []qmp.BlockJobStatus{qmp.BlockJobStatusConcluded, qmp.BlockJobStatusNull} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			jobs := []qmp.BlockJobInfo{{
				Device: "mirror-drive0",
				Len:    1000,
				Offset: 1000,
				Ready:  true,
				Status: status,
				Type:   "mirror",
			}}
			response, err := json.Marshal(jobs)
			if err != nil {
				t.Fatal(err)
			}
			sock := qmptest.StartScriptedQMP(t, map[string][]string{
				`"query-block-jobs"`: {`{"return":` + string(response) + `}`},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := qmp.NewClient(ctx, sock)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			defer client.Close()

			want := fmt.Sprintf("block mirror job %q failed (status=%s)", jobs[0].Device, status)
			if err := waitForStorageSync(ctx, client, jobs[0].Device); err == nil || err.Error() != want {
				t.Fatalf("waitForStorageSync = %v, want %q", err, want)
			}
		})
	}
}

func TestWaitForStorageSync_FailureOrder(t *testing.T) {
	t.Parallel()
	orders := [][]string{
		{"mirror-drive0", "mirror-drive1"},
		{"mirror-drive1", "mirror-drive0"},
	}
	for inputOrder, jobIDs := range orders {
		for responseOrder, responseIDs := range orders {
			t.Run(fmt.Sprintf("%d/%d", inputOrder, responseOrder), func(t *testing.T) {
				t.Parallel()
				jobs := []qmp.BlockJobInfo{
					{Device: responseIDs[0], Status: qmp.BlockJobStatusConcluded, Type: "mirror"},
					{Device: responseIDs[1], Status: qmp.BlockJobStatusConcluded, Type: "mirror"},
				}
				response, err := json.Marshal(jobs)
				if err != nil {
					t.Fatal(err)
				}
				sock := qmptest.StartScriptedQMP(t, map[string][]string{
					`"query-block-jobs"`: {`{"return":` + string(response) + `}`},
				})
				ctx := context.Background()
				client, err := qmp.NewClient(ctx, sock)
				if err != nil {
					t.Fatalf("NewClient: %v", err)
				}
				defer client.Close()

				want := fmt.Sprintf("block mirror job %q failed (status=concluded)", jobIDs[0])
				const replays = 64
				for replay := range replays {
					err := waitForStorageSync(ctx, client, jobIDs...)
					if err == nil || err.Error() != want {
						t.Fatalf("replay %d: waitForStorageSync = %v, want %q", replay, err, want)
					}
				}
			})
		}
	}
}

func TestWaitForStorageSync_ReadyJobDisappears(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		qmptest.ConsumeCommand(conn)
		jobs := []qmp.BlockJobInfo{
			{Device: "mirror-drive0", Len: 1000, Offset: 1000, Ready: true, Status: "running", Type: "mirror"},
			{Device: "mirror-drive1", Len: 1000, Offset: 500, Ready: false, Status: "running", Type: "mirror"},
		}
		b, _ := json.Marshal(jobs)
		conn.Write([]byte(`{"return":` + string(b) + "}\n"))
		qmptest.ConsumeCommand(conn)
		jobs[1].Ready = true
		jobs[1].Offset = jobs[1].Len
		b, _ = json.Marshal(jobs[1:])
		conn.Write([]byte(`{"return":` + string(b) + "}\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForStorageSync(ctx, client, "mirror-drive0", "mirror-drive1")
	if err == nil {
		t.Fatal("expected error when a ready job disappears")
	}
	if !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("expected 'disappeared' in error, got: %v", err)
	}
}

func TestWaitForMigrationComplete_Completed(t *testing.T) {
	t.Parallel()
	var polls atomic.Int32
	sock, rec := startRecordingQMP(t, func(_ net.Conn, cmd recordedQMPCommand) string {
		if cmd.Execute != "query-migrate" {
			t.Errorf("execute = %q, want query-migrate", cmd.Execute)
			return `{"error":{"class":"CommandNotFound","desc":"unexpected command"}}`
		}
		if polls.Add(1) == 1 {
			return `{"return":{"status":"active","ram":{"total":1000,"transferred":500,"remaining":500}}}`
		}
		return `{"return":{"status":"completed"}}`
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	_, err = waitForMigrationComplete(ctx, client)
	if err != nil {
		t.Fatalf("waitForMigrationComplete: %v", err)
	}
	if got := len(rec.Commands()); got != 2 {
		t.Fatalf("migration queries = %d, want 2 (active then completed)", got)
	}
}

func TestWaitForMigrationComplete_Failed(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		qmptest.ConsumeCommand(conn)
		conn.Write([]byte(`{"return":{"status":"failed","error-desc":"out of memory"}}` + "\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	_, err = waitForMigrationComplete(ctx, client)
	if err == nil {
		t.Fatal("expected error for failed migration")
	}
	if !errors.Is(err, errMigrationFailed) {
		t.Fatalf("expected errMigrationFailed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("expected error description in message, got: %v", err)
	}
}

func TestWaitForMigrationComplete_Cancelled(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		qmptest.ConsumeCommand(conn)
		conn.Write([]byte(`{"return":{"status":"cancelled"}}` + "\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	_, err = waitForMigrationComplete(ctx, client)
	if err == nil {
		t.Fatal("expected error for cancelled migration")
	}
	if !errors.Is(err, errMigrationCancelled) {
		t.Fatalf("expected errMigrationCancelled, got: %v", err)
	}
}

// postActiveStallGraceForTest overrides postActiveStallGrace for the
// caller test and returns a restore func suitable for t.Cleanup.
func postActiveStallGraceForTest(d time.Duration) func() {
	prev := postActiveStallGrace
	postActiveStallGrace = d
	return func() { postActiveStallGrace = prev }
}

// TestWaitForMigrationComplete_QMPStallTreatedAsSuccess locks the
// post-pause hand-off contract: when query-migrate fails repeatedly
// after kata-shim tears down the source QEMU, the source binary must
// declare success rather than fail. Regression test for the T2 e2e
// failure where source returned "QMP read i/o timeout" although the
// dest had already resumed the VM. The stall grace fires immediately
// because the function is only entered post-pause.
func TestWaitForMigrationComplete_QMPStallTreatedAsSuccess(t *testing.T) {
	// Not parallel: shrinks the package-level postActiveStallGrace while
	// running, and sibling parallel tests call waitForMigrationComplete,
	// which reads it.
	t.Cleanup(postActiveStallGraceForTest(200 * time.Millisecond))

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// Accept the first query-migrate command, never reply, simulating
		// kata-shim tearing down source QEMU after handover so the QMP
		// socket stops responding.
		qmptest.ConsumeCommand(conn)
		io.Copy(io.Discard, conn)
	})

	// Need ≥ queryMigrateTimeout (5s) + pollInterval (1s) + grace
	// (200ms) for two query-migrate attempts, since the first one's
	// stall sets firstStallAt and the second one trips the grace check.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	info, err := waitForMigrationComplete(ctx, client)
	if err != nil {
		t.Fatalf("waitForMigrationComplete should treat sustained QMP stall as success after grace; got %v", err)
	}
	if info.Status != "" {
		t.Fatalf("stalled QMP returned metrics: %+v", info)
	}
}

func TestWaitForMigrationComplete_ContextCancelled(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// Block until client disconnects, never responding.
		io.Copy(io.Discard, conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	_, err = waitForMigrationComplete(ctx, client)
	if err == nil {
		t.Fatal("expected error on context cancellation")
	}
}

func TestWaitForMigrationComplete_FailedNoDesc(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		qmptest.ConsumeCommand(conn)
		conn.Write([]byte(`{"return":{"status":"failed"}}` + "\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	_, err = waitForMigrationComplete(ctx, client)
	if err == nil {
		t.Fatal("expected error for failed migration")
	}
	if !errors.Is(err, errMigrationFailed) {
		t.Fatalf("expected errMigrationFailed, got: %v", err)
	}
}

func TestRunSource_SharedStorage_HappyPath(t *testing.T) {
	var polls atomic.Int32
	sock, _ := startRecordingQMP(t, func(_ net.Conn, cmd recordedQMPCommand) string {
		switch cmd.Execute {
		case "migrate":
			return "{\"return\":{}}\n{\"event\":\"STOP\"}"
		case "query-migrate":
			polls.Add(1)
			return `{"return":{"status":"completed","downtime":15,"total-time":1200,"setup-time":50,"ram":{"transferred":1000,"total":1000}}}`
		default:
			return `{"return":{}}`
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := captureStdout(t, func() {
		err := RunSource(ctx, SourceConfig{
			QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
			SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
		})
		if err != nil {
			t.Fatalf("RunSource shared-storage happy path: %v", err)
		}
	})
	if got := polls.Load(); got != 1 {
		t.Fatalf("migration queries = %d, want 1", got)
	}
	if want := ResultMarker + "downtime_ms=15 total_time_ms=1200 ram_transferred=1000 ram_total=1000\n"; !strings.Contains(out, want) {
		t.Fatalf("output = %q, want result %q", out, want)
	}
}

func TestRunSource_SharedStorage_MigrationFailed(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		`"migrate"`:       {`{"return":{}}`, `{"event":"STOP"}`},
		`"query-migrate"`: {`{"return":{"status":"failed","error-desc":"test failure"}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error for failed migration")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected 'failed' in error, got: %v", err)
	}
}

// TestRunSource_AbandonedMigrationCancelledOnStopWaitError pins the deferred
// migrate-cancel safety net: when the source gives up between starting the
// RAM migration and waiting out its completion (here the STOP-event stream
// delivers an unparsable line), the in-flight migration must be cancelled
// via QMP instead of streaming on unowned inside the source QEMU.
func TestRunSource_AbandonedMigrationCancelledOnStopWaitError(t *testing.T) {
	t.Parallel()

	sock, rec := startRecordingQMP(t, func(conn net.Conn, cmd recordedQMPCommand) string {
		switch cmd.Execute {
		case "migrate":
			// Command response followed by a malformed event line: the
			// STOP wait fails with an unmarshaling error while the socket
			// stays alive, so the deferred cleanup can still reach QEMU.
			return `{"return":{}}` + "\n" + `{"event":broken`
		default:
			return `{"return":{}}`
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error when STOP-event stream is unparsable")
	}
	if !strings.Contains(err.Error(), "unexpected error waiting for STOP event") {
		t.Fatalf("expected STOP-wait failure, got: %v", err)
	}
	assertRecordedSubsequence(t, rec.Commands(), []string{"migrate", "migrate-cancel"})
}

// TestRunSource_SuccessIssuesNoMigrateCancel pins the disarm side of the
// deferred safety net: a completed migration must not be followed by a
// redundant migrate-cancel command.
func TestRunSource_SuccessIssuesNoMigrateCancel(t *testing.T) {
	t.Parallel()

	sock, rec := startRecordingQMP(t, func(conn net.Conn, cmd recordedQMPCommand) string {
		switch cmd.Execute {
		case "migrate":
			return `{"return":{}}` + "\n" + `{"event":"STOP"}`
		case "query-migrate":
			return `{"return":{"status":"completed","downtime":10,"total-time":800,"setup-time":30}}`
		default:
			return `{"return":{}}`
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err != nil {
		t.Fatalf("RunSource happy path: %v", err)
	}
	for _, cmd := range rec.Commands() {
		if cmd.Execute == "migrate-cancel" {
			t.Fatalf("migrate-cancel issued on success path; got %v", recordedCommandNames(rec.Commands()))
		}
	}
}

func TestRunSource_NonShared_HappyPath(t *testing.T) {
	t.Parallel()
	callCount := 0

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "query-block-jobs") {
				callCount++
				if callCount <= 1 {
					conn.Write([]byte(`{"return":[{"device":"mirror-drive-virtio-disk0","len":1000,"offset":500,"ready":false,"status":"running","type":"mirror"}]}` + "\n"))
				} else {
					conn.Write([]byte(`{"return":[{"device":"mirror-drive-virtio-disk0","len":1000,"offset":1000,"ready":true,"status":"running","type":"mirror"}]}` + "\n"))
				}
				continue
			}
			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				conn.Write([]byte(`{"event":"STOP"}` + "\n"))
				continue
			}
			if strings.Contains(line, "query-migrate") {
				conn.Write([]byte(`{"return":{"status":"completed","downtime":10,"total-time":800,"setup-time":30}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err != nil {
		t.Fatalf("RunSource non-shared happy path: %v", err)
	}
}

func TestRunSource_NonShared_CommandArguments(t *testing.T) {
	t.Parallel()

	sock, rec := startRecordingQMP(t, func(conn net.Conn, cmd recordedQMPCommand) string {
		switch cmd.Execute {
		case "query-block-jobs":
			return `{"return":[{"device":"mirror-drive-virtio-disk0","len":1000,"offset":1000,"ready":true,"status":"running","type":"mirror"}]}`
		case "migrate":
			return `{"return":{}}` + "\n" + `{"event":"STOP"}`
		case "query-migrate":
			return `{"return":{"status":"completed","downtime":10,"total-time":800,"setup-time":30}}`
		default:
			return `{"return":{}}`
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket:       sock,
		DestIP:          testDestIP,
		VMIP:            testVMIP,
		DriveIDs:        []string{"drive-virtio-disk0"},
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
		MultifdChannels: 4,
	})
	if err != nil {
		t.Fatalf("RunSource non-shared command arguments: %v", err)
	}

	commands := rec.Commands()
	assertRecordedSubsequence(t, commands, []string{
		"drive-mirror",
		"query-block-jobs",
		"migrate-set-capabilities",
		"migrate-set-parameters",
		"migrate",
		"query-migrate",
		"block-job-cancel",
	})

	var mirror qmp.DriveMirrorArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "drive-mirror"), &mirror)
	if mirror.Device != "drive-virtio-disk0" {
		t.Fatalf("drive-mirror device = %q, want drive-virtio-disk0", mirror.Device)
	}
	if mirror.Target != "nbd:10.0.0.1:10809:exportname=drive-virtio-disk0" {
		t.Fatalf("drive-mirror target = %q", mirror.Target)
	}
	if mirror.Sync != "full" || mirror.Mode != "existing" || mirror.JobID != "mirror-drive-virtio-disk0" {
		t.Fatalf("unexpected drive-mirror args: %+v", mirror)
	}

	var caps qmp.MigrateSetCapabilitiesArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "migrate-set-capabilities"), &caps)
	if len(caps.Capabilities) != 2 ||
		caps.Capabilities[0] != (qmp.MigrationCapability{Capability: "auto-converge", State: true}) ||
		caps.Capabilities[1] != (qmp.MigrationCapability{Capability: "multifd", State: true}) {
		t.Fatalf("unexpected migration capabilities: %+v", caps.Capabilities)
	}

	var params qmp.MigrateSetParametersArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "migrate-set-parameters"), &params)
	if params.DowntimeLimit != 25 || params.MaxBandwidth != maxBandwidth || params.MultifdChannels != 4 {
		t.Fatalf("unexpected migration parameters: %+v", params)
	}

	var migrate qmp.MigrateArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "migrate"), &migrate)
	if migrate.URI != "tcp:10.0.0.1:4444" {
		t.Fatalf("migrate URI = %q, want tcp:10.0.0.1:4444", migrate.URI)
	}

	var cancel qmp.BlockJobCancelArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "block-job-cancel"), &cancel)
	if cancel.Device != "mirror-drive-virtio-disk0" || !cancel.Force {
		t.Fatalf("unexpected block-job-cancel args: %+v", cancel)
	}
}

func TestRunSource_MigrationFailedDuringPolling(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		// While waiting for STOP event, query-migrate returns failed status.
		`"query-migrate"`: {`{"return":{"status":"failed","error-desc":"RAM migration failed"}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error when migration fails during STOP polling")
	}
	if !errors.Is(err, errMigrationFailed) {
		t.Fatalf("expected errMigrationFailed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "RAM migration failed") {
		t.Fatalf("expected error description, got: %v", err)
	}
}

func TestRunSource_MigrationCancelledDuringPolling(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		`"query-migrate"`: {`{"return":{"status":"cancelled"}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error when migration cancelled during STOP polling")
	}
	if !errors.Is(err, errMigrationCancelled) {
		t.Fatalf("expected errMigrationCancelled, got: %v", err)
	}
}

func TestRunSource_CompletedDuringPolling(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		// Report completed without STOP event (triggered via polling).
		`"query-migrate"`: {`{"return":{"status":"completed","downtime":5,"total-time":500,"setup-time":20}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err != nil {
		t.Fatalf("RunSource completed-during-polling: %v", err)
	}
}

func TestRunSource_SharedStorage_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		`"migrate"`:       {`{"return":{}}`, `{"event":"STOP"}`},
		`"query-migrate"`: {`{"return":{"status":"completed","downtime":10,"total-time":500,"setup-time":20}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25, MultifdChannels: 4,
	})
	if err != nil {
		t.Fatalf("RunSource with multifd: %v", err)
	}
}

func TestMigrationTerminalError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    qmp.MigrateStatus
		errorDesc string
		terminal  bool
		wantErr   error
		wantDesc  string
	}{
		{"completed", qmp.MigrateStatusCompleted, "", true, nil, ""},
		{"failed_with_desc", qmp.MigrateStatusFailed, "out of memory", true, errMigrationFailed, "out of memory"},
		{"failed_no_desc", qmp.MigrateStatusFailed, "", true, errMigrationFailed, ""},
		{"cancelled", qmp.MigrateStatusCancelled, "", true, errMigrationCancelled, ""},
		{"active", "active", "", false, nil, ""},
		{"setup", "setup", "", false, nil, ""},
		{"empty", "", "", false, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			terminal, err := migrationTerminalError(tt.status, tt.errorDesc)
			if terminal != tt.terminal {
				t.Fatalf("terminal: got %v, want %v", terminal, tt.terminal)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("expected nil error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got: %v", tt.wantErr, err)
				}
				if tt.wantDesc != "" && !strings.Contains(err.Error(), tt.wantDesc) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantDesc, err)
				}
			}
		})
	}
}

func TestMeasureRTT(t *testing.T) {
	// measureRTT uses raw ICMP which needs CAP_NET_RAW (or
	// net.ipv4.ping_group_range covering this uid). Skip when we can't
	// open the listener instead of failing: production source pods run
	// privileged and always have the capability.
	if c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err != nil {
		t.Skipf("ICMP socket not available in this test env: %v", err)
	} else {
		_ = c.Close()
	}
	rtt, err := measureRTT(netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatalf("measureRTT: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("measureRTT returned non-positive duration: %v", rtt)
	}
}

func TestRunSource_DriveMirrorFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"drive-mirror": {`{"error":{"class":"GenericError","desc":"device not found"}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error for drive-mirror failure")
	}
	if !strings.Contains(err.Error(), "drive-mirror") {
		t.Fatalf("expected 'drive-mirror' in error, got: %v", err)
	}
}

func TestRunSource_PartialDriveMirrorFailureCleansUp(t *testing.T) {
	t.Parallel()

	sock, rec := startRecordingQMP(t, func(_ net.Conn, cmd recordedQMPCommand) string {
		if cmd.Execute == "drive-mirror" && strings.Contains(string(cmd.Arguments), "disk2") {
			return `{"error":{"class":"GenericError","desc":"device not found"}}`
		}
		return `{"return":{}}`
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunSource(ctx, SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP,
		DriveIDs:   []string{"disk0", "disk1", "disk2"},
		TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	var qmpErr *qmp.Error
	if !errors.As(err, &qmpErr) || qmpErr.Desc != "device not found" {
		t.Fatalf("expected original drive-mirror error, got: %v", err)
	}
	commands := rec.Commands()
	var cancelled []string
	for _, cmd := range commands {
		if cmd.Execute == "block-job-cancel" {
			var args qmp.BlockJobCancelArgs
			decodeRecordedArgs(t, cmd, &args)
			if !args.Force {
				t.Error("cleanup must force block job cancellation")
			}
			cancelled = append(cancelled, args.Device)
		}
		if cmd.Execute == "query-block-jobs" || cmd.Execute == "migrate" {
			t.Errorf("unexpected command after partial mirror failure: %s", cmd.Execute)
		}
	}
	if strings.Join(cancelled, ",") != "mirror-disk0,mirror-disk1" {
		t.Fatalf("cancelled jobs = %v, want both successfully started mirrors", cancelled)
	}
}

func TestRunSource_SetCapabilitiesFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-set-capabilities": {`{"error":{"class":"GenericError","desc":"caps error"}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error for capabilities failure")
	}
	if !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("expected 'capabilities' in error, got: %v", err)
	}
}

func TestRunSource_SetParametersFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-set-parameters": {`{"error":{"class":"GenericError","desc":"params error"}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error for parameters failure")
	}
	if !strings.Contains(err.Error(), "parameters") {
		t.Fatalf("expected 'parameters' in error, got: %v", err)
	}
}

func TestRunSource_MigrateCommandFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		`"migrate"`: {`{"error":{"class":"GenericError","desc":"migrate failed"}}`},
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error for migrate command failure")
	}
	if !strings.Contains(err.Error(), "RAM migration") {
		t.Fatalf("expected 'RAM migration' in error, got: %v", err)
	}
}

func TestRunSource_InvalidDriveID(t *testing.T) {
	t.Parallel()
	err := RunSource(context.Background(), SourceConfig{
		QMPSocket:       "/nonexistent/qmp.sock",
		DestIP:          testDestIP,
		VMIP:            testVMIP,
		DriveIDs:        []string{";evil"},
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid drive ID") {
		t.Fatalf("expected drive ID validation error, got: %v", err)
	}
}

func TestRunSource_SharedStorage_SkipsDriveIDValidation(t *testing.T) {
	t.Parallel()
	err := RunSource(context.Background(), SourceConfig{
		QMPSocket:       "/nonexistent/qmp.sock",
		DestIP:          testDestIP,
		VMIP:            testVMIP,
		DriveIDs:        []string{";evil"},
		SharedStorage:   true,
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error (QMP connection should fail)")
	}
	if strings.Contains(err.Error(), "invalid drive ID") {
		t.Fatalf("shared storage should skip drive ID validation, got: %v", err)
	}
}

func TestRunSource_AutoDowntime_FractionalRTT(t *testing.T) {
	for _, tt := range []struct {
		name    string
		rtt     time.Duration
		floorMS int
		wantMS  int64
	}{
		{name: "zero", wantMS: 25},
		{name: "sub millisecond", rtt: 500 * time.Microsecond, wantMS: 26},
		{name: "whole millisecond", rtt: time.Millisecond, wantMS: 27},
		{name: "fractional millisecond", rtt: 1500 * time.Microsecond, wantMS: 28},
		{name: "round budget up", rtt: 1500*time.Microsecond + time.Nanosecond, wantMS: 29},
		{name: "custom floor", rtt: 1500 * time.Microsecond, floorMS: 40, wantMS: 43},
	} {
		t.Run(tt.name, func(t *testing.T) {
			origMeasureRTT := measureRTTFunc
			measureRTTFunc = func(netip.Addr) (time.Duration, error) {
				return tt.rtt, nil
			}
			t.Cleanup(func() { measureRTTFunc = origMeasureRTT })

			sock, rec := startRecordingQMP(t, func(conn net.Conn, cmd recordedQMPCommand) string {
				switch cmd.Execute {
				case "migrate":
					return `{"return":{}}` + "\n" + `{"event":"STOP"}`
				case "query-migrate":
					return `{"return":{"status":"completed"}}`
				default:
					return `{"return":{}}`
				}
			})

			err := RunSource(context.Background(), SourceConfig{
				QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP,
				SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
				AutoDowntime: true, AutoDowntimeFloorMS: tt.floorMS,
			})
			if err != nil {
				t.Fatalf("RunSource: %v", err)
			}
			var params qmp.MigrateSetParametersArgs
			decodeRecordedArgs(t, findRecordedCommand(t, rec.Commands(), "migrate-set-parameters"), &params)
			if params.DowntimeLimit != tt.wantMS {
				t.Fatalf("downtime limit = %dms, want %dms", params.DowntimeLimit, tt.wantMS)
			}
		})
	}
}

func TestRunSource_AutoDowntime_Fallback(t *testing.T) {
	var rttCalls atomic.Int32
	origMeasureRTT := measureRTTFunc
	measureRTTFunc = func(netip.Addr) (time.Duration, error) {
		rttCalls.Add(1)
		return 0, errors.New("forced RTT failure")
	}
	t.Cleanup(func() {
		measureRTTFunc = origMeasureRTT
	})

	sock, rec := startRecordingQMP(t, func(conn net.Conn, cmd recordedQMPCommand) string {
		switch cmd.Execute {
		case "migrate":
			return `{"return":{}}` + "\n" + `{"event":"STOP"}`
		case "query-migrate":
			return `{"return":{"status":"completed","downtime":10,"total-time":500,"setup-time":20}}`
		default:
			return `{"return":{}}`
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25, AutoDowntime: true,
	})
	if err != nil {
		t.Fatalf("RunSource with auto-downtime fallback: %v", err)
	}

	// Prove the AutoDowntime branch actually ran: without this, the test
	// would silently pass if the branch were skipped (since DowntimeLimitMS=25
	// matches both the fallback value and the non-AutoDowntime default).
	if got := rttCalls.Load(); got != 1 {
		t.Fatalf("measureRTTFunc was called %d times, want 1 (AutoDowntime branch must execute)", got)
	}

	// Verify the fallback path used the explicit DowntimeLimitMS rather than
	// silently using 0 or some other value when measureRTT failed.
	var params qmp.MigrateSetParametersArgs
	decodeRecordedArgs(t, findRecordedCommand(t, rec.Commands(), "migrate-set-parameters"), &params)
	if params.DowntimeLimit != 25 {
		t.Fatalf("auto-downtime fallback should use explicit DowntimeLimitMS=25, got %d", params.DowntimeLimit)
	}
}

func TestRunSource_ContextCancelled(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// Block until client disconnects, forcing context cancellation.
		io.Copy(io.Discard, conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := RunSource(ctx, SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveIDs: []string{"drive-virtio-disk0"},
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// fakeProcFS is a procFS stub for tests: it pretends each known sandbox UUID
// maps to a fixed PID and that exactly one sandbox's netns contains the
// configured pod IP. Anything else returns the zero result.
type fakeProcFS struct {
	pids       map[string]int   // sandbox UUID -> PID
	netnsByPID map[int][]string // PID -> IPs that exist in that netns
	pidErr     error            // when set, PIDsForSandboxes resolves nothing
	netnsErr   error            // forced error from NetnsHasIP
}

func (f fakeProcFS) PIDsForSandboxes(uuids []string) map[string]int {
	if f.pidErr != nil {
		return nil
	}
	out := make(map[string]int, len(uuids))
	for _, uuid := range uuids {
		if pid, ok := f.pids[uuid]; ok {
			out[uuid] = pid
		}
	}
	return out
}

func (f fakeProcFS) NetnsHasIP(pid int, ip string) (bool, error) {
	if f.netnsErr != nil {
		return false, f.netnsErr
	}
	for _, candidate := range f.netnsByPID[pid] {
		if candidate == ip {
			return true, nil
		}
	}
	return false, nil
}

// TestRunSource_PodResolver_PopulatesConfig verifies that when SourceConfig
// has PodName set, RunSource resolves VMIP and QMPSocket from the pod
// before any other validation runs. We prove the resolver ran by making it
// populate VMIP with an IPv6 address (mismatching the IPv4 DestIP) and
// asserting that the family-mismatch validator fires with that resolved IP.
func TestRunSource_PodResolver_PopulatesConfig(t *testing.T) {
	const (
		sandboxUUID = "11111111-2222-3333-4444-555555555555"
		fakeQEMUPID = 4242
		resolvedIP  = "fd00::1" // IPv6, guaranteed to mismatch testDestIP (IPv4)
	)

	// Stub the apiserver lookup so the resolver returns a known IP.
	origLookup := lookupPodIP
	lookupPodIP = func(_ context.Context, ns, name string) (string, error) {
		if ns != "default" || name != "vm-a" {
			return "", fmt.Errorf("unexpected lookup args: ns=%q name=%q", ns, name)
		}
		return resolvedIP, nil
	}
	t.Cleanup(func() { lookupPodIP = origLookup })

	// Stub the procFS so the sandbox-by-IP resolver returns our fake sandbox.
	origProc := procImpl
	procImpl = fakeProcFS{
		pids:       map[string]int{sandboxUUID: fakeQEMUPID},
		netnsByPID: map[int][]string{fakeQEMUPID: {resolvedIP}},
	}
	t.Cleanup(func() { procImpl = origProc })

	// resolveSandbox os.ReadDirs sandboxRoot for sandbox UUIDs; create one.
	tmpRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpRoot, sandboxUUID), 0o755); err != nil {
		t.Fatalf("setup tmp sandbox dir: %v", err)
	}
	origRoot := sandboxRoot
	sandboxRoot = tmpRoot
	t.Cleanup(func() { sandboxRoot = origRoot })

	err := RunSource(context.Background(), SourceConfig{
		PodName:         "vm-a",
		PodNamespace:    "default",
		DestIP:          testDestIP, // IPv4
		DriveIDs:        []string{"drive-virtio-disk0"},
		SharedStorage:   true,
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
		// VMIP and QMPSocket intentionally left zero: they must be populated by resolver.
	})
	if err == nil {
		t.Fatal("expected family-mismatch error, got nil (resolver may not have run)")
	}
	// The resolver must have populated VMIP with the IPv6 address; the family
	// validator must then have fired with both addresses interpolated.
	if !strings.Contains(err.Error(), "address families must match") {
		t.Fatalf("expected family-mismatch error proving resolver ran, got: %v", err)
	}
	if !strings.Contains(err.Error(), resolvedIP) {
		t.Fatalf("expected resolved IP %q in error (proves resolver populated VMIP), got: %v", resolvedIP, err)
	}
}

// TestRunSource_EmitCmdline_RemovesFileAtExit pins the lifecycle of the
// captured cmdline file: it exists while the source runs (consumed by the
// KATAMARAN_CMDLINE_B64 marker emission and deploy/migrate.sh's mid-run
// kubectl cp) but must be removed when RunSource exits, so repeated
// migrations don't accumulate stale cmdline files on the node-wide
// /tmp/katamaran-cmdlines hostPath.
func TestRunSource_EmitCmdline_RemovesFileAtExit(t *testing.T) {
	const (
		sandboxUUID = "11111111-2222-3333-4444-555555555555"
		resolvedIP  = "10.244.1.15" // same family as testDestIP so validation passes
	)

	origLookup := lookupPodIP
	lookupPodIP = func(_ context.Context, _, _ string) (string, error) {
		return resolvedIP, nil
	}
	t.Cleanup(func() { lookupPodIP = origLookup })

	// PIDForSandbox returns this process's PID so captureSourceCmdline can
	// read a real /proc/<pid>/cmdline without special privileges.
	origProc := procImpl
	procImpl = fakeProcFS{
		pids:       map[string]int{sandboxUUID: os.Getpid()},
		netnsByPID: map[int][]string{os.Getpid(): {resolvedIP}},
	}
	t.Cleanup(func() { procImpl = origProc })

	tmpRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpRoot, sandboxUUID), 0o755); err != nil {
		t.Fatalf("setup tmp sandbox dir: %v", err)
	}
	origRoot := sandboxRoot
	sandboxRoot = tmpRoot
	t.Cleanup(func() { sandboxRoot = origRoot })

	cmdlinePath := filepath.Join(t.TempDir(), "cmdline-test.txt")
	// Replay mode sleeps for destReplaySleep before dialing QMP; collapse it
	// so this lifecycle pin doesn't cost a minute of wall clock.
	origSleep := destReplaySleep
	destReplaySleep = 10 * time.Millisecond
	t.Cleanup(func() { destReplaySleep = origSleep })
	wantCmdline, err := os.ReadFile("/proc/self/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	observed := make(chan []byte, 1)
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		captured, readErr := os.ReadFile(cmdlinePath)
		if readErr != nil {
			t.Errorf("read captured cmdline before QMP failure: %v", readErr)
		}
		observed <- captured
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = RunSource(ctx, SourceConfig{
		PodName:         "vm-a",
		PodNamespace:    "default",
		DestIP:          testDestIP,
		QMPSocket:       sock,
		DriveIDs:        []string{"drive-virtio-disk0"},
		SharedStorage:   true,
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
		EmitCmdlineTo:   cmdlinePath,
	})
	if err == nil || !strings.Contains(err.Error(), "connecting to source QMP") || !errors.Is(err, io.EOF) {
		t.Fatalf("expected QMP greeting EOF after capture, got: %v", err)
	}
	select {
	case captured := <-observed:
		if len(captured) == 0 || !bytes.Equal(captured, wantCmdline) {
			t.Fatal("captured file must contain the source cmdline before QMP failure")
		}
	default:
		t.Fatal("source never connected to QMP after capturing cmdline")
	}
	if _, statErr := os.Stat(cmdlinePath); !os.IsNotExist(statErr) {
		t.Fatalf("captured cmdline file %s still exists after RunSource returned (leaks one file per migration on the shared hostPath)", cmdlinePath)
	}
}

// TestRunSource_PodResolver_LookupError ensures lookup failures surface as a
// clear "lookup pod IP" error rather than a downstream validation failure.
func TestRunSource_PodResolver_LookupError(t *testing.T) {
	origLookup := lookupPodIP
	lookupPodIP = func(_ context.Context, _, _ string) (string, error) {
		return "", errors.New("apiserver unreachable")
	}
	t.Cleanup(func() { lookupPodIP = origLookup })

	err := RunSource(context.Background(), SourceConfig{
		PodName:         "vm-a",
		PodNamespace:    "default",
		DestIP:          testDestIP,
		DriveIDs:        []string{"drive-virtio-disk0"},
		SharedStorage:   true,
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
	})
	if err == nil || !strings.Contains(err.Error(), "lookup pod IP") {
		t.Fatalf("expected 'lookup pod IP' error, got: %v", err)
	}
}
