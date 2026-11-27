package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/pricing"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/testsyn"
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
	// The pool's single account unrolls to a virtual runtime key; metrics are
	// committed under it.
	zk := counters.PMKey{Provider: "zhipu#" + accounts.AccountID("zhipu", accounts.Credentials{APIKey: "KEY-A"}), Model: "glm-5"}

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
	// Buffered JSON body (one write, arrives in ≤3 reads): the generation
	// window is unobservable, so it falls back to the FULL call duration.
	if snap.GenerationSum != snap.DurationSum {
		t.Errorf("buffered gen=%d want duration=%d (bulk arrival falls back to the full call)", snap.GenerationSum, snap.DurationSum)
	}
}

// TestForward_RecordsGenerationWindow pins the commit-time window
// classification (stats.GenerationWindowMs) through the forward path: a
// response body delivered in spread-out SSE chunks counts its decode window
// (duration − TTFT), a body that lands in bulk keeps the full call as the
// tok/s denominator. The bulk leg is the regression this pins — a near-miss
// arrival (first byte a few ms before the last) used to leave a millisecond
// "window" behind and produce six-figure tok/s readings.
func TestForward_RecordsGenerationWindow(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		flusher := w.(http.Flusher)
		// First chunk after a wait (the TTFT), then the body spread across
		// five flushed reads — a genuinely streamed arrival shape (a body
		// landing in fewer reads is, by definition, a bulk arrival).
		time.Sleep(40 * time.Millisecond)
		for _, chunk := range []string{
			"data: {\"delta\":\"a\"}\n\n",
			"data: {\"delta\":\"b\"}\n\n",
			"data: {\"delta\":\"c\"}\n\n",
			"data: {\"delta\":\"d\"}\n\n",
			"data: [DONE]\n\n",
		} {
			io.WriteString(w, chunk)
			flusher.Flush()
			time.Sleep(40 * time.Millisecond)
		}
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

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"glm","stream":true,"messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	key := counters.PMKey{Provider: "z", Model: "glm-rt"}
	snap := awaitCommitMetrics(t, p, key)[key]
	if snap.DurationSum == 0 || snap.TTFTSum == 0 {
		t.Fatalf("timings missing: %+v", snap)
	}
	if snap.GenerationSum >= snap.DurationSum {
		t.Errorf("streamed gen=%d must be < duration=%d (the TTFT wait is excluded)", snap.GenerationSum, snap.DurationSum)
	}
	if want := snap.DurationSum - snap.TTFTSum; snap.GenerationSum != want {
		t.Errorf("streamed gen=%d want duration−ttft=%d", snap.GenerationSum, want)
	}
}

// ---- tokens_test.go ----

// TestForwardCountsTokens verifies the proxy forward hot path wraps SSE response
// bodies in a usageScanner keyed by the chosen (provider, model), committing
// observed anthropic usage (input_tokens from message_start, output_tokens from
// message_delta) to the shared counters.TokenCounter. Non-SSE responses are not scanned.
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
	got := p.tokens.Snapshot()[counters.TokenKey{Provider: "zhipu", Model: "glm-5"}]
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
	got := p.tokens.Snapshot()[counters.TokenKey{Provider: "zhipu", Model: "glm-5"}]
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
	got := p.tokens.Snapshot()[counters.TokenKey{Provider: "zhipu", Model: "m"}]
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
		processServices: processServices{},
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
		processServices: processServices{},
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
		processServices: processServices{},
	}
	got := proxy.pricingSnapshot()
	if got == nil || len(got.ByModel) != 0 {
		t.Errorf("failed initial refresh = %+v, want non-nil empty catalog", got)
	}
}

// pricingStub serves the integration fixture; its first response waits for
// release so a test can hold a refresh in flight.
func pricingStub(t *testing.T, requests *atomic.Int32, releaseFirst <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 && releaseFirst != nil {
			<-releaseFirst
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("ETag", `"pricing-v1"`)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(pricingIntegrationFixture))
	}))
}

// pricingProxy is a proxy wired like the daemon for pricing: a lifecycle that
// owns the background refresh and the production loader, pointed at stub.
func pricingProxy(t *testing.T, stub string) *Proxy {
	t.Helper()
	return &Proxy{
		generationState: generationState{cfg: &configdomain.Config{Pricing: configdomain.PricingConfig{
			Enabled:   true,
			TTL:       "24h",
			SourceURL: stub,
		}}},
		processServices: processServices{lifecycle: runtimestate.NewLifecycle()},
		pricingLoader:   pricing.EnsureFresh,
	}
}

// pricingRefreshInFlight reads the single-flight flag: a refresh is STARTED
// synchronously inside pricingSnapshot, so the flag is the deterministic
// observation of "this call asked the source" — the stub's request counter
// only moves once the background fetch actually arrives there.
func pricingRefreshInFlight(proxy *Proxy) bool {
	proxy.pricingMu.Lock()
	defer proxy.pricingMu.Unlock()
	return proxy.pricingRefreshing
}

// TestPricingSnapshotNeverWaitsOnTheRefresh pins the request path's contract:
// /api/sessions and /api/analytics read the snapshot, they never fetch. While
// the source has not answered, every caller gets the previous catalog (empty
// when there is none) without waiting, exactly one refresh is in flight, and
// once it lands the next caller is served the fresh catalog. Before this the
// snapshot fetched synchronously under pricingMu — with the source
// unreachable every request waited out the fetch timeout (5s measured).
func TestPricingSnapshotNeverWaitsOnTheRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requests atomic.Int32
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFirst) })
	server := pricingStub(t, &requests, releaseFirst)
	defer server.Close()
	proxy := pricingProxy(t, server.URL)
	t.Cleanup(proxy.Close)

	first := make(chan *pricing.Catalog, 1)
	go func() { first <- proxy.pricingSnapshot() }()
	// The refresh is in flight (the stub has the request and holds it).
	testsyn.WaitUntil(t, "the refresh to reach the pricing source", func() bool { return requests.Load() == 1 })
	select {
	case catalog := <-first:
		if catalog == nil || len(catalog.ByModel) != 0 {
			t.Fatalf("snapshot while the refresh is in flight = %+v, want the empty catalog served without waiting", catalog)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pricingSnapshot blocked behind the refresh: the request path waited on the pricing source")
	}

	// More callers while it is still in flight: served at once, and still one
	// request to the source (single flight).
	const callers = 11
	results := make(chan *pricing.Catalog, callers)
	start := make(chan struct{})
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
	wait.Wait()
	close(results)
	for catalog := range results {
		if catalog == nil {
			t.Fatal("concurrent snapshot = nil, want a non-nil catalog")
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("pricing source requests during the refresh = %d, want 1 (single flight)", got)
	}

	// The source answers: the refresh lands and the next caller sees it.
	releaseOnce.Do(func() { close(releaseFirst) })
	testsyn.WaitUntil(t, "the refreshed catalog on the snapshot", func() bool {
		_, ok := proxy.pricingSnapshot().Lookup("glm-4.6")
		return ok
	})
	if got := proxy.pricingSnapshot().Etag; got != `"pricing-v1"` {
		t.Errorf("snapshot etag = %q, want the source's", got)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("pricing source requests after the refresh landed = %d, want 1", got)
	}
}

// TestPricingSnapshotBacksOffAfterAFailedRefresh: a source that cannot be
// reached costs one background attempt per backoff interval, not one per
// request — the next attempt waits for the interval to pass.
func TestPricingSnapshotBacksOffAfterAFailedRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requests atomic.Int32
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer unreachable.Close()
	proxy := pricingProxy(t, unreachable.URL)
	t.Cleanup(proxy.Close)

	// First caller: served at once; the one refresh starts and fails.
	if catalog := proxy.pricingSnapshot(); catalog == nil || len(catalog.ByModel) != 0 {
		t.Fatalf("snapshot before any refresh = %+v, want the empty catalog", catalog)
	}
	testsyn.WaitUntil(t, "the failed refresh to settle", func() bool {
		proxy.pricingMu.Lock()
		defer proxy.pricingMu.Unlock()
		return requests.Load() == 1 && !proxy.pricingRefreshing && time.Now().Before(proxy.pricingRetryAt) // the backoff is armed
	})
	for range 5 {
		if catalog := proxy.pricingSnapshot(); catalog == nil {
			t.Fatal("snapshot during the backoff = nil")
		}
		if pricingRefreshInFlight(proxy) {
			t.Fatal("a call inside the backoff started another refresh: the dead source is hammered per request")
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("pricing source requests during the backoff = %d, want 1", got)
	}

	// The interval passes: the next caller starts the next attempt.
	proxy.pricingMu.Lock()
	proxy.pricingRetryAt = time.Now().Add(-time.Second)
	proxy.pricingMu.Unlock()
	proxy.pricingSnapshot()
	if !pricingRefreshInFlight(proxy) {
		t.Fatal("a call after the backoff elapsed started no refresh")
	}
	testsyn.WaitUntil(t, "the retried refresh to reach the source", func() bool { return requests.Load() == 2 })
}

// The same backoff when the source cannot even be reached (a refused
// connection is an error from the loader, not a status): one attempt, then
// quiet for the interval.
func TestPricingSnapshotBacksOffAfterUnreachableSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close() // nothing listens: the fetch fails at the transport
	proxy := pricingProxy(t, url)
	t.Cleanup(proxy.Close)

	proxy.pricingSnapshot()
	if !pricingRefreshInFlight(proxy) {
		t.Fatal("the first call started no refresh")
	}
	testsyn.WaitUntil(t, "the failed refresh to settle", func() bool {
		proxy.pricingMu.Lock()
		defer proxy.pricingMu.Unlock()
		return !proxy.pricingRefreshing && time.Now().Before(proxy.pricingRetryAt)
	})
	for range 5 {
		if catalog := proxy.pricingSnapshot(); catalog == nil {
			t.Fatal("snapshot during the backoff = nil")
		}
		if pricingRefreshInFlight(proxy) {
			t.Fatal("a call inside the backoff started another refresh at an unreachable source")
		}
	}
}

// TestPricingSnapshotServesStaleCacheWhileSourceFails: with a cache on disk
// and a dead source, callers get the cached prices at once (never a fetch
// wait) and the fruitless refresh backs off like a failed one.
func TestPricingSnapshotServesStaleCacheWhileSourceFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stale := &pricing.Catalog{FetchedAt: time.Now().Add(-48 * time.Hour), Etag: `"old"`, ByModel: map[string]pricing.Entry{
		"glm-4.6": {Prompt: 1, Completion: 2},
	}}
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	cached, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pricing.CachePath(home), cached, 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer unreachable.Close()
	proxy := pricingProxy(t, unreachable.URL)
	t.Cleanup(proxy.Close)
	proxy.initPricing()

	started := time.Now()
	entry, ok := proxy.pricingSnapshot().Lookup("glm-4.6")
	if !ok || entry.Prompt != 1 {
		t.Fatalf("snapshot = %+v ok=%v, want the stale cache's prices", entry, ok)
	}
	// No wait worth measuring on a request path that never fetches; the
	// bound only catches a synchronous fetch creeping back (≥ the stub's
	// round trip is fine — it is the timeout that must not return).
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("pricingSnapshot took %v with a dead source; it must not wait on the fetch", elapsed)
	}
	testsyn.WaitUntil(t, "the fruitless refresh to settle", func() bool {
		proxy.pricingMu.Lock()
		defer proxy.pricingMu.Unlock()
		return requests.Load() == 1 && !proxy.pricingRefreshing && time.Now().Before(proxy.pricingRetryAt) // the backoff is armed
	})
	for range 3 {
		proxy.pricingSnapshot()
		if pricingRefreshInFlight(proxy) {
			t.Fatal("a call inside the backoff started another refresh")
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("pricing source requests during the backoff = %d, want 1 (a stale cache served after a failed fetch still backs off)", got)
	}
	if entry, ok := proxy.pricingSnapshot().Lookup("glm-4.6"); !ok || entry.Prompt != 1 {
		t.Fatalf("snapshot after the failed refresh = %+v ok=%v, want the stale cache kept", entry, ok)
	}
}

// TestInitPricingSeedsFromDiskAndRefreshesWhenStale: boot reads the disk
// cache without the network — a fresh cache is served as is; a stale one is
// served AND refreshed in the background before any request asks.
func TestInitPricingSeedsFromDiskAndRefreshesWhenStale(t *testing.T) {
	writeCache := func(home string, fetchedAt time.Time) {
		t.Helper()
		catalog := &pricing.Catalog{FetchedAt: fetchedAt, Etag: `"disk"`, ByModel: map[string]pricing.Entry{
			"glm-4.6": {Prompt: 3, Completion: 4},
		}}
		cached, err := json.Marshal(catalog)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pricing.CachePath(home), cached, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var requests atomic.Int32
	server := pricingStub(t, &requests, nil)
	defer server.Close()

	t.Run("fresh cache is served without a refresh", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeCache(home, time.Now())
		proxy := pricingProxy(t, server.URL)
		t.Cleanup(proxy.Close)
		proxy.initPricing()
		entry, ok := proxy.pricingSnapshot().Lookup("glm-4.6")
		if !ok || entry.Prompt != 3 {
			t.Fatalf("snapshot = %+v ok=%v, want the disk cache's prices", entry, ok)
		}
		if got := requests.Load(); got != 0 {
			t.Fatalf("pricing source requests = %d, want 0 for a cache inside its TTL", got)
		}
	})

	t.Run("stale cache is refreshed before any request asks", func(t *testing.T) {
		before := requests.Load()
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeCache(home, time.Now().Add(-48*time.Hour))
		proxy := pricingProxy(t, server.URL)
		t.Cleanup(proxy.Close)
		proxy.initPricing()
		entry, ok := proxy.pricingSnapshot().Lookup("glm-4.6")
		if !ok || entry.Prompt != 3 {
			t.Fatalf("snapshot straight after boot = %+v ok=%v, want the stale disk cache served at once", entry, ok)
		}
		testsyn.WaitUntil(t, "the boot-time refresh to land", func() bool {
			return requests.Load() > before && proxy.pricingSnapshot().Etag == `"pricing-v1"`
		})
	})

	t.Run("no cache is refreshed before any request asks", func(t *testing.T) {
		before := requests.Load()
		t.Setenv("HOME", t.TempDir())
		proxy := pricingProxy(t, server.URL)
		t.Cleanup(proxy.Close)
		proxy.initPricing()
		testsyn.WaitUntil(t, "the boot-time refresh to land", func() bool {
			return requests.Load() > before && proxy.pricingSnapshot().Etag == `"pricing-v1"`
		})
	})
}

// TestPricingRefreshStopsWithTheLifecycle: a draining lifecycle admits no new
// refresh, and one in flight is cut short instead of holding shutdown.
func TestPricingRefreshStopsWithTheLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requests atomic.Int32
	held := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			<-held // until the lifecycle's stop cancels the request
		}
	}))
	defer server.Close()
	defer close(held)
	proxy := pricingProxy(t, server.URL)
	proxy.pricingSnapshot()
	testsyn.WaitUntil(t, "the refresh to reach the pricing source", func() bool { return requests.Load() == 1 })

	stopped := make(chan struct{})
	go func() { proxy.Close(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on the pricing fetch: shutdown must cut an in-flight refresh short")
	}
	// After the stop no refresh is admitted: the call returns the previous
	// catalog and leaves the source alone.
	proxy.pricingSnapshot()
	testsyn.WaitUntilFor(t, "the cut-short refresh to settle", 2*time.Second, func() bool {
		proxy.pricingMu.Lock()
		defer proxy.pricingMu.Unlock()
		return !proxy.pricingRefreshing
	})
	if got := requests.Load(); got != 1 {
		t.Fatalf("pricing source requests after the lifecycle stopped = %d, want 1", got)
	}
}

// A reload that changes the pricing source drops the old source's backoff:
// the new source is tried on the next request, not a minute later.
func TestPricingSnapshotSourceChangeDropsBackoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var dead, alive atomic.Int32
	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dead.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer deadServer.Close()
	aliveServer := pricingStub(t, &alive, nil)
	defer aliveServer.Close()
	proxy := pricingProxy(t, deadServer.URL)
	t.Cleanup(proxy.Close)

	proxy.pricingSnapshot()
	testsyn.WaitUntil(t, "the failed refresh to settle", func() bool {
		proxy.pricingMu.Lock()
		defer proxy.pricingMu.Unlock()
		return dead.Load() == 1 && !proxy.pricingRefreshing
	})
	next := *proxy.cfg
	next.Pricing.SourceURL = aliveServer.URL
	proxy.mu.Lock()
	proxy.cfg = &next
	proxy.mu.Unlock()
	proxy.pricingSnapshot()
	testsyn.WaitUntil(t, "the new source to be asked", func() bool { return alive.Load() == 1 })
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
