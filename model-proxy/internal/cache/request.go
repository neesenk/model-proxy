package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

var keyHeaders = []string{"anthropic-beta", "accept-language"}

// Key returns the exact-match key for method, path, query, response-affecting
// headers, and the raw request body.
func Key(request *http.Request, body []byte) string {
	hash := sha256.New()
	hash.Write([]byte(request.Method))
	hash.Write([]byte{0})
	hash.Write([]byte(request.URL.Path))
	hash.Write([]byte{0})
	hash.Write([]byte(request.URL.RawQuery))
	hash.Write([]byte{0})
	for _, name := range keyHeaders {
		if value := request.Header.Get(name); value != "" {
			hash.Write([]byte(name))
			hash.Write([]byte{':'})
			hash.Write([]byte(value))
			hash.Write([]byte{0})
		}
	}
	hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}
