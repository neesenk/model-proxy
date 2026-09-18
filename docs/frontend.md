# Web 前端页面与组件实现契约

目录级硬规则（设计系统、模式注册表、交互门框架、边界、样式约定、验证）见
`internal/web/assets/AGENTS.md`；API shape、状态字段与 mutation 语义归
`docs/web-api.md`。本文件承载各页面/组件的实现级行为契约——修改对应页面前
阅读相关小节；新增页面级行为归入本文件，不要回灌目录级 AGENTS.md。

## 自动刷新 tick 盘点

Status 5s（整页重渲染，双门）、Analytics 30s（仅 live 窗口）、Security 30s（loader 只重绘数据宿主、不重建工具栏）、Accounts 30s（后台失败走 stale 横幅；**操作中守卫**——pane 内有 disabled 按钮时跳过，避免打断 Test/测活/Test All 的进行态）、顶栏 header 5s（`maybeConnRefresh`：仅 dot+meta 文本经 `setConn` 单点写入、不重建面板 DOM → **门豁免**（钉在 jstests/autorefresh.test.mjs）；Status tab 激活时跳过避免重复 /api/status）。新增 tick 的强制规则（双门 + 停止钩子 + autorefresh.test.mjs 加钉）见 `internal/web/assets/AGENTS.md` 交互门一节。

## 请求表（三合一渲染、列几何、虚拟滚动）

**表内 tokens 单元的数字用 `fmtCompactNum` 压缩（<1K 原样、<10K 一位小数 K、<1M 整数 K、≥1M 一位小数 M；精确值进单元 title tooltip），cache read 带命中占比（两位小数、去尾零；≥80% `.tok-cache.hot` ok 色）；详情结构是三区分层：labeled meta strip（pure.js `requestMetaHTML`：when/call/result/route/size 五组小写标签 + mono 值，status 用彩色 badge，相对时间 T+/Δ 由 `requestRelTimeOpts` 从已加载列表按时间序推导、弹层无邻居时只显绝对时间）→ guard 轨迹（`guardMarksDetailHTML`，仅在有标注时渲染，挂在首条记录 meta 之后：与 meta strip 同一扁平语法——单个 `⚑ guard` 标签组，每条一行 verdict 徽章引导 + rule code + 归属〔judge/cached/action/kind〕+ 右缘时间，reason 单行省略号、全文〔reason+evidence+detail〕进 title tooltip；不嵌套卡片）→ 对话/原始 body（`chatViewHTML` + 折叠 raw bodies）；model 单元的 guard 徽标（`guardMarksHTML`，≤3 个 + 溢出计数：`⚑ block`/`judge·verdict`/`unblocked`，title 载 scrub 后 reason/溯源）。三张请求表是单一份实现**（pure.js `requestTableHeadHTML` + `requestRowHTML`）：Requests 标签页、Live 全量环、Live 会话视图共用同一列集（time · agent · session · status · model · provider · ms · tokens in/out + cache read），Live 只是数据源换成实时行；Requests 记录经 `persistedSummaryRow` 投影成合并行形态后进同一渲染器（`guard` 标注随行——服务端在 `/api/requests` summary 与 `/api/requests/<id>` 的 `guard` 键按 request_id join 安全审计轨迹，**前端不做第二次推导**；详情卡经 `detailRecordsHTML` 的 `opts.guard` 注入，弹层/live 详情不传时只渲染 meta strip）；改列/徽章/单元格语义只改这一处，三表同步生效。**列几何同样固定在这一处**：`requestTableHeadHTML` 输出 colgroup（八列百分比、合计 100%），styles.css 对三张请求表开 `table-layout: fixed`（窄屏 ≤720px 给 720px min-width，靠 `.card-body` 横向滚动而不是挤压列）——Requests 按窗口挂载行、Live 按 SSE 事件重渲染，auto layout 会按在场行重算列宽，滚动时列宽肉眼抖动；改列宽只改 colgroup。bytes 列已移除（token 单元是有效信号）。**Requests 表是虚拟滚动的**（app.js `reqVirt` 一套：`reqReconcile`/`reqFrame`/`reqLoadOlder`）：浏览默认只拉 50 条（session 下钻首拉 500），滚动近底用相同过滤参数 + `to=<已加载最旧秒>` 键集分页拉更旧一页（边界秒重拉、`mergeRecordsPages` 按 id 去重，pure.js；累计 1000 封顶），DOM 只保留视口窗口内的行（spacer 行 `.req-spacer` 撑住滚动几何，测量高度进 offsets、未测行用滑动均值估计）；行节点按 id 池化复用（`reqRowNode`），**展开详情的行钉住不卸载**（详情的分块 body 状态活在 DOM 节点上），时间线跳转经 `reqRowForId` 先挂载（reveal pin 防被窗口移动卸载），再在详情填充后按行实时 rect 瞬时跳转（`behavior:auto`——平滑滚动会被窗口 reconcile 的 replaceChildren 中途取消，rAF 等待在后台标签页挂死，offsets 定位受未测行估算误差累积偏移）；只在展开且行在视口外时滚动，收起或行已可见不动页面（否则会拽动 sticky Trace 卡下的光标位置）；表格重建时 `reqTearDown` 沿池清理 chunk/raw-body 注册表（池化节点可能已脱离容器，不能只扫容器）。滚动监听是 document 级 capture + rAF 合帧（scroll 不冒泡，同 `wireBodyChunks` 惯例）；加载失败保留旧行、hint 行报错并 2.5s 退避重试；整页记录全部落在边界秒（同秒记录数 > pageSize）导致 `mergeRecordsPages` 零新增时同样走该退避，不会无退避热循环重拉同一页。

## Security 页数据面

**Security 页的数据面是两个异构源的合并**：审计半边（/api/security，30d 持久）携带服务端过滤 kind/from/limit（kind/range/limit 由控件驱动，"show more" 逐级加深到 1000 上限，换窗口重置），AI 半边（/api/security/adjudications，内存 ring 256、重启清零）只做客户端过滤。合并唯一发生在 pure.js `mergeSecurityFeed`：fresh（未命中缓存）verdict 同时落审计记录与 ring，按 request_id+kind+verdict+rule 折叠进审计行（judge model/cached/sessionId 随行）；ring-only 行（缓存命中、重启残留）保留 ai· 行；**同请求同 channel（kind）的行再归并为一行**——worst verdict 头条徽章、names 连接、per-rule verdict/理由为行内 segments（`securitySegmentsHTML` 渲染，单段行不渲染 segments）（审计库仍逐规则留痕，explain/repeat 不受影响）。请求行徽标按 (kind, verdict) 计数合并（`groupGuardMarks`，`judge·error ×2`，各规则理由并入 title tooltip）；详情 guard 组同 channel 一行、异构 verdict/理由逐段（`guardMarksDetailHTML`）。KPI verdict 计数**必须**来自 `/api/security` 的服务端 `counts` 字段（SQL 聚合 + 判定服务累计 low 计数）——不得退回客户端数合并流（合并流含 ring 缓存重放行，重启即漂移；pure.js `securityKpisHTML(blocks, counts, stats, on, countsError)` 的第二参就是 counts 对象）；聚合查询失败时响应缺省 `counts` 并带 `counts_error` 固定标记，verdict 三瓦片渲染「—」（描述行载该标记）而非误导性全零，blocked/llm 瓦片不受影响；KPI 渲染走公共瓦片组件（`.kpi-grid`/`.kpi`，Dashboard/Analytics/Security 三页同一设计系统样式，新增 KPI 位不得造私有样式）；**feed 永不渲染 low 行**（`renderSecurityFeed` 在过滤链首位硬过滤 `verdict !== 'low'`：判定已忽略的命中进列表就是噪音；low 只存在于 Rule hits 计数与 JSONL 留痕，**KPI strip 也不渲染 low**——suppressed 噪音不进摘要位），verdict 过滤器枚举因此是 high/medium/error/skipped（无 low 选项；`securityFilterFromQuery` 把 `verdict=low` 当非法值降级为不过滤，stale 书签不产生空列表）。llm 用量 stat 只在判定通道开启或有历史用量时渲染，否则显示单个 off stat。三个 loader（audit/adjudications/blocks）刷新失败保留旧数据并经 `securityRefreshOk/Fail` 聚合进 `setRefreshError` 横幅，仅首次加载（无任何数据）允许内联错误；第四个 loader `loadSecurityRules` 用**固定参数**（无 kind、无 from、limit=1000）拉自己的审计切片喂 Rule hits 排行榜——Activity 的 kind/range 是 feed 查询的服务端过滤，复用该响应会让 feed 过滤器悄悄改掉排行榜计数（收窄 feed 到 path 就从运维视图里抹掉全部 secret 规则），排行榜与 feed 过滤器唯一允许的耦合方向是行点击下钻。**Rule hits 是 Activity 卡内的折叠区**（`#sec-rules` details + `#sec-rules-body` 宿主，位于 legend 与 feed 表之间、其过滤对象正下方）：`renderRuleLeaderboard` 只重写 body div，`<details>` 的开合状态归用户所有、数据刷新不得重置；排行榜为空时整个 section `hidden`。响应级 hint（skipped 行数、guard.audit 关闭说明）存模块状态由 `renderSecurityFeed` 统一渲染，不得 insertAdjacentHTML 直插（会被下一次 innerHTML 重建抹掉）。verdict 枚举是 high/medium/low/error/skipped（medium 徽章 warn 色，explain 判定块带 reason+evidence 两段；low 徽章只出现在 explain 的判定块里——同请求的 ring low 判定仍会在展开视图中显示，feed 列表本身无 low 行）；feed 行 detail 列直显 LLM 的 reason/evidence 原文与精确匹配溯源（detail），不做程序侧转述。带 request_id 的 secret/path 行有 req 下钻链接（`#requests?request=<id>&kind=&name=`）：Requests 页顶部渲染 pinned 卡（`applyRequestDrill`）——explain 高亮定位 + 完整请求详情（复用 `detailRecordsHTML`），独立于列表过滤器；被拦截的 400 请求也落 request log，下钻同样可达。feed 表带 session 列（合并行 `sessionId`，缩写 `shortSessionId`、title 全量）：session 单元是 session-link，经 `#requests?session=…` hash 下钻跳转（与 Blocked 卡同款）。Rule hits 行可点击下钻（设 rule 过滤 + 可移除 chip）；Blocked sessions 行的 session 单元是 session-link，经 `#requests?session=…` hash 下钻跳转（同 Analytics 表格的 data-drill 惯例）。audit kind 枚举含 `unblock`（会话解除留痕行：kind 徽章 ok 色、无 verdict、不可 analyze、不进 Rule hits 计数——`ruleHitsLeaderboard` 硬排除，操作历史不是规则命中率）。

## 行内 analyze（explain）

**行内 analyze（explain）是双半边统一的按需展开，触发是整行点击（无按钮列）**：审计行与 ai·（缓存 verdict）行只要带 request_id 且 kind ∈ {secret,path} 都可 analyze（ai· 行的"为什么"正是 explain 的判定块 + 定位命中），可分析行挂 `.sec-row`（cursor:pointer）。行 `onclick` 的路由顺序：`e.detail > 1`（双击选字）直接 return → session-link 命中走 `#requests?session=…` 下钻 → `e.target.closest('a, button')` 命中（req 下钻链接等）放行导航 → 其余才 toggle analyze；既无 session 也无可分析命中的行（如无头 drift）保持惰性。展开状态存 `securityExpanded`（pure.js `explainCacheKey`：request_id+kind+names，名字序归一；key 绝不进 HTML 属性——分隔符 \u0000 过不了属性解析，会变 U+FFFD 导致恢复失配；行↔数据用 HTML 安全的整数 `data-sec-i` 关联，key 只活在 JS 闭包与 DOM property）。`renderSecurityFeed` 重建表后 `restoreSecurityDetails` 按快照恢复展开；结果与在途 promise 缓存在有界 `securityExplainCache`（32 条，跳过仍展开的 key 驱逐），恢复零 refetch，同 key 并发去重，settle 后统一 `paintSecurityDetail`（fetch 起飞后行被重渲染替换也能落笔）。explain 渲染（pure.js `securityExplainHTML`）：LLM 判定块在前（verdict/rule/cached/model/reason 一行一条），命中按规则名分组——规则身份（名/强度/解释/regex/source）每卡一次，多个 occurrence 编号 #n，片段 `pre-wrap`+`break-all` 折行（宽表格单元不得逼出横向滚动）。

## 会话视图（单一实现）

**会话视图是单一份实现**（`sessionViewHTML` + `wireSessionTimeline`：汇总 chips + Trace 时间线）：Live 会话面板与 Requests 会话汇总共用，改动一处两页同时生效；两页只各自拥有表格本体与详情打开方式（Live 弹层 / Requests 行内展开 + `scrollIntoView` 定位闪烁），新会话级展示一律进共享视图而不是某页私有。健康度 chips（第二行）全在 pure.js `sessionHealthSummary` 一处计算（跨度/活跃——空闲 >2min 不计、p50/p95、ttft p50、failovers、缓存命中率、tok/s、模型分布、shadow），providers/models 合并单行 hint；TTFT 源自记录的 `ttft_ms`（代理首读已提交响应体），live 事件不带该字段、由持久化行回填。

## Trace 时间线几何与 sticky

**时间线的几何全在 pure.js `sessionTimeline`**（app.js 只做 DOM 装配）：时间轴优先按**对话轮次**分段——每条请求携带服务端计算的 `turn_key`（请求体中消息总数 + 最后一条真实 user 文本的指纹）；相邻请求 `turn_key` 都存在且不同即断轴。`turn_key` 缺失的旧记录或无法提取 user 文本的请求回退到原空闲间隙判定（默认 2 分钟无请求即断轴，`gapMs` 可覆盖）。分段宽度 ∝ √时长并保底 24px（保底封顶 plotW/段数、总宽再按比例收进 plotW）——否则长会话里的短突发会被压成 2% 宽的栅栏；段间压缩断口条带随段数增多自动变窄（段数 × 全宽断口会顶出 viewBox 右缘、静默裁掉尾部请求条），保证所有段落始终落在 viewBox 内；泳道数 >14 时压缩行高（20→12px）防止卡片高过表格。拖拽缩放：分段映射（时间↔viewBox px）以 `data-segs` JSON 挂在 SVG 上由 app.js 反演，zoom 状态存模块级 `sessionZoomState`（按 session id 键控，跨 Live SSE 重渲染存活；窗口扑空时回落全量视图，绝不渲染空卡丢掉 reset 入口）。会话视图容器是 `.sess-sticky`（topbar 下悬浮固定，z-index 6 盖过 sticky 表头），≤720px 必须关闭（topbar 换行变高 + `.card-body` overflow-x 祖先都会破坏 sticky）。**sticky 的最大杀手是 overflow 祖先**：`.card` 自带 `overflow: hidden`（圆角裁切），会让永不滚动的卡片成为 sticky 的滚动容器、悬浮完全失效——宿主卡（Requests / Live）必须挂 `card-open`（`.card.card-open { overflow: visible }`）豁免，且只有无 flush 贴边内容的卡才能豁免；sticky 元素**必须负 margin 铺满卡片宽度并与顶栏/彼此 flush**（`.sess-sticky` 的 `margin: 0 -16px`、`top: var(--topbar-h)`、`--sess-h` 无缝隙补偿）——任何透明缝隙都会让滚过的行透出来。**表头跟随悬浮视图**：sticky 偏移看不到兄弟高度，`syncSessThOffset` 在每次会话视图渲染/隐藏和 window resize 后实测面板高度、以 `--sess-h` 注入宿主卡，表头 `top: calc(var(--sticky-top) + var(--sess-h, 0px))` 钉在悬浮视图之下；新增改面板高度的逻辑后必须重跑该测量。**条形悬停摘要在共享 tooltip**（app.js `showTlTip`/`hideTlTip`，两页共用）：元数据行来自 pure.js `sessionBarSummary`，内容摘录**两段、无文字标签**——这轮用户输入来自 `requestExcerpt`（请求体最后一条带文本的 user 消息，tool_result-only 消息跳过、向前回溯），模型返回来自 `responseExcerpt`（anthropic/openai/responses + SSE 拼接、跳过 thinking 文本；无文本时回退标记——纯工具调用轮 `[tool_use: 名字…]`、纯思考轮 `[thinking]`、错误 JSON 直接显示 error.message）；两段均 `/api/requests/<id>` 懒拉、bounded 缓存、与详情行共享 `cacheRequestDetail`，样式用 muted vs 正常文本区分（`.tl-tip-in`/`.tl-tip-out`）；它**有意不带 `data-popup`**——auto-refresh 门会因此推迟重渲染，而悬停提示绝不能拖住 Live SSE 重渲染（重渲染方自己调 `hideTlTip` 收拾它），且必须 `pointer-events: none` 防止吞掉指针。bar 的 `<title>` 已移除（避免与 tooltip 双弹），无障碍语义走 `aria-label`。

## 请求详情对话视图

**请求详情默认是人读对话视图**（pure.js `chatViewHTML`，经 `detailRecordsHTML` 同时服务 Requests 行内展开与 Live 弹层）：消息块解析覆盖 anthropic/openai/responses 与 SSE 流折叠（按 index 重组 text/thinking/tool_use 增量、usage 并入）；system、thinking、tool_result、超长参数一律 `<details>` 折叠，请求侧只展开最近 4 轮（`CHAT_RECENT`）其余收进 "N earlier turns" 且**懒加载**——折叠态只是占位符（初始 DOM 与轮数无关）；展开时 `parseChatRequest` **只解析一次**（messages 缓存在 registry entry 上），完整历史平铺渲染进 `.cv-hist-scroll` 滚动容器、复用 `bodyChunkRegistry` 每 25 轮一块滚动追加（追加经 rAF 合帧，不在 scroll 事件里同步插 DOM；`.cv-hist-scroll .cv-msg` 挂 `content-visibility: auto` 原生虚拟化，滚动成本与已加载深度无关）——不要退回"逐轮折叠/逐轮点击"的形态（用户明确要求平铺直读），也不要在展开路径重复解析大 body；工具参数与 JSON 工具结果经 `readableValue` 渲染为 `key: value` 可读文本（嵌套缩进、小标量数组逗号连接），不用 JSON 语法；**原始 body 永远保留在下方折叠 `<details>` 且懒渲染**——body 文本挂 `rawBodyRegistry`（绝不进 data 属性），document 级 capture `toggle` 监听首次展开时才调 `capturedBodyView`，程序化恢复 open 同样触发；teardown 与 `[data-chunk]` 同站清理（`dropRawBodies`，选择器是 `[data-raw]`，聊天历史折叠与 raw body 共用同一注册表）；超限 raw body 与大 SSE（`bodyLinesHTML` 行数或总量超限）一律走 `chunkedBodyHTML` 64KB 滚动分块，绝不整段塞单个 `<pre>`；单块文本 4k/参数 2k 截断、>1.5MB 不解析直接回落 raw-only；`fillLiveDetailPop` 不再默认展开任何 details。新增协议形态先扩 `contentToBlocks`/`foldSSEBlocks` 并配 jstest。

**请求详情 replay 条**（Requests 页行内展开底部，`replayStripHTML`/`wireReplayStrip`/
`replayRun`）：`replay <id> --to <provider>` 的行内版——provider 输入框带共享 datalist
（选项 = Requests facet 的 providerOptions，每次 loadRequests 后刷新）+ Replay 按钮；
shadow 记录不提供该条（后端同样拒绝，双重一致）。结果就地渲染：status 徽章
（`statusBadgeHTML`）+ 延迟 + 响应体走与请求/响应 body 同一套 `rawBodyRegistry` 懒渲染
折叠 details（4 MiB 截断标记随行），结果注入后必须调 `reqDetailChanged()` 让虚拟滚动
重测行高。Live 弹层与 pinned drill 卡不挂 replay 条（仅 Requests 表行内展开）。

**Schedule 卡 per-route Test**（`testRouteTargets`，`test <model>` 的行内版）：route-head
的 Test 按钮对 `POST /api/routes/test`，结果逐 target 一行（✓/✗ 徽章 + provider(model) +
HTTP 状态 + reason + 延迟）渲染在该 route 块内 `.route-test-result`；**原地更新不重建
Schedule 卡**（整卡重渲染会打断其他 route 进行中的测试态）。

**Models 区 Model Catalog 卡**（`refreshModelsCatalog`，`models pull` 的 Web 版）：
`POST /api/models/catalog/refresh` 强刷 models.dev 缓存，结果（数量/etag）行内展示，
提示下一次 reload/takeover 才消费；失败保留旧缓存并显示后端 message。

## URL hash 视图状态

**视图状态进 URL hash**（tab / Accounts provider / Status section / Requests 过滤器 / Security 过滤器 / Live 会话选择，见 app.js 顶部路由注释）：requests 过滤器只携带非默认值（pure.js `requestsFilterQuery/FromQuery`），变更走 `replaceState` 不刷历史；hashchange 还原时**先种过滤器再激活 tab**，且自由输入控件（provider/model/errors/shadow）必须跟随 `syncRequestsFreeControls`，否则下次 Refresh 会把旧值读回过滤器。Live 会话选择 rides `#status/live?session=…`（`statusHash()`）：**写 hash 必须在渲染之后**（挂载 Live 卡会重置选择，hash 要反映渲染后真值）；hashchange/boot 还原要在 section 挂载之后 apply——boot 特别不能直接 apply（Status tab 是异步渲染，直接 apply 会与 Live 卡挂载竞态、挂载重置把选择冲掉），必须存 `bootLiveSession` pending、由 `renderLiveCard` 挂载后消费；下拉选项构建要保底保留当前选中 id（hash 还原的会话可能暂不在 live 行/聚合里）。

## 窄屏断点

**窄屏是系统断点而非逐案修补**：≤720px（topbar 两行 + tabs 横向滚动、status/accounts 单列、表头停 sticky、`.card-body` 横向滚动、Dashboard KPI 6→2 列）与 ≤560px（`.row-actions` 换行、更紧的页面 gutter）两层，见 styles.css 的 responsive 段；新增布局必须说明这两个断点下的行为（表格靠 `.card-body` 横向滚动，不隐藏列）。uPlot 图表随窗口 resize 由防抖钩子重设宽度（app.js `chartResizeTimer`）。

## Eval 页

Eval tab（`#eval`，无子段/hash 参数）承载「已有 API 无 UI」的评测观测面：**Shadow
Report** 卡（`/api/shadow-report`，固定 24h 窗口：Route/Primary/Shadow/Samples/Status
Match——pure.js `shadowMatchBadge` 分档 ≥99% ok / ≥95% warn / 以下 err / 缺失 muted——
与双侧延迟、Δms；`enabled:false`（request_log 关闭）渲染说明性 hint）与 **Fusion** 卡
（`/api/fusion`：workflows 汇总表 Runs/Today/Quorum Met/Amplification/Degraded 原因计数
+ 最近 runs 表 Time/Workflow/Route/Quorum 徽章/Drafts/Legs（title 载逐 leg 状态）/Judge/
Synth）。两个 loader 经 `Promise.allSettled` **独立 settle**：半边失败保留旧数据，错误经
`setRefreshError` 横幅聚合，只有两边都失败且无旧数据时才只剩横幅。全部渲染为用户触发
（tab 激活经 `retainTab` 重入守卫 `.eval-host`、Refresh 点击），无自动刷新 tick。

## Takeover 页

Takeover tab（`#takeover`，无子段/hash 参数）是 `takeover`/`restore` CLI 的 Web 面
（`/api/takeover` 一族端点，语义归 docs/web-api.md 与 docs/client-takeover.md）：单卡
「Client Takeover」列出全部模板——Client 列是客户端族（pi/opencode/claude…），Template
列是具体变体（pi-openai…；标记：`*` = 当前模式下该族会被写入的变体、只标多变体族，
`⇄` = split 会写出不同集合；纯逻辑 pure.js `takeoverClientLabel`）/Format/Source/
Config File/Status，状态徽章五态——not installed（muted）/ not taken over（muted）/
taken over（ok）/ drift（err，title 载 current→expected），徽章唯一实现 pure.js
`takeoverStatusBadge`。头部动作行：Mode 选择器（`.req-input` select，
unified/split/anthropic/openai/responses，会话内记忆 `takeoverMode`）——**切换即经
`?mode=` 重拉 surface 刷新 `*` 预览**，旁挂模式语义 hint（pure.js `takeoverModeHint`）；
restore 不看 mode——+ Takeover All / Restore All / New Template；行内动作 Takeover
（未接管且已安装）或 Restore（已接管，confirmDialog 确认）+ Edit（模板编辑器）。
执行结果渲染进卡内 `.msg` 区（applied 名单带 note、skipped、warnings），mutation 后
整表重拉；表尾 `.tk-dirs` 行展示 templates/backup 目录。模板编辑器是 `#tk-modal`
`<dialog>`：preset 只读查看 + 「Save As Override」；user 模板可编辑保存（PUT 先校验
后落盘，后端 400 消息内联显示）/ 删除（confirmDialog）；New Template 需要名字输入。
**全部渲染为用户触发**（tab 激活经 `retainTab` 重入守卫、Refresh/按钮点击），无自动
刷新 tick，不适用 deferAutoRefresh 双门；失败保留旧 DOM 走 `setRefreshError`。结构标记
`.tk-host`（无视觉样式，registry CLASS_EXEMPT）；表格/徽章/按钮/模态/表单全部复用
既有 `.table`/`.badge`/`.btn`/`.field`/`.msg` 模式；新增样式仅 `.tk-dirs`（目录行）。

## MCP 页

MCP tab（`#mcp`，无子段/hash 参数）展示网关面（`/api/mcp`）：servers 卡（Name/Enabled/
Transport/Auth/Endpoint/Accounts/Sessions + 行内 Test 按钮）与 routes 卡（Name/Enabled/
Targets 链/Sessions）。Test 触发 `POST /api/mcp/test`（握手探测），结果行内渲染在该 server
行正下方（ok/fail badge + serverInfo/延迟 + 工具徽章，超 8 个折叠计数）。**全部渲染为用户
触发**（tab 激活经 `retainTab` 重入守卫、Refresh/Test 点击），无自动刷新 tick，因此不适用
deferAutoRefresh 双门；后台刷新失败保留旧 DOM 走 `setRefreshError`。结构标记 `.mcp-host`
（无视觉样式，已登记 registry 的 CLASS_EXEMPT）；表格/徽章/按钮全部复用既有
`.table`/`.card`/`.badge`/`.btn` 模式，无新增样式。
