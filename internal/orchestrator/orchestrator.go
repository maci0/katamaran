// Package orchestrator coordinates a single live-migration: it takes a
// high-level Request, renders the source/destination Job manifests, applies
// them via the Kubernetes API, and reports Status back. It is the layer that
// the dashboard's HTTP handlers and the Migration CRD controller both consume.
//
// One implementation exists: the in-cluster client-go path
// (native.go). It renders the Jobs in-process, submits them via the
// apiserver, and reconciles status by polling Job conditions.
// Constructed via New and consumed
// by the dashboard, the Migration CRD controller, and the
// katamaran-orchestrator CLI.
//
// deploy/migrate.sh is a standalone bash wrapper for manual shell-driven
// testing. It applies the same Job templates via envsubst + kubectl and
// is not exercised through this package.
//
// The Request type is mode-agnostic: callers can specify either an explicit
// QMP socket path (legacy) or a pod identity (modern, lets the source job
// resolve sandbox/PID/IP at runtime). See Request.SourcePod in types.go.
//
// Determinism: poll's tickers, deadlines and status timestamps all come from
// the time package, so a test that wraps the call in a testing/synctest
// bubble virtualizes them, and a scripted apiserver then replays byte for
// byte (see replay_test.go). Migration IDs are the one input that bubble
// cannot virtualize: native.ids is the seam that injects a scripted source.
package orchestrator

import (
	"context"
	"errors"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// Orchestrator runs a single live migration to completion.
type Orchestrator interface {
	// Apply submits the migration jobs and returns immediately with a handle.
	// The migration runs asynchronously; use Watch to observe progress.
	Apply(ctx context.Context, req Request) (MigrationID, error)

	// Watch streams Status updates for the given migration until it reaches a
	// terminal state (Succeeded or Failed). The channel is closed by the
	// implementation when no more updates are coming.
	Watch(ctx context.Context, id MigrationID) (<-chan StatusUpdate, error)

	// Stop requests cancellation of an in-flight migration. Best-effort: the
	// caller should still Watch for the terminal state to confirm.
	Stop(ctx context.Context, id MigrationID) error

	// Resume re-attempts the post-Apply staging step (resolving the source
	// pod and submitting the destination Job in ReplayCmdline mode) for an
	// in-flight migration whose orchestrator-side state was lost (e.g. the
	// controller pod restarted between Apply returning and the staging
	// goroutine completing). Idempotent: returns (false, nil) when the
	// destination Job already exists, (true, nil) when this call actually
	// created it, and (false, err) when the source Job is missing or has
	// no reachable pod.
	Resume(ctx context.Context, id MigrationID, req Request) (created bool, err error)
}

// SourceJobName / DestJobName follow the rendered Job naming convention
// (`katamaran-source-<id>` / `katamaran-dest-<id>`). Exported so the
// controller's recovery path can construct the names from a Migration
// CR's .status.migrationID without round-tripping through label
// listing.
func SourceJobName(id MigrationID) string { return "katamaran-source-" + string(id) }
func DestJobName(id MigrationID) string   { return "katamaran-dest-" + string(id) }

// TerminalJobCondition returns the most recent terminal condition (Complete or Failed)
// on a Job, or "" if neither is set yet. Shared between orchestrator and controller.
func TerminalJobCondition(job *batchv1.Job) batchv1.JobConditionType {
	cond, ok := LatestTerminalJobCondition(job)
	if !ok {
		return ""
	}
	return cond.Type
}

// LatestTerminalJobCondition returns the most recent terminal Job condition
// (Complete or Failed) along with ok=false when no terminal condition is
// present. Used by the controller to surface Reason/Message fields that the
// type-only TerminalJobCondition discards.
func LatestTerminalJobCondition(job *batchv1.Job) (batchv1.JobCondition, bool) {
	var latest batchv1.JobCondition
	found := false
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue || (c.Type != batchv1.JobComplete && c.Type != batchv1.JobFailed) {
			continue
		}
		if !found || !c.LastTransitionTime.Time.Before(latest.LastTransitionTime.Time) {
			latest = c
			found = true
		}
	}
	return latest, found
}

// DefaultJobNamespace is the namespace where the native orchestrator creates Jobs.
const DefaultJobNamespace = "kube-system"

// MigrationID is the orchestrator's per-migration correlation handle. It is
// also propagated into the source/dest pods via the KATAMARAN_MIGRATION_ID
// env var so logs and metrics correlate end-to-end.
type MigrationID string

// ErrUnknownID is returned by Watch/Stop for a migration ID that the
// orchestrator does not know about (already finished + cleaned, or never
// started).
var ErrUnknownID = errors.New("unknown migration ID")

// MigrationIDLabel is the Kubernetes label key used to tag Jobs belonging
// to a specific migration, allowing the controller to find them.
const MigrationIDLabel = "katamaran.io/migration-id"

// SourcePodLabel tags every Job of a migration with the name of the pod
// being migrated. Apply uses it to find an in-flight migration for a pod
// before submitting a second set of Jobs against the same VM, so a
// duplicated request (dashboard retry, CR re-dispatch after a lost status
// patch) joins the running migration instead of restarting it.
const SourcePodLabel = "katamaran.io/source-pod"

// maxLabelValueLen is the Kubernetes limit for a label value. Pod names
// may be longer, so a source pod whose name does not fit is simply left
// unlabeled and gets no duplicate-submission protection.
const maxLabelValueLen = 63

// sourcePodLabelValue returns the SourcePodLabel value for req, or "" when
// the request has no source pod or its name is not a valid label value.
func sourcePodLabelValue(req Request) string {
	if req.SourcePod == nil {
		return ""
	}
	name := req.SourcePod.Name
	if name == "" || len(name) > maxLabelValueLen {
		return ""
	}
	return name
}

// DrainInBackground consumes ch until it closes. Callers that abandon a
// Watch stream early (error return, panic recovery) must keep draining:
// once the buffered updates fill, the orchestrator's poll goroutine blocks
// forever on its next send, pinning its inflight entry and poll/tail
// goroutines until process exit. On the normal path the channel is already
// closed by the time this runs and the drainer exits immediately.
func DrainInBackground(ch <-chan StatusUpdate) {
	go func() {
		for range ch {
		}
	}()
}
