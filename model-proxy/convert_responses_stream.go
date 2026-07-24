// convert_responses_stream.go — streaming SSE transformers for the Responses
// protocol, modeled on convert.go's openaiSSEToAnthropicSSE / anthropicSSEToOpenAISSE
// (io.Reader wrappers; bufio.Scanner over the upstream body; emit converted
// events into an out buffer drained by Read).
//
// Responses SSE carries BOTH an `event:` line and a `data:` line per event
// (response.created, response.output_item.added, response.output_text.delta,
// response.function_call_arguments.delta, response.reasoning_summary_text.delta,
// response.output_item.done, response.completed, …), unlike OpenAI chat chunks
// which are `data:`-only. Each transformer tracks the pending event type from
// the `event:` line and dispatches on the `data:` payload.
package main

import (
	"bufio"
	"io"
	"strings"

	sonic "github.com/bytedance/sonic"
)

// sseEmit appends one SSE-formatted event (event: T\ndata: J\n\n) to dst.
func sseEmit(dst *[]byte, event string, payload map[string]any) {
	b, _ := sonic.Marshal(payload)
	*dst = append(*dst, []byte("event: "+event+"\n")...)
	*dst = append(*dst, []byte("data: ")...)
	*dst = append(*dst, b...)
	*dst = append(*dst, []byte("\n\n")...)
}

// sseEmitData appends a data-only SSE line (data: J\n\n), for OpenAI-style
// streams that omit the `event:` line.
func sseEmitData(dst *[]byte, payload map[string]any) {
	b, _ := sonic.Marshal(payload)
	*dst = append(*dst, []byte("data: ")...)
	*dst = append(*dst, b...)
	*dst = append(*dst, []byte("\n\n")...)
}

// sseDone emits the terminal data: [DONE] marker (OpenAI convention).
func sseDone(dst *[]byte) { *dst = append(*dst, []byte("data: [DONE]\n\n")...) }

// ===========================================================================
// responses → anthropic (codex/Responses backend → Claude Code client)
// ===========================================================================

// rsBlock tracks one Responses output item's anthropic-side block state.
type rsBlock struct {
	kind    string // "text" | "tool_use" | "thinking"
	idx     int    // anthropic content_block index
	opened  bool   // content_block_start emitted
	itemID  string // responses item id (for completeness)
}

type responsesSSEToAnthropicSSE struct {
	sc          *bufio.Scanner
	out         []byte
	model, id   string
	started     bool
	done        bool
	errored     bool
	nextIdx     int
	blocks      map[int]*rsBlock // responses output_index → block
	curTextOut  int              // output_index of the open text block (-1 none)
	curThinkOut int              // output_index of the open thinking block (-1 none)
	inTok       int
	outTok      int
	stopRsn     string
	hasToolUse  bool
}

func newResponsesToAnthropicSSE(r io.Reader, model string) *responsesSSEToAnthropicSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &responsesSSEToAnthropicSSE{
		sc: sc, model: model, id: "msg_conv",
		blocks: map[int]*rsBlock{}, curTextOut: -1, curThinkOut: -1,
	}
}

func (t *responsesSSEToAnthropicSSE) ensureStart() {
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

func (t *responsesSSEToAnthropicSSE) emit(event string, payload map[string]any) {
	sseEmit(&t.out, event, payload)
}

func (t *responsesSSEToAnthropicSSE) closeBlock(outIdx int) {
	if b, ok := t.blocks[outIdx]; ok && b.opened {
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.idx})
		b.opened = false
	}
}

func (t *responsesSSEToAnthropicSSE) Read(p []byte) (int, error) {
	pendingEvent := ""
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		if !t.sc.Scan() {
			if err := t.sc.Err(); err != nil {
				convertWarn("responses SSE scanner error: " + err.Error())
			}
			t.finish()
			continue
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" {
			pendingEvent = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			t.finish()
			continue
		}
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) != nil {
			continue
		}
		t.handle(pendingEvent, data)
		pendingEvent = ""
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *responsesSSEToAnthropicSSE) handle(event string, data map[string]any) {
	switch event {
	case "response.created", "response.in_progress":
		t.ensureStart()
		if resp := asMap(data["response"]); resp != nil {
			if id := strOf(resp["id"]); id != "" {
				t.id = id
			}
			if m := strOf(resp["model"]); m != "" {
				t.model = m
			}
		}
	case "response.output_item.added":
		item := asMap(data["item"])
		if item == nil {
			return
		}
		outIdx := intOf(data["output_index"])
		kind := ""
		switch item["type"] {
		case "message":
			kind = "text"
		case "function_call":
			kind = "tool_use"
		case "reasoning":
			kind = "thinking"
		default:
			return
		}
		t.ensureStart()
		b := &rsBlock{kind: kind, idx: t.nextIdx, itemID: strOf(item["id"])}
		t.nextIdx++
		t.blocks[outIdx] = b
		if kind == "tool_use" {
			t.hasToolUse = true
			b.opened = true
			t.emit("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": b.idx,
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   firstNonEmpty(strOf(item["call_id"]), strOf(item["id"])),
					"name": strOf(item["name"]),
					"input": map[string]any{},
				},
			})
		}
	case "response.output_text.delta":
		t.ensureStart()
		outIdx := intOf(data["output_index"])
		b := t.blocks[outIdx]
		if b == nil {
			b = &rsBlock{kind: "text", idx: t.nextIdx}
			t.nextIdx++
			t.blocks[outIdx] = b
		}
		if !b.opened {
			b.opened = true
			t.emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         b.idx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		t.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": b.idx,
			"delta": map[string]any{"type": "text_delta", "text": strOf(data["delta"])},
		})
	case "response.function_call_arguments.delta":
		outIdx := intOf(data["output_index"])
		if b := t.blocks[outIdx]; b != nil && b.opened {
			t.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": b.idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": strOf(data["delta"])},
			})
		}
	case "response.reasoning_summary_text.delta":
		t.ensureStart()
		outIdx := intOf(data["output_index"])
		b := t.blocks[outIdx]
		if b == nil {
			b = &rsBlock{kind: "thinking", idx: t.nextIdx}
			t.nextIdx++
			t.blocks[outIdx] = b
		}
		if !b.opened {
			b.opened = true
			t.emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         b.idx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			})
		}
		t.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": b.idx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": strOf(data["delta"])},
		})
	case "response.output_item.done":
		t.closeBlock(intOf(data["output_index"]))
	case "response.completed":
		if resp := asMap(data["response"]); resp != nil {
			if u := asMap(resp["usage"]); u != nil {
				t.inTok = intOf(u["input_tokens"])
				t.outTok = intOf(u["output_tokens"])
			}
			if id := strOf(resp["id"]); id != "" {
				t.id = id
			}
		}
		t.finish()
	case "response.incomplete":
		t.stopRsn = "max_tokens"
		t.finish()
	case "response.failed", "error":
		t.ensureStart()
		emsg, etype := "", "api_error"
		if e := asMap(data["error"]); e != nil {
			emsg = strOf(e["message"])
			etype = firstNonEmpty(strOf(e["type"]), "api_error")
		}
		t.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": etype, "message": emsg}})
		t.errored = true
		t.done = true
	}
}

func (t *responsesSSEToAnthropicSSE) finish() {
	if t.done {
		return
	}
	t.done = true
	t.ensureStart()
	for outIdx := range t.blocks {
		t.closeBlock(outIdx)
	}
	if t.stopRsn == "" {
		if t.hasToolUse {
			t.stopRsn = "tool_use"
		} else {
			t.stopRsn = "end_turn"
		}
	}
	if !t.errored {
		t.emit("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": t.stopRsn},
			"usage": map[string]any{"input_tokens": t.inTok, "output_tokens": t.outTok},
		})
		t.emit("message_stop", map[string]any{"type": "message_stop"})
	}
}

// ===========================================================================
// responses → openai-chat (codex/Responses backend → chat client)
// ===========================================================================

type responsesSSEToOpenAISSE struct {
	sc         *bufio.Scanner
	out        []byte
	model, id  string
	started    bool
	done       bool
	errored    bool
	toolIdx    map[int]int // responses output_index → chat tool_calls index
	inTok      int
	outTok     int
	finishReason string
	hasToolUse bool
}

func newResponsesToOpenAISSE(r io.Reader, model string) *responsesSSEToOpenAISSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &responsesSSEToOpenAISSE{sc: sc, model: model, id: "chatcmpl-conv", toolIdx: map[int]int{}}
}

func (t *responsesSSEToOpenAISSE) ensureStart() {
	if t.started {
		return
	}
	t.started = true
	sseEmitData(&t.out, map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	})
}

func (t *responsesSSEToOpenAISSE) toolIndex(outIdx int) int {
	if i, ok := t.toolIdx[outIdx]; ok {
		return i
	}
	i := len(t.toolIdx)
	t.toolIdx[outIdx] = i
	return i
}

func (t *responsesSSEToOpenAISSE) Read(p []byte) (int, error) {
	pendingEvent := ""
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		if !t.sc.Scan() {
			if err := t.sc.Err(); err != nil {
				convertWarn("responses SSE scanner error: " + err.Error())
			}
			t.finish()
			continue
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" {
			pendingEvent = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			t.finish()
			continue
		}
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) != nil {
			continue
		}
		t.handle(pendingEvent, data)
		pendingEvent = ""
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *responsesSSEToOpenAISSE) handle(event string, data map[string]any) {
	switch event {
	case "response.created", "response.in_progress":
		if resp := asMap(data["response"]); resp != nil {
			if id := strOf(resp["id"]); id != "" {
				t.id = id
			}
			if m := strOf(resp["model"]); m != "" {
				t.model = m
			}
		}
		t.ensureStart()
	case "response.output_text.delta":
		t.ensureStart()
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": strOf(data["delta"])}, "finish_reason": nil}},
		})
	case "response.output_item.added":
		item := asMap(data["item"])
		if item == nil || item["type"] != "function_call" {
			return
		}
		t.ensureStart()
		t.hasToolUse = true
		outIdx := intOf(data["output_index"])
		idx := t.toolIndex(outIdx)
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"tool_calls": []map[string]any{{
					"index": idx, "id": firstNonEmpty(strOf(item["call_id"]), strOf(item["id"])),
					"type": "function", "function": map[string]any{"name": strOf(item["name"]), "arguments": ""},
				}},
			}, "finish_reason": nil}},
		})
	case "response.function_call_arguments.delta":
		outIdx := intOf(data["output_index"])
		idx, ok := t.toolIdx[outIdx]
		if !ok {
			return
		}
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"tool_calls": []map[string]any{{"index": idx, "function": map[string]any{"arguments": strOf(data["delta"])}}},
			}, "finish_reason": nil}},
		})
	case "response.reasoning_summary_text.delta":
		t.ensureStart()
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"reasoning_content": strOf(data["delta"])}, "finish_reason": nil}},
		})
	case "response.completed":
		if resp := asMap(data["response"]); resp != nil {
			if u := asMap(resp["usage"]); u != nil {
				t.inTok = intOf(u["input_tokens"])
				t.outTok = intOf(u["output_tokens"])
			}
		}
		t.finish()
	case "response.incomplete":
		t.finishReason = "length"
		t.finish()
	case "response.failed", "error":
		emsg, etype := "", "api_error"
		if e := asMap(data["error"]); e != nil {
			emsg = strOf(e["message"])
			etype = firstNonEmpty(strOf(e["type"]), "api_error")
		}
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"error": map[string]any{"message": emsg, "type": etype},
		})
		t.errored = true
		t.done = true
	}
}

// finish finalizes the chat chunk stream (called on EOF, completed, incomplete).
func (t *responsesSSEToOpenAISSE) finish() {
	if t.done {
		return
	}
	t.done = true
	t.emitFinish()
}

func (t *responsesSSEToOpenAISSE) emitFinish() {
	fr := t.finishReason
	if fr == "" {
		if t.hasToolUse {
			fr = "tool_calls"
		} else {
			fr = "stop"
		}
	}
	if !t.errored {
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": fr}},
			"usage": map[string]any{"prompt_tokens": t.inTok, "completion_tokens": t.outTok, "total_tokens": t.inTok + t.outTok},
		})
		sseDone(&t.out)
	}
}

// ===========================================================================
// anthropic → responses (Claude Code client → Responses backend is the forward
// direction; this transformer serves a Responses-speaking client, e.g. codex
// CLI, reaching an anthropic backend) — synthesizes response.* events.
// ===========================================================================

// rsRevBlock tracks one anthropic content block → a Responses output item.
type rsRevBlock struct {
	outIdx    int
	kind      string // "text" | "thinking" | "tool_use"
	itemID    string
	opened    bool
	acc       string // accumulated text/args (for the *.done text field)
	signature string
}

type anthropicSSEToResponsesSSE struct {
	sc        *bufio.Scanner
	out       []byte
	model, id string
	started   bool
	done      bool
	nextOutIdx int
	blocks    map[int]*rsRevBlock
	inTok     int
	outTok    int
	stopStatus string
}

func newAnthropicToResponsesSSE(r io.Reader, model string) *anthropicSSEToResponsesSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &anthropicSSEToResponsesSSE{sc: sc, model: model, id: "resp_conv", blocks: map[int]*rsRevBlock{}}
}

func (t *anthropicSSEToResponsesSSE) emit(event string, payload map[string]any) {
	sseEmit(&t.out, event, payload)
}

func (t *anthropicSSEToResponsesSSE) ensureCreated() {
	if t.started {
		return
	}
	t.started = true
	t.emit("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": t.id, "object": "response", "status": "in_progress",
			"model": t.model, "output": []any{},
		},
	})
}

func (t *anthropicSSEToResponsesSSE) Read(p []byte) (int, error) {
	pendingEvent := ""
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		if !t.sc.Scan() {
			if err := t.sc.Err(); err != nil {
				convertWarn("anthropic SSE scanner error: " + err.Error())
			}
			t.finish()
			continue
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" {
			pendingEvent = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			t.finish()
			continue
		}
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) != nil {
			continue
		}
		t.handle(pendingEvent, data)
		pendingEvent = ""
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *anthropicSSEToResponsesSSE) handle(event string, data map[string]any) {
	switch event {
	case "message_start":
		t.ensureCreated()
		if msg := asMap(data["message"]); msg != nil {
			if id := strOf(msg["id"]); id != "" {
				t.id = id
			}
			if m := strOf(msg["model"]); m != "" {
				t.model = m
			}
		}
	case "content_block_start":
		idx := intOf(data["index"])
		cb := asMap(data["content_block"])
		kind := ""
		if cb != nil {
			kind = strOf(cb["type"])
		}
		outIdx := t.nextOutIdx
		t.nextOutIdx++
		b := &rsRevBlock{outIdx: outIdx, kind: kind, itemID: fmtItemID(kind, outIdx)}
		if cb != nil && kind == "tool_use" {
			b.opened = true
			t.ensureCreated()
			t.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": outIdx,
				"item": map[string]any{
					"type": "function_call", "id": b.itemID, "status": "in_progress",
					"call_id": firstNonEmpty(strOf(cb["id"]), b.itemID),
					"name":    strOf(cb["name"]),
					"arguments": "",
				},
			})
		}
		t.blocks[idx] = b
	case "content_block_delta":
		idx := intOf(data["index"])
		b := t.blocks[idx]
		if b == nil {
			return
		}
		delta := asMap(data["delta"])
		if delta == nil {
			return
		}
		switch strOf(delta["type"]) {
		case "text_delta":
			t.ensureCreated()
			if !b.opened {
				b.opened = true
				t.emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": b.outIdx,
					"item": map[string]any{"type": "message", "id": b.itemID, "status": "in_progress", "role": "assistant", "content": []any{}},
				})
				t.emit("response.content_part.added", map[string]any{
					"type": "response.content_part.added", "output_index": b.outIdx, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": ""},
				})
			}
			d := strOf(delta["text"])
			b.acc += d
			t.emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "output_index": b.outIdx, "content_index": 0, "delta": d,
			})
		case "thinking_delta":
			t.ensureCreated()
			if !b.opened {
				b.opened = true
				t.emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": b.outIdx,
					"item": map[string]any{"type": "reasoning", "id": b.itemID, "status": "in_progress", "summary": []any{}},
				})
				t.emit("response.reasoning_summary_part.added", map[string]any{
					"type": "response.reasoning_summary_part.added", "output_index": b.outIdx, "summary_index": 0,
					"item": map[string]any{"type": "reasoning", "id": b.itemID, "summary": []any{}},
				})
			}
			d := strOf(delta["thinking"])
			b.acc += d
			t.emit("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "output_index": b.outIdx, "summary_index": 0, "delta": d,
			})
		case "signature_delta":
			b.signature += strOf(delta["signature"])
		case "input_json_delta":
			d := strOf(delta["partial_json"])
			b.acc += d
			t.emit("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "output_index": b.outIdx, "item_id": b.itemID, "delta": d,
			})
		}
	case "content_block_stop":
		idx := intOf(data["index"])
		b := t.blocks[idx]
		if b == nil {
			return
		}
		switch b.kind {
		case "text":
			t.emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": b.outIdx, "content_index": 0, "text": b.acc})
			t.emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "output_index": b.outIdx, "content_index": 0, "part": map[string]any{"type": "output_text", "text": b.acc}})
		case "thinking":
			item := map[string]any{"type": "reasoning", "id": b.itemID, "status": "completed", "summary": []map[string]any{{"type": "summary_text", "text": b.acc}}}
			if b.signature != "" {
				item["encrypted_content"] = b.signature
			}
			t.emit("response.reasoning_summary_part.done", map[string]any{"type": "response.reasoning_summary_part.done", "output_index": b.outIdx, "summary_index": 0, "item": item})
		case "tool_use":
			t.emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": b.outIdx, "item_id": b.itemID, "arguments": b.acc})
		}
		t.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": b.outIdx,
			"item": map[string]any{"type": b.kind, "id": b.itemID, "status": "completed"},
		})
	case "message_delta":
		if d := asMap(data["delta"]); d != nil {
			if sr := strOf(d["stop_reason"]); sr != "" {
				t.stopStatus = anthropicStopToResponsesStatus(sr)
			}
		}
		if u := asMap(data["usage"]); u != nil {
			t.inTok = intOf(u["input_tokens"])
			t.outTok = intOf(u["output_tokens"])
		}
	case "message_stop":
		t.finish()
	case "error":
		emsg, etype := "", "api_error"
		if e := asMap(data["error"]); e != nil {
			emsg = strOf(e["message"])
			etype = firstNonEmpty(strOf(e["type"]), "api_error")
		}
		t.emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{"id": t.id, "status": "failed", "error": map[string]any{"code": etype, "message": emsg}},
		})
		t.done = true
	}
}

func (t *anthropicSSEToResponsesSSE) finish() {
	if t.done {
		return
	}
	t.done = true
	t.ensureCreated()
	status := t.stopStatus
	if status == "" {
		status = "completed"
	}
	t.emit("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": t.id, "object": "response", "status": status, "model": t.model, "output": []any{},
			"usage": map[string]any{"input_tokens": t.inTok, "output_tokens": t.outTok, "total_tokens": t.inTok + t.outTok},
		},
	})
}

// fmtItemID synthesizes a Responses item id for a reverse-synthesized stream.
func fmtItemID(kind string, outIdx int) string {
	switch kind {
	case "tool_use":
		return "fc_item_" + itoa(outIdx)
	case "thinking":
		return "rs_item_" + itoa(outIdx)
	default:
		return "msg_item_" + itoa(outIdx)
	}
}

// itoa is a strconv-free int→string (avoids an extra import here).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// ===========================================================================
// openai-chat → responses (chat client reaching a Responses backend is forward;
// this serves a Responses-speaking client reaching a chat backend).
// ===========================================================================

type openaiSSEToResponsesSSE struct {
	sc         *bufio.Scanner
	out        []byte
	model, id  string
	started    bool
	done       bool
	nextOutIdx int
	textOut    int  // output_index of the open message/text item (-1 none)
	textOpened bool
	toolOut    map[int]int // chat tool_calls index → responses output_index
	toolIDs    map[int]string
	inTok      int
	outTok     int
	finishStat string
	hasToolUse bool
}

func newOpenAIToResponsesSSE(r io.Reader, model string) *openaiSSEToResponsesSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &openaiSSEToResponsesSSE{sc: sc, model: model, id: "resp_conv", textOut: -1, toolOut: map[int]int{}, toolIDs: map[int]string{}}
}

func (t *openaiSSEToResponsesSSE) emit(event string, payload map[string]any) {
	sseEmit(&t.out, event, payload)
}

func (t *openaiSSEToResponsesSSE) ensureCreated() {
	if t.started {
		return
	}
	t.started = true
	t.emit("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{"id": t.id, "object": "response", "status": "in_progress", "model": t.model, "output": []any{}},
	})
}

func (t *openaiSSEToResponsesSSE) openText() {
	if t.textOpened {
		return
	}
	t.textOpened = true
	t.textOut = t.nextOutIdx
	t.nextOutIdx++
	id := "msg_item_" + itoa(t.textOut)
	t.ensureCreated()
	t.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": t.textOut,
		"item": map[string]any{"type": "message", "id": id, "status": "in_progress", "role": "assistant", "content": []any{}},
	})
	t.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "output_index": t.textOut, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": ""},
	})
}

func (t *openaiSSEToResponsesSSE) Read(p []byte) (int, error) {
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		if !t.sc.Scan() {
			if err := t.sc.Err(); err != nil {
				convertWarn("openai SSE scanner error: " + err.Error())
			}
			t.finish()
			continue
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			t.finish()
			continue
		}
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) != nil {
			continue
		}
		t.handle(data)
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *openaiSSEToResponsesSSE) handle(data map[string]any) {
	if id := strOf(data["id"]); id != "" {
		t.id = id
	}
	if m := strOf(data["model"]); m != "" {
		t.model = m
	}
	if u := asMap(data["usage"]); u != nil {
		t.inTok = intOf(u["prompt_tokens"])
		t.outTok = intOf(u["completion_tokens"])
	}
	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	ch := asMap(choices[0])
	if ch == nil {
		return
	}
	delta := asMap(ch["delta"])
	if fr := strOf(ch["finish_reason"]); fr != "" {
		t.finishStat = openAIFinishToResponsesStatus(fr)
	}
	if delta == nil {
		return
	}
	if c, ok := delta["content"].(string); ok && c != "" {
		t.ensureCreated()
		t.openText()
		t.emit("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "output_index": t.textOut, "content_index": 0, "delta": c,
		})
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		t.ensureCreated()
		// best-effort: stream reasoning as a reasoning-summary delta on its own item
		outIdx := t.nextOutIdx
		id := "rs_item_" + itoa(outIdx)
		t.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": outIdx,
			"item": map[string]any{"type": "reasoning", "id": id, "status": "in_progress", "summary": []any{}},
		})
		t.nextOutIdx++
		t.emit("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "output_index": outIdx, "summary_index": 0, "delta": rc,
		})
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm := asMap(tc)
			if tcm == nil {
				continue
			}
			idx := intOf(tcm["index"])
			fn := asMap(tcm["function"])
			if _, exists := t.toolOut[idx]; !exists {
				t.hasToolUse = true
				outIdx := t.nextOutIdx
				t.nextOutIdx++
				t.toolOut[idx] = outIdx
				t.toolIDs[idx] = firstNonEmpty(strOf(tcm["id"]), "fc_item_"+itoa(outIdx))
				t.ensureCreated()
				t.emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": outIdx,
					"item": map[string]any{
						"type": "function_call", "id": t.toolIDs[idx], "status": "in_progress",
						"call_id": t.toolIDs[idx], "name": strOf(fnMap(fn, "name")), "arguments": "",
					},
				})
			}
			if args := strOf(fnMap(fn, "arguments")); args != "" {
				t.emit("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "output_index": t.toolOut[idx], "item_id": t.toolIDs[idx], "delta": args,
				})
			}
		}
	}
}

func (t *openaiSSEToResponsesSSE) finish() {
	if t.done {
		return
	}
	t.done = true
	t.ensureCreated()
	status := t.finishStat
	if status == "" {
		status = "completed"
	}
	t.emit("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": t.id, "object": "response", "status": status, "model": t.model, "output": []any{},
			"usage": map[string]any{"input_tokens": t.inTok, "output_tokens": t.outTok, "total_tokens": t.inTok + t.outTok},
		},
	})
}
