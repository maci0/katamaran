package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

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
// Kubernetes cluster. It backs the dashboard's pod-picker dropdowns and a
// future operator's resource-watching reconciler.
//
// Two implementations exist:
//
//   - Kubectl (KubectlDiscoverer): shells out to `kubectl`. Used by the
//     dashboard image which already ships kubectl. Suited to ad-hoc clients.
//
//   - Native (TODO): client-go informers backed by an in-cluster client.
//     The operator implementation will use this so it does not need kubectl
//     in its image.
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
}

// KataRuntimeClassName is the kata runtime class used to filter discoverable
// pods. Aligns with what kata-deploy installs as the default.
const KataRuntimeClassName = "kata-qemu"

// KataNodeLabel is the label kata-deploy applies to nodes after a successful
// install. Used to filter nodes the migrator can target.
const KataNodeLabel = "katacontainers.io/kata-runtime=true"

// kubectlLookupTimeout caps each individual lookup call (LookupPodNode,
// LookupNodeInternalIP) so a wedged apiserver does not hang the caller.
const kubectlLookupTimeout = 10 * time.Second

// KubectlDiscoverer implements Discoverer by shelling out to the `kubectl`
// binary on PATH. The dashboard image already includes kubectl; the operator
// should switch to the (future) Native client-go implementation.
type KubectlDiscoverer struct{}

// NewKubectlDiscoverer returns a fresh discoverer using the kubectl on PATH.
func NewKubectlDiscoverer() *KubectlDiscoverer { return &KubectlDiscoverer{} }

func (KubectlDiscoverer) ListKataPods(ctx context.Context) ([]PodInfo, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "pods", "-A", "-o", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl get pods: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var raw struct {
		Items []struct {
			Metadata struct{ Namespace, Name string }
			Spec     struct{ RuntimeClassName, NodeName string }
			Status   struct{ PodIP string }
		}
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse pod list: %w", err)
	}
	pods := make([]PodInfo, 0, len(raw.Items))
	for _, it := range raw.Items {
		if it.Spec.RuntimeClassName != KataRuntimeClassName {
			continue
		}
		pods = append(pods, PodInfo{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Node:      it.Spec.NodeName,
			PodIP:     it.Status.PodIP,
		})
	}
	return pods, nil
}

func (KubectlDiscoverer) ListKataNodes(ctx context.Context) ([]NodeInfo, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "nodes", "-l", KataNodeLabel, "-o", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl get nodes: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var raw struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct {
				Addresses []struct{ Type, Address string }
			}
		}
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse node list: %w", err)
	}
	nodes := make([]NodeInfo, 0, len(raw.Items))
	for _, it := range raw.Items {
		ip := ""
		for _, a := range it.Status.Addresses {
			if a.Type == "InternalIP" {
				ip = a.Address
				break
			}
		}
		nodes = append(nodes, NodeInfo{Name: it.Metadata.Name, InternalIP: ip})
	}
	return nodes, nil
}

func (KubectlDiscoverer) LookupPodNode(ctx context.Context, namespace, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, kubectlLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "pod", name,
		"-o", "jsonpath={.spec.nodeName}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("kubectl: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("pod %s/%s has no nodeName", namespace, name)
	}
	return v, nil
}

func (KubectlDiscoverer) LookupNodeInternalIP(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, kubectlLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "get", "node", name,
		"-o", "jsonpath={.status.addresses[?(@.type==\"InternalIP\")].address}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("kubectl: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("node %s has no InternalIP", name)
	}
	return v, nil
}
