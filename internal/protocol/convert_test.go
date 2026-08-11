package protocol

import (
	"io"
	"strings"
	"testing"
)

func TestConvertAnthropicRequestToOpenAI(t *testing.T) {
	in := []byte(`{"model":"claude-x","max_tokens":512,"system":"be brief","messages":[{"role":"user","content":"hi"}],"stop_sequences":["END"],"stream":true}`)
	out, err := convertAnthropicRequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// system becomes a leading system message (key order is map-dependent).
	if !strings.Contains(s, `"role":"system"`) || !strings.Contains(s, `"content":"be brief"`) {
		t.Errorf("system not converted to a system message: %s", s)
	}
	if !strings.Contains(s, `"role":"user"`) || !strings.Contains(s, `"content":"hi"`) {
		t.Errorf("user message not mapped: %s", s)
	}
	if !strings.Contains(s, `"stop":["END"]`) || strings.Contains(s, "stop_sequences") {
		t.Errorf("stop_sequences not converted to stop: %s", s)
	}
	if !strings.Contains(s, `"max_tokens":512`) || !strings.Contains(s, `"stream":true`) {
		t.Errorf("max_tokens/stream not carried through: %s", s)
	}
}

func TestConvertOpenAIRequestToAnthropic(t *testing.T) {
	in := []byte(`{"model":"gpt-x","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"stop":["END"]}`)
	out, err := convertOpenAIRequestToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"system":"sys"`) {
		t.Errorf("system message not lifted to top-level system: %s", s)
	}
	if !strings.Contains(s, `"max_tokens":`) {
		t.Errorf("max_tokens default not injected (Anthropic requires it): %s", s)
	}
	if !strings.Contains(s, `"stop_sequences":["END"]`) {
		t.Errorf("stop not converted to stop_sequences: %s", s)
	}
}

// TestConvertOpenAIRequestToAnthropic_ResponsesInputFailsClosed (bug 2): the
// OpenAI Responses API (/v1/responses) carries its payload in `input` (a list),
// not Chat Completions' `messages`. Responses is now its own "responses" protocol
// (dispatched to convert_responses.go), so a Responses body should not reach this
// chat-only converter via normal routing. The `input` guard remains a fail-closed
// defense: if a chat-completions body lacks `messages` but carries `input`, fail
// rather than silently emit an empty-messages Anthropic request (repo rule:
// conversion fail-closed).
func TestConvertOpenAIRequestToAnthropic_ResponsesInputFailsClosed(t *testing.T) {
	respBody := []byte(`{"model":"gpt-x","input":[{"role":"user","content":"hi"}],"stream":true}`)
	if out, err := convertOpenAIRequestToAnthropic(respBody); err == nil {
		t.Fatalf("expected fail-closed error for Responses `input` body, got nil; out=%s", out)
	}
	// A genuine Chat Completions body must still convert cleanly.
	chatBody := []byte(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := convertOpenAIRequestToAnthropic(chatBody); err != nil {
		t.Fatalf("chat-completions body must still convert: %v", err)
	}
}

func TestConvertOpenAIResponseToAnthropic(t *testing.T) {
	in := []byte(`{"id":"abc","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	out, err := convertOpenAIResponseToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"type":"message"`, `"text":"hello"`, `"stop_reason":"end_turn"`, `"input_tokens":10`, `"output_tokens":5`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}

func TestConvertAnthropicResponseToOpenAI(t *testing.T) {
	in := []byte(`{"id":"msg_xyz","model":"claude","stop_reason":"max_tokens","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":7}}`)
	out, err := convertAnthropicResponseToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"object":"chat.completion"`, `"content":"hi"`, `"finish_reason":"length"`, `"prompt_tokens":3`, `"completion_tokens":7`, `"total_tokens":10`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}

func TestOpenAIToAnthropicSSE(t *testing.T) {
	in := "data: {\"model\":\"gpt-x\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
		"data: {\"model\":\"gpt-x\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: {\"model\":\"gpt-x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	r := newOpenAIToAnthropicSSE(strings.NewReader(in), "gpt-x")
	out, _ := io.ReadAll(r)
	s := string(out)
	for _, want := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta", `"text":"hel"`, `"text":"lo"`, "event: content_block_stop", "event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in stream:\n%s", want, s)
		}
	}
}

func TestAnthropicToOpenAISSE(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	r := newAnthropicToOpenAISSE(strings.NewReader(in), "claude")
	out, _ := io.ReadAll(r)
	s := string(out)
	for _, want := range []string{`"object":"chat.completion.chunk"`, `"content":"hi"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in stream:\n%s", want, s)
		}
	}
}

// Review fix: multiple thinking blocks join with a separator instead of being
// glued together — response direction (reasoning_content).
func TestConvertAnthropicResponseToOpenAI_MultiThinkingSeparated(t *testing.T) {
	in := []byte(`{"id":"msg_1","model":"claude","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"t1"},{"type":"thinking","thinking":"t2"},{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	out, err := convertAnthropicResponseToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	msg := asMap(asMap(asSlice(unmarshalMap(t, out)["choices"], 0))["message"])
	if msg["reasoning_content"] != "t1\n\nt2" {
		t.Errorf("reasoning_content = %q, want %q", msg["reasoning_content"], "t1\n\nt2")
	}
}

// Review fix: same separator rule for the request direction (assistant
// history with multiple thinking blocks → chat reasoning_content).
func TestConvertAnthropicRequestToOpenAI_MultiThinkingSeparated(t *testing.T) {
	in := []byte(`{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"thinking","thinking":"t1"},{"type":"thinking","thinking":"t2"},{"type":"text","text":"a"}]}]}`)
	out, err := convertAnthropicRequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	var assistant map[string]any
	for _, m := range msgs {
		if asMap(m)["role"] == "assistant" {
			assistant = asMap(m)
		}
	}
	if assistant == nil || assistant["reasoning_content"] != "t1\n\nt2" {
		t.Fatalf("assistant reasoning_content = %v (%s)", assistant, out)
	}
}

// Review fix: chat→a with parts-array content keeps message-level annotations
// as appended source links — previously only the string-content path folded
// them in and the citations vanished.
func TestConvertOpenAIResponseToAnthropic_PartsContentAnnotations(t *testing.T) {
	in := []byte(`{"id":"a","model":"g","choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"annotations":[{"type":"url_citation","url_citation":{"url":"https://s.example","title":"S"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	out, err := convertOpenAIResponseToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := unmarshalMap(t, out)["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("content = %v, want 1 text block", blocks)
	}
	text := strOf(asMap(blocks[0])["text"])
	if !strings.Contains(text, "hello") || !strings.Contains(text, "Sources: [S](https://s.example)") {
		t.Errorf("text = %q, want content + appended source link", text)
	}
}
