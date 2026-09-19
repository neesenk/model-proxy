package protocol

import (
	"strings"
	"testing"
)

func TestStreamTerminalComplete(t *testing.T) {
	anthropicComplete := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hi\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	// The exact live failure shape (kimi-code 2026-09-14): thinking deltas,
	// then a bare message_stop — no content_block_stop, no message_delta.
	anthropicBareStop := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"partial\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	openAIComplete := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	openAITruncated := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"
	responsesComplete := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

	tests := []struct {
		name  string
		proto Protocol
		body  string
		want  bool
	}{
		{name: "anthropic complete", proto: Anthropic, body: anthropicComplete, want: true},
		{name: "anthropic bare message_stop", proto: Anthropic, body: anthropicBareStop, want: false},
		{name: "anthropic stop_reason without message_stop", proto: Anthropic, body: "event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}}\n\n", want: false},
		{name: "anthropic null stop_reason", proto: Anthropic, body: "event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null,\"stop_sequence\":null}}\n\n" +
			"event: message_stop\n" +
			"data: {\"type\":\"message_stop\"}\n\n", want: false},
		{name: "anthropic error event", proto: Anthropic, body: "event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: error\n" +
			"data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream stream terminated before a terminal event\"}}\n\n", want: false},
		{name: "anthropic empty", proto: Anthropic, body: "", want: false},
		{name: "openai complete", proto: OpenAI, body: openAIComplete, want: true},
		{name: "openai finish_reason without DONE", proto: OpenAI, body: "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", want: true},
		{name: "openai DONE only", proto: OpenAI, body: "data: [DONE]\n\n", want: true},
		{name: "openai truncated", proto: OpenAI, body: openAITruncated, want: false},
		{name: "openai error chunk", proto: OpenAI, body: "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"error\":{\"message\":\"boom\",\"type\":\"api_error\",\"param\":null,\"code\":null}}\n\n", want: false},
		{name: "responses completed", proto: Responses, body: responsesComplete, want: true},
		{name: "responses incomplete (max tokens)", proto: Responses, body: "event: response.incomplete\n" +
			"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n", want: true},
		{name: "responses payload type without event line", proto: Responses, body: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", want: true},
		{name: "responses failed", proto: Responses, body: "event: response.failed\n" +
			"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", want: false},
		{name: "responses truncated", proto: Responses, body: "event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n", want: false},
		{name: "unknown protocol", proto: Protocol("bogus"), body: anthropicComplete, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := StreamTerminalComplete(test.proto, []byte(test.body)); got != test.want {
				t.Fatalf("StreamTerminalComplete(%s) = %v, want %v", test.proto, got, test.want)
			}
		})
	}
}

// TestStreamTerminalComplete_ConvertedTruncation: a truncated anthropic
// upstream (bare message_stop, message_delta with an EMPTY stop_reason, or a
// non-empty stop_reason followed by EOF with no message_stop) must NOT become
// terminal-complete on the client side — the anthropic→chat and
// anthropic→responses converters fail closed (error chunk / response.failed)
// instead of synthesizing finish_reason:"stop" or response.completed, because
// the executor's cache gate checks exactly these converted bytes (caching a
// fake-clean terminal would poison every retry for the TTL). An explicit
// data: [DONE] delimiter (OpenRouter dialect) stays a clean terminal, matching
// the same-protocol openai gate.
func TestStreamTerminalComplete_ConvertedTruncation(t *testing.T) {
	const head = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	const bareStop = head +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	const emptyReason = head +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	const complete = head +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	const doneDelim = head + "data: [DONE]\n\n"
	const stopReasonEOF = head +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n" // no blank line / message_stop

	chat := func(t *testing.T, in string) []byte {
		t.Helper()
		return readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
	}
	resp := func(t *testing.T, in string) []byte {
		t.Helper()
		return readAllChecked(t, newAnthropicToResponsesSSE(strings.NewReader(in), "m"))
	}
	if got := StreamTerminalComplete(OpenAI, chat(t, bareStop)); got {
		t.Errorf("chat-converted bare message_stop judged complete — truncated stream would poison the cache:\n%s", chat(t, bareStop))
	}
	if got := StreamTerminalComplete(OpenAI, chat(t, emptyReason)); got {
		t.Errorf("chat-converted empty stop_reason judged complete:\n%s", chat(t, emptyReason))
	}
	if got := StreamTerminalComplete(OpenAI, chat(t, complete)); !got {
		t.Errorf("chat-converted complete stream judged incomplete:\n%s", chat(t, complete))
	}
	if got := StreamTerminalComplete(OpenAI, chat(t, doneDelim)); !got {
		t.Errorf("chat-converted explicit [DONE] delimiter judged incomplete (OpenRouter dialect is a clean terminal):\n%s", chat(t, doneDelim))
	}
	if got := StreamTerminalComplete(Responses, resp(t, bareStop)); got {
		t.Errorf("responses-converted bare message_stop judged complete — truncated stream would poison the cache:\n%s", resp(t, bareStop))
	}
	if got := StreamTerminalComplete(Responses, resp(t, emptyReason)); got {
		t.Errorf("responses-converted empty stop_reason judged complete:\n%s", resp(t, emptyReason))
	}
	if got := StreamTerminalComplete(Responses, resp(t, complete)); !got {
		t.Errorf("responses-converted complete stream judged incomplete:\n%s", resp(t, complete))
	}
	if got := StreamTerminalComplete(OpenAI, chat(t, stopReasonEOF)); got {
		t.Errorf("chat-converted message_delta(stop_reason) without message_stop judged complete:\n%s", chat(t, stopReasonEOF))
	}
	if got := StreamTerminalComplete(Responses, resp(t, stopReasonEOF)); got {
		t.Errorf("responses-converted message_delta(stop_reason) without message_stop judged complete:\n%s", resp(t, stopReasonEOF))
	}
}
