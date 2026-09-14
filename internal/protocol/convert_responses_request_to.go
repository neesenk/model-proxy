// convert_responses_request_to.go — request converters targeting the Responses
// API (anthropic→responses, openai-chat→responses). Split from
// convert_responses.go, which keeps the shared helpers.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	sonic "github.com/bytedance/sonic"
	"strings"
)

// ---------------------------------------------------------------------------
// request: anthropic → responses
// ---------------------------------------------------------------------------

// anthropicMsgToResponsesItems turns one anthropic message into one or more
// Responses `input` items. text/image blocks collect into a `message` item;
// tool_use → function_call, tool_result → function_call_output, thinking →
// reasoning — each its own item (Responses separates them out of the message).
func anthropicMsgToResponsesItems(m map[string]any, imageOK bool, d *Diagnostics) []map[string]any {
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
				if file := anthropicDocumentToResponsesPart(b, d); file != nil {
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
					warnDiag(d, "server_tool_dropped", "dropping unsupported anthropic server_tool_use in a→r request: "+strOpt(b["name"]))
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
				txt := anthropicToolResultText(b["content"], d)
				imgs := anthropicToolResultImagesResponses(b["content"])
				if len(imgs) > 0 && !imageOK {
					warnDiagf(d, "media_degraded",
						"target has no vision: %d tool-result image(s) collapsed to placeholder text", len(imgs))
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
				warnDiag(d, "block_dropped", "dropping anthropic content block in a→r request: "+strOf(b["type"]))
			}
		}
	}
	flush()
	return items
}

func anthropicDocumentToResponsesPart(block map[string]any, d *Diagnostics) map[string]any {
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
			return map[string]any{"type": "input_text", "text": degradeFileIDText(id, filename, d)}
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
func dropOrphanReasoningItems(d *Diagnostics, items []map[string]any, start int) []map[string]any {
	for _, it := range items[start:] {
		if it["type"] == "message" || it["type"] == "function_call" {
			return items
		}
	}
	out := items[:start]
	for _, it := range items[start:] {
		if it["type"] == "reasoning" {
			warnDiag(d, "orphan_reasoning_dropped", "dropping orphan reasoning item (no following message/function_call in the same assistant turn)")
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

func anthropicToolsToResponses(tools []any, d *Diagnostics) []map[string]any {
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
				// The Responses web_search tool schema takes
				// filters.allowed_domains and has no max_uses/blocked_domains —
				// those anthropic-only fields drop with a warning rather than
				// landing as invalid top-level Responses fields.
				if domains, ok := tm["allowed_domains"].([]any); ok && len(domains) > 0 {
					rt["filters"] = map[string]any{"allowed_domains": domains}
				}
				for _, k := range []string{"max_uses", "blocked_domains"} {
					if _, ok := tm[k]; ok {
						convertWarn("dropping anthropic-only web_search tool field in a→r request: " + k)
					}
				}
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
	return convertAnthropicRequestToResponsesV(body, true, nil)
}

// convertAnthropicRequestToResponsesV is convertAnthropicRequestToResponses
// with the target model's vision capability (media reinjection gate).
func convertAnthropicRequestToResponsesV(body []byte, imageOK bool, d *Diagnostics) ([]byte, error) {
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
		if txt := anthropicTextOf(sys, d); txt != "" {
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
				if txt := anthropicTextOf(mm["content"], d); txt != "" {
					instructionParts = append(instructionParts, txt)
				}
				continue
			}
			start := len(input)
			input = append(input, anthropicMsgToResponsesItems(mm, imageOK, d)...)
			if strOpt(mm["role"]) == "assistant" {
				input = dropOrphanReasoningItems(d, input, start)
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
		if rt := anthropicToolsToResponses(tools, d); len(rt) > 0 {
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
		warnDiag(d, "stop_dropped", "dropping stop_sequences (Responses API has no stop parameter)")
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
func anthropicExplicitPromptCacheKey(src, converted map[string]any, d *Diagnostics) string {
	if !anthropicHasExplicitCacheControl(src) {
		return ""
	}
	if uid := strOpt(asMap(src["metadata"])["user_id"]); uid != "" {
		return sha256Hex32(uid)
	}
	fp, _ := sonic.ConfigStd.MarshalToString(map[string]any{
		"model": strOpt(converted["model"]),
		// nil collector: the main conversion walk over the same system
		// blocks already reports cache_control_dropped — collecting here
		// too duplicated the diagnostic (convertWarn dedupes, Diag didn't).
		"system": anthropicTextOf(src["system"], nil),
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
func chatMsgToResponsesItems(m map[string]any, d *Diagnostics) []map[string]any {
	role, _ := m["role"].(string)
	if role == "tool" {
		out := map[string]any{
			"type":    "function_call_output",
			"call_id": strOpt(m["tool_call_id"]),
			"output":  chatContentText(m["content"]),
		}
		return []map[string]any{out}
	}
	if role == "function" {
		// Legacy pre-tool_calls shape: Responses has no "function" role
		// (passing it through as a message role 400s upstream). A named
		// function result rides as a function_call_output item; legacy
		// messages carry no call id, so the caller pairs it with the matching
		// earlier function_call by name (pairLegacyFunctionOutputs).
		name := strOpt(m["name"])
		if name == "" || m["content"] == nil {
			convertWarn("dropping legacy role:function message without name/content in chat→r request")
			return nil
		}
		return []map[string]any{{
			"type":    "function_call_output",
			"call_id": strOpt(m["tool_call_id"]),
			"name":    name, // pairing marker, stripped by pairLegacyFunctionOutputs
			"output":  chatContentText(m["content"]),
		}}
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
					part := map[string]any{"type": "input_image", "image_url": strOf(ium["url"])}
					// Responses input_image supports detail — preserve it.
					if detail := strOpt(ium["detail"]); detail != "" {
						part["detail"] = detail
					}
					parts = append(parts, part)
				}
			case "input_file", "file":
				file := map[string]any{"type": "input_file"}
				// Chat nests the fields under "file"; responses keeps them flat.
				fm := asMap(pm["file"])
				if fm == nil {
					fm = pm
				}
				if id := strOpt(fm["file_id"]); id != "" {
					if strOpt(fm["file_data"]) == "" && strOpt(fm["file_url"]) == "" {
						parts = append(parts, map[string]any{"type": "text", "text": degradeFileIDText(id, strOpt(fm["filename"]), d)})
						continue
					}
					// file_id alongside an inline/URL source: the source is the
					// transportable form, so keep it and drop only the
					// provider-scoped id (r→chat/r→a prefer it the same way).
					warnFileIDDropped(id, d)
				}
				copyOpt(file, fm, "file_data", "file_url", "filename")
				if len(file) > 1 {
					parts = append(parts, file)
				}
			default:
				warnDiag(d, "unknown_part", "dropping chat content part in chat→r request: "+strOf(pm["type"]))
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
	// Legacy pre-tool_calls assistant function_call (no tool_calls array):
	// map to a function_call item when name+arguments are present; the
	// synthesized call_id lets a following legacy role:"function" message
	// pair with it by name.
	if role == "assistant" {
		if tcs, ok := m["tool_calls"].([]any); !ok || len(tcs) == 0 {
			if fc := asMap(m["function_call"]); fc != nil {
				name := strOpt(fc["name"])
				args := strOpt(fc["arguments"])
				if name == "" || args == "" {
					convertWarn("dropping assistant legacy function_call without name/arguments in chat→r request: " + firstNonEmpty(name, "<unnamed>"))
				} else {
					items = append(items, map[string]any{
						"type":      "function_call",
						"call_id":   "legacy_fc_" + name,
						"name":      name,
						"arguments": args,
					})
				}
			}
		}
	}
	return items
}

// pairLegacyFunctionOutputs pairs legacy role:"function" results (converted
// to function_call_output items carrying a name but no call_id) with the most
// recent earlier function_call of the same name; unpairable ones drop + warn
// (a function_call_output without call_id 400s upstream). The temporary
// "name" pairing marker is always stripped.
func pairLegacyFunctionOutputs(items []map[string]any) []map[string]any {
	hasLegacy := false
	for _, it := range items {
		if it["type"] == "function_call_output" && strOpt(it["name"]) != "" {
			hasLegacy = true
			break
		}
	}
	if !hasLegacy {
		return items
	}
	lastByName := map[string]string{}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		switch it["type"] {
		case "function_call":
			if n := strOpt(it["name"]); n != "" {
				lastByName[n] = strOpt(it["call_id"])
			}
		case "function_call_output":
			if name := strOpt(it["name"]); name != "" {
				delete(it, "name")
				if strOpt(it["call_id"]) == "" {
					id := lastByName[name]
					if id == "" {
						convertWarn("dropping legacy role:function message with no matching function_call in chat→r request: " + name)
						continue
					}
					it["call_id"] = id
				}
			}
		}
		out = append(out, it)
	}
	return out
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
func chatResponseFormatToTextFormat(d *Diagnostics, rf map[string]any) map[string]any {
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
			warnDiag(d, "empty_json_schema_dropped", "dropping empty json_schema response_format (no schema)")
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

func convertOpenAIRequestToResponses(body []byte, d *Diagnostics) ([]byte, error) {
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
			input = append(input, chatMsgToResponsesItems(mm, d)...)
		}
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	// Replace-style clients resend a tool_call carrying only the id (no name)
	// in later turns — backfill from earlier items with the same call_id
	// (opencodex's chat inbound does the same).
	backfillToolNames(d, input)
	// Legacy role:"function" results pair with their function_call by name.
	input = pairLegacyFunctionOutputs(input)
	if len(input) > 0 {
		out["input"] = input
	}
	// Legacy request-level params have no Responses equivalent (tools/
	// tool_choice replaced them); never pass them through silently.
	if _, ok := src["functions"]; ok {
		convertWarn("dropping legacy `functions` parameter in chat→r request (declare `tools` instead)")
	}
	if _, ok := src["function_call"]; ok {
		convertWarn("dropping legacy `function_call` parameter in chat→r request (use `tool_choice` instead)")
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
		// summary:"auto" asks the backend to stream reasoning summaries
		// (opencodex sets it unconditionally); without it codex responses
		// carry no reasoning items at all.
		out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
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
			warnDiag(d, "stop_dropped", "dropping stop (Responses API has no stop parameter)")
		}
	case string:
		if stops != "" {
			warnDiag(d, "stop_dropped", "dropping stop (Responses API has no stop parameter)")
		}
	}
	if rf := asMap(src["response_format"]); rf != nil {
		if f := chatResponseFormatToTextFormat(d, rf); f != nil {
			out["text"] = map[string]any{"format": f}
		}
	}
	copyOpt(out, src, "temperature", "top_p", "stream", "parallel_tool_calls",
		"prompt_cache_key", "prompt_cache_retention")
	return sonic.Marshal(out)
}
