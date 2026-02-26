package main

import (
	"context"
	"fmt"
	"log"
	"net/netip"
)

// setupIPIPTunnel creates an IPIP tunnel to the destination node and installs
// a host route for the VM IP through it. This ensures packets arriving at the
// (now-stale) source during CNI convergence are forwarded to the destination.
//
// The function is idempotent: any pre-existing tunnel with the same name is
// removed before creation to handle restarts or repeated invocations cleanly.
//
// On partial failure (e.g., route add fails after tunnel is created), the
// tunnel is cleaned up before returning the error to prevent resource leaks.
func setupIPIPTunnel(ctx context.Context, destIP, vmIP string) error {
	// Strictly validate IPs to prevent shell injection or invalid route configs.
	if _, err := netip.ParseAddr(destIP); err != nil {
		return fmt.Errorf("invalid destIP %q: %w", destIP, err)
	}
	if _, err := netip.ParseAddr(vmIP); err != nil {
		return fmt.Errorf("invalid vmIP %q: %w", vmIP, err)
	}
	// Remove any stale tunnel from a previous run. Errors are ignored
	// because the tunnel may not exist, which is the common case.
	if err := runCmd(ctx, "ip", "tunnel", "del", tunnelName); err == nil {
		log.Printf("Removed stale IPIP tunnel %s from previous run.", tunnelName)
	}

	if err := runCmd(ctx, "ip", "tunnel", "add", tunnelName, "mode", "ipip", "remote", destIP, "local", "any"); err != nil {
		return fmt.Errorf("creating tunnel: %w", err)
	}
	if err := runCmd(ctx, "ip", "link", "set", tunnelName, "up"); err != nil {
		_ = runCmd(context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second), "ip", "tunnel", "del", tunnelName)
		return fmt.Errorf("bringing up tunnel: %w", err)
	}
	if err := runCmd(ctx, "ip", "route", "add", vmIP, "dev", tunnelName); err != nil {
		_ = runCmd(context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second), "ip", "tunnel", "del", tunnelName)
		return fmt.Errorf("adding route for %s through tunnel: %w", vmIP, err)
	}
	return nil
}

// teardownIPIPTunnel removes the IPIP tunnel created during migration.
// Deleting the tunnel implicitly removes the associated host route.
func teardownIPIPTunnel(ctx context.Context) error {
	if err := runCmd(ctx, "ip", "tunnel", "del", tunnelName); err != nil {
		return fmt.Errorf("deleting tunnel %s: %w", tunnelName, err)
	}
	return nil
}
