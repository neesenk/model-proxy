// model-proxy admin SPA (v2 design system). Vanilla JS module — no
// framework, no CDN. Behavior core (SSE, deferAutoRefresh interaction gate,
// comboboxes, YAML editor, charts) plus the v2 presentation layer — semantic
// status badges, SVG icons, KPI delta coloring, tokenized log coloring.
// Pure helpers live in pure.js (single source, behavior-tested by
// jstests/pure.test.mjs).
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
  verdictBadge, modelCapMatrix, providerCapsSummary, providerFrozen, providerNames, cacheHitRate,
  settingsDiff, settingsRestartKeys, TOKEN_RANGES, tokensRangeQuery, tokenRangeLabel,
  tokenRangeTriggerLabel, parseLocalDate, tokenRangeBounds, tokenCustomBounds,
  WEEKDAYS, monthTitle, calendarMonthGrid, twoMonthWindow, shiftMonth, ymd, isFutureDay, rangePick,
  parseSSE, isSSE, prettyJSON, formatJSONLoose, highlightJSON, splitLinesByBudget, linkedModels,
  sessionsForAgent, linkedAgents,
  analyticsChartSeries, analyticsTableRows, ANALYTICS_METRICS, pctDelta,
  analyticsGranularity, analyticsGranOptions, analyticsValueText, modelHealthFromSeries, fmtCompact,
  HEAT_DAYS, analyticsHeatLevel, analyticsYearGrid, analyticsYearMonthSpans, analyticsHeatCellSize, analyticsHeatTipLines,
  analyticsRowSortKey, ANALYTICS_TABLE_SORT, analyticsSortRows,
  analyticsMetricOptions, analyticsMetricAllowed,
  liveSessionSummary, liveSessionOrder, shortSessionId, ruleHitsLeaderboard, sessionTimeline, sessionBarSummary, responseExcerpt, requestExcerpt, chatViewHTML, parseChatRequest, chatTurnsSliceHTML, CHAT_RECENT, requestRowHTML, requestTableHeadHTML, sessionHealthSummary,
  cumulativeOffsets, virtualWindow, mergeRecordsPages, oldestTsSec,
  hashQueryParams, requestsFilterQuery, requestsFilterFromQuery,
  fmtGuardDetail, fmtProgressBytes, mergeLiveAndPersistedRow, shouldFetchDetail,
  detailFetchState, quotaErrKind, accountUsageState,
  pathStrengthFromAction, securityLegendHTML, securityExplainHTML, securityKpisHTML, mergeSecurityFeed,
  SECURITY_RANGES, securityRangeFromSecs, securityFilterQuery, securityFilterFromQuery, explainCacheKey,
  POPUP_OPEN_SEL, INTERACTIVE_CONTROL_SEL, refreshHoldReason, staleDataText,
  iconPin, iconRefresh, iconChevron, statusBadgeHTML, kpiDeltaClass, logLineHTML,
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
//   #requests?session=…&agent=…   (Requests tab filter — refresh/shared link
//                                  lands on the same view; only non-default
//                                  filter values ride along)
//   #status/live?session=…        (Status → Live section with one session
//                                  selected — same refresh/restore story)
//   #security?kind=…&verdict=…&range=…&rule=…
//                                 (Security tab filter — same story: kind is
//                                  the server-side audit query filter, the
//                                  rest narrow the merged feed client-side)
//
// activateTab/selectProvider push the hash; a hashchange listener (browser
// back/forward) re-activates without pushing, so the two stay in sync without a
// feedback loop. An unrecognized/empty hash defaults to #status.

function parseHash() {
  const raw = (location.hash || '').replace(/^#\/?/, ''); // drop leading "#"/"#/"
  const [path, queryRaw] = raw.split('?');
  const [tab, ...rest] = path.split('/');
  if (tab === 'config' || tab === 'accounts' || tab === 'status' || tab === 'analytics' || tab === 'requests' || tab === 'security') {
    // decodeURIComponent so provider/section names with special chars
    // round-trip; a malformed sequence decodes to "" (treated as "no sub" ->
    // first provider / default section). For #status/<section>, sub is the
    // section key read by selectStatusSectionSilent. The query portion feeds
    // the Requests filter (hashQueryParams).
    let sub = '';
    try { sub = decodeURIComponent(rest.join('/')); } catch (_) { sub = ''; }
    return { tab, sub, query: hashQueryParams(queryRaw) };
  }
  return { tab: 'status', sub: '', query: {} };
}

// statusHash builds the Status URL hash: the active section plus — when the
// Live section has a session selected — that session as a query param, so a
// refresh or shared link lands on the same live session view. Callers render
// BEFORE hashing: mounting the Live card resets the selection, and the hash
// must reflect the post-render truth.
function statusHash() {
  let h = '#status/' + statusSelected;
  if (statusSelected === 'live' && liveSessionFilter) {
    h += '?session=' + encodeURIComponent(liveSessionFilter);
  }
  return h;
}

// requestsHash builds the Requests URL hash from the live filter; only
// non-default values ride along so an unfiltered tab stays a clean #requests.
// A live request-drill pin (the Security page's "view the original request"
// link) rides along too, so filter refinement does not drop the pinned view.
function requestsHash() {
  const parts = [requestsFilterQuery(requestsFilter)];
  if (requestDrill) {
    parts.push('request=' + encodeURIComponent(requestDrill.request));
    if (requestDrill.kind) parts.push('kind=' + encodeURIComponent(requestDrill.kind));
    if (requestDrill.name) parts.push('name=' + encodeURIComponent(requestDrill.name));
  }
  const q = parts.filter(Boolean).join('&');
  return '#requests' + (q ? '?' + q : '');
}

// securityHash builds the Security URL hash from the live filter (same
// only-non-defaults rule — an unfiltered tab stays a clean #security).
function securityHash() {
  const q = securityFilterQuery(securityFilter);
  return '#security' + (q ? '?' + q : '');
}

// updateSecurityHash mirrors filter changes into the URL (replaceState, like
// updateRequestsHash — in-tab refinement must not spam history).
function updateSecurityHash() {
  if (activeTab === 'security') setHash(securityHash(), false);
}

// updateRequestsHash mirrors filter changes into the URL (replaceState, like
// the Accounts provider pin — in-tab refinement must not spam history).
function updateRequestsHash() {
  if (activeTab === 'requests') setHash(requestsHash(), false);
}

// syncRequestsFreeControls pushes the filter state into the free-form
// controls (provider/model inputs, errors checkbox, shadow select). The
// linked selects are repainted by renderRequestSelectors, but these four
// hold their DOM value across re-renders — after a hash-driven filter change
// (back/forward) they must follow, or the next Refresh would read the stale
// values back into the filter.
function syncRequestsFreeControls() {
  const p = document.getElementById('req-provider');
  if (p) p.value = requestsFilter.provider;
  const m = document.getElementById('req-model');
  if (m) m.value = requestsFilter.model;
  const e = document.getElementById('req-errors');
  if (e) e.checked = !!requestsFilter.errors;
  const sh = document.getElementById('req-shadow');
  if (sh) sh.value = requestsFilter.shadow;
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
    securityStopAutoRefresh();
    accountsStopAutoRefresh();
    renderStatusTab();
  } else {
    stopStatusRefresh();
    stopLiveEvents();
    securityStopAutoRefresh();
    accountsStopAutoRefresh();
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
  if (name === 'status') setHash(statusHash(), true);
  else if (name === 'requests') setHash(requestsHash(), true);
  else if (name === 'security') setHash(securityHash(), true);
  else if (name !== 'accounts') setHash(tabHash(name), true);
}

for (const b of tabBtns) {
  b.addEventListener('click', () => activateTab(b.dataset.tab));
}

// hashchange: browser back/forward (or manual hash edit) drives the view. Apply
// the hash's tab + (Accounts) provider WITHOUT pushing back, avoiding a loop.
window.addEventListener('hashchange', () => {
  const { tab, sub, query } = parseHash();
  // Seed the Requests filter BEFORE activation: the first mount templates the
  // free inputs from it and the re-entry path loads through it. Same for the
  // Security filter (its first mount templates the selects from it).
  const nextFilter = tab === 'requests' ? requestsFilterFromQuery(query) : null;
  if (nextFilter) requestsFilter = nextFilter;
  if (tab === 'requests') requestDrill = requestDrillFromQuery(query);
  const secSeed = tab === 'security' ? securityFilterFromQuery(query) : null;
  if (secSeed) securityFilter = secSeed;
  const switched = tab !== activeTab;
  if (switched) {
    activateTabSilent(tab);
  }
  if (tab === 'accounts' && sub) {
    selectProviderSilent(sub);
  }
  if (tab === 'status' && sub) {
    selectStatusSectionSilent(sub);
  }
  if (tab === 'status' && sub === 'live') {
    // Apply the hash's session (empty clears to all-live). When the live
    // card is already mounted a direct apply is safe; when it is not (first
    // entry to Status — the tab renders async through its fetches), apply
    // via the bootLiveSession pending that renderLiveCard consumes on
    // mount, or the mount's selection reset would wipe the hash session —
    // the same race boot fixed.
    const liveSess = query.session || '';
    if (document.getElementById('live-session')) {
      if (liveSessionFilter !== liveSess) onLiveSessionChange(liveSess);
    } else {
      bootLiveSession = liveSess;
    }
  }
  if (tab === 'requests' && nextFilter) {
    // After a tab switch renderRequestsTab already rendered through the
    // seeded filter; the free controls still need the sync (the re-entry
    // path does not rebuild them). Without a switch this IS the reload.
    syncRequestsFreeControls();
    if (!switched && requestsCombos && document.getElementById('req-table')) {
      renderRequestSelectors(requestsCombos);
      loadRequests(requestsCombos);
    }
  }
  if (tab === 'requests') applyRequestDrill();
  if (tab === 'security' && secSeed) {
    // Mirror image of the requests branch: after a switch the tab render
    // already ran through the seeded filter; without one (an in-tab hash
    // edit / leaderboard drill) sync the controls and reload in place.
    syncSecurityControls();
    if (!switched && document.getElementById('sec-table')) {
      loadSecurity();
      renderSecurityFeed();
    }
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
    securityStopAutoRefresh();
    accountsStopAutoRefresh();
    renderStatusTab();
  } else {
    stopStatusRefresh();
    stopLiveEvents();
    securityStopAutoRefresh();
    accountsStopAutoRefresh();
  }
  if (name === 'config') renderConfigTab();
  if (name === 'accounts') renderAccountsTab();
  if (name === 'analytics') renderAnalyticsTab();
  if (name === 'requests') renderRequestsTab();
  if (name === 'security') renderSecurityTab();
}

// ---------- Requests tab (request-log query UI) ----------

// Per-tab filter state (model/provider substring, exact agent/session, plus
// errors-only and the shadow tri-state).
// Persists across re-renders within a session so a refresh keeps the view.
let requestsFilter = { session: '', agent: '', model: '', provider: '', errors: false, shadow: '' };

// renderRequestsTab builds the request-log query view: a filter row + a table of
// metadata-only summaries fetched from /api/requests, with click-to-expand rows
// that load the full request/response bodies from /api/requests/<id>. On-demand
// (no 5s poll) — fetch happens on tab entry and on Refresh.
// requestsCombos carries the data-driven provider/model facet state, the
// recent session list (dropdown + per-session aggregate), and the last
// fetched rows (so the session summary can render once the aggregate
// arrives). Module scope: tab re-entry skips the skeleton rebuild below and
// must refresh through the same object the wired controls use.
let requestsCombos = null;

// retainTab implements the stale-while-revalidate tab re-entry guard (the
// "切 tab 不得闪骨架屏" rule): when the first-activation skeleton has already
// been built (the marker exists in the panel), the rendered DOM stays on
// screen and only the data refreshes. Returns false on first activation so
// the caller falls through to the skeleton mount; awaits refresh() so async
// renderers keep their completion semantics.
async function retainTab(panel, marker, refresh) {
  if (!panel || !panel.querySelector(marker)) return false;
  await refresh();
  return true;
}

// sessionLinkClick returns the session id when a row click landed on the
// row's session-link cell (the entry point into that session's view), null
// anywhere else — the shared prologue of the row click routers (Requests and
// Live), so the affordance cannot drift between them.
function sessionLinkClick(e) {
  const link = e.target && e.target.closest ? e.target.closest('.session-link') : null;
  return link && link.dataset.session ? link.dataset.session : null;
}

// The request-drill pin: #requests?request=<id>&kind=&name= (the Security
// page's "view the original request" link). The pinned request renders as an
// expanded card above the table — the guard hits located and highlighted
// (the explain view) on top of the full request detail (the same renderer
// the table's row expansion uses) — independent of the table's filters, so
// an old or filtered-out request still drills cleanly.
let requestDrill = null;

function requestDrillFromQuery(params) {
  if (!params || !params.request) return null;
  return {
    request: String(params.request),
    kind: params.kind === 'secret' || params.kind === 'path' ? params.kind : '',
    name: params.name || '',
  };
}

// applyRequestDrill renders (or clears) the pinned request card. Fetches the
// request detail through the shared bounded cache; the explain context is
// fetched once per pin (no cache — reopening is rare and freshness matters
// more than the request saved).
async function applyRequestDrill() {
  const host = document.getElementById('req-drill');
  if (!host) return;
  if (!requestDrill) {
    host.innerHTML = '';
    host.hidden = true;
    return;
  }
  host.hidden = false;
  const { request, kind, name } = requestDrill;
  host.innerHTML = '<div class="msg hint">loading request ' + esc(String(request).slice(-8)) + '…</div>';
  let recs = requestsDetailCache.get(request);
  if (!recs) {
    let resp;
    try {
      resp = await apiGet('/api/requests/' + encodeURIComponent(request));
    } catch (e) {
      host.innerHTML = (e && e.status === 404)
        ? '<div class="msg hint">request ' + esc(request) + ' is not in the request log (retention pruned it, or it predates logging)</div>'
        : '<div class="msg err">' + esc(e.message) + '</div>';
      return;
    }
    recs = resp.records || [];
    cacheRequestDetail(request, recs);
  }
  let explainHtml = '';
  if (kind && name) {
    try {
      const q = 'request_id=' + encodeURIComponent(request) + '&kind=' + encodeURIComponent(kind) + '&name=' + encodeURIComponent(name);
      const ex = await apiGet('/api/security/explain?' + q);
      explainHtml = securityExplainHTML(ex);
    } catch (e) {
      explainHtml = '<div class="msg hint">guard hits could not be re-located: ' + esc(e.message) + '</div>';
    }
  }
  if (!requestDrill || requestDrill.request !== request || !host.isConnected) return; // pin changed/unmounted mid-fetch
  host.innerHTML = `<div class="card" style="margin-bottom:12px;">
    <header class="card-head"><span class="card-head-title"><h2>Request ${esc(request)}</h2><span class="meta">pinned from the Security drill${kind ? ' · guard hits located below' : ''}</span></span>
    <span class="card-head-side"><button id="req-drill-close" class="btn" title="close the pinned view">✕</button></span></header>
    <div class="card-body">
    ${explainHtml}
    <div class="req-drill-detail">${recs.length ? detailRecordsHTML(recs, {}) : '<div class="msg hint">no record</div>'}</div>
  </div></div>`;
  const close = document.getElementById('req-drill-close');
  if (close) close.onclick = () => {
    requestDrill = null;
    setHash(requestsHash(), false);
    applyRequestDrill();
  };
  host.scrollIntoView({ behavior: 'smooth', block: 'start' });
  reqDetailChanged();
}

async function renderRequestsTab() {
  const panel = panels.requests;
  if (!panel) return;
  if (await retainTab(panel, '#req-table', () => refreshRequestsData(requestsCombos))) return;
  // Seed the filter from the URL hash (#requests?session=…): a refresh or a
  // shared link must land on the same view, not the unfiltered list. The
  // skeleton below templates the free inputs from the filter; the linked
  // selects pick it up in renderRequestSelectors (an unknown id is kept as
  // an option so the selection stays visible and reversible).
  const seeded = requestsFilterFromQuery(parseHash().query);
  if (seeded) requestsFilter = seeded;
  requestDrill = requestDrillFromQuery(parseHash().query);
  resetCombos();
  panel.innerHTML = `<div class="card card-open"><div class="card-body">
    <div class="req-controls" style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-bottom:12px;">
      <select id="req-agent" class="req-input" title="filter by client agent"><option value="">All Agents</option></select>
      <select id="req-session" class="req-input" title="filter by client session"><option value="">All Sessions</option></select>
      <span class="combo"><input id="req-provider" placeholder="All Providers" value="${esc(requestsFilter.provider)}" class="req-input"/></span>
      <span class="combo"><input id="req-model" placeholder="All Models" value="${esc(requestsFilter.model)}" class="req-input"/></span>
      <select id="req-shadow" class="req-input">
        <option value="" ${requestsFilter.shadow === '' ? 'selected' : ''}>All</option>
        <option value="only" ${requestsFilter.shadow === 'only' ? 'selected' : ''}>Shadow Only</option>
        <option value="exclude" ${requestsFilter.shadow === 'exclude' ? 'selected' : ''}>No Shadow</option>
      </select>
      <label style="display:flex;align-items:center;gap:4px;"><input type="checkbox" id="req-errors" ${requestsFilter.errors ? 'checked' : ''}/> Errors Only</label>
      <button id="req-refresh" class="btn">${iconRefresh()}Refresh</button>
    </div>
    <div id="req-session-summary" class="sess-sticky" style="margin-bottom:12px" hidden></div>
    <div id="req-drill" hidden></div>
    <div id="req-table"></div>
  </div></div>`;
  applyRequestDrill();
  const combos = requestsCombos = {
    providerOptions: [], modelOptions: [],
    facetState: { providerModels: {}, agents: [] },
    sessions: [], lastRecords: [],
  };
  const refresh = () => {
    requestsFilter.agent = document.getElementById('req-agent').value;
    requestsFilter.session = document.getElementById('req-session').value;
    requestsFilter.provider = document.getElementById('req-provider').value.trim();
    requestsFilter.model = document.getElementById('req-model').value.trim();
    requestsFilter.errors = document.getElementById('req-errors').checked;
    requestsFilter.shadow = document.getElementById('req-shadow').value;
    updateRequestsHash();
    loadRequests(combos);
  };
  // Agent and session are linked both ways: picking an agent narrows the
  // session list to that agent's sessions, and picking a session narrows the
  // agent list to the agents seen on it (normally pinning a single one). A
  // selection the other dimension no longer offers is cleared rather than
  // silently ANDed into an empty result.
  const onAgentSelect = () => {
    requestsFilter.agent = document.getElementById('req-agent').value;
    const allowed = sessionsForAgent(requestsFilter.agent, combos.sessions).map((s) => s.session_id);
    if (requestsFilter.session && !allowed.includes(requestsFilter.session)) requestsFilter.session = '';
    renderRequestSelectors(combos);
    refresh();
  };
  const onSessionSelect = () => {
    requestsFilter.session = document.getElementById('req-session').value;
    const allowed = linkedAgents(requestsFilter.session, combos.sessions, combos.facetState.agents);
    if (requestsFilter.agent && !allowed.includes(requestsFilter.agent)) requestsFilter.agent = '';
    renderRequestSelectors(combos);
    refresh();
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
  document.getElementById('req-agent').onchange = onAgentSelect;
  document.getElementById('req-session').onchange = onSessionSelect;
  // The checkbox applies immediately too — every filter control (session,
  // combos, shadow select, errors only) has the same on-change behavior.
  document.getElementById('req-errors').onchange = refresh;
  attachCombo(document.getElementById('req-provider'), combos.providerOptions, onProviderSelect);
  attachCombo(document.getElementById('req-model'), combos.modelOptions, refresh);
  // Paint the retained agent/session selections into the freshly rendered
  // (option-less) selects BEFORE the first fetch: refresh() reads the filter
  // back out of the DOM, so an empty select would otherwise clear a filter
  // that survived the re-render.
  refreshRequestsData(combos);
}

// refreshRequestsData repaints the retained filter selections, reloads the
// table in place, and re-pulls the session aggregate behind it. Shared by the
// first mount and every tab re-entry.
function refreshRequestsData(combos) {
  renderRequestSelectors(combos);
  loadRequests(combos);
  // Agent/session dropdown options come from the log facets and the persisted
  // aggregate (request logging may be off → empty lists, selects stay "all
  // agents"/"all sessions"). Fetched after the first load so the table renders
  // immediately; the summary re-renders once the aggregate for a persisted
  // selection is available.
  apiGet('/api/sessions?limit=200').then((resp) => {
    combos.sessions = (resp && resp.sessions) || [];
    renderRequestSelectors(combos);
    renderRequestsSessionSummary(combos);
  }).catch(() => { /* request logging off / unavailable */ });
}

// renderRequestSelectors repaints the linked agent and session dropdowns from
// the current selection: the agent options come from the log-wide agent facet
// narrowed by the selected session, the session options from the aggregate
// narrowed by the selected agent. A still-selected value that the aggregate
// does not know (a session aged out of /api/sessions, an agent whose records
// aged out of the facet window) is kept as an option so the active filter
// stays visible and reversible.
function renderRequestSelectors(combos) {
  const agentSel = document.getElementById('req-agent');
  const sessionSel = document.getElementById('req-session');
  if (agentSel) {
    const agents = linkedAgents(requestsFilter.session, combos.sessions, combos.facetState.agents);
    if (requestsFilter.agent && !agents.includes(requestsFilter.agent)) agents.unshift(requestsFilter.agent);
    agentSel.innerHTML = '<option value="">All Agents</option>' +
      agents.map((name) => `<option value="${esc(name)}">${esc(name)}</option>`).join('');
    agentSel.value = requestsFilter.agent;
  }
  if (sessionSel) {
    // Same ordering as the Live dropdown: most recently active first, ties
    // by session id (pure.js liveSessionOrder over the agent-filtered
    // summaries; no live rows to merge here).
    let ids = liveSessionOrder(sessionsForAgent(requestsFilter.agent, combos.sessions), []);
    if (requestsFilter.session && !ids.includes(requestsFilter.session)) ids = [requestsFilter.session, ...ids];
    // v2: full session ids in the options (the select is fixed 16rem wide;
    // the closed box ellipsizes, the OS popup shows the whole id). Matches
    // the Live page's full-id dropdown.
    sessionSel.innerHTML = '<option value="">All Sessions</option>' +
      ids.map((id) => `<option value="${esc(id)}">${esc(id)}</option>`).join('');
    sessionSel.value = requestsFilter.session;
  }
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
  menu.setAttribute('data-popup', ''); // the auto-refresh gate looks for this
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

// ===========================================================================
// AUTO-REFRESH INTERACTION GATE (framework)
// ===========================================================================
//
// Any timer- or event-driven re-render that wipes a panel's DOM must first
// consult the gate: if the user is interacting inside that panel — an open
// popover/dropdown/calendar (POPUP_OPEN_SEL: anything carrying `data-popup`
// that isn't `hidden`, plus combobox menus attached to document.body), focus
// on an editable control (typing, an open native <select> popup, a datalist
// suggestion popup — the control keeps focus while those are open), or an
// active text selection (copying log lines) — the refresh is deferred, not
// dropped: a hold watcher re-checks ~400ms after every interaction change
// and fires the pending refresh as soon as the user is done.
//
// Two checkpoints share this gate so a fetch that was already in flight
// when the interaction started cannot clobber it either:
//   1. tick time  — the interval callback skips (and arms the watcher);
//   2. commit time — a background render re-checks right before mutating
//      DOM and defers itself if an interaction began mid-fetch.
// User-initiated renders (filter clicks, mutations, section switches)
// bypass the gate: those close the popups themselves before re-rendering.
// Adding a new auto-refresh surface = route its tick AND its background
// render through deferAutoRefresh; adding a new popup = give it `data-popup`
// + the `hidden` attribute. Nothing else.

// focusInInteractive reports whether focus sits on an editable control
// inside rootEl. Buttons and links are deliberately excluded: clicking them
// commits instantly and must not stall background refreshes.
function focusInInteractive(rootEl) {
  const ae = document.activeElement;
  if (!rootEl || !ae || ae === document.body || ae === document.documentElement) return false;
  if (!rootEl.contains(ae)) return false;
  return ae.matches(INTERACTIVE_CONTROL_SEL) || !!ae.closest(INTERACTIVE_CONTROL_SEL);
}

// textSelectionIn reports whether a non-collapsed selection (drag-selected
// text, e.g. log lines being copied) starts inside rootEl.
function textSelectionIn(rootEl) {
  const sel = window.getSelection && window.getSelection();
  if (!sel || sel.isCollapsed || !sel.anchorNode) return false;
  return rootEl.contains(sel.anchorNode);
}

// comboMenuOpenIn reports whether one of the body-attached combobox menus
// (their inputs live inside rootEl, the floating menu does not) is open.
function comboMenuOpenIn(rootEl) {
  for (const combo of comboInstances) {
    if (!combo.menu.hidden && rootEl.contains(combo.input)) return true;
  }
  return false;
}

// autoRefreshBlocked returns WHY a background re-render of rootEl must wait
// ('popup' | 'focus' | 'selection'), or null when it may proceed.
function autoRefreshBlocked(rootEl) {
  if (!rootEl || !rootEl.isConnected) return null;
  return refreshHoldReason({
    openPopup: !!rootEl.querySelector(POPUP_OPEN_SEL) || comboMenuOpenIn(rootEl),
    focusInteractive: focusInInteractive(rootEl),
    selection: textSelectionIn(rootEl),
  });
}

// holdWatchers maps a panel element to its pending refresh (one per panel —
// repeated ticks during one hold just replace the pending callback).
const holdWatchers = new Map();
const HOLD_POLL_MS = 400;

// deferAutoRefresh is the gate entry point: when rootEl is interaction-held
// it parks `fire` on the panel's hold watcher (polling until the hold
// releases, then firing once) and returns true. Returns false when the
// refresh may run now. A detached panel cancels its watcher without firing.
function deferAutoRefresh(rootEl, fire) {
  if (!autoRefreshBlocked(rootEl)) return false;
  const existing = holdWatchers.get(rootEl);
  if (existing) {
    existing.fire = fire;
    return true;
  }
  const watcher = { fire, timer: 0 };
  watcher.timer = setInterval(() => {
    if (!rootEl.isConnected) {
      clearInterval(watcher.timer);
      holdWatchers.delete(rootEl);
      return;
    }
    if (autoRefreshBlocked(rootEl)) return;
    clearInterval(watcher.timer);
    holdWatchers.delete(rootEl);
    watcher.fire();
  }, HOLD_POLL_MS);
  holdWatchers.set(rootEl, watcher);
  return true;
}

// cancelAutoRefreshHold drops a panel's pending refresh (tab switch, timer
// teardown) without firing it.
function cancelAutoRefreshHold(rootEl) {
  const watcher = holdWatchers.get(rootEl);
  if (watcher) {
    clearInterval(watcher.timer);
    holdWatchers.delete(rootEl);
  }
}

// setRefreshError is the shared "background refresh failed, old data kept"
// banner: a failed auto-refresh must never overwrite the last successfully
// rendered content — it reports through this banner (a direct child of the
// panel, above the layout) and the next successful refresh clears it
// (text=null removes it). Banner text comes from pure.js staleDataText.
function setRefreshError(panel, text) {
  if (!panel) return;
  let el = panel.querySelector(':scope > .refresh-err');
  if (text == null) {
    if (el) el.remove();
    return;
  }
  if (!el) {
    el = document.createElement('div');
    el.className = 'msg err refresh-err';
    panel.prepend(el);
  }
  el.textContent = text;
}

// syncRequestFacets updates the filter dropdowns from the response's
// data-driven facets (distinct providers/models/agents observed in the log
// window, NOT the config catalog — agents are never configured, so the log is
// their only source). The model list narrows to the selected provider via the
// facet's provider→models map, and the agent list is repainted (linked to the
// selected session) because the facets arrive after the first render.
function syncRequestFacets(facets, combos) {
  if (!combos) return;
  const data = facets || {};
  combos.facetState.providerModels = data.provider_models || {};
  combos.facetState.agents = data.agents || [];
  const providers = data.providers || [];
  combos.providerOptions.splice(0, combos.providerOptions.length, ...providers);
  const providerInput = document.getElementById('req-provider');
  const provider = providerInput ? providerInput.value.trim() : '';
  const models = linkedModels(provider, combos.facetState.providerModels, {});
  combos.modelOptions.splice(0, combos.modelOptions.length, ...models);
  renderRequestSelectors(combos);
}

// hideRequestsSessionSummary clears the session aggregate strip (request
// logging off or a failed query must not leave a stale summary behind).
function hideRequestsSessionSummary() {
  const host = document.getElementById('req-session-summary');
  if (host) { host.hidden = true; host.innerHTML = ''; syncSessThOffset(host); }
}

// renderRequestsSessionSummary shows the selected session's aggregate above the
// request table (same chips as the Live session panel), in a sticky container
// so it stays in view while the (often long) session's table scrolls. Hidden
// when no session is selected; the aggregate comes from /api/sessions
// (combos.sessions) and the displayed rows fill in when the aggregate is
// missing (aged-out session).
function renderRequestsSessionSummary(combos) {
  const host = document.getElementById('req-session-summary');
  if (!host) return;
  if (!requestsFilter.session) { host.hidden = true; host.innerHTML = ''; return; }
  const agg = (combos.sessions || []).find((s) => s.session_id === requestsFilter.session) || null;
  const rows = (combos.lastRecords || []).map(persistedSummaryRow);
  // The SAME session view the Live panel renders (chips + trace timeline —
  // one implementation, shared); bars toggle the table's inline detail row
  // AND locate it: the row scrolls into view and flashes, because a busy
  // session's table can be hundreds of rows deep.
  host.hidden = false;
  hideTlTip();
  host.innerHTML = sessionViewHTML(rows, agg, { live: false, session: requestsFilter.session });
  wireSessionTimeline(host, (id) => {
    // The row may be virtualized out of the DOM — mount it first, then
    // expand + flash as before.
    const tr = reqRowForId(id);
    if (!tr) return;
    toggleRequestDetail(tr);
    tr.scrollIntoView({ behavior: 'smooth', block: 'center' });
    tr.classList.add('row-flash');
    setTimeout(() => tr.classList.remove('row-flash'), 1800);
  }, {
    session: requestsFilter.session,
    rerender: () => renderRequestsSessionSummary(combos),
    rows,
  });
  syncSessThOffset(host);
}

// reqFilterParams serializes the Requests tab filter into /api/requests
// query params (shared by the initial load and the scroll-driven older-page
// fetches, which must agree or the pages would not line up).
function reqFilterParams() {
  const q = new URLSearchParams();
  if (requestsFilter.session) q.set('session', requestsFilter.session);
  if (requestsFilter.agent) q.set('agent', requestsFilter.agent);
  if (requestsFilter.model) q.set('model', requestsFilter.model);
  if (requestsFilter.provider) q.set('provider', requestsFilter.provider);
  if (requestsFilter.errors) q.set('errors', '1');
  if (requestsFilter.shadow) q.set('shadow', requestsFilter.shadow);
  return q;
}

async function loadRequests(combos) {
  const tbl = document.getElementById('req-table');
  // Painting a fresh table drops every open detail row (an in-flight fetch
  // checks row.isConnected before filling); their chunk state drops with the
  // rows. The previous table stays visible while the fetch runs — only an
  // empty table shows the loading hint — so a refresh never flashes blank.
  const paint = (html) => {
    if (!tbl) return;
    reqTearDown(reqVirt);
    reqVirt = null;
    tbl.querySelectorAll('[data-chunk]').forEach((host) => bodyChunkRegistry.delete(host.dataset.chunk));
    dropRawBodies(tbl);
    tbl.innerHTML = html;
  };
  if (tbl && !tbl.firstElementChild) paint('<span class="hint">loading…</span>');
  const q = reqFilterParams();
  // The browse list starts small (REQ_BROWSE_LIMIT); scrolling near the
  // bottom pulls older pages on demand (reqLoadOlder). A session view is a
  // focused drill-down, so it still opens with a deeper window (under the
  // backend's 1000 cap) — scroll loading extends it to the cap from there.
  const pageSize = requestsFilter.session ? REQ_SESSION_LIMIT : REQ_BROWSE_LIMIT;
  q.set('limit', String(pageSize));
  const token = ++reqLoadSeq;
  let resp;
  try {
    resp = await apiGet('/api/requests?' + q.toString());
  } catch (e) {
    if (token !== reqLoadSeq) return;
    paint(`<div class="msg err">${esc(e.message)}</div>`);
    hideRequestsSessionSummary();
    return;
  }
  if (token !== reqLoadSeq) return;
  if (!resp.enabled) {
    paint('<div class="msg hint">Request logging is off. Enable <code>request_log.enabled</code> in config to capture request/response bodies for replay and debugging.</div>');
    hideRequestsSessionSummary();
    return;
  }
  // Facets come from the indexed log (data-driven, limit-independent on the
  // index path), so refresh the dropdowns even when the current filter
  // matches nothing. Scroll-loaded pages do NOT re-sync: a to=-bounded page
  // sees a narrower window and would shrink the dropdowns for no reason.
  syncRequestFacets(resp.facets, combos);
  const recs = resp.records || [];
  combos.lastRecords = recs;
  renderRequestsSessionSummary(combos);
  if (!recs.length) {
    paint('<div class="msg hint">No matching requests.</div>');
    return;
  }
  // Virtual table: the thead is static, the tbody renders only the viewport
  // window of rows (spacers keep the scrollbar sized to the whole list) and
  // a hint line reports the loaded count / load-older state.
  paint(`<table class="table">${requestTableHeadHTML()}<tbody></tbody></table><div class="hint req-count" hidden></div>`);
  if (!tbl) return;
  const v = reqVirt = {
    tbl: tbl.querySelector('table'),
    hint: tbl.querySelector('.req-count'),
    combos, recs, pageSize,
    more: recs.length >= pageSize && recs.length < REQ_MAX_LOADED,
    loading: false, failedAt: 0, err: '', revealId: '',
    heights: new Map(), avg: 38, offsets: [0], pool: new Map(), key: '', byId: null,
  };
  v.tbody = v.tbl.tBodies[0];
  v.byId = new Map(recs.map((r, i) => [r.request_id, i]));
  wireReqScroll();
  reqHint(v);
  reqFrame(v);
}

// ---------- Requests table virtual scrolling ----------
//
// The Requests table keeps only the rows near the viewport in the DOM:
// spacer rows carry the height of the unloaded gaps so the page scrollbar
// reflects every loaded record; rows enter the DOM as the window approaches
// and leave it again once the window moves on. Pooled <tr> nodes keep open
// detail rows (and their chunked-body state) alive across window moves — a
// row with an open detail is pinned into every window until it is closed.
// Older pages are fetched on demand: nearing the bottom pulls the next page
// via to=<oldest second> keyset pagination (the boundary second is re-fetched
// and deduped by request id, see mergeRecordsPages), so the default browse
// window stays at 50 while deeper history is one scroll away (capped at the
// backend's 1000).
const REQ_BROWSE_LIMIT = 50;
const REQ_SESSION_LIMIT = 500;
const REQ_MAX_LOADED = 1000;
const REQ_OVERSCAN_PX = 480;
const REQ_LOAD_AHEAD_PX = 700;
let reqVirt = null;
let reqLoadSeq = 0;
let reqScrollWired = false;
let reqScrollScheduled = false;

// reqTearDown frees the pooled rows' lazy-render state (chunked bodies, raw
// body registries) when the table is replaced by a fresh load — pooled nodes
// may be detached from the DOM, so they are walked via the pool, not the
// (already replaced) container.
function reqTearDown(v) {
  if (!v) return;
  v.pool.forEach((tr) => {
    const units = [tr];
    const det = tr.nextElementSibling;
    if (det && det.classList.contains('req-detail-row')) units.push(det);
    for (const node of units) {
      node.querySelectorAll('[data-chunk]').forEach((h) => bodyChunkRegistry.delete(h.dataset.chunk));
      dropRawBodies(node);
    }
  });
  v.pool.clear();
}

// reqRowNode returns the pooled <tr> for record i, creating (and wiring) it
// on first use. Rows reuse the SAME renderer as the Live tables (one
// implementation): snake_case records project through persistedSummaryRow
// into the merged camelCase row shape.
function reqRowNode(v, i) {
  const rec = v.recs[i];
  let tr = v.pool.get(rec.request_id);
  if (tr) return tr;
  const holder = document.createElement('tbody');
  holder.innerHTML = requestRowHTML(persistedSummaryRow(rec), {
    rowClass: 'req-row' + (rec.status >= 400 ? ' req-row-err' : ''),
    fmtTime,
  });
  tr = holder.firstElementChild;
  v.pool.set(rec.request_id, tr);
  // Session cell → filter this tab to that session (same linkage as picking
  // it in the dropdown: the agent filter narrows to that session's agents);
  // anywhere else on the row toggles the inline detail.
  tr.onclick = (e) => {
    const session = sessionLinkClick(e);
    if (session) {
      requestsFilter.session = session;
      const allowed = linkedAgents(requestsFilter.session, v.combos.sessions, v.combos.facetState.agents);
      if (requestsFilter.agent && !allowed.includes(requestsFilter.agent)) requestsFilter.agent = '';
      updateRequestsHash();
      renderRequestSelectors(v.combos);
      loadRequests(v.combos);
      return;
    }
    toggleRequestDetail(tr);
  };
  return tr;
}

function reqSpacer(px) {
  const tr = document.createElement('tr');
  tr.className = 'req-spacer';
  tr.innerHTML = `<td style="height:${Math.max(0, Math.round(px))}px"></td>`;
  return tr;
}

// reqRebuildOffsets recomputes the running row tops from the measured
// heights (cumulativeOffsets is the single definition); unmeasured rows fall
// back to the running average (rows are near-uniform, so the estimate only
// carries the scrollbar until first measure).
function reqRebuildOffsets(v) {
  const hs = new Array(v.recs.length);
  for (let i = 0; i < v.recs.length; i += 1) {
    const h = v.heights.get(v.recs[i].request_id);
    hs[i] = Number.isFinite(h) && h > 0 ? h : v.avg;
  }
  v.offsets = cumulativeOffsets(hs);
}

// reqReconcile mounts exactly the desired rows — the viewport window plus
// every pinned row (open detail) and any in-flight reveal (timeline jump) —
// and unloads the rest back into the pool. Spacer rows fill the gaps so
// scroll geometry never changes as rows load/unload.
function reqReconcile(v, extraIndex) {
  if (!v.tbody || !v.tbl.isConnected) return;
  const n = v.recs.length;
  // Viewport in table coordinates (the table's top edge is 0).
  const rect = v.tbl.getBoundingClientRect();
  const win = virtualWindow(v.offsets, Math.max(0, -rect.top), Math.max(0, window.innerHeight - rect.top), REQ_OVERSCAN_PX);
  const want = new Set();
  if (win) for (let i = win.first; i <= win.last; i += 1) want.add(i);
  // Pinned rows: an open detail must survive window moves (its chunked body
  // state lives on the DOM nodes), wherever the user scrolls.
  v.pool.forEach((tr) => {
    if (!tr.classList.contains('req-open')) return;
    const i = v.byId.get(tr.dataset.id);
    if (i != null) want.add(i);
  });
  // A timeline jump scrolls SMOOTHLY to a row that may start far outside
  // the window: keep it mounted until the window reaches it naturally,
  // otherwise the first scroll event mid-flight would unmount the target
  // and abort the scroll.
  let reveal = -1;
  if (v.revealId) {
    const ri = v.byId.get(v.revealId);
    reveal = ri != null ? ri : -1;
    if (reveal >= 0) {
      if (win && reveal >= win.first && reveal <= win.last) v.revealId = '';
      else want.add(reveal);
    } else {
      v.revealId = '';
    }
  }
  if (extraIndex != null && extraIndex >= 0 && extraIndex < n) want.add(extraIndex);
  const idx = [...want].sort((a, b) => a - b);
  if (!idx.length) {
    if (v.key !== '') { v.key = ''; v.tbody.replaceChildren(); }
    return;
  }
  // Skip the rebuild while the mounted set and the spacer bounds are
  // unchanged — scrolling within a window must not touch the DOM at all.
  const key = idx.join(',') + '|' + Math.round(v.offsets[idx[0]]) + '|' + Math.round(v.offsets[idx[idx.length - 1] + 1]);
  if (key === v.key) return;
  v.key = key;
  const frag = document.createDocumentFragment();
  const mounted = [];
  let prev = -1;
  for (const i of idx) {
    const gap = prev < 0 ? v.offsets[i] : v.offsets[i] - v.offsets[prev + 1];
    if (gap >= 1) frag.appendChild(reqSpacer(gap));
    const tr = reqRowNode(v, i);
    const det = tr.classList.contains('req-open') && tr.nextElementSibling && tr.nextElementSibling.classList.contains('req-detail-row')
      ? tr.nextElementSibling : null;
    frag.appendChild(tr);
    if (det) frag.appendChild(det);
    mounted.push({ i, tr, det });
    prev = i;
  }
  const tail = v.offsets[n] - v.offsets[prev + 1];
  if (tail >= 1) frag.appendChild(reqSpacer(tail));
  v.tbody.replaceChildren(frag);
  // Measure the mounted units (row + its open detail) so the next offsets —
  // and with them the spacer heights — use real heights, not the average.
  let sum = 0;
  for (const m of mounted) {
    let h = m.tr.getBoundingClientRect().height;
    if (m.det) h += m.det.getBoundingClientRect().height;
    if (h > 0) { v.heights.set(v.recs[m.i].request_id, h); sum += h; }
  }
  if (mounted.length) v.avg = Math.max(12, sum / mounted.length);
}

// reqFrame is the scroll/resize entry: refresh the window, then fetch the
// next older page when the bottom of the loaded list comes into reach. A
// short first page (list shorter than the viewport) chains itself until the
// scrollbar engages or the log is exhausted.
function reqFrame(v) {
  if (!v || !v.tbl || !v.tbl.isConnected) return;
  reqRebuildOffsets(v);
  reqReconcile(v);
  const rect = v.tbl.getBoundingClientRect();
  if (v.more && !v.loading && rect.bottom - window.innerHeight < REQ_LOAD_AHEAD_PX && Date.now() - v.failedAt > 2500) {
    reqLoadOlder(v);
  }
}

function wireReqScroll() {
  if (reqScrollWired) return;
  reqScrollWired = true;
  // Capture phase: scroll does not bubble, and the same frame must not
  // touch the DOM synchronously (rAF coalescing, same as wireBodyChunks).
  document.addEventListener('scroll', () => {
    const v = reqVirt;
    if (!v || !v.tbl || !v.tbl.isConnected || reqScrollScheduled) return;
    reqScrollScheduled = true;
    requestAnimationFrame(() => {
      reqScrollScheduled = false;
      const cur = reqVirt;
      if (cur && cur.tbl && cur.tbl.isConnected) reqFrame(cur);
    });
  }, true);
}

// reqLoadOlder fetches the next page of older records: same filters, limit
// at the page size, to = the oldest loaded record's second. The boundary
// second is re-fetched; mergeRecordsPages dedupes by id. A short page means
// the (filtered) log is exhausted; the 1000-row backend cap ends paging too.
async function reqLoadOlder(v) {
  if (v.loading || !v.more) return;
  v.loading = true;
  v.err = '';
  reqHint(v);
  const q = reqFilterParams();
  const to = oldestTsSec(v.recs);
  if (to != null) q.set('to', String(to));
  q.set('limit', String(v.pageSize));
  let chained = false;
  try {
    const resp = await apiGet('/api/requests?' + q.toString());
    if (reqVirt !== v) return;
    const page = (resp && resp.records) || [];
    const merged = mergeRecordsPages(v.recs, page);
    v.recs = merged.records;
    v.byId = new Map(v.recs.map((r, i) => [r.request_id, i]));
    v.combos.lastRecords = v.recs;
    v.more = page.length >= v.pageSize && v.recs.length < REQ_MAX_LOADED;
    chained = merged.added > 0;
  } catch (e) {
    // Keep the loaded rows on screen; the hint carries the error and the
    // failedAt backoff keeps a dead upstream from being re-hit every frame.
    v.failedAt = Date.now();
    v.err = (e && e.message) || 'request failed';
  }
  v.loading = false;
  if (reqVirt !== v || !v.tbl.isConnected) return;
  reqHint(v);
  if (chained) reqFrame(v);
}

// reqHint paints the count/load-older status line under the table.
function reqHint(v) {
  if (!v.hint) return;
  if (v.loading) {
    v.hint.hidden = false;
    v.hint.textContent = 'loading older requests…';
  } else if (v.err) {
    v.hint.hidden = false;
    v.hint.textContent = `could not load older requests: ${v.err}`;
  } else if (v.more) {
    v.hint.hidden = false;
    v.hint.textContent = `showing ${fmtNum(v.recs.length)} requests · scroll down for older`;
  } else {
    // A list that fits the default window needs no narration.
    const quiet = v.recs.length <= REQ_BROWSE_LIMIT;
    v.hint.hidden = quiet;
    v.hint.textContent = v.recs.length >= REQ_MAX_LOADED
      ? `showing the first ${fmtNum(v.recs.length)} requests`
      : `showing ${fmtNum(v.recs.length)} requests`;
  }
}

// reqDetailChanged re-measures a row's unit after its inline detail opens,
// fills or closes: the detail's height belongs to the row's footprint, so
// the offsets (and the spacers below) go stale the moment it changes.
function reqDetailChanged() {
  const v = reqVirt;
  if (!v) return;
  requestAnimationFrame(() => {
    if (reqVirt !== v || !v.tbody || !v.tbl.isConnected) return;
    v.pool.forEach((tr) => {
      if (!tr.classList.contains('req-open') || !tr.isConnected) return;
      const det = tr.nextElementSibling;
      let h = tr.getBoundingClientRect().height;
      if (det && det.classList.contains('req-detail-row')) h += det.getBoundingClientRect().height;
      if (h > 0) v.heights.set(tr.dataset.id, h);
    });
    v.key = '';
    reqFrame(v);
  });
}

// reqRowForId returns the summary row for a request id, mounting it first if
// the window virtualized it away — the session timeline jumps to bars whose
// rows may sit far outside the rendered window. The reveal pin keeps the
// row mounted through the smooth scroll (see reqReconcile).
function reqRowForId(id) {
  const v = reqVirt;
  if (!v || !v.tbody) {
    return document.querySelector('.req-row[data-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]');
  }
  for (let i = 0; i < v.recs.length; i += 1) {
    if (v.recs[i].request_id !== id) continue;
    v.revealId = id;
    reqRebuildOffsets(v);
    reqReconcile(v, i);
    return v.pool.get(id) || null;
  }  return null;
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

// ---------- raw body lazy render ----------
//
// Raw request/response bodies render ONLY on first expand: the pretty/SSE
// views are the expensive part of a detail (multi-MB JSON highlight), and
// the chat transcript above already covers reading. The body text rides a
// registry keyed by generated id — never a data attribute, bodies are huge —
// and the document-level toggle listener (capture: details' toggle does not
// bubble) builds the view once, on open. Programmatic restores (the live
// popover's open-state capture) assign .open, which fires toggle too, so
// they lazy-render the same way. Entries are dropped wherever the chunked
// body views' state is dropped (same teardown sites).
const rawBodyRegistry = new Map();
let rawBodySeq = 0;
let rawBodiesWired = false;

function wireRawBodies() {
  if (rawBodiesWired) return;
  rawBodiesWired = true;
  document.addEventListener('toggle', (e) => {
    const d = e.target;
    if (!d || !d.classList || !d.open || d.dataset.rawDone) return;
    // Raw request/response bodies: render the (chunked) view on first open.
    if (d.classList.contains('raw-body')) {
      d.dataset.rawDone = '1';
      const entry = rawBodyRegistry.get(d.dataset.raw);
      if (!entry) return;
      const host = d.querySelector('.raw-body-host');
      if (host) host.innerHTML = capturedBodyView(entry.text, entry.contentType, entry.kind).html;
      return;
    }
    // Chat history fold: swap the placeholder for the FULL earlier turns as
    // a flat, scroll-chunked transcript (25 turns per chunk, appended by the
    // shared bodyChunkRegistry scroll loader). The request body parses ONCE
    // per expand and the parsed messages ride the registry entry — a
    // thousand-turn session expands fast and reads linearly, no per-turn
    // clicking, and later chunks cost only string rendering.
    if (d.classList.contains('cv-history')) {
      d.dataset.rawDone = '1';
      const entry = rawBodyRegistry.get(d.dataset.raw);
      const host = d.querySelector('.cv-hist-host');
      if (!entry || !host) return;
      if (!entry.msgs) {
        const parsed = parseChatRequest(entry.text);
        entry.msgs = parsed ? parsed.messages : [];
      }
      const end = Math.max(0, entry.msgs.length - CHAT_RECENT);
      if (!end) { host.innerHTML = '<div class="hint">(no earlier turns)</div>'; return; }
      const ranges = [];
      for (let i = 0; i < end; i += 25) ranges.push({ from: i, to: Math.min(i + 25, end) });
      const id = 'cv-hist-' + (++bodyChunkSeq);
      bodyChunkRegistry.set(id, { chunks: ranges, index: 0, render: (r) => chatTurnsSliceHTML(entry.msgs, r.from, r.to) });
      host.innerHTML = `<div class="cv-hist-scroll" data-chunk="${id}">${chatTurnsSliceHTML(entry.msgs, 0, ranges[0].to)}</div>`;
      wireBodyChunks();
      // The first chunk may not fill the scroll box — top it up so the
      // scrollbar engages (same reason appendNextChunk loops internally).
      const scroller = host.querySelector('.cv-hist-scroll');
      if (scroller && scroller.scrollHeight <= scroller.clientHeight + 80) appendNextChunk(scroller);
      return;
    }
  }, true);
}

function registerRawBody(text, contentType, kind) {
  const id = 'raw-' + (++rawBodySeq);
  rawBodyRegistry.set(id, { text: text || '', contentType, kind });
  return id;
}

// dropRawBodies frees the registry entries of lazily-rendered details inside
// (or equal to) container — raw bodies and chat history folds both carry
// data-raw. Call wherever the corresponding DOM goes away.
function dropRawBodies(container) {
  if (!container || !container.querySelectorAll) return;
  container.querySelectorAll('[data-raw]').forEach((d) => {
    rawBodyRegistry.delete(d.dataset.raw);
  });
}

// rawBodyLabel is the cheap summary label for a collapsed raw body (no view
// built until open): the old label needed the full parse, this one only
// classifies.
function rawBodyLabel(text, contentType, kind) {
  if (!text) return 'empty';
  if (text.length > BODY_RENDER_MAX) return 'raw';
  if (kind === 'response' && isSSE(text, contentType)) return 'SSE';
  return kind === 'request' ? 'json' : 'text';
}

// detailRecordsHTML renders the expanded detail for one request's records.
// The human-readable chat transcript (pure.js chatViewHTML — role-labeled
// turns, collapsed thinking/tool blocks with readable arguments, usage line)
// leads; the raw JSON/SSE bodies stay below in collapsed <details> that only
// render their (bounded, lazily chunked) view on first expand.
// fmtBytes humanizes a captured body size for the detail hint line.
function fmtBytes(n) {
  const v = Number(n) || 0;
  if (v >= 1048576) return (v / 1048576).toFixed(1) + 'MB';
  if (v >= 1024) return Math.round(v / 1024) + 'KB';
  return v + 'B';
}

// detailRecordsHTML renders the expanded detail for one request's records.
// opts.prevTs / opts.sessionStartTs (unix ms) add the relative-time context
// (T+ since session start, Δ after the previous request) when the caller
// knows the neighbors — the Requests table paint path does; the popover and
// re-open paths render absolute time only.
function detailRecordsHTML(recs, opts) {
  const o = opts || {};
  wireBodyChunks();
  wireRawBodies();
  let html = '';
  for (const r of recs) {
    const ct = responseContentType(r);
    const histId = registerRawBody(r.request_body, '', 'chat-history');
    const chat = chatViewHTML(r.request_body, r.response_body, ct, { histKey: histId });
    // Pre-warm the history parse during idle time after the detail paints:
    // the expand click then only renders. A multi-MB JSON.parse on the click
    // path is the one remaining heavy step — on slow machines it walks into
    // hundreds of ms between click and first paint of the turns.
    const histEntry = rawBodyRegistry.get(histId);
    if (histEntry) {
      const warm = () => {
        if (!histEntry.msgs) histEntry.msgs = (parseChatRequest(histEntry.text) || { messages: [] }).messages;
      };
      if (typeof requestIdleCallback === 'function') requestIdleCallback(warm, { timeout: 1500 });
      else setTimeout(warm, 400);
    }
    const reqId = registerRawBody(r.request_body, '', 'request');
    const resId = registerRawBody(r.response_body, ct, 'response');
    const recTs = Number.isFinite(Date.parse(r.ts)) ? Date.parse(r.ts) : null;
    const rel = [];
    if (recTs != null && o.sessionStartTs != null && recTs >= o.sessionStartTs) rel.push('T+' + fmtDurMs(recTs - o.sessionStartTs));
    if (recTs != null && o.prevTs != null && recTs >= o.prevTs) rel.push('Δ' + fmtDurMs(recTs - o.prevTs) + ' after prev');
    html += `<div class="req-rec">
      <div class="hint">${esc(r.ts)}${rel.length ? ' · ' + esc(rel.join(' · ')) : ''} · ${esc(r.method)} ${esc(r.path)} · attempt ${r.attempt} · ${r.status} · ${r.latency_ms}ms${r.ttft_ms ? ' · ttft ' + fmtDurMs(r.ttft_ms) : ''} · req ${fmtBytes(r.request_size)} · resp ${fmtBytes(r.response_size)} · ${esc(r.provider)}/${esc(r.upstream_model)}</div>
      ${chat}
      <details class="raw-body" data-raw="${esc(reqId)}"><summary>raw request body (${fmtNum(r.request_size)} bytes · ${esc(rawBodyLabel(r.request_body, '', 'request'))})</summary><div class="raw-body-host"><span class="hint">renders on first expand</span></div></details>
      <details class="raw-body" data-raw="${esc(resId)}"><summary>raw response body (${fmtNum(r.response_size)} bytes · ${esc(rawBodyLabel(r.response_body, ct, 'response'))})</summary><div class="raw-body-host"><span class="hint">renders on first expand</span></div></details>
    </div>`;
  }
  return html;
}

// requestRelTimeOpts derives the relative-time context for one request from
// the loaded list: session start = min ts, prev = the closest earlier ts.
// Null opts when the list (or ts) is unavailable — the hint then shows
// absolute time only.
function requestRelTimeOpts(id) {
  const all = (requestsCombos && requestsCombos.lastRecords) || [];
  const mine = all.find((x) => x && x.request_id === id);
  const mineTs = mine ? Date.parse(mine.ts) : NaN;
  if (!Number.isFinite(mineTs)) return {};
  const tsList = all.map((x) => (x && x.ts != null ? Date.parse(x.ts) : NaN)).filter(Number.isFinite);
  if (!tsList.length) return {};
  const earlier = tsList.filter((t) => t < mineTs);
  return {
    sessionStartTs: Math.min(...tsList),
    prevTs: earlier.length ? Math.max(...earlier) : null,
  };
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
  row.innerHTML = '<td colspan="8"><span class="hint">loading…</span></td>';
  tr.insertAdjacentElement('afterend', row);
  // Relative-time context: T+ since the session window's first request and
  // Δ after the chronologically previous one — both derived from the loaded
  // list (table sort order does not matter).
  const relOpts = requestRelTimeOpts(id);
  const cached = requestsDetailCache.get(id);
  if (cached) {
    row.firstElementChild.innerHTML = detailRecordsHTML(cached, relOpts);
    reqDetailChanged();
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
    reqDetailChanged();
    return;
  }
  if (!row.isConnected) return; // closed, or the table was rebuilt while loading
  const recs = resp.records || [];
  if (!recs.length) {
    row.firstElementChild.innerHTML = '<div class="msg hint">no record</div>';
    reqDetailChanged();
    return;
  }
  row.firstElementChild.innerHTML = detailRecordsHTML(recs, requestRelTimeOpts(id));
  cacheRequestDetail(id, recs);
  // The detail's height joins the row's footprint — remeasure so the
  // spacers below (and any further fetches) stay anchored.
  reqDetailChanged();
}

// closeDetailFor removes the detail row anchored to `tr` (its next sibling) and
// drops that row's chunk state so it cannot leak.
function closeDetailFor(tr) {
  tr.classList.remove('req-open');
  const row = tr.nextElementSibling;
  if (!row || !row.classList.contains('req-detail-row')) return;
  row.querySelectorAll('[data-chunk]').forEach((host) => bodyChunkRegistry.delete(host.dataset.chunk));
  dropRawBodies(row);
  row.remove();
  reqDetailChanged();
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
// The append is coalesced into requestAnimationFrame per host: inserting DOM
// synchronously inside the scroll handler janks the very frame the user is
// trying to scroll (the history turns are heavy enough to feel).
const chunkAppendScheduled = new WeakSet();
function wireBodyChunks() {
  if (bodyChunkWired) return;
  bodyChunkWired = true;
  document.addEventListener('scroll', (event) => {
    const host = event.target;
    if (!host || !host.dataset || !host.dataset.chunk) return;
    if (host.scrollTop + host.clientHeight < host.scrollHeight - 80) return;
    if (chunkAppendScheduled.has(host)) return;
    chunkAppendScheduled.add(host);
    requestAnimationFrame(() => {
      chunkAppendScheduled.delete(host);
      if (host.isConnected && host.dataset.chunk) appendNextChunk(host);
    });
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
    // Over the cap the body still renders, but CHUNKED (64KB slices appended
    // on scroll): a multi-MB single <pre> text node froze the tab on expand.
    return {
      label: 'raw',
      html: `<div class="hint">body over ${fmtNum(BODY_RENDER_MAX)} bytes — showing raw, chunked</div>` + chunkedBodyHTML(text, plainLinesHTML, ''),
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
  // Bodies cut mid-capture (request_log max_body_bytes) fail strict JSON.parse;
  // re-indent the valid prefix structurally so they stay readable.
  const loose = formatJSONLoose(text);
  if (loose !== null) {
    return { label: 'JSON · partial', html: chunkedBodyHTML(loose, plainLinesHTML, '') };
  }
  return { label: 'text', html: bodyLinesHTML(text) };
}

// bodyLinesHTML renders captured text one source line per row in the log
// gutter style (line numbers), so a streamed response stays compact instead
// of expanding into pretty-printed indentation. Over-long lines collapse, and
// very long bodies fall back to the chunked viewer.
function bodyLinesHTML(text) {
  const lines = text.split('\n');
  // Chunk on either dimension: many lines OR simply a large body — SSE lines
  // with fat deltas can hold hundreds of KB in only a few hundred lines.
  if (lines.length > BODY_LINE_MAX || text.length > BODY_HIGHLIGHT_MAX) return chunkedBodyHTML(text, plainLinesHTML, '');
  return `<div class="log-pre">${lines.map((line) => longLineHTML(line) || `<span class="log-line">${esc(line)}</span>`).join('')}</div>`;
}

// ---------- Security tab (guard audit log) ----------

// Per-tab filter state. kind narrows the AUDIT half server-side (the ring is
// client-filtered to match); verdict/rule narrow the merged feed client-side;
// range bounds the audit query (from=<now-secs>). Persists across re-renders
// within a session and rides the URL hash (#security?kind=…, see
// securityFilterQuery).
let securityFilter = { kind: '', verdict: '', range: 'all', rule: '' };
let securityReqSeq = 0;
// Row budget of the audit half — "show more" grows it to the backend's 1000
// cap; a kind/range change resets it (a new window is a new query).
let securityLimit = 100;
// Flags of the last audit query, rendered by renderSecurityFeed (inserting
// them ad-hoc after a table build would be wiped by the next rebuild — the
// old skipped-lines hint died exactly that way): skipped-lines count, and
// whether the audit log is enabled at all (drives the off-hints).
let securityFeedSkipped = 0;
let securityAuditOn = true;
// Summary/feed-layer cache: the audit and adjudication loaders each own one
// half of both the KPI row and the MERGED chronological feed, so whichever
// lands first paints with the data it has and the second refresh completes
// the picture (stale halves are never blanked).
let securityKpiData = { blocks: null, stats: null, counts: null };
let securityFeedData = { records: null, adjudications: null };
// The Rule-hits leaderboard's own audit slice: FIXED query params (all
// kinds, whole retention, the 1000-row cap) so the Activity filters — which
// go server-side on the FEED query — cannot change its counts. The
// leaderboard is the rule-ops view; the only allowed coupling is its own
// rows drilling INTO the feed (setSecurityRule), never the reverse.
let securityRulesData = { records: null };
let securityRulesSeq = 0;
// The adjudication channel's current on/off switch (feed.enabled) — the KPI
// row's LLM tiles and the rule-leaderboard's noise hint key off it, telling
// off from merely quiet.
let securityAdjudicationEnabled = false;
// Which refresh halves failed since their last success — the parts list of
// the shared stale-data banner (setRefreshError).
const securityFailParts = new Set();
// analyze expansions the user opened, keyed by explainCacheKey. renderSecurity
// Feed rebuilds #sec-table wholesale (Refresh / auto-refresh / filter change),
// so the open set is snapshotted here and every expansion that still has a row
// is restored after the rebuild — same snapshot-then-restore rule as
// renderLogsInto's open <details>.
const securityExpanded = new Set();
// Bounded explain-result cache by the same key. A restored expansion paints
// from cache instead of refetching every 30s tick; entries hold either the
// settled result or the in-flight promise (concurrent restores share one
// request). In-page rendered data only — nothing persisted beyond the page.
const SECURITY_EXPLAIN_CACHE_MAX = 32;
const securityExplainCache = new Map();

// securityRefreshOk clears one half's failure mark, dropping the banner once
// every half is healthy again (or narrowing it while others still fail).
function securityRefreshOk(part) {
  securityFailParts.delete(part);
  if (!securityFailParts.size) setRefreshError(panels.security, null);
  else setRefreshError(panels.security, staleDataText('refresh failed', [...securityFailParts]));
}

// securityRefreshFail records one half's failure. A failed half keeps the
// last successful data on screen and names itself in the banner; only a
// first-load failure (no data anywhere yet) returns true so the caller can
// fall back to its inline error card.
function securityRefreshFail(part) {
  securityFailParts.add(part);
  const hasData = securityFeedData.records || securityFeedData.adjudications || securityKpiData.blocks;
  if (hasData) setRefreshError(panels.security, staleDataText('refresh failed', [...securityFailParts]));
  return !hasData;
}

// securityRenderAll repaints every Security data host from stored data. The
// loaders commit through this ONE function so the gate's parked fire (which
// replaces earlier pending fires) is always a complete picture — a half's
// commit can never be lost to another half landing during the same gesture.
function securityRenderAll() {
  renderSecurityKpis();
  renderSecurityFeed();
  renderRuleLeaderboard();
  paintSecurityBlocks();
}

// commitSecurityRender is the loaders' commit-time checkpoint of the
// interaction gate (see deferAutoRefresh): a fetch that was still in flight
// when the user started a text selection / opened a select must NOT rebuild
// the panel when it lands — that wipes the selection mid-gesture and janks
// the double-click (the deterministic "switch back to Security, dblclick,
// page stutters" path: re-entry always fires these fetches). The data is
// already stored; the parked commit paints from it once the gesture ends
// (hold watcher). Synchronous user actions (filter clicks, verdict select)
// keep calling the renderers directly — user-initiated renders bypass the
// gate by contract.
function commitSecurityRender() {
  const panel = panels.security;
  if (panel && deferAutoRefresh(panel, securityRenderAll)) return;
  securityRenderAll();
}

// securityMergedRows merges the two feed halves narrowed to the current kind
// window (the audit half is already server-filtered by kind; the ring half
// is filtered here so the two agree). Verdict/rule narrowing stays with the
// callers that honor it: the KPI row counts the whole kind window, the
// activity table narrows further.
function securityMergedRows() {
  return mergeSecurityFeed(securityFeedData.records, securityFeedData.adjudications)
    .filter((r) => !securityFilter.kind || r.kind === securityFilter.kind);
}

function renderSecurityKpis() {
  const el = document.getElementById('sec-kpis');
  if (!el) return;
  el.innerHTML = securityKpisHTML(securityKpiData.blocks, securityKpiData.counts, securityKpiData.stats, securityAdjudicationEnabled);
}

// syncSecurityRuleChip paints the removable rule-filter chip into the
// Activity controls row (the leaderboard click and the chip's ✕ are the two
// ways to toggle it — both go through setSecurityRule).
function syncSecurityRuleChip() {
  const chip = document.getElementById('sec-rule-chip');
  if (!chip) return;
  chip.innerHTML = securityFilter.rule
    ? `<button id="sec-rule-clear" class="btn" title="clear rule filter">rule: ${esc(securityFilter.rule)} ✕</button>`
    : '';
  const btn = document.getElementById('sec-rule-clear');
  if (btn) btn.onclick = () => setSecurityRule('');
}

// setSecurityRule toggles the feed's rule filter (client-side over the
// merged rows) and mirrors it into the URL hash. The leaderboard's selection
// flips in place (syncRuleSel) — rebuilding that table on click is what used
// to eat double-click text selections on its rows: the clicked row died
// mid-gesture, between the two clicks.
function setSecurityRule(rule) {
  securityFilter.rule = rule || '';
  updateSecurityHash();
  renderSecurityFeed();
  syncRuleSel();
}

// syncRuleSel flips the leaderboard rows' selection marker to the current
// rule filter without rebuilding the table (the data did not change — only
// which row is the active filter).
function syncRuleSel() {
  document.querySelectorAll('#sec-rules tr.sec-rule').forEach((tr) => {
    tr.classList.toggle('sel', securityFilter.rule === tr.dataset.rule);
  });
}

// renderSecurityFeed renders the merged audit + AI-verdict feed: one
// chronological table (newest first). Fresh verdicts are deduped upstream
// (mergeSecurityFeed folds the ring entry into its audit record, surfacing
// the judge model/cached inline); ring-only rows (cached occurrences,
// pre-restart leftovers) keep the ai· badge. LOW verdicts never render
// here: the judge suppressed them, so they exist only in the KPI counter,
// the rule-hit counts and the JSONL trail — listing them would be noise.
//
// Re-entry and every 30s tick re-fetch and re-render; on an unchanged feed
// the rebuild is pure waste AND lands as a full-table innerHTML swap that
// eats a text selection just started (the gate only sees selections that
// already exist — a fetch landing between the two clicks of a double-click
// is invisible to it). So: fingerprint everything that feeds the markup and
// skip the DOM write entirely when nothing changed (same idiom as the Logs
// card's logsPrevKey). Incremental state (open analyze expansions, the
// user's own DOM edits) rides the untouched DOM.
let securityFeedFingerprint = '';

function renderSecurityFeed() {
  const tbl = document.getElementById('sec-table');
  if (!tbl) return;
  syncSecurityRuleChip();
  const ring = securityFeedData.adjudications;
  if (!securityAuditOn && (!ring || !ring.length)) {
    securityFeedFingerprint = '';
    tbl.innerHTML = '<div class="msg hint">Security audit is off. Enable <code>guard.audit</code> in config to persist guard hits (secret / path / drift) to the audit log.</div>';
    return;
  }
  const rows = securityMergedRows()
    .filter((r) => r.verdict !== 'low')
    .filter((r) => !securityFilter.verdict || r.verdict === securityFilter.verdict)
    .filter((r) => !securityFilter.rule || r.names.includes(securityFilter.rule));
  const hints = [];
  if (!securityAuditOn && ring && ring.length) {
    hints.push('<div class="msg hint">guard.audit is off — history is not persisted; only the in-memory recent AI verdicts are shown.</div>');
  }
  if (securityFeedSkipped > 0) {
    hints.push(`<div class="hint" style="margin-bottom:8px;">skipped ${fmtNum(securityFeedSkipped)} unreadable line(s) while scanning</div>`);
  }
  if (!rows.length) {
    securityFeedFingerprint = '';
    tbl.innerHTML = hints.join('') + '<div class="msg hint">No matching records.</div>';
    return;
  }
  const recs = securityFeedData.records || [];
  const fingerprint = JSON.stringify([
    rows, hints, securityLimit,
    securityAuditOn && recs.length >= securityLimit && securityLimit < 1000,
  ]);
  if (fingerprint === securityFeedFingerprint && tbl.querySelector('table')) {
    // Identical data, table already on screen: keep the DOM (and any
    // in-progress text selection / open expansion) exactly as it is.
    syncRuleSel();
    return;
  }
  securityFeedFingerprint = fingerprint;
  const kindBadge = { secret: 'warn', path: '', drift: 'muted' };
  const vBadge = { high: 'err', medium: 'warn', error: 'warn', skipped: 'muted' };
  const body = [];
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    // Both halves are analyzable when the explain endpoint can query them
    // (request id + secret/path kind) — cached ring verdicts (ai· rows) and
    // audit records share the same re-scan surface, and the ring row's
    // "why" (verdict reason + located hits) is exactly what analyze shows.
    // The explain key never rides an HTML attribute (its separator would not
    // survive attribute parsing); handlers carry it in closures and the
    // expansion row holds it as a DOM property.
    const analyzable = r.requestId && (r.kind === 'secret' || r.kind === 'path');
    // Drill into the original request: opens the Requests tab pinned to this
    // request with the guard hits located and highlighted (the explain view
    // rides the request detail). kind+name let the drill re-scan server-side.
    const drillUrl = r.requestId && (r.kind === 'secret' || r.kind === 'path')
      ? '#requests?request=' + encodeURIComponent(r.requestId) +
        '&kind=' + encodeURIComponent(r.kind) +
        '&name=' + encodeURIComponent(r.names.join(','))
      : '';
    const drillLink = drillUrl
      ? `<div><a class="mono drill-link" href="${drillUrl}" title="view the original request with the hit highlighted">req ${esc(String(r.requestId).slice(-6))} ↗</a></div>`
      : '';
    // Session cell: session-link (same idiom as the Blocked table and the
    // request tables) — click drills into that session's requests.
    const sessionCell = r.sessionId
      ? `<td class="mono session-link" data-session="${esc(r.sessionId)}" title="${esc(r.sessionId)} — view this session's requests">${esc(shortSessionId(r.sessionId))}</td>`
      : '<td class="mono">—</td>';
    if (r.src === 'ai') {
      const why = r.reason || r.detail;
      const evidence = r.evidence ? `<div class="hint">${esc(r.evidence)}</div>` : '';
      body.push(`<tr data-sec-i="${i}"${analyzable ? ' class="sec-row"' : ''}>
      <td class="mono">${esc(fmtMs(r.ts))}</td>
      <td><span class="badge ${vBadge[r.verdict] || ''}">${esc(r.verdict)}</span>${r.cached ? ' <span class="badge muted">cached</span>' : ''}</td>
      <td><span class="badge ${r.kind === 'path' ? '' : 'warn'}">ai·${esc(r.kind)}</span></td>
      <td class="mono">${esc(r.names.join(', ') || '—')}</td>
      <td class="mono">${esc(r.exposed || '—')}</td>
      ${sessionCell}
      <td class="mono">${esc(why || '—')}${evidence}${drillLink}</td>
    </tr>`);
      continue;
    }
    const strength = r.kind === 'path' ? r.strength : '';
    const strengthBadge = strength ? ` <span class="badge ${strength === 'strong' ? 'warn' : 'muted'}">${strength}</span>` : '';
    // Merged rows carry the judge attribution the ring added on top of the
    // persistent audit record. The verdict's own judgment logic (the LLM
    // reason, verbatim) and the exact-match attribution (source label +
    // masked key, in detail) render directly — no program-side paraphrase.
    const judge = r.judge ? ` <span class="hint">judge ${esc(r.judge)}${r.cached ? ' · cached' : ''}</span>` : '';
    const verdictText = r.reason ? `<div>${esc(r.reason)}</div>${r.evidence ? `<div class="hint">evidence: ${esc(r.evidence)}</div>` : ''}` : '';
    const detailText = r.detail ? `<div>${esc(r.detail)}</div>` : '';
    body.push(`<tr data-sec-i="${i}"${analyzable ? ' class="sec-row"' : ''}>
      <td class="mono">${esc(fmtMs(r.ts))}</td>
      <td>${r.verdict ? `<span class="badge ${vBadge[r.verdict] || ''}">${esc(r.verdict)}</span>` : '—'}</td>
      <td><span class="badge ${kindBadge[r.kind] || ''}">${esc(r.kind)}</span></td>
      <td class="mono">${esc(r.names.join(', ') || '—')}</td>
      <td class="mono">${esc(r.agent ? r.agent + (r.exposed ? ' @ ' + r.exposed : '') : '—')}</td>
      ${sessionCell}
      <td class="mono">${verdictText}${detailText}<div>action=${esc(r.action || '—')}${strengthBadge}${judge}</div>${drillLink}</td>
    </tr>`);
  }
  // The audit half may have more rows than the current budget (the ring
  // half is always fully loaded) — offer to deepen the query window.
  const more = securityAuditOn && recs.length >= securityLimit && securityLimit < 1000
    ? `<div style="margin-top:8px;"><button id="sec-more" class="btn" title="deepen the audit window (up to 1,000 rows)">Show More</button></div>`
    : '';
  tbl.innerHTML = hints.join('') + `<table class="table">
  <thead><tr><th>time</th><th>verdict</th><th>kind</th><th>rule / names</th><th>who</th><th>session</th><th>detail</th></tr></thead>
  <tbody>${body.join('')}</tbody></table>` + more;
  const moreBtn = document.getElementById('sec-more');
  if (moreBtn) {
    moreBtn.onclick = () => {
      securityLimit = Math.min(1000, securityLimit >= 500 ? 1000 : securityLimit >= 250 ? 500 : 250);
      loadSecurity();
    };
  }
  // Each data row carries one click router: the session cell is a
  // session-link drilling into that session's requests (same idiom as the
  // Blocked table), anywhere else on an analyzable row toggles its analyze
  // expansion (cursor:pointer via .sec-row), clicks that land on the drill
  // link (or any future a/button) navigate instead, and the second click of
  // a double-click is a text-selection gesture — the detail cell carries
  // copyable verdict text. Rows with neither a session nor an analyzable
  // hit (headless drift) stay inert.
  tbl.querySelectorAll('tr[data-sec-i]').forEach((tr) => {
    const rec = rows[Number(tr.dataset.secI)];
    if (!rec) return;
    const analyzable = !!rec.requestId && (rec.kind === 'secret' || rec.kind === 'path');
    if (!analyzable && !rec.sessionId) return;
    const key = analyzable ? explainCacheKey(rec.requestId, rec.kind, rec.names) : '';
    tr.onclick = (e) => {
      if (e.detail > 1) return;
      const session = sessionLinkClick(e);
      if (session) {
        location.hash = '#requests?' + requestsFilterQuery({ session });
        return;
      }
      if (e.target.closest('a, button')) return;
      if (analyzable) analyzeSecurityHit(tr, key, rec);
    };
  });
  restoreSecurityDetails(rows);
  // No leaderboard rebuild here: this render also runs on rule-filter clicks
  // (setSecurityRule), and replacing leaderboard rows mid-gesture kills the
  // double-click text selection being made on them. The loaders rebuild it
  // when their data actually changes.
  syncRuleSel();
}

// fmtMs renders a unix-millisecond audit timestamp as "MM-DD HH:MM:SS" —
// the audit log spans days (30d retention), so a time-only format is wrong.
function fmtMs(ms) {
  if (!ms) return '—';
  const d = new Date(Number(ms));
  if (isNaN(d.getTime())) return '—';
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${d.toLocaleTimeString('en-US', { hour12: false })}`;
}

// renderSecurityTab builds the guard audit-log view: KPI tiles, the
// blocked-session card, the rule leaderboard, and the merged Activity feed
// with its filter row. On-demand (no poll) — fetch happens on tab entry and on
// Refresh, like Requests. Records carry pattern/path NAMES and the action
// only; matched content never reaches the API, so every field is safe to
// render verbatim.
// Security auto-refresh: a 30s background tick while the tab is the ACTIVE
// view. The tick passes through the interaction gate (open select/popover,
// focused control, text selection defers it; the hold watcher re-runs the
// refresh when the interaction ends). The loaders render only their own
// data hosts — the toolbar selects are never rebuilt — so a tick can never
// close a dropdown; failures keep the last successful halves on screen via
// securityRefreshFail's stale banner (first-load failures still fall back
// to the inline error card).
let securityRefreshTimer = null;

function securityStopAutoRefresh() {
  if (securityRefreshTimer) {
    clearInterval(securityRefreshTimer);
    securityRefreshTimer = null;
  }
  cancelAutoRefreshHold(panels.security);
}

function securityMaybeAutoRefresh() {
  securityStopAutoRefresh();
  securityRefreshTimer = setInterval(() => {
    const panel = panels.security;
    if (!panel || !panel.classList.contains('active')) return;
    if (deferAutoRefresh(panel, () => refreshSecurityData())) return;
    refreshSecurityData();
  }, 30000);
}

async function renderSecurityTab() {
  const panel = panels.security;
  if (!panel) return;
  if (await retainTab(panel, '#sec-table', () => { refreshSecurityData(); securityMaybeAutoRefresh(); })) {
    securityMaybeAutoRefresh();
    return;
  }
  securityStopAutoRefresh();
  // Seed the filter from the URL hash (#security?kind=…): a refresh or a
  // shared link must land on the same view, not the unfiltered list.
  const seeded = securityFilterFromQuery(parseHash().query);
  if (seeded) securityFilter = seeded;
  securityKpiData = { blocks: null, stats: null, counts: null };
  securityFeedData = { records: null, adjudications: null };
  securityRulesData = { records: null };
  // Information hierarchy: summary tiles first, then the actionable blocked
  // list with its unblock controls, then the merged chronological feed. The
  // rule leaderboard lives INSIDE Activity (a collapsible section between
  // the legend and the feed it filters) — its click-to-filter target is the
  // feed table right below it.
  panel.innerHTML = `<div id="sec-kpis"></div>
  <div class="card">
    <header class="card-head"><span class="card-head-title"><h2>Blocked sessions</h2><span class="meta">high verdicts + exact matches · persist until unblocked</span></span><span class="card-head-side"><button id="sec-unblock-all" class="btn danger" hidden>unblock all</button></span></header>
    <div class="card-body">
    <div id="sec-blocks"><span class="hint">loading…</span></div>
  </div></div>
  <div class="card">
    <header class="card-head"><span class="card-head-title"><h2>Activity</h2><span class="meta">audit + AI verdicts · newest first · low verdicts suppressed</span></span></header>
    <div class="card-body">
    <div class="req-controls" style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-bottom:12px;">
      <select id="sec-kind" class="req-input" title="filter by guard hit kind (server-side)">
        <option value="" ${securityFilter.kind === '' ? 'selected' : ''}>All Kinds</option>
        <option value="secret" ${securityFilter.kind === 'secret' ? 'selected' : ''}>Secret</option>
        <option value="path" ${securityFilter.kind === 'path' ? 'selected' : ''}>Path</option>
        <option value="drift" ${securityFilter.kind === 'drift' ? 'selected' : ''}>Drift</option>
      </select>
      <select id="sec-verdict" class="req-input" title="filter by adjudication verdict">
        <option value="" ${securityFilter.verdict === '' ? 'selected' : ''}>All Verdicts</option>
        <option value="high" ${securityFilter.verdict === 'high' ? 'selected' : ''}>High</option>
        <option value="medium" ${securityFilter.verdict === 'medium' ? 'selected' : ''}>Medium</option>
        <option value="error" ${securityFilter.verdict === 'error' ? 'selected' : ''}>Error</option>
        <option value="skipped" ${securityFilter.verdict === 'skipped' ? 'selected' : ''}>Skipped</option>
      </select>
      <select id="sec-range" class="req-input" title="audit query window">
        ${SECURITY_RANGES.map((r) => `<option value="${r.value}" ${securityFilter.range === r.value ? 'selected' : ''}>${r.label}</option>`).join('')}
      </select>
      <span id="sec-rule-chip"></span>
      <button id="sec-refresh" class="btn">${iconRefresh()}Refresh</button>
    </div>
    ${securityLegendHTML()}
    <details id="sec-rules" class="sec-rules" open>
      <summary>Rule hits <span class="meta">whole audit window · all kinds · independent of the feed filters</span></summary>
      <div id="sec-rules-body"></div>
    </details>
    <div id="sec-table"></div>
  </div></div>`;
  // kind and range change the SERVER query (a new window is a new query:
  // the row budget resets); verdict narrows the merged feed client-side.
  const reloadAudit = () => {
    securityFilter.kind = document.getElementById('sec-kind').value;
    securityFilter.range = document.getElementById('sec-range').value;
    securityLimit = 100;
    updateSecurityHash();
    loadSecurity();
  };
  document.getElementById('sec-refresh').onclick = () => refreshSecurityData();
  document.getElementById('sec-kind').onchange = reloadAudit;
  document.getElementById('sec-range').onchange = reloadAudit;
  document.getElementById('sec-verdict').onchange = () => {
    securityFilter.verdict = document.getElementById('sec-verdict').value;
    updateSecurityHash();
    renderSecurityFeed();
  };
  const unblockAll = document.getElementById('sec-unblock-all');
  if (unblockAll) unblockAll.onclick = () => unblockAllSessions();
  refreshSecurityData();
  securityMaybeAutoRefresh();
}

// syncSecurityControls pushes the filter state back into the tab's selects
// (a hash-driven filter change must reach them, or the next Refresh would
// read the stale DOM values back into the filter — same story as the
// Requests tab's syncRequestsFreeControls).

function syncSecurityControls() {
  const k = document.getElementById('sec-kind');
  if (k) k.value = securityFilter.kind;
  const v = document.getElementById('sec-verdict');
  if (v) v.value = securityFilter.verdict;
  const r = document.getElementById('sec-range');
  if (r) r.value = securityFilter.range;
}

// unblockAllSessions clears every persisted block (the blocked list is short
// and the action is reversible by re-judgment). Sequential DELETEs keep the
// failure semantics simple: stop on the first error, the refresh shows what
// actually cleared.
async function unblockAllSessions() {
  const btn = document.getElementById('sec-unblock-all');
  if (btn) btn.disabled = true;
  try {
    for (const b of securityKpiData.blocks || []) {
      try {
        await apiDel('/api/security/blocks/' + encodeURIComponent(b.session_id));
      } catch (e) {
        // Surface the first failure and stop; the table refresh shows what
        // actually cleared.
        const el = document.getElementById('sec-blocks');
        if (el) el.insertAdjacentHTML('afterbegin', `<div class="msg err">${esc(e.message)}</div>`);
        return;
      }
    }
    loadSecurityBlocks();
    loadSecurity();
  } finally {
    if (btn) btn.disabled = false;
  }
}

// refreshSecurityData reloads the audit table and the guard cards. Shared by
// the first mount and every tab re-entry (which keeps the rendered cards on
// screen instead of re-flashing the loading skeleton).
function refreshSecurityData() {
  loadSecurity();
  loadSecurityAdjudications();
  loadSecurityBlocks();
  loadSecurityRules();
}

// loadSecurityRules feeds the Rule-hits leaderboard: a second audit query
// with FIXED parameters (no kind, no from — the whole 30d retention — at the
// backend's 1000-row cap), deliberately independent of the Activity filters.
// The feed's kind/range selects go server-side on ITS query; sharing that
// response would let a feed filter silently re-scope the leaderboard (narrow
// the feed to path hits and every secret rule would vanish from the ops
// view). Same stale-on-failure contract as the other loaders: banner part
// "rules", old data stays on screen.
async function loadSecurityRules() {
  const seq = ++securityRulesSeq;
  let resp;
  try {
    resp = await apiGet('/api/security?limit=1000');
  } catch (e) {
    if (seq !== securityRulesSeq) return;
    securityRefreshFail('rules');
    return;
  }
  if (seq !== securityRulesSeq) return;
  securityRulesData.records = resp.records || [];
  securityRefreshOk('rules');
  commitSecurityRender();
}

// loadSecurityAdjudications feeds the AI-verdict half of the KPI row and
// the merged activity feed (suppressed low verdicts are visible in the feed
// and nowhere else). Failures keep the last successful data (never blank the
// halves already rendered) and surface through the shared banner.
async function loadSecurityAdjudications() {
  const tbl = document.getElementById('sec-table');
  let resp;
  try {
    resp = await apiGet('/api/security/adjudications');
  } catch (e) {
    if (securityRefreshFail('adjudications') && tbl && !tbl.firstElementChild) {
      tbl.innerHTML = `<div class="msg err">${esc(e.message)}</div>`;
    }
    return;
  }
  securityRefreshOk('adjudications');
  const recs = (resp && resp.adjudications) || [];
  securityAdjudicationEnabled = !!(resp && resp.enabled);
  securityKpiData.stats = (resp && resp.stats) || {};
  securityFeedData.adjudications = recs;
  commitSecurityRender();
}

// renderRuleLeaderboard paints the rule-ops table INSIDE Activity (the
// collapsible #sec-rules section above the feed): per-rule hit counts over
// the whole audit window from the leaderboard's OWN unfiltered audit slice
// (see loadSecurityRules — the Activity kind/range filters must not change
// these counts) plus suppressed low verdicts from the ring. Pattern rules
// piling up hits while the AI second opinion is OFF get the "enable
// adjudicate" hint — high-frequency false-positive rules are exactly the
// channel's use case (the threshold avoids hinting on one-off hits). Rows
// are drill targets: clicking one narrows the Activity feed to that rule
// (click again / the chip ✕ clears). Only the body div is rewritten — the
// <details> wrapper (and the user's open/collapsed choice) survives data
// refreshes; an empty leaderboard hides the section entirely.
function renderRuleLeaderboard() {
  const wrap = document.getElementById('sec-rules');
  const el = document.getElementById('sec-rules-body');
  if (!wrap || !el) return;
  const rows = ruleHitsLeaderboard(securityRulesData.records, securityFeedData.adjudications);
  if (!rows.length) {
    wrap.hidden = true;
    el.innerHTML = '';
    return;
  }
  wrap.hidden = false;
  const top = rows.slice(0, 8);
  const kindBadge = { secret: 'warn', path: '', drift: 'muted' };
  const body = top.map((r) => {
    const hint = r.adjudicable && r.hits >= 3 && !securityAdjudicationEnabled
      ? ' <span class="badge warn" title="Pattern hits at this frequency are usually benign fixtures/docs — the AI second opinion can suppress them (guard.adjudicate)">noisy — adjudicate can suppress</span>'
      : '';
    const sel = securityFilter.rule === r.name ? ' sel' : '';
    return `<tr class="sec-rule${sel}" data-rule="${esc(r.name)}" title="filter the Activity feed by this rule">
      <td class="mono">${esc(r.name)}</td>
      <td><span class="badge ${kindBadge[r.kind] || ''}">${esc(r.kind)}</span></td>
      <td class="num">${fmtNum(r.hits)}</td>
      <td class="mono">${esc(r.lastTs ? fmtMs(r.lastTs) : '—')}${hint}</td>
    </tr>`;
  }).join('');
  el.innerHTML = `<table class="table">
    <thead><tr><th>rule</th><th>kind</th><th class="num">hits</th><th>last seen</th></tr></thead>
    <tbody>${body}</tbody>
  </table>`;
  el.querySelectorAll('tr.sec-rule').forEach((tr) => {
    tr.onclick = (e) => {
      // The second click of a double-click (text selection) must not re-toggle
      // the filter: click one already re-rendered this row away, and running
      // the action again mid-gesture destroys the selection being made.
      if (e.detail > 1) return;
      setSecurityRule(securityFilter.rule === tr.dataset.rule ? '' : tr.dataset.rule);
    };
  });
}

// paintSecurityBlocks renders the persisted session-block table from stored
// data (called by securityRenderAll). The session cell is a session-link:
// blocked-session → "what did it send" is the natural investigation path,
// and the hash drill lands on the Requests tab with that session pinned
// (Back returns to Security).
function paintSecurityBlocks() {
  const el = document.getElementById('sec-blocks');
  if (!el) return;
  const blocks = securityKpiData.blocks || [];
  const all = document.getElementById('sec-unblock-all');
  if (all) all.hidden = blocks.length === 0;
  if (!blocks.length) {
    el.innerHTML = '<div class="msg hint">No blocked sessions.</div>';
    return;
  }
  const rows = blocks.map((b) => `<tr>
    <td class="mono session-link" data-session="${esc(b.session_id)}" title="${esc(b.session_id)} — view this session's requests">${esc(b.session_id)}</td>
    <td><span class="badge ${b.kind === 'path' ? '' : 'warn'}">${esc(b.kind)}</span></td>
    <td class="mono">${esc(b.rule)}</td>
    <td class="mono">${esc(b.reason || '—')}</td>
    <td class="mono">${esc(fmtMs(b.ts))}</td>
    <td class="mono">${esc(b.request_id || '—')}</td>
    <td><button class="btn sec-unblock" data-sid="${esc(b.session_id)}">unblock</button></td>
  </tr>`).join('');
  el.innerHTML = `<table class="table">
  <thead><tr><th>session</th><th>kind</th><th>rule</th><th>reason</th><th>since</th><th>request</th><th></th></tr></thead>
  <tbody>${rows}</tbody></table>`;
  el.querySelectorAll('tr').forEach((tr) => {
    tr.onclick = (e) => {
      // Same double-click guard as the rule leaderboard: the second click of
      // a selection gesture must not re-fire the drill navigation.
      if (e.detail > 1) return;
      const session = sessionLinkClick(e);
      if (session) location.hash = '#requests?' + requestsFilterQuery({ session });
    };
  });
  el.querySelectorAll('.sec-unblock').forEach((btn) => {
    btn.onclick = async (e) => {
      e.stopPropagation();
      btn.disabled = true;
      try {
        await apiDel('/api/security/blocks/' + encodeURIComponent(btn.dataset.sid));
        loadSecurityBlocks();
        loadSecurity();
      } catch (err) {
        btn.disabled = false;
        el.insertAdjacentHTML('afterbegin', `<div class="msg err">${esc(err.message)}</div>`);
      }
    };
  });
}

// loadSecurityBlocks fetches the persisted session blocks (with per-row
// unblock, DELETE /api/security/blocks/<id>); errors surface the backend
// message. Painting goes through the gated commit like every loader.
async function loadSecurityBlocks() {
  const el = document.getElementById('sec-blocks');
  if (!el) return;
  let resp;
  try {
    resp = await apiGet('/api/security/blocks');
  } catch (e) {
    // Keep the last successful table on screen; only a first-load failure
    // (nothing rendered yet) shows the inline error card.
    if (securityRefreshFail('blocks') && !el.querySelector('table')) {
      el.innerHTML = `<div class="msg err">${esc(e.message)}</div>`;
    }
    return;
  }
  securityRefreshOk('blocks');
  securityKpiData.blocks = (resp && resp.blocks) || [];
  commitSecurityRender();
}

async function loadSecurity() {
  const tbl = document.getElementById('sec-table');
  // Keep the previous table visible while the fetch runs; only an empty
  // table shows the loading hint (a refresh must not flash blank).
  if (tbl && !tbl.firstElementChild) tbl.innerHTML = '<span class="hint">loading…</span>';
  const q = new URLSearchParams();
  // The audit half is the dense historical tail; the KPI tiles carry the
  // digest. "show more" deepens this to the backend's 1000 cap.
  q.set('limit', String(securityLimit));
  // kind and the range preset go server-side — a client-side filter would
  // only narrow the newest N rows and hide older history forever.
  if (securityFilter.kind) q.set('kind', securityFilter.kind);
  const from = securityRangeFromSecs(securityFilter.range);
  if (from) q.set('from', String(from));
  // Sequence guard: a slow older response must not overwrite the newer one's
  // rendering.
  const seq = ++securityReqSeq;
  let resp;
  try {
    resp = await apiGet('/api/security?' + q.toString());
  } catch (e) {
    if (seq !== securityReqSeq) return;
    if (securityRefreshFail('security') && tbl && !tbl.firstElementChild) {
      tbl.innerHTML = `<div class="msg err">${esc(e.message)}</div>`;
    }
    return;
  }
  if (seq !== securityReqSeq) return;
  securityAuditOn = !!resp.enabled;
  securityFeedSkipped = resp.skipped || 0;
  securityFeedData.records = resp.records || [];
  // Server-side verdict aggregation over the same window (the KPI tiles read
  // this; counting the client-merged feed drifted with the in-memory ring).
  securityKpiData.counts = resp.counts || null;
  securityRefreshOk('security');
  commitSecurityRender();
}

// analyzeSecurityHit toggles one row's inline analysis expansion: on expand
// it fetches /api/security/explain ON DEMAND (never prefetched) and renders
// located, highlighted match snippets; a second click collapses (the trigger
// is a click on the row itself — there is no button column). The open
// state rides securityExpanded, so the feed re-render that follows a Refresh
// or auto-refresh tick restores the expansion instead of silently collapsing
// it. Rows sharing one key (same request + kind + names — e.g. repeated
// cached verdicts of one request) share one expansion.
function analyzeSecurityHit(tr, key, rec) {
  if (!rec || !key) return;
  if (securityExpanded.has(key)) {
    securityExpanded.delete(key);
    removeSecurityDetail(key);
    return;
  }
  securityExpanded.add(key);
  insertSecurityDetail(tr, key, rec);
}


// insertSecurityDetail builds the expansion row under tr. A cached result
// paints immediately; anything else (nothing yet, or a fetch still in flight)
// parks an "analyzing…" placeholder that the fetch's settle handler fills.
function insertSecurityDetail(tr, key, rec) {
  const detailTr = document.createElement('tr');
  detailTr.className = 'sec-detail';
  detailTr.dataset.secKey = key;
  const td = document.createElement('td');
  td.colSpan = 7;
  const cached = securityExplainCache.get(key);
  if (cached && typeof cached.then !== 'function') {
    td.innerHTML = securityExplainHTML(cached);
  } else {
    td.innerHTML = '<span class="hint">analyzing…</span>';
    securityExplainFor(key, rec);
  }
  detailTr.appendChild(td);
  tr.after(detailTr);
}

// removeSecurityDetail drops the expansion row(s) for one key (toggle-off).
function removeSecurityDetail(key) {
  for (const tr of document.querySelectorAll('#sec-table tr.sec-detail')) {
    if (tr.dataset.secKey === key) tr.remove();
  }
}

// securityExplainFor returns the (possibly still in-flight) explain result
// for key, fetching it at most once: the promise itself is cached so a
// re-render's restore racing the original click shares one request. On
// settle the promise entry is swapped for the result (dropped on error) and
// every connected expansion row is painted — the row the fetch started
// under may have been replaced by a re-render in the meantime.
function securityExplainFor(key, rec) {
  const hit = securityExplainCache.get(key);
  if (hit) return hit;
  const q = new URLSearchParams({
    request_id: rec.requestId,
    kind: rec.kind,
    name: (rec.names || []).join(','),
  });
  const p = apiGet('/api/security/explain?' + q.toString()).then(
    (resp) => {
      securityExplainSet(key, resp);
      paintSecurityDetail(key);
      return resp;
    },
    (e) => {
      securityExplainCache.delete(key);
      paintSecurityDetail(key, `<div class="msg err">${esc(e.message || String(e))}</div>`);
      return null;
    },
  );
  securityExplainSet(key, p);
  return p;
}

// securityExplainSet stores a result or in-flight promise and keeps the cache
// bounded: oldest-first eviction, skipping keys with a live expansion (their
// entry is what the next restore paints from).
function securityExplainSet(key, value) {
  securityExplainCache.delete(key);
  securityExplainCache.set(key, value);
  while (securityExplainCache.size > SECURITY_EXPLAIN_CACHE_MAX) {
    let evicted = false;
    for (const k of securityExplainCache.keys()) {
      if (securityExpanded.has(k)) continue;
      securityExplainCache.delete(k);
      evicted = true;
      break;
    }
    if (!evicted) break; // everything resident is expanded — let the cap slide
  }
}

// paintSecurityDetail writes rendered explain HTML (or a ready-made error
// block) into every connected expansion row for key. html omitted = paint
// from cache ("analyzing…" placeholder while the entry is still a promise).
function paintSecurityDetail(key, html) {
  for (const tr of document.querySelectorAll('#sec-table tr.sec-detail')) {
    if (tr.dataset.secKey !== key) continue;
    const cached = securityExplainCache.get(key);
    tr.cells[0].innerHTML = html !== undefined
      ? html
      : (cached && typeof cached.then !== 'function' ? securityExplainHTML(cached) : '<span class="hint">analyzing…</span>');
  }
}

// restoreSecurityDetails re-inserts the expansions the user opened before
// this table rebuild (Refresh / auto-refresh / filter change wiped them with
// the innerHTML swap). Row→tr correlation goes through the HTML-safe row
// index (data-sec-i), never the explain key itself. One expansion per key:
// the first row carrying it wins, so repeated identical rows (cached verdict
// floods) show one canonical detail instead of one per row.
function restoreSecurityDetails(rows) {
  const idxByKey = new Map();
  (rows || []).forEach((r, i) => {
    if (!r.requestId || (r.kind !== 'secret' && r.kind !== 'path')) return;
    const key = explainCacheKey(r.requestId, r.kind, r.names);
    if (!idxByKey.has(key)) idxByKey.set(key, i);
  });
  for (const key of securityExpanded) {
    const i = idxByKey.get(key);
    if (i === undefined) continue;
    const tr = document.querySelector(`#sec-table tr[data-sec-i="${i}"]`);
    if (!tr) continue;
    if (tr.nextElementSibling && tr.nextElementSibling.classList.contains('sec-detail')) continue;
    insertSecurityDetail(tr, key, rows[i]);
  }
}

// ---------- Live monitor (Status → Live section, SSE /api/events) ----------

let liveES = null;       // the EventSource for /api/events (null when not connected)
let liveActive = false;  // the section's card is mounted and connected
let liveRows = [];       // newest-first ring of merged request rows (capped)
let liveByReq = {};      // request_id -> row object (while in the ring)
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
let liveSessionOptionsKey = '';
// Boot-time #status/live?session=… pin, consumed by renderLiveCard right
// after the mount that resets the selection (see there for the race).
let bootLiveSession = '';

// renderLiveCard mounts the live request monitor into the Status Live section
// and opens the SSE connection (closed by stopLiveEvents when the section or
// tab is left). Rows are merged per request: a start event opens a dimmed
// in-flight row; guard hits attach a ⚑ badge and accumulate in the row; the
// end event fills in provider/status/latency/tokens. Clicking a request row
// opens the full detail in a modal popover (fetched from /api/requests/<id>
// once the request ends) — the popover lives in the top layer, so list
// refreshes never disturb it. Non-request events without a known request id
// (budget & friends) still render as standalone one-line rows. The All/live
// table is updated incrementally on each SSE event; the session panel keeps
// its full re-render (scroll anchor preserved).
function renderLiveCard(target) {
  stopLiveEvents();
  liveRows = [];
  liveByReq = {};
  liveDetailState.clear();
  livePendingGuards.clear();
  liveEventSeq = 0;
  liveSessionFilter = '';
  liveSessionRecords = [];
  liveSessionAgg = null;
  liveSessionList = [];
  liveSessionLoading = false;
  liveSessionError = '';
  liveSessionOptionsKey = '';
  target.insertAdjacentHTML('beforeend', buildCard('Live requests', '',
    `<div class="live-toolbar">
       <label class="hint" for="live-session">Session</label>
       <select id="live-session" class="req-input"><option value="">All (live)</option></select>
     </div>
     <div id="live-table"><span class="msg hint">connecting…</span></div>
     <div id="live-session-panel" hidden></div>`, 'tight', '', 'card-open'));
  const sel = document.getElementById('live-session');
  if (sel) {
    sel.onchange = () => onLiveSessionChange(sel.value);
    // Retry an options rebuild deferred by the focused-guard above: the next
    // SSE event may be far away, and a closed dropdown must not leave the
    // session list stale until it arrives.
    sel.onblur = () => refreshLiveSessionOptions();
  }
  // Consume a boot-time #status/live?session=… pin here: this mount is the
  // point that resets liveSessionFilter, and boot's direct apply raced it
  // (the status tab renders async, so the card mounted AFTER boot applied
  // and wiped the selection — refresh lost the ?session= param).
  if (bootLiveSession) {
    const v = bootLiveSession;
    bootLiveSession = '';
    onLiveSessionChange(v);
  }
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
    // An event for the request whose detail popover is open updates the
    // popover in place (stream progress, end status, guard hits).
    if (liveDetailPopId && e.request_id === liveDetailPopId) updateLiveDetailPop();
  };
}

// refreshLiveSessionOptions rebuilds the session dropdown from live rows plus
// the persisted session list, ordered most-recently-active first (session id
// ascending as the tie-break; pure.js liveSessionOrder). Rebuilds only when
// the ordered set changes so a quiet stream does not reset the control. While
// the select holds focus (its native dropdown may be open, or the user is
// keyboard-navigating it) the options are NEVER swapped — replacing them
// closes the OS-drawn popup; the blur handler below retries once the user is
// done.
function refreshLiveSessionOptions() {
  const sel = document.getElementById('live-session');
  if (!sel) return;
  if (sel === document.activeElement) return;
  let sorted = liveSessionOrder(liveSessionList, liveRows);
  // Keep the active selection as an option even when neither source knows it
  // (a hash-restored session whose live rows aged out of the ring / whose
  // /api/sessions entry is still loading) — same guarantee as the Requests
  // dropdown, so the visible selection never silently reverts to all-live.
  if (liveSessionFilter && !sorted.includes(liveSessionFilter)) {
    sorted = [liveSessionFilter, ...sorted];
  }
  const key = sorted.join('\n');
  if (key === liveSessionOptionsKey) return;
  liveSessionOptionsKey = key;
  // v2: the Live session dropdown shows the FULL id — this toolbar holds a
  // single control with the rest of the row empty, so unlike the dense
  // Requests filter row there is no reason to abbreviate (and the select is
  // widened via .live-toolbar select to match; see styles.css).
  sel.innerHTML = '<option value="">All (live)</option>' +
    sorted.map((id) => `<option value="${esc(id)}">${esc(id)}</option>`).join('');
  sel.value = liveSessionFilter;
}

// (v2: liveSessionLabel — the 8…4 session-id abbreviation — was removed;
// both the Live and Requests dropdowns now show full ids.)

// onLiveSessionChange switches between the live table and one session's
// analysis, loading the persisted request list + aggregate for the selection.
function onLiveSessionChange(value) {
  liveSessionFilter = value;
  // Mirror the selection into the URL (#status/live?session=…) so refresh/
  // shared links restore this view; replaceState keeps dropdown refinement
  // out of the back-history. setHash is a no-op when the hash already
  // matches (boot/hashchange apply paths), so no loop.
  if (activeTab === 'status') setHash(statusHash(), false);
  liveSessionRecords = [];
  liveSessionAgg = null;
  liveSessionError = '';
  // Entering a session from a record (row cell / detail popover) must move
  // the dropdown selection too; setting value programmatically fires no
  // change event, so no loop.
  const sel = document.getElementById('live-session');
  if (sel) sel.value = value;
  const tbl = document.getElementById('live-table');
  const panel = document.getElementById('live-session-panel');
  if (!value) {
    // All (live) mode: the pure live ring, no persisted backfill.
    if (tbl) tbl.hidden = false;
    if (panel) { panel.hidden = true; panel.innerHTML = ''; syncSessThOffset(panel); }
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

// enterLiveSession switches the Live card into one session's view from a
// single record — the summary row's session cell or the detail popover's
// session link — selecting it in the dropdown and loading the session panel.
function enterLiveSession(id) {
  if (!id) return;
  closeLiveDetailPop();
  // The click that landed here blurred the select, so a rebuild deferred by
  // the focused-guard can run now and the option exists before selection.
  refreshLiveSessionOptions();
  onLiveSessionChange(id);
}

// wireLiveRow binds one summary row's click behavior: the session cell jumps
// to that session's view, anywhere else on the row opens the detail popover.
// Shared by every render path (full table, incremental prepend, in-place row
// update, session panel) so the affordance cannot drift between them.
function wireLiveRow(tr) {
  tr.onclick = (e) => {
    const session = sessionLinkClick(e);
    if (session) {
      enterLiveSession(session);
      return;
    }
    openLiveDetailPop(tr.dataset.id);
  };
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
    ttftMs: rec.ttft_ms != null ? rec.ttft_ms : null,
    attempt: rec.attempt || 0,
    input: rec.input || 0,
    output: rec.output || 0,
    cacheRead: rec.cache_read || 0,
    cacheCreation: rec.cache_creation || 0,
    shadow: !!rec.shadow,
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
function sessionSummaryHTML(s, opts, health) {
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
  // Identity footer: small uppercase key + mono value pairs, a deliberate
  // step down from the metric chips above (metrics = chips, identity =
  // key/value line). Comma-joined when a session spans several.
  const item = (k, vals) => (vals && vals.length)
    ? `<span class="sess-meta-item"><span class="sess-meta-k">${esc(k)}</span><span class="sess-meta-v">${esc(vals.join(', '))}</span></span>`
    : '';
  const meta = item('provider', s.providers) + item('model', s.models);
  return `<div class="live-session-summary">${chips}</div>${sessionHealthChipsHTML(health)}${meta ? `<div class="sess-meta">${meta}</div>` : ''}`;
}

// fmtDurMs is the chip-tier duration format (ms → s → m → h).
function fmtDurMs(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return '0ms';
  if (ms < 1000) return Math.round(ms) + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  if (ms < 3600000) return Math.round(ms / 60000) + 'm';
  return (ms / 3600000).toFixed(1) + 'h';
}

// sessionHealthChipsHTML renders the second chip row (health signals from
// pure.js sessionHealthSummary). Every chip drops out when its signal is
// absent; failovers carry the warn tone — a retrying session is the thing
// this row exists to surface.
function sessionHealthChipsHTML(h) {
  if (!h) return '';
  const dur = fmtDurMs;
  const chips = [];
  if (h.spanMs > 0) chips.push({ text: `span ${dur(h.spanMs)}${h.activeMs > 0 ? ` · active ${dur(h.activeMs)}` : ''}` });
  if (h.p50Ms != null) chips.push({ text: `p50 ${dur(h.p50Ms)} · p95 ${dur(h.p95Ms)}` });
  if (h.ttftP50Ms != null) chips.push({ text: `ttft p50 ${dur(h.ttftP50Ms)}` });
  if (h.failovers > 0) chips.push({ text: `${h.failovers} failover${h.failovers === 1 ? '' : 's'}`, cls: 'warn' });
  if (h.cacheHitPct != null && h.cacheHitPct > 0) chips.push({ text: `cache ${h.cacheHitPct}%` });
  if (h.tokPerSec) chips.push({ text: `${h.tokPerSec} tok/s` });
  if (h.models.length > 1) {
    const top = h.models.slice(0, 2).map((m) => `${m.model} ×${m.n}`).join(' · ');
    const more = h.models.length - 2;
    chips.push({ text: more > 0 ? `${top} +${more}` : top });
  }
  if (h.shadow > 0) chips.push({ text: `${h.shadow} shadow` });
  if (!chips.length) return '';
  return `<div class="sess-health">${chips.map((c) => `<span class="live-chip${c.cls ? ' ' + c.cls : ''}">${esc(c.text)}</span>`).join('')}</div>`;
}

// sessionViewHTML renders the shared SESSION VIEW — summary chips plus the
// trace timeline — consumed by BOTH the Live session panel and the Requests
// session summary. The pages own only the table below it (Live merges
// in-flight rows and opens the detail popover; Requests expands inline) and
// wire the timeline bars through wireSessionTimeline with their own detail
// opener. opts.session keys the timeline zoom (drag-select) state; pass it
// whenever the owning page also passes a zoom rerender. One implementation:
// a chip or timeline change lands on both pages.
function sessionViewHTML(rows, agg, opts) {
  const o = opts || {};
  const s = liveSessionSummary(rows, agg);
  return sessionSummaryHTML(s, o, sessionHealthSummary(rows)) + sessionTimelineCard(rows, o);
}

// Timeline zoom lives OUTSIDE the render cycle: the Live panel re-renders on
// every SSE session event and the Requests table repaints on refresh, so a
// zoom held in local state would reset on the next event. Keyed by session
// id — switching sessions drops the stale window (session ids are unique).
const sessionZoomState = { key: '', from: 0, to: 0 };
function sessionZoomFor(session) {
  return session && sessionZoomState.key === session
    ? { from: sessionZoomState.from, to: sessionZoomState.to }
    : null;
}

// wireSessionTimeline binds one session view's timeline: bars open the owning
// page's detail (Live: popover; Requests: inline row expand + locate) and on
// hover show the shared tooltip (metadata + lazily fetched response excerpt);
// the reset chip clears the zoom, and a horizontal drag on the SVG selects a
// time window to zoom into. opts = {session, rerender, rows} — rows backs the
// hover summary; without opts only the bar clicks are bound.
function wireSessionTimeline(host, openDetail, opts) {
  const rows = (opts && opts.rows) || [];
  host.querySelectorAll('.tl-bar').forEach((bar) => {
    bar.addEventListener('click', () => openDetail(bar.dataset.id));
    if (!rows.length) return;
    bar.addEventListener('mouseenter', (e) => {
      if (tlTipTimer) clearTimeout(tlTipTimer);
      tlTipTimer = setTimeout(() => {
        tlTipTimer = 0;
        tlTipPos = { x: e.clientX, y: e.clientY };
        const row = rows.find((r) => r && String(r.requestId) === bar.dataset.id) || null;
        showTlTip(bar, row);
      }, 90);
    });
    bar.addEventListener('mouseleave', hideTlTip);
    bar.addEventListener('pointerdown', hideTlTip);
  });
  const zoom = opts && opts.session ? opts : null;
  if (!zoom || !zoom.rerender) return;
  const reset = host.querySelector('.tl-reset');
  if (reset) {
    reset.addEventListener('click', () => {
      sessionZoomState.key = '';
      zoom.rerender();
    });
  }
  const svg = host.querySelector('.tl-svg');
  if (svg && svg.dataset.segs) wireTimelineZoom(svg, zoom);
}

// syncSessThOffset makes the session table's headers stick right BELOW the
// pinned session view. A sticky offset cannot see a sibling's height, so the
// pinned view is measured here and exported as --sess-h on the host card;
// the th top calc adds it (0 whenever no session view is mounted — hidden
// hosts measure 0). host is the sticky element itself or a container that
// holds one (.sess-sticky). Re-run after every session-view render, after
// hiding it, and on window resize (the trace SVG's height rides its aspect
// ratio, so the panel height changes with the viewport width).
function syncSessThOffset(host) {
  const card = host && host.closest ? host.closest('.card') : null;
  if (!card) return;
  const sticky = host.classList && host.classList.contains('sess-sticky')
    ? host
    : (host.querySelector ? host.querySelector('.sess-sticky') : null);
  const h = sticky && sticky.isConnected ? sticky.offsetHeight : 0;
  // Flush geometry: the panel pins directly under the topbar and the th
  // directly under the panel — no gap strips where scrolled rows would show.
  card.style.setProperty('--sess-h', (h || 0) + 'px');
}

// ---------- timeline hover tooltip (shared session view) ----------
//
// Hovering a trace bar shows a metadata summary (pure.js sessionBarSummary)
// immediately, then lazily appends a short RESPONSE EXCERPT fetched from
// /api/requests/<id> — the list projection is UsageOnly, bodies only exist
// per record. The excerpt result is cached per request id (bounded) and the
// fetch shares cacheRequestDetail with the Requests detail rows, so a hover
// pre-warms the row expand.
//
// Deliberately NOT marked data-popup: the auto-refresh gate defers panel
// re-renders while a data-popup is open, and a transient hover hint must
// never stall the Live SSE re-renders — the re-render itself hides the
// tooltip (both session renderers call hideTlTip). pointer-events:none so it
// can never trap the cursor or steal a click.
const TL_TIP_EXCERPT_MAX = 220;
const TL_EXCERPT_CACHE_MAX = 60;
const tlExcerptCache = new Map(); // request id -> excerpt ('' = no text found)
let tlTipEl = null;
let tlTipTimer = 0;
let tlTipSeq = 0; // bump on hide: invalidates in-flight excerpt fills
let tlTipPos = { x: 0, y: 0 };

function tlTip() {
  if (!tlTipEl) {
    tlTipEl = document.createElement('div');
    tlTipEl.className = 'tl-tip';
    tlTipEl.hidden = true;
    document.body.appendChild(tlTipEl);
    window.addEventListener('scroll', hideTlTip, true);
    window.addEventListener('pointerdown', hideTlTip, true);
  }
  return tlTipEl;
}

function hideTlTip() {
  if (tlTipTimer) { clearTimeout(tlTipTimer); tlTipTimer = 0; }
  tlTipSeq++;
  if (tlTipEl) tlTipEl.hidden = true;
}

function placeTlTip(el) {
  el.style.left = '0px';
  el.style.top = '0px'; // reset before measuring so the clamp math is exact
  const r = el.getBoundingClientRect();
  let left = tlTipPos.x + 14;
  let top = tlTipPos.y + 16;
  if (left + r.width > window.innerWidth - 8) left = Math.max(8, tlTipPos.x - r.width - 14);
  if (top + r.height > window.innerHeight - 8) top = Math.max(8, tlTipPos.y - r.height - 14);
  el.style.left = left + 'px';
  el.style.top = top + 'px';
}

function fillTlExcerpt(el, ex, seq) {
  if (seq !== tlTipSeq || el.hidden) return;
  const inHost = el.querySelector('.tl-tip-in');
  const outHost = el.querySelector('.tl-tip-out');
  if (!inHost || !outHost) return;
  if (ex && ex.in) {
    inHost.textContent = ex.in;
    inHost.hidden = false;
  }
  if (ex && ex.out) {
    outHost.textContent = ex.out;
    outHost.hidden = false;
  }
  placeTlTip(el); // the tooltip grew: re-clamp against the viewport
}

function showTlTip(bar, row) {
  const id = bar.dataset.id || '';
  tlTipSeq++;
  const seq = tlTipSeq;
  const lines = sessionBarSummary(row, (t) => fmtTimeSafe(t));
  const el = tlTip();
  el.innerHTML = `<div class="tl-tip-meta">${lines.length ? lines.map((l) => `<div>${esc(l)}</div>`).join('') : esc(id)}</div><div class="tl-tip-ex tl-tip-in" hidden></div><div class="tl-tip-ex tl-tip-out" hidden></div>`;
  el.hidden = false;
  placeTlTip(el);
  const cached = tlExcerptCache.get(id);
  if (cached !== undefined) {
    fillTlExcerpt(el, cached, seq);
    return;
  }
  apiGet('/api/requests/' + encodeURIComponent(id)).then((resp) => {
    const recs = (resp && resp.records) || [];
    if (!recs.length) return;
    cacheRequestDetail(id, recs);
    // Two unlabeled preview paragraphs: this turn's user input first, then
    // the assistant's response — styling (muted vs normal) tells them apart.
    const ex = {
      in: requestExcerpt(recs[0].request_body, TL_TIP_EXCERPT_MAX),
      out: responseExcerpt(recs[0].response_body, TL_TIP_EXCERPT_MAX),
    };
    if (tlExcerptCache.size >= TL_EXCERPT_CACHE_MAX) tlExcerptCache.delete(tlExcerptCache.keys().next().value);
    tlExcerptCache.set(id, ex);
    fillTlExcerpt(el, ex, seq);
  }).catch(() => { /* in flight / not logged: skip — no negative caching */ });
}

// wireTimelineZoom drag-selects a time range on the timeline SVG. The pure
// renderer stamps its segment map (time ↔ viewBox px) as data-segs; inverting
// a drag through it yields the [from, to] window, stored in sessionZoomState
// and applied by the next render. Drags shorter than a few px fall through
// to the bar's own click (detail popover / inline expand).
function wireTimelineZoom(svg, zoom) {
  let segs = [];
  try { segs = JSON.parse(svg.dataset.segs || '[]'); } catch (_) { segs = []; }
  if (!segs.length) return;
  const pxToT = (clientX) => {
    const r = svg.getBoundingClientRect();
    const vw = svg.viewBox && svg.viewBox.baseVal ? svg.viewBox.baseVal.width : 900;
    const vx = ((clientX - r.left) / Math.max(r.width, 1)) * vw;
    for (const s of segs) {
      if (vx <= s.x1 || s === segs[segs.length - 1]) {
        return s.t0 + ((vx - s.x0) / Math.max(s.x1 - s.x0, 1)) * Math.max(s.t1 - s.t0, 1);
      }
    }
    return segs[segs.length - 1].t1;
  };
  let start = null;
  let selRect = null;
  let suppressed = false;
  svg.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    // Record only — do NOT capture here: pointer capture retargets the
    // release (and its derived click) to the SVG element, which silently
    // swallows every plain bar click. Capture is taken only once a drag
    // actually starts, below. A fresh press also clears the trailing-click
    // suppress flag — a drag that ended outside the SVG never produces that
    // click, and a stale flag would eat the next real bar click.
    suppressed = false;
    start = { x: e.clientX, id: e.pointerId };
  });
  svg.addEventListener('pointermove', (e) => {
    if (!start) return;
    if (!selRect && Math.abs(e.clientX - start.x) < 8) return;
    if (!selRect) {
      // The press became a drag: from here on capture the pointer so the
      // selection keeps tracking even outside the SVG, and the trailing
      // click (if the release lands on a bar) is suppressed separately.
      try { svg.setPointerCapture(start.id); } catch (_) { /* detached mid-drag */ }
      selRect = document.createElementNS(svg.namespaceURI, 'rect');
      selRect.setAttribute('class', 'tl-sel');
      svg.appendChild(selRect);
    }
    const r = svg.getBoundingClientRect();
    const vw = svg.viewBox && svg.viewBox.baseVal ? svg.viewBox.baseVal.width : 900;
    const clamp = (cx) => Math.min(Math.max(((cx - r.left) / Math.max(r.width, 1)) * vw, 0), vw);
    const x0 = clamp(start.x);
    const x1 = clamp(e.clientX);
    selRect.setAttribute('x', Math.min(x0, x1).toFixed(1));
    selRect.setAttribute('width', Math.max(Math.abs(x1 - x0), 1).toFixed(1));
    selRect.setAttribute('y', '0');
    selRect.setAttribute('height', '100%');
  });
  const cleanup = () => {
    if (selRect) { selRect.remove(); selRect = null; }
    if (start != null) {
      try { svg.releasePointerCapture(start.id); } catch (_) { /* already gone */ }
    }
    start = null;
  };
  svg.addEventListener('pointerup', (e) => {
    if (start == null) return;
    const sx = start.x;
    const hadRect = !!selRect;
    cleanup();
    if (!hadRect || Math.abs(e.clientX - sx) < 12) return; // a click, not a drag
    const from = Math.min(pxToT(sx), pxToT(e.clientX));
    const to = Math.max(pxToT(sx), pxToT(e.clientX));
    if (to - from < 1000) return; // <1s window: noise
    sessionZoomState.key = zoom.session;
    sessionZoomState.from = from;
    sessionZoomState.to = to;
    suppressed = true;
    zoom.rerender();
  });
  svg.addEventListener('pointercancel', cleanup);
  // The browser still fires a click after a drag that ended on a bar; swallow
  // it for one tick so zooming does not also pop the detail open.
  svg.addEventListener('click', (e) => {
    if (suppressed) {
      suppressed = false;
      e.stopPropagation();
    }
  }, true);
}

// renderLiveSessionPanel renders the selected session's analysis: the shared
// session view (summary chips + trace timeline), then the merged request
// table. The session view sits in a sticky container so it stays visible
// while the table scrolls (disabled on narrow screens — the wrapped topbar
// makes a fixed offset wrong there). Clicking a row opens the request detail
// in the modal popover.
function renderLiveSessionPanel() {
  const panel = document.getElementById('live-session-panel');
  if (!panel || !liveSessionFilter) return;
  if (liveSessionLoading) { panel.innerHTML = '<span class="hint">loading session…</span>'; syncSessThOffset(panel); return; }
  if (liveSessionError) { panel.innerHTML = `<div class="msg err">${esc(liveSessionError)}</div>`; syncSessThOffset(panel); return; }
  const rows = liveSessionRows();
  // The panel re-renders on every session event; snapshot the scroll anchor
  // first so a rebuild does not shift what the user is reading, and drop the
  // hover tooltip — its anchor bar is about to be replaced.
  const viewState = captureLiveViewState(panel);
  hideTlTip();
  const body = rows.length
    ? `<table class="table">${requestTableHeadHTML()}<tbody>${rows.map((r) => liveSummaryRowHTML(r, liveDetailPopId === r.requestId)).join('')}</tbody></table>`
    : '<div class="msg hint">no requests recorded for this session yet</div>';
  panel.innerHTML = `<div class="sess-sticky">${sessionViewHTML(rows, liveSessionAgg, { session: liveSessionFilter })}</div>${body}`;
  panel.querySelectorAll('.live-row').forEach((tr) => {
    wireLiveRow(tr);
  });
  wireSessionTimeline(panel, openLiveDetailPop, {
    session: liveSessionFilter,
    rerender: renderLiveSessionPanel,
    rows,
  });
  restoreLiveViewState(panel, viewState);
  syncSessThOffset(panel);
}

// sessionTimelineCard wraps the pure sessionTimeline SVG in the session
// panel's trace card: bars are the requests' spans (stacked swimlanes), the
// polyline is cumulative tokens (cost stays a session-level chip in the
// summary — pricing is server-owned), the amber dot marks a request that
// failover-retried (attempt > 0 from the request log) and red bars are
// error responses. In-flight requests run to their segment's right edge.
// Long idle gaps are compressed (dashed divider) so a short burst inside a
// multi-hour session stays readable; a drag on the SVG zooms into a time
// window (segment map rides data-segs for the px→time inversion), and the
// reset chip appears while a zoom is active.
function sessionTimelineCard(rows, opts) {
  const o = opts || {};
  const zoom = o.session ? sessionZoomFor(o.session) : null;
  const tl = sessionTimeline(rows, { fmt: (t) => fmtTimeSafe(t), window: zoom });
  if (!tl.svg) return '';
  // Chart-style legend: color swatches + short labels (a prose "bar = …"
  // line reads as a wall of text). The zoom control keeps its class and
  // wiring; drag-to-zoom stays as a quiet trailing hint when not zoomed.
  const legend = [
    `<span class="tl-lg"><i class="tl-swatch tl-sw-bar"></i>span</span>`,
    `<span class="tl-lg"><i class="tl-swatch tl-sw-line"></i>tokens</span>`,
    `<span class="tl-lg"><i class="tl-swatch tl-sw-retry"></i>retry</span>`,
    `<span class="tl-lg"><i class="tl-swatch tl-sw-err"></i>error</span>`,
  ];
  if (tl.segments.length > 1) legend.push(`<span class="tl-lg"><span class="tl-legend-gap">⫽</span> gap</span>`);
  const zoomChip = zoom
    ? `<button class="tl-reset" type="button" title="clear zoom window">zoomed ${esc(fmtTimeSafe(zoom.from))}–${esc(fmtTimeSafe(zoom.to))} · reset</button>`
    : `<span class="tl-lg">drag to zoom</span>`;
  return `<div class="card sess-tl"><div class="card-body">
    <div class="tl-title"><span class="tl-title-name">Trace</span><span class="tl-count">${rows.length} requests · ${tl.lanes} lane${tl.lanes === 1 ? '' : 's'}</span><span class="tl-legend">${legend.join('')}${zoomChip}</span></div>
    ${tl.svg.replace('<svg ', `<svg data-segs="${esc(JSON.stringify(tl.segments))}" `)}
  </div></div>`;
}

function stopLiveEvents() {
  closeLiveDetailPop();
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
  // end because new rows are prepended.
  const tbl = document.getElementById('live-table');
  const tbody = tbl && tbl.querySelector('tbody');
  if (!tbody) return;
  let removed = 0;
  let node = tbody.lastElementChild;
  while (node && removed < excess) {
    const prev = node.previousElementSibling;
    if (node.dataset.id) liveDetailState.delete(node.dataset.id);
    node.remove();
    removed++;
    node = prev;
  }
}

// liveSummaryRowHTML returns the summary <tr> for one live request row — the
// SHARED requestRowHTML with the live specifics (guard badge, live-key, the
// popover-open highlight). `open` marks the row whose detail popover is open.
function liveSummaryRowHTML(r, open) {
  const guardCount = r.guardHits.length;
  const guardTitle = guardCount
    ? esc(r.guardHits.map((h) => fmtGuardDetail(h.detail)).join('\n'))
    : '';
  const guard = guardCount
    ? ` <span class="badge warn" title="${guardTitle}">⚑ guard${guardCount > 1 ? ' ×' + guardCount : ''}</span>`
    : '';
  return requestRowHTML(r, {
    rowClass: 'live-row' + (open ? ' live-open' : ''),
    liveKey: true,
    modelNote: guard,
    fmtTime: fmtTimeSafe,
  });
}

// liveEventRowHTML returns a one-line standalone row for non-request events.
function liveEventRowHTML(r) {
  return `<tr data-live-key="${esc(r.key)}">
    <td class="mono">${esc(fmtTimeSafe(r.ts))}</td>
    <td class="mono">${esc(r.agent || '—')}</td>
    <td colspan="6"><span class="badge warn" title="${esc(fmtGuardDetail(r.guardDetail))}">⚑ ${esc(fmtGuardDetail(r.guardDetail) || 'event')}</span></td>
  </tr>`;
}

// liveRowHTML returns the summary HTML for one live row (event-only rows have
// no detail popover). Used by full renders and by incremental prepends.
function liveRowHTML(r) {
  if (r.eventOnly) return liveEventRowHTML(r);
  return liveSummaryRowHTML(r, liveDetailPopId === r.requestId);
}

// renderLiveTable redraws the merged rows: one line per request. In-flight
// rows are dimmed with a pending marker; the tokens column reads "in / out".
// This is the full-rebuild path used on tab switches and as a fallback; the
// hot SSE path uses applyLiveEventDOM for targeted surgery. Because new rows
// are PREPENDED, the rebuild preserves the viewport (captureLiveViewState /
// restoreLiveViewState): when the page is scrolled away from the top the
// visible region does not shift.
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
    return;
  }
  const viewState = captureLiveViewState(tbl);
  tbl.innerHTML = `<table class="table">${requestTableHeadHTML()}<tbody>${rows.map((r) => liveRowHTML(r)).join('')}</tbody></table>`;
  document.querySelectorAll('#live-table .live-row').forEach((tr) => wireLiveRow(tr));
  restoreLiveViewState(tbl, viewState);
}

// captureLiveViewState snapshots, before a full-table rebuild, the scroll
// anchor: a visible row highlighted by the open detail popover wins (the user
// is reading it); otherwise the first visible row when the page is scrolled
// away from the top. scrollY ≈ 0 keeps the natural "pinned to newest"
// behavior.
function captureLiveViewState(tbl) {
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
  return { anchor };
}

// restoreLiveViewState re-applies captureLiveViewState after the rebuild:
// scroll so the anchor row sits exactly where it was. A trimmed-out anchor
// (ring overflow) degrades to no adjustment.
function restoreLiveViewState(tbl, state) {
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
  tr.insertAdjacentHTML('beforebegin', liveSummaryRowHTML(r, liveDetailPopId === r.requestId));
  const next = tr.previousElementSibling;
  tr.remove();
  wireLiveRow(next);
  return next;
}

// captureLiveDetailOpenBodies snapshots which <details> are open inside one
// detail container (the popover body), keyed "recIndex:ordinal" so incremental
// content replacements can preserve them. The record HTML is deterministic
// across re-renders and body chunks only append, so ordinals are stable; this
// covers the request/response body containers AND the nested over-long-line
// blocks.
function captureLiveDetailOpenBodies(container) {
  const openBodies = new Set();
  container.querySelectorAll('.req-rec').forEach((rec, recIdx) => {
    rec.querySelectorAll('details').forEach((d, dIdx) => {
      if (d.open) openBodies.add(recIdx + ':' + dIdx);
    });
  });
  return openBodies;
}

// restoreLiveDetailOpenBodies re-opens the details captured by
// captureLiveDetailOpenBodies after an in-place content replacement.
function restoreLiveDetailOpenBodies(container, openBodies) {
  if (!openBodies.size) return;
  container.querySelectorAll('.req-rec').forEach((rec, recIdx) => {
    rec.querySelectorAll('details').forEach((d, dIdx) => {
      if (openBodies.has(recIdx + ':' + dIdx)) d.open = true;
    });
  });
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

// prependLiveRows inserts new rows at the top of the live tbody with scroll
// compensation so the visible viewport does not jump.
function prependLiveRows(tbl, tbody, rows) {
  const anchor = captureLiveScrollAnchor(tbl);
  const html = rows.map((r) => liveRowHTML(r)).join('');
  tbody.insertAdjacentHTML('afterbegin', html);
  for (const r of rows) {
    if (r.requestId) {
      const tr = findLiveSummaryRow(tbody, r.requestId);
      if (tr) wireLiveRow(tr);
    }
  }
  compensateLiveScroll(anchor);
}

// applyLiveEventDOM performs targeted DOM surgery for All/live mode after
// applyLiveEvent has updated liveRows/liveByReq. Session mode falls back to
// the full (scroll-preserving) panel re-render, and only for the selected
// session's events. start and eventOnly rows are prepended; end/guard events
// update the existing row in place; progress events only feed the detail
// popover (via the onmessage hook), the summary row does not change.
function applyLiveEventDOM(e) {
  if (liveSessionFilter) {
    // Session mode: only the selected session's events change the panel;
    // unrelated events must not rebuild it. The dropdown still picks up new
    // sessions cheaply.
    const owner = e.request_id && liveByReq[e.request_id];
    const relevant = e.session_id === liveSessionFilter ||
      (owner && owner.session === liveSessionFilter);
    if (!relevant) {
      refreshLiveSessionOptions();
      return;
    }
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
    updateLiveSummaryRow(tr, row);
    return;
  }

  if (e.type === 'progress') {
    // Summary rows carry no progress; the open detail popover is refreshed by
    // the onmessage hook.
    return;
  }

  // Guard/budget/other non-lifecycle events attached to a known request.
  if (e.request_id && liveByReq[e.request_id]) {
    const row = liveByReq[e.request_id];
    const tr = findLiveSummaryRow(tbody, row.requestId);
    if (!tr) return;
    updateLiveSummaryRow(tr, row);
    return;
  }

  // Event-only row (no request id): prepend it.
  const row = liveRows[0];
  if (row && row.eventOnly) prependLiveRows(tbl, tbody, [row]);
}

// liveGuardSectionHTML renders the accumulated guard hits for a live detail
// view. Extracted so popover updates can replace just this section.
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

// liveDetailHTML renders the popover content for one request row: accumulated
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
  // Ended but records not fetched yet; openLiveDetailPop starts the fetch.
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

// ---------- live request detail popover ----------
//
// Clicking a request row (All/live table or session panel) opens the request's
// full record in a modal <dialog>. The dialog lives in the top layer, outside
// #live-table / #live-session-panel, so any list refresh (SSE events, panel
// re-renders, ring trims) never touches it. Events for the open request update
// the dialog in place (stream progress, end status, guard hits), preserving
// open body <details> and the body scroll position.
let liveDetailPopId = null;  // request_id shown in the popover (null = closed)
let liveDetailPop = null;    // the lazily-created shared <dialog>

// liveRowById resolves the row object for a request id from the live ring or,
// in session mode, the merged persisted + live rows.
function liveRowById(id) {
  const live = liveByReq[id];
  if (live) return live;
  if (!liveSessionFilter) return null;
  return liveSessionRows().find((r) => r.requestId === id) || null;
}

function ensureLiveDetailPop() {
  if (liveDetailPop) return liveDetailPop;
  const dialog = document.createElement('dialog');
  dialog.className = 'live-detail-pop';
  dialog.innerHTML = `
    <div class="modal-head">
      <h2>request detail</h2>
      <span class="meta live-pop-meta"></span>
      <button type="button" class="btn small live-pop-close">Close</button>
    </div>
    <div class="modal-body live-pop-body"></div>`;
  dialog.querySelector('.live-pop-close').addEventListener('click', () => dialog.close());
  // A click landing on the dialog element itself is a backdrop click (the
  // content box is smaller than the top-layer dialog area).
  dialog.addEventListener('click', (e) => { if (e.target === dialog) dialog.close(); });
  dialog.addEventListener('close', () => {
    liveDetailPopId = null;
    document.querySelectorAll('#live-table tr.live-open, #live-session-panel tr.live-open')
      .forEach((tr) => tr.classList.remove('live-open'));
  });
  document.body.appendChild(dialog);
  liveDetailPop = dialog;
  return dialog;
}

// fillLiveDetailPop renders the meta line + body content for a row. The
// record detail leads with the chat transcript; the raw-body <details> stay
// collapsed by default (the user expands them when the exact bytes matter),
// and updateLiveDetailPop preserves the user's own open/closed state across
// refreshes.
function fillLiveDetailPop(dialog, row) {
  const meta = [
    fmtTimeSafe(row.ts),
    row.agent || '',
    row.model || '',
    row.provider || '',
    row.inFlight ? 'in flight' : (row.status || ''),
    (!row.inFlight && row.latencyMs != null) ? row.latencyMs + 'ms' : '',
    (row.input || row.output) ? `${fmtNum(row.input)} in / ${fmtNum(row.output)} out` : '',
  ].filter((x) => x !== '').join(' · ');
  const metaEl = dialog.querySelector('.live-pop-meta');
  metaEl.textContent = meta;
  // The session rides as a link instead of meta text: one click leaves the
  // single record and enters that session's view. Re-appended on every fill
  // (updateLiveDetailPop re-runs this), so no listener leaks.
  metaEl.querySelectorAll('.live-pop-session').forEach((b) => b.remove());
  if (row.session) {
    const btn = el('button', {
      cls: 'live-pop-session mono',
      text: 'session ' + shortSessionId(row.session),
      attrs: { type: 'button', title: row.session + ' — view this session' },
    });
    btn.addEventListener('click', () => enterLiveSession(row.session));
    metaEl.appendChild(btn);
  }
  const body = dialog.querySelector('.live-pop-body');
  // Rebuild the BODY only when its inputs changed. updateLiveDetailPop fires
  // on every SSE event for the open row (progress ticks ~4Hz), and a rebuild
  // re-runs detailRecordsHTML — whose chat view re-parses a possibly
  // multi-MB request body each time. The body depends on: the request, its
  // records (reference-stable until refetched), guard hits, and the
  // loading/error/not-logged state. Meta above always updates.
  const recs = requestsDetailCache.get(row.requestId);
  const state = liveDetailState.get(row.requestId) || {};
  const key = [
    row.requestId,
    row.inFlight ? 'inflight' : recs ? 'recs' : state.loading ? 'load' : state.notLogged ? 'nl' : state.error ? 'err:' + state.error : 'wait',
    (row.guardHits ? row.guardHits.length : 0),
  ].join('|');
  if (key !== livePopBodyKey) {
    livePopBodyKey = key;
    // Drop chunk state for body views that are about to be replaced.
    body.querySelectorAll('[data-chunk]').forEach((host) => {
      bodyChunkRegistry.delete(host.dataset.chunk);
    });
    dropRawBodies(body);
    body.innerHTML = liveDetailHTML(row);
  }
}
// livePopBodyKey memoizes fillLiveDetailPop's last body render (see there).
let livePopBodyKey = '';

// openLiveDetailPop opens the popover for one request row and starts the
// record fetch once the request has ended.
function openLiveDetailPop(id) {
  const row = liveRowById(id);
  if (!row) return;
  liveDetailPopId = id;
  // Re-opening after a fetch error clears it so the fetch is retried.
  const state = liveDetailState.get(id) || {};
  if (state.error || state.notLogged) liveDetailState.set(id, { ...state, error: '', notLogged: false });
  const dialog = ensureLiveDetailPop();
  fillLiveDetailPop(dialog, row);
  if (!dialog.open) dialog.showModal();
  const tr = document.querySelector(
    `#live-table tr.live-row[data-id="${esc(id)}"], #live-session-panel tr.live-row[data-id="${esc(id)}"]`);
  if (tr) tr.classList.add('live-open');
  if (!row.inFlight) ensureLiveDetailFetched(id, row);
}

function closeLiveDetailPop() {
  if (liveDetailPop && liveDetailPop.open) liveDetailPop.close();
  liveDetailPopId = null;
}

// updateLiveDetailPop refreshes the open popover after an event or record
// fetch for the same request, preserving open body <details> and scroll.
function updateLiveDetailPop() {
  if (!liveDetailPopId || !liveDetailPop || !liveDetailPop.open) return;
  const row = liveRowById(liveDetailPopId);
  if (!row) return;
  const body = liveDetailPop.querySelector('.live-pop-body');
  const openBodies = captureLiveDetailOpenBodies(body);
  const scrollTop = body.scrollTop;
  fillLiveDetailPop(liveDetailPop, row);
  restoreLiveDetailOpenBodies(body, openBodies);
  body.scrollTop = scrollTop;
  // The row may have just ended (popover opened while in flight): start the
  // record fetch now.
  if (!row.inFlight) ensureLiveDetailFetched(liveDetailPopId, row);
}

// ensureLiveDetailFetched starts a fetch for the requestlog records if the row
// has ended and the records are not already cached, in flight, or failed. A
// recorded error is terminal here (see shouldFetchDetail) so a 404 cannot loop.
function ensureLiveDetailFetched(id, row) {
  if (!shouldFetchDetail(requestsDetailCache.has(id), liveDetailState.get(id), !row || row.inFlight)) return;
  const state = liveDetailState.get(id) || {};
  liveDetailState.set(id, { ...state, loading: true, error: '' });
  fetchLiveDetail(id);
}

// fetchLiveDetail loads /api/requests/<id>, caches the records, and refreshes
// the popover if it is showing this request.
async function fetchLiveDetail(id) {
  let recs = [];
  try {
    const resp = await apiGet('/api/requests/' + encodeURIComponent(id));
    recs = resp.records || [];
  } catch (e) {
    liveDetailState.set(id, detailFetchState(e.status, e.message));
    if (liveDetailPopId === id) updateLiveDetailPop();
    return;
  }
  cacheRequestDetail(id, recs);
  liveDetailState.set(id, { loading: false, error: '' });
  if (liveDetailPopId === id) updateLiveDetailPop();
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
// statusSelected value; `label` is the nav button text. The default selection
// is statusSelected's initial value ('schedule'), not the first entry.
const STATUS_SECTIONS = [
  { key: 'dashboard', label: 'Dashboard' },
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
  cancelAutoRefreshHold(panels.status);
}

// refreshConnIndicator does a lightweight /api/status fetch solely to update
// the header connection dot + brand-meta (ok/err), independent of which tab is
// active. Called at boot so a refresh landing on #config or #accounts still
// shows the daemon's reachability (otherwise the header stays stuck on the
// initial "connecting…" - setConn('ok') otherwise only runs inside
// renderStatusTab, which the non-Status boot path skips). The inflight guard
// mirrors statusInflight: a stalled request must not let ticks pile up.
let connInflight = false;
async function refreshConnIndicator() {
  if (connInflight) return;
  connInflight = true;
  try {
    const st = await apiGet('/api/status');
    setConn('ok', `v${st.version || '?'} · ${st.uptime || '-'} · ${st.listen || ''}`);
  } catch (e) {
    setConn('err', 'connection lost');
  } finally {
    connInflight = false;
  }
}

// maybeConnRefresh arms the header-meta tick that keeps the brand-meta
// (version · uptime · listen) live on EVERY tab: renderStatusTab's 5s tick
// writes the header only while the Status tab is active, so without this
// timer the uptime froze at its boot value forever on
// Config/Accounts/Analytics/Requests/Security. The tick skips while the
// Status tab is active — its own /api/status fetch already refreshed the
// header, so no duplicate request. This tick is exempt from the
// auto-refresh interaction gate (documented exemption, pinned in
// jstests/autorefresh.test.mjs): setConn only swaps the dot class and one
// text node in the topbar chrome — no panel DOM is rebuilt, so no popup,
// selection, or in-progress input can be disrupted. Like the header itself
// it lives for the page's lifetime and has no per-tab teardown.
let connTimer = null;
function maybeConnRefresh() {
  if (connTimer) { clearInterval(connTimer); connTimer = null; }
  connTimer = setInterval(() => {
    if (activeTab === 'status') return;
    refreshConnIndicator();
  }, 5000);
}

// renderStatusTab fetches the dashboard snapshot (status + tokens + logs +
// accounts), caches it, and renders the Status panel: a Warnings banner (only
// when warnings exist) + a sidebar+detail layout. Only the active section's
// pane re-renders on each 5s tick; section switches render from the cache with
// no extra fetch. Auto-refreshes every 5s while the Status tab is active; the
// timer is cleared when the user leaves the tab. Background ticks and their
// renders pass through the interaction gate (autoRefreshBlocked): an open
// popover/menu, a focused control, or an active text selection defers the
// render — including one whose fetch was already in flight when the
// interaction started (the commit-time re-check below) — so a background
// refresh can never close a dropdown or eat uncommitted input.
async function renderStatusTab(background = false) {
  if (statusInflight) return;
  statusInflight = true;
  try {
    // An incomplete custom range (tokensRangeQuery → null) never fires a
    // request the server would 400 — the cards keep their previous data
    // while the date-input hint shows.
    const rangeQuery = tokensRangeQuery(tokensRange, Date.now());
    // Each part settles independently ({ok, data?}): a failed part must
    // keep the last successful value in statusCache (never an empty array
    // that renders as "no data") and surface via the .refresh-err banner.
    const tokensFetch = rangeQuery === null
      ? Promise.resolve({ ok: true, data: { usage: statusCache.tok || [], agents: statusCache.agents || [], since: statusCache.since } })
      : apiGet('/api/tokens' + rangeQuery).then((data) => ({ ok: true, data }), () => ({ ok: false }));
    const [stR, tokR, logsR, accR, modelsR] = await Promise.all([
      apiGet('/api/status').then((data) => ({ ok: true, data }), (e) => ({ ok: false, error: e && e.message })),
      tokensFetch,
      apiGet('/api/logs?tail=200').then((data) => ({ ok: true, data }), () => ({ ok: false })),
      apiGet('/api/accounts').then((data) => ({ ok: true, data }), () => ({ ok: false })),
      apiGet('/api/models').then((data) => ({ ok: true, data }), () => ({ ok: false })),
    ]);
    if (!stR.ok && !statusCache.st) {
      // Nothing rendered yet — the full error card is the only honest view.
      throw new Error(stR.error || 'connection lost');
    }
    setConn(stR.ok ? 'ok' : 'err', stR.ok
      ? `v${stR.data.version || '?'} · ${stR.data.uptime || '—'} · ${stR.data.listen || ''}`
      : 'connection lost');
    // Keep the last good value for every failed part — a transient backend
    // error must not blank the cards.
    const prev = statusCache;
    statusCache = {
      st: stR.ok ? stR.data : prev.st,
      tok: tokR.ok ? (tokR.data.usage || []) : prev.tok,
      logs: logsR.ok ? (logsR.data.lines || []) : prev.logs,
      accounts: accR.ok ? (accR.data.providers || []) : prev.accounts,
      agents: tokR.ok ? (tokR.data.agents || []) : prev.agents,
      since: tokR.ok ? (tokR.data.since || 0) : prev.since,
    };
    if (modelsR.ok) modelsCache = modelsR.data;
    if (!stR.ok) {
      // Core snapshot failed with data already on screen: keep the render,
      // report via the banner (no re-render churn needed).
      setRefreshError(panels.status, staleDataText('refresh failed', ['status']));
      return;
    }
    const failed = [
      tokR.ok ? null : 'tokens',
      logsR.ok ? null : 'logs',
      accR.ok ? null : 'accounts',
      modelsR.ok ? null : 'models',
    ].filter(Boolean);
    // Commit-time gate: an interaction that started while this fetch was in
    // flight (e.g. the pin menu opened milliseconds after the tick) defers
    // the DOM write — the hold watcher re-runs a fresh background render as
    // soon as the interaction ends. The cached data above is simply
    // superseded by that re-fetch.
    if (background && deferAutoRefresh(panels.status, () => renderStatusTab(true))) {
      return;
    }
    renderStatusPanel(failed);
  } catch (e) {
    setConn('err', 'connection lost');
    if (!statusCache.st) {
      // First load: nothing to preserve — the full error card is correct.
      if (panels.status) {
        panels.status.innerHTML =
          `<div class="card"><div class="card-body"><div class="msg err">${esc(e.message)}</div></div></div>`;
      }
    } else {
      // Defensive path (unexpected throw mid-render): keep what's on screen.
      setRefreshError(panels.status, staleDataText('refresh failed', ['status']));
    }
  } finally {
    statusInflight = false;
  }
  if (activeTab === 'status' && !statusTimer) {
    statusTimer = setInterval(() => {
      // Tick-time gate: skip while the user interacts inside the panel; the
      // hold watcher refreshes (fresh fetch, not stale cache) shortly after
      // the interaction ends. Explicit renders (mutations, section
      // switches) are direct renderStatusTab() calls and bypass this.
      if (deferAutoRefresh(panels.status, () => renderStatusTab(true))) return;
      renderStatusTab(true);
    }, 5000);
  }
}

// renderStatusPanel draws the Warnings banner (if any) + the sidebar+detail
// layout, then renders the active section. Called after each fetch. The sidebar
// is built only when it isn't already present, so a 5s tick that finds the
// layout in place just refreshes the warnings + re-renders the active section,
// preserving scroll position (e.g. Logs scrolled up) in the pane.
// refreshFailures lists the parts whose fetch failed this round (['tokens',
// 'logs', …]): non-empty shows the shared stale-data banner (the cards keep
// the last successful data); empty/undefined clears it. Callers that
// re-render on user actions pass nothing — the next background tick
// re-reports if the failure persists.
function renderStatusPanel(refreshFailures) {
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
  // Stale-data banner — managed AFTER the layout block above, whose rebuild
  // branch wipes the panel: a failed background refresh reports here while
  // the last successful data stays on screen; a clean refresh clears it.
  setRefreshError(panel, refreshFailures && refreshFailures.length
    ? staleDataText('refresh failed', refreshFailures)
    : null);
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
  // Leaving the Dashboard section tears its uPlot instance down (the cases
  // below wipe .status-main, detaching the chart host); re-entry rebuilds
  // it fresh.
  if (key !== 'dashboard') destroyDashChart();
  // status-logs-active makes the Logs pane fill the viewport height (the log
  // <pre> flex-grows). Only set for the logs section; other sections are short
  // and should size to content.
  main.classList.toggle('status-logs-active', key === 'logs');
  switch (key) {
    case 'dashboard':
      // The dashboard keeps its skeleton between ticks: the chart updates in
      // place (u.setData) and content refreshes at most every ~30s from
      // /api/analytics (see refreshDashboardData).
      renderDashboardSection(main);
      break;
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
  if (push) setHash(statusHash(), true);
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

// ===========================================================================
// STATUS → DASHBOARD (analytics-style live view)
// ===========================================================================
//
// The dashboard reuses the Analytics tab's rendering — KPI chips with
// period-over-period deltas, one metric-switchable per-model trend chart and
// the leaderboard table — pinned to a fixed window: last 1 hour, per-minute
// buckets, by model. Data comes from /api/analytics (which already drops the
// guard/attempts/routing/fusion virtual counter namespaces server-side), so
// every series is a real upstream model. The former Model Health section is
// merged into the leaderboard: each row carries its health grade
// (latency/ttft/tok-s scoring, pure.js modelHealthFromSeries) and the graded
// value cells keep their dimension's ok/warn/err color. Refreshes ride the
// status tab's 5s tick but fetch at most every ~30s (a minute-granularity
// window changes slowly); the chart updates in place (u.setData) so a
// refresh neither flickers nor resets hover.

// DASH_WINDOW_SEC is the dashboard's fixed trailing window: the last hour.
const DASH_WINDOW_SEC = 3600;

// DASH_REFRESH_MS is the minimum age of the cached /api/analytics response
// before the 5s status tick refetches it.
const DASH_REFRESH_MS = 30000;

// Dashboard state: the active chart metric (metric-switcher selection,
// session scope), the last response + its fetch time, the inflight flag, the
// live uPlot instance, and the per-session set of legend-toggled series
// labels (persists across in-place chart updates).
let dashMetric = 'tokens';
let dashData = null;
let dashFetchedAt = 0;
let dashInflight = false;
let dashChart = null; // {u, sig, width, labels}
const dashLegendHidden = new Set();

// destroyDashChart tears the dashboard chart down (the section switch wipes
// its hosts) and detaches the legend dropdown's document-level listener.
function destroyDashChart() {
  if (anLegendOutside) {
    document.removeEventListener('click', anLegendOutside);
    anLegendOutside = null;
  }
  if (dashChart) {
    try { dashChart.u.destroy(); } catch (_) { /* already detached */ }
    dashChart = null;
  }
}

// renderDashboardSection builds the skeleton once (window header + KPI row +
// metric-switchable chart + leaderboard), then keeps it fed: every 5s status
// tick lands here, but refreshDashboardData only refetches when the cached
// window is stale. Section re-entry (the pane was wiped by another section)
// rebuilds the skeleton and repaints from the cache instantly.
function renderDashboardSection(main) {
  if (!main.querySelector('.dash-wrap')) {
    destroyDashChart();
    main.innerHTML = `<div class="dash-wrap">
      <header class="dash-head">
        <h2>Last hour</h2>
        <span class="meta">trailing 60-minute window · per-minute buckets by model · deltas vs the hour before</span>
      </header>
      <div id="dash-error" class="msg err" hidden></div>
      <div id="dash-kpis" class="an-kpis"></div>
      <div class="an-chart-card">
        <div class="an-chart-head">
          <div class="an-seg" id="dash-metric" role="group" aria-label="Metric"></div>
        </div>
        <div class="an-chart-wrap"><div id="dash-chart" class="an-chart"></div></div>
        <div id="dash-legend"></div>
      </div>
      <div id="dash-table" class="an-table-card"></div>
    </div>`;
    // First paint from the cache (if any) so section re-entry is instant;
    // refreshDashboardData below brings in fresh numbers. The metric
    // switcher itself is (re)rendered by renderDashboardContent — a switch
    // must update its own active highlight.
    renderDashboardContent();
  }
  refreshDashboardData();
}

// refreshDashboardData fetches the fixed 1h/minute/model window into dashData
// and repaints the section's content. Cheap guards: one fetch at a time and
// no refetch while the cached response is younger than DASH_REFRESH_MS. The
// interaction protection rides the auto-refresh gate at both checkpoints:
// a fetch is not started while any popup is open in the section (the
// legend's "+N more" dropdown carries data-popup), and a fetch that was
// already in flight when the interaction began defers its repaint until the
// hold watcher reports the section idle — an in-flight landing must not
// close a dropdown the user just opened.
async function refreshDashboardData() {
  const main = document.querySelector('.status-main');
  if (!main || !main.querySelector('.dash-wrap')) return;
  if (dashInflight) return;
  if (dashData && Date.now() - dashFetchedAt < DASH_REFRESH_MS) return;
  if (main.querySelector(POPUP_OPEN_SEL)) return;
  dashInflight = true;
  const errEl = main.querySelector('#dash-error');
  try {
    const to = Math.floor(Date.now() / 1000);
    const resp = await apiGet('/api/analytics?from=' + (to - DASH_WINDOW_SEC) + '&to=' + to + '&granularity=minute&by=model');
    dashData = resp;
    dashFetchedAt = Date.now();
    if (errEl) errEl.hidden = true;
    // Commit-time gate (root = .status-main, deliberately separate from the
    // status tab's panels.status watcher so the two pending refreshes never
    // overwrite each other's fire callbacks). dashData is already fresh —
    // the deferred fire paints from the cache without a refetch.
    if (deferAutoRefresh(main, () => renderDashboardContent())) return;
    renderDashboardContent();
  } catch (e) {
    if (errEl) {
      errEl.hidden = false;
      // A failed refresh keeps the last successful data on screen — the banner
    // (not a wipe) is how the failure surfaces.
    errEl.textContent = 'Dashboard unavailable: ' + e.message + (dashData ? ' — showing last successful data' : '');
    }
  } finally {
    dashInflight = false;
  }
}

// renderDashboardContent paints KPIs, chart and leaderboard from dashData.
// Before the first response lands it shows a neutral loading state instead of
// misleading zeroed KPIs.
function renderDashboardContent() {
  const main = document.querySelector('.status-main');
  if (!main || !main.querySelector('.dash-wrap')) return;
  if (!dashData) {
    main.querySelector('#dash-kpis').innerHTML = '';
    main.querySelector('#dash-chart').innerHTML = '<div class="empty-state">loading…</div>';
    main.querySelector('#dash-legend').innerHTML = '';
    main.querySelector('#dash-table').innerHTML = '';
    return;
  }
  analyticsRenderKpis(main.querySelector('#dash-kpis'), dashData);
  // Metric switch: repaint chart + leaderboard from the cached response —
  // the window itself never changes, so no refetch is needed. The seg is
  // redrawn here so its active highlight follows the selection.
  analyticsSeg(main.querySelector('#dash-metric'), ANALYTICS_METRICS.map((m) => ({ value: m.id, label: m.label })),
    dashMetric, (v) => {
      dashMetric = v;
      renderDashboardContent();
    });
  dashRenderChart(main.querySelector('#dash-chart'), main.querySelector('#dash-legend'));
  dashRenderTable(main.querySelector('#dash-table'));
}

// uplotAxisStyle reads the theme colors for uPlot's canvas-drawn axis text,
// ticks and grid. Canvas pixels don't inherit CSS, so without this the
// default dark strokes are unreadable in dark mode. Colors still originate
// from the :root variables (single source); this only forwards them.
function uplotAxisStyle() {
  const cs = getComputedStyle(document.documentElement);
  const text = cs.getPropertyValue('--muted').trim() || '#656d76';
  const line = cs.getPropertyValue('--border').trim() || '#d0d7de';
  return { stroke: text, grid: { stroke: line, width: 1 }, ticks: { stroke: line, width: 1 } };
}

// dashRenderChart draws the dashboard's metric trend chart with uPlot — the
// same translucent per-bucket columns, tooltip and legend chips as the
// Analytics tab (shared helpers), but with the x window pinned to the
// trailing hour and no drag-zoom: this is an at-a-glance live view, the
// Analytics tab remains the deep-dive tool. The y scale keeps the 0 baseline
// (all metrics are non-negative; auto-zoom would magnify noise). Updates the
// existing instance in place (u.setData + sliding x window) when the series
// set is unchanged so a refresh neither flickers nor resets hover.
function dashRenderChart(host, legendHost) {
  if (!host || !legendHost) return;
  if (typeof uPlot === 'undefined') { // vendored script failed to load
    host.innerHTML = '<div class="empty-state">Charts unavailable</div>';
    return;
  }
  const resp = dashData;
  // The window grid comes from the response's echoed from/to (not the local
  // clock) so the axis covers exactly what was queried.
  const grid = analyticsWindowGrid(resp && resp.from, resp && resp.to, 'minute');
  const data = analyticsChartSeries(resp && resp.series, dashMetric, grid);
  if (!data.x.length || !data.labels.length) {
    destroyDashChart();
    host.innerHTML = '<div class="empty-state">No traffic in the last hour.</div>';
    legendHost.innerHTML = '';
    return;
  }
  const colors = analyticsChartColors();
  const width = Math.max(host.clientWidth || 600, 320);
  // The signature covers the metric too: a metric switch must rebuild the
  // chart (new y axis label + tooltip formatter), not just swap the data.
  const sig = dashMetric + '|' + data.labels.join('|');
  if (dashChart && dashChart.sig === sig) {
    // In-place update: same models on screen, new window slice. setData
    // re-derives the x window from the live data (the scale's range fn
    // below), so the axis slides with the fetch window.
    dashChart.u.setData([data.x, ...data.ys]);
    if (Math.abs(dashChart.width - width) > 1) {
      dashChart.u.setSize({ width, height: 260 });
      dashChart.width = width;
    }
    return;
  }
  destroyDashChart();
  host.innerHTML = '';
  const metric = ANALYTICS_METRICS.find((m) => m.id === dashMetric) || ANALYTICS_METRICS[0];
  const uSeries = [{ label: 'time' }];
  data.labels.forEach((label, i) => {
    const stroke = colors[i % colors.length];
    uSeries.push({ label, stroke, width: 1, fill: withAlpha(stroke, 0.55), paths: analyticsBarPaths(), points: { show: false } });
  });
  const axis = uplotAxisStyle();
  try {
    const u = new uPlot({
      title: metric.label + ' (' + metric.axis + ')',
      width,
      height: 260,
      series: uSeries,
      scales: {
        // The x window is derived from the live data on every (re)autscale,
        // so it slides with each fetch's trailing-hour slice; the padded
        // range keeps edge columns unclipped (see analyticsXRange). y keeps
        // the 0 baseline (all metrics are non-negative).
        x: { time: true, range: (u, min, max) => (u.data[0] && u.data[0].length ? analyticsXRange(u.data[0]) : [min, max]) },
        y: { range: (_u, min, max) => [0, Math.max(max, min || 0, 1)] },
      },
      plugins: [analyticsTooltip(dashMetric, 'minute')],
      axes: [
        { ...axis, values: analyticsXAxisValues },
        { label: metric.axis, size: 60, ...axis, values: (_u, splits) => splits.map((v) => (v == null ? '' : fmtCompact(v))) },
      ],
      legend: { show: false },
    }, [data.x, ...data.ys], host);
    dashChart = { u, sig, width, labels: data.labels };
    analyticsRenderLegend(legendHost, u, data.labels, colors, dashLegendHidden);
  } catch (_) { /* malformed data */ }
}

// dashRenderTable renders the dashboard leaderboard: the Analytics table's
// window rows plus the merged Model Health view — a health status badge per
// model (ok/warn/err from modelHealthFromSeries) and the graded value cells
// (avg lat / ttft / tok/s carry their dimension's grade color, so a slow
// dimension is visible at a glance next to the overall badge). Rows sort by
// the active chart metric, same as the Analytics leaderboard.
function dashRenderTable(host) {
  if (!host) return;
  const resp = dashData;
  const rows = analyticsTableRows(resp && resp.series);
  const health = new Map(modelHealthFromSeries(resp && resp.series).map((r) => [r.label, r]));
  const totalCost = rows.reduce((sum, r) => sum + (r.cost || 0), 0);
  rows.sort((a, b) => analyticsRowSortKey(b, dashMetric) - analyticsRowSortKey(a, dashMetric));
  const gradeBadge = (g) => g == null ? '<span class="badge muted">n/a</span>'
    : `<span class="badge ${g}">${g === 'ok' ? 'healthy' : g === 'warn' ? 'degraded' : 'poor'}</span>`;
  const graded = (dim, text) => `<td class="num${dim && dim.grade ? ' ' + dim.grade : ''}">${text}</td>`;
  const body = rows.map((r) => {
    const h = health.get(r.label) || {};
    const dims = h.dims || {};
    const share = r.cost != null && totalCost > 0 ? r.cost / totalCost * 100 : null;
    // v2: $/1M tok lives on the cost cell as a hover note (the dashboard pane
    // is narrower than the Analytics tab — the extra column crowded it), and
    // cost share is a plain number (no inline bar).
    const costTip = r.costPerMTok == null ? '' : ` title="$${r.costPerMTok.toFixed(2)} per 1M tokens"`;
    const shareTip = share == null ? '' : ` title="${share.toFixed(1)}% of priced cost"`;
    return `<tr>
      <td class="mono">${esc(r.label)}</td>
      <td>${gradeBadge(h.grade)}</td>
      <td class="num">${fmtNum(r.requests)}</td>
      <td class="num" title="${esc(fmtNum(r.tokens))} tokens">${fmtCompact(r.tokens)}</td>
      <td class="num">${r.errPct == null ? '—' : r.errPct.toFixed(1) + '%'}</td>
      ${graded(dims.latency, r.latencyMs == null ? '—' : fmtNum(Math.round(r.latencyMs)) + 'ms')}
      ${graded(dims.ttft, r.ttftMs == null ? '—' : fmtNum(Math.round(r.ttftMs)) + 'ms')}
      ${graded(dims.toksec, r.tokSec == null ? '—' : r.tokSec.toFixed(1))}
      <td class="num"${costTip}>${r.cost == null ? 'n/a' : '$' + r.cost.toFixed(4)}</td>
      <td class="num"${shareTip}>${share == null ? '—' : share.toFixed(1) + '%'}</td>
    </tr>`;
  }).join('');
  host.innerHTML = `<table class="table">
      <thead><tr><th>Series</th><th>Status</th><th class="num">Requests</th><th class="num">Tokens</th><th class="num">Err</th><th class="num">Avg Lat</th><th class="num">TTFT</th><th class="num">Tok/s</th><th class="num">Cost</th><th class="num">Cost Share</th></tr></thead>
      <tbody>${body || '<tr><td colspan="10" class="hint">No series in range</td></tr>'}</tbody>
    </table>`;
}

// buildCard wraps a title + body in the .card/.card-head/.card-body shell.
// headActionsHTML, when given, pins extra controls (buttons) to the right end
// of the header, grouped with the meta text.
function buildCard(title, meta, bodyHTML, extraBodyClass = '', headActionsHTML = '', extraCardClass = '') {
  // v2: the meta count ("16 routes", "200 lines", …) rides INLINE right
  // after the title instead of floating to the card's far-right edge — with
  // space-between and no actions the lone meta read as a detached far-right
  // fragment. Head actions (Refresh buttons) still pin to the right.
  // extraCardClass decorates the <section class="card"> itself (e.g.
  // card-open drops the overflow clipping that would kill inner sticky).
  const metaHTML = meta ? `<span class="meta">${esc(meta)}</span>` : '';
  const headRight = headActionsHTML
    ? `<span class="card-head-side">${headActionsHTML}</span>`
    : '';
  return `<section class="card${extraCardClass ? ' ' + extraCardClass : ''}">
    <header class="card-head"><span class="card-head-title"><h2>${esc(title)}</h2>${metaHTML}</span>${headRight}</header>
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
    return `<span class="pill warn" title="Manually frozen by operator — excluded from scheduling until unfreeze">Frozen</span>`;
  }
  if (h.circuit_state === 'open' || h.circuit_state === 'half_open') {
    const tail = h.circuit_state === 'half_open' ? ' (probing)' : untilHuman(h.circuit_until, now);
    return `<span class="pill err" title="Circuit ${esc(h.circuit_state)}">Circuit ${esc(h.circuit_state)}${esc(tail)}</span>`;
  }
  if (rlUntil && rlUntil > now) {
    const kind = h.rate_limit_kind && h.rate_limit_kind !== 'transient' ? ` (${h.rate_limit_kind})` : '';
    return `<span class="pill warn" title="Rate-limited (${esc(h.rate_limit_kind || 'transient')}) until ${esc(h.rate_limited_until)}">Rate-limited${esc(kind)}${esc(untilHuman(h.rate_limited_until, now))}</span>`;
  }
  if (h.available) {
    return `<span class="pill ok">Available</span>`;
  }
  return `<span class="pill muted">Unavailable</span>`;
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
  if (!snap) return `<span class="pill muted">No data</span>`;
  if (snap.Err) {
    const k = quotaErrKind(snap);
    const lbl = k === 'session-expired' ? 'Session expired'
      : k === 'not-logged-in' ? 'Not logged in' : 'Error';
    return `<span class="pill err">${esc(lbl)}</span>`;
  }
  const ult = (snap.Windows || []).find((w) => w.Ultimate);
  if (ult && ult.RemainingPct != null && ult.RemainingPct >= 0) {
    const p = ult.RemainingPct;
    const cls = p > 0.3 ? 'ok' : (p > 0.1 ? 'warn' : 'err');
    return `<span class="pill ${cls}">${(p * 100).toFixed(1)}% left</span>`;
  }
  if (snap.Plan) return `<span class="pill muted">${esc(snap.Plan)}</span>`;
  return `<span class="pill ok">Available</span>`;
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
      ? ` <button class="btn small danger-solid" data-unfreeze="${esc(name)}" title="Clear circuit/rate-limit cooldowns + model locks">Unfreeze</button>`
      : ` <button class="btn small" data-freeze="${esc(name)}" title="Exclude from scheduling until unfreeze">Freeze</button>`;
    rows += `<tr>
      <td class="mono">${esc(name)}${action}</td>
      <td>${healthPill(health[name])}</td>
      <td class="num">${fmtNum(c.requests)}</td>
      <td class="num">${fmtNum(c.failovers)}</td>
      <td class="num${c.rate_limited_429 > 0 ? ' err' : ''}">${fmtNum(c.rate_limited_429)}</td>
      <td class="num${c.failures > 0 ? ' err' : ''}">${fmtNum(c.failures)}</td>
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
  if (btn) { btn.disabled = true; btn.textContent = 'Unfreezing…'; }
  try {
    await apiPost('/api/health/reset', { provider: name });
    await renderStatusTab();
  } catch (e) {
    if (btn) { btn.disabled = false; btn.textContent = 'Unfreeze'; }
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
  if (btn) { btn.disabled = true; btn.textContent = 'Freezing…'; }
  try {
    await apiPost('/api/health/freeze', { provider: name });
    await renderStatusTab();
  } catch (e) {
    if (btn) { btn.disabled = false; btn.textContent = 'Freeze'; }
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
      chain += `<span class="${classes.join(' ')}" title="${esc(title)}">${pinned ? iconPin() : ''}${esc(p.provider)}${esc(parent)}${tier}</span>`;
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
        `<button class="btn small" data-pin-toggle="${esc(route)}" title="pin ${esc(route)} to one provider (no failover)">${iconPin()}pin</button>` +
        `<div class="route-pin-menu" data-popup data-pin-menu="${esc(route)}" hidden>${items}</div>` +
        `</span>`;
    }
    if (!chain) chain = `<span class="route-meta">No providers available</span>`;
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
        pools += `Pool ${esc(pl.parent)}: ${pl.available}/${pl.accounts} available · `;
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
      <td class="num">${fmtNum(u.requests)}</td>
      <td class="num">${fmtNum(u.input)}</td>
      <td class="num">${fmtNum(u.output)}</td>
      <td class="num">${fmtNum(u.cache_creation)}</td>
      <td class="num">${fmtNum(u.cache_read)}</td>
      <td class="num">${fmtNum(u.total)}</td>
    </tr>`;
  }
  const html = buildCard('Token usage', `${totalReqs} requests · ${tokensRangeMeta()}`, `
      <table class="table">
        <thead><tr>
          <th>Provider</th><th>Model</th><th class="num">Requests</th>
          <th class="num">Input</th><th class="num">Output</th>
          <th class="num">Cache Create</th><th class="num">Cache Read</th>
          <th class="num">Total</th>
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
        <div class="tr-popover" data-popup ${picker.open ? '' : 'hidden'}>
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
          <th>Agent / Model</th><th class="num">Requests</th>
          <th class="num">Input</th><th class="num">Output</th>
          <th class="num">Cache Create</th><th class="num">Cache Read</th>
          <th class="num">Total</th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table>`, 'flush');
  target.insertAdjacentHTML('beforeend', html);
}

// confirmDialog shows the themed #confirm-modal in place of window.confirm.
// Resolves true only when the confirm button is clicked; Esc, Close and
// Cancel all resolve false.
// confirmDialog renders the shared themed confirm modal. opts.challenge adds
// a fool-proof step for destructive actions: a random code is shown and must
// be typed back exactly. The confirm button is always clickable — a wrong
// code is reported on the confirm attempt (inline error + red input), never
// mid-typing; typing again clears the previous attempt's error. Enter routes
// through the same confirm path. All other call sites keep the plain
// two-button form.
function confirmDialog(title, message, confirmLabel, opts = {}) {
  const modal = document.getElementById('confirm-modal');
  if (!modal) return Promise.resolve(false);
  const challenge = !!(opts && opts.challenge);
  const code = challenge ? String(1000 + Math.floor(Math.random() * 9000)) : '';
  return new Promise((resolve) => {
    const challengeHTML = challenge ? `
          <div class="field" style="margin-top:12px">
            <label for="confirm-challenge">Type <span class="mono" style="font-weight:600">${esc(code)}</span> to confirm</label>
            <input id="confirm-challenge" autocomplete="off" spellcheck="false" inputmode="numeric" aria-describedby="confirm-challenge-hint"
                   placeholder="${esc(code)}" style="font-family:var(--mono)">
            <span class="hint" id="confirm-challenge-hint">The code proves the reset is intentional.</span>
            <div class="msg err" id="confirm-challenge-err" hidden>wrong code — type the 4 digits shown above</div>
          </div>` : '';
    modal.innerHTML =
      `<form method="dialog">
        <header class="modal-head">
          <h2 id="confirm-title">${esc(title)}</h2>
          <button type="button" class="link-btn" id="confirm-cancel" aria-label="Close">Close</button>
        </header>
        <div class="modal-body">
          <p>${esc(message)}</p>${challengeHTML}
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
    // Challenge dialogs verify here, on the confirm attempt — the single
    // place a wrong code is reported.
    document.getElementById('confirm-yes').addEventListener('click', () => {
      if (!challenge) { done(true); return; }
      const input = document.getElementById('confirm-challenge');
      const err = document.getElementById('confirm-challenge-err');
      if (input.value.trim() === code) { done(true); return; }
      input.setAttribute('aria-invalid', 'true');
      if (err) err.hidden = false;
      input.select();
    });
    modal.showModal();
    if (challenge) {
      const input = document.getElementById('confirm-challenge');
      const err = document.getElementById('confirm-challenge-err');
      // Typing gives no verdict; it only clears the previous attempt's
      // error so the next confirm attempt reports afresh.
      input.addEventListener('input', () => {
        input.setAttribute('aria-invalid', 'false');
        if (err) err.hidden = true;
      });
      input.addEventListener('keydown', (e) => {
        if (e.key !== 'Enter') return;
        // Enter always preventDefault: with method=dialog a native submit
        // would close the modal without resolving (Cancel never fires on a
        // form submit), leaving the await hung. Enter = confirm attempt.
        e.preventDefault();
        document.getElementById('confirm-yes').click();
      });
      input.focus();
    }
  });
}

async function resetTokens() {
  // v2: the reset is irreversible and zeroes the live counters, so the
  // confirm modal requires typing the displayed challenge code first.
  const ok = await confirmDialog('Reset token usage counters',
    'This zeroes the per-(provider, model) token usage stats. The action cannot be undone.',
    'Reset counters', { challenge: true });
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
    // v2: logLineHTML (pure.js) tokenizes each raw line — timestamp prefix,
    // severity token and key=value pairs get semantic coloring; every value
    // is escaped inside the tokenizer. The colored spans must sit inside ONE
    // wrapper: .log-line is a grid (gutter | content), so loose spans would
    // each become their own grid item and scatter across both columns.
    const rendered = lines.map((l) => `<span class="log-line"><span class="log-msg">${logLineHTML(l)}</span></span>`).join('');
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
// logsOpenDetails remembers which over-long log lines the user expanded
// (keyed by the line's <summary> prefix text) so a re-render that appends
// new lines re-opens exactly those <details> instead of collapsing them.
let logsOpenDetails = new Set();

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
  // Snapshot expanded long-line <details> before the rebuild and re-open
  // the survivors after (lines that scrolled out of the 200-line tail drop
  // out of the set naturally).
  const openNow = new Set();
  target.querySelectorAll('details.json-long[open]').forEach((d) => {
    const s = d.querySelector('summary');
    if (s) openNow.add(s.textContent);
  });
  logsOpenDetails = openNow;
  target.innerHTML = '';
  renderLogsCard(target, arr);
  const found = new Set();
  target.querySelectorAll('details.json-long').forEach((d) => {
    const s = d.querySelector('summary');
    if (s && logsOpenDetails.has(s.textContent)) {
      d.open = true;
      found.add(s.textContent);
    }
  });
  logsOpenDetails = found;
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
  // Re-entry keeps the mounted tab in place (stale-while-revalidate, same
  // policy as the Status cache): the skeleton below is a first-activation
  // mount, later switches keep the rendered forms + YAML editor visible and
  // loadConfigAll refreshes them in place — no loading… flash between
  // switches. The Summary card head only exists after a successful fill, so
  // a failed first activation re-mounts the skeleton and retries.
  if (await retainTab(panel, '#config-summary .card-head', loadConfigData)) return;
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
     <details class="editor" id="ed-provider"><summary>${iconChevron()}Provider scalars</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-route"><summary>${iconChevron()}Routes</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-guard-rules"><summary>${iconChevron()}Guard rules (extra patterns / paths)</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-settings"><summary>${iconChevron()}Settings (log / scheduling / request log / stats / cache / guard)</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
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

  await loadConfigData();
}

// loadConfigData fetches the config snapshot into the mounted tab; a failure
// surfaces in the Summary card while the rest of the tab keeps its last
// rendered state.
async function loadConfigData() {
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

  // Guard rule lists (extra_patterns / extra_paths) form
  buildGuardRulesForm('ed-guard-rules');

  // Scalar settings form (log level, scheduling, request_log, stats, cache, guard)
  buildSettingsForm();
  scheduleYamlEditorResize();
}

// buildGuardRulesForm renders the guard rule-list editor: extra_patterns
// rows (name / regex / optional literal pre-filter) and extra_paths rows
// (literal strings). It edits the same /api/config/edit kind the scalar
// Guard settings group uses; the lists are always sent wholesale (present
// replaces, empty lists send null to delete the key). Validation is
// backend-owned (name charset, literal-must-substring); the backend message
// surfaces verbatim.
function buildGuardRulesForm(editorId) {
  const ed = document.getElementById(editorId);
  if (!ed) return;
  const body = ed.querySelector('.editor-body');
  if (!body) return;
  const g = ((configCache && configCache.settings) || {}).guard || {};
  const patternsEmpty = '<tr><td colspan="4" class="subdue">no custom patterns</td></tr>';
  const pathsEmpty = '<tr><td colspan="2" class="subdue">no custom paths</td></tr>';
  const patterns = (g.extra_patterns || []).map((p) => `<tr>
      <td><input class="req-input gr-name" value="${esc(p.name || '')}" placeholder="myvendor_key" aria-label="pattern name"></td>
      <td><input class="req-input gr-regex" value="${esc(p.regex || '')}" placeholder="\\bmv-[A-Za-z0-9]{32,}" aria-label="pattern regex"></td>
      <td><input class="req-input gr-literal" value="${esc(p.literal || '')}" placeholder="mv- (optional)" aria-label="pattern literal"></td>
      <td><button class="btn small danger gr-del" title="Remove this pattern">✕</button></td>
    </tr>`).join('');
  const paths = (g.extra_paths || []).map((p) => `<tr>
      <td><input class="req-input gp-lit" value="${esc(p)}" placeholder="~/.company/secrets" aria-label="sensitive path"></td>
      <td><button class="btn small danger gp-del" title="Remove this path">✕</button></td>
    </tr>`).join('');
  body.innerHTML = `
    <div class="section-title">extra_patterns <span class="hint">(custom secret rules; name ^[a-z0-9_]{1,32}$, literal must be a guaranteed substring of every regex match)</span></div>
    <table class="table guard-rules-table"><thead><tr><th>name</th><th>regex</th><th>literal</th><th></th></tr></thead>
      <tbody id="gr-rows">${patterns || patternsEmpty}</tbody></table>
    <div class="row-actions"><button class="btn small" id="gr-add">+ pattern</button></div>
    <div class="section-title" style="margin-top:14px">extra_paths <span class="hint">(literal sensitive-path strings matched against the request body; "~" is matched literally)</span></div>
    <table class="table guard-rules-table"><thead><tr><th>path</th><th></th></tr></thead>
      <tbody id="gp-rows">${paths || pathsEmpty}</tbody></table>
    <div class="row-actions"><button class="btn small" id="gp-add">+ path</button></div>
    <div class="row-actions" style="margin-top:14px">
      <span class="spacer"></span>
      <button class="btn primary small" id="gr-save">Save guard rules</button>
    </div>
    <div class="msg" id="gr-msg"></div>`;
  const wireRemove = (btn, emptyHtml) => {
    btn.addEventListener('click', () => {
      const row = btn.closest('tr');
      const rows = btn.closest('tbody');
      row.remove();
      if (rows && !rows.querySelector('input')) rows.innerHTML = emptyHtml;
    });
  };
  body.querySelectorAll('.gr-del').forEach((b) => wireRemove(b, patternsEmpty));
  body.querySelectorAll('.gp-del').forEach((b) => wireRemove(b, pathsEmpty));
  document.getElementById('gr-add').addEventListener('click', () => {
    const rows = document.getElementById('gr-rows');
    const empty = rows.querySelector('.subdue');
    if (empty) empty.remove();
    rows.insertAdjacentHTML('beforeend', `<tr>
      <td><input class="req-input gr-name" placeholder="myvendor_key"></td>
      <td><input class="req-input gr-regex" placeholder="\\bmv-[A-Za-z0-9]{32,}"></td>
      <td><input class="req-input gr-literal" placeholder="mv- (optional)"></td>
      <td><button class="btn small danger gr-del" title="Remove this pattern">✕</button></td>
    </tr>`);
    wireRemove(rows.lastElementChild.querySelector('.gr-del'), patternsEmpty);
    rows.lastElementChild.querySelector('.gr-name').focus();
  });
  document.getElementById('gp-add').addEventListener('click', () => {
    const rows = document.getElementById('gp-rows');
    const empty = rows.querySelector('.subdue');
    if (empty) empty.remove();
    rows.insertAdjacentHTML('beforeend', `<tr>
      <td><input class="req-input gp-lit" placeholder="~/.company/secrets"></td>
      <td><button class="btn small danger gp-del" title="Remove this path">✕</button></td>
    </tr>`);
    wireRemove(rows.lastElementChild.querySelector('.gp-del'), pathsEmpty);
    rows.lastElementChild.querySelector('.gp-lit').focus();
  });
  document.getElementById('gr-save').addEventListener('click', saveGuardRules);
}

// saveGuardRules collects the edited lists and posts them as one guard edit.
// Empty lists send null (delete the key → revert to none), matching the
// scalar form's revert-to-default signal; rows with a blank name AND regex
// are skipped as artifacts of the add-row UI.
async function saveGuardRules() {
  const msg = document.getElementById('gr-msg');
  const btn = document.getElementById('gr-save');
  const read = (sel) => Array.from(document.querySelectorAll(sel)).map((i) => i.value);
  const names = read('#gr-rows .gr-name');
  const regexes = read('#gr-rows .gr-regex');
  const literals = read('#gr-rows .gr-literal');
  const patterns = [];
  for (let i = 0; i < names.length; i++) {
    const name = (names[i] || '').trim();
    const regex = (regexes[i] || '').trim();
    if (!name && !regex) continue;
    patterns.push({ name, regex, literal: (literals[i] || '').trim() });
  }
  const paths = read('#gp-rows .gp-lit').map((v) => v.trim()).filter(Boolean);
  btn.disabled = true;
  showMsg(msg, 'ok', 'saving…');
  try {
    await apiPost('/api/config/edit', {
      kind: 'guard',
      data: {
        extra_patterns: patterns.length ? patterns : null,
        extra_paths: paths.length ? paths : null,
      },
    });
    await loadConfigAll();
    showMsg(msg, 'ok', 'saved & reloaded');
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    btn.disabled = false;
  }
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
  {
    kind: 'guard', title: 'Guard',
    note: 'Rule lists live in the Guard rules editor above; guard.adjudicate stays YAML-only (explicit opt-in to send matched snippets to a model).',
    fields: [
      {
        key: 'secrets', label: 'secrets', type: 'select', options: ['log', 'redact', 'block', 'off'], def: 'log',
        help: 'Action on secret-channel hits: log (counter + event + audit), redact (rewrite the body, then log), block (reject with 400), off.',
      },
      {
        key: 'paths', label: 'paths', type: 'select', options: ['log', 'block', 'off'], def: 'log',
        help: 'Action on STRONG sensitive-path hits (tool-invocation side). Weak mentions are always ignored.',
      },
      {
        key: 'known_secrets', label: 'known_secrets', type: 'checkbox', def: 'true',
        help: 'Also match the proxy\'s own managed credentials (pool keys, OAuth tokens) exactly.',
      },
      {
        key: 'decode', label: 'decode', type: 'checkbox', def: 'true',
        help: 'Catch base64/hex/url-encoded forms of known secrets.',
      },
      {
        key: 'audit', label: 'audit', type: 'checkbox', def: 'true',
        help: 'Persist security events to the audit log (guard.audit_path, 30d retention).',
      },
      {
        key: 'session_scan', label: 'session_scan', type: 'checkbox', def: 'true',
        help: 'Detect a known credential split into fragments across one session\'s requests (reported as known_secret_fragmented).',
      },
      {
        key: 'audit_path', label: 'audit_path', type: 'text', def: '~/.model-proxy/log/security/security.log',
        help: 'Optional absolute path for the security audit log. Empty = the default location.',
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
// Accounts auto-refresh: a 30s background tick while the tab is the ACTIVE
// view, through the interaction gate (Add/login modals are [data-popup],
// open selects hold focus — both defer). Mid-operation guard: probes,
// quota polls and the Test All run disable their buttons and render
// progress into the pane — a background re-render would wipe that, so the
// tick skips while any pane control is disabled. Background failures keep
// the rendered view and report through the stale banner; only the first
// load (no nav yet) falls back to the inline error.
let accountsRefreshTimer = null;

function accountsStopAutoRefresh() {
  if (accountsRefreshTimer) {
    clearInterval(accountsRefreshTimer);
    accountsRefreshTimer = null;
  }
  cancelAutoRefreshHold(panels.accounts);
}

function accountsMaybeAutoRefresh() {
  accountsStopAutoRefresh();
  accountsRefreshTimer = setInterval(() => {
    const panel = panels.accounts;
    if (!panel || !panel.classList.contains('active')) return;
    if (panel.querySelector('.acct-main button:disabled, .acct-nav button:disabled')) return;
    if (deferAutoRefresh(panel, () => loadAccountsData(true))) return;
    loadAccountsData(true);
  }, 30000);
}

async function renderAccountsTab() {
  const panel = panels.accounts;
  // Re-entry keeps the rendered nav/detail on screen and refreshes in place
  // (the nav title the marker checks only exists after a successful fill, so
  // a failed first activation re-mounts the skeleton and retries).
  if (await retainTab(panel, '.acct-nav-title', () => { loadAccountsData(); accountsMaybeAutoRefresh(); })) {
    accountsMaybeAutoRefresh();
    return;
  }
  accountsStopAutoRefresh();
  panel.innerHTML = `<div class="acct-tab-head"><div id="acc-msg"></div></div>
    <div class="accounts-layout">
    <nav class="acct-nav" aria-label="Providers"><span class="msg">loading…</span></nav>
    <div class="acct-main"></div>
  </div>`;
  await loadAccountsData();
  accountsMaybeAutoRefresh();
}

// loadAccountsData fetches the accounts view state and re-renders the nav +
// the selected provider's detail. On tab re-entry a failed fetch keeps the
// old data and reports via #acc-msg. The nav title the re-entry guard checks
// only exists after a successful fill, so a failed first activation
// re-mounts the skeleton and retries.
async function loadAccountsData(background = false) {
  const panel = panels.accounts;
  try {
    const [acc, st, tok] = await Promise.all([
      apiGet('/api/accounts'),
      apiGet('/api/status').catch(() => null),
      apiGet('/api/tokens').catch(() => ({ usage: [] })),
    ]);
    // Commit-time gate: an interaction that started while the fetches were
    // in flight defers the landing render (the hold watcher re-runs a fresh
    // background load once the user is done).
    if (background && deferAutoRefresh(panel, () => loadAccountsData(true))) return;
    accountsCache = acc;
    accountsQuota = (st && st.quota) || {};
    accountsTokens = (tok && tok.usage) || [];
    renderAccountsNav(acc.providers || []);
    if (background) setRefreshError(panel, null);
  } catch (e) {
    setConn('err');
    if (background && panel && panel.querySelector('.acct-nav-title')) {
      // Keep the rendered view; the stale banner carries the failure.
      setRefreshError(panel, staleDataText('refresh failed', ['accounts']));
      return;
    }
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
  btn.textContent = 'Testing…';
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

// testAllAccounts probes every account of the provider sequentially (the
// same POST /api/accounts/<p>/<id>/test the per-card Test button uses) and
// renders the results as a matrix: one row per account plus the provider's
// tri-protocol capability counts (GET /api/models — the same probe data the
// Status Models card renders), so liveness and protocol capability read side
// by side. Sequential on purpose: each probe is a REAL upstream request;
// firing the whole pool at once would race upstream rate limits. Results
// paint incrementally so a slow account does not hide the earlier verdicts.
async function testAllAccounts(btn, p) {
  const accounts = p.accounts || [];
  const host = document.getElementById('acct-test-matrix');
  if (!host || !accounts.length) return;
  // Capability header is best-effort: no probe data → the matrix still
  // renders, just without the caps line.
  let caps = null;
  try {
    const md = await apiGet('/api/models');
    const entry = modelCapMatrix((md && md.providers) || {}).find((e) => e.name === p.name);
    if (entry) caps = providerCapsSummary(entry);
  } catch { /* no probe data — liveness matrix alone */ }
  const orig = btn.textContent;
  btn.disabled = true;
  const results = [];
  try {
    for (let i = 0; i < accounts.length; i++) {
      const a = accounts[i];
      btn.textContent = `testing ${i + 1}/${accounts.length}…`;
      let r;
      try {
        r = await apiPost(`/api/accounts/${encodeURIComponent(p.name)}/${encodeURIComponent(a.id)}/test`);
      } catch (e) {
        r = { status: 'err', http_status: 0, reason: e.message };
      }
      results.push({ label: a.label || a.id, ...r });
      renderAccountTestMatrix(host, p, results, accounts.length, false, caps);
    }
  } finally {
    btn.disabled = false;
    btn.textContent = orig;
  }
  renderAccountTestMatrix(host, p, results, accounts.length, true, caps);
}

// renderAccountTestMatrix paints the test-all progress/result table. Verdicts
// are backend-owned (status ok / error); http_status 0 means no upstream
// answer (build/auth/network error) and renders the reason instead.
function renderAccountTestMatrix(host, p, results, total, done, caps) {
  const capsLine = caps
    ? `${caps.models} model${caps.models === 1 ? '' : 's'} · chat ${caps.chat} · anthropic ${caps.anthropic} · responses ${caps.responses}`
    : '';
  const pending = total - results.length;
  const rows = results.map((r) => {
    const ok = r.status === 'ok';
    const head = r.http_status ? `HTTP ${r.http_status}` : (r.reason || 'probe failed');
    return `<tr>
      <td>${esc(r.label || '—')}</td>
      <td><span class="badge ${ok ? 'ok' : 'err'}">${ok ? 'ok' : 'fail'}</span></td>
      <td class="mono">${esc(head)}</td>
      <td class="num">${r.latency_ms != null ? esc(String(r.latency_ms)) + 'ms' : '—'}</td>
      <td class="mono">${esc(r.model || '—')}</td>
    </tr>`;
  }).join('');
  host.innerHTML = `<section class="card acct-matrix"><div class="card-body">
    <div class="card-title">Test All <span class="hint">${esc(p.name)}${capsLine ? ' · ' + esc(capsLine) : ''}${done ? '' : ' · ' + pending + ' pending'}</span></div>
    <table class="table">
      <thead><tr><th>account</th><th>verdict</th><th>http</th><th class="num">latency</th><th>model</th></tr></thead>
      <tbody>${rows}</tbody>
    </table>
  </div></section>`;
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
  // Batch "Test all" - sequential probe over every account, matrix results.
  const testAll = main.querySelector('[data-test-all]');
  if (testAll) testAll.addEventListener('click', () => testAllAccounts(testAll, p));
  // Re-login buttons appear on session-expired / not-logged-in aqp/codex
  // accounts - they reuse the same async login flow as Add account (aqp is
  // single-credential, so re-login overwrites the stale SSO cookie in place).
  main.querySelectorAll('[data-relogin]').forEach((b) => {
    b.addEventListener('click', () => startAsyncLogin(p.name, p.provider_id));
  });
}

// renderProviderDetail builds the right pane: a toolbar (provider name + meta +
// Add button) and the list of account cards (or an empty-state prompt). With
// two or more accounts the toolbar also carries "Test all" — the batch probe
// whose matrix lands in #acct-test-matrix between toolbar and cards.
function renderProviderDetail(p, quota, tokens) {
  const meta = `${esc(p.provider_id || '?')}${p.billing ? ' · ' + esc(p.billing) : ''}`;
  const accounts = p.accounts || [];
  const testAll = accounts.length >= 2
    ? `<button class="btn small" data-test-all
               title="Probe every account sequentially (real upstream requests)">Test All (${accounts.length})</button>`
    : '';
  const body = accounts.length === 0
    ? `<div class="empty-state">No account configured. Click <strong>Add</strong> to sign in.</div>`
    : accounts.map((a) => accountCard(p, a, quota, tokens)).join('');
  return `<div class="acct-toolbar">
      <div class="acct-toolbar-title">
        <h2>${esc(p.name)}</h2>
        <span class="meta">${meta}</span>
      </div>
      <div class="row-actions">
        ${testAll}
        <button class="btn small" data-add data-provider="${esc(p.name)}">+ Add account</button>
      </div>
    </div>
    <div id="acct-test-matrix"></div>
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
  // Providers without a usage endpoint have no Quota() to poll, so the
  // Refresh-usage button (which re-polls quota) is meaningless for them — hide
  // it. Pay-as-you-go alone doesn't decide: deepseek is pay-as-you-go WITH a
  // usage_url (/user/balance) and is polled like a plan provider.
  const refreshBtn = (p.billing === 'pay-as-you-go' && !p.usage_endpoint) ? ''
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
// so the user can scan without expanding. Pure hint/open logic lives in
// accountUsageState (pure.js) so it can be unit-tested.
function accountUsageDetails(p, snap, acctKey) {
  const { hint, open } = accountUsageState(snap);
  const openAttr = open ? ' open' : '';
  return `<details class="acct-section" data-acct="${esc(acctKey)}" data-sec="usage"${openAttr}>
    <summary>${iconChevron()}Usage<span class="acct-hint">${esc(hint)}</span></summary>
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
    : 'No usage';
  // Default the section to collapsed when there are no token rows ("no usage").
  const openAttr = rows.length ? ' open' : '';
  return `<details class="acct-section" data-acct="${esc(acctKey)}" data-sec="tokens"${openAttr}>
    <summary>${iconChevron()}Token usage<span class="acct-hint">${esc(hint)}</span></summary>
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
function renderAccountUsage(p, snap) {
  if (!snap) return `<div class="acct-empty">No usage data</div>`;
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
  if (!rows || rows.length === 0) return `<div class="acct-empty">No token usage observed</div>`;
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
      <th>Model</th><th class="num">Input</th><th class="num">Output</th>
      <th class="num">Cache Create</th><th class="num">Cache Read</th><th class="num">Requests</th>
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
// Renders the Analytics tab: one compact toolbar (range / granularity /
// provider / model / agent / dimension), a KPI row with period-over-period
// deltas, ONE trend chart with a metric switcher (tokens / cost / requests /
// failovers / 429s / errors / latency / ttft / cache — failovers/429s are
// minute_buckets-only and disable in agent-dimension reads), a weekday×hour
// token-usage heatmap (fixed semantic — when the proxy actually gets used,
// independent of the metric switcher), and a leaderboard table sorted
// by the active metric. The
// API binding (see /api/analytics in docs/web-api.md):
//   series[].points[].{bucket,requests,failovers,rate_limited_429,failures,
//     input,output,cache_creation,cache_read,avg_latency_ms,avg_ttft_ms,cost,priced}
//   totals.{requests,failures,input,output,cache_creation,cache_read,cost}
//   compare.{from,to,requests,failures,input,output,cost}  (equal-length previous window)
//   price_coverage.{priced,unpriced}
//   heatmap.cells[].{weekday(0=Mon),hour,…same derived block as totals}
//   agents.[]  (in-range facet, feeds the agent filter's datalist)
// Per-request/token tables live on the Status page (Token usage/Agents); this
// tab owns trends, reliability and cost analysis. by=agent switches the
// series dimension to agent_buckets ("which client is burning tokens"); the
// agent filter narrows BOTH dimensions (by=model reroutes through
// agent_buckets server-side).
// Control selections persist to localStorage so a refresh keeps the view.

// analyticsState reads the tab's control selections from localStorage (with
// sane defaults). Returns {range, gran, provider, model, by, metric}.
// analyticsState reads the tab's control selections from localStorage (with
// sane defaults). Returns {range, gran, provider, model, by, metric}; range
// is the same {preset, customStart, customEnd} shape as the Status→Token
// usage picker (persisted as JSON under 'an-range2').
function analyticsState() {
  let range = { preset: '1h', customStart: '', customEnd: '' };
  try {
    const raw = localStorage.getItem('an-range2');
    if (raw) {
      const v = JSON.parse(raw);
      if (v && typeof v.preset === 'string') range = { customStart: '', customEnd: '', ...v };
    }
  } catch (_) { /* ignore */ }
  return {
    range,
    gran: localStorage.getItem('an-gran') || 'auto',
    provider: localStorage.getItem('an-provider') || '',
    model: localStorage.getItem('an-model') || '',
    agent: localStorage.getItem('an-agent') || '',
    by: localStorage.getItem('an-by') || 'model',
    metric: localStorage.getItem('an-metric') || 'tokens',
  };
}

// analyticsSave persists one control value (strings verbatim, objects as
// JSON). Wrap in try/catch so private-mode browsers (where localStorage
// throws) don't break the tab.
function analyticsSave(name, val) {
  try { localStorage.setItem('an-' + name, typeof val === 'string' ? val : JSON.stringify(val)); } catch (_) { /* ignore */ }
}

// analyticsRangeBounds resolves the picker state to {from, to} unix seconds:
// presets via tokenRangeBounds (local-time aligned), custom via
// tokenCustomBounds (closed full local days), 'all' → from 0 (whole history).
// An invalid custom range returns null (the caller falls back to all-time).
function analyticsRangeBounds(range) {
  if (range.preset === 'custom') return tokenCustomBounds(range.customStart, range.customEnd);
  if (range.preset === 'all') return { from: 0, to: Math.floor(Date.now() / 1000) };
  return tokenRangeBounds(range.preset);
}

// anRangePicker is the analytics date picker's own UI state (same shape as
// the Status tab's tokensRangePicker): open flag, the two-month calendar
// view anchor, an in-progress custom start-day pick. Module-level so it
// survives the picker's own re-renders (which deliberately do NOT refetch).
let anRangePicker = { open: false, view: null, pick: null, selecting: false };

// analyticsPickerClose closes the popover and discards any in-progress pick
// — Esc and outside clicks never change the applied range.
function analyticsPickerClose() {
  anRangePicker = { open: false, view: null, pick: null, selecting: false };
  document.removeEventListener('keydown', analyticsPickerOnKey);
  document.removeEventListener('click', analyticsPickerOnOutside, true);
}

function analyticsPickerOnKey(e) {
  if (e.key === 'Escape') {
    const panel = panels.analytics;
    analyticsPickerClose();
    if (panel) analyticsPickerRender(panel);
  }
}

function analyticsPickerOnOutside(e) {
  if (!e.target.closest('.tr-wrap')) {
    const panel = panels.analytics;
    analyticsPickerClose();
    if (panel) analyticsPickerRender(panel);
  }
}

// analyticsPickerHTML renders the date-range trigger + popover using the same
// .tr-* markup and pure.js calendar helpers as the Status→Token usage picker
// (identical look and interaction), bound to the analytics range state.
function analyticsPickerHTML() {
  const range = analyticsState().range;
  const picker = anRangePicker;
  const presets = TOKEN_RANGES.map((w) => {
    const active = w.value === 'custom'
      ? (range.preset === 'custom' || picker.selecting)
      : range.preset === w.value;
    return `<button class="tr-preset${active ? ' active' : ''}" data-an-preset="${esc(w.value)}">
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
        const applied = range.preset === 'custom' && !picker.pick ? range : null;
        const isStart = dayYmd === picker.pick || (applied && dayYmd === applied.customStart);
        const isEnd = applied && dayYmd === applied.customEnd;
        const inRange = applied && !isStart && !isEnd &&
          dayYmd > applied.customStart && dayYmd < applied.customEnd;
        const cls = ['tr-day'];
        if (isStart || isEnd) cls.push('tr-day-selected');
        else if (inRange) cls.push('tr-day-inrange');
        return `<td><button class="${cls.join(' ')}" data-an-day="${dayYmd}" ${future ? 'disabled' : ''}>${day}</button></td>`;
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
      <button class="tr-nav tr-prev" data-an-nav="-1" aria-label="previous month">‹</button>
      <div class="tr-months">${months}</div>
      <button class="tr-nav tr-next" data-an-nav="1" aria-label="next month" ${viewRight >= thisMonth ? 'disabled' : ''}>›</button>
    </div>`;
  }

  return `<div class="tr-wrap">
    <button class="btn small tr-trigger" type="button" aria-haspopup="true" aria-expanded="${picker.open}">
      <span class="tr-caption">Time Range</span>
      <span class="tr-value">${esc(tokenRangeTriggerLabel(range))}</span>
      <span class="tr-chevron">▾</span>
    </button>
    <div class="tr-popover" data-popup ${picker.open ? '' : 'hidden'}>
      <div class="tr-presets">${presets}</div>
      ${calendar}
    </div>
  </div>`;
}

// analyticsPickerRender (re)draws the picker into its toolbar host and wires
// the interactions. Picker-internal updates (open/close, month navigation,
// day picking) re-render ONLY this markup — no refetch, no chart rebuild;
// applying a range persists it and re-renders the whole tab.
function analyticsPickerRender(panel) {
  const host = panel.querySelector('#an-range-host');
  if (!host) return;
  host.innerHTML = analyticsPickerHTML();
  host.querySelector('.tr-trigger').onclick = () => {
    if (anRangePicker.open) {
      analyticsPickerClose();
    } else {
      const anchor = (analyticsState().range.preset === 'custom' && parseLocalDate(analyticsState().range.customStart)) || new Date();
      anRangePicker = { open: true, view: { year: anchor.getFullYear(), month: anchor.getMonth() }, pick: null, selecting: false };
      document.addEventListener('keydown', analyticsPickerOnKey);
      document.addEventListener('click', analyticsPickerOnOutside, true);
    }
    analyticsPickerRender(panel);
  };
  host.querySelectorAll('[data-an-preset]').forEach((btn) => {
    btn.onclick = () => {
      const value = btn.dataset.anPreset;
      if (value === 'custom') {
        anRangePicker.selecting = true;
        anRangePicker.pick = null;
        analyticsPickerRender(panel);
        return;
      }
      analyticsSave('range2', { preset: value, customStart: '', customEnd: '' });
      // A new window invalidates an explicit granularity pick (minute over a
      // month is nonsense) — reset to auto, which re-derives from the span;
      // a new window also drops any drag-zoom selection.
      analyticsSave('gran', 'auto');
      anZoom = null;
      analyticsPickerClose();
      renderAnalyticsTab();
    };
  });
  host.querySelectorAll('[data-an-day]').forEach((btn) => {
    btn.onclick = () => {
      if (!anRangePicker.selecting && analyticsState().range.preset !== 'custom') {
        anRangePicker.selecting = true;
      }
      const result = rangePick(anRangePicker.pick, btn.dataset.anDay);
      if (!result.complete) {
        anRangePicker.pick = result.pick;
        analyticsPickerRender(panel);
        return;
      }
      analyticsSave('range2', { preset: 'custom', customStart: result.start, customEnd: result.end });
      analyticsSave('gran', 'auto'); // same reset as preset switches
      anZoom = null;
      analyticsPickerClose();
      renderAnalyticsTab();
    };
  });
  host.querySelectorAll('[data-an-nav]').forEach((btn) => {
    btn.onclick = () => {
      anRangePicker.view = shiftMonth(anRangePicker.view.year, anRangePicker.view.month, Number(btn.dataset.anNav));
      analyticsPickerRender(panel);
    };
  });
}

// analyticsSeg renders one segmented control into `host` ([value,label,
// disabled?] options + the active value) and wires onclick. Everything on
// the toolbar applies immediately — there is no separate Apply/Refresh step.
// Disabled options stay visible (stable layout) but are unclickable. Shared
// by the Analytics tab's toolbar and the Status→Dashboard metric switcher.
function analyticsSeg(host, options, active, onChange) {
  if (!host) return;
  host.innerHTML = options.map((o) =>
    `<button type="button" data-v="${esc(o.value)}"${o.value === active ? ' class="active"' : ''}${o.disabled ? ' disabled' : ''}>${esc(o.label)}</button>`).join('');
  host.onclick = (e) => {
    const btn = e.target.closest('button[data-v]');
    if (btn && !btn.disabled && btn.dataset.v !== active) onChange(btn.dataset.v);
  };
}

// renderAnalyticsTab fetches /api/analytics and renders the toolbar, KPI row
// (with deltas vs the previous equal-length window), the metric-switchable
// trend chart and the leaderboard table. background=true marks the 30s
// live-window tick: entry (and therefore the panel wipe below) is gated on
// user interaction inside the panel — an open picker popover, typing in the
// provider/model filters — so the auto-refresh can never close a dropdown or
// eat uncommitted filter text. Explicit renders (control changes, tab entry)
// are user-initiated: those close the popups themselves and run directly.
// analyticsLayoutHTML is the Analytics tab's full skeleton (toolbar + KPI
// host + chart card + leaderboard host), shared by the success render and
// the first-load failure render so both build the identical DOM.
function analyticsLayoutHTML() {
  return `
    <div class="an-toolbar">
      <span id="an-range-host"></span>
      <div class="an-seg" id="an-gran" role="group" aria-label="Granularity"></div>
      <input id="an-provider" class="req-input" placeholder="Provider" list="an-provider-list" autocomplete="off" spellcheck="false" />
      <datalist id="an-provider-list"></datalist>
      <input id="an-model" class="req-input" placeholder="Model" list="an-model-list" autocomplete="off" spellcheck="false" />
      <datalist id="an-model-list"></datalist>
      <input id="an-agent" class="req-input" placeholder="Agent" list="an-agent-list" autocomplete="off" spellcheck="false" />
      <datalist id="an-agent-list"></datalist>
      <div class="an-seg" id="an-by" role="group" aria-label="Dimension"></div>
    </div>
    <div id="an-error" class="msg err" hidden></div>
    <div id="an-filter-hint" class="an-filter-hint" hidden></div>
    <div id="an-kpis" class="an-kpis"></div>
    <div class="an-chart-card">
      <div class="an-chart-head">
        <div class="an-seg" id="an-metric" role="group" aria-label="Metric"></div>
      </div>
      <div class="an-chart-wrap">
        <div id="an-chart" class="an-chart"></div>
        <button type="button" id="an-zoom-reset" class="an-zoom-reset" hidden>↔ Reset Zoom</button>
      </div>
      <div id="an-legend"></div>
    </div>
    <div id="an-table" class="an-table-card"></div>
    <div class="an-heat-card" hidden>
      <div class="an-heat-head">
        <span id="an-heat-title" class="an-heat-title"></span>
        <span class="an-heat-scale" aria-hidden="true">Less<i class="hm hm-l1"></i><i class="hm hm-l2"></i><i class="hm hm-l3"></i><i class="hm hm-l4"></i>More</span>
      </div>
      <div id="an-heat" class="an-heat-scroll"></div>
    </div>`;
}

// buildAnalyticsLayout (re)builds the toolbar + skeleton and wires every
// control. Shared by every render path (success and first-load failure) so
// the two never drift.
function buildAnalyticsLayout(panel, state, granOptions, granActive) {
  destroyAnalyticsCharts(); // the innerHTML reset below drops the chart DOM
  panel.innerHTML = analyticsLayoutHTML();
  // Date-range picker (same trigger/popover/calendar as Status→Token usage).
  analyticsPickerRender(panel);
  analyticsSeg(panel.querySelector('#an-gran'), granOptions.map((o) => ({
    value: o.id, label: o.label, disabled: !o.allowed,
  })), granActive, (v) => { analyticsSave('gran', v); anZoom = null; renderAnalyticsTab(); });
  analyticsSeg(panel.querySelector('#an-by'), [
    { value: 'model', label: 'By Model' }, { value: 'agent', label: 'By Agent' },
  ], state.by, (v) => { analyticsSave('by', v); renderAnalyticsTab(); });
  // Failovers/429s exist only in minute_buckets: every agent-dimension read
  // (by=agent or an agent filter — both reroute to agent_buckets server-side)
  // disables them; a stored pick that becomes unavailable falls back to
  // tokens (the granularity control's span-gating pattern).
  analyticsSeg(panel.querySelector('#an-metric'), analyticsMetricOptions(state.by, state.agent),
    analyticsMetricAllowed(state.metric, state.by, state.agent) ? state.metric : 'tokens',
    (v) => { analyticsSave('metric', v); renderAnalyticsTab(); });
  const elProvider = panel.querySelector('#an-provider');
  const elModel = panel.querySelector('#an-model');
  const elAgent = panel.querySelector('#an-agent');
  elProvider.value = state.provider;
  elModel.value = state.model;
  elAgent.value = state.agent;
  // Free-text filters: commit on Enter or blur (change), not per keystroke.
  for (const [input, key] of [[elProvider, 'provider'], [elModel, 'model'], [elAgent, 'agent']]) {
    input.onchange = () => {
      const v = input.value.trim();
      if (v === analyticsState()[key]) return; // unchanged — no refetch
      analyticsSave(key, v);
      renderAnalyticsTab();
    };
    input.onkeydown = (e) => { if (e.key === 'Enter') input.blur(); };
  }
}

async function renderAnalyticsTab(background = false) {
  const panel = panels.analytics;
  if (!panel) return;
  if (background && deferAutoRefresh(panel, () => renderAnalyticsTab(true))) return;
  analyticsStopAutoRefresh();
  const state = analyticsState();
  // Window bounds drive both the query and the granularity gating — computed
  // from state (localStorage) BEFORE any DOM write so a failed background
  // refresh can keep the previous view untouched.
  const bounds = analyticsRangeBounds(state.range) || { from: 0, to: Math.floor(Date.now() / 1000) };
  const spanSec = Math.max(bounds.to - bounds.from, 1);
  const granOptions = analyticsGranOptions(spanSec);
  // A stored explicit granularity the new range disallows shows 'auto'
  // active (the effective pick falls back inside analyticsGranularity).
  const granPrefAllowed = state.gran === 'auto' || (granOptions.find((o) => o.id === state.gran) || {}).allowed;
  const granActive = granPrefAllowed ? state.gran : 'auto';
  const gran = analyticsGranularity(spanSec, granActive);
  // The effective metric can differ from the stored pick in agent-dimension
  // reads (failovers/429s are minute_buckets-only); every renderer below and
  // the toolbar's segment must agree on it.
  const metric = analyticsMetricAllowed(state.metric, state.by, state.agent) ? state.metric : 'tokens';
  const q = new URLSearchParams({ from: String(bounds.from), to: String(bounds.to), granularity: gran, by: state.by });
  if (state.provider) q.set('provider', state.provider);
  if (state.model) q.set('model', state.model);
  if (state.agent) q.set('agent', state.agent);
  let resp = null;
  let fetchErr = null;
  try {
    resp = await apiGet('/api/analytics?' + q.toString());
  } catch (e) {
    fetchErr = e;
  }
  // A failed refresh with a rendered view must not destroy it: keep the
  // charts/KPIs on screen and report through the shared stale-data banner.
  // (The auto-refresh timer is re-armed below so a later tick retries.)
  if (fetchErr && panel.querySelector('#an-kpis')) {
    setRefreshError(panel, staleDataText('Analytics unavailable: ' + fetchErr.message));
    analyticsMaybeAutoRefresh();
    return;
  }
  // Commit-time gate: an interaction that started while this fetch was in
  // flight (picker opened, filter focused) defers the landing writes — the
  // hold watcher re-runs a fresh background render once the user is done.
  if (background && deferAutoRefresh(panel, () => renderAnalyticsTab(true))) return;
  buildAnalyticsLayout(panel, state, granOptions, granActive);
  const errEl = panel.querySelector('#an-error');
  if (fetchErr) {
    // First render with nothing to preserve: skeleton + inline error (the
    // toolbar stays wired so the user can change filters and retry).
    if (errEl) {
      errEl.hidden = false;
      errEl.textContent = 'Analytics unavailable: ' + fetchErr.message;
    }
    analyticsMaybeAutoRefresh();
    return;
  }
  if (errEl) errEl.hidden = true;
  analyticsFillDatalists(panel, resp, state.provider);
  analyticsFilterHint(panel, resp, state);
  analyticsRenderKpis(panel.querySelector('#an-kpis'), resp);
  analyticsRenderCharts(panel, resp, metric, gran);
  analyticsRenderHeatmap(panel, resp);
  analyticsRenderTable(panel, resp, metric);
  analyticsMaybeAutoRefresh();
}

// anRefreshTimer periodically re-renders the tab for the LIVE windows (Last
// 1h / Today) whose trailing edge moves with the clock — a stale "last hour"
// chart is a lie by omission. Everything else refreshes on demand. Ticks pass
// through the interaction gate: an open picker or focused filter defers the
// re-render until the interaction ends (the hold watcher fires a fresh
// background render). The interval is re-armed by every render; switching
// tabs leaves a ticking no-op until the next analytics render stops it.
let anRefreshTimer = null;

function analyticsStopAutoRefresh() {
  if (anRefreshTimer) {
    clearInterval(anRefreshTimer);
    anRefreshTimer = null;
  }
  cancelAutoRefreshHold(panels.analytics);
}

// analyticsMaybeAutoRefresh arms the live-window refresh: every 30s while
// the Analytics tab is the ACTIVE view, deferred while the user interacts
// with the toolbar (open date picker, focused filter input) via the shared
// auto-refresh gate.
function analyticsMaybeAutoRefresh() {
  analyticsStopAutoRefresh();
  const st = analyticsState();
  if (st.range.preset !== '1h' && st.range.preset !== 'today') return;
  anRefreshTimer = setInterval(() => {
    const panel = panels.analytics;
    if (!panel || !panel.classList.contains('active')) return;
    if (deferAutoRefresh(panel, () => renderAnalyticsTab(true))) return;
    renderAnalyticsTab(true);
  }, 30000);
}

// fmtDelta renders a pctDelta as the chip's delta line: "▲ +12.3%". null
// (undefined comparison: no previous window, or previous was zero) renders
// an em dash so the chip stays aligned. warn=true colors a rise as a warning
// (failures); otherwise deltas stay neutral — more requests or cost is not
// inherently bad.
function fmtDelta(delta, warn) {
  // v2: delta direction is colored by semantics (kpiDeltaClass): up is ok
  // for normal metrics, err for warn metrics (failures rising); down/flat
  // stays muted — quieter than red, since a drop is not an error.
  if (delta == null) return '<span class="d flat">—</span>';
  const arrow = delta > 0 ? '▲' : delta < 0 ? '▼' : '';
  const sign = delta > 0 ? '+' : '';
  return `<span class="d ${kpiDeltaClass(delta, warn)}">${arrow} ${sign}${delta}%</span>`;
}

// analyticsFilterHint surfaces the silent-empty trap: an active free-text
// filter that matches nothing zeroes every view on the page (the agent
// filter is an EXACT server-side match — one typo and KPIs, chart, heatmap
// and table all read "no data"), which looks like the data is gone. When
// the response has no series but filters are set, name the active filters
// (and, for a misspelled agent, the agents that DO have traffic) and offer
// a one-click clear.
function analyticsFilterHint(panel, resp, state) {
  const hint = panel.querySelector('#an-filter-hint');
  if (!hint) return;
  const series = (resp && resp.series) || [];
  const active = [];
  if (state.provider) active.push(`provider:${state.provider}`);
  if (state.model) active.push(`model:${state.model}`);
  if (state.agent) active.push(`agent:${state.agent}`);
  const agents = (resp && resp.agents) || [];
  if (!series.length && active.length) {
    const miss = state.agent && agents.length && !agents.includes(state.agent)
      ? ` — agents with traffic: ${agents.join(', ')}` : '';
    hint.innerHTML = `No data for ${active.map((a) => `<code>${esc(a)}</code>`).join(' · ')}${esc(miss)} <button type="button" class="btn small" id="an-filter-clear">Clear Filters</button>`;
    hint.hidden = false;
    hint.querySelector('#an-filter-clear').onclick = () => {
      for (const key of ['provider', 'model', 'agent']) analyticsSave(key, '');
      renderAnalyticsTab();
    };
    return;
  }
  hint.hidden = true;
}

// analyticsRenderKpis renders the summary chips: tokens / tok-s / cache
// hit / requests / failures / cost, each with the delta vs the equal-length
// window before the selected one (resp.compare). Shared by the Analytics
// tab and the Status→Dashboard (fixed 1h window). The tokens chip is the
// four-bucket total (input+output+cache_creation+cache_read — the same
// total as Status→Token usage): cache reads dominate real prompt workloads,
// so an in+out-only count reads as an undercount; the tooltip carries the
// per-bucket breakdown. The hit rate is cache_read over the full prompt
// workload (input + cache_creation + cache_read — the buckets keep input
// EXCLUDING cached tokens). The unpriced-models note rides the cost chip:
// equivalent cost silently excludes unpriced series, so the note keeps that
// visible without a separate banner.
function analyticsRenderKpis(host, resp) {
  if (!host) return;
  const t = (resp && resp.totals) || {};
  const c = (resp && resp.compare) || null;
  const un = (resp && resp.price_coverage && resp.price_coverage.unpriced) || [];
  // Derived metrics (tokens total, tok/s, cache hit) are read straight from
  // the server's unified block — the same definitions as every other view.
  const hit = t.cache_hit_pct == null ? null : Number(t.cache_hit_pct);
  const prevHit = c && c.cache_hit_pct != null ? Number(c.cache_hit_pct) : null;
  // Token counts render K/M-compacted (fmtCompact, 2 decimals); the exact
  // per-bucket breakdown stays one hover away via the title tooltip.
  const tokensChip = {
    k: 'Tokens',
    v: fmtCompact(t.tokens || 0, 2),
    tip: `in ${fmtNum(t.input || 0)} · out ${fmtNum(t.output || 0)} · cache read ${fmtNum(t.cache_read || 0)} · cache write ${fmtNum(t.cache_creation || 0)}`,
    d: pctDelta(t.tokens || 0, c ? c.tokens : null),
  };
  const chips = [
    tokensChip,
    (() => {
      // Window tok/s: the server's OUTPUT-decode-speed field (output ÷ full
      // call seconds — matches what clients display; fresh input is prefix
      // speed, not decode).
      const v = t.tok_sec == null ? null : Number(t.tok_sec);
      const pv = c && c.tok_sec != null ? Number(c.tok_sec) : null;
      return { k: 'Tok/s', v: v == null ? '—' : v.toFixed(1), tip: v == null ? '' : v.toFixed(2) + ' output tokens per call-second', d: pctDelta(v, pv) };
    })(),
    { k: 'Cache Hit', v: hit == null ? '—' : hit.toFixed(1) + '%', d: pctDelta(hit, prevHit) },
    { k: 'Requests', v: fmtNum(t.requests || 0), d: pctDelta(t.requests || 0, c && c.requests) },
    {
      k: 'Failures',
      v: fmtNum(t.failures || 0),
      d: pctDelta(t.failures || 0, c && c.failures),
      warn: true,
      // Attempt-level pressure hides behind the terminal-failure count:
      // retried failovers and upstream rate limits both burn quota without
      // failing the request — keep them one hover away.
      tip: (t.failovers || t.rate_limited_429)
        ? `${fmtNum(t.failovers || 0)} failover attempts · ${fmtNum(t.rate_limited_429 || 0)} rate-limited (429)`
        : '',
    },
    {
      k: 'Cost (USD)',
      v: t.cost == null ? 'n/a' : '$' + t.cost.toFixed(2),
      d: pctDelta(t.cost == null ? null : t.cost, c ? c.cost : null),
      note: un.length ? `${un.length} unpriced: ${un.join(', ')}` : '',
    },
  ];
  host.innerHTML = chips.map((chip) => `
    <div class="an-kpi">
      <div class="k">${esc(chip.k)}</div>
      <div class="v${chip.k === 'Failures' && Number(chip.v) > 0 ? ' err' : ''}"${chip.tip ? ` title="${esc(chip.tip)}"` : ''}>${esc(chip.v)}</div>
      <div>${fmtDelta(chip.d, chip.warn)}${chip.note ? `<div class="note" title="${esc(chip.note)}">${esc(chip.note)}</div>` : ''}</div>
    </div>`).join('');
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

// Debounced window-resize re-size for the live uPlot canvases: both the
// Dashboard and Analytics charts size to their host at render time, and
// without this hook a window resize leaves them stale until the next data
// tick (30s on the live window). Only charts still mounted in the DOM
// resize — hidden tabs rebuild on entry. Height is constant (260).
let chartResizeTimer = null;
window.addEventListener('resize', () => {
  if (chartResizeTimer) clearTimeout(chartResizeTimer);
  chartResizeTimer = setTimeout(() => {
    chartResizeTimer = null;
    const rewidth = (u, tracked) => {
      try {
        if (!u || !u.root || !document.contains(u.root)) return tracked;
        const host = u.root.parentElement;
        if (!host) return tracked;
        const w = Math.max(host.clientWidth || 600, 320);
        if (tracked == null || Math.abs(tracked - w) > 1) u.setSize({ width: w, height: 260 });
        return w;
      } catch (_) { return tracked; }
    };
    if (dashChart && dashChart.u) dashChart.width = rewidth(dashChart.u, dashChart.width) ?? dashChart.width;
    for (const u of analyticsCharts) rewidth(u, null);
    // The sticky session views' heights ride the trace SVG's aspect ratio,
    // so the table-header offset they feed (--sess-h) goes stale on resize.
    document.querySelectorAll('.sess-sticky').forEach((el) => syncSessThOffset(el));
    // A taller/shorter viewport shows more/fewer virtualized request rows.
    if (reqVirt) reqFrame(reqVirt);
  }, 150);
});

// destroyAnalyticsCharts tears down the charts rendered for the previous view
// and detaches the legend dropdown's document-level listener (the innerHTML
// reset below would otherwise orphan it).
function destroyAnalyticsCharts() {
  if (anLegendOutside) {
    document.removeEventListener('click', anLegendOutside);
    anLegendOutside = null;
  }
  for (const u of analyticsCharts) {
    try { u.destroy(); } catch (_) { /* already detached */ }
  }
  analyticsCharts = [];
}

// analyticsFillDatalists populates the provider/model/agent suggestion lists
// from the response so the free-text filters are usable. The provider→models
// map (and the agent set) only grow within a session: a narrowed response
// must not erase options the user can switch back to. Agent suggestions come
// from the server's in-range facet (ignoring the agent filter itself) unioned
// with agents seen in by=agent series.
const analyticsFacetModels = new Map();
const analyticsFacetAgents = new Set();
function analyticsFillDatalists(panel, resp, provider) {
  for (const s of ((resp && resp.series) || [])) {
    if (!s || !s.provider) continue;
    let set = analyticsFacetModels.get(s.provider);
    if (!set) { set = new Set(); analyticsFacetModels.set(s.provider, set); }
    set.add(s.model);
    if (s.agent) analyticsFacetAgents.add(s.agent);
  }
  for (const a of ((resp && resp.agents) || [])) analyticsFacetAgents.add(a);
  const providers = [...analyticsFacetModels.keys()].sort();
  const models = linkedModels(provider || '', Object.fromEntries([...analyticsFacetModels].map(([p, set]) => [p, [...set]])), {});
  const agents = [...analyticsFacetAgents].sort();
  const pList = panel.querySelector('#an-provider-list');
  const mList = panel.querySelector('#an-model-list');
  const aList = panel.querySelector('#an-agent-list');
  if (pList) pList.innerHTML = providers.map((p) => `<option value="${esc(p)}"></option>`).join('');
  if (mList) mList.innerHTML = models.map((m) => `<option value="${esc(m)}"></option>`).join('');
  if (aList) aList.innerHTML = agents.map((a) => `<option value="${esc(a)}"></option>`).join('');
}

// analyticsBucketLabel formats one bucket timestamp (unix seconds) for the
// tooltip header, matching the granularity's natural resolution in the local
// timezone.
function analyticsBucketLabel(t, gran) {
  const d = new Date(t * 1000);
  const p = (n) => String(n).padStart(2, '0');
  if (gran === 'month') return `${d.getFullYear()}-${p(d.getMonth() + 1)}`;
  if (gran === 'week' || gran === 'day') return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  if (gran === 'minute') return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:00`;
}

// analyticsWindowGrid builds the complete bucket-timestamp grid covering the
// queried window at the chosen granularity (local-timezone day/month starts,
// matching the server's calendar bucketing). The chart x axis uses this grid
// so sparse traffic renders honestly: empty buckets become zero (count
// metrics) or gaps (rate metrics) instead of long diagonal lines connecting
// distant points. Capped defensively; an oversized/degenerate grid falls back
// to empty (the chart then spans only the data buckets).
function analyticsWindowGrid(from, to, gran) {
  if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from) return [];
  const out = [];
  if (gran === 'minute' || gran === 'hour') {
    // Fixed-second steps; anchor at the boundary (timezone offsets are
    // constant except across DST, a cosmetic edge).
    const step = gran === 'minute' ? 60 : 3600;
    for (let t = Math.floor(from / step) * step; t <= to && out.length < 10000; t += step) out.push(t);
    return out;
  }
  const end = new Date(to * 1000);
  let d = new Date(from * 1000);
  if (gran === 'month') {
    d = new Date(d.getFullYear(), d.getMonth(), 1);
    while (d <= end && out.length < 10000) {
      out.push(Math.floor(d.getTime() / 1000));
      d.setMonth(d.getMonth() + 1);
    }
  } else if (gran === 'week') {
    // Local weeks start Monday (same as the server's bucketing).
    d = new Date(d.getFullYear(), d.getMonth(), d.getDate() - ((d.getDay() + 6) % 7));
    while (d <= end && out.length < 10000) {
      out.push(Math.floor(d.getTime() / 1000));
      d.setDate(d.getDate() + 7);
    }
  } else {
    d = new Date(d.getFullYear(), d.getMonth(), d.getDate());
    while (d <= end && out.length < 10000) {
      out.push(Math.floor(d.getTime() / 1000));
      d.setDate(d.getDate() + 1);
    }
  }
  return out;
}

// withAlpha turns a #rrggbb chart color into an rgba() string at alpha a
// (overlapping translucent bars stay distinguishable). Non-hex inputs pass
// through unchanged (the palette is hex; the fallback keeps the stroke).
function withAlpha(color, a) {
  if (/^#[0-9a-f]{6}$/i.test(color)) {
    const n = parseInt(color.slice(1), 16);
    return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`;
  }
  return color;
}

// analyticsBarPaths returns a per-series uPlot path builder that draws one
// column per bucket instead of a connecting line — the honest rendering for
// the additive metrics (tokens/cost/requests): an empty bucket has NO bar
// (not a zero-line), and a lone bucket is a single column (not a long
// interpolating diagonal). Columns span 70% of the smallest gap between
// adjacent x ticks (the window grid is complete, so the gap is the bucket
// step) and overlap translucently across series; the legend's
// click-to-toggle isolates a noisy series.
function analyticsBarPaths() {
  return (u, sidx, i0, i1) => {
    const p = new Path2D();
    const xs = u.data[0];
    const ys = u.data[sidx];
    let step = Infinity;
    for (let k = 1; k < xs.length; k++) {
      const d = xs[k] - xs[k - 1];
      if (d > 0 && d < step) step = d;
    }
    if (!isFinite(step)) step = 3600; // single bucket: hour-wide default
    const wSec = step * 0.35; // half-width → 70% total span
    const yBase = u.valToPos(0, 'y', true);
    for (let i = i0; i <= i1; i++) {
      const v = ys[i];
      if (v == null) continue;
      const yV = u.valToPos(v, 'y', true);
      if (yBase - yV < 0.5) continue; // zero-height column = no usage
      const x0 = u.valToPos(xs[i] - wSec, 'x', true);
      const x1 = u.valToPos(xs[i] + wSec, 'x', true);
      p.rect(x0, yV, x1 - x0, yBase - yV);
    }
    return { stroke: p, fill: p };
  };
}

// analyticsTooltip builds a uPlot plugin rendering a floating tooltip at the
// cursor: the bucket's local time plus one row per VISIBLE series (color dot,
// label, metric-formatted value — legend-toggled series are skipped). The tip
// lives inside u.over so it clips with the plot; pointer-events: none keeps
// it from swallowing the cursor. Flips to the left of the cursor near the
// right edge.
function analyticsTooltip(metricId, gran) {
  return {
    hooks: {
      init: (u) => {
        const tip = document.createElement('div');
        tip.className = 'an-tip';
        tip.hidden = true;
        u.over.appendChild(tip);
        u.anTip = tip;
      },
      setCursor: (u) => {
        const tip = u.anTip;
        if (!tip) return;
        const idx = u.cursor.idx;
        if (idx == null || u.cursor.left < 0 || !u.data[0] || !u.data[0].length) {
          tip.hidden = true;
          return;
        }
        const t = u.data[0][idx];
        let rows = '';
        for (let i = 1; i < u.series.length; i++) {
          if (u.series[i].show === false) continue; // legend-toggled off
          const v = u.data[i][idx];
          const stroke = u.series[i].stroke;
          const dot = typeof stroke === 'string' ? ` style="background:${stroke}"` : '';
          rows += `<div class="row"><i class="dot"${dot}></i><span class="lab">${esc(u.series[i].label)}</span><span>${esc(analyticsValueText(metricId, v))}</span></div>`;
        }
        tip.innerHTML = `<div class="t">${esc(analyticsBucketLabel(t, gran))}</div>${rows}`;
        tip.hidden = false;
        // Position next to the cursor, flipping near the right edge.
        const w = tip.offsetWidth, h = tip.offsetHeight;
        let x = u.cursor.left + 14;
        if (x + w > u.over.clientWidth - 4) x = Math.max(u.cursor.left - w - 14, 4);
        let y = u.cursor.top + 14;
        if (y + h > u.over.clientHeight - 4) y = Math.max(u.cursor.top - h - 14, 4);
        tip.style.left = x + 'px';
        tip.style.top = y + 'px';
      },
    },
  };
}

// analyticsRenderCharts draws the selected metric's trend chart with uPlot.
// Each series becomes one line sharing the palette across metric switches. x
// is in unix seconds — uPlot's time unit — and every series gets an explicit
// stroke because uPlot 1.6.x does not auto-assign colors (a missing stroke
// renders the axes and legend but no line). Points stay visible: a series
// with a single bucket (sparse traffic) would otherwise render nothing at
// all — an isolated point draws no line segment.
function analyticsRenderCharts(panel, resp, metricId, gran) {
  if (typeof uPlot === 'undefined') return; // vendored script failed to load
  const host = panel.querySelector('#an-chart');
  if (!host) return;
  destroyAnalyticsCharts();
  host.innerHTML = '';
  const metric = ANALYTICS_METRICS.find((m) => m.id === metricId) || ANALYTICS_METRICS[0];
  // The window grid comes from the response's echoed from/to (not the local
  // clock) so the axis covers exactly what was queried.
  const grid = analyticsWindowGrid(resp && resp.from, resp && resp.to, gran);
  const data = analyticsChartSeries(resp && resp.series, metric.id, grid);
  if (!data.x.length || !data.labels.length) {
    host.innerHTML = '<div class="empty-state">No series in range</div>';
    return;
  }
  const colors = analyticsChartColors();
  const fullRange = analyticsXRange(data.x);
  const series = [{ label: 'time' }];
  data.labels.forEach((label, i) => {
    // Every metric renders as translucent columns (one aggregate per
    // bucket): a connecting line would imply traffic between sparse
    // buckets, and isolated points read as scatter, not as levels.
    const stroke = colors[i % colors.length];
    series.push({ label, stroke, width: 1, fill: withAlpha(stroke, 0.55), paths: analyticsBarPaths(), points: { show: false } });
  });
  // Canvas-drawn axis text/ticks/grid don't inherit CSS: forward the theme
  // colors (same helper as the Status→Dashboard charts) so the axes stay
  // readable in dark mode. Tick values use K/M compaction — token counts
  // dwarf the default width. The x axis formats in LOCAL 24-hour time.
  const axis = uplotAxisStyle();
  const opts = {
    title: metric.label + ' (' + metric.axis + ')',
    width: Math.max(host.clientWidth || 600, 320),
    height: 260,
    series,
    // Every metric here is a non-negative quantity, so the y scale is pinned
    // to a 0 baseline: uPlot's auto-zoom would magnify noise (a 5%→8% error
    // rate as a full-height cliff) and drop the bar columns' shared floor.
    // The x scale pads both edges (see analyticsXRange) so edge columns
    // render at full width with breathing room at the right — and defers to
    // an active drag-zoom selection, which auto-refresh re-renders must
    // preserve instead of resetting.
    scales: {
      x: { time: true, range: () => (anZoom ? [anZoom.from, anZoom.to] : fullRange) },
      y: { range: (_u, min, max) => [0, Math.max(max, min || 0, 1)] },
    },
    cursor: { drag: { x: true, y: false, setScale: true } },
    hooks: {
      // Record the finished drag-selection's data range so re-renders
      // (auto-refresh, metric switch) restore the zoomed window.
      setSelect: [(u) => {
        const from = u.posToVal(u.select.left, 'x');
        const to = u.posToVal(u.select.left + u.select.width, 'x');
        if (to - from > 1) {
          anZoom = { from, to };
          analyticsZoomControls(panel);
        }
      }],
    },
    plugins: [analyticsTooltip(metric.id, gran)],
    axes: [
      { ...axis, values: analyticsXAxisValues },
      { label: metric.axis, size: 60, ...axis, values: (_u, splits) => splits.map((v) => (v == null ? '' : fmtCompact(v))) },
    ],
    // The built-in legend is replaced by analyticsRenderLegend (row-limited
    // chips + a "+N more" dropdown when the series list is long).
    legend: { show: false },
  };
  try {
    const u = new uPlot(opts, [data.x, ...data.ys], host);
    analyticsCharts.push(u);
    analyticsRenderLegend(panel.querySelector('#an-legend'), u, data.labels, colors, anLegendHidden);
    analyticsZoomControls(panel);
  } catch (_) { /* malformed data */ }
}

// anZoom holds the operator's drag-zoom selection ({from, to} unix seconds)
// across re-renders — the 30s live-window refresh and metric switches must
// not reset the view the user deliberately zoomed into. Cleared by the reset
// button and whenever the queried window itself changes (range/granularity).
let anZoom = null;

// analyticsXRange computes the full padded x range for the bucket grid: each
// column spans ~70% of the bucket step CENTERED on its timestamp, so the
// first/last columns stick out past the data extent — pad both edges by at
// least half a column (plus a visual margin on the right) or uPlot clips the
// edge columns narrow.
function analyticsXRange(xs) {
  const first = xs[0], last = xs[xs.length - 1];
  let stepSec = last - first;
  for (let k = 1; k < xs.length; k++) {
    const d = xs[k] - xs[k - 1];
    if (d > 0 && d < stepSec) stepSec = d;
  }
  const padSec = Math.max(stepSec * 0.45, (last - first) * 0.01);
  return [first - padSec, last + Math.max((last - first) * 0.05, padSec)];
}

// analyticsZoomControls toggles the chart corner's "reset zoom" button from
// anZoom and wires its click: clear the selection and restore the full
// padded window on the live chart.
function analyticsZoomControls(panel) {
  const btn = panel.querySelector('#an-zoom-reset');
  if (!btn) return;
  btn.hidden = !anZoom;
  if (btn.onclick) return; // wired once per render
  btn.onclick = () => {
    anZoom = null;
    const u = analyticsCharts[analyticsCharts.length - 1];
    if (u && u.data[0] && u.data[0].length) {
      try { u.setScale('x', { min: analyticsXRange(u.data[0])[0], max: analyticsXRange(u.data[0])[1] }); } catch (_) { /* malformed */ }
    }
    btn.hidden = true;
  };
}

// analyticsTickLabel formats one x-axis tick in LOCAL time, 24-hour clock,
// adapting to the tick's natural resolution: month starts show the month,
// midnights show the date, anything else shows date + HH:mm. `tickSpanSec`
// (the gap between adjacent ticks, when known) demotes dense minute-level
// ticks to HH:mm only: uPlot sizes tick density for its own short time
// labels, and a full MM-DD HH:mm at that density overlaps (measured: 67px
// labels on 63px spacing).
function analyticsTickLabel(v, tickSpanSec) {
  const d = new Date(v * 1000);
  const p = (n) => String(n).padStart(2, '0');
  const midnight = d.getHours() === 0 && d.getMinutes() === 0;
  if (midnight && d.getDate() === 1) return `${d.getFullYear()}-${p(d.getMonth() + 1)}`;
  if (midnight) return `${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  if (tickSpanSec != null && tickSpanSec > 0 && tickSpanSec <= 15 * 60) {
    return `${p(d.getHours())}:${p(d.getMinutes())}`;
  }
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// analyticsXAxisValues is the shared x-axis `values` callback for the trend
// charts: it derives the tick spacing from the split list so every label is
// formatted at a width that fits the density uPlot chose.
function analyticsXAxisValues(_u, splits) {
  const span = splits.length > 1 ? splits[1] - splits[0] : null;
  return splits.map((v) => (v == null ? '' : analyticsTickLabel(v, span)));
}

// anLegendHidden holds series labels toggled off via the legend chips — it
// persists across re-renders within the session (like the dashboard's
// dashHidden) so a metric switch or refetch doesn't silently re-show a series
// the operator dimmed.
const anLegendHidden = new Set();

// anLegendOutside is the document-level click handler that closes the
// overflow dropdown; module-level so destroyAnalyticsCharts can detach it
// when the tab re-renders (no listener leaks across innerHTML resets).
let anLegendOutside = null;

// analyticsRenderLegend draws the custom legend under the chart: one chip
// per series (color dot + label; click toggles the series' columns via
// setSeries) into `host`. `hiddenSet` is the caller's session-persistent set
// of toggled-off labels (the Analytics tab and the Status→Dashboard each own
// one) so a metric switch or refetch doesn't silently re-show a series the
// operator dimmed. When the chips would wrap past two rows, the rest move
// into a "+N more" dropdown instead of sprawling down the card.
function analyticsRenderLegend(host, u, labels, colors, hiddenSet) {
  if (!host) return;
  // Re-apply the caller's hidden set to the fresh chart.
  labels.forEach((label, i) => {
    if (hiddenSet.has(label)) u.setSeries(i + 1, { show: false });
  });
  const chips = labels.map((label, i) => {
    const off = hiddenSet.has(label);
    return `<button type="button" class="an-chip${off ? ' off' : ''}" data-legend="${esc(label)}"><i style="background:${colors[i % colors.length]}"></i>${esc(label)}</button>`;
  }).join('');
  host.innerHTML = `<div class="an-legend">${chips}</div><div class="an-legend-wrap"></div>`;
  host.querySelectorAll('[data-legend]').forEach((btn) => {
    btn.onclick = () => {
      const label = btn.dataset.legend;
      if (hiddenSet.has(label)) hiddenSet.delete(label); else hiddenSet.add(label);
      const idx = labels.indexOf(label);
      if (u && idx >= 0) u.setSeries(idx + 1, { show: !hiddenSet.has(label) });
      btn.classList.toggle('off', hiddenSet.has(label));
    };
  });
  analyticsLegendCollapse(host);
}

// analyticsLegendCollapse measures the rendered chips' rows (offsetTop) and,
// when they exceed two, moves the overflow into a dropdown opened by a
// "+N more" chip. Outside clicks close the dropdown.
function analyticsLegendCollapse(host) {
  const chipsEl = host.querySelector('.an-legend');
  const wrap = host.querySelector('.an-legend-wrap');
  if (!chipsEl || !wrap) return;
  const chips = [...chipsEl.querySelectorAll('.an-chip')];
  if (chips.length < 3) return; // two rows of chips needs at least 3 entries
  const rowTops = [...new Set(chips.map((c) => c.offsetTop))].sort((a, b) => a - b);
  if (rowTops.length <= 2) return;
  const overflow = chips.filter((c) => c.offsetTop > rowTops[1]);
  if (!overflow.length) return;
  const drop = document.createElement('div');
  drop.className = 'an-legend-drop';
  drop.setAttribute('data-popup', ''); // the auto-refresh gate looks for this
  drop.hidden = true;
  overflow.forEach((c) => drop.appendChild(c)); // move, not clone — wiring stays
  const more = document.createElement('button');
  more.type = 'button';
  more.className = 'an-chip an-more';
  more.textContent = `+${overflow.length} more ▾`;
  chipsEl.appendChild(more);
  wrap.appendChild(drop);
  const close = () => { drop.hidden = true; document.removeEventListener('click', anLegendOutside); anLegendOutside = null; };
  more.onclick = (e) => {
    e.stopPropagation();
    if (drop.hidden) {
      drop.hidden = false;
      anLegendOutside = (ev) => { if (!ev.target.closest('.an-legend-wrap')) close(); };
      document.addEventListener('click', anLegendOutside, true);
    } else {
      close();
    }
  };
}

// showHeatTip fills the shared floating tooltip (the same body-level,
// pointer-events-none .tl-tip the session timeline uses — deliberately
// without data-popup so hover can never defer the auto-refresh) with one
// day cell's lines; the renderer stamps data-day on every in-window cell
// so empty days still name their date.
function showHeatTip(cellEl) {
  const dayUnix = Number(cellEl.dataset.day);
  if (!Number.isFinite(dayUnix)) return;
  let cell = null;
  if (cellEl.dataset.cell) {
    try { cell = JSON.parse(cellEl.dataset.cell); } catch (_) { cell = null; }
  }
  const lines = analyticsHeatTipLines(dayUnix, cell);
  const el = tlTip();
  el.innerHTML = `<div class="tl-tip-meta">${lines.map((l) => `<div>${esc(l)}</div>`).join('')}</div>`;
  el.hidden = false;
  placeTlTip(el);
}

// analyticsRenderHeatmap draws the GitHub-style token-usage heatmap: one
// column per Monday-first week over the server's FIXED window (the last
// twelve whole months plus the current month-to-date — independent of the
// toolbar's range/granularity/metric), rows Mon..Sun, month labels CENTERED
// over each month's span (reference only — week columns never align exactly
// to calendar months), SQUARE cells that stretch to FILL the card width.
// Cells shade by the day's four-bucket token total in the 5-level ordinal
// scale; out-of-window slots render invisible, in-window days without
// traffic stay bare. The card hides entirely when the year has no cells.
function analyticsRenderHeatmap(panel, resp) {
  const host = panel.querySelector('#an-heat');
  const card = panel.querySelector('.an-heat-card');
  hideTlTip(); // the innerHTML reset below drops the hovered cell
  if (!host || !card) return;
  const heat = (resp && resp.heatmap) || {};
  const cells = heat.cells || [];
  if (!cells.length) {
    card.hidden = true;
    return;
  }
  const { weeks, max } = analyticsYearGrid(cells, heat.from, heat.to);
  card.hidden = false;
  const title = panel.querySelector('#an-heat-title');
  if (title) title.textContent = 'Token Usage';
  // Square edge from the MEASURED available width (the card is unhidden, so
  // the scroll host has a real clientWidth) — explicit tracks and sizes,
  // no engine-dependent aspect-ratio-in-grid sizing.
  const cell = analyticsHeatCellSize(host.clientWidth, weeks.length);
  const template = `grid-template-columns:2.6em repeat(${weeks.length}, ${cell}px)`;
  let html = `<div class="an-heat an-heat-year" style="--hm:${cell}px" role="img" aria-label="token usage over the past year">`;
  html += `<div class="an-heat-mon" style="${template}"><div class="hm-corner"></div>`;
  for (const s of analyticsYearMonthSpans(weeks)) {
    html += `<div class="hm-mon" style="grid-column:${s.col + 2} / span ${s.span}">${esc(s.label)}</div>`;
  }
  html += `</div><div class="an-heat-days" style="${template}">`;
  // Row labels alternate Mon/Wed/Fri only — one per two rows keeps the
  // gutter narrow without losing week orientation. Every in-window cell
  // carries its day (data-day; data-cell for traffic) to back the hover
  // tooltip — out-of-window slots stay anonymous.
  for (let wd = 0; wd < 7; wd++) {
    html += `<div class="hm-day">${wd % 2 === 0 ? HEAT_DAYS[wd] : ''}</div>`;
    for (const w of weeks) {
      const c = w.days[wd];
      if (c === false) {
        html += '<div class="hm hm-off"></div>';
        continue;
      }
      const [y, m, d] = w.lead.split('-').map(Number);
      const dayUnix = Math.floor(new Date(y, m - 1, d + wd).getTime() / 1000);
      const lvl = analyticsHeatLevel(c ? Number(c.tokens || 0) : 0, max);
      html += `<div class="hm hm-l${lvl}" data-day="${dayUnix}"${c ? ` data-cell="${esc(JSON.stringify(c))}"` : ''}></div>`;
    }
  }
  html += '</div></div>';
  host.innerHTML = html;
  // Hover tooltip via delegation (hundreds of cells × per-cell listeners
  // would be noise): the 90ms settle delay mirrors the session-timeline
  // bars, and leaving the grid or the shared hide paths (scroll,
  // pointerdown, re-render) close it.
  const grid = host.querySelector('.an-heat-days');
  if (grid) {
    let last = null;
    grid.addEventListener('mouseover', (e) => {
      const cell = e.target.closest('.hm');
      if (!cell || !cell.dataset.day || cell === last) return;
      last = cell;
      if (tlTipTimer) clearTimeout(tlTipTimer);
      tlTipTimer = setTimeout(() => {
        tlTipTimer = 0;
        tlTipPos = { x: e.clientX, y: e.clientY };
        showHeatTip(cell);
      }, 90);
    });
    grid.addEventListener('mouseleave', () => { last = null; hideTlTip(); });
  }
}

// analyticsSortState reads the leaderboard's pinned sort ({col, dir}; col
// null = follow the active chart metric — the default) from localStorage.
// Unknown columns or malformed values fall back to unpinned, so a stale key
// from an older markup never breaks rendering.
function analyticsSortState() {
  try {
    const raw = localStorage.getItem('an-sort');
    if (raw) {
      const v = JSON.parse(raw);
      if (v && typeof v === 'object') {
        const col = v.col && ANALYTICS_TABLE_SORT[v.col] ? v.col : null;
        const dir = col && v.dir === 'asc' ? 'asc' : 'desc';
        return { col, dir };
      }
    }
  } catch (_) { /* ignore */ }
  return { col: null, dir: 'desc' };
}

// analyticsRenderTable renders the leaderboard: one row per series (provider/
// model, or agent/model in the agent dimension) with the window's requests,
// tokens, failure rate, average latency, equivalent cost, blended $/1M tokens
// and a cost share. Rows order by the pinned column sort (header click) or,
// unpinned, the active chart metric — table and chart tell the same story
// until the operator picks a column. Clicking a row drills into the Requests
// tab seeded with that series' filters (by=model → provider+model; agent
// rows add the agent) through the hash pipeline, which applies the filter
// before the tab renders.
function analyticsRenderTable(panel, resp, metricId) {
  const host = panel.querySelector('#an-table');
  if (!host) return;
  const rows = analyticsTableRows(resp && resp.series);
  // v2: identical table shape to Status→Dashboard — health status badge,
  // ttft column, graded latency/ttft/tok-s cells, $/1M tok as a cost-cell
  // hover note, cost share as a plain number. The two views differ only in
  // their window (Analytics = selected range, Dashboard = fixed 1h).
  const health = new Map(modelHealthFromSeries(resp && resp.series).map((r) => [r.label, r]));
  const totalCost = rows.reduce((sum, r) => sum + (r.cost || 0), 0);
  // Join the sort-only fields onto each row (analyticsSortRows reads them):
  // the health score keys by the same label as the badge join, and the cost
  // share is cost over the window's priced total.
  for (const r of rows) {
    r.healthScore = (health.get(r.label) || {}).score ?? null;
    r.costShare = r.cost != null && totalCost > 0 ? r.cost / totalCost : null;
  }
  const sort = analyticsSortState();
  const ordered = analyticsSortRows(rows, metricId, sort);
  const gradeBadge = (g) => g == null ? '<span class="badge muted">n/a</span>'
    : `<span class="badge ${g}">${g === 'ok' ? 'healthy' : g === 'warn' ? 'degraded' : 'poor'}</span>`;
  const graded = (dim, text) => `<td class="num${dim && dim.grade ? ' ' + dim.grade : ''}">${text}</td>`;
  const body = ordered.map((r) => {
    const h = health.get(r.label) || {};
    const dims = h.dims || {};
    const share = r.costShare != null ? r.costShare * 100 : null;
    const costTip = r.costPerMTok == null ? '' : ` title="$${r.costPerMTok.toFixed(2)} per 1M tokens"`;
    const shareTip = share == null ? '' : ` title="${share.toFixed(1)}% of priced cost"`;
    const drill = '#requests?' + requestsFilterQuery({ provider: r.provider, model: r.model, agent: r.agent });
    const errTip = r.errPct == null ? '' : ` title="${esc(`${fmtNum(r.failures)} failures · ${fmtNum(r.failovers)} failover attempts · ${fmtNum(r.rateLimited)} rate-limited (429)`)}"`;
    return `<tr data-drill="${esc(drill)}" title="view requests · ${esc(r.label)}">
      <td class="mono">${esc(r.label)}</td>
      <td>${gradeBadge(h.grade)}</td>
      <td class="num">${fmtNum(r.requests)}</td>
      <td class="num" title="${esc(fmtNum(r.tokens))} tokens">${fmtCompact(r.tokens)}</td>
      <td class="num"${errTip}>${r.errPct == null ? '—' : r.errPct.toFixed(1) + '%'}</td>
      ${graded(dims.latency, r.latencyMs == null ? '—' : fmtNum(Math.round(r.latencyMs)) + 'ms')}
      ${graded(dims.ttft, r.ttftMs == null ? '—' : fmtNum(Math.round(r.ttftMs)) + 'ms')}
      ${graded(dims.toksec, r.tokSec == null ? '—' : r.tokSec.toFixed(1))}
      <td class="num"${costTip}>${r.cost == null ? 'n/a' : '$' + r.cost.toFixed(4)}</td>
      <td class="num"${shareTip}>${share == null ? '—' : share.toFixed(1) + '%'}</td>
    </tr>`;
  }).join('');
  // Every column is sortable: the header carries the column id, the active
  // pin shows its direction arrow (and aria-sort), first click uses the
  // column's default direction, the next click toggles.
  const th = (col, label, cls = '') => {
    const active = sort.col === col;
    const arrow = active ? (sort.dir === 'asc' ? ' ▲' : ' ▼') : '';
    const aria = active ? ` aria-sort="${sort.dir === 'asc' ? 'ascending' : 'descending'}"` : '';
    return `<th${cls ? ` class="${cls}"` : ''}${aria} data-sort="${col}" title="sort by ${esc(label.toLowerCase())}">${label}${arrow}</th>`;
  };
  host.innerHTML = `<table class="table">
      <thead><tr>${th('series', 'Series')}${th('status', 'Status')}${th('requests', 'Requests', 'num')}${th('tokens', 'Tokens', 'num')}${th('err', 'Err', 'num')}${th('latency', 'Avg Lat', 'num')}${th('ttft', 'TTFT', 'num')}${th('toksec', 'Tok/s', 'num')}${th('cost', 'Cost', 'num')}${th('share', 'Cost Share', 'num')}</tr></thead>
      <tbody>${body || '<tr><td colspan="10" class="hint">No series in range</td></tr>'}</tbody>
    </table>`;
  // Header click: pin the column (default direction) or toggle the pinned
  // one; re-render ONLY this table from the same response — sorting is
  // client-side, no refetch.
  host.querySelector('thead').onclick = (e) => {
    const cell = e.target && e.target.closest ? e.target.closest('th[data-sort]') : null;
    if (!cell) return;
    const col = cell.dataset.sort;
    const next = sort.col === col
      ? { col, dir: sort.dir === 'asc' ? 'desc' : 'asc' }
      : { col, dir: ANALYTICS_TABLE_SORT[col] || 'desc' };
    analyticsSave('sort', next);
    analyticsRenderTable(panel, resp, metricId);
  };
  // Row click → Requests tab with this series' filters. The hash navigation
  // drives the full pipeline (seed filter → activate tab), and a history
  // entry lands so Back returns to the Analytics view.
  host.querySelectorAll('tr[data-drill]').forEach((tr) => {
    tr.onclick = () => { location.hash = tr.dataset.drill; };
  });
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
  const { tab: bootTab, sub: bootSub, query: bootQuery } = parseHash();
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
  } else if (bootTab === 'requests') {
    activateTabSilent('requests');
  } else if (bootTab === 'security') {
    activateTabSilent('security');
  } else {
    activateTabSilent('status');
  }
  // #status/live?session=… — stash as pending instead of applying here: the
  // status tab renders ASYNC (5 fetches), so applying now races the Live
  // card mount, whose reset would wipe the selection. renderLiveCard
  // consumes the pin right after mounting (the deterministic point).
  if (bootTab === 'status' && bootSub === 'live' && bootQuery.session && statusSelected === 'live') {
    bootLiveSession = bootQuery.session;
    if (document.getElementById('live-session')) {
      // Defensive: the live card is already mounted (not the normal boot
      // path) — consume the pin now instead of waiting for a mount.
      const v = bootLiveSession;
      bootLiveSession = '';
      onLiveSessionChange(v);
    }
  }
  // Update the header connection indicator regardless of the landing tab,
  // then keep it ticking on every tab (see maybeConnRefresh).
  refreshConnIndicator();
  maybeConnRefresh();
}

boot();
