package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
)

var keyHeaders = []string{"anthropic-beta", "accept-language"}

// Key returns the exact-match key for method, path, query, response-affecting
// headers, and the raw request body. Every field is length-prefixed: the body
// is arbitrary bytes, so a bare delimiter join let a crafted body collide
// with a different (method, path, query, header) prefix.
func Key(request *http.Request, body []byte) string {
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
	// Same length-prefixed encoding as writeField, but hashing the body bytes
	// directly: converting to string first would copy the whole body twice.
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(body)))
	hash.Write(length[:])
	hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}
