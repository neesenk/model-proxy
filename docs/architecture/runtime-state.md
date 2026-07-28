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

## quotaTracker

`quotaTracker` 默认每 5 分钟并行轮询，缓存到内存并原子写入 `~/.model-proxy/quota_state.json`。文件包含：

- provider quota snapshot；
- route-keyed sticky；
- provider health cooldown；
- model locks；
- model-scoped paramBlock；
- config fingerprint；
- 顶层 `wire_caps`：wire 探测 verdict（`{base_url, responses, anthropic, probed_at}`，三态以 `"yes"/"no"/"unknown"` 字符串落盘），按 parent provider 名 keyed。与 health 不同：**不受 config fingerprint 门控、reload 不清空**（能力是端点属性而非凭据/配额状态）；恢复时同时要求 parent 仍存在且记录的 `base_url` 与当前 config 一致，不匹配即作废重探。探测完成与 404 纠正时经 async persist 写盘（请求路径不得同步 persist——persist 经 fullSnapshot 取 p.mu.RLock，handler 已持有该锁，可能撞 reload 写者死锁）。verdict、选择策略与并发 map 统一归 `internal/runtime/wirecap.Store`；其 mutex 是 leaf lock，持锁时不回调 Proxy，也不进入 `p.mu → healthMu → quotaMu` 锁序。

陈旧超过 `3 × quota_poll_interval` 或带错误的 quota snapshot 视为 `BillingUnknown`，不得误当 pay-as-you-go。

当前实现使用 tracker 实例内的 `persistMu` 串行化 snapshot → **唯一同目录临时文件** → rename（每次写一个唯一 `.tmp`，多个 tracker/process 或 tracker 与同步调用者不再争用同名，rename 不会再 ENOENT），并在 quota poll、manual refresh、部分 429 refresh 和 unfreeze 时写盘。`Proxy.Close` 先通过 lifecycle gate 停止接收新任务，再等待 poller goroutine（含 reload 的 `pollAsync` 与 429 的 `refreshAsync`）后做 final flush；dispatch 的 accepting 检查与 `WaitGroup.Add` 在同一把锁内，不得与 shutdown 的 `Wait` 竞争。

### config generation 一致性

Proxy 为每次成功 reload 分配单调递增的 config generation。forward、Fusion 和 quota poll 都携带开始时的 generation；health/sticky/modelLock/paramBlock mutation 与 quota poll 提交前必须校验 generation，旧请求和慢 poll 的结果直接丢弃。

reload 按 `p.mu → healthMu → quotaMu` 一次性切换 cfg/providers/routes generation，并清空旧 health、sticky、model lock、paramBlock 和 quota snapshot。`persist()` 按同一锁顺序一次性复制 quota + health + sticky + config fingerprint，不允许分别回调后拼装。reload 交换完成后同步写入「新 fingerprint + 空运行态」；写盘失败以“配置已生效但 durability 降级”的 warning 返回，调用方不得回滚已经与 runtime 分叉的 config 文件。

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

`proxyLifecycle` 是 Proxy 级后台任务的唯一 owner。daemon 只调用
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

HTTP listener、在途 handler、SIGHUP reload loop，以及 `webTaskOwner` 管理的
login-session GC/AQP/Codex 异步登录属于 daemon transport 生命周期，不属于
`proxyLifecycle`。SIGINT/SIGTERM 的关闭顺序是：

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

锁顺序是：

```text
healthMu → quotaMu
```

不得持有 `healthMu` 时回调会反向获取 quota/外部锁的代码。持久化应先分别复制 snapshot，再做 JSON 和文件 I/O，不能长期持锁。

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

## Sticky

route sticky 记录 current provider 和 since。在 `sticky_dwell` 内优先当前 provider；驻留期结束后，只有 tier、priority 或 surplus margin 足够更优才切换。

session sticky 使用 `x-claude-code-session-id`；没有 session id 才退回 route key。session-keyed sticky 不落盘，route-keyed sticky 可落盘。

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

- tier/priority/surplus 排序和 sticky margin。
- stale/error quota 的 unknown 降级。
- 并发 poll/manual refresh/429 refresh 的单写正确性。
- 多 tracker 同 path、进程退出、测试 TempDir cleanup。
- lifecycle 关闭时拒绝新任务、等待已接纳任务、drain request logger，重复 Close
  不阻塞。
- transport shutdown 先停止 listener/reload/Web owner，取消并等待异步登录与 GC，
  正常等待在途 handler；超时强制取消连接后仍等待 handler 退栈，再关闭并 final
  flush logger/Responses state。
- fingerprint mismatch、旧请求/慢 quota poll 跨 generation、reload clear、mutation 后立即重启。
- pin、force、unfreeze 与 cache/failover 的交互。
