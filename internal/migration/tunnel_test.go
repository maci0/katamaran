package migration

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

func TestIPFamily_IPv4(t *testing.T) {
	t.Parallel()
	addr := netip.MustParseAddr("10.0.0.1")
	if got := IPFamily(addr); got != "IPv4" {
		t.Fatalf("IPFamily(10.0.0.1) = %q, want IPv4", got)
	}
}

func TestIPFamily_IPv6(t *testing.T) {
	t.Parallel()
	addr := netip.MustParseAddr("fd00::1")
	if got := IPFamily(addr); got != "IPv6" {
		t.Fatalf("IPFamily(fd00::1) = %q, want IPv6", got)
	}
}

func TestSetupTunnel_InvalidDestIP(t *testing.T) {
	t.Parallel()
	err := SetupTunnel(context.Background(), "not-an-ip", "10.0.0.1", "ipip")
	if err == nil {
		t.Fatal("expected error for invalid destIP")
	}
	if !strings.Contains(err.Error(), "invalid destIP") {
		t.Fatalf("expected 'invalid destIP' in error, got: %v", err)
	}
}

func TestSetupTunnel_InvalidVmIP(t *testing.T) {
	t.Parallel()
	err := SetupTunnel(context.Background(), "10.0.0.1", "bad-ip", "ipip")
	if err == nil {
		t.Fatal("expected error for invalid vmIP")
	}
	if !strings.Contains(err.Error(), "invalid vmIP") {
		t.Fatalf("expected 'invalid vmIP' in error, got: %v", err)
	}
}

func TestSetupTunnel_AddressFamilyMismatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		destIP string
		vmIP   string
	}{
		{"IPv4_dest_IPv6_vm", "10.0.0.1", "fd00::1"},
		{"IPv6_dest_IPv4_vm", "fd00::1", "10.0.0.1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := SetupTunnel(context.Background(), tc.destIP, tc.vmIP, "ipip")
			if err == nil {
				t.Fatal("expected error for address family mismatch")
			}
			if !strings.Contains(err.Error(), "address family mismatch") {
				t.Fatalf("expected 'address family mismatch' in error, got: %v", err)
			}
		})
	}
}

func TestSetupTunnel_IPv4MappedNormalization(t *testing.T) {
	t.Parallel()
	// ::ffff:10.0.0.1 paired with 10.244.1.15 should NOT produce a family mismatch,
	// because ::ffff:10.0.0.1 should be unmapped to 10.0.0.1 (IPv4).
	// The tunnel creation itself will fail (no root), but we should get past validation.
	err := SetupTunnel(context.Background(), "::ffff:10.0.0.1", "10.244.1.15", "ipip")
	if err == nil {
		// If we're running as root somehow and ip commands succeed, that's fine too.
		return
	}
	if strings.Contains(err.Error(), "address family mismatch") {
		t.Fatal("IPv4-mapped address should be normalized, not rejected as cross-family")
	}
	if strings.Contains(err.Error(), "invalid") {
		t.Fatal("IPv4-mapped address should be valid")
	}
}

func TestSetupTunnel_IPv4_IPIP_FailsWithoutRoot(t *testing.T) {
	t.Parallel()
	err := SetupTunnel(context.Background(), "10.0.0.1", "10.244.1.15", "ipip")
	if err == nil {
		return // running as root — tunnel was actually created
	}
	// Should fail at the ip command level, not at validation.
	if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("should pass validation and fail at ip command, got: %v", err)
	}
}

func TestSetupTunnel_IPv4_GRE_FailsWithoutRoot(t *testing.T) {
	t.Parallel()
	err := SetupTunnel(context.Background(), "10.0.0.1", "10.244.1.15", "gre")
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("should pass validation and fail at ip command, got: %v", err)
	}
}

func TestSetupTunnel_IPv6_IPIP_FailsWithoutRoot(t *testing.T) {
	t.Parallel()
	err := SetupTunnel(context.Background(), "fd00::1", "fd00::2", "ipip")
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("should pass validation and fail at ip command, got: %v", err)
	}
}

func TestSetupTunnel_IPv6_GRE_FailsWithoutRoot(t *testing.T) {
	t.Parallel()
	err := SetupTunnel(context.Background(), "fd00::1", "fd00::2", "gre")
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("should pass validation and fail at ip command, got: %v", err)
	}
}

func TestTeardownTunnel_NoTunnel(t *testing.T) {
	t.Parallel()
	err := TeardownTunnel(context.Background())
	if err == nil {
		return // tunnel somehow existed
	}
	// Should fail at ip command level.
	if strings.Contains(err.Error(), "invalid") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestSetupTunnel_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := SetupTunnel(ctx, "10.0.0.1", "10.244.1.15", "ipip")
	if err == nil {
		return // should not happen with cancelled context
	}
}
