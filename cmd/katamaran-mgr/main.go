// katamaran-mgr is a minimal Kubernetes controller for the Migration CRD
// (katamaran.io/v1alpha1). It runs in-cluster, polls Migration
// resources, and submits each Pending migration to the embedded Native
// orchestrator. Status is patched back to the CR.
//
// Active replica is selected via Lease-based leader election so a
// Deployment scaled past 1 stays consistent (only the leader reconciles).
//
// Observability: a small HTTP server exposes /healthz, /readyz, and
// /debug/vars (expvar counters for dispatched / succeeded / failed /
// recovered / deleted / inflight migrations).
//
// Deployment: see config/crd/migration.yaml for the CRD itself, and a
// matching ServiceAccount + ClusterRole + ClusterRoleBinding granting
// `migrations.katamaran.io` get/list/watch/patch, `jobs` create/get/list/delete,
// `pods/exec` create, `pods/log` get, and coordination.k8s.io/leases
// get/list/create/update for leader election.
package main

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
		fmt.Println("katamaran-mgr", buildinfo.Version)
		return
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %s\n", fs.Arg(0))
		printUsage(os.Stderr)
		os.Exit(2)
	}

	if err := logging.SetupLogger(os.Stderr, *logFormat, *logLevel, "katamaran-mgr"); err != nil {
		fail(err)
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
	if err := rec.Run(ctx); err != nil && err != context.Canceled {
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
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fail(fmt.Errorf("debug server: %w", err))
	}
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
	fmt.Fprintf(os.Stderr, "katamaran-mgr: %v\n", err)
	os.Exit(1)
}
