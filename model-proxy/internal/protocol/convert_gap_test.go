package protocol

// convert_gap_test.go — tests for the three-way conversion audit mappings:
// usage details (A), stop_reason/content_filter semantics (B), server-tool
// filtering (C), first-user placeholder (D), max_completion_tokens (F),
// response_format ↔ text.format (H), parallel_tool_calls on responses
// directions (I). Reasoning-related gaps (E redacted_thinking, G chat→a
// reasoning) live in convert_reasoning_test.go; warn coverage (J) in
// convert_fault_test.go. Section K covers the cc-switch parity fixes on the
// anthropic↔openai-chat pair: single-image-part preservation, system/tool
// content extraction + "\n" joining, refusal → text, cache_write split, and
// empty-choices fail-closed.

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A. usage details
// ---------------------------------------------------------------------------

// a→r / chat→r non-stream: cache + reasoning token details map into
// input/output_tokens_details (OpenAI-inclusive convention).
func TestConvertUsageDetails_IntoResponses(t *testing.T) {
	// a→r: input = input + cache_read + cache_creation; cached → details.
	ant := `{"id":"msg_1","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":3,"output_tokens":7,"cache_read_input_tokens":2,"cache_creation_input_tokens":5}}`
	out, err := convertAnthropicResponseToResponses([]byte(ant))
	if err != nil {
		t.Fatal(err)
	}
	u := asMap(unmarshalMap(t, out)["usage"])
	if u["input_tokens"] != float64(10) || u["output_tokens"] != float64(7) || u["total_tokens"] != float64(17) {
		t.Errorf("a→r usage = %v, want 10/7/17", u)
	}
	if got := objOf(t, u, "input_tokens_details", "cached_tokens"); got != float64(2) {
		t.Errorf("a→r cached_tokens = %v, want 2", got)
	}

	// chat→r: prompt/completion details pass through.
	oai := `{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":3}}}`
	out2, err := convertOpenAIResponseToResponses([]byte(oai))
	if err != nil {
		t.Fatal(err)
	}
	u2 := asMap(unmarshalMap(t, out2)["usage"])
	if got := objOf(t, u2, "input_tokens_details", "cached_tokens"); got != float64(4) {
		t.Errorf("chat→r cached_tokens = %v, want 4", got)
	}
	if got := objOf(t, u2, "output_tokens_details", "reasoning_tokens"); got != float64(3) {
		t.Errorf("chat→r reasoning_tokens = %v, want 3", got)
	}
}

// r→a / r→chat non-stream: details map back (input = input − cached, clamped).
func TestConvertUsageDetails_FromResponses(t *testing.T) {
	mk := func(cached int) string {
		return `{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
			`"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":` + itoa(cached) + `},"output_tokens_details":{"reasoning_tokens":3}}}`
	}
	ant, err := convertResponsesToAnthropic([]byte(mk(4)))
	if err != nil {
		t.Fatal(err)
	}
	u := asMap(unmarshalMap(t, ant)["usage"])
	if u["input_tokens"] != float64(6) || u["cache_read_input_tokens"] != float64(4) || u["output_tokens"] != float64(5) {
		t.Errorf("r→a usage = %v, want input=6 cache_read=4 output=5", u)
	}

	// Clamp: cached > input → input 0 (cache_read still verbatim).
	ant2, err := convertResponsesToAnthropic([]byte(mk(12)))
	if err != nil {
		t.Fatal(err)
	}
	u2 := asMap(unmarshalMap(t, ant2)["usage"])
	if u2["input_tokens"] != float64(0) || u2["cache_read_input_tokens"] != float64(12) {
		t.Errorf("r→a clamp usage = %v, want input=0 cache_read=12", u2)
	}

	oai, err := convertResponsesToOpenAI([]byte(mk(4)))
	if err != nil {
		t.Fatal(err)
	}
	u3 := asMap(unmarshalMap(t, oai)["usage"])
	if got := objOf(t, u3, "prompt_tokens_details", "cached_tokens"); got != float64(4) {
		t.Errorf("r→chat cached_tokens = %v, want 4", got)
	}
	if got := objOf(t, u3, "completion_tokens_details", "reasoning_tokens"); got != float64(3) {
		t.Errorf("r→chat reasoning_tokens = %v, want 3", got)
	}
}

// Streaming usage details, all four responses directions.
func TestConvertUsageDetails_Stream(t *testing.T) {
	// r→a: response.completed usage details → message_delta usage split.
	inRA := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}}}}` + "\n\n"
	eventsRA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(inRA), "gpt-x"))
	md := sseFilter(eventsRA, "message_delta")
	if len(md) != 1 {
		t.Fatalf("r→a: message_delta count = %d", len(md))
	}
	uRA := asMap(sseDataMap(t, md[0])["usage"])
	if uRA["input_tokens"] != float64(6) || uRA["cache_read_input_tokens"] != float64(4) {
		t.Errorf("r→a stream usage = %v, want input=6 cache_read=4", uRA)
	}

	// r→chat: details on the finish chunk.
	eventsRC := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(inRA), "gpt-x"))
	var uRC map[string]any
	for _, c := range sseFilter(eventsRC, "chat.completion.chunk") {
		if u := asMap(sseDataMap(t, c)["usage"]); u != nil {
			uRC = u
		}
	}
	if uRC == nil {
		t.Fatal("r→chat stream: no usage chunk")
	}
	if objOf(t, uRC, "prompt_tokens_details", "cached_tokens") != float64(4) {
		t.Errorf("r→chat stream cached = %v", uRC)
	}

	// a→r: message_start cache usage + message_delta output → inclusive input.
	inAR := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":5}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	eventsAR := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(inAR), "c"))
	completedAR := asMap(sseDataMap(t, sseFilter(eventsAR, "response.completed")[0])["response"])
	uAR := asMap(completedAR["usage"])
	if uAR["input_tokens"] != float64(10) || uAR["output_tokens"] != float64(7) {
		t.Errorf("a→r stream usage = %v, want 10/7 (input inclusive of cache)", uAR)
	}
	if objOf(t, uAR, "input_tokens_details", "cached_tokens") != float64(2) {
		t.Errorf("a→r stream cached = %v", uAR)
	}

	// chat→r: usage chunk details → response.completed details.
	inCR := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n"
	eventsCR := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(inCR), "gpt-x"))
	completedCR := asMap(sseDataMap(t, sseFilter(eventsCR, "response.completed")[0])["response"])
	uCR := asMap(completedCR["usage"])
	if objOf(t, uCR, "input_tokens_details", "cached_tokens") != float64(4) {
		t.Errorf("chat→r stream cached = %v", uCR)
	}
	if objOf(t, uCR, "output_tokens_details", "reasoning_tokens") != float64(3) {
		t.Errorf("chat→r stream reasoning = %v", uCR)
	}
}

// ---------------------------------------------------------------------------
// B. stop_reason / content_filter semantics
// ---------------------------------------------------------------------------

func TestConvertStopSemantics_ContentFilter(t *testing.T) {
	// chat→a non-stream: content_filter → refusal.
	oaiResp := `{"id":"a","model":"g","choices":[{"message":{"role":"assistant","content":"no"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	ant, err := convertOpenAIResponseToAnthropic([]byte(oaiResp))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, ant)["stop_reason"]; got != "refusal" {
		t.Errorf("chat→a stop_reason = %v, want refusal", got)
	}

	// chat→a stream: same via the terminal message_delta.
	streamOA := "data: {\"choices\":[{\"delta\":{\"content\":\"no\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\ndata: [DONE]\n\n"
	eventsOA := drainSSE(t, newOpenAIToAnthropicSSE(strings.NewReader(streamOA), "g"))
	mdOA := sseFilter(eventsOA, "message_delta")
	if got := strOf(asMap(sseDataMap(t, mdOA[0])["delta"])["stop_reason"]); got != "refusal" {
		t.Errorf("chat→a stream stop_reason = %q, want refusal", got)
	}

	// chat→r non-stream: content_filter → incomplete + incomplete_details.
	rOut, err := convertOpenAIResponseToResponses([]byte(oaiResp))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, rOut)
	if m["status"] != "incomplete" || strOf(asMap(m["incomplete_details"])["reason"]) != "content_filter" {
		t.Errorf("chat→r status/details = %v/%v", m["status"], m["incomplete_details"])
	}

	// chat→r stream: response.incomplete event (not response.completed).
	streamCR := "data: {\"choices\":[{\"delta\":{\"content\":\"no\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\ndata: [DONE]\n\n"
	eventsCR := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(streamCR), "g"))
	if got := sseCount(eventsCR, "response.completed"); got != 0 {
		t.Errorf("chat→r content_filter: response.completed count = %d, want 0", got)
	}
	inc := sseFilter(eventsCR, "response.incomplete")
	if len(inc) != 1 {
		t.Fatalf("chat→r content_filter: response.incomplete count = %d, want 1: %v", len(inc), sseEventTypes(eventsCR))
	}
	resp := asMap(sseDataMap(t, inc[0])["response"])
	if resp["status"] != "incomplete" || strOf(asMap(resp["incomplete_details"])["reason"]) != "content_filter" {
		t.Errorf("chat→r incomplete response = %v", resp)
	}

	// a→chat non-stream + stream: refusal → content_filter.
	antResp := `{"id":"msg_x","model":"c","stop_reason":"refusal","content":[{"type":"text","text":"no"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	oai2, err := convertAnthropicResponseToOpenAI([]byte(antResp))
	if err != nil {
		t.Fatal(err)
	}
	ch := asMap(asSlice(unmarshalMap(t, oai2)["choices"], 0))
	if ch["finish_reason"] != "content_filter" {
		t.Errorf("a→chat finish_reason = %v, want content_filter", ch["finish_reason"])
	}

	// a→r non-stream + stream: refusal → incomplete + content_filter.
	rOut2, err := convertAnthropicResponseToResponses([]byte(antResp))
	if err != nil {
		t.Fatal(err)
	}
	m2 := unmarshalMap(t, rOut2)
	if m2["status"] != "incomplete" || strOf(asMap(m2["incomplete_details"])["reason"]) != "content_filter" {
		t.Errorf("a→r refusal status = %v/%v", m2["status"], m2["incomplete_details"])
	}
	streamAR := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"refusal"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	eventsAR := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(streamAR), "c"))
	incAR := sseFilter(eventsAR, "response.incomplete")
	if len(incAR) != 1 || sseCount(eventsAR, "response.completed") != 0 {
		t.Fatalf("a→r refusal stream: incomplete=%d: %v", len(incAR), sseEventTypes(eventsAR))
	}
}

// r→{a,chat}: incomplete_details.reason selects max_tokens/length vs
// refusal/content_filter (non-stream and stream).
func TestConvertStopSemantics_IncompleteReasons(t *testing.T) {
	mk := func(reason string) string {
		details := ""
		if reason != "" {
			details = `,"incomplete_details":{"reason":"` + reason + `"}`
		}
		return `{"id":"r1","status":"incomplete"` + details + `,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	}
	// max_output_tokens → max_tokens / length.
	ant, _ := convertResponsesToAnthropic([]byte(mk("max_output_tokens")))
	if got := unmarshalMap(t, ant)["stop_reason"]; got != "max_tokens" {
		t.Errorf("r→a max_output_tokens stop = %v", got)
	}
	oai, _ := convertResponsesToOpenAI([]byte(mk("max_output_tokens")))
	if got := objOf(t, mustJSON(t, oai), "choices"); asMap(asSlice(got, 0))["finish_reason"] != "length" {
		t.Errorf("r→chat max_output_tokens finish = %v", got)
	}
	// content_filter → refusal / content_filter.
	ant2, _ := convertResponsesToAnthropic([]byte(mk("content_filter")))
	if got := unmarshalMap(t, ant2)["stop_reason"]; got != "refusal" {
		t.Errorf("r→a content_filter stop = %v, want refusal", got)
	}
	oai2, _ := convertResponsesToOpenAI([]byte(mk("content_filter")))
	if got := asMap(asSlice(unmarshalMap(t, oai2)["choices"], 0))["finish_reason"]; got != "content_filter" {
		t.Errorf("r→chat content_filter finish = %v", got)
	}
	// No reason → default max_tokens / length (locked current default).
	ant3, _ := convertResponsesToAnthropic([]byte(mk("")))
	if got := unmarshalMap(t, ant3)["stop_reason"]; got != "max_tokens" {
		t.Errorf("r→a bare incomplete stop = %v", got)
	}

	// Stream: response.incomplete with reason content_filter.
	in := "event: response.incomplete\n" +
		`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}` + "\n\n"
	eventsA := drainSSE(t, newResponsesToAnthropicSSE(strings.NewReader(in), "g"))
	md := sseFilter(eventsA, "message_delta")
	if got := strOf(asMap(sseDataMap(t, md[0])["delta"])["stop_reason"]); got != "refusal" {
		t.Errorf("r→a stream content_filter stop = %q, want refusal", got)
	}
	eventsO := drainSSE(t, newResponsesToOpenAISSE(strings.NewReader(in), "g"))
	fr := ""
	for _, c := range sseFilter(eventsO, "chat.completion.chunk") {
		if s, ok := asMap(asSlice(sseDataMap(t, c)["choices"], 0))["finish_reason"].(string); ok && s != "" {
			fr = s
		}
	}
	if fr != "content_filter" {
		t.Errorf("r→chat stream finish = %q, want content_filter", fr)
	}
}

// r→{a,chat} non-stream: status failed/cancelled with an error object is a
// conversion ERROR (fail-closed → 502 at the forward layer), never a fake
// end_turn/stop message.
func TestConvertStopSemantics_FailedFailClosed(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		body := `{"id":"r1","status":"` + status + `","error":{"code":"server_error","message":"boom"},"output":[]}`
		if out, err := convertResponsesToAnthropic([]byte(body)); err == nil {
			t.Errorf("r→a %s: expected conversion error, got %s", status, out)
		}
		if out, err := convertResponsesToOpenAI([]byte(body)); err == nil {
			t.Errorf("r→chat %s: expected conversion error, got %s", status, out)
		}
	}
}

// a→chat: pause_turn has no chat equivalent — best-effort stop.
func TestConvertStopSemantics_PauseTurn(t *testing.T) {
	ant := `{"id":"msg_x","stop_reason":"pause_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	oai, err := convertAnthropicResponseToOpenAI([]byte(ant))
	if err != nil {
		t.Fatal(err)
	}
	if got := asMap(asSlice(unmarshalMap(t, oai)["choices"], 0))["finish_reason"]; got != "stop" {
		t.Errorf("a→chat pause_turn finish = %v, want stop (best-effort)", got)
	}
	// a→r: pause_turn → completed (best-effort).
	r, err := convertAnthropicResponseToResponses([]byte(ant))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, r)["status"]; got != "completed" {
		t.Errorf("a→r pause_turn status = %v, want completed (best-effort)", got)
	}
}

// ---------------------------------------------------------------------------
// C. a→r server tools filtering
// ---------------------------------------------------------------------------

func TestConvertRequest_HostedWebSearchPreservedResponses(t *testing.T) {
	in := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"},` +
		`{"type":"computer_20250124","name":"computer","display_width_px":1024},` +
		`{"name":"get_weather","input_schema":{"type":"object"}}]}`
	var out []byte
	logs := captureConvertLog(t, func() {
		var err error
		out, err = convertAnthropicRequestToResponses([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
	})
	tools, _ := unmarshalMap(t, out)["tools"].([]any)
	if len(tools) != 2 || asMap(tools[0])["type"] != "web_search" || asMap(tools[1])["name"] != "get_weather" {
		t.Errorf("tools = %v, want hosted web_search plus custom function", tools)
	}
	if strings.Contains(logs, "web_search_20250305") {
		t.Errorf("supported web_search was warned as dropped, log = %q", logs)
	}
	if !strings.Contains(logs, "dropping server-side anthropic tool type: computer_20250124") {
		t.Errorf("computer tool not warned, log = %q", logs)
	}
}

// ---------------------------------------------------------------------------
// D. r→a first-message-user placeholder
// ---------------------------------------------------------------------------

func TestConvertRequest_ResponsesToAnthropic_FirstUserPlaceholder(t *testing.T) {
	// input starting with a function_call would otherwise produce a leading
	// assistant message (anthropic 400s).
	in := `{"model":"gpt-x","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (placeholder + tool_use): %s", len(msgs), out)
	}
	first := asMap(msgs[0])
	if first["role"] != "user" || strOf(asMap(asSlice(first["content"], 0))["text"]) != "." {
		t.Errorf("placeholder message = %v", first)
	}
	if asMap(msgs[1])["role"] != "assistant" {
		t.Errorf("msgs[1] role = %v", asMap(msgs[1])["role"])
	}

	// A leading user message gets NO placeholder (unchanged behavior).
	in2 := `{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out2, err := convertResponsesRequestToAnthropic([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	if msgs2, _ := unmarshalMap(t, out2)["messages"].([]any); len(msgs2) != 1 {
		t.Errorf("leading-user messages = %d, want 1 (no placeholder): %s", len(msgs2), out2)
	}
}

// ---------------------------------------------------------------------------
// F. max_completion_tokens
// ---------------------------------------------------------------------------

func TestConvertRequest_MaxCompletionTokens(t *testing.T) {
	// chat→a: max_completion_tokens wins over max_tokens; alone it still maps.
	both := `{"model":"g","max_tokens":100,"max_completion_tokens":50,"messages":[{"role":"user","content":"hi"}]}`
	out, err := convertOpenAIRequestToAnthropic([]byte(both))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out)["max_tokens"]; got != float64(50) {
		t.Errorf("chat→a max_tokens = %v, want 50 (max_completion_tokens wins)", got)
	}
	only := `{"model":"g","max_completion_tokens":77,"messages":[{"role":"user","content":"hi"}]}`
	out2, err := convertOpenAIRequestToAnthropic([]byte(only))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out2)["max_tokens"]; got != float64(77) {
		t.Errorf("chat→a max_tokens = %v, want 77", got)
	}

	// chat→r: same precedence into max_output_tokens.
	out3, err := convertOpenAIRequestToResponses([]byte(both))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out3)["max_output_tokens"]; got != float64(50) {
		t.Errorf("chat→r max_output_tokens = %v, want 50", got)
	}
}

// ---------------------------------------------------------------------------
// H. response_format ↔ text.format
// ---------------------------------------------------------------------------

func TestConvertRequest_ResponseFormatTextFormat(t *testing.T) {
	// chat→r: json_object.
	out, err := convertOpenAIRequestToResponses([]byte(
		`{"model":"g","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := objOf(t, mustJSON(t, out), "text", "format", "type"); got != "json_object" {
		t.Errorf("chat→r text.format = %v, want json_object", got)
	}

	// chat→r: json_schema unwrapped.
	out2, err := convertOpenAIRequestToResponses([]byte(
		`{"model":"g","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"S","schema":{"type":"object"},"strict":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	f := objOf(t, mustJSON(t, out2), "text", "format").(map[string]any)
	if f["type"] != "json_schema" || f["name"] != "S" || f["strict"] != true || asMap(f["schema"])["type"] != "object" {
		t.Errorf("chat→r text.format json_schema = %v", f)
	}

	// r→chat: reverse.
	out3, err := convertResponsesRequestToOpenAI([]byte(
		`{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"text":{"format":{"type":"json_schema","name":"S","schema":{"type":"object"},"strict":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	rf := asMap(unmarshalMap(t, out3)["response_format"])
	if rf["type"] != "json_schema" || objOf(t, rf, "json_schema", "name") != "S" || objOf(t, rf, "json_schema", "strict") != true {
		t.Errorf("r→chat response_format = %v", rf)
	}
	out4, err := convertResponsesRequestToOpenAI([]byte(
		`{"model":"g","input":[],"text":{"format":{"type":"json_object"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strOf(asMap(unmarshalMap(t, out4)["response_format"])["type"]); got != "json_object" {
		t.Errorf("r→chat response_format = %v, want json_object", got)
	}

	// r→a: no anthropic equivalent — dropped + warned.
	var out5 []byte
	logs := captureConvertLog(t, func() {
		var err error
		out5, err = convertResponsesRequestToAnthropic([]byte(
			`{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"text":{"format":{"type":"json_object"}}}`))
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(logs, "dropping text.format") {
		t.Errorf("r→a text.format not warned, log = %q", logs)
	}
	if strings.Contains(string(out5), "json_object") {
		t.Errorf("r→a text.format leaked: %s", out5)
	}
}

// ---------------------------------------------------------------------------
// I. parallel_tool_calls (responses four directions)
// ---------------------------------------------------------------------------

func TestConvertRequest_ParallelToolCallsResponses(t *testing.T) {
	// a→r: disable_parallel_tool_use:true → parallel_tool_calls:false.
	out, err := convertAnthropicRequestToResponses([]byte(
		`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out)["parallel_tool_calls"]; got != false {
		t.Errorf("a→r parallel_tool_calls = %v, want false", got)
	}

	// chat→r: passthrough.
	out2, err := convertOpenAIRequestToResponses([]byte(
		`{"model":"g","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out2)["parallel_tool_calls"]; got != false {
		t.Errorf("chat→r parallel_tool_calls = %v, want false", got)
	}

	// r→a: false → disable_parallel_tool_use (synthesized {type:auto} when the
	// client gave no tool_choice; tools present).
	out3, err := convertResponsesRequestToAnthropic([]byte(
		`{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"parallel_tool_calls":false}`))
	if err != nil {
		t.Fatal(err)
	}
	tc := asMap(unmarshalMap(t, out3)["tool_choice"])
	if tc["type"] != "auto" || tc["disable_parallel_tool_use"] != true {
		t.Errorf("r→a tool_choice = %v, want {auto, disable_parallel_tool_use}", tc)
	}
	// r→a: without tools → no synthesized tool_choice.
	out4, err := convertResponsesRequestToAnthropic([]byte(
		`{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"parallel_tool_calls":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := unmarshalMap(t, out4)["tool_choice"]; has {
		t.Errorf("r→a without tools must not synthesize tool_choice: %s", out4)
	}

	// r→chat: passthrough (tools present — without tools the field is dropped
	// per the #3557 guard, covered in convert_parity_test.go).
	out5, err := convertResponsesRequestToOpenAI([]byte(
		`{"model":"g","input":[],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"parallel_tool_calls":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out5)["parallel_tool_calls"]; got != false {
		t.Errorf("r→chat parallel_tool_calls = %v, want false", got)
	}
}

// ---------------------------------------------------------------------------
// P0 遗留缺陷（2026-07 盘点）
// ---------------------------------------------------------------------------

// P0-1: OpenRouter-style reasoning items carry text in content parts
// ({type:"reasoning_text"}), not summary — non-stream r→{a,chat} must fall
// back to content (the stream path already handles reasoning_text.delta).
func TestConvertGap_ReasoningTextContentFallback(t *testing.T) {
	resp := `{"id":"r1","status":"completed","output":[` +
		`{"type":"reasoning","content":[{"type":"reasoning_text","text":"deep thought"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":2}}`

	// r→anthropic non-stream: thinking block with the content text.
	outA, err := convertResponsesToAnthropic([]byte(resp))
	if err != nil {
		t.Fatalf("r→a: %v", err)
	}
	if !strings.Contains(string(outA), `"type":"thinking"`) || !strings.Contains(string(outA), "deep thought") {
		t.Errorf("r→a lost reasoning_text content: %s", outA)
	}

	// r→chat non-stream: reasoning_content.
	outC, err := convertResponsesToOpenAI([]byte(resp))
	if err != nil {
		t.Fatalf("r→chat: %v", err)
	}
	if !strings.Contains(string(outC), `"reasoning_content":"deep thought"`) {
		t.Errorf("r→chat lost reasoning_text content: %s", outC)
	}
}

// P0-2: anthropic url-source images must survive the a→r hop (a→chat and
// tool_result reinjection already support url sources).
func TestConvertGap_AnthropicURLImageToResponses(t *testing.T) {
	in := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/pic.png"}},` +
		`{"type":"text","text":"what is this?"}]}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatalf("a→r: %v", err)
	}
	if !strings.Contains(string(out), `"input_image"`) || !strings.Contains(string(out), "https://example.com/pic.png") {
		t.Errorf("a→r dropped url-source image: %s", out)
	}
}

// P0-3: codex backend rejects max_output_tokens/temperature/top_p — they are
// stripped at conversion time (not learned via a first-400 paramBlock cycle).
// Other providers keep them.
func TestConvertGap_CodexStripsUnsupportedParams(t *testing.T) {
	in := `{"model":"gpt-x","max_tokens":100,"temperature":0.5,"top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`

	out, err := convertRequestFor([]byte(in), "anthropic", "responses", convertReqOpts{CodexShaping: true, ImageOK: true})
	if err != nil {
		t.Fatalf("a→r codex: %v", err)
	}
	m := unmarshalMap(t, out)
	for _, k := range []string{"max_output_tokens", "temperature", "top_p"} {
		if _, has := m[k]; has {
			t.Errorf("codex-bound request still carries %s: %s", k, out)
		}
	}
	if m["model"] != "gpt-x" || m["input"] == nil {
		t.Errorf("codex strip damaged the body: %s", out)
	}

	// Same conversion for a non-codex provider keeps everything.
	out2, err := convertRequestFor([]byte(in), "anthropic", "responses", convertReqOpts{ReasoningDialect: ReasoningEnableThinking, ImageOK: true})
	if err != nil {
		t.Fatalf("a→r qwen: %v", err)
	}
	m2 := unmarshalMap(t, out2)
	for _, k := range []string{"max_output_tokens", "temperature", "top_p"} {
		if _, has := m2[k]; !has {
			t.Errorf("non-codex request lost %s: %s", k, out2)
		}
	}
}

// tool_choice coverage: chat→r (chatToolChoiceToResponses) and r→a
// (responsesToolChoiceToAnthropic) full-variant tables, driven through the
// request converters (the functions were previously 0% covered).
func TestConvertRequest_ToolChoiceCoverage(t *testing.T) {
	// chat→r: string forms passthrough; {type:function,function:{name}} unwraps
	// one level; an unrecognized object shape passes through as-is (locked).
	for _, c := range []struct {
		name string
		tc   string
		want any // expected out["tool_choice"]; nil = field absent
	}{
		{"auto", `"auto"`, "auto"},
		{"none", `"none"`, "none"},
		{"required", `"required"`, "required"},
		{"function unwrap", `{"type":"function","function":{"name":"f"}}`, map[string]any{"type": "function", "name": "f"}},
		{"unrecognized object", `{"type":"bogus","x":1}`, map[string]any{"type": "bogus", "x": float64(1)}},
	} {
		in := `{"model":"g","messages":[{"role":"user","content":"hi"}],"tool_choice":` + c.tc + `}`
		out, err := convertOpenAIRequestToResponses([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := unmarshalMap(t, out)["tool_choice"]
		if c.want == nil {
			if got != nil {
				t.Errorf("chat→r %s: tool_choice = %v, want absent", c.name, got)
			}
			continue
		}
		if wantMap, ok := c.want.(map[string]any); ok {
			gotMap := asMap(got)
			for k, v := range wantMap {
				if gotMap[k] != v {
					t.Errorf("chat→r %s: tool_choice[%s] = %v, want %v (got %v)", c.name, k, gotMap[k], v, got)
				}
			}
		} else if got != c.want {
			t.Errorf("chat→r %s: tool_choice = %v, want %v", c.name, got, c.want)
		}
	}

	// r→a: auto/none passthrough as objects, required→any, function→tool;
	// unrecognized string dropped.
	for _, c := range []struct {
		name    string
		tc      string
		want    string // expected tool_choice.type; "" = field absent
		wantNam string
	}{
		{"auto", `"auto"`, "auto", ""},
		{"none", `"none"`, "none", ""},
		{"required→any", `"required"`, "any", ""},
		{"function→tool", `{"type":"function","name":"f"}`, "tool", "f"},
		{"unrecognized string", `"bogus"`, "", ""},
	} {
		in := `{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tool_choice":` + c.tc + `}`
		out, err := convertResponsesRequestToAnthropic([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := asMap(unmarshalMap(t, out)["tool_choice"])
		if c.want == "" {
			if got != nil {
				t.Errorf("r→a %s: tool_choice = %v, want absent", c.name, got)
			}
			continue
		}
		if got == nil {
			t.Fatalf("r→a %s: tool_choice missing: %s", c.name, out)
		}
		if got["type"] != c.want {
			t.Errorf("r→a %s: type = %v, want %q", c.name, got["type"], c.want)
		}
		if c.wantNam != "" && got["name"] != c.wantNam {
			t.Errorf("r→a %s: name = %v, want %q", c.name, got["name"], c.wantNam)
		}
	}
}

// ---------------------------------------------------------------------------
// K. cc-switch parity fixes (anthropic↔openai-chat)
// ---------------------------------------------------------------------------

// K1. a→chat: a message whose ONLY content part is an image has no `text` key
// to simplify to — the parts array must be kept (previously it collapsed to
// nil content, silently dropping the image). Single TEXT parts still simplify.
func TestConvertGap_SingleImagePartKept(t *testing.T) {
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`
	out, err := convertAnthropicRequestToOpenAI([]byte(
		`{"model":"m","max_tokens":8,"messages":[` +
			`{"role":"user","content":[` + img + `]},` +
			`{"role":"assistant","content":[` + img + `]},` +
			`{"role":"user","content":[{"type":"text","text":"just text"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(msgs), out)
	}
	for i, role := range []string{"user", "assistant"} {
		parts, ok := asMap(msgs[i])["content"].([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("%s single-image content = %#v, want 1-part array", role, asMap(msgs[i])["content"])
		}
		if got := objOf(t, parts[0], "image_url", "url"); got != "data:image/png;base64,QUJD" {
			t.Errorf("%s image url = %v", role, got)
		}
	}
	if got := asMap(msgs[2])["content"]; got != "just text" {
		t.Errorf("single text part content = %#v, want simplified string", got)
	}
}

// K2. chat→a: a tool message WITHOUT content must map to an empty tool_result
// string — never the literal "null" (the strOf(nil) trap, see
// protocol-conversion.md "可选字段"). Parts-array content extracts its text.
func TestConvertGap_ToolMessageContentExtraction(t *testing.T) {
	out, err := convertOpenAIRequestToAnthropic([]byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},` +
		`{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1"},` +
		`{"role":"tool","tool_call_id":"c2","content":[{"type":"text","text":"r1"},{"type":"text","text":"r2"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	var blocks []any
	for _, m := range msgs {
		mm := asMap(m)
		if mm["role"] != "user" {
			continue
		}
		for _, b := range mm["content"].([]any) {
			if asMap(b)["type"] == "tool_result" {
				blocks = append(blocks, b)
			}
		}
	}
	if len(blocks) != 2 {
		t.Fatalf("tool_result blocks = %d, want 2: %s", len(blocks), out)
	}
	if got := asMap(blocks[0])["content"]; got != "" {
		t.Errorf("missing tool content = %#v, want empty string (never the literal \"null\")", got)
	}
	if got := asMap(blocks[1])["content"]; got != "r1\nr2" {
		t.Errorf("parts tool content = %#v, want joined text %q", got, "r1\nr2")
	}
}

// K3. chat→a: system content given as a parts array must extract the text
// parts (previously leaked the raw `[{"type":"text",...}]` JSON literal into
// anthropic system); multiple system messages join with "\n". a→chat: a
// multi-block anthropic system also joins blocks with "\n" (was verbatim
// concatenation).
func TestConvertGap_SystemContentExtraction(t *testing.T) {
	out, err := convertOpenAIRequestToAnthropic([]byte(`{"model":"m","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}]},` +
		`{"role":"system","content":"s3"},` +
		`{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out)["system"]; got != "s1\ns2\ns3" {
		t.Errorf("chat→a system = %#v, want %q", got, "s1\ns2\ns3")
	}

	out2, err := convertAnthropicRequestToOpenAI([]byte(`{"model":"m","max_tokens":8,` +
		`"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out2)["messages"].([]any)
	if len(msgs) == 0 || asMap(msgs[0])["role"] != "system" {
		t.Fatalf("a→chat system message missing: %s", out2)
	}
	if got := asMap(msgs[0])["content"]; got != "a\nb" {
		t.Errorf("a→chat system = %#v, want %q", got, "a\nb")
	}
}

// K4. chat→a: refusal content must reach the anthropic client as text blocks
// (previously warned + dropped, leaving an empty body whose stop_reason said
// "refusal"). Covers refusal content parts, the message-level refusal field,
// and streaming refusal deltas.
func TestConvertGap_RefusalContent(t *testing.T) {
	out, err := convertOpenAIResponseToAnthropic([]byte(`{"id":"a","model":"g","choices":[` +
		`{"message":{"role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that."}]},"finish_reason":"content_filter"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal", m["stop_reason"])
	}
	content, _ := m["content"].([]any)
	if len(content) != 1 || objOf(t, content[0], "type") != "text" || objOf(t, content[0], "text") != "I cannot help with that." {
		t.Errorf("refusal part content = %v, want a single text block", content)
	}

	// Message-level refusal field (content null).
	out2, err := convertOpenAIResponseToAnthropic([]byte(`{"id":"a","model":"g","choices":[` +
		`{"message":{"role":"assistant","content":null,"refusal":"nope"},"finish_reason":"content_filter"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	content2, _ := unmarshalMap(t, out2)["content"].([]any)
	if len(content2) != 1 || objOf(t, content2[0], "type") != "text" || objOf(t, content2[0], "text") != "nope" {
		t.Errorf("message-level refusal content = %v, want a single text block", content2)
	}

	// Streaming refusal delta → text_delta + stop_reason refusal.
	in := `data: {"id":"c1","model":"g","choices":[{"delta":{"role":"assistant","refusal":"I cannot"},"finish_reason":"content_filter"}]}` + "\n\n" +
		`data: {"id":"c1","model":"g","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "g"))
	found := false
	for _, d := range sseFilter(events, "content_block_delta") {
		delta := sseDataMap(t, d)["delta"]
		if objOf(t, delta, "type") == "text_delta" && objOf(t, delta, "text") == "I cannot" {
			found = true
		}
	}
	if !found {
		t.Errorf("stream: no text_delta carrying the refusal; events=%v", sseEventTypes(events))
	}
	md := sseFilter(events, "message_delta")
	if len(md) != 1 || objOf(t, sseDataMap(t, md[0])["delta"], "stop_reason") != "refusal" {
		t.Errorf("stream stop_reason: %v", md)
	}
}

// K5. chat→a: cache-write tokens must split out of prompt_tokens into
// cache_creation_input_tokens (spellings: direct cache_creation_input_tokens,
// or prompt_tokens_details.cache_write_tokens) — otherwise the write is
// double-counted in BOTH the input and cache buckets.
func TestConvertGap_CacheWriteSplit(t *testing.T) {
	for _, c := range []struct {
		name  string
		usage string
	}{
		{"details.cache_write_tokens", `"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10,"cache_write_tokens":20}`},
		{"direct cache_creation_input_tokens", `"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10},"cache_creation_input_tokens":20`},
	} {
		out, err := convertOpenAIResponseToAnthropic([]byte(`{"id":"a","model":"g","choices":[` +
			`{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{` + c.usage + `}}`))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		u := asMap(unmarshalMap(t, out)["usage"])
		if u["input_tokens"] != float64(70) || u["cache_read_input_tokens"] != float64(10) || u["cache_creation_input_tokens"] != float64(20) {
			t.Errorf("%s: usage = %v, want input=70 cache_read=10 cache_creation=20", c.name, u)
		}
	}

	// Streaming: trailing usage with cache_write_tokens splits the same way.
	in := `data: {"id":"c1","model":"g","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"c1","model":"g","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10,"cache_write_tokens":20}}}` + "\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "g"))
	md := sseFilter(events, "message_delta")
	if len(md) != 1 {
		t.Fatalf("message_delta count = %d", len(md))
	}
	u := asMap(sseDataMap(t, md[0])["usage"])
	if u["input_tokens"] != float64(70) || u["cache_read_input_tokens"] != float64(10) || u["cache_creation_input_tokens"] != float64(20) {
		t.Errorf("stream usage = %v, want input=70 cache_read=10 cache_creation=20", u)
	}
}

// K6. chat→a: an empty-choices 200 is a model failure (intentional-behaviors
// #1), not a well-formed empty message — conversion must fail so the forward
// path surfaces it instead of committing content:[] + end_turn.
func TestConvertGap_EmptyChoicesFails(t *testing.T) {
	if _, err := convertOpenAIResponseToAnthropic([]byte(
		`{"id":"a","model":"g","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)); err == nil {
		t.Fatal("empty choices: want conversion error, got nil")
	}
}
