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
