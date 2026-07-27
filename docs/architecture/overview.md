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
  provider implementations、pool identity、expanded routes、catalog 和 cache。
- `serveRequest`：一次 schedule/failover pass 的稳定输入。
- `targetPlan`：普通 route、Fusion、Shadow 共用的 provider/protocol/model/body/
  base URL/path 准备。
- `targetAttempt`：一个已解析上游目标的完整执行契约。
- `attemptExecutor`：只通过 `attemptState` 修改健康、参数学习和 wire state；
  其余 HTTP、metrics、tokens、request log、Responses state、events 显式注入。

同协议保持字节透传。跨协议方向只在 `conversion_registry.go` 注册；每个
client→backend pair 必须同时提供 request、反向 response、反向 SSE codec。
专用 pair codec 保留 hosted tools、reasoning 方言和 namespace 等协议特有语义。

## 编排与异步分支

- Fusion 全程持有主请求的 `runtimeSnapshot`。panel/judge 共用内部非流式策略，
  synthesizer 通过正常 `targetAttempt → attemptExecutor` 返回客户端。
- Shadow 在主请求 commit 时同时捕获 `runtimeSnapshot` 和 `shadowRuntime`，
  goroutine 内不得重新读取 reload-owned 状态。
- Cache、request log、usage scanner 位于响应转换外层，只观察客户端协议字节。

## 状态与锁

- `Proxy.mu`：只保护 reload-owned 对象交换；请求流式期间不持有。
- `healthMu`：health、sticky、pin、model lock、paramBlock、spread counter。
- quota、wire caps、metrics/tokens/agents、cache、pricing 各有独立 owner/leaf lock。
- 跨域锁顺序仅允许 `healthMu → quotaMu`。

Web/API 通过 `proxyReadView` 读取 detached snapshot，不直接获取 Proxy 锁或内部
map。写操作调用明确的 reload、pin、health reset、quota refresh 等应用命令。

## 生命周期

`proxyLifecycle` 是 Proxy 级后台任务的唯一 owner：

- daemon 只调用 `startRuntimeServices` 与 `Proxy.Close`；
- reload catalog refresh 必须通过 lifecycle gate 接纳；
- Close 拒绝新任务、停止 loop、drain request log、等待任务，再完成 stats、
  Responses state 和 quota final flush。

quota tracker 与 Responses state store 各自拥有内部 debounce/worker，但由
`Proxy.Close` 按统一顺序停止。

## 依赖规则

允许：

```text
transport → orchestration → target plan → target executor → provider
Fusion/Shadow ────────────────┘
Web → proxyReadView
lifecycle → background components
conversion entrypoints → conversion registry → pair codecs
```

禁止：

- Web handler 直接访问 Proxy 锁、config/provider/health map；
- Fusion/Shadow 复制 provider lookup、协议选择、转换或 URL/path 逻辑；
- request、response、SSE 各自维护协议方向 switch；
- attemptExecutor 持有完整 `*Proxy`；
- daemon/reload 绕过 `proxyLifecycle` 启动 Proxy 级 goroutine；
- 将 config generation 内的 map 原地修改。

## 专题文档

- 路由与失败：`routing-and-failure.md`
- 运行态与生命周期：`runtime-state.md`
- Provider pools：`provider-pools.md`
- 请求感知路由：`request-routing.md`
- 协议转换：`protocol-conversion.md`
- Fusion、Shadow、Cache 与观测：`fusion-shadow-cache.md`
- Web/API：`../web-api.md`

架构边界的静态回归位于 `architecture_contract_test.go`；行为与并发验证仍按
`docs/engineering/testing.md` 执行。
