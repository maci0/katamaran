package qmptest

import (
	"net"
	"path/filepath"
	"testing"
)

// TestStartFakeQMPSocketPathFitsSunPath pins the socket path budget. Darwin
// caps sockaddr_un.sun_path at 104 bytes, so a fake QMP server bound under
// testing.T.TempDir's long /var/folders/... path fails with a bare
// "bind: invalid argument" and takes the test with it.
func TestStartFakeQMPSocketPathFitsSunPath(t *testing.T) {
	t.Parallel()
	sock := StartFakeQMP(t, func(conn net.Conn) { holdUntilClosed(conn) })

	if len(sock) >= maxSocketPathLen {
		t.Fatalf("socket path is %d bytes, over the %d-byte sun_path limit: %s",
			len(sock), maxSocketPathLen, sock)
	}
	if got := filepath.Base(sock); got != socketName {
		t.Fatalf("socket file name = %q, want %q", got, socketName)
	}
}

func holdUntilClosed(conn net.Conn) {
	buf := make([]byte, 1024)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}
