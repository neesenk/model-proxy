package main

// convert_media_test.go — tool output media reinjection (cc-switch
// strip-and-reinject, no vision gating): images inside tool results are
// re-delivered as a synthetic user message right after the tool message,
// across a→chat / a→responses / responses→chat. Image-free tool results are
// byte-identical to before.

import (
	"testing"
)

const (
	mediaPNG  = "data:image/png;base64,aGVsbG8="
	mediaJPEG = "data:image/jpeg;base64,d29ybGQ="
)

// a→chat: tool_result images → role:tool (text only) + synthetic user message.
func TestConvertMedia_AnthropicToChat(t *testing.T) {
	// Pure image (no text): the tool message keeps an EMPTY content, and the
	// image rides the synthetic user message.
	in := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`
	out, err := convertAnthropicRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (tool + synthetic user): %s", len(msgs), out)
	}
	tool := asMap(msgs[0])
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" || tool["content"] != "" {
		t.Errorf("tool message = %v", tool)
	}
	synth := asMap(msgs[1])
	if synth["role"] != "user" {
		t.Fatalf("synthetic message role = %v", synth["role"])
	}
	parts, _ := synth["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("synthetic parts = %d, want 2: %v", len(parts), parts)
	}
	if asMap(parts[0])["text"] != "[image returned by tool]" {
		t.Errorf("synthetic lead text = %v", asMap(parts[0]))
	}
	img := asMap(parts[1])
	if img["type"] != "image_url" || strOf(asMap(img["image_url"])["url"]) != mediaPNG {
		t.Errorf("synthetic image = %v, want data URL %s", img, mediaPNG)
	}

	// Multi-image incl. url source + text: text stays in the tool message,
	// both images reinjected in order.
	in2 := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":[` +
		`{"type":"text","text":"screenshots:"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},` +
		`{"type":"image","source":{"type":"url","url":"https://example.test/x.jpg"}}]}]}]}`
	out2, err := convertAnthropicRequestToOpenAI([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	msgs2, _ := unmarshalMap(t, out2)["messages"].([]any)
	if len(msgs2) != 2 {
		t.Fatalf("multi-image messages = %d: %s", len(msgs2), out2)
	}
	if asMap(msgs2[0])["content"] != "screenshots:" {
		t.Errorf("tool text = %v", asMap(msgs2[0])["content"])
	}
	parts2, _ := asMap(msgs2[1])["content"].([]any)
	if len(parts2) != 3 {
		t.Fatalf("multi-image synthetic parts = %d: %v", len(parts2), parts2)
	}
	if strOf(asMap(asMap(parts2[1])["image_url"])["url"]) != mediaPNG {
		t.Errorf("image[0] = %v", parts2[1])
	}
	if strOf(asMap(asMap(parts2[2])["image_url"])["url"]) != "https://example.test/x.jpg" {
		t.Errorf("image[1] url source not kept: %v", parts2[2])
	}

	// No image: unchanged behavior (no synthetic message).
	in3 := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":"plain text"}]}]}`
	out3, err := convertAnthropicRequestToOpenAI([]byte(in3))
	if err != nil {
		t.Fatal(err)
	}
	if msgs3, _ := unmarshalMap(t, out3)["messages"].([]any); len(msgs3) != 1 {
		t.Errorf("image-free tool_result messages = %d, want 1 (no synthetic): %s", len(msgs3), out3)
	}
}

// a→responses: tool_result images → function_call_output + synthetic user
// message item (input_text + input_image).
func TestConvertMedia_AnthropicToResponses(t *testing.T) {
	in := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"d29ybGQ="}}]}]}]}`
	out, err := convertAnthropicRequestToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	input, _ := unmarshalMap(t, out)["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input items = %d, want 2 (fco + synthetic): %s", len(input), out)
	}
	fco := asMap(input[0])
	if fco["type"] != "function_call_output" || fco["call_id"] != "t1" || fco["output"] != "" {
		t.Errorf("function_call_output = %v", fco)
	}
	synth := asMap(input[1])
	if synth["type"] != "message" || synth["role"] != "user" {
		t.Fatalf("synthetic item = %v", synth)
	}
	parts, _ := synth["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("synthetic parts = %d, want 3: %v", len(parts), parts)
	}
	if asMap(parts[0])["type"] != "input_text" || asMap(parts[0])["text"] != "[image returned by tool]" {
		t.Errorf("synthetic lead = %v", parts[0])
	}
	if asMap(parts[1])["type"] != "input_image" || asMap(parts[1])["image_url"] != mediaPNG {
		t.Errorf("synthetic image[0] = %v", parts[1])
	}
	if asMap(parts[2])["image_url"] != mediaJPEG {
		t.Errorf("synthetic image[1] = %v", parts[2])
	}

	// No image: unchanged.
	in2 := `{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":"plain text"}]}]}`
	out2, err := convertAnthropicRequestToResponses([]byte(in2))
	if err != nil {
		t.Fatal(err)
	}
	if input2, _ := unmarshalMap(t, out2)["input"].([]any); len(input2) != 1 {
		t.Errorf("image-free input items = %d, want 1: %s", len(input2), out2)
	}
}

// responses→chat: function_call_output with a parts-array output → text in
// the tool message, images reinjected as a synthetic user message.
func TestConvertMedia_ResponsesToChat(t *testing.T) {
	in := `{"model":"g","input":[` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_text","text":"plot:"},` +
		`{"type":"input_image","image_url":"` + mediaPNG + `"},` +
		`{"type":"input_image","image_url":"` + mediaJPEG + `"}]}]}`
	out, err := convertResponsesRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := unmarshalMap(t, out)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2: %s", len(msgs), out)
	}
	tool := asMap(msgs[0])
	if tool["role"] != "tool" || tool["content"] != "plot:" {
		t.Errorf("tool message = %v", tool)
	}
	parts, _ := asMap(msgs[1])["content"].([]any)
	if len(parts) != 3 || strOf(asMap(asMap(parts[1])["image_url"])["url"]) != mediaPNG ||
		strOf(asMap(asMap(parts[2])["image_url"])["url"]) != mediaJPEG {
		t.Errorf("synthetic parts = %v", parts)
	}

	// String output: unchanged (no synthetic).
	out2, err := convertResponsesRequestToOpenAI([]byte(
		`{"model":"g","input":[{"type":"function_call_output","call_id":"c1","output":"plain"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if msgs2, _ := unmarshalMap(t, out2)["messages"].([]any); len(msgs2) != 1 {
		t.Errorf("string-output messages = %d, want 1: %s", len(msgs2), out2)
	}
}
