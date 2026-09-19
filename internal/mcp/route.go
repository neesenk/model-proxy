package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// route.go — the pure logic of aggregated MCP routes (mcp_routes:): JSON-RPC
// message building for proxy-synthesized responses, tools/call name
// extraction and rewriting, tools/list splitting and canonical merging. All
// I/O (backend handshakes, HTTP) stays with the caller.

// TargetMapping is one route target's tool-name equivalence: the canonical
// tool name exposed to clients → the backend server's actual tool name.
// Defined here (not in internal/config) so the leaf stays dependency-free;
// the caller adapts config values.
type TargetMapping struct {
	Server string            // mcp: server name backing this target
	Tools  map[string]string // canonical name → backend tool name
}

// ToolSpec is the subset of an MCP tool descriptor the route merges and
// re-exposes. Anything outside these fields (annotations etc.) is dropped —
// canonical tools are a curated surface, not a byte copy.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// BuildResultResponse renders a JSON-RPC result message echoing the client's
// id. id may be nil (notifications are never answered; callers avoid that).
func BuildResultResponse(id json.RawMessage, result json.RawMessage) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	buf.Write(id)
	buf.WriteString(`,"result":`)
	buf.Write(result)
	buf.WriteByte('}')
	return buf.Bytes()
}

// BuildErrorResponse renders a JSON-RPC error message echoing the client's id.
func BuildErrorResponse(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	msg, _ := json.Marshal(message)
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	buf.Write(id)
	fmt.Fprintf(&buf, `,"error":{"code":%d,"message":%s}}`, code, msg)
	return buf.Bytes()
}

// BuildInitializeResult renders the route's own initialize result: the
// protocol version echoes what the client asked for (the proxy negotiates
// with each backend separately), serverInfo names the route.
func BuildInitializeResult(clientProtocol, routeName string) json.RawMessage {
	if clientProtocol == "" {
		clientProtocol = "2025-03-26"
	}
	out := map[string]any{
		"protocolVersion": clientProtocol,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"serverInfo":      map[string]any{"name": "model-proxy route " + routeName, "version": "1.0"},
	}
	data, _ := json.Marshal(out)
	return data
}

// ParseToolCallName extracts params.name from a tools/call body ("" when the
// body is not a well-formed tools/call request).
func ParseToolCallName(body []byte) string {
	var v struct {
		Params *struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Params == nil {
		return ""
	}
	return v.Params.Name
}

// ParseClientProtocol extracts params.protocolVersion from an initialize body.
func ParseClientProtocol(body []byte) string {
	var v struct {
		Params *struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Params == nil {
		return ""
	}
	return v.Params.ProtocolVersion
}

var clientInfoNeedle = []byte(`"clientInfo"`)

// ParseClientInfo extracts params.clientInfo.name from an initialize body —
// the MCP-side client identity (e.g. "codex-mcp-client"). "" when the body
// is not parseable or carries no clientInfo. The name is the raw client
// declaration; mapping it onto agent labels belongs to the observe layer.
func ParseClientInfo(body []byte) string {
	if !bytes.Contains(body, clientInfoNeedle) {
		return ""
	}
	var v struct {
		Params *struct {
			ClientInfo *struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Params == nil || v.Params.ClientInfo == nil {
		return ""
	}
	name := strings.TrimSpace(v.Params.ClientInfo.Name)
	if len(name) > 128 {
		return ""
	}
	return name
}

// RewriteToolCallName returns the tools/call body with params.name replaced
// by backendName, all other members preserved semantically (re-marshaled).
// An invalid body is returned unchanged with an error.
func RewriteToolCallName(body []byte, backendName string) ([]byte, error) {
	var v map[string]json.RawMessage
	if err := json.Unmarshal(body, &v); err != nil {
		return body, err
	}
	var params map[string]json.RawMessage
	if raw, ok := v["params"]; ok {
		if err := json.Unmarshal(raw, &params); err != nil {
			return body, fmt.Errorf("params: %w", err)
		}
	} else {
		params = map[string]json.RawMessage{}
	}
	name, _ := json.Marshal(backendName)
	params["name"] = name
	rawParams, err := json.Marshal(params)
	if err != nil {
		return body, err
	}
	v["params"] = rawParams
	return json.Marshal(v)
}

// ParseToolsListResult extracts the tool specs from one tools/list result
// message (the payload of a JSON-RPC result, i.e. the "result" member or a
// full response message — both accepted).
func ParseToolsListResult(payload []byte) ([]ToolSpec, error) {
	// Accept a full response message first.
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &env); err == nil && len(env.Result) > 0 {
		payload = env.Result
	}
	var v struct {
		Tools []ToolSpec `json:"tools"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, err
	}
	return v.Tools, nil
}

// MergeCanonicalTools aggregates the per-backend tool lists into the route's
// canonical surface, in target order: for each target mapping, every
// canonical name resolves to that backend's actual tool spec, re-exposed
// under the canonical name. A canonical name already exposed by an earlier
// target is skipped (first target wins — it is the failover head). Canonical
// names whose backend tool is missing from the fetched list are skipped
// (a backend that dropped a tool silently degrades only its own target).
// Within a target, canonical names are emitted in sorted order so the
// aggregated tools/list is deterministic.
func MergeCanonicalTools(toolsByServer map[string][]ToolSpec, targets []TargetMapping) []ToolSpec {
	var out []ToolSpec
	exposed := map[string]bool{}
	for _, t := range targets {
		backend := toolsByServer[t.Server]
		byName := make(map[string]ToolSpec, len(backend))
		for _, spec := range backend {
			byName[spec.Name] = spec
		}
		canonicals := make([]string, 0, len(t.Tools))
		for canonical := range t.Tools {
			canonicals = append(canonicals, canonical)
		}
		sort.Strings(canonicals)
		for _, canonical := range canonicals {
			if exposed[canonical] {
				continue
			}
			spec, ok := byName[t.Tools[canonical]]
			if !ok {
				continue
			}
			spec.Name = canonical
			out = append(out, spec)
			exposed[canonical] = true
		}
	}
	return out
}

// FailoverStatus reports whether an upstream HTTP status triggers trying the
// next route target: credential/auth problems (401), invalid sessions (404),
// rate limiting (429) and server faults (5xx). Other statuses — including
// JSON-RPC business errors delivered over 200 — return to the client as-is.
func FailoverStatus(status int) bool {
	return status == 401 || status == 404 || status == 429 || status >= 500
}

// SplitRPCMessages splits one streamable-HTTP response body into JSON-RPC
// message payloads: SSE data frames when event-stream, the whole body
// otherwise. Used by the route handler to read buffered backend responses
// (initialize, tools/list).
func SplitRPCMessages(contentType string, body []byte) [][]byte {
	if bytes.Contains([]byte(contentType), []byte("text/event-stream")) {
		return ExtractSSEData(body)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	return [][]byte{trimmed}
}

// ParseInitializeResult reads protocolVersion/serverInfo from one initialize
// result payload (full response message or bare result, like
// ParseToolsListResult).
func ParseInitializeResult(payload []byte) (protocol, serverName, serverVersion string, err error) {
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err = json.Unmarshal(payload, &env); err == nil && len(env.Result) > 0 {
		payload = env.Result
	}
	var v struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err = json.Unmarshal(payload, &v); err != nil {
		return "", "", "", err
	}
	return v.ProtocolVersion, v.ServerInfo.Name, v.ServerInfo.Version, nil
}
