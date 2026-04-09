package migration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"
)

// TunnelMode specifies the encapsulation protocol for the migration IP tunnel.
type TunnelMode string

const (
	// tunnelPrefix is the prefix for the IP tunnel interface name created
	// during migration to forward in-flight traffic from source to destination.
	// Each migration generates a unique suffix to support parallel migrations.
	tunnelPrefix = "mig-"

	// TunnelModeIPIP uses IPIP (IPv4) or IP6IP6 (IPv6). Minimal overhead.
	TunnelModeIPIP TunnelMode = "ipip"
	// TunnelModeGRE uses GRE (IPv4) or IP6GRE (IPv6). Supported by cloud middleboxes.
	TunnelModeGRE TunnelMode = "gre"
	// TunnelModeNone skips tunnel creation.
	TunnelModeNone TunnelMode = "none"
)

// generateTunnelName returns a unique tunnel interface name for this migration.
// Uses tunnelPrefix with a random hex suffix. The result is 14 characters,
// within the Linux IFNAMSIZ limit (15 chars + null terminator).
func generateTunnelName() (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating tunnel name: %w", err)
	}
	return tunnelPrefix + hex.EncodeToString(b[:]), nil // "mig-" (4) + 10 hex = 14 chars
}

// setupTunnel creates an IP tunnel to the destination node and installs
// a host route for the VM IP through it. This ensures packets arriving at the
// (now-stale) source during CNI convergence are forwarded to the destination.
//
// Both addresses must be the same family and already Unmap'd by the caller.
//
// The function is idempotent: any pre-existing tunnel with the same name is
// removed before creation to handle restarts or repeated invocations cleanly.
//
// On partial failure (e.g., route add fails after tunnel is created), the
// tunnel is cleaned up before returning the error to prevent resource leaks.
func setupTunnel(ctx context.Context, dest, vm netip.Addr, tunnelMode TunnelMode, tunnelName string) error {
	tunnelStart := time.Now()
	if !dest.IsValid() {
		return fmt.Errorf("invalid destination address: %s", dest)
	}
	if !vm.IsValid() {
		return fmt.Errorf("invalid VM address: %s", vm)
	}

	if dest.Is4() != vm.Is4() {
		return fmt.Errorf("destination (%s) and VM (%s) address families must match", dest, vm)
	}

	destStr := dest.String()
	vmStr := vm.String()

	// Remove any stale tunnel from a previous run. Errors are expected
	// (tunnel typically doesn't exist on first run) and silently ignored.
	// If there is a real problem (e.g., EPERM), it will surface when we
	// attempt to create the new tunnel below.
	cctx, ccancel := cleanupCtx(ctx)
	defer ccancel()
	if err := runCmd(cctx, "ip", "link", "del", tunnelName); err == nil {
		slog.Info("Removed stale tunnel from previous run", "tunnel", tunnelName)
	}

	// Create tunnel with the selected encapsulation mode.
	// ipip: ipip (v4) / ip6ip6 (v6) — minimal overhead, may be blocked by cloud VPCs.
	// gre:  gre  (v4) / ip6gre  (v6) — +4 bytes overhead, widely supported by middleboxes.
	var mode string
	switch {
	case tunnelMode == TunnelModeGRE && dest.Is6():
		mode = "ip6gre"
	case tunnelMode == TunnelModeGRE:
		mode = "gre"
	case dest.Is6():
		mode = "ip6ip6"
	default:
		mode = "ipip"
	}

	slog.Debug("Selected tunnel encapsulation", "mode", mode, "tunnel", tunnelName, "dest", destStr, "vm", vmStr)

	var err error
	if dest.Is6() {
		err = runCmd(ctx, "ip", "-6", "tunnel", "add", tunnelName,
			"mode", mode, "remote", destStr, "local", "::")
	} else {
		err = runCmd(ctx, "ip", "tunnel", "add", tunnelName,
			"mode", mode, "remote", destStr, "local", "any")
	}
	if err != nil {
		return fmt.Errorf("creating tunnel: %w", err)
	}

	if err := runCmd(ctx, "ip", "link", "set", tunnelName, "up"); err != nil {
		return errors.Join(fmt.Errorf("bringing up tunnel: %w", err), rollbackTunnel(ctx, tunnelName))
	}

	// Add host route: "ip route replace" for IPv4, "ip -6 route replace" for IPv6.
	// Use "replace" instead of "add" for idempotency — the VM IP may already
	// have a route via the local pod network on the source node.
	if vm.Is6() {
		err = runCmd(ctx, "ip", "-6", "route", "replace", vmStr, "dev", tunnelName)
	} else {
		err = runCmd(ctx, "ip", "route", "replace", vmStr, "dev", tunnelName)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("adding route for %s through tunnel: %w", vmStr, err), rollbackTunnel(ctx, tunnelName))
	}
	slog.Info("Tunnel setup complete", "tunnel", tunnelName, "mode", mode, "dest", destStr, "vm", vmStr, "elapsed", time.Since(tunnelStart).Round(time.Millisecond))
	return nil
}

// rollbackTunnel deletes the tunnel interface on partial setup failure.
// Returns the error (if any) for callers to combine with errors.Join.
func rollbackTunnel(ctx context.Context, tunnelName string) error {
	cctx, ccancel := cleanupCtx(ctx)
	defer ccancel()
	err := runCmd(cctx, "ip", "link", "del", tunnelName)
	if err != nil {
		slog.Warn("Failed to clean up tunnel", "tunnel", tunnelName, "error", err)
	}
	return err
}

// teardownTunnel removes the IP tunnel created during migration.
// Uses "ip link del" which works for all tunnel types (ipip, ip6ip6, gre, ip6gre).
// Deleting the tunnel implicitly removes the associated host route.
//
// Best-effort: all errors are logged as warnings but otherwise ignored,
// since this runs during cleanup where the tunnel may already be gone.
func teardownTunnel(ctx context.Context, tunnelName string) {
	if err := runCmd(ctx, "ip", "link", "del", tunnelName); err != nil {
		slog.Warn("Tunnel teardown failed", "tunnel", tunnelName, "error", err)
	}
}
