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
      const key = token.trim();
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

test('settings form spec matches documented edit kinds and settings keys', () => {
  const start = appJs.indexOf('const SETTINGS_GROUPS = [');
  assert.ok(start >= 0, 'app.js has no SETTINGS_GROUPS spec');
  const end = appJs.indexOf('\n];', start);
  assert.ok(end > start, 'SETTINGS_GROUPS block is not terminated');
  const spec = appJs.slice(start, end);

  const kinds = [...new Set([...spec.matchAll(/kind: '([a-z_]+)'/g)].map((m) => m[1]))].sort();
  assert.deepEqual(kinds, ['cache', 'general', 'guard', 'request_log', 'scheduling', 'stats']);
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
