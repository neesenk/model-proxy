# Request log 与实时观测

## 适用范围

修改 request log、`internal/transport/bodycapture`、live events 或 replay
时必读。API 字段另见 `docs/web-api.md`。

## Live events

`internal/observe/events.Hub` 保存最近事件并做非阻塞 fan-out。慢订阅者
丢事件，不能反压请求路径；`/api/events` 的 SSE/keepalive 由应用层
`internal/app/proxy_http.go` 与 `internal/web/server.go` 服务（events 包只提供
`ServeEvents` handler，不拥有 HTTP 路由）。

ring 按协议分环：`protocol="mcp"` 的事件进 MCP 环，其余（LLM 协议路径、
guard/独立事件）进 model 环，各自独立淘汰（容量 model 2000 条 /
MCP 500 条——MCP 每次交换只发 start/end 两条，500 条已覆盖约 250 次交换）
——重 model 流式流量（progress 事件每 16 KiB 一条）不再把 MCP 历史挤出
live 视图（反向同理）。`Subscribe`/`Snapshot` 返回按 ts 升序合并的两环重放；
ring 内保留的 progress 事件 `text` 截断到 4 KiB（rune 边界，实时订阅者收
全量前缀，只有留存副本受限），环内存上限 (2000 + 500) × 4 KiB。

forward 产生 start/end，包含 agent、protocol、provider、status、latency、tokens（end 另带 `cache_read`/`cache_creation`，omitempty）和稳定 request_id。400/502 终局也必须产生 end。进入 live/请求日志的是 LLM 协议路径（`/v1/messages`、`/v1/chat/completions`、`/v1/responses`、`/v1/decisions`）与 MCP 网关交换（`/mcp/<name>`，`protocol="mcp"`、request log `kind="mcp"`，见 `mcp.md`）；未知路径（浏览器 `/.well-known/...` 探测、favicon、迷路 GET）在 handler 层直接 502，**不产生 live 事件、不写请求日志**（unrouted model 仍是非空 proto，照旧产生终局 end）。`GET /api/events` 先重放 ring，再推送 SSE，15 秒 keepalive。

出站秘密扫描（DLP-lite，`guard.secrets`）在 forward 读取完整请求体后、请求画像计算与所有 forward 分支之前对共享 body 扫描一次（`internal/guard` 的高置信
模式表，只扫请求不扫响应，SSE/流式同样适用）。命中时额外产生一条
`type:"guard"` 事件，`detail` 只含模式类型名与生效动作
（`secrets=<名字,…> action=<log|redact|block>`），并按 `("guard", <模式类型名>)`
虚拟键计数（requests 列为命中次数）——命中内容本身绝不进入事件、计数器、
日志或 request log（redact 后 request log 看到的也是 `[REDACTED]` 占位）。
`block` 动作返回 400 并照常产生终局 end 事件；`off` 完全不扫。

## Request log

`replay` 使用 request_log 的原始客户端 body 和原 path，通过 force-provider 重发。
拒绝历史 Shadow record、非 `/v1` 路径和截断 body。

request log 是异步、非阻塞、owner-only 的 JSONL。每条记录可选地携带 `turn_key`——写入时从 request body 提取的对话轮次指纹（携带真实文本的 user 消息数 + 最后一条真实 user 文本的哈希；tool_result 块不算，agent 注入的 `<system-reminder>`/`<git-context>` 信封会被剥离、既不计数也不作为提取文本，因此 agentic 轮次内消息增长或追加 reminder 时指纹恒定），供 UI Trace 时间线按轮次分段；旧记录或无法提取 user 文本时为空/省略。

`routing` 是 route 级策略（`route_policy:` bands / latch / selector）的判定结果元数据，只含结构信息（档名/来源/候选 id/置信度/难度），不含请求/响应文本。JSON 形状：

```json
{"source":"band|selector|latch|fallback", "grade":"flash", "target":"zhipu/glm-5.3-flash",
 "selector":{"choice":"g0","confidence":0.62,"difficulty":2,"enforced":false}, "latch":"grade:pro"}
```

- `source`：最终生效的决策来源（优先级 latch > selector > band > fallback）。
- `grade`/`target`：graded route 填档名，非 graded route 填首个目标。
- `selector`：只要 selector 运行过就记录（shadow 模式 `enforced=false`），含原始 choice id、confidence、difficulty、是否 enforce，以及可选的 `err`。这里的 shadow 是 selector 的“只记录、不执行”模式，与已删除的顶层 Shadow 转发无关。
- `latch`：命中会话 latch 时记录 latch 值（如 `grade:pro` 或 `provider/model`）。

未配置 `route_policy` 的 route、pin/force-provider 路径、没有任何策略步骤命中时，该字段整体省略。索引侧把 `routing` 作为 JSON TEXT 列持久化，旧行保持 NULL，L0 查询侧不解 JSON。

`profile` 是请求画像与上下文构成的持久化投影（committed 记录携带；
attempt-only 失败腿与字段存在前的旧记录省略）。产生权威在 `internal/routing`
（`ProfileRequest`/`CountRoles`——与 request-aware 路由用的是同一组 body 事实），
经 `targetexec.RequestProfile` 词表投到日志形状（`forward.BuildRequestLogInput` 是唯一
映射点）。字段：`est_tokens`（输入 token 估算，口径同路由画像：CJK 每字符 1 token、
其余约 4 字节 1 token、base64 长段排除）、`image`/`tools`/`follow_up`（布尔，省略假值）、
`roles`（对话构成计数：`system`〔含 openai developer role 与 anthropic 顶层 system 字段〕/
`user`/`assistant`/`tool`〔openai tool+function role 与 anthropic tool_result 块〕）。
UI Requests 详情的 meta 条渲染为一行（magpie context 网格的日志字段版：不打开 body
即可回答"这次请求的窗口被什么填满"）。计数是 marker 级扫描，只在 request_log 开启时
为已提交请求付费，且每请求至多一次（`serveState` 缓存）。

`schedule` 是调度器视角的补充元数据（每条 attempt 记录一层）：`source` 命名该 pass 的调度引擎（`quota`/`sticky`/`pin`/`load_balance`/`force`——`force` 标记请求被 force-provider 收窄到单 provider）。与 `routing`（route_policy 层）正交：`routing` 重排档位/候选，`schedule` 只说明调度器给出的基础顺序。索引侧同样作为 JSON TEXT 列持久化，旧行保持 NULL。

**MCP 拆分流（`request_log.mcp_split`，默认关）**：开启后 `kind="mcp"` 记录写入
独立的 `mcp-YYYYMMDD.log` 流（目录 `mcp_dir`，默认 `~/.model-proxy/log/mcp`，
与 requests- 流共享 max_file_size/max_body_bytes/retention/hot_retention 策略（拆分流同样按热窗
压缩为 `mcp-*.log.zst`，`kind=mcp` 视图的可查窗口即热窗）；`requestlog.Options.FilePrefix`
参数化流前缀，查询侧用 `QueryRecordsIn`/`QuerySummariesWithFacetsIn` 按前缀扫描）。
requests- 流与尾随索引回到 LLM-only；admin 适配层把 `kind=mcp` 查询路由到拆分流
（目录扫描，无索引），其余查询钉在 LLM 行（`kind ""` 等价 `"llm"`，旧文件里的历史
mcp 行不再出现在默认视图与 facets），`Detail` 对两流做先 requests 后 mcp 的
fallthrough。拆分流 restart-only，与主 logger 同生命周期。

查询分两条路径：

协议转换后的响应流与 Responses state/request log 共用
`internal/transport/bodycapture.Reader`：reader 只负责有界 tee、完整长度和
Close-once 回调，日志 schema、入队与 replay 判断不进入 transport 包。

- `requestlog.QuerySummaries` 在 top-K 保留前强制丢弃 request/response body
  和 response headers，内存边界按 `limit × metadata` 计算，不是
  `limit × max_body`；UI list 上限 1000，只保留 metadata，调用方没有可忘记设置的开关。
- detail/replay 才调用 `requestlog.QueryRecords` 保留完整 body。

**尾随索引（`/api/requests`、`/api/sessions` 的默认读路径）**：
`requestlog.Indexer`（`index.go`）把 `requests-*.log` 增量尾随进
`<request_log.dir>/index.db`（SQLite，DSN/WAL/单连接与 stats store 同纪律）——
每行记录的元数据、`ExtractUsage` 投影（索引时解析一次）与原始行的
(file, offset, length) 坐标入库，body 仍只留在 JSONL 文件。索引是**派生视图**：
`files` 表（`size_indexed` cursor + `head`）是权威，reconcile 循环只持有它的内存镜像
（`index_files.go` 的 `fileTrack`）。**文件身份 = head 指纹**：首行前 ≤256 字节（记录行以
ts + request_id 开头，足以唯一标识文件）。每轮：stat 与上一轮相同的文件直接跳过（零 SQL、
零读取——一个月的轮转文件不再每轮逐个查表）；变化的文件读 head 比对——head 相同且 size 不低于
cursor 才是同一文件增长（从 cursor 尾随），否则其已索引内容已离开该名字（替换/截断——**包括
被更大内容替换**，旧实现只看 size 回退会从旧 cursor 读进无关字节）；消失或被替换的名字是
**rename 源**：新出现的文件若 head 相同且 size ≥ 源 cursor，即源文件被改名（writer 的 size 轮转
把活动文件改名为归档名），行与 cursor 一条 `UPDATE records SET file` 搬过去，不再把最大 1GiB
的归档从 0 重读重解析；未被认领的消失文件删行（retention sweep），未被认领的替换文件删行并从 0
重索引；之后所有文件从 cursor 尾随。升级前建的索引（`head` 为 NULL）首轮按 size 规则回填 head。
按文件删除/搬迁走 `idx_records_file`（无该索引时每次轮转/删除都全表扫描）。index.db 丢失或损坏时
删库重建、全量重尾随自愈。**节拍**：ticker 每 ~250ms 一拍，但只有在 log writer 报告目录变化
（`Indexer.Notify`，经 `logfile.Options.OnChange` 在每次写行/轮转与每次 retention/disk-guard 之后
触发；daemon 的 logger 是该目录唯一写者）后的下一拍才跑一轮，否则每 20 拍（~5s）兜底一轮以拾取
外部改动；单事务批量插入（每批至多 500 行**或 64MiB 行字节，先到为准**），
尾部不完整行留到下一轮；`rowFromLine` 在 `ExtractUsage` 之后立刻置空
request/response body 字符串——索引行只驻留元数据（历史上整批 500 行持有完整
multi-MB body 曾把堆峰值顶到数 GB，字节封顶与置空 body 是它的双层防护）。
**冷层归档**（`request_log.hot_retention`，默认 168h、`0` 禁用、restart-only）：未配置时也按
168h 参与加载期交叉校验——`hot_retention ≥ retention`（两者都 > 0）即拒绝加载，且不看
`request_log.enabled`，所以 `retention ≤ 168h` 的配置必须显式设置本键或设 `0`（`retention: "0"`
永久保留不构成上界）。启动与每小时的 sweep 在 retention 删除与磁盘护栏之后，把滚出热窗的轮转
`.log`（mtime 基准）交给 logfile 的旁路压缩 worker（独立
goroutine——压缩耗时不得阻塞写路径；owner/stop/wait/final flush 由 Logger 生命周期承担）。worker 以
`X.log.zst.tmp` → rename `X.log.zst` → 删原文件的原子序列落盘（崩溃后 `.zst` 已在则只删原文件，幂等；
压缩或 rename 失败时顺手清掉 `.zst.tmp`——否则源文件随后被护栏/retention 删走后，孤儿 tmp 不属于任何可删
集合；崩溃残留的 tmp 在其源 `.log` 消失后由派发步的回收清扫，源还在的 tmp 属于在途/可重试压缩、不动；
sweep/磁盘护栏在压缩期间删掉源文件时 rename 仍落 `.zst`：归档保留不回收，并记一条 info 日志
（`[<tag>] archive <名>.log.zst: the plain file was removed while it was being compressed …`），
retention 场景下一拍按日戳清掉、guard 场景由后续护栏按名序回收，重复 offer 与重跑路径保持静默）。
每拍扫描一次目录，直接交出一个队列（16）的量，其余留在待派列表并告警
（`archive queue full; N more file(s) waiting`；积压跨拍时 N 含上一拍已交出、仍在队列或压缩中的文件，
至多 17 个）。worker 每完成一项都向 writer goroutine 发可合并、非阻塞的补派发信号，writer 按扫描顺序从
待派列表补位，积压不再等下一拍，补位路径不重扫目录。补位受连续失败预算约束：连续失败达到 3 次即停止
补位，直到出现一次成功或下一拍；预算以下每次失败只补 1 个。因此归档持续失败（磁盘写满且护栏无可删）
时每拍最多尝试 16 + 3 − 1 = 18 个文件、各告警一条，而不是整个积压；压缩失败的文件保持明文并在下一拍
重试（同一拍内不会被补位重试，期间仍可查）；
停机只收尾在途的一项，排队项原样留给下次运行（关停那次 sweep 只做 retention 清理）。
完成即触发 OnChange 让 indexer 走现有「未认领消失文件删行」路径清索引行并按 session 失效缓存。
`.zst` 后缀使 indexer 认领/reconcile/查询扫描三处 `.log` 精确匹配天然不认领——归档是纯离线数据
（`zstd -d | jq`），没有在线读路径，查询与 Requests 页的可查窗口即热窗。retention/disk-guard/prune 的
可删集合包含 `.zst`；`.zst` 时龄取文件名 YYYYMMDD 日戳（其 mtime 是压缩时刻），日戳不可解析的
`.zst` 不被 retention/prune 按时龄删除、只有磁盘护栏按名序回收；在途的 `.zst.tmp` 三处都不删。
护栏触发时在报告行里计入本流在途 `.zst.tmp` 的数量与占用（`N unfinished archive(s) hold X in .zst.tmp
(never deleted)`），但它们不进可删集合。
运行日志：启用热窗时启动行 `[request_log] enabled -> …` 末尾带 `, hot window <时长> (older rotated
files zstd-archived, offline-only)`；归档队列满打 `[request_log] archive queue full; N more file(s)
waiting`，压缩或改名失败打 `[request_log] archive <文件> failed (kept plain; retried next sweep): <原因>`；
源文件先被 retention/护栏删走时静默跳过、不打行（压缩期间被删落 `.zst` 时记 info）。残留 tmp 的回收在每拍的派发步里，不依赖归档
是否开启：归档开启时只清源 `.log` 已消失的孤儿；`hot_retention: "0"` 下进程内没有压缩 worker，
本流的 `.zst.tmp` 一律视为残留清掉（三处可删集合都不含它，这一步是它们唯一的出口）。
**流应答聚合落盘**（`request_log.aggregate_streams`，默认关、restart-only）：流式应答每输出 token 约 100 字节
chunk 信封（2026-11-12 峰值日 94.5GB 请求日志里约 80% 是响应流），开启后已完成的应答以聚合后的 JSON
响应落盘、记录带 `response_aggregated:true`，`response_size`/`response_headers` 仍描述线上字节，客户端字节
不受影响。聚合在 log writer goroutine 的 encode 闭包里做（`Logger.aggregateStream`，请求路径不付这份
CPU），折叠器由组合根注入（`internal/app/observe_adapters.go aggregateLoggedStream` = `protocol.AggregateSSE`，
与 stream-only 上游应答非流式客户端的是同一折叠；requestlog 不依赖 protocol 包）。只替换不是证据的字节：
2xx、未截断、LLM（非 MCP）、以 SSE 字段行开头的 body，折叠器干净折叠（无终止信号或流内报错的流它拒绝），
且折叠结果的 `ExtractUsage` 与原流**逐值相等**（索引 usage 列与会话成本从 body 派生，不能因落盘形态改变）；
其余一律保留原始 SSE。成本权衡：`AggregateSSE` 每流约 5ms/12MB 分配，与它省下的行编码 + 索引重解析大致
相抵——这项开关的收益是磁盘写入与留存（典型 20-40 倍）而不是 CPU。
`ExtractUsage` 的成本形态：只有 body 以 `{` 开头才尝试整体 JSON 解码（其余形态整体解码必败，
还白付一次整 body 拷贝）；SSE 按行切子串（无逐行拷贝），一帧只有可能拼出 usage 键时才解码——
encoding/json 以 `strings.EqualFold` 匹配键（ASCII 大小写变体 + `ſ`(U+017F)→`s`）且键可用 `\u`
转义，`usageKeyCandidate` 只跳过不可能含这种拼写的帧，结果与逐帧全解码逐值相同，普通 delta 帧零分配。
Shutdown 在 log writer drain 之后跑一次 final reconcile（final flush）。
**usage 投影与账本重建**：索引的 usage 投影是启动时 usage 账本重建的来源（`observestats.Store.RebuildTokenLedger`
经 `internal/app/observe_adapters.go` 的 `initStats` 读取 `<RequestLog.ResolvedDir()>/index.db`——与 logger /
indexer 同一解析点，`dir` 未配置时即默认目录——用索引读数覆盖 cutoff 前分钟桶，只聚合 LLM 的
live 行），所以热路径
`UsageScanner` 的形态内合并规则必须与 `extractSSEUsage` 同构（规则见
`docs/architecture/protocol-conversion.md`），否则重建会静默改写历史；回归钉在
`internal/app/r6_usage_ledger_matrix_test.go`。跨形态汇总两侧有意不同：本侧逐字段取最大，计数侧
anthropic 帧与其余形态相加。索引存在但没有记录时，本次启动跳过重建并打印一行说明；索引文件缺失或
不可读时重建返回错误，由 `Bootstrap` 告警。重建同步执行、发生在开始监听之前；汇总行
`[stats] rebuilt …` 带聚合组数（`N minute + M agent group(s) aggregated`）与耗时，对非空
索引的每次重建都打印。
写热路径（forward commit → Enqueue）不碰 SQLite。查询与扫描版语义逐字段一致：
`SummariesWithFacets`（WHERE 映射 Filter——From/To 时间窗作用于 `ts_ms`，`ensureColumns` 建的 `idx_records_ts_ms` 让历史窗口（UI 分钟级时间筛选）免于沿 TEXT `ts` 索引逐行过滤所有更新行 + **failover 腿折叠**：已配对 `attempt_only` 行不出列、折入其已提交行的 `failed_hops`（attempt 升序元数据），未应答请求〔既无已提交行也无全链失败终态行，即客户端中途断开〕保留最新一腿独立成行——status/errors 过滤作用于存活行终态，扫描回退路径同语义折叠；facets 在 filter 之前对全部已索引行
采集（扫描回退路径的 facets 受其文件级提前终止窗约束，From/To/Limit 压力下可比索引侧窄——两路径都是「先采集后过滤」，差异只在扫描能整文件跳过）——例外是 `Kind`：它是流选择器而非数据 facet，llm/mcp 过滤同样收窄 facets，
mcp server 名不会污染 LLM 视图的 model 下拉；**attempt_only 行同样不进 facets**（下拉描述产出行的流量，腿已折叠）；usage 字段只在 `UsageOnly` 下填充）、`Detail`（按 (file, offset, length)
seek 读回完整行；索引未命中或 seek 失败回落 `tailScan`——只流式读取各文件「已索引
cursor 之后」的未索引字节：一个 reconcile tick 内刚 commit、indexer 尚未追上的
记录不会误报 not logged，而从未落盘的 id（任何 pre-commit 终局，如 live 视图的
client-gone 499）保持毫秒级 miss，不再是全目录秒级扫描。从未 reconcile 过的文件
〔索引重建中〕cursor 为 0、整个文件都是尾部；截断/替换文件在 reconcile 回卷 cursor
之前没有可读尾部）、`SessionSummaries`（**对每个入列 session 的全部留存历史精确聚合**，
与 `/api/requests?session=` 下钻同口径——排名走 `idx_records_ts_session` 部分覆盖索引的
newest-first 游标，聚合由 Indexer 内增量 per-session 缓存供给：缺失子集一次分组 SQL 算齐、
插入批次逐条折叠、行删除按 session 失效；扫描回退路径保持有界窗口，老 session 在降级下
少计，详见 `docs/web-api.md` 的会话成本汇总节）。indexer 由
`internal/app/observe_adapters.go initRequestLog` 创建、与 logger 同为
restart-only 进程级服务（reload 不重建）；打开失败只 warn，
web 读路径经 `appapi.RequestLogQueries` 端口回落目录扫描（admin 适配层负责
nil-index 退化）。

扫描 `requests-*.log` 时不假设文件名顺序等于 record timestamp 严格顺序（孤儿 active 文件或时钟纠正可能让旧名文件持有新记录），单行用 `bufio.Reader.ReadBytes`（不用 Scanner，避免默认 token cap 丢尾）。查询带**文件级提前终止**：单 writer 向同一文件按 Ts 非降序追加，因此文件最后一条可采纳记录是全文件上界——top-K 堆满后，最新记录仍严格老于堆底的文件不可能改变结果，直接跳过不流式读取；最新记录老于 From 下界（含边界，matches 只丢严格小于 From 的记录）的文件同理无命中；**To 上界有镜像规则**——文件按新到旧迭代，历史窗口查询（UI 的分钟级时间筛选）会先撞到整体晚于 To 的文件，首条（最旧）记录晚于 To（含边界，matches 只丢严格晚于 To 的记录）即整文件跳过，否则选一个老分钟要把该分钟之后的全部记录流式解码一遍。该顺序前提**逐文件验证而非假设**：每个文件独立 peek 首条（头部 128KiB）与末条（尾部 128KiB）记录，首条晚于末条（手工构造/损坏文件）或任何异常（打不开、行超长、JSON 解析失败）都回退为完整流式扫描；跨文件乱序仍被容忍。按 `request_id`/`session_id` 查询另有**行级提前终止**：`rawPrefilter` 先做字节包含检查，行内不含该值就跳过 `json.Unmarshal`（避免为多 MB body 反复分配/解析），命中唯一 request_id 后立即停止扫描——多 GB 活动日志下打开一条记录从秒级降到几十毫秒，唯一性保证早停不改变结果（body 里恰好出现该字符串的假命中仍由 `Filter.matches` 精确校验）。

JSONL schema、Record 编码、查询 heap、Summary 与查询聚合由
`internal/observe/requestlog` 拥有；非阻塞队列、单 writer、按天命名+size 轮转、retention、
**磁盘护栏**与 owner-only 权限等持久化机制由共享 sink `internal/observe/logfile` 拥有
（seclog 同一模式但护栏未接线；活动文件按天命名，同日重启追加同一文件，size 轮转改名，
首次写入才懒建文件）。磁盘护栏（`request_log.disk_guard`，缺省 enabled/50GiB/95%）是
retention 之上的空间下限：每小时 sweep 在时龄删除后检查日志目录所在卷，空闲低于
`min_free_bytes` 或使用率 ≥ `max_used_percent`（unix Statfs / windows GetDiskFreeSpaceEx，
包内 seam 可注入测试）时按名序删最旧轮转文件至条件解除，报告行同时给出本流在途归档的占用；活动文件与 `index.db` 永不删，
条件值 0 关闭该条件，索引一致性沿用 prune 契约（reconcile/启动重建丢弃消失文件的行）。
`internal/app/observe_adapters.go` 只完成 config
和执行上下文到纯值 Input 的映射（外加 Indexer 的创建——索引 schema、reconcile 与
索引侧查询仍归 requestlog）。通用 stream capture 留在
`internal/transport/bodycapture`，两者不反向依赖。


## 回归测试

- request log 大 body 的 metadata 内存边界和跨文件乱序。
- request log 尾随索引：与扫描逐字段等价、rotation/retention/截断/半行补全、
  索引损坏重建、detail seek 与未命中回落、并发 race-clean（`index_test.go`）。
- lifecycle 在 shutdown 后拒绝新任务，并等待已接纳的后台任务退出。
- decisions selector 的 generation-bound resolver、target plan、model lock、
  paramBlock 即时重试、429/5xx/空 answers/缺 model_choice/超时健康门口径。
