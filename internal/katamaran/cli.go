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
		"emit-cmdline-to":        true,
		"pod-name":               true,
		"pod-namespace":          true,
	}
	destOnlyFlags = map[string]bool{
		"tap":                true,
		"tap-netns":          true,
		"replay-cmdline":     true,
		"dest-pod-name":      true,
		"dest-pod-namespace": true,
	}
)

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `katamaran — Zero-packet-drop live migration for Kata Containers

Usage:
  katamaran --mode <source|dest> [flags]
  katamaran --version
  katamaran --help

Common flags:
  --mode string            Migration role: 'source' or 'dest' (required)
  --qmp string             Path to QEMU QMP unix socket (default "/run/vc/vm/extra-monitor.sock")
  --drive-id string        QEMU block device ID to migrate (default "drive-virtio-disk0")
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
                           Lower bound + overhead for auto-downtime in ms (default 0, uses 25ms)
  --emit-cmdline-to string Capture source QEMU /proc/<pid>/cmdline to this path before migration

Destination mode flags:
  --tap string             Tap interface name for tc sch_plug buffering
  --tap-netns string       Network namespace path for tap interface (e.g. /proc/PID/ns/net)
  --dest-pod-name string   Destination pod name (alternative to --qmp)
  --dest-pod-namespace string
                           Destination pod namespace (required with --dest-pod-name)
  --replay-cmdline string  Spawn QEMU on dest by replaying captured source cmdline (with -incoming defer)

Other:
  -v, --version            Show version and exit
  -h, --help               Show this help and exit

Exit codes:
  0   Migration succeeded
  1   Migration failed (runtime error)
  2   Argument or validation error
  130 Interrupted by signal (SIGINT/SIGTERM)

Environment variables:
  KATAMARAN_MIGRATION_ID   Correlation ID added to all log entries (set by the dashboard)

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

// Run contains all CLI logic: flag parsing, validation, and migration execution.
// It is separate from cmd/katamaran so validation paths can be tested without os.Exit.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("katamaran", flag.ContinueOnError)
	fs.SetOutput(stderr)

	modeFlag := fs.String("mode", "", "Migration role: 'source' or 'dest'")
	qmpSocket := fs.String("qmp", "/run/vc/vm/extra-monitor.sock", "Path to QEMU QMP unix socket")
	tapIface := fs.String("tap", "", "Tap interface name for tc sch_plug buffering")
	tapNetns := fs.String("tap-netns", "", "Network namespace path for tap interface")
	destIP := fs.String("dest-ip", "", "Destination node IP address")
	vmIP := fs.String("vm-ip", "", "VM pod IP for traffic redirection")
	driveID := fs.String("drive-id", "drive-virtio-disk0", "QEMU block device ID to migrate")
	sharedStorage := fs.Bool("shared-storage", false, "Skip NBD drive-mirror (use with shared storage)")
	tunnelMode := fs.String("tunnel-mode", "ipip", "Tunnel mode: 'ipip', 'gre', or 'none'")
	downtimeLimit := fs.Int("downtime", 25, "Max allowed downtime in milliseconds")
	autoDowntime := fs.Bool("auto-downtime", false, "Auto-calculate downtime based on RTT")
	autoDowntimeFloor := fs.Int("auto-downtime-floor-ms", 0, "Lower bound + overhead for the auto-calculated downtime (0 uses the compiled-in default of 25ms). Ignored without --auto-downtime")
	multifdChannels := fs.Int("multifd-channels", migration.DefaultMultifdChannels, "Parallel TCP channels for RAM migration (0 to disable)")
	logFormat := fs.String("log-format", "text", "Log output format: 'text' or 'json'")
	logLevel := fs.String("log-level", "info", "Log level: 'debug', 'info', 'warn', or 'error'")
	podName := fs.String("pod-name", "", "source pod name (alternative to --qmp/--vm-ip)")
	podNS := fs.String("pod-namespace", "", "source pod namespace")
	destPodName := fs.String("dest-pod-name", "", "destination pod name (alternative to --qmp)")
	destPodNS := fs.String("dest-pod-namespace", "", "destination pod namespace")
	emitCmdlineTo := fs.String("emit-cmdline-to", "", "source mode: capture /proc/<qemu_pid>/cmdline to this path before migration")
	replayCmdline := fs.String("replay-cmdline", "", "dest mode: spawn QEMU by replaying the source cmdline at this path with -incoming defer")
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
		_, _ = fmt.Fprintf(stderr, "Error: unexpected arguments: %s\n\n", strings.Join(fs.Args(), " "))
		printUsage(stderr)
		return 2
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
		// valid
	case "":
		_, _ = fmt.Fprintf(stderr, "Error: --mode is required (valid: source, dest)\n\n")
		printUsage(stderr)
		return 2
	default:
		_, _ = fmt.Fprintf(stderr, "Error: invalid --mode %q (valid: source, dest)\n\n", *modeFlag)
		printUsage(stderr)
		return 2
	}
	if err := logging.SetupLogger(stderr, *logFormat, *logLevel, "katamaran"); err != nil {
		_, _ = fmt.Fprintf(stderr, "Error: %v\n\n", err)
		printUsage(stderr)
		return 2
	}
	// Propagate migration ID from the dashboard's environment variable
	// into all log entries for cross-component correlation.
	if mid := os.Getenv("KATAMARAN_MIGRATION_ID"); mid != "" {
		slog.SetDefault(slog.Default().With("migration_id", mid))
	}

	if *multifdChannels < 0 {
		_, _ = fmt.Fprintf(stderr, "Error: --multifd-channels must be non-negative, got %d\n\n", *multifdChannels)
		printUsage(stderr)
		return 2
	}
	if mode == roleSource && *autoDowntimeFloor < 0 {
		_, _ = fmt.Fprintf(stderr, "Error: --auto-downtime-floor-ms must be non-negative, got %d\n\n", *autoDowntimeFloor)
		printUsage(stderr)
		return 2
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
	if *autoDowntime && seenFlags["downtime"] {
		slog.Warn("--auto-downtime overrides --downtime; explicit --downtime value will be ignored")
	}
	if mode == roleSource && seenFlags["auto-downtime-floor-ms"] && !*autoDowntime {
		slog.Warn("--auto-downtime-floor-ms is ignored without --auto-downtime")
	}
	slog.Info("katamaran starting", "version", buildinfo.Version, "mode", string(mode), "pid", os.Getpid())

	var err error
	switch mode {
	case roleDest:
		// Validate that --dest-pod-name and --dest-pod-namespace come together.
		// Unlike source, no XOR check is needed: --qmp has a sensible default
		// and the resolver overrides it (including the well-known migrate.sh
		// placeholder) when a dest pod is supplied.
		if seenFlags["dest-pod-name"] != seenFlags["dest-pod-namespace"] {
			_, _ = fmt.Fprintf(stderr, "Error: --dest-pod-name and --dest-pod-namespace must be supplied together\n\n")
			printUsage(stderr)
			return 2
		}
		err = migration.RunDestination(ctx, migration.DestConfig{
			QMPSocket:         *qmpSocket,
			TapIface:          *tapIface,
			TapNetns:          *tapNetns,
			DriveID:           *driveID,
			SharedStorage:     *sharedStorage,
			MultifdChannels:   *multifdChannels,
			DestPodName:       *destPodName,
			DestPodNamespace:  *destPodNS,
			ReplayCmdlineFile: *replayCmdline,
		})
	case roleSource:
		if *destIP == "" {
			_, _ = fmt.Fprintf(stderr, "Error: required flag(s) not set: --dest-ip\n\n")
			printUsage(stderr)
			return 2
		}
		// Mode selection: pod mode requires both pod flags; legacy mode requires
		// --vm-ip (and uses --qmp's default if not explicitly set). Mixing the
		// two pod flags with --vm-ip or an explicit --qmp is rejected.
		visitedPodName := seenFlags["pod-name"]
		visitedPodNS := seenFlags["pod-namespace"]
		if visitedPodName != visitedPodNS {
			_, _ = fmt.Fprintf(stderr, "Error: --pod-name and --pod-namespace must be supplied together\n\n")
			printUsage(stderr)
			return 2
		}
		hasPod := *podName != "" && *podNS != ""
		if hasPod && (seenFlags["vm-ip"] || seenFlags["qmp"]) {
			_, _ = fmt.Fprintf(stderr, "Error: --pod-name/--pod-namespace cannot be combined with --qmp or --vm-ip\n\n")
			printUsage(stderr)
			return 2
		}
		if !hasPod && *vmIP == "" {
			_, _ = fmt.Fprintf(stderr, "Error: source mode requires either (--vm-ip [+ --qmp]) or (--pod-name + --pod-namespace)\n\n")
			printUsage(stderr)
			return 2
		}
		hasExplicit := !hasPod

		var parsedDest, parsedVM netip.Addr
		var err1 error
		parsedDest, err1 = netip.ParseAddr(*destIP)
		if err1 != nil {
			_, _ = fmt.Fprintf(stderr, "Error: invalid --dest-ip %q: %v\n\n", *destIP, err1)
			printUsage(stderr)
			return 2
		}
		parsedDest = parsedDest.Unmap()
		if hasExplicit {
			var err2 error
			parsedVM, err2 = netip.ParseAddr(*vmIP)
			if err2 != nil {
				_, _ = fmt.Fprintf(stderr, "Error: invalid --vm-ip %q: %v\n\n", *vmIP, err2)
				printUsage(stderr)
				return 2
			}
			parsedVM = parsedVM.Unmap()
			if parsedDest.Is4() != parsedVM.Is4() {
				_, _ = fmt.Fprintf(stderr, "Error: --dest-ip and --vm-ip address family mismatch (%s vs %s)\n\n",
					migration.IPFamily(parsedDest), migration.IPFamily(parsedVM))
				printUsage(stderr)
				return 2
			}
		}
		// In pod mode the VM IP is resolved inside the source binary at
		// runtime, so we can't validate IP family vs --dest-ip here. The
		// resolver enforces it itself before opening the migration
		// listener.
		tm := migration.TunnelMode(*tunnelMode)
		if tm != migration.TunnelModeIPIP && tm != migration.TunnelModeGRE && tm != migration.TunnelModeNone {
			_, _ = fmt.Fprintf(stderr, "Error: invalid --tunnel-mode %q (valid: ipip, gre, none)\n\n", *tunnelMode)
			printUsage(stderr)
			return 2
		}
		if *downtimeLimit < 1 || *downtimeLimit > 60000 {
			_, _ = fmt.Fprintf(stderr, "Error: --downtime must be between 1 and 60000, got %d\n\n", *downtimeLimit)
			printUsage(stderr)
			return 2
		}

		err = migration.RunSource(ctx, migration.SourceConfig{
			QMPSocket:           *qmpSocket,
			DestIP:              parsedDest,
			VMIP:                parsedVM,
			DriveID:             *driveID,
			SharedStorage:       *sharedStorage,
			TunnelMode:          tm,
			DowntimeLimitMS:     *downtimeLimit,
			AutoDowntime:        *autoDowntime,
			AutoDowntimeFloorMS: *autoDowntimeFloor,
			MultifdChannels:     *multifdChannels,
			PodName:             *podName,
			PodNamespace:        *podNS,
			EmitCmdlineTo:       *emitCmdlineTo,
		})
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
