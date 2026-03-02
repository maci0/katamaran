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
func RunSource(ctx context.Context, qmpSocket string, destIP, vmIP netip.Addr, driveID string, sharedStorage bool, tunnelMode string, downtimeLimitMS int) error {
	ctx, cancel := context.WithTimeout(ctx, MigrationTimeout+StorageSyncTimeout)
	defer cancel()

	log.Printf("Starting live migration to %s...", destIP)

	client, err := qmp.NewClient(ctx, qmpSocket)
	if err != nil {
		return fmt.Errorf("connecting to source QMP: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("warning: closing QMP client: %v", err)
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

	// Step 4: Wait for the STOP event (downtime window begins).
	// ARCHITECTURE UPDATE (Phase 1/2): Migration Polling
	// We deliberately poll migration status sequentially in the same loop rather
	// than using a separate goroutine for `WaitForEvent` vs `query-migrate`.
	// This prevents concurrent state access issues and QMP socket data races,
	// while ensuring we don't hang forever if QEMU silently fails the migration
	// without emitting a STOP event.
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
			if err := json.Unmarshal(raw, &info); err == nil {
				log.Printf("Migration progress: %s (RAM: %d / %d bytes, Remaining: %d bytes)", info.Status, info.RAM.Transferred, info.RAM.Total, info.RAM.Remaining)
				if info.Status == "failed" {
					return fmt.Errorf("migration background process failed: %s", info.ErrorDesc)
				} else if info.Status == "cancelled" {
					return ErrMigrationCancelled
				} else if info.Status == "completed" {
					log.Println("Migration completed without explicit STOP event.")
					break stopLoop
				}
			}
			continue
		}
		return fmt.Errorf("unexpected error waiting for STOP event: %w", err)
	}

	log.Println("VM paused. Redirecting in-flight packets to destination...")

	tunnelCreated := false
	if err := SetupTunnel(ctx, destIP, vmIP, tunnelMode); err != nil {
		return fmt.Errorf("failed to create IP tunnel: %w", err)
	}
	tunnelCreated = true
	log.Println("IP tunnel established. Traffic redirected.")
	log.Println("Waiting for migration to complete...")

	migrationErr := waitForMigrationComplete(ctx, client)

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
			if job.Status == "concluded" || job.Status == "null" {
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
			return fmt.Errorf("migration timed out (status: %s)", info.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
