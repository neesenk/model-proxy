import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import test from 'node:test';
import assert from 'node:assert/strict';

// Contract: a panel that can show a stale-data banner must also clear it on
// recovery. The failure paths prepend `.refresh-err` and return WITHOUT
// re-rendering, so the ONLY thing that removes the banner is an explicit
// `setRefreshError(<panel>, null)` once the data is good again. A missing clear
// is not cosmetic: it leaves a permanent "refresh failed — showing last
// successful data" error on screen after a transient outage (daemon restart,
// network blip), which reads as a live failure and sends operators hunting a
// bug that already recovered. The status and analytics panels both shipped
// exactly that way.
//
// Static analysis (no DOM): every panel IDENTITY that shows a stale banner
// (`setRefreshError(X, staleDataText(`) must also have a `setRefreshError(X,
// null)` clear. Identities are resolved through each function's local alias
// (`const panel = panels.analytics` makes `panel` mean `panels.analytics`
// there), so a shared local name cannot mask a missing clear. The clear may
// live in a sibling function — security deliberately splits its per-part
// bookkeeping into securityRefreshFail/securityRefreshOk.
const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');

function functionBodies(src) {
  const bodies = [];
  const re = /^(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*\{/gm;
  let m;
  while ((m = re.exec(src))) {
    const start = src.indexOf('{', m.index + m[0].length - 1);
    let depth = 0, i = start;
    for (; i < src.length; i++) {
      if (src[i] === '{') depth++;
      else if (src[i] === '}') { depth--; if (depth === 0) break; }
    }
    bodies.push({ name: m[1], body: src.slice(start, i + 1) });
  }
  return bodies;
}

// aliases maps a function's local panel bindings to their panels.<name>
// identity, so `panel` in renderAnalyticsTab resolves to panels.analytics.
function aliases(body) {
  const map = {};
  const re = /(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*panels\.([A-Za-z_$][\w$]*)\s*[;,]/g;
  let m;
  while ((m = re.exec(body))) map[m[1]] = `panels.${m[2]}`;
  return map;
}

function resolve(expr, alias) {
  return alias[expr] || expr;
}

test('every stale-banner show has a matching clear for the same panel', () => {
  const bodies = functionBodies(appJs);
  assert.ok(bodies.length > 20, `parsed only ${bodies.length} functions — regex drift?`);
  const showRe = /setRefreshError\(\s*([A-Za-z_$][\w$.]*)\s*,\s*staleDataText\(/g;
  const shown = new Map(); // identity -> showing function names
  const cleared = new Set();
  for (const fn of bodies) {
    const alias = aliases(fn.body);
    let m;
    while ((m = showRe.exec(fn.body))) {
      const id = resolve(m[1], alias);
      if (!shown.has(id)) shown.set(id, []);
      shown.get(id).push(fn.name);
    }
    const clearRe = /setRefreshError\(\s*([A-Za-z_$][\w$.]*)\s*,\s*null\s*\)/g;
    while ((m = clearRe.exec(fn.body))) cleared.add(resolve(m[1], alias));
  }
  const offenders = [];
  for (const [id, fns] of shown) {
    if (!cleared.has(id)) {
      offenders.push(`${id}: shows a stale banner in ${fns.join(', ')} but is never cleared (setRefreshError(${id}, null) missing)`);
    }
  }
  assert.deepEqual(offenders, [], offenders.join('\n'));
  assert.ok(shown.size >= 4, `only ${shown.size} panels show stale banners — regex drift?`);
});
