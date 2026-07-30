// Package daemonctl owns the CLI-side HTTP client used to talk to a running
// model-proxy daemon. Timeouts and transport policy live here so command
// packages never hand-roll their own client.
package daemonctl

import (
	"io"
	"net/http"
	"time"
)

// Client is the shared daemon HTTP client. Long-poll endpoints (live events,
// logs tail) are excluded from the timeout by callers that stream.
var Client = &http.Client{Timeout: 10 * time.Second}

// Get fetches base+path and returns the raw body and status code.
func Get(base, path string) (body []byte, status int, err error) {
	resp, err := Client.Get(base + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}
