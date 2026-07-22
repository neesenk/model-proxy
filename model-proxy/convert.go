package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	sonic "github.com/bytedance/sonic"
)

// convert.go implements PROTOCOL CONVERSION (#11): let a client speak one protocol
// (Anthropic /v1/messages or OpenAI /v1/chat/completions) while the upstream
// backend speaks the OTHER, with FULL tool-call support (request + response +
// streaming, both directions) plus text/system/max_tokens/temperature/stop/
// stream/images.
//
// The converters are pure (testable in isolation); the forward path only invokes
// them when a target's declared protocol differs from the client's, so the
// default same-protocol path stays byte-identical.
//
// Out of scope (dropped + warned, never silently): thinking/redacted_thinking
// blocks, cache_control breakpoints, server-side tools (web_search/computer/...),
// images inside tool_result, and logprobs. Claude Code's client tools
// (bash/edit/...) are ordinary function tools and convert normally.

// needsConversion reports whether a client protocol and a target's declared
// backend protocol differ (and thus conversion applies). Either empty = "same as
// client" (no conversion).
func needsConversion(clientProto, targetProto string) bool {
	if targetProto == "" || targetProto == clientProto {
		return false
	}
	return (clientProto == "anthropic" && targetProto == "openai") ||
		(clientProto == "openai" && targetProto == "anthropic")
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
		// Server-side/built-in tools declare a `type` that isn't a plain function
		// tool (which has name+input_schema, no `type`). Drop them.
		if bt, ok := t["type"].(string); ok && bt != "" {
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
	case "tool_use", "tool_result":
		return nil // handled at message level
	case "thinking", "redacted_thinking":
		convertWarn("dropping " + strOf(blk["type"]) + " block (no cross-protocol equivalent)")
		return nil
	}
	return nil
}

// anthropicMsgToOpenAIMsgs converts one anthropic message to one or more openai
// messages. A user message carrying tool_result blocks expands to separate openai
// `tool` messages (one per result) plus a `user` message for any text/image.
func anthropicMsgToOpenAIMsgs(m map[string]any) []map[string]any {
	role, _ := m["role"].(string)
	content := m["content"]
	var out []map[string]any

	if role == "assistant" {
		om := map[string]any{"role": "assistant"}
		var textParts []map[string]any
		var toolCalls []map[string]any
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
				} else {
					if p := anthropicContentBlockToOpenAIPart(blk); p != nil {
						textParts = append(textParts, p)
					}
				}
			}
		} else if s, ok := content.(string); ok && s != "" {
			om["content"] = s
		}
		if len(textParts) == 1 {
			om["content"] = textParts[0]["text"]
		} else if len(textParts) > 1 {
			om["content"] = textParts
		} else if len(toolCalls) > 0 && len(textParts) == 0 {
			om["content"] = nil // assistant turn is all tool calls
		}
		if len(toolCalls) > 0 {
			om["tool_calls"] = toolCalls
		}
		return []map[string]any{om}
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
				txt := anthropicToolResultText(blk["content"])
				out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": txt})
				continue
			}
			if p := anthropicContentBlockToOpenAIPart(blk); p != nil {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			um := map[string]any{"role": "user"}
			if len(parts) == 1 {
				um["content"] = parts[0]["text"]
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
// text blocks); images inside tool_result are warned + dropped.
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
			if t, _ := m["type"].(string); t == "text" || t == "" {
				if s, _ := m["text"].(string); s != "" {
					b.WriteString(s)
				}
			} else {
				convertWarn("dropping non-text block inside tool_result: " + t)
			}
		}
		return b.String()
	}
	return ""
}

// convertAnthropicRequestToOpenAI transforms an Anthropic /v1/messages body into
// an OpenAI /v1/chat/completions body (full tools support).
func convertAnthropicRequestToOpenAI(body []byte) ([]byte, error) {
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
				msgs = append(msgs, anthropicMsgToOpenAIMsgs(mm)...)
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
			at["input_schema"] = p
		}
		out = append(out, at)
	}
	return out
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

// convertOpenAIRequestToAnthropic transforms an OpenAI /v1/chat/completions body
// into an Anthropic /v1/messages body (full tools support). max_tokens is required
// by Anthropic; if absent a generous default is injected.
func convertOpenAIRequestToAnthropic(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}
	// This converter handles ONLY Chat Completions (`messages`). The OpenAI
	// Responses API (/v1/responses) carries its payload in `input` (a list) and
	// is labeled the same "openai" protocol, so a cross-protocol route feeds a
	// Responses body in here. Rather than read only `messages`, find none, and
	// silently emit an empty-messages Anthropic request (dropping the whole
	// prompt), fail closed so the proxy skips the target. Per
	// docs/architecture/protocol-conversion.md the `openai` flavor is Chat
	// Completions, not Responses; real Responses↔Anthropic conversion is unimplemented.
	if _, hasInput := src["input"]; hasInput {
		return nil, fmt.Errorf("openai→anthropic conversion supports only Chat Completions (messages); got a Responses-style `input` body — not convertible")
	}
	out := map[string]any{}
	for _, k := range []string{"model", "temperature", "top_p"} {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	if mt, ok := src["max_tokens"]; ok {
		out["max_tokens"] = mt
	} else {
		out["max_tokens"] = 4096 // Anthropic requires it
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
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": normID(tid),
				"content":     strOf(tm["content"]),
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
		case "system":
			systemText += strOf(mm["content"])
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
			var blocks []map[string]any
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
	if !needsConversion(clientProto, targetProto) {
		return body, nil
	}
	if clientProto == "anthropic" && targetProto == "openai" {
		return convertAnthropicRequestToOpenAI(body)
	}
	return convertOpenAIRequestToAnthropic(body)
}

// --- response (non-streaming) ---

// mapFinishToStopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
func mapFinishToStopReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
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
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls []any           `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	var content []map[string]any
	stopReason := "end_turn"
	if len(src.Choices) > 0 {
		c := src.Choices[0]
		// content may be a string or null.
		if len(c.Message.Content) > 0 && string(c.Message.Content) != "null" {
			var cv any
			sonic.Unmarshal(c.Message.Content, &cv)
			if s, ok := cv.(string); ok {
				content = append(content, map[string]any{"type": "text", "text": s})
			} else if parts, ok := cv.([]any); ok {
				for _, p := range parts {
					if blk := openaiContentPartToAnthropicBlock(asMap(p)); blk != nil {
						content = append(content, blk)
					}
				}
			}
		}
		if len(c.Message.ToolCalls) > 0 {
			content = append(content, openaiToolCallsToAnthropic(c.Message.ToolCalls)...)
		}
		stopReason = mapFinishToStopReason(c.FinishReason)
	}
	if content == nil {
		content = []map[string]any{}
	}
	// openai counts cached tokens as a SUBSET of prompt_tokens; anthropic counts
	// input_tokens excluding cache reads. Split them out (clamped ≥0).
	cached := src.Usage.PromptDetails.CachedTokens
	inTok := src.Usage.PromptTokens - cached
	if inTok < 0 {
		inTok = 0
	}
	usage := map[string]any{"input_tokens": inTok, "output_tokens": src.Usage.CompletionTokens}
	if cached > 0 {
		usage["cache_read_input_tokens"] = cached
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
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
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
	for _, c := range src.Content {
		switch c.Type {
		case "text":
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

// convertResponse converts a non-streaming response body from the target protocol
// back to the client protocol.
func convertResponse(body []byte, clientProto, targetProto string) ([]byte, error) {
	if !needsConversion(clientProto, targetProto) {
		return body, nil
	}
	if targetProto == "openai" && clientProto == "anthropic" {
		return convertOpenAIResponseToAnthropic(body)
	}
	return convertAnthropicResponseToOpenAI(body)
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
	// INCLUDES cached tokens; anthropic's input_tokens excludes them (clamp ≥0).
	inTok := t.inTok - t.cachedTok
	if inTok < 0 {
		inTok = 0
	}
	usage := map[string]any{"input_tokens": inTok, "output_tokens": t.outTok}
	if t.cachedTok > 0 {
		usage["cache_read_input_tokens"] = t.cachedTok
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
			}
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
					Content   string `json:"content"`
					ToolCalls []struct {
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
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
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
		}
		t.ensureStart()
		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			if c.Delta.Content != "" {
				t.openText()
				t.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": t.curIdx,
					"delta": map[string]any{"type": "text_delta", "text": c.Delta.Content},
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
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
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
				}
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
	if backendProto == "anthropic" {
		return "/v1/messages"
	}
	return "/chat/completions"
}

// convertSSEReader wraps an upstream SSE body reader with the right streaming
// transformer for the conversion direction. Caller has verified needsConversion.
func convertSSEReader(r io.Reader, clientProto, targetProto, model string) io.Reader {
	if targetProto == "openai" && clientProto == "anthropic" {
		return newOpenAIToAnthropicSSE(r, model)
	}
	return newAnthropicToOpenAISSE(r, model)
}
