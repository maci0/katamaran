package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// formToOrchestratorRequest reads the (already-validated) form fields and
// builds an orchestrator.Request. resolvedSrcNode and resolvedDestIP are the
// values handleMigrate looked up via the Discoverer when the form is in pod
// mode; pass empty strings in legacy mode.
func formToOrchestratorRequest(r *http.Request, podMode bool, resolvedSrcNode, resolvedDestIP, downtimeArg string) orchestrator.Request {
	req := orchestrator.Request{
		DestNode:      r.PostFormValue("dest_node"),
		Image:         r.PostFormValue("image"),
		SharedStorage: r.PostFormValue("shared_storage") == "true",
		ReplayCmdline: r.PostFormValue("replay_cmdline") == "true",
		TapNetns:      r.PostFormValue("tap_netns"),
	}
	if podMode {
		req.SourceNode = resolvedSrcNode
		req.DestIP = resolvedDestIP
		req.SourcePod = &orchestrator.PodRef{
			Namespace: r.PostFormValue("source_pod_namespace"),
			Name:      r.PostFormValue("source_pod_name"),
		}
		// Advanced overrides — apply only when non-empty.
		req.SourceQMP = r.PostFormValue("qmp_source")
		req.DestQMP = r.PostFormValue("qmp_dest")
		req.VMIP = r.PostFormValue("vm_ip")
		req.TapIface = r.PostFormValue("tap")
		if dpNS := r.PostFormValue("dest_pod_namespace"); dpNS != "" {
			req.DestPod = &orchestrator.PodRef{
				Namespace: dpNS,
				Name:      r.PostFormValue("dest_pod_name"),
			}
		}
	} else {
		req.SourceNode = r.PostFormValue("source_node")
		req.DestIP = r.PostFormValue("dest_ip")
		req.SourceQMP = r.PostFormValue("qmp_source")
		req.DestQMP = r.PostFormValue("qmp_dest")
		req.VMIP = r.PostFormValue("vm_ip")
		req.TapIface = r.PostFormValue("tap")
	}
	if d, err := strconv.Atoi(downtimeArg); err == nil && d > 0 {
		req.DowntimeMS = d
	}
	return req
}

// handleMigrate processes a form POST to start a new migration.
func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	// Reject non-form content types early. Without this check, ParseForm
	// silently ignores non-form bodies (e.g. JSON), and all fields appear
	// empty — producing confusing "Missing required field" errors.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			jsonError(w, "Content-Type must be application/x-www-form-urlencoded", http.StatusUnsupportedMediaType)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := r.ParseForm(); err != nil {
		reqID := requestIDFromContext(r.Context())
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			slog.Warn("Request body too large", "request_id", reqID)
			jsonError(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			slog.Warn("Failed to parse form body", "error", err, "request_id", reqID)
			jsonError(w, "Invalid request body", http.StatusBadRequest)
		}
		return
	}

	// Validate all form values against shell metacharacters.
	formKeys := []string{"source_node", "dest_node", "qmp_source", "qmp_dest", "tap", "tap_netns", "dest_ip", "vm_ip", "image", "shared_storage", "downtime", "source_pod_name", "source_pod_namespace", "dest_pod_name", "dest_pod_namespace", "replay_cmdline"}
	for _, key := range formKeys {
		if v := r.PostFormValue(key); v != "" && !validFormValue(v) {
			slog.Warn("Rejected invalid form value", "field", key, "request_id", requestIDFromContext(r.Context()))
			jsonError(w, fmt.Sprintf("Invalid value for %s", key), http.StatusBadRequest)
			return
		}
	}

	// Validate shared_storage as a boolean if present.
	if v := r.PostFormValue("shared_storage"); v != "" && v != "true" && v != "false" {
		slog.Warn("Rejected invalid shared_storage value", "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Invalid value for shared_storage (must be 'true' or 'false')", http.StatusBadRequest)
		return
	}
	if v := r.PostFormValue("replay_cmdline"); v != "" && v != "true" && v != "false" {
		slog.Warn("Rejected invalid replay_cmdline value", "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Invalid value for replay_cmdline (must be 'true' or 'false')", http.StatusBadRequest)
		return
	}

	if a.allowedImage != "" && r.PostFormValue("image") != a.allowedImage {
		slog.Warn("Rejected disallowed migration image", "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Image is not allowed", http.StatusBadRequest)
		return
	}

	// Reject requests missing required fields. The frontend validates
	// these too, but direct API callers (curl, scripts) bypass that.
	// Aligned with orchestrator validation and the legacy migrate.sh flags.
	//
	// Pod-picker mode: when source_pod_name is set, the user picked a pod
	// from the dropdown and we resolve source_node + dest_ip via the
	// Discoverer; the legacy explicit-fields path stays unchanged for
	// backward compat.
	podMode := r.PostFormValue("source_pod_name") != ""
	required := []string{"image"}
	if podMode {
		required = append(required, "source_pod_namespace", "source_pod_name", "dest_node")
	} else {
		required = append(required, "source_node", "dest_node", "qmp_source", "qmp_dest", "dest_ip", "vm_ip", "tap")
	}
	for _, key := range required {
		if r.PostFormValue(key) == "" {
			jsonError(w, fmt.Sprintf("Missing required field: %s", key), http.StatusBadRequest)
			return
		}
	}

	// Validate optional fields before acquiring the migration lock to avoid
	// needing state rollback on validation failure.
	var downtimeArg string
	if dt := r.PostFormValue("downtime"); dt != "" {
		d, err := strconv.Atoi(dt)
		if err != nil || d < 1 || d > 60000 {
			jsonError(w, "Invalid downtime value (must be between 1 and 60000)", http.StatusBadRequest)
			return
		}
		downtimeArg = strconv.Itoa(d)
	}

	// Resolve pod-picker fields via the Discoverer up front, before
	// acquiring the migration lock — keeps state-rollback off the failure
	// paths.
	var resolvedSrcNode, resolvedDestIP string
	if podMode {
		disc := a.discovery()
		if disc == nil {
			jsonError(w, "Discoverer not configured (no in-cluster config or KUBECONFIG)", http.StatusServiceUnavailable)
			return
		}
		pod := r.PostFormValue("source_pod_name")
		ns := r.PostFormValue("source_pod_namespace")
		dest := r.PostFormValue("dest_node")
		var err error
		resolvedSrcNode, err = disc.LookupPodNode(r.Context(), ns, pod)
		if err != nil {
			jsonError(w, "lookup source pod: "+err.Error(), http.StatusBadRequest)
			return
		}
		resolvedDestIP, err = disc.LookupNodeInternalIP(r.Context(), dest)
		if err != nil {
			jsonError(w, "lookup dest node: "+err.Error(), http.StatusBadRequest)
			return
		}
		if resolvedSrcNode == dest {
			jsonError(w, "source and dest node must differ", http.StatusBadRequest)
			return
		}
	}

	a.migrationMutex.Lock()
	if a.isMigrating {
		runningID := a.migrationID
		a.migrationMutex.Unlock()
		slog.Warn("Migration request rejected: already running", "running_migration_id", runningID, "request_id", requestIDFromContext(r.Context()), "remote_addr", r.RemoteAddr)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":        "Migration already running",
			"migration_id": runningID,
		})
		return
	}
	a.isMigrating = true
	a.migrationOutput = nil
	a.logBufferWrapped = false
	a.latestProgress = nil
	migrationID := generateID()
	a.migrationID = migrationID
	a.migrationStart = time.Now()
	a.migrationsStarted++
	dashboardMigrationsActive.Add(1)
	// Use context.Background() so the migration process survives after
	// the HTTP response is sent (r.Context() cancels on response write).
	ctx, cancel := context.WithCancel(context.Background())
	a.migrationCancel = cancel
	a.migrationMutex.Unlock()

	// Translate the form into an orchestrator.Request and validate.
	req := formToOrchestratorRequest(r, podMode, resolvedSrcNode, resolvedDestIP, downtimeArg)
	if err := orchestrator.Validate(req); err != nil {
		a.abortPendingMigration(cancel, err.Error())
		jsonError(w, "Invalid migration request: "+err.Error(), http.StatusBadRequest)
		return
	}

	logSourceNode, logDestIP, logVMIP := req.SourceNode, req.DestIP, req.VMIP
	slog.Info("Migration initiated", "migration_id", migrationID, "request_id", requestIDFromContext(r.Context()), "remote_addr", r.RemoteAddr, "source_node", logSourceNode, "dest_node", req.DestNode, "image", req.Image, "dest_ip", logDestIP, "vm_ip", logVMIP, "shared_storage", req.SharedStorage, "pod_mode", podMode, "replay_cmdline", req.ReplayCmdline)

	if a.orch == nil {
		a.abortPendingMigration(cancel, "no orchestrator wired")
		jsonError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	go a.runOrchestrator(ctx, a.orch, req, migrationID)

	writeJSON(w, http.StatusAccepted, map[string]string{"message": "Migration started", "migration_id": migrationID})
}

// runOrchestrator submits req to orch, reflects each StatusUpdate into the
// dashboard log buffer, and finalises migration counters when the watch
// channel closes, so /api/status behaves identically regardless of which
// orchestrator backs the migration.
func (a *App) runOrchestrator(ctx context.Context, orch orchestrator.Orchestrator, req orchestrator.Request, migrationID string) {
	start := time.Now()
	defer func() {
		a.migrationMutex.Lock()
		a.isMigrating = false
		a.migrationCancel = nil
		outcome := a.lastMigrationResult
		a.migrationMutex.Unlock()
		dashboardMigrationsActive.Add(-1)
		recordMigrationDuration(time.Since(start), outcome)
	}()

	a.appendLog(">>> Submitting migration via Native orchestrator…")
	id, err := orch.Apply(ctx, req)
	if err != nil {
		a.appendLog("Error: " + err.Error())
		a.setMigrationResult("error", err.Error())
		return
	}
	a.appendLog(">>> Migration submitted, id=" + string(id))
	updates, err := orch.Watch(ctx, id)
	if err != nil {
		a.appendLog("Error: " + err.Error())
		a.setMigrationResult("error", err.Error())
		return
	}
	var terminal orchestrator.StatusPhase
	var terminalErr error
	phaseAt := map[orchestrator.StatusPhase]time.Time{}
	for u := range updates {
		if _, seen := phaseAt[u.Phase]; !seen {
			phaseAt[u.Phase] = u.When
		}
		if u.RAMTotal > 0 || u.Phase == orchestrator.PhaseSucceeded {
			a.migrationMutex.Lock()
			a.latestProgress = &MigrationProgress{
				Phase:          string(u.Phase),
				RAMTransferred: u.RAMTransferred,
				RAMTotal:       u.RAMTotal,
				DowntimeMS:     u.DowntimeMS,
			}
			a.migrationMutex.Unlock()
		}
		line := ">>> " + string(u.Phase)
		switch {
		case u.Phase == orchestrator.PhaseSucceeded && u.RAMTotal > 0:
			line += fmt.Sprintf(": %s transferred", humanBytes(u.RAMTotal))
			if u.DowntimeMS > 0 {
				line += fmt.Sprintf(", %dms downtime", u.DowntimeMS)
			}
			if breakdown := phaseBreakdown(start, phaseAt, u.When); breakdown != "" {
				line += ", " + breakdown
			}
		case u.RAMTotal > 0:
			pct := int((u.RAMTransferred * 100) / u.RAMTotal)
			line += fmt.Sprintf(": %d%% (%s / %s)", pct, humanBytes(u.RAMTransferred), humanBytes(u.RAMTotal))
		case u.Message != "":
			line += ": " + u.Message
		}
		if u.Error != nil {
			line += ": " + u.Error.Error()
		}
		a.appendLog(line)
		if u.Phase.IsTerminal() {
			terminal = u.Phase
			terminalErr = u.Error
		}
	}
	switch terminal {
	case orchestrator.PhaseSucceeded:
		a.setMigrationResult("success", "")
	case orchestrator.PhaseFailed:
		msg := "migration failed"
		if terminalErr != nil {
			msg = terminalErr.Error()
		}
		a.setMigrationResult("error", msg)
	default:
		a.setMigrationResult("error", "watch closed without terminal status")
	}
}

// phaseBreakdown formats the wall-clock split between phases for the
// final succeeded log line, e.g. "35s wall (4s setup + 31s xfer)".
// Returns "" when the timing data is incomplete (e.g. no transferring
// phase was seen, as in fast paths or test fakes).
func phaseBreakdown(start time.Time, phaseAt map[orchestrator.StatusPhase]time.Time, end time.Time) string {
	wall := end.Sub(start).Round(time.Second)
	if wall <= 0 {
		return ""
	}
	xferStart, ok := phaseAt[orchestrator.PhaseTransferring]
	if !ok {
		return fmt.Sprintf("%s wall", wall)
	}
	setup := xferStart.Sub(start).Round(time.Second)
	xfer := end.Sub(xferStart).Round(time.Second)
	return fmt.Sprintf("%s wall (%s setup + %s xfer)", wall, setup, xfer)
}

// humanBytes formats a byte count as MB / GB for log lines. Avoids the
// overhead of pulling in a units library for one display string.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// handleMigrateStop processes a request to cancel an ongoing migration.
func (a *App) handleMigrateStop(w http.ResponseWriter, r *http.Request) {
	a.migrationMutex.Lock()
	wasRunning := a.migrationCancel != nil
	migrationID := a.migrationID
	if wasRunning {
		a.migrationCancel()
	}
	a.migrationMutex.Unlock()
	if wasRunning {
		slog.Info("Migration stop requested", "migration_id", migrationID, "remote_addr", r.RemoteAddr, "request_id", requestIDFromContext(r.Context()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Migration stop requested", "stopped": wasRunning, "migration_id": migrationID})
}

// abortPendingMigration unwinds the migration state set up at the start of
// handleMigrate when a pre-launch check (Validate, missing orchestrator) fails.
// Counterpart to runOrchestrator's deferred cleanup, which only runs once the
// orchestrator goroutine has been kicked off.
func (a *App) abortPendingMigration(cancel context.CancelFunc, errMsg string) {
	a.migrationMutex.Lock()
	a.isMigrating = false
	a.migrationID = ""
	a.migrationCancel = nil
	a.migrationsFailed++
	a.lastMigrationResult = "error"
	a.lastMigrationError = errMsg
	a.migrationMutex.Unlock()
	dashboardMigrationsActive.Add(-1)
	dashboardMigrationResultsByOutcome.Add("error", 1)
	cancel()
}

// setMigrationResult updates the final status and error message of the completed migration.
func (a *App) setMigrationResult(result, errMsg string) {
	a.migrationMutex.Lock()
	defer a.migrationMutex.Unlock()
	a.lastMigrationResult = result
	a.lastMigrationError = errMsg
	switch result {
	case "success":
		a.migrationsSucceeded++
	case "error":
		a.migrationsFailed++
	}
}

// appendLog adds a new log line to the migration output buffer, discarding the oldest if full.
func (a *App) appendLog(msg string) {
	if len(msg) > maxLogLineSize {
		msg = msg[:maxLogLineSize] + " ... [truncated]"
	}
	a.migrationMutex.Lock()
	defer a.migrationMutex.Unlock()
	a.migrationOutput = append(a.migrationOutput, msg)
	if len(a.migrationOutput) > maxLogLines {
		a.migrationOutput = slices.Delete(a.migrationOutput, 0, len(a.migrationOutput)-maxLogLines)
		if !a.logBufferWrapped {
			a.logBufferWrapped = true
			slog.Warn("Migration output buffer full, oldest lines dropped", "max_lines", maxLogLines, "migration_id", a.migrationID)
		}
	}
}
