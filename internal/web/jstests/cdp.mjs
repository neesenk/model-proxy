// cdp.mjs — minimal zero-dependency Chrome DevTools Protocol driver for the UI
// e2e tests (uie2e.test.mjs). Uses Node's built-in WebSocket client; no npm
// packages, no CDN. Discovers a local Chromium/Chrome binary, launches it
// headless with --remote-debugging-port=0, and exposes just the CDP surface
// the tests need: attach, evaluate, page-error collection.

import { spawn } from 'node:child_process';
import { existsSync, readdirSync, statSync, mkdtempSync, rmSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// findBrowser locates a Chromium-family binary: $MP_BROWSER first, then
// playwright's download cache, then system installs. Returns null when none
// exists (the test suite treats that as an explicit external precondition and
// skips — or fails when MP_REQUIRE_UI_E2E=1).
export function findBrowser() {
  if (process.env.MP_BROWSER) {
    return existsSync(process.env.MP_BROWSER) ? process.env.MP_BROWSER : null;
  }
  const home = os.homedir();
  const candidates = [];
  for (const base of [
    path.join(home, 'Library/Caches', 'ms-playwright'),
    path.join(home, '.cache', 'ms-playwright'),
  ]) {
    if (!existsSync(base)) continue;
    for (const dir of readdirSync(base)) {
      if (!/^chromium/.test(dir)) continue;
      const root = path.join(base, dir);
      for (const rel of [
        'chrome-headless-shell-mac-arm64/chrome-headless-shell',
        'chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing',
        'chrome-mac/Chromium.app/Contents/MacOS/Chromium',
        'chrome-linux/chrome',
        'chrome-linux/headless_shell',
      ]) {
        const p = path.join(root, rel);
        if (isExecutable(p)) candidates.push(p);
      }
    }
  }
  for (const p of [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    '/Applications/Chromium.app/Contents/MacOS/Chromium',
    '/usr/bin/google-chrome',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
  ]) {
    if (isExecutable(p)) candidates.push(p);
  }
  return candidates[0] ?? null;
}

function isExecutable(p) {
  try {
    return statSync(p).mode & 0o111;
  } catch {
    return false;
  }
}

// launchBrowser starts the browser headless and resolves with the browser-level
// DevTools WebSocket URL plus the child process handle.
export function launchBrowser(bin) {
  const profile = mkdtempSync(path.join(os.tmpdir(), 'mp-uie2e-profile-'));
  const headlessFlag = path.basename(bin).includes('headless') ? [] : ['--headless=new'];
  const proc = spawn(bin, [
    ...headlessFlag,
    '--remote-debugging-port=0',
    `--user-data-dir=${profile}`,
    '--no-first-run',
    '--disable-gpu',
    '--disable-dev-shm-usage',
    'about:blank',
  ], { stdio: ['ignore', 'ignore', 'pipe'] });
  return new Promise((resolve, reject) => {
    let buf = '';
    const timer = setTimeout(() => reject(new Error(`browser did not expose DevTools within 30s; stderr so far:\n${buf}`)), 30000);
    proc.stderr.on('data', (chunk) => {
      buf += chunk;
      const m = buf.match(/DevTools listening on (ws:\/\/\S+)/);
      if (m) {
        clearTimeout(timer);
        resolve({ wsUrl: m[1], proc, profile });
      }
    });
    proc.on('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`browser exited early (code ${code}):\n${buf}`));
    });
  });
}

export class CDP {
  constructor(ws) {
    this.ws = ws;
    this.nextId = 1;
    this.pending = new Map();
    this.pageErrors = []; // Runtime.exceptionThrown + console.error, any session
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        msg.error ? reject(new Error(`${msg.error.message} (${msg.error.code})`)) : resolve(msg.result);
        return;
      }
      if (msg.method === 'Runtime.exceptionThrown') {
        this.pageErrors.push(msg.params?.exceptionDetails?.exception?.description
          ?? msg.params?.exceptionDetails?.text ?? 'unknown exception');
      }
      if (msg.method === 'Runtime.consoleAPICalled' && msg.params?.type === 'error') {
        this.pageErrors.push((msg.params.args ?? []).map((a) => a.value ?? a.description ?? '').join(' '));
      }
    };
  }

  static async connect(wsUrl) {
    const ws = new WebSocket(wsUrl);
    await new Promise((resolve, reject) => {
      ws.onopen = resolve;
      ws.onerror = () => reject(new Error(`WebSocket connect failed: ${wsUrl}`));
    });
    return new CDP(ws);
  }

  send(method, params = {}, sessionId = undefined) {
    const id = this.nextId++;
    const payload = { id, method, params };
    if (sessionId) payload.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify(payload));
    });
  }

  // openTab attaches a fresh tab (flatten mode) with Runtime/Page enabled and a
  // pre-navigation error hook; returns the sessionId for page-scoped commands.
  async openTab(url) {
    const { targetId } = await this.send('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await this.send('Target.attachToTarget', { targetId, flatten: true });
    await this.send('Runtime.enable', {}, sessionId);
    await this.send('Page.enable', {}, sessionId);
    await this.send('Page.addScriptToEvaluateOnNewDocument', {
      source: 'window.__errs=[];window.addEventListener("error",e=>window.__errs.push(String(e.message||e)));',
    }, sessionId);
    await this.send('Page.navigate', { url }, sessionId);
    return sessionId;
  }

  // evaluate runs an expression in the page and returns the value by reference
  // semantics (returnByValue + awaitPromise); throws on page exceptions.
  async evaluate(sessionId, expression) {
    const r = await this.send('Runtime.evaluate', {
      expression, returnByValue: true, awaitPromise: true,
    }, sessionId);
    if (r.exceptionDetails) {
      throw new Error(`page evaluation failed: ${r.exceptionDetails.text} ${r.exceptionDetails.exception?.description ?? ''}\nexpr: ${expression.slice(0, 200)}`);
    }
    return r.result?.value;
  }

  // setViewport drives responsive breakpoints live (CSS media queries react
  // to metrics overrides without reload). Pass null to clear.
  async setViewport(sessionId, width, height, mobile = false) {
    await this.send('Emulation.setDeviceMetricsOverride', {
      width, height, deviceScaleFactor: 1, mobile,
    }, sessionId);
  }

  async clearViewport(sessionId) {
    await this.send('Emulation.clearDeviceMetricsOverride', {}, sessionId);
  }

  // setColorScheme emulates prefers-color-scheme ('dark'/'light'); the design
  // contract's dark theme is pure CSS-variable redefinition behind that query.
  async setColorScheme(sessionId, scheme) {
    await this.send('Emulation.setEmulatedMedia', {
      features: [{ name: 'prefers-color-scheme', value: scheme }],
    }, sessionId);
  }

  // screenshot returns the rendered page as a PNG Buffer.
  async screenshot(sessionId) {
    const { data } = await this.send('Page.captureScreenshot', { format: 'png' }, sessionId);
    return Buffer.from(data, 'base64');
  }

  async close() {
    try { await this.send('Browser.close'); } catch { /* already gone */ }
    this.ws.close();
  }
}

export function killBrowser(browser) {
  if (!browser) return;
  try { browser.proc.kill('SIGKILL'); } catch { /* gone */ }
  try { rmSync(browser.profile, { recursive: true, force: true }); } catch { /* best-effort */ }
}
