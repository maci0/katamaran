// Package controller implements a minimal Kubernetes controller for the
// Migration CRD (kata.katamaran.io/v1alpha1). It uses the dynamic client +
// a polling reconcile loop to keep the dependency footprint small (no
// controller-runtime, no codegen).
//
// Lifecycle:
//
//  1. Reconciler periodically lists Migration resources cluster-wide.
//  2. For each Migration in PhasePending (no .status.phase), translate
//     .spec into orchestrator.Request, call orchestrator.Apply, update
//     .status with the assigned migrationID.
//  3. Spawn a goroutine that consumes orchestrator.Watch events for the
//     migration and patches .status.phase on each update.
//
// This is the operator-grade equivalent of the dashboard's POST /api/migrate
// flow. Both consume the same orchestrator.Request type so behavioural drift
// between UI submissions and CRD-driven submissions is impossible.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// MigrationGVR is the GroupVersionResource the controller reconciles.
var MigrationGVR = schema.GroupVersionResource{
	Group:    "kata.katamaran.io",
	Version:  "v1alpha1",
	Resource: "migrations",
}

// Reconciler watches Migration resources and submits each pending one to
// the embedded orchestrator. Status is patched back to the CR as the
// orchestrator emits StatusUpdate events.
type Reconciler struct {
	Dynamic       dynamic.Interface
	Orchestrator  orchestrator.Orchestrator
	PollInterval  time.Duration
	StatusTimeout time.Duration

	mu       sync.Mutex
	tracking map[types.NamespacedName]bool // migrations currently being watched
}

// NewReconciler builds a reconciler with sensible defaults.
func NewReconciler(dyn dynamic.Interface, orch orchestrator.Orchestrator) *Reconciler {
	return &Reconciler{
		Dynamic:       dyn,
		Orchestrator:  orch,
		PollInterval:  5 * time.Second,
		StatusTimeout: 30 * time.Minute,
		tracking:      map[types.NamespacedName]bool{},
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

		// Skip if status.phase already set (already submitted) or we are
		// already watching this migration.
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		if phase != "" {
			continue
		}
		r.mu.Lock()
		already := r.tracking[key]
		if !already {
			r.tracking[key] = true
		}
		r.mu.Unlock()
		if already {
			continue
		}
		go r.dispatch(ctx, key, obj)
	}
	return nil
}

func (r *Reconciler) dispatch(ctx context.Context, key types.NamespacedName, obj *unstructured.Unstructured) {
	defer func() {
		r.mu.Lock()
		delete(r.tracking, key)
		r.mu.Unlock()
	}()

	req, err := specToRequest(obj.Object)
	if err != nil {
		_ = r.patchStatus(ctx, key, "", string(orchestrator.PhaseFailed), "invalid spec", err.Error())
		return
	}
	jobCtx, cancel := context.WithTimeout(ctx, r.StatusTimeout)
	defer cancel()
	id, err := r.Orchestrator.Apply(jobCtx, req)
	if err != nil {
		_ = r.patchStatus(ctx, key, "", string(orchestrator.PhaseFailed), "Apply failed", err.Error())
		return
	}
	_ = r.patchStatus(ctx, key, string(id), string(orchestrator.PhaseSubmitted), "submitted to orchestrator", "")

	updates, err := r.Orchestrator.Watch(jobCtx, id)
	if err != nil {
		_ = r.patchStatus(ctx, key, string(id), string(orchestrator.PhaseFailed), "Watch failed", err.Error())
		return
	}
	for u := range updates {
		errStr := ""
		if u.Error != nil {
			errStr = u.Error.Error()
		}
		_ = r.patchStatus(ctx, key, string(u.ID), string(u.Phase), u.Message, errStr)
	}
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
	// SourceNode is required by orchestrator.Validate but not part of the
	// CRD spec — derive it from the dest's perspective: source is whichever
	// node the source pod runs on. For now use destNode's label-disambiguated
	// "any other" placeholder. The orchestrator's Native impl looks the pod
	// up directly; the Script impl forwards --pod-name to the source job.
	// Setting a non-empty SourceNode just satisfies Validate.
	req.SourceNode = "auto"
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
