package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/katamaran"
)

func TestRun_Help(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	// "Usage:" distinguishes the help banner from the version line, which also
	// contains "katamaran".
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("expected usage output containing 'Usage:', got: %s", stdout.String())
	}
}

func TestRun_HelpShort(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("expected usage output containing 'Usage:', got: %s", stdout.String())
	}
}

func TestRun_Version(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), buildinfo.Version) {
		t.Fatalf("expected version %q in output, got: %s", buildinfo.Version, stdout.String())
	}
}

func TestRun_VersionShort(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"-v"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), buildinfo.Version) {
		t.Fatalf("expected version %q in output, got: %s", buildinfo.Version, stdout.String())
	}
}

func TestRun_MissingMode(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--mode") {
		t.Fatalf("expected mode error in stderr, got: %s", stderr.String())
	}
}

func TestRun_InvalidMode(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "invalid"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid --mode") {
		t.Fatalf("expected invalid mode error, got: %s", stderr.String())
	}
}

func TestRun_UnexpectedArgs(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"foo", "bar"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Fatalf("expected unexpected args error, got: %s", stderr.String())
	}
}

func TestRun_UnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--nonexistent-flag"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "nonexistent-flag") {
		t.Fatalf("expected error mentioning unknown flag, got: %s", stderr.String())
	}
}

func TestRun_InvalidLogFormat(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "source", "--log-format", "yaml"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid log format") {
		t.Fatalf("expected log format error, got: %s", stderr.String())
	}
}

// Tests below this point exercise katamaran.Run() past the logging.SetupLogger() call,
// which calls slog.SetDefault() and mutates global state. They must not be
// parallel. Tests above this block that exit before reaching SetupLogger are
// safe for parallel.

func TestRun_NegativeMultifd(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "source", "--multifd-channels", "-1"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--multifd-channels") {
		t.Fatalf("expected multifd error, got: %s", stderr.String())
	}
}

func TestRun_SourceMissingRequiredFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "source"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "--dest-ip") || !strings.Contains(out, "--vm-ip") {
		t.Fatalf("expected missing flags error mentioning --dest-ip and --vm-ip, got: %s", out)
	}
}

func TestRun_SourceInvalidDestIP(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "source", "--dest-ip", "not-an-ip", "--vm-ip", "10.0.0.1"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--dest-ip") {
		t.Fatalf("expected dest-ip error, got: %s", stderr.String())
	}
}

func TestRun_SourceInvalidVMIP(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "source", "--dest-ip", "10.0.0.1", "--vm-ip", "not-an-ip"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--vm-ip") {
		t.Fatalf("expected vm-ip error, got: %s", stderr.String())
	}
}

func TestRun_SourceIPFamilyMismatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "source", "--dest-ip", "10.0.0.1", "--vm-ip", "fd00::1"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "mismatch") {
		t.Fatalf("expected family mismatch error, got: %s", stderr.String())
	}
}

func TestRun_SourceInvalidTunnelMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "source", "--dest-ip", "10.0.0.1", "--vm-ip", "10.0.0.2",
		"--tunnel-mode", "invalid",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--tunnel-mode") {
		t.Fatalf("expected tunnel mode error, got: %s", stderr.String())
	}
}

func TestRun_SourceInvalidDowntime(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "source", "--dest-ip", "10.0.0.1", "--vm-ip", "10.0.0.2",
		"--downtime", "0",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--downtime") {
		t.Fatalf("expected downtime error, got: %s", stderr.String())
	}
}

func TestRun_SourceDowntimeUpperBound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "source", "--dest-ip", "10.0.0.1", "--vm-ip", "10.0.0.2",
		"--downtime", "70000",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--downtime") {
		t.Fatalf("expected downtime error, got: %s", stderr.String())
	}
}

func TestRun_SourceBadQMPSocket(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "source",
		"--dest-ip", "10.0.0.1", "--vm-ip", "10.0.0.2",
		"--qmp", "/nonexistent/qmp.sock",
		"--tunnel-mode", "none",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "QMP") {
		t.Fatalf("expected QMP-related error in stderr, got: %s", stderr.String())
	}
}

func TestRun_DestBadQMPSocket(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "dest",
		"--qmp", "/nonexistent/qmp.sock",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "QMP") {
		t.Fatalf("expected QMP-related error in stderr, got: %s", stderr.String())
	}
}

func TestRun_DestIgnoredSourceFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Migration will fail (bad socket), but the warning should still be printed.
	code := katamaran.Run(context.Background(), []string{
		"--mode", "dest",
		"--dest-ip", "10.0.0.1",
		"--qmp", "/nonexistent/qmp.sock",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit code for bad socket")
	}
	if !strings.Contains(stderr.String(), "ignored in dest mode") {
		t.Fatalf("expected warning about ignored source flags, got stderr: %s", stderr.String())
	}
}

func TestRun_SourceIgnoredDestFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "source",
		"--dest-ip", "10.0.0.1", "--vm-ip", "10.0.0.2",
		"--tap", "tap0",
		"--qmp", "/nonexistent/qmp.sock",
		"--tunnel-mode", "none",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit code for bad socket")
	}
	if !strings.Contains(stderr.String(), "ignored in source mode") {
		t.Fatalf("expected warning about ignored dest flags, got stderr: %s", stderr.String())
	}
}

func TestRun_CaseInsensitiveMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{"--mode", "SOURCE"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	out := stderr.String()
	if strings.Contains(out, "invalid --mode") {
		t.Fatalf("uppercase --mode rejected as invalid: %s", out)
	}
	if !strings.Contains(out, "--dest-ip") {
		t.Fatalf("expected missing flags error (mode accepted), got: %s", out)
	}
}

func TestRun_AutoDowntimeOverridesDowntimeWarning(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "source",
		"--dest-ip", "10.0.0.1", "--vm-ip", "10.0.0.2",
		"--downtime", "50", "--auto-downtime",
		"--qmp", "/nonexistent/qmp.sock",
		"--tunnel-mode", "none",
	}, &stdout, &stderr)
	// Bad QMP socket must surface as exit 1 — guarantees the warning was
	// emitted on the path through to migration, not because validation
	// short-circuited before the auto-downtime check ran.
	if code == 0 {
		t.Fatalf("expected non-zero exit (bad QMP socket), got 0; stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--auto-downtime overrides --downtime") {
		t.Fatalf("expected warning about --auto-downtime overriding --downtime, got stderr: %s", stderr.String())
	}
}

func TestRun_CaseInsensitiveTunnelMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := katamaran.Run(context.Background(), []string{
		"--mode", "source",
		"--dest-ip", "10.0.0.1", "--vm-ip", "10.0.0.2",
		"--tunnel-mode", "GRE",
		"--qmp", "/nonexistent/qmp.sock",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code %d, want 1 (accepted tunnel mode, failed at QMP)", code)
	}
	out := stderr.String()
	if strings.Contains(out, "invalid --tunnel-mode") {
		t.Fatalf("uppercase --tunnel-mode rejected as invalid: %s", out)
	}
}
