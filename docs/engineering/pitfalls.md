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
7. 响应转换必须位于 logger/scanner 内层，让它们看到客户端协议。
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
14. duration 字段必须在加载期经 `internal/config` 的 `durationChecks` 校验（写错单位如 `"3d"` 一律拒绝加载）；只有该表标了 `allowZero` 的字段接受 `"0"`，零值语义写在各字段契约里：`scheduling.circuit_persist: "0"`（关闭 streak 持续门槛）、`scheduling.retry_wait: "0"`（关闭冷却等待重试）、`scheduling.stream_keepalive: "0"`（关闭 SSE 心跳）、`stats.retention: "0"` 与 `request_log.retention: "0"`（永久保留）、`request_log.hot_retention: "0"`（关闭冷层归档）。新增 duration 字段必须进该表并明确 `allowZero`。
15. `BillingClass` iota 不是调度顺序，必须通过独立 `schedulingTier` 映射 `plan < unknown < payg`。
43. config 引用「随二进制内置的 stdio 子命令」（`command: [<path>/model-proxy, mcp-server,
    <name>]`）存在部署先后窗口：旧二进制 + 新 config **不会启动失败**（校验只看
    argv 形状，不解析子命令），而是该 server 的 initialize 时子进程秒退（CLI 前门答
    `unknown command`）、网关回 502——降级只限该 server，daemon 其余面不受影响。按
    惯例（`scripts/restart_serve.sh` + 已提交 HEAD 构建，见 24b 与 `CLI.md`「手动重启」）
    先换二进制再 reload config 即无窗口。`/mcp/<name>` initialize 502 + 子进程
    stderr 带 `unknown command` 即此症，勿当成上游故障排查。

## 并发与生命周期

16. 在 Manager 状态锁内调用应用回调或执行文件 I/O 会阻塞请求与 reload。先取 detached snapshot，再在锁外编码或持久化。跨域锁边见[运行态锁顺序](../architecture/runtime-state.md#锁顺序)。
17. 后台 goroutine 必须有 owner、stop、wait 和 final flush。测试创建 owner 后必须注册 cleanup。
18. 原子文件写不能在多个实例/进程间共享固定 `.tmp` 名：daemon 的 Web 配置编辑器
    与 CLI 是不同进程，可并发写同一 config.yaml——共享 `.tmp` 名会让一个进程把另一个
    进程写了一半的文件 rename 走。现行实现：`internal/configedit.AtomicWrite` 用
    `os.CreateTemp` 生成唯一 `.<base>.tmp-*` 临时名（fsync + chmod 0644 + rename），
    credstore 的凭据写同模式；provider/persist.go 旧 `atomicWriteFile` 已删除，勿恢复
    固定名写法。
19. reload 中 config generation 与运行态 snapshot/fingerprint 必须一致。
19b. reload 不得把「后端同步状态」当 config 派生态清空：quota snapshot 是同步
    观测（last-known-good——用上一次的 cache，同步成功才切换，失败不改变任何
    数据）。旧实现 `ReplaceGeneration` 整表清空 quota，把未声明 `billing:` 的
    实测 plan provider 在 reload→PollAsync 的窗口内降为 unknown 档，声明了
    `billing: plan` 的 provider 便越过它（tier 排在 priority 之前）——表现为
    会话中间歇性打到并非性价比首选的 provider（opencode-go 事故）。同步
    失败同理：`Err != ""` 的快照不得覆盖已有条目，也不得参与
    `QuotaRecoveredClearCooldown`（no-op 同步零副作用）。现行契约：
    `ReplaceGeneration(generation, liveQuotaKeys)` 同一临界区内存活 cache、剪枝
    死 key；`quotaSyncFailedKeepsExisting` 守卫提交侧。新增后端同步状态
    （模型能力、配额、账号信息）时必须套同一模式，不得退回“先置默认再同步切回”。
20. `internal/cli/serve/supervisor.go` 的 supervisor `SpawnWorker` 可能返回 nil，调用方必须检查。
21. Proxy 级 goroutine 必须经 `Lifecycle.Run` 接纳；serve process 只能通过
    `applicationRuntime` 调用 `StartRuntimeServices`/`Proxy.Close`。不得绕过它分散
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
24b. 重启运行中的 `model-proxy serve` 时，停与启必须在**同一条 shell 命令内原子完成**
    （`scripts/restart_serve.sh` 已封装：`serve stop`（pidfile 属主——daemon supervisor
    或占 pidfile 的前台 serve，优雅→SIGKILL）→ 端口残留 SIGINT→SIGKILL → 轮询等端口
    释放 → `serve daemon` 拉起 supervised daemon → 轮询等端口监听（1s settle 复查）；
    `--build` 先构建 `.new`、快照 `model-proxy.old` 再切换，新二进制起不来时**先 stop
    再回滚旧二进制重新 daemon** 并 exit 1——pidfile 存活会让回滚的 daemonize 拒绝启动，
    漏掉这步的回滚必然失败。手动模板见 `CLI.md`「手动重启」）。把 kill 和 start 拆到
    两次工具调用/两个终端步饗，中间的下线窗口会断掉一切依赖该代理的下游——典型是把它
    当 LLM 网关用的 coding agent 本身：agent 的下一次模型调用直接 `Connection error`，
    连"生成下一条重启命令"都做不到，形成自锁。换新二进制时先单独 `go build`（构建不
    动运行中的进程），再用单命令切换。对 daemon 模式（supervisor+worker）的 serve
    绝不允许只 SIGINT 端口监听者：那只是 worker，supervisor 约 1s 后退避会重拉它——
    要么旧 worker 抢回端口造成"重启成功但跑旧二进制"，要么脚本实例胜出后 supervisor
    残留并在新实例退出时让旧二进制复活。pidfile 派生有多处（脚本 daemon 的
    `--log-file` 旁、config `log_file:`、缺省 `$TMPDIR/model-proxy.pid`）：config
    显式设置 `log_file:` 让所有派生收敛到同一位置，否则裸 `serve stop`/login-SIGHUP
    会找错文件。

## 日志和持久化

24. 文件日志禁用 ANSI color。
25. request log list/report 不得在 metadata 查询中持有完整 body。
26. 运行态按名字落盘必须经过 config fingerprint；测试不得写真实 `~/.model-proxy`。
26b. takeover 相关测试必须 `t.Setenv("HOME", t.TempDir())` **整户隔离** HOME：预设模板的
    `file:`~展开走 `os.UserHomeDir()`，`takeover all`（含 admin/Web 面的
    `RunTakeover("all")`）在真实 HOME 下会把**真实客户端配置**（`~/.claude/settings.json`、
    `~/.pi/agent/models.json` 等）改写进备份目录指到临时路径的状态——备份随 `t.TempDir()`
    清理即不可恢复。只替换 `HomeDir` 端口/templatesDir 不够（模板 file 展开不看它们）；
    `internal/takeover` 与 `internal/admin` 的 takeover 测试都以 `t.Setenv("HOME", …)` 为准。
27. quota snapshot 超过 `3×poll_interval` 或带错误时只能视为 unknown。
28. Go 标准库行为：取消一个**带请求 body** 的外发 `http.Client.Do` 不会立刻
    关闭到上游的连接（不带 body 的会）。客户端在响应头阶段断开后，代理与上游
    之间的连接会存留到上游响应或 `upstream_timeout` 兜底。测试里模拟"上游挂起
    等待取消"时，不要依赖上游 handler 的 `r.Context().Done()` 传播，用测试自己
    控制的释放信号（见 `internal/app/forward_retry_test.go` 的
    `TestUC_ClientCancelDuringHeadersStopsFailoverAndKeepsCircuitClosed` release channel）。
29. `quota_poll_interval` 在 tracker `Start()` 时读取一次并冻结 ticker：reload
    热改不生效，重启才生效（Web 配置编辑该键后需重启 daemon）。快照失鲜窗口
    （`QuotaTracker.FreshnessMaxAge`）在同一时刻以同一 cadence 冻结——调度 skip、
    失败分类和轮询必须同源，否则热改会把窗口缩到比轮询周期短，周期尾部把新鲜快照
    误判 stale、tier 在 plan/unknown 间抖动。
30. sticky 落盘键不区分命名空间：客户端可控的 `x-claude-code-session-id` 与
    route 名共享同一键空间。会话 id 恰好等于某 route 名时，会话选择会按该 route
    的 sticky 落盘并在重启后恢复。route 名是操作者控制的，实践中撞名概率极低，
    但新增 route 命名时避开常见会话 id 形态。
31. 条目保留：`budgets:` 月度预算告警及其 watcher 已移除，旧配置在加载时明确报错。
32. **会话身份双轨**：观测面（request log / live events / `/api/sessions`）的
    `session_id` 来自 `request_log.session_headers` 允许列表（`requestlog.SessionID`，
    有序取第一个非空头），而 guard 域（session_scan 分片检测、
    `guard.adjudicate` 会话拉黑，见 `internal/forward/forward.go`）硬编码
    `x-claude-code-session-id`。operator 把允许列表配成其它头时，两套会话维度
    对不上：拉黑/分片检测拿不到会话键（不拉黑、不跨请求重组），但 Live/Requests
    页照常按会话聚合——排查"为什么没拉黑"先查客户端发的是不是 claude-code 头。
    统一成单一解析点是长期方向；短期改动 guard 域会话键时必须同步
    decision 20/36 的"无头不拉黑"约束。
33. `model_caps.json` 有**两个无协调的写进程**（CLI `models refresh` 与 daemon 的
    async persist，各自原子 rename）：CLI 写文件 → SIGHUP 的窗口内，daemon 一个早先
    触发的 async persist 若抢在 reload 重读前执行，会用较旧的内存快照覆盖 CLI 的结论。
    daemon 侧的协调是 mtime 基准守卫（`Proxy.modelCapsFileBaseline`）：persist 写盘前
    stat 目标，比 boot restore/最近 reload 重读记录的基准新即判定外部写过——此时
    不是跳过，而是 CAS-with-adopt：重新 Load 采纳外部状态并 re-baseline（reload 重读
    仍是主采纳路径），文件不可读/版本不符才 defer。CLI 侧有同构反向守卫：写前重 stat，
    冲突则重新 Load 合并再写（上限 3 次）。
    stat→rename 之间仍有固有 TOCTOU 残窗，两个不加锁的进程无法彻底消除；不要给这个
    文件加跨进程锁来"修"它（CLI 是短命进程，锁文件 lifecycle 比竞态本身更危险）。

## 测试与 CI

34. 客户端读完响应 ≠ 服务端 commit 效应落账：带 Content-Length 的响应，客户端
    收满字节即返回，handler goroutine 此时可能还没执行 `flushCopy` 之后的效应
    （`Effects.Committed` 的 Requests/latency/attempts-ok、agent 计数、
    body.Close 触发的 request log 入队）。本机核多负载低
    几乎永不踩中，CI（2 vCPU + race/coverage 插桩拖慢 handler 收尾）窗口被
    放大，同一竞态会在不同步骤轮流挂不同的测试。断言这类状态必须轮询等待
    （`internal/app` 的 `awaitCommitMetrics`/`waitUntil`），不得赌调度顺序；
    断言 commit 前效应（failures/failovers/guard 命中）或用
    `httptest.ResponseRecorder` 同步驱动 handler 的测试不受影响。注意 fake
    upstream 一次性 `Write` 的 "SSE" 同样带 Content-Length，不享流式豁免。
35. 覆盖率口径以 CI 工具链为准：CI 经 `go-version-file: go.mod` 用 go1.27.2，
    本机更高版本的语句计数不同（实测 `internal/cli/models` 本地 63.4% vs CI
    60.3%，足以跌破 floor）。floor/baseline 验证用
    `GOTOOLCHAIN=go1.27.2 scripts/cover.sh`；`go test` 结果缓存会掩盖重测，
    本地压测与复跑一律 `-count=1`。cover.sh 在本地工具链与 go.mod 不一致时
    会打印漂移警告（不 fail，CI 口径仍是权威）。go 指令升级（如 1.27.1→1.27.2
    修 stdlib 漏洞让 govulncheck 回绿）时同步改本条版本号。
36. `post()` 返回 ≠ post-commit dispatch 已执行：shadow 的
    `dispatchShadowAfterCommit`（`internal/forward/forward.go`）在响应写给
    客户端之后才在请求 goroutine 里同步跑，客户端返回时它可能还没执行。对
    「dispatch 恰好发生在某窗口内」（如并发 gate 饱和期）的断言，用计数
    seam/barrier（如 `shadow.Runtime.Dropped()` + `waitUntil`）而不是赌
    时序——慢机器 + race 下 dispatch 可能晚到窗口结束之后，产生合法但意外
    的结果。
37. Go http transport 对**带 body 的请求** cancel 后不关闭上游连接：客户端 ctx
    取消时，服务端 handler 的 `r.Context().Done()` 不一定触发（连接被保留复用）。
    测试「客户端取消 → 上游快速感知」时，fake 上游不能靠 `r.Context().Done()` 断言，
    要用测试控制的 channel/release 门（参考 `internal/app` 的
    `TestProxyCloseWaitsForOwnedTasksAndRejectsNewWork`）。
38. 测试 fake 逐行孪生生产实现必然漂移：`internal/forward` 的 harness 曾把
    `Manager.RecordLatchOutcome` 状态机逐行复制成 fake，5 个 latch 测试全在验副本，
    生产回归能全绿通过。测试需要生产状态机时直接用真实 owner（测试侧 import
    不破模块边界，archtest 只扫生产文件）；做不到时也绝不复制分支逻辑，改用
    窄 seam 注入。
39. 「外部写采纳」必须与自家写互斥：model_caps.json 的异步 persist 以
    `文件 mtime > baseline` 判定 CLI `models refresh` 外部写并采纳（Restore 回内存）。
    但每次探测 pass 各起一个 quota-tracked persist goroutine，可并发——自家 persist A
    rename 完成后、baseline 记账前，persist B 的 stat 会把 A 刚写的文件当成外部写，
    用**旧快照整体替换**内存 verdicts，恰好落在某 pass 两个模型 Put 之间时，先落地的
    探测结论被抹掉（Get 返回零值 unknown×3、无 inconclusive 日志——因为探测根本
    没跑，是结果被回滚）。修复：`modelCapsPersistMu` 把 stat→采纳/写入→re-baseline
    临界区化（reload 的重读+re-baseline 同锁），自家持久化串行后 mtime 必 ≤ baseline，
    外部写检测不受影响。锁序约束：持该锁不得再取 p.mu（采纳路径的 cfgSnapshot
    只拿 RLock，reload 的加锁段在 p.mu 之前）。回归：`TestModelCaps_ConcurrentPersistNeverRollsBackPuts`
    （修复前 -cpu 1/2 可靠复现）。

40. 跨进程文件锁的「陈旧恢复」必须二次证据，不得只凭 mtime：账号池锁曾以
    `mtime > 60s` 即 `Remove` 回收——但 `time.Since` 是墙钟运算，持有进程被
    SIGSTOP/调试器挂起或 NTP 回拨 >60s 时心跳停摆而进程健在，锁被偷后两个
    写者同入 load→modify→save，池文件丢账号；原持有者恢复后心跳还会打到
    新锁文件上、退出时删掉新持有者的锁。修复：锁文件里已有持有者 PID，
    Remove 前先 `Signal(0)` 探活，活着就继续等（deadline 兜底），死/无法解析
    才回收（legacy 空文件退回 mtime 判定；Windows 无 signal-0 探活同样退回
    旧行为，不劣化）。PID 复用只会导致「不回收→超时报错」，方向保守安全。
    回归：TestWithPoolLock_StaleButAliveHolderIsNotStolen、TestLockHolderAlive、
    StaleSteal 的死 PID 用真实退出子进程（硬编码 PID 机器相关会 flake）。
    同型原则：凡「看起来死了」就破坏互斥的恢复路径，都需要存活证据。

41. 池化/回调切片的 retain 必须拷贝，锁管不住所有权：bodycapture 的回调
    切片契约明文「仅在回调期间有效，retain 必须拷贝」——Close 归还缓冲后，
    并发请求复用同一 backing array。任何异步分支若直接 retain 该切片，都
    会出现数据竞争并可能把另一请求的正文混进当前请求（锁维度审计不可见，
    违规在切片生命周期）。审查多 goroutine 共享时除了查锁，必须追**值的
    来源生命周期**（池、回调参数、复用 buffer）。
    同批修复：wirecap 404 运行时纠正补 expected-fingerprint 门（旧代 404
    不得翻转新代模型裁决，模型级 no 无 TTL）；MCP stdio initialize 与
    LRU 逐出竞态的丢 put 必须 kill 子进程（否则泄漏到进程关闭）；adjudicate
    Unblock 先拆拦截面后删 block（防并发缓存命中把刚解封的会话悄悄复原）；
    logfile Run 退出后 Enqueue 计入 Dropped（stopMu 序列化 closed 检查与
    close+drain）。

42. **usage 账本有两条独立解析路径，必须逐字段同口径**（热路径 UsageScanner →
    分钟桶/Accounts/Analytics、requestlog.ExtractUsage → Keys 用量/会话聚合）：anthropic 口径（input 不含 cache，hit/write
    单列），inclusive wire 计数器（openai prompt_tokens、responses
    input_tokens）入桶前扣减钳零；hit 双拼写 max 合并
    （prompt_tokens_details.cached_tokens / deepseek prompt_cache_hit_tokens），
    write 双拼写（details.cache_write_tokens / cache_creation_input_tokens
    直拼）——转换器发射侧与消费侧必须成对补拼写（cache_write_tokens 是转换器
    家族的发射约定，消费侧不解析则 write 全额落 input、成本按全价低估）。历史
    教训：89630e1 前缓存命中双重计入 input+cache_read（deepseek 控制台对账
    差值恰=命中量）、2537235 前网关逐 chunk 重复累计把单请求膨胀上亿 token；
    三方一致性由 `internal/app/usage_ledger_test.go`（同一批 wire 样本喂两个
    解析器断言同值）钉住——counters 与 requestlog 的多帧序列、全部真实录制流
    （`testdata/wire/*.sse`）两侧一致性另由
    `internal/app/r6_usage_ledger_matrix_test.go` 钉住（形态内规则同构，
    规则权威在 `docs/architecture/protocol-conversion.md`，改一处必须同步另一处；
    跨形态汇总相加 vs 取最大是有意差异），历史脏数据由
    `observestats.Store.RebuildTokenLedger`（启动时按请求日志索引重写 cutoff
    前分钟桶，带 count 防御）修正；count 防御跳过、且请求日志源行已被轮转
    删掉的桶无法逐笔重建——物理不可能账本（output>128k/笔 或 input>2M/笔，
    见 `repairImpossibleLedgers`）token 四列清零（requests/timings 保留），
    2026-10-11 step-plan 16 笔 116M output/2.24B input 的 pocket 即此修法。
    **请求数口径**：EvRequests/attempts.ok/
    平均延迟只计 **2xx commit**——上游终局 4xx（客户端坏 body 风暴，实测单日
    10,753 条零 token 400 vs 3,484 条真实调用）是"服务了的响应"不是"成功请求"，
    控制台请求数=计费成功数；修复前 4xx 也无条件计入（与 attempts.ok 注释
    "committed 2xx" 相悖的存量 bug），requests 列历史由同一重建按索引 2xx 数
    修复（防御区间 [n200, n]，幂等）。

44. **多写者共享继承 fd 的日志文件只能 copy+truncate 轮转，不能 rename**：
    serve.log 由 supervisor/worker 经 exec 继承的同一个 O_APPEND fd 写入（前台镜像
    再开一个），rename 轮转会把后续写入留在旧 inode——所有写者继续写"归档"、
    新建的活跃文件永远为空。`internal/cli/serve/logrotate.go` 用快照副本 + 原文件
    truncate 归零（inode 不变，O_APPEND 自动续写新末尾），代价是 copy 与 truncate
    之间写入的行丢失（copytruncate 标准语义，面向人的运行日志可接受；无损轮转属于
    单写者的 logfile sink，见 observe.md）。同理，时龄 retention 防不住
    突发写满盘——2026-11-12 单日 94.5GB 的 request-log 证明 30d 窗口在突发速率下
    只是慢动作溢出；`request_log.disk_guard`（缺省 enabled/50GiB/95%）在每小时
    sweep 上按最旧优先删轮转文件至卷空闲恢复，活动文件与 index.db 永不删
    （索引一致性沿用 prune 的 reconcile 契约）。

45. **夹在连接与 executor 之间的 `http.ResponseWriter` wrapper 必须实现 `Unwrap()`**：
    executor 用 `http.ResponseController` 在连接上设客户端写 deadline——不读取的对端
    不得把 handler 占过 `upstream_timeout`（规则见 `routing-and-failure.md`「客户端写入上限」）。
    控制器只穿过实现了 `Unwrap` 的 wrapper：`forward.countingResponseWriter` 与
    `targetexec.timingResponseWriter` 任一丢掉它，deadline 就设不到连接上、上限静默失效，
    而两个包各自的单元测试全绿——只有穿过组合根的集成测试（`internal/app` 的
    `TestForward_ClientStopsReadingIsBoundedByUpstreamTimeout`）能抓到。二者还实现
    `FlushError()`：小写入先落 bufio，超时失败发生在 flush，而 `http.Flusher.Flush` 报不出错误。
    在这条链上新增中间层 wrapper 时同此。

46. **请求路径上不要同步做带超时的网络刷新**：`Proxy.pricingSnapshot` 曾在 `pricingMu` 下同步调
    `pricing.EnsureFresh`——缓存陈旧且源不可达时，每个 `/api/sessions`、`/api/analytics` 请求都等完
    fetch 超时（5s/请求），而且失败不做负缓存，请求越多撞得越多。现状：读内存快照、后台单飞刷新、
    失败退避、启动/reload 预热（`docs/web-api.md`「价格」）。**症状识别**：某个只读端点延迟恒等于一个
    整数秒（像超时）而其它端点几毫秒——先量端点延迟，别先怀疑用例。另一面是**测试夹具里的隐式外网
    依赖**：UI 浏览器 e2e 的沙箱 serve 只带 `PATH`/`HOME`/`MP_MODELSDEV_URL`，没有代理环境变量，
    任何没被 stub 的外部源（当时是定价源）在换过网络的机器上要么超时要么被拦，表现为一批互不相关
    的用例「等不到」而各自单跑又是绿的——凡守护进程会主动访问的外部源都要指到进程内 stub
    （`MP_MODELSDEV_URL`、`MP_PRICING_URL`），`uiboot.mjs` 现在两者都覆盖。

## 回归要求

- 每个陷阱应有测试或明确的测试缺口。
- 修复回归时优先把规则归入对应专题文档；这里只保留跨模块摘要。
- 已迁入 `backend-contracts.md` 或 `web-api.md` 的细节不要在这里复制。
