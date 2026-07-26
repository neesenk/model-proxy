package main

import (
	"io"
	"strings"
	"testing"

	sonic "github.com/bytedance/sonic"
)

func TestAnthropicThinkingReplaysThroughChatRequest(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":128,"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"Need current conditions.","signature":"sig_123"},
			{"type":"tool_use","id":"call_1","name":"weather","input":{"city":"SG"}}
		]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}
	]}`)
	raw, err := convertAnthropicRequestToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	out := unmarshalMap(t, raw)
	assistant := asMap(anySlice(out["messages"])[1])
	if assistant["reasoning_content"] != "Need current conditions." {
		t.Fatalf("a→chat reasoning replay = %s", raw)
	}
	if len(anySlice(assistant["tool_calls"])) != 1 {
		t.Fatalf("a→chat tool call missing = %s", raw)
	}
}

func TestSignedAnthropicReasoningEnvelopeRoundTripViaChat(t *testing.T) {
	anthropicResponse := []byte(`{"id":"msg_1","model":"claude","stop_reason":"tool_use","content":[
		{"type":"redacted_thinking","data":"opaque_1"},
		{"type":"thinking","thinking":"Need current conditions.","signature":"sig_123"},
		{"type":"tool_use","id":"call_1","name":"weather","input":{"city":"SG"}}
	],"usage":{}}`)
	chatRaw, err := convertAnthropicResponseToOpenAI(anthropicResponse)
	if err != nil {
		t.Fatal(err)
	}
	chat := unmarshalMap(t, chatRaw)
	message := asMap(asMap(anySlice(chat["choices"])[0])["message"])
	if message["reasoning_content"] != "Need current conditions." || len(anySlice(message["reasoning_details"])) != 2 {
		t.Fatalf("a→chat reasoning envelope = %s", chatRaw)
	}

	replayRequest := map[string]any{
		"model": "claude", "messages": []any{
			map[string]any{"role": "user", "content": "weather?"},
			message,
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
		},
	}
	requestRaw, _ := jsonMarshal(replayRequest)
	backRaw, err := convertOpenAIRequestToAnthropic(requestRaw)
	if err != nil {
		t.Fatal(err)
	}
	back := unmarshalMap(t, backRaw)
	messages := anySlice(back["messages"])
	assistant := asMap(messages[1])
	blocks := anySlice(assistant["content"])
	if len(blocks) < 3 ||
		asMap(blocks[0])["type"] != "redacted_thinking" ||
		asMap(blocks[0])["data"] != "opaque_1" ||
		asMap(blocks[1])["type"] != "thinking" ||
		asMap(blocks[1])["signature"] != "sig_123" ||
		asMap(blocks[2])["type"] != "tool_use" {
		t.Fatalf("chat→a signed reasoning replay = %s", backRaw)
	}
}

func TestAnthropicThinkingReplayStreamingToChat(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Need weather."}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_123"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	raw, err := io.ReadAll(newAnthropicToOpenAISSE(strings.NewReader(stream), "claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reasoning_content":"Need weather."`, `"type":"anthropic_thinking"`, `"signature":"sig_123"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("a→chat reasoning SSE missing %q:\n%s", want, raw)
		}
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return sonic.Marshal(v)
}
