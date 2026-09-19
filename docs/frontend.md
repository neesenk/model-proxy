# Web 前端页面与组件实现契约

目录级硬规则（设计系统、模式注册表、交互门框架、边界、样式约定、验证）见
`internal/web/assets/AGENTS.md`；API shape、状态字段与 mutation 语义归
`docs/web-api.md`。本文件承载各页面/组件的实现级行为契约——修改对应页面前
阅读相关小节；新增页面级行为归入本文件，不要回灌目录级 AGENTS.md。

## 自动刷新 tick 盘点

Status 5s（整页重渲染，双门）、Analytics 30s（仅 live 窗口）、Security 30s（loader 只重绘数据宿主、不重建工具栏）、Accounts 30s（后台失败走 stale 横幅；**操作中守卫**——pane 内有 disabled 按钮时跳过，避免打断 Test/测活/Test All 的进行态）、顶栏 header 5s（`maybeConnRefresh`：仅 dot+meta 文本经 `setConn` 单点写入、不重建面板 DOM → **门豁免**（钉在 jstests/autorefresh.test.mjs）；Status tab 激活时跳过避免重复 /api/status）。新增 tick 的强制规则（双门 + 停止钩子 + autorefresh.test.mjs 加钉）见 `internal/web/assets/AGENTS.md` 交互门一节。

## 请求表（三合一渲染、列几何、虚拟滚动）

**表内 tokens 单元的数字用 `fmtCompactNum` 压缩（<1K 原样、<10K 一位小数 K、<1M 整数 K、≥1M 一位小数 M；精确值进单元 title tooltip），cache read 带命中占比（两位小数、去尾零；≥80% `.tok-cache.hot` ok 色）；详情结构是三区分层：labeled meta strip（pure.js `requestMetaHTML`：when/call/result/route/size 五组小写标签 + mono 值，status 用彩色 badge，相对时间 T+/Δ 由 `requestRelTimeOpts` 从已加载列表按时间序推导、弹层无邻居时只显绝对时间）→ guard 轨迹（`guardMarksDetailHTML`，仅在有标注时渲染，挂在首条记录 meta 之后：与 meta strip 同一扁平语法——单个 `⚑ guard` 标签组，每条一行 verdict 徽章引导 + rule code + 归属〔judge/cached/action/kind〕+ 右缘时间，reason 单行省略号、全文〔reason+evidence+detail〕进 title tooltip；不嵌套卡片）→ 对话/原始 body（`chatViewHTML` + 折叠 raw bodies）；model 单元的 guard 徽标（`guardMarksHTML`，≤3 个 + 溢出计数：`⚑ block`/`judge·verdict`/`unblocked`，title 载 scrub 后 reason/溯源）。三张请求表是单一份实现**（pure.js `requestTableHeadHTML` + `requestRowHTML`）：Requests 标签页、Live 全量环、Live 会话视图共用同一列集（time · agent · session · status · model · provider · ms · tokens in/out + cache read），Live 只是数据源换成实时行；Requests 记录经 `persistedSummaryRow` 投影成合并行形态后进同一渲染器（`guard` 标注随行——服务端在 `/api/requests` summary 与 `/api/requests/<id>` 的 `guard` 键按 request_id join 安全审计轨迹，**前端不做第二次推导**；详情卡经 `detailRecordsHTML` 的 `opts.guard` 注入，弹层/live 详情不传时只渲染 meta strip）；改列/徽章/单元格语义只改这一处，三表同步生效。**列几何同样固定在这一处**：`requestTableHeadHTML` 输出 colgroup（八列百分比、合计 100%），styles.css 对三张请求表开 `table-layout: fixed`（窄屏 ≤720px 给 720px min-width，靠 `.card-body` 横向滚动而不是挤压列）——Requests 按窗口挂载行、Live 按 SSE 事件重渲染，auto layout 会按在场行重算列宽，滚动时列宽肉眼抖动；改列宽只改 colgroup。bytes 列已移除（token 单元是有效信号）。**Requests 页是左侧边栏导航**（`.req-layout` 网格 210px+1fr，与 Status/Accounts 同款侧边栏几何但**独立 class**——`.req-nav`/`.req-nav-item`/`.req-nav-title`，与 `.status-nav`/`.acct-nav` 的「视觉相同、class 隔离」先例一致，tab 级 querySelector 永不串扰；≤720px 折为换行 chip 行）。导航按**流量类型分组、视图作项**：`Model` 组与 `MCP` 组（`.req-nav-group`，标题 `.req-nav-title`），每组下 `Log` / `Live` 两项（缩进一级）——每项钉住 (流, 子视图) 对，`selectRequestsView` 一次点击切换两者（流切换清 session/shadow、同步双侧 chrome）；当前项高亮 = (sub, stream) 同时匹配，所在组标 `data-on`（标题着 accent ink 强化层次）（`syncReqNav`）。**MCP 会话查找是原生功能**（不是 Model 机制的搬运）：MCP 流的 session 选项来自已加载的 MCP 记录自身（`/api/sessions` 聚合是 LLM-only，MCP 不读它），live 会话下拉选项 = 环内当前流的行 + 按流的持久化池（Model 流 `/api/sessions`，MCP 流 `/api/requests?kind=mcp` 记录去重；`loadLiveSessionPool`，流切换时清旧池重载——`applyRequestsStreamFlip` 两条入口共享，哨兵 `null` 强制重建）；会话视图的持久化拉取带 `kind=`（MCP 会话只显 MCP 交换，聚合 chips 由行推导——requests/错误/延迟/服务器/账号，无 LLM 的 token/成本）。流切换清会话选择（两侧会话 id 语义不同、不互通）。shadow 过滤器已从工具行移除（控制项与查询参数都不再发送，legacy hash 值解析后惰性）。流即 `/api/requests` 的 `kind=` 参数——同一张表、同一套过滤器服务 LLM 转发日志与 MCP 网关日志（`request_log.mcp_split` 开启时 kind=mcp 读拆分流）；hashchange 路径经 `setRequestsStream` 分片应用同一状态。model 输入 placeholder 换 "All Servers"（MCP 记录的 exposed 是服务器名）；agent 过滤器照常工作（MCP 记录的 agent 来自 clientInfo/UA 归属）。流选择随 URL hash 保持（`#requests?stream=mcp` / `#requests/live?stream=mcp`，pure.js `requestsFilterQuery/FromQuery` 的 `stream` 键）。MCP 流的表头与行单元格换 MCP 域语义（同一渲染器、**MCP 专属 7 列几何**，`requestTableHeadHTML({mcp})`/`requestRowHTML` 的 `mcp` opt）：Model→**Server**（exposed=服务器名）、Provider→**Account**（池虚拟账号 id）、**无 token 列**（MCP 交换无用量，会话查找才是分析轴）——LLM 术语不出现在 MCP 表里。详情下钻带 `kind=mcp` 路由提示（`requestDetailURL`）：MCP id 不在 requests 索引里，无提示时索引 miss 兜底会全量扫描多 GB 的 requests 目录（秒级），提示后直读拆分流（毫秒级）。导航项命名 `All Requests` / `Live Requests`，Live 卡无标题栏（侧边栏已命名视图，标题只会重复）。MCP 记录的详情展开复用同一 `detailRecordsHTML`——JSON-RPC body 不解析为对话（`chatViewHTML` 返回空），自动回落 raw body 折叠视图，meta strip 的 call 组显示 JSON-RPC method + `/mcp/<name>` path。

**Requests 表是虚拟滚动的**（app.js `reqVirt` 一套：`reqReconcile`/`reqFrame`/`reqLoadOlder`）：浏览默认只拉 50 条（session 下钻首拉 500），滚动近底用相同过滤参数 + `to=<已加载最旧秒>` 键集分页拉更旧一页（边界秒重拉、`mergeRecordsPages` 按 id 去重，pure.js；累计 1000 封顶），DOM 只保留视口窗口内的行（spacer 行 `.req-spacer` 撑住滚动几何，测量高度进 offsets、未测行用滑动均值估计）；行节点按 id 池化复用（`reqRowNode`），**展开详情的行钉住不卸载**（详情的分块 body 状态活在 DOM 节点上），时间线跳转经 `reqRowForId` 先挂载（reveal pin 防被窗口移动卸载），再在详情填充后按行实时 rect 瞬时跳转（`behavior:auto`——平滑滚动会被窗口 reconcile 的 replaceChildren 中途取消，rAF 等待在后台标签页挂死，offsets 定位受未测行估算误差累积偏移）；只在展开且行在视口外时滚动，收起或行已可见不动页面（否则会拽动 sticky Trace 卡下的光标位置）；表格重建时 `reqTearDown` 沿池清理 chunk/raw-body 注册表（池化节点可能已脱离容器，不能只扫容器）。滚动监听是 document 级 capture + rAF 合帧（scroll 不冒泡，同 `wireBodyChunks` 惯例）；加载失败保留旧行、hint 行报错并 2.5s 退避重试；整页记录全部落在边界秒（同秒记录数 > pageSize）导致 `mergeRecordsPages` 零新增时同样走该退避，不会无退避热循环重拉同一页。

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
`replayRun`）：`replay <id> --to <provider>` 的行内版——provider 是**常规 select 下拉**
（无输入）：选项按**该请求的模型过滤**（pure.js `linkedProviders` 反查 provider→models，
模型取记录的 exposed（客户端请求名），上游/精确名兜底），数据源优先 `/api/config` 目录
`provider_models`（权威、覆盖未在日志出现过的 provider，首次使用后台拉取并回填所有已挂
载条），目录未热时用 Requests facet 的 provider→models 过渡，完全无匹配（别名/路由改名）
时回落全量 providerOptions 而不是空挂控件；shadow 记录与 MCP 流不提供该条（后端同样
拒绝 shadow；replay 是 LLM 转发管线，MCP 无 provider 轴）。结果就地渲染成**与请求详情
相同的可读格式**：status 徽章 + 延迟 + `chatViewHTML` 对话视图（原始请求体取自展开详情
的缓存，冷路径回源 `/api/requests/<id>` 预热），raw 响应体在与请求/响应 body 同一套
`rawBodyRegistry` 懒渲染折叠 details 里垫底（4 MiB 截断标记随行）；非 chat body（MCP、
错误页）chat 视图为空、raw details 独立承载。结果注入后必须调 `reqDetailChanged()` 让
虚拟滚动重测行高。Live 弹层与 pinned drill 卡不挂 replay 条（仅 Requests 表行内展开）。

**Schedule 卡 per-route Test**（`testRouteTargets`，`test <model>` 的行内版）：route-head
的 Test 按钮对 `POST /api/routes/test`，结果逐 target 一行（✓/✗ 徽章 + provider(model) +
HTTP 状态 + reason + 延迟）渲染在该 route 块内 `.route-test-result`；**原地更新不重建
Schedule 卡**（整卡重渲染会打断其他 route 进行中的测试态）。

**Models 区 Model Catalog 卡**（`refreshModelsCatalog`，`models pull` 的 Web 版）：
`POST /api/models/catalog/refresh` 强刷 models.dev 缓存，结果（数量/etag）行内展示，
提示下一次 reload/takeover 才消费；失败保留旧缓存并显示后端 message。

## URL hash 视图状态

**视图状态进 URL hash**（tab / Accounts provider / Status section / Requests 过滤器与子视图 / Security 过滤器 / Live 会话选择，见 app.js 顶部路由注释）：requests 过滤器只携带非默认值（pure.js `requestsFilterQuery/FromQuery`），变更走 `replaceState` 不刷历史；hashchange 还原时**先种过滤器再激活 tab**，且自由输入控件（provider/model/errors/shadow）必须跟随 `syncRequestsFreeControls`，否则下次 Refresh 会把旧值读回过滤器。**Live 监视已从 Status 移到 Requests 页**（侧边栏 `Live` 组导航，hash `#requests/live`；旧 `#status/live` 链接在 hashchange/boot 原地重写为 `#requests/live`，query 保留——Status 的 section 列表不再含 live）。Live 与 Log 共享 `requestsFilter.stream`（侧边栏 `Live` 组的 Model / MCP 项即流选择；在 Live 切流后回 Log，隐藏控件/placeholder/导航高亮一致），live 环按行携带的 `proto` 过滤（`liveRowInStream`：mcp 流只显 protocol="mcp" 交换，Model 流显其余 + 独立事件行）；MCP live 隐藏 session 选择器（无 /api/sessions backing）。Live 会话选择 rides `#requests/live?stream=…&session=…`（`requestsHash()` 的 live 分支）：**写 hash 必须在渲染之后**（挂载 Live 卡会重置选择，hash 要反映渲染后真值）；hashchange/boot 还原要在子视图挂载之后 apply——boot 特别不能直接 apply（requests tab 异步渲染，直接 apply 会与 Live 卡挂载竞态、挂载重置把选择冲掉），必须存 `bootLiveSession` pending、由 `renderLiveCard` 挂载后消费；下拉选项构建要保底保留当前选中 id（hash 还原的会话可能暂不在 live 行/聚合里）。SSE 生命周期归 Requests tab：离开 tab 关闭连接（`activateTab` 的 prevTab 检查），重进由 `applyRequestsSub` 清空宿主后重挂（保留已完成行）。

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
「Client Takeover」，表格**每客户端一行**（一个模板文档 = 一个客户端），四列（Client /
Config File / Status / Actions）。族行（`.tk-family`）：族名 + hint（配置格式 json/toml/env，与其他客户端一致）+
族配置文件 +
聚合状态徽章 `takeoverFamilyBadge`（changed externally（drift，带解释 tooltip）> taken over
（title 列变体）> not installed（title 说明磁盘上无配置文件）/
not taken over，在 STATUS 列）+ 族级 Takeover（`.btn.primary`，琥珀实心）/ Restore（`.btn.ok`，绿色描边）按钮
（与 CLI `takeover <client>` 同一操作单位）+ Edit（默认描边，打开**整个合并文
档**——多协议变体声明在模板的 variants: 块
里，变体选择发生在 Takeover 确认对话框的单选中）。头部动作行：New Template /
Refresh；表尾 `.tk-dirs` 行展示 templates/backup 目录。

Takeover 走**预览确认对话框**（`#tk-run-modal`，CLI 交互提示的 Web 对应物），执行点
选择一切（行标签 Title Case：Include / Models / MCP / Protocol）：①**范围复选**
Models/MCP（模板带 MCP 面才显示 MCP 复选；全取消禁用确认）；
②**子集选择**（`.tk-subset` 折叠列表：暴露模型/网关 MCP 条目默认全选，取消任一即按
子集请求）；③多变体族的**变体单选**（默认 auto_selected，选其他变体 = 精确 pin 模板
名，与 CLI 指名变体等价；chip 带解释 tooltip，chip/summary/运行结果/Restore 清单一律
显示友好标签 Auto (…)/All Anthropic/All Chat Completion/All Responses/Split by Protocol
或 `family (协议)`，模板 id 如 `opencode-openai` 不上界面）+ split 选项（每协议一条、
全直通，模型分区与子集相交）。summary 条（`.tk-summary`）实时镜像选择：族名 · 变体标签
· 计数（`18/18 models · 14/14 MCP`）。
对话框经
`POST /api/takeover/preview`（`managed_only: true`）dry-run 展示**本次接管会增加/
替换的条目**（`.tk-write` 块：路径 + updates-existing-file/creates-new-file 徽章（前者
title 注明只动 model-proxy 管理的条目）+ 参与变体（渲染为
协议标签 Anthropic/Chat Completion/Responses，原始模板 id 只留 title tooltip）+ 变体选择
说明 `.tk-note` 信息条（accent 淡底，`white-space: pre-line`）+ `.code`
高亮渲染——不含客户端自有内容，聚焦本次变更），确认按钮才执行 `POST /api/takeover`。
Restore 走 confirmDialog（`htmlMessage` 选项渲染备份清单）。执行结果渲染进卡内 `.msg` 区，mutation 后整表重拉；
`loadTakeover` 带请求序号守卫（迟到的旧响应不得覆盖新 surface）。

模板编辑器是 `#tk-modal` `<dialog>`（与 `#tk-run-modal` 同用 `dialog.tk-wide` 加宽
变体；标题 `<name> (built-in|custom)` 标明来源）：**所有模板打开即可编辑**——内置 preset 亦然（hint 注明保存即存用户副本并优先于 preset，
Save As Override），保存走 PUT 先校验后落盘（后端 400 消息内联显示），user 模板另可
删除（confirmDialog）；
编辑器下方 **Rendered Config** 区经 preview 端点（client=模板名、mode unified、
`managed_only: true`）展示**该模板自身写入的条目**（空文件起点渲染——编辑器回答
"这个模板贡献什么"，执行确认对话框回答"我的文件最终长什么样"）。New Template：Template Name + **Format
选择器**（json/toml/env，切换换入对应入门骨架 `TAKEOVER_TEMPLATE_EXAMPLES`，已编辑
内容经 confirm 保护）+ 占位符速查表（`TAKEOVER_PLACEHOLDERS`，引擎占位符集的文档
投影）；保存前无渲染预览。**全部渲染为用户触发**（tab 激活经 `retainTab` 重入守卫、
Refresh/按钮点击），无自动刷新 tick，不适用 deferAutoRefresh 双门；失败保留旧 DOM
走 `setRefreshError`。结构标记 `.tk-host`（无视觉样式，registry CLASS_EXEMPT）；
表格/徽章/按钮/模态/表单/代码块复用既有 `.table`/`.badge`/`.btn`/`.field`/`.msg`/
`.code` 模式；新增样式 `.tk-dirs`/`.tk-family`/`.tk-fam-name`/`.tk-variant`/
`.tk-write`/`.tk-write-head`/`.tk-writes`/`.tk-note`。

## MCP 页

MCP tab（`#mcp`，无子段/hash 参数）展示网关面（`/api/mcp`）：servers 卡（Name/Enabled/
Transport/Auth/Endpoint/Accounts/Sessions + 行内 Test 按钮）与 routes 卡（Name/Enabled/
Targets 链/Sessions）。Test 触发 `POST /api/mcp/test`（握手探测），结果行内渲染在该 server
行正下方（ok/fail badge + serverInfo/延迟 + 工具徽章，超 8 个折叠计数）。**全部渲染为用户
触发**（tab 激活经 `retainTab` 重入守卫、Refresh/Test 点击），无自动刷新 tick，因此不适用
deferAutoRefresh 双门；后台刷新失败保留旧 DOM 走 `setRefreshError`。结构标记 `.mcp-host`
（无视觉样式，已登记 registry 的 CLASS_EXEMPT）；表格/徽章/按钮全部复用既有
`.table`/`.card`/`.badge`/`.btn` 模式，无新增样式。
