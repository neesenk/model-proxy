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
- config fingerprint。

陈旧超过 `3 × quota_poll_interval` 或带错误的 quota snapshot 视为 `BillingUnknown`，不得误当 pay-as-you-go。

当前实现使用 tracker 实例内的 `persistMu` 串行化 snapshot → 固定 `.tmp` → rename，并在 quota poll、manual refresh、部分 429 refresh 和 unfreeze 时写盘。

### 已知缺口与目标契约

当前持久化还没有完全满足运行态 durability，后续修复应达到：

- snapshot、fingerprint 必须属于同一 config generation；
- 每次原子写使用唯一的同目录临时文件，不能让多个 tracker/process 争用固定 `.tmp`；
- health/model lock/paramBlock mutation 应进入 debounce 单写者，而不是只依赖下次 quota poll；
- reload 清空运行态后必须持久化空状态，防止重启复活旧冻结态；
- `Proxy.Close` 应停止并等待后台 goroutine，完成 final flush；
- 测试必须使用隔离 HOME/state path，并在 cleanup 中关闭 Proxy。

恢复只采纳未过期条目，且 config fingerprint 必须匹配。恢复的 circuit failure count 置为 threshold，使下一次失败立即重新开路。

旧版无 fingerprint 文件仅保留兼容读取，不应扩展新的无指纹状态。

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
- fingerprint mismatch、reload clear、mutation 后立即重启。
- pin、force、unfreeze 与 cache/failover 的交互。
