# 请求日志 zstd 冷层设计（30/7 双窗口）

- 日期：2026-11-22
- 状态：已评审（用户拍板：zstd 归档、索引只保热窗、归档离线分析、读路径零功能变更）
- 上游背景：2026-11-17 `cc2fb68`（流应答聚合落盘，实测折叠 38–44x）之后，请求体成为请求日志的主要字节源（2026-11-22 410MB 文件中 request+response body 占 89%，元数据仅 2%）

## 背景与数据

- 正常日（2026-11-21）225.7MB → 30d retention 稳态 ≈ 6.8GB；异常日（2026-11-22，单个 1,927 请求的 agent 会话占请求字节 47%）半天 410MB。
- 放大结构是「次数 × 中等体积」（p50=5KB、p99=80KB、零条触顶 5MB cap），不是单条巨大。
- **已否决的替代方案**：会话内请求体去重/前缀增量——数学上限仅 2.3x（sum(req_size)=261MB vs per-session max=115MB），摊到整文件 ~1.4x，却要付跨记录引用、Detail 重建、扫描/索引语义一致、retention 耦合的复杂度。
- **已否决的解压路径方案**：归档在线可读（zstd + 行号偏移映射或按需解压）——破坏 `(file, offset, length)` seek、目录扫描回退、jq/grep 明文可读性，省下的复杂度全花在解压层。

## 决策

用户决策三条：

1. 归档（zstd）文件的索引行删除——索引只覆盖热窗。
2. **读路径不做任何功能变更**——归档数据视为离线分析用（`zstd -d < requests-X.log.zst | jq ...`）。
3. 配置两个时长：`retention`（总留存，默认 720h=30d）+ 新增 `hot_retention`（热窗，默认 168h=7d）。

### 现有机制红利（读路径零变更的依据）

三处文件过滤都是 `prefix + ".log"` 后缀精确匹配，`.zst` 天然不被认领：

- indexer 文件认领：`internal/observe/requestlog/index_files.go:103`
- reconcile 目录扫描：`internal/observe/requestlog/index.go:1035`
- 查询扫描回退：`internal/observe/requestlog/query.go:363`

原 `.log` 被删除后，indexer 现有「未认领的消失文件删行」逻辑自动清索引行并按 session 失效增量缓存（cc2fb68 引入）。时龄基准沿用 sweep 的 mtime（`internal/observe/logfile/logfile.go:295`）；轮转 rename 保留最后写入时间。

归档命名：活动文件 `requests-YYYYMMDD.log`，size 轮转 `requests-YYYYMMDD--HHMMSS-<seq>.log`（`logfile.go:559`），二者同规则处理。

## 机制

### 压缩 worker（唯一新功能代码，全在 `internal/observe/logfile`）

- **独立 goroutine**，owner = logfile.Logger。不在 writer goroutine 上执行压缩——1GB 文件 zstd-3 需几十秒，会堵队列 drain。
- sweep tick（每小时）选出 mtime 老于 `HotRetention` 的归档文件（非活动文件），交给 worker；一次压一个。
- **原子序列**：写 `X.log.zst.tmp` → rename 为 `X.log.zst` → 删除原 `X.log` → 触发 `OnChange`（indexer 在下一拍 reconcile 清行）。
- **崩溃恢复幂等**：`.zst` 已存在 → 跳过压缩、直接删原文件；残留 `.tmp` 下轮清理。
- **生命周期**（AGENTS.md 红线 2）：stop/wait/final flush——Shutdown 停止派发，worker 完成当前文件后退出。
- 压缩级别 zstd level 3（SpeedDefault）；纯 Go 实现（`github.com/klauspost/compress/zstd`，新增唯一直接依赖，过 govulncheck 门禁）。

### retention / disk_guard 扩展

- `sweep` 与 `guard` 的可删集合纳入 `.zst`（oldest-first by mtime）；guard 现按 `isLogFile`（prefix+`.log`）匹配，需扩展匹配 `.zst`。
- `HotRetention <= 0` = 禁用压缩（全热，行为与现状一致）；校验 `HotRetention < Retention` 否则配置 fail-closed。

### 配置

```yaml
request_log:
  retention: 720h   # 总留存（含义不变，30d 后删除，热/冷同此）
  hot_retention: 168h  # 新增：热窗，滚出即 zstd；0 = 禁用；restart-only
```

- 代码默认 `168h`；restart-only（与 `aggregate_streams`、`mcp_split` 同节奏）。
- MCP 流共享同策略（`requestlog.Options` → logfile）；seclog 不接线不受影响。

## 用户可见语义变化（红线 6）

Requests 页 / `/api/requests` / `/api/sessions` 成本汇总 / Trace / Detail / Replay 的**可查窗口从 30d 缩到 7d**（热窗）。更早数据离线分析。`/api/sessions` 对跨热窗的老 session 只聚合热窗内部分（与扫描回退降级语义同构，现在是常态而非降级）。

文档同步：`README.md`、`config.yaml`、`docs/web-api.md`、`docs/architecture/fusion-shadow-cache.md`。

## 量级预估

- 稳态（正常日 250MB/天、zstd 对重复会话 JSON 压 ~8x）：热 7×250MB ≈ 1.7GB 明文 + 23×~31MB ≈ 0.7GB 压缩 ≈ **2.4GB**（现 6.8GB）。
- 异常日持续（800MB/天）：≈ 6.5GB（现 24GB）。
- 改动面：go.mod +1 依赖；logfile ~150 行 + 测试；config 校验；app 接线 ~10 行；requestlog 零功能代码（仅测试锁定）；文档 4 处；测试 ~400 行。

## 测试计划（测试核心约定）

- logfile：fake clock 驱动热转换时序（无 Sleep）；原子序列与崩溃恢复（`.zst`+`.log` 并存、残留 `.tmp`）；worker stop/wait/final flush；sweep/guard 删除 `.zst` oldest-first；`HotRetention=0` 禁用。
- requestlog：`.zst` 不被三入口认领；原文件消失后索引行删除 + per-session 缓存失效；Detail 对已归档 id 走 seek 失败 → tailScan miss 的既有优雅降级。
- config：`hot_retention >= retention` fail-closed；默认值。
- app/composition：config → logfile Options 接线；restart-only 语义。

## 验证

按 AGENTS.md 验证矩阵：`git diff --check`、`go vet ./...`、`go test ./... -count=1`、`go test -race ./... -count=1`、`gofmt -l .`、`scripts/static.sh`、`scripts/cover.sh`；平台追加 Linux/Windows 构建；文档检查清单。
