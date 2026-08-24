package protocol

// convert_reasoning_test.go — reasoning / encrypted_content cross-protocol
// mapping (convert_responses.go + convert_responses_stream.go). The contract:
// responses reasoning (summary.text + encrypted_content) ↔ anthropic thinking
// (thinking + signature) ↔ chat reasoning_content, preserved verbatim
// (signatures must round-trip byte-exact or the upstream 400s).

import (
	"strings"
	"testing"
)

// B1: request a→r — an assistant thinking block with signature becomes a
// reasoning item with summary.text and encrypted_content, verbatim.
func TestConvertReasoning_AnthropicToResponsesRequest(t *testing.T) {
	in := `{"model":"claude-x","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"let me think","signature":"sig_ABC-123+="},` +
		`{"type":"text","text":"ok"}]}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	input, _ := m["input"].([]any)
	// user msg, reasoning item, assistant message
	if len(input) != 3 {
		t.Fatalf("input items = %d, want 3: %s", len(input), out)
	}
	ri := asMap(input[1])
	if ri["type"] != "reasoning" {
		t.Fatalf("item[1] type = %v, want reasoning: %s", ri["type"], out)
	}
	if ri["encrypted_content"] != "sig_ABC-123+=" {
		t.Errorf("encrypted_content = %v, want verbatim signature", ri["encrypted_content"])
	}
	sum := asMap(asSlice(ri["summary"], 0))
	if sum["type"] != "summary_text" || sum["text"] != "let me think" {
		t.Errorf("summary = %v", sum)
	}
	// A thinking block without a signature gets no encrypted_content key.
	in2 := `{"model":"c","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t"}]}]}`
	out2, err := convertAnthropicRequestToResponses([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	ri2 := asMap(asSlice(unmarshalMap(t, out2)["input"], 0))
	if _, has := ri2["encrypted_content"]; has {
		t.Errorf("unsigned thinking must not gain encrypted_content: %v", ri2)
	}
}

// B2: request r→a — a reasoning item becomes an assistant thinking block with
// the signature preserved verbatim (a lossy signature 400s on the next turn).
func TestConvertReasoning_ResponsesToAnthropicRequest(t *testing.T) {
	in := `{"model":"gpt-x","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"let me think"}],"encrypted_content":"sig_ABC-123+="}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2: %s", len(msgs), out)
	}
	asst := asMap(msgs[1])
	if asst["role"] != "assistant" {
		t.Errorf("reasoning msg role = %v", asst["role"])
	}
	blk := asMap(asSlice(asst["content"], 0))
	if blk["type"] != "thinking" || blk["thinking"] != "let me think" {
		t.Errorf("thinking block = %v", blk)
	}
	if blk["signature"] != "sig_ABC-123+=" {
		t.Errorf("signature = %v, want verbatim sig_ABC-123+=", blk["signature"])
	}
}

// B3: non-streaming response r→a / r→o — an output reasoning item becomes an
// anthropic thinking block (with signature) / chat reasoning_content.
func TestConvertReasoning_ResponsesResponseOut(t *testing.T) {
	in := `{"id":"resp_1","status":"completed","model":"gpt-x","output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"sig_x"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],` +
		`"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`

	ant, err := convertResponsesToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := unmarshalMap(t, ant)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("anthropic content blocks = %d, want 2: %s", len(blocks), ant)
	}
	tb := asMap(blocks[0])
	if tb["type"] != "thinking" || tb["thinking"] != "thought" || tb["signature"] != "sig_x" {
		t.Errorf("r→a thinking block = %v", tb)
	}

	oai, err := convertResponsesToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	msg := asMap(asSlice(unmarshalMap(t, oai)["choices"], 0))
	msg = asMap(msg["message"])
	if msg["reasoning_content"] != "thought" {
		t.Errorf("r→o reasoning_content = %v", msg["reasoning_content"])
	}
	if msg["content"] != "answer" {
		t.Errorf("r→o content = %v", msg["content"])
	}
}

// B4: non-streaming response a→r / o→r — a thinking block / chat
// reasoning_content becomes a reasoning output item (signature preserved a→r).
func TestConvertReasoning_ResponseIntoResponses(t *testing.T) {
	ant := `{"id":"msg_1","model":"claude","stop_reason":"end_turn","content":[` +
		`{"type":"thinking","thinking":"thought","signature":"sig_x"},` +
		`{"type":"text","text":"answer"}],"usage":{"input_tokens":3,"output_tokens":5}}`
	out, err := convertAnthropicResponseToResponses([]byte(ant))
	if err != nil {
		t.Fatal(err)
	}
	items, _ := unmarshalMap(t, out)["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("a→r output items = %d, want 2: %s", len(items), out)
	}
	ri := asMap(items[0])
	if ri["type"] != "reasoning" || ri["encrypted_content"] != "sig_x" {
		t.Errorf("a→r reasoning item = %v", ri)
	}
	if strOf(asMap(asSlice(ri["summary"], 0))["text"]) != "thought" {
		t.Errorf("a→r reasoning summary = %v", ri["summary"])
	}

	oai := `{"id":"chatcmpl-1","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"answer","reasoning_content":"thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`
	out2, err := convertOpenAIResponseToResponses([]byte(oai))
	if err != nil {
		t.Fatal(err)
	}
	items2, _ := unmarshalMap(t, out2)["output"].([]any)
	if len(items2) != 2 {
		t.Fatalf("o→r output items = %d, want 2: %s", len(items2), out2)
	}
	// Current implementation emits the message item first, reasoning second
	// (content is flushed before reasoning_content is appended).
	ri2 := asMap(items2[1])
	if ri2["type"] != "reasoning" || strOf(asMap(asSlice(ri2["summary"], 0))["text"]) != "thought" {
		t.Errorf("o→r reasoning item = %v", ri2)
	}
}

// B5: chat direction request mapping. chat reasoning_content → responses
// reasoning item; responses reasoning item → chat reasoning_content.
func TestConvertReasoning_ChatRequest(t *testing.T) {
	in := `{"model":"gpt-x","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"answer","reasoning_content":"thought"}]}`
	out, err := convertOpenAIRequestToResponses([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := unmarshalMap(t, out)["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("o→r request input items = %d, want 3 (user, reasoning, message): %s", len(input), out)
	}
	ri := asMap(input[1])
	if ri["type"] != "reasoning" || strOf(asMap(asSlice(ri["summary"], 0))["text"]) != "thought" {
		t.Errorf("o→r request reasoning item = %v", ri)
	}

	in2 := `{"model":"gpt-x","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}]}`
	out2, err := convertResponsesRequestToOpenAI([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out2)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("r→o request messages = %d, want 2: %s", len(msgs), out2)
	}
	asst := asMap(msgs[1])
	if asst["role"] != "assistant" || asst["reasoning_content"] != "thought" {
		t.Errorf("r→o request reasoning message = %v", asst)
	}
}

// B6: thinking.budget_tokens ↔ reasoning.effort (best-effort approximation).
func TestConvertReasoning_EffortMapping(t *testing.T) {
	// a→r: budget thresholds — >=10000 high, >=5000 medium, >0 low.
	for budget, want := range map[int]string{10000: "high", 24000: "high", 5000: "medium", 9999: "medium", 1: "low", 4999: "low"} {
		in := `{"model":"c","max_tokens":10,"thinking":{"type":"enabled","budget_tokens":` + itoa(budget) + `},"messages":[{"role":"user","content":"hi"}]}`
		out, err := convertAnthropicRequestToResponses([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if got := strOf(asMap(unmarshalMap(t, out)["reasoning"])["effort"]); got != want {
			t.Errorf("budget %d → effort %q, want %q", budget, got, want)
		}
	}
	// thinking.type != "enabled" → no reasoning field.
	in := `{"model":"c","max_tokens":10,"thinking":{"type":"disabled","budget_tokens":10000},"messages":[{"role":"user","content":"hi"}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := unmarshalMap(t, out)["reasoning"]; has {
		t.Errorf("disabled thinking must not set reasoning: %s", out)
	}

	// r→a: effort → thinking config with the fixed budget ladder.
	in2 := `{"model":"gpt-x","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out2, err := convertResponsesRequestToAnthropic([]byte(in2), nil)
	if err != nil {
		t.Fatal(err)
	}
	th := asMap(unmarshalMap(t, out2)["thinking"])
	if th["type"] != "enabled" || th["budget_tokens"] != float64(24000) {
		t.Errorf("effort high → thinking = %v, want enabled/24000", th)
	}
}

// B-stream: responses-side reasoning deltas → anthropic thinking_delta /
// chat reasoning_content (the other half of the stream mapping).
func TestConvertReasoning_ResponsesStreamOut(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_0","summary":[]}}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"hmm"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_0","status":"completed"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"

	// r→a: thinking block with thinking_delta.
	eventsA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	assertEventSequence(t, eventsA, []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})
	startBlk := asMap(sseDataMap(t, eventsA[1])["content_block"])
	if startBlk["type"] != "thinking" {
		t.Errorf("r→a content_block = %v, want thinking", startBlk)
	}
	delta := asMap(sseDataMap(t, eventsA[2])["delta"])
	if delta["type"] != "thinking_delta" || delta["thinking"] != "hmm" {
		t.Errorf("r→a delta = %v", delta)
	}

	// r→o: delta.reasoning_content.
	eventsO := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "gpt-x"))
	chunks := sseFilter(eventsO, "chat.completion.chunk")
	foundReasoning := false
	for _, c := range chunks {
		ch := asMap(asSlice(sseDataMap(t, c)["choices"], 0))
		if d := asMap(ch["delta"]); d != nil && d["reasoning_content"] == "hmm" {
			foundReasoning = true
		}
	}
	if !foundReasoning {
		t.Errorf("r→o stream missing reasoning_content chunk: %v", sseEventTypes(eventsO))
	}
}

// ---------------------------------------------------------------------------
// E. redacted_thinking ↔ encrypted-only reasoning
// ---------------------------------------------------------------------------

// a→r request + response: a redacted_thinking block becomes a reasoning item
// carrying ONLY encrypted_content (no summary).
func TestConvertReasoning_RedactedThinking_IntoResponses(t *testing.T) {
	in := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"redacted_thinking","data":"enc_ABC+="},{"type":"text","text":"ok"}]}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	input, _ := unmarshalMap(t, out)["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input items = %d, want 3: %s", len(input), out)
	}
	ri := asMap(input[1])
	if ri["type"] != "reasoning" || ri["encrypted_content"] != "enc_ABC+=" {
		t.Errorf("a→r request redacted reasoning = %v", ri)
	}
	// codex requires summary even when empty (live-verified 400
	// "Missing required parameter: 'input[N].summary'").
	if sum, ok := ri["summary"].([]any); !ok || len(sum) != 0 {
		t.Errorf("redacted reasoning summary = %v, want present-but-empty (codex requires the key)", ri["summary"])
	}

	// a→r response side.
	ant := `{"id":"msg_1","stop_reason":"end_turn","content":[{"type":"redacted_thinking","data":"enc_ABC+="},{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	out2, err := convertAnthropicResponseToResponses([]byte(ant))
	if err != nil {
		t.Fatal(err)
	}
	ri2 := asMap(asSlice(unmarshalMap(t, out2)["output"], 0))
	if ri2["type"] != "reasoning" || ri2["encrypted_content"] != "enc_ABC+=" {
		t.Errorf("a→r response redacted reasoning = %v", ri2)
	}
	if sum, ok := ri2["summary"].([]any); !ok || len(sum) != 0 {
		t.Errorf("redacted reasoning summary = %v, want present-but-empty", ri2["summary"])
	}
}

// r→a request + response: a reasoning item with encrypted_content but an
// empty/missing summary maps back to redacted_thinking (data verbatim);
// one with a summary stays thinking+signature.
func TestConvertReasoning_RedactedThinking_FromResponses(t *testing.T) {
	// request side (first message is assistant → placeholder user is inserted;
	// find the redacted block anywhere).
	in := `{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"enc_ABC+="},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"sig_x"}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	var redacted, thinking map[string]any
	for _, m := range msgs {
		for _, b := range asMap(m)["content"].([]any) {
			bm := asMap(b)
			switch bm["type"] {
			case "redacted_thinking":
				redacted = bm
			case "thinking":
				thinking = bm
			}
		}
	}
	if redacted == nil || redacted["data"] != "enc_ABC+=" {
		t.Errorf("redacted_thinking block = %v, want data verbatim", redacted)
	}
	if thinking == nil || thinking["thinking"] != "thought" || thinking["signature"] != "sig_x" {
		t.Errorf("summarized reasoning must stay thinking+signature: %v", thinking)
	}

	// response side.
	rsp := `{"id":"r1","status":"completed","output":[{"type":"reasoning","summary":[],"encrypted_content":"enc_ABC+="}],"usage":{"input_tokens":1,"output_tokens":1}}`
	out2, err := convertResponsesToAnthropic([]byte(rsp))
	if err != nil {
		t.Fatal(err)
	}
	blk := asMap(asSlice(unmarshalMap(t, out2)["content"], 0))
	if blk["type"] != "redacted_thinking" || blk["data"] != "enc_ABC+=" {
		t.Errorf("r→a response redacted block = %v", blk)
	}
}

// Streaming redacted/encrypted handling, both directions.
func TestConvertReasoning_RedactedThinking_Stream(t *testing.T) {
	// a→r: a redacted_thinking content block → a reasoning item whose added
	// frame carries encrypted_content; added/done pair up.
	inAR := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"enc_ABC+="}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	eventsAR := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(inAR), "c"))
	assertResponsesItemPairing(t, eventsAR)
	added := asMap(sseDataMap(t, sseFilter(eventsAR, "response.output_item.added")[0])["item"])
	if added["type"] != "reasoning" || added["id"] != "rs_item_0" || added["encrypted_content"] != "enc_ABC+=" {
		t.Errorf("a→r stream redacted item = %v", added)
	}

	// r→a: reasoning item WITH summary deltas + encrypted_content → thinking
	// block closed by a signature_delta before content_block_stop.
	inRA := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_0","summary":[]}}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"hmm"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_0","status":"completed","encrypted_content":"sig_x"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"
	eventsRA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(inRA), "gpt-x"))
	assertEventSequence(t, eventsRA, []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop",
	})
	sigDelta := asMap(sseDataMap(t, eventsRA[3])["delta"])
	if sigDelta["type"] != "signature_delta" || sigDelta["signature"] != "sig_x" {
		t.Errorf("r→a stream signature delta = %v", sigDelta)
	}

	// r→a: encrypted-only reasoning (no summary deltas) → redacted_thinking block.
	inRA2 := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_0","summary":[]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_0","status":"completed","encrypted_content":"enc_ABC+="}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"
	eventsRA2 := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(inRA2), "gpt-x"))
	assertEventSequence(t, eventsRA2, []string{
		"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop",
	})
	blk := asMap(sseDataMap(t, eventsRA2[1])["content_block"])
	if blk["type"] != "redacted_thinking" || blk["data"] != "enc_ABC+=" {
		t.Errorf("r→a stream redacted block = %v", blk)
	}
}

// ---------------------------------------------------------------------------
// G. chat→anthropic reasoning
// ---------------------------------------------------------------------------

// chat→a request: reasoning_effort → thinking config (same best-effort
// ladder as the responses direction).
func TestConvertReasoning_ChatToAnthropic_Request(t *testing.T) {
	in := `{"model":"g","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	out, err := convertOpenAIRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	th := asMap(unmarshalMap(t, out)["thinking"])
	if th["type"] != "enabled" || th["budget_tokens"] != float64(24000) {
		t.Errorf("chat→a thinking = %v, want enabled/24000", th)
	}
	// No effort → no thinking key.
	out2, err := convertOpenAIRequestToAnthropic([]byte(`{"model":"g","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := unmarshalMap(t, out2)["thinking"]; has {
		t.Errorf("chat→a without effort must not set thinking: %s", out2)
	}
}

// chat→a non-stream response: reasoning_content → thinking block BEFORE text.
func TestConvertReasoning_ChatToAnthropic_Response(t *testing.T) {
	in := `{"id":"a","model":"g","choices":[{"message":{"role":"assistant","content":"answer","reasoning_content":"thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	out, err := convertOpenAIResponseToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := unmarshalMap(t, out)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2 (thinking + text): %s", len(blocks), out)
	}
	if asMap(blocks[0])["type"] != "thinking" || asMap(blocks[0])["thinking"] != "thought" {
		t.Errorf("blocks[0] = %v, want thinking first", blocks[0])
	}
	if asMap(blocks[1])["type"] != "text" || asMap(blocks[1])["text"] != "answer" {
		t.Errorf("blocks[1] = %v", blocks[1])
	}
}

// chat→a stream: reasoning_content deltas → thinking block with thinking_delta,
// switching cleanly to a text block when content starts.
func TestConvertReasoning_ChatToAnthropic_Stream(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think1\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think2\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "g"))
	assertEventSequence(t, events, []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})
	if got := strOf(asMap(sseDataMap(t, events[1])["content_block"])["type"]); got != "thinking" {
		t.Errorf("block 0 type = %q, want thinking", got)
	}
	if got := strOf(asMap(sseDataMap(t, events[2])["delta"])["thinking"]); got != "think1" {
		t.Errorf("thinking delta = %q", got)
	}
	if got := strOf(asMap(sseDataMap(t, events[5])["content_block"])["type"]); got != "text" {
		t.Errorf("block 1 type = %q, want text", got)
	}
}

// Review fix: a reasoning_details item with a signature but an EMPTY thinking
// text is skipped — replaying an empty thinking block can be rejected by
// Anthropic.
func TestConvertReasoning_ReplaySkipsEmptyThinking(t *testing.T) {
	msg := map[string]any{"reasoning_details": []any{
		map[string]any{"type": "anthropic_thinking", "thinking": "", "signature": "sig1"},
		map[string]any{"type": "anthropic_thinking", "thinking": "real", "signature": "sig2"},
	}}
	out := chatAnthropicThinkingReplay(msg)
	if len(out) != 1 || out[0]["thinking"] != "real" || out[0]["signature"] != "sig2" {
		t.Errorf("replay = %v, want only the non-empty signed block", out)
	}
}
