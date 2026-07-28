# 工程陷阱与回归守则

本文件记录跨模块、仍具有维护价值的陷阱。Provider 端点和认证细节归 `docs/backend-contracts.md`，Web/API 细节归 `docs/web-api.md`。

## URL 与协议

1. Provider OpenAI base URL 常已包含 `/v1`，拼客户端 `/v1/...` 前要按协议规则剥离，避免双 `/v1`。
2. Anthropic base URL 不带 `/v1`，SDK 自行追加 `/v1/messages`。
3. Codex 是 Responses API：`store:false`、`stream:true`、无 `max_tokens`，且 `input` 必须是 list；不能发送 Chat Completions `messages`。
4. reasoning/thinking 模型需要 reasoning replay，当前协议转换不支持。

## 请求与流

5. 上游请求使用 `http.NewRequestWithContext(r.Context(), ...)`。
6. `flushCopy` 在客户端写错误后必须立即停止读取上游。
7. 响应转换必须位于 logger/scanner/cache 内层，让它们看到客户端协议。
8. 非流式转换必须在 WriteHeader 前完成，失败时 fail-closed。

## 凭据与 Provider

9. SSO cookie 和 auth 信息只记录掩码或长度。
10. aqp/codex 登录文件使用 config provider name，不得硬编码 provider id。
11. 池化虚拟 provider 必须 BoundAPIKey；单账号池文件也必须 bind。
12. volcengine quota 使用管控面 AK/SK，Ark API Key 不能调用 GetAFPUsage。

## 配置

13. 新顶层配置字段必须同时加入 `Config`、`rawConfig` 和拷贝段。yaml.v3 会静默忽略未知键，因此必须增加 YAML 加载测试，不能只直接构造 Config。
14. duration 字段除明确允许的 `retry_wait: "0"` 外应验证为正数；任何允许零/负数的字段都要写入契约。
15. `BillingClass` iota 不是调度顺序，必须通过独立 `tierRank` 映射 `plan < unknown < payg`。

## 并发与生命周期

16. 锁顺序是 `healthMu → quotaMu`，禁止反向嵌套。
17. 后台 goroutine 必须有 owner、stop、wait 和 final flush。测试创建 owner 后必须注册 cleanup。
18. 原子文件写不能在多个实例间共享固定 `.tmp` 名。
19. reload 中 config generation 与运行态 snapshot/fingerprint 必须一致。
20. supervisor 的 `spawnWorker` 可能返回 nil，调用方必须检查。
21. Proxy 级 goroutine 必须经 `proxyLifecycle.run` 接纳；daemon 不得绕过
    `startRuntimeServices`/`Proxy.Close` 分散启动或 final flush。
22. daemon 收到退出信号时必须先 `http.Server.Shutdown` drain handler，再
    `Proxy.Close`；deadline 超时调用 `Server.Close` 只能取消连接，仍须等待 handler
    退栈。SIGHUP loop 和 Web GC 必须有 transport owner、stop 和 wait；禁止在 signal
    goroutine 中直接 `os.Exit`。

## 日志和持久化

23. 文件日志禁用 ANSI color。
24. request log list/report 不得在 metadata 查询中持有完整 body。
25. 运行态按名字落盘必须经过 config fingerprint；测试不得写真实 `~/.model-proxy`。
26. quota snapshot 超过 `3×poll_interval` 或带错误时只能视为 unknown。

## 回归要求

- 每个陷阱应有测试或明确的测试缺口。
- 修复回归时优先把规则归入对应专题文档；这里只保留跨模块摘要。
- 已迁入 `backend-contracts.md` 或 `web-api.md` 的细节不要在这里复制。
