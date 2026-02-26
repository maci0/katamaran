package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Migration tuning constants.
const (
	// nbdPort is the TCP port used for NBD storage mirroring.
	nbdPort = "10809"

	// ramMigrationPort is the TCP port used for QEMU RAM migration.
	ramMigrationPort = "4444"

	// maxDowntimeMS is the maximum allowed VM pause duration in milliseconds.
	// QEMU will keep iterating RAM pre-copy rounds until the remaining dirty
	// pages can be transferred within this budget.
	maxDowntimeMS = 50

	// maxBandwidth is the maximum migration bandwidth in bytes/second (10 GB/s).
	// Set high to ensure the final dirty page flush completes as fast as possible.
	maxBandwidth = 10_000_000_000

	// qmpDialTimeout is the maximum time to wait for a QMP socket connection.
	qmpDialTimeout = 10 * time.Second

	// eventWaitTimeout is the maximum time to wait for a single QMP event
	// before assuming the migration has stalled.
	eventWaitTimeout = 30 * time.Minute

	// storagePollInterval is how often to check drive-mirror sync progress.
	storagePollInterval = 2 * time.Second

	// migrationPollInterval is how often to check RAM migration status.
	migrationPollInterval = 1 * time.Second

	// postMigrationTunnelDelay is how long to keep the IPIP tunnel alive
	// after migration completes, allowing the CNI control plane to converge.
	postMigrationTunnelDelay = 5 * time.Second

	// plugQdiscLimit is the packet buffer size for the tc sch_plug qdisc.
	plugQdiscLimit = "32768"

	// garpInitialMS is the initial delay before the first GARP announcement.
	garpInitialMS = 50

	// garpMaxMS is the maximum delay between GARP announcements.
	garpMaxMS = 550

	// garpRounds is the number of GARP/RARP announcement packets to send.
	garpRounds = 5

	// garpStepMS is the delay increase added after each announcement.
	garpStepMS = 100

	// tunnelName is the name of the IPIP tunnel interface created during
	// migration to forward in-flight traffic from source to destination.
	tunnelName = "mig-tun"

	// greetingTimeout is the maximum time to wait for the QMP greeting banner
	// during initial connection. If QEMU was started with -qmp wait=off, no
	// greeting is sent and we proceed after this timeout elapses.
	greetingTimeout = 1 * time.Second

	// qmpExecuteTimeout is the maximum time to wait for a synchronous QMP
	// command response. If QEMU becomes unresponsive mid-command, Execute()
	// returns a timeout error instead of blocking forever.
	qmpExecuteTimeout = 2 * time.Minute

	// migrationTimeout is the maximum wall-clock time allowed for the entire
	// RAM migration polling loop (query-migrate). Prevents infinite polling
	// if migration never reaches a terminal state (e.g., perpetual dirty page
	// churn with auto-converge unable to catch up).
	migrationTimeout = 1 * time.Hour

	// storageSyncTimeout is the maximum wall-clock time allowed for the
	// drive-mirror synchronization loop. Prevents infinite polling if the
	// mirror never converges (e.g., VM write rate exceeds mirror bandwidth).
	storageSyncTimeout = 2 * time.Hour

	// jobAppearTimeout is the maximum time to wait for a block job to appear
	// in query-block-jobs after being submitted. If it doesn't appear within
	// this window, the drive-mirror command likely failed silently.
	jobAppearTimeout = 30 * time.Second
)

// runCmd executes an external command. It captures stderr and returns a
// wrapped error including the full command line and stderr if the command fails.
// It respects the provided context for cancellation.
func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.Canceled {
			return fmt.Errorf("command cancelled: %s %v", name, args)
		}
		errMsg := strings.TrimSpace(out.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return fmt.Errorf("executing %s %v: %s", name, args, errMsg)
	}
	return nil
}
