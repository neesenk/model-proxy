# Auto-source model metadata from models.dev

**Date:** 2026-07-11
**Status:** Approved (pending spec review)
**Scope:** new `modelsdev` module (catalog fetch + cache + matching), `models`/`takeover` command enrichment, `Provider.Models` hydration in those CLI paths, tests, docs. **No** change to routing, forwarding, auth, quota, or `config.yaml` writes.

## Problem

Every model a provider serves must be hand-listed under `providers.<name>.models` in `config.yaml`
with `context` / `output` / `modalities` — tedious, error-prone, and it drifts the moment an upstream
ships a new model. The operator wants this metadata sourced automatically from
[models.dev](https://models.dev) so the `models:` block is no longer hand-maintained.

## Constraint: what `models` is actually used for

`Provider.Models` (`config.go:99`, `map[string]ProviderModel`) is **read-only after config load** and
is consumed only for:

- **takeover** — `exposedModels()` (`clients.go:44-71`) attaches `prov.Models[t.Model]`; opencode/pi
  config writers read its `Context`/`Output`/`Modalities` (`clients.go:112-191`).
- **`model-proxy models` display** — `printAllModels` (`models.go:78-109`).
- **`usage`/`doctor`** — model IDs + `len(prov.Models)` counts (`main.go:1420-1430,1633,1652`).

It is **never** read by request forwarding, scheduling, `GET /v1/models` (which uses route keys only),
auth, or quota. So auto-sourcing it is low-risk: the metadata is informational, never on the hot path.

## models.dev contract (`GET https://models.dev/api.json`)

Provider-keyed (155 providers, 244 models, ~tens of KB). Each provider:

```json
"zhipuai": {
  "id": "zhipuai", "name": "Zhipu",
  "api": "https://open.bigmodel.cn/api/paas/v4",
  "models": {
    "glm-4.6": {
      "modalities": {"input": ["text"], "output": ["text"]},
      "limit": {"context": 204800, "output": 131072},
      "cost": {...}, "reasoning": true, ...
    }
  }
}
```

- `api` = the provider's OpenAI-style base URL (host+path).
- `limit.context` / `limit.output` map 1:1 to `ProviderModel.Context` / `.Output`.
- `modalities.input` / `modalities.output` map 1:1 to `ProviderModalities`.

**Key limitation:** models.dev has **no** entry for several model-proxy providers — `aqp`
(`compass.llm.shopee.io`), `codex` (`chatgpt.com/backend-api/codex`), `volcengine`
(`ark.cn-beijing.volces.com`). It also lacks some model families volcengine serves (e.g. `doubao-*`).
So we cannot map each model-proxy provider to one models.dev provider. Matching must be tolerant.

**Size (measured):** `api.json` is **3.05 MB raw** — 5,478 provider×model pairs, heavily duplicated
(every reseller re-lists every model; only 244 unique models). It is **~286 KB gzipped** (Go's transport
requests gzip transparently) and `If-None-Match`→`304` returns **0 bytes** when unchanged (verified).
`catalog.json` (3.25 MB combined) and `models.json` (201 KB / 244 deduped models, but provider-agnostic —
no `api`) also exist; `api.json` alone has everything needed (provider `api` + per-model limits/modalities)
and its cost is bounded by gzip + ETag.

## Design

### Precedence model (the core rule)

For a given `{provider, model}`, effective metadata resolves as:

1. **config** — if `providers.<name>.models[<model>]` exists → its values are authoritative.
2. **models.dev** — else if matched in the catalog (algorithm below) → catalog values.
3. **default** — else → conservative defaults (warning emitted at takeover).

**`config.yaml` is never written** by any of this. models.dev only *supplements* gaps; config values
win wherever config already has the model. The operator may delete `models:` blocks by hand once they
trust the supplement; the feature works identically with or without them (backward-compatible).

### Effective model set per provider

`effective = config.models ∪ { model : (provider, model) appears in routes }`

i.e. everything configured *plus* any model named in `routes:` for this provider that config omitted.
(Models not in any route and not in config do not appear — they are neither reachable nor displayed.)

### Matching algorithm (config-miss → models.dev lookup)

Given `(provider, modelName)` with no config entry:

1. **Endpoint scope (first).** Normalize the provider's `openai_base_url` **and** `anthropic_base_url`,
   and the models.dev provider's `api` (lowercase, strip trailing `/`). If a models.dev provider matches
   — **full URL first, then host** — look up `modelName` in *that* provider's `models`. Hit examples:
   `zhipu` (`open.bigmodel.cn/api/paas/v4`) → `zhipuai`; `deepseek` (`api.deepseek.com`) → `deepseek`.
2. **Global name fallback.** Else look up `modelName` as a **suffix** across the whole catalog
   (`deepseek-v4-pro` → `deepseek/deepseek-v4-pro`). This rescues borrowed models: `aqp` serves
   `glm-5.2` / `deepseek-v4-pro` / `deepseek-v4-flash` — all matched via suffix despite `aqp`'s endpoint
   having no models.dev entry. First match wins, with a small canonical-owner preference
   (`zhipuai` for `glm-*`, `deepseek` for `deepseek-*`, `openai` for `gpt-*`, `moonshotai` for `kimi-*`)
   so a reseller (`openrouter/deepseek-chat`) does not shadow the canonical metadata.
3. **Unmatched.** No catalog hit → defaults apply (see below). `takeover` warns.

### Catalog cache (size-aware — stores a slim projection, never the raw blob)

`api.json` is 3.05 MB raw but ~286 KB gzipped and `304` is 0 bytes — so the network cost is bounded
(one ~286 KB transfer on first cache / content change; free 304s thereafter). To keep **disk + per-
invocation parse** light, we cache only a slim, **deduplicated** projection (the full-blob parse runs
solely on a `200` refresh, never on the load path).

- File `~/.model-proxy/models_cache.json`, atomic tmp+rename (mirrors `persist`/`savePool`):
  ```json
  {
    "fetched_at": "<RFC3339>",
    "etag": "\"9287e5…\"",
    "by_endpoint": {"https://open.bigmodel.cn/api/paas/v4": ["glm-4.6", "glm-5.2"]},
    "by_name":     {"glm-4.6": {"c": 204800, "o": 131072, "i": ["text"], "oo": ["text"]}}
  }
  ```
  - `by_name` — deduplicated (244 unique models, ~30 KB); reseller duplication collapsed. On a name
    collision the canonical owner wins (`zhipuai` for `glm-*`, `deepseek` for `deepseek-*`, `openai`
    for `gpt-*`, `moonshotai` for `kimi-*`).
  - `by_endpoint` — each models.dev provider's normalized `api` URL → its model-name list (endpoint
    scoping for the first match step).
  - Total ~150 KB on disk (not 3 MB); load + unmarshal ≈ 2–4 ms per CLI invocation.
- TTL **24h** (gates the *check*, not the download). `ensureCatalogFresh()`:
  - cache present + age < 24h → load projection, build in-memory indexes (**no network**).
  - else conditional GET `api.json` with `Accept-Encoding: gzip` (automatic) + `If-None-Match: <etag>`:
    - `304` → keep projection, update `fetched_at` only (0 bytes).
    - `200` → gunzip+parse body → rebuild `by_endpoint` + `by_name` → rewrite cache + etag + fetched_at.
    - non-2xx / network error → stale cache: use it + stderr note ("models.dev unreachable, using
      catalog cached <age> ago"); no cache: proceed with empty catalog (every model → default) so the
      command still runs.
- HTTP client: 10s timeout (mirrors codex `FetchModels`). Injectable fetch func for tests (httptest
  server + fixture, honors `If-None-Match`); no live network in unit tests.

### Defaults (unmatched models)

When neither config nor the catalog has metadata for a model:

```go
defaultProviderModel = ProviderModel{
    Context: 200000,
    Output:  16384,
    Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}},
}
```

`takeover` writes these defaults to the client config **and** prints a stderr warning per unmatched model:

```
warning: model gpt-5.5 at codex: no models.dev metadata — wrote defaults (ctx=200000 out=16384 text-only)
```

(Defaults are conservative so a client is never given a zero/nonsense limit. Tunable later if needed — YAGNI for now.)

### CLI surface

- **`model-proxy models` / `models <provider>`** — display now shows the *effective* set (config ∪
  routes) with resolved metadata, and a **source marker** per model so the supplement is visible:
  `[cfg]` (config), `[dev]` (models.dev), `[def]` (default/unmatched). Auto-calls
  `ensureCatalogFresh()`. **Never writes `config.yaml`.**
- **`model-proxy models pull`** (new, no provider arg) — force-refresh the catalog cache from models.dev
  (one global catalog). Distinct from the existing per-provider `models refresh <provider>` (upstream
  live `/models` list, unchanged).
- **`takeover <client>`** — auto-calls `ensureCatalogFresh()`, resolves effective metadata per route
  target, writes client config; emits the unmatched-default warning for any `[def]` model. Applies where
  metadata is actually written (opencode/pi; `claude` writes only env vars, `codex` writes provider
  config — verify per-client during impl, warn only where fields are written).

### Hydration placement (scope confinement)

- A function `hydrateModels(cfg, catalog)` materializes the effective `Provider.Models` for every
  provider (config ∪ routes, resolved via precedence). It mutates the in-memory `cfg` only.
- It is called **only** in the `models` and `takeover` CLI entry points — **never** in `LoadConfig`,
  `NewProxy`, `buildProviders`, or the daemon. So the proxy hot path and daemon reloads gain **zero**
  models.dev dependency (no network, no cache read). `doctor`/`usage` continue to read `cfg.Models`
  as loaded; since they run as offline CLI diagnostics they will show config models as-is (if the
  operator removed `models:`, those commands show fewer models — acceptable, they are config diagnostics,
  and a future tweak can call `hydrateModels` there too if desired).

## Decisions (confirmed with user)

- **Effective set source = routes-derived** (config ∪ routes). models.dev supplies metadata only.
- **Matching = endpoint-first (URL then host), then global name suffix, then default.**
- **Precedence = config > models.dev > default.** config is authoritative; models.dev only supplements
  gaps; it never overrides config values.
- **`config.yaml` is never written.** The operator removes `models:` blocks by hand if desired; the
  feature works either way.
- **Unmatched at takeover = warn + write defaults** (not omit). Defaults `ctx=200000 out=16384 text-only`.
- **Cache maintained by both `models` and `takeover`** (`ensureCatalogFresh`, 24h TTL, ETag conditional
  GET). Plus explicit `models pull` force-refresh. No daemon background fetch.
- **Scope confined to `models` + `takeover` CLI paths.** No daemon/proxy/hot-path coupling.

## Testing (white-box, stdlib + httptest, ≥80% coverage)

Per the repo's exact-value contract — assert precise values, not "non-empty".

- **Catalog fetch + cache** — httptest server serving a fixture `api.json` (gzip + honoring
  `If-None-Match`): assert the cache file holds the **slim projection** (`by_endpoint` + `by_name`), not
  the raw blob; `304` updates `fetched_at` only (projection byte-identical); `200` rebuilds it. Assert
  **dedup**: a fixture with the same model under two providers yields one `by_name` entry. Assert
  `etag`/`fetched_at` round-trip. 10s timeout respected.
- **Endpoint match** — cfg zhipu provider (`open.bigmodel.cn/api/paas/v4`) + fixture with `zhipuai`
  having the same `api`: assert `glm-5.2` metadata sourced from `zhipuai` (exact-URL hit), exact
  `context`/`output` values. deepseek provider likewise (`api.deepseek.com` → `deepseek`).
- **Host-only match** — provider base URL with a trailing path models.dev lacks (e.g. `…/v1`) → assert
  host match still scopes correctly.
- **Name fallback** — `aqp` provider (endpoint absent from catalog) + model `deepseek-v4-pro`: assert
  metadata sourced from `deepseek/deepseek-v4-pro` via suffix, *not* zero.
- **Canonical-owner preference** — fixture with both `openrouter/deepseek-chat` and `deepseek/deepseek-chat`:
  assert the `deepseek` owner wins.
- **Unmatched → default + warning** — `codex`/`gpt-5.5` absent from fixture: assert effective
  `ProviderModel` equals the defaults exactly; assert `takeover` emits the stderr warning (capture via
  `os.Pipe`/test helper) and writes defaults to the generated client config.
- **Precedence (config wins)** — cfg with `models: {glm-5.2: {context: 999}}` + catalog with different
  value: assert effective value is `999` (config), catalog ignored for that model; assert a config-absent
  route model still pulls from catalog.
- **Effective set (config ∪ routes)** — cfg with a `models:` entry *not* in routes + a route model *not*
  in config: assert both present; assert `volcengine` (no routes, config models present) shows config
  models.
- **Offline fallback** — fetch returns 500 + stale cache present: assert stale cache used + note logged;
  500 + no cache: assert empty catalog (all models default) + command still succeeds.
- **No config.yaml mutation** — snapshot `config.yaml` mtime/content before `models`/`takeover`; assert
  unchanged after.

## Out of scope (YAGNI)

- Writing models.dev data back into `config.yaml` (explicitly rejected — supplement only).
- `models.dev` override of config values (config is authoritative).
- Background/daemon catalog refresh (only `models`/`takeover` maintain the cache).
- `GET /v1/models` enrichment (still route keys only; separate concern).
- `doctor`/`usage` hydration (offline diagnostics; show config as-is — can revisit).
- `catalog.json` / `models.json` endpoints (`api.json` suffices).
- Interactive yes/no prompt for unmatched at takeover (warn + continue instead).

## Open implementation notes

- Confirm per takeover client which fields carry metadata (opencode/pi yes; verify `codex`/`claude`) and
  emit the warning only where defaults are actually written.
- Cache file lives under `~/.model-proxy/` alongside other state (`token_usage.json`, `quota_state.json`,
  pool files) — reuse the existing `configDir()` / atomic-write helpers.
- `models pull` subcommand parsing fits the existing `cmdModels` arg dispatch (`models.go:28`); add a
  `pull` branch alongside `refresh`.
- Update `CLAUDE.md` (models bullet + provider section) and `AGENTS.md` with the models.dev contract and
  the no-config-write / supplement-only invariant.
