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

`scripts/cover.sh [threshold] [--no-enforce]` 生成 `cov.out` 和 `coverage.html`，默认列出低于 60% 的函数，并按 80% package baseline gate。底层 `go test` 任一 package 失败、缺少预期 package 输出或缺少覆盖率百分比时，脚本必须非零退出，已有或不完整的 `cov.out` 不得形成假绿。

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

异步测试优先使用 channel、barrier、context deadline 或可观察状态同步；不得以固定 `Sleep` 证明异步工作“已经完成”或“没有发生”。流式读取必须设置 deadline，并同时断言读取错误、字节数和内容。

### 模块归属

叶子包的纯行为测试与实现放在同一模块目录；composition root 只保留跨模块行为和
HTTP/CLI 生命周期集成测试。例如 `internal/observe/events/hub_test.go` 精确断言
ring cap、detached snapshot、取消订阅、慢消费者丢弃和终态查询；根包只验证
forward/cache/Fusion 发布语义及 `/api/events` SSE 契约。测试不得为读取内部状态
而恢复根包 type alias、访问模块互斥锁或暴露 test-only 生产接口。

精确响应缓存的 key/store/recorder/header/replay 单测归
`internal/cache/*_test.go`；根包保留 force/pin bypass、协议转换后的
客户端字节、client cancel、reload generation、live event 与 Web status 集成
测试。缓存断言通过公开 `Stats` 与 HTTP 结果完成，不得读取内部 entry map/counter。

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
- 普通功能测试统一使用 `newTestProxy`；它在构造前注入独立 state path，并自动注册 `Proxy.Close`。需要验证重启恢复时使用 `newTestProxyAt` 显式共享同一个测试 state path，并在创建下一实例前关闭旧实例。仅验证生产构造器本身时可在隔离 HOME 下直接调用 `NewProxy`，并精确断言默认 state path。
- 不涉及凭据语义的根包行为测试使用 `testProviderID`，不得借用 `static` 形成对账号文件策略的隐式依赖。必须从 YAML 加载真实 `provider_id: static` 的 reload/CLI 测试，使用 `useStaticProviderPools` 写入隔离 HOME 下的 plural pool；static 凭据边界本身则精确断言 missing、legacy、损坏/空 plural 均 fail-closed。
- 禁止在测试日志输出真实 token、cookie、prompt 或用户请求体。


## 协议转换的三层边界测试

协议转换（`internal/protocol/convert*.go`）在普通单测之外有三层补充覆盖，修改转换器时按需运行：

1. **黄金文件回放**（`internal/protocol/convert_golden_test.go` + `testdata/wire/<proto>_<provider|场景>.sse`）：目录下每个原始 SSE 流（按文件名前缀选源协议）喂给所有以该协议为源的转换器，断言不变量而非精确输出——不 panic、输出可解析为 SSE 帧、终态事件恰好一个、responses 目标 `output_item.added/done` 按 id+type 配对（failed 终态豁免）、无空 data 帧；以及**场景存活断言**——输入流真实携带 tool call 或 reasoning 时，输出必须保留目标协议的对应形状。断言在压缩空白后匹配（zhipu 的 SSE JSON 带空格），标记精确化以防 `server_tool_use`、空 `tool_calls:[]` 误伤。种子为手写高保真流；真实上游流用 `model-proxy wire record <provider>` 录制进同一目录（每端点 text/_tool/_thinking 三场景；凭据来自 login，提交前人工审查脱敏，见 CLI.md §17）。
2. **Fuzz**（`internal/protocol/convert_fuzz_test.go` + `internal/protocol/testdata/fuzz/`）：`FuzzConvertRequest`（12 个请求/响应 converter 不 panic）、`FuzzConvertSSE`（6 个流式 transformer 不 panic、输出有界 `128×len+16KiB`，且终态唯一、clean/error 不混发、Responses added/done 配对）、`FuzzParseToolArgs`（确定性）、`FuzzSanitizeToolUseID`（确定性 + 字符集 `^[a-zA-Z0-9_-]+$`；空 id 的计数器占位是设计例外）。普通 `go test` 跑种子语料；真 fuzz：

   ```bash
   go test -fuzz=FuzzConvertRequest -fuzztime=20s -run '^$' ./internal/protocol
   go test -fuzz=FuzzConvertSSE -fuzztime=20s -run '^$' ./internal/protocol
   go test -fuzz=FuzzParseToolArgs -fuzztime=15s -run '^$' ./internal/protocol
   go test -fuzz=FuzzSanitizeToolUseID -fuzztime=15s -run '^$' ./internal/protocol
   ```

3. **差分测试**（`internal/protocol/convert_differential_test.go` + `internal/protocol/testdata/differential/`）：输入流逐字取自 opencodex 测试（fixture 顶部注释注明来源文件），断言语义等价；有意分歧处（`docs/decisions/intentional-behaviors.md` 第 10、11 条）按本仓语义断言并引用条目编号。

Fuzz 语料补充规则：`FuzzConvertSSE` 的 seed 阶段会从协议包读取模块根
`testdata/wire/*.sse`（≤64KiB）并全部加入语料；目录不可读或没有 `.sse` 时测试
必须失败，不能静默假绿。每次 `wire record` 录制的真实流自动成为 fuzz 输入，
无需手工同步。`internal/protocol/convert_golden_test.go` 还必须消费全部 `*.err`：
识别的 JSON error envelope 在每个跨协议目标下校验结构，HTML/空/未知 body
必须明确 fail-closed。

协议能力和语义边界还必须覆盖：已知不支持字段的客户端原生 400 与 compatible-target failover；tool_search_output 的 discovered tools 物化；citation 非流式/SSE 六方向；signed/redacted reasoning replay；prompt cache key/retention；previous_response_id 命中、miss 修复、TTL、重启恢复及仅 token-limit incomplete 可缓存。

## live e2e（真实上游，默认跳过）

`live_e2e_test.go` 用**真实 `./config.yaml` 和 login 管理的真实凭据**（`~/.model-proxy`）端到端验证协议转换在真实 vendor 方言上的表现（mock 覆盖不了的差异：thinking 方言、视觉门控、custom 工具、占位 reasoning_content）。

- 运行：`MODEL_PROXY_LIVE=1 go test -run 'TestLive_' -count=1 -timeout 10m .`。不设 `MODEL_PROXY_LIVE` 时全部 `t.Skip`，常规 `go test ./...` 保持 hermetic。
- 跳过逻辑：config/provider 缺失、未 login（无 runtime impl）→ skip；上游 429 / zhipu 资源包 1113 → skip（账号/资源问题，不是转换缺陷，注释区分）。其他 4xx/5xx 如实 FAIL。
- 路由换成测试给定的单目标（显式 `protocol:`，不依赖 wirecap verdict，避免 failover 干扰）；model 缺省取 provider.Models 最后一个（zhipu 旧模型受资源包限制会 429/1113，故取靠后的 glm-4.7）。
- 包级 TestMain 会把 HOME 重定向到临时目录，live helper 用包级 init 捕获的真实 HOME 还原后再构建 provider。
- 成本：每个用例都是真实付费调用，prompt 必须极短、max_tokens 给小值（≤512）。
- 安全：响应 body 不打全量（失败 excerpt ≤500 字符）；禁止输出 API key/token/凭据。

## 文档修改检查清单

修改 `AGENTS.md`（含目录级）或 `docs/` 下任何文档时，除 `git diff --check` 外逐项核对：

- 引用的每个文件路径真实存在（相对链接逐个点开）；
- 每个事实只有一个权威定义，其余位置是指针而非拷贝（根规则 5）；
- 不引入 `file.go:123` 式行号引用——行号必然腐烂，引用符号名/配置键/命令名；
- `docs/superpowers/` 的内容不得当作现状引用（历史档案，见其目录 AGENTS.md）；
- 新增专题前，确认现有权威文档确实无法承载（根规则 6）。
