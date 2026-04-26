// Package migration orchestrates zero-packet-drop live migration for
// Kata Containers with support for both shared and non-shared (NBD
// drive-mirror) storage.
package migration

import (
	"context"
	"net/netip"
	"time"
)

// Migration tuning constants.
const (
	// nbdPort is the TCP port used for NBD storage mirroring.
	nbdPort = "10809"

	// ramMigrationPort is the TCP port used for QEMU RAM migration.
	ramMigrationPort = "4444"

	// maxBandwidth is the maximum migration bandwidth in bytes/second (10 GB/s).
	// Set high to ensure the final dirty page flush completes as fast as possible.
	maxBandwidth = 10_000_000_000

	// eventWaitTimeout is the maximum time to wait for a single QMP event
	// before assuming the migration has stalled.
	eventWaitTimeout = 30 * time.Minute

	// storagePollInterval is how often to check drive-mirror sync progress.
	storagePollInterval = 2 * time.Second

	// migrationPollInterval is the interval for migration status polling.
	// Used both as the STOP event wait timeout and the query-migrate poll rate.
	migrationPollInterval = 1 * time.Second

	// postMigrationTunnelDelay is how long to keep the IP tunnel alive
	// after migration completes, allowing the CNI control plane to converge.
	postMigrationTunnelDelay = 5 * time.Second

	// plugQdiscLimit is the maximum number of bytes the tc sch_plug qdisc
	// will buffer before dropping. Passed as the "limit" argument to tc.
	plugQdiscLimit = "32768"

	// garpInitialMS is the initial delay before the first GARP announcement.
	garpInitialMS = 20

	// garpMaxMS is the maximum delay between GARP announcements.
	garpMaxMS = 550

	// garpRounds is the number of GARP/RARP announcement packets to send.
	garpRounds = 5

	// garpStepMS is the delay increase added after each announcement.
	garpStepMS = 100

	// migrationTimeout is the maximum wall-clock time allowed for the entire
	// RAM migration polling loop (query-migrate). Prevents infinite polling
	// if migration never converges (e.g., perpetual dirty page churn with
	// auto-converge unable to catch up).
	migrationTimeout = 1 * time.Hour

	// storageSyncTimeout is the maximum wall-clock time allowed for the
	// drive-mirror synchronization loop. Prevents infinite polling if the
	// mirror never converges (e.g., VM write rate exceeds mirror bandwidth).
	storageSyncTimeout = 2 * time.Hour

	// jobAppearTimeout is the maximum time to wait for a block job to appear
	// in query-block-jobs after being submitted. If it doesn't appear within
	// this window, the drive-mirror command likely failed silently.
	jobAppearTimeout = 30 * time.Second

	// DefaultMultifdChannels is the number of parallel TCP connections used
	// for RAM migration. Multifd distributes page transfer across channels,
	// improving throughput when per-connection bandwidth is limited (e.g.
	// nested KVM, high-latency links). Set to 0 to disable multifd.
	DefaultMultifdChannels = 4

	// cleanupTimeout is the deadline for deferred cleanup operations
	// (qdisc removal, NBD server stop, block-job-cancel, tunnel teardown).
	// Cleanup uses context.WithoutCancel to run even after main ctx cancel.
	cleanupTimeout = 10 * time.Second

	// rttMultiplier is the factor applied to measured RTT for auto-downtime
	// calculation. A value of 2 accounts for round-trip jitter and protocol
	// overhead during the final migration switchover.
	rttMultiplier = 2

	// rttMinOverheadMS is the minimum overhead in milliseconds added to the
	// RTT-based downtime estimate. Accounts for QEMU processing latency
	// that is independent of network RTT.
	rttMinOverheadMS = 10

	// rttDialTimeout is the maximum time to wait for each TCP handshake
	// when measuring round-trip time to the destination.
	rttDialTimeout = 5 * time.Second
)

// SourceConfig holds all parameters for RunSource.
type SourceConfig struct {
	QMPSocket       string
	DestIP          netip.Addr
	VMIP            netip.Addr
	DriveID         string
	SharedStorage   bool
	TunnelMode      TunnelMode
	DowntimeLimitMS int
	AutoDowntime    bool
	MultifdChannels int
}

// DestConfig holds all parameters for RunDestination.
type DestConfig struct {
	QMPSocket       string
	TapIface        string
	TapNetns        string
	DriveID         string
	SharedStorage   bool
	MultifdChannels int
}

// cleanupCtx returns a context with cleanupTimeout that is independent of the
// parent's cancellation state but inherits all its values.
//
// Uses context.WithoutCancel so cleanup operations are not aborted if the main
// context is cancelled (e.g. by SIGINT), while preserving parent context values.
func cleanupCtx(baseCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(baseCtx), cleanupTimeout)
}

// IPFamily returns a human-readable label for the IP address family.
func IPFamily(addr netip.Addr) string {
	if addr.Is4() {
		return "IPv4"
	}
	return "IPv6"
}
