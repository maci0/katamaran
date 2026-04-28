package dashboard

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// PodInfo and NodeInfo are re-exported here so existing dashboard call sites
// don't need to import the orchestrator package directly. The single source
// of truth for the discovery wire format is the orchestrator package.
type (
	PodInfo  = orchestrator.PodInfo
	NodeInfo = orchestrator.NodeInfo
)

// defaultDiscoverer prefers the Native (client-go) discoverer when running
// in-cluster (no kubectl exec, no JSON-of-stdout parsing), and falls back
// to the kubectl shell-out for out-of-cluster development. The choice is
// made lazily on first use so unit tests that stub kubectl on PATH still
// work without an in-cluster Kubernetes API.
var (
	defaultDiscoverer     orchestrator.Discoverer
	defaultDiscovererOnce sync.Once
)

func discoverer() orchestrator.Discoverer {
	defaultDiscovererOnce.Do(func() {
		if d, err := orchestrator.NewNativeDiscoverer(); err == nil {
			slog.Info("Discovery: using NativeDiscoverer (client-go)")
			defaultDiscoverer = d
			return
		}
		if os.Getenv("KUBECONFIG") != "" {
			if d, err := orchestrator.NewNativeDiscovererFromKubeconfig("", ""); err == nil {
				slog.Info("Discovery: using NativeDiscoverer (kubeconfig)")
				defaultDiscoverer = d
				return
			}
		}
		slog.Info("Discovery: using KubectlDiscoverer (kubectl shell-out)")
		defaultDiscoverer = orchestrator.NewKubectlDiscoverer()
	})
	return defaultDiscoverer
}

func (a *App) discovery() orchestrator.Discoverer {
	if a.discoverer != nil {
		return a.discoverer
	}
	return discoverer()
}

// ListKataPods lists kata-qemu pods cluster-wide via the default discoverer.
func ListKataPods(ctx context.Context) ([]PodInfo, error) {
	return discoverer().ListKataPods(ctx)
}

// ListKataNodes lists kata-runtime-labeled nodes via the default discoverer.
func ListKataNodes(ctx context.Context) ([]NodeInfo, error) {
	return discoverer().ListKataNodes(ctx)
}
