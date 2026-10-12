package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
)

var keyHeaders = []string{"anthropic-beta", "accept-language"}

// Key returns the exact-match key for method, path, query, response-affecting
// headers, the caller's key id, and the raw request body. Every field is
// length-prefixed: the body is arbitrary bytes, so a bare delimiter join let a
// crafted body collide with a different (method, path, query, header) prefix.
//
// caller is the authenticated API-key id ("" on the unauthenticated loopback
// default; an empty caller writes NOTHING, keeping the legacy byte layout —
// the golden encoding test pins it). Responses are cached PER CALLER: with api-keys enabled, an entry
// served under one key must never replay to another — the cached bytes may
// embed key-attributed accounting/quota meaning and, at minimum, cross-key
// replay is an information flow the operator did not authorize (Bifrost
// isolated its semantic cache per virtual key for the same reason; upgrading
// turns existing entries cold by design). "" keys stay byte-identical to the
// legacy composition so single-tenant loopback deployments keep their cache.
func Key(request *http.Request, body []byte, caller string) string {
	hash := sha256.New()
	writeField := func(field string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		hash.Write(length[:])
		hash.Write([]byte(field))
	}
	writeField(request.Method)
	writeField(request.URL.Path)
	writeField(request.URL.RawQuery)
	for _, name := range keyHeaders {
		if value := request.Header.Get(name); value != "" {
			writeField(name + ":" + value)
		}
	}
	if caller != "" {
		writeField(caller)
	}
	// Same length-prefixed encoding as writeField, but hashing the body bytes
	// directly: converting to string first would copy the whole body twice.
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(body)))
	hash.Write(length[:])
	hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}
