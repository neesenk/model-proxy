# AGENTS.md - model-proxy 架构与后端契约参考

model-proxy 的架构/契约权威参考：关键设计、各后端契约（逆向实测）、踩过的坑。CLAUDE.md 指向此处。改了实现就同步更新本文件。

## model-proxy 实现经验

### 架构：Provider 抽象 + 协议路由

```
provider/                    # Provider 实现（每个上游一个文件）
  provider.go                # 接口 + 注册 + New()
  apikey.go                  # ApiKeyBase（共享 auth file + Bearer 注入）
  fetch_models.go            # fetchModelsBearer（OpenAI 风格 /models 共享 helper）
  probe.go                   # baseProbe（ProbeRequest/ExtraHeaders/FilterModelIDs 默认实现）+ body 构造 + newRequestID
  auth.go / display.go       # auth 注入器（AqpKeyProvider/CodexOAuthProvider）+ 颜色/格式 helper
  aqp.go / codex.go          # SSO+monthly_usage / OAuth device flow + wham/usage + store:false
  zhipu.go / deepseek.go / volcengine.go / kimi_code.go / static.go
config.yaml:
  providers:                 # openai_base_url + provider_id + models（凭据不落 config）
  claude_mapping:            # anthropic-only：claude 别名 → 对外模型名（路由前先翻译）
  routes:                    # 对外模型名 → provider/真实名 目标列表（不按协议）
  scheduling:                # 可选（durations 用字符串；省略则用 config.go 的代码默认）
```

对外协议 = 转发协议（不做转换）。凭据由 `login <provider>` 管理，存 `~/.model-proxy/<name>_<suffix>.json`。

熔断/限频/粘性状态在 `Proxy.health`（`healthMu`，与 reload 的 `mu` 分开）。`schedule` 跳过开路/限频 provider；`tryTarget` 在超时/5xx/conn-error 计熔断、429 记限频（Retry-After 或默认退避）、成功清零；半开用 `halfOpenInFlight` 单飞。粘性：每路由记 current provider + since，驻留窗口内优先它（保 cache）。

**Provider 接口**（`provider/provider.go`）：`AuthHeaders`/`Refresh`/`RewriteRequest`/`Logout`/`Usage`/`FetchModels`/`Quota`/`ProbeRequest`/`ExtraHeaders`/`FilterModelIDs`（`Surplus` 在 `QuotaSnapshot` 上，非接口方法）。后三者承载 provider 专属探测/过滤/请求头知识，**绝不放在 main 包的 `if prov.Provider == ...` 分支**；默认实现集中在 `baseProbe`（每个 provider embed）：
- `ProbeRequest` — 默认 OpenAI `POST /chat/completions`；aqp/kimi-code override `/v1/messages`+anthropic body，codex override `/responses`+Responses API body。
- `ExtraHeaders` — 默认 no-op；aqp override 设 `anthropic-version` + UUID `x-compass-request-id`；kimi-code override 设 `anthropic-version: 2023-06-01`。
- `FilterModelIDs` — 默认透传；volcengine override 剔除 `*-latest`/`doubao-seed-1-*`/lite/mini。

**provider 自实现**：`Auth`/`Logout`/`Usage`/`Quota` 都由 provider struct 承载，fetch+parse 全在 provider 内完成。main 只剩一个窄回调 `FetchModelsFn`（volcengine 的 V4 签名 `ListArkAgentPlanModel`）；`buildOne`（`proxy.go`）的 switch 只剩它 + per-provider 配置字段。aqp 的 account store 在 `provider/aqp_store.go`；main 的 `AqpClient`（SSO login/web 编排，cookie jar）经 `provider.*` 访问。`login` 由 `cmdLogin` 直派 `run*`（只读 config，不构造 provider 实例；Login 已从接口移除）；`logout` 池路径直接操作池文件，单文件路径（aqp/codex/legacy 单数文件）经 `buildProviders` 构造实例调 `Logout()`。

### 配额感知调度（quota-aware scheduling）

`Provider.Quota()` 把每个上游用量端点解析成归一化的 `provider.QuotaSnapshot{Billing, RemainingPct, Windows, ...}`。各 provider 来源：

| provider_id | 来源端点 | Billing |
|---|---|---|
| zhipu | `quota/limit`（5h/周 token + 月度时间） | plan |
| codex | `wham/usage`（primary/weekly 窗口 + spend） | plan |
| volcengine | `GetAFPUsage`（V4 签名，需 AK/SK） | plan |
| aqp | `monthly_usage`（月度 ratio/balance） | plan |
| kimi-code | `/usages`（weekly=Ultimate + 5h=Short + extra-usage 钱包，Bearer） | plan |
| deepseek | `/user/balance`（按量余额） | pay-as-you-go |

`RemainingPct` = 该 provider **最终窗口**（总预算）的 remaining%：zhipu/kimi-code=周、volcengine/codex/aqp=月、deepseek=payg（无窗口）。`QuotaWindow` 带 `Ultimate`/`Short` 标记 + `Duration`。短窗口（5h 等）是 rate-cap，**不参与 min**（用满即 429，反应式跳过）；zhipu `TIME_LIMIT`（MCP 工具配额）与 kimi-code `boosterWallet`（钱包）不参与（只展示，`RemainingPct=-1`）。

> **多窗口共识规则**（kimi-code 起确立）：多时间窗口限额时，**最长周期** = `Ultimate`（硬性总预算，调度基数 + 节奏源），**更短**的 = `Short`（软性 rate-cap）。kimi-code parser 取 `Duration` 最大者为 Ultimate、其余 Short（非硬编码，新窗口自动适配）；zhipu 是硬编码 unit switch（3=5h→Short、6=weekly→Ultimate），新 unit 两个标记都没有。CLI `usage` 显示：kimi-code 的 token 窗口用 0–100 抽象刻度（百分比），不打印绝对 `Usage:` 行、只有 money 窗口打印绝对值；zhipu 走共享 `printQuotaSnapshot`，每个 `Total>0` 窗口都打印绝对 `Usage:` 行。显示顺序：Short 在前，Ultimate 在后。

**`quotaTracker`**（`quota.go`）：后台 goroutine 每 `quota_poll_interval`（默认 5m）并行轮询所有 `Quota()`，内存缓存 + 原子落盘 `~/.model-proxy/quota_state.json`（启动加载为基线；**携带每路由 `sticky` map，重启恢复**）。`NewProxy` 启动；`reload` 不重建（经 cfg/providers 快照闭包读新配置），只 kick 一次 `pollAll`；429 触发该 provider 异步 `refreshOne`。陈旧保护：snapshot 老于 `3×poll_interval` 视为 `BillingUnknown`。锁 `quotaMu`（独立于 `healthMu`/reload `mu`）；**锁顺序 `healthMu` → `quotaMu`**。

**调度分 = surplus**（`(*QuotaSnapshot).Surplus(now, peakMult)`）：`surplus = (ultimate.remaining − short.remaining × (short.total/ultimate.total) × (peakMult−1)) − clamp((ultimate.reset−now)/ultimate.duration, 0,1)`。>0 落后节奏（优先用）；<0 超前（回避）。`schedule()`（`decideOrder()` + 提交 sticky）把可用目标按 `(tierRank, priority asc, surplus desc)` 排序 —— **priority 压过 surplus，surplus 只在同 priority 内破平**：

- **`tierRank`**：`plan(0) < unknown(1) < payg(2)` —— `BillingClass` iota（`Unknown=0,Plan=1,PayG=2`）≠ 调度序，故单独映射；pay-as-you-go 严格兜底。
- **peak 只走短窗口折算**：`peakMult` 只烧短窗口项。无同单位短窗口的 provider（codex/compass money-ultimate；未轮询）**高峰不打折**；multiplier=1 关闭。
- **粘性切换**（cache 友好）：路由停 current provider 一个 `sticky_dwell`（默认 10m）；到期后仅当最优者在 **tier → priority → surplus 边际（`quota_switch_margin`，默认 15 pts）** 任一更优时才换。
- **重复 priority 合法**：同路由多 target 同 priority 构成 surplus 竞争池（高 surplus 胜，边际更优才切）；`config.go:validate` 不拒绝。

**可见性**：`GET /debug/schedule`（只读 peek，不改 sticky）；`model-proxy schedule`（CLI，查该接口，需 daemon）；`model-proxy doctor`（离线 config 诊断）。

**新配置字段**：provider 级 `billing: pay-as-you-go`（默认 plan）；多段 `peak_hours`；`scheduling.quota_poll_interval`/`quota_switch_margin`。`sticky_dwell` 同时是切换前最小驻留。

### 多账号凭据池（credential pool）+ session-sticky 路由

一个 config provider 可挂多账号（`login <provider>` 重复录入）。账号存**复数池** `~/.model-proxy/<name>_apikeys.json`；旧单数 `<name>_apikey.json` 是只读 fallback（包成 1 条池）。`accountIDFor`（`pool.go`）：volcengine = `access_key`（为空时回落 sha256），其余 apikey = `sha256(api_key)[:16]`。`login` 按 id 去重（`--replace`/交互确认；`--label` 命名；写完 SIGHUP 热重载）。**aqp/codex 不池化**；zhipu/deepseek/volcengine/kimi-code 池化。

**构建期展开**（`buildProviders`）：≥2 账号池展开成 N 个虚拟 provider，key = `name#<accountID>`（单账号仍是 `name`）。每个虚拟共享父 config，但三处绑各自凭据：① 内嵌 `ApiKeyBase`（forward `AuthHeaders`，经 `BoundAPIKey`，**不是 `cfg.Auth`**）；② `cfg.Auth`（FetchModels）；③ Usage/Quota 闭包。路由 target 经 `buildExpandedRoutes` 展开成 N 个虚拟；per-account 熔断/429 跳过/配额/failover 全白送。

**池内路由 = session-sticky**：sticky key 从「路由名」换成请求的 `x-claude-code-session-id`（无则退回路由名）。新 session 由 per-parent 计数器（`spreadCtr`）轮询分配到一个账号；该对话整场停在该账号（仅 429/熔断重选，不中途迁到边际更优账号）→ 保 cache；不同对话分流。无 `strategy` 配置。session-keyed sticky **不落盘**（`snapshotSticky` 只持久化路由名 key）。

**踩过的坑**：① 虚拟必须经 `BoundAPIKey` 绑内存 key，否则读不存在的 `<name#id>_apikey.json` → 502；② `Refresh()` 在 bound 实例是 no-op（bound key 不可变）；③ **1 条池也必须 bind**（否则 `login` 后 502 —— `buildProviders` 用 `os.Stat(poolPath)` 区分「复数池存在」与「单数 fallback」）；④ volcengine 每账号要完整 `{api_key, access_key, secret_key}`，`(*VolcengineProvider).resolveAKSK`（provider 包）对非 nil cred **排他**（不回落文件，防泄漏兄弟账号 AK/SK）；⑤ volcengine `FetchModels` 暂未按账号绑，池化时 `models refresh` 复用首个 virtual 凭据，探测全失败退回合并集不写空。

### Token 文件命名

| provider_id | suffix | 文件名 |
|---|---|---|
| aqp / codex | oauth_auth | `~/.model-proxy/<name>_oauth_auth.json` |
| zhipu / deepseek / kimi-code | apikey | `~/.model-proxy/<name>_apikey.json`（单一 `{api_key}`）|
| volcengine | apikey | `~/.model-proxy/<name>_apikey.json` — `{api_key, access_key, secret_key}` |

路径从 provider name（config 一级 key）派生，支持多实例（如 `zhipu-personal` / `codex-work`）。多账号见上「凭据池」。所有路径（CLI `login`、`buildOne`、web 异步登录、`logout`）一律用 config name，**包括 aqp/codex**（`runLogin`/`cmdCodexLogin` 接收 `provName` → `authFilePath(provName, "oauth_auth")`）；曾有的「CLI login 硬编码 provider_id → 非同名实例读写错位」bug 已修，`TestAqpCodexLogin_UsesConfigNameForAuthFile` 守护。

### CLI 命令

```
login <provider> [--label NAME] [--replace]   # aqp: SSO / codex: device flow / apikey 类: 输入 key（可重复 → 多账号池；写完热重载）
logout <provider> [--label NAME | --all]       # 删一个账号（默认交互式）/ 清空池
usage [provider]          # 省略则展示全部；池化时逐账号展示
models [provider]         # 从 config 列模型
models refresh <provider> # 从服务端刷新 + endpoint 探测；无 /models 时回退探测路由模型
models pull               # 强制刷新 models.dev 元数据缓存
serve [daemon|stop|reload|status]
takeover/restore <client>
config init|print|check
schedule                   # 查 daemon：每 model 当前调度（GET /debug/schedule）
doctor                     # 离线 config 调度诊断
serve status               # 终端状态面板（= Web UI Status 标签页）；--logs [N]/--json/--config
stats                      # per-(provider,model) 调用统计（SQLite）；--from/--to/--provider/--model/--bucket/--json；--granularity day|month|--cost 改走 /api/analytics（见下）
```

### Web UI + `/api/*` 接口契约

`web.enabled`（默认 true）时 daemon 同一 mux 挂 `/ui/`（embed 静态资源）和 `/api/`（JSON）。**无鉴权**；loopback 仅靠默认 `listen: 127.0.0.1:15721`，改 `0.0.0.0` 即无鉴权暴露 `/api/*`。

**前端布局契约**：Status→Logs 每条日志是「行号 gutter + 正文」两列网格；行号与 gutter 右边框留 2px，gutter 背景只覆盖行号列，鼠标悬停标出整条逻辑行，单击选中该行（改变行号前景色，不干预原生选择）。Config→Raw YAML **硬最小高度 480px**，按编辑器 viewport top + 卡片下方 chrome 重新计算，可见空间大于 480px 铺满、不足仍 480px 并允许滚动，绝不靠固定 `100vh - 常量` 推测。

| 方法 | 路径 | 请求 | 响应 | 备注 |
|---|---|---|---|---|
| GET | `/api/status` | — | `{uptime,version,listen,health{...},quota{...},schedule{...},counters{...},warnings}` | 锁 `p.mu`→`healthMu`→`quotaMu` 顺序不嵌套。`quota` 是 `QuotaSnapshot` 原样序列化（无 json tag → **PascalCase**） |
| GET | `/api/logs?tail=N` | — | `{lines:[…]}` | 读 log 文件末尾 N 行（默认 200，上限 1000）；无 log 路径 → 404 |
| GET | `/api/config` | — | `{yaml, summary, provider_models, routes}` | 原文件 verbatim round-trip |
| POST | `/api/config` | `{yaml}` | `{status:"reloaded"}` / 400 | `saveAndReload`：validate → backup `back/<base>.<ts>.bak` → atomicWrite → reload。校验失败不落盘；reload 失败从当次备份回滚 |
| POST | `/api/config/edit` | `{kind,name,data}` | `{status:"reloaded"}` / 400 | 结构化编辑 `kind∈{general,scheduling,provider,route,claude_mapping}`，改 `yaml.Node`（保留注释/键序）→ `saveAndReload` |
| GET | `/api/accounts` | — | `{providers:[{name,provider_id,billing,accounts:[{id,label,added_at,[email]}]}]}` | **响应无任何 key 字段**（无法泄漏）；id 不掩码（UI 要用它删） |
| POST | `/api/accounts/<provider>` | `{api_key, access_key?, secret_key?, label?, replace?}` | `{id,status:"added"}` | 仅 apikey 类；aqp/codex 返 400 指向 async login。volcengine 走 `addVolcengineAccount`（探 usage_url 验 Ark API Key + 可选 AK/SK 经签名 GetAFPUsage），其余（含 kimi-code）探 usage_url。best-effort reload |
| DELETE | `/api/accounts/<provider>/<id>` | — | `{status:"removed"}` | apikey 类 `removeApikeyAccount`；aqp `provider.ClearAqpAccount`；codex `os.Remove`。best-effort reload |
| GET | `/api/tokens` | — | `{usage:[{provider,model,input,output,cache_creation,cache_read,requests}]}` | SSE 扫描器累计的观测用量（flat 数组） |
| POST | `/api/tokens/reset` | — | `{status:"reset"}` | 清零内存 + SQLite + flusher 基线 |
| GET | `/api/stats?from=&to=&provider=&model=&bucket=` | — | `{from,to,bucket,buckets:[...]}` | 存储 1 分钟桶；`bucket` 仅展示聚合（SQL GROUP BY） |
| GET | `/api/analytics?from=&to=&provider=&model=&granularity=day\|month` | — | `{granularity,from,to,series:[{provider,model,points:[{bucket,requests,input,output,cache_creation,cache_read,cost,priced}]}],totals:{input,output,cost},price_coverage:{priced:[],unpriced:[]}}` | 日历日/月聚合（存储恒 1 分钟）+ **服务端现算等价 payg 成本**（price×tokens，不落盘、不伪造；未知价 `cost:null,priced:false`）。价格优先级：config `prices:` > OpenRouter 缓存目录（bare-name 精确匹配）。见下「Analytics 等价成本」 |
| POST | `/api/quota/refresh` | 空 body 或 `{"provider":key}` | `{status:"refreshed"[,provider]}` / 404 | 同步刷新配额缓存（可立即重查 `/api/status`）：空 → `pollAll`，指定 → `pollOne`（key 即 `name` 或 `name#accountID`），未知 key 404 |
| POST/GET | `/api/login/<provider>/start`、`/api/login/<session>/poll` | — | `{session_id,...}` / `{state, detail, result}` | 异步登录（aqp SSO URL / codex device flow）；poll 状态 pending/done/error |

**写操作统一热重载**：所有 mutation 落盘后触发进程内 `proxy.reload` —— 同一 worker 进程原地换 cfg/providers，不重启。账号增删虽不改 config.yaml，但 reload→`buildProviders`→`loadPool` 重读池文件，新账号随即展开成虚拟。reload 还清空 `health`/`sticky`/`spreadCtr` 并 kick `quota.pollAll`。注意：进程内 reload（UI 与 worker 同进程）≠ `serve reload`（给独立进程发 SIGHUP）。

`serve status`（CLI）是 `/api/status`+`/api/tokens`（带 `--logs` 再加 `/api/logs`）的终端消费者，内容与 Status 标签页一致。

#### SSE token 扫描器（`tokens.go`，forward 2xx 提交处接入）

`usageScanner` 是 `io.ReadCloser`，仅当 `isSSE(resp.Header)` 包在 `resp.Body` 外，字节**原样透传**（不修改/缓冲/阻塞）；失败静默。bounded 64KB 行缓冲（防 OOM）。commit-on-EOF/close（含客户端断开，`forward` 在 `flushCopy` 后显式 `body.Close()`）。解析只看 `data:` + 首字符 `{` 的行：anthropic shape（`message_start`→input/cache、`message_delta`→output）或 openai shape（`prompt_tokens`/`completion_tokens`，best-effort——`usage` 仅当客户端发 `stream_options.include_usage` 才有）。`tokenCounter` 纯内存；持久化由 SQLite stats 接管。其内嵌 `mu` 是独立叶子锁，不与 `p.mu`/`healthMu`/`quotaMu` 嵌套。

#### 调用统计持久化（`stats.go`，SQLite 单一来源）

`metricsStore`（per-(provider,model) 原子计数器）+ `tokenCounter` 在 hot path 纯内存，**hot path 不碰 SQLite**。`statsFlusher` 按墙钟分钟边界 tick，快照两者与上次 diff，非零 delta 作分钟桶 upsert 到 `~/.model-proxy/stats.db`（`minute_buckets` 表，`ON CONFLICT DO UPDATE` 累加，`last_request_at` 用 `MAX`）。`modernc.org/sqlite` 纯 Go（`CGO_ENABLED=0`）；`config.stats.{db_path, retention}`（默认 30d）。SIGINT/SIGTERM 最终 flush + pid 清理。查询 `GET /api/stats`（`bucket` 聚合，存储恒 1 分钟）+ `model-proxy stats` CLI。锁纪律：metrics/token/stats 都是独立叶子锁，不与其它嵌套。

#### Analytics 等价成本（`pricing.go` + `web.go:handleAnalytics`，默认开启）

`GET /api/analytics?from=&to=&provider=&model=&granularity=day|month` 在 SQLite stats 之上做**日历日/月聚合**（存储恒 1 分钟）：`queryAnalytics` 用 SQL `date(minute,'unixepoch','localtime','start of day'/'start of month')` GROUP BY；bucket = 本地时区自然日/月初的 unix instant，由 `localDayStart` 经 `time.ParseInLocation(...,time.Local)` 转——不用 `strftime('%s',…)`（会把本地日期误读为 UTC 当天 0 点，偏移一个时区）。每个 point 现算**等价 payg 成本**：price × tokens，**不落盘、不伪造**；未知价 → `cost:null, priced:false`。响应：`{granularity, from, to, series:[{provider, model, points:[…]}], totals:{input, output, cost}, price_coverage:{priced:[], unpriced:[]}}`。

**价格优先级**（`resolvePrice`）：config `prices:`（USD/M tokens，查询时 ÷1e6 转 USD/token）> `pricingCache`（OpenRouter 目录，bare-name 精确匹配，无 endpoint/后缀模糊匹配）。OpenRouter 目录在 parse 期按 vendor rank 去重（`canonicalORVendors` rank 0 胜出，如 `deepseek/deepseek-v4-pro` 击败 `openrouter/deepseek-v4-pro`；tilde 别名 `~openai/gpt-5.6-luna` 剥成 bare 名 `gpt-5.6-luna`）。`computeCost`：`input×Prompt + output×Completion + cacheRead×CacheRead + cacheCreation×CacheWrite`。

**新配置**（`config.go`）：
- 顶层 `pricing:{enabled, ttl, source_url}` — 默认 `enabled:true` / TTL `24h` / `source_url` 默认 `https://openrouter.ai/api/v1/models`（`defaultPricingEndpoint`）。`enabled:false` → `pricingSnapshot` 返回 nil，cost 恒 n/a（不抓取）。
- 顶层 `prices:` map — per-model override，单位 **USD per MILLION tokens**（人类单位）；字段 `input`/`output`/`cache_read`/`cache_write`（后两者默认 0）。命中即盖过目录。

**缓存**（`pricing.go`，镜像 models.dev 模式）：`~/.model-proxy/pricing_cache.json`，TTL 24h（`pricingTTL`，可被 `pricing.ttl` 覆盖），atomic tmp+rename。`ensurePricingFresh`：fresh → 用；stale → conditional GET（带 `If-None-Match`）；304 → 只刷 `fetched_at` + 持久化；200 → 重建 + 持久化。抓取失败：有旧 → 用旧 + stderr 告警；无 → 空 catalog（未知价显示 n/a，不阻塞 UI）。

**环境变量 `MP_PRICING_URL`**：pricing 端点解析优先级 **config `pricing.source_url` > `MP_PRICING_URL` env > OpenRouter 默认**（`defaultPricingEndpoint`）。`PricingConfig.sourceURL()`（`config.go`）在 config 未设时回落到 env 感知的 `pricingEndpoint()`（`pricing.go`，读 `MP_PRICING_URL`，**镜像 `MP_MODELSDEV_URL`** 的 test/mirror override 语义），生产路径 `Proxy.pricingSnapshot`（`proxy.go`）经此生效。单测：`TestPricingEndpoint_EnvOverride`（`pricing_test.go`）+ `TestPricingConfigSourceURL_Precedence`（`config_test.go`，钉死 config > env > default 三级优先级）。

**Web UI**：`/ui/` Analytics 标签页（`web_assets/`）消费 `/api/analytics`，渲染 token + 等价成本趋势（uPlot）。未定价模型（如 `doubao-*`）显示 `n/a` + UI 提示。

**CLI**：`stats --granularity day|month` 或 `--cost` 任一 → `renderAnalytics` 改打 `/api/analytics`（`formatAnalyticsTable`：每 (provider,model) 一行 = 窗口内 SUM，`--cost` 才出 cost 列，未定价 `n/a`；`--json` 原样）。两者都省略 → 走 `/api/stats`，输出与原 `stats` **字节一致**（CLI 契约不变，append-only）。

**锁纪律**：`Proxy.pricingMu` 独立叶子锁；`pricingSnapshot`/`priceOverrides` 经 `cfgSnapshot()` RLock 读 cfg，不持 `p.mu` 调入。无 stats store → `series:[]`（nil-safe）。

#### 请求访问日志（`request_log.go`，JSONL 文件，默认关闭）

记录每个 commit 的 upstream 调用的**完整 request+response body** + 元数据，逐行 JSON 写轮转文件，供离线分析（`jq`/`grep`）。**默认 `enabled: false`**（零开销：不 wrap、不开文件、不起 goroutine）；改 `enabled` 需**重启**（reload 不重建 logger）。

热路径：`p.reqLog != nil` 时 `resp.Body` 包 `captureReader`（有界 tee，`max_body_bytes` 封顶，超限停捕获但字节仍透传）→ 非阻塞 `select` enqueue 到 buffered chan（cap 2048，满则计数降频 log，**绝不阻塞 forward**）。`request_id`（crypto/rand）仅在 `p.reqLog != nil` 时生成。单 goroutine drain 写文件；写失败计 `writeErrors` + 降频 log；`MkdirAll` 失败置 `dead`。轮转：超 `max_file_size`（默认 1G）或自然日变更时归档为 `requests-<start>--<end>-<seq>.log`，空文件不归档。retention（默认 30d）按 mtime 删归档文件，**活跃文件永不删**。SIGINT/SIGTERM 排空 chan + 写完 + sweep + 关文件。文件 `0o600`、目录 `0o700`（含用户 prompt）。只记 commit 响应（2xx + 非 failover 4xx）；failover 中间尝试与 all-failed 502 不记。**无 API/无 CLI**，直接读 `.log` 文件。`config.request_log.{enabled, dir, max_file_size, max_body_bytes, retention}`。

### compass 网关契约（实测）

> model-proxy 内部称 `aqp`（config `provider_id`、`aqp_oauth_auth.json`、`aqp_mint_url`）；上游是 `compass.llm.shopee.io`，故保留 compass/CQP 称谓。

| 端点 | 方法 | 鉴权 | 路径 |
|---|---|---|---|
| CQP key 签发 | POST | SSO cookie | `/api/v1/cqp/ccswitch/api_key/get_or_generate` body `{}` |
| 模型列表 | GET | CQP Bearer | `/compass-api/v1/models` |
| 消息（anthropic） | POST | CQP Bearer | `/compass-api/v1/messages` |
| 响应（openai） | POST | CQP Bearer | `/compass-api/v1/responses` |
| 月度用量 | POST | SSO cookie | `/api/v1/cqp/ccswitch/monthly_usage` body `{"project_id":"..."}` |

CQP key 长效，缓存 50min。SSO cookie 值已含 `SSO_C=` 前缀，直接作 Cookie 头值。`/v1/messages` 走 cqp 时需 `?beta=true`、`anthropic-version: 2023-06-01`、`x-compass-request-id`(UUID)。转发头用白名单（不透传客户端 Cookie/Authorization）。

### codex 后端契约（实测）

| 端点 | 方法 | 鉴权 | 备注 |
|---|---|---|---|
| responses | POST | OAuth Bearer | `/backend-api/codex/responses`，需 `store:false` + `stream:true`，不接受 `max_tokens`；body 用 `input`（**必须是 list**，字符串→`Input must be a list`；不能用 `messages`→`Unsupported parameter: messages`） |
| usage | GET | OAuth Bearer | `/backend-api/wham/usage`（**不在 `/codex/` 子路径下**） |
| models | GET | OAuth Bearer | `/backend-api/codex/models?client_version=<ver>`，`{"models":[{slug,visibility,...}]}`，仅取 `visibility=="list"`；`client_version` 决定可见模型 |
| headers | — | — | `originator: codex_cli_rs` 必须（否则 403）；`ChatGPT-Account-Id` 从 id_token JWT 解析 |

OAuth device flow（从 codex-rs 源码确认）：issuer `https://auth.openai.com`，client_id `app_EMoamEEZ73f0CkXaXp7hrann`。`POST /api/accounts/deviceauth/usercode` → `{device_auth_id, user_code, interval}` → 用户访问 `https://auth.openai.com/codex/device` → 轮询 `POST /api/accounts/deviceauth/token`（错误码 `deviceauth_authorization_pending`/`deviceauth_slow_down`）→ `{authorization_code,...}` → `POST /oauth/token` grant_type=authorization_code → tokens。刷新：`POST /oauth/token` grant_type=refresh_token。**代理用独立 OAuth**（不读 codex CLI `~/.codex/auth.json`），避免 refresh_token 轮换竞争。

### Zhipu BigModel 契约

- OpenAI base `https://open.bigmodel.cn/api/paas/v4`（`/chat/completions`、`/models`）；Anthropic base `https://open.bigmodel.cn/api/anthropic/v1`（`/v1/messages`，Bearer）。代理按协议转发。
- 鉴权 `Authorization: Bearer <api_key>`（OpenAI 与 Anthropic 端点都用 Bearer，不像 DeepSeek 需 x-api-key）。
- `/models` 只列 8 个文本对话模型；多模态（glm-4v-plus/cogview-4-plus）需手动加 config。
- 配额 `GET .../api/monitor/usage/quota/limit`（Bearer）→ `{success, data:{limits:[{type,unit,number,percentage,nextResetTime,usage,currentValue,remaining,usageDetails}], level}}`；`type`=TOKENS_LIMIT|TIME_LIMIT，`unit` 3=5h/6=weekly/5=monthly。**`currentValue`=已用、`remaining`=剩余、`usage`=总额**（勿把 `usage` 当已用）。`/users/balance`、`/users/usage` 均 404。
- `usageDetails`：`TIME_LIMIT`（月度）按 **MCP 工具**拆（search-prime/web-reader/zread，工具调用消耗非模型 token），`TOKENS_LIMIT` 按**模型**拆。

### DeepSeek 契约（双协议，一个 key）

- OpenAI base `https://api.deepseek.com`（`/chat/completions`、`/responses`、`/models`、`/user/balance`，Bearer）；Anthropic base `https://api.deepseek.com/anthropic/v1`（`/v1/messages`，`x-api-key`，`anthropic-version`/`anthropic-beta` 被忽略）。
- Anthropic SDK 打 `/anthropic/v1/messages`（base + `/v1/messages`）。代理剥客户端 `/v1`，故 `anthropic_base_url` 须自带 `/v1`。`RewriteRequest` no-op，URL 选择在 `proxy.forward` 按 protocol 完成。
- 鉴权双写：每请求同时设 `Authorization: Bearer` + `x-api-key`，一个 config 服务两协议。
- 服务端模型自动映射（Anthropic）：`claude-opus*`→`deepseek-v4-pro`；`claude-sonnet*`/`claude-haiku*`→`deepseek-v4-flash`。
- `/user/balance` → `{is_available, balance_infos:[{currency, total_balance, granted_balance, topped_up_balance}]}`（注意 `balance_infos` 非 `wallets`）。`/models` → OpenAI 风格；当前 `deepseek-v4-pro`/`deepseek-v4-flash`，旧名 `deepseek-chat`/`reasoner` 2026-07-24 弃用。

### Volcengine Ark 契约（双协议，含 Agent Plan，一个 key）

- OpenAI base `https://ark.cn-beijing.volces.com/api/plan/v3`（Agent Plan；标准 Ark 是 `/api/v3`）；Anthropic base `.../api/plan/compatible/v1`（标准 Ark 是 `/api/compatible`）。`anthropic_base_url` 须自带 `/v1`（代理剥客户端 `/v1`）。鉴权双写（同 DeepSeek，`provider/volcengine.go`）。
- Agent Plan 的 5h/每日/周/月额度在 **GetAFPUsage**（火山引擎签名 OpenAPI：`Action=GetAFPUsage&Version=2024-01-01&serviceCode=ark`，管控面，HMAC-SHA256/V4，需 AccessKey/SecretKey）—— Ark API Key（Bearer）调不了 GetAFPUsage，但能调 `/models`（`login` 用作 key 校验）。`volcengine_sign.go` 做 V4 签名（CredentialScope `{date}/cn-beijing/ark/request`，signing key 链 SK→kDate→kRegion→kService→kSigning，**末项 `"request"` 非 `"volcengine_request"`**；签名头仅 `host;x-date`，**不含 `x-content-sha256`**）。`login volcengine` 收 Ark API Key（必填）+ AK/SK（可选，仅 chat 可缺省）；`login` 用 `usage_url`（Bearer GET `/api/plan/v3/models`）验 Ark Key，可选 AK/SK 经 GetAFPUsage 验证（401/403 拒，其余放行）。`usage volcengine` 解析 `Result.{AFPFiveHour,AFPDaily,AFPWeekly,AFPMonthly}`（各 `Quota/Used/ResetTime`）。`models refresh` 调 **ListArkAgentPlanModel**（同理 V4 签名）解析 `Result.Datas[].ModelID`，经正则过滤 + endpoint 探测后写。未配 AK/SK 退化列 config 模型。
- 模型 ID 是模型名（如 `doubao-seed-1-8-251228`），非推理接入点 endpoint id。

### models.dev 元数据契约（`modelsdev.go`，实测）

- 数据源 `GET https://models.dev/api.json`（raw 3.05 MB）。gzip 后 ~286 KB（Go Transport 自动 gzip——**勿手动设 Accept-Encoding**，否则关掉自动解压）；`If-None-Match`→304 返回 0 字节。磁盘缓存只存去重 slim 投影（`by_name` 244 项，~150 KB），**绝不存 3 MB blob**。
- 缓存 `~/.model-proxy/models_cache.json`，TTL 24h，atomic tmp+rename。`ensureCatalogFresh`：fresh→用；stale/force→conditional GET；fetch 失败+有旧→用旧+stderr；无→空 catalog。`MP_MODELSDEV_URL` 覆盖端点。
- 匹配：`lookup()` 纯全局精确名查找，未命中→default（无 endpoint/后缀匹配逻辑）。借名模型（aqp 借的 `glm-*`/`deepseek-*`）靠 parse 期 `by_name` 去重时 canonical owner 胜出解析。
- **`models:` 只配名字；元数据全来自 models.dev**（context/output/modalities 运行时由 `hydrateModels` 补，失败→default）。effective = config 名字 ∪ routes 引用模型。
- **`models refresh <provider>` 写 config**：拉 upstream 列表 + 现有 `models:` 合并去重 → 逐个 endpoint 探测（`probeModelCallable` 复刻 forward 的 base/path/auth）→ 仅 2xx 保留 → **覆盖写**回 `models:`（非 append-only）。`FilterModelIDs` 做静态策略过滤。探测 infra 不可用→写未校验合并集；**全部失败→保留 config 不清空+告警**。写是保注释的 yaml.Node 往返。**只有 `models refresh` 写 config.yaml；`models`/`takeover` 显示永不写**。
- 覆盖盲区：models.dev **没有** aqp/compass、codex/ChatGPT、volcengine；未命中→default。
- 作用域：仅 `models`/`takeover` CLI 调 `ensureCatalogFresh`+`hydrateModels`（只改内存 cfg，不进 `LoadConfig`/daemon）；代理热路径、`GET /v1/models`、quota 不受影响。`models` 显示 `SRC` 列（`models.dev`/`default`）。

### 隐式路由（implicit routes，实测）

- 已 login provider 的 `models:` 里某个名字**无显式 route** 时，`synthesizeImplicitRoutes`（`NewProxy`/`reload` 跑，此时 login 状态已知）自动合成单目标 route `{首个按字母序已登录 provider, priority 1}`，merge 进 `expandedRoutes`（显式 route 优先）。`forward`/`/debug/schedule`/`/api/status.schedule`/serve-status/`GET /v1/models` 自动看到。
- 多 provider 歧义（同名字被 >1 已登录 provider 提供且无显式 route）→ 只用首个，其余忽略，发**歧义警告**（`model-proxy models` stderr + `/api/status.warnings`）。单 provider 隐式路由静默。
- login 判定：`loadPool(name, provID).Accounts > 0`（`buildProviders` 不返回 login 状态）。
- 作用域：只在 daemon 侧生效；`doctor` 离线只看显式 `cfg.Routes`。隐式路由 sticky 随路由名 key 一并持久化（session-id key 不落盘，见 `snapshotSticky`）。

### 踩过的坑

1. **路径双 `/v1`**：provider openai_base_url 已含 `/v1`，client path `/v1/messages` 拼接变双 `/v1`。需剥 client 的 `/v1`。
2. **codex 请求体**：`store:false`+`stream:true`+无 `max_tokens`（否则 400）。body 是 Responses API 形状：`input` 必须 list（字符串→`Input must be a list`；`messages`→`Unsupported parameter: messages`）。
3. **monthly_usage**：POST 非 GET，需 `project_id` 入参。字段 `total_amount`/`usage`/`balance`/`plan`（非 camelCase）。
4. **wham/usage 路径**：`/backend-api/wham/usage`，不是 `/backend-api/codex/wham/usage`（后者 403）。
5. **日志掩码**：SSO cookie 用 `mask()`（首2…尾2），auth/info 响应体只记长度。
6. **文件日志无色**：`--log-file` 时 `logColorEnabled=false`，否则 ANSI 污染。
7. **flushCopy 写错误**：客户端断开后 `w.Write` 出错须立即 break，否则继续拉上游流浪费 compute。
8. **codex /models client_version 闸门**：必须带 `client_version`；过旧则新模型（gpt-5.6）不返回。解析序：config → `codex --version` → `~/.codex/models_cache.json` → 内置常量。
9. **context 传播**：`http.NewRequestWithContext(r.Context(), ...)`。
10. **supervisor nil panic**：`spawnWorker` 失败返 nil，`runSupervisor` 需检查。
11. **配额陈旧保护**：snapshot 老于 `3×poll_interval` 一律 `BillingUnknown`；`Quota()` 失败也 `BillingUnknown`（按 priority 排，**绝不**当 payg）。
12. **volcengine 配额需 AK/SK**：`GetAFPUsage` 是签名管控面 OpenAPI，Ark API Key 调不了；`login` 收 Ark Key（必填）+ AK/SK（可选）。Ark Key 用 `/models` Bearer GET 校验，AK/SK 用 GetAFPUsage 校验。
13. **锁顺序 `healthMu` → `quotaMu`**：`schedule` 先 `allSnapshots()`（quotaMu RLock）再 `healthMu.Lock()`，绝不在持 `healthMu` 时回调 quota 接口。
14. **`BillingClass` iota ≠ 调度序**：`Unknown=0,Plan=1,PayG=2`，调度序 `Plan<Unknown<PayG`，故 `tierRank` 单独映射。
15. **aqp/codex CLI login 文件名用 config name**：`runLogin`/`cmdCodexLogin` 接收 `provName`（config 一级 key）→ `authFilePath(provName, "oauth_auth")`，**勿**改回硬编码 `authFilePath("aqp"/"codex", ...)`。读侧（`buildOne` 的 `OAuthAuthFile`、web 异步登录、`logout`）都用 config name，硬编码会让非同名实例（如 `codex-work`）登录后读不到凭据 → 401/502。`TestAqpCodexLogin_UsesConfigNameForAuthFile` 守护。

### opencode/pi takeover 配置

| 客户端 | baseURL 格式 | 关键差异 |
|---|---|---|
| opencode | `http://<proxy>/v1` | `@ai-sdk/anthropic` 拼 `baseURL+/messages`，要带 `/v1` |
| pi | `http://<proxy>` | pi 自拼 `/v1/messages`，不带 `/v1`（否则 `/v1/v1/messages`→502） |

`provider_id` 统一一项（默认 `model-proxy`），opencode/pi/codex 共用。备份存 `<configDir>/.model-proxy/<client>.bak`。`takeover:` 块可省略——四 client 路径 + provider_id 在 `LoadConfigFromBytes` 有代码默认值，只覆盖某项才需写。`takeover all`/`restore all` 遇不存在 client 文件（agent 没装）跳过并继续，单 client 缺文件仍是硬错误。
