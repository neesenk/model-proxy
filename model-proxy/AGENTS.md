# AGENTS.md - 核心运行时规则

适用于 `model-proxy/` 下的 Go daemon、CLI 和测试。Provider 子目录和 Web 前端有更具体的就近规则。

## 核心边界

- `Proxy.mu` 保护 reload 交换的 config/providers/routes 快照；请求转发不得在流式响应期间长期持有它。
- 每个请求只通过 `snapshotRuntime()` 捕获一次 reload-owned 依赖，并以
  `runtimeSnapshot → serveRequest → targetAttempt` 传递；普通路由和 Fusion
  synthesis 必须共享 `targetAttempt` 执行契约，不得重新扩张 positional 参数链。
- 单目标 I/O 由 `attemptExecutor` 执行；它只能通过 `attemptState` 窄端口修改
  runtime state，不得重新持有完整 `*Proxy` 或访问调度、reload、Web 职责。
- 三协议方向只在 `conversion_registry.go` 注册；request、response、SSE 入口
  必须共享同一 pair 定义，不得各自维护方向 switch。
- 普通 route 和所有 Fusion leg 共享 `targetPlan`；provider/protocol/model/body/
  URL/path 准备逻辑不得在 Fusion 中复制，Fusion 只能使用请求捕获的 runtime snapshot。
- Web/API handler 只能通过 `proxyReadView` 读取运行时；不得直接获取 Proxy 锁或
  读取 config/provider/health/model-lock 内部 map。
- `healthMu` 保护熔断、限频、modelLocks、paramBlock、sticky、spread counter。
- quota tracker 使用独立 mutex；锁顺序始终为 `healthMu → quotaMu`。
- 正常转发、Fusion、Shadow、probe 共享 provider identity resolver；池化父名不能直接进入上游请求。
- provider config 通过 parentOf 解析，runtime implementation 使用虚拟 provider id 查找。
- reload 应让请求看到一致的 cfg/providers/poolIndex/expandedRoutes generation。

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

新顶层配置字段的六步同步（`Config` → `rawConfig` → 拷贝段 → validate → YAML 加载测试 → 示例与文档）以 `../docs/engineering/pitfalls.md` 的配置节为权威定义。不要用直接构造 `Config` 的测试替代 YAML 加载覆盖。

## CLI

CLI 输出是 change-controlled contract。修改命令、字段、颜色、顺序或提示前读取 `CLI.md`，实现后同步更新它。文件日志不得带 ANSI color。

## 测试

完整测试矩阵和断言规范见 `../docs/engineering/testing.md`。

- 构造 `NewProxy` 的测试必须隔离 state path/HOME，并注册 cleanup。
- 并发和时间边界测试使用可缩短的 duration/hook，避免真实长等待。
- 新状态机至少覆盖成功、硬失败、限频、取消和 reload/重启。
- 修复 race 时运行定向 `go test -race -run ... -count=20`，再跑全量 race。
