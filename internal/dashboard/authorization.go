package dashboard

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// namespaceScope is the dashboard's authorization chokepoint over
// namespace-scoped objects. The dashboard's ServiceAccount can read every
// pod in the cluster and creates hostPID migration Jobs in kube-system, so
// without this scope any caller reaching /api/migrate or /api/pods reaches
// objects in every namespace, not just their own. The Migration CRD path
// pins pod references to the CR's namespace for the same reason (see
// controller.specToRequest); this is the dashboard-side equivalent.
//
// An empty allowed set means unrestricted: the dashboard ships as a
// cluster-admin tool reached over `kubectl port-forward`, so cluster-wide
// access is the default posture. Setting KATAMARAN_ALLOWED_NAMESPACES
// narrows both the read surface and the migration surface to the listed
// namespaces.
type namespaceScope struct {
	allowed map[string]struct{}
}

// newNamespaceScope parses a comma-separated namespace allowlist. Entries
// are validated as DNS-1123 labels so a typo fails at startup instead of
// silently authorizing nothing (or, worse, everything).
func newNamespaceScope(csv string) (namespaceScope, error) {
	scope := namespaceScope{allowed: map[string]struct{}{}}
	for _, ns := range strings.Split(csv, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return namespaceScope{}, fmt.Errorf("invalid namespace %q in KATAMARAN_ALLOWED_NAMESPACES: %s", ns, strings.Join(errs, "; "))
		}
		scope.allowed[ns] = struct{}{}
	}
	return scope, nil
}

// allows reports whether the dashboard may act on an object in ns. The
// zero namespaceScope (no allowlist configured) allows everything.
func (s namespaceScope) allows(ns string) bool {
	if len(s.allowed) == 0 {
		return true
	}
	_, ok := s.allowed[ns]
	return ok
}

// names returns the allowlist, for the startup log line.
func (s namespaceScope) names() []string {
	return slices.Sorted(maps.Keys(s.allowed))
}

// filterPods drops pods outside the allowlist so /api/pods never discloses
// a namespace the caller could not migrate. The result is never nil, so the
// endpoint answers [] rather than a JSON null on an empty match.
func (s namespaceScope) filterPods(pods []orchestrator.PodInfo) []orchestrator.PodInfo {
	if len(s.allowed) == 0 {
		if pods == nil {
			return []orchestrator.PodInfo{}
		}
		return pods
	}
	kept := make([]orchestrator.PodInfo, 0, len(pods))
	for _, p := range pods {
		if s.allows(p.Namespace) {
			kept = append(kept, p)
		}
	}
	return kept
}
