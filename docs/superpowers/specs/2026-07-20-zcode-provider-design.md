# zcode provider — design spec

- **Date:** 2026-07-20
- **Status:** Design (approved direction; pending spec review)
- **Scope:** Add a `zcode` provider that forwards to Zhipu BigModel's Anthropic
  endpoint while presenting ZCode's client fingerprint, so a Coding Plan API key
  gets the Coding Plan quota treatment (0.67 consumption coefficient) and is not
  deprioritized as a "generic agent".

## 1. Context & goal

The user has a **domestic BigModel Coding Plan** (open.bigmodel.cn). They want to
drive it through model-proxy as if the requests originated from the official
**ZCode** desktop client — same endpoint, same protocol, same identity headers —
so the plan's quota discount applies and the soft "official-tools-get-priority"
layer does not throttle the traffic.

ZCode 3.3.6 is installed at `/Applications/ZCode.app`; this spec is grounded in a
live probe of that binary (ASAR + bundled catalog), **not** the third-party
reverse-engineering report (which targeted 3.0.1). Where the two agree the report
is corroborated; where they differ, the probe wins.

### Non-goals (explicitly out of scope)
- **No OAuth/JWT login flow.** The `zcode://oauth/callback` custom-scheme
  redirect cannot be intercepted by a CLI, and the API-key path is sufficient
  (confirmed: ZCode's own `bigmodel-coding-plan` preset hits the same endpoint
  with an API key). The OAuth endpoints/client_ids are real (probed) but unused.
- **No protocol conversion.** zcode speaks Anthropic end-to-end (matches ZCode).
- **No `X-Device-Mid` initially.** ZCode only sends it when populated; omitting
  it is valid ZCode behavior.

## 2. Ground truth — what ZCode 3.3.6 actually sends

Probed from `/Applications/ZCode.app/Contents/Resources/app.asar`
(`out/host/index.js`) and `model-providers/models_catalog_china_llm_zcode_2026-06-03.json`.

The real request to BigModel:

```http
POST https://open.bigmodel.cn/api/anthropic/v1/messages
content-type: application/json
Authorization: Bearer <key>            # BOTH auth headers, same key
x-api-key: <key>
anthropic-version: 2023-06-01
User-Agent: ZCode/<appVersion>         # appVersion from ZCODE_VERSION env / app config → 3.3.6
HTTP-Referer: https://zcode.z.ai
X-Title: Z Code@electron
X-ZCode-App-Version: <appVersion>
X-Platform: <platform>-<arch>          # Node names: darwin|win32|linux - arm64|x64|...
X-Release-Channel: production
X-Client-Language: <Intl locale>       # ASCII-printable only, else "unknown"
X-Client-Timezone: <Intl timeZone>     # ASCII-printable only, else "unknown"
X-Os-Category: macos|windows|linux     # darwin→macos, win32→windows, else linux
X-Os-Version: <os version>             # only if available
X-Device-Mid: <device id>              # only if available (new in 3.3.6; omitted here)
```

Verified facts:
- **Endpoint** = `open.bigmodel.cn/api/anthropic` (catalog: `bigmodel` and
  `bigmodel-coding-plan` share `baseURL https://open.bigmodel.cn` +
  `paths.anthropic /api/anthropic/v1/messages`). Coding Plan does **not** use a
  special base — the coefficient is keyed to the plan/API-key at this endpoint.
- **Auth = `Authorization: Bearer` AND `x-api-key`, same value** (from the
  request builder `Gse`/`buildAnthropicConnectivityAuthHeaders`). This differs
  from the existing `zhipu` provider, which sends Bearer and **deletes**
  `x-api-key`.
- **`anthropic-version` = `2023-06-01`** (literal in the request builder).
- **All 10 fingerprint headers present**; 3.3.6 adds `X-Device-Mid` (conditional).
- **UA** = `` `ZCode/${appVersion ?? "unknown"}` ``.
- **`X-Platform`** = `` `${platform}-${arch}` `` with Node naming.
- **`X-Os-Category`** = `darwin→macos`, `win32→windows`, else `linux`.
- **Language/Timezone** from `Intl.DateTimeFormat().resolvedOptions()`, passed
  through a printable-ASCII normalizer (else `"unknown"`).
- **Models** (catalog, BigModel): glm-5.1, glm-5.1-highspeed, glm-5, glm-5-turbo,
  glm-4.7, glm-4.7-flash, glm-4.7-flashx, glm-4.6, glm-4.5, glm-4.5-air,
  glm-4.6v, glm-4.6v-flash, glm-4.6v-flashx, glm-4.1v-thinking-flash(-x),
  glm-4-flash(-x)-250414, glm-4v-flash, codegeex-4, charglm-4, emohaa.

## 3. Architecture

A new apikey-style provider, structurally aligned with `zhipu` but with two
provider-specific overrides driven by the ground truth above:

```
provider/zcode.go
  ZCodeProvider {
    *ApiKeyBase        // key storage + pool binding (LoadKey/Refresh/DeleteKey)
    baseProbe          // default FilterModelIDs (passthrough); ProbeRequest overridden
    cfg          *Config
    providerName string
  }
  init() → Register("zcode", …)
```

Why embed `ApiKeyBase` but override `AuthHeaders`: the base's `Inject` sets Bearer
and deletes `x-api-key`; ZCode needs **both**. The base is still reused for
`LoadKey()` / `Refresh()` / `DeleteKey()` / pool binding (`NewApiKeyBaseWithKey`
via `cfg.BoundAPIKey`).

### 3.1 AuthHeaders (override — the key divergence from zhipu)
```go
func (p *ZCodeProvider) AuthHeaders(req *http.Request) error {
    key, err := p.LoadKey()
    if err != nil { return err }
    req.Header.Set("Authorization", "Bearer "+key)
    req.Header.Set("x-api-key", key)   // ZCode sends BOTH (same key)
    return nil
}
```
`Refresh()` delegates to `ApiKeyBase.Refresh()`; `Logout()` calls
`ApiKeyBase.DeleteKey()` (mirrors `ZhipuProvider.Logout`).

### 3.2 ExtraHeaders — `anthropic-version` + ZCode fingerprint
Applied last in the forward path (`proxy.go:1221`, after the client-UA whitelist
copy and `prov.Headers`), so it **overrides** whatever UA the client sent.

| Header | Value |
|---|---|
| `anthropic-version` | `2023-06-01` |
| `User-Agent` | `ZCode/` + `zcodeAppVersion` (`const "3.3.6"`) |
| `HTTP-Referer` | `https://zcode.z.ai` |
| `X-Title` | `Z Code@electron` |
| `X-ZCode-App-Version` | `zcodeAppVersion` |
| `X-Platform` | `nodePlatform(GOOS)` + `-` + `nodeArch(GOARCH)` |
| `X-Release-Channel` | `production` |
| `X-Client-Language` | `resolveClientLanguage()` — from env (`LANG`/`LC_ALL`), ASCII-printable else `unknown` |
| `X-Client-Timezone` | `resolveClientTimezone()` — from env (`TZ`) / `time.Local`, ASCII-printable else `unknown` |
| `X-Os-Category` | `darwin→macos`, `windows→win32→windows`, else `linux` |
| `X-Os-Version` | best-effort (`syscall.Sysctl("kern.osproductversion")` on darwin; `/etc/os-release` on linux); omitted if unavailable |

Name-mapping helpers (Go → Node, to match ZCode exactly):
- `nodePlatform`: `darwin→darwin`, `windows→win32`, `linux→linux`.
- `nodeArch`: `amd64→x64`, `arm64→arm64`, `386→ia32`, else passthrough.
- `osCategory`: `darwin→macos`, `windows→windows`, `linux→linux`.

All header values pass a printable-ASCII guard mirroring ZCode's
`normalizePrintableHeaderValue` (non-printable → omit/`unknown`), so a weird
locale never produces a malformed header.

### 3.3 ProbeRequest (override — Anthropic shape)
Same as `aqp`: `POST /v1/messages` + `anthropicProbeBody(modelID)`, so
`models refresh` probes the right path/shape.

### 3.4 Quota / FetchModels / Usage / RewriteRequest
- `Quota()`: GET `cfg.UsageURL` with Bearer (via `AuthHeaders`), parse with the
  existing `ParseZhipuQuota` (zcode **is** BigModel — same quota envelope,
  `TOKENS_LIMIT`/`TIME_LIMIT` windows, `currentValue`/`remaining`/`usage`).
  On any failure returns `BillingUnknown` carrying the error (keeps the poll
  alive), mirroring `ZhipuProvider.Quota`.
- `FetchModels()`: `fetchModelsBearer(p.cfg, p.AuthHeaders)` (GET `/models` on
  the OpenAI base). Best-effort; the `models:` list in config is authoritative
  for routing.
- `Usage()`: prints `Provider:  zcode` as the first line (interface contract),
  then the parsed quota windows (mirror zhipu's usage display).
- `RewriteRequest`: no-op (passthrough).

## 4. Config & routing

```yaml
providers:
  zcode:
    provider_id: zcode
    anthropic_base_url: https://open.bigmodel.cn/api/anthropic   # NO trailing /v1 (config.go:585)
    openai_base_url: https://open.bigmodel.cn/api/paas/v4        # only for /models listing
    usage_url: https://open.bigmodel.cn/api/monitor/usage/quota/limit
    models: [glm-4.6, glm-4.5, glm-4.5-air, glm-4.7, glm-5.1, glm-5]
routes:
  glm-4.6:
    - { provider: zcode, model: glm-4.6 }
```

Claude Code (`/v1/messages`, anthropic) → same-protocol passthrough to
`open.bigmodel.cn/api/anthropic/v1/messages`. No `protocol:` field needed.

## 5. Login flow (`login zcode`)

zcode is an apikey provider → credential pool (`<name>_apikeys.json`), multi-account.
Add a `case "zcode":` in `cmdLogin` (`login.go:56`) that:
1. `openBrowser("https://bigmodel.cn/login")` (honoring the BigModel login URL —
   user logs in, navigates to API Keys, creates/copies a Coding Plan key).
2. Prints a one-line hint pointing at the API-keys page.
3. Delegates to `runApiKeyLoginWithInput(cfg, provName, prov, "", label, replace)`
   (prompt → validate against `usage_url` → dedup → save pool → `maybeReloadDaemon`).

Falls back gracefully if `openBrowser` fails (SSH/remote) — the prompt still works.

## 6. Touch points

| File | Change |
|---|---|
| `provider/zcode.go` | **new** — provider + fingerprint header helpers |
| `provider/zcode_test.go` | **new** — white-box tests (see §7) |
| `config.go:580` | add `"zcode": true` to the `known` whitelist (+ error msg at :582) |
| `login.go:56` | add `case "zcode":` (browser-open + delegate to apikey flow) |
| `main.go:30` | update the `usage` string's `login <provider>` list |
| `main.go:963` | add `case "zcode":` → `"quota/limit"` in `quotaSourceLabel` (for `doctor`) |
| `CLI.md` §4 | document the zcode login flow stdout/stderr contract |
| `docs/backend-contracts.md` | (optional aside) fix the stale `anthropic_base_url …/v1` note at :42/:51 |

**No changes needed:** `buildOne`/`proxy.go` (`BoundAPIKey` handles pool binding
generically, like zhipu/deepseek), `main.go:358` logout dispatch (zcode uses the
pool path, not the `oauth_auth` single-file branch).

## 7. Testing contract (per CLAUDE.md)

White-box (`package main` / `package provider`), stdlib `testing` + `httptest`,
no testify, 80% coverage baseline.

- **AuthHeaders — exact values:** assert `Authorization == "Bearer <key>"` **and**
  `x-api-key == "<key>"` (both present, same value). Deleting either set line must
  turn the test red. (This is the contract that enforces the "both headers"
  divergence from zhipu.)
- **ExtraHeaders — exact fingerprint:** assert `User-Agent == "ZCode/3.3.6"`,
  `HTTP-Referer == "https://zcode.z.ai"`, `X-Title == "Z Code@electron"`,
  `X-ZCode-App-Version == "3.3.6"`, `anthropic-version == "2023-06-01"`, and that
  `X-Platform`/`X-Os-Category` match the runtime mapping for the test platform.
  Each header is an exact-value assertion (no `Contains(x) || Contains(y)`).
- **ProbeRequest:** assert `Path == "/v1/messages"` and anthropic body shape.
- **Quota:** feed a known BigModel quota fixture through `ParseZhipuQuota`,
  assert which window is `Ultimate` vs `Short`, `Duration`, `ResetsAt`.
- **Name-mapping helpers:** table-driven (`darwin/arm64 → darwin-arm64 / macos`,
  `windows/amd64 → win32-x64 / windows`, …).

## 8. Open questions / risks

- **Coefficient on `/api/anthropic`:** the catalog proves Coding Plan uses this
  endpoint, but the live 0.67 coefficient should be confirmed by checking
  `usage_url` numbers after a real call (the reverse-eng `zcodePlanAnthropicBaseUrl`
  is runtime-fetched and not in the static binary — it is not needed given the
  catalog shows the standard endpoint). If the discount does not appear, try
  `anthropic_base_url: https://open.bigmodel.cn/api/coding/anthropic` (config-only
  change, no code).
- **Telemetry header exactness:** `X-Client-Language`/`X-Client-Timezone`/
  `X-Os-Version` are best-effort; they are unlikely to be enforced (ZCode itself
  sends `"unknown"` when they can't be resolved). Overridable via config `headers:`.
- **Spec commit:** per CLAUDE.md, commit only on request — this file is written
  but not committed; commit (on a branch off `main`) when approved.

## 9. Future (not in this spec)
- OAuth/JWT login (if the API-key path is ever deprioritized) — blocked by the
  `zcode://` custom-scheme redirect; would need scheme registration or JWT
  extraction from a real ZCode login.
- `X-Device-Mid`: generate a stable per-install UUID stored under
  `~/.model-proxy/` and send it, for full fidelity.
