// katamaran-orchestrator is a thin CLI wrapper around the orchestrator
// package. It reads a single JSON-encoded orchestrator.Request from stdin,
// submits the migration, and streams structured StatusUpdate events as
// newline-delimited JSON on stdout. Exit codes: 0 on PhaseSucceeded, 1 on
// PhaseFailed or runtime error, 2 on argument/decoding errors, 130 on
// signal-induced shutdown.
//
// Intended for scripts and CI pipelines that want a structured (not
// bash-tail) migration runner. The dashboard and the Migration CRD
// reconciler call into the orchestrator package directly rather than
// shelling out to this binary.
//
// Example:
//
//	echo '{
//	  "SourceNode":"worker-a","DestNode":"worker-b","DestIP":"10.0.0.20",
//	  "Image":"localhost/katamaran:dev",
//	  "SourcePod":{"Namespace":"default","Name":"kata-demo"},
//	  "DestPod":{"Namespace":"default","Name":"kata-dest-shell"},
//	  "SharedStorage":true,"ReplayCmdline":true
//	}' | katamaran-orchestrator
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/logging"
	"github.com/maci0/katamaran/internal/orchestrator"
)

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `katamaran-orchestrator — Submit a Migration request (JSON on stdin) and stream NDJSON status updates

Usage:
  echo '<json>' | katamaran-orchestrator [flags]
  katamaran-orchestrator --version
  katamaran-orchestrator --help

I/O:
  stdin    A single JSON-encoded orchestrator.Request object (required; max 1 MiB).
  stdout   Newline-delimited JSON status updates (one object per line) until a
           terminal phase is reached. Fields: id, phase, time, msg, err,
           ram_transferred, ram_total, downtime_ms, applied_downtime_ms,
           rtt_ms, auto_downtime.
  stderr   Diagnostic messages and errors.

Flags:
  --kubeconfig string    Optional path to kubeconfig (out-of-cluster only)
  --log-format string    Log output format: 'text' or 'json' (default "text")
  --log-level string     Log level: 'debug', 'info', 'warn', or 'error' (default "info")

Other:
  -v, --version          Show version and exit
  -h, --help             Show this help and exit

Exit codes:
  0   PhaseSucceeded
  1   PhaseFailed or runtime error
  2   Argument or request-decoding error
  130 Interrupted by signal (SIGINT/SIGTERM)

Example:
  echo '{
    "SourceNode":"worker-a","DestNode":"worker-b","DestIP":"10.0.0.20",
    "Image":"localhost/katamaran:dev",
    "SourcePod":{"Namespace":"default","Name":"kata-demo"},
    "DestPod":{"Namespace":"default","Name":"kata-dest-shell"},
    "SharedStorage":true,"ReplayCmdline":true
  }' | katamaran-orchestrator
`)
}

// isStdinTTY reports whether stdin is connected to a terminal. The CLI is
// designed to take a piped JSON request; if a user runs the binary
// interactively without piping, ReadAll would block forever waiting for
// EOF — this lets us fail fast with a helpful message instead.
func isStdinTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return (st.Mode() & os.ModeCharDevice) != 0
}

func main() {
	fs := flag.NewFlagSet("katamaran-orchestrator", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kubeconfig := fs.String("kubeconfig", "", "Optional path to kubeconfig (out-of-cluster only)")
	logFormat := fs.String("log-format", "text", "Log output format: 'text' or 'json'")
	logLevel := fs.String("log-level", "info", "Log level: 'debug', 'info', 'warn', or 'error'")
	showVersion := fs.Bool("version", false, "Show version and exit")
	showVersionShort := fs.Bool("v", false, "")
	helpFlag := fs.Bool("help", false, "")
	helpFlagShort := fs.Bool("h", false, "")
	fs.Usage = func() { printUsage(os.Stderr) }
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *helpFlag || *helpFlagShort {
		printUsage(os.Stdout)
		return
	}
	if *showVersion || *showVersionShort {
		fmt.Fprintf(os.Stdout, "katamaran-orchestrator %s\n", buildinfo.Version)
		return
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %s\n\n", strings.Join(fs.Args(), " "))
		printUsage(os.Stderr)
		os.Exit(2)
	}

	*logFormat = strings.ToLower(*logFormat)
	*logLevel = strings.ToLower(*logLevel)
	if err := logging.SetupLogger(os.Stderr, *logFormat, *logLevel, "katamaran-orchestrator"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	if isStdinTTY(os.Stdin) {
		fmt.Fprintf(os.Stderr, "Error: stdin is a terminal; pipe a JSON request, e.g. `echo '{...}' | katamaran-orchestrator`\n\n")
		printUsage(os.Stderr)
		os.Exit(2)
	}
	req, err := readRequest(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if err := orchestrator.Validate(req); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid request: %v\n\n", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	// Catch SIGINT/SIGTERM so a Ctrl-C cleanly stops the in-flight migration.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	o, err := orchestrator.New()
	if err != nil {
		o, err = orchestrator.NewFromKubeconfig(*kubeconfig, "")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: orchestrator init: %v\n", err)
		os.Exit(1)
	}
	id, err := o.Apply(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: apply: %v\n", err)
		os.Exit(1)
	}
	updates, err := o.Watch(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: watch: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	go func() {
		<-ctx.Done()
		// Best-effort stop on signal. The watcher will still emit the final
		// PhaseFailed update once the orchestrator finishes tearing down.
		_ = o.Stop(context.Background(), id)
	}()
	exit := 0
	for u := range updates {
		if err := enc.Encode(newStatusOutput(u)); err != nil {
			fmt.Fprintf(os.Stderr, "Error: write status update: %v\n", err)
			os.Exit(1)
		}
		if u.Phase == orchestrator.PhaseFailed {
			exit = 1
		}
	}
	// Signal-induced shutdown surfaces 130 even when the orchestrator
	// emitted a final PhaseFailed update during teardown — otherwise a
	// Ctrl-C looks indistinguishable from a real migration failure.
	if ctx.Err() != nil {
		os.Exit(130)
	}
	os.Exit(exit)
}
