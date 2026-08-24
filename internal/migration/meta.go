package migration

import "encoding/json"

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
