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
}

for (const b of tabBtns) {
  b.addEventListener('click', () => activateTab(b.dataset.tab));
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

function stopStatusRefresh() {
  if (statusTimer) { clearInterval(statusTimer); statusTimer = null; }
}

// renderStatusTab fetches the dashboard snapshot (status + tokens + logs) and
// re-renders the Status panel. Auto-refreshes every 5s while the Status tab is
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
    panels.status.innerHTML = '';
    renderProvidersCard(st);
    renderScheduleCard(st);
    renderQuotaCard(st);
    renderTokensCard(tok.usage || []);
    renderLogsCard(logs.lines || []);
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

// buildCard wraps a title + body in the .card/.card-head/.card-body shell.
function buildCard(title, meta, bodyHTML, extraBodyClass = '') {
  return `<section class="card">
    <header class="card-head"><h2>${esc(title)}</h2>${meta ? `<span class="meta">${esc(meta)}</span>` : ''}</header>
    <div class="card-body ${extraBodyClass}">${bodyHTML}</div>
  </section>`;
}

// healthPill renders a status pill reflecting circuit + rate-limit state.
function healthPill(h) {
  if (!h) return `<span class="pill muted"><span class="dot"></span>unknown</span>`;
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

function renderProvidersCard(st) {
  const health = st.health || {};
  const counters = st.counters || {};
  const names = Object.keys(health).sort();
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
  panels.status.insertAdjacentHTML('beforeend', html);
}

// renderScheduleCard builds the per-route schedule view: each route shows its
// ordered provider chain with the first choice highlighted, sticky marker, and
// pool summary.
function renderScheduleCard(st) {
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
  panels.status.insertAdjacentHTML('beforeend', html);
}

// renderQuotaCard draws per-provider quota bars (ultimate window + short windows).
function renderQuotaCard(st) {
  const quota = st.quota || {};
  const names = Object.keys(quota).sort();
  if (names.length === 0) return;
  let rows = '';
  for (const name of names) {
    const snap = quota[name];
    if (!snap || snap.Err) {
      rows += `<div class="bar-row"><div class="bar-label">
        <span class="name">${esc(name)}</span>
        <span class="pct">${snap && snap.Err ? esc(snap.Err) : 'no data'}</span>
      </div></div>`;
      continue;
    }
    const windows = snap.Windows || [];
    let subBlock = '';
    for (const w of windows) {
      const p = (w.RemainingPct != null && w.RemainingPct >= 0) ? w.RemainingPct : null;
      const fillCls = p == null ? '' : (p > 0.3 ? 'ok' : (p > 0.1 ? 'warn' : 'err'));
      const ulg = w.Ultimate ? ' · ultimate' : (w.Short ? ' · short' : '');
      const reset = w.ResetsAt ? `resets ${esc(fmtTime(w.ResetsAt))}` : '';
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
  panels.status.insertAdjacentHTML('beforeend', html);
}

// renderTokensCard draws the per-(provider, model) token usage table.
function renderTokensCard(usage) {
  if (!usage || usage.length === 0) {
    const html = buildCard('Token usage', '0',
      `<div class="empty-state">No observed usage yet. Counts accrue as the proxy streams SSE responses.</div>`);
    panels.status.insertAdjacentHTML('beforeend', html);
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
  panels.status.insertAdjacentHTML('beforeend', html);
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

// renderLogsCard renders the tail of the daemon log.
function renderLogsCard(lines) {
  let body;
  if (!lines || lines.length === 0) {
    body = `<pre class="log-pre"><span class="log-empty">log is empty or unavailable</span></pre>`;
  } else {
    const rendered = lines.map((l) => `<span class="log-line">${esc(l)}</span>`).join('\n');
    body = `<pre class="log-pre">${rendered}</pre>`;
  }
  const html = buildCard('Logs', lines ? `${lines.length} lines` : '', body, 'flush');
  panels.status.insertAdjacentHTML('beforeend', html);
}

// ===========================================================================
// CONFIG TAB
// ===========================================================================

let configCache = null; // last /api/config response {yaml, summary}

async function renderConfigTab() {
  const panel = panels.config;
  panel.innerHTML =
    `<div id="config-summary" class="card"><div class="card-body"><span class="msg">loading…</span></div></div>
     <details class="editor" id="ed-general"><summary>General</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-scheduling"><summary>Scheduling</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-provider"><summary>Provider scalars</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-route"><summary>Routes</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <details class="editor" id="ed-claude"><summary>Claude mapping</summary><div class="editor-body"><span class="msg">loading…</span></div></details>
     <div class="card">
       <header class="card-head"><h2>Raw YAML</h2><span class="meta" id="yaml-meta"></span></header>
       <div class="card-body">
         <textarea class="yaml" id="yaml-editor" spellcheck="false" autocomplete="off"></textarea>
         <div class="row-actions" style="margin-top: 10px;">
           <span class="spacer"></span>
           <button class="btn small" id="btn-yaml-reload">Reload from disk</button>
           <button class="btn primary small" id="btn-yaml-save">Save &amp; reload</button>
         </div>
         <div id="yaml-msg"></div>
       </div>
     </div>`;
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
  if (ta) ta.value = cfg.yaml || '';
  const meta = document.getElementById('yaml-meta');
  if (meta) meta.textContent = `${(cfg.yaml || '').length} bytes`;

  // General form
  buildForm('ed-general', 'General', [
    { name: 'listen',     label: 'listen address', hint: 'e.g. 127.0.0.1:8787' },
    { name: 'log_level',  label: 'log level',      hint: 'debug / info / warn / error' },
    { name: 'log_file',   label: 'log file',       hint: 'path to daemon log (optional)' },
  ], 'general');

  // Scheduling form
  buildForm('ed-scheduling', 'Scheduling', [
    { name: 'circuit_threshold',     label: 'circuit threshold',     hint: 'failures to open (default 3)' },
    { name: 'circuit_cooldown',      label: 'circuit cooldown',      hint: 'Go duration, e.g. 10m' },
    { name: 'rate_limit_backoff',    label: 'rate-limit backoff',    hint: 'default Retry-After, e.g. 60s' },
    { name: 'upstream_timeout',       label: 'upstream timeout',      hint: 'e.g. 5m' },
    { name: 'sticky_dwell',          label: 'sticky dwell',          hint: 'e.g. 10m' },
    { name: 'quota_poll_interval',   label: 'quota poll interval',   hint: 'e.g. 5m' },
    { name: 'quota_switch_margin',   label: 'quota switch margin',   hint: 'points, e.g. 15' },
  ], 'scheduling');

  // Provider scalars form
  buildProviderForm('ed-provider');

  // Route form
  buildRouteForm('ed-route');

  // Claude mapping form
  buildClaudeForm('ed-claude');
}

// buildForm renders a flat key→scalar form bound to a kind (general/scheduling).
function buildForm(editorId, title, fields, kind) {
  const ed = document.getElementById(editorId);
  if (!ed) return;
  let body = `<div class="section-title">${esc(title)}</div>`;
  for (const f of fields) {
    body += `<div class="field">
      <label for="fld-${esc(f.name)}">${esc(f.label)}</label>
      <input id="fld-${esc(f.name)}" name="${esc(f.name)}" type="text" autocomplete="off">
      ${f.hint ? `<span class="hint">${esc(f.hint)}</span>` : ''}
    </div>`;
  }
  body += `<div class="row-actions">
    <span class="spacer"></span>
    <button class="btn primary small" data-save="${esc(kind)}">Apply</button>
  </div>
  <div class="msg" data-msg="${esc(kind)}"></div>`;
  ed.querySelector('.editor-body').innerHTML = body;
  const btn = ed.querySelector(`[data-save="${kind}"]`);
  btn.addEventListener('click', () => applyScalarForm(ed, kind, fields));
}

async function applyScalarForm(ed, kind, fields) {
  const msg = ed.querySelector(`[data-msg="${kind}"]`);
  const btn = ed.querySelector(`[data-save="${kind}"]`);
  const data = {};
  for (const f of fields) {
    const inp = ed.querySelector(`[name="${f.name}"]`);
    const v = inp ? inp.value.trim() : '';
    if (v) data[f.name] = v;
  }
  if (Object.keys(data).length === 0) {
    showMsg(msg, 'err', 'no fields filled');
    return;
  }
  btn.disabled = true;
  showMsg(msg, 'ok', 'saving…');
  try {
    await apiPost('/api/config/edit', { kind, data });
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
     <div class="field"><label for="fld-openai_base_url">openai_base_url</label><input id="fld-openai_base_url" name="openai_base_url" type="text"></div>
     <div class="field"><label for="fld-anthropic_base_url">anthropic_base_url</label><input id="fld-anthropic_base_url" name="anthropic_base_url" type="text" placeholder="optional override for /v1/messages"></div>
     <div class="field"><label for="fld-usage_url">usage_url</label><input id="fld-usage_url" name="usage_url" type="text"></div>
     <div class="field"><label for="fld-billing">billing</label>
       <select id="fld-billing" name="billing">
         <option value="plan">plan</option>
         <option value="pay-as-you-go">pay-as-you-go</option>
       </select>
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

async function populateProviderDatalist() {
  try {
    const [st, acc] = await Promise.all([
      apiGet('/api/status').catch(() => null),
      apiGet('/api/accounts').catch(() => null),
    ]);
    const names = new Set();
    if (st && st.health) Object.keys(st.health).forEach((n) => names.add(n));
    if (acc && acc.providers) acc.providers.forEach((p) => names.add(p.name));
    const dl = document.getElementById('prov-list');
    if (dl) dl.innerHTML = Array.from(names).sort().map((n) => `<option value="${esc(n)}">`).join('');
  } catch (_) { /* best-effort */ }
}

async function applyProviderEdit() {
  const msg = document.getElementById('prov-msg');
  const btn = document.getElementById('btn-prov-save');
  const name = (document.getElementById('prov-name').value || '').trim();
  if (!name) { showMsg(msg, 'err', 'provider name required'); return; }
  const data = {};
  for (const fld of ['openai_base_url', 'anthropic_base_url', 'usage_url', 'billing']) {
    const inp = document.getElementById('fld-' + fld);
    const v = (inp ? inp.value : '').trim();
    if (v) data[fld] = v;
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

// buildRouteForm: name + targets (newline- or comma-separated) → replace sequence.
function buildRouteForm(editorId) {
  const ed = document.getElementById(editorId);
  if (!ed) return;
  ed.querySelector('.editor-body').innerHTML =
    `<div class="section-title">Routes</div>
     <div class="field"><label for="route-name">exposed model name</label><input id="route-name" name="name" type="text" placeholder="e.g. sonnet"></div>
     <div class="field"><label for="route-targets">targets</label>
       <textarea id="route-targets" name="targets" rows="3" placeholder="provider/model, one per line (priority ascending)&#10;e.g. zhipu/glm-4.6&#10;deepseek/deepseek-chat"></textarea>
       <span class="hint">Each non-empty line is one target: &lt;provider&gt;/&lt;model&gt;[:priority]. Order = failover priority.</span>
     </div>
     <div class="row-actions">
       <button class="btn danger small" id="btn-route-delete">Delete route</button>
       <span class="spacer"></span>
       <button class="btn primary small" id="btn-route-save">Apply</button>
     </div>
     <div class="msg" id="route-msg"></div>`;
  document.getElementById('btn-route-save').addEventListener('click', applyRouteEdit);
  document.getElementById('btn-route-delete').addEventListener('click', deleteRoute);
}

function parseRouteTargets(raw) {
  // Returns [{provider, model, priority?}] or throws on a malformed line.
  const lines = raw.split(/\r?\n/).map((l) => l.trim()).filter((l) => l && !l.startsWith('#'));
  // Also support comma-separated single-line input.
  const flat = lines.length === 1 && lines[0].includes(',')
    ? lines[0].split(',').map((s) => s.trim()).filter(Boolean)
    : lines;
  const out = [];
  for (const line of flat) {
    const m = line.match(/^([^\/\s]+)\/([^\s:]+)(?::(\d+))?$/);
    if (!m) throw new Error(`bad target "${line}" — expected provider/model[:priority]`);
    const t = { provider: m[1], model: m[2] };
    if (m[3]) t.priority = parseInt(m[3], 10);
    out.push(t);
  }
  if (out.length === 0) throw new Error('no targets given');
  return out;
}

async function applyRouteEdit() {
  const msg = document.getElementById('route-msg');
  const btn = document.getElementById('btn-route-save');
  const name = (document.getElementById('route-name').value || '').trim();
  const raw = document.getElementById('route-targets').value;
  if (!name) { showMsg(msg, 'err', 'route name required'); return; }
  let targets;
  try { targets = parseRouteTargets(raw); }
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

// buildClaudeForm: alias + route → set; delete by alias.
function buildClaudeForm(editorId) {
  const ed = document.getElementById(editorId);
  if (!ed) return;
  ed.querySelector('.editor-body').innerHTML =
    `<div class="section-title">Claude mapping</div>
     <div class="field"><label for="cm-alias">anthropic alias</label><input id="cm-alias" name="alias" type="text" placeholder="e.g. claude-sonnet-4-5"></div>
     <div class="field"><label for="cm-route">exposed route</label><input id="cm-route" name="route" type="text" placeholder="exposed model name from routes"></div>
     <div class="row-actions">
       <button class="btn danger small" id="btn-cm-delete">Delete alias</button>
       <span class="spacer"></span>
       <button class="btn primary small" id="btn-cm-save">Apply</button>
     </div>
     <div class="msg" id="cm-msg"></div>`;
  document.getElementById('btn-cm-save').addEventListener('click', applyClaudeMap);
  document.getElementById('btn-cm-delete').addEventListener('click', deleteClaudeMap);
}

async function applyClaudeMap() {
  const msg = document.getElementById('cm-msg');
  const btn = document.getElementById('btn-cm-save');
  const alias = (document.getElementById('cm-alias').value || '').trim();
  const route = (document.getElementById('cm-route').value || '').trim();
  if (!alias || !route) { showMsg(msg, 'err', 'alias and route required'); return; }
  btn.disabled = true;
  showMsg(msg, 'ok', 'saving…');
  try {
    await apiPost('/api/config/edit', { kind: 'claude_mapping', data: { alias, route } });
    await loadConfigAll();
    showMsg(msg, 'ok', 'saved & reloaded');
  } catch (e) {
    showMsg(msg, 'err', e.message);
  } finally {
    btn.disabled = false;
  }
}

async function deleteClaudeMap() {
  const msg = document.getElementById('cm-msg');
  const alias = (document.getElementById('cm-alias').value || '').trim();
  if (!alias) { showMsg(msg, 'err', 'alias required'); return; }
  if (!window.confirm(`Delete claude_mapping alias "${alias}"?`)) return;
  try {
    await apiPost('/api/config/edit', { kind: 'claude_mapping', data: { alias, delete: true } });
    await loadConfigAll();
    showMsg(msg, 'ok', `deleted ${alias}`);
  } catch (e) {
    showMsg(msg, 'err', e.message);
  }
}

async function loadConfigYAML() {
  const ta = document.getElementById('yaml-editor');
  const meta = document.getElementById('yaml-meta');
  const msg = document.getElementById('yaml-msg');
  try {
    const cfg = await apiGet('/api/config');
    configCache = cfg;
    if (ta) ta.value = cfg.yaml || '';
    if (meta) meta.textContent = `${(cfg.yaml || '').length} bytes`;
    if (msg) showMsg(msg, 'ok', 'reloaded from disk');
  } catch (e) {
    if (msg) showMsg(msg, 'err', e.message);
  }
}

async function saveConfigYAML() {
  const ta = document.getElementById('yaml-editor');
  const msg = document.getElementById('yaml-msg');
  const btn = document.getElementById('btn-yaml-save');
  if (!ta) return;
  btn.disabled = true;
  showMsg(msg, 'ok', 'validating + reloading…');
  try {
    await apiPost('/api/config', { yaml: ta.value });
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

let accountsCache = null;

async function renderAccountsTab() {
  const panel = panels.accounts;
  panel.innerHTML = `<div id="acc-msg"></div><div class="grid cols-2" id="acc-grid"><div class="card-body"><span class="msg">loading…</span></div></div>`;
  try {
    const acc = await apiGet('/api/accounts');
    accountsCache = acc;
    renderAccountsGrid(acc.providers || []);
  } catch (e) {
    setConn('err');
    showMsg(document.getElementById('acc-msg'), 'err', e.message);
  }
}

function renderAccountsGrid(providers) {
  const grid = document.getElementById('acc-grid');
  if (!grid) return;
  const sorted = providers.slice().sort((a, b) => (a.name || '').localeCompare(b.name || ''));
  if (sorted.length === 0) {
    grid.innerHTML = `<div class="card"><div class="empty-state">No providers configured. Add one in <code>config.yaml</code> first.</div></div>`;
    return;
  }
  grid.innerHTML = sorted.map(providerAccountsCard).join('');
  // Wire Add buttons.
  for (const p of sorted) {
    const add = document.getElementById(`add-${cssEscape(p.name)}`);
    if (add) add.addEventListener('click', () => openAddFor(p));
  }
  // Wire Remove buttons.
  document.querySelectorAll('[data-remove]').forEach((b) => {
    b.addEventListener('click', () => removeAccount(b.dataset.provider, b.dataset.remove, b.dataset.label));
  });
}

// cssEscape is a tiny id sanitizer (provider names are config keys: usually
// [a-z0-9_-]+). We use this to build unique button ids without depending on
// the CSS.escape API.
function cssEscape(s) {
  return String(s).replace(/[^a-zA-Z0-9_-]/g, '_');
}

function providerAccountsCard(p) {
  const id = cssEscape(p.name);
  const isOauth = p.provider_id === 'aqp' || p.provider_id === 'codex';
  let rows = '';
  if (!p.accounts || p.accounts.length === 0) {
    rows = `<div class="empty-state">No account configured. Click <strong>Add</strong> to sign in.</div>`;
  } else {
    for (const a of p.accounts) {
      const label = a.label || a.id;
      const sub = `id: ${a.id || '—'}`;
      const added = a.added_at ? ` · added ${esc(fmtTime(a.added_at))}` : '';
      const mail = a.email ? `<div class="acct-mail">${esc(a.email)}</div>` : '';
      rows += `<div class="account-row">
        <div>
          <div class="acct-label">${esc(label)}</div>
          <div class="acct-sub">${esc(sub)}${added}</div>
          ${mail}
        </div>
        <div class="row-actions">
          <button class="btn danger small" data-remove="${esc(a.id)}"
                  data-provider="${esc(p.name)}" data-label="${esc(label)}">Remove</button>
        </div>
      </div>`;
    }
  }
  const addKind = isOauth ? 'oauth' : 'apikey';
  return `<section class="card">
    <header class="card-head">
      <h2>${esc(p.name)}</h2>
      <span class="meta">${esc(p.provider_id || '?')}${p.billing ? ' · ' + esc(p.billing) : ''}</span>
    </header>
    <div class="card-body flush">
      ${rows}
      <div class="row-actions" style="padding: 10px 14px;">
        <span class="spacer"></span>
        <button class="btn small" id="add-${esc(id)}" data-add-kind="${addKind}" data-provider="${esc(p.name)}" data-provider-id="${esc(p.provider_id || '')}">Add account</button>
      </div>
    </div>
  </section>`;
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

// Initial render: status tab is active by default.
renderStatusTab();
