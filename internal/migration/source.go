package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/maci0/katamaran/internal/qmp"
)

// Sentinel errors for migration terminal states.
var (
	errMigrationFailed    = errors.New("migration failed")
	errMigrationCancelled = errors.New("migration cancelled")
)

// RunSource initiates live migration from the source node to the destination.
//
// Deferred cleanups ensure the drive-mirror job and tunnel are torn down on
// any early return, preventing resource leaks. They are disarmed on the
// success path.
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
//   - Cancels the drive-mirror block job
//   - Tears down the IP tunnel after a CNI convergence delay
func RunSource(ctx context.Context, cfg SourceConfig) error {
	ctx, cancel := context.WithTimeout(ctx, migrationTimeout+storageSyncTimeout)
	defer cancel()

	slog.Info("Starting live migration",
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
	multifdChannels := cfg.MultifdChannels

	if !cfg.SharedStorage {
		slog.Info("Initiating storage mirror (drive-mirror)")
		targetNBD := fmt.Sprintf("nbd:%s:%s:exportname=%s", formatQEMUHost(cfg.DestIP), nbdPort, cfg.DriveID)
		if _, err = client.Execute(ctx, "drive-mirror", qmp.DriveMirrorArgs{
			Device: cfg.DriveID,
			Target: targetNBD,
			Sync:   "full",
			Mode:   "existing",
			JobID:  jobID,
		}); err != nil {
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
		if err = waitForStorageSync(ctx, client, jobID); err != nil {
			return fmt.Errorf("storage sync failed: %w", err)
		}
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
	if multifdChannels > 0 {
		caps = append(caps, qmp.MigrationCapability{Capability: "multifd", State: true})
		slog.Info("Multifd enabled", "channels", multifdChannels)
	}
	if _, err = client.Execute(ctx, "migrate-set-capabilities", qmp.MigrateSetCapabilitiesArgs{
		Capabilities: caps,
	}); err != nil {
		return fmt.Errorf("setting migration capabilities: %w", err)
	}

	if cfg.AutoDowntime {
		rtt, err := measureRTT(cfg.DestIP)
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
		MultifdChannels: int64(multifdChannels),
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
				slog.Warn("Transient query-migrate error during STOP polling", "error", qerr)
				continue
			}
			var info qmp.MigrateInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				slog.Warn("Failed to parse query-migrate response", "error", err)
				continue
			}
			slog.Info("Migration progress", "status", info.Status, "ram_transferred", info.RAM.Transferred, "ram_total", info.RAM.Total, "ram_remaining", info.RAM.Remaining)
			switch info.Status {
			case qmp.MigrateStatusFailed:
				if info.ErrorDesc != "" {
					return fmt.Errorf("%w: %s", errMigrationFailed, info.ErrorDesc)
				}
				return errMigrationFailed
			case qmp.MigrateStatusCancelled:
				return errMigrationCancelled
			case qmp.MigrateStatusCompleted:
				slog.Info("Migration completed without explicit STOP event")
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
		if raw, err := client.Execute(ctx, "query-migrate", nil); err == nil {
			var info qmp.MigrateInfo
			if err := json.Unmarshal(raw, &info); err == nil {
				slog.Info("Migration completed", "actual_downtime_ms", info.Downtime, "total_time_ms", info.TotalTime, "setup_time_ms", info.SetupTime)
			}
		}
	}

	if migrationErr != nil {
		cctx, ccancel := cleanupCtx(ctx)
		defer ccancel()
		if _, cancelErr := client.Execute(cctx, "migrate_cancel", nil); cancelErr != nil {
			slog.Warn("Failed to cancel migration", "error", cancelErr)
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
		return fmt.Errorf("migration failed: %w", migrationErr)
	}

	slog.Info("Source cleanup complete. Migration succeeded")
	return nil
}

// measureRTT estimates network round-trip time to the destination by performing
// TCP handshake timing against the RAM migration port. Returns the best (lowest)
// of 3 samples.
func measureRTT(destIP netip.Addr) (time.Duration, error) {
	const samples = 3
	addr := net.JoinHostPort(destIP.String(), ramMigrationPort)
	var best time.Duration

	for i := 0; i < samples; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return 0, fmt.Errorf("RTT sample %d/%d failed: %w", i+1, samples, err)
		}
		rtt := time.Since(start)
		conn.Close()
		slog.Info("RTT sample", "sample", i+1, "of", samples, "rtt", rtt)
		if i == 0 || rtt < best {
			best = rtt
		}
	}

	slog.Info("RTT measurement complete", "best", best, "samples", samples)
	return best, nil
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

	for {
		if ctx.Err() != nil {
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
				slog.Info("Storage mirror synchronized", "progress_pct", 100.0)
				return nil
			}
			if job.Len > 0 {
				pct := float64(job.Offset) / float64(job.Len) * 100
				slog.Info("Storage sync progress", "progress_pct", fmt.Sprintf("%.2f", pct), "offset", job.Offset, "len", job.Len)
			}
			if job.Status == qmp.BlockJobStatusConcluded || job.Status == qmp.BlockJobStatusNull {
				return fmt.Errorf("block mirror job %q failed", jobID)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("storage sync: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// waitForMigrationComplete polls query-migrate until migration reaches a terminal
// state (completed, failed, or cancelled). Times out after migrationTimeout.
func waitForMigrationComplete(ctx context.Context, client *qmp.Client) error {
	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()

	ticker := time.NewTicker(migrationPollInterval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("migration: %w", ctx.Err())
		}

		raw, err := client.Execute(ctx, "query-migrate", nil)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("migration: %w", ctx.Err())
			}
			return fmt.Errorf("querying migration status: %w", err)
		}
		var info qmp.MigrateInfo
		if err = json.Unmarshal(raw, &info); err != nil {
			return fmt.Errorf("unmarshaling migration status: %w", err)
		}
		slog.Info("Migration status", "status", info.Status)
		switch info.Status {
		case qmp.MigrateStatusCompleted:
			return nil
		case qmp.MigrateStatusFailed:
			if info.ErrorDesc != "" {
				return fmt.Errorf("%w: %s", errMigrationFailed, info.ErrorDesc)
			}
			return errMigrationFailed
		case qmp.MigrateStatusCancelled:
			return errMigrationCancelled
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("migration: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
