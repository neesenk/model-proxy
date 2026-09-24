# Fusion、Shadow、Cache 与实时观测

## 适用范围

修改 `internal/forward/fusion.go`、`internal/fusion`、`internal/app/proxy_shadow.go`、`internal/shadow`、
request log、`internal/cache`、
`internal/transport/bodycapture`、live events 或 replay 时必读。API 字段另见
`docs/web-api.md`。

## 精确响应缓存

缓存默认关闭：`cache.enabled/ttl/max_entries/max_body_bytes`，默认 TTL 10m、1000 条、单 body 256 KiB。

- key 是 method、path、原始 query、白名单 header（`anthropic-beta`、
  `accept-language`）与原始请求 body 的 SHA-256，各字段长度前缀编码
  （body 是任意字节，裸分隔符拼接存在理论碰撞）；
- 只缓存 `<300`、完整读到 EOF、未超过 body cap 的响应；
- 客户端断开或半截响应不得入缓存。流式响应的"半截"按协议终态序列判定
  （`protocol.StreamTerminalComplete`，在 `targetexec` commit 的 cache Put 前执行）：
  anthropic 需带非空 `stop_reason` 的 `message_delta` + `message_stop`，openai 需
  非空 `finish_reason` 或 `[DONE]`，responses 需 `response.completed`/`response.incomplete`；
  终态错误帧（anthropic `error`、openai error chunk、`response.failed`）一律视为不完整。
  干净 EOF 但终态缺失（实测：kimi-code 上游弃单后只发裸 `message_stop`）照旧透传给
  客户端，但不入缓存——否则同一请求的重试会在 TTL 内反复吃到截断副本；
- 转换后的响应删除旧 Content-Length/Transfer-Encoding 后保存；mode mismatch
  重写响应时保存的是 live 路径实际发送的 content-type（客户端最终收到的
  协议体与 content-type 一致）；
- 命中重放原始客户端协议字节并设置 `x-mp-cache: hit`；
- cache hit 不计 provider metrics/agent stats，但产生 live end event，并写一条 `provider="(cache)"` 的 request-log 记录（请求体 + 重放的响应体，usage 为空）——缓存命中的请求在 Requests 页/会话聚合/守卫关联里历史可见，不再只活在 Live；
- pin 和 force-provider 跳过读写缓存；
- reload 重建缓存并清空条目（缓存 body 不落盘）；
- **命中/未命中计数器持久化**：`~/.model-proxy/cache_state.json`（`internal/app/cache_state.go`）按
  called model 名记录累计 hits/misses（含 per-model 明细；entries 是 live gauge 不落盘）。app 拥有
  文件 I/O，cache 叶子保持零 I/O：启动时只 load-seed 一次 `cache.Counters`，各代 Store 经
  `Options.Counters` 共享同一个进程级计数 owner；reload 仅重建响应条目，不读盘、不重复 seed，
  旧 snapshot 的迟到 Lookup 仍累计在同一 owner。缓存关闭时 owner 继续存在。
  lifecycle 拥有的每分钟循环（含启动时关闭、之后 reload 开启的情况）+ shutdown final save
  原子写（temp+fsync+rename）；`cachePersistMu` 串行化 snapshot→write，避免迟到写相互覆盖。
  reset-stats（POST /api/tokens/reset）**不动**响应缓存计数器——命中率是运营指标而非用量
  记账，内存计数与持久化历史都保留（契约由 `cache_state_test.go` 的
  `TestResetStatsKeepsResponseCacheCounters` 钉住）；I/O 不持 `Proxy.mu`。
  corrupt/未来 version 文件按零状态起步，绝不阻塞启动。

缓存机制由 `internal/cache` 叶子包拥有：request key、TTL/容量 store、
bounded recorder、转换后 header normalization 与逐块 flush replay。
`internal/app/proxy.go` 的 `NewResponseCache` 只注入配置生效值；
请求通过 `RuntimeSnapshot.Cache` 保持
generation 隔离，reload 后旧请求即使完成也只能写入旧 Store。

缓存定位是重复请求/重试盾牌，不是多轮对话前缀缓存。

## Live events

`internal/observe/events.Hub` 保存最近事件并做非阻塞 fan-out。慢订阅者
丢事件，不能反压请求路径；`/api/events` 的 SSE/keepalive 由应用层
`internal/app/proxy_http.go` 与 `internal/web/server.go` 服务（events 包只提供
`ServeEvents` handler，不拥有 HTTP 路由）。

ring 按协议分环：`protocol="mcp"` 的事件进 MCP 环，其余（LLM 协议路径、
guard/budget/独立事件）进 model 环，各自独立淘汰（容量 model 2000 条 /
MCP 500 条——MCP 每次交换只发 start/end 两条，500 条已覆盖约 250 次交换）
——重 model 流式流量（progress 事件每 16 KiB 一条）不再把 MCP 历史挤出
live 视图（反向同理）。`Subscribe`/`Snapshot` 返回按 ts 升序合并的两环重放；
ring 内保留的 progress 事件 `text` 截断到 4 KiB（rune 边界，实时订阅者收
全量前缀，只有留存副本受限），环内存上限 (2000 + 500) × 4 KiB。

forward 产生 start/end，包含 agent、protocol、provider、status、latency、tokens（end 另带 `cache_read`/`cache_creation`，omitempty）和稳定 request_id。cache hit、400/502 终局也必须产生 end。进入 live/请求日志的是 LLM 协议路径（`/v1/messages`、`/v1/chat/completions`、`/v1/responses`）与 MCP 网关交换（`/mcp/<name>`，`protocol="mcp"`、request log `kind="mcp"`，见 `mcp.md`）；未知路径（浏览器 `/.well-known/...` 探测、favicon、迷路 GET）在 handler 层直接 502，**不产生 live 事件、不写请求日志**（unrouted model 仍是非空 proto，照旧产生终局 end）。`GET /api/events` 先重放 ring，再推送 SSE，15 秒 keepalive。

出站秘密扫描（DLP-lite，`guard.secrets`）在 forward 读取完整请求体后、cache
查询与所有 forward 分支之前对共享 body 扫描一次（`internal/guard` 的高置信
模式表，只扫请求不扫响应，SSE/流式同样适用）。命中时额外产生一条
`type:"guard"` 事件，`detail` 只含模式类型名与生效动作
（`secrets=<名字,…> action=<log|redact|block>`），并按 `("guard", <模式类型名>)`
虚拟键计数（requests 列为命中次数）——命中内容本身绝不进入事件、计数器、
日志或 request log（redact 后 request log 看到的也是 `[REDACTED]` 占位）。
`block` 动作返回 400 并照常产生终局 end 事件；`off` 完全不扫。

## Shadow

每 route 的 `shadow:` 条目（`config.ShadowTarget`）只声明 provider/model/protocol；
`shadow_sample_rate` 与 `shadow_max_concurrent` 是顶层全局配置（不随 route 变），
`shadow.Runtime` 是进程级单例。生产响应 commit 后 fire-and-forget：

- sample rate 的 nil 表示 1.0，显式 0 表示关闭；
- semaphore 满时丢弃 shadow（丢弃可见：runtime 的 dropped 计数驱动 `[shadow] concurrency gate full` 告警日志，首次与每第 50 次一条）；
- shadow 的 HTTP 预算独立封顶：`min(scheduling.upstream_timeout, 5 分钟)`——上游超时默认 1800s 是为生产流式生成设计的，一个挂起的 shadow 不得占用并发槽半小时；
- shadow 使用自身 protocol/baseURL；
- 不得影响熔断、sticky、生产 metrics 或响应；
- 结果进入 request_log，id 以 `shadow-<primary-id>` 配对，agent 与 session_id 复用主请求的解析值（合成的上游请求不带客户端 UA/session 头，探测它恒为空）；
- reload 必须让一次 dispatch 全程使用同一 generation 的 runtime、target、provider map 和 client。

`internal/shadow.Runtime` 拥有可热重载的 sample decision、semaphore、专用
timeout client 和 detached transport；`Execute` 只消费应用层已解析的
`targetexec.Plan` 与主请求 commit body，完成 model rewrite、fail-closed
conversion、provider rewrite/auth/header、HTTP drain 和 bounded capture，不得
进入 `targetexec.Executor` 或生产 metrics/health/sticky/events。

`internal/app/proxy_shadow.go` 是唯一 post-commit adapter。单目标 executor 只返回含实际
上游请求体的最小 `targetexec.Commit`，不持有 Shadow 或 lifecycle 回调。
`serveOnce` 确认 commit 后调用 adapter；adapter 按
eligibility → sample → non-blocking acquire → lifecycle admission 的顺序接纳，
`TryAcquire` 返回一次性、幂等释放的 permit，admission 拒绝与任务完成路径各自
释放同一 permit，禁止直接操作共享 semaphore。adapter 在启动 goroutine 前同时
捕获 `RuntimeSnapshot` 与 `*shadow.Runtime`。
goroutine 内由 `internal/app/proxy_shadow.go` 使用 captured provider/pool/generation 解析 virtual
target 和 `targetexec.Plan`，再调用 `Runtime.Execute` 并映射 request log；
禁止重新读取 `p.cfg`/`p.providers`/`p.catalog` 或再次 load `p.shadow`。Fusion
synthesizer 不经过这条 post-commit hook，禁止递归派发 Shadow。

Shadow 作为 `internal/runtime.Lifecycle` 的有限 log-producing task 接纳：shutdown 开始后
拒绝新任务，已接纳任务持有 stop 信号并带 `shadowShutdownGrace`（2s）宽限——宽限内完成的
评估照常在 request logger drain 前落记录；仍挂起的请求在宽限后取消，`Proxy.Close` 不会
被拖过 supervisor 的 10s SIGTERM 强杀窗口（否则其后的 request-log drain、quota persist
等 final flush 全部丢失）。

显式 `protocol:` 在配置加载时同时校验 endpoint：`anthropic` 需要
`anthropic_base_url`，`openai`/`responses` 需要 `openai_base_url`；不允许把
合法协议名配到缺失的 base URL 后留到运行期静默跳过。

`replay` 使用 request_log 的原始客户端 body 和原 path，通过 force-provider 重发。拒绝 shadow record、非 `/v1` 路径和截断 body。

## Request log

request log 是异步、非阻塞、owner-only 的 JSONL。每条记录可选地携带 `turn_key`——写入时从 request body 提取的对话轮次指纹（携带真实文本的 user 消息数 + 最后一条真实 user 文本的哈希；tool_result 块不算，因此 agentic 轮次内消息增长时指纹恒定），供 UI Trace 时间线按轮次分段；旧记录或无法提取 user 文本时为空/省略。

**MCP 拆分流（`request_log.mcp_split`，默认关）**：开启后 `kind="mcp"` 记录写入
独立的 `mcp-YYYYMMDD.log` 流（目录 `mcp_dir`，默认 `~/.model-proxy/log/mcp`，
与 requests- 流共享 max_file_size/max_body_bytes/retention 策略；`requestlog.Options.FilePrefix`
参数化流前缀，查询侧用 `QueryRecordsIn`/`QuerySummariesWithFacetsIn` 按前缀扫描）。
requests- 流与尾随索引回到 LLM-only；admin 适配层把 `kind=mcp` 查询路由到拆分流
（目录扫描，无索引），其余查询钉在 LLM 行（`kind ""` 等价 `"llm"`，旧文件里的历史
mcp 行不再出现在默认视图与 facets），`Detail` 对两流做先 requests 后 mcp 的
fallthrough。拆分流 restart-only，与主 logger 同生命周期。

查询分两条路径：

协议转换后的响应流与 Responses state/Shadow 共用
`internal/transport/bodycapture.Reader`：reader 只负责有界 tee、完整长度和
Close-once 回调，日志 schema、入队与 replay 判断不进入 transport 包。

- `requestlog.QuerySummaries` 与 `requestlog.ShadowReport` 在 top-K 保留前强制
  丢弃 request/response body 和 response headers，内存边界按
  `limit × metadata` 计算，不是 `limit × max_body`；UI list 上限 1000、
  shadow report 上限 10000 均只保留 metadata，调用方没有可忘记设置的开关。
- detail/replay 才调用 `requestlog.QueryRecords` 保留完整 body。

**尾随索引（`/api/requests`、`/api/sessions` 的默认读路径）**：
`requestlog.Indexer`（`index.go`）把 `requests-*.log` 增量尾随进
`<request_log.dir>/index.db`（SQLite，DSN/WAL/单连接与 stats store 同纪律）——
每行记录的元数据、`ExtractUsage` 投影（索引时解析一次）与原始行的
(file, offset, length) 坐标入库，body 仍只留在 JSONL 文件。索引是**派生视图**：
文件消失（retention sweep）对应行删除；文件 size 回退（截断/轮转重建）该文件
行清零并从 0 重索引；index.db 丢失或损坏时删库重建、全量重尾随自愈。reconcile
每 ~250ms 一轮，单事务批量插入（每批 500 行），尾部不完整行留到下一轮；
Shutdown 在 log writer drain 之后跑一次 final reconcile（final flush）。
写热路径（forward commit → Enqueue）不碰 SQLite。查询与扫描版语义逐字段一致：
`SummariesWithFacets`（WHERE 映射 Filter，facets 在 filter 之前对全部已索引行
采集——例外是 `Kind`：它是流选择器而非数据 facet，llm/mcp 过滤同样收窄 facets，
mcp server 名不会污染 LLM 视图的 model 下拉；usage 字段只在 `UsageOnly` 下填充）、`Detail`（按 (file, offset, length)
seek 读回完整行；索引未命中或 seek 失败回落 `tailScan`——只流式读取各文件「已索引
cursor 之后」的未索引字节：一个 reconcile tick 内刚 commit、indexer 尚未追上的
记录不会误报 not logged，而从未落盘的 id（任何 pre-commit 终局，如 live 视图的
client-gone 499）保持毫秒级 miss，不再是全目录秒级扫描。从未 reconcile 过的文件
〔索引重建中〕cursor 为 0、整个文件都是尾部；截断/替换文件在 reconcile 回卷 cursor
之前没有可读尾部）、`SessionSummaries`（索引列供给
usage，聚合复用扫描版同一 Go 代码）。`/api/shadow-report` 的配对只需要 metadata
（request_id/shadow 标志/provider/status/latency/response_size，不需要 body），同样
走索引：`ShadowReport`（索引列供给配对元数据，聚合复用扫描版同一 Go 代码，
Limit 同为配对前的新到旧 top-K）；索引不可用回落目录扫描，语义一致。indexer 由 `internal/app/observe_adapters.go initRequestLog`
创建、与 logger 同为 restart-only 进程级服务（reload 不重建）；打开失败只 warn，
web 读路径经 `appapi.RequestLogQueries` 端口回落目录扫描（admin 适配层负责
nil-index 退化）。

扫描 `requests-*.log` 时不假设文件名顺序等于 record timestamp 严格顺序（孤儿 active 文件或时钟纠正可能让旧名文件持有新记录），单行用 `bufio.Reader.ReadBytes`（不用 Scanner，避免默认 token cap 丢尾）。查询带**文件级提前终止**：单 writer 向同一文件按 Ts 非降序追加，因此文件最后一条可采纳记录是全文件上界——top-K 堆满后，最新记录仍严格老于堆底的文件不可能改变结果，直接跳过不流式读取；最新记录老于 From 下界（含边界，matches 只丢严格小于 From 的记录）的文件同理无命中。该顺序前提**逐文件验证而非假设**：每个文件独立 peek 首条（头部 128KiB）与末条（尾部 128KiB）记录，首条晚于末条（手工构造/损坏文件）或任何异常（打不开、行超长、JSON 解析失败）都回退为完整流式扫描；跨文件乱序仍被容忍。按 `request_id`/`session_id` 查询另有**行级提前终止**：`rawPrefilter` 先做字节包含检查，行内不含该值就跳过 `json.Unmarshal`（避免为多 MB body 反复分配/解析），命中唯一 request_id 后立即停止扫描——多 GB 活动日志下打开一条记录从秒级降到几十毫秒，唯一性保证早停不改变结果（body 里恰好出现该字符串的假命中仍由 `Filter.matches` 精确校验）。

JSONL schema、Record 编码、查询 heap、Summary 与 Shadow 聚合由
`internal/observe/requestlog` 拥有；非阻塞队列、单 writer、按天命名+size 轮转、retention 与
owner-only 权限等持久化机制由共享 sink `internal/observe/logfile` 拥有（seclog 同一模式；
活动文件按天命名，同日重启追加同一文件，size 轮转改名，首次写入才懒建文件）；
`internal/app/observe_adapters.go` 只完成 config
和执行上下文到纯值 Input 的映射（外加 Indexer 的创建——索引 schema、reconcile 与
索引侧查询仍归 requestlog）。通用 stream capture 留在
`internal/transport/bodycapture`，两者不反向依赖。

## Fusion 工作流

顶层 `fusion:` 定义 2–4 个 panel 成员、synthesizer、可选 `min_panel`、judge、budget、instruction 和 selector。route 通过 `{provider: fusion, model: <workflow>}` 引用。

`internal/fusion.Engine` 是工作流 owner：拥有 first-turn/selector/tools/budget gates、
panel fan-out、quorum/grace、judge、候选注入、degrade 和 registry 记录；
`Registry` 拥有有界 run ring、aggregate 与 daily admission。Engine 只消费
`fusion.Ports`，不持有 HTTP、Proxy、runtime state 或 observability stores。
`internal/forward/fusion.go` 的 `fusionAdapter` 把同一个 `fusionCtx.runtime` 绑定为四个窄能力：
tool capability、非流式 leg、client-facing synthesis 和 selector 判定调用
（`internal/forward/fusion_select.go`）；管线侧 `runFusion` 不再
启动 goroutine、计算 quorum 或维护 registry。

### Panel

每个成员独立 goroutine、独立 timeout。整个 Fusion 请求持有与普通 route 相同的
`RuntimeSnapshot`；goroutine 由 `internal/fusion.Engine` 启动，实际 leg 由管线侧
generation-bound adapter 执行。panel、judge、synthesizer 与普通 route 均通过
`targetexec.Plan` 完成 provider config/runtime impl、backend protocol、model
rewrite、协议转换及 base URL/path 选择，Fusion 不得重新读取 reload-owned
catalog/config。

panel/judge 在此共享 plan 上执行非流式、tool-free 分支，并应用与普通目标一致的
half-open、401 refresh、paramBlock 预应用及即时学习重试、429/5xx、
model-denied/404、empty-200 模型锁、metrics、usage、live event 和 request log
策略。leg 的发送循环（URL 构建、provider rewrite、paramBlock 预应用、POST、
一次性 401 refresh / 400 参数 learn-strip 重试、响应上限读取）由
`targetexec.BufferedLeg` 拥有——Executor 的 headless 非流式对应物；circuit、
metrics、rate-limit 记录与 request log 仍留在 `internal/forward/fusion.go`（最终
exchange 经 `BufferedLeg.Capture` 回填）。verdict 驱动的 /responses leg 遇 404 时同样经
`noteWireResponsesMiss` 翻转 verdict（模型粒度：模型级矩阵有条目时只翻转模型级，否则翻转
provider 级，见 routing-and-failure.md）且**不锁模型**——verdict 判错而非模型缺失。metrics
口径与 tryTarget 对齐：被放弃的 leg 记 evFailovers（连接错误/5xx 另记
evFailures；401 只记 evFailovers）。候选文本按 leg 的 backendProto 解析
（anthropic content[]、chat choices[] 或原生 responses output[] 的
output_text），任何形状解析为空才判 empty draft 并锁模型。responses 客户端的
previous_response_id 在每个 leg 上按 forward 同一规则展开（仅无状态后端；
原生 responses 后端保持透传链路）。被 quorum/grace 主动 cancel 的分支不计
provider failure。候选文本最多 24k rune。

### Quorum 与 grace

`min_panel` 默认 2。达到 quorum 后再等待 5 秒 grace 接收较慢成功；若成功数加在途数已不可能达到 quorum，立即取消剩余分支。

### Synthesizer

候选段随机顺序注入：Anthropic 追加 system，OpenAI 追加 user，responses 追加带
`type: "message"` 的 input item（无类型的 map 会被 r→chat 转换器丢弃）。
synthesizer 与普通 route 共享 `targetexec.Plan`，再构造同一个
`targetexec.Attempt` 进入 `targetexec.Executor.Execute`，因此 SSE、转换、
metrics、cache、request log 和统一失败策略全部一致；两者只能经
`newTargetAttempt(runtime, plan, exchange, scope, policy)` 装配，Fusion 不得
直接构造 attempt 或重新增加平行的 positional 参数/HTTP 执行接口。factory
之前的 Responses expansion、model rewrite 与 request conversion 仍保留
Fusion 的客户端可见 history 语义。

responses 客户端命中 fusion 时，synthesizer 为无状态后端则展开
previous_response_id：发送体是合成体的展开，但**录制的 history 取客户端可见的
原始对话**（注入的 instruction/候选段是单轮脚手架，不得回放到后续轮次）；最终
返回给客户端的 responses 响应由 executor 录制，供下一轮换链。原生 responses
后端保持透传，由上游自维护链。

工具请求只在 synthesizer 保留 tools。synthesizer 不支持 tools 时降级为原始请求直打 synthesizer。

### Judge

judge 是可选的一次非流式内部调用，复用 panel leg 管道。成功报告以 `<JUDGE_ANALYSIS>` 注入候选前；失败只跳过报告，不让整次 Fusion 降级。

### Selector（Jev 路由判定）

selector 是可选的 fan-out 前路由判定：一次 decisions 协议调用（`selector.target`，
必须显式 `protocol: decisions`，如 TypeSafe Jev）并行回答一道 Choice（最佳候选，
选项 = panel ∪ synthesizer 去重，criterion = `rubric:` 或 `provider/model`）和一道
Score（5 级难度）。判定调用复用与 panel leg 相同的 generation-bound 机制（resolver、
plan、health gate、`targetexec.BufferedLeg`、metrics、request log、live event，
tag `fusion-select`），凭据走目标 provider 的 apikey 池——config 里零凭据。

- **state 构造**（`internal/fusion/selector.go` 纯函数）：只取最后一条 user 文本
  （截断 6000 rune）+ 代码算好的画像（轮数/has_tools/has_image/estimated_input_tokens/
  protocol）+ 候选列表。计数与估算留在代码（decisions 模型计数不可靠）；无关上下文
  会拉低判定准确率（context rot）。
- **gate 顺序**：first_turn → selector → tools → budget → fan-out。selector 在 budget
  admission **之前**：`selector_direct` 直调不消耗当日编排预算（扣额度的 `Admit` 只在
  编排路径上执行）；budget 耗尽时（`Registry.Exhausted` 预检，不扣额度）不走 selector
  直接降级——否则耗尽后每个请求都要白付一次 decisions 判定调用。
- **模式**：`shadow`（默认）只记录 `run.Selector`（mode/action/choice/confidence/
  difficulty/latency/err，`/api/fusion` 透出）用于攒对账数据；`enforce` 在
  `confidence ≥ confidence`（默认 0.55）时行动：`difficulty ≤ direct_score_max`
  → `selector_direct`（原始 body 直调选中模型，选中模型不支持 tools 且请求带 tools
  时放弃直调）；`panel_top_k > 0` → 按判定概率裁 panel（永不裁到 quorum 以下，
  保持配置顺序）。判定失败/超时/低置信/choice 不在候选集 → `fallback` 静态全 panel。
- **硬约束在代码**：图片请求跳过 selector（decisions 模型纯文本）；trim 只作用于
  panel 成员；pin/force-provider 在 fusion 拦截点前已处理，selector 不可见。
- **成本与延迟**：一次判定 ≈ 70–500ms（`selector.timeout` 默认 800ms 兜底，失败即
  回退；判定超时是策略性回退，**不计** provider 熔断/失败账——熔断属于共享该 provider
  的 chat/panel 腿，真传输错误仍计）、~340–1000 input tokens（$0.042/M，输出免费）。
  trim 降低容错
  （top_k=2 + quorum=2 时单成员失败即 insufficient_proposers 降级）——默认关闭。

### 成本和观测

- `max_runs_per_day` 在 fan-out 前 admission；进入 fan-out 之前的降级（multi_turn/tools_unsupported/selector_direct）不消耗预算，
fan-out 之后的降级（insufficient_proposers、body_build_failed）仍计入当日预算。
- `first_turn_only` 在多轮会话直接降级。
- 降级原因：`insufficient_proposers`、`tools_unsupported`、`body_build_failed`、`budget_exceeded`、`multi_turn`、`selector_direct`。
- registry 保留 200 条 run，跨 reload 存活。
- SQLite 中 `(fusion, workflow)` 的 requests 表示编排次数，failovers 表示降级次数。
- Fusion 适合高质量单发场景，不应作为默认 route；典型延迟约 2 倍、成本 N+1 倍。

## 回归测试

- cache 完整 EOF、client cancel、转换响应 header、流式终态序列准入
  （裸 `message_stop` 截断不入缓存，`executor_test.go`/`stream_complete_test.go`）。
- shadow detached transport 路径的 reload generation、Close 时 logger drain 顺序、
  协议与 base URL 校验、并发 cap、sample_rate=0。
- request log 大 body 的 metadata 内存边界和跨文件乱序。
- request log 尾随索引：与扫描逐字段等价、rotation/retention/截断/半行补全、
  索引损坏重建、detail seek 与未命中回落、并发 race-clean（`index_test.go`）。
- Fusion pooled resolver、共享 target plan、model lock、paramBlock 即时重试、
  429、empty 200。
- Fusion leg 的 wire-verdict 404 翻转（不锁模型）与失败 metrics 口径、
  原生 responses 后端 leg 的候选文本提取（不误判 empty draft）、
  responses 客户端的 previous_response_id 展开与最终响应录制。
- quorum impossible、grace、judge failure、tools fallback、daily budget。
- Fusion selector：shadow 记录不改路由、enforce direct（不消耗预算、tools 门拦截）、
  低置信/错误/非法 choice 回退静态 panel、按概率 trim（不低于 quorum）、图片请求跳过、
  state 三协议提取与截断（`internal/fusion/selector_test.go`）；decisions body shape、
  429/5xx/空 answers/缺 model_choice/超时的健康门口径（`internal/forward/fusion_select_test.go`）。
- `internal/fusion` 直接断言 gate、fan-out/cancel、三协议 body、registry detached
  snapshot；`internal/shadow` 直接断言 sampling 阈值、gate、header/auth/path、
  conversion fail-closed 与 bounded capture。根包只保留 generation/lifecycle/
  pool/protocol/log/metrics 的跨模块集成断言。
