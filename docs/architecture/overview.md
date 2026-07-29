# 架构总览

## 定位

model-proxy 是单进程模块化单体。Provider、调度、协议转换、运行态、观测和
Web/API 保持同一部署单元，但通过显式数据结构和窄端口隔离；当前复杂度不需要
拆成微服务。

`Proxy` 是 composition root：负责持有组件引用和装配，不是允许任意模块访问的
共享状态袋。新增行为应进入下述模块边界，不能继续给长参数链或 Web handler
增加内部字段访问。

根包的物理布局按职责逐步收敛：`proxy.go` 只保留 composition-root owner；
`proxy_forward.go` 集中主请求的 forward/serve 调度、failover 与 commit 编排；
`proxy_shadow.go` 集中 Shadow post-commit dispatch、可热重载 runtime、sampling
与异步执行；
`provider_build.go` 以一次账号 snapshot 同时构建 provider、pool identity 和
implicit-route eligibility；
`proxy_constructor.go` 负责组件装配、状态恢复、Close 委派和 stats reset；
`proxy_snapshot.go` 集中 config/provider/catalog/pricing 读取与 generation 一致的
持久化快照；
`proxy_routes_compile.go` 只编译 explicit/implicit route 与 pool fan-out；
`proxy_reload.go` 执行 generation 原子交换及交换后的持久化/刷新编排；
`proxy_http.go` 只承载主代理 HTTP 路由、models 响应和早期终态事件；
`proxy_schedule_view.go` 从单个 detached dashboard snapshot 投影调度状态；
`proxy_schedule_adapter.go` 只把 config route/pin 输入映射到 runtime Manager；
`proxy_runtime_identity.go` 集中跨执行领域共享的 generation 与 pool provider
identity 投影；
`proxy_health_adapter.go` 只把根层 health/cooldown/param/rate-limit 输入映射到
runtime Manager，并保留 429 后 quota refresh 编排；
`proxy_transport.go` 集中 SSE/HTTP 流识别、复制和 ResponseWriter primitive；
`json_model_body.go` 只处理顶层 model 的提取与改写。移动到这些文件不改变同包
调用边界，也不允许 transport helper 反向持有 `Proxy`。

## 请求执行链

```text
HTTP handler
  → runtimeSnapshot
  → serveRequest
  → schedule / failover
  → internal/targetexec.Plan
  → internal/targetexec.Attempt
  → attemptExecutor
  → provider.Provider
```

- `runtimeSnapshot`：一次请求只捕获一个 reload generation 的 config、
  provider implementations、pool identity、expanded routes、catalog、cache 和
  Shadow dispatch runtime。
- `serveRequest`：一次 schedule/failover pass 的稳定输入。
- `internal/targetexec.Plan`：根 `planTarget` 只解析同一 `runtimeSnapshot` 中的
  provider implementation、backend protocol、wire verdict 与视觉能力；不可变
  的 model/body/base URL/path wire preparation 和 target/provider facts 由该
  Plan 拥有，普通 route、Fusion、Shadow 共用。
- `internal/targetexec.Attempt`：一个已解析上游目标的完整执行契约，只由
  `runtime + plan + exchange + scope + policy` 五组强类型字段组成；
  `newTargetAttempt → targetexec.NewAttempt` 是普通 route 与 Fusion
  synthesizer 的唯一构造入口。Runtime 只投影 captured scheduling、generation
  与 cache，不允许用 `any` 或完整 `runtimeSnapshot` 绕过边界。
- `attemptExecutor`：只通过 `attemptState` 修改健康、参数学习和 wire state；
  其余 HTTP、metrics、tokens、request log、Responses state、events 显式注入；
  commit 后只返回最小 `targetexec.Commit`，不持有生命周期或 Shadow 调度能力。

`runtimeSnapshot` 与 `internal/targetexec.Plan` 是执行器内
reload-owned/config/provider/protocol 事实的唯一来源；根 assembly 只把 snapshot
投影为 typed `targetexec.Runtime`，不得在普通/Fusion/Shadow 分支各自重算
endpoint 或 conversion options。exchange 只承载 HTTP request/writer/body，
scope 只承载本次请求身份与 Responses 上下文，policy 只承载
force/last-target/context-retry。
`newTargetAttempt` 不负责 model rewrite、Responses history expansion 或协议转换，
这些准备语义仍由普通/Fusion 各自编排后再进入执行器。

同协议保持字节透传。`internal/protocol` 是无仓库内依赖的叶子包，统一拥有协议
identity、六组 pairwise codec、request/response/SSE registry、SSE↔JSON 模式
桥接、跨协议图片约束，以及 Responses `previous_response_id` 的有界状态。
每个 client→backend pair 必须同时提供 request、反向 response、反向 SSE codec；
专用 pair codec 保留 hosted tools、reasoning 方言和 namespace 等协议特有语义。
Provider 方言和目标视觉能力由根 `planTarget` 解析后作为纯值注入
`internal/targetexec.Plan` 的窄 request options；
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
交互、reload、运行时虚拟化和健康选择不进入存储包。legacy singular 路径只由
`accounts.Store` 的读取兼容逻辑拥有，根 adapter 不再导出路径 wrapper。

`internal/observe/events` 是无仓库内依赖的实时事件叶子包，拥有事件 DTO、最近
200 条的有界 ring、非阻塞 fan-out、订阅快照和终态查询。根
`live_events.go` 只把该组件适配为 `/api/events` SSE 与 keepalive；业务发布点
显式依赖 `events.Hub`，不得重新访问 ring、subscriber map 或互斥锁。纯 ring/
订阅测试归内部包，HTTP、forward、Fusion 与 cache 事件契约仍在根包做集成测试。

`internal/observe/requestlog` 是无仓库内依赖的请求访问日志数据面叶子包，拥有
JSONL Record schema、body/header 截断与白名单、非阻塞队列、单 writer 的轮转/
retention/owner-only 权限、全文件流式 top-K 查询、list-safe Summary 和 Shadow
聚合。根 `request_log_adapter.go` 只把 `RequestLogConfig` 生效值及
`forwardLogCtx`/HTTP/RouteTarget 映射为纯值输入；capture 在转换器外层的位置、
`proxyLifecycle` 的 Shadow-before-drain 顺序、Web 参数、CLI replay policy 和
Fusion/Shadow eligibility 继续由应用层编排。列表与 Shadow 必须调用强制丢弃
body/header 的 metadata API，detail/replay 才能查询完整 Record。

`internal/cache` 是无仓库内依赖的精确响应缓存叶子包，拥有请求 key、
TTL/容量 store、客户端可见响应的 bounded recorder、header normalization 与
逐块 flush replay。根 `cache_adapter.go` 只把 `CacheConfig` accessor 的生效值
转换为 `cache.Options`；force/pin bypass、`<300` eligibility、转换器外层捕获
位置、cache-hit live event、reload generation swap 与 stats reset 仍由应用编排。
一次请求继续使用 `runtimeSnapshot.cache` 捕获的 Store，旧 generation 完成时不得
向 reload 后的新 Store 写入。

`internal/transport/bodycapture` 是无仓库内依赖的通用响应流捕获叶子包：
字节原样透传，只保存有界 prefix，同时统计完整长度和截断状态，并在首次 Close
执行一次回调。Responses state、request log 与 Shadow 共用这一 transport
primitive；各自的持久化和业务判断不得反向塞进通用 reader。

`internal/runtime/wirecap` 是无仓库内依赖的端点协议能力叶子包，拥有三态
verdict、JSON 持久化表示、协议选择纯策略，以及 parent provider keyed 的并发
Store。根 `wirecap.go` 只保留 HTTP probe、provider/config 适配、404 纠正触发和
异步持久化编排；Store 的 leaf lock 内不得回调应用代码。

`internal/runtime.Manager` 是 config generation 内可变路由状态的唯一 owner，
以单 mutex 统一 health、sticky、pin、model lock、paramBlock、spread、quota、
schedule 决策以及 persistence/Web detached snapshot。该包只依赖 `provider`
中的 quota 值类型；Config、HTTP、文件持久化和 Web DTO 映射仍由 composition
root 编排。`quotaTracker` 只执行轮询、refresh 去重和文件写入，不再拥有第二份
quota 状态。

Manager 的物理文件按职责拆分，但不形成多 owner：`manager.go` 只定义 owner、
单锁与 generation；`manager_types.go` 放边界 DTO；`manager_persist.go`、
`manager_quota.go`、`manager_routing_state.go`、`manager_health.go`、
`manager_schedule.go` 分别承载持久化投影、配额、路由选择状态、健康状态和调度。
新增可变 map 或锁必须仍回到 `Manager`，不得因文件拆分建立子状态仓库。

## 编排与异步分支

- Fusion 全程持有主请求的 `runtimeSnapshot`。panel/judge 共用内部非流式策略，
  synthesizer 通过正常 `targetexec.Attempt → attemptExecutor` 返回客户端。
- Shadow 由 `serveOnce` 在主请求 commit 后根据 `targetexec.Commit` 接纳和派发，同时
  捕获 `runtimeSnapshot` 与 `shadowRuntime`；executor 和 Fusion synthesizer
  均不得启动 Shadow，goroutine 内不得重新读取 reload-owned 状态。
- Cache、request log、usage scanner 位于响应转换外层，只观察客户端协议字节。
- Analytics 的价格目录、条件抓取、原子缓存、override 解析与成本公式由
  `internal/pricing` 这一无主包依赖的叶子包拥有；价格端点优先级由
  `internal/config` 解析，根 `pricing.go` 只适配应用 HOME 路径，
  `Proxy.pricingSnapshot` 保留配置快照和并发刷新锁。
- 统计热路径仍由根层 `metricsStore`、`tokenCounter`、`agentCounter` 各自拥有；
  根 `statsFlusher` 只做 cumulative snapshot → minute delta 的应用投影，
  SQLite schema、迁移、upsert、聚合查询、retention 与 legacy token import
  统一归无仓库内依赖的 `internal/observe/stats`。

## 状态与锁

- `Proxy.mu`：只保护 reload-owned 对象交换；请求流式期间不持有。
- `internal/runtime.Manager`：以单锁保护 generation、health、sticky、pin、
  model lock、paramBlock、spread、quota 和 schedule；所有返回给 persistence
  或 Web 的复合结果必须在该锁内原子复制并与 generation 一起返回。
  请求排序在同一次临界区内完成 quota projection 与 health/pin/sticky/spread
  选择；Web/调试调度从同一个 detached DashboardSnapshot 做只读 preview，
  禁止为 order 二次读取 Manager。
- `internal/runtime/wirecap.Store`、metrics/tokens/agents、stats flusher、
  cache、pricing 各有独立 owner/leaf lock；SQLite Store 与 wire-capability
  Store 均不拥有或回调应用运行时。
- 跨域锁顺序仅允许 `Proxy.mu → runtime.Manager`。Manager 持锁时不得回调
  Proxy、quota tracker 或任何外部 I/O。

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
  flush；stats 在 final flush 后关闭 SQLite Store。这样 Close 返回后不会有
  Shadow 向已关闭 logger 补写，也不会遗留数据库连接。

quota tracker 的 poll/refresh task 与 persist 编排、Responses state store 的
debounce/worker 都由 `Proxy.Close` 按统一顺序停止。

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
live-event publishers / SSE adapter → internal/observe/events
target executor / Fusion / Shadow / Web / CLI → internal/observe/requestlog
stats flusher / proxyReadView → internal/observe/stats
forward / target executor / cache adapter → internal/cache
target executor / Shadow → internal/transport/bodycapture
wire probe / target plan → internal/runtime/wirecap
schedule / health / resolver / quota adapter → internal/runtime
target plan / target executor → internal/protocol
composition root → internal/config → internal/pricing / internal/protocol
composition root → internal/targetexec → internal/protocol / provider
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
- `internal/observe/events` 反向依赖 Proxy、HTTP/Web、Config、Provider 或任意
  `model-proxy/*` 包；根 SSE adapter 重新声明事件类型或拥有 ring/fan-out 状态；
- `internal/observe/requestlog` 反向依赖 Config、Proxy、RouteTarget、Provider、
  protocol、Web/CLI 或任意 `model-proxy/*` 包；根包重新声明 Record、writer、
  logger、query heap 或 Shadow 聚合；
- `internal/cache` 反向依赖 Config、Proxy、Provider、protocol、events
  或任意 `model-proxy/*` 包；根包重新声明 store、entry 或 recorder；
- `internal/transport/bodycapture` 反向依赖 request log、protocol、Proxy、
  Config、Provider 或任意 `model-proxy/*` 包；
- `internal/runtime/wirecap` 反向依赖 Config、Proxy、Provider、HTTP/Web/CLI
  或任意 `model-proxy/*` 包；根包重新声明 verdict、capabilities map 或其锁；
- `internal/runtime` 依赖 `provider` 值类型之外的 Config、Proxy、HTTP/Web/CLI
  或持久化实现；根包恢复 health/sticky/pin/model-lock/paramBlock/spread/quota
  的第二份 map 或互斥锁；
- `internal/pricing` 反向依赖 `main` 的 YAML 配置、Proxy、Web 或通用 helper；
- `internal/protocol` import 任意 `model-proxy/*`，或反向读取 Config、Provider、
  Proxy、Web/CLI；Fusion 直接 import protocol 绕过 `targetexec.Plan`；
- 将 config generation 内的 map 原地修改。

## 专题文档

- 路由与失败：`routing-and-failure.md`
- 运行态与生命周期：`runtime-state.md`
- Provider pools：`provider-pools.md`
- 请求感知路由：`request-routing.md`
- 协议转换：`protocol-conversion.md`
- Fusion、Shadow、Cache 与观测：`fusion-shadow-cache.md`
- Web/API：`../web-api.md`

架构边界的静态回归位于 `architecture_*_contract_test.go` 套件（基于 go/ast 检查
`webServer` 字段类型、Web capability 方法 allowlist、禁止的 `w.p` selector、
账号测活的单次 runtime snapshot、internal 叶子包 import（含 accounts/catalog）、
`internal/observe/events`、`internal/cache` 与各自根 adapter 的职责、
`internal/config` 依赖 allowlist、根配置兼容 facade 以及 Fusion 不绕过
`targetexec.Plan`，不是字符串扫描）。`Proxy` / `runtime.Manager` 的语义所有权检查
合并 package 内全部生产 Go 声明，不绑定单一物理文件；adapter/facade 的精确形状
约束仍保持 file-scoped。行为与并发验证仍按
`docs/engineering/testing.md` 执行。
