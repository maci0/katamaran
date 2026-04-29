package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() MigrationID {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
	return MigrationID(hex.EncodeToString(b[:]))
}
