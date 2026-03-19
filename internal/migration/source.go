package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/maci0/katamaran/internal/qmp"
)

// Sentinel errors for migration terminal states.
var (
	ErrMigrationFailed    = errors.New("migration failed")
	ErrMigrationCancelled = errors.New("migration cancelled")
)

// RunSource initiates live migration from the source node to the destination.
func RunSource(ctx context.Context, qmpSocket string, destIP, vmIP netip.Addr, driveID string, sharedStorage bool, tunnelMode TunnelMode, downtimeLimitMS int, autoDowntime bool) error {
	ctx, cancel := context.WithTimeout(ctx, MigrationTimeout+StorageSyncTimeout)
	defer cancel()

	log.Printf("Starting live migration to %s...", destIP)

	client, err := qmp.NewClient(ctx, qmpSocket)
	if err != nil {
		return fmt.Errorf("connecting to source QMP: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("Warning: closing QMP client: %v", err)
		}
	}()

	jobID := "mirror-" + driveID
	mirrorStarted := false

	if !sharedStorage {
		log.Println("Initiating storage mirror (drive-mirror)...")
		targetNBD := fmt.Sprintf("nbd:%s:%s:exportname=%s", FormatQEMUHost(destIP), NBDPort, driveID)
		if _, err = client.Execute(ctx, "drive-mirror", qmp.DriveMirrorArgs{
			Device: driveID,
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
				cctx, ccancel := CleanupCtx(ctx)
				defer ccancel()
				if _, cancelErr := client.Execute(cctx, "block-job-cancel", qmp.BlockJobCancelArgs{
					Device: jobID,
					Force:  true,
				}); cancelErr != nil {
					log.Printf("Warning: deferred block job cancel failed: %v", cancelErr)
				}
			}
		}()

		log.Println("Waiting for storage mirror to synchronize...")
		if err = waitForStorageSync(ctx, client, jobID); err != nil {
			return fmt.Errorf("storage sync failed: %w", err)
		}
	} else {
		log.Println("Shared storage mode: skipping drive-mirror.")
	}

	log.Println("Configuring RAM migration...")
	if _, err = client.Execute(ctx, "migrate-set-capabilities", qmp.MigrateSetCapabilitiesArgs{
		Capabilities: []qmp.MigrationCapability{
			{Capability: "auto-converge", State: true},
		},
	}); err != nil {
		return fmt.Errorf("setting migration capabilities: %w", err)
	}

	if autoDowntime {
		rtt, err := measureRTT(destIP)
		if err != nil {
			log.Printf("Warning: failed to measure RTT for auto-downtime: %v. Falling back to %d ms.", err, downtimeLimitMS)
		} else {
			calculatedDowntime := int(rtt.Milliseconds()*2) + 10
			log.Printf("Auto-calculated downtime limit: %dms (based on RTT: %dms)", calculatedDowntime, rtt.Milliseconds())
			downtimeLimitMS = calculatedDowntime
		}
	}

	if _, err = client.Execute(ctx, "migrate-set-parameters", qmp.MigrateSetParametersArgs{
		DowntimeLimit: int64(downtimeLimitMS),
		MaxBandwidth:  MaxBandwidth,
	}); err != nil {
		return fmt.Errorf("setting migration parameters: %w", err)
	}

	uri := fmt.Sprintf("tcp:%s:%s", FormatQEMUHost(destIP), RAMMigrationPort)
	if _, err = client.Execute(ctx, "migrate", qmp.MigrateArgs{URI: uri}); err != nil {
		return fmt.Errorf("starting RAM migration to %s: %w", uri, err)
	}
	log.Println("RAM migration started. Waiting for VM to pause (STOP event)...")

	// Wait for the STOP event (downtime window begins).
	// We poll migration status sequentially in the same loop rather than using a
	// separate goroutine for WaitForEvent vs query-migrate. This prevents QMP
	// socket data races and ensures we detect silent migration failures.
stopLoop:
	for {
		err = client.WaitForEvent(ctx, "STOP", MigrationPollInterval)
		if err == nil {
			break // Success: VM stopped.
		}

		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// Check if the background migration process failed.
			raw, qerr := client.Execute(ctx, "query-migrate", nil)
			if qerr != nil {
				continue // Ignore transient query errors.
			}
			var info qmp.MigrateInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				log.Printf("Warning: failed to parse query-migrate response: %v", err)
				continue
			}
			log.Printf("Migration progress: %s (RAM: %d / %d bytes, Remaining: %d bytes)", info.Status, info.RAM.Transferred, info.RAM.Total, info.RAM.Remaining)
			if info.Status == qmp.MigrateStatusFailed {
				return fmt.Errorf("migration background process failed: %s", info.ErrorDesc)
			} else if info.Status == qmp.MigrateStatusCancelled {
				return ErrMigrationCancelled
			} else if info.Status == qmp.MigrateStatusCompleted {
				log.Println("Migration completed without explicit STOP event.")
				break stopLoop
			}
			continue
		}
		return fmt.Errorf("unexpected error waiting for STOP event: %w", err)
	}

	log.Println("VM paused. Redirecting in-flight packets to destination...")

	tunnelCreated := false
	if tunnelMode == TunnelModeNone {
		log.Println("Tunnel mode 'none': skipping IP tunnel setup.")
	} else if err := SetupTunnel(ctx, destIP, vmIP, tunnelMode); err != nil {
		return fmt.Errorf("failed to create IP tunnel: %w", err)
	} else {
		tunnelCreated = true
		log.Println("IP tunnel established. Traffic redirected.")
	}
	log.Println("Waiting for migration to complete...")

	migrationErr := waitForMigrationComplete(ctx, client)

	if migrationErr == nil {
		// Capture actual migration metrics from QEMU.
		if raw, err := client.Execute(ctx, "query-migrate", nil); err == nil {
			var info qmp.MigrateInfo
			if err := json.Unmarshal(raw, &info); err == nil {
				log.Printf("Migration completed: actual_downtime=%dms total_time=%dms setup_time=%dms",
					info.Downtime, info.TotalTime, info.SetupTime)
			}
		}
	}

	if migrationErr != nil {
		cctx, ccancel := CleanupCtx(ctx)
		if _, cancelErr := client.Execute(cctx, "migrate_cancel", nil); cancelErr != nil {
			log.Printf("Warning: failed to cancel migration: %v", cancelErr)
		}
		ccancel()
	}

	if !sharedStorage {
		cctx, ccancel := CleanupCtx(ctx)
		if _, err := client.Execute(cctx, "block-job-cancel", qmp.BlockJobCancelArgs{
			Device: jobID,
			Force:  true,
		}); err != nil {
			log.Printf("Warning: failed to cancel block job: %v", err)
		} else {
			mirrorStarted = false
			log.Println("Storage mirror cancelled.")
		}
		ccancel()
	}

	if tunnelCreated {
		if migrationErr == nil {
			log.Printf("Waiting %v for CNI convergence...", PostMigrationTunnelDelay)
			timer := time.NewTimer(PostMigrationTunnelDelay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			}
		}
		cctx, ccancel := CleanupCtx(ctx)
		TeardownTunnel(cctx)
		ccancel()
	}

	if migrationErr != nil {
		return fmt.Errorf("migration failed: %w", migrationErr)
	}

	log.Println("Source cleanup complete. Migration succeeded.")
	return nil
}

func measureRTT(destIP netip.Addr) (time.Duration, error) {
	const samples = 3
	addr := net.JoinHostPort(destIP.String(), RAMMigrationPort)
	var best time.Duration

	for i := 0; i < samples; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return 0, fmt.Errorf("RTT sample %d/%d failed: %w", i+1, samples, err)
		}
		rtt := time.Since(start)
		conn.Close()
		log.Printf("RTT sample %d/%d: %s", i+1, samples, rtt)
		if i == 0 || rtt < best {
			best = rtt
		}
	}

	log.Printf("RTT best of %d samples: %s", samples, best)
	return best, nil
}

func waitForStorageSync(ctx context.Context, client *qmp.Client, jobID string) error {
	jobSeen := false
	appearDeadline := time.Now().Add(JobAppearTimeout)
	syncDeadline := time.Now().Add(StorageSyncTimeout)
	ticker := time.NewTicker(StoragePollInterval)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		raw, err := client.Execute(ctx, "query-block-jobs", nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
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
				log.Printf("Storage sync progress: 100.00%%")
				log.Println("Storage mirror synchronized.")
				return nil
			}
			if job.Len > 0 {
				pct := float64(job.Offset) / float64(job.Len) * 100
				log.Printf("Storage sync progress: %.2f%%", pct)
			}
			if job.Status == qmp.BlockJobStatusConcluded || job.Status == qmp.BlockJobStatusNull {
				return fmt.Errorf("block mirror job %q failed", jobID)
			}
		}
		if time.Now().After(syncDeadline) {
			return fmt.Errorf("storage sync timed out")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForMigrationComplete(ctx context.Context, client *qmp.Client) error {
	deadline := time.Now().Add(MigrationTimeout)
	ticker := time.NewTicker(MigrationPollInterval)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		raw, err := client.Execute(ctx, "query-migrate", nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("querying migration status: %w", err)
		}
		var info qmp.MigrateInfo
		if err = json.Unmarshal(raw, &info); err != nil {
			return fmt.Errorf("unmarshaling migration status: %w", err)
		}
		log.Printf("Migration status: %s", info.Status)
		switch info.Status {
		case qmp.MigrateStatusCompleted:
			return nil
		case qmp.MigrateStatusFailed:
			if info.ErrorDesc != "" {
				return fmt.Errorf("%w: %s", ErrMigrationFailed, info.ErrorDesc)
			}
			return ErrMigrationFailed
		case qmp.MigrateStatusCancelled:
			return ErrMigrationCancelled
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("migration timed out (status: %s)", info.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
