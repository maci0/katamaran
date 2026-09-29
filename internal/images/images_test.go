// Package images owns the invariants of the four shipped container images:
// digest-pinned bases, exec-form entrypoints, OCI labels, and a documented
// reason for every image that has to run as root. It holds no code.
package images

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dockerfiles maps each shipped image to the permissions its runtime stage
// must justify. requiresUser is false only for the node-agent image, which
// the DaemonSet runs privileged as root (deploy/daemonset.yaml).
var dockerfiles = map[string]struct {
	requiresUser bool
}{
	"Dockerfile":           {requiresUser: false},
	"Dockerfile.dashboard": {requiresUser: true},
	"Dockerfile.factory":   {requiresUser: true},
	"Dockerfile.mgr":       {requiresUser: true},
}

const rootRationaleMarker = "No USER:"

// repoRoot walks up from the test's working directory to the module root.
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
			t.Fatal("go.mod not found above the test working directory")
		}
		dir = parent
	}
}

func readDockerfile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func TestBaseImagesAreDigestPinned(t *testing.T) {
	for name := range dockerfiles {
		for _, line := range strings.Split(readDockerfile(t, name), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "FROM ") {
				continue
			}
			if !strings.Contains(line, "@sha256:") {
				t.Errorf("%s: base image is not digest-pinned: %s", name, line)
			}
		}
	}
}

func TestEntrypointsAreExecForm(t *testing.T) {
	for name := range dockerfiles {
		found := false
		for _, line := range strings.Split(readDockerfile(t, name), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "ENTRYPOINT ") {
				continue
			}
			found = true
			if !strings.HasPrefix(trimmed, `ENTRYPOINT ["`) {
				t.Errorf("%s: ENTRYPOINT must be exec form so PID 1 is the binary: %s", name, trimmed)
			}
		}
		if !found {
			t.Errorf("%s: no ENTRYPOINT", name)
		}
	}
}

func TestOCILabels(t *testing.T) {
	labels := []string{
		"org.opencontainers.image.title",
		"org.opencontainers.image.source",
		"org.opencontainers.image.documentation",
		"org.opencontainers.image.description",
		"org.opencontainers.image.version=",
	}
	for name := range dockerfiles {
		body := readDockerfile(t, name)
		for _, label := range labels {
			if !strings.Contains(body, label) {
				t.Errorf("%s: missing %s label", name, label)
			}
		}
	}
}

// TestRuntimeUserIsJustified pins the least-privilege rule: every image that
// can drop root must, and the one that cannot has to say why in the file.
func TestRuntimeUserIsJustified(t *testing.T) {
	for name, want := range dockerfiles {
		body := readDockerfile(t, name)
		hasUser := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "USER ") {
				hasUser = true
			}
		}
		switch {
		case want.requiresUser && !hasUser:
			t.Errorf("%s: runtime stage has no USER", name)
		case !want.requiresUser && !strings.Contains(body, rootRationaleMarker):
			t.Errorf("%s: runs as root without a %q rationale in the file", name, rootRationaleMarker)
		}
	}
}
