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
keychain/原子文件能力。该包暴露 `HomeDir()`（`os.UserHomeDir()`）供调用方显式解析路径，
其余存储 API 接收已解析的 home directory、不隐式读取 HOME，
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
kimi-code、mimo、qwen-plan、step-plan、openrouter、opencode-go、typesafe）与
OAuth provider（aqp、codex）都支持池化（见下「OAuth 账号池」）。login 按 id
去重，支持 label/replace，成功后触发热 reload。

## 构建期展开

多账号 provider 展开成虚拟 provider：

```text
<parent>#<accountID>
```

**plural 凭据池按虚拟键展开，与池大小无关**——单账号池同样展开为
`parent#<accountID>`，父名不是任何运行时键（只有 legacy 单文件/未登录的
file-backed 实例保持裸名）。账号 id 是稳定身份：兄弟账号增删不改变本账号的
键，token 用量、quota 快照与 request log 归属因此跨账号变更连续。历史版本
单账号池绑裸名，跨越该边界的旧数据由启动迁移重键（见下「启动键迁移」）。

每个虚拟实例必须把自己的 API key 绑定到 `provider.Config.BoundAPIKey`；同一
provider 实例的 `AuthHeaders` 为 forward 以及采用该通用认证入口的
FetchModels、Usage/Quota 提供认证。volcengine 还必须把该账号的
AccessKey/SecretKey 绑定到同一实例，供 V4-signed quota 调用；FetchModels
（ListArkAgentPlanModel）同样用该虚拟自己的 AK/SK 签名（残留边界见
「已知边界」）。不得恢复一套独立的 `cfg.Auth` 分支。

volcengine 每账号包含 `{api_key, access_key, secret_key}`。绑定凭据存在时，`resolveAKSK` 必须排他使用该账号，不得回落到兄弟账号文件。chat-only 虚拟（`BoundAPIKey` 非空、无 AK/SK）是合法形态：Quota 与 FetchModels 都必须直接报 AK/SK 未配置，不得回读 legacy 单账号文件——legacy 回读仅保留给未绑定的单账号实例，且经 credstore Ref 读取（keychain 模式同样覆盖）。

## OAuth 账号池（aqp/codex）

aqp/codex 复用同一套池文件（`<name>_apikeys.json`）与机制（锁、原子写、
SourcePlural 权威、虚拟键展开、spread/sticky、用量归属、启动键迁移），差异在
凭据形态与身份锚定：

- **凭据 blob**：每账号的 OAuth 凭据整体存于 `Account.OAuth`（对 accounts 包
  透明的 JSON：aqp=`AqpAccountData`，codex=`CodexAuthFile`），`Account.Kind=
  "oauth"` 标记形态（metadata-only 时仍可辨识），与 api_key/access/secret 互斥。
  keychain 模式下 blob 为每账号一个 keychain 条目（`<name>/<id>/oauth`），超
  平台上限时按 codex 单文件先例显式回落 inline（0600 池文件）。
- **id 锚定**：`OAuthAccountID(upstream)` = sha256(上游账号身份)[:16]——aqp 用
  employee email、codex 用 chatgpt_account_id（缺失时回落 refresh_token 哈希）。
  id 在添加时一次性锚定，**token 轮换不重算**（normalizePool 跳过 OAuth 条目）。
- **provider 存储缝**：`provider.OAuthBlobStore`（Load/Save/Delete）——池化实例
  绑定 `poolOAuthStore`（providerbuild）：读走原子池读，**写（codex refresh 的
  轮换 refresh_token）在池跨进程锁内 load→替换本条目→save**；无池时回落
  `FileOAuthStore`（legacy 单文件路径，即历史行为）。
- **登录**：`login.AddOAuthAccount` 在锁内 upsert——同上游身份 = **静默替换**
  （重新授权；label/added_at 保留），首次建池时把 legacy 单账号文件吸收为条目
  #1（见「凭据文件」节的 plural 权威规则）。Web Re-login 可带 `account=<id>`
  定向替换（授权到别的账号时报错，不改原账号）。
- **畸形池 fail-closed**：OAuth provider 名下的 plural 池若含非 OAuth 条目
  （手误写入 API-key 池），整个 provider 禁用并 warn。
- legacy `<name>_oauth_auth.json` 只在无池时生效（裸名单实例）；guard 已知密钥
  集按账号枚举（标签 `oauth:<name>#<id>`）。

## 账号 label（显示名）

`Account.Label` 是用户提供的显示名：login/添加账号时给定（缺省为 id），Web
Accounts 页可随时改名（`POST /api/accounts/<provider>/<id>/label` →
`accounts.Store.SetLabel`，跨进程锁内重读改写；trim 后非空、≤64 字符）。
label 是纯元数据——不参与账号 ID 推导、虚拟键或任何凭据路径，改名不触发
reload。运行时键里的 `#<accountID>` 哈希是关联用途，不做用户面显示：Web 各
视图（Status Providers/Token usage、Requests 表、Analytics、Keys 用量）把
虚拟键折叠为 `parent · <label>`（pure.js `providerDisplayName`），仅 Accounts
页的 `id:` 副行展示完整 id 供删除/排查对号。

## 启动键迁移

历史版本把单账号 plural 池绑在裸父名下：这些数据（stats.db 分钟桶、
quota_state.json 快照）在池恒虚拟键后必须跟随账号迁移，否则首次加账号/
升级后全部视图失去历史。启动时（`internal/app/provider_key_migration.go`，
NewProxy 构建后、quota tracker Load 前）对每个池化父名执行一次重键：

- **归属**：裸名历史记在池只有单账号的年代，归属 `added_at` 最早的现存账号
  （不可解析/缺失按最新处理，id 作确定性 tie-break）；
- **stats.db**：`Store.RenameProviderKey` 在 Bootstrap 内、基线恢复**之前**
  执行——`minute_buckets`/`agent_buckets` 的裸名行与目标虚拟键行按桶合并
  （计数加和、`last_request_at` 取 max），内存累计计数器随基线 seed 到新键；
- **quota_state.json**：`providers` 段的裸名条目改名为虚拟键（目标已存在时
  丢弃较旧的裸名条目），sticky/health 短时效状态不迁移；
- 迁移幂等（无裸名行即 no-op）、失败降级为 warning（历史留在旧键下），不阻塞
  启动；legacy 单文件 provider 不在迁移范围（无 plural 池）。

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

- 单账号池、多账号池和 legacy fallback；单账号 plural 池同样展开虚拟键
  （`TestBuildProviders_SingleEntryPluralPoolBindsVirtualKey`）。
- 启动键迁移：裸名 stats/quota 历史重键到最早账号的虚拟键（含合并与幂等，
  `TestStartupProviderKeyMigration_*`、stats 层 `TestRenameProviderKeyMergesBuckets`）；
  无 plural 池的 provider 不迁移。
- label 改名：持久化生效、账号 ID/凭据不动、未知 id 与非法 label fail-closed
  （`accounts.TestSetLabel`、`admin.TestRenameAccount`）。
- OAuth 池：展开虚拟键（`providerbuild.TestBuildProviders_UnrollsOAuthPool`）、
  畸形池禁用、legacy 裸名单保持；`poolOAuthStore` 写回只动本账号
  （`TestPoolOAuthStore_SaveReplacesOnlyOwnAccount`）；codex refresh 经 seam 落
  池（`provider.TestCodexOAuth_RefreshWritesBackThroughSeam`）；upsert 静默替换
  保留 label/added_at、legacy 吸收为条目 #1（`login.TestAddOAuthAccount_*`）。
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
