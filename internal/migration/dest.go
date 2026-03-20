package migration

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/maci0/katamaran/internal/qmp"
)

// RunDestination prepares the destination node for incoming live migration.
//
// Deferred cleanups ensure the qdisc and NBD server are released on any early
// return, preventing resource leaks. They are disarmed on the success path by
// setting the corresponding guard bool to false.
//
// If tapNetns is non-empty, all tc commands are executed inside the given
// network namespace via nsenter (e.g. "/proc/PID/ns/net"). This supports
// scenarios where the tap interface lives in a different namespace than
// the katamaran process (e.g. helper pod approach for manual destination QEMU).
//
// Sequentially it:
//  1. Installs a tc sch_plug qdisc on the tap interface in pass-through mode
//     (sch_plug defaults to buffering, so we immediately release_indefinite;
//     skipped if tapIface is empty or the interface does not exist)
//  2. Opens an incoming migration listener via QMP migrate-incoming
//  3. Starts an NBD server for storage mirroring (unless shared-storage mode)
//  4. Plugs the network queue to catch in-flight packets (skipped if no qdisc installed)
//  5. Waits for the RESUME event (unconditional)
//  6. Flushes all buffered packets via release_indefinite (skipped if no qdisc installed)
//  7. Stops the NBD server (unless shared-storage mode)
//  8. Sends Gratuitous ARP via QEMU announce-self (correct guest MAC)
func RunDestination(ctx context.Context, qmpSocket, tapIface, tapNetns, driveID string, sharedStorage bool, multifdChannels int) error {
	log.Println("Setting up destination node...")

	// Step 1: Install sch_plug qdisc in pass-through mode.
	qdiscInstalled := false
	if tapIface != "" {
		log.Printf("Preparing network queue on %s...", tapIface)

		// Check interface exists (in netns if specified, else locally).
		ifaceErr := RunCmdInNetns(ctx, tapNetns, "ip", "link", "show", tapIface)
		if ifaceErr != nil {
			log.Printf("Warning: TAP interface %q not found (%v). Skipping network queue setup.", tapIface, ifaceErr)
		} else {
			// Idempotency: clear any existing qdisc on this interface before adding.
			cctx, ccancel := CleanupCtx(ctx)
			if err := RunCmdInNetns(cctx, tapNetns, "tc", "qdisc", "del", "dev", tapIface, "root"); err != nil {
				// Expected to fail if no qdisc exists (first run).
				log.Printf("Pre-clearing qdisc on %s: %v (expected if none exists)", tapIface, err)
			}
			ccancel()

			if err := RunCmdInNetns(ctx, tapNetns, "tc", "qdisc", "add", "dev", tapIface, "root", "plug", "limit", PlugQdiscLimit); err != nil {
				return fmt.Errorf("failed to add plug qdisc on %s (is sch_plug available?): %w", tapIface, err)
			}
			if err := RunCmdInNetns(ctx, tapNetns, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
				cctx, ccancel := CleanupCtx(ctx)
				cleanupErr := RunCmdInNetns(cctx, tapNetns, "tc", "qdisc", "del", "dev", tapIface, "root")
				ccancel()
				return errors.Join(fmt.Errorf("failed to release plug qdisc on %s, removing it: %w", tapIface, err), cleanupErr)
			}
			qdiscInstalled = true
			log.Println("Network queue installed (pass-through, not plugged yet).")
		}
	} else {
		log.Println("No TAP interface specified, skipping network queue setup.")
	}

	// Deferred cleanup: remove qdisc on any early return to prevent leaking it.
	// Disarmed on the success path by setting qdiscInstalled = false.
	defer func() {
		if qdiscInstalled {
			cctx, ccancel := CleanupCtx(ctx)
			defer ccancel()
			if err := RunCmdInNetns(cctx, tapNetns, "tc", "qdisc", "del", "dev", tapIface, "root"); err != nil {
				log.Printf("Warning: failed to remove qdisc: %v", err)
			}
		}
	}()

	client, err := qmp.NewClient(ctx, qmpSocket)
	if err != nil {
		return fmt.Errorf("connecting to destination QMP: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("Warning: closing QMP client: %v", err)
		}
	}()

	// Step 2: Configure migration capabilities and open incoming listener.
	// Multifd must be enabled BEFORE migrate-incoming so the destination
	// opens the parallel channel listeners alongside the main listener.
	if multifdChannels > 0 {
		if _, err = client.Execute(ctx, "migrate-set-capabilities", qmp.MigrateSetCapabilitiesArgs{
			Capabilities: []qmp.MigrationCapability{
				{Capability: "multifd", State: true},
			},
		}); err != nil {
			return fmt.Errorf("setting destination migration capabilities: %w", err)
		}
		if _, err = client.Execute(ctx, "migrate-set-parameters", qmp.MigrateSetParametersArgs{
			MultiFDChannels: int64(multifdChannels),
		}); err != nil {
			return fmt.Errorf("setting destination migration parameters: %w", err)
		}
		log.Printf("Multifd enabled on destination: %d channels.", multifdChannels)
	}

	// Starting QEMU with -incoming is incompatible with Kata's sandbox lifecycle
	// (Kata kills the QEMU because kata-agent never connects via vsock in
	// incoming mode), so we use a QMP command on the already-running instance.
	incomingURI := fmt.Sprintf("tcp:0.0.0.0:%s", RAMMigrationPort)
	log.Printf("Opening incoming migration listener on %s...", incomingURI)
	if _, err = client.Execute(ctx, "migrate-incoming", qmp.MigrateArgs{URI: incomingURI}); err != nil {
		return fmt.Errorf("configuring incoming migration listener: %w", err)
	}

	nbdStarted := false
	if !sharedStorage {
		// Step 3: Start NBD server to receive storage mirroring from the source.
		log.Println("Starting NBD server for storage migration...")
		// Idempotency: attempt to stop any existing NBD server first, ignore errors.
		_, _ = client.Execute(ctx, "nbd-server-stop", nil)

		if _, err = client.Execute(ctx, "nbd-server-start", qmp.NBDServerStartArgs{
			Addr: qmp.NBDServerAddr{
				Type: "inet",
				Data: qmp.NBDServerAddrData{
					Host: "::",
					Port: NBDPort,
				},
			},
		}); err != nil {
			return fmt.Errorf("starting NBD server: %w", err)
		}
		nbdStarted = true

		// Deferred cleanup: stop NBD server on any early return to prevent
		// leaking it. Disarmed on the success path by setting nbdStarted = false.
		defer func() {
			if nbdStarted {
				cctx, ccancel := CleanupCtx(ctx)
				defer ccancel()
				if _, stopErr := client.Execute(cctx, "nbd-server-stop", nil); stopErr != nil {
					log.Printf("Warning: deferred NBD server stop failed: %v", stopErr)
				}
			}
		}()

		if _, err = client.Execute(ctx, "nbd-server-add", qmp.NBDServerAddArgs{
			Device:   driveID,
			Writable: true,
		}); err != nil {
			return fmt.Errorf("adding NBD export for drive %q: %w", driveID, err)
		}
		log.Printf("NBD server listening on [::]:%s", NBDPort)
	} else {
		log.Println("Shared storage mode: skipping NBD server setup.")
	}

	// Step 4: Plug the network queue to begin catching in-flight packets.
	//
	// In a production orchestrator, this would be triggered via an RPC callback
	// when the source emits its STOP event. In this standalone tool, we plug
	// proactively before waiting for RESUME.
	if qdiscInstalled {
		if err := RunCmdInNetns(ctx, tapNetns, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "block"); err != nil {
			return fmt.Errorf("failed to plug network queue on %s: %w", tapIface, err)
		}
		log.Println("Network queue plugged. Buffering in-flight packets...")
	}

	// Step 5: Wait for the destination VM to resume.
	log.Println("Waiting for QEMU RESUME event...")
	if err = client.WaitForEvent(ctx, "RESUME", EventWaitTimeout); err != nil {
		return fmt.Errorf("waiting for RESUME event: %w", err)
	}
	if qdiscInstalled {
		log.Println("VM resumed. Flushing buffered packets...")
	} else {
		log.Println("VM resumed.")
	}

	// Step 6: Unplug the queue — flush all buffered packets into the now-running VM.
	// Only disarm the deferred cleanup if the unplug succeeds. If it fails,
	// the qdisc is still in "plugged" state and the deferred cleanup must
	// remove it so the VM's network isn't left permanently blocked.
	if qdiscInstalled {
		if err := RunCmdInNetns(ctx, tapNetns, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
			return fmt.Errorf("failed to unplug network queue on %s: %w", tapIface, err)
		}
		log.Println("Queue unplugged. Buffered packets delivered. Zero drops achieved.")
		// Disarm qdisc deferred cleanup — we've successfully flushed and the
		// qdisc will be naturally removed when the tap interface is torn down.
		qdiscInstalled = false
	}

	if !sharedStorage {
		// Step 7: Stop the NBD server (storage migration is complete).
		// Disarm the deferred cleanup since we're handling it explicitly.
		nbdStarted = false
		cctx, ccancel := CleanupCtx(ctx)
		if _, err := client.Execute(cctx, "nbd-server-stop", nil); err != nil {
			log.Printf("Warning: failed to stop NBD server: %v", err)
		} else {
			log.Println("NBD server stopped.")
		}
		ccancel()
	}

	// Step 8: Broadcast Gratuitous ARP via QEMU's announce-self command.
	// Unlike host-side arping (which sends the host tap MAC), announce-self
	// emits GARP/RARP from the guest's actual MAC address on all NICs,
	// ensuring switches learn the correct port-to-MAC binding.
	// With OVN-based CNIs (OVN-Kubernetes, Kube-OVN), OVN handles port-chassis rebinding automatically.
	// For other CNIs (Cilium, Calico, Flannel), GARP accelerates convergence.
	log.Println("Broadcasting Gratuitous ARP via QEMU announce-self...")
	garpCtx, garpCancel := CleanupCtx(ctx)
	defer garpCancel()
	if _, err := client.Execute(garpCtx, "announce-self", qmp.AnnounceSelfArgs{
		Initial: GARPInitialMS,
		Max:     GARPMaxMS,
		Rounds:  GARPRounds,
		Step:    GARPStepMS,
	}); err != nil {
		return fmt.Errorf("GARP announce-self failed: %w", err)
	}
	log.Printf("GARP announce-self scheduled (%d rounds).", GARPRounds)

	log.Println("Destination setup complete.")
	return nil
}
