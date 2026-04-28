package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() MigrationID {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return MigrationID(hex.EncodeToString(b[:]))
}
