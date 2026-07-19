# AGENTS.md - model-proxy 架构与契约参考

model-proxy 的架构权威参考：关键设计、调度、路由、踩过的坑。CLAUDE.md 指向此处。改了实现就同步更新本文件（及对应参考文档）。

**按需加载的参考文档**（勿全文读，用到再查）：
- `docs/backend-contracts.md` — 各上游后端契约（compass/codex/zhipu/deepseek/volcengine 逆向实测）、models.dev 元数据、token 文件命名。**改 provider 前必读**。
- `docs/web-api.md` — Web UI + `/api/*` 契约表、SSE token 扫描器、SQLite stats/延迟/agent 维度、Analytics 等价成本、request_log。**改 web/API/stats 前必读**。
- `model-proxy/CLI.md` — CLI 显示契约（change-controlled，规则见 CLAUDE.md）。

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
  providers:                 # openai_base_url + provider_id + models + 可选 capabilities（凭据不落 config）
  claude_mapping:            # anthropic-only：claude 别名 → 对外模型名（路由前先翻译）
  routes:                    # 对外模型名 → provider/真实名 目标列表（target 可声明 protocol: 触发转换）
  scheduling:                # 可选（durations 用字符串；省略则用 config.go 的代码默认）
  cache: / shadow:           # 可选：响应缓存 / 影子评测（见对应章节）
```

默认「对外协议 = 转发协议」，同协议字节级透传；RouteTarget 声明 `protocol:` 与客户端不同则走 opt-in 协议转换（见下）。凭据由 `login <provider>` 管理，存 `~/.model-proxy/<name>_<suffix>.json`（命名规则见 `docs/backend-contracts.md`）。

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
doctor                     # 离线 config 调度诊断（含 shadow 配置段）
serve status               # 终端状态面板（= Web UI Status 标签页）；--logs [N]/--json/--config
stats                      # per-(provider,model) 调用统计（SQLite）；--from/--to/--provider/--model/--bucket/--json；--by-agent（--agent/--provider/--model 过滤）按 agent 出报表；--granularity day|month|--cost 改走 /api/analytics
test <model>               # 端到端探测路由每个 target（claude_mapping→routes/隐式；probeModelCallable；任一通则 exit 0）。池化只测首账号；逐账号用 POST /api/accounts/<p>/<id>/test
pin <route> <provider> [--ttl DUR]  # 临时钉住路由到 provider（硬禁 failover；reload 不清、重启即失效）；unpin <route> / pin（列表）
replay <request_id> --to <provider> # 从 request_log 取原始请求体重发到指定 provider（x-mp-force-provider）；shadow 记录/非 /v1 路径/截断 body 拒绝
shadow report [--from --to]         # 影子评测聚合：按 (route,primary,shadow) 配对，输出样本数/状态一致率/延迟差/大小比（GET /api/shadow-report）
```

### Web UI / API

`web.enabled`（默认 true）时 daemon 挂 `/ui/` + `/api/`（JSON），**无鉴权**；`validate()` 强制 `listen` 回环（`requireLoopbackListen`：拒绝 `0.0.0.0`/空 host/内网 IP/域名，放行 `127.x`/`[::1]`/`localhost`）。写操作统一走进程内 `proxy.reload` 热重载（≠ `serve reload` 的 SIGHUP）。**完整接口契约表、stats/延迟/agent 维度、Analytics、request_log 见 `docs/web-api.md`**。

### 协议转换（`convert.go`，per-target opt-in）

RouteTarget 声明 `protocol:`（`anthropic`/`openai`）≠ 客户端协议 → 转换；同协议**字节级透传**（不走 hub 架构）。anthropic `/v1/messages` ↔ openai `/v1/chat/completions` 双向，请求+响应+流式，**tools 全链路**，映射规则对齐 LiteLLM/ccr/new-api 同构：

- 请求：`tools`/`tool_choice` 双向（`any↔required` 等）；assistant `tool_use`↔`tool_calls`（arguments 是 JSON 字符串）；user 内 `tool_result`→独立 `role:"tool"` 消息在前、text/image 聚 user 消息在后；连续 tool 消息合并进同一 user 消息；`mergeConsecutiveAnthropicRoles` 角色交替；首消息非 user 插占位；image↔`image_url`（data URL 互转）；`parallel_tool_calls ↔ disable_parallel_tool_use`（`tool_choice=none` 不携带 disable）。
- 流式 o→a：openai index→block index 分槽；`input_json_delta` 原样透传；**交错并行 tool 整块缓冲、finish 按序完整发出**（anthropic 块模型无法 resume 已 stop 块）；finish 延迟等 trailing usage；error chunk→`error` 事件。
- 流式 a→o：`curType=="tool_use"` 门控（非工具块的 input_json_delta 不误发）；零 args 补 `"{}"`；`finished` 门控不重复发；error 事件→error chunk。
- id 规范化（o→a）：`sanitizeToolUseID` 满足 `^[a-zA-Z0-9_-]+$`，tool_use/tool_result 成对同映射（per-call memo + 确定性纯函数）。
- usage：a→o 注入 `stream_options.include_usage`；o→a `input=prompt−cached`(clamp≥0)+`cache_read_input_tokens`；a→o `prompt=input+read+creation`+`prompt_tokens_details.cached_tokens`。
- 丢弃但 `convertWarn` 告警（每进程每消息一次）：thinking/cache_control/server tools/tool_result 内图片/logprobs/未知 role/流式 thinking_delta。
- 接线：响应转换是**最内层** wrap——logger/scanner/cache 只见客户端协议字节（`captureReader` 必须包转换后的 body，曾包错 `resp.Body` 致客户端收到未转换 SSE）；非流式 `io.LimitReader` 64MB；SSE 行上限 8MB+错误告警。

### 请求感知路由（`request_routing.go`）

schedule 之后按请求内容过滤/改道；catalog（models.dev context/modalities/`tool_call`）由 daemon 启动/reload 经 `initCatalog` 异步加载，**nil catalog 全功能 no-op**。

- `profileRequest`：`hasImage`（字节标记）、`hasTools`（`"tools":[`）、`est`（rune 感知：CJK 每字 1 token、其余字节/4、>100 字符 base64 段剔除）。
- `modelFits` 判定序：provider config `capabilities: {model: [image,tools]}` **优先**（声明即权威，catalog 被忽略——codex/aqp/volcengine 盲区逃生口；池化虚拟经 parentOf 取父 config）；未声明 → catalog（查不到=保守不支持）；context 窗口恒走 catalog。
- 改道：in-route 过滤 → 无匹配时跨路由池（`crossRoutePool`，合成 sticky key 排序）；**二次 schedule 返回空 → 回落原 ordered**（宁试不匹配目标，不零尝试 502）；pin/force 不改道。
- **400 被动重试**：`tryTarget` 对 4xx commit 前 peek ≤64KB，`isContextOverflow`（保守子串）命中且未重试过 → `contextOverflowRetry` 选 catalog 严格更大 context 的跨路由目标重试一次；找不到则 peek 过的字节经 MultiReader 原样 commit。溢出尝试只计 `evFailovers`（**不进熔断**、不计 requests/latency）。

### 精确响应缓存（`cache.go`，默认关）

`config.cache.{enabled, ttl(10m), max_entries(1000), max_body_bytes(256KiB)}`。key = SHA-256(method+path+body) 字节级匹配；只缓存 <300 + `sawEOF` 门槛（客户端断开/半截不入缓存）；转换响应剥 content-length 再存。命中重放**原始字节**（SSE 逐字节一致）+ `x-mp-cache: hit` + live end 事件（provider=`(cache)`），**不进 metrics/agents**。`x-mp-force-provider`/pin 生效时查写全跳过（否则 replay 被旧缓存架空）。观测 `/api/status.cache`；reload 重建（立即生效、条目清空）。**定位是「重试/重复请求盾牌」**：多轮会话 body 逐轮变长，命中率≈0；前缀经济性归上游 prompt caching。

### pin（运行期钉路由，排查用）

`pin <route> <provider> [--ttl]` / `unpin` / `GET|POST|DELETE /api/pin`。`decideOrder` 在 `avail()` **之前**收窄到 pinned provider（pinned 时跳过熔断槽）→ **硬禁 failover**：pinned 熔断/失败 → 502 也绝不逃别家。钉池化父名 = 钉全部账号。纯内存：重启即失，**reload 不清**（与 health/sticky 不对称，注意）。单次覆盖：`x-mp-force-provider` 头 / `force_provider` query（replay 用）。`/debug/schedule` 展示 pin + 过期。

### 实时请求监视（`live_events.go`）

`eventHub`（200 条 recent ring + 非阻塞 fan-out，慢订阅者丢事件）。forward 发 start/end 事件（agent/protocol/provider/status/latency/tokens/`request_id`——id 恒生成：启动 nonce + 原子计数器）；缓存命中与 400/502 终止有独立 end。`GET /api/events` SSE（重放 ring + 推流，15s keepalive；挂主 mux 不经 webServer）；UI Live 标签页。

### 影子评测 + 一键重答（`runShadow` + `replay_cmd.go`）

`config.shadow.{<route>: {provider, model?, protocol?, sample_rate?, max_concurrent?}}`（validate 校验）。commit 后 fire-and-forget 重发候选后端：`sample_rate`（`*float64`：nil→1.0、显式 0=关闭）采样 + `shadowSem` 并发闸（默认 4，满则丢弃）+ 共享 `shadowClient`；按影子**自身 protocol** 选 baseURL（可跨协议影子）。**绝不污染生产**：不进熔断/stats/sticky/metrics。结果写 request_log（`shadow:true` + `shadow-<原id>` 配对）。聚合：`shadow report` / `GET /api/shadow-report`（成对样本 → 样本数/状态一致率/延迟差/大小比；judge 胜率未做）。replay：`replay <id> --to <provider>` 取 `orig_body`（原始客户端 body）+ 原 path 重发；拒绝 shadow 记录/非 `/v1` 路径/截断 body。对标：开源代理层无对应物（Envoy traffic mirroring 是 infra 层），评测报告是差异化。

### 多模型编排（`fusion.go`，OpenRouter Fusion 式）

顶层 `fusion:` 配置「配方」（panel 2..4 + synthesizer + 可选 `min_panel`），路由用 `{provider: fusion, model: <配方名>}` 引用；forward 目标循环在 providerConfig 查询前拦截 `t.Provider == "fusion"` 走编排引擎（`expandTarget` 对非池化名透传，provider 包零改动；`force_provider` 指定具体 provider 时跳过）。

- **fan-out**：每成员一条 goroutine——fail-closed 构建门（无 impl 直接剔除）→ `takeHalfOpenSlot` 熔断门 → 按成员 `protocol:` 转换 + rewriteModel → **非流式**子调用（per-leg `upstream_timeout`）。草稿截断 24k 字符。成员全套走 `recordSuccess`/`recordFailure`/`recordRateLimit`（熔断/quota 语义与普通转发一致；**被 quorum/grace 砍掉的腿不算失败**——`ctx.Canceled` 不进熔断不计 metrics）。
- **quorum + grace**：`min_panel`（默认 2）份候选达成即开合成，**再等 5s grace** 收留 straggler（`fusionGracePeriod` 包级 var，测试可缩）；quorum 永不可达（成功+在途 < quorum）→ 立即取消剩余腿。
- **工具轮**：请求带 tools 时**草稿腿剥 `tools`/`tool_choice`**（纯文本分析），**合成腿带 tools**（tool_use 原样透传）；synthesizer 不支持 tools 则降级。
- **合成**：`buildSynthesisBody`——anthropic 往 `system` 追加候选段 / openai 追加 user 消息（不动对话尾部），shuffle 防位置偏好，固定指令模板；合成腿**复用 `tryTarget`**（SSE 直通、metrics/latency/live/request_log/缓存录制全白拿）。客户端 TTFT = quorum 达成 + grace + 合成 TTFT。
- **错误语义**：合成腿失败 = 硬终结（原样回客户端，不重试——重试是 N+1 次全套）；仅 quorum 不足 / synthesizer 不支持 tools / body 构建失败时降级「原始 body 直打 synthesizer」。
- **观测**：草稿腿 request_log 记 `fusion-panel-<i>-<原id>`、live 事件标 `fusion-panel:<model>`；usage 分腿记 tokenCounter+agentSink（**客户端 usage 数值不改写**，与分腿记账避免重复）；stats 里编排成本 = 各成员正常计量（N+1 倍开销一目了然）。
- **validate**：配方引用的 provider 存在、禁嵌套 fusion、protocol 值合法且有对应 base URL；routes 的 `provider: fusion` 特判 + 配方存在性；shadow 拒绝指向 fusion。`Fusion` 在 Config/rawConfig/拷贝段三处（踩坑 #16）。
- **定位**：只给「困难问题要最好效果」的路由用（2× 延迟、N+1 倍成本），别当默认路由；多轮会话每轮都编排会放大成本，建议单发场景。学术/开源出处：MoA、LLM-Blender（v2 judge）、OpenSquilla B5（机制蓝本）。

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
16. **新顶层配置字段必须同时进 `rawConfig` + 拷贝段**：`LoadConfigFromBytes` 用独立 `rawConfig` 解码再逐字段拷到 `Config`，yaml.v3 静默忽略未知键——漏加则配置写了不报错也不生效（`cache`/`shadow`、`ShadowSampleRate`/`ShadowMaxConcurrent` 各踩过一次；测试直接构造 `Config` 结构体所以全绿发现不了）。加字段时配一条 YAML 加载测试（如 `TestLoadCacheAndShadow`）。

### opencode/pi takeover 配置

| 客户端 | baseURL 格式 | 关键差异 |
|---|---|---|
| opencode | `http://<proxy>/v1` | `@ai-sdk/anthropic` 拼 `baseURL+/messages`，要带 `/v1` |
| pi | `http://<proxy>` | pi 自拼 `/v1/messages`，不带 `/v1`（否则 `/v1/v1/messages`→502） |

`provider_id` 统一一项（默认 `model-proxy`），opencode/pi/codex 共用。备份存 `<configDir>/.model-proxy/<client>.bak`。`takeover:` 块可省略——四 client 路径 + provider_id 在 `LoadConfigFromBytes` 有代码默认值，只覆盖某项才需写。`takeover all`/`restore all` 遇不存在 client 文件（agent 没装）跳过并继续，单 client 缺文件仍是硬错误。
