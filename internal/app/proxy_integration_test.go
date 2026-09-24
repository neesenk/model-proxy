package app

import (
	"context"
	"io"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/provider"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- proxy_failure_integration_test.go ----

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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: configdomain.Scheduling{CircuitThreshold: 3},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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
	if got := atomic.LoadInt32(&primaryHits); got != 3 {
		t.Errorf("primary hits=%d, want exactly 3 (circuit opens AT the threshold, not before)", got)
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newTestProxy(t, cfg)
	rp := &recordingProv{testProv: testProv{key: "k"}}
	p.providers["codex"] = rp
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	rp := &recordingProv{testProv: testProv{key: "p"}}
	p.providers["primary"] = rp
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (should failover after 401-refresh fails)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&rp.refreshCalls); got != 1 {
		t.Errorf("Refresh calls=%d want 1 (401 → refresh → retry before failover)", got)
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: up.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"slow":     {OpenAIBaseURL: slow.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "slow", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: configdomain.Scheduling{UpstreamTimeout: "200ms"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"slow": "s", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"codex": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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

	// The upstream write error is observable — poll for it instead of
	// sleeping a fixed 300ms (AGENTS.md: no fixed sleeps to prove timing).
	waitUntil(t, "upstream to see the disconnect write error", func() bool {
		return atomic.LoadInt32(&upstreamBroken) == 1
	})
}

// --- UC9: all targets fail → 502 ---

func TestUC_AllTargetsFailReturns502(t *testing.T) {
	a, _ := newCaptureUpstream(500, `{}`)
	defer a.Close()
	b, _ := newCaptureUpstream(500, `{}`)
	defer b.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: a.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: b.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "a", Model: "m1", Priority: 1},
				{Provider: "b", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "a", "b": "b"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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

// ---- proxy_integration_support_test.go ----

// usecase_test.go organizes tests by user-facing use case (end-to-end through
// the proxy), rather than by function. Each test drives a real HTTP request
// through p.Handler against httptest upstreams and asserts the observable
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
func newProxyWithStatic(t testing.TB, cfg *configdomain.Config, keys map[string]string) *Proxy {
	return newProxyWithStaticAt(t, cfg, filepath.Join(t.TempDir(), "quota_state.json"), keys)
}

// newProxyWithStaticAt is newProxyWithStatic with an explicit state path, for
// tests that assert on state persisted next to quota_state.json.
func newProxyWithStaticAt(t testing.TB, cfg *configdomain.Config, qpath string, keys map[string]string) *Proxy {
	p := newTestProxyAt(t, cfg, qpath)
	for name, key := range keys {
		p.providers[name] = &testProv{key: key}
	}
	// The injected impls stand in for credentials (login → reload): rebuild
	// the effective route table so targets dropped at construction for having
	// no runnable provider (e.g. an empty plural pool) come back — expandTarget
	// only lists impl-backed ids.
	p.mu.Lock()
	p.expandedRoutes = p.buildExpandedRoutes()
	p.routeKeys = routeKeySet(p.expandedRoutes)
	p.mu.Unlock()
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

// waitUntil polls a condition until it holds or the deadline lapses. It
// replaces fixed sleeps that "prove" timing (AGENTS.md testing rules): the
// wait ends as soon as the observable state flips (fast machines finish
// early) and never flakes on slow ones, and a genuine failure still fails
// with a named condition instead of a downstream assertion mystery.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// awaitCommitMetrics waits until the server-side Committed effect for key
// (target Requests, attempts/ok, latency/TTFT) has been recorded, then
// returns the metrics snapshot. A client can finish reading a Content-Length
// (non-streaming) body before the handler goroutine runs the post-copy
// Committed effect, so snapshotting right after ReadAll races on loaded
// machines. Streaming responses don't need this: their terminating chunk
// only reaches the client after the handler returns.
func awaitCommitMetrics(t *testing.T, p *Proxy, key counters.PMKey) map[counters.PMKey]counters.ProviderMetricsSnapshot {
	t.Helper()
	waitUntil(t, "commit metrics for "+key.Provider+"/"+key.Model, func() bool {
		return p.metrics.Snapshot()[key].Requests >= 1
	})
	return p.metrics.Snapshot()
}

// ---- fake_provider_test.go ----

// fakeProviderImpl is the shared no-op provider implementation for app tests.
type fakeProviderImpl struct {
	rewritePath string
	mu          sync.Mutex
}

func (f *fakeProviderImpl) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer TEST")
	return nil
}
func (f *fakeProviderImpl) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	f.mu.Lock()
	f.rewritePath = path
	f.mu.Unlock()
	return targetURL, body
}
func (f *fakeProviderImpl) Refresh() error                 { return nil }
func (f *fakeProviderImpl) Logout() error                  { return nil }
func (f *fakeProviderImpl) Usage() error                   { return nil }
func (f *fakeProviderImpl) FetchModels() ([]string, error) { return nil, nil }
func (f *fakeProviderImpl) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingUnknown}, nil
}
func (f *fakeProviderImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{
		Method: http.MethodPost,
		Path:   "/chat/completions",
		Body:   []byte(`{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":false}`),
	}
}
func (f *fakeProviderImpl) ExtraHeaders(req *http.Request, path string)          {}
func (f *fakeProviderImpl) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

// ---- test_provider_test.go ----

// testProviderID gives proxy behavior tests a credential-independent upstream.
// Historically those tests used static, which accidentally coupled routing,
// lifecycle, cache, Fusion, Shadow, and protocol fixtures to static's account
// storage policy. Keep the same provider behavior by delegating to static's
// constructor, but use a distinct registry key so static remains available only
// to tests that deliberately exercise its plural-pool credential contract.
const testProviderID = "test-static"

func init() {
	provider.Register(testProviderID, func(cfg *provider.Config, providerName string) (provider.Provider, error) {
		staticCfg := *cfg
		staticCfg.ProviderID = "static"
		return provider.New(&staticCfg, providerName)
	})
}

// ---- accounts_test_support_test.go ----

// setPoolHome isolates account storage for root-package integration tests.
func setPoolHome(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
}

// legacyPoolPath is test support for constructing compatibility fixtures. The
// production adapter intentionally exposes no legacy-path wrapper; fallback
// ownership remains inside accounts.Store.LoadSnapshot.
func legacyPoolPath(name string) string {
	return accounts.NewStore(accounts.HomeDir()).LegacyPath(name)
}

// useStaticProviderPools prepares real plural credentials for tests that load
// production-style static providers from YAML. Behavior tests that construct
// Config directly should use testProviderID instead.
func useStaticProviderPools(t *testing.T, names ...string) {
	t.Helper()
	setPoolHome(t, t.TempDir())
	for _, name := range names {
		writePoolFile(t, name, "static", "STATIC-TEST-KEY")
	}
}
