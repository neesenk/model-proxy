# AGENTS.md - Web UI 规则

修改本目录前读取 `../../../../docs/web-api.md`。API shape、状态字段和 mutation 语义以该文档为准；页面/组件实现细节（tick 盘点、请求表、Security 数据面、explain、会话视图与时间线、chat 详情、URL hash、窄屏断点）以 `docs/frontend.md` 为权威定义，本文件不复制。

## 设计系统（v2）

当前 UI 是 v2 设计系统（历史上曾与 v1 在 `/v2/` 并行共存，现已原位替换、v1 已删除；`/v2/` 仅 301 到 `/ui/`）。`styles.css` 文件头注释是设计契约：rem 字阶（根字号是唯一字号旋钮）、圆角分级、海拔分层、选中态统一为指示条/凸起滑块（实心琥珀只留给 `.btn.primary`）、状态一律 badge、颜色只编码语义。

- 纯函数单一事实源在 `pure.js`（含 v2 展示辅助：iconPin/iconRefresh/iconChevron/statusBadge*/kpiDeltaClass/logLineHTML），全部归 `jstests/pure.test.mjs` 行为覆盖；新增纯逻辑先进 pure.js 并补用例。
- `node --check` 语法门禁与 `jstests/contract.test.mjs`（对 `docs/web-api.md` 的字段契约）覆盖 `app.js`/`pure.js`；样式漂移由 `jstests/registry.test.mjs` 门禁保护（见「模式注册表」）。

## 模式注册表（单一实现目录）

相似 UI 功能不得出现第二份实现。新 UI 元素的决策路由：① 翻 `styles.css` 的
`/* ---------- 区段 ---------- */` 注释找相近模式 → ② 查下表归类、消费其唯一实现 →
③ 查 `pure.js` 导出；仍找不到才允许新建。新建必须同 commit 完成：实现进唯一事实源
（纯逻辑 pure.js / DOM 装配 app.js / 样式 styles.css 对应区段）、登记下表、
`jstests/registry.test.mjs` 的门禁按需加条目。发现第二份实现时合并回唯一实现，不并存。

| 模式 | 唯一实现 | 已有用例 |
|---|---|---|
| 卡片面板 | `.card`/`.card-head`/`.card-body`，app.js `buildCard` | 所有 tab 的卡 |
| 状态徽章 | `.badge` + pure.js `statusBadge` | 请求 status / guard verdict / pills |
| KPI 瓦片 | `.kpi-grid`/`.kpi` | Dashboard / Analytics / Security |
| 数据表 | `.table`（sticky th 用 `--sticky-top`） | 全部列表 |
| 请求表（三表一份） | pure.js `requestTableHeadHTML`+`requestRowHTML` | Requests / Live 环 / Live 会话 |
| 表格溢出 tooltip | app.js `reqTableCellTip`（仅真实截断时动态 `title`，`data-tip-dyn` 标记区分渲染器 title；三表单行省略样式在 styles.css 请求表区段，status 单元除外——badge 芯片不省略） | jstests/uie2e.test.mjs（长 agent 悬停出全文，renderer title 不被覆盖，status 芯片无 “200…” 假截断） |
| 请求详情 | app.js `detailRecordsHTML`（`requestMetaHTML`/`guardMarksDetailHTML`/`chatViewHTML`） | 行内展开 / Live 弹层 |
| guard 徽标 | pure.js `guardMarksHTML` | 请求表 model 单元 |
| 会话视图 | app.js `sessionViewHTML`+`wireSessionTimeline`，pure.js `sessionHealthSummary` | Live 会话 / Requests 汇总 |
| 时间线 tooltip | app.js `showTlTip`/`hideTlTip`，pure.js `sessionBarSummary` | 会话时间线 |
| 大 body | app.js `chunkedBodyHTML` | raw body / 大 SSE |
| 弹层 | `data-popup`+`hidden`；body 级走 app.js `comboInstances` | 日历 / combobox / 下拉 |
| 时间维度选择器 | pure.js `tokenRangePickerHTML`（`TOKEN_RANGES` 预设 + 双月日历；tr-* 样式）+ 各 tab wiring（Status tokens 区 / Analytics 工具栏 / Accounts Token usage 区块 / MCP Analytics 子标签）；Quota Window 预设跨 provider 语境复用 `quotaWindowFromSec`，provider 语境（Analytics）用 `quotaWindowForProvider` | jstests/pure.test.mjs（tokenRangePickerHTML 开/合两态 + quotaWindowFromSec/quotaWindowForProvider） |
| 面板内子标签导航 | Status 同款左侧边栏 `.status-layout`/`.status-nav`/`.status-nav-item`（`data-mcp-tab` 驱动） | MCP tab 的 Servers/Routes/Analytics 子标签切换 |
| MCP server/route 详情（行内展开） | app.js `mcpServerDetailHTML`+`mcpRouteDetailHTML`+`mcpToggleDetail`+`mcpRunProbe`（Servers 与 Routes 行点击原地插删详情行，双击守卫；probe 结果与 tools 表住在详情里），pure.js `mcpToolsTableHTML`，样式 `.mcp-row`/`.mcp-open`/`.mcp-detail-row` | jstests/pure.test.mjs（mcpToolsTableHTML 转义/空描述/空列表/markdown 描述）、uie2e.test.mjs（详情族：展开 + 自动 probe + tools 表） |
| Markdown 子集渲染 | pure.js `miniMarkdownHTML`（工具描述等第三方 markdown：标题/段落/列表〔含嵌套〕/代码块/引用/粗斜体/行内码/链接；先转义再只输出自产标签，链接仅 http(s)，单换行 `<br>`），样式 `.md-p`/`.md-h`/`.md-ul`/`.md-ol`/`.md-li`/`.md-code`/`.md-pre`/`.md-quote`/`.md-hr`/`.md-a` | jstests/pure.test.mjs（转义/注入/列表嵌套/代码块/链接协议/换行） |
| 筛选输入清除 | `.clearable`/`.clear-x`，app.js `attachClearable`（契约见 `docs/frontend.md`） | combobox / datalist 筛选输入、带 All 默认项的筛选 select |
| 目录匹配列表 | `details.cat-match` + pure.js `catalogMatchHTML`/`catalogMatchEditorHTML` + app.js `saveCatalogMatch`（契约见 `docs/frontend.md`） | Status Model Catalog 卡 |
| 模态 | `<dialog>`（`.live-detail-pop`） | Live 详情 / confirm |
| 表单控件 | `.field`/`.req-input`/`.route-target-row` | 全部表单与工具行 |
| 按钮 | `.btn`（`.small`/`.primary`/`.danger`） | 全部动作 |
| 状态开关 | `.switch`（原生 checkbox 语义 + appearance:none 轨道/滑块；macOS 风格：蓝=on 中性灰=off；styles.css switch 区段） | Status→Models 模型行 |
| 日志行 | `.log-line` + app.js `bindLogSelection` | Logs 卡 |
| JSON 高亮 | `.code` 容器 + `.j-key`/`.j-str`/`.j-num`/`.j-lit`/`.j-com` token；pure.js `highlightJSON`（json）/`highlightYAML`/`highlightTOML`/`highlightEnv`（`highlightConfig` 分发） | body/JSON 视图、takeover 模板/渲染预览 |
| 刷新门 | app.js `deferAutoRefresh`+`cancelAutoRefreshHold` | 全部 tick/SSE 渲染 |
| tab 保留 | app.js `retainTab` | 全部 tab |
| stale 横幅 | app.js `setRefreshError` + pure.js `staleDataText` | 刷新失败路径 |
| 视图状态 | pure.js `requestsFilterQuery`/`requestsFilterFromQuery`/`mcpSubTabFromHash`/`mcpHash`，app.js `statusHash` | URL hash |

样式漂移门禁（`jstests/registry.test.mjs`，随 `node --test` 与 `go test ./internal/web` 跑）：

- **class 白名单**：app.js/pure.js/index.html 输出的每个静态 class 必须在
  styles.css 有定义，或登记进测试的 `CLASS_EXEMPT`（每条带一句理由）。
  新 class 二选一：按既有模式补样式，或进 EXEMPT 说明为何不需要样式；
  不再输出的 EXEMPT 条目会被测试清退（防死豁免）。
- **色值区**：styles.css 的硬编码颜色只允许出现在 `:root` 块（亮/暗两处）、
  CodeMirror 区段、以及字面白名单（`.btn` 实心档的 `#fff`/`#000`/`#1c1200`
  与两处 scrim 阴影）；新颜色必须进 `:root` 变量（设计契约见文件头）。
- **ID 锚点**：styles.css 的 `#id` 选择器只允许既有的页面宿主/结构锚点
  （表格几何、sticky 宿主、单页覆盖），组件样式一律 class——`#id` 规则无法
  复用，正是一次性实现滋生的位置。
- **文档同步**：上表反引号符号由测试核对确实存在于资产中——实现改名时
  注册表必须跟着改，否则测试红。

## 边界

- UI 展示后端返回的状态，不在前端重新推导熔断、quota 或 schedule 结论。
- 所有 mutation 成功后刷新对应视图；错误必须显示后端 message，不能静默吞掉。
- SSE 订阅断开后清理 listener/timer，重连不得重复绑定。
- 日志、Raw YAML 和大 JSON 保持页面可滚动，不能通过压缩容器隐藏内容。
- 敏感 request/response body 只在 detail 视图按需加载，列表只使用 metadata。

## 自动刷新与用户交互门（框架特性）

- 任何 timer/SSE 驱动的局部重渲染必须经过 `deferAutoRefresh`（入口/commit 两处都要）：
  面板内有弹层（`[data-popup]` 且非 `hidden`）、焦点在可编辑控件（input/select/textarea/
  contenteditable/combobox —— 原生 `<select>` 或 datalist 弹层打开时控件持焦点，同一机制覆盖）、
  或存在文本选区时，刷新延后，交互结束约 400ms 后由 hold watcher 补一次新刷新。
- 新增弹层（popover/日历/下拉菜单）一律加 `data-popup` 属性 + 关闭时用 `hidden` 属性；
  挂在 `document.body` 的浮层走 `comboInstances` 注册（见 combobox）。
- 不得在 `document.activeElement === select` 时重建该 `<select>` 的 `<option>`（会关掉
  OS 绘制的下拉框）；延后并在 blur 时重试。
- 重渲染会折叠用户展开的 `<details>` 时必须先快照后恢复（见 renderLogsInto 的
  `logsOpenDetails`）。
- 用户主动触发的渲染（筛选点击、mutation、切 section）绕过门：它们自己会先关闭弹层。
- 新增 timer/SSE tick 必须复用 `deferAutoRefresh` 双门 + `cancelAutoRefreshHold` 停止钩子（仅写 topbar chrome 文本的 tick 才允许豁免），并在 `jstests/autorefresh.test.mjs` 加钉；现有 tick 盘点见 `docs/frontend.md`。
- **可点击表格行的双击守卫**：rule 行、blocked 行与 Activity feed 行（点击 toggle analyze 展开）的 `onclick` 必须 `if (e.detail > 1) return`——双击的第二击是文字选择手势的一半，再跑一次动作（toggle/下钻）既破坏选择又白付一次全表重渲染；feed 行的 detail 列带可复制的 LLM verdict 原文，该守卫是复制体验的一部分。rule 过滤点击路径（`setSecurityRule`）**不得重建 leaderboard 表**（只 `syncRuleSel` 原地翻 class）：两击之间替换行节点会打断浏览器的双击计数，文字永远选不中；leaderboard 的数据重建只在 loader（loadSecurity/loadSecurityAdjudications/loadSecurityRules，数据真变了）里做，`renderSecurityFeed` 尾部只做 sel 同步。
- **Security loader 的渲染 commit 必须过交互门的 commit-time 检查点**（框架双门的第二门，见 deferAutoRefresh 注释；Status 页的同款）：四个 loader（loadSecurity/loadSecurityAdjudications/loadSecurityBlocks/loadSecurityRules）fetch 落地后**不得直接重建面板**——切回 Security 必然触发这些 fetch，用户双击落在飞行窗口内时无条件 commit 会吃掉选区并卡顿（"切回来一双击就卡"的确定路径）。统一走 `commitSecurityRender()`：无交互立即 `securityRenderAll()`（KPI+feed+leaderboard+blocks 从已存数据整面重绘——parked fire 会被后落的 loader 替换，任何半边的提交都不得丢失，所以 commit 必须是全量单函数）；有选区/焦点时 park 到 hold watcher，手势结束约 400ms 后落地。同步用户动作（过滤点击、verdict select、hash 恢复）仍直接调 renderer——用户发起的渲染按契约绕过门。
- **feed 表数据未变时必须跳过重建**（`securityFeedFingerprint`，同 Logs 卡 logsPrevKey 惯用法）：commit 门只能看到"已存在"的选区，fetch 落在双击两下之间时门看不到、却照样整表 innerHTML 换血。因此 `renderSecurityFeed` 对"决定 markup 的一切输入"（过滤后的 rows、hints、limit/show-more 可见性）做指纹，指纹未变且表已在屏上就完全不写 DOM——增量状态（analyze 展开、用户选区）随原 DOM 存活；渲染非表格态（audit off、无匹配）时指纹清零。任何新增影响 feed markup 的输入（新列、新徽章、新 hint 源）都必须进指纹，否则会出现"数据变了表不刷"。
- **切 tab 不得闪骨架屏**：每个 tab 的骨架/loading 只在首次激活构建；再次进入保留已渲染 DOM，原地刷新数据（stale-while-revalidate，与 Status cache 同策略）。重入守卫用共享的 `retainTab(panel, marker, refresh)`，行内 session 链接点击拦截用 `sessionLinkClick`（app.js）。表格类刷新（loadRequests/loadSecurity）fetch 期间保留旧表，仅空表才显示 loading 提示。
- **切 tab 的几何稳定**：文档滚动跨 tab 共享（面板用 display 切换），所以每 tab 记住自己的 scrollY（`tabScrollMemory`，存/取都在 `showTabPanel`），不让浏览器把 scrollY 钳到新面板高度；Live 环**跨重挂载保留已完成行**（in-flight 行丢弃——断连期间错过 end 事件，那些行会永远挂着进行中态），保留环在挂载同一 task 内同步回放，session 选择重入时恢复，空环由 CSS min-height 兜底——切进 Status/Live 不许出现「旧表 → connecting 短桩 → 逐行长回」的三段跳。入场淡入只允许纯 opacity 动画（不动 layout，避开 sticky 陷阱），`prefers-reduced-motion` 下关闭。钉在 `assets_test.go` 的 `TestWebAssetsTabSwitchStabilityContract`。
- **后台刷新失败不得覆盖旧数据**：每个部分独立 settle，失败部分保留上次成功值
  （绝不写成空数组/空骨架），通过 `setRefreshError`（`.refresh-err` 横幅，文案用
  pure.js `staleDataText`）提示，下一次成功清除横幅；只有首次加载（无任何数据）才
  允许整页错误卡片。失败路径必须重新武装自动刷新 timer。
- 各页面/组件的实现级行为契约（请求表三合一与虚拟滚动、Security 数据面合并、行内 analyze、会话视图与时间线、chat 详情视图、URL hash、窄屏断点、tick 盘点）见 `docs/frontend.md`；新增页面级行为归该文件。

## 契约变更

新增或修改 `/api/*` 字段时先更新 `docs/web-api.md`，再更新前端。前端不得依赖未文档化字段。

## 样式约定

- 文案大小写规范：**控件标签、列头、卡标题、KPI 键一律 Title Case**（Auto/Minute/By Model/Cost Share/Test All/All Agents…）；豁免：数据式 chip/tooltip 行（`243 requests`、`p50 4.2s`）、单位（`ms`）、镜像 YAML 键的表单/摘要标识（`openai_base_url`、`listen`）、句子型提示（首字母大写即可）。API 值（granularity id、by 值、过滤器值）不受展示层影响，仍为小写。
- 颜色一律引用 `:root` 的 CSS 变量（`--surface-2`、`--border`、`--text`、`--muted`、`--accent` 等），禁止写死色值——dark 主题靠 `prefers-color-scheme` 重定义同一组变量成立，写死色值会在深色下破损。
- 字号一律 rem（根字号 `html { font-size }` 是全局缩放旋钮）；间距/固定 chrome 高度用 px。
- 表单控件必须套用既有的控件样式组之一：整宽表单用 `.field`（input/textarea/select），工具行内联控件用 `.req-input`，路由编辑行用 `.route-target-row`。**裸 `<input>`/`<select>` 会渲染成浏览器原生样式，与主题不符**——新增控件时先归组，不要写一次性 ID 样式。
- 控件视觉语言统一为：`var(--surface-2)` 底 + `1px solid var(--border)` + `var(--r-control)` 圆角，focus 态 `outline: 2px solid color-mix(in srgb, var(--accent) 45%, transparent)`；新控件样式沿用这组值。
- 原生控件优先保留语义：能用 `accent-color`（checkbox/radio）就不用 `appearance: none` 全自定义；确需自定义时必须补全勾选标记、focus 环、disabled 态。

## 验证

```bash
node --check internal/web/assets/app.js internal/web/assets/pure.js
node --test internal/web/jstests/*.test.mjs
go test ./internal/web -count=1
```

jstests glob 包含浏览器 e2e（`uie2e.test.mjs` 行为流 + `uivisual.test.mjs` 视觉检查），
但它们是**按需门禁**：默认快速 Skip，不进每次修改的全量；UI 改动提交前用
`MP_UI_E2E=1 node --test internal/web/jstests/uie2e.test.mjs internal/web/jstests/uivisual.test.mjs`
运行（无浏览器 Skip，加 `MP_REQUIRE_UI_E2E=1` 升级为 FAIL；契约与实现陷阱见
`docs/engineering/testing.md`「UI 浏览器 e2e」）。

结构自查（新增 UI 元素时）：模式注册表是否已归类？相似功能（表格/徽章/弹层/tick）是否复用唯一实现？新 class 是否有样式定义或 `CLASS_EXEMPT` 理由？

`pure.js` 只收零 DOM 依赖的纯函数（esc、格式化、YAML 高度计算等），
`app.js` 从 `./pure.js` import；新增纯逻辑先进 pure.js 并在
`jstests/pure.test.mjs` 加行为用例，locale/时区相关的渲染留在 app.js。

布局改动还需检查窄窗口、短窗口、长日志、长 YAML 和 hover/selection 状态。
