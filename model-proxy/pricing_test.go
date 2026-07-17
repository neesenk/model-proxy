package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureOR is a tiny OpenRouter /api/v1/models-shaped blob. deepseek-v4-pro is
// listed by its canonical vendor (deepseek) AND a reseller (openrouter) under the
// same bare name, so the dedup test exercises canonical-vendor preference.
const fixtureOR = `{
  "data": [
    {"id": "deepseek/deepseek-v4-pro", "pricing": {"prompt": "0.0000011", "completion": "0.0000028", "input_cache_read": "0.0000001"}},
    {"id": "openrouter/deepseek-v4-pro", "pricing": {"prompt": "0.0000099", "completion": "0.0000099"}},
    {"id": "z-ai/glm-4.6", "pricing": {"prompt": "0.0000009", "completion": "0.0000009"}},
    {"id": "~openai/gpt-5.6-luna", "pricing": {"prompt": "0.000005", "completion": "0.000015"}}
  ]
}`

func TestParsePricingAPI_DedupCanonicalVendor(t *testing.T) {
	cat := parsePricingAPI([]byte(fixtureOR))
	e, ok := cat.ByModel["deepseek-v4-pro"]
	if !ok {
		t.Fatal("deepseek-v4-pro missing")
	}
	// Canonical vendor deepseek wins: prompt 1.1e-6, NOT the reseller's 9.9e-6.
	if e.Prompt != 1.1e-6 || e.Completion != 2.8e-6 {
		t.Errorf("canonical vendor not preferred: %+v", e)
	}
	if len(cat.ByModel) != 3 {
		t.Errorf("dedup count = %d, want 3 (deepseek-v4-pro, glm-4.6, gpt-5.6-luna); got %v",
			len(cat.ByModel), pricingKeys(cat.ByModel))
	}
	// Tilde alias stripped to bare name.
	if _, ok := cat.ByModel["gpt-5.6-luna"]; !ok {
		t.Errorf("tilde-prefixed id should strip to bare name; got %v", pricingKeys(cat.ByModel))
	}
}

func TestPricingLookup(t *testing.T) {
	cat := parsePricingAPI([]byte(fixtureOR))
	if e, ok := cat.lookup("glm-4.6"); !ok || e.Prompt != 0.9e-6 {
		t.Errorf("glm-4.6 lookup: ok=%v prompt=%v", ok, e.Prompt)
	}
	if _, ok := cat.lookup("doubao-seed-1-8"); ok {
		t.Error("doubao-seed-1-8 must be unpriced (not in catalog)")
	}
	var nilCat *pricingCatalog
	if _, ok := nilCat.lookup("m"); ok {
		t.Error("nil catalog lookup should return false")
	}
}

func TestPricingCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing_cache.json")
	cat := &pricingCatalog{
		FetchedAt: time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
		Etag:      `"abc"`,
		ByModel:   map[string]pricingEntry{"glm-4.6": {Prompt: 0.9e-6, Completion: 0.9e-6}},
	}
	if err := saveCachedPricing(path, cat); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCachedPricing(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Etag != `"abc"` || loaded.ByModel["glm-4.6"].Prompt != 0.9e-6 {
		t.Errorf("round-trip lost data: %+v", loaded)
	}
	none, err := loadCachedPricing(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || none != nil {
		t.Errorf("missing cache should yield (nil,nil); got (%+v,%v)", none, err)
	}
}

func TestPricingEndpoint_EnvOverride(t *testing.T) {
	t.Setenv("MP_PRICING_URL", "http://example.test/models")
	if got := pricingEndpoint(); got != "http://example.test/models" {
		t.Errorf("env override ignored: got %q", got)
	}
}

// helpers + keep imports honest
func pricingKeys(m map[string]pricingEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var (
	_ = os.ErrNotExist
)

func pricingFakeFetch(status int, body []byte, etag string) pricingFetchFunc {
	return func(endpoint, inEtag string) (int, []byte, string, error) {
		if status == 304 {
			return 304, nil, etag, nil
		}
		return status, body, etag, nil
	}
}

func TestEnsurePricingFresh_200(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing_cache.json")
	cat, err := ensurePricingFresh(path, "http://x", pricingFakeFetch(200, []byte(fixtureOR), `"new"`), false, pricingTTL)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Etag != `"new"` || cat.ByModel["glm-4.6"].Prompt != 0.9e-6 {
		t.Errorf("200 should rebuild: %+v", cat.ByModel["glm-4.6"])
	}
	loaded, _ := loadCachedPricing(path)
	if loaded == nil || loaded.Etag != `"new"` {
		t.Errorf("200 should persist: %+v", loaded)
	}
}

func TestEnsurePricingFresh_TTLHitNoFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing_cache.json")
	fresh := &pricingCatalog{FetchedAt: time.Now(), Etag: `"e"`,
		ByModel: map[string]pricingEntry{"glm-4.6": {Prompt: 0.9e-6}}}
	saveCachedPricing(path, fresh)
	var called bool
	errFetch := func(endpoint, etag string) (int, []byte, string, error) { called = true; return 0, nil, "", nil }
	cat, err := ensurePricingFresh(path, "http://x", errFetch, false, pricingTTL)
	if err != nil || called {
		t.Errorf("fresh cache should not fetch: err=%v called=%v", err, called)
	}
	if cat.ByModel["glm-4.6"].Prompt != 0.9e-6 {
		t.Errorf("fresh cache should return cached data: %+v", cat.ByModel["glm-4.6"])
	}
}

func TestEnsurePricingFresh_FetchErrorFallsBackToStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing_cache.json")
	stale := &pricingCatalog{FetchedAt: time.Now().Add(-2 * pricingTTL), Etag: `"e"`,
		ByModel: map[string]pricingEntry{"glm-4.6": {Prompt: 0.9e-6}}}
	saveCachedPricing(path, stale)
	errFetch := func(endpoint, etag string) (int, []byte, string, error) { return 0, nil, "", os.ErrNotExist }
	cat, err := ensurePricingFresh(path, "http://x", errFetch, false, pricingTTL)
	if err != nil {
		t.Fatalf("fetch error with stale cache should not error: %v", err)
	}
	if cat.ByModel["glm-4.6"].Prompt != 0.9e-6 {
		t.Errorf("should fall back to stale: %+v", cat.ByModel["glm-4.6"])
	}
}
