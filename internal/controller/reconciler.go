// Package controller implements a minimal Kubernetes controller for the
// Migration CRD (katamaran.io/v1alpha1). It uses the dynamic client +
// a polling reconcile loop to keep the dependency footprint small (no
// controller-runtime, no codegen).
//
// Lifecycle:
//
//  1. Reconciler periodically lists Migration resources cluster-wide.
//  2. For each new Migration (no .status.phase), translate .spec into
//     orchestrator.Request, call orchestrator.Apply, update .status with
//     the assigned migrationID. A goroutine consumes Watch events and
//     patches .status.phase on each update.
//  3. For each Migration in a non-terminal phase that the controller is
//     not currently tracking (e.g. after a controller restart), poll the
//     underlying source/dest Jobs directly to determine the outcome and
//     patch .status.phase accordingly.
//  4. For each Migration with a DeletionTimestamp set, call
//     orchestrator.Stop on its tracked migrationID, then remove the
//     finalizer so kube-apiserver can finish deleting the CR.
//
// This is the operator-grade equivalent of the dashboard's POST
// /api/migrate flow. Both consume the same orchestrator.Request type so
// behavioural drift between UI submissions and CRD-driven submissions is
// impossible.
package controller

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// Process-wide expvar counters surfaced by katamaran-mgr's /metrics
// (Prometheus text-format) and /debug/vars (JSON) endpoints. Plain
// expvar (no Prometheus client dep) keeps the mgr image small; the
// /metrics handler walks the expvar registry in-process.
var (
	mDispatched      = expvar.NewInt("katamaran_migrations_dispatched_total")
	mSucceeded       = expvar.NewInt("katamaran_migrations_succeeded_total")
	mFailed          = expvar.NewInt("katamaran_migrations_failed_total")
	mRecovered       = expvar.NewInt("katamaran_migrations_recovered_total")
	mResumed         = expvar.NewInt("katamaran_migrations_resumed_total")
	mDeleted         = expvar.NewInt("katamaran_migrations_deleted_total")
	mInflight        = expvar.NewInt("katamaran_migrations_inflight")
	mReconcileErrors = expvar.NewInt("katamaran_migrations_reconcile_errors_total")
	mStatusPatchErrs = expvar.NewInt("katamaran_migrations_status_patch_errors_total")
	mWatchLost       = expvar.NewInt("katamaran_migrations_watch_lost_total")
	mWorkerPanics    = expvar.NewInt("katamaran_migrations_worker_panics_total")
)

// migrationProgress tracks per-migration progress for Prometheus export.
// Keyed by migration ID. Entries are added on dispatch and removed when
// the dispatch goroutine exits. The /metrics handler iterates this map
// via MigrationProgressSnapshot and emits labeled gauges.
var migrationProgress sync.Map // map[string]MigrationProgressEntry

// MigrationProgressEntry holds per-migration metrics for Prometheus export.
type MigrationProgressEntry struct {
	Phase             string
	RAMTransferred    int64
	RAMTotal          int64
	DowntimeMS        int64
	AppliedDowntimeMS int64
	RTTMS             int64
}

func updateProgressMetrics(u orchestrator.StatusUpdate) {
	id := string(u.ID)
	if id == "" {
		return
	}
	var e MigrationProgressEntry
	if v, ok := migrationProgress.Load(id); ok {
		e = v.(MigrationProgressEntry)
	}
	e.Phase = string(u.Phase)
	if u.RAMTransferred > 0 {
		e.RAMTransferred = u.RAMTransferred
	}
	if u.RAMTotal > 0 {
		e.RAMTotal = u.RAMTotal
	}
	if u.DowntimeMS > 0 {
		e.DowntimeMS = u.DowntimeMS
	}
	if u.AppliedDowntimeMS > 0 {
		e.AppliedDowntimeMS = u.AppliedDowntimeMS
	}
	if u.RTTMS > 0 {
		e.RTTMS = u.RTTMS
	}
	migrationProgress.Store(id, e)
}

// MigrationProgressSnapshot returns a point-in-time copy of all tracked
// migration progress entries, for use by the /metrics handler.
func MigrationProgressSnapshot() map[string]MigrationProgressEntry {
	out := make(map[string]MigrationProgressEntry)
	migrationProgress.Range(func(k, v any) bool {
		out[k.(string)] = v.(MigrationProgressEntry)
		return true
	})
	return out
}

// MigrationGVR is the GroupVersionResource the controller reconciles.
var MigrationGVR = schema.GroupVersionResource{
	Group:    "katamaran.io",
	Version:  "v1alpha1",
	Resource: "migrations",
}

// finalizerName guards against deletion of a Migration CR while the
// underlying Jobs are still running. Reconcile removes it after
// orchestrator.Stop has been called on the tracked migrationID.
const finalizerName = "katamaran.io/finalizer"

// Reconciler watches Migration resources and submits each pending one to
// the embedded orchestrator. Status is patched back to the CR as the
// orchestrator emits StatusUpdate events.
type Reconciler struct {
	Dynamic       dynamic.Interface
	Kube          kubernetes.Interface // optional; enables restart recovery via direct Job inspection
	Orchestrator  orchestrator.Orchestrator
	Discoverer    orchestrator.Discoverer // resolves source node + dest IP from the spec
	PollInterval  time.Duration
	StatusTimeout time.Duration

	mu       sync.Mutex
	tracking map[types.NamespacedName]*track // migrations currently being watched
}

// track holds the per-migration state the controller needs to handle
// CR deletion: the assigned migrationID (so we can call Stop) and a
// cancel func for the Watch goroutine.
type track struct {
	id     orchestrator.MigrationID
	cancel context.CancelFunc
}

// NewReconciler builds a reconciler with sensible defaults.
func NewReconciler(dyn dynamic.Interface, kube kubernetes.Interface, orch orchestrator.Orchestrator, disc orchestrator.Discoverer) *Reconciler {
	return &Reconciler{
		Dynamic:       dyn,
		Kube:          kube,
		Orchestrator:  orch,
		Discoverer:    disc,
		PollInterval:  5 * time.Second,
		StatusTimeout: 30 * time.Minute,
		tracking:      map[types.NamespacedName]*track{},
	}
}

// Run blocks until ctx is cancelled. It periodically polls the Migration
// resources and dispatches new ones to the orchestrator.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.tickOnce(ctx)
		}
	}
}

// tickOnce runs a single reconcileAll pass with panic recovery. A panic in
// the reconcile loop would otherwise kill the goroutine and the controller
// would silently stop reconciling without any liveness signal change.
func (r *Reconciler) tickOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			mWorkerPanics.Add(1)
			slog.Error("reconcile tick panic", "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	if err := r.reconcileAll(ctx); err != nil {
		mReconcileErrors.Add(1)
		slog.Error("reconcile loop failed", "error", err)
	}
}

func (r *Reconciler) reconcileAll(ctx context.Context) error {
	list, err := r.Dynamic.Resource(MigrationGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Migrations: %w", err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}

		// Deletion path first — runs even when phase is set.
		if obj.GetDeletionTimestamp() != nil {
			r.handleDeletion(ctx, key, obj)
			continue
		}

		// Ensure the finalizer is present before we touch any state, so
		// a race between submit and delete cannot orphan jobs.
		if !hasFinalizer(obj) {
			if err := r.addFinalizer(ctx, obj); err != nil {
				slog.Error("add finalizer failed", "migration", key, "error", err)
				continue
			}
		}

		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		switch {
		case phase == "":
			// Brand-new migration, dispatch.
			if !r.markTracking(key) {
				continue
			}
			go r.dispatch(ctx, key, obj)
		case !orchestrator.StatusPhase(phase).IsTerminal():
			// In-flight from a previous controller incarnation. Recover
			// by inspecting Job state directly.
			if r.isTracked(key) {
				continue
			}
			if !r.markTracking(key) {
				continue
			}
			go r.recover(ctx, key, obj)
		}
	}
	return nil
}

// markTracking returns true if the caller is the first to claim key.
// Subsequent calls return false until the goroutine clears tracking.
func (r *Reconciler) markTracking(key types.NamespacedName) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tracking[key]; ok {
		return false
	}
	r.tracking[key] = &track{}
	return true
}

func (r *Reconciler) isTracked(key types.NamespacedName) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tracking[key]
	return ok
}

func (r *Reconciler) untrack(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tracking, key)
}

func (r *Reconciler) updateTrack(key types.NamespacedName, id orchestrator.MigrationID, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tracking[key]; ok {
		t.id = id
		t.cancel = cancel
	}
}

func (r *Reconciler) dispatch(ctx context.Context, key types.NamespacedName, obj *unstructured.Unstructured) {
	mInflight.Add(1)
	defer mInflight.Add(-1)
	defer r.untrack(key)
	defer func() {
		if rec := recover(); rec != nil {
			mWorkerPanics.Add(1)
			slog.Error("Migration dispatch panic", "migration", key, "panic", rec, "stack", string(debug.Stack()))
		}
	}()

	slog.Info("Dispatching new Migration", "migration", key)
	req, err := specToRequest(obj.Object)
	if err != nil {
		slog.Warn("Migration spec invalid", "migration", key, "error", err)
		r.patchFailedStatus(ctx, key, "", "invalid spec", err.Error())
		return
	}
	if req.SourcePod != nil {
		if r.Discoverer == nil {
			slog.Error("Migration cannot be resolved: discoverer unavailable", "migration", key)
			r.patchFailedStatus(ctx, key, "", "resolve migration", "discoverer unavailable")
			return
		}
		lookupCtx, lookupCancel := context.WithTimeout(ctx, 30*time.Second)
		srcNode, lerr := r.Discoverer.LookupPodNode(lookupCtx, req.SourcePod.Namespace, req.SourcePod.Name)
		if lerr != nil || srcNode == "" {
			lookupCancel()
			if lerr == nil {
				lerr = fmt.Errorf("source pod node is empty")
			}
			slog.Error("Resolve source pod node failed", "migration", key, "source_pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name, "error", lerr)
			r.patchFailedStatus(ctx, key, "", "resolve source pod node", lerr.Error())
			return
		}
		req.SourceNode = srcNode

		if req.DestNode == "" {
			// Auto-select mode: copy the source pod's scheduling
			// constraints so the dest Job lands on a compatible node.
			sched, lerr := r.Discoverer.LookupPodScheduling(lookupCtx, req.SourcePod.Namespace, req.SourcePod.Name)
			lookupCancel()
			if lerr != nil {
				slog.Error("Resolve source pod scheduling failed", "migration", key, "source_pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name, "error", lerr)
				r.patchFailedStatus(ctx, key, "", "resolve source pod scheduling", lerr.Error())
				return
			}
			// Merge source pod nodeSelector into any CRD-level destNodeSelector.
			if len(sched.NodeSelector) > 0 {
				if req.DestNodeSelector == nil {
					req.DestNodeSelector = make(map[string]string, len(sched.NodeSelector))
				}
				for k, v := range sched.NodeSelector {
					if _, exists := req.DestNodeSelector[k]; !exists {
						req.DestNodeSelector[k] = v
					}
				}
			}
			req.DestTolerations = sched.Tolerations
			// DestIP will be resolved after the dest Job pod is scheduled
			// (inside native.Apply's auto-select path).
		} else {
			destIP, lerr := r.Discoverer.LookupNodeInternalIP(lookupCtx, req.DestNode)
			lookupCancel()
			if lerr != nil || destIP == "" {
				if lerr == nil {
					lerr = fmt.Errorf("destination node InternalIP is empty")
				}
				slog.Error("Resolve destination node IP failed", "migration", key, "dest_node", req.DestNode, "error", lerr)
				r.patchFailedStatus(ctx, key, "", "resolve dest node IP", lerr.Error())
				return
			}
			req.DestIP = destIP
			if req.SourceNode == req.DestNode {
				slog.Warn("Migration spec invalid: source pod already on destination node", "migration", key, "node", req.SourceNode)
				r.patchFailedStatus(ctx, key, "", "invalid spec", "source pod already runs on destNode")
				return
			}
		}
	}
	jobCtx, cancel := context.WithTimeout(ctx, r.StatusTimeout)
	defer cancel()
	id, err := r.Orchestrator.Apply(jobCtx, req)
	if err != nil {
		slog.Error("Apply failed", "migration", key, "error", err)
		r.patchFailedStatus(ctx, key, "", "Apply failed", err.Error())
		return
	}
	mDispatched.Add(1)
	r.updateTrack(key, id, cancel)
	defer migrationProgress.Delete(string(id))
	slog.Info("Migration submitted", "migration", key, "migration_id", id, "source_node", req.SourceNode, "dest_node", req.DestNode)
	_ = r.patchStatus(ctx, key, string(id), string(orchestrator.PhaseSubmitted), "submitted to orchestrator", "")

	updates, err := r.Orchestrator.Watch(jobCtx, id)
	if err != nil {
		slog.Error("Watch failed", "migration", key, "migration_id", id, "error", err)
		r.patchFailedStatus(ctx, key, string(id), "Watch failed", err.Error())
		return
	}
	var lastPhase string
	for u := range updates {
		errStr := ""
		if u.Error != nil {
			errStr = u.Error.Error()
		}
		_ = r.patchStatusUpdate(ctx, key, u, errStr)
		lastPhase = string(u.Phase)
		updateProgressMetrics(u)
	}
	switch lastPhase {
	case string(orchestrator.PhaseSucceeded):
		mSucceeded.Add(1)
	case string(orchestrator.PhaseFailed):
		mFailed.Add(1)
	default:
		mFailed.Add(1)
		mWatchLost.Add(1)
		msg := "watch closed without terminal status"
		if lastPhase != "" {
			msg += " (last phase " + lastPhase + ")"
		}
		slog.Error("Migration watch closed without terminal status", "migration", key, "migration_id", id, "last_phase", lastPhase)
		_ = r.patchStatus(ctx, key, string(id), string(orchestrator.PhaseFailed), msg, "")
	}
	if lastPhase == string(orchestrator.PhaseSucceeded) && req.SourceCleanup != "" && req.SourceCleanup != "none" {
		if req.SourcePod != nil && r.Discoverer != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			switch req.SourceCleanup {
			case "delete":
				if err := r.Discoverer.DeletePod(cleanupCtx, req.SourcePod.Namespace, req.SourcePod.Name); err != nil {
					slog.Warn("Source pod delete failed (migration succeeded)", "pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name, "error", err)
				} else {
					slog.Info("Source pod deleted", "pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name)
				}
			case "orphan":
				if err := r.Discoverer.OrphanAndDeletePod(cleanupCtx, req.SourcePod.Namespace, req.SourcePod.Name); err != nil {
					slog.Warn("Source pod orphan+delete failed (migration succeeded)", "pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name, "error", err)
				} else {
					slog.Info("Source pod orphaned and deleted", "pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name)
				}
			}
		}
	}
	if lastPhase == string(orchestrator.PhaseSucceeded) && req.AdoptVM {
		if req.SourcePod != nil {
			adoptCtx, adoptCancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer adoptCancel()
			adoptName := "adopted-" + string(id)[:8]
			destNode := req.DestNode
			// If auto-scheduled, resolve dest node from the dest job
			if destNode == "" {
				// Try to find the dest job's node
				slog.Warn("AdoptVM with auto-scheduled dest: dest node unknown, skipping adoption", "migration", key, "migration_id", id)
			} else {
				// Wait for the factory to load VMConfig from the migration
				// state or a sandbox persist.json before creating the pod.
				slog.Info("Waiting for factory VMConfig before adoption", "migration", key, "migration_id", id, "delay", "5s")
				time.Sleep(5 * time.Second)
				if err := r.createAdoptionPod(adoptCtx, req, adoptName, destNode); err != nil {
					slog.Warn("Failed to create adoption pod", "migration", key, "migration_id", id, "name", adoptName, "node", destNode, "error", err)
				} else {
					slog.Info("Adoption pod created", "migration", key, "migration_id", id, "name", adoptName, "node", destNode)
				}
			}
		}
	}
	slog.Info("Migration finished", "migration", key, "migration_id", id, "final_phase", lastPhase)
}

// recover reattaches to a Migration left in a non-terminal phase by a
// previous controller incarnation. It polls the source/dest Jobs in
// kube-system (located by the katamaran.io/migration-id label) and
// patches .status.phase based on their conditions.
func (r *Reconciler) recover(ctx context.Context, key types.NamespacedName, obj *unstructured.Unstructured) {
	mRecovered.Add(1)
	mInflight.Add(1)
	defer mInflight.Add(-1)
	defer r.untrack(key)
	defer func() {
		if rec := recover(); rec != nil {
			mWorkerPanics.Add(1)
			slog.Error("Migration recover panic", "migration", key, "panic", rec, "stack", string(debug.Stack()))
		}
	}()

	id, _, _ := unstructured.NestedString(obj.Object, "status", "migrationID")
	slog.Info("Recovering in-flight Migration after controller restart", "migration", key, "migration_id", id)

	if r.Kube == nil {
		slog.Warn("Recovery skipped: no Kube clientset wired", "migration", key)
		_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "controller restarted; recovery unavailable", "")
		return
	}
	if id == "" {
		slog.Error("Recovery failed: no migration ID on status", "migration", key)
		_ = r.patchStatus(ctx, key, "", string(orchestrator.PhaseFailed), "recovery: no migrationID on status", "")
		return
	}

	selector := orchestrator.MigrationIDLabel + "=" + id
	deadline := time.Now().Add(r.StatusTimeout)
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			slog.Error("Recovery timed out waiting for jobs", "migration", key, "migration_id", id, "timeout", r.StatusTimeout)
			_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovery timed out waiting for jobs", "")
			return
		}
		jobs, err := r.Kube.BatchV1().Jobs(orchestrator.DefaultJobNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			slog.Error("recover: list jobs failed", "migration", key, "migration_id", id, "error", err)
			continue
		}
		var src, dest *batchv1.Job
		for i := range jobs.Items {
			j := &jobs.Items[i]
			switch j.Labels["app.kubernetes.io/component"] {
			case "source":
				src = j
			case "dest":
				dest = j
			}
		}
		if dest != nil {
			if cond := orchestrator.TerminalJobCondition(dest); cond == batchv1.JobComplete {
				slog.Info("Recovery completed from destination job", "migration", key, "migration_id", id, "dest_job", dest.Name)
				_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseSucceeded), "recovered: dest job complete", "")
				return
			} else if cond == batchv1.JobFailed {
				detail, attrs := jobFailureDetails(dest)
				attrs = append([]any{"migration", key, "migration_id", id, "dest_job", dest.Name}, attrs...)
				slog.Error("Recovery failed from destination job", attrs...)
				_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovered: dest job failed", detail)
				return
			}
		}
		if dest == nil && src != nil && orchestrator.TerminalJobCondition(src) == batchv1.JobFailed {
			detail, attrs := jobFailureDetails(src)
			attrs = append([]any{"migration", key, "migration_id", id, "source_job", src.Name}, attrs...)
			slog.Error("Recovery failed from source job before destination started", attrs...)
			_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovered: source job failed before dest started", detail)
			return
		}
		// Source still running but dest never got created — orchestrator
		// staging goroutine died with the previous controller leader. Re-attempt
		// staging via Orchestrator.Resume; subsequent reconcile ticks will see
		// the new dest Job and fall through to the normal terminal-condition
		// branches above. Resume is idempotent, so calling it on every tick is
		// safe — the counter only bumps on actual create.
		//
		// Pass the spec-derived Request directly: Resume only consults
		// req.ReplayCmdline + the dest-side fields (DestNode, Image, DestQMP)
		// when rendering the dest Job. SourceNode / DestIP are already baked
		// into the running source Job's argv, so the Discoverer round-trip
		// the dispatch path uses would be dead work here.
		if dest == nil && src != nil && orchestrator.TerminalJobCondition(src) == "" {
			req, sErr := specToRequest(obj.Object)
			if sErr != nil {
				slog.Warn("recover: specToRequest failed; cannot resume", "migration", key, "error", sErr)
				continue
			}
			created, rErr := r.Orchestrator.Resume(ctx, orchestrator.MigrationID(id), req)
			switch {
			case rErr != nil:
				slog.Warn("recover: Resume failed; will retry next tick", "migration", key, "migration_id", id, "error", rErr)
			case created:
				mResumed.Add(1)
				slog.Info("Recovery: triggered Resume to create destination job", "migration", key, "migration_id", id, "source_job", src.Name)
			}
		}
		if dest == nil && src == nil {
			slog.Error("Recovery failed: source and destination jobs disappeared", "migration", key, "migration_id", id)
			_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovered: source/dest jobs disappeared", "")
			return
		}
	}
}

func jobFailureDetails(job *batchv1.Job) (string, []any) {
	cond, ok := orchestrator.LatestTerminalJobCondition(job)
	if !ok {
		return "", nil
	}
	details := make([]string, 0, 2)
	attrs := make([]any, 0, 4)
	if cond.Reason != "" {
		details = append(details, "reason="+cond.Reason)
		attrs = append(attrs, "reason", cond.Reason)
	}
	if cond.Message != "" {
		details = append(details, "message="+cond.Message)
		attrs = append(attrs, "message", cond.Message)
	}
	return strings.Join(details, " "), attrs
}

// handleDeletion runs when the user has issued `kubectl delete migration`.
// We call orchestrator.Stop to clean up any in-flight Jobs, then patch
// the CR to remove the finalizer so kube-apiserver can finish deleting.
func (r *Reconciler) handleDeletion(ctx context.Context, key types.NamespacedName, obj *unstructured.Unstructured) {
	if !hasFinalizer(obj) {
		return // nothing to do; kube-apiserver already finished deleting
	}
	id, _, _ := unstructured.NestedString(obj.Object, "status", "migrationID")
	slog.Info("Migration deleted; stopping orchestrator + removing finalizer", "migration", key, "migration_id", id)
	if id != "" {
		stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := r.Orchestrator.Stop(stopCtx, orchestrator.MigrationID(id)); err != nil {
			slog.Warn("Stop failed; removing finalizer anyway to unblock deletion", "migration", key, "migration_id", id, "error", err)
		}
		cancel()
	}
	r.mu.Lock()
	if t, ok := r.tracking[key]; ok && t.cancel != nil {
		t.cancel()
	}
	delete(r.tracking, key)
	r.mu.Unlock()
	if err := r.removeFinalizer(ctx, obj); err != nil {
		slog.Error("remove finalizer failed", "migration", key, "error", err)
	} else {
		mDeleted.Add(1)
	}
}

// hasFinalizer returns true if the Migration carries our finalizer.
func hasFinalizer(obj *unstructured.Unstructured) bool {
	for _, f := range obj.GetFinalizers() {
		if f == finalizerName {
			return true
		}
	}
	return false
}

// patchFinalizers issues a merge-patch overwriting the Migration's
// metadata.finalizers slice.
func (r *Reconciler) patchFinalizers(ctx context.Context, obj *unstructured.Unstructured, finalizers []string) error {
	patch := map[string]any{"metadata": map[string]any{"finalizers": finalizers}}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = r.Dynamic.Resource(MigrationGVR).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}

// addFinalizer patches the Migration to carry our finalizer.
func (r *Reconciler) addFinalizer(ctx context.Context, obj *unstructured.Unstructured) error {
	return r.patchFinalizers(ctx, obj, append(obj.GetFinalizers(), finalizerName))
}

// removeFinalizer patches the Migration to drop our finalizer. Other
// finalizers (if any) are preserved.
func (r *Reconciler) removeFinalizer(ctx context.Context, obj *unstructured.Unstructured) error {
	out := make([]string, 0, len(obj.GetFinalizers()))
	for _, f := range obj.GetFinalizers() {
		if f != finalizerName {
			out = append(out, f)
		}
	}
	return r.patchFinalizers(ctx, obj, out)
}

// specToRequest extracts the .spec fields into an orchestrator.Request.
func specToRequest(obj map[string]any) (orchestrator.Request, error) {
	var req orchestrator.Request
	srcNS, _, _ := unstructured.NestedString(obj, "spec", "sourcePod", "namespace")
	srcName, _, _ := unstructured.NestedString(obj, "spec", "sourcePod", "name")
	if srcNS == "" || srcName == "" {
		return req, fmt.Errorf("spec.sourcePod.{namespace,name} are required")
	}
	req.SourcePod = &orchestrator.PodRef{Namespace: srcNS, Name: srcName}

	if dstNS, found, _ := unstructured.NestedString(obj, "spec", "destPod", "namespace"); found && dstNS != "" {
		dstName, _, _ := unstructured.NestedString(obj, "spec", "destPod", "name")
		req.DestPod = &orchestrator.PodRef{Namespace: dstNS, Name: dstName}
	}

	req.DestNode, _, _ = unstructured.NestedString(obj, "spec", "destNode")
	req.Image, _, _ = unstructured.NestedString(obj, "spec", "image")
	if req.Image == "" {
		return req, fmt.Errorf("spec.image is required")
	}
	if ns, found, _ := unstructured.NestedStringMap(obj, "spec", "destNodeSelector"); found {
		req.DestNodeSelector = ns
	}
	req.SharedStorage, _, _ = unstructured.NestedBool(obj, "spec", "sharedStorage")
	req.ReplayCmdline, _, _ = unstructured.NestedBool(obj, "spec", "replayCmdline")
	req.TunnelMode, _, _ = unstructured.NestedString(obj, "spec", "tunnelMode")
	if dt, found, _ := unstructured.NestedInt64(obj, "spec", "downtimeMS"); found {
		req.DowntimeMS = int(dt)
	}
	req.AutoDowntime, _, _ = unstructured.NestedBool(obj, "spec", "autoDowntime")
	if floor, found, _ := unstructured.NestedInt64(obj, "spec", "autoDowntimeFloorMS"); found {
		req.AutoDowntimeFloorMS = int(floor)
	}
	if cni, found, _ := unstructured.NestedInt64(obj, "spec", "cniConvergenceDelaySeconds"); found {
		req.CNIConvergenceDelaySeconds = int(cni)
	}
	if mc, found, _ := unstructured.NestedInt64(obj, "spec", "multifdChannels"); found {
		req.MultifdChannels = int(mc)
	}
	if pwt, found, _ := unstructured.NestedInt64(obj, "spec", "podWaitTimeoutSeconds"); found {
		req.PodWaitTimeoutSeconds = int(pwt)
	}
	req.SourceCleanup, _, _ = unstructured.NestedString(obj, "spec", "sourceCleanup")
	req.AdoptVM, _, _ = unstructured.NestedBool(obj, "spec", "adoptVM")
	// SourceNode + DestIP are not in the CRD spec — Reconciler.dispatch
	// looks them up via the injected Discoverer before calling Apply.
	return req, nil
}

// patchStatus issues a JSON merge patch against the Migration's status
// subresource. Errors are logged and swallowed because the next reconcile
// tick will retry.
func (r *Reconciler) patchStatus(ctx context.Context, key types.NamespacedName, migrationID, phase, message, errStr string) error {
	u := orchestrator.StatusUpdate{
		Phase:   orchestrator.StatusPhase(phase),
		Message: message,
	}
	if migrationID != "" {
		u.ID = orchestrator.MigrationID(migrationID)
	}
	return r.patchStatusUpdate(ctx, key, u, errStr)
}

func (r *Reconciler) patchFailedStatus(ctx context.Context, key types.NamespacedName, migrationID, message, errStr string) {
	if err := r.patchStatus(ctx, key, migrationID, string(orchestrator.PhaseFailed), message, errStr); err == nil {
		mFailed.Add(1)
	}
}

func (r *Reconciler) patchStatusUpdate(ctx context.Context, key types.NamespacedName, u orchestrator.StatusUpdate, errStr string) error {
	status := map[string]any{
		"phase":   string(u.Phase),
		"message": nil,
		"error":   nil,
	}
	if u.ID != "" {
		status["migrationID"] = string(u.ID)
	}
	if u.Message != "" {
		status["message"] = u.Message
	}
	if errStr != "" {
		status["error"] = errStr
	}
	if u.RAMTransferred > 0 || u.RAMTotal > 0 {
		status["ramTransferred"] = u.RAMTransferred
		status["ramTotal"] = u.RAMTotal
	}
	if u.DowntimeMS > 0 {
		status["actualDowntimeMS"] = u.DowntimeMS
	}
	if u.AppliedDowntimeMS > 0 {
		status["appliedDowntimeMS"] = u.AppliedDowntimeMS
	}
	if u.RTTMS > 0 {
		status["rttMS"] = u.RTTMS
	}
	if u.AutoDowntime {
		status["autoDowntime"] = true
	}
	if u.Phase == orchestrator.PhaseSubmitted {
		status["startedAt"] = time.Now().UTC().Format(time.RFC3339)
	}
	if u.Phase == orchestrator.PhaseSucceeded || u.Phase == orchestrator.PhaseFailed {
		status["completedAt"] = time.Now().UTC().Format(time.RFC3339)
	}
	patch := map[string]any{"status": status}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = r.Dynamic.Resource(MigrationGVR).Namespace(key.Namespace).Patch(ctx, key.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		mStatusPatchErrs.Add(1)
		slog.Error("patch status failed", "migration", key, "error", err)
	}
	return err
}

// createAdoptionPod creates a minimal Kata pod on the destination node.
// When the Kata shim starts, it calls the factory's GetBaseVM which returns
// the migrated QEMU. The pause container is just a placeholder — the real
// workload is already running inside the migrated VM.
//
// Strategy A part 1 (label/owner inheritance): when the source pod was
// owned by a ReplicaSet (i.e. part of a Deployment), copy its
// labels (including the Deployment selector label and the
// pod-template-hash that the RS uses to count its replicas) and its
// ownerReferences. The RS sees the adoption pod as one of its own
// pods, so once the source is deleted the replica count stays at the
// desired value and no replacement is spawned. Without this, the RS
// would create a fresh cold pod on the source node alongside the
// adopted pod on the destination, leaving the Deployment with two
// VMs (the migrated one as a "memorial" and a brand-new one).
//
// Note: a 5s race window remains between source-pod delete and
// adoption-pod create. During that window the RS sees zero matching
// pods and may spawn a replacement. Strategy A part 2 (validating
// admission webhook) closes that window. Without the webhook, the
// label/owner inheritance still avoids the steady-state "two pods"
// problem on retries: the next reconcile sees the spurious replacement
// + the adopted pod and the RS will pick one to delete (RS does not
// know which carries the migrated VM, so this is best-effort
// pre-webhook).
func (r *Reconciler) createAdoptionPod(ctx context.Context, req orchestrator.Request, name, destNode string) error {
	labels := map[string]string{
		"app.kubernetes.io/name":      "katamaran",
		"app.kubernetes.io/component": "adopted-vm",
		"katamaran.io/source-pod":     req.SourcePod.Name,
	}
	var ownerRefs []metav1.OwnerReference
	if r.Kube != nil {
		src, err := r.Kube.CoreV1().Pods(req.SourcePod.Namespace).Get(ctx, req.SourcePod.Name, metav1.GetOptions{})
		if err == nil {
			for k, v := range src.Labels {
				if _, taken := labels[k]; taken {
					continue
				}
				labels[k] = v
			}
			ownerRefs = src.OwnerReferences
		} else if !apierrors.IsNotFound(err) {
			slog.Warn("createAdoptionPod: source-pod lookup failed; proceeding without label/owner inheritance",
				"pod", req.SourcePod.Namespace+"/"+req.SourcePod.Name, "error", err)
		}
	}

	labelsAny := make(map[string]any, len(labels))
	for k, v := range labels {
		labelsAny[k] = v
	}
	meta := map[string]any{
		"name":      name,
		"namespace": req.SourcePod.Namespace,
		"labels":    labelsAny,
	}
	if len(ownerRefs) > 0 {
		refs := make([]any, 0, len(ownerRefs))
		for _, o := range ownerRefs {
			ref := map[string]any{
				"apiVersion": o.APIVersion,
				"kind":       o.Kind,
				"name":       o.Name,
				"uid":        string(o.UID),
			}
			if o.Controller != nil {
				ref["controller"] = *o.Controller
			}
			if o.BlockOwnerDeletion != nil {
				ref["blockOwnerDeletion"] = *o.BlockOwnerDeletion
			}
			refs = append(refs, ref)
		}
		meta["ownerReferences"] = refs
	}
	pod := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   meta,
			"spec": map[string]any{
				"runtimeClassName": "kata-qemu",
				"nodeName":         destNode,
				"containers": []any{
					map[string]any{
						"name":  "vm",
						"image": "registry.k8s.io/pause:3.9",
					},
				},
			},
		},
	}

	podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	_, err := r.Dynamic.Resource(podGVR).Namespace(req.SourcePod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	return err
}
