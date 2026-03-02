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

	ctx, cancel := CleanupCtx(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("CleanupCtx should have a deadline")
	}

	remaining := time.Until(deadline)
	if remaining < 5*time.Second || remaining > CleanupTimeout+time.Second {
		t.Fatalf("expected deadline ~%v from now, got %v", CleanupTimeout, remaining)
	}

	select {
	case <-ctx.Done():
		t.Fatal("CleanupCtx should not be cancelled when parent is")
	default:
	}

	_ = parent
}

func TestRunCmd(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}

	ctx := context.Background()
	cancelledCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		cmd     string
		args    []string
		wantErr string
	}{
		{"Success", ctx, "true", nil, ""},
		{"Failure", ctx, "false", nil, "executing false"},
		{"WithOutput", ctx, "sh", []string{"-c", "echo 'failure output' >&2; exit 1"}, "failure output"},
		{"ContextCancelled", cancelledCtx, "sleep", []string{"30"}, "cancel"},
		{"NotFound", ctx, "nonexistent-binary-xyz-123", nil, "executing nonexistent"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunCmd(tt.ctx, tt.cmd, tt.args...)
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
}

func TestConstants_Reasonable(t *testing.T) {
	t.Parallel()
	if NBDPort == "" || RAMMigrationPort == "" || PlugQdiscLimit == "" || TunnelName == "" {
		t.Fatal("string constants should not be empty")
	}
	if MaxBandwidth <= 0 || EventWaitTimeout <= 0 || StoragePollInterval <= 0 || MigrationPollInterval <= 0 || PostMigrationTunnelDelay <= 0 || GARPRounds <= 0 || MigrationTimeout <= 0 || StorageSyncTimeout <= 0 || JobAppearTimeout <= 0 || CleanupTimeout <= 0 {
		t.Fatal("numeric constants should be positive")
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
	f.Add("::")

	f.Fuzz(func(t *testing.T, s string) {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return
		}
		if result := FormatQEMUHost(addr); result == "" {
			t.Fatal("FormatQEMUHost returned empty string for valid address")
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
			if got := FormatQEMUHost(tt.addr); got != tt.want {
				t.Errorf("FormatQEMUHost(%v) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}
