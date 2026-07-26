// convert_namespace.go — MCP namespace tool-name flatten/restore (cc-switch
// transform_codex_responses_namespace.rs port). Responses MCP tools carry a
// two-dimensional name {type:"function", name:"read", namespace:"mcp__files__"};
// chat has only a flat ≤64-char name. The responses→chat REQUEST converter
// flattens (namespace+"__"+name, deterministic ≤64 truncation, collision =
// fail-closed error); the chat→responses RESPONSE converter restores the
// original二维 name from a map rebuilt STATELESSLY from the original
// responses request body (cc-switch build_codex_tool_context_from_request —
// no cross-request state).
package main

import (
	"fmt"

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
// so flatten and restore always agree.
func nsFlattenName(namespace, name string) string {
	flat := namespace + "__" + name
	if len(flat) > nsFlatMaxLen {
		flat = flat[:nsFlatMaxLen]
	}
	return flat
}

// responsesNamespaceRestoreMap rebuilds the flat→original mapping from a
// responses request body (top-level tools + input additional_tools items —
// codex 0.145 declares tools ONLY in additional_tools). Returns nil when no
// tool carries a namespace — the common case costs nothing.
func responsesNamespaceRestoreMap(reqBody []byte) map[string]nsRestore {
	var src map[string]any
	if err := sonic.Unmarshal(reqBody, &src); err != nil {
		return nil
	}
	var out map[string]nsRestore
	for _, e := range nsExpandTools(responsesRequestTools(src)) {
		if strOf(e.tm["type"]) != "function" || e.namespace == "" {
			continue
		}
		name := strOpt(e.tm["name"])
		if name == "" {
			continue
		}
		if out == nil {
			out = map[string]nsRestore{}
		}
		out[nsFlattenName(e.namespace, name)] = nsRestore{Namespace: e.namespace, Name: name}
	}
	return out
}

// r2cCtx is the responses→chat conversion context, rebuilt STATELESSLY from
// the original responses request body (cc-switch build context from request):
// the MCP namespace restore map plus the custom/freeform tool name set.
type r2cCtx struct {
	ns     map[string]nsRestore
	custom map[string]bool
}

// r2cCtxFor builds the context when the client speaks responses and the
// backend speaks chat — the only direction where names are flattened/wrapped.
// origBody is the ORIGINAL client request (responses protocol); zero value
// (nil maps) otherwise, which makes every consumer a no-op.
func r2cCtxFor(clientProto, backendProto string, origBody []byte) r2cCtx {
	if clientProto != "responses" || backendProto != "openai" || len(origBody) == 0 {
		return r2cCtx{}
	}
	return r2cCtx{ns: responsesNamespaceRestoreMap(origBody), custom: responsesCustomToolSet(origBody)}
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
func nsFlattenResponsesTools(tools []any) ([]map[string]any, error) {
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
		default:
			// Unknown tool type (tool_search, hosted tools): dropped, but
			// named in a warning — silently vanishing tools are undebuggable.
			if ty != "" {
				convertWarn("dropping responses tool of unknown type in r→chat request: " + ty)
			}
		}
	}
	return out, nil
}
