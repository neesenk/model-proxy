# 后端契约参考（逆向实测）

> 从 AGENTS.md 拆出。**改对应 provider 前必读**；新增 provider 契约追加于此。文件命名/凭据规则也在此。

## Token 文件命名

| provider_id | suffix | 文件名 |
|---|---|---|
| aqp / codex | oauth_auth | `~/.model-proxy/<name>_oauth_auth.json` |
| static | apikey | `~/.model-proxy/<name>_apikeys.json`（账号池）；无 legacy singular fallback |
| zhipu / zcode / deepseek / kimi-code / qwen-plan | apikey | `~/.model-proxy/<name>_apikeys.json`（账号池）；旧单数 `_apikey.json` 仅只读 fallback |
| volcengine | apikey | `~/.model-proxy/<name>_apikeys.json` — 每账号 `{api_key, access_key, secret_key}`；旧单数仅 fallback |

路径从 provider name（config 一级 key）派生，支持多实例（如 `zhipu-personal` / `codex-work`）。多账号见 `docs/architecture/provider-pools.md`。所有路径（CLI `login`、`BuildOne`、web 异步登录、`logout`）一律用 config name，**包括 aqp/codex**（`RunLogin`/`runCodexLoginFlow` 接收 `provName` → login 内部 `oauthAuthFilePath(HomeDir(), provName)`，与 `internal/accounts` 的 `AuthFilePath(provName, "oauth_auth")` 同一路径）；曾有的「CLI login 硬编码 provider_id → 非同名实例读写错位」bug 已修，`TestAqpCodexLogin_UsesConfigNameForAuthFile` 守护。OAuth 文件不得由 Web/CLI 直接 `os.WriteFile`/`os.Remove`：codex 统一经 `provider.WriteCodexAuthFile` / `provider.ClearCodexAccount`，aqp 经对应 provider helper，最终由 `credstore.Ref` 执行 file/keychain 选择、原子权限、来源标记与跨模式删除。

## 可取消模型列表读取

daemon 模型刷新通过 `provider.FetchModelsContext` 调用各 HTTP provider 的
`FetchModelsContext(context.Context)` 能力；Bearer `/models`、Codex 定制列表和
Volcengine V4 callback 均把 context 传到 HTTP 请求。原有 CLI `FetchModels()` 仍复用同一
读取逻辑。缺少可取消能力的实现返回 unsupported，不能启动脱离生命周期的后台 fetch。
刷新事务与取消后的副作用契约见 [Web/API](web-api.md#模型刷新提交边界)。

## compass 网关契约（实测）

> model-proxy 内部称 `aqp`（config `provider_id`、`aqp_oauth_auth.json`、`aqp_mint_url`）；上游是 `compass.llm.shopee.io`，故保留 compass/CQP 称谓。

| 端点 | 方法 | 鉴权 | 路径 |
|---|---|---|---|
| CQP key 签发 | POST | SSO cookie | `/api/v1/cqp/ccswitch/api_key/get_or_generate` body `{}` |
| 模型列表 | GET | CQP Bearer | `/compass-api/v1/models` |
| 消息（anthropic） | POST | CQP Bearer | `/compass-api/v1/messages` |
| 响应（openai） | POST | CQP Bearer | `/compass-api/v1/responses` |
| 月度用量 | POST | SSO cookie | `/api/v1/cqp/ccswitch/monthly_usage` body `{"project_id":"..."}` |
| 客户端版本检查 | POST | SSO cookie | `/api/v1/cqp/ccswitch/client-version/check` body `{"client_version":"x.y.z","platform":"macos|windows|linux"}`（AIS Switch 0.2.1 新增） |

CQP key 长效，缓存 50min。SSO cookie 值已含 `SSO_C=` 前缀，直接作 Cookie 头值。`/v1/messages` 走 cqp 时需 `?beta=true`、`anthropic-version: 2023-06-01`、`x-compass-request-id`(UUID)。转发头用白名单（不透传客户端 Cookie/Authorization）。

**0.2.1 实测增补**（2026-07-28，对照 AIS Switch 0.2.1 二进制 + 线上探测；均为增量、向后兼容，model-proxy 无需改动）：

- `get_or_generate` 的 `data` 扩为 9 字段：`api_key`、`generated`、`quota_type`（如 `"CQP"`）、`employee_email`、`employee_user_id`、`employee_role`、`team_name`、`project_id`、`business_name`、`project_name`。代理只读 `api_key`/`project_id`/`employee_email`，不受影响。
- `client-version/check`：`client_version`/`platform` 均必填（缺→422），platform 大小写/别名归一（`darwin`/`mac`→`macos`）。无 cookie→401 `Session expired`。`data` = `{force_update, latest_version, force_below_version, download_url(S3 签名), team_id, reason, message, release_notes{zh,zh-TW,en,ja,...}, platform, updater_endpoint}`；`client_version < force_below_version` 时 `force_update=true`（reason 如 `cqp_ccswitch_global_whitelist`）。
- 客户端新增自标识头 `x-ccswitch-client`（模型拉取与转发都带），**值是 app 裸版本号**（如 `0.2.1`，无 `ais-switch/` 前缀——app 日志的 `client_version=0.2.1` 字段 + 二进制 rodata 共置 + 无任何前缀格式串三重佐证）。网关**不校验**：不带/任意值均 200，行为无差异。代理经通用 provider 级 `headers:` 发送（config.yaml aqp 节配 `x-ccswitch-client: "0.2.7"`，与 AIS Switch 版本对齐、随其升级同步改）——转发/探测/Fusion/Shadow 各路径在 `ExtraHeaders` 前统一应用 `prov.Headers`，无需 aqp 专属代码。
- `monthly_usage` 契约不变（7 个 snake_case 字段）；二进制里的 camelCase（`projectId` 等）是 app 内 serde 命名，不是 wire 格式。
- `auth/info` 的 `data.user` 新增 `teams`/`hris_status`/`updated_time`/`usertype`，顶层新增 `roles`/`permissions` 数组；成功信封消息键是 `errmessage`（非 `message`）。代理只读 `user.{userid,email,is_active}`，不受影响。
- `/v1/messages` 响应 usage 扩展：`cost`、`price_cost_usd`、`cost_details{upstream_inference_cost,...}`、`is_byok`、`speed`、`inference_geo`、`output_tokens_details{thinking_tokens}`；顶层有 `container`/`stop_details`。字节透传与转换层均忽略未知字段。
- `/compass-api/v1/models`：**2026-07-31 实测契约已变**——对有效 CQP key 不再返回空，而是返回**全量通用 MaaS 目录**（232 个 id：Chirp/Qwen 全系列/DeepSeek-R1/fal 视频模型等 + 全套 coding 模型 kimi-k3/glm-5.2/gpt-5.4/deepseek-v4-*/claude-opus-4-8/claude-sonnet-5）。每条 `{id, object:"model", owned_by, created:0, all_providers:[{<后端>:[<子provider>...]}], context_window?}`：`owned_by` 是属主（anthropic/openai/openrouter/google/qwen/fal），`all_providers` 是可服务后端，`context_window` **可选**（部分模型有才返回，如 kimi-k3=1048576、gpt-5.4=1050000、claude-opus-4-8 无）。**不含** inputTypes/推理 variants/smartRanking/output 上限——那些是 AIS Switch 客户端内置 catalog 富化的，非网关下发。`?app_type=` 被忽略（返回同样通用列表）。⚠️ 对 `models refresh` 的影响：该端点从「返回空→走 config 兜底」变成「返回 232 个通用模型」，若直接消费会把大量非 coding 的通用 MaaS 模型灌进 models:，需按 curated coding 集合过滤或继续以 config models: 为准。（2026-07-28 时该端点对有效 CQP key 返回空 `data:[]`、cookie 鉴权→401 `API key not found`。）
- **gpt-5.x 系列（gpt-5.4/5.5/5.6-*）拒绝 `max_tokens`**：网关把 anthropic `/v1/messages` 的 `max_tokens` 透传给 OpenAI 后端，后端 400 返回 `Unsupported parameter: 'max_tokens' ... Use 'max_completion_tokens' instead.`（外层包一层 `{"message":"<内层 JSON 字符串>"}`）。这不是模型不可调用——`internal/probe.Callable` 识别该错误后把 probe body 的 `max_tokens` 改名为 `max_completion_tokens` 重试一次再判定（2026-09 实测：重试后这些模型 2xx 可保留）。

## codex 后端契约（实测）

| 端点 | 方法 | 鉴权 | 备注 |
|---|---|---|---|
| responses | POST | OAuth Bearer | `/backend-api/codex/responses`，需 `store:false` + `stream:true`，不接受 `max_tokens`；body 用 `input`（**必须是 list**，字符串→`Input must be a list`；不能用 `messages`→`Unsupported parameter: messages`） |
| usage | GET | OAuth Bearer | `/backend-api/wham/usage`（**不在 `/codex/` 子路径下**） |
| models | GET | OAuth Bearer | `/backend-api/codex/models?client_version=<ver>`，`{"models":[{slug,visibility,...}]}`，仅取 `visibility=="list"`；`client_version` 决定可见模型 |
| headers | — | — | `originator: codex_cli_rs` 必须（否则 403）；`ChatGPT-Account-Id` 从 id_token JWT 解析 |

**协议归属**：codex 是 `responses` 协议（`/v1/responses`，`input` list）。`ProtocolHint("codex") = "responses"`——转发路径经 `resolvedBackendProto` 自动回退到该 hint(forward/fusion/shadow 共用),所以 anthropic/chat 客户端打 codex 路由**自动转换**(无需显式 `protocol: responses`)。anthropic/chat 客户端经 `internal/protocol/convert_responses.go` 的 Responses 转换器可达 codex（请求 anthropic/chat→responses，响应/流式 responses→anthropic/chat），转换路径会预剥离 `max_output_tokens`/`temperature`/`top_p`；原生 responses 客户端（codex CLI / opencode Responses 模式）走字节级透传，不预剥离——codex 的 unsupported parameter 400 由 paramBlock 学习后预防性剥离自愈（`docs/architecture/routing-and-failure.md`）。codex 不再带 `WireProtocolNote`（已可转换）。

OAuth device flow（从 codex-rs 源码确认）：issuer `https://auth.openai.com`，client_id `app_EMoamEEZ73f0CkXaXp7hrann`。`POST /api/accounts/deviceauth/usercode` → `{device_auth_id, user_code, interval}` → 用户访问 `https://auth.openai.com/codex/device` → 轮询 `POST /api/accounts/deviceauth/token`（错误码 `deviceauth_authorization_pending`/`deviceauth_slow_down`）→ `{authorization_code,...}` → `POST /oauth/token` grant_type=authorization_code → tokens。刷新：`POST /oauth/token` grant_type=refresh_token。**代理用独立 OAuth**（不读 codex CLI `~/.codex/auth.json`），避免 refresh_token 轮换竞争。

## Zhipu BigModel 契约

- OpenAI base `https://open.bigmodel.cn/api/paas/v4`（`/chat/completions`、`/models`）；Anthropic base `https://open.bigmodel.cn/api/anthropic`（**不带 /v1**，代理保留客户端 `/v1/messages`；见 `internal/config` 校验），Bearer。代理按协议转发。
- 鉴权 `Authorization: Bearer <api_key>`（OpenAI 与 Anthropic 端点都用 Bearer，不像 DeepSeek 需 x-api-key）。
- `/models` 只列 8 个文本对话模型；多模态（glm-4v-plus/cogview-4-plus）需手动加 config。
- 配额 `GET .../api/monitor/usage/quota/limit`（Bearer）→ `{success, data:{limits:[{type,unit,number,percentage,nextResetTime,usage,currentValue,remaining,usageDetails}], level}}`；`type`=TOKENS_LIMIT|TIME_LIMIT，`unit` 3=5h/6=weekly/5=monthly。**`currentValue`=已用、`remaining`=剩余、`usage`=总额**（勿把 `usage` 当已用）。`/users/balance`、`/users/usage` 均 404。
- `usageDetails`：`TIME_LIMIT`（月度）按 **MCP 工具**拆（search-prime/web-reader/zread，工具调用消耗非模型 token），`TOKENS_LIMIT` 按**模型**拆。

## zcode 契约（BigModel Coding Plan + ZCode 指纹）

`zcode` 是 BigModel 的 Coding Plan 变体（同一后端、同一配额信封），转发时附带 ZCode 客户端指纹，使 Coding Plan API key 享受套餐配额（0.67 消耗系数 ≈ 1.5×）且不被当作通用 agent 降级。**2026-09-21 ZCode 已开源（`github.com/zai-org/ZCode`，Apache-2.0，main = v3.14.0），本契约指纹值从"逆向实测"升级为"源码对照"**；3.3.6 ASAR 探测与 2026-09-11 mitmproxy 抓包（3.11.2）结论保留为历史佐证，设计文档见 `docs/superpowers/specs/2026-07-20-zcode-provider-design.md`（末尾附 2026-09-21 开源复核补遗）。

- 端点同 zhipu：OpenAI `https://open.bigmodel.cn/api/paas/v4`；Anthropic `https://open.bigmodel.cn/api/anthropic`（**不带 /v1**）。开源内置 catalog（`config/provider/zcode-builtin.json`，revision 30）`bigmodel-api` 模板 = `access: zhipu-coding-plan-api-key` + `anthropic-messages` + 上述 base，模型 `GLM-5.3`/`GLM-5.3-Flash`（大写）；普通 `bigmodel-standard-api` 模板才是 `api-key` + OpenAI `paas/v4`。Coding Plan 折扣由 key+端点决定，**无单独 coding base**。
- **客户端已不走直连路径（3.14.0 起，本模拟最大结构差异）**：`official-coding-plan-gateway.ts` 把 `https://open.bigmodel.cn/api/anthropic/v1/messages` 改写为 **`https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages`**（`api.z.ai` → `/api/v1/ultra-zai/anthropic/v1/messages`），经 `createProviderTransportFetch` 对所有 model provider 生效，"方法/body/鉴权头原样透传"，仅按下表精确匹配官方端点。即真实 ZCode 3.14.0 在网络上不再直连 open.bigmodel.cn；本代理仍直连（2026-09 实测两端均存活：无凭据时直连报 BigModel `1001`、网关报 `4011 SERVICE_AUTHENTICATION_INVALID`）。套餐系数是否仍认直连路径需真 key 复测（见下"复测清单"）。
- 鉴权**双写**：每请求同时发 `Authorization: Bearer <key>` + `x-api-key: <key>`。源码确认（`model-execution.ts`）：`createAnthropic({apiKey})` 令 AI SDK 发 `x-api-key`，`withAnthropicAuthorizationHeader` 在无显式 Authorization 时补 Bearer（源码注释："Anthropic 兼容网关会同时读取 x-api-key 和 Bearer Authorization"）；与 zhipu 只发 Bearer 不同。2026-09-11 抓包两头同值（CLI apikey、桌面 JWT 均双写）。
- 指纹三处拼合：① base `packages/shared/src/zcode-source-headers.ts`（`buildZCodeSourceHeadersFromContext`：`HTTP-Referer` 默认 `https://zcode.z.ai`、UA 基座 `ZCode/<ver>`、`X-Title` `Z Code@<sourceTitle>`、`X-ZCode-App-Version`、`X-Platform` `<platform>-<arch>`、`X-Release-Channel`、`X-Client-Language`/`X-Client-Timezone`、`X-Os-Category`（darwin→macos/win32→windows/else linux）、`X-Os-Version`、`X-Device-Mid` 仅 telemetry-state.json 有值才发；normalize 规则 = trim + `[\x20-\x7e]` printable 否则丢弃，language/timezone 回落 `unknown`）；② CLI 聊天路径 `apps/zcode-cli/packages/bootstrap/src/model-config.ts`（`buildCliZCodeSourceHeaders`）追加 **`X-ZCode-Agent: glm`** 与 `X-Release-Channel`（`resolveRuntimeZCodeEnv` → production/test）；③ 平台头 `runtime-platform-headers.ts`（`X-Platform`/`X-Os-Category`/`X-Os-Version`，取 `process.platform`/`os.arch()`/`os.release()`）。`anthropic-version: 2023-06-01` 由 `@ai-sdk/anthropic@3.0.81` 默认注入（dist 确认）。
- **UA 构成（3.14.0）**：`ZCode/3.14.0 ai-sdk/anthropic/3.0.81 ai-sdk/provider-utils/4.0.27 runtime/node.js/24`。追加顺序：createAnthropic 建 provider 时加 `ai-sdk/anthropic/<ver>`，provider-utils 发请求时加 `ai-sdk/provider-utils/<ver>` + runtime 段；Node≥21.1（CLI 引擎要求 node≥24，`.nvmrc` 24.14.0）runtime 走 `navigator.userAgent` 分支 → `runtime/node.js/<major>`，与 3.11.2 抓包的 `/24` 吻合。注意 3.11.2 抓包 UA **没有** `ai-sdk/anthropic/` 段（当年 SDK 版本不同），代理按 3.14.0 源码含该段；若复抓 3.14.0 见异常可回落。
- **X-Title 取 `Z Code@cli`**：源码 `detectDefaultProviderSourceTitle()` 按 argv 是否含 app-server/agent-server 选 `electron`/`cli`——桌面 agent-server 对应 OAuth 订阅路径，独立 CLI + 用户自带 coding-plan apikey（本代理模拟的 `bigmodel-api` 模板）发 `cli`。历史抓包来自桌面进程故旧文档记 `electron`。
- **归因 id 头**（`adapters/src/model/runner-attribution.ts` `createModelRequestAttributionHeaders`）：`x-request-id`（每模型调用新 v4 UUID，**重试 attempt 也换新**）、`x-zcode-session-type`（`main`/`subagent`/`other`，**Coding Plan 服务端用它区分请求来源**，由 actorKind+operation 推导，非 id）、`x-zcode-trace-id`（`createTraceId()` = randomUUID）、`x-query-id`（`query_<uuid>`）、`x-session-id`（`sess_<uuid>`；后两者发 wire 前剥内部前缀，线上即裸 UUID）。代理同发五个：request/trace 每请求新 UUID；query 有客户端 `X-Interaction-Id` 时派生稳定 UUID（一次交互的多次调用共享，贴合 per-query 作用域）、否则每请求新；session 从客户端 `X-Claude-Code-Session-Id`/`X-Session-Id`/`User-Id` 派生稳定 UUID（sha256→v4 形状，跨重启稳定），无 hint 时回落进程级随机 UUID（探针/direct 客户端）。`X-Client-Language` 代理用 LC_ALL/LC_MESSAGES/LANG 近似 Intl locale 并归一化为 BCP-47（`en_US.UTF-8`→`en-US`，`C`/`POSIX` 跳过）；`X-Client-Timezone` 读 TZ，未设时回退 `/etc/localtime` 符号链（ICU 行为），再否则 `unknown`。
- **探针行为（3.14.0 已变）**：`providerSettingsConnectivity.ts` 的连通性测试直接跑正式 model 执行链——完整指纹 + 归因头，与转发一致。3.11.2 时代"apikey connectivity probe 只发 3 个基础头（UA `ZCode/unknown` + referer + X-Title）"的差异已不存在；代理对探针与转发统一发完整指纹，与现行客户端一致（唯一差异：探针也带 `x-zcode-session-type: main`，真实探针为 `other`，纯观测字段）。
- **签名**：开源仓库**不含**任何签名代码（无 `x-client-sig`/`x-client-pow`/`c1f3a7e2` 握手；源码里的 `signature` 命中均为 Anthropic thinking block 字段）。桌面端 3.11.2 抓包观测到的 V4 签名器（先 `POST open.bigmodel.cn/api/paas/c1f3a7e2/v2/client` 握手 `{apiKey,nonce,sig,ts}`，再附 `x-client-sig/-pow/-ts/-nonce/-version` + `x-app-id: zcode`，feature gate 对 *.z.ai 与 *.bigmodel.cn 均已激活）只在桌面二进制里，CLI 引擎无签名路径。**model-proxy 无法复现签名**：本代理模拟的 apikey coding-plan 路径在抓包窗口内未被服务端强制验签（现有部署仍工作）；若出现异常 403/401 优先重抓该路径。
- **端点补充事实**：桌面端 Ultra（OAuth JWT）模型请求走 `zcode.z.ai/api/v1/ultra/anthropic`；apikey Coding Plan 的 catalog base 是 `open.bigmodel.cn/api/anthropic`（本代理目标），但 3.14.0 客户端会把它改写进 ultra 网关（见上）。
- `provider_id: zcode`；`login zcode` 开 `bigmodel.cn/login` + apikey 池（多账号）。配额/usage 复用 zhipu 的 `quota/limit` 解析（`ParseZhipuQuota`；开源侧 `bigmodelUsageQuotaProvider.ts` 仍为 `/api/monitor/usage/quota/limit`）。**未实现 OAuth/JWT 登录**（`zcode://oauth/callback` 自定义 scheme CLI 无法截获，且 apikey 路径已足够）。3.14.0 另有 start-plan provider（`account:bigmodel-start-plan`/`account:zai-start-plan`）与 off-peak 子系统（`X-Coding-Plan-Api-Key` + `X-Off-Peak-Ticket-ID` + JWT，仅闲时路径），均不触碰本代理的 apikey 路径。另注：`officialMcpCredentials.ts` 注释称服务端已把 API key 通道标为"仅存量客户端兼容"（指 MCP quota 查询通道，非模型端点，但是同方向信号）。
- **复测清单（开源后待办）**：① 用真 coding-plan key 对直连 `open.bigmodel.cn/api/anthropic/v1/messages` 跑 messages + `quota/limit`，确认套餐系数/优先级仍在；② 若失效，评估把 `anthropic_base_url` 切到 `https://zcode.z.ai/api/v1/ultra/anthropic`（指纹与鉴权头原样透传，仅换 base）；③ 复抓 3.14.0 桌面/CLI 核对 UA 是否含 `ai-sdk/anthropic/` 段、直连路径是否还有真实流量。

## DeepSeek 契约（双协议，一个 key）

- OpenAI base `https://api.deepseek.com`（`/chat/completions`、`/responses`、`/models`、`/user/balance`，Bearer）；Anthropic base `https://api.deepseek.com/anthropic`（**不带 /v1**，代理保留客户端 `/v1/messages`；`x-api-key`，`anthropic-version`/`anthropic-beta` 被忽略）。
- Anthropic SDK 打 `/anthropic/v1/messages`（base `/anthropic` + `/v1/messages`）。代理保留客户端的 `/v1/messages`（不剥），故 `anthropic_base_url` **不带 /v1**（`internal/config` 校验拒绝尾部 `/v1`）。`RewriteRequest` no-op，URL 选择在 `proxy.forward` 按 protocol 完成。
- 鉴权双写：每请求同时设 `Authorization: Bearer` + `x-api-key`，一个 config 服务两协议。
- 服务端模型自动映射（Anthropic）：`claude-opus*`→`deepseek-v4-pro`；`claude-sonnet*`/`claude-haiku*`→`deepseek-v4-flash`。
- `/user/balance` → `{is_available, balance_infos:[{currency, total_balance, granted_balance, topped_up_balance}]}`（注意 `balance_infos` 非 `wallets`）。`/models` → OpenAI 风格；当前 `deepseek-v4-pro`/`deepseek-v4-flash`，旧名 `deepseek-chat`/`reasoner` 2026-07-24 弃用。

## 千问 Token Plan 个人版契约（实测 + 探测）

- OpenAI base `https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1`（`/chat/completions`、`/responses`、`/models`，Bearer）；Anthropic base `https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic`（**不带 /v1**，代理保留客户端 `/v1/messages`，同 DeepSeek/zhipu）。代理按协议字节级透传，`RewriteRequest` no-op，URL 选择在 `proxy.forward` 按 protocol 完成。
- 鉴权双写：每请求同时设 `Authorization: Bearer <sk-sp-key>` + `x-api-key: <key>`（同 DeepSeek/zcode，覆盖 Anthropic 网关偏好；OpenAI 端忽略 x-api-key）。key 为 `sk-sp-` 前缀的**套餐专属 key**，与通用 `sk-` key / `dashscope.aliyuncs.com` 域名**不可混用**（混用报鉴权错误）。
- `/models` 为真实路由端点（无鉴权返回结构化 `{"code":"InvalidApiKey","message":"No API-key provided."}` 401，非 404）；但套餐可能不开放列表（Coding Plan FAQ 称「模型列表不支持通过接口查询」），故 `FetchModels` 失败时回退 config `models:`。个人版模型：`qwen3.8-max-preview`/`qwen3.7-max`/`qwen3.7-plus`/`qwen3.6-flash`/`glm-5.2`/`deepseek-v4-pro`（另有 `wan2.7-image`/`happyhorse-*` 走独立生成接口，不进 chat `models:`）。
- **无公开用量/Credits 接口**：个人版 5h/7d Credits 用量仅在控制台「用量分析」页（文档「以控制台订阅页用量明细为准」）。`Quota()` 返回 `BillingUnknown` + `Notes`（含控制台 URL `https://platform.qianwenai.com/home/billing/subscription/token-plan-individual`），CLI `usage` 与 Web UI（`/api/status.quota` → app.js 渲染 `snap.Notes`）均展示该 URL。**有意不轮询/不抓控制台**（尊重平台「严禁 API 调用」条款，见 `docs/decisions/intentional-behaviors.md`）。
- 配额窗口：5h = 700/3000/12000 Credits，7d = 2500/10000/40000 Credits（Lite/Standard/Pro）；每次消耗同时计入两层，任一层触顶暂停。429 `Allocated quota exceeded`（窗口耗尽）→ `internal/targetexec.ParseRateLimit` 分类为 `quota`（默认 1h 冷却或 body reset hint，上限 7d）→ 调度跳过并 failover；429 `Requests rate limit exceeded`（并发限频）→ `transient`（60s）。
- `login qwen-plan` 无 `usage_url`，走 `apiKeyValidationURL` 兜底：用 `openai_base_url/models`（Bearer GET，401/403 拒）验 key。`Logout` = `DeleteKey`。`ProbeRequest` 覆盖为 anthropic `/v1/messages` + `anthropicProbeBody`、`ExtraHeaders` 注入 `anthropic-version: 2023-06-01`（`probe.Callable` 在 `anthropic_base_url` 存在时走 anthropic base，故 probe 路径必须 anthropic；OpenAI 的 `/chat/completions` 套在 anthropic base 上会 404）；`FilterModelIDs` 用 baseProbe 默认透传。无 `ProtocolHint`/`WireProtocolNote`（双协议直通）。

## Volcengine Ark 契约（双协议，含 Agent Plan，一个 key）

- OpenAI base `https://ark.cn-beijing.volces.com/api/plan/v3`（Agent Plan；标准 Ark 是 `/api/v3`）；Anthropic base `.../api/plan`（标准 Ark 是 `/api/compatible`）。代理保留客户端的 `/v1/messages` 路径（base + `/v1/messages`），故 `anthropic_base_url` **不带** `/v1`（同 DeepSeek/qwen-plan）。鉴权双写（同 DeepSeek，`internal/provider/volcengine.go`）。`ProbeRequest` 覆盖为 anthropic `/v1/messages` + `anthropicProbeBody`、`ExtraHeaders` 注入 `anthropic-version: 2023-06-01`——`probe.Callable` 在 `anthropic_base_url` 存在时走 anthropic base，probe 路径必须 anthropic；OpenAI 的 `/chat/completions` 套在 anthropic base 上（`.../api/plan/chat/completions`）会 404（曾导致 `models refresh volcengine` 全模型探测失败）。
- Agent Plan 的 5h/每日/周/月额度在 **GetAFPUsage**（火山引擎签名 OpenAPI：`Action=GetAFPUsage&Version=2024-01-01&serviceCode=ark`，管控面，HMAC-SHA256/V4，需 AccessKey/SecretKey）—— Ark API Key（Bearer）调不了 GetAFPUsage，但能调 `/models`（`login` 用作 key 校验）。`internal/provider/volcengine_sign.go` 做 V4 签名（CredentialScope `{date}/cn-beijing/ark/request`，signing key 链 SK→kDate→kRegion→kService→kSigning，**末项 `"request"` 非 `"volcengine_request"`**；签名头仅 `host;x-date`，**不含 `x-content-sha256`**）。`login volcengine` 收 Ark API Key（必填）+ AK/SK（可选，仅 chat 可缺省）；`login` 用 `usage_url`（Bearer GET `/api/plan/v3/models`）验 Ark Key，可选 AK/SK 经 GetAFPUsage 验证（401/403 拒，其余放行）。`usage volcengine` 解析 `Result.{AFPFiveHour,AFPDaily,AFPWeekly,AFPMonthly}`（各 `Quota/Used/ResetTime`）。`models refresh` 调 **ListArkAgentPlanModel**（同理 V4 签名）解析 `Result.Datas[].ModelID`，经正则过滤 + endpoint 探测后写。未配 AK/SK 退化列 config 模型。
- 模型 ID 是模型名（如 `doubao-seed-1-8-251228`），非推理接入点 endpoint id。

## TypeSafe System One 契约（官方文档）

- decisions base `https://api.typesafe.ai/v1`（`decisions_base_url` 配置；OpenRouter 兼容同一 shape，用 `openai_base_url: https://openrouter.ai/api/v1` 走回落即可）。端点 `POST /systemone`，`Authorization: Bearer <key>`；请求 `{model, state, questions}`（questions 三种题型：`choice` criteria 为 map[string]string ≤255 项、`score` criteria 为 []string 2–10 级、`noul` 无 criteria），响应 `{model, answers, usage:{input_tokens, output_tokens}}`（OpenRouter 附加 `id`/`provider`/`usage.cost`，解析容忍）。答案 typed：choice 返回 `choice`+逐项 `probabilities`+`confidence`，score 返回可落在级别之间的分值，noul 返回 0..1 概率（无 confidence）。无流式、无 tools、纯文本（不看图）。
- 模型 id：`jev-latest`（滚动别名，会漂移）/`jev-1.13.0`（精确版本，pin 用）；调过阈值的部署应 pin。定价输入 $0.042/M token、输出免费；官方延迟 70–500ms。**实测怪癖（2026-09）**：`GET /v1/models` 只返回短家族 id（如 `jev-1.13`），而 `/systemone` 拒绝它（`api_usage_error: Unknown model`）——可调用的版本 id 要补 `.0`（`jev-1.13.0`）；`FilterModelIDs` 把短 id 改写成可调用形（别名 `jev-latest` 不在 /v1/models 里，preset 手工带）。
- `GET /v1/models` 存在（`login typesafe` 的 key 校验与 `FetchModels` 均走它，Bearer）——返回 shape 以实测为准，解析失败时在 provider 内收敛，不扩散。
- **无公开 billing/quota 接口**：`Quota()` 返回 `BillingUnknown` + 控制台 Notes（`https://console.typesafe.ai`），CLI `usage typesafe` 打印计费说明 + config 模型列表。
- `ProbeRequest` 覆盖为 `POST /systemone` + 最小 noul 题（probe 框架在仅 decisions_base_url 时用其作 base，不重写 path/body）；`AuthHeaders` 用 ApiKeyBase 默认 Bearer（剥离 x-api-key）；`RewriteRequest` no-op；`FilterModelIDs` baseProbe 默认透传；`ProtocolHint("typesafe") = "decisions"`（隐式路由自动声明，chat 客户端打到 target 由 fail-closed stub 拒绝）。

## models.dev 元数据契约（`internal/catalog`，实测）

- 数据源 `GET https://models.dev/api.json`（raw 3.05 MB）。gzip 后 ~286 KB（Go Transport 自动 gzip——**勿手动设 Accept-Encoding**，否则关掉自动解压）；`If-None-Match`→304 返回 0 字节。磁盘缓存只存去重 slim 投影（`by_name` 244 项，~150 KB），**绝不存 3 MB blob**。
- 缓存 `~/.model-proxy/models_cache.json`，TTL 24h；写入使用目标目录中的唯一临时文件再 atomic rename，多个进程不会争用固定 `.tmp`。`catalog.EnsureFresh`：fresh→用；stale/force→conditional GET；304→刷新并持久化 `fetched_at`/ETag；合法 200→重建并持久化。fetch 错误、非 2xx 或 malformed 200 有旧 cache 时告警并回落旧值，无旧 cache 时返回空 catalog + error，绝不以坏响应覆盖旧数据。`MP_MODELSDEV_URL` 由 `internal/config/modelscatalog.go` 覆盖端点。
- 匹配：`Catalog.Lookup()` 纯全局精确名查找，未命中→default（无 endpoint/后缀匹配逻辑）。借名模型（aqp 借的 `glm-*`/`deepseek-*`）靠 parse 期 `by_name` 去重时 canonical owner 胜出解析。
- **`models:` 只配名字；元数据全来自 models.dev**（context/output/modalities/`tool_call` 运行时由 `hydrateModels` 补，失败→default；slim 投影含 `tool_call` 供能力路由）。effective = config 名字 ∪ routes 引用模型。
- **`models refresh <provider>` 写 config**：拉 upstream 列表 + 现有 `models:` 合并去重 → 逐候选经 `probe.ProbeModelProtocols` 跑三协议矩阵探测（chat/responses 打 openai base，anthropic 仅在配置 `anthropic_base_url` 时探；复刻 forward 的 base/path/auth；上游以 `max_completion_tokens` 为由拒绝 `max_tokens` 时按腿自动改名重试一次，见 compass 契约节），每腿经 `wirecap.ClassifyModelStatus` 归类 → **任一腿 Yes 即保留**，全否 drop（原因 = 逐腿摘要 `chat HTTP 404 / anthropic not probed (no base) / responses HTTP 400: invalid model`，CLI 渲染为 `not callable on any protocol - <legs>`）→ **覆盖写**回 `models:`（非 append-only）。探测成功后把该 provider 的新鲜矩阵**整体替换式**写入 `~/.model-proxy/model_caps.json`（fingerprint = `providerbuild.ProtocolConfigFingerprint`，best-effort，失败仅 stderr 告警），供 daemon 的模型级协议选择与 `models` 列表 PROTOCOLS 列复用。`FilterModelIDs` 做静态策略过滤。探测 infra 不可用→写未校验合并集（不写 model_caps.json）；**全部失败→保留 config 不清空+告警**。写是保注释的 yaml.Node 往返。**只有 `models refresh` 写 config.yaml；`models`/`takeover` 显示永不写**。daemon 内有同一流程的孪生：`POST /api/models/refresh`（`internal/admin.RefreshModels`，UI Status→Models 每 provider Refresh 按钮）复用运行中的 provider impl 做 fetch+探测，写 config 后**进程内热 reload**（不经 SIGHUP），并用新鲜矩阵经 `ModelStore.ReplaceProviderModels` 整体替换内存 model_caps 缓存（fingerprint 取自当前 generation；与 CLI 的差异：探测全失败/impl 缺失时**不**替换缓存，fail-closed）。
- 覆盖盲区：models.dev **没有** aqp/compass、codex/ChatGPT、volcengine；未命中→default（能力路由的逃生口：provider config `capabilities:`，见 `docs/architecture/request-routing.md`）。
- 作用域：`models`/`takeover` CLI 经 `internal/config/modelscatalog.go` 调 `catalog.EnsureFresh` +
  `internal/routing.HydrateModels`（只改内存 cfg，不进 `LoadConfig`）；daemon 启动/reload 经
  `initCatalog` 加载供请求感知路由用（失败→nil，路由功能 no-op）。`models`
  显示 `SRC` 列（`models.dev`/`default`）。
