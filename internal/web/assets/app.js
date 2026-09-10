// model-proxy admin SPA. Vanilla JS module — no framework, no CDN.
//
// Drives the admin tabs and three modals (#admin-auth-modal for the browser
// session, #login-modal for async aqp/codex login, #add-modal for apikey add)
// defined in index.html. All backend calls go to same-origin /api/* endpoints.
//
// Security posture: every value interpolated into innerHTML is run through
// esc() first. We prefer textContent (inherently safe) wherever no markup is
// required. API keys are never read back from the server — /api/accounts
// returns only {id,label,added_at,email} — and the add forms' inputs are
// cleared the instant a POST succeeds.

// ---------- tiny DOM helpers ----------



// el builds an element with optional className/text/attrs/handlers.
import {
  esc, fmtNum, avgLatencyMs, hasReset, fmtDur, untilHuman,
  YAML_EDITOR_MIN_HEIGHT, visibleYamlEditorHeight,
  verdictBadge, modelCapMatrix, providerFrozen, providerNames, cacheHitRate,
  settingsDiff, settingsRestartKeys, TOKEN_RANGES, tokensRangeQuery, tokenRangeLabel,
  tokenRangeTriggerLabel, parseLocalDate, WEEKDAYS, monthTitle, calendarMonthGrid,
  twoMonthWindow, shiftMonth, ymd, isFutureDay, rangePick,
  parseSSE, isSSE, prettyJSON, highlightJSON, splitLinesByBudget, linkedModels,
  analyticsChartSeries, liveSessionSummary,
  fmtGuardDetail, fmtProgressBytes, mergeLiveAndPersistedRow, shouldFetchDetail,
  detailFetchState,
} from './pure.js';

function el(tag, opts = {}) {
  const e = document.createElement(tag);
  if (opts.cls) e.className = opts.cls;
  if (opts.text !== undefined) e.textContent = opts.text;
  if (opts.attrs) for (const [k, v] of Object.entries(opts.attrs)) e.setAttribute(k, v);
  if (opts.on) for (const [evt, fn] of Object.entries(opts.on)) e.addEventListener(evt, fn);
  return e;
}

// fmtNum renders an integer with thousands separators.




// fmtTime renders an RFC3339 string as a local HH:MM:SS.
function fmtTimeSafe(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  return isNaN(d.getTime()) ? String(ts) : fmtTime(d.toISOString());
}

function fmtTime(s) {
  if (!s) return '—';
  const d = new Date(s);
  if (isNaN(d.getTime())) return String(s);
  return d.toLocaleTimeString('en-US', { hour12: false });
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

// fmtSinceDate renders a unix-seconds anchor as the compact "YY-MM-DD HH:MM"
// used by the cumulative usage cards' "Since …" label (e.g. Since 26-09-04 14:15).
function fmtSinceDate(sec) {
  if (!sec) return '';
  const d = new Date(sec * 1000);
  if (isNaN(d.getTime())) return '';
  const p = (n, w = 2) => String(n).padStart(w, '0');
  return `${p(d.getFullYear() % 100)}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
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
    const err = new Error(msg);
    err.status = r.status;
    throw err;
  }
  return data;
}

// The static UI is intentionally public when admin auth is configured so it
// can bootstrap a browser. A valid bearer is exchanged exactly once for an
// HttpOnly, SameSite=Strict, /api-scoped session cookie; JavaScript clears the
// input immediately and never persists the bearer in storage or a URL.
async function establishAdminSessionIfRequired() {
  try {
    await apiGet('/api/status');
    return;
  } catch (e) {
    if (e.status !== 401) return; // daemon/network errors render normally
  }

  const dialog = document.getElementById('admin-auth-modal');
  const form = document.getElementById('admin-auth-form');
  const input = document.getElementById('admin-auth-token');
  const submit = document.getElementById('admin-auth-submit');
  const msg = document.getElementById('admin-auth-msg');
  if (!dialog || !form || !input || !submit || !msg) return;

  setConn('warn', 'admin authentication required');
  dialog.showModal();
  input.focus();
  await new Promise((resolve) => {
    const authenticate = async (event) => {
      event.preventDefault();
      let token = input.value;
      input.value = '';
      submit.disabled = true;
      msg.className = 'msg';
      msg.textContent = 'authenticating…';
      try {
        const response = await fetch('/api/auth/session', {
          method: 'POST',
          headers: { authorization: `Bearer ${token}` },
          cache: 'no-store',
        });
        await apiParse(response);
        form.removeEventListener('submit', authenticate);
        dialog.close();
        resolve();
      } catch (e) {
        msg.className = 'msg err';
        msg.textContent = e.message || 'authentication failed';
        input.focus();
      } finally {
        token = '';
        submit.disabled = false;
      }
    };
    form.addEventListener('submit', authenticate);
  });
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
  analytics: document.getElementById('tab-analytics'),
  requests: document.getElementById('tab-requests'),
  security: document.getElementById('tab-security'),
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
  if (tab === 'config' || tab === 'accounts' || tab === 'status' || tab === 'analytics' || tab === 'requests' || tab === 'security') {
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
    stopLiveEvents();
  }
  if (name === 'config') renderConfigTab();
  if (name === 'accounts') renderAccountsTab();
  if (name === 'analytics') renderAnalyticsTab();
  if (name === 'requests') renderRequestsTab();
  if (name === 'security') renderSecurityTab();
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
    stopLiveEvents();
  }
  if (name === 'config') renderConfigTab();
  if (name === 'accounts') renderAccountsTab();
  if (name === 'analytics') renderAnalyticsTab();
  if (name === 'requests') renderRequestsTab();
  if (name === 'security') renderSecurityTab();
}

// ---------- Requests tab (request-log query UI) ----------

// Per-tab filter state (model/provider substring + errors-only + shadow tri-state).
// Persists across re-renders within a session so a refresh keeps the view.
let requestsFilter = { session: '', model: '', provider: '', errors: false, shadow: '' };

// renderRequestsTab builds the request-log query view: a filter row + a table of
// metadata-only summaries fetched from /api/requests, with click-to-expand rows
// that load the full request/response bodies from /api/requests/<id>. On-demand
// (no 5s poll) — fetch happens on tab entry and on Refresh.
async function renderRequestsTab() {
  const panel = panels.requests;
  if (!panel) return;
  resetCombos();
  panel.innerHTML = `<div class="card"><div class="card-body">
    <div class="req-controls" style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-bottom:12px;">
      <select id="req-session" class="req-input" title="filter by client session"><option value="">all sessions</option></select>
      <span class="combo"><input id="req-provider" placeholder="all providers" value="${esc(requestsFilter.provider)}" class="req-input"/></span>
      <span class="combo"><input id="req-model" placeholder="all models" value="${esc(requestsFilter.model)}" class="req-input"/></span>
      <select id="req-shadow" class="req-input">
        <option value="" ${requestsFilter.shadow === '' ? 'selected' : ''}>all</option>
        <option value="only" ${requestsFilter.shadow === 'only' ? 'selected' : ''}>shadow only</option>
        <option value="exclude" ${requestsFilter.shadow === 'exclude' ? 'selected' : ''}>no shadow</option>
      </select>
      <label style="display:flex;align-items:center;gap:4px;"><input type="checkbox" id="req-errors" ${requestsFilter.errors ? 'checked' : ''}/> errors only</label>
      <button id="req-refresh" class="btn danger-solid">Refresh</button>
    </div>
    <div id="req-session-summary" style="margin-bottom:12px" hidden></div>
    <div id="req-table"></div>
  </div></div>`;
  // combos carries the data-driven provider/model facet state, the recent
  // session list (dropdown + per-session aggregate), and the last fetched rows
  // (so the session summary can render once the aggregate arrives).
  const combos = {
    providerOptions: [], modelOptions: [],
    facetState: { providerModels: {} },
    sessions: [], lastRecords: [],
  };
  const refresh = () => {
    requestsFilter.session = document.getElementById('req-session').value;
    requestsFilter.provider = document.getElementById('req-provider').value.trim();
    requestsFilter.model = document.getElementById('req-model').value.trim();
    requestsFilter.errors = document.getElementById('req-errors').checked;
    requestsFilter.shadow = document.getElementById('req-shadow').value;
    loadRequests(combos);
  };
  const onProviderSelect = () => {
    const provider = document.getElementById('req-provider').value.trim();
    const next = linkedModels(provider, combos.facetState.providerModels, {});
    combos.modelOptions.splice(0, combos.modelOptions.length, ...next);
    const modelInput = document.getElementById('req-model');
    if (modelInput && modelInput.value.trim() && !next.includes(modelInput.value.trim())) modelInput.value = '';
    refresh();
  };
  document.getElementById('req-refresh').onclick = refresh;
  document.getElementById('req-shadow').onchange = refresh;
  document.getElementById('req-session').onchange = refresh;
  // The checkbox applies immediately too — every filter control (session,
  // combos, shadow select, errors only) has the same on-change behavior.
  document.getElementById('req-errors').onchange = refresh;
  attachCombo(document.getElementById('req-provider'), combos.providerOptions, onProviderSelect);
  attachCombo(document.getElementById('req-model'), combos.modelOptions, refresh);
  loadRequests(combos);
  // Session dropdown options come from the persisted aggregate (request logging
  // may be off → empty list, select stays "all sessions"). Fetched after the
  // first load so the table renders immediately; the summary re-renders once
  // the aggregate for a persisted selection is available.
  apiGet('/api/sessions?limit=200').then((resp) => {
    combos.sessions = (resp && resp.sessions) || [];
    const sel = document.getElementById('req-session');
    if (!sel) return;
    const ids = combos.sessions.map((s) => s.session_id).filter(Boolean);
    if (requestsFilter.session && !ids.includes(requestsFilter.session)) ids.unshift(requestsFilter.session);
    sel.innerHTML = '<option value="">all sessions</option>' +
      ids.map((id) => `<option value="${esc(id)}">${esc(liveSessionLabel(id))}</option>`).join('');
    sel.value = requestsFilter.session;
    renderRequestsSessionSummary(combos);
  }).catch(() => { /* request logging off / unavailable */ });
}

// comboInstances tracks live comboboxes so one set of global listeners can
// close any open menu. Per-combo document/window listeners would leak on every
// Requests re-render.
const comboInstances = new Set();
let comboGlobalsWired = false;

function wireComboGlobals() {
  if (comboGlobalsWired) return;
  comboGlobalsWired = true;
  document.addEventListener('pointerdown', (event) => {
    comboInstances.forEach((combo) => {
      if (!combo.menu.hidden && event.target !== combo.input && !combo.menu.contains(event.target)) combo.close();
    });
  });
  const closeAll = () => comboInstances.forEach((combo) => combo.close());
  window.addEventListener('resize', closeAll);
  window.addEventListener('scroll', closeAll, true);
}

// resetCombos drops a previous Requests render's menus/state (its inputs are
// gone) so stale fixed-positioned menus cannot float over the new tab.
function resetCombos() {
  comboInstances.forEach((combo) => combo.menu.remove());
  comboInstances.clear();
}

// attachCombo turns a text input into a searchable dropdown. The menu is a
// themed, fixed-positioned list aligned to the input's left edge and width
// (the native <datalist> popup is browser chrome we cannot align or style).
// Typing filters, ArrowUp/Down moves, Enter picks the active option (or, with
// nothing active, falls through to onSelect), Escape/outside click closes.
function attachCombo(input, options, onSelect) {
  if (!input || input.dataset.comboWired === '1') return;
  input.dataset.comboWired = '1';
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-expanded', 'false');
  input.setAttribute('autocomplete', 'off');

  const menu = document.createElement('div');
  menu.className = 'combo-menu';
  menu.setAttribute('role', 'listbox');
  menu.hidden = true;
  document.body.appendChild(menu);
  let active = -1;

  const close = () => {
    if (menu.hidden) return;
    menu.hidden = true;
    input.setAttribute('aria-expanded', 'false');
    active = -1;
  };

  const open = () => {
    const query = input.value.trim().toLowerCase();
    const list = (query ? options.filter((option) => option.toLowerCase().includes(query)) : options).slice(0, 200);
    if (!list.length) {
      menu.innerHTML = '<div class="combo-empty">no matching option</div>';
    } else {
      menu.innerHTML = list.map((option) => `<div class="combo-option" role="option" data-value="${esc(option)}">${esc(option)}</div>`).join('');
    }
    menu.hidden = false;
    input.setAttribute('aria-expanded', 'true');
    active = -1;
    positionCombo(input, menu);
    menu.querySelectorAll('.combo-option').forEach((option) => {
      // mousedown (not click) so the pick lands before the input's blur.
      option.addEventListener('mousedown', (event) => {
        event.preventDefault();
        input.value = option.dataset.value;
        close();
        onSelect();
      });
    });
  };

  input.addEventListener('focus', open);
  input.addEventListener('click', open);
  input.addEventListener('input', open);
  input.addEventListener('keydown', (event) => {
    const items = [...menu.querySelectorAll('.combo-option')];
    if (event.key === 'Escape') { close(); return; }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      if (menu.hidden) { open(); return; }
      if (!items.length) return;
      active = event.key === 'ArrowDown'
        ? (active + 1) % items.length
        : (active - 1 + items.length) % items.length;
      items.forEach((item, index) => item.classList.toggle('active', index === active));
      items[active].scrollIntoView({ block: 'nearest' });
      return;
    }
    if (event.key === 'Enter') {
      // Pick the highlighted option when there is one, otherwise apply the
      // typed text (the backend match is a substring, so free text is valid).
      if (!menu.hidden && active >= 0 && items[active]) {
        event.preventDefault();
        input.value = items[active].dataset.value;
      }
      close();
      onSelect();
    }
  });
  input.addEventListener('blur', () => { setTimeout(close, 0); });
  comboInstances.add({ input, menu, close });
  wireComboGlobals();
}

// positionCombo pins the fixed-positioned menu under the input, flipping above
// and clamping to the viewport when there is no room below.
function positionCombo(input, menu) {
  const margin = 8;
  const rect = input.getBoundingClientRect();
  const box = menu.getBoundingClientRect();
  let left = rect.left;
  if (left + box.width > window.innerWidth - margin) left = window.innerWidth - margin - box.width;
  if (left < margin) left = margin;
  let top = rect.bottom + 4;
  if (top + box.height > window.innerHeight - margin) top = rect.top - box.height - 4;
  if (top < margin) top = margin;
  menu.style.left = `${left}px`;
  menu.style.top = `${top}px`;
  menu.style.minWidth = `${rect.width}px`;
}

// syncRequestFacets updates the two dropdowns from the response's data-driven
// facets (distinct providers/models observed in the log window, NOT the config
// catalog). The model list narrows to the selected provider via the facet's
// provider→models map.
function syncRequestFacets(facets, combos) {
  if (!combos) return;
  const data = facets || {};
  combos.facetState.providerModels = data.provider_models || {};
  const providers = data.providers || [];
  combos.providerOptions.splice(0, combos.providerOptions.length, ...providers);
  const providerInput = document.getElementById('req-provider');
  const provider = providerInput ? providerInput.value.trim() : '';
  const models = linkedModels(provider, combos.facetState.providerModels, {});
  combos.modelOptions.splice(0, combos.modelOptions.length, ...models);
}

// hideRequestsSessionSummary clears the session aggregate strip (request
// logging off or a failed query must not leave a stale summary behind).
function hideRequestsSessionSummary() {
  const host = document.getElementById('req-session-summary');
  if (host) { host.hidden = true; host.innerHTML = ''; }
}

// renderRequestsSessionSummary shows the selected session's aggregate above the
// request table (same chips as the Live session panel). Hidden when no session
// is selected; the aggregate comes from /api/sessions (combos.sessions) and the
// displayed rows fill in when the aggregate is missing (aged-out session).
function renderRequestsSessionSummary(combos) {
  const host = document.getElementById('req-session-summary');
  if (!host) return;
  if (!requestsFilter.session) { host.hidden = true; host.innerHTML = ''; return; }
  const agg = (combos.sessions || []).find((s) => s.session_id === requestsFilter.session) || null;
  const s = liveSessionSummary((combos.lastRecords || []).map(persistedSummaryRow), agg);
  host.hidden = false;
  host.innerHTML = sessionSummaryHTML(s, { live: false });
}

async function loadRequests(combos) {
  const tbl = document.getElementById('req-table');
  // Rebuilding the table drops every open detail row (an in-flight fetch
  // checks row.isConnected before filling); drop their chunk state too.
  if (tbl) {
    tbl.querySelectorAll('[data-chunk]').forEach((host) => bodyChunkRegistry.delete(host.dataset.chunk));
    tbl.innerHTML = '<span class="hint">loading…</span>';
  }
  const q = new URLSearchParams();
  if (requestsFilter.session) q.set('session', requestsFilter.session);
  if (requestsFilter.model) q.set('model', requestsFilter.model);
  if (requestsFilter.provider) q.set('provider', requestsFilter.provider);
  if (requestsFilter.errors) q.set('errors', '1');
  if (requestsFilter.shadow) q.set('shadow', requestsFilter.shadow);
  // A session view is a focused drill-down, so pull more of it (still under
  // the backend's 1000 cap) than the default browse window.
  q.set('limit', requestsFilter.session ? '500' : '200');
  let resp;
  try {
    resp = await apiGet('/api/requests?' + q.toString());
  } catch (e) {
    if (tbl) tbl.innerHTML = `<div class="msg err">${esc(e.message)}</div>`;
    hideRequestsSessionSummary();
    return;
  }
  if (!resp.enabled) {
    if (tbl) tbl.innerHTML = '<div class="msg hint">Request logging is off. Enable <code>request_log.enabled</code> in config to capture request/response bodies for replay and debugging.</div>';
    hideRequestsSessionSummary();
    return;
  }
  // Facets come from the scanned log window (data-driven), so refresh the
  // dropdowns even when the current filter matches nothing.
  syncRequestFacets(resp.facets, combos);
  const recs = resp.records || [];
  combos.lastRecords = recs;
  renderRequestsSessionSummary(combos);
  if (!recs.length) {
    if (tbl) tbl.innerHTML = '<div class="msg hint">No matching requests.</div>';
    return;
  }
  let rows = '';
  for (const r of recs) {
    rows += `<tr class="req-row" data-id="${esc(r.request_id)}">
      <td class="mono">${esc(fmtTime(r.ts))}</td>
      <td class="num ${r.status >= 400 ? 'err' : ''}">${r.status}</td>
      <td>${esc(r.exposed || r.called_model)}</td>
      <td class="mono">${esc(r.provider)}${r.shadow ? ' <span class="badge muted">shadow</span>' : ''}</td>
      <td class="num">${fmtNum(r.latency_ms)}</td>
      <td class="num">${fmtNum(r.request_size)}</td>
      <td class="num">${fmtNum(r.response_size)}</td>
    </tr>`;
  }
  if (tbl) tbl.innerHTML = `<table class="table">
    <thead><tr><th>time</th><th>status</th><th>model</th><th>provider</th>
    <th class="num">ms</th><th class="num">req bytes</th><th class="num">resp bytes</th></tr></thead>
    <tbody>${rows}</tbody></table>`;
  document.querySelectorAll('.req-row').forEach((tr) => {
    tr.onclick = () => toggleRequestDetail(tr);
  });
}

// requestsDetailCache holds the fetched records by request_id so re-opening a
// record skips the (slow) log rescan. Records are immutable once written; the
// cache is bounded and in-page. Raw records are cached (not rendered HTML)
// because the body renderer is stateful (chunked scroll-load).
const REQUESTS_DETAIL_CACHE_MAX = 10;
const requestsDetailCache = new Map();

function cacheRequestDetail(id, recs) {
  requestsDetailCache.set(id, recs);
  while (requestsDetailCache.size > REQUESTS_DETAIL_CACHE_MAX) {
    requestsDetailCache.delete(requestsDetailCache.keys().next().value);
  }
}

// detailRecordsHTML renders the expanded detail for one request's records. Each
// body is built lazily/bounded by capturedBodyView, so a multi-MB record does
// not dump megabytes of DOM at once.
function detailRecordsHTML(recs) {
  wireBodyChunks();
  let html = '';
  for (const r of recs) {
    const req = capturedBodyView(r.request_body, '', 'request');
    const res = capturedBodyView(r.response_body, responseContentType(r), 'response');
    html += `<div class="req-rec">
      <div class="hint">${esc(r.ts)} · ${esc(r.method)} ${esc(r.path)} · attempt ${r.attempt} · ${r.status} · ${r.latency_ms}ms · ${esc(r.provider)}/${esc(r.upstream_model)}</div>
      <details><summary>request body (${fmtNum(r.request_size)} bytes · ${esc(req.label)})</summary>${req.html}</details>
      <details><summary>response body (${fmtNum(r.response_size)} bytes · ${esc(res.label)})</summary>${res.html}</details>
    </div>`;
  }
  return html;
}

// toggleRequestDetail expands/collapses the full record under a summary row.
// Multiple rows can stay open at once: clicking a row toggles only that row's
// detail and never closes another row's. An in-flight fetch fills its own row
// only if that row is still connected, so closing/re-opening while loading
// cannot cross wires.
async function toggleRequestDetail(tr) {
  if (tr.classList.contains('req-open')) {
    closeDetailFor(tr);
    return;
  }
  const id = tr.dataset.id;
  tr.classList.add('req-open');
  const row = document.createElement('tr');
  row.className = 'req-detail-row';
  row.innerHTML = '<td colspan="7"><span class="hint">loading…</span></td>';
  tr.insertAdjacentElement('afterend', row);
  const cached = requestsDetailCache.get(id);
  if (cached) {
    row.firstElementChild.innerHTML = detailRecordsHTML(cached);
    return;
  }
  let resp;
  try {
    resp = await apiGet('/api/requests/' + encodeURIComponent(id));
  } catch (e) {
    if (!row.isConnected) return;
    row.firstElementChild.innerHTML = (e && e.status === 404)
      ? '<div class="msg hint">not logged — the request did not commit, so there is no request-log record</div>'
      : `<div class="msg err">${esc(e.message)}</div>`;
    return;
  }
  if (!row.isConnected) return; // closed, or the table was rebuilt while loading
  const recs = resp.records || [];
  if (!recs.length) {
    row.firstElementChild.innerHTML = '<div class="msg hint">no record</div>';
    return;
  }
  row.firstElementChild.innerHTML = detailRecordsHTML(recs);
  cacheRequestDetail(id, recs);
}

// closeDetailFor removes the detail row anchored to `tr` (its next sibling) and
// drops that row's chunk state so it cannot leak.
function closeDetailFor(tr) {
  tr.classList.remove('req-open');
  const row = tr.nextElementSibling;
  if (!row || !row.classList.contains('req-detail-row')) return;
  row.querySelectorAll('[data-chunk]').forEach((host) => bodyChunkRegistry.delete(host.dataset.chunk));
  row.remove();
}

// BODY_CHUNK_CHARS bounds how much of a large body enters the DOM at once.
// Reaching the bottom of the scroll box appends the next chunk (no button).
const BODY_CHUNK_CHARS = 64 * 1024;
// BODY_LONG_LINE collapses a single over-long line — a pasted file, a base64
// blob, a multi-MB JSON string value — into an expandable block. It sits well
// above ordinary long prose (a 1–3 KB thinking/text block) so reading a
// conversation by scrolling still works; only genuinely huge values collapse.
const BODY_LONG_LINE = 4000;
const bodyChunkRegistry = new Map();
let bodyChunkSeq = 0;
let bodyChunkWired = false;

// wireBodyChunks installs one capture-phase scroll listener (scroll does not
// bubble) that appends the next chunk when a chunked body reaches its bottom.
function wireBodyChunks() {
  if (bodyChunkWired) return;
  bodyChunkWired = true;
  document.addEventListener('scroll', (event) => {
    const host = event.target;
    if (!host || !host.dataset || !host.dataset.chunk) return;
    if (host.scrollTop + host.clientHeight < host.scrollHeight - 80) return;
    appendNextChunk(host);
  }, true);
}

// appendNextChunk appends the following chunk of a scroll-loaded body and drops
// the chunk state once the last one is in the DOM. A chunk whose content is
// entirely collapsed (over-long lines render as closed <details>) adds almost
// no height, so keep appending while the host still has no scroll room —
// otherwise the scrollbar would already sit at the bottom and the user could
// never trigger the remaining chunks.
function appendNextChunk(host) {
  const id = host.dataset.chunk;
  const state = bodyChunkRegistry.get(id);
  if (!state) return;
  // Bounded so a pathological body cannot stall the main thread: every pass
  // either consumes a chunk or returns once the host has scroll room again.
  for (let guard = 0; guard < 64; guard += 1) {
    state.index += 1;
    const chunk = state.chunks[state.index];
    if (chunk === undefined) {
      bodyChunkRegistry.delete(id);
      delete host.dataset.chunk;
      return;
    }
    host.insertAdjacentHTML('beforeend', state.render(chunk));
    if (state.chunks[state.index + 1] === undefined) {
      bodyChunkRegistry.delete(id);
      delete host.dataset.chunk;
      return;
    }
    if (host.scrollHeight > host.clientHeight + 80) return;
  }
}

// longLineHTML renders one over-long line as a collapsed <details>: a short
// escaped preview in the summary, the full (wrapped) value on expand. Returns
// '' for a normal line so callers can fall back to their own rendering.
function longLineHTML(line) {
  if (line.length <= BODY_LONG_LINE) return '';
  return `<details class="json-long"><summary>${esc(line.slice(0, 100))}… <span class="json-long-meta">${fmtNum(line.length)} chars</span></summary><pre class="json-long-body">${esc(line)}</pre></details>`;
}

// jsonLinesHTML renders pretty JSON line by line: normal lines get syntax
// highlighting, over-long lines collapse into an expandable block.
function jsonLinesHTML(text) {
  return text.split('\n').map((line) => longLineHTML(line) || highlightJSON(line)).join('\n');
}

// plainLinesHTML is jsonLinesHTML without highlighting (non-JSON bodies).
function plainLinesHTML(text) {
  return text.split('\n').map((line) => longLineHTML(line) || esc(line)).join('\n');
}

// chunkedBodyHTML renders `text` with `render` in whole-line chunks: the first
// chunk in the DOM, the rest appended as the user scrolls to the bottom.
function chunkedBodyHTML(text, render, cls) {
  const chunks = splitLinesByBudget(text, BODY_CHUNK_CHARS);
  if (chunks.length <= 1) return `<div class="log-pre body-pre ${cls}">${render(text)}</div>`;
  const id = `body-chunk-${++bodyChunkSeq}`;
  bodyChunkRegistry.set(id, { chunks, index: 0, render });
  return `<div class="log-pre body-pre ${cls}" data-chunk="${id}">${render(chunks[0])}</div>`;
}

// BODY_RENDER_MAX caps pretty-printing: beyond it the detail view falls back
// to the raw pre. Coding-agent contexts routinely reach a few MB, so the
// formatting ceiling sits above the default 5 MiB max_body_bytes.
const BODY_RENDER_MAX = 8 * 1024 * 1024;
// BODY_HIGHLIGHT_MAX caps syntax highlighting. A multi-MB body is still
// pretty-printed, but skipping the per-token <span> keeps the DOM small.
const BODY_HIGHLIGHT_MAX = 256 * 1024;
// BODY_LINE_MAX caps per-line rendering; a longer body falls back to the
// chunked viewer.
const BODY_LINE_MAX = 5000;

// responseContentType extracts content-type from the record's allowlisted
// response_headers JSON ({"content-type":…}); empty when absent/unparseable.
function responseContentType(record) {
  if (!record || !record.response_headers) return '';
  try {
    const headers = JSON.parse(record.response_headers);
    return String((headers && (headers['content-type'] || headers['Content-Type'])) || '');
  } catch (_) {
    return '';
  }
}

// capturedBodyView renders one captured body and returns {label, html}. The
// label feeds the <details> summary; the html is already escaped/safe:
//   - request JSON object/array → pretty-printed, syntax-highlighted, with
//     over-long string values collapsed into expandable blocks
//   - response (SSE stream or JSON) → line-based raw view (one row per source
//     line), because pretty-printing a stream made the detail unreasonably tall
//   - anything else (or over the size cap) → plain <pre>
function capturedBodyView(text, contentType, kind) {
  if (!text) return { label: 'empty', html: '<div class="hint">(empty body)</div>' };
  if (text.length > BODY_RENDER_MAX) {
    return {
      label: 'raw',
      html: `<div class="hint">body over ${fmtNum(BODY_RENDER_MAX)} bytes — showing raw</div><pre class="log-pre body-pre">${esc(text)}</pre>`,
    };
  }
  if (kind === 'response') {
    if (isSSE(text, contentType)) {
      const label = text.length <= BODY_HIGHLIGHT_MAX ? `SSE · ${fmtNum(parseSSE(text).length)} events` : 'SSE';
      return { label, html: bodyLinesHTML(text) };
    }
    return { label: 'text', html: bodyLinesHTML(text) };
  }
  const pretty = prettyJSON(text);
  if (pretty !== null) {
    // Highlight only small bodies: per-line spans on a multi-MB body would
    // accumulate tens of thousands of nodes as the user scrolls. Over the cap
    // the JSON is still pretty-printed and long values still collapse.
    const highlight = pretty.length <= BODY_HIGHLIGHT_MAX;
    return { label: 'JSON', html: chunkedBodyHTML(pretty, highlight ? jsonLinesHTML : plainLinesHTML, highlight ? 'code-json' : '') };
  }
  return { label: 'text', html: bodyLinesHTML(text) };
}

// bodyLinesHTML renders captured text one source line per row in the log
// gutter style (line numbers), so a streamed response stays compact instead
// of expanding into pretty-printed indentation. Over-long lines collapse, and
// very long bodies fall back to the chunked viewer.
function bodyLinesHTML(text) {
  const lines = text.split('\n');
  if (lines.length > BODY_LINE_MAX) return chunkedBodyHTML(text, plainLinesHTML, '');
  return `<div class="log-pre">${lines.map((line) => longLineHTML(line) || `<span class="log-line">${esc(line)}</span>`).join('')}</div>`;
}

// ---------- Security tab (guard audit log) ----------

// Per-tab filter state (kind only). Persists across re-renders within a
// session so a refresh keeps the view.
let securityFilter = { kind: '' };
let securityReqSeq = 0;

// fmtMs renders a unix-millisecond audit timestamp as "MM-DD HH:MM:SS" —
// the audit log spans days (30d retention), so a time-only format is wrong.
function fmtMs(ms) {
  if (!ms) return '—';
  const d = new Date(Number(ms));
  if (isNaN(d.getTime())) return '—';
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${d.toLocaleTimeString('en-US', { hour12: false })}`;
}

// renderSecurityTab builds the guard audit-log view: a kind filter + Refresh
// button and a table of audit records fetched from /api/security. On-demand
// (no poll) — fetch happens on tab entry and on Refresh, like Requests.
// Records carry pattern/path NAMES and the action only; matched content never
// reaches the API, so every field is safe to render verbatim.
async function renderSecurityTab() {
  const panel = panels.security;
  if (!panel) return;
  panel.innerHTML = `<div class="card"><div class="card-body">
    <div class="req-controls" style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-bottom:12px;">
      <select id="sec-kind" class="req-input">
        <option value="" ${securityFilter.kind === '' ? 'selected' : ''}>all kinds</option>
        <option value="secret" ${securityFilter.kind === 'secret' ? 'selected' : ''}>secret</option>
        <option value="path" ${securityFilter.kind === 'path' ? 'selected' : ''}>path</option>
        <option value="drift" ${securityFilter.kind === 'drift' ? 'selected' : ''}>drift</option>
      </select>
      <button id="sec-refresh" class="btn">Refresh</button>
    </div>
    <div id="sec-table"></div>
  </div></div>`;
  const refresh = () => {
    securityFilter.kind = document.getElementById('sec-kind').value;
    loadSecurity();
  };
  document.getElementById('sec-refresh').onclick = refresh;
  document.getElementById('sec-kind').onchange = refresh;
  loadSecurity();
}

async function loadSecurity() {
  const tbl = document.getElementById('sec-table');
  if (tbl) tbl.innerHTML = '<span class="hint">loading…</span>';
  const q = new URLSearchParams();
  if (securityFilter.kind) q.set('kind', securityFilter.kind);
  q.set('limit', '200');
  // Sequence guard: switching kind mid-flight races two fetches; a slow
  // older response must not overwrite the newer one's rendering.
  const seq = ++securityReqSeq;
  let resp;
  try {
    resp = await apiGet('/api/security?' + q.toString());
  } catch (e) {
    if (seq === securityReqSeq && tbl) tbl.innerHTML = `<div class="msg err">${esc(e.message)}</div>`;
    return;
  }
  if (seq !== securityReqSeq) return;
  if (!resp.enabled) {
    if (tbl) tbl.innerHTML = '<div class="msg hint">Security audit is off. Enable <code>guard.audit</code> in config to persist guard hits (secret / path / drift) to the audit log.</div>';
    return;
  }
  const recs = resp.records || [];
  const skipped = resp.skipped > 0 ? `<div class="hint" style="margin-bottom:8px;">skipped ${fmtNum(resp.skipped)} unreadable line(s) while scanning</div>` : '';
  if (!recs.length) {
    if (tbl) tbl.innerHTML = skipped + '<div class="msg hint">No matching audit records.</div>';
    return;
  }
  const kindBadge = { secret: 'warn', path: '', drift: 'muted' };
  let rows = '';
  for (const r of recs) {
    rows += `<tr>
      <td class="mono">${esc(fmtMs(r.ts))}</td>
      <td><span class="badge ${kindBadge[r.kind] || ''}">${esc(r.kind)}</span></td>
      <td class="mono">${esc(r.agent || '—')}</td>
      <td class="mono">${esc(r.exposed || '—')}</td>
      <td class="mono">${esc((r.names || []).join(', ') || '—')}</td>
      <td>${esc(r.action || '—')}</td>
      <td class="subdue">${esc(r.detail || '')}</td>
    </tr>`;
  }
  if (tbl) tbl.innerHTML = skipped + `<table class="table">
    <thead><tr><th>time</th><th>kind</th><th>agent</th><th>route</th>
    <th>names</th><th>action</th><th>detail</th></tr></thead>
    <tbody>${rows}</tbody></table>`;
}

// ---------- Live monitor (Status → Live section, SSE /api/events) ----------

let liveES = null;       // the EventSource for /api/events (null when not connected)
let liveActive = false;  // the section's card is mounted and connected
let liveRows = [];       // newest-first ring of merged request rows (capped)
let liveByReq = {};      // request_id -> row object (while in the ring)
let liveOpenIds = new Set();     // request_ids whose detail row is expanded
let liveDetailState = new Map(); // request_id -> {loading, error} (records live in requestsDetailCache)
let livePendingGuards = new Map(); // request_id -> [{ts, type, detail}] for hits that arrived before start
let liveEventSeq = 0;    // synthetic key counter for standalone event-only rows

// Session mode (Live card): selecting a session replaces the live table with a
// per-session analysis. Rows merge the persisted request log (/api/requests?
// session=) with the live event rows; token/cost totals come from the
// /api/sessions aggregate (persisted rows carry no tokens).
let liveSessionFilter = '';   // selected session id ('' = live mode)
let liveSessionRecords = [];  // persisted Summary rows for the selected session
let liveSessionAgg = null;    // persisted SessionSummary for the selected session
let liveSessionList = [];     // recent SessionSummary list (dropdown options)
let liveSessionLoading = false;
let liveSessionError = '';
const liveSessionOpenIds = new Set();
let liveSessionOptionsKey = '';

// renderLiveCard mounts the live request monitor into the Status Live section
// and opens the SSE connection (closed by stopLiveEvents when the section or
// tab is left). Rows are merged per request: a start event opens a dimmed
// in-flight row; guard hits attach a ⚑ badge and accumulate in the row so the
// expanded detail can list every hit; the end event fills in
// provider/status/latency/tokens. Clicking a request row toggles the full detail
// (fetched from /api/requests/<id> once the request ends). Non-request events
// without a known request id (budget & friends) still render as standalone
// one-line rows. The All/live table is updated incrementally on each SSE event;
// the session panel keeps its full re-render.
function renderLiveCard(target) {
  stopLiveEvents();
  liveRows = [];
  liveByReq = {};
  liveOpenIds.clear();
  liveDetailState.clear();
  livePendingGuards.clear();
  liveEventSeq = 0;
  liveSessionFilter = '';
  liveSessionRecords = [];
  liveSessionAgg = null;
  liveSessionList = [];
  liveSessionLoading = false;
  liveSessionError = '';
  liveSessionOpenIds.clear();
  liveSessionOptionsKey = '';
  target.insertAdjacentHTML('beforeend', buildCard('Live requests', '',
    `<div class="live-toolbar">
       <label class="hint" for="live-session">session</label>
       <select id="live-session" class="req-input"><option value="">all (live)</option></select>
     </div>
     <div id="live-table"><span class="msg hint">connecting…</span></div>
     <div id="live-session-panel" hidden></div>`, 'tight'));
  const sel = document.getElementById('live-session');
  if (sel) sel.onchange = () => onLiveSessionChange(sel.value);
  // Preload recent persisted sessions so the dropdown lists them even before
  // the first live event (best-effort: request logging may be off).
  apiGet('/api/sessions?limit=200').then((resp) => {
    liveSessionList = (resp && resp.sessions) || [];
    refreshLiveSessionOptions();
  }).catch(() => { /* request logging off / unavailable */ });
  liveActive = true;
  try {
    liveES = new EventSource('/api/events');
  } catch (e) {
    liveActive = false;
    const t = document.getElementById('live-table');
    if (t) t.innerHTML = '<span class="msg err">SSE unsupported by this browser.</span>';
    return;
  }
  liveES.onopen = () => {
    const t = document.getElementById('live-table');
    if (t && !liveRows.length) t.innerHTML = '<span class="msg hint">Waiting for requests…</span>';
  };
  liveES.onerror = () => {
    const t = document.getElementById('live-table');
    if (t && !liveRows.length) t.innerHTML = '<span class="msg hint">reconnecting…</span>';
  };
  liveES.onmessage = (m) => {
    let e;
    try { e = JSON.parse(m.data); } catch (_) { return; }
    applyLiveEvent(e);
    applyLiveEventDOM(e);
  };
}

// refreshLiveSessionOptions rebuilds the session dropdown from live rows plus
// the persisted session list. Rebuilds only when the option set changes so a
// busy stream does not reset the control on every event.
function refreshLiveSessionOptions() {
  const sel = document.getElementById('live-session');
  if (!sel) return;
  const ids = new Set();
  for (const r of liveRows) if (r.session) ids.add(r.session);
  for (const s of liveSessionList) if (s.session_id) ids.add(s.session_id);
  const sorted = [...ids].sort();
  const key = sorted.join('\n');
  if (key === liveSessionOptionsKey) return;
  liveSessionOptionsKey = key;
  sel.innerHTML = '<option value="">all (live)</option>' +
    sorted.map((id) => `<option value="${esc(id)}">${esc(liveSessionLabel(id))}</option>`).join('');
  sel.value = liveSessionFilter;
}

// liveSessionLabel shortens a session UUID for the dropdown; the option value
// keeps the full id.
function liveSessionLabel(id) {
  return id.length > 14 ? id.slice(0, 8) + '…' + id.slice(-4) : id;
}

// onLiveSessionChange switches between the live table and one session's
// analysis, loading the persisted request list + aggregate for the selection.
function onLiveSessionChange(value) {
  liveSessionFilter = value;
  liveSessionRecords = [];
  liveSessionAgg = null;
  liveSessionError = '';
  liveSessionOpenIds.clear();
  const tbl = document.getElementById('live-table');
  const panel = document.getElementById('live-session-panel');
  if (!value) {
    // All (live) mode: the pure live ring, no persisted backfill.
    if (tbl) tbl.hidden = false;
    if (panel) { panel.hidden = true; panel.innerHTML = ''; }
    liveSessionLoading = false;
    renderLiveTable();
    return;
  }
  if (tbl) tbl.hidden = true;
  if (panel) panel.hidden = false;
  liveSessionLoading = true;
  renderLiveSessionPanel();
  Promise.all([
    apiGet('/api/requests?session=' + encodeURIComponent(value) + '&limit=500')
      .then((r) => ({ recs: (r && r.records) || [] }))
      .catch((e) => ({ err: e.message })),
    apiGet('/api/sessions?limit=200').then((r) => (r && r.sessions) || []).catch(() => []),
  ]).then(([reqs, sessions]) => {
    if (liveSessionFilter !== value) return; // switched away while loading
    liveSessionLoading = false;
    if (reqs.err) liveSessionError = reqs.err;
    else liveSessionRecords = reqs.recs;
    liveSessionList = sessions;
    liveSessionAgg = sessions.find((s) => s.session_id === value) || null;
    refreshLiveSessionOptions();
    renderLiveSessionPanel();
  });
}

// persistedSummaryRow projects a /api/requests Summary (snake_case) into the
// camelCase row shape shared with live event rows (latencyMs/cacheRead/
// cacheCreation), so liveSessionSummary folds persisted rows correctly even
// without an /api/sessions aggregate.
function persistedSummaryRow(rec) {
  return {
    requestId: rec.request_id,
    ts: rec.ts,
    session: rec.session_id,
    agent: rec.agent || '',
    model: rec.exposed || rec.upstream_model || rec.called_model || '',
    provider: rec.provider || '',
    status: rec.status || 0,
    latencyMs: rec.latency_ms != null ? rec.latency_ms : null,
    input: rec.input || 0,
    output: rec.output || 0,
    cacheRead: rec.cache_read || 0,
    cacheCreation: rec.cache_creation || 0,
    inFlight: false,
    guardHits: [],
    progressText: '', progressBytes: 0,
    persisted: true,
  };
}

// liveSessionRows merges the persisted request summaries with the live event
// rows for the selected session. Live wins on in-flight state,
// status, latency, provider, and model; persisted fills missing agent and
// tokens. Newest first.
function liveSessionRows() {
  const byId = new Map();
  for (const rec of liveSessionRecords) {
    byId.set(rec.request_id, persistedSummaryRow(rec));
  }
  for (const r of liveRows) {
    if (r.session !== liveSessionFilter) continue;
    const persisted = byId.get(r.requestId);
    byId.set(r.requestId, mergeLiveAndPersistedRow(r, persisted || {
      requestId: r.requestId, ts: r.ts, session: r.session,
      agent: '', model: '', provider: '', status: 0, latencyMs: null,
      input: 0, output: 0, cacheRead: 0, cacheCreation: 0,
      inFlight: false, guardHits: [],
      progressText: '', progressBytes: 0,
    }));
  }
  return [...byId.values()].sort((a, b) => liveTsMs(b.ts) - liveTsMs(a.ts));
}

// liveTsMs normalizes a live event timestamp (unix ms) or a persisted record
// timestamp (RFC3339) to a comparable millisecond value.
function liveTsMs(ts) {
  const n = typeof ts === 'number' ? ts : Date.parse(ts);
  return Number.isFinite(n) ? n : 0;
}

// sessionSummaryHTML renders the shared session chips + providers/models lines
// used by the Live session panel and the Requests session filter. `opts.live`
// false omits the trailing live-row count (Requests rows are all persisted).
function sessionSummaryHTML(s, opts) {
  const showLive = !opts || opts.live !== false;
  const chips = [
    `${fmtNum(s.requests)} requests`,
    `${fmtNum(s.input)} in / ${fmtNum(s.output)} out`,
    (s.cacheRead || s.cacheCreation) ? `${fmtNum(s.cacheRead)} cache-read · ${fmtNum(s.cacheCreation)} cache-write` : '',
    s.avgLatencyMs != null ? `avg ${s.avgLatencyMs}ms` : '',
    s.errors ? `${fmtNum(s.errors)} errors` : '',
    s.cost != null ? '$' + s.cost.toFixed(4) : '',
    showLive ? `${fmtNum(s.liveRows)} live` : '',
  ].filter(Boolean).map((c) => `<span class="live-chip">${esc(c)}</span>`).join('');
  const meta = [
    s.providers.length ? 'providers: ' + s.providers.join(', ') : '',
    s.models.length ? 'models: ' + s.models.join(', ') : '',
  ].filter(Boolean).map((line) => `<div class="hint">${esc(line)}</div>`).join('');
  return `<div class="live-session-summary">${chips}</div>${meta}`;
}

// renderLiveSessionPanel renders the selected session's analysis: summary
// chips + distinct models/providers, then the merged request table with the
// existing click-to-expand detail.
function renderLiveSessionPanel() {
  const panel = document.getElementById('live-session-panel');
  if (!panel || !liveSessionFilter) return;
  if (liveSessionLoading) { panel.innerHTML = '<span class="hint">loading session…</span>'; return; }
  if (liveSessionError) { panel.innerHTML = `<div class="msg err">${esc(liveSessionError)}</div>`; return; }
  const rows = liveSessionRows();
  const s = liveSessionSummary(rows, liveSessionAgg);
  const body = rows.length
    ? `<table class="table"><thead><tr>
         <th>time</th><th>agent</th><th>model</th><th>provider</th>
         <th>status</th><th class="num">latency</th><th class="num">tokens in / out</th>
       </tr></thead><tbody>${rows.map((r) => {
         const open = liveSessionOpenIds.has(r.requestId);
         const sc = r.status >= 400 ? 'err' : '';
         const lt = r.latencyMs != null ? r.latencyMs + 'ms' : '';
         const tk = (r.input || r.output) ? `${fmtNum(r.input)} / ${fmtNum(r.output)}` : '';
         const detail = open ? renderLiveDetailRow(r) : '';
         return `<tr class="live-row${open ? ' live-open' : ''}" data-id="${esc(r.requestId)}" data-live-key="${esc(r.requestId)}">
           <td class="mono">${esc(fmtTimeSafe(r.ts))}</td>
           <td class="mono">${esc(r.agent || '—')}</td>
           <td>${esc(r.model || '—')}</td>
           <td class="mono">${esc(r.provider || '—')}</td>
           <td class="num ${sc}">${esc(String(r.status || '—'))}</td>
           <td class="num">${esc(lt)}</td>
           <td class="num">${esc(tk)}</td>
         </tr>${detail}`;
       }).join('')}</tbody></table>`
    : '<div class="msg hint">no requests recorded for this session yet</div>';
  panel.innerHTML = `${sessionSummaryHTML(s)}${body}`;
  panel.querySelectorAll('.live-row').forEach((tr) => {
    tr.onclick = () => toggleLiveSessionRow(tr.dataset.id);
  });
  for (const id of liveSessionOpenIds) ensureLiveSessionDetail(id);
}

// toggleLiveSessionRow expands/collapses one request's detail in the session
// panel.
function toggleLiveSessionRow(id) {
  if (liveSessionOpenIds.has(id)) {
    liveSessionOpenIds.delete(id);
  } else {
    liveSessionOpenIds.add(id);
    // Explicit re-open clears a recorded fetch error so it can be retried.
    const state = liveDetailState.get(id) || {};
    if (state.error || state.notLogged) liveDetailState.set(id, { ...state, error: '', notLogged: false });
    ensureLiveSessionDetail(id);
  }
  renderLiveSessionPanel();
}

// ensureLiveSessionDetail loads /api/requests/<id> once per open row.
function ensureLiveSessionDetail(id) {
  if (!shouldFetchDetail(requestsDetailCache.has(id), liveDetailState.get(id), false)) return;
  const state = liveDetailState.get(id) || {};
  liveDetailState.set(id, { ...state, loading: true, error: '' });
  fetchLiveSessionDetail(id);
}

async function fetchLiveSessionDetail(id) {
  try {
    const resp = await apiGet('/api/requests/' + encodeURIComponent(id));
    cacheRequestDetail(id, resp.records || []);
    liveDetailState.set(id, { loading: false, error: '' });
  } catch (e) {
    liveDetailState.set(id, detailFetchState(e.status, e.message));
  }
  if (liveSessionOpenIds.has(id)) renderLiveSessionPanel();
}

function stopLiveEvents() {
  if (liveES) {
    liveES.close();
    liveES = null;
  }
  liveActive = false;
}

// applyLiveEvent folds one SSE event into the merged row set (liveRows /
// liveByReq). It does not touch the DOM; the caller applies the corresponding
// DOM update via applyLiveEventDOM, which performs targeted surgery in
// All/live mode and falls back to renderLiveTable for the session panel.
function applyLiveEvent(e) {
  if (e.type === 'start') {
    liveByReq[e.request_id] = {
      requestId: e.request_id,
      ts: e.ts, session: e.session_id || '', agent: e.agent, model: e.exposed || '—',
      provider: '', status: 0, latencyMs: null, input: 0, output: 0,
      inFlight: true, guardHits: popPendingGuards(e.request_id),
      progressText: '', progressBytes: 0,
    };
    liveRows.unshift(liveByReq[e.request_id]);
    trimLiveRows();
    return;
  }
  if (e.type === 'end') {
    // An end without a start (ring trimmed or missed start): synthesize the row.
    const row = liveByReq[e.request_id] || synthLiveRow(e);
    row.ts = e.ts;
    if (!row.session && e.session_id) row.session = e.session_id;
    row.provider = e.provider || '—';
    row.status = e.status || 0;
    row.latencyMs = e.latency_ms;
    row.input = e.input || 0;
    row.output = e.output || 0;
    row.inFlight = false;
    return;
  }
  if (e.type === 'progress') {
    if (e.request_id && liveByReq[e.request_id]) {
      const row = liveByReq[e.request_id];
      row.progressText = e.text || '';
      row.progressBytes = e.received_bytes || 0;
    }
    return;
  }
  // Non-lifecycle event (guard/budget/…): attach to the request's row when the
  // id is known, else queue it so a later start/end can claim it.
  const hit = { ts: e.ts, type: e.type, detail: e.detail || '' };
  if (e.request_id && liveByReq[e.request_id]) {
    liveByReq[e.request_id].guardHits.push(hit);
  } else if (e.request_id) {
    const pending = livePendingGuards.get(e.request_id) || [];
    pending.push(hit);
    livePendingGuards.set(e.request_id, pending);
  } else {
    liveRows.unshift({
      ts: e.ts, agent: e.agent, model: e.exposed || '',
      eventOnly: true, guardDetail: (e.type || '') + ' ' + (e.detail || ''),
      key: 'ev-' + (liveEventSeq++),
    });
    trimLiveRows();
  }
}

// popPendingGuards returns and clears any guard hits that arrived before the
// request's start event, so they still attach to the correct row.
function popPendingGuards(id) {
  const hits = livePendingGuards.get(id) || [];
  livePendingGuards.delete(id);
  return hits;
}

// synthLiveRow back-fills a ring entry for an end event whose start row was
// already trimmed, keeping the by-id index consistent.
function synthLiveRow(e) {
  const row = {
    requestId: e.request_id,
    ts: e.ts, session: e.session_id || '', agent: e.agent, model: e.exposed || '—',
    provider: '', status: 0, latencyMs: null, input: 0, output: 0,
    inFlight: false, guardHits: popPendingGuards(e.request_id),
    progressText: '', progressBytes: 0,
  };
  liveByReq[e.request_id] = row;
  liveRows.unshift(row);
  trimLiveRows();
  return row;
}

function trimLiveRows() {
  if (liveRows.length <= 100) return;
  const excess = liveRows.length - 100;
  liveRows.length = 100;
  // Rebuild the id index from what survives the ring.
  liveByReq = {};
  for (const r of liveRows) {
    if (r.requestId) liveByReq[r.requestId] = r;
  }
  // Remove surplus DOM rows from the end of the tbody. Oldest rows live at the
  // end because new rows are prepended; each summary may be followed by its
  // detail row.
  const tbl = document.getElementById('live-table');
  const tbody = tbl && tbl.querySelector('tbody');
  if (!tbody) return;
  let removed = 0;
  let node = tbody.lastElementChild;
  while (node && removed < excess) {
    const prev = node.previousElementSibling;
    if (node.classList.contains('live-detail-row')) {
      node.remove();
    } else {
      const id = node.dataset.id;
      if (id && liveOpenIds.has(id)) {
        liveOpenIds.delete(id);
        liveDetailState.delete(id);
      }
      const detail = node.nextElementSibling;
      if (detail && detail.classList.contains('live-detail-row')) detail.remove();
      node.remove();
      removed++;
    }
    node = prev;
  }
}

// liveSummaryRowHTML returns the summary <tr> for one live request row. `open`
// is whether the detail is currently expanded; the caller decides based on
// liveOpenIds.
function liveSummaryRowHTML(r, open) {
  const dim = r.inFlight ? ' subdue' : '';
  const openCls = open ? ' live-open' : '';
  const guardCount = r.guardHits.length;
  const guardTitle = guardCount
    ? esc(r.guardHits.map((h) => fmtGuardDetail(h.detail)).join('\n'))
    : '';
  const guard = guardCount
    ? ` <span class="badge warn" title="${guardTitle}">⚑ guard${guardCount > 1 ? ' ×' + guardCount : ''}</span>`
    : '';
  const sc = r.status >= 400 ? 'err' : (r.inFlight ? 'subdue' : '');
  const status = r.inFlight ? '···' : (r.status || '—');
  const lt = (!r.inFlight && r.latencyMs != null) ? r.latencyMs + 'ms' : '';
  const tk = (!r.inFlight && (r.input || r.output)) ? `${fmtNum(r.input)} / ${fmtNum(r.output)}` : '';
  return `<tr class="live-row${openCls}" data-id="${esc(r.requestId)}" data-live-key="${esc(r.requestId)}">
    <td class="mono${dim}">${esc(fmtTimeSafe(r.ts))}</td>
    <td class="mono${dim}">${esc(r.agent || '—')}</td>
    <td class="${dim ? 'subdue' : ''}">${esc(r.model)}${guard}</td>
    <td class="mono${dim}">${esc(r.inFlight ? '…' : (r.provider || '—'))}</td>
    <td class="num ${sc}">${esc(String(status))}</td>
    <td class="num">${lt}</td>
    <td class="num">${tk}</td>
  </tr>`;
}

// liveEventRowHTML returns a one-line standalone row for non-request events.
function liveEventRowHTML(r) {
  return `<tr data-live-key="${esc(r.key)}">
    <td class="mono">${esc(fmtTimeSafe(r.ts))}</td>
    <td class="mono">${esc(r.agent || '—')}</td>
    <td colspan="5"><span class="badge warn" title="${esc(fmtGuardDetail(r.guardDetail))}">⚑ ${esc(fmtGuardDetail(r.guardDetail) || 'event')}</span></td>
  </tr>`;
}

// liveRowHTML returns {summary, detail} HTML for one live row. The detail string
// is empty when the row is collapsed. Used by full renders and by incremental
// updates that replace a single row in place.
function liveRowHTML(r) {
  if (r.eventOnly) {
    return { summary: liveEventRowHTML(r), detail: '' };
  }
  const open = liveOpenIds.has(r.requestId);
  return {
    summary: liveSummaryRowHTML(r, open),
    detail: open ? renderLiveDetailRow(r) : '',
  };
}

// renderLiveTable redraws the merged rows: one line per request. In-flight
// rows are dimmed with a pending marker; the tokens column reads "in / out".
// This is the full-rebuild path used on tab switches and as a fallback; the
// hot SSE path uses applyLiveEventDOM for targeted surgery. Because new rows
// are PREPENDED, the rebuild preserves the viewport (captureLiveViewState /
// restoreLiveViewState): a visible expanded row is pinned in place, and when
// the page is scrolled away from the top the visible region does not shift;
// open request/response body <details> are re-opened after the rebuild.
function renderLiveTable() {
  refreshLiveSessionOptions();
  if (liveSessionFilter) {
    // Session mode: the live table is hidden; refresh the session panel from
    // the merged live + persisted rows instead.
    renderLiveSessionPanel();
    return;
  }
  const tbl = document.getElementById('live-table');
  if (!tbl) return;
  // All (live) view is the pure live ring (newest 100 events, no persisted
  // backfill); persisted history lives in the session view and Requests tab.
  const rows = liveRows;
  if (!rows.length) {
    tbl.innerHTML = '<span class="msg hint">Waiting for requests…</span>';
    maybeRemoveLiveSpacer(tbl);
    return;
  }
  const viewState = captureLiveViewState(tbl);
  // Drop chunk state for body views that are about to be replaced.
  tbl.querySelectorAll('.req-detail-row [data-chunk]').forEach((host) => {
    bodyChunkRegistry.delete(host.dataset.chunk);
  });
  tbl.innerHTML = `<table class="table"><thead><tr>
    <th>time</th><th>agent</th><th>model</th><th>provider</th>
    <th>status</th><th class="num">latency</th><th class="num">tokens in / out</th></tr></thead>
    <tbody>${rows.map((r) => { const h = liveRowHTML(r); return h.summary + h.detail; }).join('')}</tbody></table>`;
  document.querySelectorAll('#live-table .live-row').forEach((tr) => {
    tr.onclick = () => toggleLiveRowDetail(tr.dataset.id);
  });
  for (const r of rows) {
    if (r.requestId && liveOpenIds.has(r.requestId) && !r.inFlight) {
      ensureLiveDetailFetched(r.requestId);
    }
  }
  if (liveOpenIds.size > 0) maybeAddLiveSpacer(tbl);
  else maybeRemoveLiveSpacer(tbl);
  restoreLiveViewState(tbl, viewState);
}

// captureLiveViewState snapshots, before a full-table rebuild: (1) which
// <details> are open inside expanded rows (keyed "requestId:recIndex:ordinal"
// where ordinal is the element's index among ALL details in its .req-rec —
// the record HTML is deterministic across rebuilds and body chunks only
// append, so ordinals are stable; this covers the request/response body
// containers AND the nested over-long-line blocks), and (2) the scroll
// anchor — a visible expanded row wins (the user is reading it); otherwise
// the first visible row when the page is scrolled away from the top.
// scrollY ≈ 0 keeps the natural "pinned to newest" behavior.
function captureLiveViewState(tbl) {
  const openBodies = new Set();
  tbl.querySelectorAll('.live-detail-row').forEach((row) => {
    let owner = row.previousElementSibling;
    while (owner && !owner.classList.contains('live-row')) owner = owner.previousElementSibling;
    const id = owner && owner.dataset.id;
    if (!id) return;
    row.querySelectorAll('.req-rec').forEach((rec, recIdx) => {
      rec.querySelectorAll('details').forEach((d, dIdx) => {
        if (d.open) openBodies.add(id + ':' + recIdx + ':' + dIdx);
      });
    });
  });
  const vh = window.innerHeight || document.documentElement.clientHeight;
  let anchor = null;
  let firstVisible = null;
  for (const tr of tbl.querySelectorAll('tr[data-live-key]')) {
    const rect = tr.getBoundingClientRect();
    if (rect.bottom <= 0 || rect.top >= vh) continue;
    if (tr.classList.contains('live-open')) {
      anchor = { key: tr.dataset.liveKey, top: rect.top };
      break;
    }
    if (!firstVisible) firstVisible = { key: tr.dataset.liveKey, top: rect.top };
  }
  if (!anchor && firstVisible && window.scrollY > 2) anchor = firstVisible;
  return { anchor, openBodies };
}

// restoreLiveViewState re-applies captureLiveViewState after the rebuild:
// re-open the body <details> first (they change heights), then scroll so the
// anchor row sits exactly where it was. A trimmed-out anchor (ring overflow)
// degrades to no adjustment.
function restoreLiveViewState(tbl, state) {
  if (state.openBodies.size) {
    tbl.querySelectorAll('.live-detail-row').forEach((row) => {
      let owner = row.previousElementSibling;
      while (owner && !owner.classList.contains('live-row')) owner = owner.previousElementSibling;
      const id = owner && owner.dataset.id;
      if (!id) return;
      row.querySelectorAll('.req-rec').forEach((rec, recIdx) => {
        rec.querySelectorAll('details').forEach((d, dIdx) => {
          if (state.openBodies.has(id + ':' + recIdx + ':' + dIdx)) d.open = true;
        });
      });
    });
  }
  if (!state.anchor) return;
  for (const tr of tbl.querySelectorAll('tr[data-live-key]')) {
    if (tr.dataset.liveKey !== state.anchor.key) continue;
    const delta = tr.getBoundingClientRect().top - state.anchor.top;
    if (delta) window.scrollBy(0, delta);
    return;
  }
}

// findLiveSummaryRow locates a request's summary <tr> inside the live tbody.
function findLiveSummaryRow(tbody, id) {
  return tbody.querySelector(`tr.live-row[data-id="${esc(id)}"]`);
}

// updateLiveSummaryRow replaces one summary <tr> in place with its current
// rendering and re-attaches the click handler.
function updateLiveSummaryRow(tr, r) {
  const open = liveOpenIds.has(r.requestId);
  tr.insertAdjacentHTML('beforebegin', liveSummaryRowHTML(r, open));
  const next = tr.previousElementSibling;
  tr.remove();
  next.onclick = () => toggleLiveRowDetail(next.dataset.id);
  return next;
}

// captureLiveDetailOpenBodies snapshots which <details> are open inside one
// detail row, scoped to that row so incremental replacements can preserve them.
function captureLiveDetailOpenBodies(detailRow) {
  const openBodies = new Set();
  detailRow.querySelectorAll('.req-rec').forEach((rec, recIdx) => {
    rec.querySelectorAll('details').forEach((d, dIdx) => {
      if (d.open) openBodies.add(recIdx + ':' + dIdx);
    });
  });
  return openBodies;
}

// restoreLiveDetailOpenBodies re-opens the details captured by
// captureLiveDetailOpenBodies after an in-place detail replacement.
function restoreLiveDetailOpenBodies(detailRow, openBodies) {
  if (!openBodies.size) return;
  detailRow.querySelectorAll('.req-rec').forEach((rec, recIdx) => {
    rec.querySelectorAll('details').forEach((d, dIdx) => {
      if (openBodies.has(recIdx + ':' + dIdx)) d.open = true;
    });
  });
}

// replaceLiveDetailInPlace swaps the content of an existing detail <tr> without
// disturbing its adjacent summary row, preserving open body <details> state.
function replaceLiveDetailInPlace(detailRow, r) {
  detailRow.querySelectorAll('[data-chunk]').forEach((host) => {
    bodyChunkRegistry.delete(host.dataset.chunk);
  });
  const openBodies = captureLiveDetailOpenBodies(detailRow);
  const cell = detailRow.querySelector('td');
  if (cell) cell.innerHTML = liveDetailHTML(r);
  restoreLiveDetailOpenBodies(detailRow, openBodies);
}

// updateLiveDetailForRow inserts, updates, or removes the detail <tr> that
// follows a summary row to match liveOpenIds and the current row state.
function updateLiveDetailForRow(tr, r) {
  const wantDetail = liveOpenIds.has(r.requestId);
  let detail = tr.nextElementSibling;
  const hasDetail = detail && detail.classList.contains('live-detail-row');
  if (!wantDetail) {
    if (hasDetail) detail.remove();
    return;
  }
  if (hasDetail) {
    replaceLiveDetailInPlace(detail, r);
  } else {
    tr.insertAdjacentHTML('afterend', renderLiveDetailRow(r));
  }
  if (!r.inFlight) ensureLiveDetailFetched(r.requestId);
}

// updateLiveResponseSection swaps just the in-flight response area inside an
// expanded detail row, used by progress events that do not touch the summary.
function updateLiveResponseSection(detailCell, r) {
  if (!detailCell) return;
  const section = detailCell.querySelector('.live-response-section');
  if (section) section.outerHTML = liveResponseHTML(r);
}

// captureLiveScrollAnchor picks a visible row to pin during a prepend. An
// expanded row wins; otherwise the first visible row is used only when the
// page is scrolled away from the top (scrollY > 2), so the "pinned to newest"
// behavior at the top is preserved.
function captureLiveScrollAnchor(tbl) {
  if (window.scrollY <= 2) return null;
  const vh = window.innerHeight || document.documentElement.clientHeight;
  let anchor = null;
  let firstVisible = null;
  for (const tr of tbl.querySelectorAll('tr[data-live-key]')) {
    const rect = tr.getBoundingClientRect();
    if (rect.bottom <= 0 || rect.top >= vh) continue;
    if (tr.classList.contains('live-open')) {
      anchor = { key: tr.dataset.liveKey, top: rect.top };
      break;
    }
    if (!firstVisible) firstVisible = { key: tr.dataset.liveKey, top: rect.top };
  }
  return anchor || firstVisible || null;
}

// compensateLiveScroll scrolls by the delta needed to keep the anchor row at
// the same viewport position after a prepend changed its location.
function compensateLiveScroll(anchor) {
  if (!anchor) return;
  for (const tr of document.querySelectorAll('#live-table tr[data-live-key]')) {
    if (tr.dataset.liveKey !== anchor.key) continue;
    const delta = tr.getBoundingClientRect().top - anchor.top;
    if (delta) window.scrollBy(0, delta);
    return;
  }
}

// maybeAddLiveSpacer ensures a tall trailing spacer exists inside #live-table
// whenever a row is expanded. The spacer gives the sub-screen (page not
// scrollable) case enough room for prepend scroll compensation to work.
function maybeAddLiveSpacer(tbl) {
  if (!tbl || liveOpenIds.size === 0) return;
  let spacer = tbl.querySelector('#live-spacer');
  if (!spacer) {
    spacer = document.createElement('div');
    spacer.id = 'live-spacer';
    tbl.appendChild(spacer);
  }
}

// maybeRemoveLiveSpacer drops the spacer once no live rows are expanded.
function maybeRemoveLiveSpacer(tbl) {
  if (!tbl) return;
  const spacer = tbl.querySelector('#live-spacer');
  if (spacer && liveOpenIds.size === 0) spacer.remove();
}

// prependLiveRows inserts new rows at the top of the live tbody with scroll
// compensation so the visible viewport does not jump.
function prependLiveRows(tbl, tbody, rows) {
  maybeAddLiveSpacer(tbl);
  const anchor = captureLiveScrollAnchor(tbl);
  const html = rows.map((r) => { const h = liveRowHTML(r); return h.summary + h.detail; }).join('');
  tbody.insertAdjacentHTML('afterbegin', html);
  for (const r of rows) {
    if (r.requestId) {
      const tr = findLiveSummaryRow(tbody, r.requestId);
      if (tr) tr.onclick = () => toggleLiveRowDetail(tr.dataset.id);
    }
  }
  compensateLiveScroll(anchor);
}

// applyLiveEventDOM performs targeted DOM surgery for All/live mode after
// applyLiveEvent has updated liveRows/liveByReq. Session mode still falls back
// to the existing full re-render. start and eventOnly rows are prepended;
// end/progress/guard events update the existing row in place.
function applyLiveEventDOM(e) {
  if (liveSessionFilter) {
    renderLiveTable();
    return;
  }
  refreshLiveSessionOptions();
  const tbl = document.getElementById('live-table');
  if (!tbl) return;
  const tbody = tbl.querySelector('tbody');
  if (!tbody) {
    renderLiveTable();
    return;
  }

  if (e.type === 'start') {
    const row = liveByReq[e.request_id];
    if (!row) return;
    prependLiveRows(tbl, tbody, [row]);
    return;
  }

  if (e.type === 'end') {
    const row = liveByReq[e.request_id];
    if (!row) return;
    const tr = findLiveSummaryRow(tbody, row.requestId);
    if (!tr) {
      // End without a matching DOM row (synthesized/trimmed): prepend it.
      prependLiveRows(tbl, tbody, [row]);
      return;
    }
    const next = updateLiveSummaryRow(tr, row);
    updateLiveDetailForRow(next, row);
    return;
  }

  if (e.type === 'progress') {
    const row = liveByReq[e.request_id];
    if (!row) return;
    const tr = findLiveSummaryRow(tbody, row.requestId);
    if (!tr) return;
    const detail = tr.nextElementSibling;
    if (detail && detail.classList.contains('live-detail-row')) {
      updateLiveResponseSection(detail.querySelector('td'), row);
    }
    return;
  }

  // Guard/budget/other non-lifecycle events attached to a known request.
  if (e.request_id && liveByReq[e.request_id]) {
    const row = liveByReq[e.request_id];
    const tr = findLiveSummaryRow(tbody, row.requestId);
    if (!tr) return;
    const next = updateLiveSummaryRow(tr, row);
    updateLiveDetailForRow(next, row);
    return;
  }

  // Event-only row (no request id): prepend it.
  const row = liveRows[0];
  if (row && row.eventOnly) prependLiveRows(tbl, tbody, [row]);
}

// renderLiveDetailRow builds the <tr> shown beneath an expanded live row.
function renderLiveDetailRow(r) {
  return `<tr class="req-detail-row live-detail-row"><td colspan="7">${liveDetailHTML(r)}</td></tr>`;
}

// liveGuardSectionHTML renders the accumulated guard hits for a live detail
// cell. Extracted so incremental updates can replace just this section.
function liveGuardSectionHTML(r) {
  if (!r.guardHits || !r.guardHits.length) return '';
  let html = `<div class="live-guard-section"><div class="section-title">guard hits</div>`;
  for (const h of r.guardHits) {
    html += `<div class="live-guard-hit">
      <span class="mono">${esc(fmtTimeSafe(h.ts))}</span>
      <span class="badge warn">⚑ ${esc(fmtGuardDetail(h.detail))}</span>
    </div>`;
  }
  html += `</div>`;
  return html;
}

// liveDetailHTML renders the expanded content for one live row: accumulated
// guard hits first, then the requestlog body records (or a hint while in flight
// or before the first fetch).
function liveDetailHTML(r) {
  let html = liveGuardSectionHTML(r);
  const state = liveDetailState.get(r.requestId) || {};
  const recs = requestsDetailCache.get(r.requestId);
  if (r.inFlight) {
    html += liveResponseHTML(r);
    html += '<div class="msg hint">request still in flight — body records appear when it completes</div>';
    return html;
  }
  if (state.loading) {
    html += '<span class="hint">loading…</span>';
    return html;
  }
  if (state.notLogged) {
    html += '<div class="msg hint">not logged — the request did not commit, so there is no request-log record</div>';
    return html;
  }
  if (state.error) {
    html += `<div class="msg err">${esc(state.error)}</div>`;
    return html;
  }
  if (recs && recs.length) {
    html += detailRecordsHTML(recs);
    return html;
  }
  if (recs && !recs.length) {
    html += '<div class="msg hint">no record</div>';
    return html;
  }
  // Ended but records not fetched yet; the post-render pass will start the fetch.
  html += '<span class="hint">loading…</span>';
  return html;
}

// liveResponseHTML renders the accumulated upstream response for an in-flight
// request. The text is head-capped and throttled by the backend; the label
// shows the total bytes received so far.
function liveResponseHTML(r) {
  const text = r.progressText || '';
  const bytes = r.progressBytes || 0;
  let html = '<div class="live-response-section"><div class="section-title">live response';
  if (bytes > 0) {
    html += ` <span class="hint">(${esc(fmtProgressBytes(bytes))})</span>`;
  }
  html += '</div>';
  if (text) {
    html += `<pre class="live-response-pre mono">${esc(text)}</pre>`;
  } else {
    html += '<div class="msg hint">waiting for first bytes from upstream…</div>';
  }
  html += '</div>';
  return html;
}

// toggleLiveRowDetail expands/collapses the detail for a live request row.
// In All/live mode this performs in-place DOM surgery; the session panel keeps
// its full re-render.
function toggleLiveRowDetail(id) {
  if (liveSessionFilter) {
    // Session panel is not yet incremental; reuse its existing toggle path.
    toggleLiveSessionRow(id);
    return;
  }
  const tbl = document.getElementById('live-table');
  const tr = tbl && tbl.querySelector(`tbody tr.live-row[data-id="${esc(id)}"]`);
  const row = liveByReq[id];
  if (!tr || !row) {
    // DOM not built yet or out of sync: fall back to a full render.
    if (liveOpenIds.has(id)) liveOpenIds.delete(id);
    else liveOpenIds.add(id);
    renderLiveTable();
    return;
  }

  if (liveOpenIds.has(id)) {
    liveOpenIds.delete(id);
    const detail = tr.nextElementSibling;
    if (detail && detail.classList.contains('live-detail-row')) detail.remove();
    tr.classList.remove('live-open');
    maybeRemoveLiveSpacer(tbl);
  } else {
    liveOpenIds.add(id);
    if (!row.inFlight) {
      const state = liveDetailState.get(id) || {};
      if (state.error || state.notLogged) liveDetailState.set(id, { ...state, error: '', notLogged: false });
    }
    tr.insertAdjacentHTML('afterend', renderLiveDetailRow(row));
    tr.classList.add('live-open');
    maybeAddLiveSpacer(tbl);
    if (!row.inFlight) ensureLiveDetailFetched(id);
  }
}

// ensureLiveDetailFetched starts a fetch for the requestlog records if the row
// has ended and the records are not already cached, in flight, or failed. A
// recorded error is terminal here (see shouldFetchDetail) so a 404 cannot loop.
function ensureLiveDetailFetched(id) {
  const row = liveByReq[id];
  if (!shouldFetchDetail(requestsDetailCache.has(id), liveDetailState.get(id), !row || row.inFlight)) return;
  const state = liveDetailState.get(id) || {};
  liveDetailState.set(id, { ...state, loading: true, error: '' });
  fetchLiveDetail(id);
}

// fetchLiveDetail loads /api/requests/<id>, caches the records, and updates
// the detail row in place if the row is still open. Session mode still falls
// back to the session panel's full re-render.
async function fetchLiveDetail(id) {
  let recs = [];
  try {
    const resp = await apiGet('/api/requests/' + encodeURIComponent(id));
    recs = resp.records || [];
  } catch (e) {
    liveDetailState.set(id, detailFetchState(e.status, e.message));
    updateLiveDetailRow(id);
    return;
  }
  cacheRequestDetail(id, recs);
  liveDetailState.set(id, { loading: false, error: '' });
  updateLiveDetailRow(id);
}

// updateLiveDetailRow finds the open live detail row and refreshes its content.
function updateLiveDetailRow(id) {
  if (liveSessionFilter) {
    renderLiveSessionPanel();
    return;
  }
  if (!liveOpenIds.has(id)) return;
  const row = liveByReq[id];
  if (!row) return;
  const tbl = document.getElementById('live-table');
  const tr = tbl && tbl.querySelector(`tbody tr.live-row[data-id="${esc(id)}"]`);
  if (!tr) return;
  updateLiveDetailForRow(tr, row);
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
  { key: 'models', label: 'Models' },
  { key: 'tokens', label: 'Token Usage' },
  { key: 'cache', label: 'Cache' },
  { key: 'logs', label: 'Logs' },
  { key: 'live', label: 'Live' },
];
let statusCache = { st: null, tok: [], logs: [], accounts: [], agents: [], since: 0 };
// modelsCache is the last GET /api/models response ({providers:{…}}): the
// startup protocol probe's per-provider capability matrix. Read only through
// modelsCache.<field> so jstests/contract.test.mjs pins the documented fields.
let modelsCache = { providers: {} };
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

// renderStatusTab fetches the dashboard snapshot (status + tokens + logs +
// accounts), caches it, and renders the Status panel: a Warnings banner (only
// when warnings exist) + a sidebar+detail layout. Only the active section's
// pane re-renders on each 5s tick; section switches render from the cache with
// no extra fetch. Auto-refreshes every 5s while the Status tab is active; the
// timer is cleared when the user leaves the tab, and a tick is skipped while
// a popup (e.g. the pin menu) is open inside the panel so the background
// refresh never closes it.
async function renderStatusTab() {
  if (statusInflight) return;
  statusInflight = true;
  try {
    // An incomplete custom range (tokensRangeQuery → null) never fires a
    // request the server would 400 — the cards keep their previous data
    // while the date-input hint shows.
    const rangeQuery = tokensRangeQuery(tokensRange, Date.now());
    const tokensFetch = rangeQuery === null
      ? Promise.resolve({ usage: statusCache.tok || [], agents: statusCache.agents || [] })
      : apiGet('/api/tokens' + rangeQuery).catch(() => ({ usage: [], agents: [] }));
    const [st, tok, logs, acc, modelsDoc] = await Promise.all([
      apiGet('/api/status'),
      tokensFetch,
      apiGet('/api/logs?tail=200').catch(() => ({ lines: [] })),
      apiGet('/api/accounts').catch(() => ({ providers: [] })),
      apiGet('/api/models').catch(() => ({ providers: {} })),
    ]);
    setConn('ok', `v${st.version || '?'} · ${st.uptime || '—'} · ${st.listen || ''}`);
    statusCache = { st, tok: tok.usage || [], logs: logs.lines || [], accounts: acc.providers || [], agents: tok.agents || [], since: tok.since || 0 };
    modelsCache = modelsDoc;
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
    statusTimer = setInterval(() => {
      // A background tick must not wipe an open popup (the pin menu lives
      // inside the re-rendered pane) — skip this tick; the next one after
      // the menu closes picks the data up. Explicit renders (mutations,
      // section switches) bypass this guard and refresh immediately.
      if (panels.status && panels.status.querySelector('.route-pin-menu:not([hidden]), .tr-popover:not([hidden])')) return;
      renderStatusTab();
    }, 5000);
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
  // status-logs-active makes the Logs pane fill the viewport height (the log
  // <pre> flex-grows). Only set for the logs section; other sections are short
  // and should size to content.
  main.classList.toggle('status-logs-active', key === 'logs');
  switch (key) {
    case 'schedule':
      main.innerHTML = '';
      if (st) renderScheduleCard(main, st);
      break;
    case 'providers':
      main.innerHTML = '';
      if (st) renderProvidersCard(main, st);
      break;
    case 'models':
      main.innerHTML = '';
      renderModelsCard(main, modelsCache.providers || {});
      break;
    case 'tokens':
      main.innerHTML = '';
      renderTokensRangeControls(main);
      renderTokensCard(main, statusCache.tok || []);
      renderAgentsCard(main, statusCache.agents || []);
      break;
    case 'cache':
      main.innerHTML = '';
      if (st) renderCacheCard(main, st);
      break;
    case 'logs':
      // Don't clear here — renderLogsInto captures the existing scroll position
      // from the current .log-pre BEFORE wiping the container, so it can restore
      // it (freeze-on-scroll-up). Clearing here first would detach the old
      // .log-pre and defeat the capture.
      renderLogsInto(main, statusCache.logs || []);
      break;
    case 'live':
      // The live card is event-driven (SSE), not poll-driven: mount it once
      // per section entry. The 5s status tick re-renders the active section,
      // which must NOT wipe the card or reconnect the EventSource.
      if (!liveActive) {
        main.innerHTML = '';
        renderLiveCard(main);
      }
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
  // Leaving the Live section closes its SSE connection (the card is remounted
  // fresh, rows reset, on the next entry).
  if (name !== 'live') stopLiveEvents();
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
// headActionsHTML, when given, pins extra controls (buttons) to the right end
// of the header, grouped with the meta text.
function buildCard(title, meta, bodyHTML, extraBodyClass = '', headActionsHTML = '') {
  const metaHTML = meta ? `<span class="meta">${esc(meta)}</span>` : '';
  const headRight = headActionsHTML
    ? `<span class="card-head-side">${metaHTML}${headActionsHTML}</span>`
    : metaHTML;
  return `<section class="card">
    <header class="card-head"><h2>${esc(title)}</h2>${headRight}</header>
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
  if (!h) return `<span class="pill muted">—</span>`;
  const now = Date.now();
  const rlUntil = h.rate_limited_until ? new Date(h.rate_limited_until).getTime() : 0;
  if (h.frozen) {
    return `<span class="pill warn" title="manually frozen by operator — excluded from scheduling until unfreeze">frozen</span>`;
  }
  if (h.circuit_state === 'open' || h.circuit_state === 'half_open') {
    const tail = h.circuit_state === 'half_open' ? ' (probing)' : untilHuman(h.circuit_until, now);
    return `<span class="pill err" title="circuit ${esc(h.circuit_state)}">circuit ${esc(h.circuit_state)}${esc(tail)}</span>`;
  }
  if (rlUntil && rlUntil > now) {
    const kind = h.rate_limit_kind && h.rate_limit_kind !== 'transient' ? ` (${h.rate_limit_kind})` : '';
    return `<span class="pill warn" title="rate-limited (${esc(h.rate_limit_kind || 'transient')}) until ${esc(h.rate_limited_until)}">rate-limited${esc(kind)}${esc(untilHuman(h.rate_limited_until, now))}</span>`;
  }
  if (h.available) {
    return `<span class="pill ok">available</span>`;
  }
  return `<span class="pill muted">unavailable</span>`;
}

// accountRemainingPill renders a compact pill summarizing one account's quota
// snapshot — the "remaining amount" surfacing the old Quota card's data inline
// per account. Mirrors the Accounts tab's collapsed hint:
//   session-expired / not-logged-in / error  → err pill
//   ultimate window RemainingPct             → "X% left" (ok/warn/err by threshold)
//   otherwise (plan / no ultimate window)    → muted plan / "available"
// `snap` is a raw provider.QuotaSnapshot (PascalCase) from /api/status.quota,
// keyed by accountProviderKey(p, a); null when the account has no snapshot yet.
function accountRemainingPill(snap) {
  if (!snap) return `<span class="pill muted">no data</span>`;
  if (snap.Err) {
    const k = quotaErrKind(snap);
    const lbl = k === 'session-expired' ? 'session expired'
      : k === 'not-logged-in' ? 'not logged in' : 'error';
    return `<span class="pill err">${esc(lbl)}</span>`;
  }
  const ult = (snap.Windows || []).find((w) => w.Ultimate);
  if (ult && ult.RemainingPct != null && ult.RemainingPct >= 0) {
    const p = ult.RemainingPct;
    const cls = p > 0.3 ? 'ok' : (p > 0.1 ? 'warn' : 'err');
    return `<span class="pill ${cls}">${(p * 100).toFixed(1)}% left</span>`;
  }
  if (snap.Plan) return `<span class="pill muted">${esc(snap.Plan)}</span>`;
  return `<span class="pill ok">available</span>`;
}

// renderProvidersCard draws the per-provider health + request-counter table,
// with each provider's accounts listed inline beneath its row (one sub-row per
// account showing label/email + a remaining-amount pill). This replaces the
// standalone Quota section: the per-account remaining quota now lives here.
//
// Provider names come from providerNames (pure.js): the UNION of the schedule
// preview's ordered chains and the health map's keys. Schedule alone
// enumerates every configured provider for a fresh all-healthy daemon (health
// is created lazily on failure/freeze, so health={} then), but scheduling
// drops UNAVAILABLE targets (operator-frozen, circuit-open, rate-limit
// cooldown, quota-exhausted) from ordered — the health union keeps those rows
// (and their unfreeze button) visible. `health` (may be absent → neutral "—")
// and `counters` (absent → 0) are joined per name; accounts come from
// /api/accounts (statusCache.accounts).
function renderProvidersCard(target, st) {
  const health = st.health || {};
  const counters = st.counters || {};
  const quota = st.quota || {};
  const models = (st.schedule && st.schedule.models) || {};
  const names = providerNames(models, health);
  if (names.length === 0) return;
  // Index /api/accounts by provider name so each row can look up its accounts.
  const acctByName = {};
  for (const p of (statusCache.accounts || [])) acctByName[p.name] = p;
  let rows = '';
  const nowMs = Date.now();
  for (const name of names) {
    const c = counters[name] || {};
    // Per-row unfreeze only makes sense while the provider is actually frozen
    // (operator freeze, circuit open/half-open, or inside a rate-limit
    // cooldown); health entries are created lazily on failure/freeze, so a
    // healthy provider has no entry and gets the freeze button instead.
    const h = health[name];
    const frozen = providerFrozen(h, nowMs);
    const action = frozen
      ? ` <button class="btn small danger-solid" data-unfreeze="${esc(name)}" title="clear circuit/rate-limit cooldowns + model locks">unfreeze</button>`
      : ` <button class="btn small" data-freeze="${esc(name)}" title="exclude from scheduling until unfreeze">freeze</button>`;
    rows += `<tr>
      <td class="mono">${esc(name)}${action}</td>
      <td>${healthPill(health[name])}</td>
      <td class="num">${fmtNum(c.requests)}</td>
      <td class="num">${fmtNum(c.failovers)}</td>
      <td class="num">${fmtNum(c.rate_limited_429)}</td>
      <td class="num">${fmtNum(c.failures)}</td>
      <td class="num">${fmtNum(avgLatencyMs(c))}</td>
      <td class="num subdue">${esc(fmtUnix(c.last_request_at))}</td>
    </tr>`;
    // Per-account expanded list: one sub-row spanning all columns, showing each
    // account's label/email + remaining-amount pill.
    const p = acctByName[name];
    const accs = (p && p.accounts) || [];
    if (accs.length) {
      const isOAuth = p.provider_id === 'aqp' || p.provider_id === 'codex';
      const items = accs.map((a, i) => {
        const key = accountProviderKey(p, a);
        // OAuth accounts (aqp/codex) are SSO logins identified by email;
        // apikey accounts (zhipu/deepseek/volcengine) have no email, so they
        // are identified by `id: <id>` (with an optional label suffix).
        const ident = isOAuth
          ? (a.email || a.label || a.id)
          : `id: ${a.id || '-'}${a.label ? ' · ' + a.label : ''}`;
        const slot = i % 4;
        return `<div class="prov-account prov-acct-c${slot}">
          <span class="prov-acct-label">${esc(ident)}</span>
          ${accountRemainingPill(quota[key])}
        </div>`;
      }).join('');
      rows += `<tr class="prov-accounts-row"><td colspan="8"><div class="prov-accounts">${items}</div></td></tr>`;
    } else {
      rows += `<tr class="prov-accounts-row"><td colspan="8"><div class="prov-accounts"><div class="prov-account"><span class="prov-acct-label muted">no accounts</span></div></div></td></tr>`;
    }
  }
  const html = buildCard('Providers', `${names.length} configured`, `
      <table class="table prov-table">
        <thead><tr>
          <th>provider</th><th>health</th>
          <th class="num">reqs</th><th class="num">failovers</th>
          <th class="num">429</th><th class="num">failures</th>
          <th class="num">lat</th><th class="num">last</th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table>`, 'flush');
  target.insertAdjacentHTML('beforeend', html);
  for (const b of document.querySelectorAll('[data-unfreeze]')) {
    b.addEventListener('click', () => unfreezeProvider(b.dataset.unfreeze, b));
  }
  for (const b of document.querySelectorAll('[data-freeze]')) {
    b.addEventListener('click', () => freezeProvider(b.dataset.freeze, b));
  }
}

// unfreezeProvider clears one provider's frozen state (circuit open/half-open,
// rate-limit cooldown, model locks) via POST /api/health/reset so it is
// retried immediately — the operator escape hatch for abnormal edge cases
// (account topped up, misclassified 429, window reset early). Rendered only on
// frozen rows; confirmed via the themed modal (same pattern as unfreezeAll).
async function unfreezeProvider(name, btn) {
  const ok = await confirmDialog('Unfreeze provider',
    `Clear circuit-breaker, rate-limit cooldown and model-lock state for ${name}? The provider is retried immediately.`,
    'Unfreeze');
  if (!ok) return;
  if (btn) { btn.disabled = true; btn.textContent = 'unfreezing…'; }
  try {
    await apiPost('/api/health/reset', { provider: name });
    await renderStatusTab();
  } catch (e) {
    if (btn) { btn.disabled = false; btn.textContent = 'unfreeze'; }
    window.alert('unfreeze failed: ' + e.message);
  }
}

// freezeProvider manually freezes one provider via POST /api/health/freeze so
// scheduling skips it until unfreeze — the operator counterpart of
// unfreezeProvider, for taking a degraded/misbehaving provider out of rotation
// without editing config. Rendered only on non-frozen rows; confirmed via the
// themed modal (same pattern as unfreezeProvider).
async function freezeProvider(name, btn) {
  const ok = await confirmDialog('Freeze provider',
    `Freeze ${name}? The provider is excluded from scheduling — no requests are routed to it — until you unfreeze it. The freeze persists across restarts.`,
    'Freeze');
  if (!ok) return;
  if (btn) { btn.disabled = true; btn.textContent = 'freezing…'; }
  try {
    await apiPost('/api/health/freeze', { provider: name });
    await renderStatusTab();
  } catch (e) {
    if (btn) { btn.disabled = false; btn.textContent = 'freeze'; }
    window.alert('freeze failed: ' + e.message);
  }
}

// renderModelsCard draws the startup protocol probe's capability matrix
// (GET /api/models, cached in modelsCache): ONE CARD PER PROVIDER — the card
// head carries the provider name, config fingerprint, last probe time and the
// Refresh action; the body is the model × protocol table with a verdict pill
// per leg (chat / anthropic / responses). Verdicts are backend-owned — the UI
// never re-derives support, it renders yes ✓ / no ✗ / unknown ? with unknown
// (muted, probe pending) visually distinct from no (err, concluded negative
// or unsupported by definition). Providers with no probe data are omitted
// server-side; an empty store renders a hint instead of a blank section.
function renderModelsCard(target, providers) {
  const entries = modelCapMatrix(providers);
  if (!entries.length) {
    target.insertAdjacentHTML('beforeend', buildCard('Models', '',
      '<div class="model-caps-empty">no probe data yet — provider models are probed for protocol support at daemon startup</div>'));
    return;
  }
  for (const p of entries) {
    let rows = '';
    for (const m of p.models) {
      rows += `<tr>
        <td class="mono">${esc(m.id)}</td>
        <td>${protoVerdictPill(m.chat)}</td>
        <td>${protoVerdictPill(m.anthropic)}</td>
        <td>${protoVerdictPill(m.responses)}</td>
      </tr>`;
    }
    if (!p.models.length) {
      rows = '<tr><td colspan="4" class="subdue">probed, no models recorded</td></tr>';
    }
    target.insertAdjacentHTML('beforeend', buildCard(
      p.name,
      `fp ${p.fingerprint || '—'} · probed ${fmtTimeSafe(p.probedAt) || '—'}`,
      `<table class="table">
        <thead><tr><th>model</th><th>chat</th><th>anthropic</th><th>responses</th></tr></thead>
        <tbody>${rows}</tbody>
      </table>`,
      'flush model-caps',
      `<button class="btn small danger-solid model-caps-refresh" data-models-refresh="${esc(p.name)}">Refresh</button>`));
  }
  target.querySelectorAll('[data-models-refresh]').forEach((btn) => {
    btn.addEventListener('click', () => refreshProviderModels(btn));
  });
}

// refreshProviderModels runs the daemon's models refresh (the web twin of
// `model-proxy models refresh <provider>`) for one provider: fetch the live
// list, probe every candidate, write the callable subset to config and
// reload. The result summary is surfaced verbatim (the backend owns the
// verdicts); the tab re-renders from fresh /api/models data afterwards.
async function refreshProviderModels(btn) {
  const provider = btn.getAttribute('data-models-refresh');
  btn.disabled = true;
  btn.textContent = 'refreshing…';
  try {
    const r = await apiPost('/api/models/refresh', { provider });
    const lines = [`${r.provider}: ${r.kept.length} model${r.kept.length === 1 ? '' : 's'} kept (${r.config_updated ? 'config updated, reloaded' : 'config unchanged'})`];
    if (r.added.length) lines.push(`added: ${r.added.join(', ')}`);
    if (r.removed.length) lines.push(`removed: ${r.removed.join(', ')}`);
    for (const d of r.probe_dropped) lines.push(`dropped: ${d.model} — ${d.reason}`);
    if (r.policy_dropped.length) lines.push(`policy-filtered: ${r.policy_dropped.join(', ')}`);
    if (r.warning) lines.push(`warning: ${r.warning}`);
    window.alert(lines.join('\n'));
    await renderStatusTab();
  } catch (e) {
    btn.disabled = false;
    btn.textContent = 'Refresh';
    window.alert('models refresh failed: ' + e.message);
  }
}

// protoVerdictPill renders one protocol leg's probe verdict as a pill:
// yes → ok ✓, no → err ✗, unknown → muted ? (see verdictBadge in pure.js).
// No leading dot — the glyph already encodes the verdict, and three dotted
// pills per row read as noise.
function protoVerdictPill(v) {
  const b = verdictBadge(v);
  return `<span class="pill ${esc(b.cls)}">${esc(b.glyph)} ${esc(b.label)}</span>`;
}

// renderScheduleCard builds the per-route schedule view: each route shows its
// ordered provider chain with the first choice highlighted, sticky marker, and
// pool summary. An active operator pin is OVERLAID on the default chain (the
// backend reports the unpinned order under pin) — the pinned node gets a 📌
// badge + the effective-first highlight stays on the pinned provider, so the
// override and what it overrides are both visible. Pin/unpin actions mutate
// via /api/pin and re-render from the refreshed /api/status.
function renderScheduleCard(target, st) {
  const models = (st.schedule && st.schedule.models) || {};
  const names = Object.keys(models).sort();
  if (names.length === 0) return;
  let blocks = '';
  for (const route of names) {
    const info = models[route];
    const ordered = info.ordered || [];
    const effectiveFirst = info.first || (ordered[0] && ordered[0].provider) || '';
    let chain = '';
    ordered.forEach((p, i) => {
      const classes = ['route-node'];
      if (p.provider === effectiveFirst) classes.push('first');
      if (info.sticky && info.sticky === p.provider) classes.push('sticky');
      if (!p.available) classes.push('unavailable');
      const pinned = info.pin && (info.pin === p.provider || info.pin === p.pool_parent);
      if (pinned) classes.push('pinned');
      const peak = p.peak ? ' · peak' : '';
      const tier = p.tier ? ` · ${esc(p.tier)}` : '';
      const parent = p.pool_parent ? ` (${esc(p.pool_parent)})` : '';
      const title = `priority ${p.priority} · tier ${esc(p.tier || '?')} · surplus ${(p.surplus || 0).toFixed(2)}${peak}${pinned ? ' · pinned (no failover)' : ''}`;
      chain += `<span class="${classes.join(' ')}" title="${esc(title)}">${pinned ? '📌 ' : ''}${esc(p.provider)}${esc(parent)}${tier}</span>`;
      if (i < ordered.length - 1) chain += `<span class="route-sep">→</span>`;
    });
    // Unpinned routes get a pin button opening a small popover menu of the
    // route's own chain providers (pinning anything else is a no-op
    // server-side); pinned routes keep the pinned-node + unpin UX. The menu
    // floats over the card (absolute in .route-pin-wrap) — not an in-flow
    // banner, not a native <select>.
    if (!info.pin && ordered.length) {
      const items = ordered.map((p) =>
        `<button class="route-pin-item" data-pin-route="${esc(route)}" data-pin-provider="${esc(p.provider)}">${esc(p.provider)}${p.pool_parent ? ` <span class="route-meta">(${esc(p.pool_parent)})</span>` : ''}</button>`,
      ).join('');
      chain += `<span class="route-pin-wrap">` +
        `<button class="btn small" data-pin-toggle="${esc(route)}" title="pin ${esc(route)} to one provider (no failover)">📌 pin</button>` +
        `<div class="route-pin-menu" data-pin-menu="${esc(route)}" hidden>${items}</div>` +
        `</span>`;
    }
    if (!chain) chain = `<span class="route-meta">no providers available</span>`;
    let meta = '';
    if (info.pin) {
      const exp = info.pin_expires ? ` · expires ${esc(info.pin_expires)}` : ' · no expiry';
      meta += `<span class="route-pin">pinned: <span class="mono">${esc(info.pin)}</span>${exp}</span> `;
      meta += `<button class="btn small" data-unpin-route="${esc(route)}">unpin</button>`;
      if (info.first && info.first !== info.pin) {
        meta += ` <span class="route-meta">effective: ${esc(info.first)} (pool account of ${esc(info.pin)})</span>`;
      }
      meta += '<br>';
    }
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
  const card = target.lastElementChild;
  const msg = card && card.querySelector('.card-body');
  card.addEventListener('click', async (e) => {
    const toggle = e.target.closest('[data-pin-toggle]');
    if (toggle) {
      const menu = card.querySelector(`[data-pin-menu="${CSS.escape(toggle.dataset.pinToggle)}"]`);
      const wasHidden = menu && menu.hidden;
      card.querySelectorAll('.route-pin-menu').forEach((m) => { m.hidden = true; });
      if (menu && wasHidden) menu.hidden = false;
      return;
    }
    if (!e.target.closest('.route-pin-menu')) {
      card.querySelectorAll('.route-pin-menu').forEach((m) => { m.hidden = true; });
    }
    const pinBtn = e.target.closest('[data-pin-route]');
    const unpinBtn = e.target.closest('[data-unpin-route]');
    if (!pinBtn && !unpinBtn) return;
    try {
      if (pinBtn) {
        await apiPost('/api/pin', { route: pinBtn.dataset.pinRoute, provider: pinBtn.dataset.pinProvider });
      } else {
        await apiDel('/api/pin?route=' + encodeURIComponent(unpinBtn.dataset.unpinRoute));
      }
      await renderStatusTab();
    } catch (err) {
      if (msg) showMsg(msg, 'err', err.message);
    }
  });
}

// renderTokensCard draws the per-(provider, model) token usage table.
// renderCacheCard shows the exact-response cache counters (GET /api/status
// `cache`: {enabled,hits,misses,entries,models}). A disabled cache renders a
// hint instead of a zero table; hit rate comes from the pure cacheHitRate
// helper and shows "—" before the first lookup. entries is a live gauge
// (current held responses), not a cumulative counter — it can be lower than
// misses because failed requests miss without storing, and TTL/eviction
// removes entries while counters only grow.
function renderCacheCard(target, st) {
  const c = (st && st.cache) || {};
  let body;
  if (!c.enabled) {
    body = `<div class="empty-state">Exact response cache is off — enable <code>cache.enabled</code> in config to dedupe identical requests.</div>`;
  } else {
    const models = c.models || [];
    let rows = `<tr class="mono">
      <td>total</td>
      <td class="num">${fmtNum(c.entries)}</td>
      <td class="num">${fmtNum(c.hits)}</td>
      <td class="num">${fmtNum(c.misses)}</td>
      <td class="num">${esc(cacheHitRate(c.hits, c.misses))}</td>
    </tr>`;
    for (const m of models) {
      rows += `<tr class="mono">
        <td>${esc(m.model)}</td>
        <td class="num">${fmtNum(m.entries)}</td>
        <td class="num">${fmtNum(m.hits)}</td>
        <td class="num">${fmtNum(m.misses)}</td>
        <td class="num">${esc(cacheHitRate(m.hits, m.misses))}</td>
      </tr>`;
    }
    body = `<table class="table">
      <thead><tr>
        <th>model</th><th class="num">entries (live)</th><th class="num">hits</th><th class="num">misses</th><th class="num">hit rate</th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
  }
  const html = buildCard('Cache', 'exact response cache', body, 'flush');
  target.insertAdjacentHTML('beforeend', html);
}

function renderTokensCard(target, usage) {
  if (!usage || usage.length === 0) {
    const empty = tokensRange.preset === 'all'
      ? 'No observed usage yet. Counts accrue as the proxy streams SSE responses.'
      : `No usage in the selected range (${tokenRangeLabel(tokensRange)}).`;
    const html = buildCard('Token usage', '0',
      `<div class="empty-state">${empty}</div>`);
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
      <td class="num">${fmtNum(u.total)}</td>
      <td class="num">${fmtNum(u.requests)}</td>
    </tr>`;
  }
  const html = buildCard('Token usage', `${totalReqs} requests · ${tokensRangeMeta()}`, `
      <table class="table">
        <thead><tr>
          <th>provider</th><th>model</th>
          <th class="num">input</th><th class="num">output</th>
          <th class="num">cache create</th><th class="num">cache read</th>
          <th class="num">total</th><th class="num">requests</th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table>`, 'flush');
  target.insertAdjacentHTML('beforeend', html);
}

// sinceLabel renders the counting-epoch anchor shared by the Token usage and
// Agents cards: the earliest persisted bucket as "since YY-MM-DD HH:MM", or
// "since start" before any bucket exists (fresh DB / stats disabled).
function sinceLabel() {
  const s = fmtSinceDate(statusCache.since);
  return s ? `since ${s}` : 'since start';
}

// tokensRange is the APPLIED time-dimension state of the token usage cards: a
// preset value from pure.js TOKEN_RANGES (default 'today' — the day's usage is
// the common case; 'all' = cumulative counters) plus the applied custom
// range's 'YYYY-MM-DD' days. tokensRangePicker
// is the popover's own UI state (open flag, the left month of the two-month
// calendar view, an in-progress custom start-day pick, and whether the custom
// row shows the checkmark while picking). Both are module-level like
// requestsFilter: they persist across re-renders within the session, and an
// open popover blocks the 5s tick (see renderStatusTab) so a re-render can
// never clobber an in-progress pick.
let tokensRange = { preset: 'today', customStart: '', customEnd: '' };
let tokensRangePicker = { open: false, view: null, pick: null, selecting: false };

// tokensRangeMeta is the shared range label for the Token usage and Agents
// cards: the counting-epoch anchor for the cumulative view, the selected
// range otherwise.
function tokensRangeMeta() {
  return tokensRange.preset === 'all' ? sinceLabel() : tokenRangeLabel(tokensRange);
}

// rangePickerClose closes the popover and discards any in-progress custom
// pick — Esc and outside clicks never change the applied range.
function rangePickerClose() {
  tokensRangePicker = { open: false, view: null, pick: null, selecting: false };
  document.removeEventListener('keydown', rangePickerOnKey);
  document.removeEventListener('click', rangePickerOnOutside, true);
}

function rangePickerOnKey(e) {
  if (e.key === 'Escape') {
    rangePickerClose();
    renderStatusPanel();
  }
}

function rangePickerOnOutside(e) {
  if (!e.target.closest('.tr-wrap')) {
    rangePickerClose();
    renderStatusPanel();
  }
}

// renderTokensRangeControls draws the 时间维度-style time-dimension picker
// above the Token usage / Agents cards (one picker drives both — they share
// the same /api/tokens payload): a two-part trigger button (caption + active
// dimension + chevron) opening a popover with a preset list on the left
// (checkmark on the active preset, click applies and closes) and a two-month
// calendar on the right for the custom range (first click sets start, second
// sets end with swap, complete range applies and closes; future days are
// dimmed and unclickable; ‹ › move the window by one month, never past the
// month containing today).
function renderTokensRangeControls(target) {
  const picker = tokensRangePicker;
  const presets = TOKEN_RANGES.map((w) => {
    const active = w.value === 'custom'
      ? (tokensRange.preset === 'custom' || picker.selecting)
      : tokensRange.preset === w.value;
    return `<button class="tr-preset${active ? ' active' : ''}" data-tr-preset="${esc(w.value)}">
      <span class="tr-check">${active ? '✓' : ''}</span>${esc(w.label)}
    </button>`;
  }).join('');

  let calendar = '';
  if (picker.open) {
    const now = Date.now();
    const months = twoMonthWindow(picker.view.year, picker.view.month).map(({ year, month }) => {
      const weeks = calendarMonthGrid(year, month).map((week) => `<tr>${week.map((day) => {
        if (day === null) return '<td class="tr-blank"></td>';
        const dayYmd = ymd(year, month, day);
        const future = isFutureDay(year, month, day, now);
        // While a NEW pick is in progress the previously applied range's
        // highlight gives way to the pick's own start circle.
        const applied = tokensRange.preset === 'custom' && !picker.pick ? tokensRange : null;
        const isStart = dayYmd === picker.pick || (applied && dayYmd === applied.customStart);
        const isEnd = applied && dayYmd === applied.customEnd;
        const inRange = applied && !isStart && !isEnd &&
          dayYmd > applied.customStart && dayYmd < applied.customEnd;
        const cls = ['tr-day'];
        if (isStart || isEnd) cls.push('tr-day-selected');
        else if (inRange) cls.push('tr-day-inrange');
        return `<td><button class="${cls.join(' ')}" data-tr-day="${dayYmd}" ${future ? 'disabled' : ''}>${day}</button></td>`;
      }).join('')}</tr>`).join('');
      const header = WEEKDAYS.map((w) => `<th>${w}</th>`).join('');
      return `<div class="tr-month">
        <div class="tr-month-title">${esc(monthTitle(year, month))}</div>
        <table class="tr-grid"><thead><tr>${header}</tr></thead><tbody>${weeks}</tbody></table>
      </div>`;
    }).join('');
    const thisMonth = (() => { const d = new Date(now); return d.getFullYear() * 12 + d.getMonth(); })();
    const viewRight = picker.view.year * 12 + picker.view.month + 1;
    calendar = `<div class="tr-cal">
      <button class="tr-nav tr-prev" data-tr-nav="-1" aria-label="previous month">‹</button>
      <div class="tr-months">${months}</div>
      <button class="tr-nav tr-next" data-tr-nav="1" aria-label="next month" ${viewRight >= thisMonth ? 'disabled' : ''}>›</button>
    </div>`;
  }

  target.insertAdjacentHTML('beforeend',
    `<div class="tokens-toolbar">
      <div class="tr-wrap">
        <button class="btn small tr-trigger" id="tr-trigger" aria-haspopup="true" aria-expanded="${picker.open}">
          <span class="tr-caption">Time Range</span>
          <span class="tr-value">${esc(tokenRangeTriggerLabel(tokensRange))}</span>
          <span class="tr-chevron">▾</span>
        </button>
        <div class="tr-popover" ${picker.open ? '' : 'hidden'}>
          <div class="tr-presets">${presets}</div>
          ${calendar}
        </div>
      </div>
      <button class="btn small danger-solid" id="btn-tokens-reset">Reset counters</button>
    </div>`);

  const wrap = target.lastElementChild.querySelector('.tr-wrap');
  target.querySelector('#btn-tokens-reset').addEventListener('click', resetTokens);
  wrap.querySelector('#tr-trigger').addEventListener('click', () => {
    if (picker.open) {
      rangePickerClose();
    } else {
      // Open on the month containing the current selection (custom start day
      // when a custom range is applied), else the month containing today.
      const anchor = (tokensRange.preset === 'custom' && parseLocalDate(tokensRange.customStart)) || new Date();
      tokensRangePicker = { open: true, view: { year: anchor.getFullYear(), month: anchor.getMonth() }, pick: null, selecting: false };
      document.addEventListener('keydown', rangePickerOnKey);
      document.addEventListener('click', rangePickerOnOutside, true);
    }
    renderStatusPanel();
  });
  wrap.querySelectorAll('[data-tr-preset]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const value = btn.dataset.trPreset;
      if (value === 'custom') {
        // Checkmark moves to custom and the calendar takes over; the applied
        // range only changes once both days are picked.
        tokensRangePicker.selecting = true;
        tokensRangePicker.pick = null;
        renderStatusPanel();
        return;
      }
      tokensRange.preset = value;
      rangePickerClose();
      renderStatusTab();
    });
  });
  wrap.querySelectorAll('[data-tr-day]').forEach((btn) => {
    btn.addEventListener('click', () => {
      if (!tokensRangePicker.selecting && tokensRange.preset !== 'custom') {
        // Clicking days without the custom dimension armed starts a custom pick.
        tokensRangePicker.selecting = true;
      }
      const result = rangePick(tokensRangePicker.pick, btn.dataset.trDay);
      if (!result.complete) {
        tokensRangePicker.pick = result.pick;
        renderStatusPanel();
        return;
      }
      tokensRange = { preset: 'custom', customStart: result.start, customEnd: result.end };
      rangePickerClose();
      renderStatusTab();
    });
  });
  wrap.querySelectorAll('[data-tr-nav]').forEach((btn) => {
    btn.addEventListener('click', () => {
      tokensRangePicker.view = shiftMonth(picker.view.year, picker.view.month, Number(btn.dataset.trNav));
      renderStatusPanel();
    });
  });
}

// renderAgentsCard draws the per-agent breakdown ("who is burning my quota"):
// one summary row per agent (the server-provided totals from /api/tokens,
// heaviest first) with its per-(provider, model) breakdown rows nested
// underneath — every row shows all five token dimensions. Rendered directly
// under the Token usage card in the same section — the same since-daemon-start
// window as the provider/model table (both reset by the Reset counters button).
function renderAgentsCard(target, agents) {
  if (!agents || !agents.length) {
    const hint = tokensRange.preset === 'all'
      ? 'No agent activity yet. Agents are detected from the client User-Agent (claude-cli, codex, opencode, pi); unrecognized clients are labeled by their User-Agent.'
      : `No agent activity in the selected range (${tokenRangeLabel(tokensRange)}).`;
    target.insertAdjacentHTML('beforeend', buildCard('Agents', tokensRangeMeta(),
      `<div class="msg hint">${hint}</div>`));
    return;
  }
  let rows = '';
  for (const a of agents) {
    rows += `<tr class="agent-summary">
      <td class="mono">${esc(a.agent)}</td>
      <td class="num">${fmtNum(a.requests)}</td>
      <td class="num">${fmtNum(a.input)}</td>
      <td class="num">${fmtNum(a.output)}</td>
      <td class="num">${fmtNum(a.cache_creation)}</td>
      <td class="num">${fmtNum(a.cache_read)}</td>
      <td class="num">${fmtNum(a.total)}</td>
    </tr>`;
    for (const m of (a.models || [])) {
      rows += `<tr class="agent-model">
        <td class="mono">${esc(m.provider)}/${esc(m.model)}</td>
        <td class="num">${fmtNum(m.requests)}</td>
        <td class="num">${fmtNum(m.input)}</td>
        <td class="num">${fmtNum(m.output)}</td>
        <td class="num">${fmtNum(m.cache_creation)}</td>
        <td class="num">${fmtNum(m.cache_read)}</td>
        <td class="num">${fmtNum(m.total)}</td>
      </tr>`;
    }
  }
  const html = buildCard('Agents', `${agents.length} active · ${tokensRangeMeta()}`, `
      <table class="table">
        <thead><tr>
          <th>agent / model</th><th class="num">requests</th>
          <th class="num">input</th><th class="num">output</th>
          <th class="num">cache create</th><th class="num">cache read</th>
          <th class="num">total</th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table>`, 'flush');
  target.insertAdjacentHTML('beforeend', html);
}

// confirmDialog shows the themed #confirm-modal in place of window.confirm.
// Resolves true only when the confirm button is clicked; Esc, Close and
// Cancel all resolve false.
function confirmDialog(title, message, confirmLabel) {
  const modal = document.getElementById('confirm-modal');
  if (!modal) return Promise.resolve(false);
  return new Promise((resolve) => {
    modal.innerHTML =
      `<form method="dialog">
        <header class="modal-head">
          <h2 id="confirm-title">${esc(title)}</h2>
          <button type="button" class="link-btn" id="confirm-cancel" aria-label="Close">Close</button>
        </header>
        <div class="modal-body">
          <p>${esc(message)}</p>
          <div class="modal-actions">
            <button type="button" class="btn small" id="confirm-no">Cancel</button>
            <button type="button" class="btn small danger-solid" id="confirm-yes">${esc(confirmLabel)}</button>
          </div>
        </div>
      </form>`;
    const onCancel = () => resolve(false);
    const done = (ok) => {
      modal.removeEventListener('cancel', onCancel);
      if (modal.open) modal.close();
      resolve(ok);
    };
    modal.addEventListener('cancel', onCancel, { once: true });
    document.getElementById('confirm-cancel').addEventListener('click', () => done(false));
    document.getElementById('confirm-no').addEventListener('click', () => done(false));
    document.getElementById('confirm-yes').addEventListener('click', () => done(true));
    modal.showModal();
  });
}

async function resetTokens() {
  const ok = await confirmDialog('Reset token usage counters',
    'This zeroes the per-(provider, model) token usage stats. The action cannot be undone.',
    'Reset counters');
  if (!ok) return;
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

// bindLogSelection makes a clicked log entry the sole selected row. Selection
// is visual only: native text selection/copying remains untouched, while the
// selected class lets CSS emphasize the row's generated line number.
function bindLogSelection(pre) {
  if (!pre) return;
  pre.addEventListener('click', (event) => {
    const line = event.target.closest('.log-line');
    if (!line || !pre.contains(line)) return;
    pre.querySelectorAll('.log-line.selected').forEach((el) => {
      if (el !== line) el.classList.remove('selected');
    });
    line.classList.add('selected');
  });
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
  bindLogSelection(logsPre);
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
let yamlResizeFrame = 0;


// resizeYamlEditor measures the rendered Config layout instead of guessing a
// fixed top offset. cardRect.bottom - targetRect.bottom captures the action row,
// message area, and card padding beneath the editor; page spacing is added when
// filling a tall viewport. On short viewports the 480px hard minimum wins and
// the page scrolls vertically.
function resizeYamlEditor() {
  yamlResizeFrame = 0;
  const host = document.getElementById('yaml-editor');
  const card = document.getElementById('yaml-card');
  const target = yamlEditor ? yamlEditor.getWrapperElement()
    : document.getElementById('yaml-editor-fallback');
  if (!host || !card || !target || !host.offsetParent) return;

  const hostRect = host.getBoundingClientRect();
  const targetRect = target.getBoundingClientRect();
  const cardRect = card.getBoundingClientRect();
  const main = card.closest('main');
  const cardMargin = parseFloat(getComputedStyle(card).marginBottom) || 0;
  const mainPadding = main ? (parseFloat(getComputedStyle(main).paddingBottom) || 0) : 0;
  const spaceBelow = Math.max(0, cardRect.bottom - targetRect.bottom) + cardMargin + mainPadding;
  const height = visibleYamlEditorHeight(window.innerHeight, hostRect.top, spaceBelow);
  if (yamlEditor) yamlEditor.setSize(null, height);
  else target.style.height = `${height}px`;
}

function scheduleYamlEditorResize() {
  if (yamlResizeFrame) cancelAnimationFrame(yamlResizeFrame);
  yamlResizeFrame = requestAnimationFrame(resizeYamlEditor);
}

window.addEventListener('resize', scheduleYamlEditorResize);

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
    scheduleYamlEditorResize();
    return;
  }
  const fb = document.getElementById('yaml-editor-fallback');
  if (fb) {
    fb.value = v || '';
    scheduleYamlEditorResize();
  }
}

// ---------------------------------------------------------------------------
// Live validation + restart-required key hints (Raw YAML editor)
// ---------------------------------------------------------------------------

// yamlLint* backs the debounced POST /api/config/validate lint: yamlLintSeq
// discards stale responses, yamlLintErrors holds the last {line, message}
// list, and yamlSaving lets updateYamlSaveState arbitrate the Save button.
let yamlLintTimer = 0;
let yamlLintSeq = 0;
let yamlLintErrors = [];
let yamlSaving = false;
// yamlSavedText is the last disk-loaded (or successfully saved) YAML — the
// diff baseline for restart-required key detection.
let yamlSavedText = '';

// RESTART_KEYS are config keys a hot reload does NOT apply — edits only take
// effect after a daemon restart. Code facts: the request_log logger and the
// stats store are built once at startup (web-api.md request-log section,
// internal/app/stats_runtime.go initStats), quota_poll_interval is frozen into
// the tracker ticker at Start (pitfalls #29), the budgets watcher binds its
// config snapshot at startup (internal/app/budget_watch.go startBudgetWatcher),
// and listen/log/web bind into the process at boot. Keep in sync with
// docs/engineering/pitfalls.md.
const RESTART_KEYS = [
  { path: ['listen'], label: 'listen' },
  { path: ['log_level'], label: 'log_level' },
  { path: ['log_file'], label: 'log_file' },
  { path: ['web', 'enabled'], label: 'web.enabled' },
  { path: ['request_log'], label: 'request_log.*' },
  { path: ['stats', 'db_path'], label: 'stats.db_path' },
  { path: ['stats', 'retention'], label: 'stats.retention' },
  { path: ['budgets'], label: 'budgets' },
  { path: ['scheduling', 'quota_poll_interval'], label: 'scheduling.quota_poll_interval' },
];

// scanRestartKeyValues maps each RESTART_KEYS label present in text to a raw
// value string: the full block for top-level keys (so any edit under e.g.
// request_log: counts), or the child's trimmed line for nested keys. It is a
// line scanner, not a YAML parser — good enough for a hint, validation stays
// server-side.
function scanRestartKeyValues(text) {
  const blocks = new Map(); // top-level key → raw lines incl. the header line
  let current = null;
  for (const raw of (text || '').split('\n')) {
    const top = raw.match(/^([A-Za-z_][A-Za-z0-9_]*)\s*:/);
    if (top) {
      current = top[1];
      blocks.set(current, [raw]);
    } else if (current && /^[ \t]/.test(raw)) {
      blocks.get(current).push(raw);
    } else if (raw.trim() && !raw.trimStart().startsWith('#')) {
      current = null;
    }
  }
  const values = new Map();
  for (const { path, label } of RESTART_KEYS) {
    const block = blocks.get(path[0]);
    if (!block) continue;
    if (path.length === 1) {
      values.set(label, block.join('\n'));
      continue;
    }
    const childRe = new RegExp(`^\\s+${path[1]}\\s*:`);
    const child = block.slice(1).find((line) => childRe.test(line));
    if (child) values.set(label, child.trim());
  }
  return values;
}

// changedRestartKeys returns the RESTART_KEYS labels whose scanned value
// differs from the saved baseline (added/removed keys count as changed).
function changedRestartKeys() {
  const text = getYamlValue();
  if (text === null) return [];
  const now = scanRestartKeyValues(text);
  const saved = scanRestartKeyValues(yamlSavedText);
  const changed = [];
  for (const [label, value] of now) {
    if (saved.get(label) !== value) changed.push(label);
  }
  return changed;
}

function renderRestartHint() {
  const box = document.getElementById('yaml-restart-hint');
  if (!box) return;
  const changed = changedRestartKeys();
  if (!changed.length) {
    box.innerHTML = '';
    return;
  }
  box.innerHTML = `<div class="msg warn">ⓘ ${esc(changed.join(', '))} only take effect after a daemon restart — saving reloads the rest, but these need a restart.</div>`;
}

function scheduleYamlLint() {
  if (yamlLintTimer) clearTimeout(yamlLintTimer);
  yamlLintTimer = setTimeout(runYamlLint, 500);
}

async function runYamlLint() {
  yamlLintTimer = 0;
  const text = getYamlValue();
  if (text === null) return;
  const seq = ++yamlLintSeq;
  try {
    const res = await apiPost('/api/config/validate', { yaml: text });
    if (seq !== yamlLintSeq) return; // superseded by a newer edit
    yamlLintErrors = (res && res.errors) || [];
  } catch (e) {
    if (seq !== yamlLintSeq) return;
    // A transport failure must not block saving — the save path re-validates.
    yamlLintErrors = [];
  }
  renderYamlLint();
  renderRestartHint();
  updateYamlSaveState();
}

function renderYamlLint() {
  const box = document.getElementById('yaml-lint');
  if (!box) return;
  if (!yamlLintErrors.length) {
    box.innerHTML = '';
    return;
  }
  box.innerHTML = yamlLintErrors.map((issue) => {
    const where = issue.line > 0 ? `line ${issue.line}: ` : '';
    return `<div class="yaml-lint-err" data-line="${issue.line | 0}">${esc(where + issue.message)}</div>`;
  }).join('');
  box.querySelectorAll('.yaml-lint-err').forEach((el) => {
    el.addEventListener('click', () => jumpToYamlLine(parseInt(el.dataset.line, 10)));
  });
}

// jumpToYamlLine focuses the editor on a 1-based line (CodeMirror only).
function jumpToYamlLine(line) {
  if (!yamlEditor || !(line > 0)) return;
  const pos = { line: line - 1, ch: 0 };
  yamlEditor.setCursor(pos);
  yamlEditor.scrollIntoView(pos, 80);
  yamlEditor.focus();
}

function updateYamlSaveState() {
  const btn = document.getElementById('btn-yaml-save');
  if (btn) btn.disabled = yamlSaving || yamlLintErrors.length > 0;
}

async function renderConfigTab() {
  const panel = panels.config;
  panel.innerHTML =
    `<div id="config-summary" class="card"><div class="card-body"><span class="msg">loading…</span></div></div>
     <div class="card" id="preset-card">
       <header class="card-head"><h2>Add provider preset</h2></header>
       <div class="card-body">
         <div class="row-actions">
           <select id="preset-select" class="req-input" aria-label="Provider preset"></select>
           <button class="btn small" id="btn-preset-add">Add &amp; reload</button>
         </div>
         <div id="preset-msg" aria-live="polite"></div>
       </div>
     </div>
     <details class="editor" id="ed-provider"><summary>Provider scalars</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-route"><summary>Routes</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-settings"><summary>Settings (log / scheduling / request log / stats / cache)</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <div class="card" id="yaml-card">
       <header class="card-head"><h2>Raw YAML</h2><span class="meta" id="yaml-meta"></span></header>
       <div class="card-body">
         <div id="yaml-editor" class="yaml-cm-host"></div>
         <div id="yaml-lint" aria-live="polite"></div>
         <div id="yaml-restart-hint"></div>
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
  // Live validation: debounce edits into POST /api/config/validate. Loading
  // text via setValue fires CodeMirror 'change' too, so the initial lint comes
  // for free once loadConfigAll fills the editor.
  if (yamlEditor) yamlEditor.on('change', scheduleYamlLint);
  else {
    const fb = document.getElementById('yaml-editor-fallback');
    if (fb) fb.addEventListener('input', scheduleYamlLint);
  }
  scheduleYamlEditorResize();
  document.getElementById('btn-yaml-reload').addEventListener('click', loadConfigYAML);
  document.getElementById('btn-yaml-save').addEventListener('click', saveConfigYAML);
  document.getElementById('btn-preset-add').addEventListener('click', addPresetFromWizard);

  try {
    await loadConfigAll();
  } catch (e) {
    setConn('err');
    const sum = document.getElementById('config-summary');
    if (sum) showMsg(sum.querySelector('.card-body'), 'err', e.message);
  }
}

// --- Add provider preset wizard (S3) ---

// loadPresetSelect fills the wizard dropdown from GET /api/presets. Options
// carry the preset's model count; already-configured providers are marked so
// the wizard reads honestly (adding is still allowed — it's idempotent and
// the credential step may still be pending).
async function loadPresetSelect() {
  const sel = document.getElementById('preset-select');
  if (!sel) return;
  try {
    const data = await apiGet('/api/presets');
    const presets = (data && data.presets) || [];
    const configured = new Set(Object.keys((configCache && configCache.provider_models) || {}));
    sel.innerHTML = presets.map(p => {
      const known = configured.has(p.name) ? ' (already in config)' : '';
      return `<option value="${esc(p.name)}">${esc(p.name)} — ${p.models.length} models${known}</option>`;
    }).join('') || '<option value="">(no presets)</option>';
  } catch (e) {
    sel.innerHTML = '<option value="">(presets unavailable)</option>';
  }
}

// addPresetFromWizard POSTs /api/presets/<name>, surfaces the server's
// ambiguity warnings verbatim (backend message is authoritative), renders a
// reload failure as its own sentence (reload_warning is an error string,
// never a model name), then refreshes the whole Config tab (summary + YAML
// baseline + provider editors) so the merged block shows up everywhere at
// once.
async function addPresetFromWizard() {
  const sel = document.getElementById('preset-select');
  const msg = document.getElementById('preset-msg');
  const btn = document.getElementById('btn-preset-add');
  if (!sel || !sel.value) return;
  btn.disabled = true;
  try {
    const res = await apiPost('/api/presets/' + encodeURIComponent(sel.value));
    const warns = (res && res.warnings) || [];
    const reloadWarn = (res && res.reload_warning) || '';
    const parts = [];
    if (warns.length) {
      parts.push(`these models are now served by multiple providers: ${warns.join(', ')} — set each provider's priority to control failover order.`);
    }
    if (reloadWarn) {
      parts.push(`hot-reload failed: ${reloadWarn} — the block is saved; the daemon picks it up once config.yaml is fixed and reloaded.`);
    }
    if (parts.length) {
      showMsg(msg, 'warn', `Added, but ${parts.join(' Also, ')}`);
    } else {
      showMsg(msg, 'ok', `Preset ${sel.value} added. Next: add its credential in Accounts (or login).`);
    }
    await loadConfigAll();
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    btn.disabled = false;
  }
}

async function loadConfigAll() {
  const cfg = await apiGet('/api/config');
  configCache = cfg;
  // Fill the preset wizard select in the same pass (independent of the
  // config body; failures leave the select empty without breaking the tab).
  loadPresetSelect();
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
  setYamlValue(cfg.yaml || '');
  // Fresh baseline for restart-key diffing; the lint result for the loaded
  // text arrives via the 'change'-triggered debounce.
  yamlSavedText = cfg.yaml || '';
  yamlLintErrors = [];
  renderYamlLint();
  renderRestartHint();
  updateYamlSaveState();
  const meta = document.getElementById('yaml-meta');
  if (meta) meta.textContent = `${(cfg.yaml || '').length} bytes`;

  // Provider scalars form
  buildProviderForm('ed-provider');

  // Route form
  buildRouteForm('ed-route');

  // Scalar settings form (log level, scheduling, request_log, stats, cache)
  buildSettingsForm();
  scheduleYamlEditorResize();
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
       <span class="hint">Each row is one failover target. Provider options come from configured providers; model options from that provider's models. Priority lower = tried first; empty = inherit the provider's priority.</span>
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
  // A blank t.provider renders as the FIRST option (browser default), so the
  // model list must be built for that effective provider — otherwise the
  // select shows aqp while the model dropdown stays empty.
  const effProvider = t.provider || provs[0] || '';
  const models = modelsForProvider(effProvider);
  // ensure the current model is selectable even if not in the provider's list
  const modelSet = !t.model || models.includes(t.model) ? models : [...models, t.model];
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
    modelSel.innerHTML = ms.map((m) => `<option value="${esc(m)}">${esc(m)}</option>`).join('');
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

// --- Settings form (scalar config blocks) ---

// SETTINGS_GROUPS is the form spec for the scalar config blocks that used to be
// YAML-editor-only. Each group maps to one /api/config/edit kind; `restart:true`
// marks a field a hot reload does NOT apply (same code facts as RESTART_KEYS
// above: request_log logger and stats store are built once at startup).
// `placeholder` shows the code default for a key that is absent in the file.
const SETTINGS_GROUPS = [
  {
    kind: 'general', title: 'General', fields: [
      {
        key: 'log_level', label: 'log_level', type: 'select', options: ['debug', 'info', 'warn', 'error'], def: 'info', restart: true,
        help: 'Runtime log verbosity for the daemon log.',
      },
      {
        key: 'log_file', label: 'log_file', type: 'text', def: '/tmp/model-proxy.log', restart: true,
        help: 'Log file path (the pid file is derived from the same directory).',
      },
    ],
  },
  {
    kind: 'scheduling', title: 'Scheduling', fields: [
      {
        key: 'circuit_threshold', label: 'circuit_threshold', type: 'number', def: '3',
        help: 'Consecutive failover-eligible failures before a provider circuit opens.',
      },
      {
        key: 'circuit_cooldown', label: 'circuit_cooldown', type: 'text', def: '10m',
        help: 'How long the circuit stays open before a single half-open probe.',
      },
      {
        key: 'rate_limit_backoff', label: 'rate_limit_backoff', type: 'text', def: '60s',
        help: 'Skip duration after a transient 429 with no reset hint or Retry-After, then probe.',
      },
      {
        key: 'quota_cooldown', label: 'quota_cooldown', type: 'text', def: '1h',
        help: 'Skip duration after a 429 classified quota-exhausted with no reset hint. The daily class locks until midnight instead.',
      },
      {
        key: 'model_lockout', label: 'model_lockout', type: 'text', def: '10m',
        help: 'Lock a (provider, model) pair after a model-level failure: 404, model-denied, or an empty 200.',
      },
      {
        key: 'retry_wait', label: 'retry_wait', type: 'text', def: '10s',
        help: 'When every target is cooling down, wait up to this long for the earliest expiry and retry (at most twice). 0 disables the wait.',
      },
      {
        key: 'upstream_timeout', label: 'upstream_timeout', type: 'text', def: '1800s',
        help: 'Per-upstream-request timeout.',
      },
      {
        key: 'sticky_dwell', label: 'sticky_dwell', type: 'text', def: '10m',
        help: 'Minimum time on the chosen provider before re-evaluating (conversation stickiness).',
      },
      {
        key: 'quota_poll_interval', label: 'quota_poll_interval', type: 'text', def: '5m', restart: true,
        help: 'Background quota poll cadence.',
      },
      {
        key: 'quota_switch_margin', label: 'quota_switch_margin', type: 'number', def: '15',
        help: 'Switch provider when another one beats the current effective remaining by at least this many percentage points.',
      },
      {
        key: 'quality_error_weight', label: 'quality_error_weight', type: 'number', def: '100',
        help: 'Surplus penalty per unit error-rate EWMA (2m half-life), in percent (100 = 1.0), subtracted from the quota surplus. 0 disables the signal.',
      },
      {
        key: 'quality_ttft_weight', label: 'quality_ttft_weight', type: 'number', def: '20',
        help: 'Surplus penalty per unit normalized TTFT EWMA (10s reference), in percent (20 = 0.2). 0 disables the signal.',
      },
    ],
  },
  {
    kind: 'request_log', title: 'Request log', restart: true,
    note: 'request_log.* only takes effect after a daemon restart (the logger is built at startup).',
    fields: [
      {
        key: 'enabled', label: 'enabled', type: 'checkbox', def: 'false',
        help: 'Write each committed upstream call (request + response body) as one JSONL line to a rotating file. Off by default so the hot path stays free.',
      },
      {
        key: 'dir', label: 'dir', type: 'text', def: '~/.model-proxy/log/requests',
        help: 'Directory holding the daily rotating request-log files.',
      },
      {
        key: 'max_file_size', label: 'max_file_size (bytes)', type: 'number', def: '1073741824',
        help: 'Rotate to a new file when the next line would exceed this size (1 GiB).',
      },
      {
        key: 'max_body_bytes', label: 'max_body_bytes (bytes)', type: 'number', def: '5242880',
        help: 'Per-body capture cap (5 MiB). Larger bodies are truncated in the log but still forwarded intact.',
      },
      {
        key: 'retention', label: 'retention', type: 'text', def: '720h',
        help: 'Delete rotated files older than this (30d). 0 keeps them forever. The active file is never deleted.',
      },
    ],
  },
  {
    kind: 'stats', title: 'Stats', restart: true,
    note: 'stats.db_path / stats.retention only take effect after a daemon restart (the SQLite store is opened at startup).',
    fields: [
      {
        key: 'db_path', label: 'db_path', type: 'text', def: '~/.model-proxy/stats.db',
        help: 'SQLite database for per-provider x model x minute call statistics.',
      },
      {
        key: 'retention', label: 'retention', type: 'text', def: '720h',
        help: 'Delete minute buckets older than this (30d). 0 keeps history forever.',
      },
    ],
  },
  {
    kind: 'cache', title: 'Response cache',
    fields: [
      {
        key: 'enabled', label: 'enabled', type: 'checkbox', def: 'false',
        help: 'Replay a byte-identical request (SHA-256 of method + path + body) from the exact response cache, marked x-mp-cache: hit.',
      },
      {
        key: 'ttl', label: 'ttl', type: 'text', def: '10m',
        help: 'Cached entry lifetime.',
      },
      {
        key: 'max_entries', label: 'max_entries', type: 'number', def: '1000',
        help: 'Maximum live cache entries; least-recently-used entries are evicted first.',
      },
      {
        key: 'max_body_bytes', label: 'max_body_bytes (bytes)', type: 'number', def: '262144',
        help: 'Only responses at or below this size (256 KiB) are cached.',
      },
    ],
  },
];

// HELP_ICON_SVG is the inline info glyph used by every settings help button.
// currentColor keeps it on the theme (muted → accent on hover/open); an SVG
// avoids font-dependent glyph metrics and stays crisp at small sizes.
const HELP_ICON_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"><circle cx="8" cy="8" r="6.3"/><path d="M8 7.2v4.1"/><path d="M8 4.7h.01"/></svg>';

// buildSettingsForm renders every group into #ed-settings. Values come from the
// cached /api/config `settings` projection (null → empty input, so the
// placeholder's code default shows through).
function buildSettingsForm() {
  const host = document.querySelector('#ed-settings .editor-body');
  if (!host) return;
  // A re-render replaces the buttons the popover is anchored to; close it so it
  // cannot float over a detached control.
  closeSettingsHelp();
  const settings = (configCache && configCache.settings) || {};
  const blocks = SETTINGS_GROUPS.map((group) => {
    const values = settings[group.kind] || {};
    const fields = group.fields.map((field) => settingsFieldHTML(group, field, values[field.key])).join('');
    return `<section class="settings-group">
      <div class="section-title">${esc(group.title)}</div>
      <div class="grid cols-3 settings-grid">${fields}</div>
      ${group.note ? `<div class="settings-note">ⓘ ${esc(group.note)}</div>` : ''}
    </section>`;
  }).join('');
  host.innerHTML = `${blocks}
    <div class="row-actions settings-actions">
      <span class="spacer"></span>
      <button class="btn primary small" id="btn-settings-save">Apply settings</button>
    </div>
    <div class="msg" id="settings-msg"></div>`;
  document.getElementById('btn-settings-save').addEventListener('click', applySettings);
  wireSettingsHelp();
}

// settingsLabelHTML renders the field label plus its click-to-open (?) help
// button. The help text and default are carried in data attributes so the
// popover is built from the same spec that renders the control.
function settingsLabelHTML(id, group, field) {
  if (!field.help) return `<label for="${id}">${esc(field.label)}</label>`;
  return `<div class="field-head">
    <label for="${id}">${esc(field.label)}</label>
    <button type="button" class="help-btn" aria-expanded="false" aria-label="help: ${esc(group.kind)}.${esc(field.key)}" data-help="${esc(field.help)}" data-default="${esc(field.def || '')}">${HELP_ICON_SVG}</button>
  </div>`;
}

// settingsFieldHTML renders one control using the shared .field / .field.check
// control styles (never a bare input, which would render OS-native chrome).
// `def` is the code default: it is the placeholder for an absent key and the
// value shown in the (?) popover.
function settingsFieldHTML(group, field, value) {
  const id = `set-${group.kind}-${field.key}`;
  const text = value === null || value === undefined ? '' : String(value);
  const label = settingsLabelHTML(id, group, field);
  if (field.type === 'checkbox') {
    return `<div class="field check">
      <input type="checkbox" id="${id}"${value === true ? ' checked' : ''}>
      ${label}
    </div>`;
  }
  if (field.type === 'select') {
    const options = (field.options || []).map((opt) => `<option value="${esc(opt)}"${opt === text ? ' selected' : ''}>${esc(opt)}</option>`).join('');
    return `<div class="field">
      ${label}
      <select id="${id}">${options}</select>
    </div>`;
  }
  const type = field.type === 'number' ? 'number' : 'text';
  const placeholder = field.def ? ` placeholder="${esc(field.def)}"` : '';
  return `<div class="field">
    ${label}
    <input id="${id}" type="${type}" value="${esc(text)}"${placeholder} autocomplete="off">
  </div>`;
}

// settingsHelpEl is the single floating (?) popover shared by every field. It
// is created lazily and positioned with fixed coordinates from the button's
// viewport rect, so it escapes the details/overflow containers.
let settingsHelpEl = null;
let settingsHelpWired = false;

function wireSettingsHelp() {
  if (settingsHelpWired) return;
  settingsHelpWired = true;
  document.addEventListener('click', (event) => {
    const btn = event.target && event.target.closest ? event.target.closest('.help-btn') : null;
    if (btn) {
      event.preventDefault();
      toggleSettingsHelp(btn);
      return;
    }
    if (settingsHelpEl && !settingsHelpEl.hidden && !settingsHelpEl.contains(event.target)) closeSettingsHelp();
  });
  document.addEventListener('keydown', (event) => { if (event.key === 'Escape') closeSettingsHelp(); });
  window.addEventListener('resize', closeSettingsHelp);
  window.addEventListener('scroll', closeSettingsHelp, true);
}

function closeSettingsHelp() {
  if (!settingsHelpEl || settingsHelpEl.hidden) return;
  settingsHelpEl.hidden = true;
  const open = document.querySelector('.help-btn[aria-expanded="true"]');
  if (open) open.setAttribute('aria-expanded', 'false');
}

// toggleSettingsHelp opens the popover for `btn` (or closes it when the same
// button is clicked again). Content is the spec's help text plus the code
// default, so a field can never show help without a default and vice versa.
function toggleSettingsHelp(btn) {
  if (!settingsHelpEl) {
    settingsHelpEl = document.createElement('div');
    settingsHelpEl.className = 'help-popover';
    settingsHelpEl.setAttribute('role', 'dialog');
    settingsHelpEl.setAttribute('aria-label', 'setting help');
    settingsHelpEl.hidden = true;
    document.body.appendChild(settingsHelpEl);
  }
  const alreadyOpen = !settingsHelpEl.hidden && btn.getAttribute('aria-expanded') === 'true';
  if (alreadyOpen) {
    closeSettingsHelp();
    return;
  }
  closeSettingsHelp();
  const def = btn.dataset.default || '';
  settingsHelpEl.innerHTML = `<p>${esc(btn.dataset.help || '')}</p>
    ${def ? `<p class="help-popover-def">default: <code>${esc(def)}</code></p>` : ''}`;
  settingsHelpEl.hidden = false;
  btn.setAttribute('aria-expanded', 'true');
  positionSettingsHelp(btn);
}

// positionSettingsHelp anchors the popover under the button, flipping above it
// and clamping to the viewport when there is no room below.
function positionSettingsHelp(btn) {
  const margin = 8;
  const rect = btn.getBoundingClientRect();
  const box = settingsHelpEl.getBoundingClientRect();
  let left = rect.left;
  if (left + box.width > window.innerWidth - margin) left = window.innerWidth - margin - box.width;
  if (left < margin) left = margin;
  let top = rect.bottom + 6;
  if (top + box.height > window.innerHeight - margin) top = rect.top - box.height - 6;
  if (top < margin) top = margin;
  settingsHelpEl.style.left = `${left}px`;
  settingsHelpEl.style.top = `${top}px`;
}

// readSettingsForm reads the controls back into the settings shape keyed by
// edit kind (checkbox → boolean, everything else → string).
function readSettingsForm() {
  const out = {};
  for (const group of SETTINGS_GROUPS) {
    const data = {};
    for (const field of group.fields) {
      const input = document.getElementById(`set-${group.kind}-${field.key}`);
      if (!input) continue;
      data[field.key] = field.type === 'checkbox' ? input.checked : input.value;
    }
    out[group.kind] = data;
  }
  return out;
}

// applySettings POSTs one /api/config/edit per changed kind (only changed fields
// are sent, see settingsDiff), then refreshes the tab so the YAML editor and
// every form show the persisted state.
async function applySettings() {
  const btn = document.getElementById('btn-settings-save');
  const diff = settingsDiff((configCache && configCache.settings) || {}, readSettingsForm());
  const kinds = Object.keys(diff);
  if (!kinds.length) {
    showMsg(document.getElementById('settings-msg'), 'ok', 'no changes');
    return;
  }
  const restart = settingsRestartKeys(diff, SETTINGS_GROUPS);
  btn.disabled = true;
  showMsg(document.getElementById('settings-msg'), 'ok', 'saving…');
  const applied = [];
  try {
    for (const kind of kinds) {
      await apiPost('/api/config/edit', { kind, data: diff[kind] });
      applied.push(kind);
    }
  } catch (e) {
    // A multi-kind apply can fail halfway; resync so the form and YAML show
    // exactly what was persisted, then report the backend error verbatim.
    await loadConfigAll().catch(() => {});
    const msg = document.getElementById('settings-msg');
    const partial = applied.length ? ` (applied: ${applied.join(', ')})` : '';
    showMsg(msg, 'err', e.message + partial);
    return;
  } finally {
    btn.disabled = false;
  }
  await loadConfigAll();
  const msg = document.getElementById('settings-msg');
  if (restart.length) {
    showMsg(msg, 'warn', `saved & reloaded — ${restart.join(', ')} only take effect after a daemon restart.`);
  } else {
    showMsg(msg, 'ok', 'saved & reloaded');
  }
}

async function loadConfigYAML() {
  const meta = document.getElementById('yaml-meta');
  const msg = document.getElementById('yaml-msg');
  try {
    const cfg = await apiGet('/api/config');
    configCache = cfg;
    setYamlValue(cfg.yaml || '');
    yamlSavedText = cfg.yaml || '';
    yamlLintErrors = [];
    renderYamlLint();
    renderRestartHint();
    updateYamlSaveState();
    if (meta) meta.textContent = `${(cfg.yaml || '').length} bytes`;
    if (msg) showMsg(msg, 'ok', 'reloaded from disk');
  } catch (e) {
    if (msg) showMsg(msg, 'err', e.message);
  }
}

async function saveConfigYAML() {
  const msg = document.getElementById('yaml-msg');
  const yaml = getYamlValue();
  if (yaml === null) return; // no editor mounted
  if (yamlLintErrors.length) {
    showMsg(msg, 'err', 'fix the validation errors above before saving');
    return;
  }
  yamlSaving = true;
  updateYamlSaveState();
  showMsg(msg, 'ok', 'validating + reloading…');
  try {
    await apiPost('/api/config', { yaml });
    yamlSavedText = yaml;
    showMsg(msg, 'ok', 'saved & reloaded');
    await loadConfigAll();
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    yamlSaving = false;
    updateYamlSaveState();
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

// testAccount sends ONE real end-to-end probe through this account's credential
// (POST /api/accounts/<provider>/<id>/test — the same probe `models refresh`
// uses) and reports the result inline in #acc-msg. Read-only server-side (no
// reload, no state change), so the pane is NOT re-rendered and the button is
// simply restored when the call settles. The id is the same one the Remove
// button passes (pool id; AccountID for aqp/codex).
async function testAccount(btn, p) {
  const id = btn.dataset.test;
  if (!id) return;
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = 'testing…';
  const msg = document.getElementById('acc-msg');
  try {
    const r = await apiPost(`/api/accounts/${encodeURIComponent(p.name)}/${encodeURIComponent(id)}/test`);
    if (r.status === 'ok') {
      showMsg(msg, 'ok', `${p.name} — HTTP ${r.http_status} in ${r.latency_ms}ms (${r.model})`);
    } else {
      // http_status 0 = build/auth/network error (no upstream answer).
      const head = r.http_status ? `HTTP ${r.http_status}: ` : '';
      showMsg(msg, 'err', `${p.name} — ${head}${r.reason || 'probe failed'} (${r.model})`);
    }
  } catch (e) {
    showMsg(msg, 'err', `${p.name} — test failed: ${e.message}`);
  } finally {
    btn.disabled = false;
    btn.textContent = orig;
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
  // Capture each <details> section's open/closed state BEFORE the re-render
  // wipes them, so we can restore it after (e.g. a Refresh-usage click re-renders
  // the pane - a collapsed Usage section should stay collapsed, an expanded one
  // stay expanded). Sections with data default to open (the `open` attribute in
  // accountUsageDetails/accountTokensDetails; empty ones default to collapsed);
  // this restore only kicks in for a re-render where the user changed a
  // section's state.
  const secOpen = {};
  main.querySelectorAll('details.acct-section').forEach((d) => {
    secOpen[(d.dataset.acct || '') + '/' + (d.dataset.sec || '')] = d.open;
  });
  main.innerHTML = renderProviderDetail(p, accountsQuota, accountsTokens);
  main.querySelectorAll('details.acct-section').forEach((d) => {
    const k = (d.dataset.acct || '') + '/' + (d.dataset.sec || '');
    if (k in secOpen) d.open = secOpen[k];
  });
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
  // Per-account "Test" - one real end-to-end probe through this account's
  // credential; result reported inline (no re-render).
  main.querySelectorAll('[data-test]').forEach((b) => {
    b.addEventListener('click', () => testAccount(b, p));
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
  // pay-as-you-go providers have no Quota() to poll, so the Refresh-usage button
  // (which re-polls quota) is meaningless for them — hide it. Plan/quota
  // providers keep it.
  const refreshBtn = p.billing === 'pay-as-you-go' ? ''
    : `<button class="btn small" data-refresh="${esc(key)}" title="Re-poll this account's quota now">Refresh usage</button>`;
  return `<section class="card acct-card">
    <div class="account-row acct-card-head">
      <div>
        <div class="acct-label">${esc(label)}</div>
        <div class="acct-sub">${esc(sub)}${added}</div>
        ${mail}
      </div>
      <div class="row-actions">
        <button class="btn small" data-test="${esc(a.id)}"
                title="Send a real end-to-end probe request through this account">Test</button>
        ${refreshBtn}
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
  // Default the section to collapsed when there is no snapshot at all ("no
  // data") - an error snapshot stays open so the Re-login action is visible.
  const openAttr = snap ? ' open' : '';
  return `<details class="acct-section" data-acct="${esc(acctKey)}" data-sec="usage"${openAttr}>
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
  // Default the section to collapsed when there are no token rows ("no usage").
  const openAttr = rows.length ? ' open' : '';
  return `<details class="acct-section" data-acct="${esc(acctKey)}" data-sec="tokens"${openAttr}>
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
    // Burn-rate exhaustion prediction (QuotaSnapshot.ExhaustionEta, set by the
    // quota tracker; absent → no valid prediction: first poll, flat usage, or
    // a poll gap). Ultimate window only, appended to the same meta line.
    const etaMs = (w.Ultimate && snap.ExhaustionEta) ? new Date(snap.ExhaustionEta).getTime() - Date.now() : 0;
    const eta = etaMs > 0 ? `exhausts in ~${esc(fmtDur(etaMs / 1000))} at current rate` : '';
    const meta = [reset, eta].filter(Boolean).join(' · ');
    bars += `<div class="bar-row">
      <div class="bar-label">
        <span class="name">${esc(w.Label || 'quota')}${esc(ulg)}</span>
        <span class="pct">${p == null ? (w.Total > 0 ? fmtNum(w.Total) : '-') : (p * 100).toFixed(1) + '%'}</span>
      </div>
      <div class="bar-track"><div class="bar-fill ${fillCls}" style="width:${p == null ? 0 : Math.max(0, Math.min(1, p)) * 100}%"></div></div>
      ${meta ? `<div class="bar-meta">${meta}</div>` : ''}
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
    const data = await apiDel(`/api/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}`);
    const accMsg = document.getElementById('acc-msg');
    if (data && data.warning) {
      showMsg(accMsg, 'warn', `Removed from disk, but not live yet: ${data.warning} — fix config.yaml and reload. See logs.`);
    } else {
      showMsg(accMsg, 'ok', 'removed — reloading');
    }
    await renderAccountsTab();
    if (!(data && data.warning)) clearMsg(accMsg);
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
    const data = await apiPost(`/api/accounts/${encodeURIComponent(providerName)}`, body);
    // Clear secrets from the DOM immediately on success.
    document.getElementById('add-apikey').value = '';
    const ak = document.getElementById('add-accesskey'); if (ak) ak.value = '';
    const sk = document.getElementById('add-secretkey'); if (sk) sk.value = '';
    if (data && data.warning) {
      // Account saved to disk, but the in-process reload failed (config.yaml
      // unreadable/invalid) — runtime keeps the old set until config is fixed +
      // reloaded. Keep the modal open so the warning is read; see logs.
      showMsg(msg, 'warn', `Saved, but not live yet: ${data.warning} — fix config.yaml and reload. See logs.`);
      return;
    }
    showMsg(msg, 'ok', 'added — reloading');
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
          ${r.warning ? `<p class="modal-status err">Saved, but not live yet: ${esc(r.warning)} — fix config.yaml and reload. See logs.</p>` : ''}
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
// ANALYTICS TAB
// ===========================================================================
//
// Renders the Analytics tab: range/granularity/provider/model controls, then
// fetches /api/analytics and draws two uPlot trend charts (tokens + equivalent
// cost), a cost-only summary table, and an unpriced-models hint when some
// series have no configured price. Per-request/token totals live on the Status
// page (Token usage), so this tab is deliberately trends + cost only. The chart
// data binding matches the /api/analytics JSON shape:
//   series[].points[].{bucket,requests,input,output,cache_creation,cache_read,cost,priced}
//   price_coverage.{priced,unpriced}
// Control selections persist to localStorage so a refresh keeps the view.

// analyticsState reads the tab's control selections from localStorage (with
// sane defaults). Returns {range, gran, provider, model}.
function analyticsState() {
  return {
    range: localStorage.getItem('an-range') || '30',
    gran: localStorage.getItem('an-gran') || 'day',
    provider: localStorage.getItem('an-provider') || '',
    model: localStorage.getItem('an-model') || '',
  };
}

// analyticsSave persists one control value. Wrap in try/catch so private-mode
// browsers (where localStorage throws) don't break the tab.
function analyticsSave(name, val) {
  try { localStorage.setItem('an-' + name, val); } catch (_) { /* ignore */ }
}

// renderAnalyticsTab fetches /api/analytics and renders token + equivalent-cost
// trend charts (uPlot), a summary table, and an unpriced-models hint.
async function renderAnalyticsTab() {
  const panel = panels.analytics;
  if (!panel) return;
  destroyAnalyticsCharts(); // the innerHTML reset below drops the chart DOM
  const state = analyticsState();
  panel.innerHTML = `
    <div class="analytics-controls">
      <label>Range
        <select id="an-range">
          <option value="7">7d</option><option value="30">30d</option>
          <option value="90">90d</option><option value="365">all</option>
        </select>
      </label>
      <label>Granularity
        <select id="an-gran">
          <option value="day">Day</option><option value="month">Month</option>
        </select>
      </label>
      <label>Provider
        <input id="an-provider" placeholder="provider" list="an-provider-list" />
      </label>
      <datalist id="an-provider-list"></datalist>
      <label>Model
        <input id="an-model" placeholder="model" list="an-model-list" />
      </label>
      <datalist id="an-model-list"></datalist>
      <button id="an-refresh" class="btn small" type="button">Refresh</button>
    </div>
    <div id="an-error" class="msg err" hidden></div>
    <div id="an-unpriced" class="an-hint" hidden></div>
    <div class="an-charts">
      <div id="an-token-chart" class="an-chart"></div>
      <div id="an-cost-chart" class="an-chart"></div>
    </div>
    <div id="an-cost-table" class="an-cost-table"></div>`;
  const elRange = panel.querySelector('#an-range');
  const elGran = panel.querySelector('#an-gran');
  const elProvider = panel.querySelector('#an-provider');
  const elModel = panel.querySelector('#an-model');
  elRange.value = state.range;
  elGran.value = state.gran;
  elProvider.value = state.provider;
  elModel.value = state.model;
  // Persist on change + re-render so the new selection takes effect immediately.
  elRange.onchange = () => { analyticsSave('range', elRange.value); renderAnalyticsTab(); };
  elGran.onchange = () => { analyticsSave('gran', elGran.value); renderAnalyticsTab(); };
  elProvider.onchange = () => { analyticsSave('provider', elProvider.value); renderAnalyticsTab(); };
  elModel.onchange = () => { analyticsSave('model', elModel.value); renderAnalyticsTab(); };
  panel.querySelector('#an-refresh').onclick = () => { renderAnalyticsTab(); };

  const from = Math.floor((Date.now() - Number(state.range) * 86400 * 1000) / 1000);
  const to = Math.floor(Date.now() / 1000);
  const q = new URLSearchParams({ from: String(from), to: String(to), granularity: state.gran });
  if (state.provider) q.set('provider', state.provider);
  if (state.model) q.set('model', state.model);
  let resp;
  const errEl = panel.querySelector('#an-error');
  try {
    resp = await apiGet('/api/analytics?' + q.toString());
    if (errEl) errEl.hidden = true;
  } catch (e) {
    if (errEl) {
      errEl.hidden = false;
      errEl.textContent = 'analytics unavailable: ' + e.message;
    }
    return;
  }
  analyticsFillDatalists(panel, resp, state.provider);
  analyticsRenderHints(panel, resp);
  analyticsRenderCharts(panel, resp);
  analyticsRenderCostTable(panel, resp);
}

// analyticsRenderHints surfaces the unpriced-models hint when /api/analytics
// reports models with no configured price. The hint nudges the operator toward
// adding a `prices:` entry, since equivalent-cost totals silently exclude
// unpriced series.
function analyticsRenderHints(panel, resp) {
  const el = panel.querySelector('#an-unpriced');
  if (!el) return;
  const un = (resp && resp.price_coverage && resp.price_coverage.unpriced) || [];
  if (un.length) {
    el.hidden = false;
    el.textContent = un.length + ' model(s) unpriced (no equivalent cost): ' + un.join(', ') +
      '. Add a `prices:` entry in config to price them.';
  } else {
    el.hidden = true;
    el.textContent = '';
  }
}

// analyticsChartColors reads the categorical chart palette from the :root CSS
// variables (the only place colors are defined). Falls back to a single hue if
// the stylesheet is unavailable.
function analyticsChartColors() {
  const cs = getComputedStyle(document.documentElement);
  const out = [];
  for (let i = 0; i < 10; i++) {
    const v = cs.getPropertyValue('--chart-' + i).trim();
    if (v) out.push(v);
  }
  return out.length ? out : ['#2563eb'];
}

// analyticsCharts holds the live uPlot instances so a re-render destroys the
// previous ones instead of leaking them.
let analyticsCharts = [];

// destroyAnalyticsCharts tears down the charts rendered for the previous view.
function destroyAnalyticsCharts() {
  for (const u of analyticsCharts) {
    try { u.destroy(); } catch (_) { /* already detached */ }
  }
  analyticsCharts = [];
}

// analyticsFillDatalists populates the provider/model suggestion lists from the
// response so the free-text filters are usable. The provider→models map only
// grows within a session: a narrowed response must not erase options the user
// can switch back to.
const analyticsFacetModels = new Map();
function analyticsFillDatalists(panel, resp, provider) {
  for (const s of ((resp && resp.series) || [])) {
    if (!s || !s.provider) continue;
    let set = analyticsFacetModels.get(s.provider);
    if (!set) { set = new Set(); analyticsFacetModels.set(s.provider, set); }
    set.add(s.model);
  }
  const providers = [...analyticsFacetModels.keys()].sort();
  const models = linkedModels(provider || '', Object.fromEntries([...analyticsFacetModels].map(([p, set]) => [p, [...set]])), {});
  const pList = panel.querySelector('#an-provider-list');
  const mList = panel.querySelector('#an-model-list');
  if (pList) pList.innerHTML = providers.map((p) => `<option value="${esc(p)}"></option>`).join('');
  if (mList) mList.innerHTML = models.map((m) => `<option value="${esc(m)}"></option>`).join('');
}

// analyticsRenderCharts draws the token + equivalent-cost trend charts with
// uPlot. Each (provider,model) series becomes one line, sharing a color across
// both charts. x is in unix seconds — uPlot's time unit — and every series gets
// an explicit stroke because uPlot 1.6.x does not auto-assign colors (a missing
// stroke renders the axes and legend but no line).
function analyticsRenderCharts(panel, resp) {
  if (typeof uPlot === 'undefined') return; // vendored script failed to load
  const tokenHost = panel.querySelector('#an-token-chart');
  const costHost = panel.querySelector('#an-cost-chart');
  if (!tokenHost || !costHost) return;
  destroyAnalyticsCharts();
  tokenHost.innerHTML = '';
  costHost.innerHTML = '';
  const token = analyticsChartSeries(resp && resp.series, 'tokens');
  const cost = analyticsChartSeries(resp && resp.series, 'cost');
  if (!token.x.length) return;
  const colors = analyticsChartColors();
  const tokenSeries = [{ label: 'time' }];
  const costSeries = [{ label: 'time' }];
  token.labels.forEach((label, i) => {
    const stroke = colors[i % colors.length];
    tokenSeries.push({ label, stroke, width: 1.5, points: { show: false } });
    costSeries.push({ label, stroke, width: 1.5, points: { show: false } });
  });
  const baseOpts = (host, title, yLabel) => ({
    title,
    width: Math.max(host.clientWidth || 600, 320),
    height: 220,
    series: [],
    scales: { x: { time: true } },
    axes: [{}, { label: yLabel, size: 60 }],
    legend: { show: true, live: false },
  });
  const tokenOpts = baseOpts(tokenHost, 'Tokens (input + output)', 'tokens');
  tokenOpts.series = tokenSeries;
  try { analyticsCharts.push(new uPlot(tokenOpts, [token.x, ...token.ys], tokenHost)); } catch (_) { /* malformed data */ }
  const costOpts = baseOpts(costHost, 'Equivalent cost (USD)', 'USD');
  costOpts.series = costSeries;
  try { analyticsCharts.push(new uPlot(costOpts, [cost.x, ...cost.ys], costHost)); } catch (_) { /* malformed data */ }
}

// analyticsRenderCostTable renders the cost view Token Usage does not have: one
// row per (provider,model) with the window's equivalent cost and its share of
// the priced total. Token/request totals deliberately stay on the Status page,
// so the two views do not duplicate each other.
function analyticsRenderCostTable(panel, resp) {
  const host = panel.querySelector('#an-cost-table');
  if (!host) return;
  const rows = ((resp && resp.series) || []).map((s) => {
    let cost = null;
    for (const p of s.points) {
      if (p.cost != null) cost = (cost || 0) + p.cost;
    }
    return { provider: s.provider, model: s.model, cost };
  });
  const total = rows.reduce((sum, r) => sum + (r.cost || 0), 0);
  rows.sort((a, b) => (b.cost || 0) - (a.cost || 0));
  const body = rows.map((r) => `<tr>
      <td class="mono">${esc(r.provider)}</td>
      <td class="mono">${esc(r.model)}</td>
      <td class="num">${r.cost == null ? 'n/a' : '$' + r.cost.toFixed(4)}</td>
      <td class="num">${r.cost == null || total <= 0 ? '—' : (r.cost / total * 100).toFixed(1) + '%'}</td>
    </tr>`).join('');
  host.innerHTML = `<table class="table">
      <thead><tr><th>provider</th><th>model</th><th class="num">equivalent cost</th><th class="num">share</th></tr></thead>
      <tbody>${body || '<tr><td colspan="4" class="hint">no series in range</td></tr>'}</tbody>
    </table>`;
}

// ===========================================================================
// boot
// ===========================================================================

// Wire the persistent #login-cancel button (defined in the static HTML).
const loginCancelStatic = document.getElementById('login-cancel');
if (loginCancelStatic) loginCancelStatic.addEventListener('click', closeLoginModal);
// Esc closes a <dialog> via the `cancel` event, bypassing closeLoginModal —
// clear the poll timer there too so polling doesn't run behind a closed modal.
const loginModalEl = document.getElementById('login-modal');
if (loginModalEl) loginModalEl.addEventListener('cancel', () => {
  if (loginPollTimer) { clearInterval(loginPollTimer); loginPollTimer = null; }
});

async function boot() {
  await establishAdminSessionIfRequired();

  // Initial render: activate the tab the URL hash names (so a refresh or shared
  // link lands on the same view), defaulting to Status. For
  // #accounts/<provider>, preset the selection before the fetch.
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
  } else if (bootTab === 'analytics') {
    activateTabSilent('analytics');
  } else if (bootTab === 'security') {
    activateTabSilent('security');
  } else {
    activateTabSilent('status');
  }
  // Update the header connection indicator regardless of the landing tab.
  refreshConnIndicator();
}

boot();
