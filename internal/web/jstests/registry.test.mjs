import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import test from 'node:test';
import assert from 'node:assert/strict';

// Contract (internal/web/assets/AGENTS.md「模式注册表」): similar UI must
// never grow a second implementation. Four gates:
//   1. class registry — every static class emitted by app.js/pure.js/
//      index.html is either styled in styles.css or exempted below WITH a
//      reason (an unregistered class is a one-off implementation);
//   2. color zones — hard-coded colors live only in :root blocks, the
//      CodeMirror section, or the small literal allowlist (anything new
//      must become a :root variable, per the styles.css header contract);
//   3. id anchors — #id selectors in styles.css are page-host structural
//      anchors only; reusable component styling rides classes;
//   4. docs lockstep — every backticked symbol in the AGENTS.md pattern
//      registry exists in the assets, so renames break the test instead of
//      silently rotting the registry.
const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');
const pureJs = readFileSync(join(here, '..', 'assets', 'pure.js'), 'utf8');
const html = readFileSync(join(here, '..', 'assets', 'index.html'), 'utf8');
const css = readFileSync(join(here, '..', 'assets', 'styles.css'), 'utf8');
const agentsMd = readFileSync(join(here, '..', 'assets', 'AGENTS.md'), 'utf8');

// stripCssComments blanks comments while preserving every offset and line
// number, so indexes found on the stripped text map 1:1 onto the raw file.
function stripCssComments(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ' '));
}

// rootBlockRanges returns [start, end) char ranges of every `:root { … }`
// block — the light-theme block plus the dark-mode redefinition inside
// @media (both are legitimate places for hard-coded colors).
function rootBlockRanges(text) {
  const ranges = [];
  let at = 0;
  for (;;) {
    const i = text.indexOf(':root', at);
    if (i < 0) break;
    const open = text.indexOf('{', i);
    if (open < 0) break;
    let depth = 1;
    let j = open + 1;
    for (; depth > 0 && j < text.length; j += 1) {
      if (text[j] === '{') depth += 1;
      else if (text[j] === '}') depth -= 1;
    }
    ranges.push([i, j]);
    at = j;
  }
  return ranges;
}

// --- gate 1: emitted classes are styled or exempted -----------------------

// Unstyled-but-intentional classes: structural/JS hooks whose styling rides
// a styled ancestor or a data attribute, bases of dynamically-suffixed
// classes, and modifiers that only toggle behavior. Every entry needs a
// reason; styling the class instead (via an existing pattern) is usually
// the right fix. Entries that stop being emitted are reported as dead.
const CLASS_EXEMPT = {
  'card-title': 'buildCard 标题行语义钩子，样式走 .card-head 上下文',
  'cv-history': '聊天历史滚动容器钩子，样式走 .cv-* 后代规则',
  'cv-hist-host': '聊天历史展开块宿主钩子（[data-chunk] 注册表）',
  'cv-system': 'system 消息折叠钩子，样式走 .cv-fold',
  'flat': 'KPI 增量行无变化修饰（.kpi .d 上下文）',
  'gp-lit': 'glob path 编辑行输入钩子，控件样式走 .req-input',
  'gp-del': 'glob path 编辑行删除按钮钩子，样式走 .btn',
  'gr-literal': 'guard 规则编辑行字面量输入钩子，控件样式走 .req-input',
  'gr-name': 'guard 规则编辑行名称输入钩子，控件样式走 .req-input',
  'gr-regex': 'guard 规则编辑行正则输入钩子，控件样式走 .req-input',
  'gr-del': 'guard 规则编辑行删除按钮钩子，样式走 .btn',
  'guard-rules-table': '复合 .table 的语义标记，样式走 .table',
  'hm-l': '热图等级动态前缀（hm-l${lvl}；hm-l1..l4 有样式）',
  'live-pop-body': 'Live 弹层主体钩子，样式走 dialog.live-detail-pop 后代',
  'live-pop-close': 'Live 弹层关闭按钮钩子，样式走 .btn',
  'live-pop-meta': 'Live 弹层 meta 行钩子，样式走 .modal-head .meta',
  'logs': 'Logs 视图标记（样式由 status-logs-active 状态类承载）',
  'model-caps-refresh': 'Test All 刷新按钮的 JS 绑定钩子，样式走 .btn',
  'prov-acct-c': '账号 tint 槽位动态前缀（prov-acct-c${slot}）',
  'prov-table': '复合 .table 的语义标记，样式走 .table',
  'raw-body': '懒渲染 raw body 钩子（注册表选择器是 [data-raw]）',
  'raw-body-host': '懒渲染 raw body 宿主钩子（同 [data-raw] 注册表）',
  'rt-model': 'route-target 行模型单元格钩子，样式走 .route-target-row',
  'rt-provider': 'route-target 行 provider 单元格钩子，样式走 .route-target-row',
  'sec-unblock': 'Blocked sessions 行内 unblock 按钮绑定钩子，样式走 .btn',
  'sess-meta-item': '会话身份行条目钩子，样式走 .sess-meta 容器',
  'status-warnings': '状态页警告容器钩子（内容自渲染 badge）',
  'tr-month': '时间线日历月份标签钩子（SVG 内联属性承载样式）',
  'tr-next': '时间线日历下月导航钩子（SVG 内联属性承载样式）',
  'tr-prev': '时间线日历上月导航钩子（SVG 内联属性承载样式）',
};

// emittedClasses collects the STATIC class tokens the frontend can emit.
// For interpolated class lists only the static prefix is knowable without
// evaluating the template; dynamic suffixes (badge-${kind},
// prov-acct-c${slot}, hm-l${lvl}) register their base token here.
function emittedClasses(...sources) {
  const out = new Set();
  const tokenOk = /^[A-Za-z][A-Za-z0-9_-]*$/;
  const add = (val) => {
    const staticPart = val.includes('${') ? val.slice(0, val.indexOf('${')) : val;
    for (const tok of staticPart.split(/\s+/)) {
      if (tokenOk.test(tok) && !tok.endsWith('-')) out.add(tok);
    }
  };
  for (const src of sources) {
    for (const m of src.matchAll(/class="([^"]*)"/g)) add(m[1]);
    for (const m of src.matchAll(/classList\.(?:add|toggle|remove)\(([^)]*)\)/g)) {
      for (const q of m[1].matchAll(/'([^']+)'/g)) add(q[1]);
    }
    for (const m of src.matchAll(/className\s*=\s*'([^']+)'/g)) add(m[1]);
  }
  return out;
}

// cssDefinedClasses collects selector class tokens from styles.css.
// A leading digit before the dot (decimal values like .06 / .5px) is not a
// class; comment occurrences count too, which only makes the set larger.
function cssDefinedClasses(cssText) {
  const set = new Set();
  for (const m of cssText.matchAll(/\.(-?[A-Za-z_][A-Za-z0-9_-]*)/g)) {
    const before = cssText[m.index - 1] ?? '';
    if (/[0-9]/.test(before)) continue;
    set.add(m[1]);
  }
  return set;
}

test('emitted classes are styled or registered with a reason', () => {
  const defined = cssDefinedClasses(css);
  assert.ok(defined.size > 100, 'styles.css class extraction is blind');
  const emitted = emittedClasses(appJs, pureJs, html);
  assert.ok(emitted.size > 100, 'emitted-class extraction is blind');
  const unregistered = [...emitted]
    .filter((c) => !defined.has(c) && !Object.hasOwn(CLASS_EXEMPT, c))
    .sort();
  assert.deepEqual(
    unregistered,
    [],
    `unregistered class(es) ${unregistered.join(', ')} — style them via an existing pattern (AGENTS.md 模式注册表) or register them in CLASS_EXEMPT with a reason`,
  );
  const dead = Object.keys(CLASS_EXEMPT).filter((c) => !emitted.has(c)).sort();
  assert.deepEqual(
    dead,
    [],
    `CLASS_EXEMPT entries no longer emitted — remove them: ${dead.join(', ')}`,
  );
});

// --- gate 2: hard-coded colors stay in their zones ------------------------

test('hard-coded colors stay inside :root / CodeMirror / allowlist', () => {
  const stripped = stripCssComments(css);
  const zones = rootBlockRanges(stripped);
  assert.ok(zones.length >= 2, 'expected the light + dark :root blocks');
  // CodeMirror vendor-override section: raw-text marker, offsets preserved
  // by the comment stripper, so index math stays valid on stripped text.
  const cmStart = css.indexOf('/* ---------- CodeMirror');
  const cmEnd = cmStart >= 0 ? css.indexOf('\n/* ----------', cmStart + 10) : -1;
  const inZone = (i) =>
    zones.some(([a, b]) => i >= a && i < b) ||
    (cmStart >= 0 && i >= cmStart && i < cmEnd);

  // Historical literals: the solid button grade (#fff ink over an
  // accent×#000 fill, #1c1200 primary ink) and two scrim shadows. A new
  // hard-coded color must become a :root variable instead; growing this
  // list needs a written reason like the ones above.
  const ALLOWED = new Set([
    '#fff', '#000', '#1c1200',
    'rgba(8,12,20,.55)', 'rgba(16,24,40,.10)', 'rgba(0,0,0,.5)',
  ]);

  const offenders = [];
  const colorRe = /#[0-9a-fA-F]{3,8}(?![\w-])|rgba?\([^)]*\)/g;
  for (const m of stripped.matchAll(colorRe)) {
    if (inZone(m.index)) continue;
    if (ALLOWED.has(m[0])) continue;
    const line = stripped.slice(0, m.index).split('\n').length;
    offenders.push(`line ${line}: ${m[0]}`);
  }
  assert.deepEqual(
    offenders,
    [],
    `styles.css hard-codes color(s) outside :root — add a :root variable: ${offenders.join('; ')}`,
  );
});

// --- gate 3: #id selectors are structural anchors only --------------------

test('#id selectors are page-host structural anchors only', () => {
  // Page-host anchors: table geometry, sticky hosts, per-page overrides.
  // Component styling must ride classes — an #id rule cannot be reused and
  // is exactly where one-off implementations breed.
  const ANCHORS = new Set([
    'tab-config', 'tab-security', 'sec-kpis', 'sec-rules', 'sec-rules-body',
    'req-table', 'live-table', 'live-session-panel', 'confirm-challenge',
    'req-session', 'req-provider', 'req-model', 'acc-msg',
  ]);
  const stripped = stripCssComments(css).replace(/url\([^)]*\)/g, (m) => m.replace(/[^\n]/g, ' '));
  const isHex = (t) => /^[0-9a-fA-F]{3}$/.test(t) || /^[0-9a-fA-F]{6}$/.test(t) || /^[0-9a-fA-F]{8}$/.test(t);
  const offenders = [];
  for (const m of stripped.matchAll(/#([A-Za-z_][A-Za-z0-9_-]*)/g)) {
    if (isHex(m[1]) || ANCHORS.has(m[1])) continue;
    const line = stripped.slice(0, m.index).split('\n').length;
    offenders.push(`line ${line}: #${m[1]}`);
  }
  assert.deepEqual(
    offenders,
    [],
    `new #id selector(s) — style the component with a class instead: ${offenders.join('; ')}`,
  );
  // Dead anchors rot silently: every anchor must still be selected.
  const live = new Set(
    [...stripped.matchAll(/#([A-Za-z_][A-Za-z0-9_-]*)/g)].map((m) => m[1]).filter((t) => !isHex(t)),
  );
  const dead = [...ANCHORS].filter((a) => !live.has(a)).sort();
  assert.deepEqual(dead, [], `ANCHORS entries no longer selected in styles.css: ${dead.join(', ')}`);
});

// --- gate 4: the AGENTS.md registry stays in lockstep ---------------------

test('AGENTS.md pattern registry stays in lockstep with the assets', () => {
  const start = agentsMd.indexOf('## 模式注册表');
  assert.ok(start >= 0, 'AGENTS.md has no 模式注册表 section');
  const end = agentsMd.indexOf('\n## ', start + 1);
  const section = agentsMd.slice(start, end < 0 ? undefined : end);
  const rows = section.split('\n').filter((l) => l.startsWith('|') && l.includes('`'));
  assert.ok(rows.length >= 10, 'pattern registry table is missing or empty');
  const corpus = css + appJs + pureJs + html;
  const missing = new Set();
  for (const row of rows) {
    for (const m of row.matchAll(/`([^`]+)`/g)) {
      const sym = m[1].replace(/^<|>$/g, '').trim();
      if (sym && !corpus.includes(sym)) missing.add(sym);
    }
  }
  assert.deepEqual(
    [...missing].sort(),
    [],
    `registry symbol(s) not found in assets — update the AGENTS.md 模式注册表 when implementations move: ${[...missing].join(', ')}`,
  );
});
