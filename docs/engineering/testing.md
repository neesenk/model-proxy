# 测试与验证契约

## 适用范围

修改 Go 实现、构建脚本、并发状态机、Provider 或 Web UI 时按本文件选择验证范围。测试使用标准库 `testing`、`httptest`，不引入 testify。

## 基线命令

所有 Go 命令从 `model-proxy/` 运行：

```bash
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
gofmt -l .
```

覆盖率基线为每个 package 80%，由以下命令执行：

```bash
scripts/cover.sh
```

`scripts/cover.sh [threshold] [--no-enforce]` 生成 `cov.out` 和 `coverage.html`，默认列出低于 60% 的函数，并按 80% package baseline gate。

daemon process、浏览器 OAuth/SSO、交互 stdin 和真实上游 FetchModels 属于外部 I/O 路径，可通过集成验证覆盖，不强制全部单元化。

## 构建验证

```bash
scripts/build.sh
scripts/build.sh linux/amd64
scripts/build.sh --strip all
```

项目使用 pure-Go `modernc.org/sqlite`，跨平台构建为 `CGO_ENABLED=0`。涉及 build tags、平台探测、daemon 或文件路径时至少补 Linux/Windows amd64 build。

## 断言要求

### Auth headers

断言精确值，不只检查非空：

- codex：完整 Bearer、`originator=codex_cli_rs`、`ChatGPT-Account-Id`；
- deepseek/volcengine：两种协议路径都断言 Bearer 和 `x-api-key`。

删除任一 header 设置代码必须使测试失败。

### Quota windows

quota parser 测试必须断言：

- Ultimate / Short 标记；
- Duration；
- ResetsAt；
- 最终 RemainingPct 来源。

### 429 refresh

`refreshHook` 必须记录并断言具体 provider 名，不能只断言调用次数。

### Route side effects

forward 测试至少断言：

- 实际命中的 provider；
- 上游 JSON 中改写后的 model；
- 客户端 status/body；
- 需要时断言 failover metrics 或 health state。

### Stats buckets

宽 bucket 查询需要断言各 counter 的 SUM、`MAX(last_request_at)` 和 bucket start floor，并再次以 60 秒粒度查询，证明底层仍无损保存分钟数据。

### Concurrency

race-clean 只是必要条件。并发测试还必须断言功能不变量，例如 reload 期间旧请求完成且后续请求命中新配置。

## 禁止的弱测试

- 只有 `t.Logf`，没有断言；
- 错误的 `&&` / `||` 使单侧成功即可通过；
- 使用 `Contains(x) || Contains(y)`，panic stack 也可能误通过；
- 只断言“有请求”，不验证 model/header/status；
- 直接构造 `Config` 来代替 YAML 加载测试；
- 创建 `NewProxy` 后不隔离 HOME/state path 或不 cleanup；
- 时间竞态只跑一次，不做定向重复。

优先断言精确值或解析后的结构字段。

## 按改动范围追加

| 改动 | 追加验证 |
|---|---|
| Web JS | `node --check web_assets/app.js` |
| 并发、reload、持久化 | 定向 `go test -race -run ... -count=20` |
| build tags/平台代码 | Linux + Windows cross build |
| CLI 显示 | `CLI.md` 对应 stdout/stderr/exit code 测试 |
| Provider parser | 成功、错误、空 body、认证隔离 |
| SSE/转换 | client cancel、read error、trailing usage、逐字节输出 |

## 测试数据安全

- 测试不得写真实 `~/.model-proxy`。
- state、credential、request log 使用 `t.TempDir()` 或显式注入 path。
- 后台 owner 必须提供 stop/wait；测试通过 `t.Cleanup` 关闭。
- 禁止在测试日志输出真实 token、cookie、prompt 或用户请求体。


## 文档修改检查清单

修改 `AGENTS.md`（含目录级）或 `docs/` 下任何文档时，除 `git diff --check` 外逐项核对：

- 引用的每个文件路径真实存在（相对链接逐个点开）；
- 每个事实只有一个权威定义，其余位置是指针而非拷贝（根规则 5）；
- 不引入 `file.go:123` 式行号引用——行号必然腐烂，引用符号名/配置键/命令名；
- `docs/superpowers/` 的内容不得当作现状引用（历史档案，见其目录 AGENTS.md）；
- 新增专题前，确认现有权威文档确实无法承载（根规则 6）。
