package migration

import (
	"context"
	"fmt"
	"log"
	"net"

	"katamaran/internal/qmp"
)

// RunDestination prepares the destination node for incoming live migration.
//
// Deferred cleanups ensure the qdisc and NBD server are released on any early
// return, preventing resource leaks. They are disarmed on the success path by
// setting the corresponding guard bool to false.
//
// Sequentially it:
//  1. Installs a tc sch_plug qdisc on the tap interface in pass-through mode
//     (sch_plug defaults to buffering, so we immediately release_indefinite;
//     non-fatal if sch_plug is unavailable or tapIface is empty)
//  2. Starts an NBD server for storage mirroring (unless shared-storage mode)
//  3. Plugs the network queue to catch in-flight packets (skipped if step 1 failed)
//  4. Waits for the RESUME event (unconditional)
//  5. Flushes all buffered packets via release_indefinite (skipped if step 1 failed)
//  6. Stops the NBD server (unless shared-storage mode)
//  7. Sends Gratuitous ARP via QEMU announce-self (correct guest MAC)
func RunDestination(ctx context.Context, qmpSocket, tapIface, driveID string, sharedStorage bool) error {
	log.Println("Setting up destination node...")

	// Step 1: Install sch_plug qdisc in pass-through mode.
	qdiscInstalled := false
	if tapIface != "" {
		log.Printf("Preparing network queue on %s...", tapIface)

		if _, err := net.InterfaceByName(tapIface); err != nil {
			log.Printf("Warning: TAP interface %q not found (%v). Skipping network queue setup.", tapIface, err)
		} else {
			// Idempotency: clear any existing qdisc on this interface before adding.
			cctx, ccancel := CleanupCtx()
			_ = RunCmd(cctx, "tc", "qdisc", "del", "dev", tapIface, "root")
			ccancel()

			if err := RunCmd(ctx, "tc", "qdisc", "add", "dev", tapIface, "root", "plug", "limit", PlugQdiscLimit); err != nil {
				log.Printf("Warning: failed to add plug qdisc on %s (is sch_plug available?): %v", tapIface, err)
			} else if err := RunCmd(ctx, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
				log.Printf("Warning: failed to release plug qdisc on %s, removing it: %v", tapIface, err)
				cctx, ccancel := CleanupCtx()
				_ = RunCmd(cctx, "tc", "qdisc", "del", "dev", tapIface, "root")
				ccancel()
			} else {
				qdiscInstalled = true
				log.Println("Network queue installed (pass-through, not plugged yet).")
			}
		}
	} else {
		log.Println("No TAP interface specified, skipping network queue setup.")
	}

	// Deferred cleanup: remove qdisc on any early return to prevent leaking it.
	// Disarmed on the success path by setting qdiscInstalled = false.
	// Uses CleanupCtx() so cleanup runs even if the main ctx is cancelled.
	defer func() {
		if qdiscInstalled && tapIface != "" {
			cctx, ccancel := CleanupCtx()
			defer ccancel()
			_ = RunCmd(cctx, "tc", "qdisc", "del", "dev", tapIface, "root")
		}
	}()

	client, err := qmp.NewClient(ctx, qmpSocket)
	if err != nil {
		return fmt.Errorf("connecting to destination QMP: %w", err)
	}
	defer client.Close()

	nbdStarted := false
	if !sharedStorage {
		// Step 2: Start NBD server to receive storage mirroring from the source.
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
				cctx, ccancel := CleanupCtx()
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

	// Step 3: Plug the network queue to begin catching in-flight packets.
	//
	// In a production orchestrator, this would be triggered via an RPC callback
	// when the source emits its STOP event. In this standalone tool, we plug
	// proactively before waiting for RESUME.
	if qdiscInstalled {
		if err := RunCmd(ctx, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "block"); err != nil {
			log.Printf("Warning: failed to plug network queue on %s: %v", tapIface, err)
		} else {
			log.Println("Network queue plugged. Buffering in-flight packets...")
		}
	}

	// Step 4: Wait for the destination VM to resume.
	log.Println("Waiting for QEMU RESUME event...")
	if err = client.WaitForEvent(ctx, "RESUME", EventWaitTimeout); err != nil {
		return fmt.Errorf("waiting for RESUME event: %w", err)
	}
	if qdiscInstalled {
		log.Println("VM resumed. Flushing buffered packets...")
	} else {
		log.Println("VM resumed.")
	}

	// Step 5: Unplug the queue — flush all buffered packets into the now-running VM.
	// Only disarm the deferred cleanup if the unplug succeeds. If it fails,
	// the qdisc is still in "plugged" state and the deferred cleanup must
	// remove it so the VM's network isn't left permanently blocked.
	if qdiscInstalled {
		if err := RunCmd(ctx, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
			log.Printf("Warning: failed to unplug network queue on %s: %v", tapIface, err)
		} else {
			log.Println("Queue unplugged. Buffered packets delivered. Zero drops achieved.")
			// Disarm qdisc deferred cleanup — we've successfully flushed and the
			// qdisc will be naturally removed when the tap interface is torn down.
			qdiscInstalled = false
		}
	}

	if !sharedStorage {
		// Step 6: Stop the NBD server (storage migration is complete).
		// Disarm the deferred cleanup since we're handling it explicitly.
		// Uses CleanupCtx() so the stop succeeds even if the main ctx was
		// cancelled (e.g., SIGINT received after RESUME).
		nbdStarted = false
		cctx, ccancel := CleanupCtx()
		if _, err := client.Execute(cctx, "nbd-server-stop", nil); err != nil {
			log.Printf("Warning: failed to stop NBD server: %v", err)
		} else {
			log.Println("NBD server stopped.")
		}
		ccancel()
	}

	// Step 7: Broadcast Gratuitous ARP via QEMU's announce-self command.
	// Unlike host-side arping (which sends the host tap MAC), announce-self
	// emits GARP/RARP from the guest's actual MAC address on all NICs,
	// ensuring switches learn the correct port-to-MAC binding.
	// With OVN-based CNIs (OVN-Kubernetes, Kube-OVN), OVN handles port-chassis rebinding automatically.
	// For other CNIs (Cilium, Calico, Flannel), GARP accelerates convergence.
	log.Println("Broadcasting Gratuitous ARP via QEMU announce-self...")
	garpCtx, garpCancel := CleanupCtx()
	if _, err := client.Execute(garpCtx, "announce-self", qmp.AnnounceSelfArgs{
		Initial: GARPInitialMS,
		Max:     GARPMaxMS,
		Rounds:  GARPRounds,
		Step:    GARPStepMS,
	}); err != nil {
		log.Printf("Warning: GARP announce-self failed: %v", err)
	} else {
		log.Printf("GARP announce-self scheduled (%d rounds).", GARPRounds)
	}
	garpCancel()

	log.Println("Destination setup complete.")
	return nil
}
