package protocol

// convert_custom_tool_test.go — custom/freeform tool support for the
// responses↔chat pair (cc-switch transform_codex_chat.rs port): request-side
// wrapper ({input: string} function + history encoding), response-side
// unwrap (non-stream item + stream progressive unwrapping).

import (
	"strings"
	"testing"

	sonic "github.com/bytedance/sonic"
)

// Request: a custom tool definition wraps into a one-parameter function.
func TestCustomTool_WrapDefinition(t *testing.T) {
	in := `{"model":"g","input":[],"tools":[{"type":"custom","name":"shell","description":"run a command"}]}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := unmarshalMap(t, out)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d: %s", len(tools), out)
	}
	fn := asMap(asMap(tools[0])["function"])
	if fn["name"] != "shell" || fn["description"] != "run a command" {
		t.Errorf("wrapped function = %v", fn)
	}
	params := asMap(fn["parameters"])
	if params["type"] != "object" || params["additionalProperties"] != false {
		t.Errorf("wrapper parameters = %v", params)
	}
	inputProp := asMap(asMap(params["properties"])["input"])
	if inputProp["type"] != "string" {
		t.Errorf("input property = %v", inputProp)
	}
	req, _ := params["required"].([]any)
	if len(req) != 1 || req[0] != "input" {
		t.Errorf("required = %v", params["required"])
	}
}

// Request: custom_tool_call history encodes the raw input as {"input": raw}
// arguments (escaping JSON-special chars correctly), and consecutive custom
// calls merge into one assistant message like plain function_calls.
func TestCustomTool_WrapHistory(t *testing.T) {
	rawInput := "echo \"hi\" && cat <<'EOF'\n{\"nested\": true}\nEOF"
	in := `{"model":"g","input":[` +
		`{"type":"custom_tool_call","call_id":"c1","name":"shell","input":` + mustJSONStr(t, rawInput) + `},` +
		`{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"patch-body"},` +
		`{"type":"custom_tool_call_output","call_id":"c1","output":"done"}]}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	// One assistant (merged tool_calls) + one tool message.
	var asst, tool map[string]any
	for _, m := range msgs {
		mm := asMap(m)
		if mm["role"] == "assistant" {
			asst = mm
		}
		if mm["role"] == "tool" {
			tool = mm
		}
	}
	if asst == nil || tool == nil {
		t.Fatalf("messages = %v", msgs)
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("tool_calls = %d, want 2 merged: %v", len(tcs), asst["tool_calls"])
	}
	args := strOf(asMap(asMap(tcs[0])["function"])["arguments"])
	var decoded map[string]any
	if err := sonic.UnmarshalString(args, &decoded); err != nil {
		t.Fatalf("arguments not valid JSON: %q: %v", args, err)
	}
	if decoded["input"] != rawInput {
		t.Errorf("decoded input = %q, want verbatim raw input", decoded["input"])
	}
	if strOf(asMap(asMap(tcs[1])["function"])["name"]) != "apply_patch" {
		t.Errorf("second call = %v", tcs[1])
	}
	if tool["tool_call_id"] != "c1" || tool["content"] != "done" {
		t.Errorf("custom_tool_call_output = %v", tool)
	}
}

// mustJSONStr encodes s as a JSON string literal (for fixtures).
func mustJSONStr(t *testing.T, s string) string {
	t.Helper()
	b, err := sonic.MarshalString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Non-streaming response: a chat tool_call to a custom tool unwraps back to a
// custom_tool_call item with the raw input; parse failure falls back to the
// raw arguments string.
func TestCustomTool_UnwrapResponse(t *testing.T) {
	r2c := r2cCtx{custom: map[string]bool{"shell": true}}
	mk := func(name, args string) string {
		return `{"id":"c1","choices":[{"message":{"role":"assistant","tool_calls":[` +
			`{"id":"call_1","type":"function","function":{"name":` + mustJSONStr(t, name) + `,"arguments":` + mustJSONStr(t, args) + `}}]},"finish_reason":"tool_calls"}]}`
	}
	out, err := convertOpenAIResponseToResponsesNS([]byte(mk("shell", `{"input":"ls -la /tmp"}`)), r2c)
	if err != nil {
		t.Fatal(err)
	}
	item := asMap(asSlice(unmarshalMap(t, out)["output"], 0))
	if item["type"] != "custom_tool_call" || item["name"] != "shell" || item["call_id"] != "call_1" {
		t.Errorf("custom item = %v", item)
	}
	if item["input"] != "ls -la /tmp" {
		t.Errorf("unwrapped input = %v", item["input"])
	}
	if _, has := item["arguments"]; has {
		t.Errorf("custom item must not carry arguments: %v", item)
	}

	// Arguments that don't parse → raw passthrough.
	out2, err := convertOpenAIResponseToResponsesNS([]byte(mk("shell", `not-json{`)), r2c)
	if err != nil {
		t.Fatal(err)
	}
	item2 := asMap(asSlice(unmarshalMap(t, out2)["output"], 0))
	if item2["input"] != "not-json{" {
		t.Errorf("fallback input = %v", item2["input"])
	}

	// A name NOT in the custom set stays a function_call (regression).
	out3, err := convertOpenAIResponseToResponsesNS([]byte(mk("search", `{"q":"x"}`)), r2c)
	if err != nil {
		t.Fatal(err)
	}
	item3 := asMap(asSlice(unmarshalMap(t, out3)["output"], 0))
	if item3["type"] != "function_call" || item3["arguments"] != `{"q":"x"}` {
		t.Errorf("non-custom item changed: %v", item3)
	}
}

// Streaming: progressive unwrap across chunk boundaries — prefix split,
// escaped chars, and a \u escape split mid-sequence. done carries the full
// unwrapped input; added/done pair with the ctc_item_ id.
func TestCustomTool_StreamUnwrap(t *testing.T) {
	r2c := r2cCtx{custom: map[string]bool{"shell": true}}
	// arguments arrive as: {"input": "he + llo\nwor + ld 中 + !"}
	// with the \u4e2d escape split across two chunks.
	in := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"shell\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"input\\\": \"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"he\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"llo\\\\nwor\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ld \\\\u4e\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"2d!\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSENS(strings.NewReader(in), "g", r2c))
	assertResponsesItemPairing(t, events)

	added := sseFilter(events, "response.output_item.added")
	if len(added) != 1 {
		t.Fatalf("added = %d: %v", len(added), sseEventTypes(events))
	}
	item := asMap(sseDataMap(t, added[0])["item"])
	if item["type"] != "custom_tool_call" || item["id"] != "ctc_item_0" || item["name"] != "shell" || item["call_id"] != "call_1" {
		t.Errorf("custom added item = %v", item)
	}

	// Deltas concatenate to the fully unwrapped input (prefix stripped,
	// escapes decoded, split \u held then completed).
	deltas := sseFilter(events, "response.custom_tool_call_input.delta")
	var acc strings.Builder
	for _, d := range deltas {
		acc.WriteString(strOf(sseDataMap(t, d)["delta"]))
	}
	want := "hello\nworld 中!"
	if acc.String() != want {
		t.Errorf("unwrapped stream = %q, want %q", acc.String(), want)
	}
	// The split \u4e2d must not leak as raw text.
	if strings.Contains(acc.String(), "u4e") {
		t.Errorf("split escape leaked: %q", acc.String())
	}

	doneEv := sseFilter(events, "response.custom_tool_call_input.done")
	if len(doneEv) != 1 {
		t.Fatalf("input done = %d: %v", len(doneEv), sseEventTypes(events))
	}
	if got := strOf(sseDataMap(t, doneEv[0])["input"]); got != want {
		t.Errorf("done input = %q, want %q", got, want)
	}
	doneItem := asMap(sseDataMap(t, sseFilter(events, "response.output_item.done")[0])["item"])
	if doneItem["type"] != "custom_tool_call" || doneItem["id"] != "ctc_item_0" {
		t.Errorf("done item = %v", doneItem)
	}
}

// Streaming regression: a function tool in the same stream is unaffected.
func TestCustomTool_StreamMixedTools(t *testing.T) {
	r2c := r2cCtx{custom: map[string]bool{"shell": true}}
	in := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"input\\\": \\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"c2\",\"function\":{\"name\":\"search\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSENS(strings.NewReader(in), "g", r2c))
	assertResponsesItemPairing(t, events)
	types := map[string]string{}
	for _, ev := range sseFilter(events, "response.output_item.done") {
		item := asMap(sseDataMap(t, ev)["item"])
		types[strOf(item["type"])] = strOf(item["id"])
	}
	if types["custom_tool_call"] == "" || types["function_call"] == "" {
		t.Errorf("mixed done items = %v", types)
	}
	if got := sseCount(events, "response.function_call_arguments.done"); got != 1 {
		t.Errorf("function args done = %d, want 1 (only the plain tool)", got)
	}
	if got := sseCount(events, "response.custom_tool_call_input.done"); got != 1 {
		t.Errorf("custom input done = %d, want 1", got)
	}
}

// --- coverage: exception branches (convert_custom_tool.go) ---

// stripInputSuffix: closing `"}` (and lone `"`) removal, whitespace variants.
func TestCustomTool_StripInputSuffix(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`abc"}`, "abc"},
		{`abc"`, "abc"},
		{`abc`, "abc"},
		{`"}`, ""},
		{`"x"}`, `"x`}, // inner quote untouched — only the suffix goes
	} {
		if got := stripInputSuffix(c.in); got != c.want {
			t.Errorf("stripInputSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// unwrap with whitespace inside the wrapper: {"input":  "…" } style.
	u := newPartialInputUnwrapper()
	if got := u.unwrap(`{"input":  "hi" }`, true); got != "hi" {
		t.Errorf("unwrap with inner whitespace = %q, want hi", got)
	}
}

// unescapeHold: incomplete trailing escapes are held (not emitted) for the
// next chunk, then completed; unknown escapes pass through raw (locked).
func TestCustomTool_UnescapeHold(t *testing.T) {
	// Lone trailing backslash: held when incomplete, forced verbatim at done.
	if got := unescapeHold(`ab\`, false); got != "ab" {
		t.Errorf("trailing \\ held: %q, want ab", got)
	}
	if got := unescapeHold(`ab\`, true); got != `ab\` {
		t.Errorf("trailing \\ at done: %q, want verbatim ab\\", got)
	}
	// Incomplete \u and \u12: held; completing the sequence decodes it.
	if got := unescapeHold(`x\u`, false); got != "x" {
		t.Errorf(`incomplete \u held: %q, want x`, got)
	}
	if got := unescapeHold(`x\u12`, false); got != "x" {
		t.Errorf(`incomplete \u12 held: %q, want x`, got)
	}
	if got := unescapeHold(`x\u0041`, false); got != "xA" {
		t.Errorf(`complete A: %q, want xA`, got)
	}
	if got := unescapeHold(`x\u0041`, true); got != "xA" {
		t.Errorf(`complete A at done: %q, want xA`, got)
	}
	// Unknown escape \x: kept byte-for-byte (locked: fidelity over strictness).
	if got := unescapeHold(`a\xb`, false); got != `a\xb` {
		t.Errorf(`unknown escape: %q, want raw a\xb`, got)
	}
	// Standard escapes decode.
	if got := unescapeHold(`a\nb\tc\"d\\e\/f\rg\bh\fi`, false); got != "a\nb\tc\"d\\e/f\rg\bh\fi" {
		t.Errorf("standard escapes = %q", got)
	}
}

// unwrap raw fallback: arguments that are NOT the {"input": "…"} wrapper pass
// through unchanged (never crash, never drop).
func TestCustomTool_UnwrapFallback(t *testing.T) {
	for _, raw := range []string{`{"a":1}`, `not json at all`, `[1,2]`, `"plain"`} {
		u := newPartialInputUnwrapper()
		if got := u.unwrap(raw, true); got != raw {
			t.Errorf("unwrap fallback(%q) = %q, want raw passthrough", raw, got)
		}
		// Fallback is sticky per unwrapper: subsequent calls stay raw.
		if got := u.unwrap(raw, false); got != raw {
			t.Errorf("sticky fallback(%q) = %q", raw, got)
		}
	}
	// Short possible-prefix input waits (returns "") rather than falling back.
	u := newPartialInputUnwrapper()
	if got := u.unwrap(`{"input"`, false); got != "" {
		t.Errorf("short prefix must wait, got %q", got)
	}
}

// responsesCustomToolSet variants.
func TestCustomTool_CustomToolSetVariants(t *testing.T) {
	// No tools → nil.
	if m := responsesCustomToolSet([]byte(`{"model":"g","input":[]}`)); m != nil {
		t.Errorf("no tools: %v, want nil", m)
	}
	// Function-only tools → nil.
	if m := responsesCustomToolSet([]byte(`{"tools":[{"type":"function","name":"f"}]}`)); m != nil {
		t.Errorf("function-only: %v, want nil", m)
	}
	// Custom missing name → skipped.
	if m := responsesCustomToolSet([]byte(`{"tools":[{"type":"custom","description":"d"}]}`)); m != nil {
		t.Errorf("nameless custom: %v, want nil", m)
	}
	// Mixed: only the named customs land in the set.
	m := responsesCustomToolSet([]byte(`{"tools":[{"type":"function","name":"f"},{"type":"custom","name":"shell"},{"type":"custom","name":"apply_patch"}]}`))
	if len(m) != 2 || !m["shell"] || !m["apply_patch"] {
		t.Errorf("mixed set = %v", m)
	}
	// Malformed body → nil (no crash).
	if m := responsesCustomToolSet([]byte(`{bad json`)); m != nil {
		t.Errorf("malformed: %v, want nil", m)
	}
}

// decodeHex4 invalid digits: the bad escape is skipped (locked per
// implementation — nothing written for it, stream continues).
func TestCustomTool_UnescapeHoldInvalidHex(t *testing.T) {
	if got := unescapeHold(`a\uZZZZB`, false); got != "aB" {
		t.Errorf("invalid hex escape = %q, want aB (bad escape skipped)", got)
	}
	if r, ok := decodeHex4("ZZZZ"); ok || r != 0 {
		t.Errorf("decodeHex4(ZZZZ) = %v,%v, want 0,false", r, ok)
	}
}
