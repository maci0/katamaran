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
		"dest-ip": true, "vm-ip": true, "tunnel-mode": true,
		"downtime": true, "auto-downtime": true,
	}
	destOnlyFlags = map[string]bool{
		"tap": true, "tap-netns": true,
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
  --vm-ip string           VM pod IP for traffic redirection (required)
  --tunnel-mode string     Tunnel mode: 'ipip', 'gre', or 'none' (default "ipip")
  --downtime int           Max allowed downtime in milliseconds, 1-60000 (default 25)
  --auto-downtime          Auto-calculate downtime based on RTT (overrides --downtime)

Destination mode flags:
  --tap string             Tap interface name for tc sch_plug buffering
  --tap-netns string       Network namespace path for tap interface (e.g. /proc/PID/ns/net)

Other:
  -v, --version            Show version and exit
  -h, --help               Show this help and exit

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
	multifdChannels := fs.Int("multifd-channels", migration.DefaultMultifdChannels, "Parallel TCP channels for RAM migration (0 to disable)")
	logFormat := fs.String("log-format", "text", "Log output format: 'text' or 'json'")
	logLevel := fs.String("log-level", "info", "Log level: 'debug', 'info', 'warn', or 'error'")
	podName := fs.String("pod-name", "", "source pod name (alternative to --qmp/--vm-ip)")
	podNS := fs.String("pod-namespace", "", "source pod namespace")
	destPodName := fs.String("dest-pod-name", "", "destination pod name (alternative to --qmp)")
	destPodNS := fs.String("dest-pod-namespace", "", "destination pod namespace")
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

	// Warn about mode-irrelevant flags and conflicting flag combinations.
	explicitDowntime := false
	fs.Visit(func(f *flag.Flag) {
		if mode == roleDest && sourceOnlyFlags[f.Name] {
			slog.Warn("Flag ignored in dest mode", "flag", f.Name)
		}
		if mode == roleSource && destOnlyFlags[f.Name] {
			slog.Warn("Flag ignored in source mode", "flag", f.Name)
		}
		if f.Name == "downtime" {
			explicitDowntime = true
		}
	})
	if *autoDowntime && explicitDowntime {
		slog.Warn("--auto-downtime overrides --downtime; explicit --downtime value will be ignored")
	}

	if *multifdChannels < 0 {
		_, _ = fmt.Fprintf(stderr, "Error: --multifd-channels must be non-negative, got %d\n\n", *multifdChannels)
		printUsage(stderr)
		return 2
	}
	slog.Info("katamaran starting", "version", buildinfo.Version, "mode", string(mode), "pid", os.Getpid())

	var err error
	switch mode {
	case roleDest:
		// Validate that --dest-pod-name and --dest-pod-namespace come together.
		// Unlike source, no XOR check is needed: --qmp has a sensible default
		// and the resolver overrides it (including the well-known migrate.sh
		// placeholder) when a dest pod is supplied.
		destSeen := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { destSeen[f.Name] = true })
		if destSeen["dest-pod-name"] != destSeen["dest-pod-namespace"] {
			_, _ = fmt.Fprintf(stderr, "Error: --dest-pod-name and --dest-pod-namespace must be supplied together\n\n")
			printUsage(stderr)
			return 2
		}
		err = migration.RunDestination(ctx, migration.DestConfig{
			QMPSocket:        *qmpSocket,
			TapIface:         *tapIface,
			TapNetns:         *tapNetns,
			DriveID:          *driveID,
			SharedStorage:    *sharedStorage,
			MultifdChannels:  *multifdChannels,
			DestPodName:      *destPodName,
			DestPodNamespace: *destPodNS,
		})
	case roleSource:
		if *destIP == "" {
			_, _ = fmt.Fprintf(stderr, "Error: required flag(s) not set: --dest-ip\n\n")
			printUsage(stderr)
			return 2
		}
		// XOR check: exactly one of (--qmp + --vm-ip) or (--pod-name + --pod-namespace)
		// must be supplied. Use fs.Visit to detect explicitly-set flags, since
		// --qmp has a non-empty default value.
		seen := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
		visitedPodName := seen["pod-name"]
		visitedPodNS := seen["pod-namespace"]
		if visitedPodName != visitedPodNS {
			_, _ = fmt.Fprintf(stderr, "Error: --pod-name and --pod-namespace must be supplied together\n\n")
			printUsage(stderr)
			return 2
		}
		hasPod := *podName != "" && *podNS != ""
		hasExplicit := seen["qmp"] && seen["vm-ip"]
		if hasPod == hasExplicit {
			_, _ = fmt.Fprintf(stderr, "Error: source mode requires exactly one of: (--qmp + --vm-ip) or (--pod-name + --pod-namespace)\n\n")
			printUsage(stderr)
			return 2
		}

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
		} else {
			// TODO(task-7): in pod mode, populate parsedVM after resolver runs
			// and re-run the IP family check that we currently skip.
		}
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
			QMPSocket:       *qmpSocket,
			DestIP:          parsedDest,
			VMIP:            parsedVM,
			DriveID:         *driveID,
			SharedStorage:   *sharedStorage,
			TunnelMode:      tm,
			DowntimeLimitMS: *downtimeLimit,
			AutoDowntime:    *autoDowntime,
			MultifdChannels: *multifdChannels,
			PodName:         *podName,
			PodNamespace:    *podNS,
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
