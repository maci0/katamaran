package migration

import (
	"context"
	"fmt"
	"log"
	"net/netip"
)

// SetupTunnel creates an IP tunnel to the destination node and installs
// a host route for the VM IP through it. This ensures packets arriving at the
// (now-stale) source during CNI convergence are forwarded to the destination.
//
// tunnelMode selects the encapsulation protocol:
//   - "ipip": IPIP for IPv4 (mode ipip), ip6tnl for IPv6 (mode ip6ip6).
//     Minimal overhead but may be blocked by cloud VPC security groups.
//   - "gre": GRE for IPv4 (mode gre), ip6gre for IPv6. Widely supported
//     by cloud middleboxes (AWS, GCP, Azure) at +4 bytes overhead.
//
// Mixed address families (e.g., IPv4 destIP with IPv6 vmIP) are rejected.
//
// The function is idempotent: any pre-existing tunnel with the same name is
// removed before creation to handle restarts or repeated invocations cleanly.
//
// On partial failure (e.g., route add fails after tunnel is created), the
// tunnel is cleaned up before returning the error to prevent resource leaks.
func SetupTunnel(ctx context.Context, destIP, vmIP, tunnelMode string) error {
	// Strictly validate IPs to prevent shell injection or invalid route configs.
	dest, err := netip.ParseAddr(destIP)
	if err != nil {
		return fmt.Errorf("invalid destIP %q: %w", destIP, err)
	}
	vm, err := netip.ParseAddr(vmIP)
	if err != nil {
		return fmt.Errorf("invalid vmIP %q: %w", vmIP, err)
	}

	// Normalize IPv4-mapped IPv6 addresses (e.g., ::ffff:10.0.0.1 → 10.0.0.1).
	// Without this, an IPv4-mapped address would be misclassified as IPv6,
	// creating a broken ip6ip6/ip6gre tunnel to what is actually an IPv4 host.
	dest = dest.Unmap()
	vm = vm.Unmap()

	// Use the normalized string representations for ip commands, since
	// Unmap() may have changed the textual form.
	destStr := dest.String()
	vmStr := vm.String()

	// Both addresses must be the same IP family. Cross-family tunnels
	// (e.g., IPv4-in-IPv6 via ip4ip6) are not supported.
	if dest.Is4() != vm.Is4() {
		return fmt.Errorf("address family mismatch: destIP %q is %s but vmIP %q is %s",
			destIP, IPFamily(dest), vmIP, IPFamily(vm))
	}

	// Remove any stale tunnel from a previous run. Errors are ignored
	// because the tunnel may not exist, which is the common case.
	cctx, ccancel := CleanupCtx()
	if err := RunCmd(cctx, "ip", "link", "del", TunnelName); err == nil {
		log.Printf("Removed stale tunnel %s from previous run.", TunnelName)
	}
	ccancel()

	// Create tunnel with the selected encapsulation mode.
	// ipip: ipip (v4) / ip6ip6 (v6) — minimal overhead, may be blocked by cloud VPCs.
	// gre:  gre  (v4) / ip6gre  (v6) — +4 bytes overhead, widely supported by middleboxes.
	var mode string
	switch {
	case tunnelMode == "gre" && dest.Is6():
		mode = "ip6gre"
	case tunnelMode == "gre":
		mode = "gre"
	case dest.Is6():
		mode = "ip6ip6"
	default:
		mode = "ipip"
	}

	if dest.Is6() {
		err = RunCmd(ctx, "ip", "-6", "tunnel", "add", TunnelName,
			"mode", mode, "remote", destStr, "local", "::")
	} else {
		err = RunCmd(ctx, "ip", "tunnel", "add", TunnelName,
			"mode", mode, "remote", destStr, "local", "any")
	}
	if err != nil {
		return fmt.Errorf("creating tunnel: %w", err)
	}

	if err := RunCmd(ctx, "ip", "link", "set", TunnelName, "up"); err != nil {
		cctx, ccancel := CleanupCtx()
		_ = RunCmd(cctx, "ip", "link", "del", TunnelName)
		ccancel()
		return fmt.Errorf("bringing up tunnel: %w", err)
	}

	// Add host route: "ip route add" for IPv4, "ip -6 route add" for IPv6.
	if vm.Is6() {
		err = RunCmd(ctx, "ip", "-6", "route", "add", vmStr, "dev", TunnelName)
	} else {
		err = RunCmd(ctx, "ip", "route", "add", vmStr, "dev", TunnelName)
	}
	if err != nil {
		cctx, ccancel := CleanupCtx()
		_ = RunCmd(cctx, "ip", "link", "del", TunnelName)
		ccancel()
		return fmt.Errorf("adding route for %s through tunnel: %w", vmStr, err)
	}
	return nil
}

// IPFamily returns a human-readable label for the IP address family.
func IPFamily(addr netip.Addr) string {
	if addr.Is4() {
		return "IPv4"
	}
	return "IPv6"
}

// TeardownTunnel removes the IP tunnel created during migration.
// Uses "ip link del" which works for all tunnel types (ipip, ip6tnl, gre, ip6gre).
// Deleting the tunnel implicitly removes the associated host route.
func TeardownTunnel(ctx context.Context) error {
	if err := RunCmd(ctx, "ip", "link", "del", TunnelName); err != nil {
		return fmt.Errorf("deleting tunnel %s: %w", TunnelName, err)
	}
	return nil
}
