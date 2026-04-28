package orchestrator

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// nativeDiscoverer is the only Discoverer implementation. Constructors
// return the Discoverer interface to keep the concrete type private.
type nativeDiscoverer struct {
	client kubernetes.Interface
}

// NewDiscoverer constructs a Discoverer using the in-cluster service
// account credentials (works inside any pod with the right RBAC).
func NewDiscoverer() (Discoverer, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return &nativeDiscoverer{client: cs}, nil
}

// NewDiscovererFromKubeconfig builds a Discoverer from a kubeconfig
// file. Intended for local development and tests; production pods
// should use NewDiscoverer.
func NewDiscovererFromKubeconfig(path, contextName string) (Discoverer, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return &nativeDiscoverer{client: cs}, nil
}

// NewDiscovererFromClient is the test-friendly constructor: pass any
// kubernetes.Interface (e.g. fake.NewSimpleClientset).
func NewDiscovererFromClient(c kubernetes.Interface) Discoverer {
	return &nativeDiscoverer{client: c}
}

func (d *nativeDiscoverer) ListKataPods(ctx context.Context) ([]PodInfo, error) {
	list, err := d.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	out := make([]PodInfo, 0, len(list.Items))
	for _, p := range list.Items {
		if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != KataRuntimeClassName {
			continue
		}
		out = append(out, PodInfo{
			Namespace: p.Namespace,
			Name:      p.Name,
			Node:      p.Spec.NodeName,
			PodIP:     p.Status.PodIP,
		})
	}
	return out, nil
}

func (d *nativeDiscoverer) ListKataNodes(ctx context.Context) ([]NodeInfo, error) {
	list, err := d.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: KataNodeLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make([]NodeInfo, 0, len(list.Items))
	for _, n := range list.Items {
		out = append(out, NodeInfo{
			Name:       n.Name,
			InternalIP: pickInternalIP(n.Status.Addresses),
		})
	}
	return out, nil
}

func (d *nativeDiscoverer) LookupPodNode(ctx context.Context, namespace, name string) (string, error) {
	p, err := d.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get pod %s/%s: %w", namespace, name, err)
	}
	if p.Spec.NodeName == "" {
		return "", fmt.Errorf("pod %s/%s has no nodeName", namespace, name)
	}
	return p.Spec.NodeName, nil
}

func (d *nativeDiscoverer) LookupNodeInternalIP(ctx context.Context, name string) (string, error) {
	n, err := d.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get node %s: %w", name, err)
	}
	ip := pickInternalIP(n.Status.Addresses)
	if ip == "" {
		return "", fmt.Errorf("node %s has no InternalIP", name)
	}
	return ip, nil
}

func pickInternalIP(addrs []corev1.NodeAddress) string {
	for _, a := range addrs {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}
