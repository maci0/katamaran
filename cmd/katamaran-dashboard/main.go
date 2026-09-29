// Command katamaran-dashboard starts the katamaran web dashboard. The
// command stays intentionally thin and delegates flag parsing, HTTP setup,
// and orchestration wiring to internal/dashboard so those paths can be
// exercised by tests.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/maci0/katamaran/internal/dashboard"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // A second signal will now force exit
	}()

	os.Exit(dashboard.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
