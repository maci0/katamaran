// Package qmptest provides shared test helpers for faking a QMP server.
//
// Used by both internal/qmp and internal/migration test suites to avoid
// duplicating the fake server setup and QMP handshake logic.
package qmptest

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// socketName is the fake QMP socket's file name. Kept short because the
	// whole path has to fit in sockaddr_un.sun_path.
	socketName = "q.sock"

	// maxSocketPathLen is the shorter of the two sun_path limits the test
	// suite can run into: 104 bytes on Darwin, 108 on Linux. A path over the
	// limit makes net.Listen fail with a bare "bind: invalid argument", so
	// the length is checked up front and reported with the offending path.
	maxSocketPathLen = 100
)

// TempDir returns a short-lived directory suitable for holding a unix socket.
// Darwin caps sockaddr_un.sun_path at 104 bytes and testing.T.TempDir lands
// under /var/folders/... on macOS, which alone eats most of that budget, so
// socket-bearing directories use a two-character os.MkdirTemp prefix instead.
// Use it in place of t.TempDir() wherever the directory ends up in a socket
// path; plain file trees can keep t.TempDir().
func TempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kq")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// StartFakeQMP creates a Unix listener that accepts one connection and runs handler.
func StartFakeQMP(t *testing.T, handler func(conn net.Conn)) string {
	t.Helper()
	socketPath := filepath.Join(TempDir(t), socketName)
	if len(socketPath) >= maxSocketPathLen {
		t.Fatalf("fake QMP socket path is %d bytes, over the %d-byte sun_path limit: %s",
			len(socketPath), maxSocketPathLen, socketPath)
	}
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

// ConsumeCommand reads and discards one QMP command from the connection,
// used in tests to skip expected intermediate commands during handshake flows.
func ConsumeCommand(conn net.Conn) {
	buf := make([]byte, 4096)
	conn.Read(buf)
}

// IsMigrateCommand returns true if line contains the "migrate" QMP command,
// excluding "migrate-set-*", "migrate-incoming", "query-migrate", and "migrate-cancel".
func IsMigrateCommand(line string) bool {
	return strings.Contains(line, `"migrate"`) &&
		!strings.Contains(line, "migrate-set") &&
		!strings.Contains(line, "migrate-incoming") &&
		!strings.Contains(line, "query-migrate") &&
		!strings.Contains(line, "migrate-cancel")
}

// StartScriptedQMP starts a fake QMP server that completes the handshake and
// then answers every command with {"return":{}} except when the raw command
// line contains a script key, in which case the mapped responses are written
// verbatim instead (each newline-terminated).
//
// Keys are matched with strings.Contains against the raw line; include quotes
// to disambiguate (the key `"migrate"` matches only the bare migrate command).
func StartScriptedQMP(t *testing.T, script map[string][]string) string {
	t.Helper()
	return StartFakeQMP(t, func(conn net.Conn) {
		QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])
			responses := []string{`{"return":{}}`}
			for key, resp := range script {
				if strings.Contains(line, key) {
					responses = resp
					break
				}
			}
			for _, resp := range responses {
				conn.Write([]byte(resp + "\n"))
			}
		}
	})
}
