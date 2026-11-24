package requestlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// anthropicUsageStream builds a minimal anthropic message stream whose frames
// carry only the usage under test: one message_start, zero or more
// message_delta frames (each payload is the frame's usage object), closed by
// message_stop.
func anthropicUsageStream(start string, deltas ...string) string {
	var b strings.Builder
	b.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":" + start + "}}\n\n")
	for _, usage := range deltas {
		b.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":" + usage + "}\n\n")
	}
	b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return b.String()
}

// TestExtractUsageAnthropicTerminalUsageFrameIsFinal: message_delta is the
// terminal usage frame of an anthropic message stream — a field it carries,
// including an explicit zero, is the stream's final value, and message_start
// supplies only the fields it omits (a native anthropic stream puts just
// output_tokens there).
//
// Regression (serve audit 2026-11-24): these frames were merged across frames
// by per-field maximum, on the assumption that both frames repeat cumulative
// counters of the same quantity. Two live dialects break it:
//
//   - step-plan's native /v1/messages reports the WHOLE prompt in
//     message_start as a placeholder split (input_tokens 1 +
//     cache_read_input_tokens prompt-1) and the real split only in
//     message_delta. Max-merge kept the placeholder: 8 pi requests on
//     2026-11-24 were indexed with 94,668 cache-read tokens where their
//     message_delta frames reported 60,160 — two requests with no cache hit
//     at all were billed 4,925 and 5,406.
//   - kimi-code moves input into cache_read at the terminal event
//     (message_start input 222 / cache_read 0, message_delta input 0 /
//     cache_read 222). Max-merge kept both: 444 prompt tokens for 222.
//
// The anthropic→openai streaming converter already reads these frames by
// field presence (anthropicSSEToOpenAISSE, "Kimi moves input into
// cache_read at the terminal event"); this is the same rule, so an
// openai-protocol client and an anthropic-protocol client read one ledger off
// one upstream stream.
func TestExtractUsageAnthropicTerminalUsageFrameIsFinal(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Usage
	}{
		{
			name: "step-plan: placeholder split at message_start, real split at message_delta",
			body: anthropicUsageStream(
				`{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":10533}`,
				`{"input_tokens":5670,"output_tokens":672,"cache_creation_input_tokens":0,"cache_read_input_tokens":4864}`),
			want: Usage{Input: 5670, Output: 672, CacheRead: 4864},
		},
		{
			name: "step-plan: no cache hit — the explicit zero at message_delta is final",
			body: anthropicUsageStream(
				`{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":4925}`,
				`{"input_tokens":4926,"output_tokens":393,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`),
			want: Usage{Input: 4926, Output: 393},
		},
		{
			name: "kimi-code: input moved into cache_read at the terminal event",
			body: anthropicUsageStream(
				`{"input_tokens":222,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`,
				`{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":222,"output_tokens":82}`),
			want: Usage{Output: 82, CacheRead: 222},
		},
		{
			name: "deepseek: the same cumulative values in both frames count once",
			body: anthropicUsageStream(
				`{"input_tokens":39,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":256}`,
				`{"input_tokens":39,"output_tokens":72,"cache_creation_input_tokens":0,"cache_read_input_tokens":256}`),
			want: Usage{Input: 39, Output: 72, CacheRead: 256},
		},
		{
			name: "native anthropic: message_delta carries only output_tokens — message_start values stand",
			body: anthropicUsageStream(
				`{"input_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":1}`,
				`{"output_tokens":42}`),
			want: Usage{Input: 25, Output: 42, CacheRead: 40, CacheCreation: 10},
		},
		{
			name: "null at message_delta reports nothing — message_start value stands",
			body: anthropicUsageStream(
				`{"input_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":1}`,
				`{"output_tokens":42,"cache_read_input_tokens":null}`),
			want: Usage{Input: 25, Output: 42, CacheRead: 40, CacheCreation: 10},
		},
		{
			name: "cut before message_delta: message_start values stand",
			body: anthropicUsageStream(
				`{"input_tokens":39,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":256}`),
			want: Usage{Input: 39, CacheRead: 256},
		},
		{
			name: "a later message_delta supersedes an earlier one",
			body: anthropicUsageStream(
				`{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`,
				`{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10}`,
				`{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":100,"output_tokens":20}`),
			want: Usage{Output: 20, CacheRead: 100},
		},
	}
	for _, c := range cases {
		if got := ExtractUsage(c.body); got != c.want {
			t.Errorf("%s: ExtractUsage = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestExtractUsageAnthropicWireDialects pins the ledger against every
// recorded anthropic-protocol stream: each value is the stream's final usage
// (message_delta where it reports the field, else message_start).
func TestExtractUsageAnthropicWireDialects(t *testing.T) {
	cases := map[string]Usage{
		// step-plan: placeholder split at message_start, real split at message_delta.
		"anthropic_step-plan_tool.sse": {Input: 5670, Output: 672, CacheRead: 4864},
		// kimi-code: input moved into cache_read at the terminal event.
		"anthropic_kimi-code.sse":          {Output: 33, CacheRead: 93},
		"anthropic_kimi-code_thinking.sse": {Output: 48, CacheRead: 93},
		"anthropic_kimi-code_tool.sse":     {Output: 82, CacheRead: 222},
		// deepseek: the same cumulative values in both frames.
		"anthropic_deepseek_tool.sse": {Input: 39, Output: 72, CacheRead: 256},
		// aqp / shopee / zhipu: real values only at message_delta.
		"anthropic_aqp_tool.sse":    {Input: 112, Output: 11, CacheRead: 64},
		"anthropic_shopee_tool.sse": {Input: 232, Output: 45, CacheRead: 64},
		"anthropic_zhipu.sse":       {Input: 12, Output: 3},
		// qwen-plan: a token count at message_start that is not the final input.
		"anthropic_qwen-plan_tool.sse": {Input: 327, Output: 77},
		// native anthropic: message_delta carries only output_tokens.
		"anthropic_text_tool.sse": {Input: 25, Output: 42, CacheRead: 4},
	}
	for name, want := range cases {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "wire", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := ExtractUsage(string(raw)); got != want {
			t.Errorf("%s: ExtractUsage = %+v, want %+v", name, got, want)
		}
	}
}
