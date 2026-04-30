package orchestrator

import "time"

// Request is the high-level shape of a migration submission. It is consumed
// by both dashboard form posts and Migration CRD reconciliation.
//
// Source selection has two modes:
//
//   - SourcePod = pod-picker mode. The source job's resolver finds the kata
//     sandbox, QEMU PID, and pod IP at runtime via the in-cluster apiserver.
//     SourceQMP and VMIP may still be set as explicit overrides.
//
//   - SourceQMP + VMIP = legacy explicit mode. The caller already knows the
//     QMP socket path and pod IP.
//
// DestPod and DestQMP select an existing destination QEMU. In ReplayCmdline
// mode both are optional because the destination job can spawn QEMU itself.
type Request struct {
	// SourceNode is the Kubernetes node name where the source job runs.
	// Required.
	SourceNode string

	// DestNode is the Kubernetes node name where the destination job runs.
	// Required, must differ from SourceNode.
	DestNode string

	// SourcePod identifies the source pod (pod-picker mode). SourceQMP and
	// VMIP are optional overrides when SourcePod is set.
	SourcePod *PodRef

	// SourceQMP is an absolute path to the source QEMU's QMP unix socket.
	// Required with VMIP in legacy explicit mode; optional in pod-picker mode.
	SourceQMP string

	// VMIP is the source VM's pod IP. Required with SourceQMP in legacy
	// explicit mode; optional in pod-picker mode, where the resolver derives
	// it from the apiserver by default.
	VMIP string

	// DestPod identifies a kata pod on the destination node whose sandbox
	// QMP socket the destination job should connect to (pod-picker mode).
	// Optional in ReplayCmdline mode. If DestQMP is also set, DestQMP is an
	// explicit override.
	DestPod *PodRef

	// DestQMP is an absolute path to a destination-side QEMU QMP socket
	// (legacy mode or an override for DestPod resolution).
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

	// AutoDowntimeFloorMS is the lower bound (and additive overhead) for
	// the auto-calculated downtime: applied = max(rtt*multiplier + floor,
	// floor). Zero falls back to the source binary's compile-time default
	// (currently 25ms). Only consulted when AutoDowntime is true.
	AutoDowntimeFloorMS int

	// CNIConvergenceDelaySeconds is how long the source keeps the IP
	// tunnel alive after the cutover so the cluster's CNI can propagate
	// the pod's new node binding. Zero falls back to the source binary's
	// compile-time default (5s). Cilium / OVN-Kubernetes converge
	// sub-second; Calico / Flannel often want 5-10s.
	CNIConvergenceDelaySeconds int

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

	// AppliedDowntimeMS is the downtime limit the source binary
	// programmed into QEMU before starting RAM migration. Equal to the
	// caller-supplied value when AutoDowntime is false, or to the
	// auto-calculated rtt*multiplier+overhead value when true. Emitted
	// once at the start of the run via a separate StatusUpdate so the
	// dashboard / CR controller can surface the chosen number before
	// the cutover happens.
	AppliedDowntimeMS int64

	// RTTMS is the round-trip-time measurement that fed the auto-downtime
	// calculation. Zero when AutoDowntime is off or RTT measurement
	// failed.
	RTTMS int64

	// AutoDowntime mirrors Request.AutoDowntime so consumers know
	// whether AppliedDowntimeMS came from the auto-calc path.
	AutoDowntime bool
}
