# 日志可排查性增强实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** serve.log 单文件可回答 failover 原因（A）、serve.log 轮转有界（B）、request-log 磁盘护栏防写满盘（C）。

**Architecture:** A 在 forward.go 警告行附加 AttemptStatus；C 在 observe/logfile 的每小时 sweep 后追加空间检查（statfs 平台 seam + 包内注入测试）；B 为 cli/serve 新 TransportTask，copy+truncate 轮转。配置经 config 新键（disk_guard 子块 + log_rotate_* 顶层键，rawConfig 预置默认）。

**Tech Stack:** Go stdlib + golang.org/x/sys（已有）。

**Spec:** `docs/superpowers/specs/2026-11-16-log-diagnosability-design.md`

## Global Constraints

- 测试不写真盘空间、不 Sleep 证时序、不泄凭据；race-clean 必要条件。
- 未经用户要求不 commit/push。
- 验证门禁：`go vet ./...`、`go test ./... -count=1`、`go test -race ./... -count=1`、`gofmt -l .`、`scripts/static.sh`（dupl 阈值 120，不得新增 clone group）、`scripts/cover.sh`、`scripts/build.sh linux/amd64`、`scripts/build.sh windows/amd64`。
- 文档同步：routing-and-failure.md、fusion-shadow-cache.md、CLI.md、README.md、config.yaml、default_template.go、pitfalls.md。

---

### Task 1: failover 警告行带 status

**Files:**
- Modify: `internal/forward/forward.go:955`
- Test: `internal/forward/forward_test.go`

- [x] 失败测试：仿 `TestServeFailoverLegCarriesFailureEvidence`（forward_test.go:840）新增 `TestServeFailoverWarnCarriesAttemptStatus`：bad upstream 回 429，good 回 200；用 `log.SetOutput(&buf)`（先例 internal/app/request_log_test.go:618）在 `h.serve(...)` 期间捕获，断言含 `failed (status 429); trying next` 且响应 200。
- [x] 跑测试确认失败（当前行无 status）。
- [x] 实现：
```go
logx.Warnf("[proto=%s model=%s] target %d (%s/%s) failed (status %d); trying next",
	proto, exposed, ti, t.Provider, t.Model, result.AttemptStatus)
```
- [x] 跑 `go test ./internal/forward/ -run TestServeFailoverWarn -count=1` 通过。

### Task 2: logfile 磁盘护栏核心

**Files:**
- Create: `internal/observe/logfile/freespace_unix.go`、`internal/observe/logfile/freespace_windows.go`、`internal/observe/logfile/guard_test.go`
- Modify: `internal/observe/logfile/logfile.go`

**Interfaces（Produces）:**
- `logfile.Options` 新增 `MinFreeBytes int64`、`MaxUsedPercent int`（均 <=0 关闭该条件；双 0 整体关闭）。
- `Logger` 新增包内 seam 字段 `freeSpace func(dir string) (free, total int64, err error)`，默认平台实现 `volumeFreeSpace`。
- 包内函数 `rotatedFiles(dir, filePrefix, activeName string) ([]PruneTarget, error)`。

- [x] 失败测试（guard_test.go）：①min_free 触发删最旧至解除（伪 freeSpace=100/1000、MinFreeBytes=250、三个 100B 轮转文件 + 活动文件：删 2 个最旧、活动与其余保留）②percent 条件（used*100 >= total*pct）触发与不触发 ③双条件 0 不删 ④freeSpace 报错只告警不删 ⑤无可删候选（仅活动文件）告警且活动保留。
- [x] 跑测试失败确认。
- [x] 实现 freespace 平台文件（unix: `unix.Statfs`，free=Bavail*Bsize, total=Blocks*Bsize；windows: `windows.GetDiskFreeSpaceEx`），Options/Logger 字段、New 默认 seam、Run 的初始与 ticker 两处 sweep 后调 `l.guard(writer.Path())`、`guard`/`guardTriggered`/`rotatedFiles`（删除后按 `free += size` 累加判断，注释说明；消息用 `formatGiB` 助手）。
- [x] `go test ./internal/observe/logfile/ -count=1` 通过。

### Task 3: config schema（disk_guard + log_rotate_*）

**Files:**
- Modify: `internal/config/config.go`（Config、rawConfig、parse、validate）、`internal/config/default_template.go`
- Test: `internal/config/config_test.go`（新增用例）

**Interfaces（Produces）:**
- `RequestLogConfig.DiskGuard DiskGuardConfig`（`Enabled/MinFreeBytes/MaxUsedPercent`；`GuardArmed() bool`）；常量 `DefaultDiskGuardMinFreeBytes = 50 << 30`、`DefaultDiskGuardMaxUsedPercent = 95`；rawConfig 预置默认（absent → true/50GiB/95，Guard 同款 bool 预置模式）。
- `Config.LogRotateMaxSize int64`（yaml:"-"，parse 应用默认 128MiB；显式 0=不轮转；负值 parse 报错）、`Config.LogRotateKeep int`（默认 4，负值报错）；rawConfig 用 `*int64`/`*int` 区分 absent 与显式 0。

- [x] 失败测试：缺省值（disk_guard true/50GiB/95；rotate 128MiB/4）、显式覆盖、`enabled: false`、`min_free_bytes: 0`、`log_rotate_max_size: 0`（off）、负值报错文案含键名。
- [x] 实现上述字段与逻辑；模板 default_template.go 加注释化键示例。
- [x] `go test ./internal/config/ -count=1` 通过。

### Task 4: 接线护栏到两条 request-log 流

**Files:**
- Modify: `internal/observe/requestlog/logger.go`（Options 透传）、`internal/app/observe_adapters.go`（initRequestLog）
- Test: `internal/app/observe_adapters_test.go` 或就近已有 request_log 测试文件补用例

- [x] 失败测试：`initRequestLog` 构造的 reqLog options 带上 armed 护栏值（armed=false 时 0/0）——若结构不可直接观察则断言 `[request_log] enabled` info 行含 `disk guard min_free=…`（log.SetOutput 捕获）。
- [x] 实现：requestlog.Options 增两字段透传 logfile；initRequestLog 计算 `minFree/maxUsed`（GuardArmed 才传），info 行追加 `, disk guard min_free=%s max_used=%d%%`。
- [x] `go test ./internal/app/ -run RequestLog -count=1` 通过。

### Task 5: serve.log 轮转

**Files:**
- Create: `internal/cli/serve/logrotate.go`、`internal/cli/serve/logrotate_test.go`
- Modify: `cli_serve.go`（runProxyProcess：提升 logFile 解析、挂 TransportTask）

**Interfaces:**
- Consumes: Task 3 的 `cfg.LogRotateMaxSizeBytes()`（返回 0=off）与 `cfg.LogRotateKeepFiles()`。（实现为 Config 方法或直接字段访问，与 Task 3 保持一致。）
- Produces: `RunLogRotatorLoop(stop <-chan struct{}, path string, maxBytes int64, keep int)`、包内 `performRotation(path string, keep int, now time.Time) (archive string, err error)`、`pruneArchives(path string, keep int) ([]string, error)`。

- [x] 失败测试：①performRotation：归档内容等于原文、原文件 truncate 0 ②归档重名加 `-N` 序号 ③pruneArchives 保留最新 keep 份 ④stop 已关闭时 loop 经 done channel 立即返回（deadline 守卫非 Sleep）。
- [x] 实现：copy（`io.Copy`，O_CREATE|O_EXCL 0644）→ `os.Truncate(path,0)` → prune（stem-*.log 最旧优先）；注释写明共享 O_APPEND fd 下为何不能 rename、copy/truncate 间丢行语义；loop 每分钟 stat，size ≥ maxBytes 才转，`[logrotate]` info/warn 行用 MiB 粒度 humanBytes。
- [x] cli_serve.go：`logFile := cliserve.ResolveLogFile(sa, cfg)` 提升到前台分支之前，前台镜像块复用之；`newApplicationRuntime` 后，`rotateMax > 0 && logFile != ""` 时 append TransportTask 跑 RunLogRotatorLoop（与其他 TransportTask 同等 stop/wait）。
- [x] `go test ./internal/cli/serve/ -count=1` 通过；`go test . -count=1`（根包 serve 装配）通过。

### Task 6: 文档同步与全量验证

**Files:**
- Modify: `docs/architecture/routing-and-failure.md`（警告行新格式）、`docs/architecture/fusion-shadow-cache.md`（request log 磁盘护栏契约）、`CLI.md`（serve.log 轮转 + 新配置键）、`README.md`（如含 request_log/日志键则同步）、`config.yaml`（新键注释示例）、`docs/engineering/pitfalls.md`（共享 fd 下 rename 轮转陷阱 + 磁盘护栏条目）
- Test: `docs/` 检查清单（若 testing.md 末尾有 docs 验收项则执行）

- [x] 各文档对应小节更新（一个事实一个权威定义：护栏契约在 fusion-shadow-cache.md，pitfalls 只记跨模块陷阱）。
- [x] 全量验证：`git diff --check`、`go vet ./...`、`go test ./... -count=1`、`go test -race ./... -count=1`、`gofmt -l .`、`scripts/static.sh`、`scripts/cover.sh`、`scripts/build.sh linux/amd64`、`scripts/build.sh windows/amd64`。
- [x] 汇总：不 commit（仓库规则），改动清单交用户。

## Self-Review

- 覆盖：spec A→Task1、C 核心→Task2、配置→Task3、接线→Task4、B→Task5、文档验证→Task6。无遗漏。
- 类型一致：`LogRotateMaxSizeBytes()`/`LogRotateKeepFiles()` 在 Task 3 产出、Task 5 消费；`volumeFreeSpace` 仅包内。
- 无占位符：所有代码步骤含实际代码或精确前后对照。
