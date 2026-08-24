package protocol

// convert_fault_test.go — fault-tolerance and terminal-state tests for the
// protocol converters: bad SSE frames, unknown events, error/incomplete
// terminal events, duplicate terminals, orphaned tool pairs, oversized lines,
// same-protocol byte-identical passthrough, and image mapping.

import (
	"io"
	"strings"
	"testing"
)

// C1: unknown SSE event types are silently skipped and the stream continues
// (all directions that dispatch on the event: line).
func TestConvertFault_UnknownSSEEventSkipped(t *testing.T) {
	// responses→anthropic: an unknown response.* event AND an unknown output
	// item type (web_search_call) between known events.
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}` + "\n\n" +
		"event: response.some_future_event\n" +
		`data: {"type":"response.some_future_event","foo":1}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_0"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` + "\n\n"
	events := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	assertEventSequence(t, events, []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop",
	})

	// anthropic→responses: ping/unknown anthropic events are skipped.
	inA := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"c"}}` + "\n\n" +
		"event: ping\n" +
		`data: {"type":"ping"}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	eventsA := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(inA), "c"))
	if got := sseCount(eventsA, "response.completed"); got != 1 {
		t.Errorf("a→r with unknown events: completed count = %d, want 1: %v", got, sseEventTypes(eventsA))
	}
	if got := sseCount(eventsA, "response.output_text.delta"); got != 1 {
		t.Errorf("a→r with unknown events: text delta count = %d, want 1", got)
	}
}

// C2: a malformed JSON data frame is skipped (not fatal); the stream
// continues with the following valid frames. Locked per current behavior.
func TestConvertFault_MalformedDataFrameSkipped(t *testing.T) {
	// anthropic→responses.
	inA := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"c"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		"data: {broken json\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	eventsA := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(inA), "c"))
	if got := sseCount(eventsA, "response.output_text.delta"); got != 1 {
		t.Errorf("a→r malformed frame: text delta count = %d, want 1 (bad frame skipped)", got)
	}
	if got := sseCount(eventsA, "response.completed"); got != 1 {
		t.Errorf("a→r malformed frame: completed count = %d, want 1", got)
	}

	// responses→anthropic.
	inR := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		"data: <<<>>>\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"
	eventsR := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(inR), "gpt-x"))
	assertEventSequence(t, eventsR, []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop",
	})
}

// C3: duplicate response.completed frames produce exactly ONE terminal
// (message_stop / finish chunk + [DONE]) — terminal singleness.
func TestConvertFault_DuplicateCompletedSingleTerminal(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":9,"output_tokens":9}}}` + "\n\n"

	eventsA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	if got := sseCount(eventsA, "message_stop"); got != 1 {
		t.Errorf("r→a duplicate completed: message_stop count = %d, want 1", got)
	}
	if got := sseCount(eventsA, "message_delta"); got != 1 {
		t.Errorf("r→a duplicate completed: message_delta count = %d, want 1", got)
	}

	eventsO := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	if got := sseCount(eventsO, "[DONE]"); got != 1 {
		t.Errorf("r→o duplicate completed: [DONE] count = %d, want 1", got)
	}
	finishChunks := 0
	for _, c := range sseFilter(eventsO, "chat.completion.chunk") {
		ch := asMap(asSlice(sseDataMap(t, c)["choices"], 0))
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			finishChunks++
		}
	}
	if finishChunks != 1 {
		t.Errorf("r→o duplicate completed: finish chunks = %d, want 1", finishChunks)
	}
}

// C4: response.failed → anthropic error event / chat error chunk, with NO
// clean terminal frame; response.incomplete → max_tokens / length (a normal
// terminal with the truncated stop reason).
func TestConvertFault_ResponsesFailed(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		"event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"boom"}}}` + "\n\n"

	eventsA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	errEvents := sseFilter(eventsA, "error")
	if len(errEvents) != 1 {
		t.Fatalf("r→a failed: error event count = %d, want 1: %v", len(errEvents), sseEventTypes(eventsA))
	}
	if msg := strOf(asMap(sseDataMap(t, errEvents[0])["error"])["message"]); msg != "boom" {
		t.Errorf("r→a error message = %q", msg)
	}
	if got := sseCount(eventsA, "message_stop"); got != 0 {
		t.Errorf("r→a failed: message_stop count = %d, want 0 (no clean terminal after failure)", got)
	}

	eventsO := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	sawErrChunk := false
	for _, ev := range eventsO {
		if e := asMap(sseDataMap(t, ev)["error"]); e != nil && e["message"] == "boom" {
			sawErrChunk = true
		}
	}
	if !sawErrChunk {
		t.Errorf("r→o failed: no error chunk in: %v", sseEventTypes(eventsO))
	}
	if got := sseCount(eventsO, "[DONE]"); got != 0 {
		t.Errorf("r→o failed: [DONE] count = %d, want 0 (no clean terminal after failure)", got)
	}
}

func TestConvertFault_ResponsesIncomplete(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.incomplete\n" +
		`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","usage":{"input_tokens":2,"output_tokens":3}}}` + "\n\n"

	eventsA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	md := sseFilter(eventsA, "message_delta")
	if len(md) != 1 {
		t.Fatalf("r→a incomplete: message_delta count = %d, want 1", len(md))
	}
	if sr := strOf(asMap(sseDataMap(t, md[0])["delta"])["stop_reason"]); sr != "max_tokens" {
		t.Errorf("r→a incomplete stop_reason = %q, want max_tokens", sr)
	}
	if got := sseCount(eventsA, "message_stop"); got != 1 {
		t.Errorf("r→a incomplete: message_stop count = %d, want 1", got)
	}

	eventsO := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	fr := ""
	for _, c := range sseFilter(eventsO, "chat.completion.chunk") {
		ch := asMap(asSlice(sseDataMap(t, c)["choices"], 0))
		if s, ok := ch["finish_reason"].(string); ok && s != "" {
			fr = s
		}
	}
	if fr != "length" {
		t.Errorf("r→o incomplete finish_reason = %q, want length", fr)
	}
}

// C5: a non-JSON function_call arguments string is wrapped recoverably as
// {"raw": <text>} with a convertWarn (anthropic tool_use.input must be an
// object — the raw text stays available to the tool instead of being lost).
func TestConvertFault_NonJSONToolArgsFallback(t *testing.T) {
	in := `{"model":"gpt-x","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"not-json{"}]}`
	var out []byte
	logs := captureConvertLog(t, func() {
		var err error
		out, err = convertResponsesRequestToAnthropic([]byte(in), nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(logs, "tool_call arguments not JSON") {
		t.Errorf("missing fallback warning, log = %q", logs)
	}
	// The input starts with a function_call, so a placeholder user message is
	// inserted first (first-message-must-be-user); find the tool_use block.
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	var blk map[string]any
	for _, m := range msgs {
		for _, b := range asMap(m)["content"].([]any) {
			if bm := asMap(b); bm != nil && bm["type"] == "tool_use" {
				blk = bm
			}
		}
	}
	if blk == nil {
		t.Fatalf("tool_use block not found: %s", out)
	}
	input := asMap(blk["input"])
	if input == nil || input["raw"] != "not-json{" {
		t.Errorf("tool_use input = %v, want {raw: not-json{}", blk["input"])
	}
}

// C6: ordinary full-history conversion preserves incomplete tool pairs. Repair
// is scoped to a missed previous_response_id expansion, not every request.
func TestConvertFault_OrphanToolPairs(t *testing.T) {
	// responses→anthropic: orphan function_call_output.
	out, err := convertResponsesRequestToAnthropic([]byte(
		`{"model":"gpt-x","input":[{"type":"function_call_output","call_id":"call_9","output":"found"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("orphan fco messages = %d, want 1: %s", len(msgs), out)
	}
	blk := asMap(asSlice(asMap(msgs[0])["content"], 0))
	if blk["type"] != "tool_result" || blk["tool_use_id"] != "call_9" || blk["content"] != "found" {
		t.Errorf("orphan tool_result = %v", blk)
	}

	// openai→anthropic: orphan role:tool (id sanitized per Tool ID contract).
	out2, err := convertOpenAIRequestToAnthropic([]byte(
		`{"model":"gpt-x","messages":[{"role":"tool","tool_call_id":"orphan.id:1","content":"x"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs2, _ := unmarshalMap(t, out2)["messages"].([]any)
	blk2 := asMap(asSlice(asMap(msgs2[0])["content"], 0))
	if blk2["type"] != "tool_result" || blk2["tool_use_id"] != "orphan_id_1" {
		t.Errorf("orphan chat tool msg = %v", blk2)
	}

	// anthropic→responses: dangling tool_use (no result) still converts.
	out3, err := convertAnthropicRequestToResponses([]byte(
		`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	input3, _ := unmarshalMap(t, out3)["input"].([]any)
	fc := asMap(input3[1])
	if fc["type"] != "function_call" || fc["call_id"] != "t1" {
		t.Errorf("dangling tool_use = %v", fc)
	}
}

// C7: an SSE stream whose final frame has no trailing newline still parses
// (bufio.Scanner yields the final token).
func TestConvertFault_FinalFrameNoTrailingNewline(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]"
	out, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(in), "gpt"))
	s := string(out)
	if !strings.Contains(s, "event: message_stop") || !strings.Contains(s, `"text":"hi"`) {
		t.Errorf("unterminated stream did not convert cleanly:\n%s", s)
	}
}

// C8: an SSE line over the 8 MiB scanner limit trips the scanner error
// warning (convertWarn) instead of silently truncating.
func TestConvertFault_OversizedLineWarns(t *testing.T) {
	big := "data: " + strings.Repeat("x", sseScanBuf) + "\n\n"
	logs := captureConvertLog(t, func() {
		io.Copy(io.Discard, newOpenAIToAnthropicSSE(strings.NewReader(big), "gpt"))
	})
	if !strings.Contains(logs, "SSE scanner error") {
		t.Errorf("oversized line did not warn, log = %q", logs)
	}
}

// C10: responses-direction image mapping (request side): anthropic base64
// image ↔ input_image data URL ↔ chat image_url.
func TestConvertFault_ResponsesImageMapping(t *testing.T) {
	const dataURL = "data:image/png;base64,aGVsbG8="

	// anthropic→responses: image block → input_image data URL.
	out, err := convertAnthropicRequestToResponses([]byte(
		`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	part := asMap(asSlice(asMap(asSlice(unmarshalMap(t, out)["input"], 0))["content"], 0))
	if part["type"] != "input_image" || part["image_url"] != dataURL {
		t.Errorf("a→r image part = %v", part)
	}

	// responses→anthropic: input_image data URL → base64 image block.
	out2, err := convertResponsesRequestToAnthropic([]byte(
		`{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"`+dataURL+`"}]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	blk := asMap(asSlice(asMap(asSlice(unmarshalMap(t, out2)["messages"], 0))["content"], 0))
	src := asMap(blk["source"])
	if blk["type"] != "image" || src["media_type"] != "image/png" || src["data"] != "aGVsbG8=" {
		t.Errorf("r→a image block = %v", blk)
	}

	// openai→responses: chat image_url part → input_image.
	out3, err := convertOpenAIRequestToResponses([]byte(
		`{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+dataURL+`"}}]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	part3 := asMap(asSlice(asMap(asSlice(unmarshalMap(t, out3)["input"], 0))["content"], 0))
	if part3["type"] != "input_image" || part3["image_url"] != dataURL {
		t.Errorf("o→r image part = %v", part3)
	}

	// responses→openai: input_image → chat image_url part (not silently dropped).
	out4, err := convertResponsesRequestToOpenAI([]byte(
		`{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this?"},{"type":"input_image","image_url":"` + dataURL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := asMap(asSlice(unmarshalMap(t, out4)["messages"], 0))["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("r→o message content parts = %d, want 2 (text + image): %s", len(content), out4)
	}
	img := asMap(content[1])
	if img["type"] != "image_url" || strOf(asMap(img["image_url"])["url"]) != dataURL {
		t.Errorf("r→o image part = %v", img)
	}
}

// J: unrecognized blocks/parts/items/events are dropped WITH a convertWarn,
// never silently (the "no silent drops" principle), across all converters.
func TestConvertFault_UnknownMappingWarns(t *testing.T) {
	cases := []struct {
		name string
		run  func() // must call the converter inside captureConvertLog's fn
		want string
	}{
		{"chat→r input_audio part", func() {
			convertOpenAIRequestToResponses([]byte(`{"model":"g","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]}]}`), nil)
		}, "dropping chat content part in chat→r request: input_audio"},
		{"chat→a input_audio part", func() {
			convertOpenAIRequestToAnthropic([]byte(`{"model":"g","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]}]}`), nil)
		}, "dropping unknown openai content part: input_audio"},
		{"a→r stop_sequences", func() {
			convertAnthropicRequestToResponses([]byte(`{"model":"c","max_tokens":10,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`))
		}, "dropping stop_sequences"},
		{"chat→r stop", func() {
			convertOpenAIRequestToResponses([]byte(`{"model":"g","stop":["END"],"messages":[{"role":"user","content":"hi"}]}`), nil)
		}, "dropping stop (Responses API has no stop parameter)"},
	}
	for _, c := range cases {
		logs := captureConvertLog(t, c.run)
		if !strings.Contains(logs, c.want) {
			t.Errorf("%s: missing warning %q, log = %q", c.name, c.want, logs)
		}
	}
}

// J (stream): unknown SSE event types warn (once per process per type) while
// the stream continues — C1 locks the continuation, this locks the warning.
func TestConvertFault_UnknownStreamEventWarns(t *testing.T) {
	in := "event: response.some_future_event\n" +
		`data: {"type":"response.some_future_event","foo":1}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"
	logs := captureConvertLog(t, func() {
		io.Copy(io.Discard, newResponsesToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	})
	if !strings.Contains(logs, "ignoring unknown responses SSE event (r→a): response.some_future_event") {
		t.Errorf("r→a unknown event not warned, log = %q", logs)
	}

	inA := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n" +
		"event: some_future_event\n" +
		`data: {"type":"some_future_event"}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	logsA := captureConvertLog(t, func() {
		io.Copy(io.Discard, newAnthropicToResponsesSSE(strings.NewReader(inA), "c"))
	})
	if !strings.Contains(logsA, "ignoring unknown anthropic SSE event (a→r): some_future_event") {
		t.Errorf("a→r unknown event not warned, log = %q", logsA)
	}
}

// TestConvertFault_NoNullStringPollution: missing optional tool fields must
// NOT leak the literal string "null" into converted output (strOf(nil) =
// "null" regression class). Missing arguments default to "{}", missing
// function names are backfilled from earlier items with the same call_id.
func TestConvertFault_NoNullStringPollution(t *testing.T) {
	// chat→r request: tool_call missing id/name/arguments; a later turn
	// resends the call with ONLY the id (replace-style client) → name
	// backfilled; role:tool without tool_call_id → empty call_id, not "null".
	chatReq := `{"model":"m","messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"search","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"r1"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{}}]},` +
		`{"role":"tool","content":"r2"},` +
		`{"role":"user","content":"go"}]}`
	out, err := convertRequest([]byte(chatReq), "openai", "responses")
	if err != nil {
		t.Fatalf("chat→r request: %v", err)
	}
	if strings.Contains(string(out), `"null"`) {
		t.Errorf("chat→r request leaked literal null: %s", out)
	}
	items := unmarshalMap(t, out)["input"].([]any)
	var lastCall map[string]any
	for _, it := range items {
		if asMap(it)["type"] == "function_call" {
			lastCall = asMap(it)
		}
	}
	if lastCall == nil || lastCall["name"] != "search" {
		t.Errorf("replace-style tool_call name not backfilled: %v", lastCall)
	}
	if lastCall["arguments"] != "{}" {
		t.Errorf("missing arguments not defaulted to {}: %v", lastCall["arguments"])
	}

	// r→a request: function_call without call_id/id → empty tool_use id.
	rReq := `{"model":"m","input":[{"type":"function_call","name":"f","arguments":"{}"}]}`
	outA, err := convertRequest([]byte(rReq), "responses", "anthropic")
	if err != nil {
		t.Fatalf("r→a request: %v", err)
	}
	if strings.Contains(string(outA), `"null"`) {
		t.Errorf("r→a request leaked literal null: %s", outA)
	}

	// chat→r non-stream response: tool_call missing arguments → "{}".
	chatResp := `{"id":"c1","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"x","function":{"name":"f"}}],"content":""},"finish_reason":"tool_calls"}]}`
	outR, err := convertOpenAIResponseToResponses([]byte(chatResp))
	if err != nil {
		t.Fatalf("chat→r response: %v", err)
	}
	if strings.Contains(string(outR), `"null"`) {
		t.Errorf("chat→r response leaked literal null: %s", outR)
	}
	for _, it := range unmarshalMap(t, outR)["output"].([]any) {
		if m := asMap(it); m["type"] == "function_call" && m["arguments"] != "{}" {
			t.Errorf("response tool_call arguments = %v, want {}", m["arguments"])
		}
	}

	// chat→r stream: tool_call chunk without id → fc_item_ fallback; without
	// name/arguments → no "null" frames.
	streamIn := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw, _ := io.ReadAll(newOpenAIToResponsesSSE(strings.NewReader(streamIn), "m"))
	if strings.Contains(string(raw), `"null"`) {
		t.Errorf("chat→r stream leaked literal null:\n%s", raw)
	}
	if !strings.Contains(string(raw), "fc_item_") {
		t.Errorf("chat→r stream missing fc_item_ fallback id:\n%s", raw)
	}

	// a→r stream: tool_use block without id → call_id falls back to the
	// synthesized item id, never "null".
	streamA := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"f","input":{}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	rawA, _ := io.ReadAll(newAnthropicToResponsesSSE(strings.NewReader(streamA), "m"))
	if strings.Contains(string(rawA), `"null"`) {
		t.Errorf("a→r stream leaked literal null:\n%s", rawA)
	}
	if !strings.Contains(string(rawA), `"call_id":"fc_item_0"`) {
		t.Errorf("a→r stream call_id not backed by item id:\n%s", rawA)
	}
}

// --- coverage: boundary conditions ---

// 8 MiB SSE line boundary: a line clearly under sseScanBuf converts fine;
// one over it trips the scanner-error convertWarn (C8 covered the oversize
// path — this pins the pass side at boundary-ε and the warn side at +1).
func TestConvertFault_SSELineBoundary(t *testing.T) {
	// boundary-ε: passes and converts.
	small := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", sseScanBuf-64) + "\"}}]}\n\ndata: [DONE]\n\n"
	out, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(small), "g"))
	if !strings.Contains(string(out), "event: message_stop") {
		t.Errorf("boundary-ε line did not convert (len=%d)", len("data: {\"choices\":[{\"delta\":{\"content\":\"")+sseScanBuf-64)
	}
	// +1 over the scanner max: warns (never silently truncates).
	big := "data: " + strings.Repeat("x", sseScanBuf) + "\n\n"
	logs := captureConvertLog(t, func() {
		io.Copy(io.Discard, newOpenAIToAnthropicSSE(strings.NewReader(big), "g"))
	})
	if !strings.Contains(logs, "SSE scanner error") {
		t.Errorf("+1 line did not warn, log = %q", logs)
	}
}
