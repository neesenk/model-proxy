package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/observe/logx"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sonic "github.com/bytedance/sonic"
)

// convert.go implements protocol-specific codecs used by the typed registry in
// conversion_registry.go. Clients and upstreams may independently speak
// Anthropic Messages, OpenAI Chat Completions, or OpenAI Responses, with full
// request/response/streaming conversion and tool-call support.
//
// The converters are pure (testable in isolation); the forward path only invokes
// them when a target's declared protocol differs from the client's, so the
// default same-protocol path stays byte-identical.
//
// Out of scope for the anthropic↔chat pair (dropped + warned, never silently):
// anthropic thinking/redacted_thinking blocks (chat reasoning_content DOES map
// back to thinking), cache_control breakpoints, server-side tools
// (web_search/computer/...). Images inside tool_result are NOT dropped — they
// are reinjected via a synthetic user message (media reinjection). Refusals
// map to plain text blocks. logprobs is dropped silently (debug-only field).
// Claude Code's client tools (bash/edit/...) are ordinary function tools and
// convert normally.

const toolResultErrorMarker = "[cc-switch:tool-result-error]"

func markToolResultError(text string, isError bool) string {
	if !isError {
		return text
	}
	if text == "" {
		return toolResultErrorMarker
	}
	return toolResultErrorMarker + "\n" + text
}

func splitToolResultError(text string) (string, bool) {
	if text == toolResultErrorMarker {
		return "", true
	}
	if strings.HasPrefix(text, toolResultErrorMarker+"\n") {
		return strings.TrimPrefix(text, toolResultErrorMarker+"\n"), true
	}
	return text, false
}

// needsConversion reports whether a client protocol and a target's declared
// backend protocol differ (and thus conversion applies). Any two distinct
// protocols among {anthropic, openai-chat, responses} need conversion; either
// empty = "same as client" (no conversion). Unknown protocols (shouldn't occur —
// validate rejects them) are treated as non-convertible (passthrough) so a bad
// value never triggers a no-op conversion path.
func needsConversion(clientProto, targetProto string) bool {
	if targetProto == "" || targetProto == clientProto {
		return false
	}
	_, clientOK := parseWireProtocol(clientProto)
	_, targetOK := parseWireProtocol(targetProto)
	return clientOK && targetOK
}

// convertWarn logs a conversion warning once per process per message (rate-limited
// dedup) so a flood of identical warnings doesn't spam the log, but the operator
// still sees each distinct dropped/unmappable field at least once. The dedup set
// is capped: warning strings embed client-/upstream-controlled values (raw file
// ids, unknown type names, scanner errors), so an unbounded map would grow with
// request content. Past the cap new messages are simply not deduped — dedup is
// best-effort log-spam control, never correctness.
const convertWarnSeenCap = 1024

var convertWarnSeen = &convertWarnDedup{seen: make(map[string]struct{})}

type convertWarnDedup struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// mark reports whether msg was already deduped. capped reports whether the
// dedup set is full (msg is then NOT remembered — every occurrence logs).
func (w *convertWarnDedup) mark(msg string) (dup, capped bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen[msg]; ok {
		return true, false
	}
	if len(w.seen) >= convertWarnSeenCap {
		return false, true
	}
	w.seen[msg] = struct{}{}
	return false, false
}

func convertWarn(msg string) {
	dup, capped := convertWarnSeen.mark(msg)
	if dup {
		return
	}
	if capped {
		logx.Warnf("[convert] WARN: %s", msg)
		return
	}
	logx.Warnf("[convert] WARN: %s (suppressed further occurrences)", msg)
}

// asMap type-asserts v to map[string]any, returning nil if it isn't one.
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// strOf returns v as a string (best-effort for JSON strings/numbers/bools).
func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := sonic.Marshal(v)
	return strings.Trim(string(b), `"`)
}

// --- request: anthropic → openai ---

// anthropicTextOf extracts concatenated text from an anthropic system or
// tool_result content value (string, or array of text blocks). Non-text blocks
// are warned + skipped (system/tool_result are inherently text-shaped).
func anthropicTextOf(content any, d *Diagnostics) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, blk := range v {
			m := asMap(blk)
			if m == nil {
				continue
			}
			if _, hasCC := m["cache_control"]; hasCC {
				warnDiag(d, "cache_control_dropped", "dropping cache_control breakpoint (no cross-protocol equivalent)")
			}
			t, _ := m["type"].(string)
			if t == "text" || t == "" {
				if s, _ := m["text"].(string); s != "" {
					if b.Len() > 0 {
						b.WriteString("\n") // blocks join with "\n", not verbatim concat
					}
					b.WriteString(s)
				}
			} else {
				warnDiag(d, "non_text_block_dropped", "dropping non-text block in system/tool_result: "+t)
			}
		}
		return b.String()
	}
	return ""
}

// anthropicToolsToOpenAI maps anthropic tools to openai function tools. Built-in
// server tools (web_search_*/computer/bash/text_editor/...) carry a `type` other
// than the custom-tool shape and are dropped + warned.
func anthropicToolsToOpenAI(tools []any, d *Diagnostics) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		t := asMap(tool)
		if t == nil {
			continue
		}
		// Preserve hosted web search as a callable fallback on Chat backends.
		// "custom" is Anthropic's explicit default type for regular function
		// tools (name+input_schema) and falls through to the normal mapping.
		if bt, ok := t["type"].(string); ok && bt != "" && bt != "custom" {
			if strings.HasPrefix(bt, "web_search") {
				fn := map[string]any{
					"name":        "web_search",
					"description": "Search the web for current information.",
					"parameters":  hostedToolSchema("web_search"),
				}
				out = append(out, map[string]any{"type": "function", "function": fn})
				continue
			}
			warnDiag(d, "server_tool_dropped", "dropping server-side anthropic tool type: "+bt)
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d, ok := t["description"]; ok {
			fn["description"] = d
		}
		if schema, ok := t["input_schema"]; ok {
			fn["parameters"] = schema
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// anthropicToolChoiceToOpenAI maps an anthropic tool_choice to its openai form.
// Returns nil for unmappable values (caller omits the field).
func anthropicToolChoiceToOpenAI(tc any) any {
	m := asMap(tc)
	if m == nil {
		return nil
	}
	switch m["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if name, ok := m["name"].(string); ok && name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	case "none":
		return "none"
	}
	return nil
}

// anthropicContentBlockToOpenAIPart maps a single anthropic content block to an
// openai content part. Returns nil for tool_use/tool_result (handled at message
// level) and for dropped blocks (thinking/redacted_thinking).
func anthropicContentBlockToOpenAIPart(blk map[string]any, d *Diagnostics) map[string]any {
	if _, hasCC := blk["cache_control"]; hasCC {
		warnDiag(d, "cache_control_dropped", "dropping cache_control breakpoint (no cross-protocol equivalent)")
	}
	switch blk["type"] {
	case "text", "":
		return map[string]any{"type": "text", "text": strOf(blk["text"])}
	case "image":
		src := asMap(blk["source"])
		if src == nil {
			return nil
		}
		if st, _ := src["type"].(string); st == "base64" {
			mt, _ := src["media_type"].(string)
			data, _ := src["data"].(string)
			return map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:" + mt + ";base64," + data,
			}}
		}
		if u, ok := src["url"].(string); ok && u != "" {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}
		}
		return nil
	case "document":
		src := asMap(blk["source"])
		if src == nil {
			return nil
		}
		filename := firstNonEmpty(strOpt(blk["title"]), strOpt(blk["filename"]), "document.pdf")
		switch strOpt(src["type"]) {
		case "base64":
			data := strOpt(src["data"])
			if data == "" {
				return nil
			}
			mediaType := firstNonEmpty(strOpt(src["media_type"]), "application/pdf")
			return map[string]any{"type": "file", "file": map[string]any{
				"filename":  filename,
				"file_data": "data:" + mediaType + ";base64," + data,
			}}
		case "file":
			if id := strOpt(src["file_id"]); id != "" {
				return map[string]any{"type": "text", "text": degradeFileIDText(id, filename, d)}
			}
		case "url":
			if u := strOpt(src["url"]); u != "" {
				// Chat Completions has no URL-backed file part. Preserve the
				// reference as text instead of silently dropping the document.
				return map[string]any{"type": "text", "text": "[document " + filename + "] " + u}
			}
		case "text":
			if data := strOpt(src["data"]); data != "" {
				return map[string]any{"type": "text", "text": "[document " + filename + "]\n" + data}
			}
		}
		return nil
	case "tool_use", "tool_result":
		return nil // handled at message level
	case "thinking", "redacted_thinking":
		warnDiag(d, "block_dropped", "dropping "+strOf(blk["type"])+" block (no cross-protocol equivalent)")
		return nil
	}
	warnDiag(d, "unknown_block", "dropping unknown anthropic content block: "+strOf(blk["type"]))
	return nil
}

// anthropicMsgToOpenAIMsgs converts one anthropic message to one or more openai
// messages. A user message carrying tool_result blocks expands to separate openai
// `tool` messages (one per result) plus a `user` message for any text/image.
// imageOK gates the media reinjection (#6): without vision the images collapse
// to a placeholder line inside the tool message instead of image_url parts.
func anthropicMsgToOpenAIMsgs(m map[string]any, imageOK bool, d *Diagnostics) []map[string]any {
	role, _ := m["role"].(string)
	content := m["content"]
	var out []map[string]any

	if role == "assistant" {
		om := map[string]any{"role": "assistant"}
		var textParts []map[string]any
		var toolCalls []map[string]any
		var hostedResults []map[string]any
		var annotations []map[string]any
		var reasoningParts []string
		textOffset := 0
		if blocks, ok := content.([]any); ok {
			for _, b := range blocks {
				blk := asMap(b)
				if blk == nil {
					continue
				}
				if blk["type"] == "tool_use" {
					id, _ := blk["id"].(string)
					name, _ := blk["name"].(string)
					args, _ := sonic.Marshal(blk["input"]) // openai arguments is a JSON string
					toolCalls = append(toolCalls, map[string]any{
						"id": id, "type": "function",
						"function": map[string]any{"name": name, "arguments": string(args)},
					})
				} else if blk["type"] == "server_tool_use" && strOpt(blk["name"]) == "web_search" {
					args, _ := sonic.Marshal(blk["input"])
					toolCalls = append(toolCalls, map[string]any{
						"id": strOpt(blk["id"]), "type": "function",
						"function": map[string]any{"name": "web_search", "arguments": string(args)},
					})
				} else if blk["type"] == "web_search_tool_result" {
					data, _ := sonic.Marshal(blk["content"])
					hostedResults = append(hostedResults, map[string]any{
						"role": "tool", "tool_call_id": strOpt(blk["tool_use_id"]), "content": string(data),
					})
				} else if blk["type"] == "thinking" {
					if thinking := strOpt(blk["thinking"]); thinking != "" {
						reasoningParts = append(reasoningParts, thinking)
					}
				} else if blk["type"] == "redacted_thinking" {
					// Opaque Anthropic payloads have no Chat equivalent. The
					// Anthropic client still owns and replays the original
					// block on its next request.
				} else {
					if blk["type"] == "text" {
						text := strOf(blk["text"])
						annotations = append(annotations, anthropicCitationsToChat(blk["citations"], text, textOffset)...)
						textOffset += len([]rune(text))
					}
					if p := anthropicContentBlockToOpenAIPart(blk, d); p != nil {
						textParts = append(textParts, p)
					}
				}
			}
		} else if s, ok := content.(string); ok && s != "" {
			om["content"] = s
		}
		if len(textParts) == 1 {
			// Simplify a lone part to a bare string only when it IS a text part;
			// a single image (etc.) part has no `text` key — keep the array,
			// otherwise the content collapses to nil and the part is dropped.
			if txt, ok := textParts[0]["text"]; ok {
				om["content"] = txt
			} else {
				om["content"] = textParts
			}
		} else if len(textParts) > 1 {
			om["content"] = textParts
		} else if len(toolCalls) > 0 && len(textParts) == 0 {
			om["content"] = nil // assistant turn is all tool calls
		}
		if len(toolCalls) > 0 {
			om["tool_calls"] = toolCalls
		}
		if len(annotations) > 0 {
			om["annotations"] = annotations
		}
		if len(reasoningParts) > 0 {
			om["reasoning_content"] = strings.Join(reasoningParts, "\n\n")
		}
		return append([]map[string]any{om}, hostedResults...)
	}

	// user (and any other role): split tool_result blocks from text/image parts.
	if blocks, ok := content.([]any); ok {
		var parts []map[string]any
		for _, b := range blocks {
			blk := asMap(b)
			if blk == nil {
				continue
			}
			if blk["type"] == "tool_result" {
				id, _ := blk["tool_use_id"].(string)
				txt := markToolResultError(anthropicToolResultText(blk["content"], d), blk["is_error"] == true)
				imgs := anthropicToolResultImages(blk["content"], d)
				if len(imgs) > 0 && !imageOK {
					// No vision on the target: no synthetic user message, no
					// image_url parts (deepseek 400s on them) — placeholder text.
					warnDiagf(d, "media_degraded",
						"target has no vision: %d tool-result image(s) collapsed to placeholder text", len(imgs))
					txt = appendMediaPlaceholder(txt)
					imgs = nil
				}
				out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": txt})
				// Media reinjection (cc-switch): images inside tool_result are
				// re-delivered as a synthetic user message right after the tool
				// message, instead of being dropped (a pure-image tool_result
				// would otherwise become content:"" → tool loops).
				if len(imgs) > 0 {
					synth := []map[string]any{{"type": "text", "text": "[image returned by tool]"}}
					synth = append(synth, imgs...)
					out = append(out, map[string]any{"role": "user", "content": synth})
				}
				continue
			}
			if p := anthropicContentBlockToOpenAIPart(blk, d); p != nil {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			um := map[string]any{"role": "user"}
			if len(parts) == 1 {
				// Same lone-part rule as the assistant branch: only a text part
				// simplifies to a string; a single image part keeps the array.
				if txt, ok := parts[0]["text"]; ok {
					um["content"] = txt
				} else {
					um["content"] = parts
				}
			} else {
				um["content"] = parts
			}
			out = append(out, um)
		}
	} else if s, ok := content.(string); ok {
		out = append(out, map[string]any{"role": role, "content": s})
	}
	return out
}

// anthropicToolResultText extracts the text of a tool_result content (string or
// text blocks). Image blocks are skipped WITHOUT a warning — they are
// re-delivered via anthropicToolResultImages (synthetic user message); other
// non-text blocks are warned + dropped.
func anthropicToolResultText(content any, d *Diagnostics) string {
	if s, ok := content.(string); ok {
		return s
	}
	if blocks, ok := content.([]any); ok {
		var b strings.Builder
		for _, blk := range blocks {
			m := asMap(blk)
			if m == nil {
				continue
			}
			t, _ := m["type"].(string)
			if t == "text" || t == "" {
				if s, _ := m["text"].(string); s != "" {
					b.WriteString(s)
				}
			} else if t == "image" {
				continue // reinjected, not dropped
			} else {
				warnDiag(d, "non_text_block_dropped", "dropping non-text block inside tool_result: "+t)
			}
		}
		return b.String()
	}
	return ""
}

// anthropicToolResultImages extracts image blocks from a tool_result content
// value as openai image_url parts (base64 → data URL, url source kept) for
// synthetic-user reinjection.
func anthropicToolResultImages(content any, d *Diagnostics) []map[string]any {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, blk := range blocks {
		m := asMap(blk)
		if m == nil || m["type"] != "image" {
			continue
		}
		if p := anthropicContentBlockToOpenAIPart(m, d); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// mediaOmittedPlaceholder replaces reinjected images when the target model
// has no vision capability (#6: deepseek 400s "unknown variant" on image_url).
const mediaOmittedPlaceholder = "[image omitted: target model has no vision capability]"

// appendMediaPlaceholder folds the no-vision placeholder into a tool
// message's text content (it IS the whole content when the result had no text).
func appendMediaPlaceholder(txt string) string {
	if txt == "" {
		return mediaOmittedPlaceholder
	}
	return txt + "\n" + mediaOmittedPlaceholder
}

// convertAnthropicRequestToOpenAI transforms an Anthropic /v1/messages body into
// an OpenAI /v1/chat/completions body (full tools support).
func convertAnthropicRequestToOpenAI(body []byte) ([]byte, error) {
	return convertAnthropicRequestToOpenAIV(body, true, nil)
}

// convertAnthropicRequestToOpenAIV is convertAnthropicRequestToOpenAI with the
// target model's vision capability (media reinjection gate).
func convertAnthropicRequestToOpenAIV(body []byte, imageOK bool, d *Diagnostics) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	out := map[string]any{}
	for _, k := range []string{"model", "max_tokens", "temperature", "top_p"} {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	if stream, ok := src["stream"]; ok {
		out["stream"] = stream
		if b, ok := stream.(bool); ok && b {
			out["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	var msgs []map[string]any
	if sys, ok := src["system"]; ok {
		if txt := anthropicTextOf(sys, d); txt != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": txt})
		}
	}
	if raw, ok := src["messages"].([]any); ok {
		for _, m := range raw {
			if mm := asMap(m); mm != nil {
				msgs = append(msgs, anthropicMsgToOpenAIMsgs(mm, imageOK, d)...)
			}
		}
	}
	out["messages"] = msgs
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		if ot := anthropicToolsToOpenAI(tools, d); len(ot) > 0 {
			out["tools"] = ot
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if oc := anthropicToolChoiceToOpenAI(tc); oc != nil {
			out["tool_choice"] = oc
		}
		// disable_parallel_tool_use:true → parallel_tool_calls:false.
		if tcm := asMap(tc); tcm != nil {
			if dis, _ := tcm["disable_parallel_tool_use"].(bool); dis {
				out["parallel_tool_calls"] = false
			}
		}
	}
	if stops, ok := src["stop_sequences"].([]any); ok && len(stops) > 0 {
		out["stop"] = stops
	}
	if key := anthropicExplicitPromptCacheKey(src, out, d); key != "" {
		out["prompt_cache_key"] = key
	}
	return sonic.Marshal(out)
}

// --- request: openai → anthropic ---

// openaiToolsToAnthropic maps openai function tools to anthropic tools.
func openaiToolsToAnthropic(tools []any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		t := asMap(tool)
		if t == nil {
			continue
		}
		fn := asMap(t["function"])
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		at := map[string]any{"name": name}
		if d, ok := fn["description"]; ok {
			at["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			at["input_schema"] = normalizeAnthropicInputSchema(p)
		} else {
			at["input_schema"] = normalizeAnthropicInputSchema(nil)
		}
		out = append(out, at)
	}
	return out
}

// normalizeAnthropicInputSchema enforces Anthropic's root-object constraint.
// Root oneOf/anyOf/allOf compositions are flattened while nested schemas stay
// intact. The transport-only `encrypted` marker is removed from schema keyword
// positions but properties literally named "encrypted" are preserved.
func normalizeAnthropicInputSchema(schema any) map[string]any {
	clean, _ := stripSchemaEncryptedMarker(schema, false).(map[string]any)
	if clean == nil {
		clean = map[string]any{}
	}
	composition := false
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if _, ok := clean[key].([]any); ok {
			composition = true
			break
		}
	}
	if !composition {
		clean["type"] = "object"
		if clean["properties"] == nil {
			clean["properties"] = map[string]any{}
		}
		return clean
	}
	properties := map[string]any{}
	if p := asMap(clean["properties"]); p != nil {
		for k, v := range p {
			properties[k] = v
		}
	}
	required := map[string]bool{}
	for _, raw := range anySlice(clean["required"]) {
		if name, ok := raw.(string); ok {
			required[name] = true
		}
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		for _, raw := range anySlice(clean[key]) {
			branch := asMap(raw)
			for name, value := range asMap(branch["properties"]) {
				properties[name] = value
			}
			if key == "allOf" {
				for _, req := range anySlice(branch["required"]) {
					if name, ok := req.(string); ok {
						required[name] = true
					}
				}
			}
		}
		delete(clean, key)
	}
	clean["type"] = "object"
	clean["properties"] = properties
	delete(clean, "required")
	if len(required) > 0 {
		names := make([]string, 0, len(required))
		for name := range required {
			names = append(names, name)
		}
		sort.Strings(names)
		clean["required"] = names
	}
	return clean
}

func stripSchemaEncryptedMarker(value any, inNameBag bool) any {
	switch node := value.(type) {
	case []any:
		out := make([]any, len(node))
		for i := range node {
			out[i] = stripSchemaEncryptedMarker(node[i], false)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, child := range node {
			if !inNameBag && key == "encrypted" {
				continue
			}
			nameBag := key == "properties" || key == "patternProperties" || key == "$defs" || key == "definitions"
			literal := key == "const" || key == "default" || key == "enum" || key == "examples"
			if literal {
				out[key] = child
			} else {
				out[key] = stripSchemaEncryptedMarker(child, nameBag)
			}
		}
		return out
	default:
		return value
	}
}

// openaiToolChoiceToAnthropic maps an openai tool_choice to anthropic form.
func openaiToolChoiceToAnthropic(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
	case map[string]any:
		fn := asMap(v["function"])
		if fn != nil {
			if name, ok := fn["name"].(string); ok && name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// warnFileIDDropped emits the shared diagnostic for a dropped cross-protocol
// file_id. A file_id is scoped to the provider it was uploaded to; forwarding
// it through a protocol conversion (almost always a provider change in this
// topology) would send the target an id its file storage has never seen.
// Inline base64/URL sources are unaffected; the drop is observable
// (convertWarn + Diagnostics), never silent.
func warnFileIDDropped(id string, d *Diagnostics) {
	warnDiag(d, "file_id_degraded", "dropping cross-protocol file_id attachment "+id+" (provider-scoped; inline the file content instead)")
}

// degradeFileIDText renders the note replacing a file-id-only attachment on
// cross-protocol conversion (the whole attachment degrades when no inline
// base64/URL source exists to carry it).
func degradeFileIDText(id, filename string, d *Diagnostics) string {
	warnFileIDDropped(id, d)
	return "[document " + firstNonEmpty(filename, "file") + " attached as file_id " + id + " — not forwarded across providers]"
}

// openaiContentPartToAnthropicBlock maps an openai content part to an anthropic
// content block (text or image_url→image base64).
func openaiContentPartToAnthropicBlock(part map[string]any, d *Diagnostics) map[string]any {
	switch part["type"] {
	case "text", "":
		// Skip empty text parts (Anthropic rejects empty text blocks,
		// TextBlockParam minLength 1) — same rule as the string-content path.
		if s := strOf(part["text"]); s != "" {
			return map[string]any{"type": "text", "text": s}
		}
		return nil
	case "image_url":
		iu := asMap(part["image_url"])
		if iu == nil {
			return nil
		}
		u, _ := iu["url"].(string)
		if strings.HasPrefix(u, "data:") {
			// data:<media>[;params];base64,<data> — keep only the bare MIME as
			// media_type (parameters like charset would be rejected).
			rest := strings.TrimPrefix(u, "data:")
			semi := strings.Index(rest, ";base64,")
			if semi < 0 {
				// Percent-encoded (non-base64) data URI: no Anthropic source
				// form can carry it — drop observably, not silently.
				warnDiag(d, "data_uri_dropped", "dropping non-base64 data URI image (no Anthropic equivalent)")
				return nil
			}
			media := rest[:semi]
			if i := strings.Index(media, ";"); i >= 0 {
				media = media[:i]
			}
			data := rest[semi+len(";base64,"):]
			return map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": media, "data": data,
			}}
		}
		if u != "" {
			return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
		}
	case "file":
		f := asMap(part["file"])
		if f == nil {
			return nil
		}
		filename := strOpt(f["filename"])
		var block map[string]any
		if id := strOpt(f["file_id"]); id != "" && strOpt(f["file_data"]) == "" {
			block = map[string]any{"type": "text", "text": degradeFileIDText(id, filename, d)}
		} else if dataURL := strOpt(f["file_data"]); dataURL != "" {
			if mt, data, ok := parseDataURL(dataURL); ok {
				block = map[string]any{"type": "document", "source": map[string]any{
					"type": "base64", "media_type": mt, "data": data,
				}}
			}
		}
		if block != nil && filename != "" {
			block["title"] = filename
		}
		return block
	case "refusal":
		// Refusals map to plain text (anthropic has no refusal block type);
		// stop_reason already carries the refusal semantics (cc-switch同款).
		if s, _ := part["refusal"].(string); s != "" {
			return map[string]any{"type": "text", "text": s}
		}
		return nil
	default:
		warnDiag(d, "unknown_part", "dropping unknown openai content part: "+strOf(part["type"]))
	}
	return nil
}

// openaiContentToAnthropicBlocks converts an openai message content (string or
// parts array) into anthropic content blocks.
func openaiContentToAnthropicBlocks(content any, d *Diagnostics) []map[string]any {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": s}}
	}
	parts, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, p := range parts {
		if blk := openaiContentPartToAnthropicBlock(asMap(p), d); blk != nil {
			out = append(out, blk)
		}
	}
	return out
}

// openaiTextOf extracts concatenated text from an openai system/tool message
// content value (string, or array of text parts joined with "\n"). Non-text
// parts are warned + skipped; nil/absent content yields "" — NEVER the literal
// "null" that strOf(nil) would render.
func openaiTextOf(content any, d *Diagnostics) string {
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		m := asMap(p)
		if m == nil {
			continue
		}
		if t, _ := m["type"].(string); t == "text" || t == "" {
			if s, _ := m["text"].(string); s != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		} else {
			warnDiag(d, "non_text_part_dropped", "dropping non-text part in system/tool content: "+t)
		}
	}
	return b.String()
}

// parseToolArgs parses an openai tool_call arguments JSON string into an object;
// on failure returns an empty object + a warning (anthropic tool_use.input must be
// an object).
func parseToolArgs(args string, d *Diagnostics) any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var obj any
	if err := sonic.Unmarshal([]byte(args), &obj); err == nil {
		switch obj.(type) {
		case map[string]any:
			return obj
		case nil:
			return map[string]any{}
		default:
			// Valid JSON but not an object (array/scalar): Anthropic's
			// tool_use input must be an object — wrap so the value survives.
			warnDiag(d, "tool_args_wrapped", "tool_call arguments not a JSON object; wrapped as {value: …}")
			return map[string]any{"value": obj}
		}
	}
	// Not JSON at all: keep the raw text recoverable instead of discarding it.
	warnDiag(d, "tool_args_raw", "tool_call arguments not JSON; wrapping raw text as input")
	return map[string]any{"raw": args}
}

// appendSSEData folds one data:-line payload into the frame in progress:
// consecutive data lines join with "\n" per the SSE spec (a single-line
// frame — the only form LLM vendors emit — passes through unchanged). open
// distinguishes "no frame yet" from a frame whose data lines so far folded to
// "" (a lone empty `data:` line), so empty payloads fold spec-consistently:
// `data:` + `data: x` is "\nx", not "x".
func appendSSEData(pend string, open bool, payload string) string {
	if !open {
		return payload
	}
	return pend + "\n" + payload
}

type parsedFoldedSSEFrame[T any] struct {
	value T
	// line is -1 when the whole folded payload parsed as one spec-compliant
	// frame. For the missing-blank-line fallback it identifies the original
	// data-line index, allowing callers to recover the event paired with that
	// line instead of applying the final event to every recovered frame.
	line int
	// done marks a literal [DONE] data line recovered by the per-line
	// fallback: the terminator must survive the fold instead of being
	// silently dropped as an unparseable line.
	done bool
}

func foldedSSEFrameEvent(frameEvent string, dataEvents []string, line int) string {
	if line >= 0 && line < len(dataEvents) {
		return dataEvents[line]
	}
	return frameEvent
}

// parseFoldedSSEFrames parses a folded SSE data payload into frames. In a
// spec-compliant stream the fold joins only the data lines of ONE frame, so
// the merged payload parses directly. Gateways that omit the blank line
// between frames fold DISTINCT frames together and the merged payload no
// longer parses; in that case retry each folded line as its own frame so the
// frames are not silently dropped, and warn either way (the warn dedup cap
// keeps a persistently malformed stream from flooding diagnostics).
func parseFoldedSSEFrames[T any](payload string) []parsedFoldedSSEFrame[T] {
	var first T
	if sonic.UnmarshalString(payload, &first) == nil {
		return []parsedFoldedSSEFrame[T]{{value: first, line: -1}}
	}
	if !strings.Contains(payload, "\n") {
		convertWarn("dropping unparseable SSE data payload")
		return nil
	}
	var frames []parsedFoldedSSEFrame[T]
	for lineIndex, line := range strings.Split(payload, "\n") {
		if strings.TrimSpace(line) == "[DONE]" {
			frames = append(frames, parsedFoldedSSEFrame[T]{done: true, line: lineIndex})
			continue
		}
		var f T
		if sonic.UnmarshalString(line, &f) == nil {
			frames = append(frames, parsedFoldedSSEFrame[T]{value: f, line: lineIndex})
		}
	}
	if len(frames) > 0 {
		convertWarn(fmt.Sprintf("SSE frames merged by a non-spec gateway (missing blank line between frames); parsed %d frames individually", len(frames)))
	} else {
		convertWarn("dropping unparseable SSE data payload")
	}
	return frames
}

// sanitizeToolUseID rewrites an openai tool_call id into anthropic's tool_use id
// charset (^[a-zA-Z0-9_-]+$): illegal characters become "_" and an empty id gets
// a unique placeholder (toolu_empty_<counter>) so multiple empty-id tool calls in
// one response don't collide. Within a single convertOpenAIRequestToAnthropic call,
// the per-call idMap memo ensures the SAME original id always yields the SAME
// sanitized id (clients echo these ids back in history).
var emptyToolIDCounter atomic.Uint64

func sanitizeToolUseID(id string) string {
	if id == "" {
		return fmt.Sprintf("toolu_empty_%d", emptyToolIDCounter.Add(1))
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, id)
}

// mergeConsecutiveAnthropicRoles concatenates the content blocks of consecutive
// same-role messages. Anthropic requires strictly alternating user/assistant
// roles (after the first user); OpenAI permits consecutive same-role messages,
// so an OpenAI conversation fed through conversion would otherwise 400.
func mergeConsecutiveAnthropicRoles(msgs []map[string]any) []map[string]any {
	if len(msgs) == 0 {
		return msgs
	}
	out := []map[string]any{msgs[0]}
	for _, m := range msgs[1:] {
		last := out[len(out)-1]
		if lr, _ := last["role"].(string); lr == m["role"] {
			lb, _ := last["content"].([]map[string]any)
			cb, _ := m["content"].([]map[string]any)
			last["content"] = append(lb, cb...)
		} else {
			out = append(out, m)
		}
	}
	return out
}

func anySlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []map[string]any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out
	default:
		return nil
	}
}

// convertOpenAIRequestToAnthropic transforms an OpenAI /v1/chat/completions body
// into an Anthropic /v1/messages body (full tools support). max_tokens is required
// by Anthropic; if absent a generous default is injected.
func convertOpenAIRequestToAnthropic(body []byte, d *Diagnostics) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}
	// This converter handles ONLY Chat Completions (`messages`). The OpenAI
	// Responses API (/v1/responses) is now its own "responses" protocol with
	// dedicated converters in convert_responses.go, so a Responses body should
	// never reach here via the dispatch. Keep this `input` guard as a fail-closed
	// defense: if a chat-completions body is missing `messages` but carries an
	// `input` list, fail rather than silently emit an empty-messages request.
	if _, hasInput := src["input"]; hasInput {
		return nil, fmt.Errorf("openai→anthropic conversion supports only Chat Completions (messages); got a Responses-style `input` body — route it as protocol: responses instead")
	}
	out := map[string]any{}
	for _, k := range []string{"model", "temperature", "top_p"} {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	// max_completion_tokens wins over the legacy max_tokens when both are set;
	// Anthropic requires max_tokens, so a generous default is injected last.
	// An explicit null is treated as absent (mirrors the r→a v != nil guard).
	if mct, ok := src["max_completion_tokens"]; ok && mct != nil {
		out["max_tokens"] = mct
	} else if mt, ok := src["max_tokens"]; ok && mt != nil {
		out["max_tokens"] = mt
	} else {
		out["max_tokens"] = 4096 // Anthropic requires it
	}
	if effort, ok := src["reasoning_effort"].(string); ok {
		if th := effortToThinking(normalizeReasoningEffort(d, effort), intOf(out["max_tokens"])); th != nil {
			out["thinking"] = th
		}
	}
	if stream, ok := src["stream"]; ok {
		out["stream"] = stream
	}

	var msgs []map[string]any
	var systemText string
	var pendingTool []map[string]any // consecutive role:"tool" → one user tool_result msg
	raw, _ := src["messages"].([]any)
	// Anthropic constrains tool_use ids to ^[a-zA-Z0-9_-]+$ while openai history
	// may carry ids like "functions.Bash:0". Normalize with a per-call memo so a
	// tool_use and its tool_result(s) map to the SAME sanitized id. Two
	// DIFFERENT hostile ids may sanitize to the same string ("call.a" and
	// "call_a" both → "call_a") — a deterministic suffix keeps them apart so
	// the upstream never sees colliding tool_use ids (Switchyard FNV-1a
	// suffixes solve the same problem).
	idMap := map[string]string{}
	usedNorm := map[string]bool{}
	// Id-less tool_calls get a FRESH placeholder per occurrence: two id-less
	// calls in one message must not collapse onto one memoized toolu_empty_N
	// (duplicate tool_use ids are a hard 400). Id-less tool_results pair
	// positionally with those placeholders in order of appearance — the only
	// deterministic pairing available when neither side carries an id.
	var emptyToolIDs []string
	emptyResults := 0
	normID := func(id string) string {
		if id == "" {
			n := sanitizeToolUseID("")
			emptyToolIDs = append(emptyToolIDs, n)
			return n
		}
		if n, ok := idMap[id]; ok {
			return n
		}
		n := sanitizeToolUseID(id)
		for i := 2; usedNorm[n]; i++ {
			n = fmt.Sprintf("%s_%d", sanitizeToolUseID(id), i)
		}
		usedNorm[n] = true
		idMap[id] = n
		return n
	}
	// normResultID maps a tool message's tool_call_id: non-empty ids go through
	// the same memo as the tool_use side; an empty/missing id consumes the next
	// unpaired id-less tool_use placeholder (order of appearance).
	normResultID := func(id string) string {
		if id == "" && emptyResults < len(emptyToolIDs) {
			n := emptyToolIDs[emptyResults]
			emptyResults++
			return n
		}
		return normID(id)
	}
	flushPendingTool := func() {
		if len(pendingTool) == 0 {
			return
		}
		blocks := make([]map[string]any, 0, len(pendingTool))
		for _, tm := range pendingTool {
			tid, _ := tm["tool_call_id"].(string)
			content, isError := splitToolResultError(openaiTextOf(tm["content"], d))
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": normResultID(tid),
				"content":     content,
				"is_error":    isError,
			})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": blocks})
		pendingTool = nil
	}
	for _, m := range raw {
		mm := asMap(m)
		if mm == nil {
			continue
		}
		role, _ := mm["role"].(string)
		switch role {
		case "system", "developer":
			// Parts-array content extracts its text parts; multiple system
			// messages join with "\n" (cc-switch/opencodex join the same way).
			if sys := openaiTextOf(mm["content"], d); sys != "" {
				if systemText != "" {
					systemText += "\n"
				}
				systemText += sys
			}
			continue
		case "tool":
			pendingTool = append(pendingTool, mm)
			continue
		}
		flushPendingTool()
		switch role {
		case "user":
			if blocks := openaiContentToAnthropicBlocks(mm["content"], d); len(blocks) > 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": blocks})
			}
		case "assistant":
			// A Chat client which preserved our reasoning_details extension
			// can replay the exact signed Anthropic blocks before tool_use.
			// Unsigned reasoning_content is intentionally not promoted to an
			// Anthropic thinking block.
			blocks := chatAnthropicThinkingReplay(mm)
			if tb := openaiContentToAnthropicBlocks(mm["content"], d); len(tb) > 0 {
				blocks = append(blocks, tb...)
			}
			if tcs, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm := asMap(tc)
					if tcm == nil {
						continue
					}
					fn := asMap(tcm["function"])
					if fn == nil {
						continue
					}
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					id, _ := tcm["id"].(string)
					blocks = append(blocks, map[string]any{
						"type": "tool_use", "id": normID(id), "name": name, "input": parseToolArgs(args, d),
					})
				}
			}
			if fc := asMap(mm["function_call"]); fc != nil {
				// Legacy (pre-tool_calls) assistant function_call: deprecated
				// and without an anthropic request-side equivalent — dropped,
				// but never silently.
				warnDiag(d, "block_dropped", "dropping legacy assistant function_call "+strOf(fc["name"])+" (deprecated; no anthropic equivalent)")
			}
			if len(blocks) > 0 {
				msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
			}
		default:
			// developer/function/... have no anthropic equivalent — never silently
			// swallow a message.
			warnDiag(d, "unknown_role_dropped", "dropping message with unknown role: "+role)
		}
	}
	flushPendingTool()
	// Anthropic requires strictly alternating user/assistant roles after the
	// first user; OpenAI permits consecutive same-role messages. Merge consecutive
	// same-role messages (concatenate their content blocks) so the converted
	// request doesn't 400.
	msgs = mergeConsecutiveAnthropicRoles(msgs)
	// Anthropic requires the first message to be role:user. If the converted list
	// starts with something else, insert a minimal user placeholder to prevent a 400
	// (was a convertWarn — now actively fixes it).
	if len(msgs) > 0 {
		if r, _ := msgs[0]["role"].(string); r != "user" {
			msgs = append([]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "."}}}}, msgs...)
		}
	}
	out["messages"] = msgs
	if systemText != "" {
		out["system"] = systemText
	}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		if at := openaiToolsToAnthropic(tools); len(at) > 0 {
			out["tools"] = at
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if at := openaiToolChoiceToAnthropic(tc); at != nil {
			out["tool_choice"] = at
		}
	}
	// parallel_tool_calls:false → disable_parallel_tool_use:true. Only when the
	// request actually has tools (otherwise Anthropic rejects a tool_choice with
	// no tools). Skip when tool_choice is already type:"none" (incompatible).
	if ptc, ok := src["parallel_tool_calls"].(bool); ok && !ptc {
		if toolsArr, hasTools := src["tools"].([]any); hasTools && len(toolsArr) > 0 {
			atm := asMap(out["tool_choice"])
			if atm == nil {
				atm = map[string]any{"type": "auto"}
			}
			if atm["type"] != "none" {
				atm["disable_parallel_tool_use"] = true
				out["tool_choice"] = atm
			}
		}
	}
	// OpenAI accepts `stop` as an array of strings or a single string;
	// Anthropic only has the array form (stop_sequences).
	switch stops := src["stop"].(type) {
	case []any:
		if len(stops) > 0 {
			out["stop_sequences"] = stops
		}
	case string:
		if stops != "" {
			out["stop_sequences"] = []any{stops}
		}
	}
	// Anthropic has no response_format equivalent — drop observably (same
	// contract as the r→a direction dropping text.format).
	if rf := asMap(src["response_format"]); rf != nil {
		warnDiag(d, "response_format_dropped", "dropping response_format (no anthropic equivalent)")
	}
	// Legacy request-level function-calling params (deprecated in favor of
	// tools/tool_choice) have no anthropic equivalent — drop observably.
	if fns, ok := src["functions"].([]any); ok && len(fns) > 0 {
		warnDiag(d, "block_dropped", "dropping legacy request-level functions (deprecated; no anthropic equivalent)")
	}
	if src["function_call"] != nil {
		warnDiag(d, "block_dropped", "dropping legacy request-level function_call (deprecated; no anthropic equivalent)")
	}
	return sonic.Marshal(out)
}

// convertRequest converts a request body from the client protocol to the target
// protocol. Returns the original body unchanged when no conversion is needed.
func convertRequest(body []byte, clientProto, targetProto string) ([]byte, error) {
	return convertRequestFor(body, clientProto, targetProto, convertReqOpts{ImageOK: true})
}

// convertRequestFor is convertRequest with per-target options.
func convertRequestFor(body []byte, clientProto, targetProto string, opts convertReqOpts) ([]byte, error) {
	if err := validateConversionCapabilities(body, clientProto, targetProto); err != nil {
		return nil, err
	}
	// Strict-lossy needs somewhere to collect codes; a caller that sets
	// StrictLossy without Diag would otherwise opt out of the gate silently.
	// Auto-provision instead of failing loud: internal wiring always pairs
	// the two, this covers direct API use.
	if opts.StrictLossy && opts.Diag == nil {
		opts.Diag = NewDiagnostics()
	}
	out := body
	var err error
	if conversion, ok := lookupProtocolConversion(clientProto, targetProto); ok {
		out, err = conversion.request(body, opts)
	}
	if err != nil {
		return out, err
	}
	// Strict-lossy gate: refuse (typed error → the forward path skips this
	// target exactly like a capability-scanner verdict) instead of degrading.
	if opts.StrictLossy && opts.Diag != nil {
		var codes []string
		for _, item := range opts.Diag.Items() {
			codes = append(codes, item.Code)
		}
		if len(codes) > 0 {
			// Reuse the capability-scanner error channel: the forward path
			// skips this target and, when no target converts, answers with
			// the 400 unsupported envelope (no proxy changes needed).
			return out, &unsupportedConversionError{
				ClientProto: clientProto, TargetProto: targetProto,
				Feature: "strict_lossy:" + strings.Join(codes, ","),
				Detail:  "conversion.strict_lossy refuses lossy-but-degradable mappings",
			}
		}
	}
	if !needsConversion(clientProto, targetProto) {
		return out, nil
	}
	// One decode/encode round for every post-conversion fixup (Anthropic cache
	// breakpoints, image shrink, codex param strip): the passes used to
	// re-parse the converted body one at a time. Number literals are preserved
	// (UseNumber) so the re-marshal never corrupts large ids, and an unchanged
	// tree keeps the converter's original bytes.
	var root map[string]any
	if sonicNumberLiteral.Unmarshal(out, &root) != nil {
		return out, nil
	}
	changed := false
	if targetProto == "anthropic" {
		changed = injectAnthropicCacheBreakpointsTree(root)
	}
	changed = shrinkImageNode(root, 4<<20, 4096) || changed
	if opts.CodexShaping && targetProto == "responses" {
		// Codex backend strictness (cc-switch's codex shaping): it 400s on
		// max_output_tokens ("Unsupported parameter") and the sampling knobs —
		// strip them so the FIRST converted request doesn't have to fail for
		// paramBlock to learn the same lesson. This shaping only runs on the
		// cross-protocol conversion path: targetexec.Plan.ConvertBody short-circuits
		// same-protocol responses→codex traffic to byte-identical passthrough,
		// which self-heals via targetexec's 400→paramBlock learning retry.
		for _, k := range []string{"max_output_tokens", "temperature", "top_p"} {
			if _, present := root[k]; present {
				delete(root, k)
				changed = true
			}
		}
	}
	if !changed {
		return out, nil
	}
	stripped, serr := sonicNumberLiteral.Marshal(root)
	if serr != nil {
		return out, nil
	}
	return stripped, nil
}

// injectAnthropicCacheBreakpointsTree is the tree form used by the merged
// post-conversion pass; it reports whether anything was injected.
func injectAnthropicCacheBreakpointsTree(root map[string]any) bool {
	changed := false
	cache := map[string]any{"type": "ephemeral"}
	if tools, ok := root["tools"].([]any); ok && len(tools) > 0 {
		if last := asMap(tools[len(tools)-1]); last != nil {
			last["cache_control"] = cache
			changed = true
		}
	}
	switch system := root["system"].(type) {
	case string:
		if system != "" {
			root["system"] = []map[string]any{{
				"type": "text", "text": system, "cache_control": cache,
			}}
			changed = true
		}
	case []any:
		if len(system) > 0 {
			if last := asMap(system[len(system)-1]); last != nil {
				last["cache_control"] = cache
				changed = true
			}
		}
	}
	if messages, ok := root["messages"].([]any); ok {
		for i := len(messages) - 1; i >= 0; i-- {
			msg := asMap(messages[i])
			if strOpt(msg["role"]) != "user" {
				continue
			}
			switch content := msg["content"].(type) {
			case string:
				if content != "" {
					msg["content"] = []map[string]any{{
						"type": "text", "text": content, "cache_control": cache,
					}}
					changed = true
				}
			case []any:
				if len(content) > 0 {
					if last := asMap(content[len(content)-1]); last != nil {
						last["cache_control"] = cache
						changed = true
					}
				}
			}
			break
		}
	}
	return changed
}

// --- response (non-streaming) ---

// convertErrorResponse translates a committed upstream HTTP error envelope into
// the client protocol. Error bodies must never pass through a success-response
// converter: an OpenAI {"error":...} otherwise looks like an empty completed
// Responses object (or an empty Anthropic message).
//
// An unrecognized or unparseable body NEVER fails the conversion: the client
// must still learn WHY the request died (codex rejects unsupported models with
// a FastAPI {"detail":...} envelope; some gateways answer 4xx with no body at
// all). Such bodies degrade to a synthesized envelope carrying the raw text
// (capped) — failing closed here would escalate a translatable 400 into an
// opaque "response conversion failed" 502 that hides the upstream diagnosis.
func convertErrorResponse(body []byte, clientProto, targetProto string, status int) ([]byte, error) {
	if !needsConversion(clientProto, targetProto) {
		return body, nil
	}
	var src map[string]any
	parseErr := sonic.Unmarshal(body, &src)
	errSrc := asMap(src["error"])
	if errSrc == nil && strOf(src["type"]) == "error" {
		// Bare error envelope with no nested "error" object — the fields live
		// at the top level ({"type":"error","message":...}).
		errSrc = src
	}
	if errSrc == nil && parseErr == nil && strOf(src["detail"]) != "" {
		// FastAPI-style envelope (codex/ChatGPT backend): {"detail":"..."}.
		errSrc = map[string]any{"message": strOf(src["detail"])}
	}
	if errSrc == nil {
		errSrc = map[string]any{"message": rawErrorMessage(body, status)}
	}
	message := firstNonEmpty(strOpt(errSrc["message"]), fmt.Sprintf("upstream request failed with status %d", status))
	// A bare envelope's top-level "type" is the literal "error" marker, not an
	// error type — never propagate it as one.
	errType := strOpt(errSrc["type"])
	if errType == "error" {
		errType = ""
	}
	errType = firstNonEmpty(errType, protocolErrorType(status))
	if clientProto != "anthropic" {
		errType = firstNonEmpty(errType, strOpt(errSrc["code"]), protocolErrorType(status))
	}
	requestID := firstNonEmpty(strOpt(src["request_id"]), strOpt(errSrc["request_id"]))

	var out map[string]any
	if clientProto == "anthropic" {
		out = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    errType,
				"message": message,
			},
		}
	} else {
		e := map[string]any{
			"message": message,
			"type":    errType,
		}
		if v, ok := errSrc["param"]; ok {
			e["param"] = v
		}
		if v, ok := errSrc["code"]; ok {
			e["code"] = v
		}
		out = map[string]any{"error": e}
	}
	if requestID != "" {
		out["request_id"] = requestID
	}
	return sonic.Marshal(out)
}

func protocolErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// mapFinishToStopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
func mapFinishToStopReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "":
		return "end_turn"
	default:
		convertWarn("unknown openai finish_reason mapped to end_turn: " + finish)
		return "end_turn"
	}
}

// mapStopReasonToFinish maps an Anthropic stop_reason to an OpenAI finish_reason.
func mapStopReasonToFinish(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "pause_turn":
		return "stop" // best-effort: no chat equivalent
	default:
		return "stop"
	}
}

// openaiToolCallsToAnthropic maps openai choice.tool_calls → anthropic tool_use
// content blocks (arguments JSON string parsed to an input object).
func openaiToolCallsToAnthropic(toolCalls []any, d *Diagnostics) []map[string]any {
	var out []map[string]any
	for _, tc := range toolCalls {
		tcm := asMap(tc)
		if tcm == nil {
			continue
		}
		fn := asMap(tcm["function"])
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		id, _ := tcm["id"].(string)
		out = append(out, map[string]any{
			"type": "tool_use", "id": sanitizeToolUseID(id), "name": name, "input": parseToolArgs(args, d),
		})
	}
	return out
}

// convertOpenAIResponseToAnthropic transforms a non-streaming OpenAI
// /v1/chat/completions response into an Anthropic /v1/messages response.
func convertOpenAIResponseToAnthropic(body []byte) ([]byte, error) {
	var src struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string          `json:"role"`
				Content          json.RawMessage `json:"content"`
				Refusal          string          `json:"refusal"` // message-level refusal (content often null)
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"` // OpenRouter spelling (its reasoning_content stays null)
				ToolCalls        []any           `json:"tool_calls"`
				Annotations      []any           `json:"annotations"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"` // direct spelling
		} `json:"usage"`
	}
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	// An empty-choices 200 is a broken upstream (some reverse gateways return
	// empty 200s when overloaded) — fail the conversion so the forward path
	// surfaces an error instead of committing a well-formed empty message
	// (intentional-behaviors #1: 空 200 视为模型失败).
	if len(src.Choices) == 0 {
		return nil, fmt.Errorf("openai response has no choices (treating as upstream failure)")
	}
	var content []map[string]any
	stopReason := "end_turn"
	if len(src.Choices) > 0 {
		c := src.Choices[0]
		// reasoning_content → a thinking block, placed BEFORE the text (the
		// canonical anthropic ordering).
		if rc := firstNonEmpty(c.Message.ReasoningContent, c.Message.Reasoning); rc != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": rc})
		}
		// content may be a string or null.
		if len(c.Message.Content) > 0 && string(c.Message.Content) != "null" {
			var cv any
			sonic.Unmarshal(c.Message.Content, &cv)
			if s, ok := cv.(string); ok {
				part := map[string]any{"type": "output_text", "text": s}
				if annotations := chatAnnotationsToResponses(c.Message.Annotations); len(annotations) > 0 {
					part["annotations"] = annotations
				}
				content = append(content, map[string]any{"type": "text", "text": responsesTextWithCitationLinks(part)})
			} else if parts, ok := cv.([]any); ok {
				for _, p := range parts {
					if blk := openaiContentPartToAnthropicBlock(asMap(p), nil); blk != nil {
						content = append(content, blk)
					}
				}
				// Message-level annotations apply to the whole message — the
				// string-content path above folds them into the text; do the
				// same for parts content or the citations vanish.
				if annotations := chatAnnotationsToResponses(c.Message.Annotations); len(annotations) > 0 {
					if links := responsesCitationLinks(annotations, nil); links != "" {
						appended := false
						for i := len(content) - 1; i >= 0; i-- {
							if content[i]["type"] == "text" {
								content[i]["text"] = strOf(content[i]["text"]) + "\n\nSources: " + links
								appended = true
								break
							}
						}
						if !appended {
							content = append(content, map[string]any{"type": "text", "text": "Sources: " + links})
						}
					}
				}
			}
		}
		// Message-level refusal field → text block (some providers put the
		// refusal here instead of in a content part; content is usually null).
		if c.Message.Refusal != "" {
			content = append(content, map[string]any{"type": "text", "text": c.Message.Refusal})
		}
		if len(c.Message.ToolCalls) > 0 {
			content = append(content, openaiToolCallsToAnthropic(c.Message.ToolCalls, nil)...)
		}
		stopReason = mapFinishToStopReason(c.FinishReason)
	}
	if content == nil {
		// Anthropic rejects an assistant message with an empty content array;
		// a genuine no-content answer still needs one (empty) text block.
		content = []map[string]any{{"type": "text", "text": ""}}
	}
	// openai counts cached AND cache-creation tokens as a SUBSET of
	// prompt_tokens; anthropic counts both separately from input_tokens. Split
	// them out (clamped ≥0) — leaving cache creation in input_tokens would
	// count the write in BOTH the input and cache buckets. The direct
	// cache_creation_input_tokens spelling wins over
	// prompt_tokens_details.cache_write_tokens.
	cached := src.Usage.PromptDetails.CachedTokens
	cacheCreate := src.Usage.CacheCreationInputTokens
	if cacheCreate == 0 {
		cacheCreate = src.Usage.PromptDetails.CacheWriteTokens
	}
	inTok := src.Usage.PromptTokens - cached - cacheCreate
	if inTok < 0 {
		inTok = 0
	}
	usage := map[string]any{"input_tokens": inTok, "output_tokens": src.Usage.CompletionTokens}
	if cached > 0 {
		usage["cache_read_input_tokens"] = cached
	}
	if cacheCreate > 0 {
		usage["cache_creation_input_tokens"] = cacheCreate
	}
	out := map[string]any{
		"id":          "msg_" + src.ID,
		"type":        "message",
		"role":        "assistant",
		"model":       src.Model,
		"content":     content,
		"stop_reason": stopReason,
		// Official Message objects always carry the key (null when no stop
		// sequence fired); the streaming converter already emits it.
		"stop_sequence": nil,
		"usage":         usage,
	}
	return sonic.Marshal(out)
}

// convertAnthropicResponseToOpenAI transforms a non-streaming Anthropic
// /v1/messages response into an OpenAI /v1/chat/completions response.
func convertAnthropicResponseToOpenAI(body []byte) ([]byte, error) {
	var src struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			Citations []any           `json:"citations"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Data      string          `json:"data"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse anthropic response: %w", err)
	}
	var text string
	var toolCalls []any
	var annotations []map[string]any
	var reasoning strings.Builder
	var reasoningDetails []map[string]any
	for _, c := range src.Content {
		switch c.Type {
		case "text":
			annotations = append(annotations, anthropicCitationsToChat(c.Citations, c.Text, len([]rune(text)))...)
			text += c.Text
		case "tool_use":
			tc := map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": strOf(string(c.Input))},
			}
			if len(c.Input) == 0 {
				tc["function"].(map[string]any)["arguments"] = "{}"
			}
			toolCalls = append(toolCalls, tc)
		case "thinking":
			if reasoning.Len() > 0 && c.Thinking != "" {
				reasoning.WriteString("\n\n")
			}
			reasoning.WriteString(c.Thinking)
			if c.Signature != "" {
				reasoningDetails = append(reasoningDetails, map[string]any{
					"type": "anthropic_thinking", "thinking": c.Thinking, "signature": c.Signature,
				})
			}
		case "redacted_thinking":
			if c.Data != "" {
				reasoningDetails = append(reasoningDetails, map[string]any{
					"type": "anthropic_redacted_thinking", "data": c.Data,
				})
			}
		}
	}
	msg := map[string]any{"role": "assistant"}
	if text != "" {
		msg["content"] = text
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if text == "" {
			msg["content"] = nil
		}
	}
	if len(annotations) > 0 {
		msg["annotations"] = annotations
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(reasoningDetails) > 0 {
		msg["reasoning_details"] = reasoningDetails
	}
	// anthropic counts cache reads/creation separately from input_tokens; openai
	// folds them into prompt_tokens (cache reads surfaced via prompt_tokens_details).
	promptTok := src.Usage.InputTokens + src.Usage.CacheReadInputTokens + src.Usage.CacheCreationInputTokens
	usage := map[string]any{
		"prompt_tokens":     promptTok,
		"completion_tokens": src.Usage.OutputTokens,
		"total_tokens":      promptTok + src.Usage.OutputTokens,
	}
	if src.Usage.CacheReadInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": src.Usage.CacheReadInputTokens}
	}
	out := map[string]any{
		"id":      strings.TrimPrefix(src.ID, "msg_"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   src.Model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": mapStopReasonToFinish(src.StopReason)}},
		"usage":   usage,
	}
	return sonic.Marshal(out)
}

// convertResponse converts a BACKEND response body (in targetProto) back into the
// CLIENT protocol (clientProto). The conversion direction is target→client (the
// reverse of convertRequest), so the function chosen is the one named for that
// direction, NOT the client->target key.
func convertResponse(body []byte, clientProto, targetProto string) ([]byte, error) {
	return convertResponseNS(body, clientProto, targetProto, r2cCtx{})
}

// convertResponseNS is convertResponse with an r2cCtx (namespace restore map
// + custom tool set) for the responses→chat/anthropic directions — built by
// r2cCtxFor when the client speaks responses and the backend is chat or
// anthropic; the zero value (nil maps) makes every consumer a no-op.
func convertResponseNS(body []byte, clientProto, targetProto string, r2c r2cCtx) ([]byte, error) {
	conversion, ok := lookupProtocolConversion(clientProto, targetProto)
	if !ok {
		return body, nil
	}
	return conversion.response(body, r2c)
}

// converting — the path follows the BACKEND's protocol, not the client's).
// decisions is the only protocol whose client-facing path (/v1/decisions)
// differs from its upstream path: upstreams (TypeSafe/OpenRouter) serve the
// System One shape at /systemone relative to a versioned base URL.
func backendPath(backendProto string) string {
	switch backendProto {
	case "anthropic":
		return "/v1/messages"
	case "responses":
		return "/responses"
	case "decisions":
		return "/systemone"
	default: // "openai" (chat completions)
		return "/chat/completions"
	}
}

// convertSSEReader wraps an upstream SSE body reader with the right streaming
// transformer for the conversion direction. The transformer reads the BACKEND
// stream (targetProto) and emits the CLIENT stream (clientProto); caller has
// verified needsConversion.
func convertSSEReader(r io.Reader, clientProto, targetProto, model string) io.Reader {
	return convertSSEReaderNS(r, clientProto, targetProto, model, r2cCtx{})
}

// convertSSEReaderNS is convertSSEReader with an r2cCtx (namespace restore
// map + custom tool set) for the responses→chat/anthropic directions — built
// by r2cCtxFor when the client speaks responses and the backend is chat or
// anthropic; the zero value (nil maps) makes every consumer a no-op.
func convertSSEReaderNS(r io.Reader, clientProto, targetProto, model string, r2c r2cCtx) io.Reader {
	if conversion, ok := lookupProtocolConversion(clientProto, targetProto); ok {
		return conversion.stream(r, model, r2c)
	}
	return r
}

// rawErrorMessage extracts a client-safe message from an unparseable or
// unrecognized error body: the trimmed raw text capped at 500 bytes, or the
// generic status line for an empty body.
func rawErrorMessage(body []byte, status int) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fmt.Sprintf("upstream request failed with status %d", status)
	}
	// Truncate on a rune boundary — slicing bytes mid-rune produces invalid
	// UTF-8 that downstream JSON marshaling replaces with U+FFFD noise.
	if runes := []rune(text); len(runes) > 500 {
		text = string(runes[:500]) + "…"
	}
	return text
}
