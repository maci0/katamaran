package migration

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

func TestIPFamily(t *testing.T) {
	t.Parallel()
	if got := IPFamily(netip.MustParseAddr("10.0.0.1")); got != "IPv4" {
		t.Fatalf("got %q, want IPv4", got)
	}
	if got := IPFamily(netip.MustParseAddr("fd00::1")); got != "IPv6" {
		t.Fatalf("got %q, want IPv6", got)
	}
}

func TestSetupTunnel_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dest    netip.Addr
		vm      netip.Addr
		wantErr string
	}{
		{"InvalidDest", netip.Addr{}, netip.MustParseAddr("10.0.0.1"), "invalid destination"},
		{"InvalidVM", netip.MustParseAddr("10.0.0.1"), netip.Addr{}, "invalid destination"},
		{"FamilyMismatch_v4v6", netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("fd00::1"), "families must match"},
		{"FamilyMismatch_v6v4", netip.MustParseAddr("fd00::1"), netip.MustParseAddr("10.0.0.1"), "families must match"},
		{"MappedV4", netip.MustParseAddr("::ffff:192.168.1.1").Unmap(), netip.MustParseAddr("192.168.1.2"), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := SetupTunnel(context.Background(), tt.dest, tt.vm, "ipip")
			if tt.wantErr == "" {
				if err != nil && (strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "mismatch") || strings.Contains(err.Error(), "must match")) {
					t.Fatalf("expected no validation error, got: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			}
		})
	}
}

func TestSetupTunnel_WithoutRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dest string
		vm   string
		mode TunnelMode
	}{
		{"IPv4_IPIP", "10.0.0.1", "10.244.1.15", TunnelModeIPIP},
		{"IPv4_GRE", "10.0.0.1", "10.244.1.15", TunnelModeGRE},
		{"IPv6_IPIP", "fd00::1", "fd00::2", TunnelModeIPIP},
		{"IPv6_GRE", "fd00::1", "fd00::2", TunnelModeGRE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := SetupTunnel(context.Background(),
				netip.MustParseAddr(tt.dest),
				netip.MustParseAddr(tt.vm),
				tt.mode,
			)
			if err != nil && (strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "mismatch")) {
				t.Fatalf("should pass validation and fail at ip command, got: %v", err)
			}
		})
	}
}

func TestTeardownTunnel_NoTunnel(t *testing.T) {
	t.Parallel()
	err := TeardownTunnel(context.Background())
	if err != nil && strings.Contains(err.Error(), "invalid") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestSetupTunnel_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = SetupTunnel(ctx,
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("10.244.1.15"),
		"ipip",
	)
}
