package migration

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MigrationMetaFile is the on-disk filename of the MigrationMeta contract
// below. Both the writer (dest binary) and the reader (factory Watcher)
// must reference this constant so the name cannot drift.
const MigrationMetaFile = "migration-meta.json"

// MigrationMeta is the on-disk migration-meta.json contract between the
// dest binary (writer: writeMigrationMeta) and the factory server
// (reader: internal/factory Watcher/Server). Both sides must use this one
// definition so the file format cannot drift.
//
// The dest binary writes it next to the QMP socket after a successful
// incoming live migration; the factory's directory watcher picks it up and
// offers the state to Kata shims via GetBaseVM for VM adoption.
//
// Pid fields are explicitly 64-bit (not int): writer and reader are
// separate binaries that may run on different architectures, and the
// protobuf surface this feeds (cachepb) already uses int64.
type MigrationMeta struct {
	ID              string          `json:"id"`
	QEMUPid         int64           `json:"qemu_pid"`
	QMPSocket       string          `json:"qmp_socket"`
	VirtiofsdPid    int64           `json:"virtiofsd_pid"`
	HypervisorState json.RawMessage `json:"hypervisor_state,omitempty"`
	CPU             uint32          `json:"cpu"`
	Memory          uint32          `json:"memory"`
	VMConfig        json.RawMessage `json:"vm_config,omitempty"`
	AgentConfig     json.RawMessage `json:"agent_config,omitempty"`
}

// WriteMigrationMetaFile publishes meta at <dir>/<MigrationMetaFile> so the
// factory watcher, which polls dir from another process, always reads either
// the previous file or the complete new one.
//
// The rename alone gives readers that atomicity, but it publishes a name whose
// bytes are still only in the page cache. A node crash after the rename leaves
// a zero-length file behind, and the watcher's response to an unparseable file
// is to stop retrying it, so that sandbox's VM is silently never offered for
// adoption. fsync the temp file before the rename and the directory after it,
// which is the ordering that makes the published content durable.
func WriteMigrationMetaFile(dir string, meta MigrationMeta) (string, error) {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal migration metadata: %w", err)
	}
	metaPath := filepath.Join(dir, MigrationMetaFile)
	// A per-write temp name, not "<meta>.tmp": two publishers in the same
	// directory would otherwise share one staging file, and the first rename
	// would pull it out from under the second, which then renames a path that
	// no longer exists and publishes nothing.
	tmp, err := os.CreateTemp(dir, MigrationMetaFile+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create staging file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := writeAndSync(tmp, data); err != nil {
		cleanup()
		return "", fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, metaPath); err != nil {
		cleanup()
		return "", fmt.Errorf("rename %s: %w", metaPath, err)
	}
	// The file is published either way. A directory fsync failure does not
	// unpublish it, but it does mean a crash can still lose the rename, so
	// the caller must hear about it instead of treating the publish as durable.
	// Flush the directory before closing: a close error must not skip the
	// flush that makes the rename durable.
	dirErr := syncDir(dir)
	// The fd still names the published file after the rename. Close it before
	// claiming success: a close error is a flush the caller would otherwise
	// never see, and the fd would stay open until process exit.
	if err := tmp.Close(); err != nil {
		err = fmt.Errorf("close %s: %w", metaPath, err)
		if dirErr != nil {
			err = errors.Join(err, fmt.Errorf("sync directory %s: %w", dir, dirErr))
		}
		return metaPath, err
	}
	if dirErr != nil {
		return metaPath, fmt.Errorf("sync directory %s after publishing %s: %w", dir, metaPath, dirErr)
	}
	return metaPath, nil
}

// writeAndSync writes data to f and flushes it to stable storage. The file is
// left open; the caller closes it.
func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// syncDir flushes a directory entry so a completed rename survives a crash.
// Both the open and the fsync are returned: dropping either leaves the caller
// believing the published metadata is durable when a crash can still lose it.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
