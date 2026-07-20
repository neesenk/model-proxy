package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheKeyOf(t *testing.T) {
	body := []byte(`{"model":"glm","input":[]}`)
	mk := func(path, query string, headers map[string]string) *http.Request {
		u := "http://x" + path
		if query != "" {
			u += "?" + query
		}
		req, _ := http.NewRequest("POST", u, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req
	}
	k1 := cacheKeyOf(mk("/v1/responses", "", nil), body)
	// identical → same key
	if cacheKeyOf(mk("/v1/responses", "", nil), body) != k1 {
		t.Error("identical requests must hash to the same key")
	}
	// different body → different key
	if cacheKeyOf(mk("/v1/responses", "", nil), []byte(`{"model":"glm","input":[{"role":"user"}]}`)) == k1 {
		t.Error("different body hashed to same key")
	}
	// different path → different key
	if cacheKeyOf(mk("/v1/messages", "", nil), body) == k1 {
		t.Error("different path hashed to same key")
	}
	// different query → different key (regression #8)
	if cacheKeyOf(mk("/v1/responses", "version=2", nil), body) == k1 {
		t.Error("different query hashed to same key (cache collision)")
	}
	// different anthropic-beta → different key
	if cacheKeyOf(mk("/v1/responses", "", map[string]string{"anthropic-beta": "output-128k-2025-02-19"}), body) == k1 {
		t.Error("different anthropic-beta hashed to same key (response-affecting header ignored)")
	}
	// different accept-language → different key
	if cacheKeyOf(mk("/v1/responses", "", map[string]string{"accept-language": "zh-CN"}), body) == k1 {
		t.Error("different accept-language hashed to same key (response-affecting header ignored)")
	}
	// a NON-response-affecting header must NOT change the key (guard against over-folding)
	if cacheKeyOf(mk("/v1/responses", "", map[string]string{"x-custom": "whatever"}), body) != k1 {
		t.Error("non-response-affecting header changed the key (over-folding)")
	}
}

func TestResponseCache_GetPutExpiry(t *testing.T) {
	c := newResponseCache(CacheConfig{Enabled: true, TTL: "1h"})
	now := time.Now()
	c.put("k", &cacheEntry{status: 200, header: http.Header{"content-type": {"application/json"}}, body: []byte("ok")}, now)
	if c.hits != 0 || c.misses != 0 {
		t.Errorf("post-put counters hits=%d misses=%d want 0/0", c.hits, c.misses)
	}
	e, ok := c.get("k", now.Add(time.Second))
	if !ok || e.status != 200 || string(e.body) != "ok" {
		t.Errorf("get hit=%v entry=%+v want 200/ok", ok, e)
	}
	if c.hits != 1 {
		t.Errorf("hits=%d want 1", c.hits)
	}
	// Expired → miss + lazy evict.
	got, ok2 := c.get("k", now.Add(2*time.Hour))
	if ok2 || got != nil {
		t.Errorf("expired entry should miss, got=%+v ok=%v", got, ok2)
	}
	if _, stillThere := c.get("k", now.Add(2*time.Hour)); stillThere {
		t.Error("expired entry not evicted")
	}
}

func TestResponseCache_NilSafe(t *testing.T) {
	var c *responseCache // nil (cache disabled)
	if _, ok := c.get("k", time.Now()); ok {
		t.Error("nil cache get should miss")
	}
	// put must not panic on a nil cache.
	c.put("k", &cacheEntry{status: 200, body: []byte("x")}, time.Now())
}

func TestResponseCache_Eviction(t *testing.T) {
	c := newResponseCache(CacheConfig{Enabled: true, TTL: "1h", MaxEntries: 2})
	now := time.Now()
	c.put("a", &cacheEntry{body: []byte("a")}, now)
	c.put("b", &cacheEntry{body: []byte("b")}, now)
	c.put("c", &cacheEntry{body: []byte("c")}, now) // over cap → evict one
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n != 2 {
		t.Errorf("after over-cap put, len=%d want 2 (max_entries)", n)
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
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := NewProxy(cfg)
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
	if p.cache.hits != 1 {
		t.Errorf("cache.hits=%d want 1", p.cache.hits)
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
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := NewProxy(cfg)
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
	p := NewProxy(cfg)
	if p.cache == nil {
		t.Fatal("cache not created despite cache.enabled")
	}
	p.cache.put("k", &cacheEntry{status: 200, body: []byte("x")}, time.Now())

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
	if _, _, entries := p.cache.stats(); entries != 0 {
		t.Errorf("rebuilt cache entries=%d want 0 (fresh, old entries not carried)", entries)
	}
}

// TestCacheRecorder_Completeness (F3): sawEOF is set only on a clean EOF, so a
// response truncated by a client disconnect (read ends with a non-EOF error) is
// NOT treated as cacheable. A full read (EOF) is.
func TestCacheRecorder_Completeness(t *testing.T) {
	// Clean EOF → sawEOF true, not truncated.
	rec := newCacheRecorder(io.NopCloser(strings.NewReader("hello world")), 100)
	io.Copy(io.Discard, rec)
	if !rec.sawEOF || rec.truncated {
		t.Errorf("clean EOF: sawEOF=%v truncated=%v want true/false", rec.sawEOF, rec.truncated)
	}
	// Non-EOF termination (simulated disconnect) → sawEOF false.
	rec2 := newCacheRecorder(&errReader{err: io.ErrUnexpectedEOF}, 100)
	io.Copy(io.Discard, rec2)
	if rec2.sawEOF {
		t.Error("non-EOF termination should leave sawEOF false (partial → uncachable)")
	}
}

// errReader is a ReadCloser that returns 0 bytes + a fixed non-EOF error,
// simulating a mid-stream abort (client disconnect / upstream error).
type errReader struct{ err error }

func (e *errReader) Read(p []byte) (int, error) { return 0, e.err }
func (e *errReader) Close() error               { return nil }

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
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt", Protocol: "openai"}}},
		Cache:     CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := NewProxy(cfg)
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
			"a": {OpenAIBaseURL: aUp.URL, Provider: "static"},
			"b": {OpenAIBaseURL: bUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{"glm": {
			{Provider: "a", Model: "glm", Priority: 1},
			{Provider: "b", Model: "glm", Priority: 2},
		}},
		Cache: CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := NewProxy(cfg)
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
			"a": {OpenAIBaseURL: aUp.URL, Provider: "static"},
			"b": {OpenAIBaseURL: bUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{"glm": {
			{Provider: "a", Model: "glm", Priority: 1},
			{Provider: "b", Model: "glm", Priority: 2},
		}},
	}
	p := NewProxy(cfg)
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
