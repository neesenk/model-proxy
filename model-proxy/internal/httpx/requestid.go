// Package httpx owns small HTTP transport identities shared by the proxy
// front door and provider clients.
package httpx

import (
	"crypto/rand"
	"fmt"
	"time"
)

// NewRequestID returns a 32-character random hex seed. Callers combine a
// prefix of this seed with an atomic counter for cheap per-request identity,
// or use it whole as an upstream request-correlation header value.
func NewRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", bytes[:])
}
