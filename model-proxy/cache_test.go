package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var errCacheTestClientGone = errors.New("test client write failed")

// cacheTestFailingWriter deterministically emulates a client that disconnects
// at the first streamed byte. Unlike closing an httptest TCP client, its Write
// error is synchronous, so the cache-completeness assertion has no timing
// dependency.
type cacheTestFailingWriter struct {
	header http.Header
	writes int
}

func (w *cacheTestFailingWriter) Header() http.Header { return w.Header() }
func (w *cacheTestFailingWriter) WriteHeader(int)     {}
func (w *cacheTestFailingWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errCacheTestClientGone
}

func TestNewResponseCacheAdaptsResolvedConfig(t *testing.T) {
	if store := newResponseCache(CacheConfig{}); store != nil {
		t.Fatalf("disabled cache created Store %#v", store)
	}
	store := newResponseCache(CacheConfig{
		Enabled: true, TTL: "1m", MaxEntries: 2, MaxBodyBytes: 123,
	})
	if store == nil {
		t.Fatal("enabled cache did not create Store")
	}
	if got := store.MaxBodyBytes(); got != 123 {
		t.Errorf("MaxBodyBytes = %d, want configured 123", got)
	}
	now := time.Unix(1000, 0)
	store.Put("key", http.StatusOK, nil, []byte("body"), now)
	if _, ok := store.Lookup("key", now.Add(2*time.Minute)); ok {
		t.Error("configured one-minute TTL did not expire entry")
	}
}

// TestForward_CacheHit: the first of two byte-identical requests is served from
// the upstream (and cached); the second is served from cache with NO upstream
// call. A different body bypasses the cache and hits the upstream again.
func TestForward_CacheHit(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","content":"hello"}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	if p.cache == nil {
		t.Fatal("cache not created despite cache.enabled")
	}
	p.providers["z"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	body := `{"model":"glm","input":[{"role":"user","content":"same"}]}`
	do := func() string {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	first := do()
	if hits != 1 {
		t.Errorf("first request: upstream hits=%d want 1", hits)
	}
	second := do()
	if hits != 1 {
		t.Errorf("second (cached) request: upstream hits=%d want 1 (served from cache)", hits)
	}
	if first != second {
		t.Errorf("cached response differs from original:\nfirst:  %s\nsecond: %s", first, second)
	}
	if hits := p.cache.Stats().Hits; hits != 1 {
		t.Errorf("cache hits=%d want 1", hits)
	}

	// A different body misses → upstream hit again.
	body = `{"model":"glm","input":[{"role":"user","content":"different"}]}`
	do()
	if hits != 2 {
		t.Errorf("different body: upstream hits=%d want 2 (cache miss)", hits)
	}
}

// TestForward_CacheHitHeader: a cache hit is marked with the `x-mp-cache: hit`
// response header (proxy.go) so a client can tell a replayed response apart
// from a fresh upstream one; the first (miss) response carries no such header.
func TestForward_CacheHitHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","content":"hello"}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	body := `{"model":"glm","input":[{"role":"user","content":"same"}]}`
	do := func() http.Header {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		h := resp.Header
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return h
	}
	if h := do(); h.Get("x-mp-cache") != "" {
		t.Errorf("first (miss) response x-mp-cache=%q want empty", h.Get("x-mp-cache"))
	}
	if h := do(); h.Get("x-mp-cache") != "hit" {
		t.Errorf("second (cached) response x-mp-cache=%q want hit", h.Get("x-mp-cache"))
	}
}

// TestReload_RebuildsCache: reload() swaps the cache from the NEW config
// (proxy.go), so toggling cache.enabled via SIGHUP/web-edit takes effect
// without a restart: enabled → disabled drops the cache entirely; re-enabling
// builds a FRESH one (old entries/counters are not carried over).
func TestReload_RebuildsCache(t *testing.T) {
	useStaticProviderPools(t, "z")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	base := "listen: 127.0.0.1:0\n" +
		"providers:\n  z:\n    openai_base_url: http://127.0.0.1:1\n    provider_id: static\n" +
		"routes:\n  m:\n    - {provider: z, model: m}\n"
	write := func(cacheBlock string) {
		if err := os.WriteFile(cfgPath, []byte(base+cacheBlock), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cache:\n  enabled: true\n")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	if p.cache == nil {
		t.Fatal("cache not created despite cache.enabled")
	}
	p.cache.Put("k", http.StatusOK, nil, []byte("x"), time.Now())

	// Disable via reload → cache gone.
	write("")
	if err := p.reload(cfgPath); err != nil {
		t.Fatalf("reload (disable): %v", err)
	}
	if p.cache != nil {
		t.Error("reload with cache disabled should drop the cache (p.cache != nil)")
	}

	// Re-enable via reload → a fresh, empty cache.
	write("cache:\n  enabled: true\n")
	if err := p.reload(cfgPath); err != nil {
		t.Fatalf("reload (re-enable): %v", err)
	}
	if p.cache == nil {
		t.Fatal("reload with cache re-enabled should create a cache")
	}
	if entries := p.cache.Stats().Entries; entries != 0 {
		t.Errorf("rebuilt cache entries=%d want 0 (fresh, old entries not carried)", entries)
	}
}

func TestReload_DoesNotAdoptOldGenerationCacheWrites(t *testing.T) {
	useStaticProviderPools(t, "z")
	firstUpstream := make(chan struct{})
	releaseFirst := make(chan struct{})
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			close(firstUpstream)
			<-releaseFirst
		}
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, `{"id":"response"}`)
	}))
	defer upstream.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := "listen: 127.0.0.1:0\n" +
		"providers:\n  z:\n    provider_id: static\n    openai_base_url: " + upstream.URL + "\n" +
		"routes:\n  model:\n    - {provider: z, model: model}\n" +
		"cache: {enabled: true, ttl: 1h}\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newTestProxy(t, config)
	server := httptest.NewServer(http.HandlerFunc(proxy.handler))
	defer server.Close()

	requestBody := `{"model":"model","input":[{"role":"user","content":"same"}]}`
	type result struct {
		cacheHeader string
		err         error
	}
	do := func() result {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(requestBody))
		if err != nil {
			return result{err: err}
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return result{err: err}
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			return result{err: readErr}
		}
		return result{cacheHeader: response.Header.Get("x-mp-cache"), err: closeErr}
	}

	firstDone := make(chan result, 1)
	go func() { firstDone <- do() }()
	select {
	case <-firstUpstream:
	case <-time.After(3 * time.Second):
		close(releaseFirst)
		t.Fatal("old-generation request did not reach upstream")
	}
	reloadErr := proxy.reload(configPath)
	close(releaseFirst)
	if reloadErr != nil {
		t.Fatalf("reload while old request is in flight: %v", reloadErr)
	}
	if first := <-firstDone; first.err != nil {
		t.Fatalf("old-generation request: %v", first.err)
	}

	if second := do(); second.err != nil || second.cacheHeader != "" {
		t.Fatalf("first new-generation request = cache:%q err:%v, want upstream miss", second.cacheHeader, second.err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits after new-generation miss = %d, want 2", got)
	}
	if third := do(); third.err != nil || third.cacheHeader != "hit" {
		t.Fatalf("second new-generation request = cache:%q err:%v, want cache hit", third.cacheHeader, third.err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("upstream hits after cache replay = %d, want 2", got)
	}
}

// TestForward_CacheModeMismatch_ContentType (E1): a converted response whose
// stream mode was rewritten (client asked stream:true, the openai backend
// answered a plain JSON body) is served live as SSE with content-type
// text/event-stream. The cached entry must record THAT content-type — before
// the fix the cache stored the upstream's application/json, so a replay paired
// an SSE body with a JSON content-type.
func TestForward_CacheModeMismatch_ContentType(t *testing.T) {
	const openaiResp = `{"id":"a","model":"gpt","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(openaiResp))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt", Protocol: "openai"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	do := func() (string, string) {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/messages",
			strings.NewReader(`{"model":"claude-x","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		ct := resp.Header.Get("content-type")
		mark := resp.Header.Get("x-mp-cache")
		resp.Body.Close()
		if mark == "" {
			return string(b), ct
		}
		return string(b), ct + " [cached]"
	}
	firstBody, firstCT := do()
	if firstCT != "text/event-stream" {
		t.Fatalf("live mode-mismatch response content-type=%q want text/event-stream", firstCT)
	}
	secondBody, secondCT := do() // cache hit
	if secondCT != "text/event-stream [cached]" {
		t.Errorf("cache replay content-type=%q want text/event-stream (SSE body mislabeled)", secondCT)
	}
	if firstBody != secondBody {
		t.Errorf("cache replay body differs from live:\nlive:   %s\nreplay: %s", firstBody, secondBody)
	}
}

// TestForward_CacheConvert_ReplayIntact: a converted response that is cached
// must replay byte-identical. Before the fix the cached entry held the backend's
// Content-Length (openai body length) while the cached body was the converted
// (anthropic, different-length) body — replay sent a mismatched length and
// corrupted the response. The fix strips Content-Length when caching a converted
// response; Go re-derives it on replay.
func TestForward_CacheConvert_ReplayIntact(t *testing.T) {
	const openaiResp = `{"id":"a","model":"gpt","choices":[{"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(openaiResp))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt", Protocol: "openai"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	do := func() string {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/messages", strings.NewReader(`{"model":"claude-x","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	first := do()
	second := do() // cache hit
	if !strings.Contains(first, `"type":"message"`) || !strings.Contains(first, `"text":"hello world"`) {
		t.Fatalf("first response not the anthropic conversion: %s", first)
	}
	if first != second {
		t.Errorf("cache replay differs from the original converted response (corrupted by cached Content-Length):\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestForward_CancelledConvertedSSEIsNotCached verifies that a write failure
// while converting a Responses SSE stream to Chat cannot cache a partial client
// stream. The retry must reach the backend and receive one complete [DONE]
// terminator, rather than replaying the interrupted first response.
func TestForward_CancelledConvertedSSEIsNotCached(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/responses" {
			t.Errorf("backend path=%q want /responses", r.URL.Path)
		}
		got := captureHit(r)
		if got.model != "backend-model" {
			t.Errorf("backend model=%q want backend-model", got.model)
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, responsesTextSSE)
	}))
	defer up.Close()

	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"backend": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"client-model": {{Provider: "backend", Model: "backend-model", Protocol: "responses"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	})
	p.providers["backend"] = &testProv{key: "test-key"}

	body := `{"model":"client-model","messages":[{"role":"user","content":"hello"}],"stream":true}`
	firstReq := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", strings.NewReader(body))
	failing := &cacheTestFailingWriter{header: make(http.Header)}
	p.handler(failing, firstReq)
	if failing.writes != 1 {
		t.Fatalf("interrupted request writes=%d want 1", failing.writes)
	}
	if hits != 1 {
		t.Fatalf("interrupted request upstream hits=%d want 1", hits)
	}
	if stats := p.cache.Stats(); stats.Hits != 0 || stats.Entries != 0 {
		t.Fatalf("interrupted stream cache stats hits=%d entries=%d want 0/0", stats.Hits, stats.Entries)
	}

	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	secondReq, err := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	secondResp, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := io.ReadAll(secondResp.Body)
	secondResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if secondResp.StatusCode != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", secondResp.StatusCode, secondBody)
	}
	if got := secondResp.Header.Get("x-mp-cache"); got != "" {
		t.Fatalf("retry x-mp-cache=%q want empty (must not replay partial stream)", got)
	}
	if hits != 2 {
		t.Fatalf("retry upstream hits=%d want 2 (partial stream must not be cached)", hits)
	}
	if got := strings.Count(string(secondBody), "data: [DONE]"); got != 1 {
		t.Fatalf("retry [DONE] count=%d want 1; body=%s", got, secondBody)
	}
	if !strings.Contains(string(secondBody), `"content":"hel"`) || !strings.Contains(string(secondBody), `"content":"lo"`) {
		t.Fatalf("retry missing complete converted content: %s", secondBody)
	}
	if stats := p.cache.Stats(); stats.Hits != 0 || stats.Entries != 1 {
		t.Fatalf("after clean retry cache stats hits=%d entries=%d want 0/1", stats.Hits, stats.Entries)
	}
}

// TestForward_ForcedPooledProviderBypassesCache verifies that a cache primed by
// the primary route never masks a forced pooled-parent request. The forced
// request must use exactly one account-bound virtual provider, including its
// bound Bearer token and rewritten backend model.
func TestForward_ForcedPooledProviderBypassesCache(t *testing.T) {
	poolHome := t.TempDir()
	setPoolHome(t, poolHome)
	writePoolFile(t, "pooled", "zhipu", "POOL-KEY-A", "POOL-KEY-B")

	var primaryHits, pooledHits int
	pooledAuth := make(chan string, 2)
	primaryUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		got := captureHit(r)
		if got.model != "primary-model" {
			t.Errorf("primary model=%q want primary-model", got.model)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"from":"primary"}`)
	}))
	defer primaryUp.Close()
	pooledUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pooledHits++
		got := captureHit(r)
		if got.model != "pooled-model" {
			t.Errorf("pooled model=%q want pooled-model", got.model)
		}
		if got.auth != "Bearer POOL-KEY-A" && got.auth != "Bearer POOL-KEY-B" {
			t.Errorf("pooled Authorization=%q want exact bound pool key", got.auth)
		}
		pooledAuth <- got.auth
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"from":"pooled"}`)
	}))
	defer pooledUp.Close()

	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"primary": {OpenAIBaseURL: primaryUp.URL, Provider: testProviderID},
			"pooled":  {OpenAIBaseURL: pooledUp.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{"client-model": {
			{Provider: "primary", Model: "primary-model", Priority: 1},
			{Provider: "pooled", Model: "pooled-model", Priority: 2},
		}},
		Cache: CacheConfig{Enabled: true, TTL: "1h"},
	})
	p.providers["primary"] = &testProv{key: "primary-key"}
	if got := len(p.poolIndex["pooled"]); got != 2 {
		t.Fatalf("pooled virtual count=%d want 2", got)
	}
	// A mixed route is normally pool-assigned. Temporarily make both pool
	// virtuals unavailable so an ordinary (cacheable) request deterministically
	// primes from the static primary; no force or pin may be used because both
	// deliberately bypass the cache.
	for _, virtual := range p.poolIndex["pooled"] {
		seedRuntimeRateLimit(t, p, virtual, time.Now().Add(time.Hour), rlTransient)
	}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	body := `{"model":"client-model","input":[]}`
	do := func(force string) (http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if force != "" {
			req.Header.Set("x-mp-force-provider", force)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("force=%q status=%d body=%s", force, resp.StatusCode, out)
		}
		return resp.Header, string(out)
	}

	if _, got := do(""); got != `{"from":"primary"}` {
		t.Fatalf("prime body=%s want primary", got)
	}
	if primaryHits != 1 || pooledHits != 0 {
		t.Fatalf("after prime primary/pooled hits=%d/%d want 1/0", primaryHits, pooledHits)
	}
	if h, got := do(""); h.Get("x-mp-cache") != "hit" || got != `{"from":"primary"}` {
		t.Fatalf("ordinary replay cache=%q body=%s want hit/primary", h.Get("x-mp-cache"), got)
	}
	if primaryHits != 1 || pooledHits != 0 {
		t.Fatalf("ordinary replay primary/pooled hits=%d/%d want 1/0", primaryHits, pooledHits)
	}
	p.resetHealth("pooled")
	if h, got := do("pooled"); h.Get("x-mp-cache") != "" || got != `{"from":"pooled"}` {
		t.Fatalf("forced pooled response cache=%q body=%s want empty/pooled", h.Get("x-mp-cache"), got)
	}
	if primaryHits != 1 || pooledHits != 1 {
		t.Fatalf("forced pooled primary/pooled hits=%d/%d want 1/1", primaryHits, pooledHits)
	}
	parentAuth := <-pooledAuth
	if parentAuth != "Bearer POOL-KEY-A" && parentAuth != "Bearer POOL-KEY-B" {
		t.Fatalf("forced parent Authorization=%q want one configured pool key", parentAuth)
	}

	// The parent case above proves force-provider expands a pool. Also force one
	// concrete virtual so the test binds virtual identity to its exact key;
	// accepting either key alone would miss a credential swap between accounts.
	specificVirtual := p.poolIndex["pooled"][0]
	keyByVirtual := map[string]string{
		"pooled#" + accountIDFor("zhipu", accountCred{APIKey: "POOL-KEY-A"}): "POOL-KEY-A",
		"pooled#" + accountIDFor("zhipu", accountCred{APIKey: "POOL-KEY-B"}): "POOL-KEY-B",
	}
	specificKey, ok := keyByVirtual[specificVirtual]
	if !ok {
		t.Fatalf("specific virtual %q has no configured key mapping", specificVirtual)
	}
	if h, got := do(specificVirtual); h.Get("x-mp-cache") != "" || got != `{"from":"pooled"}` {
		t.Fatalf("forced virtual response cache=%q body=%s want empty/pooled", h.Get("x-mp-cache"), got)
	}
	if got := <-pooledAuth; got != "Bearer "+specificKey {
		t.Fatalf("forced virtual %q Authorization=%q want Bearer %s", specificVirtual, got, specificKey)
	}
	if primaryHits != 1 || pooledHits != 2 {
		t.Fatalf("forced virtual primary/pooled hits=%d/%d want 1/2", primaryHits, pooledHits)
	}
}

// TestForward_CacheBypassedByForceProvider (F3): a primed cache must NOT serve a
// `replay`/force-provider request — it must hit the chosen backend. Before the
// fix, the cache key (method+path+body) matched and replay returned the OLD
// provider's cached answer.
func TestForward_CacheBypassedByForceProvider(t *testing.T) {
	var aHits, bHits int
	aUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits++
		w.Write([]byte(`{"from":"a"}`))
	}))
	defer aUp.Close()
	bUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits++
		w.Write([]byte(`{"from":"b"}`))
	}))
	defer bUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: bUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"glm": {
			{Provider: "a", Model: "glm", Priority: 1},
			{Provider: "b", Model: "glm", Priority: 2},
		}},
		Cache: CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "a"}
	p.providers["b"] = &testProv{key: "b"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	body := `{"model":"glm","input":[]}`
	// 1) Prime the cache with provider A (priority 1).
	do := func(headers map[string]string) string {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	if first := do(nil); !strings.Contains(first, `"from":"a"`) {
		t.Fatalf("prime: expected a, got %s", first)
	}
	if aHits != 1 {
		t.Fatalf("prime: aHits=%d want 1", aHits)
	}
	// 2) Same body from cache (no force header) → served from cache, no new upstream hit.
	if second := do(nil); !strings.Contains(second, `"from":"a"`) {
		t.Errorf("cached: expected a, got %s", second)
	}
	if aHits != 1 || bHits != 0 {
		t.Errorf("cached: aHits=%d bHits=%d want 1/0 (cache hit)", aHits, bHits)
	}
	// 3) force-provider: b → must bypass cache and hit B (replay semantics).
	if third := do(map[string]string{"x-mp-force-provider": "b"}); !strings.Contains(third, `"from":"b"`) {
		t.Errorf("force-provider b: expected b, got %s (cache not bypassed)", third)
	}
	if bHits != 1 {
		t.Errorf("force-provider b: bHits=%d want 1", bHits)
	}
}

// TestForward_ForceProvider_TypoHardFails (regression #2): when the force-provider
// override names a provider that is NOT a target for the route (a `replay --to`
// typo), the request must HARD-FAIL (400) instead of silently falling back to
// normal scheduling. Pre-fix another provider answered while `replay --to typo`
// still reported the typo'd name, polluting comparison conclusions.
func TestForward_ForceProvider_TypoHardFails(t *testing.T) {
	var aHits, bHits int
	aUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits++
		w.Write([]byte(`{"from":"a"}`))
	}))
	defer aUp.Close()
	bUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits++
		w.Write([]byte(`{"from":"b"}`))
	}))
	defer bUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: bUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"glm": {
			{Provider: "a", Model: "glm", Priority: 1},
			{Provider: "b", Model: "glm", Priority: 2},
		}},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "a"}
	p.providers["b"] = &testProv{key: "b"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	do := func(force string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":[]}`))
		if force != "" {
			req.Header.Set("x-mp-force-provider", force)
		}
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// Typo: "z" is not a target of route glm. Pre-fix this fell back to normal
	// scheduling (a answered 200) while replay --to z claimed z served it.
	status, body := do("z")
	if status != http.StatusBadRequest {
		t.Errorf("typo force-provider: status=%d body=%q, want 400 (hard-fail, not silent fallback)", status, body)
	}
	if aHits != 0 || bHits != 0 {
		t.Errorf("typo force-provider was served anyway: aHits=%d bHits=%d, want 0/0", aHits, bHits)
	}

	// Sanity: a VALID force-provider still routes to that provider.
	if status, _ := do("b"); status != http.StatusOK {
		t.Errorf("valid force-provider b: status=%d, want 200 (hard-fail must not break the valid path)", status)
	}
	if bHits != 1 {
		t.Errorf("valid force-provider b: bHits=%d want 1", bHits)
	}
}
