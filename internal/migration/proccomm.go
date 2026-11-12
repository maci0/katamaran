package migration

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// kvmHelperPrefix is the start of every KVM helper kernel thread's comm
// (kvm-nx-lpage-recovery-<pid>, kvm-pit/<pid>, ...).
var kvmHelperPrefix = []byte("kvm-")

// readProcComm reads a /proc/<pid>/comm file into buf and returns the bytes
// read. comm is at most TASK_COMM_LEN (16) bytes plus a newline, so a small
// buf always holds it and the read never allocates. The returned slice aliases
// buf and is valid only until the next call with the same buf. ok is false
// when the process vanished between the directory read and the open, the
// normal case for a short-lived process during a full /proc walk.
func readProcComm(path string, buf []byte) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	n, err := f.Read(buf)
	if n == 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return nil, false
	}
	return buf[:n], true
}

// kvmHelperName reports comm with its trailing newline removed when it names a
// KVM helper thread for suffix, which is "-<qemu pid>". Both comparisons run
// on the raw bytes so a /proc walk never allocates a string for the processes
// that do not match.
func kvmHelperName(comm, suffix []byte) ([]byte, bool) {
	name := bytes.TrimRight(comm, "\n")
	if !bytes.HasPrefix(name, kvmHelperPrefix) || !bytes.HasSuffix(name, suffix) {
		return nil, false
	}
	return name, true
}
