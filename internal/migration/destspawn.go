package migration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Defaults for cmdline replay. Var-not-const so tests can swap them.
var (
	// destReplayDefaultSandbox is the sandbox identifier used for the synthetic
	// dest VM directory under /run/vc/vm/<id>/ when DestConfig.SandboxID is
	// empty. The directory must already match the QMPSocket parent so that
	// the existing dest QMP-connect path picks up the spawned QEMU's monitor.
	destReplayDefaultSandbox = "katamaran-dest"

	// kataSharedSandboxRoot is where Kata stages per-sandbox virtiofs shared
	// directories. virtiofsd is started with --shared-dir under this root.
	// Test seam: tests replace it with a writable temp dir to avoid needing
	// root for the production /run path.
	kataSharedSandboxRoot = "/run/kata-containers/shared/sandboxes"

	// destReplayDefaultQEMU is the fallback QEMU binary path. Kata's bundled
	// QEMU lives here. Used only when the captured cmdline's argv[0] is not a
	// usable absolute path (defensive — argv[0] from /proc/<pid>/cmdline is
	// effectively always absolute for QEMU).
	destReplayDefaultQEMU = "/opt/kata/bin/qemu-system-x86_64"

	// destReplayVirtiofsd is the virtiofsd binary path. Mirrors e2e.sh:518.
	destReplayVirtiofsd = "/opt/kata/libexec/virtiofsd"

	// destReplaySocketWaitTotal bounds how long we wait for the QEMU QMP
	// socket to appear after spawning QEMU. e2e.sh:564 polls 15 × 1s.
	destReplaySocketWaitTotal = 15 * time.Second

	// destReplaySocketPollInterval is the polling cadence for the QMP socket.
	destReplaySocketPollInterval = 1 * time.Second

	// destReplayVirtiofsdSettleDelay matches e2e.sh:519 (sleep 2 after spawn).
	// virtiofsd needs a beat to bind its UNIX socket before QEMU connects.
	destReplayVirtiofsdSettleDelay = 2 * time.Second

	// destReadyTimeout caps how long the source waits for the destination's
	// RAM-migration listener to come up. The orchestrator must (1) ship the
	// captured cmdline to the dest node, (2) start the dest job, and
	// (3) the dest must finish copying nvdimm + spawning QEMU + opening the
	// migrate-incoming listener inside this window. Five minutes is generous
	// for nested-KVM CI; production environments converge in well under 30s.
	destReadyTimeout = 5 * time.Minute

	// destReadyPollInterval is the cadence for retrying the dest TCP probe.
	destReadyPollInterval = 2 * time.Second

	// destReadyDialTimeout bounds each individual TCP connect attempt.
	destReadyDialTimeout = 2 * time.Second
)

// memPathRegex matches `mem-path=<path>` clauses inside a memory-backend-file
// argument. Used to locate the nvdimm image path in the captured source cmdline.
// We reject any match where the path lives under /dev/shm because that's the
// guest RAM backing file (also a memory-backend-file), not the nvdimm image.
var memPathRegex = regexp.MustCompile(`mem-path=([^,]+)`)

// readonlyRegex strips `,readonly=on` and `,readonly=true` clauses from a
// device argument. The source uses readonly=on for the nvdimm image; the
// destination must accept writes during migration so QEMU can apply the
// transferred nvdimm pages. Equivalent to e2e.sh:539's two sed substitutions.
var readonlyRegex = regexp.MustCompile(`,readonly=(?:on|true)`)

// extractNvdimmPath finds the nvdimm image path in the captured cmdline.
// It returns the first mem-path= value that is not under /dev/shm. Returns
// the empty string if no candidate is found (in which case the caller skips
// the writable-copy step).
func extractNvdimmPath(args []string) string {
	for _, a := range args {
		for _, m := range memPathRegex.FindAllStringSubmatch(a, -1) {
			if len(m) < 2 {
				continue
			}
			p := m[1]
			if strings.HasPrefix(p, "/dev/shm") {
				continue
			}
			return p
		}
	}
	return ""
}

// transformCmdline rewrites a captured source QEMU cmdline so it can run on
// the destination node with -incoming defer.
//
// args is the full /proc/<src_qemu>/cmdline split on NUL. The first element
// is argv[0] (the QEMU binary itself) and is consumed as the spawn target.
//
// Substitutions performed (mirrors scripts/e2e.sh:496-559):
//   - srcSandboxDir → dstSandboxDir (typically /run/vc/vm/<src> → /run/vc/vm/<dst>)
//   - sandbox-<srcID> → sandbox-<dstID> (kata-agent / virtiofs share-dir paths)
//   - srcNvdimmPath → dstNvdimmPath (writable copy)
//   - strip ,readonly=on / ,readonly=true on the nvdimm backend
//   - drop existing -daemonize and -incoming <arg>
//   - append -incoming defer -daemonize
//
// The returned slice is the QEMU argv (without argv[0], which is returned
// separately so the caller can wrap it in nsenter or similar). Returns an
// error only if args is empty.
func transformCmdline(args []string, srcSandboxDir, dstSandboxDir, srcSandboxID, dstSandboxID, srcNvdimmPath, dstNvdimmPath string) (binary string, qemuArgs []string, err error) {
	if len(args) == 0 {
		return "", nil, errors.New("empty cmdline (no argv[0])")
	}
	binary = args[0]

	out := make([]string, 0, len(args)-1)
	skipNext := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if skipNext {
			skipNext = false
			continue
		}
		switch a {
		case "-daemonize":
			continue
		case "-incoming":
			// -incoming takes one positional argument (e.g. tcp:[::]:4444).
			skipNext = true
			continue
		case "-qmp":
			// -qmp may be specified multiple times. The kata-shim passes its
			// primary QMP via inherited fd=N (e.g. `unix:fd=3,server=on`),
			// which has no replay analog because the receiving fork-exec
			// chain doesn't carry that fd. The "extra" socket bound to
			// path=... is the one we want to keep.
			if i+1 < len(args) && strings.Contains(args[i+1], "fd=") {
				skipNext = true
				continue
			}
			out = append(out, a)
			continue
		case "-netdev":
			// -netdev tap,...,vhostfds=N,fds=M references inherited file
			// descriptors. Strip those keys so QEMU opens the tap itself.
			// Also append script=no,downscript=no so QEMU doesn't try to run
			// /opt/kata/etc/qemu-ifup which doesn't exist in our pod context.
			if i+1 < len(args) {
				next := stripFDKeys(args[i+1])
				if strings.HasPrefix(next, "tap,") || next == "tap" || strings.HasPrefix(next, "tap:") {
					if !strings.Contains(next, "script=") {
						next += ",script=no,downscript=no"
					}
				}
				out = append(out, a, next)
			}
			skipNext = true
			continue
		case "-device":
			// vhost-vsock-pci passes vhostfd=N from kata-shim; drop the key
			// so QEMU opens /dev/vhost-vsock itself.
			if i+1 < len(args) {
				next := stripFDKeys(args[i+1])
				out = append(out, a, next)
			}
			skipNext = true
			continue
		}

		if srcSandboxDir != "" && dstSandboxDir != "" {
			a = strings.ReplaceAll(a, srcSandboxDir, dstSandboxDir)
		}
		if srcSandboxID != "" && dstSandboxID != "" {
			a = strings.ReplaceAll(a, "sandbox-"+srcSandboxID, "sandbox-"+dstSandboxID)
		}
		if srcNvdimmPath != "" && dstNvdimmPath != "" {
			a = strings.ReplaceAll(a, srcNvdimmPath, dstNvdimmPath)
		}
		a = readonlyRegex.ReplaceAllString(a, "")
		out = append(out, a)
	}

	// Append -incoming defer; do NOT add -daemonize. We keep QEMU in the
	// foreground so its stderr stays connected to the dest pod's logger
	// (daemonized QEMU silently closes stderr after fork).
	out = append(out, "-incoming", "defer")
	return binary, out, nil
}

// stripFDKeys removes vhostfd, vhostfds, and fds key=value pairs from a
// comma-separated QEMU arg value. Used when respawning a captured cmdline:
// fd= references point at file descriptors inherited from the kata-shim
// parent and are invalid in a fresh exec.
func stripFDKeys(v string) string {
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		k := p
		if eq := strings.IndexByte(p, '='); eq != -1 {
			k = p[:eq]
		}
		switch k {
		case "fd", "fds", "vhostfd", "vhostfds":
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

// readCmdlineFile loads a captured QEMU cmdline file. The file is the result
// of `cat /proc/<pid>/cmdline | tr '\0' '\n'`: one argument per line, no
// trailing newline expected (but tolerated). Returns the argv slice including
// argv[0].
//
// We split on '\n' rather than '\x00' because the orchestrator transports the
// file in textual form (kubectl cp / ConfigMap) which mangles NUL bytes.
func readCmdlineFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cmdline file %s: %w", path, err)
	}
	return parseCmdlineBytes(data), nil
}

// parseCmdlineBytes accepts either NUL-delimited (`/proc/<pid>/cmdline` raw)
// or newline-delimited (post-`tr '\0' '\n'`) cmdline data. NUL takes
// precedence: if the buffer contains any NUL byte it is split on NUL, else
// on newline. Empty fields are dropped.
func parseCmdlineBytes(data []byte) []string {
	sep := byte('\n')
	for _, b := range data {
		if b == 0 {
			sep = 0
			break
		}
	}
	parts := strings.Split(string(data), string(sep))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// captureSourceCmdline reads /proc/<pid>/cmdline for the source QEMU and
// writes it (NUL→newline) to outPath. The output directory is created if it
// does not exist. Errors are returned to the caller; the source path emits
// a `KATAMARAN_CMDLINE_AT=<path>` marker only on success.
func captureSourceCmdline(qemuPID int, outPath string) error {
	src := fmt.Sprintf("/proc/%d/cmdline", qemuPID)
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	args := parseCmdlineBytes(raw)
	if len(args) == 0 {
		return fmt.Errorf("captured cmdline for pid %d is empty", qemuPID)
	}

	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create cmdline output dir %s: %w", dir, err)
		}
	}
	body := strings.Join(args, "\n") + "\n"
	if err := os.WriteFile(outPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	return nil
}

// findSrcSandboxDir scans the captured cmdline for the source's
// /run/vc/vm/<uuid>/ prefix, used to remap to the dest sandbox dir. We look
// for any token containing the sandbox root and trim the suffix at the
// next '/'. Returns ("", "") if no such token is found.
//
// The sandbox UUID is the directory's basename. Several tokens reference
// this path (qmp socket, vhostfs socket, hmp pipe, etc.); the first match
// wins because they all carry the same sandbox UUID.
func findSrcSandboxDir(args []string, sandboxRoot string) (sandboxDir, sandboxID string) {
	prefix := strings.TrimRight(sandboxRoot, "/") + "/"
	for _, a := range args {
		// arg may contain other text before the prefix (e.g. socket=) — find it.
		idx := strings.Index(a, prefix)
		if idx < 0 {
			continue
		}
		rest := a[idx+len(prefix):]
		end := strings.IndexAny(rest, "/,= \t")
		if end < 0 {
			end = len(rest)
		}
		id := rest[:end]
		if id == "" {
			continue
		}
		return prefix + id, id
	}
	return "", ""
}

// spawnReplayedQEMU performs the full destination cmdline-replay flow:
//
//  1. Reads cfg.ReplayCmdlineFile (one QEMU arg per line, plus argv[0]).
//  2. Computes path substitutions from the captured cmdline against
//     cfg.QMPSocket's parent (which is also the synthetic dest sandbox dir).
//  3. Copies the source nvdimm image to a writable temp file on dest.
//  4. Starts virtiofsd with --migration-on-error=guest-error.
//  5. Spawns the destination QEMU with the transformed cmdline plus
//     -incoming defer -daemonize. QEMU runs in the current process's
//     network namespace (the dest job is intentionally not hostNetwork
//     so it has its own pod IP).
//  6. Waits for the QEMU QMP socket to appear under the dest sandbox dir.
//
// On success cfg.QMPSocket points at a live QMP socket and the caller can
// proceed with the normal RunDestination flow. The function does not
// return cleanup handles — virtiofsd and QEMU are left running until they
// terminate naturally (dest job pod teardown). This matches e2e.sh, which
// also leaks them and relies on pod GC.
//
// Mutates cfg.QMPSocket to point at the spawned QEMU's monitor socket.
func spawnReplayedQEMU(ctx context.Context, cfg *DestConfig) error {
	args, err := readCmdlineFile(cfg.ReplayCmdlineFile)
	if err != nil {
		return err
	}
	if len(args) < 2 {
		return fmt.Errorf("cmdline file %s has too few args (%d), need argv[0] + at least one flag", cfg.ReplayCmdlineFile, len(args))
	}

	dstSandboxID := cfg.SandboxID
	if dstSandboxID == "" {
		dstSandboxID = destReplayDefaultSandbox
	}
	dstSandboxDir := filepath.Join(sandboxRoot, dstSandboxID)
	dstSocket := filepath.Join(dstSandboxDir, "extra-monitor.sock")

	srcSandboxDir, srcSandboxID := findSrcSandboxDir(args, sandboxRoot)
	if srcSandboxDir == "" {
		// Fall back to dst sandbox dir as both src and dst — substitution
		// becomes a no-op but at least the cmdline is still usable when the
		// source happened to use the same path layout (rare).
		slog.Warn("Could not locate source sandbox dir in captured cmdline; skipping path substitution", "sandbox_root", sandboxRoot)
	}

	srcNvdimm := extractNvdimmPath(args)
	dstNvdimm := ""
	if srcNvdimm != "" {
		dstNvdimm, err = copyNvdimmImage(srcNvdimm)
		if err != nil {
			return fmt.Errorf("copy nvdimm image: %w", err)
		}
		slog.Info("Copied nvdimm image to writable dest path", "src", srcNvdimm, "dst", dstNvdimm)
	}

	// Ensure dest sandbox dir + virtiofs shared dir exist before QEMU starts.
	if err := os.MkdirAll(dstSandboxDir, 0o755); err != nil {
		return fmt.Errorf("create dest sandbox dir %s: %w", dstSandboxDir, err)
	}
	// Wipe stale sockets from prior failed attempts so waitForSocket doesn't
	// return immediately on a leftover file. The dest sandbox dir is reused
	// across restart attempts of the dest job pod.
	for _, name := range []string{"extra-monitor.sock", "vhost-fs.sock", "console.sock"} {
		_ = os.Remove(filepath.Join(dstSandboxDir, name))
	}
	sharedDir := filepath.Join(kataSharedSandboxRoot, dstSandboxID, "shared")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		return fmt.Errorf("create virtiofs shared dir %s: %w", sharedDir, err)
	}

	// Start virtiofsd. e2e.sh:518 nohups the daemon; we use exec.Cmd with
	// detached stdio + Setpgid so the process survives our exit if needed.
	vhostSock := filepath.Join(dstSandboxDir, "vhost-fs.sock")
	if err := startVirtiofsd(ctx, vhostSock, sharedDir); err != nil {
		return fmt.Errorf("start virtiofsd: %w", err)
	}

	binary, qemuArgs, err := transformCmdline(args, srcSandboxDir, dstSandboxDir, srcSandboxID, dstSandboxID, srcNvdimm, dstNvdimm)
	if err != nil {
		return fmt.Errorf("transform cmdline: %w", err)
	}
	if cfg.QEMUBinary != "" {
		binary = cfg.QEMUBinary
	}
	if !filepath.IsAbs(binary) {
		slog.Warn("argv[0] is not absolute, falling back to default QEMU binary", "argv0", binary, "fallback", destReplayDefaultQEMU)
		binary = destReplayDefaultQEMU
	}

	slog.Info("Spawning destination QEMU via cmdline replay",
		"binary", binary,
		"args_count", len(qemuArgs),
		"dst_sandbox_dir", dstSandboxDir,
		"qmp_socket", dstSocket,
	)
	if err := spawnDetachedProcess(ctx, binary, qemuArgs); err != nil {
		return fmt.Errorf("spawn dest QEMU: %w", err)
	}

	if err := waitForSocket(ctx, dstSocket, destReplaySocketWaitTotal); err != nil {
		return fmt.Errorf("dest QEMU QMP socket %s did not appear: %w", dstSocket, err)
	}
	slog.Info("Destination QEMU is up; QMP socket ready", "qmp_socket", dstSocket)

	cfg.QMPSocket = dstSocket
	return nil
}

// copyNvdimmImage copies the source nvdimm image to a writable file under
// /tmp. The destination QEMU writes nvdimm pages received from the source
// during migration; mapping the kata-supplied readonly image read-only would
// SIGSEGV the dest QEMU on the first received nvdimm page (see the project
// memory: "Fix 1: Writable nvdimm on destination").
func copyNvdimmImage(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.CreateTemp("/tmp", "kata-dst-nvdimm-*.img")
	if err != nil {
		return "", fmt.Errorf("create temp nvdimm: %w", err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		_ = os.Remove(out.Name())
		return "", fmt.Errorf("copy %s -> %s: %w", src, out.Name(), err)
	}
	return out.Name(), nil
}

// startVirtiofsd spawns virtiofsd in the background. Mirrors the flag set
// from e2e.sh:518. --migration-on-error=guest-error is the critical bit:
// without it, inode-state mismatches between source and dest virtiofsd
// would abort the migration mid-stream (see project memory "Fix 2").
func startVirtiofsd(ctx context.Context, socketPath, sharedDir string) error {
	args := []string{
		"--socket-path=" + socketPath,
		"--shared-dir=" + sharedDir,
		"--cache=auto",
		"--thread-pool-size=1",
		"--announce-submounts",
		"--sandbox=none",
		"--migration-on-error=guest-error",
	}
	if err := spawnDetachedProcess(ctx, destReplayVirtiofsd, args); err != nil {
		return err
	}
	if err := waitForSocket(ctx, socketPath, destReplayVirtiofsdSettleDelay+3*time.Second); err != nil {
		return fmt.Errorf("virtiofsd socket %s did not appear: %w", socketPath, err)
	}
	return nil
}

// spawnDetachedProcess launches name+args as a detached child process. Stdout
// and stderr are silenced (matching e2e.sh's `>/dev/null 2>&1`). The child
// is not waited on; QEMU and virtiofsd run for the lifetime of the dest pod.
//
// We deliberately do not use exec.CommandContext because the context's
// cancellation should not kill QEMU mid-migration — QEMU exits on its own
// when migration completes (or when the dest job pod is torn down).
var spawnDetachedProcess = func(_ context.Context, name string, args []string) error {
	cmd := exec.Command(name, args...) // #nosec G204 -- args sourced from captured QEMU cmdline + fixed flag set
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe %s: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe %s: %w", name, err)
	}
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	slog.Info("child process started", "process", name, "pid", cmd.Process.Pid)
	// Stream both stderr and stdout to slog as they appear. Keeps QEMU
	// crash diagnostics visible via `kubectl logs` even after the parent
	// process moves on to RunDestination.
	streamPipe := func(stream string, r interface{ Read([]byte) (int, error) }) {
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				slog.Warn("child process output", "process", name, "stream", stream, "output", string(buf[:n]))
			}
			if rerr != nil {
				return
			}
		}
	}
	go streamPipe("stderr", stderr)
	go streamPipe("stdout", stdout)
	go func() {
		err := cmd.Wait()
		slog.Warn("child process exited", "process", name, "pid", cmd.Process.Pid, "error", fmt.Sprintf("%v", err))
	}()
	return nil
}

// waitForDestReady blocks until a TCP connection to (destIP, port) succeeds
// or total elapses. Used by the source side in replay-cmdline mode to wait
// for the dest job to spawn QEMU and open its migrate-incoming listener.
//
// Each connect attempt is bounded by destReadyDialTimeout so a black-holed
// dest doesn't pin the source for the full poll interval.
var waitForDestReady = func(ctx context.Context, destIP netip.Addr, port string, total time.Duration) error {
	deadline := time.Now().Add(total)
	addr := net.JoinHostPort(destIP.String(), port)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dialer := net.Dialer{Timeout: destReadyDialTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("dest %s not reachable after %s: %w", addr, total, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(destReadyPollInterval):
		}
	}
}

// waitForSocket polls until path exists as a UNIX socket or total elapses.
// Returns ctx.Err() if the context is cancelled first, else a "did not
// appear" error after the deadline.
var waitForSocket = func(ctx context.Context, path string, total time.Duration) error {
	deadline := time.Now().Add(total)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", total)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(destReplaySocketPollInterval):
		}
	}
}
