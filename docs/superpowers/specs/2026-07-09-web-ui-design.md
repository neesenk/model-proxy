# Web UI — config editing, account management, service status

**Date:** 2026-07-09
**Status:** Design (awaiting review)
**Scope:** `model-proxy/` Go module

## 1. Goal

Add a browser-based admin UI to the running `model-proxy` daemon, on the same
port it already listens on, that lets the operator:

1. **Edit `config.yaml`** — a hybrid of structured forms (common ops) and a raw
   YAML editor (power-user escape hatch), with validate → backup → atomic write →
   hot reload.
2. **Manage provider accounts** — view all accounts (keys masked), and add/remove
   accounts for **every** provider including the async `aqp` (SSO) and `codex`
   (OAuth device) flows, by porting the existing CLI login functions to HTTP.
3. **View service status** — daemon uptime + version, per-provider health
   (circuit/rate-limit state), quota/usage, schedule, plus **request counters and
   per-provider/account token usage** (input/output/cache) parsed live from
   response streams, and a log tail.

The proxy already binds `127.0.0.1` and serves `/debug/schedule`; this design
extends that surface.

## 2. Non-goals

- **Multi-user / remote access.** The UI is local-trusted, no auth (user's
  explicit choice), on the existing loopback-only bind. A kill-switch
  (`web.enabled: false`) is the only mitigation offered.
- **A frontend framework / build step.** Vanilla JS embedded via `//go:embed`,
  matching the stdlib-only, single-binary ethos (only `gopkg.in/yaml.v3` dep
  today). No React/Vue/Node toolchain.
- **A separate UI process.** All state the UI needs (health, quota, sticky,
  counters, tokens, login sessions) lives in the worker — a second process would
  fragment token accounting (only the proxy sees streams) and the async login
  flows. Ruled out in the approach phase.
- **Per-request body rewriting to force OpenAI usage.** Token counting is
  best-effort for OpenAI-protocol streams (see §10); injecting
  `stream_options.include_usage` is a deferred enhancement, not v1.
- **Persisting runtime counters across restart.** Request/failover/429 counters
  are "since boot" (reset on restart), by convention. Token *usage* is persisted.

## 3. Architecture & integration

The UI runs **inside the existing daemon worker** (`runProxy` in `daemon.go`).
After `NewProxy(cfg)`, construct a `webServer` and register it on the same mux:

```go
mux := http.NewServeMux()
mux.HandleFunc("/ui/", web.serveUI)   // embedded static SPA
mux.HandleFunc("/api/", web.serveAPI) // JSON data + mutations
mux.HandleFunc("/", p.handler)        // unchanged: /v1/*, /debug/schedule, /health
```

Go's `ServeMux` gives precedence to more specific patterns, so `/ui/` and `/api/`
win over `/` with no change to `p.handler`. The proxy's existing paths are
untouched.

### New files (all `package main`, white-box testable)

- `web.go` — `webServer` struct + `/api/*` + `/ui` handlers, routing, request
  validation, the shared `saveAndReload` pipeline.
- `tokens.go` — token-accounting subsystem: the SSE-scanning pass-through reader
  + `tokenCounter` store + persist to `~/.model-proxy/token_usage.json`.
- `metrics.go` — request/failover/429/failure counters + daemon start time, using
  `atomic.Uint64` on the hot path.
- `login_session.go` — in-memory `loginSessionStore` for the async aqp/codex
  login flows (holds the aqp `AqpClient` jar / codex device-code state, TTL + GC).
- `web_assets/` — embedded via `//go:embed web_assets/*`: `index.html`, `app.js`
  (small modules), `styles.css`.

### Config knob

A new optional `web:` section — a single kill-switch, default on:

```yaml
web:
  enabled: true   # set false to disable the /ui + /api endpoints
```

### `webServer` struct — no global state

```go
type webServer struct {
    p          *Proxy         // reload, health, quota, schedule, tokens, metrics
    configFile string         // read/write config.yaml + .bak
    sessions   *loginSessionStore
    tokens     *tokenCounter  // == p.tokens
    metrics    *metricsStore  // == p.metrics
}
```

### Locking discipline (the codebase is strict about this)

All new locks are **independent leaf locks**, never nested with the existing
`healthMu`/`quotaMu`/`mu`:

- Read proxy state under the existing locks, mirroring the snapshot-then-release
  pattern already used by `scheduleStatus()` (`p.mu.RLock` for cfg/providers,
  `healthMu` for health/sticky, `quota.allSnapshots()` for quota).
- Token counter: own `tokenMu`. Metrics: `atomic.Uint64` fields (lock-free on the
  hot path; lock only for snapshot). Login sessions: own `sessionMu`.
- **Config write** is the one place the web layer mutates the live proxy: atomic
  file write → `p.reload(configFile)` (takes `p.mu.Lock()` internally). The web
  handler never holds a proxy lock during file I/O.

Existing lock ordering (`healthMu` → `quotaMu`) is unchanged; the new locks are
never acquired while holding an existing one.

## 4. API surface

All JSON. Mutations funnel through the shared pipeline (§5). Errors: 4xx for
bad input (400 with the validation message), 5xx for internal failure.

### Reads (`GET`)

| Endpoint | Returns |
|---|---|
| `/api/status` | Dashboard: `uptime`, `version`, `listen`; per-provider `health` (`circuit_state` closed\|open\|half_open, `circuit_until`, `rate_limited_until`, `available`); quota snapshots; `schedule` (reuses `scheduleStatus()`); per-provider `counters` |
| `/api/config` | `{yaml: "<raw text>", summary: {providers, routes, scheduling, claude_mapping}}` |
| `/api/accounts` | `{providers:[{name, provider_id, billing, accounts:[{id, label, added_at}]}]}` — **all keys masked** |
| `/api/tokens` | Per `{provider, model}`: `input, output, cache_creation, cache_read, requests` (virtual provider name encodes the account → per-account granularity) |
| `/api/logs?tail=N` | Last `N` lines of `log_file` (default 200, capped) |

### Mutations (`POST`/`DELETE`)

**Config (hybrid editor):**

- `POST /api/config` — body `{yaml}`: full raw-YAML replace → pipeline.
- `POST /api/config/edit` — body `{kind, name?, data}`, `kind` ∈ `general`,
  `scheduling`, `provider`, `route`, `claude_mapping` (see §5). YAML tab is the
  escape hatch for anything not covered.

**Accounts:**

- `POST /api/accounts/<provider>` — for apikey-type providers:
  `{api_key, access_key?, secret_key?, label, replace?}` → validate against
  `usage_url` → `savePool` → reload. Synchronous.
- aqp/codex (single-credential, async) — three-step, server holds flow state:
  - `POST /api/login/<provider>/start` → codex: `{session_id, verify_url, user_code}`;
    aqp: `{session_id, login_url}` (server bootstraps, holds the cookie jar in
    the session)
  - `GET /api/login/<session>/poll` → `{state: pending|done|error, ...}`; on
    `done` the credential is already saved + reload triggered
  - `POST /api/login/<session>/cancel`
- `DELETE /api/accounts/<provider>/<account_id>` — remove from pool (apikey-type)
  or clear the single credential file (aqp/codex) → reload.

### Optional

- `POST /api/tokens/reset` — zero token counters (nice-to-have; deferred unless
  trivial).

## 5. Config editing (hybrid)

**The comment-preservation problem.** `config.yaml` is comment-rich and
hand-maintained. Round-tripping a structured edit through the Go `Config` struct
(`yaml.Marshal`) **strips comments and reorders fields**. To avoid this:

- **YAML tab**: raw text → validate (unmarshal to check) → write the raw bytes
  as-is → reload. Whatever the user typed is preserved verbatim.
- **Structured forms**: **node-tree editing** via the `yaml.v3` `yaml.Node` API,
  which preserves comments and ordering. A typed editor applies one targeted
  mutation to the node tree → re-encode the node tree → validate → write →
  reload.

### Unified tail pipeline

Both paths funnel through one function:

```go
// saveAndReload validates, backs up, atomically writes, then hot-reloads.
// It never touches the live Proxy until reload; validation failure returns an
// error and writes nothing.
func (w *webServer) saveAndReload(newBytes []byte) error {
    if _, err := LoadConfigFromBytes(newBytes); err != nil {
        return err                       // 400 — validate only, no write
    }
    backup(w.configFile, w.configFile+".bak")
    atomicWrite(w.configFile, newBytes)  // tmp + os.Rename
    if err := w.p.reload(w.configFile); err != nil {
        restore(w.configFile+".bak", w.configFile)  // defensive; reload shouldn't fail post-validate
        return err                       // 500
    }
    return nil
}
```

`LoadConfigFromBytes` is a small refactor of `LoadConfig` that takes bytes (the
path expansion of takeover/log paths still applies; `--config` path is
unchanged). `atomicWrite` mirrors `savePool`'s tmp+rename.

### Structured-edit vocabulary

`POST /api/config/edit` body `{kind, name?, data}`. Each `kind` is a typed,
unit-testable editor on the node tree:

| `kind` | `data` | Effect |
|---|---|---|
| `general` | `{listen?, log_level?, log_file?}` | set top-level scalars |
| `scheduling` | `{threshold?, cooldown?, rate_backoff?, timeout?, dwell?, poll_interval?, switch_margin?}` | set under `scheduling:` |
| `provider` | name + `{openai_base_url?, anthropic_base_url?, usage_url?, billing?, peak_hours?, models?}` | set fields / add-remove model entries; `delete: true` removes the provider |
| `route` | name + `{targets: [{provider, model, priority}]}` | targets CRUD; `delete: true` removes the route |
| `claude_mapping` | `{alias, route}` / `{delete: alias}` | add/remove an alias |

Each editor loads the current node tree (fresh from disk, not the live cfg),
applies its mutation, re-encodes, and calls `saveAndReload`. The YAML tab is the
escape hatch for anything outside this vocabulary.

### Validation

`Config.validate()` (already strict) is the gate: unknown `provider_id`, route
target referencing a missing provider, malformed `peak_hours`, bad `billing`,
`anthropic_base_url` ending in `/v1`, duplicate priorities, etc. — all surface as
400 with the existing human-readable messages, **before** any write.

## 6. Account management & login flows

### View (`/api/accounts`)

Reads credential state per provider:
- apikey-type (`zhipu`, `deepseek`, `volcengine`): `loadPool(name, providerID)`
  → each account's `{id, label, added_at}`.
- `aqp`: `loadAccount(authFilePath("aqp","oauth_auth"))` → `{email, project_id}`
  as one account.
- `codex`: read the codex auth file → account id from the JWT.

**Every key is masked** via the existing `mask()` helper (first2…last2). Raw
secrets never appear in any response. POST bodies carrying keys are handled
in-memory and never logged (logging-hygiene rule from CLAUDE.md).

### Add — apikey-type providers

Reuse the existing login logic by extracting **non-printing cores** so both the
CLI and HTTP handler call the same function:

```go
// extracted from runApiKeyLoginWithInput; the CLI wrapper keeps the printing.
func addApikeyAccount(cfg *Config, name string, prov Provider,
    cred accountCred, label string, replace bool) (id string, err error)
// extracted from runVolcengineLoginWithInput.
func addVolcengineAccount(cfg *Config, name string, prov Provider,
    cred accountCred, label string, replace bool) (id string, err error)
```

These already take the key as a parameter (no stdin), validate against
`usage_url`, dedup by `accountIDFor` under `withPoolLock`, and `savePool`. The
HTTP handler calls the core then `p.reload()`. Synchronous; returns the saved
`{id, label}`.

### Add — aqp (async, single-credential)

The CLI flow is `BootstrapLoginURL` → (loopback or ENTER signal) → `PollSession`
→ `fetchAPIKey` → `saveAccount`. The web flow drops the loopback server (the
poll *is* the completion signal):

1. `POST /api/login/aqp/start`: create a `loginSession` with a fresh `AqpClient`
   (cookie jar), call `BootstrapLoginURL()` synchronously, store the client in
   the session, return `{session_id, login_url}`. Kick a goroutine that loops
   `checkSessionAt(aqpAuthInfo)` every 2s.
2. Goroutine success → `fetchAPIKey()` → `saveAccount()` → `p.reload()` → mark
   session `done` with the email.
3. `GET /api/login/<session>/poll`: return current `state` (+ email on done).

Because aqp is single-credential, "add" replaces the existing account.

### Add — codex (async, OAuth device flow)

1. `POST /api/login/codex/start`: `requestUserCode` → store
   `{device_auth_id, user_code, interval}` in the session; return
   `{session_id, verify_url, user_code}`. Kick a goroutine running the existing
   `pollForToken`.
2. Goroutine success → `exchangeCodeForTokens` → write the auth file (0600) →
   `p.reload()` → mark `done`.
3. `GET /api/login/<session>/poll`: return `state` (+ account_id on done).

Both async flows reuse **existing pure functions** (`BootstrapLoginURL`,
`checkSessionAt`, `fetchAPIKey`, `saveAccount`; `requestUserCode`, `pollForToken`,
`exchangeCodeForTokens`). Only the session store, the driving goroutines, and
the HTTP shape are new.

### `loginSessionStore`

```go
type loginSession struct {
    id        string
    provider  string
    state     string        // "pending" | "done" | "error"
    detail    string        // verify_url/user_code (codex) or login_url (aqp); or error msg
    result    string        // email/account_id on done
    aqpClient *AqpClient    // aqp only
    codex     *codexLoginState // codex only
    created   time.Time
    mu        sync.Mutex
}
```

A GC goroutine drops sessions older than 15 min. Sessions are in-memory only
(lost on restart; the user re-triggers the flow).

### Remove (`DELETE /api/accounts/<provider>/<id>`)

- apikey-type: extracted core `removeApikeyAccount(name, providerID, id)` —
  `withPoolLock` → load → filter out the id → save (or, if last account, the
  pool becomes empty; `logout --all` semantics). Then reload.
- aqp: `clearAccount(authFilePath("aqp","oauth_auth"))`.
- codex: remove `~/.model-proxy/codex_oauth_auth.json`.

All paths → `p.reload()`.

## 7. Status dashboard

`/api/status` assembles (all under the existing locks, snapshot-then-release):

- **Uptime / version / listen** — `metricsStore.startedAt` (set in `NewProxy`),
  a compile-time version constant, `cfg.Listen`.
- **Per-provider health** — from `Proxy.health` (under `healthMu`): derive
  `circuit_state` (`closed` / `open` if `now < circuitOpenUntil` / `half_open` if
  a probe is in flight), `circuit_until`, `rate_limited_until`, and `available`
  (the existing `providerHealth.available(now)`).
- **Quota snapshots** — `p.quota.allSnapshots()` (already computed, polled every
  5 min).
- **Schedule** — reuse `scheduleStatus()` verbatim (first-choice + ordered +
  sticky per route).
- **Counters** — from `metricsStore` (§8).

`/api/logs?tail=N` reads the last N lines of the resolved `log_file`
(`resolveLogFile`). Bounded N (cap ~1000) to avoid large reads.

## 8. Metrics counters (`metrics.go`)

Lock-free on the hot path (`atomic.Uint64`), locked only for snapshot. Reset on
restart (no persist).

```go
type providerMetrics struct {  // keyed by virtual provider name in metricsStore.m
    Requests       atomic.Uint64 // bumped when a forward attempt targets this provider
    Failovers      atomic.Uint64 // bumped when this provider is abandoned for the next target
    RateLimited429 atomic.Uint64 // bumped on a 429 from this provider
    Failures       atomic.Uint64 // bumped on 5xx / conn-error / timeout from this provider
    LastRequestAt  atomic.Int64  // unix seconds
}
type metricsStore struct {
    mu       sync.Mutex
    startedAt time.Time
    version  string
    m        map[string]*providerMetrics
}
```

Wired into `forward`, each attributed to the provider it concerns: `Requests++`
when a target is attempted (a client request that fails over counts on each
provider it touches — the intended "load per provider" signal); `Failovers++`
when advancing off a provider to the next; `RateLimited429++` on a 429;
`Failures++` on 5xx / conn-error / timeout. Each is a single atomic op —
negligible cost. Reset on restart (no persist).

## 9. Token accounting (`tokens.go`)

Per `{provider, model}` usage, persisted to `~/.model-proxy/token_usage.json`
(periodic flush + graceful shutdown, mirroring `quota_state.json`; loaded at boot
as baseline). The virtual provider name encodes the account, so this is
per-account granularity.

### The SSE-scanning pass-through reader

```go
type usageScanner struct {
    src    io.ReadCloser        // upstream body
    buf    []byte               // incomplete-line reassembly (bounded)
    acc    tokenUsage           // accumulated this stream
    key    tokenKey             // {provider, model} for commit-on-EOF
    sink   *tokenCounter
}
// Read reads from src, writes the SAME bytes to dst (via the caller's io.Copy),
// and observes the bytes to extract usage. It never modifies, buffers the
// stream, or blocks.
```

Design:
- As bytes flow to the client, the scanner *observes* them; it is strictly
  **pass-through** — the client receives bytes identical to upstream.
- A bounded line scanner (cap 64 KB/line; an over-long line is skipped for
  scanning but still passed through verbatim) reassembles `data: {...}` lines
  across read boundaries and extracts usage:
  - **anthropic**: `message_start` → `message.usage.{input_tokens,
    cache_creation_input_tokens, cache_read_input_tokens}`; `message_delta` →
    `usage.output_tokens`.
  - **openai**: top-level `usage.{prompt_tokens, completion_tokens,
    completion_tokens_details}`.
- On EOF (upstream close), the accumulated `tokenUsage` is committed **once** to
  `tokenCounter` for the request's `(provider, model)` — both known in `forward`
  at the successful-response point (the chosen target + `rewriteModel` result).

Fail-safe contract: a parse failure records no usage and **never** affects the
stream. It must be race-clean and byte-identical-passthrough (tested, §11).

### OpenAI limitation (documented)

OpenAI-protocol streams include `usage` only when the request sets
`stream_options.include_usage`. v1 is **best-effort**: the scanner does not
rewrite the request body. DeepSeek includes usage by default; others may not.
Per-request input/output may be partial for such providers; quota snapshots (§7)
remain the source of truth for totals. Injecting `include_usage` is a deferred
enhancement.

### Wiring

In `forward`, on the 2xx commit path, the upstream body is wrapped in a
`usageScanner` keyed by the chosen `(provider, model)` before `flushCopy`. The
scanner is only attached for streaming responses (non-streaming bodies are not
scanned in v1). A failed parse is silent.

## 10. Frontend (vanilla JS, `//go:embed`)

- `index.html` shell + nav (three tabs: **Status / Config / Accounts**) + a login
  modal.
- `app.js`: tiny hash router, `apiGet`/`apiPost` helpers, render functions per
  tab. Status tab auto-refreshes every few seconds (polls `/api/status` +
  `/api/tokens`).
- **Status tab**: provider health pills, quota bars, schedule table, counters,
  token totals, log tail.
- **Config tab**: collapsible structured forms per `kind` (§5) **+** a YAML
  textarea (monospace) with Save → inline validation-error display.
- **Accounts tab**: per-provider list with masked keys; **Add** (apikey form,
  or aqp/codex → modal showing URL + code and polling `/poll`); **Remove** per
  account (confirm).
- `styles.css`: minimal, readable, light/dark via `prefers-color-scheme`.

The visual build uses the **frontend-design** skill during implementation, not
in this spec. Frontend JS is not unit-tested (codebase convention: Go white-box
tests only); it is kept small and dependency-free.

## 11. Testing & coverage

White-box, stdlib `testing` + `httptest` only (no testify), exact-value
assertions — per the codebase's testing contract.

- **`web_test.go`** — each `/api/*` handler via httptest:
  - Exact JSON shape for status/config/accounts/tokens/logs.
  - Config YAML replace: invalid YAML → **400**, and assert `config.yaml` and
    `.bak` are byte-unchanged (no partial write).
  - Structured edit: assert the targeted field changed **and a comment survived**
    (yaml.v3 Node round-trip regression guard).
  - Accounts list: assert a key is **masked**, not raw (exact masked form).
  - Account add/remove: assert pool file state transitions + reload hook fired.
- **`tokens_test.go`** — the SSE scanner:
  - (a) byte-identical passthrough (every byte written == upstream bytes),
  - (b) correct extraction from anthropic `message_start`+`message_delta` and an
    openai `usage` chunk,
  - (c) usage event **split across N read boundaries** still extracted,
  - (d) an oversized line is skipped for scanning but passed through intact,
  - (e) persist/load round-trip; commit keyed by `(provider, model)`.
- **`metrics_test.go`** — counters increment on each `forward` event; snapshot
  matches; uptime monotonic.
- **`login_session_test.go`** — aqp + codex flows against the existing URL-param
  test hooks (`bootstrapAt`/`pollAt`/`fetchAPIKeyAt`, `codexLoginServerOptions`):
  assert pending→done, credential file saved, reload hook fired, session GC'd.
- **Concurrency** — `-race` clean; a token-counter-under-streaming test and a
  config-edit-during-request test (assert post-edit requests hit the new config —
  mirrors the existing reload-during-request contract).

Coverage: new files stay ≥80% (`scripts/cover.sh` gate). The handlers, scanner,
metrics, and node-editors are fully unit-testable. The async-login goroutine
*scheduler* is the intentional gap (like existing daemon/login paths); the state
machine is covered via the URL-param mock hooks.

## 12. Decisions & risks

| Decision / risk | Mitigation |
|---|---|
| SSE scanner is the highest-risk piece (proxy hot path) | Byte-identical-passthrough test + split-boundary test; strictly observe-only, fail-safe (no usage recorded → no stream effect) |
| yaml.v3 Node round-trip must preserve comments | Regression test asserts a comment survives a structured edit |
| OpenAI token counts best-effort | Documented; quota snapshots remain authoritative; `include_usage` injection deferred |
| Async login sessions in-memory only | Acceptable; user re-triggers on restart. 15-min TTL + GC prevents unbounded growth |
| No auth (user's choice) | `web.enabled: false` kill-switch; loopback-only bind unchanged |
| Metrics counters not persisted | Intentional ("since boot" convention) |
| Config write while a request is in flight | `p.reload` already handles this under `p.mu`; config-edit-during-request test asserts consistency |

## 13. Phasing (suggested delivery)

- **Phase 1 — usable panel**: web skeleton + `/api/status` (reads existing
  health/quota/schedule) + config editing (YAML + structured forms) + account
  CRUD (incl. aqp/codex async) + metrics counters + log tail. A complete admin
  panel.
- **Phase 2 — token accounting**: the SSE scanner + `tokenCounter` + `/api/tokens`
  + persist. The heaviest, most separable piece.

Both phases are in scope; phasing only affects delivery order so a useful panel
lands first. The `/api/tokens` endpoint and the Accounts/Status tabs degrade
gracefully if Phase 2 is deferred (empty token totals).

## 14. Open questions for review

1. **Phasing** — ship Phase 1 first (recommended), or build both in one pass?
2. **`web.enabled` default** — `true` (recommended, matches "feature on") or
   `false` (opt-in)?
3. **`POST /api/tokens/reset`** — include in v1 or defer?
4. **Backup retention** — single `config.yaml.bak` slot (simple; recommended) vs.
   timestamped history?
