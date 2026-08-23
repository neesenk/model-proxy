package protocol

// convert_namespace_test.go — MCP namespace tool-name flatten/restore
// (cc-switch transform_codex_responses_namespace.rs port, 9 scenarios):
// flatten on the responses→chat request, restore on the chat→responses
// response via a map rebuilt statelessly from the original request body.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 1. Flatten promotes namespaced sub-tools to flat chat names.
func TestNamespace_FlattenTools(t *testing.T) {
	in := `{"model":"g","input":[],"tools":[` +
		`{"type":"function","name":"read","namespace":"mcp__files__","description":"d","parameters":{"type":"object"}},` +
		`{"type":"function","name":"plain","parameters":{"type":"object"}}]}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := unmarshalMap(t, out)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %d: %s", len(tools), out)
	}
	fn0 := asMap(asMap(tools[0])["function"])
	if fn0["name"] != "mcp__files____read" {
		t.Errorf("flattened name = %v, want mcp__files____read (namespace __ + __ joiner)", fn0["name"])
	}
	if asMap(asMap(tools[1])["function"])["name"] != "plain" {
		t.Errorf("plain tool renamed: %v", tools[1])
	}
}

// 2. History function_call items and tool_choice are flattened too; a
// namespace-selecting tool_choice degrades to "auto".
func TestNamespace_FlattenHistoryAndToolChoice(t *testing.T) {
	in := `{"model":"g","input":[` +
		`{"type":"function_call","call_id":"c1","name":"read","namespace":"mcp__files__","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"}],` +
		`"tool_choice":{"type":"function","name":"read","namespace":"mcp__files__"},"tools":[{"type":"function","name":"read","namespace":"mcp__files__"}]}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	tc := asMap(asSlice(m["messages"], 0))
	call := asMap(asSlice(tc["tool_calls"], 0))
	if asMap(call["function"])["name"] != "mcp__files____read" {
		t.Errorf("history call name = %v", call)
	}
	if s := string(out); strings.Contains(s, "namespace") {
		t.Errorf("namespace field leaked into chat request: %s", s)
	}
	choice := asMap(m["tool_choice"])
	if objOf(t, choice, "function", "name") != "mcp__files____read" {
		t.Errorf("tool_choice = %v", choice)
	}

	in2 := `{"model":"g","input":[],"tool_choice":{"type":"namespace","namespace":"mcp__files__"},` +
		`"tools":[{"type":"function","name":"read","namespace":"mcp__files__"}]}`
	out2, err := convertResponsesRequestToOpenAI([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out2)["tool_choice"]; got != "auto" {
		t.Errorf("namespace tool_choice = %v, want degraded auto", got)
	}
}

// 3. No namespace anywhere → byte-level no-op of the flatten machinery.
func TestNamespace_NoNamespaceNoOp(t *testing.T) {
	in := `{"model":"g","input":[{"type":"function_call","call_id":"c1","name":"plain","arguments":"{}"}],` +
		`"tools":[{"type":"function","name":"plain","parameters":{"type":"object"}}],` +
		`"tool_choice":{"type":"function","name":"plain"}}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if got := objOf(t, asSlice(m["tools"], 0), "function", "name"); got != "plain" {
		t.Errorf("plain tool name changed: %v", got)
	}
	if got := objOf(t, m, "tool_choice", "function", "name"); got != "plain" {
		t.Errorf("tool_choice changed: %v", got)
	}
	call := asMap(asSlice(objOf(t, asSlice(m["messages"], 0), "tool_calls"), 0))
	if call == nil || asMap(call["function"])["name"] != "plain" {
		t.Errorf("history call changed: %v", call)
	}
	if m, _ := responsesNamespaceRestoreMap([]byte(in)); m != nil {
		t.Errorf("restore map = %v, want nil without namespaces", m)
	}
}

// 4. A flattened name colliding with an existing top-level tool → conversion
// error (fail-closed → forward 502).
func TestNamespace_CollisionFails(t *testing.T) {
	in := `{"model":"g","input":[],"tools":[` +
		`{"type":"function","name":"read","namespace":"mcp__files__"},` +
		`{"type":"function","name":"mcp__files____read"}]}`
	if out, err := convertResponsesRequestToOpenAI([]byte(in)); err == nil {
		t.Fatalf("expected collision error, got %s", out)
	}
}

// 5. Restore map reverse lookup from the original request body.
func TestNamespace_RestoreMapLookup(t *testing.T) {
	req := `{"model":"g","input":[],"tools":[` +
		`{"type":"function","name":"read","namespace":"mcp__files__"},` +
		`{"type":"function","name":"plain"}]}`
	m, _ := responsesNamespaceRestoreMap([]byte(req))
	if len(m) != 1 {
		t.Fatalf("restore map = %v, want 1 entry", m)
	}
	r, ok := m["mcp__files____read"]
	if !ok || r.Name != "read" || r.Namespace != "mcp__files__" {
		t.Errorf("restore entry = %+v", r)
	}
	if _, _, ok := nsRestoreName(m, "plain"); ok {
		t.Error("plain name must not hit the map")
	}
	if _, _, ok := nsRestoreName(nil, "mcp__files____read"); ok {
		t.Error("nil map must never hit")
	}
}

// 5b. Near-miss restore: a bare echo (gateway stripped the namespace) restores
// iff the bare name is owned by exactly one namespace AND is not itself a
// declared plain tool; any ambiguity passes through unchanged.
func TestNamespace_NearMissBareNameRestore(t *testing.T) {
	unique := `{"model":"g","input":[],"tools":[` +
		`{"type":"function","name":"search","namespace":"mcp__docs__"},` +
		`{"type":"function","name":"other","namespace":"mcp__other__"}]}`
	ctx := r2cCtxFor("responses", "openai", []byte(unique))
	name, ns, ok := ctx.restoreName("search")
	if !ok || name != "search" || ns != "mcp__docs__" {
		t.Errorf("unique bare restore = (%q,%q,%v), want (search,mcp__docs__,true)", name, ns, ok)
	}
	// Exact flattened key still wins first.
	name, ns, ok = ctx.restoreName("mcp__docs____search")
	if !ok || ns != "mcp__docs__" {
		t.Errorf("exact key restore = (%q,%q,%v)", name, ns, ok)
	}

	ambiguous := `{"model":"g","input":[],"tools":[` +
		`{"type":"function","name":"search","namespace":"mcp__a__"},` +
		`{"type":"function","name":"search","namespace":"mcp__b__"}]}`
	ctx = r2cCtxFor("responses", "openai", []byte(ambiguous))
	if _, _, ok := ctx.restoreName("search"); ok {
		t.Errorf("ambiguous bare name restored — must pass through")
	}

	collision := `{"model":"g","input":[],"tools":[` +
		`{"type":"function","name":"search","namespace":"mcp__a__"},` +
		`{"type":"function","name":"search"}]}`
	ctx = r2cCtxFor("responses", "openai", []byte(collision))
	if _, _, ok := ctx.restoreName("search"); ok {
		t.Errorf("bare name colliding with a plain tool restored — the echo most likely names the plain tool")
	}
	// The flattened key still restores in the collision case.
	if _, _, ok := ctx.restoreName("mcp__a____search"); !ok {
		t.Errorf("flattened key not restored in plain-collision case")
	}
}

// 6. flatten → restore round-trip (non-streaming): the namespaced tool is
// flattened on the way out and restored to its二维 name on the way back.
func TestNamespace_RoundTrip(t *testing.T) {
	req := `{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"read","namespace":"mcp__files__","parameters":{"type":"object"}}]}`
	chatReq, err := convertResponsesRequestToOpenAI([]byte(req))
	if err != nil {
		t.Fatal(err)
	}
	flat := strOf(objOf(t, asSlice(unmarshalMap(t, chatReq)["tools"], 0), "function", "name"))
	if flat != "mcp__files____read" {
		t.Fatalf("flattened name = %q", flat)
	}
	// Upstream answers with a call to the FLAT name.
	chatResp := `{"id":"c1","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + flat + `","arguments":"{\"path\":\"/tmp/x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	nsMap, _ := responsesNamespaceRestoreMap([]byte(req))
	out, err := convertOpenAIResponseToResponsesNS([]byte(chatResp), r2cCtx{ns: nsMap})
	if err != nil {
		t.Fatal(err)
	}
	item := asMap(asSlice(unmarshalMap(t, out)["output"], 0))
	if item["type"] != "function_call" || item["name"] != "read" || item["namespace"] != "mcp__files__" {
		t.Errorf("restored item = %v", item)
	}
	if item["arguments"] != `{"path":"/tmp/x"}` {
		t.Errorf("arguments = %v", item["arguments"])
	}
}

// 7. A call to a name NOT in the map passes through untouched.
func TestNamespace_UnmappedCallPassthrough(t *testing.T) {
	chatResp := `{"id":"c1","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"other","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	nsMap := map[string]nsRestore{"mcp__files____read": {Namespace: "mcp__files__", Name: "read"}}
	out, err := convertOpenAIResponseToResponsesNS([]byte(chatResp), r2cCtx{ns: nsMap})
	if err != nil {
		t.Fatal(err)
	}
	item := asMap(asSlice(unmarshalMap(t, out)["output"], 0))
	if item["name"] != "other" {
		t.Errorf("unmapped name rewritten: %v", item)
	}
	if _, has := item["namespace"]; has {
		t.Errorf("unmapped call gained a namespace: %v", item)
	}
}

// 8. An 80-char combined name truncates deterministically to 64 chars, and
// the restore map (keyed by the SAME truncation) still round-trips.
func TestNamespace_LongNameTruncation(t *testing.T) {
	ns := "mcp__" + strings.Repeat("n", 40)
	name := strings.Repeat("t", 35) // total 5+40+2+35 = 82 > 64
	flat := nsFlattenName(ns, name)
	if len(flat) != nsFlatMaxLen {
		t.Fatalf("flat len = %d, want %d", len(flat), nsFlatMaxLen)
	}
	if flat != (ns + "__" + name)[:nsFlatMaxLen] {
		t.Errorf("truncation not a plain prefix cut: %q", flat)
	}
	req := `{"model":"g","input":[],"tools":[{"type":"function","name":"` + name + `","namespace":"` + ns + `"}]}`
	m, _ := responsesNamespaceRestoreMap([]byte(req))
	r, ok := m[flat]
	if !ok || r.Name != name || r.Namespace != ns {
		t.Fatalf("restore map must key on the truncated flat name: %v", m)
	}
	chatResp := `{"id":"c1","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"` + flat + `","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	out, err := convertOpenAIResponseToResponsesNS([]byte(chatResp), r2cCtx{ns: m})
	if err != nil {
		t.Fatal(err)
	}
	item := asMap(asSlice(unmarshalMap(t, out)["output"], 0))
	if item["name"] != name || item["namespace"] != ns {
		t.Errorf("truncated round-trip restored = %v", item)
	}
}

// 9. Streaming restore: the output_item.added frame restores the二维 name;
// unrelated frames (text, completed) pass through untouched.
func TestNamespace_StreamRestore(t *testing.T) {
	flat := "mcp__files____read"
	nsMap := map[string]nsRestore{flat: {Namespace: "mcp__files__", Name: "read"}}
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"checking\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"" + flat + "\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSENS(strings.NewReader(in), "g", r2cCtx{ns: nsMap}))
	assertResponsesItemPairing(t, events)
	added := sseFilter(events, "response.output_item.added")
	var fc map[string]any
	for _, ev := range added {
		item := asMap(sseDataMap(t, ev)["item"])
		if item["type"] == "function_call" {
			fc = item
		}
	}
	if fc == nil {
		t.Fatalf("no function_call added frame: %v", sseEventTypes(events))
	}
	if fc["name"] != "read" || fc["namespace"] != "mcp__files__" {
		t.Errorf("stream restored item = %v", fc)
	}
	// The text item and the terminal frame are untouched by the map.
	if got := sseCount(events, "response.completed"); got != 1 {
		t.Errorf("response.completed = %d", got)
	}
	for _, ev := range added {
		item := asMap(sseDataMap(t, ev)["item"])
		if item["type"] == "message" {
			if _, has := item["namespace"]; has {
				t.Errorf("text item gained a namespace: %v", item)
			}
		}
	}
}

// web_search/tool_search degrade to plain function tools in the r→chat
// direction (hosted fallback); genuinely unknown tool types are dropped by
// nsFlattenResponsesTools with a convertWarn naming the type — silently
// vanishing tools are undebuggable. (namespace CONTAINERS are supported since
// the codex 0.145 capture: they expand into their subtools, see
// TestNSFlatten_NamespaceContainer.)
func TestNSFlatten_HostedToolFallbacks(t *testing.T) {
	tools := []any{
		map[string]any{"type": "tool_search"},
		map[string]any{"type": "web_search"},
		map[string]any{"type": "function", "name": "f", "parameters": map[string]any{"type": "object"}},
	}
	out, err := nsFlattenResponsesTools(tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 ||
		strOf(asMap(out[0]["function"])["name"]) != "tool_search" ||
		strOf(asMap(out[1]["function"])["name"]) != "web_search" ||
		strOf(asMap(out[2]["function"])["name"]) != "f" {
		t.Errorf("hosted fallback tools = %v", out)
	}
}

// P0 (codex 0.145 capture): {type:"namespace", name, tools:[…]} CONTAINER
// declarations promote each subtool to a top-level entry, flattened with the
// same scheme as field-form {type:"function", namespace:"x"} tools — so the
// response-side restore map works for both declaration shapes.
func TestNSFlatten_NamespaceContainer(t *testing.T) {
	container := []any{
		map[string]any{"type": "namespace", "name": "collaboration", "description": "d", "tools": []any{
			map[string]any{"type": "function", "name": "followup_task", "description": "f", "parameters": map[string]any{"type": "object"}, "strict": false},
			map[string]any{"type": "function", "name": "wait_agent", "parameters": map[string]any{"type": "object"}},
		}},
	}
	out, err := nsFlattenResponsesTools(container)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("flattened = %d, want 2: %v", len(out), out)
	}
	fn0 := asMap(out[0]["function"])
	if fn0["name"] != "collaboration__followup_task" || fn0["description"] != "f" || fn0["strict"] != false {
		t.Errorf("subtool 0 = %v, want flattened name + description + strict", fn0)
	}
	// Field-form declaration of the same tool flattens to the SAME name.
	fieldForm := []any{
		map[string]any{"type": "function", "name": "followup_task", "namespace": "collaboration", "parameters": map[string]any{"type": "object"}},
	}
	out2, err := nsFlattenResponsesTools(fieldForm)
	if err != nil {
		t.Fatal(err)
	}
	if asMap(out2[0]["function"])["name"] != asMap(out[0]["function"])["name"] {
		t.Errorf("container %v vs field-form %v flattened differently", asMap(out[0]["function"])["name"], asMap(out2[0]["function"])["name"])
	}
	// A subtool carrying its OWN namespace wins over the container name.
	own := []any{
		map[string]any{"type": "namespace", "name": "outer", "tools": []any{
			map[string]any{"type": "function", "name": "f", "namespace": "inner"},
		}},
	}
	out3, err := nsFlattenResponsesTools(own)
	if err != nil {
		t.Fatal(err)
	}
	if got := asMap(out3[0]["function"])["name"]; got != "inner__f" {
		t.Errorf("own-namespace subtool = %v, want inner__f", got)
	}
	// Nested containers are illegal — fail closed.
	nested := []any{
		map[string]any{"type": "namespace", "name": "a", "tools": []any{
			map[string]any{"type": "namespace", "name": "b", "tools": []any{}},
		}},
	}
	if _, err := nsFlattenResponsesTools(nested); err == nil {
		t.Error("nested namespace container must fail closed")
	}
}

// The restore map must cover container subtools declared in additional_tools
// input items (codex 0.145 declares tools ONLY there).
func TestNSRestoreMap_NamespaceContainerInAdditionalTools(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"additional_tools","role":"developer","tools":[` +
		`{"type":"namespace","name":"collaboration","tools":[` +
		`{"type":"function","name":"followup_task"}]},` +
		`{"type":"custom","name":"exec"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	// Restore map: container subtool recoverable by its flattened name.
	r, _ := responsesNamespaceRestoreMap(body)
	name, ns, ok := nsRestoreName(r, "collaboration__followup_task")
	if !ok || name != "followup_task" || ns != "collaboration" {
		t.Errorf("restore = %q/%q/%v, want followup_task/collaboration/true", name, ns, ok)
	}
	// Custom set: custom tools in additional_tools are registered.
	if !responsesCustomToolSet(body)["exec"] {
		t.Error("custom tool set missing exec from additional_tools")
	}
}

func TestNamespace_AnthropicRoundTrip(t *testing.T) {
	request := []byte(`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"read","namespace":"mcp__files","arguments":"{}"}],` +
		`"tool_choice":{"type":"function","name":"read","namespace":"mcp__files"},` +
		`"tools":[{"type":"function","name":"read","namespace":"mcp__files","parameters":{"type":"object"}}]}`)
	converted, err := convertResponsesRequestToAnthropic(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"name":"mcp__files__read"`) {
		t.Fatalf("namespace was not flattened for Anthropic: %s", converted)
	}

	ctx := r2cCtxFor("responses", "anthropic", request)
	response := []byte(`{"id":"msg_1","model":"claude","stop_reason":"tool_use","content":[` +
		`{"type":"tool_use","id":"c2","name":"mcp__files__read","input":{}}],"usage":{}}`)
	restored, err := convertAnthropicResponseToResponsesNS(response, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), `"name":"read"`) ||
		!strings.Contains(string(restored), `"namespace":"mcp__files"`) {
		t.Fatalf("namespace was not restored from Anthropic response: %s", restored)
	}

	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c2","name":"mcp__files__read","input":{}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	events := drainSSE(t, newAnthropicToResponsesSSENS(strings.NewReader(stream), "claude", ctx))
	var joined strings.Builder
	for _, event := range events {
		joined.WriteString(event.data)
	}
	if !strings.Contains(joined.String(), `"name":"read"`) ||
		!strings.Contains(joined.String(), `"namespace":"mcp__files"`) {
		t.Fatalf("stream namespace was not restored: %s", joined.String())
	}
}

// Review fix: hosted fallback tool names join the collision set in the r→a
// direction too (the r→chat converter already checks them) — a user function
// named web_search/tool_search plus the hosted fallback would otherwise send
// duplicate tool names to Anthropic (400).
func TestResponsesToolsToAnthropic_HostedFallbackCollision(t *testing.T) {
	for _, name := range []string{"web_search", "tool_search"} {
		hosted := map[string]any{"type": name}
		userFn := map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}
		if _, err := responsesToolsToAnthropic([]any{hosted, userFn}); err == nil {
			t.Errorf("hosted fallback %q colliding with a user function must fail closed", name)
		}
		if _, err := responsesToolsToAnthropic([]any{userFn, hosted}); err == nil {
			t.Errorf("user function %q colliding with the hosted fallback must fail closed", name)
		}
	}
	// No collision → hosted fallbacks still convert.
	out, err := responsesToolsToAnthropic([]any{map[string]any{"type": "web_search"}, map[string]any{"type": "tool_search"}})
	if err != nil || len(out) != 2 {
		t.Errorf("hosted fallbacks = %v, err %v, want 2 tools", out, err)
	}
}

// Review fix: truncation backs off to a rune boundary — a raw 64-byte cut can
// split a multi-byte rune and produce invalid UTF-8.
func TestNamespace_TruncationRuneBoundary(t *testing.T) {
	ns := strings.Repeat("n", 61) // 61 + "__" = 63 bytes; the next rune starts at byte 63
	name := "界" + strings.Repeat("t", 10)
	flat := nsFlattenName(ns, name)
	if len(flat) > nsFlatMaxLen {
		t.Fatalf("flat len = %d, want ≤ %d", len(flat), nsFlatMaxLen)
	}
	if !utf8.ValidString(flat) {
		t.Fatalf("flat %q is not valid UTF-8", flat)
	}
	if flat != ns+"__" {
		t.Errorf("flat = %q, want the rune-boundary prefix %q", flat, ns+"__")
	}
	// Determinism: flatten and the restore map agree on the truncated key.
	req := `{"model":"g","input":[],"tools":[{"type":"function","name":"` + name + `","namespace":"` + ns + `"}]}`
	m, _ := responsesNamespaceRestoreMap([]byte(req))
	if r, ok := m[flat]; !ok || r.Name != name || r.Namespace != ns {
		t.Errorf("restore map must key on the rune-boundary flat name: %v", m)
	}
}
