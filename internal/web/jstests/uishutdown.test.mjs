// uishutdown.test.mjs — real-browser regression for the restart drain: the
// page must be compatible with a daemon that stops and comes back.
//
// A daemon that is told to stop drains its in-flight handlers for 8s before
// it force-closes connections. The page's one long-lived handler is the Live
// view's /api/events subscription: left open, it held every restart to that
// deadline (serve audit 2026-11-26: 3 of 4 restarts logged `HTTP drain
// exceeded 8s`, two of them with exactly one in-flight handler and no stream
// attribution line). The page already runs a 5s header heartbeat on every
// non-Status tab; once the daemon stops, that heartbeat fails while the
// event stream still hangs off the draining process. Compatibility = the
// page releases the stream on a failed heartbeat (the old process finishes
// its drain and exits) and re-opens it on the next successful one (the SSE
// handshake replays the recent-event ring, so nothing visible is lost).
//
// Both halves are pinned here against a real stop → start of the sandboxed
// daemon, the same sequence as scripts/restart_serve.sh:
//   release — the draining daemon must not reach its deadline;
//   resume  — the SAME page must carry live rows again afterwards. The page
//             closed the stream itself, so the browser's EventSource
//             auto-reconnect is out of the picture: without the resume the
//             Live view would stay silent for good.
//
// Same hermetic fixture as uie2e.test.mjs (uiboot.mjs). This suite stops and
// restarts its own sandboxed serve mid-run, so it owns a boot of its own.
//
// External precondition: a local Chromium/Chrome binary — missing → skip;
// MP_REQUIRE_UI_E2E=1 turns that into a failure (same escalation pattern as
// uie2e).

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { bootUiE2E } from './uiboot.mjs';

// The drain deadline is cliserve.GracefulShutdownTimeout (8s). A page that
// releases its stream lets the daemon leave one heartbeat interval (5s) after
// the stop signal at the latest; a page that keeps it holds the daemon to the
// deadline itself (measured 8031ms before the fix).
const DRAIN_DEADLINE_MS = 8000;
const RELEASE_BUDGET_MS = 7500;

let ctx = null;

before(async () => { ctx = await bootUiE2E(); });
after(async () => { if (ctx) await ctx.shutdown(); });

test('Live 页在 daemon 关停时放开 /api/events、重启后接回，drain 不被页面占满 (关停兼容族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }

  // Requests → Live, and wait until the stream is actually open: the empty
  // table's hint flips to "Waiting for requests…" in the stream's onopen.
  await ctx.ev(`document.querySelector('[data-tab="requests"]').click()`);
  await ctx.waitFor('requests tab active', () => ctx.ev(
    `document.getElementById('tab-requests').classList.contains('active')`));
  await ctx.waitFor('requests nav mounted', () => ctx.ev(
    `!!document.querySelector('.req-nav-item[data-sub="live"][data-stream=""]')`));
  await ctx.ev(`document.querySelector('.req-nav-item[data-sub="live"][data-stream=""]').click()`);
  await ctx.waitFor('live card mounted', () => ctx.ev(`!!document.getElementById('live-table')`));
  await ctx.waitFor('live stream open', () => ctx.ev(
    `document.getElementById('live-table').textContent.includes('Waiting for requests')`));

  // --- release -------------------------------------------------------------
  // `serve stop` is a SIGTERM to the supervisor, which forwards it to the
  // worker and exits once the worker has drained and flushed.
  const exited = new Promise((resolve) => ctx.proxy.once('exit', (code, signal) => resolve({ code, signal })));
  const stoppedAt = Date.now();
  ctx.proxy.kill('SIGTERM');
  const outcome = await exited;
  const elapsed = Date.now() - stoppedAt;
  // The daemon is gone; the shared teardown must not wait on it again until
  // the restart below hands it a new process.
  ctx.proxy = null;
  t.diagnostic(`daemon exited ${elapsed}ms after SIGTERM`);

  const stoppedLog = readFileSync(ctx.proxyLog, 'utf8');
  assert.ok(!stoppedLog.includes('HTTP drain exceeded'),
    `the page held the draining daemon to its deadline (${elapsed}ms):\n${stoppedLog}`);
  assert.ok(elapsed < RELEASE_BUDGET_MS,
    `daemon exit took ${elapsed}ms after SIGTERM; with the stream released it must leave within one heartbeat interval, not the ${DRAIN_DEADLINE_MS}ms deadline`);
  assert.equal(outcome.signal, null, `the daemon was killed (${outcome.signal}) instead of exiting on its own`);

  // The page side: the heartbeat reports the daemon gone, and the empty
  // Live table says its stream is down instead of waiting for requests.
  await ctx.waitFor('header heartbeat reports the daemon gone', () => ctx.ev(
    `document.getElementById('live-dot').classList.contains('err')`));
  assert.ok(await ctx.ev(`document.getElementById('live-table').textContent.includes('reconnecting')`),
    'a released stream must leave the empty Live table on the reconnecting hint');
  assert.deepEqual(await ctx.pageErrors(), [], 'releasing the live stream must not raise in the page');

  // --- resume --------------------------------------------------------------
  // The daemon comes back on the same port. The page's next successful
  // heartbeat re-opens the stream; its onopen turns the hint back.
  ctx.startProxy();
  await ctx.waitProxyReady();
  await ctx.waitFor('header heartbeat sees the daemon again', () => ctx.ev(
    `document.getElementById('live-dot').classList.contains('ok')`));
  await ctx.waitFor('live stream taken back', () => ctx.ev(
    `document.getElementById('live-table').textContent.includes('Waiting for requests')`));

  // A request through the restarted daemon shows up as a live row: the
  // stream is delivering, not merely re-created. (The fresh daemon's event
  // ring is empty, so this row can only arrive over the live subscription.)
  const driven = await fetch(`${ctx.baseUrl}/v1/chat/completions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ model: 'm1', messages: [{ role: 'user', content: 'hi' }] }),
  });
  assert.equal(driven.status, 200, `driven request got ${driven.status}`);
  await ctx.waitFor('live row arrives through the resumed stream', () => ctx.ev(
    `document.querySelectorAll('#live-table tr.live-row[data-id]').length >= 1`), 20000);
  assert.deepEqual(await ctx.pageErrors(), [], 'taking the live stream back must not raise in the page');
});
