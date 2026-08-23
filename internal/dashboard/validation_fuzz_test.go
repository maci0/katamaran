package dashboard

import (
	"strconv"
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// FuzzSplitTarget exercises the host:port splitter on arbitrary user-supplied
// target strings. It must never panic and, on success, must report a host that
// is a substring of the original target (the SSRF checks downstream rely on
// splitTarget returning something derived from the raw input).
func FuzzSplitTarget(f *testing.F) {
	seeds := []string{
		"", "host", "host:80", "10.0.0.1:65535", "[::1]", "[fe80::1]:443",
		"[", "]", "[]", ":", ":80", "host:", "host:port", "a:b:c",
		"-flag", "h ost", "host\x00", strings.Repeat("a", 300),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, target string) {
		host, port, hasPort, ok := splitTarget(target)
		if !ok {
			return
		}
		if !strings.Contains(target, host) {
			t.Fatalf("split host %q not contained in target %q", host, target)
		}
		if hasPort && port != "" && !strings.Contains(target, port) {
			t.Fatalf("split port %q not contained in target %q", port, target)
		}
	})
}

// FuzzValidTargetPort asserts the port validator only accepts canonical decimal
// strings in the legal 1-65535 range — anything it accepts must round-trip
// through strconv with an in-range result.
func FuzzValidTargetPort(f *testing.F) {
	for _, s := range []string{"", "0", "1", "80", "65535", "65536", "-1", "+1", "08", "0x10", "99999999999999999999", "8a", " 80"} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, port string) {
		if !validTargetPort(port) {
			return
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			t.Fatalf("accepted unparseable port %q", port)
		}
		if p < 1 || p > 65535 {
			t.Fatalf("accepted out-of-range port %q (%d)", port, p)
		}
		for _, c := range port {
			if c < '0' || c > '9' {
				t.Fatalf("accepted port with non-digit %q: %q", c, port)
			}
		}
	})
}

// FuzzValidFormValue confirms the dashboard form-value guard never panics on
// arbitrary input and stays consistent with the underlying orchestrator gate:
// any value it accepts must be free of shell metacharacters and path traversal,
// since accepted values are interpolated into the rendered migration Job's
// shell command.
func FuzzValidFormValue(f *testing.F) {
	for _, s := range []string{"", "ok-value", "../x", ";id", "$(x)", "a\x00b", strings.Repeat("z", 600)} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, v string) {
		ok := validFormValue(v)
		// validFormValue must agree with the orchestrator gate it wraps;
		// a disagreement means the dashboard would admit (or reject) a value
		// the migration Job renderer treats differently.
		if want := orchestrator.ValidateSafeArgValue("form field", v) == nil; ok != want {
			t.Fatalf("validFormValue(%q)=%v disagrees with orchestrator gate (=%v)", v, ok, want)
		}
		if !ok {
			return
		}
		// Accepted: prove the value cannot carry a shell/path-traversal payload.
		if len(v) > orchestrator.MaxSafeArgValueLen {
			t.Fatalf("accepted over-long value (len=%d > %d)", len(v), orchestrator.MaxSafeArgValueLen)
		}
		if strings.Contains(v, "..") {
			t.Fatalf("accepted value with path traversal: %q", v)
		}
		for _, r := range v {
			allowed := (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') ||
				r == '_' || r == '.' || r == '/' || r == ':' ||
				r == '@' || r == '=' || r == '-'
			if !allowed {
				t.Fatalf("accepted value with disallowed rune %q in %q", r, v)
			}
		}
	})
}
