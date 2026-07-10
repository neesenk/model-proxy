# Live codex `models refresh`

**Date:** 2026-07-11
**Status:** Approved
**Scope:** `provider/codex.go` `FetchModels`, `buildOne` wiring, `main` version resolver, one optional config field, tests, docs.

## Problem

`model-proxy models refresh codex` always returns `["gpt-5.5"]`. The codex provider's `FetchModels`
hardcodes that single value (`provider/codex.go`), so it cannot surface newer models the backend now
serves (e.g. `gpt-5.6-sol`). The operator has no way to discover the current codex model list through
the proxy.

## Root cause

The codex backend's `/models` endpoint is gated on a `client_version` query param and returns a
**custom** (non-OpenAI) shape. The original author judged client-version correctness "not worth
querying" and hardcoded the list. As a result the list drifts the moment OpenAI ships a new model.

## Reverse-engineered contract (sourced from openai/codex `codex-rs`)

- **Endpoint:** `GET https://chatgpt.com/backend-api/codex/models?client_version=<VER>`
  - Path `models` appended to the base `https://chatgpt.com/backend-api/codex`
    (`model-provider-info/src/lib.rs:38`, `codex-api/src/endpoint/models.rs:31`).
  - `client_version` is the **only** query param (`append_client_version_query`,
    `codex-api/src/endpoint/models.rs:35`).
- **Headers:** `Authorization: Bearer <token>`, `originator: codex_cli_rs` (mandatory, else 403),
  `ChatGPT-Account-Id`. No `session_id`. (Our `cfg.Auth.Inject` already sets all three.)
- **Response shape (custom):** `{"models": [ {slug, display_name, visibility, supported_in_api, priority, …} ]}`.
  The model id is the **`slug`** (e.g. `gpt-5.6-sol`). Not OpenAI `{data:[]}`.
- **Server-side version gating (the crux):** the backend uses `client_version` to decide which models
  to return. A stale/low version → newer models simply absent from the response. Each model carries a
  `minimal_client_version` server-side; codex itself sends its cargo version trimmed to
  `MAJOR.MINOR.PATCH` (`client_version_to_whole`, `models-manager/src/lib.rs:19`, e.g. `0.144.1`).
- **Filtering:** the codex client only treats `visibility == "list"` entries as user-facing
  (`apply_remote_models`, `models-manager/src/manager.rs:399`).
- **Timeout:** codex uses a 5s upstream timeout (`MODELS_REFRESH_TIMEOUT`).

## Design

### 1. `client_version` resolution (in `main`, pure + injectable)

First non-empty source wins:

1. config `providers.codex.client_version` (operator force-override)
2. `codex --version` → parse the `\d+\.\d+\.\d+` token
3. `$CODEX_HOME/models_cache.json` field `.client_version` (fallback `~/.codex`)
4. baked constant `defaultCodexClientVersion = "0.144.1"`

Config wins; auto-detect keeps it current when the codex CLI is installed; the constant guarantees a
value on hosts without codex. The version string is read from the codex **CLI** install while the
**auth token** comes from model-proxy's own OAuth store — independent, no conflict with the proxy's
separate OAuth client.

The resolver takes the config value plus injectable `cliVersion()` / `cacheVersion()` source funcs so
the precedence logic is unit-testable without spawning processes or touching the filesystem.

### 2. Live fetch (in `provider/codex.go`, self-contained)

`FetchModels` keeps its current self-contained style (no `FetchModelsFn` callback) but does a real GET:

- `GET <TrimRight(OpenAIBaseURL,"/")>/models?client_version=<resolved>`
- auth via `cfg.Auth.Inject` (Bearer + originator + ChatGPT-Account-Id)
- 10s client timeout
- parse `{"models":[{slug, visibility, …}]}` — custom shape, **not** `fetchModelsBearer`
- filter `visibility == "list"`; return `slug`s in server order (priority-sorted)
- non-200 → error with truncated body

`client_version` is threaded in via a new `provider.Config.ClientVersion` field, populated by `buildOne`
calling the resolver. Env detection stays in `main`; HTTP stays in the testable `provider` package.

### 3. Config schema

One optional field on the codex provider (mirrors `aqp_mint_url` / `usage_url`):

```yaml
providers:
  codex:
    client_version: "0.144.1"   # optional; auto-detected if omitted
```

### 4. Error handling

Any fetch failure (network / 401 / 403 / parse) → `FetchModels` returns the error; `models refresh codex`
surfaces it. **No silent fallback to a hardcoded list.** The version always resolves (constant
backstop), so only the HTTP call can fail. Refresh is an explicit operator action — a clear error beats
stale data.

## Decisions (confirmed with user)

- **`client_version` source = Hybrid** (auto-detect + config override + constant fallback).
- **Filter `visibility == "list"` only** — matches the codex CLI; drops `codex-auto-review` and
  `hide`/`none` entries.
- **Error-on-failure, no silent fallback.**
- **Placement:** fetch+parse in the `provider` package (testable with a fake `Authenticator` +
  httptest, matching the deepseek/volcengine provider-test pattern); version detection in `main`.

## Testing (white-box, stdlib + httptest, ≥80% coverage)

- **provider package** — `CodexProvider.FetchModels` against an httptest server with a fake
  `Authenticator`. Assert: request URL carries `?client_version=<X>`; headers `originator` / `Bearer` /
  `ChatGPT-Account-Id`; `visibility:"list"` slugs returned and `hide`/`none` excluded; non-200 → error.
- **main package** — `resolveCodexClientVersion` with injected fake sources: config-wins, then
  CLI → cache → constant fallthrough.

## Out of scope (YAGNI)

- ETag / `X-Models-Etag` conditional re-fetch (refresh is manual, on-demand).
- Auto-editing `config.yaml` routes with newly-discovered models (operator edits after seeing them).
- Updating `defaults.go`'s shipped `gpt-5.5` route (operator can add `gpt-5.6` once refresh shows it).
- `User-Agent` header — `originator` is the mandatory one; add UA only if a 403 appears in practice.

## Open implementation notes

- Confirm how `fetchCodexQuota`'s existing test (if any) mocks auth; the provider-package placement
  sidesteps this by using `cfg.Auth.Inject` with a fake `Authenticator`, so no new auth seam is needed.
- Update `CLAUDE.md` (codex bullet) and `AGENTS.md` (codex contract table + gotchas) with the
  reverse-engineered `/models` endpoint and the new optional `client_version` config field.
