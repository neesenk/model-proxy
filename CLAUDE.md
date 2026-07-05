# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository layout

The git repo root (`/Users/zhiyong.liu/model-proxy`) contains docs (`AGENTS.md`, `.gitignore`) and the Go module in the **`model-proxy/`** subdirectory. All `go` commands run from there:

```bash
cd model-proxy
go build -o model-proxy .                          # build
GOOS=linux GOARCH=amd64 go build -o model-proxy-linux .   # cross-compile Linux
go test ./...                                      # run all tests (~24s)
go test -run TestForward_ProviderRouting .         # run one test (white-box, package main)
go vet ./...                                       # lint
```

Tests are white-box (`package main`) using only the stdlib `testing` + `httptest` — no testify. The `provider/` package has test files for deepseek + volcengine (auth + rewrite); other providers are tested through the proxy in `package main`.

`AGENTS.md` (repo root) is the authoritative deep-dive: reverse-engineered Compass/codex/Zhipu/DeepSeek/Volcengine backend contracts, the codex OAuth device-flow sequence, Volcengine V4 signing, and a gotchas log. Read it before touching provider auth or request rewriting.

## Architecture

`model-proxy` is a multi-provider LLM reverse proxy. Clients speak Anthropic or OpenAI protocol; the proxy forwards using the **same protocol the client used (no conversion)**, swapping in real credentials and mapping model aliases to upstream model names.

### Two-layer config (`config.yaml`)

- **`providers`** — upstream backends. Each has an `openai_base_url` (default base; OpenAI protocol + `/models` + `usage`), an optional `anthropic_base_url` (overrides for anthropic requests), a `provider_id` (selects the `provider/` implementation), a `models` map (real model names → context/output/modalities), an optional multi-segment `peak_hours` (single `"HH:MM-HH:MM"` string / list of strings / list of `{window, multiplier}`; peak discounts effective remaining via the per-segment multiplier), and an optional `billing: pay-as-you-go` (default `plan`; payg is strict last-resort). Provider-specific fields live here (`cqp_mint_url` for compass, `usage_url` for zhipu).
- **`routes`** — exposed model name → ordered list of `provider/model` targets (`RouteTarget`: `provider`, `model`, `priority`). Not keyed by protocol: either protocol may reach any exposed model. `schedule` (a `Proxy` method) orders targets by **`(billing tier: plan < unknown < payg, effective_remaining desc = RemainingPct/peak_multiplier, priority asc)`**, then applies sticky routing + circuit-breaker filtering, and the proxy fails over to the next on connection error / 401-after-refresh / 5xx / 429.
- **`claude_mapping`** — anthropic-only: maps a client `claude-*` name to an exposed model name, consulted before route lookup (two-step). If a called anthropic model isn't listed, the called name is used as the exposed name. Openai skips this step.

`usage` without a provider arg shows usage for all configured (logged-in) providers, separated by a divider line (like `models` without args). `models refresh <provider>` delegates to `p.FetchModels()` — provider-owned, not special-cased in `models.go`.

### Protocol routing (`proxy.go`, `config.go:protocolForPath`)

Path prefix selects the protocol, which selects the upstream path + base URL (the route itself is protocol-agnostic):
- `/v1/messages` → anthropic
- `/v1/responses`, `/v1/chat/completions` → openai
- `GET /v1/models` → union of `routes` keys (exposed names) + `claude_mapping` keys (OpenAI-style)

**Strip the client's `/v1` prefix before forwarding** — provider base URLs already include the version segment (e.g. `…/compass-api/v1`), so naively appending the client path produces a double `/v1`.

**Per-protocol base URLs**: each provider has `openai_base_url` (the default base, used for the OpenAI protocol + `/models` + `usage`) and an optional `anthropic_base_url` that overrides it for anthropic (`/v1/messages`) requests. `forward` selects the base by protocol (anthropic→`anthropic_base_url` if set, else `openai_base_url`; openai→`openai_base_url`).
- **Openai**: the proxy strips the client's `/v1` prefix (provider base URLs include their own version segment, e.g. `…/v3`, `…/paas/v4`).
- **Anthropic**: the proxy keeps the client's `/v1/messages` path intact — matching the official Anthropic SDK convention (`base_url + /v1/messages`). So `anthropic_base_url` should NOT include `/v1`.

### Provider abstraction (`provider/provider.go`)

`Provider` interface = `AuthHeaders`, `Refresh`, `RewriteRequest`, `Login`, `Logout`, `Usage`, `FetchModels`, `Quota`. Implementations register themselves in `init()` via `Register(providerID, constructor)`. `provider.New` dispatches by `provider_id`.

The `provider/` package stays decoupled from `main`: the main package wires its existing auth + login/usage functions into the provider via callbacks (`Config.Auth` `Authenticator`, `LoginFn`/`LogoutFn`/`UsageFn`/`FetchModelsFn`) in `proxy.go:buildProviders`. `authAdapter` bridges `main.AuthProvider` → `provider.Authenticator`. `FetchModels` returns the upstream's live model IDs — Bearer-based providers use the shared `fetchModelsBearer` helper (`provider/fetch_models.go`); volcengine uses a `FetchModelsFn` callback (V4-signed `ListArkAgentPlanModel`). Existing providers:
- `compass` — SSO cookie → mints CQP Bearer key; `RewriteRequest` adds `?beta=true` to `/messages`.
- `codex` — OAuth device flow; `RewriteRequest` injects `store:false`; `FetchModels` hardcodes `["gpt-5.5"]` (the codex backend's `/models` needs a `client_version` param, not worth querying).
- `zhipu` — embeds `ApiKeyBase` (shared file-stored Bearer); `Usage` delegates to the main package's `showGenericUsage` (parses the BigModel quota format or lists models). `FetchModels` uses `fetchModelsBearer`.
- `deepseek` / `volcengine` — embed `ApiKeyBase`; dual auth (`Authorization: Bearer` + `x-api-key`) so one key serves both protocol bases (OpenAI + Anthropic-compatible); `RewriteRequest` no-op (the proxy selects the base by protocol). Volcengine Ark "Agent Plan" uses plan-specific bases (`/api/plan/v3`, `/api/plan/compatible/v1`); its 5h/daily/weekly/monthly quota (`GetAFPUsage`) is a signed control-plane OpenAPI needing Volcengine AK/SK + V4 signing (`volcengine_sign.go`, wired via `login volcengine` + `usage volcengine`), not the Ark API Key.

### Auth strategies (`auth.go`)

`AuthProvider` (`Inject` + `Refresh`) has three implementations, selected by `provider_id` in `newAuthProvider`:
- **CQP** (compass) — reads SSO cookie from `~/.model-proxy/<name>_oauth_auth.json`, POSTs to `cqp_mint_url` to get a Bearer key, cached 50 min.
- **codex OAuth** — reads the proxy's *own* tokens (NOT the codex CLI's `~/.codex/auth.json` — using a separate OAuth client avoids refresh_token rotation contention), refreshes via `refresh_token`, parses `exp`/`chatgpt_account_id` from JWTs.
- **apikey** (zhipu, deepseek, volcengine) — reads `api_key` from `~/.model-proxy/<name>_apikey.json`. Volcengine also stores Volcengine AccessKey/SecretKey for V4-signed control-plane calls (GetAFPUsage).

On upstream **401**: `forward` calls `Refresh()` and retries **once** (`proxy.go`, attempt loop). Forwarded headers use a **whitelist** (`copyHeaderWhitelist`) — client `Cookie`/`Authorization` are never passed upstream.

### Request flow (`proxy.go:forward`)

read body → `extractModel` → **two-step lookup**: anthropic translates the called name via `claude_mapping` (if present) to an exposed name (openai uses the called name directly) → `routes[exposed]` → `schedule` (sticky provider first if available+within dwell, else best available by non-peak then priority; open-circuit/rate-limited providers skipped) → `takeHalfOpenSlot` → for each target (failover): `rewriteModel` → strip `/v1` → select base URL by protocol (`anthropic_base_url` if set else `openai_base_url`) → `provImpl.RewriteRequest` → copy whitelisted headers → `AuthHeaders` → set compass headers if `openai_base_url` contains "compass" → `client.Do` (with `upstream_timeout`) → on 2xx/4xx commit + stream; on conn-error/timeout/5xx `recordFailure` (circuit) + next; on 429 `recordRateLimit` (Retry-After or default backoff) + next; on 401 refresh+retry same target then failover. All targets exhausted → 502.

`flushCopy` streams SSE chunk-by-chunk and **breaks on the first write error** (client disconnect). Upstream requests use `context.WithTimeout(r.Context(), upstream_timeout)` so a hanging upstream fails fast into the circuit/failover path.

### Failover health & sticky routing (`scheduling:` config, `proxy.go`)

Per-provider runtime state (`Proxy.health`, guarded by `healthMu` — separate from the reload `mu`); quota snapshots live in `quotaTracker` under a third lock (`quotaMu`). **Lock ordering: `healthMu` → `quotaMu`** (never the reverse).
- **Circuit breaker** (timeout / 5xx / conn-error / 401-after-refresh): `consecutiveFailures++`; at `circuit_threshold` (3) the circuit opens for `circuit_cooldown` (10m), then **half-open allows 1 probe** (single-flight via `halfOpenInFlight`) — success closes, failure re-opens. An open / half-open-busy provider is skipped by `schedule`.
- **Rate-limit skip** (429): the provider is skipped until `Retry-After` (seconds or HTTP-date) or `rate_limit_backoff` (60s) elapses. Does not count toward the circuit. Covers long quota windows (5h/weekly) and short frequency limits — the duration comes from the response.
- **Sticky dwell** (`sticky_dwell`, 10m): each route parks on a "current" provider; `schedule` tries it first while available and within the dwell window — a conversation stays on one provider (prompt-cache friendly) and doesn't bounce back to a recovered priority-1 provider until the dwell expires. The dwell resets only when the sticky provider becomes unavailable (circuit/rate-limit) or the dwell expires. (`sticky_dwell` ≈ 2× the ~5m prompt-cache TTL: keeps an active conversation's cache warm and rides out a blip, but returns to the preferred provider in bounded time.)
- **Quota-aware scheduling** (`quota.go`): a `quotaTracker` goroutine polls each provider's `Provider.Quota()` (the `QuotaFn` callback, wired in `buildProviders` — mirrors `UsageFn`) every `scheduling.quota_poll_interval` (5m), caches `provider.QuotaSnapshot`s in memory + atomically persists `~/.model-proxy/quota_state.json` (loaded at boot as a baseline). Started by `NewProxy`; `reload` keeps the tracker (it reads cfg/providers via snapshot closures) and kicks a fresh `pollAll`; a 429 triggers an async `refreshOne` for that provider so its quota is fresh when the rate-limit clears. The tracker owns `quotaMu` (independent of `healthMu` and the reload `mu`). **Lock ordering: `healthMu` → `quotaMu`** — `schedule` calls `allSnapshots()` (quotaMu RLock) *before* `healthMu.Lock()` and never nests them the other way. `schedule()` then ranks available targets by `(tierRank, effective_remaining desc, priority asc)` where: `tierRank` maps `BillingClass` to **plan(0) < unknown(1) < payg(2)** (the `BillingClass` iota `Unknown=0,Plan=1,PayG=2` does **not** match scheduling order, so `tierRank` is used); `effective_remaining = RemainingPct / peak_multiplier` (for unknown/no-data snapshots it's `1.0/mult` — neutral, but peak still discounts). Per-provider quota sources: zhipu (`quota/limit`, TIME_LIMIT/MCP-tool windows excluded from binding), codex (`wham/usage`), volcengine (`GetAFPUsage`, needs AK/SK), compass (`monthly_usage`) → **plan**; deepseek (`/user/balance`) → **pay-as-you-go**. After `sticky_dwell` elapses, the route switches to the best provider only when the best wins on **tier → quota margin (`scheduling.quota_switch_margin`, 15 pts) → priority** — staying when the best's sole edge is a sub-margin quota gain. `billing: pay-as-you-go` (default `plan`) marks a provider strict last-resort. Staleness guard: a snapshot older than `3×poll_interval` is treated as `BillingUnknown`; a provider whose `Quota()` fails is `BillingUnknown` (ranked by priority, never treated as payg).

### Usage display (`cmdUsage` → `p.Usage()` → provider `UsageFn`)

Each provider's `Usage()` delegates to a main-package display function (wired in `buildProviders`):
- **compass** — `showCompassUsage`: CQP-cookie-authed POST to `monthly_usage` (project_id in body); shows account, plan, balance, usage ratio.
- **codex** — `showCodexUsage`: GET `/backend-api/wham/usage` with Bearer + `originator`; shows credits, rate-limit windows (primary/weekly), spend-control progress bar.
- **zhipu** — `showGenericUsage`: GET `usage_url` (BigModel quota `/api/monitor/usage/quota/limit`); parses `{data:{limits:[{type,unit,percentage,nextResetTime,usage,currentValue,remaining,usageDetails}]}}`. Shows 5h/weekly token limits + monthly time limit with bars; `TIME_LIMIT` breakdown by MCP tool (`search-prime`/`web-reader`/`zread`). Fallback: OpenAI-style model list.
- **deepseek** — `showDeepseekUsage`: GET `usage_url` (`/user/balance`); shows `is_available` + per-currency `total/granted/topped-up` balance.
- **volcengine** — `showVolcengineUsage`: if AK/SK configured → V4-signed `GetAFPUsage` (Volcengine OpenAPI, `volcengine_sign.go`); shows `AFPFiveHour/Daily/Weekly/Monthly` (Quota/Used/Remaining/ResetTime). `models refresh volcengine` → V4-signed `ListArkAgentPlanModel` via `FetchModelsFn` → `Result.Datas[].ModelID` (17 models incl. doubao/glm-5.2/kimi/minimax/deepseek). Else → config models + note.

Reset times display as `<duration>(at <time>)` — `formatResetAt` shows `HH:MM` if today, `MM-DD HH:MM` otherwise.

### Daemon (`daemon.go`, `daemon_unix.go`, `daemon_windows.go`)

`serve daemon` runs a detached **supervisor** that supervises a **worker** (the actual `http.ListenAndServe`). Roles are selected by the `AIS_SWITCH_PROXY_ROLE` env var (no new subcommand): supervisor spawns worker → waits → restarts with exponential backoff (reset after 30s sustained uptime). `stop`/`reload` read the pid file (derived from the log path) and signal the supervisor; the supervisor forwards SIGHUP to the worker for **hot config reload** (`Proxy.reload` rebuilds providers under a `sync.RWMutex`).

The worker's stdio is the log file, so `color.go`'s tty check auto-disables color → file logs stay escape-free. Foreground mode mirrors logs to the file via `MultiWriter` and explicitly sets `logColorEnabled = false`.

### Takeover (`takeover.go`, `clients.go`)

`takeover <client>` backs up a client's config file (verbatim copy + sha256 meta, idempotent) into `<configDir>/.model-proxy/`, then rewrites it to point at the proxy. Clients: `claude` (env vars), `opencode`/`codex`/`pi` (provider config with `provider_id` from `takeover.provider_id`). `restore` copies the backup back. opencode's baseURL needs `/v1`; pi's must NOT have it (pi appends `/v1/messages` itself) — see `clients.go`.

## Credentials convention

Credentials are managed by `login`/`logout`, stored at `~/.model-proxy/<providerName>_<suffix>.json`, and **never put in config.yaml**. Suffix by provider: compass/codex → `oauth_auth`, zhipu/deepseek/volcengine → `apikey`. The volcengine `apikey` file stores `{api_key, access_key, secret_key}` — the API Key for chat (Bearer) + the Volcengine AK/SK for `GetAFPUsage` (V4 signing). The provider *name* (config top-level key, not `provider_id`) derives the path, so multiple instances of the same `provider_id` (e.g. `zhipu-personal`, `zhipu-work`) get separate credential files.

## Adding a new provider

1. Create `provider/xxx.go` implementing `Provider` (embed `ApiKeyBase` if it's a file-stored API key; use `fetchModelsBearer(p.cfg)` for `FetchModels` if it has an OpenAI-style `/models`). A new **plan** provider should also implement `Quota()` — write a `fetchXxxQuota` parser in `main.go` returning `*provider.QuotaSnapshot` (Billing=`BillingPlan`, `RemainingPct`=`BindingRemaining(windows)`), and wire it via `pcfg.QuotaFn` in `buildProviders`. Without `Quota()`, the provider is `BillingUnknown` (ranked by priority). A pay-as-you-go upstream instead sets `billing: pay-as-you-go` in config (no `Quota()` needed).
2. `Register("xxx", constructor)` in `init()`.
3. Add a `provider_id: xxx` entry under `providers:` in `config.yaml`, plus route entries.

You should **not** need to edit `proxy.go`, `login.go`, `logout`, `usage`, or `models.go` — the routing/auth/CLI/model-list are provider-agnostic. To reuse an existing implementation for a new upstream (e.g. DeepSeek on zhipu's apikey flow), just add a config entry with `provider_id: zhipu` and a different `openai_base_url`/`usage_url`.

## Cross-cutting gotchas

- **codex backend**: requires `store:false` (injected by `CodexProvider.RewriteRequest`), `stream:true`, and rejects `max_tokens`. Needs `originator: codex_cli_rs` header (else 403) and `ChatGPT-Account-Id` (parsed from id_token JWT). Usage endpoint is `/backend-api/wham/usage`, **not** under `/codex/` (that returns 403).
- **compass**: SSO cookie value already includes the `SSO_C=` prefix — use it as the whole `Cookie:` header value, don't re-prefix. `/v1/messages` needs `?beta=true`, `anthropic-version: 2023-06-01`, and a UUID `x-compass-request-id`. `monthly_usage` is **POST** with `project_id` in the body (field names are snake_case: `total_amount`, not `totalAmount`).
- **Logging hygiene**: mask SSO cookies with `mask()` (first2…last2); log `auth/info` response bodies by length only (they carry identity). When logs go to a file, color must be off.
- **supervisor nil panic**: `spawnWorker` can return nil on failure — `runSupervisor` checks before using the result.
- **Volcengine V4 signing** (`volcengine_sign.go`): the credential-scope terminator is **`request`** (not `volcengine_request` — the official signing demo is authoritative). Signed headers are `host;x-date` only — do NOT sign or send `x-content-sha256` for GET (it causes "Invalid Authorization"). The signing-key chain: `HMAC(sk→date→region→service→"request")`. The Agent Plan's `GetAFPUsage` needs AK/SK (not the Ark API Key); the Ark API Key is Bearer chat-only.
