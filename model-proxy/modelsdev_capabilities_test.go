package main

import "testing"

func TestParseModelsDevAPI_ToolCall(t *testing.T) {
	blob := []byte(`{"anthropic":{"api":"anthropic","models":{"claude-sonnet-4":{"limit":{"context":200000,"output":8192},"modalities":{"input":["text","image"],"output":["text"]},"features":{"tool_call":true}},"claude-3-haiku":{"limit":{"context":200000,"output":4096},"modalities":{"input":["text"],"output":["text"]}}}}}`)
	cat := parseModelsDevAPI(blob)
	m, ok := cat.lookup("claude-sonnet-4")
	if !ok {
		t.Fatal("claude-sonnet-4 not found")
	}
	if !m.ToolCall {
		t.Error("claude-sonnet-4 should have tool_call=true")
	}
	h, ok := cat.lookup("claude-3-haiku")
	if !ok {
		t.Fatal("claude-3-haiku not found")
	}
	if h.ToolCall {
		t.Error("claude-3-haiku should have tool_call=false (features absent)")
	}
}

// TestShouldShadow: rate=0 → false, rate>=1 → true, rate between → probabilistic.
