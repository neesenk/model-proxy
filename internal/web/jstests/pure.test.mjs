// pure.test.mjs — behavioral unit tests for assets/pure.js, run via
// `node --test jstests/` (driven by TestWebAssetsPureJSUnitTests in
// assets_test.go). Pure functions only: no DOM, fixed `now` injections,
// explicit locales — fully deterministic.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  esc, fmtNum, avgLatencyMs, hasReset, fmtDur, untilHuman,
  YAML_EDITOR_MIN_HEIGHT, visibleYamlEditorHeight,
  verdictBadge, modelCapMatrix, providerFrozen, providerNames,
  cacheHitRate, settingsDiff, settingsRestartKeys,
  TOKEN_RANGES, tokenRangeBounds, tokenRangeLabel, tokensRangeQuery,
  tokenCustomBounds, parseLocalDate,
  WEEKDAYS, monthTitle, calendarMonthGrid, twoMonthWindow, shiftMonth, ymd,
  isFutureDay, rangePick, customRangeLabel, tokenRangeTriggerLabel,
  prettyJSON, formatJSONLoose, jsonToHTML, parseSSE, isSSE, highlightJSON, splitLinesByBudget, linkedModels,
  sessionsForAgent, linkedAgents,
  fmtGuardDetail, fmtProgressBytes, analyticsChartSeries, analyticsPointValue, analyticsTableRows,
  ANALYTICS_METRICS, pctDelta, analyticsGranularity, analyticsGranOptions, analyticsValueText,
  modelHealthFromSeries, modelHealthGrade, MODEL_HEALTH_DIMS, liveSessionSummary,
  fmtCompact,
  mergeLiveAndPersistedRow, shouldFetchDetail, detailFetchState,
  quotaErrKind, accountUsageState,
  pathStrengthFromAction, securityLegendHTML, securityExplainHTML, SECURITY_EXPLAIN_STATUS_NOTES,
  POPUP_OPEN_SEL, INTERACTIVE_CONTROL_SEL, refreshHoldReason, staleDataText,
} from '../assets/pure.js';

test('esc escapes all five HTML-significant chars', () => {
  assert.equal(esc(`<a href="x" class='y'>&</a>`), '&lt;a href=&quot;x&quot; class=&#39;y&#39;&gt;&amp;&lt;/a&gt;');
});

test('esc neutralizes an attribute-injection payload', () => {
  const hostile = `"><img src=x onerror=alert(1)>`;
  const out = esc(hostile);
  assert.ok(!out.includes('<img'), `payload survived: ${out}`);
  assert.equal(out, '&quot;&gt;&lt;img src=x onerror=alert(1)&gt;');
});

test('esc maps nullish to empty string and stringifies values', () => {
  assert.equal(esc(null), '');
  assert.equal(esc(undefined), '');
  assert.equal(esc(0), '0');
  assert.equal(esc(false), 'false');
});

test('esc passes plain text (incl. CJK) through unchanged', () => {
  assert.equal(esc('hello 世界'), 'hello 世界');
});

test('fmtNum renders en-US grouping and maps nullish to 0', () => {
  assert.equal(fmtNum(null), '0');
  assert.equal(fmtNum(undefined), '0');
  assert.equal(fmtNum(0), '0');
  assert.equal(fmtNum(1234567), '1,234,567');
});

test('avgLatencyMs divides summed latency by request count, rounded', () => {
  assert.equal(avgLatencyMs(null), 0);
  assert.equal(avgLatencyMs({}), 0);
  assert.equal(avgLatencyMs({ requests: 0, latency_ms_sum: 500 }), 0);
  assert.equal(avgLatencyMs({ requests: 3, latency_ms_sum: 100 }), 33);
  assert.equal(avgLatencyMs({ requests: 2, latency_ms_sum: 101 }), 51);
});

test('hasReset treats Go zero time and garbage as "no reset"', () => {
  assert.equal(hasReset(''), false);
  assert.equal(hasReset(null), false);
  assert.equal(hasReset('not-a-date'), false);
  // Go time.Time zero sentinel: year 1 — truthy string, must NOT count.
  assert.equal(hasReset('0001-01-01T00:00:00Z'), false);
  assert.equal(hasReset('2026-08-29T00:00:00.000Z'), true);
});

test('fmtDur buckets seconds into trimmed human units', () => {
  assert.equal(fmtDur(null), '—');
  assert.equal(fmtDur(-1), '—');
  assert.equal(fmtDur(NaN), '—');
  assert.equal(fmtDur(0), '0s');
  assert.equal(fmtDur(59), '59s');
  assert.equal(fmtDur(60), '1m 0s');
  assert.equal(fmtDur(3661), '1h 1m');
  assert.equal(fmtDur(90000), '1d 1h'); // 25h → days bucket
});

test('untilHuman renders remaining duration for future timestamps only', () => {
  const now = Date.parse('2026-08-29T00:00:00.000Z');
  assert.equal(untilHuman('', now), '');
  assert.equal(untilHuman('garbage', now), '');
  assert.equal(untilHuman('2026-08-28T00:00:00.000Z', now), ''); // past
  assert.equal(untilHuman('2026-08-29T02:00:00.000Z', now), ' (in 2h 0m)');
  assert.equal(untilHuman('2026-08-31T00:00:00.000Z', now), ' (in 2d 0h)');
});

test('providerFrozen covers operator freeze, circuit, and rate-limit cooldown', () => {
  const now = Date.parse('2026-08-29T00:00:00.000Z');
  // No health entry (healthy provider; health is created lazily) → not frozen.
  assert.equal(providerFrozen(null, now), false);
  assert.equal(providerFrozen(undefined, now), false);
  assert.equal(providerFrozen({}, now), false);
  // Operator freeze wins on its own — no circuit, no cooldown needed.
  assert.equal(providerFrozen({ frozen: true, available: false }, now), true);
  assert.equal(providerFrozen({ frozen: false }, now), false);
  // Circuit open / half-open counts; closed does not.
  assert.equal(providerFrozen({ circuit_state: 'open' }, now), true);
  assert.equal(providerFrozen({ circuit_state: 'half_open' }, now), true);
  assert.equal(providerFrozen({ circuit_state: 'closed' }, now), false);
  // Rate-limit cooldown only while still in the future.
  assert.equal(providerFrozen({ rate_limited_until: '2026-08-29T01:00:00.000Z' }, now), true);
  assert.equal(providerFrozen({ rate_limited_until: '2026-08-28T23:00:00.000Z' }, now), false);
  assert.equal(providerFrozen({ rate_limited_until: 'garbage' }, now), false);
  // A lapsed cooldown + closed circuit renders not frozen even with an entry.
  assert.equal(providerFrozen({ circuit_state: 'closed', rate_limited_until: '2026-08-28T00:00:00.000Z' }, now), false);
});

test('YAML_EDITOR_MIN_HEIGHT is the documented 480px floor', () => {
  assert.equal(YAML_EDITOR_MIN_HEIGHT, 480);
});

test('visibleYamlEditorHeight uses remaining space but never under the floor', () => {
  // Tall viewport: remaining space wins.
  assert.equal(visibleYamlEditorHeight(800, 100, 50), 650);
  // Short viewport: the hard minimum wins over a smaller remainder.
  assert.equal(visibleYamlEditorHeight(500, 100, 50), 480);
  // Negative editor top (element above viewport) clamps to 0.
  assert.equal(visibleYamlEditorHeight(800, -50, 50), 750);
  // Fractional remaining space floors to whole pixels.
  assert.equal(visibleYamlEditorHeight(801, 100, 50), 651);
});

test('verdictBadge maps yes/no to concluded pills, everything else to unknown', () => {
  assert.deepEqual(verdictBadge('yes'), { cls: 'ok', glyph: '✓', label: 'yes' });
  assert.deepEqual(verdictBadge('no'), { cls: 'err', glyph: '✗', label: 'no' });
  // unknown (probe pending / retry next pass) must render distinctly from no.
  const unknown = verdictBadge('unknown');
  assert.equal(unknown.cls, 'muted');
  assert.equal(unknown.glyph, '?');
  assert.equal(unknown.label, 'unknown');
  assert.notEqual(unknown.cls, verdictBadge('no').cls);
  // Missing/garbage verdicts degrade to unknown, never to a concluded state.
  assert.equal(verdictBadge('').cls, 'muted');
  assert.equal(verdictBadge(undefined).cls, 'muted');
  assert.equal(verdictBadge(null).cls, 'muted');
  assert.equal(verdictBadge('YES').cls, 'muted');
});

test('modelCapMatrix sorts providers and models, normalizing fields', () => {
  const matrix = modelCapMatrix({
    zeta: {
      fingerprint: 'fp-z', probed_at: '2026-09-01T10:00:00Z',
      models: {
        'm-b': { chat: 'yes', anthropic: 'no', responses: 'unknown' },
        'm-a': { chat: 'no', anthropic: 'no', responses: 'no' },
      },
    },
    alpha: { fingerprint: 'fp-a', probed_at: '2026-09-01T09:00:00Z', models: {} },
  });
  assert.deepEqual(matrix.map((p) => p.name), ['alpha', 'zeta']);
  assert.deepEqual(matrix[1].models.map((m) => m.id), ['m-a', 'm-b']);
  assert.deepEqual(matrix[1].models[1], { id: 'm-b', chat: 'yes', anthropic: 'no', responses: 'unknown' });
  assert.deepEqual(matrix[1].fingerprint, 'fp-z');
  assert.deepEqual(matrix[1].probedAt, '2026-09-01T10:00:00Z');
  // A probed provider with no recorded models keeps an empty list.
  assert.deepEqual(matrix[0], { name: 'alpha', fingerprint: 'fp-a', probedAt: '2026-09-01T09:00:00Z', models: [] });
});

test('modelCapMatrix tolerates empty and malformed providers maps', () => {
  assert.deepEqual(modelCapMatrix(null), []);
  assert.deepEqual(modelCapMatrix(undefined), []);
  assert.deepEqual(modelCapMatrix({}), []);
  assert.deepEqual(modelCapMatrix({ up: null }),
    [{ name: 'up', fingerprint: '', probedAt: '', models: [] }]);
  // A model entry without verdict fields surfaces undefined legs, which
  // verdictBadge renders as unknown.
  const [p] = modelCapMatrix({ up: { models: { m: null } } });
  assert.deepEqual(p.models, [{ id: 'm', chat: undefined, anthropic: undefined, responses: undefined }]);
});

test('providerNames keeps frozen/unavailable providers listed via the health union', () => {
  // Regression: renderProvidersCard used to enumerate names ONLY from the
  // schedule preview's ordered chains — but scheduling drops unavailable
  // targets (operator-frozen, circuit-open, rate-limit cooldown) from
  // ordered, so freezing a provider made its row (and unfreeze button)
  // vanish from the Providers card.
  const models = {
    glm: { ordered: [{ provider: 'zhipu' }] }, // deepseek frozen → dropped from ordered
    k2: { ordered: [{ provider: 'kimi-code' }, { provider: 'zhipu' }] },
  };
  const health = { deepseek: { frozen: true, available: false } };
  assert.deepEqual(providerNames(models, health), ['deepseek', 'kimi-code', 'zhipu']);
  // A provider present in both sources appears exactly once.
  assert.deepEqual(providerNames(models, { zhipu: { available: true } }), ['kimi-code', 'zhipu']);
  // Health-only enumeration (no schedule) and empty inputs behave.
  assert.deepEqual(providerNames({}, health), ['deepseek']);
  assert.deepEqual(providerNames(null, null), []);
  assert.deepEqual(providerNames({ glm: {} }, {}), []);
  // Ordered entries without a provider field are ignored.
  assert.deepEqual(providerNames({ glm: { ordered: [null, {}, { provider: 'a' }] } }), ['a']);
});

test('cacheHitRate derives the hit percentage from exact-cache counters', () => {
  assert.equal(cacheHitRate(5, 13), '27.8%');
  assert.equal(cacheHitRate(0, 7), '0.0%');
  assert.equal(cacheHitRate(9, 0), '100.0%');
});

test('cacheHitRate returns an em dash before the first lookup', () => {
  // A fresh daemon has hits=0 misses=0 — no lookups, no rate to speak of.
  assert.equal(cacheHitRate(0, 0), '—');
  assert.equal(cacheHitRate(undefined, undefined), '—');
  assert.equal(cacheHitRate(null, null), '—');
});

test('settingsDiff returns only changed fields, grouped by edit kind', () => {
  const loaded = {
    scheduling: { circuit_threshold: 3, sticky_dwell: '10m' },
    cache: { enabled: true, ttl: '10m', max_entries: 1000 },
  };
  const current = {
    scheduling: { circuit_threshold: '3', sticky_dwell: '2m' },
    cache: { enabled: true, ttl: '10m', max_entries: '6000' },
  };
  assert.deepEqual(settingsDiff(loaded, current), {
    scheduling: { sticky_dwell: '2m' },
    cache: { max_entries: '6000' },
  });
});

test('settingsDiff normalizes form strings against JSON numbers and booleans', () => {
  const loaded = { request_log: { enabled: false, max_file_size: 1073741824 } };
  const current = { request_log: { enabled: false, max_file_size: '1073741824' } };
  assert.deepEqual(settingsDiff(loaded, current), {});
});

test('settingsDiff turns a cleared field into an explicit null (delete key)', () => {
  const loaded = { scheduling: { circuit_threshold: 5, quality_error_weight: 0 } };
  const current = { scheduling: { circuit_threshold: '', quality_error_weight: '' } };
  assert.deepEqual(settingsDiff(loaded, current), {
    scheduling: { circuit_threshold: null, quality_error_weight: null },
  });
});

test('settingsDiff treats unset loaded values as empty and skips blank fields', () => {
  const loaded = { stats: { db_path: null, retention: '' } };
  const current = { stats: { db_path: '', retention: '' } };
  assert.deepEqual(settingsDiff(loaded, current), {});
});

test('settingsDiff reports an unchecked box that was unset as no change', () => {
  const loaded = { cache: { enabled: false } };
  const current = { cache: { enabled: false } };
  assert.deepEqual(settingsDiff(loaded, current), {});
});

test('settingsDiff reports flipping a checkbox', () => {
  const loaded = { cache: { enabled: false } };
  const current = { cache: { enabled: true } };
  assert.deepEqual(settingsDiff(loaded, current), { cache: { enabled: true } });
});

const RESTART_GROUPS = [
  { kind: 'general', fields: [{ key: 'log_level', restart: true }, { key: 'log_file', restart: true }] },
  { kind: 'scheduling', fields: [{ key: 'sticky_dwell' }, { key: 'quota_poll_interval', restart: true }] },
  { kind: 'request_log', restart: true, fields: [{ key: 'enabled' }, { key: 'dir' }] },
  { kind: 'cache', fields: [{ key: 'enabled' }, { key: 'ttl' }] },
];

test('settingsRestartKeys flags field-level restart-only keys', () => {
  assert.deepEqual(settingsRestartKeys({ general: { log_level: 'debug' } }, RESTART_GROUPS), ['general.log_level']);
  assert.deepEqual(settingsRestartKeys({ scheduling: { quota_poll_interval: '1m' } }, RESTART_GROUPS), ['scheduling.quota_poll_interval']);
});

test('settingsRestartKeys flags every changed field of a group-level restart block', () => {
  assert.deepEqual(
    settingsRestartKeys({ request_log: { enabled: true, dir: '~/r' } }, RESTART_GROUPS),
    ['request_log.enabled', 'request_log.dir'],
  );
});

test('settingsRestartKeys ignores hot-reloadable changes', () => {
  assert.deepEqual(settingsRestartKeys({ cache: { enabled: true, ttl: '5m' } }, RESTART_GROUPS), []);
  assert.deepEqual(settingsRestartKeys({ scheduling: { sticky_dwell: '2m' } }, RESTART_GROUPS), []);
  assert.deepEqual(settingsRestartKeys({}, RESTART_GROUPS), []);
});

test('tokenRangeBounds resolves rolling presets against now', () => {
  // 2026-03-01 10:30:15 LOCAL (constructed via the Date API, so the test is
  // timezone-independent). Rolling presets end at now.
  const now = new Date(2026, 2, 1, 10, 30, 15).getTime();
  const sec = (ms) => Math.floor(ms / 1000);
  assert.deepEqual(tokenRangeBounds('1h', now), { from: sec(now - 3600e3), to: sec(now) });
  assert.deepEqual(tokenRangeBounds('7d', now), { from: sec(now - 7 * 86400e3), to: sec(now) });
  assert.deepEqual(tokenRangeBounds('30d', now), { from: sec(now - 30 * 86400e3), to: sec(now) });
  assert.equal(tokenRangeBounds('all', now), null);
  assert.equal(tokenRangeBounds('bogus', now), null);
});

test('tokenRangeBounds aligns day/month presets to local boundaries incl. rollovers', () => {
  const sec = (ms) => Math.floor(ms / 1000);
  // 2026-03-01 10:30 LOCAL — yesterday/lastmonth roll over the month boundary
  // (Feb 2026 has 28 days).
  const now = new Date(2026, 2, 1, 10, 30, 0).getTime();
  assert.deepEqual(tokenRangeBounds('today', now),
    { from: sec(new Date(2026, 2, 1).getTime()), to: sec(now) });
  assert.deepEqual(tokenRangeBounds('yesterday', now),
    { from: sec(new Date(2026, 1, 28).getTime()), to: sec(new Date(2026, 2, 1).getTime()) - 1 });
  assert.deepEqual(tokenRangeBounds('month', now),
    { from: sec(new Date(2026, 2, 1).getTime()), to: sec(now) });
  assert.deepEqual(tokenRangeBounds('lastmonth', now),
    { from: sec(new Date(2026, 1, 1).getTime()), to: sec(new Date(2026, 2, 1).getTime()) - 1 });
  // Year rollover: Jan 15 2026 → last month is Dec 2025; leap year: Mar 1
  // 2028 → yesterday is Feb 29 2028.
  const jan = new Date(2026, 0, 15, 12, 0, 0).getTime();
  assert.deepEqual(tokenRangeBounds('lastmonth', jan),
    { from: sec(new Date(2025, 11, 1).getTime()), to: sec(new Date(2026, 0, 1).getTime()) - 1 });
  assert.deepEqual(tokenRangeBounds('yesterday', jan),
    { from: sec(new Date(2026, 0, 14).getTime()), to: sec(new Date(2026, 0, 15).getTime()) - 1 });
  const leap = new Date(2028, 2, 1, 9, 0, 0).getTime();
  assert.deepEqual(tokenRangeBounds('yesterday', leap),
    { from: sec(new Date(2028, 1, 29).getTime()), to: sec(new Date(2028, 2, 1).getTime()) - 1 });
});

test('parseLocalDate parses date-input values as LOCAL days and rejects garbage', () => {
  const d = parseLocalDate('2026-09-08');
  assert.equal(d.getFullYear(), 2026);
  assert.equal(d.getMonth(), 8);
  assert.equal(d.getDate(), 8);
  assert.equal(d.getHours(), 0); // local midnight, not UTC-shifted
  assert.equal(parseLocalDate('2026-02-30'), null); // impossible day must not roll over
  assert.equal(parseLocalDate('2026-2-8'), null);
  assert.equal(parseLocalDate('garbage'), null);
  assert.equal(parseLocalDate(''), null);
  assert.equal(parseLocalDate(undefined), null);
});

test('tokenCustomBounds closes over inclusive full local days', () => {
  const b = tokenCustomBounds('2026-09-01', '2026-09-05');
  assert.equal(b.from, Math.floor(new Date(2026, 8, 1).getTime() / 1000));
  assert.equal(b.to, Math.floor(new Date(2026, 8, 5).getTime() / 1000) + 86399); // 23:59:59
  // Same-day range is valid; inverted or incomplete input is null.
  assert.ok(tokenCustomBounds('2026-09-05', '2026-09-05'));
  assert.equal(tokenCustomBounds('2026-09-05', '2026-09-01'), null);
  assert.equal(tokenCustomBounds('', '2026-09-05'), null);
  assert.equal(tokenCustomBounds('2026-09-01', ''), null);
});

test('tokensRangeQuery builds from/to params; null means do-not-fetch', () => {
  const now = new Date(2026, 2, 1, 10, 30, 0).getTime();
  assert.equal(tokensRangeQuery({ preset: 'all' }, now), '');
  const today = tokensRangeQuery({ preset: 'today' }, now);
  assert.equal(today, `?from=${Math.floor(new Date(2026, 2, 1).getTime() / 1000)}&to=${Math.floor(now / 1000)}`);
  // Unknown presets degrade to the cumulative default, never a 400.
  assert.equal(tokensRangeQuery({ preset: 'fortnight' }, now), '');
  // Custom: valid → closed range; incomplete/inverted → null (caller must not
  // fire a request — the server would 400 from > to).
  const custom = tokensRangeQuery({ preset: 'custom', customStart: '2026-02-01', customEnd: '2026-02-28' }, now);
  assert.equal(custom,
    `?from=${Math.floor(new Date(2026, 1, 1).getTime() / 1000)}&to=${Math.floor(new Date(2026, 1, 28).getTime() / 1000) + 86399}`);
  assert.equal(tokensRangeQuery({ preset: 'custom', customStart: '', customEnd: '' }, now), null);
  assert.equal(tokensRangeQuery({ preset: 'custom', customStart: '2026-02-28', customEnd: '2026-02-01' }, now), null);
});

test('tokenRangeLabel renders presets, custom ranges, and the incomplete state', () => {
  assert.deepEqual(TOKEN_RANGES.map((w) => w.value),
    ['all', '1h', 'today', 'yesterday', '7d', '30d', 'month', 'lastmonth', 'custom']);
  assert.equal(tokenRangeLabel({ preset: 'all' }), 'All Time');
  assert.equal(tokenRangeLabel({ preset: '30d' }), 'Last 30d');
  assert.equal(tokenRangeLabel({ preset: 'bogus' }), 'All Time');
  assert.equal(tokenRangeLabel({ preset: 'custom', customStart: '2026-09-01', customEnd: '2026-09-05' }),
    '2026-09-01 – 2026-09-05');
  assert.equal(tokenRangeLabel({ preset: 'custom', customStart: '', customEnd: '' }), 'Custom…');
});

test('calendarMonthGrid aligns weeks Sunday-first with blank edge cells', () => {
  // Feb 2026 starts on a Sunday and has 28 days: exactly 4 full weeks, no
  // blank cells at all.
  const feb = calendarMonthGrid(2026, 1);
  assert.equal(feb.length, 4);
  assert.deepEqual(feb[0], [1, 2, 3, 4, 5, 6, 7]);
  assert.deepEqual(feb[3], [22, 23, 24, 25, 26, 27, 28]);
  // Jan 2026 starts on a Thursday: 4 leading nulls; Jan 31 is a Saturday, so
  // the last week is full (no trailing blanks).
  const jan = calendarMonthGrid(2026, 0);
  assert.deepEqual(jan[0], [null, null, null, null, 1, 2, 3]);
  assert.deepEqual(jan[jan.length - 1], [25, 26, 27, 28, 29, 30, 31]);
  // Leap year: Feb 2028 has 29 days (Feb 1 2028 is a Tuesday).
  const leap = calendarMonthGrid(2028, 1);
  assert.deepEqual(leap[0], [null, null, 1, 2, 3, 4, 5]);
  assert.equal(leap.flat().filter(Boolean).length, 29);
});

test('twoMonthWindow pairs consecutive months across the year boundary', () => {
  assert.deepEqual(twoMonthWindow(2026, 7), [{ year: 2026, month: 7 }, { year: 2026, month: 8 }]);
  assert.deepEqual(twoMonthWindow(2026, 11), [{ year: 2026, month: 11 }, { year: 2027, month: 0 }]);
});

test('shiftMonth normalizes across year boundaries in both directions', () => {
  assert.deepEqual(shiftMonth(2026, 0, -1), { year: 2025, month: 11 });
  assert.deepEqual(shiftMonth(2026, 11, 1), { year: 2027, month: 0 });
  assert.deepEqual(shiftMonth(2026, 5, 0), { year: 2026, month: 5 });
});

test('ymd zero-pads and sorts chronologically as a string', () => {
  assert.equal(ymd(2026, 7, 3), '2026-08-03');
  assert.equal(ymd(2026, 11, 31), '2026-12-31');
  assert.ok(ymd(2026, 0, 9) < ymd(2026, 0, 10));
  assert.ok(ymd(2025, 11, 31) < ymd(2026, 0, 1));
});

test('isFutureDay disables days after today only', () => {
  const now = new Date(2026, 8, 8, 15, 30).getTime(); // Sep 8 2026 15:30 local
  assert.equal(isFutureDay(2026, 8, 8, now), false);  // today is selectable
  assert.equal(isFutureDay(2026, 8, 7, now), false);
  assert.equal(isFutureDay(2026, 8, 9, now), true);   // tomorrow is not
  assert.equal(isFutureDay(2026, 9, 1, now), true);   // next month is not
  assert.equal(isFutureDay(2025, 11, 31, now), false);
});

test('rangePick runs the start → end (with swap) → restart state machine', () => {
  // First click: partial pick, nothing applied yet.
  assert.deepEqual(rangePick(null, '2026-09-03'),
    { pick: '2026-09-03', start: null, end: null, complete: false });
  // Second click at/after the start completes the range.
  assert.deepEqual(rangePick('2026-09-03', '2026-09-05'),
    { pick: null, start: '2026-09-03', end: '2026-09-05', complete: true });
  // Same-day second click is a valid one-day range.
  assert.deepEqual(rangePick('2026-09-03', '2026-09-03'),
    { pick: null, start: '2026-09-03', end: '2026-09-03', complete: true });
  // An earlier second click swaps so start ≤ end.
  assert.deepEqual(rangePick('2026-09-05', '2026-09-03'),
    { pick: null, start: '2026-09-03', end: '2026-09-05', complete: true });
  // After a completed/discarded range the caller passes null: next click
  // starts a NEW range instead of extending the old one.
  assert.deepEqual(rangePick(null, '2026-08-01'),
    { pick: '2026-08-01', start: null, end: null, complete: false });
});

test('monthTitle renders deterministic English month names', () => {
  assert.equal(monthTitle(2026, 7), 'August 2026');
  assert.equal(monthTitle(2026, 0), 'January 2026');
});

test('customRangeLabel compacts the applied range for the trigger', () => {
  assert.equal(customRangeLabel('2026-08-03', '2026-09-05'), 'Aug 3 – Sep 5');
  assert.equal(customRangeLabel('2026-08-03', '2026-08-03'), 'Aug 3 – Aug 3');
  // Defensive: unparseable values pass through raw.
  assert.equal(customRangeLabel('garbage', '2026-09-05'), 'garbage – Sep 5');
});

test('tokenRangeTriggerLabel shows the preset or the compact custom range', () => {
  assert.equal(tokenRangeTriggerLabel({ preset: 'all' }), 'All Time');
  assert.equal(tokenRangeTriggerLabel({ preset: 'today' }), 'Today');
  assert.equal(tokenRangeTriggerLabel({ preset: 'custom', customStart: '2026-08-03', customEnd: '2026-09-05' }),
    'Aug 3 – Sep 5');
  assert.equal(tokenRangeTriggerLabel({ preset: 'custom', customStart: '', customEnd: '' }), 'Custom…');
});

test('prettyJSON indents objects and arrays, rejects non-JSON', () => {
  assert.equal(prettyJSON('{"a":1}'), '{\n  "a": 1\n}');
  assert.equal(prettyJSON('[1,2]'), '[\n  1,\n  2\n]');
  assert.equal(prettyJSON('  {"a":1}  '), '{\n  "a": 1\n}');
  assert.equal(prettyJSON('not json'), null);
  assert.equal(prettyJSON('42'), null);
  assert.equal(prettyJSON('"str"'), null);
  assert.equal(prettyJSON('null'), null);
  assert.equal(prettyJSON(''), null);
  assert.equal(prettyJSON(null), null);
});

test('formatJSONLoose re-indents without validation, tolerates truncation', () => {
  // Valid compact JSON gets structural indentation identical in shape to a
  // strict pretty print.
  assert.equal(formatJSONLoose('{"a":1,"b":{"c":[true,null]}}'),
    '{\n  "a": 1,\n  "b": {\n    "c": [\n      true,\n      null\n    ]\n  }\n}');
  // Truncated mid-string (request_log max_body_bytes cut): no throw, the valid
  // prefix keeps its structure and the partial string just ends.
  const truncated = formatJSONLoose('{"a":"hello wor');
  assert.ok(truncated.startsWith('{\n  "a": "hello wor'), truncated);
  // JSON-shaped but invalid separators still produce indented lines.
  assert.ok(formatJSONLoose('{"a":1,,"b":2}').includes('\n  "b": 2\n'));
  // Braces inside strings must not change depth.
  assert.equal(formatJSONLoose('{"x":"a{b}"}'), '{\n  "x": "a{b}"\n}');
  // Non-JSON-shaped input is rejected.
  assert.equal(formatJSONLoose('plain text'), null);
  assert.equal(formatJSONLoose(''), null);
  assert.equal(formatJSONLoose(null), null);
  assert.equal(formatJSONLoose('42'), null);
});

test('jsonToHTML highlights keys, strings, numbers and literals', () => {
  const html = jsonToHTML('{"name":"ada","n":3,"ok":true,"x":null}');
  assert.ok(html.includes('<span class="j-key">&quot;name&quot;</span>: <span class="j-str">&quot;ada&quot;</span>'), html);
  assert.ok(html.includes('<span class="j-num">3</span>'), html);
  assert.ok(html.includes('<span class="j-lit">true</span>'), html);
  assert.ok(html.includes('<span class="j-lit">null</span>'), html);
  assert.equal(jsonToHTML('plain text'), null);
});

test('jsonToHTML escapes hostile content before wrapping tokens', () => {
  const html = jsonToHTML('{"x":"<img src=x onerror=alert(1)>"}');
  assert.ok(!html.includes('<img'), html);
  assert.ok(html.includes('&lt;img'), html);
});

test('parseSSE groups events by blank line and joins multi-line data', () => {
  const events = parseSSE('event: message\ndata: {"a":1}\n\ndata: [DONE]\n\n');
  assert.deepEqual(events, [
    { event: 'message', data: '{"a":1}', id: '', retry: '' },
    { event: '', data: '[DONE]', id: '', retry: '' },
  ]);
  const multiline = parseSSE('data: line1\ndata: line2\n\n');
  assert.equal(multiline[0].data, 'line1\nline2');
});

test('parseSSE ignores comments and returns [] for non-SSE', () => {
  assert.deepEqual(parseSSE(': keepalive\n\ndata: x\n\n')[0].data, 'x');
  assert.deepEqual(parseSSE('{"a":1}'), []);
  assert.deepEqual(parseSSE(''), []);
});

test('isSSE trusts the content-type and otherwise requires a data field', () => {
  assert.equal(isSSE('anything', 'text/event-stream; charset=utf-8'), true);
  assert.equal(isSSE('data: {"a":1}\n\n', ''), true);
  assert.equal(isSSE('event: x\ndata: y\n\n', ''), true);
  assert.equal(isSSE('{"a":1}', 'application/json'), false);
  assert.equal(isSSE('plain text', ''), false);
});

test('highlightJSON tokenizes pre-pretty JSON and escapes non-string input', () => {
  assert.equal(highlightJSON('{\n  "a": 1\n}'), '{\n  <span class="j-key">&quot;a&quot;</span>: <span class="j-num">1</span>\n}');
  assert.equal(highlightJSON(null), '');
});

test('splitLinesByBudget chunks at line boundaries and reassembles exactly', () => {
  const text = 'aaa\nbbb\nccc\nddd\n';
  const chunks = splitLinesByBudget(text, 5);
  assert.equal(chunks.join(''), text);
  assert.ok(chunks.every((c) => c.length <= 5), JSON.stringify(chunks));
  assert.deepEqual(chunks, ['aaa\n', 'bbb\n', 'ccc\n', 'ddd\n']);
});

test('splitLinesByBudget keeps an over-long line intact in its own chunk', () => {
  const chunks = splitLinesByBudget('x'.repeat(10), 4);
  assert.equal(chunks.join(''), 'x'.repeat(10));
  assert.deepEqual(chunks, ['x'.repeat(10)]);
});

test('splitLinesByBudget never splits a line (surrogate pairs stay intact)', () => {
  const emoji = '😀😀😀';
  const chunks = splitLinesByBudget(emoji, 3);
  assert.equal(chunks.join(''), emoji);
  assert.deepEqual(chunks, [emoji]);
});

test('splitLinesByBudget handles empty and non-positive budgets', () => {
  assert.deepEqual(splitLinesByBudget('', 10), []);
  assert.deepEqual(splitLinesByBudget('abc', 0), ['abc']);
  assert.deepEqual(splitLinesByBudget('abc', -1), ['abc']);
});

const LINKED_PROVIDER_MODELS = {
  zhipu: ['glm-5.3', 'glm-5.2'],
  deepseek: ['deepseek-v4-pro'],
};
const LINKED_ROUTES = {
  'glm-5.3': [{ provider: 'zhipu', model: 'glm-5.3' }, { provider: 'deepseek', model: 'deepseek-v4-pro' }],
  'kimi-k3': [{ provider: 'zhipu', model: 'glm-5.3' }],
};

test('linkedModels narrows to the selected provider (models + exposed routes)', () => {
  assert.deepEqual(
    linkedModels('zhipu', LINKED_PROVIDER_MODELS, LINKED_ROUTES),
    ['glm-5.2', 'glm-5.3', 'kimi-k3'],
  );
  assert.deepEqual(
    linkedModels('deepseek', LINKED_PROVIDER_MODELS, LINKED_ROUTES),
    ['deepseek-v4-pro', 'glm-5.3'],
  );
});

test('linkedModels with no provider unions every model and route name', () => {
  assert.deepEqual(
    linkedModels('', LINKED_PROVIDER_MODELS, LINKED_ROUTES),
    ['deepseek-v4-pro', 'glm-5.2', 'glm-5.3', 'kimi-k3'],
  );
});

test('linkedModels tolerates missing/empty inputs', () => {
  assert.deepEqual(linkedModels('', {}, {}), []);
  assert.deepEqual(linkedModels('ghost', LINKED_PROVIDER_MODELS, LINKED_ROUTES), []);
  assert.deepEqual(linkedModels('zhipu', null, null), []);
});

test('linkedModels matches a partial provider name case-insensitively', () => {
  assert.deepEqual(
    linkedModels('ZHI', LINKED_PROVIDER_MODELS, LINKED_ROUTES),
    ['glm-5.2', 'glm-5.3', 'kimi-k3'],
  );
});

// The Requests page links the agent and session filters through each session
// summary's `agents` list (GET /api/sessions).
const LINKED_SESSIONS = [
  { session_id: 'sess-c', agents: ['pi', 'codex'] },
  { session_id: 'sess-b', agents: ['codex'] },
  { session_id: 'sess-a', agents: ['claude-code'] },
  { session_id: 'sess-old', agents: [] },
];

test('sessionsForAgent narrows the session list to the selected agent, keeping recency order', () => {
  assert.deepEqual(
    sessionsForAgent('codex', LINKED_SESSIONS).map((s) => s.session_id),
    ['sess-c', 'sess-b'],
  );
  assert.deepEqual(sessionsForAgent('claude-code', LINKED_SESSIONS).map((s) => s.session_id), ['sess-a']);
});

test('sessionsForAgent with no agent offers every session, including agent-less ones', () => {
  assert.deepEqual(
    sessionsForAgent('', LINKED_SESSIONS).map((s) => s.session_id),
    ['sess-c', 'sess-b', 'sess-a', 'sess-old'],
  );
});

test('sessionsForAgent excludes agent-less sessions once an agent is picked and tolerates empty input', () => {
  assert.deepEqual(sessionsForAgent('pi', LINKED_SESSIONS).map((s) => s.session_id), ['sess-c']);
  assert.deepEqual(sessionsForAgent('ghost', LINKED_SESSIONS), []);
  assert.deepEqual(sessionsForAgent('codex', null), []);
  assert.deepEqual(sessionsForAgent('', null), []);
  assert.deepEqual(sessionsForAgent('codex', [{ session_id: 'x' }]), []);
});

test('linkedAgents pins the agent options to the selected session', () => {
  assert.deepEqual(linkedAgents('sess-a', LINKED_SESSIONS, ['claude-code', 'codex', 'pi']), ['claude-code']);
  assert.deepEqual(linkedAgents('sess-c', LINKED_SESSIONS, ['claude-code', 'codex', 'pi']), ['codex', 'pi']);
});

test('linkedAgents falls back to the full facet for no/unknown/agent-less sessions', () => {
  const facet = ['claude-code', 'codex', 'pi'];
  assert.deepEqual(linkedAgents('', LINKED_SESSIONS, facet), facet);
  // Selected but aged out of /api/sessions, or a session with no agent
  // recorded: keep every option rather than collapsing to a dead end.
  assert.deepEqual(linkedAgents('sess-gone', LINKED_SESSIONS, facet), facet);
  assert.deepEqual(linkedAgents('sess-old', LINKED_SESSIONS, facet), facet);
  assert.deepEqual(linkedAgents('sess-a', LINKED_SESSIONS, null), ['claude-code']);
  assert.deepEqual(linkedAgents('', null, null), []);
});

test('linkedAgents and sessionsForAgent return copies (callers splice the option arrays)', () => {
  const facet = ['codex'];
  const out = linkedAgents('', LINKED_SESSIONS, facet);
  out.push('mutated');
  assert.deepEqual(facet, ['codex']);
  const pinned = linkedAgents('sess-c', LINKED_SESSIONS, facet);
  pinned.push('mutated');
  assert.deepEqual(LINKED_SESSIONS[0].agents, ['pi', 'codex']);
  const sessions = sessionsForAgent('', LINKED_SESSIONS);
  sessions.push({ session_id: 'mutated' });
  assert.equal(LINKED_SESSIONS.length, 4);
});

test('fmtGuardDetail humanizes paths and secrets guard details', () => {
  assert.equal(fmtGuardDetail('paths=proxy_creds action=log'), 'sensitive paths: proxy_creds · action: log');
  assert.equal(fmtGuardDetail('secrets=aws,github action=block'), 'secrets: aws,github · action: block');
});

test('fmtGuardDetail leaves unknown details intact and trims', () => {
  assert.equal(fmtGuardDetail('  budget monthly_usd=10  '), 'budget monthly_usd=10');
  assert.equal(fmtGuardDetail(''), '');
  assert.equal(fmtGuardDetail(null), '');
  assert.equal(fmtGuardDetail(undefined), '');
});

test('fmtProgressBytes renders byte counts compactly', () => {
  assert.equal(fmtProgressBytes(0), '0 B');
  assert.equal(fmtProgressBytes(12), '12 B');
  assert.equal(fmtProgressBytes(1536), '1.5 KiB');
  assert.equal(fmtProgressBytes(2097152), '2 MiB');
  assert.equal(fmtProgressBytes(1073741824), '1 GiB');
  assert.equal(fmtProgressBytes(null), '0 B');
  assert.equal(fmtProgressBytes(undefined), '0 B');
});

test('analyticsChartSeries uses unix seconds and reads server token totals', () => {
  const series = [
    { provider: 'aqp', model: 'glm', points: [
      { bucket: 1788796800, tokens: 15, cost: 0.5 },
      { bucket: 1788883200, tokens: 5, cost: null },
    ] },
    { provider: 'zhipu', model: 'glm', points: [
      { bucket: 1788883200, tokens: 8, cost: 0.25 },
    ] },
  ];
  const tok = analyticsChartSeries(series, 'tokens');
  assert.deepEqual(tok.x, [1788796800, 1788883200]);
  assert.deepEqual(tok.labels, ['aqp/glm', 'zhipu/glm']);
  assert.deepEqual(tok.ys, [[15, 5], [0, 8]]);
});

test('analyticsChartSeries keeps null cost for unpriced and missing buckets', () => {
  const series = [
    { provider: 'aqp', model: 'glm', points: [
      { bucket: 1788796800, cost: 0.5 },
      { bucket: 1788883200, cost: null },
    ] },
    { provider: 'zhipu', model: 'glm', points: [
      { bucket: 1788883200, cost: 0.25 },
    ] },
  ];
  const cost = analyticsChartSeries(series, 'cost');
  assert.deepEqual(cost.x, [1788796800, 1788883200]);
  assert.deepEqual(cost.ys, [[0.5, null], [null, 0.25]]);
});

test('analyticsChartSeries spans the caller grid: zero-fill counts, gap rates', () => {
  const series = [
    { provider: 'aqp', model: 'glm', points: [
      { bucket: 200, requests: 3, tokens: 15, avg_latency_ms: 0, err_pct: 0 },
    ] },
  ];
  // Window grid covers 100..300 but only bucket 200 has traffic.
  const tok = analyticsChartSeries(series, 'tokens', [100, 200, 300]);
  assert.deepEqual(tok.x, [100, 200, 300]);
  assert.deepEqual(tok.ys, [[0, 15, 0]]); // count metric zero-fills
  const lat = analyticsChartSeries(series, 'latency', [100, 200, 300]);
  // Bucket 200 carries avg_latency_ms 0 with requests → 0; empty buckets
  // stay null ("no data", not 0ms).
  assert.deepEqual(lat.ys, [[null, 0, null]]);
  const err = analyticsChartSeries(series, 'errors', [100, 200, 300]);
  assert.deepEqual(err.ys, [[null, 0, null]]); // server 0% with requests, gaps around
  // Buckets outside the grid (e.g. rounding at the window edge) still show.
  const union = analyticsChartSeries(series, 'tokens', [150]);
  assert.deepEqual(union.x, [150, 200]);
});

test('analyticsChartSeries tolerates empty/missing series', () => {
  assert.deepEqual(analyticsChartSeries([], 'tokens'), { x: [], ys: [], labels: [] });
  assert.deepEqual(analyticsChartSeries(null, 'cost'), { x: [], ys: [], labels: [] });
  assert.deepEqual(analyticsChartSeries([{ provider: 'a', model: 'b' }], 'tokens'), { x: [], ys: [[]], labels: ['a/b'] });
});

test('analyticsChartSeries derives the gap/count metrics and agent labels', () => {
  const series = [
    { agent: 'codex', provider: 'aqp', model: 'glm', points: [
      // Server-derived fields: 50% err, 300ms latency, 75% cache hit.
      { bucket: 100, requests: 2, tokens: 24, err_pct: 50, avg_latency_ms: 300, avg_ttft_ms: 30, cache_hit_pct: 75 },
      // No requests → rate/latency are null ("no data"), not zero.
      { bucket: 200, requests: 0, tokens: 0, err_pct: null, avg_latency_ms: 0, avg_ttft_ms: 0, cache_hit_pct: null },
    ] },
    { provider: 'zhipu', model: 'glm', points: [
      { bucket: 100, requests: 4, tokens: 16, err_pct: 0, avg_latency_ms: 100, avg_ttft_ms: 10, cache_hit_pct: 0 },
    ] },
  ];
  const errs = analyticsChartSeries(series, 'errors');
  assert.deepEqual(errs.labels, ['codex/glm', 'zhipu/glm']);
  assert.deepEqual(errs.ys, [[50, null], [0, null]]); // missing bucket gaps too
  const lat = analyticsChartSeries(series, 'latency');
  assert.deepEqual(lat.ys, [[300, null], [100, null]]);
  const ttft = analyticsChartSeries(series, 'ttft');
  assert.deepEqual(ttft.ys, [[30, null], [10, null]]);
  const cache = analyticsChartSeries(series, 'cache');
  // server cache_hit_pct; zero read → 0%, not null
  assert.deepEqual(cache.ys, [[75, null], [0, null]]);
  const reqs = analyticsChartSeries(series, 'requests');
  assert.deepEqual(reqs.ys, [[2, 0], [4, 0]]); // count metrics zero-fill
});

test('analyticsPointValue reads server fields and never fabricates', () => {
  assert.equal(analyticsPointValue({ cost: null }, 'cost'), null);
  assert.equal(analyticsPointValue(null, 'tokens'), null);
  assert.equal(analyticsPointValue({ err_pct: null }, 'errors'), null);
  assert.equal(analyticsPointValue({ cache_hit_pct: null }, 'cache'), null);
  // tokens is the server's four-bucket total (a count: absent → 0).
  assert.equal(analyticsPointValue({ tokens: 14 }, 'tokens'), 14);
  assert.equal(analyticsPointValue({}, 'tokens'), 0);
  assert.equal(analyticsPointValue({ err_pct: 25 }, 'errors'), 25);
  assert.equal(analyticsPointValue({ cache_hit_pct: 75 }, 'cache'), 75);
  // tok/s: the server's OUTPUT-decode-speed field (output ÷ full call
  // seconds); null without call time.
  assert.equal(analyticsPointValue({ tok_sec: 5 }, 'toksec'), 5);
  assert.equal(analyticsPointValue({ tok_sec: null }, 'toksec'), null);
  assert.equal(analyticsPointValue({ requests: 1, avg_latency_ms: 1200, tok_sec: 25 }, 'toksec'), 25);
});

test('ANALYTICS_METRICS covers exactly the chart metric ids', () => {
  assert.deepEqual(ANALYTICS_METRICS.map((m) => m.id),
    ['tokens', 'toksec', 'cache', 'latency', 'ttft', 'requests', 'errors', 'cost']);
  for (const m of ANALYTICS_METRICS) {
    assert.ok(m.label && m.axis, `metric ${m.id} needs label+axis`);
    assert.equal(typeof m.gap, 'boolean', `metric ${m.id} needs a gap flag`);
  }
});

test('analyticsTableRows reads the server series totals block', () => {
  // The server folds each series into the same unified derived block as the
  // window totals; analyticsTableRows is a thin reader over it.
  const rows = analyticsTableRows([
    { provider: 'aqp', model: 'glm', totals: {
      // 10k four-bucket tokens priced $0.03 → $3.00 per 1M billable tokens.
      requests: 4, tokens: 10000, err_pct: 50, avg_latency_ms: 200, avg_ttft_ms: 20,
      tok_sec: 1000 / 45, cache_hit_pct: 6500 / 9000 * 100, cost: 0.03,
    } },
    { agent: 'codex', provider: 'zhipu', model: 'glm', totals: {
      requests: 2, tokens: 200, err_pct: 0, avg_latency_ms: 200, avg_ttft_ms: 20,
      tok_sec: 62.5, cache_hit_pct: 100 / 3, cost: null,
    } },
  ]);
  assert.equal(rows.length, 2);
  const aqp = rows[0];
  assert.equal(aqp.label, 'aqp/glm');
  assert.equal(aqp.requests, 4);
  assert.equal(aqp.tokens, 10000);
  assert.equal(aqp.errPct, 50);
  assert.equal(aqp.latencyMs, 200);
  assert.equal(aqp.ttftMs, 20);
  assert.equal(aqp.tokSec, 1000 / 45);
  assert.equal(aqp.cost, 0.03);
  // Blended unit price over the four-bucket billable volume.
  assert.equal(aqp.costPerMTok, 3);
  const codex = rows[1];
  assert.equal(codex.label, 'codex/glm'); // agent dimension labels by agent
  assert.equal(codex.cost, null);
  assert.equal(codex.costPerMTok, null);
  assert.equal(codex.tokSec, 62.5);
  assert.ok(Math.abs(codex.cachePct - 100 / 3) < 1e-9);
  assert.deepEqual(analyticsTableRows(null), []);
  // A series without the block degrades to zeros/nulls (never NaN).
  const idle = analyticsTableRows([{ provider: 'p', model: 'm' }])[0];
  assert.equal(idle.requests, 0);
  assert.equal(idle.tokens, 0);
  assert.equal(idle.errPct, null);
  assert.equal(idle.latencyMs, null);
  assert.equal(idle.tokSec, null);
  assert.equal(idle.cost, null);
});

test('analyticsValueText formats per metric and null-safes', () => {
  assert.equal(analyticsValueText('tokens', 30268), '30.3K');
  assert.equal(analyticsValueText('requests', 999), '999');
  assert.equal(analyticsValueText('cost', 0.01234), '$0.0123');
  assert.equal(analyticsValueText('errors', 12.25), '12.3%');
  assert.equal(analyticsValueText('cache', 0), '0.0%');
  assert.equal(analyticsValueText('latency', 1234.5), '1235ms');
  assert.equal(analyticsValueText('ttft', 320.4), '320ms');
  assert.equal(analyticsValueText('toksec', 43.25), '43.3 t/s');
  assert.equal(analyticsValueText('tokens', null), '—');
  assert.equal(analyticsValueText('tokens', NaN), '—');
  assert.equal(analyticsValueText('unknown-metric', 1500), '1.5K');
});

test('modelHealthFromSeries scores latency/ttft/tok-s, worst first', () => {
  // /api/analytics series shape: requests-weighted per-bucket averages (avg
  // × requests rebuilds each bucket's sums, same as the folding in
  // analyticsTableRows).
  const series = [
    // Healthy: fast ttft, solid decode speed (long calls at ~67 out-tok/s
    // still fail the latency dim — same tradeoff the old Model Health card
    // showed).
    { provider: 'zhipu', model: 'glm-5.3', points: [
      { bucket: 100, requests: 10, failures: 0, input: 40000, output: 20000, avg_ttft_ms: 800, avg_duration_ms: 30000 },
    ] },
    // Degraded: 6s ttft, 9s calls, ~11 out-tok/s.
    { provider: 'aqp', model: 'slow-model', points: [
      { bucket: 100, requests: 4, failures: 0, input: 100, output: 100, avg_ttft_ms: 6000, avg_duration_ms: 9000 },
    ] },
  ];
  const rows = modelHealthFromSeries(series);
  assert.equal(rows.length, 2);
  assert.equal(rows[0].provider, 'aqp'); // worst first
  assert.equal(rows[0].label, 'aqp/slow-model');
  assert.equal(rows[0].grade, 'err');
  assert.equal(rows[0].ttftMs, 6000);
  assert.equal(rows[0].latencyMs, 9000);
  assert.equal(rows[0].dims.ttft.grade, 'err');
  assert.equal(rows[0].dims.latency.grade, 'err');
  assert.equal(rows[0].dims.toksec.grade, 'err');
  const good = rows[1];
  assert.equal(good.grade, 'ok');
  assert.equal(good.ttftMs, 800);
  assert.equal(good.latencyMs, 30000);
  assert.equal(good.tokSec, 20000 / 300); // 20k output tokens / 300s
  assert.equal(good.dims.latency.grade, 'err'); // 30s calls fail latency even at 200 tok/s
});

test('modelHealthFromSeries falls back to header latency and tolerates empty input', () => {
  // No duration recorded yet: latency (header time) becomes the call-time
  // fallback for both the latency dimension and tok/s.
  const rows = modelHealthFromSeries([
    { provider: 'p', model: 'm', points: [
      { bucket: 1, requests: 2, failures: 1, input: 300, output: 100, avg_latency_ms: 4000, avg_ttft_ms: 1000 },
    ] },
  ]);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].latencyMs, 4000);
  assert.equal(rows[0].tokSec, 12.5); // 100 output tokens over the 8s summed call time
  assert.equal(rows[0].errPct, 50);
  assert.ok(rows[0].dims.toksec.grade); // dimension graded from fallback data
  // Aggregates several points into one series row.
  const multi = modelHealthFromSeries([
    { provider: 'p', model: 'm', points: [
      { bucket: 1, requests: 1, input: 10, output: 0, avg_latency_ms: 1000 },
      { bucket: 2, requests: 3, input: 30, output: 0, avg_latency_ms: 3000 },
    ] },
  ]);
  assert.equal(multi.length, 1);
  assert.equal(multi[0].requests, 4);
  assert.equal(multi[0].latencyMs, 2500); // (1s + 9s) / 4 requests
  // Degenerate inputs.
  assert.deepEqual(modelHealthFromSeries(null), []);
  assert.deepEqual(modelHealthFromSeries([{ provider: 'p', model: 'm', points: [{ requests: 0 }] }]), []);
});

test('MODEL_HEALTH_DIMS thresholds bracket the grade boundaries', () => {
  assert.deepEqual(MODEL_HEALTH_DIMS.map((d) => d.id), ['latency', 'ttft', 'toksec']);
  for (const d of MODEL_HEALTH_DIMS) assert.ok(d.weight > 0);
  assert.equal(modelHealthGrade(0.75), 'ok');
  assert.equal(modelHealthGrade(0.5), 'warn');
  assert.equal(modelHealthGrade(0.1), 'err');
  assert.equal(modelHealthGrade(null), null);
});

test('pctDelta is null on undefined comparisons, one decimal otherwise', () => {
  assert.equal(pctDelta(null, 5), null);
  assert.equal(pctDelta(5, null), null);
  assert.equal(pctDelta(5, 0), null); // +∞% is noise, not signal
  assert.equal(pctDelta(110, 100), 10);
  assert.equal(pctDelta(95, 100), -5);
  assert.equal(pctDelta(100.4, 100), 0.4);
  assert.equal(pctDelta(100, 100), 0);
});

test('analyticsGranularity maps auto by span and gates explicit choices', () => {
  const H = 3600, D = 86400;
  // auto: finest granularity that stays a few hundred buckets.
  assert.equal(analyticsGranularity(H, 'auto'), 'minute');
  assert.equal(analyticsGranularity(6 * H, 'auto'), 'minute');
  assert.equal(analyticsGranularity(10 * H, 'auto'), 'hour');
  assert.equal(analyticsGranularity(7 * D, 'auto'), 'hour');
  assert.equal(analyticsGranularity(30 * D, 'auto'), 'day');
  assert.equal(analyticsGranularity(90 * D, 'auto'), 'day');
  assert.equal(analyticsGranularity(365 * D, 'auto'), 'week');
  // Explicit choices win when the span allows them…
  assert.equal(analyticsGranularity(30 * D, 'week'), 'week');
  assert.equal(analyticsGranularity(10 * H, 'minute'), 'minute');
  // …and are overridden by auto's pick when they don't (a 1h window has no
  // hour view; minute over a month would explode the point count).
  assert.equal(analyticsGranularity(H, 'hour'), 'minute');
  assert.equal(analyticsGranularity(30 * D, 'minute'), 'day');
  assert.equal(analyticsGranularity(7 * D, 'week'), 'hour');
});

test('analyticsGranOptions gates each granularity by window span', () => {
  const H = 3600, D = 86400;
  const ids = (span) => analyticsGranOptions(span).filter((o) => o.allowed).map((o) => o.id);
  assert.deepEqual(ids(H), ['auto', 'minute']); // "小时范围只有分钟"
  assert.deepEqual(ids(20 * H), ['auto', 'minute', 'hour']);
  assert.deepEqual(ids(7 * D), ['auto', 'hour', 'day']);
  assert.deepEqual(ids(30 * D), ['auto', 'hour', 'day', 'week', 'month']);
  assert.deepEqual(ids(365 * D), ['auto', 'day', 'week', 'month']);
  // Every option stays present (stable layout), just disabled.
  assert.equal(analyticsGranOptions(H).length, 6);
  assert.ok(analyticsGranOptions(H).some((o) => o.id === 'month' && !o.allowed));
});

test('fmtCompact renders K/M units with trimmed decimals', () => {
  assert.equal(fmtCompact(0), '0');
  assert.equal(fmtCompact(999), '999');
  assert.equal(fmtCompact(1500), '1.5K');
  assert.equal(fmtCompact(26268), '26.3K');
  assert.equal(fmtCompact(30000), '30K');
  assert.equal(fmtCompact(1200000), '1.2M');
  assert.equal(fmtCompact(2500000), '2.5M');
  assert.equal(fmtCompact(null), '0');
  // decimals=2 (the KPI chip): two places, trailing zeros still trimmed.
  assert.equal(fmtCompact(30268, 2), '30.27K');
  assert.equal(fmtCompact(1500, 2), '1.5K');
  assert.equal(fmtCompact(1234567, 2), '1.23M');
  assert.equal(fmtCompact(999999, 2), '1000K');
  assert.equal(fmtCompact(undefined), '0');
});

test('liveSessionSummary folds rows + persisted aggregate', () => {
  const rows = [
    { status: 200, latencyMs: 100, model: 'glm', provider: 'zhipu', input: 10, output: 2 },
    { status: 500, latencyMs: 300, model: 'glm', provider: 'aqp', input: 5, output: 1 },
  ];
  const agg = {
    requests: 12, errors: 3, shadow_requests: 1,
    usage: { Input: 1000, Output: 200, CacheRead: 40, CacheCreation: 8 },
    models: ['glm'], providers: ['zhipu', 'aqp'], cost_usd: 0.5,
  };
  const s = liveSessionSummary(rows, agg);
  assert.equal(s.requests, 12);
  assert.equal(s.errors, 3);
  assert.equal(s.input, 1000);
  assert.equal(s.output, 200);
  assert.equal(s.cacheRead, 40);
  assert.equal(s.cacheCreation, 8);
  assert.equal(s.avgLatencyMs, 200);
  assert.deepEqual(s.models, ['glm']);
  assert.deepEqual(s.providers, ['aqp', 'zhipu']);
  assert.equal(s.cost, 0.5);
  assert.equal(s.liveRows, 2);
});

test('liveSessionSummary falls back to rows when no aggregate', () => {
  const rows = [
    { status: 200, latencyMs: 50, model: 'b', provider: 'p1', input: 3, output: 1, cacheRead: 1, cacheCreation: 0 },
    { status: 429, latencyMs: 0, model: 'a', provider: 'p1', input: 0, output: 0, cacheRead: 0, cacheCreation: 0 },
  ];
  const s = liveSessionSummary(rows, null);
  assert.equal(s.requests, 2);
  assert.equal(s.errors, 1);
  assert.equal(s.input, 3);
  assert.equal(s.output, 1);
  assert.equal(s.cacheRead, 1);
  assert.equal(s.cacheCreation, 0);
  assert.equal(s.avgLatencyMs, 50);
  assert.deepEqual(s.models, ['a', 'b']);
  assert.deepEqual(s.providers, ['p1']);
  assert.equal(s.cost, null);
});

test('liveSessionSummary tolerates empty input', () => {
  const s = liveSessionSummary([], undefined);
  assert.deepEqual(s, {
    requests: 0, errors: 0, shadow: 0, input: 0, output: 0, cacheRead: 0, cacheCreation: 0,
    avgLatencyMs: null, models: [], providers: [], cost: null, liveRows: 0,
  });
});

test('shouldFetchDetail treats a recorded error as terminal (no 404 loop)', () => {
  // Fresh ended row: fetch.
  assert.equal(shouldFetchDetail(false, undefined, false), true);
  assert.equal(shouldFetchDetail(false, { error: '' }, false), true);
  // Already resolved, loading, streaming, or failed: do not re-fetch.
  assert.equal(shouldFetchDetail(true, undefined, false), false);
  assert.equal(shouldFetchDetail(false, { loading: true }, false), false);
  assert.equal(shouldFetchDetail(false, { error: 'no record for request id x' }, false), false);
  assert.equal(shouldFetchDetail(false, { notLogged: true }, false), false);
  assert.equal(shouldFetchDetail(false, undefined, true), false);
  // Clearing the error (explicit re-open) allows a retry.
  assert.equal(shouldFetchDetail(false, { loading: false, error: '' }, false), true);
  assert.equal(shouldFetchDetail(false, { loading: false, notLogged: false }, false), true);
});

test('detailFetchState treats a 404 as an expected unlogged request', () => {
  assert.deepEqual(detailFetchState(404, 'no record for request id x'), { loading: false, error: '', notLogged: true });
  assert.deepEqual(detailFetchState(500, 'boom'), { loading: false, error: 'boom' });
  assert.deepEqual(detailFetchState(0, undefined), { loading: false, error: 'load failed' });
});


test('mergeLiveAndPersistedRow lets live tokens win', () => {
  const live = { requestId: 'r1', status: 200, latencyMs: 100, input: 10, output: 5, provider: 'p', agent: 'codex' };
  const persisted = { requestId: 'r1', status: 0, input: 3, output: 1, provider: 'old', agent: '' };
  const got = mergeLiveAndPersistedRow(live, persisted);
  assert.equal(got.status, 200);
  assert.equal(got.latencyMs, 100);
  assert.equal(got.input, 10);
  assert.equal(got.output, 5);
  assert.equal(got.provider, 'p');
  assert.equal(got.agent, 'codex');
});

test('mergeLiveAndPersistedRow inherits persisted tokens when live has zero', () => {
  const live = { requestId: 'r1', status: 200, input: 0, output: 0, provider: 'p' };
  const persisted = { requestId: 'r1', input: 7, output: 3, provider: 'old', agent: 'claude-code' };
  const got = mergeLiveAndPersistedRow(live, persisted);
  assert.equal(got.input, 7);
  assert.equal(got.output, 3);
  assert.equal(got.agent, 'claude-code');
  assert.equal(got.provider, 'p');
});

test('mergeLiveAndPersistedRow inherits persisted model and agent', () => {
  const live = { requestId: 'r1', status: 200, model: '—', agent: '' };
  const persisted = { requestId: 'r1', model: 'glm', agent: 'pi' };
  const got = mergeLiveAndPersistedRow(live, persisted);
  assert.equal(got.model, 'glm');
  assert.equal(got.agent, 'pi');
});

test('mergeLiveAndPersistedRow keeps live progress and guard hits', () => {
  const live = { requestId: 'r1', inFlight: true, progressText: 'ok', progressBytes: 42, guardHits: [{ type: 'guard' }] };
  const persisted = { requestId: 'r1', status: 200 };
  const got = mergeLiveAndPersistedRow(live, persisted);
  assert.equal(got.inFlight, true);
  assert.equal(got.progressText, 'ok');
  assert.equal(got.progressBytes, 42);
  assert.equal(got.guardHits.length, 1);
});

test('mergeLiveAndPersistedRow returns persisted when live is null', () => {
  const persisted = { requestId: 'r1', agent: 'curl' };
  assert.deepEqual(mergeLiveAndPersistedRow(null, persisted), persisted);
});

test('mergeLiveAndPersistedRow returns live when persisted is null', () => {
  const live = { requestId: 'r1', agent: 'curl' };
  assert.deepEqual(mergeLiveAndPersistedRow(live, null), live);
});

test('quotaErrKind classifies quota snapshot errors', () => {
  assert.equal(quotaErrKind(null), '');
  assert.equal(quotaErrKind({}), '');
  assert.equal(quotaErrKind({ Err: 'Session expired, please re-login' }), 'session-expired');
  assert.equal(quotaErrKind({ Err: 'not logged in' }), 'not-logged-in');
  assert.equal(quotaErrKind({ Err: 'HTTP 500' }), 'error');
});

test('accountUsageState: null snapshot is collapsed "no data"', () => {
  assert.deepEqual(accountUsageState(null), { hint: 'no data', open: false });
  assert.deepEqual(accountUsageState(undefined), { hint: 'no data', open: false });
});

test('accountUsageState: error snapshots stay open', () => {
  assert.deepEqual(accountUsageState({ Err: 'Session expired' }), { hint: 'session expired', open: true });
  assert.deepEqual(accountUsageState({ Err: 'not logged in' }), { hint: 'not logged in', open: true });
  assert.deepEqual(accountUsageState({ Err: 'HTTP 500' }), { hint: 'error', open: true });
});

test('accountUsageState: empty windows without error is collapsed unmeasured', () => {
  assert.deepEqual(accountUsageState({ Windows: [] }), { hint: 'unmeasured', open: false });
  assert.deepEqual(accountUsageState({ Plan: 'Pro', Windows: [] }), { hint: 'unmeasured', open: false });
});

test('accountUsageState: snapshots with windows default open', () => {
  const withUlt = {
    Windows: [{ Ultimate: true, RemainingPct: 0.486 }],
  };
  assert.deepEqual(accountUsageState(withUlt), { hint: '48.6% left', open: true });

  const withPlan = {
    Plan: 'Team',
    Windows: [{ Label: 'Daily', RemainingPct: 0.5 }],
  };
  assert.deepEqual(accountUsageState(withPlan), { hint: 'Team', open: true });

  const noPlan = {
    Windows: [{ Label: 'Daily', RemainingPct: 0.5 }],
  };
  assert.deepEqual(accountUsageState(noPlan), { hint: 'available', open: true });
});

test('pathStrengthFromAction maps log-weak to weak, configured actions to strong', () => {
  assert.equal(pathStrengthFromAction('log-weak'), 'weak');
  assert.equal(pathStrengthFromAction('log'), 'strong');
  assert.equal(pathStrengthFromAction('block'), 'strong');
  assert.equal(pathStrengthFromAction(''), '');
  assert.equal(pathStrengthFromAction(undefined), '');
});

test('securityLegendHTML explains every kind and action, escaped', () => {
  const html = securityLegendHTML();
  for (const token of ['secret', 'path', 'drift', 'log', 'redact', 'block', 'log-weak']) {
    assert.ok(html.includes(token), `legend missing ${token}`);
  }
  assert.ok(html.includes('<details'), 'legend is collapsible');
});

test('securityExplainHTML highlights the hit inside the escaped snippet', () => {
  const html = securityExplainHTML({
    status: 'ok',
    matches: [{
      name: 'aws_creds', strength: 'strong', located: true,
      explanation: 'Reference to ~/.aws/credentials.',
      pre: 'run cat ', hit: '~/.aws/credentials', post: ' <now>',
    }],
  });
  assert.ok(html.includes('<mark>~/.aws/credentials</mark>'), html);
  assert.ok(html.includes('&lt;now&gt;'), 'snippet context must be escaped');
  assert.ok(html.includes('badge warn'), 'strong renders as warn badge');
  assert.ok(html.includes('Reference to ~/.aws/credentials.'));
});

test('securityExplainHTML escapes an XSS payload inside the snippet and explanation', () => {
  const html = securityExplainHTML({
    status: 'ok',
    matches: [{
      name: 'x"><img src=x onerror=alert(1)>', located: true,
      explanation: '"><script>alert(2)</script>',
      pre: 'aa', hit: '<img src=x onerror=alert(3)>', post: 'bb',
    }],
  });
  assert.ok(!html.includes('<img'), html);
  assert.ok(!html.includes('<script'), html);
  assert.ok(html.includes('&lt;img'), html);
});

test('securityExplainHTML un-located matches show explanation without snippet', () => {
  const html = securityExplainHTML({
    status: 'ok',
    matches: [{ name: 'known_secret_fragmented', located: false, explanation: 'cross-request note' }],
  });
  assert.ok(html.includes('not re-located'), html);
  assert.ok(html.includes('cross-request note'));
  assert.ok(!html.includes('<mark'), html);
});

test('securityExplainHTML status notes cover non-ok statuses', () => {
  for (const status of ['no_request_log', 'not_found', 'redacted', 'cross_request', 'scanner_unavailable']) {
    const html = securityExplainHTML({ status, matches: [] });
    assert.ok(html.includes(SECURITY_EXPLAIN_STATUS_NOTES[status]), `${status} note missing`);
  }
  assert.equal(securityExplainHTML(null), '');
  assert.equal(securityExplainHTML({ status: 'ok', matches: [] }), '<div class="msg hint">Nothing to show.</div>');
});

// ---------------------------------------------------------------------------
// Auto-refresh interaction gate (framework)
// ---------------------------------------------------------------------------

test('POPUP_OPEN_SEL matches open popups and skips hidden ones', () => {
  // The gate's popup contract: transient layers carry data-popup + hidden.
  assert.equal(POPUP_OPEN_SEL, '[data-popup]:not([hidden])');
});

test('INTERACTIVE_CONTROL_SEL covers every uncommitted-input control kind', () => {
  for (const needed of ['input', 'select', 'textarea', '[role="combobox"]', '[contenteditable="true"]']) {
    assert.ok(INTERACTIVE_CONTROL_SEL.includes(needed),
      `INTERACTIVE_CONTROL_SEL lost ${needed} — focused controls of that kind would no longer hold auto-refreshes back`);
  }
});

test('refreshHoldReason reports the most specific hold, null when free', () => {
  // Popup outranks focus (a calendar popover contains focused buttons).
  assert.equal(refreshHoldReason({ openPopup: true, focusInteractive: true, selection: true }), 'popup');
  // Focus outranks a text selection.
  assert.equal(refreshHoldReason({ openPopup: false, focusInteractive: true, selection: true }), 'focus');
  assert.equal(refreshHoldReason({ openPopup: false, focusInteractive: false, selection: true }), 'selection');
  // No interaction → refresh freely. Missing/absent flags must not hold.
  assert.equal(refreshHoldReason({}), null);
  assert.equal(refreshHoldReason(null), null);
  assert.equal(refreshHoldReason(), null);
});

test('staleDataText renders the keep-old-data banner text', () => {
  // With failed parts: the parenthetical lists them.
  assert.equal(staleDataText('refresh failed', ['tokens', 'logs']),
    'refresh failed (tokens, logs) — showing last successful data');
  // Single part keeps the list form.
  assert.equal(staleDataText('refresh failed', ['status']),
    'refresh failed (status) — showing last successful data');
  // Without parts (analytics-style single message): no empty parentheses.
  assert.equal(staleDataText('analytics unavailable: http 503'),
    'analytics unavailable: http 503 — showing last successful data');
  assert.equal(staleDataText('x', []), 'x — showing last successful data');
  assert.equal(staleDataText('x', null), 'x — showing last successful data');
});
