package main

// convert_responses_stream_test.go — tests for the *ToResponsesSSE streaming
// synthesizers (convert_responses_stream.go): anthropic→responses and
// chat→responses, the direction that synthesizes response.* events. Assertions
// are on the EVENT SEQUENCE (drainSSE helpers), item id consistency, and
// added/done pairing — not substring matching.

import (
	"io"
	"strings"
	"testing"
)

// anthropicTextStream is a plain text anthropic message stream (a→r input).
const anthropicTextStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":11}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":4}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// A1: anthropic→responses text stream. message_start→response.created;
// text block→output_item.added+output_text.delta+output_item.done;
// message_stop→response.completed (with usage). Item id is msg_item_* and
// added/done are strictly paired.
func TestResponsesSynthesis_AnthropicText(t *testing.T) {
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(anthropicTextStream), "claude-x"))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)

	// response.created is synthesized at message_start with the placeholder id;
	// the real message id lands on response.completed.
	created := asMap(sseDataMap(t, events[0])["response"])
	if created["status"] != "in_progress" {
		t.Errorf("response.created status = %v", created["status"])
	}
	// The message item uses the synthesized msg_item_<idx> id.
	added := asMap(sseDataMap(t, events[1])["item"])
	if added["type"] != "message" || added["id"] != "msg_item_0" {
		t.Errorf("added item = %v, want message msg_item_0", added)
	}
	// Deltas accumulate into the done frame's text.
	if d := strOf(sseDataMap(t, events[3])["delta"]); d != "hel" {
		t.Errorf("delta[0] = %q", d)
	}
	if txt := strOf(sseDataMap(t, events[5])["text"]); txt != "hello" {
		t.Errorf("output_text.done text = %q, want hello (accumulated deltas)", txt)
	}
	// response.completed carries the usage from message_delta.
	completed := asMap(sseDataMap(t, events[8])["response"])
	if completed["id"] != "msg_1" || completed["status"] != "completed" {
		t.Errorf("completed response = %v", completed)
	}
	u := asMap(completed["usage"])
	if u["input_tokens"] != float64(11) || u["output_tokens"] != float64(4) || u["total_tokens"] != float64(15) {
		t.Errorf("completed usage = %v, want 11/4/15", u)
	}
}

// anthropicToolStream is a tool_use anthropic message stream (a→r input).
const anthropicToolStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"search"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// A2: anthropic→responses tool stream. tool_use→function_call item (fc_item_*
// id, call_id from the block id); input_json_delta→function_call_arguments.delta;
// the done frame's arguments reconcile with the concatenated deltas.
func TestResponsesSynthesis_AnthropicToolCall(t *testing.T) {
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(anthropicToolStream), "claude-x"))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)

	added := asMap(sseDataMap(t, events[1])["item"])
	if added["type"] != "function_call" || added["id"] != "fc_item_0" {
		t.Errorf("added item = %v, want function_call fc_item_0", added)
	}
	if added["call_id"] != "toolu_1" || added["name"] != "search" {
		t.Errorf("function_call call_id/name = %v/%v", added["call_id"], added["name"])
	}
	// Argument deltas carry the item id and stream verbatim.
	for i, want := range []string{`{"q":`, `"x"}`} {
		d := sseDataMap(t, events[2+i])
		if strOf(d["delta"]) != want || strOf(d["item_id"]) != "fc_item_0" {
			t.Errorf("args delta %d = %v (item_id %v), want delta %q", i, d["delta"], d["item_id"], want)
		}
	}
	// Done frame arguments = concatenation of the deltas (reconciliation).
	done := sseDataMap(t, events[4])
	if strOf(done["arguments"]) != `{"q":"x"}` {
		t.Errorf("function_call_arguments.done arguments = %q, want %q", done["arguments"], `{"q":"x"}`)
	}
	// stop_reason tool_use maps to status completed.
	completed := asMap(sseDataMap(t, events[6])["response"])
	if completed["status"] != "completed" {
		t.Errorf("completed status = %v", completed["status"])
	}
}

// A3: chat→responses text stream. First content chunk→response.created +
// message item; delta.content→output_text.delta; finish→response.completed.
// Responses streams have no [DONE] marker (that is chat-side).
func TestResponsesSynthesis_ChatText(t *testing.T) {
	in := "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)

	created := asMap(sseDataMap(t, events[0])["response"])
	if created["status"] != "in_progress" {
		t.Errorf("created status = %v", created["status"])
	}
	if got := sseCount(events, "[DONE]"); got != 0 {
		t.Errorf("responses stream must not carry the chat [DONE] marker, got %d", got)
	}
	completed := asMap(sseDataMap(t, events[8])["response"])
	if completed["id"] != "chatcmpl-1" || completed["status"] != "completed" {
		t.Errorf("completed response = %v", completed)
	}
	u := asMap(completed["usage"])
	if u["input_tokens"] != float64(9) || u["output_tokens"] != float64(3) {
		t.Errorf("completed usage = %v, want 9/3", u)
	}
}

// A4: chat→responses parallel interleaved tool_calls. index 0/1 deltas
// interleave without cross-talk; each call's events go start→delta…→done→
// item done; ids only appear on the item.added frames.
func TestResponsesSynthesis_ChatParallelToolCalls(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"fa\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"fb\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"y\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"2}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added", // call_a
		"response.output_item.added", // call_b
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)

	// Interleaved deltas route to the right item (no cross-talk).
	type deltaWant struct{ itemID, delta string }
	deltas := sseFilter(events, "response.function_call_arguments.delta")
	wantDeltas := []deltaWant{{"call_a", `{"x":`}, {"call_b", `{"y":`}, {"call_a", "1}"}, {"call_b", "2}"}}
	if len(deltas) != len(wantDeltas) {
		t.Fatalf("args deltas = %d, want %d", len(deltas), len(wantDeltas))
	}
	for i, w := range wantDeltas {
		d := sseDataMap(t, deltas[i])
		if strOf(d["item_id"]) != w.itemID || strOf(d["delta"]) != w.delta {
			t.Errorf("delta %d = item_id %v delta %v, want %v %q", i, d["item_id"], d["delta"], w.itemID, w.delta)
		}
	}
	// Done frames reconcile accumulated arguments per call.
	dones := sseFilter(events, "response.function_call_arguments.done")
	if len(dones) != 2 {
		t.Fatalf("args done frames = %d, want 2", len(dones))
	}
	if got := strOf(sseDataMap(t, dones[0])["arguments"]); got != `{"x":1}` {
		t.Errorf("call_a done arguments = %q, want %q", got, `{"x":1}`)
	}
	if got := strOf(sseDataMap(t, dones[1])["arguments"]); got != `{"y":2}` {
		t.Errorf("call_b done arguments = %q, want %q", got, `{"y":2}`)
	}
}

// A5a: anthropic→responses reasoning stream. thinking_delta→
// reasoning_summary_text.delta on an rs_item_* reasoning item; signature_delta
// accumulates into the done item's encrypted_content.
func TestResponsesSynthesis_AnthropicReasoning(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"..."}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_abc"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(in), "claude-x"))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)

	added := asMap(sseDataMap(t, events[1])["item"])
	if added["type"] != "reasoning" || added["id"] != "rs_item_0" {
		t.Errorf("reasoning item = %v, want reasoning rs_item_0", added)
	}
	// The signature lands verbatim on encrypted_content (round-trips back to
	// anthropic thinking.signature on the next turn).
	doneItem := asMap(sseDataMap(t, events[5])["item"])
	if doneItem["encrypted_content"] != "sig_abc" {
		t.Errorf("encrypted_content = %v, want sig_abc", doneItem["encrypted_content"])
	}
	summary := asSlice(doneItem["summary"], 0)
	if strOf(asMap(summary)["text"]) != "hmm..." {
		t.Errorf("summary text = %v, want hmm... (accumulated deltas)", asMap(summary)["text"])
	}
}

// A5b: chat→responses reasoning stream. delta.reasoning_content maps to a
// single reasoning item (rs_item_*) with reasoning_summary_text.delta frames,
// closed like any other item.
func TestResponsesSynthesis_ChatReasoning(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"reasoning_content\":\"think1\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"reasoning_content\":\"think2\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added", // reasoning rs_item_0
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		"response.output_item.added", // message msg_item_1
		"response.content_part.added",
		"response.output_text.delta",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)

	reasoning := asMap(sseDataMap(t, events[1])["item"])
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_item_0" {
		t.Errorf("reasoning item = %v, want reasoning rs_item_0", reasoning)
	}
	// Both reasoning deltas land on the SAME item (output_index 0).
	for i := 3; i <= 4; i++ {
		d := sseDataMap(t, events[i])
		if d["output_index"] != float64(0) {
			t.Errorf("reasoning delta output_index = %v, want 0 (single reasoning item)", d["output_index"])
		}
	}
	doneItem := asMap(sseDataMap(t, events[8])["item"])
	if strOf(asMap(asSlice(doneItem["summary"], 0))["text"]) != "think1think2" {
		t.Errorf("reasoning summary = %v, want think1think2", doneItem["summary"])
	}
}

// A6: EOF without a terminal signal (no message_stop / finish / [DONE]) is a
// truncated stream and must produce response.failed, never response.completed.
func TestResponsesSynthesis_EOF(t *testing.T) {
	// anthropic→responses: message_start then EOF.
	inA := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}` + "\n\n"
	eventsA := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(inA), "claude-x"))
	assertEventSequence(t, eventsA, []string{"response.created", "response.failed"})
	failedA := asMap(sseDataMap(t, eventsA[1])["response"])
	if failedA["status"] != "failed" {
		t.Errorf("EOF failed status = %v", failedA["status"])
	}

	// chat→responses: one content chunk then EOF (no finish_reason, no [DONE]).
	inC := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	eventsC := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(inC), "gpt-x"))
	if got := sseCount(eventsC, "response.failed"); got != 1 {
		t.Fatalf("chat EOF: response.failed count = %d, want 1: %v", got, sseEventTypes(eventsC))
	}
	if got := sseCount(eventsC, "response.completed"); got != 0 {
		t.Errorf("chat EOF: response.completed count = %d, want 0", got)
	}
}

// A7: mid-stream upstream errors. anthropic error event → response.failed
// (no response.completed); chat error chunk → response.failed too (never a
// silently-synthesized clean completion).
func TestResponsesSynthesis_Errors(t *testing.T) {
	// anthropic error event.
	inA := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}` + "\n\n"
	eventsA := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(inA), "claude-x"))
	failedA := sseFilter(eventsA, "response.failed")
	if len(failedA) != 1 {
		t.Fatalf("anthropic error: response.failed count = %d, want 1: %v", len(failedA), sseEventTypes(eventsA))
	}
	resp := asMap(sseDataMap(t, failedA[0])["response"])
	if resp["status"] != "failed" || strOf(asMap(resp["error"])["code"]) != "overloaded_error" {
		t.Errorf("failed response = %v", resp)
	}
	if got := sseCount(eventsA, "response.completed"); got != 0 {
		t.Errorf("anthropic error: response.completed count = %d, want 0", got)
	}

	// chat error chunk.
	inC := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_exceeded\"}}\n\n"
	eventsC := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(inC), "gpt-x"))
	failedC := sseFilter(eventsC, "response.failed")
	if len(failedC) != 1 {
		t.Fatalf("chat error: response.failed count = %d, want 1: %v", len(failedC), sseEventTypes(eventsC))
	}
	if msg := strOf(asMap(asMap(sseDataMap(t, failedC[0])["response"])["error"])["message"]); msg != "rate limited" {
		t.Errorf("failed error message = %q", msg)
	}
	if got := sseCount(eventsC, "response.completed"); got != 0 {
		t.Errorf("chat error: response.completed count = %d, want 0 (error must not become a clean finish)", got)
	}
}

// TestResponsesSynthesis_ChatLateToolIdentity: DashScope/xAI-style streams
// send the tool_call id/name in a LATER chunk (first chunks carry only
// arguments). output_item.added must be DELAYED until the name is known
// (added with an empty name is a protocol violation), the real id must
// replace the synthesized fallback, and buffered arguments flush right after
// added. Empty fragments never overwrite known identity.
func TestResponsesSynthesis_ChatLateToolIdentity(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_real\",\"function\":{\"name\":\"search\",\"arguments\":\"1}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw, err := io.ReadAll(newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	events := drainSSE(t, strings.NewReader(string(raw)))
	types := sseEventTypes(events)

	// added comes AFTER the first arguments-bearing chunk (delayed), carries
	// the real identity, and is immediately followed by the buffered args.
	addedAt, deltaAt := -1, -1
	for i, ev := range events {
		if ev.event == "response.output_item.added" {
			if addedAt != -1 {
				t.Fatalf("duplicate output_item.added: %v", types)
			}
			addedAt = i
			m := unmarshalMap(t, []byte(ev.data))
			item := asMap(m["item"])
			if item["name"] != "search" {
				t.Errorf("added item name = %v, want search", item["name"])
			}
			if item["call_id"] != "call_real" {
				t.Errorf("added item call_id = %v, want call_real (real id replaces fallback)", item["call_id"])
			}
		}
		if ev.event == "response.function_call_arguments.delta" && deltaAt == -1 {
			deltaAt = i
			m := unmarshalMap(t, []byte(ev.data))
			if m["delta"] != `{"a":1}` {
				t.Errorf("first args delta = %v, want buffered %q", m["delta"], `{"a":1}`)
			}
		}
	}
	if addedAt == -1 {
		t.Fatalf("no output_item.added emitted: %v", types)
	}
	if deltaAt == -1 || deltaAt < addedAt {
		t.Errorf("buffered args delta must follow added: addedAt=%d deltaAt=%d (%v)", addedAt, deltaAt, types)
	}
	// done carries the full accumulated arguments and pairs with added.
	assertResponsesItemPairing(t, events)
	for _, ev := range events {
		if ev.event == "response.function_call_arguments.done" {
			m := unmarshalMap(t, []byte(ev.data))
			if m["arguments"] != `{"a":1}` {
				t.Errorf("done arguments = %v, want %q", m["arguments"], `{"a":1}`)
			}
			if m["item_id"] != "call_real" {
				t.Errorf("done item_id = %v, want call_real", m["item_id"])
			}
		}
	}
}

// P1: synthesized output_item.done frames carry the COMPLETE item (real
// upstreams do; cc-switch streaming_codex_chat.rs:603-636,642-649), and
// response.completed carries the full output array — not a bare
// {type,id,status} stub and an empty output list. This test supersedes the
// earlier stub-shape pinning: the event SEQUENCE contract is unchanged.
func TestResponsesSynthesis_DoneFramesCarryFullItems(t *testing.T) {
	// chat→r: text + one tool call.
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"search\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	assertResponsesItemPairing(t, events)
	dones := sseFilter(events, "response.output_item.done")
	if len(dones) != 2 {
		t.Fatalf("output_item.done = %d, want 2", len(dones))
	}
	msgItem := asMap(sseDataMap(t, dones[0])["item"])
	if msgItem["type"] != "message" || msgItem["status"] != "completed" {
		t.Errorf("message done item = %v", msgItem)
	}
	part := asMap(asSlice(msgItem["content"], 0))
	if part["type"] != "output_text" || part["text"] != "hello" {
		t.Errorf("message done content = %v, want full output_text", msgItem["content"])
	}
	fcItem := asMap(sseDataMap(t, dones[1])["item"])
	if fcItem["type"] != "function_call" || fcItem["call_id"] != "call_1" || fcItem["name"] != "search" || fcItem["arguments"] != `{"q":"x"}` {
		t.Errorf("function_call done item = %v, want complete call", fcItem)
	}
	completed := asMap(sseDataMap(t, events[len(events)-1])["response"])
	output, _ := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed output = %d items, want 2: %v", len(output), completed["output"])
	}
	if asMap(output[0])["type"] != "message" || asMap(output[1])["type"] != "function_call" {
		t.Errorf("completed output types = %v", output)
	}
	if strOf(asMap(output[1])["arguments"]) != `{"q":"x"}` {
		t.Errorf("completed output arguments = %v", asMap(output[1])["arguments"])
	}
}

// P1 (a→r direction): same full-item contract for the anthropic-sourced
// synthesizer — done frames and the completed output carry the accumulated
// text/arguments/reasoning.
func TestResponsesSynthesis_AnthropicDoneFramesFullItems(t *testing.T) {
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(anthropicToolStream), "claude-x"))
	assertResponsesItemPairing(t, events)
	dones := sseFilter(events, "response.output_item.done")
	if len(dones) != 1 {
		t.Fatalf("output_item.done = %d, want 1", len(dones))
	}
	item := asMap(sseDataMap(t, dones[0])["item"])
	if item["type"] != "function_call" || item["call_id"] != "toolu_1" || item["name"] != "search" || item["arguments"] != `{"q":"x"}` {
		t.Errorf("done item = %v, want complete function_call", item)
	}
	completed := asMap(sseDataMap(t, events[len(events)-1])["response"])
	output, _ := completed["output"].([]any)
	if len(output) != 1 || asMap(output[0])["arguments"] != `{"q":"x"}` {
		t.Errorf("completed output = %v, want the full function_call item", completed["output"])
	}
}

// P1 tail: an anthropic text/thinking block that NEVER emitted any delta (no
// output_item.added was synthesized) must not emit *.done frames either —
// otherwise added/done pairing breaks for empty blocks.
func TestResponsesSynthesis_EmptyBlockNoDoneFrames(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"real"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(in), "claude-x"))
	assertResponsesItemPairing(t, events)
	if n := sseCount(events, "response.output_item.added"); n != 1 {
		t.Errorf("added = %d, want 1 (the empty block must not produce frames)", n)
	}
	if n := sseCount(events, "response.output_item.done"); n != 1 {
		t.Errorf("done = %d, want 1 (no done for the unopened block)", n)
	}
}

// P1: r→a stream usage splits cache WRITE out of input_tokens into
// cache_creation_input_tokens (details spelling), aligned with the
// non-streaming converter and the chat→a direction.
func TestResponsesStream_R2A_CacheWriteUsage(t *testing.T) {
	in := `data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20}}}}` + "\n\n"
	events := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	var usage map[string]any
	for _, ev := range events {
		if ev.event == "message_delta" {
			usage = asMap(sseDataMap(t, ev)["usage"])
		}
	}
	if usage == nil {
		t.Fatal("no message_delta usage")
	}
	if usage["input_tokens"] != float64(70) || usage["cache_read_input_tokens"] != float64(10) || usage["cache_creation_input_tokens"] != float64(20) {
		t.Errorf("usage = %v, want input 70 / read 10 / create 20", usage)
	}
}
