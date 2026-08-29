package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// mustJSON unmarshals b or fails; returns the generic value.
func mustJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(b))
	}
	return v
}

// objOf navigates v.(map)[key]; nil-safe.
func objOf(t *testing.T, v any, path ...string) any {
	t.Helper()
	cur := v
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// TestConvertRequest_AnthropicToOpenAI_Tools: a 3-turn tool conversation
// (user → assistant tool_use → user tool_result) converts with tools,
// tool_choice, tool_use→tool_calls, and tool_result→tool messages — structurally.
func TestConvertRequest_AnthropicToOpenAI_Tools(t *testing.T) {
	in := []byte(`{
		"model":"claude","max_tokens":1024,"system":"be brief",
		"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"get_weather"},
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{"city":"SF"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"sunny"}]}
		]
	}`)
	out, err := convertAnthropicRequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	o := mustJSON(t, out)

	// tools → function tools.
	tools := objOf(t, o, "tools").([]any)
	fn := objOf(t, tools[0], "function").(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "weather" {
		t.Errorf("tool function = %+v", fn)
	}
	if objOf(t, fn, "parameters", "type") != "object" {
		t.Errorf("tool parameters not carried: %+v", fn["parameters"])
	}

	// tool_choice {type:tool,name} → {type:function,function:{name}}.
	tc := objOf(t, o, "tool_choice").(map[string]any)
	if tc["type"] != "function" || objOf(t, tc, "function", "name") != "get_weather" {
		t.Errorf("tool_choice = %+v", tc)
	}

	msgs := objOf(t, o, "messages").([]any)
	// [0]=system, [1]=user text, [2]=assistant(tool_calls), [3]=tool result.
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d want 4: %+v", len(msgs), msgs)
	}
	if objOf(t, msgs[0], "role") != "system" || objOf(t, msgs[0], "content") != "be brief" {
		t.Errorf("system msg = %+v", msgs[0])
	}
	// assistant tool_use → tool_calls (content null).
	am := msgs[2].(map[string]any)
	if am["role"] != "assistant" {
		t.Errorf("msg[2] role=%v", am["role"])
	}
	tcs := am["tool_calls"].([]any)
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "t1" || tc0["type"] != "function" {
		t.Errorf("tool_call = %+v", tc0)
	}
	// arguments is a JSON string carrying the input object.
	args, _ := objOf(t, tc0, "function", "arguments").(string)
	var argObj map[string]any
	json.Unmarshal([]byte(args), &argObj)
	if argObj["city"] != "SF" {
		t.Errorf("tool_call arguments = %q want city=SF", args)
	}
	// tool_result → separate tool message.
	tm := msgs[3].(map[string]any)
	if tm["role"] != "tool" || tm["tool_call_id"] != "t1" || tm["content"] != "sunny" {
		t.Errorf("tool msg = %+v", tm)
	}
}

// TestConvertRequest_OpenAIToAnthropic_Tools: reverse — consecutive tool messages
// merge into ONE user message with a tool_result block array.
func TestConvertRequest_OpenAIToAnthropic_Tools(t *testing.T) {
	in := []byte(`{
		"model":"gpt","max_tokens":1024,
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
			{"role":"tool","tool_call_id":"t1","content":"sunny"}
		]
	}`)
	out, err := convertOpenAIRequestToAnthropic(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := mustJSON(t, out)
	// anthropic tools (no `type` wrapper).
	tools := objOf(t, o, "tools").([]any)
	if objOf(t, tools[0], "name") != "get_weather" {
		t.Errorf("anthropic tool = %+v", tools[0])
	}
	msgs := objOf(t, o, "messages").([]any)
	// [0]=user text, [1]=assistant(tool_use), [2]=user(tool_result block array).
	if len(msgs) != 3 {
		t.Fatalf("messages len=%d want 3", len(msgs))
	}
	// assistant tool_calls → tool_use block.
	am := msgs[1].(map[string]any)
	if am["role"] != "assistant" {
		t.Fatalf("msg[1] role=%v", am["role"])
	}
	blocks := am["content"].([]any)
	tu := blocks[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "t1" || tu["name"] != "get_weather" {
		t.Errorf("tool_use block = %+v", tu)
	}
	if tu["input"].(map[string]any)["city"] != "SF" {
		t.Errorf("tool_use input = %+v", tu["input"])
	}
	// tool message → ONE user message with a tool_result block.
	um := msgs[2].(map[string]any)
	if um["role"] != "user" {
		t.Fatalf("msg[2] role=%v want user (merged tool)", um["role"])
	}
	trs := um["content"].([]any)
	if len(trs) != 1 {
		t.Fatalf("tool_result blocks = %d want 1", len(trs))
	}
	tr := trs[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "t1" || tr["content"] != "sunny" {
		t.Errorf("tool_result block = %+v", tr)
	}
}

// TestConvertResponse_NonStream_Tools: openai tool_calls response → anthropic
// tool_use blocks + stop_reason tool_use; reverse symmetric.
func TestConvertResponse_NonStream_Tools(t *testing.T) {
	// openai → anthropic
	oaiResp := []byte(`{"id":"a","model":"gpt","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	ant, err := convertOpenAIResponseToAnthropic(oaiResp)
	if err != nil {
		t.Fatal(err)
	}
	a := mustJSON(t, ant)
	if objOf(t, a, "stop_reason") != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", objOf(t, a, "stop_reason"))
	}
	content := objOf(t, a, "content").([]any)
	tu := content[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "get_weather" || tu["input"].(map[string]any)["city"] != "SF" {
		t.Errorf("tool_use content block = %+v", tu)
	}

	// anthropic → openai
	antResp := []byte(`{"id":"msg_x","model":"claude","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{"city":"SF"}}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	oai, err := convertAnthropicResponseToOpenAI(antResp)
	if err != nil {
		t.Fatal(err)
	}
	o := mustJSON(t, oai)
	choice0 := objOf(t, o, "choices").([]any)[0].(map[string]any)
	if choice0["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice0["finish_reason"])
	}
	tc := objOf(t, choice0, "message", "tool_calls").([]any)[0].(map[string]any)
	if tc["id"] != "t1" || objOf(t, tc, "function", "name") != "get_weather" {
		t.Errorf("tool_call = %+v", tc)
	}
	args, _ := objOf(t, tc, "function", "arguments").(string)
	if !strings.Contains(args, "SF") {
		t.Errorf("arguments = %q want SF", args)
	}
}

// TestStreaming_OpenAIToolCallsToAnthropic: openai tool_calls delta stream →
// anthropic tool_use block. Tools are BUFFERED (anthropic content blocks are
// sequential — can't represent openai's interleaved parallel-tool fragments), so
// the argument fragments are concatenated and emitted as ONE input_json_delta in
// a complete tool_use block at the end.
func TestStreaming_OpenAIToolCallsToAnthropic(t *testing.T) {
	stream := "data: {\"model\":\"gpt\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"ci\"}}]}}]}\n\n" +
		"data: {\"model\":\"gpt\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ty\\\":\\\"SF\\\"}\"}}]}}]}\n\n" +
		"data: {\"model\":\"gpt\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	r := newOpenAIToAnthropicSSE(strings.NewReader(stream), "gpt")
	out := readAllChecked(t, r)
	s := string(out)
	for _, want := range []string{
		"event: message_start",
		`"type":"tool_use"`, `"name":"get_weather"`, `"id":"t1"`,
		`"type":"input_json_delta"`, `"partial_json":"{\"city\":\"SF\"}"`, // fragments concatenated
		"event: content_block_stop",
		`"stop_reason":"tool_use"`,
		`"input_tokens":5`, `"output_tokens":2`, // usage carried
		"event: message_stop",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in stream:\n%s", want, s)
		}
	}
}

// TestStreaming_AnthropicToolUseToOpenAI: reverse — anthropic tool_use events →
// openai delta.tool_calls + arguments fragments.
func TestStreaming_AnthropicToolUseToOpenAI(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"get_weather\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"SF\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	r := newAnthropicToOpenAISSE(strings.NewReader(stream), "claude")
	out := readAllChecked(t, r)
	s := string(out)
	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"tool_calls"`, `"id":"t1"`, `"name":"get_weather"`,
		`"arguments":"{\"city\":\"SF\"}"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":5`, `"completion_tokens":2`,
		"data: [DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in stream:\n%s", want, s)
		}
	}
}

// TestStreaming_OpenAIByteBoundarySplit: the same logical chunks split at arbitrary
// byte boundaries still convert correctly (the bufio.Scanner reassembles lines).
func TestStreaming_OpenAIByteBoundarySplit(t *testing.T) {
	full := "data: {\"model\":\"gpt\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
		"data: {\"model\":\"gpt\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: [DONE]\n\n"
	// Deliver 30 bytes per Read (mid-line) — a transformer keyed on Read
	// boundaries instead of reassembled lines would break here.
	r := newOpenAIToAnthropicSSE(&fixedChunksReader{b: []byte(full), n: 30}, "gpt")
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"text":"hel"`) || !strings.Contains(s, `"text":"lo"`) || !strings.Contains(s, "event: message_stop") {
		t.Errorf("byte-boundary split conversion wrong:\n%s", s)
	}
}

// fixedChunksReader returns at most n bytes per Read, simulating arbitrary
// network fragmentation (SSE reassembly must not depend on Read boundaries).
type fixedChunksReader struct {
	b []byte
	n int
}

func (r *fixedChunksReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := r.n
	if len(r.b) < n {
		n = len(r.b)
	}
	copy(p, r.b[:n])
	r.b = r.b[n:]
	return n, nil
}

// TestRoundTrip_AnthropicOpenAIAnthropic: a→o→a preserves tool structure (tool
// count, names, and input depth-equal). Catches asymmetric conversion bugs.
func TestRoundTrip_AnthropicOpenAIAnthropic(t *testing.T) {
	orig := []byte(`{"model":"c","max_tokens":100,
		"tools":[{"name":"f","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"a":1,"b":{"c":2}}}]}
		]}`)
	openai, err := convertAnthropicRequestToOpenAI(orig)
	if err != nil {
		t.Fatal(err)
	}
	back, err := convertOpenAIRequestToAnthropic(openai, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := mustJSON(t, back)
	msgs := objOf(t, b, "messages").([]any)
	// First message may be a user placeholder (anthropic requires first=user when
	// the original starts with assistant). Find the assistant message.
	var assistant map[string]any
	for _, m := range msgs {
		if objOf(t, m, "role") == "assistant" {
			assistant = m.(map[string]any)
			break
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message found in round-trip result")
	}
	blocks := objOf(t, assistant, "content").([]any)
	tu := blocks[0].(map[string]any)
	if tu["name"] != "f" || tu["id"] != "t1" {
		t.Errorf("round-trip tool_use identity lost: %+v", tu)
	}
	// input depth preserved: a=1, b.c=2.
	in := tu["input"].(map[string]any)
	if in["a"] != float64(1) || objOf(t, in, "b", "c") != float64(2) {
		t.Errorf("round-trip tool input not depth-equal: %+v", in)
	}
}

// TestStreaming_OpenAIInterleavedParallelTools (#2 regression): interleaved
// parallel tool fragments (tool 0, tool 1, tool 0 again) must NOT produce an
// invalid anthropic sequence (input_json_delta for a stopped block). Tools are
// buffered, so each tool_use block is emitted complete + sequential at the end —
// both tools appear, each with its full concatenated arguments, in index order.
func TestStreaming_OpenAIInterleavedParallelTools(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"fa\",\"arguments\":\"{\\\"x\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"fb\",\"arguments\":\"{\\\"y\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\":1}\"}},{\"index\":1,\"function\":{\"arguments\":\"\\\":2}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	out := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(stream), "gpt"))
	s := string(out)
	// Both tools present, complete, in index order (fa before fb).
	if strings.Index(s, `"name":"fa"`) < 0 || strings.Index(s, `"name":"fb"`) < 0 {
		t.Errorf("missing a tool_use block:\n%s", s)
	}
	if strings.Index(s, `"name":"fa"`) > strings.Index(s, `"name":"fb"`) {
		t.Errorf("tool blocks not in index order (fa should precede fb):\n%s", s)
	}
	// Each tool's arguments fully concatenated.
	if !strings.Contains(s, `"partial_json":"{\"x\":1}"`) || !strings.Contains(s, `"partial_json":"{\"y\":2}"`) {
		t.Errorf("interleaved args not concatenated per tool:\n%s", s)
	}
	// No content_block_stop may appear BEFORE the first content_block_start —
	// the invalid resume sequence the buffer approach eliminates. Position, not
	// count: equal counts with a stop-first order silently pass a count check.
	if strings.Index(s, "content_block_stop") < strings.Index(s, "content_block_start") {
		t.Errorf("a stop preceded the first start (invalid sequence):\n%s", s)
	}
	if strings.Count(s, "content_block_start") != strings.Count(s, "content_block_stop") {
		t.Errorf("content_block_start/stop counts differ (unpaired blocks):\n%s", s)
	}
}

// TestStreaming_AnthropicEmptyToolArgs (#3 regression): an anthropic tool_use
// block with NO input_json_delta still yields valid openai arguments ("{}"),
// not an empty string.
func TestStreaming_AnthropicEmptyToolArgs(t *testing.T) {
	stream := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(stream), "c"))
	if !strings.Contains(string(out), `"arguments":"{}"`) {
		t.Errorf("empty-args tool_use did not get a {} arguments fallback:\n%s", string(out))
	}
}

// TestStreaming_AnthropicDuplicateMessageDelta (#4 regression): a malformed
// anthropic stream with two message_delta events emits exactly ONE openai finish
// chunk (no duplicate).
func TestStreaming_AnthropicDuplicateMessageDelta(t *testing.T) {
	stream := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(stream), "c"))
	if n := strings.Count(string(out), `"finish_reason":"stop"`); n != 1 {
		t.Errorf("duplicate message_delta produced %d finish chunks, want 1:\n%s", n, string(out))
	}
}

// TestStreaming_NoUsageNoDone: an openai stream that omits both a finish_reason
// and [DONE] is truncated and must fail closed.
func TestStreaming_NoUsageNoDone(t *testing.T) {
	stream := "data: {\"model\":\"gpt\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	r := newOpenAIToAnthropicSSE(strings.NewReader(stream), "gpt")
	out := readAllChecked(t, r)
	s := string(out)
	if !strings.Contains(s, "event: error") || strings.Contains(s, "event: message_stop") {
		t.Errorf("truncated stream did not fail closed:\n%s", s)
	}
}

// TestStreaming_ErrorEvents: a mid-stream upstream error is propagated, not
// silently turned into a clean finish. Forward: openai error chunk → anthropic
// error event; reverse: anthropic error event → openai error chunk + [DONE].
func TestStreaming_ErrorEvents(t *testing.T) {
	// forward: openai error → anthropic error event
	fwd := "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_exceeded\"}}\n\n"
	out := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(fwd), "gpt"))
	s := string(out)
	if !strings.Contains(s, "event: error") || !strings.Contains(s, "rate limited") || !strings.Contains(s, "rate_limit_exceeded") {
		t.Errorf("forward error not propagated:\n%s", s)
	}

	// reverse: anthropic error → openai error chunk + [DONE]
	rev := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n"
	out2 := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(rev), "claude"))
	s2 := string(out2)
	if !strings.Contains(s2, `"error"`) || !strings.Contains(s2, "overloaded") || !strings.Contains(s2, "data: [DONE]") {
		t.Errorf("reverse error not propagated:\n%s", s2)
	}
}

// TestStreaming_MessageIdPassthrough: the upstream's real message id is passed
// through (not a constant). Forward: openai chunk id → anthropic message_start
// id; reverse: anthropic message.id → openai chunk id.
func TestStreaming_MessageIdPassthrough(t *testing.T) {
	fwd := "data: {\"id\":\"chatcmpl-real\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	out := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(fwd), "gpt"))
	if !strings.Contains(string(out), `"id":"chatcmpl-real"`) {
		t.Errorf("forward did not pass through message id:\n%s", string(out))
	}
	rev := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_real\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out2 := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(rev), "claude"))
	if !strings.Contains(string(out2), `"id":"msg_real"`) {
		t.Errorf("reverse did not pass through message id:\n%s", string(out2))
	}
}

// TestConvertRequest_ConsecutiveRoleMerge: openai consecutive same-role messages
// merge into one anthropic message (anthropic requires alternating roles).
func TestConvertRequest_ConsecutiveRoleMerge(t *testing.T) {
	in := []byte(`{"model":"gpt","max_tokens":10,"messages":[
		{"role":"user","content":"a"},
		{"role":"user","content":"b"},
		{"role":"assistant","content":"x"},
		{"role":"assistant","content":"y"}
	]}`)
	out, err := convertOpenAIRequestToAnthropic(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := mustJSON(t, out)
	msgs := objOf(t, o, "messages").([]any)
	if len(msgs) != 2 {
		t.Fatalf("consecutive-role merge: msgs=%d want 2 (user, assistant)", len(msgs))
	}
	um := msgs[0].(map[string]any)
	if um["role"] != "user" {
		t.Errorf("first msg role=%v want user", um["role"])
	}
	ublocks := um["content"].([]any)
	if len(ublocks) != 2 {
		t.Errorf("merged user content blocks=%d want 2", len(ublocks))
	}
}

// TestConvert_ContentBlocksAndChoices covers the per-block mappers + tool_choice
// maps directly (image↔image_url, thinking drop, tool_result text, all choice
// variants) — the branches the end-to-end tests don't reach.
func TestConvert_ContentBlocksAndChoices(t *testing.T) {
	// anthropic content blocks → openai parts.
	if p := anthropicContentBlockToOpenAIPart(map[string]any{"type": "text", "text": "hi"}, nil); p["text"] != "hi" {
		t.Errorf("text block: %+v", p)
	}
	img := anthropicContentBlockToOpenAIPart(map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "QUJD"}}, nil)
	if u, _ := img["image_url"].(map[string]any); u == nil || u["url"] != "data:image/png;base64,QUJD" {
		t.Errorf("image block → %v", img)
	}
	if anthropicContentBlockToOpenAIPart(map[string]any{"type": "thinking", "thinking": "x"}, nil) != nil {
		t.Error("thinking block should drop to nil")
	}
	if anthropicContentBlockToOpenAIPart(map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://i/x.png"}}, nil) == nil {
		t.Error("image url source should map")
	}

	// openai content parts → anthropic blocks.
	if b := openaiContentPartToAnthropicBlock(map[string]any{"type": "text", "text": "hi"}, nil); b["text"] != "hi" {
		t.Errorf("text part: %+v", b)
	}
	b := openaiContentPartToAnthropicBlock(map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,QUJD"}}, nil)
	src, _ := b["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "QUJD" {
		t.Errorf("image_url part → %v", b)
	}

	// anthropic tool_choice → openai.
	for _, c := range []struct{ in, want any }{
		{map[string]any{"type": "auto"}, "auto"},
		{map[string]any{"type": "any"}, "required"},
		{map[string]any{"type": "none"}, "none"},
		{map[string]any{"type": "tool", "name": "f"}, map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
		{map[string]any{"type": "weird"}, nil},
	} {
		if got := anthropicToolChoiceToOpenAI(c.in); fmt.Sprintf("%v", got) != fmt.Sprintf("%v", c.want) {
			t.Errorf("anthropicToolChoiceToOpenAI(%v)=%v want %v", c.in, got, c.want)
		}
	}

	// openai tool_choice → anthropic.
	for _, c := range []struct{ in, want any }{
		{"auto", map[string]any{"type": "auto"}},
		{"required", map[string]any{"type": "any"}},
		{"none", map[string]any{"type": "none"}},
		{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}, map[string]any{"type": "tool", "name": "f"}},
	} {
		if got := openaiToolChoiceToAnthropic(c.in); fmt.Sprintf("%v", got) != fmt.Sprintf("%v", c.want) {
			t.Errorf("openaiToolChoiceToAnthropic(%v)=%v want %v", c.in, got, c.want)
		}
	}

	// anthropic tool_result content (string + blocks; image inside dropped).
	if got := anthropicToolResultText("done", nil); got != "done" {
		t.Errorf("tool_result string = %q", got)
	}
	if got := anthropicToolResultText([]any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image", "source": map[string]any{}}, // dropped + warned
		map[string]any{"type": "text", "text": "b"},
	}, nil); got != "ab" {
		t.Errorf("tool_result blocks = %q want ab", got)
	}
}
