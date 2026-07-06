# model-proxy

多 Provider LLM 代理 — 统一管理 Compass/codex/Zhipu 等上游后端，按协议（Anthropic/OpenAI）对外暴露，自动处理鉴权、模型映射、流式转发。

## 架构

```
┌─────────────┐     ┌───────────────────────────────────┐     ┌──────────────┐
│  客户端      │────▶│  model-proxy                      │────▶│  Provider    │
│  claude/     │     │  ┌─────────┐  ┌────────────────┐ │     │  compass     │
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
go build -o model-proxy .

# 交叉编译 Linux
GOOS=linux GOARCH=amd64 go build -o model-proxy-linux .
```

## 配置

`config.yaml`（`model-proxy config init` 生成模板）。查找顺序：`--config PATH` > `~/.model-proxy/config.yaml` > `./config.yaml`。

```yaml
listen: 127.0.0.1:15721
log_level: info

providers:
  compass:
    provider_id: compass
    openai_base_url: https://compass.llm.shopee.io/compass-api/v1
    cqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
    models:
      glm-5.2: {context: 1024000, output: 4096, modalities: {input: [text], output: [text]}}
  codex:
    provider_id: codex
    openai_base_url: https://chatgpt.com/backend-api/codex
    models:
      gpt-5.5: {context: 200000, output: 32768, modalities: {input: [text, image], output: [text]}}
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    usage_url: https://open.bigmodel.cn/api/paas/v4/models
    models:
      glm-5.2: {context: 128000, output: 4096, modalities: {input: [text], output: [text]}}

claude_mapping:
  claude-opus-4-7: glm-5.2          # anthropic-only: claude 别名 → 对外模型名
  claude-sonnet-4-6: deepseek-v4-pro

routes:
  glm-5.2:
    - {provider: compass, model: glm-5.2, priority: 1}
    - {provider: zhipu,   model: glm-5.2, priority: 2}   # failover 备选
  gpt-5.5:
    - {provider: codex, model: gpt-5.5, priority: 1}

takeover:
  claude: ~/.claude/settings.json
  opencode: ~/.config/opencode/opencode.json
  codex: ~/.codex/config.toml
  pi: ~/.pi/agent/models.json
  provider_id: model-proxy
```

## 用法

```bash
# 登录（凭据存储在 ~/.model-proxy/<name>_<suffix>.json）
model-proxy login compass          # Compass SSO 浏览器登录
model-proxy login codex            # codex OAuth device flow
model-proxy login zhipu            # 输入 Zhipu API key
model-proxy login deepseek         # 输入 DeepSeek API key
model-proxy login volcengine       # 输入火山方舟（Agent Plan）API key

# 启动代理
model-proxy serve                  # 前台
model-proxy serve daemon           # 后台（自动重启）
model-proxy serve stop             # 停止 daemon
model-proxy serve reload           # 热加载配置（SIGHUP）

# 查看用量
model-proxy usage compass          # 月度用量/余额
model-proxy usage codex            # credits/spend/rate limits
model-proxy usage zhipu            # 5h/周/月配额 + token 消耗
model-proxy usage deepseek         # 账户余额（is_available + 各币种）
model-proxy usage volcengine       # 模型列表（Agent Plan 无简单余额 API）

# 登出
model-proxy logout compass         # 清除凭据文件

# 模型列表
model-proxy models                 # 所有 provider 的模型（从 config）
model-proxy models compass         # 单个 provider
model-proxy models refresh zhipu   # 从服务端刷新

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
```

## Token 文件

凭据由 `login` 管理，按 provider name 派生路径，不落 config：

| Provider | Token 文件 | 内容 |
|---|---|---|
| compass | `~/.model-proxy/compass_oauth_auth.json` | SSO cookie + account data |
| codex | `~/.model-proxy/codex_oauth_auth.json` | OAuth access/refresh/id token |
| zhipu | `~/.model-proxy/zhipu_apikey.json` | API key |
| deepseek | `~/.model-proxy/deepseek_apikey.json` | API key |
| volcengine | `~/.model-proxy/volcengine_apikey.json` | API key（火山方舟） |

多实例支持：同一 `provider_id` 可有多个不同 name（如 `zhipu-personal` / `zhipu-work`），各自独立凭据文件。

## 协议

代理按 URL 路径前缀路由，对外协议 = 转发协议（不做转换）：

| 协议 | 端点 | 转发到 |
|---|---|---|
| Anthropic | `POST /v1/messages` | provider 的 `/messages` |
| OpenAI | `POST /v1/responses`, `/v1/chat/completions` | provider 的同路径 |
| 模型列表 | `GET /v1/models` | 合并所有 routes 的模型 |

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

- **调度分 = surplus**（provider 接口方法 `Provider.Surplus`）：`surplus = (最终窗口.remaining − 短窗口.remaining × (短窗口配额/最终窗口配额) × (peakMult−1)) − 时间剩余比例`。surplus>0 = 落后节奏（不用就浪费 → 优先用）；<0 = 超前（会提前耗尽 → 回避）。最终窗口（总预算）：zhipu=周、volcengine/codex/compass=月、deepseek=按量（无窗口）。
- **三层 tier**：`plan`（按 surplus 排）< `unknown`（按 priority 排）< `pay-as-you-go`（严格兜底，仅当所有 plan provider 都不可用）。设 `billing: pay-as-you-go` 即兜底（如 DeepSeek）。排序 `(tier, priority asc, surplus desc)` —— **priority 压过 surplus，surplus 只在同 priority 间决定**。
- **peak 只烧短窗口**：`peak_hours` 的 multiplier 只折算 provider 的短 rate-cap 窗口（5h 等）；没有短窗口的 provider（codex/compass、未轮询的）高峰不打折。`peak_hours` 支持多段、每段独立 multiplier；multiplier=1 关闭。
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
      - {window: "09:00-12:00", multiplier: 2.0}
      - {window: "14:00-18:00", multiplier: 1.5}
    # ...

scheduling:
  quota_poll_interval: 5m
  quota_switch_margin: 15
```

`Quota()` 来源：zhipu/codex/volcengine/compass → plan tier；deepseek → pay-as-you-go。volcengine 的 `GetAFPUsage` 需 AccessKey/SecretKey（Ark API Key 调不了），未配时该 provider 退化为 `unknown`。

**查看调度**：`model-proxy schedule` 查询运行中的 daemon，显示每个 model 当前调度到哪个 provider（后台接口 `GET /debug/schedule`，含 ordered 列表/sticky 状态）；`model-proxy doctor` 离线诊断 config 的调度设置（每 provider tier/quota/peak + 每路由 dry-run 顺序 + warning，不需 daemon）。

## 添加新 Provider

1. 建 `provider/xxx.go`，实现 Provider 接口（或 embed `ApiKeyBase`）
2. `init()` 里 `Register("xxx", constructor)`
3. config 加 `provider_id: xxx`
4. plan 类 provider 还应实现 `Quota()`（在 `buildProviders` 里 wire `QuotaFn`），否则会被当作 `unknown`（按 priority 排）；按量计费的设 `billing: pay-as-you-go`

不改 proxy/login/logout/usage 的代码。

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
    models:
      deepseek-v4-pro:   {context: 1000000, output: 65536, modalities: {input: [text], output: [text]}}
      deepseek-v4-flash: {context: 1000000, output: 65536, modalities: {input: [text], output: [text]}}

claude_mapping:
  claude-opus-4-8: deepseek-v4-pro   # DeepSeek 服务端也会自动映射 claude-opus*→v4-pro

routes:
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
      doubao-seed-1-8-251228: {context: 256000, output: 32768, modalities: {input: [text], output: [text]}}

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

