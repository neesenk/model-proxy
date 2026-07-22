# qwen-plan provider — design spec

- **Date:** 2026-07-23
- **Status:** Design (approved direction; pending spec review)
- **Scope:** Add a `qwen-plan` provider for the **千问 AI Token Plan 个人版**
  (personal edition), so model-proxy can drive it through Claude Code / Cursor /
  other interactive tools. Dual-protocol OpenAI + Anthropic byte-level passthrough,
  one Bearer API key. The personal-edition **Credits usage is console-only** (no
  public API), so the provider exposes only forwarding + model listing and treats
  quota as unmeasured, relying on the existing 429 → cooldown → failover path.

## 1. Context & goal

Token Plan 个人版 is Alibaba Cloud Bailian (DashScope) personal AI subscription.
It works with Claude Code, Cursor, Qwen Code, Qoder, OpenClaw, etc. — which means
it must speak both OpenAI- and Anthropic-compatible protocols. A single subscription
key (`sk-sp-…`) serves both.

model-proxy should treat it like the existing dual-protocol Bearer providers
(deepseek / zhipu): pure passthrough, no protocol conversion. The user explicitly
scoped the work to the personal edition and named the only two genuinely unknown
surfaces — **the Models interface** and **the 用量 (usage/credits) interface** —
which this spec resolves by probing.

### Decisions locked with the user
1. **Provider id:** `qwen-plan`.
2. **Quota/usage handling:** static + reactive. `Quota()` returns `BillingUnknown`;
   the proxy relies on the existing 429 classification + cooldown + failover.
   The CLI `usage` command and Web UI print the subscription console URL
   (`https://platform.qianwenai.com/home/billing/subscription/token-plan-individual`)
   so the user can click through for real numbers.

### Non-goals (explicitly out of scope)
- **No proactive Credits/usage fetch.** There is no public usage API (probed; see
  §3). No console-cookie scraping, no local credit estimation — both intentionally
  omitted (see §7 intentional-behavior note on the platform's "严禁 API 调用" rule).
- **No team edition.** Personal edition only (Lite/Standard/Pro tiers).
- **No protocol conversion.** qwen-plan speaks whatever the client speaks.
- **No OAuth.** The key is a long-lived `sk-sp-` API key stored in the apikey pool.

## 2. Ground truth — probed facts

Source: `platform.qianwenai.com/docs/token-plan/personal/*` (overview, quickstart,
faq) + Aliyun Model Studio mirror + live endpoint probes (no key, observing the
gateway's auth-error shape).

### Forward endpoints & auth
- **OpenAI base:** `https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1`
- **Anthropic base:** `https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic`
  (**no `/v1`** — the proxy keeps the client's `/v1/messages`, same convention as
  deepseek/zhipu; enforced by `config.go` validation that rejects a trailing `/v1`).
- **API key:** `sk-sp-xxxxx` (token-plan-specific; **not** the generic `sk-xxxxx`,
  and **not** `dashscope.aliyuncs.com`). Key + base URL are paired and isolated
  from pay-as-you-go.
- **Auth:** Bearer. (Implementation note: see §4 auth — dual-write Bearer + x-api-key
  as the robust default since the Anthropic gateway's exact preference is untestable
  without a key.)

### Models (probe #1 — resolved)
- `GET <openai_base_url>/models` is a **real routed endpoint**: a no-auth request
  returns a well-formed `{"request_id":...,"code":"InvalidApiKey","message":"No
  API-key provided."}` (HTTP 401), whereas a non-existent path 404s. With a valid
  key it is expected to return the OpenAI `{data:[{id}]}` shape → `FetchModels`
  reuses `fetchModelsBearer`.
- The documented personal-edition model list (fallback if `/models` is hidden for
  subscription plans, as the Coding-Plan FAQ warns it can be):

  | Brand | Model ID | Capability |
  |---|---|---|
  | 千问 | `qwen3.8-max-preview` | reasoning, vision, text |
  | 千问 | `qwen3.7-max` | reasoning, text |
  | 千问 | `qwen3.7-plus` | reasoning, vision, text |
  | 千问 | `qwen3.6-flash` | reasoning, vision, text |
  | 智谱 AI | `glm-5.2` | reasoning, text |
  | DeepSeek | `deepseek-v4-pro` | reasoning, text |
  | 万相 | `wan2.7-image` / `wan2.7-image-pro` | image generation (separate endpoint) |
  | HappyHorse | `happyhorse-1.1-i2v` / `-t2v` / `-r2v` | video generation (separate endpoint) |

  Image/video models use separate generation endpoints and are **omitted from the
  chat `models:` list** (documented as a note, not configured).

### Usage / Credits (probe #2 — resolved: there is no public API)
- **No documented usage/quota endpoint.** The API-reference nav (chat / images /
  video / platform-API) has no usage/billing call. The Coding-Plan FAQ states
  plainly *"模型列表不支持通过接口查询"* and personal-edition docs state
  *"实际消耗以控制台订阅页用量明细为准"* (consumption is per the console
  subscription page). Web search across Aliyun docs confirms no public REST API.
- **Billing model (for display/notes):** Credits, two **independent** fixed windows;
  either hitting the cap pauses service, surfaced as `429 Allocated quota exceeded`.

  | Tier | 5-hour window | 7-day window |
  |---|---|---|
  | Lite | 700 Credits | 2,500 Credits |
  | Standard | 3,000 Credits | 10,000 Credits |
  | Pro | 12,000 Credits | 40,000 Credits |

  Each call debits **both** windows simultaneously. Windows start at first call;
  unused Credits do not carry over. `429 Requests rate limit exceeded` = transient
  request-rate (concurrency), distinct from quota.

## 3. How quota is handled (the user's option 1)

- **`Quota()`** returns `BillingUnknown`, **no network call**, carrying `Notes`:
  - `"Credits usage (5h/7d windows) is viewable only in the console"`
  - `"Subscription details: https://platform.qianwenai.com/home/billing/subscription/token-plan-individual"`
- The surplus scheduler therefore treats qwen-plan as unmeasured → ranked by
  priority (neutral surplus), same as a `static` provider. No measured ranking.
- **Reactive cooldown is already correct, zero new failure-classification code:**
  - `"Allocated quota exceeded"` → lowercased contains the marker `"quota exceeded"`
    (`failclass.go` `quotaExhaustedMarkers`) → `rlQuota` → default **1h** cooldown
    (or a body reset-hint if present, capped at 7d) → scheduler skips it and
    **failovers to other accounts/providers**, re-probing after cooldown.
  - `"Requests rate limit exceeded"` → matches no marker → `rlTransient` → 60s.
- Verification item (impl): confirm the real 429 body shape and whether it carries
  a reset hint (`parseResetHint` honors it if so).

## 4. Design — the provider (`model-proxy/provider/qwen_plan.go`)

`QwenPlanProvider` struct embedding `*ApiKeyBase` + `baseProbe`, holding
`cfg`/`providerName`. Registered via `init()` → `Register("qwen-plan", …)`, built
with `newApiKeyBaseBound(cfg, providerName)` (same as zhipu/deepseek).

| Method | Behavior |
|---|---|
| `AuthHeaders` | Override `ApiKeyBase`: set **both** `Authorization: Bearer <key>` **and** `x-api-key: <key>` (deepseek/zcode pattern). Robust to the Anthropic gateway preferring either; the OpenAI endpoint ignores `x-api-key`. Falls back to Bearer-only if impl probing shows dual-write is unnecessary. |
| `Refresh` / `Logout` | Inherited from `ApiKeyBase` (clear cache / `DeleteKey`). |
| `RewriteRequest` | No-op (pure passthrough; base URL selected by protocol in `proxy.forward`). |
| `FetchModels` | `fetchModelsBearer(cfg, AuthHeaders)` → `GET <openai_base_url>/models`. On non-200/parse failure returns the error; the caller (`models refresh`) keeps config `models:`. |
| `Quota()` | `&QuotaSnapshot{Billing: BillingUnknown, Notes: [...console-only + URL...], AsOf: now}`, no fetch. |
| `Usage()` | Prints `Provider:` line, the static 5h/7d + Lite/Std/Pro note, **the console URL**, then the model list (live via `FetchModels`, fallback `listConfigModels(cfg.Models)`). |
| `ProbeRequest` / `ExtraHeaders` / `FilterModelIDs` | `baseProbe` defaults (OpenAI `/chat/completions` probe; no extra headers; passthrough filter) — **no overrides**. |
| `ProtocolHint` / `WireProtocolNote` | None — dual-protocol passthrough, no conversion. |

## 5. Wiring (main package)

| Touchpoint | File:line | Change |
|---|---|---|
| provider-id allowlist (**the one hard gate**) | `config.go:623` | add `"qwen-plan": true` to `known` |
| error string | `config.go:625` | append `qwen-plan` to the enumerated valid ids |
| empty-id hint | `config.go:618` | append `qwen-plan` (cosmetic) |
| `buildOne` | `proxy.go:246-272` | **none** — falls through the switch (only codex/volcengine branch) |
| **login validation fallback** | `login.go:122-254` | new `apiKeyValidationURL(prov)` helper: returns `prov.UsageURL` if set, else `strings.TrimRight(prov.OpenAIBaseURL,"/")+"/models"` if openai_base_url set, else `""`. Used by both `runApiKeyLoginWithInput` (the "Validating…" stderr gate, :137) and `addApikeyAccount` (the `validateKeyBearerGET` arg, :191). |
| logout | `main.go:346-381` | **none** — apikey-pool default path |
| web account CRUD / async login | `web.go:899-1130` | **none** — apikey default path; no async login needed |
| takeover/restore | `takeover.go`, `main.go:313/325` | **none** — dispatches on client name, not provider id |
| `ProtocolHint`/`WireProtocolNote` | `provider/protocol_hint.go` | **none** |
| *(optional polish)* `quotaSourceLabel` | `main.go:1036-1054` | add `case "qwen-plan"` → `"console-only (5h/7d Credits)"` |

**Why the login fallback is safe:** zhipu/deepseek/volcengine/kimi-code all set
`usage_url` → the fallback never triggers for them → no behavior change. Only
qwen-plan (the one apikey provider without `usage_url`) validates via
`openai_base_url/models`. It is field-based, not `if providerID == "qwen-plan"`,
so it honors the "no main-package provider_id branching" rule (AGENTS.md).

## 6. Config example (`config.yaml` + `defaults.go`)

```yaml
qwen-plan:
  provider_id: qwen-plan
  openai_base_url: https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
  anthropic_base_url: https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic  # NO /v1
  models:
    - qwen3.8-max-preview
    - qwen3.7-max
    - qwen3.7-plus
    - qwen3.6-flash
    - glm-5.2
    - deepseek-v4-pro
```

No `usage_url` — login validates the key by probing `openai_base_url/models`
(§5 fallback). The example block is added to both `config.yaml` and
`defaults.go` `defaultConfigYAML` (the `config init` template).

## 7. Docs (AGENTS.md rule 4 — code + docs in one commit)

- **`docs/backend-contracts.md`** — new authoritative `qwen-plan` section: the two
  base URLs, Bearer (dual-write) auth, `/models`, the "no public Credits-usage API
  → console-only" fact, and the two 429 strings. Append a row to the token-file
  naming table: `qwen-plan` → `<name>_apikey.json` (`{api_key}`).
- **`model-proxy/CLI.md`** — login/usage dispatch prose + the `quotaSourceLabel`
  value; note the `usage_url`-optional / `openai_base_url/models` fallback.
- **`model-proxy/README.md`** — provider block, login/usage examples, credential
  storage table row.
- **`docs/decisions/intentional-behaviors.md`** — the platform's "严禁 API 调用"
  restriction: model-proxy is an allowed interactive dev tool (forwarding for
  Claude Code/Cursor/etc.), so forwarding is legitimate; but background
  quota-polling / console-scraping is intentionally **not** implemented to respect
  the clause, which is why usage is console-only.

### Pre-existing, out-of-scope flag
`README.md:451-458` says deepseek's `anthropic_base_url` "须含 /v1", contradicting
the code (`config.go:628-630` rejects a trailing `/v1`) and `config.yaml:72`. This
change does **not** touch it (unrelated to qwen-plan); flagged for a separate fix.

## 8. Testing (TDD, per `docs/engineering/testing.md`)

- **`provider/qwen_plan_test.go`:**
  - `AuthHeaders` injects **both** `Authorization: Bearer` and `x-api-key`.
  - `FetchModels` parses OpenAI `{data:[]}`; returns error on non-200.
  - `Quota()` returns `BillingUnknown` with both `Notes` lines (incl. the console URL).
  - `Usage()` output contains the console URL.
  - Credential isolation: bound key (`BoundAPIKey`) vs file-backed.
- **429-classification regression** (`failclass` test): `"Allocated quota exceeded"`
  → `rlQuota`; `"Requests rate limit exceeded"` → `rlTransient` (guards the reactive
  path; should pass with no code change).
- **login fallback** (`login_test.go`): usage_url empty + openai_base_url set →
  validation GETs `openai_base_url/models`; usage_url set → unchanged; both empty
  → no validation.
- **config validation** (`config_test.go`): qwen-plan block accepted; `anthropic_base_url`
  ending `/v1` rejected; unknown provider_id without the allowlist entry rejected.

## 9. Implementation order (sketch — detailed plan via writing-plans)

1. Allowlist + config example + `defaults.go`.
2. `qwen_plan.go` + `qwen_plan_test.go` (TDD).
3. `login.go` `apiKeyValidationURL` fallback + test.
4. `backend-contracts.md` + `CLI.md` + `README.md` + `intentional-behaviors.md`.
5. `go test ./...`, `scripts/cover.sh` (≥80%), `git diff --check`.
