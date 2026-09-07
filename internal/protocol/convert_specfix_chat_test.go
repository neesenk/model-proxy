package protocol

// convert_specfix_chat_test.go — regression tests for the protocol-spec
// conformance fixes in convert.go (anthropic ↔ openai-chat direction):
// fail-closed error streams (no synthesized [DONE]), the required `created`
// field, the spec-shaped terminal usage chunk, response_format / legacy
// function-calling diagnostics, empty text part skipping, the post-terminal
// content guard, the stop_sequence key, unique id-less tool_use placeholders,
// folded [DONE] recovery, `event: error` frames, and BOM tolerance.

import (
	"strings"
	"testing"
)

// --- fix 1: an a→chat stream terminated via an error path must NOT emit [DONE] ---

func TestSpecfix_AToChat_ErrorStreamNeverEmitsDone(t *testing.T) {
	t.Run("EOF before terminal event", func(t *testing.T) {
		in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n"
		out := string(readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c")))
		if !strings.Contains(out, "terminated before a terminal event") {
			t.Errorf("missing fail-closed error chunk:\n%s", out)
		}
		if strings.Contains(out, "data: [DONE]") || strings.Contains(out, `"finish_reason"`) {
			t.Errorf("error-terminated stream synthesized a clean terminal:\n%s", out)
		}
	})
	t.Run("upstream error event", func(t *testing.T) {
		in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n"
		out := string(readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c")))
		if !strings.Contains(out, `"error"`) || !strings.Contains(out, "overloaded") {
			t.Errorf("upstream error not propagated:\n%s", out)
		}
		if strings.Contains(out, "data: [DONE]") || strings.Contains(out, `"finish_reason"`) {
			t.Errorf("error-terminated stream synthesized a clean terminal:\n%s", out)
		}
	})
	t.Run("clean stream still ends with exactly one [DONE]", func(t *testing.T) {
		in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		out := string(readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c")))
		if n := strings.Count(out, "data: [DONE]"); n != 1 {
			t.Errorf("[DONE] count = %d, want 1:\n%s", n, out)
		}
	})
}

// --- fix 2: chat.completion(.chunk) requires `created` (unix seconds) ---

func TestSpecfix_AToChat_CreatedField(t *testing.T) {
	out, err := convertAnthropicResponseToOpenAI([]byte(
		`{"id":"msg_1","model":"c","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	created, ok := unmarshalMap(t, out)["created"].(float64)
	if !ok || created <= 0 || created != float64(int64(created)) {
		t.Errorf("non-stream created = %v, want a positive unix-seconds integer: %s", created, out)
	}

	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	events := drainSSE(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c"))
	var streamCreated any
	chunks := 0
	for _, ev := range events {
		if ev.data == "[DONE]" {
			continue
		}
		m := sseDataMap(t, ev)
		ts, ok := m["created"].(float64)
		if !ok || ts <= 0 || ts != float64(int64(ts)) {
			t.Fatalf("stream chunk missing integer created: %v", m)
		}
		chunks++
		if streamCreated == nil {
			streamCreated = m["created"]
		} else if m["created"] != streamCreated {
			t.Errorf("created differs across chunks of one stream: %v vs %v", m["created"], streamCreated)
		}
	}
	if chunks == 0 {
		t.Fatal("no chunks in converted stream")
	}
}

// --- fix 3: terminal usage rides its own empty-choices chunk before [DONE] ---

// assertTerminalUsageShape pins the include_usage wire shape on the tail of a
// converted a→chat stream: finish chunk (finish_reason, empty delta, NO usage
// key) → usage chunk (choices == [], usage object) → exactly one [DONE].
func assertTerminalUsageShape(t *testing.T, events []sseEvent, wantFinish string, wantPrompt, wantCompletion float64) {
	t.Helper()
	var finish, usage map[string]any
	done := 0
	for _, ev := range events {
		if ev.data == "[DONE]" {
			done++
			continue
		}
		m := sseDataMap(t, ev)
		choices, _ := m["choices"].([]any)
		if len(choices) == 0 {
			if usage != nil {
				t.Fatalf("more than one empty-choices usage chunk: %v", events)
			}
			usage = m
			continue
		}
		if fr, _ := asMap(choices[0])["finish_reason"].(string); fr != "" {
			if finish != nil {
				t.Fatalf("more than one finish chunk: %v", events)
			}
			finish = m
		}
	}
	if done != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %v", done, sseEventTypes(events))
	}
	if finish == nil {
		t.Fatalf("no finish chunk: %v", sseEventTypes(events))
	}
	fc := asMap(asSliceAny(finish["choices"])[0])
	if fr, _ := fc["finish_reason"].(string); fr != wantFinish {
		t.Errorf("finish_reason = %q, want %q", fr, wantFinish)
	}
	if delta := asMap(fc["delta"]); len(delta) != 0 {
		t.Errorf("finish chunk delta = %v, want empty", delta)
	}
	if _, hasUsage := finish["usage"]; hasUsage {
		t.Errorf("finish chunk carries usage (must ride its own chunk): %v", finish)
	}
	if usage == nil {
		t.Fatalf("no terminal usage chunk: %v", sseEventTypes(events))
	}
	u := asMap(usage["usage"])
	if u == nil {
		t.Fatalf("usage chunk has no usage object: %v", usage)
	}
	if u["prompt_tokens"] != wantPrompt || u["completion_tokens"] != wantCompletion {
		t.Errorf("usage = %v, want prompt=%v completion=%v", u, wantPrompt, wantCompletion)
	}
	// Ordering: finish chunk, then usage chunk, then [DONE].
	var tail []string
	for _, ev := range events {
		if ev.data == "[DONE]" {
			tail = append(tail, "[DONE]")
			continue
		}
		m := sseDataMap(t, ev)
		choices, _ := m["choices"].([]any)
		switch {
		case len(choices) == 0:
			tail = append(tail, "usage")
		default:
			if fr, _ := asMap(choices[0])["finish_reason"].(string); fr != "" {
				tail = append(tail, "finish")
			} else {
				tail = append(tail, "content")
			}
		}
	}
	if len(tail) < 3 || tail[len(tail)-3] != "finish" || tail[len(tail)-2] != "usage" || tail[len(tail)-1] != "[DONE]" {
		t.Errorf("terminal sequence = %v, want ... finish, usage, [DONE]", tail)
	}
}

func TestSpecfix_AToChat_UsageChunkShape(t *testing.T) {
	t.Run("message_delta path", func(t *testing.T) {
		in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		events := drainSSE(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c"))
		assertTerminalUsageShape(t, events, "stop", 5, 3)
	})
	t.Run("[DONE] fallback path (no message_delta)", func(t *testing.T) {
		in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"data: [DONE]\n\n"
		events := drainSSE(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c"))
		assertTerminalUsageShape(t, events, "stop", 5, 0)
	})
}

// --- fix 4: chat→a drops response_format with a structured diagnostic ---

func TestSpecfix_ChatToAnthropic_ResponseFormatDropped(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object"}}}}`)
	diag := NewDiagnostics()
	out, err := convertOpenAIRequestToAnthropic(body, diag)
	if err != nil {
		t.Fatal(err)
	}
	if !diag.HasCode("response_format_dropped") {
		t.Errorf("response_format_dropped missing: %+v", diag.Items())
	}
	root := unmarshalMap(t, out)
	if _, leaked := root["response_format"]; leaked {
		t.Errorf("response_format leaked into anthropic request: %s", out)
	}
	if msgs, _ := root["messages"].([]any); len(msgs) != 1 {
		t.Errorf("conversion otherwise broken: %s", out)
	}
}

// --- fix 5: empty text parts are skipped (Anthropic rejects empty blocks) ---

func TestSpecfix_ChatToAnthropic_EmptyTextPartSkipped(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"hi"}]},
		{"role":"user","content":[{"type":"text","text":""}]},
		{"role":"user","content":"after"}]}`)
	out, err := convertOpenAIRequestToAnthropic(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (consecutive users merge; empty-only message dropped): %s", len(msgs), out)
	}
	blocks, _ := asMap(msgs[0])["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (empty text part skipped): %s", len(blocks), out)
	}
	if got := strOf(asMap(blocks[0])["text"]); got != "hi" {
		t.Errorf("block 0 text = %q, want hi", got)
	}
	if got := strOf(asMap(blocks[1])["text"]); got != "after" {
		t.Errorf("block 1 text = %q, want after", got)
	}
}

// --- fix 6: a→chat suppresses content/tool frames after the finish chunk ---

func TestSpecfix_AToChat_PostTerminalGuard(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"late\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := string(readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c")))
	if strings.Contains(out, "late") || strings.Contains(out, "tool_calls") {
		t.Errorf("content/tool frames after the finish chunk leaked:\n%s", out)
	}
	if n := strings.Count(out, `"finish_reason":"stop"`); n != 1 {
		t.Errorf("non-null finish chunks = %d, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, `"content":"hi"`) || !strings.Contains(out, "data: [DONE]") {
		t.Errorf("pre-terminal content or [DONE] lost:\n%s", out)
	}
}

// --- fix 7: legacy function-calling shapes drop with diagnostics ---

func TestSpecfix_ChatToAnthropic_LegacyFunctionCallingWarns(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","function_call":{"name":"f","arguments":"{}"}}],
		"functions":[{"name":"f","parameters":{"type":"object"}}],
		"function_call":{"name":"f"}}`)
	diag := NewDiagnostics()
	out, err := convertOpenAIRequestToAnthropic(body, diag)
	if err != nil {
		t.Fatal(err)
	}
	if n := countDiagCode(diag, "block_dropped"); n != 3 {
		t.Errorf("block_dropped count = %d, want 3 (message function_call, functions, function_call): %+v", n, diag.Items())
	}
	if strings.Contains(string(out), `"functions"`) || strings.Contains(string(out), "function_call") {
		t.Errorf("legacy function-calling shapes leaked into anthropic request: %s", out)
	}
	// The assistant message carried ONLY the legacy shape: zero blocks, dropped.
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 1 || strOf(asMap(msgs[0])["role"]) != "user" {
		t.Errorf("messages = %v, want just the user message: %s", msgs, out)
	}
}

func countDiagCode(d *Diagnostics, code string) int {
	n := 0
	for _, item := range d.Items() {
		if item.Code == code {
			n++
		}
	}
	return n
}

// --- fix 8: non-streaming chat→a response always carries stop_sequence ---

func TestSpecfix_ChatToAnthropic_StopSequenceKey(t *testing.T) {
	out, err := convertOpenAIResponseToAnthropic([]byte(
		`{"id":"c1","model":"gpt","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	root := unmarshalMap(t, out)
	v, ok := root["stop_sequence"]
	if !ok {
		t.Fatalf("stop_sequence key missing (official Messages carry it as null): %s", out)
	}
	if v != nil {
		t.Errorf("stop_sequence = %v, want null when absent upstream", v)
	}
}

// --- fix 10: id-less tool_calls get unique placeholders, positional pairing ---

func TestSpecfix_ChatToAnthropic_IdlessToolCallsUnique(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","tool_calls":[
			{"type":"function","function":{"name":"f","arguments":"{}"}},
			{"type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","content":"r1"},
		{"role":"tool","content":"r2"},
		{"role":"assistant","tool_calls":[{"id":"call.x","type":"function","function":{"name":"h","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call.x","content":"r3"}]}`)
	out, err := convertOpenAIRequestToAnthropic(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var useIDs, resultIDs []string
	for _, m := range asSliceAny(unmarshalMap(t, out)["messages"]) {
		for _, b := range asSliceAny(asMap(m)["content"]) {
			bm := asMap(b)
			switch bm["type"] {
			case "tool_use":
				useIDs = append(useIDs, strOf(bm["id"]))
			case "tool_result":
				resultIDs = append(resultIDs, strOf(bm["tool_use_id"]))
			}
		}
	}
	if len(useIDs) != 3 || len(resultIDs) != 3 {
		t.Fatalf("tool_use/tool_result counts = %d/%d, want 3/3: %s", len(useIDs), len(resultIDs), out)
	}
	if useIDs[0] == useIDs[1] {
		t.Errorf("id-less tool_calls collided on one placeholder: %v", useIDs)
	}
	for _, id := range useIDs[:2] {
		if !strings.HasPrefix(id, "toolu_empty_") {
			t.Errorf("id-less tool_call id = %q, want a toolu_empty_ placeholder", id)
		}
	}
	// Id-less results pair positionally with the id-less calls.
	if resultIDs[0] != useIDs[0] || resultIDs[1] != useIDs[1] {
		t.Errorf("positional pairing broken: use=%v result=%v", useIDs, resultIDs)
	}
	// A tool_result referencing a known id still maps through the memo.
	if useIDs[2] != "call_x" || resultIDs[2] != "call_x" {
		t.Errorf("named id pairing broken: use=%q result=%q, want call_x", useIDs[2], resultIDs[2])
	}
}

// --- fix 11a: a [DONE] line folded into a multi-frame payload still terminates ---

func TestSpecfix_FoldedDoneTerminator(t *testing.T) {
	t.Run("a→chat: folded [DONE] is a clean terminal", func(t *testing.T) {
		in := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n" +
			"data: [DONE]\n\n"
		out := string(readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c")))
		if !strings.Contains(out, `"finish_reason":"stop"`) || strings.Count(out, "data: [DONE]") != 1 {
			t.Errorf("folded [DONE] did not terminate cleanly:\n%s", out)
		}
		if strings.Contains(out, "terminated before a terminal event") {
			t.Errorf("folded [DONE] lost, spurious error chunk:\n%s", out)
		}
	})
	t.Run("a→chat: frames after a folded [DONE] are not processed", func(t *testing.T) {
		in := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n" +
			"data: [DONE]\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"evil\",\"usage\":{\"input_tokens\":999}}}\n\n"
		out := string(readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "c")))
		if strings.Contains(out, "evil") || strings.Contains(out, "999") {
			t.Errorf("frame after folded [DONE] leaked into the client stream:\n%s", out)
		}
	})
	t.Run("chat→a: folded [DONE] is a clean terminal", func(t *testing.T) {
		in := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
			"data: [DONE]\n\n"
		out := string(readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "gpt")))
		if !strings.Contains(out, "event: message_stop") || !strings.Contains(out, `"text":"hi"`) {
			t.Errorf("folded [DONE] did not terminate cleanly:\n%s", out)
		}
		if strings.Contains(out, "event: error") {
			t.Errorf("folded [DONE] lost, spurious error event:\n%s", out)
		}
	})
}

// --- fix 11b: chat→a recognizes an explicit `event: error` frame ---

func TestSpecfix_ChatToAnthropic_EventErrorFrame(t *testing.T) {
	// Payload carries no "error" key — only the event line marks it.
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"event: error\ndata: {\"message\":\"boom\"}\n\n"
	out := string(readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "gpt")))
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "boom") {
		t.Errorf("event:error frame not surfaced as an anthropic error event:\n%s", out)
	}
	if strings.Contains(out, "event: message_stop") {
		t.Errorf("error frame synthesized a clean terminal:\n%s", out)
	}

	// A non-error event: line must not trip the error path.
	clean := "event: chunk\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	out2 := string(readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(clean), "gpt")))
	if strings.Contains(out2, "event: error") || !strings.Contains(out2, "event: message_stop") {
		t.Errorf("harmless event line broke the stream:\n%s", out2)
	}
}

// --- fix 11c: one leading UTF-8 BOM at stream start is tolerated ---

func TestSpecfix_LeadingBOMStripped(t *testing.T) {
	const bom = "\ufeff"
	anthropicIn := bom + "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := string(readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(anthropicIn), "c")))
	if !strings.Contains(out, `"finish_reason":"stop"`) || strings.Contains(out, "terminated before a terminal event") {
		t.Errorf("BOM-prefixed anthropic stream did not convert cleanly:\n%s", out)
	}

	chatIn := bom + "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	out2 := string(readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(chatIn), "gpt")))
	if !strings.Contains(out2, `"text":"hi"`) || !strings.Contains(out2, "event: message_stop") {
		t.Errorf("BOM-prefixed chat stream did not convert cleanly:\n%s", out2)
	}
}
