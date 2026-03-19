// katamaran orchestrates zero-packet-drop live migration for Kata Containers
// with support for both shared and non-shared (NBD drive-mirror) storage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/maci0/katamaran/internal/migration"
)

var version = "v0.3.0"

type Role string

const (
	RoleSource Role = "source"
	RoleDest   Role = "dest"
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

func printUsage() {
	fmt.Fprintf(os.Stderr, `katamaran — Zero-packet-drop live migration for Kata Containers

Usage:
  katamaran --mode <source|dest> [flags]
  katamaran --version

Common flags:
  --qmp string             Path to QEMU QMP unix socket (default "/run/vc/vm/extra-monitor.sock")
  --drive-id string        QEMU block device ID to migrate (default "drive-virtio-disk0")
  --shared-storage         Skip NBD drive-mirror (use with shared storage)
  --multifd-channels int   Parallel TCP channels for RAM migration, 0 to disable (default 4)

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
	modeFlag := flag.String("mode", "", "Migration role: 'source' or 'dest'")
	qmpSocket := flag.String("qmp", "/run/vc/vm/extra-monitor.sock", "Path to QEMU QMP unix socket")
	tapIface := flag.String("tap", "", "Tap interface name for tc sch_plug buffering")
	tapNetns := flag.String("tap-netns", "", "Network namespace path for tap interface")
	destIP := flag.String("dest-ip", "", "Destination node IP address")
	vmIP := flag.String("vm-ip", "", "VM pod IP for traffic redirection")
	driveID := flag.String("drive-id", "drive-virtio-disk0", "QEMU block device ID to migrate")
	sharedStorage := flag.Bool("shared-storage", false, "Skip NBD drive-mirror (use with shared storage)")
	tunnelMode := flag.String("tunnel-mode", "ipip", "Tunnel mode: 'ipip', 'gre', or 'none'")
	downtimeLimit := flag.Int("downtime", 25, "Max allowed downtime in milliseconds")
	autoDowntime := flag.Bool("auto-downtime", false, "Auto-calculate downtime based on RTT")
	multifdChannels := flag.Int("multifd-channels", migration.DefaultMultiFDChannels, "Parallel TCP channels for RAM migration (0 to disable)")
	showVersion := flag.Bool("version", false, "Show version and exit")

	flag.Usage = printUsage
	flag.Parse()

	if *showVersion {
		fmt.Printf("katamaran %s\n", version)
		os.Exit(0)
	}

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %s\n\n", strings.Join(flag.Args(), " "))
		printUsage()
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // A second signal will now force exit
	}()

	var err error
	mode := Role(*modeFlag)

	// Warn about mode-irrelevant flags before running.
	if mode == RoleDest || mode == RoleSource {
		flag.Visit(func(f *flag.Flag) {
			if mode == RoleDest && sourceOnlyFlags[f.Name] {
				fmt.Fprintf(os.Stderr, "Warning: --%s is ignored in dest mode\n", f.Name)
			}
			if mode == RoleSource && destOnlyFlags[f.Name] {
				fmt.Fprintf(os.Stderr, "Warning: --%s is ignored in source mode\n", f.Name)
			}
		})
	}

	if *multifdChannels < 0 {
		fmt.Fprintf(os.Stderr, "Error: --multifd-channels must be non-negative, got %d\n", *multifdChannels)
		os.Exit(1)
	}

	switch mode {
	case RoleDest:
		err = migration.RunDestination(ctx, *qmpSocket, *tapIface, *tapNetns, *driveID, *sharedStorage, *multifdChannels)
	case RoleSource:
		var missing []string
		if *destIP == "" {
			missing = append(missing, "--dest-ip")
		}
		if *vmIP == "" {
			missing = append(missing, "--vm-ip")
		}
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "Error: required flag(s) not set: %s\n", strings.Join(missing, ", "))
			os.Exit(1)
		}
		parsedDest, err1 := netip.ParseAddr(*destIP)
		if err1 != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid --dest-ip %q: %v\n", *destIP, err1)
			os.Exit(1)
		}
		parsedVM, err2 := netip.ParseAddr(*vmIP)
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid --vm-ip %q: %v\n", *vmIP, err2)
			os.Exit(1)
		}
		parsedDest = parsedDest.Unmap()
		parsedVM = parsedVM.Unmap()
		if parsedDest.Is4() != parsedVM.Is4() {
			fmt.Fprintf(os.Stderr, "Error: --dest-ip and --vm-ip address family mismatch (%s vs %s)\n",
				migration.IPFamily(parsedDest), migration.IPFamily(parsedVM))
			os.Exit(1)
		}
		tm := migration.TunnelMode(*tunnelMode)
		if tm != migration.TunnelModeIPIP && tm != migration.TunnelModeGRE && tm != migration.TunnelModeNone {
			fmt.Fprintf(os.Stderr, "Error: invalid --tunnel-mode %q (valid: ipip, gre, none)\n", *tunnelMode)
			os.Exit(1)
		}
		if *downtimeLimit <= 0 {
			fmt.Fprintf(os.Stderr, "Error: --downtime must be positive, got %d\n", *downtimeLimit)
			os.Exit(1)
		}
		err = migration.RunSource(ctx, *qmpSocket, parsedDest, parsedVM, *driveID, *sharedStorage, tm, *downtimeLimit, *autoDowntime, *multifdChannels)
	case "":
		printUsage()
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid mode %q (valid: source, dest)\n", *modeFlag)
		os.Exit(1)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Println("Migration aborted. Cleanup finished.")
			os.Exit(130)
		}
		log.Fatalf("Fatal: %v", err)
	}
}
