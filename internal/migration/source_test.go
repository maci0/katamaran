package migration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestErrMigrationFailed_Exists(t *testing.T) {
	t.Parallel()
	if ErrMigrationFailed == nil {
		t.Fatal("ErrMigrationFailed should not be nil")
	}
	if !errors.Is(ErrMigrationFailed, ErrMigrationFailed) {
		t.Fatal("ErrMigrationFailed should be matchable with errors.Is")
	}
}

func TestErrMigrationCancelled_Exists(t *testing.T) {
	t.Parallel()
	if ErrMigrationCancelled == nil {
		t.Fatal("ErrMigrationCancelled should not be nil")
	}
	if !errors.Is(ErrMigrationCancelled, ErrMigrationCancelled) {
		t.Fatal("ErrMigrationCancelled should be matchable with errors.Is")
	}
}

func TestErrMigrationFailed_Distinct(t *testing.T) {
	t.Parallel()
	if errors.Is(ErrMigrationFailed, ErrMigrationCancelled) {
		t.Fatal("ErrMigrationFailed and ErrMigrationCancelled should be distinct")
	}
}

func TestRunSource_BadQMPSocket(t *testing.T) {
	t.Parallel()
	err := RunSource(
		context.Background(),
		"/nonexistent/qmp.sock",
		"10.0.0.1", "10.244.1.15",
		"drive-virtio-disk0",
		false,
		"ipip",
	)
	if err == nil {
		t.Fatal("expected error for nonexistent QMP socket")
	}
	if !strings.Contains(err.Error(), "QMP") {
		t.Fatalf("expected QMP-related error, got: %v", err)
	}
}

func TestRunSource_SharedStorage_BadQMPSocket(t *testing.T) {
	t.Parallel()
	err := RunSource(
		context.Background(),
		"/nonexistent/qmp.sock",
		"10.0.0.1", "10.244.1.15",
		"drive-virtio-disk0",
		true,
		"ipip",
	)
	if err == nil {
		t.Fatal("expected error for nonexistent QMP socket")
	}
}

// startFakeQMPServer starts a Unix socket server that handles the QMP handshake
// and dispatches commands to the given handler.
func startFakeQMPServer(t *testing.T, handler func(cmd string, args json.RawMessage) interface{}) string {
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

		// Send greeting.
		conn.Write([]byte(`{"QMP":{"version":{"qemu":{"micro":0,"minor":2,"major":6}}}}` + "\n"))

		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		// qmp_capabilities response
		conn.Write([]byte(`{"return":{}}` + "\n"))

		for scanner.Scan() {
			line := scanner.Text()
			var req struct {
				Execute   string          `json:"execute"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				continue
			}

			resp := handler(req.Execute, req.Arguments)
			b, _ := json.Marshal(resp)
			conn.Write(append(b, '\n'))
		}
	}()
	return socketPath
}

func TestRunSource_NonShared_BadQMPSocket(t *testing.T) {
	t.Parallel()
	err := RunSource(
		context.Background(),
		"/nonexistent/qmp.sock",
		"10.0.0.1", "10.244.1.15",
		"drive-virtio-disk0",
		false,
		"gre",
	)
	if err == nil {
		t.Fatal("expected error")
	}
}
