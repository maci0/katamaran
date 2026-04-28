package orchestrator

import "time"

// Request is the high-level shape of a migration submission. It is consumed
// by both dashboard form posts and Migration CRD reconciliation.
//
// One of SourcePod or SourceQMP must be supplied, but not both:
//
//   - SourcePod = pod-picker mode. The source job's resolver finds the kata
//     sandbox, QEMU PID, and pod IP at runtime via the in-cluster apiserver.
//
//   - SourceQMP = legacy explicit mode. The caller already knows the QMP
//     socket path and supplies VMIP separately.
//
// DestPod / DestQMP work the same way for the destination side.
type Request struct {
	// SourceNode is the Kubernetes node name where the source job runs.
	// Required.
	SourceNode string

	// DestNode is the Kubernetes node name where the destination job runs.
	// Required, must differ from SourceNode.
	DestNode string

	// SourcePod identifies the source pod (pod-picker mode). Mutually
	// exclusive with SourceQMP+VMIP.
	SourcePod *PodRef

	// SourceQMP is an absolute path to the source QEMU's QMP unix socket
	// (legacy mode). Mutually exclusive with SourcePod.
	SourceQMP string

	// VMIP is the source VM's pod IP (legacy mode only). When SourcePod is
	// set, the source resolver derives this from the apiserver.
	VMIP string

	// DestPod identifies a kata pod on the destination node whose sandbox
	// QMP socket the destination job should connect to (pod-picker mode).
	// Mutually exclusive with DestQMP. Optional in ReplayCmdline mode.
	DestPod *PodRef

	// DestQMP is an absolute path to a destination-side QEMU QMP socket
	// (legacy mode). Mutually exclusive with DestPod.
	DestQMP string

	// DestIP is the destination node IP that source QEMU connects to for
	// the migration TCP stream. Required.
	DestIP string

	// Image is the katamaran container image used for both source and dest
	// jobs. Required.
	Image string

	// SharedStorage skips the NBD drive-mirror phase. Set true when source
	// and dest share a storage backend (Ceph, NFS).
	SharedStorage bool

	// ReplayCmdline enables capture-and-replay of the source QEMU command
	// line on the destination node, so the dest job spawns its own QEMU
	// with -incoming defer instead of connecting to a kata-shim sandbox.
	// When true, DestPod is optional.
	ReplayCmdline bool

	// TunnelMode is the migration network tunnel encapsulation: "ipip",
	// "gre", or "none". Defaults to "ipip" when empty.
	TunnelMode string

	// DowntimeMS is the maximum allowed VM pause in milliseconds.
	// Defaults to 25 when zero.
	DowntimeMS int

	// AutoDowntime calculates the downtime budget from the source-to-dest
	// RTT instead of using the fixed DowntimeMS value.
	AutoDowntime bool

	// MultifdChannels enables parallel RAM-migration TCP channels. Zero
	// disables multifd. Both source and destination must agree on the count.
	MultifdChannels int

	// TapIface is the destination tap interface to buffer with tc sch_plug.
	// Defaults to "tap0_kata" in pod-picker mode.
	TapIface string

	// TapNetns is the network namespace path containing TapIface, e.g.
	// "/proc/<qemu_pid>/ns/net". Defaults derived from the resolved QEMU
	// PID in pod-picker mode.
	TapNetns string

	// LogLevel and LogFormat are passed through to the source/dest binaries.
	// Empty means "use the binary default".
	LogLevel  string
	LogFormat string

	// KubectlContext, when non-empty, is passed to kubectl invocations made
	// by the orchestrator. The native orchestrator ignores it (uses its own
	// in-cluster client).
	KubectlContext string
}

// PodRef identifies a Kubernetes pod by namespace + name.
type PodRef struct {
	Namespace string
	Name      string
}

// StatusPhase enumerates the orchestrator's view of a migration lifecycle.
type StatusPhase string

const (
	PhaseSubmitted    StatusPhase = "submitted"
	PhaseDestStarting StatusPhase = "dest-starting"
	PhaseSrcStarting  StatusPhase = "src-starting"
	PhaseTransferring StatusPhase = "transferring"
	PhaseCutover      StatusPhase = "cutover"
	PhaseSucceeded    StatusPhase = "succeeded"
	PhaseFailed       StatusPhase = "failed"
)

// IsTerminal reports whether p is a terminal state (no further updates
// should follow on the watch channel).
func (p StatusPhase) IsTerminal() bool {
	return p == PhaseSucceeded || p == PhaseFailed
}

// StatusUpdate is a single point-in-time observation of a running migration.
// Implementations emit one update per state transition plus periodic
// transferring updates.
type StatusUpdate struct {
	ID    MigrationID
	Phase StatusPhase
	When  time.Time

	// Message is a human-readable progress note. Empty for routine updates.
	Message string

	// Error is set on PhaseFailed.
	Error error

	// RAMTransferred / RAMTotal are populated during PhaseTransferring and
	// in the final PhaseSucceeded update. Zero before transfer begins.
	RAMTransferred int64
	RAMTotal       int64

	// DowntimeMS is set in the final PhaseSucceeded update — the actual VM
	// pause duration measured by QEMU's query-migrate.
	DowntimeMS int64
}
