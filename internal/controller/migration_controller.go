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
	"fmt"
	"log/slog"
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

// jobNamespace is where the Native orchestrator submits source/dest Jobs.
// Kept in sync with the Native default so recovery can find them on restart.
const jobNamespace = "kube-system"

// Reconciler watches Migration resources and submits each pending one to
// the embedded orchestrator. Status is patched back to the CR as the
// orchestrator emits StatusUpdate events.
type Reconciler struct {
	Dynamic       dynamic.Interface
	Kube          kubernetes.Interface    // optional; enables restart recovery via direct Job inspection
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
			if err := r.reconcileAll(ctx); err != nil {
				slog.Warn("reconcile loop", "error", err)
			}
		}
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
				slog.Warn("add finalizer", "migration", key, "error", err)
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
		case phase == string(orchestrator.PhaseSubmitted) || phase == string(orchestrator.PhaseTransferring):
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
	defer r.untrack(key)

	slog.Info("Dispatching new Migration", "migration", key)
	req, err := specToRequest(obj.Object)
	if err != nil {
		slog.Warn("Migration spec invalid", "migration", key, "error", err)
		_ = r.patchStatus(ctx, key, "", string(orchestrator.PhaseFailed), "invalid spec", err.Error())
		return
	}
	if r.Discoverer != nil && req.SourcePod != nil {
		lookupCtx, lookupCancel := context.WithTimeout(ctx, 30*time.Second)
		srcNode, lerr := r.Discoverer.LookupPodNode(lookupCtx, req.SourcePod.Namespace, req.SourcePod.Name)
		if lerr == nil && srcNode != "" {
			req.SourceNode = srcNode
		}
		destIP, lerr := r.Discoverer.LookupNodeInternalIP(lookupCtx, req.DestNode)
		lookupCancel()
		if lerr != nil || destIP == "" {
			_ = r.patchStatus(ctx, key, "", string(orchestrator.PhaseFailed), "resolve dest node IP", fmt.Sprintf("%v", lerr))
			return
		}
		req.DestIP = destIP
		if req.SourceNode == req.DestNode {
			_ = r.patchStatus(ctx, key, "", string(orchestrator.PhaseFailed), "invalid spec", "source pod already runs on destNode")
			return
		}
	}
	jobCtx, cancel := context.WithTimeout(ctx, r.StatusTimeout)
	defer cancel()
	id, err := r.Orchestrator.Apply(jobCtx, req)
	if err != nil {
		slog.Warn("Apply failed", "migration", key, "error", err)
		_ = r.patchStatus(ctx, key, "", string(orchestrator.PhaseFailed), "Apply failed", err.Error())
		return
	}
	r.updateTrack(key, id, cancel)
	slog.Info("Migration submitted", "migration", key, "migrationID", id, "source_node", req.SourceNode, "dest_node", req.DestNode)
	_ = r.patchStatus(ctx, key, string(id), string(orchestrator.PhaseSubmitted), "submitted to orchestrator", "")

	updates, err := r.Orchestrator.Watch(jobCtx, id)
	if err != nil {
		slog.Warn("Watch failed", "migration", key, "migrationID", id, "error", err)
		_ = r.patchStatus(ctx, key, string(id), string(orchestrator.PhaseFailed), "Watch failed", err.Error())
		return
	}
	var lastPhase string
	for u := range updates {
		errStr := ""
		if u.Error != nil {
			errStr = u.Error.Error()
		}
		_ = r.patchStatus(ctx, key, string(u.ID), string(u.Phase), u.Message, errStr)
		lastPhase = string(u.Phase)
	}
	slog.Info("Migration finished", "migration", key, "migrationID", id, "final_phase", lastPhase)
}

// recover reattaches to a Migration left in a non-terminal phase by a
// previous controller incarnation. It polls the source/dest Jobs in
// kube-system and patches .status.phase based on their conditions.
//
// Native uses singleton Job names (katamaran-source / katamaran-dest), so
// only one in-flight migration can ever exist at a time. After a controller
// restart we know the Jobs we're inspecting belong to whatever is in the
// only non-terminal Migration. If two non-terminal Migrations exist after
// a restart (which would be a pre-existing data corruption), we mark the
// extras as failed so the queue can drain.
func (r *Reconciler) recover(ctx context.Context, key types.NamespacedName, obj *unstructured.Unstructured) {
	defer r.untrack(key)

	id, _, _ := unstructured.NestedString(obj.Object, "status", "migrationID")
	slog.Info("Recovering in-flight Migration after controller restart", "migration", key, "migrationID", id)

	if r.Kube == nil {
		slog.Warn("Recovery skipped: no Kube clientset wired", "migration", key)
		_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "controller restarted; recovery unavailable", "")
		return
	}

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
			_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovery timed out waiting for jobs", "")
			return
		}
		dest, derr := r.Kube.BatchV1().Jobs(jobNamespace).Get(ctx, "katamaran-dest", metav1.GetOptions{})
		if derr == nil {
			if cond := jobCondition(dest); cond == batchv1.JobComplete {
				_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseSucceeded), "recovered: dest job complete", "")
				return
			} else if cond == batchv1.JobFailed {
				_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovered: dest job failed", "")
				return
			}
		} else if !apierrors.IsNotFound(derr) {
			slog.Warn("recover: get dest job", "migration", key, "error", derr)
			continue
		}
		// dest missing or still running — check source for an early failure.
		src, serr := r.Kube.BatchV1().Jobs(jobNamespace).Get(ctx, "katamaran-source", metav1.GetOptions{})
		if serr == nil && jobCondition(src) == batchv1.JobFailed && (derr != nil && apierrors.IsNotFound(derr)) {
			_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovered: source job failed before dest started", "")
			return
		}
		if apierrors.IsNotFound(derr) && apierrors.IsNotFound(serr) {
			_ = r.patchStatus(ctx, key, id, string(orchestrator.PhaseFailed), "recovered: source/dest jobs disappeared", "")
			return
		}
	}
}

// handleDeletion runs when the user has issued `kubectl delete migration`.
// We call orchestrator.Stop to clean up any in-flight Jobs, then patch
// the CR to remove the finalizer so kube-apiserver can finish deleting.
func (r *Reconciler) handleDeletion(ctx context.Context, key types.NamespacedName, obj *unstructured.Unstructured) {
	if !hasFinalizer(obj) {
		return // nothing to do; kube-apiserver already finished deleting
	}
	id, _, _ := unstructured.NestedString(obj.Object, "status", "migrationID")
	slog.Info("Migration deleted; stopping orchestrator + removing finalizer", "migration", key, "migrationID", id)
	if id != "" {
		stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := r.Orchestrator.Stop(stopCtx, orchestrator.MigrationID(id)); err != nil {
			slog.Warn("Stop failed; removing finalizer anyway to unblock deletion", "migration", key, "migrationID", id, "error", err)
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
		slog.Warn("remove finalizer", "migration", key, "error", err)
	}
}

// jobCondition returns the most recent terminal condition (Complete or Failed)
// on a Job, or "" if neither is set yet.
func jobCondition(job *batchv1.Job) batchv1.JobConditionType {
	for _, c := range job.Status.Conditions {
		if c.Status == "True" && (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) {
			return c.Type
		}
	}
	return ""
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

// addFinalizer patches the Migration to carry our finalizer.
func (r *Reconciler) addFinalizer(ctx context.Context, obj *unstructured.Unstructured) error {
	finalizers := append([]string{}, obj.GetFinalizers()...)
	finalizers = append(finalizers, finalizerName)
	patch := map[string]any{"metadata": map[string]any{"finalizers": finalizers}}
	patchBytes, err := jsonMarshal(patch)
	if err != nil {
		return err
	}
	_, err = r.Dynamic.Resource(MigrationGVR).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}

// removeFinalizer patches the Migration to drop our finalizer. Other
// finalizers (if any) are preserved.
func (r *Reconciler) removeFinalizer(ctx context.Context, obj *unstructured.Unstructured) error {
	out := []string{}
	for _, f := range obj.GetFinalizers() {
		if f != finalizerName {
			out = append(out, f)
		}
	}
	patch := map[string]any{"metadata": map[string]any{"finalizers": out}}
	patchBytes, err := jsonMarshal(patch)
	if err != nil {
		return err
	}
	_, err = r.Dynamic.Resource(MigrationGVR).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
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
	if req.DestNode == "" || req.Image == "" {
		return req, fmt.Errorf("spec.destNode and spec.image are required")
	}
	req.SharedStorage, _, _ = unstructured.NestedBool(obj, "spec", "sharedStorage")
	req.ReplayCmdline, _, _ = unstructured.NestedBool(obj, "spec", "replayCmdline")
	req.TunnelMode, _, _ = unstructured.NestedString(obj, "spec", "tunnelMode")
	if dt, found, _ := unstructured.NestedInt64(obj, "spec", "downtimeMS"); found {
		req.DowntimeMS = int(dt)
	}
	req.AutoDowntime, _, _ = unstructured.NestedBool(obj, "spec", "autoDowntime")
	if mc, found, _ := unstructured.NestedInt64(obj, "spec", "multifdChannels"); found {
		req.MultifdChannels = int(mc)
	}
	// SourceNode + DestIP are not in the CRD spec — Reconciler.dispatch
	// looks them up via the injected Discoverer before calling Apply.
	return req, nil
}

// patchStatus issues a JSON merge patch against the Migration's status
// subresource. Errors are logged and swallowed because the next reconcile
// tick will retry.
func (r *Reconciler) patchStatus(ctx context.Context, key types.NamespacedName, migrationID, phase, message, errStr string) error {
	status := map[string]any{
		"phase": phase,
	}
	if migrationID != "" {
		status["migrationID"] = migrationID
	}
	if message != "" {
		status["message"] = message
	}
	if errStr != "" {
		status["error"] = errStr
	}
	if phase == string(orchestrator.PhaseSubmitted) {
		status["startedAt"] = time.Now().UTC().Format(time.RFC3339)
	}
	if phase == string(orchestrator.PhaseSucceeded) || phase == string(orchestrator.PhaseFailed) {
		status["completedAt"] = time.Now().UTC().Format(time.RFC3339)
	}
	patch := map[string]any{"status": status}
	patchBytes, err := jsonMarshal(patch)
	if err != nil {
		return err
	}
	_, err = r.Dynamic.Resource(MigrationGVR).Namespace(key.Namespace).Patch(ctx, key.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		slog.Warn("patch status", "migration", key, "error", err)
	}
	return err
}
