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

// fnCode 在 fnBody 之上再去掉注释：下面两条钉的是代码而不是注释——注释里会出现
// 被钉的标识符（旧 end 分支的注释就提到 liveDetailState），不去掉会误报。
function fnCode(src, name) {
  return fnBody(src, name).replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
}

test('a live end event never pre-marks the detail state: every terminal takes the fetch path', () => {
  // client-gone（499）不写终态行，但断开前已有失败跳时该 id 留有 attempt_only 腿行
  // （/api/requests/<id> 取得到）。end 分支一旦预置 not-logged，
  // shouldFetchDetail 就把这一行钉死成「无记录」且永不取详情、弹层不展示腿行。
  // 现在终局一律不写 liveDetailState：取详情只有 ensureLiveDetailFetched 一条路，
  // 404 由 detailFetchState 回落成 not-logged。
  const apply = fnCode(appJs, 'applyLiveEvent');
  assert.ok(apply.includes("if (e.type === 'end') {"), 'applyLiveEvent has no end branch — contract is blind');
  assert.ok(!apply.includes('liveDetailState'),
    'applyLiveEvent must not write liveDetailState (a pre-marked not-logged skips the detail fetch)');
  // 取详情的入口不按行状态开特例：所有已结束的行都经同一个门。
  const ensure = fnCode(appJs, 'ensureLiveDetailFetched');
  assert.ok(ensure.includes('shouldFetchDetail('), 'ensureLiveDetailFetched must gate on shouldFetchDetail');
  assert.ok(!ensure.includes('CLIENT_GONE_STATUS'), 'the ensure path must not special-case client-gone rows');
});

test('openLiveDetailPop routes the explicit re-open decision through detailReopenState with the row status', () => {
  // 规则本体是 pure.js detailReopenState（行为用例在 pure.test.mjs）：只有「取过且
  // 404」的 499 保留 not-logged 标记。这里钉接线——重开必须带着行状态走它，不得
  // 再按状态整类跳过清理（旧写法会把 499 的取详情错误也一起钉死）。
  const body = fnCode(appJs, 'openLiveDetailPop');
  assert.ok(body.includes('const state = liveDetailState.get(id);'),
    'openLiveDetailPop must read the raw liveDetailState entry for the re-open decision');
  assert.ok(body.includes('detailReopenState(state, row.status)'),
    'openLiveDetailPop must decide the re-open state via detailReopenState(state, row.status)');
  assert.ok(!body.includes('row.status !== CLIENT_GONE_STATUS'),
    'openLiveDetailPop must not skip the clear for a whole status class');
  // 状态确实变了才写回（无可清的终态不落一条空状态）。
  assert.ok(body.includes('if (reopened !== state) liveDetailState.set(id, reopened);'),
    'openLiveDetailPop must write the re-open state only when it changed');
});
