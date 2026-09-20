// uie2e.test.mjs — real browser end-to-end tests for the admin UI: the actual
// model-proxy binary serves the actual UI, driven by an actual headless
// Chromium over CDP. Boot fixture lives in uiboot.mjs (single definition);
// visual/screenshot checks live in uivisual.test.mjs.
//
// Hermetic: temp HOME / sandbox config / loopback port / in-process stub
// upstream; never touches real ~/.model-proxy, real credentials or the
// network. External precondition: a local Chromium/Chrome binary — missing →
// skip; MP_REQUIRE_UI_E2E=1 turns that into a failure (same escalation pattern
// as MP_REQUIRE_NODE in assets_test.go).

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdirSync, writeFileSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { bootUiE2E } from './uiboot.mjs';

let ctx = null;

before(async () => { ctx = await bootUiE2E(); });
after(async () => { if (ctx) await ctx.shutdown(); });

// driveRequest pushes n real client requests through the proxy to the stub
// upstream (each must round-trip 200), giving the UI real records to render.
// gapMs spaces them across distinct unix-second timestamps — the request
// log's keyset pagination (`to=<oldest loaded second>` + boundary rescan +
// id 去重) cannot advance when every record shares one second (实测：60 条同秒
// 记录让更旧页查询永远拉回同一批、去重后为零增长)。
async function driveRequest(n = 1, gapMs = 0) {
  for (let i = 0; i < n; i++) {
    const resp = await fetch(`${ctx.baseUrl}/v1/chat/completions`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ model: 'm1', messages: [{ role: 'user', content: 'hi' }] }),
    });
    assert.equal(resp.status, 200, `driveRequest(${i}) got ${resp.status}`);
    if (gapMs) await new Promise((r) => setTimeout(r, gapMs));
  }
}

// gotoRequestsWithRows lands on the Requests tab with at least one mounted
// data row (excluding the 0-height .req-spacer rows of the virtual scroll).
// The Refresh click rides the poll: the controls are built asynchronously
// after tab activation, so a single immediate click can no-op. Returns only
// after the table SETTLES — the poll's last refresh click may still be
// in-flight, and its landing rebuild (replaceChildren) would wipe rows that
// a subsequent click just expanded; worse, a rebuild during an in-flight
// detail fetch strands the detail row at "loading…" forever
// (toggleRequestDetail 在 row.isConnected=false 时放弃回填).
async function gotoRequestsWithRows() {
  await ctx.ev(`document.querySelector('[data-tab="requests"]').click()`);
  await ctx.waitFor('requests tab active', () => ctx.ev(
    `document.getElementById('tab-requests').classList.contains('active')`));
  await ctx.waitFor('mounted request row', async () => {
    await ctx.ev(`document.getElementById('req-refresh')?.click()`);
    return ctx.ev(`document.querySelectorAll('#req-table tbody tr:not(.req-spacer)').length >= 1`);
  });
  const signature = () => ctx.ev(
    `(document.querySelector('#req-table tbody tr:not(.req-spacer)')?.dataset.id || '')
      + '|' + document.querySelectorAll('#req-table tbody tr').length`);
  let prev = '';
  await ctx.waitFor('requests table settled', async () => {
    const cur = await signature();
    await new Promise((r) => setTimeout(r, 400));
    const next = await signature();
    const settled = cur === next && cur === prev && cur !== '|0';
    prev = cur;
    return settled;
  });
}

test('UI shell boots against the real backend', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.waitFor('connection meta leaves "connecting…"', () => ctx.ev(
    `document.getElementById('brand-meta').textContent.trim() !== 'connecting…'`));
  // Status tab (default) renders the configured provider from /api/status.
  await ctx.waitFor('status tab renders provider', () => ctx.ev(
    `document.getElementById('tab-status').innerHTML.includes('dummy')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'page must boot without JS errors');
});

test('every tab renders a non-empty panel', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  for (const name of ['requests', 'security', 'analytics', 'config', 'accounts', 'takeover', 'eval', 'status']) {
    await ctx.ev(`document.querySelector('[data-tab="${name}"]').click()`);
    await ctx.waitFor(`#tab-${name} active`, () => ctx.ev(
      `document.getElementById('tab-${name}').classList.contains('active')`));
    await ctx.waitFor(`#tab-${name} content`, () => ctx.ev(
      `document.getElementById('tab-${name}').innerHTML.length > 200`));
  }
  assert.deepEqual(await ctx.pageErrors(), [], 'tab navigation must not raise JS errors');
});

test('a real proxied request lands in the Requests UI', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }

  // Drive a real client request through the proxy to the stub upstream: the
  // response must round-trip verbatim (proving the full forward path), and
  // the committed exchange must be request-logged.
  const resp = await fetch(`${ctx.baseUrl}/v1/chat/completions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ model: 'm1', messages: [{ role: 'user', content: 'hi' }] }),
  });
  assert.equal(resp.status, 200, `expected 200 from stub upstream, got ${resp.status}`);
  const body = await resp.json();
  assert.equal(body.choices?.[0]?.message?.content, 'pong');

  // Server-side truth first: the record exists in the request log API.
  await ctx.waitFor('request record in /api/requests', async () => {
    const r = await fetch(`${ctx.baseUrl}/api/requests?limit=5`);
    const j = await r.json();
    return j.enabled && j.records.length >= 1 && j.records.some((rec) => (rec.called_model || '').includes('m1'));
  });

  // Then the UI surface: Requests tab, explicit user Refresh, row appears.
  await ctx.ev(`document.querySelector('[data-tab="requests"]').click()`);
  await ctx.waitFor('request row in #req-table', async () => {
    await ctx.ev(`document.getElementById('req-refresh')?.click()`);
    return ctx.ev(`document.querySelectorAll('#req-table tbody tr').length >= 1`);
  });
  assert.ok(await ctx.ev(`document.getElementById('req-table').innerText.includes('m1')`),
    'requests table should show the m1 request');
  assert.deepEqual(await ctx.pageErrors(), [], 'requests flow must not raise JS errors');
});

test('request row click expands and collapses inline detail (点击展开)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await driveRequest(1);
  await gotoRequestsWithRows();

  // 点击摘要行 → 行内展开详情（meta strip + 对话视图），再点 → 折叠。
  // 展开点击可能与在途 refresh 重建竞争（replaceChildren 会带走刚展开的
  // 详情行）：轮询中未展开则重点，直到详情稳定出现。
  await ctx.waitFor('inline detail expanded', async () => {
    const open = await ctx.ev(`!!document.querySelector('#req-table tbody tr.req-open')`);
    if (!open) {
      await ctx.ev(`document.querySelector('#req-table tbody tr:not(.req-spacer)')?.click()`);
    }
    return ctx.ev(`(() => {
      const tr = document.querySelector('#req-table tbody tr.req-open');
      const det = tr && tr.nextElementSibling;
      if (!det || !det.classList.contains('req-detail-row')) return false;
      const r = det.getBoundingClientRect();
      return r.width > 0 && r.height > 0;
    })()`);
  });
  // 详情体经 /api/requests/<id> 懒加载：可见性通过时可能还是 loading…，
  // 等真实内容填充。
  await ctx.waitFor('detail filled with request body', () => ctx.ev(
    `(document.querySelector('#req-table .req-detail-row')?.innerText || '').includes('hi')`));
  await ctx.waitFor('detail filled with upstream response', () => ctx.ev(
    `(document.querySelector('#req-table .req-detail-row')?.innerText || '').includes('pong')`));

  await ctx.ev(`document.querySelector('#req-table tbody tr.req-open').click()`);
  await ctx.waitFor('inline detail collapsed', () => ctx.ev(
    `!document.querySelector('#req-table tbody tr.req-open')
      && !document.querySelector('#req-table .req-detail-row')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'expand/collapse must not raise JS errors');
});

test('combobox popup opens, picks option, applies filter (浮层 + 下拉)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await driveRequest(1);
  await gotoRequestsWithRows(); // facets (provider 'dummy') load with the refresh

  // 点击 combobox 输入框 → 浮层打开（data-popup 非 hidden、几何非零）。
  await ctx.ev(`document.getElementById('req-provider').click()`);
  await ctx.waitFor('combo popup open', () => ctx.ev(`(() => {
    const m = document.querySelector('.combo-menu[data-popup]:not([hidden])');
    if (!m) return false;
    const r = m.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  })()`));
  // 选项含 facet 的 dummy provider；combobox 用 mousedown（不是 click）选中。
  await ctx.waitFor('dummy option in popup', () => ctx.ev(
    `!!document.querySelector('.combo-menu[data-popup]:not([hidden]) .combo-option[data-value="dummy"]')`));
  await ctx.ev(`document.querySelector('.combo-menu[data-popup]:not([hidden]) .combo-option[data-value="dummy"]')
    .dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }))`);
  // 选中后浮层关闭、值落入输入框、刷新后过滤生效（dummy 行仍在）。
  await ctx.waitFor('popup closed after pick', () => ctx.ev(
    `!document.querySelector('.combo-menu[data-popup]:not([hidden])')`));
  assert.equal(await ctx.ev(`document.getElementById('req-provider').value`), 'dummy');
  await ctx.waitFor('filtered rows still match', async () => {
    await ctx.ev(`document.getElementById('req-refresh')?.click()`);
    return ctx.ev(`document.querySelectorAll('#req-table tbody tr:not(.req-spacer)').length >= 1`);
  });
  assert.deepEqual(await ctx.pageErrors(), [], 'combobox flow must not raise JS errors');
});

test('suggestion-dropdown inputs and filter selects expose the inline ✕ clear (清除按钮族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await driveRequest(1);
  await gotoRequestsWithRows();

  // Combobox (Requests provider): typing turns on .has-text and reveals the ✕
  // inside the input's right edge. Enter commits the typed filter; clicking
  // the ✕ then clears, refocuses the input, and commits the empty filter
  // (the provider= key leaves the URL hash) — same path as a manual clear.
  const clearXOf = (id) => `document.getElementById('${id}').closest('.clearable').querySelector('.clear-x')`;
  const clickClearX = (id) => ctx.ev(`(() => {
    const b = ${clearXOf(id)};
    b.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, cancelable: true }));
    b.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    b.click();
  })()`);
  await ctx.ev(`(() => {
    const i = document.getElementById('req-provider');
    i.value = 'dummy';
    i.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  await ctx.waitFor('provider ✕ visible', () => ctx.ev(`(() => {
    const i = document.getElementById('req-provider');
    const host = i.closest('.clearable');
    return host.classList.contains('has-text')
      && ${clearXOf('req-provider')}.getBoundingClientRect().width > 0;
  })()`));
  await ctx.ev(`document.getElementById('req-provider')
    .dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' }))`);
  await ctx.waitFor('provider filter committed', () => ctx.ev(`location.hash.includes('provider=dummy')`));
  await clickClearX('req-provider');
  await ctx.waitFor('provider cleared + committed', () => ctx.ev(
    `document.getElementById('req-provider').value === '' && !location.hash.includes('provider=')`));
  await ctx.waitFor('✕ hidden after clear', () => ctx.ev(
    `!document.getElementById('req-provider').closest('.clearable').classList.contains('has-text')`));
  assert.equal(await ctx.ev(
    `document.activeElement === document.getElementById('req-provider')`), true, 'clear keeps focus on the input');

  // Datalist (Analytics provider): same affordance, commit lands in
  // localStorage via the input's change handler.
  await ctx.ev(`document.querySelector('[data-tab="analytics"]').click()`);
  await ctx.waitFor('analytics toolbar', () => ctx.ev(`!!document.getElementById('an-provider')`));
  await ctx.ev(`(() => {
    const i = document.getElementById('an-provider');
    i.value = 'zzz';
    i.dispatchEvent(new Event('input', { bubbles: true }));
    i.dispatchEvent(new Event('change', { bubbles: true }));
  })()`);
  await ctx.waitFor('analytics provider committed', () => ctx.ev(
    `localStorage.getItem('an-provider') === 'zzz' && !!document.getElementById('an-provider')`));
  await ctx.waitFor('analytics ✕ visible', () => ctx.ev(`(() => {
    const i = document.getElementById('an-provider');
    if (!i) return false;
    const host = i.closest('.clearable');
    return host && host.classList.contains('has-text')
      && host.querySelector('.clear-x').getBoundingClientRect().width > 0;
  })()`));
  await ctx.ev(`(() => {
    const b = document.getElementById('an-provider').closest('.clearable').querySelector('.clear-x');
    b.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    b.click();
  })()`);
  await ctx.waitFor('analytics provider cleared + committed', () => ctx.ev(
    `localStorage.getItem('an-provider') === '' && document.getElementById('an-provider')
      && document.getElementById('an-provider').value === ''`));

  // Filter <select> (Requests agent): a picked value shows the ✕ left of the
  // OS arrow; clicking it resets to All and commits through onchange.
  await ctx.ev(`document.querySelector('[data-tab="requests"]').click()`);
  await ctx.waitFor('requests tab active again', () => ctx.ev(
    `document.getElementById('tab-requests').classList.contains('active')`));
  await ctx.waitFor('agent option populated', () => ctx.ev(
    `[...document.getElementById('req-agent').options].some(o => o.value === 'node')`));
  await ctx.ev(`(() => {
    const s = document.getElementById('req-agent');
    s.value = 'node';
    s.dispatchEvent(new Event('change', { bubbles: true }));
  })()`);
  await ctx.waitFor('agent ✕ visible', () => ctx.ev(`(() => {
    const s = document.getElementById('req-agent');
    const host = s.closest('.clearable');
    return host && host.classList.contains('has-text')
      && host.querySelector('.clear-x').getBoundingClientRect().width > 0;
  })()`));
  await clickClearX('req-agent');
  await ctx.waitFor('agent reset to All + committed', () => ctx.ev(
    `document.getElementById('req-agent').value === '' && !location.hash.includes('agent=')`));
  await ctx.waitFor('select ✕ hidden after reset', () => ctx.ev(
    `!document.getElementById('req-agent').closest('.clearable').classList.contains('has-text')`));

  // Live view session select (#live-session): same ✕ reset, committed through
  // onLiveSessionChange (the session= key leaves the hash). One request carries
  // a session header so the dropdown has a real option (the pool comes from
  // /api/sessions, which only aggregates session-tagged records).
  {
    const resp = await fetch(`${ctx.baseUrl}/v1/chat/completions`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-claude-code-session-id': 'e2e-clear-sess' },
      body: JSON.stringify({ model: 'm1', messages: [{ role: 'user', content: 'hi' }] }),
    });
    assert.equal(resp.status, 200, `session drive failed: ${resp.status}`);
  }
  await ctx.ev(`document.querySelector('.req-nav-item[data-sub="live"][data-stream=""]').click()`);
  await ctx.waitFor('live view mounted', () => ctx.ev(`!!document.getElementById('live-session')`));
  await ctx.waitFor('live session option populated', () => ctx.ev(
    `[...document.getElementById('live-session').options].some(o => o.value)`));
  await ctx.ev(`(() => {
    const s = document.getElementById('live-session');
    s.value = [...s.options].find((o) => o.value).value;
    s.dispatchEvent(new Event('change', { bubbles: true }));
  })()`);
  await ctx.waitFor('live session ✕ visible', () => ctx.ev(`(() => {
    const s = document.getElementById('live-session');
    const host = s.closest('.clearable');
    return s.value && host && host.classList.contains('has-text')
      && host.querySelector('.clear-x').getBoundingClientRect().width > 0;
  })()`));
  await clickClearX('live-session');
  await ctx.waitFor('live session reset + committed', () => ctx.ev(
    `document.getElementById('live-session').value === '' && !location.hash.includes('session=')`));
  // Leave the Requests page back on the Log sub-view for the following tests.
  await ctx.ev(`document.querySelector('.req-nav-item[data-sub="log"][data-stream=""]').click()`);
  await ctx.waitFor('log view restored', () => ctx.ev(`!!document.getElementById('req-refresh')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'clear-affordance flow must not raise JS errors');
});

test('native select filter applies (下拉表单)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await driveRequest(1);
  await gotoRequestsWithRows();

  // agent 下拉由 facets 填充（我们驱动的请求 agent=node）；选择后过滤生效。
  await ctx.waitFor('agent option populated', () => ctx.ev(
    `[...document.getElementById('req-agent').options].some(o => o.value === 'node')`));
  await ctx.ev(`(() => {
    const s = document.getElementById('req-agent');
    s.value = 'node';
    s.dispatchEvent(new Event('change', { bubbles: true }));
  })()`);
  await ctx.waitFor('rows after agent filter', () => ctx.ev(
    `document.querySelectorAll('#req-table tbody tr:not(.req-spacer)').length >= 1`));
  assert.ok(await ctx.ev(`document.getElementById('req-table').innerText.includes('m1')`));
  // 还原不过滤，避免污染同文件的后续场景。
  await ctx.ev(`(() => {
    const s = document.getElementById('req-agent');
    s.value = '';
    s.dispatchEvent(new Event('change', { bubbles: true }));
  })()`);
  assert.deepEqual(await ctx.pageErrors(), [], 'select filter must not raise JS errors');
});

test('requests virtual scroll loads older pages near the bottom (滚动加载)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  // 超过首页 50 条：滚到底必须按需拉回更旧一页（reqLoadOlder）。
  await driveRequest(60, 100);
  await gotoRequestsWithRows();

  const height = () => ctx.ev(`document.documentElement.scrollHeight`);
  // 等首页挂载稳定（刷新完成后估算高度还会跳一次），再取基准。
  let h0 = 0;
  await ctx.waitFor('first page height stable', async () => {
    const h = await height();
    const stable = h === h0 && h > 0;
    h0 = h;
    return stable;
  });
  await ctx.ev(`window.scrollTo(0, document.documentElement.scrollHeight)`);
  await ctx.waitFor('older page loaded (document grew)', async () => (await height()) > h0, 20000);
  assert.ok(await ctx.ev(
    `document.querySelectorAll('#req-table tbody tr:not(.req-spacer)').length >= 1`),
    'table must keep mounted rows after paging');
  assert.deepEqual(await ctx.pageErrors(), [], 'virtual scroll paging must not raise JS errors');
});

// ---- 同族场景的全 UI 覆盖：展开/浮层/表单/懒加载不限于 Requests 页 ----

test('config 页 editor 折叠组开合，表单与 YAML 填充真实配置 (点击展开族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="config"]').click()`);
  await ctx.waitFor('config active', () => ctx.ev(
    `document.getElementById('tab-config').classList.contains('active')`));
  // 折叠组初始闭合，点击 summary 展开并异步填充表单（不再是 loading…）。
  for (const id of ['ed-provider', 'ed-settings']) {
    assert.ok(await ctx.ev(`!document.getElementById('${id}').open`), `${id} 应初始闭合`);
    await ctx.ev(`document.querySelector('#${id} summary').click()`);
    await ctx.waitFor(`#${id} 展开且填充`, () => ctx.ev(
      `document.getElementById('${id}').open
        && !document.querySelector('#${id} .editor-body').innerText.startsWith('loading')
        && document.querySelector('#${id} .editor-body').innerText.length > 20`));
  }
  // Raw YAML 编辑器（CodeMirror 5）加载真实配置文本。
  await ctx.waitFor('YAML 含 dummy provider', () => ctx.ev(
    `(document.querySelector('#yaml-editor .CodeMirror')?.innerText || '').includes('dummy')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'config 展开不得有 JS 错误');
});

test('security 页命中行点击展开 analyze，秘密片段掩码 (点击展开族 + guard 链)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  // 驱动一次带假 AWS key 的请求：guard.secrets 默认 log 档记录审计（不落内容），请求照常转发。
  const KEY = 'AKIAIOSFODNN7EXAMPLE';
  const resp = await fetch(`${ctx.baseUrl}/v1/chat/completions`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ model: 'm1', messages: [{ role: 'user', content: `my key is ${KEY}` }] }),
  });
  assert.equal(resp.status, 200);

  await ctx.ev(`document.querySelector('[data-tab="security"]').click()`);
  await ctx.waitFor('security active', () => ctx.ev(
    `document.getElementById('tab-security').classList.contains('active')`));
  // feed 出现 aws_access_key_id 命中行（可 analyze 的 .sec-row）。
  await ctx.waitFor('audit row in feed', () => ctx.ev(
    `[...document.querySelectorAll('#sec-table tr.sec-row')]
      .some(tr => tr.innerText.includes('aws_access_key_id'))`));
  // 点击行 → analyze 展开（explain：定位命中 + 规则身份），秘密片段掩码。
  await ctx.ev(`[...document.querySelectorAll('#sec-table tr.sec-row')]
    .find(tr => tr.innerText.includes('aws_access_key_id')).click()`);
  await ctx.waitFor('explain expanded', () => ctx.ev(
    `(document.getElementById('sec-table').innerText || '').length > 0
      && !!document.querySelector('#sec-table .sec-explain, #sec-table [class*="explain" ]')`));
  const pageText = await ctx.ev(`document.getElementById('tab-security').innerText`);
  assert.ok(!pageText.includes(KEY), '红线：完整秘密值绝不出现在 UI');
  assert.ok(pageText.includes('AKIA'), '掩码片段（头4…尾2）应可见');
  assert.deepEqual(await ctx.pageErrors(), [], 'analyze 展开不得有 JS 错误');
});

test('status Live 段点击行打开详情弹层并关闭 (弹层族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  // Live 监视已从 Status 迁到 Requests 页（侧栏 Live Requests 导航）。
  await ctx.ev(`document.querySelector('[data-tab="requests"]').click()`);
  await ctx.waitFor('requests active', () => ctx.ev(
    `document.getElementById('tab-requests').classList.contains('active')`));
  await ctx.waitFor('requests nav mounted', () => ctx.ev(
    `!!document.querySelector('.req-nav-item[data-sub="live"][data-stream=""]')`));
  await ctx.ev(`document.querySelector('.req-nav-item[data-sub="live"][data-stream=""]').click()`);
  await ctx.waitFor('live card mounted', () => ctx.ev(`!!document.getElementById('live-table')`));
  // SSE 订阅下驱动真实请求 → live 行出现。
  await driveRequest(1);
  await ctx.waitFor('live row arrives', () => ctx.ev(
    `document.querySelectorAll('#live-table tr.live-row[data-id]').length >= 1`), 20000);
  // 点击行 → dialog.live-detail-pop 打开且有内容；Close 按钮关闭。
  await ctx.ev(`document.querySelector('#live-table tr.live-row[data-id]').click()`);
  await ctx.waitFor('live detail pop open', () => ctx.ev(`(() => {
    const d = document.querySelector('dialog.live-detail-pop');
    if (!d || !d.open) return false;
    const r = d.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  })()`));
  await ctx.waitFor('live detail content', () => ctx.ev(
    `(document.querySelector('dialog.live-detail-pop .live-pop-body')?.innerText || '').length > 20`));
  await ctx.ev(`document.querySelector('dialog.live-detail-pop .live-pop-close').click()`);
  await ctx.waitFor('live detail pop closed', () => ctx.ev(
    `!document.querySelector('dialog.live-detail-pop')?.open`));
  assert.deepEqual(await ctx.pageErrors(), [], 'live 弹层不得有 JS 错误');
});

test('token usage 时间范围日历浮层开合 (弹层族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="status"]').click()`);
  await ctx.waitFor('status active', () => ctx.ev(
    `document.getElementById('tab-status').classList.contains('active')`));
  await ctx.waitFor('tokens section', () => ctx.ev(
    `!!document.querySelector('.status-nav-item[data-section="tokens"]')`));
  await ctx.ev(`document.querySelector('.status-nav-item[data-section="tokens"]').click()`);
  // 触发按钮 → .tr-popover[data-popup] 打开（含预设与日历）。
  await ctx.waitFor('range trigger', () => ctx.ev(`!!document.getElementById('tr-trigger')`));
  await ctx.ev(`document.getElementById('tr-trigger').click()`);
  await ctx.waitFor('range popover open', () => ctx.ev(`(() => {
    const p = document.querySelector('.tr-popover[data-popup]:not([hidden])');
    if (!p) return false;
    const r = p.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && p.querySelectorAll('.tr-presets *').length > 0;
  })()`));
  // 选一个预设范围 → 浮层关闭、触发器标签更新。
  await ctx.ev(`document.querySelector('.tr-popover .tr-presets button, .tr-popover .tr-presets [role="option"]').click()`);
  await ctx.waitFor('range popover closed after pick', () => ctx.ev(
    `!document.querySelector('.tr-popover[data-popup]:not([hidden])')`));
  assert.deepEqual(await ctx.pageErrors(), [], '日历浮层不得有 JS 错误');
});

test('请求详情 chat 历史懒加载：展开 earlier turns (懒加载族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  // 12 条消息 = 6 轮 > CHAT_RECENT(4)：前 2 轮收进 “N earlier turns” 懒加载。
  const msgs = [];
  for (let i = 0; i < 12; i++) msgs.push({ role: i % 2 ? 'assistant' : 'user', content: `turn-${i}` });
  const resp = await fetch(`${ctx.baseUrl}/v1/chat/completions`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ model: 'm1', messages: msgs }),
  });
  assert.equal(resp.status, 200);
  // 服务端事实：最新记录就是这条 12 轮请求。注意 request-log 索引是异步
  // reconcile（~250ms），records[0] 可能是上一条——用体积区分（12 轮 body
  // 远大于单条 'hi'）。
  const newest = await ctx.waitFor('newest record id', async () => {
    const r = await fetch(`${ctx.baseUrl}/api/requests?limit=1`);
    const j = await r.json();
    const rec = j.records?.[0];
    return rec && rec.request_size > 200 ? rec.request_id : null;
  });

  // 前序滚动场景可能把视口留在列表底部（旧记录）；回顶部并等虚拟滚动窗口
  // reconcile 到顶部、目标行挂载后再点（否则点到的是窗口残留的旧行）。
  await gotoRequestsWithRows();
  await ctx.ev(`window.scrollTo(0, 0)`);
  await ctx.waitFor('newest row mounted at top', () => ctx.ev(
    `window.scrollY < 200 && !!document.querySelector('#req-table tbody tr[data-id="${newest}"]')`));
  await ctx.waitFor('detail expanded', async () => {
    const open = await ctx.ev(`!!document.querySelector('#req-table tbody tr[data-id="${newest}"].req-open')`);
    if (!open) await ctx.ev(`document.querySelector('#req-table tbody tr[data-id="${newest}"]')?.click()`);
    return ctx.ev(`!!document.querySelector('#req-table .req-detail-row')`);
  });
  // 等详情体填充完（懒加载），且展开前 early turn 不在 DOM。
  await ctx.waitFor('detail filled', () => ctx.ev(
    `(document.querySelector('#req-table .req-detail-row')?.innerText || '').includes('turn-11')`));
  await ctx.waitFor('earlier-turns fold present', () => ctx.ev(
    `!!document.querySelector('#req-table .req-detail-row details.cv-history')`));
  assert.ok(await ctx.ev(
    `!(document.querySelector('#req-table .req-detail-row')?.textContent || '').includes('turn-0')`),
    '懒加载展开前 early turn 不应渲染');
  // 首次展开才解析渲染历史（cv-hist-scroll 出现且含 early turn）。注意该区域
  // 挂 content-visibility:auto 原生虚拟化，跳过渲染的子树 innerText 为空——
  // 必须用 textContent 断言。
  await ctx.ev(`document.querySelector('#req-table .req-detail-row details.cv-history summary').click()`);
  await ctx.waitFor('lazy history rendered', () => ctx.ev(
    `!!document.querySelector('#req-table .req-detail-row .cv-hist-scroll')
      && (document.querySelector('#req-table .req-detail-row').textContent || '').includes('turn-0')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'chat 懒加载不得有 JS 错误');
});

test('accounts 页点击 provider 渲染账号详情 (点击族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="accounts"]').click()`);
  await ctx.waitFor('accounts active', () => ctx.ev(
    `document.getElementById('tab-accounts').classList.contains('active')`));
  await ctx.waitFor('provider nav', () => ctx.ev(
    `[...document.querySelectorAll('.acct-nav-item')].some(b => b.innerText.includes('dummy'))`));
  await ctx.ev(`[...document.querySelectorAll('.acct-nav-item')]
    .find(b => b.innerText.includes('dummy')).click()`);
  // 主区渲染该 provider 的账号（夹具 label=e2e）。
  await ctx.waitFor('account detail', () => ctx.ev(
    `(document.querySelector('.acct-main')?.innerText || '').includes('e2e')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'accounts 选择不得有 JS 错误');
});

test('accounts 页 Token usage 时间范围选择器驱动窗口化拉取 (弹层族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  // 只在未激活时点击 tab：重复点击会重新触发 renderAccountsTab 的前台
  // loadAccountsData（popover 状态虽跨重建存活，不必要的重拉只增加抖动）。
  await ctx.ev(`(() => {
    if (!document.getElementById('tab-accounts').classList.contains('active'))
      document.querySelector('[data-tab="accounts"]').click();
    return true;
  })()`);
  await ctx.waitFor('accounts active', () => ctx.ev(
    `document.getElementById('tab-accounts').classList.contains('active')`));
  // 选择器在各账号卡的 Token usage 区块内：展开（有数据的卡默认开，无数据
  // 的卡默认折叠——程序性展开不依赖默认）。
  await ctx.waitFor('tokens section mounted', () => ctx.ev(
    `!!document.querySelector('details.acct-section[data-sec="tokens"] .acc-range-host')`));
  await ctx.ev(`(() => {
    document.querySelectorAll('details.acct-section[data-sec="tokens"]').forEach((d) => { d.open = true; });
    return true;
  })()`);
  await ctx.waitFor('default label All Time', () => ctx.ev(
    `document.querySelector('.acc-range-host .tr-value').textContent.trim() === 'All Time'`));
  await ctx.ev(`document.querySelector('.acc-range-host .tr-trigger').click()`);
  await ctx.waitFor('accounts range popover open', () => ctx.ev(`(() => {
    const p = document.querySelector('.acc-range-host .tr-popover[data-popup]:not([hidden])');
    if (!p) return false;
    const r = p.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && p.querySelectorAll('.tr-presets *').length > 0;
  })()`));
  // 服务端事实先行：窗口化 /api/tokens 可用（同一解析器驱动 from/to）。选
  // Last 7d → popover 关闭、触发器标签与折叠态 hint 更新、前端带 from/to 重拉。
  const wr = await fetch(`${ctx.baseUrl}/api/tokens?from=0&to=${Math.floor(Date.now() / 1000)}`);
  assert.equal(wr.status, 200, 'windowed /api/tokens must answer 200');
  await ctx.ev(`[...document.querySelectorAll('.acc-range-host .tr-presets button')]
    .find(b => b.textContent.includes('Last 7d')).click()`);
  await ctx.waitFor('range applied to trigger', () => ctx.ev(
    `document.querySelector('.acc-range-host .tr-value').textContent.trim() === 'Last 7d'`));
  await ctx.waitFor('range applied to collapsed hint', () => ctx.ev(
    `[...document.querySelectorAll('.acct-section[data-sec="tokens"] .acct-hint')]
      .every(h => h.textContent.includes('Last 7d'))`));
  await ctx.waitFor('accounts range popover closed after pick', () => ctx.ev(
    `!document.querySelector('.acc-range-host .tr-popover[data-popup]:not([hidden])')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'accounts 时间选择器不得有 JS 错误');
});

test('takeover 页：模板表渲染与真实 takeover/restore 闭环 (mutation 族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  // Install a fake claude config inside the sandbox HOME — the proxy process
  // runs with HOME=<sandbox>/home, so takeover rewrites exactly this file.
  const claudeDir = path.join(ctx.sandbox, 'home', '.claude');
  mkdirSync(claudeDir, { recursive: true });
  const claudeFile = path.join(claudeDir, 'settings.json');
  writeFileSync(claudeFile, '{"env":{"KEEP":"1"}}');

  await ctx.ev(`document.querySelector('[data-tab="takeover"]').click()`);
  await ctx.waitFor('takeover table renders claude row', () => ctx.ev(
    `document.querySelector('#tab-takeover .table') && document.querySelector('#tab-takeover').innerHTML.includes('claude')`));
  // Installed + not taken over → the row offers a Takeover button.
  await ctx.waitFor('claude row offers takeover', () => ctx.ev(
    `!!document.querySelector('[data-tk-takeover="claude"]')`));

  await ctx.ev(`document.querySelector('[data-tk-takeover="claude"]').click()`);
  // The confirm dialog renders the dry-run preview: the exact config the
  // takeover would write, before the run is confirmed.
  await ctx.waitFor('preview dialog open with rendered write', () => ctx.ev(
    `(() => { const w = document.getElementById('tkr-writes');
      return document.getElementById('tk-run-modal').open === true
        && w.innerHTML.includes('ANTHROPIC_BASE_URL')
        && !w.innerHTML.includes('KEEP'); })()`));
  assert.equal(await ctx.ev(`document.getElementById('tkr-writes').querySelector('.code') !== null`), true,
    'preview renders highlighted config');
  await ctx.ev(`document.getElementById('tkr-run').click()`);
  await ctx.waitFor('claude taken over (family row offers restore)', () => ctx.ev(
    `!!document.querySelector('[data-tk-restore="claude"]')`));
  const written = JSON.parse(readFileSync(claudeFile, 'utf8'));
  assert.equal(written.env.ANTHROPIC_BASE_URL, `http://127.0.0.1:${ctx.port}`,
    'takeover points the client at the sandbox proxy');
  assert.equal(written.env.ANTHROPIC_AUTH_TOKEN, 'PROXY_MANAGED');
  assert.equal(written.env.KEEP, '1', 'unrelated keys survive the rewrite');

  // Restore goes through the shared confirm modal.
  await ctx.ev(`document.querySelector('[data-tk-restore="claude"]').click()`);
  await ctx.waitFor('confirm modal open', () => ctx.ev(`document.getElementById('confirm-modal').open === true`));
  await ctx.ev(`document.getElementById('confirm-yes').click()`);
  await ctx.waitFor('claude restored (row offers takeover again)', () => ctx.ev(
    `!!document.querySelector('[data-tk-takeover="claude"]')`));
  assert.equal(readFileSync(claudeFile, 'utf8'), '{"env":{"KEEP":"1"}}',
    'restore returns the verbatim backup');
  assert.deepEqual(await ctx.pageErrors(), [], 'takeover flow must not raise JS errors');
});

test('takeover 模板编辑器：preset 查看/覆盖/校验拒绝/删除恢复 (模态族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="takeover"]').click()`);
  await ctx.waitFor('takeover table', () => ctx.ev(`!!document.querySelector('[data-tk-edit="claude"]')`));

  // Preset view: read-only textarea with the embedded document.
  await ctx.ev(`document.querySelector('[data-tk-edit="claude"]').click()`);
  await ctx.waitFor('template modal open', () => ctx.ev(
    `document.getElementById('tk-modal').open === true && !!document.getElementById('tk-yaml')`));
  // Every template opens EDITABLE — including built-in presets (saving
  // writes the user override; the hint says so).
  assert.equal(await ctx.ev(`document.getElementById('tk-yaml').hidden`), false);
  assert.equal(await ctx.ev(`document.getElementById('tk-yaml').readOnly`), false);
  assert.ok(await ctx.ev(`document.getElementById('tk-yaml').value.includes('ANTHROPIC_BASE_URL')`));
  // The rendered-config section dry-runs this exact template.
  await ctx.waitFor('rendered config preview', () => ctx.ev(
    `(document.getElementById('tk-rendered') || {}).innerHTML.includes('ANTHROPIC_BASE_URL')`));

  // Save As Override → the user template replaces the preset.
  await ctx.ev(`document.getElementById('tk-save').click()`);
  await ctx.waitFor('override active server-side', async () => {
    const r = await fetch(`${ctx.baseUrl}/api/takeover/templates/claude`);
    return r.ok && (await r.json()).source === 'user';
  });
  await ctx.waitFor('table re-rendered with user template', () => ctx.ev(
    `(() => { const b = document.querySelector('[data-tk-edit="claude"]');
      return b && b.textContent.trim() === 'Edit'; })()`));

  // Invalid candidate (pi family variant without protocol) is rejected
  // inline — the backend validates the merged template set fail-closed.
  await ctx.ev(`document.querySelector('[data-tk-new]').click()`);
  await ctx.waitFor('new-template modal', () => ctx.ev(
    `document.getElementById('tk-modal').open === true && !!document.getElementById('tk-name')`));
  // The Format picker seeds a starter skeleton per format; the placeholder
  // reference table documents the engine's variable set.
  assert.equal(await ctx.ev(`document.getElementById('tk-format').value`), 'json');
  assert.ok(await ctx.ev(`document.getElementById('tk-yaml').value.includes('# Takeover template')`),
    'starter skeleton prefilled');
  assert.ok(await ctx.ev(`document.querySelector('#tk-modal details').innerHTML.includes('placeholder')`),
    'placeholder help rendered');
  await ctx.ev(`(() => { const f = document.getElementById('tk-format'); f.value = 'toml';
    f.dispatchEvent(new Event('change')); })()`);
  assert.ok(await ctx.ev(`document.getElementById('tk-yaml').value.includes('top_keys')`),
    'toml starter swapped in');
  await ctx.ev(`(() => { const f = document.getElementById('tk-format'); f.value = 'json';
    f.dispatchEvent(new Event('change')); })()`);
  const badYaml = 'file: ~/x.json\nformat: json\nclient: pi\njson:\n  set:\n    env.X: "{{base_url}}"\n';
  await ctx.ev(`(() => { document.getElementById('tk-name').value = 'pi-broken';
    document.getElementById('tk-yaml').value = ${JSON.stringify(badYaml)}; })()`);
  await ctx.ev(`document.getElementById('tk-save').click()`);
  await ctx.waitFor('validation error inline', () => ctx.ev(
    `!document.getElementById('tk-msg').hidden && document.getElementById('tk-msg').textContent.includes('protocol')`));
  await ctx.waitFor('modal still open after rejected save', () => ctx.ev(
    `document.getElementById('tk-modal').open === true`));
  await ctx.ev(`document.getElementById('tk-cancel').click()`);

  // Delete the override (confirm modal) → the preset is active again.
  await ctx.ev(`document.querySelector('[data-tk-edit="claude"]').click()`);
  await ctx.waitFor('editor reopened', () => ctx.ev(`document.getElementById('tk-modal').open === true`));
  await ctx.ev(`document.getElementById('tk-delete').click()`);
  await ctx.waitFor('delete confirm modal', () => ctx.ev(`document.getElementById('confirm-modal').open === true`));
  await ctx.ev(`document.getElementById('confirm-yes').click()`);
  await ctx.waitFor('preset restored server-side', async () => {
    const r = await fetch(`${ctx.baseUrl}/api/takeover/templates/claude`);
    return r.ok && (await r.json()).source === 'preset';
  });
  assert.deepEqual(await ctx.pageErrors(), [], 'template editor flow must not raise JS errors');
});

test('eval 页渲染 shadow report 与 fusion 空态 (渲染族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="eval"]').click()`);
  await ctx.waitFor('shadow card empty hint', () => ctx.ev(
    `document.getElementById('tab-eval').innerHTML.includes('No shadow pairs')`));
  await ctx.waitFor('fusion card empty hint', () => ctx.ev(
    `document.getElementById('tab-eval').innerHTML.includes('No fusion workflows')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'eval tab must not raise JS errors');
});

test('请求详情 replay 条：常规下拉 + 按模型过滤 + 可读渲染 (mutation 族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await driveRequest();
  await gotoRequestsWithRows();
  await ctx.ev(`document.querySelector('#req-table tbody tr:not(.req-spacer)').click()`);
  await ctx.waitFor('replay strip in the detail row', () => ctx.ev(
    `!!document.querySelector('.req-detail-row [data-replay-run]')`));
  // The provider control is a PLAIN select (no free-typing input), and its
  // options are filtered to the providers that carry the request's model:
  // the sandbox config has exactly one provider (dummy, models [m1]) and the
  // driven request asked for m1, so dummy is the only eligible option.
  assert.ok(await ctx.ev(`document.querySelector('.req-detail-row [data-replay-provider]').tagName === 'SELECT'`),
    'provider 控件应是常规 select 下拉框');
  const opts = await ctx.ev(`[...document.querySelectorAll('.req-detail-row [data-replay-provider] option')].map((o) => o.value)`);
  assert.deepEqual(opts.filter(Boolean), ['dummy'], `按模型过滤后的 provider 选项（got: ${opts.join(',')}）`);
  await ctx.ev(`(() => { document.querySelector('.req-detail-row [data-replay-provider]').value = 'dummy'; })()`);
  await ctx.ev(`document.querySelector('.req-detail-row [data-replay-run]').click()`);
  await ctx.waitFor('replay answer status 200', () => ctx.ev(
    `(document.querySelector('.req-detail-row [data-replay-status]') || {}).textContent?.includes('200')`));
  // The replayed answer renders in the SAME readable format as the record
  // detail (chat view turns), with the raw body in the lazy details below.
  assert.ok(await ctx.ev(`!!document.querySelector('.req-detail-row [data-replay-result] .cv')`),
    'replay 结果应以 chat 视图（与请求详情同格式）渲染');
  assert.ok(await ctx.ev(`!!document.querySelector('.req-detail-row [data-replay-result] .raw-body')`));
  assert.deepEqual(await ctx.pageErrors(), [], 'replay flow must not raise JS errors');
});

test('status 页 schedule route test 与 models catalog refresh (mutation 族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="status"]').click()`);
  await ctx.waitFor('status nav', () => ctx.ev(`!!document.querySelector('[data-section="schedule"]')`));
  await ctx.ev(`document.querySelector('[data-section="schedule"]').click()`);
  await ctx.waitFor('route m1 test button', () => ctx.ev(`!!document.querySelector('[data-test-route="m1"]')`));
  await ctx.ev(`document.querySelector('[data-test-route="m1"]').click()`);
  await ctx.waitFor('route test probes ok', () => ctx.ev(
    `(document.querySelector('[data-test-result-for="m1"]') || {}).innerHTML?.includes('HTTP 200')`));

  await ctx.ev(`document.querySelector('[data-section="models"]').click()`);
  await ctx.waitFor('catalog refresh button', () => ctx.ev(`!!document.querySelector('[data-catalog-refresh]')`));
  await ctx.ev(`document.querySelector('[data-catalog-refresh]').click()`);
  await ctx.waitFor('catalog refreshed from the stub', () => ctx.ev(
    `(document.querySelector('[data-catalog-result]') || {}).textContent?.includes('models cached')`));
  // The stub catalog carries exactly one model.
  await ctx.waitFor('catalog count from stub', () => ctx.ev(
    `(document.querySelector('[data-catalog-result]') || {}).textContent?.includes('1 models')`));

  // Model Matching: the sandbox provider's m1 is not in the one-model stub
  // catalog, so the row starts unmatched; matching it to the stub's glm-4.7
  // writes catalog_alias via /api/config/edit and the reloaded match list
  // flips the row to matched + aliased (the backend owns the verdict).
  await ctx.waitFor('match details', () => ctx.ev(`!!document.querySelector('details.cat-match')`));
  await ctx.ev(`document.querySelector('details.cat-match > summary').click()`);
  await ctx.waitFor('m1 row unmatched', () => ctx.ev(
    `!!document.querySelector('details.cat-match [data-cat-match-edit][data-provider="dummy"][data-model="m1"]')`));
  assert.ok(await ctx.ev(`document.querySelector('details.cat-match').innerHTML.includes('0/1 matched')`),
    'match summary should start at 0/1 matched');
  await ctx.ev(`document.querySelector('details.cat-match [data-cat-match-edit]').click()`);
  await ctx.waitFor('match editor with catalog ids datalist', () => ctx.ev(
    `!!document.querySelector('details.cat-match .cat-match-editor input[list="cat-id-list"]')`));
  const modelsDoc = await ctx.ev(`fetch('/api/models').then((r) => r.json())`);
  assert.ok((modelsDoc.catalog_ids || []).includes('glm-4.7'),
    `/api/models catalog_ids should carry the stub id after refresh (got ${JSON.stringify({ catalog: modelsDoc.catalog, ids: (modelsDoc.catalog_ids || []).length, match: modelsDoc.match })})`);
  await ctx.waitFor('datalist carries the stub catalog id', () => ctx.ev(
    `[...document.querySelectorAll('#cat-id-list option')].some((o) => o.value === 'glm-4.7')`));
  await ctx.ev(`(() => {
    const input = document.querySelector('details.cat-match .cat-match-editor input');
    input.value = 'glm-4.7';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  await ctx.ev(`document.querySelector('details.cat-match [data-cat-match-save]').click()`);
  await ctx.waitFor('m1 row flips to matched via alias', () => ctx.ev(
    `(() => {
      const d = document.querySelector('details.cat-match');
      return d && d.innerHTML.includes('1/1 matched')
        && d.innerHTML.includes('<span class="badge muted">aliased</span>');
    })()`));
  // Clear removes the mapping (empty map deletes the key server-side) and the
  // row returns to unmatched.
  await ctx.ev(`document.querySelector('details.cat-match [data-cat-match-clear]').click()`);
  await ctx.waitFor('m1 row back to unmatched after clear', () => ctx.ev(
    `(() => {
      const d = document.querySelector('details.cat-match');
      return d && d.innerHTML.includes('0/1 matched')
        && !!d.querySelector('[data-cat-match-edit][data-model="m1"]');
    })()`));
  assert.deepEqual(await ctx.pageErrors(), [], 'status diagnostics must not raise JS errors');
});

test('takeover 确认对话框：变体切换驱动预览与执行单位 (表单族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  // Install a fake pi config so the pi family (multi-variant) is takeoverable.
  const piDir = path.join(ctx.sandbox, 'home', '.pi', 'agent');
  mkdirSync(piDir, { recursive: true });
  writeFileSync(path.join(piDir, 'models.json'), '{}');
  await ctx.ev(`document.querySelector('[data-tab="takeover"]').click()`);
  await ctx.waitFor('takeover table', () => ctx.ev(`!!document.querySelector('.tk-family')`));
  await ctx.waitFor('pi family offers takeover', () => ctx.ev(
    `!!document.querySelector('[data-tk-takeover="pi"]')`));

  // One row per family; the config format renders as a small badge.
  const famFormats = () => ctx.ev(`(() => {
    const out = {};
    for (const tr of document.querySelectorAll('#tab-takeover tbody tr')) {
      const name = tr.querySelector('.tk-fam-name');
      const badge = tr.querySelector('td:first-child .badge');
      if (name && badge) out[name.textContent] = badge.textContent;
    }
    return out;
  })()`);
  await ctx.waitFor('format badges render', async () => {
    const h = await famFormats();
    return h.opencode === 'json' && h.pi === 'json' && h.codex === 'toml';
  });

  // The pi family Takeover opens the confirm dialog with the auto variant
  // preselected; its preview shows the openai provider entry.
  await ctx.ev(`document.querySelector('[data-tk-takeover="pi"]').click()`);
  await ctx.waitFor('dialog open with auto variant chip pressed', () => ctx.ev(
    `document.getElementById('tk-run-modal').open === true`
    + ` && !!document.querySelector('[data-tkr-variant-chip="pi-openai"][aria-pressed="true"]')`));
  await ctx.waitFor('preview shows the openai provider entry', () => ctx.ev(
    `document.getElementById('tkr-writes').innerHTML.includes('pi-openai')`));

  // Picking another variant re-resolves the preview server-side (exact
  // template pin) and moves the preview to that variant's provider entry.
  await ctx.ev(`document.querySelector('[data-tkr-variant-chip="pi-responses"]').click()`);
  await ctx.waitFor('preview switched to the responses variant', () => ctx.ev(
    `document.getElementById('tkr-writes').innerHTML.includes('pi-responses')`));

  // Scope checkboxes: unchecking mcp narrows the preview to the model part
  // (families without an MCP block render no mcp checkbox — skip then).
  const hasMcpChip = await ctx.ev(`document.querySelector('[data-tkr-scope="mcp"]') !== null`);
  if (hasMcpChip) {
    await ctx.ev(`document.querySelector('[data-tkr-scope="mcp"]').click()`);
    await ctx.waitFor('mcp chip off preview', () => ctx.ev(
      `(() => { const w = document.getElementById('tkr-writes');
        return !w.innerHTML.includes('mcpServers'); })()`));
    await ctx.ev(`document.querySelector('[data-tkr-scope="mcp"]').click()`);
    await ctx.waitFor('mcp chip on preview', () => ctx.ev(
      `document.getElementById('tkr-writes').innerHTML.includes('mcpServers')`));
    // Subset chip: pick only one MCP server; the preview narrows to it.
    await ctx.ev(`(() => { const chips = document.querySelectorAll('[data-tkr-mcp]');
      for (let i = 1; i < chips.length; i++) chips[i].click(); })()`);
    await ctx.waitFor('mcp subset preview narrows', () => ctx.ev(
      `(() => { const w = document.getElementById('tkr-writes');
        const m = w.innerHTML.match(/127\.0\.0\.1:\d+\/mcp\/[a-z0-9-]+/g) || [];
        return m.length > 0 && new Set(m).size === 1; })()`));
  }

  // Model chips toggle too (regression: dataset key mismatch made model
  // chips no-ops) — deselect one model and the preview loses it.
  const modelChips = await ctx.ev(`document.querySelectorAll('[data-tkr-model]').length`);
  if (modelChips > 0) {
    const before = await ctx.ev(`(() => { const w = document.getElementById('tkr-writes');
      return (w.innerHTML.match(/127[.]0[.]0[.]1:\\d+\\/v1\\/chat\\/completions|glm-5/g) || []).length; })()`);
    await ctx.ev(`document.querySelector('[data-tkr-model]').click()`);
    await new Promise(r => setTimeout(r, 600));
    const after = await ctx.ev(`(() => { const w = document.getElementById('tkr-writes');
      return (w.innerHTML.match(/127[.]0[.]0[.]1:\\d+\\/v1\\/chat\\/completions|glm-5/g) || []).length; })()`);
    // toggling a model off must CHANGE the rendered preview (the sandbox
    // route table has few models, so any change proves the chip works)
    assert.notEqual(after, before, 'model chip toggle must change the preview');
  }

  // The split option previews the partition: m1 is openai-native, so split
  // assigns it to pi-openai alone — variants with no models are dropped
  // (no empty provider entries), exactly like the run would.
  await ctx.ev(`document.querySelector('[data-tkr-variant-chip="split"]').click()`);
  await ctx.waitFor('split preview shows the partition', () => ctx.ev(
    `(() => { const w = document.getElementById('tkr-writes');
      return w.innerHTML.includes('pi-openai') && !w.innerHTML.includes('pi-responses')
        && w.querySelectorAll('.tk-write').length === 1; })()`));

  // Cancel — nothing was written.
  await ctx.ev(`document.getElementById('tkr-no').click()`);
  assert.equal(await ctx.ev(`document.getElementById('tk-run-modal').open`), false);

  // Variant rows are gone entirely — one row per family, no stray templates.
  assert.equal(await ctx.ev(`document.querySelectorAll('#tab-takeover tbody tr').length`), 6,
    'exactly one row per family');
  assert.deepEqual(await ctx.pageErrors(), [], 'dialog flow must not raise JS errors');
});

// template editor draft preview: editing the YAML textarea re-renders the
// UNSAVED draft (debounced auto + manual Refresh), while the saved template
// stays untouched on disk until Save.
test('takeover 模板编辑器：草稿实时预览（未保存编辑即时渲染）', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="takeover"]').click()`);
  await ctx.waitFor('takeover table', () => ctx.ev(`!!document.querySelector('.tk-family')`));
  await ctx.ev(`document.querySelector('[data-tk-edit="claude"]').click()`);
  await ctx.waitFor('editor open with rendered preview', () => ctx.ev(
    `document.getElementById('tk-modal').open === true`
    + ` && !!document.getElementById('tk-rendered')`));
  await ctx.waitFor('initial preview renders the saved template', () => ctx.ev(
    `(document.getElementById('tk-rendered').textContent || '').includes('ANTHROPIC_BASE_URL')`));
  // Refresh (manual draft trigger) exists.
  assert.equal(await ctx.ev(`!!document.getElementById('tk-refresh-draft')`), true,
    'draft Refresh button present');
  // Edit the YAML (input event): the debounced draft preview must carry the
  // unsaved marker — rendered through template_body, not the disk template.
  const NL = 'String.fromCharCode(10)';
  await ctx.ev(
    `"use strict"; (() => { const ta = document.getElementById("tk-yaml");` +
    ` const lines = ta.value.split(${NL}).map(l =>` +
    ` l.trim().startsWith("env.ANTHROPIC_BASE_URL:") ? l + ${NL} + "    env.E2E_DRAFT_MARKER: live" : l);` +
    ` ta.value = lines.join(${NL}); ta.dispatchEvent(new Event("input")); })()`);
  await ctx.waitFor('draft preview carries the unsaved marker', () => ctx.ev(
    `(document.getElementById('tk-rendered').textContent || '').includes('E2E_DRAFT_MARKER')`),
    6000);
  // The disk template is untouched (the draft never saves): reopening the
  // editor shows the saved doc without the marker.
  await ctx.ev(`document.getElementById('tk-cancel').click()`);
  await ctx.ev(`document.querySelector('[data-tk-edit="claude"]').click()`);
  await ctx.waitFor('reopened editor renders the SAVED template', () => ctx.ev(
    `(document.getElementById('tk-rendered').textContent || '').includes('ANTHROPIC_BASE_URL')`));
  assert.equal(await ctx.ev(
    `(document.getElementById('tk-rendered').textContent || '').includes('E2E_DRAFT_MARKER')`),
    false, 'draft edits must not persist without Save');
  await ctx.ev(`document.getElementById('tk-cancel').click()`);
  assert.deepEqual(await ctx.pageErrors(), [], 'editor draft flow must not raise JS errors');
});

// admin-auth modal: dismissing it with Esc must resolve the boot promise —
// pre-fix, the promise only settled on a successful submit, so Esc left the
// whole UI hung on a blank page (no tab ever activated). Boots its own
// auth-enabled proxy instance (the shared ctx has no web.auth).
test('Esc on the admin-auth modal keeps boot alive', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  const auth = await bootUiE2E({ adminToken: 'e2e-admin-token' });
  if (auth.skipReason) { t.skip(auth.skipReason); return; }
  try {
    await auth.waitFor('admin-auth modal open', () => auth.ev(
      'document.getElementById("admin-auth-modal").open === true'));
    // Real Escape key press: the browser fires cancel + closes the dialog.
    await auth.cdp.send('Input.dispatchKeyEvent', {
      type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27,
    }, auth.tab);
    await auth.waitFor('modal dismissed', () => auth.ev(
      'document.getElementById("admin-auth-modal").open === false'));
    // Boot continued past the auth gate: the default tab activated (with the
    // pre-fix hang, activateTabSilent never ran and no tab had .active).
    await auth.waitFor('a tab activated after dismiss', () => auth.ev(
      'document.querySelector("[data-tab].active") !== null'));
    assert.deepEqual(await auth.pageErrors(), [], 'dismiss flow must not raise JS errors');
  } finally {
    await auth.shutdown();
  }
});

// MCP Live 的会话下拉必须只列 MCP 会话：/api/sessions 是 LLM-only 聚合，
// 把它灌进 MCP 流的下拉会把 Model 会话列在 MCP 工具栏下（回归：流切换后
// 旧池不清不换 + MCP 直接复用 /api/sessions）。
test('MCP Live 会话下拉只列 MCP 会话，不读 /api/sessions (流隔离族)', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="requests"]').click()`);
  await ctx.waitFor('requests tab active', () => ctx.ev(
    `document.getElementById('tab-requests').classList.contains('active')`));
  // 进入 Model 流 Live，等 LLM 池加载（真实 /api/sessions）。
  await ctx.ev(`document.querySelector('.req-nav-item[data-sub="live"][data-stream=""]').click()`);
  await ctx.waitFor('live card mounted', () => ctx.ev(`!!document.getElementById('live-table')`));
  // 仪表化 fetch：切换后命中 /api/sessions 即失败；kind=mcp 池查询喂固定记录。
  await ctx.ev(`(() => {
    window.__sessHits = 0;
    window.__origFetch = window.fetch;
    window.fetch = (url, ...rest) => {
      const u = String(url);
      if (u.includes('/api/sessions')) window.__sessHits++;
      if (u.includes('kind=mcp') && u.includes('limit=500')) {
        return Promise.resolve(new Response(JSON.stringify({
          enabled: true,
          records: [
            { request_id: 'mcp-live-e2e-1', kind: 'mcp', session_id: 'mcp-live-sess-a', ts: '2026-09-18T10:00:00Z', status: 200 },
            { request_id: 'mcp-live-e2e-2', kind: 'mcp', session_id: 'mcp-live-sess-b', ts: '2026-09-18T09:00:00Z', status: 200 },
          ],
          facets: { providers: [], models: [], agents: [], provider_models: {} },
        }), { headers: { 'content-type': 'application/json' } }));
      }
      return window.__origFetch(url, ...rest);
    };
  })()`);
  try {
    await ctx.ev(`document.querySelector('.req-nav-item[data-sub="live"][data-stream="mcp"]').click()`);
    await ctx.waitFor('mcp pool option mounted', () => ctx.ev(
      `!!document.querySelector('#live-session option[value="mcp-live-sess-a"]')`));
    // 下拉只含空值与 MCP 池会话（环内无 MCP 行时）；LLM 会话 id 不得残留。
    const opts = await ctx.ev(`[...document.querySelectorAll('#live-session option')].map((o) => o.value)`);
    const llmIds = (await (await fetch(`${ctx.baseUrl}/api/sessions?limit=200`)).json()).sessions.map((x) => x.session_id);
    for (const id of llmIds) {
      assert.ok(!opts.includes(id), `MCP 下拉不得列出 LLM 会话 ${id}（got: ${opts.join(',')}）`);
    }
    assert.ok(opts.includes('mcp-live-sess-b'), '池内第二个 MCP 会话也在下拉里');
    assert.equal(await ctx.ev('window.__sessHits'), 0, '切换到 MCP 流后不得再请求 /api/sessions');
  } finally {
    await ctx.ev(`window.fetch = window.__origFetch; delete window.__origFetch;`);
    // 回到 Model 流 Live，真实池重新加载（先还原 fetch 再切，避免假数据污染）。
    await ctx.ev(`document.querySelector('.req-nav-item[data-sub="live"][data-stream=""]').click()`);
    await ctx.waitFor('model pool reloaded', () => ctx.ev(
      `![...document.querySelectorAll('#live-session option')].some((o) => o.value === 'mcp-live-sess-a')`));
  }
  assert.deepEqual(await ctx.pageErrors(), [], 'MCP 会话下拉隔离不得有 JS 错误');
});
