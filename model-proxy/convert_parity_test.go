package main

// convert_parity_test.go — scenarios ported from cc-switch's conversion tests
// (src-tauri/src/proxy/providers/, Rust). Each test names its source scenario.
// Where our behavior deliberately differs, the test pins OUR semantics and the
// comment says so.

import (
	"io"
	"strings"
	"testing"
)

// cc-switch streaming_codex_chat.rs:1914 (done-only arguments fallback, both
// r→ directions): no arguments deltas, full arguments only in output_item.done.
func TestParity_DoneOnlyArgumentsFallback(t *testing.T) {
	in := `data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"

	// r→anthropic: the args must arrive as input_json_delta before block stop.
	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	outA := string(rawA)
	if !strings.Contains(outA, `"input_json_delta"`) || !strings.Contains(outA, `{\"q\":\"x\"}`) {
		t.Errorf("r→a: done-only arguments lost:\n%s", outA)
	}

	// r→chat: the args must arrive as an arguments chunk.
	rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(in), "m"))
	outC := string(rawC)
	if !strings.Contains(outC, `"arguments":"{\"q\":\"x\"}"`) {
		t.Errorf("r→chat: done-only arguments lost:\n%s", outC)
	}
}

// cc-switch streaming_responses.rs:1805 (duplicate added must not reset the
// block / buffered args): r→a gets a repeated output_item.added mid-stream.
func TestParity_DuplicateAddedKeepsBlock(t *testing.T) {
	in := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"x\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"
	raw, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	events := drainSSE(t, strings.NewReader(string(raw)))
	if n := sseCount(events, "content_block_start"); n != 1 {
		t.Errorf("content_block_start = %d, want 1 (duplicate added must not reopen):\n%s", n, raw)
	}
	// Both deltas land on the SAME block index, in order.
	var deltas []string
	for _, ev := range events {
		if ev.event == "content_block_delta" {
			deltas = append(deltas, ev.data)
		}
	}
	if len(deltas) != 2 || !strings.Contains(deltas[0], `{\"q\":`) || !strings.Contains(deltas[1], `\"x\"}`) {
		t.Errorf("args deltas wrong after duplicate added: %v", deltas)
	}
}

// cc-switch streaming_responses.rs:1620 (completed event carrying
// status:"failed" is an error, both r→ directions).
func TestParity_CompletedWithFailedStatus(t *testing.T) {
	in := `data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"failed","error":{"message":"boom","type":"server_error"}}}` + "\n\n"

	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	eventsA := drainSSE(t, strings.NewReader(string(rawA)))
	if sseCount(eventsA, "error") != 1 || sseCount(eventsA, "message_stop") != 0 {
		t.Errorf("r→a: completed+failed must emit error without message_stop: %v", sseEventTypes(eventsA))
	}

	rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(in), "m"))
	outC := string(rawC)
	if !strings.Contains(outC, `"error"`) || strings.Contains(outC, `[DONE]`) {
		t.Errorf("r→chat: completed+failed must emit error chunk without [DONE]:\n%s", outC)
	}
}

// cc-switch streaming_codex_chat.rs:949 (DashScope: empty id/name fragments
// must not overwrite established identity) — chat→r direction.
func TestParity_EmptyFragmentKeepsIdentity(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_ds\",\"function\":{\"name\":\"exec\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"\",\"function\":{\"name\":\"\",\"arguments\":\"\\\"cmd\\\":\\\"date\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw, _ := io.ReadAll(newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
	out := string(raw)
	events := drainSSE(t, strings.NewReader(out))
	if n := sseCount(events, "response.output_item.added"); n != 1 {
		t.Errorf("output_item.added = %d, want 1: %v", n, sseEventTypes(events))
	}
	if strings.Contains(out, `"name":""`) || strings.Contains(out, `"call_id":""`) {
		t.Errorf("empty identity leaked into output:\n%s", out)
	}
	for _, ev := range events {
		if ev.event == "response.function_call_arguments.done" {
			m := unmarshalMap(t, []byte(ev.data))
			if m["arguments"] != `{"cmd":"date"}` || m["item_id"] != "call_ds" {
				t.Errorf("done = %v/%v, want call_ds + full args", m["item_id"], m["arguments"])
			}
		}
	}
}

// cc-switch streaming_codex_chat.rs:1037 (sparse tool_calls index — only
// index 2 — still closes cleanly).
func TestParity_SparseToolIndex(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":2,\"id\":\"call_2\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw, _ := io.ReadAll(newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
	events := drainSSE(t, strings.NewReader(string(raw)))
	assertResponsesItemPairing(t, events)
	if sseCount(events, "response.completed") != 1 {
		t.Errorf("sparse index: want exactly one completed: %v", sseEventTypes(events))
	}
}

// cc-switch streaming_codex_chat.rs:982 (parallel: index 0's name arrives
// AFTER index 1 completed). Contiguous release (ported from cc-switch): index
// 1's added is HELD until index 0's name arrives, so added order always
// matches output_index order.
func TestParity_ParallelLateNameOrder(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_first\",\"function\":{\"name\":\"\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_second\",\"function\":{\"name\":\"second\",\"arguments\":\"{\\\"v\\\":2}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"first\",\"arguments\":\"\\\"v\\\":1}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw, _ := io.ReadAll(newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
	events := drainSSE(t, strings.NewReader(string(raw)))
	var addedIDs []string
	for _, ev := range events {
		if ev.event == "response.output_item.added" {
			addedIDs = append(addedIDs, strOf(asMap(unmarshalMap(t, []byte(ev.data))["item"])["call_id"]))
		}
	}
	if len(addedIDs) != 2 || addedIDs[0] != "call_first" || addedIDs[1] != "call_second" {
		t.Errorf("added order = %v, want [call_first call_second] (contiguous release)", addedIDs)
	}
	assertResponsesItemPairing(t, events)
	doneArgs := map[string]string{}
	for _, ev := range events {
		if ev.event == "response.function_call_arguments.done" {
			m := unmarshalMap(t, []byte(ev.data))
			doneArgs[strOf(m["item_id"])] = strOf(m["arguments"])
		}
	}
	if doneArgs["call_first"] != `{"v":1}` || doneArgs["call_second"] != `{"v":2}` {
		t.Errorf("done args = %v, want call_first/call_second each intact", doneArgs)
	}
}

// cc-switch streaming_responses.rs:2158 (a multi-byte UTF-8 character split
// across transport chunks must not produce U+FFFD).
func TestParity_UTF8BoundarySplit(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"你好，世界\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	// 7-byte fixed chunks slice through multi-byte characters.
	raw, _ := io.ReadAll(newOpenAIToResponsesSSE(&fixedChunksReader{b: []byte(in), n: 7}, "m"))
	out := string(raw)
	if !strings.Contains(out, "你好，世界") {
		t.Errorf("multi-byte content corrupted:\n%s", out)
	}
	if strings.Contains(out, "�") {
		t.Errorf("U+FFFD replacement char in output:\n%s", out)
	}
}

// --- r→chat request: cc-switch transform_codex_chat.rs request-side ports ---

// Port of cc-switch's reasoning-attachment suite (:2713-3074): reasoning
// items attach to the ADJACENT assistant message as reasoning_content, not a
// standalone assistant message (DeepSeek-style upstreams require
// reasoning_content on the message carrying tool_calls).
func TestParity_ResponsesToChat_ReasoningAttachment(t *testing.T) {
	// reasoning BEFORE the function_call group attaches forward to the
	// assistant tool_calls message.
	in := `{"model":"m","input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"need to inspect"}]},` +
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"done"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`
	msgs := unmarshalMap(t, mustConvertResponsesToChat(t, in))["messages"].([]any)
	asst := asMap(msgs[0])
	if asst["role"] != "assistant" || asst["reasoning_content"] != "need to inspect" {
		t.Errorf("reasoning not attached forward to tool_calls message: %v", asst)
	}
	if tcs, ok := asst["tool_calls"].([]any); !ok || len(tcs) != 1 {
		t.Errorf("tool_calls message malformed: %v", asst)
	}
	// No standalone reasoning assistant message anywhere.
	for _, m := range msgs {
		mm := asMap(m)
		if mm["role"] == "assistant" && mm["tool_calls"] == nil && mm["content"] == nil && m != msgs[0] {
			t.Errorf("standalone reasoning assistant message leaked: %v", mm)
		}
	}

	// TRAILING reasoning (after the tool turn, nothing follows) attaches
	// BACKWARD to the previous assistant message.
	in2 := `{"model":"m","input":[` +
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"done"},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"trailing thought"}]}]}`
	msgs2 := unmarshalMap(t, mustConvertResponsesToChat(t, in2))["messages"].([]any)
	if asMap(msgs2[0])["reasoning_content"] != "trailing thought" {
		t.Errorf("trailing reasoning not attached backward: %v", msgs2[0])
	}

	// reasoning with NO assistant anywhere, hit by a user-turn boundary →
	// dropped (+convertWarn): reasoning must not leak across a user turn
	// (cc-switch; assertion updated for the boundary-consumption fix — the
	// previous standalone-fallback here WAS the leak).
	in3 := `{"model":"m","input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"lone"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	msgs3 := unmarshalMap(t, mustConvertResponsesToChat(t, in3))["messages"].([]any)
	for _, m := range msgs3 {
		if asMap(m)["reasoning_content"] == "lone" {
			t.Errorf("boundary-less reasoning leaked onto %v", m)
		}
	}
	// reasoning as the LAST item with no assistant anywhere → standalone
	// fallback (no boundary to consume it, nothing better).
	in4 := `{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"trailing lone"}]}]}`
	msgs4 := unmarshalMap(t, mustConvertResponsesToChat(t, in4))["messages"].([]any)
	found := false
	for _, m := range msgs4 {
		if asMap(m)["reasoning_content"] == "trailing lone" {
			found = true
		}
	}
	if !found {
		t.Errorf("trailing lone reasoning lost: %v", msgs4)
	}
}

// Port of cc-switch issue #3557 guard suite (:4117-4346): tool_choice and
// parallel_tool_calls are dropped when the (filtered) tools list is empty.
func TestParity_ResponsesToChat_NoToolsDropsToolChoice(t *testing.T) {
	// No tools at all.
	in := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tool_choice":"auto","parallel_tool_calls":false}`
	out := unmarshalMap(t, mustConvertResponsesToChat(t, in))
	if _, ok := out["tool_choice"]; ok {
		t.Errorf("tool_choice kept without tools: %v", out)
	}
	if _, ok := out["parallel_tool_calls"]; ok {
		t.Errorf("parallel_tool_calls kept without tools: %v", out)
	}

	// Tools present but ALL filtered out (hosted tools only) → same drop.
	in2 := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"web_search"}],"tool_choice":"auto","parallel_tool_calls":true}`
	out2 := unmarshalMap(t, mustConvertResponsesToChat(t, in2))
	if _, ok := out2["tool_choice"]; ok {
		t.Errorf("tool_choice kept with empty filtered tools: %v", out2)
	}
	if _, ok := out2["parallel_tool_calls"]; ok {
		t.Errorf("parallel_tool_calls kept with empty filtered tools: %v", out2)
	}

	// Real tools present → both kept.
	in3 := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"auto","parallel_tool_calls":true}`
	out3 := unmarshalMap(t, mustConvertResponsesToChat(t, in3))
	if out3["tool_choice"] == nil || out3["parallel_tool_calls"] != true {
		t.Errorf("tool_choice/parallel_tool_calls dropped despite tools: %v", out3)
	}
}

// Port of cc-switch's collapse_system_messages_to_head (:2622-2712):
// mid-thread system/developer items are pulled to the head, order preserved.
func TestParity_ResponsesToChat_SystemCollapseToHead(t *testing.T) {
	in := `{"model":"m","instructions":"top",` +
		`"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"u1"}]},` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev-note"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"}]}]}`
	msgs := unmarshalMap(t, mustConvertResponsesToChat(t, in))["messages"].([]any)
	if asMap(msgs[0])["role"] != "system" || asMap(msgs[0])["content"] != "top" {
		t.Errorf("msgs[0] = %v, want system/top", msgs[0])
	}
	if asMap(msgs[1])["role"] != "system" || asMap(msgs[1])["content"] != "dev-note" {
		t.Errorf("msgs[1] = %v, want system/dev-note (collapsed to head)", msgs[1])
	}
	if asMap(msgs[2])["role"] != "user" {
		t.Errorf("msgs[2] = %v, want user (order preserved after collapse)", msgs[2])
	}
}

func mustConvertResponsesToChat(t *testing.T, in string) []byte {
	t.Helper()
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatalf("r→chat request: %v", err)
	}
	if strings.Contains(string(out), `"null"`) {
		t.Fatalf("literal null leaked: %s", out)
	}
	return out
}

// cc-switch streaming_codex_chat.rs:1057 (restores_custom_tool_input_stream_events):
// a chat tool_call to a custom tool restores as custom_tool_call +
// custom_tool_call_input.delta + custom_tool_call_input.done + output_item.done.
func TestParity_CustomToolInputStreamEvents(t *testing.T) {
	r2c := r2cCtx{custom: map[string]bool{"apply_patch": true}}
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_p\",\"type\":\"function\",\"function\":{\"name\":\"apply_patch\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"input\\\": \\\"*** Begin\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\" Patch\\\\n+line\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSENS(strings.NewReader(in), "g", r2c))
	assertEventSequence(t, events, []string{
		"response.created",
		"response.output_item.added",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",
		"response.completed",
	})
	assertResponsesItemPairing(t, events)
	item := asMap(sseDataMap(t, events[1])["item"])
	if item["type"] != "custom_tool_call" || item["call_id"] != "call_p" || item["name"] != "apply_patch" {
		t.Errorf("custom added item = %v", item)
	}
	// Deltas are the unwrapped content (prefix stripped, \n decoded).
	d1 := strOf(sseDataMap(t, events[2])["delta"])
	d2 := strOf(sseDataMap(t, events[3])["delta"])
	if d1 != "*** Begin" || d2 != " Patch\n+line" {
		t.Errorf("deltas = %q / %q", d1, d2)
	}
	if got := strOf(sseDataMap(t, events[4])["input"]); got != "*** Begin Patch\n+line" {
		t.Errorf("done input = %q", got)
	}
}

// cc-switch mapReasoningEffort dialects: r→chat renders reasoning.effort per
// provider family (provider.ChatReasoningMode).
func TestParity_ReasoningEffortDialects(t *testing.T) {
	mk := func(effort string) string {
		return `{"model":"g","reasoning":{"effort":"` + effort + `"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	}
	// thinking 系: zhipu/volcengine/kimi-code/deepseek → thinking.type
	for _, pid := range []string{"zhipu", "volcengine", "kimi-code", "deepseek"} {
		out, err := convertResponsesRequestToOpenAIFor([]byte(mk("high")), convertReqOpts{ProviderID: pid, ImageOK: true})
		if err != nil {
			t.Fatal(err)
		}
		m := unmarshalMap(t, out)
		if got := strOf(asMap(m["thinking"])["type"]); got != "enabled" {
			t.Errorf("%s: thinking = %v, want enabled", pid, m["thinking"])
		}
		if _, has := m["reasoning_effort"]; has {
			t.Errorf("%s: reasoning_effort must not be emitted in thinking mode", pid)
		}
		out2, _ := convertResponsesRequestToOpenAIFor([]byte(mk("none")), convertReqOpts{ProviderID: pid, ImageOK: true})
		if got := strOf(asMap(unmarshalMap(t, out2)["thinking"])["type"]); got != "disabled" {
			t.Errorf("%s: effort none → thinking = %v, want disabled", pid, got)
		}
	}
	// qwen-plan → enable_thinking bool.
	out, err := convertResponsesRequestToOpenAIFor([]byte(mk("high")), convertReqOpts{ProviderID: "qwen-plan", ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out)["enable_thinking"]; got != true {
		t.Errorf("qwen-plan enable_thinking = %v, want true", got)
	}
	out2, _ := convertResponsesRequestToOpenAIFor([]byte(mk("minimal")), convertReqOpts{ProviderID: "qwen-plan", ImageOK: true})
	if got := unmarshalMap(t, out2)["enable_thinking"]; got != false {
		t.Errorf("qwen-plan minimal → enable_thinking = %v, want false", got)
	}
	// aqp (OpenRouter 系) → native reasoning object.
	out3, err := convertResponsesRequestToOpenAIFor([]byte(mk("medium")), convertReqOpts{ProviderID: "aqp", ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strOf(asMap(unmarshalMap(t, out3)["reasoning"])["effort"]); got != "medium" {
		t.Errorf("aqp reasoning = %v", unmarshalMap(t, out3)["reasoning"])
	}
	// Default: flat reasoning_effort unchanged.
	out4, err := convertResponsesRequestToOpenAIFor([]byte(mk("low")), convertReqOpts{ProviderID: "static", ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out4)["reasoning_effort"]; got != "low" {
		t.Errorf("default reasoning_effort = %v", got)
	}

	// No reasoning in the request → no thinking/enable_thinking keys either.
	noR := `{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	for _, pid := range []string{"zhipu", "qwen-plan", "aqp"} {
		out5, _ := convertResponsesRequestToOpenAIFor([]byte(noR), convertReqOpts{ProviderID: pid, ImageOK: true})
		m := unmarshalMap(t, out5)
		if _, has := m["thinking"]; has {
			t.Errorf("%s: no reasoning → thinking must be absent", pid)
		}
		if _, has := m["enable_thinking"]; has {
			t.Errorf("%s: no reasoning → enable_thinking must be absent", pid)
		}
	}
}

// Pool virtual names (name#id) normalize to the parent's provider id before
// the dialect lookup (providerConfig resolves via parentOf).
func TestParity_ReasoningDialectPooledProvider(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://x"}},
	}
	parentOf := map[string]string{"zhipu#ab12": "zhipu"}
	prov, ok := providerConfig(cfg, parentOf, "zhipu#ab12")
	if !ok {
		t.Fatal("providerConfig did not resolve pooled virtual")
	}
	out, err := convertRequestFor([]byte(`{"model":"g","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), "responses", "openai", convertReqOpts{ProviderID: prov.Provider, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strOf(asMap(unmarshalMap(t, out)["thinking"])["type"]); got != "enabled" {
		t.Errorf("pooled zhipu#ab12 → thinking = %v, want enabled (parent id zhipu)", got)
	}
}

// opencodex chat/outbound.ts:317-343 (done-only tool CALL, both r→
// directions): a gateway that skips output_item.added AND arguments deltas,
// sending only output_item.done with the complete function_call item, must
// still yield the whole tool call instead of dropping it silently.
func TestParity_DoneOnlyToolCallSynthesized(t *testing.T) {
	in := `data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"

	// r→anthropic: a complete tool_use block (start → args delta → stop) and a
	// tool_use stop_reason.
	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	eventsA := drainSSE(t, strings.NewReader(string(rawA)))
	assertEventSequence(t, eventsA, []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})
	start := asMap(sseDataMap(t, eventsA[1])["content_block"])
	if start["type"] != "tool_use" || start["id"] != "call_1" || start["name"] != "search" {
		t.Errorf("r→a synthesized block = %v", start)
	}
	if d := strOf(asMap(sseDataMap(t, eventsA[2])["delta"])["partial_json"]); d != `{"q":"x"}` {
		t.Errorf("r→a synthesized args = %q", d)
	}
	if sr := strOf(asMap(sseDataMap(t, eventsA[4])["delta"])["stop_reason"]); sr != "tool_use" {
		t.Errorf("r→a stop_reason = %q, want tool_use", sr)
	}

	// r→chat: one tool_calls chunk carrying id+name+arguments, finish tool_calls.
	rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(in), "m"))
	outC := string(rawC)
	if !strings.Contains(outC, `"id":"call_1"`) || !strings.Contains(outC, `"name":"search"`) ||
		!strings.Contains(outC, `"arguments":"{\"q\":\"x\"}"`) {
		t.Errorf("r→chat done-only tool call lost or incomplete:\n%s", outC)
	}
	if !strings.Contains(outC, `"finish_reason":"tool_calls"`) {
		t.Errorf("r→chat finish_reason ≠ tool_calls:\n%s", outC)
	}
}

// opencodex chat/outbound.ts:291-300 (arguments delta BEFORE output_item.added,
// both r→ directions): early deltas must be buffered (by item_id/output_index)
// and replayed when the item opens — dropping them corrupts the arguments JSON.
func TestParity_ArgsDeltaBeforeAdded(t *testing.T) {
	in := `data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"q\":"}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"\"x\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"

	// r→anthropic: the early delta lands on the opened block, in order — the
	// concatenated partial_json is the complete arguments.
	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	eventsA := drainSSE(t, strings.NewReader(string(rawA)))
	var argsA strings.Builder
	for _, ev := range eventsA {
		d := asMap(sseDataMap(t, ev)["delta"])
		if strOf(d["type"]) == "input_json_delta" {
			argsA.WriteString(strOf(d["partial_json"]))
		}
	}
	if argsA.String() != `{"q":"x"}` {
		t.Errorf("r→a concatenated args = %q, want %q (early delta lost or reordered):\n%s", argsA.String(), `{"q":"x"}`, rawA)
	}
	// The done frame's arguments must NOT double-emit after deltas were seen.
	if n := sseCount(eventsA, "content_block_delta"); n != 2 {
		t.Errorf("r→a content_block_delta = %d, want 2 (no double-emit from done)", n)
	}

	// r→chat: concatenated arguments chunks reconstruct the full JSON.
	rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(in), "m"))
	eventsC := drainSSE(t, strings.NewReader(string(rawC)))
	var argsC strings.Builder
	for _, ev := range eventsC {
		if ev.data == "[DONE]" {
			continue
		}
		m := sseDataMap(t, ev)
		choices, _ := m["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta := asMap(asMap(choices[0])["delta"])
		tcs, _ := delta["tool_calls"].([]any)
		for _, tc := range tcs {
			argsC.WriteString(strOf(asMap(asMap(tc)["function"])["arguments"]))
		}
	}
	if argsC.String() != `{"q":"x"}` {
		t.Errorf("r→chat concatenated args = %q, want %q (early delta lost):\n%s", argsC.String(), `{"q":"x"}`, rawC)
	}
}

// cc-switch codex_chat_common.rs:8-34 (reasoning dialects on STREAM deltas,
// chat→r): reasoning_content > reasoning (string/object) > reasoning_details.
func TestParity_ChatToResponses_ReasoningDetailsDialect(t *testing.T) {
	// reasoning_details array on the delta.
	in := "data: {\"choices\":[{\"delta\":{\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"d1\"},{\"type\":\"reasoning.summary\",\"summary\":\"d2\"}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
	var got []string
	for _, ev := range events {
		if ev.event == "response.reasoning_summary_text.delta" {
			got = append(got, strOf(sseDataMap(t, ev)["delta"]))
		}
	}
	if len(got) != 1 || got[0] != "d1\n\nd2" {
		t.Errorf("reasoning_details deltas = %v, want [d1\\n\\nd2]", got)
	}
	// reasoning as an OBJECT on the delta.
	in2 := "data: {\"choices\":[{\"delta\":{\"reasoning\":{\"text\":\"obj-text\"}}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events2 := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in2), "m"))
	found := false
	for _, ev := range events2 {
		if ev.event == "response.reasoning_summary_text.delta" && strOf(sseDataMap(t, ev)["delta"]) == "obj-text" {
			found = true
		}
	}
	if !found {
		t.Errorf("reasoning object delta lost: %v", sseEventTypes(events2))
	}
}

// cc-switch transform_codex_chat.rs:1012-1045: pending reasoning must NOT leak
// across a user-turn boundary into the next assistant message — at the
// boundary it attaches BACKWARD to the previous assistant (appended), and is
// dropped (+warn) only when no assistant exists to take it.
func TestParity_ResponsesToChat_ReasoningUserBoundary(t *testing.T) {
	in := `{"model":"m","input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"r1"}]},` +
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"done"},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"r2"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"next question"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
	msgs := unmarshalMap(t, mustConvertResponsesToChat(t, in))["messages"].([]any)
	// msgs: assistant(tool_calls), tool, user, assistant(answer).
	if len(msgs) != 4 {
		t.Fatalf("msgs = %d, want 4: %v", len(msgs), msgs)
	}
	// r2 must NOT leak onto the post-boundary assistant message.
	if rc := strOpt(asMap(msgs[3])["reasoning_content"]); rc != "" {
		t.Errorf("reasoning leaked across user boundary: %q", rc)
	}
	// r1 attached forward to the tool_calls message; r2 appended backward at
	// the user boundary (\n\n separator, cc-switch append_reasoning_content).
	if rc := strOpt(asMap(msgs[0])["reasoning_content"]); rc != "r1\n\nr2" {
		t.Errorf("boundary reasoning = %q, want %q", rc, "r1\n\nr2")
	}
}

// cc-switch streaming_responses.rs:1229-1231: response.incomplete carries
// usage too (a max_tokens truncation is exactly what costs money) — both r→
// stream directions must surface it in the terminal usage.
func TestParity_IncompleteCarriesUsage(t *testing.T) {
	in := `data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n" +
		`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n"

	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	eventsA := drainSSE(t, strings.NewReader(string(rawA)))
	var usageA map[string]any
	for _, ev := range eventsA {
		if ev.event == "message_delta" {
			usageA = asMap(sseDataMap(t, ev)["usage"])
		}
	}
	if usageA["input_tokens"] != float64(8) || usageA["output_tokens"] != float64(7) || usageA["cache_read_input_tokens"] != float64(3) {
		t.Errorf("r→a incomplete usage = %v, want 8/7 cached 3", usageA)
	}

	rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(in), "m"))
	eventsC := drainSSE(t, strings.NewReader(string(rawC)))
	var usageC map[string]any
	var finish string
	for _, ev := range eventsC {
		if ev.data == "[DONE]" {
			continue
		}
		m := sseDataMap(t, ev)
		if u := asMap(m["usage"]); u != nil {
			usageC = u
		}
		if ch := asMap(choices0(m)); ch != nil && strOpt(ch["finish_reason"]) != "" {
			finish = strOpt(ch["finish_reason"])
		}
	}
	if finish != "length" {
		t.Errorf("r→chat incomplete finish = %q, want length", finish)
	}
	if usageC["prompt_tokens"] != float64(11) || usageC["completion_tokens"] != float64(7) {
		t.Errorf("r→chat incomplete usage = %v, want 11/7", usageC)
	}
}

// choices0 is a tiny test helper: choices[0] of a chat chunk (nil-safe).
func choices0(m map[string]any) any {
	ch, _ := m["choices"].([]any)
	if len(ch) == 0 {
		return nil
	}
	return ch[0]
}

// P1: a completed event carrying status "cancelled" or a non-null error must
// be treated as a failure in BOTH r→ stream directions (the non-streaming
// converters already fail closed on cancelled).
func TestParity_CompletedCancelledOrErrorIsError(t *testing.T) {
	mk := func(resp string) string {
		return `data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n" +
			`data: {"type":"response.completed","response":` + resp + `}` + "\n\n"
	}
	cases := map[string]string{
		"cancelled":    `{"id":"r1","status":"cancelled","error":{"message":"user abort","type":"cancelled"}}`,
		"errorNonNull": `{"id":"r1","status":"completed","error":{"message":"late failure","type":"server_error"}}`,
	}
	for name, resp := range cases {
		rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(mk(resp)), "m"))
		eventsA := drainSSE(t, strings.NewReader(string(rawA)))
		if sseCount(eventsA, "error") != 1 || sseCount(eventsA, "message_stop") != 0 {
			t.Errorf("%s: r→a must emit error without message_stop: %v", name, sseEventTypes(eventsA))
		}
		rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(mk(resp)), "m"))
		outC := string(rawC)
		if !strings.Contains(outC, `"error"`) || strings.Contains(outC, `[DONE]`) {
			t.Errorf("%s: r→chat must emit error chunk without [DONE]:\n%s", name, outC)
		}
	}
}

// P1: response.refusal.delta streams as visible text — r→a emits text_delta,
// r→chat emits delta.content (aligned with the convert.go refusal→text fix).
func TestParity_RefusalDeltaStreamsAsText(t *testing.T) {
	in := `data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.refusal.delta","output_index":0,"delta":"I cannot "}` + "\n\n" +
		`data: {"type":"response.refusal.delta","output_index":0,"delta":"help with that."}` + "\n\n" +
		`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}` + "\n\n"

	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	eventsA := drainSSE(t, strings.NewReader(string(rawA)))
	var textA strings.Builder
	for _, ev := range eventsA {
		d := asMap(sseDataMap(t, ev)["delta"])
		if strOf(d["type"]) == "text_delta" {
			textA.WriteString(strOf(d["text"]))
		}
	}
	if textA.String() != "I cannot help with that." {
		t.Errorf("r→a refusal text = %q", textA.String())
	}
	var stopA string
	for _, ev := range eventsA {
		if ev.event == "message_delta" {
			stopA = strOf(asMap(sseDataMap(t, ev)["delta"])["stop_reason"])
		}
	}
	if stopA != "refusal" {
		t.Errorf("r→a stop_reason = %q, want refusal", stopA)
	}

	rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(in), "m"))
	outC := string(rawC)
	if !strings.Contains(outC, `"content":"I cannot "`) || !strings.Contains(outC, `"content":"help with that."`) {
		t.Errorf("r→chat refusal content lost:\n%s", outC)
	}
	if !strings.Contains(outC, `"finish_reason":"content_filter"`) {
		t.Errorf("r→chat finish_reason ≠ content_filter:\n%s", outC)
	}
}

// cc-switch streaming_codex_chat.rs:777,824-844: a chat SSE frame with an
// explicit `event: error` line is an error even when the payload has no
// top-level "error" key — extract message/detail from the bare payload.
func TestParity_ChatStreamEventErrorLine(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"event: error\n" +
		"data: {\"message\":\"upstream exploded\"}\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
	failed := sseFilter(events, "response.failed")
	if len(failed) != 1 {
		t.Fatalf("response.failed = %d, want 1: %v", len(failed), sseEventTypes(events))
	}
	resp := asMap(sseDataMap(t, failed[0])["response"])
	if got := strOf(asMap(resp["error"])["message"]); got != "upstream exploded" {
		t.Errorf("error message = %q", got)
	}
	if sseCount(events, "response.completed") != 0 {
		t.Errorf("error stream must not complete cleanly: %v", sseEventTypes(events))
	}
	// detail spelling also works.
	in2 := "event: error\ndata: {\"detail\":\"bad key\"}\n\n"
	events2 := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in2), "m"))
	failed2 := sseFilter(events2, "response.failed")
	if len(failed2) != 1 || strOf(asMap(asMap(sseDataMap(t, failed2[0])["response"])["error"])["message"]) != "bad key" {
		t.Errorf("detail payload: %v", sseEventTypes(events2))
	}
}

// cc-switch streaming_codex_chat.rs:175-265 (inline <think> splitting,
// chat→r stream): a LEADING <think>…</think> block inline in content streams
// as reasoning, the rest as text — even when tags straddle chunk boundaries;
// an unterminated leading block ends up entirely as reasoning.
func TestParity_ChatToResponses_InlineThinkStream(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"<thi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"nk>let me\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" think</think>\\n\\nthe\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" answer\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
	assertResponsesItemPairing(t, events)
	var reasoning, text strings.Builder
	for _, ev := range events {
		m := sseDataMap(t, ev)
		switch ev.event {
		case "response.reasoning_summary_text.delta":
			reasoning.WriteString(strOf(m["delta"]))
		case "response.output_text.delta":
			text.WriteString(strOf(m["delta"]))
		}
	}
	if reasoning.String() != "let me think" {
		t.Errorf("reasoning = %q, want %q", reasoning.String(), "let me think")
	}
	if text.String() != "the answer" {
		t.Errorf("text = %q, want think block stripped", text.String())
	}
	// Unterminated leading think block → entirely reasoning at stream end.
	in2 := "data: {\"choices\":[{\"delta\":{\"content\":\"<think>never closed\"}}]}\n\n" +
		"data: [DONE]\n\n"
	events2 := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in2), "m"))
	var r2, t2 strings.Builder
	for _, ev := range events2 {
		m := sseDataMap(t, ev)
		switch ev.event {
		case "response.reasoning_summary_text.delta":
			r2.WriteString(strOf(m["delta"]))
		case "response.output_text.delta":
			t2.WriteString(strOf(m["delta"]))
		}
	}
	if r2.String() != "never closed" || t2.String() != "" {
		t.Errorf("unterminated: reasoning=%q text=%q, want all reasoning", r2.String(), t2.String())
	}
	// Plain text (no think) streams through untouched.
	in3 := "data: {\"choices\":[{\"delta\":{\"content\":\"plain <think>not leading</think>\"}}]}\n\n" +
		"data: [DONE]\n\n"
	events3 := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in3), "m"))
	var t3 strings.Builder
	for _, ev := range events3 {
		if ev.event == "response.output_text.delta" {
			t3.WriteString(strOf(sseDataMap(t, ev)["delta"]))
		}
	}
	if t3.String() != "plain <think>not leading</think>" {
		t.Errorf("non-leading think altered: %q", t3.String())
	}
}
