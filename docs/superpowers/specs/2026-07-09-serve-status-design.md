# `serve status` — terminal status command

- **Date:** 2026-07-09
- **Status:** Approved (design)
- **Scope:** One new CLI subcommand; no server-side behavior changes

## Goal

The Web UI ships a "Status" tab (five cards: Providers, Schedule, Quota, Tokens,
Logs) that auto-refreshes every 5 s. Operators who live in the terminal have no
equivalent one-glance view — they must open the browser. Add `model-proxy serve
status`: a one-shot CLI command that fetches the **same data the UI does** and
renders it **optimized for the terminal** (columns, alignment, color, compact
numbers).

## Background / what already exists

The running daemon (foreground `serve` or `serve daemon`) serves three HTTP
endpoints the Status tab consumes — no new server data is needed:

| Endpoint | Handler | Provides |
|---|---|---|
| `GET /api/status` | `web.go:handleStatus` | `uptime`, `version`, `listen`, `health{}`, `quota{}`, `schedule{}`, `counters{}` |
| `GET /api/tokens` | `web.go:handleTokens` | per provider/model token usage |
| `GET /api/logs?tail=N` | `web.go:handleLogs` | last N log lines |

The closest existing analog is `model-proxy schedule` (`main.go:cmdSchedule`),
which does `LoadConfig → http.Get("http://"+cfg.Listen+"/debug/schedule")` and
renders a colorized table. `serve status` follows that exact pattern.

### JSON shapes consumed (confirmed against source)

- `/api/status` top-level keys (lowercase snake): `uptime`, `version`, `listen`,
  `health`, `quota`, `schedule`, `counters`.
- `health[name]`: `circuit_state` (`closed`/`open`/`half_open`), `available`, and
  optional `circuit_until` / `rate_limited_until` (RFC3339). No
  `consecutiveFailures` / `retry_after`.
- `counters[name]`: `requests`, `failovers`, `rate_limited_429`, `failures`,
  `last_request_at` (unix seconds).
- `schedule.models[route]`: `first`, `ordered[]` (`provider`, `pool_parent`,
  `priority`, `tier`, `surplus`, `available`, `peak`), `sticky`,
  `sticky_dwell_remaining_sec`, `pools[]` (`parent`, `accounts`, `available`).
- `quota[name]`: **PascalCase** `*provider.QuotaSnapshot` — `Billing` (int),
  `RemainingPct`, `Account`, `Plan`, `Level`, `Windows[]`, `Notes[]`, `AsOf`,
  `Err`. Each window: `Label`, `Kind`, `Used`, `Total`, `RemainingPct`,
  `ResetsAt`, `Details[]`, `Ultimate`, `Short`, `Duration`.
- `/api/tokens` `usage[]`: `provider`, `model`, `input`, `output`,
  `cache_creation`, `cache_read`, `requests`.

## Locked decisions (from brainstorming)

1. **One-shot only.** Print and exit; no `--watch`/auto-refresh. (Re-run to
   refresh.)
2. **Logs hidden by default.** `--logs [N]` opts in (default `N=20` when the
   flag is present with no value).
3. **`--json` added.** Dumps raw merged JSON for scripting.

## Approach chosen (A): reuse the existing `/api/*` endpoints

`serve status` performs up to three HTTP GETs against the running daemon and
renders. **No changes to `proxy.go` or `web.go`.** Rationale: the data already
exists and is what the UI consumes; lowest risk; matches `cmdSchedule`'s reuse
of `/debug/schedule`.

Rejected alternative (B): add an always-on `/debug/status` endpoint by extracting
the builder from `handleStatus`. Removes the `web.enabled` dependency but adds a
server endpoint + test burden. Not worth it — `web.enabled` defaults to `true`,
and the command's premise is "same content as the WebUI".

**`web.enabled` dependency:** if `/api/status` returns 404 (Web UI disabled),
the command prints a clear, actionable error rather than failing opaquely.

## Command interface

Wired as `case "status": cmdServeStatus(args)` in the `cmdServe` switch
(`daemon.go`, before the foreground `default`).

```
model-proxy serve status [--logs [N]] [--json] [--config <path>]
```

- `--config <path>` — resolved by existing `configPath(args)` → selects the
  `listen:` address to query (default `127.0.0.1:15721`).
- `--logs [N]` — append a Logs section with the last `N` lines (default 20).
  Without the flag, logs are not fetched or shown.
- `--json` — skip rendering; print merged raw JSON and exit.

Help/usage updates: add a `serve status` line to the top-level `usage` const
(`main.go`) and a block to `cmdHelp["serve"]`.

## Terminal layout

Color via existing `cBold`/`cGreen`/`cRed`/`cYellow`/`cDim`/`cGray`; alignment
via `pad` (`models.go:255`); quota bars via `progressBar` (`main.go:1589`).

```
model-proxy  v0.4.2 · 2h15m3s · 127.0.0.1:15721

Providers (4)
  PROVIDER        HEALTH          REQS    FAILOVERS   429   FAILURES   LAST
  aqp             available       1.2k           12     3          5   14:32:01
  codex#acct2     circuit open      567           45     8         20   14:30:55
  zhipu           rate-limited      210            0     1          0   14:31:40
  deepseek        available           0            0     0          0   —

Schedule (6 routes)
  claude-sonnet-4.5 → aqp
      aqp        plan          surplus +12.30  p1
      codex      plan          surplus  +8.10  p1   (unavailable)  peak
    sticky: aqp · 320s dwell left
  gpt-5.5 → codex
      ...

Quota (3)
  aqp · work-acct · plan
      Monthly (ultimate)  62%  ████████░░░░░░░░  resets 09:00
      5h tokens (short)   88%  ████████████░░░  resets 14:30
  codex · personal · plan
      no data (rate limited)

Tokens (5 models · 1,234 requests)
  PROVIDER  MODEL             INPUT   OUTPUT  CACHE-CR  CACHE-RD  REQUESTS
  aqp       claude-sonnet-4.5 1.2M    450K    200K      1.1M      1.2k
  ...

Logs (last 20)                                       ← only with --logs
  14:32:01 200 claude-sonnet-4.5 → aqp  ...
  ...
```

### Section rules

- **Header** (always): `model-proxy  v<version> · <uptime> · <listen>` — the
  `uptime`/`version`/`listen` strings taken verbatim from `/api/status`;
  version+meta dimmed.
- **Providers** (hidden if `health` empty): rows = sorted `health` keys;
  `counters[name]` looked up per row (matches the UI). Health cell:
  - `circuit_state` `open`/`half_open` → red `circuit open` (or `half-open`);
  - future `rate_limited_until` → yellow `rate-limited`;
  - `available` → green `available`;
  - else dim `unavailable`.
  Counters compact (`compactNum`: `123`/`1.2k`/`3.4M`/`5.6B`); `last` = local
  `HH:MM:SS` from `last_request_at`, `—` if zero.
- **Schedule** (hidden if `schedule.models` empty; else `(no routes)`): same
  shape as the standalone `schedule` command — route → bold first-choice, indented
  ordered list (`provider`, tier, `surplus ±X.XX`, `pN`, `(unavailable)`/`peak`
  tags), `sticky: <p> · <N>s dwell left`, and `pool <parent>: a/n available`.
- **Quota** (hidden if `quota` empty): per-provider header `name · account ·
  plan`; each window = `Label (ultimate|short)  NN%  <bar>  resets HH:MM`;
  `progressBar` colored by remaining ratio. `snap.Err` → `no data (<err>)`.
- **Tokens** (hidden if `usage` empty): table sorted by provider then model;
  header shows model count + total requests. Counts via `compactNum`.
- **Logs** (`--logs` only): indented raw lines from `/api/logs?tail=N`, header
  `Logs (last N)`.

### `--json`

Fetches `status` + `tokens` (+ `logs` only if `--logs`), keeps each response as
`json.RawMessage`, and prints one pretty object:

```json
{ "status": {…}, "tokens": {…} }
```

(`"logs": […]` present only with `--logs`.) Honest "raw" passthrough — no
reshaping — for `jq`/scripts.

## Error handling (mirrors `cmdSchedule`)

- Transport error → `✗ cannot reach daemon at <listen>: <err>` + hint
  `is 'model-proxy serve' running?`, exit 1.
- `/api/status` → 404 → `✗ web UI endpoints not available — is web.enabled true
  on the daemon?`, exit 1.
- Other non-200 → `✗ daemon returned HTTP <code>: <truncated body>`, exit 1.
- Parse failure → `✗ parse status response: <err>`, exit 1.

## Fetch plan

- `/api/status` — always.
- `/api/tokens` — always (drives the Tokens section).
- `/api/logs?tail=N` — only when `--logs` (or never, otherwise).

## Testability / test plan (white-box, stdlib `testing` + `httptest` only)

Split into a thin CLI wrapper and a pure core:

- `cmdServeStatus(args)` — parse flags (`--logs`/`--json`/`--config`), resolve
  `cfg.Listen`, call the core, then print + `os.Exit`. (The wrapper holds the
  exit-code logic so the core is testable.)
- `renderStatus(baseURL string, opts statusOpts) (string, error)` — performs the
  GETs against `baseURL` (e.g. `http://127.0.0.1:PORT`) and returns the rendered
  string. `statusOpts{ Logs bool; LogsN int; JSON bool }`.

Tests (`serve_status_test.go`, `package main`) drive `renderStatus` with an
`httptest.Server` returning canned JSON for the three paths and assert:

- exact rendered substrings per section (a known provider name appears with its
  health pill text; a known route's `first` choice; a quota window's label +
  `(ultimate)` + bar; a token row) — green-signal assertions, not "non-empty";
- `--json` output is valid JSON and round-trips the injected payloads;
- `--logs` includes a log line, default omits it;
- transport error (bad `baseURL`) → the expected `✗ cannot reach daemon …`
  message;
- `/api/status` 404 → the `web.enabled` message;
- non-empty / empty-section edge cases (`(no routes)`, missing quota).

Coverage target: keep `main` ≥ 80% (enforced by `scripts/cover.sh`).

## Files touched

- **New** `serve_status.go` — `cmdServeStatus`, `renderStatus`, per-section
  renderers, `compactNum`, a local `HH:MM:SS` clock helper.
- `daemon.go` — one `case "status":` line in `cmdServe`.
- `main.go` — `usage` const + `cmdHelp["serve"]` text.
- **New** `serve_status_test.go` — white-box tests.
- `README.md` — list `serve status` under serve commands.

## Out of scope (YAGNI)

- No `--watch`/auto-refresh.
- No per-section filter flags (`--providers`/`--quota`/…).
- No new server endpoint; no changes to `/api/*` shapes.
- No interactivity (no token-reset, no config edit) — `status` is read-only.
