package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const kataRuntimeClass = "kata-qemu"

// ListKataPods runs `kubectl get pods -A -o json` and returns pods whose
// runtimeClassName matches the kata-qemu runtime.
func ListKataPods(ctx context.Context) ([]PodInfo, error) {
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
	cmd := exec.CommandContext(ctx, "kubectl", "get", "nodes", "-l", "katacontainers.io/kata-runtime=true", "-o", "json")
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

// lookupPodNode returns the spec.nodeName of a single pod.
func lookupPodNode(ctx context.Context, ns, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "-n", ns, "get", "pod", name,
		"-o", "jsonpath={.spec.nodeName}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("kubectl: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("pod %s/%s has no nodeName", ns, name)
	}
	return v, nil
}

// lookupNodeInternalIP returns the InternalIP address of the named node.
func lookupNodeInternalIP(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
