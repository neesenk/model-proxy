# 架构总览

## 定位

model-proxy 是单进程模块化单体。Provider、调度、协议转换、运行态、观测和
Web/API 保持同一部署单元，但通过显式数据结构和窄端口隔离；当前复杂度不需要
拆成微服务。

`Proxy` 是 composition root：负责持有组件引用和装配，不是允许任意模块访问的
共享状态袋。新增行为应进入下述模块边界，不能继续给长参数链或 Web handler
增加内部字段访问。

## 请求执行链

```text
HTTP handler
  → runtimeSnapshot
  → serveRequest
  → schedule / failover
  → targetPlan
  → targetAttempt
  → attemptExecutor
  → provider.Provider
```

- `runtimeSnapshot`：一次请求只捕获一个 reload generation 的 config、
  provider implementations、pool identity、expanded routes、catalog、cache 和
  Shadow dispatch runtime。
- `serveRequest`：一次 schedule/failover pass 的稳定输入。
- `targetPlan`：普通 route、Fusion、Shadow 共用的 provider/protocol/model/body/
  base URL/path 准备。
- `targetAttempt`：一个已解析上游目标的完整执行契约，只由
  `runtime + plan + exchange + scope + policy` 五组字段组成；`newTargetAttempt`
  是普通 route 与 Fusion synthesizer 的唯一构造入口。
- `attemptExecutor`：只通过 `attemptState` 修改健康、参数学习和 wire state；
  其余 HTTP、metrics、tokens、request log、Responses state、events 显式注入；
  commit 后只返回最小 `attemptCommit`，不持有生命周期或 Shadow 调度能力。

`runtimeSnapshot` 与 `targetPlan` 是执行器内 reload-owned/config/provider/protocol
事实的唯一来源；exchange 只承载 HTTP request/writer/body，scope 只承载本次请求
身份与 Responses 上下文，policy 只承载 force/last-target/context-retry。
`newTargetAttempt` 不负责 model rewrite、Responses history expansion 或协议转换，
这些准备语义仍由普通/Fusion 各自编排后再进入执行器。

同协议保持字节透传。跨协议方向只在 `conversion_registry.go` 注册；每个
client→backend pair 必须同时提供 request、反向 response、反向 SSE codec。
专用 pair codec 保留 hosted tools、reasoning 方言和 namespace 等协议特有语义。

## 编排与异步分支

- Fusion 全程持有主请求的 `runtimeSnapshot`。panel/judge 共用内部非流式策略，
  synthesizer 通过正常 `targetAttempt → attemptExecutor` 返回客户端。
- Shadow 由 `serveOnce` 在主请求 commit 后根据 `attemptCommit` 接纳和派发，同时
  捕获 `runtimeSnapshot` 与 `shadowRuntime`；executor 和 Fusion synthesizer
  均不得启动 Shadow，goroutine 内不得重新读取 reload-owned 状态。
- Cache、request log、usage scanner 位于响应转换外层，只观察客户端协议字节。
- Analytics 的价格目录、条件抓取、原子缓存、override 解析与成本公式由
  `internal/pricing` 这一无主包依赖的叶子包拥有；`pricing.go` 只适配环境变量与
  应用 HOME 路径，`Proxy.pricingSnapshot` 保留配置快照和并发刷新锁。

## 状态与锁

- `Proxy.mu`：只保护 reload-owned 对象交换；请求流式期间不持有。
- `healthMu`：health、sticky、pin、model lock、paramBlock、spread counter。
- quota、wire caps、metrics/tokens/agents、cache、pricing 各有独立 owner/leaf lock。
- 跨域锁顺序仅允许 `healthMu → quotaMu`。

Web/API 的 `webServer` 不持有 `*Proxy`，只持有 composition root 在构造时创建的
具体 capability：`proxyReadView` 返回 detached snapshot，`proxyAdminCommands`
执行 reload、pin、health reset、quota refresh 和账号测活等应用命令。账号测活
只捕获一次 `runtimeSnapshot`，因此配置、路由与 provider implementation 始终来自
同一 reload generation；网络 I/O 发生在快照完成、锁已释放之后。

## 生命周期

`proxyLifecycle` 是 Proxy 级后台任务的唯一 owner：

- daemon 只调用 `startRuntimeServices` 与 `Proxy.Close`；
- reload catalog refresh 必须通过 lifecycle gate 接纳；
- Close 拒绝新任务，先等待会产生日志的有限任务（Shadow），再 drain request
  log；随后等待 loop/refresh，最后完成 stats、Responses state 和 quota final
  flush。这样 Close 返回后不会有 Shadow 向已关闭 logger 补写。

quota tracker 与 Responses state store 各自拥有内部 debounce/worker，但由
`Proxy.Close` 按统一顺序停止。

Web 后台任务由独立的 `webTaskOwner` 管理，不混入 `proxyLifecycle`：login-session
GC 与 AQP/Codex 异步登录轮询都必须经同一 admission gate 启动。HTTP transport
关闭时先拒绝新 Web task、取消轮询并等待已接纳任务；若任务已进入凭据落盘
commit，则允许 save + reload 完成后再关闭 Proxy。

## 依赖规则

允许：

```text
transport → orchestration → target plan → target executor → provider
Fusion/Shadow ────────────────┘
Web → proxyReadView / proxyAdminCommands
lifecycle → background components
conversion entrypoints → conversion registry → pair codecs
analytics adapter → internal/pricing
```

禁止：

- `webServer` 持有 `*Proxy`，或 Web handler 绕过 capability 直接访问 Proxy；
- `web.go` 用裸 `go` 启动绕过 `webTaskOwner` 的后台任务；
- Fusion/Shadow 复制 provider lookup、协议选择、转换或 URL/path 逻辑；
- request、response、SSE 各自维护协议方向 switch；
- attemptExecutor 持有完整 `*Proxy`；
- 普通 route/Fusion 绕过 `newTargetAttempt` 直接拼装执行器输入；
- daemon/reload 绕过 `proxyLifecycle` 启动 Proxy 级 goroutine；
- `internal/pricing` 反向依赖 `main` 的 YAML 配置、Proxy、Web 或通用 helper；
- 将 config generation 内的 map 原地修改。

## 专题文档

- 路由与失败：`routing-and-failure.md`
- 运行态与生命周期：`runtime-state.md`
- Provider pools：`provider-pools.md`
- 请求感知路由：`request-routing.md`
- 协议转换：`protocol-conversion.md`
- Fusion、Shadow、Cache 与观测：`fusion-shadow-cache.md`
- Web/API：`../web-api.md`

架构边界的静态回归位于 `architecture_contract_test.go`（基于 go/ast 检查
`webServer` 字段类型、Web capability 方法 allowlist、禁止的 `w.p` selector 和
账号测活的单次 runtime snapshot，不是字符串扫描）；行为与并发验证仍按
`docs/engineering/testing.md` 执行。
