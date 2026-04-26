package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestListKataPods_ParsesKubectlJSON(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
cat <<'EOF'
{"items":[{"metadata":{"namespace":"default","name":"vm-a"},"spec":{"runtimeClassName":"kata-qemu","nodeName":"n1"},"status":{"podIP":"10.0.0.5"}},
{"metadata":{"namespace":"kube-system","name":"other"},"spec":{"runtimeClassName":"runc"},"status":{"podIP":"10.0.0.6"}}]}
EOF
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	got, err := ListKataPods(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 kata pod, got %d: %+v", len(got), got)
	}
	want := PodInfo{Namespace: "default", Name: "vm-a", Node: "n1", PodIP: "10.0.0.5"}
	if got[0] != want {
		t.Fatalf("want %+v, got %+v", want, got[0])
	}
}

func TestListKataNodes_ParsesKubectlJSON(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
cat <<'EOF'
{"items":[
  {"metadata":{"name":"n1"},"status":{"addresses":[{"type":"Hostname","address":"n1"},{"type":"InternalIP","address":"192.168.1.10"}]}},
  {"metadata":{"name":"n2"},"status":{"addresses":[{"type":"InternalIP","address":"192.168.1.11"},{"type":"ExternalIP","address":"203.0.113.5"}]}}
]}
EOF
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	got, err := ListKataNodes(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 nodes, got %d: %+v", len(got), got)
	}
	want := []NodeInfo{
		{Name: "n1", InternalIP: "192.168.1.10"},
		{Name: "n2", InternalIP: "192.168.1.11"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node %d: want %+v, got %+v", i, want[i], got[i])
		}
	}
}
