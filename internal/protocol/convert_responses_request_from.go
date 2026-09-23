// convert_responses_request_from.go — request converters reading Responses-API
// shaped histories (responses→anthropic, responses→openai-chat). Split from
// convert_responses.go, which keeps the shared helpers.
package protocol

import (
	"fmt"
	sonic "github.com/bytedance/sonic"
	"strings"
)

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
func responsesContentToAnthropicBlocks(content any, d *Diagnostics) []map[string]any {
	var out []map[string]any
	parts, ok := content.([]any)
	if !ok {
		// String shorthand ({role, content: "…"} message items) becomes a
		// single text block, mirroring responsesMessageText.
		if s := strOpt(content); s != "" {
			out = append(out, map[string]any{"type": "text", "text": s})
		}
		return out
	}
	for _, p := range parts {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		switch pm["type"] {
		case "input_text", "output_text", "text":
			// Empty text violates anthropic's minLength:1 — skip it (an empty
			// part carries no information; citation links make text non-empty).
			if text := responsesTextWithCitationLinks(pm); text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
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
			if block := responsesInputFileToAnthropicDocument(pm, d); block != nil {
				out = append(out, block)
			}
		default:
			warnDiag(d, "unknown_part", "dropping responses content part in r→a request: "+strOf(pm["type"]))
		}
	}
	return out
}

func responsesInputFileToAnthropicDocument(part map[string]any, d *Diagnostics) map[string]any {
	filename := strOpt(part["filename"])
	var block map[string]any
	if u := strOpt(part["file_url"]); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		block = map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": u}}
	} else if id := strOpt(part["file_id"]); id != "" && strOpt(part["file_data"]) == "" {
		block = map[string]any{"type": "text", "text": degradeFileIDText(id, filename, d)}
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

func responsesWebSearchToAnthropicBlocks(item map[string]any, d *Diagnostics) []map[string]any {
	id := firstNonEmpty(strOpt(item["id"]), strOpt(item["call_id"]), "web_search")
	input := asMap(item["action"])
	if input == nil {
		input = parseToolArgs(hostedCallArguments(item), d).(map[string]any)
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

func convertResponsesRequestToAnthropic(body []byte, d *Diagnostics) ([]byte, error) {
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
	// Anthropic constrains tool_use ids to ^[a-zA-Z0-9_-]+$; responses call_ids
	// are free-form ("call.a"). The memo keeps use/result pairing identical to
	// the chat→a direction.
	normID := newToolIDNormalizer()
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
			blocks := responsesContentToAnthropicBlocks(item["content"], d)
			// Empty content (e.g. a single empty text part) must not emit
			// "content":null / an empty blocks array — same len guard as the
			// chat→a sibling (convert.go).
			if (role == "assistant" || role == "user") && len(blocks) > 0 {
				msgs = append(msgs, map[string]any{"role": role, "content": blocks})
			}
		case "function_call":
			args := parseToolArgs(strOf(item["arguments"]), d)
			name := strOpt(item["name"])
			if namespace := strOpt(item["namespace"]); namespace != "" {
				name = nsFlattenName(namespace, name)
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{
				"type":  "tool_use",
				"id":    normID.use(firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]))),
				"name":  name,
				"input": args,
			}}})
		case "tool_search_call":
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{
				"type": "tool_use", "id": hostedCallID(item), "name": "tool_search",
				"input": parseToolArgs(hostedCallArguments(item), d),
			}}})
		case "tool_search_output":
			text, isError := responsesToolSearchOutputText(item)
			msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{{
				"type": "tool_result", "tool_use_id": hostedCallID(item), "content": text, "is_error": isError,
			}}})
		case "web_search_call":
			msgs = append(msgs, map[string]any{"role": "assistant", "content": responsesWebSearchToAnthropicBlocks(item, d)})
		case "function_call_output":
			// output may be a string OR a parts array (input_text/input_image);
			// array parts become anthropic blocks — text into the tool_result
			// text, images as native image blocks (base64/url source). r→a has
			// no vision gate (anthropic targets always accept image blocks).
			text, imgs := responsesOutputTextAndImages(item["output"])
			text, isError := splitToolResultError(text)
			var content any = text
			if len(imgs) > 0 {
				var blocks []map[string]any
				// An empty leading text block violates anthropic's minLength:1
				// (parts array carrying only images leaves text "").
				if text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
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
				// All images unparseable and no text: keep the (possibly empty)
				// string form — a valid tool_result — rather than an empty
				// content array.
				if len(blocks) > 0 {
					content = blocks
				}
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": normID.result(strOpt(item["call_id"])),
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
			if text == "" && sig == "" {
				// Anthropic may reject an empty thinking block with no
				// signature — drop the item observably.
				warnDiag(d, "reasoning_dropped", "dropping empty reasoning item in r→a request (no summary, no encrypted_content)")
				continue
			}
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
			warnDiag(d, "unknown_item", "dropping responses input item in r→a request: "+strOf(item["type"]))
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
		warnDiag(d, "response_format_dropped", "dropping text.format (no anthropic equivalent)")
	}
	// Anthropic requires max_tokens; codex clients routinely omit
	// max_output_tokens (or send an explicit null), so inject the same
	// generous default the chat→a direction uses (convert.go). Computed
	// BEFORE reasoning: the thinking budget clamps below this value.
	if v, ok := src["max_output_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = defaultAnthropicMaxTokens
	}
	if r := asMap(src["reasoning"]); r != nil {
		if th := effortToThinking(normalizeReasoningEffort(d, strOf(r["effort"])), intOf(out["max_tokens"])); th != nil {
			out["thinking"] = th
		}
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
func responsesContentToChat(content any, d *Diagnostics) any {
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
			if id := strOpt(pm["file_id"]); id != "" && strOpt(pm["file_data"]) == "" {
				if u := strOpt(pm["file_url"]); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
					// file_id + file_url: the URL is the transportable form
					// for a chat target (r→a prefers it the same way); only
					// the provider-scoped file_id itself degrades.
					out = append(out, map[string]any{"type": "text", "text": "[document " + firstNonEmpty(strOpt(pm["filename"]), "file") + "] " + u})
					break
				}
				out = append(out, map[string]any{"type": "text", "text": degradeFileIDText(id, strOpt(pm["filename"]), d)})
				break
			}
			file := map[string]any{}
			copyOpt(file, pm, "file_id", "file_data", "filename")
			if file["file_id"] != nil || file["file_data"] != nil {
				hasFile = true
				out = append(out, map[string]any{"type": "file", "file": file})
			} else if u := strOpt(pm["file_url"]); u != "" {
				out = append(out, map[string]any{"type": "text", "text": "[document " + firstNonEmpty(strOpt(pm["filename"]), "file") + "] " + u})
			}
		default:
			warnDiag(d, "unknown_part", "dropping responses content part in r→chat request: "+strOf(pm["type"]))
		}
	}
	if !hasImage && !hasFile {
		// Fold the PRODUCED parts back to a plain string — re-walking the
		// original content (chatContentText) would drop the degrade/URL
		// notes this converter just synthesized when a file part is the
		// only content (degradations must stay observable).
		var b strings.Builder
		for _, p := range out {
			b.WriteString(strOf(p["text"]))
		}
		return b.String()
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
	diag             *Diagnostics
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
		warnDiag(w.diag, "reasoning_dropped", "dropping reasoning with no assistant message to attach to (r→chat)")
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
			if text, ok := responsesContentToChat(item["content"], w.diag).(string); ok && text != "" {
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
	w.msgs = append(w.msgs, map[string]any{"role": role, "content": responsesContentToChat(item["content"], w.diag)})
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
		warnDiagf(w.diag, "media_degraded",
			"target has no vision: %d tool-output image(s) collapsed to placeholder text", len(imgs))
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
// 502), tool_choice, the vendor-specific reasoning-effort dialect (switch
// shape from ReasoningDialect, plus the provider effort-enum layer from
// ReasoningEffortEnum/ReasoningEffortOnly), output caps, response_format and
// sampling params.
func applyResponsesRequestChatFields(d *Diagnostics, out, src map[string]any, reasoningDialect ReasoningDialect, opts convertReqOpts) error {
	if tools := responsesRequestTools(src); len(tools) > 0 {
		ot, err := nsFlattenResponsesTools(d, tools)
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
		// Unknown non-empty efforts (codex "persistent" etc.) clamp down to
		// high before any dialect renders them.
		effort := normalizeReasoningEffort(d, strOf(r["effort"]))
		enum := opts.ReasoningEffortEnum
		// reasoning.context (codex sends "all_turns") has no chat equivalent.
		if strOpt(r["context"]) != "" {
			warnDiag(d, "reasoning_context_dropped", "dropping reasoning.context (no chat equivalent)")
		}
		// Reasoning effort dialect: chat vendors disagree on the field shape,
		// so the transport injects the target provider's dialect. On top of
		// the switch shape, providers whose chat endpoint accepts a
		// NON-pass-through effort enum (internal/provider.ChatEffortProfile)
		// also emit the mapped reasoning_effort value.
		switch reasoningDialect {
		case ReasoningThinking:
			if opts.ReasoningEffortOnly {
				// The enum REPLACES the thinking switch (kimi-k3 rejects
				// thinking+reasoning_effort together).
				if v, ok := enum[effort]; ok && effort != "" {
					out["reasoning_effort"] = v
				} else {
					if effort != "" {
						warnDiagf(d, "unknown_effort", "effort profile has no entry for %q, falling back to thinking switch", effort)
					}
					if effort == "none" {
						out["thinking"] = map[string]any{"type": "disabled"}
					} else {
						out["thinking"] = map[string]any{"type": "enabled"}
					}
				}
				break
			}
			// With an enum the minimal rung maps to a real low level, so only
			// "none" disables; without one keep the legacy none/minimal→disabled.
			if effort == "none" || (effort == "minimal" && enum == nil) {
				out["thinking"] = map[string]any{"type": "disabled"}
			} else {
				out["thinking"] = map[string]any{"type": "enabled"}
			}
			if v, ok := enum[effort]; ok && effort != "" {
				out["reasoning_effort"] = v
			}
		case ReasoningEnableThinking:
			out["enable_thinking"] = effort != "none"
			if effort != "none" {
				if v, ok := enum[effort]; ok && effort != "" {
					out["reasoning_effort"] = v
				}
			}
		case ReasoningOpenRouter:
			out["reasoning"] = map[string]any{"effort": effort}
		default:
			// reasoning_effort dialect + enum (step-plan): the endpoint accepts
			// the same field but only a RESTRICTED enum (low|medium|high), so a
			// registered profile maps the canonical rung through it; nil enum
			// keeps the pure pass-through.
			if v, ok := enum[effort]; ok && effort != "" {
				out["reasoning_effort"] = v
			} else {
				out["reasoning_effort"] = effort
			}
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
	walk := &r2chatWalk{lastAssistant: -1, imageOK: opts.ImageOK, diag: opts.Diag}
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
			warnDiag(opts.Diag, "unknown_item", "dropping responses input item in r→chat request: "+strOf(item["type"]))
		}
	}
	if msgs := walk.finish(reasoningDialect); len(msgs) > 0 {
		out["messages"] = msgs
	}
	if err := applyResponsesRequestChatFields(opts.Diag, out, src, reasoningDialect, opts); err != nil {
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
	case "max_tokens", "model_context_window_exceeded":
		// Both are truncations; the Responses incomplete-details enum has no
		// context-window value, so context exhaustion reports as
		// max_output_tokens (same value as the streaming direction).
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
