package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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

func cmdReplay(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err)
		os.Exit(1)
	}
	pos := positionalArgs(args)
	if len(pos) == 0 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy replay <id> --to <provider>\n", cRed("✗"))
		os.Exit(1)
	}
	provider := replayTarget(args)
	if provider == "" {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy replay <id> --to <provider> (--to is required)\n", cRed("✗"))
		os.Exit(1)
	}
	body, err := doReplay("http://"+cfg.Listen, pos[0], provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", cRed("✗"), err)
		os.Exit(1)
	}
	os.Stdout.Write(body)
}

// doReplay fetches the stored request by id, re-sends it to the proxy with a
// force-provider override, and returns the new backend's response body. base is
// "http://<listen>". Errors carry a clear message (404, no body, upstream error).
func doReplay(base, id, provider string) ([]byte, error) {
	recBody, status, err := statusGet(base, "/api/requests/"+id)
	if err != nil {
		return nil, fmt.Errorf("cannot reach daemon: %v", err)
	}
	if status == 404 {
		return nil, fmt.Errorf("no request log for id %s (is request_log.enabled on?)", id)
	}
	if status != 200 {
		return nil, fmt.Errorf("daemon returned HTTP %d: %s", status, truncate(string(recBody), 200))
	}
	var got struct {
		Records []struct {
			Method      string `json:"method"`
			Path        string `json:"path"`
			RequestBody string `json:"request_body"`
		} `json:"records"`
	}
	if err := json.Unmarshal(recBody, &got); err != nil {
		return nil, fmt.Errorf("parse records: %w", err)
	}
	if len(got.Records) == 0 {
		return nil, fmt.Errorf("no record for id %s", id)
	}
	rec := got.Records[0]
	if rec.RequestBody == "" {
		return nil, fmt.Errorf("record %s has no captured request body", id)
	}
	// Refuse to replay a truncated capture: the request_log caps each body at
	// max_body_bytes (appending a truncation marker), so resending it verbatim
	// would send an incomplete request → a misleading upstream 400. Ask the user
	// to raise max_body_bytes instead.
	if strings.HasSuffix(rec.RequestBody, truncMarker) {
		return nil, fmt.Errorf("record %s request body was truncated at request_log.max_body_bytes — replay would send an incomplete request; raise max_body_bytes and recapture", id)
	}
	path := rec.Path
	if path == "" {
		path = "/v1/responses"
	}
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader([]byte(rec.RequestBody)))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-mp-force-provider", provider)
	resp, err := daemonHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("replay request failed: %v", err)
	}
	defer resp.Body.Close()
	fmt.Fprintf(os.Stderr, "%s replay %s → %s (HTTP %d)\n", cDim("•"), id, provider, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s", truncate(strings.TrimSpace(string(body)), 400))
	}
	return body, nil
}

// replayTarget extracts the --to <provider> value from args (--to X or --to=X).
func replayTarget(args []string) string {
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
