# Quota-Aware Scheduling — Design

**Date:** 2026-07-05 (last revised 2026-07-06)
**Status:** Implemented on branch `quota-aware-scheduling` (merge-base `d40c8b4`).
**Canonical docs:** `CLAUDE.md` / `AGENTS.md` (deep-dive) / `model-proxy/README.md`
are the source of truth — read those first. This design doc describes the original
design + the first as-built revision; **the scheduling score has since evolved from
`effective_remaining = RemainingPct/peak_mult` to the surplus model**
(`surplus = (ultimate.rem − short.rem×share×(peakMult−1)) − fLeft`, exposed as a
`Provider.Surplus` interface method; ranking is `(tier, priority asc, surplus desc)` — priority (config) beats surplus;
peak now only burns the short rate-cap window; sticky is persisted in
`quota_state.json`; `GET /debug/schedule` + `schedule`/`doctor` commands added).
The surplus-specific sections below are updated; where this doc still says
`effective_remaining`, defer to the canonical docs.
**Scope:** `model-proxy/` Go module

## Problem

`model-proxy` routes requests across multiple upstream providers. Several are
**Coding Plan / Agent Plan subscriptions** with hard, windowed quotas — a 5-hour
window, a weekly window, a monthly window (zhipu, codex, volcengine, compass).
One (deepseek) is **pay-as-you-go** (balance, no window).

Before this change, the scheduler (`proxy.go:schedule`) was purely *reactive* about
quota:

- It ranked targets by `peak_hours` then static `priority`, kept a per-route
  *sticky* provider for `sticky_dwell` (≈10m, partially cache-friendly), and only
  learned a provider was out of quota when it returned **429** — by which point it
  was already depleted.
- It had no visibility into *remaining* quota, so it could not keep providers
  balanced: one got ridden into the wall while others sat idle.
- Pay-as-you-go was only "last" by virtue of static `priority`, not enforced.
- Cache warmth (which directly affects token/quota burn) was preserved only
  indirectly via sticky dwell.

## Goal

A scheduler that:

1. **Balances quota** across plan providers — prefer the one with the most
   remaining quota in its binding window, so no provider is overused while others
   idle.
2. **Minimizes pay-as-you-go** — strict last-resort, never chosen while any plan
   provider has usable quota.
3. **Preserves cache** — stay sticky within a conversation (a switch breaks the
   prompt cache and *increases* token burn), switching providers only when
   another is meaningfully ahead *and* the minimum dwell has elapsed.
4. **Folds `peak_hours` into the score** — peak is no longer a separate sort tier;
   it discounts effective remaining via a per-segment multiplier.

## As-built deviations from the original draft

The build deviated from the first draft in five places. Each was caught during
implementation/review and is the authoritative behavior now:

1. **`tierRank` indirection for billing order.** `BillingClass` is declared
   `BillingUnknown=0, BillingPlan=1, BillingPayG=2` (iota), which does **not**
   match the scheduling order `plan < unknown < payg`. Sorting on the raw
   constants would rank *unknown ahead of plan*. The build adds a
   `tierRank(BillingClass) int` map (`plan→0, unknown→1, payg→2`) used in both
   the `schedule()` sort and the sticky-switch tier comparison. (Found by the
   Task 9 review; locked in by `TestSchedule_PlanBeforeUnknown`.)
2. **`effective_remaining = 1.0/mult` for unknown (not bare `1.0`).** Unknown
   snapshots carry `RemainingPct: 0` (Go zero value), so the draft's "return 1.0
   only when snapshot is nil" check would have ranked them as eff=0 (last). The
   build returns `1.0/mult` for nil/`BillingUnknown`/`RemainingPct<0`, which both
   keeps unknown neutral **and** lets `peak_multiplier` still discount a provider
   that has no quota data (so peak deprioritization works without polling).
3. **Sticky-switch rule is tier → quota-margin → priority.** The draft's "switch
   only when another plan provider is ahead by ≥ quota margin" would, with
   all-unknown quota (eff equal), never switch after dwell — regressing the
   pre-existing "return to the priority-1 provider after a blip" behavior. The
   build switches to the best provider (`availTargets[0]`) after dwell when the
   best wins on **billing tier, then quota margin, then priority** — only staying
   when the best's sole edge is a sub-margin quota difference.
4. **Display refactor is zhipu-only.** The draft proposed unifying all providers'
   `usage` display onto a shared `printQuotaSnapshot` renderer. The build applies
   that only to zhipu (simple windows); codex/volcengine/deepseek/compass keep
   their byte-identical custom display, and the new `fetch*Quota` parsers feed the
   scheduler only. Accepted DRY cost (each rich provider's JSON is parsed in two
   places) to preserve display fidelity.
5. **`stop()` hardened with `sync.Once`.** `quotaTracker.stop()` closes a channel
   and would panic on a double-close; the build wraps it in `stopOnce` (carried
   from a Task 7 review finding). `stop()` currently has no production caller —
   the tracker lives for the process lifetime — but the guard is cheap insurance.

Everything else in this document matches the build.

## Decisions (locked)

| Concern | Decision |
|---|---|
| Scheduling goal | **Quota-aware sticky** — sticky per conversation; rank by **surplus** (use-it-or-lose-it pace score); switch off when another is meaningfully ahead. |
| Quota source | **Periodic background polling** of existing usage/quota endpoints; **persisted to file** (survives restart/reload, **including the per-route sticky map**). No response-usage parsing in v1. |
| Scheduling score | **surplus** (a `Provider.Surplus(snap, now, peakMult)` interface method delegating to `(*QuotaSnapshot).Surplus`): `surplus = (ultimate.remaining − short.remaining × (short.total/ultimate.total) × (peakMult−1)) − fLeft`, where `fLeft = clamp((ultimate.reset−now)/ultimate.duration, 0,1)`. `RemainingPct` = the **ultimate** window's remaining (zhipu=weekly, volcengine/codex/compass=monthly, deepseek=payg). surplus>0 = under pace → prioritize; <0 = over pace → avoid. Ranking: `(tier: plan<unknown<payg, priority asc, surplus desc)` — **priority (config) beats surplus; surplus only breaks priority ties**. |
| Switch trigger | **After `sticky_dwell` (≈10m), switch to the best provider when it wins on tier → priority → surplus-margin (`quota_switch_margin`, 15 pts)** (only stay when the best's sole edge is a sub-margin surplus difference at equal priority). No hard floor; reactive 429 remains the ultimate backstop. |
| Peak formula | `peakMult` only burns the **short rate-cap window** (the `×(peakMult−1)` term in surplus). Providers without a same-unit short window (codex/compass money-ultimate; not-yet-polled) get **no peak discount** — peak is no longer a blanket `RemainingPct/mult` latency cut. Multiplier=1 disables. |
| Peak config | **Multi-segment, per-segment multiplier**, with shorthand forms (single string / list-of-strings use a default multiplier of 2.0). |
| Pay-as-you-go | Strict last-resort, designated by an explicit `billing: pay-as-you-go` config flag. |
| Display scope | zhipu uses the shared `printQuotaSnapshot` renderer; the other four providers keep byte-identical custom display. Parsers are shared by the scheduler only. |

## Architecture

One new capability layered on top of the existing sticky + circuit + rate-limit
machinery. Nothing about circuit-breaker / rate-limit / half-open / failover
changes.

```
proxy.go (Proxy)  ──holds──►  quotaTracker                 (quota.go, NEW)
                                  │ poll loop goroutine, started in NewProxy
                                  ▼
                         ~/.model-proxy/quota_state.json    (persisted, atomic, 0600)
                                  │ loaded at boot (baseline before first poll)
                                  ▲
provider.Provider  ──new method──►  Quota() (*QuotaSnapshot, error)
                                  ▲ wired via cfg.QuotaFn callback (mirrors UsageFn)
                                  │
schedule()  ──reads──►  quotaTracker.allSnapshots()  (quotaMu RLock, brief,
                            called BEFORE healthMu.Lock — lock order healthMu→quotaMu)
```

New / changed files:

- **`quota.go`** (NEW, `package main`) — `quotaTracker` (state + `quotaMu` RWMutex
  + poll loop + atomic persistence + boot load + staleness + `refreshOne`).
- **`provider/provider.go`** — `QuotaSnapshot` / `QuotaWindow` / `QuotaDetail` /
  `BillingClass` types; `Quota()` on the `Provider` interface; `QuotaFn` +
  `QuotaOrUnknown()` on `provider.Config`; `BindingRemaining` helper.
- **`provider/*.go`** — each provider gains `Quota() { return p.cfg.QuotaOrUnknown() }`.
- **`proxy.go`** — `Proxy` holds `quota *quotaTracker`; `NewProxy` starts it;
  `reload` keeps it and kicks `pollAll`; `recordRateLimit` triggers async
  `refreshOne`; `schedule()` + `billingClass` + `effectiveRemaining` + `tierRank`.
- **`main.go`** — `parse*Quota` + `fetch*Quota` per provider; `printQuotaSnapshot`
  generic renderer (zhipu display); `showGenericUsage` zhipu branch refactored.
- **`config.go`** — `Scheduling.QuotaPollInterval`/`QuotaSwitchMargin` + accessors;
  per-provider `Billing`; `PeakSegment`/`PeakConfig` (custom unmarshal) +
  `Provider.peakMultiplier(now)`; `validate()`.
- **`defaults.go`** / **`config.yaml`** — deepseek `billing: pay-as-you-go`,
  multi-segment zhipu `peak_hours`, two new `scheduling` fields.

No new CLI subcommand. `usage` keeps working; zhipu output is essentially unchanged
(the `Level:` line is preserved); the other four are byte-identical.

## Components

### 1. Quota model — `QuotaSnapshot` (in `provider/provider.go`)

Normalized, provider-agnostic snapshot of one provider's polled quota:

```go
type BillingClass int
const (
    BillingUnknown BillingClass = iota // can't measure (no AK/SK, not logged in, poll failed, stale)
    BillingPlan                        // Coding/Agent Plan: windowed quotas
    BillingPayG                        // pay-as-you-go: strict last-resort
)
// NOTE: the iota order (Unknown=0,Plan=1,PayG=2) is NOT the scheduling order.
// schedule() routes billing comparisons through tierRank() — see §4.

type QuotaDetail struct { Label string; Used float64 }

type QuotaWindow struct {
    Label        string        // "5h tokens", "Weekly tokens", "Monthly time", "Spend", "Balance"
    Kind         string        // "tokens" | "time" | "money"
    Used, Total  float64
    RemainingPct float64       // 0..1; -1 if unmeasured (e.g. balance-only)
    ResetsAt     time.Time     // zero if unknown
    Details      []QuotaDetail // per-model / per-tool breakdown (display)
    DetailLabel  string        // breakdown header for display ("By model"/"By MCP tool")
}

type QuotaSnapshot struct {
    Billing      BillingClass
    RemainingPct float64    // binding min over windows; -1 if unknown
    Account, Plan, Level string
    Windows      []QuotaWindow
    Notes        []string   // provider-specific status lines (display)
    AsOf         time.Time
    Err          string
}
```

`RemainingPct` = **min remaining% across that provider's windows** via
`BindingRemaining` (which skips `RemainingPct < 0`). Per-provider mapping:

| Provider | Source endpoint | Windows | Billing |
|---|---|---|---|
| compass | `monthly_usage` (POST, SSO cookie) | monthly $ budget → 1 window (`RemainingPct = Balance/TotalAmount`; zero-guard) | plan |
| codex | `wham/usage` (GET, Bearer) | primary(5h) + secondary(weekly) + spend-control(monthly $) | plan |
| zhipu | `quota/limit` (GET, Bearer) | TOKENS_LIMIT unit=3 (5h) + unit=6 (weekly). `TIME_LIMIT` (monthly, MCP tools) is **excluded** from `min()` — it's tool quota, not LLM tokens. | plan |
| volcengine | `GetAFPUsage` (signed OpenAPI, AK/SK) | 5h / daily / weekly / monthly AFP | plan (→ unknown if no AK/SK) |
| deepseek | `user/balance` (GET, Bearer) | balance only (no window) | **payg** |

For compass/codex **spend** windows that are money, `RemainingPct = (limit−used)/limit`;
they still participate in the `min()` so a provider nearing its monthly $ cap ranks
low. zhipu's `percentage` field is the **used** %, so per-window `RemainingPct =
(100−percentage)/100`.

### 2. Provider quota adapters — `Quota()` + parse

`Quota()` is added to the `Provider` interface, wired through `cfg.QuotaFn`
exactly like `UsageFn`; each provider implements it as
`return p.cfg.QuotaOrUnknown()` (nil `QuotaFn` → `BillingUnknown`, never panics).
`buildProviders` wires `QuotaFn` per `provider_id` to a main-package function:

```go
func parseZhipuQuota(body []byte, account string) (*QuotaSnapshot, error)  // pure; nil if not-zhipu body
func fetchZhipuQuota(cfg *Config, name string, prov Provider) (*QuotaSnapshot, error)
func parseCodexQuota(body []byte, account, plan string) (*QuotaSnapshot, error)
func fetchCodexQuota(cfg *Config, prov Provider) (*QuotaSnapshot, error)
func parseVolcengineQuota(u *afpUsage) *QuotaSnapshot
func fetchVolcengineQuota(name string) (*QuotaSnapshot, error)
func parseDeepseekQuota(body []byte) *QuotaSnapshot
func fetchDeepseekQuota(cfg *Config, name string, prov Provider) (*QuotaSnapshot, error)
func parseCompassQuota(mu *MonthlyProjectUsage, account string) *QuotaSnapshot
func fetchCompassQuota(cfg *Config) (*QuotaSnapshot, error)
```

`fetch*Quota` degrades to `BillingUnknown + Err` (never a hard Go error on
auth/network/non-200 paths) so a poll failure can't crash the poller. deepseek
returns `BillingPayG` with `RemainingPct: -1`.

**Display (zhipu only):** `showGenericUsage`'s zhipu branch calls
`parseZhipuQuota`, prints the `Level:` line, then the generic
`printQuotaSnapshot(s)` renderer (bars / `% used` / reset strings / `By model` /
`By MCP tool` labels). codex/volcengine/deepseek/compass displays are unchanged.

### 3. `quotaTracker` — poll loop + persistence (in `quota.go`)

```go
type quotaTracker struct {
    mu        sync.RWMutex
    state     map[string]*provider.QuotaSnapshot
    path      string                    // ~/.model-proxy/quota_state.json
    cfg       func() *Config            // RLock-brief snapshots (reload-safe)
    provs     func() map[string]provider.Provider
    stopCh    chan struct{}
    stopOnce  sync.Once
    refreshHook func(name string)       // test override for refreshOne
}
```

Behavior:

- **Cadence** `quota_poll_interval` (default **5m**). Plus:
  - a **bootstrap poll** ~10s after start;
  - an **immediate re-poll of a provider after it 429s** — `recordRateLimit`
    spawns `go p.quota.refreshOne(name)` (after releasing `healthMu`).
- **`pollAll`** snapshots cfg/providers via the closures, spawns one goroutine per
  provider calling `Quota()` (no lock held during the call), records results under
  `mu`, and `persist()`s once at the end. `refreshOne` polls a single provider.
- **Persistence** → `~/.model-proxy/quota_state.json`, written atomically
  (`path+".tmp"` → `os.Rename`, `MkdirAll(dir, 0o700)`, file `0o600`) after each
  poll. **Loaded at boot** so there's a baseline before the first poll completes.
- **Reload-safe:** `Proxy.reload` keeps the tracker (doesn't stop/recreate it) and
  kicks `go p.quota.pollAll(time.Now())` so added/removed providers are picked up
  immediately. The tracker reads fresh cfg/providers each cycle through closures
  that briefly `RLock` the reload `mu`.
- **Graceful degradation:** on `Quota()` failure (volcengine without AK/SK, not
  logged in, network), record `BillingUnknown + Err`, keep the last known
  snapshot, retry next cycle. The poller never crashes the proxy.
- **Staleness guard:** if `AsOf` is older than `3 × quota_poll_interval`, treat the
  snapshot as `BillingUnknown` (fall back to priority ordering).
- Owns `quotaMu` only — independent of `healthMu` and the reload `mu`.

### 4. `schedule()` — ranking + switch rule (in `proxy.go`)

`schedule` is split into a non-mutating `decideOrder()` (returns the ordered
targets + the provider to park sticky on) + a thin `schedule()` that commits the
sticky. `decideOrder` calls `p.quota.allSnapshots()` **before** taking `healthMu`
(lock order `healthMu → quotaMu`, never reversed), filters available targets
(circuit/rate-limit, unchanged), then sorts and applies the sticky rule.

Sort key (primary → secondary):

```
1. tierRank(billing):  plan(0) < unknown(1) < payg(2)   ← payg strict last-resort
2. priority asc                                         ← config; beats surplus
3. surplus desc                                         ← breaks priority ties (use it or lose it)
```

`surplus` comes from `provs[name].Surplus(snap, now, peakMult)` (a `Provider`
interface method; each impl delegates to `(*QuotaSnapshot).Surplus`, the shared
formula in `provider/`):

```go
// (*QuotaSnapshot).Surplus(now, peakMult)
remaining := ultimate.remaining
if peakMult > 1 { remaining -= short.remaining * (short.total/ultimate.total) * (peakMult - 1) }
fLeft   := clamp((ultimate.reset - now)/ultimate.duration, 0, 1)
surplus := remaining - fLeft     // >0: under pace (prioritize); <0: over pace (avoid)
// nil/unknown/no-ultimate/no-reset → 0 (neutral)
```

`billingClass` (tier) is `classifyBilling(snap, billingCfg, pollInterval)` —
pay-as-you-go config → PayG; nil/`BillingUnknown`/`Err`/stale(>3×interval) → Unknown;
else the snapshot's Billing. `tierRank` maps that to plan(0)<unknown(1)<payg(2) (the
`BillingClass` iota order differs, hence `tierRank`).

**peak_hours only burns the short window** (the `×(peakMult−1)` term). Providers
without a same-unit short window (codex/compass money-ultimate; not-yet-polled) get
no peak discount — peak is no longer a blanket sort-tier or `/mult`. Multiplier=1 disables.

**Sticky-switch rule** (after dwell, switch to the best provider unless its only edge
is a sub-margin surplus gain):

```
keep the current sticky provider if:
    cur.provider != "" AND cur is in the available set (curInAvail)
  AND
    now − sticky.since < sticky_dwell                       ← min dwell (preserve cache)
    OR (after dwell) the best provider (availTargets[0]) does NOT win on:
         tierRank(best) < tierRank(cur)                     ← better billing tier → switch
         priority(best) < priority(cur)                      ← better priority → switch (priority beats surplus)
         surplus(best) − surplus(cur) ≥ quota_switch_margin ← ahead by surplus margin (equal priority) → switch
       (i.e. stay only when same tier + same priority + sub-margin surplus)
```

If the keep-condition fails, re-pick = `availTargets[0]`. The returned `ordered`
slice is `[chosen] + [rest in sorted order]`, so failover walks next-best → … → payg
last. The per-route sticky map is persisted in `quota_state.json` and restored on boot.
Circuit / rate-limit / half-open filtering happens first (unchanged).

**Visibility** (read-only): `GET /debug/schedule` peeks via `decideOrder` (no sticky
mutation) and returns per route the first-choice provider + ordered list
(tier/surplus/available/peak) + sticky state. `model-proxy schedule` queries it
(daemon must run); `model-proxy doctor` is an offline config diagnostic.

Note: because the switch compares *effective* remaining, a sticky provider that
enters its peak window (halved at mult=2) will commonly meet the margin against a
non-peak peer after dwell and get switched off — the desired "avoid the congested
provider" behavior. The priority arm preserves the pre-existing "return to the
priority-1 provider after a blip, in bounded time" behavior when quota is unknown.

### 5. Peak config — multi-segment, per-segment multiplier (in `config.go`)

`peak_hours` is a custom type accepting three forms (backward-compatible with the
legacy single string):

```go
type PeakSegment struct {
    Window     string  `yaml:"window"`     // "HH:MM-HH:MM", supports wrap-around
    Multiplier float64 `yaml:"multiplier"` // 0 → default (2.0); must be >= 0
}
type PeakConfig []PeakSegment   // custom UnmarshalYAML: string | []string | []map
```

Accepted YAML shapes:

```yaml
peak_hours: "09:00-18:00"                                   # legacy → 1 segment, default mult
peak_hours: ["09:00-12:00", "14:00-18:00"]                  # → N segments, default mult
peak_hours:
  - {window: "09:00-12:00", multiplier: 2}                  # explicit per-segment
  - {window: "14:00-18:00", multiplier: 3}
```

`Provider.peakMultiplier(now)` iterates segments; returns the first containing
segment's multiplier (default **2.0** when omitted), else 1.0. `parseHHMMRange` /
wrap-around logic reused. The old `Provider.inPeak` / `RouteTarget.inPeak` were
removed (unused after the rewrite).

## Config schema (new fields)

```yaml
providers:
  deepseek:
    provider_id: deepseek
    billing: pay-as-you-go        # NEW. default: plan. payg = strict last-resort.
  zhipu:
    provider_id: zhipu
    peak_hours:                   # NEW shape (multi-segment). legacy single string still valid.
      - {window: "14:00-18:00", multiplier: 2}

scheduling:
  quota_poll_interval: 5m         # NEW. background poll cadence. default 5m.
  quota_switch_margin: 15         # NEW. switch if another provider's surplus
                                  #      beats the sticky one by ≥ this many percentage
                                  #      points. default 15 (= 0.15 fraction internally).
  # unchanged: circuit_threshold, circuit_cooldown, rate_limit_backoff,
  # upstream_timeout, sticky_dwell (sticky_dwell doubles as the min-dwell-before-switch)
```

`peak_multiplier` is per-segment (inside `peak_hours`), not a separate field.

## Data flow

1. Boot: `NewProxy` builds providers, **loads `quota_state.json`** into the tracker
   (baseline), starts the poll goroutine.
2. ~10s later: bootstrap poll refreshes all providers' snapshots; file rewritten.
3. Every `quota_poll_interval`: `pollAll` polls each provider in parallel (no lock
   held during `Quota()`), updates `state`, rewrites the file atomically.
   Failed/stale → `BillingUnknown`.
4. Request arrives → `forward` → `schedule(exposed, targets)`:
   - snapshot quota via `allSnapshots()` (quotaMu RLock, brief);
   - `healthMu.Lock()`; filter available targets (circuit/rate-limit/half-open);
   - sort by `(tierRank, priority asc, surplus desc)`;
   - apply sticky-switch rule → pick `chosen`, build `ordered`.
5. For each target in `ordered`, `tryTarget` (unchanged); on 429, `recordRateLimit`
   (releases `healthMu`) + **async `refreshOne`** of that provider.
6. Hot reload (`SIGHUP`): `Proxy.reload` swaps `cfg`/`providers`; tracker keeps
   running, kicks `pollAll`, picks up new providers immediately.

## Error handling / edge cases

- **Poll failure** → keep last snapshot, mark `Err`, retry next cycle. Poller never
  crashes.
- **Unknown quota** (volcengine no AK/SK, not logged in, poll failed, stale) →
  `BillingUnknown`; ranked by priority in tier 1, between plan and payg. Never
  treated as payg.
- **Stale snapshot** (> 3× interval) → treated as unknown; fall back to priority.
- **All plan/unknown providers unavailable** → payg tier used (the safety net).
- **Reload race** → tracker snapshots cfg names per cycle; atomic cfg swap; removed
  providers' entries drop next cycle.
- **First request before first poll** → uses persisted baseline; if no file, unknown
  → priority ordering (today's behavior).
- **volcengine without AK/SK** → `Quota()` returns `BillingUnknown + Err`;
  scheduler ranks it by priority.
- **Concurrency** → `quotaMu` (tracker) independent of `healthMu` and reload `mu`;
  `schedule` takes only a brief quotaMu RLock (via `allSnapshots()`) before
  `healthMu`. `recordRateLimit` releases `healthMu` before spawning `refreshOne`.

## Testing (white-box, stdlib only — matches the repo)

Implemented tests (`go test ./...`, 72 passing, `-race` clean):

- `provider/quota_test.go` — `BindingRemaining` (skip-`-1`, min) + `QuotaOrUnknown`
  nil-guard.
- `config_test.go` — `PeakConfig.UnmarshalYAML` (single string / list of strings /
  list of maps), `peakMultiplier` (in/out/wrap), `Scheduling` quota defaults.
- `quota_test.go` — `parse*Quota` for all five providers (binding math, billing,
  windows); `quotaTracker` persistence round-trip, staleness → Unknown,
  `pollAll`→`Quota`.
- `proxy_quota_test.go` — `TestProxy_QuotaRefreshOnRateLimit` (429 → refreshOne);
  `TestSchedule_PicksMostRemaining`, `_StickyHoldsWithinDwell`,
  `_SwitchesAfterDwellByMargin`, `_NoSwitchBelowMargin`, `_PayGStrictLastResort`,
  `_PlanBeforeUnknown` (the tier-order regression test).

## Out of scope (v1) / future

- **Per-response token/cache accounting** (the deferred "hybrid" path): parsing
  `usage` from streaming responses to compute per-provider burn rate and cache-hit
  rate. Would enable projected-time-to-empty ranking and cache-hit-as-a-signal.
  Deferred — polling alone covers the goal.
- **Weighted-random fair-share** — only matters under high concurrency /
  thundering-herd; the deterministic ranking suffices for a single-developer proxy.
- **Full display unification** — route codex/volcengine/deepseek/compass displays
  through `printQuotaSnapshot` too (currently zhipu-only), eliminating the accepted
  dual-parse DRY cost.
- **Per-route quota overrides** — not needed yet.

## Known follow-ups (accepted Minors, not blocking)

- `config.yaml`'s routes comment still says "schedules non-peak providers first"
  (stale wording; the code is correct).
- `PeakConfig.UnmarshalYAML` rejects an explicit `peak_hours: ""` (convention is to
  omit the key, which works).
- `persist()` uses a fixed tmp path; a `refreshOne` racing a tick/reload `pollAll`
  could in principle interleave bytes (in-memory state is safe; only the persisted
  baseline is at risk across a restart). The 429-storm window is closed by
  `refreshOne` dedup, but a persist mutex / unique tmp name would fully eliminate it.
- `schedule` holds `healthMu.Lock()` for its whole duration. Fine at low RPS
  (target counts are small); if concurrency grows, split into an RLock read path +
  a separate write lock.
- The per-route `sticky` map is only cleared on reload; entries for removed routes
  linger otherwise (negligible memory; reload resets it consistently).
