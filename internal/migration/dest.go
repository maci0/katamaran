package migration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/maci0/katamaran/internal/qmp"
)

// destDefaultQMPSocket is the well-known socket path that deploy/migrate.sh
// fills into the dest job when no explicit --qmp-dest is provided. It is
// intentionally overridable here so the pod-resolver can replace it with the
// real sandbox-derived path at runtime.
const destDefaultQMPSocket = "/run/vc/vm/katamaran-dest/qmp.sock"

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
//  2. Configures multifd capabilities (if enabled) and opens an incoming
//     migration listener via QMP migrate-incoming
//  3. Starts an NBD server for storage mirroring (unless shared-storage mode)
//  4. Plugs the network queue to catch in-flight packets (skipped if no qdisc installed)
//  5. Waits for the RESUME event (unconditional)
//  6. Flushes all buffered packets via release_indefinite (skipped if no qdisc installed)
//  7. Stops the NBD server (unless shared-storage mode)
//  8. Sends Gratuitous ARP via QEMU announce-self (correct guest MAC)
func RunDestination(ctx context.Context, cfg DestConfig) (retErr error) {
	if cfg.DestPodName != "" {
		ip, err := lookupPodIP(ctx, cfg.DestPodNamespace, cfg.DestPodName)
		if err != nil {
			return fmt.Errorf("lookup dest pod IP: %w", err)
		}
		res, err := resolveSandbox(sandboxRoot, procImpl, ip)
		if err != nil {
			return fmt.Errorf("resolve dest sandbox: %w", err)
		}
		// Override both an empty QMPSocket and the migrate.sh-generated default,
		// since the orchestrator can't know the real sandbox UUID up front and
		// passes the well-known placeholder path through to the dest job.
		if cfg.QMPSocket == "" || cfg.QMPSocket == destDefaultQMPSocket {
			cfg.QMPSocket = filepath.Join(sandboxRoot, res.Sandbox, "extra-monitor.sock")
		}
	}

	if cfg.MultifdChannels < 0 {
		return fmt.Errorf("multifd channels must be non-negative, got %d", cfg.MultifdChannels)
	}
	if cfg.TapIface != "" {
		if err := validateTapIface(cfg.TapIface); err != nil {
			return fmt.Errorf("validating tap interface: %w", err)
		}
	}
	if cfg.TapNetns != "" {
		if err := validateTapNetns(cfg.TapNetns); err != nil {
			return fmt.Errorf("validating tap netns: %w", err)
		}
	}
	if !cfg.SharedStorage {
		if err := validateDriveID(cfg.DriveID); err != nil {
			return fmt.Errorf("validating drive ID: %w", err)
		}
	}

	destStart := time.Now()
	defer func() {
		if retErr != nil {
			slog.Error("Destination setup failed", "error", retErr, "elapsed", time.Since(destStart).Round(time.Millisecond))
		}
	}()

	slog.Info("Setting up destination node",
		"qmp_socket", cfg.QMPSocket,
		"tap_iface", cfg.TapIface,
		"tap_netns", cfg.TapNetns,
		"shared_storage", cfg.SharedStorage,
		"multifd_channels", cfg.MultifdChannels,
		"drive_id", cfg.DriveID,
	)

	// Step 1: Install sch_plug qdisc in pass-through mode.
	qdiscInstalled := false
	tapIface := cfg.TapIface
	tapNetns := cfg.TapNetns
	if tapIface != "" {
		slog.Info("Preparing network queue", "tap_iface", tapIface)

		// Check interface exists (in netns if specified, else locally).
		ifaceErr := runCmdInNetns(ctx, tapNetns, "ip", "link", "show", tapIface)
		if ifaceErr != nil {
			slog.Warn("TAP interface not found, skipping network queue setup", "tap_iface", tapIface, "error", ifaceErr)
		} else {
			// Idempotency: clear any existing qdisc on this interface before adding.
			cctx, ccancel := cleanupCtx(ctx)
			if err := runCmdInNetns(cctx, tapNetns, "tc", "qdisc", "del", "dev", tapIface, "root"); err != nil {
				// Expected to fail if no qdisc exists (first run).
				slog.Debug("Pre-clearing qdisc (expected if none exists)", "tap_iface", tapIface, "error", err)
			}
			ccancel()

			if err := runCmdInNetns(ctx, tapNetns, "tc", "qdisc", "add", "dev", tapIface, "root", "plug", "limit", plugQdiscLimit); err != nil {
				return fmt.Errorf("failed to add plug qdisc on %s (is sch_plug available?): %w", tapIface, err)
			}
			if err := runCmdInNetns(ctx, tapNetns, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
				cctx, ccancel := cleanupCtx(ctx)
				cleanupErr := runCmdInNetns(cctx, tapNetns, "tc", "qdisc", "del", "dev", tapIface, "root")
				ccancel()
				return errors.Join(fmt.Errorf("failed to release plug qdisc on %s, removing it: %w", tapIface, err), cleanupErr)
			}
			qdiscInstalled = true
			slog.Info("Network queue installed (pass-through, not plugged yet)", "tap_iface", tapIface)
		}
	} else {
		slog.Info("No TAP interface specified, skipping network queue setup")
	}

	// Deferred cleanup: remove qdisc on any early return to prevent leaking it.
	// Disarmed on the success path by setting qdiscInstalled = false.
	defer func() {
		if qdiscInstalled {
			cctx, ccancel := cleanupCtx(ctx)
			defer ccancel()
			if err := runCmdInNetns(cctx, tapNetns, "tc", "qdisc", "del", "dev", tapIface, "root"); err != nil {
				slog.Warn("Failed to remove qdisc", "tap_iface", tapIface, "error", err)
			}
		}
	}()

	client, err := qmp.NewClient(ctx, cfg.QMPSocket)
	if err != nil {
		return fmt.Errorf("connecting to destination QMP: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			slog.Warn("Failed to close QMP client", "error", err)
		}
	}()

	// Step 2: Configure migration capabilities and open incoming listener.
	// Multifd must be enabled BEFORE migrate-incoming so the destination
	// opens the parallel channel listeners alongside the main listener.
	if cfg.MultifdChannels > 0 {
		if _, err = client.Execute(ctx, "migrate-set-capabilities", qmp.MigrateSetCapabilitiesArgs{
			Capabilities: []qmp.MigrationCapability{
				{Capability: "multifd", State: true},
			},
		}); err != nil {
			return fmt.Errorf("setting destination migration capabilities: %w", err)
		}
		if _, err = client.Execute(ctx, "migrate-set-parameters", qmp.MigrateSetParametersArgs{
			MultifdChannels: int64(cfg.MultifdChannels),
		}); err != nil {
			return fmt.Errorf("setting destination migration parameters: %w", err)
		}
		slog.Info("Multifd enabled on destination", "channels", cfg.MultifdChannels)
	}

	// Starting QEMU with -incoming is incompatible with Kata's sandbox lifecycle
	// (Kata kills the QEMU because kata-agent never connects via vsock in
	// incoming mode), so we use a QMP command on the already-running instance.
	incomingURI := fmt.Sprintf("tcp:[::]:%s", ramMigrationPort)
	slog.Info("Opening incoming migration listener", "uri", incomingURI)
	if _, err = client.Execute(ctx, "migrate-incoming", qmp.MigrateArgs{URI: incomingURI}); err != nil {
		return fmt.Errorf("configuring incoming migration listener: %w", err)
	}
	slog.Info("Incoming migration listener ready", "uri", incomingURI)

	nbdStarted := false
	if !cfg.SharedStorage {
		// Step 3: Start NBD server to receive storage mirroring from the source.
		slog.Info("Starting NBD server for storage migration", "drive_id", cfg.DriveID)
		// Idempotency: attempt to stop any existing NBD server first, ignore errors.
		if _, err := client.Execute(ctx, "nbd-server-stop", nil); err != nil {
			slog.Debug("Pre-clearing NBD server (expected if none exists)", "error", err)
		}

		if _, err = client.Execute(ctx, "nbd-server-start", qmp.NBDServerStartArgs{
			Addr: qmp.NBDServerAddr{
				Type: "inet",
				Data: qmp.NBDServerAddrData{
					Host: "::",
					Port: nbdPort,
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
				cctx, ccancel := cleanupCtx(ctx)
				defer ccancel()
				if _, stopErr := client.Execute(cctx, "nbd-server-stop", nil); stopErr != nil {
					slog.Warn("Deferred NBD server stop failed", "error", stopErr)
				}
			}
		}()

		if _, err = client.Execute(ctx, "nbd-server-add", qmp.NBDServerAddArgs{
			Device:   cfg.DriveID,
			Writable: true,
		}); err != nil {
			return fmt.Errorf("adding NBD export for drive %q: %w", cfg.DriveID, err)
		}
		slog.Info("NBD server listening", "addr", "[::]", "port", nbdPort)
	} else {
		slog.Info("Shared storage mode: skipping NBD server setup")
	}

	// Step 4: Plug the network queue to begin catching in-flight packets.
	//
	// In a production orchestrator, this would be triggered via an RPC callback
	// when the source emits its STOP event. In this standalone tool, we plug
	// proactively before waiting for RESUME.
	if qdiscInstalled {
		if err := runCmdInNetns(ctx, tapNetns, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "block"); err != nil {
			return fmt.Errorf("failed to plug network queue on %s: %w", tapIface, err)
		}
		slog.Info("Network queue plugged. Buffering in-flight packets", "tap_iface", tapIface)
	}

	// Step 5: Wait for the destination VM to resume.
	slog.Info("Waiting for QEMU RESUME event")
	if err = client.WaitForEvent(ctx, "RESUME", eventWaitTimeout); err != nil {
		return fmt.Errorf("waiting for RESUME event: %w", err)
	}
	if qdiscInstalled {
		slog.Info("VM resumed. Flushing buffered packets")
	} else {
		slog.Info("VM resumed")
	}

	// Step 6: Unplug the queue — flush all buffered packets into the now-running VM.
	// Only disarm the deferred cleanup if the unplug succeeds. If it fails,
	// the qdisc is still in "plugged" state and the deferred cleanup must
	// remove it so the VM's network isn't left permanently blocked.
	if qdiscInstalled {
		if err := runCmdInNetns(ctx, tapNetns, "tc", "qdisc", "change", "dev", tapIface, "root", "plug", "release_indefinite"); err != nil {
			return fmt.Errorf("failed to unplug network queue on %s: %w", tapIface, err)
		}
		slog.Info("Queue unplugged. Buffered packets delivered. Zero drops achieved")
		// Disarm qdisc deferred cleanup — we've successfully flushed and the
		// qdisc will be naturally removed when the tap interface is torn down.
		qdiscInstalled = false
	}

	if !cfg.SharedStorage {
		// Step 7: Stop the NBD server (storage migration is complete).
		// Disarm the deferred cleanup since we're handling it explicitly.
		nbdStarted = false
		cctx, ccancel := cleanupCtx(ctx)
		defer ccancel()
		if _, err := client.Execute(cctx, "nbd-server-stop", nil); err != nil {
			slog.Warn("Failed to stop NBD server", "error", err)
		} else {
			slog.Info("NBD server stopped")
		}
	}

	// Step 8: Broadcast Gratuitous ARP via QEMU's announce-self command.
	// Unlike host-side arping (which sends the host tap MAC), announce-self
	// emits GARP/RARP from the guest's actual MAC address on all NICs,
	// ensuring switches learn the correct port-to-MAC binding.
	// With OVN-based CNIs (OVN-Kubernetes, Kube-OVN), OVN handles port-chassis rebinding automatically.
	// For other CNIs (Cilium, Calico, Flannel), GARP accelerates convergence.
	slog.Info("Broadcasting Gratuitous ARP via QEMU announce-self")
	garpCtx, garpCancel := cleanupCtx(ctx)
	defer garpCancel()
	if _, err := client.Execute(garpCtx, "announce-self", qmp.AnnounceSelfArgs{
		Initial: garpInitialMS,
		Max:     garpMaxMS,
		Rounds:  garpRounds,
		Step:    garpStepMS,
	}); err != nil {
		return fmt.Errorf("GARP announce-self failed: %w", err)
	}
	slog.Info("GARP announce-self scheduled", "rounds", garpRounds)

	slog.Info("Destination setup complete", "elapsed", time.Since(destStart).Round(time.Millisecond))
	return nil
}
