package protocol

import (
	"io"
	"strings"
	"testing"
)

func TestAnthropicCitationsToOpenAIProtocols(t *testing.T) {
	body := []byte(`{"id":"msg_cite","model":"claude","stop_reason":"end_turn","content":[
		{"type":"text","text":"据报道，"},
		{"type":"text","text":"天气晴朗","citations":[{
			"type":"web_search_result_location",
			"cited_text":"Sunny weather",
			"encrypted_index":"enc_1",
			"title":"Forecast",
			"url":"https://example.test/weather"
		}]}
	],"usage":{"input_tokens":2,"output_tokens":3}}`)

	responsesRaw, err := convertAnthropicResponseToResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	responses := unmarshalMap(t, responsesRaw)
	content := anySlice(asMap(anySlice(responses["output"])[0])["content"])
	if len(content) != 2 {
		t.Fatalf("a→r text parts = %#v", content)
	}
	annotation := asMap(anySlice(asMap(content[1])["annotations"])[0])
	if annotation["type"] != "url_citation" ||
		annotation["url"] != "https://example.test/weather" ||
		annotation["start_index"] != float64(0) ||
		annotation["end_index"] != float64(4) {
		t.Fatalf("a→r citation = %#v", annotation)
	}

	chatRaw, err := convertAnthropicResponseToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	chat := unmarshalMap(t, chatRaw)
	message := asMap(asMap(anySlice(chat["choices"])[0])["message"])
	chatCitation := asMap(asMap(anySlice(message["annotations"])[0])["url_citation"])
	if chatCitation["url"] != "https://example.test/weather" ||
		chatCitation["start_index"] != float64(4) ||
		chatCitation["end_index"] != float64(8) {
		t.Fatalf("a→chat citation = %#v", chatCitation)
	}
}

func TestChatAndResponsesCitationRoundTrip(t *testing.T) {
	chatBody := []byte(`{"id":"c1","model":"gpt","choices":[{"message":{
		"role":"assistant","content":"Forecast",
		"annotations":[{"type":"url_citation","url_citation":{
			"start_index":0,"end_index":8,"title":"Weather","url":"https://example.test/weather"
		}}]
	},"finish_reason":"stop"}],"usage":{}}`)
	responsesRaw, err := convertOpenAIResponseToResponses(chatBody)
	if err != nil {
		t.Fatal(err)
	}
	responses := unmarshalMap(t, responsesRaw)
	part := asMap(anySlice(asMap(anySlice(responses["output"])[0])["content"])[0])
	annotation := asMap(anySlice(part["annotations"])[0])
	if annotation["type"] != "url_citation" || annotation["url"] != "https://example.test/weather" {
		t.Fatalf("chat→r citation = %#v", annotation)
	}

	chatBackRaw, err := convertResponsesToOpenAI(responsesRaw)
	if err != nil {
		t.Fatal(err)
	}
	chatBack := unmarshalMap(t, chatBackRaw)
	message := asMap(asMap(anySlice(chatBack["choices"])[0])["message"])
	back := asMap(asMap(anySlice(message["annotations"])[0])["url_citation"])
	if back["title"] != "Weather" || back["start_index"] != float64(0) || back["end_index"] != float64(8) {
		t.Fatalf("r→chat citation = %#v", back)
	}
}

func TestResponsesCitationFallbackToAnthropic(t *testing.T) {
	body := []byte(`{"id":"r1","model":"gpt","status":"completed","output":[{
		"type":"message","role":"assistant","status":"completed","content":[{
			"type":"output_text","text":"Current forecast",
			"annotations":[{"type":"url_citation","start_index":0,"end_index":16,
				"title":"Weather","url":"https://example.test/weather"}]
		}]
	}],"usage":{}}`)
	anthropicRaw, err := convertResponsesToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	anthropic := unmarshalMap(t, anthropicRaw)
	block := asMap(anySlice(anthropic["content"])[0])
	text := strOpt(block["text"])
	if !strings.Contains(text, "Current forecast") ||
		!strings.Contains(text, "[Weather](https://example.test/weather)") {
		t.Fatalf("r→a citation fallback = %q", text)
	}
	if _, fabricated := block["citations"]; fabricated {
		t.Fatalf("r→a fabricated a structured citation without encrypted_index: %#v", block)
	}
}

func TestCitationReplayInRequests(t *testing.T) {
	anthropic := []byte(`{"model":"claude","max_tokens":32,"messages":[{
		"role":"assistant","content":[{"type":"text","text":"Weather",
			"citations":[{"type":"web_search_result_location","cited_text":"Sunny",
				"encrypted_index":"enc_1","title":"Forecast","url":"https://example.test/weather"}]}]
	},{"role":"user","content":"continue"}]}`)
	responsesRaw, err := convertAnthropicRequestToResponses(anthropic)
	if err != nil {
		t.Fatal(err)
	}
	responses := unmarshalMap(t, responsesRaw)
	first := asMap(anySlice(responses["input"])[0])
	part := asMap(anySlice(first["content"])[0])
	if len(anySlice(part["annotations"])) != 1 {
		t.Fatalf("a→r request citation = %s", responsesRaw)
	}

	backRaw, err := convertResponsesRequestToAnthropic(responsesRaw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backRaw), `[Forecast](https://example.test/weather)`) {
		t.Fatalf("r→a request citation fallback missing: %s", backRaw)
	}
}

func TestCitationStreamingMatrix(t *testing.T) {
	anthropicSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Weather"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","cited_text":"Sunny","encrypted_index":"enc_1","title":"Forecast","url":"https://example.test/weather"}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	toResponses, err := io.ReadAll(newAnthropicToResponsesSSE(strings.NewReader(anthropicSSE), "claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`response.output_text.annotation.added`, `"type":"url_citation"`, `"annotations":[`} {
		if !strings.Contains(string(toResponses), want) {
			t.Fatalf("a→r citation SSE missing %q:\n%s", want, toResponses)
		}
	}

	toChat, err := io.ReadAll(newAnthropicToOpenAISSE(strings.NewReader(anthropicSSE), "claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"annotations":[`, `"type":"url_citation"`, `https://example.test/weather`} {
		if !strings.Contains(string(toChat), want) {
			t.Fatalf("a→chat citation SSE missing %q:\n%s", want, toChat)
		}
	}

	responsesSSE := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"r1","model":"gpt","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"in_progress","content":[]}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Weather"}`,
		``,
		`event: response.output_text.annotation.added`,
		`data: {"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation_index":0,"annotation":{"type":"url_citation","start_index":0,"end_index":7,"title":"Forecast","url":"https://example.test/weather"}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Weather"}]}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"gpt","status":"completed","usage":{}}}`,
		``,
	}, "\n")

	toAnthropic, err := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(responsesSSE), "gpt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toAnthropic), `[Forecast](https://example.test/weather)`) {
		t.Fatalf("r→a citation SSE fallback missing:\n%s", toAnthropic)
	}

	toChatFromResponses, err := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(responsesSSE), "gpt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"annotations":[`, `"type":"url_citation"`, `https://example.test/weather`} {
		if !strings.Contains(string(toChatFromResponses), want) {
			t.Fatalf("r→chat citation SSE missing %q:\n%s", want, toChatFromResponses)
		}
	}

	chatSSE := strings.Join([]string{
		`data: {"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":"Weather"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"annotations":[{"type":"url_citation","url_citation":{"start_index":0,"end_index":7,"title":"Forecast","url":"https://example.test/weather"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"gpt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	toResponsesFromChat, err := io.ReadAll(newOpenAIToResponsesSSE(strings.NewReader(chatSSE), "gpt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`response.output_text.annotation.added`, `"annotations":[`, `https://example.test/weather`} {
		if !strings.Contains(string(toResponsesFromChat), want) {
			t.Fatalf("chat→r citation SSE missing %q:\n%s", want, toResponsesFromChat)
		}
	}
}
