package orchestrator

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// createSourceJob creates the source Job, treating an already-present Job of
// the same name as success. The name is derived from the migration ID, so an
// existing Job is this same submission whose Create response was lost after
// the API server persisted it, not a different migration. Reporting that as a
// failure would delete the dest Job (already running and listening for the
// migration stream) out from under a live migration. A genuine failure still
// removes the dest Job, which would otherwise run until activeDeadlineSeconds.
func (n *native) createSourceJob(ctx context.Context, job *batchv1.Job, destJobName string) error {
	_, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, job, metav1.CreateOptions{})
	switch {
	case err == nil || apierrors.IsAlreadyExists(err):
		return nil
	default:
		n.cleanupDestJob(ctx, destJobName, "source create failed; manual cleanup may be required")
		return fmt.Errorf("create source job: %w", err)
	}
}
