# 路由、失败分类与恢复

## 适用范围

修改 `proxy.go`、`failclass.go`、`resolve.go`、`health_test.go`、`model_lock_test.go` 或 cooldown/retry 行为时必读。

## 实现入口

- `Proxy.forward` / `serveOnce` / `tryTarget`
- `providerHealth`、`modelLocks`、`paramBlock`
- `classify429`、`parseResetHint`、`isModelDenied`、`parseUnsupportedParam`
- `cooldownState`、`hasRecoveredUntried`

## 基本路由语义

默认“客户端协议 = 上游协议”，同协议请求和响应字节级透传。目标声明 `protocol:` 时才进行协议转换。

运行态健康信息位于 `Proxy.health`，由 `healthMu` 保护，与 reload 使用的 `mu` 分离：

- `schedule` 跳过熔断、限频和模型锁定目标。
- timeout、连接错误和 5xx 计入 provider 熔断。
- 429 进入 provider 限频冷却，不计熔断。
- 404、model-denied、空 200 只锁 `(provider, model)`。
- 半开状态使用 `halfOpenInFlight` 单飞。
- 普通 4xx commit 必须释放半开槽，但不得清除失败历史。
- runtime provider implementation 缺失时必须 fail-closed，禁止发送无认证请求。

## 429 分类

429 分为：

| kind | 含义 | 无明确 reset 时的默认值 |
|---|---|---|
| `transient` | 请求速率或短期 token rate | `rate_limit_backoff`，默认 60s |
| `quota` | 账号余额、套餐或总配额耗尽 | `quota_cooldown`，默认 1h |
| `daily` | 当日配额耗尽 | 本地时间下一日 00:00 |

冷却时间采用顺序：

1. body reset hint；
2. `Retry-After`；
3. 分类默认值。

body hint 支持 `retry after N s/m/h`、`reset after 2h5m`、`Resets in 164h` 和 reset/retry 关键词邻近的 RFC3339。上限 7 天；duration 在乘法前必须 clamp，防止溢出。

同一 provider 收到多个 429 时，只采用更晚的 horizon；`rateLimitKind` 必须跟随胜出的 horizon，短冷却不能覆盖既有长冷却的 kind。

## 模型级失败

`modelLocks` 的 key 是 `(provider, model)`，避免一个模型的问题污染同账号的其他模型。

会记录模型锁的情况：

- 任意 404；
- 400/403 且 `isModelDenied` 保守命中；
- `Content-Length: 0` 的成功响应；
- 流式成功响应在 clean EOF、零字节且客户端仍连接时。

客户端断开、写错误、上游 read error 都不得学习为空响应。最后一个 target 的 404/400 仍原样 commit 给客户端，但模型锁继续记录，供后续请求跳过。

## Unsupported parameter 学习

`paramBlock` 按 `(provider, model)` 隔离：

1. 400 body 命中已知 unsupported/unknown parameter 形式；
2. 提取违规顶层字段；
3. 当次剥离并重试一次；
4. 后续请求在 `RewriteRequest` 后预防性剥离。

以下字段永不自动剥离：

`model`、`messages`、`input`、`prompt`、`system`、`stream`、`tools`、`tool_choice`、`response_format`。

剥参重试不占用 401 refresh 的 retry slot。匹配规则必须保守，不能根据裸的通用错误词锁模型或删除语义字段。

## 全冷却等待

`retry_wait` 默认 10s，`"0"` 关闭。`forward` 通过可重入的 `serveOnce` 执行完整调度和 failover：

- 仅当全部实际候选目标都处于冷却且最早恢复时间不超过预算时等待；
- 最多整体重试两次；
- 已恢复但本轮未尝试的目标应零等待重新调度；
- `x-mp-force-provider` 和已断开的客户端不等待；
- 终局分类应基于跨轮实际失败类别：纯限频返回 429 和 `Retry-After`，出现硬失败返回 502。

### 已知缺口

当前等待层仍可能回看原始 route targets。请求感知改道或 context overflow 替换目标后，等待、attempted 集合和 Retry-After 尚未完全基于实际 effective targets。

修复时应由 `serveResult` 返回实际目标和失败 horizon。

`hasRecoveredUntried` 当前还要求“至少仍有一个目标在冷却”。全部未尝试目标在终局检查前同时恢复时可能漏掉零等待重排。

该条件应只依赖是否存在 recovered-untried，并由 round budget 防止无限循环。

## 回归测试

- half-open 的成功、5xx、429、普通 4xx 生命周期。
- transient/quota/daily 及 body/header reset 优先级。
- 同 provider 不同 model 的锁和 paramBlock 隔离。
- empty stream 的 EOF、client cancel、upstream read error。
- cooldown 全部恢复、部分恢复、跨轮纯 429、混合硬失败。
- request-aware/context retry 后的实际目标冷却判定。
