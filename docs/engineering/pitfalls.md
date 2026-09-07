# 工程陷阱与回归守则

本文件记录跨模块、仍具有维护价值的陷阱。Provider 端点和认证细节归 `docs/backend-contracts.md`，Web/API 细节归 `docs/web-api.md`。

## URL 与协议

1. Provider OpenAI base URL 常已包含 `/v1`，拼客户端 `/v1/...` 前要按协议规则剥离，避免双 `/v1`。
2. Anthropic base URL 不带 `/v1`，SDK 自行追加 `/v1/messages`。
3. Codex 是 Responses API：`store:false`、`stream:true`、无 `max_tokens`，且 `input` 必须是 list；不能发送 Chat Completions `messages`。
4. reasoning/thinking 模型需要 reasoning replay，当前协议转换不支持。

## 请求与流

5. 上游请求使用 `http.NewRequestWithContext(r.Context(), ...)`，并受 `upstream_timeout` 限制。
6. `flushCopy` 在客户端写错误后必须立即停止读取上游。
7. 响应转换必须位于 logger/scanner/cache 内层，让它们看到客户端协议。
8. 非流式转换必须在 WriteHeader 前完成，失败时 fail-closed。

## 凭据与 Provider

9. SSO cookie 和 auth 信息只记录掩码或长度。
10. aqp/codex 登录文件使用 config provider name，不得硬编码 provider id。
11. 池化虚拟 provider 必须 BoundAPIKey；单账号池文件也必须 bind。
12. volcengine quota 使用管控面 AK/SK，Ark API Key 不能调用 GetAFPUsage。

## 配置

12b. config.yaml 的全部写路径必须进入同一个 `configedit.WithConfigLock` 边界：
    load→mutate→write（web 配置编辑器 `editConfigNode`、`AddPreset`/CLI `add` 的
    `MergeBlock`、`WriteProviderModels`）持锁覆盖**整个** RMW 区间，Web `/api/config`
    的 whole-document `SaveConfig` 也必须持锁到 reload/回滚结束，才能与 RMW writer
    串行。temp+rename 原子写只保证文件不损坏，不阻止后写者的 rename 静默丢弃先写者
    的变更（lost update；CLI 与 daemon 是不同进程，进程内互斥不够）。锁文件是
    `<config>.lock`（advisory flock，Windows 用 LockFileEx），只阻塞其他写者、从不阻塞读。
13. 新顶层配置字段必须六步同步：`internal/config.Config` → `rawConfig` → 拷贝段 → validate → 该包的 YAML 加载测试（yaml.v3 会静默忽略未知键，不能只直接构造 Config）→ 示例与文档（`config.yaml` 模板/README）。根包不承载字段或默认值逻辑（`config_compat.go` 已删除）。
14. duration 字段除明确允许的 `retry_wait: "0"` 外应验证为正数；任何允许零/负数的字段都要写入契约。
15. `BillingClass` iota 不是调度顺序，必须通过独立 `tierRank` 映射 `plan < unknown < payg`。

## 并发与生命周期

16. 跨域锁顺序是 `Proxy.mu → internal/runtime.Manager`；Manager 持锁时禁止回调
    Proxy、quota tracker、wirecap Store 或外部 I/O。
17. 后台 goroutine 必须有 owner、stop、wait 和 final flush。测试创建 owner 后必须注册 cleanup。
18. 原子文件写不能在多个实例/进程间共享固定 `.tmp` 名：daemon 的 Web 配置编辑器
    与 CLI 是不同进程，可并发写同一 config.yaml——共享 `.tmp` 名会让一个进程把另一个
    进程写了一半的文件 rename 走。现行实现：`internal/configedit.AtomicWrite` 用
    `os.CreateTemp` 生成唯一 `.<base>.tmp-*` 临时名（fsync + chmod 0644 + rename），
    credstore 的凭据写同模式；provider/persist.go 旧 `atomicWriteFile` 已删除，勿恢复
    固定名写法。
19. reload 中 config generation 与运行态 snapshot/fingerprint 必须一致。
20. `internal/cli/serve/supervisor.go` 的 supervisor `SpawnWorker` 可能返回 nil，调用方必须检查。
21. Proxy 级 goroutine 必须经 `Lifecycle.Run` 接纳；serve process 只能通过
    `applicationRuntime` 调用 `startRuntimeServices`/`Proxy.Close`。不得绕过它分散
    启动或 final flush。
22. daemon 收到退出信号时必须先 `http.Server.Shutdown` drain handler，再
    `Proxy.Close`；deadline 超时调用 `Server.Close` 只能取消连接，仍须等待 handler
    退栈。SIGHUP loop 和 Web GC 必须有 transport owner、stop 和 wait；禁止在 signal
    goroutine 中直接 `os.Exit`。
23. `main` 函数只绑定 OS 参数、I/O 与最终进程退出；根 `package main` 的
    `application` 是实际进程 composition owner，拥有命令表与 `serveAssembly`。
    无参数、help、未知命令及已知命令分发 seam 统一经过可测试的 `application.Run`
    （唯一 CLI 分发入口）。现阶段既有 handler 仍保留
    进程 I/O 和 `log.Fatal` / `os.Exit` 语义，不得误写成命令级迁移已完成。
    `serveAssembly` 拥有 serve 与前台/worker signal、HTTP 生命周期；
    `applicationRuntime` 构造/关闭 Proxy，启动运行时服务，装配 mux/Web，投影 reload
    并交出 transport task。daemon/supervisor signal 与 pid/probe 归
    `internal/cli/serve/supervisor.go`，平台 companion（`internal/cli/serve/detach_unix.go` /
    `internal/cli/serve/detach_windows.go`）只提供 child detach 属性，HTTP drain primitive
    留在 `internal/cli/serve/shutdown.go`；不要恢复第二个顶层分发器。

## 日志和持久化

24. 文件日志禁用 ANSI color。
25. request log list/report 不得在 metadata 查询中持有完整 body。
26. 运行态按名字落盘必须经过 config fingerprint；测试不得写真实 `~/.model-proxy`。
27. quota snapshot 超过 `3×poll_interval` 或带错误时只能视为 unknown。
28. Go 标准库行为：取消一个**带请求 body** 的外发 `http.Client.Do` 不会立刻
    关闭到上游的连接（不带 body 的会）。客户端在响应头阶段断开后，代理与上游
    之间的连接会存留到上游响应或 `upstream_timeout` 兜底。测试里模拟"上游挂起
    等待取消"时，不要依赖上游 handler 的 `r.Context().Done()` 传播，用测试自己
    控制的释放信号（见 `internal/app/client_cancel_test.go` 的 release channel）。
29. `quota_poll_interval` 在 tracker `Start()` 时读取一次并冻结 ticker：reload
    热改不生效，重启才生效（Web 配置编辑该键后需重启 daemon）。快照失鲜窗口
    （`QuotaTracker.FreshnessMaxAge`）在同一时刻以同一 cadence 冻结——调度 skip、
    失败分类和轮询必须同源，否则热改会把窗口缩到比轮询周期短，周期尾部把新鲜快照
    误判 stale、tier 在 plan/unknown 间抖动。
30. sticky 落盘键不区分命名空间：客户端可控的 `x-claude-code-session-id` 与
    route 名共享同一键空间。会话 id 恰好等于某 route 名时，会话选择会按该 route
    的 sticky 落盘并在重启后恢复。route 名是操作者控制的，实践中撞名概率极低，
    但新增 route 命名时避开常见会话 id 形态。
31. `budgets:` 的预算告警 watcher（tick 循环归 `internal/observe/budget`，startup
    准入与 lifecycle 接线归 `internal/app/proxy_lifecycle.go`）只在
    `StartRuntimeServices` 按启动配置决定
    是否创建（与 request_log 同为 startup-only）：从无到有加 `budgets:` 需重启
    daemon；已有 watcher 时阈值数值热改生效（每次 check 经端口读当前 cfg 快照
    拷贝）。去重
    记录是进程内的，重启后同一 (scope, 月份, 阈值) 会重新告警一次。

## 测试与 CI

32. 客户端读完响应 ≠ 服务端 commit 效应落账：带 Content-Length 的响应，客户端
    收满字节即返回，handler goroutine 此时可能还没执行 `flushCopy` 之后的效应
    （`Effects.Committed` 的 Requests/latency/attempts-ok、agent 计数、
    body.Close 触发的 request log 入队、fusion 的 run 计数）。本机核多负载低
    几乎永不踩中，CI（2 vCPU + race/coverage 插桩拖慢 handler 收尾）窗口被
    放大，同一竞态会在不同步骤轮流挂不同的测试。断言这类状态必须轮询等待
    （`internal/app` 的 `awaitCommitMetrics`/`waitUntil`），不得赌调度顺序；
    断言 commit 前效应（failures/failovers/guard 命中）或用
    `httptest.ResponseRecorder` 同步驱动 handler 的测试不受影响。注意 fake
    upstream 一次性 `Write` 的 "SSE" 同样带 Content-Length，不享流式豁免。
33. 覆盖率口径以 CI 工具链为准：CI 经 `go-version-file: go.mod` 用 go1.26.4，
    本机更高版本的语句计数不同（实测 `internal/cli/models` 本地 63.4% vs CI
    60.3%，足以跌破 floor）。floor/baseline 验证用
    `GOTOOLCHAIN=go1.26.4 scripts/cover.sh`；`go test` 结果缓存会掩盖重测，
    本地压测与复跑一律 `-count=1`。cover.sh 在本地工具链与 go.mod 不一致时
    会打印漂移警告（不 fail，CI 口径仍是权威）。
34. `post()` 返回 ≠ post-commit dispatch 已执行：shadow 的
    `dispatchShadowAfterCommit`（`internal/forward/forward.go`）在响应写给
    客户端之后才在请求 goroutine 里同步跑，客户端返回时它可能还没执行。对
    「dispatch 恰好发生在某窗口内」（如并发 gate 饱和期）的断言，用计数
    seam/barrier（如 `shadow.Runtime.Dropped()` + `waitUntil`）而不是赌
    时序——慢机器 + race 下 dispatch 可能晚到窗口结束之后，产生合法但意外
    的结果。

## 回归要求

- 每个陷阱应有测试或明确的测试缺口。
- 修复回归时优先把规则归入对应专题文档；这里只保留跨模块摘要。
- 已迁入 `backend-contracts.md` 或 `web-api.md` 的细节不要在这里复制。
