// Command katamaran-dashboard starts the katamaran web dashboard. The
// command stays intentionally thin and delegates flag parsing, HTTP setup,
// and orchestration wiring to internal/dashboard so those paths can be
// exercised by tests.
package main

import (
	"os"

	"github.com/maci0/katamaran/internal/dashboard"
	"github.com/maci0/katamaran/internal/signalctx"
)

func main() {
	ctx, stop := signalctx.Notify()
	defer stop()
	os.Exit(dashboard.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
