package orchestrator

import (
	"fmt"
	"path/filepath"

	"github.com/maci0/katamaran/internal/migration"
)

// cmdlinePathFor returns the per-migration cmdline file path inside
// migration.CmdlineHostDir. Used by Apply to build the source binary's
// --emit-cmdline-to flag. The dest binary materializes pod-log-fetched
// cmdlines under the same directory (migration.CmdlineHostDir), so both
// sides must agree on it.
func cmdlinePathFor(id MigrationID) string {
	return filepath.Join(migration.CmdlineHostDir, fmt.Sprintf("cmdline-%s.txt", id))
}
