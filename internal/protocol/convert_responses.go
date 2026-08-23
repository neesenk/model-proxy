// convert_responses.go — direct pairwise converters for the OpenAI Responses
// API (/v1/responses), the 3rd wire protocol alongside anthropic (/v1/messages)
// and openai-chat (/v1/chat/completions). Mirrors convert.go's direct-pairwise
// style (sonic + map[string]any, no Core IR); the anthropic↔chat converters
// there are untouched.
//
// Wire shapes (field-level reference: moon-bridge internal/protocol/openai):
//
//	Responses request:  {model, instructions, input:[items], tools, tool_choice,
//	                    reasoning:{effort}, max_output_tokens, temperature, top_p, stream}
//	  input items: {type:"message", role, content:[{type:"input_text"|"output_text"|
//	                "input_image", text|image_url}]}, {type:"function_call", name,
//	                arguments, call_id}, {type:"function_call_output", call_id, output},
//	                {type:"reasoning", summary:[{type:"summary_text",text}], encrypted_content}
//	  tools:       {type:"function", name, description, parameters}
//	Responses response: {id, object:"response", status, model, output:[items], usage}
//	  output items: {type:"message", role:"assistant", content:[{type:"output_text",text}]},
//	                {type:"function_call", id, call_id, name, arguments},
//	                {type:"reasoning", summary:[...]}
//
// The pure pairwise converter consumes a full input history. Proxy expands
// previous_response_id from its bounded local state store before calling it;
// callers that use this helper directly should likewise supply complete input.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	sonic "github.com/bytedance/sonic"
)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// defaultAnthropicMaxTokens is the generous max_tokens injected when the
// source request carries no cap — Anthropic 400s "max_tokens required"
// otherwise. Same value as the chat→a direction (convert.go).
const defaultAnthropicMaxTokens = 4096

// copyOpt copies optional top-level keys from src to out when present.
func copyOpt(out, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
}

// firstNonEmpty returns the first non-empty string arg.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// strKey returns m[key] as a string, "" when m or the key is absent (unlike
// strOf, which renders a missing key as "null").
func strKey(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// strOpt is strKey for a bare value: "" for nil/non-string. Use it for
// OPTIONAL protocol fields (call_id/name) — strOf would stringify nil to
// "null", which is a non-empty string and both poisons the field and defeats
// firstNonEmpty fallbacks.
func strOpt(v any) string { s, _ := v.(string); return s }

// backfillToolNames fills empty function_call names from earlier items with
// the same call_id; names still missing afterwards get a convertWarn (an
// empty name is a protocol violation upstream).
func backfillToolNames(items []map[string]any) {
	names := map[string]string{}
	for _, it := range items {
		if it["type"] != "function_call" {
			continue
		}
		id := strOpt(it["call_id"])
		if name := strOpt(it["name"]); name != "" {
			if id != "" {
				names[id] = name
			}
			continue
		}
		if n, ok := names[id]; ok {
			it["name"] = n
		} else {
			convertWarn("function_call missing name (call_id " + id + ")")
		}
	}
}

// responsesInputItems normalizes a Responses request `input` field (string
// shorthand | array of items) into a slice of item maps. nil if absent/empty.
func responsesInputItems(v any) []map[string]any {
	switch raw := v.(type) {
	case string:
		if raw == "" {
			return nil
		}
		return []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": raw}},
		}}
	case []any:
		var out []map[string]any
		for _, it := range raw {
			if m := asMap(it); m != nil {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// chatContentText concatenates text from an OpenAI-chat message content (string,
// or array of {type:"text",text} parts). Image parts are ignored here.
func chatContentText(v any) string {
	switch raw := v.(type) {
	case string:
		return raw
	case []any:
		var b strings.Builder
		for _, p := range raw {
			if pm := asMap(p); pm != nil {
				if t, ok := pm["type"].(string); ok && (t == "text" || t == "input_text" || t == "output_text") {
					b.WriteString(strOf(pm["text"]))
				}
			}
		}
		return b.String()
	}
	return ""
}

// parseDataURL splits a "data:<media>;base64,<data>" URL into media type + data.
func parseDataURL(s string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", "", false
	}
	return rest[:semi], rest[semi+len(";base64,"):], true
}

// thinkingBudgetToEffort maps an anthropic thinking config {type, budget_tokens}
// to a Responses reasoning.effort (best-effort; budget↔effort is not 1:1).
// "" means "do not set reasoning".
func thinkingBudgetToEffort(thinking map[string]any) string {
	if t, _ := thinking["type"].(string); t != "" && t != "enabled" {
		return ""
	}
	budget, _ := thinking["budget_tokens"].(float64)
	switch {
	case budget >= 10000:
		return "high"
	case budget >= 5000:
		return "medium"
	case budget > 0:
		return "low"
	}
	return "medium"
}

// outputConfigEffort reads the adaptive-thinking effort level from an
// anthropic output_config object (Claude Code /effort wire). Unknown strings
// return "" so downstream defaults win (opencodex effortFromOutputConfig).
func outputConfigEffort(outputConfig any) string {
	effort := strOpt(asMap(outputConfig)["effort"])
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return effort
	}
	return ""
}

// effortToThinking maps a Responses reasoning.effort back to an anthropic
// thinking config (best-effort budget). nil means "do not set thinking".
func effortToThinking(effort string) map[string]any {
	budget := 0
	switch effort {
	case "high":
		budget = 24000
	case "medium":
		budget = 8000
	case "low":
		budget = 2000
	default:
		return nil
	}
	return map[string]any{"type": "enabled", "budget_tokens": budget}
}

// marshalToolInput serializes an anthropic tool_use input to a JSON arguments
// string; a MISSING input becomes "{}" (sonic renders nil as "null", which
// downstream JSON.parse rejects).
func marshalToolInput(v any) string {
	if v == nil {
		return "{}"
	}
	args, _ := sonic.MarshalString(v)
	if args == "" || args == "null" {
		return "{}"
	}
	return args
}

// thinkOpenTag / thinkCloseTag mark inline thinking blocks that some chat
// upstreams (MiniMax-style) emit inside content instead of a reasoning field.
const thinkOpenTag = "<think>"
const thinkCloseTag = "</think>"

// splitLeadingThinkBlock splits a LEADING <think>…</think> block (leading
// whitespace tolerated) into (reasoning, answer): reasoning trimmed, the
// answer's leading separator whitespace stripped (cc-switch
// split_leading_think_block). ok=false when the text does not START with a
// think block — mid-text blocks are never split.
func splitLeadingThinkBlock(text string) (reasoning, answer string, ok bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, thinkOpenTag) {
		return "", "", false
	}
	body := trimmed[len(thinkOpenTag):]
	close := strings.Index(body, thinkCloseTag)
	if close < 0 {
		return "", "", false
	}
	reasoning = strings.TrimSpace(body[:close])
	answer = strings.TrimLeft(body[close+len(thinkCloseTag):], " \t\r\n")
	return reasoning, answer, true
}

// responsesReasoningText extracts (text, signature) from a Responses reasoning
// item's summary array (+ encrypted_content as signature). OpenRouter-style
// items carry the text in content parts ({type:"reasoning_text"}) instead of a
// summary — fall back to those (their streams use reasoning_text.delta too).
func responsesReasoningText(item map[string]any) (text, sig string) {
	if enc, ok := item["encrypted_content"].(string); ok {
		sig = enc
	}
	if sum, ok := item["summary"].([]any); ok {
		var b strings.Builder
		for _, s := range sum {
			if sm := asMap(s); sm != nil {
				b.WriteString(strOf(sm["text"]))
			}
		}
		text = b.String()
	}
	if text == "" {
		if parts, ok := item["content"].([]any); ok {
			var b strings.Builder
			for _, p := range parts {
				if pm := asMap(p); pm != nil {
					b.WriteString(strOf(pm["text"]))
				}
			}
			text = b.String()
		}
	}
	return
}

// chatReasoningText extracts reasoning text from a chat message or stream
// delta, exhausting the vendor spellings (cc-switch codex_chat_common's
// extraction order): reasoning_content > reasoning (string, or an object with
// content/text/summary) > reasoning_details (array/object; OpenRouter-style,
// e.g. aqp). "" when nothing replayable is present.
func chatReasoningText(m map[string]any) string {
	for _, k := range []string{"reasoning_content", "reasoning"} {
		if s := strOpt(m[k]); s != "" {
			return s
		}
	}
	if r := asMap(m["reasoning"]); r != nil {
		for _, k := range []string{"content", "text", "summary"} {
			if s := strOpt(r[k]); s != "" {
				return s
			}
		}
	}
	return reasoningDetailsText(m["reasoning_details"])
}

// reasoningDetailsText extracts text from a reasoning_details value (string,
// object, or array of detail entries joined "\n\n").
func reasoningDetailsText(v any) string {
	switch raw := v.(type) {
	case string:
		return raw
	case map[string]any:
		return reasoningDetailPartText(raw)
	case []any:
		var parts []string
		for _, p := range raw {
			if s := reasoningDetailPartText(p); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// reasoningDetailPartText reads one reasoning_details entry: text/content/
// summary keys carry the replayable text (encrypted entries have none and are
// skipped naturally).
func reasoningDetailPartText(v any) string {
	m := asMap(v)
	if m == nil {
		return strOpt(v)
	}
	for _, k := range []string{"text", "content", "summary"} {
		if s := strOpt(m[k]); s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// request: anthropic → responses
// ---------------------------------------------------------------------------

// anthropicMsgToResponsesItems turns one anthropic message into one or more
// Responses `input` items. text/image blocks collect into a `message` item;
// tool_use → function_call, tool_result → function_call_output, thinking →
// reasoning — each its own item (Responses separates them out of the message).
func anthropicMsgToResponsesItems(m map[string]any, imageOK bool) []map[string]any {
	role, _ := m["role"].(string)
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}

	var items []map[string]any
	var parts []map[string]any
	webSearchInputs := map[string]map[string]any{}
	flush := func() {
		if len(parts) > 0 {
			items = append(items, map[string]any{
				"type": "message", "role": role, "content": parts,
			})
			parts = nil
		}
	}

	switch c := m["content"].(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"type": partType, "text": c})
		}
	case []any:
		for _, blk := range c {
			b := asMap(blk)
			if b == nil {
				continue
			}
			switch b["type"] {
			case "text":
				part := map[string]any{"type": partType, "text": strOf(b["text"])}
				if partType == "output_text" {
					if annotations := anthropicCitationsToResponses(b["citations"], strOf(b["text"])); len(annotations) > 0 {
						part["annotations"] = annotations
					}
				}
				parts = append(parts, part)
			case "image":
				if src := asMap(b["source"]); src != nil {
					if mt, _ := src["media_type"].(string); mt != "" {
						if data, _ := src["data"].(string); data != "" {
							parts = append(parts, map[string]any{
								"type":      "input_image",
								"image_url": "data:" + mt + ";base64," + data,
							})
						}
					} else if u, _ := src["url"].(string); u != "" {
						// url-source images pass through as-is (a→chat supports
						// them too; dropping would lose the content silently).
						parts = append(parts, map[string]any{"type": "input_image", "image_url": u})
					}
				}
			case "document":
				if file := anthropicDocumentToResponsesPart(b); file != nil {
					parts = append(parts, file)
				}
			case "tool_use":
				flush()
				args := marshalToolInput(b["input"])
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   firstNonEmpty(strOpt(b["id"]), strOpt(b["name"])),
					"name":      strOpt(b["name"]),
					"arguments": args,
				})
			case "server_tool_use":
				flush()
				if strOpt(b["name"]) == "web_search" {
					webSearchInputs[strOpt(b["id"])] = asMap(b["input"])
				} else {
					convertWarn("dropping unsupported anthropic server_tool_use in a→r request: " + strOpt(b["name"]))
				}
			case "web_search_tool_result":
				flush()
				id := strOpt(b["tool_use_id"])
				item := map[string]any{
					"type": "web_search_call", "id": id, "status": "completed", "action": webSearchInputs[id],
				}
				switch content := b["content"].(type) {
				case []any:
					var sources []map[string]any
					for _, raw := range content {
						hit := asMap(raw)
						if hit["type"] == "web_search_result" && strOpt(hit["url"]) != "" {
							sources = append(sources, map[string]any{"url": hit["url"], "title": hit["title"]})
						}
					}
					item["sources"] = sources
				case map[string]any:
					if content["type"] == "web_search_tool_result_error" {
						item["status"] = "failed"
					}
				}
				items = append(items, item)
			case "tool_result":
				flush()
				txt := anthropicToolResultText(b["content"])
				imgs := anthropicToolResultImagesResponses(b["content"])
				if len(imgs) > 0 && !imageOK {
					txt = appendMediaPlaceholder(txt)
					imgs = nil
				}
				output := any(markToolResultError(txt, b["is_error"] == true))
				if b["is_error"] == true && len(imgs) > 0 {
					errorParts := []map[string]any{{"type": "input_text", "text": toolResultErrorMarker}}
					if txt != "" {
						errorParts = append(errorParts, map[string]any{"type": "input_text", "text": txt})
					}
					output = errorParts
				}
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": strOf(b["tool_use_id"]),
					"output":  output,
				})
				// Media reinjection (cc-switch): tool_result images are
				// re-delivered as a synthetic user message item, not dropped.
				if len(imgs) > 0 {
					parts := []map[string]any{{"type": "input_text", "text": "[image returned by tool]"}}
					parts = append(parts, imgs...)
					items = append(items, map[string]any{"type": "message", "role": "user", "content": parts})
				}
			case "thinking":
				flush()
				item := map[string]any{
					"type": "reasoning",
					"summary": []map[string]any{{
						"type": "summary_text", "text": strOf(b["thinking"]),
					}},
				}
				if sig, _ := b["signature"].(string); sig != "" {
					item["encrypted_content"] = sig
				}
				items = append(items, item)
			case "redacted_thinking":
				flush()
				// codex requires `summary` even when it's empty
				// ("Missing required parameter: 'input[N].summary'", live-verified).
				item := map[string]any{"type": "reasoning", "summary": []any{}}
				if data, _ := b["data"].(string); data != "" {
					item["encrypted_content"] = data
				}
				items = append(items, item)
			default:
				convertWarn("dropping anthropic content block in a→r request: " + strOf(b["type"]))
			}
		}
	}
	flush()
	return items
}

func anthropicDocumentToResponsesPart(block map[string]any) map[string]any {
	src := asMap(block["source"])
	if src == nil {
		return nil
	}
	filename := firstNonEmpty(strOpt(block["title"]), strOpt(block["filename"]), "document.pdf")
	switch strOpt(src["type"]) {
	case "url":
		if u := strOpt(src["url"]); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			return map[string]any{"type": "input_file", "file_url": u, "filename": filename}
		}
	case "base64":
		if data := strOpt(src["data"]); data != "" {
			mediaType := firstNonEmpty(strOpt(src["media_type"]), "application/pdf")
			return map[string]any{
				"type": "input_file", "file_data": "data:" + mediaType + ";base64," + data, "filename": filename,
			}
		}
	case "file":
		if id := strOpt(src["file_id"]); id != "" {
			return map[string]any{"type": "input_file", "file_id": id, "filename": filename}
		}
	case "text":
		if text := strOpt(src["data"]); text != "" {
			return map[string]any{"type": "input_text", "text": "[document " + filename + "]\n" + text}
		}
	}
	return nil
}

// dropOrphanReasoningItems removes reasoning items produced from ONE assistant
// message (input[start:]) when that generation contains no message or
// function_call item to follow them — codex 400s "reasoning item without its
// required following item" on thinking-only incomplete turns (cc-switch
// transform_responses.rs does the same removal).
func dropOrphanReasoningItems(items []map[string]any, start int) []map[string]any {
	for _, it := range items[start:] {
		if it["type"] == "message" || it["type"] == "function_call" {
			return items
		}
	}
	out := items[:start]
	for _, it := range items[start:] {
		if it["type"] == "reasoning" {
			convertWarn("dropping orphan reasoning item (no following message/function_call in the same assistant turn)")
			continue
		}
		out = append(out, it)
	}
	return out
}

// anthropicToolResultImagesResponses extracts image blocks from a tool_result
// content value as responses input_image parts (base64 → data URL, url source
// kept) for synthetic-user reinjection.
func anthropicToolResultImagesResponses(content any) []map[string]any {
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
		src := asMap(m["source"])
		if src == nil {
			continue
		}
		if mt, _ := src["media_type"].(string); mt != "" {
			if data, _ := src["data"].(string); data != "" {
				out = append(out, map[string]any{"type": "input_image", "image_url": "data:" + mt + ";base64," + data})
				continue
			}
		}
		if u, _ := src["url"].(string); u != "" {
			out = append(out, map[string]any{"type": "input_image", "image_url": u})
		}
	}
	return out
}

func anthropicToolsToResponses(tools []any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		tm := asMap(t)
		if tm == nil {
			continue
		}
		// Server-side/built-in tools declare a `type` (web_search_*, computer,
		// bash, text_editor, ...) unlike custom function tools (name+input_schema;
		// type absent or the explicit default "custom"). Drop + warn, same as
		// anthropicToolsToOpenAI.
		if bt, ok := tm["type"].(string); ok && bt != "" && bt != "custom" {
			if strings.HasPrefix(bt, "web_search") {
				rt := map[string]any{"type": "web_search"}
				copyOpt(rt, tm, "max_uses", "allowed_domains", "blocked_domains")
				out = append(out, rt)
				continue
			}
			convertWarn("dropping server-side anthropic tool type: " + bt)
			continue
		}
		name := strOpt(tm["name"])
		if name == "" {
			continue
		}
		rt := map[string]any{"type": "function", "name": name}
		if d, ok := tm["description"]; ok {
			rt["description"] = d
		}
		if sch, ok := tm["input_schema"]; ok {
			rt["parameters"] = sch
		}
		out = append(out, rt)
	}
	return out
}

// anthropicToolChoiceToResponses maps anthropic tool_choice to Responses.
// anthropic {type:"auto"|"any"|"tool"|"none", name?} → responses
// "auto"|"required"|"none"|{type:"function",name}.
func anthropicToolChoiceToResponses(tc any) any {
	tcm := asMap(tc)
	if tcm == nil {
		return nil
	}
	switch tcm["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "name": strOf(tcm["name"])}
	}
	return nil
}

func convertAnthropicRequestToResponses(body []byte) ([]byte, error) {
	return convertAnthropicRequestToResponsesV(body, true)
}

// convertAnthropicRequestToResponsesV is convertAnthropicRequestToResponses
// with the target model's vision capability (media reinjection gate).
func convertAnthropicRequestToResponsesV(body []byte, imageOK bool) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	out := map[string]any{}
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	var instructionParts []string
	if sys, ok := src["system"]; ok {
		if txt := anthropicTextOf(sys); txt != "" {
			instructionParts = append(instructionParts, txt)
		}
	}
	var input []map[string]any
	if raw, ok := src["messages"].([]any); ok {
		for _, m := range raw {
			mm := asMap(m)
			if mm == nil {
				continue
			}
			// system/developer-role MESSAGES fold into instructions: Responses
			// input rejects system-role message items (codex 400s "System
			// messages are not allowed"; opencodex's inbound folds the same).
			if role := strOpt(mm["role"]); role == "system" || role == "developer" {
				if txt := anthropicTextOf(mm["content"]); txt != "" {
					instructionParts = append(instructionParts, txt)
				}
				continue
			}
			start := len(input)
			input = append(input, anthropicMsgToResponsesItems(mm, imageOK)...)
			if strOpt(mm["role"]) == "assistant" {
				input = dropOrphanReasoningItems(input, start)
			}
		}
	}
	if len(instructionParts) > 0 {
		out["instructions"] = strings.Join(instructionParts, "\n\n")
	}
	if len(input) > 0 {
		out["input"] = input
	}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		if rt := anthropicToolsToResponses(tools); len(rt) > 0 {
			out["tools"] = rt
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if rc := anthropicToolChoiceToResponses(tc); rc != nil {
			out["tool_choice"] = rc
		}
		// disable_parallel_tool_use:true → parallel_tool_calls:false.
		if tcm := asMap(tc); tcm != nil {
			if dis, _ := tcm["disable_parallel_tool_use"].(bool); dis {
				out["parallel_tool_calls"] = false
			}
		}
	}
	if stops, ok := src["stop_sequences"].([]any); ok && len(stops) > 0 {
		convertWarn("dropping stop_sequences (Responses API has no stop parameter)")
	}
	if thinking, ok := src["thinking"].(map[string]any); ok {
		effort := ""
		if strOpt(thinking["type"]) == "adaptive" {
			// Adaptive-thinking wire (Claude Code /effort, opencodex
			// claude/inbound.ts): the level rides in output_config.effort;
			// unknown strings drop to the default so garbage never crosses.
			effort = outputConfigEffort(src["output_config"])
			if effort == "" {
				effort = "high"
			}
		} else {
			effort = thinkingBudgetToEffort(thinking)
		}
		if effort != "" {
			// summary:"auto" asks the backend to stream reasoning summaries
			// (opencodex sets it unconditionally); without it codex responses
			// carry no reasoning items at all.
			out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
		}
	}
	if v, ok := src["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	// prompt_cache_key (codex reports cached_tokens:0 without one):
	// metadata.user_id's sha256 is the stable per-session key — a deliberate
	// exception to the "metadata is dropped" lossy-field rule, and only the
	// HASH crosses (never the raw user_id). Without user_id, fingerprint the
	// model + instructions + converted tools (cache-cohort key, opencodex
	// claude/inbound.ts:443-475).
	if uid := strOpt(asMap(src["metadata"])["user_id"]); uid != "" {
		out["prompt_cache_key"] = sha256Hex32(uid)
	} else if ins := strOpt(out["instructions"]); ins != "" || out["tools"] != nil {
		// ConfigStd sorts map keys (like encoding/json) — the default sonic
		// config does not, and an unsorted fingerprint is non-deterministic.
		fp, _ := sonic.ConfigStd.MarshalToString(map[string]any{
			"model": strOpt(out["model"]), "system": ins, "tools": out["tools"],
		})
		out["prompt_cache_key"] = sha256Hex32(fp)
	}
	copyOpt(out, src, "temperature", "top_p", "stream")
	return sonic.Marshal(out)
}

// sha256Hex32 hashes s with SHA-256 and returns the first 32 hex chars (the
// prompt_cache_key shape opencodex uses).
func sha256Hex32(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

func anthropicHasExplicitCacheControl(v any) bool {
	switch value := v.(type) {
	case map[string]any:
		if value["cache_control"] != nil {
			return true
		}
		for _, child := range value {
			if anthropicHasExplicitCacheControl(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if anthropicHasExplicitCacheControl(child) {
				return true
			}
		}
	}
	return false
}

// anthropicExplicitPromptCacheKey gives Chat/Responses backends a stable cache
// cohort when the Anthropic client explicitly placed cache breakpoints. Those
// protocols cache prefixes automatically and have no per-block breakpoint,
// so this preserves cache affinity without copying an invalid cache_control
// field onto their wire format.
func anthropicExplicitPromptCacheKey(src, converted map[string]any) string {
	if !anthropicHasExplicitCacheControl(src) {
		return ""
	}
	if uid := strOpt(asMap(src["metadata"])["user_id"]); uid != "" {
		return sha256Hex32(uid)
	}
	fp, _ := sonic.ConfigStd.MarshalToString(map[string]any{
		"model":  strOpt(converted["model"]),
		"system": anthropicTextOf(src["system"]),
		"tools":  converted["tools"],
	})
	return sha256Hex32(fp)
}

// ---------------------------------------------------------------------------
// request: openai-chat → responses
// ---------------------------------------------------------------------------

// chatMsgToResponsesItems turns one OpenAI-chat message into one or more
// Responses `input` items. tool_calls → function_call items; role:"tool" →
// function_call_output; text/image content → a message item.
func chatMsgToResponsesItems(m map[string]any) []map[string]any {
	role, _ := m["role"].(string)
	if role == "tool" {
		out := map[string]any{
			"type":    "function_call_output",
			"call_id": strOpt(m["tool_call_id"]),
			"output":  chatContentText(m["content"]),
		}
		return []map[string]any{out}
	}
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	var items []map[string]any
	// reasoning_content rides as its own reasoning item (preserved, like the
	// anthropic thinking block — contract: reasoning survives the responses hop).
	if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
		items = append(items, map[string]any{
			"type":    "reasoning",
			"summary": []map[string]any{{"type": "summary_text", "text": rc}},
		})
	}
	var parts []map[string]any
	switch c := m["content"].(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"type": partType, "text": c})
		}
	case []any:
		for _, p := range c {
			pm := asMap(p)
			if pm == nil {
				continue
			}
			switch pm["type"] {
			case "text", "input_text", "output_text":
				parts = append(parts, map[string]any{"type": partType, "text": strOf(pm["text"])})
			case "input_image", "image", "image_url":
				if u, ok := pm["image_url"].(string); ok && u != "" {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": u})
				} else if ium := asMap(pm["image_url"]); ium != nil {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": strOf(ium["url"])})
				}
			case "input_file", "file":
				file := map[string]any{"type": "input_file"}
				// Chat nests the fields under "file"; responses keeps them flat.
				fm := asMap(pm["file"])
				if fm == nil {
					fm = pm
				}
				copyOpt(file, fm, "file_id", "file_data", "file_url", "filename")
				if len(file) > 1 {
					parts = append(parts, file)
				}
			default:
				convertWarn("dropping chat content part in chat→r request: " + strOf(pm["type"]))
			}
		}
	}
	if len(parts) > 0 {
		items = append(items, map[string]any{"type": "message", "role": role, "content": parts})
	}
	if tcs, ok := m["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm := asMap(tc)
			if tcm == nil {
				continue
			}
			fn := asMap(tcm["function"])
			// arguments must be a JSON STRING; "" is invalid (downstream
			// JSON.parse("") breaks), default to "{}" like the stream path.
			args := strOpt(fnMap(fn, "arguments"))
			if args == "" {
				if raw := fnMap(fn, "arguments"); raw != nil {
					args = strOf(raw) // non-string (object) → compact JSON
				}
			}
			if args == "" {
				args = "{}"
			}
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   strOpt(tcm["id"]),
				"name":      firstNonEmpty(strOpt(fnMap(fn, "name")), strOpt(tcm["name"])),
				"arguments": args,
			})
		}
	}
	return items
}

// fnMap returns the value for key from a function map (nil-safe).
func fnMap(fn map[string]any, key string) any {
	if fn == nil {
		return nil
	}
	return fn[key]
}

func chatToolsToResponses(tools []any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		tm := asMap(t)
		if tm == nil {
			continue
		}
		fn := asMap(tm["function"])
		rt := map[string]any{"type": "function", "name": strOpt(fnMap(fn, "name"))}
		if d := fnMap(fn, "description"); d != nil {
			rt["description"] = d
		}
		if p := fnMap(fn, "parameters"); p != nil {
			rt["parameters"] = p
		}
		if s := fnMap(fn, "strict"); s != nil {
			rt["strict"] = s
		}
		out = append(out, rt)
	}
	return out
}

// chatToolChoiceToResponses: "auto"/"none"/"required" → same; {type:"function",
// function:{name}} → {type:"function", name}.
func chatToolChoiceToResponses(tc any) any {
	if s, ok := tc.(string); ok {
		return s
	}
	tcm := asMap(tc)
	if tcm == nil {
		return nil
	}
	if t, _ := tcm["type"].(string); t == "function" {
		if fn := asMap(tcm["function"]); fn != nil {
			return map[string]any{"type": "function", "name": strOf(fn["name"])}
		}
	}
	return tcm
}

// chatResponseFormatToTextFormat maps a chat response_format to a Responses
// text.format: json_object passes through; json_schema is unwrapped one level
// (name/schema/strict/description live directly on the format object).
func chatResponseFormatToTextFormat(rf map[string]any) map[string]any {
	switch strOf(rf["type"]) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := asMap(rf["json_schema"])
		if js == nil {
			return nil
		}
		// The nested type never overrides the discriminator: a wrapper saying
		// {"type":"json_schema","json_schema":{"type":"text"}} is still a
		// json_schema request (Switchyard ports the same rule).
		f := map[string]any{"type": "json_schema"}
		copyOpt(f, js, "name", "description", "schema", "strict")
		// An empty wrapper (no schema) has nothing to express: a bare
		// {"type":"json_schema"} without schema is invalid upstream — drop the
		// format observably and let the backend use its default.
		if _, ok := f["schema"]; !ok {
			convertWarn("dropping empty json_schema response_format (no schema)")
			return nil
		}
		return f
	}
	return nil
}

// textFormatToChatResponseFormat maps a Responses text.format back to a chat
// response_format (reverse of chatResponseFormatToTextFormat).
func textFormatToChatResponseFormat(f map[string]any) map[string]any {
	switch strOf(f["type"]) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := map[string]any{}
		copyOpt(js, f, "name", "description", "schema", "strict")
		return map[string]any{"type": "json_schema", "json_schema": js}
	}
	return nil
}

func convertOpenAIRequestToResponses(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}
	out := map[string]any{}
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	var input []map[string]any
	var instructions []string
	if raw, ok := src["messages"].([]any); ok {
		for _, m := range raw {
			mm := asMap(m)
			if mm == nil {
				continue
			}
			role, _ := mm["role"].(string)
			if role == "system" || role == "developer" {
				if txt := chatContentText(mm["content"]); txt != "" {
					instructions = append(instructions, txt)
				}
				continue
			}
			input = append(input, chatMsgToResponsesItems(mm)...)
		}
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	// Replace-style clients resend a tool_call carrying only the id (no name)
	// in later turns — backfill from earlier items with the same call_id
	// (opencodex's chat inbound does the same).
	backfillToolNames(input)
	if len(input) > 0 {
		out["input"] = input
	}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		if rt := chatToolsToResponses(tools); len(rt) > 0 {
			out["tools"] = rt
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if rc := chatToolChoiceToResponses(tc); rc != nil {
			out["tool_choice"] = rc
		}
	}
	if effort, ok := src["reasoning_effort"].(string); ok && effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
	}
	// max_completion_tokens wins over the legacy max_tokens when both are set.
	if v, ok := src["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := src["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	switch stops := src["stop"].(type) {
	case []any:
		if len(stops) > 0 {
			convertWarn("dropping stop (Responses API has no stop parameter)")
		}
	case string:
		if stops != "" {
			convertWarn("dropping stop (Responses API has no stop parameter)")
		}
	}
	if rf := asMap(src["response_format"]); rf != nil {
		if f := chatResponseFormatToTextFormat(rf); f != nil {
			out["text"] = map[string]any{"format": f}
		}
	}
	copyOpt(out, src, "temperature", "top_p", "stream", "parallel_tool_calls",
		"prompt_cache_key", "prompt_cache_retention")
	return sonic.Marshal(out)
}

// ---------------------------------------------------------------------------
// request: responses → anthropic
// ---------------------------------------------------------------------------

// responsesRequestTools returns the effective tool declarations of a
// responses request: top-level `tools` first, then the tools of every
// {type:"additional_tools"} input item (codex 0.145+ declares tools ONLY in
// such items, role:"developer" — a tool declaration, not a message), followed
// by tools materialized by client-executed tool_search_output items. The latter
// are active declarations for the next model turn, not merely display data.
func responsesRequestTools(src map[string]any) []any {
	var out []any
	if tools, ok := src["tools"].([]any); ok {
		out = append(out, tools...)
	}
	for _, item := range responsesInputItems(src["input"]) {
		switch item["type"] {
		case "additional_tools", "tool_search_output":
			if tools, ok := item["tools"].([]any); ok {
				out = append(out, tools...)
			}
		}
	}
	return out
}

// responsesToolSearchOutputText renders the client-executed Responses
// tool_search result as a tool-result message for protocols which lack the
// typed tool_search_output item. The exact wire names are intentionally kept
// in the result text: the model must call one of the newly materialized tools
// by that name on the following turn.
func responsesToolSearchOutputText(item map[string]any) (string, bool) {
	status := strings.ToLower(strOpt(item["status"]))
	isError := status != "" && status != "completed" && status != "success" && status != "succeeded"

	var names []string
	if tools, ok := item["tools"].([]any); ok {
		for _, entry := range nsExpandTools(tools) {
			tm := entry.tm
			switch strOpt(tm["type"]) {
			case "function", "custom":
				name := strOpt(tm["name"])
				if entry.namespace != "" && name != "" {
					name = nsFlattenName(entry.namespace, name)
				}
				if name != "" {
					names = append(names, name)
				}
			}
		}
	}
	if len(names) > 0 {
		return "Tool search loaded these exact tool names: " + strings.Join(names, ", "), isError
	}

	// Backward compatibility for older clients/proxies which used an opaque
	// output string instead of the current typed `tools` array.
	if _, exists := item["output"]; exists {
		text, markedError := splitToolResultError(responsesToolOutputText(item["output"]))
		return text, isError || markedError
	}
	if isError {
		return "Tool search failed (status: " + status + ").", true
	}
	return "Tool search returned no tools.", false
}

// responsesContentToAnthropicBlocks turns a Responses message item's content
// parts into anthropic content blocks.
func responsesContentToAnthropicBlocks(content any) []map[string]any {
	var out []map[string]any
	parts, ok := content.([]any)
	if !ok {
		return out
	}
	for _, p := range parts {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		switch pm["type"] {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": responsesTextWithCitationLinks(pm)})
		case "input_image", "image", "image_url":
			url := strOf(pm["image_url"])
			if ium := asMap(pm["image_url"]); ium != nil {
				url = strOf(ium["url"])
			}
			if mt, data, ok := parseDataURL(url); ok {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": mt, "data": data,
				}})
			} else if url != "" {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{
					"type": "url", "url": url,
				}})
			}
		case "input_file", "file":
			if block := responsesInputFileToAnthropicDocument(pm); block != nil {
				out = append(out, block)
			}
		default:
			convertWarn("dropping responses content part in r→a request: " + strOf(pm["type"]))
		}
	}
	return out
}

func responsesInputFileToAnthropicDocument(part map[string]any) map[string]any {
	filename := strOpt(part["filename"])
	var block map[string]any
	if u := strOpt(part["file_url"]); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		block = map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": u}}
	} else if id := strOpt(part["file_id"]); id != "" {
		block = map[string]any{"type": "document", "source": map[string]any{"type": "file", "file_id": id}}
	} else if mt, data, ok := parseDataURL(strOpt(part["file_data"])); ok && data != "" {
		block = map[string]any{"type": "document", "source": map[string]any{
			"type": "base64", "media_type": mt, "data": data,
		}}
	}
	if block != nil && filename != "" {
		block["title"] = filename
	}
	return block
}

func hostedCallID(item map[string]any) string {
	return firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]))
}

func hostedCallArguments(item map[string]any) string {
	if raw, ok := item["arguments"].(string); ok && raw != "" {
		return raw
	}
	if args := item["arguments"]; args != nil {
		if encoded, err := sonic.Marshal(args); err == nil {
			return string(encoded)
		}
	}
	if action := item["action"]; action != nil {
		if encoded, err := sonic.Marshal(action); err == nil {
			return string(encoded)
		}
	}
	return "{}"
}

func responsesWebSearchToAnthropicBlocks(item map[string]any) []map[string]any {
	id := firstNonEmpty(strOpt(item["id"]), strOpt(item["call_id"]), "web_search")
	input := asMap(item["action"])
	if input == nil {
		input = parseToolArgs(hostedCallArguments(item)).(map[string]any)
	}
	var result any = []any{}
	if strOpt(item["status"]) == "failed" {
		result = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	} else if sources, ok := item["sources"].([]any); ok {
		hits := make([]map[string]any, 0, len(sources))
		for _, raw := range sources {
			source := asMap(raw)
			if url := strOpt(source["url"]); url != "" {
				hits = append(hits, map[string]any{
					"type": "web_search_result", "title": strOpt(source["title"]), "url": url,
				})
			}
		}
		result = hits
	}
	return []map[string]any{
		{"type": "server_tool_use", "id": id, "name": "web_search", "input": input},
		{"type": "web_search_tool_result", "tool_use_id": id, "content": result},
	}
}

// responsesMessageText concatenates the text of a Responses message item's
// content parts (for folding system/developer items into a plain-text field).
func responsesMessageText(content any) string {
	parts, ok := content.([]any)
	if !ok {
		return strOpt(content)
	}
	var b strings.Builder
	for _, p := range parts {
		if pm := asMap(p); pm != nil {
			b.WriteString(strOf(pm["text"]))
		}
	}
	return b.String()
}

func responsesToolsToAnthropic(tools []any) ([]map[string]any, error) {
	var out []map[string]any
	seen := map[string]bool{}
	for _, e := range nsExpandTools(tools) {
		tm := e.tm
		toolType := strOf(tm["type"])
		if toolType == "web_search" || toolType == "web_search_preview" {
			// The hosted fallback occupies the plain name "web_search" — check
			// it against user tools like any other name (the r→chat converter
			// does the same), or Anthropic rejects duplicate tool names.
			if seen["web_search"] {
				return nil, fmt.Errorf("responses→anthropic tool-name collision after hosted-tool fallback: %q", "web_search")
			}
			seen["web_search"] = true
			at := map[string]any{"type": "web_search_20250305", "name": "web_search"}
			copyOpt(at, tm, "max_uses", "allowed_domains", "blocked_domains", "user_location")
			out = append(out, at)
			continue
		}
		if toolType == "tool_search" {
			if seen["tool_search"] {
				return nil, fmt.Errorf("responses→anthropic tool-name collision after hosted-tool fallback: %q", "tool_search")
			}
			seen["tool_search"] = true
			out = append(out, map[string]any{
				"name": "tool_search", "description": hostedToolDescription("tool_search"),
				"input_schema": normalizeAnthropicInputSchema(hostedToolSchema("tool_search")),
			})
			continue
		}
		if toolType != "function" {
			continue
		}
		name := strOf(tm["name"])
		if e.namespace != "" {
			name = nsFlattenName(e.namespace, name)
		}
		if name == "" {
			continue
		}
		if seen[name] {
			return nil, fmt.Errorf("responses→anthropic tool-name collision after namespace flattening: %q", name)
		}
		seen[name] = true
		at := map[string]any{"name": name}
		if d, ok := tm["description"]; ok {
			at["description"] = d
		}
		if p, ok := tm["parameters"]; ok {
			at["input_schema"] = normalizeAnthropicInputSchema(p)
		} else {
			at["input_schema"] = normalizeAnthropicInputSchema(nil)
		}
		out = append(out, at)
	}
	return out, nil
}

// responsesToolChoiceToAnthropic: "auto"→{type:auto}, "required"→{type:any},
// "none"→{type:none}, {type:function,name}→{type:tool,name}.
func responsesToolChoiceToAnthropic(tc any) any {
	if s, ok := tc.(string); ok {
		switch s {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
		return nil
	}
	tcm := asMap(tc)
	if tcm == nil {
		return nil
	}
	if t, _ := tcm["type"].(string); t == "function" {
		name := strOpt(tcm["name"])
		if namespace := strOpt(tcm["namespace"]); namespace != "" {
			name = nsFlattenName(namespace, name)
		}
		return map[string]any{"type": "tool", "name": name}
	}
	if t, _ := tcm["type"].(string); t == "tool_search" {
		return map[string]any{"type": "tool", "name": "tool_search"}
	}
	return nil
}

func convertResponsesRequestToAnthropic(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	out := map[string]any{}
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	var systemParts []string
	if ins, ok := src["instructions"].(string); ok && ins != "" {
		systemParts = append(systemParts, ins)
	}
	var msgs []map[string]any
	for _, item := range responsesInputItems(src["input"]) {
		switch item["type"] {
		case "message":
			role, _ := item["role"].(string)
			// Anthropic messages accept only user/assistant: system/developer
			// items fold into the TOP-LEVEL system field (cc-switch
			// transform_codex_anthropic; degrading to user would silently
			// change instruction precedence, passing role:"system" through 400s).
			if role == "system" || role == "developer" {
				if txt := responsesMessageText(item["content"]); txt != "" {
					systemParts = append(systemParts, txt)
				}
				continue
			}
			if role == "" {
				role = "user"
			}
			blocks := responsesContentToAnthropicBlocks(item["content"])
			if role == "assistant" || role == "user" {
				msgs = append(msgs, map[string]any{"role": role, "content": blocks})
			}
		case "function_call":
			args := parseToolArgs(strOf(item["arguments"]))
			name := strOpt(item["name"])
			if namespace := strOpt(item["namespace"]); namespace != "" {
				name = nsFlattenName(namespace, name)
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{
				"type":  "tool_use",
				"id":    firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
				"name":  name,
				"input": args,
			}}})
		case "tool_search_call":
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{
				"type": "tool_use", "id": hostedCallID(item), "name": "tool_search",
				"input": parseToolArgs(hostedCallArguments(item)),
			}}})
		case "tool_search_output":
			text, isError := responsesToolSearchOutputText(item)
			msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{{
				"type": "tool_result", "tool_use_id": hostedCallID(item), "content": text, "is_error": isError,
			}}})
		case "web_search_call":
			msgs = append(msgs, map[string]any{"role": "assistant", "content": responsesWebSearchToAnthropicBlocks(item)})
		case "function_call_output":
			// output may be a string OR a parts array (input_text/input_image);
			// array parts become anthropic blocks — text into the tool_result
			// text, images as native image blocks (base64/url source). r→a has
			// no vision gate (anthropic targets always accept image blocks).
			text, imgs := responsesOutputTextAndImages(item["output"])
			text, isError := splitToolResultError(text)
			var content any = text
			if len(imgs) > 0 {
				blocks := []map[string]any{{"type": "text", "text": text}}
				for _, im := range imgs {
					url := strKey(asMap(im["image_url"]), "url")
					if mt, data, ok := parseDataURL(url); ok {
						blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
							"type": "base64", "media_type": mt, "data": data,
						}})
					} else if url != "" {
						blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
							"type": "url", "url": url,
						}})
					}
				}
				content = blocks
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": strOpt(item["call_id"]),
				"content":     content,
				"is_error":    isError,
			}}})
		case "additional_tools":
			// Tool declaration (codex 0.145+), consumed via
			// responsesRequestTools — NOT a message; its role:"developer"
			// must not fold into `system` either.
			continue
		case "reasoning":
			text, sig := responsesReasoningText(item)
			var blk map[string]any
			if text == "" && sig != "" {
				// encrypted-only reasoning ↔ redacted_thinking (data verbatim).
				blk = map[string]any{"type": "redacted_thinking", "data": sig}
			} else {
				blk = map[string]any{"type": "thinking", "thinking": text}
				if sig != "" {
					blk["signature"] = sig
				}
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{blk}})
		default:
			convertWarn("dropping responses input item in r→a request: " + strOf(item["type"]))
		}
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if len(msgs) > 0 {
		msgs = mergeConsecutiveAnthropicRoles(msgs)
		// Anthropic requires the first message to be role:user; insert a minimal
		// placeholder when the converted list starts otherwise (e.g. input
		// beginning with a function_call) — same fix as chat→anthropic.
		if r, _ := msgs[0]["role"].(string); r != "user" {
			msgs = append([]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "."}}}}, msgs...)
		}
		out["messages"] = msgs
	}
	if tools := responsesRequestTools(src); len(tools) > 0 {
		at, err := responsesToolsToAnthropic(tools)
		if err != nil {
			return nil, err
		}
		if len(at) > 0 {
			out["tools"] = at
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if at := responsesToolChoiceToAnthropic(tc); at != nil {
			out["tool_choice"] = at
		}
	}
	// parallel_tool_calls:false → disable_parallel_tool_use:true. Only with
	// tools present; never on tool_choice none (mirrors the chat→a logic).
	if ptc, ok := src["parallel_tool_calls"].(bool); ok && !ptc {
		if toolsArr := responsesRequestTools(src); len(toolsArr) > 0 {
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
	if f := asMap(asMap(src["text"])["format"]); f != nil {
		convertWarn("dropping text.format (no anthropic equivalent)")
	}
	if r := asMap(src["reasoning"]); r != nil {
		if th := effortToThinking(strOf(r["effort"])); th != nil {
			out["thinking"] = th
		}
	}
	// Anthropic requires max_tokens; codex clients routinely omit
	// max_output_tokens (or send an explicit null), so inject the same
	// generous default the chat→a direction uses (convert.go).
	if v, ok := src["max_output_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = defaultAnthropicMaxTokens
	}
	copyOpt(out, src, "temperature", "top_p", "stream")
	return sonic.Marshal(out)
}

// ---------------------------------------------------------------------------
// request: responses → openai-chat
// ---------------------------------------------------------------------------

// responsesOutputTextAndImages splits a function_call_output `output` value
// (string or parts array) into concatenated text + image parts (as chat
// image_url parts) for media reinjection.
func responsesOutputTextAndImages(v any) (text string, imgs []map[string]any) {
	switch raw := v.(type) {
	case string:
		return raw, nil
	case []any:
		var b strings.Builder
		for _, p := range raw {
			pm := asMap(p)
			if pm == nil {
				continue
			}
			switch pm["type"] {
			case "input_text", "output_text", "text":
				text := strOf(pm["text"])
				if text == toolResultErrorMarker && b.Len() == 0 {
					b.WriteString(toolResultErrorMarker + "\n")
				} else {
					b.WriteString(text)
				}
			case "input_image", "image", "image_url":
				url := strOf(pm["image_url"])
				if ium := asMap(pm["image_url"]); ium != nil {
					url = strOf(ium["url"])
				}
				if url != "" {
					imgs = append(imgs, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		return b.String(), imgs
	}
	return strOf(v), nil
}

// responsesContentToChat converts a Responses message item's content parts to
// chat message content: a plain string when text-only (the common shape), or
// a parts array when images are present — an input_image must not be silently
// dropped on the responses→chat hop.
func responsesContentToChat(content any) any {
	parts, ok := content.([]any)
	if !ok {
		return chatContentText(content)
	}
	var out []map[string]any
	hasImage := false
	hasFile := false
	for _, p := range parts {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		switch pm["type"] {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": responsesTextWithCitationLinks(pm)})
		case "input_image", "image", "image_url":
			url := strOf(pm["image_url"])
			if ium := asMap(pm["image_url"]); ium != nil {
				url = strOf(ium["url"])
			}
			if url != "" {
				hasImage = true
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "input_file", "file":
			file := map[string]any{}
			copyOpt(file, pm, "file_id", "file_data", "filename")
			if file["file_id"] != nil || file["file_data"] != nil {
				hasFile = true
				out = append(out, map[string]any{"type": "file", "file": file})
			} else if u := strOpt(pm["file_url"]); u != "" {
				out = append(out, map[string]any{"type": "text", "text": "[document " + firstNonEmpty(strOpt(pm["filename"]), "file") + "] " + u})
			}
		default:
			convertWarn("dropping responses content part in r→chat request: " + strOf(pm["type"]))
		}
	}
	if !hasImage && !hasFile {
		return chatContentText(content)
	}
	return out
}

// responsesToolChoiceToOpenAI: "auto"/"none"/"required" passthrough;
// {type:"function", name, namespace?} flattens the namespace into the name
// (MCP); a namespace-selecting tool_choice degrades to "auto" (chat has no
// namespace selector).
func responsesToolChoiceToOpenAI(tc any) any {
	if s, ok := tc.(string); ok {
		return s
	}
	tcm := asMap(tc)
	if tcm == nil {
		return nil
	}
	switch t, _ := tcm["type"].(string); t {
	case "function":
		name := strOpt(tcm["name"])
		if ns := strOpt(tcm["namespace"]); ns != "" {
			name = nsFlattenName(ns, name)
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	case "namespace":
		return "auto"
	}
	return tcm
}

func convertResponsesRequestToOpenAI(body []byte) ([]byte, error) {
	return convertResponsesRequestToOpenAIFor(body, convertReqOpts{ImageOK: true})
}

// convertResponsesRequestToOpenAIFor is convertResponsesRequestToOpenAI with
// per-target options (reasoning-effort dialect + vision gate for media
// reinjection).
// r2chatWalk carries the chat-message list being built while walking
// Responses input items (the r→chat direction), plus the pending-reasoning
// attachment state that must survive between items.
type r2chatWalk struct {
	msgs             []map[string]any
	pendingReasoning string
	lastAssistant    int  // msgs index of the last assistant message; -1 = none yet
	imageOK          bool // target model accepts images (gates tool-output image re-injection)
}

// attachForward rides pendingReasoning on assistant message mi (cc-switch's
// rule: reasoning_content must ride on the assistant message — DeepSeek-style
// upstreams reject tool turns whose assistant message lacks it; a standalone
// reasoning assistant message breaks role expectations).
func (w *r2chatWalk) attachForward(mi int) {
	if w.pendingReasoning != "" && mi >= 0 {
		w.msgs[mi]["reasoning_content"] = w.pendingReasoning
		w.pendingReasoning = ""
	}
}

// attachBackward consumes pendingReasoning onto the LAST assistant message
// (appending when it already carries reasoning_content, cc-switch's
// append_reasoning_content "\n\n" separator). With no assistant to take it,
// the reasoning is dropped + warned — never carried forward.
func (w *r2chatWalk) attachBackward() {
	if w.pendingReasoning == "" {
		return
	}
	if w.lastAssistant < 0 {
		convertWarn("dropping reasoning with no assistant message to attach to (r→chat)")
		w.pendingReasoning = ""
		return
	}
	if prev := strOpt(w.msgs[w.lastAssistant]["reasoning_content"]); prev != "" {
		w.msgs[w.lastAssistant]["reasoning_content"] = prev + "\n\n" + w.pendingReasoning
	} else {
		w.msgs[w.lastAssistant]["reasoning_content"] = w.pendingReasoning
	}
	w.pendingReasoning = ""
}

// addMessage converts one "message" input item into a chat message.
func (w *r2chatWalk) addMessage(item map[string]any) {
	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	if role == "system" || role == "developer" {
		role = "system"
	}
	if role != "assistant" {
		// User/system turn boundary: consume pending reasoning backward
		// NOW so it cannot leak across a user turn into the next assistant
		// message (cc-switch transform_codex_chat.rs:1012-1045).
		w.attachBackward()
	} else if n := len(w.msgs); n > 0 && w.msgs[n-1]["role"] == "assistant" {
		// An assistant message interleaved between a tool call and its output
		// must not break the tool_calls→tool adjacency strict upstreams
		// require: merge its text into the pending assistant tool-call message
		// (content + tool_calls on one assistant message is valid chat).
		if _, hasCalls := w.msgs[n-1]["tool_calls"].([]map[string]any); hasCalls {
			if text, ok := responsesContentToChat(item["content"]).(string); ok && text != "" {
				if prev := strOpt(w.msgs[n-1]["content"]); prev != "" {
					w.msgs[n-1]["content"] = prev + "\n\n" + text
				} else {
					w.msgs[n-1]["content"] = text
				}
				w.attachBackward()
				return
			}
		}
	}
	w.msgs = append(w.msgs, map[string]any{"role": role, "content": responsesContentToChat(item["content"])})
	if role == "assistant" {
		w.lastAssistant = len(w.msgs) - 1
		w.attachForward(w.lastAssistant)
	}
}

// addToolCall converts one function / custom / hosted tool CALL item into an
// assistant tool_calls entry.
func (w *r2chatWalk) addToolCall(item map[string]any) {
	callID := firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]))
	var name, arguments string
	if item["type"] == "custom_tool_call" {
		// Custom/freeform call: raw string input wrapped as
		// {"input": <raw>} arguments for the wrapper function.
		name = strOpt(item["name"])
		arguments = wrapCustomCallArguments(strOpt(item["input"]))
	} else if item["type"] == "tool_search_call" {
		name = "tool_search"
		arguments = hostedCallArguments(item)
	} else if item["type"] == "web_search_call" {
		name = "web_search"
		arguments = hostedCallArguments(item)
	} else {
		// MCP namespace: history calls reference the flattened chat name;
		// the namespace field does not cross over.
		name = strOpt(item["name"])
		if ns := strOpt(item["namespace"]); ns != "" {
			name = nsFlattenName(ns, name)
		}
		arguments = firstNonEmpty(strKey(item, "arguments"), "{}")
	}
	tc := map[string]any{
		"id": callID, "type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
	// Append to the previous message if it is an assistant tool_calls
	// message; otherwise start a new one.
	if n := len(w.msgs); n > 0 && w.msgs[n-1]["role"] == "assistant" {
		if tcs, ok := w.msgs[n-1]["tool_calls"].([]map[string]any); ok {
			w.msgs[n-1]["tool_calls"] = append(tcs, tc)
			return
		}
	}
	w.msgs = append(w.msgs, map[string]any{"role": "assistant", "tool_calls": []map[string]any{tc}})
	w.lastAssistant = len(w.msgs) - 1
	w.attachForward(w.lastAssistant)
}

// addToolOutput converts one tool OUTPUT item. The output may be a string OR
// a parts array (input_text/input_image); images are reinjected as a
// synthetic user message (cc-switch), gated on the target model's vision (#6).
func (w *r2chatWalk) addToolOutput(item map[string]any) {
	var text string
	var imgs []map[string]any
	if item["type"] == "tool_search_output" {
		var isError bool
		text, isError = responsesToolSearchOutputText(item)
		text = markToolResultError(text, isError)
	} else {
		text, imgs = responsesOutputTextAndImages(item["output"])
	}
	if len(imgs) > 0 && !w.imageOK {
		text = appendMediaPlaceholder(text)
		imgs = nil
	}
	w.msgs = append(w.msgs, map[string]any{
		"role":         "tool",
		"tool_call_id": hostedCallID(item),
		"content":      text,
	})
	if len(imgs) > 0 {
		parts := []map[string]any{{"type": "text", "text": "[image returned by tool]"}}
		parts = append(parts, imgs...)
		w.msgs = append(w.msgs, map[string]any{"role": "user", "content": parts})
	}
}

// addReasoning accumulates reasoning-item text until it can attach to an
// assistant message (multi-item reasoning joins with "\n").
func (w *r2chatWalk) addReasoning(item map[string]any) {
	text, _ := responsesReasoningText(item)
	if w.pendingReasoning != "" && text != "" {
		w.pendingReasoning += "\n"
	}
	w.pendingReasoning += text
}

// finish normalizes the completed message list: trailing reasoning attaches
// backward to the last assistant message (appended, same as the boundary
// path); with no assistant at all, fall back to a standalone one (pinned our
// semantics — cc-switch drops it). System messages are pulled to the head,
// preserving relative order (cc-switch's collapse_system_messages_to_head —
// MiniMax-style upstreams reject mid-thread system). Placeholder
// reasoning_content (cc-switch): thinking-dialect upstreams (deepseek 400s
// "reasoning_content must be passed back"; kimi/Moonshot likewise) require
// EVERY assistant tool_calls message to carry it. Codex reasoning items are
// empty-summary + encrypted_content, so the attached reasoning_content is
// exactly empty here — inject the same placeholder cc-switch uses. Other
// dialects don't inject.
func (w *r2chatWalk) finish(reasoningDialect ReasoningDialect) []map[string]any {
	if w.pendingReasoning != "" {
		if w.lastAssistant >= 0 {
			w.attachBackward()
		} else {
			w.msgs = append(w.msgs, map[string]any{"role": "assistant", "reasoning_content": w.pendingReasoning})
			w.pendingReasoning = ""
		}
	}
	msgs := collapseSystemToHead(w.msgs)
	if reasoningDialect == ReasoningThinking {
		for _, m := range msgs {
			if m["role"] != "assistant" {
				continue
			}
			tcs, _ := m["tool_calls"].([]map[string]any)
			if len(tcs) == 0 {
				continue
			}
			if strOpt(m["reasoning_content"]) == "" {
				m["reasoning_content"] = "tool call"
			}
		}
	}
	return msgs
}

// applyResponsesRequestChatFields maps the request-level (non-item) fields of
// a Responses request onto the chat-completions output: tools (MCP namespace
// flattening fails CLOSED — the forward layer turns a conversion error into a
// 502), tool_choice, the vendor-specific reasoning-effort dialect, output
// caps, response_format and sampling params.
func applyResponsesRequestChatFields(out, src map[string]any, reasoningDialect ReasoningDialect) error {
	if tools := responsesRequestTools(src); len(tools) > 0 {
		ot, err := nsFlattenResponsesTools(tools)
		if err != nil {
			return err
		}
		if len(ot) > 0 {
			out["tools"] = ot
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if ot := responsesToolChoiceToOpenAI(tc); ot != nil {
			out["tool_choice"] = ot
		}
	}
	if r := asMap(src["reasoning"]); r != nil {
		effort := strOf(r["effort"])
		// reasoning.context (codex sends "all_turns") has no chat equivalent.
		if strOpt(r["context"]) != "" {
			convertWarn("dropping reasoning.context (no chat equivalent)")
		}
		// Reasoning effort dialect: chat vendors disagree on the field shape,
		// so the transport injects the target provider's dialect.
		switch reasoningDialect {
		case ReasoningThinking:
			if effort == "none" || effort == "minimal" {
				out["thinking"] = map[string]any{"type": "disabled"}
			} else {
				out["thinking"] = map[string]any{"type": "enabled"}
			}
		case ReasoningEnableThinking:
			out["enable_thinking"] = effort != "none" && effort != "minimal"
		case ReasoningOpenRouter:
			out["reasoning"] = map[string]any{"effort": effort}
		default:
			out["reasoning_effort"] = effort
		}
	}
	if v, ok := src["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if f := asMap(asMap(src["text"])["format"]); f != nil {
		if rf := textFormatToChatResponseFormat(f); rf != nil {
			out["response_format"] = rf
		}
	}
	copyOpt(out, src, "temperature", "top_p", "stream", "parallel_tool_calls",
		"prompt_cache_key", "prompt_cache_retention")
	// Streaming requests ask for a usage chunk (same as the a→chat direction):
	// kimi/MiniMax-style upstreams otherwise report all-zero stream usage.
	if b, ok := out["stream"].(bool); ok && b {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	// cc-switch issue #3557: tool_choice / parallel_tool_calls with NO tools is
	// rejected by several chat upstreams — drop both when the (filtered) tool
	// list is empty.
	if _, hasTools := out["tools"]; !hasTools {
		delete(out, "tool_choice")
		delete(out, "parallel_tool_calls")
	}
	return nil
}

func convertResponsesRequestToOpenAIFor(body []byte, opts convertReqOpts) ([]byte, error) {
	reasoningDialect := opts.ReasoningDialect
	if reasoningDialect == "" {
		reasoningDialect = ReasoningEffort
	}
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	out := map[string]any{}
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	walk := &r2chatWalk{lastAssistant: -1, imageOK: opts.ImageOK}
	if ins, ok := src["instructions"].(string); ok && ins != "" {
		walk.msgs = append(walk.msgs, map[string]any{"role": "system", "content": ins})
	}
	for _, item := range responsesInputItems(src["input"]) {
		switch item["type"] {
		case "message":
			walk.addMessage(item)
		case "function_call", "custom_tool_call", "tool_search_call", "web_search_call":
			walk.addToolCall(item)
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			walk.addToolOutput(item)
		case "additional_tools":
			// Tool declaration (codex 0.145+), consumed via
			// responsesRequestTools — NOT a message; its role:"developer"
			// must not enter the chat message stream.
			continue
		case "reasoning":
			walk.addReasoning(item)
		default:
			convertWarn("dropping responses input item in r→chat request: " + strOf(item["type"]))
		}
	}
	if msgs := walk.finish(reasoningDialect); len(msgs) > 0 {
		out["messages"] = msgs
	}
	if err := applyResponsesRequestChatFields(out, src, reasoningDialect); err != nil {
		return nil, err
	}
	return sonic.Marshal(out)
}

// collapseSystemToHead pulls all system messages to the front, preserving
// their relative order and the order of the remaining messages (cc-switch's
// collapse_system_messages_to_head — MiniMax-style upstreams reject
// mid-thread system messages).
func collapseSystemToHead(msgs []map[string]any) []map[string]any {
	var sys, rest []map[string]any
	for _, m := range msgs {
		if m["role"] == "system" {
			sys = append(sys, m)
		} else {
			rest = append(rest, m)
		}
	}
	return append(sys, rest...)
}

// ===========================================================================
// response (non-streaming)
// ===========================================================================

// responsesStatusToAnthropicStop maps a Responses status (+ the
// incomplete_details.reason) to an anthropic stop_reason. A function_call in
// the output → tool_use (regardless of status).
func responsesStatusToAnthropicStop(status, incReason string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}
	if status == "incomplete" {
		if incReason == "content_filter" {
			return "refusal"
		}
		return "max_tokens"
	}
	return "end_turn"
}

// responsesStatusToOpenAIFinish maps a Responses status (+ reason) to a chat
// finish_reason.
func responsesStatusToOpenAIFinish(status, incReason string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_calls"
	}
	if status == "incomplete" {
		if incReason == "content_filter" {
			return "content_filter"
		}
		return "length"
	}
	return "stop"
}

// anthropicStopToResponsesDetail maps an anthropic stop_reason to a Responses
// status + incomplete_details.reason (reason "" when completed). pause_turn has
// no Responses equivalent — best-effort completed.
func anthropicStopToResponsesDetail(stop string) (status, reason string) {
	switch stop {
	case "max_tokens":
		return "incomplete", "max_output_tokens"
	case "refusal":
		return "incomplete", "content_filter"
	default:
		return "completed", ""
	}
}

// openAIFinishToResponsesDetail maps a chat finish_reason to a Responses
// status + incomplete_details.reason.
func openAIFinishToResponsesDetail(finish string) (status, reason string) {
	switch finish {
	case "length":
		return "incomplete", "max_output_tokens"
	case "content_filter":
		return "incomplete", "content_filter"
	default:
		return "completed", ""
	}
}

// responsesOutputItems returns src["output"] as a slice of item maps.
func responsesOutputItems(src map[string]any) []map[string]any {
	raw, ok := src["output"].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, it := range raw {
		if m := asMap(it); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// --- response: responses → anthropic ---

func convertResponsesToAnthropic(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	// Fail-closed: a failed/cancelled response carrying an error must NOT be
	// wrapped as a normal end_turn message (the forward layer turns a
	// conversion error into a 502 before commit).
	if st := strOf(src["status"]); st == "failed" || st == "cancelled" {
		message := "upstream response " + st
		if e := asMap(src["error"]); e != nil {
			message = firstNonEmpty(strOpt(e["message"]), message)
		}
		return nil, fmt.Errorf("responses status %s: %s", st, message)
	}
	var blocks []map[string]any
	hasToolUse := false
	var textParts []map[string]any
	flushText := func() {
		for _, part := range textParts {
			blocks = append(blocks, map[string]any{"type": "text", "text": responsesTextWithCitationLinks(part)})
		}
		textParts = nil
	}
	for _, item := range responsesOutputItems(src) {
		switch item["type"] {
		case "message":
			if parts, ok := item["content"].([]any); ok {
				for _, p := range parts {
					if pm := asMap(p); pm != nil {
						switch pm["type"] {
						case "output_text", "text", "input_text":
							textParts = append(textParts, pm)
						case "refusal":
							// Refusal text is real content — map to a text
							// block (anthropic has no refusal block type;
							// stop_reason carries the semantics).
							textParts = append(textParts, map[string]any{"type": "text", "text": strOf(pm["refusal"])})
						default:
							convertWarn("dropping responses content part in r→a response: " + strOf(pm["type"]))
						}
					}
				}
			}
		case "function_call":
			flushText()
			hasToolUse = true
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
				"name":  strOpt(item["name"]),
				"input": parseToolArgs(strOf(item["arguments"])),
			})
		case "tool_search_call":
			flushText()
			hasToolUse = true
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": hostedCallID(item), "name": "tool_search",
				"input": parseToolArgs(hostedCallArguments(item)),
			})
		case "web_search_call":
			flushText()
			blocks = append(blocks, responsesWebSearchToAnthropicBlocks(item)...)
		case "reasoning":
			flushText()
			text, sig := responsesReasoningText(item)
			var blk map[string]any
			if text == "" && sig != "" {
				blk = map[string]any{"type": "redacted_thinking", "data": sig}
			} else {
				blk = map[string]any{"type": "thinking", "thinking": text}
				if sig != "" {
					blk["signature"] = sig
				}
			}
			blocks = append(blocks, blk)
		default:
			convertWarn("dropping responses output item in r→a response: " + strOf(item["type"]))
		}
	}
	flushText()
	if len(blocks) == 0 {
		blocks = []map[string]any{{"type": "text", "text": ""}}
	}
	out := map[string]any{
		"id":      strOf(src["id"]),
		"type":    "message",
		"role":    "assistant",
		"content": blocks,
	}
	if m, ok := src["model"]; ok {
		out["model"] = m
	}
	status := strOf(src["status"])
	incReason := strKey(asMap(src["incomplete_details"]), "reason")
	out["stop_reason"] = responsesStatusToAnthropicStop(status, incReason, hasToolUse)
	out["usage"] = responsesUsageToAnthropic(src["usage"])
	return sonic.Marshal(out)
}

// joinTextParts concatenates the `text` field of content-part maps.
func joinTextParts(parts []map[string]any) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strOf(p["text"]))
	}
	return b.String()
}

// responsesUsageToAnthropic maps a Responses usage object to anthropic usage.
// responses input_tokens INCLUDES cached AND cache-write tokens
// (input_tokens_details); anthropic counts both separately from input_tokens —
// split them out, clamped ≥0 (same convention as chat→anthropic). The direct
// cache_creation_input_tokens spelling wins over details.cache_write_tokens.
func responsesUsageToAnthropic(u any) map[string]any {
	um := asMap(u)
	if um == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	cached := intOf(asMap(um["input_tokens_details"])["cached_tokens"])
	cacheCreate := intOf(um["cache_creation_input_tokens"])
	if cacheCreate == 0 {
		cacheCreate = intOf(asMap(um["input_tokens_details"])["cache_write_tokens"])
	}
	inTok := intOf(um["input_tokens"]) - cached - cacheCreate
	if inTok < 0 {
		inTok = 0
	}
	out := map[string]any{
		"input_tokens":  inTok,
		"output_tokens": intOf(um["output_tokens"]),
	}
	if cached > 0 {
		out["cache_read_input_tokens"] = cached
	}
	if cacheCreate > 0 {
		out["cache_creation_input_tokens"] = cacheCreate
	}
	return out
}

// intOf coerces a JSON number to int (sonic decodes to float64).
func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	if i, ok := v.(int); ok {
		return i
	}
	return 0
}

// --- response: responses → openai-chat ---

func convertResponsesToOpenAI(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	// Fail-closed (same as convertResponsesToAnthropic).
	if st := strOf(src["status"]); st == "failed" || st == "cancelled" {
		message := "upstream response " + st
		if e := asMap(src["error"]); e != nil {
			message = firstNonEmpty(strOpt(e["message"]), message)
		}
		return nil, fmt.Errorf("responses status %s: %s", st, message)
	}
	var contentText strings.Builder
	var annotations []map[string]any
	var toolCalls []map[string]any
	var reasoning strings.Builder
	for _, item := range responsesOutputItems(src) {
		switch item["type"] {
		case "message":
			if parts, ok := item["content"].([]any); ok {
				for _, p := range parts {
					if pm := asMap(p); pm != nil {
						switch pm["type"] {
						case "output_text", "text", "input_text":
							base := utf8.RuneCountInString(contentText.String())
							for _, annotation := range responsesAnnotationsToChat(pm["annotations"]) {
								citation := asMap(annotation["url_citation"])
								citation["start_index"] = intOf(citation["start_index"]) + base
								citation["end_index"] = intOf(citation["end_index"]) + base
								annotations = append(annotations, annotation)
							}
							contentText.WriteString(strOf(pm["text"]))
						case "refusal":
							// Refusal text is real content (finish_reason
							// carries the refusal semantics).
							contentText.WriteString(strOf(pm["refusal"]))
						default:
							convertWarn("dropping responses content part in r→chat response: " + strOf(pm["type"]))
						}
					}
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
				"type": "function",
				"function": map[string]any{
					"name":      strOpt(item["name"]),
					"arguments": strOf(item["arguments"]),
				},
			})
		case "tool_search_call", "web_search_call":
			name := "tool_search"
			if item["type"] == "web_search_call" {
				name = "web_search"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": hostedCallID(item), "type": "function",
				"function": map[string]any{"name": name, "arguments": hostedCallArguments(item)},
			})
		case "reasoning":
			text, _ := responsesReasoningText(item)
			reasoning.WriteString(text)
		default:
			convertWarn("dropping responses output item in r→chat response: " + strOf(item["type"]))
		}
	}
	msg := map[string]any{"role": "assistant"}
	if contentText.Len() > 0 {
		msg["content"] = contentText.String()
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(annotations) > 0 {
		msg["annotations"] = annotations
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	status := strOf(src["status"])
	incReason := strKey(asMap(src["incomplete_details"]), "reason")
	finish := responsesStatusToOpenAIFinish(status, incReason, len(toolCalls) > 0)
	choice := map[string]any{
		"index":         0,
		"message":       msg,
		"finish_reason": finish,
	}
	out := map[string]any{
		"id":      strOf(src["id"]),
		"object":  "chat.completion",
		"choices": []map[string]any{choice},
	}
	if m, ok := src["model"]; ok {
		out["model"] = m
	}
	out["usage"] = responsesUsageToOpenAI(src["usage"])
	return sonic.Marshal(out)
}

func responsesUsageToOpenAI(u any) map[string]any {
	um := asMap(u)
	in, out := 0, 0
	cached, reasoning := 0, 0
	if um != nil {
		in = intOf(um["input_tokens"])
		out = intOf(um["output_tokens"])
		cached = intOf(asMap(um["input_tokens_details"])["cached_tokens"])
		reasoning = intOf(asMap(um["output_tokens_details"])["reasoning_tokens"])
	}
	m := map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
	if cached > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoning > 0 {
		m["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return m
}

// --- response: anthropic → responses ---

func convertAnthropicResponseToResponses(body []byte) ([]byte, error) {
	return convertAnthropicResponseToResponsesNS(body, r2cCtx{})
}

func convertAnthropicResponseToResponsesNS(body []byte, r2c r2cCtx) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse anthropic response: %w", err)
	}
	var output []map[string]any
	webSearchInputs := map[string]map[string]any{}
	var textParts []map[string]any
	flushText := func() {
		if len(textParts) > 0 {
			output = append(output, map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": textParts,
			})
			textParts = nil
		}
	}
	if raw, ok := src["content"].([]any); ok {
		for _, blk := range raw {
			b := asMap(blk)
			if b == nil {
				continue
			}
			switch b["type"] {
			case "text":
				part := map[string]any{"type": "output_text", "text": strOf(b["text"])}
				if annotations := anthropicCitationsToResponses(b["citations"], strOf(b["text"])); len(annotations) > 0 {
					part["annotations"] = annotations
				}
				textParts = append(textParts, part)
			case "tool_use":
				flushText()
				args := marshalToolInput(b["input"])
				name := strOf(b["name"])
				item := map[string]any{
					"type": "function_call", "status": "completed",
					"id": strOf(b["id"]), "call_id": strOf(b["id"]),
					"name": name, "arguments": args,
				}
				if original, namespace, ok := r2c.restoreName(name); ok {
					item["name"] = original
					item["namespace"] = namespace
				}
				output = append(output, item)
			case "server_tool_use":
				flushText()
				if strOpt(b["name"]) == "web_search" {
					webSearchInputs[strOpt(b["id"])] = asMap(b["input"])
				} else {
					convertWarn("dropping unsupported anthropic server_tool_use in a→r response: " + strOpt(b["name"]))
				}
			case "web_search_tool_result":
				flushText()
				id := strOpt(b["tool_use_id"])
				item := map[string]any{
					"type": "web_search_call", "id": id, "status": "completed", "action": webSearchInputs[id],
				}
				switch content := b["content"].(type) {
				case []any:
					var sources []map[string]any
					for _, raw := range content {
						hit := asMap(raw)
						if hit["type"] == "web_search_result" && strOpt(hit["url"]) != "" {
							sources = append(sources, map[string]any{"url": hit["url"], "title": hit["title"]})
						}
					}
					item["sources"] = sources
				case map[string]any:
					if content["type"] == "web_search_tool_result_error" {
						item["status"] = "failed"
					}
				}
				output = append(output, item)
			case "thinking":
				flushText()
				item := map[string]any{
					"type": "reasoning", "status": "completed",
					"summary": []map[string]any{{"type": "summary_text", "text": strOf(b["thinking"])}},
				}
				if sig, _ := b["signature"].(string); sig != "" {
					item["encrypted_content"] = sig
				}
				output = append(output, item)
			case "redacted_thinking":
				flushText()
				item := map[string]any{"type": "reasoning", "status": "completed", "summary": []any{}}
				if data, _ := b["data"].(string); data != "" {
					item["encrypted_content"] = data
				}
				output = append(output, item)
			default:
				convertWarn("dropping anthropic content block in a→r response: " + strOf(b["type"]))
			}
		}
	}
	flushText()
	if len(output) == 0 {
		output = []map[string]any{{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": ""}},
		}}
	}
	status, incReason := anthropicStopToResponsesDetail(strOf(src["stop_reason"]))
	out := map[string]any{
		"id":     strOf(src["id"]),
		"object": "response",
		"status": status,
		"output": output,
	}
	if incReason != "" {
		out["incomplete_details"] = map[string]any{"reason": incReason}
	}
	if m, ok := src["model"]; ok {
		out["model"] = m
	}
	out["usage"] = anthropicUsageToResponses(src["usage"])
	return sonic.Marshal(out)
}

// --- response: openai-chat → responses ---

func convertOpenAIResponseToResponses(body []byte) ([]byte, error) {
	return convertOpenAIResponseToResponsesNS(body, r2cCtx{})
}

// convertOpenAIResponseToResponsesNS converts with the responses→chat
// context: MCP namespace restore + custom/freeform tool unwrapping (zero
// value = no-op for both).
func convertOpenAIResponseToResponsesNS(body []byte, r2c r2cCtx) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	if asMap(src["error"]) != nil {
		return nil, fmt.Errorf("openai error envelope cannot be converted as a successful response")
	}
	choices, choicesOK := src["choices"].([]any)
	if !choicesOK || len(choices) == 0 {
		return nil, fmt.Errorf("openai response has no choices")
	}
	var output []map[string]any
	finish := ""
	var textParts []string
	var chatAnnotations []map[string]any
	flushText := func() {
		if len(textParts) > 0 {
			output = append(output, map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": strings.Join(textParts, "")}},
			})
			textParts = nil
		}
	}
	if ch := asMap(choices[0]); ch != nil {
		finish = strOf(ch["finish_reason"])
		if msg := asMap(ch["message"]); msg != nil {
			if c, ok := msg["content"].(string); ok && c != "" {
				// MiniMax-style inline thinking: a LEADING <think>…</think>
				// block inside content splits into a reasoning item
				// (cc-switch split_leading_think_block); mid-text blocks
				// stay literal.
				if r, answer, ok := splitLeadingThinkBlock(c); ok {
					if r != "" {
						output = append(output, map[string]any{
							"type": "reasoning", "status": "completed",
							"summary": []map[string]any{{"type": "summary_text", "text": r}},
						})
					}
					c = answer
				}
				if c != "" {
					textParts = append(textParts, c)
				}
			}
			chatAnnotations = chatAnnotationsToResponses(msg["annotations"])
			if rc := chatReasoningText(msg); rc != "" {
				flushText()
				output = append(output, map[string]any{
					"type": "reasoning", "status": "completed",
					"summary": []map[string]any{{"type": "summary_text", "text": rc}},
				})
			}
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm := asMap(tc)
					if tcm == nil {
						continue
					}
					flushText()
					fn := asMap(tcm["function"])
					args := strOpt(fnMap(fn, "arguments"))
					if args == "" {
						if raw := fnMap(fn, "arguments"); raw != nil {
							args = strOf(raw)
						}
					}
					if args == "" {
						args = "{}"
					}
					name := strOpt(fnMap(fn, "name"))
					// Custom/freeform call: unwrap {"input": "<raw>"} back
					// to a custom_tool_call item (raw string input).
					if r2c.custom[name] {
						output = append(output, map[string]any{
							"type": "custom_tool_call", "status": "completed",
							"id":      strOpt(tcm["id"]),
							"call_id": strOpt(tcm["id"]),
							"name":    name,
							"input":   unwrapCustomCallArguments(args),
						})
						continue
					}
					item := map[string]any{
						"type": "function_call", "status": "completed",
						"id":        strOpt(tcm["id"]),
						"call_id":   strOpt(tcm["id"]),
						"name":      name,
						"arguments": args,
					}
					// MCP namespace restore: a chat name that was flattened
					// from {namespace, name} round-trips back to二维.
					if orig, ns, ok := r2c.restoreName(name); ok {
						item["name"] = orig
						item["namespace"] = ns
					}
					output = append(output, item)
				}
			}
		}
	}
	flushText()
	if len(chatAnnotations) > 0 {
		for i := len(output) - 1; i >= 0; i-- {
			item := output[i]
			if item["type"] != "message" {
				continue
			}
			content := anySlice(item["content"])
			if len(content) > 0 {
				asMap(content[0])["annotations"] = chatAnnotations
			}
			break
		}
	}
	if len(output) == 0 {
		output = []map[string]any{{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": ""}},
		}}
	}
	status, incReason := openAIFinishToResponsesDetail(finish)
	out := map[string]any{
		"id":     strOf(src["id"]),
		"object": "response",
		"status": status,
		"output": output,
	}
	if incReason != "" {
		out["incomplete_details"] = map[string]any{"reason": incReason}
	}
	if m, ok := src["model"]; ok {
		out["model"] = m
	}
	out["usage"] = openAIUsageToResponses(src["usage"])
	return sonic.Marshal(out)
}

// anthropicUsageToResponses maps anthropic usage to Responses usage. Responses
// input_tokens is INCLUSIVE of cache traffic (same convention as
// anthropic→chat prompt_tokens): input + cache_read + cache_creation, with
// cache reads surfaced via input_tokens_details.
func anthropicUsageToResponses(u any) map[string]any {
	um := asMap(u)
	in, out := 0, 0
	cached, created := 0, 0
	if um != nil {
		in = intOf(um["input_tokens"])
		out = intOf(um["output_tokens"])
		cached = intOf(um["cache_read_input_tokens"])
		created = intOf(um["cache_creation_input_tokens"])
	}
	total := in + cached + created
	m := map[string]any{"input_tokens": total, "output_tokens": out, "total_tokens": total + out}
	if cached > 0 {
		m["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return m
}

func openAIUsageToResponses(u any) map[string]any {
	um := asMap(u)
	in, out := 0, 0
	cached, reasoning := 0, 0
	if um != nil {
		in = intOf(um["prompt_tokens"])
		out = intOf(um["completion_tokens"])
		cached = intOf(asMap(um["prompt_tokens_details"])["cached_tokens"])
		reasoning = intOf(asMap(um["completion_tokens_details"])["reasoning_tokens"])
	}
	m := map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out}
	if cached > 0 {
		m["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoning > 0 {
		m["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return m
}
