# 路由、失败分类与恢复

## 适用范围

修改 `internal/forward/forward.go`、`internal/forward/plan.go`、
`internal/targetexec/executor.go`、`internal/targetexec/rate_limit.go`、
`internal/routing/retry.go`、`internal/app/proxy_read_endpoints.go`、
`internal/app/target_pipeline.go`、`internal/app/wirecap.go`、
`internal/app/modelcaps.go`、`internal/probe/`、`internal/app/health_test.go`、
`internal/app/model_lock_test.go` 或 cooldown/retry 行为时必读。

## 实现入口

- `Proxy.forward`（app 薄 shim）→ `internal/forward.Serve` / `serveOnce` / `targetexec.Executor.Execute`
- `internal/app/proxy_read_endpoints.go`：应用执行层到 runtime health/cooldown/param/rate-limit
  状态端口的适配
- `internal/forward/snapshot.go` / `internal/app/dispatch_context.go`：`Snapshot`
  （app 别名 `RuntimeSnapshot`，单次捕获红线在 `Proxy.SnapshotRuntime`）；
  `internal/forward/forward.go` 的 `serveRequest` 与 `internal/forward/attempt.go` 的
  `targetexec.Attempt` 唯一 assembly adapter
- `internal/targetexec.Plan`：已解析目标的不可变 model/body/base URL/path wire
  preparation；`internal/forward/plan.go` 的 `planTarget` 只做 snapshot-owned facts 的投影
- `internal/targetexec/executor.go`：完整单目标 I/O、401/参数/图片 retry、
  failure classification、response conversion/capture/cache pipeline
- `internal/app/target_pipeline.go`：captured generation/scheduling 的 `targetexec.State`
  与 metrics/tokens/request-log/events 的 `targetexec.Effects` 应用适配
- `internal/routing.DecideFailure`：跨 pass 失败类别、短 cooldown wait、
  recovered-untried 与 429/502 终局的纯策略
- `internal/runtime.Manager`：health、model lock、paramBlock、sticky、pin、
  spread、quota 与 schedule
- `targetexec.ParseRateLimit`、`targetexec.IsModelDenied`、
  `targetexec.ParseUnsupportedParam`
- `cooldownState`、`hasRecoveredUntried`

## 基本路由语义

一次客户端请求在 `forward` 开始时只捕获一次 `RuntimeSnapshot`。该快照包含
同一 config generation 的 config、provider implementations、pool identity、
expanded routes、models.dev catalog 与 response cache；reload 只交换新对象，
不得原地修改快照持有的 map。`serveRequest` 是一次完整 schedule/failover pass
的输入，`internal/targetexec.Attempt` 是单次 resolved target 的强类型执行契约。
普通 route 与 Fusion synthesizer 均通过该契约进入 `targetexec.Executor.Execute`，
新增横切能力不得继续扩张 positional 参数列表或以 `any` 夹带 root owner。

`targetexec.Attempt` 固定为五组：`targetexec.Runtime`
（captured scheduling/generation/cache）、`targetexec.Plan`、
`targetexec.Exchange`（request/writer/body）、`targetexec.Scope`（调用模型、
agent、cache key/log/Responses 上下文）和 `targetexec.Policy`（force、last
target、context retry）。两条调用链只能经
`newTargetAttempt → targetexec.NewAttempt` 构造；factory 仅投影/装配，不做
model rewrite、state expansion 或协议转换。普通/Fusion/Shadow 不得复制
endpoint、model rewrite 或 conversion-option 逻辑。执行器必须从 typed
Runtime 与 Plan 读取事实，禁止在 scope/log 中复制第二份 generation、嵌入完整
`RuntimeSnapshot`、使用 `any` 或重新查 Proxy。

`targetexec.Executor` 只允许依赖 generation-frozen `targetexec.State` 暴露的
健康、参数学习和 wire 能力，以及 typed `Effects/Responses` 端口；不得 import
或持有完整 `*Proxy`，也不得访问调度、reload、Web、lifecycle 或 Shadow。
`internal/app/target_pipeline.go` 从 `Attempt.Runtime()` 绑定 generation/scheduling，再把
根 metrics/token/request-log/events 映射为语义 effect，不得包含 `client.Do`、
转换、retry 或 failover pipeline；`internal/forward/plan.go` 只组装这些端口。
`internal/forward/forward.go` 只负责编排和调用。
executor commit 后只返回含实际上游请求体的最小 `targetexec.Commit`；Shadow
sampling、semaphore、lifecycle admission 与 dispatch 由 `serveOnce` 在
executor 外完成，Fusion synthesizer 丢弃该 commit 元数据，禁止递归触发 Shadow。

默认“客户端协议 = 上游协议”，同协议请求和响应字节级透传。目标声明 `protocol:` 时才进行协议转换。

后端协议解析优先级（`(*Proxy).resolvedBackendProto`，`internal/app/wirecap.go`）：显式 `protocol:` > `ProtocolHint`（codex→responses）> **模型级协议矩阵**（`runtimewire.ResolveModel`）> **provider 级 wire 探测 verdict**（`runtimewire.Resolve`）> 客户端协议透传。空 `target.Model`（透传 target）跳过模型级查询，直接进入 provider 级。

provider 级 wire verdict 由 `probeAllWireCaps` 在 boot/reload 时异步探测并按 parent provider 名缓存。探测只有 openai base 上的两条腿：`/chat/completions` 与 `/responses` 各一个请求（经 `probe.Do` 构造，配方见 overview.md 的 `internal/probe` 条目）。**anthropic 支持是 config 声明、永不探测**：provider 配置 `anthropic_base_url` 即支持（声明 = 事实），未配置即定义上不支持；旧的「在无 anthropic_base_url 时对 openai base 探 `/v1/messages`、网关接受 anthropic 则字节透传」分支已删除——探测绝不在 openai base 上伪造 anthropic 请求体。分类：404→no，其余 2xx–4xx（含 3xx 与 400/401/403/429）→yes，超时/连接错误/5xx→unknown。provider 级 verdict 生效的决策矩阵：

| 客户端协议 | 条件 | 后端协议 |
|---|---|---|
| openai(chat) | — | 透传 |
| responses | verdict.responses ≠ no | 透传 |
| responses | verdict.responses == no | 转 chat |
| anthropic | provider 有 anthropic_base_url | 透传（专用 base） |
| anthropic | verdict.responses == yes | 转 responses（reasoning 保留） |
| anthropic | verdict.responses == no | 转 chat |
| anthropic | verdict unknown | 透传（维持现状，探测窗口期不改变行为） |

**模型级协议矩阵**：boot/reload 的探测 pass（`(*Proxy).probeAllModelCaps`，`internal/app/modelcaps.go`，与 `probeAllWireCaps` 同由 `startWireCapProbe` 派发，并发上限 4）对每个 provider 的 model 集合（config models ∪ 显式 route target ∪ derived target）经 `probe.ProbeModelProtocols` 并发探三条腿：chat → POST {openai_base}/chat/completions，responses → POST {openai_base}/responses，anthropic → POST {anthropic_base}/v1/messages；腿的 base 未配置则不探（`LegResult.Probed=false`）。**每条腿的 body 都注入一个最小 function tool 声明（`attachProbeTool`，按腿成形：chat 的 `{type:function,function:{...}}` / anthropic 的 `input_schema` / responses 的扁平 `{type:function,...}`），verdict 因此是"agent 级可用性"而非裸 ping**——真实案例：aqp 的 gpt-5.6 系列裸 ping 三协议皆通，但带工具时 chat/anthropic 被上游拒绝（"Function tools ... are not supported for gpt-5.6-luna in /v1/chat/completions"），只有 responses 可用。impl 的 `ProbeRequest` 路径与腿的 canonical 路径一致时（如 codex 的 /responses 方言），impl 的 body 优先于通用最小 body（工具声明仍会并入）；max_tokens→max_completion_tokens 改名重试按腿适用。ProtocolHint 覆盖的 provider（codex）不探，直接合成 `{responses:yes, chat:no, anthropic:no}`。分类（`runtimewire.ClassifyModelStatus`）与 provider 级不同：未探腿→no（定义上不支持）；2xx→yes；404→no；400 带模型拒绝措辞（"model not found"/"does not exist"/"unsupported model"/"invalid model"/"model is not supported"/"unknown model"/"no such model"/"not supported with this model"/"not supported for"（某特性对该模型在该路径不支持，如上面的 gpt-5.6 工具拒绝），大小写不敏感）→no，其余 400→yes（形状争议反证该路由服务此模型，刻意粗粒度）；401/403/429→unknown（auth/quota 回答证明路由存在，但说明不了该模型）；5xx/3xx/网络→unknown（与 provider 级 3xx→yes 不同）。unknown 腿在下一次探测 pass 重探。模型级矩阵的生效规则（`runtimewire.ResolveModel`，先于 provider 级）：

| 客户端协议 | 条件 | 后端协议 |
|---|---|---|
| anthropic | 有 anthropic_base_url 且 model.anthropic == yes | 透传 |
| anthropic | model.responses == yes | 转 responses（含「有 anthropic base 但该模型不在其上」） |
| anthropic | 矩阵全部已结论且 responses ≠ yes | 转 chat |
| responses | model.responses == yes | 透传 |
| responses | model.responses == no | 转 chat |
| openai(chat) | — | 透传 |
| 任意 | 模型无条目或相关腿未结论 | 回落 provider 级矩阵 |

**运行时 404 纠正（模型粒度）**：因 verdict 转到 `/responses` 的请求若上游 404，说明 verdict 有误而非模型缺失——`noteWireResponsesMiss(parent, model)` 经 `targetexec` State/HealthGate 传导（executor 与 Fusion leg 共用）：该 model 在模型级矩阵有条目时**只翻转模型级** responses verdict 并持久化 model_caps.json（provider 级不动）；无模型条目（透传 target）才翻转 provider 级 verdict（legacy 路径）。两种情况都**跳过 recordModelFailure**（这是我们的协议选择失误，不是模型的失败），按正常失败走 failover；后续请求自动转 chat。非 verdict 驱动的 404 行为不变（模型锁）。Fusion leg 共享同一纠正（`planTarget` 已算出 `viaResponsesVerdict`，见 fusion-shadow-cache.md）。

provider 级 yes 结论永久信任（错误 yes 由上述运行时路径纠正）；**provider 级 no 结论有 24h TTL**（`wireCapNegativeTTL`），到期后下一次 boot/reload 探测 pass 重探——一次性错误 no（上游发布中临时 404 等）不会永久降级该 provider。模型级矩阵**无 TTL**：失效只由 config fingerprint（`providerbuild.ProtocolConfigFingerprint`）触发，fingerprint 匹配即复用结论（持久化格式与恢复门控见 `runtime-state.md`）。

**判定的不对称兜底**：provider 级 `classifyWireStatus` 把 404 以外的全部 4xx（含 401/403/405/429）一律判 yes，而运行时纠正只认 404。对 `/responses` 需要不同鉴权、或对未实现路径返 405 的网关会产生 wrong-yes 且不会被自动翻转——此时只能显式声明 `protocol:` 兜底，绕过 verdict。模型级的 400 措辞嗅探与 401/403/429→unknown 是有意的口径差异，见 `docs/decisions/intentional-behaviors.md`。

运行态健康、模型锁和调度状态统一位于 `internal/runtime.Manager`。Manager 以
单锁和 generation gate 保证健康 mutation、availability 过滤、schedule、
cooldown 与 detached dashboard/persistence snapshot 看到一致状态；reload 只按
`Proxy.mu → runtime.Manager` 进入它：

- `schedule` 跳过熔断、限频和模型锁定目标。
- timeout、连接错误、5xx 和 refresh 后仍失败的 401 计入 provider 熔断。
- **客户端取消不计熔断**：调用方断开（`exchange.Request.Context()` 取消，
  含调用方自身 deadline）不是上游判决——不记 `RecordFailure`、不计
  `evFailures`，executor 返回 `OutcomeClientGone`，`serveOnce` 立即终止
  failover 链（不再把剩余 target 在死掉的 ctx 上各烧一次）并以 499 结束
  live 事件。执行器自身 upstream timeout 触发的
  `DeadlineExceeded`（父 ctx 仍存活）照常计入熔断。
- exhausted 401 的观测只记 failover，不记 `evFailures`；熔断状态与请求 metric
  是不同语义，普通执行器和 Fusion leg 必须保持一致。
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

body hint 支持 `retry after N s/m/h/d`、`reset after 2h5m`、`Resets in 164h`（`days?|d|hours?|minutes?|seconds?` 等单位）和 reset/retry 关键词邻近的 RFC3339。上限 7 天；duration 在乘法前必须 clamp，防止溢出。

分类与 horizon 只由 `internal/targetexec.ParseRateLimit` 计算；普通
`targetexec.Executor` 与 Fusion leg 必须调用同一入口，再把预计算的
`RateLimitDecision` 交给 runtime adapter。根包不得恢复 `classify429`、
`parseResetHint`、`hintDuration` 或 `parseRateLimit` 副本。

同一 provider 收到多个 429 时，只采用更晚的 horizon；`rateLimitKind` 必须跟随胜出的 horizon，短冷却不能覆盖既有长冷却的 kind。

## 模型级失败

`modelLocks` 的 key 是 `(provider, model)`，避免一个模型的问题污染同账号的其他模型。

会记录模型锁的情况：

- 任意 404；
- 400/403 且 `targetexec.IsModelDenied` 保守命中；
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
- 已恢复但本轮未尝试的目标应零等待重新调度（配额已知耗尽、被 skip 的目标**不算**已恢复）；
- `x-mp-force-provider` 和已断开的客户端不等待；
- 终局分类应基于跨轮实际失败类别：纯限频返回 429 和 `Retry-After`，出现硬失败返回 502。配额已知耗尽（新鲜 plan 快照 ultimate RemainingPct==0）按限频类参与分类——全耗尽 route 终局 429，`Retry-After` 取窗口 reset 与快照失鲜边界的较早者（详见 `runtime-state.md` 的 Sticky/availability 一节）。

### 等待层目标来源

等待层基于 `serveResult.effectiveTargets` —— `serveOnce` 实际考虑的目标集合（schedule 过滤冷却目标、request-aware 改道、context overflow 替换之后），而非原始 route targets。`forward` 用它计算 cooldown / Retry-After / TOCTOU；effective 为空（例如 schedule 因全部冷却而返回空）时回退到原始 targets。这避免「健康但被过滤的兄弟目标」让 `cooldownState` 误判仍有可用目标而跳过等待。

`hasRecoveredUntried` 只依赖「是否存在 available 但本 pass 未尝试的目标」：冷却中途恢复（TOCTOU）或全部目标同时恢复时给予零等待重排，由 forward 的 round budget（≤2）防止无限循环。不再要求「至少仍有一个目标在冷却」。

## 回归测试

- half-open 的成功、5xx、429、普通 4xx 生命周期。
- transient/quota/daily 及 body/header reset 优先级。
- 普通 target 与 Fusion leg 的 429 kind/horizon、metric、quota refresh 一致。
- 同 provider 不同 model 的锁和 paramBlock 隔离。
- empty stream 的 EOF、client cancel、upstream read error。
- cooldown 全部恢复、部分恢复、跨轮纯 429、混合硬失败。
- request-aware/context retry 后的实际目标冷却判定。
