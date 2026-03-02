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
	"syscall"

	"github.com/maci0/katamaran/internal/migration"
)

var version = "v0.3.0"

func main() {
	mode := flag.String("mode", "", "Migration role: 'source' or 'dest'")
	qmpSocket := flag.String("qmp", "/run/vc/vm/extra-monitor.sock", "Path to QEMU QMP unix socket")
	tapIface := flag.String("tap", "", "Tap interface name (dest mode only, leave empty to skip tc sch_plug)")
	tapNetns := flag.String("tap-netns", "", "Network namespace path for tap interface (e.g. /proc/PID/ns/net)")
	destIP := flag.String("dest-ip", "", "Destination node IP address (source mode only)")
	vmIP := flag.String("vm-ip", "", "VM pod IP for traffic redirection (source mode only)")
	driveID := flag.String("drive-id", "drive-virtio-disk0", "QEMU block device ID to migrate")
	sharedStorage := flag.Bool("shared-storage", false, "Skip NBD drive-mirror (use with shared storage)")
	tunnelMode := flag.String("tunnel-mode", "ipip", "Tunnel mode: 'ipip', 'gre', or 'none'")
	downtimeLimit := flag.Int("downtime", 25, "Max allowed downtime (ms)")
	showVersion := flag.Bool("version", false, "Show version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Printf("katamaran %s\n", version)
		os.Exit(0)
	}

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %v\n\n", flag.Args())
		flag.PrintDefaults()
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // A second signal will now force exit
	}()

	var err error
	switch *mode {
	case "dest":
		err = migration.RunDestination(ctx, *qmpSocket, *tapIface, *tapNetns, *driveID, *sharedStorage)
	case "source":
		if *destIP == "" || *vmIP == "" {
			fmt.Fprintln(os.Stderr, "Error: -dest-ip and -vm-ip are required for source mode")
			flag.PrintDefaults()
			os.Exit(1)
		}
		parsedDest, err1 := netip.ParseAddr(*destIP)
		if err1 != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid -dest-ip: %v\n", err1)
			os.Exit(1)
		}
		parsedVM, err2 := netip.ParseAddr(*vmIP)
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid -vm-ip: %v\n", err2)
			os.Exit(1)
		}
		parsedDest = parsedDest.Unmap()
		parsedVM = parsedVM.Unmap()
		if parsedDest.Is4() != parsedVM.Is4() {
			fmt.Fprintf(os.Stderr, "Error: address family mismatch (%s vs %s)\n",
				migration.IPFamily(parsedDest), migration.IPFamily(parsedVM))
			os.Exit(1)
		}
		if *tunnelMode != "ipip" && *tunnelMode != "gre" && *tunnelMode != "none" {
			fmt.Fprintf(os.Stderr, "Error: invalid -tunnel-mode: %q\n", *tunnelMode)
			os.Exit(1)
		}
		if *downtimeLimit <= 0 {
			fmt.Fprintf(os.Stderr, "Error: -downtime must be positive: %d\n", *downtimeLimit)
			os.Exit(1)
		}
		err = migration.RunSource(ctx, *qmpSocket, parsedDest, parsedVM, *driveID, *sharedStorage, *tunnelMode, *downtimeLimit)
	case "":
		fmt.Fprintf(os.Stderr, "Usage: %s -mode <source|dest> [options]\n\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid mode %q\n", *mode)
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
