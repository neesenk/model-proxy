package app

// usage_ledger_test.go pins ONE usage ledger across the three independent
// extraction paths: the hot-path UsageScanner (counters → minute buckets /
// Accounts & Analytics), requestlog.ExtractUsage (request log index → Keys
// usage, session aggregates), and fusion.ParseUsage (orchestration legs).
// They must agree field-for-field on every wire spelling — the historical bug
// classes this guards against: cached shares landing in BOTH Input and
// CacheRead (double count, the deepseek discrepancy), cache writes folded
// into inclusive totals with no recoverable spelling (cost under-billed),
// and one parser supporting a dialect another misses.

import (
	"io"
	"strings"
	"testing"

	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
)

// scannerUsage runs one SSE/JSON body through the hot-path scanner and
// returns the committed usage.
func scannerUsage(t *testing.T, body string) counters.TokenUsage {
	t.Helper()
	tc := counters.NewTokenCounter()
	s := counters.NewUsageScanner(io.NopCloser(strings.NewReader(body)), counters.TokenKey{Provider: "p", Model: "m"}, tc, nil)
	if _, err := io.ReadAll(s); err != nil {
		t.Fatalf("scanner read: %v", err)
	}
	snap := tc.Snapshot()
	return snap[counters.TokenKey{Provider: "p", Model: "m"}]
}

func assertUsageEqual(t *testing.T, label string, got, want requestlog.Usage) {
	t.Helper()
	if got.Input != want.Input || got.Output != want.Output ||
		got.CacheRead != want.CacheRead || got.CacheCreation != want.CacheCreation {
		t.Errorf("%s usage = %+v, want %+v", label, got, want)
	}
}

// TestUsageLedgerConsistency_SSE runs every SSE dialect through the scanner
// and requestlog.ExtractUsage and requires identical four-bucket results.
func TestUsageLedgerConsistency_SSE(t *testing.T) {
	cases := []struct {
		name string
		sse  string
		want requestlog.Usage
	}{
		{
			name: "chat deepseek dialect hit",
			sse:  "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":90}}\n\ndata: [DONE]\n\n",
			want: requestlog.Usage{Input: 10, Output: 7, CacheRead: 90},
		},
		{
			name: "chat details hit+write",
			sse:  "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":60,\"cache_write_tokens\":30}}}\n\ndata: [DONE]\n\n",
			want: requestlog.Usage{Input: 10, Output: 7, CacheRead: 60, CacheCreation: 30},
		},
		{
			name: "chat direct write spelling",
			sse:  "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":40},\"cache_creation_input_tokens\":20}}\n\ndata: [DONE]\n\n",
			want: requestlog.Usage{Input: 40, Output: 7, CacheRead: 40, CacheCreation: 20},
		},
		{
			name: "responses completed with both details",
			sse:  "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":150,\"cache_write_tokens\":20}}}}\n\n",
			want: requestlog.Usage{Input: 30, Output: 9, CacheRead: 150, CacheCreation: 20},
		},
		{
			name: "responses completed direct write",
			sse:  "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":150},\"cache_creation_input_tokens\":20}}}\n\n",
			want: requestlog.Usage{Input: 30, Output: 9, CacheRead: 150, CacheCreation: 20},
		},
		{
			name: "anthropic native two frames",
			sse:  "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":34,\"cache_creation_input_tokens\":56}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":7}}\n\n",
			want: requestlog.Usage{Input: 12, Output: 7, CacheRead: 34, CacheCreation: 56},
		},
		{
			name: "clamped shares exceed total",
			sse:  "data: {\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":60}}}\n\ndata: [DONE]\n\n",
			want: requestlog.Usage{Input: 0, Output: 3, CacheRead: 60},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := scannerUsage(t, tc.sse)
			gotScanner := requestlog.Usage{Input: sc.Input, Output: sc.Output, CacheRead: sc.CacheRead, CacheCreation: sc.CacheCreation}
			assertUsageEqual(t, "scanner", gotScanner, tc.want)
			assertUsageEqual(t, "requestlog", requestlog.ExtractUsage(tc.sse), tc.want)
		})
	}
}

// TestUsageLedgerConsistency_JSON runs the non-streaming dialects through all
// THREE parsers (fusion.ParseUsage only consumes non-streaming bodies).
func TestUsageLedgerConsistency_JSON(t *testing.T) {
	cases := []struct {
		name string
		body string
		want requestlog.Usage
	}{
		{
			name: "chat deepseek dialect hit",
			body: `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_cache_hit_tokens":90}}`,
			want: requestlog.Usage{Input: 10, Output: 7, CacheRead: 90},
		},
		{
			name: "chat details hit+write",
			body: `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":30}}}`,
			want: requestlog.Usage{Input: 10, Output: 7, CacheRead: 60, CacheCreation: 30},
		},
		{
			name: "responses top-level usage with details",
			body: `{"id":"r","object":"response","usage":{"input_tokens":200,"output_tokens":9,"input_tokens_details":{"cached_tokens":150,"cache_write_tokens":20}}}`,
			want: requestlog.Usage{Input: 30, Output: 9, CacheRead: 150, CacheCreation: 20},
		},
		{
			name: "anthropic native non-stream",
			body: `{"id":"msg_1","usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":34,"cache_creation_input_tokens":56}}`,
			want: requestlog.Usage{Input: 12, Output: 7, CacheRead: 34, CacheCreation: 56},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := scannerUsage(t, tc.body)
			gotScanner := requestlog.Usage{Input: sc.Input, Output: sc.Output, CacheRead: sc.CacheRead, CacheCreation: sc.CacheCreation}
			assertUsageEqual(t, "scanner", gotScanner, tc.want)
			assertUsageEqual(t, "requestlog", requestlog.ExtractUsage(tc.body), tc.want)
			fu := fusion.ParseUsage([]byte(tc.body))
			assertUsageEqual(t, "fusion", requestlog.Usage{Input: fu.Input, Output: fu.Output, CacheRead: fu.CacheRead, CacheCreation: fu.CacheCreation}, tc.want)
		})
	}
}
