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

// defaultDiscoverer is the lazily-initialised process-wide
// NativeDiscoverer. In-cluster service-account creds are tried first;
// a kubeconfig fallback covers developer laptops. There is no kubectl
// shell-out fallback — the dashboard image no longer ships kubectl.
var (
	defaultDiscoverer     orchestrator.Discoverer
	defaultDiscovererOnce sync.Once
)

func discoverer() orchestrator.Discoverer {
	defaultDiscovererOnce.Do(func() {
		if d, err := orchestrator.NewDiscoverer(); err == nil {
			slog.Info("Discovery: using NativeDiscoverer (client-go)")
			defaultDiscoverer = d
			return
		}
		if os.Getenv("KUBECONFIG") != "" {
			if d, err := orchestrator.NewDiscovererFromKubeconfig("", ""); err == nil {
				slog.Info("Discovery: using NativeDiscoverer (kubeconfig)")
				defaultDiscoverer = d
				return
			}
		}
		slog.Warn("Discovery: no Kubernetes API reachable; pod/node lookups will fail until in-cluster config or KUBECONFIG is available")
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
