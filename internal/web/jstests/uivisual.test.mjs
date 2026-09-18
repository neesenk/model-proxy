// uivisual.test.mjs — 视觉检查：核心问题是「UI 是否正常渲染」。在真实浏览器
// e2e 栈上（boot fixture: uiboot.mjs）逐 tab 断言**核心内容真实可见**——
// 不是 DOM 存在，而是渲染后的可见性（非零几何 + 非 hidden/display:none）+
// 真实数据文本（dummy provider、m1 请求行、KPI 瓦片、YAML 内容、账号导航），
// 并且没有错误横幅（.refresh-err / .msg.err）与 JS 错误。每 tab 落一张 PNG
// 截图（非空白 + 跨 tab 互不相同），写入 $MP_UI_SHOTS 或打印出的临时目录供
// 人工复核。
//
// 门控同 uie2e.test.mjs：无浏览器 Skip，MP_REQUIRE_UI_E2E=1 时 FAIL。

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { bootUiE2E } from './uiboot.mjs';

let ctx = null;
let shotsDir = null;

before(async () => {
  ctx = await bootUiE2E();
  if (ctx.skipReason) return;
  shotsDir = process.env.MP_UI_SHOTS || mkdtempSync(path.join(os.tmpdir(), 'mp-uishots-'));
  mkdirSync(shotsDir, { recursive: true });
  await ctx.cdp.setViewport(ctx.tab, 1440, 900);
  // 驱动一次真实代理请求，让 Requests 页有真实数据可渲染。
  const resp = await fetch(`${ctx.baseUrl}/v1/chat/completions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ model: 'm1', messages: [{ role: 'user', content: 'hi' }] }),
  });
  assert.equal(resp.status, 200, `boot request failed: ${resp.status}`);
});

after(async () => {
  if (shotsDir) console.log(`\nUI screenshots saved to: ${shotsDir}`);
  if (ctx) await ctx.shutdown();
});

// visible(sel)：渲染后真实可见（在文档中、非零几何、非 visibility/display 隐藏）。
const visible = (sel) => `(() => {
  const el = document.querySelector(${JSON.stringify(sel)});
  if (!el) return false;
  const r = el.getBoundingClientRect();
  const cs = getComputedStyle(el);
  return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && cs.display !== 'none';
})()`;

// 每个 tab 的「正常」判定：核心内容标记 + 必须出现的真实数据文本。
const TAB_MARKERS = {
  status: { sel: '.status-nav-item', text: 'dummy' },
  requests: { sel: '#req-table tbody tr:not(.req-spacer)', text: 'm1', refresh: true },
  security: { sel: '#sec-kpis .kpi' },
  analytics: { sel: '#an-kpis .kpi' },
  config: { sel: '#yaml-editor .CodeMirror, #yaml-editor-fallback', text: 'dummy' },
  accounts: { sel: '.acct-nav-item', text: 'dummy' },
  takeover: { sel: '#tab-takeover .table', text: 'claude' },
  mcp: { sel: '.mcp-host' },
  eval: { sel: '.eval-host', text: 'Shadow Report' },
};

const PNG_MIN_BYTES = 10000; // 渲染正常的 1440x900 页面不可能小于它

test('每个 tab 核心内容真实可见、无错误横幅、截图非空白', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }

  // 全局健康信号：顶栏已连上后端。
  await ctx.waitFor('连接元信息就绪', () => ctx.ev(
    `document.getElementById('brand-meta').textContent.trim() !== 'connecting…'`));

  const shots = {};
  for (const [name, marker] of Object.entries(TAB_MARKERS)) {
    await ctx.ev(`document.querySelector('[data-tab="${name}"]').click()`);
    await ctx.waitFor(`#tab-${name} 激活`, () => ctx.ev(
      `document.getElementById('tab-${name}').classList.contains('active')`));
    if (marker.refresh) {
      // 激活时 renderRequestsTab 异步构建控件，立即点 Refresh 会空踏；随轮询重点。
      await ctx.waitFor(`${name} 核心标记 ${marker.sel} 可见`, async () => {
        await ctx.ev(`document.getElementById('req-refresh')?.click()`);
        return ctx.ev(visible(marker.sel));
      });
    } else {
      // 核心内容可见
      await ctx.waitFor(`${name} 核心标记 ${marker.sel} 可见`, () => ctx.ev(visible(marker.sel)));
    }
    // 真实数据文本渲染到位
    if (marker.text) {
      await ctx.waitFor(`${name} 包含 "${marker.text}"`, () => ctx.ev(
        `document.getElementById('tab-${name}').innerText.includes('${marker.text}')`));
    }
    // 无可见错误横幅
    assert.ok(await ctx.ev(`![...document.querySelectorAll('#tab-${name} .refresh-err, #tab-${name} .msg.err')]
      .some(e => !e.hidden && e.offsetParent !== null && e.textContent.trim())`),
      `${name} tab 出现可见的错误横幅`);
    // 截图非空白
    const buf = await ctx.shot();
    assert.ok(buf[0] === 0x89 && buf.toString('latin1', 1, 4) === 'PNG', `${name} 截图不是 PNG`);
    assert.ok(buf.length > PNG_MIN_BYTES, `${name} 截图仅 ${buf.length} 字节——页面疑似空白`);
    writeFileSync(path.join(shotsDir, `tab-${name}.png`), buf);
    shots[name] = buf;
  }

  // 不同 tab 的渲染结果必须真的不同（防止全在拍同一个坏页面）。
  assert.ok(!shots.status.equals(shots.requests), 'status 与 requests 截图完全一致');
  assert.ok(!shots.status.equals(shots.security), 'status 与 security 截图完全一致');
  assert.deepEqual(await ctx.pageErrors(), [], '视觉检查全程不得有 JS 错误');
});

// 列几何均衡：表格不能「几列挤到一块、右侧大片死空白」，也不能列间重叠——
// fixed layout 的 colgroup 契约（docs/frontend.md：八列百分比、合计 100%）的
// 渲染级验证。
test('requests 表格列几何：不挤叠、无死空白、头体对齐', async (t) => {
  if (ctx.skipReason) { t.skip(ctx.skipReason); return; }
  await ctx.ev(`document.querySelector('[data-tab="requests"]').click()`);
  await ctx.waitFor('requests active', () => ctx.ev(
    `document.getElementById('tab-requests').classList.contains('active')`));
  await ctx.waitFor('mounted request row', async () => {
    await ctx.ev(`document.getElementById('req-refresh')?.click()`);
    return ctx.ev(`document.querySelectorAll('#req-table tbody tr:not(.req-spacer)').length >= 1`);
  });

  const geo = JSON.parse(await ctx.ev(`JSON.stringify((() => {
    const tbl = document.querySelector('#req-table .table');
    const cb = tbl.closest('.card-body');
    const ths = [...tbl.querySelectorAll('thead th')];
    const tds = [...tbl.querySelector('tbody tr:not(.req-spacer)').querySelectorAll('td')];
    const R = (el) => { const r = el.getBoundingClientRect(); return { left: r.left, right: r.right, width: r.width }; };
    return {
      tableWidth: tbl.getBoundingClientRect().width,
      cardBodyClientWidth: cb.clientWidth,
      cardPaddingLeft: parseFloat(getComputedStyle(cb).paddingLeft),
      cardPaddingRight: parseFloat(getComputedStyle(cb).paddingRight),
      cols: ths.map(R),
      dataCols: tds.map(R),
    };
  })())`));

  // 表铺满卡片内容区：右侧无大片死空白（fixed layout + width:100%；
  // clientWidth 含 padding，比的是内容盒）。
  const pad = geo.cardPaddingLeft + geo.cardPaddingRight;
  assert.ok(Math.abs(geo.tableWidth - (geo.cardBodyClientWidth - pad)) <= 2,
    `table width ${geo.tableWidth} vs card content ${geo.cardBodyClientWidth - pad}——右侧死空白或溢出`);
  // 八列契约；每列都有非挤压宽度；列间不重叠。
  assert.equal(geo.cols.length, 8, `列数 ${geo.cols.length}，应为 8`);
  for (const [i, col] of geo.cols.entries()) {
    assert.ok(col.width >= 24, `列 ${i} 宽 ${Math.round(col.width)}px——被挤压到不可读`);
    if (i > 0) {
      assert.ok(col.left >= geo.cols[i - 1].right - 1,
        `列 ${i} 与前一列重叠（left ${col.left} < prev right ${geo.cols[i - 1].right}）`);
    }
  }
  // 表头与数据行逐列对齐（fixed layout 的头体同一 colgroup）。
  assert.equal(geo.dataCols.length, geo.cols.length, '数据行列数与表头不一致');
  for (const [i, th] of geo.cols.entries()) {
    assert.ok(Math.abs(geo.dataCols[i].left - th.left) <= 2,
      `列 ${i} 头体错位：th.left ${th.left} vs td.left ${geo.dataCols[i].left}`);
  }
  assert.deepEqual(await ctx.pageErrors(), [], '列几何检查不得有 JS 错误');
});
