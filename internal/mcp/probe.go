package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeResult reports one initialize + tools/list exchange against an MCP
// endpoint — the `mcp test` CLI output and the daemon-free connectivity check.
type ProbeResult struct {
	ServerName    string   // serverInfo.name from initialize
	ServerVersion string   // serverInfo.version
	Protocol      string   // negotiated protocolVersion
	Sessionful    bool     // upstream issued an Mcp-Session-Id
	Tools         []string // tool names from tools/list
	Latency       time.Duration
}

// probeTimeout bounds the whole probe exchange; the caller's http.Client
// governs per-request transport behavior.
const probeBodyCap = 1 << 20 // 1 MiB — tools/list responses are small by nature

// Probe runs the MCP handshake against url: initialize → (optional)
// notifications/initialized → tools/list. auth injects credentials onto each
// outbound request (nil for anonymous endpoints). It works against both
// JSON and SSE-framed streamable-HTTP responses.
func Probe(ctx context.Context, client *http.Client, url string, auth func(*http.Request) error) (*ProbeResult, error) {
	start := time.Now()
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"model-proxy-probe","version":"1.0"}}}`)
	resp, err := doRPC(ctx, client, url, auth, "", initBody)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	res := &ProbeResult{Latency: time.Since(start)}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") && !strings.Contains(ct, "event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("initialize: unexpected content-type %q", ct)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	res.Sessionful = sessionID != ""
	initMsgs, err := readRPCMessages(resp)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var initOK bool
	for _, msg := range initMsgs {
		var v struct {
			Result *struct {
				ProtocolVersion string `json:"protocolVersion"`
				ServerInfo      struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"serverInfo"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &v) != nil {
			continue
		}
		if v.Error != nil {
			return nil, fmt.Errorf("initialize: RPC error: %s", v.Error.Message)
		}
		if v.Result != nil {
			res.Protocol = v.Result.ProtocolVersion
			res.ServerName = v.Result.ServerInfo.Name
			res.ServerVersion = v.Result.ServerInfo.Version
			initOK = true
			break
		}
	}
	if !initOK {
		return nil, fmt.Errorf("initialize: no result in response")
	}
	// Stateful servers expect the initialized notification before other calls;
	// stateless ones tolerate it (202 or 200, either is fine — never fatal).
	notif := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if nresp, err := doRPC(ctx, client, url, auth, sessionID, notif); err == nil {
		io.Copy(io.Discard, io.LimitReader(nresp.Body, probeBodyCap))
		nresp.Body.Close()
	}
	listBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	lresp, err := doRPC(ctx, client, url, auth, sessionID, listBody)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	lmsgs, err := readRPCMessages(lresp)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var listOK bool
	for _, msg := range lmsgs {
		var v struct {
			Result *struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &v) != nil {
			continue
		}
		if v.Error != nil {
			return nil, fmt.Errorf("tools/list: RPC error: %s", v.Error.Message)
		}
		if v.Result != nil {
			for _, t := range v.Result.Tools {
				res.Tools = append(res.Tools, t.Name)
			}
			listOK = true
			break
		}
	}
	if !listOK {
		return nil, fmt.Errorf("tools/list: no result in response")
	}
	res.Latency = time.Since(start)
	return res, nil
}

// doRPC sends one JSON-RPC POST with the MCP accept header and optional
// upstream session id, returning the response for the caller to drain.
func doRPC(ctx context.Context, client *http.Client, url string, auth func(*http.Request) error, sessionID string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if auth != nil {
		if err := auth(req); err != nil {
			return nil, err
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// readRPCMessages drains a response body and splits it into JSON-RPC message
// payloads — one message for plain JSON, N data-frame payloads for SSE.
func readRPCMessages(resp *http.Response) ([][]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, probeBodyCap))
	if err != nil {
		return nil, err
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return ExtractSSEData(body), nil
	}
	return [][]byte{bytes.TrimSpace(body)}, nil
}
