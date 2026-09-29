package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a 16-char lowercase hex migration ID. The length is short
// enough to embed in a 253-char Job name and long enough to avoid
// collisions across concurrent migrations.
func newID() MigrationID {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
	return MigrationID(hex.EncodeToString(b[:]))
}
