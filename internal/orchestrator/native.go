package orchestrator

import (
	"context"
	"errors"
)

// Native is the placeholder for the future controller-runtime / client-go
// orchestrator. Once implemented, both the dashboard and a Migration CRD
// reconciler can use it without shelling out to migrate.sh or kubectl.
//
// Implementation roadmap (each can be a separate PR):
//
//  1. Render the source/dest Job manifests in-process from
//     deploy/job-source.yaml and deploy/job-dest.yaml templates.
//  2. Submit + watch the Jobs via a controller-runtime client.
//  3. Stream pod logs for the Logs UI without requiring kubectl exec/cp.
//  4. Replace the cmdline-staging stager-pod with a ConfigMap or downward
//     API mount.
//  5. Surface RAM-transfer progress by tailing katamaran's structured logs
//     and updating StatusUpdate.RAMTransferred / Phase.
//
// Until item 1 lands, NewNative returns ErrNotImplemented from every method.
type Native struct{}

// NewNative returns a placeholder Native orchestrator. All methods currently
// return ErrNotImplemented; switch to NewScript for the production path.
func NewNative() *Native { return &Native{} }

// ErrNotImplemented is returned by Native methods until the migration to a
// shell-free orchestrator is complete.
var ErrNotImplemented = errors.New("native orchestrator not implemented yet (use NewScript)")

func (n *Native) Apply(_ context.Context, _ Request) (MigrationID, error) {
	return "", ErrNotImplemented
}

func (n *Native) Watch(_ context.Context, _ MigrationID) (<-chan StatusUpdate, error) {
	return nil, ErrNotImplemented
}

func (n *Native) Stop(_ context.Context, _ MigrationID) error {
	return ErrNotImplemented
}
