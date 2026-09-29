package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// podModeForm builds a minimal valid pod-mode migration form.
func podModeForm(sourceNS, sourceName string) url.Values {
	form := url.Values{}
	form.Set("source_pod_namespace", sourceNS)
	form.Set("source_pod_name", sourceName)
	form.Set("dest_node", "node2")
	form.Set("image", "katamaran:dev")
	return form
}

func TestNamespaceScope_UnsetAllowsEverything(t *testing.T) {
	t.Parallel()
	scope, err := newNamespaceScope("")
	if err != nil {
		t.Fatalf("newNamespaceScope(%q) = %v, want no error", "", err)
	}
	if !scope.allows("kube-system") {
		t.Fatal("empty allowlist must leave the dashboard cluster-wide")
	}
}

func TestNamespaceScope_RejectsMalformedNamespace(t *testing.T) {
	t.Parallel()
	if _, err := newNamespaceScope("team-a,NOT A NAMESPACE"); err == nil {
		t.Fatal("newNamespaceScope accepted a malformed namespace, want a startup error")
	}
}

func TestNamespaceScope_TrimsAndDeduplicates(t *testing.T) {
	t.Parallel()
	scope, err := newNamespaceScope(" team-a , team-b ,team-a")
	if err != nil {
		t.Fatalf("newNamespaceScope = %v, want no error", err)
	}
	if got := strings.Join(scope.names(), ","); got != "team-a,team-b" {
		t.Fatalf("names() = %q, want %q", got, "team-a,team-b")
	}
}

// TestHandleListPods_HidesDisallowedNamespaces is the deny side of the pod
// picker: with an allowlist configured, a caller must not learn that pods in
// other namespaces exist.
func TestHandleListPods_HidesDisallowedNamespaces(t *testing.T) {
	t.Parallel()
	scope, err := newNamespaceScope("team-a")
	if err != nil {
		t.Fatalf("newNamespaceScope = %v", err)
	}
	app := &App{
		namespaces: scope,
		discoverer: &stubDiscoverer{pods: []orchestrator.PodInfo{
			{Namespace: "team-a", Name: "web-0", Node: "node1", PodIP: "10.244.1.5"},
			{Namespace: "team-b", Name: "billing-0", Node: "node1", PodIP: "10.244.1.6"},
		}},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/pods", nil)
	w := httptest.NewRecorder()
	app.handleListPods(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %v", w.Code)
	}
	if strings.Contains(w.Body.String(), "team-b") || strings.Contains(w.Body.String(), "billing-0") {
		t.Fatalf("response leaked a pod outside the allowlist: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "web-0") {
		t.Fatalf("response dropped an allowed pod: %s", w.Body.String())
	}
}

func TestHandleListPods_NoAllowlistReturnsEverything(t *testing.T) {
	t.Parallel()
	app := &App{discoverer: &stubDiscoverer{pods: []orchestrator.PodInfo{
		{Namespace: "team-a", Name: "web-0"},
		{Namespace: "team-b", Name: "billing-0"},
	}}}
	req := httptest.NewRequest(http.MethodGet, "/api/pods", nil)
	w := httptest.NewRecorder()
	app.handleListPods(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %v", w.Code)
	}
	if !strings.Contains(w.Body.String(), "team-b") {
		t.Fatalf("unrestricted listing dropped a pod: %s", w.Body.String())
	}
}

// TestHandleMigrate_RejectsNamespaceOutsideAllowlist is the deny side of the
// migration mutation: the dashboard's ServiceAccount can migrate any pod in
// the cluster, so the namespace allowlist must be enforced before the
// request reaches the orchestrator.
func TestHandleMigrate_RejectsNamespaceOutsideAllowlist(t *testing.T) {
	t.Parallel()
	scope, err := newNamespaceScope("team-a")
	if err != nil {
		t.Fatalf("newNamespaceScope = %v", err)
	}
	app := &App{orch: dummyOrchestrator(t), namespaces: scope, discoverer: &stubDiscoverer{
		pods: []orchestrator.PodInfo{
			{Namespace: "team-b", Name: "billing-0", Node: "node1"},
		},
		nodes: []orchestrator.NodeInfo{{Name: "node2", InternalIP: "10.0.0.2"}},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(podModeForm("team-b", "billing-0").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a namespace outside the allowlist, got %v (%s)", w.Code, w.Body.String())
	}
	app.migrationMutex.Lock()
	started := app.isMigrating
	app.migrationMutex.Unlock()
	if started {
		t.Fatal("migration should not start for a namespace outside the allowlist")
	}
}

func TestHandleMigrate_RejectsDestNamespaceOutsideAllowlist(t *testing.T) {
	t.Parallel()
	scope, err := newNamespaceScope("team-a")
	if err != nil {
		t.Fatalf("newNamespaceScope = %v", err)
	}
	app := &App{orch: dummyOrchestrator(t), namespaces: scope, discoverer: &stubDiscoverer{
		pods:  []orchestrator.PodInfo{{Namespace: "team-a", Name: "web-0", Node: "node1"}},
		nodes: []orchestrator.NodeInfo{{Name: "node2", InternalIP: "10.0.0.2"}},
	}}
	form := podModeForm("team-a", "web-0")
	form.Set("dest_pod_namespace", "team-b")
	form.Set("dest_pod_name", "web-1")
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a dest pod outside the allowlist, got %v (%s)", w.Code, w.Body.String())
	}
}

func TestHandleMigrate_AllowsNamespaceInAllowlist(t *testing.T) {
	t.Parallel()
	scope, err := newNamespaceScope("team-a")
	if err != nil {
		t.Fatalf("newNamespaceScope = %v", err)
	}
	app := &App{orch: dummyOrchestrator(t), namespaces: scope, discoverer: &stubDiscoverer{
		pods:  []orchestrator.PodInfo{{Namespace: "team-a", Name: "web-0", Node: "node1"}},
		nodes: []orchestrator.NodeInfo{{Name: "node2", InternalIP: "10.0.0.2"}},
	}}
	t.Cleanup(func() {
		app.migrationMutex.Lock()
		if app.migrationCancel != nil {
			app.migrationCancel()
		}
		app.migrationMutex.Unlock()
	})
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", strings.NewReader(podModeForm("team-a", "web-0").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.handleMigrate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for an allowed namespace, got %v (%s)", w.Code, w.Body.String())
	}
	waitMigrationDone(t, app, 5*time.Second)
	got := app.orch.(*fakeOrchestrator).LastRequest()
	if got.SourcePod == nil || got.SourcePod.Namespace != "team-a" {
		t.Fatalf("SourcePod = %+v, want team-a/web-0", got.SourcePod)
	}
}
