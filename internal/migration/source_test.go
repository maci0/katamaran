package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"runtime"
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
	if errMigrationFailed == nil || !errors.Is(errMigrationFailed, errMigrationFailed) {
		t.Fatal("errMigrationFailed should be valid and matchable")
	}
	if errMigrationCancelled == nil || !errors.Is(errMigrationCancelled, errMigrationCancelled) {
		t.Fatal("errMigrationCancelled should be valid and matchable")
	}
	if errors.Is(errMigrationFailed, errMigrationCancelled) {
		t.Fatal("errMigrationFailed and errMigrationCancelled should be distinct")
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
		// Never respond — force context timeout.
		time.Sleep(30 * time.Second)
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

func TestMeasureRTT(t *testing.T) {
	t.Parallel()

	// measureRTT hardcodes ramMigrationPort, so we can only test the error
	// path with an unreachable address (RFC 5737 TEST-NET).
	_, err := measureRTT(netip.MustParseAddr("192.0.2.1"))
	if err == nil {
		t.Fatal("expected error for unreachable address")
	}
}

func TestRunCmdInNetns_EmptyNetns(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	// With empty netnsPath, should delegate directly to runCmd.
	if err := runCmdInNetns(context.Background(), "", "true"); err != nil {
		t.Fatalf("expected success with empty netns, got: %v", err)
	}
}

func TestRunCmdInNetns_WithNetns(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	// nsenter with /proc/1/ns/net requires root; as non-root this errors.
	// Either outcome is acceptable — the assertion verifies the nsenter code
	// path is taken (error mentions "nsenter") and doesn't panic.
	err := runCmdInNetns(context.Background(), "/proc/1/ns/net", "true")
	if err != nil && !strings.Contains(err.Error(), "nsenter") {
		t.Fatalf("expected nsenter-related error, got: %v", err)
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

func TestRunSource_AutoDowntime_Fallback(t *testing.T) {
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
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	// Use TEST-NET address (RFC 5737) — guaranteed unreachable, so measureRTT
	// fails and the code falls back to the provided DowntimeLimitMS.
	unreachableIP := netip.MustParseAddr("192.0.2.1")
	err := RunSource(context.Background(), SourceConfig{
		QMPSocket: sock, DestIP: unreachableIP, VMIP: testVMIP, DriveID: "drive-virtio-disk0",
		SharedStorage: true, TunnelMode: TunnelModeNone, DowntimeLimitMS: 25, AutoDowntime: true,
	})
	if err != nil {
		t.Fatalf("RunSource with auto-downtime fallback: %v", err)
	}
}

func TestRunSource_ContextCancelled(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		// Hang forever after handshake — force context cancellation.
		time.Sleep(30 * time.Second)
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
