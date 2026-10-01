package app

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	obscounters "model-proxy/internal/observe/counters"
	"model-proxy/internal/pricing"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- metrics_test.go ----
// (MetricsStore leaf behavior — counters, StartedAt, AddLatency sums — is
// owned by internal/observe/counters; this file keeps only the forward-path
// composition wiring.)

// TestMetricsForwardWiring verifies the forward hot path bumps the right
// counters, attributed to (provider, model). Each scenario uses a fresh Proxy so
// prior cases don't leave scheduling state (a 429 marks the provider
// rate-limited for ~60s, which would make a later 500 case skip the provider and
// never bump Failures).
func TestMetricsForwardWiring(t *testing.T) {
	// Isolate HOME and provision a zhipu pool account so buildProviders binds a
	// real credential — without it the proxy can't build auth headers and the
	// forward never reaches the metrics-recording commit path. (The fake upstream
	// ignores auth, but the proxy must still be able to CONSTRUCT it.) This used
	// to pass only because the real ~/.model-proxy had zhipu creds on the dev box.
	setPoolHome(t, t.TempDir())
	writePoolFile(t, "zhipu", "zhipu", "KEY-A")
	const cfgYAML = `
listen: 127.0.0.1:0
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: %s
routes:
  m: [{provider: zhipu, model: glm-5}]
`
	makeProxy := func(upURL string) *Proxy {
		cfg, err := configdomain.LoadConfigFromBytes("test", []byte(strings.Replace(cfgYAML, "%s", upURL, 1)))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes: %v", err)
		}
		return newTestProxy(t, cfg)
	}
	doRequest := func(p *Proxy) {
		body := []byte(`{"model":"m","stream":false}`)
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		p.Handler(rec, r)
		io.Copy(io.Discard, rec.Result().Body)
		rec.Result().Body.Close()
	}
	zk := counters.PMKey{Provider: "zhipu", Model: "glm-5"}

	t.Run("2xx bumps Requests", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		got := p.metrics.Snapshot()[zk].Requests
		if got != 1 {
			t.Fatalf("after 2xx, requests=%d want 1", got)
		}
	})

	t.Run("429 bumps RateLimited429", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"rate"}`))
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		snap := p.metrics.Snapshot()[zk]
		if snap.RateLimited429 != 1 {
			t.Fatalf("after 429, rate_limited_429=%d want 1", snap.RateLimited429)
		}
		if snap.Requests != 0 {
			t.Errorf("after 429, requests=%d want 0 (commit-only: failed target never served)", snap.Requests)
		}
		if snap.Failovers != 1 {
			t.Errorf("after 429, failovers=%d want 1 (429 abandons target)", snap.Failovers)
		}
	})

	t.Run("500 bumps Failures and Failovers", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"boom"}`))
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		snap := p.metrics.Snapshot()[zk]
		if snap.Failures != 1 {
			t.Fatalf("after 500, failures=%d want 1", snap.Failures)
		}
		if snap.Failovers != 1 {
			t.Errorf("after 500, failovers=%d want 1 (5xx abandons target)", snap.Failovers)
		}
		if snap.Requests != 0 {
			t.Errorf("after 500, requests=%d want 0 (commit-only: failed target never served)", snap.Requests)
		}
	})

	t.Run("connection error bumps Failures and Failovers", func(t *testing.T) {
		// A server that immediately closes the connection without responding.
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatalf("server doesn't support hijack")
			}
			c, _, _ := hj.Hijack()
			c.Close()
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		snap := p.metrics.Snapshot()[zk]
		if snap.Failures != 1 {
			t.Fatalf("after conn error, failures=%d want 1", snap.Failures)
		}
		if snap.Failovers != 1 {
			t.Errorf("after conn error, failovers=%d want 1", snap.Failovers)
		}
	})
}

// ---- metrics_latency_test.go ----

// TestForward_RecordsLatency: a served request records a non-zero total latency
// and TTFT against its (provider, model) in the metrics store on the hot path.
// The upstream deliberately delays before responding so latency is measurable.
func TestForward_RecordsLatency(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(8 * time.Millisecond) // ensure latency >= a few ms
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"z": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {{Provider: "z", Model: "glm-rt"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "z-key"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":[]}`))
	req = req.WithContext(context.Background())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Commit metrics (Requests/LatencySum/TTFTSum) land in the post-copy
	// Committed effect — wait for it before asserting the snapshot.
	snap := awaitCommitMetrics(t, p, counters.PMKey{Provider: "z", Model: "glm-rt"})[counters.PMKey{Provider: "z", Model: "glm-rt"}]
	if snap.Requests != 1 {
		t.Errorf("requests=%d want 1", snap.Requests)
	}
	if snap.LatencySum == 0 {
		t.Error("latency not recorded (LatencySum=0) for served request")
	}
	if snap.TTFTSum == 0 {
		t.Error("TTFT not recorded (TTFTSum=0); first-byte stamp should fire on body write")
	}
	// Ordering: the recorded latency is the UPSTREAM response time (send →
	// headers received, see proxy.go), while TTFT runs to the first byte
	// written to the CLIENT — necessarily after the headers arrive. Both share
	// the same `start`, so TTFT can never be SMALLER; the reverse (ttft >
	// latency) is normal whenever the body copy lands in a later millisecond
	// (ms truncation under load), not a bug.
	if snap.TTFTSum < snap.LatencySum {
		t.Errorf("ttft=%d < latency=%d (impossible: first client byte precedes upstream headers)", snap.TTFTSum, snap.LatencySum)
	}
}

// ---- tokens_test.go ----

// TestForwardCountsTokens verifies the proxy forward hot path wraps SSE response
// bodies in a usageScanner keyed by the chosen (provider, model), committing
// observed anthropic usage (input_tokens from message_start, output_tokens from
// message_delta) to the shared obscounters.TokenCounter. Non-SSE responses are not scanned.
// HOME is pinned to a temp dir (with a dummy zhipu apikey) so NewProxy's
// baseline load can't pick up the developer's real ~/.model-proxy/token_usage.json
// and the provider can authenticate against the mock upstream.
func TestForwardCountsTokens(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"), []byte(`{"api_key":"sk-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":8}}\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		w.Write(stream)
	}))
	defer up.Close()
	cfg, err := configdomain.LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: "+up.URL+"\nroutes:\n  m: [{provider: zhipu, model: glm-5}]\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	p := newTestProxy(t, cfg)
	rec := httptest.NewRecorder()
	p.Handler(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`)))
	io.Copy(io.Discard, rec.Result().Body)
	rec.Result().Body.Close()
	got := p.tokens.Snapshot()[obscounters.TokenKey{Provider: "zhipu", Model: "glm-5"}]
	if got.Input != 42 || got.Output != 8 {
		t.Errorf("tokens = %+v, want in=42 out=8", got)
	}
	if got.Requests != 1 {
		t.Errorf("requests = %d, want 1", got.Requests)
	}
}

// TestForwardDoesNotScanNonSSE verifies a non-SSE 2xx response is NOT wrapped:
// the counter must stay zero (no scanner overhead, no commit) for plain JSON.
func TestForwardDoesNotScanNonSSE(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"), []byte(`{"api_key":"sk-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: "+up.URL+"\nroutes:\n  m: [{provider: zhipu, model: glm-5}]\n"))
	p := newTestProxy(t, cfg)
	rec := httptest.NewRecorder()
	p.Handler(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	io.Copy(io.Discard, rec.Result().Body)
	rec.Result().Body.Close()
	got := p.tokens.Snapshot()[obscounters.TokenKey{Provider: "zhipu", Model: "glm-5"}]
	if got.Input != 0 || got.Output != 0 || got.Requests != 0 {
		t.Errorf("non-SSE tokens = %+v, want zero (non-SSE must not be scanned)", got)
	}
}

// disconnectWriter wraps an httptest.ResponseRecorder and fails every Write,
// simulating a client that has already gone away (broken pipe). flushCopy must
// stop reading the upstream stream on the first failed write.
type disconnectWriter struct {
	*httptest.ResponseRecorder
}

func (disconnectWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected: broken pipe")
}

// TestForwardCommitsOnDisconnect verifies that when a client disconnects
// mid-stream (flushCopy's w.Write returns an error after the upstream has
// already delivered usage events), usage observed BEFORE the disconnect —
// notably input_tokens from message_start, which arrives at the START of the
// stream before any cancel — is still committed.
//
// Previously the inline usageScanner passed to flushCopy was never assigned,
// so resp.Body.Close() closed the underlying body and bypassed the scanner's
// Close → commit path, silently dropping observed usage. With the fix the
// wrapped body is bound to a variable and closed explicitly, firing the
// commit. On a normal (EOF) stream the scanner's Read already committed, so
// the explicit Close is a harmless no-op (no double-count) — covered by
// TestForwardCountsTokens above.
func TestForwardCommitsOnDisconnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"), []byte(`{"api_key":"sk-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Upstream delivers both usage events, then HOLDS the stream open (err==nil
	// on the proxy's first Read) to simulate ongoing generation the client
	// cancels. This is the critical precondition: the scanner's Read-err commit
	// path must NOT fire (no error yet), so the only commit path is the
	// explicit body.Close() the fix adds.
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":8}}\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		w.Write(stream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold open until the proxy closes the body (disconnect)
	}))
	defer up.Close()
	// The route's target model equals the called model ("m") so the stream
	// stays on the zero-copy passthrough this test was written for: both usage
	// frames coalesce into the proxy's first Read before the disconnect.
	cfg, err := configdomain.LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: "+up.URL+"\nroutes:\n  m: [{provider: zhipu, model: m}]\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	p := newTestProxy(t, cfg)
	rec := &disconnectWriter{ResponseRecorder: httptest.NewRecorder()}
	p.Handler(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`)))
	got := p.tokens.Snapshot()[obscounters.TokenKey{Provider: "zhipu", Model: "m"}]
	if got.Input != 42 {
		t.Errorf("input tokens after disconnect = %d, want 42 (observed usage must commit on client-cancel, not be silently dropped)", got.Input)
	}
	if got.Output != 8 {
		t.Errorf("output tokens after disconnect = %d, want 8", got.Output)
	}
	if got.Requests != 1 {
		t.Errorf("requests after disconnect = %d, want 1", got.Requests)
	}
}

// ---- pricing_test.go ----

const pricingIntegrationFixture = `{
  "data": [
    {"id": "z-ai/glm-4.6", "pricing": {
      "prompt": "0.0000009",
      "completion": "0.0000018",
      "input_cache_read": "0.0000001",
      "input_cache_write": "0.0000002"
    }}
  ]
}`

func TestPricingCachePathUsesApplicationHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := pricing.CachePath(accounts.HomeDir()), filepath.Join(home, ".model-proxy", "pricing_cache.json"); got != want {
		t.Errorf("pricing cache path = %q, want %q", got, want)
	}
}

func TestDetachedPricingDetachesAndConvertsOverrides(t *testing.T) {
	proxy := &Proxy{
		generationState: generationState{cfg: &configdomain.Config{
			Pricing: configdomain.PricingConfig{Enabled: false},
			Prices: map[string]configdomain.PriceConfig{
				"glm-4.6": {Input: 9, Output: 18, CacheRead: 1, CacheWrite: 2},
			},
		}},
		processServices: processServices{evalRand: rand.Float64},
	}

	overrides, catalog, _ := proxy.detachedPricing()
	if catalog != nil {
		t.Fatalf("disabled pricing catalog = %+v, want nil", catalog)
	}
	entry, ok := pricing.Resolve(overrides, nil, "glm-4.6")
	if !ok {
		t.Fatal("converted override was not resolvable")
	}
	if entry.Prompt != 9e-6 || entry.Completion != 18e-6 ||
		entry.CacheRead != 1e-6 || entry.CacheWrite != 2e-6 {
		t.Errorf("override conversion = %+v", entry)
	}

	overrides["glm-4.6"] = pricing.Override{Input: 999}
	if got := proxy.cfg.Prices["glm-4.6"].Input; got != 9 {
		t.Errorf("detached pricing mutated config override: input=%v", got)
	}
}

func TestPricingSnapshotDisabled(t *testing.T) {
	proxy := &Proxy{
		generationState: generationState{cfg: &configdomain.Config{Pricing: configdomain.PricingConfig{Enabled: false}}},
		processServices: processServices{evalRand: rand.Float64},
	}
	if got := proxy.pricingSnapshot(); got != nil {
		t.Errorf("disabled pricing snapshot = %+v, want nil", got)
	}
}

func TestPricingSnapshotInitialFailureReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	proxy := &Proxy{
		generationState: generationState{cfg: &configdomain.Config{Pricing: configdomain.PricingConfig{
			Enabled:   true,
			TTL:       "24h",
			SourceURL: "://invalid-pricing-url",
		}}},
		processServices: processServices{evalRand: rand.Float64},
	}
	got := proxy.pricingSnapshot()
	if got == nil || len(got.ByModel) != 0 {
		t.Errorf("failed initial refresh = %+v, want non-nil empty catalog", got)
	}
}

func TestPricingSnapshotSerializesConcurrentRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requests atomic.Int32
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			close(firstRequest)
			<-releaseFirst
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("ETag", `"pricing-v1"`)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(pricingIntegrationFixture))
	}))
	defer server.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFirst) })

	proxy := &Proxy{
		generationState: generationState{cfg: &configdomain.Config{Pricing: configdomain.PricingConfig{
			Enabled:   true,
			TTL:       "24h",
			SourceURL: server.URL,
		}}},
		processServices: processServices{evalRand: rand.Float64},
	}

	firstResult := make(chan *pricing.Catalog, 1)
	go func() {
		firstResult <- proxy.pricingSnapshot()
	}()
	<-firstRequest
	if proxy.pricingMu.TryLock() {
		proxy.pricingMu.Unlock()
		t.Fatal("pricingMu was not held while the first catalog refresh was in flight")
	}

	const callers = 11
	start := make(chan struct{})
	results := make(chan *pricing.Catalog, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			<-start
			results <- proxy.pricingSnapshot()
		}()
	}
	close(start)
	releaseOnce.Do(func() { close(releaseFirst) })
	wait.Wait()
	close(results)

	first := <-firstResult
	entry, ok := first.Lookup("glm-4.6")
	if !ok || entry.Prompt != 0.9e-6 || first.Etag != `"pricing-v1"` {
		t.Errorf("first snapshot = %+v, entry=%+v, ok=%v", first, entry, ok)
	}
	for catalog := range results {
		entry, ok = catalog.Lookup("glm-4.6")
		if !ok || entry.Prompt != 0.9e-6 || catalog.Etag != `"pricing-v1"` {
			t.Errorf("concurrent snapshot = %+v, entry=%+v, ok=%v", catalog, entry, ok)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("upstream refresh requests = %d, want 1", got)
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp": {AnthropicBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"claude-sonnet-4": {{Provider: "aqp", Model: "claude-sonnet-4"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["aqp"] = &testProv{key: "tok"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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

	// The client can finish reading a Content-Length body before the server
	// handler goroutine runs the post-copy Committed effect that increments
	// the agent Requests/latency cells (same race as awaitCommitMetrics in
	// 811d074; token cells are filled earlier, during the body scan) — poll
	// for both agents' requests instead of asserting a racy snapshot.
	waitUntil(t, "agent commit requests", func() bool {
		snap := p.agents.Snapshot()
		return snap[counters.AgentKey{Agent: "claude-code", Provider: "aqp", Model: "claude-sonnet-4"}].Requests >= 1 &&
			snap[counters.AgentKey{Agent: "codex", Provider: "aqp", Model: "claude-sonnet-4"}].Requests >= 1
	})
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
