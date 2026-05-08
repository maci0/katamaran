package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	// If a captured source cmdline is supplied, spawn the destination QEMU
	// ourselves with -incoming defer + -daemonize before connecting to QMP.
	// This bypasses Kata's sandbox lifecycle (which kills VMs that don't
	// connect via vsock within dial_timeout — incompatible with -incoming).
	// spawnReplayedQEMU mutates cfg.QMPSocket to point at the spawned QEMU's
	// monitor; pod-resolver overrides above are intentionally superseded.
	//
	// Two delivery modes for the cmdline:
	//
	//   --replay-cmdline-from-pod <ns>/<pod>: dest binary fetches the
	//   source pod's log via the in-cluster apiserver, scans for the
	//   KATAMARAN_CMDLINE_B64 marker, decodes it, writes to a local
	//   tmp file, and falls through to the file-based replay path
	//   below. No stager pod, no SPDY exec, no hostPath shuffling.
	//
	//   --replay-cmdline <path>: legacy file-based mode, kept for
	//   manual testing where the file is staged out-of-band (e.g.
	//   deploy/migrate.sh's kubectl-cp shuffle).
	if cfg.ReplayCmdlineFromPod != "" {
		path, err := fetchCmdlineFromPodLog(ctx, cfg.ReplayCmdlineFromPod)
		if err != nil {
			return fmt.Errorf("replay-cmdline-from-pod: %w", err)
		}
		cfg.ReplayCmdlineFile = path
	}
	if cfg.ReplayCmdlineFile != "" {
		if err := spawnReplayedQEMU(ctx, &cfg); err != nil {
			return fmt.Errorf("replay source QEMU cmdline: %w", err)
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
		if err := validateDriveIDs(cfg.DriveIDs); err != nil {
			return fmt.Errorf("validating drive IDs: %w", err)
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
		"drive_ids", cfg.DriveIDs,
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
	// Capabilities must match the source's; otherwise the migration handshake
	// fails with "Failed to peek at channel" or similar magic-mismatch errors.
	caps := []qmp.MigrationCapability{
		{Capability: "auto-converge", State: true},
	}
	if cfg.MultifdChannels > 0 {
		caps = append(caps, qmp.MigrationCapability{Capability: "multifd", State: true})
	}
	if _, err = client.Execute(ctx, "migrate-set-capabilities", qmp.MigrateSetCapabilitiesArgs{
		Capabilities: caps,
	}); err != nil {
		return fmt.Errorf("setting destination migration capabilities: %w", err)
	}
	if cfg.MultifdChannels > 0 {
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
		slog.Info("Starting NBD server for storage migration", "drives", len(cfg.DriveIDs))
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

		defer func() {
			if nbdStarted {
				cctx, ccancel := cleanupCtx(ctx)
				defer ccancel()
				if _, stopErr := client.Execute(cctx, "nbd-server-stop", nil); stopErr != nil {
					slog.Warn("Deferred NBD server stop failed", "error", stopErr)
				}
			}
		}()

		for _, driveID := range cfg.DriveIDs {
			if _, err = client.Execute(ctx, "nbd-server-add", qmp.NBDServerAddArgs{
				Device:   driveID,
				Writable: true,
			}); err != nil {
				return fmt.Errorf("adding NBD export for drive %q: %w", driveID, err)
			}
			slog.Info("NBD export added", "drive_id", driveID)
		}
		slog.Info("NBD server listening", "addr", "[::]", "port", nbdPort, "exports", len(cfg.DriveIDs))
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

	// Best-effort: write migration-meta.json so the factory server can
	// adopt this VM. Failures are logged but never fail the migration.
	writeMigrationMeta(ctx, cfg, client)

	return nil
}

// writeMigrationMeta writes a migration-meta.json file next to the QMP socket.
// The factory watcher picks this up and offers the VM to Kata shims via
// GetBaseVM. All errors are logged as warnings and never propagated.
func writeMigrationMeta(ctx context.Context, cfg DestConfig, client *qmp.Client) {
	type migrationMeta struct {
		ID              string          `json:"id"`
		QEMUPid         int             `json:"qemu_pid"`
		QMPSocket       string          `json:"qmp_socket"`
		VsockCID        uint32          `json:"vsock_cid"`
		UUID            string          `json:"uuid"`
		VirtiofsdPid    int             `json:"virtiofsd_pid"`
		HypervisorState json.RawMessage `json:"hypervisor_state,omitempty"`
		CPU             uint32          `json:"cpu"`
		Memory          uint32          `json:"memory"`
		VMConfig        json.RawMessage `json:"vm_config,omitempty"`
		AgentConfig     json.RawMessage `json:"agent_config,omitempty"`
	}

	sandboxID := filepath.Base(filepath.Dir(cfg.QMPSocket))
	meta := migrationMeta{
		ID:        sandboxID,
		QMPSocket: cfg.QMPSocket,
	}

	// Read QEMU PID from the pid file next to the QMP socket.
	pidPath := filepath.Join(filepath.Dir(cfg.QMPSocket), "pid")
	if pidBytes, err := os.ReadFile(pidPath); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes))); err == nil {
			meta.QEMUPid = pid
		}
	}

	// Query VM status via QMP (informational).
	if raw, err := client.Execute(ctx, "query-status", nil); err == nil {
		slog.Info("Dest VM status after migration", "status", string(raw))
	}

	// Try to load VMConfig from any sandbox persist.json on this node.
	// We scan all sandboxes, not just ours — VMConfig is the same across
	// all Kata pods on the same node (same Kata version + config).
	persistBytes, persistPath := findAnyPersistJSON()
	if persistBytes != nil {
		var persist struct {
			HypervisorState json.RawMessage `json:"HypervisorState"`
			Config          struct {
				HypervisorType   string          `json:"HypervisorType"`
				HypervisorConfig json.RawMessage `json:"HypervisorConfig"`
				KataAgentConfig  json.RawMessage `json:"KataAgentConfig"`
			} `json:"Config"`
		}
		if json.Unmarshal(persistBytes, &persist) == nil {
			if len(persist.HypervisorState) > 0 {
				meta.HypervisorState = persist.HypervisorState
			}
			if len(persist.Config.HypervisorConfig) > 0 {
				vmCfg, _ := json.Marshal(map[string]any{
					"HypervisorType":   persist.Config.HypervisorType,
					"HypervisorConfig": json.RawMessage(persist.Config.HypervisorConfig),
					"AgentConfig":      json.RawMessage(persist.Config.KataAgentConfig),
				})
				meta.VMConfig = vmCfg
				meta.AgentConfig = persist.Config.KataAgentConfig
			}
			slog.Info("Loaded state from persist.json", "path", persistPath)
		}
	} else {
		// No persist.json locally — try to fetch VMConfig from the source pod's log.
		srcRef := cfg.ReplayCmdlineFromPod
		if srcRef == "" {
			srcRef = cfg.SourcePodRef
		}
		if srcRef != "" {
			vmCfg, agentCfg := fetchVMConfigFromPodLog(ctx, srcRef)
			if len(vmCfg) > 0 {
				meta.VMConfig = vmCfg
				meta.AgentConfig = agentCfg
				slog.Info("Loaded VMConfig from source pod log", "ref", srcRef)
			}
		} else {
			slog.Info("No persist.json and no source pod ref for VMConfig")
		}
	}

	metaPath := filepath.Join(filepath.Dir(cfg.QMPSocket), "migration-meta.json")
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		slog.Warn("Failed to marshal migration metadata", "error", err)
		return
	}
	if err := os.WriteFile(metaPath, metaJSON, 0o644); err != nil {
		slog.Warn("Failed to write migration metadata", "path", metaPath, "error", err)
	} else {
		slog.Info("Migration metadata written for factory adoption", "path", metaPath)
	}
}

// findAnyPersistJSON scans /run/vc/sbs/ for any sandbox persist.json.
func findAnyPersistJSON() ([]byte, string) {
	entries, err := os.ReadDir("/run/vc/sbs")
	if err != nil {
		return nil, ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join("/run/vc/sbs", e.Name(), "persist.json")
		data, err := os.ReadFile(p)
		if err == nil {
			return data, p
		}
	}
	return nil, ""
}
