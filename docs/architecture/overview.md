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

同协议保持字节透传。`internal/protocol` 是无仓库内依赖的叶子包，统一拥有协议
identity、六组 pairwise codec、request/response/SSE registry、SSE↔JSON 模式
桥接、跨协议图片约束，以及 Responses `previous_response_id` 的有界状态。
每个 client→backend pair 必须同时提供 request、反向 response、反向 SSE codec；
专用 pair codec 保留 hosted tools、reasoning 方言和 namespace 等协议特有语义。
Provider 方言和目标视觉能力由 `targetPlan` 解析为窄 request options 后注入；
具体 `http.ResponseWriter` 错误 envelope 仍由 transport 层负责。

`internal/config` 统一拥有配置类型、YAML 加载、默认值、校验和生效值
accessor；它不是无仓库依赖叶子，只允许依赖其校验/默认值实际需要的
`internal/pricing` 与 `internal/protocol`。根包 `config_compat.go` 只保留类型别名
和 `LoadConfig` / `LoadConfigFromBytes` 兼容 wrapper，composition root 与现有
调用方不得在根包重新建立第二套配置事实或恢复 `config.go`。

`internal/catalog` 是无仓库内依赖的 models.dev 元数据源叶子包，拥有 slim
projection、canonical-owner 去重、HTTP/ETag/TTL 刷新和原子磁盘缓存。根包只把
HOME、`MP_MODELSDEV_URL` 与 Config 的 provider/route 名单适配成 catalog 输入；
请求感知路由继续消费一次性捕获在 `runtimeSnapshot` 中的不可变 catalog 指针。

`internal/accounts` 是无仓库内依赖的 API-key 账号存储叶子包，拥有 credential
tuple、稳定账号 ID、plural/legacy 读取优先级、原子保存和跨进程锁。根
`accounts_adapter.go` 只适配 HOME 并为尚在组合层的登录、Web、Provider 构建保留
窄兼容入口；`buildProviders` 以一次 `LoadSnapshot` 同时取得 pool 与来源，并在
同一 build result 中派生 providers、pool identity 和 implicit-route eligibility，
避免二次文件探测改变同一 runtime generation 的 authority 决策。网络验证、
交互、reload、运行时虚拟化和健康选择不进入存储包。

## 编排与异步分支

- Fusion 全程持有主请求的 `runtimeSnapshot`。panel/judge 共用内部非流式策略，
  synthesizer 通过正常 `targetAttempt → attemptExecutor` 返回客户端。
- Shadow 由 `serveOnce` 在主请求 commit 后根据 `attemptCommit` 接纳和派发，同时
  捕获 `runtimeSnapshot` 与 `shadowRuntime`；executor 和 Fusion synthesizer
  均不得启动 Shadow，goroutine 内不得重新读取 reload-owned 状态。
- Cache、request log、usage scanner 位于响应转换外层，只观察客户端协议字节。
- Analytics 的价格目录、条件抓取、原子缓存、override 解析与成本公式由
  `internal/pricing` 这一无主包依赖的叶子包拥有；价格端点优先级由
  `internal/config` 解析，根 `pricing.go` 只适配应用 HOME 路径，
  `Proxy.pricingSnapshot` 保留配置快照和并发刷新锁。

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
catalog adapter / routing → internal/catalog
accounts adapter / login / provider builder → internal/accounts
target plan / target executor → internal/protocol
composition root → internal/config → internal/pricing / internal/protocol
```

禁止：

- `webServer` 持有 `*Proxy`，或 Web handler 绕过 capability 直接访问 Proxy；
- `web.go` 用裸 `go` 启动绕过 `webTaskOwner` 的后台任务；
- Fusion/Shadow 复制 provider lookup、协议选择、转换或 URL/path 逻辑；
- request、response、SSE 各自维护协议方向 switch；
- attemptExecutor 持有完整 `*Proxy`；
- 普通 route/Fusion 绕过 `newTargetAttempt` 直接拼装执行器输入；
- daemon/reload 绕过 `proxyLifecycle` 启动 Proxy 级 goroutine；
- `internal/config` import `internal/pricing`、`internal/protocol` 以外的
  `model-proxy/*` 包，或根 `config_compat.go` 承载类型别名与加载 wrapper
  之外的配置实现；
- `internal/catalog` 反向依赖 Config、Proxy、Provider、Web/CLI 或任意
  `model-proxy/*` 包；
- `internal/accounts` 读取 HOME、反向依赖 Config、Proxy、Provider、Web/CLI，
  或承担网络验证、Provider 构建、reload 与路由选择；
- `internal/pricing` 反向依赖 `main` 的 YAML 配置、Proxy、Web 或通用 helper；
- `internal/protocol` import 任意 `model-proxy/*`，或反向读取 Config、Provider、
  Proxy、Web/CLI；Fusion 直接 import protocol 绕过 `targetPlan`；
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
`webServer` 字段类型、Web capability 方法 allowlist、禁止的 `w.p` selector、
账号测活的单次 runtime snapshot、internal 叶子包 import（含 accounts/catalog）、
`internal/config` 依赖 allowlist、根配置兼容 facade 以及 Fusion 不绕过
`targetPlan`，不是字符串扫描）；行为与并发验证仍按
`docs/engineering/testing.md` 执行。
