# 测试与验证契约

## 适用范围

修改 Go 实现、构建脚本、并发状态机、Provider 或 Web UI 时按本文件选择验证范围。测试使用标准库 `testing`、`httptest`，不引入 testify。

## 基线命令

所有 Go 命令从仓库根目录运行：

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

并发密集 package（per-request snapshot、reload swap、SSE fan-out、后台 flusher 所在的 `internal/app`、`internal/runtime/...`、`internal/targetexec`、`internal/fusion`、`internal/cache`、`internal/shadow`、`internal/guard`、`internal/accounts`、`internal/observe/...`）的快速 race 门禁由以下命令执行，适合提交前快速验证；全量 `go test -race ./...` 仍是权威门禁，不可被它替代：

```bash
scripts/race.sh
```

`scripts/cover.sh [threshold] [--no-enforce]` 生成 `cov.out` 和 `coverage.html`，默认列出低于 60% 的函数，并按 80% package baseline gate。底层 `go test` 任一 package 失败、缺少预期 package 输出或缺少覆盖率百分比时，脚本必须非零退出，已有或不完整的 `cov.out` 不得形成假绿。无可覆盖语句的纯测试包（`[no statements]`，如 `internal/archtest`）是合法例外；有生产 statements 但无测试文件的包会打印裸 `coverage: 0.0%` 行，默认必须判失败，只有组合入口等确实无需包内单测的 package 才能显式列入 `scripts/cover.sh` 的 `no_test_exemptions`（当前仅根 package main）。历史上尚未达到 80% 的 package 不再使用可降到 0% 的 blanket exemption，而在 `coverage_floor_for` 中逐包记录明确 floor；低于 floor 必须失败，达到 floor 但低于 80% 显示 `gap`，新增可靠测试后同步提高 floor。`scripts/cover.sh --self-test` 校验 coverage 行解析、普通 package 的 80% 合同，以及所有历史 floor 都拒绝 0% 和 `floor - 0.1%`。

普通 package coverage 是包内测试视角；composition/integration 测试对 owner package 的执行归因需要按需使用 `go test -coverpkg=<owner packages> <test packages>` 复核，不能用跨包执行率替代逐包 floor。daemon process、浏览器 OAuth/SSO、交互 stdin 和真实上游 FetchModels 属于外部 I/O 路径；其控制状态机必须通过包内窄接口、可取消等待或受控 subprocess/fake 验证，不得为 coverage 向外暴露无约束 production hook，也不得向真实用户进程发送信号。

## 构建验证

```bash
scripts/build.sh
scripts/build.sh linux/amd64
scripts/build.sh --strip all
```

项目使用 pure-Go `modernc.org/sqlite`，跨平台构建为 `CGO_ENABLED=0`。涉及 build tags、平台探测、daemon 或文件路径时至少补 Linux/Windows amd64 build。

发布流水线（`.goreleaser.yaml` + `.github/workflows/release.yml`，tag `v*` 触发）改动后跑 `goreleaser check` 与 `goreleaser release --snapshot --clean` 验证，勿推送 snapshot 产物。

## Benchmark 约束

`go test ./...` 不执行 benchmark，坏掉的基准（fixture 失效、断言 Fatalf、测到错误路径）会长期静默失真——已有教训：四个转发基准曾因 nil model_map 一直在测"无路由错误路径"。规则：

- 改动涉及基准覆盖的性能敏感路径（转发、转换、quota/调度、request log）时，追加 smoke：`go test ./... -run='^$' -bench=. -benchtime=1x`，必须全绿。
- 转发类基准若经 HTTP round-trip，必须断言响应 status==200（否则可能在测错误路径）；断言语义必须可观察（如提取出的 model 值），不能只跑循环。
- 基准 fixture 必须代表真实流量形态（如 LLM 客户端的 model 是首个 key）；偏离现实的 fixture 会逼出错误的优化方向。

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

使用可控 fake provider 的真实 `Quota()` 调用观察 refresh，必须记录并断言具体
provider 名，不能只断言调用次数；不得为此在生产结构体增加 test-only hook。

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

### 状态机

新状态机至少覆盖成功、硬失败、限频、取消和 reload/重启。

### 模块归属

叶子包的纯行为测试与实现放在同一模块目录；composition root 只保留跨模块行为和
HTTP/CLI 生命周期集成测试。例如 `internal/observe/events/hub_test.go` 精确断言
ring cap、detached snapshot、取消订阅、慢消费者丢弃和终态查询；根包只验证
forward/cache/Fusion 发布语义及 `/api/events` SSE 契约。测试不得为读取内部状态
而恢复根包 type alias、访问模块互斥锁或暴露 test-only 生产接口。

根包不再保留测试文件。CLI 子命令与进程生命周期集成测试归 `internal/cli`
（os.Exit/log.Fatal 命令经 subprocess harness 覆盖：共享实现在
`internal/cli/clitest`（`HelperProcess` / `RunCLI*` / 共享 fixture），每个命令包以
自己的 `TestHelperProcess` 注册本包 handler）；`app.NewRuntime`
装配行为测试归 `internal/app/runtime_assembly_test.go`；架构 AST 契约测试归
`internal/archtest`（纯测试包，经 `repoRoot` 定位模块根，调用点写模块根相对路径）。
跨文件共享 fixture 只保留在各包的 `*_test_support_test.go`，不得复制 helper 或
把 test-only hook 塞回生产结构。一个文件只覆盖一个清晰领域时不按行数强拆；
当同一 catch-all 文件混合 HTTP status、配置、账号、登录等独立契约时，必须按
领域拆分，并保持原测试名、断言与 cleanup 语义。

Provider 的 `Usage`、`Quota`、fetch/parse、认证和显示格式测试直接归
`internal/provider/*_test.go`；根包只验证 YAML/账号池/build dispatch/CLI 输出等组合行为，
不得在 `_test.go` 重建已删除的 `show*Usage` / `fetch*Quota` 兼容函数后重复测试。

### 架构 DAG 与交互合同

`internal/archtest/architecture_dependency_dag_contract_test.go` 必须枚举全部生产 `internal` package，并按
`docs/architecture/overview.md` 的闭合 allowlist 检查直接仓库依赖和无环性。新增
`internal` 目录必须显式分类；不得通过跳过目录、只扫描部分文件或给未知 package
隐式空规则形成假绿，也不得使用 dot/blank repository import 绕过 matcher。依赖变更
应缩小或保持边界；确需扩大时先更新权威架构契约并说明 owner 关系。

架构交互 guard（`internal/archtest`）必须解析 Go AST/类型形状，断言构造器或 mutable owner 的精确生产
callsite；不能用注释/字符串包含、只断言“至少一次”，也不能只检查预期文件而不扫描
其他生产文件。受保护 owner 符号的引用必须保持已审查的直接形态：root 函数只允许
裸标识符调用，imported 构造器只允许 `pkg.F(...)`，imported 类型只允许 `pkg.T{...}`
或直接函数签名引用，受保护方法只允许 `receiver.Method(...)`。function value
（`f := pkg.NewPlan`）、method value（`fn := runtime.Execute`）、type alias
（`type E = pkg.Executor`）与 method expression（`(*pkg.T).Execute`）都计入引用点
集合，从而破坏 owner 唯一性断言而失败。通用 guard helper 要有 synthetic
positive/negative control，证明它既能接受合法图/调用点，也能抓到未知 package、越级
边、环、重复 owner 或上述 alias/method-value 绕过。跨调用的数据流合同必须从构造/执行赋值推导接收者，不能硬编码局部变量名或仅
比较词法先后；commit 后派发必须绑定同一个 executor result，reload-owned runtime
必须捕获一次并贯穿采样、准入和异步执行。静态合同只防止结构漂移，不能替代对应
HTTP/CLI、reload-generation、shutdown order 和并发行为测试。

Web transport 的 HTTP routing、JSON presentation、asset serving、login session
store 和 task owner 测试归 `internal/web/*_test.go`；根包只保留真实应用
`ReadAPI` / `CommandAPI` 适配、mux composition 及 Web 与 daemon/Proxy lifecycle
的集成行为。测试通过端口和 HTTP 结果断言，不得让 `internal/web` import root 或为
读取 session/task 内部状态恢复 root-private hook。

精确响应缓存的 key/store/recorder/header/replay 单测归
`internal/cache/*_test.go`；根包保留 force/pin bypass、协议转换后的
客户端字节、client cancel、reload generation、live event 与 Web status 集成
测试。缓存断言通过公开 `Stats` 与 HTTP 结果完成，不得读取内部 entry map/counter。

请求日志的 Record 构造、header allowlist、writer rotation/权限、异步 drain/drop、
retention、top-K 查询、Summary 脱敏与 Shadow 聚合单测归
`internal/observe/requestlog/*_test.go`；通用有界流捕获归
`internal/transport/bodycapture/reader_test.go`。根包只保留原始请求体与上游改写
body 的映射、协议转换后客户端字节、HTTP list/detail 脱敏、replay 拒绝截断、
Fusion/Shadow 记录以及 shutdown drain 顺序的集成测试。列表测试必须同时断言
request body、response body、response headers 均不出现，不能只检查其中一项。

Fusion 的 registry/budget、quorum/grace collection、judge/synthesis body、
usage/rune helper 和 Engine gate/fan-out 单测归 `internal/fusion/*_test.go`；
Shadow 的 sampling、concurrency gate、detached request rewrite/auth/header、
fail-closed conversion 与 bounded capture 单测归
`internal/shadow/*_test.go`。根包只保留真实 resolver/target plan、reload
generation、target policy、stream/client response、metrics/events/request log
和 Shadow-before-drain 集成；不得为检查 semaphore、registry ring 或随机数而
暴露内部字段或恢复根兼容类型。

SQLite stats 的 schema/additive migration、legacy import、minute/agent upsert、
retention、raw/wide/calendar 查询与 query plan 测试归
`internal/observe/stats/*_test.go`。根包只保留 metrics/tokens/agents → flusher
投影、失败分钟批次重试、reset-vs-flush、Proxy final flush/Store close、HTTP JSON
shape 和 CLI 显示集成。宽 bucket 必须逐字段断言所有 additive counter、
`MAX(last_request_at)`、平均值与 bucket floor；shutdown 测试需重开数据库证明
final delta 恰好落盘一次，并覆盖一次瞬时 final-flush 失败后的 retry window；
持续失败测试必须证明 pending batch 有界、最早时间边界稳定且合并后累计量不丢；
SQLite 写锁测试必须证明 Store 的短 busy timeout 将单次锁等待限制在 1 秒内，
另以可取消 sink 证明 shutdown context 的 retry window。reset 测试需证明历史与
三个 counter 同步清零且首个 post-reset delta 不重复。Stats/Agents/Analytics
HTTP 测试必须覆盖筛选参数接线、nil Store 空数组与 Store 错误 500，CLI
`--json` 必须断言响应 body 字节级透传。

wire capability 的 verdict JSON、freshness、HTTP status 分类、协议选择矩阵、
detached snapshot、404 纠正和按当前 provider/base URL 恢复的纯测试归
`internal/runtime/wirecap/*_test.go`；根包只保留真实 HTTP probe、认证/header、
boot/reload、forward 协议选择、runtime 404 纠正与持久化 round trip。恢复测试
必须覆盖“未知 parent + 空 base URL”不得被缺省 map lookup 误接纳。

generation-scoped health、sticky、pin、model lock、paramBlock、spread、quota、
schedule、persist/dashboard snapshot 的纯状态机测试归
`internal/runtime/manager*_test.go`。必须精确断言 generation 替换清理集合与 pin
保留、旧 generation mutation 拒绝、half-open 生命周期、429 horizon/kind、
model/param 隔离、quota 嵌套 slice 深拷贝、schedule commit gate、resolver spread
gate、quota projection 与 health/pin/sticky/spread 的单锁决策、Dashboard
PreviewOrder 的 order/sticky/facts parity 与无 mutation，以及并发 snapshot 的
generation 与内容不会混代。根包只保留真实
forward/Fusion/reload/HTTP/CLI/persistence/quota poll 编排；集成测试通过公开行为
或 detached snapshot 断言，不得访问 Manager mutex 或恢复第二份内部 map。

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
| Web JS | `node --check internal/web/assets/app.js internal/web/assets/pure.js` + `node --test internal/web/jstests/pure.test.mjs`（Go 侧 `TestWebAssets*` 驱动；无 node 时 Skip，`MP_REQUIRE_NODE=1` 时缺 node 必须 FAIL，CI 以该开关运行） |
| 并发、reload、持久化 | 定向 `go test -race -run ... -count=20` |
| build tags/平台代码 | Linux + Windows cross build |
| CLI 显示 | `CLI.md` 对应 stdout/stderr/exit code 测试 |
| Provider parser | 成功、错误、空 body、认证隔离 |
| SSE/转换 | client cancel、read error、trailing usage、逐字节输出 |
| 性能敏感路径 | `go test ./... -run='^$' -bench=. -benchtime=1x` smoke（见「Benchmark 约束」） |

## 测试数据安全

- 测试不得写真实 `~/.model-proxy`。
- state、credential、request log 使用 `t.TempDir()` 或显式注入 path。
- 后台 owner 必须提供 stop/wait；测试通过 `t.Cleanup` 关闭。
- 普通功能测试统一使用 `newTestProxy`；它在构造前注入独立 state path，并自动注册 `Proxy.Close`。需要验证重启恢复时使用 `newTestProxyAt` 显式共享同一个测试 state path，并在创建下一实例前关闭旧实例。仅验证生产构造器本身时可在隔离 HOME 下直接调用 `NewProxy`，并精确断言默认 state path。
- 不涉及凭据语义的根包行为测试使用 `testProviderID`，不得借用 `static` 形成对账号文件策略的隐式依赖。必须从 YAML 加载真实 `provider_id: static` 的 reload/CLI 测试，使用 `useStaticProviderPools` 写入隔离 HOME 下的 plural pool；static 凭据边界本身则精确断言 missing、legacy、损坏/空 plural 均 fail-closed。
- 禁止在测试日志输出真实 token、cookie、prompt 或用户请求体。


## 协议转换的三层边界测试

协议转换（`internal/protocol/convert*.go`）在普通单测之外有三层补充覆盖，修改转换器时按需运行：

1. **黄金文件回放**（`internal/protocol/convert_golden_test.go` + `testdata/wire/<proto>_<provider|场景>.sse`）：目录下每个原始 SSE 流（按文件名前缀选源协议）喂给所有以该协议为源的转换器，断言不变量而非精确输出——不 panic、输出可解析为 SSE 帧、终态事件恰好一个、responses 目标 `output_item.added/done` 按 id+type 配对（failed 终态豁免）、无空 data 帧；以及**场景存活与语义断言**——输入流真实携带 tool call 或 reasoning 时，输出必须保留目标协议的对应形状；输入含非空正文或 reasoning 文本时，目标协议必须逐字保留；输入含正数 terminal usage 时，按各协议的 inclusive input/cache 口径归一后 input/output 必须精确相等，不允许按转换方向整体豁免。形状断言在压缩空白后匹配（zhipu 的 SSE JSON 带空格），标记精确化以防 `server_tool_use`、空 `tool_calls:[]` 误伤。种子为手写高保真流；真实上游流用 `model-proxy wire record <provider>` 录制进同一目录（每端点 text/_tool/_thinking 三场景；凭据来自 login，提交前人工审查脱敏，见 CLI.md §17）。
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

## soak 压测工具（`scripts/soak`，手动运行）

`go run ./scripts/soak` 是对**运行中的代理**做闭环稳态压测的工具（borrow 自
Switchyard soak 的场景思路）：7 个可复现场景（short 非流式、stream SSE 到 EOF、
longctx ≈48KiB 上下文、tools 工具流量、prefix 字节相同重复、errors 未知模型 502
风暴、cancel 200ms 客户端中途断开），`-scenario mix` 全跑，输出每场景
ok/err/错误率/p50/p95/p99/流式 TTFT/状态码分布，`-max-error-rate` 超阈退出非零
（error/cancel 场景按预期结果反转不计错）。场景截止时刻在途的请求标记 `aborted`
并剔除统计。它发送**真实请求**——`-model` 解析到的路由会消耗真实上游配额，长跑
前先指向 mock/dev 后端。工具自身的场景构造器与 harness 循环由
`scripts/soak/main_test.go` 用本地 httptest stub 验证（hermetic，不碰网络），属于
`go test ./...` 常规矩阵；race gate 覆盖其并发汇总。

## live e2e（真实上游，默认跳过）

`internal/app/live_e2e_test.go` 用**仓库根 `config.yaml`（可由 `MODEL_PROXY_LIVE_CONFIG` 覆盖）和 login 管理的真实凭据**（`~/.model-proxy`）端到端验证协议转换在真实 vendor 方言上的表现（mock 覆盖不了的差异：thinking 方言、视觉门控、custom 工具、占位 reasoning_content）。override 的绝对路径原样使用，相对路径按仓库根解析，不依赖 `go test` 的 package working directory。

- 运行：`MODEL_PROXY_LIVE=1 go test -run 'TestLive_' -count=1 -timeout 10m ./internal/app`。不设 `MODEL_PROXY_LIVE=1` 时全部 `t.Skip`，常规 `go test ./...` 保持 hermetic。
- 失败/跳过逻辑：显式启用 live 后 config 路径无法解析、文件缺失或内容无效 → FAIL（禁止整套假绿）；provider 缺失、未 login（无 runtime impl）→ skip；上游 429 / zhipu 资源包 1113 → skip（账号/资源问题，不是转换缺陷，注释区分）。其他 4xx/5xx 如实 FAIL。
- Responses round-trip 必须统一解析 SSE，并断言恰有一个 `response.completed`，且 `response.incomplete` / `response.failed` / error 为零；不得用 raw substring 或把 incomplete 当成功。强制工具场景还必须断言 call id/name/arguments 非空、arguments 为合法 JSON 且 `city` 精确为 `Paris`，禁止测试侧补默认参数。转换结果缺 reasoning/signature 等协议字段属于 FAIL，只有上条列出的外部前置条件可 skip。
- 路由换成测试给定的单目标（显式 `protocol:`，不依赖 wirecap verdict，避免 failover 干扰）；model 缺省取 provider.Models 最后一个（zhipu 旧模型受资源包限制会 429/1113，故取靠后的 glm-4.7）。
- 包级 TestMain 会把 HOME 重定向到临时目录，live helper 用包级 init 捕获的真实 HOME 还原后再构建 provider；配置始终通过上面的显式绝对路径加载。
- 成本：每个用例都是真实付费调用，prompt 必须极短、max_tokens 给小值（≤512）。
- 安全：响应 body 不打全量（失败 excerpt ≤500 字符）；禁止输出 API key/token/凭据。

## 文档修改检查清单

修改 `AGENTS.md`（含目录级）或 `docs/` 下任何文档时，除 `git diff --check` 外逐项核对：

- 引用的每个文件路径真实存在（相对链接逐个点开）；
- 每个事实只有一个权威定义，其余位置是指针而非拷贝（根规则 5）；
- 不引入 `file.go:123` 式行号引用——行号必然腐烂，引用符号名/配置键/命令名；
- `docs/superpowers/` 的内容不得当作现状引用（历史档案，见其目录 AGENTS.md）；
- 新增专题前，确认现有权威文档确实无法承载（根规则 6）。
