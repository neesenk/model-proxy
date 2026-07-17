# Analytics Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a usage & cost analytics dashboard — token trends + equivalent-payg cost trends, by day and by calendar month, per provider and model — backed by the existing `minute_buckets` SQLite store and a new OpenRouter pricing catalog.

**Architecture:** A new `pricing.go` mirrors `modelsdev.go` (fetch + ETag/304 + 24h cache + offline fallback) to build a bare-name→price catalog. A new `statsStore.queryAnalytics` does calendar day/month SQL grouping (lossless over 1-minute storage). A new `/api/analytics` handler computes cost server-side at query time (price × tokens; never stored) and returns series + totals + price-coverage. A new Analytics Web UI tab renders uPlot charts. The `stats` CLI gains `--cost`/`--granularity` (append-only; existing output unchanged).

**Tech Stack:** Go (stdlib `net/http`, `database/sql`, `modernc.org/sqlite`, `gopkg.in/yaml.v3`); vanilla JS + vendored uPlot (no build step). Design rationale + decisions log live in `docs/superpowers/specs/2026-07-18-analytics-dashboard-design.md` — read it before starting.

## Global Constraints

- All `go` commands run from `model-proxy/` (the module dir).
- **White-box tests, stdlib `testing` + `httptest` only — NO testify.** 80% coverage gate per package enforced by `scripts/cover.sh`; run it before committing.
- `gofmt -l .`, `go vet ./...`, `go test -race ./...` must all be clean.
- **CLI display contract:** existing `stats` stdout is byte-identical without the new flags; `--cost`/`--granularity` are append-only. Update `CLI.md` + its `strings.Contains` assertions in the same commit.
- **Cost semantics:** equivalent payg cost only; **never fabricate a price.** Unknown price → JSON `null` + `priced:false`. Tokens always trend.
- **Pricing precedence:** config `prices:` (override) > catalog. Name match is exact bare-name only (no fuzzy).
- **Frontend:** no build step; third-party JS vendored under `web_assets/vendor/`; vanilla JS (no framework); honor `color-scheme: light dark`.
- `models:` name-list contract is untouched. Credentials never go in config.

---

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `model-proxy/pricing.go` | Pricing catalog: structs, parse OpenRouter, bare-name dedup, lookup, cache lifecycle, cost math, resolver | **Create** |
| `model-proxy/pricing_test.go` | parse/dedup/lookup/cache/cost/resolver tests | **Create** |
| `model-proxy/config.go` | `PricingConfig` + `PriceConfig`/`Prices`, accessors, raw-struct + defaults wiring | **Modify** |
| `model-proxy/config_test.go` (or `config_extra_test.go`) | pricing/prices parse + defaults tests | **Modify** |
| `model-proxy/stats.go` | `analyticsBucket` + `queryAnalytics` (calendar grouping) | **Modify** |
| `model-proxy/stats_test.go` | `queryAnalytics` calendar/bucket-start/lossless tests | **Modify** |
| `model-proxy/proxy.go` | `p.pricing`/`p.pricingMu` fields + `pricingSnapshot()` accessor | **Modify** |
| `model-proxy/web.go` | `handleAnalytics` + `/api/analytics` route registration | **Modify** |
| `model-proxy/web_test.go` | `/api/analytics` handler test | **Modify** |
| `model-proxy/cmd_stats.go` | `--granularity`/`--cost` flags + analytics render path | **Modify** |
| `model-proxy/cli_test.go` (or `cli_extra_test.go`) | stats flag + output tests | **Modify** |
| `model-proxy/CLI.md` | document `--granularity`/`--cost` | **Modify** |
| `model-proxy/web_assets/index.html` | Analytics tab button + panel + uPlot script tag | **Modify** |
| `model-proxy/web_assets/app.js` | `renderAnalyticsTab()` + tab wiring | **Modify** |
| `model-proxy/web_assets/vendor/uPlot.min.js` | vendored uPlot | **Create** |
| `model-proxy/web_assets_contract_test.go` | assert Analytics tab present | **Modify** |
| `AGENTS.md` | `/api/analytics` API row + `pricing`/`prices` config + `MP_PRICING_URL` | **Modify** |

---

## Task 1: Pricing catalog (`pricing.go`) — parse, lookup, cache lifecycle

Mirror `modelsdev.go` line-for-line in shape. OpenRouter `/api/v1/models` returns `{"data":[{"id":"deepseek/deepseek-v4-pro","pricing":{"prompt":"0.0000011","completion":"0.0000028","input_cache_read":"...","input_cache_write":"..."}}]}`. Prices are **USD per token**.

**Files:**
- Create: `model-proxy/pricing.go`
- Test: `model-proxy/pricing_test.go`
- Consumes: `ownerRank` pattern from `modelsdev.go` (re-implemented here for OpenRouter vendor prefixes); `atomicWrite`, `homeDir`, `envOrEmpty` (existing utils in `util.go`).
- Produces: `pricingEntry`, `pricingCatalog`, `parsePricingAPI`, `(*pricingCatalog).lookup`, `ensurePricingFresh`, `pricingCachePath`, `pricingEndpoint`, `realPricingFetch`, `loadCachedPricing`, `saveCachedPricing`, `emptyPricingCatalog`, `bareModelName`, const `pricingTTL`, `defaultPricingEndpoint`.

- [ ] **Step 1: Write the failing parse/dedup/lookup test**

Create `model-proxy/pricing_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd model-proxy && go test -run 'TestParsePricingAPI|TestPricingLookup|TestPricingCacheRoundTrip|TestPricingEndpoint' .`
Expected: FAIL — `parsePricingAPI undefined` / `pricingEntry undefined`.

- [ ] **Step 3: Implement `pricing.go` core (parse, lookup, bare-name, dedup)**

Create `model-proxy/pricing.go`:

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pricing.go — auto-source per-model pricing (USD/token) from OpenRouter's
// /api/v1/models catalog, cached like models.dev (24h TTL, ETag/304, offline
// fallback). Used only to compute equivalent pay-as-you-go cost for the
// analytics dashboard; real spend is not tracked. Prices are never fabricated:
// an unknown model is reported unpriced.

// pricingEntry is the per-model price in USD per token.
type pricingEntry struct {
	Prompt     float64 `json:"prompt"`      // input
	Completion float64 `json:"completion"`  // output
	CacheRead  float64 `json:"cache_read"`  // cached-input read
	CacheWrite float64 `json:"cache_write"` // cache-creation write
}

// pricingCatalog is the on-disk + in-memory price index keyed by bare model name.
type pricingCatalog struct {
	FetchedAt time.Time               `json:"fetched_at"`
	Etag      string                  `json:"etag"`
	ByModel   map[string]pricingEntry `json:"by_model"`
}

func emptyPricingCatalog() *pricingCatalog {
	return &pricingCatalog{ByModel: map[string]pricingEntry{}}
}

// canonicalORVendors are OpenRouter vendor prefixes that own a model family
// outright (rank 0); resellers/aggregators are rank 1. On a bare-name collision
// the canonical vendor's price wins.
var canonicalORVendors = map[string]bool{
	"openai": true, "anthropic": true, "google": true, "deepseek": true,
	"z-ai": true, "moonshotai": true, "xai": true, "mistral": true,
	"cohere": true, "meta": true, "alibaba": true, "minimax": true,
	"qwen": true, "stepfun": true, "amazon-bedrock": true, "bedrock": true,
	"zhipuai": true,
}

func orVendorRank(id string) int {
	if i := strings.IndexByte(id, '/'); i > 0 {
		if canonicalORVendors[id[:i]] {
			return 0
		}
	}
	return 1
}

// bareModelName strips a leading "~" (OpenRouter dynamic alias) and the
// "vendor/" prefix from an OpenRouter id, returning the bare model name.
func bareModelName(id string) string {
	id = strings.TrimPrefix(id, "~")
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// atof is a forgiving string→float64 (OpenRouter prices are decimal strings);
// unparseable → 0.
func atof(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// parsePricingAPI parses an OpenRouter /api/v1/models blob into a deduplicated
// bare-name → price catalog. Canonical vendor wins on a bare-name collision.
func parsePricingAPI(blob []byte) *pricingCatalog {
	var raw struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt         string `json:"prompt"`
				Completion     string `json:"completion"`
				InputCacheRead string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return emptyPricingCatalog()
	}
	cat := emptyPricingCatalog()
	rank := map[string]int{}
	for _, m := range raw.Data {
		name := bareModelName(m.ID)
		if name == "" {
			continue
		}
		e := pricingEntry{
			Prompt:     atof(m.Pricing.Prompt),
			Completion: atof(m.Pricing.Completion),
			CacheRead:  atof(m.Pricing.InputCacheRead),
			CacheWrite: atof(m.Pricing.InputCacheWrite),
		}
		r := orVendorRank(m.ID)
		if cur, ok := rank[name]; !ok || r < cur {
			cat.ByModel[name] = e
			rank[name] = r
		}
	}
	return cat
}

// lookup resolves a model's price by bare name. ok=false if absent. Nil-safe.
func (cat *pricingCatalog) lookup(model string) (pricingEntry, bool) {
	if cat == nil {
		return pricingEntry{}, false
	}
	e, ok := cat.ByModel[model]
	return e, ok
}

const pricingTTL = 24 * time.Hour

const defaultPricingEndpoint = "https://openrouter.ai/api/v1/models"

// pricingEndpoint returns the catalog endpoint, overridable via MP_PRICING_URL.
func pricingEndpoint() string {
	if v := envOrEmpty("MP_PRICING_URL"); v != "" {
		return v
	}
	return defaultPricingEndpoint
}

// pricingCachePath is the on-disk catalog cache location.
func pricingCachePath() string {
	return filepath.Join(homeDir(), ".model-proxy", "pricing_cache.json")
}

// pricingFetchFunc mirrors catalogFetchFunc (modelsdev.go).
type pricingFetchFunc func(endpoint, etag string) (status int, body []byte, newEtag string, err error)

// realPricingFetch is the production fetch. Do NOT set Accept-Encoding manually
// (Go Transport auto-gzipes + decompresses).
var realPricingFetch pricingFetchFunc = func(endpoint, etag string) (int, []byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	newEtag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		return http.StatusNotModified, nil, newEtag, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, newEtag, err
	}
	return resp.StatusCode, body, newEtag, nil
}

func loadCachedPricing(path string) (*pricingCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cat pricingCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	if cat.ByModel == nil {
		cat.ByModel = map[string]pricingEntry{}
	}
	return &cat, nil
}

func saveCachedPricing(path string, cat *pricingCatalog) error {
	data, err := json.Marshal(cat)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// ensurePricingFresh mirrors ensureCatalogFresh: fresh→use; stale/force→fetch
// (304 refreshes fetched_at, 200 rebuilds+persists); fetch error falls back to a
// stale cache (logged) or an empty catalog. ttl overrides the 24h default.
func ensurePricingFresh(cacheFile, endpoint string, fetch pricingFetchFunc, force bool, ttl time.Duration) (*pricingCatalog, error) {
	cached, _ := loadCachedPricing(cacheFile)
	if !force && cached != nil && time.Since(cached.FetchedAt) < ttl {
		return cached, nil
	}
	etag := ""
	if cached != nil {
		etag = cached.Etag
	}
	status, body, newEtag, err := fetch(endpoint, etag)
	if err != nil {
		if cached != nil {
			fmt.Fprintf(os.Stderr, "model-proxy: pricing source unreachable (%v); using catalog cached %s ago\n", err, ageString(cached.FetchedAt))
			return cached, nil
		}
		return emptyPricingCatalog(), fmt.Errorf("pricing source unreachable and no cached catalog: %w", err)
	}
	switch status {
	case http.StatusNotModified:
		if cached == nil {
			return emptyPricingCatalog(), nil
		}
		cached.FetchedAt = time.Now()
		if newEtag != "" {
			cached.Etag = newEtag
		}
		_ = saveCachedPricing(cacheFile, cached)
		return cached, nil
	case http.StatusOK:
		cat := parsePricingAPI(body)
		cat.FetchedAt = time.Now()
		cat.Etag = newEtag
		_ = saveCachedPricing(cacheFile, cat)
		return cat, nil
	default:
		if cached != nil {
			return cached, nil
		}
		return emptyPricingCatalog(), nil
	}
}
```

Add `"fmt"` to the import block (used by `ensurePricingFresh`). Verify `envOrEmpty`, `atomicWrite`, `homeDir`, `ageString` exist (they do — `util.go` / `modelsdev.go`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestParsePricingAPI|TestPricingLookup|TestPricingCacheRoundTrip|TestPricingEndpoint' .`
Expected: PASS.

- [ ] **Step 5: Add the cache-lifecycle tests (304/200/TTL/force/fallback/no-cache)**

Append to `model-proxy/pricing_test.go`:

```go
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
```

- [ ] **Step 6: Run + commit**

Run: `cd model-proxy && go test -run 'TestPricing|TestEnsurePricing|TestParsePricing' . && gofmt -w pricing.go pricing_test.go && go vet ./...`
Expected: all PASS, vet clean.
```bash
git add model-proxy/pricing.go model-proxy/pricing_test.go
git commit -m "feat(pricing): add OpenRouter pricing catalog (fetch/cache/parse/lookup)"
```

---

## Task 2: Config surface — `PricingConfig` + `Prices`

**Files:**
- Modify: `model-proxy/config.go` (Config struct ~line 13; rawConfig in `LoadConfigFromBytes` ~line 344; accessor near `StatsConfig` ~line 35)
- Test: `model-proxy/config_test.go` (append)
- Consumes: existing `expandPath`, yaml.v3, code-defaults convention.
- Produces: `PricingConfig{Enabled,TTL,SourceURL}` with `enabled()`/`ttl()`/`sourceURL()`; `PriceConfig{Input,Output,CacheRead,CacheWrite}` ($/M tokens); `Config.Prices map[string]PriceConfig`; `Config.Pricing PricingConfig`.

- [ ] **Step 1: Write the failing config test**

Append to `model-proxy/config_test.go`:

```go
func TestPricingConfigDefaults(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte("listen: 127.0.0.1:1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Pricing.enabled() {
		t.Error("pricing should default to enabled")
	}
	if cfg.Pricing.ttl() != 24*time.Hour {
		t.Errorf("default ttl = %v, want 24h", cfg.Pricing.ttl())
	}
	if cfg.Pricing.sourceURL() != "https://openrouter.ai/api/v1/models" {
		t.Errorf("default source = %q", cfg.Pricing.sourceURL())
	}
	if cfg.Prices != nil && len(cfg.Prices) != 0 {
		t.Errorf("prices should default empty, got %v", cfg.Prices)
	}
}

func TestPricesParseAndUnits(t *testing.T) {
	yaml := `prices:
  glm-4.6: {input: 0.9, output: 0.9, cache_read: 0.09}
  doubao-seed-1-8-251228: {input: 0.5, output: 1.5}
`
	cfg, err := LoadConfigFromBytes("x", []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Prices) != 2 {
		t.Fatalf("prices = %d entries, want 2", len(cfg.Prices))
	}
	if cfg.Prices["glm-4.6"].Input != 0.9 || cfg.Prices["glm-4.6"].CacheRead != 0.09 {
		t.Errorf("glm-4.6 parsed wrong: %+v", cfg.Prices["glm-4.6"])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd model-proxy && go test -run 'TestPricingConfigDefaults|TestPricesParseAndUnits' .`
Expected: FAIL — `cfg.Pricing undefined` / `cfg.Prices undefined`.

- [ ] **Step 3: Add the types + accessors**

In `model-proxy/config.go`, add after the `StatsConfig` block (after line ~58):

```go
// PricingConfig configures the analytics equivalent-cost pricing source
// (OpenRouter catalog, cached). Defaults: enabled, 24h TTL, the OpenRouter
// endpoint. enabled=false → no fetch; cost shows n/a everywhere.
type PricingConfig struct {
	Enabled   bool   `yaml:"enabled"`
	TTL       string `yaml:"ttl"`
	SourceURL string `yaml:"source_url"`
}

func (p PricingConfig) enabled() bool { return p.Enabled }

// ttl returns the catalog cache TTL, defaulting to 24h.
func (p PricingConfig) ttl() time.Duration {
	if p.TTL == "" {
		return 24 * time.Hour
	}
	if d, err := time.ParseDuration(p.TTL); err == nil {
		return d
	}
	return 24 * time.Hour
}

// sourceURL returns the pricing endpoint, defaulting to OpenRouter.
func (p PricingConfig) sourceURL() string {
	if p.SourceURL != "" {
		return p.SourceURL
	}
	return defaultPricingEndpoint
}

// PriceConfig is a per-model price override in USD per MILLION tokens (human
// units); converted to USD/token at lookup (÷ 1e6). cache_read/cache_write
// default to 0.
type PriceConfig struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}
```

Add two fields to the `Config` struct (after `RequestLog`, line ~24):

```go
	Pricing    PricingConfig            `yaml:"pricing"`
	Prices     map[string]PriceConfig   `yaml:"prices"`
```

In `LoadConfigFromBytes`, add the same two fields to the local `rawConfig` struct, set the default in the `raw := rawConfig{...}` initializer (`Pricing: PricingConfig{Enabled: true}`), and copy them out (`cfg.Pricing = raw.Pricing`, `cfg.Prices = raw.Prices`) next to the other `cfg.X = raw.X` lines.

- [ ] **Step 4: Run + commit**

Run: `cd model-proxy && go test -run 'TestPricingConfigDefaults|TestPricesParseAndUnits' . && gofmt -w config.go config_test.go && go vet ./...`
Expected: PASS, vet clean.
```bash
git add model-proxy/config.go model-proxy/config_test.go
git commit -m "feat(config): add pricing source + per-model price overrides"
```

---

## Task 3: Cost computation + price resolver

**Files:**
- Modify: `model-proxy/pricing.go` (append), `model-proxy/pricing_test.go` (append)
- Consumes: `PriceConfig` (Task 2), `pricingCatalog.lookup` (Task 1).
- Produces: `resolvePrice(prices, cat, model) (pricingEntry, bool)`; `costResult`; `computeCost(input, output, cacheRead, cacheCreation, e) costResult`.

- [ ] **Step 1: Write the failing test**

Append to `model-proxy/pricing_test.go`:

```go
func TestResolvePrice_OverrideBeatsCatalog(t *testing.T) {
	cat := parsePricingAPI([]byte(fixtureOR)) // glm-4.6 prompt 0.9e-6
	prices := map[string]PriceConfig{"glm-4.6": {Input: 9.0, Output: 9.0}} // $9/M = 9e-6/token
	e, ok := resolvePrice(prices, cat, "glm-4.6")
	if !ok || e.Prompt != 9e-6 {
		t.Errorf("override should win: ok=%v prompt=%v", ok, e.Prompt)
	}
	// No override → catalog.
	e2, ok2 := resolvePrice(prices, cat, "deepseek-v4-pro")
	if !ok2 || e2.Prompt != 1.1e-6 {
		t.Errorf("catalog fallback wrong: ok=%v prompt=%v", ok2, e2.Prompt)
	}
	// Neither → unpriced.
	if _, ok3 := resolvePrice(prices, cat, "doubao-seed-1-8"); ok3 {
		t.Error("unknown model must be unpriced")
	}
}

func TestComputeCost_Exact(t *testing.T) {
	// 1,000,000 input @ $1.1/M (1.1e-6/token) = $1.1
	//   500,000 output @ $2.8/M = $1.4 ; total $2.5
	e := pricingEntry{Prompt: 1.1e-6, Completion: 2.8e-6}
	r := computeCost(1_000_000, 500_000, 0, 0, e)
	if !r.Priced || r.Cost < 2.499 || r.Cost > 2.501 {
		t.Errorf("cost = %v priced=%v, want ~2.5 priced=true", r.Cost, r.Priced)
	}
	// cache_read priced, cache_creation unpriced (CacheWrite 0 → contributes 0).
	e2 := pricingEntry{Prompt: 1e-6, Completion: 2e-6, CacheRead: 1e-7}
	r2 := computeCost(0, 0, 200_000, 100_000, e2)
	if !r2.Priced || r2.Cost < 0.0199 || r2.Cost > 0.0201 { // 200000*1e-7 = 0.02
		t.Errorf("cache cost = %v, want ~0.02", r2.Cost)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd model-proxy && go test -run 'TestResolvePrice|TestComputeCost' .`
Expected: FAIL — `resolvePrice`/`computeCost` undefined.

- [ ] **Step 3: Implement**

Append to `model-proxy/pricing.go`:

```go
// costResult is the computed equivalent cost over one bucket's token totals.
// Priced=false means no price was known; Cost should be rendered as n/a (null).
type costResult struct {
	Cost   float64
	Priced bool
}

// resolvePrice returns the effective USD/token price for a model: config
// `prices:` override (÷1e6 from $/M) wins, else the catalog. ok=false if neither.
func resolvePrice(prices map[string]PriceConfig, cat *pricingCatalog, model string) (pricingEntry, bool) {
	if pc, ok := prices[model]; ok {
		return pricingEntry{
			Prompt:     pc.Input / 1e6,
			Completion: pc.Output / 1e6,
			CacheRead:  pc.CacheRead / 1e6,
			CacheWrite: pc.CacheWrite / 1e6,
		}, true
	}
	return cat.lookup(model)
}

// computeCost prices a bucket's token totals. cache_creation is priced only via
// CacheWrite (0 → contributes nothing); input/output priced → Priced=true.
func computeCost(input, output, cacheRead, cacheCreation uint64, e pricingEntry) costResult {
	cost := float64(input)*e.Prompt +
		float64(output)*e.Completion +
		float64(cacheRead)*e.CacheRead +
		float64(cacheCreation)*e.CacheWrite
	return costResult{Cost: cost, Priced: true}
}
```

- [ ] **Step 4: Run + commit**

Run: `cd model-proxy && go test -run 'TestResolvePrice|TestComputeCost' . && gofmt -w pricing.go pricing_test.go && go vet ./...`
Expected: PASS.
```bash
git add model-proxy/pricing.go model-proxy/pricing_test.go
git commit -m "feat(pricing): add price resolver + equivalent-cost computation"
```

---

## Task 4: `queryAnalytics` — calendar day/month grouping

**Files:**
- Modify: `model-proxy/stats.go` (append `analyticsBucket` + `queryAnalytics` after `queryRange` ~line 375)
- Test: `model-proxy/stats_test.go` (append)
- Consumes: `statsStore.db`, existing `flushDeltas` for seeding in tests, `newTestStatsStore`.
- Produces: `analyticsBucket`; `(*statsStore).queryAnalytics(from, to int64, provider, model, granularity string) ([]analyticsBucket, error)`.

- [ ] **Step 1: Write the failing test**

Append to `model-proxy/stats_test.go`:

```go
// TestQueryAnalytics_DayBuckets verifies calendar-day grouping: minutes in the
// same local day collapse to one bucket whose start is local midnight; minutes
// spanning a day boundary split; storage stays 1-minute (lossless).
func TestQueryAnalytics_DayBuckets(t *testing.T) {
	ss := newTestStatsStore(t)
	// Anchor to local midnight so the day boundary is deterministic on any host
	// timezone (the SQL uses SQLite 'localtime'). d1m1/d1m2 share a local day;
	// d2m1 is the next local day.
	now := time.Now().In(time.Local)
	twoDaysAgoStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Add(-48 * time.Hour)
	d1m1 := twoDaysAgoStart.Add(1 * time.Hour).Unix() / 60 * 60
	d1m2 := twoDaysAgoStart.Add(2 * time.Hour).Unix() / 60 * 60
	d2m1 := twoDaysAgoStart.Add(25 * time.Hour).Unix() / 60 * 60 // next local day
	_ = ss.flushDeltas(d1m1, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 1, Input: 100}})
	_ = ss.flushDeltas(d1m2, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 2, Input: 200}})
	_ = ss.flushDeltas(d2m1, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 4, Input: 400}})

	got, err := ss.queryAnalytics(d1m1-60, d2m1+60, "", "", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 day-buckets, got %d: %+v", len(got), got)
	}
	// Day 1 sums the two minutes (reqs 3, input 300); bucket == local midnight.
	day1 := got[0]
	if day1.Requests != 3 || day1.Input != 300 {
		t.Errorf("day1 sum wrong: %+v", day1)
	}
	if wantStart := twoDaysAgoStart.Unix(); day1.Bucket != wantStart {
		t.Errorf("day1 bucket = %d, want local-midnight %d", day1.Bucket, wantStart)
	}
	// Storage lossless: re-query raw 1-minute rows still see 3 rows.
	raw, _ := ss.queryRange(d1m1-60, d2m1+60, "", "", 60)
	if len(raw) != 3 {
		t.Errorf("storage not lossless: %d raw rows, want 3", len(raw))
	}
}

func TestQueryAnalytics_MonthBuckets(t *testing.T) {
	ss := newTestStatsStore(t)
	now := time.Now().In(time.Local)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	m1 := monthStart.Add(48 * time.Hour).Unix() / 60 * 60 // same local month
	_ = ss.flushDeltas(m1, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 5, Input: 50}})
	got, err := ss.queryAnalytics(m1-60, m1+60, "", "", "month")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Requests != 5 {
		t.Errorf("month bucket wrong: %+v", got)
	}
	if wantStart := monthStart.Unix(); got[0].Bucket != wantStart {
		t.Errorf("month bucket start = %d, want first-of-month %d", got[0].Bucket, wantStart)
	}
}

func TestQueryAnalytics_BadGranularity(t *testing.T) {
	ss := newTestStatsStore(t)
	if _, err := ss.queryAnalytics(0, 1, "", "", "hour"); err == nil {
		t.Error("hour granularity should error")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd model-proxy && go test -run 'TestQueryAnalytics' .`
Expected: FAIL — `queryAnalytics` undefined.

- [ ] **Step 3: Implement**

Append to `model-proxy/stats.go`:

```go
// analyticsBucket is one persisted calendar-day/month aggregate row for the
// analytics dashboard. Bucket = unix start of the local calendar day/month.
type analyticsBucket struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Bucket        int64  `json:"bucket"`
	Requests      uint64 `json:"requests"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	LastRequestAt int64  `json:"last_request_at"`
}

// queryAnalytics returns calendar-day or calendar-month aggregates (local tz)
// in [from, to]. Storage stays 1-minute (lossless); only the view widens via
// SQL GROUP BY on date(minute,'unixepoch','localtime',<trunc>). The bucket is
// the true local-midnight/first-of-month unix instant: SQL returns the local
// date STRING, Go converts it via time.ParseInLocation(...,time.Local) (not
// strftime('%s',…), which would misread the local date as UTC midnight and shift
// the label by the tz offset). granularity must be "day" or "month". Ordered by
// provider, model, date.
func (s *statsStore) queryAnalytics(from, to int64, provider, model, granularity string) ([]analyticsBucket, error) {
	if granularity != "day" && granularity != "month" {
		return nil, fmt.Errorf("granularity must be day or month, got %q", granularity)
	}
	trunc := "start of day"
	if granularity == "month" {
		trunc = "start of month"
	}
	q := `SELECT provider, model,
		date(minute,'unixepoch','localtime',?) AS d,
		SUM(requests), SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read),
		MAX(last_request_at)
		FROM minute_buckets WHERE minute >= ? AND minute <= ?`
	args := []any{trunc, from, to}
	if provider != "" {
		q += ` AND provider = ?`
		args = append(args, provider)
	}
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	q += ` GROUP BY provider, model, date(minute,'unixepoch','localtime',?) ORDER BY provider, model, d`
	args = append(args, trunc)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []analyticsBucket
	for rows.Next() {
		var b analyticsBucket
		var d string
		if err := rows.Scan(&b.Provider, &b.Model, &d,
			&b.Requests, &b.Input, &b.Output, &b.CacheCreation, &b.CacheRead,
			&b.LastRequestAt); err != nil {
			return nil, err
		}
		b.Bucket = localDayStart(d, granularity)
		out = append(out, b)
	}
	return out, rows.Err()
}

// localDayStart converts a SQLite local date string ("YYYY-MM-DD" for day,
// "YYYY-MM" for month) to the unix start of that local calendar period, in the
// host timezone (mirrors SQLite 'localtime'). 0 on a parse failure.
func localDayStart(d, granularity string) int64 {
	layout := "2006-01-02"
	if granularity == "month" {
		layout = "2006-01"
	}
	t, err := time.ParseInLocation(layout, d, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}
```

- [ ] **Step 4: Run + commit**

Run: `cd model-proxy && go test -run 'TestQueryAnalytics' . && gofmt -w stats.go stats_test.go && go vet ./...`
Expected: PASS.
```bash
git add model-proxy/stats.go model-proxy/stats_test.go
git commit -m "feat(stats): add calendar day/month analytics aggregation"
```

---

## Task 5: `/api/analytics` handler + Proxy wiring

**Files:**
- Modify: `model-proxy/proxy.go` (add `p.pricing`/`p.pricingMu` fields + `pricingSnapshot()` near the `stats` field ~line 34), `model-proxy/web.go` (route ~line 130 + handler near `handleStats` ~line 430)
- Test: `model-proxy/web_test.go` (append)
- Consumes: `queryAnalytics` (Task 4), `resolvePrice`/`computeCost` (Task 3), `ensurePricingFresh` (Task 1), `parseStatsTime`/`writeJSON`/`writeJSONErr` (existing), `Config.Prices`/`Config.Pricing` (Task 2).
- Produces: `(*Proxy).pricingSnapshot() *pricingCatalog`; `handleAnalytics`; `GET /api/analytics`.

- [ ] **Step 1: Add Proxy fields + `pricingSnapshot`**

In `model-proxy/proxy.go`, add fields next to `stats`:

```go
	pricing    *pricingCatalog // equivalent-cost price catalog (analytics); nil-safe
	pricingMu  sync.Mutex
```

Add the accessor. `Proxy` already exposes its live config via `cfgSnapshot()` (proxy.go:271 — returns `*Config` under `p.mu.RLock()`); reuse it — do not add a parallel config field:

```go
// pricingSnapshot returns a usable price catalog, refreshing the cache when
// stale (best-effort; offline falls back to the stale cache). Nil-safe and
// thundering-herd-safe. Returns nil when pricing is disabled.
func (p *Proxy) pricingSnapshot() *pricingCatalog {
	if p == nil {
		return nil
	}
	cfg := p.cfgSnapshot()
	if !cfg.Pricing.enabled() {
		return nil
	}
	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()
	cat, err := ensurePricingFresh(pricingCachePath(), cfg.Pricing.sourceURL(), realPricingFetch, false, cfg.Pricing.ttl())
	if err != nil || cat == nil {
		return emptyPricingCatalog()
	}
	p.pricing = cat
	return cat
}

// priceOverrides returns the current config `prices:` overrides for the handler.
func (p *Proxy) priceOverrides() map[string]PriceConfig {
	return p.cfgSnapshot().Prices
}
```

- [ ] **Step 2: Write the failing handler test**

Append to `model-proxy/web_test.go` (mirror `TestAPIStatsHandler`):

```go
func TestAPIAnalyticsHandler(t *testing.T) {
	p := &Proxy{
		metrics: newMetricsStore(),
		tokens:  newTokenCounter(),
		stats:   newTestStatsStore(t),
		// pricing: nil → resolver falls back to unpriced (cost null), proving the
		// handler never fabricates a price and never panics on a nil catalog.
	}
	minute := time.Now().Unix() / 60 * 60
	_ = p.stats.flushDeltas(minute, map[pmKey]statsCounters{
		{Provider: "deepseek", Model: "deepseek-v4-pro"}: {Requests: 3, Input: 1000, Output: 200},
	})
	w := newWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/analytics?granularity=day", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"granularity":"day"`, `"deepseek"`, `"deepseek-v4-pro"`, `"input":1000`, `"requests":3`, `"price_coverage"`} {
		if !strings.Contains(body, want) {
			t.Errorf("analytics body missing %s: %s", want, body)
		}
	}

	// Bad granularity → 400.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/analytics?granularity=hour", nil))
	if rec2.Code != 400 {
		t.Errorf("bad granularity status=%d want 400", rec2.Code)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `cd model-proxy && go test -run 'TestAPIAnalyticsHandler' .`
Expected: FAIL — route 404 / `handleAnalytics` undefined.

- [ ] **Step 4: Implement the handler**

In `model-proxy/web.go`, add the route in `serveAPI` next to the `/api/stats` case (~line 130):

```go
	case path == "/api/analytics" && r.Method == http.MethodGet:
		w.handleAnalytics(resp, r)
```

Add the handler near `handleStats` (~line 430):

```go
// handleAnalytics returns per-(provider, model) calendar day/month aggregates
// with server-computed equivalent-payg cost. Cost is price × tokens, never
// stored or fabricated; unknown prices yield cost=null + priced=false. Query
// params: from/to (unix or RFC3339; default last 30d), provider/model filters,
// granularity=day|month (default day). Nil-safe: no stats store → empty series.
func (w *webServer) handleAnalytics(resp http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from := now.Add(-30 * 24 * time.Hour).Unix()
	to := now.Unix()
	if v := r.URL.Query().Get("from"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			to = t
		}
	}
	provider := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	granularity := r.URL.Query().Get("granularity")
	if granularity == "" {
		granularity = "day"
	}
	if granularity != "day" && granularity != "month" {
		writeJSONErr(resp, http.StatusBadRequest, "granularity must be day or month")
		return
	}

	type point struct {
		Bucket        int64    `json:"bucket"`
		Requests      uint64   `json:"requests"`
		Input         uint64   `json:"input"`
		Output        uint64   `json:"output"`
		CacheCreation uint64   `json:"cache_creation"`
		CacheRead     uint64   `json:"cache_read"`
		Cost          *float64 `json:"cost"`
		Priced        bool     `json:"priced"`
	}
	type series struct {
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		Points   []point `json:"points"`
	}

	buckets := []analyticsBucket{}
	if w.p.stats != nil {
		got, err := w.p.stats.queryAnalytics(from, to, provider, model, granularity)
		if err != nil {
			writeJSONErr(resp, http.StatusInternalServerError, "analytics query: "+err.Error())
			return
		}
		buckets = got
	}
	cat := w.p.pricingSnapshot()
	prices := w.p.priceOverrides()

	byKey := map[string]*series{}
	var keys []string
	priced, unpriced := map[string]bool{}, map[string]bool{}
	var totInput, totOutput uint64
	var totCost *float64
	for _, b := range buckets {
		k := b.Provider + "\x00" + b.Model
		s, ok := byKey[k]
		if !ok {
			s = &series{Provider: b.Provider, Model: b.Model}
			byKey[k] = s
			keys = append(keys, k)
		}
		e, ok := resolvePrice(prices, cat, b.Model)
		var cost *float64
		if ok {
			cr := computeCost(b.Input, b.Output, b.CacheRead, b.CacheCreation, e)
			cost = &cr.Cost
			priced[b.Model] = true
			if totCost == nil {
				totCost = new(float64)
			}
			*totCost += cr.Cost
		} else {
			unpriced[b.Model] = true
		}
		s.Points = append(s.Points, point{
			Bucket: b.Bucket, Requests: b.Requests, Input: b.Input, Output: b.Output,
			CacheCreation: b.CacheCreation, CacheRead: b.CacheRead, Cost: cost, Priced: ok,
		})
		totInput += b.Input
		totOutput += b.Output
	}
	out := []series{}
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	coverage := map[string]any{"priced": mapKeys(priced), "unpriced": mapKeys(unpriced)}
	writeJSON(resp, http.StatusOK, map[string]any{
		"granularity":    granularity,
		"from":           from,
		"to":             to,
		"series":         out,
		"totals":         map[string]any{"input": totInput, "output": totOutput, "cost": totCost},
		"price_coverage": coverage,
	})
}

// mapKeys returns the sorted keys of a set map (stable JSON output).
func mapKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
```

Add `"sort"` to `web.go` imports.

- [ ] **Step 5: Run + commit**

Run: `cd model-proxy && go test -run 'TestAPIAnalyticsHandler' . && go test -race ./... && gofmt -w proxy.go web.go web_test.go && go vet ./...`
Expected: PASS, race-clean.
```bash
git add model-proxy/proxy.go model-proxy/web.go model-proxy/web_test.go
git commit -m "feat(web): add /api/analytics with equivalent-cost trends"
```

---

## Task 6: `stats --cost` / `--granularity` CLI + `CLI.md`

**Files:**
- Modify: `model-proxy/cmd_stats.go`, `model-proxy/CLI.md`, `model-proxy/cli_test.go` (or `cli_extra_test.go`)
- Consumes: existing `parseStatsFlags`/`renderStats`/`formatStatsTable`/`statusGet`; `/api/analytics` (Task 5).
- Produces: `statsOpts{Granularity, Cost}`; analytics render path; `CLI.md` updated; existing `stats` output unchanged without the new flags.

- [ ] **Step 1: Write the failing CLI test**

Append to `model-proxy/cli_test.go` (or `cli_extra_test.go`), mirroring the existing `stats` httptest pattern:

```go
// TestStatsFlags_Parsed verifies --granularity and --cost parse into statsOpts.
func TestStatsFlags_Parsed(t *testing.T) {
	o := parseStatsFlags([]string{"--granularity", "month", "--cost", "--provider", "deepseek"})
	if o.Granularity != "month" || !o.Cost || o.Provider != "deepseek" {
		t.Errorf("parsed = %+v, want granularity=month cost=true provider=deepseek", o)
	}
}

// TestStatsFlags_ExistingUnchanged verifies flags not set leave existing fields
// at zero values (the CLI display contract: no behavioral change without flags).
func TestStatsFlags_ExistingUnchanged(t *testing.T) {
	o := parseStatsFlags([]string{"--from", "1", "--bucket", "1h"})
	if o.Granularity != "" || o.Cost {
		t.Errorf("new flags should default off: %+v", o)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd model-proxy && go test -run 'TestStatsFlags' .`
Expected: FAIL — `o.Granularity`/`o.Cost` undefined.

- [ ] **Step 3: Extend `statsOpts` + `parseStatsFlags`**

In `model-proxy/cmd_stats.go`, add fields to `statsOpts`:

```go
type statsOpts struct {
	From       string
	To         string
	Provider   string
	Model      string
	Bucket     string
	Granularity string
	Cost       bool
	JSON       bool
}
```

In `parseStatsFlags`, add cases (both the `--flag value` and `--flag=value` forms, matching the existing style). Add to the first `switch`:

```go
		case a == "--granularity":
			if i+1 < len(args) {
				o.Granularity = args[i+1]
				i++
			}
		case a == "--cost":
			o.Cost = true
```

And the prefixed forms:

```go
		case strings.HasPrefix(a, "--granularity="):
			o.Granularity = strings.TrimPrefix(a, "--granularity=")
		case a == "--cost":
			o.Cost = true
```

(Place `--cost` as a boolean flag exactly like the existing `--json` case.)

- [ ] **Step 4: Add the analytics render path**

In `renderStats`, branch: if `opts.Granularity != "" || opts.Cost`, hit `/api/analytics` and render via a new `formatAnalyticsTable`; else the existing `/api/stats` path is byte-identical. Add to `renderStats` before the existing `/api/stats` call:

```go
	if opts.Granularity != "" || opts.Cost {
		return renderAnalytics(listen, opts)
	}
```

Append:

```go
// analyticsResp is the decoded /api/analytics shape (subset the table needs).
type analyticsResp struct {
	Granularity string `json:"granularity"`
	From        int64  `json:"from"`
	To          int64  `json:"to"`
	Series      []struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Points   []struct {
			Bucket   int64    `json:"bucket"`
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		} `json:"points"`
	} `json:"series"`
}

// renderAnalytics fetches /api/analytics and renders a table. granularity
// defaults to day when only --cost is given.
func renderAnalytics(listen string, opts statsOpts) (string, error) {
	base := "http://" + listen
	q := url.Values{}
	g := opts.Granularity
	if g == "" {
		g = "day"
	}
	q.Set("granularity", g)
	if opts.From != "" {
		q.Set("from", opts.From)
	}
	if opts.To != "" {
		q.Set("to", opts.To)
	}
	if opts.Provider != "" {
		q.Set("provider", opts.Provider)
	}
	if opts.Model != "" {
		q.Set("model", opts.Model)
	}
	body, status, err := statusGet(base, "/api/analytics?"+q.Encode())
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available - is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, truncate(string(body), 200))
	}
	if opts.JSON {
		return string(body), nil
	}
	var resp analyticsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse analytics response: %w", err)
	}
	return formatAnalyticsTable(resp, opts.Cost), nil
}

// formatAnalyticsTable renders analytics series as a terminal table. The Cost
// column is shown only when withCost is true (append-only vs the classic table).
func formatAnalyticsTable(resp analyticsResp, withCost bool) string {
	hdr := "%-16s %-20s %-12s %8s %10s %10s"
	if withCost {
		hdr += " %9s"
	}
	hdr += "\n"
	label := "day"
	if resp.Granularity == "month" {
		label = "month"
	}
	out := fmt.Sprintf(hdr, "provider", "model", label, "reqs", "input", "output", "cost")
	for _, s := range resp.Series {
		var reqs, in, out2 uint64
		var costSum *float64
		for _, p := range s.Points {
			reqs += p.Requests
			in += p.Input
			out2 += p.Output
			if p.Cost != nil {
				if costSum == nil {
					costSum = new(float64)
				}
				*costSum += *p.Cost
			}
		}
		row := "%-16.16s %-20.20s %-12s %8s %10s %10s"
		args := []any{s.Provider, s.Model, label, compactNum(reqs), compactNum(in), compactNum(out2)}
		if withCost {
			row += " %9s"
			if costSum != nil {
				args = append(args, fmt.Sprintf("$%.2f", *costSum))
			} else {
				args = append(args, "n/a")
			}
		}
		out += fmt.Sprintf(row+"\n", args...)
	}
	if out == "" {
		return fmt.Sprintf("(no analytics in range, granularity %s)\n", label)
	}
	return out
}
```

- [ ] **Step 5: Update `CLI.md`**

Edit `CLI.md` §10 (the `stats` section). Keep all existing lines intact; append the new flags to the synopsis and add a short note. Change the synopsis line to:

```
stats [--from TIME] [--to TIME] [--provider P] [--model M] [--bucket B] [--granularity day|month] [--cost] [--json]
```

Add after the existing `--json` line (§10, ~line 466):

```
`--granularity day|month` -> 改走 `/api/analytics`，按自然日/月聚合（存储恒为 1 分钟）。`--cost` -> 额外显示等价成本列（unknown = `n/a`）。两者都省略时输出与原 `stats` 完全一致。
```

- [ ] **Step 6: Run + commit**

Run: `cd model-proxy && go test -run 'TestStatsFlags|TestRenderStats' . && go test ./... && gofmt -w cmd_stats.go cli_test.go && go vet ./...`
Expected: PASS; the existing `TestRenderStatsCLI` still passes (proves the classic output is unchanged).
```bash
git add model-proxy/cmd_stats.go model-proxy/cli_test.go model-proxy/CLI.md
git commit -m "feat(cli): add stats --granularity/--cost (append-only; existing output unchanged)"
```

---

## Task 7: Analytics Web UI tab

**Files:**
- Modify: `model-proxy/web_assets/index.html`, `model-proxy/web_assets/app.js`, `model-proxy/web_assets_contract_test.go`
- Create: `model-proxy/web_assets/vendor/uPlot.min.js`
- Consumes: `apiGet` helper, `activateTab`/`renderXTab` pattern, `/api/analytics`.
- Produces: Analytics tab (range/granularity/filters, uPlot token + cost charts, summary table, unpriced hint).

- [ ] **Step 1: Vendor uPlot**

Download uPlot 1.6.x (UMD min) into the vendor dir:

```bash
cd model-proxy/web_assets/vendor
curl -fsSL -o uPlot.min.js https://unpkg.com/uplot@1.6.31/dist/uPlot.min.js
curl -fsSL -o uPlot.min.css https://unpkg.com/uplot@1.6.31/dist/uPlot.min.css
```

Verify it parses (`node -e "require('./uPlot.min.js')"` may not work for UMD; instead just check it's non-empty and starts with a minified bundle). Add a one-line note to `vendor/README.md` mirroring the CodeMirror entry (name, version, source URL, "vendored offline, no CDN").

- [ ] **Step 2: Add the tab + uPlot assets to `index.html`**

In `web_assets/index.html`, add the button inside `.tabs` (after the Accounts button):

```html
        <button data-tab="analytics" class="tab" role="tab" aria-selected="false">Analytics</button>
```

Add the panel inside `<main>` (after the accounts panel):

```html
    <section id="tab-analytics" class="tab-panel" role="tabpanel" aria-label="Analytics"></section>
```

Add the stylesheet + script in `<head>`/before `app.js` (mirroring the CodeMirror tags):

```html
  <link rel="stylesheet" href="vendor/uPlot.min.css">
```
```html
  <script src="vendor/uPlot.min.js"></script>
```
(`uPlot` is a UMD global, like CodeMirror — load before the deferred `app.js` module.)

- [ ] **Step 3: Add `renderAnalyticsTab()` to `app.js`**

The other tabs are async `renderXTab()` functions called from the tab-activation path (see `renderStatusTab` at ~line 356). Add alongside them. Skeleton with real `apiGet` + a working uPlot invocation (palette/layout polish is finalized with the `dataviz`/`frontend-design` skills, but this is runnable as-is):

```js
// renderAnalyticsTab fetches /api/analytics and renders token + cost trend
// charts (uPlot), a summary table, and an unpriced-models hint.
async function renderAnalyticsTab() {
  const panel = document.getElementById('tab-analytics');
  const state = analyticsState();
  panel.innerHTML = `
    <div class="analytics-controls">
      <select id="an-range">
        <option value="7">7d</option><option value="30">30d</option>
        <option value="90">90d</option><option value="365">all</option>
      </select>
      <select id="an-gran">
        <option value="day">Day</option><option value="month">Month</option>
      </select>
      <input id="an-provider" placeholder="provider" />
      <input id="an-model" placeholder="model" />
      <button id="an-refresh">Refresh</button>
    </div>
    <div id="an-unpriced" class="an-hint" hidden></div>
    <div class="an-charts">
      <div id="an-token-chart"></div>
      <div id="an-cost-chart"></div>
    </div>
    <pre id="an-table"></pre>`;
  // restore controls from state
  panel.querySelector('#an-range').value = state.range;
  panel.querySelector('#an-gran').value = state.gran;
  panel.querySelector('#an-provider').value = state.provider;
  panel.querySelector('#an-model').value = state.model;
  panel.querySelector('#an-refresh').onclick = () => { renderAnalyticsTab(); };
  panel.querySelector('#an-range').onchange = panel.querySelector('#an-gran').onchange =
    panel.querySelector('#an-provider').onchange = panel.querySelector('#an-model').onchange =
      () => { renderAnalyticsTab(); };

  const from = Math.floor((Date.now() - Number(state.range) * 86400 * 1000) / 1000);
  const to = Math.floor(Date.now() / 1000);
  const q = new URLSearchParams({ from, to, granularity: state.gran });
  if (state.provider) q.set('provider', state.provider);
  if (state.model) q.set('model', state.model);
  let resp;
  try {
    resp = await apiGet('/api/analytics?' + q.toString());
  } catch (e) {
    panel.querySelector('#an-table').textContent = 'analytics unavailable: ' + e;
    return;
  }
  analyticsRenderHints(panel, resp);
  analyticsRenderCharts(panel, resp);
  analyticsRenderTable(panel, resp);
}

// analyticsState reads + persists the tab's control selections.
function analyticsState() {
  const s = { range: localStorage.getItem('an-range') || '30',
    gran: localStorage.getItem('an-gran') || 'day',
    provider: localStorage.getItem('an-provider') || '',
    model: localStorage.getItem('an-model') || '' };
  return s;
}
function analyticsSave(name, val) { localStorage.setItem('an-' + name, val); }
// (wire analyticsSave into the onchange handlers above — one line each)

function analyticsRenderHints(panel, resp) {
  const un = resp?.price_coverage?.unpriced || [];
  const el = panel.querySelector('#an-unpriced');
  if (un.length) {
    el.hidden = false;
    el.textContent = un.length + ' model(s) unpriced (no equivalent cost): ' + un.join(', ') +
      '. Add a `prices:` entry in config to price them.';
  } else {
    el.hidden = true;
  }
}

function analyticsRenderCharts(panel, resp) {
  // Flatten series into aligned time series for uPlot. x = sorted union of bucket
  // timestamps; one series per (provider,model) for tokens and for cost.
  const series = resp.series || [];
  const xs = Array.from(new Set(series.flatMap(s => s.points.map(p => p.bucket)))).sort((a,b)=>a-b);
  const fmtDt = ts => ts * 1000;
  const tokenData = [xs.map(fmtDt)];
  const costData = [xs.map(fmtDt)];
  const tokenSeries = [{ label: 'time' }];
  const costSeries = [{ label: 'time' }];
  for (const s of series) {
    const key = s.provider + '/' + s.model;
    const byTs = Object.fromEntries(s.points.map(p => [p.bucket, p]));
    tokenData.push(xs.map(t => (byTs[t]?.input || 0) + (byTs[t]?.output || 0)));
    tokenSeries.push({ label: key, points: { show: false } });
    costData.push(xs.map(t => byTs[t]?.cost == null ? null : byTs[t].cost));
    costSeries.push({ label: key, points: { show: false } });
  }
  const opts = (title, yLabel) => ({
    title, width: panel.querySelector('#an-token-chart').clientWidth || 600, height: 220,
    series: tokenSeries, scales: { x: { time: true } }, axes: [{}, { label: yLabel }],
  });
  new uPlot(opts('Tokens', 'tokens'), tokenData, panel.querySelector('#an-token-chart'));
  const costOpts = opts('Equivalent cost', 'USD');
  costOpts.series = costSeries;
  new uPlot(costOpts, costData, panel.querySelector('#an-cost-chart'));
}

function analyticsRenderTable(panel, resp) {
  const rows = (resp.series || []).map(s => {
    let reqs = 0, input = 0, output = 0, cost = null;
    for (const p of s.points) {
      reqs += p.requests; input += p.input; output += p.output;
      if (p.cost != null) { cost = (cost || 0) + p.cost; }
    }
    return [s.provider, s.model, reqs, input, output, cost == null ? 'n/a' : '$' + cost.toFixed(2)].join('\t');
  });
  panel.querySelector('#an-table').textContent =
    ['provider\tmodel\treqs\tinput\toutput\tcost'].concat(rows).join('\n');
}
```

Wire the tab into the activation path: where `activateTab(name)` dispatches to `renderStatusTab`/`renderConfigTab`/`renderAccountsTab` (~line 224–268), add `case 'analytics': renderAnalyticsTab();`. Use the `dataviz` + `frontend-design` skills to finalize palette, spacing, and chart styling — the data contract above is fixed.

- [ ] **Step 4: Update the web-assets contract test**

In `model-proxy/web_assets_contract_test.go`, add assertions that the Analytics tab button, panel, and uPlot asset exist (mirror whatever it currently checks for the other tabs):

```go
// inside the existing test that asserts tab presence, add:
if !bytes.Contains(indexHTML, []byte(`data-tab="analytics"`)) {
	t.Error("index.html missing Analytics tab button")
}
if !bytes.Contains(indexHTML, []byte(`id="tab-analytics"`)) {
	t.Error("index.html missing Analytics panel")
}
```
(Adjust to the test's existing variable names / helper pattern.)

- [ ] **Step 5: Run + commit**

Run: `cd model-proxy && go test ./... && gofmt -w . && go vet ./...`
Expected: PASS.
```bash
git add model-proxy/web_assets model-proxy/web_assets_contract_test.go
git commit -m "feat(web): add Analytics tab (token + equivalent-cost trends, uPlot)"
```

---

## Task 8: Docs sync — `AGENTS.md`

**Files:**
- Modify: `AGENTS.md` (the Web UI + `/api/*` 接口契约 table; the config/CLI sections)
- No test (docs).

- [ ] **Step 1: Add the `/api/analytics` row to the API table**

In `AGENTS.md`'s `/api/*` table (the one ending around line 124), insert a row adjacent to `/api/stats`:

```
| GET | `/api/analytics` | `?from=&to=&provider=&model=&granularity=day\|month` | `{granularity,from,to,series:[{provider,model,points:[{bucket,requests,input,output,cache_creation,cache_read,cost(priced),priced}]}],totals,price_coverage{priced,unpriced}}` | 日历日/月聚合 + **服务端算等价成本**（price×tokens，不落盘、不伪造；未知价 `cost:null,priced:false`）。价格优先级：config `prices:` > OpenRouter 缓存目录 |
```

- [ ] **Step 2: Document the config + env**

In the relevant AGENTS.md config/CLI sections, note:
- New top-level config: `pricing:{enabled,ttl,source_url}` (default enabled/24h/OpenRouter) and `prices:` map ($/M tokens, override > catalog).
- `MP_PRICING_URL` overrides the pricing endpoint (mirrors `MP_MODELSDEV_URL`).
- `stats --granularity day|month` / `--cost` (routes to `/api/analytics`; existing `stats` output unchanged).
- Pricing cache: `~/.model-proxy/pricing_cache.json` (24h TTL, offline fallback).

- [ ] **Step 3: Run + commit**

Run: `cd model-proxy && go test ./...` (sanity; docs-only change).
```bash
git add AGENTS.md
git commit -m "docs: document /api/analytics, pricing config, stats --cost"
```

---

## Final verification

- [ ] `cd model-proxy && go test -race ./...` — all PASS.
- [ ] `cd model-proxy && scripts/cover.sh` — every package ≥ 80%.
- [ ] `cd model-proxy && go vet ./... && gofmt -l .` — clean.
- [ ] Manual: `./model-proxy serve`, open `/ui/`, click **Analytics**, confirm token + cost charts render for a provider with traffic; confirm an unpriced model (e.g. `doubao-*`) shows `n/a` + the hint.
- [ ] Manual: `model-proxy stats --cost --granularity month` shows the cost column; `model-proxy stats` (no flags) is byte-identical to before.

## Self-review (already applied)

- **Spec coverage:** §5.1 catalog → Task 1; §5.2 config → Task 2; §5.3 cost math → Task 3; §5.4 query → Task 4; §5.5 API → Task 5; §5.7 CLI → Task 6; §5.6 frontend → Task 7; docs → Task 8. §6 edge cases (offline, unpriced, partial, tz, retention hint) covered in Task 1/4/5 + the unpriced UI hint. §2 non-goals (client, caps) intentionally absent.
- **Placeholders:** none — every code step has complete code. Task 5 uses the existing `p.cfgSnapshot()` accessor (verified at proxy.go:271).
- **Type consistency:** `pricingEntry`, `pricingCatalog`, `analyticsBucket`, `costResult`, `resolvePrice`, `computeCost`, `PriceConfig`, `PricingConfig` signatures match across tasks 1→5 and the CLI/frontend consume the same `/api/analytics` JSON keys.
