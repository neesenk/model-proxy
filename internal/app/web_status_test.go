package app

import (
	"encoding/json"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPIStatus(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	// Exact structural fields must be present (shape check on key presence).
	for _, want := range []string{`"uptime"`, `"version"`, `"listen"`, `"health"`, `"schedule"`, `"counters"`} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing %s: %s", want, body)
		}
	}
}

// TestAPIStatusCacheField: /api/status exposes cache observability — with
// cache.enabled on, the cache object carries enabled + hits/misses/entries;
// with the cache off (default), it reports enabled:false.
func TestAPIStatusCacheField(t *testing.T) {
	// Disabled path (newTestWeb's config has no cache block).
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	var off struct {
		Cache struct {
			Enabled bool `json:"enabled"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &off); err != nil {
		t.Fatalf("parse status (off): %v", err)
	}
	if off.Cache.Enabled {
		t.Errorf("cache.enabled=true want false (cache disabled): %s", rec.Body.String())
	}

	// Enabled path with one recorded hit + one stored entry.
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
cache: {enabled: true, ttl: 1h}
`))
	p := newTestProxy(t, cfg)
	if p.cache == nil {
		t.Fatal("cache not created despite cache.enabled")
	}
	now := time.Now()
	p.cache.Put("k", http.StatusOK, nil, []byte("x"), now)
	if _, ok := p.cache.Lookup("k", now); !ok {
		t.Fatal("seeded cache entry should hit")
	}
	w2 := NewWebServer(p, "test-config.yaml")
	mux2 := http.NewServeMux()
	w2.Register(mux2)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/status", nil))
	var on struct {
		Cache struct {
			Enabled bool   `json:"enabled"`
			Hits    uint64 `json:"hits"`
			Misses  uint64 `json:"misses"`
			Entries uint64 `json:"entries"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &on); err != nil {
		t.Fatalf("parse status (on): %v", err)
	}
	if !on.Cache.Enabled || on.Cache.Hits != 1 || on.Cache.Entries != 1 {
		t.Errorf("cache status = %+v want enabled=true hits=1 entries=1: %s", on.Cache, rec2.Body.String())
	}
}

// TestAPIStatusModelLocks: /api/status exposes ACTIVE model locks (provider →
// [{model, until}]) so `doctor --live` can explain a route whose targets are
// locked out. Expired locks are omitted — same future-only convention as
// circuit_until / rate_limited_until.
func TestAPIStatusModelLocks(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.recordModelFailure("zhipu", "glm-x", Scheduling{ModelLockout: "1h"})
	p.runtimeState.RecordModelFailure("zhipu", "old", -time.Minute, 0)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var out struct {
		ModelLocks map[string][]struct {
			Model string `json:"model"`
			Until string `json:"until"`
		} `json:"model_locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	locks := out.ModelLocks["zhipu"]
	if len(locks) != 1 || locks[0].Model != "glm-x" {
		t.Fatalf("model_locks = %+v, want exactly the active (zhipu, glm-x) lock", out.ModelLocks)
	}
	until, err := time.Parse(time.RFC3339, locks[0].Until)
	if err != nil || !time.Now().Before(until) {
		t.Errorf("until = %q, want a future RFC3339 time", locks[0].Until)
	}
}

func TestAPIStatus_IncludesRouteWarnings(t *testing.T) {
	// Two logged-in apikey providers share an unrated model → ambiguity warning.
	home := t.TempDir()
	credDir := home + "/.model-proxy"
	os.MkdirAll(credDir, 0o700)
	for _, n := range []string{"zhipu", "deepseek"} {
		os.WriteFile(credDir+"/"+n+"_apikey.json", []byte(`{"api_key":"k"}`), 0o600)
	}
	t.Setenv("HOME", home)

	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x, models: [shared-model]}
  deepseek: {provider_id: deepseek, openai_base_url: https://x, models: [shared-model]}
`))
	p := newTestProxy(t, cfg)
	if len(p.routeWarnings) == 0 {
		t.Fatalf("expected routeWarnings, got none (implicit=%v)", p.implicitRoutes)
	}
	w := NewWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"warnings"`) || !strings.Contains(body, "shared-model") {
		t.Errorf("/api/status should include warnings with shared-model: %s", body)
	}
}

// TestAPIStatusUnknown404 asserts the catch-all still 404s for unknown /api paths
// once the first real route (/api/status) is wired. Guards against a future
// router change silently swallowing unknown paths.
func TestAPIStatusUnknown404(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/no-such-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown /api path status=%d want 404", rec.Code)
	}
}

// TestAPITokens verifies /api/tokens returns the snapshot and /api/tokens/reset
// zeros it. The snapshot key shape is {provider, model} → {input, output, ...}.
func TestAPITokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	p.tokens.Commit(counters.TokenKey{Provider: "zhipu", Model: "glm-5"}, counters.TokenUsage{Input: 30, Output: 12})

	mux := http.NewServeMux()
	w.Register(mux)

	// GET /api/tokens returns the committed usage.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/tokens", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"input":30`) || !strings.Contains(rec.Body.String(), `"output":12`) {
		t.Errorf("tokens body missing committed usage: %s", rec.Body.String())
	}

	// POST /api/tokens/reset clears the counter.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/tokens/reset", nil))
	if rec2.Code != 200 {
		t.Fatalf("reset status=%d want 200", rec2.Code)
	}
	if len(p.tokens.Snapshot()) != 0 {
		t.Errorf("after reset, snapshot non-empty: %+v", p.tokens.Snapshot())
	}
}

// TestAPIQuotaRefresh verifies POST /api/quota/refresh triggers an immediate
// quota poll of every provider (so the Web UI's "Refresh usage" button re-polls
// on demand instead of waiting for the next interval). A counting provider
// asserts Quota() was actually invoked - a 200-only check would miss a no-op.
func TestAPIQuotaRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	// Inject a counting provider; the background poll goroutine is debounced by
	// its 10s bootstrap delay, so calls here are from the refresh endpoint.
	var calls atomic.Int32
	prov := &quotaCallProv{calls: &calls}
	p.providers["zhipu"] = prov
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/quota/refresh", nil))
	if rec.Code != 200 {
		t.Fatalf("refresh status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "refreshed") {
		t.Errorf("body=%s want refreshed", rec.Body.String())
	}
	// The endpoint runs pollAll synchronously, so the Quota() call has landed by
	// the time the response returns.
	if got := calls.Load(); got < 1 {
		t.Errorf("refresh did not poll: Quota() called %d times, want >=1", got)
	}
	// The snapshot is populated (proves the poll result was stored).
	if s := p.quota.Snapshot("zhipu"); s == nil || s.RemainingPct != 0.5 {
		t.Errorf("after refresh, snapshot=%+v want RemainingPct 0.5", s)
	}
}

// TestAPIQuotaRefreshOne verifies POST /api/quota/refresh {"provider":"<key>"}
// polls ONLY that one account's provider (not every provider). Mirrors the
// per-account "Refresh usage" button: the provider key is a config name or a
// pooled-account virtual id. Asserts the named provider was polled AND another
// was not - a 200-only check would miss a "poll all" regression. An unknown key
// is a 404.
func TestAPIQuotaRefreshOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	var zhipuCalls, deepseekCalls atomic.Int32
	p.providers["zhipu"] = &quotaCallProv{calls: &zhipuCalls}
	p.providers["deepseek"] = &quotaCallProv{calls: &deepseekCalls}
	mux := http.NewServeMux()
	w.Register(mux)

	// Poll just zhipu.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/quota/refresh",
		strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("refresh-one status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider":"zhipu"`) {
		t.Errorf("body=%s want provider echo", rec.Body.String())
	}
	if got := zhipuCalls.Load(); got != 1 {
		t.Errorf("zhipu Quota() called %d times, want 1", got)
	}
	// deepseek must NOT have been polled - this is the single-account contract.
	if got := deepseekCalls.Load(); got != 0 {
		t.Errorf("deepseek Quota() called %d times, want 0 (single-account refresh)", got)
	}

	// Unknown provider key -> 404, nothing polled.
	before := zhipuCalls.Load()
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/quota/refresh",
		strings.NewReader(`{"provider":"ghost"}`)))
	if rec2.Code != 404 {
		t.Errorf("unknown provider status=%d want 404, body=%s", rec2.Code, rec2.Body.String())
	}
	if got := zhipuCalls.Load(); got != before {
		t.Errorf("unknown-provider refresh polled zhipu %d extra times", got-before)
	}
}
func TestAPILogs(t *testing.T) {
	tmp := t.TempDir() + "/model-proxy.log"
	logContent := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(tmp, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestWeb(t)
	w.logFile = tmp // override hook for tests

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	w.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/logs?tail=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	got := rec.Body.String()
	// last 2 lines
	if !strings.Contains(got, "line4") || !strings.Contains(got, "line5") {
		t.Errorf("tail=2 missing last lines: %q", got)
	}
	if strings.Contains(got, "line1") {
		t.Errorf("tail=2 should drop line1: %q", got)
	}
}

// TestAPIStatusCredentialStore pins the S1 observability closeout: /api/status
// carries credential_store naming the resolved credstore backend. Test binaries
// resolve to file mode (credstore hermeticity guard), which the assertion pins.
func TestAPIStatusCredentialStore(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var v struct {
		CredentialStore string `json:"credential_store"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if v.CredentialStore != "file" {
		t.Fatalf("credential_store = %q, want \"file\" under the test-binary guard", v.CredentialStore)
	}
}
