package migration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRunDestination_Failures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		tap           string
		sharedStorage bool
	}{
		{"BadQMPSocket", "", false},
		{"SharedStorage_BadQMPSocket", "", true},
		{"WithTap_BadQMPSocket", "nonexistent-tap0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunDestination(
				context.Background(),
				"/nonexistent/qmp.sock",
				tt.tap,
				"",
				"drive-virtio-disk0",
				tt.sharedStorage,
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

func TestRunDestination_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := RunDestination(ctx, "/nonexistent/qmp.sock", "", "", "drive-virtio-disk0", false)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestRunDestination_SharedStorage_HappyPath(t *testing.T) {
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

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				// After accepting migrate-incoming, send RESUME event.
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
				continue
			}
			// Respond to all other commands with success.
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), sock, "", "", "drive-virtio-disk0", true)
	if err != nil {
		t.Fatalf("RunDestination shared-storage happy path: %v", err)
	}
}

func TestRunDestination_NonShared_HappyPath(t *testing.T) {
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

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				// After migrate-incoming, the NBD setup commands come next,
				// then RESUME should fire.
				continue
			}
			if strings.Contains(line, "nbd-server-stop") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			if strings.Contains(line, "nbd-server-start") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			if strings.Contains(line, "nbd-server-add") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				// After NBD setup is complete, send RESUME event.
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), sock, "", "", "drive-virtio-disk0", false)
	if err != nil {
		t.Fatalf("RunDestination non-shared happy path: %v", err)
	}
}

func TestRunDestination_MigrateIncomingFailure(t *testing.T) {
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

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"incoming failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), sock, "", "", "drive-virtio-disk0", true)
	if err == nil {
		t.Fatal("expected error for migrate-incoming failure")
	}
	if !strings.Contains(err.Error(), "incoming") {
		t.Fatalf("expected 'incoming' in error, got: %v", err)
	}
}

func TestRunDestination_NBDServerStartFailure(t *testing.T) {
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

			if strings.Contains(line, "nbd-server-start") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"bind failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), sock, "", "", "drive-virtio-disk0", false)
	if err == nil {
		t.Fatal("expected error for NBD server start failure")
	}
	if !strings.Contains(err.Error(), "NBD") {
		t.Fatalf("expected 'NBD' in error, got: %v", err)
	}
}

func TestRunDestination_NBDServerAddFailure(t *testing.T) {
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

			if strings.Contains(line, "nbd-server-add") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"export failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), sock, "", "", "drive-virtio-disk0", false)
	if err == nil {
		t.Fatal("expected error for NBD server add failure")
	}
	if !strings.Contains(err.Error(), "NBD export") {
		t.Fatalf("expected 'NBD export' in error, got: %v", err)
	}
}

func TestRunDestination_GARPFailure(t *testing.T) {
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

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
				continue
			}
			if strings.Contains(line, "announce-self") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"announce failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), sock, "", "", "drive-virtio-disk0", true)
	if err == nil {
		t.Fatal("expected error for GARP failure")
	}
	if !strings.Contains(err.Error(), "GARP") {
		t.Fatalf("expected 'GARP' in error, got: %v", err)
	}
}
