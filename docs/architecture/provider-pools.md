# Provider 多账号池与统一解析

## 适用范围

修改 `internal/accounts/*`、`internal/routing/resolver.go`、
`internal/providerbuild`、登录/登出、多账号 quota 或 Fusion/Shadow provider 解析时必读。

## 凭据文件

池化 provider 使用 `~/.model-proxy/<name>_apikeys.json`。对曾使用旧格式的
provider，单数 `<name>_apikey.json` 仅作为只读 fallback，包装成一条账号；
`static` 从引入起只使用 plural pool，没有可运行的 legacy singular 路径。

文件 schema、稳定账号 ID、plural 优先/legacy fallback、原子保存和跨进程锁由
`internal/accounts` 统一拥有；它只允许向存储叶子 `internal/credstore` 依赖以访问
keychain/原子文件能力。该包接收已解析的 home directory，不得自行读取 HOME，
也不得依赖 Config、Provider、Proxy、Web/CLI 或执行网络验证。

存储后端由 config `credentials:` 选择（`accounts.Backend`）：`file`（默认）把
秘密值内联在 0600 pool JSON；`keychain` 经 credstore 把 api_key/access_key/
secret_key 写入 OS keychain（service "model-proxy"），pool 文件只含 metadata
（`{version, accounts:[{id,label,added_at,...}]}`，秘密字段为空）——读写路径为
`saveKeychain`/`loadSnapshotKeychain`，keychain 不可达、缺条目或 hydrated 凭据
无法推导出原 metadata namespace 时 fail-closed，不能在内存中静默换 ID；这也禁止
Volcengine AK-bound 账号因两个 AK/SK 条目同时丢失而降级成 API-only。
keychain→file 切回有反向回迁（`restoreFromKeychain`）：file 模式读到纯 metadata
池时按条目从 keychain 读回秘密并原子重写明文池，缺条目的账号保留 metadata 并经
`Snapshot.ReloginNeeded` 报出需重新 login。只有全部账号都恢复成功、且秘密推导出的
canonical ID 与原 metadata/keychain namespace 完全一致时，才允许写明文池；
Volcengine 的非 API-only 身份必须同时恢复 AK/SK 并复核同一 ID，缺任一字段不得
降级成 API-only。部分恢复或非 canonical ID 均保持原 metadata 文件不变，也不生成
完整恢复标记。

完整回迁在写明文前先原子写入 0600 的 `<pool>.keychain-origin`：marker 只含
`version=1` 和 canonical account IDs，不含任何凭据值。读取回迁不会删除 keychain
副本；显式 `Store.RemoveAccount` / `Store.RemoveAllAccounts` 才消费 marker 并先清理
对应 keychain 字段。清理失败时 pool 与 marker 保持可重试；纯 file 历史没有 marker，
删除绝不访问 keychain。部分回迁时原 metadata 本身是 authority，Store 的删除路径
直接在该原始 metadata 上移除所选 ID，不能使用丢失 `ReloginNeeded` 的 hydrated pool。
metadata-only 删除始终清理当前被移除 ID 的 keychain namespace；marker 只补充历史
provenance（例如 `RemoveAllAccounts` 清掉已不在当前 metadata 的旧 ID），即使切回
keychain 后 marker 变旧，也不能压制新账号的实际删除。

metadata-only pool 转成普通 file `Save` 前有全量守卫：原 metadata 的每个 ID 必须都
出现在新 pool 中，并由新凭据推导出同一 canonical ID；否则拒绝写明文与 marker。
因此多账号逐个重新 login 不会静默丢掉尚未处理的 metadata；用户要放弃某条记录时
必须先显式 remove/logout。

`Store.Save` 与 `Store.LoadSnapshot` 使用同一套账号语义校验；保存调用必须传入
provider ID。非法 ID、空 key、重复 ID 或不完整的 Volcengine AK/SK 在写临时文件
前即失败，不能覆盖磁盘上已有的有效 pool；keychain Save 还必须在第一次后端写入
前验证每条凭据可推导出其 metadata/keychain namespace ID。

写操作必须在跨进程锁内重新读取当前 pool，再按账号 ID 修改并保存；账号删除由
`Store.RemoveAccount` / `Store.RemoveAllAccounts` 统一拥有 keychain provenance 与
metadata-only 语义，CLI/Web 不得自行 `os.Remove` 或重写 pool。stdin、
浏览器和上游凭据验证必须在锁外完成。目录保持 `0700`，pool/lock 文件保持
`0600`，保存使用同目录临时文件后 rename，避免读到半写 JSON。

运行时构建只调用一次 `Store.LoadSnapshot`，由同一个结果携带 `Pool` 与
`SourceMissing | SourceLegacy | SourcePlural`。plural 成功读取后始终是权威来源，
即使账号列表为空也不能再读取 legacy；plural/legacy 不可读或 JSON 损坏时禁用该
provider。只有 missing/legacy 来源允许普通 API-key provider 保持旧 file-backed
路径；`static` 是 plural-only，missing/legacy 时同样不构建。

同一次 `providerbuild.BuildProviders` pass 必须同时产出 runtime providers、`poolIndex`、
`parentOf`；startup/reload 将 build 结果直接
传给 `routing.DeriveRoutesFrom` + `routing.BuildExpandedRoutes`（`internal/app/proxy_reload.go`），
不得再读取账号文件。这样一次 generation
不会出现“新 route eligibility + 旧 provider key”或反向组合。

账号 ID：

- volcengine 优先使用 `access_key` 的 hash，为空时回落到 api_key 的 hash；
- 其他 API key provider 使用 `sha256(api_key)[:16]`。

ID 永远是 hash：virtual id 会进入日志、request log 与持久化状态，不得携带
凭据材料。`accounts.Store.LoadSnapshot` 在读取 plaintext 存量池时把 ID 归一到当前
推导（修复历史版本把 volcengine 明文 access key 当 ID 的池文件），并在下一次
Save 时写回；metadata-only 池的 ID 同时是现有 keychain namespace，不能只归一化
文件侧，否则旧 namespace 会失去清理依据，因此此类非 canonical ID 要求显式
remove 后重新 login。

API-key provider（当前包括 static、zhipu、zcode、deepseek、volcengine、
kimi-code、mimo、qwen-plan、step-plan）支持池化；aqp、codex 使用各自 OAuth/SSO 单账号文件，
不进入 API-key pool。login 按 id 去重，支持 label/replace，成功后触发热 reload。

## 构建期展开

多账号 provider 展开成虚拟 provider：

```text
<parent>#<accountID>
```

每个虚拟实例必须把自己的 API key 绑定到 `provider.Config.BoundAPIKey`；同一
provider 实例的 `AuthHeaders` 为 forward 以及采用该通用认证入口的
FetchModels、Usage/Quota 提供认证。volcengine 还必须把该账号的
AccessKey/SecretKey 绑定到同一实例，供 V4-signed quota 调用；FetchModels
（ListArkAgentPlanModel）同样用该虚拟自己的 AK/SK 签名（残留边界见
「已知边界」）。不得恢复一套独立的 `cfg.Auth` 分支。

单账号池文件也必须 bind；不能因为只有一条记录而退回运行期文件读取。

volcengine 每账号包含 `{api_key, access_key, secret_key}`。绑定凭据存在时，`resolveAKSK` 必须排他使用该账号，不得回落到兄弟账号文件。chat-only 虚拟（`BoundAPIKey` 非空、无 AK/SK）是合法形态：Quota 与 FetchModels 都必须直接报 AK/SK 未配置，不得回读 legacy 单账号文件——legacy 回读仅保留给未绑定的单账号实例，且经 credstore Ref 读取（keychain 模式同样覆盖）。

## Resolver

所有“配置 provider 名 → 可运行 provider 实例”的路径统一经过 resolver：

- `Expand`：父 provider 展开为全部虚拟账号，用于正常路由和 failover；
- `Pick`：为 Fusion/Shadow 等单目标调用选择一个 built 且 healthy 的虚拟账号。

resolver 负责 identity mapping 和健康预过滤，不负责占用 half-open slot。权威 gate 仍由调用方的 `takeHalfOpenSlot` 完成。

resolver 不持有 composition root `*Proxy`；它只依赖 `resolverState` 暴露的
Manager health gate 与 generation-aware spread index 能力，以及当前
`RuntimeSnapshot` 的 provider/pool map。无 session 的 spread 推进必须携带该
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

volcengine `FetchModels` 已按账号绑定：每个虚拟用自己的 AK/SK 签名 `ListArkAgentPlanModel`；chat-only 虚拟（无 AK/SK）返回 needs AK/SK 错误，不借用 legacy 单账号文件；未绑定的单账号实例传空 AK/SK，由实现经 credstore 回读 `<name>_apikey.json`。池化时 `models refresh` 仍取排序后首个虚拟——若该虚拟是 chat-only 则报 needs AK/SK，不会自动换用带 AK/SK 的兄弟虚拟。探测全失败时保留合并模型集，不写空。

## 回归测试

- 单账号池、多账号池和 legacy fallback。
- plural 与 legacy 并存时只用 plural；损坏/空 plural 必须零上游请求。
- 空 key、空/重复/含 `#` 的账号 ID 和不完整 Volcengine AK/SK 必须拒绝。
- keychain→file 的 Volcengine AK/SK 缺半、非 canonical ID、部分恢复、逐个重新
  login 与显式删除；marker 必须无秘密、0600、失败可重试，纯 file 无 marker 不碰
  keychain。
- legacy-only 普通 provider 保持 file-backed；static 只接受 bound plural key。
- BoundAPIKey 不读取虚拟名字对应的不存在文件。
- session 稳定、不同 session 分流、账号冷却 failover，以及 pool spread 不跨
  winning rank。
- Fusion panel/synthesizer 和 Shadow 的 pooled parent 解析。
- resolver !ok 时没有任何上游请求。
