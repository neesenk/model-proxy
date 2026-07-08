# AIS Switch → opencode：接入本地代理 / 直连网关 配置指南

本文件记录如何让 opencode 使用 AIS Switch 的 LLM Gateway 模型（glm-5.2 / deepseek-v4-pro / deepseek-v4-flash）。有两种方式：**走本地代理**（`127.0.0.1:15721`）和**直连网关**（绕过代理）。所有内容均已在 2026-06-20 实测验证。

> **命名说明**：model-proxy 内部将该 provider 重命名为 `aqp`（config 的 `provider_id`、凭据文件 `aqp_oauth_auth.json`、字段 `aqp_mint_url`）；上游服务本身仍是 `compass.llm.shopee.io`，故本文件保留 “compass / CQP” 称谓以匹配逆向出的真实后端契约。

---

## 背景：代理做了什么

AIS Switch（Shopee fork of cc-switch，`io.shopee.aisswitch`，v0.1.8）内嵌一个 Rust 反向代理，监听 `127.0.0.1:15721`。对 claude app_type，它透明地做三件事：

1. **改写 baseURL** → 真实上游 `https://compass.llm.shopee.io/compass-api/v1`
2. **注入鉴权** → 用 SSO cookie 运行时换取 CQP API key，替换请求里的占位 token
3. **模型别名映射** → `opus`/`claude-opus-4-*` → `glm-5.2`；`sonnet`/`claude-sonnet-4-6` → `deepseek-v4-pro`；`claude-haiku-4-5` → `deepseek-v4-flash`

**路由按 URL 路径固定**：`/v1/messages` → claude provider；`/v1/chat/completions`、`/v1/responses` → codex provider（需 Codex OAuth）；`/v1beta/*` → gemini。opencode 必须走 **Anthropic 协议**（`/v1/messages`）才能命中 claude 的网关 provider。opencode **不在** `proxy_config` 白名单里，AIS Switch 不接管 opencode 配置——只能手动配。

## 模型映射表

| opencode 别名（须在 anthropic 内置白名单） | 网关真实模型名（`id` 字段 / 直连时发送） |
|---|---|
| `claude-opus-4-7` / `claude-opus-4-8` | `glm-5.2` |
| `claude-sonnet-4-6` | `deepseek-v4-pro` |
| `claude-haiku-4-5` | `deepseek-v4-flash` |

---

## 方式 A：走本地代理（`127.0.0.1:15721`）

依赖 AIS Switch 在运行、且 claude 的 live-takeover 处于激活状态（`proxy_config.enabled=1`，默认开）。代理负责鉴权与模型映射，opencode 侧 apiKey 随便填。

### 配置（项目级 `.opencode.json` 或合并进 `~/.config/opencode/opencode.json` 的 `provider.anthropic` 段）

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "anthropic": {
      "name": "AIS Switch Proxy (Claude→glm-5.2)",
      "options": {
        "baseURL": "http://127.0.0.1:15721/v1",
        "apiKey": "PROXY_MANAGED"
      },
      "models": {
        "claude-opus-4-7":   { "name": "glm-5.2 (opus)",           "limit": { "context": 200000, "output": 32768 } },
        "claude-sonnet-4-6": { "name": "deepseek-v4-pro (sonnet)",  "limit": { "context": 200000, "output": 32768 } },
        "claude-haiku-4-5":  { "name": "deepseek-v4-flash (haiku)", "limit": { "context": 200000, "output": 32768 } }
      }
    }
  }
}
```

### 使用

```bash
opencode run -m anthropic/claude-opus-4-7 "..."
```

或顶层默认模型：`"model": "anthropic/claude-opus-4-7"`。

### 校验

- `curl http://127.0.0.1:15721/health/status` 应有响应；`GET /v1/models` 返回 200。
- `curl -X POST http://127.0.0.1:15721/v1/messages -H "x-api-key: FAKE" -d '{"model":"claude-haiku-4-5",...}'` 应返回 200 + 上游 `model=deepseek/deepseek-v4-flash...`（证明代理替换了鉴权 + 做了映射；incoming key 写什么都行）。
- 代理日志 `~/.ais-switch/logs/ais-switch.log` 出现 `[ProxyRequest] 发送上游请求 ... url=.../compass-api/v1/messages ... (model=glm-5.2)`。

### 注意

- `baseURL` 必须带 `/v1`：opencode 内置 anthropic provider 会拼 `baseURL + /messages`，得到 `http://127.0.0.1:15721/v1/messages` 正好命中代理路由。
- 走 OpenAI 协议（`/v1/chat/completions`）会落到 codex 路由，返回 401 `cc_switch_auth_error`（要 Codex OAuth）——不要用。
- 代理停了 opencode 就连不上；无明文 key 落盘；有用量统计/熔断/failover。

---

## 方式 B：直连网关（绕过代理）

不依赖 AIS Switch 运行，但需手动补齐代理做的三件事：真实 baseURL、CQP key、真实模型名。

### 第 1 步：换 CQP API key

CQP key 不在 DB 里持久化，用 SSO cookie 现换：

```bash
COOKIE=$(python3 -c "import json;print(json.load(open('/Users/zhiyong.liu/.ais-switch/google_oauth_auth.json'))['sso_session_cookie'])")
curl -s -X POST "https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate" \
  -H "content-type: application/json" -H "Cookie: $COOKIE" -d '{}'
# → {"retcode":0,"data":{"api_key":"<64-hex>","quota_type":"CQP","project_id":"ae0ef3655e8c4a889373fef1a36e0b26", ...}}
```

**陷阱**：`sso_session_cookie` 的值已含 `SSO_C=` 前缀，直接作为整个 `Cookie:` 头的值传入，**不要**再拼 `SSO_C=`（否则 HTTP 000 / 认证失败）。

> 这把 key（project `ae0ef3655e...`，"Supply Chain"）才有 glm-5.2 权限。opencode.json 里另一把 `41997e0d...` 是别的 project scope，调 glm-5.2 返回 `403 glm-5.2 is not enabled for this project`——不要混用。

### 第 2 步：raw curl 校验（证明 key + 模型名可用）

```bash
curl -s -X POST "https://compass.llm.shopee.io/compass-api/v1/messages" \
  -H "content-type: application/json" -H "anthropic-version: 2023-06-01" \
  -H "Authorization: Bearer <CQPKEY>" \
  -d '{"model":"glm-5.2","max_tokens":200,"messages":[{"role":"user","content":"reply with exactly: pong-direct"}]}'
# 期望 200，content 含 type:text "pong-direct"
# 注意：glm-5.2 先吐 thinking 块，max_tokens 太小（如 20）会把预算全花在 thinking 上、text 为空，用 >=~100
```

### 第 3 步：opencode 配置（直连）

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "anthropic": {
      "name": "Shopee Compass direct",
      "options": {
        "baseURL": "https://compass.llm.shopee.io/compass-api/v1",
        "apiKey": "<CQPKEY>"
      },
      "models": {
        "claude-opus-4-7":   { "id": "glm-5.2",           "name": "glm-5.2",           "limit": { "context": 200000, "output": 32768 } },
        "claude-sonnet-4-6": { "id": "deepseek-v4-pro",   "name": "deepseek-v4-pro",   "limit": { "context": 200000, "output": 32768 } },
        "claude-haiku-4-5":  { "id": "deepseek-v4-flash", "name": "deepseek-v4-flash", "limit": { "context": 200000, "output": 32768 } }
      }
    }
  }
}
```

### 使用

```bash
opencode run -m anthropic/claude-opus-4-7 "..."
```

opencode 发出的请求 `model` 字段为 `glm-5.2`（因 `id` 覆盖），直连网关 200。

### opencode 配置坑（均已踩过并规避）

1. **必须用内置 `anthropic` provider id**（覆盖其 `options`）。自定义 provider id 需 `npm: "@ai-sdk/anthropic"`，而该包未安装（`~/.config/opencode/node_modules` 无 `@ai-sdk/*`）→ opencode 静默卡死。不支持无 npm 的自定义 provider。
2. **model 的 key 必须在 opencode 的 anthropic 内置白名单里**（如 `claude-opus-4-7`、`claude-sonnet-4-6`、`claude-haiku-4-5`）。任意 key（如直接用 `glm-5.2`）→ `Model not found`。
3. **`id` 字段是发给 API 的真实模型名**，map 的 key 只是显示别名。所以 `claude-opus-4-7: {id:"glm-5.2"}` 会向网关发 `model:"glm-5.2"`——这就是直连时替代代理「模型别名映射」的手段。
4. **title 生成**用 opencode 内置默认 haiku（`claude-haiku-4-5-20251001`，无 `id` 覆盖）→ 以原名打网关 → 日志报 404 stream error。不影响主请求；要消除，给 haiku 别名也加 `id` 映射。
5. opencode `--format json` 的 run 若在 `run_in_background` 命令里带尾部 `&`，会丢失重定向（输出文件空）。别加 `&`，交给 Bash tool 后台化。

---

## 两种方式对比

| | 走代理 (15721) | 直连网关 |
|---|---|---|
| 依赖 AIS Switch 运行 | 是 | 否 |
| apiKey 落盘 | 否（`PROXY_MANAGED`） | 是（明文 CQP key） |
| 用量统计 / 熔断 / failover | 有 | 无 |
| key 自动刷新 | 代理管 | SSO cookie 过期需重换 key |
| 配置复杂度 | 低 | 中（换 key + `id` 映射） |

CQP key 在 SSO cookie 失效后过期，重跑 `get_or_generate` 即可重换（AIS Switch 登录时刷新该 cookie 文件）。

---

## 逆向锚点（便于后续深挖）

- **DB** `~/.ais-switch/cc-switch.db`：`providers`（PK `id,app_type`；`settings_config` JSON）、`proxy_config`（CHECK `app_type IN ('claude','codex','gemini')`，opencode 不在内）、`proxy_live_backup`、`proxy_request_logs`（有 `request_model` vs 真实 `model`，证明别名映射）、`provider_health`。
- **二进制** `/Applications/AIS Switch.app/Contents/MacOS/ais-switch`：`strings` 可挖出路由前缀、CQP 端点、`cc_switch_lib::proxy::*` 模块名。
- **SSO cookie** `~/.ais-switch/google_oauth_auth.json` 的 `sso_session_cookie` 字段——换 CQP key 的唯一依赖。

---

## 版本兼容（0.1.8 → 0.1.12）

本文件所有契约最初逆向自 **v0.1.8**，AIS Switch 于 2026-06-25 更新至 **v0.1.12**（CQP 检查 `latest_version=0.1.12 force_update=false`，已是最新）。逐项复核，**四项契约均未变，两种接入方式与 `model-proxy` 均兼容，无需改动**：

| 契约 | 0.1.12 现状 |
|---|---|
| CQP mint 端点 `…/api/v1/cqp/ccswitch/api_key/get_or_generate` | 一致 |
| `sso_session_cookie` 字段（`google_oauth_auth.json`） | 一致 |
| 路由前缀 `/v1/messages`、`/v1/chat/completions`、`/v1/responses`、`/v1beta/*` | 一致 |
| 模型映射 opus→glm-5.2、sonnet→deepseek-v4-pro、haiku→deepseek-v4-flash | 一致（请求日志实证 `claude-opus-4-8`→`z-ai/glm-5.2-20260616` 等未变） |
| `proxy_config` CHECK 仍 `('claude','codex','gemini')`，不含 opencode | 一致 |

0.1.12 的新增均为附加功能，不破坏既有契约：codex_desktop / codex-vscode 安装路径探测、Copilot `copilot_model_map`、CQP `force_below_version`/`team_id` 字段、oh-my-openagent 团队配置同步。

> 复核方法：`strings -a /Applications/AIS\ Switch.app/Contents/MacOS/ais-switch | grep <端点/字段>`，配合 `sqlite3 ~/.ais-switch/cc-switch.db` 查 `providers.settings_config` 与 `proxy_request_logs`。若 AIS Switch 跨大版本升级，按上表重新核对四项契约。

---

## model-proxy 实现经验

以下经验来自 `model-proxy/` 的完整开发过程（2026-06-29 ~ 07-03），记录关键契约、踩过的坑和架构决策。

### 架构：Provider 抽象 + 协议路由

```
provider/                    # Provider 实现（每个上游一个文件）
  provider.go                # 接口 + 注册 + New()
  apikey.go                  # ApiKeyBase（共享 auth file + Bearer 注入）
  compass.go                 # compass: SSO + CQP + monthly_usage + ?beta + headers
  codex.go                   # codex: OAuth device flow + wham/usage + store:false
  zhipu.go                   # zhipu: API key prompt + /models 校验 + 模型列表

config.yaml:
  providers:                 # provider 定义（openai_base_url + provider_id + models）
    compass:
      provider_id: compass   # 路由到 provider/compass.go
      openai_base_url: ...
      cqp_mint_url: ...      # compass 专属
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
      - {provider: compass, model: glm-5.2, priority: 1}
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

### 配额感知调度（quota-aware scheduling）

`Provider.Quota()`（接口方法，镜像 `UsageFn` 由 `QuotaFn` 回调注入）把每个上游的用量端点解析成归一化的 `provider.QuotaSnapshot{Billing, RemainingPct, Windows, ...}`。各 provider 来源：

| provider_id | 来源端点 | Billing |
|---|---|---|
| zhipu | `quota/limit`（5h/周 token + 月度时间） | plan |
| codex | `wham/usage`（primary/weekly 窗口 + spend） | plan |
| volcengine | `GetAFPUsage`（V4 签名，需 AK/SK） | plan |
| compass | `monthly_usage`（月度 ratio/balance） | plan |
| deepseek | `/user/balance`（按量余额） | pay-as-you-go |

`RemainingPct` = 该 provider **最终窗口**（总预算）的 remaining%：zhipu=周、volcengine=月、codex=月度 spend、compass=月、deepseek=payg（无窗口）。`QuotaWindow` 带 `Ultimate`/`Short` 标记 + `Duration`（名义周期），由各 parser 设置。短窗口（5h 等）是 rate-cap，**不参与 min**（用满即 429，反应式跳过）；zhipu 的 `TIME_LIMIT`（MCP 工具配额）不参与（只展示）。

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

**踩过的坑**：① forward 鉴权读内嵌 `ApiKeyBase`（文件名 = providerName），虚拟必须经 `BoundAPIKey` 绑内存 key，否则会去读不存在的 `<name#id>_apikey.json` → 502；② `Refresh()` 在 bound 实例上是 no-op（bound key 不可变，清缓存会注入空 Bearer）；③ `login` 写的是复数池，故 **1 条池（单账号）也必须 bind**（不能走 file-backed 读单数文件，否则 `login` 后 502 —— 这是 buildProviders 用 `os.Stat(poolPath)` 区分「复数池存在」与「单数 fallback」的原因）；④ volcengine 每账号要完整 `{api_key, access_key, secret_key}`（`GetAFPUsage` 签名用 AK/SK），`resolveVolcengineAKSK` 对非 nil cred **排他**（不回落文件，防泄漏兄弟账号 AK/SK）；⑤ volcengine 的 `FetchModels`（`ListArkAgentPlanModel`）暂未按账号绑 —— 池化时 `models refresh volcengine` 优雅退回 config 模型。

### Token 文件命名

| provider_id | suffix | 文件名 |
|---|---|---|
| compass | oauth_auth | `~/.model-proxy/<name>_oauth_auth.json` |
| codex | oauth_auth | `~/.model-proxy/<name>_oauth_auth.json` |
| zhipu / deepseek | apikey | `~/.model-proxy/<name>_apikey.json`（单数，只读 fallback）|
| volcengine | apikey | `~/.model-proxy/<name>_apikey.json` —— `{api_key, access_key, secret_key}`（API Key 聊天 + AK/SK 签 `GetAFPUsage`）|

路径从 provider name（config 一级 key）派生，支持多实例（如 `zhipu-personal` / `zhipu-work`）。**多账号**：zhipu/deepseek/volcengine 重复 `login` 写**复数池** `~/.model-proxy/<name>_apikeys.json`（见上「多账号凭据池 + session-sticky 路由」），单数文件是其 fallback；aqp/codex 不池化。

### CLI 命令

```
login <provider> [--label NAME] [--replace]   # compass: SSO / codex: device flow / apikey 类: 输入 key（可重复录入 → 多账号池，按 id 去重；写完热重载）
logout <provider> [--label NAME | --all]       # 删一个账号（默认交互式选择）/ 清空池
usage <provider>          # 池化时默认逐账号展示全部账号用量（--label 看某一个）
models [provider]         # 从 config 列模型
models refresh <provider> # 从服务端刷新
serve [daemon|stop|reload]
takeover/restore <client>
config init|print|check
schedule                   # 查询运行中的 daemon：每个 model 当前调度到哪个 provider（GET /debug/schedule）
doctor                     # 离线 config 调度诊断（每 provider tier/quota/peak + 每路由 dry-run 顺序 + warning）
```

### Web UI + `/api/*` 接口契约

`web.enabled`（默认 true）时，daemon 在同一个 mux 上挂 `/ui/`（嵌入式静态资源，`web_assets/` 经 `go:embed`）和 `/api/`（JSON）。**仅 loopback、无鉴权**（信任来自「本地」）。`/api/*` 路由表：

| 方法 | 路径 | 请求 body | 响应 shape | 备注 |
|---|---|---|---|---|
| GET | `/api/status` | — | `{uptime, version, listen, health{<prov>:{circuit_state, available, [circuit_until], [rate_limited_until]}}, quota{<prov>: <QuotaSnapshot 原样, PascalCase 键>}, schedule: {models:[…]}(来自 /debug/schedule), counters{<prov>:{requests, failovers, rate_limited_429, failures, last_request_at}}}` | 锁：`p.mu`(RLock) → `healthMu` → `quotaMu`(经 allSnapshots) **顺序获取不嵌套**。`quota` 字段是 provider 包的 `QuotaSnapshot` 原样序列化（无 json tag → **PascalCase**：`RemainingPct`/`Windows`/`Billing`…） |
| GET | `/api/logs?tail=N` | — | `{lines:[…]}` | 读 log 文件末尾 N 行（默认 200，上限 1000）。路径取 `web.logFile`（runProxy 时解析）回落 `cfg.LogFile`，都没配 → 404 |
| GET | `/api/config` | — | `{yaml: "<原文件 verbatim>", summary:{listen, provider_count, route_count}}` | 原文件 round-trip（saveAndReload 保证 byte-identical 落盘） |
| POST | `/api/config` | `{yaml:"…"}` | `{status:"reloaded"}` 或 400 `{error}` | 走 `saveAndReload`：validate(`LoadConfigFromBytes`, 不动盘) → backup `<file>.bak` → `atomicWrite`(tmp+rename) → `proxy.reload`。校验失败不落盘、不建 .bak；reload 失败（防御性，post-validate 不应发生）从 .bak 回滚 |
| POST | `/api/config/edit` | `{kind, name, data}` | `{status:"reloaded"}` 或 400 | 结构化编辑（`kind ∈ {general, scheduling, provider, route, claude_mapping}`），改 `yaml.Node` 树（**保留注释与键序**）→ 重编码 SetIndent(2) → 经 `saveAndReload`。`data.delete:true` 删除具名实体（claude_mapping 用 `data.alias` 定位）。route targets 经 `mustEncode`（JSON 值 → yaml.Node 内层）。**坑**：新 int 键默认 `!!str`（yaml.v3 unmarshal 时自动识别既有 int 字段；目前无新建 int 键的路径） |
| GET | `/api/accounts` | — | `{providers:[{name, provider_id, billing, accounts:[{id, label, added_at, [email}]}]}` | **响应结构里根本没有 key 字段** —— 即使 builder 出 bug 也无法泄漏 api_key/SSO cookie/access_key/secret_key。id **不掩码**（UI 要用它来删除） |
| POST | `/api/accounts/<provider>` | `{api_key, access_key?, secret_key?, label?, replace?}` | `{id, status:"added"}` | 仅 apikey 类（zhipu/deepseek/volcengine）。aqp/codex 返 400 指向 async login。volcengine 走 `addVolcengineAccount`（AK/SK 三元组，不探 usage_url）；其余走 `addApikeyAccount`（探 usage_url 校验）。落盘后 best-effort reload（账号已存盘，reload 失败也返回成功） |
| DELETE | `/api/accounts/<provider>/<id>` | — | `{status:"removed"}` | apikey 类走 `removeApikeyAccount`；aqp = `clearAccount`（oauth_auth，logout 语义）；codex = `os.Remove`（单凭据）。best-effort reload |
| GET | `/api/tokens` | — | `{usage:[{provider, model, input, output, cache_creation, cache_read, requests}]}` | 由 SSE 扫描器累计的观测用量（见下）。flat 数组（map[tokenKey]tokenUsage 摊平，JSON 对象 key 必须是 string） |
| POST | `/api/tokens/reset` | — | `{status:"reset"}` | 清空内存计数；不动盘（下一次 persist tick 用空快照覆盖 `~/.model-proxy/token_usage.json`） |
| POST | `/api/login/<provider>/start` | — | `{session_id, login_url}`(aqp) 或 `{session_id, verify_url, user_code}`(codex) | 异步登录启动。aqp：`BootstrapLoginURL` + 建 cookie-jar `AqpClient` + 起 poll goroutine；codex：`requestUserCode`（device flow）+ 起 poll goroutine。unknown provider → 404；非 aqp/codex → 400 |
| GET | `/api/login/<session>/poll` | — | `{state:"pending"|"done"|"error", detail, result}` | poll 异步登录。`detail` = login_url/verify_url+user_code（pending，UI 可恢复）/ error msg（error）；`result` = email/account_id（done）。unknown/expired session → 404 |

#### SSE token 扫描器契约（`tokens.go`，`forward` 2xx 提交处接入）

- **Pass-through only**：`usageScanner` 是个 `io.ReadCloser`，包在 `resp.Body` 外**仅当 `isSSE(resp.Header)`**。读到的字节原样返回给客户端 —— **不修改、不缓冲流、不阻塞客户端**。失败静默（不记 usage）。
- **bounded 64KB 行缓冲**（`scanLineCap = 64 * 1024`）：`observe` 按 `\n` 切完整行喂给 `parseLine`；不完整行的字节存进 `s.line`（cap 到 64KB，超出则丢弃该字节 —— 字节仍会 pass through，只是超长行不解析）。防 OOM。
- **commit-on-EOF/close（含客户端断开）**：`Read` 见到 err 且 `!done` → `tc.commit(key, acc)`；`Close` 若 `!done` 也 commit。`forward` 在 `flushCopy` 后显式 `body.Close()` —— 客户端中途断开（`flushCopy` 写错即 break，未达 EOF）时也能记下已观测的 input tokens（`message_start` 在流首、cancel 前到达）。正常 EOF 时 `Read` 已置 `done=true` 并 commit，`Close` 是 no-op（**不重复计数**）。
- **解析**：仅看 `data:` 前缀 + 首字符 `{` 的行。先试 anthropic shape（`message_start` → input/cache_creation/cache_read；`message_delta` → output），再试 openai shape（`usage.prompt_tokens`/`completion_tokens`）。OpenAI token 计数是 **best-effort**：`usage` 仅当客户端发 `stream_options.include_usage`（且上游愿给）时才有 —— 扫描器宁可啥也不记也不 zero-fill。
- **持久化**：`persistTokensLoop`（daemon.go）周期 `tc.save()` 到 `~/.model-proxy/token_usage.json`（原子 tmp+rename，0600）。key 摊平用 `provider\x00model`（NUL 分隔，避免 `model` 里含 `/`）。启动时 `tc.load()` 作为基线（文件不存在视为空，不报错）。
- **锁**：`tokenMu` 是**独立叶子锁**，与 `p.mu`/`healthMu`/`quotaMu` 互不嵌套。`commit`/`snapshot`/`save` 的 `*tokenUsage` 字段读写都在 `tc.mu` 下。

### compass 网关契约（实测）

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
| responses | POST | codex OAuth Bearer | `/backend-api/codex/responses`，需 `store:false` + `stream:true`，不接受 `max_tokens` |
| usage | GET | codex OAuth Bearer | `/backend-api/wham/usage`（注意：不在 `/codex/` 子路径下） |
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
- Agent Plan 的 5h/每日/周/月额度在 **GetAFPUsage**（火山引擎签名 OpenAPI：`Action=GetAFPUsage&Version=2024-01-01&serviceCode=ark`，管控面、HMAC-SHA256/V4 签名，需 AccessKey/SecretKey）—— Ark API Key（Bearer，仅对话）调不了（实测 `/api/v3/models`→401、`/api/plan/v3/models`→404）。**已实现**：`volcengine_sign.go` 做 V4 签名（CredentialScope `{date}/cn-beijing/ark/request`，signing key 链 SK→kDate→kRegion→kService→kSigning，**末项 `"request"` 非 `"volcengine_request"`**；签名头仅 `host;x-date`，**不含 `x-content-sha256`**）。`login volcengine` 同时收 Ark API Key + AK/SK。`usage volcengine` 调 GetAFPUsage 解析 `Result.{AFPFiveHour,AFPDaily,AFPWeekly,AFPMonthly}`（各含 `Quota/Used/ResetTime`，Remaining=Quota−Used）。`models refresh volcengine` 调 **ListArkAgentPlanModel**（同理 V4 签名）解析 `Result.Datas[].ModelID`（当前 17 个模型，含 doubao/glm-5.2/kimi/minimax/deepseek）。未配 AK/SK 时退化为列 config 模型
- 模型 ID 是模型名（如 `doubao-seed-1-8-251228`、`doubao-seed-2-0-code`），非推理接入点 endpoint id（标准 Ark 按量计费才用 endpoint id）
- 凭据存 `~/.model-proxy/<name>_apikey.json`

### 踩过的坑

1. **路径双 `/v1`**：provider openai_base_url 已含 `/compass-api/v1`，client path `/v1/messages` 拼接后变双 `/v1`。需剥 client 的 `/v1` 前缀。
2. **codex 后端请求体**：`store:false`（否则 400）+ `stream:true`（否则 400）+ 无 `max_tokens`（否则 400）。代理在 RewriteRequest 自动注入 `store:false`。
3. **monthly_usage**：POST 非 GET，需 `project_id` 入参。字段名 `total_amount`/`usage`/`balance`/`plan`（非 `totalAmount`）。
4. **wham/usage 路径**：`/backend-api/wham/usage`，不是 `/backend-api/codex/wham/usage`（后者 403）。
5. **日志掩码**：SSO cookie 必须用 `mask()`（首2…尾2），auth/info 响应体只记长度。
6. **文件日志无色**：`--log-file` 时 `logColorEnabled` 置 `false`，否则 ANSI 污染日志文件。
7. **flushCopy 写错误**：客户端断开后 `w.Write` 返回错误须立即 break，否则代理继续拉上游流浪费 compute。
8. **context 传播**：用 `http.NewRequestWithContext(r.Context(), ...)` 让客户端取消传播到上游。
9. **supervisor nil panic**：`spawnWorker` 失败时返回 nil，`runSupervisor` 需检查再处理。
10. **配额陈旧保护**：snapshot 老于 `3×quota_poll_interval` 一律视为 `BillingUnknown`（不再相信缓存值）。`Quota()` 失败的 provider 也是 `BillingUnknown`（按 priority 排，**绝不**当 payg）。
11. **volcengine 配额需 AK/SK**：`GetAFPUsage` 是火山引擎签名 OpenAPI（管控面），Ark API Key（Bearer，仅对话）调不了；`login volcengine` 必须同时收 AK/SK 才能产 quota。
12. **锁顺序 `healthMu` → `quotaMu`**：`schedule` 先 `allSnapshots()`（quotaMu RLock）拿到快照，再 `healthMu.Lock()`；绝不在持 `healthMu` 时回调 quota 接口，否则与 `refreshOne`/`pollAll` 反向嵌套死锁。
13. **`BillingClass` iota ≠ 调度序**：`Unknown=0, Plan=1, PayG=2`，但调度序是 `Plan < Unknown < PayG`，故 `tierRank` 单独映射（不要直接比较 `BillingClass` 常量）。

### opencode/pi takeover 配置

| 客户端 | baseURL 格式 | 关键差异 |
|---|---|---|
| opencode | `http://<proxy>/v1` | `@ai-sdk/anthropic` 拼 `baseURL+/messages`，baseURL 要带 `/v1` |
| pi | `http://<proxy>` | pi 的 `anthropic-messages` 自己拼 `/v1/messages`，baseURL 不带 `/v1`（否则 `/v1/v1/messages` → 502） |

provider_id 统一为一个配置项（默认 `model-proxy`），opencode/pi/codex 共用。备份文件存 `<configDir>/.model-proxy/<client>.bak`。

