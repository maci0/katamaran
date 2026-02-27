package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"katamaran/internal/qmp"
)

// Sentinel errors for migration terminal states.
var (
	ErrMigrationFailed    = errors.New("migration failed")
	ErrMigrationCancelled = errors.New("migration cancelled")
)

// RunSource initiates live migration from the source node to the destination.
//
// If drive-mirror is started (non-shared-storage mode), a deferred cleanup
// ensures the block job is cancelled on any early return, preventing resource
// leaks. The deferred cancel uses force:true to avoid accidentally pivoting
// the mirror, and is disarmed when step 8 handles it explicitly.
//
// Sequentially it:
//  1. Starts drive-mirror to replicate the block device via NBD (unless shared-storage)
//  2. Waits for the mirror to reach "ready" (fully synchronized)
//  3. Configures and starts RAM pre-copy migration with auto-converge
//  4. Waits for VM pause (STOP event — downtime window begins)
//  5. Creates an IP tunnel to forward in-flight traffic to destination
//  6. Monitors migration until completion
//  7. Cancels migration via migrate_cancel if it failed or timed out
//  8. Aborts the block job to stop the mirror (unless shared-storage)
//  9. Tears down the IP tunnel after CNI convergence delay
func RunSource(ctx context.Context, qmpSocket, destIP, vmIP, driveID string, sharedStorage bool, tunnelMode string) error {
	log.Printf("Starting live migration to %s...", destIP)

	client, err := qmp.NewClient(ctx, qmpSocket)
	if err != nil {
		return fmt.Errorf("connecting to source QMP: %w", err)
	}
	defer client.Close()

	jobID := "mirror-" + driveID
	mirrorStarted := false

	if !sharedStorage {
		// Step 1: Initiate drive-mirror to the destination's NBD server.
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

		// Ensure the block job is cancelled if we return early due to an error
		// in a later step. This prevents leaking a running drive-mirror job.
		// Uses force:true to avoid accidentally pivoting the mirror to the
		// destination disk — we want an immediate abort, not a graceful finish.
		defer func() {
			if mirrorStarted {
				cctx, ccancel := CleanupCtx()
				defer ccancel()
				if _, cancelErr := client.Execute(cctx, "block-job-cancel", qmp.BlockJobCancelArgs{
					Device: jobID,
					Force:  true,
				}); cancelErr != nil {
					log.Printf("Warning: deferred block job cancel for %q failed: %v", jobID, cancelErr)
				}
			}
		}()

		// Step 2: Poll until the mirror reports ready (fully synchronized).
		log.Println("Waiting for storage mirror to synchronize...")
		if err = waitForStorageSync(ctx, client, jobID); err != nil {
			return fmt.Errorf("storage sync failed: %w", err)
		}
	} else {
		log.Println("Shared storage mode: skipping drive-mirror.")
	}

	// Step 3: Configure and start RAM pre-copy migration.
	log.Println("Configuring RAM migration...")
	if _, err = client.Execute(ctx, "migrate-set-capabilities", qmp.MigrateSetCapabilitiesArgs{
		Capabilities: []qmp.MigrationCapability{
			{Capability: "auto-converge", State: true},
		},
	}); err != nil {
		return fmt.Errorf("setting migration capabilities: %w", err)
	}

	// Enforce strict downtime limits for "zero downtime" perception:
	// 50ms max pause ensures the STOP→RESUME gap is imperceptible.
	// 10 GB/s bandwidth cap ensures final dirty pages flush instantly.
	if _, err = client.Execute(ctx, "migrate-set-parameters", qmp.MigrateSetParametersArgs{
		DowntimeLimit: MaxDowntimeMS,
		MaxBandwidth:  MaxBandwidth,
	}); err != nil {
		return fmt.Errorf("setting migration parameters: %w", err)
	}

	uri := fmt.Sprintf("tcp:%s:%s", FormatQEMUHost(destIP), RAMMigrationPort)
	if _, err = client.Execute(ctx, "migrate", qmp.MigrateArgs{URI: uri}); err != nil {
		return fmt.Errorf("starting RAM migration to %s: %w", uri, err)
	}
	log.Println("RAM migration started. Waiting for VM to pause (STOP event)...")

	// Step 4: Wait for the STOP event (downtime window begins).
	// At this point QEMU performs a final incremental copy of the remaining
	// dirty RAM pages and any in-flight storage blocks.
	if err = client.WaitForEvent(ctx, "STOP", EventWaitTimeout); err != nil {
		return fmt.Errorf("waiting for STOP event: %w", err)
	}
	log.Println("VM paused. Redirecting in-flight packets to destination...")

	// Step 5: Create an IP tunnel to forward traffic during CNI convergence.
	// This bridges the gap between VM cutover and CNI route propagation for
	// all supported plugins (Cilium, Calico, Flannel, OVN-Kubernetes, Kube-OVN).
	// The setup is idempotent — any stale tunnel from a previous run is
	// removed before creation.
	tunnelCreated := false
	if err := SetupTunnel(ctx, destIP, vmIP, tunnelMode); err != nil {
		log.Printf("Warning: failed to create IP tunnel: %v", err)
	} else {
		tunnelCreated = true
		log.Println("IP tunnel established. Traffic redirected.")
	}
	log.Println("Waiting for migration to complete...")

	// Step 6: Monitor migration status until completion or failure.
	migrationErr := waitForMigrationComplete(ctx, client)

	// Step 7: If migration failed or timed out, explicitly cancel it so QEMU
	// stops the in-progress migration and resumes the source VM. Without this,
	// the source VM stays paused and the migration stream keeps running.
	if migrationErr != nil {
		cctx, ccancel := CleanupCtx()
		if _, cancelErr := client.Execute(cctx, "migrate_cancel", nil); cancelErr != nil {
			log.Printf("Warning: failed to cancel migration after error: %v", cancelErr)
		} else {
			log.Println("Migration cancelled after failure.")
		}
		ccancel()
	}

	// Always attempt cleanup regardless of migration outcome.
	// This ensures we don't leak the IP tunnel or leave block jobs running.
	if !sharedStorage {
		// Step 8: Abort the block job to stop the mirror.
		// With force:true, QEMU immediately cancels the job without
		// waiting for in-flight I/O or attempting to pivot the mirror.
		// This matches the deferred cleanup behavior. Without force,
		// QEMU may attempt to complete pending writes which can hang
		// if the NBD target is already gone.
		// Disarm the deferred safety cancel since we're handling it explicitly.
		mirrorStarted = false
		cctx, ccancel := CleanupCtx()
		if _, err := client.Execute(cctx, "block-job-cancel", qmp.BlockJobCancelArgs{
			Device: jobID,
			Force:  true,
		}); err != nil {
			log.Printf("Warning: failed to cancel block job %q: %v", jobID, err)
		} else {
			log.Println("Storage mirror cancelled.")
		}
		ccancel()
	}

	// Step 9: Tear down the IP tunnel after allowing CNI to converge.
	if tunnelCreated {
		if migrationErr == nil {
			log.Printf("Waiting %v for CNI convergence before removing tunnel...", PostMigrationTunnelDelay)

			// Try to respect context cancellation during the delay, but we MUST
			// still tear down the tunnel. Use a select to wait.
			select {
			case <-ctx.Done():
				log.Println("Context cancelled during CNI convergence wait; tearing down early.")
			case <-time.After(PostMigrationTunnelDelay):
			}
		}
		cctx, ccancel := CleanupCtx()
		if err := TeardownTunnel(cctx); err != nil {
			log.Printf("Warning: failed to remove IP tunnel: %v", err)
		}
		ccancel()
	}

	if migrationErr != nil {
		return fmt.Errorf("migration failed: %w", migrationErr)
	}

	log.Println("Source cleanup complete. Migration succeeded.")
	return nil
}

// waitForStorageSync polls query-block-jobs until the mirror job with the
// given ID reports ready, indicating the source and destination block devices
// are synchronized. Returns an error if the job enters a terminal error state,
// disappears unexpectedly, fails to appear within JobAppearTimeout, or does
// not become ready within StorageSyncTimeout.
func waitForStorageSync(ctx context.Context, client *qmp.Client, jobID string) error {
	jobSeen := false
	appearDeadline := time.Now().Add(JobAppearTimeout)
	syncDeadline := time.Now().Add(StorageSyncTimeout)

	ticker := time.NewTicker(StoragePollInterval)
	defer ticker.Stop()

	for {
		raw, err := client.Execute(ctx, "query-block-jobs", nil)
		if err != nil {
			return fmt.Errorf("querying block jobs: %w", err)
		}

		var jobs []qmp.BlockJobInfo
		if err = json.Unmarshal(raw, &jobs); err != nil {
			return fmt.Errorf("unmarshaling block jobs response: %w", err)
		}

		// Find our specific mirror job by ID.
		var job *qmp.BlockJobInfo
		for i := range jobs {
			if jobs[i].Device == jobID {
				job = &jobs[i]
				break
			}
		}

		if job == nil {
			if jobSeen {
				// Job was running but has disappeared — it concluded (error or cancel).
				return fmt.Errorf("block mirror job %q disappeared unexpectedly (may have failed or been cancelled)", jobID)
			}
			// Job hasn't appeared yet; check if we've exceeded the appearance timeout.
			if time.Now().After(appearDeadline) {
				return fmt.Errorf("block mirror job %q did not appear within %v (drive-mirror may have failed silently)", jobID, JobAppearTimeout)
			}
		} else {
			jobSeen = true

			if job.Len > 0 {
				progress := float64(job.Offset) / float64(job.Len) * 100
				log.Printf("Storage sync progress: %.2f%%", progress)
			}

			if job.Ready {
				log.Println("Storage mirror synchronized (BLOCK_JOB_READY).")
				return nil
			}

			// Detect terminal error states reported by QEMU block jobs.
			switch job.Status {
			case "concluded", "null":
				return fmt.Errorf("block mirror job %q entered terminal state %q without becoming ready", jobID, job.Status)
			}
		}

		if time.Now().After(syncDeadline) {
			return fmt.Errorf("storage sync for job %q did not complete within %v", jobID, StorageSyncTimeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitForMigrationComplete polls query-migrate until the migration status
// reaches a terminal state ("completed", "failed", or "cancelled"), or the
// MigrationTimeout is exceeded. The timeout prevents infinite polling if
// migration never converges (e.g., perpetual dirty page churn).
func waitForMigrationComplete(ctx context.Context, client *qmp.Client) error {
	deadline := time.Now().Add(MigrationTimeout)

	ticker := time.NewTicker(MigrationPollInterval)
	defer ticker.Stop()

	for {
		raw, err := client.Execute(ctx, "query-migrate", nil)
		if err != nil {
			return fmt.Errorf("querying migration status: %w", err)
		}

		var info qmp.MigrateInfo
		if err = json.Unmarshal(raw, &info); err != nil {
			return fmt.Errorf("unmarshaling migration status: %w", err)
		}

		log.Printf("Migration status: %s", info.Status)

		switch info.Status {
		case "completed":
			return nil
		case "failed":
			if info.ErrorDesc != "" {
				return fmt.Errorf("%w: %s", ErrMigrationFailed, info.ErrorDesc)
			}
			return ErrMigrationFailed
		case "cancelled":
			return ErrMigrationCancelled
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("migration did not complete within %v (last status: %s)", MigrationTimeout, info.Status)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
