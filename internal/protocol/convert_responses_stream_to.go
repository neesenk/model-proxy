// convert_responses_stream_to.go — the *→responses streaming converters
// (anthropic events / openai chat chunks → Responses SSE). Split from
// convert_responses_stream.go, which keeps the responses→* readers.
// The shared SSE frame pump lives in sse_pump.go.

package protocol

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// rsRevBlock tracks one anthropic content block → a Responses output item.
type rsRevBlock struct {
	outIdx      int
	kind        string // "text" | "thinking" | "tool_use"
	itemID      string
	opened      bool
	acc         string // accumulated text/args (for the *.done text field)
	annotations []map[string]any
	signature   string
	callID      string // tool_use: anthropic block id (becomes call_id)
	name        string // tool_use: function name
}

type anthropicSSEToResponsesSSE struct {
	sc          *bufio.Scanner
	out         []byte
	model, id   string
	started     bool
	done        bool
	nextOutIdx  int
	blocks      map[int]*rsRevBlock
	doneItems   []map[string]any // completed output items (for response.completed.output)
	inTok       int
	outTok      int
	cacheRead   int
	cacheCreate int
	stopRsn     string // raw anthropic stop_reason
	seq         int    // response.* frame sequence_number (real upstreams number every frame)
	r2c         r2cCtx
}

func newAnthropicToResponsesSSE(r io.Reader, model string) *anthropicSSEToResponsesSSE {
	return newAnthropicToResponsesSSENS(r, model, r2cCtx{})
}

func newAnthropicToResponsesSSENS(r io.Reader, model string, r2c r2cCtx) *anthropicSSEToResponsesSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &anthropicSSEToResponsesSSE{sc: sc, model: model, id: "resp_conv", blocks: map[int]*rsRevBlock{}, r2c: r2c}
}

func (t *anthropicSSEToResponsesSSE) emit(event string, payload map[string]any) {
	// Real Responses upstreams put an incrementing sequence_number on every
	// frame (verified 5/5 live); codex CLI validates it.
	payload["sequence_number"] = t.seq
	t.seq++
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
	if pumpSSEFrames(t, t.sc, nil, true) {
		return 0, io.EOF
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *anthropicSSEToResponsesSSE) hasOutput() bool { return len(t.out) > 0 }
func (t *anthropicSSEToResponsesSSE) isDone() bool    { return t.done }

func (t *anthropicSSEToResponsesSSE) drainDone() (eof bool) {
	return len(t.out) == 0
}

// emitFailed emits the terminal response.failed event.
func (t *anthropicSSEToResponsesSSE) emitFailed(message string) {
	t.ensureCreated()
	t.emit("response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": t.id, "object": "response", "status": "failed",
			"error": map[string]any{"code": "api_error", "message": message},
		},
	})
}

func (t *anthropicSSEToResponsesSSE) scanError(err error) {
	convertWarn("anthropic SSE scanner error: " + err.Error())
	t.emitFailed("upstream stream terminated unexpectedly")
	t.done = true
}

func (t *anthropicSSEToResponsesSSE) streamEnd() {
	if t.stopRsn != "" {
		t.finish()
		return
	}
	t.emitFailed("upstream stream terminated before a terminal event")
	t.done = true
}

// dispatch ============================================================

func (t *anthropicSSEToResponsesSSE) dispatch(frameEvent string, dataEvents []string, payload string) {
	if payload == "[DONE]" {
		t.finish()
		return
	}
	for _, parsed := range parseFoldedSSEFrames[map[string]any](payload) {
		data := parsed.value
		event := foldedSSEFrameEvent(frameEvent, dataEvents, parsed.line)
		// Some providers (OpenRouter-style, e.g. aqp's /responses) omit SSE
		// event: lines entirely — fall back to the payload's own "type".
		if event == "" {
			event = strKey(data, "type")
		}
		t.handle(event, data)
		if t.done {
			break
		}
	}
}

func (t *anthropicSSEToResponsesSSE) handle(event string, data map[string]any) {
	switch event {
	case "message_start":
		t.ensureCreated()
		if msg := asMap(data["message"]); msg != nil {
			if id := strOpt(msg["id"]); id != "" {
				t.id = id
			}
			if m := strOpt(msg["model"]); m != "" {
				t.model = m
			}
			if u := asMap(msg["usage"]); u != nil {
				t.inTok = intOf(u["input_tokens"])
				t.cacheRead = intOf(u["cache_read_input_tokens"])
				t.cacheCreate = intOf(u["cache_creation_input_tokens"])
			}
		}
	case "content_block_start":
		idx := intOf(data["index"])
		// Anthropic content blocks are sequential. Some compatible gateways
		// omit/mistype content_block_stop before starting the next block; close
		// every still-open block in index order so output_item.added/done stay
		// paired in the same order and no item is orphaned.
		var openIndexes []int
		for openIndex, prior := range t.blocks {
			if prior != nil && prior.opened {
				openIndexes = append(openIndexes, openIndex)
			}
		}
		sort.Ints(openIndexes)
		for _, openIndex := range openIndexes {
			t.handle("content_block_stop", map[string]any{"index": openIndex})
		}
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
			b.callID = firstNonEmpty(strOpt(cb["id"]), b.itemID)
			b.name = strOf(cb["name"])
			name := b.name
			item := map[string]any{
				"type": "function_call", "id": b.itemID, "status": "in_progress",
				"call_id": b.callID, "name": name, "arguments": "",
			}
			if original, namespace, ok := t.r2c.restoreName(name); ok {
				item["name"] = original
				item["namespace"] = namespace
			}
			t.ensureCreated()
			t.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": outIdx,
				"item": item,
			})
		}
		if cb != nil && kind == "redacted_thinking" {
			// redacted_thinking carries no deltas: emit the reasoning item (with
			// encrypted_content) up front, like tool_use.
			b.opened = true
			b.signature = strOf(cb["data"])
			t.ensureCreated()
			item := map[string]any{"type": "reasoning", "id": b.itemID, "status": "in_progress", "summary": []any{}}
			if b.signature != "" {
				item["encrypted_content"] = b.signature
			}
			t.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": outIdx, "item": item,
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
			if !prepareAnthropicReverseBlock(b, "text") {
				return
			}
			t.ensureCreated()
			if !b.opened {
				b.opened = true
				t.emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": b.outIdx,
					"item": map[string]any{"type": "message", "id": b.itemID, "status": "in_progress", "role": "assistant", "content": []any{}},
				})
				t.emit("response.content_part.added", map[string]any{
					"type": "response.content_part.added", "output_index": b.outIdx, "content_index": 0,
					"item_id": b.itemID,
					"part":    map[string]any{"type": "output_text", "text": ""},
				})
			}
			d := strOf(delta["text"])
			b.acc += d
			t.emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "output_index": b.outIdx, "content_index": 0,
				"item_id": b.itemID, "delta": d,
			})
		case "citations_delta":
			if b.kind != "text" || !b.opened {
				return
			}
			citation := asMap(delta["citation"])
			converted := anthropicCitationsToResponses([]any{citation}, b.acc)
			for _, annotation := range converted {
				index := len(b.annotations)
				b.annotations = append(b.annotations, annotation)
				t.emit("response.output_text.annotation.added", map[string]any{
					"type": "response.output_text.annotation.added", "output_index": b.outIdx,
					"content_index": 0, "item_id": b.itemID, "annotation_index": index,
					"annotation": annotation,
				})
			}
		case "thinking_delta":
			if !prepareAnthropicReverseBlock(b, "thinking") {
				return
			}
			t.ensureCreated()
			if !b.opened {
				b.opened = true
				t.emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": b.outIdx,
					"item": map[string]any{"type": "reasoning", "id": b.itemID, "status": "in_progress", "summary": []any{}},
				})
				t.emit("response.reasoning_summary_part.added", map[string]any{
					"type": "response.reasoning_summary_part.added", "output_index": b.outIdx, "summary_index": 0,
					"item_id": b.itemID,
					"part":    map[string]any{"type": "summary_text", "text": ""},
				})
			}
			d := strOf(delta["thinking"])
			b.acc += d
			t.emit("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "output_index": b.outIdx, "summary_index": 0,
				"item_id": b.itemID, "delta": d,
			})
		case "signature_delta":
			if !prepareAnthropicReverseBlock(b, "thinking") {
				return
			}
			b.signature += strOf(delta["signature"])
		case "input_json_delta":
			// A tool call needs the id/name from content_block_start. Do not
			// manufacture a callable item from a stray arguments delta.
			if b.kind != "tool_use" || !b.opened {
				convertWarn("dropping input_json_delta without an open tool_use block")
				return
			}
			d := strOf(delta["partial_json"])
			b.acc += d
			t.emit("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "output_index": b.outIdx, "item_id": b.itemID, "delta": d,
			})
		default:
			if dt := strOpt(delta["type"]); dt != "" {
				convertWarn("dropping " + dt + " delta (no responses-stream equivalent)")
			}
		}
	case "content_block_stop":
		idx := intOf(data["index"])
		b := t.blocks[idx]
		if b == nil || !b.opened {
			// Unopened block (an empty text/thinking block that never emitted
			// a delta — no output_item.added was synthesized): emit nothing,
			// added/done pairing must hold.
			return
		}
		// done frames carry the COMPLETE item (real upstreams do), built from
		// the accumulated state; it also lands in response.completed.output.
		var doneItem map[string]any
		switch b.kind {
		case "text":
			t.emit("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "output_index": b.outIdx, "content_index": 0,
				"item_id": b.itemID, "text": b.acc,
			})
			part := map[string]any{"type": "output_text", "text": b.acc}
			if len(b.annotations) > 0 {
				part["annotations"] = b.annotations
			}
			t.emit("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "output_index": b.outIdx, "content_index": 0,
				"item_id": b.itemID, "part": part,
			})
			doneItem = map[string]any{
				"type": "message", "id": b.itemID, "status": "completed", "role": "assistant",
				"content": []map[string]any{part},
			}
		case "thinking":
			doneItem = map[string]any{"type": "reasoning", "id": b.itemID, "status": "completed", "summary": []map[string]any{{"type": "summary_text", "text": b.acc}}}
			if b.signature != "" {
				doneItem["encrypted_content"] = b.signature
			}
			// Real upstreams pair reasoning_summary_text.delta with a .done
			// carrying the full text, and summary_part.added/done carry the
			// PART (not the item).
			t.emit("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "output_index": b.outIdx, "summary_index": 0,
				"item_id": b.itemID, "text": b.acc,
			})
			t.emit("response.reasoning_summary_part.done", map[string]any{
				"type": "response.reasoning_summary_part.done", "output_index": b.outIdx, "summary_index": 0,
				"item_id": b.itemID,
				"part":    map[string]any{"type": "summary_text", "text": b.acc},
			})
		case "redacted_thinking":
			doneItem = map[string]any{"type": "reasoning", "id": b.itemID, "status": "completed", "summary": []any{}}
			if b.signature != "" {
				doneItem["encrypted_content"] = b.signature
			}
		case "tool_use":
			t.emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": b.outIdx, "item_id": b.itemID, "arguments": b.acc})
			doneItem = map[string]any{
				"type": "function_call", "id": b.itemID, "status": "completed",
				"call_id":   firstNonEmpty(b.callID, b.itemID),
				"name":      b.name,
				"arguments": firstNonEmpty(b.acc, "{}"),
			}
			if original, namespace, ok := t.r2c.restoreName(b.name); ok {
				doneItem["name"] = original
				doneItem["namespace"] = namespace
			}
		default:
			doneItem = map[string]any{"type": anthropicKindToResponsesItem(b.kind), "id": b.itemID, "status": "completed"}
		}
		t.doneItems = append(t.doneItems, doneItem)
		t.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": b.outIdx,
			"item": doneItem,
		})
		b.opened = false
	case "message_delta":
		if d := asMap(data["delta"]); d != nil {
			if sr := strOpt(d["stop_reason"]); sr != "" {
				t.stopRsn = sr // raw; mapped to status at finish
			}
		}
		if u := asMap(data["usage"]); u != nil {
			// Presence-based: anthropic message_delta usage usually carries only
			// output_tokens — don't clobber message_start's input/cache values.
			if v, ok := u["input_tokens"]; ok {
				t.inTok = intOf(v)
			}
			if v, ok := u["output_tokens"]; ok {
				t.outTok = intOf(v)
			}
			if v, ok := u["cache_read_input_tokens"]; ok {
				t.cacheRead = intOf(v)
			}
			if v, ok := u["cache_creation_input_tokens"]; ok {
				t.cacheCreate = intOf(v)
			}
		}
	case "message_stop":
		// A bare message_stop without a preceding stop_reason-carrying
		// message_delta is a truncated generation — the same-protocol cache
		// gate requires a non-empty stop_reason. finish() would synthesize
		// response.completed, faking a clean terminal on exactly the client
		// bytes that gate checks, so fail closed exactly like streamEnd's
		// no-terminal branch.
		if t.stopRsn == "" {
			t.emitFailed("upstream stream ended without a terminal stop_reason")
			t.done = true
			return
		}
		t.finish()
	case "ping":
		// keep-alive, no responses equivalent
	case "error":
		emsg, etype := "", "api_error"
		if e := asMap(data["error"]); e != nil {
			emsg = strOf(e["message"])
			etype = firstNonEmpty(strOpt(e["type"]), "api_error")
		}
		t.ensureCreated()
		t.emit("response.failed", map[string]any{
			"type":     "response.failed",
			"response": map[string]any{"id": t.id, "object": "response", "status": "failed", "error": map[string]any{"code": etype, "message": emsg}},
		})
		t.done = true
	default:
		convertWarn("ignoring unknown anthropic SSE event (a→r): " + event)
	}
}

// prepareAnthropicReverseBlock aligns a block with the semantic type carried
// by its delta. A few compatible gateways omit content_block.type; infer it
// before output_item.added so its id/type still matches output_item.done.
// Conflicting explicit block/delta types are malformed and are ignored rather
// than producing an internally inconsistent Responses stream.
func prepareAnthropicReverseBlock(b *rsRevBlock, kind string) bool {
	if b.kind == "" {
		b.kind = kind
		b.itemID = fmtItemID(kind, b.outIdx)
		return true
	}
	if b.kind != kind {
		convertWarn("dropping " + kind + " delta for incompatible anthropic block type " + b.kind)
		return false
	}
	return true
}

func (t *anthropicSSEToResponsesSSE) finish() {
	if t.done {
		return
	}
	// Some Anthropic-compatible gateways omit content_block_stop and jump
	// directly to message_stop. Close every opened item before the terminal
	// response so output_item.added/done remains paired.
	indexes := make([]int, 0, len(t.blocks))
	for index, block := range t.blocks {
		if block.opened {
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		t.handle("content_block_stop", map[string]any{"index": index})
	}
	t.done = true
	t.ensureCreated()
	status, incReason := anthropicStopToResponsesDetail(t.stopRsn)
	// responses input_tokens is inclusive of cache traffic (same convention as
	// anthropic→chat prompt_tokens).
	input := t.inTok + t.cacheRead + t.cacheCreate
	usage := map[string]any{"input_tokens": input, "output_tokens": t.outTok, "total_tokens": input + t.outTok}
	if t.cacheRead > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": t.cacheRead}
	}
	resp := map[string]any{
		"id": t.id, "object": "response", "status": status, "model": t.model, "output": doneItemsOutput(t.doneItems),
		"created_at": time.Now().Unix(),
		"usage":      usage,
	}
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
		if incReason != "" {
			resp["incomplete_details"] = map[string]any{"reason": incReason}
		}
	}
	t.emit(event, map[string]any{"type": event, "response": resp})
}

// doneItemsOutput renders the completed-items list as a non-nil []any for
// response.completed.output (nil would marshal as "null").
func doneItemsOutput(items []map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	return out
}

// fmtItemID synthesizes a Responses item id for a reverse-synthesized stream.
func fmtItemID(kind string, outIdx int) string {
	switch kind {
	case "tool_use":
		return "fc_item_" + itoa(outIdx)
	case "thinking", "redacted_thinking":
		return "rs_item_" + itoa(outIdx)
	default:
		return "msg_item_" + itoa(outIdx)
	}
}

// anthropicKindToResponsesItem maps an anthropic content-block kind to the
// Responses output-item type (so output_item.done matches output_item.added).
func anthropicKindToResponsesItem(kind string) string {
	switch kind {
	case "tool_use":
		return "function_call"
	case "thinking", "redacted_thinking":
		return "reasoning"
	default: // "text"
		return "message"
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
	sc              *bufio.Scanner
	out             []byte
	model, id       string
	started         bool
	done            bool
	nextOutIdx      int
	textOut         int // output_index of the open message/text item (-1 none)
	msgOpened       bool
	msgParts        int // content parts added to the message item (next content_index)
	textOpened      bool
	textPartIdx     int    // content_index of the output_text part within the message item
	textAcc         string // accumulated text (for the *.done text field)
	textAnnotations []map[string]any
	refOpened       bool   // refusal content part opened on the message item
	refPartIdx      int    // content_index of the refusal part
	refAcc          string // accumulated refusal text
	rsOut           int    // output_index of the open reasoning item (-1 none)
	rsOpened        bool
	rsAcc           string           // accumulated reasoning text
	toolOut         map[int]int      // chat tool_calls index → responses output_index
	toolIDs         map[int]string   // chat tool_calls index → call id
	toolNames       map[int]string   // chat tool_calls index → function name (may arrive in a LATER chunk)
	toolAcc         map[int]string   // chat tool_calls index → accumulated arguments
	toolAdded       map[int]bool     // output_item.added emitted (delayed until the name is known)
	toolDelta       map[int]int      // bytes of toolAcc already streamed as arguments.delta
	toolCustom      map[int]bool     // chat tool index → custom/freeform call (unwrap {"input":...})
	toolInSent      map[int]int      // bytes of the UNWRAPPED custom input already streamed
	r2c             r2cCtx           // responses→chat context (namespace restore + custom set; zero = no-op)
	doneItems       []map[string]any // completed output items (for response.completed.output)
	inTok           int
	outTok          int
	cachedTok       int    // prompt_tokens_details.cached_tokens from the usage chunk
	rsTok           int    // completion_tokens_details.reasoning_tokens from the usage chunk
	finishRsn       string // raw chat finish_reason
	hasToolUse      bool
	thinkMode       int    // inline <think> splitter: thinkDetecting | thinkReasoning | thinkText
	thinkBuf        string // buffer while thinkMode != thinkText
	seq             int    // response.* frame sequence_number (real upstreams number every frame)
}

func newOpenAIToResponsesSSE(r io.Reader, model string) *openaiSSEToResponsesSSE {
	return newOpenAIToResponsesSSENS(r, model, r2cCtx{})
}

// newOpenAIToResponsesSSENS wires the responses→chat context (MCP namespace
// restore + custom/freeform tool set, rebuilt from the original responses
// request); zero value = no-op.
func newOpenAIToResponsesSSENS(r io.Reader, model string, r2c r2cCtx) *openaiSSEToResponsesSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &openaiSSEToResponsesSSE{sc: sc, model: model, id: "resp_conv", textOut: -1, rsOut: -1,
		toolOut: map[int]int{}, toolIDs: map[int]string{}, toolNames: map[int]string{},
		toolAcc: map[int]string{}, toolAdded: map[int]bool{}, toolDelta: map[int]int{},
		toolCustom: map[int]bool{}, toolInSent: map[int]int{}, r2c: r2c}
}

func (t *openaiSSEToResponsesSSE) emit(event string, payload map[string]any) {
	// Real Responses upstreams put an incrementing sequence_number on every
	// frame (verified 5/5 live); codex CLI validates it.
	payload["sequence_number"] = t.seq
	t.seq++
	sseEmit(&t.out, event, payload)
}

func (t *openaiSSEToResponsesSSE) ensureCreated() {
	if t.started {
		return
	}
	t.started = true
	t.emit("response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": t.id, "object": "response", "status": "in_progress", "model": t.model, "output": []any{}},
	})
}

// msgItemID returns the message item's synthesized id (empty-safe: only
// meaningful once openMsgItem ran).
func (t *openaiSSEToResponsesSSE) msgItemID() string { return "msg_item_" + itoa(t.textOut) }

// rsItemID returns the reasoning item's synthesized id.
func (t *openaiSSEToResponsesSSE) rsItemID() string { return "rs_item_" + itoa(t.rsOut) }

// openMsgItem emits the message output_item.added once; text and refusal
// content parts attach to it on demand.
func (t *openaiSSEToResponsesSSE) openMsgItem() {
	if t.msgOpened {
		return
	}
	t.msgOpened = true
	t.textOut = t.nextOutIdx
	t.nextOutIdx++
	t.ensureCreated()
	t.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": t.textOut,
		"item": map[string]any{"type": "message", "id": t.msgItemID(), "status": "in_progress", "role": "assistant", "content": []any{}},
	})
}

func (t *openaiSSEToResponsesSSE) openText() {
	if t.textOpened {
		return
	}
	t.textOpened = true
	t.openMsgItem()
	t.textPartIdx = t.msgParts
	t.msgParts++
	t.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "output_index": t.textOut, "content_index": t.textPartIdx,
		"item_id": t.msgItemID(),
		"part":    map[string]any{"type": "output_text", "text": ""},
	})
}

// pushRefusalDelta streams one chat delta.refusal fragment as a
// response.refusal.delta within a refusal content part (the Responses
// representation of refusal; aligned with the non-streaming refusal part).
func (t *openaiSSEToResponsesSSE) pushRefusalDelta(d string) {
	t.ensureCreated()
	if !t.refOpened {
		t.refOpened = true
		t.openMsgItem()
		t.refPartIdx = t.msgParts
		t.msgParts++
		t.emit("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "output_index": t.textOut, "content_index": t.refPartIdx,
			"item_id": t.msgItemID(),
			"part":    map[string]any{"type": "refusal", "refusal": ""},
		})
	}
	t.refAcc += d
	t.emit("response.refusal.delta", map[string]any{
		"type": "response.refusal.delta", "output_index": t.textOut, "content_index": t.refPartIdx,
		"item_id": t.msgItemID(), "delta": d,
	})
}

// openReasoning opens the single reasoning item (chat reasoning_content
// streams as one reasoning-summary item, like openText).
func (t *openaiSSEToResponsesSSE) openReasoning() {
	if t.rsOpened {
		return
	}
	t.rsOpened = true
	t.rsOut = t.nextOutIdx
	t.nextOutIdx++
	t.ensureCreated()
	t.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": t.rsOut,
		"item": map[string]any{"type": "reasoning", "id": t.rsItemID(), "status": "in_progress", "summary": []any{}},
	})
	t.emit("response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "output_index": t.rsOut, "summary_index": 0,
		"item_id": t.rsItemID(),
		"part":    map[string]any{"type": "summary_text", "text": ""},
	})
}

func (t *openaiSSEToResponsesSSE) Read(p []byte) (int, error) {
	if pumpSSEFrames(t, t.sc, nil, true) {
		return 0, io.EOF
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *openaiSSEToResponsesSSE) hasOutput() bool { return len(t.out) > 0 }
func (t *openaiSSEToResponsesSSE) isDone() bool    { return t.done }

func (t *openaiSSEToResponsesSSE) drainDone() (eof bool) {
	return len(t.out) == 0
}

// emitFailed emits the terminal response.failed event.
func (t *openaiSSEToResponsesSSE) emitFailed(message string) {
	t.ensureCreated()
	t.emit("response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": t.id, "object": "response", "status": "failed",
			"error": map[string]any{"code": "api_error", "message": message},
		},
	})
}

func (t *openaiSSEToResponsesSSE) scanError(err error) {
	convertWarn("openai SSE scanner error: " + err.Error())
	t.emitFailed("upstream stream terminated unexpectedly")
	t.done = true
}

func (t *openaiSSEToResponsesSSE) streamEnd() {
	if t.finishRsn != "" {
		t.finish()
		return
	}
	t.emitFailed("upstream stream terminated before a terminal event")
	t.done = true
}

// dispatch ============================================================

func (t *openaiSSEToResponsesSSE) dispatch(frameEvent string, dataEvents []string, payload string) {
	if payload == "[DONE]" {
		t.finish()
		return
	}
	for _, parsed := range parseFoldedSSEFrames[map[string]any](payload) {
		event := foldedSSEFrameEvent(frameEvent, dataEvents, parsed.line)
		t.handle(event, parsed.value)
		if t.done {
			break
		}
	}
}

// chatSSEErrorOf extracts (message, type) from a chat error frame: the error
// object when present, else the bare payload (message/detail keys; cc-switch
// extract_chat_sse_error).
func chatSSEErrorOf(data map[string]any) (emsg, etype string) {
	e := data
	if em := asMap(data["error"]); em != nil {
		e = em
	}
	etype = firstNonEmpty(strOpt(e["type"]), strOpt(e["code"]), "api_error")
	emsg = firstNonEmpty(strOpt(e["message"]), strOpt(e["detail"]))
	if emsg == "" {
		emsg = strOpt(data["error"])
	}
	if emsg == "" {
		emsg = "upstream stream error"
	}
	return
}

func (t *openaiSSEToResponsesSSE) handle(event string, data map[string]any) {
	if id := strOpt(data["id"]); id != "" {
		t.id = id
	}
	if m := strOpt(data["model"]); m != "" {
		t.model = m
	}
	if u := asMap(data["usage"]); u != nil {
		t.inTok = intOf(u["prompt_tokens"])
		t.outTok = intOf(u["completion_tokens"])
		t.cachedTok = intOf(asMap(u["prompt_tokens_details"])["cached_tokens"])
		t.rsTok = intOf(asMap(u["completion_tokens_details"])["reasoning_tokens"])
	}
	// OpenAI error chunk (data: {"error":{...}} — or the string form some
	// gateways emit, which chatSSEErrorOf unwraps) or an explicit `event:
	// error` frame → response.failed. Never silently turn a mid-stream
	// upstream error into a clean response.completed.
	if data["error"] != nil || event == "error" {
		emsg, etype := chatSSEErrorOf(data)
		t.ensureCreated()
		t.emit("response.failed", map[string]any{
			"type":     "response.failed",
			"response": map[string]any{"id": t.id, "object": "response", "status": "failed", "error": map[string]any{"code": etype, "message": emsg}},
		})
		t.done = true
		return
	}
	// finish_reason ends normal Chat content. Continue reading so a
	// conventional trailing usage-only chunk can update accounting and a
	// later explicit error can still fail closed, but never expose normal
	// content/tools emitted after the terminal.
	if t.finishRsn != "" {
		return
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
	if fr := strOpt(ch["finish_reason"]); fr != "" {
		t.finishRsn = fr
	}
	if delta == nil {
		return
	}
	if c, ok := delta["content"].(string); ok && c != "" {
		t.pushContentDelta(c)
	}
	// Refusal streams as a refusal content part with response.refusal.delta
	// events (the Responses representation of refusal).
	if rf, ok := delta["refusal"].(string); ok && rf != "" {
		t.pushRefusalDelta(rf)
	}
	for _, annotation := range chatAnnotationsToResponses(delta["annotations"]) {
		t.ensureCreated()
		t.openText()
		index := len(t.textAnnotations)
		t.textAnnotations = append(t.textAnnotations, annotation)
		t.emit("response.output_text.annotation.added", map[string]any{
			"type": "response.output_text.annotation.added", "output_index": t.textOut,
			"content_index": t.textPartIdx, "item_id": t.msgItemID(),
			"annotation_index": index, "annotation": annotation,
		})
	}
	// Vendor reasoning spellings (cc-switch codex_chat_common's extraction
	// order): reasoning_content (DeepSeek/zhipu) > reasoning (OpenRouter
	// string, or a {content,text,summary} object) > reasoning_details
	// (OpenRouter-style array/object).
	rc := chatReasoningText(delta)
	if rc != "" {
		t.ensureCreated()
		t.openReasoning()
		t.rsAcc += rc
		t.emit("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "output_index": t.rsOut, "summary_index": 0,
			"item_id": t.rsItemID(), "delta": rc,
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
				t.toolIDs[idx] = firstNonEmpty(strOpt(tcm["id"]), "fc_item_"+itoa(outIdx))
			}
			// Late-arriving identity (DashScope/xAI-style: id/name can come in a
			// LATER chunk; empty fragments never overwrite what we already have).
			// A real id upgrades the synthesized fallback only while the item is
			// unopened — after added, the id must stay stable across frames.
			if name := strOpt(fnMap(fn, "name")); name != "" && t.toolNames[idx] == "" {
				t.toolNames[idx] = name
			}
			if id := strOpt(tcm["id"]); id != "" && !t.toolAdded[idx] {
				t.toolIDs[idx] = id
			}
			if args := strOpt(fnMap(fn, "arguments")); args != "" {
				t.toolAcc[idx] += args
			}
		}
		t.openReadyToolCalls()
	}
}

// openReadyToolCalls emits output_item.added (+ buffered arguments deltas) for
// tool calls whose name is known, in output_index order, stopping at the
// first still-anonymous call — contiguous release (cc-switch does the same):
// a later call's added never overtakes an earlier anonymous one, so added
// order always matches output_index order.
func (t *openaiSSEToResponsesSSE) openReadyToolCalls() {
	var pending []int
	for idx := range t.toolOut {
		if !t.toolAdded[idx] {
			pending = append(pending, idx)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return t.toolOut[pending[i]] < t.toolOut[pending[j]] })
	for _, idx := range pending {
		if t.toolNames[idx] == "" {
			return // hold everything behind the first anonymous call
		}
		t.toolAdded[idx] = true
		t.ensureCreated()
		name := t.toolNames[idx]
		if t.r2c.custom[name] {
			// Custom/freeform call: custom_tool_call item (ctc_item_ id); args
			// stream as progressive-unwrapped custom_tool_call_input deltas.
			t.toolCustom[idx] = true
			t.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": t.toolOut[idx],
				"item": map[string]any{
					"type": "custom_tool_call", "id": "ctc_item_" + itoa(t.toolOut[idx]), "status": "in_progress",
					"call_id": t.toolIDs[idx], "name": name, "input": "",
				},
			})
			continue
		}
		item := map[string]any{
			"type": "function_call", "id": t.toolIDs[idx], "status": "in_progress",
			"call_id": t.toolIDs[idx], "name": name, "arguments": "",
		}
		// MCP namespace restore on the added frame (done frames carry no name).
		if orig, ns, ok := t.r2c.restoreName(name); ok {
			item["name"] = orig
			item["namespace"] = ns
		}
		t.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": t.toolOut[idx],
			"item": item,
		})
	}
	// Flush buffered args for everything now open.
	for idx := range t.toolOut {
		if !t.toolAdded[idx] {
			continue
		}
		if t.toolCustom[idx] {
			// Progressive unwrap (idempotent from the full accumulated raw):
			// emit only the newly decodable unescaped content.
			full := newPartialInputUnwrapper().unwrap(t.toolAcc[idx], false)
			if len(full) > t.toolInSent[idx] {
				d := full[t.toolInSent[idx]:]
				t.toolInSent[idx] = len(full)
				t.emit("response.custom_tool_call_input.delta", map[string]any{
					"type": "response.custom_tool_call_input.delta", "output_index": t.toolOut[idx],
					"item_id": "ctc_item_" + itoa(t.toolOut[idx]), "delta": d,
				})
			}
			continue
		}
		if len(t.toolAcc[idx]) > t.toolDelta[idx] {
			d := t.toolAcc[idx][t.toolDelta[idx]:]
			t.toolDelta[idx] = len(t.toolAcc[idx])
			t.emit("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "output_index": t.toolOut[idx], "item_id": t.toolIDs[idx], "delta": d,
			})
		}
	}
}

// Inline <think> splitting (cc-switch streaming_codex_chat.rs:175-265): some
// chat upstreams (MiniMax-style) inline a LEADING <think>…</think> block in
// content instead of using a reasoning field. The block streams as reasoning,
// the rest as text; mid-text blocks stay literal.
const (
	thinkDetecting = iota // still possible the stream starts with <think>
	thinkReasoning        // inside a leading think block
	thinkText             // plain text (decided)
)

const (
	thinkNeedMore = iota
	thinkIsReasoning
	thinkIsText
)

// thinkPrefixDecision classifies a buffered content prefix: still a possible
// <think> prefix, a confirmed think block start, or definitely plain text.
func thinkPrefixDecision(buf string) int {
	trimmed := strings.TrimLeft(buf, " \t\r\n")
	switch {
	case strings.HasPrefix(trimmed, thinkOpenTag):
		return thinkIsReasoning
	case strings.HasPrefix(thinkOpenTag, trimmed):
		return thinkNeedMore
	}
	return thinkIsText
}

// pushContentDelta routes one content delta through the inline <think>
// splitter.
func (t *openaiSSEToResponsesSSE) pushContentDelta(d string) {
	switch t.thinkMode {
	case thinkReasoning:
		t.thinkBuf += d
		t.drainInlineThink()
	case thinkText:
		t.pushTextDelta(d)
	default: // thinkDetecting
		t.thinkBuf += d
		switch thinkPrefixDecision(t.thinkBuf) {
		case thinkIsReasoning:
			t.thinkMode = thinkReasoning
			t.drainInlineThink()
		case thinkIsText:
			t.thinkMode = thinkText
			buf := t.thinkBuf
			t.thinkBuf = ""
			t.pushTextDelta(buf)
		} // thinkNeedMore: keep buffering
	}
}

// pushTextDelta emits one text delta (opening the message item lazily).
func (t *openaiSSEToResponsesSSE) pushTextDelta(d string) {
	t.ensureCreated()
	t.openText()
	t.textAcc += d
	t.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": t.textOut, "content_index": t.textPartIdx,
		"item_id": t.msgItemID(), "delta": d,
	})
}

// pushReasoningDelta emits one reasoning delta (opening the reasoning item
// lazily).
func (t *openaiSSEToResponsesSSE) pushReasoningDelta(d string) {
	t.ensureCreated()
	t.openReasoning()
	t.rsAcc += d
	t.emit("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "output_index": t.rsOut, "summary_index": 0,
		"item_id": t.rsItemID(), "delta": d,
	})
}

// drainInlineThink emits the split once the buffered think block is complete.
func (t *openaiSSEToResponsesSSE) drainInlineThink() {
	reasoning, answer, ok := splitLeadingThinkBlock(t.thinkBuf)
	if !ok {
		return
	}
	t.thinkMode = thinkText
	t.thinkBuf = ""
	if reasoning != "" {
		t.pushReasoningDelta(reasoning)
	}
	if answer != "" {
		t.pushTextDelta(answer)
	}
}

// flushInlineThink settles a pending think buffer at stream end: an
// undecided buffer is plain text; an UNTERMINATED leading think block is
// entirely reasoning (minus the open tag) — better reasoning than lost.
func (t *openaiSSEToResponsesSSE) flushInlineThink() {
	switch t.thinkMode {
	case thinkDetecting:
		t.thinkMode = thinkText
		if t.thinkBuf != "" {
			buf := t.thinkBuf
			t.thinkBuf = ""
			t.pushTextDelta(buf)
		}
	case thinkReasoning:
		t.thinkMode = thinkText
		buffered := t.thinkBuf
		t.thinkBuf = ""
		if reasoning, answer, ok := splitLeadingThinkBlock(buffered); ok {
			if reasoning != "" {
				t.pushReasoningDelta(reasoning)
			}
			if answer != "" {
				t.pushTextDelta(answer)
			}
			return
		}
		reasoning := strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(buffered, " \t\r\n"), thinkOpenTag))
		if reasoning != "" {
			t.pushReasoningDelta(reasoning)
		}
	}
}

func (t *openaiSSEToResponsesSSE) finish() {
	if t.done {
		return
	}
	for _, args := range t.toolAcc {
		if args == "" {
			continue
		}
		if !json.Valid([]byte(args)) {
			t.done = true
			t.ensureCreated()
			t.emit("response.failed", map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id": t.id, "object": "response", "status": "failed",
					"error": map[string]any{"code": "api_error", "message": "upstream stream ended with incomplete tool arguments"},
				},
			})
			return
		}
	}
	t.done = true
	t.ensureCreated()
	t.flushInlineThink()
	t.closeItems()
	status, incReason := openAIFinishToResponsesDetail(t.finishRsn)
	usage := map[string]any{"input_tokens": t.inTok, "output_tokens": t.outTok, "total_tokens": t.inTok + t.outTok}
	if t.cachedTok > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": t.cachedTok}
	}
	if t.rsTok > 0 {
		usage["output_tokens_details"] = map[string]any{"reasoning_tokens": t.rsTok}
	}
	resp := map[string]any{
		"id": t.id, "object": "response", "status": status, "model": t.model, "output": doneItemsOutput(t.doneItems),
		"created_at": time.Now().Unix(),
		"usage":      usage,
	}
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
		if incReason != "" {
			resp["incomplete_details"] = map[string]any{"reason": incReason}
		}
	}
	t.emit(event, map[string]any{"type": event, "response": resp})
}

// closeItems emits the *.done + response.output_item.done frames for every
// opened item, in output_index order — the contract pairs every
// output_item.added with an output_item.done carrying the same id and type.
func (t *openaiSSEToResponsesSSE) closeItems() {
	type openItem struct {
		outIdx  int
		kind    string // "message" | "reasoning" | "function_call"
		toolIdx int    // chat tool_calls index (function_call only)
	}
	var open []openItem
	if t.rsOpened {
		open = append(open, openItem{t.rsOut, "reasoning", -1})
	}
	if t.msgOpened {
		open = append(open, openItem{t.textOut, "message", -1})
	}
	for chatIdx, outIdx := range t.toolOut {
		open = append(open, openItem{outIdx, "function_call", chatIdx})
	}
	sort.Slice(open, func(i, j int) bool { return open[i].outIdx < open[j].outIdx })
	for _, it := range open {
		switch it.kind {
		case "message":
			id := t.msgItemID()
			// Close the opened content parts in content_index order (text and
			// refusal are the only parts this converter opens).
			type msgPart struct {
				idx  int
				part map[string]any
			}
			var parts []msgPart
			if t.textOpened {
				part := map[string]any{"type": "output_text", "text": t.textAcc}
				if len(t.textAnnotations) > 0 {
					part["annotations"] = t.textAnnotations
				}
				parts = append(parts, msgPart{t.textPartIdx, part})
			}
			if t.refOpened {
				parts = append(parts, msgPart{t.refPartIdx, map[string]any{"type": "refusal", "refusal": t.refAcc}})
			}
			sort.Slice(parts, func(i, j int) bool { return parts[i].idx < parts[j].idx })
			content := make([]map[string]any, 0, len(parts))
			for _, mp := range parts {
				switch strOf(mp.part["type"]) {
				case "output_text":
					t.emit("response.output_text.done", map[string]any{
						"type": "response.output_text.done", "output_index": it.outIdx, "content_index": mp.idx,
						"item_id": id, "text": t.textAcc,
					})
				case "refusal":
					t.emit("response.refusal.done", map[string]any{
						"type": "response.refusal.done", "output_index": it.outIdx, "content_index": mp.idx,
						"item_id": id, "refusal": t.refAcc,
					})
				}
				t.emit("response.content_part.done", map[string]any{
					"type": "response.content_part.done", "output_index": it.outIdx, "content_index": mp.idx,
					"item_id": id, "part": mp.part,
				})
				content = append(content, mp.part)
			}
			doneItem := map[string]any{
				"type": "message", "id": id, "status": "completed", "role": "assistant",
				"content": content,
			}
			t.doneItems = append(t.doneItems, doneItem)
			t.emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": it.outIdx,
				"item": doneItem,
			})
		case "reasoning":
			id := t.rsItemID()
			doneItem := map[string]any{"type": "reasoning", "id": id, "status": "completed",
				"summary": []map[string]any{{"type": "summary_text", "text": t.rsAcc}}}
			// Real upstreams pair reasoning_summary_text.delta with a .done
			// carrying the full text, and summary_part.added/done carry the
			// PART (not the item).
			t.emit("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "output_index": it.outIdx, "summary_index": 0,
				"item_id": id, "text": t.rsAcc,
			})
			t.emit("response.reasoning_summary_part.done", map[string]any{
				"type": "response.reasoning_summary_part.done", "output_index": it.outIdx, "summary_index": 0,
				"item_id": id,
				"part":    map[string]any{"type": "summary_text", "text": t.rsAcc},
			})
			t.doneItems = append(t.doneItems, doneItem)
			t.emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": it.outIdx,
				"item": doneItem,
			})
		case "function_call":
			id := t.toolIDs[it.toolIdx]
			if t.toolCustom[it.toolIdx] {
				// Custom/freeform done: final unwrapped delta (if any) +
				// custom_tool_call_input.done with the FULL unwrapped input.
				ctcID := "ctc_item_" + itoa(it.outIdx)
				full := newPartialInputUnwrapper().unwrap(t.toolAcc[it.toolIdx], true)
				if len(full) > t.toolInSent[it.toolIdx] {
					d := full[t.toolInSent[it.toolIdx]:]
					t.emit("response.custom_tool_call_input.delta", map[string]any{
						"type": "response.custom_tool_call_input.delta", "output_index": it.outIdx, "item_id": ctcID, "delta": d,
					})
				}
				t.emit("response.custom_tool_call_input.done", map[string]any{
					"type": "response.custom_tool_call_input.done", "output_index": it.outIdx, "item_id": ctcID, "input": full,
				})
				doneItem := map[string]any{
					"type": "custom_tool_call", "id": ctcID, "status": "completed",
					"call_id": id, "name": t.toolNames[it.toolIdx], "input": full,
				}
				t.doneItems = append(t.doneItems, doneItem)
				t.emit("response.output_item.done", map[string]any{
					"type": "response.output_item.done", "output_index": it.outIdx,
					"item": doneItem,
				})
				continue
			}
			if !t.toolAdded[it.toolIdx] {
				// The name never arrived: open the item now (added/done must
				// pair) with whatever identity we have, even a nameless one.
				if t.toolNames[it.toolIdx] == "" {
					convertWarn("chat→r stream: tool_call finished without a function name")
				}
				t.emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": it.outIdx,
					"item": map[string]any{
						"type": "function_call", "id": id, "status": "in_progress",
						"call_id": id, "name": t.toolNames[it.toolIdx], "arguments": "",
					},
				})
			}
			t.emit("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "output_index": it.outIdx, "item_id": id,
				"arguments": t.toolAcc[it.toolIdx],
			})
			doneItem := map[string]any{
				"type": "function_call", "id": id, "status": "completed",
				"call_id":   id,
				"name":      t.toolNames[it.toolIdx],
				"arguments": firstNonEmpty(t.toolAcc[it.toolIdx], "{}"),
			}
			// MCP namespace restore on the done frame, mirroring added.
			if orig, ns, ok := t.r2c.restoreName(t.toolNames[it.toolIdx]); ok {
				doneItem["name"] = orig
				doneItem["namespace"] = ns
			}
			t.doneItems = append(t.doneItems, doneItem)
			t.emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": it.outIdx,
				"item": doneItem,
			})
		}
	}
}
