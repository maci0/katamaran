package migration

import "encoding/json"

// MigrationMeta is the on-disk migration-meta.json contract between the
// dest binary (writer: writeMigrationMeta) and the factory server
// (reader: internal/factory Watcher/Server). Both sides must use this one
// definition so the file format cannot drift.
//
// The dest binary writes it next to the QMP socket after a successful
// incoming live migration; the factory's directory watcher picks it up and
// offers the state to Kata shims via GetBaseVM for VM adoption.
type MigrationMeta struct {
	ID              string          `json:"id"`
	QEMUPid         int             `json:"qemu_pid"`
	QMPSocket       string          `json:"qmp_socket"`
	VsockCID        uint32          `json:"vsock_cid"`
	UUID            string          `json:"uuid"`
	VirtiofsdPid    int             `json:"virtiofsd_pid"`
	HypervisorState json.RawMessage `json:"hypervisor_state,omitempty"`
	CPU             uint32          `json:"cpu"`
	Memory          uint32          `json:"memory"`
	VMConfig        json.RawMessage `json:"vm_config,omitempty"`
	AgentConfig     json.RawMessage `json:"agent_config,omitempty"`
}
