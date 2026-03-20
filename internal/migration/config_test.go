package migration

import (
	"context"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCleanupCtx(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()

	ctx, cancel := cleanupCtx(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cleanupCtx should have a deadline")
	}

	remaining := time.Until(deadline)
	if remaining < 5*time.Second || remaining > cleanupTimeout+time.Second {
		t.Fatalf("expected deadline ~%v from now, got %v", cleanupTimeout, remaining)
	}

	select {
	case <-ctx.Done():
		t.Fatal("cleanupCtx should not be cancelled when parent is")
	default:
	}
}

func TestRunCmd(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}

	ctx := context.Background()

	tests := []struct {
		name    string
		cmd     string
		args    []string
		wantErr string
	}{
		{"Success", "true", nil, ""},
		{"Failure", "false", nil, "executing false"},
		{"WithOutput", "sh", []string{"-c", "echo 'failure output' >&2; exit 1"}, "failure output"},
		{"NotFound", "nonexistent-binary-xyz-123", nil, "executing nonexistent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := runCmd(ctx, tt.cmd, tt.args...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			}
		})
	}

	// Context cancellation tested separately so the context is created
	// inside the subtest — avoids the parent's 100ms timeout expiring
	// before parallel subtests start.
	t.Run("ContextCancelled", func(t *testing.T) {
		t.Parallel()
		cancelCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		err := runCmd(cancelCtx, "sleep", "30")
		if err == nil {
			t.Fatal("expected error for cancelled context")
		}
		if !strings.Contains(err.Error(), "cancel") {
			t.Fatalf("expected error containing %q, got: %v", "cancel", err)
		}
	})
}

func TestConstants_Reasonable(t *testing.T) {
	t.Parallel()

	for name, val := range map[string]string{
		"nbdPort":          nbdPort,
		"ramMigrationPort": ramMigrationPort,
		"plugQdiscLimit":   plugQdiscLimit,
		"tunnelPrefix":     tunnelPrefix,
	} {
		if val == "" {
			t.Errorf("%s must not be empty", name)
		}
	}

	// Check each numeric/duration constant individually so failures
	// identify exactly which constant is wrong.
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"maxBandwidth", maxBandwidth > 0},
		{"eventWaitTimeout", eventWaitTimeout > 0},
		{"storagePollInterval", storagePollInterval > 0},
		{"migrationPollInterval", migrationPollInterval > 0},
		{"postMigrationTunnelDelay", postMigrationTunnelDelay > 0},
		{"garpRounds", garpRounds > 0},
		{"migrationTimeout", migrationTimeout > 0},
		{"storageSyncTimeout", storageSyncTimeout > 0},
		{"jobAppearTimeout", jobAppearTimeout > 0},
		{"cleanupTimeout", cleanupTimeout > 0},
		{"DefaultMultifdChannels", DefaultMultifdChannels > 0},
		{"rttMultiplier", rttMultiplier > 0},
		{"rttMinOverheadMS", rttMinOverheadMS > 0},
	} {
		if !c.ok {
			t.Errorf("%s must be positive", c.name)
		}
	}
}

func TestGenerateTunnelName(t *testing.T) {
	t.Parallel()
	name, err := generateTunnelName()
	if err != nil {
		t.Fatalf("generateTunnelName: %v", err)
	}
	if !strings.HasPrefix(name, tunnelPrefix) {
		t.Fatalf("expected prefix %q, got %q", tunnelPrefix, name)
	}
	// Linux IFNAMSIZ is 16 (15 chars + null). Name must fit.
	if len(name) > 15 {
		t.Fatalf("tunnel name %q exceeds 15 chars (IFNAMSIZ-1)", name)
	}

	// Names should be unique.
	name2, err := generateTunnelName()
	if err != nil {
		t.Fatalf("second generateTunnelName: %v", err)
	}
	if name == name2 {
		t.Fatalf("expected unique names, got %q twice", name)
	}
}

func FuzzFormatQEMUHost(f *testing.F) {
	f.Add("10.0.0.1")
	f.Add("127.0.0.1")
	f.Add("fd00::1")
	f.Add("::1")
	f.Add("2001:db8::1")
	f.Add("::ffff:10.0.0.1")
	f.Add("0.0.0.0")
	f.Add("::")
	f.Add("255.255.255.255")
	f.Add("fe80::1%eth0")
	f.Add("::ffff:192.168.0.1")
	f.Add("1.1.1.1")

	f.Fuzz(func(t *testing.T, s string) {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return
		}
		if result := formatQEMUHost(addr); result == "" {
			t.Fatal("formatQEMUHost returned empty string for valid address")
		}
	})
}

func TestFormatQEMUHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr netip.Addr
		want string
	}{
		{"ipv4", netip.MustParseAddr("10.0.0.1"), "10.0.0.1"},
		{"ipv4 loopback", netip.MustParseAddr("127.0.0.1"), "127.0.0.1"},
		{"ipv6 full", netip.MustParseAddr("fd00::1"), "[fd00::1]"},
		{"ipv6 loopback", netip.MustParseAddr("::1"), "[::1]"},
		{"ipv6 long", netip.MustParseAddr("2001:db8::1"), "[2001:db8::1]"},
		{"ipv4-mapped ipv6", netip.MustParseAddr("::ffff:10.0.0.1"), "::ffff:10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatQEMUHost(tt.addr); got != tt.want {
				t.Errorf("formatQEMUHost(%v) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}
