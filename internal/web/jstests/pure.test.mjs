// pure.test.mjs — behavioral unit tests for assets/pure.js, run via
// `node --test jstests/` (driven by TestWebAssetsPureJSUnitTests in
// assets_test.go). Pure functions only: no DOM, fixed `now` injections,
// explicit locales — fully deterministic.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  esc, fmtNum, fmtCompactNum, avgLatencyMs, hasReset, fmtDur, untilHuman,
  YAML_EDITOR_MIN_HEIGHT, visibleYamlEditorHeight,
  verdictBadge, modelCapMatrix, providerCapsSummary, providerFrozen, providerNames,
  catalogMatchHTML, catalogMatchSummary, catalogMatchEditorHTML,
  ruleHitsLeaderboard,
  sessionTimeline, sessionBarSummary, responseExcerpt, requestExcerpt, chatViewHTML, readableValue, parseChatRequest, chatTurnsSliceHTML, CHAT_RECENT, requestRowHTML, requestTableHeadHTML, linkedProviders, sessionHealthSummary, guardMarksHTML, guardMarksDetailHTML, requestMetaHTML,
  cumulativeOffsets, virtualWindow, mergeRecordsPages, oldestTsSec,
  hashQueryParams, requestsFilterQuery, requestsFilterFromQuery,
  cacheHitRate, settingsDiff, settingsRestartKeys, configSummaryHTML,
  TOKEN_RANGES, tokenRangeBounds, tokenRangeLabel, tokensRangeQuery,
  tokenCustomBounds, parseLocalDate,
  WEEKDAYS, monthTitle, calendarMonthGrid, twoMonthWindow, shiftMonth, ymd,
  isFutureDay, rangePick, customRangeLabel, tokenRangeTriggerLabel, tokenRangePickerHTML,
  quotaUsageFromSec, quotaWindowFromSec, quotaWindowForProvider,
  prettyJSON, formatJSONLoose, jsonToHTML, parseSSE, isSSE, highlightJSON, splitLinesByBudget, linkedModels,
  sessionsForAgent, linkedAgents,
  fmtGuardDetail, fmtProgressBytes, analyticsChartSeries, analyticsPointValue, analyticsTableRows, analyticsTickLabel,
  ANALYTICS_METRICS, pctDelta, analyticsGranularity, analyticsGranOptions, analyticsValueText,
  HEAT_DAYS, analyticsHeatLevel, analyticsYearGrid, analyticsYearMonthSpans, analyticsHeatCellSize, analyticsHeatTip,
  analyticsRowSortKey, ANALYTICS_TABLE_SORT, analyticsTableSortValue, analyticsSortRows,
  analyticsMetricOptions, analyticsMetricAllowed,
  modelHealthFromSeries, modelHealthGrade, MODEL_HEALTH_DIMS, liveSessionSummary,
  liveSessionOrder, shortSessionId,
  fmtCompact,
  mergeLiveAndPersistedRow, shouldFetchDetail, detailFetchState,
  CLIENT_GONE_STATUS, notLoggedHint,
  quotaErrKind, accountUsageState,
  pathStrengthFromAction, securityLegendHTML, securityExplainHTML, SECURITY_EXPLAIN_STATUS_NOTES,
  securityKpisHTML, mergeSecurityFeed, securitySegmentsHTML, SECURITY_RANGES, securityRangeFromSecs,
  securityFilterQuery, securityFilterFromQuery, explainCacheKey,
  POPUP_OPEN_SEL, INTERACTIVE_CONTROL_SEL, refreshHoldReason, staleDataText,
  iconPin, iconRefresh, iconChevron, statusBadgeClass, statusBadgeHTML,
  kpiDeltaClass, logLineHTML,
takeoverRunSummary, takeoverRestoreSummary, takeoverVariantLabel,
  takeoverProtocolLabel, takeoverWriteVariantsLabel, takeoverClientLabel,
  highlightYAML, highlightTOML, highlightEnv, highlightConfig,
  takeoverFamilyGroups, takeoverFamilyBadge, TAKEOVER_TEMPLATE_EXAMPLES, TAKEOVER_PLACEHOLDERS,
  shadowMatchBadge,
  MCP_ANALYTICS_METRICS, mcpAnalyticsFilterSeries, mcpAnalyticsToolFilter, mcpAnalyticsSummaryGroups,
  mcpAnalyticsChartSeries, mcpAnalyticsMetricOptions, mcpAnalyticsPointValue, mcpAnalyticsValueText,
  mcpAnalyticsSummaryTableHTML, mcpAnalyticsEmptyHTML, mcpAnalyticsSkeletonHTML,
  VALID_MCP_SUB_TABS, mcpSubTabFromHash, mcpHash,
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

test('fmtCompactNum shortens large counts with K/M suffixes', () => {
  assert.equal(fmtCompactNum(null), '0');
  assert.equal(fmtCompactNum(0), '0');
  assert.equal(fmtCompactNum(512), '512');
  assert.equal(fmtCompactNum(999), '999');
  assert.equal(fmtCompactNum(2400), '2.4K');
  assert.equal(fmtCompactNum(3581), '3.6K');
  assert.equal(fmtCompactNum(9990), '10K');
  assert.equal(fmtCompactNum(15060), '15K');
  assert.equal(fmtCompactNum(118400), '118K');
  assert.equal(fmtCompactNum(999499), '999K');
  assert.equal(fmtCompactNum(999500), '1M');
  assert.equal(fmtCompactNum(118208), '118K');
  assert.equal(fmtCompactNum(1234567), '1.2M');
  assert.equal(fmtCompactNum(2340000), '2.3M');
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

test('catalogMatchSummary counts matched entries, tolerating junk', () => {
  assert.deepEqual(catalogMatchSummary(null), { total: 0, matched: 0 });
  assert.deepEqual(catalogMatchSummary([
    { matched: true }, { matched: false }, null, {},
  ]), { total: 4, matched: 1 });
});

test('catalogMatchHTML renders a collapsed details with per-row badges and actions', () => {
  const html = catalogMatchHTML([
    { provider: 'zhipu', model: 'glm-4.6', catalog_id: 'glm-4.6', matched: true, aliased: false },
    { provider: 'kimi', model: 'k2', catalog_id: 'kimi-k2-0905-preview', matched: true, aliased: true },
    { provider: 'codex', model: 'gpt-5.5', catalog_id: 'gpt-5.5', matched: false, aliased: false },
  ], false, false);
  // Collapsed by default, matched count in the summary.
  assert.match(html, /<details class="cat-match">/);
  assert.match(html, /Model Matching <span class="meta">2\/3 matched<\/span>/);
  // Direct match: ok badge, no action buttons.
  assert.match(html, /<td class="mono">zhipu<\/td>/);
  assert.ok(html.includes('<span class="badge ok">matched</span>'));
  // Aliased row: catalog id + aliased marker + Edit/Clear actions.
  assert.ok(html.includes('<span class="badge muted">aliased</span>'));
  assert.match(html, /data-cat-match-edit data-provider="kimi" data-model="k2"/);
  assert.match(html, /data-cat-match-clear data-provider="kimi" data-model="k2"/);
  // Unmatched row: warn badge + Match action.
  assert.ok(html.includes('<span class="badge warn">unmatched</span>'));
  assert.match(html, /data-cat-match-edit data-provider="codex" data-model="gpt-5.5"/);
  // open=true restores the expanded state.
  assert.match(catalogMatchHTML([{ provider: 'p', model: 'm', matched: true }], false, true),
    /<details class="cat-match" open>/);
});

test('catalogMatchHTML escapes names and handles the empty-catalog and empty-list cases', () => {
  assert.equal(catalogMatchHTML([], false, false), '');
  assert.equal(catalogMatchHTML(null, false, false), '');
  // No catalog cache: hint instead of a table (nothing to match against).
  const empty = catalogMatchHTML([{ provider: 'p', model: 'm', matched: false }], true, false);
  assert.ok(empty.includes('catalog cache is empty'));
  assert.ok(!empty.includes('<table'));
  const xss = catalogMatchHTML([{ provider: '"><img>', model: '<b>', catalog_id: 'x"', matched: false }], false, false);
  assert.ok(!xss.includes('<img>') && !xss.includes('<b>'));
  assert.ok(xss.includes('&quot;'));
});

test('catalogMatchEditorHTML renders the datalist editor with the current id prefilled', () => {
  const html = catalogMatchEditorHTML('kimi-k2-0905-preview');
  assert.ok(html.includes('list="cat-id-list"'));
  assert.ok(html.includes('value="kimi-k2-0905-preview"'));
  assert.ok(html.includes('data-cat-match-save'));
  assert.ok(html.includes('data-cat-match-cancel'));
  assert.ok(catalogMatchEditorHTML('').includes('value=""'));
});

test('providerCapsSummary counts yes-verdict legs per protocol', () => {
  const [zeta] = modelCapMatrix({
    zeta: { models: {
      'm-b': { chat: 'yes', anthropic: 'no', responses: 'unknown' },
      'm-a': { chat: 'yes', anthropic: 'yes', responses: 'no' },
      'm-c': { chat: 'no', anthropic: 'yes', responses: 'yes' },
    } },
  });
  assert.deepEqual(providerCapsSummary(zeta), { models: 3, chat: 2, anthropic: 2, responses: 1 });
  // Unknown is NOT yes: a probe-pending leg counts nowhere.
  const [empty] = modelCapMatrix({ alpha: { models: {} } });
  assert.deepEqual(providerCapsSummary(empty), { models: 0, chat: 0, anthropic: 0, responses: 0 });
  assert.deepEqual(providerCapsSummary(null), { models: 0, chat: 0, anthropic: 0, responses: 0 });
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

test('configSummaryHTML renders KPI tiles plus the feature-block grid', () => {
  const html = configSummaryHTML({
    summary: { listen: '127.0.0.1:15722', provider_count: 2, route_count: 5 },
    provider_models: { a: ['m1', 'm2'], b: ['m3'] },
    provider_meta: { a: { priority: 0, alias: { m1: 'alias-1' } }, b: { priority: 1 } },
    settings: {
      log_level: 'debug', log_file: '/tmp/mp.log',
      request_log: { enabled: true, dir: '~/r' },
      stats: { db_path: '~/stats.db' },
      cache: { enabled: true, ttl: '5m', max_entries: 42 },
      guard: { secrets: 'block', paths: 'log', known_secrets: true, decode: false },
    },
  });
  assert.match(html, /^<div class="kpi-grid sum-tiles">/);
  assert.match(html, /<div class="k">listen<\/div><div class="v">127\.0\.0\.1:15722<\/div>/);
  assert.match(html, /<div class="k">providers<\/div><div class="v">2<\/div><div class="d">3 models · 1 aliases<\/div>/);
  assert.match(html, /<div class="k">routes<\/div><div class="v">5<\/div><div class="d">effective table<\/div>/);
  assert.match(html, /<div class="sum-grid">/);
  assert.match(html, /<div class="sum-k">log<\/div><div class="sum-v" title="debug → \/tmp\/mp\.log">debug → \/tmp\/mp\.log<\/div>/);
  assert.match(html, /<div class="sum-k">request_log <span class="badge ok">on<\/span><\/div><div class="sum-v" title="~\/r">~\/r<\/div>/);
  assert.match(html, /<div class="sum-k">stats<\/div><div class="sum-v" title="~\/stats\.db">~\/stats\.db<\/div>/);
  assert.match(html, /<div class="sum-k">cache <span class="badge ok">on<\/span><\/div><div class="sum-v" title="ttl 5m · max 42 entries">ttl 5m · max 42 entries<\/div>/);
  assert.match(html, /<div class="sum-k">guard<\/div><div class="sum-v" title="secrets block · paths log · known on · decode off">/);
});

test('configSummaryHTML marks absent keys with the code default and off blocks with a muted badge', () => {
  const html = configSummaryHTML({});
  assert.match(html, /<div class="k">listen<\/div><div class="v">—<\/div>/);
  assert.match(html, /<div class="k">providers<\/div><div class="v">0<\/div><div class="d">0 models · 0 aliases<\/div>/);
  assert.match(html, /<div class="sum-v" title="info → \/tmp\/model-proxy\.log">/);
  assert.match(html, /<div class="sum-k">request_log <span class="badge muted">off<\/span><\/div><div class="sum-v" title="—">—<\/div>/);
  assert.match(html, /<div class="sum-v" title="~\/\.model-proxy\/stats\.db \(default\)">/);
  assert.match(html, /<div class="sum-k">cache <span class="badge muted">off<\/span><\/div><div class="sum-v" title="—">—<\/div>/);
  assert.match(html, /secrets log · paths log · known off · decode off/);
});

test('configSummaryHTML keeps enabled blocks honest about defaulted sub-keys', () => {
  const html = configSummaryHTML({
    settings: { request_log: { enabled: true }, cache: { enabled: true } },
  });
  assert.match(html, /<div class="sum-v" title="~\/\.model-proxy\/log\/requests \(default\)">/);
  assert.match(html, /<div class="sum-v" title="ttl 10m \(default\) · max 1000 entries">/);
});

test('configSummaryHTML escapes config values before interpolation', () => {
  const html = configSummaryHTML({
    summary: { listen: '<img>' },
    settings: { stats: { db_path: '"><script>' } },
  });
  assert.ok(!html.includes('<img>') && !html.includes('<script>'));
  assert.match(html, /&lt;img&gt;/);
  assert.match(html, /&quot;&gt;&lt;script&gt;/);
});

test('configSummaryHTML renders the MCP gateway surface with enabled counts', () => {
  const html = configSummaryHTML({}, {
    servers: [{ name: 'a', enabled: true }, { name: 'b', enabled: false }],
    routes: [{ name: 'r', enabled: true, targets: [] }],
  });
  assert.match(html, /<div class="sum-k">mcp <span class="badge ok">on<\/span><\/div><div class="sum-v" title="1\/2 servers · 1\/1 routes enabled">/);
});

test('configSummaryHTML shows mcp off when the gateway has no servers or routes', () => {
  const html = configSummaryHTML({}, { servers: [], routes: [] });
  assert.match(html, /<div class="sum-k">mcp <span class="badge muted">off<\/span><\/div><div class="sum-v" title="—">—<\/div>/);
});

test('configSummaryHTML renders mcp as unavailable (no badge) when the fetch failed', () => {
  const html = configSummaryHTML({}, null);
  assert.match(html, /<div class="sum-k">mcp<\/div><div class="sum-v" title="—">—<\/div>/);
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

test('tokenRangePickerHTML opts: extraPresets prepend + label override', () => {
  const html = tokenRangePickerHTML(
    { preset: 'quota', customStart: '', customEnd: '' },
    { open: false, view: null, pick: null, selecting: false },
    'acc',
    { extraPresets: [{ value: 'quota', label: 'Quota Window' }], label: 'Quota Window' });
  // The tab-local preset is the FIRST row and carries the checkmark; the
  // shared presets follow in their canonical order.
  assert.ok(/<button class="tr-preset active" data-acc-preset="quota">[\s\S]*<span class="tr-check">✓<\/span>Quota Window/.test(html));
  assert.ok(html.indexOf('data-acc-preset="quota"') < html.indexOf('data-acc-preset="all"'));
  // The label override wins the trigger's value half (the shared label
  // helper does not know the tab-local preset).
  assert.ok(html.includes('<span class="tr-value">Quota Window</span>'));
  // Without the option the shared presets alone are rendered.
  const plain = tokenRangePickerHTML(
    { preset: '7d', customStart: '', customEnd: '' },
    { open: false, view: null, pick: null, selecting: false },
    'tr');
  assert.ok(!plain.includes('data-tr-preset="quota"'));
  assert.ok(plain.includes('<span class="tr-value">Last 7d</span>'));
});

test('quotaUsageFromSec reads the projected billing-period start, guards garbage', () => {
  const now = new Date(2026, 8, 20, 12, 0, 0).getTime();
  // zhipu-shaped weekly window: reset 09-25 17:50 on a 7d cycle → started
  // 09-18 17:50 (the provider layer derives this; the UI only parses it).
  const from = quotaUsageFromSec({ UsageFrom: '2026-09-18T17:50:59+08:00' }, now);
  assert.equal(from, Math.floor(new Date('2026-09-18T17:50:59+08:00').getTime() / 1000));
  // Zero time (Go's unset time.Time) parses far in the past → null.
  assert.equal(quotaUsageFromSec({ UsageFrom: '0001-01-01T00:00:00Z' }, now), null);
  assert.equal(quotaUsageFromSec({ UsageFrom: '2027-01-01T00:00:00Z' }, now), null); // future
  assert.equal(quotaUsageFromSec({ UsageFrom: 'not a date' }, now), null);
  assert.equal(quotaUsageFromSec({}, now), null);
  assert.equal(quotaUsageFromSec(null, now), null);
  assert.equal(quotaUsageFromSec(undefined, now), null);
});

test('quotaWindowFromSec picks the first resolvable plan provider, keys sorted', () => {
  const now = new Date(2026, 8, 20, 12, 0, 0).getTime();
  const quota = {
    zhipu: { UsageFrom: '2026-09-18T17:50:59+08:00' },
    codex: { UsageFrom: '2026-09-01T08:00:00+08:00' },   // 30d cycle, later in sort
    deepseek: { UsageFrom: '0001-01-01T00:00:00Z' },     // payg: zero time → skipped
    broken: { Err: 'boom' },                              // no UsageFrom → skipped
  };
  // codex < zhipu alphabetically — the first SORTED key wins, not insertion.
  const got = quotaWindowFromSec(quota, now);
  assert.equal(got.key, 'codex');
  assert.equal(got.from, Math.floor(new Date('2026-09-01T08:00:00+08:00').getTime() / 1000));
  // Only unresolvable entries → null; empty/garbage maps → null.
  assert.equal(quotaWindowFromSec({ deepseek: quota.deepseek }, now), null);
  assert.equal(quotaWindowFromSec({}, now), null);
  assert.equal(quotaWindowFromSec(null, now), null);
  // Snapshot values may be projections (plain objects) — numeric-ish or
  // malformed UsageFrom fields are skipped, not thrown.
  assert.equal(quotaWindowFromSec({ x: { UsageFrom: 123 } }, now), null);
});

test('quotaWindowForProvider resolves the selected provider, own key then pool entries', () => {
  const now = new Date(2026, 8, 20, 12, 0, 0).getTime();
  const quota = {
    aqp: { UsageFrom: '2026-09-01T08:00:00+08:00' },
    zhipu: { UsageFrom: '2026-09-18T17:50:59+08:00' },
    'pool#a1': { UsageFrom: '0001-01-01T00:00:00Z' },   // unresolvable → skipped
    'pool#a2': { UsageFrom: '2026-09-10T00:00:00+08:00' },
  };
  // The provider's own entry wins and drives the window (NOT the first
  // sorted key of the whole map — aqp exists but zhipu was asked for).
  const z = quotaWindowForProvider(quota, 'zhipu', now);
  assert.equal(z.key, 'zhipu');
  assert.equal(z.from, Math.floor(new Date('2026-09-18T17:50:59+08:00').getTime() / 1000));
  // Multi-entry pools key quota per account ("name#<accountId>"): the first
  // resolvable account stands in for the pool.
  const p = quotaWindowForProvider(quota, 'pool', now);
  assert.equal(p.key, 'pool#a2');
  assert.equal(p.from, Math.floor(new Date('2026-09-10T00:00:00+08:00').getTime() / 1000));
  // A prefix look-alike ("poolx") must not match "pool" — the '#' boundary
  // is part of the match.
  assert.equal(quotaWindowForProvider({ 'poolx#a1': quota['pool#a2'] }, 'pool', now), null);
  // No provider selected, unknown provider, or no resolvable entry → null.
  assert.equal(quotaWindowForProvider(quota, '', now), null);
  assert.equal(quotaWindowForProvider(quota, 'deepseek', now), null);
  assert.equal(quotaWindowForProvider({ zhipu: { Err: 'boom' } }, 'zhipu', now), null);
  assert.equal(quotaWindowForProvider(null, 'zhipu', now), null);
});

test('tokenRangePickerHTML disabled extra preset renders unclickable', () => {
  const html = tokenRangePickerHTML(
    { preset: 'all', customStart: '', customEnd: '' },
    { open: false, view: null, pick: null, selecting: false },
    'an',
    { extraPresets: [{ value: 'quota', label: 'Quota Window', disabled: true }] });
  // Disabled rows stay visible (stable layout) but carry the disabled attr
  // and never the active checkmark.
  assert.ok(/<button class="tr-preset" data-an-preset="quota" disabled>/.test(html));
  assert.ok(!/data-an-preset="quota" disabled>\s*<span class="tr-check">✓/.test(html));
  // Enabled extra presets render without the attribute.
  const enabled = tokenRangePickerHTML(
    { preset: 'all', customStart: '', customEnd: '' },
    { open: false, view: null, pick: null, selecting: false },
    'an',
    { extraPresets: [{ value: 'quota', label: 'Quota Window' }] });
  assert.ok(/<button class="tr-preset" data-an-preset="quota">/.test(enabled));
});

test('tokenRangePickerHTML closed state: hidden popover, namespace attrs, trigger label', () => {
  const closed = tokenRangePickerHTML(
    { preset: '7d', customStart: '', customEnd: '' },
    { open: false, view: null, pick: null, selecting: false },
    'acc');
  // Popover is present but hidden, and carries the interaction-gate hook.
  assert.ok(closed.includes('class="tr-popover" data-popup hidden'));
  // No calendar while closed.
  assert.ok(!closed.includes('tr-cal'));
  // Trigger reflects the applied preset label.
  assert.ok(closed.includes('<span class="tr-value">Last 7d</span>'));
  // Namespace rides every wired control so tabs never collide; the active
  // preset carries the checkmark.
  assert.ok(closed.includes('data-acc-preset="7d"'));
  assert.ok(!closed.includes('data-tr-preset'));
  assert.ok(/<button class="tr-preset active" data-acc-preset="7d">[\s\S]*<span class="tr-check">✓<\/span>Last 7d/
    .test(closed));
  // opt.triggerId pins the Status e2e's #tr-trigger address.
  const status = tokenRangePickerHTML(
    { preset: 'today', customStart: '', customEnd: '' },
    { open: false, view: null, pick: null, selecting: false },
    'tr', { triggerId: 'tr-trigger' });
  assert.ok(status.includes('class="btn small tr-trigger" type="button" id="tr-trigger"'));
  assert.ok(status.includes('data-tr-preset="today"'));
});

test('tokenRangePickerHTML open state: two-month calendar, applied range, future days', () => {
  const now = new Date(2026, 8, 8, 12, 0, 0).getTime(); // 2026-09-08 local
  const html = tokenRangePickerHTML(
    { preset: 'custom', customStart: '2026-09-02', customEnd: '2026-09-05' },
    { open: true, view: { year: 2026, month: 8 }, pick: null, selecting: false },
    'an', { now });
  assert.ok(html.includes('class="tr-popover" data-popup ')); // open → no hidden
  // Two month panes: September + October 2026, nav buttons wired per ns.
  assert.ok(html.includes('September 2026'));
  assert.ok(html.includes('October 2026'));
  assert.ok(html.includes('data-an-nav="-1"'));
  // The view's right edge reaches this month → next is clamped disabled.
  assert.ok(/data-an-nav="1" aria-label="next month" disabled/.test(html));
  const cell = (dayYmd) => {
    const m = new RegExp(`<button class="([^"]*)" data-an-day="${dayYmd}"([^>]*)>`).exec(html);
    return m ? m[1] + (m[2].includes('disabled') ? ' disabled' : '') : null;
  };
  // Applied custom range: endpoints circled, middle days in-range, today
  // selectable, future days disabled, adjacent-month days absent (blank).
  assert.equal(cell('2026-09-02'), 'tr-day tr-day-selected');
  assert.equal(cell('2026-09-05'), 'tr-day tr-day-selected');
  assert.equal(cell('2026-09-03'), 'tr-day tr-day-inrange');
  assert.equal(cell('2026-09-06'), 'tr-day'); // outside range
  assert.equal(cell('2026-09-08'), 'tr-day'); // today
  assert.equal(cell('2026-09-09'), 'tr-day disabled'); // future
  assert.equal(cell('2026-10-01'), 'tr-day disabled'); // next month is all future
  // While a NEW pick is in progress the pick's start circle replaces the
  // applied range's highlight.
  const picking = tokenRangePickerHTML(
    { preset: 'custom', customStart: '2026-09-02', customEnd: '2026-09-05' },
    { open: true, view: { year: 2026, month: 8 }, pick: '2026-09-07', selecting: true },
    'an', { now });
  const pickCell = (dayYmd) => {
    const m = new RegExp(`<button class="([^"]*)" data-an-day="${dayYmd}"([^>]*)>`).exec(picking);
    return m ? m[1] : null;
  };
  assert.equal(pickCell('2026-09-07'), 'tr-day tr-day-selected');
  assert.equal(pickCell('2026-09-02'), 'tr-day'); // applied start no longer circled
  assert.equal(pickCell('2026-09-03'), 'tr-day'); // applied range no longer shaded
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

test('linkedProviders filters providers by the model they carry', () => {
  const map = { 'zhipu': ['glm-5.3', 'glm-5.3-air'], 'deepseek': ['deepseek-v4'], 'openai': ['gpt-6'] };
  assert.deepEqual(linkedProviders('glm-5.3', map), ['zhipu']);
  // Case/whitespace-insensitive exact name; substring partials do NOT match
  // (unlike linkedModels — replay targets a concrete catalog entry).
  assert.deepEqual(linkedProviders('  GLM-5.3 ', map), ['zhipu']);
  assert.deepEqual(linkedProviders('glm', map), []);
  // Unknown model / empty inputs degrade to empty (caller falls back).
  assert.deepEqual(linkedProviders('nope', map), []);
  assert.deepEqual(linkedProviders('', map), []);
  assert.deepEqual(linkedProviders('glm-5.3', null), []);
  assert.deepEqual(linkedProviders('glm-5.3', {}), []);
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

test('analyticsTickLabel demotes to HH:mm when ticks are denser than the full label', () => {
  const v = new Date(2026, 8, 14, 9, 0, 0).getTime() / 1000; // local 09-14 09:00
  // Wide hourly ticks keep the full MM-DD HH:mm label.
  assert.equal(analyticsTickLabel(v, 3600, 200), '09-14 09:00');
  // Hourly ticks packed under the ~67px full-label width (the reported
  // overlap) demote to HH:mm.
  assert.equal(analyticsTickLabel(v, 3600, 66), '09:00');
  assert.equal(analyticsTickLabel(v, 3600, 71.9), '09:00');
  assert.equal(analyticsTickLabel(v, 3600, 72), '09-14 09:00');
  // Minute spans demote on span alone, regardless of measured px.
  assert.equal(analyticsTickLabel(v, 900, 500), '09:00');
  // Unknown spacing keeps the full label.
  assert.equal(analyticsTickLabel(v, null, null), '09-14 09:00');
  assert.equal(analyticsTickLabel(v, 3600, null), '09-14 09:00');
});

test('analyticsTickLabel anchors dates at midnight and month starts', () => {
  const midnight = new Date(2026, 8, 14, 0, 0, 0).getTime() / 1000;
  assert.equal(analyticsTickLabel(midnight, 3600, 40), '09-14');
  const monthStart = new Date(2026, 8, 1, 0, 0, 0).getTime() / 1000;
  assert.equal(analyticsTickLabel(monthStart, 86400, 40), '2026-09');
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
    ['tokens', 'toksec', 'cache', 'latency', 'ttft', 'requests', 'failovers', 'rate429', 'errors', 'cost']);
  for (const m of ANALYTICS_METRICS) {
    assert.ok(m.label && m.axis, `metric ${m.id} needs label+axis`);
    assert.equal(typeof m.gap, 'boolean', `metric ${m.id} needs a gap flag`);
  }
  // failovers/429s are additive attempt counts: zero-fill like requests,
  // never uPlot gaps.
  assert.equal(ANALYTICS_METRICS.find((m) => m.id === 'failovers').gap, false);
  assert.equal(ANALYTICS_METRICS.find((m) => m.id === 'rate429').gap, false);
});

test('failover/429 metrics disable on agent-dimension reads', () => {
  // by=agent and any agent filter both reroute to agent_buckets, which has
  // no failover/429 columns — the metrics are structurally absent, not zero.
  const model = analyticsMetricOptions('model', '');
  assert.ok(model.every((o) => !o.disabled), 'model dimension: all metrics enabled');
  const byAgent = analyticsMetricOptions('agent', '');
  assert.equal(byAgent.find((o) => o.value === 'failovers').disabled, true);
  assert.equal(byAgent.find((o) => o.value === 'rate429').disabled, true);
  assert.equal(byAgent.find((o) => o.value === 'tokens').disabled, false);
  const agentFilter = analyticsMetricOptions('model', 'codex');
  assert.equal(agentFilter.find((o) => o.value === 'failovers').disabled, true);
  // The allowed-pick gate: an unavailable stored pick falls back (caller
  // renders tokens), a valid one passes through.
  assert.equal(analyticsMetricAllowed('failovers', 'model', ''), true);
  assert.equal(analyticsMetricAllowed('failovers', 'agent', ''), false);
  assert.equal(analyticsMetricAllowed('rate429', 'model', 'codex'), false);
  assert.equal(analyticsMetricAllowed('tokens', 'agent', 'codex'), true);
});

test('failover/429 read as count metrics from points and totals', () => {
  // Point reader (trend chart).
  assert.equal(analyticsPointValue({ failovers: 3, rate_limited_429: 2 }, 'failovers'), 3);
  assert.equal(analyticsPointValue({ failovers: 3, rate_limited_429: 2 }, 'rate429'), 2);
  assert.equal(analyticsPointValue({}, 'failovers'), 0); // zero-fill, not null
  assert.equal(analyticsPointValue(null, 'rate429'), null);
  // Series totals reader (leaderboard rows: sort keys + err-cell tooltip).
  const rows = analyticsTableRows([{ provider: 'p', model: 'm', totals: {
    requests: 5, failures: 1, failovers: 4, rate_limited_429: 2, tokens: 9,
  } }]);
  assert.equal(rows[0].failovers, 4);
  assert.equal(rows[0].rateLimited, 2);
  assert.equal(rows[0].failures, 1);
  // Row sort keys follow the active metric (shared with the Dashboard).
  assert.equal(analyticsRowSortKey(rows[0], 'failovers'), 4);
  assert.equal(analyticsRowSortKey(rows[0], 'rate429'), 2);
  assert.equal(analyticsRowSortKey({ failovers: 0 }, 'failovers'), 0);
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
  assert.equal(codex.agent, 'codex'); // kept for the drilldown filter
  assert.equal(aqp.agent, '');
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
  // Day stays selectable over multi-year windows (all-time is anchored to the
  // oldest data, so the span reflects real history).
  assert.equal(analyticsGranularity(800 * D, 'day'), 'day');
  assert.equal(analyticsGranularity(2000 * D, 'day'), 'day');
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
  assert.deepEqual(ids(1500 * D), ['auto', 'day', 'week', 'month']);
  assert.deepEqual(ids(3000 * D), ['auto', 'week', 'month']); // day caps at 2000d
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

test('liveSessionOrder sorts by recency (live beats stale persisted), ties by id', () => {
  const sessions = [
    { session_id: 'old-session', last_ts: '2026-09-11T00:00:00Z' },   // persisted, quiet
    { session_id: 'aaa', last_ts: '2026-09-11T00:00:00Z' },            // ties with bbb
    { session_id: 'bbb', last_ts: '2026-09-11T00:00:00Z' },
    { session_id: 'no-ts' },                                           // unparseable → end
  ];
  const liveRows = [
    { session: 'old-session', ts: Date.parse('2026-09-11T12:00:00Z') }, // live newer than persisted
    { session: 'live-only', ts: Date.parse('2026-09-11T09:00:00Z') },
  ];
  const out = liveSessionOrder(sessions, liveRows);
  assert.deepEqual(out, ['old-session', 'live-only', 'aaa', 'bbb', 'no-ts']);
});

test('liveSessionOrder keeps live-only sessions with missing timestamps and dedupes', () => {
  const liveRows = [
    { session: 'x', ts: 0 },        // unparseable ts still listed
    { session: 'x', ts: 500 },      // duplicate row folds into max → newest
    { session: 'y', ts: 100 },
  ];
  assert.deepEqual(liveSessionOrder([], liveRows), ['x', 'y']);
  assert.deepEqual(liveSessionOrder(null, null), []);
});

test('shortSessionId abbreviates long ids, passes short labels through', () => {
  assert.equal(shortSessionId('1234567890abcdef9012'), '1234…9012');
  assert.equal(shortSessionId('main'), 'main');
  assert.equal(shortSessionId('1234567890123456'), '1234567890123456'); // 16 chars: unchanged
  assert.equal(shortSessionId('12345678901234567'), '1234…4567');  // 17 chars: abbreviated
  assert.equal(shortSessionId(''), '');
  assert.equal(shortSessionId(null), '');
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

test('notLoggedHint names the client-gone terminal, stays generic otherwise', () => {
  // The client-gone terminal (live status 499) never has a record — the
  // pipeline publishes the end event instead of committing — and the Live
  // popover marks it not-logged without fetching; the hint names the cause.
  assert.equal(CLIENT_GONE_STATUS, 499);
  assert.equal(notLoggedHint(499), 'client cancelled before commit — no request-log record');
  // Every other unlogged terminal (a fetch that 404'd, e.g. an unrouted
  // 502) keeps the generic wording, including absent/unknown status.
  const generic = 'not logged — the request did not commit, so there is no request-log record';
  assert.equal(notLoggedHint(502), generic);
  assert.equal(notLoggedHint(undefined), generic);
  assert.equal(notLoggedHint(0), generic);
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

test('mergeLiveAndPersistedRow merges cache buckets with live-non-zero-wins', () => {
  // A persisted row truncated to zero tokens by max_body_bytes must pick up
  // the live end event's cache counts instead of showing 0 (which disagreed
  // with the All live table).
  const truncated = mergeLiveAndPersistedRow(
    { requestId: 'r1', status: 200, input: 0, output: 0, cacheRead: 4200, cacheCreation: 128 },
    { requestId: 'r1', status: 0, input: 0, output: 0, cacheRead: 0, cacheCreation: 0 },
  );
  assert.equal(truncated.cacheRead, 4200);
  assert.equal(truncated.cacheCreation, 128);
  // A live zero must never erase a real persisted count.
  const kept = mergeLiveAndPersistedRow(
    { requestId: 'r1', status: 200, input: 5, output: 2, cacheRead: 0, cacheCreation: 0 },
    { requestId: 'r1', input: 7, output: 3, cacheRead: 900, cacheCreation: 50 },
  );
  assert.equal(kept.cacheRead, 900);
  assert.equal(kept.cacheCreation, 50);
  // A live non-zero cache value wins over the persisted one.
  const wins = mergeLiveAndPersistedRow(
    { requestId: 'r1', cacheRead: 100, cacheCreation: 0 },
    { requestId: 'r1', cacheRead: 40, cacheCreation: 60 },
  );
  assert.equal(wins.cacheRead, 100);
  assert.equal(wins.cacheCreation, 60);
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

test('mergeLiveAndPersistedRow merges the MCP tool: live end event wins, persisted backfills', () => {
  // Live start rows never carry the tool (the body is parsed after the
  // start fires) — the persisted summary backfills it.
  const startOnly = mergeLiveAndPersistedRow(
    { requestId: 'm1', status: 200, model: 'web-search' },
    { requestId: 'm1', model: 'web-search', tool: 'search_videos' });
  assert.equal(startOnly.tool, 'search_videos');
  // The end event's tool is authoritative once it lands.
  const ended = mergeLiveAndPersistedRow(
    { requestId: 'm1', status: 200, model: 'web-search', tool: 'get_status' },
    { requestId: 'm1', model: 'web-search', tool: 'search_videos' });
  assert.equal(ended.tool, 'get_status');
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
  assert.deepEqual(accountUsageState(null), { hint: 'No data', open: false });
  assert.deepEqual(accountUsageState(undefined), { hint: 'No data', open: false });
});

test('accountUsageState: error snapshots stay open', () => {
  assert.deepEqual(accountUsageState({ Err: 'Session expired' }), { hint: 'Session expired', open: true });
  assert.deepEqual(accountUsageState({ Err: 'not logged in' }), { hint: 'Not logged in', open: true });
  assert.deepEqual(accountUsageState({ Err: 'HTTP 500' }), { hint: 'Error', open: true });
});

test('accountUsageState: empty windows without error is collapsed unmeasured', () => {
  assert.deepEqual(accountUsageState({ Windows: [] }), { hint: 'Unmeasured', open: false });
  assert.deepEqual(accountUsageState({ Plan: 'Pro', Windows: [] }), { hint: 'Unmeasured', open: false });
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
  assert.deepEqual(accountUsageState(noPlan), { hint: 'Available', open: true });
});

test('pathStrengthFromAction maps log-weak to weak, configured actions to strong', () => {
  assert.equal(pathStrengthFromAction('log-weak'), 'weak');
  assert.equal(pathStrengthFromAction('log'), 'strong');
  assert.equal(pathStrengthFromAction('block'), 'strong');
  assert.equal(pathStrengthFromAction(''), '');
  assert.equal(pathStrengthFromAction(undefined), '');
});

test('securityKpisHTML summarizes blocks, verdict counts and LLM usage', () => {
  // Verdict counts come from the SERVER-side aggregation (/api/security
  // counts — SQL GROUP BY over the audit store plus the cumulative low
  // counter), never from counting the client-merged feed: that blend
  // included the in-memory ring's cached-replay rows and drifted on restart.
  const counts = { high: 2, medium: 1, low: 2, error: 1, skipped: 1 };
  const stats = { calls: 7, input_tokens: 12345, output_tokens: 678, low_verdicts: 2 };
  const html = securityKpisHTML([{ session_id: 's' }], counts, stats, true);
  if (!html.includes('class="kpi-grid"') || !html.includes('class="kpi"')) throw new Error('shared KPI tile component (.kpi-grid/.kpi)');
  if (!html.includes('blocked sessions') || !html.includes('>1<')) throw new Error('blocked stat');
  if (!html.includes('high verdicts') || !html.includes('>2<')) throw new Error('high stat');
  if (!html.includes('medium verdicts') || !html.includes('>1<')) throw new Error('medium stat');
  if (!html.includes('no session block')) throw new Error('medium stat description');
  // The low tier is deliberately absent from the strip (suppressed noise) —
  // it still counts in Rule hits and the JSONL trail.
  if (/suppressed|low \(/.test(html)) throw new Error('low tier must not render as a KPI stat');
  if (!html.includes('errors') || !html.includes('>2<')) throw new Error('errors stat (error+skipped)');
  if (!html.includes('llm calls') || !html.includes('>7<')) throw new Error('llm calls stat');
  if (!html.includes('llm tokens')) throw new Error('llm tokens stat');
  if (!html.includes('13K')) throw new Error('token total uses compact format (12345+678=13023)');
  if (!html.includes('in 12.3K') || !html.includes('out 678')) throw new Error('token split detail line');
  if (!html.includes('class="v err"')) throw new Error('non-zero blocked/high must use the err accent');
  // zero-state: no err accents anywhere, tokens stat renders 0
  const clean = securityKpisHTML([], {}, {}, true);
  if (clean.includes('class="v err"')) throw new Error('zero state must not use err accent');
  if (!clean.includes('>0<')) throw new Error('zero calls stat');
  // adjudication channel off and never used: one "off" stat instead of two
  // permanent zeros (past usage still shows the real stats)
  const off = securityKpisHTML([], {}, {}, false);
  if (!off.includes('llm adjudication') || !off.includes('>off<')) throw new Error('off stat');
  if (off.includes('llm calls') || off.includes('llm tokens')) throw new Error('off state must drop the usage stats');
  const past = securityKpisHTML([], {}, { calls: 3, input_tokens: 10, output_tokens: 5 }, false);
  if (!past.includes('llm calls') || !past.includes('>3<')) throw new Error('past usage keeps the real stats');
  // Counts unavailable (server-side aggregation failed): verdict tiles render
  // '—' with the reason, never misleading zeros; blocks/llm tiles keep real data.
  const unavail = securityKpisHTML([{ session_id: 's' }], null, stats, true, 'verdict counts unavailable');
  if (!unavail.includes('verdict counts unavailable')) throw new Error('countsError rides the verdict tiles');
  const dashCount = (unavail.match(/<div class="v">—<\/div>/g) || []).length;
  if (dashCount !== 3) throw new Error(`expected 3 unavailable verdict tiles, got ${dashCount}`);
  if (unavail.includes('high verdicts') && unavail.includes('>2<')) throw new Error('unavailable counts must not render zeros/stale numbers');
  if (!unavail.includes('llm calls') || !unavail.includes('>7<')) throw new Error('llm tiles unaffected by countsError');
});

test('securityExplainHTML renders verdict badges and per-verdict evidence', () => {
  const html = securityExplainHTML({
    status: 'ok',
    adjudications: [
      { rule: 'ssh', verdict: 'medium', reason: 'tool call touches key path', evidence: 'bash argument references ~/.ssh/id_rsa', model: 'glm-5.3-flash' },
      { rule: 'jwt', verdict: 'high', reason: 'live token shape' },
    ],
    matches: [{ name: 'ssh', located: true, pre: 'x', hit: 'id_rsa', post: 'y' }],
  });
  if (!/>medium</.test(html)) throw new Error('medium verdict badge');
  if (!/badge warn[^>]*>medium</.test(html)) throw new Error('medium must use the warn badge');
  if (!html.includes('evidence: bash argument references ~/.ssh/id_rsa')) throw new Error('evidence row');
  const count = (html.match(/evidence:/g) || []).length;
  if (count !== 1) throw new Error(`evidence rows = ${count}, want exactly 1 (jwt has none)`);
});

test('mergeSecurityFeed carries the LLM verdict attribution for the drill views', () => {
  const rows = mergeSecurityFeed(
    [
      { ts: 300, kind: 'secret', names: ['ssh'], action: 'log', request_id: 'r1', verdict: 'medium', reason: 'tool call touches key path', evidence: 'bash argument references ~/.ssh/id_rsa', model: 'glm-5.3-flash', session_id: 's1' },
    ],
    [
      { ts: 100, kind: 'secret', rule: 'ssh', verdict: 'medium', reason: 'tool call touches key path', evidence: 'bash argument references ~/.ssh/id_rsa', model: 'glm-5.3-flash', request_id: 'r1', session_id: 's1', cached: true },
    ],
  );
  if (rows.length !== 1) throw new Error('ring entry must fold into the audit row');
  const r = rows[0];
  if (r.reason !== 'tool call touches key path') throw new Error('reason must ride the merged row');
  if (r.evidence !== 'bash argument references ~/.ssh/id_rsa') throw new Error('evidence must ride the merged row');
  if (r.judge !== 'glm-5.3-flash') throw new Error('model attribution');
  if (r.sessionId !== 's1') throw new Error('session attribution');
});

test('mergeSecurityFeed interleaves audit and AI rows, newest first, normalized', () => {
  const rows = mergeSecurityFeed(
    [
      { ts: 300, kind: 'secret', names: ['openai_api_key'], action: 'log', agent: 'pi', exposed: 'glm', request_id: 'r1', verdict: 'high', detail: 'live key' },
      { ts: 100, kind: 'path', names: ['ssh'], action: 'log' },
    ],
    [
      { ts: 200, kind: 'secret', rule: 'openai_api_key', verdict: 'low', reason: 'fixture', model: 'glm-5.3-flash', request_id: 'r9', session_id: 's1', cached: true },
    ],
  );
  if (rows.length !== 3) throw new Error('length');
  if (rows.map((r) => r.ts).join(',') !== '300,200,100') throw new Error('ordering');
  const audit = rows[0];
  if (audit.src !== 'audit' || audit.requestId !== 'r1' || audit.agent !== 'pi') throw new Error('audit projection');
  const ai = rows[1];
  if (ai.src !== 'ai' || ai.names[0] !== 'openai_api_key' || ai.exposed !== 'glm-5.3-flash' || !ai.cached) throw new Error('ai projection');
  const path = rows[2];
  if (path.strength !== 'strong') throw new Error('recorded path hits default to strong (weak is never recorded)');
  // legacy pre-decision-23 rows carried action=log-weak
  const legacy = mergeSecurityFeed([{ ts: 1, kind: 'path', names: ['ssh'], action: 'log-weak' }], []);
  if (legacy[0].strength !== 'weak') throw new Error('legacy log-weak derives weak');
});

test('mergeSecurityFeed folds a fresh verdict ring entry into its audit record', () => {
  // A fresh (uncached) verdict is written to BOTH the audit log (persistent,
  // with verdict+reason) and the ring — the same event must render once.
  const rows = mergeSecurityFeed(
    [
      { ts: 300, kind: 'secret', names: ['openai_api_key'], action: 'log', agent: 'pi', exposed: 'glm', request_id: 'r1', verdict: 'high', detail: 'live key' },
      { ts: 290, kind: 'path', names: ['ssh', 'aws_creds'], action: 'log', request_id: 'r1', verdict: 'skipped' },
    ],
    [
      { ts: 305, kind: 'secret', rule: 'openai_api_key', verdict: 'high', reason: 'live key', model: 'glm-5.3-flash', request_id: 'r1', session_id: 's1', cached: false },
      // cached occurrence: no second audit record — ring-only row stays
      { ts: 200, kind: 'secret', rule: 'openai_api_key', verdict: 'low', reason: 'fixture', model: 'glm-5.3-flash', request_id: 'r8', session_id: 's2', cached: true },
      // same request+rule but a DIFFERENT verdict: a distinct event, both rows
      { ts: 150, kind: 'secret', rule: 'github_token', verdict: 'error', reason: 'judge down', model: 'glm-5.3-flash', request_id: 'r2' },
    ],
  );
  if (rows.length !== 4) throw new Error('length ' + rows.length);
  const merged = rows.find((r) => r.src === 'audit' && r.verdict === 'high');
  if (!merged) throw new Error('merged audit row missing');
  if (merged.judge !== 'glm-5.3-flash' || merged.cached !== false || merged.sessionId !== 's1') throw new Error('judge attribution must ride along');
  if (merged.agent !== 'pi' || merged.names[0] !== 'openai_api_key') throw new Error('audit fields must survive the merge');
  if (rows.some((r) => r.src === 'ai' && r.requestId === 'r1' && r.verdict === 'high')) throw new Error('ring half of the deduped event must not render');
  const multi = rows.find((r) => r.requestId === 'r1' && r.verdict === 'skipped');
  if (!multi || multi.names.length !== 2) throw new Error('multi-name fail-open record stays its own row');
  const cached = rows.find((r) => r.src === 'ai' && r.cached);
  if (!cached) throw new Error('cached ring-only row keeps its ai row');
  const err = rows.filter((r) => r.requestId === 'r2');
  if (err.length !== 1 || err[0].src !== 'ai') throw new Error('verdict mismatch does not dedup');
  // classic (verdict-less) audit records never match: the ring entry for a
  // different request id stays a standalone ai row
  const classic = mergeSecurityFeed([{ ts: 10, kind: 'secret', names: ['x'], action: 'log', request_id: 'r3' }], [
    { ts: 11, kind: 'secret', rule: 'x', verdict: 'low', request_id: 'r4' },
  ]);
  if (classic.length !== 2) throw new Error('classic records do not participate in dedup');
});

test('security filter query round-trips through the URL hash', () => {
  if (securityFilterQuery(null) !== '') throw new Error('null');
  if (securityFilterQuery({ kind: '', verdict: '', range: 'all', rule: '' }) !== '') throw new Error('defaults stay a clean hash');
  const q = securityFilterQuery({ kind: 'secret', verdict: 'high', range: '7d', rule: 'openai_api_key' });
  if (q !== 'kind=secret&verdict=high&range=7d&rule=openai_api_key') throw new Error(q);
  const back = securityFilterFromQuery(hashQueryParams(q));
  if (!back || back.kind !== 'secret' || back.verdict !== 'high' || back.range !== '7d' || back.rule !== 'openai_api_key') throw new Error('round-trip');
  if (securityFilterFromQuery({}) !== null) throw new Error('no keys → null (bare hash must not clobber the filter)');
  if (securityFilterFromQuery(null) !== null) throw new Error('null params');
  // junk enum values drop back to defaults, free-text rule survives; 'low'
  // is junk for the verdict filter — the feed never renders low rows, so a
  // stale verdict=low bookmark must land on the unfiltered view
  const junk = securityFilterFromQuery({ kind: 'bogus', verdict: 'nope', range: '99d', rule: 'my_rule' });
  if (!junk || junk.kind !== '' || junk.verdict !== '' || junk.range !== 'all' || junk.rule !== 'my_rule') throw new Error('junk handling');
  const lowLink = securityFilterFromQuery({ verdict: 'low' });
  if (!lowLink || lowLink.verdict !== '') throw new Error('stale verdict=low degrades to unfiltered');
});

test('securityRangeFromSecs maps window presets to unix-second bounds', () => {
  const now = 1700000000000;
  if (securityRangeFromSecs('all', now) !== null) throw new Error('all → no bound');
  if (securityRangeFromSecs('unknown', now) !== null) throw new Error('unknown → no bound');
  if (securityRangeFromSecs('24h', now) !== 1700000000 - 86400) throw new Error('24h');
  if (securityRangeFromSecs('30d', now) !== 1700000000 - 30 * 86400) throw new Error('30d');
  if (SECURITY_RANGES.length !== 4 || SECURITY_RANGES[0].value !== 'all') throw new Error('preset table');
});

test('securityExplainHTML renders the LLM adjudication block', () => {
  const html = securityExplainHTML({
    status: 'ok',
    matches: [{ name: 'ssh', located: true, pre: 'a', hit: '~/.ssh', post: 'b' }],
    adjudications: [
      { rule: 'ssh', verdict: 'high', reason: 'exfil command', model: 'glm-5.3-flash' },
      { rule: 'openai_api_key', verdict: 'low', reason: 'fixture', cached: true },
    ],
  });
  if (!html.includes('LLM adjudication:')) throw new Error('header missing');
  if (!html.includes('badge err">high') || !html.includes('exfil command')) throw new Error('high entry');
  if (!html.includes('badge ok">low') || !html.includes('cached')) throw new Error('low entry + cached badge');
  if (!html.includes('glm-5.3-flash')) throw new Error('model attribution');
  // absent verdicts render nothing extra
  const bare = securityExplainHTML({ status: 'ok', matches: [{ name: 'x', located: false }] });
  if (bare.includes('LLM adjudication')) throw new Error('no adjudications must not render the block');
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

test('explainCacheKey normalizes name order and copies the input array', () => {
  const a = explainCacheKey('r1', 'secret', ['openai_api_key', 'ssh']);
  const b = explainCacheKey('r1', 'secret', ['ssh', 'openai_api_key']);
  assert.equal(a, b, 'same record with reordered names must share one key');
  assert.notEqual(a, explainCacheKey('r2', 'secret', ['openai_api_key', 'ssh']), 'different request');
  assert.notEqual(a, explainCacheKey('r1', 'path', ['openai_api_key', 'ssh']), 'different kind');
  const names = ['a', 'b'];
  explainCacheKey('r', 'secret', names);
  assert.deepEqual(names, ['a', 'b'], 'input array must not be sorted in place');
  assert.ok(explainCacheKey('', 'secret', ['x']).startsWith('\u0000'), 'empty request id stays distinct from a real one');
});

test('securityExplainHTML groups interleaved occurrences under one rule card', () => {
  const html = securityExplainHTML({
    status: 'ok',
    matches: [
      { name: 'ssh', strength: 'strong', located: true, pre: 'a', hit: '~/.ssh', post: 'b' },
      { name: 'openai_api_key', located: true, pre: 'x', hit: 'sk-…-ab', post: 'y' },
      { name: 'ssh', strength: 'strong', located: true, pre: 'c', hit: '~/id_rsa', post: 'd' },
    ],
  });
  const firstSsh = html.indexOf('~/.ssh');
  const apiKey = html.indexOf('sk-…-ab');
  const secondSsh = html.indexOf('~/id_rsa');
  assert.ok(firstSsh >= 0 && apiKey >= 0 && secondSsh >= 0, 'all occurrences rendered');
  // grouping is the point: one rule's occurrences render adjacently in its
  // card instead of staying interleaved in body-offset order
  assert.ok(firstSsh < secondSsh && secondSsh < apiKey, 'same-rule occurrences group into one card');
  // one rule card per name: identity and explanation appear once, occurrences numbered
  assert.equal(html.split('sec-explain-rule-head').length - 1, 2, 'two rule cards');
  assert.ok(html.includes('#1') && html.includes('#2'), 'occurrences numbered when a rule fired twice');
  assert.ok(!html.includes('#1') || !html.includes('#3'), 'no phantom third occurrence');
});

test('securityExplainHTML single occurrences render without an index label', () => {
  const html = securityExplainHTML({
    status: 'ok',
    matches: [{ name: 'ssh', located: true, pre: 'a', hit: '~/.ssh', post: 'b' }],
  });
  assert.ok(!html.includes('sec-explain-occ-n'), 'one occurrence needs no #1 label');
  assert.ok(html.includes('<mark>~/.ssh</mark>'));
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

// ---------- v2 presentation helpers ----------

test('icon builders emit themed inline SVG with currentColor stroke', () => {
  for (const svg of [iconPin(), iconRefresh(), iconChevron()]) {
    assert.ok(svg.startsWith('<svg class="icon'), svg);
    assert.match(svg, /viewBox="0 0 24 24"/);
    assert.match(svg, /stroke="currentColor"/);
    assert.match(svg, /aria-hidden="true"/);
  }
  assert.match(iconPin(), /M12 17v5/);
  assert.match(iconChevron(), /class="icon icon-chevron"/);
});

test('statusBadgeClass splits HTTP classes semantically', () => {
  assert.equal(statusBadgeClass(200), 'ok');
  assert.equal(statusBadgeClass(204), 'ok');
  assert.equal(statusBadgeClass(301), 'warn');
  assert.equal(statusBadgeClass(404), 'warn');
  assert.equal(statusBadgeClass(429), 'warn');
  assert.equal(statusBadgeClass(500), 'err');
  assert.equal(statusBadgeClass(503), 'err');
  assert.equal(statusBadgeClass(null), 'muted');
  assert.equal(statusBadgeClass(undefined), 'muted');
  assert.equal(statusBadgeClass(0), 'muted');
  assert.equal(statusBadgeClass('x'), 'muted');
});

test('statusBadgeHTML escapes and marks pending rows', () => {
  assert.equal(statusBadgeHTML(200), '<span class="badge ok">200</span>');
  assert.equal(statusBadgeHTML(500), '<span class="badge err">500</span>');
  assert.equal(statusBadgeHTML(null), '<span class="badge muted">\u2014</span>');
  assert.equal(statusBadgeHTML('<x>', true), '<span class="badge muted">\u00b7\u00b7\u00b7</span>');
  assert.equal(statusBadgeHTML('<script>'), '<span class="badge muted">\u2014</span>');
});

test('kpiDeltaClass colors up as ok, bad-increase metrics as err', () => {
  assert.equal(kpiDeltaClass(12.5), 'up');
  assert.equal(kpiDeltaClass(-3), 'down');
  assert.equal(kpiDeltaClass(0), 'flat');
  assert.equal(kpiDeltaClass(null), 'flat');
  assert.equal(kpiDeltaClass(5, true), 'up bad');
  assert.equal(kpiDeltaClass(-5, true), 'down');
});

test('logLineHTML colors timestamp, severity and key=value tokens', () => {
  const html = logLineHTML('2026/09/11 20:07:41 INFO proxy status=200 provider=zhipu model=glm-5.3');
  assert.match(html, /^<span class="log-ts">2026\/09\/11 20:07:41<\/span> /);
  assert.match(html, /<span class="log-lvl lvl">INFO<\/span>/);
  assert.match(html, /<span class="log-k">status<\/span>=<span class="log-v ok">200<\/span>/);
  assert.match(html, /<span class="log-k">provider<\/span>=<span class="log-v">zhipu<\/span>/);
});

test('logLineHTML colors WARN/ERROR severity and semantic error values', () => {
  const warn = logLineHTML('2026/09/11 20:07:41 WARN upstream retry_in=10s error_msg=boom status=503');
  assert.match(warn, /<span class="log-lvl warn">WARN<\/span>/);
  assert.match(warn, /<span class="log-k">retry_in<\/span>=<span class="log-v warn">10s<\/span>/);
  assert.match(warn, /<span class="log-k">error_msg<\/span>=<span class="log-v err">boom<\/span>/);
  assert.match(warn, /<span class="log-k">status<\/span>=<span class="log-v err">503<\/span>/);
  const err = logLineHTML('2026/09/11 20:07:41 ERROR failover failed');
  assert.match(err, /<span class="log-lvl err">ERROR<\/span>/);
  assert.ok(err.endsWith(' failover failed'), err);
});

test('logLineHTML escapes hostile log content', () => {
  const html = logLineHTML('2026/09/11 20:07:41 INFO msg=<img src=x onerror=alert(1)> a=b');
  assert.ok(!html.includes('<img'), html);
  assert.ok(!html.includes('onerror='), html);
  assert.match(html, /<span class="log-v">&lt;img<\/span>/);
  assert.match(html, /<span class="log-v">alert\(1\)&gt;<\/span>/);
  assert.match(html, /<span class="log-k">a<\/span>=<span class="log-v">b<\/span>/);
});

test('logLineHTML handles lines without timestamp, severity or pairs', () => {
  assert.equal(logLineHTML('plain text only'), 'plain text only');
  assert.equal(logLineHTML('no-ts status=200'), 'no-ts <span class="log-k">status</span>=<span class="log-v ok">200</span>');
  assert.equal(logLineHTML('a =b stray = x c=1'), 'a =b stray = x <span class="log-k">c</span>=<span class="log-v">1</span>');
});

test('ruleHitsLeaderboard counts audit names plus suppressed lows, sorts by hits', () => {
  const records = [
    { ts: 100, kind: 'secret', names: ['openai_api_key', 'jwt'] },
    { ts: 200, kind: 'secret', names: ['openai_api_key'] },
    { ts: 300, kind: 'path', names: ['ssh'] },
    { ts: 0, kind: 'secret', names: ['known_secret'] },
  ];
  const adjudications = [
    { ts: 400, kind: 'secret', rule: 'openai_api_key', verdict: 'low' },   // suppressed: counts, no audit twin
    { ts: 500, kind: 'secret', rule: 'custom_pat', verdict: 'high' },      // high HAS an audit record — must not double-count its absence here
  ];
  const rows = ruleHitsLeaderboard(records, adjudications);
  assert.equal(rows[0].name, 'openai_api_key');
  assert.equal(rows[0].hits, 3); // 2 audit + 1 suppressed low
  assert.equal(rows[0].lastTs, 400);
  assert.equal(rows[0].adjudicable, true);
  // The known-secret exact channel never defers.
  const known = rows.find((r) => r.name === 'known_secret');
  assert.equal(known.adjudicable, false);
  // 'custom_pat' has no audit record and its verdict is high, not low — the
  // leaderboard counts only suppressed lows from the ring.
  assert.equal(rows.some((r) => r.name === 'custom_pat'), false);
  // Ties fall back to newest last hit; empty input normalizes.
  assert.deepEqual(ruleHitsLeaderboard(null, null), []);
  assert.deepEqual(ruleHitsLeaderboard([{ names: null }], []), []);
});

test('sessionTimeline stacks overlapping spans into new lanes, sequential into one', () => {
  const rows = [
    // Two overlapping requests (0-100, 50-150) → two lanes; the third starts
    // after the first frees its lane (200 > 100) → reuses lane 0.
    { requestId: 'a', ts: 1000, latencyMs: 100, status: 200, input: 10, output: 5 },
    { requestId: 'b', ts: 1050, latencyMs: 100, status: 200, input: 0, output: 0 },
    { requestId: 'c', ts: 1200, latencyMs: 50, status: 200, input: 0, output: 5 },
  ];
  const tl = sessionTimeline(rows, { fmt: (t) => String(t) });
  assert.equal(tl.lanes, 2);
  assert.equal(tl.skipped, 0);
  // Bar order is chronological regardless of input order; each bar carries
  // its request id as the click target.
  const ids = [...tl.svg.matchAll(/data-id="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(ids, ['a', 'b', 'c']);
  // Error rows carry the error class; retried rows carry the amber mark.
  const err = sessionTimeline([
    { requestId: 'x', ts: 0, latencyMs: 10, status: 502 },
    { requestId: 'y', ts: 5000, latencyMs: 10, status: 200, attempt: 2 },
  ], { fmt: (t) => String(t) });
  assert.ok(/tl-err/.test(err.svg));
  assert.ok(/tl-retry/.test(err.svg));
  assert.ok(/attempt 3/.test(err.svg));
});

test('sessionTimeline cumulative tokens line spans bottom-left to top-right', () => {
  const rows = [
    { requestId: 'a', ts: 0, latencyMs: 100, input: 10, output: 0 },
    { requestId: 'b', ts: 1000, latencyMs: 100, input: 0, output: 30 },
  ];
  const tl = sessionTimeline(rows, { fmt: (t) => String(t) });
  const pts = /<polyline class="tl-tokens" points="([^"]+)"/.exec(tl.svg);
  assert.ok(pts, 'tokens polyline missing');
  const coords = pts[1].split(' ').map((p) => p.split(',').map(Number));
  assert.equal(coords.length, 2);
  // First point at 10/40 of the height; second at the full height.
  assert.ok(coords[0][1] > coords[1][1], 'cumulative line must rise');
  // A session with no token usage omits the line entirely.
  const none = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 10, status: 200 },
    { requestId: 'b', ts: 999, latencyMs: 10, status: 200 },
  ], { fmt: (t) => String(t) });
  assert.ok(!/tl-tokens/.test(none.svg));
});

test('sessionTimeline tolerates unparseable rows and single-row sessions', () => {
  assert.deepEqual(sessionTimeline([], {}).svg, '');
  const one = sessionTimeline([{ requestId: 'a', ts: 5, latencyMs: 5 }], {});
  assert.equal(one.svg, '');
  const mixed = sessionTimeline([
    { requestId: 'a', ts: 'garbage', latencyMs: 5 },
    { requestId: 'b', ts: 1000, latencyMs: 5 },
    { requestId: 'c', ts: 2000, latencyMs: 5 },
  ], { fmt: (t) => String(t) });
  assert.equal(mixed.skipped, 1);
  assert.equal(mixed.lanes, 1);
  // In-flight rows (no latency yet) run to the right edge visually.
  const run = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 5 },
    { requestId: 'b', ts: 1000, inFlight: true },
  ], { fmt: (t) => String(t) });
  assert.ok(/tl-run/.test(run.svg));
});

test('sessionTimeline compresses long idle gaps into a segmented axis', () => {
  // A 30s burst followed by a 6h idle stretch and one more request: without
  // segmentation the burst would squeeze into ~0.1% of the axis width.
  const rows = [
    { requestId: 'a', ts: 0, latencyMs: 10000, status: 200 },
    { requestId: 'b', ts: 15000, latencyMs: 10000, status: 200 },
    { requestId: 'c', ts: 6 * 3600 * 1000, latencyMs: 10000, status: 200 },
  ];
  const tl = sessionTimeline(rows, { fmt: (t) => String(t) });
  assert.equal(tl.segments.length, 2, 'the 6h gap must break the axis');
  assert.ok(/tl-break/.test(tl.svg), 'a divider marks the compressed gap');
  // Segment order is chronological, x ranges are monotonic with a break
  // strip between them, and the burst keeps a readable share of the width.
  const [s0, s1] = tl.segments;
  assert.ok(s1.x0 > s0.x1 + 10, 'a reserved strip separates the segments');
  assert.ok(s0.x1 - s0.x0 > 250, `burst segment must stay readable, got ${s0.x1 - s0.x0}px`);
  assert.ok(s0.t0 <= 0 && s0.t1 >= 20000, 'segment 0 spans the burst');
  assert.equal(s1.t0, 6 * 3600 * 1000);
  // A tight session (all gaps under the 2-minute default) stays one segment
  // with no divider.
  const tight = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 1000 },
    { requestId: 'b', ts: 5000, latencyMs: 1000 },
  ], { fmt: (t) => String(t) });
  assert.equal(tight.segments.length, 1);
  assert.ok(!/tl-break/.test(tight.svg));
  // gapMs overrides the threshold: a 5s gap splits at 1s, not at 2 minutes.
  const fussy = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 500 },
    { requestId: 'b', ts: 5000, latencyMs: 500 },
  ], { fmt: (t) => String(t), gapMs: 1000 });
  assert.equal(fussy.segments.length, 2);
});

test('sessionTimeline segments by turn_key when both rows have one', () => {
  const sameTurn = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 500, turnKey: 'turn-1' },
    { requestId: 'b', ts: 1000, latencyMs: 500, turnKey: 'turn-1' },
    { requestId: 'c', ts: 2000, latencyMs: 500, turnKey: 'turn-1' },
  ], { fmt: (t) => String(t) });
  assert.equal(sameTurn.segments.length, 1, 'identical turn keys stay in one segment');

  const differentTurns = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 500, turnKey: 'turn-1' },
    { requestId: 'b', ts: 1000, latencyMs: 500, turnKey: 'turn-2' },
    { requestId: 'c', ts: 2000, latencyMs: 500, turnKey: 'turn-3' },
  ], { fmt: (t) => String(t) });
  assert.equal(differentTurns.segments.length, 3, 'each different turn key starts a new segment');

  // A turn-key change wins over a short gap that would not trigger the old
  // 2-minute heuristic.
  const quickSwitch = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 500, turnKey: 'turn-1' },
    { requestId: 'b', ts: 5000, latencyMs: 500, turnKey: 'turn-2' },
  ], { fmt: (t) => String(t) });
  assert.equal(quickSwitch.segments.length, 2, 'turn boundary splits even under 2 minutes');
});

test('sessionTimeline keeps every segment inside the viewBox for many-turn sessions', () => {
  // Agentic sessions split one segment per turn; 100 turns must not push the
  // tail past the right edge — the compressed-gap strip shrinks instead of
  // silently clipping bars while the title still counts them.
  const rows = Array.from({ length: 100 }, (_, i) => ({
    requestId: 'r' + i, ts: i * 10000, latencyMs: 1000, status: 200, turnKey: 'turn-' + i,
  }));
  const tl = sessionTimeline(rows, { fmt: (t) => String(t) });
  assert.equal(tl.segments.length, 100);
  const right = 900 - 8; // W - padR
  for (const s of tl.segments) {
    assert.ok(s.x0 >= 8 - 1e-6 && s.x1 <= right + 1e-6, `segment out of viewBox: ${s.x0}..${s.x1}`);
  }
  assert.equal(tl.svg.match(/<rect /g).length, 100, 'every request bar renders');
  // The small-segment floor must never overflow either: one dominant turn
  // plus 11 tiny ones still fits the axis.
  const mixed = sessionTimeline([
    { requestId: 'big', ts: 0, latencyMs: 3600000, turnKey: 't0' },
    ...Array.from({ length: 11 }, (_, i) => ({ requestId: 's' + i, ts: 3600000 + (i + 1) * 10000, latencyMs: 100, turnKey: 't' + (i + 1) })),
  ], { fmt: (t) => String(t) });
  assert.equal(mixed.segments.length, 12);
  for (const s of mixed.segments) {
    assert.ok(s.x1 <= right + 1e-6, `floored segment out of viewBox: ${s.x1}`);
  }
});

test('sessionTimeline falls back to idle gap when turn_key is missing', () => {
  // Both rows lack a turn key: the legacy 2-minute gap heuristic applies.
  const emptyGap = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 500 },
    { requestId: 'b', ts: 6 * 60 * 1000, latencyMs: 500 },
  ], { fmt: (t) => String(t) });
  assert.equal(emptyGap.segments.length, 2);

  // A mix: the keyed row keeps its boundary with an empty-key neighbour by
  // falling back to the time heuristic.
  const mixedShort = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 500, turnKey: 'turn-1' },
    { requestId: 'b', ts: 1000, latencyMs: 500 },
  ], { fmt: (t) => String(t) });
  assert.equal(mixedShort.segments.length, 1, 'short gap with an empty turn key stays together');

  const mixedLong = sessionTimeline([
    { requestId: 'a', ts: 0, latencyMs: 500, turnKey: 'turn-1' },
    { requestId: 'b', ts: 6 * 60 * 1000, latencyMs: 500 },
  ], { fmt: (t) => String(t) });
  assert.equal(mixedLong.segments.length, 2, 'long gap with an empty turn key still splits');
});

test('sessionTimeline zoom window filters rows and owns the axis domain', () => {
  const rows = [
    { requestId: 'a', ts: 0, latencyMs: 100, status: 200 },
    { requestId: 'b', ts: 1000, latencyMs: 100, status: 200 },
    { requestId: 'c', ts: 3600 * 1000, latencyMs: 100, status: 200 },
  ];
  const ids = (svg) => [...svg.matchAll(/data-id="([^"]+)"/g)].map((m) => m[1]);
  // Zooming into the first two requests drops the hour-later row and re-bases
  // the axis on what remains; a single visible row still renders (the reset
  // chip must stay reachable).
  const zoomed = sessionTimeline(rows, { fmt: (t) => String(t), window: { from: -100, to: 2000 } });
  assert.deepEqual(ids(zoomed.svg), ['a', 'b']);
  assert.ok(zoomed.segments.length >= 1);
  assert.equal(zoomed.shown, 2, 'title count tracks the zoomed rows');
  const lone = sessionTimeline(rows, { fmt: (t) => String(t), window: { from: -100, to: 500 } });
  assert.deepEqual(ids(lone.svg), ['a']);
  assert.equal(lone.shown, 1);
  // A window that catches nothing falls back to the full view instead of
  // blanking the card (a stray drag must never orphan the reset control).
  const missed = sessionTimeline(rows, { fmt: (t) => String(t), window: { from: 100 * 3600 * 1000, to: 101 * 3600 * 1000 } });
  assert.deepEqual(ids(missed.svg), ['a', 'b', 'c']);
  assert.equal(missed.shown, 3, 'fallback to the full view restores the full count');
  // A degenerate window (to <= from) is ignored outright.
  const junk = sessionTimeline(rows, { fmt: (t) => String(t), window: { from: 500, to: 500 } });
  assert.deepEqual(ids(junk.svg), ['a', 'b', 'c']);
  assert.equal(junk.shown, 3);
  // Without a window the count is every parseable row (unparseable ones are
  // skipped, not counted).
  const withJunkRow = sessionTimeline(rows.concat([{ requestId: 'z', ts: 'not-a-time' }]), { fmt: (t) => String(t) });
  assert.equal(withJunkRow.shown, 3);
  assert.equal(withJunkRow.skipped, 1);
});

test('sessionTimeline compresses lane height for busy parallel sessions', () => {
  // 30 fully-overlapping requests stack 30 lanes; past 14 lanes the row
  // height steps down so the card cannot dwarf the table below it.
  const rows = Array.from({ length: 30 }, (_, i) => ({
    requestId: 'r' + i, ts: 0, latencyMs: 5000, status: 200,
  }));
  const tl = sessionTimeline(rows, { fmt: (t) => String(t) });
  assert.equal(tl.lanes, 30);
  const h = Number(/viewBox="0 0 900 (\d+(?:\.\d+)?)"/.exec(tl.svg)[1]);
  assert.equal(h, 8 + 30 * 12 + 18, 'compact metrics: laneH 12 not 20');
});

test('sessionBarSummary renders full and sparse rows, dropping absent fields', () => {
  const fmt = (t) => 'T' + t;
  const full = sessionBarSummary({
    ts: 5000, agent: 'claude-code', model: 'glm-5.3', provider: 'zhipu',
    status: 200, latencyMs: 4172, input: 310, output: 132, cacheRead: 2400,
    attempt: 2, inFlight: false,
  }, fmt);
  assert.deepEqual(full, [
    'T5000 · claude-code',
    'glm-5.3 → zhipu',
    '200 · 4.2s · in 310 / out 132 tok · cache 2.4k',
    'attempt 3 (failover)',
  ]);
  // Missing fields drop out line by line; latency under 1s stays in ms.
  const sparse = sessionBarSummary({ ts: 0, status: 502, latencyMs: 300 }, fmt);
  assert.deepEqual(sparse, ['T0', '502 · 300ms']);
  // In-flight note; null row says nothing.
  assert.deepEqual(sessionBarSummary({ ts: 1, inFlight: true }, fmt), ['T1', 'in flight']);
  assert.deepEqual(sessionBarSummary(null, fmt), []);
  // RFC3339 timestamps go through the caller's fmt like numbers do.
  assert.equal(sessionBarSummary({ ts: '2026-09-12T01:02:03Z' }, (t) => new Date(t).toISOString()).length, 1);
});

test('responseExcerpt extracts assistant text from all three protocol shapes', () => {
  // anthropic JSON message (thinking blocks skipped, text joined)
  const anthropic = JSON.stringify({
    content: [
      { type: 'thinking', thinking: 'internal reasoning must not leak' },
      { type: 'text', text: '  Hello   there. ' },
      { type: 'text', text: 'Second block.' },
    ],
  });
  assert.equal(responseExcerpt(anthropic, 100), 'Hello there. Second block.');
  // openai chat, string content
  const openai = JSON.stringify({ choices: [{ message: { content: 'Hi from openai' } }] });
  assert.equal(responseExcerpt(openai, 50), 'Hi from openai');
  // openai chat, array content pieces
  const openaiArr = JSON.stringify({ choices: [{ message: { content: [{ type: 'text', text: 'part A ' }, { type: 'text', text: 'part B' }] } }] });
  assert.equal(responseExcerpt(openaiArr, 50), 'part A part B');
  // responses API shape
  const responses = JSON.stringify({ output: [{ content: [{ type: 'output_text', text: 'responses shape' }] }] });
  assert.equal(responseExcerpt(responses, 50), 'responses shape');
});

test('responseExcerpt folds SSE deltas, caps length, and bails safely', () => {
  const sse = [
    'data: ' + JSON.stringify({ type: 'content_block_delta', delta: { type: 'text_delta', text: 'Stre' } }),
    '',
    'data: ' + JSON.stringify({ type: 'content_block_delta', delta: { type: 'text_delta', text: 'amed!' } }),
    'data: [DONE]',
  ].join('\n');
  assert.equal(responseExcerpt(sse, 50), 'Streamed!');
  // Cap with ellipsis; whitespace collapses.
  const long = JSON.stringify({ choices: [{ message: { content: 'x'.repeat(50) + '   tail' } }] });
  assert.equal(responseExcerpt(long, 10), 'xxxxxxxxxx…');
  // Non-JSON plain text (upstream error page) falls back to the raw string.
  assert.equal(responseExcerpt('upstream connect error', 40), 'upstream connect error');
  // Garbage JSON, empty, and oversize bodies yield '' — never throw.
  assert.equal(responseExcerpt('{"broken": ', 10), '');
  assert.equal(responseExcerpt('', 10), '');
  assert.equal(responseExcerpt('x'.repeat(1500001), 10), '');
  assert.equal(responseExcerpt(null, 10), '');
});

test('requestExcerpt surfaces the newest human text across shapes and agents', () => {
  // anthropic: string content; newest user message wins.
  const str = JSON.stringify({ messages: [
    { role: 'user', content: 'older turn' },
    { role: 'assistant', content: 'reply' },
    { role: 'user', content: 'fix the login bug' },
  ] });
  assert.equal(requestExcerpt(str, 50), 'fix the login bug');
  // Coding-agent turn: the LAST user message is tool_result-only — the
  // newest human text sits in an earlier user message and must surface.
  const agentTurn = JSON.stringify({ messages: [
    { role: 'user', content: [
      { type: 'tool_result', tool_use_id: 't1', content: 'file contents' },
      { type: 'text', text: 'keep going' },
    ] },
    { role: 'assistant', content: [{ type: 'text', text: 'ok' }] },
    { role: 'user', content: [
      { type: 'tool_result', tool_use_id: 't2', content: 'more output' },
    ] },
  ] });
  assert.equal(requestExcerpt(agentTurn, 50), 'keep going');
  // openai array content pieces.
  const openai = JSON.stringify({ messages: [
    { role: 'user', content: [{ type: 'text', text: 'part A ' }, { type: 'text', text: 'part B' }] },
  ] });
  assert.equal(requestExcerpt(openai, 50), 'part A part B');
  // responses shape: plain string input and message array.
  assert.equal(requestExcerpt(JSON.stringify({ input: 'string input' }), 50), 'string input');
  assert.equal(requestExcerpt(JSON.stringify({ input: [
    { role: 'user', content: 'array input' },
  ] }), 50), 'array input');
  // Cap with ellipsis; non-user tails never leak.
  const capped = JSON.stringify({ messages: [{ role: 'user', content: 'y'.repeat(30) }] });
  assert.equal(requestExcerpt(capped, 10), 'yyyyyyyyyy…');
  // No user text / garbage / oversize → ''.
  assert.equal(requestExcerpt(JSON.stringify({ messages: [{ role: 'assistant', content: 'no' }] }), 10), '');
  assert.equal(requestExcerpt('not json', 10), '');
  assert.equal(requestExcerpt('x'.repeat(1500001), 10), '');
});

test('responseExcerpt falls back to tool/thinking/error markers on text-less turns', () => {
  // SSE coding-agent turn: thinking block + tool_use block, no text deltas.
  const sse = [
    'data: ' + JSON.stringify({ type: 'content_block_start', content_block: { type: 'thinking' } }),
    'data: ' + JSON.stringify({ type: 'content_block_delta', delta: { type: 'thinking_delta', thinking: 'must not leak' } }),
    'data: ' + JSON.stringify({ type: 'content_block_start', content_block: { type: 'tool_use', id: 't1', name: 'Bash', input: {} } }),
    'data: ' + JSON.stringify({ type: 'content_block_delta', delta: { type: 'input_json_delta', partial_json: '{"cmd":' } }),
    'data: ' + JSON.stringify({ type: 'message_stop' }),
  ].join('\n');
  assert.equal(responseExcerpt(sse, 60), '[tool_use: Bash]');
  // Thinking-only turn.
  const think = 'data: ' + JSON.stringify({ type: 'content_block_start', content_block: { type: 'thinking' } });
  assert.equal(responseExcerpt(think, 30), '[thinking]');
  // JSON message with tool_use blocks (deduped names) — anthropic + openai
  // tool_calls + responses function_call all note their tool names.
  const anthropicTools = JSON.stringify({ content: [
    { type: 'tool_use', id: 't1', name: 'Bash', input: {} },
    { type: 'tool_use', id: 't2', name: 'Edit', input: {} },
    { type: 'tool_use', id: 't3', name: 'Bash', input: {} },
  ] });
  assert.equal(responseExcerpt(anthropicTools, 60), '[tool_use: Bash, Edit]');
  const openaiTools = JSON.stringify({ choices: [{ message: { content: null, tool_calls: [
    { function: { name: 'get_weather' } },
  ] } }] });
  assert.equal(responseExcerpt(openaiTools, 60), '[tool_use: get_weather]');
  const responsesTool = JSON.stringify({ output: [{ type: 'function_call', name: 'search' }] });
  assert.equal(responseExcerpt(responsesTool, 60), '[tool_use: search]');
  // Error JSON surfaces its message as the text itself.
  assert.equal(responseExcerpt(JSON.stringify({ error: { message: 'quota exceeded' } }), 60), 'quota exceeded');
  // Text still wins over markers when present.
  const mixed = JSON.stringify({ content: [
    { type: 'tool_use', id: 't1', name: 'Bash', input: {} },
    { type: 'text', text: 'ran it' },
  ] });
  assert.equal(responseExcerpt(mixed, 60), 'ran it');
});

test('chatViewHTML renders a readable transcript with collapsed aux blocks', () => {
  const req = JSON.stringify({
    system: 'You are helpful.',
    messages: [
      { role: 'user', content: 'first turn' },
      { role: 'assistant', content: [{ type: 'text', text: 'first answer' }] },
      { role: 'user', content: 'mid turn' },
      { role: 'assistant', content: [{ type: 'text', text: 'mid answer' }] },
      { role: 'user', content: [{ type: 'tool_result', content: 'ls output' }, { type: 'text', text: 'keep going' }] },
    ],
  });
  const res = JSON.stringify({
    content: [
      { type: 'thinking', thinking: 'plan it' },
      { type: 'tool_use', name: 'Bash', input: { command: 'ls -la' } },
      { type: 'text', text: 'all <done>' },
    ],
    usage: { input_tokens: 10, output_tokens: 5 },
  });
  const html = chatViewHTML(req, res, 'application/json', { histKey: 'raw-77' });
  // Structure: system + LAZY history fold, roles labeled, escaping intact.
  assert.ok(html.includes('cv-system'), 'system collapses');
  assert.ok(html.includes('1 earlier turn'), 'history collapses beyond the recent window');
  assert.ok(html.includes('data-raw="raw-77"'), 'history fold carries the lazy registry key');
  // The earlier turns do NOT ride the initial HTML — only a placeholder does.
  assert.ok(html.includes('renders on first expand'));
  assert.ok(!html.includes('<div class="cv-text">first turn</div>'), 'earlier turn content stays out of the DOM');
  assert.ok(html.includes('<div class="cv-text">keep going</div>'), 'recent turns render');
  assert.ok(html.includes('all &lt;done&gt;'), 'text is escaped');
  assert.ok(html.includes('<details class="cv-fold"><summary>thinking</summary>'), 'thinking collapses');
  assert.ok(html.includes('<summary>tool result</summary>'), 'tool results collapse');
  assert.ok(html.includes('<span class="cv-tool-name">Bash</span>'), 'tool call named');
  assert.ok(html.includes('command: ls -la'), 'tool args render as readable text, not JSON');
  assert.ok(html.includes('in 10 · out 5'), 'usage line');
  assert.ok(html.includes('>user</span><div class="cv-text">mid turn</div>'), 'recent window keeps its turns');
  // Lazy expansion feeds a flat chunked transcript: parse once, render
  // arbitrary ranges (the scroll loader appends 25-turn chunks).
  const parsed = parseChatRequest(req);
  assert.equal(parsed.messages.length, 5);
  const end = parsed.messages.length - CHAT_RECENT;
  const firstChunk = chatTurnsSliceHTML(parsed.messages, 0, 1);
  assert.ok(firstChunk.includes('<div class="cv-text">first turn</div>'), 'chunk renders full turns');
  assert.ok(!firstChunk.includes('mid turn'), 'range respects its bounds');
  assert.equal(chatTurnsSliceHTML(parsed.messages, 0, end).split('cv-msg').length - 1, end, 'full history renders every turn');
  assert.equal(chatTurnsSliceHTML(parsed.messages, 3, 99).split('cv-msg').length - 1, 2, 'upper bound clamps');
  assert.equal(chatTurnsSliceHTML(null, 0, 5), '');
  assert.equal(chatTurnsSliceHTML(parsed.messages, 2, 2), '');
  assert.equal(parseChatRequest('not json'), null);
});

test('chatViewHTML folds SSE streams (anthropic + openai) with usage', () => {
  const anthropicSSE = [
    'data: ' + JSON.stringify({ type: 'message_start', message: { usage: { input_tokens: 7, cache_read_input_tokens: 40 } } }),
    'data: ' + JSON.stringify({ type: 'content_block_start', index: 0, content_block: { type: 'tool_use', name: 'Edit', input: {} } }),
    'data: ' + JSON.stringify({ type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: '{"path":"a.ts' } }),
    'data: ' + JSON.stringify({ type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: '"}' } }),
    'data: ' + JSON.stringify({ type: 'content_block_delta', index: 1, delta: { type: 'text_delta', text: 'He' } }),
    'data: ' + JSON.stringify({ type: 'content_block_delta', index: 1, delta: { type: 'text_delta', text: 'y' } }),
    'data: ' + JSON.stringify({ type: 'message_delta', usage: { output_tokens: 3 } }),
  ].join('\n');
  const req = JSON.stringify({ messages: [{ role: 'user', content: 'do it' }] });
  const html = chatViewHTML(req, anthropicSSE, 'text/event-stream');
  assert.ok(html.includes('<span class="cv-tool-name">Edit</span>'), 'streamed tool call named');
  assert.ok(html.includes('path: a.ts'), 'partial_json fragments parse back to readable args');
  assert.ok(html.includes('>user</span><div class="cv-text">do it</div>'));
  assert.ok(/assistant[\s\S]*<div class="cv-text">Hey<\/div>/.test(html), 'text deltas join');
  assert.ok(html.includes('in 7 · out 3 · cache read 40'), 'usage folded from stream events');
  // openai chunks: content pieces + tool_calls accumulation.
  const openaiSSE = [
    'data: ' + JSON.stringify({ choices: [{ delta: { content: 'Hi ' } }] }),
    'data: ' + JSON.stringify({ choices: [{ delta: { content: 'there', tool_calls: [{ index: 0, function: { name: 'sea', arguments: '{"q":' } }] } }] }),
    'data: ' + JSON.stringify({ choices: [{ delta: { tool_calls: [{ index: 0, function: { arguments: '1}' } }] } }] }),
    'data: ' + JSON.stringify({ choices: [], usage: { prompt_tokens: 4, completion_tokens: 2 } }),
  ].join('\n');
  const html2 = chatViewHTML('', openaiSSE, '');
  assert.ok(html2.includes('<div class="cv-text">Hi there</div>'), 'openai content joins');
  assert.ok(html2.includes('<span class="cv-tool-name">sea</span>'), 'openai tool call named');
  assert.ok(html2.includes('q: 1'), 'openai tool args accumulate readable');
  assert.ok(html2.includes('in 4 · out 2'), 'openai usage');
});

test('chatViewHTML bails out safely on unusable bodies', () => {
  // Errors render as an error block; everything unparseable yields ''.
  const errBody = JSON.stringify({ error: { message: 'rate limited <fast>' } });
  assert.ok(chatViewHTML('', errBody, '').includes('msg err'));
  assert.ok(chatViewHTML('', errBody, '').includes('rate limited &lt;fast&gt;'));
  assert.equal(chatViewHTML('not json', 'also not json', ''), '');
  assert.equal(chatViewHTML('', '', ''), '');
  assert.equal(chatViewHTML('x'.repeat(1500001), '', ''), '');
  // Oversize single blocks cap with a visible note instead of flooding DOM.
  const big = JSON.stringify({ messages: [{ role: 'user', content: 'z'.repeat(5000) }] });
  assert.ok(chatViewHTML(big, '', '').includes('+1k chars, see raw body'));
});

test('readableValue flattens JSON into human text', () => {
  assert.equal(readableValue('plain'), 'plain');
  assert.equal(readableValue(42), '42');
  assert.equal(readableValue(null), 'null');
  assert.equal(readableValue({ command: 'ls -la', timeout: 5000 }), 'command: ls -la\ntimeout: 5000');
  // Nested containers indent; small scalar arrays comma-join.
  assert.equal(readableValue({ file: 'a.ts', edits: [{ old: 'x', new: 'y' }] }),
    'file: a.ts\nedits:\n  - old: x\n    new: y');
  assert.equal(readableValue({ models: ['a', 'b', 'c'] }), 'models: a, b, c');
  assert.equal(readableValue({ empty: {}, none: [] }), 'empty: {}\nnone: []');
  // A JSON tool result renders through it (chat tool_result path).
  const res = JSON.stringify({ content: [{ type: 'tool_use', name: 'jq', input: { filter: '.name' } }] });
  assert.ok(chatViewHTML('', res, '').includes('filter: .name'));
});

test('requests filter hash round-trips, omitting defaults and dropping junk', () => {
  // Only non-default values ride along; an all-default filter yields no query.
  // The stream is NOT a query key — it lives in the hash segment
  // (#requests/<stream>_<view>, app.js requestsViewKey), so it neither emits
  // nor parses here (a stray legacy ?stream= is absorbed by
  // normalizeRequestsHash before FromQuery ever runs).
  assert.equal(requestsFilterQuery({ stream: '', session: '', agent: '', model: '', provider: '', errors: false, shadow: '' }), '');
  assert.equal(requestsFilterQuery({ stream: 'mcp', session: '', agent: '', model: '', provider: '', errors: false, shadow: '' }), '');
  const q = requestsFilterQuery({ stream: '', session: 'sess demo/1', agent: 'pi', model: '', provider: 'zhipu', errors: true, shadow: 'only' });
  assert.equal(q, 'session=sess+demo%2F1&agent=pi&provider=zhipu&errors=1&shadow=only');
  // Round-trip through hashQueryParams + fromQuery.
  const back = requestsFilterFromQuery(hashQueryParams(q));
  assert.deepEqual(back, { stream: '', session: 'sess demo/1', agent: 'pi', model: '', provider: 'zhipu', errors: true, shadow: 'only' });
  const mcp = requestsFilterQuery({ stream: 'mcp', session: '', agent: 'codex', model: '', provider: '', errors: false, shadow: '' });
  assert.equal(mcp, 'agent=codex');
  assert.deepEqual(requestsFilterFromQuery(hashQueryParams(mcp)), { stream: '', session: '', agent: 'codex', model: '', provider: '', errors: false, shadow: '' });
  // A stream-only query is not a filter key: null (segment-only hash must
  // not clobber an in-memory filter).
  assert.equal(requestsFilterFromQuery({ stream: 'mcp' }), null);
  assert.equal(requestsFilterFromQuery({ stream: 'bogus' }), null);
  // Junk shadow falls back to the tri-state default; truthy errors spellings
  // other than 1/true normalize to false.
  const junk = requestsFilterFromQuery({ session: 's', shadow: 'bogus', errors: 'yes' });
  assert.deepEqual(junk, { stream: '', session: 's', agent: '', model: '', provider: '', errors: false, shadow: '' });
  // No filter keys → null (a bare #requests must not clobber live state);
  // null/undefined params normalize to null.
  assert.equal(requestsFilterFromQuery({}), null);
  assert.equal(requestsFilterFromQuery(null), null);
  assert.equal(requestsFilterFromQuery(hashQueryParams('')), null);
  assert.deepEqual(hashQueryParams(null), {});
});

test('unified request table: one head and row renderer for all three tables', () => {
  const head = requestTableHeadHTML();
  assert.equal((head.match(/<th[ >]/g) || []).length, 8, '8 columns');
  assert.ok(head.includes('Tokens In / Out'), 'token column replaces bytes');
  assert.ok(!/req bytes|resp bytes/.test(head), 'bytes columns are gone');
  // Fixed geometry: the colgroup pins every column's share (8 cols summing
  // to 100%) so virtual scrolling / live re-renders cannot re-derive column
  // widths from the mounted rows (scroll jitter). styles.css pairs it with
  // table-layout: fixed on the three request tables.
  const widths = [...head.matchAll(/<col style="width:(\d+)%"\/>/g)].map((m) => Number(m[1]));
  assert.equal(widths.length, 8, 'colgroup pins all 8 columns');
  assert.equal(widths.reduce((a, b) => a + b, 0), 100, 'column shares sum to 100%');
  // The MCP variant speaks the MCP domain: Server/Tool/Account labels, no
  // token column (8-column geometry) and session cells drill the session.
  const mcpHead = requestTableHeadHTML({ mcp: true });
  assert.ok(mcpHead.includes('<th>Server</th>') && mcpHead.includes('<th>Account</th>'));
  assert.ok(mcpHead.includes('<th>Tool</th>'), 'tool column sits in the mcp head');
  assert.ok(mcpHead.indexOf('<th>Tool</th>') > mcpHead.indexOf('<th>Server</th>'), 'tool column is right of Server');
  assert.ok(!mcpHead.includes('Tokens'), 'mcp head carries no token column');
  assert.equal((mcpHead.match(/<th[ >]/g) || []).length, 8, 'mcp head has 8 columns');
  const mcpWidths = [...mcpHead.matchAll(/<col style="width:(\d+)%"\/>/g)].map((m) => Number(m[1]));
  assert.equal(mcpWidths.length, 8, 'mcp colgroup pins 8 columns');
  assert.equal(mcpWidths.reduce((a, b) => a + b, 0), 100, 'mcp column shares sum to 100%');
  const mcpRow = requestRowHTML({
    requestId: 'm1', ts: 5000, session: 'sess-abcd1234', agent: 'codex',
    model: 'web-search', tool: 'search_videos', provider: 'zhipu#2', status: 200, latencyMs: 30,
    input: 0, output: 0,
  }, { mcp: true, fmtTime: (t) => 'T' + t });
  assert.equal((mcpRow.match(/<td/g) || []).length, 8, 'mcp row has 8 cells');
  assert.ok(mcpRow.includes('session-link'), 'mcp session cell drills the session');
  assert.ok(/cell-model[^>]*title="search_videos"[^>]*>search_videos</.test(mcpRow), 'tool cell renders the tools/call name');
  assert.ok(mcpRow.indexOf('search_videos') > mcpRow.indexOf('web-search'), 'tool cell sits right of the server cell');
  assert.ok(!mcpRow.includes('cache'), 'mcp row carries no token markup');
  // Protocol traffic (initialize/tools/list) and pre-tool records read —;
  // in-flight rows pend until the end event carries the name.
  const noTool = requestRowHTML({ requestId: 'm2', ts: 1, agent: 'codex', model: 'web-search' }, { mcp: true });
  assert.ok(/cell-model[^>]*>—<\/td>/.test(noTool), 'missing tool reads —');
  const inflight = requestRowHTML({ requestId: 'm3', ts: 1, agent: 'codex', model: 'web-search', inFlight: true, guardHits: [] }, { mcp: true });
  assert.ok(/cell-model subdue[^>]*>…<\/td>/.test(inflight), 'in-flight tool pends');
  // A full row: session link, status badge, tokens with cache read.
  const row = requestRowHTML({
    requestId: 'r1', ts: 5000, session: 'sess-abcd1234', agent: 'claude-code',
    model: 'glm-5.3', provider: 'zhipu', status: 200, latencyMs: 15000,
    input: 310, output: 132, cacheRead: 2400, shadow: false,
  }, { rowClass: 'req-row', fmtTime: (t) => 'T' + t });
  assert.ok(row.includes('data-id="r1"'));
  assert.ok(row.includes('session-link'), 'session cell is a link');
  assert.ok(row.includes('badge ok'), 'status renders as a badge');
  assert.ok(row.includes('class="num warn"'), '>10s latency flags warn');
  assert.ok(row.includes('310/132'), 'tokens in / out');
  assert.ok(row.includes('· cache 2.4K'), 'cache read rides the token cell, compacted');
  assert.ok(row.includes('title="in 310 · out 132 · cache 2,400"'), 'exact counts ride the cell title');
  // In-flight live row: dimmed, pending badge, empty tokens, live-key.
  const live = requestRowHTML({
    requestId: 'r2', ts: 1, model: 'm', inFlight: true, guardHits: [],
  }, { rowClass: 'live-row', liveKey: true, fmtTime: (t) => 'T' + t });
  assert.ok(live.includes('data-live-key="r2"'));
  assert.ok(live.includes('subdue') && live.includes('···'), 'in-flight dim + pending badge');
  assert.ok(!/cache/.test(live), 'no token cell while in flight');
  // Shadow badge and modelNote (guard) land in their cells.
  const shadow = requestRowHTML({ requestId: 'r3', ts: 1, shadow: true, status: 200 }, { rowClass: 'req-row' });
  assert.ok(shadow.includes('shadow'), 'shadow badge');
  const guard = requestRowHTML({ requestId: 'r4', ts: 1, status: 200 }, { rowClass: 'live-row', modelNote: ' <span class="badge warn">⚑ guard</span>' });
  assert.ok(/⚑ guard<\/span><\/td>/.test(guard), 'guard note sits in the model cell');
});

test('sessionHealthSummary derives health signals with gap/percentile guards', () => {
  const rows = [
    { ts: 0, latencyMs: 1000, input: 100, output: 10, model: 'm1', attempt: 0 },
    { ts: 5000, latencyMs: 3000, input: 200, output: 30, model: 'm1', cacheRead: 800, attempt: 0 },
    { ts: 30000, latencyMs: 9000, input: 0, output: 60, model: 'm2', attempt: 2 },
    { ts: 9 * 3600 * 1000, latencyMs: 2000, input: 50, output: 20, model: 'm2', shadow: true },
  ];
  const h = sessionHealthSummary(rows);
  assert.equal(h.spanMs, 9 * 3600 * 1000, 'span is first→last');
  assert.equal(h.activeMs, 30000, 'the 9h idle gap drops; sub-2m gaps count (5s + 25s)');
  assert.equal(h.p50Ms, 3000, 'nearest-rank p50 over sorted [1000,2000,3000,9000]');
  assert.equal(h.p95Ms, 9000, 'p95 clamps to the max');
  assert.equal(h.failovers, 1);
  assert.equal(h.cacheHitPct, 70, 'cache read share of read+input (800/1150)');
  assert.equal(h.tokPerSec, Math.round((120 / 15) * 10) / 10, 'output tokens over summed latency');
  assert.deepEqual(h.models.map((m) => m.model + '×' + m.n), ['m1×2', 'm2×2'], 'model distribution sorted by count');
  assert.equal(h.shadow, 1);
  // Degenerate inputs: empty, single row (no interval), zero denominators.
  assert.deepEqual(sessionHealthSummary([]), { spanMs: 0, activeMs: 0, p50Ms: null, p95Ms: null, ttftP50Ms: null, failovers: 0, cacheHitPct: null, tokPerSec: null, models: [], shadow: 0 });
  const one = sessionHealthSummary([{ ts: '2026-09-12T00:00:00Z', latencyMs: 500 }]);
  assert.equal(one.activeMs, 0, 'single request has no interval');
  assert.equal(one.p50Ms, 500);
  const noLat = sessionHealthSummary([{ ts: 0 }, { ts: 1000 }]);
  assert.equal(noLat.p50Ms, null);
  assert.equal(noLat.cacheHitPct, null, 'zero denominator guarded');
  assert.equal(noLat.tokPerSec, null);
});

test('requestRowHTML cache badge shows the hit share and highlights at 80%', () => {
  const hot = requestRowHTML({ requestId: 'a', ts: 1, status: 200, input: 100, output: 5, cacheRead: 900 }, { rowClass: 'req-row' });
  assert.ok(hot.includes('cache 900(90%)'), 'percentage rides the cache note');
  assert.ok(hot.includes('tok-cache hot'), '≥80% highlights');
  const cool = requestRowHTML({ requestId: 'b', ts: 1, status: 200, input: 900, output: 5, cacheRead: 100 }, { rowClass: 'req-row' });
  assert.ok(cool.includes('cache 100(10%)'));
  assert.ok(!cool.includes('tok-cache hot'), 'low share stays muted');
  const none = requestRowHTML({ requestId: 'c', ts: 1, status: 200, input: 10, output: 5 }, { rowClass: 'req-row' });
  assert.ok(!/cache/.test(none), 'no cache read → no note');
  // Two-decimal precision: near-total hits must not read as "100%".
  const near = requestRowHTML({ requestId: 'd', ts: 1, status: 200, input: 4, output: 5, cacheRead: 9996 }, { rowClass: 'req-row' });
  assert.ok(near.includes('cache 10K(99.96%)'), near);
  const full = requestRowHTML({ requestId: 'e', ts: 1, status: 200, input: 0, output: 5, cacheRead: 5000 }, { rowClass: 'req-row' });
  assert.ok(full.includes('cache 5K(100%)'), 'exact 100% stays integer');
});

// The request↔guard correlation: badges on the row, the full trail in the
// detail block, same-outcome multi-rule marks grouped, and unblock trail
// rows excluded from rule-hit counts.
test('guardMarksHTML renders interception, verdict and unblock badges', () => {
  const html = guardMarksHTML([
    { ts: 5, kind: 'secret', names: ['openai_api_key'], action: 'block', source: 'audit' },
    { ts: 4, kind: 'secret', names: ['openai_api_key'], verdict: 'high', reason: 'real key', model: 'glm-5.3', source: 'judge', cached: true },
    { ts: 3, kind: 'unblock', names: ['openai_api_key'], action: 'unblock', detail: 'session re-admitted' },
  ]);
  assert.ok(html.includes('⚑ block openai_api_key'), html);
  assert.ok(html.includes('badge err'), 'a block badge is an error badge');
  // A repeat-interception record carries action=block AND the original
  // verdict — the BLOCK wins; rendering judge·high would hide that this
  // request was rejected.
  const intercepted = guardMarksHTML([
    { ts: 2, kind: 'secret', names: ['openai_api_key'], action: 'block', verdict: 'high', reason: 'live key', source: 'audit' },
  ]);
  assert.ok(intercepted.includes('⚑ block openai_api_key'), intercepted);
  assert.ok(!intercepted.includes('judge·'), intercepted);
  assert.ok(html.includes('judge·high·cached'), html);
  assert.ok(html.includes('unblocked'), html);
  assert.ok(html.includes('badge ok'), 'the unblock badge uses the ok class');
  // The verdict reason rides the title (scrubbed text, escaped).
  assert.ok(html.includes('title="real key"'), html);
  // Several rules under ONE outcome collapse into a single counted badge —
  // a multi-rule request reads as one event, not a badge flood.
  const multi = guardMarksHTML([
    { ts: 3, kind: 'secret', names: ['kube'], verdict: 'error', reason: 'timeout', model: 'glm-5.3' },
    { ts: 2, kind: 'secret', names: ['docker'], verdict: 'error', reason: 'timeout', model: 'glm-5.3' },
    { ts: 1, kind: 'secret', names: ['jwt'], verdict: 'low', reason: 'fixture' },
  ]);
  assert.ok(multi.includes('judge·error ×2'), multi);
  assert.ok(multi.includes('judge·low'), multi);
  assert.ok(!multi.includes('+'), 'no overflow chip — grouping kept it to two badges');
  // Different verdicts never merge even on the same rule.
  const split = guardMarksHTML([
    { ts: 2, kind: 'secret', names: ['jwt'], verdict: 'high', reason: 'a' },
    { ts: 1, kind: 'secret', names: ['jwt'], verdict: 'low', reason: 'b' },
  ]);
  assert.ok(split.includes('judge·high') && split.includes('judge·low'), split);
  // Distinct reasons under ONE verdict still merge (kind+verdict grouping):
  // a single counted badge whose tooltip carries every rule's reason.
  const flood = guardMarksHTML([1, 2, 3, 4, 5].map((n) => ({ ts: n, kind: 'secret', names: ['r' + n], verdict: 'low', reason: 'x' + n })));
  assert.ok(flood.includes('judge·low ×5'), flood);
  assert.ok(!flood.includes('+'), flood);
  for (let i = 1; i <= 5; i++) assert.ok(flood.includes('x' + i), 'tooltip joins rule reasons');
  // Genuinely different verdicts still cap with an overflow count.
  const caps = guardMarksHTML(['high', 'medium', 'low', 'error', 'skipped'].map((v, i) => ({ ts: i, kind: 'secret', names: ['r' + i], verdict: v, reason: 'x' + i })));
  assert.ok(caps.includes('+2'), caps);
  assert.equal(guardMarksHTML([]), '');
  assert.equal(guardMarksHTML(null), '');
});

test('requestRowHTML renders joined guard marks in the model cell', () => {
  const row = requestRowHTML(
    { requestId: 'r1', ts: 1, status: 400, model: 'm', guardMarks: [{ ts: 2, kind: 'secret', names: ['jwt'], action: 'block' }] },
    { rowClass: 'req-row' },
  );
  assert.ok(row.includes('⚑ block jwt'), row);
  const plain = requestRowHTML({ requestId: 'r2', ts: 1, status: 200, model: 'm' }, { rowClass: 'req-row' });
  assert.ok(!plain.includes('guard'), 'rows without marks stay clean');
});

test('guardMarksDetailHTML renders one row per channel with per-rule reasons', () => {
  const html = guardMarksDetailHTML([
    { ts: 5, kind: 'secret', names: ['openai_api_key'], action: 'log', verdict: 'high', reason: 'real key', evidence: 'sk-proj prefix', model: 'glm-5.3', source: 'judge' },
    { ts: 6, kind: 'unblock', action: 'unblock', detail: 'session re-admitted; block was kind=secret' },
  ]);
  // One labeled group in the shared meta-strip idiom — one row per channel.
  assert.ok(html.includes('req-meta-guard'), html);
  assert.ok(html.includes('<div class="req-meta-k">⚑ guard</div>'), html);
  assert.equal(html.match(/req-guard-row/g).length, 2, html);
  assert.ok(html.includes('<span class="badge err">high</span>'), html);
  assert.ok(html.includes('<code>openai_api_key</code>'), html);
  assert.ok(html.includes('judge glm-5.3'), html);
  assert.ok(html.includes('req-guard-time'), html);
  assert.ok(html.includes('real key'), html);
  assert.ok(html.includes('<span class="badge ok">unblocked</span>'), html);
  assert.ok(html.includes('session re-admitted'), html);
  assert.equal(guardMarksDetailHTML([]), '');
  assert.equal(guardMarksDetailHTML(null), '');

  // Two rules of ONE channel with different judgment reasons: still ONE row
  // (the channel row), one reason line per rule — each with its own verdict
  // badge and rule name.
  const multi = guardMarksDetailHTML([
    { ts: 3, kind: 'path', names: ['kube'], verdict: 'medium', reason: 'cluster credentials', model: 'glm-5.3-flash' },
    { ts: 2, kind: 'path', names: ['docker'], verdict: 'medium', reason: 'registry auth', model: 'glm-5.3-flash' },
  ]);
  assert.equal(multi.match(/req-guard-row/g).length, 1, multi);
  assert.equal(multi.match(/req-guard-line/g).length, 2, multi);
  assert.ok(multi.includes('<code>kube, docker</code>'), multi);
  assert.ok(multi.includes('cluster credentials') && multi.includes('registry auth'), multi);

  // Same verdict AND same reason across rules → a single shared reason line.
  const shared = guardMarksDetailHTML([
    { ts: 3, kind: 'secret', names: ['a'], verdict: 'error', reason: 'judge timeout', model: 'm' },
    { ts: 2, kind: 'secret', names: ['b'], verdict: 'error', reason: 'judge timeout', model: 'm' },
  ]);
  assert.equal(shared.match(/req-guard-row/g).length, 1, shared);
  assert.equal(shared.match(/req-guard-line/g).length, 1, shared);
  assert.ok(shared.includes('<code>a, b</code>'), shared);
});

test('requestMetaHTML groups the record facts under labeled keys', () => {
  const html = requestMetaHTML(
    { ts: '2026-09-13T18:12:58Z', method: 'POST', path: '/v1/messages', attempt: 0, status: 200, latency_ms: 1165, ttft_ms: 1200, provider: 'zhipu', upstream_model: 'glm-5.3-flash', request_size: 177, response_size: 451 },
    ['T+4m', 'Δ48.0s after prev'],
  );
  assert.equal(html.match(/req-meta-g/g).length, 5, html);
  assert.ok(html.includes('<div class="req-meta-k">when</div>'), html);
  assert.ok(html.includes('2026-09-13T18:12:58Z · T+4m · Δ48.0s after prev'), html);
  assert.ok(html.includes('<div class="req-meta-k">call</div>'), html);
  assert.ok(html.includes('POST <span class="req-meta-dim">/v1/messages</span>'), html);
  assert.ok(html.includes('<div class="req-meta-k">result</div>'), html);
  assert.ok(html.includes('<span class="badge ok">200</span>'), 'status rides as a colored badge');
  assert.ok(html.includes('1,165ms'), html);
  assert.ok(html.includes('<div class="req-meta-k">route</div>'), html);
  assert.ok(html.includes('zhipu / glm-5.3-flash'), html);
  assert.ok(html.includes('<div class="req-meta-k">size</div>'), html);
  assert.ok(html.includes('req 177'), html);
  // Sparse records drop their empty groups instead of rendering blanks.
  const sparse = requestMetaHTML({ ts: 't', method: 'GET', status: 400 }, []);
  assert.ok(sparse.includes('when') && sparse.includes('call') && sparse.includes('result'));
  assert.ok(!sparse.includes('route'), sparse);
  assert.ok(!sparse.includes('size'), sparse);
});

test('requestMetaHTML rides the MCP tool beside the JSON-RPC method', () => {
  const html = requestMetaHTML(
    { ts: 't', method: 'tools/call', path: '/mcp/web-search', tool: 'search_videos', attempt: 0, status: 200 }, []);
  assert.ok(html.includes('tools/call · search_videos <span class="req-meta-dim">/mcp/web-search</span>'), html);
  // No tool (protocol traffic / pre-tool records): the method stands alone.
  const bare = requestMetaHTML({ ts: 't', method: 'tools/list', path: '/mcp/web-search', attempt: 0, status: 200 }, []);
  assert.ok(bare.includes('tools/list <span class="req-meta-dim">/mcp/web-search</span>'), bare);
});

test('ruleHitsLeaderboard ignores the unblock trail', () => {
  const rows = ruleHitsLeaderboard([
    { ts: 100, kind: 'secret', names: ['jwt'] },
    { ts: 200, kind: 'unblock', names: ['jwt'], action: 'unblock' },
  ], []);
  assert.equal(rows.length, 1, 'the unblock record must not create or inflate a rule row');
  assert.equal(rows[0].hits, 1);
});

test('mergeSecurityFeed passes unblock trail rows through as audit rows', () => {
  const rows = mergeSecurityFeed([
    { ts: 900, kind: 'unblock', session_id: 's', names: ['jwt'], action: 'unblock', detail: 'session re-admitted', request_id: 'r1' },
  ], []);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].kind, 'unblock');
  assert.equal(rows[0].action, 'unblock');
  assert.equal(rows[0].src, 'audit');
});

// One request hitting several rules writes one audit record PER rule (the
// adjudication sink is rule-granular); the feed display merges those rows
// into ONE row per request per channel — worst verdict headlines, per-rule
// verdicts/reasons ride as segments (securitySegmentsHTML).
test('mergeSecurityFeed merges same-request rows per channel with segments', () => {
  const rows = mergeSecurityFeed([
    { ts: 100, kind: 'path', request_id: 'r14', names: ['kube'], action: 'log', verdict: 'medium', reason: 'cluster credentials' },
    { ts: 101, kind: 'path', request_id: 'r14', names: ['docker'], action: 'log', verdict: 'medium', reason: 'registry auth' },
    { ts: 102, kind: 'secret', request_id: 'r14', names: ['openai_api_key'], action: 'log', verdict: 'high', reason: 'real key' },
    { ts: 103, kind: 'secret', request_id: 'r15', names: ['jwt'], action: 'log', verdict: 'error', reason: 'judge timeout' },
    { ts: 104, kind: 'drift', names: ['config'], action: 'log' },
  ]);
  // kube+docker: ONE row (names joined, newest ts) with per-rule segments.
  const pathRow = rows.find((r) => r.kind === 'path');
  assert.ok(pathRow, 'path row exists');
  assert.deepEqual(pathRow.names.sort(), ['docker', 'kube']);
  assert.equal(pathRow.ts, 101);
  assert.equal(pathRow.segments.length, 2);
  assert.ok(pathRow.segments.some((sg) => sg.names[0] === 'kube' && sg.reason === 'cluster credentials'), JSON.stringify(pathRow.segments));
  assert.equal(rows.filter((r) => r.kind === 'path').length, 1, 'one row per channel');
  // A different channel (secret) of the same request stays its own row; the
  // same channel of a DIFFERENT request stays separate; no-request-id rows
  // (drift) never merge.
  assert.equal(rows.length, 4, `rows = ${rows.map((r) => r.kind + ':' + r.names.join(','))}`);
  assert.ok(rows.some((r) => r.kind === 'secret' && r.names.includes('openai_api_key') && r.verdict === 'high'));
  assert.ok(rows.some((r) => r.kind === 'secret' && r.names.includes('jwt') && r.verdict === 'error'));
  assert.ok(rows.some((r) => r.kind === 'drift'));
  // Heterogeneous verdicts within one channel still merge: the worst
  // verdict headlines the row.
  const mixed = mergeSecurityFeed([
    { ts: 1, kind: 'secret', request_id: 'r1', names: ['a'], verdict: 'medium', reason: 'shape only' },
    { ts: 2, kind: 'secret', request_id: 'r1', names: ['b'], verdict: 'high', reason: 'live key' },
  ]);
  assert.equal(mixed.length, 1, 'one merged row');
  assert.equal(mixed[0].verdict, 'high', 'worst verdict headlines');
  assert.equal(mixed[0].reason, 'live key');
  assert.equal(mixed[0].segments.length, 2);
});

test('securitySegmentsHTML renders per-rule lines only for merged rows', () => {
  assert.equal(securitySegmentsHTML(null), '');
  assert.equal(securitySegmentsHTML({ segments: [{ names: ['a'], verdict: 'low' }] }), '');
  const html = securitySegmentsHTML({ segments: [
    { names: ['kube'], verdict: 'medium', reason: 'cluster credentials' },
    { names: ['docker'], verdict: 'medium', reason: 'registry auth', cached: true },
  ] });
  assert.equal(html.match(/sec-seg/g).length, 2, html);
  assert.ok(html.includes('<code>kube</code>') && html.includes('cluster credentials'), html);
  assert.ok(html.includes('<code>docker</code>') && html.includes('registry auth'), html);
  assert.ok(html.includes('badge warn'), 'medium renders warn');
  assert.ok(html.includes('badge muted">cached'), 'cached badge rides the segment');
});

test('virtual table windowing: cumulative offsets and the visible range', () => {
  const offsets = cumulativeOffsets([10, 10, 10, 10, 10]);
  assert.deepEqual(offsets, [0, 10, 20, 30, 40, 50]);
  // Variable heights fold in order; non-positive entries contribute nothing.
  assert.deepEqual(cumulativeOffsets([20, 0, 30]), [0, 20, 20, 50]);
  assert.deepEqual(cumulativeOffsets([]), [0]);
  // Viewport covering rows 1-2 plus 10px overscan pulls 0..3.
  assert.deepEqual(virtualWindow(offsets, 10, 30, 10), { first: 0, last: 3 });
  // Strictly inside row 2 with no overscan: exactly row 2.
  assert.deepEqual(virtualWindow(offsets, 21, 29, 0), { first: 2, last: 2 });
  // Viewport above/below the table, degenerate viewport, empty table.
  assert.equal(virtualWindow(offsets, -50, -10, 0), null);
  assert.equal(virtualWindow(offsets, 60, 90, 0), null);
  assert.equal(virtualWindow(offsets, 40, 40, 0), null);
  assert.equal(virtualWindow([0], 0, 100, 0), null);
});

test('mergeRecordsPages dedupes the boundary second and keeps newest-first', () => {
  const loaded = [
    { request_id: 'a', ts: '2026-08-20T12:00:03Z' },
    { request_id: 'b', ts: '2026-08-20T12:00:02Z' },
  ];
  // Keyset pagination refetches the whole boundary second: b comes back.
  const page = [
    { request_id: 'b', ts: '2026-08-20T12:00:02Z' },
    { request_id: 'c', ts: '2026-08-20T12:00:01Z' },
  ];
  const out = mergeRecordsPages(loaded, page);
  assert.equal(out.added, 1);
  assert.deepEqual(out.records.map((r) => r.request_id), ['a', 'b', 'c']);
  // Re-merging the same page adds nothing and does not mutate order.
  const again = mergeRecordsPages(out.records, page);
  assert.equal(again.added, 0);
  assert.deepEqual(again.records.map((r) => r.request_id), ['a', 'b', 'c']);
  // An out-of-order page still lands newest-first.
  const fresh = mergeRecordsPages([], [
    { request_id: 'x', ts: '2026-08-20T11:00:00Z' },
    { request_id: 'y', ts: '2026-08-20T12:59:00Z' },
  ]);
  assert.deepEqual(fresh.records.map((r) => r.request_id), ['y', 'x']);
  assert.equal(fresh.added, 2);
});

test('mergeRecordsPages keeps same-second ties in page order, loaded first', () => {
  // The sort key is only second-granular ts; the backend breaks same-second
  // ties by rowid DESC. The tie contract here is Array#sort stability over
  // base.concat(added): records already loaded (the newer page) keep their
  // slots ahead of the newly merged older page within one second.
  const ts = '2026-08-20T12:00:02Z';
  const loaded = [
    { request_id: 'a', ts },
    { request_id: 'b', ts },
  ];
  const page = [
    { request_id: 'c', ts },
    { request_id: 'b', ts }, // boundary-second refetch: deduped
    { request_id: 'd', ts: '2026-08-20T12:00:01Z' },
  ];
  const out = mergeRecordsPages(loaded, page);
  assert.equal(out.added, 2);
  assert.deepEqual(out.records.map((r) => r.request_id), ['a', 'b', 'c', 'd']);
});

test('oldestTsSec floors the oldest loaded record to whole seconds', () => {
  const sec = Math.floor(Date.parse('2026-08-20T11:59:58Z') / 1000);
  assert.equal(oldestTsSec([{ ts: '2026-08-20T12:00:03Z' }, { ts: '2026-08-20T11:59:58Z' }]), sec);
  assert.equal(oldestTsSec([{ ts: '2026-08-20T12:00:03Z' }]), Math.floor(Date.parse('2026-08-20T12:00:03Z') / 1000));
  assert.equal(oldestTsSec([]), null);
  assert.equal(oldestTsSec([{ ts: 'not-a-time' }]), null);
  assert.equal(oldestTsSec(null), null);
});

test('ttft rides the bar summary and session health p50', () => {
  const lines = sessionBarSummary({ ts: 0, status: 200, latencyMs: 4200, ttftMs: 320, input: 10, output: 5 }, (t) => 'T' + t);
  assert.ok(lines.some((l) => l.includes('200 · 4.2s · ttft 320ms')), `line: ${JSON.stringify(lines)}`);
  // absent ttft stays silent
  const noT = sessionBarSummary({ ts: 0, status: 200, latencyMs: 100 }, () => 'T');
  assert.ok(noT.every((l) => !/ttft/.test(l)));
  // health p50 over ttfts
  const h = sessionHealthSummary([
    { ts: 0, latencyMs: 1000, ttftMs: 100 },
    { ts: 1000, latencyMs: 1000, ttftMs: 300 },
    { ts: 2000, latencyMs: 1000, ttftMs: 900 },
  ]);
  assert.equal(h.ttftP50Ms, 300, 'nearest-rank p50 over [100,300,900]');
  assert.equal(sessionHealthSummary([{ ts: 0 }]).ttftP50Ms, null);
});


test('analyticsYearGrid lays days out in Monday-first week columns', () => {
  // Window 2026-01-05 (Mon) .. 2026-01-11 (Sun): exactly one full week; a
  // window starting mid-week gets a partial leading column with false
  // outside-window slots.
  const mk = (ymdKey, tokens) => {
    const [y, m, d] = ymdKey.split('-').map(Number);
    return { day: new Date(y, m - 1, d, 12).getTime() / 1000, tokens };
  };
  const cells = [mk('2026-01-05', 10), mk('2026-01-07', 30), mk('2026-01-11', 20)];
  const from = new Date(2026, 0, 5).getTime() / 1000;
  const to = new Date(2026, 0, 11, 23).getTime() / 1000;
  const { weeks, max } = analyticsYearGrid(cells, from, to);
  assert.equal(weeks.length, 1);
  assert.equal(weeks[0].lead, '2026-01-05');
  assert.equal(weeks[0].days[0].tokens, 10);       // Mon
  assert.equal(weeks[0].days[2].tokens, 30);       // Wed
  assert.equal(weeks[0].days[6].tokens, 20);       // Sun
  assert.equal(weeks[0].days[1], null);            // Tue in range, no traffic
  assert.equal(max, 30);

  // Mid-week window start: leading column holds false before the first day.
  const from2 = new Date(2026, 0, 7).getTime() / 1000; // Wed
  const to2 = new Date(2026, 0, 11, 23).getTime() / 1000; // Sun
  const g2 = analyticsYearGrid(cells, from2, to2);
  assert.equal(g2.weeks.length, 1);
  assert.equal(g2.weeks[0].lead, '2026-01-05'); // column anchored at Monday
  assert.deepEqual(g2.weeks[0].days.slice(0, 2), [false, false]); // Mon/Tue out
  assert.equal(g2.weeks[0].days[2].tokens, 30);
  assert.equal(g2.max, 30);

  // Multi-week span and max over the FIXED token metric (requests ignored).
  const from3 = new Date(2026, 0, 5).getTime() / 1000;
  const to3 = new Date(2026, 0, 18, 23).getTime() / 1000; // spans 2 weeks
  const g3 = analyticsYearGrid([...cells, { ...mk('2026-01-12', 5), requests: 999 }], from3, to3);
  assert.equal(g3.weeks.length, 2);
  assert.equal(g3.max, 30);

  // Defensive: a null/invalid cell list still builds the window scaffold
  // (all-null days, max 0); a cell without a day field is dropped.
  const gEmpty = analyticsYearGrid(null, from, to);
  assert.equal(gEmpty.weeks.length, 1);
  assert.ok(gEmpty.weeks[0].days.every((d) => d === null));
  assert.equal(gEmpty.max, 0);
  const g4 = analyticsYearGrid([{ tokens: 1 }], from, to); // no day field
  assert.equal(g4.weeks[0].days[0], null);
});

test('analyticsYearMonthSpans partitions columns into centered month runs', () => {
  const w = (ymdKey) => ({ lead: ymdKey, days: [null, null, null, null, null, null, null] });
  const weeks = [
    w('2025-12-29'), // Dec run: 1 column → unlabeled (edge run)
    w('2026-01-05'), w('2026-01-12'), w('2026-01-19'), w('2026-01-26'), // Jan ×4
    w('2026-02-02'), w('2026-02-09'), // Feb run: 2 columns → labeled
  ];
  const spans = analyticsYearMonthSpans(weeks);
  // Runs partition every column: 1 + 4 + 2 = 7.
  assert.deepEqual(spans.map((s) => [s.col, s.span]), [[0, 1], [1, 4], [5, 2]]);
  assert.deepEqual(spans.map((s) => s.label), ['', 'Jan', 'Feb']);
  // Malformed input stays silent rather than throwing.
  assert.deepEqual(analyticsYearMonthSpans([]), []);
  assert.deepEqual(analyticsYearMonthSpans(null), []);
});

test('analyticsHeatLevel buckets into five ordinal levels', () => {
  assert.equal(analyticsHeatLevel(null, 100), 0);
  assert.equal(analyticsHeatLevel(0, 100), 0);
  assert.equal(analyticsHeatLevel(5, 0), 0);
  assert.equal(analyticsHeatLevel(10, 100), 1);
  assert.equal(analyticsHeatLevel(25, 100), 2);
  assert.equal(analyticsHeatLevel(50, 100), 3);
  assert.equal(analyticsHeatLevel(75, 100), 4);
  assert.equal(analyticsHeatLevel(100, 100), 4);
});

test('analyticsHeatCellSize fills the measured width within the 8..18px band', () => {
  // 1102px available, 54 columns: (1102 - 42 - 3*54) / 54 = 16.6 → 16px squares.
  assert.equal(analyticsHeatCellSize(1102, 54), 16);
  // Ultra-wide stays capped; tiny widths floor at 8px (scroll takes over).
  assert.equal(analyticsHeatCellSize(3000, 54), 18);
  assert.equal(analyticsHeatCellSize(300, 54), 8);
  assert.equal(analyticsHeatCellSize(0, 54), 8);
  assert.equal(analyticsHeatCellSize(1102, 0), 18); // degenerate column count
});

test('analyticsHeatCellSize budget covers the gutter gap — grid never overflows the measured width', () => {
  // Regression: the budget must reserve gaps for ALL n+1 tracks (one gap
  // follows the weekday gutter too) and the 41px gutter track must fit the
  // 42px budget. The grid's real width is 41 + 3*n + n*cell; before the fix
  // the formula subtracted only n-1 gaps, so at widths where the floor
  // wasted <2px (1103/1104, 1159/1160 for 56 weeks…) the grid overflowed
  // by ~2px and re-showed the Token Activity horizontal scrollbar.
  const GUTTER = 41; // app.js grid-template-columns first track (px)
  const GAP = 3;     // .an-heat-mon/.an-heat-days gap
  for (const n of [52, 53, 54, 56, 57]) {
    for (let w = 500; w <= 1300; w++) {
      const cell = analyticsHeatCellSize(w, n);
      if (cell <= 8 || cell >= 18) continue; // floor band scrolls by design; cap band can't overflow
      const gridWidth = GUTTER + GAP * n + n * cell;
      assert.ok(gridWidth <= w,
        `${n} cols @${w}px host: grid ${gridWidth}px overflows (cell ${cell}px)`);
    }
  }
  // The exact widths that used to trigger the ~2px overflow (56 weeks).
  for (const w of [1103, 1104, 1159, 1160]) {
    const cell = analyticsHeatCellSize(w, 56);
    assert.ok(GUTTER + GAP * 56 + 56 * cell <= w, `${w}px host must fit`);
  }
});

test('analyticsHeatTip structures the day summary as title + label/value rows', () => {
  const day = new Date(2026, 0, 5, 12).getTime() / 1000; // a Monday
  const tip = analyticsHeatTip(day, { requests: 1234, tokens: 45333734, cost: 1.5, err_pct: 0.5, avg_latency_ms: 812.4 });
  assert.equal(tip.title, 'Mon, Jan 5, 2026');
  assert.deepEqual(tip.rows, [
    { label: 'Requests', value: '1,234' },
    { label: 'Tokens', value: '45.3M' },
    { label: 'Cost', value: '$1.5000' },
    { label: 'Errors', value: '0.5%' },
    { label: 'Avg Latency', value: '812ms' },
  ]);
  // Long latencies humanize to seconds.
  const slow = analyticsHeatTip(day, { requests: 2, tokens: 3, avg_latency_ms: 9335 });
  assert.deepEqual(slow.rows[2], { label: 'Avg Latency', value: '9.3s' });
  // Empty in-window day: still names the date, then says why it is bare.
  assert.deepEqual(analyticsHeatTip(new Date(2026, 0, 31, 12).getTime() / 1000, null),
    { title: 'Sat, Jan 31, 2026', rows: [], note: 'No usage' });
  // Null derived fields are omitted; weekday rolls with the date.
  const bare = analyticsHeatTip(day, { requests: 1, tokens: 2 });
  assert.deepEqual(bare, {
    title: 'Mon, Jan 5, 2026',
    rows: [{ label: 'Requests', value: '1' }, { label: 'Tokens', value: '2' }],
  });
});

test('analyticsSortRows follows the active metric until a column is pinned', () => {
  const rows = [
    { label: 'a/glm', tokens: 100, requests: 1, errPct: 5 },
    { label: 'b/glm', tokens: 300, requests: 9, errPct: 1 },
    { label: 'c/glm', tokens: 200, requests: 5, errPct: 3 },
  ];
  // Unpinned (and unknown columns) = the historical behavior: metric key desc.
  const byTokens = analyticsSortRows(rows, 'tokens', { col: null, dir: 'desc' });
  assert.deepEqual(byTokens.map((r) => r.label), ['b/glm', 'c/glm', 'a/glm']);
  const bogus = analyticsSortRows(rows, 'tokens', { col: 'nope', dir: 'asc' });
  assert.deepEqual(bogus.map((r) => r.label), byTokens.map((r) => r.label));
  // Pinned requests desc.
  const byReqs = analyticsSortRows(rows, 'tokens', { col: 'requests', dir: 'desc' });
  assert.deepEqual(byReqs.map((r) => r.label), ['b/glm', 'c/glm', 'a/glm']);
  // Asc flips.
  const byReqsAsc = analyticsSortRows(rows, 'tokens', { col: 'requests', dir: 'asc' });
  assert.deepEqual(byReqsAsc.map((r) => r.label), ['a/glm', 'c/glm', 'b/glm']);
  // Input order untouched (fresh list).
  assert.deepEqual(rows.map((r) => r.label), ['a/glm', 'b/glm', 'c/glm']);
});

test('analyticsSortRows keeps no-data rows last in both directions', () => {
  const rows = [
    { label: 'a', cost: 0.5 },
    { label: 'b', cost: null },
    { label: 'c', cost: 2 },
    { label: 'd', cost: 1 },
  ];
  const desc = analyticsSortRows(rows, 'tokens', { col: 'cost', dir: 'desc' });
  assert.deepEqual(desc.map((r) => r.label), ['c', 'd', 'a', 'b']);
  const asc = analyticsSortRows(rows, 'tokens', { col: 'cost', dir: 'asc' });
  assert.deepEqual(asc.map((r) => r.label), ['a', 'd', 'c', 'b']);
});

test('analyticsSortRows sorts the series label as a string and status worst-first', () => {
  const rows = [
    { label: 'zeta/glm', healthScore: 0.9 },
    { label: 'alpha/glm', healthScore: 0.2 },
    { label: 'mid/glm', healthScore: null },
  ];
  const byLabel = analyticsSortRows(rows, 'tokens', { col: 'series', dir: 'asc' });
  assert.deepEqual(byLabel.map((r) => r.label), ['alpha/glm', 'mid/glm', 'zeta/glm']);
  // status default dir is asc (worst model first, nulls last).
  assert.equal(ANALYTICS_TABLE_SORT.status, 'asc');
  const byHealth = analyticsSortRows(rows, 'tokens', { col: 'status', dir: ANALYTICS_TABLE_SORT.status });
  assert.deepEqual(byHealth.map((r) => r.label), ['alpha/glm', 'zeta/glm', 'mid/glm']);
});

test('analyticsTableSortValue reads columns and null-safes', () => {
  const r = { label: 'p/m', healthScore: 0.5, requests: 3, tokens: 9, errPct: 0, latencyMs: 100, ttftMs: 10, tokSec: 20, cost: 1, costShare: 0.25 };
  assert.equal(analyticsTableSortValue(r, 'series'), 'p/m');
  assert.equal(analyticsTableSortValue(r, 'status'), 0.5);
  assert.equal(analyticsTableSortValue(r, 'share'), 0.25);
  assert.equal(analyticsTableSortValue(r, 'unknown'), null);
  assert.equal(analyticsTableSortValue(null, 'cost'), null);
  const bare = { label: 'q/m' };
  assert.equal(analyticsTableSortValue(bare, 'cost'), null);
  assert.equal(analyticsTableSortValue(bare, 'requests'), 0); // count metrics zero, not null
});

test('analyticsRowSortKey survives the move to pure.js unchanged', () => {
  // Shared with the Status→Dashboard leaderboard: metric-key mapping with
  // null sentinels.
  assert.equal(analyticsRowSortKey({ requests: 2 }, 'requests'), 2);
  assert.equal(analyticsRowSortKey({ tokens: 7 }, 'tokens'), 7);
  assert.equal(analyticsRowSortKey({ errPct: null }, 'errors'), -1);
  assert.equal(analyticsRowSortKey({ cost: null }, 'cost'), -1);
});

// ---------- Takeover tab ----------



test('takeoverRunSummary / takeoverRestoreSummary flatten mutation results', () => {
  assert.equal(
    takeoverRunSummary({ applied: [{ name: 'pi', note: 'best coverage' }, { name: 'claude' }], skipped: ['gemini-cli'] }),
    'taken over: pi <span class="hint">(best coverage)</span>, claude · skipped (config not present): gemini-cli',
  );
  assert.equal(takeoverRunSummary({ applied: [], skipped: [] }), 'nothing to do');
  assert.equal(takeoverRunSummary(null), 'nothing to do');
  assert.equal(
    takeoverRestoreSummary({ restored: ['pi'], skipped: ['codex'] }),
    'restored: pi · skipped (no backup): codex',
  );
  assert.equal(takeoverRestoreSummary({ restored: [], skipped: [] }), 'nothing to restore');
  // Names are escaped.
  assert.ok(!takeoverRunSummary({ applied: [{ name: '<b>x' }], skipped: [] }).includes('<b>x'));
  // With the surface given, non-default variant ids render as family + protocol label.
  const surface = [
    { name: 'opencode', family: 'opencode', protocol: 'anthropic' },
    { name: 'opencode-openai', family: 'opencode', protocol: 'openai' },
  ];
  assert.equal(
    takeoverRunSummary({ applied: [{ name: 'opencode-openai' }, { name: 'opencode' }], skipped: ['opencode-openai'] }, surface),
    'taken over: opencode (Chat Completion), opencode · skipped (config not present): opencode (Chat Completion)',
  );
});

test('takeoverProtocolLabel / takeoverWriteVariantsLabel hide raw template ids', () => {
  assert.equal(takeoverProtocolLabel('anthropic'), 'Anthropic');
  assert.equal(takeoverProtocolLabel('openai'), 'Chat Completion');
  assert.equal(takeoverProtocolLabel('responses'), 'Responses');
  assert.equal(takeoverProtocolLabel(''), '');
  const surface = [
    { name: 'opencode', protocol: 'anthropic' },
    { name: 'opencode-responses', protocol: 'responses' },
  ];
  assert.equal(takeoverWriteVariantsLabel(['opencode'], surface), 'Anthropic');
  assert.equal(
    takeoverWriteVariantsLabel(['opencode', 'opencode-responses'], surface),
    'Anthropic + Responses',
  );
  assert.equal(takeoverWriteVariantsLabel(['opencode (mcp)'], surface), 'Anthropic (mcp)');
  // Unknown names (custom user templates) pass through; empty input is empty.
  assert.equal(takeoverWriteVariantsLabel(['my-agent'], surface), 'my-agent');
  assert.equal(takeoverWriteVariantsLabel([], surface), '');
  assert.equal(takeoverWriteVariantsLabel(null, null), '');
});

test('shadowMatchBadge classifies primary/shadow match rates', () => {
  assert.equal(shadowMatchBadge(1), '<span class="badge ok">100.0%</span>');
  assert.equal(shadowMatchBadge(0.995), '<span class="badge ok">99.5%</span>');
  assert.equal(shadowMatchBadge(0.97), '<span class="badge warn">97.0%</span>');
  assert.equal(shadowMatchBadge(0.8), '<span class="badge err">80.0%</span>');
  assert.equal(shadowMatchBadge(null), '<span class="badge muted">—</span>');
  assert.equal(shadowMatchBadge(NaN), '<span class="badge muted">—</span>');
});

// ---- takeover config highlighting ----

test('highlightYAML marks keys, strings, comments and escapes safely', () => {
  const html = highlightYAML('file: ~/.agent/config.json   # path comment\nformat: json\napi_key: "{{token}}"');
  assert.ok(html.includes('<span class="j-key">file</span>'), `key span missing: ${html}`);
  assert.ok(html.includes('<span class="j-com"># path comment</span>'), `comment span missing: ${html}`);
  assert.ok(html.includes('<span class="j-str">&quot;{{token}}&quot;</span>'), `string span missing: ${html}`);
  // HTML in input must stay escaped — the result goes into innerHTML.
  const evil = highlightYAML('x: "<script>"');
  assert.ok(!evil.includes('<script>'), `unescaped payload: ${evil}`);
  // A quoted # inside a value is not a comment.
  const noComment = highlightYAML('url: "http://x/#frag"  # real comment');
  assert.equal((noComment.match(/j-com/g) || []).length, 1, `in-string # treated as comment: ${noComment}`);
  assert.ok(noComment.includes('x/#frag'), `in-string # dropped: ${noComment}`);
});

test('highlightTOML marks sections, keys and env marks KEY= lines', () => {
  const toml = highlightTOML('[providers."model-proxy"]\nbase_url = "http://127.0.0.1:15722/v1"\n# comment');
  assert.ok(toml.includes('<span class="j-key">providers.&quot;model-proxy&quot;</span>'), `section span missing: ${toml}`);
  assert.ok(toml.includes('<span class="j-str">&quot;http://127.0.0.1:15722/v1&quot;</span>'), `string span missing: ${toml}`);
  assert.ok(toml.includes('<span class="j-com"># comment</span>'));
  const env = highlightEnv('MYAGENT_BASE_URL=http://x/v1  # endpoint');
  assert.ok(env.includes('<span class="j-key">MYAGENT_BASE_URL</span>'), `env key missing: ${env}`);
  assert.ok(env.includes('j-com'), `env comment missing: ${env}`);
});

test('highlightConfig dispatches by format', () => {
  assert.ok(highlightConfig('{"a": 1}', 'json').includes('j-key'));
  assert.ok(highlightConfig('a: 1', 'yaml').includes('j-key'));
  assert.ok(highlightConfig('[s]\nk = "v"', 'toml').includes('j-key'));
  assert.ok(highlightConfig('K=v', 'env').includes('j-key'));
  assert.equal(highlightConfig('<x>', 'unknown'), '&lt;x&gt;');
});

// ---- takeover family aggregation ----

test('takeoverFamilyGroups buckets by family and aggregates state', () => {
  const clients = [
    { name: 'pi', family: 'pi', installed: true, taken_over: true, drift_ok: true },
    { name: 'pi-openai', family: 'pi', installed: true, taken_over: false, drift_ok: true },
    { name: 'claude', family: 'claude', installed: true, taken_over: false, drift_ok: true },
    { name: 'kimi', family: 'kimi', installed: false, taken_over: false, drift_ok: true },
  ];
  const groups = takeoverFamilyGroups(clients);
  assert.deepEqual(groups.map((g) => g.family), ['claude', 'kimi', 'pi']);
  const pi = groups[2];
  assert.equal(pi.variants.length, 2);
  assert.ok(pi.multi && pi.installed);
  assert.deepEqual(pi.taken.map((c) => c.name), ['pi']);
  assert.equal(takeoverFamilyBadge(pi), '<span class="badge ok" title="pi">taken over</span>');
  assert.equal(takeoverFamilyBadge(groups[0]), '<span class="badge muted">not taken over</span>');
  assert.equal(takeoverFamilyBadge(groups[1]),
    '<span class="badge muted" title="no config file found on disk for this agent">not installed</span>');
  const drifted = takeoverFamilyGroups([
    { name: 'a', family: 'a', installed: true, taken_over: true, drift_ok: false },
  ])[0];
  const driftBadge = takeoverFamilyBadge(drifted);
  assert.ok(driftBadge.includes('badge err') && driftBadge.includes('changed externally'),
    `drift must read as changed externally with an explanation, got: ${driftBadge}`);
  assert.ok(driftBadge.includes('title="'), 'drift badge must carry an explanatory tooltip');
  const allTaken = takeoverFamilyGroups([
    { name: 'a', family: 'a', installed: true, taken_over: true, drift_ok: true },
  ])[0];
  assert.equal(takeoverFamilyBadge(allTaken), '<span class="badge ok" title="a">taken over</span>');
});

test('TAKEOVER_TEMPLATE_EXAMPLES covers every format and documents placeholders', () => {
  assert.deepEqual(Object.keys(TAKEOVER_TEMPLATE_EXAMPLES).sort(), ['env', 'json', 'toml']);
  for (const [fmt, yaml] of Object.entries(TAKEOVER_TEMPLATE_EXAMPLES)) {
    assert.ok(yaml.includes(`format: ${fmt}`), `${fmt} example must declare its format`);
    assert.ok(yaml.includes('file: ~'), `${fmt} example must show the file path`);
    assert.ok(yaml.includes('{{base_url}}'), `${fmt} example must reference placeholders`);
  }
  const documented = TAKEOVER_PLACEHOLDERS.map((p) => p.name);
  for (const ph of ['{{base_url}}', '{{token}}', '{{provider_id}}', '{{mcp.url}}', '{{model.id}}', '{{model.capabilities}}', '{{model.efforts}}']) {
    assert.ok(documented.includes(ph), `placeholder ${ph} missing from the help table`);
  }
});

test('takeoverVariantLabel names variants uniformly across agents', () => {
  assert.equal(takeoverVariantLabel({ name: 'pi', protocol: 'anthropic', auto_selected: true }), 'Auto (Anthropic)');
  assert.equal(takeoverVariantLabel({ name: 'pi', protocol: '', auto_selected: true }), 'Auto');
  assert.equal(takeoverVariantLabel({ name: 'pi', protocol: 'anthropic', auto_selected: false }), 'All Anthropic');
  assert.equal(takeoverVariantLabel({ name: 'pi-openai', protocol: 'openai' }), 'All Chat Completion');
  assert.equal(takeoverVariantLabel({ name: 'pi-responses', protocol: 'responses' }), 'All Responses');
  assert.equal(takeoverVariantLabel({ name: 'custom', protocol: '' }), 'custom');
  assert.equal(takeoverVariantLabel(null), '');
});

test('takeoverClientLabel renders family names, not variant ids', () => {
  assert.equal(takeoverClientLabel({ name: 'claude', family: 'claude', protocol: 'anthropic' }), 'claude');
  assert.equal(takeoverClientLabel({ name: 'opencode-openai', family: 'opencode', protocol: 'openai' }),
    'opencode (Chat Completion)');
  assert.equal(takeoverClientLabel({ name: 'opencode-responses', family: 'opencode', protocol: 'responses' }),
    'opencode (Responses)');
  // No family/protocol signal → the raw name is all we have.
  assert.equal(takeoverClientLabel({ name: 'my-agent' }), 'my-agent');
  assert.equal(takeoverClientLabel(null), '');
});

// ---------- MCP Analytics helpers ----------

test('MCP_ANALYTICS_METRICS lists calls/errors/avg_ms', () => {
  assert.deepEqual(MCP_ANALYTICS_METRICS.map((m) => m.id), ['calls', 'errors', 'avg_ms']);
  assert.ok(MCP_ANALYTICS_METRICS.every((m) => m.label && m.axis));
});

test('mcpAnalyticsMetricOptions exposes the three metrics', () => {
  const opts = mcpAnalyticsMetricOptions();
  assert.deepEqual(opts.map((o) => o.value), ['calls', 'errors', 'avg_ms']);
  assert.ok(opts.every((o) => !o.disabled));
});

test('mcpAnalyticsFilterSeries filters by exact server name only', () => {
  const series = [
    { name: 's1' },
    { name: 's2' },
    { name: 'r1' },
  ];
  assert.equal(mcpAnalyticsFilterSeries(series, '').length, 3);
  assert.equal(mcpAnalyticsFilterSeries(series, 's2').length, 1);
  assert.equal(mcpAnalyticsFilterSeries(series, 'missing').length, 0);
  assert.equal(mcpAnalyticsFilterSeries(null, '').length, 0);
});

test('mcpAnalyticsToolFilter requires the server and matches the tool exactly', () => {
  const toolSeries = [
    { name: 's1', tool: 'search' },
    { name: 's1', tool: 'fetch' },
    { name: 's2', tool: 'search' },
  ];
  assert.equal(mcpAnalyticsToolFilter(toolSeries, 's1', '').length, 2);
  assert.equal(mcpAnalyticsToolFilter(toolSeries, 's1', 'fetch').length, 1);
  assert.equal(mcpAnalyticsToolFilter(toolSeries, 's1', 'missing').length, 0);
  assert.equal(mcpAnalyticsToolFilter(toolSeries, '', 'search').length, 0);
  assert.equal(mcpAnalyticsToolFilter(null, 's1', '').length, 0);
});

test('mcpAnalyticsSummaryGroups groups tools under servers sorted by calls desc', () => {
  const series = [
    { name: 'b', totals: { calls: 5, errors: 1, avg_latency_ms: 100, last_call_at: 200 } },
    { name: 'a', totals: { calls: 12, errors: 3, avg_latency_ms: 50, last_call_at: 100 } },
  ];
  const toolSeries = [
    { name: 'b', tool: 'x', totals: { calls: 4, errors: 1, avg_latency_ms: 80, last_call_at: 150 } },
    { name: 'b', tool: 'y', totals: { calls: 1, errors: 0, avg_latency_ms: 120, last_call_at: 200 } },
    { name: 'a', tool: 'z', totals: { calls: 12, errors: 3, avg_latency_ms: 50, last_call_at: 100 } },
  ];
  const groups = mcpAnalyticsSummaryGroups(series, toolSeries, '');
  assert.deepEqual(groups.map((g) => g.name), ['a', 'b']);
  assert.deepEqual(groups[0], { name: 'a', calls: 12, errors: 3, avgLatencyMs: 50, lastCallAt: 100, tools: [
    { tool: 'z', calls: 12, errors: 3, avgLatencyMs: 50, lastCallAt: 100 },
  ] });
  assert.deepEqual(groups[1].tools.map((t) => t.tool), ['x', 'y']);
});

test('mcpAnalyticsSummaryGroups tool filter drops servers without the tool', () => {
  const series = [
    { name: 'a', totals: { calls: 12 } },
    { name: 'b', totals: { calls: 5 } },
  ];
  const toolSeries = [
    { name: 'a', tool: 'search', totals: { calls: 12 } },
    { name: 'b', tool: 'fetch', totals: { calls: 5 } },
  ];
  const groups = mcpAnalyticsSummaryGroups(series, toolSeries, 'search');
  assert.deepEqual(groups.map((g) => g.name), ['a']);
  assert.equal(groups[0].tools.length, 1);
  // Tool rows that exist for a name missing from `series` are ignored.
  const orphan = mcpAnalyticsSummaryGroups([{ name: 'a', totals: { calls: 1 } }], toolSeries, '');
  assert.equal(orphan.length, 1);
  assert.equal(orphan[0].tools.length, 1);
});

test('mcpAnalyticsSummaryGroups tolerates missing totals', () => {
  const groups = mcpAnalyticsSummaryGroups([{ name: 'x' }], null, '');
  assert.deepEqual(groups[0], { name: 'x', calls: 0, errors: 0, avgLatencyMs: 0, lastCallAt: 0, tools: [] });
});

test('mcpAnalyticsPointValue reads calls/errors/avg_ms', () => {
  assert.equal(mcpAnalyticsPointValue({ ts: 1, calls: 5, errors: 1, avg_latency_ms: 100 }, 'calls'), 5);
  assert.equal(mcpAnalyticsPointValue({ ts: 1, calls: 5, errors: 1, avg_latency_ms: 100 }, 'errors'), 1);
  assert.equal(mcpAnalyticsPointValue({ ts: 1, calls: 5, errors: 1, avg_latency_ms: 100 }, 'avg_ms'), 100);
  assert.equal(mcpAnalyticsPointValue({ ts: 1, calls: 0, errors: 0, avg_latency_ms: 100 }, 'avg_ms'), null);
  assert.equal(mcpAnalyticsPointValue(null, 'calls'), null);
});

test('mcpAnalyticsChartSeries builds x/ys/labels for uPlot', () => {
  const series = [
    { name: 's1', points: [{ ts: 10, calls: 3, errors: 1, avg_latency_ms: 100 }, { ts: 20, calls: 2, errors: 0, avg_latency_ms: 50 }] },
    { name: 's2', points: [{ ts: 10, calls: 1, errors: 0, avg_latency_ms: 80 }] },
  ];
  const data = mcpAnalyticsChartSeries(series, 'calls', [5, 10, 15, 20]);
  assert.deepEqual(data.x, [5, 10, 15, 20]);
  assert.deepEqual(data.labels, ['s1', 's2']);
  assert.deepEqual(data.ys[0], [0, 3, 0, 2]);
  assert.deepEqual(data.ys[1], [0, 1, 0, 0]);
});

test('mcpAnalyticsChartSeries gaps avg_ms for buckets with no calls', () => {
  const series = [
    { name: 's1', points: [{ ts: 10, calls: 3, errors: 1, avg_latency_ms: 100 }, { ts: 20, calls: 0, errors: 0, avg_latency_ms: 50 }] },
  ];
  const data = mcpAnalyticsChartSeries(series, 'avg_ms', [10, 20]);
  assert.deepEqual(data.ys[0], [100, null]);
});

test('mcpAnalyticsChartSeries returns empty for empty input', () => {
  const data = mcpAnalyticsChartSeries([], 'calls');
  assert.deepEqual(data.x, []);
  assert.deepEqual(data.ys, []);
  assert.deepEqual(data.labels, []);
});

test('mcpAnalyticsValueText formats counts and avg_ms', () => {
  assert.equal(mcpAnalyticsValueText('calls', 1234), '1.2K');
  assert.equal(mcpAnalyticsValueText('errors', 7), '7');
  assert.equal(mcpAnalyticsValueText('avg_ms', 123.7), '124ms');
  assert.equal(mcpAnalyticsValueText('calls', null), '—');
});

test('mcpAnalyticsSummaryTableHTML renders grouped parent/child rows', () => {
  const groups = [{
    name: '<srv>', calls: 5, errors: 1, avgLatencyMs: 100, lastCallAt: 200,
    tools: [{ tool: 'a<b', calls: 3, errors: 1, avgLatencyMs: 90, lastCallAt: 100 }],
  }];
  const html = mcpAnalyticsSummaryTableHTML(groups, { formatTime: () => 'never' });
  assert.match(html, /<table class="table">/);
  assert.match(html, /<colgroup>/);
  assert.match(html, /<th>Server \/ Tool<\/th>/);
  assert.match(html, /<tr class="agent-summary"><td class="mcp-clip" title="&lt;srv&gt;">&lt;srv&gt;<\/td>/);
  assert.match(html, /<tr class="agent-model"><td class="mcp-clip" title="a&lt;b">a&lt;b<\/td>/);
  assert.ok(!html.includes('<srv>'));
  assert.ok(!html.includes('a<b>'));
  assert.match(html, /never/);
});

test('mcpAnalyticsSummaryTableHTML returns empty for empty groups', () => {
  assert.equal(mcpAnalyticsSummaryTableHTML([]), '');
  assert.equal(mcpAnalyticsSummaryTableHTML(null), '');
});

test('mcpAnalyticsEmptyHTML and mcpAnalyticsSkeletonHTML are hint placeholders', () => {
  assert.ok(mcpAnalyticsEmptyHTML().includes('No MCP calls'));
  assert.ok(mcpAnalyticsSkeletonHTML().includes('loading'));
});

// ---------- MCP tab hash helpers ----------

test('VALID_MCP_SUB_TABS enumerates the three sub-tabs', () => {
  assert.deepEqual(VALID_MCP_SUB_TABS, ['servers', 'routes', 'analytics']);
});

test('mcpSubTabFromHash returns canonical sub-tabs, empty for unknown/missing', () => {
  assert.equal(mcpSubTabFromHash('servers'), 'servers');
  assert.equal(mcpSubTabFromHash('routes'), 'routes');
  assert.equal(mcpSubTabFromHash('analytics'), 'analytics');
  assert.equal(mcpSubTabFromHash('history'), '');
  assert.equal(mcpSubTabFromHash(''), '');
  assert.equal(mcpSubTabFromHash('bogus'), '');
  assert.equal(mcpSubTabFromHash(null), '');
  assert.equal(mcpSubTabFromHash(undefined), '');
});

test('mcpHash builds #mcp/<sub> for valid sub-tabs and bare #mcp otherwise', () => {
  assert.equal(mcpHash('servers'), '#mcp/servers');
  assert.equal(mcpHash('routes'), '#mcp/routes');
  assert.equal(mcpHash('analytics'), '#mcp/analytics');
  assert.equal(mcpHash('history'), '#mcp');
  assert.equal(mcpHash(''), '#mcp');
  assert.equal(mcpHash('bogus'), '#mcp');
  assert.equal(mcpHash(null), '#mcp');
});
