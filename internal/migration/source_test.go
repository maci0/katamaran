package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
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
		tunnelMode    string
	}{
		{"BadQMPSocket", false, "ipip"},
		{"SharedStorage_BadQMPSocket", true, "ipip"},
		{"NonShared_BadQMPSocket", false, "gre"},
	}

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")

	for _, tt := range tests {
		tt := tt
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
