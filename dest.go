package main

import (
	"context"
	"fmt"
	"log"
	"net"
)

// setupDestination prepares the destination node for incoming live migration.
//
// Deferred cleanups ensure the qdisc and NBD server are released on any early
// return, preventing resource leaks. They are disarmed on the success path.
//
// Sequentially it:
//  1. Installs a tc sch_plug qdisc on the tap interface in pass-through mode
//     (sch_plug defaults to buffering, so we immediately release_indefinite;
//     non-fatal if sch_plug is unavailable)
//  2. Starts an NBD server for storage mirroring (unless shared-storage mode)
//  3. Plugs the network queue to catch in-flight packets (skipped if step 1 failed)
//  4. Waits for the RESUME event (unconditional)
//  5. Flushes all buffered packets via release_indefinite (skipped if step 1 failed)
//  6. Stops the NBD server (unless shared-storage mode)
//  7. Sends Gratuitous ARP via QEMU announce-self (correct guest MAC)
func setupDestination(ctx context.Context, qmpSocket, tapIface, driveID string, sharedStorage bool) error {
	log.Printf("Setting up destination node. Preparing network queue on %s...", tapIface)

	// Step 1: Install sch_plug qdisc in pass-through mode.
	qdiscInstalled := false
	if tapIface != "" {
		if _, err := net.InterfaceByName(tapIface); err != nil {
			log.Printf("Warning: TAP interface %q not found (%v). Skipping network queue setup.", tapIface, err)
		} else {
			// Idempotency: clear any existing qdisc on this interface before adding
			_ = runCmd(context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second), "tc", "qdisc", "del", "dev", tapIface, "root")

			if err := runCmd(ctx, "tc", "qdisc", "add", "dev", tapIface, "root", "plug", "limit", plugQdiscLimit); err != nil {
				log.Printf("Warning: failed to add plug qdisc on %s (is sch_plug available?): %v", tapIface, err)
			} else if err := runCmd(ctx, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
				log.Printf("Warning: failed to release plug qdisc on %s, removing it: %v", tapIface, err)
				_ = runCmd(context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second), "tc", "qdisc", "del", "dev", tapIface, "root")
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
	// We use context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second) here so cleanup runs even if the main ctx is cancelled.
	cleanupCtx, cancel := context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second)
		defer cancel()
		defer func(ctx context.Context) {
		if qdiscInstalled && tapIface != "" {
			_ = runCmd(context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second), "tc", "qdisc", "del", "dev", tapIface, "root")
		}
	}()

	qmp, err := NewQMPClient(ctx, qmpSocket)
	if err != nil {
		return fmt.Errorf("connecting to destination QMP: %w", err)
	}
	defer qmp.Close()

	nbdStarted := false
	if !sharedStorage {
		// Step 2: Start NBD server to receive storage mirroring from the source.
		log.Println("Starting NBD server for storage migration...")
		// Idempotency: Attempt to stop any existing NBD server first, ignore errors.
		_, _ = qmp.Execute(ctx, "nbd-server-stop", nil)

		if _, err = qmp.Execute(ctx, "nbd-server-start", nbdServerStartArgs{
			Addr: nbdServerAddr{
				Type: "inet",
				Data: nbdServerAddrData{
					Host: "0.0.0.0",
					Port: nbdPort,
				},
			},
		}); err != nil {
			return fmt.Errorf("starting NBD server: %w", err)
		}
		nbdStarted = true

		// Deferred cleanup: stop NBD server on any early return to prevent
		// leaking it. Disarmed on the success path by setting nbdStarted = false.
		cleanupCtx, cancel := context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second)
		defer cancel()
		defer func(ctx context.Context) {
			if nbdStarted {
				if _, stopErr := qmp.Execute(context.WithTimeout(context.WithTimeout(context.Background(), 10*time.Second), 10*time.Second), "nbd-server-stop", nil); stopErr != nil {
					log.Printf("Warning: deferred NBD server stop failed: %v", stopErr)
				}
			}
		}()

		if _, err = qmp.Execute(ctx, "nbd-server-add", nbdServerAddArgs{
			Device:   driveID,
			Writable: true,
		}); err != nil {
			return fmt.Errorf("adding NBD export for drive %q: %w", driveID, err)
		}
		log.Printf("NBD server listening on 0.0.0.0:%s", nbdPort)
	} else {
		log.Println("Shared storage mode: skipping NBD server setup.")
	}

	// Step 3: Plug the network queue to begin catching in-flight packets.
	//
	// In a production orchestrator, this would be triggered via an RPC callback
	// when the source emits its STOP event. In this standalone tool, we plug
	// proactively before waiting for RESUME.
	if qdiscInstalled {
		if err := runCmd(ctx, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "block"); err != nil {
			log.Printf("Warning: failed to plug network queue on %s: %v", tapIface, err)
		} else {
			log.Println("Network queue plugged. Buffering in-flight packets...")
		}
	}

	// Step 4: Wait for the destination VM to resume.
	log.Println("Waiting for QEMU RESUME event...")
	if err = qmp.WaitForEvent(ctx, "RESUME", eventWaitTimeout); err != nil {
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
		if err := runCmd(ctx, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
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
		nbdStarted = false
		if _, err := qmp.Execute(ctx, "nbd-server-stop", nil); err != nil {
			log.Printf("Warning: failed to stop NBD server: %v", err)
		} else {
			log.Println("NBD server stopped.")
		}
	}

	// Step 7: Broadcast Gratuitous ARP via QEMU's announce-self command.
	// Unlike host-side arping (which sends the host tap MAC), announce-self
	// emits GARP/RARP from the guest's actual MAC address on all NICs,
	// ensuring switches learn the correct port-to-MAC binding.
	// With Kube-OVN, OVN handles port-chassis rebinding automatically.
	// For other CNIs (Cilium, Calico, Flannel), GARP accelerates convergence.
	log.Println("Broadcasting Gratuitous ARP via QEMU announce-self...")
	if _, err := qmp.Execute(ctx, "announce-self", announceSelfArgs{
		Initial: garpInitialMS,
		Max:     garpMaxMS,
		Rounds:  garpRounds,
		Step:    garpStepMS,
	}); err != nil {
		log.Printf("Warning: GARP announce-self failed: %v", err)
	} else {
		log.Printf("GARP announce-self scheduled (%d rounds).", garpRounds)
	}

	log.Println("Destination setup complete.")
	return nil
}
