# AGENTS.md - model-proxy 架构与后端契约参考

本文件是 model-proxy 的架构/契约权威参考：关键设计、各后端契约（逆向实测）、踩过的坑。CLAUDE.md 指向此处。改了实现就同步更新本文件。

## model-proxy 实现经验

### 架构：Provider 抽象 + 协议路由

```
provider/                    # Provider 实现（每个上游一个文件）
  provider.go                # 接口 + 注册 + New()
  apikey.go                  # ApiKeyBase（共享 auth file + Bearer 注入）
  fetch_models.go            # fetchModelsBearer（OpenAI 风格 /models 的共享 helper）
  probe.go                   # baseProbe（ProbeRequest/ExtraHeaders/FilterModelIDs 默认实现）+ body 构造 + newRequestID
  aqp.go                     # aqp: SSO + CQP + monthly_usage + ?beta + anthropic headers
  codex.go                   # codex: OAuth device flow + wham/usage + store:false
  zhipu.go / deepseek.go / volcengine.go / static.go  # 其余 provider

config.yaml:
  providers:                 # provider 定义（openai_base_url + provider_id + models）
    aqp:
      provider_id: aqp       # 路由到 provider/aqp.go
      openai_base_url: ...
      aqp_mint_url: ...      # aqp 专属
    zhipu:
      provider_id: zhipu     # 路由到 provider/zhipu.go
      openai_base_url: ...
      usage_url: ...          # zhipu 专属
  claude_mapping:              # anthropic-only：claude 别名 → 对外模型名（路由前先翻译）
    claude-opus-4-7: glm-5.2
    claude-sonnet-4-6: deepseek-v4-pro
  routes:                      # 对外模型名 → provider/真实名 目标列表（不按协议）
    # 调度：先看非高峰（provider 的 peak_hours），再看 priority，失败逐一 failover
    glm-5.2:
      - {provider: aqp, model: glm-5.2, priority: 1}
      - {provider: zhipu,   model: glm-5.2, priority: 2}   # failover 备选
    gpt-5.5:
      - {provider: codex, model: gpt-5.5, priority: 1}

  scheduling:                 # 调度/熔断（durations 用字符串）
    circuit_threshold: 3      # 连续失败 → 熔断
    circuit_cooldown: 10m     # 开路时长，后半开 1 个探针
    rate_limit_backoff: 60s   # 429 无 Retry-After 时的默认退避
    upstream_timeout: 30s     # 每个上游请求超时
    sticky_dwell: 10m         # 切到某 provider 后最少用多久（≈2× 缓存 TTL）
    quota_poll_interval: 5m   # 后台 Quota() 轮询周期
    quota_switch_margin: 15   # 切换 provider 的 quota 边际（百分点）
```

熔断/限频/粘性状态在 `Proxy.health`（`healthMu`，与 reload 的 `mu` 分开，避免与 `handler` 的 RLock 死锁）。`schedule` 跳过开路/限频 provider；`tryTarget` 在超时/5xx/conn-error 计熔断、429 记限频（Retry-After 或默认退避）、成功清零；半开用 `halfOpenInFlight` 单飞。粘性：每路由记一个 current provider + since，驻留窗口内优先它（保 cache、不频繁回切）。

对外协议 = 转发协议（不做转换）。凭据由 `login <provider>` 管理，存储在 `~/.model-proxy/<name>_<suffix>.json`，不落 config。

**Provider 接口**（`provider/provider.go`）：`AuthHeaders` / `Refresh` / `RewriteRequest` / `Login` / `Logout` / `Usage` / `FetchModels` / `Quota` / `Surplus` / `ProbeRequest` / `ExtraHeaders` / `FilterModelIDs`。后三者承载 provider 专属的探测/过滤/请求头知识，**绝不放在 main 包的 `if prov.Provider == ...` 分支**。默认实现集中在 `baseProbe`（`provider/probe.go`，每个 provider embed）：
- `ProbeRequest(modelID)` -- `models refresh` 探测的最小请求（method/path/body）。默认 OpenAI `POST /chat/completions`；aqp override `/v1/messages`+anthropic body，codex override `/responses`+Responses API body（`input` 列表、`stream:true`、无 `max_tokens`）。
- `ExtraHeaders(req, path)` -- 每次请求（forward + 探测）都要的专属头。默认 no-op；aqp override 设 `anthropic-version` + UUID `x-compass-request-id`（forward 与 probe 共用此实现，消除旧的两处重复分支）。
- `FilterModelIDs(ids)` -- `models refresh` 的静态策略过滤（policy pass）。默认透传；volcengine override 剔除 `*-latest`/`doubao-seed-1-*`/lite/mini。

main 包通过回调注入（`Config.Auth` / `LoginFn`/`LogoutFn`/`UsageFn`/`FetchModelsFn`/`QuotaFn`，在 `proxy.go:buildProviders`）把现有 auth/登录/用量函数接入，`provider/` 包不重新实现这些。

### 配额感知调度（quota-aware scheduling）

`Provider.Quota()`（接口方法，镜像 `UsageFn` 由 `QuotaFn` 回调注入）把每个上游的用量端点解析成归一化的 `provider.QuotaSnapshot{Billing, RemainingPct, Windows, ...}`。各 provider 来源：

| provider_id | 来源端点 | Billing |
|---|---|---|
| zhipu | `quota/limit`（5h/周 token + 月度时间） | plan |
| codex | `wham/usage`（primary/weekly 窗口 + spend） | plan |
| volcengine | `GetAFPUsage`（V4 签名，需 AK/SK） | plan |
| aqp | `monthly_usage`（月度 ratio/balance） | plan |
| deepseek | `/user/balance`（按量余额） | pay-as-you-go |

`RemainingPct` = 该 provider **最终窗口**（总预算）的 remaining%：zhipu=周、volcengine=月、codex=月度 spend、aqp=月、deepseek=payg（无窗口）。`QuotaWindow` 带 `Ultimate`/`Short` 标记 + `Duration`（名义周期），由各 parser 设置。短窗口（5h 等）是 rate-cap，**不参与 min**（用满即 429，反应式跳过）；zhipu 的 `TIME_LIMIT`（MCP 工具配额）不参与（只展示）。

**`quotaTracker`**（`quota.go`）：后台 goroutine 每 `scheduling.quota_poll_interval`（默认 5m）并行轮询所有 provider 的 `Quota()`，结果缓存在内存 + 原子落盘到 `~/.model-proxy/quota_state.json`（启动时作为基线加载，避免冷启动无数据；**现在还携带每路由 `sticky` map，重启后恢复 → 保 prompt cache + 可见上次选择**）。`NewProxy` 启动它；`reload` 不重建（通过 cfg/providers 快照闭包读取新配置），只 kick 一次 `pollAll` 让新加 provider 立即出现；429 触发该 provider 的异步 `refreshOne`，让限频窗口结束后配额已是最新。陈旧保护：snapshot 老于 `3×poll_interval` 视为 `BillingUnknown`。自带锁 `quotaMu`（独立于 `healthMu` 和 reload `mu`）。**锁顺序：`healthMu` → `quotaMu`**（`schedule` 里 `allSnapshots()` 在 `healthMu.Lock()` 之前调用，绝不反向嵌套）。

**调度分 = surplus**（`Provider` 接口方法 `Surplus(snap, now, peakMult)`，各 provider 委托给共享公式 `(*QuotaSnapshot).Surplus`，位于 `provider/` 包）：`surplus = (ultimate.remaining − short.remaining × (short.total/ultimate.total) × (peakMult−1)) − clamp((ultimate.reset−now)/ultimate.duration, 0, 1)`。surplus>0 = 落后节奏（不用就浪费 → 优先用）；<0 = 超前节奏（会提前耗尽 → 回避）。`schedule()`（拆成不写 sticky 的 `decideOrder()` + 提交 sticky）把可用目标（熔断/限频过滤不变）按 `(tierRank, priority asc, surplus desc)` 排序 —— **priority（config）压过 surplus，surplus 只在同 priority 之间打破平局**：

- **`tierRank`**：**plan(0) < unknown(1) < payg(2)** —— 注意 `BillingClass` 的 iota（`Unknown=0, Plan=1, PayG=2`）**不等于**调度顺序，故 `tierRank` 单独映射；pay-as-you-go（`billing: pay-as-you-go`）严格兜底。
- **peak 只走短窗口折算**：`peakMult` 只烧短窗口（公式里的 `×(peakMult−1)` 项）。没有同单位短窗口的 provider（codex/compass 是 money ultimate；未轮询的）**高峰不打折** —— peak 不再是整份 latency 折扣。multiplier=1 关闭。
- **粘性切换**（cache 友好）：路由停在 current provider 一个 `sticky_dwell`（默认 10m）；到期后仅当最优者在 **tier → priority → surplus 边际（`scheduling.quota_switch_margin`，默认 15 pts）** 任一更优时才换 —— 最优者只是同 priority 下 sub-margin 的 surplus 微差则保留 cache。

**可见性**：`GET /debug/schedule`（只读，经 `decideOrder` peek，不改 sticky）返回每路由首选 provider + ordered 列表（tier/surplus/可用/peak）+ sticky 状态。`model-proxy schedule`（CLI，查询该接口，需 daemon 在跑）；`model-proxy doctor`（离线 config 诊断：每 provider 的 tier/quota/peak、每路由 dry-run 顺序（无 live 配额→tier 再 priority）、warning）。

**新配置**：provider 级 `billing: pay-as-you-go`（默认 `plan`）；多段 `peak_hours`（单字符串 / 字符串列表 / `{window, multiplier}` 列表）；`scheduling.quota_poll_interval`（5m）；`scheduling.quota_switch_margin`（15）。`sticky_dwell` 同时充当「切换前的最小驻留」。

### 多账号凭据池（credential pool）+ session-sticky 路由

一个 config provider 可挂**多个账号**（`login <provider>` 重复录入）。账号存于**复数池** `~/.model-proxy/<name>_apikeys.json`（`{version, accounts:[{id, label, api_key, (access_key, secret_key), added_at}]}`）；旧的单数 `<name>_apikey.json` 是只读 fallback（包成 1 条池）。`accountIDFor`（`pool.go`）派生稳定 id —— volcengine = `access_key`，其余 apikey = `sha256(api_key)[:16]` —— 用于去重 + 虚拟名后缀。`login` 按 id 去重（`--replace` 或交互确认；`--label` 命名；写完 SIGHUP 热重载 daemon）。**aqp/codex 不池化**（单 OAuth 凭据），只有 zhipu/deepseek/volcengine 池化。

**构建期展开成虚拟 provider**（`buildProviders`）：≥2 账号的池展开成 N 个虚拟 provider，key = `name#<accountID>`（单账号时仍是 `name`，行为与之前逐字一致）。每个虚拟共享父 config（base_url/models/billing/peak），但**三处**绑各自凭据：① 内嵌 `ApiKeyBase`（forward `AuthHeaders`，经 `provider.Config.BoundAPIKey` —— zhipu/deepseek/volcengine 的 forward key 来自这里，**不是 `cfg.Auth`**）；② `cfg.Auth`（FetchModels 走 `fetchModelsBearer`）；③ Usage/Quota 闭包（透传 `*accountCred`）。路由 target 命名池父 → `buildExpandedRoutes` 展开成 N 个虚拟（同 model+priority）；`forward`/`scheduleStatus`/`doctor` 读展开表。**因为虚拟名就是普通 provider 名，per-account 熔断 / 429 跳过 / 配额快照 / failover 全白送。**

**池内路由 = session-sticky**：sticky map 的 key 从「路由名」换成请求的 `x-claude-code-session-id`（无该头则退回路由名 → 行为不变）。新 session 由 per-parent 计数器（`spreadCtr`，`commit` 时才 +1）**轮询分配**到一个池账号；之后该对话**整场停在这个账号**（只在 429/熔断时重选，**不会中途迁到边际更优的账号**）—— 保 prompt cache；不同对话落到不同账号 → 并发分流。**无 `strategy` 配置项**。session-keyed sticky **不落盘**（`snapshotSticky` 只持久化路由名 key），`quota_state.json` 不存会话 id。

**踩过的坑**：① forward 鉴权读内嵌 `ApiKeyBase`（文件名 = providerName），虚拟必须经 `BoundAPIKey` 绑内存 key，否则会去读不存在的 `<name#id>_apikey.json` → 502；② `Refresh()` 在 bound 实例上是 no-op（bound key 不可变，清缓存会注入空 Bearer）；③ `login` 写的是复数池，故 **1 条池（单账号）也必须 bind**（不能走 file-backed 读单数文件，否则 `login` 后 502 —— 这是 buildProviders 用 `os.Stat(poolPath)` 区分「复数池存在」与「单数 fallback」的原因）；④ volcengine 每账号要完整 `{api_key, access_key, secret_key}`（`GetAFPUsage` 签名用 AK/SK），`resolveVolcengineAKSK` 对非 nil cred **排他**（不回落文件，防泄漏兄弟账号 AK/SK）；⑤ volcengine 的 `FetchModels`（`ListArkAgentPlanModel`）暂未按账号绑 -- 池化时 `models refresh volcengine` 的探测复用 `poolVirtuals` 选首个 virtual 的 bound 凭据；endpoint 探测全部失败时退回合并集不写空。

### Token 文件命名

| provider_id | suffix | 文件名 |
|---|---|---|
| aqp | oauth_auth | `~/.model-proxy/<name>_oauth_auth.json` |
| codex | oauth_auth | `~/.model-proxy/<name>_oauth_auth.json` |
| zhipu / deepseek | apikey | `~/.model-proxy/<name>_apikey.json`（单数，只读 fallback）|
| volcengine | apikey | `~/.model-proxy/<name>_apikey.json` —— `{api_key, access_key, secret_key}`（API Key 聊天 + AK/SK 签 `GetAFPUsage`）|

路径从 provider name（config 一级 key）派生，支持多实例（如 `zhipu-personal` / `zhipu-work`）。**多账号**：zhipu/deepseek/volcengine 重复 `login` 写**复数池** `~/.model-proxy/<name>_apikeys.json`（见上「多账号凭据池 + session-sticky 路由」），单数文件是其 fallback；aqp/codex 不池化。

### CLI 命令

```
login <provider> [--label NAME] [--replace]   # aqp: SSO / codex: device flow / apikey 类: 输入 key（可重复录入 → 多账号池，按 id 去重；写完热重载）
logout <provider> [--label NAME | --all]       # 删一个账号（默认交互式选择）/ 清空池
usage <provider>          # 池化时默认逐账号展示全部账号用量（--label 看某一个）
models [provider]         # 从 config 列模型
models refresh <provider> # 从服务端刷新；无 /models 端点时回退探测路由模型 + 现有 config 模型
models pull               # 强制刷新 models.dev 元数据缓存
serve [daemon|stop|reload|status]
takeover/restore <client>
config init|print|check
schedule                   # 查询运行中的 daemon：每个 model 当前调度到哪个 provider（GET /debug/schedule）
doctor                     # 离线 config 调度诊断（每 provider tier/quota/peak + 每路由 dry-run 顺序 + warning）
serve status               # 终端状态面板（= Web UI Status 标签页）：拉 /api/status + /api/tokens（--logs 再加 /api/logs），终端优化输出；--logs [N] / --json / --config
stats                      # per-(provider,model) 调用统计（SQLite）；--from/--to/--provider/--model/--bucket/--json
```

### Web UI + `/api/*` 接口契约

`web.enabled`（默认 true）时，daemon 在同一个 mux 上挂 `/ui/`（嵌入式静态资源，`web_assets/` 经 `go:embed`）和 `/api/`（JSON）。**仅 loopback、无鉴权**（信任来自「本地」）。`/api/*` 路由表：

| 方法 | 路径 | 请求 body | 响应 shape | 备注 |
|---|---|---|---|---|
| GET | `/api/status` | — | `{uptime, version, listen, health{<prov>:{circuit_state, available, [circuit_until], [rate_limited_until]}}, quota{<prov>: <QuotaSnapshot 原样, PascalCase 键>}, schedule: {models:[…]}(来自 /debug/schedule), counters{<prov>:{requests, failovers, rate_limited_429, failures, last_request_at}}}` | 锁：`p.mu`(RLock) → `healthMu` → `quotaMu`(经 allSnapshots) **顺序获取不嵌套**。`quota` 字段是 provider 包的 `QuotaSnapshot` 原样序列化（无 json tag → **PascalCase**：`RemainingPct`/`Windows`/`Billing`…） |
| GET | `/api/logs?tail=N` | — | `{lines:[…]}` | 读 log 文件末尾 N 行（默认 200，上限 1000）。路径取 `web.logFile`（runProxy 时解析）回落 `cfg.LogFile`，都没配 → 404 |
| GET | `/api/config` | — | `{yaml: "<原文件 verbatim>", summary:{listen, provider_count, route_count}}` | 原文件 round-trip（saveAndReload 保证 byte-identical 落盘） |
| POST | `/api/config` | `{yaml:"…"}` | `{status:"reloaded"}` 或 400 `{error}` | 走 `saveAndReload`：validate(`LoadConfigFromBytes`, 不动盘) → backup `back/<base>.<时间戳>.bak`（每次写各留一份） → `atomicWrite`(tmp+rename) → `proxy.reload`。校验失败不落盘、不建备份；reload 失败（防御性，post-validate 不应发生）从该次备份回滚 |
| POST | `/api/config/edit` | `{kind, name, data}` | `{status:"reloaded"}` 或 400 | 结构化编辑（`kind ∈ {general, scheduling, provider, route, claude_mapping}`），改 `yaml.Node` 树（**保留注释与键序**）→ 重编码 SetIndent(2) → 经 `saveAndReload`。`data.delete:true` 删除具名实体（claude_mapping 用 `data.alias` 定位）。route targets 经 `mustEncode`（JSON 值 → yaml.Node 内层）。**坑**：新 int 键默认 `!!str`（yaml.v3 unmarshal 时自动识别既有 int 字段；目前无新建 int 键的路径） |
| GET | `/api/accounts` | — | `{providers:[{name, provider_id, billing, accounts:[{id, label, added_at, [email}]}]}` | **响应结构里根本没有 key 字段** —— 即使 builder 出 bug 也无法泄漏 api_key/SSO cookie/access_key/secret_key。id **不掩码**（UI 要用它来删除） |
| POST | `/api/accounts/<provider>` | `{api_key, access_key?, secret_key?, label?, replace?}` | `{id, status:"added"}` | 仅 apikey 类（zhipu/deepseek/volcengine）。aqp/codex 返 400 指向 async login。volcengine 走 `addVolcengineAccount`（AK/SK 三元组，不探 usage_url）；其余走 `addApikeyAccount`（探 usage_url 校验）。落盘后 best-effort reload（账号已存盘，reload 失败也返回成功） |
| DELETE | `/api/accounts/<provider>/<id>` | — | `{status:"removed"}` | apikey 类走 `removeApikeyAccount`；aqp = `clearAccount`（oauth_auth，logout 语义）；codex = `os.Remove`（单凭据）。best-effort reload |
| GET | `/api/tokens` | — | `{usage:[{provider, model, input, output, cache_creation, cache_read, requests}]}` | 由 SSE 扫描器累计的观测用量（见下）。flat 数组（map[tokenKey]tokenUsage 摊平，JSON 对象 key 必须是 string） |
| POST | `/api/tokens/reset` | - | `{status:"reset"}` | 清零内存计数 + SQLite `stats.db` + flusher 基线（`Proxy.resetStats`） |
| GET | `/api/stats?from=&to=&provider=&model=&bucket=` | - | `{from,to,bucket,buckets:[{provider,model,minute,requests,failovers,rate_limited_429,failures,input,output}]}` | per-(provider,model,minute) 调用统计，存储恒为 1 分钟桶；`bucket`（如 `10m`/`1h`）仅展示聚合（SQL GROUP BY）。由 `statsFlusher` 每分钟从内存 metrics+tokens diff 落盘 |
| POST | `/api/login/<provider>/start` | — | `{session_id, login_url}`(aqp) 或 `{session_id, verify_url, user_code}`(codex) | 异步登录启动。aqp：`BootstrapLoginURL` + 建 cookie-jar `AqpClient` + 起 poll goroutine；codex：`requestUserCode`（device flow）+ 起 poll goroutine。unknown provider → 404；非 aqp/codex → 400 |
| GET | `/api/login/<session>/poll` | — | `{state:"pending"|"done"|"error", detail, result}` | poll 异步登录。`detail` = login_url/verify_url+user_code（pending，UI 可恢复）/ error msg（error）；`result` = email/account_id（done）。unknown/expired session → 404 |

**写操作统一热重载**：上表所有 mutation（`POST /api/config`、`/api/config/edit`、`POST/DELETE /api/accounts/*`、aqp/codex 登录 goroutine 完成）落盘后都触发进程内 `proxy.reload(configFile)` —— **同一个 worker 进程原地换 cfg/providers，不重启**，改动即时生效。账号增删虽不改 `config.yaml`，但 `reload → buildProviders(cfg) → loadPool` 会重读每个 provider 的池文件，新加/删除的账号随即（取消）展开成虚拟 provider，路由立即看到。账号类 reload 是 best-effort（凭据已先落盘，reload 失败由下次 reload/请求兜底）；config 类是严格流水线（validate-before-write + reload 失败从当次 `back/<base>.<时间戳>.bak` 回滚）。reload 还会清空 `health`/`sticky`/`spreadCtr`（复位卡住的熔断/限频/粘性）并 kick 一次 `quota.pollAll`。注意：这是**进程内** reload（UI 与被重载的 worker 同进程）；与 `serve reload`（给独立进程发 SIGHUP）不同。

`serve status`（CLI，`serve_status.go`）是上表 `GET /api/status` + `/api/tokens`（带 `--logs` 再加 `/api/logs`）的终端消费者——一次性拉取、终端优化输出（列对齐 / 着色 / 紧凑数字 `compactNum`），内容与 Status 标签页一致（头部 version/uptime/listen + Providers 健康/计数器 + Schedule + Quota + Tokens，可选 Logs）。结构：纯 `renderStatus(listen, opts) (string, error)` 核心（`renderProviders`/`renderSchedule`/`renderQuota`/`renderTokens`/`renderLogs` 各段，httptest 可测）+ 薄 `cmdServeStatus` 包装（不单测，同 `cmdSchedule` 惯例）；`parseStatusFlags` 解析 `--logs [N]`(默认 20)/`--json`/`--config`（`--config` 仍交给 `configPath`，不被吞掉）。`statusGet` 用 10s 超时的 `http.Client`（卡死快速失败，错误同 `cmdSchedule` 风格：transport/404-web.enabled/非200/解析）。`--json` 原样合并 `{status, tokens[, logs]}`（`json.RawMessage` 透传，便于 jq）。`renderScheduleRoutes` 与 `cmdSchedule` 共享（重构后 `model-proxy schedule` 输出逐字不变）。需要 `web.enabled`（默认 true）；`/api/status` 返 404 → 提示开启 Web UI。

#### SSE token 扫描器契约（`tokens.go`，`forward` 2xx 提交处接入）

- **Pass-through only**：`usageScanner` 是个 `io.ReadCloser`，包在 `resp.Body` 外**仅当 `isSSE(resp.Header)`**。读到的字节原样返回给客户端 —— **不修改、不缓冲流、不阻塞客户端**。失败静默（不记 usage）。
- **bounded 64KB 行缓冲**（`scanLineCap = 64 * 1024`）：`observe` 按 `\n` 切完整行喂给 `parseLine`；不完整行的字节存进 `s.line`（cap 到 64KB，超出则丢弃该字节 —— 字节仍会 pass through，只是超长行不解析）。防 OOM。
- **commit-on-EOF/close（含客户端断开）**：`Read` 见到 err 且 `!done` → `tc.commit(key, acc)`；`Close` 若 `!done` 也 commit。`forward` 在 `flushCopy` 后显式 `body.Close()` —— 客户端中途断开（`flushCopy` 写错即 break，未达 EOF）时也能记下已观测的 input tokens（`message_start` 在流首、cancel 前到达）。正常 EOF 时 `Read` 已置 `done=true` 并 commit，`Close` 是 no-op（**不重复计数**）。
- **解析**：仅看 `data:` 前缀 + 首字符 `{` 的行。先试 anthropic shape（`message_start` → input/cache_creation/cache_read；`message_delta` → output），再试 openai shape（`usage.prompt_tokens`/`completion_tokens`）。OpenAI token 计数是 **best-effort**：`usage` 仅当客户端发 `stream_options.include_usage`（且上游愿给）时才有 —— 扫描器宁可啥也不记也不 zero-fill。
- **持久化**：`tokenCounter` 本身**纯内存**（`commit`/`snapshot`/`reset`，不落盘）；持久化由 SQLite stats 接管（见下「调用统计持久化」）。`tokenMu` 是**独立叶子锁**，`commit`/`snapshot` 的 `*tokenUsage` 字段读写都在 `tc.mu` 下，与 `p.mu`/`healthMu`/`quotaMu` 互不嵌套。

#### 调用统计持久化（`stats.go`，SQLite 单一来源）

`metricsStore`（per-(provider,model) 原子计数器，`requests`/`failovers`/`rate_limited_429`/`failures`/`last_request_at`）+ `tokenCounter` 都在 hot path 纯内存操作（一次原子 add + 短暂 map mutex），**hot path 不碰 SQLite**。一个 `statsFlusher` goroutine（`runProxy` 启动，按墙钟分钟边界 tick）快照两者、与上次快照 **diff**、把非零 delta 作为分钟桶 upsert 到 `~/.model-proxy/stats.db`（`minute_buckets` 表，key `(provider,model,minute)`，`ON CONFLICT DO UPDATE` 加法累加，`last_request_at` 用 `MAX`）。

- **启动**：`initStats`（`runProxy` 里调，非 `NewProxy` -- 直接 `NewProxy` 的测试保持纯内存、不碰 `~/.model-proxy/`）开 DB、在 DB 空时**一次性导入** legacy `~/.model-proxy/token_usage.json`、`loadCumulative()`（全表 `SUM`/`MAX`）seed 内存计数器、设定 flusher 的 diff 基线（首次 flush 只写启动后 delta，不重复计数）。
- **驱动**：`modernc.org/sqlite` 纯 Go，`CGO_ENABLED=0`，交叉编译安全。`config.stats.{db_path, retention}`（默认 `~/.model-proxy/stats.db`，30d；`0`=永久），每 tick 跑 `prune`。
- **优雅退出**：SIGINT/SIGTERM 触发最终 flush + pid 清理（覆盖 supervisor 转发的 worker SIGTERM）。
- **查询**：`GET /api/stats?from=&to=&provider=&model=&bucket=`（`bucket` 如 `10m`/`1h`，默认 `1m`）按 SQL `GROUP BY` 聚合到宽桶展示（存储恒 1 分钟，聚合只减行不丢精度）；`model-proxy stats` CLI 渲染（`--bucket`/`--json`）。`POST /api/tokens/reset` -> `Proxy.resetStats` 清零内存 + SQLite + flusher 基线。
- **锁纪律**：metrics 用原子 + 短暂 `metrics.mu`（map get-or-create）；token 用 `tokenMu`；stats DB 自己的锁 -- 都是**独立叶子锁**，不与 `p.mu`/`healthMu`/`quotaMu` 嵌套。

### compass 网关契约（实测）

> **命名说明**：model-proxy 内部将该 provider 命名为 `aqp`（config 的 `provider_id`、凭据文件 `aqp_oauth_auth.json`、字段 `aqp_mint_url`）；上游服务本身是 `compass.llm.shopee.io`，故本节保留 "compass / CQP" 称谓以匹配逆向出的真实后端契约。


| 端点 | 方法 | 鉴权 | 路径 |
|---|---|---|---|
| CQP key 签发 | POST | SSO cookie | `/api/v1/cqp/ccswitch/api_key/get_or_generate` body `{}` |
| 模型列表 | GET | CQP Bearer | `/compass-api/v1/models` |
| 消息（anthropic） | POST | CQP Bearer | `/compass-api/v1/messages` |
| 响应（openai） | POST | CQP Bearer | `/compass-api/v1/responses` |
| 月度用量 | POST | SSO cookie | `/api/v1/cqp/ccswitch/monthly_usage` body `{"project_id":"..."}` |

- CQP key 有效期长，缓存 50min
- SSO cookie 值已含 `SSO_C=` 前缀，直接作 Cookie 头值
- `/v1/messages` 走 cqp 时代理需加 `?beta=true`、`anthropic-version: 2023-06-01`、`x-compass-request-id`(UUID)
- 转发头用白名单（不透传客户端 Cookie/Authorization）

### codex 后端契约（实测）

| 端点 | 方法 | 鉴权 | 备注 |
|---|---|---|---|
| responses | POST | codex OAuth Bearer | `/backend-api/codex/responses`，需 `store:false` + `stream:true`，不接受 `max_tokens`；body 用 Responses API 的 `input`（**必须是 list**，字符串被拒 `Input must be a list`；不能用 `messages`，被拒 `Unsupported parameter: messages`） |
| usage | GET | codex OAuth Bearer | `/backend-api/wham/usage`（注意：不在 `/codex/` 子路径下） |
| models | GET | codex OAuth Bearer | `/backend-api/codex/models?client_version=<ver>`，返回 `{"models":[{slug,visibility,...}]}`，仅取 `visibility=="list"`；`client_version` 决定可见模型（过低则新模型不返回） |
| originator | header | — | `originator: codex_cli_rs` 必须设，否则 403 |
| ChatGPT-Account-Id | header | — | 从 id_token JWT 解析 |

### codex OAuth device flow（从 codex-rs 源码确认）

```
issuer = https://auth.openai.com
client_id = app_EMoamEEZ73f0CkXaXp7hrann

1. POST /api/accounts/deviceauth/usercode  {"client_id":...}
   → {device_auth_id, user_code, interval}  # interval 是字符串 "5"
2. 用户访问 https://auth.openai.com/codex/device 输入 user_code
3. POST /api/accounts/deviceauth/token  {"device_auth_id":..., "user_code":...}
   轮询，错误码：deviceauth_authorization_pending / deviceauth_slow_down
   成功 → {authorization_code, code_challenge, code_verifier}
4. POST /oauth/token  grant_type=authorization_code → {access_token, refresh_token, id_token}
```

刷新：`POST /oauth/token` + `grant_type=refresh_token` → 新 token 轮换。

**关键决策**：代理用独立 OAuth（不读 codex CLI 的 `~/.codex/auth.json`），避免 refresh_token 轮换竞争。

### Zhipu BigModel 契约

- API base (OpenAI): `https://open.bigmodel.cn/api/paas/v4`（`/chat/completions`、`/models` 等）
- Anthropic base: `https://open.bigmodel.cn/api/anthropic/v1`（`/v1/messages`，Bearer 鉴权，返回标准 Anthropic `message` 响应；实测 200）。config 里用 `openai_base_url` + `anthropic_base_url` 分别配置，代理按协议转发
- 鉴权: `Authorization: Bearer <api_key>`（用户输入，存 `~/.model-proxy/<name>_apikey.json`）；OpenAI 与 Anthropic 端点都用 Bearer（不像 DeepSeek 需要 x-api-key）
- `/models` 端点只返回 8 个文本对话模型；多模态模型（glm-4v-plus/cogview-4-plus）不列在其中，需手动加到 config
- 配额端点 `GET https://open.bigmodel.cn/api/monitor/usage/quota/limit`（Bearer api_key 鉴权）→ `{success, data:{limits:[{type,unit,number,percentage,nextResetTime,usage,currentValue,remaining,usageDetails:[{modelCode,usage}]}], level}}`；`type`=TOKENS_LIMIT|TIME_LIMIT，`unit` 3=5h/6=weekly/5=monthly。**`currentValue`=已用、`remaining`=剩余、`usage`=总额**（不要把 `usage` 当已用——它是总额，等于 currentValue+remaining）。`/users/balance` 和 `/users/usage` 都 404
- `usageDetails` 拆分：`TIME_LIMIT`（月度）按 **MCP 工具**拆分（search-prime/web-reader/zread 等，是工具调用消耗而非模型 token）；`TOKENS_LIMIT` 按**模型**拆分。显示时前者标 `by MCP tool`、后者标 `by model`

### DeepSeek 契约（双协议，一个 key）

- OpenAI base: `https://api.deepseek.com`（`/chat/completions`、`/responses`、`/models`、`/user/balance`，Bearer 鉴权）
- Anthropic base: `https://api.deepseek.com/anthropic`（`/v1/messages`，`x-api-key` 鉴权；`anthropic-version`/`anthropic-beta` 被忽略）
- **关键**：Anthropic SDK 打 `/anthropic/v1/messages`（base + `/v1/messages`）。代理剥掉客户端的 `/v1`，故 `anthropic_base_url` 须自带 `/v1`（`https://api.deepseek.com/anthropic/v1`），代理按协议转发：anthropic→`anthropic_base_url`、openai→`openai_base_url`。`RewriteRequest` 不再改 URL（no-op），URL 选择在 `proxy.forward` 按 protocol 完成
- 鉴权双写：每个请求同时设 `Authorization: Bearer` 和 `x-api-key`，一个 config 服务两种协议
- 服务端模型自动映射（Anthropic）：`claude-opus*`→`deepseek-v4-pro`；`claude-sonnet*`/`claude-haiku*`→`deepseek-v4-flash`
- `/user/balance` → `{is_available, balance_infos:[{currency, total_balance, granted_balance, topped_up_balance}]}`（注意是 `balance_infos` 非 `wallets`）
- `/models` → OpenAI 风格 `{object:"list", data:[{id,object,owned_by}]}`；当前模型 `deepseek-v4-pro`/`deepseek-v4-flash`（1M ctx，384K max output）。旧名 `deepseek-chat`/`reasoner` 2026-07-24 弃用
- 凭据存 `~/.model-proxy/<name>_apikey.json`

### 火山方舟 Volcengine Ark 契约（双协议，含 Agent Plan，一个 key）

- OpenAI base: `https://ark.cn-beijing.volces.com/api/plan/v3`（Agent Plan 套餐；标准 Ark 是 `/api/v3`）。`/chat/completions`、`/responses`、`/models`，Bearer 鉴权
- Anthropic base: `https://ark.cn-beijing.volces.com/api/plan/compatible/v1`（Anthropic-compatible，供 Claude Code；标准 Ark 是 `/api/compatible`）。`/v1/messages`，`x-api-key` 鉴权。代理按协议转发：anthropic→`anthropic_base_url`、openai→`openai_base_url`。`anthropic_base_url` 须自带 `/v1`（代理剥掉客户端 `/v1`）
- 鉴权双写：每请求同时设 `Authorization: Bearer` 和 `x-api-key`（OpenAI 端点用 Bearer，Anthropic 端点用 x-api-key），一个 key 服务两种协议（实现同 DeepSeek，`provider/volcengine.go`）
- Agent Plan 的 5h/每日/周/月额度在 **GetAFPUsage**（火山引擎签名 OpenAPI：`Action=GetAFPUsage&Version=2024-01-01&serviceCode=ark`，管控面、HMAC-SHA256/V4 签名，需 AccessKey/SecretKey）—— Ark API Key（Bearer，仅对话）调不了（实测 `/api/v3/models`→401、`/api/plan/v3/models`→404）。**已实现**：`volcengine_sign.go` 做 V4 签名（CredentialScope `{date}/cn-beijing/ark/request`，signing key 链 SK→kDate→kRegion→kService→kSigning，**末项 `"request"` 非 `"volcengine_request"`**；签名头仅 `host;x-date`，**不含 `x-content-sha256`**）。`login volcengine` 同时收 Ark API Key + AK/SK。`usage volcengine` 调 GetAFPUsage 解析 `Result.{AFPFiveHour,AFPDaily,AFPWeekly,AFPMonthly}`（各含 `Quota/Used/ResetTime`，Remaining=Quota−Used）。`models refresh volcengine` 调 **ListArkAgentPlanModel**（同理 V4 签名）解析 `Result.Datas[].ModelID`（当前 21 个原始 ID，含 doubao/glm-5.2/kimi/minimax/deepseek + 5 个非对话模型；`models refresh` 经正则过滤 `*-latest`/`doubao-seed-1-*`/lite/mini + endpoint 探测剔除非对话模型后写入）。未配 AK/SK 时退化为列 config 模型
- 模型 ID 是模型名（如 `doubao-seed-1-8-251228`、`doubao-seed-2-0-code`），非推理接入点 endpoint id（标准 Ark 按量计费才用 endpoint id）
- 凭据存 `~/.model-proxy/<name>_apikey.json`

### models.dev 元数据补充契约（`modelsdev.go`，实测）

- 数据源 `GET https://models.dev/api.json`：`{providerKey: {api, models: {modelId: {limit:{context,output}, modalities:{input,output}, cost, …}}}}`。155 providers / 244 去重模型，**raw 3.05 MB**（5478 个 provider×model 对，reseller 大量重复）
- **体积优化（实测）**：gzip 后 **~286 KB**（Go Transport 自动加 `Accept-Encoding: gzip` 并透明解压——**勿手动设该 header**，否则关掉自动解压）；`If-None-Match`→**304 返回 0 字节**（已验证）。磁盘缓存只存**去重 slim 投影**（`by_name` 244 项 + `by_endpoint`，~150 KB），**绝不存 3 MB 原始 blob**；全量解析只在 `200` 刷新时发生
- 缓存：`~/.model-proxy/models_cache.json`，TTL **24h**，atomic tmp+rename。`ensureCatalogFresh`：fresh→直接用；stale/force→conditional GET（304 仅刷新 `fetched_at`，200 重建+落盘）；fetch 失败+有旧缓存→用旧缓存+stderr 提示；无缓存→空 catalog（命令照跑，所有模型走 default）
- **匹配优先级**：① endpoint 命中（provider 的 `openai_base_url`/`anthropic_base_url` 归一化后比对 models.dev provider 的 `api`，全 URL → 再 host）→ 在该 provider 的模型列表里查；② 全局模型名后缀匹配（救 aqp 借的 `glm-*`/`deepseek-*`）；③ 未命中→default。`by_name` 去重时 canonical owner 胜（`zhipuai`/`deepseek`/`openai`/`moonshotai`/… rank 0，reseller rank 1）
- **`models:` 只配名字；元数据全来自 models.dev**：config 字段 `Provider.Models []string`（名字列表，`- glm-5.2` 形式），**不存元数据**。元数据（context/output/modalities）运行时由 models.dev 补（`hydrateModels` 返回 `meta`+`sources`），匹配失败→default。effective 集合 = config 名字列表 ∪ routes 引用的模型
- **`models refresh <provider>` 写 config**：拉取 upstream 模型列表，与 config 现有 `models:` 合并去重，再**逐个 endpoint 探测**（`models_check.go:checkProviderModels`->`probeModelCallable`，复刻 `forward` 的 base/path/RewriteRequest/AuthHeaders 按协议调用 provider 自己的 `base_url`，发最小请求）——仅 **2xx** 的保留，其余丢弃并在 stderr 输出模型名+原因。保留集**覆盖写**回 `models:`（非 append-only——既有不可调项与新拉取失败项一并删除）。provider 专属的静态策略过滤在 provider 实现的 `FilterModelIDs`（volcengine 剔除 `*-latest`/`doubao-seed-1-*`/lite/mini；其余透传）。安全网：探测 infra 不可用->写未校验合并集；**全部**探测失败（疑似未登录/断网）->保留 config 不清空并告警。写是保注释的 yaml.Node 往返（`writeProviderModels`：`loadConfigNode`->`setChildNode`/`mustEncode`->`LoadConfigFromBytes` 校验->`backupConfig`->`atomicWrite`），只重编码 `models:` 序列。写后热重载 daemon。**只有 `models refresh` 写 config.yaml；`models`/`takeover` 显示永不写**
- **覆盖盲区**：models.dev **没有** aqp/compass、codex/ChatGPT、volcengine 这几个 provider；codex `gpt-5.5`、volcengine `doubao-*` 等未命中→走 default（`ctx=200000 out=16384 text-only`），`takeover` 对 opencode/pi 发 stderr 警告（claude/codex 不写每模型元数据，不警告）
- **作用域**：仅 `models`/`takeover` CLI 路径调 `ensureCatalogFresh`+`hydrateModels`（hydrate 只改内存 cfg，**不进** `LoadConfig`/daemon）；代理热路径、`GET /v1/models`、quota 均不受影响。`models pull` 强制刷新 catalog 缓存；`MP_MODELSDEV_URL` 覆盖端点（测试/镜像）。`models` 显示新增 `SRC` 列（`models.dev`/`default`）

### 隐式路由（implicit routes，实测）

- **无 route 的 model 不再直接 502**：若某个 **已 login** 的 provider 的 `models:` 列表里有这个名字，`synthesizeImplicitRoutes`（在 `NewProxy`/`reload` 里跑——此时 `loadPool` 的 login 状态已知）自动合成一条单目标 route `{首个按字母序的已登录 provider, model, priority 1}`，merge 进 `expandedRoutes`（显式 route 永远优先）。`forward`/`/debug/schedule`/`/api/status.schedule`/serve-status 自动看到；`GET /v1/models` 也列出（可调 ⇒ 可列）
- **多 provider 歧义**：同一名字被 >1 个已登录 provider 提供且无显式 route → 只用首个，其余忽略，发 **歧义警告**，出现在 `model-proxy models`（stderr `⚠`）和 `/api/status` 的 `warnings`（Status 卡 + `serve status`）。单 provider 的隐式路由静默
- **login 判定**：`loggedInProviders(cfg)` 用 `loadPool(name, provID).Accounts > 0`（plural pool 或 legacy 单文件都算）。`buildProviders` 不返回 login 状态（无凭据的 provider 仍被 file-backed 构建进 map），所以 login 要单独算
- **作用域**：隐式路由只在 daemon 侧（`expandedRoutes`）生效；`doctor` 离线、不知道 login，仍只看显式 `cfg.Routes`。`snapshotSticky` 读 `cfg.Routes`，隐式路由的 sticky 不持久化（单 provider 无需 sticky）

### 踩过的坑

1. **路径双 `/v1`**：provider openai_base_url 已含 `/compass-api/v1`，client path `/v1/messages` 拼接后变双 `/v1`。需剥 client 的 `/v1` 前缀。
2. **codex 后端请求体**：`store:false`（否则 400）+ `stream:true`（否则 400）+ 无 `max_tokens`（否则 400）。代理在 RewriteRequest 自动注入 `store:false`。**body 是 Responses API 形状**：用户输入在 `input` 字段且**必须是 list**（字符串 -> `Input must be a list`；用 `messages` -> `Unsupported parameter: messages`）。`models refresh codex` 的探测据此构造 body。
3. **monthly_usage**：POST 非 GET，需 `project_id` 入参。字段名 `total_amount`/`usage`/`balance`/`plan`（非 `totalAmount`）。
4. **wham/usage 路径**：`/backend-api/wham/usage`，不是 `/backend-api/codex/wham/usage`（后者 403）。
5. **日志掩码**：SSO cookie 必须用 `mask()`（首2…尾2），auth/info 响应体只记长度。
6. **文件日志无色**：`--log-file` 时 `logColorEnabled` 置 `false`，否则 ANSI 污染日志文件。
7. **flushCopy 写错误**：客户端断开后 `w.Write` 返回错误须立即 break，否则代理继续拉上游流浪费 compute。
8. **codex /models 的 client_version 闸门**：`/backend-api/codex/models` 必须带 `client_version` 查询参数；后端据此决定返回哪些模型，版本过旧则新模型（如 gpt-5.6）不返回。model-proxy 的解析顺序：config `client_version` → `codex --version` → `~/.codex/models_cache.json` → 内置常量。
9. **context 传播**：用 `http.NewRequestWithContext(r.Context(), ...)` 让客户端取消传播到上游。
10. **supervisor nil panic**：`spawnWorker` 失败时返回 nil，`runSupervisor` 需检查再处理。
11. **配额陈旧保护**：snapshot 老于 `3×quota_poll_interval` 一律视为 `BillingUnknown`（不再相信缓存值）。`Quota()` 失败的 provider 也是 `BillingUnknown`（按 priority 排，**绝不**当 payg）。
12. **volcengine 配额需 AK/SK**：`GetAFPUsage` 是火山引擎签名 OpenAPI（管控面），Ark API Key（Bearer，仅对话）调不了；`login volcengine` 必须同时收 AK/SK 才能产 quota。
13. **锁顺序 `healthMu` → `quotaMu`**：`schedule` 先 `allSnapshots()`（quotaMu RLock）拿到快照，再 `healthMu.Lock()`；绝不在持 `healthMu` 时回调 quota 接口，否则与 `refreshOne`/`pollAll` 反向嵌套死锁。
14. **`BillingClass` iota ≠ 调度序**：`Unknown=0, Plan=1, PayG=2`，但调度序是 `Plan < Unknown < PayG`，故 `tierRank` 单独映射（不要直接比较 `BillingClass` 常量）。

### opencode/pi takeover 配置

| 客户端 | baseURL 格式 | 关键差异 |
|---|---|---|
| opencode | `http://<proxy>/v1` | `@ai-sdk/anthropic` 拼 `baseURL+/messages`，baseURL 要带 `/v1` |
| pi | `http://<proxy>` | pi 的 `anthropic-messages` 自己拼 `/v1/messages`，baseURL 不带 `/v1`（否则 `/v1/v1/messages` → 502） |

provider_id 统一为一个配置项（默认 `model-proxy`），opencode/pi/codex 共用。备份文件存 `<configDir>/.model-proxy/<client>.bak`。

