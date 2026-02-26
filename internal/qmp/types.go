package qmp

import (
	"encoding/json"
	"fmt"
)

// Args is a sealed marker interface for QMP command arguments.
// Only types in this package can implement it (via the unexported method),
// preventing arbitrary values from being passed to Execute().
type Args interface {
	qmpArgs() // unexported method seals the interface to this package
}

// request represents a QMP command envelope.
type request struct {
	Execute   string `json:"execute"`
	Arguments Args   `json:"arguments,omitempty"`
}

// response represents a QMP command response or asynchronous event.
type response struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
}

// Error represents a QMP protocol-level error.
type Error struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("QMP error [%s]: %s", e.Class, e.Desc)
}

// BlockJobInfo represents a single entry returned by query-block-jobs.
type BlockJobInfo struct {
	Device string `json:"device"`
	Len    int64  `json:"len"`
	Offset int64  `json:"offset"`
	Ready  bool   `json:"ready"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

// MigrateInfo represents the response from query-migrate.
type MigrateInfo struct {
	Status    string `json:"status"`
	ErrorDesc string `json:"error-desc,omitempty"`
}

// QMP command argument types — strictly typed to prevent typos and ensure
// correct JSON serialization for each QMP command.

// NBDServerStartArgs are the arguments for the nbd-server-start command.
type NBDServerStartArgs struct {
	Addr NBDServerAddr `json:"addr"`
}

// NBDServerAddr describes the listen address for the NBD server.
type NBDServerAddr struct {
	Type string            `json:"type"`
	Data NBDServerAddrData `json:"data"`
}

// NBDServerAddrData contains the host and port for the NBD server address.
type NBDServerAddrData struct {
	Host string `json:"host"`
	Port string `json:"port"`
}

// NBDServerAddArgs are the arguments for the nbd-server-add command.
type NBDServerAddArgs struct {
	Device   string `json:"device"`
	Writable bool   `json:"writable"`
}

// DriveMirrorArgs are the arguments for the drive-mirror command.
type DriveMirrorArgs struct {
	Device string `json:"device"`
	Target string `json:"target"`
	Sync   string `json:"sync"`
	Mode   string `json:"mode"`
	JobID  string `json:"job-id"`
}

// BlockJobCancelArgs are the arguments for the block-job-cancel command.
type BlockJobCancelArgs struct {
	Device string `json:"device"`
	Force  bool   `json:"force"`
}

// MigrateSetCapabilitiesArgs are the arguments for migrate-set-capabilities.
type MigrateSetCapabilitiesArgs struct {
	Capabilities []MigrationCapability `json:"capabilities"`
}

// MigrationCapability describes a single migration capability toggle.
type MigrationCapability struct {
	Capability string `json:"capability"`
	State      bool   `json:"state"`
}

// MigrateSetParametersArgs are the arguments for migrate-set-parameters.
type MigrateSetParametersArgs struct {
	DowntimeLimit int64 `json:"downtime-limit"`
	MaxBandwidth  int64 `json:"max-bandwidth"`
}

// MigrateArgs are the arguments for the migrate command.
type MigrateArgs struct {
	URI string `json:"uri"`
}

// AnnounceSelfArgs are the arguments for the announce-self command.
type AnnounceSelfArgs struct {
	Initial int `json:"initial"`
	Max     int `json:"max"`
	Rounds  int `json:"rounds"`
	Step    int `json:"step"`
}

// Seal Args to this package. Each argument struct must implement the
// unexported marker method so the compiler rejects arbitrary types.
func (NBDServerStartArgs) qmpArgs()         {}
func (NBDServerAddArgs) qmpArgs()           {}
func (DriveMirrorArgs) qmpArgs()            {}
func (BlockJobCancelArgs) qmpArgs()         {}
func (MigrateSetCapabilitiesArgs) qmpArgs() {}
func (MigrateSetParametersArgs) qmpArgs()   {}
func (MigrateArgs) qmpArgs()                {}
func (AnnounceSelfArgs) qmpArgs()           {}
