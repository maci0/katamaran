// katamaran-mgr is a minimal Kubernetes controller for the Migration CRD
// (kata.katamaran.io/v1alpha1). It runs in-cluster, polls Migration
// resources, and submits each Pending migration to the embedded Native
// orchestrator. Status is patched back to the CR.
//
// Deployment: see config/crd/migration.yaml for the CRD itself, and a
// matching ServiceAccount + ClusterRole + ClusterRoleBinding granting
// `migrations.kata.katamaran.io` get/list/watch/patch and `jobs` create/get/list/delete.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/maci0/katamaran/internal/buildinfo"
	"github.com/maci0/katamaran/internal/controller"
	"github.com/maci0/katamaran/internal/orchestrator"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "Optional path to kubeconfig (out-of-cluster only)")
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

	rec := controller.NewReconciler(dyn, orch)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("katamaran-mgr starting", "version", buildinfo.Version, "poll_interval", rec.PollInterval)
	if err := rec.Run(ctx); err != nil && err != context.Canceled {
		fail(err)
	}
	slog.Info("katamaran-mgr shutting down")
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
