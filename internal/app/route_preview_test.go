package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
)

// TestDebugRoute_Preview: POST /debug/route answers where a request would go
// right now — route resolution, ordered targets, per-target fit — without an
// upstream call and without mutating scheduler state (sticky must not latch).
func TestDebugRoute_Preview(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
  deepseek: {provider_id: deepseek, openai_base_url: https://y}
routes:
  glm: [{provider: zhipu, model: glm}, {provider: deepseek, model: ds}]
claude_mapping:
  claude-glm: glm
`))
	p := newTestProxy(t, cfg)
	post := func(body string, headers map[string]string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/debug/route", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		p.Handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("preview json: %v, body = %s", err, rec.Body)
		}
		return out
	}

	// anthropic default proto: claude_mapping resolves to the route.
	out := post(`{"model":"claude-glm","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if out["route_found"] != true || out["exposed"] != "glm" {
		t.Fatalf("mapping preview = %v", out)
	}
	ordered, ok := out["ordered"].([]any)
	if !ok || len(ordered) != 2 {
		t.Fatalf("ordered = %v, want both route targets", out["ordered"])
	}
	first := ordered[0].(map[string]any)
	if first["provider"] != "zhipu" || first["model"] != "glm" {
		t.Errorf("first ordered target = %v", first)
	}
	if _, has := first["fits"]; !has {
		t.Errorf("fit verdict missing: %v", first)
	}

	// Unknown model: route_found false, no error status.
	out = post(`{"model":"no-such","messages":[]}`, nil)
	if out["route_found"] != false {
		t.Fatalf("unknown model preview = %v", out)
	}

	// Missing model field: reported in the payload.
	out = post(`{"messages":[]}`, nil)
	if out["route_found"] != false || out["error"] == nil {
		t.Fatalf("missing model preview = %v", out)
	}

	// force-provider participates; a typo'd provider is surfaced, not fatal.
	out = post(`{"model":"glm","messages":[]}`, map[string]string{"x-mp-force-provider": "deepseek"})
	ordered = out["ordered"].([]any)
	if len(ordered) != 1 || ordered[0].(map[string]any)["provider"] != "deepseek" {
		t.Fatalf("forced preview ordered = %v", out["ordered"])
	}
	out = post(`{"model":"glm","messages":[]}`, map[string]string{"x-mp-force-provider": "typo"})
	if out["route_found"] != true || out["error"] == nil || len(out["ordered"].([]any)) != 0 {
		t.Fatalf("typo force preview = %v", out)
	}

	// No scheduler mutation: the same preview twice yields the same first
	// target (a committed schedule would advance pool spread / could latch
	// sticky), and the manager reports no sticky for the route.
	before := post(`{"model":"glm","messages":[]}`, map[string]string{"x-claude-code-session-id": "s1"})
	after := post(`{"model":"glm","messages":[]}`, map[string]string{"x-claude-code-session-id": "s1"})
	if before["sticky"] != nil || after["sticky"] != nil {
		t.Errorf("preview latched sticky: before=%v after=%v", before["sticky"], after["sticky"])
	}
	firstOf := func(out map[string]any) string {
		return out["ordered"].([]any)[0].(map[string]any)["provider"].(string)
	}
	if firstOf(before) != firstOf(after) {
		t.Errorf("preview mutated scheduling: %s → %s", firstOf(before), firstOf(after))
	}

	// Bad proto → 400.
	req := httptest.NewRequest(http.MethodPost, "/debug/route?proto=gemini", strings.NewReader(`{"model":"glm"}`))
	rec := httptest.NewRecorder()
	p.Handler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad proto status = %d, want 400", rec.Code)
	}
}

// The preview's cache probe is read-only: polling /debug/route must not book
// misses into the operational hit-rate stats (Peek, not Lookup) — a poller
// watching the preview would otherwise grind the metrics down.
func TestDebugRoute_PreviewCacheProbeIsReadOnly(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
cache:
  enabled: true
  ttl: 1m
`))
	p := newTestProxy(t, cfg)
	if p.cache == nil {
		t.Fatal("config cache: section must enable the store")
	}
	body := `{"model":"glm","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	post := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/debug/route", strings.NewReader(body))
		rec := httptest.NewRecorder()
		p.Handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("preview json: %v, body = %s", err, rec.Body)
		}
		return out
	}

	// Prime the store entry the preview's probe will resolve to.
	keyReq := httptest.NewRequest(http.MethodPost, "/debug/route", strings.NewReader(body))
	p.cache.Put(responsecache.Key(keyReq, []byte(body)), http.StatusOK,
		http.Header{"Content-Type": {"application/json"}}, []byte(`{}`), time.Now())

	for i := 0; i < 3; i++ {
		if out := post(); out["cache"] != "hit" {
			t.Fatalf("preview %d cache state = %v, want hit", i, out["cache"])
		}
	}
	if got := p.cache.Stats(); got.Hits != 0 || got.Misses != 0 {
		t.Errorf("preview probe mutated stats: %+v, want zero counters", got)
	}
}
