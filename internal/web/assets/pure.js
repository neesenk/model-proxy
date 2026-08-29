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
