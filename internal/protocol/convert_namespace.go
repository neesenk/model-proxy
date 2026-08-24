// convert_namespace.go — MCP namespace tool-name flatten/restore (cc-switch
// transform_codex_responses_namespace.rs port). Responses MCP tools carry a
// two-dimensional name {type:"function", name:"read", namespace:"mcp__files__"};
// chat has only a flat ≤64-char name. The responses→chat REQUEST converter
// flattens (namespace+"__"+name, deterministic ≤64 truncation, collision =
// fail-closed error); the chat→responses RESPONSE converter restores the
// original二维 name from a map rebuilt STATELESSLY from the original
// responses request body (cc-switch build_codex_tool_context_from_request —
// no cross-request state).
package protocol

import (
	"fmt"
	"strings"
	"unicode/utf8"

	sonic "github.com/bytedance/sonic"
)

// nsFlatMaxLen is the chat tool-name length cap.
const nsFlatMaxLen = 64

// nsRestore is one flattened tool's original二维 name.
type nsRestore struct {
	Namespace string
	Name      string
}

// nsFlattenName flattens {namespace, name} to namespace+"__"+name truncated
// to ≤64 chars. Deterministic — the truncated flat name is the restore key,
// so flatten and restore always agree. The cut backs off to a rune boundary:
// a raw byte cut could split a multi-byte rune and yield invalid UTF-8.
func nsFlattenName(namespace, name string) string {
	flat := namespace + "__" + name
	if len(flat) > nsFlatMaxLen {
		cut := nsFlatMaxLen
		for cut > 0 && !utf8.RuneStart(flat[cut]) {
			cut--
		}
		flat = flat[:cut]
	}
	return flat
}

// responsesNamespaceRestoreMap rebuilds the flat→original mapping from a
// responses request body (top-level tools + input additional_tools items —
// codex 0.145 declares tools ONLY in additional_tools). plain collects the
// names of NON-namespaced function tools in the same walk (a bare echo of one
// of those must never be restored to a namespace). Both return nil/empty when
// no tool carries a namespace — the common case costs nothing.
func responsesNamespaceRestoreMap(reqBody []byte) (m map[string]nsRestore, plain map[string]bool) {
	var src map[string]any
	if err := sonic.Unmarshal(reqBody, &src); err != nil {
		return nil, nil
	}
	for _, e := range nsExpandTools(responsesRequestTools(src)) {
		if strOf(e.tm["type"]) != "function" {
			continue
		}
		name := strOpt(e.tm["name"])
		if name == "" {
			continue
		}
		if e.namespace == "" {
			if plain == nil {
				plain = map[string]bool{}
			}
			plain[name] = true
			continue
		}
		if m == nil {
			m = map[string]nsRestore{}
		}
		m[nsFlattenName(e.namespace, name)] = nsRestore{Namespace: e.namespace, Name: name}
	}
	return m, plain
}

// r2cCtx is the responses→chat conversion context, rebuilt STATELESSLY from
// the original responses request body (cc-switch build context from request):
// the MCP namespace restore map, the plain (non-namespaced) tool name set,
// and the custom/freeform tool name set.
type r2cCtx struct {
	ns     map[string]nsRestore
	plain  map[string]bool
	custom map[string]bool
}

// r2cCtxFor builds the context when the client speaks responses and the
// backend speaks chat or Anthropic — the directions where names are flattened.
// origBody is the ORIGINAL client request (responses protocol); zero value
// (nil maps) otherwise, which makes every consumer a no-op.
func r2cCtxFor(clientProto, backendProto string, origBody []byte) r2cCtx {
	if clientProto != "responses" || (backendProto != "openai" && backendProto != "anthropic") || len(origBody) == 0 {
		return r2cCtx{}
	}
	ns, plain := responsesNamespaceRestoreMap(origBody)
	return r2cCtx{ns: ns, plain: plain, custom: responsesCustomToolSet(origBody)}
}

// nsRestoreName looks up a chat tool name in the restore map; ok=false (and
// the name passes through unchanged) when the map is nil or has no entry.
func nsRestoreName(nsMap map[string]nsRestore, flat string) (name, namespace string, ok bool) {
	if nsMap == nil {
		return "", "", false
	}
	r, ok := nsMap[flat]
	if !ok {
		return "", "", false
	}
	return r.Name, r.Namespace, true
}

// restoreName is the near-miss-tolerant restore used on the conversion paths:
// an exact flattened-key hit first; otherwise a BARE name (no "__") that names
// exactly ONE declared namespaced tool is restored — a gateway that strips the
// namespace otherwise loses it. Ambiguity (two namespaces own the name, or the
// bare name is itself a declared plain tool) passes the name through unchanged
// — never guess (Switchyard's wrong-guess-dispatch prevention).
func (c r2cCtx) restoreName(flat string) (name, namespace string, ok bool) {
	if name, namespace, ok := nsRestoreName(c.ns, flat); ok {
		return name, namespace, true
	}
	if c.ns == nil || strings.Contains(flat, "__") || c.plain[flat] {
		return "", "", false
	}
	var match nsRestore
	found := 0
	for _, r := range c.ns {
		if r.Name == flat {
			found++
			match = r
		}
	}
	if found != 1 {
		return "", "", false
	}
	return match.Name, match.Namespace, true
}

// nsToolEntry is one effective tool declaration after expanding namespace
// containers: the tool map plus its effective namespace (a field-form
// `namespace`, or the container name for container subtools without one).
type nsToolEntry struct {
	tm        map[string]any
	namespace string
}

// nsExpandTools expands namespace CONTAINERS ({type:"namespace", name,
// tools:[…]}, codex 0.145's additional_tools shape) into per-subtool entries.
// A subtool's own `namespace` field wins over the container name. Nested
// containers surface as entries with type "namespace" so callers can reject
// them fail-closed.
func nsExpandTools(tools []any) []nsToolEntry {
	var out []nsToolEntry
	for _, t := range tools {
		tm := asMap(t)
		if tm == nil {
			continue
		}
		if strOpt(tm["type"]) == "namespace" {
			containerNS := strOpt(tm["name"])
			subs, _ := tm["tools"].([]any)
			for _, s := range subs {
				sm := asMap(s)
				if sm == nil {
					continue
				}
				out = append(out, nsToolEntry{tm: sm, namespace: firstNonEmpty(strOpt(sm["namespace"]), containerNS)})
			}
			continue
		}
		out = append(out, nsToolEntry{tm: tm, namespace: strOpt(tm["namespace"])})
	}
	return out
}

// nsFlattenResponsesTools flattens namespaced function tools to chat shape
// and wraps custom/freeform tools as one-parameter functions. Namespace
// containers are expanded first (nsExpandTools) so container and field-form
// declarations flatten to the SAME chat names and share the response-side
// restore map. A flattened name colliding with ANY other tool name in the
// list (plain, wrapped, or already flattened) is a fail-closed error —
// silently serving a wrong-name tool is worse than a 502.
func nsFlattenResponsesTools(d *Diagnostics, tools []any) ([]map[string]any, error) {
	var out []map[string]any
	seen := map[string]bool{}
	for _, e := range nsExpandTools(tools) {
		tm := e.tm
		switch ty := strOpt(tm["type"]); ty {
		case "custom":
			// Custom/freeform tool (codex shell/apply_patch): wrap as a
			// one-parameter function ({input: string}); never namespaced.
			name := strOpt(tm["name"])
			if name == "" {
				continue
			}
			if seen[name] {
				return nil, fmt.Errorf("tool name %q collides after MCP namespace flattening", name)
			}
			seen[name] = true
			out = append(out, wrapCustomTool(tm))
		case "namespace":
			// A namespace type at this point is a NESTED container (the outer
			// was already expanded) — illegal, fail closed.
			return nil, fmt.Errorf("nested namespace tool container %q is not supported", strOpt(tm["name"]))
		case "function":
			name := strOpt(tm["name"])
			if e.namespace != "" {
				name = nsFlattenName(e.namespace, name)
			}
			if seen[name] {
				return nil, fmt.Errorf("tool name %q collides after MCP namespace flattening", name)
			}
			seen[name] = true
			fn := map[string]any{"name": name}
			if d := tm["description"]; d != nil {
				fn["description"] = d
			}
			if p := tm["parameters"]; p != nil {
				fn["parameters"] = p
			}
			if s, ok := tm["strict"]; ok {
				fn["strict"] = s
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		case "web_search", "web_search_preview", "tool_search":
			name := "web_search"
			if ty == "tool_search" {
				name = "tool_search"
			}
			if seen[name] {
				return nil, fmt.Errorf("tool name %q collides after hosted-tool fallback", name)
			}
			seen[name] = true
			out = append(out, map[string]any{"type": "function", "function": map[string]any{
				"name": name, "description": hostedToolDescription(name), "parameters": hostedToolSchema(name),
			}})
		default:
			// Unknown tool type: dropped, but named in a warning — silently
			// vanishing tools are undebuggable.
			if ty != "" {
				warnDiag(d, "unknown_tool_type", "dropping responses tool of unknown type in r→chat request: "+ty)
			}
		}
	}
	return out, nil
}

func hostedToolDescription(name string) string {
	if name == "tool_search" {
		return "Search for tools that can help complete the request."
	}
	return "Search the web for current information."
}

func hostedToolSchema(name string) map[string]any {
	if name == "tool_search" {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":   map[string]any{"type": "string"},
			"queries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"additionalProperties": false,
	}
}
