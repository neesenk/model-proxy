# AGENTS.md - Provider 实现规则

修改本目录前必须阅读 `../../docs/backend-contracts.md`。后端逆向实测结果以该文件为唯一权威来源。

## 抽象边界

- 每个 provider 实现 `provider.Provider`：
  `AuthHeaders`、`Refresh`、`RewriteRequest`、`Logout`、`Usage`、`FetchModels`、`Quota`、`ProbeRequest`、`ExtraHeaders`、`FilterModelIDs`。
- 凭据必需的 provider 通过可选接口 `AuthReadyProvider`（`AuthReady() bool`）声明路由资格：
  无凭据（未 login）的实现会被 `expandTarget` 从生效路由表（调度链、`/v1/models`、forward）剔除；
  `ApiKeyBase` 统一实现（绑定 key 或可读存储即 ready），aqp/codex 各自查 OAuth 存储（`cfg.Auth` 测试缝视为 ready）；
  免凭据实现（static）不实现该接口，构造即可路由。仅在 build/reload 期求值，不做请求热路径调用。
- `Login` 不属于接口，交互登录由 `internal/cli/login` 编排（组合根在 `internal/app`）。
- 共享默认行为通过 `ApiKeyBase` 和 `baseProbe` 组合：默认 probe 是 OpenAI `POST /chat/completions`，默认 ExtraHeaders no-op，默认 model filter 透传。
- `ModelInfoLister`（可选接口，`FetchModelInfos`）：/models 自报展示名（`display_name`）的 provider 升级实现（当前 kimi-code），供 `models refresh` 展现“稳定 id 背后换模型”的信号；共享实现在 `fetchModelInfosBearer`，未实现的 provider 自动退化为纯 id。
- provider 专属 auth、endpoint、request rewrite、quota/usage parser、probe header 和 model filter 全部留在本包。
- 禁止要求 main 包根据 provider id 分支处理这些知识。
- `ProbeRequest`、`ExtraHeaders`、`FilterModelIDs` 的默认实现集中在 `baseProbe`。
- `Surplus` 是 `QuotaSnapshot` 方法，不属于 Provider interface。
- Auth、Logout、Usage、Quota 的 fetch 和 parse 由 provider struct 自己承载。`internal/providerbuild` 的 `BuildOne` 只允许保留无法泛化的窄回调，例如 volcengine V4 `FetchModelsFn`；本包保持 provider 实现的叶子地位。
- 终端着色与文本格式化 helper（`Green`/`Pad`/`Truncate`/`FormatDuration` 等）已迁至 `internal/display`（纯标准库叶子包）；Usage() 等展示代码一律使用 `display.X`，本包不再持有 display 逻辑。

## 凭据

- 凭据文件路径使用 config provider name，而不是硬编码 provider id。
- BoundAPIKey 实例只使用内存绑定 key，`Refresh` 不得回读其他账号文件。
- 池化账号必须隔离 API key、AK/SK、usage 和 quota closure。
- 日志禁止输出完整 token、cookie、auth response 或 secret。

## 协议

- `ProtocolHint` 只有在现有转换器真实支持目标 wire shape 时才能返回值。
- Codex 是 Responses API，不得标记成 Chat Completions `openai` hint。
- 不可表达的 wire protocol 使用 `WireProtocolNote` 警告，不得伪装成可转换。
- typesafe 标记 `decisions` hint：decisions 与 chat 协议族不可互转，但注册表对全部 12 个 pair 有定义——含 decisions 的 pair 是 fail-closed stub（chat 客户端得到 typed unsupported 错误而非错误 body），因此 hint 产生的是诚实失败而非误导性转换；这与 codex 规则的精神一致。
- `ChatReasoningMode`（protocol_hint.go，与 ProtocolHint 并列）：r→chat 转换时 reasoning.effort 的方言形状（`reasoning_effort`/`thinking`/`enable_thinking`/`openrouter`；当前登记 `zhipu`/`volcengine`/`kimi-code`/`deepseek`/`mimo`→`thinking`，`qwen-plan`→`enable_thinking`，`aqp` 及其别名 `shopee`、以及 `openrouter` 本尊→`openrouter`）。新 provider 的 chat 端点 reasoning 字段形状不是平铺 `reasoning_effort` 时才登记，默认不要加条目。
- `ChatEffortProfile`（reasoning_effort.go，与 ChatReasoningMode 并列）：chat 端点在 switch 形状之上接受的 effort 档位枚举（canonical → vendor 字符串映射；`EnumOnly` 表示枚举**替代** thinking switch，如 kimi-k3 拒绝 thinking+reasoning_effort 同发）。仅当 vendor 的 chat 端点接受非透传枚举或替代 switch 时才登记（当前 `deepseek`、`zhipu` 按 glm-5.3/5.2 门控、`kimi-code` 按 kimi-k3 门控、`qwen-plan` 按 qwen3.8 门控）；枚举按 model 子集碎片化的（volcengine）或原生透传的（aqp/shopee）保持 nil，默认不要加条目。登记时在注释中引用 vendor 文档 URL。

## 修改要求

- 修改 endpoint、header、body、认证或 quota parser 时同步更新 `backend-contracts.md`。
- 新 provider 优先复用 shared helper，不复制 Bearer `/models`、probe body 或 display 逻辑。
- 增加 provider-specific 行为时补正例、错误响应、空响应和 credential isolation 测试。
- 出站 HTTP client 一律带 `Transport: upstreamproxy.AutoTransport()`（config 全局 → env → 系统代理链），不得再用裸 `&http.Client{Timeout: ...}`。
