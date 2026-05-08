// katamaran-factory is a gRPC server that implements Kata Containers'
// VM cache protocol (CacheService). It watches for completed live
// migrations and serves the migrated QEMU state to Kata shims, letting
// them adopt already-running VMs instead of cold-booting new ones.
//
// The server listens on a Unix socket and polls a configurable
// directory for migration-meta.json files produced by the destination
// QEMU process.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"google.golang.org/grpc"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/factory"
	"github.com/maci0/katamaran/internal/factory/cachepb"
	"github.com/maci0/katamaran/internal/logging"
)

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `katamaran-factory — gRPC VM cache server for Kata Containers live migration

Usage:
  katamaran-factory [flags]
  katamaran-factory --version
  katamaran-factory --help

Flags:
  --listen string        Unix socket path for the gRPC server (default "/var/run/katamaran/factory.sock")
  --watch-dir string     Directory to watch for migration-meta.json files (default "/run/vc/vm/")
  --log-format string    Log output format: 'text' or 'json' (default "json")
  --log-level string     Log level: 'debug', 'info', 'warn', or 'error' (default "info")

Other:
  -v, --version          Show version and exit
  -h, --help             Show this help and exit

Exit codes:
  0   Clean shutdown (signal received or Quit RPC)
  1   Runtime error
  2   Argument or configuration error

Examples:
  # Run with defaults
  katamaran-factory

  # Custom socket and watch directory, text logs
  katamaran-factory --listen /tmp/factory.sock --watch-dir /tmp/vms/ --log-format text
`)
}

func main() {
	fs := flag.NewFlagSet("katamaran-factory", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listen := fs.String("listen", "/var/run/katamaran/factory.sock", "Unix socket path for the gRPC server")
	watchDir := fs.String("watch-dir", "/run/vc/vm/", "Directory to watch for migration-meta.json files")
	logFormat := fs.String("log-format", "json", "Log output format: 'text' or 'json'")
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
		fmt.Fprintf(os.Stdout, "katamaran-factory %s\n", buildinfo.Version)
		return
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %s\n\n", strings.Join(fs.Args(), " "))
		printUsage(os.Stderr)
		os.Exit(2)
	}

	*logFormat = strings.ToLower(*logFormat)
	*logLevel = strings.ToLower(*logLevel)

	if err := logging.SetupLogger(os.Stderr, *logFormat, *logLevel, "katamaran-factory"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	// Ensure the socket directory exists.
	socketDir := filepath.Dir(*listen)
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		fail(fmt.Errorf("creating socket directory %s: %w", socketDir, err))
	}

	// Remove a stale socket file from a previous run.
	if err := os.Remove(*listen); err != nil && !os.IsNotExist(err) {
		fail(fmt.Errorf("removing stale socket %s: %w", *listen, err))
	}

	lis, err := net.Listen("unix", *listen)
	if err != nil {
		fail(fmt.Errorf("listen on %s: %w", *listen, err))
	}

	srv := factory.NewServer()
	loadVMConfig(srv, *watchDir)
	grpcServer := grpc.NewServer()
	cachepb.RegisterCacheServiceServer(grpcServer, srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the directory watcher.
	watcher := factory.NewWatcher(*watchDir, srv)
	go watcher.Run(ctx)

	// Shut down the gRPC server when the context is cancelled or Quit
	// is called via gRPC.
	go func() {
		select {
		case <-ctx.Done():
		case <-srv.QuitCh():
		}
		slog.Info("Shutting down gRPC server")
		grpcServer.GracefulStop()
	}()

	slog.Info("katamaran-factory starting",
		"version", buildinfo.Version,
		"listen", *listen,
		"watch_dir", *watchDir,
	)

	if err := grpcServer.Serve(lis); err != nil {
		fail(fmt.Errorf("gRPC server: %w", err))
	}

	slog.Info("katamaran-factory shut down")
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

// loadVMConfig scans for an existing Kata sandbox persist.json and
// extracts the VMConfig + AgentConfig for the factory's Config() RPC.
// If no sandbox exists yet, the config is loaded from the first
// migration-meta.json that arrives (via OfferVM).
func loadVMConfig(srv *factory.Server, watchDir string) {
	sbsDir := filepath.Join(filepath.Dir(strings.TrimRight(watchDir, "/")), "sbs")
	entries, err := os.ReadDir(sbsDir)
	if err != nil {
		slog.Info("No existing sandboxes for VMConfig extraction", "dir", sbsDir, "error", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		persistPath := filepath.Join(sbsDir, entry.Name(), "persist.json")
		raw, err := os.ReadFile(persistPath)
		if err != nil {
			continue
		}
		var persist struct {
			Config struct {
				HypervisorType   string          `json:"HypervisorType"`
				HypervisorConfig json.RawMessage `json:"HypervisorConfig"`
				KataAgentConfig  json.RawMessage `json:"KataAgentConfig"`
			} `json:"Config"`
		}
		if err := json.Unmarshal(raw, &persist); err != nil {
			continue
		}
		vmCfg, _ := json.Marshal(map[string]any{
			"HypervisorType":   persist.Config.HypervisorType,
			"HypervisorConfig": json.RawMessage(persist.Config.HypervisorConfig),
		})
		srv.SetConfig(vmCfg, persist.Config.KataAgentConfig)
		slog.Info("VMConfig loaded from existing sandbox", "sandbox", entry.Name(), "size", len(vmCfg))
		return
	}
	slog.Info("No sandbox persist.json found; Config() will return empty until first migration")
}
