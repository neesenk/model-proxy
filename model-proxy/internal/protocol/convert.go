package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
// still sees each distinct dropped/unmappable field at least once.
var convertWarnSeen sync.Map

func convertWarn(msg string) {
	if _, loaded := convertWarnSeen.LoadOrStore(msg, struct{}{}); !loaded {
		log.Printf("[convert] WARN: %s (suppressed further occurrences)", msg)
	}
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
func anthropicTextOf(content any) string {
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
				convertWarn("dropping cache_control breakpoint (no cross-protocol equivalent)")
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
				convertWarn("dropping non-text block in system/tool_result: " + t)
			}
		}
		return b.String()
	}
	return ""
}

// anthropicToolsToOpenAI maps anthropic tools to openai function tools. Built-in
// server tools (web_search_*/computer/bash/text_editor/...) carry a `type` other
// than the custom-tool shape and are dropped + warned.
func anthropicToolsToOpenAI(tools []any) []map[string]any {
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
			convertWarn("dropping server-side anthropic tool type: " + bt)
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
func anthropicContentBlockToOpenAIPart(blk map[string]any) map[string]any {
	if _, hasCC := blk["cache_control"]; hasCC {
		convertWarn("dropping cache_control breakpoint (no cross-protocol equivalent)")
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
				return map[string]any{"type": "file", "file": map[string]any{
					"filename": filename,
					"file_id":  id,
				}}
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
		convertWarn("dropping " + strOf(blk["type"]) + " block (no cross-protocol equivalent)")
		return nil
	}
	convertWarn("dropping unknown anthropic content block: " + strOf(blk["type"]))
	return nil
}

// anthropicMsgToOpenAIMsgs converts one anthropic message to one or more openai
// messages. A user message carrying tool_result blocks expands to separate openai
// `tool` messages (one per result) plus a `user` message for any text/image.
// imageOK gates the media reinjection (#6): without vision the images collapse
// to a placeholder line inside the tool message instead of image_url parts.
func anthropicMsgToOpenAIMsgs(m map[string]any, imageOK bool) []map[string]any {
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
					if p := anthropicContentBlockToOpenAIPart(blk); p != nil {
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
				txt := markToolResultError(anthropicToolResultText(blk["content"]), blk["is_error"] == true)
				imgs := anthropicToolResultImages(blk["content"])
				if len(imgs) > 0 && !imageOK {
					// No vision on the target: no synthetic user message, no
					// image_url parts (deepseek 400s on them) — placeholder text.
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
			if p := anthropicContentBlockToOpenAIPart(blk); p != nil {
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
func anthropicToolResultText(content any) string {
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
				convertWarn("dropping non-text block inside tool_result: " + t)
			}
		}
		return b.String()
	}
	return ""
}

// anthropicToolResultImages extracts image blocks from a tool_result content
// value as openai image_url parts (base64 → data URL, url source kept) for
// synthetic-user reinjection.
func anthropicToolResultImages(content any) []map[string]any {
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
		if p := anthropicContentBlockToOpenAIPart(m); p != nil {
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
	return convertAnthropicRequestToOpenAIV(body, true)
}

// convertAnthropicRequestToOpenAIV is convertAnthropicRequestToOpenAI with the
// target model's vision capability (media reinjection gate).
func convertAnthropicRequestToOpenAIV(body []byte, imageOK bool) ([]byte, error) {
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
		if txt := anthropicTextOf(sys); txt != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": txt})
		}
	}
	if raw, ok := src["messages"].([]any); ok {
		for _, m := range raw {
			if mm := asMap(m); mm != nil {
				msgs = append(msgs, anthropicMsgToOpenAIMsgs(mm, imageOK)...)
			}
		}
	}
	out["messages"] = msgs
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		if ot := anthropicToolsToOpenAI(tools); len(ot) > 0 {
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
	if key := anthropicExplicitPromptCacheKey(src, out); key != "" {
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

// openaiContentPartToAnthropicBlock maps an openai content part to an anthropic
// content block (text or image_url→image base64).
func openaiContentPartToAnthropicBlock(part map[string]any) map[string]any {
	switch part["type"] {
	case "text", "":
		return map[string]any{"type": "text", "text": strOf(part["text"])}
	case "image_url":
		iu := asMap(part["image_url"])
		if iu == nil {
			return nil
		}
		u, _ := iu["url"].(string)
		if strings.HasPrefix(u, "data:") {
			// data:<media>;base64,<data>
			rest := strings.TrimPrefix(u, "data:")
			semi := strings.Index(rest, ";base64,")
			if semi < 0 {
				return nil
			}
			media := rest[:semi]
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
		if id := strOpt(f["file_id"]); id != "" {
			block = map[string]any{"type": "document", "source": map[string]any{"type": "file", "file_id": id}}
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
		convertWarn("dropping unknown openai content part: " + strOf(part["type"]))
	}
	return nil
}

// openaiContentToAnthropicBlocks converts an openai message content (string or
// parts array) into anthropic content blocks.
func openaiContentToAnthropicBlocks(content any) []map[string]any {
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
		if blk := openaiContentPartToAnthropicBlock(asMap(p)); blk != nil {
			out = append(out, blk)
		}
	}
	return out
}

// openaiTextOf extracts concatenated text from an openai system/tool message
// content value (string, or array of text parts joined with "\n"). Non-text
// parts are warned + skipped; nil/absent content yields "" — NEVER the literal
// "null" that strOf(nil) would render.
func openaiTextOf(content any) string {
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
			convertWarn("dropping non-text part in system/tool content: " + t)
		}
	}
	return b.String()
}

// parseToolArgs parses an openai tool_call arguments JSON string into an object;
// on failure returns an empty object + a warning (anthropic tool_use.input must be
// an object).
func parseToolArgs(args string) any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var obj any
	if err := sonic.Unmarshal([]byte(args), &obj); err == nil {
		if _, isObj := obj.(map[string]any); isObj {
			return obj
		}
	}
	convertWarn("tool_call arguments not a JSON object; using empty input")
	return map[string]any{}
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
func convertOpenAIRequestToAnthropic(body []byte) ([]byte, error) {
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
	if mct, ok := src["max_completion_tokens"]; ok {
		out["max_tokens"] = mct
	} else if mt, ok := src["max_tokens"]; ok {
		out["max_tokens"] = mt
	} else {
		out["max_tokens"] = 4096 // Anthropic requires it
	}
	if effort, ok := src["reasoning_effort"].(string); ok {
		if th := effortToThinking(effort); th != nil {
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
	// tool_use and its tool_result(s) map to the SAME sanitized id.
	idMap := map[string]string{}
	normID := func(id string) string {
		if n, ok := idMap[id]; ok {
			return n
		}
		n := sanitizeToolUseID(id)
		idMap[id] = n
		return n
	}
	flushPendingTool := func() {
		if len(pendingTool) == 0 {
			return
		}
		blocks := make([]map[string]any, 0, len(pendingTool))
		for _, tm := range pendingTool {
			tid, _ := tm["tool_call_id"].(string)
			content, isError := splitToolResultError(openaiTextOf(tm["content"]))
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": normID(tid),
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
			if sys := openaiTextOf(mm["content"]); sys != "" {
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
			if blocks := openaiContentToAnthropicBlocks(mm["content"]); len(blocks) > 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": blocks})
			}
		case "assistant":
			// A Chat client which preserved our reasoning_details extension
			// can replay the exact signed Anthropic blocks before tool_use.
			// Unsigned reasoning_content is intentionally not promoted to an
			// Anthropic thinking block.
			blocks := chatAnthropicThinkingReplay(mm)
			if tb := openaiContentToAnthropicBlocks(mm["content"]); len(tb) > 0 {
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
						"type": "tool_use", "id": normID(id), "name": name, "input": parseToolArgs(args),
					})
				}
			}
			if len(blocks) > 0 {
				msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
			}
		default:
			// developer/function/... have no anthropic equivalent — never silently
			// swallow a message.
			convertWarn("dropping message with unknown role: " + role)
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
	if stops, ok := src["stop"].([]any); ok && len(stops) > 0 {
		out["stop_sequences"] = stops
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
	out := body
	var err error
	if conversion, ok := lookupProtocolConversion(clientProto, targetProto); ok {
		out, err = conversion.request(body, opts)
	}
	if err == nil && needsConversion(clientProto, targetProto) && targetProto == "anthropic" {
		out = injectAnthropicCacheBreakpoints(out)
	}
	if err == nil && needsConversion(clientProto, targetProto) {
		out, _ = shrinkRequestImages(out, 4<<20, 4096)
	}
	if err != nil || !opts.CodexShaping || targetProto != "responses" {
		return out, err
	}
	// Codex backend strictness (cc-switch's codex shaping): it 400s on
	// max_output_tokens ("Unsupported parameter") and the sampling knobs —
	// strip them so the FIRST converted request doesn't have to fail for
	// paramBlock to learn the same lesson. This shaping only runs on the
	// cross-protocol conversion path: targetexec.Plan.ConvertBody short-circuits
	// same-protocol responses→codex traffic to byte-identical passthrough,
	// which self-heals via the 400→paramBlock learning retry (failclass.go).
	var m map[string]any
	if sonic.Unmarshal(out, &m) != nil {
		return out, nil
	}
	for _, k := range []string{"max_output_tokens", "temperature", "top_p"} {
		delete(m, k)
	}
	stripped, serr := sonic.Marshal(m)
	if serr != nil {
		return out, nil
	}
	return stripped, nil
}

// injectAnthropicCacheBreakpoints adds stable ephemeral breakpoints to the
// converted request's system prompt, final tool declaration and last user
// content block. Same-protocol traffic remains byte-identical.
func injectAnthropicCacheBreakpoints(body []byte) []byte {
	var root map[string]any
	if sonic.Unmarshal(body, &root) != nil {
		return body
	}
	cache := map[string]any{"type": "ephemeral"}
	if tools, ok := root["tools"].([]any); ok && len(tools) > 0 {
		if last := asMap(tools[len(tools)-1]); last != nil {
			last["cache_control"] = cache
		}
	}
	switch system := root["system"].(type) {
	case string:
		if system != "" {
			root["system"] = []map[string]any{{
				"type": "text", "text": system, "cache_control": cache,
			}}
		}
	case []any:
		if len(system) > 0 {
			if last := asMap(system[len(system)-1]); last != nil {
				last["cache_control"] = cache
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
				}
			case []any:
				if len(content) > 0 {
					if last := asMap(content[len(content)-1]); last != nil {
						last["cache_control"] = cache
					}
				}
			}
			break
		}
	}
	out, err := sonic.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// --- response (non-streaming) ---

// convertErrorResponse translates a committed upstream HTTP error envelope into
// the client protocol. Error bodies must never pass through a success-response
// converter: an OpenAI {"error":...} otherwise looks like an empty completed
// Responses object (or an empty Anthropic message).
func convertErrorResponse(body []byte, clientProto, targetProto string, status int) ([]byte, error) {
	if !needsConversion(clientProto, targetProto) {
		return body, nil
	}
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse upstream error response: %w", err)
	}
	errSrc := asMap(src["error"])
	if errSrc == nil && strOf(src["type"]) == "error" {
		// Bare error envelope with no nested "error" object — the fields live
		// at the top level ({"type":"error","message":...}).
		errSrc = src
	}
	if errSrc == nil {
		return nil, fmt.Errorf("upstream status %d has no recognized error envelope", status)
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
func openaiToolCallsToAnthropic(toolCalls []any) []map[string]any {
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
			"type": "tool_use", "id": sanitizeToolUseID(id), "name": name, "input": parseToolArgs(args),
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
					if blk := openaiContentPartToAnthropicBlock(asMap(p)); blk != nil {
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
			content = append(content, openaiToolCallsToAnthropic(c.Message.ToolCalls)...)
		}
		stopReason = mapFinishToStopReason(c.FinishReason)
	}
	if content == nil {
		content = []map[string]any{}
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
		"usage":       usage,
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

// --- streaming: openai chat chunk → anthropic message events ---

const sseScanBuf = 8 * 1024 * 1024 // 8 MiB per line; oversized lines are warned + flushed

// openaiSSEToAnthropicSSE converts an OpenAI chat.completion.chunk stream into an
// Anthropic message event stream. Text streams live (content_block_delta as it
// arrives). Tool calls are BUFFERED per openai index and emitted as complete,
// sequential tool_use blocks at the end — because Anthropic's content_block model
// is strictly sequential (one open block at a time, no resuming a stopped block),
// it CANNOT represent OpenAI's interleaved parallel-tool fragment stream. Buffering
// avoids emitting input_json_delta for an already-stopped block (an invalid
// sequence) when tools interleave. usage from a trailing chunk (prompt+completion
// tokens) is carried into the terminal message_delta.usage.
type openaiSSEToAnthropicSSE struct {
	sc        *bufio.Scanner
	out       []byte
	model     string
	id        string
	started   bool
	closed    bool
	done      bool
	errored   bool
	nextIdx   int                   // next anthropic content_block index
	curKind   string                // "" / "text" (tools are buffered, never "current")
	curIdx    int                   // anthropic index of the open text block
	tools     map[int]*streamedTool // openai tool index → buffered call
	toolOrder []int                 // openai tool indices in first-seen order
	outTok    int                   // completion_tokens from trailing usage
	inTok     int                   // prompt_tokens from trailing usage
	cachedTok int                   // prompt_tokens_details.cached_tokens from trailing usage
	createTok int                   // cache write (cache_creation_input_tokens / cache_write_tokens)
	stopRsn   string                // finish_reason mapped to stop_reason
}

// streamedTool buffers one openai tool_call until the stream ends, so its
// tool_use block can be emitted as a complete, sequential anthropic block.
type streamedTool struct {
	id, name string
	args     []byte // accumulated arguments fragments (incremental JSON)
}

func newOpenAIToAnthropicSSE(r io.Reader, model string) *openaiSSEToAnthropicSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &openaiSSEToAnthropicSSE{sc: sc, model: model, id: "msg_conv", tools: map[int]*streamedTool{}}
}

func (t *openaiSSEToAnthropicSSE) emit(event string, payload map[string]any) {
	b, _ := sonic.Marshal(payload)
	t.out = append(t.out, []byte("event: "+event+"\n")...)
	t.out = append(t.out, []byte("data: ")...)
	t.out = append(t.out, b...)
	t.out = append(t.out, []byte("\n\n")...)
}

func (t *openaiSSEToAnthropicSSE) ensureStart() {
	if t.started {
		return
	}
	t.started = true
	t.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": t.id, "type": "message", "role": "assistant",
			"model": t.model, "content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// closeBlock emits content_block_stop for the open block (if any) and clears it.
func (t *openaiSSEToAnthropicSSE) closeBlock() {
	if t.curKind != "" {
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.curIdx})
		t.curKind = ""
	}
}

// openText opens a text block (closing any other open block first).
func (t *openaiSSEToAnthropicSSE) openText() {
	if t.curKind == "text" {
		return
	}
	t.closeBlock()
	t.curKind = "text"
	t.curIdx = t.nextIdx
	t.nextIdx++
	t.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": t.curIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// openThinking opens a thinking block (closing any other open block first).
func (t *openaiSSEToAnthropicSSE) openThinking() {
	if t.curKind == "thinking" {
		return
	}
	t.closeBlock()
	t.curKind = "thinking"
	t.curIdx = t.nextIdx
	t.nextIdx++
	t.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": t.curIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

// bufferTool accumulates an openai tool_call fragment (id+name on first sighting,
// argument fragments appended). The tool_use block is emitted as a complete,
// sequential block in finish() — never live — so interleaved parallel tools don't
// produce an invalid resume sequence.
func (t *openaiSSEToAnthropicSSE) bufferTool(i int, id, name, args string) {
	tc, seen := t.tools[i]
	if !seen {
		tc = &streamedTool{}
		t.tools[i] = tc
		t.toolOrder = append(t.toolOrder, i)
	}
	if id != "" {
		tc.id = id
	}
	if name != "" {
		tc.name = name
	}
	if args != "" {
		tc.args = append(tc.args, args...)
	}
}

func (t *openaiSSEToAnthropicSSE) finish() {
	if t.closed {
		return
	}
	t.ensureStart()
	t.closeBlock()
	for _, i := range t.toolOrder {
		if args := t.tools[i].args; len(args) > 0 && !json.Valid(args) {
			t.emit("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": "upstream stream ended with incomplete tool arguments",
				},
			})
			t.errored = true
			t.closed = true
			return
		}
	}
	// Emit buffered tool_use blocks sequentially (one complete block each). Empty
	// arguments → a "{}" input_json_delta so the tool_use has valid JSON input.
	for _, i := range t.toolOrder {
		tc := t.tools[i]
		idx := t.nextIdx
		t.nextIdx++
		t.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": sanitizeToolUseID(tc.id), "name": tc.name, "input": map[string]any{}},
		})
		args := string(tc.args)
		if args == "" {
			args = "{}"
		}
		t.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	}
	if len(t.toolOrder) > 0 && t.stopRsn == "" {
		t.stopRsn = "tool_use"
	}
	sr := t.stopRsn
	if sr == "" {
		sr = "end_turn"
	}
	// Same cache split as the non-streaming converter: openai's prompt_tokens
	// INCLUDES cached and cache-creation tokens; anthropic's input_tokens
	// excludes both (clamp ≥0).
	inTok := t.inTok - t.cachedTok - t.createTok
	if inTok < 0 {
		inTok = 0
	}
	usage := map[string]any{"input_tokens": inTok, "output_tokens": t.outTok}
	if t.cachedTok > 0 {
		usage["cache_read_input_tokens"] = t.cachedTok
	}
	if t.createTok > 0 {
		usage["cache_creation_input_tokens"] = t.createTok
	}
	t.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": sr, "stop_sequence": nil},
		"usage": usage,
	})
	t.emit("message_stop", map[string]any{"type": "message_stop"})
	t.closed = true
}

func (t *openaiSSEToAnthropicSSE) Read(p []byte) (int, error) {
	for len(t.out) == 0 {
		if t.done {
			t.finish()
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		if !t.sc.Scan() {
			if err := t.sc.Err(); err != nil {
				convertWarn("SSE scanner error (line too long?): " + err.Error())
				t.ensureStart()
				t.closeBlock()
				t.emit("error", map[string]any{"type": "error", "error": map[string]any{
					"type": "api_error", "message": "upstream stream terminated unexpectedly",
				}})
				t.errored = true
				t.closed = true
				t.done = true
				continue
			}
			if t.stopRsn != "" {
				t.done = true
				continue
			}
			t.ensureStart()
			t.closeBlock()
			t.emit("error", map[string]any{"type": "error", "error": map[string]any{
				"type": "api_error", "message": "upstream stream terminated before a terminal event",
			}})
			t.errored = true
			t.closed = true
			t.done = true
			continue
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			t.done = true
			continue
		}
		// OpenAI error chunk (data: {"error":{...}}) → anthropic error event.
		// Never silently swallow a mid-stream upstream error as "normal finish".
		var errChunk struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if sonic.Unmarshal([]byte(payload), &errChunk) == nil && (errChunk.Error.Message != "" || errChunk.Error.Type != "") {
			t.ensureStart()
			t.closeBlock()
			et := errChunk.Error.Type
			if et == "" {
				et = "api_error"
			}
			t.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": et, "message": errChunk.Error.Message}})
			t.errored = true
			t.closed = true
			t.done = true
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Refusal          string `json:"refusal"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"` // OpenRouter spelling
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				PromptDetails    struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"` // direct spelling
			} `json:"usage"`
		}
		if sonic.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.ID != "" {
			t.id = chunk.ID // pass the upstream's real message id through
		}
		if chunk.Model != "" {
			t.model = chunk.Model
		}
		if chunk.Usage != nil {
			t.inTok = chunk.Usage.PromptTokens
			t.outTok = chunk.Usage.CompletionTokens
			t.cachedTok = chunk.Usage.PromptDetails.CachedTokens
			t.createTok = chunk.Usage.CacheCreationInputTokens
			if t.createTok == 0 {
				t.createTok = chunk.Usage.PromptDetails.CacheWriteTokens
			}
		}
		t.ensureStart()
		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			if rc := firstNonEmpty(c.Delta.ReasoningContent, c.Delta.Reasoning); rc != "" {
				t.openThinking()
				t.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": t.curIdx,
					"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
				})
			}
			if c.Delta.Content != "" {
				t.openText()
				t.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": t.curIdx,
					"delta": map[string]any{"type": "text_delta", "text": c.Delta.Content},
				})
			}
			// Refusal deltas stream as plain text (anthropic has no refusal
			// block; the finish_reason already maps to stop_reason refusal).
			if c.Delta.Refusal != "" {
				t.openText()
				t.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": t.curIdx,
					"delta": map[string]any{"type": "text_delta", "text": c.Delta.Refusal},
				})
			}
			for _, tc := range c.Delta.ToolCalls {
				t.bufferTool(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
			if c.FinishReason != "" {
				t.stopRsn = mapFinishToStopReason(c.FinishReason)
			}
		}
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

// --- streaming: anthropic message events → openai chat chunks ---

// anthropicSSEToOpenAISSE converts an Anthropic message event stream into an OpenAI
// chat.completion.chunk stream. tool_use blocks map to delta.tool_calls (index
// assigned 0,1,2… per tool_use block); input_json_delta partial_json fragments map
// verbatim to the tool_call's function.arguments.
type anthropicSSEToOpenAISSE struct {
	sc           *bufio.Scanner
	out          []byte
	model        string
	id           string
	roleSent     bool
	done         bool
	finished     bool
	doneSent     bool
	curBlock     int          // anthropic block index currently open
	curType      string       // "text" / "tool_use" / ""
	toolCallIdx  map[int]int  // anthropic block index → openai tool_call index
	toolArgsSeen map[int]bool // anthropic block index → got ≥1 input_json_delta
	nextTool     int
	inputTokens  int
	outputTokens int
	cacheRead    int // cache_read_input_tokens from message_start
	cacheCreate  int // cache_creation_input_tokens from message_start
	textRunes    int // rune length of all emitted text blocks
	curTextStart int // rune offset of the current Anthropic text block
	curText      string
	curThinking  string
	curSignature string
	curRedacted  string
}

func newAnthropicToOpenAISSE(r io.Reader, model string) *anthropicSSEToOpenAISSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &anthropicSSEToOpenAISSE{sc: sc, model: model, id: "chatcmpl-conv",
		toolCallIdx: map[int]int{}, toolArgsSeen: map[int]bool{}}
}

// usagePayload builds the terminal chunk's usage: anthropic counts cache
// reads/creation separately from input_tokens; openai folds them into
// prompt_tokens (cache reads surfaced via prompt_tokens_details).
func (t *anthropicSSEToOpenAISSE) usagePayload() map[string]any {
	prompt := t.inputTokens + t.cacheRead + t.cacheCreate
	u := map[string]any{
		"prompt_tokens": prompt, "completion_tokens": t.outputTokens,
		"total_tokens": prompt + t.outputTokens,
	}
	if t.cacheRead > 0 {
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": t.cacheRead}
	}
	return u
}

func (t *anthropicSSEToOpenAISSE) emitChunk(delta map[string]any, finish any, usage map[string]any) {
	m := map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "model": t.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		m["usage"] = usage
	}
	b, _ := sonic.Marshal(m)
	t.out = append(t.out, []byte("data: ")...)
	t.out = append(t.out, b...)
	t.out = append(t.out, []byte("\n\n")...)
}

func (t *anthropicSSEToOpenAISSE) ensureRole() {
	if !t.roleSent {
		t.emitChunk(map[string]any{"role": "assistant"}, nil, nil)
		t.roleSent = true
	}
}

func (t *anthropicSSEToOpenAISSE) Read(p []byte) (int, error) {
	for len(t.out) == 0 {
		if t.done {
			if !t.finished {
				t.emitChunk(map[string]any{}, "stop", t.usagePayload())
				t.finished = true
			}
			if !t.doneSent {
				t.out = append(t.out, []byte("data: [DONE]\n\n")...)
				t.doneSent = true
			}
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		if !t.sc.Scan() {
			if err := t.sc.Err(); err != nil {
				convertWarn("SSE scanner error (line too long?): " + err.Error())
				errObj, _ := sonic.Marshal(map[string]any{
					"message": "upstream stream terminated unexpectedly",
					"type":    "api_error", "param": nil, "code": nil,
				})
				t.out = append(t.out, []byte("data: {\"error\":")...)
				t.out = append(t.out, errObj...)
				t.out = append(t.out, []byte("}\n\n")...)
				t.finished = true
				t.done = true
				continue
			}
			if !t.finished {
				errObj, _ := sonic.Marshal(map[string]any{
					"message": "upstream stream terminated before a terminal event",
					"type":    "api_error", "param": nil, "code": nil,
				})
				t.out = append(t.out, []byte("data: {\"error\":")...)
				t.out = append(t.out, errObj...)
				t.out = append(t.out, []byte("}\n\n")...)
				t.finished = true
			}
			t.done = true
			continue
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
				Citation    any    `json:"citation"`
			} `json:"delta"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Data string `json:"data"`
			} `json:"content_block"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
					CacheRead   int `json:"cache_read_input_tokens"`
					CacheCreate int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if sonic.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.Message.Model != "" {
			t.model = ev.Message.Model
		}
		switch ev.Type {
		case "message_start":
			t.inputTokens = ev.Message.Usage.InputTokens
			t.cacheRead = ev.Message.Usage.CacheRead
			t.cacheCreate = ev.Message.Usage.CacheCreate
			if ev.Message.ID != "" {
				t.id = ev.Message.ID // pass the upstream's real message id through
			}
		case "error":
			// anthropic error event → openai error chunk + [DONE]. Don't silently
			// turn an upstream error into a clean finish.
			et := ev.Error.Type
			if et == "" {
				et = "api_error"
			}
			errObj, _ := sonic.Marshal(map[string]any{"message": ev.Error.Message, "type": et, "param": nil, "code": nil})
			t.out = append(t.out, []byte("data: {\"error\":")...)
			t.out = append(t.out, errObj...)
			t.out = append(t.out, []byte("}\n\n")...)
			t.finished = true
			t.done = true
		case "content_block_start":
			t.curBlock = ev.Index
			t.curType = ev.ContentBlock.Type
			if ev.ContentBlock.Type == "text" {
				t.curTextStart = t.textRunes
				t.curText = ""
			} else if ev.ContentBlock.Type == "thinking" {
				t.curThinking = ""
				t.curSignature = ""
			} else if ev.ContentBlock.Type == "redacted_thinking" {
				t.curRedacted = ev.ContentBlock.Data
			}
			if ev.ContentBlock.Type == "tool_use" {
				tcIdx := t.nextTool
				t.nextTool++
				t.toolCallIdx[ev.Index] = tcIdx
				t.ensureRole()
				t.emitChunk(map[string]any{"tool_calls": []map[string]any{{
					"index": tcIdx, "id": ev.ContentBlock.ID, "type": "function",
					"function": map[string]any{"name": ev.ContentBlock.Name, "arguments": ""},
				}}}, nil, nil)
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					t.ensureRole()
					t.emitChunk(map[string]any{"content": ev.Delta.Text}, nil, nil)
					t.curText += ev.Delta.Text
					t.textRunes += len([]rune(ev.Delta.Text))
				}
			case "citations_delta":
				annotations := anthropicCitationsToChat([]any{ev.Delta.Citation}, t.curText, t.curTextStart)
				if len(annotations) > 0 {
					t.ensureRole()
					t.emitChunk(map[string]any{"annotations": annotations}, nil, nil)
				}
			case "thinking_delta":
				if thinking := firstNonEmpty(ev.Delta.Thinking, ev.Delta.Text); thinking != "" {
					t.ensureRole()
					t.emitChunk(map[string]any{"reasoning_content": thinking}, nil, nil)
					t.curThinking += thinking
				}
			case "signature_delta":
				t.curSignature += ev.Delta.Signature
			case "input_json_delta":
				if ev.Delta.PartialJSON != "" {
					if tcIdx, ok := t.toolCallIdx[t.curBlock]; ok && t.curType == "tool_use" {
						t.toolArgsSeen[t.curBlock] = true
						t.emitChunk(map[string]any{"tool_calls": []map[string]any{{
							"index": tcIdx, "function": map[string]any{"arguments": ev.Delta.PartialJSON},
						}}}, nil, nil)
					}
				}
			default:
				// thinking_delta/signature_delta/... have no openai equivalent.
				if ev.Delta.Type != "" {
					convertWarn("dropping " + ev.Delta.Type + " delta (no cross-protocol equivalent)")
				}
			}
		case "content_block_stop":
			if t.curType == "thinking" && t.curSignature != "" {
				t.emitChunk(map[string]any{"reasoning_details": []map[string]any{{
					"type": "anthropic_thinking", "thinking": t.curThinking, "signature": t.curSignature,
				}}}, nil, nil)
			} else if t.curType == "redacted_thinking" && t.curRedacted != "" {
				t.emitChunk(map[string]any{"reasoning_details": []map[string]any{{
					"type": "anthropic_redacted_thinking", "data": t.curRedacted,
				}}}, nil, nil)
			}
			// Empty-args fallback: a tool_use block with no input_json_delta still
			// gets a "{}" arguments fragment (openai requires valid JSON arguments).
			if t.curType == "tool_use" {
				if tcIdx, ok := t.toolCallIdx[t.curBlock]; ok && !t.toolArgsSeen[t.curBlock] {
					t.emitChunk(map[string]any{"tool_calls": []map[string]any{{
						"index": tcIdx, "function": map[string]any{"arguments": "{}"},
					}}}, nil, nil)
				}
			}
			t.curType = ""
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				t.outputTokens = ev.Usage.OutputTokens
			}
			// The finish chunk carries usage so the OpenAI-protocol usage scanner
			// attributes tokens. Guard: a malformed stream with >1 message_delta
			// must not emit >1 finish chunk.
			if !t.finished {
				t.emitChunk(map[string]any{}, mapStopReasonToFinish(ev.Delta.StopReason), t.usagePayload())
				t.finished = true
			}
		case "message_stop":
			t.done = true
		}
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

// backendPath returns the upstream request path for a backend protocol (used when
// converting — the path follows the BACKEND's protocol, not the client's).
func backendPath(backendProto string) string {
	switch backendProto {
	case "anthropic":
		return "/v1/messages"
	case "responses":
		return "/responses"
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
