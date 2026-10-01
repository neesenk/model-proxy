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

// (Failover/circuit, 429 Retry-After skip, and upstream-timeout failover used
// to have weaker UC-shaped twins here; the stronger per-request assertions in
// health_test.go are the single authority for those scenarios.)

// --- UC4: 401 → refresh → retry same provider → success ---

func TestUC_401RefreshRetrySucceeds(t *testing.T) {
	// First call 401, second call 200 (simulates refresh fixing the token).
	up := newHitFakeUpstream(t, respondScripted(
		respScript{status: 401, body: `{"e":"unauthorized"}`},
		respScript{status: 200, body: `{"ok":true}`},
	))

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"codex": {OpenAIBaseURL: up.srv.URL, Provider: testProviderID},
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

	code, body := post(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)
	if code != 200 {
		t.Errorf("status=%d body=%s want 200 (401 should trigger refresh+retry)", code, body)
	}
	if got := atomic.LoadInt32(&rp.refreshCalls); got != 1 {
		t.Errorf("Refresh calls=%d want 1 (exactly one refresh after 401)", got)
	}
}

// --- UC5: 401 → refresh → still 401 → failover to next provider ---

func TestUC_401RefreshFailsFailover(t *testing.T) {
	primary := newHitFakeUpstream(t, respondScripted(
		respScript{status: 401, body: `{"e":"bad"}`},
		respScript{status: 401, body: `{"e":"bad"}`},
	))
	fallback := newFakeUpstream(t, staticResponder(200, `{"ok":true}`))

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.srv.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.srv.URL, Provider: testProviderID},
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

	if code, _ := post(t, px.URL+"/v1/responses", `{"model":"m1","input":[]}`); code != 200 {
		t.Errorf("status=%d want 200 (should failover after 401-refresh fails)", code)
	}
	if got := atomic.LoadInt32(&rp.refreshCalls); got != 1 {
		t.Errorf("Refresh calls=%d want 1 (401 → refresh → retry before failover)", got)
	}
	if len(fallback.models()) != 1 {
		t.Errorf("fallback hits=%d want 1 (failover target after repeated 401)", len(fallback.models()))
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
	a := newFakeUpstream(t, staticResponder(500, `{}`))
	b := newFakeUpstream(t, staticResponder(500, `{}`))
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: a.srv.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: b.srv.URL, Provider: testProviderID},
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

	if code, _ := post(t, px.URL+"/v1/responses", `{"model":"m1","input":[]}`); code != 502 {
		t.Errorf("status=%d want 502 (all targets failed)", code)
	}
}

// ---- proxy_integration_support_test.go ----

// usecase_test.go organizes tests by user-facing use case (end-to-end through
// the proxy), rather than by function. Each test drives a real HTTP request
// through p.Handler against httptest upstreams and asserts the observable
// outcome a user cares about. Shared helpers (testProv, post, stringReader,
// fakeUpstream) live in proxy_routing_test.go / fusion_test.go.

// --- mock helpers specific to use cases ---

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
	p.expandedRoutes = p.buildExpandedRoutes(authNotReady(p.providers))
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
func (f *fakeProviderImpl) ExtraHeaders(req *http.Request, _ []byte, _ string, path string) {}
func (f *fakeProviderImpl) FilterModelIDs(ids []string) (kept, dropped []string)            { return ids, nil }

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
