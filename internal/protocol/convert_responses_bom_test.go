package protocol

import (
	"strings"
	"testing"
)

// TestResponsesDirection_LeadingBOMStripped: all four responses-direction
// streaming converters (r→a, r→chat, a→r, chat→r) must tolerate a single
// leading UTF-8 BOM on the first SSE line, matching the anthropic↔chat
// directions.
func TestResponsesDirection_LeadingBOMStripped(t *testing.T) {
	const bom = "\ufeff"

	// responses → anthropic: BOM before the first response.* event line.
	rToAIn := bom + "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","object":"response","status":"in_progress","model":"m","output":[]}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	rToAOut := string(readAllChecked(t, newResponsesToAnthropicSSE(strings.NewReader(rToAIn), "m")))
	if !strings.Contains(rToAOut, "event: message_stop") || strings.Contains(rToAOut, "terminated before a terminal event") {
		t.Errorf("r→a did not tolerate leading BOM:\n%s", rToAOut)
	}

	// responses → chat: BOM before the first response.* event line.
	rToChatIn := bom + "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","object":"response","status":"in_progress","model":"m","output":[]}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	rToChatOut := string(readAllChecked(t, newResponsesToOpenAISSE(strings.NewReader(rToChatIn), "m")))
	if !strings.Contains(rToChatOut, `"content":"hi"`) || !strings.Contains(rToChatOut, `"finish_reason":"stop"`) || strings.Contains(rToChatOut, "terminated before a terminal event") {
		t.Errorf("r→chat did not tolerate leading BOM:\n%s", rToChatOut)
	}

	// anthropic → responses: BOM before the first anthropic event line.
	aToRIn := bom + "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	aToROut := string(readAllChecked(t, newAnthropicToResponsesSSE(strings.NewReader(aToRIn), "m")))
	if !strings.Contains(aToROut, "response.completed") || strings.Contains(aToROut, "response.failed") {
		t.Errorf("a→r did not tolerate leading BOM:\n%s", aToROut)
	}

	// chat → responses: BOM before the first chat completion chunk.
	chatToRIn := bom + "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	chatToROut := string(readAllChecked(t, newOpenAIToResponsesSSE(strings.NewReader(chatToRIn), "m")))
	if !strings.Contains(chatToROut, "response.completed") || strings.Contains(chatToROut, "response.failed") {
		t.Errorf("chat→r did not tolerate leading BOM:\n%s", chatToROut)
	}
}
