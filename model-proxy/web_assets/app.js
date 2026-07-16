// model-proxy admin SPA. Vanilla JS module — no framework, no CDN.
//
// Drives the three tabs (#tab-status / #tab-config / #tab-accounts) and two
// modals (#login-modal for async aqp/codex login, #add-modal for apikey add)
// defined in index.html. All backend calls go to same-origin /api/* endpoints.
//
// Security posture: every value interpolated into innerHTML is run through
// esc() first. We prefer textContent (inherently safe) wherever no markup is
// required. API keys are never read back from the server — /api/accounts
// returns only {id,label,added_at,email} — and the add forms' inputs are
// cleared the instant a POST succeeds.

// ---------- tiny DOM helpers ----------

// esc escapes a value for safe interpolation into an HTML text or attribute
// context. Covers the five chars that matter (& < > " '). Used on EVERY
// interpolated value — provider names, labels, log lines, error messages, YAML.
function esc(s) {
  if (s === null || s === undefined) return '';
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

// el builds an element with optional className/text/attrs/handlers.
function el(tag, opts = {}) {
  const e = document.createElement(tag);
  if (opts.cls) e.className = opts.cls;
  if (opts.text !== undefined) e.textContent = opts.text;
  if (opts.attrs) for (const [k, v] of Object.entries(opts.attrs)) e.setAttribute(k, v);
  if (opts.on) for (const [evt, fn] of Object.entries(opts.on)) e.addEventListener(evt, fn);
  return e;
}

// fmtNum renders an integer with thousands separators.
function fmtNum(n) {
  if (n === null || n === undefined) return '0';
  return Number(n).toLocaleString('en-US');
}

// fmtTime renders an RFC3339 string as a local HH:MM:SS.
function fmtTime(s) {
  if (!s) return '—';
  const d = new Date(s);
  if (isNaN(d.getTime())) return String(s);
  return d.toLocaleTimeString('en-US', { hour12: false });
}

// hasReset reports whether s is a real reset time. Go's zero time.Time
// serializes as "0001-01-01T00:00:00Z" - a truthy string, so a bare truthiness
// check would render a bogus "resets …" for windows that never set ResetsAt
// (e.g. deepseek's pay-as-you-go balance, which only has remaining quota).
// Treat null/empty/invalid/year-1 (the zero sentinel) as "no reset".
function hasReset(s) {
  if (!s) return false;
  const d = new Date(s);
  if (isNaN(d.getTime())) return false;
  return d.getUTCFullYear() > 1;
}

// fmtReset renders a reset timestamp for the Usage "resets …" line. A reset can
// be minutes, days, or weeks away, so a time-only format (HH:MM:SS) is wrong for
// anything beyond today - it drops the date and looks like "today 23:59:59".
// Mirrors the CLI's FormatResetAt: today -> "HH:MM", another day -> "MM-DD HH:MM".
// Includes the remaining duration ("in 6d 3h") so a far reset is unambiguous.
function fmtReset(s, now = Date.now()) {
  const d = new Date(s);
  if (isNaN(d.getTime())) return String(s);
  const sameDay = d.toLocaleDateString('en-CA') === new Date(now).toLocaleDateString('en-CA');
  const abs = sameDay
    ? d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit' })
    : `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit' })}`;
  const ms = d.getTime() - now;
  const dur = ms > 0 ? ` (in ${fmtDur(ms / 1000)})` : '';
  return `${abs}${dur}`;
}
function fmtUnix(sec) {
  if (!sec) return '—';
  const d = new Date(sec * 1000);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString('en-US', { hour12: false });
}

// fmtDur formats seconds as "Xd Yh" / "Yh Zm" / "Zm Ws", trimmed.
function fmtDur(sec) {
  if (sec == null || sec < 0 || isNaN(sec)) return '—';
  const s = Math.floor(sec);
  const days = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const mins = Math.floor((s % 3600) / 60);
  const secs = s % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${mins}m`;
  if (mins > 0) return `${mins}m ${secs}s`;
  return `${secs}s`;
}

// untilHuman renders "(in Xd Yh)" for a future RFC3339 timestamp.
function untilHuman(ts, now = Date.now()) {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '';
  const ms = d.getTime() - now;
  if (ms <= 0) return '';
  return ` (in ${fmtDur(ms / 1000)})`;
}

// ---------- API helpers ----------
//
// Each helper rejects on a non-ok response with the parsed body's `error`
// (falling back to status text), and resolves to parsed JSON on success. The
// rejection's .message is what the UI surfaces inline — never swallowed.

async function apiGet(path) {
  const r = await fetch(path, { headers: { accept: 'application/json' } });
  return apiParse(r);
}
async function apiPost(path, body) {
  const r = await fetch(path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', accept: 'application/json' },
    body: body === undefined ? '' : JSON.stringify(body),
  });
  return apiParse(r);
}
async function apiDel(path) {
  const r = await fetch(path, {
    method: 'DELETE',
    headers: { accept: 'application/json' },
  });
  return apiParse(r);
}
async function apiParse(r) {
  let txt = '';
  try { txt = await r.text(); } catch (_) { /* network blip */ }
  let data = null;
  try { data = txt ? JSON.parse(txt) : null; } catch (_) { data = null; }
  if (!r.ok) {
    const msg = (data && data.error) || txt || (`HTTP ${r.status}`);
    throw new Error(msg);
  }
  return data;
}

// ---------- connectivity + brand meta ----------

const liveDot = document.getElementById('live-dot');
const brandMeta = document.getElementById('brand-meta');

function setConn(state, meta) {
  // state: 'ok' | 'err' | 'warn'
  if (liveDot) {
    liveDot.classList.remove('ok', 'err', 'warn');
    liveDot.classList.add(state);
  }
  if (meta !== undefined && brandMeta) brandMeta.textContent = meta;
}

// ---------- tab navigation ----------

const tabBtns = Array.from(document.querySelectorAll('[data-tab]'));
const panels = {
  status: document.getElementById('tab-status'),
  config: document.getElementById('tab-config'),
  accounts: document.getElementById('tab-accounts'),
};
let activeTab = 'status';

// ---------- URL hash routing ----------
//
// The hash pins the current view so a page refresh (or shared link) lands on
// the same tab + (for Accounts) the same provider, instead of snapping back to
// Status. Shapes:
//   #status
//   #config
//   #accounts
//   #accounts/<provider>   (Accounts tab with a provider selected)
//
// activateTab/selectProvider push the hash; a hashchange listener (browser
// back/forward) re-activates without pushing, so the two stay in sync without a
// feedback loop. An unrecognized/empty hash defaults to #status.

function parseHash() {
  const raw = (location.hash || '').replace(/^#\/?/, ''); // drop leading "#"/"#/"
  const [tab, ...rest] = raw.split('/');
  if (tab === 'config' || tab === 'accounts' || tab === 'status') {
    // decodeURIComponent so provider/section names with special chars
    // round-trip; a malformed sequence decodes to "" (treated as "no sub" ->
    // first provider / default section). For #status/<section>, sub is the
    // section key read by selectStatusSectionSilent.
    let sub = '';
    try { sub = decodeURIComponent(rest.join('/')); } catch (_) { sub = ''; }
    return { tab, sub };
  }
  return { tab: 'status', sub: '' };
}

// pushHash sets the hash without re-triggering the hashchange listener (the
// caller already applied the view). Uses history.replaceState when possible so
// tab/provider switches don't each add a back-history entry (refresh/back still
// land on the current view); falls back to location.hash assignment.
// setHash updates the URL hash. `push` true adds a browser-history entry (so
// Back navigates between tabs); false replaces the current entry (for in-tab
// sub-navigation like picking a provider, which shouldn't spam history). Uses
// history API so no hashchange event fires (the caller already applied the
// view) - avoiding a feedback loop. Falls back to location.hash assignment.
function setHash(hash, push) {
  if (location.hash === hash) return;
  if (window.history && (push ? history.pushState : history.replaceState)) {
    if (push) history.pushState(null, '', hash || '#status');
    else history.replaceState(null, '', hash || '#status');
  } else {
    location.hash = hash;
  }
}

function tabHash(tab) {
  return '#' + tab;
}

function activateTab(name) {
  activeTab = name;
  for (const b of tabBtns) {
    const on = b.dataset.tab === name;
    b.classList.toggle('active', on);
    b.setAttribute('aria-selected', on ? 'true' : 'false');
  }
  for (const [k, p] of Object.entries(panels)) {
    if (p) p.classList.toggle('active', k === name);
  }
  if (name === 'status') {
    renderStatusTab();
  } else {
    stopStatusRefresh();
  }
  if (name === 'config') renderConfigTab();
  if (name === 'accounts') renderAccountsTab();
  // Reflect the tab in the URL. A tab switch is a navigation the user may want
  // to Back out of, so push a history entry. Accounts adds its provider segment
  // in selectProvider (replaceState - same tab, finer-grained). Status includes
  // its active section so a refresh lands on the same view.
  if (name === 'status') setHash('#status/' + statusSelected, true);
  else if (name !== 'accounts') setHash(tabHash(name), true);
}

for (const b of tabBtns) {
  b.addEventListener('click', () => activateTab(b.dataset.tab));
}

// hashchange: browser back/forward (or manual hash edit) drives the view. Apply
// the hash's tab + (Accounts) provider WITHOUT pushing back, avoiding a loop.
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

// activateTab without the hash push (called from hashchange).
function activateTabSilent(name) {
  activeTab = name;
  for (const b of tabBtns) {
    const on = b.dataset.tab === name;
    b.classList.toggle('active', on);
    b.setAttribute('aria-selected', on ? 'true' : 'false');
  }
  for (const [k, p] of Object.entries(panels)) {
    if (p) p.classList.toggle('active', k === name);
  }
  if (name === 'status') {
    renderStatusTab();
  } else {
    stopStatusRefresh();
  }
  if (name === 'config') renderConfigTab();
  if (name === 'accounts') renderAccountsTab();
}

// ---------- inline message helpers ----------

function showMsg(target, kind, msg) {
  // kind: 'ok' | 'err'
  if (!target) return;
  target.innerHTML = `<div class="msg ${esc(kind)}">${esc(msg)}</div>`;
}
function clearMsg(target) {
  if (target) target.innerHTML = '';
}

// ===========================================================================
// STATUS TAB
// ===========================================================================

let statusTimer = null;
let statusInflight = false;

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

function stopStatusRefresh() {
  if (statusTimer) { clearInterval(statusTimer); statusTimer = null; }
}

// refreshConnIndicator does a lightweight /api/status fetch solely to update
// the header connection dot + brand-meta (ok/err), independent of which tab is
// active. Called at boot so a refresh landing on #config or #accounts still
// shows the daemon's reachability (otherwise the header stays stuck on the
// initial "connecting…" - setConn('ok') otherwise only runs inside
// renderStatusTab, which the non-Status boot path skips).
async function refreshConnIndicator() {
  try {
    const st = await apiGet('/api/status');
    setConn('ok', `v${st.version || '?'} · ${st.uptime || '-'} · ${st.listen || ''}`);
  } catch (e) {
    setConn('err', 'connection lost');
  }
}

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
// layout, then renders the active section. Called after each fetch. The sidebar
// is built only when it isn't already present, so a 5s tick that finds the
// layout in place just refreshes the warnings + re-renders the active section,
// preserving scroll position (e.g. Logs scrolled up) in the pane.
function renderStatusPanel() {
  const panel = panels.status;
  if (!panel || !statusCache.st) return;
  const st = statusCache.st;
  const ws = st.warnings || [];

  // Warnings banner: render when present, remove when absent. It lives above the
  // layout so it doesn't disrupt the detail pane.
  let banner = panel.querySelector('.status-warnings');
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
    // panel may still hold a stale banner/error from before — rebuild cleanly,
    // then re-add the warnings banner if it existed (innerHTML='' wipes it).
    panel.innerHTML = '';
    if (ws.length) {
      const items = ws.map((w) => `<div class="msg warn">⚠ ${esc(w)}</div>`).join('');
      panel.insertAdjacentHTML('afterbegin', `<div class="status-warnings" style="margin-bottom:12px;">${items}</div>`);
    }
    const navItems = STATUS_SECTIONS.map((s) => {
      const active = s.key === statusSelected ? ' active' : '';
      return `<button class="status-nav-item${active}" data-section="${esc(s.key)}">
        <span class="status-nav-name">${esc(s.label)}</span>
      </button>`;
    }).join('');
    layout = document.createElement('div');
    layout.className = 'status-layout';
    layout.innerHTML = `<nav class="status-nav" aria-label="Status sections">
        <div class="status-nav-title">Sections</div>
        ${navItems}
      </nav>
      <div class="status-main"></div>`;
    panel.appendChild(layout);
    layout.querySelectorAll('.status-nav-item').forEach((b) => {
      b.addEventListener('click', () => selectStatusSection(b.dataset.section));
    });
  } else {
    // Layout exists: refresh the nav highlight in case statusSelected changed
    // (e.g. via hashchange) since the layout was built.
    layout.querySelectorAll('.status-nav-item').forEach((b) => {
      b.classList.toggle('active', b.dataset.section === statusSelected);
    });
  }
  renderStatusSection(statusSelected);
}

// renderStatusSection renders the named section's card(s) into .status-main
// from statusCache. Only the active section re-renders each 5s tick, so the
// Logs scroll position (and any open <details>) in other sections is never
// disturbed by a refresh of the visible section.
function renderStatusSection(key) {
  const main = document.querySelector('.status-main');
  if (!main) return;
  const st = statusCache.st;
  switch (key) {
    case 'schedule':
      main.innerHTML = '';
      if (st) renderScheduleCard(main, st);
      break;
    case 'providers':
      main.innerHTML = '';
      if (st) renderProvidersCard(main, st);
      break;
    case 'quota':
      main.innerHTML = '';
      if (st) renderQuotaCard(main, st);
      break;
    case 'tokens':
      main.innerHTML = '';
      renderTokensCard(main, statusCache.tok || []);
      break;
    case 'logs':
      // Don't clear here — renderLogsInto captures the existing scroll position
      // from the current .log-pre BEFORE wiping the container, so it can restore
      // it (freeze-on-scroll-up). Clearing here first would detach the old
      // .log-pre and defeat the capture.
      renderLogsInto(main, statusCache.logs || []);
      break;
  }
}

// selectStatusSection highlights the sidebar item, renders the section, and
// pins it in the URL hash (#status/<section>). push=true adds a history entry
// (a section click the user may Back out of); false replaces (used by the
// hashchange listener + boot, where the hash already reflects the target).
function selectStatusSection(name, push = true) {
  if (!STATUS_SECTIONS.some((s) => s.key === name)) name = 'schedule';
  selectStatusSectionSilent(name);
  if (push) setHash('#status/' + name, true);
}

// selectStatusSectionSilent renders without touching the hash (used by the
// hashchange listener + boot, where the hash already reflects the target).
function selectStatusSectionSilent(name) {
  if (!STATUS_SECTIONS.some((s) => s.key === name)) name = 'schedule';
  statusSelected = name;
  document.querySelectorAll('.status-nav-item').forEach((b) => {
    b.classList.toggle('active', b.dataset.section === name);
  });
  // Reset logs scroll state so switching back to Logs re-snaps to the bottom
  // (the previous .log-pre is gone; a fresh render should not try to restore a
  // stale scrollTop against a new element).
  logsPre = null;
  logsPrevKey = '';
  renderStatusSection(name);
}

// buildCard wraps a title + body in the .card/.card-head/.card-body shell.
function buildCard(title, meta, bodyHTML, extraBodyClass = '') {
  return `<section class="card">
    <header class="card-head"><h2>${esc(title)}</h2>${meta ? `<span class="meta">${esc(meta)}</span>` : ''}</header>
    <div class="card-body ${extraBodyClass}">${bodyHTML}</div>
  </section>`;
}

// renderWarningsCard surfaces implicit-route ambiguity warnings (a model served
// by >1 logged-in provider with no explicit route). Hidden when none.
function renderWarningsCard(target, st) {
  const ws = st.warnings || [];
  if (!ws.length) return;
  const items = ws.map(w => `<div class="msg warn">⚠ ${esc(w)}</div>`).join('');
  target.insertAdjacentHTML('beforeend', buildCard('Warnings', `${ws.length}`, items));
}

// healthPill renders a status pill reflecting circuit + rate-limit state.
// healthPill renders a status pill reflecting circuit + rate-limit state. A
// provider with no health record (never failed / never rate-limited; health is
// created lazily on first failure) shows a neutral "—" rather than "unknown".
function healthPill(h) {
  if (!h) return `<span class="pill muted"><span class="dot"></span>—</span>`;
  const now = Date.now();
  const rlUntil = h.rate_limited_until ? new Date(h.rate_limited_until).getTime() : 0;
  if (h.circuit_state === 'open' || h.circuit_state === 'half_open') {
    const tail = h.circuit_state === 'half_open' ? ' (probing)' : untilHuman(h.circuit_until, now);
    return `<span class="pill err" title="circuit ${esc(h.circuit_state)}"><span class="dot"></span>circuit ${esc(h.circuit_state)}${esc(tail)}</span>`;
  }
  if (rlUntil && rlUntil > now) {
    return `<span class="pill warn" title="rate-limited until ${esc(h.rate_limited_until)}"><span class="dot"></span>rate-limited${esc(untilHuman(h.rate_limited_until, now))}</span>`;
  }
  if (h.available) {
    return `<span class="pill ok"><span class="dot"></span>available</span>`;
  }
  return `<span class="pill muted"><span class="dot"></span>unavailable</span>`;
}

// renderProvidersCard draws the per-provider health + request-counter table.
//
// Provider names are enumerated from the SCHEDULE (the union of every route's
// ordered chain), NOT from `health`: a health entry is only created lazily when
// a provider fails or gets rate-limited, so a fresh daemon (or all-healthy
// providers) has health={} and the old health-only enumeration rendered nothing.
// Schedule is the authoritative source of "which providers are configured".
// `health` (may be absent → neutral "—") and `counters` (absent → 0) are joined
// per name.
function renderProvidersCard(target, st) {
  const health = st.health || {};
  const counters = st.counters || {};
  const models = (st.schedule && st.schedule.models) || {};
  const nameSet = new Set();
  for (const route of Object.keys(models)) {
    for (const p of (models[route].ordered || [])) {
      if (p && p.provider) nameSet.add(p.provider);
    }
  }
  const names = Array.from(nameSet).sort();
  if (names.length === 0) return;
  let rows = '';
  for (const name of names) {
    const c = counters[name] || {};
    rows += `<tr>
      <td class="mono">${esc(name)}</td>
      <td>${healthPill(health[name])}</td>
      <td class="num">${fmtNum(c.requests)}</td>
      <td class="num">${fmtNum(c.failovers)}</td>
      <td class="num">${fmtNum(c.rate_limited_429)}</td>
      <td class="num">${fmtNum(c.failures)}</td>
      <td class="num subdue">${esc(fmtUnix(c.last_request_at))}</td>
    </tr>`;
  }
  const html = buildCard('Providers', `${names.length} configured`, `
      <table class="table">
        <thead><tr>
          <th>provider</th><th>health</th>
          <th class="num">reqs</th><th class="num">failovers</th>
          <th class="num">429</th><th class="num">failures</th>
          <th class="num">last</th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table>`, 'flush');
  target.insertAdjacentHTML('beforeend', html);
}

// renderScheduleCard builds the per-route schedule view: each route shows its
// ordered provider chain with the first choice highlighted, sticky marker, and
// pool summary.
function renderScheduleCard(target, st) {
  const models = (st.schedule && st.schedule.models) || {};
  const names = Object.keys(models).sort();
  if (names.length === 0) return;
  let blocks = '';
  for (const route of names) {
    const info = models[route];
    const ordered = info.ordered || [];
    let chain = '';
    ordered.forEach((p, i) => {
      const classes = ['route-node'];
      if (i === 0) classes.push('first');
      if (info.sticky && info.sticky === p.provider) classes.push('sticky');
      if (!p.available) classes.push('unavailable');
      const peak = p.peak ? ' · peak' : '';
      const tier = p.tier ? ` · ${esc(p.tier)}` : '';
      const parent = p.pool_parent ? ` (${esc(p.pool_parent)})` : '';
      const title = `priority ${p.priority} · tier ${esc(p.tier || '?')} · surplus ${(p.surplus || 0).toFixed(2)}${peak}`;
      chain += `<span class="${classes.join(' ')}" title="${esc(title)}">${esc(p.provider)}${esc(parent)}${tier}</span>`;
      if (i < ordered.length - 1) chain += `<span class="route-sep">→</span>`;
    });
    if (!chain) chain = `<span class="route-meta">no providers available</span>`;
    let meta = '';
    if (info.sticky) {
      meta += `sticky: <span class="mono">${esc(info.sticky)}</span>`;
      if (info.sticky_dwell_remaining_sec) {
        meta += ` · ${fmtDur(info.sticky_dwell_remaining_sec)} dwell left`;
      }
    }
    let pools = '';
    if (info.pools && info.pools.length) {
      pools = '<span class="route-meta">';
      for (const pl of info.pools) {
        pools += `pool ${esc(pl.parent)}: ${pl.available}/${pl.accounts} available · `;
      }
      pools = pools.replace(/ · $/, '') + '</span>';
    }
    blocks += `<div class="route-block">
      <div class="route-head"><span class="route-name">${esc(route)}</span></div>
      <div class="route-chain">${chain}</div>
      ${meta ? `<div class="route-meta">${meta}</div>` : ''}
      ${pools}
    </div>`;
  }
  const html = buildCard('Schedule', `${names.length} routes`, blocks, 'flush');
  target.insertAdjacentHTML('beforeend', html);
}

// renderQuotaCard draws per-provider quota bars (ultimate window + short windows).
function renderQuotaCard(target, st) {
  const quota = st.quota || {};
  const names = Object.keys(quota).sort();
  if (names.length === 0) return;
  let rows = '';
  for (const name of names) {
    const snap = quota[name];
    if (!snap || snap.Err) {
      // Collapse the (often CLI-shaped, long) provider error to a scan-friendly
      // label here - the Accounts tab carries the actionable detail + Re-login.
      let lbl = 'no data';
      if (snap && snap.Err) {
        const k = quotaErrKind(snap);
        lbl = k === 'session-expired' ? 'session expired'
          : k === 'not-logged-in' ? 'not logged in' : 'error';
      }
      rows += `<div class="bar-row"><div class="bar-label">
        <span class="name">${esc(name)}</span>
        <span class="pct">${esc(lbl)}</span>
      </div></div>`;
      continue;
    }
    const windows = snap.Windows || [];
    let subBlock = '';
    for (const w of windows) {
      const p = (w.RemainingPct != null && w.RemainingPct >= 0) ? w.RemainingPct : null;
      const fillCls = p == null ? '' : (p > 0.3 ? 'ok' : (p > 0.1 ? 'warn' : 'err'));
      const ulg = w.Ultimate ? ' · ultimate' : (w.Short ? ' · short' : '');
      const reset = hasReset(w.ResetsAt) ? `resets ${esc(fmtReset(w.ResetsAt))}` : '';
      subBlock += `<div class="bar-row">
        <div class="bar-label">
          <span class="name">${esc(w.Label || 'quota')}${esc(ulg)}</span>
          <span class="pct">${p == null ? '—' : (p * 100).toFixed(1) + '%'}</span>
        </div>
        <div class="bar-track"><div class="bar-fill ${fillCls}" style="width:${p == null ? 0 : Math.max(0, Math.min(1, p)) * 100}%"></div></div>
        ${reset ? `<div class="bar-meta">${reset}</div>` : ''}
      </div>`;
    }
    const head = `${esc(name)}${snap.Account ? ' · ' + esc(snap.Account) : ''}${snap.Plan ? ' · ' + esc(snap.Plan) : ''}`;
    if (windows.length === 0) {
      rows += `<div class="bar-row"><div class="bar-label">
        <span class="name"><strong>${esc(head)}</strong></span>
      </div></div>`;
    } else {
      rows += `<div class="bar-row">
        <div class="bar-label"><span class="name"><strong>${esc(head)}</strong></span></div>
        <div style="grid-column:1/-1; padding-left: 10px; border-left: 2px solid var(--border-2); margin-bottom: 6px;">
          ${subBlock}
        </div>
      </div>`;
    }
  }
  const html = buildCard('Quota', `${names.length} providers`, rows);
  target.insertAdjacentHTML('beforeend', html);
}

// renderTokensCard draws the per-(provider, model) token usage table.
function renderTokensCard(target, usage) {
  if (!usage || usage.length === 0) {
    const html = buildCard('Token usage', '0',
      `<div class="empty-state">No observed usage yet. Counts accrue as the proxy streams SSE responses.</div>`);
    target.insertAdjacentHTML('beforeend', html);
    return;
  }
  const sorted = usage.slice().sort((a, b) =>
    (a.provider || '').localeCompare(b.provider || '') || (a.model || '').localeCompare(b.model || ''));
  let totalReqs = 0;
  let rows = '';
  for (const u of sorted) {
    totalReqs += Number(u.requests || 0);
    rows += `<tr>
      <td class="mono">${esc(u.provider)}</td>
      <td class="mono">${esc(u.model)}</td>
      <td class="num">${fmtNum(u.input)}</td>
      <td class="num">${fmtNum(u.output)}</td>
      <td class="num">${fmtNum(u.cache_creation)}</td>
      <td class="num">${fmtNum(u.cache_read)}</td>
      <td class="num">${fmtNum(u.requests)}</td>
    </tr>`;
  }
  const html = buildCard('Token usage', `${totalReqs} requests`, `
      <table class="table">
        <thead><tr>
          <th>provider</th><th>model</th>
          <th class="num">input</th><th class="num">output</th>
          <th class="num">cache create</th><th class="num">cache read</th>
          <th class="num">requests</th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table>
      <div class="row-actions" style="padding: 8px 12px;">
        <span class="spacer"></span>
        <button class="btn small" id="btn-tokens-reset">Reset counters</button>
      </div>`, 'flush');
  target.insertAdjacentHTML('beforeend', html);
  const btn = document.getElementById('btn-tokens-reset');
  if (btn) btn.addEventListener('click', resetTokens);
}

async function resetTokens() {
  const btn = document.getElementById('btn-tokens-reset');
  if (btn) { btn.disabled = true; btn.textContent = 'resetting…'; }
  try {
    await apiPost('/api/tokens/reset');
    await renderStatusTab();
  } catch (e) {
    if (btn) { btn.disabled = false; btn.textContent = 'Reset counters'; }
    window.alert('reset failed: ' + e.message);
  }
}

// renderLogsCard renders the tail of the daemon log into `target`. Entries are
// joined with no separator: each .log-line is display:block (styles.css), so
// they stack one-per-line without a blank gap (the old '\n' join inside a
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

// renderLogsInto renders the Logs card into `target` with scroll-preserving
// behavior. Called by the Status section dispatcher on each 5s tick.
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
  let wasAtBottom = true;
  if (logsPre && document.body.contains(logsPre)) {
    const bottomDist = logsPre.scrollHeight - logsPre.scrollTop - logsPre.clientHeight;
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

// ===========================================================================
// CONFIG TAB
// ===========================================================================

let configCache = null; // last /api/config response {yaml, summary, provider_models, routes}

// yamlEditor holds the CodeMirror instance for the Raw YAML editor (created in
// initYamlEditor per Config-tab render). Null outside the tab or if CodeMirror
// failed to load.
let yamlEditor = null;

// initYamlEditor mounts CodeMirror on the #yaml-editor host div. CodeMirror 5 is
// a UMD global loaded via <script> in index.html; if it's missing (vendor file
// absent / blocked) we degrade to a plain textarea so the editor still works.
function initYamlEditor() {
  yamlEditor = null;
  const host = document.getElementById('yaml-editor');
  if (!host) return;
  if (typeof CodeMirror === 'undefined') {
    host.innerHTML = `<textarea class="yaml" id="yaml-editor-fallback" spellcheck="false" autocomplete="off"></textarea>`;
    return;
  }
  host.innerHTML = '';
  yamlEditor = CodeMirror(host, {
    mode: 'yaml',
    lineNumbers: true,
    tabSize: 2,
    indentUnit: 2,
    indentWithTabs: false,
    matchBrackets: true,
    autoCloseBrackets: true,
    lineWrapping: false,
    extraKeys: {
      'Tab': (cm) => cm.replaceSelection('  ', 'end'),
      'Shift-Tab': (cm) => {
        // outdent: remove up to 2 leading spaces on the current line
        const cur = cm.getCursor();
        const line = cm.getLine(cur.line);
        const cut = line.startsWith('  ') ? 2 : (line.startsWith(' ') ? 1 : 0);
        if (cut) cm.replaceRange('', { line: cur.line, ch: 0 }, { line: cur.line, ch: cut });
      },
    },
  });
  // Refresh after the host is visible (CM measures size; a freshly-rendered
  // details/panel can have zero height at mount, leaving the editor blank).
  setTimeout(() => { if (yamlEditor) yamlEditor.refresh(); }, 0);
}

// getYamlValue returns the editor's text (CodeMirror or fallback textarea), or
// null if no editor is mounted.
function getYamlValue() {
  if (yamlEditor) return yamlEditor.getValue();
  const fb = document.getElementById('yaml-editor-fallback');
  return fb ? fb.value : null;
}

// setYamlValue writes text into the editor and refreshes (CM needs a refresh
// when shown in a previously-hidden container).
function setYamlValue(v) {
  if (yamlEditor) {
    yamlEditor.setValue(v || '');
    yamlEditor.refresh();
    return;
  }
  const fb = document.getElementById('yaml-editor-fallback');
  if (fb) fb.value = v || '';
}

async function renderConfigTab() {
  const panel = panels.config;
  panel.innerHTML =
    `<div id="config-summary" class="card"><div class="card-body"><span class="msg">loading…</span></div></div>
     <details class="editor" id="ed-provider"><summary>Provider scalars</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-route"><summary>Routes</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <div class="card">
       <header class="card-head"><h2>Raw YAML</h2><span class="meta" id="yaml-meta"></span></header>
       <div class="card-body">
         <div id="yaml-editor" class="yaml-cm-host"></div>
         <div class="row-actions" style="margin-top: 10px;">
           <span class="spacer"></span>
           <button class="btn small" id="btn-yaml-reload">Reload from disk</button>
           <button class="btn primary small" id="btn-yaml-save">Save &amp; reload</button>
         </div>
         <div id="yaml-msg"></div>
       </div>
     </div>`;
  // Mount the CodeMirror YAML editor on the host div. CodeMirror 5 is loaded as
  // a plain <script> in index.html (UMD global), so window.CodeMirror is defined
  // by the time this module runs. Created once per Config-tab render; the
  // instance is held in yamlEditor for setValue/getValue in load/save.
  initYamlEditor();
  document.getElementById('btn-yaml-reload').addEventListener('click', loadConfigYAML);
  document.getElementById('btn-yaml-save').addEventListener('click', saveConfigYAML);

  try {
    await loadConfigAll();
  } catch (e) {
    setConn('err');
    const sum = document.getElementById('config-summary');
    if (sum) showMsg(sum.querySelector('.card-body'), 'err', e.message);
  }
}

async function loadConfigAll() {
  const cfg = await apiGet('/api/config');
  configCache = cfg;
  const s = cfg.summary || {};
  document.getElementById('config-summary').outerHTML =
    `<div id="config-summary" class="card">
       <header class="card-head"><h2>Summary</h2></header>
       <div class="card-body">
         <dl class="kvs">
           <dt>listen</dt><dd>${esc(s.listen || '—')}</dd>
           <dt>providers</dt><dd>${fmtNum(s.provider_count)}</dd>
           <dt>routes</dt><dd>${fmtNum(s.route_count)}</dd>
         </dl>
       </div>
     </div>`;
  const ta = document.getElementById('yaml-editor');
  if (yamlEditor) {
    yamlEditor.setValue(cfg.yaml || '');
  } else if (ta) {
    ta.value = cfg.yaml || '';
  }
  const meta = document.getElementById('yaml-meta');
  if (meta) meta.textContent = `${(cfg.yaml || '').length} bytes`;

  // Provider scalars form
  buildProviderForm('ed-provider');

  // Route form
  buildRouteForm('ed-route');
}

// buildProviderForm: choose a provider → edit base URLs / usage_url / billing, or delete.
function buildProviderForm(editorId) {
  const ed = document.getElementById(editorId);
  if (!ed) return;
  ed.querySelector('.editor-body').innerHTML =
    `<div class="section-title">Provider scalars</div>
     <div class="field">
       <label for="prov-name">provider name</label>
       <input id="prov-name" name="name" type="text" list="prov-list" autocomplete="off">
       <datalist id="prov-list"></datalist>
       <span class="hint">Pick an existing provider to edit, or type a new name to add one.</span>
     </div>
     <div class="field">
       <label for="fld-provider_id">provider_id</label>
       <select id="fld-provider_id" name="provider_id">
         <option value=""></option>
         <option value="aqp">aqp</option>
         <option value="codex">codex</option>
         <option value="zhipu">zhipu</option>
         <option value="deepseek">deepseek</option>
         <option value="volcengine">volcengine</option>
         <option value="static">static</option>
       </select>
       <span class="hint">Required for a new provider. Selects the auth/rewrite implementation. static = no login (put the key in \`headers\` in the YAML editor).</span>
     </div>
     <div class="field"><label for="fld-openai_base_url">openai_base_url</label><input id="fld-openai_base_url" name="openai_base_url" type="text"></div>
     <div class="field"><label for="fld-anthropic_base_url">anthropic_base_url</label><input id="fld-anthropic_base_url" name="anthropic_base_url" type="text" placeholder="optional override for /v1/messages"></div>
     <div class="field"><label for="fld-usage_url">usage_url</label><input id="fld-usage_url" name="usage_url" type="text"></div>
     <div class="field"><label for="fld-billing">billing</label>
       <select id="fld-billing" name="billing">
         <option value="plan">plan</option>
         <option value="pay-as-you-go">pay-as-you-go</option>
       </select>
     </div>
     <div class="field"><label for="fld-models">models</label>
       <textarea id="fld-models" name="models" rows="4" placeholder="one model per line (the upstream's real model id)&#10;e.g. glm-4.6&#10;glm-4.5-air"></textarea>
       <span class="hint">The provider's model whitelist (matches \`models:\` in config). Empty = leave unchanged. Prefer \`models refresh <provider>\` to auto-fetch.</span>
     </div>
     <div class="row-actions">
       <button class="btn danger small" id="btn-prov-delete">Delete provider</button>
       <span class="spacer"></span>
       <button class="btn primary small" id="btn-prov-save">Apply</button>
     </div>
     <div class="msg" id="prov-msg"></div>`;
  populateProviderDatalist();
  document.getElementById('btn-prov-save').addEventListener('click', applyProviderEdit);
  document.getElementById('btn-prov-delete').addEventListener('click', deleteProvider);
}

// providerAccounts caches /api/accounts providers so the provider form can
// (a) detect an existing vs. new name and (b) prefill provider_id/billing when
// editing. Refreshed by populateProviderDatalist on each form build.
let providerAccounts = [];

async function populateProviderDatalist() {
  try {
    const [st, acc] = await Promise.all([
      apiGet('/api/status').catch(() => null),
      apiGet('/api/accounts').catch(() => null),
    ]);
    providerAccounts = (acc && acc.providers) || [];
    const names = new Set();
    if (st && st.health) Object.keys(st.health).forEach((n) => names.add(n));
    providerAccounts.forEach((p) => names.add(p.name));
    const dl = document.getElementById('prov-list');
    if (dl) dl.innerHTML = Array.from(names).sort().map((n) => `<option value="${esc(n)}">`).join('');
    // Prefill provider_id/billing when the name input already matches an
    // existing provider (e.g. the datalist was picked). Fires on every keystroke
    // so the fields track the selected name.
    const nameInp = document.getElementById('prov-name');
    if (nameInp && !nameInp.dataset.wired) {
      nameInp.dataset.wired = '1';
      nameInp.addEventListener('input', prefillProviderForm);
    }
    prefillProviderForm();
  } catch (_) { /* best-effort */ }
}

// providerExists reports whether `name` is a configured provider (per the cached
// /api/accounts list). Used to decide whether provider_id is required.
function providerExists(name) {
  return providerAccounts.some((p) => p.name === name);
}

// prefillProviderForm loads an existing provider's provider_id + billing + models
// into the form fields when its name is entered, and clears them for a new name
// so the user must pick a provider_id. Base URLs / usage_url aren't exposed by
// /api/accounts, so those stay manual (the YAML editor covers full edits).
// models come from /api/config's provider_models (cached in configCache).
function prefillProviderForm() {
  const name = (document.getElementById('prov-name').value || '').trim();
  const p = providerAccounts.find((x) => x.name === name);
  const pidSel = document.getElementById('fld-provider_id');
  const billSel = document.getElementById('fld-billing');
  const modelsInp = document.getElementById('fld-models');
  if (pidSel) pidSel.value = p ? (p.provider_id || '') : '';
  if (billSel) billSel.value = p ? (p.billing || 'plan') : 'plan';
  if (modelsInp) {
    const list = (configCache && configCache.provider_models && configCache.provider_models[name]) || [];
    modelsInp.value = list.join('\n');
  }
}

async function applyProviderEdit() {
  const msg = document.getElementById('prov-msg');
  const btn = document.getElementById('btn-prov-save');
  const name = (document.getElementById('prov-name').value || '').trim();
  if (!name) { showMsg(msg, 'err', 'provider name required'); return; }
  const data = {};
  // provider_id (select) + scalar text/select fields. provider_id is required
  // for a new provider (config.validate rejects an empty provider_id); for an
  // existing one it can be left unchanged, so we only send it when set.
  const pidInp = document.getElementById('fld-provider_id');
  const pid = (pidInp ? pidInp.value : '').trim();
  if (pid) data.provider_id = pid;
  const isNew = !providerExists(name);
  if (isNew && !pid) {
    showMsg(msg, 'err', 'provider_id is required for a new provider');
    return;
  }
  for (const fld of ['openai_base_url', 'anthropic_base_url', 'usage_url', 'billing']) {
    const inp = document.getElementById('fld-' + fld);
    const v = (inp ? inp.value : '').trim();
    if (v) data[fld] = v;
  }
  // models: textarea -> string array (one per line, blanks dropped). Only sent
  // when non-empty so an untouched/blank field leaves the existing list intact
  // (the prefill loads the current list, so editing is non-destructive). To
  // clear a provider's models, use the YAML editor.
  const modelsInp = document.getElementById('fld-models');
  if (modelsInp) {
    const models = modelsInp.value.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
    if (models.length) data.models = models;
  }
  btn.disabled = true;
  showMsg(msg, 'ok', 'saving…');
  try {
    await apiPost('/api/config/edit', { kind: 'provider', name, data });
    await loadConfigAll();
    showMsg(msg, 'ok', 'saved & reloaded');
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    btn.disabled = false;
  }
}

async function deleteProvider() {
  const msg = document.getElementById('prov-msg');
  const name = (document.getElementById('prov-name').value || '').trim();
  if (!name) { showMsg(msg, 'err', 'provider name required'); return; }
  if (!window.confirm(`Delete provider "${name}"? This removes its block from config.yaml (credentials on disk are unaffected).`)) return;
  try {
    await apiPost('/api/config/edit', { kind: 'provider', name, data: { delete: true } });
    await loadConfigAll();
    showMsg(msg, 'ok', `deleted ${name}`);
  } catch (e) {
    showMsg(msg, 'err', e.message);
  }
}

// buildRouteForm: pick an existing route (or type a new name) → edit its targets
// as structured rows [provider ▾ | model ▾ | priority]. Replaces the old free-text
// "provider/model per line" textarea. Provider options = configured providers
// (keys of provider_models); model options = that provider's models list.
function buildRouteForm(editorId) {
  const ed = document.getElementById(editorId);
  if (!ed) return;
  const routeNames = Object.keys((configCache && configCache.routes) || {}).sort();
  ed.querySelector('.editor-body').innerHTML =
    `<div class="section-title">Routes</div>
     <div class="field"><label for="route-name">exposed model name</label>
       <input id="route-name" name="name" type="text" list="route-list" autocomplete="off" placeholder="e.g. glm-4.6 (pick existing or type new)">
       <datalist id="route-list">${routeNames.map((n) => `<option value="${esc(n)}">`).join('')}</datalist>
       <span class="hint">Pick an existing route to edit, or type a new name to create one.</span>
     </div>
     <div class="field"><label>targets</label>
       <div class="route-targets" id="route-targets"></div>
       <span class="hint">Each row is one failover target. Provider options come from configured providers; model options from that provider's models. Priority lower = tried first; empty = 0.</span>
     </div>
     <div class="row-actions">
       <button class="btn small" id="btn-route-addrow" type="button">+ add target</button>
       <span class="spacer"></span>
       <button class="btn danger small" id="btn-route-delete">Delete route</button>
       <button class="btn primary small" id="btn-route-save">Apply</button>
     </div>
     <div class="msg" id="route-msg"></div>`;
  // When the route name matches an existing route, load its targets as rows.
  const nameInp = document.getElementById('route-name');
  nameInp.addEventListener('input', () => syncRouteRowsFromConfig(nameInp.value.trim()));
  document.getElementById('btn-route-addrow').addEventListener('click', () => addRouteTargetRow({ provider: '', model: '', priority: '' }));
  document.getElementById('btn-route-save').addEventListener('click', applyRouteEdit);
  document.getElementById('btn-route-delete').addEventListener('click', deleteRoute);
  // Prefill for an existing route (or one empty row if none).
  syncRouteRowsFromConfig(nameInp.value.trim());
  if (!document.getElementById('route-targets').children.length) {
    addRouteTargetRow({ provider: '', model: '', priority: '' });
  }
}

// providerOptions / modelsForProvider read the cached config so every row shares
// one source of truth (provider_models from /api/config).
function providerOptions() {
  return Object.keys((configCache && configCache.provider_models) || {}).sort();
}
function modelsForProvider(provider) {
  const pm = (configCache && configCache.provider_models) || {};
  return pm[provider] || [];
}

// addRouteTargetRow appends one editable target row to #route-targets. `t` is
// {provider, model, priority}. The model select includes the provider's known
// models PLUS the current value (so a route referencing an unlisted model isn't
// silently dropped on edit).
function addRouteTargetRow(t) {
  const box = document.getElementById('route-targets');
  if (!box) return;
  const row = document.createElement('div');
  row.className = 'route-target-row';
  const provs = providerOptions();
  const provOpts = provs.map((p) => `<option value="${esc(p)}"${p === t.provider ? ' selected' : ''}>${esc(p)}</option>`).join('');
  const models = modelsForProvider(t.provider);
  // ensure the current model is selectable even if not in the provider's list
  const modelSet = models.includes(t.model) ? models : [...models, t.model];
  const modelOpts = modelSet.map((m) => `<option value="${esc(m)}"${m === t.model ? ' selected' : ''}>${esc(m)}</option>`).join('');
  row.innerHTML =
    `<select class="rt-provider" autocomplete="off">${provOpts}</select>
     <select class="rt-model" autocomplete="off">${modelOpts}</select>
     <input class="rt-priority" type="number" min="0" inputmode="numeric" placeholder="0" value="${t.priority ? esc(String(t.priority)) : ''}">
     <button class="btn small danger" type="button" title="remove target" aria-label="remove target">×</button>`;
  // provider change → repopulate this row's model select (keep current value if
  // it's still valid for the new provider, else clear).
  const provSel = row.querySelector('.rt-provider');
  const modelSel = row.querySelector('.rt-model');
  provSel.addEventListener('change', () => {
    const cur = modelSel.value;
    const ms = modelsForProvider(provSel.value);
    modelSel.innerHTML = ms.map((m) => `<option value="${esc(m)}">`).join('');
    if (ms.includes(cur)) modelSel.value = cur;
  });
  row.querySelector('button').addEventListener('click', () => { row.remove(); });
  box.appendChild(row);
}

// syncRouteRowsFromConfig clears the rows and rebuilds them for the named route
// if it exists in the cached config; otherwise leaves the current rows alone
// (so typing a new name doesn't wipe in-progress edits — except on first load).
function syncRouteRowsFromConfig(name) {
  const box = document.getElementById('route-targets');
  if (!box) return;
  const routes = (configCache && configCache.routes) || {};
  const targets = routes[name];
  if (!targets || !targets.length) return; // not an existing route
  box.innerHTML = '';
  for (const t of targets) addRouteTargetRow(t);
}

// collectRouteTargets reads #route-targets rows into [{provider,model,priority?}].
// Skips fully-empty rows; requires provider+model on partial rows. Throws on a
// row missing provider or model.
function collectRouteTargets() {
  const box = document.getElementById('route-targets');
  if (!box) throw new Error('no target rows');
  const out = [];
  const rows = box.querySelectorAll('.route-target-row');
  for (const row of rows) {
    const provider = row.querySelector('.rt-provider').value.trim();
    const model = row.querySelector('.rt-model').value.trim();
    const pr = row.querySelector('.rt-priority').value.trim();
    if (!provider && !model && !pr) continue; // blank row - skip
    if (!provider) throw new Error('every target needs a provider');
    if (!model) throw new Error('every target needs a model');
    const t = { provider, model };
    if (pr !== '') t.priority = parseInt(pr, 10);
    out.push(t);
  }
  if (out.length === 0) throw new Error('add at least one target');
  return out;
}

async function applyRouteEdit() {
  const msg = document.getElementById('route-msg');
  const btn = document.getElementById('btn-route-save');
  const name = (document.getElementById('route-name').value || '').trim();
  if (!name) { showMsg(msg, 'err', 'route name required'); return; }
  let targets;
  try { targets = collectRouteTargets(); }
  catch (e) { showMsg(msg, 'err', e.message); return; }
  btn.disabled = true;
  showMsg(msg, 'ok', 'saving…');
  try {
    await apiPost('/api/config/edit', { kind: 'route', name, data: { targets } });
    await loadConfigAll();
    showMsg(msg, 'ok', 'saved & reloaded');
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    btn.disabled = false;
  }
}

async function deleteRoute() {
  const msg = document.getElementById('route-msg');
  const name = (document.getElementById('route-name').value || '').trim();
  if (!name) { showMsg(msg, 'err', 'route name required'); return; }
  if (!window.confirm(`Delete route "${name}"?`)) return;
  try {
    await apiPost('/api/config/edit', { kind: 'route', name, data: { delete: true } });
    await loadConfigAll();
    showMsg(msg, 'ok', `deleted ${name}`);
  } catch (e) {
    showMsg(msg, 'err', e.message);
  }
}

async function loadConfigYAML() {
  const meta = document.getElementById('yaml-meta');
  const msg = document.getElementById('yaml-msg');
  try {
    const cfg = await apiGet('/api/config');
    configCache = cfg;
    setYamlValue(cfg.yaml || '');
    if (meta) meta.textContent = `${(cfg.yaml || '').length} bytes`;
    if (msg) showMsg(msg, 'ok', 'reloaded from disk');
  } catch (e) {
    if (msg) showMsg(msg, 'err', e.message);
  }
}

async function saveConfigYAML() {
  const msg = document.getElementById('yaml-msg');
  const btn = document.getElementById('btn-yaml-save');
  const yaml = getYamlValue();
  if (yaml === null) return; // no editor mounted
  btn.disabled = true;
  showMsg(msg, 'ok', 'validating + reloading…');
  try {
    await apiPost('/api/config', { yaml });
    showMsg(msg, 'ok', 'saved & reloaded');
    await loadConfigAll();
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    btn.disabled = false;
  }
}

// ===========================================================================
// ACCOUNTS TAB
// ===========================================================================

// accountsData holds the last fetched snapshot backing the Accounts tab:
//   providers - /api/accounts response (per-provider account list)
//   quota     - /api/status .quota map, keyed by provider name (aqp/codex and
//               1-entry pools) or virtual id "name#<accountId>" (multi-entry pools)
//   tokens    - /api/tokens .usage array, keyed the same way per (provider, model)
// accountsSelectedProvider is the sidebar selection, preserved across re-renders
// (add/remove/tab-switch) so the user doesn't snap back to the first provider.
let accountsCache = null;
let accountsQuota = null;
let accountsTokens = null;
let accountsSelectedProvider = null;

// renderAccountsTab fetches the account list + the quota + token snapshots in
// parallel, then renders the provider sidebar + the selected provider's detail.
// /api/status and /api/tokens are best-effort (a young daemon may have neither):
// a failure degrades to "no usage data" / "no token usage" per account rather
// than breaking the whole tab.
async function renderAccountsTab() {
  const panel = panels.accounts;
  panel.innerHTML = `<div class="acct-tab-head"><div id="acc-msg"></div></div>
    <div class="accounts-layout">
    <nav class="acct-nav" aria-label="Providers"><span class="msg">loading…</span></nav>
    <div class="acct-main"></div>
  </div>`;
  try {
    const [acc, st, tok] = await Promise.all([
      apiGet('/api/accounts'),
      apiGet('/api/status').catch(() => null),
      apiGet('/api/tokens').catch(() => ({ usage: [] })),
    ]);
    accountsCache = acc;
    accountsQuota = (st && st.quota) || {};
    accountsTokens = (tok && tok.usage) || [];
    renderAccountsNav(acc.providers || []);
  } catch (e) {
    setConn('err');
    showMsg(document.getElementById('acc-msg'), 'err', e.message);
  }
}

// refreshAccountUsage re-polls ONE account's quota (POST /api/quota/refresh
// {provider: <key>} -> pollOne, which retries transient errors) then re-fetches
// /api/status and re-renders the selected provider's detail so the Usage
// section shows the fresh snapshot. The clicked button is disabled + shows a
// spinner while the poll runs (it blocks until that provider responds).
async function refreshAccountUsage(btn, p) {
  const key = btn.dataset.refresh;
  if (!key) return;
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = 'refreshing…';
  try {
    await apiPost('/api/quota/refresh', { provider: key });
    const st = await apiGet('/api/status').catch(() => null);
    if (st && st.quota) accountsQuota = st.quota;
    // Re-render the detail pane with the fresh snapshot (keeps the same
    // provider selected; selectProvider re-wires the buttons with a fresh,
    // enabled Refresh button - so no manual reset is needed on success).
    selectProvider(accountsSelectedProvider);
  } catch (e) {
    // On failure the pane was NOT re-rendered, so reset the clicked button.
    btn.disabled = false;
    btn.textContent = orig;
    showMsg(document.getElementById('acc-msg'), 'err', 'refresh failed: ' + e.message);
  }
}

// renderAccountsNav builds the left sidebar (sorted providers + account-count
// badges) and wires selection. If the previously selected provider is gone
// (e.g. removed), falls back to the first.
function renderAccountsNav(providers) {
  const nav = document.querySelector('.acct-nav');
  if (!nav) return;
  const sorted = providers.slice().sort((a, b) => (a.name || '').localeCompare(b.name || ''));
  const main = document.querySelector('.acct-main');
  if (sorted.length === 0) {
    nav.innerHTML = `<div class="acct-nav-title">Providers</div>
      <div class="acct-empty">No providers configured. Add one in <code>config.yaml</code> first.</div>`;
    if (main) main.innerHTML = '';
    accountsSelectedProvider = null;
    return;
  }
  nav.innerHTML = `<div class="acct-nav-title">Providers</div>` +
    sorted.map((p) => {
      const n = (p.accounts || []).length;
      const active = p.name === accountsSelectedProvider ? ' active' : '';
      return `<button class="acct-nav-item${active}" data-provider="${esc(p.name)}">
        <span class="acct-nav-name">${esc(p.name)}</span>
        <span class="badge ${n === 0 ? 'muted' : ''}">${n}</span>
      </button>`;
    }).join('');
  nav.querySelectorAll('.acct-nav-item').forEach((b) => {
    b.addEventListener('click', () => selectProvider(b.dataset.provider));
  });
  if (!accountsSelectedProvider || !sorted.some((p) => p.name === accountsSelectedProvider)) {
    accountsSelectedProvider = sorted[0].name;
  }
  selectProvider(accountsSelectedProvider);
}

// selectProvider highlights the sidebar item and renders that provider's
// account list (toolbar with Add + per-account cards). Only the .acct-main pane
// is re-rendered, so the sidebar stays wired and the scroll position is reset
// only for the detail. Also pins the provider in the URL hash
// (#accounts/<provider>) so a refresh lands on the same provider.
function selectProvider(name) {
  selectProviderSilent(name);
  if (name) setHash('#accounts/' + encodeURIComponent(name), false);
}

// selectProviderSilent renders without touching the hash (used by the hashchange
// listener + the initial boot, where the hash already reflects the target).
function selectProviderSilent(name) {
  accountsSelectedProvider = name;
  document.querySelectorAll('.acct-nav-item').forEach((b) => {
    b.classList.toggle('active', b.dataset.provider === name);
  });
  const providers = (accountsCache && accountsCache.providers) || [];
  const p = providers.find((x) => x.name === name);
  const main = document.querySelector('.acct-main');
  if (!main) return;
  if (!p) { main.innerHTML = ''; return; }
  // Capture which <details> sections are open BEFORE the re-render wipes them,
  // so we can restore the open state after (e.g. a Refresh-usage click re-renders
  // the pane - an expanded Usage section should stay expanded, not collapse).
  const openSecs = new Set();
  main.querySelectorAll('details.acct-section[open]').forEach((d) => {
    openSecs.add((d.dataset.acct || '') + '/' + (d.dataset.sec || ''));
  });
  main.innerHTML = renderProviderDetail(p, accountsQuota, accountsTokens);
  if (openSecs.size) {
    main.querySelectorAll('details.acct-section').forEach((d) => {
      if (openSecs.has((d.dataset.acct || '') + '/' + (d.dataset.sec || ''))) {
        d.open = true;
      }
    });
  }
  const add = main.querySelector('[data-add]');
  if (add) add.addEventListener('click', () => openAddFor(p));
  main.querySelectorAll('[data-remove]').forEach((b) => {
    b.addEventListener('click', () => removeAccount(b.dataset.provider, b.dataset.remove, b.dataset.label));
  });
  // Per-account "Refresh usage" - re-polls just this account's provider key
  // (a config name or "name#<accountID>") and re-renders the detail pane.
  main.querySelectorAll('[data-refresh]').forEach((b) => {
    b.addEventListener('click', () => refreshAccountUsage(b, p));
  });
  // Re-login buttons appear on session-expired / not-logged-in aqp/codex
  // accounts - they reuse the same async login flow as Add account (aqp is
  // single-credential, so re-login overwrites the stale SSO cookie in place).
  main.querySelectorAll('[data-relogin]').forEach((b) => {
    b.addEventListener('click', () => startAsyncLogin(p.name, p.provider_id));
  });
}

// renderProviderDetail builds the right pane: a toolbar (provider name + meta +
// Add button) and the list of account cards (or an empty-state prompt).
function renderProviderDetail(p, quota, tokens) {
  const meta = `${esc(p.provider_id || '?')}${p.billing ? ' · ' + esc(p.billing) : ''}`;
  const accounts = p.accounts || [];
  const body = accounts.length === 0
    ? `<div class="empty-state">No account configured. Click <strong>Add</strong> to sign in.</div>`
    : accounts.map((a) => accountCard(p, a, quota, tokens)).join('');
  return `<div class="acct-toolbar">
      <div class="acct-toolbar-title">
        <h2>${esc(p.name)}</h2>
        <span class="meta">${meta}</span>
      </div>
      <button class="btn small" data-add data-provider="${esc(p.name)}">+ Add account</button>
    </div>
    <div class="acct-list">${body}</div>`;
}

// accountProviderKey maps an account to the provider key under which its quota
// (status.quota) and token usage (/api/tokens) are recorded. aqp/codex are
// single-credential (plain name); a pooled provider with >=2 accounts keys each
// as "name#<accountId>" (virtual id); a 1-entry pool uses the plain name.
// Mirrors buildProviders (proxy.go) + the token-commit key (t.Provider).
function accountProviderKey(p, a) {
  const pooled = p.provider_id !== 'aqp' && p.provider_id !== 'codex';
  if (pooled && (p.accounts || []).length >= 2) return p.name + '#' + a.id;
  return p.name;
}

// accountCard renders one account: the existing info row (label / id / added /
// email / Remove) plus two collapsible <details> sections - Usage (the polled
// quota snapshot) and Token usage (per-model counters) - both scoped to this
// account's provider key.
function accountCard(p, a, quota, tokens) {
  const key = accountProviderKey(p, a);
  const label = a.label || a.id;
  const sub = `id: ${a.id || '-'}`;
  const added = a.added_at ? ` · added ${esc(fmtTime(a.added_at))}` : '';
  const mail = a.email ? `<div class="acct-mail">${esc(a.email)}</div>` : '';
  const snap = quota ? quota[key] : null;
  const tokRows = (tokens || []).filter((t) => t.provider === key);
  return `<section class="card acct-card">
    <div class="account-row acct-card-head">
      <div>
        <div class="acct-label">${esc(label)}</div>
        <div class="acct-sub">${esc(sub)}${added}</div>
        ${mail}
      </div>
      <div class="row-actions">
        <button class="btn small" data-refresh="${esc(key)}" title="Re-poll this account's quota now">Refresh usage</button>
        <button class="btn danger small" data-remove="${esc(a.id)}"
                data-provider="${esc(p.name)}" data-label="${esc(label)}">Remove</button>
      </div>
    </div>
    <div class="acct-sections">
      ${accountUsageDetails(p, snap, key)}
      ${accountTokensDetails(tokRows, key)}
    </div>
  </section>`;
}

// accountUsageDetails wraps the per-account quota snapshot in a collapsible
// section. The summary hint previews the state (remaining %, plan, or "no data")
// so the user can scan without expanding.
function accountUsageDetails(p, snap, acctKey) {
  let hint = 'no data';
  if (snap && snap.Err) {
    const k = quotaErrKind(snap);
    hint = k === 'session-expired' ? 'session expired'
      : k === 'not-logged-in' ? 'not logged in' : 'error';
  } else if (snap) {
    const ult = (snap.Windows || []).find((w) => w.Ultimate);
    if (ult && ult.RemainingPct != null && ult.RemainingPct >= 0) {
      // Same 1-decimal precision as the expanded window's pct (renderAccountUsage)
      // so the collapsed hint and the expanded bar agree (e.g. both "48.6%", not
      // "49%" vs "48.6%").
      hint = (ult.RemainingPct * 100).toFixed(1) + '% left';
    } else if (snap.Plan) {
      hint = snap.Plan;
    } else {
      hint = 'available';
    }
  }
  return `<details class="acct-section" data-acct="${esc(acctKey)}" data-sec="usage">
    <summary>Usage<span class="acct-hint">${esc(hint)}</span></summary>
    <div class="acct-section-body">${renderAccountUsage(p, snap)}</div>
  </details>`;
}

// accountTokensDetails wraps the per-account token counters in a collapsible
// section. The summary hint previews the model count + request total.
function accountTokensDetails(rows, acctKey) {
  let totalReqs = 0;
  for (const r of rows) totalReqs += Number(r.requests || 0);
  const hint = rows.length
    ? `${rows.length} model${rows.length > 1 ? 's' : ''} · ${fmtNum(totalReqs)} req`
    : 'no usage';
  return `<details class="acct-section" data-acct="${esc(acctKey)}" data-sec="tokens">
    <summary>Token usage<span class="acct-hint">${esc(hint)}</span></summary>
    <div class="acct-section-body">${renderAccountTokens(rows)}</div>
  </details>`;
}

// renderAccountUsage renders the quota snapshot body: account/plan/level head,
// provider notes, and each window as a bar-row (reusing the Status tab's quota
// bar rendering). An absent snapshot (pay-as-you-go / not polled) or an Err
// yields a graceful inline message. Per-window Details (e.g. volcengine
// by-model spend) are shown compactly when present.
//
// For SSO/OAuth providers (aqp/codex) a session-expired or not-logged-in error
// renders a Re-login button instead of the bare error text (the Web UI runs its
// own login flow via startAsyncLogin; the CLI-shaped error hint is useless in a
// browser). p may be null when the provider context is unavailable (Status tab
// reuses the bar rendering without an account context) - then the bare error is
// shown.
function quotaErrKind(snap) {
  if (!snap || !snap.Err) return '';
  const e = snap.Err.toLowerCase();
  if (e.includes('session expired')) return 'session-expired';
  if (e.includes('not logged in')) return 'not-logged-in';
  return 'error';
}

function renderAccountUsage(p, snap) {
  if (!snap) return `<div class="acct-empty">no usage data</div>`;
  if (snap.Err) {
    const canRelogin = p && (p.provider_id === 'aqp' || p.provider_id === 'codex');
    const k = quotaErrKind(snap);
    if (canRelogin && (k === 'session-expired' || k === 'not-logged-in')) {
      const label = k === 'session-expired' ? 'Session expired' : 'Not logged in';
      const verb = k === 'session-expired' ? 'Re-login' : 'Sign in';
      return `<div class="acct-empty acct-err">${label} - <button type="button" class="link-btn" data-relogin>${verb}</button></div>`;
    }
    return `<div class="acct-empty acct-err">${esc(snap.Err)}</div>`;
  }
  const bits = [];
  if (snap.Account) bits.push(esc(snap.Account));
  if (snap.Plan) bits.push(esc(snap.Plan));
  if (snap.Level) bits.push(esc(snap.Level));
  const head = bits.length ? `<div class="acct-quota-head">${bits.join(' · ')}</div>` : '';
  let notes = '';
  if (snap.Notes && snap.Notes.length) {
    notes = `<div class="acct-notes">${snap.Notes.map((n) => `<div>${esc(n)}</div>`).join('')}</div>`;
  }
  const windows = snap.Windows || [];
  let bars = '';
  for (const w of windows) {
    const p = (w.RemainingPct != null && w.RemainingPct >= 0) ? w.RemainingPct : null;
    const fillCls = p == null ? '' : (p > 0.3 ? 'ok' : (p > 0.1 ? 'warn' : 'err'));
    const ulg = w.Ultimate ? ' · ultimate' : (w.Short ? ' · short' : '');
    const reset = hasReset(w.ResetsAt) ? `resets ${esc(fmtReset(w.ResetsAt))}` : '';
    bars += `<div class="bar-row">
      <div class="bar-label">
        <span class="name">${esc(w.Label || 'quota')}${esc(ulg)}</span>
        <span class="pct">${p == null ? (w.Total > 0 ? fmtNum(w.Total) : '-') : (p * 100).toFixed(1) + '%'}</span>
      </div>
      <div class="bar-track"><div class="bar-fill ${fillCls}" style="width:${p == null ? 0 : Math.max(0, Math.min(1, p)) * 100}%"></div></div>
      ${reset ? `<div class="bar-meta">${reset}</div>` : ''}
    </div>`;
    if (w.Details && w.Details.length && w.DetailLabel) {
      bars += `<div class="acct-detail">
        <div class="acct-detail-label">${esc(w.DetailLabel)}</div>
        ${w.Details.map((d) => `<div class="acct-detail-row"><span>${esc(d.Label)}</span><span class="mono">${fmtNum(d.Used)}</span></div>`).join('')}
      </div>`;
    }
  }
  if (!bars) bars = `<div class="acct-empty">no quota windows</div>`;
  return head + notes + bars;
}

// renderAccountTokens renders the per-account token table (sorted by model)
// with a totals row. Empty -> inline message.
function renderAccountTokens(rows) {
  if (!rows || rows.length === 0) return `<div class="acct-empty">no token usage observed</div>`;
  const sorted = rows.slice().sort((a, b) => (a.model || '').localeCompare(b.model || ''));
  let tIn = 0, tOut = 0, tCC = 0, tCR = 0, tReq = 0;
  let trs = '';
  for (const r of sorted) {
    tIn += Number(r.input || 0); tOut += Number(r.output || 0);
    tCC += Number(r.cache_creation || 0); tCR += Number(r.cache_read || 0);
    tReq += Number(r.requests || 0);
    trs += `<tr>
      <td class="mono">${esc(r.model || '-')}</td>
      <td class="num">${fmtNum(r.input)}</td>
      <td class="num">${fmtNum(r.output)}</td>
      <td class="num">${fmtNum(r.cache_creation)}</td>
      <td class="num">${fmtNum(r.cache_read)}</td>
      <td class="num">${fmtNum(r.requests)}</td>
    </tr>`;
  }
  trs += `<tr class="acct-totals">
    <td>total</td>
    <td class="num">${fmtNum(tIn)}</td>
    <td class="num">${fmtNum(tOut)}</td>
    <td class="num">${fmtNum(tCC)}</td>
    <td class="num">${fmtNum(tCR)}</td>
    <td class="num">${fmtNum(tReq)}</td>
  </tr>`;
  return `<table class="table acct-tokens">
    <thead><tr>
      <th>model</th><th class="num">input</th><th class="num">output</th>
      <th class="num">cache create</th><th class="num">cache read</th><th class="num">requests</th>
    </tr></thead>
    <tbody>${trs}</tbody>
  </table>`;
}

// removeAccount confirms then DELETEs /api/accounts/<provider>/<id>.
async function removeAccount(provider, id, label) {
  if (!window.confirm(`Remove account "${label || id}" from ${provider}?`)) return;
  showMsg(document.getElementById('acc-msg'), 'ok', `removing ${label || id}…`);
  try {
    await apiDel(`/api/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}`);
    showMsg(document.getElementById('acc-msg'), 'ok', 'removed — reloading');
    await renderAccountsTab();
    clearMsg(document.getElementById('acc-msg'));
  } catch (e) {
    showMsg(document.getElementById('acc-msg'), 'err', e.message);
  }
}

// openAddFor dispatches the add flow based on provider type:
//   aqp/codex → #login-modal async flow (login URL or device code)
//   apikey    → #add-modal form (api_key, label; +access_key/secret_key for volcengine)
function openAddFor(p) {
  if (p.provider_id === 'aqp' || p.provider_id === 'codex') {
    startAsyncLogin(p.name, p.provider_id);
  } else {
    openAddModal(p.name, p.provider_id);
  }
}

// ---------- #add-modal: apikey add form ----------

function openAddModal(providerName, providerId) {
  const modal = document.getElementById('add-modal');
  if (!modal) return;
  const isVolc = providerId === 'volcengine';
  modal.setAttribute('aria-labelledby', 'add-title');
  modal.innerHTML =
    `<form method="dialog">
      <header class="modal-head">
        <h2 id="add-title">Add account — ${esc(providerName)}</h2>
        <button type="button" class="link-btn" id="add-cancel" aria-label="Close">Close</button>
      </header>
      <div class="modal-body">
        <p>Credentials are stored at <code>~/.model-proxy/${esc(providerName)}_apikey.json</code> and never logged.</p>
        <div class="field">
          <label for="add-label">label (optional)</label>
          <input id="add-label" type="text" placeholder="e.g. work / personal" autocomplete="off">
        </div>
        <div class="field">
          <label for="add-apikey">api_key</label>
          <input id="add-apikey" type="password" autocomplete="off" required>
        </div>
        ${isVolc ? `
          <div class="field">
            <label for="add-accesskey">access_key</label>
            <input id="add-accesskey" type="password" autocomplete="off">
            <span class="hint">Volcengine AK — used for the V4-signed control plane (GetAFPUsage quota).</span>
          </div>
          <div class="field">
            <label for="add-secretkey">secret_key</label>
            <input id="add-secretkey" type="password" autocomplete="off">
            <span class="hint">Volcengine SK paired with the access key above.</span>
          </div>` : ''}
        <div class="field check">
          <input id="add-replace" type="checkbox">
          <label for="add-replace">replace if same account id already exists</label>
        </div>
        <div class="row-actions">
          <span class="spacer"></span>
          <button type="button" class="btn primary small" id="add-submit">Add</button>
        </div>
        <div class="msg" id="add-msg"></div>
      </div>
    </form>`;
  modal.showModal();
  document.getElementById('add-cancel').addEventListener('click', () => modal.close());
  document.getElementById('add-submit').addEventListener('click', () => submitAdd(providerName));
}

async function submitAdd(providerName) {
  const msg = document.getElementById('add-msg');
  const btn = document.getElementById('add-submit');
  const apikey = document.getElementById('add-apikey').value;
  if (!apikey || !apikey.trim()) {
    showMsg(msg, 'err', 'api_key is required');
    return;
  }
  const body = { api_key: apikey.trim() };
  const label = (document.getElementById('add-label').value || '').trim();
  if (label) body.label = label;
  if (document.getElementById('add-accesskey')) {
    body.access_key = (document.getElementById('add-accesskey').value || '').trim();
    body.secret_key = (document.getElementById('add-secretkey').value || '').trim();
  }
  if (document.getElementById('add-replace').checked) body.replace = true;
  btn.disabled = true;
  showMsg(msg, 'ok', 'adding…');
  try {
    await apiPost(`/api/accounts/${encodeURIComponent(providerName)}`, body);
    showMsg(msg, 'ok', 'added — reloading');
    // Clear secrets from the DOM immediately on success.
    document.getElementById('add-apikey').value = '';
    const ak = document.getElementById('add-accesskey'); if (ak) ak.value = '';
    const sk = document.getElementById('add-secretkey'); if (sk) sk.value = '';
    setTimeout(() => {
      const m = document.getElementById('add-modal');
      if (m && m.open) m.close();
      renderAccountsTab();
    }, 350);
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    btn.disabled = false;
  }
}

// ---------- #login-modal: async aqp/codex login ----------

let loginPollTimer = null;

function setLoginModal(state, html) {
  const body = document.getElementById('login-body');
  if (!body) return;
  body.setAttribute('live-state', state);
  body.innerHTML = html;
}

function openLoginModal(title) {
  const titleEl = document.getElementById('login-title');
  if (titleEl) titleEl.textContent = title;
  setLoginModal('pending', `<p>Starting login flow…<span class="spinner"></span></p>`);
  const modal = document.getElementById('login-modal');
  if (modal && !modal.open) modal.showModal();
}

function closeLoginModal() {
  if (loginPollTimer) { clearInterval(loginPollTimer); loginPollTimer = null; }
  const modal = document.getElementById('login-modal');
  if (modal && modal.open) modal.close();
}

// startAsyncLogin drives the aqp SSO or codex device flow:
//   POST /api/login/<provider>/start → {session_id, login_url} (aqp) or {session_id, verify_url, user_code} (codex)
//   poll GET /api/login/<session_id>/poll every ~1.5s until state is done|error.
async function startAsyncLogin(providerName, providerId) {
  openLoginModal(`Sign in — ${providerName}`);
  let sessionId = null;
  try {
    const start = await apiPost(`/api/login/${encodeURIComponent(providerName)}/start`);
    sessionId = start.session_id;
    if (!sessionId) throw new Error('no session_id from server');
    if (providerId === 'codex') {
      setLoginModal('pending', `
        <p>Open the verification URL and enter the code when prompted:</p>
        <div class="modal-link"><a href="${esc(start.verify_url)}" target="_blank" rel="noopener">${esc(start.verify_url)}</a></div>
        <div class="code-block">${esc(start.user_code || '')}</div>
        <p class="modal-status"><span class="spinner"></span>waiting for authorization…</p>`);
    } else {
      setLoginModal('pending', `
        <p>Open this URL to sign in with SSO:</p>
        <div class="modal-link"><a href="${esc(start.login_url)}" target="_blank" rel="noopener">${esc(start.login_url)}</a></div>
        <p class="modal-status"><span class="spinner"></span>waiting for sign-in…</p>`);
    }
    pollLogin(sessionId);
  } catch (e) {
    setLoginModal('error', `
      <p>The login flow could not start:</p>
      <p class="modal-status err">${esc(e.message)}</p>
      <div class="modal-actions">
        <button type="button" class="btn small" id="login-done">Close</button>
      </div>`);
    const done = document.getElementById('login-done');
    if (done) done.onclick = closeLoginModal;
  }
}

function pollLogin(sessionId) {
  if (loginPollTimer) clearInterval(loginPollTimer);
  let ticks = 0;
  const tick = async () => {
    ticks += 1;
    try {
      const r = await apiGet(`/api/login/${encodeURIComponent(sessionId)}/poll`);
      if (r.state === 'done') {
        if (loginPollTimer) { clearInterval(loginPollTimer); loginPollTimer = null; }
        setLoginModal('done', `
          <p>Sign-in complete.</p>
          ${r.result ? `<p class="modal-status">account: ${esc(r.result)}</p>` : ''}
          <div class="modal-actions">
            <button type="button" class="btn primary small" id="login-done">Done</button>
          </div>`);
        const done = document.getElementById('login-done');
        if (done) done.onclick = () => {
          closeLoginModal();
          renderAccountsTab();
        };
        return;
      }
      if (r.state === 'error') {
        if (loginPollTimer) { clearInterval(loginPollTimer); loginPollTimer = null; }
        setLoginModal('error', `
          <p>Sign-in failed.</p>
          <p class="modal-status err">${esc(r.detail || 'unknown error')}</p>
          <div class="modal-actions">
            <button type="button" class="btn small" id="login-done">Close</button>
          </div>`);
        const done = document.getElementById('login-done');
        if (done) done.onclick = closeLoginModal;
        return;
      }
      // pending — keep polling. Periodically refresh the displayed detail in
      // case the UI recovered from a refresh (the server re-surfaces it).
      if (r.detail && ticks % 20 === 0) {
        const status = document.querySelector('#login-body .modal-status');
        if (status) status.innerHTML = `<span class="spinner"></span>${esc(r.detail)}`;
      }
    } catch (e) {
      // Network blip: keep polling up to a soft cap, then surface the error.
      if (ticks > 60) {
        if (loginPollTimer) { clearInterval(loginPollTimer); loginPollTimer = null; }
        setLoginModal('error', `
          <p>Polling failed.</p>
          <p class="modal-status err">${esc(e.message)}</p>
          <div class="modal-actions">
            <button type="button" class="btn small" id="login-done">Close</button>
          </div>`);
        const done = document.getElementById('login-done');
        if (done) done.onclick = closeLoginModal;
      }
    }
  };
  tick();
  loginPollTimer = setInterval(tick, 1500);
}

// ===========================================================================
// boot
// ===========================================================================

// Wire the persistent #login-cancel button (defined in the static HTML).
const loginCancelStatic = document.getElementById('login-cancel');
if (loginCancelStatic) loginCancelStatic.addEventListener('click', closeLoginModal);

// Initial render: activate the tab the URL hash names (so a refresh or shared
// link lands on the same view), defaulting to Status. For #accounts/<provider>,
// preset the selection so renderAccountsNav lands on it after the fetch.
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
// Update the header connection indicator regardless of the landing tab. The
// Status tab sets it via renderStatusTab, but #config/#accounts skip that, so
// without this the header would stay on "connecting…" until the user opens
// Status.
refreshConnIndicator();
