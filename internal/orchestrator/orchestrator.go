// Package orchestrator coordinates a single live-migration: it takes a
// high-level Request, renders the source/destination Job manifests, applies
// them via the Kubernetes API, and reports Status back. It is the layer that
// the dashboard's HTTP handlers and the Migration CRD controller both consume.
//
// One implementation exists: the in-cluster client-go path
// (native.go). It renders the Jobs in-process, submits them via the
// apiserver, and reconciles status by polling Job conditions.
// Constructed via New / NewFromKubeconfig / NewFromClient and consumed
// by the dashboard, the Migration CRD controller, and the
// katamaran-orchestrator CLI.
//
// A previous Script wrapper around deploy/migrate.sh was removed —
// migrate.sh stays in deploy/ for manual shell-driven testing only and
// is not exercised through this package.
//
// The Request type is mode-agnostic: callers can specify either an explicit
// QMP socket path (legacy) or a pod identity (modern, lets the source job
// resolve sandbox/PID/IP at runtime). See Request.SourcePod in types.go.
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
}

// TerminalJobCondition returns the most recent terminal condition (Complete or Failed)
// on a Job, or "" if neither is set yet. Shared between orchestrator and controller.
func TerminalJobCondition(job *batchv1.Job) batchv1.JobConditionType {
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
	if !found {
		return ""
	}
	return latest.Type
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
