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

// analyticsChartSeries turns /api/analytics `series` into the arrays uPlot
// needs: x is the sorted union of bucket timestamps in unix SECONDS (uPlot's
// time scale unit — passing milliseconds renders year 58655), and ys[i] is one
// value per bucket for series[i]. kind='cost' keeps null for unpriced points
// (uPlot draws a gap); kind='tokens' sums input+output (the cache buckets stay
// on the Status page). Returns {x, ys, labels}.
export function analyticsChartSeries(series, kind) {
  const list = Array.isArray(series) ? series : [];
  const x = [...new Set(list.flatMap((s) => (s.points || []).map((p) => p.bucket)))].sort((a, b) => a - b);
  const labels = [];
  const ys = [];
  for (const s of list) {
    labels.push(`${s.provider}/${s.model}`);
    const by = Object.fromEntries((s.points || []).map((p) => [p.bucket, p]));
    ys.push(x.map((t) => {
      const p = by[t];
      if (!p) return kind === 'cost' ? null : 0;
      if (kind === 'cost') return p.cost != null ? p.cost : null;
      return (p.input || 0) + (p.output || 0);
    }));
  }
  return { x, ys, labels };
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
