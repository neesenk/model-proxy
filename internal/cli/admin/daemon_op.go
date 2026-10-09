package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"model-proxy/internal/daemonctl"
	"model-proxy/internal/display"
)

// daemon_op.go holds the shared daemon-call helper for the health/pin command
// family: POST a small JSON body, surface the daemon's message on non-200,
// decode the 200 payload.

// postProviderOp posts a {"provider": provider} JSON body to the given daemon
// endpoint and decodes the 200 response into out. A non-200 carries the
// daemon's truncated message as the error. Shared by freeze/unfreeze.
func postProviderOp(base, endpoint, provider string, out any) error {
	raw, _ := json.Marshal(map[string]string{"provider": provider})
	resp, err := daemonctl.PostJSON(base, endpoint, raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s", display.Truncate(strings.TrimSpace(string(rb)), 200))
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("parse daemon response: %v", err)
	}
	return nil
}
