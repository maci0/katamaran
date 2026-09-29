// Package katamaran implements the primary katamaran CLI.
//
// It orchestrates zero-packet-drop live migration for Kata Containers
// with support for both shared and non-shared (NBD drive-mirror) storage.
package katamaran

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strings"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/logging"
	"github.com/maci0/katamaran/internal/migration"
)

type role string

const (
	roleSource role = "source"
	roleDest   role = "dest"
)

// sourceOnlyFlags and destOnlyFlags identify flags that are only meaningful
// in one mode, used to warn users when flags are provided for the wrong mode.
var (
	sourceOnlyFlags = map[string]bool{
		"dest-ip":                true,
		"vm-ip":                  true,
		"tunnel-mode":            true,
		"downtime":               true,
		"auto-downtime":          true,
		"auto-downtime-floor-ms": true,
		"cni-convergence-delay":  true,
		"emit-cmdline-to":        true,
	}
	destOnlyFlags = map[string]bool{
		"tap":                     true,
		"tap-netns":               true,
		"replay-cmdline":          true,
		"replay-cmdline-from-pod": true,
		"dest-pod-name":           true,
		"dest-pod-namespace":      true,
	}
)

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `katamaran: Zero-packet-drop live migration for Kata Containers

Usage:
  katamaran --mode <source|dest> [flags]
  katamaran --version
  katamaran --help

Common flags:
  --mode string            Migration role: 'source' or 'dest' (required)
  --qmp string             Path to QEMU QMP unix socket (default "/run/vc/vm/extra-monitor.sock")
  --drive-id string        QEMU block device ID(s), comma-separated for multi-disk (default "drive-virtio-disk0")
  --shared-storage         Skip NBD drive-mirror (use with shared storage)
  --multifd-channels int   Parallel TCP channels for RAM migration, 0 to disable (default 4)
  --log-format string      Log output format: 'text' or 'json' (default "text")
  --log-level string       Log level: 'debug', 'info', 'warn', or 'error' (default "info")

Source mode flags:
  --dest-ip string         Destination node IP address (required)
  --vm-ip string           VM pod IP for traffic redirection (required unless using pod mode)
  --pod-name string        Source pod name (alternative to --qmp/--vm-ip)
  --pod-namespace string   Source pod namespace (required with --pod-name)
  --tunnel-mode string     Tunnel mode: 'ipip', 'gre', or 'none' (default "ipip")
  --downtime int           Max allowed downtime in milliseconds, 1-60000 (default 25)
  --auto-downtime          Auto-calculate downtime based on RTT (overrides --downtime)
  --auto-downtime-floor-ms int
                           Lower bound + overhead for auto-downtime in ms, 0-60000 (0 uses compiled-in 25ms; ignored without --auto-downtime)
  --cni-convergence-delay duration
                           Post-cutover wait keeping the IP tunnel alive while the CNI rebinds the pod (0 uses compiled-in 5s)
  --emit-cmdline-to string Capture source QEMU /proc/<pid>/cmdline to this path before migration

Destination mode flags:
  --tap string             Tap interface name for tc sch_plug buffering
  --tap-netns string       Network namespace path for tap interface (e.g. /proc/PID/ns/net)
  --dest-pod-name string   Destination pod name (alternative to --qmp)
  --dest-pod-namespace string
                           Destination pod namespace (required with --dest-pod-name)
  --pod-name string        Source pod name for VMConfig fallback (requires --pod-namespace)
  --pod-namespace string   Source pod namespace for VMConfig fallback (requires --pod-name)
  --replay-cmdline string  Spawn QEMU on dest by replaying captured source cmdline (with -incoming defer)
  --replay-cmdline-from-pod string
                           Fetch source QEMU cmdline from the named source pod's log ('<namespace>/<name>') instead of a hostPath file (requires pods/log get on the SA)

Output:
  stdout   Source mode only, and only the KATAMARAN_* marker lines the
           orchestrator and deploy/migrate.sh scrape: KATAMARAN_DOWNTIME_LIMIT,
           KATAMARAN_PHASE, KATAMARAN_PROGRESS, KATAMARAN_RESULT,
           KATAMARAN_CMDLINE_AT, KATAMARAN_CMDLINE_B64, and the VMConfig
           markers. Dest mode writes nothing to stdout. Safe to redirect to a
           file; the human-readable log stays on stderr either way.
  stderr   The full log, in --log-format, plus every error and warning.

Other:
  -v, --version            Show version and exit
  -h, --help               Show this help and exit

Exit codes:
  0   Migration succeeded
  1   Migration failed (runtime error)
  2   Argument or validation error
  130 Interrupted by signal (SIGINT/SIGTERM)

Environment variables:
  KATAMARAN_MIGRATION_ID   Correlation ID added to all log entries (set by orchestration paths)

Examples:
  # Destination (run first)
  katamaran --mode dest --qmp /run/vc/vm/<id>/extra-monitor.sock --tap tap0_kata

  # Source
  katamaran --mode source --qmp /run/vc/vm/<id>/extra-monitor.sock \
    --dest-ip 10.0.0.2 --vm-ip 10.244.1.5

  # Source with shared storage and GRE tunnel
  katamaran --mode source --qmp /run/vc/vm/<id>/extra-monitor.sock \
    --dest-ip 10.0.0.2 --vm-ip 10.244.1.5 --shared-storage --tunnel-mode gre

  # Source in pod mode (resolve QMP and VM IP from a Kubernetes pod)
  katamaran --mode source --dest-ip 10.0.0.2 \
    --pod-name kata-demo --pod-namespace default
`)
}

// usageError prints a validation failure followed by the usage text and
// returns the argument-error exit code. Every flag validation in Run funnels
// through here so the message shape and the exit code cannot drift apart.
func usageError(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, "Error: "+format+"\n\n", args...)
	printUsage(stderr)
	return 2
}

// configUsageError reports a rejected configuration value from the
// migration package. Its errors carry the ErrInvalidConfig sentinel, which
// the caller has already established; only the flag-specific part is worth
// showing.
func configUsageError(stderr io.Writer, err error) int {
	return usageError(stderr, "%s", strings.TrimPrefix(err.Error(), migration.ErrInvalidConfig.Error()+": "))
}

// Run contains all CLI logic: flag parsing, validation, and migration execution.
// It is separate from cmd/katamaran so validation paths can be tested without os.Exit.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("katamaran", flag.ContinueOnError)
	fs.SetOutput(stderr)

	modeFlag := fs.String("mode", "", "Migration role: 'source' or 'dest'")
	qmpSocket := fs.String("qmp", migration.DefaultQMPSocket, "Path to QEMU QMP unix socket")
	tapIface := fs.String("tap", "", "Tap interface name for tc sch_plug buffering")
	tapNetns := fs.String("tap-netns", "", "Network namespace path for tap interface")
	destIP := fs.String("dest-ip", "", "Destination node IP address")
	vmIP := fs.String("vm-ip", "", "VM pod IP for traffic redirection")
	driveID := fs.String("drive-id", "drive-virtio-disk0", "QEMU block device ID(s), comma-separated for multi-disk")
	sharedStorage := fs.Bool("shared-storage", false, "Skip NBD drive-mirror (use with shared storage)")
	tunnelMode := fs.String("tunnel-mode", "ipip", "Tunnel mode: 'ipip', 'gre', or 'none'")
	downtimeLimit := fs.Int("downtime", 25, fmt.Sprintf("Max allowed downtime in milliseconds (1-%d)", migration.MaxDowntimeMS))
	autoDowntime := fs.Bool("auto-downtime", false, "Auto-calculate downtime based on RTT (overrides --downtime)")
	autoDowntimeFloor := fs.Int("auto-downtime-floor-ms", 0, fmt.Sprintf("Lower bound + overhead for the auto-calculated downtime, 0-%d (0 uses the compiled-in default of 25ms). Ignored without --auto-downtime", migration.MaxDowntimeMS))
	cniConvergenceDelay := fs.Duration("cni-convergence-delay", 0, "Post-cutover wait that keeps the IP tunnel alive while the CNI propagates the pod's new node binding (0 uses the compiled-in default of 5s)")
	multifdChannels := fs.Int("multifd-channels", migration.DefaultMultifdChannels, "Parallel TCP channels for RAM migration (0 to disable)")
	logFormat := fs.String("log-format", "text", "Log output format: 'text' or 'json'")
	logLevel := fs.String("log-level", "info", "Log level: 'debug', 'info', 'warn', or 'error'")
	podName := fs.String("pod-name", "", "Source pod name (alternative to --qmp/--vm-ip)")
	podNS := fs.String("pod-namespace", "", "Source pod namespace (required with --pod-name)")
	destPodName := fs.String("dest-pod-name", "", "Destination pod name (alternative to --qmp)")
	destPodNS := fs.String("dest-pod-namespace", "", "Destination pod namespace (required with --dest-pod-name)")
	emitCmdlineTo := fs.String("emit-cmdline-to", "", "Source mode: capture /proc/<qemu_pid>/cmdline to this path before migration (the file is removed when the source run ends)")
	replayCmdline := fs.String("replay-cmdline", "", "Dest mode: spawn QEMU by replaying the source cmdline at this path with -incoming defer")
	replayCmdlineFromPod := fs.String("replay-cmdline-from-pod", "", "Dest mode: fetch the source QEMU cmdline from the named source pod's log (`<namespace>/<name>`) instead of a hostPath file. Requires pods/log get on the SA")
	showVersion := fs.Bool("version", false, "Show version and exit")
	showVersionShort := fs.Bool("v", false, "")
	helpFlag := fs.Bool("help", false, "")
	helpFlagShort := fs.Bool("h", false, "")

	fs.Usage = func() { printUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *helpFlag || *helpFlagShort {
		printUsage(stdout)
		return 0
	}

	if *showVersion || *showVersionShort {
		_, _ = fmt.Fprintf(stdout, "katamaran %s\n", buildinfo.Version)
		return 0
	}

	if fs.NArg() > 0 {
		return usageError(stderr, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	seenFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seenFlags[f.Name] = true })

	// Normalize enum flags for case-insensitive matching.
	*modeFlag = strings.ToLower(*modeFlag)
	*logFormat = strings.ToLower(*logFormat)
	*logLevel = strings.ToLower(*logLevel)
	*tunnelMode = strings.ToLower(*tunnelMode)

	mode := role(*modeFlag)

	// Validate mode before any side effects (logger setup, warnings).
	switch mode {
	case roleSource, roleDest:
	case "":
		return usageError(stderr, "--mode is required (valid: source, dest)")
	default:
		return usageError(stderr, "invalid --mode %q (valid: source, dest)", *modeFlag)
	}
	if err := logging.SetupLogger(stderr, *logFormat, *logLevel, "katamaran"); err != nil {
		return usageError(stderr, "%v", err)
	}
	// Propagate migration ID from the dashboard's environment variable
	// into all log entries for cross-component correlation.
	if mid := os.Getenv("KATAMARAN_MIGRATION_ID"); mid != "" {
		slog.SetDefault(slog.Default().With("migration_id", mid))
	}

	if mode == roleSource && (*autoDowntimeFloor < 0 || *autoDowntimeFloor > migration.MaxDowntimeMS) {
		return usageError(stderr, "--auto-downtime-floor-ms must be between 0 and %d, got %d", migration.MaxDowntimeMS, *autoDowntimeFloor)
	}
	if mode == roleSource && *cniConvergenceDelay < 0 {
		return usageError(stderr, "--cni-convergence-delay must be non-negative, got %s", *cniConvergenceDelay)
	}

	// Warn about mode-irrelevant flags and conflicting flag combinations.
	fs.Visit(func(f *flag.Flag) {
		if mode == roleDest && sourceOnlyFlags[f.Name] {
			slog.Warn("Flag ignored in dest mode", "flag", f.Name)
		}
		if mode == roleSource && destOnlyFlags[f.Name] {
			slog.Warn("Flag ignored in source mode", "flag", f.Name)
		}
	})
	if mode == roleSource && *autoDowntime && seenFlags["downtime"] {
		slog.Warn("--auto-downtime overrides --downtime; explicit --downtime value will be ignored")
	}
	if mode == roleSource && seenFlags["auto-downtime-floor-ms"] && !*autoDowntime {
		slog.Warn("--auto-downtime-floor-ms is ignored without --auto-downtime")
	}

	var err error
	switch mode {
	case roleDest:
		// Validate that --dest-pod-name and --dest-pod-namespace come together.
		// Unlike source, no XOR check is needed: --qmp has a sensible default
		// and the resolver overrides it (including the well-known
		// destDefaultQMPSocket placeholder) when a dest pod is supplied.
		if seenFlags["dest-pod-name"] != seenFlags["dest-pod-namespace"] {
			return usageError(stderr, "--dest-pod-name and --dest-pod-namespace must be supplied together")
		}
		if seenFlags["pod-name"] != seenFlags["pod-namespace"] {
			return usageError(stderr, "--pod-name and --pod-namespace must be supplied together")
		}
		if *replayCmdline != "" && *replayCmdlineFromPod != "" {
			return usageError(stderr, "--replay-cmdline and --replay-cmdline-from-pod are mutually exclusive")
		}
		var sourcePodRef string
		if *podNS != "" && *podName != "" {
			sourcePodRef = *podNS + "/" + *podName
		}
		cfg := migration.DestConfig{
			QMPSocket:            *qmpSocket,
			TapIface:             *tapIface,
			TapNetns:             *tapNetns,
			DriveIDs:             strings.Split(*driveID, ","),
			SharedStorage:        *sharedStorage,
			MultifdChannels:      *multifdChannels,
			DestPodName:          *destPodName,
			DestPodNamespace:     *destPodNS,
			ReplayCmdlineFile:    *replayCmdline,
			ReplayCmdlineFromPod: *replayCmdlineFromPod,
			SourcePodRef:         sourcePodRef,
		}
		// The migration package re-checks this; running it here is what
		// turns a bad --tap, --tap-netns, --drive-id, or --multifd-channels
		// into the documented exit 2 instead of a failed migration (1).
		if verr := migration.ValidateDestConfig(cfg); verr != nil {
			return configUsageError(stderr, verr)
		}
		slog.Info("katamaran starting", "version", buildinfo.Version, "mode", string(mode), "pid", os.Getpid())
		err = migration.RunDestination(ctx, cfg)
	case roleSource:
		cfg := migration.SourceConfig{
			QMPSocket:           *qmpSocket,
			DriveIDs:            strings.Split(*driveID, ","),
			SharedStorage:       *sharedStorage,
			TunnelMode:          migration.TunnelMode(*tunnelMode),
			DowntimeLimitMS:     *downtimeLimit,
			AutoDowntime:        *autoDowntime,
			AutoDowntimeFloorMS: *autoDowntimeFloor,
			CNIConvergenceDelay: *cniConvergenceDelay,
			MultifdChannels:     *multifdChannels,
			PodName:             *podName,
			PodNamespace:        *podNS,
			EmitCmdlineTo:       *emitCmdlineTo,
			Out:                 stdout,
		}
		if verr := migration.ValidateSourceConfig(cfg); verr != nil {
			return configUsageError(stderr, verr)
		}
		if *destIP == "" {
			return usageError(stderr, "--dest-ip is required")
		}
		// Mode selection: pod mode requires both pod flags; legacy mode requires
		// --vm-ip (and uses --qmp's default if not explicitly set). Mixing the
		// two pod flags with --vm-ip or an explicit --qmp is rejected.
		visitedPodName := seenFlags["pod-name"]
		visitedPodNS := seenFlags["pod-namespace"]
		if visitedPodName != visitedPodNS {
			return usageError(stderr, "--pod-name and --pod-namespace must be supplied together")
		}
		hasPod := *podName != "" && *podNS != ""
		if hasPod && (seenFlags["vm-ip"] || seenFlags["qmp"]) {
			return usageError(stderr, "--pod-name/--pod-namespace cannot be combined with --qmp or --vm-ip")
		}
		if !hasPod && *vmIP == "" {
			return usageError(stderr, "source mode requires either (--vm-ip [+ --qmp]) or (--pod-name + --pod-namespace)")
		}
		hasExplicit := !hasPod

		var parsedVM netip.Addr
		var err2 error
		parsedDest, err1 := netip.ParseAddr(*destIP)
		if err1 != nil {
			return usageError(stderr, "invalid --dest-ip %q: %v", *destIP, err1)
		}
		parsedDest = parsedDest.Unmap()
		if hasExplicit {
			parsedVM, err2 = netip.ParseAddr(*vmIP)
			if err2 != nil {
				return usageError(stderr, "invalid --vm-ip %q: %v", *vmIP, err2)
			}
			parsedVM = parsedVM.Unmap()
			if parsedDest.Is4() != parsedVM.Is4() {
				return usageError(stderr, "--dest-ip and --vm-ip address family mismatch (%s vs %s)",
					migration.IPFamily(parsedDest), migration.IPFamily(parsedVM))
			}
		}
		// In pod mode the VM IP is resolved inside the source binary at
		// runtime, so we can't validate IP family vs --dest-ip here. The
		// resolver enforces it itself before opening the migration
		// listener.
		if *downtimeLimit < 1 || *downtimeLimit > migration.MaxDowntimeMS {
			return usageError(stderr, "--downtime must be between 1 and %d, got %d", migration.MaxDowntimeMS, *downtimeLimit)
		}
		cfg.DestIP = parsedDest
		cfg.VMIP = parsedVM

		slog.Info("katamaran starting", "version", buildinfo.Version, "mode", string(mode), "pid", os.Getpid())
		err = migration.RunSource(ctx, cfg)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("Migration aborted. Cleanup finished", "mode", string(mode))
			return 130
		}
		slog.Error("Migration failed", "mode", string(mode), "error", err)
		return 1
	}
	return 0
}
