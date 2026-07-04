# Quota-Aware Scheduling — Design

**Date:** 2026-07-05
**Status:** Draft, pending review
**Scope:** `model-proxy/` Go module

## Problem

`model-proxy` routes requests across multiple upstream providers. Several are
**Coding Plan / Agent Plan subscriptions** with hard, windowed quotas — a 5-hour
window, a weekly window, a monthly window (zhipu, codex, volcengine, compass).
One (deepseek) is **pay-as-you-go** (balance, no window).

Today's scheduler (`proxy.go:schedule`) is purely *reactive* about quota:

- It ranks targets by `peak_hours` then static `priority`, keeps a per-route
  *sticky* provider for `sticky_dwell` (≈10m, partially cache-friendly), and
  only learns a provider is out of quota when it returns **429** — by which
  point it's already depleted.
- It has no visibility into *remaining* quota, so it cannot keep providers
  balanced: one gets ridden into the wall while others sit idle.
- Pay-as-you-go is only "last" by virtue of static `priority`, not enforced.
- Cache warmth (which directly affects token/quota burn) is preserved only
  indirectly via sticky dwell.

## Goal

A scheduler that:

1. **Balances quota** across plan providers — prefer the one with the most
   remaining quota in its binding window, so no provider is overused while
   others idle.
2. **Minimizes pay-as-you-go** — strict last-resort, never chosen while any
   plan provider has usable quota.
3. **Preserves cache** — stay sticky within a conversation (a switch breaks
   the prompt cache and *increases* token burn), switching providers only
   when another is meaningfully ahead *and* the minimum dwell has elapsed.
4. **Folds `peak_hours` into the score** — peak is no longer a separate sort
   tier; it discounts effective remaining via a per-segment multiplier.

## Decisions (locked)

| Concern | Decision |
|---|---|
| Scheduling goal | **Quota-aware sticky** — sticky per conversation; pick the *starting* provider by remaining quota; switch off when another is meaningfully ahead. |
| Quota source | **Periodic background polling** of existing usage/quota endpoints; **persisted to file** (survives restart/reload). No response-usage parsing in v1. |
| Switch trigger | **Switch when another plan provider's *effective* remaining beats the current by ≥ margin, but only after `sticky_dwell` (≈10m)** has elapsed. No hard floor; reactive 429 remains the ultimate backstop. |
| Peak formula | `effective_remaining = RemainingPct / peak_multiplier` (1.0 when not in peak). Peak = "budget drains N× faster, remaining worth 1/N". |
| Peak config | **Multi-segment, per-segment multiplier**, with shorthand forms (single string / list-of-strings use a default multiplier). |
| Pay-as-you-go | Strict last-resort, designated by an explicit `billing: pay-as-you-go` config flag. |

Two calls left to the implementer's judgement, correct at review:

- **Parse sharing (unified)** — `Quota()` and the `usage` display path share one
  parse function per provider. *Alternative if de-risking v1:* `Quota()` ships
  its own parse (small duplication), display untouched; unify later.
- **No hard floor** on remaining% — rely on margin-switch + reactive 429.

## Architecture

One new capability layered on top of the existing sticky + circuit + rate-limit
machinery. Nothing about circuit-breaker / rate-limit / half-open / failover
changes.

```
proxy.go (Proxy)  ──holds──►  quotaTracker            (quota.go, NEW)
                                  │ poll loop goroutine, started in NewProxy
                                  ▼
                         ~/.model-proxy/quota_state.json   (persisted, atomic)
                                  │ loaded at boot (baseline before first poll)
                                  ▲
provider.Provider  ──new method──►  Quota() (*QuotaSnapshot, error)
                                  ▲ wired via cfg.QuotaFn callback (mirrors UsageFn)
                                  │
schedule()  ──reads──►  quotaTracker.snapshot()  under quotaMu.RLock
```

New / changed files:

- **`quota.go`** (NEW, `package main`) — `QuotaSnapshot` model, `quotaTracker`
  (state + mutex + poll loop + persistence).
- **`provider/provider.go`** — add `Quota() (*QuotaSnapshot, error)` to the
  `Provider` interface; add `QuotaFn func() (*QuotaSnapshot, error)` to
  `provider.Config`. Define `QuotaSnapshot` / `QuotaWindow` / `BillingClass`
  here (exported, since providers return them).
- **`provider/*.go`** — each provider gains a one-liner
  `Quota() { return p.cfg.QuotaFn() }`.
- **`proxy.go`** — `Proxy` holds `quota *quotaTracker`; `NewProxy` starts it;
  `reload` keeps it alive (tracker re-reads `cfg` each cycle); `schedule()`
  consults it.
- **`provider_wire.go`** — wire `QuotaFn` per provider in `buildProviders`.
- **`config.go`** — new `Scheduling` fields, new per-provider `Billing` +
  multi-segment `PeakHours`; extend `validate()`.
- **`main.go`** — new `fetch*Quota` parse functions (shared with display);
  refactor `show*Usage` to consume them.
- **`defaults.go`** — update the embedded template + comments.

No new CLI subcommand. `usage` keeps working (and, under the unified-parse
choice, is refactored to call the shared parser — output must stay identical).

## Components

### 1. Quota model — `QuotaSnapshot`

Normalized, provider-agnostic snapshot of one provider's polled quota:

```go
type BillingClass int
const (
    BillingUnknown BillingClass = iota // can't measure (no AK/SK, not logged in, poll failed, stale)
    BillingPlan                        // Coding/Agent Plan: windowed quotas
    BillingPayG                        // pay-as-you-go: strict last-resort
)

type QuotaWindow struct {
    Label        string    // "5h", "weekly", "monthly", "spend"
    RemainingPct float64   // 0..1
    ResetsAt     time.Time // zero if unknown
}

type QuotaSnapshot struct {
    Billing      BillingClass
    RemainingPct float64    // binding constraint = min over windows; -1 if unknown
    Windows      []QuotaWindow
    AsOf         time.Time  // when polled
    Err          string     // last poll error, "" if ok
}
```

`RemainingPct` = **min remaining% across that provider's windows** (the binding
constraint). Per-provider mapping:

| Provider | Source endpoint | Windows | Billing |
|---|---|---|---|
| compass | `monthly_usage` (POST, SSO cookie) | monthly $ budget → 1 window (`RemainingPct = Balance/TotalAmount`, i.e. `(total−usage)/total`) | plan |
| codex | `wham/usage` (GET, Bearer) | primary(5h) + secondary(weekly) + spend-control(monthly $) | plan |
| zhipu | `quota/limit` (GET, Bearer) | TOKENS_LIMIT unit=3 (5h) + unit=6 (weekly). `TIME_LIMIT` (monthly, MCP tools) is **excluded** from `min()` — it's tool quota, not LLM tokens. | plan |
| volcengine | `GetAFPUsage` (signed OpenAPI, AK/SK) | 5h / daily / weekly / monthly AFP | plan (→ unknown if no AK/SK) |
| deepseek | `user/balance` (GET, Bearer) | balance only (no window) | **payg** |

For compass/codex **spend** windows that are money (not a % of a hard cap),
`RemainingPct` is computed as `(limit − used) / limit`; the window `Kind` is
`money` for display, but it still participates in the `min()` so a provider
nearing its monthly $ cap ranks low.

### 2. Provider quota adapters — `Quota()` + shared parse

Add `Quota() (*QuotaSnapshot, error)` to the `Provider` interface, wired through
`cfg.QuotaFn` exactly like `UsageFn`. Each provider implements it as
`return p.cfg.QuotaFn()`. `buildProviders` wires `QuotaFn` per `provider_id` to
a main-package function:

```go
func fetchZhipuQuota(cfg *Config, name string, prov Provider) (*QuotaSnapshot, error)
func fetchCodexQuota(cfg *Config, prov Provider) (*QuotaSnapshot, error)
func fetchVolcengineQuota(name string) (*QuotaSnapshot, error)
func fetchDeepseekQuota(cfg *Config, name string, prov Provider) (*QuotaSnapshot, error)
func fetchCompassQuota(cfg *Config) (*QuotaSnapshot, error)
```

**Unified parse (recommended):** each `fetch*Quota` owns the endpoint fetch +
JSON parse and returns a `QuotaSnapshot`; the corresponding `show*Usage` is
refactored to call the same function and *format* the result for the terminal.
One parser, two consumers; no second source of truth for "what does zhipu's
quota JSON mean". Terminal output is byte-identical to today (verified by
snapshot test).

Pay-as-you-go / no-window providers (deepseek) still fetch balance for
display, but return `Billing: BillingPayG` with `RemainingPct: -1` (unmeasured)
— their rank is fixed by the billing tier, not by remaining.

### 3. `quotaTracker` — poll loop + persistence

```go
type quotaTracker struct {
    mu    sync.RWMutex
    state map[string]*QuotaSnapshot // provider name → snapshot
    cfg   func() *Config            // read fresh cfg each cycle (reload-safe)
    provs func() map[string]provider.Provider
    path  string                    // ~/.model-proxy/quota_state.json
    stop  chan struct{}
}
```

Behavior:

- **Cadence** `quota_poll_interval` (default **5m**). Plus:
  - a **bootstrap poll** ~10s after start (so data exists before the first
    scheduling decision that needs it);
  - an **immediate re-poll of a provider after it 429s** (`recordRateLimit`
    kicks an async refresh), so remaining is fresh when its rate-limit clears.
- **Persistence** → `~/.model-proxy/quota_state.json`, written atomically
  (temp + `rename`) after each successful poll. **Loaded at boot** so there's a
  baseline before the first poll completes (survives daemon restart / hot
  reload — the explicit requirement). Format: `{ "<provider>": {billing, remaining_pct, windows, as_of, err} }`.
- **Reload-safe:** each cycle snapshots `cfg.Providers` names via the `cfg`/`provs`
  closures; `Proxy.reload` swaps `cfg`/`providers` atomically under `mu`; the
  tracker picks up added providers next cycle and drops removed ones. The
  tracker has its own `quotaMu`, separate from `healthMu` and reload `mu`.
- **Graceful degradation:** on `Quota()` failure (volcengine without AK/SK, not
  logged in, network), record `Billing: BillingUnknown` + `Err`, keep the last
  known snapshot, retry next cycle. The poller never crashes the proxy.
- **Staleness guard:** if `AsOf` is older than `3 × quota_poll_interval`, treat
  the snapshot as `BillingUnknown` (fall back to priority ordering) — don't act
  on badly stale data.

### 4. `schedule()` — new ranking + switch rule

Sort key (primary → secondary):

```
1. billing tier:   plan(0)  <  unknown(1)  <  payg(2)     ← payg strict last-resort
2. effective_remaining desc                       ← quota balance, peak-folded
3. priority asc                                       ← existing, now final tie-breaker
```

Where:

```
effective_remaining(prov, now) =
    RemainingPct / activePeakMultiplier(prov, now)      if Billing == plan and RemainingPct known
    1.0                                                 if Billing == unknown   (ranked by priority only, tier 1)
    (unused)                                            if Billing == payg       (tier 2, always last)
```

and `activePeakMultiplier` returns the multiplier of whichever peak segment
`now` falls into (1.0 if none) — see §5.

**`peak_hours` is no longer a sort tier** (today it is the primary group); it
now lives entirely inside `effective_remaining` via the multiplier. This is a
deliberate behavior change: a provider at 5% remaining is a real exhaustion
risk regardless of peak, and peak is a soft, tunable latency heuristic. Set the
multiplier to 1.0 to disable peak's effect for a provider.

**Sticky-switch rule** (the chosen "switch when ahead, after min dwell"):

```
keep the current sticky provider if:
    it is available (circuit closed, not rate-limited, not half-open-busy)
  AND
    ( now − sticky.since  <  sticky_dwell )                         ← 10m min dwell
    OR no other *plan* provider has effective_remaining exceeding
       the current's by ≥ quota_switch_margin                       ← "ahead by margin"
       (config integer, percentage points; default 15 → 0.15 fraction
        internally; comparison `eff_other − eff_current ≥ margin/100`)
```

If the keep-condition fails, re-pick = top of the effective-remaining-sorted
plan tier (else unknown tier, else payg). The returned `ordered` slice is
`[chosen] + [rest in tier order]`, so failover walks next-best → … → payg last.
Circuit / rate-limit / half-open filtering happens first (unchanged), so
unavailable providers never enter the ranking.

Note on peak + switch interaction: because the switch compares
*effective* remaining, a sticky provider that enters its peak window
(effectively halved at mult=2) will commonly meet the margin against a non-peak
peer after dwell and get switched off — exactly the desired "avoid the
congested provider" behavior.

### 5. Peak config — multi-segment, per-segment multiplier

`peak_hours` moves from a single `string` to a custom type accepting three
forms (backward-compatible with today's single string):

```go
type PeakSegment struct {
    Window     string  `yaml:"window"`     // "HH:MM-HH:MM", supports wrap-around
    Multiplier float64 `yaml:"multiplier"` // > 0; 0 → default (2.0) at validate time
}
type PeakConfig []PeakSegment   // custom UnmarshalYAML: string | []string | []map
```

Accepted YAML shapes:

```yaml
# legacy single string → 1 segment, default multiplier
peak_hours: "09:00-18:00"

# list of windows → N segments, default multiplier each
peak_hours: ["09:00-12:00", "14:00-18:00"]

# explicit per-segment multipliers
peak_hours:
  - {window: "09:00-12:00", multiplier: 2}
  - {window: "14:00-18:00", multiplier: 3}
```

`activePeakMultiplier(prov, now)` iterates segments; returns the first
containing segment's multiplier, else 1.0. The existing `parseHHMMRange` /
wrap-around logic is reused; `Provider.inPeak(now) bool` is replaced by
`Provider.peakMultiplier(now) float64`. Default multiplier (when a segment
omits it) is **2.0**.

## Config schema (new fields)

```yaml
providers:
  deepseek:
    provider_id: deepseek
    billing: pay-as-you-go        # NEW. default: plan. payg = strict last-resort.
    # ...
  zhipu:
    provider_id: zhipu
    peak_hours:                   # NEW shape (multi-segment). legacy single string still valid.
      - {window: "09:00-12:00", multiplier: 2}
      - {window: "14:00-18:00", multiplier: 2}
    # ...

scheduling:
  quota_poll_interval: 5m         # NEW. background poll cadence. default 5m.
  quota_switch_margin: 15         # NEW. switch if another plan provider's effective
                                  #      remaining beats the sticky one by ≥ this many
                                  #      percentage points. default 15.
  # unchanged: circuit_threshold, circuit_cooldown, rate_limit_backoff,
  # upstream_timeout, sticky_dwell (sticky_dwell doubles as the min-dwell-before-switch)
```

`peak_multiplier` is per-segment (inside `peak_hours`), not a separate field.

## Data flow

1. Boot: `NewProxy` builds providers, **loads `quota_state.json`** into the
   tracker (baseline), starts the poll goroutine.
2. ~10s later: bootstrap poll refreshes all providers' snapshots; file rewritten.
3. Every `quota_poll_interval`: poll each provider in parallel (bounded), update
   `state`, rewrite file atomically. Failed/stale → `BillingUnknown`.
4. Request arrives → `forward` → `schedule(exposed, targets)`:
   - filter available targets (circuit/rate-limit/half-open — unchanged);
   - read `quotaTracker.snapshot()` (RLock);
   - compute `effective_remaining` per target;
   - apply sticky-switch rule → pick `chosen`, build `ordered`.
5. For each target in `ordered`, `tryTarget` (unchanged); on 429,
   `recordRateLimit` + **async re-poll** of that provider.
6. Hot reload (`SIGHUP`): `Proxy.reload` swaps `cfg`/`providers`; tracker keeps
   running, picks up new providers next cycle.

## Error handling / edge cases

- **Poll failure** → log (masked, like existing logging hygiene), keep last
  snapshot, mark `Err`, retry next cycle. Poller never crashes.
- **Unknown quota** (volcengine no AK/SK, not logged in, poll failed) →
  `BillingUnknown`; ranked by priority in tier 1, between plan and payg. Never
  treated as payg.
- **Stale snapshot** (> 3× interval) → treated as unknown; fall back to priority.
- **All plan/unknown providers unavailable** → payg tier used (the safety net).
- **Reload race** → tracker snapshots cfg names per cycle; atomic cfg swap;
  removed providers' entries are harmless (rebuild next cycle).
- **First request before first poll** → uses persisted baseline; if no file,
  unknown → priority ordering (today's behavior).
- **volcengine without AK/SK** → `usage` already shows the note; `Quota()`
  returns `BillingUnknown` + a sentinel error; scheduler skips it for quota
  ranking.
- **Concurrency** → `quotaMu` (tracker) is independent of `healthMu`
  (circuit/rate-limit) and reload `mu`; `schedule` takes only a brief RLock on
  the tracker.

## Testing (white-box, stdlib only — matches the repo)

- `fetch*Quota` parsers: feed sample JSON → assert `RemainingPct` + binding
  window. Edge cases per provider: missing window, zero quota, reset-time
  day-wrap, spend (money) window.
- `activePeakMultiplier` / `PeakConfig.UnmarshalYAML`: single string, list of
  strings, list of maps, wrap-around windows, default multiplier.
- `effective_remaining`: plan/unknown/payg × in-peak/not × known/unknown.
- `schedule` with an injected `quotaTracker`:
  - initial pick = highest effective-remaining plan provider;
  - sticky kept within `sticky_dwell` even if another is ahead;
  - after dwell, switches when ahead by ≥ margin; does *not* switch when ahead
    by < margin;
  - peak discount correctly forces a switch off a peak sticky after dwell;
  - payg used iff no plan/unknown provider is available;
  - unknown-quota provider ranked by priority in tier 1;
  - a 429 triggers an async re-poll (assert the refresh call happens).
- Persistence: write `quota_state.json` → new tracker → assert baseline loaded;
  stale snapshot → unknown fallback.
- Display regression: snapshot test asserting `usage` output is byte-identical
  before/after the parse refactor.

## Out of scope (v1) / future

- **Per-response token/cache accounting** (the deferred "hybrid" path): parsing
  `usage` from streaming responses to compute per-provider burn rate and
  cache-hit rate. Would enable projected-time-to-empty ranking (Approach B)
  and direct cache-hit-as-a-signal. Deferred — polling alone covers the goal.
- **Weighted-random fair-share** (Approach C): only matters under high
  concurrency / thundering-herd; the deterministic ranking suffices for a
  single-developer proxy.
- **Unifying `usage` display fully onto `QuotaSnapshot`** beyond parse sharing
  (e.g. a single renderer) — optional follow-up.
- **Per-route quota overrides** (e.g. reserve a provider for a specific model)
  — not needed yet.

## Open decisions (implementer's call — correct at review)

1. **Parse sharing = unified** (recommended). Fallback: scoped parse for v1
   (duplicate the JSON structs in `fetch*Quota`, leave `show*Usage` untouched,
   unify in a follow-up).
2. **No hard floor** on remaining%. A floor could be added later if the
   margin-switch proves too lenient when *all* providers are low.
