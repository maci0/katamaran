package orchestrator

import "fmt"

// cmdlineHostDir is the hostPath mount the source job uses to capture
// /proc/<qemu>/cmdline locally before emitting it to its pod log as a
// KATAMARAN_CMDLINE_B64 marker. The dest binary scrapes that marker
// via the apiserver instead of mounting the host directory itself —
// the previous SPDY-exec / stager-pod / hostPath shuffle is gone.
const cmdlineHostDir = "/tmp/katamaran-cmdlines"

// cmdlinePathFor returns the per-migration cmdline file path inside
// cmdlineHostDir. Used by Apply to build the source binary's
// --emit-cmdline-to flag.
func cmdlinePathFor(id MigrationID) string {
	return fmt.Sprintf("%s/cmdline-%s.txt", cmdlineHostDir, id)
}
