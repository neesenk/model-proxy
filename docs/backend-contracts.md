# 后端契约参考（逆向实测）

> 从 AGENTS.md 拆出。**改对应 provider 前必读**；新增 provider 契约追加于此。文件命名/凭据规则也在此。

## Token 文件命名

| provider_id | suffix | 文件名 |
|---|---|---|
| aqp / codex | oauth_auth | `~/.model-proxy/<name>_oauth_auth.json` |
| zhipu / deepseek / kimi-code | apikey | `~/.model-proxy/<name>_apikey.json`（单一 `{api_key}`）|
| volcengine | apikey | `~/.model-proxy/<name>_apikey.json` — `{api_key, access_key, secret_key}` |

路径从 provider name（config 一级 key）派生，支持多实例（如 `zhipu-personal` / `codex-work`）。多账号见 AGENTS.md「凭据池」。所有路径（CLI `login`、`buildOne`、web 异步登录、`logout`）一律用 config name，**包括 aqp/codex**（`runLogin`/`cmdCodexLogin` 接收 `provName` → `authFilePath(provName, "oauth_auth")`）；曾有的「CLI login 硬编码 provider_id → 非同名实例读写错位」bug 已修，`TestAqpCodexLogin_UsesConfigNameForAuthFile` 守护。

## compass 网关契约（实测）

> model-proxy 内部称 `aqp`（config `provider_id`、`aqp_oauth_auth.json`、`aqp_mint_url`）；上游是 `compass.llm.shopee.io`，故保留 compass/CQP 称谓。

| 端点 | 方法 | 鉴权 | 路径 |
|---|---|---|---|
| CQP key 签发 | POST | SSO cookie | `/api/v1/cqp/ccswitch/api_key/get_or_generate` body `{}` |
| 模型列表 | GET | CQP Bearer | `/compass-api/v1/models` |
| 消息（anthropic） | POST | CQP Bearer | `/compass-api/v1/messages` |
| 响应（openai） | POST | CQP Bearer | `/compass-api/v1/responses` |
| 月度用量 | POST | SSO cookie | `/api/v1/cqp/ccswitch/monthly_usage` body `{"project_id":"..."}` |

CQP key 长效，缓存 50min。SSO cookie 值已含 `SSO_C=` 前缀，直接作 Cookie 头值。`/v1/messages` 走 cqp 时需 `?beta=true`、`anthropic-version: 2023-06-01`、`x-compass-request-id`(UUID)。转发头用白名单（不透传客户端 Cookie/Authorization）。

## codex 后端契约（实测）

| 端点 | 方法 | 鉴权 | 备注 |
|---|---|---|---|
| responses | POST | OAuth Bearer | `/backend-api/codex/responses`，需 `store:false` + `stream:true`，不接受 `max_tokens`；body 用 `input`（**必须是 list**，字符串→`Input must be a list`；不能用 `messages`→`Unsupported parameter: messages`） |
| usage | GET | OAuth Bearer | `/backend-api/wham/usage`（**不在 `/codex/` 子路径下**） |
| models | GET | OAuth Bearer | `/backend-api/codex/models?client_version=<ver>`，`{"models":[{slug,visibility,...}]}`，仅取 `visibility=="list"`；`client_version` 决定可见模型 |
| headers | — | — | `originator: codex_cli_rs` 必须（否则 403）；`ChatGPT-Account-Id` 从 id_token JWT 解析 |

OAuth device flow（从 codex-rs 源码确认）：issuer `https://auth.openai.com`，client_id `app_EMoamEEZ73f0CkXaXp7hrann`。`POST /api/accounts/deviceauth/usercode` → `{device_auth_id, user_code, interval}` → 用户访问 `https://auth.openai.com/codex/device` → 轮询 `POST /api/accounts/deviceauth/token`（错误码 `deviceauth_authorization_pending`/`deviceauth_slow_down`）→ `{authorization_code,...}` → `POST /oauth/token` grant_type=authorization_code → tokens。刷新：`POST /oauth/token` grant_type=refresh_token。**代理用独立 OAuth**（不读 codex CLI `~/.codex/auth.json`），避免 refresh_token 轮换竞争。

## Zhipu BigModel 契约

- OpenAI base `https://open.bigmodel.cn/api/paas/v4`（`/chat/completions`、`/models`）；Anthropic base `https://open.bigmodel.cn/api/anthropic/v1`（`/v1/messages`，Bearer）。代理按协议转发。
- 鉴权 `Authorization: Bearer <api_key>`（OpenAI 与 Anthropic 端点都用 Bearer，不像 DeepSeek 需 x-api-key）。
- `/models` 只列 8 个文本对话模型；多模态（glm-4v-plus/cogview-4-plus）需手动加 config。
- 配额 `GET .../api/monitor/usage/quota/limit`（Bearer）→ `{success, data:{limits:[{type,unit,number,percentage,nextResetTime,usage,currentValue,remaining,usageDetails}], level}}`；`type`=TOKENS_LIMIT|TIME_LIMIT，`unit` 3=5h/6=weekly/5=monthly。**`currentValue`=已用、`remaining`=剩余、`usage`=总额**（勿把 `usage` 当已用）。`/users/balance`、`/users/usage` 均 404。
- `usageDetails`：`TIME_LIMIT`（月度）按 **MCP 工具**拆（search-prime/web-reader/zread，工具调用消耗非模型 token），`TOKENS_LIMIT` 按**模型**拆。

## DeepSeek 契约（双协议，一个 key）

- OpenAI base `https://api.deepseek.com`（`/chat/completions`、`/responses`、`/models`、`/user/balance`，Bearer）；Anthropic base `https://api.deepseek.com/anthropic/v1`（`/v1/messages`，`x-api-key`，`anthropic-version`/`anthropic-beta` 被忽略）。
- Anthropic SDK 打 `/anthropic/v1/messages`（base + `/v1/messages`）。代理剥客户端 `/v1`，故 `anthropic_base_url` 须自带 `/v1`。`RewriteRequest` no-op，URL 选择在 `proxy.forward` 按 protocol 完成。
- 鉴权双写：每请求同时设 `Authorization: Bearer` + `x-api-key`，一个 config 服务两协议。
- 服务端模型自动映射（Anthropic）：`claude-opus*`→`deepseek-v4-pro`；`claude-sonnet*`/`claude-haiku*`→`deepseek-v4-flash`。
- `/user/balance` → `{is_available, balance_infos:[{currency, total_balance, granted_balance, topped_up_balance}]}`（注意 `balance_infos` 非 `wallets`）。`/models` → OpenAI 风格；当前 `deepseek-v4-pro`/`deepseek-v4-flash`，旧名 `deepseek-chat`/`reasoner` 2026-07-24 弃用。

## Volcengine Ark 契约（双协议，含 Agent Plan，一个 key）

- OpenAI base `https://ark.cn-beijing.volces.com/api/plan/v3`（Agent Plan；标准 Ark 是 `/api/v3`）；Anthropic base `.../api/plan/compatible/v1`（标准 Ark 是 `/api/compatible`）。`anthropic_base_url` 须自带 `/v1`（代理剥客户端 `/v1`）。鉴权双写（同 DeepSeek，`provider/volcengine.go`）。
- Agent Plan 的 5h/每日/周/月额度在 **GetAFPUsage**（火山引擎签名 OpenAPI：`Action=GetAFPUsage&Version=2024-01-01&serviceCode=ark`，管控面，HMAC-SHA256/V4，需 AccessKey/SecretKey）—— Ark API Key（Bearer）调不了 GetAFPUsage，但能调 `/models`（`login` 用作 key 校验）。`volcengine_sign.go` 做 V4 签名（CredentialScope `{date}/cn-beijing/ark/request`，signing key 链 SK→kDate→kRegion→kService→kSigning，**末项 `"request"` 非 `"volcengine_request"`**；签名头仅 `host;x-date`，**不含 `x-content-sha256`**）。`login volcengine` 收 Ark API Key（必填）+ AK/SK（可选，仅 chat 可缺省）；`login` 用 `usage_url`（Bearer GET `/api/plan/v3/models`）验 Ark Key，可选 AK/SK 经 GetAFPUsage 验证（401/403 拒，其余放行）。`usage volcengine` 解析 `Result.{AFPFiveHour,AFPDaily,AFPWeekly,AFPMonthly}`（各 `Quota/Used/ResetTime`）。`models refresh` 调 **ListArkAgentPlanModel**（同理 V4 签名）解析 `Result.Datas[].ModelID`，经正则过滤 + endpoint 探测后写。未配 AK/SK 退化列 config 模型。
- 模型 ID 是模型名（如 `doubao-seed-1-8-251228`），非推理接入点 endpoint id。

## models.dev 元数据契约（`modelsdev.go`，实测）

- 数据源 `GET https://models.dev/api.json`（raw 3.05 MB）。gzip 后 ~286 KB（Go Transport 自动 gzip——**勿手动设 Accept-Encoding**，否则关掉自动解压）；`If-None-Match`→304 返回 0 字节。磁盘缓存只存去重 slim 投影（`by_name` 244 项，~150 KB），**绝不存 3 MB blob**。
- 缓存 `~/.model-proxy/models_cache.json`，TTL 24h，atomic tmp+rename。`ensureCatalogFresh`：fresh→用；stale/force→conditional GET；fetch 失败+有旧→用旧+stderr；无→空 catalog。`MP_MODELSDEV_URL` 覆盖端点。
- 匹配：`lookup()` 纯全局精确名查找，未命中→default（无 endpoint/后缀匹配逻辑）。借名模型（aqp 借的 `glm-*`/`deepseek-*`）靠 parse 期 `by_name` 去重时 canonical owner 胜出解析。
- **`models:` 只配名字；元数据全来自 models.dev**（context/output/modalities/`tool_call` 运行时由 `hydrateModels` 补，失败→default；slim 投影含 `tool_call` 供能力路由）。effective = config 名字 ∪ routes 引用模型。
- **`models refresh <provider>` 写 config**：拉 upstream 列表 + 现有 `models:` 合并去重 → 逐个 endpoint 探测（`probeModelCallable` 复刻 forward 的 base/path/auth）→ 仅 2xx 保留 → **覆盖写**回 `models:`（非 append-only）。`FilterModelIDs` 做静态策略过滤。探测 infra 不可用→写未校验合并集；**全部失败→保留 config 不清空+告警**。写是保注释的 yaml.Node 往返。**只有 `models refresh` 写 config.yaml；`models`/`takeover` 显示永不写**。
- 覆盖盲区：models.dev **没有** aqp/compass、codex/ChatGPT、volcengine；未命中→default（能力路由的逃生口：provider config `capabilities:`，见 AGENTS.md「请求感知路由」）。
- 作用域：`models`/`takeover` CLI 调 `ensureCatalogFresh`+`hydrateModels`（只改内存 cfg，不进 `LoadConfig`）；daemon 启动/reload 经 `initCatalog` 加载供请求感知路由用（失败→nil，路由功能 no-op）。`models` 显示 `SRC` 列（`models.dev`/`default`）。
