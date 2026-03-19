package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/qmp"
)

func TestErrMigrationFailed_Exists(t *testing.T) {
	t.Parallel()
	if ErrMigrationFailed == nil || !errors.Is(ErrMigrationFailed, ErrMigrationFailed) {
		t.Fatal("ErrMigrationFailed should be valid and matchable")
	}
	if ErrMigrationCancelled == nil || !errors.Is(ErrMigrationCancelled, ErrMigrationCancelled) {
		t.Fatal("ErrMigrationCancelled should be valid and matchable")
	}
	if errors.Is(ErrMigrationFailed, ErrMigrationCancelled) {
		t.Fatal("ErrMigrationFailed and ErrMigrationCancelled should be distinct")
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunSource(
				context.Background(),
				"/nonexistent/qmp.sock",
				destIP,
				vmIP,
				"drive-virtio-disk0",
				tt.sharedStorage,
				tt.tunnelMode,
				25,
				false,
				0,
			)
			if err == nil {
				t.Fatal("expected error for nonexistent QMP socket")
			}
			if !strings.Contains(err.Error(), "QMP") {
				t.Fatalf("expected QMP-related error, got: %v", err)
			}
		})
	}
}

// startFakeQMP creates a Unix listener that accepts one connection and runs handler.
func startFakeQMP(t *testing.T, handler func(conn net.Conn)) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return socketPath
}

// qmpHandshake performs the server side of the QMP greeting + capabilities handshake.
func qmpHandshake(conn net.Conn) {
	greeting := `{"QMP":{"version":{"qemu":{"micro":0,"minor":2,"major":6}}}}`
	conn.Write([]byte(greeting + "\n"))
	buf := make([]byte, 4096)
	conn.Read(buf)
	conn.Write([]byte(`{"return":{}}` + "\n"))
}

// consumeCommand reads one command from the connection and discards it.
func consumeCommand(conn net.Conn) {
	buf := make([]byte, 4096)
	conn.Read(buf)
}

func TestWaitForStorageSync_JobReady(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		// First poll: job running at 50%.
		consumeCommand(conn)
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
		consumeCommand(conn)
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
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		// First poll: job present.
		consumeCommand(conn)
		jobs := []qmp.BlockJobInfo{{Device: "mirror-drive0", Len: 1000, Offset: 500, Status: "running", Type: "mirror"}}
		b, _ := json.Marshal(jobs)
		conn.Write([]byte(`{"return":` + string(b) + "}\n"))
		// Second poll: job gone.
		consumeCommand(conn)
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

	// Override JobAppearTimeout for faster test. We can't do that without
	// changing the constant, so just verify context cancellation works.
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		for {
			consumeCommand(conn)
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
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		consumeCommand(conn)
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
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		// First poll: active.
		consumeCommand(conn)
		conn.Write([]byte(`{"return":{"status":"active","ram":{"total":1000,"transferred":500,"remaining":500}}}` + "\n"))
		// Second poll: completed.
		consumeCommand(conn)
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
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		consumeCommand(conn)
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
	if !errors.Is(err, ErrMigrationFailed) {
		t.Fatalf("expected ErrMigrationFailed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("expected error description in message, got: %v", err)
	}
}

func TestWaitForMigrationComplete_Cancelled(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		consumeCommand(conn)
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
	if !errors.Is(err, ErrMigrationCancelled) {
		t.Fatalf("expected ErrMigrationCancelled, got: %v", err)
	}
}

func TestWaitForMigrationComplete_ContextCancelled(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
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
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		consumeCommand(conn)
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
	if !errors.Is(err, ErrMigrationFailed) {
		t.Fatalf("expected ErrMigrationFailed, got: %v", err)
	}
}

func TestRunSource_SharedStorage_HappyPath(t *testing.T) {
	t.Parallel()

	handlers := map[string]string{
		"migrate-set-capabilities": `{"return":{}}`,
		"migrate-set-parameters":   `{"return":{}}`,
		"migrate":                  `{"return":{}}`,
		"query-migrate":            `{"return":{"status":"completed","downtime":15,"total-time":1200,"setup-time":50}}`,
		"block-job-cancel":         `{"return":{}}`,
	}

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			// After "migrate" command, inject STOP event before the response.
			if strings.Contains(line, `"migrate"`) && !strings.Contains(line, "migrate-set") && !strings.Contains(line, "query-migrate") && !strings.Contains(line, "migrate_cancel") {
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
	if err != nil {
		t.Fatalf("RunSource shared-storage happy path: %v", err)
	}
}

func TestRunSource_SharedStorage_MigrationFailed(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, `"migrate"`) && !strings.Contains(line, "migrate-set") && !strings.Contains(line, "query-migrate") && !strings.Contains(line, "migrate_cancel") {
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
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

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
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
			if strings.Contains(line, `"migrate"`) && !strings.Contains(line, "migrate-set") && !strings.Contains(line, "query-migrate") && !strings.Contains(line, "migrate_cancel") {
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", false, "none", 25, false, 0)
	if err != nil {
		t.Fatalf("RunSource non-shared happy path: %v", err)
	}
}

func TestRunSource_MigrationFailedDuringPolling(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, `"migrate"`) && !strings.Contains(line, "migrate-set") && !strings.Contains(line, "query-migrate") && !strings.Contains(line, "migrate_cancel") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			// During STOP polling, report migration as failed.
			if strings.Contains(line, "query-migrate") {
				conn.Write([]byte(`{"return":{"status":"failed","error-desc":"RAM migration failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
	if err == nil {
		t.Fatal("expected error when migration fails during STOP polling")
	}
	if !strings.Contains(err.Error(), "RAM migration failed") {
		t.Fatalf("expected error description, got: %v", err)
	}
}

func TestRunSource_MigrationCancelledDuringPolling(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, `"migrate"`) && !strings.Contains(line, "migrate-set") && !strings.Contains(line, "query-migrate") && !strings.Contains(line, "migrate_cancel") {
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
	if err == nil {
		t.Fatal("expected error when migration cancelled during STOP polling")
	}
	if !errors.Is(err, ErrMigrationCancelled) {
		t.Fatalf("expected ErrMigrationCancelled, got: %v", err)
	}
}

func TestRunSource_CompletedDuringPolling(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, `"migrate"`) && !strings.Contains(line, "migrate-set") && !strings.Contains(line, "query-migrate") && !strings.Contains(line, "migrate_cancel") {
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
	if err != nil {
		t.Fatalf("RunSource completed-during-polling: %v", err)
	}
}

func TestMeasureRTT(t *testing.T) {
	t.Parallel()

	// measureRTT hardcodes RAMMigrationPort, so we can only test the error
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
	// With empty netnsPath, should delegate directly to RunCmd.
	if err := RunCmdInNetns(context.Background(), "", "true"); err != nil {
		t.Fatalf("expected success with empty netns, got: %v", err)
	}
}

func TestRunCmdInNetns_WithNetns(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	// nsenter with /proc/1/ns/net requires root; verify the nsenter code path
	// runs without panic. It succeeds as root, errors otherwise — both OK.
	_ = RunCmdInNetns(context.Background(), "/proc/1/ns/net", "true")
}

func TestRunSource_DriveMirrorFailure(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", false, "none", 25, false, 0)
	if err == nil {
		t.Fatal("expected error for drive-mirror failure")
	}
	if !strings.Contains(err.Error(), "drive-mirror") {
		t.Fatalf("expected 'drive-mirror' in error, got: %v", err)
	}
}

func TestRunSource_SetCapabilitiesFailure(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
	if err == nil {
		t.Fatal("expected error for capabilities failure")
	}
	if !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("expected 'capabilities' in error, got: %v", err)
	}
}

func TestRunSource_SetParametersFailure(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
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

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
	if err == nil {
		t.Fatal("expected error for parameters failure")
	}
	if !strings.Contains(err.Error(), "parameters") {
		t.Fatalf("expected 'parameters' in error, got: %v", err)
	}
}

func TestRunSource_MigrateCommandFailure(t *testing.T) {
	t.Parallel()

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, `"migrate"`) && !strings.Contains(line, "migrate-set") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"migrate failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")
	err := RunSource(context.Background(), sock, destIP, vmIP, "drive-virtio-disk0", true, "none", 25, false, 0)
	if err == nil {
		t.Fatal("expected error for migrate command failure")
	}
	if !strings.Contains(err.Error(), "RAM migration") {
		t.Fatalf("expected 'RAM migration' in error, got: %v", err)
	}
}
