package dashboard

import (
	"context"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// PodInfo and NodeInfo are re-exported here so existing dashboard call sites
// don't need to import the orchestrator package directly. The single source
// of truth for the discovery wire format is the orchestrator package.
type (
	PodInfo  = orchestrator.PodInfo
	NodeInfo = orchestrator.NodeInfo
)

// defaultDiscoverer is the kubectl-shell-out implementation used by the
// dashboard image (which already ships a kubectl binary).
var defaultDiscoverer orchestrator.Discoverer = orchestrator.NewKubectlDiscoverer()

// ListKataPods lists kata-qemu pods cluster-wide via the default discoverer.
func ListKataPods(ctx context.Context) ([]PodInfo, error) {
	return defaultDiscoverer.ListKataPods(ctx)
}

// ListKataNodes lists kata-runtime-labeled nodes via the default discoverer.
func ListKataNodes(ctx context.Context) ([]NodeInfo, error) {
	return defaultDiscoverer.ListKataNodes(ctx)
}

func lookupPodNode(ctx context.Context, namespace, name string) (string, error) {
	return defaultDiscoverer.LookupPodNode(ctx, namespace, name)
}

func lookupNodeInternalIP(ctx context.Context, name string) (string, error) {
	return defaultDiscoverer.LookupNodeInternalIP(ctx, name)
}
