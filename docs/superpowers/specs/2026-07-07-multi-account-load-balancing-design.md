# Multi-Account Load Balancing — per-provider credential pool

**Date:** 2026-07-07
**Status:** Design (awaiting review)
**Scope:** `model-proxy/` Go module

## 1. Goal

Let one config provider entry (e.g. `zhipu`) hold **multiple accounts**, added via
repeated `login <provider>` calls, and **actively spread** requests across them so
that:

- combined weekly quota across N accounts is usable (N× headroom);
- concurrent requests are distributed evenly instead of concentrating on one
  account until it 429s;
- each account has independent quota tracking, circuit breaker, and 429 back-off.

The user's hard constraint: **do NOT implement this by copying the provider config
N times** (no `zhipu-home` / `zhipu-team` entries). One config entry + N logins.

Secondary requirement: `login` must **detect a re-login of the same account** and
offer to update rather than duplicate.

## 2. Non-goals

- Cross-provider load balancing (already handled by routes + scheduling).
- Changing the existing surplus/tier/priority scheduling model for non-pooled
  providers. Single-account behavior is byte-for-byte unchanged.
- Per-request rotation (a `spread` mode). Deliberately out of scope: session-sticky
  keeps the prompt cache warm within a conversation while spreading across
  conversations; pure per-request rotation is cache-hostile and not needed (YAGNI).
- Phase-1 does not pool `codex` / `aqp`. Their OAuth token bundles + refresh/rotation
  semantics make pooling materially more complex; the architecture generalises, but
  MVP scopes to **apikey-type providers** (`zhipu`, `deepseek`, `volcengine`). See §14.

## 3. Design summary — the unrolling principle

A pooled provider is **unrolled at build time into N virtual providers** that share
one config but bind distinct credentials. The existing scheduling/health/quota
machinery is unchanged because, to it, each virtual provider is just an ordinary
provider name.

```
config:   providers.zhipu                       (ONE entry)
login:    zhipu × 3  →  pool file with 3 accounts
build:    providers map = { zhipu#acct1, zhipu#acct2, zhipu#acct3 }   (virtual)
routes:   glm-5.2: [{zhipu, glm-5.2}]  → expanded at schedule time to
          [{zhipu#acct1,...}, {zhipu#acct2,...}, {zhipu#acct3,...}]    (equal priority)
schedule: session-sticky — each conversation (x-claude-code-session-id) parks on one account (cache-warm); new conversations are round-robin-assigned across the pool (concurrent spread)
runtime:  each virtual has its OWN circuit / 429-skip / quota snapshot / auth
```

**Naming rule (backward compat):** pool size 1 → virtual id == parent name (`zhipu`,
identical to today). Pool size ≥ 2 → virtual ids are `name#<accountID>`. So existing
single-account setups, `quota_state.json`, sticky map, logs, and tests are untouched.

Judgement rule for which interfaces fan out per-account vs run once:
- **per-credential** (usage fetch, quota poll, auth, circuit, 429) → each account.
- **per-upstream-config** (model list) → once, with any available account's key.

## 4. Credential pool — storage & migration

New file: `~/.model-proxy/<name>_apikeys.json` (plural):

```json
{
  "version": 1,
  "accounts": [
    { "id": "a1b2c3d4e5f6a7b8", "label": "home", "api_key": "…", "added_at": "2026-07-07T12:00:00Z" },
    { "id": "9f8e7d6c5b4a3928", "label": "team", "api_key": "…", "added_at": "2026-07-07T12:05:00Z" }
  ]
}
```

- `id` = stable account identifier (§5). Used as the virtual-id suffix and the
  dedup key. Stable across restart, so `quota_state.json` / sticky resume correctly.
- `label` = human name for `usage`/`logout` display and `--label` targeting.
  Defaults to `id` if unset.
- `volcengine` accounts additionally carry `access_key` / `secret_key` (the pool
  entry is the full `{api_key, access_key, secret_key}` triple).

**Backward compatibility (non-destructive):**
- On load, if `<name>_apikeys.json` is absent but the legacy singular
  `<name>_apikey.json` exists, treat it as a read-only 1-entry pool in memory.
- The singular file is migrated to the plural pool lazily on the next `login`/
  `logout` write. We never delete the singular file until the plural exists.
- Result: upgrading introduces the feature with zero migration step.

## 5. Account identity & dedup

`accountIDFor(providerID, credential) string` — provider-specific extraction:

| provider_id | accountID source | strength |
|---|---|---|
| `codex` (phase 2) | `chatgpt_account_id` claim from id_token JWT (existing `parseJWT` helper) | account-level |
| `aqp` (phase 2) | `email` / `project_id` from auth/info | account-level |
| `volcengine` | `access_key` | account-level |
| `zhipu`, `deepseek` | `sha256(api_key)[:16]` | key-level (detects exact-duplicate re-entry; a regenerated key looks new) |

**Dedup on login:** compute the new credential's `id`; if it already exists in the
pool → prompt *"account 'home' is already logged in — replace its key? [y/N]"*; on
yes replace the entry (keeping its label), on no abort. New id → append.

**Limitation (called out, not hidden):** for zhipu/deepseek the id is key-derived,
so re-login after regenerating an API key is treated as a *new* account (the old
entry stays). Mitigation: `login <provider> --label home` lets the user target an
existing label explicitly (replace-by-label), which is the reliable override.

## 6. CLI semantics

| command | behavior with a pool |
|---|---|
| `login <provider>` | Reads one credential → computes id → dedup (§5) → append/replace in pool file. Optional `--label <name>`; `--replace` skips the replace-prompt for scripted use. After writing, **signals the running daemon to reload** (same SIGHUP path as `model-proxy reload`) so `buildProviders` re-runs and the new account is live without a restart; in foreground mode the next request re-reads. (The config file itself is unchanged — only the credential pool file changed, and `reload` already rebuilds providers from it.) |
| `logout <provider>` | **Interactive** (confirmed default): list pool accounts (`#id  label  added_at`), prompt which to remove; `--label <name>` / `--all` non-interactive. Removes the entry; deletes the pool file when it becomes empty. |
| `usage [provider]` | No-arg: unchanged (all providers). `usage zhipu` (**default = all accounts**): iterate the pool, fetch each account's usage with its own key, print one block per account headed by `label (#id)`, separated by the existing divider. `usage zhipu --label home` → one account. |
| `models refresh <provider>` | Model list is per-upstream, not per-account → **runs once**, using the first available account's key. |

`cmdUsage` / `cmdLogin` / `cmdLogout` resolve the typed parent name to its virtual
children via a `parent→children` index built at `buildProviders` time (§7). When
pool size is 1 the parent name is itself the only virtual, so resolution is a no-op.

## 7. buildProviders — virtual unrolling + credential binding

In `buildProviders(cfg)`, for each config provider `name`:

1. Load its credential pool (`<name>_apikeys.json`, with singular-file fallback). →
   `accounts []poolAccount` (size 1..N).
2. For each account `a`, build a virtual provider:
   - **virtual id**: `name` if `len(accounts)==1`, else `name#a.id`.
   - **auth**: `newAuthProvider` gets a new binding param so it uses `a`'s key
     directly instead of reading the file by name. Concretely, `ApiKeyBase` gains a
     `NewApiKeyBaseWithKey(name, key)` constructor (sets the in-memory cache, skips
     file reads) — or a `boundKey` field; `aqp`/`codex` (phase 2) get analogous
     in-memory token bindings.
   - **UsageFn / QuotaFn / FetchModelsFn closures**: re-bound to `a`. Today
     `showZhipuUsageData(cfg, name, prov)` reads the file by `name`; it changes to
     take the explicit key/entry (`showZhipuUsageData(cfg, name, prov, a)`), so each
     virtual fetches *its own* account's usage/quota.
   - Config fields (base URLs, models, usage_url, peak_hours, billing, provider_id)
     are shared from the single config entry — unchanged.
3. Register all virtuals in the returned `providers` map.
4. Build `poolIndex map[string][]string`: parent `name` → sorted virtual ids.
   Commands (§6) and the observability layer (§10) use it.

`Proxy` stores `poolIndex` (rebuilt on `reload`).

## 8. Route expansion

`cfg.Routes[exposed]` is `[]RouteTarget`. A target naming a pooled parent must fan
out to its N virtual children (same `model` + `priority`).

- Precompute `expandedRoutes map[string][]RouteTarget` at load/reload (stored on
  `Proxy`, rebuilt alongside providers).
- Every site that reads `cfg.Routes[exposed]` today — `forward`, `decideOrder`
  (via `scheduleStatus`), `doctor` — reads `expandedRoutes[exposed]` instead.
- A target whose provider is **not** pooled (size 1) passes through unchanged.

No change to `RouteTarget` schema, `schedule`, `decideOrder`, or the failover loop:
they already operate on a flat `[]RouteTarget`.

## 9. Session-sticky scheduling (the unified mechanism)

The pool's load-balancing behavior is **session-level sticky** — it unifies
prompt-cache friendliness with concurrent distribution:

- **One Claude Code conversation** (one `x-claude-code-session-id`) parks on ONE
  account for `sticky_dwell` → consecutive turns hit the same account → the Anthropic
  prompt cache (TTL ~5m) stays warm. `sticky_dwell` (10m ≈ 2× TTL) is unchanged.
- **Different conversations** are assigned to DIFFERENT accounts on first contact →
  concurrent sessions spread across the pool naturally.

There is **no `strategy` config field** — session-sticky is the only pool behavior
(YAGNI on per-request `spread`). A client without `x-claude-code-session-id`
(non-Claude) falls back to model-keyed sticky (today's behavior), so nothing
regresses.

### The key change + the one necessary complement

1. **Sticky map keyed by session, not model.** Today `p.sticky[exposed]` (key = route
   name). Change to `p.sticky[sessionKey]` where `sessionKey` = the request's
   `x-claude-code-session-id` if present, else `exposed` (fallback). `forward` reads
   the header and threads it through `schedule` → `decideOrder`.
2. **Round-robin assignment for new sessions (the complement — without this it does
   NOT spread).** Naively re-keying by session is insufficient: a new session picks
   "highest surplus", and all pool accounts start at equal surplus, so concurrent new
   sessions would ALL pile on the first account (list-order tiebreak). So when
   **assigning** a new session (no sticky entry, or dwell expired), pick the pool's
   next account via a **per-parent round-robin counter** instead of "best surplus".
   The counter advances only at assignment, not per request.

### Mechanics (all in `proxy.go`)

- **Sticky key:** `decideOrder` gains `sessionKey string`; `sk := sessionKey`; if
  `sk == ""` → `sk = exposed`.
- **New state:** `Proxy.spreadCtr map[string]uint64` (parent → assignment counter),
  guarded by `healthMu`, reset on `reload`. Advanced only when a new session is
  assigned.
- **`decideOrder` flow:**
  1. Snapshot health + quota; compute available targets; rank by tier→priority→surplus
     (unchanged).
  2. `cur := p.sticky[sk]`. If `cur` is available and within `dwell` → **reuse it**
     (cache hit). For a **session** key (`sk != exposed`), reuse also refreshes
     `since`, so an active conversation parks on its assigned account for its
     whole lifetime and re-rolls only when that account becomes unavailable
     (429/circuit) — maximal prompt-cache warmth (user-ratified design: a
     conversation locks to one account; it does NOT migrate to a marginally
     better-healthy account mid-conversation). The after-dwell
     tier/priority/surplus-margin re-evaluation applies ONLY to route-keyed
     (non-session) sticky, keeping that path byte-identical to before.
  3. Else **assign**:
     - If the route has a pooled parent: take its available band (the parent's
       virtuals), sorted by stable account-id; `start := spreadCtr[parent] %
       len(band)`; if `commit`, `spreadCtr[parent]++`; pick `band[start]`; set
       `sticky[sk] = pick`. (A `plan` pool still beats a `payg` provider by tier.)
     - If not pooled: existing best-surplus + switch-margin assignment, keyed by `sk`.
- **peek vs commit:** `decideOrder` is called by `schedule` (commit) and
  `scheduleStatus` (peek, no request → `sessionKey=""`). The counter advances only on
  `commit`, so `/debug/schedule` doesn't disturb live assignments.
- **Eviction:** keying by session makes `p.sticky` unbounded over time. Evict expired
  entries opportunistically on each `schedule` (drop entries whose `since + dwell <
  now`). Per-session entries are **not** persisted across restart (a restarted proxy
  breaks active SSE streams; sessions reconnect as new anyway) — `quota_state.json`
  keeps the non-session (model-keyed) entries best-effort.

```go
// sketch
func (p *Proxy) decideOrder(..., sessionKey string, commit bool) (ordered, stickyToSet string) {
    p.healthMu.Lock(); defer p.healthMu.Unlock()
    sk := sessionKey; if sk == "" { sk = exposed }
    avail := filterAvailable(targets)
    if cur := p.sticky[sk]; cur.available && now.Sub(cur.since) < dwell {
        return orderWithFirst(avail, cur.provider), ""   // reuse (cache hit)
    }
    if parent, pooled := routePoolParent(targets); pooled {
        band := poolBandSortedByID(avail, parent)
        start := int(p.spreadCtr[parent]) % len(band)
        if commit { p.spreadCtr[parent]++ }
        pick := band[start]
        return orderWithFirst(avail, pick.Provider), pick.Provider   // assign (round-robin)
    }
    // ---- non-pooled: existing tier→priority→surplus best-pick + switch-margin ----
}
```

### Why this beats the two naive options

| mode | prompt cache | concurrent spread |
|---|---|---|
| sticky by model (today) | warm | none — every session piles on one account |
| per-request round-robin | dead — every turn switches account | even, but cache-hostile |
| **session-sticky (this design)** | **warm within a conversation** | **different conversations → different accounts** |

### Interaction guarantees

- Failover: unchanged — if the parked account 429s/errors, the request fails over to
  the next target; the next request for that session re-assigns (parked account now
  unavailable).
- Tier/priority still rule first — assignment picks within the top tier's pool band.
- A single session firing parallel tool calls still parks on one account (cache
  requires it). Per-request rotation is deliberately NOT supported (YAGNI).

## 10. Observability

| surface | change |
|---|---|
| `quotaTracker` (background poll) | **None.** Iterates the providers map; virtuals are already in it → each account polled independently, snapshotted, persisted. |
| `GET /debug/schedule` + `model-proxy schedule` | Logic unchanged (keyed by provider name = virtual id). **Display:** group virtuals under their parent (`zhipu (3 accounts)` with the children indented) using `poolIndex`, so a 3-account pool reads as one logical provider with per-account surplus/availability/peak. |
| `model-proxy doctor` | Same grouping. Dry-run order shows the session-sticky assignment (new sessions round-robin across accounts; no live quota → falls back to priority). |
| `usage` | Per-account blocks (§6). |
| `quota_state.json` | Keys become virtual ids when pooled (stable across restart because `accountID` is stable). A leftover orphan key (e.g. after an account is removed) is harmless — it ages out / is ignored. |

## 11. What stays unchanged (the free wins)

- `proxy.go:forward`, `tryTarget`, the failover loop, 401-refresh, streaming.
- `providerHealth` (circuit breaker), `recordFailure`/`recordRateLimit`/`takeHalfOpenSlot`.
- `quota.go` (`quotaTracker`, `QuotaSnapshot.Surplus`, `decideOrder` ranking).
- `provider.Provider` interface surface (only `ApiKeyBase` + `newAuthProvider` get a
  binding variant; `Quota`/`Surplus`/`FetchModels` unchanged).
- Config schema for routes, scheduling, peak_hours, billing.
- All existing tests for single-account behavior.

## 12. Edge cases

- **Empty pool / not logged in:** parent resolves to zero virtuals → route has no
  available target → existing "all targets failed" 502 path. `usage`/`models refresh`
  print "not logged in; run `model-proxy login zhipu`".
- **Pool shrinks 1→0:** delete the plural pool file. Next `login` recreates it.
- **Pool grows 1→2:** virtual ids switch from `zhipu` to `zhipu#…`; any old
  `quota_state.json`/sticky entry keyed `zhipu` becomes an orphan (ignored, harmless).
- **Reload mid-request:** the snapshot/RLock pattern is unchanged; `expandedRoutes`
  + `poolIndex` are rebuilt under the same `reload` `mu.Lock` as providers.
- **Account removed while in-flight:** in-flight request keeps its provider snapshot;
  next request no longer schedules it.
- **`--label` not found:** error with the list of existing labels.
- **volcengine pool:** each entry needs the full `{api_key, access_key, secret_key}`
  triple for `GetAFPUsage` (V4 signing) — `login volcengine` must capture all three
  per account, and `accountID = access_key`.

## 13. Test contract (per repo conventions — exact values, no fakes)

- **Pool storage:** round-trip N accounts through login; assert the plural file has
  exactly those `id`/`label`/`api_key` entries and the singular fallback wraps a
  legacy file as a 1-entry pool.
- **Dedup:** login the same zhipu key twice → assert pool size stays 1 (not 2) and
  the replace prompt path is taken (capture via a stubbed stdin). Login a different
  key → size 2.
- **Unrolling:** 3-account `zhipu` → `buildProviders` returns exactly 3 keys
  `zhipu#<id1..3>`; each `AuthHeaders` injects a **distinct** `Bearer <keyN>` (assert
  exact tokens, not "non-empty" — green-signal guard).
- **Route expansion:** route `{zhipu, glm-5.2}` with a 3-pool → `expandedRoutes`
  yields 3 targets, same model + priority; a forward test captures the upstream
  `model` rewrite and asserts which account served it.
- **Session-sticky assignment:** 3 accounts, 3 distinct `x-claude-code-session-id`
  headers → each session lands on a DISTINCT account (round-robin assignment).
  Same session id repeated within `sticky_dwell` → same account every time (cache
  hit). No session header → falls back to one account for all requests (model-keyed).
  One account circuit-opened → new sessions skip it and round-robin over the rest.
- **Failover still works under pool:** account A 429s → next request routes to B
  (assert the rate-limited provider name, not just a count — per the 429-refresh
  contract).
- **Per-account quota:** two snapshots with different `remaining` → assert each
  virtual's `Surplus` is independent.
- **`usage`/`logout` pool CLI:** assert `usage zhipu` prints N blocks (count + label
  headers); `logout zhipu --label home` removes exactly that entry.
- Race-clean (`go test -race`): concurrent requests with distinct sessions (each
  round-robin-assigned) + a reload that adds an account → no race; post-reload
  requests can hit the new account.
- Coverage: maintain the 80% `scripts/cover.sh` gate.

## 14. Phasing

- **Phase 1 (this plan):** pool storage + migration; accountID + dedup; login/logout/
  usage pool CLI; virtual unrolling + credential binding; route expansion;
  session-sticky assignment; observability grouping. Scoped to **apikey providers**
  (`zhipu`, `deepseek`, `volcengine`).
- **Phase 2 (follow-up spec):** extend pooling to `codex` and `aqp` — their OAuth
  token bundles (refresh tokens, JWT-derived account id, SSO cookies) need
  per-account refresh/rotation and richer pool entries. The unrolling architecture is
  already provider-agnostic; phase 2 is the accountID + binding work for those two.

## 15. Open questions (to resolve before/while writing the plan)

1. **~~`strategy` default when pooled?~~** Resolved: there is no `strategy` field —
   session-sticky is the only pool behavior (see §9). Dropped per-request `spread`
   (YAGNI).
2. **`--label` uniqueness** — enforce unique labels within a pool (reject duplicate),
   or allow duplicates and disambiguate by `#id`? Lean: enforce unique.
3. **Round-robin scope** — counter per parent, or per (route × parent)? Per-parent is
   simpler and sufficient (a parent's accounts rotate globally). Lean per-parent.
4. **`usage` per-account latency** — fetching N accounts serially could be slow;
   fetch concurrently? Lean: concurrent with a small cap, render in label order.
