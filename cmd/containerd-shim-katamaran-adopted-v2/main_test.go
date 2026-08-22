package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidAdoptedSandboxID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"default", defaultAdoptedSandboxID, true},
		{"kata hex id", "3f9a1c0e5b7d4f2a8c6e1b0d9a4f7c2e", true},
		{"dotted", "sandbox.v2_test-1", true},
		{"empty", "", false},
		{"dot dot", "..", false},
		{"parent traversal", "../../../etc", false},
		{"embedded traversal", "foo/../bar", false},
		{"slash", "foo/bar", false},
		{"leading slash", "/abs", false},
		{"null byte", "foo\x00bar", false},
		{"space", "foo bar", false},
		{"newline", "foo\nbar", false},
		{"too long", strings.Repeat("a", 254), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validAdoptedSandboxID(tc.id); got != tc.want {
				t.Fatalf("validAdoptedSandboxID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// FuzzValidAdoptedSandboxID asserts the core security invariant: any id that
// passes validation must be a single safe path component that cannot escape
// adoptedCgroupRoot when joined into a cgroup path.
func FuzzValidAdoptedSandboxID(f *testing.F) {
	for _, seed := range []string{
		defaultAdoptedSandboxID, "", "..", "../etc", "a/b", "/x",
		"abc123", "x\x00y", strings.Repeat("a", 300),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		if !validAdoptedSandboxID(id) {
			return
		}
		if strings.ContainsAny(id, "/\x00") {
			t.Fatalf("accepted id %q contains a path separator or null byte", id)
		}
		if strings.Contains(id, "..") {
			t.Fatalf("accepted id %q contains traversal sequence", id)
		}
		joined := filepath.Join(adoptedCgroupRoot, id)
		if !strings.HasPrefix(joined, adoptedCgroupRoot+"/") {
			t.Fatalf("accepted id %q escapes root: %q", id, joined)
		}
	})
}

// TestRemoveShimSocket pins the server-side cleanup contract: when the
// daemonized shim exits it must unlink the ttrpc socket file runStart
// bound (the parent disabled auto-unlink-on-close so the child could
// inherit it). Without this, every adopted pod leaves a stale .sock in
// /run/containerd/s until the node reboots.
func TestRemoveShimSocket(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "katamaran-test.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	l.SetUnlinkOnClose(false) // mirror runStart: file must outlive the handle

	logs := []string{}
	logFn := func(format string, args ...any) { logs = append(logs, format) }

	removeShimSocket(socketPath, logFn)
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket file %s still exists after removeShimSocket (err=%v)", socketPath, err)
	}
	if len(logs) != 1 || strings.Contains(logs[0], "failed") {
		t.Fatalf("first removal logged %v; want exactly one success line", logs)
	}

	// Idempotent: removing an already-gone path must not log a failure.
	logs = nil
	removeShimSocket(socketPath, logFn)
	for _, l := range logs {
		if strings.Contains(l, "failed") {
			t.Fatalf("removal of missing socket logged a failure: %q", l)
		}
	}

	// Empty path (env var unset, e.g. child started by an older parent)
	// must be a no-op that neither panics nor logs.
	logs = nil
	removeShimSocket("", logFn)
	if len(logs) != 0 {
		t.Fatalf("removeShimSocket(\"\") logged %v; want none", logs)
	}
}
