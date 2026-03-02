package migration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strings"
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
// Both addresses must be the same family and already Unmap'd by the caller.
//
// The function is idempotent: any pre-existing tunnel with the same name is
// removed before creation to handle restarts or repeated invocations cleanly.
//
// On partial failure (e.g., route add fails after tunnel is created), the
// tunnel is cleaned up before returning the error to prevent resource leaks.
func SetupTunnel(ctx context.Context, dest, vm netip.Addr, tunnelMode string) error {
	dest = dest.Unmap()
	vm = vm.Unmap()

	if !dest.IsValid() || !vm.IsValid() {
		return fmt.Errorf("invalid destination or VM address")
	}

	if dest.Is4() != vm.Is4() {
		return fmt.Errorf("destination (%s) and VM (%s) address families must match", dest, vm)
	}

	destStr := dest.String()
	vmStr := vm.String()

	// Remove any stale tunnel from a previous run. Errors are ignored
	// because the tunnel may not exist, which is the common case.
	cctx, ccancel := CleanupCtx(ctx)
	if err := RunCmd(cctx, "ip", "link", "del", TunnelName); err == nil {
		log.Printf("Removed stale tunnel %s from previous run.", TunnelName)
	} else if err != nil && !strings.Contains(err.Error(), "Cannot find device") {
		log.Printf("Warning: failed to remove stale tunnel %s: %v", TunnelName, err)
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

	var err error
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
		cctx, ccancel := CleanupCtx(ctx)
		cleanupErr := RunCmd(cctx, "ip", "link", "del", TunnelName)
		ccancel()
		if cleanupErr != nil {
			log.Printf("failed to clean up tunnel after error: %v", cleanupErr)
		}
		return errors.Join(fmt.Errorf("bringing up tunnel: %w", err), cleanupErr)
	}

	// Add host route: "ip route replace" for IPv4, "ip -6 route replace" for IPv6.
	// Use "replace" instead of "add" for idempotency — the VM IP may already
	// have a route via the local pod network on the source node.
	if vm.Is6() {
		err = RunCmd(ctx, "ip", "-6", "route", "replace", vmStr, "dev", TunnelName)
	} else {
		err = RunCmd(ctx, "ip", "route", "replace", vmStr, "dev", TunnelName)
	}
	if err != nil {
		cctx, ccancel := CleanupCtx(ctx)
		cleanupErr := RunCmd(cctx, "ip", "link", "del", TunnelName)
		ccancel()
		if cleanupErr != nil {
			log.Printf("failed to clean up tunnel after error: %v", cleanupErr)
		}
		return errors.Join(fmt.Errorf("adding route for %s through tunnel: %w", vmStr, err), cleanupErr)
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
	err := RunCmd(ctx, "ip", "link", "del", TunnelName)
	if err != nil && !strings.Contains(err.Error(), "Cannot find device") {
		return fmt.Errorf("deleting tunnel %s: %w", TunnelName, err)
	}
	return nil
}
