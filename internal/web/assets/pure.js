// pure.js — DOM-free helpers extracted from app.js so they can be unit-tested
// with `node --test` (jstests/pure.test.mjs). Every function here must stay
// deterministic: no document/window access, no locale/timezone-dependent
// formatting (those stay in app.js). app.js imports this module from the same
// /ui/ directory the SPA is served from.

// esc escapes a value for safe interpolation into an HTML text or attribute
// context. Covers the five chars that matter (& < > " '). Used on EVERY
// interpolated value — provider names, labels, log lines, error messages, YAML.
export function esc(s) {
  if (s === null || s === undefined) return '';
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

export function fmtNum(n) {
  if (n === null || n === undefined) return '0';
  return Number(n).toLocaleString('en-US');
}

// avgLatencyMs derives the per-request average latency (ms) from the cumulative
// counters a provider carries: latency_ms_sum / requests (0 when no requests).
// Rounded — latencies are observability, not billing.
export function avgLatencyMs(c) {
  if (!c) return 0;
  const req = Number(c.requests || 0);
  if (!req) return 0;
  return Math.round(Number(c.latency_ms_sum || 0) / req);
}

// hasReset reports whether s is a real reset time. Go's zero time.Time
// serializes as "0001-01-01T00:00:00Z" - a truthy string, so a bare truthiness
// check would render a bogus "resets …" for windows that never set ResetsAt
// (e.g. deepseek's pay-as-you-go balance, which only has remaining quota).
// Treat null/empty/invalid/year-1 (the zero sentinel) as "no reset".
export function hasReset(s) {
  if (!s) return false;
  const d = new Date(s);
  if (isNaN(d.getTime())) return false;
  return d.getUTCFullYear() > 1;
}

// fmtDur formats seconds as "Xd Yh" / "Yh Zm" / "Zm Ws", trimmed.
export function fmtDur(sec) {
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
export function untilHuman(ts, now = Date.now()) {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '';
  const ms = d.getTime() - now;
  if (ms <= 0) return '';
  return ` (in ${fmtDur(ms / 1000)})`;
}

// providerFrozen reports whether a /api/status health entry counts as frozen
// for the Providers card: an operator freeze (h.frozen), an open/half-open
// circuit, or an active rate-limit cooldown. nowMs is injected for
// determinism. A missing entry (healthy provider, health is created lazily on
// first failure/freeze) is not frozen.
export function providerFrozen(h, nowMs = Date.now()) {
  if (!h) return false;
  if (h.frozen) return true;
  if (h.circuit_state === 'open' || h.circuit_state === 'half_open') return true;
  const rlUntil = h.rate_limited_until ? new Date(h.rate_limited_until).getTime() : 0;
  return rlUntil > nowMs;
}

// providerNames computes the row set of the Providers card: the union of the
// schedule preview's ordered provider names and the health map's keys, sorted.
// The schedule alone is NOT sufficient: scheduling drops unavailable targets
// (operator-frozen, circuit-open, rate-limit cooldown, quota-exhausted) from
// every route's ordered chain, so a frozen provider would vanish from the card
// — row, pill, and unfreeze button — exactly when the operator needs to see
// and unfreeze it. Health entries are created lazily on failure/freeze, so
// every unavailable provider is guaranteed a health key to union in.
export function providerNames(models, health) {
  const names = new Set();
  for (const route of Object.keys(models || {})) {
    for (const p of (((models || {})[route] || {}).ordered || [])) {
      if (p && p.provider) names.add(p.provider);
    }
  }
  for (const name of Object.keys(health || {})) names.add(name);
  return Array.from(names).sort();
}

// quotaErrKind classifies a quota snapshot error into a short token for UI
// presentation. Exported so app.js and accountUsageState share one definition.
export function quotaErrKind(snap) {
  if (!snap || !snap.Err) return '';
  const e = snap.Err.toLowerCase();
  if (e.includes('session expired')) return 'session-expired';
  if (e.includes('not logged in')) return 'not-logged-in';
  return 'error';
}

// accountUsageState decides the collapsed summary hint and default open state
// for an account's Usage <details> section. The logic is pure and DOM-free so
// it can be unit-tested; app.js calls it and renders the resulting attributes.
//
// Branches:
//   - snap == null          -> hint 'no data',     collapsed
//   - snap.Err              -> hint by error kind, open (login/error visible)
//   - empty Windows, no Err -> hint 'unmeasured',  collapsed
//   - non-empty Windows     -> hint by window/plan, open
export function accountUsageState(snap) {
  if (!snap) {
    return { hint: 'No data', open: false };
  }
  if (snap.Err) {
    const k = quotaErrKind(snap);
    const hint = k === 'session-expired' ? 'Session expired'
      : k === 'not-logged-in' ? 'Not logged in' : 'Error';
    return { hint, open: true };
  }
  const windows = snap.Windows || [];
  if (windows.length === 0) {
    return { hint: 'Unmeasured', open: false };
  }
  const ult = windows.find((w) => w.Ultimate);
  if (ult && ult.RemainingPct != null && ult.RemainingPct >= 0) {
    return { hint: (ult.RemainingPct * 100).toFixed(1) + '% left', open: true };
  }
  if (snap.Plan) {
    return { hint: snap.Plan, open: true };
  }
  return { hint: 'Available', open: true };
}

// settingsDiff computes the minimal POST /api/config/edit payload for the
// Config tab's settings form. `loaded` is the GET /api/config `settings`
// projection (nested by kind), `current` is the form's readback in the same
// shape. Only changed fields are returned, grouped by edit kind; a field the
// user cleared (empty string / null) becomes an explicit null, which the
// backend deletes so the code default applies again. Comparison is on the
// string form of the value (form inputs are strings, JSON gives numbers /
// booleans / null), so an untouched field never produces a write.
export function settingsDiff(loaded, current) {
  const norm = (v) => {
    if (v === null || v === undefined) return '';
    if (typeof v === 'boolean') return v ? 'true' : 'false';
    return String(v);
  };
  const out = {};
  for (const kind of Object.keys(current || {})) {
    const edited = current[kind] || {};
    const was = (loaded && loaded[kind]) || {};
    const changed = {};
    for (const key of Object.keys(edited)) {
      const now = edited[key];
      if (norm(now) === norm(was[key])) continue;
      changed[key] = (now === '' || now === null || now === undefined) ? null : now;
    }
    if (Object.keys(changed).length) out[kind] = changed;
  }
  return out;
}

// settingsRestartKeys lists the changed dotted keys a hot reload does NOT
// apply, for the settings form's post-save warning. A group-level `restart`
// marks every changed field of that block (request_log, stats); a field-level
// `restart` marks one key inside an otherwise hot-reloadable block
// (log_level/log_file, scheduling.quota_poll_interval).
export function settingsRestartKeys(diff, groups) {
  const out = [];
  for (const group of groups || []) {
    const changed = diff && diff[group.kind];
    if (!changed) continue;
    for (const field of group.fields || []) {
      if ((group.restart || field.restart) && Object.prototype.hasOwnProperty.call(changed, field.key)) {
        out.push(`${group.kind}.${field.key}`);
      }
    }
  }
  return out;
}

// YAML_EDITOR_MIN_HEIGHT is the hard floor for the Raw YAML editor: below it
// the Config card would collapse into an unusable strip, so short viewports
// scroll the page instead.
export const YAML_EDITOR_MIN_HEIGHT = 480;

// visibleYamlEditorHeight computes the editor's pixel height from the viewport:
// the remaining space, but never under the hard minimum.
export function visibleYamlEditorHeight(viewportHeight, editorTop, spaceBelow) {
  return Math.max(YAML_EDITOR_MIN_HEIGHT,
    Math.floor(viewportHeight - Math.max(0, editorTop) - spaceBelow),
  );
}

// verdictBadge maps one startup-probe verdict string ("yes"/"no"/"unknown",
// GET /api/models) to its badge presentation: pill class + glyph + label.
// unknown is deliberately distinct from no — no is a concluded negative (or
// unsupported by definition, e.g. anthropic without anthropic_base_url), while
// unknown means the probe has not concluded and will retry on the next pass.
export function verdictBadge(v) {
  if (v === 'yes') return { cls: 'ok', glyph: '✓', label: 'yes' };
  if (v === 'no') return { cls: 'err', glyph: '✗', label: 'no' };
  return { cls: 'muted', glyph: '?', label: 'unknown' };
}

// modelCapMatrix normalizes the GET /api/models providers map into a
// deterministic view model: providers sorted by name, each provider's models
// sorted by id. Missing fields normalize to empty values (verdict rendering
// falls back to "unknown" via verdictBadge); a provider with no recorded
// models keeps an empty list so the UI can show "probed, no models".
export function modelCapMatrix(providers) {
  const out = [];
  for (const name of Object.keys(providers || {}).sort()) {
    const caps = providers[name] || {};
    const models = [];
    for (const id of Object.keys(caps.models || {}).sort()) {
      const mp = caps.models[id] || {};
      models.push({ id, chat: mp.chat, anthropic: mp.anthropic, responses: mp.responses });
    }
    out.push({ name, fingerprint: caps.fingerprint || '', probedAt: caps.probed_at || '', models });
  }
  return out;
}

// cacheHitRate derives the exact-response cache hit rate (GET /api/status
// cache counters) as a percentage string. "—" when no lookups were recorded
// yet (hits + misses == 0) so a fresh daemon does not render a fake 0.0%.
// Non-numeric inputs normalize to 0, matching fmtNum's tolerance.
export function cacheHitRate(hits, misses) {
  const h = Number(hits || 0);
  const m = Number(misses || 0);
  if (!Number.isFinite(h) || !Number.isFinite(m)) return '—';
  const total = h + m;
  if (total <= 0) return '—';
  return (100 * h / total).toFixed(1) + '%';
}

// providerCapsSummary folds one modelCapMatrix entry into the counts the
// Accounts test-matrix header shows: how many models the startup probe
// recorded, and how many serve each protocol leg (chat/anthropic/responses
// "yes" verdicts — the same data source as the Status Models card, so
// account liveness and protocol capability read side by side). An entry
// without probe data normalizes to zeros.
export function providerCapsSummary(entry) {
  const models = (entry && entry.models) || [];
  const yes = (v) => (v === 'yes' ? 1 : 0);
  let chat = 0;
  let anthropic = 0;
  let responses = 0;
  for (const m of models) {
    chat += yes(m.chat);
    anthropic += yes(m.anthropic);
    responses += yes(m.responses);
  }
  return { models: models.length, chat, anthropic, responses };
}

// ruleHitsLeaderboard folds the Security feed into per-rule hit counts for
// the rule-ops card: every audit record counts once per name it carries, and
// the adjudication ring's LOW verdicts count too — suppressed lows leave no
// audit record, yet they are exactly the benign noise the leaderboard's
// "enable adjudicate" hint is about. Rows come back hits-desc (ties: newer
// last hit, then name asc). adjudicable marks the channels the AI second
// opinion can defer (pattern rules and path categories; the known_secret*
// exact channels never defer — zero false positives by construction).
export function ruleHitsLeaderboard(records, adjudications) {
  const rows = new Map();
  const bump = (name, kind, ts) => {
    if (!name) return;
    const key = kind + '\u0000' + name;
    const row = rows.get(key) || {
      name, kind,
      hits: 0,
      lastTs: 0,
      adjudicable: !name.startsWith('known_secret'),
    };
    row.hits++;
    const t = Number(ts || 0);
    if (t > row.lastTs) row.lastTs = t;
    rows.set(key, row);
  };
  for (const r of records || []) {
    if (!r) continue;
    // The unblock trail is operator action history, not rule hits — counting
    // it here would inflate the hit rates the leaderboard ranks by.
    if (r.kind === 'unblock') continue;
    for (const n of r.names || []) bump(n, r.kind, r.ts);
  }
  for (const a of adjudications || []) {
    if (a && a.verdict === 'low') bump(a.rule, a.kind, a.ts);
  }
  return [...rows.values()].sort((x, y) =>
    y.hits - x.hits || y.lastTs - x.lastTs || x.name.localeCompare(y.name));
}

// TOKEN_RANGES: the preset dimensions of the Status tab's token-usage time
// selector (the 时间维度 pattern: rolling presets, day/month-aligned presets,
// a custom range, and all-time). 'all' is the server default (cumulative
// counters) and sends no query param; every other preset maps to a from/to
// unix-second range computed in LOCAL time — client and daemon are the same
// machine, and the server treats from/to as plain unix seconds
// (docs/web-api.md, GET /api/tokens).
export const TOKEN_RANGES = [
  { value: 'all', label: 'All Time' },
  { value: '1h', label: 'Last 1h' },
  { value: 'today', label: 'Today' },
  { value: 'yesterday', label: 'Yesterday' },
  { value: '7d', label: 'Last 7d' },
  { value: '30d', label: 'Last 30d' },
  { value: 'month', label: 'This Month' },
  { value: 'lastmonth', label: 'Last Month' },
  { value: 'custom', label: 'Custom…' },
];

// tokenRangeBounds resolves a preset to {from, to} unix seconds. now is
// milliseconds, injectable for tests. Rolling presets (1h/7d/30d) end at now;
// day/month presets align to LOCAL midnight / the 1st; the closed presets
// (yesterday, lastmonth) end at 23:59:59 of their last day. 'all' (and any
// unknown value) returns null — no range, the cumulative view.
export function tokenRangeBounds(value, now = Date.now()) {
  const d = new Date(now);
  const sec = (ms) => Math.floor(ms / 1000);
  const midnight = (y, m, day) => new Date(y, m, day).getTime();
  switch (value) {
    case '1h': return { from: sec(now - 3600e3), to: sec(now) };
    case 'today': return { from: sec(midnight(d.getFullYear(), d.getMonth(), d.getDate())), to: sec(now) };
    case 'yesterday': return {
      from: sec(midnight(d.getFullYear(), d.getMonth(), d.getDate() - 1)),
      to: sec(midnight(d.getFullYear(), d.getMonth(), d.getDate())) - 1,
    };
    case '7d': return { from: sec(now - 7 * 86400e3), to: sec(now) };
    case '30d': return { from: sec(now - 30 * 86400e3), to: sec(now) };
    case 'month': return { from: sec(midnight(d.getFullYear(), d.getMonth(), 1)), to: sec(now) };
    case 'lastmonth': return {
      from: sec(midnight(d.getFullYear(), d.getMonth() - 1, 1)),
      to: sec(midnight(d.getFullYear(), d.getMonth(), 1)) - 1,
    };
  }
  return null;
}

// parseLocalDate parses an <input type="date"> value ('YYYY-MM-DD') as a LOCAL
// day — never via new Date(value), which parses date-only strings as UTC and
// would shift the day for non-UTC users. Malformed or impossible dates
// (2026-02-30) return null.
export function parseLocalDate(value) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value || '');
  if (!m) return null;
  const y = Number(m[1]), mo = Number(m[2]), day = Number(m[3]);
  const d = new Date(y, mo - 1, day);
  return d.getFullYear() === y && d.getMonth() === mo - 1 && d.getDate() === day ? d : null;
}

// tokenCustomBounds resolves the custom date inputs to a closed range of
// inclusive full LOCAL days, or null when either date is missing/invalid or
// start > end — the control must never fire a request in that state (the
// server would 400 from > to).
export function tokenCustomBounds(startValue, endValue) {
  const start = parseLocalDate(startValue);
  const end = parseLocalDate(endValue);
  if (!start || !end || start.getTime() > end.getTime()) return null;
  return { from: Math.floor(start.getTime() / 1000), to: Math.floor(end.getTime() / 1000) + 86399 };
}

// tokensRangeQuery builds the /api/tokens query for the selector state
// {preset, customStart, customEnd}: '' = all-time (no params); a from/to
// query for resolvable presets; null = an incomplete/invalid custom range —
// the caller must NOT fetch (it keeps the previous data and shows the hint).
export function tokensRangeQuery(state, now = Date.now()) {
  if (!state || state.preset === 'all') return '';
  if (state.preset === 'custom') {
    const b = tokenCustomBounds(state.customStart, state.customEnd);
    return b ? `?from=${b.from}&to=${b.to}` : null;
  }
  const b = tokenRangeBounds(state.preset, now);
  return b ? `?from=${b.from}&to=${b.to}` : '';
}

// tokenRangeLabel renders the active selection for the usage cards' meta
// line: the preset label, the custom day range, or 'Custom…' while the custom
// inputs are incomplete.
export function tokenRangeLabel(state) {
  if (!state) return TOKEN_RANGES[0].label;
  if (state.preset === 'custom') {
    return tokenCustomBounds(state.customStart, state.customEnd)
      ? `${state.customStart} – ${state.customEnd}`
      : 'Custom…';
  }
  const w = TOKEN_RANGES.find((x) => x.value === state.preset);
  return (w || TOKEN_RANGES[0]).label;
}

// ---- time-range popover calendar (token usage 时间维度 picker) ----

// WEEKDAYS is the Sunday-first single-letter header row of the calendar grid.
export const WEEKDAYS = ['S', 'M', 'T', 'W', 'T', 'F', 'S'];

// MONTHS/MONTHS_SHORT are deterministic English month names — never
// toLocaleString, which would make rendering locale-dependent.
const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December'];
const MONTHS_SHORT = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
  'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

// monthTitle renders a calendar month header ("August 2026").
export function monthTitle(year, month) {
  return `${MONTHS[month]} ${year}`;
}

// calendarMonthGrid builds the Sunday-first week grid for one (year, month)
// (month is 0-based): an array of weeks, each a 7-cell array with day numbers
// and null for the blank leading/trailing cells (no grayed overflow numbers
// from the adjacent months).
export function calendarMonthGrid(year, month) {
  const first = new Date(year, month, 1);
  const days = new Date(year, month + 1, 0).getDate();
  const weeks = [];
  let week = new Array(first.getDay()).fill(null);
  for (let d = 1; d <= days; d++) {
    week.push(d);
    if (week.length === 7) {
      weeks.push(week);
      week = [];
    }
  }
  if (week.length) {
    while (week.length < 7) week.push(null);
    weeks.push(week);
  }
  return weeks;
}

// shiftMonth normalizes (year, month) moved by delta months across year
// boundaries (month is 0-based, delta may be negative).
export function shiftMonth(year, month, delta) {
  const d = new Date(year, month + delta, 1);
  return { year: d.getFullYear(), month: d.getMonth() };
}

// twoMonthWindow pairs (year, month) with its successor for the side-by-side
// calendar, rolling over December → January.
export function twoMonthWindow(year, month) {
  return [{ year, month }, shiftMonth(year, month, 1)];
}

// ymd formats one calendar day as the 'YYYY-MM-DD' value the range logic and
// date comparisons use (zero-padded, so string order == chronological order).
export function ymd(year, month, day) {
  const p = (n) => String(n).padStart(2, '0');
  return `${year}-${p(month + 1)}-${p(day)}`;
}

// isFutureDay reports whether a calendar day is AFTER today (local time,
// injectable now in ms) — future days render dimmed and are unclickable.
export function isFutureDay(year, month, day, now = Date.now()) {
  const d = new Date(now);
  const today = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  return new Date(year, month, day).getTime() > today;
}

// rangePick advances the custom day-picking state machine. pick is the
// in-progress start day ('YYYY-MM-DD') or null (no partial pick — the caller
// also passes null after a completed or discarded range, so the next click
// starts a NEW range). A second click at or after the start completes the
// range; a click BEFORE the start swaps the ends so the range still reads
// start ≤ end.
export function rangePick(pick, day) {
  if (!pick) return { pick: day, start: null, end: null, complete: false };
  const [start, end] = pick <= day ? [pick, day] : [day, pick];
  return { pick: null, start, end, complete: true };
}

// customRangeLabel renders an applied custom range compactly for the picker
// trigger ("Aug 3 – Sep 5"); falls back to the raw values for unparseable
// input (defensive — the picker only applies complete valid ranges).
export function customRangeLabel(startValue, endValue) {
  const fmt = (v) => {
    const d = parseLocalDate(v);
    return d ? `${MONTHS_SHORT[d.getMonth()]} ${d.getDate()}` : v;
  };
  return `${fmt(startValue)} – ${fmt(endValue)}`;
}

// tokenRangeTriggerLabel is the trigger button's value half: the preset label
// or the compact custom range ('Custom…' while nothing valid is applied).
export function tokenRangeTriggerLabel(state) {
  if (state && state.preset === 'custom') {
    return tokenCustomBounds(state.customStart, state.customEnd)
      ? customRangeLabel(state.customStart, state.customEnd)
      : 'Custom…';
  }
  return tokenRangeLabel(state);
}

// prettyJSON returns indented JSON (2-space) for a captured request/response
// body, or null when the text is not a JSON object/array. Requiring an object
// or array keeps a bare number/string/quoted scalar from rendering as "JSON".
// Used by the Requests detail view to format bodies instead of dumping them.
export function prettyJSON(text) {
  if (typeof text !== 'string') return null;
  const trimmed = text.trim();
  if (!trimmed || (trimmed[0] !== '{' && trimmed[0] !== '[')) return null;
  try {
    const parsed = JSON.parse(trimmed);
    if (parsed === null || typeof parsed !== 'object') return null;
    return JSON.stringify(parsed, null, 2);
  } catch (_) {
    return null;
  }
}

// formatJSONLoose re-indents JSON-looking text WITHOUT validating it: a
// string-aware structural pass that indents on { [ , and dedents on } ].
// It exists for bodies truncated mid-capture (request_log max_body_bytes),
// where strict JSON.parse fails and prettyJSON returns null — the valid prefix
// still deserves structure instead of one multi-MB unreadable line. Returns
// null when the text does not start with { or [ (not JSON-shaped at all).
// Truncation mid-string simply ends the output there.
export function formatJSONLoose(text) {
  if (typeof text !== 'string') return null;
  const trimmed = text.trim();
  if (!trimmed || (trimmed[0] !== '{' && trimmed[0] !== '[')) return null;
  const lines = [];
  let cur = '';
  let depth = 0;
  let inString = false;
  let escaped = false;
  const push = () => {
    if (cur.trim() === '') { cur = ''; return; }
    lines.push('  '.repeat(Math.max(depth, 0)) + cur.trimEnd());
    cur = '';
  };
  for (let i = 0; i < trimmed.length; i += 1) {
    const ch = trimmed[i];
    if (inString) {
      cur += ch;
      if (escaped) escaped = false;
      else if (ch === '\\') escaped = true;
      else if (ch === '"') inString = false;
      continue;
    }
    switch (ch) {
      case '"':
        inString = true;
        cur += ch;
        break;
      case '{':
      case '[':
        cur += ch;
        push();
        depth += 1;
        break;
      case '}':
      case ']':
        push();
        depth -= 1;
        cur = ch;
        break;
      case ',':
        cur += ch;
        push();
        break;
      case ':':
        cur += ': ';
        break;
      case ' ':
      case '\t':
      case '\n':
      case '\r':
        if (cur.trim() !== '') cur += ch;
        break;
      default:
        cur += ch;
    }
  }
  push();
  return lines.join('\n');
}

// jsonToHTML pretty-prints `text` and tokenizes it into escaped HTML with
// span classes (j-key / j-str / j-num / j-lit). Returns null for non-JSON, so
// callers fall back to a plain <pre>. Every slice — token or separator — is
// escaped before interpolation: the result is safe for innerHTML. Callers with
// a very large body should use prettyJSON + highlightJSON directly so they can
// skip the span explosion while keeping the formatting.
export function jsonToHTML(text) {
  const pretty = prettyJSON(text);
  return pretty === null ? null : highlightJSON(pretty);
}

// highlightJSON tokenizes already-pretty JSON text into escaped HTML spans.
// Input must be valid JSON (prettyJSON output); anything else is escaped
// verbatim. Kept separate from jsonToHTML so the Requests view can format a
// multi-MB body without wrapping every token in a span.
export function highlightJSON(pretty) {
  if (typeof pretty !== 'string') return '';
  const token = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;
  let out = '';
  let last = 0;
  let match;
  while ((match = token.exec(pretty)) !== null) {
    out += esc(pretty.slice(last, match.index));
    if (match[1] !== undefined) {
      if (match[2] !== undefined) {
        out += `<span class="j-key">${esc(match[1])}</span>${esc(match[2])}`;
      } else {
        out += `<span class="j-str">${esc(match[1])}</span>`;
      }
    } else if (match[3] !== undefined) {
      out += `<span class="j-lit">${esc(match[3])}</span>`;
    } else if (match[4] !== undefined) {
      out += `<span class="j-num">${esc(match[4])}</span>`;
    }
    last = token.lastIndex;
  }
  out += esc(pretty.slice(last));
  return out;
}

// parseSSE splits a Server-Sent Events stream into events. Lines are grouped
// by blank line; `event`/`data`/`id`/`retry` fields are collected, multiple
// `data:` lines join with newline, and comment lines (`:`) are ignored.
// Returns [] for anything that is not an SSE stream.
export function parseSSE(text) {
  if (typeof text !== 'string' || text === '') return [];
  const normalized = text.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  const events = [];
  for (const block of normalized.split('\n\n')) {
    if (!block.trim()) continue;
    const event = { event: '', data: '', id: '', retry: '' };
    const dataLines = [];
    let hasField = false;
    for (const line of block.split('\n')) {
      if (line.startsWith(':')) continue;
      const colon = line.indexOf(':');
      const field = colon === -1 ? line : line.slice(0, colon);
      let value = colon === -1 ? '' : line.slice(colon + 1);
      if (value.startsWith(' ')) value = value.slice(1);
      if (field === 'event') { event.event = value; hasField = true; }
      else if (field === 'data') { dataLines.push(value); hasField = true; }
      else if (field === 'id') { event.id = value; hasField = true; }
      else if (field === 'retry') { event.retry = value; hasField = true; }
    }
    if (!hasField) continue;
    event.data = dataLines.join('\n');
    events.push(event);
  }
  return events;
}

// isSSE reports whether a captured body is a Server-Sent Events stream. The
// response content-type is authoritative; without it, a body is only treated
// as SSE when it starts with an SSE field and contains a `data:` line, so a
// JSON body never misclassifies.
export function isSSE(text, contentType) {
  if (contentType && /text\/event-stream/i.test(contentType)) return true;
  if (typeof text !== 'string') return false;
  const trimmed = text.replace(/^\s+/, '');
  if (!/^(data:|event:|id:|:)/.test(trimmed)) return false;
  return /(^|\n)data:/.test(trimmed);
}

// splitLinesByBudget splits text into chunks of at most maxChars UTF-16 units,
// breaking ONLY at newlines so a chunk never cuts a line (and therefore never
// cuts a long JSON string value in half). A single line longer than maxChars
// becomes its own chunk. Concatenating the chunks returns the input exactly.
// Used to render a multi-MB body in bounded pieces that a scroll handler
// appends one by one, instead of putting the whole thing in the DOM at once.
export function splitLinesByBudget(text, maxChars) {
  if (typeof text !== 'string' || text === '') return [];
  if (!(maxChars > 0)) return [text];
  const chunks = [];
  let start = 0; // start of the current chunk
  let size = 0; // chars accumulated in the current chunk
  let lineStart = 0; // start of the line being measured
  for (let i = 0; i <= text.length; i++) {
    if (i < text.length && text[i] !== '\n') continue;
    const lineEnd = i < text.length ? i + 1 : i; // include the newline
    const lineLen = lineEnd - lineStart;
    if (size > 0 && size + lineLen > maxChars) {
      chunks.push(text.slice(start, lineStart));
      start = lineStart;
      size = 0;
    }
    size += lineLen;
    lineStart = lineEnd;
  }
  if (start < text.length) chunks.push(text.slice(start));
  return chunks;
}

// fmtGuardDetail humanizes a guard event's raw detail string for display
// ("paths=proxy_creds action=log" → "sensitive paths: proxy_creds · action: log").
export function fmtGuardDetail(s) {
  return String(s || '')
    .replace(/\bpaths=/g, 'sensitive paths: ')
    .replace(/\bsecrets=/g, 'secrets: ')
    .replace(/\s*\baction=/g, ' · action: ')
    .trim();
}

// fmtProgressBytes renders a byte count as a compact human label.
// (0 → "0 B", 1536 → "1.5 KiB", 2097152 → "2 MiB").
export function fmtProgressBytes(n) {
  const units = ['B', 'KiB', 'MiB', 'GiB'];
  let value = Number(n) || 0;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  if (i === 0) return `${value} ${units[i]}`;
  const rounded = Math.round(value * 10) / 10;
  return `${rounded} ${units[i]}`;
}

// linkedModels returns the model-filter options for a selected provider: every
// model in that provider's config list plus every exposed route name that has
// a target on it. With no provider it returns the union of all providers'
// models and all route names. Keeps the Requests model dropdown in sync with
// the provider dropdown (GET /api/config provider_models + routes).
export function linkedModels(provider, providerModels, routes) {
  const set = new Set();
  const models = providerModels || {};
  if (provider) {
    // Substring match (mirrors the backend's case-insensitive provider filter),
    // so typing a partial provider still narrows the model list sensibly.
    const wanted = provider.toLowerCase();
    for (const name of Object.keys(models)) {
      if (!name.toLowerCase().includes(wanted)) continue;
      for (const model of models[name] || []) set.add(model);
    }
    for (const [exposed, targets] of Object.entries(routes || {})) {
      if ((targets || []).some((target) => target && target.provider && target.provider.toLowerCase().includes(wanted))) {
        set.add(exposed);
      }
    }
    return [...set].sort();
  }
  for (const exposed of Object.keys(routes || {})) set.add(exposed);
  for (const list of Object.values(models)) {
    for (const model of list || []) set.add(model);
  }
  return [...set].sort();
}

// sessionsForAgent narrows the Requests session dropdown to the sessions that
// actually served the selected agent, using each /api/sessions summary's
// `agents` list. Input order (most recently active first) is preserved. With
// no agent selected every session is offered; a session whose `agents` is
// empty (records written before the agent dimension, or an agent-less client)
// is kept only in that unfiltered case — it cannot be claimed for a specific
// agent without inventing an attribution.
export function sessionsForAgent(agent, sessions) {
  const list = sessions || [];
  if (!agent) return [...list];
  return list.filter((session) => ((session && session.agents) || []).includes(agent));
}

// linkedAgents returns the agent-filter options for a selected session: the
// agents observed on that session (normally exactly one, so picking a session
// pins the agent). With no session selected it falls back to the log-wide
// agent facet. An unknown session id (selected but aged out of the aggregate)
// keeps the full facet list rather than collapsing to none, so the dropdown
// never becomes a dead end.
export function linkedAgents(session, sessions, agentFacets) {
  const all = agentFacets || [];
  if (!session) return [...all];
  const found = (sessions || []).find((entry) => entry && entry.session_id === session);
  if (!found || !(found.agents || []).length) return [...all];
  return [...found.agents].sort();
}

// ANALYTICS_METRICS defines the Analytics tab's chart metrics. id feeds
// analyticsPointValue; label/axis drive the segmented control and the uPlot
// y-axis. gap=true means "no data" renders as a missing bar (a bucket with
// no requests has no error rate or latency, and an unpriced series has no
// cost). Every metric renders as translucent columns (analyticsBarPaths):
// each is one aggregate per bucket, where a line would imply traffic
// between sparse buckets. tokens is the four-bucket total (the server's
// unified `tokens` field — the same definition everywhere).
export const ANALYTICS_METRICS = [
  { id: 'tokens', label: 'Tokens', axis: 'tokens', gap: false },
  { id: 'toksec', label: 'Tok/s', axis: 'tok/s', gap: true },
  { id: 'cache', label: 'Cache', axis: 'cache hit %', gap: true },
  { id: 'latency', label: 'Latency', axis: 'avg ms', gap: true },
  { id: 'ttft', label: 'TTFT', axis: 'avg ms', gap: true },
  { id: 'requests', label: 'Requests', axis: 'requests', gap: false },
  { id: 'failovers', label: 'Failovers', axis: 'failovers', gap: false },
  { id: 'rate429', label: '429s', axis: '429s', gap: false },
  { id: 'errors', label: 'Errors', axis: 'error %', gap: true },
  { id: 'cost', label: 'Cost', axis: 'USD', gap: true },
];

// analyticsMetricOptions returns the metric segment's option list for the
// query shape. The failover/429 attempt counters exist only in
// minute_buckets, so every agent-dimension read (by=agent, or any agent
// filter — both route to agent_buckets server-side) disables those two
// metrics: the columns are structurally absent there, not merely zero.
export function analyticsMetricOptions(by, agent) {
  const agentRead = by === 'agent' || !!agent;
  return ANALYTICS_METRICS.map((m) => ({
    value: m.id,
    label: m.label,
    disabled: agentRead && (m.id === 'failovers' || m.id === 'rate429'),
  }));
}

// analyticsMetricAllowed reports whether a (possibly stored) metric pick is
// selectable for the query shape; an unavailable pick falls back to tokens —
// the same pattern as the granularity control's span gating.
export function analyticsMetricAllowed(metricId, by, agent) {
  const opt = analyticsMetricOptions(by, agent).find((m) => m.value === metricId);
  return !!opt && !opt.disabled;
}

// analyticsPointValue reads one metric from an /api/analytics point. The
// derived metrics (tokens total, tok/s, cache hit %, error %) are computed
// by the SERVER (the unified interface — one definition, see the handler's
// foldTotals/derivedPoint); this is a thin reader, not a second formula:
// - tokens: four-bucket total (server field; zero-fills as a count metric)
// - cost: null for unpriced points (no configured price — never fabricated)
// - errors/toksec/cache: server err_pct/tok_sec/cache_hit_pct, null = no
//   data (gap metrics draw a uPlot gap)
// - latency/ttft: bucket averages (requests-weighted server-side), null
//   without requests
export function analyticsPointValue(p, kind) {
  if (!p) return null;
  switch (kind) {
    case 'tokens': return Number(p.tokens || 0);
    case 'cost': return p.cost != null ? Number(p.cost) : null;
    case 'requests': return Number(p.requests || 0);
    case 'failovers': return Number(p.failovers || 0);
    case 'rate429': return Number(p.rate_limited_429 || 0);
    case 'errors': return p.err_pct == null ? null : Number(p.err_pct);
    case 'latency': return p.requests ? Number(p.avg_latency_ms || 0) : null;
    case 'ttft': return p.requests ? Number(p.avg_ttft_ms || 0) : null;
    case 'toksec': return p.tok_sec == null ? null : Number(p.tok_sec);
    case 'cache': return p.cache_hit_pct == null ? null : Number(p.cache_hit_pct);
    default: return null;
  }
}

// analyticsChartSeries turns /api/analytics `series` into the arrays uPlot
// needs: x is the sorted union of bucket timestamps in unix SECONDS (uPlot's
// time scale unit — passing milliseconds renders year 58655) — or, when the
// caller passes xGrid, the union of that grid with the data buckets, so the
// axis spans the whole queried window. Grid coverage changes sparse-data
// rendering: instead of a long diagonal line connecting two distant buckets
// (which implies traffic in between), count metrics (requests/tokens)
// zero-fill the empty buckets and gap metrics draw uPlot gaps — absence
// reads as "no usage", not interpolation. ys[i] is one value per bucket for
// series[i], labels[i] names it (agent-dimension series label by agent).
// Gap metrics keep null for missing/undefined buckets; requests/tokens
// zero-fill an absent bucket. Returns {x, ys, labels}.
export function analyticsChartSeries(series, kind, xGrid) {
  const list = Array.isArray(series) ? series : [];
  const dataX = list.flatMap((s) => (s.points || []).map((p) => p.bucket));
  const x = [...new Set((Array.isArray(xGrid) && xGrid.length ? xGrid : []).concat(dataX))].sort((a, b) => a - b);
  const labels = [];
  const ys = [];
  const metric = ANALYTICS_METRICS.find((m) => m.id === kind);
  const zeroFill = metric ? !metric.gap : false;
  for (const s of list) {
    labels.push(s.agent ? `${s.agent}/${s.model}` : `${s.provider}/${s.model}`);
    const by = Object.fromEntries((s.points || []).map((p) => [p.bucket, p]));
    ys.push(x.map((t) => {
      const v = analyticsPointValue(by[t], kind);
      return v == null && zeroFill ? 0 : v;
    }));
  }
  return { x, ys, labels };
}

// analyticsTickLabel formats one x-axis tick in LOCAL time, 24-hour clock,
// adapting to the tick's natural resolution: month starts show the month,
// midnights show the date, anything else shows date + HH:mm. `tickSpanSec`
// (the gap between adjacent ticks, when known) demotes dense minute-level
// ticks to HH:mm only: uPlot sizes tick density for its own short time
// labels, and a full MM-DD HH:mm at that density overlaps (measured: 67px
// labels on 63px spacing). `tickPx` (the measured px gap between adjacent
// ticks, when the caller can read it off the chart) applies the same demotion
// to coarser spans uPlot packed tighter than the full label's ~67px — e.g.
// hourly ticks on a narrow chart. Midnight ticks still anchor the date.
export function analyticsTickLabel(v, tickSpanSec, tickPx) {
  const d = new Date(v * 1000);
  const p = (n) => String(n).padStart(2, '0');
  const midnight = d.getHours() === 0 && d.getMinutes() === 0;
  if (midnight && d.getDate() === 1) return `${d.getFullYear()}-${p(d.getMonth() + 1)}`;
  if (midnight) return `${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  if (tickSpanSec != null && tickSpanSec > 0 && tickSpanSec <= 15 * 60) {
    return `${p(d.getHours())}:${p(d.getMinutes())}`;
  }
  if (tickPx != null && tickPx > 0 && tickPx < 72) {
    return `${p(d.getHours())}:${p(d.getMinutes())}`;
  }
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// analyticsTableRows maps each /api/analytics series to one leaderboard row.
// A thin reader over the server's unified series.totals block (same
// definitions as the window totals / compare / KPI cards — the frontend
// never re-derives a metric). costPerMTok is the blended payg unit price
// over the four-bucket billable volume (cost ÷ tokens × 1M).
export function analyticsTableRows(series) {
  const rows = [];
  for (const s of (Array.isArray(series) ? series : [])) {
    const t = (s && s.totals) || {};
    const cost = t.cost == null ? null : Number(t.cost);
    const tokens = Number(t.tokens || 0);
    rows.push({
      label: s.agent ? `${s.agent}/${s.model}` : `${s.provider}/${s.model}`,
      agent: s.agent || '',
      provider: s.provider,
      model: s.model,
      requests: Number(t.requests || 0),
      tokens,
      failures: Number(t.failures || 0),
      failovers: Number(t.failovers || 0),
      rateLimited: Number(t.rate_limited_429 || 0),
      errPct: t.err_pct == null ? null : Number(t.err_pct),
      latencyMs: t.avg_latency_ms == null ? null : Number(t.avg_latency_ms),
      ttftMs: t.avg_ttft_ms == null ? null : Number(t.avg_ttft_ms),
      // Output decode speed (server tok_sec): output over full call time.
      tokSec: t.tok_sec == null ? null : Number(t.tok_sec),
      cachePct: t.cache_hit_pct == null ? null : Number(t.cache_hit_pct),
      cost,
      costPerMTok: cost != null && tokens > 0 ? cost / tokens * 1e6 : null,
    });
  }
  return rows;
}

// analyticsRowSortKey maps one leaderboard row to the sort key of the active
// chart metric (tokens/cost by volume, errors/latency by rate, requests by
// count) so the table and the chart tell the same story. Shared by the
// Analytics tab's and the Status→Dashboard's leaderboards (null keys sort
// last via the -1 sentinel).
export function analyticsRowSortKey(r, metricId) {
  switch (metricId) {
    case 'requests': return r.requests;
    case 'failovers': return r.failovers;
    case 'rate429': return r.rateLimited;
    case 'errors': return r.errPct == null ? -1 : r.errPct;
    case 'latency': return r.latencyMs == null ? -1 : r.latencyMs;
    case 'ttft': return r.ttftMs == null ? -1 : r.ttftMs;
    case 'toksec': return r.tokSec == null ? -1 : r.tokSec;
    case 'cache': return r.cachePct == null ? -1 : r.cachePct;
    case 'cost': return r.cost == null ? -1 : r.cost;
    default: return r.tokens; // tokens + anything unlisted
  }
}

// ANALYTICS_TABLE_SORT maps the leaderboard's sortable column ids (the
// data-sort attribute on each header cell) to their first-click direction:
// volume/rate/cost columns read biggest-first; the series label reads A→Z;
// status reads score ascending — worst model first, the actionable order.
export const ANALYTICS_TABLE_SORT = {
  series: 'asc',
  status: 'asc',
  requests: 'desc',
  tokens: 'desc',
  err: 'desc',
  latency: 'desc',
  ttft: 'desc',
  toksec: 'desc',
  cost: 'desc',
  share: 'desc',
};

// analyticsTableSortValue reads one leaderboard column's sort value from a
// row. Rows carry healthScore/costShare only after the render joined them
// (modelHealthFromSeries by label, share = cost/totalCost); null anywhere
// means "no data" and sorts last regardless of direction.
export function analyticsTableSortValue(r, col) {
  if (!r) return null;
  switch (col) {
    case 'series': return r.label == null ? null : String(r.label);
    case 'status': return r.healthScore == null ? null : Number(r.healthScore);
    case 'requests': return Number(r.requests || 0);
    case 'tokens': return Number(r.tokens || 0);
    case 'err': return r.errPct == null ? null : Number(r.errPct);
    case 'latency': return r.latencyMs == null ? null : Number(r.latencyMs);
    case 'ttft': return r.ttftMs == null ? null : Number(r.ttftMs);
    case 'toksec': return r.tokSec == null ? null : Number(r.tokSec);
    case 'cost': return r.cost == null ? null : Number(r.cost);
    case 'share': return r.costShare == null ? null : Number(r.costShare);
    default: return null;
  }
}

// analyticsSortRows returns the leaderboard's display order: a pinned column
// (analyticsTableSortValue; "no data" rows always last, both directions) or,
// unpinned, the active chart metric via analyticsRowSortKey descending — the
// historical default, so the table still tells the chart's story until the
// operator picks a column. `sort` is {col, dir}; col null means unpinned.
export function analyticsSortRows(rows, metricId, sort) {
  const list = Array.isArray(rows) ? [...rows] : [];
  const col = sort && sort.col ? sort.col : null;
  if (!col || !(col in ANALYTICS_TABLE_SORT)) {
    list.sort((a, b) => analyticsRowSortKey(b, metricId) - analyticsRowSortKey(a, metricId));
    return list;
  }
  const dir = sort.dir === 'asc' ? 1 : -1;
  list.sort((a, b) => {
    const va = analyticsTableSortValue(a, col);
    const vb = analyticsTableSortValue(b, col);
    if (va == null && vb == null) return 0;
    if (va == null) return 1; // nulls last in both directions
    if (vb == null) return -1;
    if (typeof va === 'string' || typeof vb === 'string') {
      return dir * String(va).localeCompare(String(vb));
    }
    return dir * (va - vb);
  });
  return list;
}

// analyticsValueText renders one metric value for display (chart tooltip,
// live legend): metric-aware units, null/NaN → em dash. tokens/requests use
// fmtCompact; cost keeps 4 decimals (per-bucket sums are small); rates keep
// one decimal; latency rounds to ms.
export function analyticsValueText(metricId, v) {
  if (v == null || !isFinite(v)) return '—';
  switch (metricId) {
    case 'cost': return '$' + v.toFixed(4);
    case 'errors':
    case 'cache': return v.toFixed(1) + '%';
    case 'latency':
    case 'ttft': return Math.round(v) + 'ms';
    case 'toksec': return v.toFixed(1) + ' t/s';
    default: return fmtCompact(v);
  }
}

// pctDelta returns the percent change from prev to cur ((cur-prev)/prev*100,
// one decimal), or null when the comparison is undefined: no previous window,
// or a zero previous value ("+∞%" is noise, not signal).
export function pctDelta(cur, prev) {
  if (cur == null || prev == null || !Number(prev)) return null;
  return Math.round(((cur - prev) / prev) * 1000) / 10;
}

// analyticsGranularity maps the window span + granularity preference onto
// the API granularity. 'auto' (the default) picks the finest granularity
// that stays under roughly a few hundred buckets: ≤6h → minute, ≤7d → hour,
// ≤90d → day, else week. An explicit choice wins when the span allows it
// (see analyticsGranOptions); otherwise the span forces auto's pick — a
// preference can never request an absurd point count.
export function analyticsGranularity(spanSec, pref) {
  const auto = spanSec <= 6 * 3600 ? 'minute'
    : spanSec <= 7 * 86400 ? 'hour'
      : spanSec <= 90 * 86400 ? 'day'
        : 'week';
  if (pref && pref !== 'auto' && analyticsGranAllowed(spanSec, pref)) return pref;
  return auto;
}

// analyticsGranAllowed reports whether one granularity fits a window span:
// each needs enough buckets to be meaningful (a 1h window has no "hour"
// view) and must not explode the point count (minute over a week is ~10k
// columns). Thresholds in seconds; week/month have no upper bound. Day
// reaches 2000d so an all-time window over a multi-year history stays
// day-viewable (the all-time span is the server's data-anchored window, not
// epoch→now, so this bound is the real guard against runaway columns).
const GRAN_LIMITS = {
  minute: { min: 0, max: 2 * 86400 },
  hour: { min: 1 * 3600, max: 31 * 86400 }, // min exclusive (see analyticsGranAllowed)
  day: { min: 2 * 86400, max: 2000 * 86400 },
  week: { min: 7 * 86400, max: Infinity },
  month: { min: 14 * 86400, max: Infinity },
};

function analyticsGranAllowed(spanSec, gran) {
  const lim = GRAN_LIMITS[gran];
  if (!lim) return false;
  // Lower bounds are exclusive: a window exactly N granularities wide has
  // just one bucket (nothing to compare against).
  return spanSec > lim.min && spanSec <= lim.max;
}

// analyticsGranOptions returns the granularity segment's option list for a
// window span: every option is present (the control's layout stays stable)
// with an `allowed` flag — disallowed ones render disabled, and 'auto' is
// always available.
export function analyticsGranOptions(spanSec) {
  // Title-case display labels (the lowercase ids are the API values).
  const labels = { auto: 'Auto', minute: 'Minute', hour: 'Hour', day: 'Day', week: 'Week', month: 'Month' };
  return [
    { id: 'auto', label: labels.auto, allowed: true },
    ...['minute', 'hour', 'day', 'week', 'month'].map((id) => ({
      id, label: labels[id], allowed: analyticsGranAllowed(spanSec, id),
    })),
  ];
}

// HEAT_DAYS labels the year heatmap's rows, Monday-first — the server's
// day buckets are laid out GitHub-contribution style.
export const HEAT_DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

// yearDayKey formats one Date's local calendar day as YYYY-MM-DD (the grid
// key — same shape the picker's ymd() builds from parts).
function yearDayKey(d) {
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
}

// analyticsYearGrid lays the server's day cells out contribution-graph
// style: columns are Monday-first weeks, rows are Mon..Sun. from/to are the
// heatmap window's unix seconds, rendered in the browser's local calendar
// (self-consistent with the cell day instants). Each week carries `lead`
// (its Monday's YYYY-MM-DD) and days[wd]: the cell (traffic that day),
// null (in range, no traffic) or false (outside the window — rendered
// invisible). max is the largest token count across cells — the heatmap's
// FIXED usage metric, independent of the trend chart's switcher.
export function analyticsYearGrid(cells, from, to) {
  const byDay = new Map();
  for (const c of (Array.isArray(cells) ? cells : [])) {
    if (!c || !Number.isFinite(Number(c.day))) continue;
    byDay.set(yearDayKey(new Date(c.day * 1000)), c);
  }
  const fromDate = new Date(from * 1000);
  const toDate = new Date(to * 1000);
  const firstDay = new Date(fromDate.getFullYear(), fromDate.getMonth(), fromDate.getDate());
  const lastDay = new Date(toDate.getFullYear(), toDate.getMonth(), toDate.getDate());
  const gridStart = new Date(firstDay.getFullYear(), firstDay.getMonth(), firstDay.getDate() - ((firstDay.getDay() + 6) % 7));
  const weeks = [];
  let max = 0;
  for (let w = new Date(gridStart); w <= lastDay; w.setDate(w.getDate() + 7)) {
    const days = [];
    for (let i = 0; i < 7; i++) {
      const d = new Date(w.getFullYear(), w.getMonth(), w.getDate() + i);
      if (d < firstDay || d > lastDay) { days.push(false); continue; }
      const cell = byDay.get(yearDayKey(d)) || null;
      days.push(cell);
      if (cell) {
        const v = Number(cell.tokens || 0);
        if (v > max) max = v;
      }
    }
    weeks.push({ lead: yearDayKey(w), days });
  }
  return { weeks, max };
}

// analyticsYearMonthSpans partitions the week columns into month runs
// (consecutive columns whose Monday falls in the same calendar month) and
// returns one entry per run: {month, col (first column index), span, label}.
// The label CENTERS over the run's span — a reference only, the columns
// are weeks and never align exactly to calendar months. Runs shorter than
// two columns (window edges, a straddled February) stay unlabeled to avoid
// collisions. The runs partition every column, labeled or not.
export function analyticsYearMonthSpans(weeks) {
  const list = Array.isArray(weeks) ? weeks : [];
  const spans = [];
  let run = null;
  list.forEach((w, i) => {
    const m = Number(String((w && w.lead) || '').slice(5, 7)) - 1;
    const month = Number.isInteger(m) && m >= 0 && m < 12 ? m : -1;
    if (run && run.month === month) {
      run.span++;
      return;
    }
    if (run) spans.push(run)
    run = { month, col: i, span: 1 };
  });
  if (run) spans.push(run);
  return spans.map((s) => ({ ...s, label: s.span >= 2 && s.month >= 0 ? MONTHS_SHORT[s.month] : '' }));
}

// analyticsHeatCellSize picks the square cell edge for the year grid's
// week columns: fill the measured available width (minus the weekday gutter
// and inter-column gaps) with an integer edge, floored at 8px (narrower
// windows fall back to the scroll wrapper) and capped at 18px (ultra-wide
// windows keep the graph dense instead of growing giant cells). Explicit
// sizes — not aspect-ratio-in-grid — keep every engine's layout identical.
export function analyticsHeatCellSize(availablePx, weeks, gutterPx = 42, gapPx = 3) {
  const n = Math.max(1, Number(weeks) || 1);
  const usable = (Number(availablePx) || 0) - gutterPx - gapPx * (n - 1);
  return Math.max(8, Math.min(18, Math.floor(usable / n)));
}

// analyticsHeatTip composes the hover tooltip for one day cell as a
// structured title + label/value rows (the renderer right-aligns values and
// mutes labels, so the numbers scan at a glance). Null derived fields are
// omitted, never fabricated. Empty in-window days still answer "what day is
// this square": the title plus a muted "No usage" note.
export function analyticsHeatTip(dayUnix, cell) {
  const d = new Date(dayUnix * 1000);
  const title = `${HEAT_DAYS[(d.getDay() + 6) % 7]}, ${MONTHS_SHORT[d.getMonth()]} ${d.getDate()}, ${d.getFullYear()}`;
  if (!cell) return { title, rows: [], note: 'No usage' };
  const rows = [
    { label: 'Requests', value: fmtNum(cell.requests || 0) },
    { label: 'Tokens', value: fmtCompact(Number(cell.tokens || 0)) },
  ];
  if (cell.cost != null) rows.push({ label: 'Cost', value: `$${Number(cell.cost).toFixed(4)}` });
  if (cell.err_pct != null) rows.push({ label: 'Errors', value: `${Number(cell.err_pct).toFixed(1)}%` });
  if (cell.avg_latency_ms != null && cell.requests) {
    const ms = Number(cell.avg_latency_ms);
    rows.push({ label: 'Avg Latency', value: ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${Math.round(ms)}ms` });
  }
  return { title, rows };
}

// analyticsHeatLevel buckets one metric value into five ordinal intensity
// levels (0=empty/unmetric'd, 4=top quarter of the max). Ordinal buckets
// instead of a linear alpha ramp: usage distributions are peak-skewed, and
// linear shading renders every off-peak cell invisibly pale.
export function analyticsHeatLevel(v, max) {
  if (v == null || !isFinite(v) || v <= 0 || !max || max <= 0) return 0;
  const r = v / max;
  if (r >= 0.75) return 4;
  if (r >= 0.5) return 3;
  if (r >= 0.25) return 2;
  return 1;
}

// MODEL_HEALTH_DIMS defines the Status→Dashboard health scoring dimensions
// (the merged Model Health view).
// kind 'lower' scores 1 at/below goodBelow decaying to 0 at/past poorAbove;
// kind 'higher' is the mirror. Thresholds are sane LLM-proxy defaults (not
// config): a streaming call should answer within ~1.5s, finish a typical
// request within a few seconds, and decode well above 10 tok/s. A dimension
// with no data (e.g. duration not yet recorded) is excluded and the weights
// renormalize instead of dragging the score down.
export const MODEL_HEALTH_DIMS = [
  { id: 'latency', label: 'latency', kind: 'lower', goodBelow: 2000, poorAbove: 8000, weight: 0.3 },
  { id: 'ttft', label: 'ttft', kind: 'lower', goodBelow: 1500, poorAbove: 6000, weight: 0.4 },
  { id: 'toksec', label: 'tok/s', kind: 'higher', goodAbove: 40, poorBelow: 10, weight: 0.3 },
];

// modelHealthGrade buckets a 0..1 score into the semantic ok/warn/err classes
// (the same thresholds per dimension and overall).
export function modelHealthGrade(score) {
  if (score == null) return null;
  if (score >= 0.7) return 'ok';
  if (score >= 0.4) return 'warn';
  return 'err';
}

// healthDimScore maps one measurement onto 0..1 along its dimension's
// good/poor thresholds (linear between them, clamped outside).
function healthDimScore(dim, v) {
  if (v == null) return null;
  if (dim.kind === 'lower') {
    if (v <= dim.goodBelow) return 1;
    if (v >= dim.poorAbove) return 0;
    return (dim.poorAbove - v) / (dim.poorAbove - dim.goodBelow);
  }
  if (v >= dim.goodAbove) return 1;
  if (v <= dim.poorBelow) return 0;
  return (v - dim.poorBelow) / (dim.goodAbove - dim.poorBelow);
}

// modelHealthFromSeries scores each /api/analytics series (one provider/model
// over the queried window — the Status→Dashboard's fixed last-1h · by-model
// view) from three dimensions — call latency (full duration when recorded,
// else the header-time latency as a fallback), TTFT, and tok/s (tokens over
// full call duration, falling back like latency) — plus the raw error rate
// for display. Same thresholds and weighting as MODEL_HEALTH_DIMS. Returns
// one row per series with requests>0, worst grade first (ties by requests
// desc). "Worst first" is the actionable order: the degraded model an
// operator must look at is at the top. Rows key by `label`
// ("provider/model") so the leaderboard can join them onto
// analyticsTableRows output.
export function modelHealthFromSeries(series) {
  const agg = [];
  for (const s of (Array.isArray(series) ? series : [])) {
    if (!s || !s.provider || !s.model) continue;
    const a = { provider: s.provider, model: s.model, label: `${s.provider}/${s.model}`, requests: 0, failures: 0, input: 0, output: 0, latencySum: 0, ttftSum: 0, durationSum: 0 };
    for (const p of (s.points || [])) {
      const reqs = Number(p.requests || 0);
      a.requests += reqs;
      a.failures += Number(p.failures || 0);
      a.input += Number(p.input || 0);
      a.output += Number(p.output || 0);
      // The analytics averages are requests-weighted server-side: avg ×
      // requests rebuilds the bucket's summed latency/ttft/duration.
      a.latencySum += Number(p.avg_latency_ms || 0) * reqs;
      a.ttftSum += Number(p.avg_ttft_ms || 0) * reqs;
      a.durationSum += Number(p.avg_duration_ms || 0) * reqs;
    }
    agg.push(a);
  }
  const rank = { err: 0, warn: 1, ok: 2 };
  const rows = [];
  for (const a of agg) {
    if (!a.requests) continue;
    // duration is the honest call time; before it has accumulated (fresh
    // migration) the header-time latency is the fallback. tok/s counts
    // OUTPUT tokens only — the decoder's speed (fresh input arrives at
    // prefix speed and would inflate it).
    const timeSum = a.durationSum > 0 ? a.durationSum : a.latencySum;
    const values = {
      latency: timeSum > 0 ? timeSum / a.requests : null,
      ttft: a.ttftSum > 0 ? a.ttftSum / a.requests : null,
      toksec: timeSum > 0 ? a.output / (timeSum / 1000) : null,
    };
    const dims = {};
    let wsum = 0, score = 0;
    for (const dim of MODEL_HEALTH_DIMS) {
      const sc = healthDimScore(dim, values[dim.id]);
      dims[dim.id] = { v: values[dim.id], score: sc, grade: modelHealthGrade(sc) };
      if (sc != null) {
        score += dim.weight * sc;
        wsum += dim.weight;
      }
    }
    const overall = wsum > 0 ? score / wsum : null;
    rows.push({
      provider: a.provider,
      model: a.model,
      label: a.label,
      requests: a.requests,
      errPct: a.failures ? (a.failures / a.requests * 100) : 0,
      latencyMs: values.latency,
      ttftMs: values.ttft,
      tokSec: values.toksec,
      dims,
      score: overall == null ? null : Math.round(overall * 1000) / 1000,
      grade: modelHealthGrade(overall),
    });
  }
  rows.sort((x, y) => {
    const rx = rank[x.grade] ?? 3, ry = rank[y.grade] ?? 3;
    if (rx !== ry) return rx - ry;
    return y.requests - x.requests;
  });
  return rows;
}

// fmtCompact renders a count in K/M units (30268 → "30.3K",
// 1200000 → "1.2M"), up to `decimals` fractional digits (default 1;
// trailing zeros are dropped, values below 1000 stay as-is). Used for
// chart-axis ticks and the Analytics token surfaces — the KPI chip passes 2
// for extra precision (exact value stays in the title tooltip).
export function fmtCompact(n, decimals = 1) {
  const v = Number(n);
  if (!isFinite(v)) return '0';
  const places = Math.min(3, Math.max(0, Number(decimals) || 0));
  const p = 10 ** places;
  const trim = (x) => String(Math.round(x * p) / p);
  const a = Math.abs(v);
  if (a < 1000) return trim(v);
  if (a < 1e6) return trim(v / 1e3) + 'K';
  return trim(v / 1e6) + 'M';
}

// shouldFetchDetail decides whether the Live view's automatic post-render pass
// should start a requestlog detail fetch for one open row. `cached` = records
// already in requestsDetailCache; `state` = {loading,error,notLogged} from
// liveDetailState; `inFlight` = the live row is still streaming. A recorded
// error OR a 404 (notLogged) is terminal for the automatic pass — retrying on
// every re-render would loop (404 -> render -> fetch -> 404). The user's
// explicit re-open clears both and retries.
export function shouldFetchDetail(cached, state, inFlight) {
  if (cached || inFlight) return false;
  const s = state || {};
  return !s.loading && !s.error && !s.notLogged;
}

// detailFetchState normalizes a failed requestlog detail fetch. A 404 means the
// request has no log record — it never committed (an unrouted-model 502 or a
// malformed 400 is a live event but is not written to the request log) — which
// is expected, not an error, so the UI shows a neutral hint. Every other status
// stays a real error. Returns the liveDetailState shape.
export function detailFetchState(status, message) {
  if (status === 404) return { loading: false, error: '', notLogged: true };
  return { loading: false, error: message || 'load failed' };
}

// mergeLiveAndPersistedRow overlays a live event row onto a persisted request
// summary row. Live wins for in-flight state, status, latency, provider, model,
// and guard/progress; persisted fills missing agent and tokens (non-streaming
// live commits often carry zero tokens even though the recorded response body
// has real usage). Either argument may be null/undefined.
export function mergeLiveAndPersistedRow(live, persisted) {
  if (!persisted) return live || null;
  if (!live) return persisted;
  const base = { ...persisted };
  // Live carries the authoritative terminal / in-flight state.
  if (live.ts != null) base.ts = live.ts;
  if (live.status != null) base.status = live.status;
  if (live.latencyMs != null) base.latencyMs = live.latencyMs;
  if (live.provider != null && live.provider !== '') base.provider = live.provider;
  if (live.model != null && live.model !== '' && live.model !== '—') base.model = live.model;
  if (live.inFlight != null) base.inFlight = live.inFlight;
  if (live.guardHits && live.guardHits.length) base.guardHits = live.guardHits;
  if (live.progressText != null) base.progressText = live.progressText;
  if (live.progressBytes != null) base.progressBytes = live.progressBytes;
  // Agent/model: live wins when present; otherwise inherit persisted.
  if (live.agent) base.agent = live.agent;
  if (live.model && live.model !== '—') base.model = live.model;
  // Tokens: live wins when it has non-zero values; otherwise inherit persisted.
  if (live.input || live.output) {
    base.input = live.input || 0;
    base.output = live.output || 0;
  }
  // Fill from persisted only when live still lacks the value.
  if (!base.agent && persisted.agent) base.agent = persisted.agent;
  if (!base.model && persisted.model) base.model = persisted.model;
  return base;
}

// liveSessionOrder merges the persisted session summaries (/api/sessions)
// with the live event rows into the Live dropdown's ordering: most recently
// active first (a live row's newest event beats a stale persisted last_ts),
// ties broken by session id ascending so the list stays deterministic.
// Timestamps accept unix milliseconds or RFC3339 strings; sessions without a
// parseable timestamp keep their slot at the end, still ordered by id.
// Returns the ordered session ids.
export function liveSessionOrder(sessions, liveRows) {
  const tsMs = (v) => {
    const n = typeof v === 'number' ? v : Date.parse(v);
    return Number.isFinite(n) ? n : 0;
  };
  const lastMs = new Map();
  for (const s of sessions || []) {
    if (!s || !s.session_id) continue;
    lastMs.set(s.session_id, tsMs(s.last_ts));
  }
  for (const r of liveRows || []) {
    if (!r || !r.session) continue;
    if (!lastMs.has(r.session)) lastMs.set(r.session, 0);
    const ms = tsMs(r.ts);
    if (ms > lastMs.get(r.session)) lastMs.set(r.session, ms);
  }
  return [...lastMs.keys()].sort((a, b) => {
    const d = (lastMs.get(b) || 0) - (lastMs.get(a) || 0);
    return d !== 0 ? d : a.localeCompare(b);
  });
}

// shortSessionId abbreviates a session id for dense table cells: first 8 +
// '…' + last 4. Ids of 16 chars or fewer pass through unchanged (labels like
// "main" stay readable); the full id rides in the cell's title tooltip.
export function shortSessionId(id) {
  const s = String(id || '');
  return s.length > 16 ? s.slice(0, 8) + '…' + s.slice(-4) : s;
}

// liveSessionSummary folds one session's request rows (live events + persisted
// request-log summaries) and the optional /api/sessions aggregate into the Live
// session panel's numbers. Rows carry {status,latencyMs,model,provider,agent,
// input,output,cacheRead,cacheCreation,ts}; agg is the persisted SessionSummary
// (requests/errors/usage/providers/models/cost_usd/first_ts/last_ts) or null.
// Token totals prefer the aggregate when present, otherwise sum the rows (which
// now carry usage from /api/requests). Counts/latency fall back to the rows.
// Returns a plain object for the caller to render.
export function liveSessionSummary(rows, agg) {
  const list = Array.isArray(rows) ? rows : [];
  const a = agg || {};
  const usage = a.usage || a.Usage || {};
  const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
  const distinct = (vals) => [...new Set((vals || []).map((v) => String(v || '').trim()).filter(Boolean))].sort();
  const hasAgg = a.requests != null;
  const latencies = list.map((r) => num(r.latencyMs)).filter((v) => v > 0);
  const sum = (vals) => vals.reduce((s, v) => s + num(v), 0);
  return {
    requests: hasAgg ? num(a.requests) : list.length,
    errors: hasAgg ? num(a.errors) : list.filter((r) => num(r.status) >= 400).length,
    shadow: num(a.shadow_requests),
    input: hasAgg ? num(usage.Input ?? usage.input) : sum(list.map((r) => r.input)),
    output: hasAgg ? num(usage.Output ?? usage.output) : sum(list.map((r) => r.output)),
    cacheRead: hasAgg ? num(usage.CacheRead ?? usage.cache_read) : sum(list.map((r) => r.cacheRead)),
    cacheCreation: hasAgg ? num(usage.CacheCreation ?? usage.cache_creation) : sum(list.map((r) => r.cacheCreation)),
    avgLatencyMs: latencies.length ? Math.round(latencies.reduce((s, v) => s + v, 0) / latencies.length) : null,
    models: (a.models && a.models.length) ? distinct(a.models) : distinct(list.map((r) => r.model)),
    providers: (a.providers && a.providers.length) ? distinct(a.providers) : distinct(list.map((r) => r.provider)),
    cost: a.cost_usd != null ? num(a.cost_usd) : null,
    liveRows: list.length,
  };
}

// sessionHealthSummary derives the "is this session healthy" signals from
// the merged rows only (no aggregate dependency): span vs ACTIVE time (idle
// gaps over 2 min drop — the same segmentation the timeline uses), latency
// percentiles (avg hides tails), failover count, cache hit rate, throughput,
// model distribution and shadow count. Missing signals stay null/0/[] and
// drop out at render time. Shared by both session views.
export function sessionHealthSummary(rows) {
  const list = (Array.isArray(rows) ? rows : [])
    .map((r) => r || {})
    .map((r) => {
      const ts = typeof r.ts === 'number' ? r.ts : (Number.isFinite(Date.parse(r.ts)) ? Date.parse(r.ts) : null);
      return {
        ts,
        latencyMs: Number(r.latencyMs),
        ttftMs: Number(r.ttftMs),
        attempt: Number(r.attempt) || 0,
        input: Number(r.input) || 0,
        output: Number(r.output) || 0,
        cacheRead: Number(r.cacheRead) || 0,
        model: String(r.model || '').trim(),
        shadow: !!r.shadow,
      };
    })
    .filter((r) => r.ts != null)
    .sort((a, b) => a.ts - b.ts);
  const out = { spanMs: 0, activeMs: 0, p50Ms: null, p95Ms: null, ttftP50Ms: null, failovers: 0, cacheHitPct: null, tokPerSec: null, models: [], shadow: 0 };
  if (!list.length) return out;
  out.spanMs = Math.max(list[list.length - 1].ts - list[0].ts, 0);
  // Active time counts only inter-request gaps under the idle threshold —
  // a 6h lunch break must not read as "worked 6h".
  let active = 0;
  for (let i = 1; i < list.length; i++) {
    const gap = list[i].ts - list[i - 1].ts;
    if (gap > 0 && gap <= 120000) active += gap;
  }
  out.activeMs = list.length > 1 ? active : 0;
  const lats = list.map((r) => r.latencyMs).filter((v) => Number.isFinite(v) && v > 0).sort((a, b) => a - b);
  if (lats.length) {
    const pick = (p) => lats[Math.min(lats.length - 1, Math.floor(p * lats.length))];
    out.p50Ms = pick(0.5);
    out.p95Ms = pick(0.95);
  }
  const ttfts = list.map((r) => r.ttftMs).filter((v) => Number.isFinite(v) && v > 0).sort((a, b) => a - b);
  if (ttfts.length) out.ttftP50Ms = ttfts[Math.min(ttfts.length - 1, Math.floor(0.5 * ttfts.length))];
  out.failovers = list.filter((r) => r.attempt > 0).length;
  const cacheRead = list.reduce((s, r) => s + r.cacheRead, 0);
  const input = list.reduce((s, r) => s + r.input, 0);
  const denom = cacheRead + input;
  if (denom > 0) out.cacheHitPct = Math.round((cacheRead / denom) * 100);
  const output = list.reduce((s, r) => s + r.output, 0);
  const latSum = lats.reduce((s, v) => s + v, 0);
  if (latSum > 0 && output > 0) out.tokPerSec = Math.round((output / (latSum / 1000)) * 10) / 10;
  const counts = new Map();
  for (const r of list) if (r.model) counts.set(r.model, (counts.get(r.model) || 0) + 1);
  out.models = [...counts.entries()].sort((a, b) => b[1] - a[1]).map(([model, n]) => ({ model, n }));
  out.shadow = list.filter((r) => r.shadow).length;
  return out;
}

// ---------- Security tab (guard audit log) ----------

// pathStrengthFromAction maps a path-kind audit action to the strong/weak
// structural classification (decision 23): the configured action fires only
// on strong hits (path inside a tool INVOCATION — tool_use.input / function
// arguments, an agent asking to access the file). Weak mentions (prose and
// tool result content) are no longer persisted; log-weak rows survive only
// in logs written before that change. Secret-kind rows have no strength
// dimension (callers pass their action but ignore a "" result).
export function pathStrengthFromAction(action) {
  if (action === 'log-weak') return 'weak';
  if (!action) return '';
  return 'strong';
}

// SECURITY_KIND_HELP / SECURITY_ACTION_HELP back the legend on the Security
// tab — the audit table shows terse names only, so the meanings live here.
export const SECURITY_KIND_HELP = [
  { name: 'secret', text: 'A secret pattern matched the request body: a built-in rule (e.g. openai_api_key), a guard.extra_patterns custom rule, or a credential configured on this proxy (known_secret*).' },
  { name: 'path', text: 'The request referenced a sensitive, credential-bearing location (e.g. ~/.ssh, ~/.aws/credentials, ~/.kube/config, .env) in a tool INVOCATION (tool_use.input / function arguments) — an agent asking to access the file. Address mentions in prose or tool RESULT content (docs, source, error text) are not security issues by themselves: they are ignored entirely, never recorded.' },
  { name: 'drift', text: 'doctor/takeover detected a client config pointer drift — the client no longer points at this proxy.' },
];

export const SECURITY_ACTION_HELP = [
  { name: 'log', text: 'recorded; the request was forwarded unchanged' },
  { name: 'redact', text: 'forwarded with every match replaced by [REDACTED]' },
  { name: 'block', text: 'rejected with 400 — nothing was sent upstream' },
  { name: 'log-weak', text: 'weak path signal (plain-text mention); no longer persisted — only older logs carry these' },
];

// SECURITY_RANGES: the Security Activity feed's audit-window presets. 'all'
// sends no from bound (the whole 30d retention); every other preset maps to
// a from=<unix-seconds> rolling window computed at query time (client and
// daemon are the same machine — see TOKEN_RANGES for the same convention).
export const SECURITY_RANGES = [
  { value: 'all', label: 'All Time', secs: 0 },
  { value: '24h', label: 'Last 24h', secs: 86400 },
  { value: '7d', label: 'Last 7d', secs: 7 * 86400 },
  { value: '30d', label: 'Last 30d', secs: 30 * 86400 },
];

// securityRangeFromSecs resolves a preset to the unix-seconds `from` query
// value (null = no bound). now is unix milliseconds, injectable for tests.
export function securityRangeFromSecs(value, nowMs = Date.now()) {
  const r = SECURITY_RANGES.find((x) => x.value === value);
  if (!r || !r.secs) return null;
  return Math.floor(nowMs / 1000) - r.secs;
}

// securityFilterQuery projects the Security tab's filter onto URL-hash params
// (only non-default values — an unfiltered tab stays a clean #security).
// securityFilterFromQuery reads them back: null when the hash carries no
// filter keys (a bare #security must not clobber an in-memory filter), junk
// enum values dropping back to their defaults. 'low' is not a valid verdict
// filter: the feed never renders low rows, so a stale verdict=low link
// degrades to the unfiltered view.
export function securityFilterQuery(f) {
  if (!f) return '';
  const q = new URLSearchParams();
  if (f.kind) q.set('kind', f.kind);
  if (f.verdict) q.set('verdict', f.verdict);
  if (f.range && f.range !== 'all') q.set('range', f.range);
  if (f.rule) q.set('rule', f.rule);
  return q.toString();
}

export function securityFilterFromQuery(params) {
  if (!params) return null;
  const has = params.kind || params.verdict || params.rule ||
    (params.range && params.range !== 'all');
  if (!has) return null;
  return {
    kind: ['secret', 'path', 'drift'].includes(params.kind) ? params.kind : '',
    verdict: ['high', 'medium', 'error', 'skipped'].includes(params.verdict) ? params.verdict : '',
    range: ['24h', '7d', '30d'].includes(params.range) ? params.range : 'all',
    rule: params.rule || '',
  };
}

// mergeSecurityFeed merges audit records and AI adjudication results into
// one chronological feed (newest first). The projection normalizes both
// sources onto one row shape: audit rows carry agent/exposed/action/strength,
// AI rows carry verdict/model/reason/session. kind filtering is the caller's
// business (audit kinds secret|path|drift|unblock; AI rows are secret|path).
//
// A fresh (uncached) verdict lands in BOTH sources — the adjudication sink
// writes the persistent audit record (with verdict+reason) and the ring holds
// the per-occurrence entry — so a ring entry that matches an audit row by
// request_id + kind + verdict + rule is folded INTO that audit row (judge
// model / cached / session id ride along) instead of rendering a second
// near-identical row. Ring-only rows (cached occurrences, pre-restart ring
// leftovers) keep their ai· row — suppressed low verdicts are visible in the
// feed and nowhere else.
export function mergeSecurityFeed(records, adjudications) {
  const rows = [];
  const byVerdictKey = new Map();
  for (const r of records || []) {
    const row = {
      ts: r.ts, src: 'audit', kind: r.kind, names: r.names || [],
      action: r.action || '', verdict: r.verdict || '',
      agent: r.agent || '', exposed: r.exposed || '',
      detail: r.detail || '', requestId: r.request_id || '',
      reason: r.reason || '', evidence: r.evidence || '',
      strength: r.kind === 'path' ? pathStrengthFromAction(r.action) : '',
      judge: r.model || '', cached: false, sessionId: r.session_id || '',
    };
    rows.push(row);
    if (row.verdict && row.requestId) {
      for (const n of row.names) {
        byVerdictKey.set(`${row.requestId}\u0000${row.kind}\u0000${row.verdict}\u0000${n}`, row);
      }
    }
  }
  for (const a of adjudications || []) {
    const dup = a.rule && a.request_id
      ? byVerdictKey.get(`${a.request_id}\u0000${a.kind}\u0000${a.verdict}\u0000${a.rule}`)
      : null;
    if (dup) {
      dup.judge = a.model || dup.judge;
      dup.cached = dup.cached || !!a.cached;
      dup.sessionId = dup.sessionId || a.session_id || '';
      dup.reason = dup.reason || a.reason || '';
      dup.evidence = dup.evidence || a.evidence || '';
      continue;
    }
    rows.push({
      ts: a.ts, src: 'ai', kind: a.kind, names: [a.rule].filter(Boolean),
      action: a.action || '', verdict: a.verdict || '',
      agent: '', exposed: a.model || '',
      detail: a.reason || '', requestId: a.request_id || '',
      reason: a.reason || '', evidence: a.evidence || '',
      sessionId: a.session_id || '', cached: !!a.cached, strength: '',
      judge: a.model || '',
    });
  }
  // One request may hit several rules whose verdicts land as separate audit
  // records / ring entries (the adjudication sink is rule-granular). The
  // DISPLAY merges them into ONE row per request per channel (kind): the
  // worst verdict headlines the row, the per-rule verdicts/reasons ride as
  // in-row segments (securitySegmentsHTML). The operator reads one security
  // event per request per channel, never one row per rule. Rows without a
  // request id (drift, headless hits) never merge; the audit store keeps
  // per-rule records — explain's name-granular drill and the repeat index
  // are unaffected.
  const vRank = { high: 5, error: 4, medium: 3, skipped: 2, low: 1, '': 0 };
  const groups = new Map();
  const merged = [];
  for (const row of rows) {
    if (!row.requestId) { merged.push(row); continue; }
    const seg = {
      names: [...row.names], verdict: row.verdict, reason: row.reason,
      evidence: row.evidence, judge: row.judge, cached: !!row.cached, action: row.action,
    };
    const k = row.requestId + '\u0000' + row.kind;
    const i = groups.get(k);
    if (i === undefined) {
      groups.set(k, merged.length);
      merged.push({ ...row, names: [...row.names], segments: [seg] });
      continue;
    }
    const g = merged[i];
    for (const n of row.names) {
      if (!g.names.includes(n)) g.names.push(n);
    }
    if ((row.ts || 0) > (g.ts || 0)) g.ts = row.ts;
    g.sessionId = g.sessionId || row.sessionId;
    g.src = g.src === 'audit' || row.src === 'audit' ? 'audit' : g.src;
    g.cached = g.cached || !!row.cached;
    if ((vRank[row.verdict] || 0) > (vRank[g.verdict] || 0)) {
      // The new worst verdict headlines the merged row.
      g.verdict = row.verdict;
      g.reason = row.reason;
      g.evidence = row.evidence;
      g.detail = row.detail || g.detail;
      g.action = row.action || g.action;
    } else {
      g.evidence = g.evidence || row.evidence;
      g.detail = g.detail || row.detail;
      g.action = g.action || g.action;
    }
    g.judge = g.judge || row.judge;
    g.segments.push(seg);
  }
  merged.sort((x, y) => (y.ts || 0) - (x.ts || 0));
  return merged;
}

// securitySegmentsHTML renders the per-rule breakdown of a merged feed row
// (one line per rule: verdict badge + rule + its own judgment reason). Empty
// for single-segment rows — the row's own verdict/reason already say it.
export function securitySegmentsHTML(row) {
  const segs = row && row.segments;
  if (!segs || segs.length < 2) return '';
  const vBadge = { high: 'err', medium: 'warn', low: 'ok', error: 'warn', skipped: 'muted' };
  return segs.map((sg) => `<div class="sec-seg"><span class="badge ${vBadge[sg.verdict] || ''}">${esc(sg.verdict || '—')}</span>` +
    `<code>${esc((sg.names || []).join(', ') || '—')}</code>${sg.cached ? ' <span class="badge muted">cached</span>' : ''}` +
    ` <span>${esc(sg.reason || sg.evidence || '')}</span></div>`).join('');
}

// securityKpisHTML renders the Security page's summary tile row (the
// an-kpis design-system grid): blocked-session count plus the verdict
// digest. Verdict counts are the SERVER-side aggregation over the same audit
// window (/api/security counts: SQL GROUP BY in the SQLite store, plus the
// cumulative low counter from guard_stats.json) — they must NOT be counted
// from the client-merged feed: that blend included the in-memory ring's
// cached-replay rows and drifted on every restart. The LLM-usage tiles
// render only when the adjudication channel is (or was) active; a disabled
// channel shows one "off" tile instead of two permanent zeros.
export function securityKpisHTML(blocks, counts, stats, adjudicationOn) {
  const bl = blocks || [];
  const c = counts || {};
  const num = (v) => Number(v) || 0;
  const st = stats || {};
  const inTok = Number(st.input_tokens) || 0;
  const outTok = Number(st.output_tokens) || 0;
  const tile = (k, v, err, d) =>
    `<div class="an-kpi"><div class="k">${esc(k)}</div><div class="v${err ? ' err' : ''}">${v}</div>${d ? `<div class="d">${d}</div>` : ''}</div>`;
  const llm = adjudicationOn || (Number(st.calls) || 0) > 0
    ? tile('llm calls', fmtNum(st.calls || 0), false, 'judge invocations (cache hits free)') +
      tile('llm tokens', fmtCompact(inTok + outTok), false, `in ${fmtCompact(inTok)} · out ${fmtCompact(outTok)}`)
    : tile('llm adjudication', 'off', false, 'guard.adjudicate not configured');
  return `<div class="an-kpis">` +
    tile('blocked sessions', fmtNum(bl.length), bl.length > 0) +
    tile('high verdicts', fmtNum(num(c.high)), num(c.high) > 0) +
    tile('medium verdicts', fmtNum(num(c.medium)), false, 'recorded, no session block') +
    tile('low (suppressed)', fmtNum(num(c.low)), false, 'ignored tier — cumulative (rows ring-only)') +
    tile('errors', fmtNum(num(c.error) + num(c.skipped)), num(c.error) + num(c.skipped) > 0) +
    llm +
    `</div>`;
}

// securityLegendHTML renders the collapsible kind/action legend.
export function securityLegendHTML() {
  const kinds = SECURITY_KIND_HELP.map((k) => `<li><span class="badge">${esc(k.name)}</span> ${esc(k.text)}</li>`).join('');
  const actions = SECURITY_ACTION_HELP.map((a) => `<li><code>${esc(a.name)}</code> — ${esc(a.text)}</li>`).join('');
  return `<details class="sec-legend"><summary>What do these records mean?</summary>` +
    `<ul>${kinds}</ul><div class="hint">action:</div><ul>${actions}</ul>` +
    `<div class="hint">Matched content is never stored in the audit log (deliberate). Click a row to re-scan the original request and locate each hit.</div></details>`;
}

// SECURITY_EXPLAIN_STATUS_NOTES maps /api/security/explain statuses to the
// hint shown when the hits cannot be re-located.
export const SECURITY_EXPLAIN_STATUS_NOTES = {
  no_request_log: 'Request log is disabled — the original request body is not available to analyze.',
  not_found: 'The original request is no longer in the request log (retention window passed).',
  redacted: 'This request was logged after redaction (guard.secrets=redact): the matched bytes were replaced by [REDACTED] before persistence and cannot be recovered.',
  cross_request: 'Cross-request detection (split-exfiltration): the credential was fragmented across several requests of one session, so no single request body contains it.',
  scanner_unavailable: 'The current generation has no guard scanner (guard disabled or scanner build failed).',
};

// explainCacheKey is the stable identity of one analyze expansion — the
// explain endpoint's query triple (request_id, kind, names), name-order
// normalized so the same audit/AI row yields one key across re-renders.
// app.js keys the open-expansion set and the explain cache by it.
export function explainCacheKey(requestId, kind, names) {
  const ns = Array.isArray(names) ? [...names] : [names];
  ns.sort();
  return `${requestId || ''}\u0000${kind || ''}\u0000${ns.join(',')}`;
}

// securityExplainHTML renders one /api/security/explain result: the LLM
// adjudication summary first (the "why" of the verdict), then one card per
// rule name — rule identity (name + strength + explanation + regex/source)
// once, followed by each located occurrence with the hit highlighted in its
// context window (numbered when a rule fired more than once; the backend
// caps occurrences per name). Matches arrive offset-sorted, so occurrences
// of different rules interleave — grouping is done here. The window arrives
// pre-split (pre/hit/post strings) — never byte offsets, which would not
// survive the Go-bytes → JS-UTF-16 boundary. Every interpolated value is
// escaped.
export function securityExplainHTML(result) {
  if (!result || typeof result !== 'object') return '';
  const note = SECURITY_EXPLAIN_STATUS_NOTES[result.status];
  let out = note ? `<div class="msg hint">${esc(note)}</div>` : '';
  const adj = Array.isArray(result.adjudications) ? result.adjudications : [];
  if (adj.length) {
    const vBadge = { high: 'err', medium: 'warn', low: 'ok', error: 'warn', skipped: 'muted' };
    out += `<div class="sec-explain-adj"><div class="hint">LLM adjudication:</div>` + adj.map((a) =>
      `<div class="sec-explain-adj-row"><span class="badge ${vBadge[a.verdict] || ''}">${esc(a.verdict)}</span>` +
      `<code>${esc(a.rule)}</code>` +
      `${a.cached ? '<span class="badge muted">cached</span>' : ''}` +
      `${a.model ? `<span class="hint">${esc(a.model)}</span>` : ''}` +
      `${a.reason ? `<span class="sec-explain-reason">${esc(a.reason)}</span>` : ''}</div>` +
      `${a.evidence ? `<div class="sec-explain-note">evidence: ${esc(a.evidence)}</div>` : ''}`).join('') + `</div>`;
  }
  const matches = Array.isArray(result.matches) ? result.matches : [];
  if (!matches.length && !note) return '<div class="msg hint">Nothing to show.</div>';
  const groups = new Map();
  for (const m of matches) {
    if (!m || typeof m !== 'object') continue;
    const g = groups.get(m.name);
    if (g) g.push(m);
    else groups.set(m.name, [m]);
  }
  for (const [name, occ] of groups) {
    const first = occ[0];
    const strength = first.strength ? ` <span class="badge ${first.strength === 'strong' ? 'warn' : 'muted'}">${esc(first.strength)}</span>` : '';
    const rule = first.regex ? `<div class="hint">regex <code>${esc(first.regex)}</code>${first.source ? ` · source: ${esc(first.source)}` : ''}</div>` : '';
    const explanation = first.explanation ? `<div class="sec-explain-note">${esc(first.explanation)}</div>` : '';
    let occs = '';
    occ.forEach((m, i) => {
      let body = '';
      if (m.located && (m.pre !== undefined || m.hit !== undefined)) {
        body = `<pre class="log-pre body-pre sec-snippet">${esc(m.pre)}<mark>${esc(m.hit)}</mark>${esc(m.post)}</pre>`;
      } else if (!m.located) {
        body = '<div class="hint">not re-located in the stored request body</div>';
      }
      if (!body) return;
      const label = occ.length > 1 ? `<span class="sec-explain-occ-n">#${i + 1}</span>` : '';
      occs += `<div class="sec-explain-occ">${label}${body}</div>`;
    });
    out += `<div class="sec-explain-rule"><div class="sec-explain-rule-head"><code>${esc(name)}</code>${strength}</div>${explanation}${rule}${occs}</div>`;
  }
  return `<div class="sec-explain">${out}</div>`;
}

// ===========================================================================
// AUTO-REFRESH INTERACTION GATE (pure selectors + hold decision)
// ===========================================================================
//
// Every timer/SSE-driven re-render must consult the interaction gate (see
// autoRefreshBlocked in app.js) before wiping DOM the user is interacting
// with. The gate is framework-level so new pages get the protection for free:
//
//   POPUP_OPEN_SEL        — open popovers/dropdowns/calendars/menus. Any
//                           transient layer that auto-refresh would clobber
//                           MUST carry `data-popup` and use the `hidden`
//                           attribute when closed (both .tr-popover pickers,
//                           .route-pin-menu, combobox menus).
//   INTERACTIVE_CONTROL_SEL — controls holding uncommitted user input or an
//                           open native popup: a focused <input>/<select>/
//                           <textarea> covers typing, an open native <select>
//                           dropdown (the select keeps focus while its popup
//                           is open), datalist popups, and contenteditable.

export const POPUP_OPEN_SEL = '[data-popup]:not([hidden])';

export const INTERACTIVE_CONTROL_SEL = 'input, select, textarea, [contenteditable="true"], [contenteditable=""], [role="combobox"]';

// refreshHoldReason folds the gate's DOM observations into one decision:
// which interaction (if any) holds a background re-render back. Popup wins
// over focus (a calendar popover contains focused buttons — either reason
// blocks, but 'popup' is the more specific diagnosis), focus wins over
// selection. null means "refresh freely".
export function refreshHoldReason(obs) {
  if (!obs) return null;
  if (obs.openPopup) return 'popup';
  if (obs.focusInteractive) return 'focus';
  if (obs.selection) return 'selection';
  return null;
}

// staleDataText renders the banner text for a failed background refresh that
// kept the last successful data on screen: the error/what-failed prefix plus
// the shared stale-data suffix. failedParts (optional) lists the endpoints
// that failed ('status', 'tokens', …) — null/[] when the caller prefers a
// single message without the parenthetical.
export function staleDataText(errText, failedParts) {
  const parts = failedParts && failedParts.length ? ` (${failedParts.join(', ')})` : '';
  return `${errText}${parts} — showing last successful data`;
}

// ===========================================================================
// Presentation helpers for the v2 design system (SVG icons, HTTP status
// badges, KPI delta classes, log-line tokenizing). Zero-DOM like everything
// else in this file; behavior-tested in jstests/pure.test.mjs.
// ===========================================================================

// --- SVG icon set ---------------------------------------------------------
// Tiny inline icons replacing v1's text glyphs (📌 emoji, "SHOW" link
// labels). currentColor + stroke 1.5 keeps them themeable through the same
// CSS variables as text; sizing comes from CSS (.icon). No icon library —
// the UI stays fully offline/vendored.

const SVG_ATTRS = 'viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"';

// iconPin — schedule route pinning (replaces the 📌 emoji). Lucide "pin".
export function iconPin() {
  return `<svg class="icon" ${SVG_ATTRS}><path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1z"/></svg>`;
}

// iconRefresh — toolbar Refresh buttons (Lucide "refresh-cw" outline).
export function iconRefresh() {
  return `<svg class="icon" ${SVG_ATTRS}><path d="M21 12a9 9 0 1 1-2.64-6.36L21 8"/><path d="M21 3v5h-5"/></svg>`;
}

// iconChevron — disclosure summaries (details.editor, acct-section, legend).
export function iconChevron() {
  return `<svg class="icon icon-chevron" ${SVG_ATTRS}><path d="m9 18 6-6-6-6"/></svg>`;
}

// --- HTTP status badges ---------------------------------------------------

// statusBadgeClass maps an HTTP status to a semantic badge class:
// 2xx → ok, 3xx/4xx → warn, 5xx → err, missing/in-flight → muted. Color
// encodes meaning only — the same ok/warn/err palette as every badge.
export function statusBadgeClass(status) {
  const n = Number(status);
  if (!Number.isFinite(n) || n <= 0) return 'muted';
  if (n < 300) return 'ok';
  if (n < 500) return 'warn';
  return 'err';
}

// statusBadgeHTML renders an HTTP status as a semantic pill. `pending` swaps
// in the in-flight marker (···) used by the Live monitor. The label is
// always escaped — raw statuses never reach innerHTML.
export function statusBadgeHTML(status, pending) {
  if (pending) return '<span class="badge muted">···</span>';
  const n = Number(status);
  const label = Number.isFinite(n) && n > 0 ? String(n) : '—';
  return `<span class="badge ${statusBadgeClass(n)}">${esc(label)}</span>`;
}

// --- KPI delta classes ----------------------------------------------------

// kpiDeltaClass picks the delta color semantics for one KPI chip. `warn`
// marks a metric whose INCREASE is bad (failures): its up-delta is err-red
// while a decrease stays muted. For normal metrics up is ok-green and down
// muted (a drop is quieter, not an error). null/0 renders the flat class.
export function kpiDeltaClass(delta, warn) {
  if (delta == null || delta === 0) return 'flat';
  if (warn) return delta > 0 ? 'up bad' : 'down';
  return delta > 0 ? 'up' : 'down';
}

// --- log line tokenizing (Status → Logs) -----------------------------------

// Keys whose value gets semantic coloring in the runtime log viewer.
// status= splits by HTTP class (2xx ok / 4xx warn / 5xx err, mirroring
// statusBadgeClass); *error*/*fail*/*circuit* values are err;
// *retry*/*limited*/*cooldown* values are warn. Matched on lowercase keys.
const LOG_VALUE_ERR_RE = /(^|_)(err|fail|circuit|denied|timeout)/;
const LOG_VALUE_WARN_RE = /(^|_)(retry|limited|cooldown|throttl|degrad)/;

function logValueClass(key, val) {
  const lower = key.toLowerCase();
  if (lower === 'status') {
    const n = Number(val);
    if (Number.isFinite(n) && n > 0) return statusBadgeClass(n);
  }
  if (LOG_VALUE_ERR_RE.test(lower)) return 'err';
  if (LOG_VALUE_WARN_RE.test(lower)) return 'warn';
  return '';
}

// logLineHTML renders one raw runtime-log line as colored HTML: the leading
// "YYYY/MM/DD HH:MM:SS" prefix (if present) is one muted span, a Go-style
// severity token (INFO/WARN/ERROR/DEBUG) colors by severity, and `key=value`
// tokens render as key=muted value=text with semantic values colored. All
// input is escaped here — callers pass the raw line, never pre-escaped HTML.
export function logLineHTML(line) {
  const raw = String(line);
  const m = raw.match(/^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}) ?([\s\S]*)$/);
  const ts = m ? m[1] : '';
  const rest = m ? m[2] : raw;
  let out = ts ? `<span class="log-ts">${esc(ts)}</span> ` : '';
  const lvl = rest.match(/^([A-Za-z]+) /);
  if (lvl && /^(INFO|WARN|ERROR|DEBUG)$/i.test(lvl[1])) {
    const sev = lvl[1].toUpperCase();
    const cls = sev === 'ERROR' ? 'err' : sev === 'WARN' ? 'warn' : 'lvl';
    out += `<span class="log-lvl ${cls}">${esc(sev)}</span> `;
    out += logTokensHTML(rest.slice(lvl[1].length + 1));
    return out;
  }
  out += logTokensHTML(rest);
  return out;
}

// logTokensHTML splits a log message into `key=value` tokens and free text:
// each pair (value runs to the next space) becomes key/value spans, semantic
// keys color their value, and everything between stays escaped plain text.
function logTokensHTML(text) {
  let out = '';
  let i = 0;
  while (i < text.length) {
    const eq = text.indexOf('=', i);
    if (eq === -1) { out += esc(text.slice(i)); break; }
    let keyStart = eq;
    while (keyStart > i && text[keyStart - 1] !== ' ') keyStart--;
    if (keyStart === eq) {
      // '=' with no key before it: plain text through the '='.
      out += esc(text.slice(i, eq + 1));
      i = eq + 1;
      continue;
    }
    const key = text.slice(keyStart, eq);
    const valEnd = text.indexOf(' ', eq + 1);
    const val = valEnd === -1 ? text.slice(eq + 1) : text.slice(eq + 1, valEnd);
    const vcls = logValueClass(key, val);
    out += esc(text.slice(i, keyStart));
    out += `<span class="log-k">${esc(key)}</span>=<span class="log-v${vcls ? ' ' + vcls : ''}">${esc(val)}</span>`;
    if (valEnd === -1) break;
    out += ' ';
    i = valEnd + 1;
  }
  return out;
}

// ---------- session trace timeline ----------
//
// sessionBarSummary renders the hover/aria summary lines for one session-view
// row — the merged camelCase shape shared by persisted rows
// (persistedSummaryRow) and live event rows. Fields drop out when absent;
// returns [] only for a null row. Lines:
//   <time> · <agent>
//   <model> → <provider>
//   <status> · <latency> · in <n> / out <n> tok · cache <n>
//   in flight · attempt n+1 (failover)
export function sessionBarSummary(row, fmt) {
  if (!row) return [];
  const lines = [];
  const toMs = (v) => {
    const n = typeof v === 'number' ? v : Date.parse(v);
    return Number.isFinite(n) ? n : null;
  };
  const t = toMs(row.ts);
  const when = t != null && fmt ? fmt(t) : (row.ts != null ? String(row.ts) : '');
  const head = [when, row.agent].filter(Boolean).join(' · ');
  if (head) lines.push(head);
  const mp = [row.model, row.provider].filter(Boolean).join(' → ');
  if (mp) lines.push(mp);
  const bits = [];
  if (row.status) bits.push(String(row.status));
  const lat = Number(row.latencyMs);
  if (Number.isFinite(lat) && lat >= 0) bits.push(lat >= 1000 ? (lat / 1000).toFixed(1) + 's' : Math.round(lat) + 'ms');
  const ttft = Number(row.ttftMs);
  if (Number.isFinite(ttft) && ttft > 0) bits.push('ttft ' + (ttft >= 1000 ? (ttft / 1000).toFixed(1) + 's' : Math.round(ttft) + 'ms'));
  const tk = [];
  if (Number(row.input) > 0 || Number(row.output) > 0) {
    tk.push(`in ${Number(row.input) || 0} / out ${Number(row.output) || 0} tok`);
  }
  const cr = Number(row.cacheRead);
  if (cr > 0) tk.push(`cache ${cr > 999 ? (cr / 1000).toFixed(1) + 'k' : cr}`);
  if (tk.length) bits.push(tk.join(' · '));
  if (bits.length) lines.push(bits.join(' · '));
  const notes = [];
  if (row.inFlight) notes.push('in flight');
  if (Number(row.attempt) > 0) notes.push(`attempt ${Number(row.attempt) + 1} (failover)`);
  if (notes.length) lines.push(notes.join(' · '));
  return lines;
}

// collectText pulls readable assistant text pieces out of one parsed response
// event/message, covering the three protocol shapes the proxy emits:
//   anthropic  message {content:[{text}]}  + stream {content_block_delta}
//   openai     chat {choices:[{message|delta:{content}}]}
//   responses  {output:[{content:[{text}]}]}
// Thinking/partial-json deltas are skipped as text deliberately (the excerpt
// is a content preview, not a reasoning dump), but tool calls and thinking
// blocks are noted on ctx so the caller can fall back to a marker when a
// turn produced no text at all (coding-agent turns are usually tool-only).
// Error bodies ({error:{message}}) surface as text — that IS the response.
function collectText(ev, ctx) {
  const push = (s) => { if (typeof s === 'string' && s) ctx.pieces.push(s); };
  if (!ev || typeof ev !== 'object') return;
  if (Array.isArray(ev.content)) {
    for (const c of ev.content) {
      if (!c || typeof c !== 'object') continue;
      if (typeof c.text === 'string') push(c.text);
      if (c.type === 'tool_use' && c.name) ctx.tool(c.name);
      if (c.type === 'thinking') ctx.thinking = true;
    }
  }
  if (ev.type === 'content_block_start' && ev.content_block && typeof ev.content_block === 'object') {
    if (ev.content_block.type === 'tool_use' && ev.content_block.name) ctx.tool(ev.content_block.name);
    if (ev.content_block.type === 'thinking') ctx.thinking = true;
  }
  if (ev.type === 'content_block_delta' && ev.delta && typeof ev.delta.text === 'string') {
    push(ev.delta.text);
  }
  if (Array.isArray(ev.choices)) {
    for (const ch of ev.choices) {
      const holder = ch && (ch.message || ch.delta);
      if (!holder) continue;
      const c = holder.content;
      if (typeof c === 'string') push(c);
      else if (Array.isArray(c)) {
        for (const p of c) if (p && typeof p.text === 'string') push(p.text);
      }
      if (Array.isArray(holder.tool_calls)) {
        for (const tc of holder.tool_calls) {
          if (tc && tc.function && tc.function.name) ctx.tool(tc.function.name);
        }
      }
    }
  }
  if (Array.isArray(ev.output)) {
    for (const o of ev.output) {
      if (!o || typeof o !== 'object') continue;
      if (o.type === 'function_call' && o.name) ctx.tool(o.name);
      if (Array.isArray(o.content)) {
        for (const p of o.content) if (p && typeof p.text === 'string') push(p.text);
      }
    }
  }
  if (ev.error && typeof ev.error.message === 'string') push(ev.error.message);
}

// requestExcerpt extracts THIS TURN's user input — the newest human text —
// from a logged request body string. Handles anthropic/openai `messages[]`
// and responses `input` (string or message array). Tool-result-only user
// messages are skipped (they carry tool output, not the prompt), so a coding
// agent's turn still surfaces its newest human instruction. Same caps and
// bail-outs as responseExcerpt; '' when nothing textual is found.
export function requestExcerpt(text, maxChars) {
  const cap = Number.isFinite(maxChars) && maxChars > 0 ? Math.floor(maxChars) : 200;
  if (!text || typeof text !== 'string' || text.length > 1500000) return '';
  let body = null;
  try { body = JSON.parse(text); } catch (_) { return ''; }
  if (!body || typeof body !== 'object') return '';
  if (typeof body.input === 'string' && body.input.trim()) return clip(body.input, cap);
  const msgs = Array.isArray(body.messages) ? body.messages
    : (Array.isArray(body.input) ? body.input : null);
  if (!msgs) return '';
  // Scan backward: the newest user message that actually carries text.
  for (let i = msgs.length - 1; i >= 0; i--) {
    const m = msgs[i];
    if (!m || m.role !== 'user') continue;
    const c = m.content;
    if (typeof c === 'string' && c.trim()) return clip(c, cap);
    if (Array.isArray(c)) {
      const texts = [];
      for (const p of c) {
        if (p && typeof p === 'object' && typeof p.text === 'string') texts.push(p.text);
      }
      if (texts.length) return clip(texts.join(' '), cap);
    }
  }
  return '';
}

// clip collapses whitespace and caps a preview string with an ellipsis.
function clip(s, cap) {
  const flat = String(s).replace(/\s+/g, ' ').trim();
  if (!flat) return '';
  return flat.length > cap ? flat.slice(0, cap).trimEnd() + '…' : flat;
}

// responseExcerpt extracts a short assistant-text preview from a logged
// response body STRING — a JSON message, an SSE stream of data: lines, or
// (fallback) plain non-JSON text such as an upstream error page. Bodies over
// 1.5 MB are skipped (hover must stay cheap); returns '' when nothing
// textual can be extracted. Whitespace collapses, result caps at maxChars
// (default 200) with an ellipsis.
export function responseExcerpt(text, maxChars) {
  const cap = Number.isFinite(maxChars) && maxChars > 0 ? Math.floor(maxChars) : 200;
  if (!text || typeof text !== 'string' || text.length > 1500000) return '';
  const ctx = {
    pieces: [],
    tools: [],
    thinking: false,
    tool(name) { if (!this.tools.includes(name)) this.tools.push(name); },
  };
  let stream = false;
  if (/^data:/m.test(text)) {
    stream = true; // stream fragments concatenate; message blocks join with ' '
    for (const line of text.split('\n')) {
      const m = /^data:\s?(.*)$/.exec(line);
      if (!m) continue;
      const payload = m[1].trim();
      if (!payload || payload === '[DONE]') continue;
      let ev = null;
      try { ev = JSON.parse(payload); } catch (_) { continue; }
      collectText(ev, ctx);
    }
  } else if (/^\s*[{[]/.test(text)) {
    let ev = null;
    try { ev = JSON.parse(text); } catch (_) { ev = null; }
    if (ev) collectText(ev, ctx);
  } else {
    ctx.pieces.push(text); // plain error text and friends
  }
  const joined = stream ? ctx.pieces.join('') : ctx.pieces.join(' ');
  if (joined.trim()) return clip(joined, cap);
  // No text at all: a tool-only turn still says what it did; a thinking-only
  // turn says why there is nothing to preview.
  if (ctx.tools.length) return clip('[tool_use: ' + ctx.tools.join(', ') + ']', cap);
  if (ctx.thinking) return '[thinking]';
  return '';
}

// sessionTimeline lays one session's requests out as a swimlane Gantt: each
// request is a bar from its start (ts) to its end (ts + latency), assigned to
// the first lane where it fits (greedy, chronological — deterministic), with
// a cumulative-token polyline over the top and per-request marks (error
// red, retry/failover amber dot, in-flight accent). Pure geometry/markup:
// colors ride CSS variables, timestamps are formatted by the caller's fmt
// callback (locale stays in app.js), and click targets carry data-id so the
// caller can wire the detail popover. Each bar also carries an aria-label
// from sessionBarSummary (hover chrome belongs to app.js's shared tooltip).
//
// The time axis is SEGMENTED: an idle gap longer than gapMs (default 2 min)
// between consecutive requests breaks the axis, and each activity segment
// gets width ∝ sqrt(duration) — long stretches still dominate, short bursts
// stay readable. Without this a 10-minute burst inside a 7-hour session
// collapses into a 2%-wide fence of bars. A zoom {from,to} window (drag-select
// in app.js) filters the rows to those overlapping it and makes it the axis
// domain; a window that catches nothing falls back to the full view so a
// stray drag never blanks the card.
//
// Rows accept the session panel's merged shape: {requestId, ts (unix ms or
// RFC3339), latencyMs, status, input, output, attempt, inFlight}. Rows
// without a parseable ts are skipped (counted in .skipped). Returns
// {svg, lanes, skipped, segments} — empty svg when fewer than 2 rows remain
// (1 when a zoom window is active); segments is [{t0, t1, x0, x1}] in viewBox
// px so the caller can invert a drag back into time.
export function sessionTimeline(rows, opts) {
  const o = opts || {};
  const W = o.width || 900;
  const padL = 8;
  const padR = 8;
  const axisH = 18;
  const topH = 8;
  const GAP_MS = Number.isFinite(o.gapMs) && o.gapMs > 0 ? o.gapMs : 120000;
  const BREAK_W = 18; // viewBox px reserved per compressed-gap marker
  const toMs = (v) => {
    const n = typeof v === 'number' ? v : Date.parse(v);
    return Number.isFinite(n) ? n : null;
  };
  const norm = [];
  let skipped = 0;
  const byId = new Map(); // original rows, for the bars' aria summaries
  for (const r of rows || []) {
    if (!r) continue;
    if (r.requestId != null) byId.set(String(r.requestId), r);
    const ts = toMs(r.ts);
    if (ts == null) { skipped++; continue; }
    const lat = Number.isFinite(Number(r.latencyMs)) && r.latencyMs > 0 ? Number(r.latencyMs) : 0;
    norm.push({
      id: String(r.requestId || ''),
      ts,
      end: r.inFlight ? null : ts + lat, // null end = still running
      status: Number(r.status) || 0,
      tokens: (Number(r.input) || 0) + (Number(r.output) || 0),
      attempt: Number(r.attempt) || 0,
      inFlight: !!r.inFlight,
    });
  }
  norm.sort((a, b) => a.ts - b.ts);
  if (norm.length < 2) return { svg: '', lanes: 0, skipped, segments: [] };

  let win = null;
  if (o.window && Number.isFinite(o.window.from) && Number.isFinite(o.window.to) && o.window.to > o.window.from) {
    win = { from: Number(o.window.from), to: Number(o.window.to) };
  }
  let vis = norm;
  if (win) {
    vis = norm.filter((n) => (n.end != null ? n.end : n.ts) >= win.from && n.ts <= win.to);
    if (vis.length === 0) { vis = norm; win = null; }
  }
  if (vis.length < (win ? 1 : 2)) return { svg: '', lanes: 0, skipped, segments: [] };

  // Segment the visible rows at long idle gaps; each segment keeps its own
  // time→x mapping (piecewise-linear overall, monotonic).
  const segs = [];
  let cur = null;
  for (const n of vis) {
    const nEnd = n.end != null ? n.end : n.ts;
    if (!cur || n.ts - cur.t1 > GAP_MS) {
      cur = { t0: n.ts, t1: nEnd, items: [] };
      segs.push(cur);
    } else if (nEnd > cur.t1) {
      cur.t1 = nEnd;
    }
    cur.items.push(n);
    n.seg = cur;
  }
  const usable = W - padL - padR;
  const plotW = Math.max(usable - (segs.length - 1) * BREAK_W, 50);
  // sqrt weighting + a floor so single-request bursts keep a readable width;
  // the floor only applies while few segments exist (it must never overflow).
  const floorW = segs.length <= 12 ? 24 : 0;
  const weights = segs.map((s) => Math.sqrt(Math.max(s.t1 - s.t0, 1000)));
  const wsum = weights.reduce((a, b) => a + b, 0);
  let cursor = padL;
  for (let i = 0; i < segs.length; i++) {
    const s = segs[i];
    s.x0 = cursor;
    s.w = Math.max((plotW * weights[i]) / wsum, floorW);
    cursor += s.w + BREAK_W;
  }
  const x = (t) => {
    for (const s of segs) {
      if (t <= s.t1 || s === segs[segs.length - 1]) {
        return s.x0 + ((t - s.t0) / Math.max(s.t1 - s.t0, 1)) * s.w;
      }
    }
    return W - padR;
  };

  // Greedy lane assignment: first lane whose last bar ends at/before start.
  const laneEnds = [];
  for (const n of vis) {
    const nEnd = n.end != null ? n.end : n.ts;
    let lane = laneEnds.findIndex((e) => e <= n.ts);
    if (lane === -1) {
      laneEnds.push(nEnd);
      lane = laneEnds.length - 1;
    } else {
      laneEnds[lane] = nEnd;
    }
    n.lane = lane;
    n.x0 = Math.max(x(n.ts), padL);
    // An in-flight bar runs to the right edge of its segment visually.
    n.x1 = n.end != null ? Math.max(Math.min(x(n.end), W - padR), n.x0 + 2) : n.seg.x0 + n.seg.w;
  }
  const lanes = laneEnds.length;
  // Busy parallel bursts stack many lanes; compress row height past 14 so the
  // card's total height stays bounded instead of dwarfing the table.
  const compact = lanes > 14;
  const laneH = compact ? 12 : 20;
  const barH = compact ? 6 : 11;
  const H = topH + lanes * laneH + axisH;

  // Cumulative tokens polyline (left-bottom → right-top over the bars);
  // omitted entirely when the session recorded no tokens.
  const total = vis.reduce((t, n) => t + n.tokens, 0);
  const points = [];
  let cum = 0;
  for (const n of vis) {
    cum += n.tokens;
    points.push(`${(n.x1).toFixed(1)},${(H - axisH - (total > 0 ? (cum / total) : 0) * (H - axisH - topH)).toFixed(1)}`);
  }

  const bars = vis.map((n) => {
    const cls = ['tl-bar'];
    if (n.status >= 400) cls.push('tl-err');
    else if (n.inFlight) cls.push('tl-run');
    else cls.push('tl-ok');
    const w = Math.max(n.x1 - n.x0, 2);
    const y = topH + n.lane * laneH + (laneH - barH) / 2;
    let mark = '';
    if (n.attempt > 0) {
      mark = `<circle class="tl-retry" cx="${Math.max(n.x0 - 3, 1.5).toFixed(1)}" cy="${(y + barH / 2).toFixed(1)}" r="2.5"><title>attempt ${n.attempt + 1} (failover)</title></circle>`;
    }
    // No native <title> on the bar: app.js's shared hover tooltip owns the
    // visual layer (and adds a lazily fetched response excerpt); the aria
    // label keeps the summary reachable for screen readers.
    const aria = sessionBarSummary(byId.get(n.id), o.fmt).join(' · ') || n.id;
    return `${mark}<rect class="${cls.join(' ')}" data-id="${esc(n.id)}" aria-label="${esc(aria)}" x="${n.x0.toFixed(1)}" y="${y.toFixed(1)}" width="${w.toFixed(1)}" height="${barH}" rx="2"></rect>`;
  }).join('');

  const fmt = o.fmt || ((t) => String(Math.round((t - (win ? win.from : vis[0].ts)) / 1000)) + 's');
  // Ticks label each segment's start (plus the overall end) so compressed
  // gaps cannot make one label stand for a 6-hour stretch; a single segment
  // keeps the classic start/middle/end trio. Narrow segments skip their
  // start label rather than collide with the neighbours.
  const tEnd = segs[segs.length - 1].t1;
  let ticks;
  if (segs.length === 1) {
    const t0 = segs[0].t0;
    ticks = [
      { t: t0, anchor: 'start' },
      { t: t0 + (tEnd - t0) / 2, anchor: 'middle' },
      { t: tEnd, anchor: 'end' },
    ];
  } else {
    ticks = segs
      .filter((s, i) => i === 0 || s.w >= 90)
      .map((s) => ({ t: s.t0, anchor: 'start' }));
    ticks.push({ t: tEnd, anchor: 'end' });
  }
  const tickSvg = ticks.map((tk) => `<text class="tl-tick" x="${x(tk.t).toFixed(1)}" y="${H - 5}" text-anchor="${tk.anchor}">${esc(fmt(tk.t))}</text>`).join('');
  // One dashed divider per compressed gap, centered in its reserved strip.
  const breaks = segs.slice(1).map((s) => {
    const bx = s.x0 - BREAK_W / 2;
    return `<line class="tl-break" x1="${bx.toFixed(1)}" y1="${topH}" x2="${bx.toFixed(1)}" y2="${H - axisH}"><title>idle gap compressed</title></line>`;
  }).join('');

  const line = points.length >= 2 && cum > 0
    ? `<polyline class="tl-tokens" points="${points.join(' ')}"><title>cumulative tokens</title></polyline>`
    : '';
  return {
    svg: `<svg class="tl-svg" viewBox="0 0 ${W} ${H}" role="img" aria-label="session request timeline">${bars}${line}${breaks}${tickSvg}</svg>`,
    lanes,
    skipped,
    segments: segs.map((s) => ({ t0: s.t0, t1: s.t1, x0: s.x0, x1: s.x0 + s.w })),
  };
}

// ---------- unified request table ----------
//
// ONE head + ONE row renderer serve all three request tables — the Requests
// tab, the Live all-live ring, and the Live session view — so the pages stay
// functionally and visually identical; Live just feeds real-time rows.
// Columns: time · agent · session · status · model · provider · ms · tokens.
// The tokens cell appends `· cache N` when a cache read was recorded
// (persisted records carry it; pure live rows show what the end event had).
// opts: rowClass (req-row/live-row + modifiers), liveKey (adds data-live-key),
// modelNote (guard badge), fmtTime (locale stays in app.js).
export function requestTableHeadHTML() {
  return `<thead><tr><th>Time</th><th>Agent</th><th>Session</th><th>Status</th><th>Model</th><th>Provider</th><th class="num">ms</th><th class="num">Tokens In / Out</th></tr></thead>`;
}

// guardMarksHTML renders a request's guard/adjudication annotations (the
// security-audit join riding /api/requests summaries) as compact badges:
// synchronous interceptions (⚑ block), LLM verdicts (judge·high…) and
// unblock trail entries. Titles carry the scrubbed judgment logic /
// attribution. Capped at three badges plus an overflow count so dense rows
// stay readable.
// groupGuardMarks merges the marks of ONE request that share the same
// channel and verdict (kind + verdict) into a single mark with joined
// names — the ROW BADGE idiom: one badge per verdict, counted (`×N`), the
// per-rule reasons joined into the tooltip. A request hitting several rules
// reads as one event per verdict, not one badge per rule.
export function groupGuardMarks(marks) {
  if (!marks || marks.length <= 1) return marks || [];
  const key = (m) => (m.kind || '') + '\u0000' + (m.verdict || '');
  const out = [];
  const idx = new Map();
  for (const m of marks) {
    const k = key(m);
    const i = idx.get(k);
    if (i === undefined) {
      idx.set(k, out.length);
      out.push({ ...m, names: [...(m.names || [])] });
      continue;
    }
    const g = out[i];
    for (const n of m.names || []) {
      if (n && !g.names.includes(n)) g.names.push(n);
    }
    // Per-rule reasons join into the tooltip (newline-separated, deduped).
    if (m.reason && !(g.reason || '').includes(m.reason)) {
      g.reason = g.reason ? g.reason + '\n' + m.reason : m.reason;
    }
    if (!g.model && m.model) g.model = m.model;
    if ((m.ts || 0) > (g.ts || 0)) g.ts = m.ts;
  }
  return out;
}

// guardMarksHTML renders a request's guard/adjudication annotations (the
// security-audit join riding /api/requests summaries) as compact badges,
// ONE per verdict (groupGuardMarks merges the rules of a verdict into a
// counted badge; the per-rule reasons ride the tooltip): synchronous
// interceptions (⚑ block), LLM verdicts (judge·high…) and unblock entries.
// Capped at three badges plus an overflow count so dense rows stay
// readable.
export function guardMarksHTML(marks) {
  const ms = groupGuardMarks(marks);
  if (!ms.length) return '';
  const cls = { high: 'err', medium: 'warn', error: 'warn', low: 'muted', skipped: 'muted' };
  const parts = ms.slice(0, 3).map((m) => {
    const n = (m.names || []).length;
    // Single rule: name it; several rules under one verdict: count them.
    const one = n === 1 ? ' ' + esc(m.names[0]) : '';
    const many = n > 1 ? ` ×${n}` : '';
    if (m.kind === 'unblock') {
      return `<span class="badge ok" title="${esc(m.detail || 'block removed')}">unblocked</span>`;
    }
    // Priority: an interception (action=block) is a BLOCK even when the
    // record carries a verdict — repeat-interception records ride the
    // original verdict as attribution, and rendering them as judge·verdict
    // would hide that this request was rejected.
    if (m.action === 'block') {
      return `<span class="badge err" title="${esc(m.reason || m.detail || '')}">⚑ block${one || many}</span>`;
    }
    if (m.verdict) {
      return `<span class="badge ${cls[m.verdict] || ''}" title="${esc(m.reason || m.evidence || '')}">judge·${esc(m.verdict)}${m.cached ? '·cached' : ''}${many}</span>`;
    }
    return `<span class="badge warn" title="${esc(m.detail || m.reason || '')}">⚑ guard${one || many}</span>`;
  });
  if (ms.length > 3) parts.push(`<span class="badge muted">+${ms.length - 3}</span>`);
  return ' ' + parts.join(' ');
}

export function requestMetaHTML(r, rel) {
  const groups = [];
  const g = (k, v) => {
    if (v == null || v === '') return;
    groups.push(`<div class="req-meta-g"><div class="req-meta-k">${esc(k)}</div><div class="req-meta-v">${v}</div></div>`);
  };
  const when = [esc(r && r.ts)].concat((rel || []).map((x) => esc(x))).filter(Boolean).join(' · ');
  g('when', when);
  if (r && r.method) g('call', `${esc(r.method)} <span class="req-meta-dim">${esc(r.path || '')}</span> · attempt ${Number(r.attempt) || 0}`);
  if (r) {
    let result = statusBadgeHTML(r.status);
    if (Number.isFinite(Number(r.latency_ms)) && r.status != null) {
      result += ` ${fmtNum(r.latency_ms)}ms`;
      if (r.ttft_ms) result += ` <span class="req-meta-dim">· ttft ${fmtNum(r.ttft_ms)}ms</span>`;
    }
    g('result', result);
    const route = [r.provider, r.upstream_model].filter(Boolean).map((x) => esc(x)).join(' / ');
    if (route) g('route', route);
    if (r.request_size || r.response_size) {
      g('size', `req ${fmtNum(r.request_size)} <span class="req-meta-dim">→</span> resp ${fmtNum(r.response_size)}`);
    }
  }
  return `<div class="req-meta">${groups.join('')}</div>`;
}

// guardMarksDetailHTML renders the request's guard/adjudication trail as a
// FLAT extension of the meta strip: ONE row per channel (kind) — the worst
// verdict of the channel leads, per-rule judgment reasons render as their
// own lines beneath when they differ (a single shared reason renders once).
// The full text of every segment rides the row tooltip. Empty string when
// the request has no guard trail.
export function guardMarksDetailHTML(marks) {
  if (!marks || !marks.length) return '';
  const vBadge = { high: 'err', medium: 'warn', low: 'ok', error: 'warn', skipped: 'muted' };
  const vRank = { high: 5, error: 4, medium: 3, skipped: 2, low: 1, '': 0 };
  const byKind = new Map();
  for (const m of marks) {
    if (!byKind.has(m.kind)) byKind.set(m.kind, []);
    byKind.get(m.kind).push(m);
  }
  const rows = [...byKind.entries()].map(([kind, group]) => {
    let head = group[0];
    let newest = group[0];
    for (const m of group) {
      if ((vRank[m.verdict] || 0) > (vRank[head.verdict] || 0)) head = m;
      if ((m.ts || 0) > (newest.ts || 0)) newest = m;
    }
    // An interception leads with the block badge; its verdict (repeat
    // interception carries the original high) rides right beside it.
    let lead;
    if (kind === 'unblock') lead = '<span class="badge ok">unblocked</span>';
    else if (head.action === 'block') lead = '<span class="badge err">⚑ block</span>';
    else if (head.verdict) lead = `<span class="badge ${vBadge[head.verdict] || ''}">${esc(head.verdict)}</span>`;
    else lead = '<span class="badge warn">⚑ hit</span>';
    if (head.action === 'block' && head.verdict) {
      lead += ` <span class="badge ${vBadge[head.verdict] || ''}">${esc(head.verdict)}</span>`;
    }
    const names = [];
    for (const m of group) for (const n of m.names || []) if (!names.includes(n)) names.push(n);
    const trail = [];
    const judgeModel = group.map((m) => m.model).filter(Boolean)[0] || '';
    if (judgeModel) trail.push(`judge ${esc(judgeModel)}`);
    if (group.some((m) => m.cached)) trail.push('<span class="badge muted">cached</span>');
    if (head.action && kind !== 'unblock') trail.push(esc(head.action));
    trail.push(esc(kind));
    const tip = group.flatMap((m) => {
      const who = (m.names || []).join(', ');
      return [m.reason, m.evidence ? `evidence: ${m.evidence}` : '', m.detail]
        .filter(Boolean).map((x) => (who ? `[${who}] ` : '') + x);
    }).join('\n');
    // One visible line per judgment: a single line when every segment of the
    // channel shares verdict+reason, one per rule otherwise.
    const same = group.every((m) => m.verdict === group[0].verdict && m.reason === group[0].reason);
    let lines = '';
    if (same && group[0].reason) {
      lines = `<div class="req-guard-line" title="${esc(tip)}">${esc(group[0].reason)}</div>`;
    } else if (!same) {
      lines = group.filter((m) => m.reason).map((m) =>
        `<div class="req-guard-line" title="${esc(tip)}"><span class="badge ${vBadge[m.verdict] || ''}">${esc(m.verdict || '—')}</span> <code>${esc((m.names || []).join(', '))}</code> ${esc(m.reason)}</div>`).join('');
    }
    return `<div class="req-guard-row"${tip ? ` title="${esc(tip)}"` : ''}>${lead}` +
      (names.length ? `<code>${esc(names.join(', '))}</code>` : '') +
      `<span class="hint">${trail.join(' · ')}</span>` +
      `<span class="req-guard-time">${esc(fmtMsLocal(newest.ts))}</span></div>${lines}`;
  });
  return `<div class="req-meta req-meta-guard"><div class="req-meta-g">` +
    `<div class="req-meta-k">⚑ guard</div><div class="req-meta-v">${rows.join('')}</div></div></div>`;
}

// Local ms formatter for guard marks (app.js's fmtMs is not importable here —
// pure.js is the dependency leaf).
function fmtMsLocal(ts) {
  const n = Number(ts);
  if (!Number.isFinite(n)) return '';
  return new Date(n).toLocaleTimeString();
}

export function requestRowHTML(row, opts) {
  const o = opts || {};
  const r = row || {};
  const dim = r.inFlight ? ' subdue' : '';
  const lat = (r.inFlight || r.latencyMs == null) ? '' : fmtNum(r.latencyMs);
  const slow = !r.inFlight && r.latencyMs != null && r.latencyMs > 10000;
  let tk = '';
  if (!r.inFlight && (Number(r.input) || Number(r.output))) {
    tk = `${fmtNum(r.input)} / ${fmtNum(r.output)}`;
    const cr = Number(r.cacheRead);
    if (cr > 0) {
      // Cache read rides the token cell with its hit share; ≥80% highlights
      // (Portkey-style cache-HIT signal). Two-decimal precision (trailing
      // zeros trimmed by Number): integer rounding shows near-total hits as
      // a misleading "100%".
      const denom = cr + (Number(r.input) || 0);
      const pct = denom > 0 ? Math.round((cr / denom) * 10000) / 100 : null;
      tk += ` <span class="tok-cache${pct != null && pct >= 80 ? ' hot' : ''}">· cache ${fmtNum(cr)}${pct != null ? ` (${pct}%)` : ''}</span>`;
    }
  }
  const sess = r.session
    ? `<td class="mono${dim} session-link" data-session="${esc(r.session)}" title="${esc(r.session)} — view this session">${esc(shortSessionId(r.session))}</td>`
    : `<td class="mono${dim}">—</td>`;
  const t = o.fmtTime ? o.fmtTime(r.ts) : String(r.ts != null ? r.ts : '');
  return `<tr class="${esc(o.rowClass || '')}" data-id="${esc(r.requestId)}"${o.liveKey ? ` data-live-key="${esc(r.requestId)}"` : ''}>
    <td class="mono${dim}">${esc(t)}</td>
    <td class="mono${dim}">${esc(r.agent || '—')}</td>
    ${sess}
    <td class="st">${statusBadgeHTML(r.inFlight ? null : r.status, r.inFlight)}</td>
    <td>${esc(r.model || '—')}${o.modelNote || ''}${guardMarksHTML(r.guardMarks)}</td>
    <td class="mono${dim}">${esc(r.inFlight ? '…' : (r.provider || '—'))}${r.shadow ? ' <span class="badge muted">shadow</span>' : ''}</td>
    <td class="num${slow ? ' warn' : ''}">${lat}</td>
    <td class="num">${tk}</td>
  </tr>`;
}

// ---------- virtualized request table ----------
//
// Pure math for the Requests tab's scroll windowing (DOM side in app.js):
// the table keeps only the rows near the viewport in the DOM and spacer
// rows carry the height of the unloaded gaps, so the page scrollbar always
// reflects the whole loaded list.

// cumulativeOffsets turns per-row heights into running tops: offsets[i] is
// the top of row i and offsets[n] the total height.
export function cumulativeOffsets(heights) {
  const out = [0];
  let acc = 0;
  for (let i = 0; i < heights.length; i += 1) {
    const h = Number(heights[i]);
    acc += Number.isFinite(h) && h > 0 ? h : 0;
    out.push(acc);
  }
  return out;
}

// virtualWindow returns the inclusive {first,last} index range whose rows
// intersect [viewTop - overscan, viewBottom + overscan] (binary search over
// the cumulative offsets), or null when no row is in range — empty list,
// degenerate viewport, or the table scrolled fully past.
export function virtualWindow(offsets, viewTop, viewBottom, overscan) {
  const n = offsets.length - 1;
  if (n <= 0 || !(viewBottom > viewTop)) return null;
  const top = viewTop - (overscan || 0);
  const bottom = viewBottom + (overscan || 0);
  let lo = 0, hi = n - 1, first = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (offsets[mid + 1] > top) { first = mid; hi = mid - 1; } else { lo = mid + 1; }
  }
  if (first === -1) return null;
  lo = first; hi = n - 1;
  let last = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (offsets[mid] < bottom) { last = mid; lo = mid + 1; } else { hi = mid - 1; }
  }
  if (last < first) return null;
  return { first, last };
}

// mergeRecordsPages merges an older page into the loaded newest-first list.
// Keyset pagination refetches the whole boundary SECOND (to= is inclusive at
// second granularity), so duplicate ids drop here; order stays newest-first
// by parsed ts with the already-loaded record winning ties.
export function mergeRecordsPages(loaded, page) {
  const base = loaded || [];
  const seen = new Set();
  for (const r of base) if (r && r.request_id) seen.add(r.request_id);
  const added = [];
  for (const r of page || []) {
    if (!r || !r.request_id || seen.has(r.request_id)) continue;
    seen.add(r.request_id);
    added.push(r);
  }
  if (!added.length) return { records: base, added: 0 };
  const tsOf = (r) => {
    const t = Date.parse(r.ts);
    return Number.isFinite(t) ? t : 0;
  };
  const records = base.concat(added).sort((a, b) => tsOf(b) - tsOf(a));
  return { records, added: added.length };
}

// oldestTsSec is the to= bound for fetching the next older page: the oldest
// loaded record's unix second. That second is re-fetched and deduped by id
// (mergeRecordsPages), so nothing between pages can slip through. Null when
// no record carries a parsable ts.
export function oldestTsSec(records) {
  let min = Infinity;
  for (const r of records || []) {
    const t = Date.parse(r && r.ts);
    if (Number.isFinite(t) && t < min) min = t;
  }
  return Number.isFinite(min) ? Math.floor(min / 1000) : null;
}

// ---------- request detail chat view ----------
//
// The raw JSON / SSE dumps the detail rows used to lead with are unreadable
// for conversation traffic. chatViewHTML parses a logged request/response
// pair into MESSAGE BLOCKS and renders an agent-UI-style transcript:
// role-labeled turns, plain text as text, thinking blocks / tool results /
// long tool arguments collapsed into <details>, usage as a hint line. The
// raw bodies stay available below (the caller keeps its own collapsed
// <details> views). Pure: all text goes through esc(); caps keep one huge
// history from flooding the DOM. Returns '' when nothing presentable can be
// parsed — the caller then shows only the raw views.

// CHAT_TEXT_CAP bounds one rendered text/thinking block; CHAT_ARGS_CAP one
// tool-argument / tool-result payload; CHAT_RECENT is how many trailing
// request turns render expanded (earlier turns live in the lazy history).
const CHAT_TEXT_CAP = 4000;
const CHAT_ARGS_CAP = 2000;
export const CHAT_RECENT = 4;
const CHAT_PARSE_MAX = 1500000;

function parseJSONBounded(text) {
  if (!text || typeof text !== 'string' || text.length > CHAT_PARSE_MAX) return null;
  try { return JSON.parse(text); } catch (_) { return null; }
}

// contentToBlocks normalizes one message content value (string, block array,
// or openai content-part array) into flat block descriptors.
function contentToBlocks(content) {
  const blocks = [];
  if (typeof content === 'string') {
    if (content.trim()) blocks.push({ type: 'text', text: content });
    return blocks;
  }
  if (!Array.isArray(content)) return blocks;
  for (const c of content) {
    if (!c || typeof c !== 'object') continue;
    if (typeof c.text === 'string' && c.text) {
      blocks.push(c.type === 'thinking' ? { type: 'thinking', text: c.text } : { type: 'text', text: c.text });
    } else if (c.type === 'thinking' && typeof c.thinking === 'string') {
      blocks.push({ type: 'thinking', text: c.thinking });
    } else if (c.type === 'tool_use' && c.name) {
      blocks.push({ type: 'tool_use', name: c.name, input: c.input });
    } else if (c.type === 'tool_result') {
      blocks.push({ type: 'tool_result', text: flattenText(c.content) });
    } else if (c.type === 'image' || c.type === 'input_image' || c.type === 'document') {
      blocks.push({ type: 'media', kind: c.type });
    }
  }
  return blocks;
}

// flattenText pulls the text out of nested content values (tool_result
// content, responses parts) into one string.
function flattenText(v) {
  if (typeof v === 'string') return v;
  if (Array.isArray(v)) return v.map(flattenText).filter(Boolean).join('\n');
  if (v && typeof v === 'object') {
    if (typeof v.text === 'string') return v.text;
    if (typeof v.content !== 'undefined') return flattenText(v.content);
  }
  return '';
}

// parseRequestChat walks anthropic/openai `messages[]` (plus `system`) and
// the responses `input` shape into {system, messages:[{role, blocks}]}.
function parseRequestChat(body) {
  if (!body || typeof body !== 'object') return null;
  const system = [];
  const messages = [];
  let sysSrc = null;
  if (typeof body.system === 'string' && body.system.trim()) sysSrc = body.system;
  else if (Array.isArray(body.system)) sysSrc = flattenText(body.system);
  if (sysSrc && sysSrc.trim()) system.push({ type: 'text', text: sysSrc });
  const msgs = Array.isArray(body.messages) ? body.messages
    : (Array.isArray(body.input) ? body.input : null);
  if (!msgs) {
    if (typeof body.input === 'string' && body.input.trim()) {
      messages.push({ role: 'user', blocks: [{ type: 'text', text: body.input }] });
    }
  } else {
    for (const m of msgs) {
      if (!m || typeof m !== 'object') continue;
      const role = m.role === 'system' || m.role === 'developer' ? 'system' : (m.role || 'user');
      const blocks = contentToBlocks(m.content);
      // openai tool results ride as role:"tool" messages with a name field.
      if (role === 'tool') {
        messages.push({ role: 'tool', name: m.name || m.tool_call_id || '', blocks: [{ type: 'tool_result', text: flattenText(m.content) }] });
        continue;
      }
      if (Array.isArray(m.tool_calls)) {
        for (const tc of m.tool_calls) {
          if (tc && tc.function && tc.function.name) {
            let input = null;
            try { input = tc.function.arguments ? JSON.parse(tc.function.arguments) : {}; } catch (_) { input = null; }
            blocks.push({ type: 'tool_use', name: tc.function.name, input });
          }
        }
      }
      if (role === 'system') { for (const b of blocks) system.push(b); continue; }
      if (blocks.length) messages.push({ role, blocks });
    }
  }
  if (!system.length && !messages.length) return null;
  return { system, messages };
}

// foldSSEBlocks folds a logged SSE stream into blocks: anthropic
// content_block events by block index, openai chunk deltas (text +
// tool_calls by index), and the minimal responses-API delta events.
// Returns {blocks, usage} or null when the stream carries nothing usable.
function foldSSEBlocks(text) {
  const blocks = [];       // ordered render list
  const byIdx = new Map(); // per-index accumulators
  const usage = { input: 0, output: 0, cacheRead: 0, cacheCreation: 0 };
  let saw = false;
  const idx = (i, init) => {
    if (!byIdx.has(i)) byIdx.set(i, init());
    return byIdx.get(i);
  };
  for (const line of text.split('\n')) {
    const m = /^data:\s?(.*)$/.exec(line);
    if (!m) continue;
    const payload = m[1].trim();
    if (!payload || payload === '[DONE]') continue;
    let ev = null;
    try { ev = JSON.parse(payload); } catch (_) { continue; }
    if (!ev || typeof ev !== 'object') continue;
    saw = true;
    // anthropic stream
    if (ev.type === 'message_start' && ev.message && ev.message.usage) {
      const u = ev.message.usage;
      usage.input = u.input_tokens || usage.input;
      usage.cacheRead = u.cache_read_input_tokens || usage.cacheRead;
      usage.cacheCreation = u.cache_creation_input_tokens || usage.cacheCreation;
    } else if (ev.type === 'content_block_start' && ev.content_block) {
      const cb = ev.content_block;
      const slot = idx(ev.index, () => ({ block: null }));
      if (cb.type === 'tool_use') slot.block = { type: 'tool_use', name: cb.name || 'tool', json: '' };
      else if (cb.type === 'thinking') slot.block = { type: 'thinking', text: '' };
      else slot.block = { type: 'text', text: '' };
    } else if (ev.type === 'content_block_delta' && ev.delta) {
      const slot = idx(ev.index, () => ({ block: { type: 'text', text: '' } }));
      if (!slot.block) slot.block = { type: 'text', text: '' };
      if (typeof ev.delta.text === 'string') slot.block.text += ev.delta.text;
      else if (typeof ev.delta.thinking === 'string') {
        slot.block.type = 'thinking';
        slot.block.text += ev.delta.thinking;
      } else if (typeof ev.delta.partial_json === 'string') {
        slot.block.type = 'tool_use';
        slot.block.json = (slot.block.json || '') + ev.delta.partial_json;
      }
    } else if (ev.type === 'message_delta' && ev.usage) {
      usage.output = ev.usage.output_tokens || usage.output;
    } else if (ev.error && typeof ev.error.message === 'string') {
      blocks.push({ type: 'error', text: ev.error.message });
    } else if (Array.isArray(ev.choices)) {
      // openai chat chunks
      const ch = ev.choices[0];
      if (ch && ch.delta && typeof ch.delta.content === 'string') {
        const slot = idx(0, () => ({ block: { type: 'text', text: '' } }));
        slot.block.text += ch.delta.content;
      }
      if (ch && ch.delta && Array.isArray(ch.delta.tool_calls)) {
        for (const tc of ch.delta.tool_calls) {
          const slot = idx(1000 + (tc.index || 0), () => ({ block: { type: 'tool_use', name: '', json: '' } }));
          if (tc.function && tc.function.name) slot.block.name += tc.function.name;
          if (tc.function && typeof tc.function.arguments === 'string') slot.block.json += tc.function.arguments;
        }
      }
      if (ev.usage) {
        usage.input = ev.usage.prompt_tokens || usage.input;
        usage.output = ev.usage.completion_tokens || usage.output;
        const pd = ev.usage.prompt_tokens_details || {};
        usage.cacheRead = pd.cached_tokens || usage.cacheRead;
      }
    } else if (ev.type === 'response.output_text.delta' && typeof ev.delta === 'string') {
      const slot = idx(0, () => ({ block: { type: 'text', text: '' } }));
      slot.block.text += ev.delta;
    } else if (ev.type === 'response.output_item.done' && ev.item && ev.item.type === 'function_call' && ev.item.name) {
      blocks.push({ type: 'tool_use', name: ev.item.name, json: ev.item.arguments || '' });
    }
  }
  if (!saw) return null;
  for (const [, slot] of [...byIdx.entries()].sort((a, b) => a[0] - b[0])) {
    const b = slot.block;
    if (!b) continue;
    if (b.type === 'tool_use') blocks.push({ type: 'tool_use', name: b.name || 'tool', json: b.json || '' });
    else if ((b.type === 'text' || b.type === 'thinking') && b.text) blocks.push({ type: b.type, text: b.text });
  }
  return { blocks, usage };
}

// parseResponseChat parses one response body (JSON message, SSE stream, or
// error object) into {blocks, usage}.
function parseResponseChat(text, contentType) {
  if (!text || typeof text !== 'string' || text.length > CHAT_PARSE_MAX) return null;
  if (/^data:/m.test(text) || /event-stream/i.test(String(contentType || ''))) {
    const folded = foldSSEBlocks(text);
    if (folded && folded.blocks.length) return folded;
    return null;
  }
  const body = parseJSONBounded(text);
  if (!body || typeof body !== 'object') return null;
  if (body.error && typeof body.error.message === 'string') {
    return { blocks: [{ type: 'error', text: body.error.message }], usage: null };
  }
  const blocks = [];
  const usage = { input: 0, output: 0, cacheRead: 0, cacheCreation: 0 };
  if (Array.isArray(body.content)) {
    for (const b of contentToBlocks(body.content)) blocks.push(b);
    usage.input = body.usage && body.usage.input_tokens || 0;
    usage.output = body.usage && body.usage.output_tokens || 0;
    usage.cacheRead = body.usage && body.usage.cache_read_input_tokens || 0;
    usage.cacheCreation = body.usage && body.usage.cache_creation_input_tokens || 0;
  } else if (Array.isArray(body.choices)) {
    const ch = body.choices[0] || {};
    const holder = ch.message || {};
    if (typeof holder.content === 'string' && holder.content) blocks.push({ type: 'text', text: holder.content });
    else if (Array.isArray(holder.content)) for (const b of contentToBlocks(holder.content)) blocks.push(b);
    if (Array.isArray(holder.tool_calls)) {
      for (const tc of holder.tool_calls) {
        if (tc && tc.function && tc.function.name) {
          let input = null;
          try { input = tc.function.arguments ? JSON.parse(tc.function.arguments) : {}; } catch (_) { input = null; }
          blocks.push({ type: 'tool_use', name: tc.function.name, input });
        }
      }
    }
    usage.input = body.usage && body.usage.prompt_tokens || 0;
    usage.output = body.usage && body.usage.completion_tokens || 0;
  } else if (Array.isArray(body.output)) {
    for (const o of body.output) {
      if (!o || typeof o !== 'object') continue;
      if (o.type === 'function_call' && o.name) blocks.push({ type: 'tool_use', name: o.name, json: o.arguments || '' });
      if (Array.isArray(o.content)) for (const b of contentToBlocks(o.content)) blocks.push(b);
    }
    usage.input = body.usage && body.usage.input_tokens || 0;
    usage.output = body.usage && body.usage.output_tokens || 0;
  }
  if (!blocks.length) return null;
  return { blocks, usage };
}

// readableValue renders a JSON value as human text instead of JSON syntax:
// strings unquoted, objects as `key: value` lines (nested containers
// indented two spaces per level), arrays as bullet lines — or comma-joined
// when tiny and all-scalar. Tool arguments and JSON tool results go through
// this so the transcript reads like prose, not escaped syntax.
export function readableValue(v, depth = 0) {
  return rvLines(v, depth).map((l) => '  '.repeat(depth) + l).join('\n');
}

// rvLines renders a value as render lines whose FIRST line is unpadded;
// nested lines carry their own indent so callers can prefix the first line
// (`key: `, `- `) without double indentation.
function rvLines(v, depth) {
  if (v === null || v === undefined) return ['null'];
  if (typeof v === 'string') return [v];
  if (typeof v === 'number' || typeof v === 'boolean') return [String(v)];
  if (Array.isArray(v)) {
    if (!v.length) return ['[]'];
    const allScalar = v.every((x) => x === null || typeof x !== 'object');
    if (allScalar) {
      const joined = v.map((x) => (x === null ? 'null' : String(x))).join(', ');
      if (joined.length <= 60) return [joined];
    }
    const lines = [];
    for (const item of v) {
      const child = rvLines(item, depth);
      lines.push('- ' + child[0], ...child.slice(1).map((l) => '  ' + l));
    }
    return lines;
  }
  if (typeof v === 'object') {
    const keys = Object.keys(v);
    if (!keys.length) return ['{}'];
    const lines = [];
    for (const k of keys) {
      const child = rvLines(v[k], depth + 1);
      if (child.length === 1) lines.push(`${k}: ${child[0]}`);
      else lines.push(`${k}:`, ...child.map((l) => '  ' + l));
    }
    return lines;
  }
  return [String(v)];
}

// argsToReadable converts a tool-call argument payload (parsed object or a
// raw JSON string from stream fragments) to readable text.
function argsToReadable(input, json) {
  let parsed = null;
  if (input !== undefined && input !== null) parsed = input;
  else if (json) {
    try { parsed = JSON.parse(json); } catch (_) { return json; }
  }
  if (parsed === null || parsed === undefined) return '';
  return readableValue(parsed, 0);
}

// maybeReadableText renders a tool-result body: JSON goes through
// readableValue, anything else passes through unchanged.
function maybeReadableText(s) {
  const p = parseJSONBounded(s);
  if (p && typeof p === 'object') return readableValue(p, 0);
  return s;
}

// chatBlockHTML renders one block; capText truncates with a visible note.
function capText(s, cap) {
  const t = String(s);
  if (t.length <= cap) return esc(t);
  return esc(t.slice(0, cap)) + `\n… (+${fmtCap(t.length - cap)} chars, see raw body)`;
}
function fmtCap(n) { return n >= 1000 ? Math.round(n / 100) / 10 + 'k' : String(n); }

function chatBlockHTML(b) {
  if (b.type === 'text') {
    return `<div class="cv-text">${capText(b.text, CHAT_TEXT_CAP)}</div>`;
  }
  if (b.type === 'thinking') {
    return `<details class="cv-fold"><summary>thinking</summary><div class="cv-text cv-muted">${capText(b.text, CHAT_TEXT_CAP)}</div></details>`;
  }
  if (b.type === 'tool_use') {
    const args = argsToReadable(b.input, b.json);
    return `<div class="cv-tool"><span class="cv-tool-name">${esc(b.name)}</span><pre class="cv-args">${args ? capText(args, CHAT_ARGS_CAP) : '<i>(no arguments)</i>'}</pre></div>`;
  }
  if (b.type === 'tool_result') {
    const text = b.text ? maybeReadableText(b.text) : '';
    return `<details class="cv-fold"><summary>tool result</summary><pre class="cv-args">${text ? capText(text, CHAT_ARGS_CAP) : '<i>(empty)</i>'}</pre></details>`;
  }
  if (b.type === 'media') {
    return `<div class="cv-chip">[${esc(b.kind)}]</div>`;
  }
  if (b.type === 'error') {
    return `<div class="msg err">${capText(b.text, CHAT_TEXT_CAP)}</div>`;
  }
  return '';
}

function chatMsgHTML(msg) {
  const role = msg.role === 'assistant' ? 'assistant' : (msg.role === 'tool' ? 'tool' : 'user');
  const label = msg.name ? `${role} · ${msg.name}` : role;
  return `<div class="cv-msg cv-${role}"><span class="cv-role">${esc(label)}</span>${msg.blocks.map(chatBlockHTML).join('')}</div>`;
}

// parseChatRequest is the exported parse entry for the history expander:
// app.js parses once per expand and caches the messages array, then renders
// ranges through chatTurnsSliceHTML as the user scrolls.
export function parseChatRequest(requestText) {
  return parseRequestChat(parseJSONBounded(requestText));
}

// chatTurnsSliceHTML renders messages[from, to) with the same rendering as
// the recent window. Bounds clamp; empty range → ''.
export function chatTurnsSliceHTML(messages, from, to) {
  if (!Array.isArray(messages) || !messages.length) return '';
  const a = Math.max(0, Math.floor(from) || 0);
  const b = Math.min(messages.length, Math.floor(to) || 0);
  if (b <= a) return '';
  let html = '';
  for (let i = a; i < b; i++) html += chatMsgHTML(messages[i]);
  return html;
}

// chatViewHTML renders the full transcript for one record: system collapsed,
// the last CHAT_RECENT request turns expanded, earlier turns behind a LAZY
// history fold (opts.histKey names the app-side registry id carrying the
// request text; without it the fold still renders, just with no lazy hook),
// then the response turn and its usage line. Everything text-bearing is
// capped, and the history never enters the DOM until expanded — a
// thousand-turn session opens as fast as a two-turn one.
export function chatViewHTML(requestText, responseText, responseContentType, opts) {
  const o = opts || {};
  const req = parseRequestChat(parseJSONBounded(requestText));
  const res = parseResponseChat(responseText, responseContentType);
  if (!req && !res) return '';
  let html = '<div class="cv">';
  if (req) {
    if (req.system.length) {
      html += `<details class="cv-fold cv-system"><summary>system</summary><div class="cv-text cv-muted">${capText(req.system.map((b) => b.text).join('\n\n'), CHAT_TEXT_CAP)}</div></details>`;
    }
    const n = req.messages.length;
    const cut = Math.max(0, n - CHAT_RECENT);
    if (cut > 0) {
      const attr = o.histKey ? ` data-raw="${esc(o.histKey)}"` : '';
      html += `<details class="cv-fold cv-history"${attr}><summary>${cut} earlier turn${cut === 1 ? '' : 's'}</summary><div class="cv-hist-host"><span class="hint">renders on first expand</span></div></details>`;
    }
    html += req.messages.slice(cut).map(chatMsgHTML).join('');
  }
  if (res) {
    html += `<div class="cv-msg cv-assistant"><span class="cv-role">assistant</span>${res.blocks.map(chatBlockHTML).join('')}`;
    const u = res.usage;
    if (u && (u.input || u.output || u.cacheRead || u.cacheCreation)) {
      const parts = [];
      if (u.input) parts.push(`in ${u.input}`);
      if (u.output) parts.push(`out ${u.output}`);
      if (u.cacheRead) parts.push(`cache read ${u.cacheRead}`);
      if (u.cacheCreation) parts.push(`cache write ${u.cacheCreation}`);
      html += `<div class="cv-usage">${esc(parts.join(' · '))}</div>`;
    }
    html += '</div>';
  }
  html += '</div>';
  return html;
}

// ---------- Requests filter URL-hash state ----------
//
// The Requests tab's filter rides the URL hash (#requests?session=…&agent=…)
// so a refresh or shared link lands on the same view instead of the full
// list. hashQueryParams parses the query portion into a plain object;
// requestsFilterQuery projects the filter onto params (only non-default
// values — an unfiltered tab stays a clean #requests); requestsFilterFromQuery
// reads them back, returning null when the hash carries no filter keys (a
// bare #requests must not clobber an in-memory filter) and dropping junk
// values (shadow keeps its tri-state enum).

export function hashQueryParams(q) {
  const out = {};
  if (!q) return out;
  for (const [k, v] of new URLSearchParams(String(q))) out[k] = v;
  return out;
}

export function requestsFilterQuery(f) {
  if (!f) return '';
  const q = new URLSearchParams();
  if (f.session) q.set('session', f.session);
  if (f.agent) q.set('agent', f.agent);
  if (f.model) q.set('model', f.model);
  if (f.provider) q.set('provider', f.provider);
  if (f.errors) q.set('errors', '1');
  if (f.shadow) q.set('shadow', f.shadow);
  return q.toString();
}

export function requestsFilterFromQuery(params) {
  if (!params) return null;
  const has =
    params.session || params.agent || params.model ||
    params.provider || params.errors || params.shadow;
  if (!has) return null;
  return {
    session: params.session || '',
    agent: params.agent || '',
    model: params.model || '',
    provider: params.provider || '',
    errors: params.errors === '1' || params.errors === 'true',
    shadow: params.shadow === 'only' || params.shadow === 'exclude' ? params.shadow : '',
  };
}
