// katamaran-orchestrator is a thin CLI wrapper around the orchestrator
// package. It reads a single JSON-encoded orchestrator.Request from stdin,
// submits the migration, and streams structured StatusUpdate events as
// newline-delimited JSON on stdout. Exit code: 0 on PhaseSucceeded, 1 on
// PhaseFailed, 2 on argument/decoding errors.
//
// Intended for two consumers:
//
//   - Scripts and CI pipelines that want a structured (not bash-tail)
//     migration runner.
//   - A future Migration CRD reconciler that wants to invoke the same
//     orchestration code path the dashboard uses.
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
	"syscall"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/orchestrator"
)

func main() {
	scriptPath := flag.String("script", "", "Path to deploy/migrate.sh (default: search ./deploy/migrate.sh and /usr/local/bin/migrate.sh)")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("katamaran-orchestrator", buildinfo.Version)
		return
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
		os.Exit(2)
	}
	var req orchestrator.Request
	if err := json.Unmarshal(body, &req); err != nil {
		fmt.Fprintf(os.Stderr, "decode request JSON: %v\n", err)
		os.Exit(2)
	}
	if err := orchestrator.Validate(req); err != nil {
		fmt.Fprintf(os.Stderr, "invalid request: %v\n", err)
		os.Exit(2)
	}

	// Catch SIGINT/SIGTERM so a Ctrl-C cleanly stops the in-flight migration.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	o := orchestrator.NewScript(*scriptPath)
	id, err := o.Apply(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apply: %v\n", err)
		os.Exit(1)
	}
	updates, err := o.Watch(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	go func() {
		<-ctx.Done()
		// Best-effort stop on signal. The watcher will still emit the final
		// PhaseFailed update once migrate.sh exits.
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
		_ = enc.Encode(out)
		if u.Phase == orchestrator.PhaseFailed {
			exit = 1
		}
	}
	os.Exit(exit)
}
