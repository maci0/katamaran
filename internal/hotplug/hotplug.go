// Package hotplug attaches a data disk to a running QEMU over its QMP socket.
//
// It backs the e2e disk-migration path, which needs a data disk to appear in
// the source VM after the VM is already booted. The device is pinned to
// bus=pci-bridge-0,addr=0x8 so the source and destination keep matching PCI
// topology, which live migration requires.
package hotplug

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/maci0/katamaran/internal/qmp"
)

// PCI placement for the hot-plugged disk. Fixed rather than configurable:
// a different address on the destination would make the migrated VM's PCI
// topology differ from the source, and migration fails on the mismatch.
const (
	deviceBus  = "pci-bridge-0"
	deviceAddr = "0x8"
)

// QEMU object and device identifiers for the hot-plugged disk.
const (
	diskDriver   = "virtio-blk-pci"
	diskNodeName = "drive-virtio-disk0"
	diskID       = "data-disk0"
	diskFormat   = "raw"
	fileDriver   = "file"
)

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: qmp-hotplug-disk <qmp-socket> <disk-image>

Attaches a virtio-blk data disk to a running QEMU over its QMP socket, so a
booted VM gains a disk without a restart. The disk is pinned to
bus=pci-bridge-0,addr=0x8 to keep PCI topology identical across a live
migration.

Positional arguments:
  <qmp-socket>   Path to the QEMU QMP unix socket
  <disk-image>   Path to the backing image to attach

Options:
  -h, --help     Show this help and exit
`)
}

// Run contains all CLI logic: flag parsing, validation, and the QMP
// conversation. Extracted from main() so the argument handling is testable
// without os.Exit.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("qmp-hotplug-disk", flag.ContinueOnError)
	fs.SetOutput(stderr)

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

	if fs.NArg() != 2 {
		fmt.Fprintf(stderr, "Error: expected 2 arguments, got %d\n\n", fs.NArg())
		printUsage(stderr)
		return 2
	}

	if err := attach(ctx, fs.Arg(0), fs.Arg(1)); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// attach adds the block backend and then the frontend device. The two must
// land in this order: device_add references the node-name that blockdev-add
// defines, and QEMU rejects a device whose drive does not exist yet.
func attach(ctx context.Context, socketPath, diskImage string) error {
	client, err := qmp.NewClient(ctx, socketPath)
	if err != nil {
		return err
	}
	defer client.Close()

	if _, err = client.Execute(ctx, "blockdev-add", qmp.BlockdevAddArgs{
		Driver:   diskFormat,
		NodeName: diskNodeName,
		File: qmp.BlockdevFileSpec{
			Driver:   fileDriver,
			Filename: diskImage,
		},
	}); err != nil {
		return fmt.Errorf("blockdev-add: %w", err)
	}

	if _, err = client.Execute(ctx, "device_add", qmp.DeviceAddArgs{
		Driver: diskDriver,
		Drive:  diskNodeName,
		ID:     diskID,
		Bus:    deviceBus,
		Addr:   deviceAddr,
	}); err != nil {
		return fmt.Errorf("device_add: %w", err)
	}

	return nil
}
