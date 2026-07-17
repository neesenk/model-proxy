# Analytics Dashboard — Design Spec

- **Date:** 2026-07-18
- **Status:** Approved design, ready for implementation plan
- **Topic:** Per-provider / per-model token & equivalent-cost trends, by day and by month

## 1. Goal

Add a **usage & cost analytics dashboard** to model-proxy: a new Web UI tab
showing token-usage trends **and** equivalent-cost trends, time-bucketed **by
day** and **by calendar month**, sliced by **provider** and **model**, backed by
the existing `minute_buckets` SQLite store.

The dashboard answers: *which provider/model am I spending tokens on, how is it
trending, and what would that volume cost at public pay-as-you-go prices?*

## 2. Non-goals (deferred)

- **Client dimension.** Today stats are keyed `(provider, model, minute)`. Adding
  a client column is a schema + hot-path change; explicitly **deferred** per the
  design discussion. The query/aggregation layer is written so a client
  dimension can be added later without reshaping the API.
- **Spend caps / budget enforcement & alerting.** This feature is the
  *reporting* half of the cost theme. Hard caps, circuit-breaking on spend, and
  webhook/email alerts are a separate follow-up.
- **New providers, protocol conversion, caching.** Out of scope.

## 3. Background — what already exists

- `stats.go`: `statsStore` owns table `minute_buckets (provider, model, minute,
  requests, failovers, rate_limited_429, failures, input, output,
  cache_creation, cache_read, token_requests, last_request_at)`, PK
  `(provider, model, minute)`, index `idx_minute`. Storage is always 1-minute;
  `queryRange` widens into display buckets via SQL `GROUP BY` (lossless).
- `web.go` `handleStats` (~line 398) → `GET /api/stats?from=&to=&provider=&model=&bucket=`,
  read-only, no extra lock (database/sql is goroutine-safe).
- `modelsdev.go`: the **catalog-cache pattern** to replicate for pricing —
  fetch + ETag/304 + 24h TTL + offline fallback to stale cache + env override
  (`MP_MODELSDEV_URL`), slim projection cached at `~/.model-proxy/models_cache.json`.
- Config (`config.go`): top-level blocks `stats` / `request_log` / `scheduling`
  with accessor methods (`StatsConfig.dbPath()` / `.retention()`). **Defaults
  live in code** (commit 8a4e077); config files show overrides only. There is a
  parallel `raw` struct consumed by `LoadConfigFromBytes` where code defaults are
  applied.
- Web UI: vanilla JS (`app.js`, no framework, no build step), tabs are
  `<button data-tab="…">` + `<section id="tab-…">`; third-party JS vendored under
  `web_assets/vendor/` (CodeMirror). Light/dark via `color-scheme`.
- `stats` CLI + `CLI.md`: the stable CLI display contract. **Append-only**
  additions are allowed; existing output must not change without sign-off, and
  `CLI.md` + its `strings.Contains` test assertions must be updated in the same
  commit.

## 4. Cost semantics

**Equivalent (notional) pay-as-you-go cost** for *all* providers, including
flat-rate subscription providers (aqp, codex, zhipu, volcengine, kimi-code).
Rationale (decided in design discussion): it gives a meaningful, comparable
usage-value axis even for subscription providers, and it is the only
interpretation that produces non-trivial trends across the whole fleet.

- Cost is **computed at query time** from `tokens × price`, never stored. A price
  correction or config override instantly re-prices all history.
- Tokens are the stored truth; price is a read-side lookup.
- **No pricing source exists today.** models.dev's `api.json` carries no
  pricing (verified). The source is OpenRouter's maintained catalog (see §5).

## 5. Design

### 5.1 Pricing catalog — new `pricing.go` (mirrors `modelsdev.go`)

Fetch `https://openrouter.ai/api/v1/models` (verified: ~342 models, each with
`pricing.{prompt, completion, input_cache_read}` and sometimes
`input_cache_write}`, values in **$ per token**). Covers deepseek-v4-pro,
glm*, kimi*, gpt-5.6-luna, claude-opus-4.8. **Does not cover doubao***
(volcengine: 0 matches) → those rely on config override or show `n/a`.

- **Parse** into a slim projection cached at
  `~/.model-proxy/pricing_cache.json`:
  `pricingEntry { Prompt, Completion, CacheRead, CacheWrite float64 }` ($/token),
  keyed by **bare model name** (OpenRouter `id` with the `provider/` prefix
  stripped). Dedup on a bare-name collision: prefer the **vendor-canonical**
  prefix (reuse `ownerRank` from `modelsdev.go`); this keeps e.g. the DeepSeek
  price from being shadowed by a reseller's variant.
- **Cache lifecycle** identical to models.dev: `pricingCatalog { FetchedAt,
  Etag, ByModel }`; `ensurePricingFresh(cacheFile, endpoint, fetch, force)`;
  24h TTL; `If-None-Match`→304 refreshes `fetched_at` only; fetch failure falls
  back to stale cache (logged to stderr), else empty catalog. Env override
  `MP_PRICING_URL` (tests / mirrors).
- **Name matching is exact-only — no fuzzy/normalization.** A fuzzy layer risks
  pricing the wrong model (e.g. `glm-4.6` against `glm-4.7-flash`). Version-skew
  and doubao names simply miss the catalog → resolved by config override, or
  reported `n/a`. This is deliberate; precision over coverage.
- **Lookup precedence:** config `prices:` (§5.2) > catalog. Miss → `n/a`.

### 5.2 Config surface (new top-level fields)

```yaml
pricing:
  enabled: true        # default true; false → cost shows n/a everywhere, no fetch
  ttl: 24h             # catalog cache TTL
  source_url: https://openrouter.ai/api/v1/models   # optional override

prices:                # $ / million tokens (human units); override > catalog
  doubao-seed-1-8-251228: { input: 0.5, output: 1.5 }
  glm-4.6:             { input: 0.9, output: 0.9, cache_read: 0.09 }
```

- New structs in `config.go`: `PricingConfig` (with `enabled()` / `ttl()` /
  `sourceURL()` accessors mirroring `StatsConfig`) and `Prices` as
  `map[string]PriceConfig`. Units in config are **$ / million tokens** for
  readability; converted to $/token at load (**÷ 1,000,000**). `cache_read`/`cache_write`
  optional (default 0).
- Applied in both the `Config` struct and the `raw` defaults struct +
  `LoadConfigFromBytes` (code-defaults-first convention). The existing
  `models:` name-list contract is **untouched**.
- Field names to be confirmed against `config.go` conventions during planning;
  the shape above is the proposal.

### 5.3 Cost computation

For one `(provider, model)` aggregate bucket:

```
cost = input·prompt + output·completion
     + cache_read·cacheRead
     + cache_creation·cacheWrite
```

- If a price is known (override or catalog), `cost` is a number and `priced=true`.
- If unknown, `cost = null` (JSON `null`), `priced=false`; tokens still returned
  and still trended.
- `cacheWrite` is priced only when the catalog exposes `input_cache_write` (or a
  config `cache_write` is set); otherwise cache-creation contributes 0 and the
  model is considered **partially unpriced** (still `priced=true` if input/output
  are known, but flagged in `price_coverage`).

### 5.4 Analytics query — `statsStore.queryAnalytics`

New method alongside `queryRange`. Signature:

```go
func (s *statsStore) queryAnalytics(from, to int64, provider, model string,
    granularity string) ([]analyticsBucket, error)
```

`granularity ∈ {"day","month"}` (anything else → 400 from the handler).

- **Calendar grouping** (the key difference from `queryRange`'s fixed-second
  floor):
  - day: `GROUP BY provider, model, date(minute,'unixepoch','localtime')`,
    bucket = unix start-of-day.
  - month: `GROUP BY provider, model, strftime('%Y-%m', minute,'unixepoch','localtime')`,
    bucket = unix start-of-month.
  - Aggregates: `SUM` of all counters, `MAX(last_request_at)` — same discipline
    as `queryRange`. Storage stays 1-minute (lossless); only the view widens.
- **Timezone:** bucketing uses the proxy host's **local timezone** (SQLite
  `'localtime'` modifier). **No tz config knob in v1** (deferred); default =
  local. Tests pin a fixed tz for determinism.
- Returns `[]analyticsBucket` (provider, model, bucket, requests, input, output,
  cache_creation, cache_read, last_request_at). Cost is attached by the handler
  (which holds the price catalog), keeping SQLite unaware of prices.

### 5.5 API — `GET /api/analytics`

Registered alongside `/api/stats` in `web.go` `serveAPI`. Read-only; mirrors
`handleStats`.

```
GET /api/analytics?from=<unix>&to=<unix>&provider=&model=&granularity=day|month
```

Response (snake_case JSON tags, matching the existing `/api/stats` convention —
**not** the PascalCase of `/api/status`):

```json
{
  "granularity": "day",
  "from": 1752883200,
  "to": 1753056000,
  "series": [
    { "provider": "deepseek", "model": "deepseek-v4-pro",
      "points": [ { "bucket": 1752883200, "requests": 12,
                    "input": 45000, "output": 3200,
                    "cache_creation": 0, "cache_read": 0,
                    "cost": 0.0594, "priced": true } ] }
  ],
  "totals": { "by_provider": [ … ], "by_model": [ … ], "cost": 12.34 },
  "price_coverage": { "priced": ["deepseek-v4-pro", …], "unpriced": ["doubao-seed-1-8-251228"] }
}
```

- `from`/`to` default: last 30 days when omitted.
- `price_coverage` drives the UI hint ("N models have no price — add `prices:`
  entries").
- Handler applies prices: iterate series, look up price (override > catalog),
  compute `cost`/`priced` per point and roll up `totals`.

### 5.6 Frontend — new Analytics tab

- `index.html`: add `<button data-tab="analytics" class="tab" role="tab">Analytics</button>`
  and `<section id="tab-analytics" class="tab-panel" role="tabpanel"></section>`.
- `app.js`: add render logic (same vanilla pattern as the other tabs).
  - **Controls:** range (7d / 30d / 90d / all), granularity (Day / Month),
    provider filter, model filter, refresh.
  - **Charts:** (1) token trend — input / output / cache over time;
    (2) cost trend — over time. Vendor **uPlot** (~45 KB, no deps) into
    `web_assets/vendor/`, matching the CodeMirror offline pattern. Honor
    `color-scheme: light dark`.
  - **Summary table:** per provider/model — total tokens, total cost, requests.
  - **Unpriced hint:** list models from `price_coverage.unpriced`.
- Visual design (palette, chart styling, layout) is finalized during
  implementation using the `dataviz` + `frontend-design` skills; this spec fixes
  the data contract and component list, not the aesthetics.

### 5.7 CLI — `stats` extensions (append-only)

- `--granularity day|month`: calendar bucketing (distinct from the existing
  fixed-seconds `--bucket`). When set, rows group by calendar day/month.
- `--cost`: add a Cost column (and totals) computed via the same price lookup.
  Existing `stats` output (without the flags) is **unchanged**.
- Update `CLI.md` (the stable contract) and its `strings.Contains` test
  assertions in the same commit. Append-only → permitted without separate
  sign-off, but `CLI.md` must reflect the new flags.

## 6. Edge cases & error handling

- **Catalog unreachable, no cache:** empty catalog → all cost `n/a`; tokens still
  trend. Never block the dashboard on pricing.
- **`pricing.enabled: false`:** skip fetch entirely; cost column shows `n/a`
  everywhere; token trends fully functional.
- **Unpriced model:** `cost=null`, `priced=false`; surfaced in `price_coverage`.
- **Partial pricing** (no `cache_write`): input/output priced, cache-creation
  contributes 0; model listed under `priced` but the UI may annotate.
- **Timezone:** local-time buckets; documented; tests pin tz.
- **Retention:** `stats.retention` (default 30d) bounds history. **Month trends
  need retention raised to a few months to be useful** — surface this in the UI
  (e.g., "showing last 30d; raise stats.retention for longer trends") and in
  docs. No silent truncation.
- **Concurrency:** read-only SELECTs; no new lock. Same access discipline as
  `/api/stats` (single connection, WAL).

## 7. Testing strategy (per the repo testing contract)

White-box, stdlib `testing` + `httptest` only, 80% coverage gate (`scripts/cover.sh`).

- **`pricing.go`** (mirror `modelsdev_test.go`):
  - Parse OpenRouter fixture → assert exact `$`/token values for a known model
    (e.g., `deepseek-v4-pro` prompt price == fixture value).
  - Name matching: bare-name hit; `provider/` prefix stripped; a name absent
    from the catalog → miss (not a wrong-model match).
  - Override precedence: config `prices:` beats catalog for the same model.
  - Offline fallback: httptest server down + stale cache present → stale used;
    no cache → empty catalog, no panic.
  - ETag 304 refreshes `fetched_at` only.
- **`queryAnalytics`** (mirror `TestStatsQueryRangeFilters`):
  - Insert known 1-minute rows across a day/month boundary in a **fixed tz**;
    query `day`/`month`; assert `SUM` of each counter, `MAX(last_request_at)`,
    bucket == window **start** (calendar floor). Re-query raw 1-minute to prove
    lossless storage. (Mirror the exact bucket-start + lossless assertions.)
- **Cost math**: given fixed tokens + a known price, assert exact `cost`; with
  no price assert `cost == nil` and `priced == false`.
- **`/api/analytics` handler** (mirror `TestAPIStatsHandler`): seed buckets via
  `flushDeltas`, assert JSON shape, `granularity` echo, `price_coverage` contents,
  and a precisely-costed point.
- **`stats --cost` / `--granularity`**: CLI render test asserts the new column
  appears and existing output is byte-identical without the flags.
- **No fake tests** (per contract): assert exact values, not "non-empty"; no
  `Contains(x) || Contains(y)`.

## 8. Sequencing (implementation plan will detail)

1. `pricing.go` + tests (catalog fetch/cache/match) + `PricingConfig`/`Prices` in
   `config.go`.
2. `queryAnalytics` + tests (calendar grouping).
3. `/api/analytics` handler + cost computation + tests.
4. Analytics tab: `index.html` + `app.js` + vendored uPlot.
5. `stats --cost` / `--granularity` + `CLI.md` + test assertions.
6. Docs/AGENTS.md sync (new `/api/analytics` row, `pricing`/`prices` config,
   `MP_PRICING_URL`).

## 9. Decisions log (from design discussion)

- **Client dimension:** deferred — schema + hot-path change not justified now.
- **Cost meaning:** equivalent/notional payg cost across all providers (not
  real-spend for subscription providers).
- **Pricing source:** OpenRouter catalog (cached) + config override; **not**
  models.dev (no pricing), **not** config-only (too sparse at start), **not**
  token-only (doesn't meet the cost ask).
- **Name matching:** exact-only; config override is the escape hatch for doubao
  and version skew — precision over coverage.
