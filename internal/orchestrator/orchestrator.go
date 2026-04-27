// Package orchestrator coordinates a single live-migration: it takes a
// high-level Request, renders the source/destination Job manifests, applies
// them via the Kubernetes API, and reports Status back. It is the layer that
// the dashboard's HTTP handlers and a future Migration controller both consume.
//
// Two implementations exist:
//
//   - Script (orchestrator/script.go): wraps deploy/migrate.sh. Behavioural
//     parity with what the dashboard does today. Used as the default until a
//     fully native implementation lands.
//
//   - Native (orchestrator/native.go, TODO): renders the Jobs in-process via
//     client-go and reconciles status from informers. The target for the
//     operator + a future shell-free dashboard image.
//
// The Request type is mode-agnostic: callers can specify either an explicit
// QMP socket path (legacy) or a pod identity (modern, lets the source job
// resolve sandbox/PID/IP at runtime). See PodPickerMode.
package orchestrator

import (
	"context"
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

// MigrationID is the orchestrator's per-migration correlation handle. It is
// also propagated into the source/dest pods via the KATAMARAN_MIGRATION_ID
// env var so logs and metrics correlate end-to-end.
type MigrationID string
