// katamaran-mgr is a minimal Kubernetes controller for the Migration CRD
// (katamaran.io/v1alpha1). It runs in-cluster, polls Migration
// resources, and submits each Pending migration to the embedded orchestrator
// (Native in normal cluster deployments). Status is patched back to the CR.
//
// Active replica is selected via Lease-based leader election so a
// Deployment scaled past 1 stays consistent (only the leader reconciles).
//
// Observability: a small HTTP server exposes /healthz, /readyz, and
// /debug/vars (expvar counters for dispatched / succeeded / failed /
// recovered / deleted / inflight migrations).
//
// Deployment: see config/crd/migration.yaml for the CRD itself, and a
// matching ServiceAccount + ClusterRole + ClusterRoleBinding granting access
// to Migration CRs and status, Jobs, pod/node discovery, pods/log, pods/exec,
// transient stager pods for replayCmdline, and coordination.k8s.io/leases for
// leader election.
package main

import (
	"context"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
  --addr string                   HTTP listen address for /healthz, /readyz, /debug/vars (default ":8081")
  --leader-namespace string       Namespace holding the leader-election Lease (default "kube-system")
  --leader-name string            Lease object name for leader election (default "katamaran-mgr")
  --disable-leader-election       Run reconciler without leader election (single-replica development only)
  --log-format string             Log output format: 'text' or 'json' (default "json")
  --log-level string              Log level: 'debug', 'info', 'warn', or 'error' (default "info")

Other:
  -v, --version                   Show version and exit
  -h, --help                      Show this help and exit

Exit codes:
  0   Clean shutdown (signal received, leader released)
  1   Runtime error (Kubernetes connection lost, reconciler failure)
  2   Argument or configuration error

Examples:
  # Run in-cluster with leader election (default)
  katamaran-mgr

  # Local development against a kubeconfig, no leader election
  katamaran-mgr --kubeconfig ~/.kube/config --disable-leader-election --log-format text
`)
}

func main() {
	fs := flag.NewFlagSet("katamaran-mgr", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kubeconfig := fs.String("kubeconfig", "", "Optional path to kubeconfig (out-of-cluster only)")
	addr := fs.String("addr", ":8081", "HTTP listen address for /healthz, /readyz, /debug/vars")
	leaderNamespace := fs.String("leader-namespace", "kube-system", "Namespace holding the leader-election Lease")
	leaderName := fs.String("leader-name", "katamaran-mgr", "Lease object name for leader election")
	skipLeaderElect := fs.Bool("disable-leader-election", false, "Run reconciler without leader election (single-replica development only)")
	showVersion := fs.Bool("version", false, "Show version and exit")
	showVersionShort := fs.Bool("v", false, "")
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

	var orch orchestrator.Orchestrator
	nat, err := orchestrator.New()
	if err != nil {
		nat, err = orchestrator.NewFromKubeconfig(*kubeconfig, "")
	}
	if err != nil {
		// Fall back to NewScript if not running in-cluster — mostly useful
		// during local development.
		slog.Warn("Orchestrator unavailable, falling back to Script", "error", err)
		orch = orchestrator.NewScript("")
	} else {
		orch = nat
	}

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

// serveDebug exposes /healthz, /readyz, and /debug/vars (expvar). Failure
// to listen is fatal — the probes are how Kubernetes knows we're alive.
func serveDebug(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	plainOK := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body + "\n"))
		}
	}
	mux.HandleFunc("GET /healthz", plainOK("ok"))
	mux.HandleFunc("GET /readyz", plainOK("ready"))
	mux.Handle("GET /debug/vars", expvar.Handler())
	mux.HandleFunc("GET /metrics", servePrometheusMetrics)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail(fmt.Errorf("debug server: %w", err))
	}
}

// servePrometheusMetrics writes the controller's expvar counters in
// Prometheus text-exposition format. We translate in-process instead of
// pulling in the prometheus/client_golang dependency: every controller
// metric is a plain int counter or gauge, the volume is fixed at compile
// time, and Prometheus scrapers happily ingest the text format from any
// HTTP endpoint.
//
// The metric names already follow Prometheus conventions
// (`_total` suffix on counters), so the translation is mechanical:
// emit one HELP + TYPE + value triple per int var.
func servePrometheusMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	type metricSpec struct {
		help, kind string
	}
	specs := map[string]metricSpec{
		"katamaran_migrations_dispatched_total":       {"Migrations the controller has dispatched (Apply succeeded).", "counter"},
		"katamaran_migrations_succeeded_total":        {"Migrations that reached PhaseSucceeded.", "counter"},
		"katamaran_migrations_failed_total":           {"Migrations that reached PhaseFailed.", "counter"},
		"katamaran_migrations_recovered_total":        {"Migrations the controller resumed observing after a restart.", "counter"},
		"katamaran_migrations_deleted_total":          {"Migration CRs the controller cleaned up via finalizer.", "counter"},
		"katamaran_migrations_inflight":               {"Migrations currently in a non-terminal phase.", "gauge"},
		"katamaran_migrations_reconcile_errors_total": {"Reconcile loop errors observed since startup.", "counter"},
		"katamaran_migrations_watch_lost_total":       {"Watch channels that closed before reaching a terminal phase.", "counter"},
	}
	expvar.Do(func(kv expvar.KeyValue) {
		spec, ok := specs[kv.Key]
		if !ok {
			return // skip non-katamaran expvars (cmdline, memstats, ...)
		}
		raw := kv.Value.String() // expvar.Int.String() is the decimal value
		fmt.Fprintf(w, "# HELP %s %s\n", kv.Key, spec.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", kv.Key, spec.kind)
		fmt.Fprintf(w, "%s %s\n", kv.Key, raw)
	})
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func validListenAddr(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return false
	}
	_, err = net.LookupPort("tcp", port)
	return err == nil
}
