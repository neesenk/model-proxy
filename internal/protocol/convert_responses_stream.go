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
package protocol

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
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

// responsesErrorOf extracts (message, type) from a response.failed / error
// event: the error object is top-level on `error` events but nested inside
// the response object on response.failed.
func responsesErrorOf(data map[string]any) (emsg, etype string) {
	emsg, etype = "", "api_error"
	e := asMap(data["error"])
	if e == nil {
		e = asMap(asMap(data["response"])["error"])
	}
	if e != nil {
		emsg = strOf(e["message"])
		etype = firstNonEmpty(strOpt(e["type"]), "api_error")
	}
	return
}

// ===========================================================================
// responses → anthropic (codex/Responses backend → Claude Code client)
// ===========================================================================

// rsBlock tracks one Responses output item's anthropic-side block state.
type rsBlock struct {
	kind     string // "text" | "tool_use" | "thinking"
	idx      int    // anthropic content_block index
	opened   bool   // content_block_start emitted
	itemID   string // responses item id (for completeness)
	argsSeen bool   // tool_use: at least one arguments delta was emitted
	// seenCitations dedups citation link URLs across annotation events of this
	// block — each response.output_text.annotation.added event carries one
	// annotation, so a URL cited N times would otherwise append N links.
	seenCitations map[string]bool
}

// Early arguments deltas (a gateway that emits function_call_arguments.delta
// BEFORE output_item.added, or skips added entirely) are buffered so the first
// argument segment is not lost — losing it corrupts the arguments JSON.
// Buffered text is keyed by item_id when the frame carries one, else by
// output_index (opencodex's chat outbound buffers by item_id the same way).
func pendingToolArgsKey(itemID string, outIdx int) string {
	if itemID != "" {
		return "id:" + itemID
	}
	return "idx:" + itoa(outIdx)
}

// takePendingArgs removes and returns buffered early arguments deltas for an
// item, trying the item_id key first, then the output_index key.
func takePendingArgs(buf map[string]string, itemID string, outIdx int) string {
	if itemID != "" {
		k := "id:" + itemID
		if d := buf[k]; d != "" {
			delete(buf, k)
			return d
		}
	}
	k := "idx:" + itoa(outIdx)
	d := buf[k]
	delete(buf, k)
	return d
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
	cachedTok   int // input_tokens_details.cached_tokens from response.completed/incomplete
	createTok   int // cache write: cache_creation_input_tokens (direct) or input_tokens_details.cache_write_tokens
	stopRsn     string
	hasToolUse  bool
	pendArgs    map[string]string // early arguments deltas (before added/done)
}

func newResponsesToAnthropicSSE(r io.Reader, model string) *responsesSSEToAnthropicSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &responsesSSEToAnthropicSSE{
		sc: sc, model: model, id: "msg_conv",
		blocks: map[int]*rsBlock{}, curTextOut: -1, curThinkOut: -1,
		pendArgs: map[string]string{},
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
			t.ensureStart()
			for outIdx := range t.blocks {
				t.closeBlock(outIdx)
			}
			t.emit("error", map[string]any{"type": "error", "error": map[string]any{
				"type": "api_error", "message": "upstream stream terminated before a terminal event",
			}})
			t.errored = true
			t.done = true
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
			t.ensureStart()
			for outIdx := range t.blocks {
				t.closeBlock(outIdx)
			}
			t.emit("error", map[string]any{"type": "error", "error": map[string]any{
				"type": "api_error", "message": "responses stream ended before response.completed",
			}})
			t.errored = true
			t.done = true
			continue
		}
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) != nil {
			continue
		}
		// Some providers (OpenRouter-style, e.g. aqp's /responses) omit SSE
		// event: lines entirely — fall back to the payload's own "type".
		if pendingEvent == "" {
			pendingEvent = strKey(data, "type")
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
			if id := strOpt(resp["id"]); id != "" {
				t.id = id
			}
			if m := strOpt(resp["model"]); m != "" {
				t.model = m
			}
		}
	case "response.output_item.added":
		item := asMap(data["item"])
		if item == nil {
			return
		}
		outIdx := intOf(data["output_index"])
		if t.blocks[outIdx] != nil {
			// Duplicate added for the same item (some gateways re-announce):
			// keep the existing block and its streamed deltas — re-emitting
			// content_block_start would corrupt the anthropic block sequence.
			return
		}
		kind := ""
		switch item["type"] {
		case "message":
			kind = "text"
		case "function_call":
			kind = "tool_use"
		case "reasoning":
			kind = "thinking"
		case "tool_search_call":
			kind = "tool_search"
		case "web_search_call":
			kind = "web_search"
		default:
			convertWarn("ignoring unknown responses output item type: " + strOf(item["type"]))
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
					"type":  "tool_use",
					"id":    firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
					"name":  strOpt(item["name"]),
					"input": map[string]any{},
				},
			})
			// Replay arguments deltas that arrived BEFORE this added (buffered
			// by item_id/output_index) so the leading segment is not lost.
			if d := takePendingArgs(t.pendArgs, strOpt(item["id"]), outIdx); d != "" {
				b.argsSeen = true
				t.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": b.idx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": d},
				})
			}
		}
	case "response.output_text.delta", "response.refusal.delta":
		// refusal.delta carries refusal text — stream it as plain text
		// (anthropic has no refusal block; stop_reason carries the semantics,
		// aligned with the convert.go refusal→text fix).
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
	case "response.output_text.annotation.added":
		// Anthropic structured web citations require an encrypted_index that
		// Responses does not expose. Preserve the source visibly as a text
		// delta instead of fabricating an invalid citations_delta.
		outIdx := intOf(data["output_index"])
		b := t.blocks[outIdx]
		if b == nil {
			b = &rsBlock{kind: "text", idx: t.nextIdx}
			t.nextIdx++
			t.blocks[outIdx] = b
		}
		t.ensureStart()
		if !b.opened {
			b.opened = true
			t.emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": b.idx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		if b.seenCitations == nil {
			b.seenCitations = map[string]bool{}
		}
		link := responsesCitationLinks([]map[string]any{asMap(data["annotation"])}, b.seenCitations)
		if link != "" {
			t.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": b.idx,
				"delta": map[string]any{"type": "text_delta", "text": " " + link},
			})
		}
	case "response.function_call_arguments.delta":
		outIdx := intOf(data["output_index"])
		if b := t.blocks[outIdx]; b != nil && b.opened {
			b.argsSeen = true
			t.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": b.idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": strOf(data["delta"])},
			})
			return
		}
		// Delta before added (or added never comes): buffer for replay on
		// added/done — dropping it would corrupt the arguments JSON.
		t.pendArgs[pendingToolArgsKey(strOpt(data["item_id"]), outIdx)] += strOf(data["delta"])
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// reasoning_text.delta: OpenRouter-style dialect (reasoning streams as
		// content parts, not summary) — same payload shape, same handling.
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
		outIdx := intOf(data["output_index"])
		item := asMap(data["item"])
		if item != nil && (item["type"] == "tool_search_call" || item["type"] == "web_search_call") {
			t.ensureStart()
			b := t.blocks[outIdx]
			if b == nil {
				b = &rsBlock{kind: strOpt(item["type"]), idx: t.nextIdx, itemID: strOpt(item["id"])}
				t.nextIdx++
				t.blocks[outIdx] = b
			}
			if item["type"] == "tool_search_call" {
				t.hasToolUse = true
				t.emit("content_block_start", map[string]any{
					"type": "content_block_start", "index": b.idx,
					"content_block": map[string]any{
						"type": "tool_use", "id": hostedCallID(item), "name": "tool_search",
						"input": parseToolArgs(hostedCallArguments(item)),
					},
				})
				t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.idx})
			} else {
				for _, block := range responsesWebSearchToAnthropicBlocks(item) {
					idx := b.idx
					if block["type"] == "web_search_tool_result" {
						idx = t.nextIdx
						t.nextIdx++
					}
					t.emit("content_block_start", map[string]any{
						"type": "content_block_start", "index": idx, "content_block": block,
					})
					t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
				}
			}
			return
		}
		if t.blocks[outIdx] == nil {
			// Done-only tool call (the gateway skipped added AND deltas):
			// synthesize the whole block from the done frame's complete item —
			// dropping it loses the call entirely (opencodex does the same).
			if item := asMap(data["item"]); item != nil && item["type"] == "function_call" {
				t.ensureStart()
				t.hasToolUse = true
				b := &rsBlock{kind: "tool_use", idx: t.nextIdx, itemID: strOf(item["id"]), opened: true, argsSeen: true}
				t.nextIdx++
				t.blocks[outIdx] = b
				t.emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": b.idx,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
						"name":  strOpt(item["name"]),
						"input": map[string]any{},
					},
				})
				args := firstNonEmpty(strKey(item, "arguments"), takePendingArgs(t.pendArgs, strOpt(item["id"]), outIdx))
				if args != "" && args != "{}" {
					t.emit("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": b.idx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					})
				}
				t.closeBlock(outIdx)
			}
			return
		}
		if b := t.blocks[outIdx]; b != nil && b.kind == "tool_use" && b.opened && !b.argsSeen {
			// Done-only arguments fallback: some backends send NO arguments
			// deltas and carry the full arguments only in the done item —
			// without this the tool call would land with an empty input.
			if args := strKey(asMap(data["item"]), "arguments"); args != "" && args != "{}" {
				t.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": b.idx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
				})
			}
		}
		// A reasoning item's encrypted_content round-trips as a signature_delta
		// inside the thinking block; with no summary at all it is a
		// redacted_thinking block (data verbatim).
		if b := t.blocks[outIdx]; b != nil && b.kind == "thinking" {
			if item := asMap(data["item"]); item != nil {
				if enc, ok := item["encrypted_content"].(string); ok && enc != "" {
					if b.opened {
						t.emit("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": b.idx,
							"delta": map[string]any{"type": "signature_delta", "signature": enc},
						})
					} else {
						b.opened = true
						t.emit("content_block_start", map[string]any{
							"type":          "content_block_start",
							"index":         b.idx,
							"content_block": map[string]any{"type": "redacted_thinking", "data": enc},
						})
					}
				}
			}
		}
		t.closeBlock(outIdx)
	case "response.completed":
		if resp := asMap(data["response"]); resp != nil {
			// A completed EVENT can still carry a failure (status failed/
			// cancelled or a non-null error) — treat it as an error, not a
			// clean stop (aligned with the non-streaming fail-closed rule).
			if st := strOpt(resp["status"]); st == "failed" || st == "cancelled" || asMap(resp["error"]) != nil {
				emsg, etype := responsesErrorOf(data)
				t.ensureStart()
				t.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": etype, "message": emsg}})
				t.errored = true
				t.done = true
				return
			}
			t.readUsage(resp)
			if id := strOpt(resp["id"]); id != "" {
				t.id = id
			}
		}
		t.finish()
	case "response.incomplete":
		if reason := strKey(asMap(asMap(data["response"])["incomplete_details"]), "reason"); reason == "content_filter" {
			t.stopRsn = "refusal"
		} else {
			t.stopRsn = "max_tokens"
		}
		// incomplete carries usage too (a max_tokens truncation is exactly
		// what costs money) — read it like completed (cc-switch
		// streaming_responses.rs:1229-1231).
		if resp := asMap(data["response"]); resp != nil {
			t.readUsage(resp)
		}
		t.finish()
	case "response.failed", "error":
		t.ensureStart()
		emsg, etype := responsesErrorOf(data)
		t.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": etype, "message": emsg}})
		t.errored = true
		t.done = true
	default:
		// Known-but-unconsumed events (content_part.*, *.done frames, ...) are
		// ignored silently; anything else is surfaced once per process.
		if !knownResponsesEvent(event) {
			convertWarn("ignoring unknown responses SSE event (r→a): " + event)
		}
	}
}

// knownResponsesEvent reports whether a responses SSE event type is a known
// no-op for the r→{a,chat} transformers (frame markers they don't consume).
func knownResponsesEvent(event string) bool {
	switch event {
	case "response.content_part.added", "response.content_part.done",
		"response.output_text.done", "response.output_item.done",
		"response.function_call_arguments.done", "response.refusal.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return true
	}
	return false
}

// readUsage records response.completed/incomplete usage. responses
// input_tokens is INCLUSIVE of cache traffic; the direct
// cache_creation_input_tokens spelling wins over details.cache_write_tokens
// (same convention as the non-streaming converter).
func (t *responsesSSEToAnthropicSSE) readUsage(resp map[string]any) {
	u := asMap(resp["usage"])
	if u == nil {
		return
	}
	t.inTok = intOf(u["input_tokens"])
	t.outTok = intOf(u["output_tokens"])
	t.cachedTok = intOf(asMap(u["input_tokens_details"])["cached_tokens"])
	t.createTok = intOf(u["cache_creation_input_tokens"])
	if t.createTok == 0 {
		t.createTok = intOf(asMap(u["input_tokens_details"])["cache_write_tokens"])
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
		// responses input_tokens is inclusive of cached AND cache-write;
		// anthropic's excludes both (split out, clamped ≥0) — same convention
		// as the chat→a stream.
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
			"delta": map[string]any{"stop_reason": t.stopRsn},
			"usage": usage,
		})
		t.emit("message_stop", map[string]any{"type": "message_stop"})
	}
}

// ===========================================================================
// responses → openai-chat (codex/Responses backend → chat client)
// ===========================================================================

type responsesSSEToOpenAISSE struct {
	sc           *bufio.Scanner
	out          []byte
	model, id    string
	started      bool
	done         bool
	errored      bool
	toolIdx      map[int]int       // responses output_index → chat tool_calls index
	toolArgsSeen map[int]bool      // responses output_index → at least one arguments delta emitted
	pendArgs     map[string]string // early arguments deltas (before added/done)
	inTok        int
	outTok       int
	cachedTok    int // input_tokens_details.cached_tokens from response.completed
	reasoningTok int // output_tokens_details.reasoning_tokens from response.completed
	finishReason string
	hasToolUse   bool
}

func newResponsesToOpenAISSE(r io.Reader, model string) *responsesSSEToOpenAISSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &responsesSSEToOpenAISSE{sc: sc, model: model, id: "chatcmpl-conv", toolIdx: map[int]int{}, toolArgsSeen: map[int]bool{}, pendArgs: map[string]string{}}
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
			sseEmitData(&t.out, map[string]any{
				"id": t.id, "object": "chat.completion.chunk", "model": t.model,
				"error": map[string]any{
					"message": "upstream stream terminated before a terminal event",
					"type":    "api_error",
				},
			})
			t.errored = true
			t.done = true
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
			sseEmitData(&t.out, map[string]any{
				"id": t.id, "object": "chat.completion.chunk", "model": t.model,
				"error": map[string]any{
					"message": "responses stream ended before response.completed",
					"type":    "api_error",
				},
			})
			t.errored = true
			t.done = true
			continue
		}
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) != nil {
			continue
		}
		// Some providers (OpenRouter-style, e.g. aqp's /responses) omit SSE
		// event: lines entirely — fall back to the payload's own "type".
		if pendingEvent == "" {
			pendingEvent = strKey(data, "type")
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
			if id := strOpt(resp["id"]); id != "" {
				t.id = id
			}
			if m := strOpt(resp["model"]); m != "" {
				t.model = m
			}
		}
		t.ensureStart()
	case "response.output_text.delta", "response.refusal.delta":
		// refusal.delta carries refusal text — stream it as plain content
		// (finish_reason carries the refusal semantics).
		t.ensureStart()
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": strOf(data["delta"])}, "finish_reason": nil}},
		})
	case "response.output_text.annotation.added":
		annotations := responsesAnnotationsToChat([]any{data["annotation"]})
		if len(annotations) == 0 {
			return
		}
		t.ensureStart()
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"annotations": annotations}, "finish_reason": nil}},
		})
	case "response.output_item.added":
		item := asMap(data["item"])
		if item == nil {
			return
		}
		if item["type"] != "function_call" {
			// message/reasoning items are opened implicitly by their deltas;
			// anything else is surfaced once per process.
			if item["type"] != "message" && item["type"] != "reasoning" &&
				item["type"] != "tool_search_call" && item["type"] != "web_search_call" {
				convertWarn("ignoring unknown responses output item type: " + strOf(item["type"]))
			}
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
					"index": idx, "id": firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
					"type": "function", "function": map[string]any{"name": strOpt(item["name"]), "arguments": ""},
				}},
			}, "finish_reason": nil}},
		})
		// Replay arguments deltas that arrived BEFORE this added (buffered by
		// item_id/output_index) so the leading segment is not lost.
		if d := takePendingArgs(t.pendArgs, strOpt(item["id"]), outIdx); d != "" {
			t.toolArgsSeen[outIdx] = true
			sseEmitData(&t.out, map[string]any{
				"id": t.id, "object": "chat.completion.chunk", "model": t.model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{"index": idx, "function": map[string]any{"arguments": d}}},
				}, "finish_reason": nil}},
			})
		}
	case "response.function_call_arguments.delta":
		outIdx := intOf(data["output_index"])
		idx, ok := t.toolIdx[outIdx]
		if !ok {
			// Delta before added (or added never comes): buffer for replay on
			// added/done — dropping it would corrupt the arguments JSON.
			t.pendArgs[pendingToolArgsKey(strOpt(data["item_id"]), outIdx)] += strOf(data["delta"])
			return
		}
		t.toolArgsSeen[outIdx] = true
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"tool_calls": []map[string]any{{"index": idx, "function": map[string]any{"arguments": strOf(data["delta"])}}},
			}, "finish_reason": nil}},
		})
	case "response.output_item.done":
		// Done-only arguments fallback (same rationale as the r→anthropic
		// converter): backends that send no deltas carry full arguments here.
		outIdx := intOf(data["output_index"])
		item := asMap(data["item"])
		if item != nil && (item["type"] == "tool_search_call" || item["type"] == "web_search_call") {
			t.ensureStart()
			t.hasToolUse = true
			idx := t.toolIndex(outIdx)
			name := "tool_search"
			if item["type"] == "web_search_call" {
				name = "web_search"
			}
			sseEmitData(&t.out, map[string]any{
				"id": t.id, "object": "chat.completion.chunk", "model": t.model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": idx, "id": hostedCallID(item), "type": "function",
						"function": map[string]any{"name": name, "arguments": hostedCallArguments(item)},
					}},
				}, "finish_reason": nil}},
			})
			return
		}
		if item == nil || item["type"] != "function_call" {
			return
		}
		idx, ok := t.toolIdx[outIdx]
		if !ok {
			// Done-only tool call (the gateway skipped added AND deltas):
			// synthesize the complete call from the done frame's item — one
			// chunk carrying id + name + full arguments (opencodex outbound).
			t.ensureStart()
			t.hasToolUse = true
			idx = t.toolIndex(outIdx)
			args := firstNonEmpty(strKey(item, "arguments"), takePendingArgs(t.pendArgs, strOpt(item["id"]), outIdx), "{}")
			sseEmitData(&t.out, map[string]any{
				"id": t.id, "object": "chat.completion.chunk", "model": t.model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": idx, "id": firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
						"type": "function", "function": map[string]any{"name": strOpt(item["name"]), "arguments": args},
					}},
				}, "finish_reason": nil}},
			})
			return
		}
		if t.toolArgsSeen[outIdx] {
			return
		}
		if args := strKey(item, "arguments"); args != "" && args != "{}" {
			sseEmitData(&t.out, map[string]any{
				"id": t.id, "object": "chat.completion.chunk", "model": t.model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{"index": idx, "function": map[string]any{"arguments": args}}},
				}, "finish_reason": nil}},
			})
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// reasoning_text.delta: OpenRouter-style dialect (see the r→anthropic
		// converter for details).
		t.ensureStart()
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"reasoning_content": strOf(data["delta"])}, "finish_reason": nil}},
		})
	case "response.completed":
		if resp := asMap(data["response"]); resp != nil {
			// A completed EVENT can still carry a failure (status failed/
			// cancelled or a non-null error) — treat as an error, not a clean
			// finish (aligned with the non-streaming fail-closed rule).
			if st := strOpt(resp["status"]); st == "failed" || st == "cancelled" || asMap(resp["error"]) != nil {
				emsg, etype := responsesErrorOf(data)
				sseEmitData(&t.out, map[string]any{
					"id": t.id, "object": "chat.completion.chunk", "model": t.model,
					"error": map[string]any{"message": emsg, "type": etype},
				})
				t.errored = true
				t.done = true
				return
			}
			t.readUsage(resp)
		}
		t.finish()
	case "response.incomplete":
		if reason := strKey(asMap(asMap(data["response"])["incomplete_details"]), "reason"); reason == "content_filter" {
			t.finishReason = "content_filter"
		} else {
			t.finishReason = "length"
		}
		// incomplete carries usage too (cc-switch streaming_responses.rs
		// reads it uniformly for completed/incomplete).
		if resp := asMap(data["response"]); resp != nil {
			t.readUsage(resp)
		}
		t.finish()
	case "response.failed", "error":
		emsg, etype := responsesErrorOf(data)
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"error": map[string]any{"message": emsg, "type": etype},
		})
		t.errored = true
		t.done = true
	default:
		if !knownResponsesEvent(event) {
			convertWarn("ignoring unknown responses SSE event (r→chat): " + event)
		}
	}
}

// readUsage records response.completed/incomplete usage for the chat-side
// stream (inclusive input_tokens convention, cached/reasoning details).
func (t *responsesSSEToOpenAISSE) readUsage(resp map[string]any) {
	u := asMap(resp["usage"])
	if u == nil {
		return
	}
	t.inTok = intOf(u["input_tokens"])
	t.outTok = intOf(u["output_tokens"])
	t.cachedTok = intOf(asMap(u["input_tokens_details"])["cached_tokens"])
	t.reasoningTok = intOf(asMap(u["output_tokens_details"])["reasoning_tokens"])
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
		usage := map[string]any{"prompt_tokens": t.inTok, "completion_tokens": t.outTok, "total_tokens": t.inTok + t.outTok}
		if t.cachedTok > 0 {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": t.cachedTok}
		}
		if t.reasoningTok > 0 {
			usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": t.reasoningTok}
		}
		sseEmitData(&t.out, map[string]any{
			"id": t.id, "object": "chat.completion.chunk", "model": t.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": fr}},
			"usage":   usage,
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
				t.ensureCreated()
				t.emit("response.failed", map[string]any{
					"type": "response.failed",
					"response": map[string]any{
						"id": t.id, "object": "response", "status": "failed",
						"error": map[string]any{"code": "api_error", "message": "upstream stream terminated unexpectedly"},
					},
				})
				t.done = true
				continue
			}
			if t.stopRsn != "" {
				t.finish()
				continue
			}
			t.ensureCreated()
			t.emit("response.failed", map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id": t.id, "object": "response", "status": "failed",
					"error": map[string]any{"code": "api_error", "message": "upstream stream terminated before a terminal event"},
				},
			})
			t.done = true
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
		// Some providers (OpenRouter-style, e.g. aqp's /responses) omit SSE
		// event: lines entirely — fall back to the payload's own "type".
		if pendingEvent == "" {
			pendingEvent = strKey(data, "type")
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
			if original, namespace, ok := nsRestoreName(t.r2c.ns, name); ok {
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
					"part": map[string]any{"type": "output_text", "text": ""},
				})
			}
			d := strOf(delta["text"])
			b.acc += d
			t.emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "output_index": b.outIdx, "content_index": 0, "delta": d,
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
					"item": map[string]any{"type": "reasoning", "id": b.itemID, "summary": []any{}},
				})
			}
			d := strOf(delta["thinking"])
			b.acc += d
			t.emit("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "output_index": b.outIdx, "summary_index": 0, "delta": d,
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
			t.emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": b.outIdx, "content_index": 0, "text": b.acc})
			part := map[string]any{"type": "output_text", "text": b.acc}
			if len(b.annotations) > 0 {
				part["annotations"] = b.annotations
			}
			t.emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "output_index": b.outIdx, "content_index": 0, "part": part})
			doneItem = map[string]any{
				"type": "message", "id": b.itemID, "status": "completed", "role": "assistant",
				"content": []map[string]any{part},
			}
		case "thinking":
			doneItem = map[string]any{"type": "reasoning", "id": b.itemID, "status": "completed", "summary": []map[string]any{{"type": "summary_text", "text": b.acc}}}
			if b.signature != "" {
				doneItem["encrypted_content"] = b.signature
			}
			t.emit("response.reasoning_summary_part.done", map[string]any{"type": "response.reasoning_summary_part.done", "output_index": b.outIdx, "summary_index": 0, "item": doneItem})
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
			if original, namespace, ok := nsRestoreName(t.r2c.ns, b.name); ok {
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
		t.finish()
	case "ping":
		// keep-alive, no responses equivalent
	case "error":
		emsg, etype := "", "api_error"
		if e := asMap(data["error"]); e != nil {
			emsg = strOf(e["message"])
			etype = firstNonEmpty(strOpt(e["type"]), "api_error")
		}
		t.emit("response.failed", map[string]any{
			"type":     "response.failed",
			"response": map[string]any{"id": t.id, "status": "failed", "error": map[string]any{"code": etype, "message": emsg}},
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
		"usage": usage,
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
	textOpened      bool
	textAcc         string // accumulated text (for the *.done text field)
	textAnnotations []map[string]any
	rsOut           int // output_index of the open reasoning item (-1 none)
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

// openReasoning opens the single reasoning item (chat reasoning_content
// streams as one reasoning-summary item, like openText).
func (t *openaiSSEToResponsesSSE) openReasoning() {
	if t.rsOpened {
		return
	}
	t.rsOpened = true
	t.rsOut = t.nextOutIdx
	t.nextOutIdx++
	id := "rs_item_" + itoa(t.rsOut)
	t.ensureCreated()
	t.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": t.rsOut,
		"item": map[string]any{"type": "reasoning", "id": id, "status": "in_progress", "summary": []any{}},
	})
	t.emit("response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "output_index": t.rsOut, "summary_index": 0,
		"item": map[string]any{"type": "reasoning", "id": id, "summary": []any{}},
	})
}

func (t *openaiSSEToResponsesSSE) Read(p []byte) (int, error) {
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
				convertWarn("openai SSE scanner error: " + err.Error())
				t.ensureCreated()
				t.emit("response.failed", map[string]any{
					"type": "response.failed",
					"response": map[string]any{
						"id": t.id, "object": "response", "status": "failed",
						"error": map[string]any{"code": "api_error", "message": "upstream stream terminated unexpectedly"},
					},
				})
				t.done = true
				continue
			}
			if t.finishRsn != "" {
				t.finish()
				continue
			}
			t.ensureCreated()
			t.emit("response.failed", map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id": t.id, "object": "response", "status": "failed",
					"error": map[string]any{"code": "api_error", "message": "upstream stream terminated before a terminal event"},
				},
			})
			t.done = true
			continue
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" {
			pendingEvent = ""
			continue
		}
		// Chat streams are usually data-only, but some gateways DO send event:
		// lines — an explicit `event: error` marks an error frame even when the
		// payload has no top-level error key (cc-switch streaming_codex_chat).
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
	// OpenAI error chunk (data: {"error":{...}}) or an explicit `event: error`
	// frame → response.failed. Never silently turn a mid-stream upstream
	// error into a clean response.completed.
	if asMap(data["error"]) != nil || event == "error" {
		emsg, etype := chatSSEErrorOf(data)
		t.ensureCreated()
		t.emit("response.failed", map[string]any{
			"type":     "response.failed",
			"response": map[string]any{"id": t.id, "object": "response", "status": "failed", "error": map[string]any{"code": etype, "message": emsg}},
		})
		t.done = true
		return
	}
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
	for _, annotation := range chatAnnotationsToResponses(delta["annotations"]) {
		t.ensureCreated()
		t.openText()
		index := len(t.textAnnotations)
		t.textAnnotations = append(t.textAnnotations, annotation)
		t.emit("response.output_text.annotation.added", map[string]any{
			"type": "response.output_text.annotation.added", "output_index": t.textOut,
			"content_index": 0, "item_id": "msg_item_" + itoa(t.textOut),
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
			"type": "response.reasoning_summary_text.delta", "output_index": t.rsOut, "summary_index": 0, "delta": rc,
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
		if orig, ns, ok := nsRestoreName(t.r2c.ns, name); ok {
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
		"type": "response.output_text.delta", "output_index": t.textOut, "content_index": 0, "delta": d,
	})
}

// pushReasoningDelta emits one reasoning delta (opening the reasoning item
// lazily).
func (t *openaiSSEToResponsesSSE) pushReasoningDelta(d string) {
	t.ensureCreated()
	t.openReasoning()
	t.rsAcc += d
	t.emit("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "output_index": t.rsOut, "summary_index": 0, "delta": d,
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
		"usage": usage,
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
	if t.textOpened {
		open = append(open, openItem{t.textOut, "message", -1})
	}
	for chatIdx, outIdx := range t.toolOut {
		open = append(open, openItem{outIdx, "function_call", chatIdx})
	}
	sort.Slice(open, func(i, j int) bool { return open[i].outIdx < open[j].outIdx })
	for _, it := range open {
		switch it.kind {
		case "message":
			id := "msg_item_" + itoa(it.outIdx)
			t.emit("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "output_index": it.outIdx, "content_index": 0, "text": t.textAcc,
			})
			part := map[string]any{"type": "output_text", "text": t.textAcc}
			if len(t.textAnnotations) > 0 {
				part["annotations"] = t.textAnnotations
			}
			t.emit("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "output_index": it.outIdx, "content_index": 0,
				"part": part,
			})
			doneItem := map[string]any{
				"type": "message", "id": id, "status": "completed", "role": "assistant",
				"content": []map[string]any{part},
			}
			t.doneItems = append(t.doneItems, doneItem)
			t.emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": it.outIdx,
				"item": doneItem,
			})
		case "reasoning":
			id := "rs_item_" + itoa(it.outIdx)
			doneItem := map[string]any{"type": "reasoning", "id": id, "status": "completed",
				"summary": []map[string]any{{"type": "summary_text", "text": t.rsAcc}}}
			t.emit("response.reasoning_summary_part.done", map[string]any{
				"type": "response.reasoning_summary_part.done", "output_index": it.outIdx, "summary_index": 0,
				"item": doneItem,
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
			if orig, ns, ok := nsRestoreName(t.r2c.ns, t.toolNames[it.toolIdx]); ok {
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
