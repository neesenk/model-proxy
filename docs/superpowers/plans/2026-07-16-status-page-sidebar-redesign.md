# Status Page Sidebar Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restructure the Web UI Status tab into a sidebar + detail-pane layout (mirroring the Accounts tab) and fix three Logs-card UX problems (no blank lines, newest-at-bottom, scroll-snap-to-bottom with freeze-on-scroll-up).

**Architecture:** Pure frontend change to `web_assets/app.js` + `web_assets/styles.css`. A single 5s batched fetch (unchanged) feeds a `statusCache`; only the active section re-renders each tick. The sidebar reuses the Accounts CSS classes (`.accounts-layout`/`.acct-nav`/`.acct-nav-item`). Hash routing extends to `#status/<section>`. Logs gains an in-memory snapshot + scroll-distance check so no-op ticks skip the DOM write and user-scrolled views stay frozen.

**Tech Stack:** Vanilla JS (ES module), no framework/CDN. Go embeds `web_assets/` (no build step). No JS test harness — verification is manual browser checks + the Go gates (`go test ./...`, `go vet ./...`, `gofmt -l .`).

## Global Constraints

- No backend changes: `web.go`, `/api/*` handlers, and their JSON contracts are unchanged. Logs ordering stays server-side (oldest→newest); no client-side reversal.
- Reuse the Accounts CSS classes for the sidebar (`.accounts-layout`, `.acct-nav`, `.acct-nav-title`, `.acct-nav-item`, `.badge`) — do not add parallel `.status-nav*` classes. Add only `.status-main`.
- Every value interpolated into `innerHTML` goes through `esc()` (existing convention, `app.js:18`).
- CLI display contract (`model-proxy/CLI.md`) untouched.
- Go gates must stay clean: `go test ./...`, `go vet ./...`, `gofmt -l .` (run from `model-proxy/`).

**Spec:** `docs/superpowers/specs/2026-07-16-status-page-sidebar-redesign.md`

---

## File Structure

- **Modify** `model-proxy/web_assets/app.js` — Status tab restructure: sidebar layout, `statusSelected` state, `selectStatusSection`, hash routing `#status/<section>`, per-section renderers fed from a `statusCache`, logs scroll logic.
- **Modify** `model-proxy/web_assets/styles.css` — add `.status-main { min-width: 0; }`. Sidebar classes are reused from Accounts.

No new files. No Go files change.

---

## Task 1: Logs card — remove blank line between entries + reverse-to-bottom unchanged

This task is folded first because it's the most self-contained behavior change and the current `renderLogsCard` is the reference. We refactor it to render into a given target and join entries with no separator. Order stays oldest→newest (server order). Scroll logic comes in Task 3; here we only fix the blank-line gap and decouple the renderer from `panels.status`.

**Files:**
- Modify: `model-proxy/web_assets/app.js` (`renderLogsCard`, ~`app.js:582-592`)

**Interfaces:**
- Produces: `renderLogsCard(target, lines)` — renders the Logs card HTML into `target` (a DOM element). `target` may be `panels.status` (fallback) or `.status-main`.
- Consumes: `buildCard(title, meta, bodyHTML, extraBodyClass)` (`app.js:353`), `esc()` (`app.js:18`).

- [ ] **Step 1: Refactor `renderLogsCard` to take a target and drop the `'\n'` join**

Replace the existing `renderLogsCard` function with:

```javascript
// renderLogsCard renders the tail of the daemon log into `target`. Entries
// are joined with no separator: each .log-line is display:block (styles.css),
// so they stack one-per-line without a blank gap (the old '\n' join inside a
// pre-wrap <pre> produced a blank line between entries). Order is preserved
// server-side (oldest first, newest at the bottom) — no client-side reversal.
function renderLogsCard(target, lines) {
  let body;
  if (!lines || lines.length === 0) {
    body = `<pre class="log-pre"><span class="log-empty">log is empty or unavailable</span></pre>`;
  } else {
    const rendered = lines.map((l) => `<span class="log-line">${esc(l)}</span>`).join('');
    body = `<pre class="log-pre">${rendered}</pre>`;
  }
  const html = buildCard('Logs', lines ? `${lines.length} lines` : '', body, 'flush');
  target.insertAdjacentHTML('beforeend', html);
}
```

- [ ] **Step 2: Verify the file parses (no syntax error)**

Run: `node --check model-proxy/web_assets/app.js`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add model-proxy/web_assets/app.js
git commit -m "refactor(web): renderLogsCard takes target, no blank line between log entries"
```

---

## Task 2: Per-section renderers fed from a target (decouple from `panels.status`)

Refactor the other four card builders (`renderProvidersCard`, `renderScheduleCard`, `renderQuotaCard`, `renderTokensCard`) so each renders into a passed `target` instead of `panels.status` directly. `renderWarningsCard` keeps writing above the layout (Task 4 handles its placement). This is the mechanical decoupling that lets Task 4 render a single active section into `.status-main`.

**Files:**
- Modify: `model-proxy/web_assets/app.js` (`renderProvidersCard` ~`:387`, `renderScheduleCard` ~`:421`, `renderQuotaCard` ~`:470`, `renderTokensCard` ~`:527`, `renderWarningsCard` ~`:362`)

**Interfaces:**
- Produces: `renderProvidersCard(target, st)`, `renderScheduleCard(target, st)`, `renderQuotaCard(target, st)`, `renderTokensCard(target, usage)`, `renderWarningsCard(target, st)`. Each appends its card HTML to `target` via `insertAdjacentHTML('beforeend', ...)`.
- The `resetTokens` button wiring inside `renderTokensCard` stays as-is (`document.getElementById('btn-tokens-reset')`), since the rendered card is in the DOM when wiring runs.

- [ ] **Step 1: Add `target` param to `renderProvidersCard` and use it**

Change the signature line and the final insert. Replace:

```javascript
function renderProvidersCard(st) {
```

with:

```javascript
function renderProvidersCard(target, st) {
```

and replace the closing insert:

```javascript
  panels.status.insertAdjacentHTML('beforeend', html);
```

with:

```javascript
  target.insertAdjacentHTML('beforeend', html);
```

(The single `return;` early-out at the top when `names.length === 0` stays.)

- [ ] **Step 2: Same for `renderScheduleCard`**

Signature `function renderScheduleCard(st)` → `function renderScheduleCard(target, st)`; final `panels.status.insertAdjacentHTML('beforeend', html);` → `target.insertAdjacentHTML('beforeend', html);`.

- [ ] **Step 3: Same for `renderQuotaCard`**

Signature `function renderQuotaCard(st)` → `function renderQuotaCard(target, st)`; final `panels.status.insertAdjacentHTML('beforeend', html);` → `target.insertAdjacentHTML('beforeend', html);`.

- [ ] **Step 4: Same for `renderTokensCard`**

Signature `function renderTokensCard(usage)` → `function renderTokensCard(target, usage)`; the two `panels.status.insertAdjacentHTML('beforeend', html);` calls (empty-state branch + main branch) → `target.insertAdjacentHTML('beforeend', html);`. The `btn` wiring after is unchanged.

- [ ] **Step 5: Same for `renderWarningsCard`**

Signature `function renderWarningsCard(st)` → `function renderWarningsCard(target, st)`; `panels.status.insertAdjacentHTML('beforeend', buildCard(...))` → `target.insertAdjacentHTML('beforeend', buildCard(...))`.

- [ ] **Step 6: Update the existing `renderStatusTab` call sites so the app still works before Task 4 restructures it**

In `renderStatusTab` (~`app.js:331-337`), the calls `renderProvidersCard(st)` etc. must pass `panels.status`. Replace the block:

```javascript
    panels.status.innerHTML = '';
    renderProvidersCard(st);
    renderScheduleCard(st);
    renderWarningsCard(st);
    renderQuotaCard(st);
    renderTokensCard(tok.usage || []);
    renderLogsCard(logs.lines || []);
```

with (temporary — Task 4 replaces this whole body):

```javascript
    panels.status.innerHTML = '';
    renderProvidersCard(panels.status, st);
    renderScheduleCard(panels.status, st);
    renderWarningsCard(panels.status, st);
    renderQuotaCard(panels.status, st);
    renderTokensCard(panels.status, tok.usage || []);
    renderLogsCard(panels.status, logs.lines || []);
```

- [ ] **Step 7: Verify parse**

Run: `node --check model-proxy/web_assets/app.js`
Expected: exit 0, no output.

- [ ] **Step 8: Commit**

```bash
git add model-proxy/web_assets/app.js
git commit -m "refactor(web): status card builders render into a passed target"
```

---

## Task 3: Logs scroll-snap-to-bottom + freeze-on-scroll-up + skip no-op ticks

Add the logs scroll behavior. An in-memory module-level reference holds the `.log-pre` element and the previous `lines` snapshot. On each Logs render: skip if unchanged; else capture bottom-distance, re-render, snap to bottom if near-bottom, else restore `scrollTop`.

**Files:**
- Modify: `model-proxy/web_assets/app.js` (add module state near the STATUS TAB section ~`:296`; add `renderLogsInto(target, lines)` and call it from the section dispatcher in Task 4).

**Interfaces:**
- Produces: `renderLogsInto(target, lines)` — the Logs-section renderer used by the Status sidebar dispatcher. Owns the no-op skip + scroll-snap/restore logic. `renderLogsCard` (Task 1) remains the inner HTML builder.
- Module state: `let logsPre = null;` (last `.log-pre` element), `let logsPrevKey = '';` (previous lines joined, for change detection), `let logsPrevScrollTop = 0;`.

- [ ] **Step 1: Add module state + a `SNAP_THRESHOLD` constant after the `statusInflight` declarations**

Near `app.js:296-297` (after `let statusInflight = false;`), add:

```javascript
// ---------- logs scroll state ----------
//
// The Logs card auto-snaps to the bottom (newest line visible) by default, but
// if the user has scrolled up we freeze the visible content: new lines append
// below the fold without moving what's on screen. `logsPre` is the last .log-pre
// element we rendered; `logsPrevKey` is the joined previous lines so a no-op
// refresh (no new lines) skips the DOM write entirely (no flicker, no scroll
// disruption). `logsPrevScrollTop` is restored when the user was scrolled up.
let logsPre = null;
let logsPrevKey = '';
let logsPrevScrollTop = 0;
const LOG_SNAP_THRESHOLD = 4; // px — "at the bottom" within sub-pixel rounding
```

- [ ] **Step 2: Add `renderLogsInto` — the smart Logs renderer**

Add this function (place it right after `renderLogsCard` from Task 1):

```javascript
// renderLogsInto renders the Logs card into `target` with scroll-preserving
// behavior. Called by the Status section dispatcher (Task 4) on each 5s tick.
//
// - No-op skip: if `lines` is unchanged since the last render (joined-string
//   compare), do nothing — avoids flicker and leaves scrollTop untouched.
// - Snap to bottom: if the user was at (or within LOG_SNAP_THRESHOLD of) the
//   bottom, re-render then set scrollTop = scrollHeight so the newest line
//   stays visible. This is the default on first render and while parked at
//   the bottom.
// - Freeze on scroll-up: if the user had scrolled up away from the bottom,
//   capture scrollTop before re-render and restore it after — newly appended
//   lines land below the fold and the visible content stays put. Snapping
//   resumes only when the user scrolls back to the bottom.
function renderLogsInto(target, lines) {
  const arr = lines || [];
  const key = arr.join('\n');
  if (logsPre && key === logsPrevKey && document.body.contains(logsPre)) {
    // unchanged — leave the DOM and scroll position exactly as they are
    return;
  }
  // Capture bottom-distance on the existing <pre> before we wipe it.
  let bottomDist = 0;
  let wasAtBottom = true;
  if (logsPre && document.body.contains(logsPre)) {
    bottomDist = logsPre.scrollHeight - logsPre.scrollTop - logsPre.clientHeight;
    wasAtBottom = bottomDist <= LOG_SNAP_THRESHOLD;
    if (!wasAtBottom) logsPrevScrollTop = logsPre.scrollTop;
  }
  target.innerHTML = '';
  renderLogsCard(target, arr);
  logsPrevKey = key;
  logsPre = target.querySelector('.log-pre');
  if (!logsPre) return;
  if (wasAtBottom) {
    logsPre.scrollTop = logsPre.scrollHeight;
  } else {
    logsPre.scrollTop = logsPrevScrollTop;
  }
}
```

- [ ] **Step 3: Verify parse**

Run: `node --check model-proxy/web_assets/app.js`
Expected: exit 0, no output.

- [ ] **Step 4: Commit (function is unused until Task 4 wires it; that's fine)**

```bash
git add model-proxy/web_assets/app.js
git commit -m "feat(web): logs scroll-snap-to-bottom with freeze-on-scroll-up"
```

---

## Task 4: Status sidebar layout + section state + hash routing + per-section render

Replace the `renderStatusTab` body with the sidebar layout. Add `statusSelected` state, `selectStatusSection`, extend `parseHash`/hash routing to `#status/<section>`, and cache fetched data in `statusCache` so section switches render instantly. Wire the 5s timer to re-render only the active section + warnings banner.

**Files:**
- Modify: `model-proxy/web_assets/app.js` (`renderStatusTab` ~`:321-350`, `parseHash` ~`:186-197`, `activateTab`/`activateTabSilent` ~`:222-279`, boot block ~`:1683-1698`)
- Modify: `model-proxy/web_assets/styles.css` (add `.status-main`)

**Interfaces:**
- Produces: `statusCache` (`{st, tok, logs}`), `statusSelected` (string section key), `STATUS_SECTIONS` (ordered list of `{key,label}`), `selectStatusSection(name)`, `selectStatusSectionSilent(name)`, `renderStatusSection(key)`.
- Consumes: the per-section renderers from Tasks 1-3 (`renderScheduleCard`, `renderProvidersCard`, `renderQuotaCard`, `renderTokensCard`, `renderWarningsCard`, `renderLogsInto`).

- [ ] **Step 1: Add `.status-main` to styles.css**

In `model-proxy/web_assets/styles.css`, right after the `.acct-main { min-width: 0; }` line (~`:542`), add:

```css
/* status detail pane (mirrors .acct-main; the sidebar reuses .acct-nav) */
.status-main { min-width: 0; }
```

- [ ] **Step 2: Define `STATUS_SECTIONS` and `statusCache` / `statusSelected` state**

Near the top of the STATUS TAB section (after the `statusInflight` / logs-state declarations from Task 3), add:

```javascript
// Status sub-sections, in sidebar order. `key` is the hash segment + the
// statusSelected value; `label` is the nav button text. First (schedule) is the
// default selection.
const STATUS_SECTIONS = [
  { key: 'schedule', label: 'Schedule' },
  { key: 'providers', label: 'Providers' },
  { key: 'quota', label: 'Quota' },
  { key: 'tokens', label: 'Token Usage' },
  { key: 'logs', label: 'Logs' },
];
let statusCache = { st: null, tok: [], logs: [] };
let statusSelected = 'schedule';
```

- [ ] **Step 3: Add `renderStatusSection(key)` — renders one section into `.status-main`**

```javascript
// renderStatusSection renders the named section's card(s) into .status-main
// from statusCache. Only the active section re-renders each 5s tick, so the
// Logs scroll position (and any open <details>) in other sections is never
// disturbed by a refresh of the visible section.
function renderStatusSection(key) {
  const main = document.querySelector('.status-main');
  if (!main) return;
  main.innerHTML = '';
  const st = statusCache.st;
  switch (key) {
    case 'schedule':
      if (st) renderScheduleCard(main, st);
      break;
    case 'providers':
      if (st) renderProvidersCard(main, st);
      break;
    case 'quota':
      if (st) renderQuotaCard(main, st);
      break;
    case 'tokens':
      renderTokensCard(main, statusCache.tok || []);
      break;
    case 'logs':
      renderLogsInto(main, statusCache.logs || []);
      break;
  }
}
```

- [ ] **Step 4: Add `selectStatusSection` / `selectStatusSectionSilent`**

```javascript
// selectStatusSection highlights the sidebar item, renders the section, and
// pins it in the URL hash (#status/<section>). push=true adds a history entry
// (a section click the user may Back out of); false replaces (used by the
// hashchange listener + boot, where the hash already reflects the target).
function selectStatusSection(name, push = true) {
  if (!STATUS_SECTIONS.some((s) => s.key === name)) name = 'schedule';
  selectStatusSectionSilent(name);
  if (push) setHash('#status/' + name, true);
}
function selectStatusSectionSilent(name) {
  if (!STATUS_SECTIONS.some((s) => s.key === name)) name = 'schedule';
  statusSelected = name;
  document.querySelectorAll('.status-nav-item').forEach((b) => {
    b.classList.toggle('active', b.dataset.section === name);
  });
  renderStatusSection(name);
}
```

- [ ] **Step 5: Replace the `renderStatusTab` body with the sidebar layout**

Replace the entire `renderStatusTab` function (~`app.js:321-350`) with:

```javascript
// renderStatusTab fetches the dashboard snapshot (status + tokens + logs),
// caches it, and renders the Status panel: a Warnings banner (only when
// warnings exist) + an Accounts-style sidebar+detail layout. Only the active
// section's pane re-renders on each 5s tick; section switches render from the
// cache with no extra fetch. Auto-refreshes every 5s while the Status tab is
// active; the timer is cleared when the user leaves the tab.
async function renderStatusTab() {
  if (statusInflight) return;
  statusInflight = true;
  try {
    const [st, tok, logs] = await Promise.all([
      apiGet('/api/status'),
      apiGet('/api/tokens').catch(() => ({ usage: [] })),
      apiGet('/api/logs?tail=200').catch(() => ({ lines: [] })),
    ]);
    setConn('ok', `v${st.version || '?'} · ${st.uptime || '—'} · ${st.listen || ''}`);
    statusCache = { st, tok: tok.usage || [], logs: logs.lines || [] };
    renderStatusPanel();
  } catch (e) {
    setConn('err', 'connection lost');
    if (panels.status) {
      panels.status.innerHTML =
        `<div class="card"><div class="card-body"><div class="msg err">${esc(e.message)}</div></div></div>`;
    }
  } finally {
    statusInflight = false;
  }
  if (activeTab === 'status' && !statusTimer) {
    statusTimer = setInterval(() => { renderStatusTab(); }, 5000);
  }
}

// renderStatusPanel draws the Warnings banner (if any) + the sidebar+detail
// layout, then renders the active section. Called after each fetch. The
// sidebar is rebuilt only when it isn't already present (so a 5s tick that
// finds the layout in place just re-renders the active section + warnings,
// preserving any scroll position in the pane).
function renderStatusPanel() {
  const panel = panels.status;
  if (!panel || !statusCache.st) return;
  const st = statusCache.st;

  // Warnings banner: render when present, clear when absent. It lives above the
  // layout so it doesn't disrupt the detail pane.
  let banner = panel.querySelector('.status-warnings');
  const ws = st.warnings || [];
  if (ws.length) {
    const items = ws.map((w) => `<div class="msg warn">⚠ ${esc(w)}</div>`).join('');
    const html = `<div class="status-warnings" style="margin-bottom:12px;">${items}</div>`;
    if (banner) banner.outerHTML = html;
    else panel.insertAdjacentHTML('afterbegin', html);
  } else if (banner) {
    banner.remove();
  }

  // Build the sidebar+detail layout once; on later ticks it's already there.
  let layout = panel.querySelector('.status-layout');
  if (!layout) {
    panel.innerHTML = '';
    // Re-add the warnings banner if it existed (innerHTML='' wiped it).
    if (ws.length) {
      const items = ws.map((w) => `<div class="msg warn">⚠ ${esc(w)}</div>`).join('');
      panel.insertAdjacentHTML('afterbegin', `<div class="status-warnings" style="margin-bottom:12px;">${items}</div>`);
    }
    const navItems = STATUS_SECTIONS.map((s) => {
      const active = s.key === statusSelected ? ' active' : '';
      return `<button class="acct-nav-item status-nav-item${active}" data-section="${esc(s.key)}">
        <span class="acct-nav-name">${esc(s.label)}</span>
      </button>`;
    }).join('');
    layout = document.createElement('div');
    layout.className = 'accounts-layout status-layout';
    layout.innerHTML = `<nav class="acct-nav" aria-label="Status sections">
        <div class="acct-nav-title">Sections</div>
        ${navItems}
      </nav>
      <div class="status-main"></div>`;
    panel.appendChild(layout);
    layout.querySelectorAll('.status-nav-item').forEach((b) => {
      b.addEventListener('click', () => selectStatusSection(b.dataset.section));
    });
  } else {
    // Layout exists: just refresh the active-section nav highlight in case
    // statusSelected changed via hashchange.
    layout.querySelectorAll('.status-nav-item').forEach((b) => {
      b.classList.toggle('active', b.dataset.section === statusSelected);
    });
  }
  renderStatusSection(statusSelected);
}
```

- [ ] **Step 6: Extend `parseHash` to recognize `#status/<section>`**

In `parseHash` (~`app.js:186-197`), the `status` branch currently ignores `rest`. Update it to capture the sub-segment. Replace:

```javascript
  if (tab === 'config' || tab === 'accounts' || tab === 'status') {
    // decodeURIComponent so provider names with special chars round-trip; a
    // malformed sequence decodes to "" (treated as "no sub" -> first provider).
    let sub = '';
    try { sub = decodeURIComponent(rest.join('/')); } catch (_) { sub = ''; }
    return { tab, sub };
  }
  return { tab: 'status', sub: '' };
```

with:

```javascript
  if (tab === 'config' || tab === 'accounts' || tab === 'status') {
    // decodeURIComponent so provider/section names with special chars
    // round-trip; a malformed sequence decodes to "" (treated as "no sub" ->
    // first provider / default section).
    let sub = '';
    try { sub = decodeURIComponent(rest.join('/')); } catch (_) { sub = ''; }
    return { tab, sub };
  }
  return { tab: 'status', sub: '' };
```

(The `sub` is already returned for every tab; the `status` dispatcher in the next steps reads it. This is just a comment touch-up so the contract is explicit — `parseHash` already returns `{tab:'status', sub:'<section>'}` for `#status/schedule`.)

- [ ] **Step 7: Handle `#status/<section>` on boot**

In the boot block (~`app.js:1683-1693`), preset `statusSelected` from the hash before the silent activate. Replace:

```javascript
const { tab: bootTab, sub: bootSub } = parseHash();
if (bootTab === 'accounts' && bootSub) {
  accountsSelectedProvider = bootSub;
}
if (bootTab === 'config') {
  activateTabSilent('config');
} else if (bootTab === 'accounts') {
  activateTabSilent('accounts');
} else {
  activateTabSilent('status');
}
```

with:

```javascript
const { tab: bootTab, sub: bootSub } = parseHash();
if (bootTab === 'accounts' && bootSub) {
  accountsSelectedProvider = bootSub;
}
if (bootTab === 'status' && bootSub && STATUS_SECTIONS.some((s) => s.key === bootSub)) {
  statusSelected = bootSub;
}
if (bootTab === 'config') {
  activateTabSilent('config');
} else if (bootTab === 'accounts') {
  activateTabSilent('accounts');
} else {
  activateTabSilent('status');
}
```

- [ ] **Step 8: Handle `#status/<section>` on hashchange**

In the `hashchange` listener (~`app.js:251-259`), add a status-section branch. Replace:

```javascript
window.addEventListener('hashchange', () => {
  const { tab, sub } = parseHash();
  if (tab !== activeTab) {
    activateTabSilent(tab);
  }
  if (tab === 'accounts' && sub) {
    selectProviderSilent(sub);
  }
});
```

with:

```javascript
window.addEventListener('hashchange', () => {
  const { tab, sub } = parseHash();
  if (tab !== activeTab) {
    activateTabSilent(tab);
  }
  if (tab === 'accounts' && sub) {
    selectProviderSilent(sub);
  }
  if (tab === 'status' && sub) {
    selectStatusSectionSilent(sub);
  }
});
```

- [ ] **Step 9: Verify parse**

Run: `node --check model-proxy/web_assets/app.js`
Expected: exit 0, no output.

- [ ] **Step 10: Build + run the Go gates**

```bash
cd model-proxy
gofmt -l .
go vet ./...
go test ./...
```
Expected: `gofmt -l .` prints nothing; `go vet`/`go test` pass. (No Go files changed, but the gates run on the whole module and must stay clean.)

- [ ] **Step 11: Manual browser verification**

Build and run (the Web UI is embedded from `web_assets/`):

```bash
cd model-proxy
go build -o model-proxy .
./model-proxy serve   # or however the daemon is started locally
```

Open the Web UI, go to Status, and confirm:
1. Sidebar shows Schedule · Providers · Quota · Token Usage · Logs; Schedule is active by default.
2. Clicking each nav item switches only the right pane; the sidebar highlight moves.
3. `#status/<section>` updates in the URL on click; refresh lands on the same section; browser Back/Forward moves between sections.
4. If warnings exist, a Warnings banner shows above the sidebar; with no warnings it's absent.
5. Logs: no blank line between entries; newest at the bottom; on load it's scrolled to the bottom; while parked at the bottom, new lines auto-scroll into view every 5s; scroll up — the visible content stays frozen across 5s refreshes; scroll back to the bottom — auto-snap resumes.
6. Switching away from Logs and back preserves nothing special (Logs re-renders from cache and re-snaps to bottom — acceptable, since the pane was off-screen).

- [ ] **Step 12: Commit**

```bash
git add model-proxy/web_assets/app.js model-proxy/web_assets/styles.css
git commit -m "feat(web): Status tab sidebar layout with per-section render + logs scroll behavior"
```

---

## Self-Review

**1. Spec coverage:**
- Sidebar layout (Schedule, Providers, Quota, Token Usage, Logs) → Task 4 (Step 2 `STATUS_SECTIONS`, Step 5 layout).
- Warnings as top banner → Task 4 Step 5 (`renderStatusPanel` banner).
- `statusSelected` preserved across re-renders → Task 4 Step 2 state, Step 5 preserves layout across ticks.
- `selectStatusSection` renders only `.status-main` → Task 4 Steps 3-4.
- Hash routing `#status/<section>` → Task 4 Steps 6-8.
- Single batched fetch, cache, render only active section → Task 4 Step 5 (`renderStatusTab` + `renderStatusPanel` + `renderStatusSection`).
- Section switch renders from cache, no extra fetch → Task 4 Step 4 (`selectStatusSectionSilent` → `renderStatusSection` reads `statusCache`).
- Logs: no blank line → Task 1.
- Logs: order unchanged (oldest→newest, newest at bottom) → Task 1 (no reversal).
- Logs: auto-snap to bottom by default → Task 3 Step 2 (`wasAtBottom` → `scrollTop = scrollHeight`).
- Logs: freeze on scroll-up → Task 3 Step 2 (`logsPrevScrollTop` restore).
- Logs: skip no-op ticks → Task 3 Step 2 (`key === logsPrevKey` early return).
- Reuse Accounts CSS classes, add `.status-main` → Task 4 Step 1; classes used in Step 5.
- No backend / handler / contract changes → no `web.go` edits in any task. ✓
- Go gates clean → Task 4 Step 10. ✓

**2. Placeholder scan:** No TBD/TODO; every code step shows full code. ✓

**3. Type/signature consistency:**
- `renderLogsCard(target, lines)` (Task 1) is called by `renderLogsInto` (Task 3) and matches.
- `renderLogsInto(target, lines)` (Task 3) is called by `renderStatusSection` (Task 4 Step 3) as `renderLogsInto(main, statusCache.logs || [])` — matches.
- Per-section renderers `renderXxxCard(target, ...)` (Task 2) are called in `renderStatusSection` (Task 4 Step 3) with `(main, st)` / `(main, statusCache.tok)` — matches.
- `selectStatusSection(name, push=true)` / `selectStatusSectionSilent(name)` (Task 4 Step 4) match the boot (Step 7, uses `statusSelected` directly) and hashchange (Step 8, calls `selectStatusSectionSilent`) usages.
- `STATUS_SECTIONS` keyed `schedule/providers/quota/tokens/logs` (Step 2) matches the switch cases in `renderStatusSection` (Step 3). ✓
