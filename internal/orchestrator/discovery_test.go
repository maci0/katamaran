package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestKubectlDiscoverer_ListKataPods(t *testing.T) {
	stubKubectl(t, `cat <<'EOF'
{"items":[
  {"metadata":{"namespace":"default","name":"vm-a"},"spec":{"runtimeClassName":"kata-qemu","nodeName":"n1"},"status":{"podIP":"10.0.0.5"}},
  {"metadata":{"namespace":"kube-system","name":"other"},"spec":{"runtimeClassName":"runc"},"status":{"podIP":"10.0.0.6"}},
  {"metadata":{"namespace":"default","name":"vm-b"},"spec":{"runtimeClassName":"kata-qemu","nodeName":"n2"},"status":{"podIP":"10.0.0.7"}}
]}
EOF
`)
	got, err := NewKubectlDiscoverer().ListKataPods(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 kata pods, got %d: %+v", len(got), got)
	}
	if got[0].Name != "vm-a" || got[1].Name != "vm-b" {
		t.Fatalf("unexpected names: %+v", got)
	}
}

func TestKubectlDiscoverer_ListKataNodes(t *testing.T) {
	stubKubectl(t, `cat <<'EOF'
{"items":[
  {"metadata":{"name":"n1"},"status":{"addresses":[{"type":"Hostname","address":"n1"},{"type":"InternalIP","address":"10.0.1.1"}]}},
  {"metadata":{"name":"n2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.2"},{"type":"ExternalIP","address":"1.2.3.4"}]}}
]}
EOF
`)
	got, err := NewKubectlDiscoverer().ListKataNodes(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(got))
	}
	if got[0].InternalIP != "10.0.1.1" || got[1].InternalIP != "10.0.1.2" {
		t.Fatalf("unexpected IPs: %+v", got)
	}
}

func TestKubectlDiscoverer_LookupPodNode_EmptyErrors(t *testing.T) {
	stubKubectl(t, `printf ''`)
	_, err := NewKubectlDiscoverer().LookupPodNode(context.Background(), "default", "missing")
	if err == nil {
		t.Fatal("expected error for empty nodeName")
	}
}

// stubKubectl writes a `kubectl` shell-stub into a temp dir and prepends it
// to PATH for the duration of t. The body is the exact bash to execute when
// the stub runs (e.g. a heredoc cat).
func stubKubectl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}
