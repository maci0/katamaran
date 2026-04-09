package migration

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

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

func TestSetupTunnel_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dest    netip.Addr
		vm      netip.Addr
		wantErr string
	}{
		{"InvalidDest", netip.Addr{}, netip.MustParseAddr("10.0.0.1"), "invalid destination address:"},
		{"InvalidVM", netip.MustParseAddr("10.0.0.1"), netip.Addr{}, "invalid VM address:"},
		{"FamilyMismatch_v4v6", netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("fd00::1"), "families must match"},
		{"FamilyMismatch_v6v4", netip.MustParseAddr("fd00::1"), netip.MustParseAddr("10.0.0.1"), "families must match"},
		{"MappedV4", netip.MustParseAddr("::ffff:192.168.1.1").Unmap(), netip.MustParseAddr("192.168.1.2"), ""},
	}

	for i, tt := range tests {
		tunnelName := fmt.Sprintf("tv%d", i)
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer teardownTunnel(context.Background(), tunnelName)
			err := setupTunnel(context.Background(), tt.dest, tt.vm, TunnelModeIPIP, tunnelName)
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

	for i, tt := range tests {
		tunnelName := fmt.Sprintf("tw%d", i)
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer teardownTunnel(context.Background(), tunnelName)
			err := setupTunnel(context.Background(),
				netip.MustParseAddr(tt.dest),
				netip.MustParseAddr(tt.vm),
				tt.mode,
				tunnelName,
			)
			if err != nil && (strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "mismatch") || strings.Contains(err.Error(), "must match")) {
				t.Fatalf("should pass validation and fail at ip command, got: %v", err)
			}
		})
	}
}

func TestTeardownTunnel_NoTunnel(t *testing.T) {
	t.Parallel()
	// teardownTunnel is best-effort and never panics, even with no tunnel.
	teardownTunnel(context.Background(), "test-tun")
}

func TestSetupTunnel_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	defer teardownTunnel(context.Background(), "tstcc")
	err := setupTunnel(ctx,
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("10.244.1.15"),
		TunnelModeIPIP,
		"tstcc",
	)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestRollbackTunnel_NoTunnel(t *testing.T) {
	t.Parallel()
	err := rollbackTunnel(context.Background(), "nonexistent-tun")
	if err == nil {
		t.Fatal("expected error for nonexistent tunnel")
	}
}
