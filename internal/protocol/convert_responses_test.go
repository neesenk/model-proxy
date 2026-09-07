package protocol

import (
	"crypto/sha256"
	"encoding/hex"
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
	out, err := convertOpenAIRequestToResponses([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["instructions"] != "sys" {
		t.Errorf("instructions = %v (first system not promoted)", m["instructions"])
	}
	if r := asMap(m["reasoning"]); strOf(r["effort"]) != "high" || strOf(r["summary"]) != "auto" {
		t.Errorf("reasoning = %v (want effort=high, summary=auto)", m["reasoning"])
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
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
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

// TestConvertResponsesRequest_TypesLessMessageItems: the Responses API accepts
// message items in shorthand form — {role, content} with NO type key (pi-ai's
// openai-responses provider sends exactly this). They must convert as message
// items on both r→a and r→chat; dropping them empties the upstream message
// list (zhipu 400 "输入不能为空", code 1214). String content shorthand becomes
// a plain text block/message.
func TestConvertResponsesRequest_TypesLessMessageItems(t *testing.T) {
	in := `{"model":"glm-5.3","max_output_tokens":8192,"input":[` +
		`{"role":"system","content":"be nice"},` +
		`{"role":"user","content":[{"type":"input_text","text":"Reply with exactly: pong"}]},` +
		`{"role":"assistant","content":"pong"},` +
		`{"role":"user","content":"thanks"}]}`

	outA, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	ma := unmarshalMap(t, outA)
	if ma["system"] != "be nice" {
		t.Errorf("r→a system = %v", ma["system"])
	}
	msgsA, _ := ma["messages"].([]any)
	if len(msgsA) != 3 {
		t.Fatalf("r→a messages = %d, want 3 (user, assistant, user): %s", len(msgsA), outA)
	}
	if asMap(msgsA[0])["role"] != "user" {
		t.Errorf("r→a msg[0] role = %v", asMap(msgsA[0])["role"])
	}
	blk := asMap(asSlice(asMap(msgsA[1])["content"], 0))
	if blk["type"] != "text" || blk["text"] != "pong" {
		t.Errorf("r→a assistant string-content block = %v", blk)
	}
	blk2 := asMap(asSlice(asMap(msgsA[2])["content"], 0))
	if blk2["type"] != "text" || blk2["text"] != "thanks" {
		t.Errorf("r→a user string-content block = %v", blk2)
	}

	outC, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	mc := unmarshalMap(t, outC)
	msgsC, _ := mc["messages"].([]any)
	if len(msgsC) != 4 {
		t.Fatalf("r→chat messages = %d, want 4 (system, user, assistant, user): %s", len(msgsC), outC)
	}
	if asMap(msgsC[0])["role"] != "system" || asMap(msgsC[0])["content"] != "be nice" {
		t.Errorf("r→chat system msg = %v", msgsC[0])
	}
	if asMap(msgsC[2])["role"] != "assistant" || asMap(msgsC[2])["content"] != "pong" {
		t.Errorf("r→chat assistant msg = %v", msgsC[2])
	}
	if asMap(msgsC[3])["role"] != "user" || asMap(msgsC[3])["content"] != "thanks" {
		t.Errorf("r→chat user msg = %v", msgsC[3])
	}
}

// TestResponsesConversionMissingOptionalFieldsNeverEmitsLiteralNull: optional
// id/name fields that are ABSENT must convert to "" (strOpt semantics), never
// the literal string "null" (strOf(nil) renders JSON null → "null"), per the
// Tool ID rule in docs/architecture/protocol-conversion.md. Covers
// function_call_output.call_id and tool_choice.function.name on both the
// responses→anthropic and responses→openai request paths.
func TestResponsesConversionMissingOptionalFieldsNeverEmitsLiteralNull(t *testing.T) {
	in := `{"model":"gpt-x","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"function_call_output","output":"found"}],` +
		`"tool_choice":{"type":"function","function":{}}}`

	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"null"`) {
		t.Errorf("responses→anthropic output contains a literal \"null\": %s", out)
	}
	m := unmarshalMap(t, out)
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (the user text and the user tool_result merge): %s", len(msgs), out)
	}
	// The tool_result block rides in the merged user message.
	var resblk map[string]any
	if content, ok := asMap(msgs[0])["content"].([]any); ok {
		for _, part := range content {
			if blk := asMap(part); blk["type"] == "tool_result" {
				resblk = blk
			}
		}
	}
	if resblk == nil {
		t.Fatalf("no tool_result block in %s", out)
	}
	if id, _ := resblk["tool_use_id"].(string); id != "" {
		t.Errorf("tool_use_id for call_id-less function_call_output = %q, want \"\" (missing → empty, never \"null\")", id)
	}
	tc := asMap(m["tool_choice"])
	if name, _ := tc["name"].(string); name != "" {
		t.Errorf("tool_choice name for name-less function = %q, want \"\" (missing → empty, never \"null\")", name)
	}

	out2, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out2), `"null"`) {
		t.Errorf("responses→openai output contains a literal \"null\": %s", out2)
	}
	m2 := unmarshalMap(t, out2)
	tc2 := asMap(m2["tool_choice"])
	fn := asMap(tc2["function"])
	if name, _ := fn["name"].(string); name != "" {
		t.Errorf("openai tool_choice.function.name for name-less function = %q, want \"\" (missing → empty, never \"null\")", name)
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
// below use string(readAllChecked(t, r)).

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
	got := string(readAllChecked(t, newResponsesToAnthropicSSE(strings.NewReader(responsesTextSSE), "gpt-x")))
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		// Single-key substrings only: sonic marshals map keys in random order,
		// so a substring spanning two keys of one object is inherently flaky.
		`"type":"text_delta"`,
		`"text":"hel"`,
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
	got := string(readAllChecked(t, newResponsesToOpenAISSE(strings.NewReader(responsesTextSSE), "gpt-x")))
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
	got := string(readAllChecked(t, newResponsesToAnthropicSSE(strings.NewReader(responsesToolSSE), "gpt-x")))
	for _, want := range []string{
		`"type":"tool_use"`,
		`"id":"call_1"`,
		`"name":"search"`,
		// Single-key substrings only (random map key order — see above).
		`"type":"input_json_delta"`,
		`"partial_json":"{\"q\":"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestResponsesSSEToOpenAI_ToolCall(t *testing.T) {
	got := string(readAllChecked(t, newResponsesToOpenAISSE(strings.NewReader(responsesToolSSE), "gpt-x")))
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

// P0: r→a without max_output_tokens must still emit max_tokens (anthropic
// 400s "max_tokens required" otherwise; codex clients routinely omit it or
// send an explicit null — see testdata/wire/responses_codex.sse). The default
// matches the chat→a direction's generous fallback.
func TestConvertResponsesRequestToAnthropic_DefaultMaxTokens(t *testing.T) {
	mk := func(tail string) string {
		return `{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]` + tail + `}`
	}
	for name, in := range map[string]string{
		"absent": mk(""),
		"null":   mk(`,"max_output_tokens":null`),
	} {
		out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m := unmarshalMap(t, out)
		if m["max_tokens"] != float64(4096) {
			t.Errorf("%s: max_tokens = %v, want 4096 default", name, m["max_tokens"])
		}
	}
	// An explicit value still wins.
	out, err := convertResponsesRequestToAnthropic([]byte(mk(`,"max_output_tokens":100`)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out)["max_tokens"]; got != float64(100) {
		t.Errorf("explicit max_output_tokens: max_tokens = %v, want 100", got)
	}
}

// P0: r→chat streaming requests must inject stream_options.include_usage
// (same as the a→chat direction, convert.go) — kimi/MiniMax-style upstreams
// otherwise report all-zero usage on streams.
func TestConvertResponsesRequestToOpenAI_StreamIncludeUsage(t *testing.T) {
	in := `{"model":"gpt-x","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	so := asMap(m["stream_options"])
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v, want true (stream:true)", m["stream_options"])
	}
	// Non-streaming requests must NOT grow stream_options.
	out2, err := convertResponsesRequestToOpenAI([]byte(`{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := unmarshalMap(t, out2)["stream_options"]; has {
		t.Errorf("stream_options injected on a non-stream request")
	}
}

// P0: an assistant message carrying ONLY thinking blocks (common in
// incomplete-turn history) must not produce a responses reasoning item — codex
// 400s "reasoning item without its required following item" (cc-switch
// transform_responses.rs drops these).
func TestConvertAnthropicRequestToResponses_OrphanReasoning(t *testing.T) {
	// Thinking-only assistant turn → the reasoning item is dropped.
	in := `{"model":"claude-x","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"}]},` +
		`{"role":"user","content":"go on"}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	input, _ := unmarshalMap(t, out)["input"].([]any)
	for _, it := range input {
		if asMap(it)["type"] == "reasoning" {
			t.Errorf("orphan reasoning item leaked into input: %s", out)
		}
	}
	// redacted_thinking-only assistant turn → dropped too.
	inR := `{"model":"claude-x","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"redacted_thinking","data":"blob"}]}]}`
	outR, err := convertAnthropicRequestToResponses([]byte(inR))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range unmarshalMap(t, outR)["input"].([]any) {
		if asMap(it)["type"] == "reasoning" {
			t.Errorf("orphan redacted reasoning item leaked into input: %s", outR)
		}
	}
	// Thinking followed by a tool_use in the SAME assistant turn → kept.
	inK := `{"model":"claude-x","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"c1","name":"f","input":{}}]}]}`
	outK, err := convertAnthropicRequestToResponses([]byte(inK))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range unmarshalMap(t, outK)["input"].([]any) {
		if asMap(it)["type"] == "reasoning" {
			found = true
		}
	}
	if !found {
		t.Errorf("reasoning with a following function_call must be kept: %s", outK)
	}
}

// P0: r→a must fold system/developer input messages into the top-level
// anthropic system field (anthropic messages accept only user/assistant —
// a role:"system" message 400s, and developer was silently dropped).
func TestConvertResponsesRequestToAnthropic_SystemDeveloperFolded(t *testing.T) {
	in := `{"model":"gpt-x","max_output_tokens":10,"instructions":"top","input":[` +
		`{"type":"message","role":"system","content":[{"type":"input_text","text":"sys-note"}]},` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev-note"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["system"] != "top\n\nsys-note\n\ndev-note" {
		t.Errorf("system = %q, want instructions+system+developer folded", m["system"])
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 1 || asMap(msgs[0])["role"] != "user" {
		t.Errorf("messages = %v, want exactly one user message (no system/developer roles)", msgs)
	}
}

// P0: a→r must fold system/developer-role MESSAGES into instructions (codex
// 400s on system-role input items — opencodex inbound folds them too).
func TestConvertAnthropicRequestToResponses_SystemRoleMessageFoldsToInstructions(t *testing.T) {
	in := `{"model":"claude-x","max_tokens":100,"system":"top","messages":[` +
		`{"role":"system","content":"mid-sys"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"developer","content":[{"type":"text","text":"dev-note"}]}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["instructions"] != "top\n\nmid-sys\n\ndev-note" {
		t.Errorf("instructions = %q, want system field + system/developer messages folded", m["instructions"])
	}
	for _, it := range m["input"].([]any) {
		if r := strOf(asMap(it)["role"]); r == "system" || r == "developer" {
			t.Errorf("system/developer role item leaked into input: %s", out)
		}
	}
}

// P1: a→r with thinking must send reasoning.summary:"auto" alongside effort
// (opencodex's claude inbound sets it unconditionally) — without it codex
// streams carry no reasoning summary at all (testdata/wire/responses_codex_
// thinking.sse: zero reasoning items in the response).
func TestConvertAnthropicRequestToResponses_ReasoningSummaryAuto(t *testing.T) {
	in := `{"model":"claude-x","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":8000},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	r := asMap(unmarshalMap(t, out)["reasoning"])
	if r["effort"] != "medium" {
		t.Errorf("reasoning.effort = %v, want medium", r["effort"])
	}
	if r["summary"] != "auto" {
		t.Errorf("reasoning.summary = %v, want auto", r["summary"])
	}
}

// P1: OpenRouter-style reasoning dialects on chat MESSAGES (non-stream
// chat→r): reasoning_content > reasoning (string or {content,text,summary}
// object) > reasoning_details (array/object) — cc-switch codex_chat_common's
// extraction order.
func TestConvertOpenAIResponseToResponses_ReasoningDialects(t *testing.T) {
	mk := func(msgExtra string) string {
		return `{"id":"c1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"` + msgExtra + `}}]}`
	}
	reasoningText := func(t *testing.T, in string) string {
		t.Helper()
		out, err := convertOpenAIResponseToResponses([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range unmarshalMap(t, out)["output"].([]any) {
			m := asMap(it)
			if m["type"] == "reasoning" {
				return strOf(asMap(asSlice(m["summary"], 0))["text"])
			}
		}
		return ""
	}
	// reasoning as an OBJECT ({content|text|summary}).
	if got := reasoningText(t, mk(`,"reasoning":{"content":"obj-content"}`)); got != "obj-content" {
		t.Errorf("reasoning object = %q", got)
	}
	// reasoning_details array: text/summary entries join, encrypted skipped.
	details := `,"reasoning_details":[` +
		`{"type":"reasoning.text","text":"d1"},` +
		`{"type":"reasoning.encrypted","data":"blob"},` +
		`{"type":"reasoning.summary","summary":"d2"}]`
	if got := reasoningText(t, mk(details)); got != "d1\n\nd2" {
		t.Errorf("reasoning_details array = %q, want d1\\n\\nd2", got)
	}
	// reasoning_details as a bare object.
	if got := reasoningText(t, mk(`,"reasoning_details":{"type":"reasoning.text","text":"solo"}`)); got != "solo" {
		t.Errorf("reasoning_details object = %q", got)
	}
	// Priority: reasoning_content beats reasoning_details.
	if got := reasoningText(t, mk(`,"reasoning_content":"winner","reasoning_details":[{"type":"reasoning.text","text":"loser"}]`)); got != "winner" {
		t.Errorf("priority = %q, want reasoning_content to win", got)
	}
}

// P1: r→a usage must split cache WRITE tokens out of input_tokens and surface
// them as cache_creation_input_tokens (aligned with the chat→a direction):
// direct cache_creation_input_tokens spelling wins, input_tokens_details.
// cache_write_tokens is the fallback; input = input − cached − write, ≥0.
func TestConvertResponsesToAnthropic_CacheWrite(t *testing.T) {
	mk := func(usage string) string {
		return `{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":` + usage + `}`
	}
	// details spelling.
	out, err := convertResponsesToAnthropic([]byte(mk(`{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20}}`)))
	if err != nil {
		t.Fatal(err)
	}
	u := unmarshalMap(t, out)["usage"].(map[string]any)
	if u["input_tokens"] != float64(70) || u["cache_read_input_tokens"] != float64(10) || u["cache_creation_input_tokens"] != float64(20) {
		t.Errorf("details spelling: usage = %v, want input 70 / read 10 / create 20", u)
	}
	// direct spelling wins over details.
	out2, _ := convertResponsesToAnthropic([]byte(mk(`{"input_tokens":100,"output_tokens":5,"cache_creation_input_tokens":30,"input_tokens_details":{"cache_write_tokens":20}}`)))
	u2 := unmarshalMap(t, out2)["usage"].(map[string]any)
	if u2["input_tokens"] != float64(70) || u2["cache_creation_input_tokens"] != float64(30) {
		t.Errorf("direct spelling: usage = %v, want input 70 / create 30", u2)
	}
	// clamp: cache buckets exceeding input_tokens clamp to 0, never negative.
	out3, _ := convertResponsesToAnthropic([]byte(mk(`{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":9}}`)))
	u3 := unmarshalMap(t, out3)["usage"].(map[string]any)
	if u3["input_tokens"] != float64(0) {
		t.Errorf("clamp: input_tokens = %v, want 0", u3["input_tokens"])
	}
}

// P1: r→a function_call_output with a PARTS-ARRAY output (input_text +
// input_image) must convert to proper anthropic content blocks — text parts
// into the tool_result text, image parts into native image blocks — instead
// of JSON-serializing the array into one string.
func TestConvertResponsesRequestToAnthropic_ToolOutputPartsArray(t *testing.T) {
	png1x1 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	in := `{"model":"m","input":[` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_text","text":"shot taken"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + png1x1 + `"},` +
		`{"type":"input_image","image_url":"https://x/img.png"}]}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	var tr map[string]any
	for _, msg := range m["messages"].([]any) {
		for _, blk := range asMap(msg)["content"].([]any) {
			if b := asMap(blk); b["type"] == "tool_result" {
				tr = b
			}
		}
	}
	if tr == nil {
		t.Fatalf("no tool_result block: %s", out)
	}
	blocks, ok := tr["content"].([]any)
	if !ok || len(blocks) != 3 {
		t.Fatalf("tool_result content = %v, want [text image image]", tr["content"])
	}
	if asMap(blocks[0])["type"] != "text" || asMap(blocks[0])["text"] != "shot taken" {
		t.Errorf("text block = %v", blocks[0])
	}
	img1 := asMap(blocks[1])
	src1 := asMap(img1["source"])
	if img1["type"] != "image" || src1["type"] != "base64" || src1["media_type"] != "image/png" || src1["data"] != png1x1 {
		t.Errorf("base64 image block = %v", img1)
	}
	img2 := asMap(blocks[2])
	src2 := asMap(img2["source"])
	if img2["type"] != "image" || src2["type"] != "url" || src2["url"] != "https://x/img.png" {
		t.Errorf("url image block = %v", img2)
	}
	// String output still maps to a plain string (no regression).
	out2, err := convertResponsesRequestToAnthropic([]byte(`{"model":"m","input":[`+
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},`+
		`{"type":"function_call_output","call_id":"c1","output":"plain"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range unmarshalMap(t, out2)["messages"].([]any) {
		for _, blk := range asMap(msg)["content"].([]any) {
			if b := asMap(blk); b["type"] == "tool_result" {
				found = true
				if b["content"] != "plain" {
					t.Errorf("string output content = %v, want plain string", b["content"])
				}
			}
		}
	}
	if !found {
		t.Errorf("string output: no tool_result: %s", out2)
	}
}

// P1: refusal content parts carry real text — map them to text, both r→
// directions (aligned with the convert.go refusal→text fix; stop semantics
// already map via status/incomplete_details).
func TestConvertResponsesToAnthropic_RefusalPart(t *testing.T) {
	in := `{"id":"r1","status":"incomplete","incomplete_details":{"reason":"content_filter"},` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that."}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	out, err := convertResponsesToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal", m["stop_reason"])
	}
	blk := asMap(asSlice(m["content"], 0))
	if blk["type"] != "text" || blk["text"] != "I cannot help with that." {
		t.Errorf("refusal part must land as a text block, got %v", blk)
	}
}

func TestConvertResponsesToOpenAI_RefusalPart(t *testing.T) {
	in := `{"id":"r1","status":"incomplete","incomplete_details":{"reason":"content_filter"},` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that."}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	out, err := convertResponsesToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	ch := asMap(asSlice(m["choices"], 0))
	if ch["finish_reason"] != "content_filter" {
		t.Errorf("finish_reason = %v, want content_filter", ch["finish_reason"])
	}
	if got := asMap(ch["message"])["content"]; got != "I cannot help with that." {
		t.Errorf("refusal text lost: content = %v", got)
	}
}

// P1 tail: a tool_use WITHOUT an input field must not serialize as
// "arguments":"null" — default to "{}" (downstream JSON.parse breaks on
// "null"/""). Both the a→r request and a→r response converters.
func TestConvertAnthropicToResponses_ToolUseNullInput(t *testing.T) {
	// request side
	out, err := convertAnthropicRequestToResponses([]byte(
		`{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range unmarshalMap(t, out)["input"].([]any) {
		if m := asMap(it); m["type"] == "function_call" && m["arguments"] != "{}" {
			t.Errorf("request: arguments = %v, want {}", m["arguments"])
		}
	}
	// response side
	out2, err := convertAnthropicResponseToResponses([]byte(
		`{"id":"msg_1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"c1","name":"f"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range unmarshalMap(t, out2)["output"].([]any) {
		if m := asMap(it); m["type"] == "function_call" && m["arguments"] != "{}" {
			t.Errorf("response: arguments = %v, want {}", m["arguments"])
		}
	}
}

// P0 (codex 0.145 capture, /tmp/codex_req_3.json): tools declared in input
// {type:"additional_tools"} items must merge with top-level tools (top-level
// first), and the additional_tools item is a tool DECLARATION — it must not
// enter the message stream (nor fold into system/instructions as a developer
// message).
func TestConvertResponsesRequestToOpenAI_AdditionalTools(t *testing.T) {
	// Only additional_tools (codex 0.145's real shape: no top-level tools).
	in := `{"model":"m","tool_choice":"auto","input":[` +
		`{"type":"additional_tools","role":"developer","tools":[` +
		`{"type":"function","name":"wait","description":"w","parameters":{"type":"object"},"strict":false},` +
		`{"type":"custom","name":"exec","description":"x"},` +
		`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"followup_task","parameters":{"type":"object"}}]}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	tools, _ := m["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3 (function + wrapped custom + flattened ns sub): %s", len(tools), out)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[strOf(asMap(asMap(tl)["function"])["name"])] = true
	}
	for _, want := range []string{"wait", "exec", "collaboration__followup_task"} {
		if !names[want] {
			t.Errorf("tool %q missing: %s", want, out)
		}
	}
	// strict passes through on the function tool.
	if asMap(asMap(tools[0])["function"])["strict"] != false {
		t.Errorf("strict not passed through: %v", tools[0])
	}
	// The additional_tools item must NOT become a message.
	for _, msg := range m["messages"].([]any) {
		mm := asMap(msg)
		if mm["role"] == "developer" || strings.Contains(strOf(mm["content"]), "followup_task") {
			t.Errorf("additional_tools leaked into messages: %v", mm)
		}
	}
	// tool_choice survives because tools exist now.
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto (tools exist)", m["tool_choice"])
	}

	// Mixed: top-level first, additional appended.
	in2 := `{"model":"m","tools":[{"type":"function","name":"toplevel","parameters":{"type":"object"}}],"input":[` +
		`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"extra","parameters":{"type":"object"}}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out2, err := convertResponsesRequestToOpenAI([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	tools2, _ := unmarshalMap(t, out2)["tools"].([]any)
	if len(tools2) != 2 || strOf(asMap(asMap(tools2[0])["function"])["name"]) != "toplevel" || strOf(asMap(asMap(tools2[1])["function"])["name"]) != "extra" {
		t.Errorf("mixed tools order = %v, want [toplevel extra]", tools2)
	}
}

// r→a: same merge — anthropic gets the function tools, and the
// additional_tools item must not fold into `system` as developer text.
func TestConvertResponsesRequestToAnthropic_AdditionalTools(t *testing.T) {
	in := `{"model":"m","instructions":"sys","input":[` +
		`{"type":"additional_tools","role":"developer","tools":[` +
		`{"type":"function","name":"wait","description":"w","parameters":{"type":"object"},"strict":false}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 || asMap(tools[0])["name"] != "wait" {
		t.Errorf("r→a tools = %v, want [wait]", tools)
	}
	if sys := strOf(m["system"]); strings.Contains(sys, "wait") || strings.Contains(sys, "additional_tools") {
		t.Errorf("additional_tools folded into system: %q", sys)
	}
	for _, msg := range m["messages"].([]any) {
		if r := strOf(asMap(msg)["role"]); r != "user" && r != "assistant" {
			t.Errorf("additional_tools leaked as %q message", r)
		}
	}
}

// P1: adaptive thinking wire (Claude Code /effort): thinking:{type:"adaptive"}
// takes effort from output_config.effort (unknown strings drop to a default);
// type:"disabled" still emits no reasoning (opencodex claude/inbound.ts).
func TestConvertAnthropicRequestToResponses_AdaptiveThinking(t *testing.T) {
	mk := func(thinking, extra string) string {
		return `{"model":"m","max_tokens":100,"thinking":` + thinking + extra + `,"messages":[{"role":"user","content":"hi"}]}`
	}
	reasoningOf := func(t *testing.T, in string) map[string]any {
		t.Helper()
		out, err := convertAnthropicRequestToResponses([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		return asMap(unmarshalMap(t, out)["reasoning"])
	}
	// adaptive + output_config.effort → that effort, summary auto.
	r := reasoningOf(t, mk(`{"type":"adaptive"}`, `,"output_config":{"effort":"low"}`))
	if r["effort"] != "low" || r["summary"] != "auto" {
		t.Errorf("adaptive+output_config = %v, want effort low + summary auto", r)
	}
	// adaptive without output_config → a sane default effort.
	r2 := reasoningOf(t, mk(`{"type":"adaptive"}`, ``))
	if r2["effort"] != "high" {
		t.Errorf("adaptive default = %v, want high", r2["effort"])
	}
	// adaptive with an UNKNOWN effort string → default, not the garbage string.
	r3 := reasoningOf(t, mk(`{"type":"adaptive"}`, `,"output_config":{"effort":"bogus"}`))
	if r3["effort"] != "high" {
		t.Errorf("adaptive unknown effort = %v, want high default", r3["effort"])
	}
	// disabled → no reasoning at all.
	out4, _ := convertAnthropicRequestToResponses([]byte(mk(`{"type":"disabled"}`, `,"output_config":{"effort":"high"}`)))
	if _, has := unmarshalMap(t, out4)["reasoning"]; has {
		t.Errorf("disabled thinking must not emit reasoning")
	}
}

// P1: chat sources that inline thinking as a LEADING <think>…</think> block
// in content (MiniMax-style) — split it into a reasoning item; only the
// leading block splits, mid-text <think> stays literal (cc-switch
// split_leading_think_block).
func TestConvertOpenAIResponseToResponses_InlineThink(t *testing.T) {
	mk := func(content string) string {
		return `{"id":"c1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + content + `}}]}`
	}
	itemsOf := func(t *testing.T, in string) []any {
		t.Helper()
		out, err := convertOpenAIResponseToResponses([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		v, _ := unmarshalMap(t, out)["output"].([]any)
		return v
	}
	// Leading think block (leading whitespace tolerated, answer separator stripped).
	items := itemsOf(t, mk(`"  <think>let me think</think>\n\nthe answer"`))
	if len(items) != 2 || asMap(items[0])["type"] != "reasoning" || asMap(items[1])["type"] != "message" {
		t.Fatalf("output = %v, want [reasoning message]", items)
	}
	if got := strOf(asMap(asSlice(asMap(items[0])["summary"], 0))["text"]); got != "let me think" {
		t.Errorf("reasoning text = %q", got)
	}
	if got := strOf(asMap(asSlice(asMap(items[1])["content"], 0))["text"]); got != "the answer" {
		t.Errorf("message text = %q, want think block stripped", got)
	}
	// Mid-text think block stays literal.
	items2 := itemsOf(t, mk(`"answer <think>not leading</think> tail"`))
	if len(items2) != 1 || asMap(items2[0])["type"] != "message" {
		t.Fatalf("mid-text think split wrongly: %v", items2)
	}
	if got := strOf(asMap(asSlice(asMap(items2[0])["content"], 0))["text"]); got != "answer <think>not leading</think> tail" {
		t.Errorf("mid-text think altered: %q", got)
	}
}

// P1: a→r injects prompt_cache_key (codex reports cached_tokens:0 without
// one): metadata.user_id sha256 (hex, 32 chars) preferred; otherwise a
// system+tools fingerprint (opencodex claude/inbound.ts:443-475).
func TestConvertAnthropicRequestToResponses_PromptCacheKey(t *testing.T) {
	// metadata.user_id → sha256 hex (first 32 chars).
	in := `{"model":"m","max_tokens":10,"metadata":{"user_id":"user_123_session_abc"},"messages":[{"role":"user","content":"hi"}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	key := strOf(unmarshalMap(t, out)["prompt_cache_key"])
	want := sha256Hex32Ref("user_123_session_abc")
	if key != want {
		t.Errorf("prompt_cache_key = %q, want sha256(user_id) %q", key, want)
	}
	// No user_id: fingerprint from system+tools — deterministic, and changes
	// when the tool set changes.
	in2 := `{"model":"m","max_tokens":10,"system":"sys","tools":[{"name":"f","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`
	out2, _ := convertAnthropicRequestToResponses([]byte(in2))
	k1 := strOf(unmarshalMap(t, out2)["prompt_cache_key"])
	out3, _ := convertAnthropicRequestToResponses([]byte(in2))
	k2 := strOf(unmarshalMap(t, out3)["prompt_cache_key"])
	if k1 == "" || k1 != k2 {
		t.Errorf("fingerprint key not deterministic: %q vs %q", k1, k2)
	}
	in3 := `{"model":"m","max_tokens":10,"system":"sys","tools":[{"name":"g","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`
	out4, _ := convertAnthropicRequestToResponses([]byte(in3))
	if k3 := strOf(unmarshalMap(t, out4)["prompt_cache_key"]); k3 == k1 {
		t.Errorf("fingerprint unchanged across tool sets: %q", k3)
	}
	// r→chat passes through an existing prompt_cache_key (codex sends its own).
	out5, err := convertResponsesRequestToOpenAI([]byte(`{"model":"m","prompt_cache_key":"019f9ebf-8ec2","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out5)["prompt_cache_key"]; got != "019f9ebf-8ec2" {
		t.Errorf("r→chat prompt_cache_key = %v, want passthrough", got)
	}
}

// sha256Hex32Ref is the test-side reference for the cache-key hash.
func sha256Hex32Ref(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

// P1: chat→r tool conversion passes `strict` through.
func TestConvertOpenAIRequestToResponses_ToolStrict(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"},"strict":false}}]}`
	out, err := convertOpenAIRequestToResponses([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	tool := asMap(asSlice(unmarshalMap(t, out)["tools"], 0))
	if v, has := tool["strict"]; !has || v != false {
		t.Errorf("strict = %v (has=%v), want explicit false passthrough", v, has)
	}
}

// P1: reasoning.context (codex sends {"effort":…,"context":"all_turns"}) has
// no chat equivalent — dropped with a warning, not silently.
func TestConvertResponsesRequestToOpenAI_ReasoningContextWarns(t *testing.T) {
	in := `{"model":"m","reasoning":{"effort":"medium","context":"all_turns"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	var out []byte
	var err error
	logs := captureConvertLog(t, func() {
		out, err = convertResponsesRequestToOpenAI([]byte(in))
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "reasoning.context") {
		t.Errorf("expected a reasoning.context warning, got %q", logs)
	}
	if got := unmarshalMap(t, out)["reasoning_effort"]; got != "medium" {
		t.Errorf("reasoning_effort = %v (effort mapping must be unaffected)", got)
	}
}
