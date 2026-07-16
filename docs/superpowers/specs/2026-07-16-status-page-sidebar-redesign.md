# Status Page Sidebar Redesign

Date: 2026-07-16
Status: Design approved (pre-implementation)

## Goal

Restructure the Web UI **Status** tab from a vertical stack of cards into a
sidebar + detail-pane layout (like the Accounts tab), and fix three UX problems
in the Logs card.

## Non-goals

- No backend changes — `web.go`, the `/api/*` handlers, and their JSON contracts
  are all unchanged. This is a pure frontend change (`web_assets/app.js` +
  `web_assets/styles.css`).
- No change to the Config or Accounts tabs.
- No new API endpoints. Logs ordering stays server-side (oldest→newest); the
  scroll behavior is handled client-side.
- CLI display contract untouched (`model-proxy/CLI.md`).

## Current state

`renderStatusTab` (`web_assets/app.js`) re-fetches `/api/status` + `/api/tokens`
+ `/api/logs?tail=200` every 5s and rebuilds the entire `#tab-status` panel by
appending six cards in order: Providers, Schedule, Warnings, Quota, Token usage,
Logs. Each refresh wipes and re-creates all DOM, which resets the Logs scroll
position every tick.

The Accounts tab is the reference pattern: a `.accounts-layout` grid
(200px sticky `.acct-nav` sidebar + `.acct-main` pane), a `accountsSelectedProvider`
state preserved across re-renders, `selectProvider` re-rendering only the right
pane, and hash routing `#accounts/<provider>`.

## Design

### 1. Layout & navigation

Replace the vertical card stack with an Accounts-style two-column layout. The
sidebar lists the Status sub-sections; only the active section renders in the
detail pane.

```
[ Warnings banner (only when st.warnings non-empty) ]
[ status-layout grid ]
  ├─ .status-nav (200px, sticky)   →  Schedule · Providers · Quota · Token Usage · Logs
  └─ .status-main                   →  active section's card(s)
```

- Sidebar items, in order: **Schedule, Providers, Quota, Token Usage, Logs**.
  First item (Schedule) is the default selection.
- `statusSelected` state (default `'schedule'`), preserved across the 5s
  re-render ticks and across section switches so the view never snaps back.
- `selectStatusSection(name)` highlights the nav item and renders **only**
  `.status-main`. The sidebar stays wired (same pattern as `selectProvider`).
- **Warnings** stays as a banner rendered above `.status-layout` when
  `st.warnings` is non-empty, hidden otherwise. It re-renders each tick (cheap,
  above the layout — doesn't disrupt the pane).
- **Reuse the Accounts CSS classes** (`.accounts-layout`, `.acct-nav`,
  `.acct-nav-title`, `.acct-nav-item`, `.badge`, responsive stacking) rather
  than adding parallel `.status-*` classes — the sidebar is structurally
  identical. The detail pane uses a `.status-main` class (parallel to
  `.acct-main`) for clarity, styled the same as `.acct-main`.

### 2. Hash routing

Extend the existing hash router (`parseHash` / `setHash`) to support
`#status/<section>`:
- `#status` alone, or `#status/<section>` with an unrecognized section → default
  to `schedule`.
- A section click pushes `#status/<section>` (a navigation the user may Back out
  of, matching how tab switches push).
- `hashchange` (browser back/forward) re-selects the section without pushing,
  avoiding a feedback loop — mirrors the Accounts provider handling.

### 3. Data fetching & refresh

Keep the single batched fetch (`/api/status` + `/api/tokens` +
`/api/logs?tail=200`) on a 5s `statusTimer`, as today. On each tick:
1. Cache the three responses (a `statusCache` object holding `st`, `tok`,
   `logs`).
2. Re-render the Warnings banner.
3. Re-render **only the active section's** pane from the cache (not the whole
   panel, not all sections).

When the user switches sections, render that section from the cached data
immediately — no extra fetch, no fetch stall.

Each `render*Section` function takes the cached data and writes into
`.status-main`. The existing card-builder functions (`renderScheduleCard`,
`renderProvidersCard`, `renderQuotaCard`, `renderTokensCard`, `renderLogsCard`)
are refactored to return their HTML / render into a given target rather than
appending to `panels.status`.

### 4. Logs card — the three behavior changes (`renderLogsCard`)

This is the core UX fix. The Logs card keeps its server order (oldest→newest,
newest appended at the bottom) and changes only rendering + scroll behavior.

**a. No blank line between entries.** Today lines are `<span class="log-line">`
(`display:block`) joined by `'\n'` inside a `pre-wrap` `<pre>`; the `'\n'` plus
block layout produces a blank gap. Fix: join with no separator (`''`). The
`display:block` spans already put each entry on its own line, so entries are
tight, one per line.

**b. Order unchanged.** Server returns oldest→newest; render in that order, so
the newest line is at the bottom. No client-side reversal, no backend change.

**c. Scroll: auto-snap to bottom by default; freeze when the user scrolls up.**
Keep an in-memory reference to the `.log-pre` element and the previous `lines`
snapshot. On each 5s tick that touches the Logs section:

1. If `lines` is unchanged from the previous snapshot (deep compare — length +
   joined string) → skip the Logs DOM re-render entirely (no-op tick: no
   flicker, no scroll disruption).
2. Otherwise, before re-rendering capture the distance from the bottom:
   `bottomDist = scrollHeight - scrollTop - clientHeight`.
3. Re-render the pane.
4. If `bottomDist <= SNAP_THRESHOLD` (e.g. 4px — the user was at, or effectively
   at, the bottom) → set `scrollTop = scrollHeight` so the newest line stays
   visible (auto-snap to bottom). This is the default for initial render and
   whenever the user is parked at the bottom.
5. Else (the user scrolled up away from the bottom) → restore the previous
   `scrollTop` value. The newly appended lines land below the fold and the
   visible content stays put ("保持显示区的内容不刷新"). Auto-snap resumes only
   when the user scrolls back to the bottom.

Because only the active section re-renders each tick and the Logs DOM is
untouched on a no-op tick, scrolling up freezes the view reliably.

### 5. CSS

- No new sidebar classes — reuse `.accounts-layout`, `.acct-nav`,
  `.acct-nav-title`, `.acct-nav-item`, `.badge`, and the existing
  `@media (max-width: 720px)` responsive stacking. The sidebar title reads
  "Sections" (or similar) instead of "Providers".
- Add `.status-main { min-width: 0; }` mirroring `.acct-main`. The sidebar
  title reads "Sections" (concrete, not "Providers").
- Logs: `.log-pre` is unchanged; the blank-line fix is in the JS join, not CSS.

### 6. Testing & verification

The web SPA has no JS test harness — `web_test.go` covers the `/api/*` handlers
(white-box Go), not the frontend. No API contract changes here, so no
`web_test.go` changes are required. Verification:

- `go test ./...`, `go vet ./...`, `gofmt -l .` must stay clean (unchanged Go
  files, but the gates run on the whole module).
- `scripts/build.sh` rebuilds the binary; the Web UI is served from
  `web_assets/` (embedded), so a rebuild + manual browser check covers:
  - Sidebar nav switches sections; only the active pane re-renders.
  - `#status/<section>` round-trips on refresh and browser back/forward.
  - Warnings banner appears only when warnings exist.
  - Logs: no blank lines between entries; newest at bottom; auto-snap to bottom
    on load and while parked at the bottom; scrolling up freezes the visible
    content across 5s refreshes; scrolling back to the bottom resumes snapping.

## Files touched

- `model-proxy/web_assets/app.js` — Status tab restructure (layout, sidebar,
  hash routing, per-section render, logs scroll logic).
- `model-proxy/web_assets/styles.css` — `.status-main` rule (sidebar classes
  reused).

## Risks / notes

- Reusing the Accounts CSS classes means a future Accounts-only style change
  affects Status too. Acceptable: the sidebar is intentionally the same
  component, and shared styling is the goal.
- The logs scroll-snap threshold (4px) is a heuristic; if it feels sticky on
  some platforms it can be tuned. Chosen small enough that "at the bottom"
  reads as at-the-bottom despite sub-pixel rounding.
