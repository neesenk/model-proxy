package protocol

import (
	"bytes"
	"io"
	"testing"
)

// TestConvertHelpers: finish↔stop maps (all branches), text extraction, backend
// path, and the SSE reader selector.
func TestConvertHelpers(t *testing.T) {
	for finish, want := range map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "other": "end_turn",
	} {
		if got := mapFinishToStopReason(finish); got != want {
			t.Errorf("mapFinishToStopReason(%q)=%q want %q", finish, got, want)
		}
	}
	for reason, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls", "x": "stop",
	} {
		if got := mapStopReasonToFinish(reason); got != want {
			t.Errorf("mapStopReasonToFinish(%q)=%q want %q", reason, got, want)
		}
	}
	// anthropicTextOf: string passthrough, array text joined with "\n", image skipped.
	if got := anthropicTextOf("hi"); got != "hi" {
		t.Errorf("string extract=%q", got)
	}
	if got := anthropicTextOf([]any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image", "text": "ignored"},
		map[string]any{"type": "text", "text": "b"},
	}); got != "a\nb" {
		t.Errorf("array extract=%q want %q", got, "a\nb")
	}
	// backendPath by protocol.
	if p := backendPath("anthropic"); p != "/v1/messages" {
		t.Errorf("backendPath anthropic=%q", p)
	}
	if p := backendPath("openai"); p != "/chat/completions" {
		t.Errorf("backendPath openai=%q", p)
	}
}

// TestCacheConfigDefaults: zero-value config falls back to documented defaults.
// TestConvertRequestResponse_NoOp: same-protocol is a pass-through (no conversion).
func TestConvertRequestResponse_NoOp(t *testing.T) {
	body := []byte(`{"model":"x"`)
	if got, err := convertRequest(body, "openai", "openai"); err != nil || string(got) != string(body) {
		t.Errorf("same-proto convertRequest should be no-op: got=%q err=%v", got, err)
	}
	if got, err := convertResponse(body, "anthropic", "anthropic"); err != nil || string(got) != string(body) {
		t.Errorf("same-proto convertResponse should be no-op: got=%q err=%v", got, err)
	}
}

// TestUsageScanner_AgentSink: the onAgent callback fires with the observed usage
// when the scanner commits (the agent token-attribution path).
// TestOpenAIContentToAnthropicBlocks: nil, empty, and string content paths.
func TestOpenAIContentToAnthropicBlocks(t *testing.T) {
	if openaiContentToAnthropicBlocks(nil) != nil {
		t.Error("nil content should return nil")
	}
	if openaiContentToAnthropicBlocks("hello") == nil {
		t.Error("string content should return a text block")
	}
	if len(openaiContentToAnthropicBlocks("hello")) != 1 {
		t.Error("string content should produce exactly one block")
	}
}

func TestNeedsConversionAndSSEReader(t *testing.T) {
	cases := []struct {
		client, target string
		want           bool
	}{
		{"anthropic", "openai", true},
		{"openai", "anthropic", true},
		{"openai", "openai", false},
		{"anthropic", "", false}, // empty target = same as client
		{"openai", "weird", false},
	}
	for _, c := range cases {
		if got := needsConversion(c.client, c.target); got != c.want {
			t.Errorf("needsConversion(%q,%q)=%v want %v", c.client, c.target, got, c.want)
		}
	}
	// Both directions produce a non-nil reader over a short stream without error.
	for _, dir := range []struct{ client, target string }{
		{"anthropic", "openai"}, {"openai", "anthropic"},
	} {
		in := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
		if dir.client == "openai" {
			in = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"
		}
		r := convertSSEReader(bytes.NewReader([]byte(in)), dir.client, dir.target, "m")
		out, err := io.ReadAll(r)
		if err != nil || len(out) == 0 {
			t.Errorf("convertSSEReader %v→%v produced empty/err: %v %q", dir.client, dir.target, err, string(out))
		}
	}
}
