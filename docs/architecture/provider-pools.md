# Provider 多账号池与统一解析

## 适用范围

修改 `internal/accounts/*`、`internal/app/accounts_store.go`、`internal/routing/resolver.go`、
`buildProviders`、登录/登出、多账号 quota 或 Fusion/Shadow provider 解析时必读。

## 凭据文件

池化 provider 使用 `~/.model-proxy/<name>_apikeys.json`。对曾使用旧格式的
provider，单数 `<name>_apikey.json` 仅作为只读 fallback，包装成一条账号；
`static` 从引入起只使用 plural pool，没有可运行的 legacy singular 路径。

文件 schema、稳定账号 ID、plural 优先/legacy fallback、原子保存和跨进程锁由
无仓库内依赖的 `internal/accounts` 统一拥有。该包接收已解析的 home directory，
不得自行读取 HOME，也不得依赖 Config、Provider、Proxy、Web/CLI 或执行网络
验证。`internal/app/accounts_store.go` 只负责 HOME 适配和兼容入口。

`Store.Save` 与 `Store.LoadSnapshot` 使用同一套账号语义校验；保存调用必须传入
provider ID。非法 ID、空 key、重复 ID 或不完整的 Volcengine AK/SK 在写临时文件
前即失败，不能覆盖磁盘上已有的有效 pool。

写操作必须在跨进程锁内重新读取当前 pool，再按账号 ID 修改并保存；stdin、
浏览器和上游凭据验证必须在锁外完成。目录保持 `0700`，pool/lock 文件保持
`0600`，保存使用同目录临时文件后 rename，避免读到半写 JSON。

运行时构建只调用一次 `Store.LoadSnapshot`，由同一个结果携带 `Pool` 与
`SourceMissing | SourceLegacy | SourcePlural`。plural 成功读取后始终是权威来源，
即使账号列表为空也不能再读取 legacy；plural/legacy 不可读或 JSON 损坏时禁用该
provider。只有 missing/legacy 来源允许普通 API-key provider 保持旧 file-backed
路径；`static` 是 plural-only，missing/legacy 时同样不构建。

同一次 `buildProviders` pass 必须同时产出 runtime providers、`poolIndex`、
`parentOf` 和 implicit-route eligibility；startup/reload 将该 eligibility 直接
传给 `synthesizeImplicitRoutesFrom`，不得再读取账号文件。这样一次 generation
不会出现“新 route eligibility + 旧 provider key”或反向组合。

账号 ID：

- volcengine 优先使用 `access_key`，为空时回落到 hash；
- 其他 API key provider 使用 `sha256(api_key)[:16]`。

API-key provider（当前包括 static、zhipu、zcode、deepseek、volcengine、
kimi-code、qwen-plan）支持池化；aqp、codex 使用各自 OAuth/SSO 单账号文件，
不进入 API-key pool。login 按 id 去重，支持 label/replace，成功后触发热 reload。

## 构建期展开

多账号 provider 展开成虚拟 provider：

```text
<parent>#<accountID>
```

每个虚拟实例必须把自己的 API key 绑定到 `provider.Config.BoundAPIKey`；同一
provider 实例的 `AuthHeaders` 为 forward 以及采用该通用认证入口的
FetchModels、Usage/Quota 提供认证。volcengine 还必须把该账号的
AccessKey/SecretKey 绑定到同一实例，供 V4-signed quota 调用；其 FetchModels
仍是下述已知例外。不得恢复一套独立的 `cfg.Auth` 分支。

单账号池文件也必须 bind；不能因为只有一条记录而退回运行期文件读取。

volcengine 每账号包含 `{api_key, access_key, secret_key}`。绑定凭据存在时，`resolveAKSK` 必须排他使用该账号，不得回落到兄弟账号文件。

## Resolver

所有“配置 provider 名 → 可运行 provider 实例”的路径统一经过 resolver：

- `Expand`：父 provider 展开为全部虚拟账号，用于正常路由和 failover；
- `Pick`：为 Fusion/Shadow 等单目标调用选择一个 built 且 healthy 的虚拟账号。

resolver 负责 identity mapping 和健康预过滤，不负责占用 half-open slot。权威 gate 仍由调用方的 `takeHalfOpenSlot` 完成。

resolver 不持有 composition root `*Proxy`；它只依赖 `resolverState` 暴露的
Manager health gate 与 generation-aware spread index 能力，以及当前
`runtimeSnapshot` 的 provider/pool map。无 session 的 spread 推进必须携带该
snapshot 的 generation；reload 后到达的旧请求不得改变新 generation 的轮询
位置。因此 pooled identity 选择不能顺带访问 reload、完整调度、Web 或其他
Proxy 状态，也不得自行持有 health/spread map。

任何 resolver 失败都必须 fail-closed，禁止继续使用池化父名构造无认证请求。

## Session sticky

池内请求按 session sticky：

- 同一个 session 稳定映射到同一账号；
- 无 session 时用共享 spread counter 轮询；
- sticky 账号不可用时扫描兄弟账号 failover；
- 会话中不得仅因 quota surplus 边际变化迁移账号；
- session sticky 不落盘。

路由调度中的 pool spread 不能改变全局排序语义：只有排序第一名本身是池账号时
才推进该 parent 的 spread，且只在同 tier、同 priority 的 winning rank 内轮询。
较低排名的池不能越过更优的非池候选，池内较低 tier/priority 的账号也不能借
spread 提前。

## 已知边界

volcengine `FetchModels` 尚未按账号完全绑定；池化时 `models refresh` 复用首个虚拟凭据。探测全失败时保留合并模型集，不写空。

## 回归测试

- 单账号池、多账号池和 legacy fallback。
- plural 与 legacy 并存时只用 plural；损坏/空 plural 必须零上游请求。
- 空 key、空/重复/含 `#` 的账号 ID 和不完整 Volcengine AK/SK 必须拒绝。
- legacy-only 普通 provider 保持 file-backed；static 只接受 bound plural key。
- BoundAPIKey 不读取虚拟名字对应的不存在文件。
- session 稳定、不同 session 分流、账号冷却 failover，以及 pool spread 不跨
  winning rank。
- Fusion panel/synthesizer 和 Shadow 的 pooled parent 解析。
- resolver !ok 时没有任何上游请求。
