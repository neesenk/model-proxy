# 日志可排查性增强设计（2026-11-16）

## 背景（运行日志分析结论）

对 `~/model-proxy/serve.log`（2026-09-11 起 1.71M 行，当前进程 2026-11-13 09:59 起 50,197 行）与 request-log index 的排查结论：

- 当前运行段健康：47,530 个上游 200；客户端可见失败仅 3×400 + 1×451；756 个失败腿（zcode 429×248、step-plan 429×441、502×67）全部被 failover 吸收。
- 缺口 1（排查力）：`target N (<provider>/<model>) failed; trying next`（forward.go:955）不带原因。普通 429/502 失败腿在 serve.log 完全无原因行（仅传输层错误有 `upstream error:` 行），原因只存在于 request-log index.db 的 AttemptOnly 行——纯 serve.log 排查无法回答"为什么失败"。
- 缺口 2（磁盘风险）：`~/.model-proxy/log/requests` 5.5 天累计 167GB（2026-11-12 单日 94.5GB）。`request_log.retention: 720h` 只按时龄删轮转文件，突发日速率下 4–5 天可写满空闲 379GB 的磁盘；无空间护栏。
- 缺口 3（无界增长）：serve.log 由 `OpenLogFile`（O_APPEND）写入，无轮转；supervisor 与 worker 共享继承的同一 fd，前台模式另有 MultiWriter 的第二个 O_APPEND fd。

## 方案

三个独立增强，全部复用既有机制，不新增依赖（x/sys 已有）。

### A. failover 腿带 status

`internal/forward/forward.go` 的 failover 警告行加入与 request-log AttemptOnly 行同源的 `result.AttemptStatus`：

```
[proto=openai model=deepseek-v4.1-flash] target 0 (zcode#…/glm-5.3-flash) failed (status 429); trying next
```

- 不重复 0→502 映射：status=0 表示传输层错误，其原因行（`upstream error:`）就在上方。
- context overflow 分支已有专属警告行，不变。

### B. serve.log 轮转（copy+truncate，缺省 128MB × 保留 4 份）

- 语义：worker 内后台任务（TransportTask，与其他后台任务同等 owner/stop/wait）每分钟 stat serve.log；size ≥ 上限 → 复制为同级 `<stem>-YYYYMMDD-HHMMSS.log` 归档（重名追加 `-N` 序号）→ truncate 原文件到 0 → 归档超出保留份数的最旧删除。copy 与 truncate 之间写入的行会丢（logrotate copytruncate 标准语义，注释写明）。
- 为什么 copy+truncate 而非 rename：所有写入方共享继承的 O_APPEND fd，rename 后写入继续落在旧 inode；truncate 保持 inode，O_APPEND 自然续写新末尾。
- 新 owner：`internal/cli/serve/logrotate.go`（`RunLogRotatorLoop(stop, path, maxBytes, keep)` + 可测的 `performRotation`/`pruneArchives`）。
- 接线：`cli_serve.go` runProxyProcess 在 worker 与前台两条路径统一 `ResolveLogFile` 后挂 TransportTask。
- 配置（顶层，startup-only 同 log_level；缺省 128MB/4，显式 0 = 不轮转）：
  ```yaml
  log_rotate_max_size: 134217728
  log_rotate_keep: 4
  ```
- `serve.log.pid` 派生、restart_serve.sh、login SIGHUP 不受影响（文件名不变）。

### C. request-log 磁盘护栏（缺省开启：空闲 <50GiB 或 使用率 ≥95%）

- 归属：`internal/observe/logfile`。既有每小时 sweep 在时龄删除后追加空间检查，候选集复用 Prunable 语义（只删 `prefix*.log` 轮转文件、按名排序最旧优先、活动文件与 index.db 永不删）。索引一致性沿用 prune 已声明机制（daemon reconcile / 启动 rebuild 丢弃消失文件的行）。
- 触发：unix `Statfs` / windows `GetDiskFreeSpaceExW`（x/sys）查询目录所在卷；`free < min_free_bytes` 或 `used/total*100 ≥ max_used_percent`。逐个删最旧轮转文件，每删一个重新查询，直到解除或删完；触发 pass 打一行摘要（`[request_log] disk guard: … deleted N files, freed X, free now Y`）。
- 范围：request_log 两条流（requests- 与 mcp-）各自守自己的目录；seclog 不加。
- 可测性：free-space 查询为包内窄 seam（Logger 字段，默认平台实现，测试注入伪值），不写真盘。
- 配置（request_log 块内，缺省 enabled/50GiB/95；条件值 0 = 该条件不触发；enabled: false = 整体关闭）：
  ```yaml
  request_log:
    disk_guard:
      enabled: true
      min_free_bytes: 53687091200
      max_used_percent: 95
  ```

## 测试与文档

- 回归：forward 失败腿警告含 `(status 429)`（仿 TestServeFailoverLegCarriesFailureEvidence，log.SetOutput 捕获，仓库已有先例）；logfile guard 注入伪 free-space 的行为测试（触发/解除/条件关闭/活动文件不删/删完仍不足告警）；serve 轮转 performRotation/pruneArchives 行为测试；config 缺省与覆盖测试。
- 平台：freespace_unix.go / freespace_windows.go build tags；验证追加 `scripts/build.sh linux/amd64`、`scripts/build.sh windows/amd64`。
- 文档：routing-and-failure.md（警告行新格式）、fusion-shadow-cache.md（request log 磁盘护栏契约）、CLI.md（serve.log 轮转与新配置键）、README.md、config.yaml 与 default_template.go 模板、pitfalls.md（共享 fd 下 rename 轮转陷阱条目）。
