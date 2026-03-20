// katamaran orchestrates zero-packet-drop live migration for Kata Containers
// with support for both shared and non-shared (NBD drive-mirror) storage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/maci0/katamaran/internal/migration"
)

var version = "v0.3.0"

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
	fmt.Fprintf(w, `katamaran — Zero-packet-drop live migration for Kata Containers

Usage:
  katamaran --mode <source|dest> [flags]
  katamaran --version
  katamaran --help

Common flags:
  --qmp string             Path to QEMU QMP unix socket (default "/run/vc/vm/extra-monitor.sock")
  --drive-id string        QEMU block device ID to migrate (default "drive-virtio-disk0")
  --shared-storage         Skip NBD drive-mirror (use with shared storage)
  --multifd-channels int   Parallel TCP channels for RAM migration, 0 to disable (default 4)
  --log-format string      Log output format: 'text' or 'json' (default "text")

Source mode flags:
  --dest-ip string         Destination node IP address (required)
  --vm-ip string           VM pod IP for traffic redirection (required)
  --tunnel-mode string     Tunnel mode: 'ipip', 'gre', or 'none' (default "ipip")
  --downtime int           Max allowed downtime in milliseconds (default 25)
  --auto-downtime          Auto-calculate downtime based on RTT (overrides --downtime)

Destination mode flags:
  --tap string             Tap interface name for tc sch_plug buffering
  --tap-netns string       Network namespace path for tap interface (e.g. /proc/PID/ns/net)

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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // A second signal will now force exit
	}()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run contains all CLI logic: flag parsing, validation, and migration execution.
// Extracted from main() so the CLI validation paths can be tested without os.Exit.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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
	showVersion := fs.Bool("version", false, "Show version and exit")
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

	if *showVersion {
		fmt.Fprintf(stdout, "katamaran %s\n", version)
		return 0
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "Error: unexpected arguments: %s\n\n", strings.Join(fs.Args(), " "))
		printUsage(stderr)
		return 1
	}

	switch *logFormat {
	case "json":
		slog.SetDefault(slog.New(slog.NewJSONHandler(stderr, nil)))
	case "text":
		slog.SetDefault(slog.New(slog.NewTextHandler(stderr, nil)))
	default:
		fmt.Fprintf(stderr, "Error: invalid --log-format %q (valid: text, json)\n", *logFormat)
		return 1
	}

	var err error
	mode := role(*modeFlag)

	// Warn about mode-irrelevant flags before running.
	if mode == roleDest || mode == roleSource {
		fs.Visit(func(f *flag.Flag) {
			if mode == roleDest && sourceOnlyFlags[f.Name] {
				fmt.Fprintf(stderr, "Warning: --%s is ignored in dest mode\n", f.Name)
			}
			if mode == roleSource && destOnlyFlags[f.Name] {
				fmt.Fprintf(stderr, "Warning: --%s is ignored in source mode\n", f.Name)
			}
		})
	}

	if *multifdChannels < 0 {
		fmt.Fprintf(stderr, "Error: --multifd-channels must be non-negative, got %d\n", *multifdChannels)
		return 1
	}

	slog.Info("katamaran starting", "version", version, "mode", string(mode), "pid", os.Getpid())

	switch mode {
	case roleDest:
		err = migration.RunDestination(ctx, migration.DestConfig{
			QMPSocket:       *qmpSocket,
			TapIface:        *tapIface,
			TapNetns:        *tapNetns,
			DriveID:         *driveID,
			SharedStorage:   *sharedStorage,
			MultifdChannels: *multifdChannels,
		})
	case roleSource:
		var missing []string
		if *destIP == "" {
			missing = append(missing, "--dest-ip")
		}
		if *vmIP == "" {
			missing = append(missing, "--vm-ip")
		}
		if len(missing) > 0 {
			fmt.Fprintf(stderr, "Error: required flag(s) not set: %s\n", strings.Join(missing, ", "))
			return 1
		}
		parsedDest, err1 := netip.ParseAddr(*destIP)
		if err1 != nil {
			fmt.Fprintf(stderr, "Error: invalid --dest-ip %q: %v\n", *destIP, err1)
			return 1
		}
		parsedVM, err2 := netip.ParseAddr(*vmIP)
		if err2 != nil {
			fmt.Fprintf(stderr, "Error: invalid --vm-ip %q: %v\n", *vmIP, err2)
			return 1
		}
		parsedDest = parsedDest.Unmap()
		parsedVM = parsedVM.Unmap()
		if parsedDest.Is4() != parsedVM.Is4() {
			fmt.Fprintf(stderr, "Error: --dest-ip and --vm-ip address family mismatch (%s vs %s)\n",
				migration.IPFamily(parsedDest), migration.IPFamily(parsedVM))
			return 1
		}
		tm := migration.TunnelMode(*tunnelMode)
		if tm != migration.TunnelModeIPIP && tm != migration.TunnelModeGRE && tm != migration.TunnelModeNone {
			fmt.Fprintf(stderr, "Error: invalid --tunnel-mode %q (valid: ipip, gre, none)\n", *tunnelMode)
			return 1
		}
		if *downtimeLimit <= 0 {
			fmt.Fprintf(stderr, "Error: --downtime must be positive, got %d\n", *downtimeLimit)
			return 1
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
		})
	case "":
		fmt.Fprintf(stderr, "Error: --mode is required (valid: source, dest)\n\n")
		printUsage(stderr)
		return 1
	default:
		fmt.Fprintf(stderr, "Error: invalid mode %q (valid: source, dest)\n", *modeFlag)
		return 1
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("Migration aborted. Cleanup finished")
			return 130
		}
		slog.Error("Migration failed", "error", err)
		return 1
	}
	return 0
}
