// Package qmptest provides shared test helpers for faking a QMP server.
//
// Used by both internal/qmp and internal/migration test suites to avoid
// duplicating the fake server setup and QMP handshake logic.
package qmptest

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// StartFakeQMP creates a Unix listener that accepts one connection and runs handler.
func StartFakeQMP(t *testing.T, handler func(conn net.Conn)) string {
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

// QMPHandshake performs the server side of the QMP greeting + capabilities handshake.
func QMPHandshake(conn net.Conn) {
	greeting := `{"QMP":{"version":{"qemu":{"micro":0,"minor":2,"major":6}}}}`
	conn.Write([]byte(greeting + "\n"))
	buf := make([]byte, 4096)
	conn.Read(buf)
	conn.Write([]byte(`{"return":{}}` + "\n"))
}

// ConsumeCommand reads one command from the connection and discards it.
func ConsumeCommand(conn net.Conn) {
	buf := make([]byte, 4096)
	conn.Read(buf)
}

// IsMigrateCommand returns true if line contains the "migrate" QMP command,
// excluding "migrate-set-*", "migrate-incoming", "query-migrate", and "migrate_cancel".
func IsMigrateCommand(line string) bool {
	return strings.Contains(line, `"migrate"`) &&
		!strings.Contains(line, "migrate-set") &&
		!strings.Contains(line, "migrate-incoming") &&
		!strings.Contains(line, "query-migrate") &&
		!strings.Contains(line, "migrate_cancel")
}
