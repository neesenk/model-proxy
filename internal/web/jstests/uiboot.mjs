// uiboot.mjs — shared boot fixture for the UI browser e2e files
// (uie2e.test.mjs behavior flow, uivisual.test.mjs visual checks). One
// definition only: builds the real binary, boots a sandboxed `serve` (temp
// HOME, loopback, static provider + in-process stub upstream), launches
// headless Chromium and opens /ui/. Gating: no browser → skipReason set
// (tests t.skip); MP_REQUIRE_UI_E2E=1 escalates to a hard failure.

import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { findBrowser, launchBrowser, killBrowser, CDP } from './cdp.mjs';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..');

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const p = s.address().port;
      s.close(() => resolve(p));
    });
  });
}

async function waitFor(desc, fn, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const v = await fn();
      if (v) return v;
    } catch (e) { lastErr = e; }
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error(`timeout waiting for ${desc}${lastErr ? ` (last error: ${lastErr.message})` : ''}`);
}

export async function bootUiE2E() {
  const ctx = { skipReason: null, waitFor, shutdown: async () => {} };
  // 耗时的浏览器 e2e 是按需门禁，不进每次修改的默认矩阵：MP_UI_E2E=1 才启动
  // 浏览器与代理；此时无浏览器 → Skip，MP_REQUIRE_UI_E2E=1 → FAIL。
  if (!process.env.MP_UI_E2E) {
    ctx.skipReason = 'browser e2e is opt-in (耗时 UI 自动化不进每次全量): run with MP_UI_E2E=1';
    return ctx;
  }
  const bin = findBrowser();
  if (!bin) {
    ctx.skipReason = 'no Chromium/Chrome binary found (set MP_BROWSER to one)';
    if (process.env.MP_REQUIRE_UI_E2E) {
      throw new Error(`MP_REQUIRE_UI_E2E=1 but ${ctx.skipReason}`);
    }
    return ctx;
  }

  // Sandbox: isolated HOME, own config, real binary — never the developer's.
  ctx.sandbox = mkdtempSync(path.join(os.tmpdir(), 'mp-uie2e-'));
  mkdirSync(path.join(ctx.sandbox, 'home'));
  ctx.port = await freePort();
  ctx.baseUrl = `http://127.0.0.1:${ctx.port}`;

  // In-process loopback stub upstream: answers any POST with a valid OpenAI
  // chat.completion so a real request round-trips client → proxy → upstream
  // and back (terminal 5xx responses are not request-logged by design).
  ctx.upstream = http.createServer((req, res) => {
    req.resume();
    req.on('end', () => {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({
        id: 'chatcmpl-e2e', object: 'chat.completion',
        choices: [{ index: 0, message: { role: 'assistant', content: 'pong' }, finish_reason: 'stop' }],
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      }));
    });
  });
  await new Promise((r) => ctx.upstream.listen(0, '127.0.0.1', r));
  const upstreamPort = ctx.upstream.address().port;

  const proxyBin = path.join(ctx.sandbox, 'model-proxy');
  const build = spawnSync('go', ['build', '-o', proxyBin, '.'], { cwd: REPO, encoding: 'utf8' });
  assert.equal(build.status, 0, `go build failed:\n${build.stderr}`);

  writeFileSync(path.join(ctx.sandbox, 'config.yaml'), `listen: 127.0.0.1:${ctx.port}
providers:
  dummy:
    provider_id: static
    openai_base_url: http://127.0.0.1:${upstreamPort}
    models: [m1]
request_log:
  enabled: true
`, { mode: 0o600 });

  // Static providers fail closed without a pool account (the request would die
  // before the capture layer and never reach the request log). Write a real
  // plural pool: ~/.model-proxy/<name>_apikeys.json, id = sha256(api_key)[:16]
  // — the same shape internal/app's writePoolFile fixture produces.
  const key = 'STATIC-TEST-KEY';
  const id = crypto.createHash('sha256').update(key).digest('hex').slice(0, 16);
  const poolDir = path.join(ctx.sandbox, 'home', '.model-proxy');
  mkdirSync(poolDir, { recursive: true });
  writeFileSync(path.join(poolDir, 'dummy_apikeys.json'), JSON.stringify({
    version: 1,
    accounts: [{ id, label: 'e2e', api_key: key, added_at: '2026-07-08' }],
  }), { mode: 0o600 });

  ctx.proxyLog = path.join(ctx.sandbox, 'serve.log');
  ctx.proxy = spawn(proxyBin, ['serve'], {
    cwd: ctx.sandbox,
    env: { HOME: path.join(ctx.sandbox, 'home'), PATH: process.env.PATH },
    stdio: ['ignore', 'ignore', 'ignore'],
  });

  // 安全网：测试进程崩溃/被强杀时 after() 不一定执行，退出前尽力杀掉子进程，
  // 不留 serve/浏览器残迹。
  const killOrphans = () => {
    try { ctx.proxy?.kill('SIGKILL'); } catch { /* gone */ }
    try { ctx.browser?.proc?.kill('SIGKILL'); } catch { /* gone */ }
  };
  process.once('exit', killOrphans);

  await waitFor('proxy /api/status', async () => {
    const r = await fetch(`${ctx.baseUrl}/api/status`);
    return r.ok;
  }, 45000);

  ctx.browser = await launchBrowser(bin);
  ctx.cdp = await CDP.connect(ctx.browser.wsUrl);
  ctx.tab = await ctx.cdp.openTab(`${ctx.baseUrl}/ui/`);

  ctx.ev = (expr) => ctx.cdp.evaluate(ctx.tab, expr);
  ctx.shot = () => ctx.cdp.screenshot(ctx.tab);
  ctx.pageErrors = async () => {
    const inPage = await ctx.ev('window.__errs || []');
    return [...inPage, ...ctx.cdp.pageErrors];
  };
  // Navigation robustness: a tab opened while the proxy is still coming up can
  // land on chrome-error://chromewebdata/ and Chrome's auto-retry then swaps
  // the document — clicks dispatched to the doomed document are lost (the
  // dead-click flake). Wait for the REAL document: expected URL, complete,
  // app.js booted (boot()'s activateTabSilent marks a panel .active — tab
  // buttons alone exist in static HTML before the deferred module binds its
  // handlers). One re-navigation retry covers a first-load error page.
  const landedOnUi = () => ctx.ev(
    `location.href.startsWith('${ctx.baseUrl}/ui/') && document.readyState === 'complete'
      && !!document.querySelector('.tab-panel.active')
      && document.querySelectorAll('.tabs .tab').length >= 6`);
  try {
    await waitFor('real UI document loaded', landedOnUi, 20000);
  } catch {
    await ctx.cdp.send('Page.navigate', { url: `${ctx.baseUrl}/ui/` }, ctx.tab);
    await waitFor('real UI document loaded (after re-navigation)', landedOnUi, 30000);
  }

  ctx.shutdown = async () => {
    process.removeListener('exit', killOrphans);
    if (ctx.cdp) await ctx.cdp.close();
    killBrowser(ctx.browser);
    if (ctx.proxy) {
      // 先注册 exit 监听再发信号（顺序反了会漏事件白等超时）；SIGINT 的
      // 优雅 drain 超时后升级 SIGKILL——宁可硬杀也不留残迹。
      const exited = new Promise((r) => ctx.proxy.once('exit', r));
      ctx.proxy.kill('SIGINT');
      const graceful = await Promise.race([
        exited.then(() => true),
        new Promise((r) => setTimeout(() => r(false), 8000)),
      ]);
      if (!graceful) {
        ctx.proxy.kill('SIGKILL');
        await Promise.race([exited, new Promise((r) => setTimeout(r, 3000))]);
      }
    }
    if (ctx.upstream) await new Promise((r) => ctx.upstream.close(r));
    if (ctx.sandbox) rmSync(ctx.sandbox, { recursive: true, force: true });
  };
  return ctx;
}
