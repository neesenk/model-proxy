package main

import (
	"crypto/rand"
	"fmt"
	"time"
)

// newRequestID returns a 32-character random hex seed. Proxy combines a prefix
// of this seed with an atomic counter for cheap per-request identity.
func newRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", bytes[:])
}
