package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
			if got := detectAgent(req); got != c.want {
				t.Errorf("detectAgent(ua=%q, hdr=%q) = %q want %q", c.ua, c.hdr, got, c.want)
			}
		})
	}
}

// TestAgentCounter: incRequests + addTokens accumulate under the (agent,
// provider, model) key; snapshot returns a detached copy.
func TestAgentCounter(t *testing.T) {
	a := newAgentCounter()
	a.incRequests("claude-code", "z", "glm")
	a.incRequests("claude-code", "z", "glm")
	a.addTokens("claude-code", "z", "glm", tokenUsage{Input: 100, Output: 20})
	a.incRequests("codex", "z", "glm")
	snap := a.snapshot()
	cc := snap[agentKey{Agent: "claude-code", Provider: "z", Model: "glm"}]
	if cc.Requests != 2 || cc.Input != 100 || cc.Output != 20 {
		t.Errorf("claude-code cell = %+v want reqs=2 in=100 out=20", cc)
	}
	cx := snap[agentKey{Agent: "codex", Provider: "z", Model: "glm"}]
	if cx.Requests != 1 || cx.Input != 0 {
		t.Errorf("codex cell = %+v want reqs=1 in=0", cx)
	}
	// reset clears all cells.
	a.reset()
	if len(a.snapshot()) != 0 {
		t.Errorf("after reset, cells=%v want empty", a.snapshot())
	}
}

// TestStatsAgentFlushQuery: agent deltas persist additively and round-trip
// through queryAgentRange with exact values; filters narrow correctly.
func TestStatsAgentFlushQuery(t *testing.T) {
	ss := newTestStatsStore(t)
	minute := time.Now().Unix() / 60 * 60
	if err := ss.flushAgentDeltas(minute, map[agentKey]agentCount{
		{Agent: "claude-code", Provider: "z", Model: "glm"}: {Requests: 3, Input: 100, Output: 20},
		{Agent: "codex", Provider: "z", Model: "glm"}:       {Requests: 1, Input: 40, Output: 5},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ss.queryAgentRange(minute, minute, "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rows=%d want 2", len(got))
	}
	// Find the claude-code row and assert exact counters.
	var cc *agentBucket
	for i := range got {
		if got[i].Agent == "claude-code" {
			cc = &got[i]
		}
	}
	if cc == nil || cc.Requests != 3 || cc.Input != 100 || cc.Output != 20 {
		t.Errorf("claude-code row = %+v want reqs=3 in=100 out=20", cc)
	}
	// Agent filter narrows to one row.
	only, _ := ss.queryAgentRange(minute, minute, "codex", "", "", 60)
	if len(only) != 1 || only[0].Agent != "codex" || only[0].Requests != 1 {
		t.Errorf("agent filter = %+v want single codex reqs=1", only)
	}
	// A second flush into the same (agent,provider,model,minute) adds (additive).
	if err := ss.flushAgentDeltas(minute, map[agentKey]agentCount{
		{Agent: "claude-code", Provider: "z", Model: "glm"}: {Requests: 2, Input: 10, Output: 0},
	}); err != nil {
		t.Fatal(err)
	}
	got2, _ := ss.queryAgentRange(minute, minute, "claude-code", "", "", 60)
	if len(got2) != 1 || got2[0].Requests != 5 || got2[0].Input != 110 || got2[0].Output != 20 {
		t.Errorf("additive re-flush = %+v want reqs=5 in=110 out=20", got2[0])
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
	snap := p.agents.snapshot()
	cc := snap[agentKey{Agent: "claude-code", Provider: "aqp", Model: "claude-sonnet-4"}]
	if cc.Requests != 1 {
		t.Errorf("claude-code requests=%d want 1", cc.Requests)
	}
	if cc.Input != 150 || cc.Output != 42 {
		t.Errorf("claude-code tokens in=%d out=%d want 150/42 (SSE usage must attribute to agent)", cc.Input, cc.Output)
	}
	cx := snap[agentKey{Agent: "codex", Provider: "aqp", Model: "claude-sonnet-4"}]
	if cx.Requests != 1 || cx.Input != 150 {
		t.Errorf("codex cell = %+v want reqs=1 in=150 (split by UA)", cx)
	}
}

// TestRenderAgentsCLI: `stats --by-agent` fetches /api/agents and renders a
// per-agent summary sorted by total tokens desc, with the exact header + the
// heaviest agent on top. Guards the CLI display contract for the agent view.
func TestRenderAgentsCLI(t *testing.T) {
	resp := agentResp{From: 1, To: 2, Bucket: 60, Buckets: []agentBucket{
		{Agent: "claude-code", Requests: 10, Input: 5000, Output: 800},
		{Agent: "codex", Requests: 3, Input: 200, Output: 50},
	}}
	raw, _ := json.Marshal(resp)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, string(raw))
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")

	out, err := renderAgents(listen, statsOpts{ByAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "claude-code") || !strings.Contains(out, "codex") {
		t.Errorf("agent table missing agents:\n%s", out)
	}
	// Exact header columns.
	if !strings.Contains(out, "agent") || !strings.Contains(out, "reqs") || !strings.Contains(out, "input") || !strings.Contains(out, "output") {
		t.Errorf("agent table missing a header:\n%s", out)
	}
	// claude-code (5800 tokens) sorts above codex (250 tokens).
	if strings.Index(out, "claude-code") > strings.Index(out, "codex") {
		t.Errorf("heaviest agent not on top:\n%s", out)
	}
}

// TestRenderAgents_ProviderModelFilter: --provider/--model are forwarded to
// /api/agents as query params in --by-agent mode (the server side already
// filters on them); previously the CLI silently dropped them here.
func TestRenderAgents_ProviderModelFilter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("provider"); got != "zhipu" {
			t.Errorf("provider query=%q want zhipu", got)
		}
		if got := r.URL.Query().Get("model"); got != "glm-5.2" {
			t.Errorf("model query=%q want glm-5.2", got)
		}
		io.WriteString(w, `{"from":1,"to":2,"bucket":60,"buckets":[]}`)
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")

	if _, err := renderAgents(listen, statsOpts{ByAgent: true, Provider: "zhipu", Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
}
