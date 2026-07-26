package main

// convert_fixups_test.go — regression tests for three live-upstream-verified
// fixes: #8 placeholder reasoning_content (thinking-dialect upstreams 400 on
// bare tool_calls messages), #6 vision gating for media reinjection
// (deepseek 400s on image_url parts), #7 sequence_number on synthesized
// response.* frames.

import (
	"strings"
	"testing"
)

// #8: thinking-dialect providers get a "tool call" placeholder on assistant
// tool_calls messages lacking reasoning_content; everything else untouched.
func TestConvertFixup_PlaceholderReasoningContent(t *testing.T) {
	// Codex shape: reasoning item with EMPTY summary + encrypted_content, so
	// the attached reasoning_content is exactly empty → placeholder kicks in.
	// Sequence: reasoning("real thought") attaches forward to the c1 assistant;
	// the tool message in between forces c2 into a NEW (bare) assistant.
	in := `{"model":"g","reasoning":{"effort":"high"},"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"enc"},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"real thought"}],"encrypted_content":"enc2"},` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"},` +
		`{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"}]}`
	out, err := convertResponsesRequestToOpenAIFor([]byte(in), convertReqOpts{ProviderID: "deepseek", ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	var bareCalls, reasonedCalls, plainAssistant int
	for _, m := range msgs {
		mm := asMap(m)
		if mm["role"] != "assistant" {
			continue
		}
		tcs, _ := mm["tool_calls"].([]any)
		if len(tcs) == 0 {
			plainAssistant++
			if _, has := mm["reasoning_content"]; has {
				t.Errorf("assistant without tool_calls gained reasoning_content: %v", mm)
			}
			continue
		}
		rc := strOpt(mm["reasoning_content"])
		switch rc {
		case "tool call":
			bareCalls++
		case "real thought":
			reasonedCalls++
		default:
			t.Errorf("unexpected reasoning_content %q on %v", rc, mm)
		}
	}
	if bareCalls != 1 {
		t.Errorf("bare tool_calls message not injected (count=%d): %s", bareCalls, out)
	}
	if reasonedCalls != 1 {
		t.Errorf("existing reasoning_content overwritten (count=%d): %s", reasonedCalls, out)
	}

	// Non-thinking dialect: no injection.
	out2, err := convertResponsesRequestToOpenAIFor([]byte(in), convertReqOpts{ProviderID: "static", ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range unmarshalMap(t, out2)["messages"].([]any) {
		mm := asMap(m)
		if mm["role"] == "assistant" && strOpt(mm["reasoning_content"]) == "tool call" {
			t.Errorf("non-thinking provider got placeholder: %s", out2)
		}
	}
}

// #6 unit: capability resolution order (config caps > catalog > default true).
func TestConvertFixup_ImageOKForTarget(t *testing.T) {
	cat := &modelsDevCatalog{ByName: map[string]modelsDevModel{
		"m-vision": {Input: []string{"text", "image"}},
		"m-text":   {Input: []string{"text"}},
	}}
	tgt := RouteTarget{Provider: "p", Model: "m-vision"}
	// No caps anywhere: catalog decides.
	cfg := &Config{Providers: map[string]Provider{"p": {Provider: "static"}}}
	if !imageOKForTarget(cfg, nil, cat, tgt) {
		t.Error("vision model via catalog must be image-ok")
	}
	if imageOKForTarget(cfg, nil, cat, RouteTarget{Provider: "p", Model: "m-text"}) {
		t.Error("text-only model via catalog must NOT be image-ok")
	}
	// Unknown model: default true (keep reinjecting).
	if !imageOKForTarget(cfg, nil, cat, RouteTarget{Provider: "p", Model: "m-unknown"}) {
		t.Error("unknown model must default to image-ok")
	}
	// Nil catalog: default true.
	if !imageOKForTarget(cfg, nil, nil, tgt) {
		t.Error("nil catalog must default to image-ok")
	}
	// Config capabilities override wins over the catalog both ways.
	cfgCaps := &Config{Providers: map[string]Provider{"p": {Provider: "static", Capabilities: map[string][]string{
		"m-vision": {"text"},          // declared text-only → false despite catalog
		"m-text":   {"text", "image"}, // declared vision → true despite catalog
	}}}}
	if imageOKForTarget(cfgCaps, nil, cat, tgt) {
		t.Error("capabilities override (text-only) must beat catalog vision")
	}
	if !imageOKForTarget(cfgCaps, nil, cat, RouteTarget{Provider: "p", Model: "m-text"}) {
		t.Error("capabilities override (image) must beat catalog text-only")
	}
}

// #6 conversion: without vision, tool_result images fold into the placeholder
// text inside the tool message (no synthetic user message, no image parts).
func TestConvertFixup_VisionGateConversion(t *testing.T) {
	const noVision = "[image omitted: target model has no vision capability]"

	// a→chat.
	out, err := convertAnthropicRequestToOpenAIV([]byte(
		`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("no-vision messages = %d, want 1 (no synthetic): %s", len(msgs), out)
	}
	if got := strOpt(asMap(msgs[0])["content"]); got != noVision {
		t.Errorf("tool content = %q, want placeholder", got)
	}
	if strings.Contains(string(out), "image_url") {
		t.Errorf("image part leaked without vision: %s", out)
	}

	// a→r.
	out2, err := convertAnthropicRequestToResponsesV([]byte(
		`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	input2, _ := unmarshalMap(t, out2)["input"].([]any)
	if len(input2) != 1 {
		t.Fatalf("no-vision input items = %d, want 1: %s", len(input2), out2)
	}
	if got := strOpt(asMap(input2[0])["output"]); got != noVision {
		t.Errorf("fco output = %q, want placeholder", got)
	}

	// r→chat (parts-array output).
	out3, err := convertResponsesRequestToOpenAIFor([]byte(
		`{"model":"g","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"plot:"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`),
		convertReqOpts{ImageOK: false})
	if err != nil {
		t.Fatal(err)
	}
	msgs3, _ := unmarshalMap(t, out3)["messages"].([]any)
	if len(msgs3) != 1 {
		t.Fatalf("no-vision r→chat messages = %d, want 1: %s", len(msgs3), out3)
	}
	if got := strOpt(asMap(msgs3[0])["content"]); !strings.Contains(got, "plot:") || !strings.Contains(got, noVision) {
		t.Errorf("tool content = %q, want text + placeholder", got)
	}
	if strings.Contains(string(out3), "image_url") {
		t.Errorf("image part leaked without vision: %s", out3)
	}
}

// #7: synthesized response.* frames carry an incrementing sequence_number
// (0,1,2...) on every frame, both synthesis directions.
func TestConvertFixup_SequenceNumbers(t *testing.T) {
	check := func(name string, events []sseEvent) {
		t.Helper()
		for i, ev := range events {
			if ev.data == "[DONE]" {
				continue
			}
			m := unmarshalMap(t, []byte(ev.data))
			got, ok := m["sequence_number"].(float64)
			if !ok {
				t.Fatalf("%s: frame %d missing sequence_number: %s", name, i, ev.data)
			}
			if int(got) != i {
				t.Fatalf("%s: frame %d sequence_number = %v, want %d", name, i, got, i)
			}
		}
	}
	check("a→r", drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(anthropicTextStream), "c")))
	chatIn := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	check("chat→r", drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(chatIn), "g")))
}
