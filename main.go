// katamaran orchestrates zero-packet-drop live migration for Kata Containers
// with support for both shared and non-shared (NBD drive-mirror) storage.
//
// It coordinates three sequential migration phases:
//  1. Storage — NBD drive-mirror (skipped in shared-storage mode)
//  2. Compute — RAM pre-copy with auto-converge
//  3. Network — IPIP tunnel + tc sch_plug for zero-drop cutover
//
// Usage:
//
//	katamaran -mode <source|dest> [options]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	mode := flag.String("mode", "", "Migration role: 'source' or 'dest'")
	qmpSocket := flag.String("qmp", "/run/vc/vm/qmp.sock", "Path to QEMU QMP unix socket")
	tapIface := flag.String("tap", "", "Tap interface name (dest mode only, leave empty to skip tc sch_plug)")
	destIP := flag.String("dest-ip", "", "Destination node IP address (source mode only)")
	vmIP := flag.String("vm-ip", "", "VM pod IP for traffic redirection (source mode only)")
	driveID := flag.String("drive-id", "drive-virtio-disk0", "QEMU block device ID to migrate")
	sharedStorage := flag.Bool("shared-storage", false, "Skip NBD drive-mirror (use with shared storage, e.g. Ceph/NFS)")

	flag.Parse()

	// Reject unexpected positional arguments (likely typos or misuse).
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %v\n\n", flag.Args())
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Create a context that is cancelled on SIGINT (Ctrl+C) or SIGTERM.
	// This ensures deferred cleanup routines are executed even if the user
	// aborts the migration manually.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch *mode {
	case "dest":
		err = setupDestination(ctx, *qmpSocket, *tapIface, *driveID, *sharedStorage)
	case "source":
		if *destIP == "" || *vmIP == "" {
			fmt.Fprintln(os.Stderr, "Error: -dest-ip and -vm-ip are required for source mode")
			flag.PrintDefaults()
			os.Exit(1)
		}
		if net.ParseIP(*destIP) == nil {
			fmt.Fprintf(os.Stderr, "Error: invalid -dest-ip %q (must be a valid IP address)\n", *destIP)
			os.Exit(1)
		}
		if net.ParseIP(*vmIP) == nil {
			fmt.Fprintf(os.Stderr, "Error: invalid -vm-ip %q (must be a valid IP address)\n", *vmIP)
			os.Exit(1)
		}
		err = setupSource(ctx, *qmpSocket, *destIP, *vmIP, *driveID, *sharedStorage)
	case "":
		fmt.Fprintf(os.Stderr, "Usage: %s -mode <source|dest> [options]\n\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid mode %q (must be 'source' or 'dest')\n\n", *mode)
		flag.PrintDefaults()
		os.Exit(1)
	}

	if err != nil {
		// If the error was just a context cancellation from our signal handler, don't crash.
		if errors.Is(err, context.Canceled) {
			log.Println("Migration aborted by user signal. Cleanup finished.")
			os.Exit(130) // standard exit code for SIGINT
		}
		log.Fatalf("Fatal: %v", err)
	}
}
