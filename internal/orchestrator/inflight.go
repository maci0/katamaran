package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cleanupJobTimeout bounds each compensating Job delete issued by
// cleanupDestJob. Matches the per-cleanup budget migration.cleanupCtx uses.
const cleanupJobTimeout = 10 * time.Second

// cleanupDestJob best-effort deletes the dest Job after a setup error and
// logs a warning if the delete itself fails. Consolidates the cleanup
// branches scattered through the auto-select and dest-first code paths.
//
// The delete deliberately runs on a context that ignores the caller's
// cancellation (values preserved, deadline fresh): cancellation is itself one
// of the failures this compensation exists for: a Migration CR deleted while
// Apply is waiting for dest scheduling has no status.migrationID yet, so the
// controller's Stop path cannot reach these Jobs, and issuing the delete with
// the already-cancelled ctx would fail instantly and orphan the Job in the
// cluster until its activeDeadlineSeconds expires. Same pattern as
// migration.cleanupCtx.
func (n *native) cleanupDestJob(ctx context.Context, destJobName, reason string) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := n.client.BatchV1().Jobs(n.namespace).Delete(cctx, destJobName, metav1.DeleteOptions{}); err != nil {
		slog.Warn("failed to clean up dest job", "reason", reason, "dest_job", destJobName, "namespace", n.namespace, "error", err)
	}
}

// stampSourcePod records the migrated pod on the Job so inflightForSourcePod
// can find it again. Name and namespace are stamped as two labels because a
// label value cannot hold "namespace/name", and the name alone does not
// identify a pod across the cluster. Called by both render helpers.
func stampSourcePod(job *batchv1.Job, req Request) {
	if v := sourcePodLabelValue(req); v != "" {
		job.Labels[SourcePodLabel] = v
	}
	if v := sourcePodNamespaceLabelValue(req); v != "" {
		job.Labels[SourcePodNamespaceLabel] = v
	}
}

// createUnlessInFlight creates job unless a migration for req.SourcePod is
// already running, in which case it returns that migration's ID and creates
// nothing. The check and the create happen under one lock so two Apply
// calls racing in the same process cannot both pass the check. It is the
// first submit of every Apply branch, so once it returns the second Job
// (or the staging goroutine) is already visible to any later Apply.
func (n *native) createUnlessInFlight(ctx context.Context, req Request, job *batchv1.Job) (MigrationID, error) {
	n.dedupMu.Lock()
	defer n.dedupMu.Unlock()
	id, found, err := n.inflightForSourcePod(ctx, req)
	if err != nil || found {
		return id, err
	}
	if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return "", nil
}

// createDestJobIfAbsent creates the dest Job unless one with the same name is
// already there, reporting which of the two happened. Job names are derived
// from the migration ID, so an existing Job is this same staging step that
// already ran, not a different migration: a create that already succeeded
// and a retried staging pass both land here, and both must continue against
// the running Job rather than report a failure that would delete the source
// Job out from under a live migration. The Get also covers a Create whose
// response was lost after the API server persisted the object.
func (n *native) createDestJobIfAbsent(ctx context.Context, job *batchv1.Job) (bool, error) {
	_, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, job, metav1.CreateOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsAlreadyExists(err):
		return false, nil
	default:
		return false, fmt.Errorf("create dest job %s: %w", job.Name, err)
	}
}

// joinInFlight attaches this process to the migration already running for
// req.SourcePod under id, instead of starting a second set of Jobs against
// the same live VM. The Jobs already exist in the cluster, so the only work
// left is re-attaching the status watchers: when the migration was submitted
// by an earlier leader those goroutines are gone and Watch(id) would fail,
// and when it is still live startRun is a no-op.
func (n *native) joinInFlight(id MigrationID, req Request) MigrationID {
	slog.Info("Migration already in flight for source pod; joining the running migration", "migration_id", id, "source_pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name, "namespace", n.namespace)
	n.startRun(id, SourceJobName(id), DestJobName(id), req, nil, nil, true)
	return id
}

// inflightForSourcePod reports the ID of a still-running migration for
// req.SourcePod, if any. Both the name and the namespace label must match:
// Jobs from every namespace land in the same Job namespace, so a name-only
// lookup would hand one namespace's Migration the running migration of a
// same-named pod in another namespace, and that migration's status would then
// be written into the wrong Migration CR. It ignores Jobs that have reached a
// terminal condition (that migration is over, and a later request for the
// same pod name is a genuine new migration) and Jobs whose own
// activeDeadlineSeconds has elapsed (the kubelet killed their pod, so nothing
// is running and the guard would otherwise lock the pod out until the
// Objects are garbage-collected). That bounds the guard's state: it only
// ever sees Jobs the API server is already reaping via
// ttlSecondsAfterFinished.
func (n *native) inflightForSourcePod(ctx context.Context, req Request) (MigrationID, bool, error) {
	podName := sourcePodLabelValue(req)
	podNamespace := sourcePodNamespaceLabelValue(req)
	if podName == "" || podNamespace == "" {
		return "", false, nil
	}
	selector := SourcePodLabel + "=" + podName + "," + SourcePodNamespaceLabel + "=" + podNamespace
	jobs, err := n.client.BatchV1().Jobs(n.namespace).List(ctx, metav1.ListOptions{
		LabelSelector:   selector,
		ResourceVersion: "0",
	})
	if err != nil {
		// A guard that cannot read the cluster must not silently hand out a
		// fresh migration ID: that is exactly the duplicate this prevents.
		return "", false, fmt.Errorf("list jobs for source pod %s/%s: %w", podNamespace, podName, err)
	}
	now := time.Now()
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if TerminalJobCondition(job) != "" {
			continue
		}
		// A zero CreationTimestamp means the apiserver has not stamped the
		// object yet; only apply the deadline rule to a Job we can date.
		if created := job.CreationTimestamp.Time; !created.IsZero() {
			if deadline := job.Spec.ActiveDeadlineSeconds; deadline != nil &&
				now.Sub(created) > time.Duration(*deadline)*time.Second {
				continue
			}
		}
		if id := MigrationID(job.Labels[MigrationIDLabel]); id != "" {
			return id, true, nil
		}
	}
	return "", false, nil
}
