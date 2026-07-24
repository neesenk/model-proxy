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
// Stateless: Responses' previous_response_id (server-side state) is dropped — the
// full history rides in `input`, equivalent to anthropic/chat `messages`.
package main

import (
	"fmt"
	"strings"

	sonic "github.com/bytedance/sonic"
)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

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

// responsesReasoningText extracts (text, signature) from a Responses reasoning
// item's summary array (+ encrypted_content as signature).
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
	return
}

// ---------------------------------------------------------------------------
// request: anthropic → responses
// ---------------------------------------------------------------------------

// anthropicMsgToResponsesItems turns one anthropic message into one or more
// Responses `input` items. text/image blocks collect into a `message` item;
// tool_use → function_call, tool_result → function_call_output, thinking →
// reasoning — each its own item (Responses separates them out of the message).
func anthropicMsgToResponsesItems(m map[string]any) []map[string]any {
	role, _ := m["role"].(string)
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}

	var items []map[string]any
	var parts []map[string]any
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
				parts = append(parts, map[string]any{"type": partType, "text": strOf(b["text"])})
			case "image":
				if src := asMap(b["source"]); src != nil {
					if mt, _ := src["media_type"].(string); mt != "" {
						if data, _ := src["data"].(string); data != "" {
							parts = append(parts, map[string]any{
								"type":      "input_image",
								"image_url": "data:" + mt + ";base64," + data,
							})
						}
					}
				}
			case "tool_use":
				flush()
				args, _ := sonic.MarshalString(b["input"])
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   firstNonEmpty(strOf(b["id"]), strOf(b["name"])),
					"name":      strOf(b["name"]),
					"arguments": args,
				})
			case "tool_result":
				flush()
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": strOf(b["tool_use_id"]),
					"output":  anthropicTextOf(b["content"]),
				})
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
			}
		}
	}
	flush()
	return items
}

func anthropicToolsToResponses(tools []any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		tm := asMap(t)
		if tm == nil {
			continue
		}
		rt := map[string]any{"type": "function", "name": strOf(tm["name"])}
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
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	out := map[string]any{}
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	if sys, ok := src["system"]; ok {
		if txt := anthropicTextOf(sys); txt != "" {
			out["instructions"] = txt
		}
	}
	var input []map[string]any
	if raw, ok := src["messages"].([]any); ok {
		for _, m := range raw {
			if mm := asMap(m); mm != nil {
				input = append(input, anthropicMsgToResponsesItems(mm)...)
			}
		}
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
	}
	if thinking, ok := src["thinking"].(map[string]any); ok {
		if effort := thinkingBudgetToEffort(thinking); effort != "" {
			out["reasoning"] = map[string]any{"effort": effort}
		}
	}
	if v, ok := src["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	copyOpt(out, src, "temperature", "top_p", "stream")
	return sonic.Marshal(out)
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
			"call_id": strOf(m["tool_call_id"]),
			"output":  chatContentText(m["content"]),
		}
		return []map[string]any{out}
	}
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	var items []map[string]any
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
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   strOf(tcm["id"]),
				"name":      strOf(firstNonEmpty(strOf(fnMap(fn, "name")), strOf(tcm["name"]))),
				"arguments": strOf(fnMap(fn, "arguments")),
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
		rt := map[string]any{"type": "function", "name": strOf(fnMap(fn, "name"))}
		if d := fnMap(fn, "description"); d != nil {
			rt["description"] = d
		}
		if p := fnMap(fn, "parameters"); p != nil {
			rt["parameters"] = p
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
	instructionsSet := false
	if raw, ok := src["messages"].([]any); ok {
		for _, m := range raw {
			mm := asMap(m)
			if mm == nil {
				continue
			}
			role, _ := mm["role"].(string)
			if (role == "system" || role == "developer") && !instructionsSet {
				if txt := chatContentText(mm["content"]); txt != "" {
					out["instructions"] = txt
					instructionsSet = true
					continue
				}
			}
			input = append(input, chatMsgToResponsesItems(mm)...)
		}
	}
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
	if v, ok := src["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	copyOpt(out, src, "temperature", "top_p", "stream")
	return sonic.Marshal(out)
}

// ---------------------------------------------------------------------------
// request: responses → anthropic
// ---------------------------------------------------------------------------

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
			out = append(out, map[string]any{"type": "text", "text": strOf(pm["text"])})
		case "input_image", "image", "image_url":
			url := strOf(pm["image_url"])
			if ium := asMap(pm["image_url"]); ium != nil {
				url = strOf(ium["url"])
			}
			if mt, data, ok := parseDataURL(url); ok {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": mt, "data": data,
				}})
			}
		}
	}
	return out
}

func responsesToolsToAnthropic(tools []any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		tm := asMap(t)
		if tm == nil {
			continue
		}
		if strOf(tm["type"]) != "function" {
			continue
		}
		at := map[string]any{"name": strOf(tm["name"])}
		if d, ok := tm["description"]; ok {
			at["description"] = d
		}
		if p, ok := tm["parameters"]; ok {
			at["input_schema"] = p
		}
		out = append(out, at)
	}
	return out
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
		return map[string]any{"type": "tool", "name": strOf(tcm["name"])}
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
	if ins, ok := src["instructions"].(string); ok && ins != "" {
		out["system"] = ins
	}
	var msgs []map[string]any
	for _, item := range responsesInputItems(src["input"]) {
		switch item["type"] {
		case "message":
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			blocks := responsesContentToAnthropicBlocks(item["content"])
			if role == "assistant" || role == "user" || role == "system" {
				msgs = append(msgs, map[string]any{"role": role, "content": blocks})
			}
		case "function_call":
			args := parseToolArgs(strOf(item["arguments"]))
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{
				"type": "tool_use",
				"id":   firstNonEmpty(strOf(item["call_id"]), strOf(item["id"])),
				"name": strOf(item["name"]),
				"input": args,
			}}})
		case "function_call_output":
			msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": strOf(item["call_id"]),
				"content":     strOf(item["output"]),
			}}})
		case "reasoning":
			text, sig := responsesReasoningText(item)
			blk := map[string]any{"type": "thinking", "thinking": text}
			if sig != "" {
				blk["signature"] = sig
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{blk}})
		}
	}
	if len(msgs) > 0 {
		out["messages"] = mergeConsecutiveAnthropicRoles(msgs)
	}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		if at := responsesToolsToAnthropic(tools); len(at) > 0 {
			out["tools"] = at
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if at := responsesToolChoiceToAnthropic(tc); at != nil {
			out["tool_choice"] = at
		}
	}
	if r := asMap(src["reasoning"]); r != nil {
		if th := effortToThinking(strOf(r["effort"])); th != nil {
			out["thinking"] = th
		}
	}
	if v, ok := src["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	copyOpt(out, src, "temperature", "top_p", "stream")
	return sonic.Marshal(out)
}

// ---------------------------------------------------------------------------
// request: responses → openai-chat
// ---------------------------------------------------------------------------

func responsesToolsToOpenAI(tools []any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		tm := asMap(t)
		if tm == nil || strOf(tm["type"]) != "function" {
			continue
		}
		fn := map[string]any{"name": strOf(tm["name"])}
		if d, ok := tm["description"]; ok {
			fn["description"] = d
		}
		if p, ok := tm["parameters"]; ok {
			fn["parameters"] = p
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func responsesToolChoiceToOpenAI(tc any) any {
	if s, ok := tc.(string); ok {
		return s
	}
	tcm := asMap(tc)
	if tcm == nil {
		return nil
	}
	if t, _ := tcm["type"].(string); t == "function" {
		return map[string]any{"type": "function", "function": map[string]any{"name": strOf(tcm["name"])}}
	}
	return tcm
}

func convertResponsesRequestToOpenAI(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	out := map[string]any{}
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	var msgs []map[string]any
	if ins, ok := src["instructions"].(string); ok && ins != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}
	for _, item := range responsesInputItems(src["input"]) {
		switch item["type"] {
		case "message":
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			if role == "system" || role == "developer" {
				role = "system"
			}
			msgs = append(msgs, map[string]any{"role": role, "content": chatContentText(item["content"])})
		case "function_call":
			callID := firstNonEmpty(strOf(item["call_id"]), strOf(item["id"]))
			tc := map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{
					"name":      strOf(item["name"]),
					"arguments": strOf(item["arguments"]),
				},
			}
			// Append to the previous message if it is an assistant tool_calls
			// message; otherwise start a new one.
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "assistant" {
				if tcs, ok := msgs[n-1]["tool_calls"].([]map[string]any); ok {
					msgs[n-1]["tool_calls"] = append(tcs, tc)
					continue
				}
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "tool_calls": []map[string]any{tc}})
		case "function_call_output":
			msgs = append(msgs, map[string]any{
				"role":          "tool",
				"tool_call_id":  strOf(item["call_id"]),
				"content":       strOf(item["output"]),
			})
		case "reasoning":
			// Reasoning in the input is rare; attach as reasoning_content on a
			// fresh assistant message (best-effort, lossy if interleaved).
			text, _ := responsesReasoningText(item)
			msgs = append(msgs, map[string]any{"role": "assistant", "reasoning_content": text})
		}
	}
	if len(msgs) > 0 {
		out["messages"] = msgs
	}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		if ot := responsesToolsToOpenAI(tools); len(ot) > 0 {
			out["tools"] = ot
		}
	}
	if tc, ok := src["tool_choice"]; ok {
		if ot := responsesToolChoiceToOpenAI(tc); ot != nil {
			out["tool_choice"] = ot
		}
	}
	if r := asMap(src["reasoning"]); r != nil {
		out["reasoning_effort"] = strOf(r["effort"])
	}
	if v, ok := src["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	copyOpt(out, src, "temperature", "top_p", "stream")
	return sonic.Marshal(out)
}

// ===========================================================================
// response (non-streaming)
// ===========================================================================

// responsesStatusToAnthropicStop maps a Responses status to an anthropic
// stop_reason. A function_call in the output → tool_use (regardless of status).
func responsesStatusToAnthropicStop(status string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}
	switch status {
	case "incomplete":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// responsesStatusToOpenAIFinish maps a Responses status to a chat finish_reason.
func responsesStatusToOpenAIFinish(status string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_calls"
	}
	switch status {
	case "incomplete":
		return "length"
	default:
		return "stop"
	}
}

func anthropicStopToResponsesStatus(stop string) string {
	switch stop {
	case "max_tokens":
		return "incomplete"
	default:
		return "completed"
	}
}

func openAIFinishToResponsesStatus(finish string) string {
	switch finish {
	case "length":
		return "incomplete"
	default:
		return "completed"
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
	var blocks []map[string]any
	hasToolUse := false
	var textParts []map[string]any
	flushText := func() {
		if len(textParts) > 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": joinTextParts(textParts)})
			textParts = nil
		}
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
						}
					}
				}
			}
		case "function_call":
			flushText()
			hasToolUse = true
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmpty(strOf(item["call_id"]), strOf(item["id"])),
				"name":  strOf(item["name"]),
				"input": parseToolArgs(strOf(item["arguments"])),
			})
		case "reasoning":
			flushText()
			text, sig := responsesReasoningText(item)
			blk := map[string]any{"type": "thinking", "thinking": text}
			if sig != "" {
				blk["signature"] = sig
			}
			blocks = append(blocks, blk)
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
	out["stop_reason"] = responsesStatusToAnthropicStop(strOf(src["status"]), hasToolUse)
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
func responsesUsageToAnthropic(u any) map[string]any {
	um := asMap(u)
	if um == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	return map[string]any{
		"input_tokens":  intOf(um["input_tokens"]),
		"output_tokens": intOf(um["output_tokens"]),
	}
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
	var contentText strings.Builder
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
							contentText.WriteString(strOf(pm["text"]))
						}
					}
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   firstNonEmpty(strOf(item["call_id"]), strOf(item["id"])),
				"type": "function",
				"function": map[string]any{
					"name":      strOf(item["name"]),
					"arguments": strOf(item["arguments"]),
				},
			})
		case "reasoning":
			text, _ := responsesReasoningText(item)
			reasoning.WriteString(text)
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
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	finish := responsesStatusToOpenAIFinish(strOf(src["status"]), len(toolCalls) > 0)
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
	if um != nil {
		in = intOf(um["input_tokens"])
		out = intOf(um["output_tokens"])
	}
	return map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
}

// --- response: anthropic → responses ---

func convertAnthropicResponseToResponses(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse anthropic response: %w", err)
	}
	var output []map[string]any
	var textParts []string
	flushText := func() {
		if len(textParts) > 0 {
			output = append(output, map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": strings.Join(textParts, "")}},
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
				textParts = append(textParts, strOf(b["text"]))
			case "tool_use":
				flushText()
				args, _ := sonic.MarshalString(b["input"])
				item := map[string]any{
					"type": "function_call", "status": "completed",
					"id": strOf(b["id"]), "call_id": strOf(b["id"]),
					"name": strOf(b["name"]), "arguments": args,
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
	out := map[string]any{
		"id":     strOf(src["id"]),
		"object": "response",
		"status": anthropicStopToResponsesStatus(strOf(src["stop_reason"])),
		"output": output,
	}
	if m, ok := src["model"]; ok {
		out["model"] = m
	}
	out["usage"] = anthropicUsageToResponses(src["usage"])
	return sonic.Marshal(out)
}

// --- response: openai-chat → responses ---

func convertOpenAIResponseToResponses(body []byte) ([]byte, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	var output []map[string]any
	finish := ""
	var textParts []string
	flushText := func() {
		if len(textParts) > 0 {
			output = append(output, map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": strings.Join(textParts, "")}},
			})
			textParts = nil
		}
	}
	if choices, ok := src["choices"].([]any); ok && len(choices) > 0 {
		if ch := asMap(choices[0]); ch != nil {
			finish = strOf(ch["finish_reason"])
			if msg := asMap(ch["message"]); msg != nil {
				if c, ok := msg["content"].(string); ok && c != "" {
					textParts = append(textParts, c)
				}
				if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
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
						output = append(output, map[string]any{
							"type": "function_call", "status": "completed",
							"id":      strOf(tcm["id"]),
							"call_id": strOf(tcm["id"]),
							"name":    strOf(fnMap(fn, "name")),
							"arguments": strOf(fnMap(fn, "arguments")),
						})
					}
				}
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
	out := map[string]any{
		"id":     strOf(src["id"]),
		"object": "response",
		"status": openAIFinishToResponsesStatus(finish),
		"output": output,
	}
	if m, ok := src["model"]; ok {
		out["model"] = m
	}
	out["usage"] = openAIUsageToResponses(src["usage"])
	return sonic.Marshal(out)
}

func anthropicUsageToResponses(u any) map[string]any {
	um := asMap(u)
	in, out := 0, 0
	if um != nil {
		in = intOf(um["input_tokens"])
		out = intOf(um["output_tokens"])
	}
	return map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out}
}

func openAIUsageToResponses(u any) map[string]any {
	um := asMap(u)
	in, out := 0, 0
	if um != nil {
		in = intOf(um["prompt_tokens"])
		out = intOf(um["completion_tokens"])
	}
	return map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out}
}

