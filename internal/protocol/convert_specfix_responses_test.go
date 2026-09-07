package protocol

import (
	"strings"
	"testing"
)

// Regression tests for the protocol-spec conformance fixes in
// convert_responses.go (+ the effortToThinking callsite hunk in convert.go):
// thinking budget clamping, required Response-object keys, synthesized item
// ids/annotations, tool id sanitization, empty-block elision, legacy chat
// function shapes, and assorted field-mapping losses.

// --- Fix 1: synthesized thinking.budget_tokens must stay < max_tokens ---

func TestSpecFix_EffortBudgetClampedBelowMaxTokens(t *testing.T) {
	// (a) r→a with effort high and NO max_output_tokens: the injected default
	// max_tokens 4096 must clamp the 16384 ladder budget into [1024, 4095].
	out, err := convertResponsesRequestToAnthropic([]byte(
		`{"model":"gpt-x","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Errorf("max_tokens = %v, want injected default %d", m["max_tokens"], defaultAnthropicMaxTokens)
	}
	th := asMap(m["thinking"])
	if th == nil {
		t.Fatalf("no thinking config: %s", out)
	}
	budget := intOf(th["budget_tokens"])
	if budget < 1024 || budget >= defaultAnthropicMaxTokens {
		t.Errorf("budget_tokens = %d, want 1024 ≤ budget < %d", budget, defaultAnthropicMaxTokens)
	}

	// (b) chat→a with reasoning_effort medium and max_tokens 1500: the 8192
	// ladder value clamps to 1499 (1024 ≤ budget < 1500).
	out2, err := convertOpenAIRequestToAnthropic([]byte(
		`{"model":"g","reasoning_effort":"medium","max_tokens":1500,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	th2 := asMap(unmarshalMap(t, out2)["thinking"])
	if th2 == nil {
		t.Fatalf("no thinking config: %s", out2)
	}
	if b := intOf(th2["budget_tokens"]); b != 1499 {
		t.Errorf("budget_tokens = %d, want 1499 (medium 8192 clamped below max_tokens 1500)", b)
	}

	// (c) max_tokens 1000 ≤ 1024: thinking cannot be expressed legally — no
	// thinking config at all (silent deterministic best-effort).
	out3, err := convertOpenAIRequestToAnthropic([]byte(
		`{"model":"g","reasoning_effort":"low","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if th3, has := unmarshalMap(t, out3)["thinking"]; has {
		t.Errorf("max_tokens 1000 must not emit thinking (min legal budget 1024), got %v", th3)
	}
	// Same for the r→a direction.
	out4, err := convertResponsesRequestToAnthropic([]byte(
		`{"model":"gpt-x","max_output_tokens":1000,"reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if th4, has := unmarshalMap(t, out4)["thinking"]; has {
		t.Errorf("r→a max_tokens 1000 must not emit thinking, got %v", th4)
	}
}

// The ladder covers the full reasoning.effort enum: minimal/low/medium/high/
// xhigh map to fixed budgets, max tops out just below max_tokens, none
// disables thinking, and unknown non-empty values clamp down to high. Every
// rung must ENABLE thinking — mapping xhigh/max/minimal to silence was the
// original semantic-loss defect.
func TestSpecFix_EffortLadderAllRungs(t *testing.T) {
	mkReq := func(effort string, maxOut int) string {
		return `{"model":"gpt-x","max_output_tokens":` + itoa(maxOut) +
			`,"reasoning":{"effort":"` + effort + `"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	}
	// max_tokens 100000: no clamping, fixed ladder values land exactly.
	for effort, want := range map[string]int{
		"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384, "xhigh": 32000, "max": 99999,
	} {
		out, err := convertResponsesRequestToAnthropic([]byte(mkReq(effort, 100000)), nil)
		if err != nil {
			t.Fatal(err)
		}
		th := asMap(unmarshalMap(t, out)["thinking"])
		if th == nil {
			t.Errorf("effort %q produced no thinking config: %s", effort, out)
			continue
		}
		if th["type"] != "enabled" || intOf(th["budget_tokens"]) != want {
			t.Errorf("effort %q → thinking = %v, want enabled/%d", effort, th, want)
		}
	}
	// max clamps below a small cap like every other rung.
	out, err := convertResponsesRequestToAnthropic([]byte(mkReq("max", 4096)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if th := asMap(unmarshalMap(t, out)["thinking"]); th == nil || intOf(th["budget_tokens"]) != 4095 {
		t.Errorf(`effort "max" with max_tokens 4096 → thinking = %v, want enabled/4095`, th)
	}
	// none disables thinking; unknown non-empty values clamp down to high
	// (industry clamp-down convention for vendor-specific levels).
	out, err = convertResponsesRequestToAnthropic([]byte(mkReq("none", 100000)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if th, has := unmarshalMap(t, out)["thinking"]; has {
		t.Errorf(`effort "none" must not emit thinking, got %v`, th)
	}
	out, err = convertResponsesRequestToAnthropic([]byte(mkReq("bogus", 100000)), nil)
	if err != nil {
		t.Fatal(err)
	}
	th := asMap(unmarshalMap(t, out)["thinking"])
	if th == nil || th["type"] != "enabled" || intOf(th["budget_tokens"]) != 16384 {
		t.Errorf(`effort "bogus" → thinking = %v, want enabled/16384 (clamped to high)`, th)
	}
}

func TestSpecFix_ChatToAnthropic_NullMaxCompletionTokens(t *testing.T) {
	// Explicit "max_completion_tokens":null must behave as absent — never
	// emit "max_tokens":null — and max_tokens falls back / defaults.
	out, err := convertOpenAIRequestToAnthropic([]byte(
		`{"model":"g","max_completion_tokens":null,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"max_tokens":null`) {
		t.Errorf("explicit null max_completion_tokens leaked as max_tokens:null: %s", out)
	}
	if got := unmarshalMap(t, out)["max_tokens"]; got != float64(4096) {
		t.Errorf("max_tokens = %v, want injected default 4096", got)
	}
	// Null max_completion_tokens does not shadow a real max_tokens.
	out2, err := convertOpenAIRequestToAnthropic([]byte(
		`{"model":"g","max_completion_tokens":null,"max_tokens":1500,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out2)["max_tokens"]; got != float64(1500) {
		t.Errorf("max_tokens = %v, want 1500 (null max_completion_tokens treated as absent)", got)
	}
}

// --- Fix 2: synthesized Responses objects carry the spec-required keys ---

func assertResponsesRequiredKeys(t *testing.T, m map[string]any, incomplete bool) {
	t.Helper()
	// created_at: unix-seconds NUMBER (strongly-typed SDKs parse int64).
	ts, ok := m["created_at"].(float64)
	if !ok || ts <= 0 || ts != float64(int64(ts)) {
		t.Errorf("created_at must be a Unix-seconds integer, got %T (%v)", m["created_at"], m["created_at"])
	}
	if v, has := m["error"]; !has || v != nil {
		t.Errorf("error key = %v (present %v), want explicit null", v, has)
	}
	if incomplete {
		if asMap(m["incomplete_details"]) == nil {
			t.Errorf("incomplete response must carry incomplete_details: %v", m)
		}
	} else if v, has := m["incomplete_details"]; !has || v != nil {
		t.Errorf("incomplete_details key = %v (present %v), want explicit null when no reason", v, has)
	}
	if tools, ok := m["tools"].([]any); !ok || len(tools) != 0 {
		t.Errorf("tools = %v, want empty array", m["tools"])
	}
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\"", m["tool_choice"])
	}
	if ptc, ok := m["parallel_tool_calls"].(bool); !ok || ptc {
		t.Errorf("parallel_tool_calls = %v, want false", m["parallel_tool_calls"])
	}
	if md, ok := m["metadata"].(map[string]any); !ok || len(md) != 0 {
		t.Errorf("metadata = %v, want empty object", m["metadata"])
	}
}

func TestSpecFix_AnthropicToResponses_RequiredKeys(t *testing.T) {
	in := `{"id":"msg_1","model":"gpt-x","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":3,"output_tokens":1}}`
	out, err := convertAnthropicResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	assertResponsesRequiredKeys(t, unmarshalMap(t, out), false)

	// Incomplete (max_tokens) keeps a REAL incomplete_details object.
	in2 := `{"id":"msg_2","model":"gpt-x","stop_reason":"max_tokens","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":3,"output_tokens":1}}`
	out2, err := convertAnthropicResponseToResponses([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	m2 := unmarshalMap(t, out2)
	assertResponsesRequiredKeys(t, m2, true)
	if m2["status"] != "incomplete" || strOf(asMap(m2["incomplete_details"])["reason"]) != "max_output_tokens" {
		t.Errorf("incomplete mapping = %v / %v", m2["status"], m2["incomplete_details"])
	}
}

func TestSpecFix_ChatToResponses_RequiredKeys(t *testing.T) {
	in := `{"id":"chatcmpl-1","model":"gpt-x","choices":[` +
		`{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1}}`
	out, err := convertOpenAIResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	assertResponsesRequiredKeys(t, unmarshalMap(t, out), false)

	in2 := `{"id":"chatcmpl-2","model":"gpt-x","choices":[` +
		`{"message":{"role":"assistant","content":"hi"},"finish_reason":"length"}]}`
	out2, err := convertOpenAIResponseToResponses([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	m2 := unmarshalMap(t, out2)
	assertResponsesRequiredKeys(t, m2, true)
	if m2["status"] != "incomplete" || strOf(asMap(m2["incomplete_details"])["reason"]) != "max_output_tokens" {
		t.Errorf("incomplete mapping = %v / %v", m2["status"], m2["incomplete_details"])
	}
}

// --- Fix 3: r→chat chat.completion carries `created` from created_at ---

func TestSpecFix_ResponsesToChat_Created(t *testing.T) {
	in := `{"id":"resp_1","status":"completed","created_at":1720000000,"model":"gpt-x","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	out, err := convertResponsesToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out)["created"]; got != float64(1720000000) {
		t.Errorf("created = %v, want 1720000000 (mapped from created_at)", got)
	}
	// Absent created_at still yields a valid unix-seconds `created`.
	out2, err := convertResponsesToOpenAI([]byte(
		`{"id":"resp_2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ts, ok := unmarshalMap(t, out2)["created"].(float64)
	if !ok || ts <= 0 {
		t.Errorf("created fallback = %v, want a positive unix-seconds number", ts)
	}
}

// --- Fix 4: chat→r preserves message-level refusal as a refusal part ---

func TestSpecFix_ChatToResponses_Refusal(t *testing.T) {
	in := `{"id":"chatcmpl-1","model":"gpt-x","choices":[` +
		`{"message":{"role":"assistant","content":null,"refusal":"I cannot help with that."},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1}}`
	out, err := convertOpenAIResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	items, _ := m["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output items = %d, want 1 refusal message: %s", len(items), out)
	}
	item := asMap(items[0])
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("refusal item = %v", item)
	}
	part := asMap(asSlice(item["content"], 0))
	if part["type"] != "refusal" || part["refusal"] != "I cannot help with that." {
		t.Errorf("refusal part = %v", part)
	}
	if strings.Contains(string(out), `"output_text"`) {
		t.Errorf("no empty output_text may be synthesized for a refusal: %s", out)
	}
}

// --- Fix 5: synthesized output items carry ids; output_text carries annotations ---

func TestSpecFix_AnthropicToResponses_ItemIDsAndAnnotations(t *testing.T) {
	in := `{"id":"msg_1","model":"gpt-x","stop_reason":"end_turn","content":[` +
		`{"type":"thinking","thinking":"hmm","signature":"sig"},` +
		`{"type":"text","text":"one"},` +
		`{"type":"tool_use","id":"toolu_1","name":"f","input":{}},` +
		`{"type":"text","text":"two"}],` +
		`"usage":{"input_tokens":3,"output_tokens":1}}`
	out, err := convertAnthropicResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	items, _ := unmarshalMap(t, out)["output"].([]any)
	if len(items) != 4 {
		t.Fatalf("output items = %d, want 4 (reasoning, message, function_call, message): %s", len(items), out)
	}
	rs := asMap(items[0])
	if rs["type"] != "reasoning" || rs["id"] != "rs_item_0" {
		t.Errorf("reasoning item = %v, want id rs_item_0", rs)
	}
	msg0 := asMap(items[1])
	if msg0["type"] != "message" || msg0["id"] != "msg_item_0" {
		t.Errorf("message item = %v, want id msg_item_0", msg0)
	}
	part := asMap(asSlice(msg0["content"], 0))
	if anns, ok := part["annotations"].([]any); !ok || len(anns) != 0 {
		t.Errorf("output_text annotations = %v, want present empty array", part["annotations"])
	}
	msg1 := asMap(items[3])
	if msg1["type"] != "message" || msg1["id"] != "msg_item_1" {
		t.Errorf("second message item = %v, want id msg_item_1 (per-type counter)", msg1)
	}
}

func TestSpecFix_ChatToResponses_ItemIDsAndAnnotations(t *testing.T) {
	in := `{"id":"chatcmpl-1","model":"gpt-x","choices":[` +
		`{"message":{"role":"assistant","content":"hi","reasoning_content":"hmm","tool_calls":[{"id":"","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1}}`
	out, err := convertOpenAIResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	items, _ := unmarshalMap(t, out)["output"].([]any)
	if len(items) != 3 {
		t.Fatalf("output items = %d, want 3 (message, reasoning, function_call): %s", len(items), out)
	}
	msg := asMap(items[0])
	if msg["type"] != "message" || msg["id"] != "msg_item_0" {
		t.Errorf("message item = %v, want id msg_item_0", msg)
	}
	part := asMap(asSlice(msg["content"], 0))
	if anns, ok := part["annotations"].([]any); !ok || len(anns) != 0 {
		t.Errorf("output_text annotations = %v, want present empty array", part["annotations"])
	}
	rs := asMap(items[1])
	if rs["type"] != "reasoning" || rs["id"] != "rs_item_0" {
		t.Errorf("reasoning item = %v, want id rs_item_0", rs)
	}
	// Empty tool_call id falls back to the streaming fc_item_ convention.
	fc := asMap(items[2])
	if fc["type"] != "function_call" || fc["id"] != "fc_item_0" || fc["call_id"] != "fc_item_0" {
		t.Errorf("function_call item = %v, want id/call_id fc_item_0 fallback", fc)
	}
}

// --- Fix 6: r→a sanitizes tool_use ids with use/result pairing ---

func TestSpecFix_ResponsesToAnthropic_ToolIDSanitize(t *testing.T) {
	in := `{"model":"gpt-x","input":[` +
		`{"type":"function_call","call_id":"call.a","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call.a","output":"one"},` +
		`{"type":"function_call","call_id":"call_a","name":"g","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_a","output":"two"}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	type pair struct{ useID, resultID, resultText string }
	var pairs []pair
	for _, raw := range m["messages"].([]any) {
		for _, b := range asMap(raw)["content"].([]any) {
			blk := asMap(b)
			switch blk["type"] {
			case "tool_use":
				pairs = append(pairs, pair{useID: strOf(blk["id"])})
			case "tool_result":
				pairs[len(pairs)-1].resultID = strOf(blk["tool_use_id"])
				pairs[len(pairs)-1].resultText = strOf(blk["content"])
			}
		}
	}
	if len(pairs) != 2 {
		t.Fatalf("tool pairs = %d, want 2: %s", len(pairs), out)
	}
	// Dotted id sanitizes into the anthropic charset and pairs use/result.
	if pairs[0].useID != "call_a" || pairs[0].resultID != "call_a" || pairs[0].resultText != "one" {
		t.Errorf("pair 0 = %+v, want call_a/call_a/one", pairs[0])
	}
	// Collision: "call_a" also sanitizes to "call_a" → _2 suffix, still paired.
	if pairs[1].useID != "call_a_2" || pairs[1].resultID != "call_a_2" || pairs[1].resultText != "two" {
		t.Errorf("pair 1 = %+v, want call_a_2/call_a_2/two (collision suffix)", pairs[1])
	}

	// Response direction: free-form call_id sanitizes the same way.
	rout, err := convertResponsesToAnthropic([]byte(
		`{"id":"r1","status":"completed","output":[{"type":"function_call","call_id":"call.a","name":"f","arguments":"{}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var useID string
	for _, b := range unmarshalMap(t, rout)["content"].([]any) {
		if blk := asMap(b); blk["type"] == "tool_use" {
			useID = strOf(blk["id"])
		}
	}
	if useID != "call_a" {
		t.Errorf("r→a response tool_use id = %q, want call_a", useID)
	}

	// Two ID-LESS function_calls must NOT collapse onto one memoized
	// placeholder (duplicate tool_use ids are a hard 400); an id-less
	// function_call_output pairs with the first placeholder positionally.
	out2, err := convertResponsesRequestToAnthropic([]byte(`{"model":"gpt-x","input":[`+
		`{"type":"function_call","name":"f","arguments":"{}"},`+
		`{"type":"function_call","name":"g","arguments":"{}"},`+
		`{"type":"function_call_output","output":"r1"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	var resultID string
	for _, raw := range unmarshalMap(t, out2)["messages"].([]any) {
		for _, b := range asMap(raw)["content"].([]any) {
			blk := asMap(b)
			switch blk["type"] {
			case "tool_use":
				ids = append(ids, strOf(blk["id"]))
			case "tool_result":
				resultID = strOf(blk["tool_use_id"])
			}
		}
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] == ids[1] {
		t.Errorf("id-less tool_use ids = %v, want two distinct placeholders", ids)
	}
	if resultID != ids[0] {
		t.Errorf("id-less tool_result id = %q, want positional pairing with %q", resultID, ids[0])
	}
}

// --- Fix 7: no empty text blocks, no content:null messages ---

func TestSpecFix_ResponsesToAnthropic_EmptyTextSkipped(t *testing.T) {
	// (a) Empty text parts produce no empty text block; (b) a message whose
	// parts are ALL empty is dropped entirely (no "content":null).
	in := `{"model":"gpt-x","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":""},{"type":"input_text","text":"hi"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`
	out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"text":""`) || strings.Contains(string(out), `"content":null`) {
		t.Errorf("empty text block or content:null leaked: %s", out)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	// The all-empty assistant message is dropped; the two surrounding user
	// messages then merge into one (mergeConsecutiveAnthropicRoles).
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (all-empty assistant dropped, users merged): %s", len(msgs), out)
	}
	blocks, _ := asMap(msgs[0])["content"].([]any)
	if len(blocks) != 2 || asMap(blocks[0])["text"] != "hi" || asMap(blocks[1])["text"] != "next" {
		t.Errorf("user blocks = %v, want exactly [hi next] (no empty text)", blocks)
	}
}

func TestSpecFix_ResponsesToAnthropic_ToolOutputImagesOnly(t *testing.T) {
	// (c) A parts array carrying only images must not prepend an empty text
	// block to the tool_result content.
	png1x1 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	in := `{"model":"m","input":[` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + png1x1 + `"}]}]}`
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
	if !ok || len(blocks) != 1 {
		t.Fatalf("tool_result content = %v, want exactly [image]", tr["content"])
	}
	if asMap(blocks[0])["type"] != "image" {
		t.Errorf("tool_result content[0] = %v, want image (no empty text block)", blocks[0])
	}
}

// --- Fix 8: legacy chat function shapes in chat→r ---

func TestSpecFix_ChatToResponses_LegacyFunctionShapes(t *testing.T) {
	in := `{"model":"g","functions":[{"name":"f","parameters":{"type":"object"}}],"function_call":{"name":"f"},` +
		`"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{\"q\":\"x\"}"}},` +
		`{"role":"function","name":"f","content":"result-text"},` +
		`{"role":"function","name":"orphan","content":"dropped"},` +
		`{"role":"assistant","content":null,"function_call":{"name":"broken"}}]}`
	var out []byte
	logs := captureConvertLog(t, func() {
		var err error
		out, err = convertOpenAIRequestToResponses([]byte(in), nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	m := unmarshalMap(t, out)
	// Legacy request-level params never cross.
	if _, has := m["functions"]; has {
		t.Errorf("legacy `functions` param leaked: %s", out)
	}
	if _, has := m["function_call"]; has {
		t.Errorf("legacy `function_call` param leaked: %s", out)
	}
	items, _ := m["input"].([]any)
	var fc, fco map[string]any
	for _, raw := range items {
		it := asMap(raw)
		if it["role"] == "function" {
			t.Errorf("invalid Responses role:function message leaked: %v", it)
		}
		switch it["type"] {
		case "function_call":
			fc = it
		case "function_call_output":
			fco = it
		}
	}
	// Assistant legacy function_call → function_call item.
	if fc == nil || fc["name"] != "f" || strOf(fc["arguments"]) != `{"q":"x"}` {
		t.Fatalf("legacy function_call item = %v", fc)
	}
	if strOf(fc["call_id"]) == "" {
		t.Errorf("legacy function_call needs a synthesized call_id: %v", fc)
	}
	// role:function result pairs with it by name; no "name" marker remains.
	if fco == nil || fco["output"] != "result-text" || strOf(fco["call_id"]) != strOf(fc["call_id"]) {
		t.Errorf("legacy function output = %v, want call_id %v and output result-text", fco, fc["call_id"])
	}
	if _, has := fco["name"]; has {
		t.Errorf("pairing marker `name` leaked onto function_call_output: %v", fco)
	}
	// The orphan role:function and the argument-less legacy call drop + warn.
	for _, want := range []string{"orphan", "broken", "functions", "function_call"} {
		if !strings.Contains(logs, want) {
			t.Errorf("no drop warning mentioning %q, log = %q", want, logs)
		}
	}
	if strings.Contains(string(out), "dropped") || strings.Contains(string(out), `"broken"`) {
		t.Errorf("unpairable legacy content leaked: %s", out)
	}
}

// --- Fix 9: r→chat function_call without arguments defaults to "{}" ---

func TestSpecFix_ResponsesToChat_ArgumentsDefault(t *testing.T) {
	in := `{"id":"r1","status":"completed","output":[` +
		`{"type":"function_call","call_id":"c1","name":"f"}]}`
	out, err := convertResponsesToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"null"`) {
		t.Errorf(`literal "null" leaked (missing arguments): %s`, out)
	}
	msg := asMap(asSlice(unmarshalMap(t, out)["choices"], 0))
	tc := asMap(asSlice(asMap(msg["message"])["tool_calls"], 0))
	if got := asMap(tc["function"])["arguments"]; got != "{}" {
		t.Errorf("arguments = %v, want \"{}\" (same default as the streaming path)", got)
	}
}

// --- Fix 10: a→r hosted web_search maps to the Responses tool schema ---

func TestSpecFix_AnthropicToResponses_WebSearchTool(t *testing.T) {
	in := `{"model":"c","max_tokens":100,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3,` +
		`"allowed_domains":["a.com","b.com"],"blocked_domains":["evil.com"]}]}`
	var out []byte
	logs := captureConvertLog(t, func() {
		var err error
		out, err = convertAnthropicRequestToResponses([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
	})
	tool := asMap(asSlice(unmarshalMap(t, out)["tools"], 0))
	if tool["type"] != "web_search" {
		t.Fatalf("web_search tool = %v", tool)
	}
	filters := asMap(tool["filters"])
	domains, _ := filters["allowed_domains"].([]any)
	if len(domains) != 2 || domains[0] != "a.com" || domains[1] != "b.com" {
		t.Errorf("filters.allowed_domains = %v, want [a.com b.com]", filters)
	}
	for _, k := range []string{"max_uses", "blocked_domains", "allowed_domains"} {
		if _, has := tool[k]; has {
			t.Errorf("anthropic-only field %q leaked top-level onto the Responses web_search tool: %v", k, tool)
		}
	}
	if !strings.Contains(logs, "max_uses") || !strings.Contains(logs, "blocked_domains") {
		t.Errorf("no drop warnings for anthropic-only web_search fields, log = %q", logs)
	}
}

// --- Fix 11: output_config.effort "ultra" maps to the highest enum rung ---

func TestSpecFix_AnthropicToResponses_UltraEffort(t *testing.T) {
	in := `{"model":"c","max_tokens":100,"thinking":{"type":"adaptive"},"output_config":{"effort":"ultra"},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := strOf(asMap(unmarshalMap(t, out)["reasoning"])["effort"]); got != "max" {
		t.Errorf(`effort "ultra" → %q, want "max" (official enum has no ultra)`, got)
	}
}

// --- Fix 12: empty reasoning items (no text, no encrypted_content) drop ---

func TestSpecFix_ResponsesToAnthropic_EmptyReasoningDropped(t *testing.T) {
	// Request direction.
	in := `{"model":"gpt-x","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[]}]}`
	logs := captureConvertLog(t, func() {
		out, err := convertResponsesRequestToAnthropic([]byte(in), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), `"thinking"`) {
			t.Errorf("empty reasoning item produced a thinking block: %s", out)
		}
	})
	if !strings.Contains(logs, "empty reasoning") {
		t.Errorf("no drop warning for empty reasoning item, log = %q", logs)
	}

	// Response direction: an output carrying ONLY an empty reasoning item
	// still yields a valid (fallback) message.
	rout, err := convertResponsesToAnthropic([]byte(
		`{"id":"r1","status":"completed","output":[{"type":"reasoning","summary":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := unmarshalMap(t, rout)["content"].([]any)
	if len(blocks) != 1 || asMap(blocks[0])["type"] != "text" {
		t.Errorf("content = %v, want the single fallback text block", blocks)
	}
}

// --- Fix 13: model_context_window_exceeded is a truncation ---

func TestSpecFix_AnthropicToResponses_ContextWindowExceeded(t *testing.T) {
	in := `{"id":"msg_1","model":"gpt-x","stop_reason":"model_context_window_exceeded",` +
		`"content":[{"type":"text","text":"partial"}],"usage":{"input_tokens":3,"output_tokens":1}}`
	out, err := convertAnthropicResponseToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if m["status"] != "incomplete" {
		t.Errorf("status = %v, want incomplete (context exhaustion is a truncation)", m["status"])
	}
	// The incomplete-details enum has no context-window value; the
	// max-tokens-family reason is used (same as the streaming direction).
	if got := strOf(asMap(m["incomplete_details"])["reason"]); got != "max_output_tokens" {
		t.Errorf("incomplete_details.reason = %q, want max_output_tokens", got)
	}
}

// --- Fix 14: chat→r preserves image_url.detail ---

func TestSpecFix_ChatToResponses_ImageDetail(t *testing.T) {
	in := `{"model":"g","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://x/i.png","detail":"high"}}]}]}`
	out, err := convertOpenAIRequestToResponses([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := asMap(asSlice(unmarshalMap(t, out)["input"], 0))
	part := asMap(asSlice(msg["content"], 0))
	if part["type"] != "input_image" || part["image_url"] != "https://x/i.png" {
		t.Fatalf("image part = %v", part)
	}
	if part["detail"] != "high" {
		t.Errorf("image part detail = %v, want \"high\" (Responses input_image supports detail)", part["detail"])
	}
}
