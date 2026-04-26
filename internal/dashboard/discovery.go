package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

const kataRuntimeClass = "kata-qemu"

// ListKataPods runs `kubectl get pods -A -o json` and returns pods whose
// runtimeClassName matches the kata-qemu runtime.
func ListKataPods(ctx context.Context) ([]PodInfo, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "get", "pods", "-A", "-o", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl get pods: %w", err)
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
		if it.Spec.RuntimeClassName != kataRuntimeClass {
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

// ListKataNodes runs `kubectl get nodes -o json` and returns nodes labeled
// katacontainers.io/kata-runtime=true with their InternalIP.
func ListKataNodes(ctx context.Context) ([]NodeInfo, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "get", "nodes", "-l", "katacontainers.io/kata-runtime=true", "-o", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl get nodes: %w", err)
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
