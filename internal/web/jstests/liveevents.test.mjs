// liveevents.test.mjs — wiring pin for the SSE event-path session-options
// coalescing (see docs/frontend.md「视图状态进 URL hash」Live 段). The SSE
// handshake replays the hub's recent ring — up to 2000 events arriving within
// ~1s — and applyLiveEventDOM runs once per event. A synchronous
// refreshLiveSessionOptions per event (O(rows) filter+sort) froze the whole
// page under batch-scale event floods, so the event path must route through
// scheduleLiveSessionOptions while the user/load-driven paths (blur, fetch
// landing) stay synchronous. DOM behavior cannot be observed from node; what
// CAN be pinned is the wiring, exact symbols and call sites (same contract
// style as autorefresh.test.mjs).
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import test from 'node:test';
import assert from 'node:assert/strict';

const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');

// fnBody extracts a top-level `function NAME(` declaration's body via brace
// matching (same helper contract as autorefresh.test.mjs).
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

test('scheduleLiveSessionOptions coalesces: one pending timer, trailing flush', () => {
  const sched = fnBody(appJs, 'scheduleLiveSessionOptions');
  assert.ok(sched.includes('liveSessionOptionsTimer'), 'scheduler must guard on the pending-timer flag');
  assert.ok(sched.includes('if (liveSessionOptionsTimer) return;'), 'a scheduled refresh must collapse, not stack');
  assert.ok(sched.includes('setTimeout'), 'scheduler must defer (no synchronous rebuild)');
  // The timer flag must be cleared BEFORE the flush call so a refresh that
  // synchronously schedules again (or an exception) cannot wedge the flag.
  const clearAt = sched.indexOf('liveSessionOptionsTimer = 0');
  const flushAt = sched.indexOf('refreshLiveSessionOptions();');
  assert.ok(clearAt >= 0 && flushAt > clearAt, 'timer flag must reset before the flush call');
});

test('applyLiveEventDOM never rebuilds session options synchronously per event', () => {
  const body = fnBody(appJs, 'applyLiveEventDOM');
  assert.ok(!body.includes('refreshLiveSessionOptions('), 'event path must not call refreshLiveSessionOptions directly (replay burst freezes the main thread)');
  const scheduled = body.split('scheduleLiveSessionOptions(').length - 1;
  assert.ok(scheduled >= 2, 'both the session-mode branch and the All-mode prelude must route through the scheduler');
});

test('user- and load-driven session-options paths stay synchronous', () => {
  // The blur handler repaints the committed selection — deferring it would
  // leave uncommitted typing visible after blur (docs/frontend.md contract).
  assert.ok(appJs.includes("sel.onblur = () => refreshLiveSessionOptions();"),
    'the live-session blur handler must keep calling refreshLiveSessionOptions directly');
  // Fetch landing (onLiveSessionChange pool completion) repaints options and
  // value in the same task as the panel render; full renders keep their
  // synchronous refresh so a freshly painted table never shows stale options.
  const change = fnBody(appJs, 'onLiveSessionChange');
  assert.ok(change.includes('refreshLiveSessionOptions();'),
    'onLiveSessionChange must keep its synchronous post-fetch refresh');
  const render = fnBody(appJs, 'renderLiveTable');
  assert.ok(render.includes('refreshLiveSessionOptions();') &&
    !render.includes('scheduleLiveSessionOptions('),
    'renderLiveTable keeps its synchronous refresh (deferral is event-path only)');
});

// fnCode strips the comments on top of fnBody: the two pins below target
// code, not comments — a comment can mention a pinned identifier (the old end
// branch's comment named liveDetailState), which would false-positive.
function fnCode(src, name) {
  return fnBody(src, name).replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
}

test('a live end event never pre-marks the detail state: every terminal takes the fetch path', () => {
  // A client-gone (499) writes no terminal row, but failed hops before the
  // disconnect leave attempt_only legs under that id (/api/requests/<id>
  // returns them). Once the end branch presets not-logged, shouldFetchDetail
  // pins the row to "no record" for good: no detail fetch, and the popover
  // never shows the legs. No terminal writes liveDetailState now — the only
  // fetch path is ensureLiveDetailFetched, and a 404 falls back to not-logged
  // through detailFetchState.
  const apply = fnCode(appJs, 'applyLiveEvent');
  assert.ok(apply.includes("if (e.type === 'end') {"), 'applyLiveEvent has no end branch — contract is blind');
  assert.ok(!apply.includes('liveDetailState'),
    'applyLiveEvent must not write liveDetailState (a pre-marked not-logged skips the detail fetch)');
  // The fetch entry point has no per-status special case: every ended row
  // goes through the same gate.
  const ensure = fnCode(appJs, 'ensureLiveDetailFetched');
  assert.ok(ensure.includes('shouldFetchDetail('), 'ensureLiveDetailFetched must gate on shouldFetchDetail');
  assert.ok(!ensure.includes('CLIENT_GONE_STATUS'), 'the ensure path must not special-case client-gone rows');
});

test('openLiveDetailPop routes the explicit re-open decision through detailReopenState with the row status', () => {
  // The rule itself is pure.js detailReopenState (behavior covered in
  // pure.test.mjs): only a client-gone 499 whose fetch 404'd keeps the
  // not-logged mark. This pins the wiring — the re-open must route through it
  // with the row status and never skip the clear for a whole status class
  // again (the old form also pinned a client-gone fetch error for good).
  const body = fnCode(appJs, 'openLiveDetailPop');
  assert.ok(body.includes('const state = liveDetailState.get(id);'),
    'openLiveDetailPop must read the raw liveDetailState entry for the re-open decision');
  assert.ok(body.includes('detailReopenState(state, row.status)'),
    'openLiveDetailPop must decide the re-open state via detailReopenState(state, row.status)');
  assert.ok(!body.includes('row.status !== CLIENT_GONE_STATUS'),
    'openLiveDetailPop must not skip the clear for a whole status class');
  // Write back only when the state really changed (nothing to clear leaves no
  // empty state entry behind).
  assert.ok(body.includes('if (reopened !== state) liveDetailState.set(id, reopened);'),
    'openLiveDetailPop must write the re-open state only when it changed');
});
