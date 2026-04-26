package dashboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"time"
)

// migrateScriptPath finds the absolute path to the migrate.sh script.
func migrateScriptPath() (string, error) {
	paths := []string{
		"deploy/migrate.sh",
		"/usr/local/bin/migrate.sh",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("migrate.sh not found in expected locations: %v", paths)
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
	formKeys := []string{"source_node", "dest_node", "qmp_source", "qmp_dest", "tap", "tap_netns", "dest_ip", "vm_ip", "image", "shared_storage", "downtime", "source_pod_name", "source_pod_namespace", "dest_pod_name", "dest_pod_namespace"}
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

	if a.allowedImage != "" && r.PostFormValue("image") != a.allowedImage {
		slog.Warn("Rejected disallowed migration image", "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Image is not allowed", http.StatusBadRequest)
		return
	}

	// Reject requests missing required fields. The frontend validates
	// these too, but direct API callers (curl, scripts) bypass that.
	// Aligned with migrate.sh's required flags.
	//
	// Pod-picker mode: when source_pod_name is set, the user picked a pod
	// from the dropdown and we resolve source_node + dest_ip via kubectl;
	// the legacy explicit-fields path stays unchanged for backward compat.
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

	// Resolve pod-picker fields via kubectl up front, before acquiring the
	// migration lock — keeps state-rollback off the failure paths.
	var resolvedSrcNode, resolvedDestIP string
	if podMode {
		pod := r.PostFormValue("source_pod_name")
		ns := r.PostFormValue("source_pod_namespace")
		dest := r.PostFormValue("dest_node")
		var err error
		resolvedSrcNode, err = lookupPodNode(r.Context(), ns, pod)
		if err != nil {
			jsonError(w, "lookup source pod: "+err.Error(), http.StatusBadRequest)
			return
		}
		resolvedDestIP, err = lookupNodeInternalIP(r.Context(), dest)
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
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":        "Migration already running",
			"migration_id": runningID,
		})
		return
	}
	a.isMigrating = true
	a.migrationOutput = nil
	a.logBufferWrapped = false
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

	scriptPath := a.migrateScript
	if scriptPath == "" {
		var err error
		scriptPath, err = migrateScriptPath()
		if err != nil {
			a.migrationMutex.Lock()
			a.isMigrating = false
			a.migrationID = ""
			a.migrationCancel = nil
			a.migrationsFailed++
			a.lastMigrationResult = "error"
			a.lastMigrationError = err.Error()
			a.migrationMutex.Unlock()
			dashboardMigrationsActive.Add(-1)
			dashboardMigrationResultsByOutcome.Add("error", 1)
			cancel()
			slog.Error("Migration script not found", "error", err, "request_id", requestIDFromContext(r.Context()))
			jsonError(w, "Migration script not found", http.StatusInternalServerError)
			return
		}
	}

	args := []string{scriptPath, "--image", r.PostFormValue("image")}
	var logSourceNode, logDestIP, logVMIP string
	if podMode {
		args = append(args,
			"--source-node", resolvedSrcNode,
			"--dest-node", r.PostFormValue("dest_node"),
			"--dest-ip", resolvedDestIP,
			"--pod-name", r.PostFormValue("source_pod_name"),
			"--pod-namespace", r.PostFormValue("source_pod_namespace"),
		)
		// Advanced overrides: any non-empty value from the form replaces the
		// auto-derived defaults inside the source/dest jobs.
		for _, p := range []struct{ flag, key string }{
			{"--qmp-source", "qmp_source"},
			{"--qmp-dest", "qmp_dest"},
			{"--vm-ip", "vm_ip"},
			{"--tap", "tap"},
		} {
			if v := r.PostFormValue(p.key); v != "" {
				args = append(args, p.flag, v)
			}
		}
		// Dest pod picker: when set, the dest job's resolver derives qmp from
		// the pod's sandbox UUID instead of using the migrate.sh placeholder.
		if v := r.PostFormValue("dest_pod_name"); v != "" {
			args = append(args, "--dest-pod-name", v, "--dest-pod-namespace", r.PostFormValue("dest_pod_namespace"))
		}
		logSourceNode = resolvedSrcNode
		logDestIP = resolvedDestIP
	} else {
		args = append(args,
			"--source-node", r.PostFormValue("source_node"),
			"--dest-node", r.PostFormValue("dest_node"),
			"--qmp-source", r.PostFormValue("qmp_source"),
			"--qmp-dest", r.PostFormValue("qmp_dest"),
			"--tap", r.PostFormValue("tap"),
			"--dest-ip", r.PostFormValue("dest_ip"),
			"--vm-ip", r.PostFormValue("vm_ip"),
		)
		logSourceNode = r.PostFormValue("source_node")
		logDestIP = r.PostFormValue("dest_ip")
		logVMIP = r.PostFormValue("vm_ip")
	}

	if v := r.PostFormValue("tap_netns"); v != "" {
		args = append(args, "--tap-netns", v)
	}

	if r.PostFormValue("shared_storage") == "true" {
		args = append(args, "--shared-storage")
	}
	if downtimeArg != "" {
		args = append(args, "--downtime", downtimeArg)
	}

	slog.Info("Migration initiated", "migration_id", migrationID, "request_id", requestIDFromContext(r.Context()), "remote_addr", r.RemoteAddr, "source_node", logSourceNode, "dest_node", r.PostFormValue("dest_node"), "image", r.PostFormValue("image"), "dest_ip", logDestIP, "vm_ip", logVMIP, "shared_storage", r.PostFormValue("shared_storage") == "true", "pod_mode", podMode)
	go a.runCommand(ctx, args, migrationID)

	writeJSON(w, http.StatusAccepted, map[string]string{"message": "Migration started", "migration_id": migrationID})
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

// runCommand executes the migration script and streams its output to the dashboard.
func (a *App) runCommand(ctx context.Context, args []string, migrationID string) {
	start := time.Now()
	defer func() {
		a.migrationMutex.Lock()
		a.isMigrating = false
		// Keep migrationID so GET /api/status can correlate the result
		// with the migration_id returned in the 202 response. It gets
		// overwritten when a new migration starts.
		a.migrationCancel = nil
		outcome := a.lastMigrationResult
		a.migrationMutex.Unlock()
		dashboardMigrationsActive.Add(-1)
		recordMigrationDuration(time.Since(start), outcome)
	}()

	slog.Info("Starting migration command", "migration_id", migrationID, "command", args[0], "args", args[1:])

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "KATAMARAN_MIGRATION_ID="+migrationID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error("Failed to create stdout pipe", "migration_id", migrationID, "error", err)
		a.appendLog("Error: " + err.Error())
		a.setMigrationResult("error", err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		slog.Error("Failed to start migration command", "migration_id", migrationID, "error", err)
		a.appendLog("Error starting: " + err.Error())
		a.setMigrationResult("error", err.Error())
		return
	}

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, scannerInitBuf)
	scanner.Buffer(buf, scannerMaxSize)
	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("Migration output", "migration_id", migrationID, "line", line)
		a.appendLog(line)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		slog.Warn("Migration output scanner error", "migration_id", migrationID, "error", scanErr)
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	if err := cmd.Wait(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			// User-initiated stop (via /api/migrate/stop) — classified as error for metrics.
			slog.Info("Migration command stopped by user", "migration_id", migrationID, "exit_code", exitCode, "elapsed", elapsed)
			a.appendLog("Migration stopped by user.")
			a.setMigrationResult("error", "stopped by user")
		} else {
			slog.Error("Migration command finished with error", "migration_id", migrationID, "error", err, "exit_code", exitCode, "elapsed", elapsed)
			a.appendLog("Finished with error: " + err.Error())
			// Include the last 1-2 output lines in the error for API consumers,
			// since err.Error() is just "exit status 1" which is not actionable.
			errDetail := err.Error()
			a.migrationMutex.Lock()
			if n := len(a.migrationOutput); n > 0 {
				tail := a.migrationOutput[n-1]
				if n > 1 {
					tail = a.migrationOutput[n-2] + "; " + tail
				}
				errDetail = tail
			}
			a.migrationMutex.Unlock()
			a.setMigrationResult("error", errDetail)
		}
	} else {
		slog.Info("Migration command finished successfully", "migration_id", migrationID, "elapsed", elapsed)
		a.appendLog("Finished successfully.")
		a.setMigrationResult("success", "")
	}
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
