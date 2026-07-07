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
- Per-account prompt-cache awareness (spreading trades cache hits for even load;
  that is the accepted trade-off for `strategy: spread`).
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
schedule: ranks the 3 virtuals by tier→priority→surplus (or round-robin if spread)
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

## 9. strategy: spread vs sticky

New optional provider field:

```yaml
providers:
  zhipu:
    provider_id: zhipu
    strategy: spread   # omit = "sticky" (default, current behavior)
```

### sticky (default) — zero new scheduling code

After unrolling, `zhipu#acct1` etc. are ordinary providers, and the existing
`decideOrder` sticky logic (`proxy.go:652-705`) keys entirely on provider name. So a
pooled provider under `sticky` parks on one account's virtual id for `sticky_dwell`,
and fails over to the next account on 429 — with **no change to `decideOrder`**. This
is the "pool gives failover but not proactive spreading" mode.

### spread — a localized branch in decideOrder

`spread` is the only place that touches scheduling logic. Changes, all in
`proxy.go`:

- **New state:** `Proxy.spreadCtr map[string]uint64` (parent → counter), guarded by
  `healthMu`, reset on `reload` alongside `health`/`sticky`. Per-parent granularity.
  Ephemeral (restart-zeroed is fine — only affects "who is first" after restart).
- **New helpers from `buildProviders`:** `parentOf map[string]string` (virtual id →
  parent) and `spreadParents map[string]bool`. `isSpreadRoute(targets)` = any target's
  parent is in `spreadParents`.
- **`decideOrder` gains a `commit bool` param** and a leading branch:
  - **peek vs commit:** `decideOrder` is called by `schedule` (commit, `proxy.go:586`)
    and `scheduleStatus` (peek, `proxy.go:275`). When `commit` and spread, it reads
    `start := spreadCtr[parent]` **and bumps it**, both inside its single `healthMu`
    critical section → atomic read+bump (no window where two concurrent requests read
    the same `start`). Peek reads without bumping.
  - **spread branch:** skip the sticky block entirely (`stickyToSet = ""`); order the
    spread parent's available band by **stable account-id** (the `#id` suffix) rotated
    by `start`; non-spread targets (e.g. a `payg` deepseek) still rank by
    tier→priority→surplus and merge in by tier.
  - **band ordering uses account-id, not surplus** — round-robin must be predictable
    (the "6 requests → 2 each" test is deterministic). Surplus is still computed for
    display only.

```go
// sketch
func (p *Proxy) decideOrder(..., commit bool) (ordered, stickyToSet) {
    p.healthMu.Lock(); defer p.healthMu.Unlock()
    avail := filterAvailable(targets)
    if p.isSpreadRoute(targets) {
        band, rest := splitSpreadBand(avail)        // band = this parent's virtuals
        start := p.spreadCtr[parent]
        if commit { p.spreadCtr[parent]++ }         // atomic w/ the read (same lock)
        return mergeByTierPriority(rest, rotateByID(band, start)), ""
    }
    // ---- existing sticky path, byte-for-byte unchanged (proxy.go:641-705) ----
}
```

### Interaction guarantees

- Failover: `decideOrder` still returns the full ordered list; the round-robin pick is
  placed first, the rest follow. The existing failover loop iterates them unchanged.
- Tier/priority still rule first — `spread` only reorders within an equal band, so a
  `plan` spread-pool still beats a `payg` provider; payg stays last-resort.
- Prompt-cache: `spread` intentionally forfeits per-account stickiness — the accepted
  trade-off for even concurrent distribution.
- A route that mixes a spread pool and non-spread providers: the spread band
  round-robins internally; non-spread members keep tier/priority/surplus ordering.

## 10. Observability

| surface | change |
|---|---|
| `quotaTracker` (background poll) | **None.** Iterates the providers map; virtuals are already in it → each account polled independently, snapshotted, persisted. |
| `GET /debug/schedule` + `model-proxy schedule` | Logic unchanged (keyed by provider name = virtual id). **Display:** group virtuals under their parent (`zhipu (3 accounts)` with the children indented) using `poolIndex`, so a 3-account pool reads as one logical provider with per-account surplus/availability/peak. |
| `model-proxy doctor` | Same grouping. Dry-run order shows the round-robin/surplus order within a spread pool. |
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
- **Spread round-robin:** `strategy: spread`, 3 accounts, 6 sequential requests →
  assert each account serves exactly 2 (capture provider per request via the
  existing `refreshHook`-style capture). One account circuit-opened → it gets 0,
  the other two split 3/3.
- **Failover still works under pool:** account A 429s → next request routes to B
  (assert the rate-limited provider name, not just a count — per the 429-refresh
  contract).
- **Per-account quota:** two snapshots with different `remaining` → assert each
  virtual's `Surplus` is independent.
- **`usage`/`logout` pool CLI:** assert `usage zhipu` prints N blocks (count + label
  headers); `logout zhipu --label home` removes exactly that entry.
- Race-clean (`go test -race`): concurrent spread requests + a reload that adds an
  account → no race; post-reload requests can hit the new account.
- Coverage: maintain the 80% `scripts/cover.sh` gate.

## 14. Phasing

- **Phase 1 (this plan):** pool storage + migration; accountID + dedup; login/logout/
  usage pool CLI; virtual unrolling + credential binding; route expansion;
  `strategy: spread`; observability grouping. Scoped to **apikey providers**
  (`zhipu`, `deepseek`, `volcengine`).
- **Phase 2 (follow-up spec):** extend pooling to `codex` and `aqp` — their OAuth
  token bundles (refresh tokens, JWT-derived account id, SSO cookies) need
  per-account refresh/rotation and richer pool entries. The unrolling architecture is
  already provider-agnostic; phase 2 is the accountID + binding work for those two.

## 15. Open questions (to resolve before/while writing the plan)

1. **`strategy` default when pooled?** Design says `sticky` default (opt into
   `spread`). Alternative: pooling implies `spread` unless `strategy: sticky`. Lean
   keep-default-sticky for predictability; confirm.
2. **`--label` uniqueness** — enforce unique labels within a pool (reject duplicate),
   or allow duplicates and disambiguate by `#id`? Lean: enforce unique.
3. **Round-robin scope** — counter per parent, or per (route × parent)? Per-parent is
   simpler and sufficient (a parent's accounts rotate globally). Lean per-parent.
4. **`usage` per-account latency** — fetching N accounts serially could be slow;
   fetch concurrently? Lean: concurrent with a small cap, render in label order.
