package counters

import (
	"io"
	"net/http"
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
		{"no ua", "", nil, "unknown"},
		{"unrecognized", "curl/8.0", nil, "other"},
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
	a.AddTokens("codex", "p", "m", TokenUsage{Input: 3, Output: 4})
	a.AddLatency("codex", "p", "m", 25)
	a.IncFailure("codex", "p", "m")

	got := a.Snapshot()[AgentKey{Agent: "codex", Provider: "p", Model: "m"}]
	if got.Requests != 1 || got.Input != 3 || got.Output != 4 {
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
