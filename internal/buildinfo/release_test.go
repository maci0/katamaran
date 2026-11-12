package buildinfo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
			t.Fatal("could not find go.mod above the working directory")
		}
		dir = parent
	}
}

// sectionAfter returns the text between a markdown heading and the next
// heading of the same level.
func sectionAfter(text, heading string) string {
	start := strings.Index(text, heading)
	if start < 0 {
		return ""
	}
	rest := text[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestSupportedVersionMatchesRelease pins the security-support statement to
// the released version. SECURITY.md promises fixes on the current line only,
// and the table is the statement consumers read to learn whether their
// version still gets them. It was last hand-edited at 0.6.x and stayed there
// through three later releases, so a reader of 0.8.1 was told the version
// they ran was unsupported.
func TestSupportedVersionMatchesRelease(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	security, err := os.ReadFile(filepath.Join(root, "SECURITY.md"))
	if err != nil {
		t.Fatalf("read SECURITY.md: %v", err)
	}

	released := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindStringSubmatch(string(changelog))
	if released == nil {
		t.Fatal("CHANGELOG.md has no released version section")
	}
	current := released[1][:strings.LastIndex(released[1], ".")]

	table := sectionAfter(string(security), "## Supported versions")
	if table == "" {
		t.Fatal("SECURITY.md has no Supported versions section")
	}
	row := regexp.MustCompile(`(?m)^\| (` + regexp.QuoteMeta(current) + `\.x) \(current\) \| yes \|`).FindStringSubmatch(table)
	if row == nil {
		t.Errorf("SECURITY.md support table does not name %s.x as the current supported line; the latest release is %s", current, released[1])
	}

	if !strings.Contains(string(changelog), "## ["+released[1]+"]") {
		t.Errorf("CHANGELOG.md has no section for %s", released[1])
	}
	if !strings.Contains(Version, released[1]) {
		t.Errorf("buildinfo.Version %q does not carry the latest released version %s", Version, released[1])
	}

	// CommonMark resolves the first definition of a reference link and
	// ignores the rest. The compare links have to live in one block after
	// the last section: a definition left between sections wins, and the
	// footer copy that a release updates never takes effect.
	body, footer, ok := strings.Cut(string(changelog), "\n[Unreleased]:")
	if !ok {
		t.Fatal("CHANGELOG.md has no [Unreleased] link definition")
	}
	if strings.Contains(body, "\n[0.") {
		t.Errorf("CHANGELOG.md defines a version link before the footer block, which shadows the footer definition")
	}
	defined := map[string]bool{}
	for _, line := range strings.Split(footer, "\n") {
		if m := regexp.MustCompile(`^\[(\d+\.\d+\.\d+)\]:`).FindStringSubmatch(line); m != nil {
			defined[m[1]] = true
		}
	}
	for _, heading := range regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindAllStringSubmatch(string(changelog), -1) {
		if !defined[heading[1]] {
			t.Errorf("CHANGELOG.md section %s has no footer link definition", heading[1])
		}
	}
}
