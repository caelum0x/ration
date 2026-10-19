// Package idgen generates short, unique, non-sequential identifiers used
// for normalized event IDs. webhook-ingestion's own copy of this tiny
// utility (see docs/adr/0004: idgen is shared support, not owned by one
// service, so each extracted service gets its own copy rather than a
// shared internal Go module). It avoids pulling in an external UUID
// dependency for a single 16-byte random hex token.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New returns a new identifier of the form "<prefix>_<32 hex chars>".
// It panics only if the system CSPRNG is unavailable, which indicates a
// broken runtime environment rather than a recoverable error.
func New(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("idgen: system random source unavailable: %v", err))
	}
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(buf[:]))
}
