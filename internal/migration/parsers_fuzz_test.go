package migration

import (
	"strings"
	"testing"
)

// FuzzParseCmdlineBytes hammers the raw-byte cmdline splitter, which consumes
// untrusted /proc/<pid>/cmdline content and captured cmdline files. The only
// contract is "never panic, never emit empty fields"; the delimiter rule (NUL
// wins over newline) is asserted so a regression that picks the wrong
// separator is caught instead of silently mangling argv.
func FuzzParseCmdlineBytes(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		[]byte("qemu\x00-name\x00guest\x00"),
		[]byte("qemu\n-name\nguest\n"),
		[]byte("\x00\x00\x00"),
		[]byte("\n\n\n"),
		[]byte("only-one"),
		[]byte("mixed\x00with\nboth"),
		[]byte("trailing\x00"),
		{0x00, 0xff, 0x00},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		args := parseCmdlineBytes(data)
		for _, a := range args {
			if a == "" {
				t.Fatalf("parseCmdlineBytes emitted an empty field for %q", data)
			}
		}
		// Delimiter rule: NUL present => split on NUL only (newlines stay inside
		// fields); otherwise split on newline.
		if len(data) > 0 {
			sep := "\n"
			for _, b := range data {
				if b == 0 {
					sep = "\x00"
					break
				}
			}
			if sep == "\n" {
				for _, a := range args {
					if strings.Contains(a, "\n") {
						t.Fatalf("field %q contains the newline separator", a)
					}
				}
			} else {
				for _, a := range args {
					if strings.ContainsRune(a, 0) {
						t.Fatalf("field %q contains the NUL separator", a)
					}
				}
			}
		}
	})
}

// FuzzFindSrcSandboxDir targets the index/slice arithmetic that extracts the
// source sandbox UUID out of an arbitrary cmdline token. The captured cmdline
// is attacker-influenced (it comes from the source pod's QEMU), and a bad
// slice bound here would panic the dest replay path. Asserts the returned
// pair is internally consistent so a malformed token can't yield a sandbox dir
// that doesn't match its reported ID.
func FuzzFindSrcSandboxDir(f *testing.F) {
	type seed struct {
		arg  string
		root string
	}
	seeds := []seed{
		{"", "/run/vc/vm"},
		{"socket=/run/vc/vm/abc123/qmp.sock", "/run/vc/vm"},
		{"/run/vc/vm/", "/run/vc/vm"},
		{"/run/vc/vm/id,extra", "/run/vc/vm/"},
		{"/run/vc/vm/id=val", "/run/vc/vm"},
		{"noprefix", "/run/vc/vm"},
		{"/run/vc/vm/a/b/c", "///"},
	}
	for _, s := range seeds {
		f.Add(s.arg, s.root)
	}

	f.Fuzz(func(t *testing.T, arg, root string) {
		dir, id := findSrcSandboxDir([]string{arg}, root)
		if dir == "" && id == "" {
			return
		}
		if id == "" {
			t.Fatalf("returned non-empty dir %q with empty id", dir)
		}
		prefix := strings.TrimRight(root, "/") + "/"
		if dir != prefix+id {
			t.Fatalf("dir %q != prefix %q + id %q", dir, prefix, id)
		}
		// The ID is a single path segment: it must not contain any of the
		// terminators findSrcSandboxDir splits on.
		if strings.ContainsAny(id, "/,= \t") {
			t.Fatalf("id %q contains a separator", id)
		}
	})
}

// FuzzParsePodRef fuzzes the "<namespace>/<name>" reference parser. The ref
// originates from a marker emitted across the migration boundary, and the
// parser applies a DNS-1123 regex to each half, so this also exercises that
// regex against adversarial input (ReDoS / catastrophic backtracking).
func FuzzParsePodRef(f *testing.F) {
	seeds := []string{
		"",
		"/",
		"ns/pod",
		"ns/",
		"/pod",
		"a/b/c",
		strings.Repeat("a", 254) + "/pod",
		"NS/POD",
		"ns/pod-1.example",
		"-bad/pod",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, ref string) {
		ns, pod, err := parsePodRef(ref)
		if err != nil {
			if ns != "" || pod != "" {
				t.Fatalf("error path returned non-empty (%q,%q)", ns, pod)
			}
			return
		}
		if ns == "" || pod == "" {
			t.Fatalf("accepted ref %q with empty ns/pod (%q,%q)", ref, ns, pod)
		}
		if len(ns) > 253 || len(pod) > 253 {
			t.Fatalf("accepted over-long label in %q (ns=%d pod=%d)", ref, len(ns), len(pod))
		}
		if strings.ContainsRune(ns, '/') {
			t.Fatalf("namespace %q contains a slash", ns)
		}
	})
}
