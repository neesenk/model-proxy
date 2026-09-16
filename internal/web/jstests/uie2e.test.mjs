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
  for (const name of ['requests', 'security', 'analytics', 'config', 'accounts', 'status']) {
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
  await ctx.ev(`document.querySelector('[data-tab="status"]').click()`);
  await ctx.waitFor('status active', () => ctx.ev(
    `document.getElementById('tab-status').classList.contains('active')`));
  await ctx.waitFor('status nav mounted', () => ctx.ev(
    `!!document.querySelector('.status-nav-item[data-section="live"]')`));
  await ctx.ev(`document.querySelector('.status-nav-item[data-section="live"]').click()`);
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
