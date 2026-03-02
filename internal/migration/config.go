// Package migration orchestrates zero-packet-drop live migration for
// Kata Containers with support for both shared and non-shared (NBD
// drive-mirror) storage.
package migration

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

// Migration tuning constants.
const (
	// NBDPort is the TCP port used for NBD storage mirroring.
	NBDPort = "10809"

	// RAMMigrationPort is the TCP port used for QEMU RAM migration.
	RAMMigrationPort = "4444"

	// MaxBandwidth is the maximum migration bandwidth in bytes/second (10 GB/s).
	// Set high to ensure the final dirty page flush completes as fast as possible.
	MaxBandwidth = 10_000_000_000

	// EventWaitTimeout is the maximum time to wait for a single QMP event
	// before assuming the migration has stalled.
	EventWaitTimeout = 30 * time.Minute

	// StoragePollInterval is how often to check drive-mirror sync progress.
	StoragePollInterval = 2 * time.Second

	// MigrationPollInterval is how often to check RAM migration status.
	MigrationPollInterval = 1 * time.Second

	// PostMigrationTunnelDelay is how long to keep the IP tunnel alive
	// after migration completes, allowing the CNI control plane to converge.
	PostMigrationTunnelDelay = 5 * time.Second

	// PlugQdiscLimit is the packet buffer size for the tc sch_plug qdisc.
	PlugQdiscLimit = "32768"

	// GARPInitialMS is the initial delay before the first GARP announcement.
	GARPInitialMS = 20

	// GARPMaxMS is the maximum delay between GARP announcements.
	GARPMaxMS = 550

	// GARPRounds is the number of GARP/RARP announcement packets to send.
	GARPRounds = 5

	// GARPStepMS is the delay increase added after each announcement.
	GARPStepMS = 100

	// TunnelName is the name of the IP tunnel interface created during
	// migration to forward in-flight traffic from source to destination.
	TunnelName = "mig-tun"

	// MigrationTimeout is the maximum wall-clock time allowed for the entire
	// RAM migration polling loop (query-migrate). Prevents infinite polling
	// if migration never converges (e.g., perpetual dirty page churn with
	// auto-converge unable to catch up).
	MigrationTimeout = 1 * time.Hour

	// StorageSyncTimeout is the maximum wall-clock time allowed for the
	// drive-mirror synchronization loop. Prevents infinite polling if the
	// mirror never converges (e.g., VM write rate exceeds mirror bandwidth).
	StorageSyncTimeout = 2 * time.Hour

	// JobAppearTimeout is the maximum time to wait for a block job to appear
	// in query-block-jobs after being submitted. If it doesn't appear within
	// this window, the drive-mirror command likely failed silently.
	JobAppearTimeout = 30 * time.Second

	// CleanupTimeout is the deadline for deferred cleanup operations
	// (qdisc removal, NBD server stop, block-job-cancel, tunnel teardown).
	// Cleanup uses context.WithoutCancel to run even after main ctx cancel.
	CleanupTimeout = 10 * time.Second
)

// CleanupCtx returns a context with CleanupTimeout that is independent of the
// parent's cancellation state but inherits all its values.
//
// ARCHITECTURE UPDATE (Phase 1/2): Trade-off in cleanup routines
// We use context.WithoutCancel(baseCtx) instead of context.Background().
// This prevents deferred cleanups (qdisc removal, NBD stop, block-job-cancel,
// tunnel teardown) from being aborted if the main context is cancelled
// (e.g. by SIGINT or early error), while preserving logging traces, metrics,
// and other values attached to the parent context.
func CleanupCtx(baseCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(baseCtx), CleanupTimeout)
}

// FormatQEMUHost returns the IP address formatted for use in QEMU's
// colon-delimited URIs (e.g., nbd:host:port, tcp:host:port). IPv6 addresses
// are wrapped in square brackets to avoid ambiguity with URI field separators.
// IPv4 addresses are returned unchanged.
func FormatQEMUHost(addr netip.Addr) string {
	s := addr.String()
	if addr.Is6() && !addr.Is4In6() {
		return "[" + s + "]"
	}
	return s
}

// RunCmdInNetns executes a command inside the given network namespace.
// If netnsPath is empty, it runs the command in the current namespace.
func RunCmdInNetns(ctx context.Context, netnsPath string, name string, args ...string) error {
	if netnsPath == "" {
		return RunCmd(ctx, name, args...)
	}
	nsArgs := append([]string{"--net=" + netnsPath, name}, args...)
	return RunCmd(ctx, "nsenter", nsArgs...)
}

// RunCmd executes an external command. It captures combined stdout/stderr and
// returns a wrapped error including the full command line and output on failure.
// If the context was cancelled, the returned error wraps context.Canceled so
// callers can detect graceful shutdown with errors.Is(err, context.Canceled).
func RunCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("command cancelled: %s %v: %w", name, args, ctx.Err())
		}
		errMsg := strings.TrimSpace(out.String())
		if errMsg == "" {
			return fmt.Errorf("executing %s %v: %w", name, args, err)
		}
		return fmt.Errorf("executing %s %v: %s: %w", name, args, errMsg, err)
	}
	return nil
}
