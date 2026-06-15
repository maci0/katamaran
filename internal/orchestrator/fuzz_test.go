package orchestrator

import (
	"strings"
	"testing"
)

// FuzzValidateSafeArgValue targets the shell-safety gate that every raw form
// value passes through before being assembled into a katamaran CLI argument.
// A miss here is directly exploitable as argument/shell injection, so the
// harness asserts the accept/reject contract holds for arbitrary input rather
// than only checking for panics.
func FuzzValidateSafeArgValue(f *testing.F) {
	seeds := []string{
		"",
		"node-1",
		"10.0.0.5:4444",
		"ns/pod",
		"user@host",
		"a=b",
		"../etc/passwd",
		";rm -rf /",
		"$(whoami)",
		"a b",
		"a\x00b",
		"naïve",
		strings.Repeat("a", MaxSafeArgValueLen),
		strings.Repeat("a", MaxSafeArgValueLen+1),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, value string) {
		err := ValidateSafeArgValue("field", value)
		if err != nil {
			return
		}
		// Accepted: prove the value cannot carry shell/path-traversal payloads.
		if len(value) > MaxSafeArgValueLen {
			t.Fatalf("accepted over-long value (len=%d > %d)", len(value), MaxSafeArgValueLen)
		}
		if strings.Contains(value, "..") {
			t.Fatalf("accepted value with path traversal: %q", value)
		}
		for _, r := range value {
			ok := (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') ||
				r == '_' || r == '.' || r == '/' || r == ':' ||
				r == '@' || r == '=' || r == '-'
			if !ok {
				t.Fatalf("accepted value with disallowed rune %q in %q", r, value)
			}
		}
	})
}
