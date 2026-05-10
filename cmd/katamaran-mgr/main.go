// katamaran-mgr is a minimal Kubernetes controller for the Migration CRD
// (katamaran.io/v1alpha1). It runs in-cluster, polls Migration
// resources, and submits each Pending migration to the embedded orchestrator
// (Native in normal cluster deployments). Status is patched back to the CR.
//
// Active replica is selected via Lease-based leader election so a
// Deployment scaled past 1 stays consistent (only the leader reconciles).
//
// Observability: a small HTTP server exposes /healthz, /readyz,
// /metrics, and /debug/vars for controller counters and per-migration
// progress gauges.
//
// Deployment: see config/crd/migration.yaml for the CRD itself, and a
// matching ServiceAccount + ClusterRole + ClusterRoleBinding granting access
// to Migration CRs and status, Jobs, pod/node discovery, pods/log, and
// coordination.k8s.io/leases for leader election.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/controller"
	"github.com/maci0/katamaran/internal/logging"
	"github.com/maci0/katamaran/internal/orchestrator"
)

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `katamaran-mgr — Kubernetes controller for the Migration CRD (katamaran.io/v1alpha1)

Usage:
  katamaran-mgr [flags]
  katamaran-mgr --version
  katamaran-mgr --help

Flags:
  --kubeconfig string             Optional path to kubeconfig (out-of-cluster only)
  --addr string                   HTTP listen address for /healthz, /readyz, /metrics, /debug/vars (default ":8081")
  --leader-namespace string       Namespace holding the leader-election Lease (default "kube-system")
  --leader-name string            Lease object name for leader election (default "katamaran-mgr")
  --disable-leader-election       Run reconciler without leader election (single-replica development only)
  --pod-wait-timeout duration     How long to wait for migration Job pods to appear (default 60s;
                                  overridden by KATAMARAN_POD_WAIT_TIMEOUT env or per-CR spec.podWaitTimeoutSeconds)
  --log-format string             Log output format: 'text' or 'json' (default "json")
  --log-level string              Log level: 'debug', 'info', 'warn', or 'error' (default "info")

Other:
  -v, --version                   Show version and exit
  -h, --help                      Show this help and exit

Exit codes:
  0   Clean shutdown (signal received, leader released)
  1   Runtime error (Kubernetes connection lost, reconciler failure)
  2   Argument or configuration error

Environment variables:
  KATAMARAN_POD_WAIT_TIMEOUT   Override --pod-wait-timeout (Go duration; per-CR spec.podWaitTimeoutSeconds wins over both)

Examples:
  # Run in-cluster with leader election (default)
  katamaran-mgr

  # Local development against a kubeconfig, no leader election
  katamaran-mgr --kubeconfig ~/.kube/config --disable-leader-election --log-format text

  # Custom probe/metrics listen address
  katamaran-mgr --addr 0.0.0.0:9091
`)
}

func main() {
	fs := flag.NewFlagSet("katamaran-mgr", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kubeconfig := fs.String("kubeconfig", "", "Optional path to kubeconfig (out-of-cluster only)")
	addr := fs.String("addr", ":8081", "HTTP listen address for /healthz, /readyz, /metrics, /debug/vars")
	leaderNamespace := fs.String("leader-namespace", "kube-system", "Namespace holding the leader-election Lease")
	leaderName := fs.String("leader-name", "katamaran-mgr", "Lease object name for leader election")
	skipLeaderElect := fs.Bool("disable-leader-election", false, "Run reconciler without leader election (single-replica development only)")
	showVersion := fs.Bool("version", false, "Show version and exit")
	showVersionShort := fs.Bool("v", false, "")
	podWaitTimeout := fs.Duration("pod-wait-timeout", 60*time.Second, "How long to wait for migration Job pods to appear")
	logFormat := fs.String("log-format", "json", "Log output format: 'text' or 'json'")
	logLevel := fs.String("log-level", "info", "Log level: 'debug', 'info', 'warn', or 'error'")
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
		fmt.Fprintf(os.Stdout, "katamaran-mgr %s\n", buildinfo.Version)
		return
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %s\n\n", strings.Join(fs.Args(), " "))
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if !validListenAddr(*addr) {
		fmt.Fprintf(os.Stderr, "Error: invalid --addr %q (expected host:port, for example :8081 or 0.0.0.0:8081)\n\n", *addr)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if *podWaitTimeout <= 0 {
		fmt.Fprintf(os.Stderr, "Error: --pod-wait-timeout must be greater than 0, got %s\n\n", *podWaitTimeout)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if !*skipLeaderElect {
		if strings.TrimSpace(*leaderNamespace) == "" {
			fmt.Fprintf(os.Stderr, "Error: --leader-namespace must not be empty\n\n")
			printUsage(os.Stderr)
			os.Exit(2)
		}
		if strings.TrimSpace(*leaderName) == "" {
			fmt.Fprintf(os.Stderr, "Error: --leader-name must not be empty\n\n")
			printUsage(os.Stderr)
			os.Exit(2)
		}
	}

	// Normalize enum flags for case-insensitive matching.
	*logFormat = strings.ToLower(*logFormat)
	*logLevel = strings.ToLower(*logLevel)

	if err := logging.SetupLogger(os.Stderr, *logFormat, *logLevel, "katamaran-mgr"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	cfg, err := loadConfig(*kubeconfig)
	if err != nil {
		fail(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fail(fmt.Errorf("dynamic client: %w", err))
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fail(fmt.Errorf("kubernetes client: %w", err))
	}

	// Env var overrides the flag default; per-CR spec overrides both.
	if envPWT := os.Getenv("KATAMARAN_POD_WAIT_TIMEOUT"); envPWT != "" {
		if d, err := time.ParseDuration(envPWT); err == nil && d > 0 {
			*podWaitTimeout = d
		} else {
			slog.Warn("Ignoring invalid KATAMARAN_POD_WAIT_TIMEOUT", "value", envPWT, "error", err)
		}
	}

	orch, err := orchestrator.New()
	if err != nil {
		orch, err = orchestrator.NewFromKubeconfig(*kubeconfig, "")
	}
	if err != nil {
		fail(fmt.Errorf("orchestrator unavailable: %w", err))
	}
	orchestrator.SetPodWaitTimeout(orch, *podWaitTimeout)

	disc, derr := orchestrator.NewDiscoverer()
	if derr != nil {
		disc, derr = orchestrator.NewDiscovererFromKubeconfig(*kubeconfig, "")
	}
	if derr != nil {
		slog.Warn("Discoverer unavailable, controller will not resolve SourceNode/DestIP", "error", derr)
	}

	rec := controller.NewReconciler(dyn, kube, orch, disc)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // A second signal will now force exit.
	}()

	go serveDebug(ctx, *addr)

	slog.Info("katamaran-mgr starting", "version", buildinfo.Version, "poll_interval", rec.PollInterval, "addr", *addr, "leader_election", !*skipLeaderElect)

	if *skipLeaderElect {
		runReconciler(ctx, rec)
		return
	}

	identity, _ := os.Hostname()
	if identity == "" {
		identity = "katamaran-mgr"
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      *leaderName,
			Namespace: *leaderNamespace,
		},
		Client: kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				slog.Info("Acquired leader lease", "identity", identity, "lease", *leaderNamespace+"/"+*leaderName)
				runReconciler(leaderCtx, rec)
			},
			OnStoppedLeading: func() {
				slog.Info("Lost leader lease, exiting")
			},
			OnNewLeader: func(id string) {
				if id != identity {
					slog.Info("Observed leader", "identity", id)
				}
			},
		},
	})
	slog.Info("katamaran-mgr shutting down")
}

func runReconciler(ctx context.Context, rec *controller.Reconciler) {
	if err := rec.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
