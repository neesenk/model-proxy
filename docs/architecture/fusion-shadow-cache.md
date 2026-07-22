# Fusion、Shadow、Cache 与实时观测

## 适用范围

修改 `fusion.go`、`shadow`、`request_log.go`、`cache.go`、`live_events.go` 或 replay 时必读。API 字段另见 `docs/web-api.md`。

## 精确响应缓存

缓存默认关闭：`cache.enabled/ttl/max_entries/max_body_bytes`，默认 TTL 10m、1000 条、单 body 256 KiB。

- key 是 method、path、原始请求 body 的 SHA-256；
- 只缓存 `<300`、完整读到 EOF、未超过 body cap 的响应；
- 客户端断开或半截响应不得入缓存；
- 转换后的响应删除旧 Content-Length/Transfer-Encoding 后保存；
- 命中重放原始客户端协议字节并设置 `x-mp-cache: hit`；
- cache hit 不计 provider metrics/agent stats，但产生 live end event；
- pin 和 force-provider 跳过读写缓存；
- reload 重建缓存并清空条目。

缓存定位是重复请求/重试盾牌，不是多轮对话前缀缓存。

## Live events

`eventHub` 保存最近 200 条事件并非阻塞 fan-out。慢订阅者丢事件，不能反压请求路径。

forward 产生 start/end，包含 agent、protocol、provider、status、latency、tokens 和稳定 request_id。cache hit、400/502 终局也必须产生 end。`GET /api/events` 先重放 ring，再推送 SSE，15 秒 keepalive。

## Shadow

每 route 可配置 shadow provider/model/protocol/sample_rate/max_concurrent。生产响应 commit 后 fire-and-forget：

- sample rate 的 nil 表示 1.0，显式 0 表示关闭；
- semaphore 满时丢弃 shadow；
- shadow 使用自身 protocol/baseURL；
- 不得影响熔断、sticky、生产 metrics 或响应；
- 结果进入 request_log，id 以 `shadow-<primary-id>` 配对；
- reload 必须让一次 dispatch 全程使用同一 generation 的 runtime、target、provider map 和 client。

`replay` 使用 request_log 的原始客户端 body 和原 path，通过 force-provider 重发。拒绝 shadow record、非 `/v1` 路径和截断 body。

## Request log

request log 是异步、非阻塞、owner-only 的 JSONL。查询分两条路径：

- metadata 查询（list API、shadow report）置 `recordFilter.MetadataOnly`，在 top-K 保留前丢弃 request/response body，内存边界按 `limit × metadata` 计算，不是 `limit × max_body`；UI list 上限 1000、shadow report 上限 10000 均只保留 metadata。
- detail/replay 才反序列化并保留完整 body。

扫描全部 `requests-*.log`（不假设文件名顺序等于 record timestamp 严格顺序，孤儿 active 文件或时钟纠正可能让旧名文件持有新记录），单行用 `bufio.Reader.ReadBytes`（不用 Scanner，避免默认 token cap 丢尾）。

## Fusion 工作流

顶层 `fusion:` 定义 2–4 个 panel 成员、synthesizer、可选 `min_panel`、judge、budget 和 instruction。route 通过 `{provider: fusion, model: <workflow>}` 引用。

### Panel

每个成员独立 goroutine、独立 timeout。当前管道已经实现 resolver、runtime build gate、half-open gate、协议转换、provider rewrite、基本 429/5xx 记录、metrics、usage、live event 和 request log。

当前 panel/judge leg 没有复用普通 `tryTarget` 的完整模型级策略：resolver 的 `Pick` 已检查既有 model lock（与 tryTarget 同一规则，见 `resolve.go` `healthy`），但仍不会应用/学习 paramBlock、model-denied 或 empty-200。后续重构的目标顺序是：

1. ~~resolver 解析 pooled provider~~（已完成）；
2. ~~runtime impl/build gate fail-closed~~（已完成）；
3. ~~model lock / half-open gate~~（已完成：resolver `Pick` 经 `healthy` 跳过 locked `(provider, model)`）；
4. rewrite model、协议转换、provider rewrite、paramBlock；
5. 剥除 tools/tool_choice，强制非流式；
6. 使用与普通 tryTarget 一致的 429、5xx、model-denied、unsupported-param、empty-200 策略；
7. 记录 branch metrics、usage、live event 和 request log。

被 quorum/grace 主动 cancel 的分支不计 provider failure。候选文本最多 24k rune。

### Quorum 与 grace

`min_panel` 默认 2。达到 quorum 后再等待 5 秒 grace 接收较慢成功；若成功数加在途数已不可能达到 quorum，立即取消剩余分支。

### Synthesizer

候选段随机顺序注入：Anthropic 追加 system，OpenAI 追加 user/input item。synthesizer 复用普通 `tryTarget`，因此 SSE、转换、metrics、cache、request log 和统一失败策略全部一致。

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
- shadow reload generation、并发 cap、sample_rate=0。
- request log 大 body 的 metadata 内存边界和跨文件乱序。
- Fusion pooled resolver、model lock、paramBlock、429、empty 200。
- quorum impossible、grace、judge failure、tools fallback、daily budget。
