// Package manifests holds no code. Its test decodes every first-party
// Kubernetes manifest in the tree into typed API objects, which is the
// stand-in for a schema validator (kubeconform) on a machine that has none.
//
// It covers the same ground a standalone validator would, and two things it
// would not: cross-file ServiceAccount references, and single-owner
// declarations for cluster-scoped RBAC that more than one file could emit.
package manifests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// Types with no schema in the Go clientset: the repo's own CRD, the CRD
// kind itself, and a kind owned by an optional operator. They are named here
// so a misspelled apiVersion cannot pass as "just another unknown kind".
var crdGroupKinds = map[string]bool{
	"apiextensions.k8s.io":  true, // the CustomResourceDefinition kind itself
	"monitoring.coreos.com": true, // ServiceMonitor, needs a Prometheus Operator
	"katamaran.io":          true, // Migration, defined by config/crd/migration.yaml
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the working directory")
		}
		dir = parent
	}
}

// manifestFiles returns every first-party manifest the applier can be
// pointed at. The orchestrator's Job templates are included: they are
// rendered with ${VAR} placeholders, but the placeholders are string
// substitutions, so the surrounding document still has to be valid.
func manifestFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var files []string
	for _, dir := range []string{
		"deploy",
		"config/crd",
		"demo",
		"internal/orchestrator/templates",
	} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
				files = append(files, filepath.Join(root, dir, name))
			}
		}
	}
	return files
}

// decodeAll splits a possibly multi-document YAML stream and returns each
// non-empty document as an unstructured object.
func decodeAll(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []*unstructured.Unstructured
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: split: %v", path, err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(obj); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if obj.GetKind() == "" {
			continue
		}
		out = append(out, obj)
	}
}

// TestManifestsDecodeTyped is the schema-validation pass. A field the typed
// object does not know, or a scalar that retyped on the way in (a bare `1`
// where a string is required, a quoted number where an int is required),
// fails the decode or the conversion.
func TestManifestsDecodeTyped(t *testing.T) {
	for _, path := range manifestFiles(t) {
		rel, _ := filepath.Rel(repoRoot(t), path)
		for _, obj := range decodeAll(t, path) {
			gvk := obj.GroupVersionKind()
			if crdGroupKinds[gvk.Group] {
				if obj.GetKind() == "" {
					t.Fatalf("%s: document has no kind", rel)
				}
				continue
			}
			typed, err := clientgoscheme.Scheme.New(gvk)
			if err != nil {
				t.Fatalf("%s: %s is not a known API type: %v", rel, gvk, err)
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, typed); err != nil {
				t.Errorf("%s: %s does not fit %T: %v", rel, gvk, typed, err)
				continue
			}
			// The conversion is not strict, so a field name the API type
			// does not define is dropped silently and the apply succeeds
			// against a server that validates strictly. Round-tripping and
			// diffing catches it: what the converter could not represent
			// comes back missing.
			round, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
			if err != nil {
				t.Errorf("%s: %s does not round-trip: %v", rel, gvk, err)
				continue
			}
			// Both sides go through JSON so nested objects are plain
			// map[string]any on each: the converter emits a typed
			// map[string]string for label and selector maps, which would
			// otherwise look like a scalar to the diff and stop the walk.
			roundNorm, err := normalize(round)
			if err != nil {
				t.Errorf("%s: %s: %v", rel, gvk, err)
				continue
			}
			srcNorm, err := normalize(obj.Object)
			if err != nil {
				t.Errorf("%s: %s: %v", rel, gvk, err)
				continue
			}
			for _, path := range missingFields(srcNorm, roundNorm, "") {
				t.Errorf("%s: %s: %s is not a field of %T; the apiserver rejects it under strict field validation",
					rel, gvk, path, typed)
			}
		}
	}
}

// normalize re-decodes a value through JSON so every nested object is a
// map[string]any regardless of what concrete map type produced it.
func normalize(v map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}

// missingFields walks two decoded objects and returns the dotted paths
// present in src but absent from dst. Only presence is compared, not value:
// the converter normalises some values on the way through (a resource
// Quantity is a string in YAML and a struct in Go), and a field the schema
// does not define never survives the trip back at all.
func missingFields(src, dst map[string]any, prefix string) []string {
	var out []string
	for _, key := range sortedKeys(src) {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		sv := src[key]
		dv, ok := dst[key]
		if !ok {
			// An explicitly empty list or map normalises away: the Go
			// fields are omitempty slices, so `args: []` round-trips to
			// no key at all. That is not an unknown field.
			if isEmptyContainer(sv) {
				continue
			}
			out = append(out, path)
			continue
		}
		sm, sok := sv.(map[string]any)
		dm, dok := dv.(map[string]any)
		if sok && dok {
			out = append(out, missingFields(sm, dm, path)...)
			continue
		}
		// Structured lists carry the same risk as maps: a typo in a port,
		// env var or volume mount entry is the most common way an invalid
		// manifest survives review.
		sl, sok := sv.([]any)
		dl, dok := dv.([]any)
		if sok && dok {
			for i := range sl {
				if i >= len(dl) {
					break
				}
				se, eok := sl[i].(map[string]any)
				de, fok := dl[i].(map[string]any)
				if eok && fok {
					out = append(out, missingFields(se, de, fmt.Sprintf("%s[%d]", path, i))...)
				}
			}
		}
	}
	return out
}

// isEmptyContainer reports whether v is an empty list or map.
func isEmptyContainer(v any) bool {
	switch t := v.(type) {
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestCRDsAreWellFormed covers the CRD manifests, which the typed pass skips
// because apiextensions types are not in the clientset. A CRD the apiserver
// rejects takes the whole install with it, and these are the fields it
// rejects on: a name that disagrees with spec.names.plural, a scope that is
// neither Namespaced nor Cluster, no served version, or no storage version.
func TestCRDsAreWellFormed(t *testing.T) {
	root := repoRoot(t)
	crdDir := filepath.Join(root, "config", "crd")
	entries, err := os.ReadDir(crdDir)
	if err != nil {
		t.Fatalf("read config/crd: %v", err)
	}

	found := 0
	for _, e := range entries {
		path := filepath.Join(crdDir, e.Name())
		for _, obj := range decodeAll(t, path) {
			if obj.GetKind() != "CustomResourceDefinition" {
				continue
			}
			found++
			rel, _ := filepath.Rel(root, path)
			group, _, _ := unstructured.NestedString(obj.Object, "spec", "group")
			plural, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "plural")
			kind, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind")
			scope, _, _ := unstructured.NestedString(obj.Object, "spec", "scope")

			if want := plural + "." + group; obj.GetName() != want {
				t.Errorf("%s: metadata.name is %q but spec.names.plural + spec.group is %q", rel, obj.GetName(), want)
			}
			if scope != "Namespaced" && scope != "Cluster" {
				t.Errorf("%s: spec.scope is %q, want Namespaced or Cluster", rel, scope)
			}
			if kind == "" {
				t.Errorf("%s: spec.names.kind is empty", rel)
			}

			versions, _, _ := unstructured.NestedSlice(obj.Object, "spec", "versions")
			if len(versions) == 0 {
				t.Errorf("%s: spec.versions is empty, so no API is served", rel)
				continue
			}
			served, storage := 0, 0
			for i, v := range versions {
				m, ok := v.(map[string]any)
				if !ok {
					t.Errorf("%s: spec.versions[%d] is not an object", rel, i)
					continue
				}
				if s, _ := m["served"].(bool); s {
					served++
				}
				if s, _ := m["storage"].(bool); s {
					storage++
				}
				if _, ok := m["schema"]; !ok {
					t.Errorf("%s: version %v has no schema; a structural schema is required to prune unknown fields",
						rel, m["name"])
				}
			}
			if served == 0 {
				t.Errorf("%s: no version has served: true", rel)
			}
			if storage != 1 {
				t.Errorf("%s: %d versions have storage: true, want exactly 1", rel, storage)
			}
		}
	}
	if found == 0 {
		t.Fatal("no CustomResourceDefinition found under config/crd")
	}
}

// TestServiceAccountReferencesResolve checks every serviceAccountName a
// workload in the tree names against the ServiceAccounts the install
// manifests declare. A pod whose ServiceAccount does not exist never starts,
// and the failure surfaces at migration time rather than at apply time, so
// nothing else in the tree reports it.
func TestServiceAccountReferencesResolve(t *testing.T) {
	root := repoRoot(t)
	declared := map[string][]string{}
	referenced := map[string][]string{}

	for _, path := range manifestFiles(t) {
		rel, _ := filepath.Rel(root, path)
		for _, obj := range decodeAll(t, path) {
			gvk := obj.GroupVersionKind()
			if crdGroupKinds[gvk.Group] {
				continue
			}
			switch obj.GetKind() {
			case "ServiceAccount":
				// Only install manifests own a ServiceAccount for the
				// applier; scripts/manifests are per-test scratch pods.
				if strings.HasPrefix(rel, "deploy"+string(filepath.Separator)) {
					declared[obj.GetName()] = append(declared[obj.GetName()], rel)
				}
			case "Pod":
				sa, _, _ := unstructured.NestedString(obj.Object, "spec", "serviceAccountName")
				if sa != "" {
					referenced[sa] = append(referenced[sa], rel)
				}
			case "Deployment", "DaemonSet", "StatefulSet", "Job", "CronJob":
				containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
				for _, c := range containers {
					m, ok := c.(map[string]any)
					if !ok {
						continue
					}
					sa, _ := m["serviceAccountName"].(string)
					if sa != "" {
						referenced[sa] = append(referenced[sa], rel)
					}
				}
			}
		}
	}

	for sa, users := range referenced {
		owners, ok := declared[sa]
		if !ok {
			t.Errorf("serviceAccountName %q (used by %s) is declared by no manifest in deploy/",
				sa, strings.Join(users, ", "))
			continue
		}
		// Two files emitting the same ServiceAccount means the applier's
		// last-wins ordering decides which one's teardown removes it, so
		// the other producer silently loses its Jobs' identity.
		if len(owners) > 1 {
			t.Errorf("serviceAccountName %q is declared by %d files (%s); it needs exactly one owner",
				sa, len(owners), strings.Join(owners, ", "))
		}
	}
}

// TestServiceMonitorsSelectADeclaredService guards the split between
// deploy/metrics-services.yaml (core/v1, applies anywhere) and
// deploy/monitoring.yaml (ServiceMonitor, needs a Prometheus Operator). A
// selector that matches no Service scrapes nothing, and Prometheus reports
// no error, so a rename on one side is invisible until the graph goes flat.
func TestServiceMonitorsSelectADeclaredService(t *testing.T) {
	root := repoRoot(t)
	svcLabels := map[string]bool{}
	for _, path := range manifestFiles(t) {
		for _, obj := range decodeAll(t, path) {
			if obj.GetKind() != "Service" {
				continue
			}
			sel, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector")
			for k, v := range sel {
				svcLabels[k+"="+v] = true
			}
		}
	}

	checked := 0
	for _, path := range manifestFiles(t) {
		rel, _ := filepath.Rel(root, path)
		for _, obj := range decodeAll(t, path) {
			if obj.GetKind() != "ServiceMonitor" {
				continue
			}
			sel, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
			if len(sel) == 0 {
				t.Errorf("%s: ServiceMonitor %s has an empty selector", rel, obj.GetName())
				continue
			}
			checked++
			for k, v := range sel {
				if !svcLabels[k+"="+v] {
					t.Errorf("%s: ServiceMonitor %s selects %s=%s, which no declared Service carries",
						rel, obj.GetName(), k, v)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ServiceMonitor found; the selector check passed vacuously")
	}
}

// TestInstallManifestsPinTheirNamespace checks that every object in the
// install manifests carries an explicit metadata.namespace. The applier's
// context then cannot decide where the workload lands: `kubectl -n foo apply
// -f deploy/manager.yaml` and `kubectl apply -n foo -f deploy/manager.yaml`
// produce the same objects, and a CRB subject's namespace cannot drift away
// from its ServiceAccount's.
func TestInstallManifestsPinTheirNamespace(t *testing.T) {
	root := repoRoot(t)
	clusterScoped := map[schema.GroupKind]bool{
		{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:                       true,
		{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}:                true,
		{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}:               true,
		{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}: true,
		{Group: "node.k8s.io", Kind: "RuntimeClass"}:                                    true,
	}

	for _, path := range manifestFiles(t) {
		rel, _ := filepath.Rel(root, path)
		if !strings.HasPrefix(rel, "deploy"+string(filepath.Separator)) {
			continue
		}
		for _, obj := range decodeAll(t, path) {
			gk := schema.GroupKind{Group: obj.GroupVersionKind().Group, Kind: obj.GetKind()}
			if clusterScoped[gk] {
				if obj.GetNamespace() != "" {
					t.Errorf("%s: cluster-scoped %s %s declares namespace %q",
						rel, gk.Kind, obj.GetName(), obj.GetNamespace())
				}
				continue
			}
			if obj.GetNamespace() == "" {
				t.Errorf("%s: %s %s has no metadata.namespace, so the applier's context decides where it lands",
					rel, gk.Kind, obj.GetName())
			}
		}
	}
}

// TestRoleRefsResolve checks that every RoleBinding's roleRef names an object
// the tree actually declares, and that each subject's namespace matches the
// ServiceAccount it names. A dangling roleRef leaves the binding in place
// granting nothing, which reads as a working install.
func TestRoleRefsResolve(t *testing.T) {
	root := repoRoot(t)
	roles := map[schema.GroupKind]map[string]bool{
		{Group: "rbac.authorization.k8s.io", Kind: "Role"}:        {},
		{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}: {},
	}
	accounts := map[string]string{} // "ns/name" -> declaring file

	for _, path := range manifestFiles(t) {
		rel, _ := filepath.Rel(root, path)
		for _, obj := range decodeAll(t, path) {
			gk := schema.GroupKind{Group: obj.GroupVersionKind().Group, Kind: obj.GetKind()}
			switch gk {
			case schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "Role"},
				schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:
				roles[gk][obj.GetName()] = true
			case schema.GroupKind{Kind: "ServiceAccount"}:
				accounts[obj.GetNamespace()+"/"+obj.GetName()] = rel
			}
		}
	}

	for _, path := range manifestFiles(t) {
		rel, _ := filepath.Rel(root, path)
		for _, obj := range decodeAll(t, path) {
			if obj.GetKind() != "RoleBinding" && obj.GetKind() != "ClusterRoleBinding" {
				continue
			}
			ref, _, _ := unstructured.NestedString(obj.Object, "roleRef", "name")
			refKind, _, _ := unstructured.NestedString(obj.Object, "roleRef", "kind")
			gk := schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: refKind}
			byKind, ok := roles[gk]
			if !ok {
				t.Errorf("%s: %s %s binds roleRef kind %q, which is not a Role or ClusterRole",
					rel, obj.GetKind(), obj.GetName(), refKind)
				continue
			}
			if !byKind[ref] {
				t.Errorf("%s: %s %s binds %s %q, which no manifest in the tree declares",
					rel, obj.GetKind(), obj.GetName(), refKind, ref)
			}

			subjects, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
			for _, s := range subjects {
				m, ok := s.(map[string]any)
				if !ok {
					continue
				}
				if kind, _ := m["kind"].(string); kind != "ServiceAccount" {
					continue
				}
				name, _ := m["name"].(string)
				ns, _ := m["namespace"].(string)
				if ns == "" {
					t.Errorf("%s: %s %s binds ServiceAccount %q with no namespace",
						rel, obj.GetKind(), obj.GetName(), name)
					continue
				}
				if _, ok := accounts[ns+"/"+name]; !ok {
					t.Errorf("%s: %s %s binds ServiceAccount %s/%s, which no manifest declares",
						rel, obj.GetKind(), obj.GetName(), ns, name)
				}
			}
		}
	}
}
