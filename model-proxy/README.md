# model-proxy

多 Provider LLM 代理 — 统一管理 AQP/codex/Zhipu 等上游后端，按协议（Anthropic/OpenAI）对外暴露，自动处理鉴权、模型映射、流式转发。

## 架构

```
┌─────────────┐     ┌───────────────────────────────────┐     ┌──────────────┐
│  客户端      │────▶│  model-proxy                      │────▶│  Provider    │
│  claude/     │     │  ┌─────────┐  ┌────────────────┐ │     │  aqp         │
│  opencode/   │     │  │ routes  │→│ provider (auth) │ │     │  codex       │
│  codex/      │     │  │ (proto) │  │ RewriteRequest  │ │     │  zhipu       │
│  curl/SDK    │     │  └─────────┘  └────────────────┘ │     │  ...         │
└─────────────┘     └───────────────────────────────────┘     └──────────────┘
```

- **Provider 层**（`provider/` 包）：每个上游后端是一个 Provider 实现，封装鉴权、请求改写、登录、用量查询
- **Routes 层**：对外暴露模型名 → 一组 `provider/model` 目标。调度先看非高峰（provider 的 `peak_hours`），再看 `priority`，失败逐一 failover。anthropic 协议先经 `claude_mapping` 把 claude-* 别名翻译成对外模型名，再查路由
- 凭据由 `login <provider>` 管理，存储在 `~/.model-proxy/<name>_<suffix>.json`，不落 config

## 构建

```bash
cd model-proxy
go build -o model-proxy .          # 原生编译（host）

# 交叉编译（纯 Go，CGO_ENABLED=0，全静态，无需交叉工具链）
scripts/build.sh                   # 编译 host -> dist/ + ./model-proxy
scripts/build.sh linux/amd64       # 交叉编译单个目标 -> dist/
scripts/build.sh --strip all       # 全矩阵（linux/darwin/windows），-s -w 去符号
```

`scripts/build.sh` 从 `git describe --tags --always --dirty` 注入版本号（`-ldflags -X main.version`，覆盖 `version.go` 的 `dev` 默认值，显示在 `serve status` / `/api/status`）。每个目标写 `dist/model-proxy-<goos>-<goarch>`（windows 加 `.exe`）；host 构建额外复制到 `./model-proxy`（可原地运行）。Flag：`--version <v>`、`--out <dir>`（默认 `dist`）、`--strip`（`-s -w`）、`-v`。`dist/` 和 `./model-proxy` 都在 .gitignore 里。

## 配置

`config.yaml`（`model-proxy config init` 生成模板）。查找顺序：`--config PATH` > `~/.model-proxy/config.yaml` > `./config.yaml`。路径字段支持 `~/` 展开 和 `env:ENV_VAR` 前缀（从环境变量读值，如 `log_file: env:MP_LOG_FILE`）。

```yaml
listen: 127.0.0.1:15721
log_level: info

providers:
  aqp:
    provider_id: aqp
    openai_base_url: https://compass.llm.shopee.io/compass-api/v1
    aqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
    models:                       # 只填模型名；元数据(context/output/modalities)运行时从 models.dev 自动补
      - glm-5.2
  codex:
    provider_id: codex
    openai_base_url: https://chatgpt.com/backend-api/codex
    models:
      - gpt-5.5
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    usage_url: https://open.bigmodel.cn/api/paas/v4/models
    models:
      - glm-5.2

claude_mapping:
  claude-opus-4-7: glm-5.2          # anthropic-only: claude 别名 → 对外模型名
  claude-sonnet-4-6: deepseek-v4-pro

routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
    - {provider: zhipu,   model: glm-5.2, priority: 2}   # failover 备选
  gpt-5.5:
    - {provider: codex, model: gpt-5.5, priority: 1}

takeover:
  claude: ~/.claude/settings.json
  opencode: ~/.config/opencode/opencode.json
  codex: ~/.codex/config.toml
  pi: ~/.pi/agent/models.json
  provider_id: model-proxy

# web:                      # 管理后台（默认开启，仅 loopback，无鉴权）
#   enabled: true
# stats:                    # 调用统计持久化（SQLite，默认开启，30 天保留）
#   db_path: ~/.model-proxy/stats.db
#   retention: 30d          # 0 = 永久
```

> **模型元数据**：`models:` 只填模型名，`context`/`output`/`modalities` 在运行时从 [models.dev](https://models.dev) 自动补全（缓存于 `~/.model-proxy/models_cache.json`，24h TTL，ETag `304`-aware；`models pull` 强制刷新）。匹配不到的模型走保守默认值并在 `takeover` 时告警。`MP_MODELSDEV_URL` 环境变量可覆盖 models.dev 端点（测试/镜像用）。
>
> **隐式路由**：某个模型即使没在 `routes` 里配，只要某个**已登录** provider 的 `models:` 列了它，代理会自动按模型名路由到（字母序）首个 provider。若多个已登录 provider 都提供且无显式 route，只用首个并在 `models` 命令 / Web UI 发出歧义告警。显式 `routes` 永远优先（要做 failover/优先级控制仍需显式配置）。

## 用法

```bash
# 登录（凭据存储在 ~/.model-proxy/<name>_<suffix>.json；apikey 类重复 login 累积多账号池）
model-proxy login aqp          # AQP SSO 浏览器登录
model-proxy login codex            # codex OAuth device flow
model-proxy login zhipu            # 输入 Zhipu API key（--label NAME 命名；重复 login 加进池）
model-proxy login deepseek         # 输入 DeepSeek API key（可重复 -> 多账号）
model-proxy login volcengine       # Ark API Key + AccessKey/SecretKey（可重复 -> 多账号）
model-proxy login zhipu --label work --replace   # 命名账号 / 覆盖已存在的同 id 账号

# 启动代理
model-proxy serve                  # 前台
model-proxy serve daemon           # 后台（自动重启）
model-proxy serve stop             # 停止 daemon
model-proxy serve reload           # 热加载配置（SIGHUP）
model-proxy serve status           # 运行状态（providers/路由/配额/token）
# serve 通用 flag：--config <PATH>、--log-file <PATH>（覆盖 config 的 log_file）

# 查看用量
model-proxy usage aqp          # 月度用量/余额
model-proxy usage codex            # credits/spend/rate limits
model-proxy usage zhipu            # 5h/周/月配额 + token 消耗（池化时逐账号展示全部账号）
model-proxy usage deepseek         # 账户余额（is_available + 各币种）
model-proxy usage volcengine       # Agent Plan 5h/日/周/月额度（需 AK/SK；否则列 config 模型）

# 登出
model-proxy logout aqp         # 清除凭据文件
model-proxy logout zhipu --label work   # 删指定账号（apikey 类池）
model-proxy logout zhipu --all          # 清空整个池

# 模型列表
model-proxy models                 # 所有 provider 的模型（元数据从 models.dev 自动补；SRC 列标来源）
model-proxy models aqp             # 单个 provider
model-proxy models refresh zhipu   # 从服务端拉取模型，逐个探测校验后覆盖写回 config（无 /models 端点时回退探测路由模型）
model-proxy models pull            # 强制刷新 models.dev 元数据缓存

# 接管客户端配置
model-proxy takeover opencode      # claude|opencode|codex|pi|all
model-proxy restore opencode

# 配置管理
model-proxy config init            # 生成模板
model-proxy config print           # 打印生效配置
model-proxy config check           # 校验配置

# 调度诊断
model-proxy schedule               # 查询运行中的 daemon：每 model 当前调度到哪个 provider（GET /debug/schedule）
model-proxy doctor                 # 离线 config 调度诊断（tier/quota/peak + dry-run 顺序 + warning）

# 调用统计（需 daemon + web.enabled）
model-proxy stats                  # 最近 60min 的 per-(provider,model) 调用统计（reqs/failover/429/fail/input/output）
model-proxy stats --bucket 1h      # 按小时聚合展示
model-proxy stats --from 1h --to now --provider zhipu   # 时间范围 + 过滤
model-proxy stats --json           # 原始 JSON（便于 jq）
```

## Web UI

代理内置一个管理后台（admin UI），在 `http://127.0.0.1:<listen>/ui/`（如 `listen: 127.0.0.1:15721` → <http://127.0.0.1:15721/ui/>）。**默认开启，仅在 loopback 监听，无鉴权**（本地可信）。三个标签页：

- **Status** — 实时面板：uptime / 版本 / listen 地址、每 provider 的熔断/限频状态、配额快照、每路由当前调度选择（来自 `GET /debug/schedule`）、请求计数器（requests/failovers/429/failures）、观测到的 token 用量（按 provider×model）。
- **Config** — 原始 YAML 编辑器（GET 返回原文件、POST 经 `validate → backup(.bak) → atomic write → reload` 流水线落盘 + 热重载）+ 结构化编辑表单（`general` / `scheduling` / `provider` / `route` / `claude_mapping`，通过 yaml.Node API **保留注释与键序**）。
- **Accounts** — 列出每个 provider 的账号（`id` / `label` / `added_at`，aqp/codex 额外显示 email；**响应结构里根本没有 key 字段，secret 不可能被序列化出去**）；apikey 类 provider（zhipu/deepseek/volcengine）可在 UI 添加/删除账号；aqp/codex 走**异步登录**（点 "Add account" 弹模态框 → 浏览器完成 SSO / OAuth device flow → UI 轮询 `/api/login/<session>/poll` 直到 `done`/`error`）。

**所有写操作都会即时热重载运行中的 serve（进程内 `p.reload`，无需重启）**：改 config、增删账号、aqp/codex 登录完成 —— 改动立即生效。账号增删虽不改 `config.yaml`，但 reload 会重建 providers（重新读池文件），新加/删除的账号随即（取消）展开成虚拟 provider；reload 还会顺手清空熔断/限频/粘性状态，所以 UI 改动也是"给卡住的 provider 复位"的手段。

关闭 UI：

```yaml
web:
  enabled: false
```

JSON 接口在 `/api/*`（`status` / `logs?tail=N` / `config` GET·POST / `config/edit` / `accounts` GET·POST·DELETE / `tokens` / `tokens/reset` / `login/<provider>/start` + `login/<session>/poll`）；底层契约（请求/响应 shape、SSE token 扫描器语义）见 `AGENTS.md` 的「Web UI + /api/* 接口契约」一节。前端是嵌入式的静态资源（`web_assets/`，`go:embed`），无独立构建步骤。

## `serve status`（终端状态面板）

`model-proxy serve status` 是 Web UI **Status 标签页的终端等价物**——一次性拉取运行中 daemon 的 `/api/status` + `/api/tokens`（带 `--logs` 时再加 `/api/logs`），按终端优化输出（列对齐 / 着色 / 紧凑数字）。需要 daemon 在跑、且 `web.enabled`（默认 true）。

```bash
model-proxy serve status            # 头部 + Providers + Schedule + Quota + Tokens
model-proxy serve status --logs     # ……再加最近 20 行日志
model-proxy serve status --logs 5   # ……最近 5 行
model-proxy serve status --json     # 合并的原始 JSON（{status, tokens[, logs]}，方便 jq）
model-proxy serve status --config /path/to/config.yaml   # 指定 config（从而选 listen 地址）
```

显示内容（与 Status 标签页一致）：

- **头部**：`v<version> · <uptime> · <listen>`
- **Providers**：每个 provider 的健康状态（`available` / `circuit open` / `rate-limited` / `unavailable`）+ 计数器（reqs / failovers / 429 / failures / 最后请求时间）
- **Schedule**：每路由首选 provider + ordered 列表（tier / surplus / priority / 可用 / peak）+ sticky 驻留
- **Quota**：每 provider 的配额窗口（ultimate / short）+ 剩余百分比 + 进度条 + 重置时间
- **Tokens**：按 provider × model 的观测用量（input / output / cache）
- **Logs**（仅 `--logs`）：最近 N 行日志

错误处理：daemon 没在跑 → `✗ cannot reach daemon at <listen>: … is 'model-proxy serve' running?`；`web.enabled: false`（`/api/status` 返 404）→ 提示开启 Web UI。每次请求带 10s 超时，daemon 卡死会快速失败而不是一直挂起。`--json` 适合脚本，如 `model-proxy serve status --json | jq .status.quota`。

## `stats`（调用统计）

`model-proxy stats` 从 daemon 的 `/api/stats` 拉取 SQLite 存储的 per-(provider, model) 调用统计并按终端表格输出。需 daemon 在跑 + `web.enabled`（默认 true）。统计在 hot path 只更新内存计数器，由后台每分钟 flush 到 `~/.model-proxy/stats.db`（重启不丢；SIGINT/SIGTERM 触发最后一次 flush）。

```bash
model-proxy stats                             # 最近 60min（默认），1 分钟桶
model-proxy stats --bucket 10m                # 按 10 分钟桶聚合展示（存储恒为 1 分钟，聚合仅展示）
model-proxy stats --from 2h --to now          # 时间范围（unix 秒或 RFC3339；默认 60min 前..now）
model-proxy stats --provider zhipu --model glm-5.2   # 过滤
model-proxy stats --json                      # 原始 JSON（便于 jq）
```

输出列：`provider · model · <bucket> · reqs · failover · 429 · fail · input · output`（紧凑数字）。空结果 -> `(no stats in range <FROM> .. <TO>, bucket <BUCKET>)`。`POST /api/tokens/reset`（或 Web UI）可清零内存 + SQLite + flush 基线。配置：`config.stats.{db_path, retention}`（默认 `~/.model-proxy/stats.db`，30 天；`0` = 永久）。

## Token 文件

凭据由 `login` 管理，按 provider name 派生路径，不落 config：

| Provider | Token 文件 | 内容 |
|---|---|---|
| aqp | `~/.model-proxy/aqp_oauth_auth.json` | SSO cookie + account data（单账号） |
| codex | `~/.model-proxy/codex_oauth_auth.json` | OAuth access/refresh/id token（单账号） |
| zhipu | `~/.model-proxy/zhipu_apikey.json` | API key（单账号遗留文件，只读回退） |
| deepseek | `~/.model-proxy/deepseek_apikey.json` | API key（同上） |
| volcengine | `~/.model-proxy/volcengine_apikey.json` | `{api_key, access_key, secret_key}`（同上） |

**多账号凭据池**：apikey 类 provider（zhipu/deepseek/volcengine）重复 `login` 会把账号累积进**池文件** `~/.model-proxy/<name>_apikeys.json`（`{version, accounts:[{id, label, api_key, (access_key, secret_key), added_at}]}`），按账号 id（volcengine=access_key，其余=sha256(api_key)[:16]）去重。运行时每个池被展开成 N 个虚拟 provider（`<name>#<accountId>`），共享父配置但各绑自己的凭据；路由目标命名父 provider 会 fan-out 到全部账号。**路由跨池是会话粘性的**：按请求的 `x-claude-code-session-id` 粘同一个账号（保 prompt cache），新会话 round-robin 分到不同账号（并发散开）；只有 429/熔断才换账号。`usage <provider>` 逐账号展示全部账号。aqp/codex 是单凭据（不入池）。`login --label`/`--replace`、`logout --label`/`--all` 管理池内账号；Web UI Accounts 标签页也能增删。

多实例支持：同一 `provider_id` 可有多个不同 name（如 `zhipu-personal` / `zhipu-work`），各自独立凭据文件/池。

## 协议

代理按 URL 路径前缀路由，对外协议 = 转发协议（不做转换）：

| 协议 | 端点 | 转发到 |
|---|---|---|
| Anthropic | `POST /v1/messages` | provider 的 `/messages` |
| OpenAI | `POST /v1/responses`, `/v1/chat/completions` | provider 的同路径 |
| 模型列表 | `GET /v1/models` | 合并 routes + 隐式路由 + claude_mapping 的模型名 |

**按协议转发到不同 endpoint**：provider 用 `openai_base_url`（默认 base，用于 OpenAI 协议 + `/models` + `usage`）和可选的 `anthropic_base_url`（覆盖 anthropic 协议；不设则用 `openai_base_url`）。如 DeepSeek 的 OpenAI 与 Anthropic 是两个不同 base。注意代理会剥掉客户端的 `/v1` 前缀，故 base URL 须自带版本段（如 `…/v1`、`…/anthropic/v1`）。

## 调度与熔断（`scheduling`）

每个对外模型可配多个 `provider/model` 目标。代理按下面的规则选目标、失败逐一 failover，并对持续出错的 provider 熔断，避免每请求都去撞一个挂掉的上游：

- **熔断**（超时 / 5xx / 连接错误 / 401 刷新后仍失败）：连续 `circuit_threshold`（默认 3）次 → 开路 `circuit_cooldown`（默认 10m），后半开放 1 个探针请求，成功关、失败再开。开路期间该 provider 被跳过。
- **限频跳过**（429）：按 `Retry-After` 头（秒或 HTTP 日期）跳过，没有头则用 `rate_limit_backoff`（默认 60s）。不计入熔断。覆盖 5 小时/周配额窗口和秒级频率限制——时长来自响应。
- **粘性驻留**（`sticky_dwell`，默认 10m）：每个路由「停」在一个 provider 上，在驻留窗口内优先用它（保 prompt cache，不为已恢复的高优先 provider 频繁回切）；只有它熔断/限频或驻留到期才换。10m ≈ 2× 缓存 TTL（~5m）：够保住活跃会话缓存、扛过短暂抖动，又能在有限时间内回到首选 provider。
- **上游超时**（`upstream_timeout`，默认 30s）：每个上游请求带超时，挂起的上游会快速失败进入熔断/failover，而不是无限拖住请求。

```yaml
scheduling:
  circuit_threshold: 3
  circuit_cooldown: 10m
  rate_limit_backoff: 60s
  upstream_timeout: 30s
  sticky_dwell: 10m
  quota_poll_interval: 5m   # 后台 Quota() 轮询周期
  quota_switch_margin: 15   # 切换 provider 的 quota 边际（百分点）
```

## 配额感知调度（quota-aware scheduling）

代理后台轮询每个 provider 的剩余配额（`Provider.Quota()`，每 `quota_poll_interval` 默认 5m 一次），缓存到 `~/.model-proxy/quota_state.json`（启动时作为基线加载，**并携带每路由 `sticky` 选择，重启后恢复 → 保 prompt cache**），并据此排序路由目标：

- **调度分 = surplus**（provider 接口方法 `Provider.Surplus`）：`surplus = (最终窗口.remaining − 短窗口.remaining × (短窗口配额/最终窗口配额) × (peakMult−1)) − 时间剩余比例`。surplus>0 = 落后节奏（不用就浪费 → 优先用）；<0 = 超前（会提前耗尽 → 回避）。最终窗口（总预算）：zhipu=周、volcengine/codex/aqp=月、deepseek=按量（无窗口）。
- **三层 tier**：`plan`（按 surplus 排）< `unknown`（按 priority 排）< `pay-as-you-go`（严格兜底，仅当所有 plan provider 都不可用）。设 `billing: pay-as-you-go` 即兜底（如 DeepSeek）。排序 `(tier, priority asc, surplus desc)` —— **priority 压过 surplus，surplus 只在同 priority 间决定**。
- **peak 只烧短窗口**：`peak_hours` 的 multiplier 只折算 provider 的短 rate-cap 窗口（5h 等）；没有短窗口的 provider（codex/aqp、未轮询的）高峰不打折。`peak_hours` 支持多段、每段独立 multiplier；multiplier=1 关闭。
- **粘性切换**：路由停在一个 provider 至少 `sticky_dwell`（保 prompt cache）；到期后仅当另一 provider 在 **tier / priority / surplus 边际（`quota_switch_margin`，默认 15 pts）** 任一更优时才换 —— 既能短暂抖动后回首选，也能在他人明显领先时切换。

```yaml
providers:
  deepseek:
    provider_id: deepseek
    billing: pay-as-you-go          # 严格兜底（仅当所有 plan provider 不可用）
    # ...
  zhipu:
    provider_id: zhipu
    billing: plan                   # 默认；显式写也可
    peak_hours:                     # 三种写法都支持
      - {window: "14:00-18:00", multiplier: 2}
    # ...

scheduling:
  quota_poll_interval: 5m
  quota_switch_margin: 15
```

`Quota()` 来源：zhipu/codex/volcengine/aqp → plan tier；deepseek → pay-as-you-go。volcengine 的 `GetAFPUsage` 需 AccessKey/SecretKey（Ark API Key 调不了），未配时该 provider 退化为 `unknown`。

**查看调度**：`model-proxy schedule` 查询运行中的 daemon，显示每个 model 当前调度到哪个 provider（后台接口 `GET /debug/schedule`，含 ordered 列表/sticky 状态）；`model-proxy doctor` 离线诊断 config 的调度设置（每 provider tier/quota/peak + 每路由 dry-run 顺序 + warning，不需 daemon）。

## 添加新 Provider

1. 建 `provider/xxx.go`，实现 Provider 接口（embed `ApiKeyBase`（文件存 API key）+ `baseProbe`（默认探测/过滤行为））。`baseProbe` 默认：探测走 OpenAI `POST /chat/completions`、无专属请求头、候选模型透传。仅当 provider 与此不符时才 override：
   - `ProbeRequest(modelID)` -- 探测请求的 path/body（如 codex 的 `/responses` + Responses API body、aqp 的 `/v1/messages`）
   - `ExtraHeaders(req, path)` -- 每次请求（转发 + 探测）都要的专属头（如 aqp 的 `anthropic-version` + `x-compass-request-id`）
   - `FilterModelIDs(ids)` -- `models refresh` 的静态策略过滤（如 volcengine 剔除 `*-latest`/lite/mini）
2. `init()` 里 `Register("xxx", constructor)`
3. config 加 `provider_id: xxx`
4. plan 类 provider 还应实现 `Quota()`（在 `buildProviders` 里 wire `QuotaFn`），否则会被当作 `unknown`（按 priority 排）；按量计费的设 `billing: pay-as-you-go`

provider 专属的探测/过滤/请求头知识全部收敛在 `provider/xxx.go`，不写进 main 包的 switch/if。不改 proxy/login/logout/usage/models 的代码。

## Demo

```bash
model-proxy serve
python3 examples/demo.py --port 15721 "hello" glm-5.2
python3 examples/demo.py --port 15721 --protocol codex "hello" gpt-5.5
```

## DeepSeek（内置，双协议）

DeepSeek 已内置（`provider_id: deepseek`），一个 API key 同时服务 OpenAI 与 Anthropic 协议。两个 endpoint 用 `openai_base_url`（OpenAI base）和 `anthropic_base_url`（Anthropic base，须含 `/v1`，因代理会剥掉客户端的 `/v1`）分别配置；代理按调用协议转发到对应 endpoint。默认 config 含 provider 定义但**不含 routes**——按需添加：

```yaml
providers:
  deepseek:
    provider_id: deepseek
    openai_base_url: https://api.deepseek.com
    anthropic_base_url: https://api.deepseek.com/anthropic/v1
    usage_url: https://api.deepseek.com/user/balance
    models:                 # 只填模型名；元数据从 models.dev 自动补
      - deepseek-v4-pro
      - deepseek-v4-flash

claude_mapping:
  claude-opus-4-8: deepseek-v4-pro   # DeepSeek 服务端也会自动映射 claude-opus*→v4-pro

routes:                     # 可选：不配 routes 时，models 里且已 login 的模型会自动隐式路由
  deepseek-v4-pro:
    - {provider: deepseek, model: deepseek-v4-pro, priority: 1}
```

```bash
model-proxy login deepseek        # 输入 DeepSeek API key
model-proxy usage deepseek        # 查余额（is_available + 各币种 total/granted/topped-up）
```

## 火山方舟 Volcengine（含 Agent Plan，双协议）

火山方舟（Ark）已内置（`provider_id: volcengine`），一个 API key 同时服务 OpenAI 与 Anthropic 协议；两个 endpoint 用 `openai_base_url` 与 `anthropic_base_url` 分别配置（须含 `/v1`，代理会剥掉客户端的 `/v1`）。**Agent Plan** 套餐用独立的 plan base（`/api/plan/v3`、`/api/plan/compatible/v1`）。

```yaml
providers:
  volcengine:
    provider_id: volcengine
    openai_base_url: https://ark.cn-beijing.volces.com/api/plan/v3
    anthropic_base_url: https://ark.cn-beijing.volces.com/api/plan/compatible/v1
    usage_url: https://ark.cn-beijing.volces.com/api/plan/v3/models
    models:
      - doubao-seed-1-8-251228

routes:
  doubao-seed-1-8-251228:
    - {provider: volcengine, model: doubao-seed-1-8-251228, priority: 1}
```

```bash
model-proxy login volcengine        # 依次输入：Ark API Key（对话）+ AccessKey/SecretKey（GetAFPUsage 用，IAM 密钥）
model-proxy usage volcengine        # Agent Plan 的 5h/每日/周/月 AFP 额度（GetAFPUsage，需 AK/SK）
```

鉴权双写（`Authorization: Bearer` + `x-api-key`）：OpenAI 端点用 Bearer，Anthropic-compatible 端点用 x-api-key，一个 key 两种协议都能用。

> **用量（GetAFPUsage）**：Agent Plan 的 5h/每日/周/月额度在 `GetAFPUsage`——火山引擎**签名 OpenAPI**（`Action=GetAFPUsage&Version=2024-01-01`，HMAC-SHA256/V4，需 **AccessKey/SecretKey**），Ark API Key（Bearer，仅对话）调不了。`login volcengine` 会同时收 Ark API Key + AK/SK；`usage volcengine` 用 V4 签名调 GetAFPUsage 显示各窗口 Quota/Used/Remaining/ResetTime。未配 AK/SK 时退化为列 config 模型。

