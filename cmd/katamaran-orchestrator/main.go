// katamaran-orchestrator is a thin CLI wrapper around the orchestrator
// package. It reads a single JSON-encoded orchestrator.Request from stdin,
// submits the migration, and streams structured StatusUpdate events as
// newline-delimited JSON on stdout. Exit code: 0 on PhaseSucceeded, 1 on
// PhaseFailed, 2 on argument/decoding errors.
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
	"bytes"
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
	"github.com/maci0/katamaran/internal/orchestrator"
)

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `katamaran-orchestrator — Submit a Migration request (JSON on stdin) and stream NDJSON status updates

Usage:
  echo '<json>' | katamaran-orchestrator [flags]
  katamaran-orchestrator --version
  katamaran-orchestrator --help

Flags:
  --native               Use the in-cluster Native orchestrator (client-go) instead of shelling out to migrate.sh
  --script string        Path to deploy/migrate.sh (default "deploy/migrate.sh").
                         Mutually exclusive with --native.
  --kubeconfig string    Path to kubeconfig (only used out-of-cluster; ignored with --native when running inside a pod)

Other:
  -v, --version          Show version and exit
  -h, --help             Show this help and exit

Exit codes:
  0   PhaseSucceeded
  1   PhaseFailed (or runtime error)
  2   Argument or request-decoding error

Example:
  echo '{
    "SourceNode":"worker-a","DestNode":"worker-b","DestIP":"10.0.0.20",
    "Image":"localhost/katamaran:dev",
    "SourcePod":{"Namespace":"default","Name":"kata-demo"},
    "DestPod":{"Namespace":"default","Name":"kata-dest-shell"},
    "SharedStorage":true,"ReplayCmdline":true
  }' | katamaran-orchestrator --native
`)
}

func main() {
	fs := flag.NewFlagSet("katamaran-orchestrator", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scriptPath := fs.String("script", "", `Path to deploy/migrate.sh (default "deploy/migrate.sh")`)
	native := fs.Bool("native", false, "Use the in-cluster Native orchestrator (client-go) instead of shelling out to migrate.sh")
	kubeconfig := fs.String("kubeconfig", "", "Path to kubeconfig (only used out-of-cluster; ignored with --native when running inside a pod)")
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

	// Detect mutually exclusive flags so users do not silently get one mode
	// while believing they configured the other.
	if *native && *scriptPath != "" {
		fmt.Fprintln(os.Stderr, "Error: --native and --script are mutually exclusive")
		os.Exit(2)
	}

	req, err := readRequest(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	if err := orchestrator.Validate(req); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid request: %v\n", err)
		os.Exit(2)
	}

	// Catch SIGINT/SIGTERM so a Ctrl-C cleanly stops the in-flight migration.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var o orchestrator.Orchestrator
	if *native {
		nat, err := orchestrator.New()
		if err != nil {
			nat, err = orchestrator.NewFromKubeconfig(*kubeconfig, "")
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: native orchestrator init: %v\n", err)
			os.Exit(2)
		}
		o = nat
	} else {
		o = orchestrator.NewScript(*scriptPath)
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
		// Render error fields as strings — encoding/json refuses error values.
		out := struct {
			ID    orchestrator.MigrationID `json:"id"`
			Phase orchestrator.StatusPhase `json:"phase"`
			Time  string                   `json:"time"`
			Msg   string                   `json:"msg,omitempty"`
			Err   string                   `json:"err,omitempty"`
		}{ID: u.ID, Phase: u.Phase, Time: u.When.UTC().Format("2006-01-02T15:04:05.000Z"), Msg: u.Message}
		if u.Error != nil {
			out.Err = u.Error.Error()
		}
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "Error: write status update: %v\n", err)
			os.Exit(1)
		}
		if u.Phase == orchestrator.PhaseFailed {
			exit = 1
		}
	}
	os.Exit(exit)
}

func readRequest(r io.Reader) (orchestrator.Request, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return orchestrator.Request{}, fmt.Errorf("read stdin: %w", err)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return orchestrator.Request{}, fmt.Errorf("request JSON is required on stdin")
	}
	var req orchestrator.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return orchestrator.Request{}, fmt.Errorf("decode request JSON: %w", err)
	}
	return req, nil
}
