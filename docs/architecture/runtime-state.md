# 调度与运行态持久化

## 适用范围

修改 `quota.go`、调度排序、sticky、pin、health persistence、reload 或运行态锁时必读。

## 配额归一化

`Provider.Quota()` 返回 `provider.QuotaSnapshot{Billing, RemainingPct, Windows, ...}`。

| provider | 来源 | Billing |
|---|---|---|
| zhipu | `quota/limit` | plan |
| codex | `wham/usage` | plan |
| volcengine | `GetAFPUsage`，需要 AK/SK | plan |
| aqp | `monthly_usage` | plan |
| kimi-code | `/usages` | plan |
| deepseek | `/user/balance` | pay-as-you-go |

最长周期窗口标记为 `Ultimate`，作为调度总预算和节奏基准；更短窗口标记为 `Short`，表示短期 rate-cap。短窗口不直接参与最终 `RemainingPct` 的 min。

kimi-code 根据 Duration 自动选择最长窗口；zhipu 通过 unit 映射 5h/weekly。`TIME_LIMIT` 和 booster wallet 只展示，不参与调度。

## runtime Manager 与 quotaTracker

`internal/runtime.Manager` 是 config generation 内可变路由状态的唯一 owner。
它以一把 mutex 统一持有 generation、provider health、route/session sticky、
operator pin、model lock、model-scoped paramBlock、pool spread counter 和 quota
snapshot，并在同一临界区内完成 schedule、cooldown、resolver health gate、
persistence snapshot 与 Web dashboard snapshot。所有复合 snapshot 都是 detached
copy；调用方不得保留或修改 Manager 内部 map/slice。

Manager 只依赖 `provider` 的 quota 值类型。Config 适配、HTTP、状态文件编码、
Web DTO 映射和 lifecycle 都留在根包；Manager 持锁时不得回调这些外部职责。

`quotaTracker` 默认每 5 分钟并行轮询，负责 provider `Quota()` 调用、manual/429
refresh 去重、poll/refresh lifecycle 和 `~/.model-proxy/quota_state.json` 的文件
编排。quota snapshot 的内存权威值属于 Manager，tracker 不持有第二份 quota map。

状态文件包含：

- provider quota snapshot；
- route-keyed sticky；
- provider health cooldown；
- model locks；
- model-scoped paramBlock；
- config fingerprint；
- 顶层 `wire_caps`：wire 探测 verdict（`{base_url, responses, anthropic, probed_at}`，三态以 `"yes"/"no"/"unknown"` 字符串落盘），按 parent provider 名 keyed。与 health 不同：**不受 config fingerprint 门控、reload 不清空**（能力是端点属性而非凭据/配额状态）；恢复时同时要求 parent 仍存在且记录的 `base_url` 与当前 config 一致，不匹配即作废重探。探测完成与 404 纠正时经 async persist 写盘（请求路径不得同步 persist——persist 经 fullSnapshot 取 `p.mu.RLock`，handler 已持有该锁，可能撞 reload 写者死锁）。verdict、选择策略与并发 map 统一归 `internal/runtime/wirecap.Store`；其 mutex 是 leaf lock，持锁时不回调 Proxy，也不进入 `Proxy.mu → runtime.Manager` 锁序。

陈旧超过 `3 × quota_poll_interval` 或带错误的 quota snapshot 视为 `BillingUnknown`，不得误当 pay-as-you-go。

tracker 在每次 commit 新快照（pollAll/pollOne/429 refresh）时，以**上一次已 commit 快照**为基线计算 ultimate 窗口的耗尽预测：`rate = Δused/Δt`，`ExhaustionEta = as_of + remaining/rate`，挂到 `QuotaSnapshot.ExhaustionEta` 后进 Manager。以下情况不预测（零值）：首个快照无基线、任一侧带错误、速率 ≤0（空闲或窗口已 reset）、`Δt > 3 × quota_poll_interval`（轮询断档，基线陈旧）、窗口已耗尽或未测量。预测**仅展示用**（`usage` CLI 窗口行尾、Web Status 配额卡），调度不读，不落盘；重启后首轮 poll 可用从 quota_state.json 恢复的上一快照作基线（断档超界则不预测）。`usage` CLI 是独立进程、单次 live fetch，其基线是经 `provider.DecorateExhaustionEta` 读取的持久化快照（按普通 provider 名 keyed；池化虚拟账号 key 无 CLI 预测）。

当前实现使用 tracker 实例内的 `persistMu` 串行化 snapshot → **唯一同目录临时文件** → rename（每次写一个唯一 `.tmp`，多个 tracker/process 或 tracker 与同步调用者不再争用同名，rename 不会再 ENOENT），并在 quota poll、manual refresh、部分 429 refresh 和 unfreeze 时写盘。`Proxy.Close` 先通过 lifecycle gate 停止接收新任务，再等待 poller goroutine（含 reload 的 `pollAsync` 与 429 的 `refreshAsync`）后做 final flush；dispatch 的 accepting 检查与 `WaitGroup.Add` 在同一把锁内，不得与 shutdown 的 `Wait` 竞争。

### config generation 一致性

Proxy 为每次成功 reload 分配单调递增的 config generation。forward、Fusion、
resolver spread 和 quota poll 都携带开始时的 generation；health、sticky、
modelLock、paramBlock、spread 和 quota mutation 由 Manager 在同一锁内校验
generation，旧请求和慢 poll 的结果直接丢弃。

reload 按 `Proxy.mu → runtime.Manager` 一次性切换 cfg/providers/routes generation；
`Manager.ReplaceGeneration` 原子清空旧 health、sticky、model lock、paramBlock、
spread 和 quota，operator pin 有意跨 reload 保留。`persist()` 按同一锁顺序捕获
config fingerprint 和 Manager 的 `generation + quota + health + route-keyed
sticky` 原子 snapshot，不允许分别读取后拼装。reload 交换完成后同步写入「新
fingerprint + 空 generation-scoped 运行态」；写盘失败以“配置已生效但 durability
降级”的 warning 返回，调用方不得回滚已经与 runtime 分叉的 config 文件。

### 已知缺口与目标契约

- health/model lock/paramBlock mutation 应进入 debounce 单写者，而不是只依赖下次 quota poll。

恢复只采纳未过期条目，且 config fingerprint 必须匹配。恢复的 circuit failure count 置为 threshold，使下一次失败立即重新开路。

旧版无 fingerprint 文件仅保留兼容读取，不应扩展新的无指纹状态。

## Responses 对话状态

`responsesStateStore` 只服务于 Responses 客户端跨协议访问无 `previous_response_id` 能力的 chat/anthropic 后端。状态与 quota tracker 分离，写入同目录 `responses_state.json`：

- key 为 session + response id；缺 session 时仅接受唯一 response id；
- TTL 30 分钟、最多 512 条、单条 2 MiB、总量 32 MiB；
- 250ms debounce 异步落盘，目录 0700、文件 0600；每次使用同目录唯一临时文件、fsync 后原子 rename；
- `Proxy.Close` 停写、等待 owner 并 final flush；
- 命中后展开完整 input/output 历史；miss 只对本次 orphan/dangling item 做保守修复。
- 只记录 completed 和因 `max_output_tokens`/token length 截断的 incomplete；content_filter、其他中止或带 error 的响应不进入 replay state。

## Proxy 生命周期

`internal/runtime.Lifecycle` 是 Proxy 级后台任务的唯一 owner。daemon 只调用
`startRuntimeServices` 和 `Proxy.Close`，不得自行启动或关闭 stats flusher、
request logger、catalog refresh。生命周期 gate 在同一 mutex 内完成
accepting 检查与 `WaitGroup.Add`；shutdown 顺序为：

1. 拒绝新任务并关闭 stop channel；
2. 停止 stats 周期循环、drain request logger；
3. 等待 reload catalog refresh 等已接纳任务；
4. final stats flush；
5. 关闭并 final flush Responses state；
6. 停止 quota tracker 并持久化 quota/health/wire state。

`Proxy.Close` 幂等。测试直接构造 Proxy 时仍必须注册 cleanup；只有通过
`startRuntimeServices` 启动的 request logger 才由 lifecycle 关闭，测试手工注入
但未启动的 logger 不得在 Close 中等待一个不存在的 loop。

### HTTP transport 关闭

HTTP listener、在途 handler、SIGHUP reload loop，以及 `internal/web` task owner
管理的 login-session GC/AQP/Codex 异步登录属于 daemon transport 生命周期，
不属于 `internal/runtime.Lifecycle`。SIGINT/SIGTERM 的关闭顺序是：

1. transport gate 拒绝新的 handler/reload；Web owner 拒绝新任务并取消轮询；
2. 显式 `http.Server.Shutdown` 停止接入并等待在途 handler，deadline 为 8 秒；
3. deadline 超时时调用 `http.Server.Close` 强制断开连接、取消 request context，
   并继续等待所有已接纳 handler 完成退栈；
4. 等待 SIGHUP 和 Web owner 返回；已进入凭据写入 commit 的登录任务必须完成
   save + reload，未进入 commit 的网络请求/轮询等待由 context 取消；
5. 最后调用 `Proxy.Close`，执行 request logger、Responses state、stats 和 quota
   的 drain/final flush。

第 3 步不能在 `Server.Close` 返回后立即关闭 Proxy：Go HTTP server 不保证此时
所有 handler 已经返回，必须以独立 admission gate/WaitGroup 证明 handler drain
完成。8 秒 deadline 小于 supervisor 的 10 秒 worker hard-kill 窗口，为最终持久化
留出时间。信号 goroutine 只发起 shutdown；不得在其中直接 `os.Exit` 绕过上述顺序。

## 锁顺序

跨域锁顺序是：

```text
Proxy.mu → runtime.Manager
```

Manager 内部只有一把状态锁，不存在 health/quota 的嵌套锁。Manager 持锁时不得
回调 Proxy、quota tracker、wirecap Store 或外部 I/O；持久化和 Web 先取得 atomic
detached snapshot，再在锁外完成 JSON、文件或响应编码。

## 调度分

`QuotaSnapshot.Surplus(now, peakMult)`：

```text
surplus =
  ultimate.remaining
  - short.remaining × short.total/ultimate.total × (peakMult-1)
  - clamp((ultimate.reset-now)/ultimate.duration, 0, 1)
```

正数表示使用进度落后，优先消耗；负数表示超前，应回避。

排序顺序：

```text
tierRank → priority asc → surplus desc
```

- tier 顺序是 `plan < unknown < payg`，不可直接使用 `BillingClass` iota。
- priority 高于 surplus。
- 相同 priority 允许多个目标组成 surplus 竞争池。
- peak multiplier 只作用于 Short 窗口折算。
- pool spread 只在排序第一名本身属于池时启用，且轮询范围仅限同一 parent、
  同一 tier、同一 priority 的 winning rank；路由中较后的池不得越过更优的非池
  候选，池内较低 tier/priority 的账号也不得被轮询到 winning rank 前。

`Manager.DecideOrder` 在同一锁内从权威 quota snapshot 投影 billing/surplus，
并读取 pin、health、model lock、sticky 和 spread；根包只传 provider/model/
parent、priority、billing override、peak multiplier 等 config-derived 输入，
不得先读 quota 再调用 Manager 排序。这样一次请求决策不会在 quota projection
与 availability/sticky 选择之间跨过 reload 或并发 mutation。

只有 `Commit=true` 且 generation 匹配时，才允许清理陈旧 sticky 或推进 pool
spread。请求路径不深拷贝 quota 的 Notes/Windows/Details，也不为 projection
分配中间 map。

`DashboardSnapshot.PreviewOrder` 复用与 `Manager.DecideOrder` 相同的排序 core，
但只读取 snapshot 内已经脱离的 quota/health/model-lock/pin/sticky/spread，
且强制 `Commit=false`。`/debug/schedule` 与 `/api/status.schedule` 必须用捕获
config generation 时取得的这一个 DashboardSnapshot 计算，不得再次进入 Manager；
因此同一响应中的 health/quota/pin/sticky/order 属于同一时刻、同一 generation。

`POST /debug/route`（`internal/app/proxy_route_preview.go`，body = 客户端原样请求体，
`?proto=` 覆盖协议，默认 anthropic）是**单请求版**的决策预览：复刻 forward 的早期
步骤（model 提取、claude_mapping、route 查找、pin/force 收窄、cache 只读探测），
排序同样走 `PreviewOrder`（`Commit=false`，不推进 spread、不落 sticky），并输出
per-target 的 request-fit 判定（`routing.FitVerdict`，与 `Planner.Apply` 同语义，
带 reason：能力缺失或 `estimated/context` 比例）。cache 探测以**预览请求自身**的
header 计算 key——要得到与真实请求一致的 hit/miss 结论，调用方需带上同等的
`anthropic-beta`/`accept-language`。预览不发起任何上游调用。

## Quality 打分（error rate + TTFT）

quality 状态是 **copy-on-write**：record 路径（持 `m.mu`）发布新的不可变
`map[string]providerQuality`（值类型），读侧（`DecideOrder`）**不持锁**加载指针并把
衰减投影（map 分配 + 每 provider 的 EWMA 计算）移出调度临界区——锁内不再有 quality
相关的分配；一条 record 恰好在加载与加锁之间落地的情形，只是本次决策看不到它，
与原锁内投影已有的单决策级滞后同类。Dashboard 投影复用同一个纯函数
（`projectQuality`），同样在取锁前加载并投影——Dashboard 临界区里也没有 quality
相关的分配。

排序键实际是 `score = surplus − qualityPenalty`：

- 每个 provider 维护两个 EWMA（半衰期 2m，代码常量）：错误率（RecordFailure=1 /
  RecordSuccess=0）与归一化 TTFT（Committed 成功 2xx 时采样，10s 参考值封顶）。
  两个信号各自持有独立的采样锚点时间——错误样本只推进错误率锚点，TTFT 样本只推进
  TTFT 锚点：共享锚点会让持续失败期把旧 TTFT 罚分"保鲜"，且恢复后的首个 TTFT 样本
  以 dt≈0 混入而几乎不动陈旧罚分。模型级失败（RecordModelFailure）与 429 冷却不进
  错误率——它们各有独立机制，重复计入会双重惩罚。
- 决策时信号先各自衰减到 `now` 再计罚分：停止失败一段时间的 provider 不会背陈旧
  罚分；罚分 <1e-6 吸附为 0，渐近衰减的残余不得翻转原本精确打平的顺序。
- 权重来自 `scheduling.quality_error_weight`（默认 100=1.0）/
  `quality_ttft_weight`（默认 20=0.2），指针语义：显式 0 关闭该信号，未设置用
  默认。两个权重都为 0 时排序与引入质量打分前逐字节一致。
- 罚分同时进入 sticky 切换的 margin 比较——粘性账号质量恶化经同一个
  `quota_switch_margin` 闸门逃逸，无独立逃逸路径。
- quality 状态不持久化（重启后快速重建）；`unfreeze`/ResetHealth 一并清除——
  操作员说"立即重试"时不得残留降权。Dashboard 携带 Quality，PreviewOrder parity
  覆盖。

## Sticky

route sticky 记录 current provider 和 since。在 `sticky_dwell` 内优先当前 provider；驻留期结束后，只有 tier、priority 或 surplus margin 足够更优才切换。

availability 过滤（发生在 sticky 判定之前）除了熔断/限频/模型锁，还排除**配额已知耗尽**的 plan target：新鲜快照（`quota_poll_interval` 的 maxAge 内、无错误）的 ultimate RemainingPct==0 视为耗尽——粘住已知耗尽的账号只会白挨一轮确定性 429。快照过期或出错时 fail-open，退回反应式 429 冷却兜底；PayG/unknown 无窗口可耗尽，永不被此规则排除。PreviewOrder 走同一判定，Web 预览与真实调度一致。

**耗尽信号必须传导到失败分类**（`Manager.CooldownState`/`HasRecoveredUntried` 消费同一 quotaExhaustedUntil 判定）：skip 意味着耗尽 target 永远收不到那记教会 health 的上游 429，若分类只看 health，全耗尽 route 会读成"全部可用"→ 终局 502 无 `Retry-After`，且 `hasRecoveredUntried` 空转重排。正确语义：quota 耗尽在分类中是 **rate-limit 类 down 原因**，恢复时间取 ultimate 窗口 `ResetsAt` 与快照失鲜边界（`AsOf+maxAge`）的**较早者**（失鲜后 fail-open 回落反应式 429 冷却，客户端按短周期回来重探而不是对着陈旧数据等一个可能很远的 reset）；与 health 限频/熔断并存时取较晚者（两因皆清才可用）。全耗尽 route 因此终局 **429 + `Retry-After`**，与 skip 之前客户端拿到的退避信号一致。

session sticky 使用 `x-claude-code-session-id`；没有 session id 才退回 route key。session-keyed sticky 不落盘，route-keyed sticky 可落盘。session sticky 的 dwell 自**最近一次使用**起算（活跃会话每次请求刷新 since），因此连续活跃的会话会一直停留在当前 provider，直到闲置超过 `sticky_dwell` 或其失败熔断——"会话中不得仅因 quota surplus 边际变化迁移账号"（见 provider-pools.md）。

## Pin

`pin <route> <provider> [--ttl]` 是排障用硬约束：

- 在 availability 过滤之前收窄目标；
- pinned provider 熔断或失败也不得 failover；
- pin 池化父名等于 pin 全部虚拟账号；
- `x-mp-force-provider` 是单请求覆盖；
- pin 仅在内存中，reload 不清，重启清除；
- pin/force 请求必须绕过响应缓存。

## Unfreeze

`unfreeze [provider]` / `POST /api/health/reset` 清除熔断、限频和模型锁，不清 sticky、pin、paramBlock。

空 body 清全部；池化父名清所有虚拟账号。畸形 JSON 必须返回 400，持久化成功后才能返回 200。

## 回归测试

- tier/priority/surplus 排序、pool spread 不跨 winning rank，以及 sticky margin。
- Manager 的 generation 替换、stale mutation 丢弃、quota 深拷贝、schedule
  commit gate、单锁 quota+health+pin+sticky+spread 决策、PreviewOrder parity、
  atomic persist/dashboard snapshot 与 detached map/slice。
- stale/error quota 的 unknown 降级。
- quotaTracker 的并发 poll/manual refresh/429 refresh 去重、停止接纳和文件单写
  正确性；不得通过 tracker 内部 quota map 断言。
- 多 tracker 同 path、进程退出、测试 TempDir cleanup。
- lifecycle 关闭时拒绝新任务、等待已接纳任务、drain request logger，重复 Close
  不阻塞。
- transport shutdown 先停止 listener/reload/Web owner，取消并等待异步登录与 GC，
  正常等待在途 handler；超时强制取消连接后仍等待 handler 退栈，再关闭并 final
  flush logger/Responses state。
- fingerprint mismatch、旧请求/慢 quota poll/resolver spread 跨 generation、
  reload clear、pin 跨 reload 保留、mutation 后立即重启。
- pin、force、unfreeze 与 cache/failover 的交互。
