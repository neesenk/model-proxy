// autorefresh.test.mjs — architecture guard for the auto-refresh interaction
// gate (see app.js "AUTO-REFRESH INTERACTION GATE"). Auto-refresh behavior is
// DOM-driven and cannot be observed from node; what CAN be pinned is the
// wiring: every timer-driven re-render must pass through deferAutoRefresh
// (tick AND commit), every transient popup layer must carry data-popup, and
// the two DOM-stateful rebuilds (native select options, log <details>) must
// keep their focused/open-state guards. These assertions reference exact
// symbols so unrelated refactors stay free.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import test from 'node:test';
import assert from 'node:assert/strict';

const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');

// fnBody extracts a top-level `function NAME(` / `async function NAME(`
// declaration's body via brace matching (comments and strings may contain
// braces only in balanced pairs — they do in this codebase; the helper below
// tracks nothing else because app.js templates keep braces balanced).
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

function count(haystack, needle) {
  return haystack.split(needle).length - 1;
}

test('the gate exists and observes popups, focus, and text selection', () => {
  const blocked = fnBody(appJs, 'autoRefreshBlocked');
  assert.ok(blocked.includes('POPUP_OPEN_SEL'), 'gate lost the popup selector');
  assert.ok(blocked.includes('comboMenuOpenIn'), 'gate lost the body-attached combobox menus');
  assert.ok(blocked.includes('focusInInteractive'), 'gate lost focus tracking');
  assert.ok(blocked.includes('textSelectionIn'), 'gate lost text-selection tracking');
  assert.ok(blocked.includes('refreshHoldReason'), 'gate no longer folds its decision through pure.js');
});

test('deferAutoRefresh parks one pending refresh per panel and cancels cleanly', () => {
  const defer = fnBody(appJs, 'deferAutoRefresh');
  assert.ok(defer.includes('autoRefreshBlocked('), 'defer must re-check the gate');
  assert.ok(defer.includes('holdWatchers'), 'defer must route through the shared watcher map');
  assert.ok(defer.includes('setInterval'), 'defer must poll for hold release');
  const cancel = fnBody(appJs, 'cancelAutoRefreshHold');
  assert.ok(cancel.includes('clearInterval') && cancel.includes('holdWatchers.delete'), 'cancel must stop the watcher');
});

test('the Status 5s tick gates at tick time and at commit time', () => {
  const body = fnBody(appJs, 'renderStatusTab');
  // commit-time gate (inside the try) + tick-time gate (interval callback):
  assert.ok(count(body, 'deferAutoRefresh(panels.status') >= 2,
    'renderStatusTab must gate both the post-fetch commit and the interval tick');
  assert.ok(body.includes('renderStatusTab(true)'), 'the hold watcher must re-run a fresh background fetch, not stale cache');
  const stop = fnBody(appJs, 'stopStatusRefresh');
  assert.ok(stop.includes('cancelAutoRefreshHold'), 'leaving the Status tab must drop the pending hold refresh');
});

test('the Analytics 30s tick gates at entry and at commit time', () => {
  const body = fnBody(appJs, 'renderAnalyticsTab');
  assert.ok(count(body, 'deferAutoRefresh(panel') >= 2,
    'renderAnalyticsTab must gate the entry wipe and the post-fetch landing writes');
  const arm = fnBody(appJs, 'analyticsMaybeAutoRefresh');
  assert.ok(count(arm, 'deferAutoRefresh(panel') >= 1, 'the interval tick must pass through the gate');
  const stop = fnBody(appJs, 'analyticsStopAutoRefresh');
  assert.ok(stop.includes('cancelAutoRefreshHold'), 'stopping analytics auto-refresh must drop the pending hold refresh');
});

test('the Security and Accounts 30s ticks gate, guard and stop cleanly', () => {
  // Security: interval tick passes through the gate and only while active.
  const sec = fnBody(appJs, 'securityMaybeAutoRefresh');
  assert.ok(sec.includes("classList.contains('active')"), 'the security tick must no-op off-tab');
  assert.ok(count(sec, 'deferAutoRefresh(panel') >= 1, 'the security tick must pass through the gate');
  const secStop = fnBody(appJs, 'securityStopAutoRefresh');
  assert.ok(secStop.includes('cancelAutoRefreshHold'), 'stopping security auto-refresh must drop the pending hold refresh');
  // Accounts: same gate/active discipline plus the mid-operation guard —
  // probes/quota polls/Test All disable their buttons, and a background
  // re-render would wipe their progress.
  const acc = fnBody(appJs, 'accountsMaybeAutoRefresh');
  assert.ok(acc.includes("classList.contains('active')"), 'the accounts tick must no-op off-tab');
  assert.ok(acc.includes('button:disabled'), 'the accounts tick must skip while an operation is in flight');
  assert.ok(count(acc, 'deferAutoRefresh(panel') >= 1, 'the accounts tick must pass through the gate');
  const accStop = fnBody(appJs, 'accountsStopAutoRefresh');
  assert.ok(accStop.includes('cancelAutoRefreshHold'), 'stopping accounts auto-refresh must drop the pending hold refresh');
  // loadAccountsData carries the commit-time gate for background loads.
  const load = fnBody(appJs, 'loadAccountsData');
  assert.ok(load.includes('deferAutoRefresh(panel, () => loadAccountsData(true))'),
    'background account loads must gate the landing render');
  assert.ok(load.includes("staleDataText('refresh failed'"),
    'background account failures must keep old data and use the stale banner');
  // Leaving either tab (either activator) stops both tickers — and the
  // stop names must MATCH the definitions (a wrong name is a runtime
  // ReferenceError the syntax check cannot catch).
  assert.ok(appJs.includes('function securityStopAutoRefresh()'), 'securityStopAutoRefresh must be defined');
  assert.ok(appJs.includes('function accountsStopAutoRefresh()'), 'accountsStopAutoRefresh must be defined');
  for (const activator of ['activateTab', 'activateTabSilent']) {
    const body = fnBody(appJs, activator);
    assert.ok(body.includes('securityStopAutoRefresh();'),
      activator + ' must stop the security ticker when leaving the tab');
    assert.ok(body.includes('accountsStopAutoRefresh();'),
      activator + ' must stop the accounts ticker when leaving the tab');
  }
});

test('every transient popup layer carries data-popup', () => {
  // .tr-popover: tokens picker + analytics picker (two template sites).
  assert.equal(count(appJs, 'class="tr-popover" data-popup'), 2,
    'both time-range popovers must declare data-popup for the gate');
  assert.ok(appJs.includes('class="route-pin-menu" data-popup'),
    'the pin menu must declare data-popup for the gate');
  assert.ok(appJs.includes("menu.setAttribute('data-popup', '')"),
    'combobox menus must declare data-popup for the gate');
  assert.ok(appJs.includes("drop.setAttribute('data-popup', '')"),
    'the legend "+N more" dropdown must declare data-popup for the gate');
});

test('the Status→Dashboard 30s analytics refresh gates at fetch and commit', () => {
  const body = fnBody(appJs, 'refreshDashboardData');
  // Fetch-time skip: no new fetch while a popup is open in the section.
  assert.ok(body.includes('main.querySelector(POPUP_OPEN_SEL)'),
    'refreshDashboardData must skip fetching while a popup is open');
  // Commit-time gate: an in-flight fetch defers its repaint, not clobber.
  assert.ok(body.includes('deferAutoRefresh(main,'),
    'a fetch landing mid-interaction must defer its repaint through the gate');
});

test('the live session <select> never swaps options while focused', () => {
  const body = fnBody(appJs, 'refreshLiveSessionOptions');
  assert.ok(body.includes('document.activeElement'),
    'an open native dropdown (select keeps focus) must not have its options rebuilt underneath');
  const card = fnBody(appJs, 'renderLiveCard');
  assert.ok(card.includes('sel.onblur'),
    'a deferred rebuild must retry on blur — the next SSE event may be far away');
});

test('the Logs re-render preserves expanded long-line <details>', () => {
  const body = fnBody(appJs, 'renderLogsInto');
  assert.ok(body.includes('logsOpenDetails'),
    'new log lines must not collapse details the user expanded');
});

// ---------------------------------------------------------------------------
// Failed background refreshes keep the last successful data (setRefreshError)
// ---------------------------------------------------------------------------

test('failed background refreshes keep old data and report via the banner', () => {
  const banner = fnBody(appJs, 'setRefreshError');
  assert.ok(banner.includes("querySelector(':scope > .refresh-err')"),
    'the banner must be idempotent (update in place, not stack)');
  assert.ok(banner.includes('panel.prepend'), 'the banner must sit above the panel layout');
  // Status: the per-part settle keeps the previous value on failure — never
  // an empty array that renders as "no data".
  const status = fnBody(appJs, 'renderStatusTab');
  assert.ok(status.includes('tokR.ok ? (tokR.data.usage || []) : prev.tok'),
    'a failed tokens fetch must keep the previous usage data');
  assert.ok(status.includes('!stR.ok && !statusCache.st'),
    'the full error card is only for the very first render (nothing to preserve)');
  const panel = fnBody(appJs, 'renderStatusPanel');
  assert.ok(panel.includes('setRefreshError(panel'),
    'renderStatusPanel must manage the banner (show on failures, clear on success)');
  // Analytics: fetch BEFORE the wipe, keep the previous view on failure.
  const an = fnBody(appJs, 'renderAnalyticsTab');
  assert.ok(an.includes('setRefreshError(panel'),
    'a failed analytics background refresh must keep the previous view');
  assert.ok(count(an, 'analyticsMaybeAutoRefresh()') >= 2,
    'the failure paths must re-arm the live-window timer (it would otherwise die)');
  // Dashboard: banner message says the data on screen is the last successful.
  const dash = fnBody(appJs, 'refreshDashboardData');
  assert.ok(dash.includes("dashData ? ' — showing last successful data' : ''"),
    'the dashboard error message must mark the kept data as stale');
});
