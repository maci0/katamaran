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

// idSource mints migration IDs. Production uses newID (crypto/rand, since
// the ID names cluster-visible Jobs and must not collide across concurrent
// migrations). Deterministic runs substitute a scripted source: crypto/rand
// is the one input synctest cannot virtualize, so an injected source is
// what makes a replayed Apply produce the same Job names, byte for byte.
type idSource func() MigrationID
