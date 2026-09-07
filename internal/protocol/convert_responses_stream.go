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
	pendingEvent := ""
	pendData := ""    // folded data lines of the SSE frame in progress
	pendOpen := false // a data: line opened the current frame (an empty one folds to "")
	var pendEvents []string
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		line := ""
		if t.sc.Scan() {
			line = strings.TrimSpace(t.sc.Text())
		} else if pendOpen {
			// Scanner exhausted with a frame in progress: synthesize the
			// dispatch blank line (the SSE spec delivers a trailing frame
			// without its final blank line). The next iteration takes the
			// normal exhaustion path with no frame open.
			line = ""
		} else {
			if err := t.sc.Err(); err != nil {
				convertWarn("responses SSE scanner error: " + err.Error())
			}
			t.ensureStart()
			t.closeLeftoverBlocks()
			t.emit("error", map[string]any{"type": "error", "error": map[string]any{
				"type": "api_error", "message": "upstream stream terminated before a terminal event",
			}})
			t.errored = true
			t.done = true
			continue
		}
		if strings.HasPrefix(line, "data:") {
			pendData = appendSSEData(pendData, pendOpen, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			pendEvents = append(pendEvents, pendingEvent)
			pendOpen = true
			continue
		}
		// Classify the frame-terminating line first — it may open the NEXT
		// frame's event type; the closing frame keeps its own.
		frameEvent := pendingEvent
		if line == "" {
			pendingEvent = ""
		} else if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if line != "" || !pendOpen {
			// Only a blank line dispatches a frame (SSE spec): event:/retry:/
			// comment lines belong to the frame in progress even when they
			// trail its data lines — dispatching on them would classify the
			// frame under the previous event and leak the real one forward.
			continue
		}
		payload := pendData
		pendData, pendOpen = "", false
		dataEvents := pendEvents
		pendEvents = nil
		if payload == "[DONE]" {
			t.ensureStart()
			t.closeLeftoverBlocks()
			t.emit("error", map[string]any{"type": "error", "error": map[string]any{
				"type": "api_error", "message": "responses stream ended before response.completed",
			}})
			t.errored = true
			t.done = true
			continue
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
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
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
	pendingEvent := ""
	pendData := ""    // folded data lines of the SSE frame in progress
	pendOpen := false // a data: line opened the current frame (an empty one folds to "")
	var pendEvents []string
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		line := ""
		if t.sc.Scan() {
			line = strings.TrimSpace(t.sc.Text())
		} else if pendOpen {
			// Scanner exhausted with a frame in progress: synthesize the
			// dispatch blank line (the SSE spec delivers a trailing frame
			// without its final blank line). The next iteration takes the
			// normal exhaustion path with no frame open.
			line = ""
		} else {
			if err := t.sc.Err(); err != nil {
				convertWarn("responses SSE scanner error: " + err.Error())
			}
			t.emitChunk(map[string]any{
				"error": map[string]any{
					"message": "upstream stream terminated before a terminal event",
					"type":    "api_error",
				},
			})
			t.errored = true
			t.done = true
			continue
		}
		if strings.HasPrefix(line, "data:") {
			pendData = appendSSEData(pendData, pendOpen, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			pendEvents = append(pendEvents, pendingEvent)
			pendOpen = true
			continue
		}
		// Classify the frame-terminating line first — it may open the NEXT
		// frame's event type; the closing frame keeps its own.
		frameEvent := pendingEvent
		if line == "" {
			pendingEvent = ""
		} else if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if line != "" || !pendOpen {
			// Only a blank line dispatches a frame (SSE spec): event:/retry:/
			// comment lines belong to the frame in progress even when they
			// trail its data lines — dispatching on them would classify the
			// frame under the previous event and leak the real one forward.
			continue
		}
		payload := pendData
		pendData, pendOpen = "", false
		dataEvents := pendEvents
		pendEvents = nil
		if payload == "[DONE]" {
			t.emitChunk(map[string]any{
				"error": map[string]any{
					"message": "responses stream ended before response.completed",
					"type":    "api_error",
				},
			})
			t.errored = true
			t.done = true
			continue
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
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
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
	pendData := ""    // folded data lines of the SSE frame in progress
	pendOpen := false // a data: line opened the current frame (an empty one folds to "")
	var pendEvents []string
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		line := ""
		if t.sc.Scan() {
			line = strings.TrimSpace(t.sc.Text())
		} else if pendOpen {
			// Scanner exhausted with a frame in progress: synthesize the
			// dispatch blank line (the SSE spec delivers a trailing frame
			// without its final blank line). The next iteration takes the
			// normal exhaustion path with no frame open.
			line = ""
		} else {
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
		if strings.HasPrefix(line, "data:") {
			pendData = appendSSEData(pendData, pendOpen, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			pendEvents = append(pendEvents, pendingEvent)
			pendOpen = true
			continue
		}
		// Classify the frame-terminating line first — it may open the NEXT
		// frame's event type; the closing frame keeps its own.
		frameEvent := pendingEvent
		if line == "" {
			pendingEvent = ""
		} else if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if line != "" || !pendOpen {
			// Only a blank line dispatches a frame (SSE spec): event:/retry:/
			// comment lines belong to the frame in progress even when they
			// trail its data lines — dispatching on them would classify the
			// frame under the previous event and leak the real one forward.
			continue
		}
		payload := pendData
		pendData, pendOpen = "", false
		dataEvents := pendEvents
		pendEvents = nil
		if payload == "[DONE]" {
			t.finish()
			continue
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
	pendingEvent := ""
	pendData := ""    // folded data lines of the SSE frame in progress
	pendOpen := false // a data: line opened the current frame (an empty one folds to "")
	var pendEvents []string
	for len(t.out) == 0 {
		if t.done {
			if len(t.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		line := ""
		if t.sc.Scan() {
			line = strings.TrimSpace(t.sc.Text())
		} else if pendOpen {
			// Scanner exhausted with a frame in progress: synthesize the
			// dispatch blank line (the SSE spec delivers a trailing frame
			// without its final blank line). The next iteration takes the
			// normal exhaustion path with no frame open.
			line = ""
		} else {
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
		if strings.HasPrefix(line, "data:") {
			pendData = appendSSEData(pendData, pendOpen, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			pendEvents = append(pendEvents, pendingEvent)
			pendOpen = true
			continue
		}
		// Classify the frame-terminating line first — it may open the NEXT
		// frame's event type; the closing frame keeps its own.
		frameEvent := pendingEvent
		if line == "" {
			pendingEvent = ""
		} else if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if line != "" || !pendOpen {
			// Only a blank line dispatches a frame (SSE spec): event:/retry:/
			// comment lines belong to the frame in progress even when they
			// trail its data lines — dispatching on them would classify the
			// frame under the previous event and leak the real one forward.
			continue
		}
		payload := pendData
		pendData, pendOpen = "", false
		dataEvents := pendEvents
		pendEvents = nil
		if payload == "[DONE]" {
			t.finish()
			continue
		}
		for _, parsed := range parseFoldedSSEFrames[map[string]any](payload) {
			event := foldedSSEFrameEvent(frameEvent, dataEvents, parsed.line)
			t.handle(event, parsed.value)
			if t.done {
				break
			}
		}
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
