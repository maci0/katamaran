// Command katamaran runs the node-local source and destination migration CLI.
// The command stays intentionally thin and delegates CLI behavior to
// internal/katamaran so validation and dispatch remain testable.
package main

import (
	"os"

	"github.com/maci0/katamaran/internal/katamaran"
	"github.com/maci0/katamaran/internal/signalctx"
)

func main() {
	ctx, stop := signalctx.Notify()
	defer stop()
	os.Exit(katamaran.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
