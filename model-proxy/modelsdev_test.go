package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureAPI is a tiny models.dev/api.json-shaped blob. Both the canonical
// owner (zhipuai) and a reseller (openrouter) list bare "glm-4.6" — same key —
// so the dedup test exercises canonical-owner preference. deepseek lists a
// distinct model. Used across the parse/lookup/hydrate tests.
const fixtureAPI = `{
  "zhipuai": {
    "api": "https://open.bigmodel.cn/api/paas/v4",
    "models": {
      "glm-4.6": {"limit": {"context": 204800, "output": 131072}, "modalities": {"input": ["text"], "output": ["text"]}}
    }
  },
  "openrouter": {
    "api": "https://openrouter.ai/api/v1",
    "models": {
      "glm-4.6": {"limit": {"context": 999, "output": 999}, "modalities": {"input": ["text"], "output": ["text"]}}
    }
  },
  "deepseek": {
    "api": "https://api.deepseek.com",
    "models": {
      "deepseek-v4-pro": {"limit": {"context": 1000000, "output": 65536}, "modalities": {"input": ["text"], "output": ["text"]}}
    }
  }
}`

func TestParseModelsDevAPI_DedupAndEndpoints(t *testing.T) {
	cat := parseModelsDevAPI([]byte(fixtureAPI))
	// ByName: glm-4.6 deduped — canonical owner zhipuai wins (ctx 204800, not 999).
	md, ok := cat.ByName["glm-4.6"]
	if !ok {
		t.Fatal("glm-4.6 missing from ByName")
	}
	if md.Context != 204800 || md.Output != 131072 {
		t.Errorf("glm-4.6 canonical owner not preferred: got ctx=%d out=%d", md.Context, md.Output)
	}
	if len(cat.ByName) != 2 {
		t.Errorf("ByName dedup count = %d, want 2 (glm-4.6, deepseek-v4-pro); got %v", len(cat.ByName), catalogKeys(cat.ByName))
	}
	// deepseek-v4-pro present with its real limits.
	if md2 := cat.ByName["deepseek-v4-pro"]; md2.Context != 1000000 || md2.Output != 65536 {
		t.Errorf("deepseek-v4-pro metadata wrong: %+v", md2)
	}
	// ByEndpoint: zhipu's exact URL + host key both index glm-4.6.
	zhipuURL := normalizeEndpoint("https://open.bigmodel.cn/api/paas/v4")
	if names, ok := cat.ByEndpoint[zhipuURL]; !ok || !sliceContains(names, "glm-4.6") {
		t.Errorf("ByEndpoint[%s] missing glm-4.6: %v", zhipuURL, names)
	}
	if names, ok := cat.ByEndpoint["open.bigmodel.cn"]; !ok || !sliceContains(names, "glm-4.6") {
		t.Errorf("host-only endpoint key missing glm-4.6: %v", names)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://open.bigmodel.cn/api/paas/v4": "https://open.bigmodel.cn/api/paas/v4",
		"https://API.DeepSeek.com/":            "https://api.deepseek.com",
		"":                                     "",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// helpers
func catalogKeys(m map[string]modelsDevModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// keep imports used by later-appended tests honest.
var (
	_ = os.ReadFile
	_ = filepath.Join
	_ = time.Now
)

func TestCatalogLookup(t *testing.T) {
	cat := parseModelsDevAPI([]byte(fixtureAPI))

	// 1. Endpoint match: zhipu base URL → zhipuai provider → glm-4.6 (canonical).
	md, ok := cat.lookup([]string{"https://open.bigmodel.cn/api/paas/v4"}, "glm-4.6")
	if !ok || md.Context != 204800 {
		t.Errorf("endpoint match glm-4.6: ok=%v ctx=%d", ok, md.Context)
	}
	// 2. Name fallback: aqp endpoint has no models.dev entry; deepseek-v4-pro
	//    still resolves globally.
	md, ok = cat.lookup([]string{"https://compass.llm.shopee.io/compass-api/v1"}, "deepseek-v4-pro")
	if !ok || md.Context != 1000000 {
		t.Errorf("name fallback deepseek-v4-pro: ok=%v ctx=%d", ok, md.Context)
	}
	// 3. Name fallback returns canonical value even without an endpoint hit
	//    (reseller's 999 must not leak through global lookup).
	md, ok = cat.lookup(nil, "glm-4.6")
	if !ok || md.Context != 204800 {
		t.Errorf("global name lookup should give canonical: ok=%v ctx=%d", ok, md.Context)
	}
	// 4. Unmatched (no endpoint, no global name) → ok=false.
	if _, ok := cat.lookup([]string{"https://chatgpt.com/backend-api/codex"}, "gpt-5.5"); ok {
		t.Error("gpt-5.5 should be unmatched")
	}
	// 5. nil catalog never panics.
	var nilCat *modelsDevCatalog
	if _, ok := nilCat.lookup([]string{"http://x"}, "m"); ok {
		t.Error("nil catalog lookup should return false")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	cat := &modelsDevCatalog{
		FetchedAt:  time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		Etag:       `"abc"`,
		ByName:     map[string]modelsDevModel{"glm-4.6": {Context: 204800, Output: 131072, Input: []string{"text"}, OutMods: []string{"text"}}},
		ByEndpoint: map[string][]string{"https://open.bigmodel.cn/api/paas/v4": {"glm-4.6"}},
	}
	if err := saveCachedCatalog(path, cat); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCachedCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Etag != `"abc"` || loaded.ByName["glm-4.6"].Context != 204800 {
		t.Errorf("round-trip lost data: %+v", loaded)
	}
	// nonexistent → nil catalog, no error
	none, err := loadCachedCatalog(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || none != nil {
		t.Errorf("missing cache should yield (nil,nil); got (%+v,%v)", none, err)
	}
}

// fakeFetch returns a scripted fetch func.
func fakeFetch(status int, body []byte, etag string) catalogFetchFunc {
	return func(endpoint, inEtag string) (int, []byte, string, error) {
		if status == 304 {
			return 304, nil, etag, nil
		}
		return status, body, etag, nil
	}
}

func TestEnsureCatalogFresh_304(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	seed := &modelsDevCatalog{FetchedAt: time.Now().Add(-2 * catalogTTL), Etag: `"old"`,
		ByName: map[string]modelsDevModel{"glm-4.6": {Context: 204800}}, ByEndpoint: map[string][]string{}}
	saveCachedCatalog(path, seed)
	cat, err := ensureCatalogFresh(path, "http://x", fakeFetch(304, nil, `"old"`), false)
	if err != nil {
		t.Fatal(err)
	}
	if cat.ByName["glm-4.6"].Context != 204800 {
		t.Errorf("304 should preserve data: %+v", cat.ByName["glm-4.6"])
	}
	if time.Since(cat.FetchedAt) > 5*time.Second {
		t.Errorf("304 should refresh fetched_at: %v", cat.FetchedAt)
	}
}

func TestEnsureCatalogFresh_200(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	cat, err := ensureCatalogFresh(path, "http://x", fakeFetch(200, []byte(fixtureAPI), `"newetag"`), false)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Etag != `"newetag"` || cat.ByName["glm-4.6"].Context != 204800 {
		t.Errorf("200 should rebuild catalog: etag=%s %+v", cat.Etag, cat.ByName["glm-4.6"])
	}
	loaded, _ := loadCachedCatalog(path)
	if loaded == nil || loaded.Etag != `"newetag"` {
		t.Errorf("200 should persist cache: %+v", loaded)
	}
}

func TestEnsureCatalogFresh_TTLHitNoFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	fresh := &modelsDevCatalog{FetchedAt: time.Now(), Etag: `"e"`,
		ByName: map[string]modelsDevModel{"glm-4.6": {Context: 204800}}, ByEndpoint: map[string][]string{}}
	saveCachedCatalog(path, fresh)
	var called bool
	errFetch := func(endpoint, etag string) (int, []byte, string, error) { called = true; return 0, nil, "", nil }
	cat, err := ensureCatalogFresh(path, "http://x", errFetch, false)
	if err != nil || called {
		t.Errorf("fresh cache should not fetch: err=%v called=%v", err, called)
	}
	if cat.ByName["glm-4.6"].Context != 204800 {
		t.Errorf("fresh cache should return cached data: %+v", cat.ByName["glm-4.6"])
	}
}

func TestEnsureCatalogFresh_ForceBypassesTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	fresh := &modelsDevCatalog{FetchedAt: time.Now(), Etag: `"old"`,
		ByName: map[string]modelsDevModel{"glm-4.6": {Context: 1}}, ByEndpoint: map[string][]string{}}
	saveCachedCatalog(path, fresh)
	cat, err := ensureCatalogFresh(path, "http://x", fakeFetch(200, []byte(fixtureAPI), `"new"`), true)
	if err != nil {
		t.Fatal(err)
	}
	if cat.ByName["glm-4.6"].Context != 204800 {
		t.Errorf("force should re-fetch + rebuild: %+v", cat.ByName["glm-4.6"])
	}
}

func TestEnsureCatalogFresh_FetchErrorFallsBackToStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	stale := &modelsDevCatalog{FetchedAt: time.Now().Add(-2 * catalogTTL), Etag: `"e"`,
		ByName: map[string]modelsDevModel{"glm-4.6": {Context: 204800}}, ByEndpoint: map[string][]string{}}
	saveCachedCatalog(path, stale)
	errFetch := func(endpoint, etag string) (int, []byte, string, error) { return 0, nil, "", os.ErrNotExist }
	cat, err := ensureCatalogFresh(path, "http://x", errFetch, false)
	if err != nil {
		t.Fatalf("fetch error with stale cache should not error: %v", err)
	}
	if cat.ByName["glm-4.6"].Context != 204800 {
		t.Errorf("should fall back to stale cache: %+v", cat.ByName["glm-4.6"])
	}
}

func TestEnsureCatalogFresh_NoCacheNoFetchEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	errFetch := func(endpoint, etag string) (int, []byte, string, error) { return 0, nil, "", os.ErrNotExist }
	cat, err := ensureCatalogFresh(path, "http://x", errFetch, false)
	// Total failure (no cache + unreachable) now surfaces an error so `models
	// pull` doesn't print a false success; the catalog is still a usable empty.
	if err == nil {
		t.Error("no cache + fetch error should return a non-nil error")
	}
	if cat == nil || len(cat.ByName) != 0 {
		t.Errorf("no cache + fetch error → empty (non-nil) catalog, got %+v", cat)
	}
}

// 304 with no cache must not nil-deref (anomalous intermediary response).
func TestEnsureCatalogFresh_304NoCacheNoPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	fetch := func(endpoint, etag string) (int, []byte, string, error) { return 304, nil, `"e"`, nil }
	cat, err := ensureCatalogFresh(path, "http://x", fetch, true)
	if err != nil {
		t.Fatalf("304-no-cache should not error: %v", err)
	}
	if cat == nil || len(cat.ByName) != 0 {
		t.Errorf("304-no-cache → empty catalog, got %+v", cat)
	}
}

func TestModelsDevEndpoint_EnvOverride(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://example.test/api.json")
	if got := modelsDevEndpoint(); got != "http://example.test/api.json" {
		t.Errorf("env override ignored: got %q", got)
	}
}

func TestHydrateModels(t *testing.T) {
	cat := parseModelsDevAPI([]byte(fixtureAPI))
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://open.bigmodel.cn/api/paas/v4",
				Models: []string{"glm-4.6"}}, // config name; metadata now from models.dev
			"aqp":        {OpenAIBaseURL: "https://compass.llm.shopee.io/compass-api/v1"},
			"codex":      {OpenAIBaseURL: "https://chatgpt.com/backend-api/codex"},
			"volcengine": {Models: []string{"doubao-x"}}, // config-only name, no route, no models.dev entry
		},
		Routes: map[string][]RouteTarget{
			"glm-4.6":         {{Provider: "zhipu", Model: "glm-4.6", Priority: 1}},
			"deepseek-v4-pro": {{Provider: "aqp", Model: "deepseek-v4-pro", Priority: 1}},
			"gpt-5.5":         {{Provider: "codex", Model: "gpt-5.5", Priority: 1}},
		},
	}
	meta, src := hydrateModels(cfg, cat)

	// zhipu/glm-4.6: endpoint match → catalog metadata (204800), srcModelsDev
	if pm := meta["zhipu"]["glm-4.6"]; pm.Context != 204800 || pm.Output != 131072 {
		t.Errorf("zhipu/glm-4.6 should be catalog-sourced: %+v", pm)
	}
	if src["zhipu"]["glm-4.6"] != srcModelsDev {
		t.Errorf("zhipu/glm-4.6 source = %v, want srcModelsDev", src["zhipu"]["glm-4.6"])
	}
	// aqp/deepseek-v4-pro: name-fallback match → catalog values + srcModelsDev
	if pm := meta["aqp"]["deepseek-v4-pro"]; pm.Context != 1000000 || pm.Output != 65536 {
		t.Errorf("aqp/deepseek-v4-pro should be catalog-sourced: %+v", pm)
	}
	if src["aqp"]["deepseek-v4-pro"] != srcModelsDev {
		t.Errorf("aqp/deepseek-v4-pro source = %v, want srcModelsDev", src["aqp"]["deepseek-v4-pro"])
	}
	// codex/gpt-5.5: unmatched → defaults + srcDefault
	if pm := meta["codex"]["gpt-5.5"]; pm.Context != 200000 || pm.Output != 16384 {
		t.Errorf("codex/gpt-5.5 should be default: %+v", pm)
	}
	if src["codex"]["gpt-5.5"] != srcDefault {
		t.Errorf("codex/gpt-5.5 source = %v, want srcDefault", src["codex"]["gpt-5.5"])
	}
	// volcengine config-only name present (no route), unmatched → default
	if pm, ok := meta["volcengine"]["doubao-x"]; !ok || pm.Context != 200000 {
		t.Errorf("volcengine/doubao-x should be present + default: %+v ok=%v", pm, ok)
	}
	if src["volcengine"]["doubao-x"] != srcDefault {
		t.Errorf("volcengine/doubao-x source = %v, want srcDefault", src["volcengine"]["doubao-x"])
	}
}
