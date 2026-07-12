# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository layout

The git repo root (`/Users/zhiyong.liu/model-proxy`) contains docs (`AGENTS.md`, `.gitignore`) and the Go module in the **`model-proxy/`** subdirectory. All `go` commands run from there:

```bash
cd model-proxy
go build -o model-proxy .                          # build (host)
go test ./...                                      # run all tests (~35s)
go test -run TestForward_ProviderRouting .         # run one test (white-box, package main)
go vet ./...                                       # lint
scripts/build.sh                                   # build host -> dist/ + ./model-proxy
scripts/build.sh linux/amd64                       # cross-compile one target -> dist/
scripts/build.sh --strip all                       # full matrix (linux/darwin/windows)
```

`scripts/build.sh` compiles one or more GOOS/GOARCH targets (pure-Go `modernc.org/sqlite` => every build is `CGO_ENABLED=0`, fully static, no cross-toolchain). Each target writes `dist/model-proxy-<goos>-<goarch>` (.exe on windows); the version is stamped from `git describe --tags --always --dirty` via `-ldflags -X main.version` (overriding the `dev` default in `version.go`, surfaced in `serve status` / `/api/status`). A successful **host** build additionally copies the binary to `./model-proxy` (runnable in place). Flags: `--version <v>`, `--out <dir>` (default `dist`), `--strip` (`-s -w`), `-v`. Both `dist/` and `./model-proxy` are gitignored.

Tests are white-box (`package main`) using only the stdlib `testing` + `httptest` - no testify. The `provider/` package has test files for deepseek + volcengine (auth + rewrite); other providers are tested through the proxy in `package main`.

## Architecture

`model-proxy` is a multi-provider LLM reverse proxy: clients speak Anthropic or OpenAI protocol; the proxy forwards using the **same protocol the client used (no conversion)**, swapping in real credentials and mapping model aliases to upstream model names.

**`AGENTS.md` (repo root) is the authoritative architecture/contract reference** - its "model-proxy 实现经验" section covers, in depth:
- Two-layer config (`providers` / `routes` / `claude_mapping`) and protocol routing (`/v1/messages` -> anthropic, `/v1/responses`+`/v1/chat/completions` -> openai; strip client `/v1`; per-protocol base URLs).
- Provider abstraction (`provider/provider.go`): the interface (`AuthHeaders`/`Refresh`/`RewriteRequest`/`Login`/`Logout`/`Usage`/`FetchModels`/`Quota`/`Surplus`/`ProbeRequest`/`ExtraHeaders`/`FilterModelIDs`), the `baseProbe` default-impl pattern, and per-provider overrides.
- Quota-aware scheduling (surplus formula, tier ranking `plan < unknown < payg`, peak via short window, sticky dwell), failover health (circuit breaker, rate-limit skip), multi-account credential pools + session-sticky routing.
- Request flow (`proxy.go:forward`), daemon/supervisor, takeover, Web UI + `/api/*` contract table, SSE token scanner, SQLite stats.
- Reverse-engineered backend contracts (aqp/compass, codex, Zhipu, DeepSeek, Volcengine incl. V4 signing), models.dev metadata, implicit routes, and a gotchas log.

**Read AGENTS.md before touching provider auth, request rewriting, scheduling, or the Web UI.** The notes below are CLAUDE.md-specific (conventions + change-controlled contracts) not duplicated in AGENTS.md.

## Credentials convention

Credentials are managed by `login`/`logout`, stored at `~/.model-proxy/<providerName>_<suffix>.json`, and **never put in config.yaml**. Suffix by provider: aqp/codex -> `oauth_auth`, zhipu/deepseek/volcengine -> `apikey`. The volcengine `apikey` file stores `{api_key, access_key, secret_key}` - the API Key for chat (Bearer) + the Volcengine AK/SK for `GetAFPUsage` (V4 signing). The provider *name* (config top-level key, not `provider_id`) derives the path, so multiple instances of the same `provider_id` (e.g. `zhipu-personal`, `zhipu-work`) get separate credential files.

**Multiple accounts per provider (credential pool):** repeated `login <provider>` adds accounts to a plural pool `~/.model-proxy/<name>_apikeys.json`. The legacy singular `<name>_apikey.json` is a read-only fallback (wrapped as a 1-entry pool). aqp/codex are single-credential (not pooled); only zhipu/deepseek/volcengine pool. See AGENTS.md "多账号凭据池" for the pool expansion + session-sticky routing.

## Adding a new provider

1. Create `provider/xxx.go` implementing `Provider` - embed `ApiKeyBase` (if file-stored API key) AND `baseProbe` (for default probe/filter/extra-headers behavior; use `fetchModelsBearer(p.cfg)` for `FetchModels` if it has an OpenAI-style `/models`). Override `ProbeRequest`/`ExtraHeaders`/`FilterModelIDs` only if the provider's probe path/body, per-request headers, or static model-id policy differ from the OpenAI default - this keeps provider-specific knowledge out of the main package (never in `if prov.Provider == ...` branches).
2. `Register("xxx", constructor)` in `init()`.
3. Add a `provider_id: xxx` entry under `providers:` in `config.yaml`, plus route entries.

A new **plan** provider should also implement `Quota()` - write a `fetchXxxQuota` parser in `main.go` returning `*provider.QuotaSnapshot` (Billing=`BillingPlan`; mark one window `Ultimate` + its `Duration`, optionally a `Short` rate-cap window for peak burn; set `RemainingPct` = the ultimate window's remaining), and wire it via `pcfg.QuotaFn` in `buildProviders`. A pay-as-you-go upstream instead sets `billing: pay-as-you-go` in config (no `Quota()` needed).

You should **not** need to edit `proxy.go`, `login.go`, `logout`, `usage`, or `models.go` - the routing/auth/CLI/model-list are provider-agnostic.

## Cross-cutting gotchas (CLAUDE.md-specific)

- **models.dev cache** (`modelsdev.go`): do NOT set `Accept-Encoding` manually in `realModelsDevFetch` - Go's `http.Transport` auto-requests gzip AND auto-decompresses; a manual header disables auto-decompress, so you'd get gzipped bytes. `If-None-Match`->`304` returns 0 bytes and only refreshes `fetched_at`. The cache stores the deduped slim projection (`by_name` + `by_endpoint`), never the 3 MB raw blob. `models:` in config is **names only**; metadata is always runtime-sourced. `MP_MODELSDEV_URL` overrides the endpoint (tests/mirrors).
- **Volcengine V4 signing** (`volcengine_sign.go`): the credential-scope terminator is **`request`** (not `volcengine_request`). Signed headers are `host;x-date` only - do NOT sign or send `x-content-sha256` for GET (it causes "Invalid Authorization"). Signing-key chain: `HMAC(sk->date->region->service->"request")`. `GetAFPUsage` needs AK/SK (not the Ark API Key); the Ark API Key is Bearer chat-only.
- **Config backup** (`web.go:writeConfigValidated`): each config write (models refresh + Web UI edits) backs up to a sibling `back/<base>.<YYYYMMDD-HHMMSS>.bak` (one per write, not overwriting). `saveAndReload`'s reload-failure rollback reads the returned backup path. `.gitignore` has `back/` + `*.bak`.
- **Web UI secrets**: `/api/accounts` cannot serialize a credential even under a programming mistake - the `acct` response struct has no `api_key`/`access_key`/`secret_key`/SSO-cookie field; account id is emitted UNMASKED (the UI needs the real id to delete). OpenAI token counts are best-effort (`usage` only present when the client sent `stream_options.include_usage`). `/api/status`'s `quota` field uses **PascalCase** keys (raw `provider.QuotaSnapshot` structs, no `json` tags) - don't expect snake_case.
- **Logging hygiene**: mask SSO cookies with `mask()` (first2…last2); log `auth/info` response bodies by length only. When logs go to a file, color must be off.

## CLI display contract (change-controlled)

`model-proxy/CLI.md` is the **stable contract** for every CLI subcommand's logic, stdout/stderr format + text, and exit codes. Scripts and users depend on it.

**Changing any CLI display contract (existing stdout/stderr format strings, text, column layout, or exit codes recorded in `CLI.md`) requires confirming with the user first.** Adding new columns/fields/lines is allowed (append-only, backward-compatible); do not alter existing lines' format, remove existing text, or change exit codes / which stream a line goes to, without sign-off. When a change is approved, update `CLI.md` and the corresponding `strings.Contains` test assertions in the same commit. New CLI subcommands must be added to `CLI.md` when introduced.

## Testing contract & conventions

Tests are white-box (`package main` / `package provider`), stdlib `testing` + `httptest` only - **no testify**. Run from `model-proxy/`: `go test ./...` (~35s), `go test -race ./...`, `go vet ./...`, `gofmt -l .` must all be clean.

**Coverage baseline: 80% per package.** Enforced by `scripts/cover.sh` (exits non-zero if any package drops below 80%). Run `scripts/cover.sh` before committing. The ~20% gap is intentional - daemon process management (`cmdServe`/`runSupervisor`/`daemonize`), SSO/OAuth browser flows (`runLogin`/`BootstrapLoginURL`/`PollSession`), interactive stdin login, and live upstream `FetchModels` are external I/O / process-level paths not unit-tested by convention.

**Test contracts (what a test MUST assert - green-signal bugs are the failure mode this codebase guards against):**

- **Auth headers - assert exact values, not "non-empty".** codex `Inject` must assert `Bearer <exact-token>` + `originator == "codex_cli_rs"` + `ChatGPT-Account-Id == "<acct>"`. deepseek/volcengine must assert `Bearer <key>` AND `x-api-key == <key>` on both protocol paths. Deleting any header-set line must turn the test red.
- **Quota window markers - assert Ultimate/Short/Duration/ResetsAt.** Every `parse*Quota` test must verify which window is `Ultimate` (total budget) and which is `Short` (rate-cap), plus `Duration` and `ResetsAt`. Getting these wrong silently breaks surplus/peak/pacing.
- **429 refresh - assert WHICH provider, not just count.** `refreshHook` must capture the provider name; assert it's the rate-limited one (a count-only check misses a wrong-provider bug).
- **No fake tests.** A test with `t.Logf` only, inverted `&&`/`||` logic, or `Contains(x) || Contains(y)` that a panic stack passes - is a defect. Assert precise values or structural fields. `strings.Contains` with `||` is a smell; prefer exact match or parsed-structure assertion.
- **Route side-effects - assert model rewrite + response status.** Forward tests must capture the upstream request's `model` JSON field (verifies `rewriteModel`) and the client-facing status/body, not just "hit the right upstream".
- **Stats buckets - assert SUM/MAX + bucket-start + lossless storage.** A `queryRange` aggregation test must insert known 1-minute rows, query at a wider `bucketSecs`, and assert the `SUM` of each counter, `MAX(last_request_at)`, and that the bucket `minute` is the window **start** (floor), then re-query at `bucket=60` to prove storage stayed 1-minute (lossless). Align the test base to a `bucketSecs` boundary (`/B*B`), not just `/60*60`.
- **Concurrency - race-clean AND a functional invariant.** `-race` clean is necessary but not sufficient; a reload-during-request test must also assert post-reload requests hit the new config.

`scripts/cover.sh [threshold] [--no-enforce]` writes `cov.out` + `coverage.html`, lists functions below `threshold` (default 60), and gates on the 80% baseline unless `--no-enforce`.
