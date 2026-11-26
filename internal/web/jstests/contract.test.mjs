import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import test from 'node:test';
import assert from 'node:assert/strict';

// Contract (internal/web/assets/AGENTS.md: "前端不得依赖未文档化字段"):
// every top-level field the frontend reads from the cached /api/config
// response (configCache.<field>) must be documented in docs/web-api.md's
// `| GET | `/api/config` |` row — docs first, then frontend, in lockstep.
const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');
const webApiDoc = readFileSync(join(here, '..', '..', '..', 'docs', 'web-api.md'), 'utf8');

function documentedConfigKeys() {
  const row = webApiDoc.split('\n').find((l) => l.includes('| GET |') && l.includes('`/api/config`'));
  assert.ok(row, 'docs/web-api.md has no `| GET | `/api/config` |` row');
  const m = row.match(/\{([^{}]*)\}/);
  assert.ok(m, 'api/config row documents no `{...}` key list');
  const keys = new Set(m[1].split(/[, ]+/).filter(Boolean));
  assert.ok(keys.size > 0, 'api/config row documents an empty key list');
  return keys;
}

// documentedTopLevelKeys extracts the top-level keys of the first {...} JSON
// shape in a docs table row via a brace-depth scan — only depth-1 `key:`
// tokens count, so a nested response like
// {providers:{<name>:{fingerprint:…}}} yields exactly {providers}.
function documentedTopLevelKeys(row) {
  const start = row.indexOf('{');
  assert.ok(start >= 0, 'row documents no {...} response shape');
  const keys = new Set();
  let depth = 0;
  let token = '';
  for (const ch of row.slice(start)) {
    if (ch === '{') { depth++; token = ''; continue; }
    if (ch === '}') { depth--; token = ''; if (depth <= 0) break; continue; }
    if (depth !== 1) continue;
    if (ch === ':') {
      // The token since the last boundary may carry the separating comma
      // ("…}, catalog:") — the key is its last word.
      const words = token.trim().split(/[\s,]+/).filter(Boolean);
      const key = words.pop() || '';
      if (key) keys.add(key);
      token = '';
      continue;
    }
    token += ch;
  }
  assert.ok(keys.size > 0, 'row documents no top-level response keys');
  return keys;
}

test('frontend configCache reads only documented /api/config fields', () => {
  const documented = documentedConfigKeys();
  const reads = new Set();
  for (const m of appJs.matchAll(/configCache\.([A-Za-z_]+)/g)) reads.add(m[1]);
  assert.ok(reads.size > 0, 'no configCache.<field> reads found — contract is blind');
  const undocumented = [...reads].filter((k) => !documented.has(k)).sort();
  assert.deepEqual(
    undocumented,
    [],
    `app.js reads undocumented /api/config field(s) ${undocumented.join(', ')} — document them in docs/web-api.md first`,
  );
});

test('frontend modelsCache reads only documented /api/models fields', () => {
  const row = webApiDoc.split('\n').find((l) => l.includes('| GET |') && l.includes('`/api/models`'));
  assert.ok(row, 'docs/web-api.md has no `| GET | `/api/models` |` row — document the endpoint before the frontend uses it');
  const documented = documentedTopLevelKeys(row);
  const reads = new Set();
  for (const m of appJs.matchAll(/modelsCache\.([A-Za-z_]+)/g)) reads.add(m[1]);
  assert.ok(reads.size > 0, 'no modelsCache.<field> reads found — contract is blind');
  const undocumented = [...reads].filter((k) => !documented.has(k)).sort();
  assert.deepEqual(
    undocumented,
    [],
    `app.js reads undocumented /api/models field(s) ${undocumented.join(', ')} — document them in docs/web-api.md first`,
  );
});

test('frontend /api/requests record reads stay documented (fold fields)', () => {
  const row = webApiDoc.split('\n').find((l) => l.includes('| GET |') && l.includes('`/api/requests?'));
  assert.ok(row, 'docs/web-api.md has no `| GET | `/api/requests?…` |` row');
  // persistedSummaryRow's record-field reads are the frontend's whole
  // dependency on the summary shape — pin each against the row's prose.
  const start = appJs.indexOf('function persistedSummaryRow(rec) {');
  assert.ok(start >= 0, 'app.js has no persistedSummaryRow — contract is blind');
  const end = appJs.indexOf('\n}', start);
  const reads = new Set([...appJs.slice(start, end).matchAll(/rec\.([a-z_]+)/g)].map((m) => m[1]));
  assert.ok(reads.size > 0, 'persistedSummaryRow reads no fields — contract is blind');
  const missing = [...reads].filter((k) => !row.includes(`\`${k}\``)).sort();
  assert.deepEqual(
    missing,
    [],
    `persistedSummaryRow reads undocumented /api/requests field(s) ${missing.join(', ')} — document them in docs/web-api.md first`,
  );
  // The fold contract itself: both directions (the folded hop chain and the
  // surviving-leg marker) must be documented words, or the UI drifts from
  // the API with no doc to catch it.
  for (const keyword of ['failed_hops', 'attempt_only']) {
    assert.ok(row.includes(`\`${keyword}\``), `docs/web-api.md /api/requests row omits \`${keyword}\``);
  }
});

test('settings form spec matches documented edit kinds and settings keys', () => {
  const start = appJs.indexOf('const SETTINGS_GROUPS = [');
  assert.ok(start >= 0, 'app.js has no SETTINGS_GROUPS spec');
  const end = appJs.indexOf('\n];', start);
  assert.ok(end > start, 'SETTINGS_GROUPS block is not terminated');
  const spec = appJs.slice(start, end);

  const kinds = [...new Set([...spec.matchAll(/kind: '([a-z_]+)'/g)].map((m) => m[1]))].sort();
  assert.deepEqual(kinds, ['general', 'guard', 'request_log', 'scheduling', 'stats']);
  const keys = [...new Set([...spec.matchAll(/key: '([a-z0-9_]+)'/g)].map((m) => m[1]))];
  assert.ok(keys.length > 0, 'SETTINGS_GROUPS declares no field keys — contract is blind');

  const editRow = webApiDoc.split('\n').find((l) => l.includes('| POST |') && l.includes('`/api/config/edit`'));
  assert.ok(editRow, 'docs/web-api.md has no `/api/config/edit` row');
  for (const kind of kinds) {
    assert.ok(editRow.includes(kind), `docs/web-api.md /api/config/edit row omits kind ${kind}`);
  }

  const configRow = webApiDoc.split('\n').find((l) => l.includes('| GET |') && l.includes('`/api/config`'));
  assert.ok(configRow, 'docs/web-api.md has no `/api/config` row');
  const missing = keys.filter((k) => !new RegExp(`\\b${k}\\b`).test(configRow)).sort();
  assert.deepEqual(
    missing,
    [],
    `SETTINGS_GROUPS keys missing from docs/web-api.md GET /api/config settings shape: ${missing.join(', ')}`,
  );
});

test('every settings field carries help text and a default', () => {
  const start = appJs.indexOf('const SETTINGS_GROUPS = [');
  assert.ok(start >= 0, 'app.js has no SETTINGS_GROUPS spec');
  const end = appJs.indexOf('\n];', start);
  assert.ok(end > start, 'SETTINGS_GROUPS block is not terminated');
  const spec = appJs.slice(start, end);

  const fields = [...spec.matchAll(/\{\s*key: '([a-z0-9_]+)'[^}]*\}/g)];
  assert.ok(fields.length > 0, 'SETTINGS_GROUPS declares no fields — contract is blind');
  for (const match of fields) {
    assert.ok(/help:\s*'/.test(match[0]), `settings field ${match[1]} has no (?) help text`);
    assert.ok(/def:\s*'/.test(match[0]), `settings field ${match[1]} has no default for its help popover`);
  }
});

test('request-log miss copy keeps up with the documented hot window', () => {
  // docs/web-api.md: the queryable window is the hot window
  // (request_log.hot_retention); rotated files that roll out of it are
  // compressed into offline .log.zst archives — a detail-drill 404 is not
  // only a retention sweep, and the MCP split stream archives on the same hot
  // window. The app.js copies must keep up (the pure.js explain not_found
  // note is covered in pure.test.mjs).
  const reqRow = webApiDoc.split('\n').find((l) => l.includes('| GET |') && l.includes('`/api/requests?'));
  assert.ok(reqRow, 'docs/web-api.md has no `| GET | `/api/requests?…` |` row');
  for (const fact of ['hot_retention', '.log.zst']) {
    assert.ok(reqRow.includes(fact), `docs/web-api.md /api/requests row omits ${fact} — the hot-window contract moved; update this pin with it`);
  }

  // The 404 hint of the Security drill card (applyRequestDrill).
  const drillStart = appJs.indexOf('async function applyRequestDrill(');
  assert.ok(drillStart >= 0, 'app.js has no applyRequestDrill — contract is blind');
  const drill = appJs.slice(drillStart, appJs.indexOf('\n}\n', drillStart));
  const miss = drill.match(/is not in the request log \(([^)]*)\)/);
  assert.ok(miss, 'applyRequestDrill has no 404 "is not in the request log (…)" hint — contract is blind');
  assert.ok(miss[1].includes('hot window'), `drill 404 hint omits the hot window: ${miss[1]}`);
  assert.ok(miss[1].includes('offline archive'), `drill 404 hint omits the offline archive: ${miss[1]}`);

  // The 404 hint of the Requests inline row expansion (toggleRequestDetail).
  // The row was LISTED, so a 404 means its record left the online log —
  // "did not commit" is the wrong story for a row archived past the hot window.
  const expandStart = appJs.indexOf('async function toggleRequestDetail(');
  assert.ok(expandStart >= 0, 'app.js has no toggleRequestDetail — contract is blind');
  const expand = appJs.slice(expandStart, appJs.indexOf('\n}\n', expandStart));
  const expandMiss = expand.match(/msg hint">not logged — ([^<]*)</);
  assert.ok(expandMiss, 'toggleRequestDetail has no 404 "not logged — …" hint — contract is blind');
  assert.ok(expandMiss[1].includes('left the online request log'),
    `inline-expand 404 hint must say the record left the online request log: ${expandMiss[1]}`);
  assert.ok(expandMiss[1].includes('hot window'), `inline-expand 404 hint omits the hot window: ${expandMiss[1]}`);
  assert.ok(!expandMiss[1].includes('did not commit, so there is no request-log record'),
    `inline-expand 404 hint still claims the request did not commit: ${expandMiss[1]}`);

  // The settings form's mcp_dir help: the split stream shares hot_retention
  // and archives past the hot window as mcp-*.log.zst.
  const specStart = appJs.indexOf('const SETTINGS_GROUPS = [');
  const spec = appJs.slice(specStart, appJs.indexOf('\n];', specStart));
  const field = /\{\s*key: 'mcp_dir'[^}]*\}/.exec(spec);
  assert.ok(field, 'SETTINGS_GROUPS has no mcp_dir field — contract is blind');
  const help = /help:\s*'([^']*)'/.exec(field[0]);
  assert.ok(help, 'the mcp_dir field has no help text');
  assert.ok(help[1].includes('hot_retention'), `mcp_dir help must list hot_retention among the shared policies: ${help[1]}`);
  assert.ok(help[1].includes('mcp-*.log.zst'), `mcp_dir help must name the split stream's archive: ${help[1]}`);
});
