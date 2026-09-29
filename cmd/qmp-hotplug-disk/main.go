// qmp-hotplug-disk is the end-to-end test tool that hot-plugs a disk into a
// running QEMU VM over its QMP socket. It is built by scripts/e2e.sh and
// copied to the test node, so the node needs no Python or QEMU tooling.
//
// The command delegates its QMP protocol work to internal/hotplug.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/maci0/katamaran/internal/hotplug"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // A second signal will now force exit
	}()

	os.Exit(hotplug.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
