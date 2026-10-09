// Package daemonctl owns the CLI-side HTTP client used to talk to a running
// model-proxy daemon. Timeouts and transport policy live here so command
// packages never hand-roll their own client.
package daemonctl

import (
	"bytes"
	"io"
	"net/http"
	"time"
)

// Client is the shared daemon HTTP client. Long-poll endpoints (live events,
// logs tail) are excluded from the timeout by callers that stream.
var Client = &http.Client{Timeout: 10 * time.Second}

// Get fetches base+path and returns the raw body and status code.
func Get(base, path string) (body []byte, status int, err error) {
	return Do(http.MethodGet, base, path)
}

// PostJSON posts a JSON body to base+path with the path's surface credential
// attached, returning the raw response (caller closes the body).
func PostJSON(base, path string, body []byte) (*http.Response, error) {
	req, err := NewRequest(http.MethodPost, base, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	return Client.Do(req)
}

// DoRaw executes a pre-built request whose auth the caller wants handled the
// same way as Do/Get (thin wrapper for DELETE/query-string call sites).
func DoRaw(req *http.Request) (*http.Response, error) {
	Authorize(req)
	return Client.Do(req)
}

// NewGet builds an authenticated GET request for base+path (query strings
// ride inside path) for callers that need the *http.Response stream.
func NewGet(base, path string) (*http.Request, error) {
	return NewRequest(http.MethodGet, base, path, nil)
}

// Do performs one request against base+path and returns the raw body and
// status code. A non-2xx status is NOT an error here — the caller inspects
// it. Callers needing headers or a body use NewRequest + Client directly.
// The path's S2 surface credential is attached automatically when
// SetAuthFiles configured one (non-loopback deployments).
func Do(method, base, path string) (body []byte, status int, err error) {
	req, err := NewRequest(method, base, path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}
