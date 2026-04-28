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
	_ "expvar"
	"flag"
	"fmt"
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
	"github.com/maci0/katamaran/internal/orchestrator"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "Optional path to kubeconfig (out-of-cluster only)")
	addr := flag.String("addr", ":8081", "HTTP listen address for /healthz, /readyz, /debug/vars")
	leaderNamespace := flag.String("leader-namespace", "kube-system", "Namespace holding the leader-election Lease")
	leaderName := flag.String("leader-name", "katamaran-mgr", "Lease object name for leader election")
	skipLeaderElect := flag.Bool("disable-leader-election", false, "Run reconciler without leader election (single-replica development only)")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("katamaran-mgr", buildinfo.Version)
		return
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
	nat, err := orchestrator.NewNative()
	if err != nil {
		// Fall back to NewScript if not running in-cluster — mostly useful
		// during local development.
		slog.Warn("Native orchestrator unavailable, falling back to Script", "error", err)
		orch = orchestrator.NewScript("")
	} else {
		orch = nat
	}

	disc, derr := orchestrator.NewNativeDiscoverer()
	if derr != nil {
		slog.Warn("NativeDiscoverer unavailable, controller will not resolve SourceNode/DestIP", "error", derr)
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.Handle("/debug/vars", http.DefaultServeMux) // expvar default handler
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
