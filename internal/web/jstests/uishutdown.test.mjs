// uishutdown.test.mjs — real-browser regression for the daemon restart: the
// daemon announces its stop, and the page handles it.
//
// A daemon that is told to stop drains its in-flight handlers for 8s before
// it force-closes connections. The page's one long-lived handler is the Live
// view's /api/events stream — left open, it held every restart to that
// deadline (serve audit 2026-11-26: 3 of 4 restarts logged `HTTP drain
// exceeded 8s`, two of them with exactly one in-flight handler). The daemon
// now ends each stream itself with a named `shutdown` frame carrying
// `retry: 1000`: the handler returns at once, and the page — told what is
// happening — shows its restarting state instead of an error, keeps its data
// without "refresh failed" banners, and is reconnected by its own
// EventSource within a second of the daemon coming back.
//
// Pinned here against a real stop → start of the sandboxed daemon (the
// sequence of scripts/restart_serve.sh):
//   announced — from the stop signal on, the page is in its restarting state
//               (never red "connection lost"), with no error banner;
//   released  — the draining daemon leaves long before its deadline;
//   resumed   — the SAME page carries live rows again afterwards (nothing on
//               the page re-creates the stream: only the browser's own
//               reconnect, on the frame's retry, can have brought it back).
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

// The drain deadline is cliserve.GracefulShutdownTimeout (8s). A stream the
// daemon ends itself releases the handler at once, so the daemon leaves with
// its final flush alone — seconds under the deadline, and under the page's 5s
// heartbeat too (measured 8031ms before the fix, 5.3s with a heartbeat-based
// release on the page).
const DRAIN_DEADLINE_MS = 8000;
const RELEASE_BUDGET_MS = 4000;

let ctx = null;

before(async () => { ctx = await bootUiE2E(); });
after(async () => { if (ctx) await ctx.shutdown(); });

test('Live 页收到 daemon 的重启通知后进入「服务重启中」而非报错，daemon 不被占到 deadline，重启后自动接回 (关停兼容族)', async (t) => {
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

  // `serve stop` is a SIGTERM to the supervisor, which forwards it to the
  // worker and exits once the worker has drained and flushed.
  const exited = new Promise((resolve) => ctx.proxy.once('exit', (code, signal) => resolve({ code, signal })));
  const stoppedAt = Date.now();
  ctx.proxy.kill('SIGTERM');
  // From the stop signal on, sample what the page shows: the announcement
  // must reach it as the restarting state — never as a red "connection
  // lost" (what an unannounced outage, or a page discovering the stop from
  // its own failed heartbeat, would show).
  const shown = new Set();
  const sample = async () => {
    shown.add(await ctx.ev(`document.getElementById('live-dot').classList.contains('err') ? 'err'
      : document.getElementById('live-dot').classList.contains('warn') ? 'warn' : 'other'`));
  };
  const outcome = await exited;
  const elapsed = Date.now() - stoppedAt;
  await sample();
  // The daemon is gone; the shared teardown must not wait on it again until
  // the restart below hands it a new process.
  ctx.proxy = null;
  t.diagnostic(`daemon exited ${elapsed}ms after SIGTERM`);

  const stoppedLog = readFileSync(ctx.proxyLog, 'utf8');
  assert.ok(!stoppedLog.includes('HTTP drain exceeded'),
    `the page held the draining daemon to its deadline (${elapsed}ms):\n${stoppedLog}`);
  assert.ok(elapsed < RELEASE_BUDGET_MS,
    `daemon exit took ${elapsed}ms after SIGTERM; with its stream ended by the shutdown notice it must leave with its final flush alone, not the ${DRAIN_DEADLINE_MS}ms deadline`);
  assert.equal(outcome.signal, null, `the daemon was killed (${outcome.signal}) instead of exiting on its own`);

  // --- announced ------------------------------------------------------------
  assert.ok(shown.has('warn') && !shown.has('err'),
    `during the restart the header showed ${[...shown].join('/')}; want the restarting state (warn) and never "connection lost" (err)`);
  assert.ok(await ctx.ev(`document.getElementById('live-table').textContent.includes('服务重启中')`),
    'the empty Live table must say the daemon is restarting, not "reconnecting…"');
  assert.equal(await ctx.ev(`document.querySelectorAll('.refresh-err').length`), 0,
    'a restart the daemon announced is not a failed refresh: no error banner');
  assert.deepEqual(await ctx.pageErrors(), [], 'handling the shutdown notice must not raise in the page');

  // The daemon is away for a moment (the restart script's stop → start):
  // the page keeps restarting state through its own heartbeat ticks.
  const goneAt = Date.now();
  while (Date.now() - goneAt < 5500) {
    await sample();
    await new Promise((r) => setTimeout(r, 150));
  }
  assert.ok(!shown.has('err'),
    `while the daemon was away the header turned red (${[...shown].join('/')}); the page knows it is a restart`);

  // --- resumed --------------------------------------------------------------
  // The daemon comes back on the same port. The browser's EventSource
  // reconnects on the frame's retry; the page's probe sees the daemon and
  // ends the restarting state. No page code re-creates the stream.
  ctx.startProxy();
  await ctx.waitProxyReady();
  await ctx.waitFor('header heartbeat sees the daemon again', () => ctx.ev(
    `document.getElementById('live-dot').classList.contains('ok')`));
  await ctx.waitFor('live stream taken back', () => ctx.ev(
    `document.getElementById('live-table').textContent.includes('Waiting for requests')`));
  assert.equal(await ctx.ev(`document.querySelectorAll('.refresh-err').length`), 0,
    'no error banner may appear once the daemon is back either');

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
