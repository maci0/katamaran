// Package migration orchestrates zero-packet-drop live migration for
// Kata Containers with support for both shared and non-shared (NBD
// drive-mirror) storage.
package migration

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"
)

// Migration tuning constants.
const (
	// nbdPort is the TCP port used for NBD storage mirroring.
	nbdPort = "10809"

	// ramMigrationPort is the TCP port used for QEMU RAM migration.
	ramMigrationPort = "4444"

	// DefaultQMPSocket is the CLI's default --qmp value for source mode.
	// RunSource treats this exact value (or empty) as "no explicit
	// override" and replaces it with the sandbox-resolved socket path,
	// so the katamaran CLI default must stay identical to it.
	DefaultQMPSocket = "/run/vc/vm/extra-monitor.sock"

	// DestDefaultQMPSocket is the well-known placeholder socket path the
	// orchestrator bakes into the dest Job when no explicit --qmp-dest is
	// provided. RunDestination overrides it (and empty) with the real
	// sandbox-derived path at runtime, so the rendered Job template value
	// must stay identical to it.
	DestDefaultQMPSocket = "/run/vc/vm/katamaran-dest/qmp.sock"

	// extraMonitorSocketName is the per-sandbox filename Kata binds the
	// QEMU monitor socket to under /run/vc/vm/<sandbox>/.
	extraMonitorSocketName = "extra-monitor.sock"

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
	// that is independent of network RTT, plus enough headroom for
	// kata-agent / kernel page-dirtying so an idle VM still converges.
	// 25ms matches the manual --downtime default (1 frame at 40Hz).
	rttMinOverheadMS = 25

	// rttDialTimeout is the maximum time to wait for each ICMP echo reply
	// when measuring round-trip time to the destination.
	rttDialTimeout = 5 * time.Second
)

// CmdlineHostDir is the hostPath mount the source job uses to capture
// /proc/<qemu>/cmdline before emitting it to its pod log as a
// KATAMARAN_CMDLINE_B64 marker. The dest binary materializes fetched
// cmdlines under the same directory. Var (not const) so tests can point
// it at a temp dir instead of the shared hostPath.
var CmdlineHostDir = "/tmp/katamaran-cmdlines"

// SourceConfig holds all parameters for RunSource.
type SourceConfig struct {
	QMPSocket       string
	DestIP          netip.Addr
	VMIP            netip.Addr
	DriveIDs        []string
	SharedStorage   bool
	TunnelMode      TunnelMode
	DowntimeLimitMS int
	AutoDowntime    bool
	// AutoDowntimeFloorMS overrides rttMinOverheadMS (and the implicit
	// minimum on the auto-calculated downtime). Zero uses the package
	// default. Ignored when AutoDowntime is false.
	AutoDowntimeFloorMS int
	// CNIConvergenceDelay overrides the post-cutover wait that keeps the
	// IP tunnel alive while the cluster's CNI propagates the pod's new
	// node assignment. Zero uses the package default (5s). Cilium /
	// OVN-Kubernetes converge in <1s; Calico/Flannel may need 5-10s.
	CNIConvergenceDelay time.Duration
	MultifdChannels     int
	// PodName and PodNamespace are an alternative to QMPSocket+VMIP: when set,
	// the source binary resolves the pod's sandbox container at runtime to
	// derive the QMP socket path and VM IP. Consumed by the migration package.
	PodName      string
	PodNamespace string
	// EmitCmdlineTo, when non-empty, instructs the source binary to capture
	// /proc/<qemu_pid>/cmdline (NUL bytes converted to newlines) and write
	// it to this path before kicking off the migration. The destination
	// job consumes the cmdline via DestConfig.ReplayCmdlineFile or
	// ReplayCmdlineFromPod to spawn an identical QEMU with -incoming
	// defer, since Kata 3.27 has no knob to inject extra QEMU args. On
	// success the source prints two stdout markers: KATAMARAN_CMDLINE_AT=
	// (path on the source node, used by deploy/migrate.sh) and
	// KATAMARAN_CMDLINE_B64= (base64 payload, scraped from the pod log by
	// the Native orchestrator).
	EmitCmdlineTo string
}

// DestConfig holds all parameters for RunDestination.
type DestConfig struct {
	QMPSocket       string
	TapIface        string
	TapNetns        string
	DriveIDs        []string
	SharedStorage   bool
	MultifdChannels int
	// DestPodName and DestPodNamespace are an alternative to QMPSocket: when set,
	// the destination binary resolves the pod's sandbox container at runtime to
	// derive the QMP socket path. Symmetric to SourceConfig.PodName.
	DestPodName      string
	DestPodNamespace string
	// ReplayCmdlineFile, when non-empty, points at a file containing the
	// source QEMU's /proc/<pid>/cmdline (NUL→newline). The destination
	// binary transforms this cmdline (sandbox-path substitutions, strip
	// readonly=on on the nvdimm backend, drop existing -daemonize and
	// -incoming, append -incoming defer), spawns the QEMU itself inside
	// the dest container's network namespace, then drives the migration
	// via QMP as usual. Used because Kata 3.27 cannot start QEMU with
	// -incoming defer (kata-shim kills VMs that fail the vsock dial).
	ReplayCmdlineFile string
	// ReplayCmdlineFromPod, when non-empty, names the source pod
	// (`<namespace>/<name>`) whose log carries the KATAMARAN_CMDLINE_B64
	// marker. The dest binary fetches the pod's log via the in-cluster
	// apiserver, decodes the marker, writes a local tmp file, and falls
	// through to the file-based replay path. When both this and
	// ReplayCmdlineFile are set, the pod-log marker wins; orchestrators
	// normally set exactly one.
	ReplayCmdlineFromPod string
	// QEMUBinary, when non-empty, overrides the QEMU binary path used for
	// cmdline replay. Defaults to the bundled Kata QEMU for the current
	// architecture (/opt/kata/bin/qemu-system-x86_64 on amd64,
	// /opt/kata/bin/qemu-system-aarch64 on arm64); the captured cmdline's
	// argv[0] is intentionally not used (it is attacker-controlled in the
	// source pod). Mostly a test seam.
	QEMUBinary string
	// SandboxID, when non-empty, names the synthetic dest sandbox directory
	// created under /run/vc/vm/<id>/ for cmdline replay. Defaults to
	// "katamaran-dest". Also used to substitute the source sandbox path in
	// the captured cmdline.
	SandboxID string
	// SourcePodRef, when non-empty, is "<namespace>/<name>" of the source
	// pod. Used to fetch VMConfig from the source pod's log markers for
	// factory adoption. Set from --pod-name/--pod-namespace on the dest.
	SourcePodRef string
}

// kataSBSRoot is where Kata stages per-sandbox state directories, each
// holding the persist.json both VMConfig emission (source) and
// migration-meta enrichment (dest) read.
const kataSBSRoot = "/run/vc/sbs"

// KataPersistConfig is the subset of Kata's per-sandbox persist.json
// Config object shared by every reader of that file: the source/dest
// migration paths and the katamaran-factory VMConfig loader. Keep field
// tags identical to Kata's marshalling.
type KataPersistConfig struct {
	HypervisorType   string          `json:"HypervisorType"`
	HypervisorConfig json.RawMessage `json:"HypervisorConfig"`
	KataAgentConfig  json.RawMessage `json:"KataAgentConfig"`
}

// cleanupCtx returns a context with cleanupTimeout that is independent of the
// parent's cancellation state but inherits all its values.
//
// Uses context.WithoutCancel so cleanup operations are not aborted if the main
// context is cancelled (e.g. by SIGINT), while preserving parent context values.
func cleanupCtx(baseCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(baseCtx), cleanupTimeout)
}

// MarshalVMConfig builds the JSON blob matching Kata's VMConfig.ToGrpc shape
// from the pieces stored in a sandbox persist.json. The marshal cannot fail for
// these string/RawMessage inputs, so the error is dropped.
func MarshalVMConfig(hypervisorType string, hypervisorConfig, agentConfig json.RawMessage) []byte {
	b, _ := json.Marshal(map[string]any{
		"HypervisorType":   hypervisorType,
		"HypervisorConfig": hypervisorConfig,
		"AgentConfig":      agentConfig,
	})
	return b
}

// IPFamily returns a human-readable label for the IP address family.
func IPFamily(addr netip.Addr) string {
	if addr.Is4() {
		return "IPv4"
	}
	return "IPv6"
}

// formatQEMUHost returns the IP address formatted for use in QEMU's
// colon-delimited URIs (e.g., nbd:host:port, tcp:host:port). IPv6 addresses
// are wrapped in square brackets to avoid ambiguity with URI field separators.
// IPv4 addresses (including IPv4-mapped IPv6) are returned unchanged.
func formatQEMUHost(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return "[" + addr.String() + "]"
	}
	return addr.String()
}
