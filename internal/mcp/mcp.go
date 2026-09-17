// Package mcp owns the MCP (Model Context Protocol) wire semantics for the
// /mcp/ gateway: minimal JSON-RPC 2.0 frame parsing (just enough to identify
// methods and response pairing), the bounded session table mapping local
// session ids to upstream ones, header forwarding policy, and the
// initialize + tools/list probe used by the CLI. It is a leaf package with
// zero repository dependencies: all HTTP, credential, logging and lifecycle
// concerns stay with the callers (internal/app, internal/cli/mcp).
package mcp

import (
	"bytes"
	"encoding/json"
)

// Frame is a minimal parse of one JSON-RPC 2.0 message — only what the
// gateway needs: the method (for logging and initialize detection), whether
// the message expects a response, and whether it IS a response. Bodies the
// parse does not understand stay opaque (zero Frame), never an error: the
// gateway is a passthrough and must not reject valid-but-unusual payloads.
type Frame struct {
	Method     string          // request/notification method ("" for responses, batches, opaque bodies)
	ID         json.RawMessage // raw "id" member (nil when absent — notifications lack it)
	HasID      bool            // an "id" member is present (requests and responses; notifications lack it)
	IsResponse bool            // id + result/error — a response, not a request
	IsBatch    bool            // top-level JSON array — forwarded opaquely
}

// ParseFrame extracts the Frame from one JSON-RPC body.
func ParseFrame(body []byte) Frame {
	var f Frame
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return f
	}
	if t[0] == '[' {
		f.IsBatch = true
		return f
	}
	var v struct {
		Method string           `json:"method"`
		ID     *json.RawMessage `json:"id"`
		Result *json.RawMessage `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(t, &v); err != nil {
		return f // opaque: passthrough must not judge
	}
	f.Method = v.Method
	if v.ID != nil {
		f.ID = *v.ID
	}
	f.HasID = v.ID != nil
	f.IsResponse = v.ID != nil && (v.Result != nil || v.Error != nil)
	return f
}

// ExtractSSEData collects the payload of every "data:" line in an
// text/event-stream body. MCP streamable-HTTP servers commonly frame even
// unary POST responses as SSE; the probe uses this to recover the JSON-RPC
// messages. Blank-line handling follows the SSE event model loosely on
// purpose: for parsing we only need the data payloads in order.
func ExtractSSEData(body []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) > 0 {
			out = append(out, payload)
		}
	}
	return out
}
