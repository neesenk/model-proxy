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
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

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
		return
	}
	// Standalone error event (Responses streaming spec): the fields are
	// top-level — {type:"error", code, message, param, sequence_number}.
	if strKey(data, "type") == "error" {
		emsg = strOf(data["message"])
		etype = firstNonEmpty(strOpt(data["code"]), "api_error")
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
	itemsSeen   bool              // any output item frame (added/done/delta) was processed
	idMap       map[string]string // raw tool_call id → sanitized tool_use id (per-stream pairing memo)
	usedNorm    map[string]bool   // sanitized ids already taken (collision suffixing)
}

func newResponsesToAnthropicSSE(r io.Reader, model string) *responsesSSEToAnthropicSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &responsesSSEToAnthropicSSE{
		sc: sc, model: model, id: "msg_conv",
		blocks: map[int]*rsBlock{}, curTextOut: -1, curThinkOut: -1,
		pendArgs: map[string]string{},
		idMap:    map[string]string{}, usedNorm: map[string]bool{},
	}
}

// normToolID sanitizes a Responses call_id into the anthropic tool_use id
// charset (^[a-zA-Z0-9_-]+$) with a per-stream memo: the SAME raw id always
// maps to the SAME sanitized id across start/delta/paired frames, and two
// different raw ids that sanitize to the same string get _2/_3 suffixes (same
// semantics as the request-side normID in convert.go).
func (t *responsesSSEToAnthropicSSE) normToolID(id string) string {
	if n, ok := t.idMap[id]; ok {
		return n
	}
	n := sanitizeToolUseID(id)
	for i := 2; t.usedNorm[n]; i++ {
		n = fmt.Sprintf("%s_%d", sanitizeToolUseID(id), i)
	}
	t.usedNorm[n] = true
	t.idMap[id] = n
	return n
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

// closeLeftoverBlocks closes every still-open block in ascending ANTHROPIC
// block index order (the index clients saw in content_block_start). Map
// iteration is randomized — a strict client rejects stop(N) before stop(N-1).
func (t *responsesSSEToAnthropicSSE) closeLeftoverBlocks() {
	type pending struct{ outIdx, blockIdx int }
	left := make([]pending, 0, len(t.blocks))
	for outIdx, block := range t.blocks {
		if block.opened {
			left = append(left, pending{outIdx: outIdx, blockIdx: block.idx})
		}
	}
	sort.Slice(left, func(i, j int) bool { return left[i].blockIdx < left[j].blockIdx })
	for _, p := range left {
		t.closeBlock(p.outIdx)
	}
}

// emitDoneOnlyFunctionCall synthesizes a complete tool_use block (start →
// args delta → stop) from a complete function_call item — used when the
// gateway skipped added/deltas (done-only) or sent no item frames at all
// (completed-only).
func (t *responsesSSEToAnthropicSSE) emitDoneOnlyFunctionCall(item map[string]any, pendingArgs string) {
	t.ensureStart()
	t.hasToolUse = true
	idx := t.nextIdx
	t.nextIdx++
	t.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    t.normToolID(firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]))),
			"name":  strOpt(item["name"]),
			"input": map[string]any{},
		},
	})
	args := firstNonEmpty(strKey(item, "arguments"), pendingArgs)
	if args != "" && args != "{}" {
		t.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
	t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

// emitCompletedMessageItem synthesizes text blocks from a COMPLETE message
// item (done-only item, or response.completed.output when the stream carried
// no item frames): output_text parts become text blocks; refusal parts become
// text blocks too (anthropic has no refusal block — stop_reason carries the
// semantics, aligned with the non-streaming converter).
func (t *responsesSSEToAnthropicSSE) emitCompletedMessageItem(item map[string]any) {
	t.ensureStart()
	for _, p := range anySlice(item["content"]) {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		var text string
		switch strOf(pm["type"]) {
		case "output_text", "text", "input_text":
			text = strOf(pm["text"])
		case "refusal":
			text = strOf(pm["refusal"])
		default:
			convertWarn("dropping responses content part in r→a stream: " + strOf(pm["type"]))
			continue
		}
		idx := t.nextIdx
		t.nextIdx++
		t.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		if text != "" {
			t.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
		}
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	}
}

// materializeCompletedOutput synthesizes content from
// response.completed.output when the stream carried NO output item frames at
// all (a completed-only gateway): message items become text blocks,
// function_call items become tool_use blocks (same shapes as the done-only
// paths).
func (t *responsesSSEToAnthropicSSE) materializeCompletedOutput(output any) {
	for _, it := range anySlice(output) {
		item := asMap(it)
		if item == nil {
			continue
		}
		switch strOf(item["type"]) {
		case "message":
			t.emitCompletedMessageItem(item)
		case "function_call":
			t.emitDoneOnlyFunctionCall(item, "")
		}
	}
}

func (t *responsesSSEToAnthropicSSE) Read(p []byte) (int, error) {
	if pumpSSEFrames(t, t.sc, nil, true) {
		return 0, io.EOF
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *responsesSSEToAnthropicSSE) hasOutput() bool { return len(t.out) > 0 }
func (t *responsesSSEToAnthropicSSE) isDone() bool    { return t.done }

func (t *responsesSSEToAnthropicSSE) drainDone() (eof bool) {
	return len(t.out) == 0
}

// emitPrematureEnd is shared by scanError and streamEnd: a responses stream
// that ends without response.completed (or whose scanner failed) is an error
// event, never a clean finish.
func (t *responsesSSEToAnthropicSSE) emitPrematureEnd() {
	t.ensureStart()
	t.closeLeftoverBlocks()
	t.emit("error", map[string]any{"type": "error", "error": map[string]any{
		"type": "api_error", "message": "upstream stream terminated before a terminal event",
	}})
	t.errored = true
	t.done = true
}

func (t *responsesSSEToAnthropicSSE) scanError(err error) {
	convertWarn("responses SSE scanner error: " + err.Error())
	t.emitPrematureEnd()
}

func (t *responsesSSEToAnthropicSSE) streamEnd() {
	t.emitPrematureEnd()
}

// dispatch ============================================================

func (t *responsesSSEToAnthropicSSE) dispatch(frameEvent string, dataEvents []string, payload string) {
	if payload == "[DONE]" {
		t.ensureStart()
		t.closeLeftoverBlocks()
		t.emit("error", map[string]any{"type": "error", "error": map[string]any{
			"type": "api_error", "message": "responses stream ended before response.completed",
		}})
		t.errored = true
		t.done = true
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

func (t *responsesSSEToAnthropicSSE) handle(event string, data map[string]any) {
	// Any output-item frame (added/done/delta) marks the stream as carrying
	// items — a completed-only fallback must not re-synthesize them.
	if strings.HasPrefix(event, "response.output_item.") || strings.HasPrefix(event, "response.output_text.") ||
		strings.HasPrefix(event, "response.function_call") || strings.HasPrefix(event, "response.refusal.") ||
		strings.HasPrefix(event, "response.reasoning") {
		t.itemsSeen = true
	}
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
					"id":    t.normToolID(firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]))),
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
						"type": "tool_use", "id": t.normToolID(hostedCallID(item)), "name": "tool_search",
						"input": parseToolArgs(hostedCallArguments(item), nil),
					},
				})
				t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.idx})
			} else {
				for _, block := range responsesWebSearchToAnthropicBlocks(item, nil) {
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
			// Done-only item (the gateway skipped added AND deltas): synthesize
			// the whole content from the done frame's complete item — dropping
			// it loses the content entirely (opencodex does the same).
			if item := asMap(data["item"]); item != nil {
				switch item["type"] {
				case "function_call":
					t.emitDoneOnlyFunctionCall(item, takePendingArgs(t.pendArgs, strOpt(item["id"]), outIdx))
				case "message":
					t.emitCompletedMessageItem(item)
				}
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
			// Completed-only gateway: no item frames at all, full content in
			// response.output — synthesize it instead of dropping the answer.
			if !t.itemsSeen {
				t.materializeCompletedOutput(resp["output"])
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
		"response.reasoning_summary_text.done",
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
	t.closeLeftoverBlocks()
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
	created      int64 // chat.completion.chunk created (unix seconds, stable for the whole stream)
	started      bool
	done         bool
	errored      bool
	toolIdx      map[int]int       // responses output_index → chat tool_calls index
	toolArgsSeen map[int]bool      // responses output_index → at least one arguments delta emitted
	pendArgs     map[string]string // early arguments deltas (before added/done)
	contentSeen  map[int]bool      // responses output_index → a text/annotation chunk was emitted
	itemsSeen    bool              // any output item frame (added/done/delta) was processed
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
	return &responsesSSEToOpenAISSE{sc: sc, model: model, id: "chatcmpl-conv", created: time.Now().Unix(),
		toolIdx: map[int]int{}, toolArgsSeen: map[int]bool{}, pendArgs: map[string]string{}, contentSeen: map[int]bool{}}
}

// emitChunk appends one chat.completion.chunk frame, filling the required
// envelope fields (id/object/created/model — the chunk schema mandates all
// four; created is one stable unix-seconds timestamp for the whole stream).
func (t *responsesSSEToOpenAISSE) emitChunk(payload map[string]any) {
	payload["id"] = t.id
	payload["object"] = "chat.completion.chunk"
	payload["created"] = t.created
	payload["model"] = t.model
	sseEmitData(&t.out, payload)
}

func (t *responsesSSEToOpenAISSE) ensureStart() {
	if t.started {
		return
	}
	t.started = true
	t.emitChunk(map[string]any{
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
	if pumpSSEFrames(t, t.sc, nil, true) {
		return 0, io.EOF
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *responsesSSEToOpenAISSE) hasOutput() bool { return len(t.out) > 0 }
func (t *responsesSSEToOpenAISSE) isDone() bool    { return t.done }

func (t *responsesSSEToOpenAISSE) drainDone() (eof bool) {
	return len(t.out) == 0
}

// emitPrematureEnd is shared by scanError and streamEnd: a responses stream
// that ends without response.completed (or whose scanner failed) is an error
// chunk, never a clean finish.
func (t *responsesSSEToOpenAISSE) emitPrematureEnd() {
	t.emitChunk(map[string]any{
		"error": map[string]any{
			"message": "upstream stream terminated before a terminal event",
			"type":    "api_error",
		},
	})
	t.errored = true
	t.done = true
}

func (t *responsesSSEToOpenAISSE) scanError(err error) {
	convertWarn("responses SSE scanner error: " + err.Error())
	t.emitPrematureEnd()
}

func (t *responsesSSEToOpenAISSE) streamEnd() {
	t.emitPrematureEnd()
}

// dispatch ============================================================

func (t *responsesSSEToOpenAISSE) dispatch(frameEvent string, dataEvents []string, payload string) {
	if payload == "[DONE]" {
		t.emitChunk(map[string]any{
			"error": map[string]any{
				"message": "responses stream ended before response.completed",
				"type":    "api_error",
			},
		})
		t.errored = true
		t.done = true
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

func (t *responsesSSEToOpenAISSE) handle(event string, data map[string]any) {
	// Any output-item frame (added/done/delta) marks the stream as carrying
	// items — a completed-only fallback must not re-synthesize them.
	if strings.HasPrefix(event, "response.output_item.") || strings.HasPrefix(event, "response.output_text.") ||
		strings.HasPrefix(event, "response.function_call") || strings.HasPrefix(event, "response.refusal.") ||
		strings.HasPrefix(event, "response.reasoning") {
		t.itemsSeen = true
	}
	switch event {
	case "response.created", "response.in_progress":
		if resp := asMap(data["response"]); resp != nil {
			if id := strOpt(resp["id"]); id != "" {
				t.id = id
			}
			if m := strOpt(resp["model"]); m != "" {
				t.model = m
			}
			if ca := intOf(resp["created_at"]); ca > 0 {
				t.created = int64(ca)
			}
		}
		t.ensureStart()
	case "response.output_text.delta", "response.refusal.delta":
		// refusal.delta carries refusal text — stream it as plain content
		// (finish_reason carries the refusal semantics).
		t.ensureStart()
		t.contentSeen[intOf(data["output_index"])] = true
		t.emitChunk(map[string]any{
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": strOf(data["delta"])}, "finish_reason": nil}},
		})
	case "response.output_text.annotation.added":
		annotations := responsesAnnotationsToChat([]any{data["annotation"]})
		if len(annotations) == 0 {
			return
		}
		t.ensureStart()
		t.contentSeen[intOf(data["output_index"])] = true
		t.emitChunk(map[string]any{
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
		t.emitChunk(map[string]any{
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
			t.emitChunk(map[string]any{
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
		t.emitChunk(map[string]any{
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
			t.emitChunk(map[string]any{
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": idx, "id": hostedCallID(item), "type": "function",
						"function": map[string]any{"name": name, "arguments": hostedCallArguments(item)},
					}},
				}, "finish_reason": nil}},
			})
			return
		}
		if item == nil {
			return
		}
		if item["type"] == "message" {
			// Done-only message (the gateway skipped added AND deltas): the
			// full text/refusal parts live in the done item — synthesize the
			// content chunks instead of dropping the answer.
			if !t.contentSeen[outIdx] {
				t.emitCompletedMessageContent(item)
			}
			return
		}
		if item["type"] != "function_call" {
			return
		}
		idx, ok := t.toolIdx[outIdx]
		if !ok {
			// Done-only tool call (the gateway skipped added AND deltas):
			// synthesize the complete call from the done frame's item — one
			// chunk carrying id + name + full arguments (opencodex outbound).
			t.emitDoneOnlyToolCall(item, takePendingArgs(t.pendArgs, strOpt(item["id"]), outIdx), outIdx)
			return
		}
		if t.toolArgsSeen[outIdx] {
			return
		}
		if args := strKey(item, "arguments"); args != "" && args != "{}" {
			t.emitChunk(map[string]any{
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{"index": idx, "function": map[string]any{"arguments": args}}},
				}, "finish_reason": nil}},
			})
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// reasoning_text.delta: OpenRouter-style dialect (see the r→anthropic
		// converter for details).
		t.ensureStart()
		t.emitChunk(map[string]any{
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"reasoning_content": strOf(data["delta"])}, "finish_reason": nil}},
		})
	case "response.completed":
		if resp := asMap(data["response"]); resp != nil {
			// A completed EVENT can still carry a failure (status failed/
			// cancelled or a non-null error) — treat as an error, not a clean
			// finish (aligned with the non-streaming fail-closed rule).
			if st := strOpt(resp["status"]); st == "failed" || st == "cancelled" || asMap(resp["error"]) != nil {
				emsg, etype := responsesErrorOf(data)
				t.emitChunk(map[string]any{
					"error": map[string]any{"message": emsg, "type": etype},
				})
				t.errored = true
				t.done = true
				return
			}
			t.readUsage(resp)
			if ca := intOf(resp["created_at"]); ca > 0 {
				t.created = int64(ca)
			}
			// Completed-only gateway: no item frames at all, full content in
			// response.output — synthesize it instead of dropping the answer.
			if !t.itemsSeen {
				t.materializeCompletedOutput(resp["output"])
			}
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
		t.emitChunk(map[string]any{
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

// emitDoneOnlyToolCall synthesizes one complete tool_calls chunk (id + name +
// full arguments) from a complete function_call item — done-only gateways
// (skipped added AND deltas) and the completed-only output fallback.
func (t *responsesSSEToOpenAISSE) emitDoneOnlyToolCall(item map[string]any, pendingArgs string, outIdx int) {
	t.ensureStart()
	t.hasToolUse = true
	idx := t.toolIndex(outIdx)
	args := firstNonEmpty(strKey(item, "arguments"), pendingArgs, "{}")
	t.emitChunk(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{
			"tool_calls": []map[string]any{{
				"index": idx, "id": firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"])),
				"type": "function", "function": map[string]any{"name": strOpt(item["name"]), "arguments": args},
			}},
		}, "finish_reason": nil}},
	})
}

// emitCompletedMessageContent synthesizes content chunks from a COMPLETE
// message item (done-only item, or response.completed.output when the stream
// carried no item frames): output_text parts stream as content; refusal parts
// stream as content too (finish_reason carries the refusal semantics, aligned
// with response.refusal.delta handling).
func (t *responsesSSEToOpenAISSE) emitCompletedMessageContent(item map[string]any) {
	t.ensureStart()
	for _, p := range anySlice(item["content"]) {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		var text string
		switch strOf(pm["type"]) {
		case "output_text", "text", "input_text":
			text = strOf(pm["text"])
		case "refusal":
			text = strOf(pm["refusal"])
		default:
			convertWarn("dropping responses content part in r→chat stream: " + strOf(pm["type"]))
			continue
		}
		if text == "" {
			continue
		}
		t.emitChunk(map[string]any{
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
		})
	}
}

// materializeCompletedOutput synthesizes content from
// response.completed.output when the stream carried NO output item frames at
// all (a completed-only gateway): message items become content chunks,
// function_call items become complete tool_calls chunks (same shapes as the
// done-only paths).
func (t *responsesSSEToOpenAISSE) materializeCompletedOutput(output any) {
	for i, it := range anySlice(output) {
		item := asMap(it)
		if item == nil {
			continue
		}
		switch strOf(item["type"]) {
		case "message":
			t.emitCompletedMessageContent(item)
		case "function_call":
			// No item frames were seen, so the output slice position IS the
			// output_index — distinct per call (no toolIndex collision).
			t.emitDoneOnlyToolCall(item, "", i)
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
		// include_usage contract: the finish chunk carries NO usage; usage
		// arrives in a separate final chunk with an empty choices array,
		// immediately before data: [DONE].
		t.emitChunk(map[string]any{
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": fr}},
		})
		t.emitChunk(map[string]any{
			"choices": []any{},
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
