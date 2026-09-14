// reqvirt.test.mjs — architecture guard for the Requests table's virtual
// scroll geometry (app.js reqReconcile/reqDetailChanged). Row heights and
// spacers are DOM-driven and cannot be observed from node; what CAN be
// pinned is the wiring that keeps offsets honest when inline details open
// and close:
//   1. reqDetailChanged must re-measure EVERY mounted pooled row, not only
//      req-open ones — a closed row measured only-while-open keeps its stale
//      expanded height in v.heights and inflates every offset below it
//      (blank tail after collapsing a detail).
//   2. v.avg (the unmeasured-row estimate) must come from summary-row
//      heights only — folding open details into it inflates every estimate
//      and the tail spacer.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import test from 'node:test';
import assert from 'node:assert/strict';

const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');

// fnBody extracts a top-level function declaration's body via brace matching
// (same helper as autorefresh.test.mjs; app.js templates keep braces
// balanced).
function fnBody(src, name) {
  const re = new RegExp(`(?:async )?function ${name}\\(`);
  const open = src.search(re);
  assert.ok(open >= 0, `app.js no longer defines function ${name}()`);
  let i = src.indexOf('{', open);
  let depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') {
      depth--;
      if (depth === 0) break;
    }
  }
  assert.ok(i < src.length, `function ${name}() body is unbalanced`);
  return src.slice(src.indexOf('{', open), i + 1);
}

test('reqDetailChanged re-measures closed rows too (collapse must not strand the expanded height)', () => {
  const body = fnBody(appJs, 'reqDetailChanged');
  // The old guard skipped non-open rows outright: it must not come back.
  assert.ok(!body.includes("if (!tr.classList.contains('req-open') || !tr.isConnected) return"),
    'reqDetailChanged must not skip closed rows — that strands the stale expanded height');
  assert.ok(body.includes('if (!tr.isConnected) return'), 'unmounted rows are still skipped');
  // The detail's height joins the footprint ONLY while the row is open.
  assert.ok(body.includes("tr.classList.contains('req-open') && det && det.classList.contains('req-detail-row')"),
    'detail height must be gated on req-open');
});

test('reqReconcile derives the unmeasured-row average from summary-row heights only', () => {
  const body = fnBody(appJs, 'reqReconcile');
  assert.ok(body.includes('const rowH = m.tr.getBoundingClientRect().height'),
    'row height must be measured separately from the detail');
  assert.ok(body.includes('if (rowH > 0) { sum += rowH; cnt += 1; }'),
    'avg accumulator must use row-only heights');
  assert.ok(body.includes('v.avg = Math.max(12, sum / cnt)'),
    'avg must divide the row-only sum');
});

test("timeline jump scrolls only when expanding an off-screen row", () => {
  const body = fnBody(appJs, 'renderRequestsSessionSummary');
  const toggle = body.indexOf('await toggleRequestDetail(tr)');
  const rect = body.indexOf('tr.getBoundingClientRect()');
  const scroll = body.indexOf('window.scrollTo({ top: window.scrollY + rect.top');
  assert.ok(toggle >= 0, 'the detail toggle must be awaited');
  assert.ok(rect > toggle && scroll > rect, 'measure the mounted row after the fill, then jump');
  // rAF waits hang in background tabs (rAF paused), and smooth scrolling dies
  // on the first mid-flight replaceChildren — the jump must be instant and
  // rect-based (offsets drift by the estimate error of unmeasured rows).
  assert.ok(!body.includes('requestAnimationFrame'), 'no rAF wait may gate the scroll');
  assert.ok(body.includes("behavior: 'auto'"), 'the jump must be instant');
  // Collapsing, or expanding a row already in view, must not move the page —
  // the jump yanks the sticky Trace card under the user's cursor.
  assert.ok(body.includes("const opening = !tr.classList.contains('req-open')"),
    'the expand/collapse branch must be detected before the toggle');
  assert.ok(body.includes('if (!tr.isConnected || !opening) return'),
    'collapse must return before any scroll');
  assert.ok(body.includes('rect.top >= topClear && rect.bottom <= window.innerHeight'),
    'a row already visible (below the sticky card) must not scroll');
});
