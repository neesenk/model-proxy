package main

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strings"

	sonic "github.com/bytedance/sonic"
)

type wireSSEEvent struct {
	event string
	data  string
}

func requestWantsStream(body []byte) bool {
	var root map[string]any
	return sonic.Unmarshal(body, &root) == nil && root["stream"] == true
}

func parseWireSSE(raw []byte) ([]wireSSEEvent, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	var out []wireSSEEvent
	event := ""
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			event = ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			out = append(out, wireSSEEvent{event: event, data: strings.TrimSpace(strings.TrimPrefix(line, "data:"))})
			event = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func aggregateSSEToResponse(raw []byte, proto string) ([]byte, error) {
	events, err := parseWireSSE(raw)
	if err != nil {
		return nil, err
	}
	switch proto {
	case "responses":
		return aggregateResponsesSSE(events)
	case "openai":
		return aggregateChatSSE(events)
	case "anthropic":
		return aggregateAnthropicSSE(events)
	default:
		return nil, fmt.Errorf("unsupported client protocol %q", proto)
	}
}

func aggregateResponsesSSE(events []wireSSEEvent) ([]byte, error) {
	var response map[string]any
	var items []any
	var text strings.Builder
	terminal := false
	for _, event := range events {
		if event.data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if sonic.UnmarshalString(event.data, &payload) != nil {
			continue
		}
		typ := firstNonEmpty(strOpt(payload["type"]), event.event)
		switch typ {
		case "response.created":
			if response == nil {
				response = asMap(payload["response"])
			}
		case "response.output_item.done":
			if item := asMap(payload["item"]); item != nil {
				items = append(items, item)
			}
		case "response.output_text.delta":
			text.WriteString(strOpt(payload["delta"]))
		case "response.completed", "response.incomplete":
			response = asMap(payload["response"])
			terminal = true
		case "response.failed", "response.cancelled", "error":
			return nil, fmt.Errorf("responses stream terminated with %s", typ)
		}
	}
	if !terminal || response == nil {
		return nil, fmt.Errorf("responses stream ended without a terminal response")
	}
	if len(items) == 0 {
		if existing, ok := response["output"].([]any); ok {
			items = existing
		}
	}
	if len(items) == 0 && text.Len() > 0 {
		items = []any{map[string]any{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": text.String()}},
		}}
	}
	response["object"] = "response"
	response["output"] = items
	return sonic.Marshal(response)
}

func aggregateChatSSE(events []wireSSEEvent) ([]byte, error) {
	out := map[string]any{"object": "chat.completion"}
	message := map[string]any{"role": "assistant"}
	var content, reasoning strings.Builder
	tools := map[int]map[string]any{}
	finish := any(nil)
	for _, event := range events {
		if event.data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if sonic.UnmarshalString(event.data, &payload) != nil {
			continue
		}
		if asMap(payload["error"]) != nil {
			return nil, fmt.Errorf("chat stream terminated with an error")
		}
		copyOpt(out, payload, "id", "model", "created", "system_fingerprint", "usage")
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := asMap(choices[0])
		if choice["finish_reason"] != nil {
			finish = choice["finish_reason"]
		}
		delta := asMap(choice["delta"])
		content.WriteString(strOpt(delta["content"]))
		reasoning.WriteString(strOpt(delta["reasoning_content"]))
		for _, raw := range anySlice(delta["tool_calls"]) {
			tc := asMap(raw)
			index := intOf(tc["index"])
			current := tools[index]
			if current == nil {
				current = map[string]any{"type": "function", "function": map[string]any{}}
				tools[index] = current
			}
			if id := strOpt(tc["id"]); id != "" {
				current["id"] = id
			}
			fn, currentFn := asMap(tc["function"]), asMap(current["function"])
			if name := strOpt(fn["name"]); name != "" {
				currentFn["name"] = name
			}
			currentFn["arguments"] = strOpt(currentFn["arguments"]) + strOpt(fn["arguments"])
		}
	}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else {
		message["content"] = nil
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(tools) > 0 {
		indexes := make([]int, 0, len(tools))
		for index := range tools {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		calls := make([]map[string]any, 0, len(indexes))
		for _, index := range indexes {
			calls = append(calls, tools[index])
		}
		message["tool_calls"] = calls
	}
	if finish == nil {
		return nil, fmt.Errorf("chat stream ended without finish_reason")
	}
	out["choices"] = []map[string]any{{"index": 0, "message": message, "finish_reason": finish}}
	return sonic.Marshal(out)
}

func aggregateAnthropicSSE(events []wireSSEEvent) ([]byte, error) {
	var message map[string]any
	blocks := map[int]map[string]any{}
	partialJSON := map[int]string{}
	terminal := false
	for _, event := range events {
		var payload map[string]any
		if sonic.UnmarshalString(event.data, &payload) != nil {
			continue
		}
		typ := firstNonEmpty(strOpt(payload["type"]), event.event)
		switch typ {
		case "message_start":
			message = asMap(payload["message"])
		case "content_block_start":
			blocks[intOf(payload["index"])] = asMap(payload["content_block"])
		case "content_block_delta":
			index := intOf(payload["index"])
			block := blocks[index]
			if block == nil {
				block = map[string]any{}
				blocks[index] = block
			}
			delta := asMap(payload["delta"])
			switch strOpt(delta["type"]) {
			case "text_delta":
				block["type"] = "text"
				block["text"] = strOpt(block["text"]) + strOpt(delta["text"])
			case "thinking_delta":
				block["type"] = "thinking"
				block["thinking"] = strOpt(block["thinking"]) + strOpt(delta["thinking"])
			case "signature_delta":
				block["signature"] = strOpt(block["signature"]) + strOpt(delta["signature"])
			case "input_json_delta":
				partialJSON[index] += strOpt(delta["partial_json"])
			}
		case "content_block_stop":
			index := intOf(payload["index"])
			if raw := partialJSON[index]; raw != "" {
				blocks[index]["input"] = parseToolArgs(raw)
			}
		case "message_delta":
			if message == nil {
				message = map[string]any{}
			}
			for key, value := range asMap(payload["delta"]) {
				message[key] = value
			}
			if usage := asMap(payload["usage"]); usage != nil {
				base := asMap(message["usage"])
				if base == nil {
					base = map[string]any{}
				}
				for key, value := range usage {
					base[key] = value
				}
				message["usage"] = base
			}
		case "message_stop":
			terminal = true
		case "error":
			return nil, fmt.Errorf("anthropic stream terminated with an error")
		}
	}
	if !terminal || message == nil {
		return nil, fmt.Errorf("anthropic stream ended without message_stop")
	}
	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	content := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		content = append(content, blocks[index])
	}
	message["type"] = "message"
	message["role"] = "assistant"
	message["content"] = content
	return sonic.Marshal(message)
}

func responseToSSE(body []byte, proto string) ([]byte, error) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	switch proto {
	case "responses":
		return responsesJSONToSSE(root)
	case "openai":
		return chatJSONToSSE(root)
	case "anthropic":
		return anthropicJSONToSSE(root)
	default:
		return nil, fmt.Errorf("unsupported client protocol %q", proto)
	}
}

func emitWireSSE(out *bytes.Buffer, event string, payload any) {
	data, _ := sonic.Marshal(payload)
	if event != "" {
		fmt.Fprintf(out, "event: %s\n", event)
	}
	fmt.Fprintf(out, "data: %s\n\n", data)
}

func responsesJSONToSSE(root map[string]any) ([]byte, error) {
	var out bytes.Buffer
	created := cloneMap(root)
	created["status"] = "in_progress"
	delete(created, "output")
	emitWireSSE(&out, "response.created", map[string]any{"type": "response.created", "response": created})
	for index, raw := range anySlice(root["output"]) {
		item := asMap(raw)
		added := cloneMap(item)
		added["status"] = "in_progress"
		switch strOpt(item["type"]) {
		case "message":
			added["content"] = []any{}
		case "function_call":
			added["arguments"] = ""
		case "reasoning":
			added["summary"] = []any{}
		}
		emitWireSSE(&out, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": index, "item": added,
		})
		switch strOpt(item["type"]) {
		case "message":
			for contentIndex, rawPart := range anySlice(item["content"]) {
				part := asMap(rawPart)
				if strOpt(part["type"]) != "output_text" {
					continue
				}
				emitWireSSE(&out, "response.content_part.added", map[string]any{
					"type": "response.content_part.added", "output_index": index, "content_index": contentIndex,
					"part": map[string]any{"type": "output_text", "text": ""},
				})
				emitWireSSE(&out, "response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "output_index": index, "content_index": contentIndex,
					"delta": strOpt(part["text"]),
				})
				emitWireSSE(&out, "response.output_text.done", map[string]any{
					"type": "response.output_text.done", "output_index": index, "content_index": contentIndex,
					"text": strOpt(part["text"]),
				})
				emitWireSSE(&out, "response.content_part.done", map[string]any{
					"type": "response.content_part.done", "output_index": index, "content_index": contentIndex, "part": part,
				})
			}
		case "function_call":
			args := firstNonEmpty(strOpt(item["arguments"]), "{}")
			emitWireSSE(&out, "response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "output_index": index,
				"item_id": item["id"], "delta": args,
			})
			emitWireSSE(&out, "response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "output_index": index,
				"item_id": item["id"], "arguments": args,
			})
		case "reasoning":
			for summaryIndex, rawSummary := range anySlice(item["summary"]) {
				summary := asMap(rawSummary)
				text := strOpt(summary["text"])
				emitWireSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
					"type": "response.reasoning_summary_text.delta", "output_index": index,
					"summary_index": summaryIndex, "delta": text,
				})
				emitWireSSE(&out, "response.reasoning_summary_text.done", map[string]any{
					"type": "response.reasoning_summary_text.done", "output_index": index,
					"summary_index": summaryIndex, "text": text,
				})
			}
		}
		emitWireSSE(&out, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": index, "item": item,
		})
	}
	status := strOpt(root["status"])
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	} else if status == "failed" || status == "cancelled" {
		return nil, fmt.Errorf("cannot synthesize success SSE from response status %s", status)
	}
	emitWireSSE(&out, event, map[string]any{"type": event, "response": root})
	return out.Bytes(), nil
}

func chatJSONToSSE(root map[string]any) ([]byte, error) {
	choices := anySlice(root["choices"])
	if len(choices) == 0 {
		return nil, fmt.Errorf("chat response has no choices")
	}
	choice := asMap(choices[0])
	message := asMap(choice["message"])
	base := map[string]any{"id": root["id"], "object": "chat.completion.chunk", "model": root["model"]}
	delta := cloneMap(message)
	delete(delta, "role")
	emitWireSSEBuffer := func(payload map[string]any) []byte {
		data, _ := sonic.Marshal(payload)
		return append([]byte("data: "), append(data, []byte("\n\n")...)...)
	}
	var out bytes.Buffer
	first := cloneMap(base)
	first["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}
	out.Write(emitWireSSEBuffer(first))
	if len(delta) > 0 {
		chunk := cloneMap(base)
		chunk["choices"] = []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}}
		out.Write(emitWireSSEBuffer(chunk))
	}
	last := cloneMap(base)
	last["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": choice["finish_reason"]}}
	if root["usage"] != nil {
		last["usage"] = root["usage"]
	}
	out.Write(emitWireSSEBuffer(last))
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes(), nil
}

func anthropicJSONToSSE(root map[string]any) ([]byte, error) {
	var out bytes.Buffer
	start := cloneMap(root)
	start["content"] = []any{}
	start["stop_reason"] = nil
	emitWireSSE(&out, "message_start", map[string]any{"type": "message_start", "message": start})
	for index, raw := range anySlice(root["content"]) {
		block := asMap(raw)
		startBlock := cloneMap(block)
		var delta map[string]any
		switch strOpt(block["type"]) {
		case "text":
			startBlock["text"] = ""
			delta = map[string]any{"type": "text_delta", "text": strOpt(block["text"])}
		case "thinking":
			startBlock["thinking"] = ""
			delete(startBlock, "signature")
			delta = map[string]any{"type": "thinking_delta", "thinking": strOpt(block["thinking"])}
		case "tool_use":
			startBlock["input"] = map[string]any{}
			encoded, _ := sonic.Marshal(block["input"])
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}
		}
		emitWireSSE(&out, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index, "content_block": startBlock,
		})
		if delta != nil {
			emitWireSSE(&out, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index, "delta": delta,
			})
		}
		if signature := strOpt(block["signature"]); signature != "" {
			emitWireSSE(&out, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "signature_delta", "signature": signature},
			})
		}
		emitWireSSE(&out, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	}
	emitWireSSE(&out, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": root["stop_reason"], "stop_sequence": root["stop_sequence"]},
		"usage": root["usage"],
	})
	emitWireSSE(&out, "message_stop", map[string]any{"type": "message_stop"})
	return out.Bytes(), nil
}

func cloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}
