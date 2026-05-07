package orchestrator

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// PodScheduling carries the scheduling constraints copied from a source pod
// so that the destination Job can inherit them when destNode is omitted
// (automatic destination node selection).
type PodScheduling struct {
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
}

// PodInfo is the projection of a Kubernetes pod that the orchestrator and
// dashboard care about: identity, scheduling node, and pod IP.
type PodInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node"`
	PodIP     string `json:"pod_ip"`
}

// NodeInfo is the projection of a Kubernetes node: name + InternalIP.
type NodeInfo struct {
	Name       string `json:"name"`
	InternalIP string `json:"internal_ip"`
}

// Discoverer enumerates kata-qemu pods and kata-runtime nodes from a
// Kubernetes cluster. It backs the dashboard's pod-picker dropdowns and
// the Migration CRD controller's source-pod / dest-node resolution.
//
// The only implementation lives in discovery_native.go and uses client-go
// directly against the apiserver — a previous kubectl-shell-out variant
// has been removed now that all production images run with in-cluster
// service-account credentials (or a kubeconfig fallback for local
// development). Construct via NewDiscoverer / NewDiscovererFromKubeconfig
// / NewDiscovererFromClient.
type Discoverer interface {
	// ListKataPods returns all pods in the cluster whose runtimeClassName is
	// kata-qemu (across all namespaces).
	ListKataPods(ctx context.Context) ([]PodInfo, error)

	// ListKataNodes returns all nodes labeled katacontainers.io/kata-runtime=true.
	ListKataNodes(ctx context.Context) ([]NodeInfo, error)

	// LookupPodNode returns spec.nodeName for the named pod. Returns an error
	// if the pod has no nodeName (e.g. still Pending).
	LookupPodNode(ctx context.Context, namespace, name string) (string, error)

	// LookupNodeInternalIP returns the InternalIP address for the named node.
	LookupNodeInternalIP(ctx context.Context, name string) (string, error)

	// LookupPodScheduling returns the nodeSelector and tolerations from the
	// named pod's spec. Used to copy scheduling constraints from the source
	// pod to the destination Job when destNode is omitted.
	LookupPodScheduling(ctx context.Context, namespace, name string) (PodScheduling, error)
}

// KataRuntimeClassName is the kata runtime class used to filter discoverable
// pods. Aligns with what kata-deploy installs as the default.
const KataRuntimeClassName = "kata-qemu"

// KataNodeLabel is the label kata-deploy applies to nodes after a successful
// install. Used to filter nodes the migrator can target.
const KataNodeLabel = "katacontainers.io/kata-runtime=true"
