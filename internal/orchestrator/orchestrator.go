// Package orchestrator coordinates a single live-migration: it takes a
// high-level Request, renders the source/destination Job manifests, applies
// them via the Kubernetes API, and reports Status back. It is the layer that
// the dashboard's HTTP handlers and the Migration CRD controller both consume.
//
// Two implementations exist:
//
//   - Native (native.go): renders the Jobs in-process via client-go and
//     reconciles status by polling Job conditions. Constructed via New /
//     NewFromKubeconfig / NewFromClient. Used by the dashboard and the
//     Migration CRD controller, and selected via --native in the
//     katamaran-orchestrator CLI.
//
//   - Script (script.go): wraps deploy/migrate.sh for ad-hoc CLI runs and
//     CI smoke. Constructed via NewScript. Default in
//     katamaran-orchestrator unless --native is passed; not used by the
//     dashboard or the CRD controller.
//
// The Request type is mode-agnostic: callers can specify either an explicit
// QMP socket path (legacy) or a pod identity (modern, lets the source job
// resolve sandbox/PID/IP at runtime). See Request.SourcePod in types.go.
package orchestrator

import (
	"context"
	"errors"

	batchv1 "k8s.io/api/batch/v1"
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
}

// TerminalJobCondition returns the most recent terminal condition (Complete or Failed)
// on a Job, or "" if neither is set yet. Shared between orchestrator and controller.
func TerminalJobCondition(job *batchv1.Job) batchv1.JobConditionType {
	for _, c := range job.Status.Conditions {
		if c.Status == "True" && (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) {
			return c.Type
		}
	}
	return ""
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
