package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
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

func TestWriteShimLogBound(t *testing.T) {
	const maxBytes = 32
	path := filepath.Join(t.TempDir(), "shim.log")
	openLog := func() *os.File {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	writers := []*os.File{openLog(), openLog()}
	if err := syscall.Flock(int(writers[0].Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	err := writeShimLog(writers[1], maxBytes, []byte("contended\n"))
	if unlockErr := syscall.Flock(int(writers[0].Fd()), syscall.LOCK_UN); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("contended write error = %v, want EWOULDBLOCK", err)
	}
	for i, data := range [][]byte{
		[]byte("first record\n"),
		[]byte("second record\n"),
		[]byte("third record\n"),
		bytes.Repeat([]byte("x"), maxBytes*2),
		[]byte("last record\n"),
	} {
		if err := writeShimLog(writers[i%len(writers)], maxBytes, data); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := data
		if len(want) > maxBytes {
			want = want[len(want)-maxBytes:]
		}
		if len(got) > maxBytes || !bytes.HasSuffix(got, want) {
			t.Fatalf("write %d: log = %q, want at most %d bytes ending in %q", i, got, maxBytes, want)
		}
	}
	for _, f := range writers {
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		current, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(info, current) {
			t.Fatal("writer retains a rotated inode")
		}
	}
}

func TestWriteShimLogTruncationKeepsValidUTF8(t *testing.T) {
	// Every offset of a 4-byte rune is exercised: the byte-boundary cut
	// lands mid-rune for all but the leading one, and the kept window must
	// never start on a continuation byte.
	for _, r := range []rune{'é', '界', '😀'} {
		text := []byte(string(r))
		for cut := 0; cut < len(text); cut++ {
			const maxBytes = 8
			path := filepath.Join(t.TempDir(), "shim.log")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			record := append(bytes.Repeat([]byte("x"), maxBytes-len(text)+cut), text...)
			record = append(record, []byte("tail")...)
			if err := writeShimLog(f, maxBytes, record); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.Valid(got) {
				t.Errorf("rune %q cut at offset %d: kept %d bytes = %q, want valid UTF-8", r, cut, maxBytes, got)
			}
			if !bytes.HasSuffix(got, []byte("tail")) {
				t.Errorf("rune %q cut at offset %d: log = %q, want the record tail", r, cut, got)
			}
		}
	}
}

func TestWriteShimLogExistingOversizedFile(t *testing.T) {
	const maxBytes = 32
	path := filepath.Join(t.TempDir(), "shim.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxBytes*2), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := []byte("new record\n")
	if err := writeShimLog(f, maxBytes, want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestReadAdoptedSandboxID(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "annotation present",
			body: `{"annotations":{"` + adoptedSandboxAnnotation + `":"sandbox-a"}}`,
			want: "sandbox-a",
		},
		{
			name: "annotation absent",
			body: `{"annotations":{"other":"x"}}`,
			want: "",
		},
		{
			name: "needle quoted inside another value",
			body: `{"annotations":{"note":"see \"` + adoptedSandboxAnnotation + `\":\"wrong\" here","` +
				adoptedSandboxAnnotation + `":"right"}}`,
			want: "right",
		},
		{
			name: "escaped value",
			body: `{"annotations":{"` + adoptedSandboxAnnotation + `":"sandbox\u002da"}}`,
			want: "sandbox-a",
		},
		{
			name: "malformed json",
			body: `{"annotations":{"` + adoptedSandboxAnnotation + `":"a"`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readAdoptedSandboxID(write(t, tc.body)); got != tc.want {
				t.Errorf("readAdoptedSandboxID = %q, want %q", got, tc.want)
			}
		})
	}
	if got := readAdoptedSandboxID(""); got != "" {
		t.Errorf("readAdoptedSandboxID(\"\") = %q, want empty", got)
	}
}
