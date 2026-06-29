# AIS Switch → opencode：接入本地代理 / 直连网关 配置指南

本文件记录如何让 opencode 使用 AIS Switch 的 LLM Gateway 模型（glm-5.2 / deepseek-v4-pro / deepseek-v4-flash）。有两种方式：**走本地代理**（`127.0.0.1:15721`）和**直连网关**（绕过代理）。所有内容均已在 2026-06-20 实测验证。

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

本文件所有契约最初逆向自 **v0.1.8**，AIS Switch 于 2026-06-25 更新至 **v0.1.12**（CQP 检查 `latest_version=0.1.12 force_update=false`，已是最新）。逐项复核，**四项契约均未变，两种接入方式与 `ais-switch-proxy` 均兼容，无需改动**：

| 契约 | 0.1.12 现状 |
|---|---|
| CQP mint 端点 `…/api/v1/cqp/ccswitch/api_key/get_or_generate` | 一致 |
| `sso_session_cookie` 字段（`google_oauth_auth.json`） | 一致 |
| 路由前缀 `/v1/messages`、`/v1/chat/completions`、`/v1/responses`、`/v1beta/*` | 一致 |
| 模型映射 opus→glm-5.2、sonnet→deepseek-v4-pro、haiku→deepseek-v4-flash | 一致（请求日志实证 `claude-opus-4-8`→`z-ai/glm-5.2-20260616` 等未变） |
| `proxy_config` CHECK 仍 `('claude','codex','gemini')`，不含 opencode | 一致 |

0.1.12 的新增均为附加功能，不破坏既有契约：codex_desktop / codex-vscode 安装路径探测、Copilot `copilot_model_map`、CQP `force_below_version`/`team_id` 字段、oh-my-openagent 团队配置同步。

> 复核方法：`strings -a /Applications/AIS\ Switch.app/Contents/MacOS/ais-switch | grep <端点/字段>`，配合 `sqlite3 ~/.ais-switch/cc-switch.db` 查 `providers.settings_config` 与 `proxy_request_logs`。若 AIS Switch 跨大版本升级，按上表重新核对四项契约。

