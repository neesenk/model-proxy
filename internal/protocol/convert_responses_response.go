// convert_responses_response.go — Responses-API response (non-streaming)
// converters: responses→anthropic, responses→openai-chat, anthropic→responses,
// openai-chat→responses. Split from convert_responses.go; request converters
// and shared helpers live there.

package protocol

import (
	"fmt"
	sonic "github.com/bytedance/sonic"
	"strings"
	"unicode/utf8"
)

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
	// Anthropic tool_use id charset is ^[a-zA-Z0-9_-]+$; sanitize free-form
	// call_ids with the same per-conversion memo as the request direction.
	normID := newToolIDNormalizer()
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
				"id":    normID.use(firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]))),
				"name":  strOpt(item["name"]),
				"input": parseToolArgs(strOf(item["arguments"]), nil),
			})
		case "tool_search_call":
			flushText()
			hasToolUse = true
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": hostedCallID(item), "name": "tool_search",
				"input": parseToolArgs(hostedCallArguments(item), nil),
			})
		case "web_search_call":
			flushText()
			blocks = append(blocks, responsesWebSearchToAnthropicBlocks(item, nil)...)
		case "reasoning":
			text, sig := responsesReasoningText(item)
			if text == "" && sig == "" {
				// Anthropic may reject an empty thinking block with no
				// signature — drop the item observably (same rule as the
				// reasoning_details replay path).
				convertWarn("dropping empty reasoning item in r→a response (no summary, no encrypted_content)")
				continue
			}
			flushText()
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
					"arguments": firstNonEmpty(strOpt(item["arguments"]), "{}"),
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
		"created": responsesCreatedAt(src),
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
	// Synthesized items carry stable ids in the same msg_item_/rs_item_
	// convention the streaming converters use (ResponseOutputMessage and
	// ReasoningItem both REQUIRE id; ReasoningItem also requires summary).
	msgSeq, rsSeq := 0, 0
	flushText := func() {
		if len(textParts) > 0 {
			output = append(output, map[string]any{
				"id":   "msg_item_" + itoa(msgSeq),
				"type": "message", "role": "assistant", "status": "completed",
				"content": textParts,
			})
			msgSeq++
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
				// annotations is REQUIRED on ResponseOutputText (real upstreams
				// always send it, empty when there are no citations).
				part := map[string]any{"type": "output_text", "text": strOf(b["text"]), "annotations": []any{}}
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
					"id":   "rs_item_" + itoa(rsSeq),
					"type": "reasoning", "status": "completed",
					"summary": []map[string]any{{"type": "summary_text", "text": strOf(b["thinking"])}},
				}
				rsSeq++
				if sig, _ := b["signature"].(string); sig != "" {
					item["encrypted_content"] = sig
				}
				output = append(output, item)
			case "redacted_thinking":
				flushText()
				item := map[string]any{
					"id":   "rs_item_" + itoa(rsSeq),
					"type": "reasoning", "status": "completed", "summary": []any{},
				}
				rsSeq++
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
			"id":   "msg_item_" + itoa(msgSeq),
			"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": "", "annotations": []any{}}},
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
	responsesRequiredKeys(out, incReason != "")
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
	// Synthesized items carry stable ids in the same msg_item_/rs_item_/
	// fc_item_ convention the streaming converters use (OutputMessage and
	// ReasoningItem both REQUIRE id).
	msgSeq, rsSeq, fcSeq := 0, 0, 0
	flushText := func() {
		if len(textParts) > 0 {
			output = append(output, map[string]any{
				"id":   "msg_item_" + itoa(msgSeq),
				"type": "message", "role": "assistant", "status": "completed",
				// annotations is REQUIRED on ResponseOutputText (real upstreams
				// always send it, empty when there are no citations).
				"content": []map[string]any{{"type": "output_text", "text": strings.Join(textParts, ""), "annotations": []any{}}},
			})
			msgSeq++
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
							"id":   "rs_item_" + itoa(rsSeq),
							"type": "reasoning", "status": "completed",
							"summary": []map[string]any{{"type": "summary_text", "text": r}},
						})
						rsSeq++
					}
					c = answer
				}
				if c != "" {
					textParts = append(textParts, c)
				}
			}
			// Message-level refusal with empty/null content: Responses
			// represents it as a refusal content part on the output message
			// (dropping it would synthesize an empty output_text); the
			// status/incomplete semantics still ride on finish_reason.
			if ref := strOpt(msg["refusal"]); ref != "" && len(textParts) == 0 {
				output = append(output, map[string]any{
					"id":   "msg_item_" + itoa(msgSeq),
					"type": "message", "role": "assistant", "status": "completed",
					"content": []map[string]any{{"type": "refusal", "refusal": ref}},
				})
				msgSeq++
			}
			chatAnnotations = chatAnnotationsToResponses(msg["annotations"])
			if rc := chatReasoningText(msg); rc != "" {
				flushText()
				output = append(output, map[string]any{
					"id":   "rs_item_" + itoa(rsSeq),
					"type": "reasoning", "status": "completed",
					"summary": []map[string]any{{"type": "summary_text", "text": rc}},
				})
				rsSeq++
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
					// Missing tool_call id: fall back to a synthesized
					// fc_item_<idx> id, same as the streaming converter (a ""
					// id/call_id is a protocol violation upstream).
					callID := firstNonEmpty(strOpt(tcm["id"]), "fc_item_"+itoa(fcSeq))
					fcSeq++
					// Custom/freeform call: unwrap {"input": "<raw>"} back
					// to a custom_tool_call item (raw string input).
					if r2c.custom[name] {
						output = append(output, map[string]any{
							"type": "custom_tool_call", "status": "completed",
							"id":      callID,
							"call_id": callID,
							"name":    name,
							"input":   unwrapCustomCallArguments(args),
						})
						continue
					}
					item := map[string]any{
						"type": "function_call", "status": "completed",
						"id":        callID,
						"call_id":   callID,
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
			"id":   "msg_item_" + itoa(msgSeq),
			"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": "", "annotations": []any{}}},
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
	responsesRequiredKeys(out, incReason != "")
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
