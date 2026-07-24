package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sonic "github.com/bytedance/sonic"
)

// unmarshalMap is a test helper: bytes → map[string]any (fatal on error).
func unmarshalMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := sonic.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", string(b), err)
	}
	return m
}

// ===========================================================================
// request converters
// ===========================================================================

func TestConvertAnthropicRequestToResponses(t *testing.T) {
	in := `{"model":"claude-x","max_tokens":100,"system":"be nice","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"call_1","name":"search","input":{"q":"x"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"}]}],` +
		`"tools":[{"name":"search","description":"d","input_schema":{"type":"object"}}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["model"] != "claude-x" {
		t.Errorf("model = %v", m["model"])
	}
	if m["instructions"] != "be nice" {
		t.Errorf("instructions = %v (system not promoted)", m["instructions"])
	}
	if m["max_output_tokens"] != float64(100) {
		t.Errorf("max_output_tokens = %v", m["max_output_tokens"])
	}
	input, _ := m["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input items = %d, want 4 (user msg, assistant msg, function_call, function_call_output): %s", len(input), out)
	}
	// item 2 is the function_call (assistant text + tool_use → message + function_call)
	fc := asMap(input[2])
	if fc["type"] != "function_call" || fc["name"] != "search" || fc["call_id"] != "call_1" {
		t.Errorf("function_call item = %v", fc)
	}
	if !strings.Contains(strOf(fc["arguments"]), `"q":"x"`) {
		t.Errorf("function_call arguments = %v", fc["arguments"])
	}
	fco := asMap(input[3])
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != "found" {
		t.Errorf("function_call_output item = %v", fco)
	}
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 || asMap(tools[0])["type"] != "function" || asMap(tools[0])["name"] != "search" {
		t.Errorf("tools = %v", tools)
	}
}

func TestConvertOpenAIRequestToResponses(t *testing.T) {
	in := `{"model":"gpt-x","max_tokens":50,"reasoning_effort":"high","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"r"}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`
	out, err := convertOpenAIRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["instructions"] != "sys" {
		t.Errorf("instructions = %v (first system not promoted)", m["instructions"])
	}
	if r := asMap(m["reasoning"]); strOf(r["effort"]) != "high" {
		t.Errorf("reasoning = %v", m["reasoning"])
	}
	input, _ := m["input"].([]any)
	// user msg, function_call, function_call_output (system consumed as instructions)
	if len(input) != 3 {
		t.Fatalf("input items = %d, want 3: %s", len(input), out)
	}
	if asMap(input[1])["type"] != "function_call" || asMap(input[1])["call_id"] != "c1" {
		t.Errorf("function_call item = %v", asMap(input[1]))
	}
	if asMap(input[2])["type"] != "function_call_output" || asMap(input[2])["call_id"] != "c1" {
		t.Errorf("function_call_output item = %v", asMap(input[2]))
	}
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 || asMap(tools[0])["name"] != "f" {
		t.Errorf("tools = %v", tools)
	}
}

func TestConvertResponsesRequestToAnthropic(t *testing.T) {
	in := `{"model":"gpt-x","max_output_tokens":100,"instructions":"be nice","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"found"}],` +
		`"tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["system"] != "be nice" {
		t.Errorf("system = %v", m["system"])
	}
	if m["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v", m["max_tokens"])
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant tool_use, user tool_result): %s", len(msgs), out)
	}
	// assistant message holds the tool_use block
	asst := asMap(msgs[1])
	if asst["role"] != "assistant" {
		t.Errorf("msg[1] role = %v", asst["role"])
	}
	blk := asMap(asSlice(asst["content"], 0))
	if blk["type"] != "tool_use" || blk["id"] != "call_1" || blk["name"] != "search" {
		t.Errorf("tool_use block = %v", blk)
	}
	if !strings.Contains(strOf(blk["input"]), `"q":"x"`) {
		t.Errorf("tool_use input = %v", blk["input"])
	}
	// tool_result in a user message
	res := asMap(msgs[2])
	resblk := asMap(asSlice(res["content"], 0))
	if resblk["type"] != "tool_result" || resblk["tool_use_id"] != "call_1" || resblk["content"] != "found" {
		t.Errorf("tool_result block = %v", resblk)
	}
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 || asMap(tools[0])["name"] != "search" {
		t.Errorf("tools = %v", tools)
	}
}

func TestConvertResponsesRequestToOpenAI(t *testing.T) {
	in := `{"model":"gpt-x","max_output_tokens":100,"instructions":"be nice","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"found"}],` +
		`"reasoning":{"effort":"high"}}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", m["reasoning_effort"])
	}
	if m["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v", m["max_tokens"])
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (system, user, assistant tool_calls, tool): %s", len(msgs), out)
	}
	if asMap(msgs[0])["role"] != "system" || asMap(msgs[0])["content"] != "be nice" {
		t.Errorf("system msg = %v", msgs[0])
	}
	asst := asMap(msgs[2])
	if asst["role"] != "assistant" {
		t.Errorf("assistant msg role = %v", asst["role"])
	}
	tc := asMap(asSlice(asst["tool_calls"], 0))
	if tc["id"] != "call_1" || asMap(tc["function"])["name"] != "search" {
		t.Errorf("tool_call = %v", tc)
	}
	tool := asMap(msgs[3])
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Errorf("tool msg = %v", tool)
	}
}

// asSlice returns element i of a []any (nil-safe).
func asSlice(v any, i int) any {
	if s, ok := v.([]any); ok && i < len(s) {
		return s[i]
	}
	return nil
}

// ===========================================================================
// response converters
// ===========================================================================

func TestConvertResponsesToAnthropic(t *testing.T) {
	in := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-x","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}"}],` +
		`"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}`
	out, err := convertResponsesToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Errorf("type/role = %v/%v", m["type"], m["role"])
	}
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v (function_call present → tool_use)", m["stop_reason"])
	}
	blocks, _ := m["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2 (text + tool_use): %s", len(blocks), out)
	}
	if asMap(blocks[0])["type"] != "text" || asMap(blocks[0])["text"] != "hello" {
		t.Errorf("text block = %v", blocks[0])
	}
	if asMap(blocks[1])["type"] != "tool_use" || asMap(blocks[1])["name"] != "search" {
		t.Errorf("tool_use block = %v", blocks[1])
	}
	u := asMap(m["usage"])
	if u["input_tokens"] != float64(8) || u["output_tokens"] != float64(2) {
		t.Errorf("usage = %v", u)
	}
}

func TestConvertResponsesToOpenAI(t *testing.T) {
	in := `{"id":"resp_1","status":"completed","model":"gpt-x","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{}"}],` +
		`"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}`
	out, err := convertResponsesToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["object"] != "chat.completion" {
		t.Errorf("object = %v", m["object"])
	}
	choices, _ := m["choices"].([]any)
	ch := asMap(choices[0])
	if ch["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", ch["finish_reason"])
	}
	msg := asMap(ch["message"])
	if msg["content"] != "hello" {
		t.Errorf("content = %v", msg["content"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 || asMap(tcs[0])["id"] != "call_1" {
		t.Errorf("tool_calls = %v", tcs)
	}
	u := asMap(m["usage"])
	if u["prompt_tokens"] != float64(8) || u["completion_tokens"] != float64(2) || u["total_tokens"] != float64(10) {
		t.Errorf("usage = %v", u)
	}
}

func TestConvertAnthropicResponseToResponses(t *testing.T) {
	in := `{"id":"msg_1","model":"gpt-x","stop_reason":"end_turn","content":[` +
		`{"type":"text","text":"hello"},{"type":"tool_use","id":"call_1","name":"search","input":{"q":"x"}}],` +
		`"usage":{"input_tokens":8,"output_tokens":2}}`
	out, err := convertAnthropicResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("object/status = %v/%v", m["object"], m["status"])
	}
	items, _ := m["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output items = %d, want 2: %s", len(items), out)
	}
	if asMap(items[0])["type"] != "message" {
		t.Errorf("item[0] = %v", items[0])
	}
	fc := asMap(items[1])
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "search" {
		t.Errorf("function_call item = %v", fc)
	}
	if !strings.Contains(strOf(fc["arguments"]), `"q":"x"`) {
		t.Errorf("arguments = %v", fc["arguments"])
	}
}

func TestConvertOpenAIResponseToResponses(t *testing.T) {
	in := `{"id":"chatcmpl-1","model":"gpt-x","choices":[` +
		`{"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`
	out, err := convertOpenAIResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["status"] != "completed" {
		t.Errorf("status = %v", m["status"])
	}
	items, _ := m["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output items = %d, want 2: %s", len(items), out)
	}
	fc := asMap(items[1])
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" {
		t.Errorf("function_call item = %v", fc)
	}
}

// ===========================================================================
// streaming transformers
// ===========================================================================

// readAll (io.Reader → []byte) is defined in models_check_test.go; stream tests
// below use string(readAll(r)).

const responsesTextSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-x"}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_0","role":"assistant","content":[]}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hel"}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"lo"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_0","status":"completed"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-x","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

func TestResponsesSSEToAnthropic(t *testing.T) {
	got := string(readAll(newResponsesToAnthropicSSE(strings.NewReader(responsesTextSSE), "gpt-x")))
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta","text":"hel"`,
		`"text":"lo"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
		`"input_tokens":3`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestResponsesSSEToOpenAI(t *testing.T) {
	got := string(readAll(newResponsesToOpenAISSE(strings.NewReader(responsesTextSSE), "gpt-x")))
	for _, want := range []string{
		"chat.completion.chunk",
		`"role":"assistant"`,
		`"content":"hel"`,
		`"content":"lo"`,
		`"finish_reason":"stop"`,
		`"prompt_tokens":3`,
		"data: [DONE]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

const responsesToolSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-x"}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_0","call_id":"call_1","name":"search","arguments":""}}` + "\n\n" +
	"event: response.function_call_arguments.delta\n" +
	`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_0","delta":"{\"q\":"}` + "\n\n" +
	"event: response.function_call_arguments.delta\n" +
	`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_0","delta":"\"x\"}"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_0","status":"completed"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":3,"total_tokens":4}}}` + "\n\n"

func TestResponsesSSEToAnthropic_ToolCall(t *testing.T) {
	got := string(readAll(newResponsesToAnthropicSSE(strings.NewReader(responsesToolSSE), "gpt-x")))
	for _, want := range []string{
		`"type":"tool_use"`,
		`"id":"call_1"`,
		`"name":"search"`,
		`"type":"input_json_delta","partial_json":"{\"q\":"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestResponsesSSEToOpenAI_ToolCall(t *testing.T) {
	got := string(readAll(newResponsesToOpenAISSE(strings.NewReader(responsesToolSSE), "gpt-x")))
	for _, want := range []string{
		`"tool_calls"`,
		`"id":"call_1"`,
		`"arguments":"{\"q\":"`,
		`"finish_reason":"tool_calls"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// ===========================================================================
// end-to-end through proxy.handler (forward: client → codex/Responses backend)
// ===========================================================================

// TestForward_AnthropicToResponses_NonStream: an Anthropic client hitting a
// route whose target declares protocol:responses gets its request converted to
// a Responses body (POST /responses, input list), and the Responses JSON
// response converted back to an Anthropic message.
func TestForward_AnthropicToResponses_NonStream(t *testing.T) {
	var gotReq string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("backend path=%q want /responses", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotReq = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-x","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"cdx": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "cdx", Model: "gpt-x", Protocol: "responses"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["cdx"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Backend received a Responses-format request (input list, no `messages`).
	if !strings.Contains(gotReq, `"input"`) || strings.Contains(gotReq, `"messages"`) {
		t.Errorf("backend got non-Responses request: %s", gotReq)
	}
	// Client received an Anthropic-format response.
	bs := string(body)
	for _, want := range []string{`"type":"message"`, `"text":"hello world"`, `"stop_reason":"end_turn"`} {
		if !strings.Contains(bs, want) {
			t.Errorf("client response missing %q: %s", want, bs)
		}
	}
}

// TestForward_OpenAIToResponses_NonStream: a chat client (POST /v1/chat/completions)
// hitting a protocol:responses target gets converted to /responses and back.
func TestForward_OpenAIToResponses_NonStream(t *testing.T) {
	var gotReq string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("backend path=%q want /responses", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotReq = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi back"}]}],"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"cdx": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "cdx", Model: "gpt-x", Protocol: "responses"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["cdx"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(gotReq, `"input"`) {
		t.Errorf("backend got non-Responses request: %s", gotReq)
	}
	bs := string(body)
	for _, want := range []string{`"object":"chat.completion"`, `"content":"hi back"`, `"finish_reason":"stop"`} {
		if !strings.Contains(bs, want) {
			t.Errorf("client response missing %q: %s", want, bs)
		}
	}
}
