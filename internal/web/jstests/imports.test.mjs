import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import test from 'node:test';
import assert from 'node:assert/strict';

// Contract: app.js consumes pure.js helpers through ONE ES-module import
// block. A helper referenced in app.js but missing from that block compiles
// fine (Go never sees it, jstests never load app.js) and then throws
// ReferenceError at RENDER time in the browser — which the status refresh
// handler catches and reports as the generic "refresh failed (status) —
// showing last successful data" banner. That is exactly how the missing
// accountRemainingLabel / scheduleTierLabel imports shipped once.
//
// This gate is pure static analysis (no DOM, no module execution):
//   1. every pure.js export referenced in app.js must be imported;
//   2. every name in the import block must exist in pure.js (typo guard).
const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');
const pureJs = readFileSync(join(here, '..', 'assets', 'pure.js'), 'utf8');

function pureExports() {
  const names = new Set();
  const re = /^export\s+(?:async\s+)?(?:function\s+\*?\s*([A-Za-z_$][\w$]*)|(?:const|let|var)\s+([A-Za-z_$][\w$]*))/gm;
  let m;
  while ((m = re.exec(pureJs))) names.add(m[1] || m[2]);
  assert.ok(names.size > 50, `parsed only ${names.size} pure.js exports — regex drift?`);
  return names;
}

function importedFromPure() {
  const start = appJs.indexOf('import {');
  assert.ok(start >= 0, 'app.js has no `import {` block');
  const end = appJs.indexOf("} from './pure.js';", start);
  assert.ok(end > start, "app.js has no `} from './pure.js';` terminator");
  const block = appJs.slice(start, end);
  const names = new Set();
  for (const part of block.slice(block.indexOf('{') + 1).split(',')) {
    const name = part.trim().split(/\s+/).pop();
    if (name) names.add(name);
  }
  assert.ok(names.size > 50, `parsed only ${names.size} imported names — regex drift?`);
  return names;
}

// appJs minus the import block: a name occurring here is genuinely used.
function appBody() {
  const end = appJs.indexOf("} from './pure.js';") + "} from './pure.js';".length;
  return appJs.slice(end);
}

test('app.js imports every pure.js helper it references', () => {
  const exports_ = pureExports();
  const imported = importedFromPure();
  const body = appBody();
  const missing = [];
  for (const name of exports_) {
    if (imported.has(name)) continue;
    if (new RegExp(`\\b${name}\\b`).test(body)) missing.push(name);
  }
  assert.deepEqual(missing, [],
    `pure.js exports referenced by app.js but missing from its import block ` +
    `(browser ReferenceError at render time): ${missing.join(', ')}`);
});

test('app.js import block names exist in pure.js', () => {
  const exports_ = pureExports();
  const imported = importedFromPure();
  const unknown = [...imported].filter((n) => !exports_.has(n));
  assert.deepEqual(unknown, [],
    `names imported from pure.js that pure.js does not export: ${unknown.join(', ')}`);
});
