# Web UI + `/api/*` 接口契约

> 从 AGENTS.md 拆出。**改 web/API/stats 前必读**。

`web.enabled`（默认 true）时 daemon 同一 mux 挂 `/ui/`（embed 静态资源）和 `/api/`（JSON）。**无鉴权**；loopback 由 `internal/config` 校验强制——`requireLoopbackListen` 拒绝一切非回环 `listen`（`0.0.0.0`、空 host `:PORT`、`[::]`、内网 IP、域名），只放行 `127.x`/`[::1]`/`localhost`。

**前端布局契约**：Status→Logs 每条日志是「行号 gutter + 正文」两列网格；行号与 gutter 右边框留 2px，gutter 背景只覆盖行号列，鼠标悬停标出整条逻辑行，单击选中该行（改变行号前景色，不干预原生选择）。Config→Raw YAML **硬最小高度 480px**，按编辑器 viewport top + 卡片下方 chrome 重新计算，可见空间大于 480px 铺满、不足仍 480px 并允许滚动，绝不靠固定 `100vh - 常量` 推测。

| 方法 | 路径 | 请求 | 响应 | 备注 |
|---|---|---|---|---|
| GET | `/api/status` | — | `{uptime,version,listen,health{...},model_locks{...},quota{...},schedule{...},counters{...},cache{...},warnings}` | handler 只消费 `proxyReadView.dashboard` 的脱离式快照；锁 `p.mu`→`healthMu`→`quotaMu` 的顺序和内部 map 读取由 read view 封装且不嵌套。`quota` 是 `QuotaSnapshot` 原样序列化（无 json tag → **PascalCase**）。`cache` = `{enabled,hits,misses,entries}`（响应缓存观测）。`health[name]` 含 `circuit_state`/`available`/`circuit_until?`/`rate_limited_until?`/`rate_limit_kind?`（429 分类 transient/quota/daily，仅限频中输出）。`model_locks[provider]` = `[{model,until}]`（仅生效中的模型锁，与 health 同一 healthMu 快照，过期不输出；`doctor --live` 用它解释 route 全灭） |
| GET | `/api/logs?tail=N` | — | `{lines:[…]}` | 读 log 文件末尾 N 行（默认 200，上限 1000）；无 log 路径 → 404 |
| GET | `/api/config` | — | `{yaml, summary, provider_models, routes}` | 原文件 verbatim round-trip |
| POST | `/api/config` | `{yaml}` | `{status:"reloaded"}` / 400 | `saveAndReload`：validate → backup `back/<base>.<ts>.bak` → atomicWrite → reload。校验失败不落盘；reload 失败从当次备份回滚 |
| POST | `/api/config/edit` | `{kind,name,data}` | `{status:"reloaded"}` / 400 | 结构化编辑 `kind∈{general,scheduling,provider,route,claude_mapping}`，改 `yaml.Node`（保留注释/键序）→ `saveAndReload` |
| GET | `/api/accounts` | — | `{providers:[{name,provider_id,billing,accounts:[{id,label,added_at,[email]}]}]}` | **响应无任何 key 字段**（无法泄漏）；id 不掩码（UI 要用它删） |
| POST | `/api/accounts/<provider>` | `{api_key, access_key?, secret_key?, label?, replace?}` | `{id,status:"added",warning?}` | 仅 apikey 类；aqp/codex 返 400 指向 async login。volcengine 走 `addVolcengineAccount`（探 usage_url 验 Ark API Key + 可选 AK/SK 经签名 GetAFPUsage），其余（含 kimi-code）探 usage_url。落盘后 best-effort reload；reload 失败（config.yaml 不可读/非法，非本次操作所致）时账号已存盘，响应带 `warning`，runtime 保持旧集直到 config 修复并 reload |
| DELETE | `/api/accounts/<provider>/<id>` | — | `{status:"removed",warning?}` | apikey 类 `removeApikeyAccount`；aqp `provider.ClearAqpAccount`；codex `os.Remove`。落盘后 best-effort reload；失败同上，响应带 `warning` |
| POST | `/api/accounts/<provider>/<id>/test` | — | `{status:"ok"\|"failed",http_status,reason,latency_ms,provider,account_id,model}` | 账号粒度测活（probeModelCallable 真实最小请求，复用 provider 的 ProbeRequest/ExtraHeaders）；`proxyAdminCommands` 一次 `snapshotRuntime` 同代捕获 config/impl，模型取该 provider 首个路由目标否则 models[0]；只读不 reload，请求取消会取消上游 probe。UI 账号卡片 Test 按钮 |
| GET | `/api/tokens` | — | `{usage:[{provider,model,input,output,cache_creation,cache_read,requests}]}` | SSE 扫描器累计的观测用量（flat 数组） |
| POST | `/api/tokens/reset` | — | `{status:"reset"}` | 清零内存 + SQLite + flusher 基线（注意：也清空响应缓存） |
| GET | `/api/stats?from=&to=&provider=&model=&bucket=` | — | `{from,to,bucket,buckets:[...]}` | 存储 1 分钟桶；`bucket` 仅展示聚合（SQL GROUP BY）。buckets 含 `avg_latency_ms`/`avg_ttft_ms` |
| GET | `/api/agents?from=&to=&agent=&provider=&model=&bucket=` | — | `{from,to,bucket,buckets:[...]}` | per-(agent,provider,model) 桶（`agent_buckets`：requests/input/output/latency_ms_sum/failures） |
| GET | `/api/requests?model=&provider=&status=&errors=&from=&to=&limit=&shadow=` | — | `{enabled,records:[summary…]}` | request_log 查询（未启用 → `{enabled:false}`）。summary 含 `shadow` 布尔；`shadow=only\|exclude` 过滤影子记录。流式扫描（bufio.Scanner），limit 默认 100 上限 1000 |
| GET | `/api/requests/<id>` | — | 完整 record（含 request/response body） | replay 的数据源；影子记录 id 为 `shadow-<原id>` |
| GET | `/api/shadow-report?from=&to=` | — | `{from,to,entries:[{route,primary_provider,shadow_provider,samples,status_match_rate,primary_latency_ms,shadow_latency_ms,latency_diff_ms,primary_size_avg,shadow_size_avg}]}` | 影子评测聚合（按 `shadow-<父id>` 配对，仅成对样本计入） |
| GET | `/api/fusion?workflow=` | — | `{workflows:{<名>:{runs,runs_today,quorum_met,degraded{原因:次数},panel_input/output,judge_input/output,synth_input/output,amplification}},runs:[{run_id,ts,route,workflow,agent,proto,quorum,drafts_used,degraded,legs[{provider,model,kind,status,latency_ms,input,output,err,cut}],judge_used,synth_committed,synth_status,synth_latency_ms,synth_input,synth_output}]}` | 编排观测（`fusionRegistry` 纯内存，200 条 run 环形新到旧；汇总数据从 eventHub 回读，**不依赖 request_log**）。degraded 原因：`insufficient_proposers`/`tools_unsupported`/`body_build_failed`/`budget_exceeded`/`multi_turn`；`amplification`=(候选+judge+汇总)/汇总 token。时间序列走 `("fusion",<workflow>)` 分钟桶（requests=编排次数、failovers=降级次数） |
| GET | `/api/events` | — | SSE 流 | 实时请求监视：先重放 200 条 recent ring 再推 start/end 事件（含 request_id/agent/provider/status/latency/tokens），15s keepalive。挂在主 mux（不受 web.enabled 控制） |
| GET/POST/DELETE | `/api/pin` | POST `{route,provider,ttl?}` | `{pins:[...]}` / `{status:"pinned"}` / `{status:"unpinned"}` | 运行期 pin（见 `docs/architecture/runtime-state.md`）；GET 列表、DELETE `{route}` 清除 |
| GET | `/api/analytics?from=&to=&provider=&model=&granularity=day\|month` | — | `{granularity,from,to,series:[{provider,model,points:[{bucket,requests,input,output,cache_creation,cache_read,cost,priced}]}],totals:{input,output,cost},price_coverage:{priced:[],unpriced:[]}}` | 日历日/月聚合 + **服务端现算等价 payg 成本**（见下「Analytics 等价成本」） |
| POST | `/api/quota/refresh` | 空 body 或 `{"provider":key}` | `{status:"refreshed"[,provider]}` / 404 | 同步刷新配额缓存（可立即重查 `/api/status`）：空 → `pollAll`，指定 → `pollOne`（key 即 `name` 或 `name#accountID`），未知 key 404 |
| POST | `/api/health/reset` | 空 body 或 `{"provider":key}` | `{cleared:[names],model_locks_cleared:n}` | 清冻结运行态（熔断开路冷却、429 限频冷却、模型锁定），目标立即重试；空=全部，池化父名清全部虚拟；**不清** sticky/pin/剥参 blocklist。`unfreeze` CLI 与 UI Providers 卡 unfreeze 按钮 |
| POST/GET | `/api/login/<provider>/start`、`/api/login/<session>/poll` | — | `{session_id,...}` / `{state, detail, result, warning?}` | 异步登录（aqp SSO URL / codex device flow）；poll 状态 pending/done/error。轮询由 `webTaskOwner` 管理，关闭时取消 HTTP/等待；凭据 commit 前响应取消，进入 commit 后完成 save+reload 再退出。done 时若 reload 失败，`warning` 非空（凭据已落盘，runtime 旧） |

**写操作统一热重载**：所有 mutation 落盘后触发进程内 `proxy.reload` —— 同一 worker 进程原地换 cfg/providers，不重启。账号增删虽不改 config.yaml，但 reload→`buildProviders`→`loadPool` 重读池文件，新账号随即展开成虚拟。reload 还清空 `health`/`sticky`/`spreadCtr`、重建响应缓存，并 kick `quota.pollAll`。注意：进程内 reload（UI 与 worker 同进程）≠ `serve reload`（给独立进程发 SIGHUP）。

**reload 失败不是静默成功**：`reload` 仅在 `config.yaml` 自身不可读/非法时失败（mutation 写的是池文件，不是 config.yaml，所以正常操作不会触发）。失败时凭据已落盘、不可撤销，故仍返回 2xx，但响应带 `warning`（错误原文）并记一行 `[accounts] reload after mutation failed` 日志；runtime 保持旧集直到 config 修复并下次 reload（普通请求不会重读池文件）。前端在 add/remove 模态框和登录 done 状态展示该 warning。config 编辑走 `saveAndReload`，先校验、失败从备份回滚并返回 400（不同于账号增删的 best-effort）。

`serve status`（CLI）是 `/api/status`+`/api/tokens`（带 `--logs` 再加 `/api/logs`）的终端消费者，内容与 Status 标签页一致（Providers 表含 LAT/TTFT 列）。

## SSE token 扫描器（`tokens.go`，forward 2xx 提交处接入）

`usageScanner` 是 `io.ReadCloser`，仅当 `isSSE(resp.Header)` 包在 `resp.Body` 外，字节**原样透传**（不修改/缓冲/阻塞）；失败静默。bounded 64KB 行缓冲（防 OOM）。commit-on-EOF/close（含客户端断开，`forward` 在 `flushCopy` 后显式 `body.Close()`）。解析只看 `data:` + 首字符 `{` 的行：anthropic shape（`message_start`→input/cache、`message_delta`→output）或 openai shape（`prompt_tokens`/`completion_tokens`，best-effort——`usage` 仅当客户端发 `stream_options.include_usage` 才有；协议转换路径由转换器注入）。`tokenCounter` 纯内存；持久化由 SQLite stats 接管。其内嵌 `mu` 是独立叶子锁，不与 `p.mu`/`healthMu`/`quotaMu` 嵌套。

## 调用统计持久化（`stats.go`，SQLite 单一来源）

`metricsStore`（per-(provider,model) 原子计数器）+ `tokenCounter` 在 hot path 纯内存，**hot path 不碰 SQLite**。`statsFlusher` 按墙钟分钟边界 tick，快照两者与上次 diff，非零 delta 作分钟桶 upsert 到 `~/.model-proxy/stats.db`（`minute_buckets` 表，`ON CONFLICT DO UPDATE` 累加，`last_request_at` 用 `MAX`）。`modernc.org/sqlite` 纯 Go（`CGO_ENABLED=0`）；`config.stats.{db_path, retention}`（默认 30d）。SIGINT/SIGTERM 最终 flush + pid 清理。查询 `GET /api/stats`（`bucket` 聚合，存储恒 1 分钟）+ `model-proxy stats` CLI。锁纪律：metrics/token/stats 都是独立叶子锁，不与其它嵌套。

**延迟与失败口径**：`minute_buckets` 带 `latency_ms_sum`/`ttft_ms_sum`（additive ALTER 迁移，老库自动加列）。stats 延迟用**上游响应头到达**时间（`upstreamMs`，不含客户端慢读；live 事件的 latency 仍是客户端口径）。`evRequests` **只在 commit +1**（失败尝试只计 failovers/failures，不稀释 avg）。`statsFlusher.flush()` 全周期持 `f.mu`，`resetAll` 同锁（否则分钟边界撞 reset 会把全量历史写回刚清空的库）。

**agent 维度**（`agent.go`）：`detectAgent` 从 `x-claude-code-session-id`/`claude-cli` UA → `claude-code`、UA 含 `codex` → `codex`、`opencode`、`pi/` 前缀 → `pi`（无 UA=`unknown`、其余=`other`）。并行管线 `agentCounter`（key=(agent,provider,model)，独立叶子锁）→ `agent_buckets` 表（requests/input/output/`latency_ms_sum`/`failures`，同样 additive 迁移）。token 归因经 usageScanner 的 agentSink（非 SSE 无 token 只计 requests）；全失败 502 记 `incRequests`+`incFailure`（到首个尝试目标）。查询 `GET /api/agents` + `stats --by-agent`（`--agent/--provider/--model` 过滤）+ UI Status 页 Agents 卡片。

## Analytics 等价成本（`internal/pricing` + `web.go:handleAnalytics`，默认开启）

`GET /api/analytics?from=&to=&provider=&model=&granularity=day|month` 在 SQLite stats 之上做**日历日/月聚合**（存储恒 1 分钟）：`queryAnalytics` 用 SQL `date(minute,'unixepoch','localtime','start of day'/'start of month')` GROUP BY；bucket = 本地时区自然日/月初的 unix instant，由 `localDayStart` 经 `time.ParseInLocation(...,time.Local)` 转——不用 `strftime('%s',…)`（会把本地日期误读为 UTC 当天 0 点，偏移一个时区）。每个 point 现算**等价 payg 成本**：price × tokens，**不落盘、不伪造**；未知价 → `cost:null, priced:false`。响应：`{granularity, from, to, series:[{provider, model, points:[…]}], totals:{input, output, cost}, price_coverage:{priced:[], unpriced:[]}}`。

**价格优先级**（`pricing.Resolve`）：config `prices:` 在 `proxyReadView.pricing()` 边界复制并转换为 USD/M override，查询时 ÷1e6 转 USD/token；命中后覆盖 `pricing.Catalog`（OpenRouter 目录，bare-name 精确匹配，无 endpoint/后缀模糊匹配）。OpenRouter 目录在 parse 期按 vendor rank 去重（canonical vendor 胜出，如 `deepseek/deepseek-v4-pro` 击败 `openrouter/deepseek-v4-pro`；tilde 别名 `~openai/gpt-5.6-luna` 剥成 bare 名 `gpt-5.6-luna`）。`pricing.ComputeCost`：`input×Prompt + output×Completion + cacheRead×CacheRead + cacheCreation×CacheWrite`。

**新配置**（`internal/config`）：
- 顶层 `pricing:{enabled, ttl, source_url}` — 默认 `enabled:true` / TTL `24h` / `source_url` 默认 `https://openrouter.ai/api/v1/models`（`pricing.DefaultEndpoint`）。`enabled:false` → `pricingSnapshot` 返回 nil 且不抓取目录；显式 `prices:` override 仍可定价，未命中 override 的模型显示 n/a。
- 顶层 `prices:` map — per-model override，单位 **USD per MILLION tokens**（人类单位）；字段 `input`/`output`/`cache_read`/`cache_write`（后两者默认 0）。命中即盖过目录。

**缓存**（`internal/pricing`，镜像 models.dev 模式）：`~/.model-proxy/pricing_cache.json`，TTL 默认 24h（`pricing.DefaultTTL`，可被 `pricing.ttl` 覆盖）。`pricing.EnsureFresh`：fresh → 用；stale → conditional GET（带 `If-None-Match`）；304 → 只刷 `fetched_at` + 持久化；200 → 重建 + 持久化。持久化使用目标目录中的唯一临时文件再 atomic rename，多个进程不会争用固定 `.tmp`，读取者只观察完整 JSON。抓取失败：有旧 → 用旧 + stderr 告警；无 → 空 catalog（未知价显示 n/a，不阻塞 UI）。

**环境变量 `MP_PRICING_URL`**：pricing 端点解析优先级 **config `pricing.source_url` > `MP_PRICING_URL` env > OpenRouter 默认**（`pricing.DefaultEndpoint`）。`PricingConfig.ResolvedSourceURL()`（`internal/config`）在 config 未设时回落到环境变量，**镜像 `MP_MODELSDEV_URL`** 的 test/mirror override 语义，生产路径 `Proxy.pricingSnapshot`（`proxy.go`）经此生效。单测 `internal/config` 的 `TestPricingConfigSourceURL_Precedence` 钉死 config > env > default 三级优先级；目录、缓存与计算测试归属 `internal/pricing/pricing_test.go`。

**Web UI**：`/ui/` Analytics 标签页（`web_assets/`）消费 `/api/analytics`，渲染 token + 等价成本趋势（uPlot）。未定价模型（如 `doubao-*`）显示 `n/a` + UI 提示。

**CLI**：`stats --granularity day|month` 或 `--cost` 任一 → `renderAnalytics` 改打 `/api/analytics`（`formatAnalyticsTable`：每 (provider,model) 一行 = 窗口内 SUM，`--cost` 才出 cost 列，未定价 `n/a`；`--json` 原样）。两者都省略 → 走 `/api/stats`，输出与原 `stats` **字节一致**（CLI 契约不变，append-only）。

**锁与依赖纪律**：`Proxy.pricingMu` 独立叶子锁；`pricingSnapshot`/`priceOverrides` 经 `cfgSnapshot()` RLock 读 cfg，不持 `p.mu` 调入。`internal/pricing` 不依赖 main 包的 YAML 配置、Proxy 或 Web；`proxyReadView.pricing()` 是 `PriceConfig → pricing.Override` 的复制/单位边界。无 stats store → `series:[]`（nil-safe）。

## Web 运行时边界

`webServer` 不持有 `*Proxy`；构造函数只把它转换为两个具体 capability 后即丢弃：
只读 handler 通过 `proxyReadView` 查询 request log、tokens、stats、Fusion、
pins、pricing 及 detached dashboard/config/provider 快照，写操作和主动网络探测
通过 `proxyAdminCommands` 执行 reset、quota refresh、health reset + persist、
pin、reload 与 account probe。`web.go` 不得绕过这两个端口直接访问 Proxy。

账号测活必须在 admin capability 内只调用一次 `snapshotRuntime`，从同一 generation
取得 config 和 provider implementation；凭据文件检查及上游网络 I/O 在快照完成、
锁已释放后执行。`proxyReadView` 不提供按名字单独读取 runtime provider 的入口，
避免 reload 期间把旧 config 与新 impl 混用。

Web 后台工作只能经 `webTaskOwner.run` 接纳；`web.go` 不允许裸 `go`。owner 同时
管理 login-session GC 与 AQP/Codex 轮询，关闭时在同一 mutex 内停止 admission、
取消 root context 并等待任务。GC 只删除超过 TTL 的 done/error 会话，不能删除
仍 pending 的会话；否则 UI 会在后台任务仍可能落盘时提前得到 404。

## 请求访问日志（`request_log.go`，JSONL 文件，默认关闭）

记录每个 commit 的 upstream 调用的**完整 request+response body** + 元数据，逐行 JSON 写轮转文件，供离线分析（`jq`/`grep`）与查询 API。**默认 `enabled: false`**（零开销：不 wrap、不开文件、不起 goroutine）；改 `enabled` 需**重启**（reload 不重建 logger）。

热路径：`p.reqLog != nil` 时 `resp.Body`（**协议转换后**的字节）包 `internal/transport/bodycapture.Reader`（有界 tee，`max_body_bytes` 封顶，超限停捕获但字节仍透传）→ 非阻塞 `select` enqueue 到 buffered chan（cap 2048，满则计数降频 log，**绝不阻塞 forward**）。`request_id` 恒生成（启动 nonce + 原子计数器，live 事件同用）。单 goroutine drain 写文件；写失败计 `writeErrors` + 降频 log；`MkdirAll` 失败置 `dead`。轮转：超 `max_file_size`（默认 1G）或自然日变更时归档为 `requests-<start>--<end>-<seq>.log`，空文件不归档。retention（默认 30d）按 mtime 删归档文件，**活跃文件永不删**（启动/每小时/关停三次 sweep 回收上次遗留的活跃文件）。SIGINT/SIGTERM 排空 chan + 写完 + sweep + 关文件。文件 `0o600`、目录 `0o700`（含用户 prompt）。只记 commit 响应（2xx + 非 failover 4xx）；failover 中间尝试与 all-failed 502 不记。影子记录带 `shadow:true` + `request_id: shadow-<原id>`（见 `docs/architecture/fusion-shadow-cache.md`）。记录的 `request_body` 保存原始客户端请求体（replay 保真用；改写/转换前的）。`config.request_log.{enabled, dir, max_file_size, max_body_bytes, retention}`。

**查询 API/UI**：`GET /api/requests`（过滤 model/provider/status/errors/from/to/limit/`shadow=only|exclude`，流式 bufio.Scanner 逐行扫，单文件不再 GB 峰值）+ `GET /api/requests/<id>`（完整 body）+ Web UI Requests 标签页（过滤行 + 影子徽标 + 点击展开详情）。
