package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCacheKeyOf(t *testing.T) {
	body := []byte(`{"model":"glm","input":[]}`)
	k1 := cacheKeyOf("POST", "/v1/responses", body)
	k2 := cacheKeyOf("POST", "/v1/responses", body)
	if k1 != k2 {
		t.Error("identical requests must hash to the same key")
	}
	// Different body → different key.
	if cacheKeyOf("POST", "/v1/responses", []byte(`{"model":"glm","input":[{"role":"user"}]}`)) == k1 {
		t.Error("different body hashed to same key")
	}
	// Different path → different key.
	if cacheKeyOf("POST", "/v1/messages", body) == k1 {
		t.Error("different path hashed to same key")
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
