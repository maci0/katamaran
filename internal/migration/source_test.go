package migration

import (
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
	"testing"
	"time"

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
				DriveID:         "drive-virtio-disk0",
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
		DriveID:         "drive-virtio-disk0",
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
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// First poll: job running at 50%.
		qmptest.ConsumeCommand(conn)
		jobs := []qmp.BlockJobInfo{{
			Device: "mirror-drive0",
			Len:    1000,
			Offset: 500,
			Ready:  false,
			Status: "running",
			Type:   "mirror",
		}}
		b, _ := json.Marshal(jobs)
		conn.Write([]byte(`{"return":` + string(b) + "}\n"))
		// Second poll: job ready.
		qmptest.ConsumeCommand(conn)
		jobs[0].Ready = true
		jobs[0].Offset = 1000
		b, _ = json.Marshal(jobs)
		conn.Write([]byte(`{"return":` + string(b) + "}\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForStorageSync(ctx, client, "mirror-drive0")
	if err != nil {
		t.Fatalf("waitForStorageSync: %v", err)
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

	// Override jobAppearTimeout for faster test. We can't do that without
	// changing the constant, so just verify context cancellation works.
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		for {
			qmptest.ConsumeCommand(conn)
			conn.Write([]byte(`{"return":[]}` + "\n"))
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForStorageSync(ctx, client, "mirror-drive0")
	if err == nil {
		t.Fatal("expected error when context is cancelled")
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

func TestWaitForMigrationComplete_Completed(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// First poll: active.
		qmptest.ConsumeCommand(conn)
		conn.Write([]byte(`{"return":{"status":"active","ram":{"total":1000,"transferred":500,"remaining":500}}}` + "\n"))
		// Second poll: completed.
		qmptest.ConsumeCommand(conn)
		conn.Write([]byte(`{"return":{"status":"completed"}}` + "\n"))
	})

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForMigrationComplete(ctx, client)
	if err != nil {
		t.Fatalf("waitForMigrationComplete: %v", err)
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

	err = waitForMigrationComplete(ctx, client)
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

	err = waitForMigrationComplete(ctx, client)
	if err == nil {
		t.Fatal("expected error for cancelled migration")
	}
	if !errors.Is(err, errMigrationCancelled) {
		t.Fatalf("expected errMigrationCancelled, got: %v", err)
	}
}

func TestWaitForMigrationComplete_ContextCancelled(t *testing.T) {
	t.Parallel()
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// Block until client disconnects — never respond.
		io.Copy(io.Discard, conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client, err := qmp.NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = waitForMigrationComplete(ctx, client)
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

	err = waitForMigrationComplete(ctx, client)
	if err == nil {
		t.Fatal("expected error for failed migration")
	}
	if !errors.Is(err, errMigrationFailed) {
		t.Fatalf("expected errMigrationFailed, got: %v", err)
	}
}

func TestRunSource_SharedStorage_HappyPath(t *testing.T) {
	t.Parallel()

	handlers := map[string]string{
		"migrate-set-capabilities": `{"return":{}}`,
		"migrate-set-parameters":   `{"return":{}}`,
		"query-migrate":            `{"return":{"status":"completed","downtime":15,"total-time":1200,"setup-time":50}}`,
	}

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			// After "migrate" command, send response then inject STOP event.
			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				// Give the client a moment then send STOP event.
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"STOP"}` + "\n"))
				continue
			}

			responded := false
			for cmd, resp := range handlers {
				if strings.Contains(line, `"`+cmd+`"`) {
					conn.Write([]byte(resp + "\n"))
					responded = true
					break
				}
			}
			if !responded {
				conn.Write([]byte(`{"return":{}}` + "\n"))
			}
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err != nil {
		t.Fatalf("RunSource shared-storage happy path: %v", err)
	}
}

func TestRunSource_SharedStorage_MigrationFailed(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"STOP"}` + "\n"))
				continue
			}
			if strings.Contains(line, "query-migrate") {
				conn.Write([]byte(`{"return":{"status":"failed","error-desc":"test failure"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error for failed migration")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected 'failed' in error, got: %v", err)
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
				time.Sleep(10 * time.Millisecond)
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
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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
		DriveID:         "drive-virtio-disk0",
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

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			// While waiting for STOP event, query-migrate returns failed status.
			if strings.Contains(line, "query-migrate") {
				conn.Write([]byte(`{"return":{"status":"failed","error-desc":"RAM migration failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			if strings.Contains(line, "query-migrate") {
				conn.Write([]byte(`{"return":{"status":"cancelled"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			// Report completed without STOP event (triggered via polling).
			if strings.Contains(line, "query-migrate") {
				conn.Write([]byte(`{"return":{"status":"completed","downtime":5,"total-time":500,"setup-time":20}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err != nil {
		t.Fatalf("RunSource completed-during-polling: %v", err)
	}
}

func TestRunSource_SharedStorage_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"STOP"}` + "\n"))
				continue
			}
			if strings.Contains(line, "query-migrate") {
				conn.Write([]byte(`{"return":{"status":"completed","downtime":10,"total-time":500,"setup-time":20}}` + "\n"))
				continue
			}
			// migrate-set-capabilities, migrate-set-parameters, etc.
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", ramMigrationPort))
	if err != nil {
		t.Skipf("cannot bind RTT test listener on %s: %v", ramMigrationPort, err)
	}
	defer l.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	rtt, err := measureRTT(netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatalf("measureRTT: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("measureRTT returned non-positive duration: %v", rtt)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("measureRTT did not complete all RTT samples")
	}
}

func TestRunSource_DriveMirrorFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "drive-mirror") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"device not found"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
		TunnelMode: TunnelModeNone, DowntimeLimitMS: 25,
	})
	if err == nil {
		t.Fatal("expected error for drive-mirror failure")
	}
	if !strings.Contains(err.Error(), "drive-mirror") {
		t.Fatalf("expected 'drive-mirror' in error, got: %v", err)
	}
}

func TestRunSource_SetCapabilitiesFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-set-capabilities") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"caps error"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-set-parameters") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"params error"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if qmptest.IsMigrateCommand(line) {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"migrate failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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
		DriveID:         ";evil",
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
		DriveID:         ";evil",
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

func TestRunSource_AutoDowntime_Fallback(t *testing.T) {
	origMeasureRTT := measureRTTFunc
	measureRTTFunc = func(netip.Addr) (time.Duration, error) {
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
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25, AutoDowntime: true,
	})
	if err != nil {
		t.Fatalf("RunSource with auto-downtime fallback: %v", err)
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
		// Block until client disconnects — force context cancellation.
		io.Copy(io.Discard, conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := RunSource(ctx, SourceConfig{
		QMPSocket: sock, DestIP: testDestIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
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
	pidErr     error            // forced error from PIDForSandbox
	netnsErr   error            // forced error from NetnsHasIP
}

func (f fakeProcFS) PIDForSandbox(uuid string) (int, error) {
	if f.pidErr != nil {
		return 0, f.pidErr
	}
	pid, ok := f.pids[uuid]
	if !ok {
		return 0, errors.New("no such sandbox")
	}
	return pid, nil
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
		resolvedIP  = "fd00::1" // IPv6 — guaranteed to mismatch testDestIP (IPv4)
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
		DriveID:         "drive-virtio-disk0",
		SharedStorage:   true,
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
		// VMIP and QMPSocket intentionally left zero — must be populated by resolver.
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
		DriveID:         "drive-virtio-disk0",
		SharedStorage:   true,
		TunnelMode:      TunnelModeNone,
		DowntimeLimitMS: 25,
	})
	if err == nil || !strings.Contains(err.Error(), "lookup pod IP") {
		t.Fatalf("expected 'lookup pod IP' error, got: %v", err)
	}
}
