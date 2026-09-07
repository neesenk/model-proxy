package protocol

// convert_fixups_test.go — regression tests for three live-upstream-verified
// fixes: #8 placeholder reasoning_content (thinking-dialect upstreams 400 on
// bare tool_calls messages), #6 vision gating for media reinjection
// (deepseek 400s on image_url parts), #7 sequence_number on synthesized
// response.* frames.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// #8: thinking-dialect providers get a "tool call" placeholder on assistant
// tool_calls messages lacking reasoning_content; everything else untouched.
func TestConvertFixup_PlaceholderReasoningContent(t *testing.T) {
	// Codex shape: reasoning item with EMPTY summary + encrypted_content, so
	// the attached reasoning_content is exactly empty → placeholder kicks in.
	// Sequence: reasoning("real thought") attaches forward to the c1 assistant;
	// the tool message in between forces c2 into a NEW (bare) assistant.
	in := `{"model":"g","reasoning":{"effort":"high"},"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"enc"},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"real thought"}],"encrypted_content":"enc2"},` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"},` +
		`{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"}]}`
	out, err := convertResponsesRequestToOpenAIFor([]byte(in), convertReqOpts{ReasoningDialect: ReasoningThinking, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	var bareCalls, reasonedCalls, plainAssistant int
	for _, m := range msgs {
		mm := asMap(m)
		if mm["role"] != "assistant" {
			continue
		}
		tcs, _ := mm["tool_calls"].([]any)
		if len(tcs) == 0 {
			plainAssistant++
			if _, has := mm["reasoning_content"]; has {
				t.Errorf("assistant without tool_calls gained reasoning_content: %v", mm)
			}
			continue
		}
		rc := strOpt(mm["reasoning_content"])
		switch rc {
		case "tool call":
			bareCalls++
		case "real thought":
			reasonedCalls++
		default:
			t.Errorf("unexpected reasoning_content %q on %v", rc, mm)
		}
	}
	if bareCalls != 1 {
		t.Errorf("bare tool_calls message not injected (count=%d): %s", bareCalls, out)
	}
	if reasonedCalls != 1 {
		t.Errorf("existing reasoning_content overwritten (count=%d): %s", reasonedCalls, out)
	}

	// Non-thinking dialect: no injection.
	out2, err := convertResponsesRequestToOpenAIFor([]byte(in), convertReqOpts{ReasoningDialect: ReasoningEffort, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range unmarshalMap(t, out2)["messages"].([]any) {
		mm := asMap(m)
		if mm["role"] == "assistant" && strOpt(mm["reasoning_content"]) == "tool call" {
			t.Errorf("non-thinking provider got placeholder: %s", out2)
		}
	}
}

// #6 conversion: without vision, tool_result images fold into the placeholder
// text inside the tool message (no synthetic user message, no image parts).
func TestConvertFixup_VisionGateConversion(t *testing.T) {
	const noVision = "[image omitted: target model has no vision capability]"

	// a→chat.
	out, err := convertAnthropicRequestToOpenAIV([]byte(
		`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("no-vision messages = %d, want 1 (no synthetic): %s", len(msgs), out)
	}
	if got := strOpt(asMap(msgs[0])["content"]); got != noVision {
		t.Errorf("tool content = %q, want placeholder", got)
	}
	if strings.Contains(string(out), "image_url") {
		t.Errorf("image part leaked without vision: %s", out)
	}

	// a→r.
	out2, err := convertAnthropicRequestToResponsesV([]byte(
		`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	input2, _ := unmarshalMap(t, out2)["input"].([]any)
	if len(input2) != 1 {
		t.Fatalf("no-vision input items = %d, want 1: %s", len(input2), out2)
	}
	if got := strOpt(asMap(input2[0])["output"]); got != noVision {
		t.Errorf("fco output = %q, want placeholder", got)
	}

	// r→chat (parts-array output).
	out3, err := convertResponsesRequestToOpenAIFor([]byte(
		`{"model":"g","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"plot:"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`),
		convertReqOpts{ImageOK: false})
	if err != nil {
		t.Fatal(err)
	}
	msgs3, _ := unmarshalMap(t, out3)["messages"].([]any)
	if len(msgs3) != 1 {
		t.Fatalf("no-vision r→chat messages = %d, want 1: %s", len(msgs3), out3)
	}
	if got := strOpt(asMap(msgs3[0])["content"]); !strings.Contains(got, "plot:") || !strings.Contains(got, noVision) {
		t.Errorf("tool content = %q, want text + placeholder", got)
	}
	if strings.Contains(string(out3), "image_url") {
		t.Errorf("image part leaked without vision: %s", out3)
	}
}

// #7: synthesized response.* frames carry an incrementing sequence_number
// (0,1,2...) on every frame, both synthesis directions.
func TestConvertFixup_SequenceNumbers(t *testing.T) {
	check := func(name string, events []sseEvent) {
		t.Helper()
		for i, ev := range events {
			if ev.data == "[DONE]" {
				continue
			}
			m := unmarshalMap(t, []byte(ev.data))
			got, ok := m["sequence_number"].(float64)
			if !ok {
				t.Fatalf("%s: frame %d missing sequence_number: %s", name, i, ev.data)
			}
			if int(got) != i {
				t.Fatalf("%s: frame %d sequence_number = %v, want %d", name, i, got, i)
			}
		}
	}
	check("a→r", drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(anthropicTextStream), "c")))
	chatIn := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	check("chat→r", drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(chatIn), "g")))
}

// TestConvertErrorResponseDegradesUnrecognizedBodies pins the degradation
// contract: a committed upstream 4xx/5xx must ALWAYS be translatable to the
// client protocol. Codex rejects unsupported models with a FastAPI
// {"detail":...} envelope and some gateways answer with no body at all —
// failing closed there escalates a translatable 400 into an opaque
// "response conversion failed" 502 that hides the upstream diagnosis.
func TestConvertErrorResponseDegradesUnrecognizedBodies(t *testing.T) {
	detail := []byte(`{"detail":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."}`)
	html := []byte(`<html><head><title>502 Bad Gateway</title></head><body>upstream exploded</body></html>`)
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		want   string
	}{
		{"fastapi detail envelope", detail, 400, "not supported when using Codex"},
		{"empty body", nil, 400, "upstream request failed with status 400"},
		{"html body carries capped raw text", html, 502, "upstream exploded"},
	} {
		for _, client := range []string{"anthropic", "openai"} {
			out, err := convertErrorResponse(tc.body, client, "responses", tc.status)
			if err != nil {
				t.Errorf("%s client=%s: %v", tc.name, client, err)
				continue
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("%s client=%s: message lost: %s", tc.name, client, out)
			}
			if client == "anthropic" && !strings.Contains(string(out), `"type":"error"`) {
				t.Errorf("%s client=anthropic: no anthropic error envelope: %s", tc.name, out)
			}
		}
		// Same protocol stays byte passthrough (no synthesized envelope).
		if out, err := convertErrorResponse(tc.body, "responses", "responses", tc.status); err != nil || string(out) != string(tc.body) {
			t.Errorf("%s: same-protocol error body must pass through unchanged, got err=%v out=%s", tc.name, err, out)
		}
	}
}

// TestRawErrorMessageRuneBoundary pins the truncation contract: the capped
// message must stay valid UTF-8 (byte-slicing a multi-byte rune would not).
func TestRawErrorMessageRuneBoundary(t *testing.T) {
	body := []byte(`{"detail":"` + strings.Repeat("模型", 400) + `"}`) // 800 runes, multi-byte
	msg := rawErrorMessage(body, 400)
	if !utf8.ValidString(msg) {
		t.Fatal("rawErrorMessage produced invalid UTF-8")
	}
	runes := []rune(msg)
	if len(runes) > 501 { // 500 + ellipsis
		t.Errorf("message not capped: %d runes", len(runes))
	}
}
