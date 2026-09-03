// pure.test.mjs — behavioral unit tests for assets/pure.js, run via
// `node --test jstests/` (driven by TestWebAssetsPureJSUnitTests in
// assets_test.go). Pure functions only: no DOM, fixed `now` injections,
// explicit locales — fully deterministic.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  esc, fmtNum, avgLatencyMs, hasReset, fmtDur, untilHuman,
  YAML_EDITOR_MIN_HEIGHT, visibleYamlEditorHeight,
  verdictBadge, modelCapMatrix,
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
