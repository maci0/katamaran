package orchestrator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNativeDiscoverer_ListKataPods(t *testing.T) {
	t.Parallel()
	kata := "kata-qemu"
	runc := "runc"
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "vm-a"},
			Spec:       corev1.PodSpec{RuntimeClassName: &kata, NodeName: "n1"},
			Status:     corev1.PodStatus{PodIP: "10.0.0.5"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "non-kata"},
			Spec:       corev1.PodSpec{RuntimeClassName: &runc, NodeName: "n1"},
			Status:     corev1.PodStatus{PodIP: "10.0.0.6"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "vm-b"},
			Spec:       corev1.PodSpec{RuntimeClassName: &kata, NodeName: "n2"},
			Status:     corev1.PodStatus{PodIP: "10.0.0.7"},
		},
	)
	got, err := NewDiscovererFromClient(cs).ListKataPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 kata pods, got %d: %+v", len(got), got)
	}
	for _, p := range got {
		if p.Name == "non-kata" {
			t.Fatalf("non-kata pod leaked: %+v", p)
		}
	}
}

func TestNativeDiscoverer_ListKataNodes(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"katacontainers.io/kata-runtime": "true"}},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "n1"},
				{Type: corev1.NodeInternalIP, Address: "10.0.1.1"},
			}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n-no-label"},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.1.99"}}},
		},
	)
	got, err := NewDiscovererFromClient(cs).ListKataNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "n1" || got[0].InternalIP != "10.0.1.1" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestNativeDiscoverer_LookupErrors(t *testing.T) {
	t.Parallel()
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pending"},
		Spec:       corev1.PodSpec{}, // NodeName empty
	}
	cs := fake.NewSimpleClientset(pending)
	d := NewDiscovererFromClient(cs)
	if _, err := d.LookupPodNode(context.Background(), "default", "pending"); err == nil {
		t.Fatal("expected error for pending pod with no nodeName")
	}
	if _, err := d.LookupNodeInternalIP(context.Background(), "missing"); err == nil {
		t.Fatal("expected error for missing node")
	}
}
