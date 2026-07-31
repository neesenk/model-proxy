package main

import (
	"context"
	"io"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDetectAgent: each known client UA / header maps to a stable lowercase
// label; order and specificity are exact (a bare UA is "other", no UA is
// "unknown").
func TestDetectAgent(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		hdr  string // x-claude-code-session-id
		want string
	}{
		{"claude-cli ua", "claude-cli/1.2.3", "", "claude-code"},
		{"claude-code session header", "anything", "sess-123", "claude-code"},
		{"codex ua", "codex_cli_rs/0.144.1", "", "codex"},
		{"opencode ua", "opencode/0.5", "", "opencode"},
		{"pi ua", "pi/1.0", "", "pi"},
		{"unknown other ua", "curl/8.0", "", "other"},
		{"no ua", "", "", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
			req.Header.Set("user-agent", c.ua)
			if c.hdr != "" {
				req.Header.Set("x-claude-code-session-id", c.hdr)
			}
			if got := counters.DetectAgent(req); got != c.want {
				t.Errorf("counters.DetectAgent(ua=%q, hdr=%q) = %q want %q", c.ua, c.hdr, got, c.want)
			}
		})
	}
}

// TestAgentCounter: incRequests + addTokens accumulate under the (agent,
// provider, model) key; snapshot returns a detached copy.
func TestAgentCounter(t *testing.T) {
	a := counters.NewAgentCounter()
	a.IncRequests("claude-code", "z", "glm")
	a.IncRequests("claude-code", "z", "glm")
	a.AddTokens("claude-code", "z", "glm", counters.TokenUsage{Input: 100, Output: 20})
	a.IncRequests("codex", "z", "glm")
	snap := a.Snapshot()
	cc := snap[counters.AgentKey{Agent: "claude-code", Provider: "z", Model: "glm"}]
	if cc.Requests != 2 || cc.Input != 100 || cc.Output != 20 {
		t.Errorf("claude-code cell = %+v want reqs=2 in=100 out=20", cc)
	}
	cx := snap[counters.AgentKey{Agent: "codex", Provider: "z", Model: "glm"}]
	if cx.Requests != 1 || cx.Input != 0 {
		t.Errorf("codex cell = %+v want reqs=1 in=0", cx)
	}
	// reset clears all cells.
	a.Reset()
	if len(a.Snapshot()) != 0 {
		t.Errorf("after reset, cells=%v want empty", a.Snapshot())
	}
}

// TestForward_RecordsAgent: a request carrying a Claude Code UA is attributed to
// the "claude-code" agent in the agent counter — request count on commit, and
// input/output tokens observed from the SSE usage stream. A second request with
// a codex UA lands under "codex", proving the dimension splits by client.
func TestForward_RecordsAgent(t *testing.T) {
	// Anthropic-shaped SSE stream carrying input (message_start) + output
	// (message_delta) usage, so the scanner attributes tokens to the agent.
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":150,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {AnthropicBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"claude-sonnet-4": {{Provider: "aqp", Model: "claude-sonnet-4"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["aqp"] = &testProv{key: "tok"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	do := func(ua string) {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","messages":[]}`))
		req.Header.Set("user-agent", ua)
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	do("claude-cli/1.0.0")
	do("codex_cli_rs/0.144.1")

	// Give the scanner's async-ish commit a beat (it commits on Close, which is
	// synchronous in forward, so the snapshot is already final here).
	snap := p.agents.Snapshot()
	cc := snap[counters.AgentKey{Agent: "claude-code", Provider: "aqp", Model: "claude-sonnet-4"}]
	if cc.Requests != 1 {
		t.Errorf("claude-code requests=%d want 1", cc.Requests)
	}
	if cc.Input != 150 || cc.Output != 42 {
		t.Errorf("claude-code tokens in=%d out=%d want 150/42 (SSE usage must attribute to agent)", cc.Input, cc.Output)
	}
	cx := snap[counters.AgentKey{Agent: "codex", Provider: "aqp", Model: "claude-sonnet-4"}]
	if cx.Requests != 1 || cx.Input != 150 {
		t.Errorf("codex cell = %+v want reqs=1 in=150 (split by UA)", cx)
	}
}
