# Fusion、Shadow、Cache 与实时观测

## 适用范围

修改 `fusion.go`、Shadow、request log、`internal/cache`、
`internal/transport/bodycapture`、live events 或 replay 时必读。API 字段另见
`docs/web-api.md`。

## 精确响应缓存

缓存默认关闭：`cache.enabled/ttl/max_entries/max_body_bytes`，默认 TTL 10m、1000 条、单 body 256 KiB。

- key 是 method、path、原始请求 body 的 SHA-256；
- 只缓存 `<300`、完整读到 EOF、未超过 body cap 的响应；
- 客户端断开或半截响应不得入缓存；
- 转换后的响应删除旧 Content-Length/Transfer-Encoding 后保存；mode mismatch
  重写响应时保存的是 live 路径实际发送的 content-type（客户端最终收到的
  协议体与 content-type 一致）；
- 命中重放原始客户端协议字节并设置 `x-mp-cache: hit`；
- cache hit 不计 provider metrics/agent stats，但产生 live end event；
- pin 和 force-provider 跳过读写缓存；
- reload 重建缓存并清空条目。

缓存机制由 `internal/cache` 叶子包拥有：request key、TTL/容量 store、
bounded recorder、转换后 header normalization 与逐块 flush replay。根
`cache_adapter.go` 只注入配置生效值；请求通过 `runtimeSnapshot.cache` 保持
generation 隔离，reload 后旧请求即使完成也只能写入旧 Store。

缓存定位是重复请求/重试盾牌，不是多轮对话前缀缓存。

## Live events

`internal/observe/events.Hub` 保存最近 200 条事件并做非阻塞 fan-out。慢订阅者
丢事件，不能反压请求路径；根 `live_events.go` 只负责 SSE/keepalive 适配。

forward 产生 start/end，包含 agent、protocol、provider、status、latency、tokens 和稳定 request_id。cache hit、400/502 终局也必须产生 end。`GET /api/events` 先重放 ring，再推送 SSE，15 秒 keepalive。

## Shadow

每 route 可配置 shadow provider/model/protocol/sample_rate/max_concurrent。生产响应 commit 后 fire-and-forget：

- sample rate 的 nil 表示 1.0，显式 0 表示关闭；
- semaphore 满时丢弃 shadow；
- shadow 使用自身 protocol/baseURL；
- 不得影响熔断、sticky、生产 metrics 或响应；
- 结果进入 request_log，id 以 `shadow-<primary-id>` 配对；
- reload 必须让一次 dispatch 全程使用同一 generation 的 runtime、target、provider map 和 client。

单目标 executor 只返回含实际上游请求体的最小 `attemptCommit`，不持有 Shadow
或 lifecycle 回调。`serveOnce` 在确认 commit 后执行 sampling、semaphore 和
lifecycle admission，并在启动 goroutine 前同时捕获 `runtimeSnapshot` 与
`shadowRuntime`。Shadow 随后与普通 route/Fusion 共用 `targetPlan` 完成 provider
config/runtime impl、backend protocol、model rewrite、转换和 URL/path；goroutine
内禁止重新读取 `p.cfg`/`p.providers`/`p.catalog` 或再次 load `p.shadow`。Fusion
synthesizer 不经过这条 post-commit hook，禁止递归派发 Shadow。

Shadow 作为 `proxyLifecycle` 的有限 log-producing task 接纳：shutdown 开始后
拒绝新任务，已接纳任务受 shadow HTTP timeout 约束并在 request logger drain
前完成，避免 `Proxy.Close` 返回后仍向无人消费的 channel 写记录。

显式 `protocol:` 在配置加载时同时校验 endpoint：`anthropic` 需要
`anthropic_base_url`，`openai`/`responses` 需要 `openai_base_url`；不允许把
合法协议名配到缺失的 base URL 后留到运行期静默跳过。

`replay` 使用 request_log 的原始客户端 body 和原 path，通过 force-provider 重发。拒绝 shadow record、非 `/v1` 路径和截断 body。

## Request log

request log 是异步、非阻塞、owner-only 的 JSONL。查询分两条路径：

协议转换后的响应流与 Responses state/Shadow 共用
`internal/transport/bodycapture.Reader`：reader 只负责有界 tee、完整长度和
Close-once 回调，日志 schema、入队与 replay 判断不进入 transport 包。

- metadata 查询（list API、shadow report）置 `recordFilter.MetadataOnly`，在 top-K 保留前丢弃 request/response body，内存边界按 `limit × metadata` 计算，不是 `limit × max_body`；UI list 上限 1000、shadow report 上限 10000 均只保留 metadata。
- detail/replay 才反序列化并保留完整 body。

扫描全部 `requests-*.log`（不假设文件名顺序等于 record timestamp 严格顺序，孤儿 active 文件或时钟纠正可能让旧名文件持有新记录），单行用 `bufio.Reader.ReadBytes`（不用 Scanner，避免默认 token cap 丢尾）。

## Fusion 工作流

顶层 `fusion:` 定义 2–4 个 panel 成员、synthesizer、可选 `min_panel`、judge、budget 和 instruction。route 通过 `{provider: fusion, model: <workflow>}` 引用。

### Panel

每个成员独立 goroutine、独立 timeout。整个 Fusion 请求持有与普通 route 相同的
`runtimeSnapshot`；panel、judge、synthesizer 与普通 route 均通过 `targetPlan`
完成 provider config/runtime impl、backend protocol、model rewrite、协议转换及
base URL/path 选择，Fusion 不得重新读取 reload-owned catalog/config。

panel/judge 在此共享 plan 上执行非流式、tool-free 分支，并应用与普通目标一致的
half-open、401 refresh、paramBlock 预应用及即时学习重试、429/5xx、
model-denied/404、empty-200 模型锁、metrics、usage、live event 和 request log
策略。verdict 驱动的 /responses leg 遇 404 时同样翻转 wire verdict
（`noteWireResponsesMiss`）且**不锁模型**——verdict 判错而非模型缺失。metrics
口径与 tryTarget 对齐：被放弃的 leg 记 evFailovers（连接错误/5xx 另记
evFailures；401 只记 evFailovers）。候选文本按 leg 的 backendProto 解析
（anthropic content[]、chat choices[] 或原生 responses output[] 的
output_text），任何形状解析为空才判 empty draft 并锁模型。responses 客户端的
previous_response_id 在每个 leg 上按 forward 同一规则展开（仅无状态后端；
原生 responses 后端保持透传链路）。被 quorum/grace 主动 cancel 的分支不计
provider failure。候选文本最多 24k rune。

### Quorum 与 grace

`min_panel` 默认 2。达到 quorum 后再等待 5 秒 grace 接收较慢成功；若成功数加在途数已不可能达到 quorum，立即取消剩余分支。

### Synthesizer

候选段随机顺序注入：Anthropic 追加 system，OpenAI 追加 user，responses 追加带
`type: "message"` 的 input item（无类型的 map 会被 r→chat 转换器丢弃）。
synthesizer 与普通 route 共享 `targetPlan`，再构造同一个 `targetAttempt` 进入
`attemptExecutor.execute`，因此 SSE、转换、metrics、cache、request log 和统一
失败策略全部一致；两者只能经 `newTargetAttempt(runtime, plan, exchange, scope,
policy)` 装配，Fusion 不得直接构造 attempt 或重新增加平行的 positional 参数/
HTTP 执行接口。factory 之前的 Responses expansion、model rewrite 与 request
conversion 仍保留 Fusion 的客户端可见 history 语义。

responses 客户端命中 fusion 时，synthesizer 为无状态后端则展开
previous_response_id：发送体是合成体的展开，但**录制的 history 取客户端可见的
原始对话**（注入的 instruction/候选段是单轮脚手架，不得回放到后续轮次）；最终
返回给客户端的 responses 响应由 executor 录制，供下一轮换链。原生 responses
后端保持透传，由上游自维护链。

工具请求只在 synthesizer 保留 tools。synthesizer 不支持 tools 时降级为原始请求直打 synthesizer。

### Judge

judge 是可选的一次非流式内部调用，复用 panel leg 管道。成功报告以 `<JUDGE_ANALYSIS>` 注入候选前；失败只跳过报告，不让整次 Fusion 降级。

### 成本和观测

- `max_runs_per_day` 在 fan-out 前 admission；降级运行不消耗预算。
- `first_turn_only` 在多轮会话直接降级。
- 降级原因：`insufficient_proposers`、`tools_unsupported`、`body_build_failed`、`budget_exceeded`、`multi_turn`。
- registry 保留 200 条 run，跨 reload 存活。
- SQLite 中 `(fusion, workflow)` 的 requests 表示编排次数，failovers 表示降级次数。
- Fusion 适合高质量单发场景，不应作为默认 route；典型延迟约 2 倍、成本 N+1 倍。

## 回归测试

- cache 完整 EOF、client cancel、转换响应 header。
- shadow 真实 executor 路径的 reload generation、Close 时 logger drain 顺序、
  协议与 base URL 校验、并发 cap、sample_rate=0。
- request log 大 body 的 metadata 内存边界和跨文件乱序。
- Fusion pooled resolver、共享 target plan、model lock、paramBlock 即时重试、
  429、empty 200。
- Fusion leg 的 wire-verdict 404 翻转（不锁模型）与失败 metrics 口径、
  原生 responses 后端 leg 的候选文本提取（不误判 empty draft）、
  responses 客户端的 previous_response_id 展开与最终响应录制。
- quorum impossible、grace、judge failure、tools fallback、daily budget。
