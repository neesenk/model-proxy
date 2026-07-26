# AGENTS.md - Provider 实现规则

修改本目录前必须阅读 `../../docs/backend-contracts.md`。后端逆向实测结果以该文件为唯一权威来源。

## 抽象边界

- 每个 provider 实现 `provider.Provider`：
  `AuthHeaders`、`Refresh`、`RewriteRequest`、`Logout`、`Usage`、`FetchModels`、`Quota`、`ProbeRequest`、`ExtraHeaders`、`FilterModelIDs`。
- `Login` 不属于接口，交互登录由 main 包 CLI 编排。
- 共享默认行为通过 `ApiKeyBase` 和 `baseProbe` 组合：默认 probe 是 OpenAI `POST /chat/completions`，默认 ExtraHeaders no-op，默认 model filter 透传。
- provider 专属 auth、endpoint、request rewrite、quota/usage parser、probe header 和 model filter 全部留在本包。
- 禁止要求 main 包根据 provider id 分支处理这些知识。
- `ProbeRequest`、`ExtraHeaders`、`FilterModelIDs` 的默认实现集中在 `baseProbe`。
- `Surplus` 是 `QuotaSnapshot` 方法，不属于 Provider interface。
- Auth、Logout、Usage、Quota 的 fetch 和 parse 由 provider struct 自己承载。main 的 `buildOne` 只允许保留无法泛化的窄回调，例如 volcengine V4 `FetchModelsFn`。

## 凭据

- 凭据文件路径使用 config provider name，而不是硬编码 provider id。
- BoundAPIKey 实例只使用内存绑定 key，`Refresh` 不得回读其他账号文件。
- 池化账号必须隔离 API key、AK/SK、usage 和 quota closure。
- 日志禁止输出完整 token、cookie、auth response 或 secret。

## 协议

- `ProtocolHint` 只有在现有转换器真实支持目标 wire shape 时才能返回值。
- Codex 是 Responses API，不得标记成 Chat Completions `openai` hint。
- 不可表达的 wire protocol 使用 `WireProtocolNote` 警告，不得伪装成可转换。
- `ChatReasoningMode`（protocol_hint.go，与 ProtocolHint 并列）：r→chat 转换时 reasoning.effort 的方言形状（`reasoning_effort`/`thinking`/`enable_thinking`/`openrouter`）。新 provider 的 chat 端点 reasoning 字段形状不是平铺 `reasoning_effort` 时才登记，默认不要加条目。

## 修改要求

- 修改 endpoint、header、body、认证或 quota parser 时同步更新 `backend-contracts.md`。
- 新 provider 优先复用 shared helper，不复制 Bearer `/models`、probe body 或 display 逻辑。
- 增加 provider-specific 行为时补正例、错误响应、空响应和 credential isolation 测试。
