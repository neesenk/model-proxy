package protocol

// convert_specfix_stream_test.go — regression tests for protocol-spec
// conformance fixes in the streaming converters (convert_responses_stream.go):
// chat chunk envelope fields (id/object/created/model), the include_usage
// separate-usage-chunk contract, chat→r refusal streaming, required item_id
// on synthesized frames, reasoning_summary part/text.done shapes, standalone
// upstream error events, r→a tool_use id sanitizing, done-only/completed-only
// content fallbacks, and a→r response.failed well-formedness.

import (
	"strings"
	"testing"
)

// Spec fix 1: every synthesized r→chat chunk carries the full chunk envelope
// (id, object:"chat.completion.chunk", created, model) — created is one
// stable unix-seconds timestamp for the whole stream, sourced from
// response.created/response.completed created_at when the upstream sends it.
func TestSpecFix_R2Chat_ChunkEnvelopeCreated(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-x","created_at":1700000000}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","created_at":1700000000,"usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n"
	events := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	chunks := 0
	for _, ev := range events {
		if ev.data == "[DONE]" {
			continue
		}
		m := sseDataMap(t, ev)
		chunks++
		if m["id"] != "resp_1" {
			t.Errorf("chunk id = %v, want resp_1", m["id"])
		}
		if m["object"] != "chat.completion.chunk" {
			t.Errorf("chunk object = %v", m["object"])
		}
		if m["created"] != float64(1700000000) {
			t.Errorf("chunk created = %v, want 1700000000 (stable, from response.created)", m["created"])
		}
		if m["model"] != "gpt-x" {
			t.Errorf("chunk model = %v, want gpt-x (role chunk included)", m["model"])
		}
	}
	if chunks < 4 {
		t.Fatalf("chunk count = %d, want >= 4 (role/content/finish/usage)", chunks)
	}

	// No created_at anywhere: fall back to stream-start time.Now — non-zero
	// and identical on every chunk.
	in2 := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r2","status":"completed"}}` + "\n\n"
	events2 := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in2), "gpt-x"))
	var created any
	for _, ev := range events2 {
		if ev.data == "[DONE]" {
			continue
		}
		m := sseDataMap(t, ev)
		c, ok := m["created"].(float64)
		if !ok || c <= 0 {
			t.Fatalf("chunk created missing/non-positive: %v", m["created"])
		}
		if created == nil {
			created = m["created"]
		} else if m["created"] != created {
			t.Errorf("chunk created = %v, want stable %v across the stream", m["created"], created)
		}
	}
}

// Spec fix 2 (include_usage contract): the finish chunk carries NO usage;
// usage arrives in a separate final chunk with an empty choices array,
// immediately before data: [DONE].
func TestSpecFix_R2Chat_UsageInSeparateChunk(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-x"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n"
	events := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	assertEventSequence(t, events, []string{
		"chat.completion.chunk", // role
		"chat.completion.chunk", // content
		"chat.completion.chunk", // finish
		"chat.completion.chunk", // usage (empty choices)
		"[DONE]",
	})
	finish := sseDataMap(t, events[2])
	fc := asMap(asSlice(finish["choices"], 0))
	if fc["finish_reason"] != "stop" {
		t.Errorf("finish chunk finish_reason = %v, want stop", fc["finish_reason"])
	}
	if _, hasUsage := finish["usage"]; hasUsage {
		t.Errorf("finish chunk must NOT carry usage: %v", finish["usage"])
	}
	usageChunk := sseDataMap(t, events[3])
	choices, _ := usageChunk["choices"].([]any)
	if len(choices) != 0 {
		t.Errorf("usage chunk choices = %v, want empty array", usageChunk["choices"])
	}
	u := asMap(usageChunk["usage"])
	if u["prompt_tokens"] != float64(3) || u["completion_tokens"] != float64(2) || u["total_tokens"] != float64(5) {
		t.Errorf("usage = %v, want 3/2/5", u)
	}
}

// Spec fix 3: chat→r streams delta.refusal as response.refusal.delta events
// within a refusal content part; the completed snapshot carries the refusal
// part on the message item.
func TestSpecFix_ChatToResponses_RefusalStream(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"refusal\":\"I cannot\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"refusal\":\" do that\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added", // output_text
		"response.output_text.delta",
		"response.content_part.added", // refusal
		"response.refusal.delta",
		"response.refusal.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.refusal.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)

	// The refusal content part opens on the SAME message item, content_index 1.
	refPartAdded := sseDataMap(t, events[4])
	if strOf(asMap(refPartAdded["part"])["type"]) != "refusal" {
		t.Errorf("refusal part.added part = %v, want type refusal", refPartAdded["part"])
	}
	if refPartAdded["item_id"] != "msg_item_0" || refPartAdded["content_index"] != float64(1) {
		t.Errorf("refusal part.added = %v, want item_id msg_item_0 content_index 1", refPartAdded)
	}
	d1 := sseDataMap(t, events[5])
	if strOf(d1["delta"]) != "I cannot" || d1["item_id"] != "msg_item_0" {
		t.Errorf("refusal.delta = %v", d1)
	}
	if got := strOf(sseDataMap(t, events[9])["refusal"]); got != "I cannot do that" {
		t.Errorf("refusal.done refusal = %q, want full accumulated text", got)
	}
	// The completed snapshot's message item carries both parts.
	completed := asMap(sseDataMap(t, events[12])["response"])
	msg := asMap(asSlice(completed["output"], 0))
	content := anySlice(msg["content"])
	if len(content) != 2 {
		t.Fatalf("completed message content = %v, want 2 parts", content)
	}
	if strOf(asMap(content[0])["type"]) != "output_text" || strOf(asMap(content[0])["text"]) != "Hello" {
		t.Errorf("content[0] = %v, want output_text Hello", content[0])
	}
	if strOf(asMap(content[1])["type"]) != "refusal" || strOf(asMap(content[1])["refusal"]) != "I cannot do that" {
		t.Errorf("content[1] = %v, want refusal part", content[1])
	}
}

// Spec fix 4: synthesized a→r frames carry item_id matching the
// output_item.added item id (content_part.added/done, output_text.delta/done).
func TestSpecFix_AnthropicToResponses_ItemIDOnFrames(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(in), "claude-x"))
	itemID := strOf(asMap(sseDataMap(t, sseFilter(events, "response.output_item.added")[0])["item"])["id"])
	if itemID == "" {
		t.Fatal("output_item.added item has no id")
	}
	for _, name := range []string{
		"response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
	} {
		frames := sseFilter(events, name)
		if len(frames) != 1 {
			t.Fatalf("%s count = %d, want 1", name, len(frames))
		}
		if got := strOf(sseDataMap(t, frames[0])["item_id"]); got != itemID {
			t.Errorf("%s item_id = %q, want %q", name, got, itemID)
		}
	}
}

// Spec fix 4 (chat→r): same item_id requirement on the chat-sourced
// synthesized frames.
func TestSpecFix_ChatToResponses_ItemIDOnFrames(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	itemID := strOf(asMap(sseDataMap(t, sseFilter(events, "response.output_item.added")[0])["item"])["id"])
	if itemID == "" {
		t.Fatal("output_item.added item has no id")
	}
	for _, name := range []string{
		"response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
	} {
		frames := sseFilter(events, name)
		if len(frames) != 1 {
			t.Fatalf("%s count = %d, want 1", name, len(frames))
		}
		if got := strOf(sseDataMap(t, frames[0])["item_id"]); got != itemID {
			t.Errorf("%s item_id = %q, want %q", name, got, itemID)
		}
	}
}

// Spec fix 5 (a→r): reasoning_summary_part.added/done carry
// part:{type:"summary_text", text} + item_id + summary_index (not the item),
// and reasoning_summary_text.done pairs the deltas with the full text.
func TestSpecFix_AnthropicToResponses_ReasoningSummaryPartShapes(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"deep"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(in), "claude-x"))
	itemID := strOf(asMap(sseDataMap(t, sseFilter(events, "response.output_item.added")[0])["item"])["id"])

	added := sseDataMap(t, sseFilter(events, "response.reasoning_summary_part.added")[0])
	if asMap(added["item"]) != nil {
		t.Errorf("summary_part.added must not carry an item key: %v", added["item"])
	}
	part := asMap(added["part"])
	if part["type"] != "summary_text" || strOf(part["text"]) != "" {
		t.Errorf("summary_part.added part = %v, want empty summary_text", part)
	}
	if strOf(added["item_id"]) != itemID || added["summary_index"] != float64(0) {
		t.Errorf("summary_part.added = %v, want item_id %s summary_index 0", added, itemID)
	}

	textDone := sseFilter(events, "response.reasoning_summary_text.done")
	if len(textDone) != 1 {
		t.Fatalf("reasoning_summary_text.done count = %d, want 1", len(textDone))
	}
	td := sseDataMap(t, textDone[0])
	if strOf(td["text"]) != "deep" || strOf(td["item_id"]) != itemID {
		t.Errorf("reasoning_summary_text.done = %v, want text deep item_id %s", td, itemID)
	}

	done := sseDataMap(t, sseFilter(events, "response.reasoning_summary_part.done")[0])
	if asMap(done["item"]) != nil {
		t.Errorf("summary_part.done must not carry an item key: %v", done["item"])
	}
	dpart := asMap(done["part"])
	if dpart["type"] != "summary_text" || strOf(dpart["text"]) != "deep" {
		t.Errorf("summary_part.done part = %v, want summary_text deep", dpart)
	}
	if strOf(done["item_id"]) != itemID {
		t.Errorf("summary_part.done item_id = %v, want %s", done["item_id"], itemID)
	}
}

// Spec fix 5 (chat→r): same part shapes for the chat-sourced reasoning item.
func TestSpecFix_ChatToResponses_ReasoningSummaryPartShapes(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	itemID := strOf(asMap(sseDataMap(t, sseFilter(events, "response.output_item.added")[0])["item"])["id"])

	added := sseDataMap(t, sseFilter(events, "response.reasoning_summary_part.added")[0])
	if asMap(added["item"]) != nil {
		t.Errorf("summary_part.added must not carry an item key: %v", added["item"])
	}
	if part := asMap(added["part"]); part["type"] != "summary_text" {
		t.Errorf("summary_part.added part = %v, want summary_text", part)
	}
	if strOf(added["item_id"]) != itemID {
		t.Errorf("summary_part.added item_id = %v, want %s", added["item_id"], itemID)
	}

	textDone := sseFilter(events, "response.reasoning_summary_text.done")
	if len(textDone) != 1 || strOf(sseDataMap(t, textDone[0])["text"]) != "think" {
		t.Errorf("reasoning_summary_text.done = %v, want one frame with full text think", textDone)
	}
	done := sseDataMap(t, sseFilter(events, "response.reasoning_summary_part.done")[0])
	if asMap(done["item"]) != nil {
		t.Errorf("summary_part.done must not carry an item key: %v", done["item"])
	}
	if part := asMap(done["part"]); part["type"] != "summary_text" || strOf(part["text"]) != "think" {
		t.Errorf("summary_part.done part = %v, want summary_text think", part)
	}
}

// Spec fix 6: a standalone upstream `event: error` frame
// ({type:"error", code, message} — top-level fields) propagates its real
// message/code to both r→ client directions.
func TestSpecFix_ResponsesStandaloneErrorEvent(t *testing.T) {
	in := "event: error\n" +
		`data: {"type":"error","code":"rate_limit","message":"slow down","param":null,"sequence_number":7}` + "\n\n"

	eventsA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	errEvents := sseFilter(eventsA, "error")
	if len(errEvents) != 1 {
		t.Fatalf("r→a error events = %d, want 1: %v", len(errEvents), sseEventTypes(eventsA))
	}
	e := asMap(sseDataMap(t, errEvents[0])["error"])
	if strOf(e["message"]) != "slow down" || strOf(e["type"]) != "rate_limit" {
		t.Errorf("r→a error = %v, want message 'slow down' type rate_limit", e)
	}

	eventsC := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	sawErr := false
	for _, ev := range eventsC {
		if em := asMap(sseDataMap(t, ev)["error"]); em != nil {
			sawErr = true
			if strOf(em["message"]) != "slow down" || strOf(em["type"]) != "rate_limit" {
				t.Errorf("r→chat error chunk = %v, want message 'slow down' type rate_limit", em)
			}
		}
	}
	if !sawErr {
		t.Fatalf("r→chat: no error chunk in %v", sseEventTypes(eventsC))
	}
}

// Spec fix 7: r→a sanitizes streamed tool_use ids to ^[a-zA-Z0-9_-]+$, with a
// per-stream memo (the same raw call_id maps identically on every frame).
func TestSpecFix_R2A_ToolUseIDSanitized(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"functions.Bash:0","name":"Bash","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{}"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"functions.Bash:0","name":"Bash","arguments":"{}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	events := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	starts := sseFilter(events, "content_block_start")
	if len(starts) != 1 {
		t.Fatalf("content_block_start = %d, want 1", len(starts))
	}
	block := asMap(sseDataMap(t, starts[0])["content_block"])
	if block["type"] != "tool_use" || block["id"] != "functions_Bash_0" {
		t.Errorf("tool_use block = %v, want sanitized id functions_Bash_0", block)
	}

	// Done-only path: same sanitizing when the block is synthesized from the
	// done frame alone.
	inDoneOnly := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"functions.Bash:0","name":"Bash","arguments":"{}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"
	events2 := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(inDoneOnly), "m"))
	block2 := asMap(sseDataMap(t, sseFilter(events2, "content_block_start")[0])["content_block"])
	if block2["id"] != "functions_Bash_0" {
		t.Errorf("done-only tool_use id = %v, want functions_Bash_0", block2["id"])
	}
}

// Spec fix 8a (r→a): a done-only MESSAGE item (no added, no deltas — full
// content in output_item.done) synthesizes text blocks, refusal parts
// included.
func TestSpecFix_R2A_DoneOnlyMessageSynthesized(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"full answer"},{"type":"refusal","refusal":"but not that"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
	events := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	assertEventSequence(t, events, []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	})
	var texts []string
	for _, ev := range sseFilter(events, "content_block_delta") {
		texts = append(texts, strOf(asMap(sseDataMap(t, ev)["delta"])["text"]))
	}
	if len(texts) != 2 || texts[0] != "full answer" || texts[1] != "but not that" {
		t.Errorf("synthesized text blocks = %v, want [full answer but not that]", texts)
	}
}

// Spec fix 8a (r→chat): a done-only message item synthesizes content chunks.
func TestSpecFix_R2Chat_DoneOnlyMessageSynthesized(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress","model":"gpt-x"}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"full answer"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
	events := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	var contents []string
	for _, ev := range events {
		if ev.data == "[DONE]" {
			continue
		}
		ch := asMap(asSlice(sseDataMap(t, ev)["choices"], 0))
		if c := strOpt(asMap(ch["delta"])["content"]); c != "" {
			contents = append(contents, c)
		}
	}
	if len(contents) != 1 || contents[0] != "full answer" {
		t.Errorf("content chunks = %v, want [full answer]", contents)
	}
}

// Spec fix 8b: a completed-only stream (no item frames at all — full content
// in response.completed.output) materializes content in BOTH r→ directions.
func TestSpecFix_CompletedOnlyOutputMaterialized(t *testing.T) {
	in := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","model":"gpt-x",` +
		`"output":[{"type":"message","id":"msg_0","role":"assistant","status":"completed","content":[{"type":"output_text","text":"completed answer"}]},` +
		`{"type":"function_call","id":"fc_0","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}","status":"completed"}],` +
		`"usage":{"input_tokens":3,"output_tokens":5}}}` + "\n\n"

	// r→a: text block + tool_use block, tool_use stop reason.
	eventsA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	assertEventSequence(t, eventsA, []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // text
		"content_block_start", "content_block_delta", "content_block_stop", // tool_use
		"message_delta", "message_stop",
	})
	textBlock := asMap(sseDataMap(t, eventsA[1])["content_block"])
	if textBlock["type"] != "text" {
		t.Errorf("block 0 = %v, want text", textBlock)
	}
	if got := strOf(asMap(sseDataMap(t, eventsA[2])["delta"])["text"]); got != "completed answer" {
		t.Errorf("text delta = %q, want completed answer", got)
	}
	toolBlock := asMap(sseDataMap(t, eventsA[4])["content_block"])
	if toolBlock["type"] != "tool_use" || toolBlock["id"] != "call_1" || toolBlock["name"] != "search" {
		t.Errorf("tool block = %v, want tool_use call_1 search", toolBlock)
	}
	if got := strOf(asMap(sseDataMap(t, eventsA[7])["delta"])["stop_reason"]); got != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", got)
	}

	// r→chat: content chunk + complete tool_calls chunk, finish tool_calls.
	eventsC := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	var content, toolID, toolName, toolArgs, finish string
	for _, ev := range eventsC {
		if ev.data == "[DONE]" {
			continue
		}
		ch := asMap(asSlice(sseDataMap(t, ev)["choices"], 0))
		if ch == nil {
			continue
		}
		delta := asMap(ch["delta"])
		if c := strOpt(delta["content"]); c != "" {
			content += c
		}
		for _, tc := range anySlice(delta["tool_calls"]) {
			tcm := asMap(tc)
			toolID = strOpt(tcm["id"])
			toolName = strOpt(asMap(tcm["function"])["name"])
			toolArgs += strOf(asMap(tcm["function"])["arguments"])
		}
		if fr := strOpt(ch["finish_reason"]); fr != "" {
			finish = fr
		}
	}
	if content != "completed answer" {
		t.Errorf("r→chat content = %q, want completed answer", content)
	}
	if toolID != "call_1" || toolName != "search" || toolArgs != `{"q":"x"}` {
		t.Errorf("r→chat tool call = %v/%v/%v, want call_1/search args", toolID, toolName, toolArgs)
	}
	if finish != "tool_calls" {
		t.Errorf("r→chat finish = %q, want tool_calls", finish)
	}
}

// Spec fix 9: an anthropic stream opening with an error event still yields a
// well-formed response.created → response.failed sequence (with
// object:"response").
func TestSpecFix_A2R_ErrorFirstEmitsCreatedAndFailed(t *testing.T) {
	in := "event: error\n" +
		`data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}` + "\n\n"
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(in), "claude-x"))
	assertEventSequence(t, events, []string{"response.created", "response.failed"})
	resp := asMap(sseDataMap(t, events[1])["response"])
	if resp["object"] != "response" {
		t.Errorf("response.failed object = %v, want response", resp["object"])
	}
	if resp["status"] != "failed" {
		t.Errorf("response.failed status = %v, want failed", resp["status"])
	}
	if got := strOf(asMap(resp["error"])["message"]); got != "overloaded" {
		t.Errorf("response.failed error message = %q, want overloaded", got)
	}
}
