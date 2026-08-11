package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/daemonctl"
	displaypkg "model-proxy/internal/provider"
	"net/http"
	"os"
	"strings"

	"model-proxy/internal/observe/requestlog"
)

// replay_cmd.go implements `model-proxy replay <id> --to <provider>` (#12):
// re-answer a previously logged request with a DIFFERENT backend, so you can
// compare answers side-by-side instead of guessing. It fetches the stored
// request (method/path/body) from the daemon's /api/requests/<id>, then re-sends
// it to the proxy with a one-shot x-mp-force-provider override that pins routing
// to the chosen provider for THIS request only (no global pin, no failover
// change for other traffic). The new backend's response is written to stdout.
//
//	replay <id> --to <provider> [--config PATH]
//
// Requires request logging to have captured the original request.

func CmdReplay(args []string, cfg *configdomain.Config) {
	pos := PositionalArgs(args)
	if len(pos) == 0 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy replay <id> --to <provider>\n", displaypkg.Red("✗"))
		os.Exit(1)
	}
	provider := ReplayTarget(args)
	if provider == "" {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy replay <id> --to <provider> (--to is required)\n", displaypkg.Red("✗"))
		os.Exit(1)
	}
	body, err := DoReplay("http://"+cfg.Listen, pos[0], provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", displaypkg.Red("✗"), err)
		os.Exit(1)
	}
	os.Stdout.Write(body)
}

// doReplay fetches the stored request by id, re-sends it to the proxy with a
// force-provider override, and returns the new backend's response body. base is
// "http://<listen>". Errors carry a clear message (404, no body, upstream error).
func DoReplay(base, id, provider string) ([]byte, error) {
	recBody, status, err := daemonctl.Get(base, "/api/requests/"+id)
	if err != nil {
		return nil, fmt.Errorf("cannot reach daemon: %v", err)
	}
	if status == 404 {
		return nil, fmt.Errorf("no request log for id %s (is request_log.enabled on?)", id)
	}
	if status != 200 {
		return nil, fmt.Errorf("daemon returned HTTP %d: %s", status, displaypkg.Truncate(string(recBody), 200))
	}
	var got struct {
		Records []requestlog.Record `json:"records"`
	}
	if err := json.Unmarshal(recBody, &got); err != nil {
		return nil, fmt.Errorf("parse records: %w", err)
	}
	if len(got.Records) == 0 {
		return nil, fmt.Errorf("no record for id %s", id)
	}
	rec := got.Records[0]
	// Guard: shadow records can't be replayed (they're fire-and-forget logs of
	// a candidate backend, not a real client request with a route to re-enter).
	if strings.HasPrefix(rec.RequestID, "shadow-") {
		return nil, fmt.Errorf("record %s is a shadow evaluation record — shadow records cannot be replayed", id)
	}
	// Guard: the path must be a chat-completion path the proxy can forward.
	if !strings.HasPrefix(rec.Path, "/v1/") {
		return nil, fmt.Errorf("record %s path %q is not under /v1/ — cannot replay", id, rec.Path)
	}
	if rec.RequestBody == "" {
		return nil, fmt.Errorf("record %s has no captured request body", id)
	}
	// Refuse to replay a truncated capture: the request_log caps each body at
	// max_body_bytes (appending a truncation marker), so resending it verbatim
	// would send an incomplete request → a misleading upstream 400. Ask the user
	// to raise max_body_bytes instead.
	if rec.RequestBodyTruncated() {
		return nil, fmt.Errorf("record %s request body was truncated at request_log.max_body_bytes — replay would send an incomplete request; raise max_body_bytes and recapture", id)
	}
	path := rec.Path
	if path == "" {
		path = "/v1/responses"
	}
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader([]byte(rec.RequestBody)))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-mp-force-provider", provider)
	resp, err := daemonctl.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("replay request failed: %v", err)
	}
	defer resp.Body.Close()
	fmt.Fprintf(os.Stderr, "%s replay %s → %s (HTTP %d)\n", displaypkg.Dim("•"), id, provider, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s", displaypkg.Truncate(strings.TrimSpace(string(body)), 400))
	}
	return body, nil
}

// replayTarget extracts the --to <provider> value from args (--to X or --to=X).
func ReplayTarget(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--to" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "--to=") {
			return strings.TrimPrefix(a, "--to=")
		}
	}
	return ""
}
