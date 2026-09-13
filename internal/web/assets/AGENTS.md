# AGENTS.md - Web UI 规则

修改本目录前读取 `../../../../docs/web-api.md`。API shape、状态字段和 mutation 语义以该文档为准。

## 设计系统（v2）

当前 UI 是 v2 设计系统（历史上曾与 v1 在 `/v2/` 并行共存，现已原位替换、v1 已删除；`/v2/` 仅 301 到 `/ui/`）。`styles.css` 文件头注释是设计契约：rem 字阶（根字号是唯一字号旋钮）、圆角分级、海拔分层、选中态统一为指示条/凸起滑块（实心琥珀只留给 `.btn.primary`）、状态一律 badge、颜色只编码语义。

- 纯函数单一事实源在 `pure.js`（含 v2 展示辅助：iconPin/iconRefresh/iconChevron/statusBadge*/kpiDeltaClass/logLineHTML），全部归 `jstests/pure.test.mjs` 行为覆盖；新增纯逻辑先进 pure.js 并补用例。
- `node --check` 语法门禁与 `docs/frontend` 契约测试（assets_test.go）覆盖 `app.js`/`pure.js`。

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
- 定时 tick 盘点：Status 5s（整页重渲染，双门）、Analytics 30s（仅 live 窗口）、Security 30s（loader 只重绘数据宿主、不重建工具栏）、Accounts 30s（后台失败走 stale 横幅；**操作中守卫**——pane 内有 disabled 按钮时跳过，避免打断 Test/测活/Test All 的进行态）。新增 tick 必须复用 `deferAutoRefresh` 双门 + `cancelAutoRefreshHold` 停止钩子，并在 `jstests/autorefresh.test.mjs` 加钉。
- **表内 tokens 单元的 cache read 带命中占比（≥80% `.tok-cache.hot` ok 色）；详情 hint 行带 bytes 与相对时间（T+/Δ，`requestRelTimeOpts` 从已加载列表按时间序推导，弹层无邻居时只显绝对时间）。三张请求表是单一份实现**（pure.js `requestTableHeadHTML` + `requestRowHTML`）：Requests 标签页、Live 全量环、Live 会话视图共用同一列集（time · agent · session · status · model · provider · ms · tokens in/out + cache read），Live 只是数据源换成实时行；Requests 记录经 `persistedSummaryRow` 投影成合并行形态后进同一渲染器——改列/徽章/单元格语义只改这一处，三表同步生效。bytes 列已移除（token 单元是有效信号）。
- **Security 页的数据面是两个异构源的合并**：审计半边（/api/security，30d 持久）携带服务端过滤 kind/from/limit（kind/range/limit 由控件驱动，"show more" 逐级加深到 1000 上限，换窗口重置），AI 半边（/api/security/adjudications，内存 ring 256、重启清零）只做客户端过滤。合并唯一发生在 pure.js `mergeSecurityFeed`：fresh（未命中缓存）verdict 同时落审计记录与 ring，按 request_id+kind+verdict+rule 折叠进审计行（judge model/cached/sessionId 随行）；ring-only 行（缓存命中、重启残留）保留 ai· 行。KPI verdict 计数源自合并流而非 ring（否则重启后 tiles 归零与下方 feed 矛盾）；llm 用量 tiles 只在判定通道开启或有历史用量时渲染，否则显示单个 off tile。三个 loader（audit/adjudications/blocks）刷新失败保留旧数据并经 `securityRefreshOk/Fail` 聚合进 `setRefreshError` 横幅，仅首次加载（无任何数据）允许内联错误；响应级 hint（skipped 行数、guard.audit 关闭说明）存模块状态由 `renderSecurityFeed` 统一渲染，不得 insertAdjacentHTML 直插（会被下一次 innerHTML 重建抹掉）。Rule hits 行可点击下钻（设 rule 过滤 + 可移除 chip）；Blocked sessions 行的 session 单元是 session-link，经 `#requests?session=…` hash 下钻跳转（同 Analytics 表格的 data-drill 惯例）。
- **会话视图是单一份实现**（`sessionViewHTML` + `wireSessionTimeline`：汇总 chips + Trace 时间线）：Live 会话面板与 Requests 会话汇总共用，改动一处两页同时生效；两页只各自拥有表格本体与详情打开方式（Live 弹层 / Requests 行内展开 + `scrollIntoView` 定位闪烁），新会话级展示一律进共享视图而不是某页私有。健康度 chips（第二行）全在 pure.js `sessionHealthSummary` 一处计算（跨度/活跃——空闲 >2min 不计、p50/p95、ttft p50、failovers、缓存命中率、tok/s、模型分布、shadow），providers/models 合并单行 hint；TTFT 源自记录的 `ttft_ms`（代理首读已提交响应体），live 事件不带该字段、由持久化行回填。
- **时间线的几何全在 pure.js `sessionTimeline`**（app.js 只做 DOM 装配）：时间轴按空闲间隙分段（默认 2 分钟无请求即断轴，`gapMs` 可覆盖），分段宽度 ∝ √时长并保底 24px——否则长会话里的短突发会被压成 2% 宽的栅栏；泳道数 >14 时压缩行高（20→12px）防止卡片高过表格。拖拽缩放：分段映射（时间↔viewBox px）以 `data-segs` JSON 挂在 SVG 上由 app.js 反演，zoom 状态存模块级 `sessionZoomState`（按 session id 键控，跨 Live SSE 重渲染存活；窗口扑空时回落全量视图，绝不渲染空卡丢掉 reset 入口）。会话视图容器是 `.sess-sticky`（topbar 下悬浮固定，z-index 6 盖过 sticky 表头），≤720px 必须关闭（topbar 换行变高 + `.card-body` overflow-x 祖先都会破坏 sticky）。**sticky 的最大杀手是 overflow 祖先**：`.card` 自带 `overflow: hidden`（圆角裁切），会让永不滚动的卡片成为 sticky 的滚动容器、悬浮完全失效——宿主卡（Requests / Live）必须挂 `card-open`（`.card.card-open { overflow: visible }`）豁免，且只有无 flush 贴边内容的卡才能豁免；sticky 元素**必须负 margin 铺满卡片宽度并与顶栏/彼此 flush**（`.sess-sticky` 的 `margin: 0 -16px`、`top: var(--topbar-h)`、`--sess-h` 无缝隙补偿）——任何透明缝隙都会让滚过的行透出来。**表头跟随悬浮视图**：sticky 偏移看不到兄弟高度，`syncSessThOffset` 在每次会话视图渲染/隐藏和 window resize 后实测面板高度、以 `--sess-h` 注入宿主卡，表头 `top: calc(var(--sticky-top) + var(--sess-h, 0px))` 钉在悬浮视图之下；新增改面板高度的逻辑后必须重跑该测量。**条形悬停摘要在共享 tooltip**（app.js `showTlTip`/`hideTlTip`，两页共用）：元数据行来自 pure.js `sessionBarSummary`，内容摘录**两段、无文字标签**——这轮用户输入来自 `requestExcerpt`（请求体最后一条带文本的 user 消息，tool_result-only 消息跳过、向前回溯），模型返回来自 `responseExcerpt`（anthropic/openai/responses + SSE 拼接、跳过 thinking 文本；无文本时回退标记——纯工具调用轮 `[tool_use: 名字…]`、纯思考轮 `[thinking]`、错误 JSON 直接显示 error.message）；两段均 `/api/requests/<id>` 懒拉、bounded 缓存、与详情行共享 `cacheRequestDetail`，样式用 muted vs 正常文本区分（`.tl-tip-in`/`.tl-tip-out`）；它**有意不带 `data-popup`**——auto-refresh 门会因此推迟重渲染，而悬停提示绝不能拖住 Live SSE 重渲染（重渲染方自己调 `hideTlTip` 收拾它），且必须 `pointer-events: none` 防止吞掉指针。bar 的 `<title>` 已移除（避免与 tooltip 双弹），无障碍语义走 `aria-label`。
- **请求详情默认是人读对话视图**（pure.js `chatViewHTML`，经 `detailRecordsHTML` 同时服务 Requests 行内展开与 Live 弹层）：消息块解析覆盖 anthropic/openai/responses 与 SSE 流折叠（按 index 重组 text/thinking/tool_use 增量、usage 并入）；system、thinking、tool_result、超长参数一律 `<details>` 折叠，请求侧只展开最近 4 轮（`CHAT_RECENT`）其余收进 "N earlier turns" 且**懒加载**——折叠态只是占位符（初始 DOM 与轮数无关）；展开时 `parseChatRequest` **只解析一次**（messages 缓存在 registry entry 上），完整历史平铺渲染进 `.cv-hist-scroll` 滚动容器、复用 `bodyChunkRegistry` 每 25 轮一块滚动追加（追加经 rAF 合帧，不在 scroll 事件里同步插 DOM；`.cv-hist-scroll .cv-msg` 挂 `content-visibility: auto` 原生虚拟化，滚动成本与已加载深度无关）——不要退回"逐轮折叠/逐轮点击"的形态（用户明确要求平铺直读），也不要在展开路径重复解析大 body；工具参数与 JSON 工具结果经 `readableValue` 渲染为 `key: value` 可读文本（嵌套缩进、小标量数组逗号连接），不用 JSON 语法；**原始 body 永远保留在下方折叠 `<details>` 且懒渲染**——body 文本挂 `rawBodyRegistry`（绝不进 data 属性），document 级 capture `toggle` 监听首次展开时才调 `capturedBodyView`，程序化恢复 open 同样触发；teardown 与 `[data-chunk]` 同站清理（`dropRawBodies`，选择器是 `[data-raw]`，聊天历史折叠与 raw body 共用同一注册表）；超限 raw body 与大 SSE（`bodyLinesHTML` 行数或总量超限）一律走 `chunkedBodyHTML` 64KB 滚动分块，绝不整段塞单个 `<pre>`；单块文本 4k/参数 2k 截断、>1.5MB 不解析直接回落 raw-only；`fillLiveDetailPop` 不再默认展开任何 details。新增协议形态先扩 `contentToBlocks`/`foldSSEBlocks` 并配 jstest。
- **视图状态进 URL hash**（tab / Accounts provider / Status section / Requests 过滤器 / Security 过滤器 / Live 会话选择，见 app.js 顶部路由注释）：requests 过滤器只携带非默认值（pure.js `requestsFilterQuery/FromQuery`），变更走 `replaceState` 不刷历史；hashchange 还原时**先种过滤器再激活 tab**，且自由输入控件（provider/model/errors/shadow）必须跟随 `syncRequestsFreeControls`，否则下次 Refresh 会把旧值读回过滤器。Live 会话选择 rides `#status/live?session=…`（`statusHash()`）：**写 hash 必须在渲染之后**（挂载 Live 卡会重置选择，hash 要反映渲染后真值）；hashchange/boot 还原要在 section 挂载之后 apply——boot 特别不能直接 apply（Status tab 是异步渲染，直接 apply 会与 Live 卡挂载竞态、挂载重置把选择冲掉），必须存 `bootLiveSession` pending、由 `renderLiveCard` 挂载后消费；下拉选项构建要保底保留当前选中 id（hash 还原的会话可能暂不在 live 行/聚合里）。
- **切 tab 不得闪骨架屏**：每个 tab 的骨架/loading 只在首次激活构建；再次进入保留已渲染 DOM，原地刷新数据（stale-while-revalidate，与 Status cache 同策略）。重入守卫用共享的 `retainTab(panel, marker, refresh)`，行内 session 链接点击拦截用 `sessionLinkClick`（app.js）。表格类刷新（loadRequests/loadSecurity）fetch 期间保留旧表，仅空表才显示 loading 提示。
- **窄屏是系统断点而非逐案修补**：≤720px（topbar 两行 + tabs 横向滚动、status/accounts 单列、表头停 sticky、`.card-body` 横向滚动、Dashboard KPI 6→2 列）与 ≤560px（`.row-actions` 换行、更紧的页面 gutter）两层，见 styles.css 的 responsive 段；新增布局必须说明这两个断点下的行为（表格靠 `.card-body` 横向滚动，不隐藏列）。uPlot 图表随窗口 resize 由防抖钩子重设宽度（app.js `chartResizeTimer`）。
- **后台刷新失败不得覆盖旧数据**：每个部分独立 settle，失败部分保留上次成功值
  （绝不写成空数组/空骨架），通过 `setRefreshError`（`.refresh-err` 横幅，文案用
  pure.js `staleDataText`）提示，下一次成功清除横幅；只有首次加载（无任何数据）才
  允许整页错误卡片。失败路径必须重新武装自动刷新 timer。

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
node --test internal/web/jstests/pure.test.mjs
go test ./internal/web -count=1
```

`pure.js` 只收零 DOM 依赖的纯函数（esc、格式化、YAML 高度计算等），
`app.js` 从 `./pure.js` import；新增纯逻辑先进 pure.js 并在
`jstests/pure.test.mjs` 加行为用例，locale/时区相关的渲染留在 app.js。

布局改动还需检查窄窗口、短窗口、长日志、长 YAML 和 hover/selection 状态。
