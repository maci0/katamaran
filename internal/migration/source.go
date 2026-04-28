package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"time"

	"github.com/maci0/katamaran/internal/qmp"
)

// Sentinel errors for migration terminal states.
var (
	errMigrationFailed    = errors.New("migration failed")
	errMigrationCancelled = errors.New("migration cancelled")
)

// Test injection points for the pod-resolver flow. Production code uses
// LookupPodIP and realProc{}; tests swap these to stub apiserver / procfs.
var (
	lookupPodIP        = LookupPodIP
	procImpl    procFS = realProc{}
	sandboxRoot        = "/run/vc/vm"
)

// RunSource initiates live migration from the source node to the destination.
//
// A deferred cleanup ensures the drive-mirror job is torn down on any early
// return, preventing resource leaks. It is disarmed on the success path.
// The IP tunnel is torn down inline after migration completes.
//
// Sequentially it:
//   - Starts a drive-mirror job to synchronize storage via NBD (unless shared-storage mode)
//   - Waits for drive-mirror to reach "ready" (full sync)
//   - Configures migration capabilities (auto-converge, multifd) and parameters
//   - Optionally measures RTT for auto-downtime calculation
//   - Starts RAM migration via QMP migrate command
//   - Polls for the STOP event (VM pause), checking for migration failures
//   - Creates an IP tunnel to forward in-flight traffic to the destination
//   - Waits for migration to complete (query-migrate polling)
//   - If migration failed, cancels it via QMP migrate-cancel
//   - Cancels the drive-mirror block job (disarms the deferred cleanup)
//   - Tears down the IP tunnel after a CNI convergence delay (immediately on failure)
func RunSource(ctx context.Context, cfg SourceConfig) error {
	var resolvedQEMUPID int
	if cfg.PodName != "" {
		ip, err := lookupPodIP(ctx, cfg.PodNamespace, cfg.PodName)
		if err != nil {
			return fmt.Errorf("lookup pod IP: %w", err)
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			return fmt.Errorf("parse resolved pod IP %q: %w", ip, err)
		}
		cfg.VMIP = addr
		res, err := resolveSandbox(sandboxRoot, procImpl, ip)
		if err != nil {
			return fmt.Errorf("resolve sandbox: %w", err)
		}
		// Override QMPSocket in pod mode unless the user supplied an explicit
		// non-default override. The CLI default is `/run/vc/vm/extra-monitor.sock`
		// (no sandbox UUID) — anything matching that gets replaced with the
		// resolved sandbox-specific path.
		if cfg.QMPSocket == "" || cfg.QMPSocket == "/run/vc/vm/extra-monitor.sock" {
			cfg.QMPSocket = filepath.Join(sandboxRoot, res.Sandbox, "extra-monitor.sock")
		}
		resolvedQEMUPID = res.PID
		// Emit netns/iface so the orchestrator (deploy/migrate.sh) can pass them
		// to the dest job. Format is parser-friendly and unique per migration.
		fmt.Printf("KATAMARAN_RESOLVED tap_netns=/proc/%d/ns/net tap=tap0_kata\n", res.PID)
		// Remove the kata-installed tc mirred ingress filter on the pod's eth0,
		// which redirects ALL ingress to tap0_kata and breaks QEMU's outbound
		// TCP migration stream. Mirrors scripts/e2e.sh:454. Best-effort: a pod
		// without the filter (e.g. host-network) is fine.
		netnsPath := fmt.Sprintf("/proc/%d/ns/net", res.PID)
		if err := runCmd(ctx, "nsenter", "--net="+netnsPath, "tc", "filter", "del", "dev", "eth0", "ingress"); err != nil {
			slog.Warn("tc filter del eth0 ingress failed (probably already absent)", "error", err)
		} else {
			slog.Info("Removed kata tc mirred ingress filter on eth0", "netns", netnsPath)
		}
	}

	// Capture the QEMU cmdline for the dest job to replay with -incoming defer.
	// Done after pod resolution (when the QEMU PID is known) and before any
	// QMP work, so the orchestrator can ship the file to the dest node while
	// the dest job is still starting up. Failure here is fatal: a downstream
	// dest job in replay mode will be unable to start QEMU without this file.
	if cfg.EmitCmdlineTo != "" {
		if resolvedQEMUPID == 0 {
			return fmt.Errorf("--emit-cmdline-to requires pod-mode (--pod-name) so the QEMU PID can be resolved")
		}
		if err := captureSourceCmdline(resolvedQEMUPID, cfg.EmitCmdlineTo); err != nil {
			return fmt.Errorf("capture source QEMU cmdline: %w", err)
		}
		// Marker line consumed by deploy/migrate.sh — print on stdout so it
		// survives log re-formatting (slog writes to stderr in this binary).
		fmt.Printf("KATAMARAN_CMDLINE_AT=%s\n", cfg.EmitCmdlineTo)
		slog.Info("Captured source QEMU cmdline", "path", cfg.EmitCmdlineTo, "qemu_pid", resolvedQEMUPID)
	}

	cfg.DestIP = cfg.DestIP.Unmap()
	cfg.VMIP = cfg.VMIP.Unmap()
	if !cfg.DestIP.IsValid() {
		return fmt.Errorf("invalid destination address: %s", cfg.DestIP)
	}
	if !cfg.VMIP.IsValid() {
		return fmt.Errorf("invalid VM address: %s", cfg.VMIP)
	}
	if cfg.DestIP.Is4() != cfg.VMIP.Is4() {
		return fmt.Errorf("destination (%s) and VM (%s) address families must match", cfg.DestIP, cfg.VMIP)
	}
	if cfg.TunnelMode == "" {
		cfg.TunnelMode = TunnelModeIPIP
	}
	if cfg.TunnelMode != TunnelModeIPIP && cfg.TunnelMode != TunnelModeGRE && cfg.TunnelMode != TunnelModeNone {
		return fmt.Errorf("invalid tunnel mode: %q", cfg.TunnelMode)
	}
	if cfg.MultifdChannels < 0 {
		return fmt.Errorf("multifd channels must be non-negative, got %d", cfg.MultifdChannels)
	}
	if !cfg.SharedStorage {
		if err := validateDriveID(cfg.DriveID); err != nil {
			return fmt.Errorf("validating drive ID: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, migrationTimeout+storageSyncTimeout)
	defer cancel()

	// In replay-cmdline mode the dest job starts AFTER us (the orchestrator
	// needs our captured cmdline to spawn dest QEMU). Block here until the
	// dest's RAM-migration TCP port becomes connectable, otherwise the very
	// first migrate-incoming-driven QMP we issue would race the dest pod's
	// startup and fail. The wait is bounded so a botched orchestration does
	// not hang the source job indefinitely.
	if cfg.EmitCmdlineTo != "" {
		// In replay-cmdline mode we cannot TCP-probe dest:4444: the probe
		// connects, sends nothing, closes. Dest QEMU's incoming-migration
		// listener peeks for the migration magic on every connection, sees
		// EOF, and dies with "Failed to peek at channel". Instead, sleep a
		// fixed conservative window for dest QEMU to come up, then let the
		// QMP `migrate` command be the first connection.
		slog.Info("Waiting (sleep) for dest QEMU to come up",
			"sleep", destReplaySleep)
		select {
		case <-time.After(destReplaySleep):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	migrationStart := time.Now()

	slog.Info("Starting live migration",
		"qmp_socket", cfg.QMPSocket,
		"dest_ip", cfg.DestIP,
		"vm_ip", cfg.VMIP,
		"tunnel_mode", string(cfg.TunnelMode),
		"shared_storage", cfg.SharedStorage,
		"multifd_channels", cfg.MultifdChannels,
		"downtime_limit_ms", cfg.DowntimeLimitMS,
		"auto_downtime", cfg.AutoDowntime,
	)

	client, err := qmp.NewClient(ctx, cfg.QMPSocket)
	if err != nil {
		return fmt.Errorf("connecting to source QMP: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			slog.Warn("Failed to close QMP client", "error", err)
		}
	}()

	jobID := "mirror-" + cfg.DriveID
	mirrorStarted := false
	downtimeLimitMS := cfg.DowntimeLimitMS

	if !cfg.SharedStorage {
		targetNBD := fmt.Sprintf("nbd:%s:%s:exportname=%s", formatQEMUHost(cfg.DestIP), nbdPort, cfg.DriveID)
		slog.Info("Initiating storage mirror (drive-mirror)", "target", targetNBD, "drive_id", cfg.DriveID)
		if _, err = client.Execute(ctx, "drive-mirror", qmp.DriveMirrorArgs{
			Device: cfg.DriveID,
			Target: targetNBD,
			Sync:   "full",
			Mode:   "existing", // target is an NBD export, not a new file
			JobID:  jobID,
		}); err != nil {
			slog.Error("Drive-mirror failed", "target", targetNBD, "drive_id", cfg.DriveID, "error", err)
			return fmt.Errorf("starting drive-mirror: %w", err)
		}
		mirrorStarted = true

		defer func() {
			if mirrorStarted {
				cctx, ccancel := cleanupCtx(ctx)
				defer ccancel()
				if _, cancelErr := client.Execute(cctx, "block-job-cancel", qmp.BlockJobCancelArgs{
					Device: jobID,
					Force:  true,
				}); cancelErr != nil {
					slog.Warn("Deferred block job cancel failed", "error", cancelErr)
				}
			}
		}()

		slog.Info("Waiting for storage mirror to synchronize")
		storageSyncStart := time.Now()
		if err = waitForStorageSync(ctx, client, jobID); err != nil {
			return fmt.Errorf("storage sync failed after %s: %w", time.Since(storageSyncStart).Round(time.Millisecond), err)
		}
		slog.Info("Storage mirror synchronized", "elapsed", time.Since(storageSyncStart).Round(time.Millisecond))
	} else {
		slog.Info("Shared storage mode: skipping drive-mirror")
	}

	slog.Info("Configuring RAM migration")
	// Always enable auto-converge: if the guest's dirty page rate exceeds the
	// transfer rate, QEMU will throttle guest vCPUs to ensure migration converges.
	// Without this, migration could run indefinitely on write-heavy workloads.
	caps := []qmp.MigrationCapability{
		{Capability: "auto-converge", State: true},
	}
	if cfg.MultifdChannels > 0 {
		caps = append(caps, qmp.MigrationCapability{Capability: "multifd", State: true})
		slog.Info("Multifd enabled", "channels", cfg.MultifdChannels)
	}
	if _, err = client.Execute(ctx, "migrate-set-capabilities", qmp.MigrateSetCapabilitiesArgs{
		Capabilities: caps,
	}); err != nil {
		return fmt.Errorf("setting migration capabilities: %w", err)
	}

	if cfg.AutoDowntime {
		rtt, err := measureRTTFunc(cfg.DestIP)
		if err != nil {
			slog.Warn("Failed to measure RTT for auto-downtime, using fallback", "error", err, "fallback_ms", downtimeLimitMS)
		} else {
			calculatedDowntime := int(rtt.Milliseconds()*rttMultiplier) + rttMinOverheadMS
			slog.Info("Auto-calculated downtime limit", "downtime_ms", calculatedDowntime, "rtt_ms", rtt.Milliseconds())
			downtimeLimitMS = calculatedDowntime
		}
	}

	if _, err = client.Execute(ctx, "migrate-set-parameters", qmp.MigrateSetParametersArgs{
		DowntimeLimit:   int64(downtimeLimitMS),
		MaxBandwidth:    maxBandwidth,
		MultifdChannels: int64(cfg.MultifdChannels),
	}); err != nil {
		return fmt.Errorf("setting migration parameters: %w", err)
	}

	uri := fmt.Sprintf("tcp:%s:%s", formatQEMUHost(cfg.DestIP), ramMigrationPort)
	if _, err = client.Execute(ctx, "migrate", qmp.MigrateArgs{URI: uri}); err != nil {
		return fmt.Errorf("starting RAM migration to %s: %w", uri, err)
	}
	slog.Info("RAM migration started. Waiting for VM to pause (STOP event)")

	// Wait for the STOP event (downtime window begins).
	// We poll migration status sequentially in the same loop rather than using a
	// separate goroutine for WaitForEvent vs query-migrate. This prevents QMP
	// socket data races and ensures we detect silent migration failures.
	var lastLoggedStatus qmp.MigrateStatus
	var lastLoggedRemaining int64
	var queryErrors int
stopLoop:
	for {
		err = client.WaitForEvent(ctx, "STOP", migrationPollInterval)
		if err == nil {
			break // Success: VM stopped.
		}

		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// Check if the background migration process failed.
			raw, qerr := client.Execute(ctx, "query-migrate", nil)
			if qerr != nil {
				queryErrors++
				lvl := slog.LevelDebug
				if queryErrors >= 10 {
					lvl = slog.LevelWarn
				}
				slog.Log(ctx, lvl, "Transient query-migrate error during STOP polling", "error", qerr, "consecutive_errors", queryErrors)
				continue
			}
			var info qmp.MigrateInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				queryErrors++
				lvl := slog.LevelDebug
				if queryErrors >= 10 {
					lvl = slog.LevelWarn
				}
				slog.Log(ctx, lvl, "Failed to parse query-migrate response", "error", err, "consecutive_errors", queryErrors)
				continue
			}
			queryErrors = 0
			// Log only on status change or significant progress (remaining bytes halved).
			statusChanged := info.Status != lastLoggedStatus
			remainingChanged := lastLoggedRemaining > 0 && info.RAM.Remaining <= lastLoggedRemaining/2
			if statusChanged || remainingChanged {
				var pct float64
				if info.RAM.Total > 0 {
					pct = float64(info.RAM.Transferred) / float64(info.RAM.Total) * 100
				}
				slog.Info("Migration progress", "status", info.Status, "progress_pct", pct, "ram_transferred", info.RAM.Transferred, "ram_total", info.RAM.Total, "ram_remaining", info.RAM.Remaining)
				lastLoggedStatus = info.Status
				lastLoggedRemaining = info.RAM.Remaining
			}
			if terminal, termErr := migrationTerminalError(info.Status, info.ErrorDesc); terminal {
				if termErr != nil {
					return fmt.Errorf("during STOP polling: %w", termErr)
				}
				slog.Warn("Migration completed without explicit STOP event", "status", info.Status)
				break stopLoop
			}
			continue
		}
		return fmt.Errorf("unexpected error waiting for STOP event: %w", err)
	}

	slog.Info("VM paused. Redirecting in-flight packets to destination")

	tunnelCreated := false
	var tunnelName string
	if cfg.TunnelMode == TunnelModeNone {
		slog.Info("Tunnel mode 'none': skipping IP tunnel setup")
	} else if name, genErr := generateTunnelName(); genErr != nil {
		return fmt.Errorf("generating tunnel name: %w", genErr)
	} else if err := setupTunnel(ctx, cfg.DestIP, cfg.VMIP, cfg.TunnelMode, name); err != nil {
		return fmt.Errorf("failed to create IP tunnel: %w", err)
	} else {
		tunnelName = name
		tunnelCreated = true
		slog.Info("IP tunnel established. Traffic redirected", "tunnel", tunnelName)
	}
	slog.Info("Waiting for migration to complete")

	migrationErr := waitForMigrationComplete(ctx, client)

	if migrationErr == nil {
		// Capture actual migration metrics from QEMU.
		raw, qerr := client.Execute(ctx, "query-migrate", nil)
		if qerr != nil {
			slog.Warn("Failed to capture migration metrics", "error", qerr)
		} else {
			var info qmp.MigrateInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				slog.Warn("Failed to parse migration metrics", "error", err)
			} else {
				slog.Info("Migration completed", "actual_downtime_ms", info.Downtime, "total_time_ms", info.TotalTime, "setup_time_ms", info.SetupTime, "ram_transferred", info.RAM.Transferred, "ram_total", info.RAM.Total)
			}
		}
	}

	if migrationErr != nil {
		cctx, ccancel := cleanupCtx(ctx)
		defer ccancel()
		if _, cancelErr := client.Execute(cctx, "migrate-cancel", nil); cancelErr != nil {
			slog.Warn("Failed to cancel migration", "error", cancelErr)
		} else {
			slog.Info("Migration cancelled via QMP")
		}
	}

	if !cfg.SharedStorage {
		cctx, ccancel := cleanupCtx(ctx)
		defer ccancel()
		if _, err := client.Execute(cctx, "block-job-cancel", qmp.BlockJobCancelArgs{
			Device: jobID,
			Force:  true,
		}); err != nil {
			slog.Warn("Failed to cancel block job", "error", err)
		} else {
			mirrorStarted = false
			slog.Info("Storage mirror cancelled")
		}
	}

	if tunnelCreated {
		if migrationErr == nil {
			slog.Info("Waiting for CNI convergence", "delay", postMigrationTunnelDelay)
			timer := time.NewTimer(postMigrationTunnelDelay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			}
		}
		cctx, ccancel := cleanupCtx(ctx)
		defer ccancel()
		teardownTunnel(cctx, tunnelName)
	}

	if migrationErr != nil {
		slog.Error("Migration failed", "error", migrationErr, "elapsed", time.Since(migrationStart).Round(time.Millisecond))
		return migrationErr
	}

	slog.Info("Source cleanup complete. Migration succeeded", "elapsed", time.Since(migrationStart).Round(time.Millisecond))
	return nil
}

var measureRTTFunc = measureRTT

// measureRTT estimates network round-trip time to the destination by performing
// TCP handshake timing against the RAM migration port, used for auto-downtime
// calculation. Returns the best (lowest) of three successful samples; any
// failed sample aborts the measurement so callers can fall back explicitly.
func measureRTT(destIP netip.Addr) (time.Duration, error) {
	const samples = 3
	addr := net.JoinHostPort(destIP.String(), ramMigrationPort)
	var best time.Duration

	for i := 0; i < samples; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, rttDialTimeout)
		if err != nil {
			return 0, fmt.Errorf("RTT sample %d/%d failed: %w", i+1, samples, err)
		}
		rtt := time.Since(start)
		if cerr := conn.Close(); cerr != nil {
			slog.Debug("RTT probe connection close error", "sample", i+1, "error", cerr)
		}
		slog.Debug("RTT sample", "sample", i+1, "of", samples, "rtt", rtt)
		if i == 0 || rtt < best {
			best = rtt
		}
	}

	slog.Info("RTT measurement complete", "best", best, "samples", samples)
	return best, nil
}

// migrationTerminalError checks if a migration status indicates a terminal
// state. Returns (true, nil) for completed, (true, err) for failed/cancelled,
// and (false, nil) for in-progress states.
func migrationTerminalError(status qmp.MigrateStatus, errorDesc string) (bool, error) {
	switch status {
	case qmp.MigrateStatusCompleted:
		return true, nil
	case qmp.MigrateStatusFailed:
		if errorDesc != "" {
			return true, fmt.Errorf("%w: %s", errMigrationFailed, errorDesc)
		}
		return true, errMigrationFailed
	case qmp.MigrateStatusCancelled:
		return true, errMigrationCancelled
	}
	return false, nil
}

// waitForStorageSync polls query-block-jobs until the drive-mirror job reaches
// the "ready" state, indicating full synchronization. Fails if the job disappears,
// never appears within jobAppearTimeout, or reaches a terminal failure state.
func waitForStorageSync(ctx context.Context, client *qmp.Client, jobID string) error {
	ctx, cancel := context.WithTimeout(ctx, storageSyncTimeout)
	defer cancel()

	jobSeen := false
	appearDeadline := time.Now().Add(jobAppearTimeout)
	ticker := time.NewTicker(storagePollInterval)
	defer ticker.Stop()

	lastLoggedPct := -1.0
	var lastLoggedOffset int64
	var lastLoggedTime time.Time

	for {
		if ctx.Err() != nil {
			slog.Warn("Storage sync timed out", "job_id", jobID)
			return fmt.Errorf("storage sync: %w", ctx.Err())
		}

		raw, err := client.Execute(ctx, "query-block-jobs", nil)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("storage sync: %w", ctx.Err())
			}
			return fmt.Errorf("querying block jobs: %w", err)
		}
		var jobs []qmp.BlockJobInfo
		if err = json.Unmarshal(raw, &jobs); err != nil {
			return fmt.Errorf("unmarshaling block jobs: %w", err)
		}
		var job *qmp.BlockJobInfo
		if idx := slices.IndexFunc(jobs, func(j qmp.BlockJobInfo) bool { return j.Device == jobID }); idx != -1 {
			job = &jobs[idx]
		}
		if job == nil {
			if jobSeen {
				return fmt.Errorf("block mirror job %q disappeared", jobID)
			}
			if time.Now().After(appearDeadline) {
				return fmt.Errorf("block mirror job %q did not appear", jobID)
			}
		} else {
			jobSeen = true
			if job.Ready {
				return nil
			}
			if job.Len > 0 {
				pct := float64(job.Offset) / float64(job.Len) * 100
				if pct-lastLoggedPct >= 5 || lastLoggedPct < 0 {
					attrs := []any{"job_id", jobID, "progress_pct", pct, "offset", job.Offset, "len", job.Len}
					if !lastLoggedTime.IsZero() && job.Offset > lastLoggedOffset {
						dt := time.Since(lastLoggedTime).Seconds()
						if dt > 0 {
							mbps := float64(job.Offset-lastLoggedOffset) / dt / (1024 * 1024)
							attrs = append(attrs, "throughput_mbps", mbps)
						}
					}
					if job.Speed > 0 {
						attrs = append(attrs, "speed_limit", job.Speed)
					}
					slog.Info("Storage sync progress", attrs...)
					lastLoggedPct = pct
					lastLoggedOffset = job.Offset
					lastLoggedTime = time.Now()
				}
			}
			if job.Status == qmp.BlockJobStatusConcluded || job.Status == qmp.BlockJobStatusNull {
				return fmt.Errorf("block mirror job %q failed (status=%s)", jobID, job.Status)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("storage sync: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// queryMigrateTimeout caps each individual query-migrate call. Far below
// the global executeTimeout so a stalled QMP socket (typical post-handover
// behavior on the source side as kata-shim cleans up the source QEMU)
// fails fast and the polling loop's retry/grace logic takes over.
const queryMigrateTimeout = 5 * time.Second

// postActiveStallGrace is how long waitForMigrationComplete tolerates
// consecutive query-migrate failures AFTER the migration was last seen in
// the "active" state before declaring success. The dest side has the
// authoritative completion signal (RESUME event); on the source side the
// QMP socket frequently goes silent once kata-shim notices the source
// QEMU has handed over and starts tearing it down. Treating that stall as
// success matches the orchestrator's dual-poll outcome matrix.
const postActiveStallGrace = 30 * time.Second

// waitForMigrationComplete polls query-migrate until migration reaches a terminal
// state (completed, failed, or cancelled). Times out after migrationTimeout.
//
// Each poll uses a per-call queryMigrateTimeout — a stalled QMP socket
// fails this short call rather than hanging the whole polling loop on the
// global executeTimeout (2 min). After the migration has been "active"
// once, sustained QMP failures are interpreted as a successful handover
// (kata-shim tearing down source QEMU post-completion).
func waitForMigrationComplete(ctx context.Context, client *qmp.Client) error {
	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()

	ticker := time.NewTicker(migrationPollInterval)
	defer ticker.Stop()

	var prevStatus qmp.MigrateStatus
	var lastLoggedRemaining int64
	sawActive := false
	var firstStallAt time.Time

	for {
		if ctx.Err() != nil {
			slog.Warn("Migration timed out during completion wait", "last_status", prevStatus)
			return fmt.Errorf("migration: %w", ctx.Err())
		}

		queryCtx, queryCancel := context.WithTimeout(ctx, queryMigrateTimeout)
		raw, err := client.Execute(queryCtx, "query-migrate", nil)
		queryCancel()
		if err != nil {
			if ctx.Err() != nil {
				slog.Warn("Migration timed out during completion wait", "last_status", prevStatus)
				return fmt.Errorf("migration: %w", ctx.Err())
			}
			// Per-call timeout or transient QMP error.
			if sawActive {
				if firstStallAt.IsZero() {
					firstStallAt = time.Now()
				}
				if time.Since(firstStallAt) > postActiveStallGrace {
					slog.Info("Source QMP stalled after migration was active; assuming completed (kata-shim teardown)",
						"last_status", prevStatus, "stall", time.Since(firstStallAt).Round(time.Millisecond))
					return nil
				}
				slog.Warn("query-migrate stalled (will retry; assume completed if grace exceeded)",
					"error", err, "last_status", prevStatus, "stall", time.Since(firstStallAt).Round(time.Millisecond))
			} else {
				return fmt.Errorf("querying migration status: %w", err)
			}
		} else {
			firstStallAt = time.Time{} // reset on any successful query
			var info qmp.MigrateInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				return fmt.Errorf("unmarshaling migration status: %w", err)
			}
			if info.Status == qmp.MigrateStatusActive {
				sawActive = true
			}
			statusChanged := info.Status != prevStatus
			remainingChanged := lastLoggedRemaining > 0 && info.RAM.Remaining <= lastLoggedRemaining/2
			if statusChanged || remainingChanged {
				slog.Info("Migration status", "status", info.Status, "ram_transferred", info.RAM.Transferred, "ram_total", info.RAM.Total, "ram_remaining", info.RAM.Remaining)
				// Stable, parser-friendly progress marker the orchestrator
				// scrapes from pod logs to surface RAM transfer progress
				// without depending on slog's text/json layout.
				fmt.Printf("KATAMARAN_PROGRESS status=%s ram_transferred=%d ram_total=%d ram_remaining=%d\n",
					info.Status, info.RAM.Transferred, info.RAM.Total, info.RAM.Remaining)
				prevStatus = info.Status
				lastLoggedRemaining = info.RAM.Remaining
			}
			if terminal, termErr := migrationTerminalError(info.Status, info.ErrorDesc); terminal {
				return termErr
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("migration: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
