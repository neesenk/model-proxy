# AGENTS.md - 核心运行时规则

适用于 `model-proxy/` 下的 Go daemon、CLI 和测试。Provider 子目录和 Web 前端有更具体的就近规则。
全局模块依赖与 composition root 契约见 `../docs/architecture/overview.md`。

## 核心边界

- `Proxy.mu` 保护 reload 交换的 config/providers/routes 快照；请求转发不得在流式响应期间长期持有它。
- 每个请求只通过 `snapshotRuntime()` 捕获一次 reload-owned 依赖，并以
  `runtimeSnapshot → serveRequest → internal/targetexec.Attempt` 传递；普通路由和
  Fusion synthesis 必须共享该五组强类型执行契约，不得重新扩张 positional
  参数链或用 `any` 携带根对象。
  Shadow 的采样率、semaphore 和 client 也属于该快照，commit 后不得重新读取
  `p.shadow`。
- 单目标 I/O 由 `internal/targetexec.Executor` 执行；它只能通过
  generation-frozen `targetexec.State` 与 typed `Effects/Responses` 端口访问
  runtime/observability，不得 import 或持有完整 `*Proxy`，也不得访问调度、
  reload、Web、lifecycle 或 Shadow。根 `targetexec_adapter.go` 只绑定 captured
  generation/scheduling 和应用 stores，不得重新实现 HTTP、转换或 retry。
- 主请求的调度、failover、cooldown 重试与 commit 编排统一位于
  `proxy_forward.go`；`proxy.go` 只声明 composition-root owner。
- 三协议方向只在 `internal/protocol/conversion_registry.go` 注册；request、
  response、SSE 入口必须共享同一 pair 定义，不得各自维护方向 switch。
  `internal/protocol` 是仓库依赖叶子，不得 import `model-proxy/*`；Provider 方言、
  目标视觉能力由 `planTarget` 解析后通过 `targetexec.Plan` request options
  注入，HTTP 错误写入留在 transport 层。
- 普通 route、所有 Fusion leg 和 Shadow 共享 `targetexec.Plan`；其中不可变的
  model/body/URL/path wire preparation 统一归
  `internal/targetexec.Plan`，根 `planTarget` 只把同一 runtime snapshot 解析出的
  provider、protocol、wire verdict 与视觉能力投影为 plan input。准备逻辑不得
  复制，异步分支只能使用主请求捕获的 runtime snapshot，禁止在 goroutine 内
  重新读取 reload-owned 状态。
- Web/API handler 只能通过 `proxyReadView` 读取运行时；不得直接获取 Proxy 锁或
  读取 config/provider/health/model-lock 内部 map。
- Proxy 级后台任务必须由 `proxyLifecycle` 接纳，daemon 只调用
  `startRuntimeServices`/`Proxy.Close`；会写 request log 的有限任务必须在 logger
  drain 前完成，禁止分散启动 goroutine 或重复 final flush。
- 实时事件 DTO、最近 ring、订阅和非阻塞 fan-out 统一归
  `internal/observe/events`；该包是无仓库内依赖叶子。根 `live_events.go` 只保留
  SSE/keepalive 适配，不得重新持有 event ring、subscriber map 或其锁。
- request log 的 JSONL schema、header 白名单/body 截断、非阻塞 logger、
  rotation/retention/owner-only 权限、top-K 查询、Summary 脱敏与 Shadow 聚合
  统一归 `internal/observe/requestlog`；该包是无仓库内依赖叶子。根
  `request_log_adapter.go` 只映射 Config 和执行上下文纯值，capture 位置、
  lifecycle drain、Web 参数、replay policy、Fusion/Shadow eligibility 留在根层。
  metadata 调用必须使用 `QuerySummaries`/`ShadowReport`，不得用完整 Record
  查询后再忘记清 body/header。
- 通用有界响应流 tee 统一归 `internal/transport/bodycapture`，不得把 request
  log、Responses state 或 Shadow 业务语义塞回 Reader。
- 精确响应 cache 的 key、TTL/容量 store、bounded recorder、header normalization
  和 replay 统一归 `internal/cache`；该包是无仓库内依赖叶子。根层只做
  `CacheConfig` 适配和 force/pin/eligibility/lifecycle 编排，禁止重新声明 entry、
  recorder 或访问 Store 内部锁与 map。
- SQLite stats 的 schema、additive migration、minute/agent upsert、retention、
  legacy import 与聚合查询统一归 `internal/observe/stats`；该包是无仓库内依赖
  叶子，不得读取 Config/HOME、持有 metrics/tokens/agents 或参与 Proxy lifecycle。
  根 `statsFlusher` 只负责运行时 snapshot/diff/reset baseline，`proxyLifecycle`
  只负责 loop、final flush 与 Store close；Web 仍经 `proxyReadView` 查询。
- config generation 内的 health、sticky、pin、model lock、paramBlock、spread、
  quota、调度决策和 detached snapshot 统一归 `internal/runtime.Manager`，由其单
  mutex 保持原子性。该包只允许依赖 `provider` 值类型，不得读取 Config、执行
  HTTP/持久化或承担 Web 展示。
- `quotaTracker` 只负责轮询、manual/429 refresh 去重和状态文件编排，不得恢复
  第二份 quota map。跨域锁顺序始终为 `Proxy.mu → runtime.Manager`；Manager
  持锁期间不得回调 Proxy、quota tracker 或外部 I/O。
- wire capability 的三态、选择策略和并发状态统一归
  `internal/runtime/wirecap`；其 Store mutex 是 leaf lock，持锁时不得回调
  Proxy。根 `wirecap.go` 只负责 probe、provider/config 适配与持久化调度。
- 正常转发、Fusion、Shadow、probe 共享 provider identity resolver；池化父名不能直接进入上游请求。
- provider config 通过 parentOf 解析，runtime implementation 使用虚拟 provider id 查找。
- reload 应让请求看到一致的 cfg/providers/poolIndex/expandedRoutes generation；
  resolver spread 与所有运行态 mutation 都必须携带该 generation。

## 转发契约

- 同协议字节级透传；跨协议 target（显式声明 `protocol:`，或经 `resolvedBackendProto` 自动回退 `ProtocolHint`，如 codex→responses，或 wire 探测 verdict 判定端点不支持客户端协议）才转换。verdict unknown 维持透传；因 verdict 转 responses 后上游 404 时翻转 verdict 且不锁模型（wirecap.go）。
- 上游请求必须继承客户端 context，并受 `upstream_timeout` 限制。
- nil runtime impl、request conversion 和 response conversion 都 fail-closed。
- 客户端断开后停止继续拉取上游流。
- logger、token scanner、cache 捕获的是客户端最终收到的协议字节。
- pin/force-provider 是硬选择，且绕过 cache。

失败分类、模型锁、剥参和 cooldown 详见 `../docs/architecture/routing-and-failure.md`。

## 调度与状态

- 排序是 tier → priority → surplus；priority 高于 surplus。
- plan 优于 unknown，unknown 优于 pay-as-you-go。
- session sticky 保账号 cache；账号不可用时才迁移。
- quota/health/sticky 持久化必须与 config generation 一致。
- 后台 goroutine 必须有明确关闭生命周期；测试不得写真实 HOME。

详见 `../docs/architecture/runtime-state.md` 和 `../docs/architecture/provider-pools.md`。

## 配置

配置类型、YAML 加载、默认值、校验和生效值 accessor 统一归
`internal/config`；该包只允许依赖 `internal/pricing` 与 `internal/protocol`。
根包 `config_compat.go` 仅保留类型别名以及 `LoadConfig` /
`LoadConfigFromBytes` 兼容 wrapper，不得承载配置逻辑，也不得恢复根
`config.go`。

新顶层配置字段的六步同步（`internal/config.Config` → `rawConfig` → 拷贝段
→ validate → YAML 加载测试 → 示例与文档）以
`../docs/engineering/pitfalls.md` 的配置节为权威定义。不要用直接构造
`Config` 的测试替代 YAML 加载覆盖。

## Models.dev catalog

slim metadata projection、canonical-owner 去重、HTTP/ETag/TTL 刷新和磁盘缓存
统一归 `internal/catalog`；该包是无仓库内依赖叶子，不能读取 Config、HOME、
环境变量、Proxy、Provider 或 Web/CLI。根 adapter 只注入 cache path、endpoint
和 provider/route 名单；请求路由必须继续使用 `runtimeSnapshot` 捕获的 catalog，
不得在请求内刷新或重新读取。

## API-key accounts

账号 schema、稳定 ID、plural/legacy 文件优先级、原子保存和跨进程 mutation lock
统一归 `internal/accounts`；该包是无仓库内依赖叶子，不得读取 HOME、Config、
Provider、Proxy、Web/CLI 或发起网络验证。根 `accounts_adapter.go` 只注入 HOME
并保留迁移期兼容 wrapper；交互、凭据验证、Provider 构建和 reload 继续留在应用
编排层。任何 pool 写操作必须保持“网络/用户输入在锁外，锁内重新
load → 按稳定 ID 修改 → save”的顺序。

运行时 Provider 构建必须消费一次 `accounts.Store.LoadSnapshot` 同时取得 pool
与 Source；不得在 `Load` 后另做 `Stat`。损坏 plural、空 plural tombstone 和
损坏 legacy 均 fail-closed，不得构建可能重新读取旧凭据的 runtime provider。
同一 build pass 必须连同 providers/poolIndex/parentOf 一起派生 implicit-route
eligibility；startup/reload 不得再读账号文件生成同一 generation 的 routes。

## CLI

CLI 输出是 change-controlled contract。修改命令、字段、颜色、顺序或提示前读取 `CLI.md`，实现后同步更新它。文件日志不得带 ANSI color。

## 测试

完整测试矩阵和断言规范见 `../docs/engineering/testing.md`。

- 构造 `NewProxy` 的测试必须隔离 state path/HOME，并注册 cleanup。
- 并发和时间边界测试使用可缩短的 duration、fake Provider、channel/barrier 或
  可观察状态，避免真实长等待；不得为测试向生产结构体增加 hook 字段。
- 新状态机至少覆盖成功、硬失败、限频、取消和 reload/重启。
- 修复 race 时运行定向 `go test -race -run ... -count=20`，再跑全量 race。
