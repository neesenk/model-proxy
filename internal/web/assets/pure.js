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
    return { hint: 'no data', open: false };
  }
  if (snap.Err) {
    const k = quotaErrKind(snap);
    const hint = k === 'session-expired' ? 'session expired'
      : k === 'not-logged-in' ? 'not logged in' : 'error';
    return { hint, open: true };
  }
  const windows = snap.Windows || [];
  if (windows.length === 0) {
    return { hint: 'unmeasured', open: false };
  }
  const ult = windows.find((w) => w.Ultimate);
  if (ult && ult.RemainingPct != null && ult.RemainingPct >= 0) {
    return { hint: (ult.RemainingPct * 100).toFixed(1) + '% left', open: true };
  }
  if (snap.Plan) {
    return { hint: snap.Plan, open: true };
  }
  return { hint: 'available', open: true };
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
  { id: 'errors', label: 'Errors', axis: 'error %', gap: true },
  { id: 'cost', label: 'Cost', axis: 'USD', gap: true },
];

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
    case 'cost': return p.cost != null ? p.cost : null;
    case 'requests': return Number(p.requests || 0);
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
      provider: s.provider,
      model: s.model,
      requests: Number(t.requests || 0),
      tokens,
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
// columns). Thresholds in seconds; week/month have no upper bound.
const GRAN_LIMITS = {
  minute: { min: 0, max: 2 * 86400 },
  hour: { min: 1 * 3600, max: 31 * 86400 }, // min exclusive (see analyticsGranAllowed)
  day: { min: 2 * 86400, max: 400 * 86400 },
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
  return [
    { id: 'auto', label: 'auto', allowed: true },
    ...['minute', 'hour', 'day', 'week', 'month'].map((id) => ({
      id, label: id, allowed: analyticsGranAllowed(spanSec, id),
    })),
  ];
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

// securityLegendHTML renders the collapsible kind/action legend.
export function securityLegendHTML() {
  const kinds = SECURITY_KIND_HELP.map((k) => `<li><span class="badge">${esc(k.name)}</span> ${esc(k.text)}</li>`).join('');
  const actions = SECURITY_ACTION_HELP.map((a) => `<li><code>${esc(a.name)}</code> — ${esc(a.text)}</li>`).join('');
  return `<details class="sec-legend"><summary>What do these records mean?</summary>` +
    `<ul>${kinds}</ul><div class="hint">action:</div><ul>${actions}</ul>` +
    `<div class="hint">Matched content is never stored in the audit log (deliberate). Use analyze on a row to re-scan the original request and locate each hit.</div></details>`;
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

// securityExplainHTML renders one /api/security/explain result: per located
// match a header (name + strength badge + explanation + regex/source) and a
// context window with the hit highlighted. The window arrives pre-split
// (pre/hit/post strings) — never byte offsets, which would not survive the
// Go-bytes → JS-UTF-16 boundary. Every interpolated value is escaped.
export function securityExplainHTML(result) {
  if (!result || typeof result !== 'object') return '';
  const note = SECURITY_EXPLAIN_STATUS_NOTES[result.status];
  let out = note ? `<div class="msg hint">${esc(note)}</div>` : '';
  const matches = Array.isArray(result.matches) ? result.matches : [];
  if (!matches.length && !note) return '<div class="msg hint">Nothing to show.</div>';
  for (const m of matches) {
    const strength = m.strength ? ` <span class="badge ${m.strength === 'strong' ? 'warn' : 'muted'}">${esc(m.strength)}</span>` : '';
    const rule = m.regex ? `<div class="hint">regex <code>${esc(m.regex)}</code>${m.source ? ` · source: ${esc(m.source)}` : ''}</div>` : '';
    const explanation = m.explanation ? `<div class="hint">${esc(m.explanation)}</div>` : '';
    let body = '';
    if (m.located && (m.pre !== undefined || m.hit !== undefined)) {
      body = `<pre class="log-pre body-pre sec-snippet">${esc(m.pre)}<mark>${esc(m.hit)}</mark>${esc(m.post)}</pre>`;
    } else if (!m.located) {
      body = '<div class="hint">not re-located in the stored request body</div>';
    }
    out += `<div class="sec-explain-match"><div><code>${esc(m.name)}</code>${strength}</div>${explanation}${rule}${body}</div>`;
  }
  return out;
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
