package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

// usecase_test.go organizes tests by user-facing use case (end-to-end through
// the proxy), rather than by function. Each test drives a real HTTP request
// through p.handler against httptest upstreams and asserts the observable
// outcome a user cares about. Shared helpers (testProv, post, stringReader,
// newCaptureUpstream) live in proxy_routing_test.go / routes_test.go.

// --- mock helpers specific to use cases ---

// scriptedUpstream responds with a sequence of (status, body) per request,
// letting a single server simulate "first 401 then 200 after refresh".
func scriptedUpstream(scripts ...respScript) *httptest.Server {
	var idx int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&idx, 1)) - 1
		if i >= len(scripts) {
			i = len(scripts) - 1
		}
		s := scripts[i]
		if s.headers != nil {
			for k, v := range s.headers {
				w.Header().Set(k, v)
			}
		}
		if s.status != 200 {
			w.WriteHeader(s.status)
		}
		w.Write([]byte(s.body))
	}))
}

type respScript struct {
	status  int
	body    string
	headers map[string]string
}

// recordingProv is a testProv that records calls to AuthHeaders/Refresh, so a
// test can assert the 401-refresh-retry path actually invoked Refresh.
type recordingProv struct {
	testProv
	authCalls    int32
	refreshCalls int32
}

func (p *recordingProv) AuthHeaders(req *http.Request) error {
	atomic.AddInt32(&p.authCalls, 1)
	return p.testProv.AuthHeaders(req)
}
func (p *recordingProv) Refresh() error {
	atomic.AddInt32(&p.refreshCalls, 1)
	return nil
}

// newProxyWithStatic builds a Proxy whose providers are all testProv with the
// given keys, so tests don't hit real auth files.
func newProxyWithStatic(t testing.TB, cfg *Config, keys map[string]string) *Proxy {
	p := newTestProxy(t, cfg)
	for name, key := range keys {
		p.providers[name] = &testProv{key: key}
	}
	return p
}

// fakeAuth is a provider.Authenticator that injects a Bearer key, for building
// real provider implementations (AqpProvider/CodexProvider) in tests
// without reading auth files.
type fakeAuth struct{ key string }

func (a fakeAuth) Inject(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.key)
	req.Header.Del("x-api-key")
	return nil
}
func (a fakeAuth) Refresh() error { return nil }

// mustRealProvider builds a real provider implementation by provider_id (so its
// RewriteRequest/AuthHeaders logic is exercised), wired with the given cfg.
func mustRealProvider(t *testing.T, providerID string, cfg *provider.Config) provider.Provider {
	t.Helper()
	p, err := provider.New(cfg, providerID+"_test")
	if err != nil {
		t.Fatalf("provider.New(%s): %v", providerID, err)
	}
	return p
}

// --- UC1: anthropic request translated via claude_mapping + forwarded with /v1 kept ---

func TestUC_AnthropicMappingAndPathKept(t *testing.T) {
	var hitPath string
	var hitModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		json.Unmarshal(b, &m)
		hitModel = m.Model
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: "aqp"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
		ClaudeMapping: map[string]string{"claude-opus-4-8": "glm-5.2"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"aqp": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/messages", `{"model":"claude-opus-4-8","messages":[]}`)

	if hitModel != "glm-5.2" {
		t.Errorf("upstream model=%q want glm-5.2 (claude_mapping should translate)", hitModel)
	}
	if hitPath != "/v1/messages" {
		t.Errorf("upstream path=%q want /v1/messages (anthropic keeps /v1)", hitPath)
	}
}

// --- UC2: openai request strips the client /v1 prefix ---

func TestUC_OpenAIStripsV1Prefix(t *testing.T) {
	var hitPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"codex": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/chat/completions", `{"model":"gpt-5.5","messages":[]}`)

	if hitPath != "/chat/completions" {
		t.Errorf("upstream path=%q want /chat/completions (openai strips /v1)", hitPath)
	}
}

// --- UC3: primary 5xx → failover to fallback, and circuit opens so 2nd call skips primary ---

func TestUC_FailoverAndCircuitSkipsOpenProvider(t *testing.T) {
	var primaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(500)
		w.Write([]byte(`{"e":"primary"}`))
	}))
	defer primary.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{CircuitThreshold: 3},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// Call 1-2: primary 500 → failover to fallback. Primary circuit not yet open (threshold 3).
	for i := 0; i < 2; i++ {
		resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("call %d: status=%d want 200 (fallback)", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Call 3-4: after 3 failures primary circuit opens; primary should NOT be hit again.
	*fallbackSeen = nil
	for i := 0; i < 2; i++ {
		resp, _ := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := atomic.LoadInt32(&primaryHits); got > 3 {
		t.Errorf("primary hits=%d, want ≤3 (circuit should open after 3 and skip it)", got)
	}
	if len(*fallbackSeen) != 2 {
		t.Errorf("fallback hits=%d want 2 (open circuit routes straight to fallback)", len(*fallbackSeen))
	}
}

// --- UC4: 401 → refresh → retry same provider → success ---

func TestUC_401RefreshRetrySucceeds(t *testing.T) {
	// First call 401, second call 200 (simulates refresh fixing the token).
	up := scriptedUpstream(
		respScript{status: 401, body: `{"e":"unauthorized"}`},
		respScript{status: 200, body: `{"ok":true}`},
	)
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newTestProxy(t, cfg)
	rp := &recordingProv{testProv: testProv{key: "k"}}
	p.providers["codex"] = rp
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"gpt-5.5","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status=%d body=%s want 200 (401 should trigger refresh+retry)", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&rp.refreshCalls); got != 1 {
		t.Errorf("Refresh calls=%d want 1 (exactly one refresh after 401)", got)
	}
}

// --- UC5: 401 → refresh → still 401 → failover to next provider ---

func TestUC_401RefreshFailsFailover(t *testing.T) {
	primary := scriptedUpstream(
		respScript{status: 401, body: `{"e":"bad"}`},
		respScript{status: 401, body: `{"e":"bad"}`},
	)
	defer primary.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &recordingProv{testProv: testProv{key: "p"}}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (should failover after 401-refresh fails)", resp.StatusCode)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback hits=%d want 1 (failover target after repeated 401)", len(*fallbackSeen))
	}
}

// --- UC6: 429 with Retry-After → provider skipped on next call within the window ---

func TestUC_429RetryAfterSkipsProvider(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		w.Write([]byte(`{"e":"rate"}`))
	}))
	defer up.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: up.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// Call 1: primary 429 → failover to fallback.
	resp, _ := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Call 2: primary still rate-limited (60s) → straight to fallback, no primary hit.
	*fallbackSeen = nil
	hitsBefore := atomic.LoadInt32(&hits)
	resp, _ = http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := atomic.LoadInt32(&hits) - hitsBefore; got != 0 {
		t.Errorf("primary hits on call 2 = %d want 0 (Retry-After should skip it)", got)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback hits on call 2 = %d want 1", len(*fallbackSeen))
	}
}

// --- UC7: upstream timeout → failover ---

func TestUC_UpstreamTimeoutFailover(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{}`))
	}))
	defer slow.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"slow":     {OpenAIBaseURL: slow.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "slow", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{UpstreamTimeout: "200ms"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"slow": "s", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	start := time.Now()
	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (should failover after timeout)", resp.StatusCode)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("elapsed=%v want <1.5s (timeout should fail fast, not wait full 2s)", elapsed)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback hits=%d want 1", len(*fallbackSeen))
	}
}

// --- UC8: client disconnects mid-stream → proxy stops pulling upstream ---

func TestUC_ClientDisconnectStopsUpstream(t *testing.T) {
	var upstreamRead int64
	var upstreamBroken int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stream slowly; detect when the proxy closes the connection.
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			if _, err := w.Write([]byte("data: chunk\n\n")); err != nil {
				atomic.StoreInt32(&upstreamBroken, 1)
				return
			}
			atomic.AddInt64(&upstreamRead, 1)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"codex": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// Client cancels its request almost immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", px.URL+"/v1/responses", stringReader(`{"model":"gpt-5.5","input":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// context deadline exceeded during the dial/read is fine — the point is
		// the proxy should stop pulling upstream shortly after.
		if resp != nil {
			resp.Body.Close()
		}
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Give the proxy a moment to propagate the write error up to the upstream.
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt32(&upstreamBroken) == 0 {
		t.Error("upstream never saw a write error — proxy may be keep pulling after client disconnect")
	}
}

// --- UC9: all targets fail → 502 ---

func TestUC_AllTargetsFailReturns502(t *testing.T) {
	a, _ := newCaptureUpstream(500, `{}`)
	defer a.Close()
	b, _ := newCaptureUpstream(500, `{}`)
	defer b.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: a.URL, Provider: "static"},
			"b": {OpenAIBaseURL: b.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "a", Model: "m1", Priority: 1},
				{Provider: "b", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "a", "b": "b"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status=%d want 502 (all targets failed)", resp.StatusCode)
	}
}

// --- UC10: GET /v1/models returns exposed names ∪ claude_mapping keys ---

func TestUC_ModelsEndpointUnion(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2":         {{Provider: "aqp", Model: "glm-5.2"}},
			"deepseek-v4-pro": {{Provider: "aqp", Model: "deepseek-v4-pro"}},
		},
		ClaudeMapping: map[string]string{
			"claude-opus-4-8":   "glm-5.2",
			"claude-sonnet-4-6": "deepseek-v4-pro",
		},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Get(px.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	got := map[string]bool{}
	for _, m := range out.Data {
		got[m.ID] = true
	}
	for _, want := range []string{"glm-5.2", "deepseek-v4-pro", "claude-opus-4-8", "claude-sonnet-4-6"} {
		if !got[want] {
			t.Errorf("GET /v1/models missing %q (routes ∪ claude_mapping); got %v", want, got)
		}
	}
}

// --- UC11: /debug/schedule reports sticky + dwell remaining ---

func TestUC_DebugScheduleReportsSticky(t *testing.T) {
	up, _ := newCaptureUpstream(200, `{}`)
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: up.URL, Provider: "static"},
			"b": {OpenAIBaseURL: up.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "a", Model: "m1", Priority: 1},
				{Provider: "b", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "a", "b": "b"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// Send a request so the route parks sticky on provider "a".
	post(t, px.URL+"/v1/responses", `{"model":"m1","input":[]}`)

	resp, err := http.Get(px.URL + "/debug/schedule")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models map[string]struct {
			First    string  `json:"first"`
			Sticky   string  `json:"sticky"`
			DwellRem float64 `json:"sticky_dwell_remaining_sec"`
			Ordered  []struct {
				Provider  string  `json:"provider"`
				Tier      string  `json:"tier"`
				Available bool    `json:"available"`
				Peak      bool    `json:"peak"`
				Surplus   float64 `json:"surplus"`
			} `json:"ordered"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	m := out.Models["m1"]
	if m.Sticky != "a" {
		t.Errorf("sticky=%q want a (first request parks sticky)", m.Sticky)
	}
	if m.DwellRem <= 0 {
		t.Errorf("dwell_remaining=%v want >0 (within dwell window)", m.DwellRem)
	}
	if len(m.Ordered) != 2 {
		t.Errorf("ordered len=%d want 2", len(m.Ordered))
	}
	if m.Ordered[0].Provider != "a" {
		t.Errorf("ordered[0]=%q want a (sticky first)", m.Ordered[0].Provider)
	}
}

// --- UC12: sticky — two consecutive requests hit the same provider ---

func TestUC_StickySameProvider(t *testing.T) {
	var mu sync.Mutex
	hits := []string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: up.URL, Provider: "static"},
			"b": {OpenAIBaseURL: up.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "a", Model: "m1", Priority: 1},
				{Provider: "b", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "a-key", "b": "b-key"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	for i := 0; i < 3; i++ {
		post(t, px.URL+"/v1/responses", `{"model":"m1","input":[]}`)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 3 {
		t.Fatalf("hits=%d want 3", len(hits))
	}
	for _, h := range hits {
		if h != "Bearer a-key" {
			t.Errorf("sticky expected all hits on provider a (Bearer a-key), got %q", h)
		}
	}
}

// --- UC13: aqp /messages gets ?beta=true + anthropic-version + x-compass-request-id ---

func TestUC_AqpBetaAndHeaders(t *testing.T) {
	var gotURL, gotAV, gotRID string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAV = r.Header.Get("anthropic-version")
		gotRID = r.Header.Get("x-compass-request-id")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: "aqp"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
	}
	p := newTestProxy(t, cfg)
	// Build a REAL AqpProvider (so RewriteRequest adds ?beta) with a fake
	// Authenticator, so no auth file is read.
	p.providers["aqp"] = mustRealProvider(t, "aqp", &provider.Config{
		ProviderID:    "aqp",
		OpenAIBaseURL: up.URL,
		Auth:          fakeAuth{key: "k"},
	})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/messages", `{"model":"glm-5.2","messages":[]}`)

	if !strings.Contains(gotURL, "beta=true") {
		t.Errorf("upstream URL=%q missing beta=true (aqp /messages needs it)", gotURL)
	}
	if gotAV != "2023-06-01" {
		t.Errorf("anthropic-version=%q want 2023-06-01", gotAV)
	}
	if gotRID == "" {
		t.Error("x-compass-request-id empty (aqp requests must set a UUID)")
	}
}

// --- UC14: codex request gets store:false injected ---

func TestUC_CodexStoreFalseInjected(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: "codex"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newTestProxy(t, cfg)
	// Build a REAL CodexProvider (so RewriteRequest injects store:false) with a
	// fake Authenticator, so no auth file is read.
	p.providers["codex"] = mustRealProvider(t, "codex", &provider.Config{
		ProviderID:    "codex",
		OpenAIBaseURL: up.URL,
		Auth:          fakeAuth{key: "k"},
	})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)

	var m struct {
		Store bool `json:"store"`
	}
	if err := json.Unmarshal([]byte(gotBody), &m); err != nil {
		t.Fatalf("body not JSON: %s", gotBody)
	}
	if m.Store != false {
		t.Errorf("store=%v want false (codex backend requires store:false)", m.Store)
	}
}

// --- UC15: deepseek dual-protocol — anthropic→anthropic_base_url, openai→openai_base_url ---

func TestUC_DeepSeekDualProtocolBaseURL(t *testing.T) {
	var anthropicHit, openaiHit string
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHit = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer anthUp.Close()
	oaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHit = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer oaiUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"deepseek": {
				OpenAIBaseURL:    oaiUp.URL,
				AnthropicBaseURL: anthUp.URL,
				Provider:         "deepseek",
			},
		},
		Routes: map[string][]RouteTarget{
			"deepseek-v4-pro": {{Provider: "deepseek", Model: "deepseek-v4-pro"}},
		},
	}
	p := newTestProxy(t, cfg)
	// deepseek provider sets both Bearer + x-api-key; use a key file via testProv override.
	p.providers["deepseek"] = &testProv{key: "ds-key"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/messages", `{"model":"deepseek-v4-pro","messages":[]}`)
	post(t, px.URL+"/v1/chat/completions", `{"model":"deepseek-v4-pro","messages":[]}`)

	if anthropicHit == "" {
		t.Error("anthropic request did not hit anthropic_base_url upstream")
	}
	if openaiHit == "" {
		t.Error("openai request did not hit openai_base_url upstream")
	}
	if anthropicHit == openaiHit {
		t.Errorf("both protocols hit the same upstream path %q (should use per-protocol base)", anthropicHit)
	}
}

// --- UC16: unknown path → 502; /health → 200 ---

func TestUC_UnknownPathAndHealth(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// Unknown path → 502.
	resp, _ := http.Post(px.URL+"/v1/whatever", "application/json", stringReader(`{"model":"m1"}`))
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("unknown path status=%d want 502", resp.StatusCode)
	}

	// /health → 200.
	resp, _ = http.Get(px.URL + "/health")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/health status=%d want 200", resp.StatusCode)
	}

	// /health/status → 200.
	resp, _ = http.Get(px.URL + "/health/status")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/health/status status=%d want 200", resp.StatusCode)
	}
}

// --- UC17: missing model field → 400 ---

func TestUC_MissingModelField400(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, _ := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"input":[]}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("missing model: status=%d want 400", resp.StatusCode)
	}
}

// --- UC18: unparseable JSON body → 400 ---

func TestUC_UnparseableBody400(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, _ := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`not-json`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("unparseable body: status=%d want 400", resp.StatusCode)
	}
}

// --- UC extra: header whitelist — client Authorization is NOT forwarded ---

func TestUC_ClientAuthNotForwarded(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "proxy-key"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	req, _ := http.NewRequest("POST", px.URL+"/v1/responses", stringReader(`{"model":"m1","input":[]}`))
	req.Header.Set("Authorization", "Bearer CLIENT-SECRET") // must NOT reach upstream
	req.Header.Set("Cookie", "SSO_C=secret")                // must NOT reach upstream
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer proxy-key" {
		t.Errorf("upstream Authorization=%q want Bearer proxy-key (client secret must not leak)", gotAuth)
	}
}

// ensure bytes import is used (some build configs need it)
var _ = bytes.MinRead
