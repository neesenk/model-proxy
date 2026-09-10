package counters

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDetectAgent(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		hdr  map[string]string
		want string
	}{
		{"claude session header", "", map[string]string{"x-claude-code-session-id": "s"}, "claude-code"},
		{"claude-cli ua", "claude-cli/1.2.3", nil, "claude-code"},
		{"codex ua", "codex_cli_rs/0.5.0", nil, "codex"},
		{"opencode ua", "opencode/0.1", nil, "opencode"},
		{"pi ua", "pi/2.0", nil, "pi"},
		// pi-ai (pi >= 0.85) sends "pi (<platform> <release>; <arch>)" for LLM calls.
		{"pi ai ua", "pi (darwin 25.6.0; arm64)", nil, "pi"},
		{"pi ai ua embedded", "somehost pi (darwin 25.6.0; arm64)", nil, "pi"},
		{"pi ua with version detail", "pi/0.85.1 (darwin; node/v26.8.1; arm64)", nil, "pi"},
		{"not pi (substring)", "pinecone/1.0", nil, "pinecone"},
		{"not pi (prefix only)", "pip/24.0", nil, "pip"},
		{"no ua", "", nil, "unknown"},
		// Unrecognized UAs fall back to their product token instead of a flat
		// "other", so unknown clients stay distinguishable.
		{"unrecognized curl", "curl/8.0", nil, "curl"},
		{"unrecognized go", "Go-http-client/2.0", nil, "go-http-client"},
		{"unrecognized python", "python-requests/2.31.0", nil, "python-requests"},
		{"unrecognized caps lowered", "OpenAI/JS 4.2", nil, "openai"},
		{"unrecognized truncated", "a-very-long-client-product-name/9.9.9", nil, "a-very-long-client-produ"},
		// No usable product token: label with the raw UA instead of "other".
		{"garbage raw ua", "()/*", nil, "()/*"},
		{"punct ua", "/§!", nil, "/\u00a7!"},
		{"whitespace collapsed", "  ()  /*  ", nil, "()-/*"},
		{"whitespace only", "   ", nil, "other"},
	}
	for _, c := range cases {
		r := &http.Request{Header: http.Header{}}
		if c.ua != "" {
			r.Header.Set("user-agent", c.ua)
		}
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := DetectAgent(r); got != c.want {
			t.Errorf("%s: DetectAgent = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMetricsStore(t *testing.T) {
	s := NewMetricsStore()
	k := PMKey{Provider: "p", Model: "m"}
	s.Inc("p", "m", EvRequests)
	s.Inc("p", "m", EvRequests)
	s.Inc("p", "m", EvFailures)
	s.AddLatency("p", "m", 100, 40)

	snap := s.Snapshot()[k]
	if snap.Requests != 2 || snap.Failures != 1 || snap.LatencySum != 100 || snap.TTFTSum != 40 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if s.StartedAt().IsZero() {
		t.Error("StartedAt must be set")
	}

	agg := s.AggregateByProvider()["p"]
	if agg.Requests != 2 || agg.Failures != 1 {
		t.Errorf("aggregate = %+v", agg)
	}

	s.Seed(k, ProviderMetricsSnapshot{Requests: 10})
	if got := s.Snapshot()[k].Requests; got != 10 {
		t.Errorf("after Seed Requests = %d, want 10", got)
	}

	s.Reset()
	if len(s.Snapshot()) != 0 {
		t.Error("Reset must clear all entries")
	}
}

func TestMetricsStoreConcurrent(t *testing.T) {
	s := NewMetricsStore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Inc("p", "m", EvRequests)
			}
		}()
	}
	wg.Wait()
	if got := s.Snapshot()[PMKey{Provider: "p", Model: "m"}].Requests; got != 800 {
		t.Errorf("concurrent Requests = %d, want 800", got)
	}
}

func TestTokenCounter(t *testing.T) {
	tc := NewTokenCounter()
	k := TokenKey{Provider: "p", Model: "m"}
	tc.Commit(k, TokenUsage{Input: 5, Output: 7, Requests: 1})
	tc.Commit(k, TokenUsage{Input: 5, Output: 3, Requests: 1})
	snap := tc.Snapshot()[k]
	if snap.Input != 10 || snap.Output != 10 || snap.Requests != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	tc.Seed(k, TokenUsage{Input: 100})
	if got := tc.Snapshot()[k].Input; got != 100 {
		t.Errorf("after Seed Input = %d, want 100", got)
	}
	tc.Reset()
	if len(tc.Snapshot()) != 0 {
		t.Error("Reset must clear")
	}
}

func TestAgentCounter(t *testing.T) {
	a := NewAgentCounter()
	a.IncRequests("codex", "p", "m")
	a.AddTokens("codex", "p", "m", TokenUsage{Input: 3, Output: 4, CacheCreation: 1, CacheRead: 6})
	a.AddLatency("codex", "p", "m", 25)
	a.IncFailure("codex", "p", "m")

	got := a.Snapshot()[AgentKey{Agent: "codex", Provider: "p", Model: "m"}]
	if got.Requests != 1 || got.Input != 3 || got.Output != 4 ||
		got.CacheCreation != 1 || got.CacheRead != 6 {
		t.Fatalf("snapshot = %+v", got)
	}
	a.Reset()
	if len(a.Snapshot()) != 0 {
		t.Error("Reset must clear")
	}
}

func TestUsageScannerCommit(t *testing.T) {
	tc := NewTokenCounter()
	k := TokenKey{Provider: "p", Model: "m"}
	var agentSeen TokenUsage
	body := io.NopCloser(strings.NewReader(
		"data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5}}\n\n"))
	s := NewUsageScanner(body, k, tc, func(u TokenUsage) { agentSeen = u })
	if _, err := io.Copy(io.Discard, s); err != nil {
		t.Fatal(err)
	}
	snap := tc.Snapshot()[k]
	if snap.Input != 12 || snap.Output != 5 {
		t.Fatalf("committed = %+v", snap)
	}
	if agentSeen.Input != 12 {
		t.Errorf("agent callback = %+v", agentSeen)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// sevenByteReader forces every Read to return at most 7 bytes so SSE lines
// split across chunks — exercising the scanner's partial-line reassembly.
type sevenByteReader struct{ r *strings.Reader }

func (c *sevenByteReader) Read(p []byte) (int, error) {
	if len(p) > 7 {
		p = p[:7]
	}
	return c.r.Read(p)
}

// TestUsageScannerShapesAndChunking: anthropic message_start/message_delta and
// openai usage lines all count (input sums both shapes' contributions); plain
// delta lines without a "usage" key count nothing; lines split across Read
// calls reassemble; an over-cap line is skipped without poisoning the next one.
func TestUsageScannerShapesAndChunking(t *testing.T) {
	tc := NewTokenCounter()
	k := TokenKey{Provider: "p", Model: "m"}
	stream := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":3}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"plain delta\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" +
		"data: {\"pad\":\"" + strings.Repeat("x", 70*1024) + "\"}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"
	s := NewUsageScanner(io.NopCloser(&sevenByteReader{r: strings.NewReader(stream)}), k, tc, nil)
	if _, err := io.Copy(io.Discard, s); err != nil {
		t.Fatal(err)
	}
	snap := tc.Snapshot()[k]
	want := TokenUsage{Input: 17, Output: 7, CacheRead: 3, Requests: 1}
	if snap != want {
		t.Fatalf("committed = %+v, want %+v", snap, want)
	}
}

// scanWireFixture replays one recorded wire stream through a UsageScanner and
// returns the committed usage.
func scanWireFixture(t *testing.T, name string) TokenUsage {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "wire", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := NewTokenCounter()
	k := TokenKey{Provider: "p", Model: "m"}
	s := NewUsageScanner(f, k, tc, nil)
	if _, err := io.Copy(io.Discard, s); err != nil {
		t.Fatal(err)
	}
	return tc.Snapshot()[k]
}

// TestUsageScannerAnthropicCacheWireShapes pins cache accounting on real
// recorded anthropic-protocol streams: dialects that send zero/null cache
// usage in message_start and the real values only in message_delta must still
// count (zhipu, aqp, kimi-code), and a dialect repeating the same cumulative
// cache_read in BOTH frames must count it once, not twice (deepseek).
func TestUsageScannerAnthropicCacheWireShapes(t *testing.T) {
	cases := []struct {
		fixture string
		want    TokenUsage
	}{
		// message_start all-zero usage; message_delta carries input 12, output 3.
		{"anthropic_zhipu.sse", TokenUsage{Input: 12, Output: 3, Requests: 1}},
		// message_start cache fields null; message_delta cache_read 64.
		{"anthropic_aqp_tool.sse", TokenUsage{Input: 112, Output: 11, CacheRead: 64, Requests: 1}},
		// message_start input 222 / cache_read 0; message_delta cache_read 222.
		{"anthropic_kimi-code_tool.sse", TokenUsage{Input: 222, Output: 82, CacheRead: 222, Requests: 1}},
	}
	for _, c := range cases {
		if got := scanWireFixture(t, c.fixture); got != c.want {
			t.Errorf("%s: committed = %+v, want %+v", c.fixture, got, c.want)
		}
	}

	// deepseek repeats cache_read_input_tokens:256 in message_start AND
	// message_delta — max-merge must count it once (naive += would give 512).
	// (Input is 39+39=78 under the scanner's long-standing += semantics for
	// converted-route input; only the cache merge rule is pinned here.)
	got := scanWireFixture(t, "anthropic_deepseek_tool.sse")
	if got.CacheRead != 256 || got.CacheCreation != 0 || got.Output != 72 {
		t.Errorf("deepseek double-frame cache: committed = %+v, want cache_read=256 cache_creation=0 output=72", got)
	}
}

// TestUsageScannerChatCachedTokensWireShapes pins cache accounting on real
// recorded chat-protocol streams: cached prompt tokens ride in
// usage.prompt_tokens_details.cached_tokens (deepseek also spells them
// prompt_cache_hit_tokens).
func TestUsageScannerChatCachedTokensWireShapes(t *testing.T) {
	cases := []struct {
		fixture string
		want    TokenUsage
	}{
		{"chat_deepseek_tool.sse", TokenUsage{Input: 295, Output: 70, CacheRead: 256, Requests: 1}},
		{"chat_shopee.sse", TokenUsage{Input: 11, Output: 3, CacheRead: 4, Requests: 1}},
		// prompt_tokens_details present with cached_tokens 0 → no cache credit.
		{"chat_zhipu.sse", TokenUsage{Input: 12, Output: 389, Requests: 1}},
	}
	for _, c := range cases {
		if got := scanWireFixture(t, c.fixture); got != c.want {
			t.Errorf("%s: committed = %+v, want %+v", c.fixture, got, c.want)
		}
	}
}

// prompt_cache_hit_tokens without prompt_tokens_details still counts (the
// deepseek spelling, details preferred when both are present).
func TestUsageScannerChatPromptCacheHitFallback(t *testing.T) {
	stream := "data: {\"choices\":[]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":10,\"prompt_cache_hit_tokens\":128}}\n\n"
	tc := NewTokenCounter()
	k := TokenKey{Provider: "deepseek", Model: "d"}
	s := NewUsageScanner(io.NopCloser(strings.NewReader(stream)), k, tc, nil)
	if _, err := io.Copy(io.Discard, s); err != nil {
		t.Fatal(err)
	}
	if got := tc.Snapshot()[k]; got.CacheRead != 128 || got.Input != 300 {
		t.Errorf("committed = %+v, want cache_read=128 input=300", got)
	}
}

// TestUsageScannerResponsesWireShapes pins usage accounting for /v1/responses
// client streams: one cumulative snapshot in response.completed (usage null on
// in_progress frames), cached input in input_tokens_details.cached_tokens.
func TestUsageScannerResponsesWireShapes(t *testing.T) {
	cases := []struct {
		fixture string
		want    TokenUsage
	}{
		{"responses_shopee.sse", TokenUsage{Input: 11, Output: 3, CacheRead: 4, Requests: 1}},
		{"responses_aqp.sse", TokenUsage{Input: 19, Output: 144, Requests: 1}},
	}
	for _, c := range cases {
		if got := scanWireFixture(t, c.fixture); got != c.want {
			t.Errorf("%s: committed = %+v, want %+v", c.fixture, got, c.want)
		}
	}
}
