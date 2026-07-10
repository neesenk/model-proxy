# models.dev Metadata Auto-Source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Source model context/output/modalities metadata from `https://models.dev/api.json` so `providers.<name>.models` no longer needs hand-maintenance; the metadata supplements (never overrides) config and is consumed only by `model-proxy models` display and `takeover`.

**Architecture:** A new `modelsdev.go` module fetches+parses+dedupes the models.dev catalog into a slim on-disk cache (`~/.model-proxy/models_cache.json`, 24h TTL, ETag `304`-aware). `hydrateModels(cfg, cat)` merges config `models:` ∪ route-referenced models with precedence **config > models.dev > default**, returning a per-model source map. `cmdModels` (display + new `pull`) and `runTakeover` call `ensureCatalogFresh` + `hydrateModels` in-process; `config.yaml` is never written. Routing/forwarding/daemon are untouched (they never read `models`).

**Tech Stack:** Go stdlib only (`net/http`, `net/url`, `encoding/json`, `encoding/json` decode, `os`, `path/filepath`, `time`, `sort`, `strings`). Tests white-box `package main`, stdlib `testing` + `httptest`, no testify.

## Global Constraints

- **No new dependencies.** Stdlib only (repo convention: no testify).
- **`config.yaml` is never written** by any new code. Only the cache file `~/.model-proxy/models_cache.json` is written (atomic tmp+rename via `atomicWrite`).
- **Precedence is config > models.dev > default.** Config `models:` entries are authoritative per-model; models.dev only fills gaps (route models missing from config); unmatched → `defaultProviderModel` with a takeover warning.
- **Scope confined to `models` + `takeover` CLI paths.** Never call `ensureCatalogFresh`/`hydrateModels` from `LoadConfig`, `NewProxy`, `buildProviders`, or the daemon.
- **Default metadata (unmatched):** `ProviderModel{Context: 200000, Output: 16384, Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}}}`.
- **models.dev endpoint is env-overridable** via `MP_MODELSDEV_URL` (default `https://models.dev/api.json`) so subprocess CLI tests can mock it.
- **Do not set `Accept-Encoding` manually** in the real fetch — Go's `http.Transport` auto-requests gzip and transparently decompresses (manual header disables auto-decompress). The 3 MB body transfers as ~286 KB gzipped.
- **Tests assert exact values** (auth/quota contract rule): exact context/output numbers, exact source enums, exact membership — never "non-empty".
- Run from `model-proxy/`: `go test ./...` (~30s), `go test -race ./...`, `go vet ./...`, `gofmt -l .` must be clean. `scripts/cover.sh` gates at 80%/pkg.

## File Structure

- **Create `model-proxy/modelsdev.go`** — the whole feature: types, parser, lookup, cache, freshness, hydration. One focused file (~250 lines).
- **Create `model-proxy/modelsdev_test.go`** — unit tests for parse/lookup/cache/freshness/hydrate (same-process, injected fetch + temp-dir cache paths).
- **Modify `model-proxy/models.go`** — `cmdModels` gains `pull` branch + display-branch hydration; `printAllModels` gains a `sources` param + SRC column.
- **Modify `model-proxy/models_extra_test.go`** — update 3 `printAllModels` call sites to the new signature (pass `nil`).
- **Modify `model-proxy/takeover.go`** — `runTakeover` hydrates + emits default-source warnings.
- **Modify `model-proxy/cli_extra2_test.go`** (or new `cli_extra4_test.go`) — subprocess tests for `models` display (pre-seeded cache), `models pull` (mocked endpoint), `takeover` warning.
- **Modify `CLAUDE.md` + `AGENTS.md`** — document the feature + contract.

---

### Task 1: Catalog types + pure parser (`modelsdev.go`)

**Files:**
- Create: `model-proxy/modelsdev.go`
- Test: `model-proxy/modelsdev_test.go`

**Interfaces:**
- Produces: `modelsDevModel` struct, `modelsDevCatalog` struct, `modelSource` enum (`srcConfig/srcModelsDev/srcDefault`), `defaultProviderModel`, `parseModelsDevAPI([]byte) *modelsDevCatalog`, `normalizeEndpoint(string) string`, `hostOf(string) string`.

- [ ] **Step 1: Write the failing test**

Create `model-proxy/modelsdev_test.go`:

```go
package main

import (
	"encoding/json"
	"testing"
)

// fixtureAPI is a tiny models.dev/api.json-shaped blob: two providers, one
// canonical (zhipuai) + one reseller (openrouter) both listing glm-4.6, plus
// a deepseek provider. Used to prove dedup (canonical wins) + endpoint indexing.
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
      "zhipuai/glm-4.6": {"limit": {"context": 999, "output": 999}, "modalities": {"input": ["text"], "output": ["text"]}}
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
		t.Errorf("ByName dedup count = %d, want 2 (glm-4.6, deepseek-v4-pro); got %v", len(cat.ByName), keys(cat.ByName))
	}
	// deepseek-v4-pro present with its real limits.
	if md2 := cat.ByName["deepseek-v4-pro"]; md2.Context != 1000000 || md2.Output != 65536 {
		t.Errorf("deepseek-v4-pro metadata wrong: %+v", md2)
	}
	// ByEndpoint: zhipu's exact URL + host key both index glm-4.6.
	zhipuURL := normalizeEndpoint("https://open.bigmodel.cn/api/paas/v4")
	if names, ok := cat.ByEndpoint[zhipuURL]; !ok || !contains(names, "glm-4.6") {
		t.Errorf("ByEndpoint[%s] missing glm-4.6: %v", zhipuURL, names)
	}
	if names, ok := cat.ByEndpoint["open.bigmodel.cn"]; !ok || !contains(names, "glm-4.6") {
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
func keys(m map[string]modelsDevModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ensure json import used (decoder path in later tasks); keep compile honest.
var _ = json.Unmarshal
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestParseModelsDevAPI -v .`
Expected: FAIL / compile error (`parseModelsDevAPI` and types undefined).

- [ ] **Step 3: Write minimal implementation**

Create `model-proxy/modelsdev.go`:

```go
package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// modelsDevModel is the slim per-model metadata projection cached from
// models.dev/api.json (only the fields model-proxy uses). Acronym keys keep the
// cache file small (~150 KB vs the 3 MB raw blob).
type modelsDevModel struct {
	Context int64    `json:"ctx"`
	Output  int      `json:"out"`
	Input   []string `json:"in"`
	Output_ []string `json:"out_mod"` // output modalities ("out" taken by Output)
}

// toProviderModel converts the slim projection into a config ProviderModel.
func (m modelsDevModel) toProviderModel() ProviderModel {
	return ProviderModel{
		Context:    m.Context,
		Output:     m.Output,
		Modalities: ProviderModalities{Input: m.Input, Output: m.Output_},
	}
}

// modelsDevCatalog is the on-disk + in-memory cache: a deduplicated name→metadata
// index plus an endpoint→model-names index for endpoint-scoped matching.
type modelsDevCatalog struct {
	FetchedAt  time.Time               `json:"fetched_at"`
	Etag       string                  `json:"etag"`
	ByName     map[string]modelsDevModel `json:"by_name"`
	ByEndpoint map[string][]string     `json:"by_endpoint"`
}

// modelSource records where a model's effective metadata came from.
type modelSource int

const (
	srcConfig modelsDevSource = iota // present in config models: block
	srcModelsDev                     // matched in the models.dev catalog
	srcDefault                       // unmatched → defaultProviderModel
)

// (alias so the iota is usable as modelsDevSource below without churn)
type modelsDevSource = modelSource

// defaultProviderModel is written for models found in neither config nor the
// catalog, so a takeover client config never gets a zero/nonsense limit.
var defaultProviderModel = ProviderModel{
	Context:    200000,
	Output:     16384,
	Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}},
}

// canonicalModelsDevOwners are models.dev provider keys that own a model family
// outright (rank 0); all other providers (resellers/aggregators) are rank 1.
// On a name collision the canonical owner's metadata wins.
var canonicalModelsDevOwners = map[string]bool{
	"zhipuai": true, "deepseek": true, "openai": true, "anthropic": true,
	"google": true, "moonshotai": true, "xai": true, "mistral": true,
	"cohere": true, "meta": true, "alibaba": true, "minimax": true,
	"stepfun": true, "amazon-bedrock": true,
}

func ownerRank(providerKey string) int {
	if canonicalModelsDevOwners[providerKey] {
		return 0
	}
	return 1
}

// normalizeEndpoint lowercases and strips a trailing slash for stable matching.
func normalizeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")
	return strings.ToLower(raw)
}

// hostOf returns the lowercased host of a URL string, or "" if unparseable.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// parseModelsDevAPI parses a models.dev/api.json blob into a deduplicated
// catalog. Canonical owners win on name collisions. Each provider's models are
// indexed under both the full-normalized api URL and its host.
func parseModelsDevAPI(blob []byte) *modelsDevCatalog {
	var raw map[string]struct {
		API    string `json:"api"`
		Models map[string]struct {
			Limit struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limit"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return &modelsDevCatalog{ByName: map[string]modelsDevModel{}, ByEndpoint: map[string][]string{}}
	}
	cat := &modelsDevCatalog{ByName: map[string]modelsDevModel{}, ByEndpoint: map[string][]string{}}
	rank := map[string]int{} // modelName → current owner rank
	for provKey, p := range raw {
		r := ownerRank(provKey)
		epFull := normalizeEndpoint(p.API)
		epHost := hostOf(epFull)
		var names []string
		for modelName, m := range p.Models {
			names = append(names, modelName)
			md := modelsDevModel{
				Context: m.Limit.Context,
				Output:  int(m.Limit.Output),
				Input:   m.Modalities.Input,
				Output_: m.Modalities.Output,
			}
			if cur, ok := rank[modelName]; !ok || r < cur {
				cat.ByName[modelName] = md
				rank[modelName] = r
			}
		}
		sortStrings(names)
		if epFull != "" {
			cat.ByEndpoint[epFull] = appendUnique(cat.ByEndpoint[epFull], names)
		}
		if epHost != "" && epHost != epFull {
			cat.ByEndpoint[epHost] = appendUnique(cat.ByEndpoint[epHost], names)
		}
	}
	return cat
}

// sortStrings returns a sorted copy (keeps modelsdev.go import-light; defined here
// to avoid pulling sort into the parse hot path awkwardly).
func sortStrings(s []string) {
	sortStringsInPlace(s)
}

// appendUnique appends names not already present.
func appendUnique(dst, add []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, x := range dst {
		seen[x] = true
	}
	for _, x := range add {
		if !seen[x] {
			dst = append(dst, x)
			seen[x] = true
		}
	}
	return dst
}
```

Also add to `model-proxy/modelsdev.go` (below) the tiny sort shim, OR just use `sort` directly — simplest is to import `sort` and replace `sortStrings(names)` with `sort.Strings(names)`. **Do that instead** (remove the `sortStrings`/`sortStringsInPlace` shim):

Replace the `sortStrings(names)` line with:
```go
		sort.Strings(names)
```
and add `"sort"` to the import block. Remove the `sortStrings` and `sortStringsInPlace` helpers entirely (they were a placeholder — use stdlib `sort.Strings`).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run 'TestParseModelsDevAPI|TestNormalizeEndpoint' -v .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add modelsdev.go modelsdev_test.go
git commit -m "feat(modelsdev): add catalog types + api.json parser (dedup, endpoint index)"
```

---

### Task 2: Catalog lookup

**Files:**
- Modify: `model-proxy/modelsdev.go`
- Test: `model-proxy/modelsdev_test.go`

**Interfaces:**
- Produces: `(*modelsDevCatalog).lookup(endpoints []string, model string) (modelsDevModel, bool)` — endpoint-scope first (full URL then host), then global name fallback.

- [ ] **Step 1: Write the failing test**

Append to `model-proxy/modelsdev_test.go`:

```go
func TestCatalogLookup(t *testing.T) {
	cat := parseModelsDevAPI([]byte(fixtureAPI))

	// 1. Endpoint match: zhipu base URL → zhipuai provider → glm-4.6.
	md, ok := cat.lookup([]string{"https://open.bigmodel.cn/api/paas/v4"}, "glm-4.6")
	if !ok || md.Context != 204800 {
		t.Errorf("endpoint match glm-4.6: ok=%v ctx=%d", ok, md.Context)
	}
	// 2. Name fallback: aqp has no matching endpoint; deepseek-v4-pro resolves globally.
	md, ok = cat.lookup([]string{"https://compass.llm.shopee.io/compass-api/v1"}, "deepseek-v4-pro")
	if !ok || md.Context != 1000000 {
		t.Errorf("name fallback deepseek-v4-pro: ok=%v ctx=%d", ok, md.Context)
	}
	// 3. Endpoint match wins over reseller name: zhipu endpoint scopes to zhipuai,
	//    so even though openrouter lists "zhipuai/glm-4.6" we get the canonical value.
	md, ok = cat.lookup([]string{"https://open.bigmodel.cn/api/paas/v4"}, "glm-4.6")
	if !ok || md.Context != 204800 {
		t.Errorf("endpoint scoping should pick canonical: ok=%v ctx=%d", ok, md.Context)
	}
	// 4. Unmatched (no endpoint, no global name) → ok=false.
	_, ok = cat.lookup([]string{"https://chatgpt.com/backend-api/codex"}, "gpt-5.5")
	if ok {
		t.Error("gpt-5.5 should be unmatched")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestCatalogLookup -v .`
Expected: FAIL (`cat.lookup undefined`).

- [ ] **Step 3: Write minimal implementation**

Append to `model-proxy/modelsdev.go`:

```go
// lookup resolves a model's metadata given the provider's base URLs (openai +
// anthropic). It tries endpoint-scoped matching first (full URL, then host) so a
// provider's own models.dev entry wins; then falls back to a global name match
// (rescuing models a provider borrows from another vendor). ok=false if neither.
func (cat *modelsDevCatalog) lookup(endpoints []string, model string) (modelsDevModel, bool) {
	if cat == nil {
		return modelsDevModel{}, false
	}
	for _, e := range endpoints {
		ne := normalizeEndpoint(e)
		for _, key := range []string{ne, hostOf(ne)} {
			if key == "" {
				continue
			}
			names, ok := cat.ByEndpoint[key]
			if !ok {
				continue
			}
			for _, n := range names {
				if n == model {
					md, ok := cat.ByName[model]
					return md, ok // endpoint-scoped hit
				}
			}
		}
	}
	if md, ok := cat.ByName[model]; ok {
		return md, true // global name fallback
	}
	return modelsDevModel{}, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestCatalogLookup -v .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add modelsdev.go modelsdev_test.go
git commit -m "feat(modelsdev): add catalog lookup (endpoint-scope then name fallback)"
```

---

### Task 3: Cache + ensureCatalogFresh

**Files:**
- Modify: `model-proxy/modelsdev.go`
- Test: `model-proxy/modelsdev_test.go`

**Interfaces:**
- Produces: `cachePath() string`, `loadCachedCatalog(path string) (*modelsDevCatalog, error)`, `saveCachedCatalog(path string, cat *modelsDevCatalog) error`, `catalogFetchFunc` type, `realModelsDevFetch(endpoint, etag string) (status int, body []byte, newEtag string, err error)`, `modelsDevEndpoint() string`, `ensureCatalogFresh(cacheFile, endpoint string, fetch catalogFetchFunc, force bool) (*modelsDevCatalog, error)`.
- Consumes: `atomicWrite` (web.go:686), `homeDir()` (util.go:44), `envOrEmpty` (util.go:15).

- [ ] **Step 1: Write the failing test**

Append to `model-proxy/modelsdev_test.go`:

```go
import (
	// add to existing import block: "os", "path/filepath", "time"
)

// catalogTTL must be exported/package-level so tests can read it; defined in modelsdev.go.
func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	cat := &modelsDevCatalog{
		FetchedAt: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		Etag:      `"abc"`,
		ByName:    map[string]modelsDevModel{"glm-4.6": {Context: 204800, Output: 131072, Input: []string{"text"}, Output_: []string{"text"}}},
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

// fakeFetch returns a fetch func scripted by status.
func fakeFetch(status int, body []byte, etag string) catalogFetchFunc {
	called := false
	return func(endpoint, inEtag string) (int, []byte, string, error) {
		called = true
		_ = called
		if status == 304 {
			return 304, nil, etag, nil
		}
		return status, body, etag, nil
	}
}

func TestEnsureCatalogFresh_304(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	// seed a stale cache (old fetched_at) so TTL forces a recheck
	seed := &modelsDevCatalog{FetchedAt: time.Now().Add(-2 * catalogTTL), Etag: `"old"`, ByName: map[string]modelsDevModel{"glm-4.6": {Context: 204800}}, ByEndpoint: map[string][]string{}}
	saveCachedCatalog(path, seed)
	cat, err := ensureCatalogFresh(path, "http://x", fakeFetch(304, nil, `"old"`), false)
	if err != nil {
		t.Fatal(err)
	}
	// 304 keeps the body/data, refreshes fetched_at to recent.
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
	// persisted to disk
	loaded, _ := loadCachedCatalog(path)
	if loaded == nil || loaded.Etag != `"newetag"` {
		t.Errorf("200 should persist cache: %+v", loaded)
	}
}

func TestEnsureCatalogFresh_TTLHitNoFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	fresh := &modelsDevCatalog{FetchedAt: time.Now(), Etag: `"e"`, ByName: map[string]modelsDevModel{"glm-4.6": {Context: 204800}}, ByEndpoint: map[string][]string{}}
	saveCachedCatalog(path, fresh)
	// fetch that would error if called
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
	fresh := &modelsDevCatalog{FetchedAt: time.Now(), Etag: `"old"`, ByName: map[string]modelsDevModel{"glm-4.6": {Context: 1}}, ByEndpoint: map[string][]string{}}
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
	stale := &modelsDevCatalog{FetchedAt: time.Now().Add(-2 * catalogTTL), Etag: `"e"`, ByName: map[string]modelsDevModel{"glm-4.6": {Context: 204800}}, ByEndpoint: map[string][]string{}}
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
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.ByName) != 0 || cat.ByName == nil {
		t.Errorf("no cache + fetch error → empty catalog, got %+v", cat.ByName)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestEnsureCatalogFresh -v .`
Expected: FAIL (undefined `catalogFetchFunc`, `ensureCatalogFresh`, `saveCachedCatalog`, `loadCachedCatalog`, `catalogTTL`).

- [ ] **Step 3: Write minimal implementation**

Append to `model-proxy/modelsdev.go` (add `"os"`, `"path/filepath"`, `"net/http"`, `"fmt"`, `"io"` to imports as needed):

```go
const catalogTTL = 24 * time.Hour

const defaultModelsDevEndpoint = "https://models.dev/api.json"

// modelsDevEndpoint returns the catalog endpoint, overridable via MP_MODELSDEV_URL
// for tests / self-hosted mirrors.
func modelsDevEndpoint() string {
	if v := envOrEmpty("MP_MODELSDEV_URL"); v != "" {
		return v
	}
	return defaultModelsDevEndpoint
}

// cachePath is the on-disk catalog cache location.
func cachePath() string {
	return filepath.Join(homeDir(), ".model-proxy", "models_cache.json")
}

// catalogFetchFunc fetches the catalog given an endpoint + last-known etag.
// Returns HTTP status, body (nil on 304), the response etag, and any error.
type catalogFetchFunc func(endpoint, etag string) (status int, body []byte, newEtag string, err error)

// realModelsDevFetch is the production fetch. Do NOT set Accept-Encoding manually
// — Go's Transport auto-requests gzip and transparently decompresses (a manual
// header disables auto-decompress). If-None-Match yields a free 304.
var realModelsDevFetch catalogFetchFunc = func(endpoint, etag string) (int, []byte, string, error) {
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
		return 304, nil, newEtag, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, newEtag, err
	}
	return resp.StatusCode, body, newEtag, nil
}

// loadCachedCatalog reads the cache file; returns (nil, nil) if absent.
func loadCachedCatalog(path string) (*modelsDevCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cat modelsDevCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	if cat.ByName == nil {
		cat.ByName = map[string]modelsDevModel{}
	}
	if cat.ByEndpoint == nil {
		cat.ByEndpoint = map[string][]string{}
	}
	return &cat, nil
}

// saveCachedCatalog atomically writes the cache.
func saveCachedCatalog(path string, cat *modelsDevCatalog) error {
	data, err := json.Marshal(cat)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// ensureCatalogFresh returns a usable catalog, fetching from models.dev only when
// the cache is missing/stale (or force=true). 304 refreshes fetched_at only;
// 200 rebuilds + persists; a fetch error falls back to a stale cache (logged)
// or an empty catalog so the calling command still runs.
func ensureCatalogFresh(cacheFile, endpoint string, fetch catalogFetchFunc, force bool) (*modelsDevCatalog, error) {
	cached, _ := loadCachedCatalog(cacheFile)
	if !force && cached != nil && time.Since(cached.FetchedAt) < catalogTTL {
		return cached, nil
	}
	etag := ""
	if cached != nil {
		etag = cached.Etag
	}
	status, body, newEtag, err := fetch(endpoint, etag)
	if err != nil {
		if cached != nil {
			fmt.Fprintf(os.Stderr, "model-proxy: models.dev unreachable (%v); using catalog cached %s ago\n", err, ageString(cached.FetchedAt))
			return cached, nil
		}
		return emptyCatalog(), nil
	}
	switch status {
	case http.StatusNotModified:
		cached.FetchedAt = time.Now()
		if newEtag != "" {
			cached.Etag = newEtag
		}
		_ = saveCachedCatalog(cacheFile, cached)
		return cached, nil
	case http.StatusOK:
		cat := parseModelsDevAPI(body)
		cat.FetchedAt = time.Now()
		cat.Etag = newEtag
		_ = saveCachedCatalog(cacheFile, cat)
		return cat, nil
	default:
		if cached != nil {
			return cached, nil
		}
		return emptyCatalog(), nil
	}
}

func emptyCatalog() *modelsDevCatalog {
	return &modelsDevCatalog{ByName: map[string]modelsDevModel{}, ByEndpoint: map[string][]string{}}
}

// ageString renders a duration since t as e.g. "5h" / "3m" / "just now".
func ageString(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run 'TestCacheRoundTrip|TestEnsureCatalogFresh' -v .`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add modelsdev.go modelsdev_test.go
git commit -m "feat(modelsdev): add cache + ensureCatalogFresh (TTL, ETag 304/200, fallback)"
```

---

### Task 4: hydrateModels

**Files:**
- Modify: `model-proxy/modelsdev.go`
- Test: `model-proxy/modelsdev_test.go`

**Interfaces:**
- Produces: `hydrateModels(cfg *Config, cat *modelsDevCatalog) map[string]map[string]modelSource` — mutates `cfg.Providers[*].Models` (config ∪ routes; precedence config>dev>default), returns the source map keyed `sources[provider][model]`.

- [ ] **Step 1: Write the failing test**

Append to `model-proxy/modelsdev_test.go`:

```go
func TestHydrateModels(t *testing.T) {
	cat := parseModelsDevAPI([]byte(fixtureAPI))
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://open.bigmodel.cn/api/paas/v4",
				Models: map[string]ProviderModel{"glm-4.6": {Context: 999, Output: 999}}}}, // config override wins
			"aqp":   {OpenAIBaseURL: "https://compass.llm.shopee.io/compass-api/v1"},
			"codex": {OpenAIBaseURL: "https://chatgpt.com/backend-api/codex"},
			"volcengine": {Models: map[string]ProviderModel{"doubao-x": {Context: 262144}}}, // config-only, no routes
		},
		Routes: map[string][]RouteTarget{
			"glm-4.6":        {{Provider: "zhipu", Model: "glm-4.6", Priority: 1}},
			"deepseek-v4-pro": {{Provider: "aqp", Model: "deepseek-v4-pro", Priority: 1}},
			"gpt-5.5":        {{Provider: "codex", Model: "gpt-5.5", Priority: 1}},
		},
	}
	src := hydrateModels(cfg, cat)

	// config wins for zhipu/glm-4.6 (999, not catalog 204800)
	if cfg.Providers["zhipu"].Models["glm-4.6"].Context != 999 {
		t.Errorf("config override lost: %d", cfg.Providers["zhipu"].Models["glm-4.6"].Context)
	}
	if src["zhipu"]["glm-4.6"] != srcConfig {
		t.Errorf("zhipu/glm-4.6 source = %v, want srcConfig", src["zhipu"]["glm-4.6"])
	}
	// aqp/deepseek-v4-pro: name-fallback match → catalog values + srcModelsDev
	if pm := cfg.Providers["aqp"].Models["deepseek-v4-pro"]; pm.Context != 1000000 || pm.Output != 65536 {
		t.Errorf("aqp/deepseek-v4-pro should be catalog-sourced: %+v", pm)
	}
	if src["aqp"]["deepseek-v4-pro"] != srcModelsDev {
		t.Errorf("aqp/deepseek-v4-pro source = %v, want srcModelsDev", src["aqp"]["deepseek-v4-pro"])
	}
	// codex/gpt-5.5: unmatched → defaults + srcDefault
	if pm := cfg.Providers["codex"].Models["gpt-5.5"]; pm.Context != 200000 || pm.Output != 16384 {
		t.Errorf("codex/gpt-5.5 should be default: %+v", pm)
	}
	if src["codex"]["gpt-5.5"] != srcDefault {
		t.Errorf("codex/gpt-5.5 source = %v, want srcDefault", src["codex"]["gpt-5.5"])
	}
	// volcengine config-only model preserved (no route), source = config
	if cfg.Providers["volcengine"].Models["doubao-x"].Context != 262144 {
		t.Errorf("volcengine config-only model lost: %+v", cfg.Providers["volcengine"].Models["doubao-x"])
	}
	if src["volcengine"]["doubao-x"] != srcConfig {
		t.Errorf("volcengine/doubao-x source = %v, want srcConfig", src["volcengine"]["doubao-x"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestHydrateModels -v .`
Expected: FAIL (`hydrateModels` undefined).

- [ ] **Step 3: Write minimal implementation**

Append to `model-proxy/modelsdev.go`:

```go
// hydrateModels makes cfg.Providers[*].Models "effective": every model already
// in config is authoritative (srcConfig); every model referenced in routes but
// missing from config is added from the catalog (srcModelsDev) or set to defaults
// (srcDefault). It mutates cfg in place and returns the per-(provider,model)
// source map. config.yaml is never touched — only the in-memory cfg.
func hydrateModels(cfg *Config, cat *modelsDevCatalog) map[string]map[string]modelSource {
	sources := map[string]map[string]modelSource{}
	ensure := func(prov string) map[string]modelSource {
		if sources[prov] == nil {
			sources[prov] = map[string]modelSource{}
		}
		return sources[prov]
	}
	// 1. seed: existing config models are authoritative.
	for provName, prov := range cfg.Providers {
		s := ensure(provName)
		for mid := range prov.Models {
			s[mid] = srcConfig
		}
	}
	// 2. add route models missing from config (config ∪ routes).
	for _, targets := range cfg.Routes {
		for _, t := range targets {
			prov, ok := cfg.Providers[t.Provider]
			if !ok {
				continue
			}
			if _, exists := prov.Models[t.Model]; exists {
				continue // config wins
			}
			if prov.Models == nil {
				prov.Models = map[string]ProviderModel{}
			}
			s := ensure(t.Provider)
			if md, ok := cat.lookup([]string{prov.OpenAIBaseURL, prov.AnthropicBaseURL}, t.Model); ok {
				prov.Models[t.Model] = md.toProviderModel()
				s[t.Model] = srcModelsDev
			} else {
				prov.Models[t.Model] = defaultProviderModel
				s[t.Model] = srcDefault
			}
			cfg.Providers[t.Provider] = prov // map value copy: write back
		}
	}
	return sources
}
```

> **Note:** `cfg.Providers[t.Provider] = prov` writes back because ranging over a map yields a value copy; mutating `prov.Models` then requires reassigning. (Go map-of-struct semantics.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestHydrateModels -v .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add modelsdev.go modelsdev_test.go
git commit -m "feat(modelsdev): add hydrateModels (config ∪ routes, config>dev>default)"
```

---

### Task 5: Wire `models` display + `models pull`

**Files:**
- Modify: `model-proxy/models.go:28` (`cmdModels`), `model-proxy/models.go:68` (`printAllModels`)
- Modify: `model-proxy/models_extra_test.go` (3 call sites)
- Test: `model-proxy/cli_extra2_test.go` (append subprocess tests)

**Interfaces:**
- Consumes: `ensureCatalogFresh`, `hydrateModels`, `cachePath`, `modelsDevEndpoint`, `realModelsDevFetch` (from Tasks 3–4), `modelSource` values.
- Produces: `printAllModels(cfg *Config, provFilter string, sources map[string]map[string]modelSource)`.

- [ ] **Step 1: Update `printAllModels` signature + SRC column**

In `model-proxy/models.go`, replace the `printAllModels` function (lines 67–110) with:

```go
// printAllModels prints all models from the (possibly hydrated) config. `sources`
// (nil in legacy callers) drives a trailing SRC tag: config / models.dev / default.
func printAllModels(cfg *Config, provFilter string, sources map[string]map[string]modelSource) {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		if provFilter != "" && n != provFilter {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("%s  %s  %s  %s  %s  %s  %s\n",
		cDim(pad("PROVIDER", 12)), cDim(pad("MODEL ID", 22)),
		cDim(pad("NAME", 20)), cDim(pad("CTX", 10)),
		cDim(pad("OUTPUT", 8)), cDim(pad("MODALITIES", 16)), cDim(pad("SRC", 10)))
	for _, pn := range names {
		prov := cfg.Providers[pn]
		modelIDs := make([]string, 0, len(prov.Models))
		for mid := range prov.Models {
			modelIDs = append(modelIDs, mid)
		}
		sort.Strings(modelIDs)
		for _, mid := range modelIDs {
			m := prov.Models[mid]
			name := mid
			ctx := "—"
			if m.Context > 0 {
				ctx = fmt.Sprintf("%d", m.Context)
			}
			out := "—"
			if m.Output > 0 {
				out = fmt.Sprintf("%d", m.Output)
			}
			mod := "text"
			if len(m.Modalities.Input) > 0 {
				mod = strings.Join(m.Modalities.Input, "/")
			}
			src := "config"
			if sources != nil {
				if s, ok := sources[pn][mid]; ok {
					switch s {
					case srcModelsDev:
						src = "models.dev"
					case srcDefault:
						src = "default"
					}
				}
			} else {
				src = "" // legacy/no-hydration call: blank SRC
			}
			fmt.Printf("%s  %s  %s  %s  %s  %s  %s\n",
				cBlue(pad(pn, 12)), cCyan(pad(mid, 22)),
				cGreen(pad(name, 20)), cGray(pad(ctx, 10)),
				cGray(pad(out, 8)), cGray(pad(mod, 16)), cGray(pad(src, 10)))
		}
	}
}
```

- [ ] **Step 2: Update the 3 legacy call sites in `models_extra_test.go`**

In `model-proxy/models_extra_test.go`, change every `printAllModels(cfg, "...")` to `printAllModels(cfg, "...", nil)`:
- line ~50: `printAllModels(cfg, "")` → `printAllModels(cfg, "", nil)`
- line ~63: `printAllModels(cfg, "a")` → `printAllModels(cfg, "a", nil)`
- line ~78: `printAllModels(cfg, "")` → `printAllModels(cfg, "", nil)`

(Run `cd model-proxy && go vet .` to catch any missed call site.)

- [ ] **Step 3: Wire `cmdModels` — `pull` branch + display hydration**

In `model-proxy/models.go`, replace `cmdModels` (lines 28–65) with:

```go
// cmdModels handles:
//
//	models              — list all models from all providers (config + models.dev supplement)
//	models <provider>   — list models for one provider
//	models pull         — force-refresh the models.dev catalog cache
//	models refresh <provider> — fetch live model list from a provider's server
func cmdModels(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	rest := nonFlagArgs(args)
	if len(rest) > 0 && rest[0] == "pull" {
		// models pull — force-refresh the global models.dev catalog cache.
		cat, ferr := ensureCatalogFresh(cachePath(), modelsDevEndpoint(), realModelsDevFetch, true)
		if ferr != nil {
			log.Fatal(ferr)
		}
		nProviders := len(cat.ByEndpoint) / 2 // url+host keys per provider (approx)
		fmt.Printf("models.dev catalog refreshed: %d unique models, etag %s\n", len(cat.ByName), cat.Etag)
		_ = nProviders
		return
	}
	if len(rest) > 0 && rest[0] == "refresh" {
		// models refresh <provider>
		if len(rest) < 2 {
			fmt.Println("usage: model-proxy models refresh <provider>")
			fmt.Println("available providers:")
			for name := range cfg.Providers {
				fmt.Printf("  %s\n", name)
			}
			return
		}
		provName := rest[1]
		if _, ok := cfg.Providers[provName]; !ok {
			log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
		}
		fmt.Fprintf(os.Stderr, "Refreshing models from %s...\n", provName)
		entries, err := fetchProviderModels(cfg, provName)
		if err != nil {
			log.Fatal(err)
		}
		printProviderModels(provName, entries)
		return
	}
	// models [provider] — config + models.dev supplement
	provFilter := ""
	if len(rest) > 0 {
		provFilter = rest[0]
		if _, ok := cfg.Providers[provFilter]; !ok {
			log.Fatalf("unknown provider %q; available: %s", provFilter, providerNames(cfg))
		}
	}
	cat, _ := ensureCatalogFresh(cachePath(), modelsDevEndpoint(), realModelsDevFetch, false)
	sources := hydrateModels(cfg, cat)
	printAllModels(cfg, provFilter, sources)
}
```

- [ ] **Step 4: Write the subprocess tests**

Append to `model-proxy/cli_extra2_test.go` (add `"path/filepath"`, `"os"` to imports if missing):

```go
// --- models pull: force-refresh from a mocked models.dev endpoint ---

func TestCLI_ModelsPull_MockedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag := r.Header.Get("If-None-Match"); etag != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"zhipuai":{"api":"https://open.bigmodel.cn/api/paas/v4","models":{"glm-4.6":{"limit":{"context":204800,"output":131072},"modalities":{"input":["text"],"output":["text"]}}}}}`))
	}))
	defer srv.Close()

	cfgPath := writeTempConfig(t, minimalConfig)
	t.Setenv("MP_MODELSDEV_URL", srv.URL)
	home := t.TempDir()
	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath, "pull")
	if code != 0 {
		t.Fatalf("models pull exit=%d", code)
	}
	if !strings.Contains(stdout, "models.dev catalog refreshed") || !strings.Contains(stdout, "1 unique models") {
		t.Errorf("models pull output unexpected:\n%s", stdout)
	}
	// cache file created
	if _, err := os.Stat(filepath.Join(home, ".model-proxy", "models_cache.json")); err != nil {
		t.Errorf("cache file not created: %v", err)
	}
}

// --- models display: hydrates from a pre-seeded cache (no network) ---

func TestCLI_ModelsDisplay_HydratesFromCache(t *testing.T) {
	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://open.bigmodel.cn/api/paas/v4\nroutes:\n  glm-4.6:\n    - {provider: zhipu, model: glm-4.6}\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// pre-seed a FRESH cache (within TTL) so no fetch happens
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"\"v1\"","by_name":{"glm-4.6":{"ctx":204800,"out":131072,"in":["text"],"out_mod":["text"]}},"by_endpoint":{"https://open.bigmodel.cn/api/paas/v4":["glm-4.6"]}}`
	os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600)

	// point endpoint at a server that FAILS if contacted (proves cache hit)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath)
	if code != 0 {
		t.Fatalf("models display exit=%d", code)
	}
	if called {
		t.Error("fresh cache should NOT have fetched from endpoint")
	}
	if !strings.Contains(stdout, "glm-4.6") || !strings.Contains(stdout, "models.dev") {
		t.Errorf("display should show glm-4.6 with models.dev source:\n%s", stdout)
	}
	// ctx 204800 came from the cache, not config (config had no models:)
	if !strings.Contains(stdout, "204800") {
		t.Errorf("display should show cached context 204800:\n%s", stdout)
	}
}
```

Add `"time"` to cli_extra2_test.go imports if not present.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestCLI_ModelsPull|TestCLI_ModelsDisplay|TestPrintAllModels' -v .`
Expected: PASS. Then run the full models suite: `go test -run Models -v .` — all PASS.

- [ ] **Step 6: Commit**

```bash
cd model-proxy
git add models.go models_extra_test.go cli_extra2_test.go
git commit -m "feat(models): hydrate display from models.dev + add `models pull`"
```

---

### Task 6: Wire takeover hydration + default-source warnings

**Files:**
- Modify: `model-proxy/takeover.go:66` (`runTakeover`)
- Test: `model-proxy/cli_extra2_test.go` (append) or `takeover_extra_test.go`

**Interfaces:**
- Consumes: `ensureCatalogFresh`, `hydrateModels`, `cachePath`, `modelsDevEndpoint`, `realModelsDevFetch`, `exposedModels` (clients.go:44), `srcDefault`.

- [ ] **Step 1: Write the failing test**

Append to `model-proxy/cli_extra2_test.go`:

```go
// --- takeover opencode: warns on default-sourced models + writes defaults ---

func TestCLI_TakeoverOpencode_WarnsDefault(t *testing.T) {
	// codex/gpt-5.5 is NOT in models.dev fixture → default + warning
	cfgBody := "listen: 127.0.0.1:15721\ntakeover:\n  provider_id: model-proxy\n  opencode: " + filepath.Join(t.TempDir(), "oc.json") + "\nproviders:\n  codex:\n    provider_id: codex\n    openai_base_url: https://chatgpt.com/backend-api/codex\nroutes:\n  gpt-5.5:\n    - {provider: codex, model: gpt-5.5}\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// fresh empty cache (no models) → gpt-5.5 unmatched → default
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"","by_name":{},"by_endpoint":{}}`
	os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	_, stderr, code := runCLIWithHome(t, home, "takeover", cfgPath, "opencode")
	if code != 0 {
		t.Fatalf("takeover exit=%d", code)
	}
	if !strings.Contains(stderr, "gpt-5.5") || !strings.Contains(stderr, "default") {
		t.Errorf("takeover should warn about gpt-5.5 default:\n%s", stderr)
	}
	// opencode config written with default ctx 200000
	ocPath := strings.TrimSpace(strings.TrimPrefix(cfgBody[strings.Index(cfgBody, "opencode: "):], "opencode: "))
	// (the path is the temp oc.json; recompute simply by reading the written file)
	_ = ocPath
}
```

> Note: the assertion on the written file is fiddly to parse from cfgBody; instead read the opencode file directly. Simplify Step 1's tail: drop the `ocPath` parsing block and instead, in the test, read `cfg.Takeover.Opencode` is not accessible here. **Simpler:** assert the warning only (the default-values-written assertion is already covered by `TestHydrateModels` proving `cfg.Providers["codex"].Models["gpt-5.5"].Context == 200000`). So remove the trailing `ocPath` lines; the test asserts the stderr warning. Final test body ends after the `stderr` Contains check.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestCLI_TakeoverOpencode_WarnsDefault -v .`
Expected: FAIL (no warning emitted yet).

- [ ] **Step 3: Write minimal implementation**

In `model-proxy/takeover.go`, modify `runTakeover` (lines 66–79) to hydrate first and warn on default-sourced models that opencode/pi will write:

```go
func runTakeover(cfg *Config, which, bakDir string) error {
	// Hydrate model metadata from models.dev (config > models.dev > default) so
	// takeover writes real context/output/modalities. Warn about models that fell
	// back to defaults (unmatched in config + catalog). config.yaml is untouched.
	cat, _ := ensureCatalogFresh(cachePath(), modelsDevEndpoint(), realModelsDevFetch, false)
	sources := hydrateModels(cfg, cat)
	emitTakeoverWarnings(cfg, which, sources)

	clients := listClients(cfg, which)
	for _, c := range clients {
		log.Printf("takeover %s: %s (backup → %s/)", c.name, c.file, bakDir)
		if err := backup(c.file, bakDir, c.name); err != nil {
			return fmt.Errorf("%s backup: %w", c.name, err)
		}
		if err := c.rewrite(cfg); err != nil {
			return fmt.Errorf("%s rewrite: %w", c.name, err)
		}
		log.Printf("  ✓ %s done", c.name)
	}
	return nil
}

// emitTakeoverWarnings prints a stderr warning for each default-sourced model
// that a metadata-writing client (opencode, pi) in this takeover set will emit.
// claude/codex don't write per-model metadata, so they are skipped to avoid noise.
func emitTakeoverWarnings(cfg *Config, which string, sources map[string]map[string]modelSource) {
	clients := listClients(cfg, which)
	writesMetadata := false
	for _, c := range clients {
		if c.name == "opencode" || c.name == "pi" {
			writesMetadata = true
			break
		}
	}
	if !writesMetadata {
		return
	}
	for _, m := range exposedModels(cfg) {
		if sources[m.provider] != nil && sources[m.provider][m.realModel] == srcDefault {
			fmt.Fprintf(os.Stderr, "warning: model %s at %s: no models.dev metadata — wrote defaults (ctx=%d out=%d text-only)\n",
				m.realModel, m.provider, defaultProviderModel.Context, defaultProviderModel.Output)
		}
	}
}
```

Add `"os"` to takeover.go imports.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestCLI_TakeoverOpencode_WarnsDefault -v .`
Expected: PASS. Then run the full takeover suite: `go test -run Takeover -v .` — all PASS (existing takeover tests build cfg directly and call `runTakeover`? verify they still pass — they pre-seed no cache → empty catalog → models default, but their configs already have `models:` so config wins; warnings only for route models missing from config. If an existing test asserts clean stderr, it may now see warnings — check and adjust).

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add takeover.go cli_extra2_test.go
git commit -m "feat(takeover): hydrate from models.dev + warn on default-sourced models"
```

---

### Task 7: Docs + coverage gate

**Files:**
- Modify: `CLAUDE.md`, `AGENTS.md`

- [ ] **Step 1: Run the full suite + race + vet + fmt + coverage**

```bash
cd model-proxy
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
bash scripts/cover.sh
```
Expected: all green; coverage ≥80% per package (main, provider). `scripts/cover.sh` exits non-zero if any package <80%.

- [ ] **Step 2: Update CLAUDE.md**

Add a bullet to the Architecture section (near the `models` description) and the "Adding a new provider" / usage notes:

> **models.dev metadata supplement**: `providers.<name>.models` is optional. `model-proxy models` and `takeover` auto-fetch a slim, deduplicated projection of `https://models.dev/api.json` into `~/.model-proxy/models_cache.json` (24h TTL, ETag `304`-aware, gzip — 3 MB raw → ~286 KB transfer, ~150 KB on disk). Effective model metadata resolves **config > models.dev > default**; models.dev only fills gaps (route models missing from config), never overrides config, and **config.yaml is never written**. Matching is endpoint-first (provider base URL vs models.dev `api`, full-URL then host) then global model-name suffix; unmatched models get defaults (`ctx=200000 out=16384 text`) with a takeover warning. Scope: only the `models`/`takeover` CLI paths — the daemon/proxy hot path is unaffected. `models pull` force-refreshes the cache; `MP_MODELSDEV_URL` overrides the endpoint (tests/mirrors).

- [ ] **Step 3: Update AGENTS.md**

Add a `models.dev` subsection to the contracts/gotchas log: the api.json shape (`provider → {api, models → {limit.{context,output}, modalities.{input,output}}}`), the 3 MB/gzip/ETag/304 facts, the dedup + canonical-owner rule, and the `MP_MODELSDEV_URL` seam.

- [ ] **Step 4: Commit docs + final verification**

```bash
cd model-proxy
git add CLAUDE.md AGENTS.md
git commit -m "docs: document models.dev metadata supplement + cache contract"
git go test ./...   # final clean run
```

---

## Self-Review (completed by plan author)

- **Spec coverage**: precedence (config>dev>default) → Task 4; effective set (config∪routes) → Task 4; matching (endpoint→name→default) → Task 2 + Task 4; cache (slim projection, TTL, ETag 304/200, fallback) → Task 3; `models` display + `models pull` → Task 5; takeover warnings + defaults → Task 6; no-config-write → enforced (only cache file written, Global Constraint); scope confinement → hydrate called only in cmdModels/runTakeover. ✅
- **Placeholder scan**: removed the `sortStrings` shim (use stdlib `sort.Strings`); removed the fiddly `ocPath` parsing in the takeover test (warning-only assertion; default-values-written covered by `TestHydrateModels`). No TBD/TODO remain.
- **Type consistency**: `modelsDevModel` fields `Context/Output/Input/Output_` used consistently; `modelSource`/`srcConfig/srcModelsDev/srcDefault` used in hydrate, printAllModels, emitTakeoverWarnings; `catalogFetchFunc` signature `(endpoint, etag) (status, body, newEtag, err)` matches `realModelsDevFetch` + test fakes; `printAllModels(cfg, provFilter, sources)` matches all call sites.
