# 架构总览

## 定位

model-proxy 是单进程模块化单体。Provider、调度、协议转换、运行态、观测和
Web/API 保持同一部署单元，但通过显式数据结构和窄端口隔离；当前复杂度不需要
拆成微服务。

`Proxy` 与其组合根已迁入 `internal/app`：根 `package main` 只剩薄进程入口。
`internal/cli.Application` 一次性绑定具体 CLI 命令；`internal/app.Runtime` 装配并
持有一次 serve 进程使用的 `Proxy`、HTTP 与 Web 组件；`Proxy` 是应用运行时聚合对象，
不是允许任意模块访问的共享状态袋。新增行为应进入下述模块边界，不能继续给长参数链
或 Web handler 增加内部字段访问。

```text
main (OS args/streams/exit)
  → internal/cli.Application (process composition, command table)
    → cli_serve.go serveAssembly (serve/daemon process lifecycle)
      → internal/app.Runtime (Proxy construction, runtime services, mux/Web,
                              reload projection, transport tasks, Close)
        → internal/app.Proxy (application runtime aggregate and domain lifecycle)
```

根 `package main` 只保留进程边界文件：`main.go`（OS args/streams/exit）、
`app_assembly.go`（把 serve 驱动注入 `internal/cli.Application`）、`cli_serve.go`
（serve/daemon 角色分发、signal/listener/drain 编排）与 `version.go`
（构建注入的版本号）。CLI 命令实现、参数解析与调度循环归 `internal/cli*`；
组合根、Proxy 与 Web/API 适配归 `internal/app`；请求转发管线（guard/cache/schedule/
failover/Fusion 编排与 target plan/executor 装配）归 `internal/forward`。

`internal/app` 的物理布局按职责收敛：`internal/app/proxy.go` 只保留应用运行时聚合对象
（`Proxy` 字段分组成内嵌的 `generationState`——reload swap unit——与
`processServices`——跨 reload 的进程级服务）；
`internal/forward/forward.go` 集中主请求的 forward/serve 调度、failover 与 commit 编排，
`internal/app/proxy_forward.go` 只是薄 shim：每请求一次 `SnapshotRuntime` 捕获、一次
`forward.Services` 装配、一次 `forward.Serve` 调用（管线永远看不到 Proxy、不碰 p.mu）；
`internal/app/proxy_shadow.go` 只保留 Shadow post-commit policy、lifecycle admission、
generation-bound resolver/plan 与 request-log 投影；可热重载 sampling/
concurrency/client 及 detached HTTP 执行归 `internal/shadow`；
`internal/forward/fusion.go` 是 captured snapshot 到 `internal/fusion.Engine` 的适配，并保留
panel/judge 的 target policy adapter 与 synthesizer 的正常 target executor
接线；fan-out、quorum/grace、judge/body 构造、budget 和 registry 归
`internal/fusion`；
`internal/providerbuild` 以一次账号 snapshot 同时构建 provider、pool identity 和
route derivation 输入（见 request-routing 文档），不持有 Proxy/runtime 状态；
`internal/app/proxy.go` 负责 Proxy 内部组件装配、状态恢复、Close 委派和 stats reset；
`internal/app/proxy_lifecycle.go` 承载 `StartRuntimeServices`/`closeRuntimeServices` 的进程级后台服务
启停编排（`Close` 经 `closeOnce` 委派至此）；
`internal/app/config_alias.go` 是 archtest 认可的唯一存活配置 facade（`internal/config` 类型别名与
加载 wrapper，根 `package main` 不得重建）；
`internal/app/proxy_snapshot.go` 集中 config/provider/catalog/pricing 读取与 generation 一致的
持久化快照；
`internal/app/proxy_reload.go` 只编译 explicit/derived route 与 pool fan-out；
`internal/app/proxy_reload.go` 执行 generation 原子交换及交换后的持久化/刷新编排；
`internal/app/proxy_http.go` 只承载主代理 HTTP 路由、models 响应和早期终态事件；
`internal/app/proxy_schedule.go` 从单个 detached dashboard snapshot 投影调度状态；
`internal/app/proxy_schedule.go` 只把 config route/pin 输入映射到 runtime Manager；
`internal/app/proxy_read_endpoints.go` 只把应用层 health/cooldown/param/rate-limit 输入映射到
runtime Manager，并保留 429 后 quota refresh 编排；
`internal/admin` 是 Web admin 应用服务：把组合根经 `internal/app/web_adapter.go`
注入的窄端口（copy-by-value、自带锁纪律）投影为 `internal/appapi` consumer-owned
`ReadAPI` / `CommandAPI`，包括 JSON-safe DTO 和全部凭据/配置 mutation；
`internal/app/web_adapter.go` 只装配 `internal/web.Server` 并挂载到主 mux；
`internal/app/target_pipeline.go` 只把 captured generation/scheduling 与应用 observability
映射到 `targetexec.State/Effects`（`proxyHealthGate` 与 `targetExecutionEffects`，经
`forward.Services` 的 `NewHealthGate`/`NewEffects` 端口注入管线）；一次 runtime snapshot 的
config、parent identity、route keys 与 generation 到 `internal/routing.Planner` scheduler
端口的绑定随管线归 `internal/forward/plan.go`；SSE/HTTP 流识别、复制和 ResponseWriter
primitive 归 `internal/targetexec/transport.go`；
`internal/app/runtime.go`（`Runtime`）只在配置及进程日志准备好后构造 Proxy、启动运行时服务、
注册根 handler/Web、保存 reload 投影和 transport tasks，并以 `Close` 委派给 Proxy。

## 请求执行链

```text
HTTP handler
  → RuntimeSnapshot
  → serveRequest
  → schedule / internal/routing.Planner / failover
  → internal/targetexec.Plan
  → internal/targetexec.Attempt
  → internal/targetexec.Executor
  → provider.Provider
```

- `RuntimeSnapshot`（canonical 类型 `internal/forward.Snapshot`，app 侧保留别名）：一次请求只捕获一个 reload generation 的 config、
  provider implementations、pool identity、expanded routes、catalog、cache 和
  Shadow dispatch runtime；锁纪律（单次捕获）留在 `Proxy.SnapshotRuntime`。
- `serveRequest`：一次 schedule/failover pass 的稳定输入。
- `internal/targetexec.Plan`：`internal/forward/plan.go` 的 `planTarget` 只解析同一 `Snapshot` 中的
  provider implementation、backend protocol、wire verdict 与视觉能力；不可变
  的 model/body/base URL/path wire preparation 和 target/provider facts 由该
  Plan 拥有，普通 route、Fusion、Shadow 共用。
- `internal/targetexec.Attempt`：一个已解析上游目标的完整执行契约，只由
  `runtime + plan + exchange + scope + policy` 五组强类型字段组成；
  `newTargetAttempt → targetexec.NewAttempt` 是普通 route 与 Fusion
  synthesizer 的唯一构造入口。Runtime 只投影 captured scheduling、generation
  与 cache，不允许用 `any` 或完整 `RuntimeSnapshot` 绕过边界。
- `internal/targetexec.Executor`：拥有完整单目标 HTTP/retry/response pipeline，
  只通过 generation-frozen `targetexec.State` 修改健康、参数学习和 wire state；
  metrics、tokens、request log、Responses state、events 只经 typed
  `targetexec.Effects/Responses` 注入。`internal/forward/plan.go` 只组装这些端口（app 侧
  `proxyHealthGate`/`targetExecutionEffects` 经 Services 端口注入），
  不执行 I/O、转换或 failover。commit 后只返回最小 `targetexec.Commit`，不持有
  生命周期或 Shadow 调度能力。
- `internal/targetexec.BufferedLeg`：Executor 的 headless 非流式对应物，面向需要
  响应字节而非 client stream 的调用方（Fusion panel/judge leg）。拥有 URL 构建 +
  provider rewrite + paramBlock 预应用、JSON POST 发送，以及一次性 401 auth
  refresh / 400 参数 learn-strip 重试；circuit/metrics/rate-limit 等 effect 记录
  留在调用方，最终 exchange 经 `Capture` 回填给调用方的 request log。

`RuntimeSnapshot` 与 `internal/targetexec.Plan` 是执行器内
reload-owned/config/provider/protocol 事实的唯一来源；`internal/forward` 只把 snapshot
投影为 typed `targetexec.Runtime`，不得在普通/Fusion/Shadow 分支各自重算
endpoint 或 conversion options。exchange 只承载 HTTP request/writer/body，
scope 只承载本次请求身份与 Responses 上下文，policy 只承载
force/last-target/context-retry。
`newTargetAttempt` 不负责 model rewrite、Responses history expansion 或协议转换，
这些准备语义仍由普通/Fusion 各自编排后再进入执行器。

同协议保持字节透传。`internal/protocol` 是无仓库内依赖的叶子包，统一拥有协议
identity、六组 pairwise codec、request/response/SSE registry、SSE↔JSON 模式
桥接、跨协议图片约束，以及 Responses `previous_response_id` 的有界状态。
每个 client→backend pair 必须同时提供 request、反向 response、反向 SSE codec；
专用 pair codec 保留 hosted tools、reasoning 方言和 namespace 等协议特有语义。
Provider 方言和目标视觉能力由 `internal/forward/plan.go` 的 `planTarget` 解析后作为纯值注入
`internal/targetexec.Plan` 的窄 request options；
具体 `http.ResponseWriter` 错误 envelope 仍由 transport 层负责。

`internal/config` 统一拥有配置类型、YAML 加载、默认值、校验和生效值
accessor；它不是无仓库依赖叶子，只允许依赖其校验/默认值实际需要的
`internal/pricing` 与 `internal/protocol`，以及承载 models.dev 端点/缓存位置
策略的 `internal/catalog`（`MP_MODELSDEV_URL` 环境解释与
`~/.model-proxy/models_cache.json` 位置由 `internal/config/modelscatalog.go`
拥有，镜像 `MP_PRICING_URL` 的 config 域行为）。根包的类型别名与兼容 wrapper 已删除，
composition root 与现有
调用方不得在根包重新建立第二套配置事实或恢复 `config.go` / `config_compat.go`。

`internal/catalog` 是无仓库内依赖的 models.dev 元数据源叶子包，拥有 slim
projection、canonical-owner 去重、HTTP/ETag/TTL 刷新和原子磁盘缓存。
`internal/config/modelscatalog.go` 只把
HOME、`MP_MODELSDEV_URL` 适配成 catalog 输入；`internal/routing/model_metadata.go`
拥有 Config 的 provider/route 名单遍历与 fallback/source 策略（`HydrateModels`、
`DefaultModelMetadata`）；
请求感知路由继续消费一次性捕获在 `RuntimeSnapshot` 中的不可变 catalog 指针。

`internal/routing` 是只依赖 `internal/catalog`、`internal/config`、
`internal/protocol` 与 `internal/provider` 值类型的无状态策略包，拥有请求画像、能力/context 判断、跨 route pool、context overflow
replacement 和跨 pass cooldown 终局决策，以及 config→route table 推导
（implicit routes 聚合、target priority 回填、pool fan-out 前的 expanded routes）
和 config-time route warnings（reasoning-replay marker 等启动期 hazard），以及 models.dev
元数据 hydration（`HydrateModels`：config provider/route 名单遍历 + catalog 查找 +
fallback/source 策略）。`routing.Planner` 只能由
`requestRoutingPlanner` 通过 constructor 装配；`internal/forward/plan.go` 捕获 generation 并
调用注入的 schedule 端口（app: `Proxy.schedule`），策略包不得 import Proxy、runtime Manager、target executor
或任何 I/O owner。

`internal/fusion` 是只依赖 `internal/config` 值类型的编排包：
`Engine` 拥有 first-turn/tools/budget gate、panel fan-out、quorum/grace、
judge、三协议 synthesis body 构造和 run 记录时序；`Registry` 拥有 200 条有界
run ring、per-workflow aggregate 与 local-day budget。该包只经
`fusion.Ports` 请求 generation-bound leg/synthesis 能力，不得访问 `Proxy`、
HTTP client、runtime Manager、metrics/events/request log 或 reload-owned
对象。管线侧 `fusionAdapter`（`internal/forward/fusion.go`）在一次 `fusionCtx.runtime` 上实现这些端口；
panel/judge 仍使用共享 `targetexec.Plan`，synthesizer 仍通过唯一
`newTargetAttempt → targetexec.Executor` 返回客户端。

`internal/shadow` 拥有 reload-swappable `Runtime`：sample decision、非阻塞
concurrency gate、专用 timeout client，以及基于已解析 `targetexec.Plan` 的
model rewrite、fail-closed conversion、provider auth/rewrite/header、detached
HTTP drain 和 bounded capture。它不得使用 target executor 或生产
state/effects。`internal/app/proxy_shadow.go` 是唯一 post-commit adapter，先基于主请求
captured runtime 做 eligibility/sample/acquire，再经 `internal/runtime.Lifecycle` 接纳；
goroutine 内仅做 generation-bound resolver/plan、调用 `shadow.Runtime.Execute`
并投影 request log。

`internal/accounts` 是无仓库内依赖的 API-key 账号存储叶子包（唯一例外：
`internal/credstore`，keychain 后端经其实现），拥有 credential
tuple、稳定账号 ID、plural/legacy 读取优先级、原子保存和跨进程锁。存储后端由
config `credentials:` 选择：`file`（默认，秘密值内联在 0600 池 JSON）或
`keychain`（api_key/access_key/secret_key 经 credstore 进 OS keychain，池文件只留
id/label/added_at 元数据；明文池与 legacy 文件首读懒迁移，keychain 不可达时
fail-closed 报错而非回落明文）。`credentials:` 是两类凭据的单一开关：
config 加载点统一调 `accounts.SetProcessCredentialsMode`，同时设置池后端与
credstore 的 OAuth blob 模式；env `MP_CRED_STORE` 仅作 OAuth 侧的显式 override
（env 非空 > config > 默认 file），分歧由 `accounts.CredentialMismatchNote`
在 `config check`/启动/reload 日志报出。keychain→file 切回有反向回迁：file 模式
读到纯元数据池时按条目从 keychain 读回秘密并原子重写明文池，缺条目的账号保留
元数据并经 `Snapshot.ReloginNeeded` 报出需重新 login（部分回迁不整体失败），
仅在全量恢复且 identity 与原 keychain namespace 一致时写明文；写前落一个无秘密、
0600 的 `.keychain-origin` marker。读取回迁保留 keychain 副本，显式
`Store.RemoveAccount` / `Store.RemoveAllAccounts` 才按 marker 清理，失败保留
pool/marker 供重试；纯 file 历史无 marker 时不访问 keychain。metadata-only 普通
Save 必须覆盖并 canonical 地恢复全部原 ID，未处理账号只能通过显式删除移除。
登录、Web 与 Provider 构建的调用方直接使用 `accounts.NewStore(accounts.HomeDir())`；
`providerbuild.BuildProviders` 以一次 `LoadSnapshot` 同时取得 pool 与来源，并在
同一 build result 中派生 providers、pool identity 和 route-derivation 输入，
避免二次文件探测改变同一 runtime generation 的 authority 决策。网络验证、
交互、reload、运行时虚拟化和健康选择不进入存储包。legacy singular 路径只由
`accounts.Store` 的读取兼容逻辑拥有，存储包之外不再导出路径 wrapper。

`internal/login` 是传输中立的登录/账号核心：拥有 codex OAuth device flow
（usercode → 轮询 → 换 token）、AQP（公司 Google SSO）网关客户端
（`AqpClient` 的 cookie jar、bootstrap、session 轮询、API key 申领）以及
apikey/volcengine 池的校验→去重→写入/删除（`AddApikeyAccount` /
`AddVolcengineAccount` / `RemoveApikeyAccount` / `ValidateKeyBearerGET` /
`FetchVisibleModels` / `LatestAPIKey` / `PoolPath`）。核心只返回值与 error，
不读 stdin、不打印；Web 层（`internal/app`）直接驱动它。`internal/cli/login`
是交互 shell：拥有 `login` 命令编排、flag 解析、stdin 提示、终端输出、
loopback 回调页与 serve daemon 热重载 nudge，并把核心返回值适配为终端 UX。
`internal/login` 不得 import `cli/**`。

`internal/observe/events` 是无仓库内依赖的实时事件叶子包，拥有事件 DTO、最近
200 条的有界 ring、非阻塞 fan-out、订阅快照和终态查询。`/api/events` SSE 与
keepalive 由应用层 `internal/app/proxy_http.go` 与 `internal/web/server.go` 服务；业务发布点
显式依赖 `events.Hub`，不得重新访问 ring、subscriber map 或互斥锁。纯 ring/
订阅测试归内部包，HTTP、forward、Fusion 与 cache 事件契约的集成测试在
`internal/app`。

`internal/observe/requestlog` 是只依赖 `internal/config` 值类型（生效值
accessor）的请求访问日志数据面叶子包，拥有
JSONL Record schema、body/header 截断与白名单、非阻塞队列、单 writer 的轮转/
retention/owner-only 权限、流式 top-K 查询（带文件级提前终止：peek 文件末尾记录
Ts 为上界，堆满或越 From 下界的文件整文件跳过，peek 异常回退全量流扫）、list-safe
Summary 和 Shadow 聚合。应用层 `internal/app/observe_adapters.go` 只把 `RequestLogConfig` 生效值
适配为纯值输入；`forward.LogCtx`/HTTP/RouteTarget 到 Record 的映射归
`internal/forward.BuildRequestLogInput`；capture 在转换器外层的位置、
`internal/runtime.Lifecycle` 的 Shadow-before-drain 顺序、Web 参数、CLI replay policy 和
Fusion/Shadow eligibility 继续由应用层编排。列表与 Shadow 必须调用强制丢弃
body/header 的 metadata API，detail/replay 才能查询完整 Record。

`internal/observe/seclog` 是只依赖 `observe/logx`（本身为叶子）的安全审计日志叶子包，拥有审计事件
Record schema（kind: secret/path/drift）、JSONL 写入、按大小+按天轮转、retention
sweep、owner-only 权限（文件 0600/目录 0700）、非阻塞队列与单 writer、离线 top-K
查询（与 requestlog 同形的文件级提前终止：peek 最后一条完整记录——torn tail 不算——
作为全文件 Ts 上界；被跳过文件内的 corrupt 行不再计入 Skipped，打不开的文件仍计入），
以及供 CLI 绕开 daemon 直接追加的 `AppendSync`。红线：`Record.Names` 只含
模式类型名/路径类别名，秘密值永不进入 Record；drift 记录的 detail 只含客户端名与
指针 host。应用层只注入纯值（命中名、动作、路由元数据），扫描、阈值与派发决策
不进本包。Logger 是 reload-owned：`internal/app/observe_adapters.go` 的
`reconcileSecLog` 在启动与每次 reload 按当前代调和（`guard.audit` off→on 当场建
logger、on→off 置 nil、`audit_path` 变更换新目录），构建与 goroutine admission 在
`p.mu` 外、换代后 drain 关停旧 logger；forward 经 `RuntimeSnapshot.SecLog` 写请求
自己代的 logger，换代瞬间旧快照的迟到 enqueue 允许丢弃（见
`docs/decisions/intentional-behaviors.md` 条目 19）。

`internal/observe/budget` 是月度等价成本告警包，拥有按墙钟分钟对齐的 tick 循环、
按 (scope, 月份, 阈值) 的进程内去重、live 事件与可选 webhook 派发（5s 超时、
有限重试、stop 同时中止重试睡眠与在途请求）。包内不触碰 reload-owned 状态：
每 tick 的 budgets/parentOf、analytics 查询与定价快照经窄口 `Ports` 以逐次
拷贝注入；startup-only 创建准入（从无到有加 `budgets:` 需重启，见
`docs/engineering/pitfalls.md` 条目 31）与 lifecycle 接线留在应用层
`internal/app/proxy_lifecycle.go` 适配器。

`internal/cache` 是无仓库内依赖的精确响应缓存叶子包，拥有请求 key、
TTL/容量 store、客户端可见响应的 bounded recorder、header normalization 与
逐块 flush replay。应用层 `internal/app/proxy.go` 的 `NewResponseCache`
只把 `CacheConfig` accessor 的生效值转换为 `cache.Options`；force/pin bypass、`<300` eligibility、转换器外层捕获
位置、cache-hit live event、reload generation swap 与 stats reset 仍由应用编排。
一次请求继续使用 `RuntimeSnapshot.Cache` 捕获的 Store，旧 generation 完成时不得
向 reload 后的新 Store 写入。

`internal/guard` 是无仓库内依赖的出站请求体安全扫描叶子包，拥有：嵌入式规则表
`rules.json`（53 条高置信秘密模式，46 条精选自 gitleaks v8.28.0 并保留溯源与熵
阈值，7 条本仓自有）、按生成期构建的不可变 `Scanner`（Aho-Corasick 字面量预过滤
+ 命中才精读的两阶段管线、known-secret 精确值变体集、规则前缀的 base64/hex 编码
通道、敏感路径类别表——路径字面量同为自动机 needle、span 去重的
Scan/ScanPaths/Redact，以及双通道同体的 `ScanSecretsAndPaths`：live 请求路径用它让
路径 gate 判定搭 secrets 扫描的同一趟自动机 pass，standalone 的 ScanPaths/
ScanPathsContext 保留 per-literal memchr gate——单独立趟时 memchr 每字节便宜约 25 倍、
字面量只有约 10 个，专用自动机 pass 反而更慢）。Scanner 以
`RuntimeSnapshot.Guard` 随 generation 原子交换；known-secret 凭据值只以内存形式
存在，永不落盘/序列化/进事件。codex/aqp 在 serve 期间原地轮转 OAuth token，因此
Proxy 另有一个 lifecycle 循环按 `scheduling.quota_poll_interval` 节拍重收 OAuth
auth 文件、并收集 provider 经 `provider.SecretReporter` 上报的内存凭据（aqp 的
minted managed key 只存在于内存、codex 的缓存 access_token 可能比文件新——
provider 凭据的首次接口级暴露，只读内存、只为扫描，见
`docs/decisions/intentional-behaviors.md` 条目 15），以当前代的池秘密基重建
scanner 并在 `p.mu` 下换代指针（文件 I/O 与构建
在锁外；I/O 期间发生的 reload 由代检查判废，绝不跨代混用）。
`internal/forward/forward.go` 在请求体完整读取后、
cache 查询与所有 forward 分支之前对共享 body 扫描一次，按 `guard.secrets`
（log/redact/block/off）与 `guard.paths`（log/block/off，不支持 redact）放行、
替换 `[REDACTED]` 或 400 拒绝；secrets/paths 两类扫描都完成后才统一评估响应动作
（secrets=block 不短路 paths 的计数/事件/审计，响应动作 secrets 优先），命中只以
模式类型名/路径类别名进入 live event、
`("guard", <名>)` 计数器与 seclog 审计记录，命中内容永不落日志或事件。

`internal/guard/session`（只依赖 `internal/guard`）拥有分片外传检测的会话窗口
Store：按 `x-claude-code-session-id` 的有界 LRU（256 会话 × 32KiB 尾窗），保存
redact 前请求体尾部与跨请求分片进度。窗口可能含凭据，只活在进程内存，永不日志/
序列化/落盘/API；它是进程生命期的跨代观察态（不进 RuntimeSnapshot，reload 不清空），
红线与启发式边界见 `docs/decisions/intentional-behaviors.md` 条目 20/21。

`internal/forward` 是请求转发管线包：从「snapshot + HTTP 请求」到「响应 commit 或终态错误」
的全部编排归它——body 读取与上限、route 解析、pin/force-provider 硬选择
（含 cache bypass）、outbound guard 扫描与统一动作评估、精确响应 cache、schedule →
request-aware routing → per-target failover（`serveOnce`）、cooldown wait-retry 与终局
分类、Fusion 编排适配，以及 target plan / `targetexec.Executor` / request `routing.Planner`
的装配。进程级依赖只经 `forward.Services`（client/metrics/tokens/agents/events/sessionScan/
responsesState/fusionReg/reqLog + `NewHealthGate`/`NewEffects`/`Schedule`/`ShadowDispatch`/
`ResolveBackendProto`/`ResolverState` 端口）注入；pin/cooldown 查询只经 consumer-owned
`forward.RouteState`。管线不得 import `internal/app`、不得持有 Proxy 或触碰 `p.mu`——
单次快照红线由调用方（app 的 `Proxy.forward` shim）拥有，Fusion/Shadow 分支只见到同一个
`forward.Snapshot`。

`internal/transport/bodycapture` 是无仓库内依赖的通用响应流捕获叶子包：
字节原样透传，只保存有界 prefix，同时统计完整长度和截断状态，并在首次 Close
执行一次回调。Responses state、request log 与 Shadow 共用这一 transport
primitive；各自的持久化和业务判断不得反向塞进通用 reader。

`internal/probe` 拥有全仓所有探测执行：唯一的请求构造+发送配方 `probe.Do`
（URL join → `RewriteRequest` → headers（`/v1/messages` 预置 `anthropic-version`）→
`AuthHeaders` → `prov.Headers` → `ExtraHeaders`）、模型可调性探测
`Callable`/`Exchange`（CLI `test`、`models refresh` 与 Web 账号测活端口共用）、
三协议矩阵探测 `ProbeModelProtocols`（daemon 启动/reload 能力探测与
`models refresh` 共用）、探测模型选择 `PickModel`（provider models[0] → 显式
route target → derived target）和 max_tokens→max_completion_tokens 改名重试。
daemon 的 wirecap/modelcaps 探测 pass 与 `wire record`（`internal/cli/diag`，
`Accept: text/event-stream` + 64MB BodyLimit）都经 `probe.Do` 发送。该包只执行
交换并报告原始结果——verdict 分类与存储归 `internal/runtime/wirecap`，provider
构建与凭据绑定留在应用层。

`internal/runtime/wirecap` 是端点协议能力包，拥有三态 verdict、两级并发 verdict
Store（provider 级 `Store` 与模型级 `ModelStore`，均 parent provider keyed、
leaf lock、持锁不回调应用代码）、协议选择纯策略（`ClassifyStatus`/
`ClassifyModelStatus`/`Resolve`/`ResolveModel`）、quota_state.json 顶层
`wire_caps` 的 JSON 表示，以及独立文件 `model_caps.json` 的格式与原子读写
（`ModelCapsPath`/`LoadModelCapsFile`/`SaveModelCapsFile`）。
`internal/app/wirecap.go` 与 `internal/app/modelcaps.go` 只保留 Proxy 侧的探测
编排、404 纠正触发和异步持久化。

`internal/runtime.Manager` 是 config generation 内可变路由状态的唯一 owner，
以单 mutex 统一 health、sticky、pin、model lock、paramBlock、spread、quota、
schedule 决策以及 persistence/Web detached snapshot；quality 子状态（per-provider
EWMA errRate 与 TTFT 质量惩罚，参与 schedule 排序）同样归 Manager，但经
`atomic.Pointer` copy-on-write 发布 immutable map：写入在 `m.mu` 下整图替换，读取
（`DecideOrder` 的 decayed projection）在锁外完成，map 分配与 EWMA 计算不进调度
临界区。该包只依赖 `internal/config`
值类型、`internal/runtime/wirecap` 与 `internal/provider`
中的 quota 值类型；HTTP、文件持久化和 Web DTO 映射仍由 composition
root 编排。`quotaTracker` 只执行轮询、refresh 去重和文件写入，不再拥有第二份
quota 状态。

Manager 的物理文件按职责拆分，但不形成多 owner：`internal/runtime/manager.go` 只定义 owner、
单锁与 generation；`internal/runtime/manager_types.go` 放边界 DTO；`internal/runtime/manager_persist.go`、
`internal/runtime/manager_quota.go`、`internal/runtime/manager_routing_state.go`、`internal/runtime/manager_health.go`、
`internal/runtime/manager_schedule.go` 分别承载持久化投影、配额、路由选择状态、健康状态和调度；
`internal/runtime/manager_quality.go` 承载 quality EWMA 状态与采样（errRate 与 TTFT 各有独立 anchor，
样本写入带 generation gate）。
新增可变 map 或锁必须仍回到 `Manager`，不得因文件拆分建立子状态仓库。

## 编排与异步分支

- Fusion 全程持有主请求的 `RuntimeSnapshot`。`internal/fusion.Engine` 拥有
  gates/fan-out/quorum/judge/registry，`internal/forward/fusion.go` 让 panel/judge 共用非流式
  target policy，并让 synthesizer 通过正常
  `targetexec.Attempt → targetexec.Executor` 返回客户端。
- Shadow 由 `serveOnce` 在主请求 commit 后根据 `targetexec.Commit` 接纳和派发，同时
  捕获 `RuntimeSnapshot` 与 `*shadow.Runtime`；executor 和 Fusion synthesizer
  均不得启动 Shadow，goroutine 内不得重新读取 reload-owned 状态。
- Cache、request log、usage scanner 位于响应转换外层，只观察客户端协议字节。
- Analytics 的价格目录、条件抓取、原子缓存、override 解析与成本公式由
  `internal/pricing` 这一无主包依赖的叶子包拥有；价格端点优先级由
  `internal/config` 解析，应用层 `internal/app/proxy_snapshot.go` 只适配应用 HOME 路径，
  `Proxy.pricingSnapshot` 保留配置快照和并发刷新锁。
- 统计热路径仍由 `internal/app.Proxy` 的 `metricsStore`、`tokenCounter`、`agentCounter` 各自拥有；
  `internal/observe/stats.Flusher` 只做 cumulative snapshot → minute delta 的应用投影，
  SQLite schema、迁移、upsert、聚合查询、retention 与 legacy token import
  统一归无仓库内依赖的 `internal/observe/stats`。

## 状态与锁

- `Proxy.mu`：只保护 reload-owned 对象交换（内嵌 `generationState` 整体）；请求流式期间不持有。
- `internal/runtime.Manager`：以单锁保护 generation、health、sticky、pin、
  model lock、paramBlock、spread、quota 和 schedule；quality map 经
  `atomic.Pointer` copy-on-write 发布，写入持锁、读取免锁（调度临界区外投影
  decayed 状态）。所有返回给 persistence
  或 Web 的复合结果必须在该锁内原子复制并与 generation 一起返回。
  请求排序在同一次临界区内完成 quota projection 与 health/pin/sticky/spread
  选择；Web/调试调度从同一个 detached DashboardSnapshot 做只读 preview，
  禁止为 order 二次读取 Manager。
- `internal/runtime/wirecap.Store`/`ModelStore`、metrics/tokens/agents、stats flusher、
  cache、pricing 各有独立 owner/leaf lock；SQLite Store 与 wire-capability
  Store 均不拥有或回调应用运行时。
- 跨域锁顺序仅允许 `Proxy.mu → runtime.Manager`。Manager 持锁时不得回调
  Proxy、quota tracker 或任何外部 I/O。

HTTP/UI transport 归 `internal/web`：`Server` 只消费 `internal/appapi` 的
consumer-owned `ReadAPI` / `CommandAPI`，不 import 或持有 `*Proxy`。`internal/appapi`
拥有全部 Web/CLI 共享的 DTO 与端口契约（JSON-safe、不含凭据），不依赖任何应用运行时。`internal/admin`
是这两个端口的唯一应用适配：它把组合根经 `internal/app/web_adapter.go` 注入的
detached snapshot、mutation 与 active probe 能力投影到两个端口（端口闭包在 app 内捕获
*Proxy 并持有锁纪律，admin 自身不触碰锁或 reload-owned 状态）；
`internal/app/web_adapter.go` 只负责 composition 与
mux 挂载。账号测活只捕获一次 runtime 快照（ProbeRuntime 端口），因此配置、路由与 provider
implementation 始终来自同一 reload generation；网络 I/O 在快照完成、锁已释放后
执行。嵌入式 UI 资源归 `internal/web/assets`，由 `internal/web/assets.go` 提供给
transport，不由根包承载。

## 生命周期

`internal/runtime.Lifecycle` 是 Proxy 级后台任务的唯一 owner：

- serve 进程只能通过 `applicationRuntime` 调用 `StartRuntimeServices` 与
  `Proxy.Close`；`serveAssembly` 在 process lifecycle 中创建它，并把其 transport
  task 和 Close callback 交给 HTTP server；
- reload catalog refresh 必须通过 lifecycle gate 接纳；
- Close 拒绝新任务，先等待会产生日志的有限任务（Shadow），再 drain request
  log；随后等待 loop/refresh，最后完成 stats、Responses state 和 quota final
  flush；stats 在 final flush 后关闭 SQLite Store。这样 Close 返回后不会有
  Shadow 向已关闭 logger 补写，也不会遗留数据库连接。

quota tracker 的 poll/refresh task 与 persist 编排、Responses state store 的
debounce/worker 都由 `Proxy.Close` 按统一顺序停止。

Web 生命周期归 `internal/web` 的 task owner 与 session store，不混入
`internal/runtime.Lifecycle`：login-session GC 与 AQP/Codex 异步登录轮询都必须经同一
admission gate 启动。transport 关闭时先拒绝新 Web task、取消轮询并等待已接纳
任务；若任务已进入凭据落盘 commit，则允许 save + reload 完成后再关闭 Proxy。
session store 只保存 detached、transport-visible login 更新；GC 只删除超过 TTL 的
done/error 会话，不能删除仍 pending 的会话。

## CLI 与进程入口

根 `main` 函数只绑定 OS 进程边界：构造 `application`，把参数与标准输入输出错误流
交给 `application.Run`，再使用其 exit code 结束进程。`application` 拥有命令表，
`Run` 是唯一 CLI 分发入口（可测试的顶层入口），直接处理无参数、help、未知命令及其
exit code。现阶段已知命令
仍由兼容 adapter 调用既有 handler，保留其进程 I/O 与 `log.Fatal` / `os.Exit`
语义；命令级注入式 I/O 和返回式退出尚未完成。`serveAssembly` 拥有 `serve` 命令、
前台/worker signal 与 HTTP
transport 生命周期；它创建 `applicationRuntime`，后者构造 Proxy、启动运行时服务、
装配 mux/Web、执行 reload projection、持有 transport task，并以 `Close` 结束 Proxy。
`internal/cli/serve/shutdown.go` 保留可独立测试的 HTTP drain primitive；`internal/cli/serve/supervisor.go` 拥有
daemon/supervisor 的 signal 与 pid/probe 编排。child process detach 属性的平台差异归
`internal/cli/serve/detach_unix.go` / `internal/cli/serve/detach_windows.go`。

这是根 package main 内的真实进程装配收敛，不改变任何用户可见 CLI 或 serve 行为。

`internal/cli` 本身是 registry + 进程级 shell：`app.go` 绑定命令表，
`commands.go` 只保留调度循环、`Command` 契约与 takeover/restore 进程入口，
`help.go` 拥有 usage/help 渲染。每个命令域拥有一个子包并导出自身的进程入口
（`Run<Command>`，自查配置）；子包只能向下依赖 `cli/framework`（arg/config 加载/
数字格式化等 CLI 公共 helper）与 `cli/clicommon`（daemon fetch/section 渲染），
不得回依赖根 `cli`（DAG guard 强制）：`cli/login`（login/import）、
`cli/models`（models/test）、`cli/doctor`（doctor）、`cli/presets`（add/presets）、
`cli/stats`（stats/usage）、`cli/audit`（audit）、`cli/status`（serve
status/schedule/routes）、`cli/admin`（pin/unpin/unfreeze）、`cli/diag`
（wire/replay/shadow）、`cli/config`（config init|print|check）、
`cli/account`（logout）、`cli/serve`（daemon/stop/reload 编排）。
`cli/clitest` 是纯测试支撑包：拥有子进程 harness（TestHelperProcess 分发）与共享
fixture，只被各命令包的测试 import。

## 依赖规则

允许：

```text
transport → orchestration → target plan → target executor → provider
internal/app Fusion adapter → internal/fusion
internal/app Shadow adapter → internal/shadow → target plan / bodycapture
internal/web → internal/appapi（ReadAPI / CommandAPI）
internal/app/web_adapter.go → internal/admin（admin.Service）→ internal/appapi.ReadAPI / CommandAPI；Proxy 触达点经 internal/app/web_adapter.go 窄端口注入
lifecycle → background components
conversion entrypoints → conversion registry → pair codecs
analytics adapter → internal/pricing
config (models.dev endpoint/cache policy) / routing → internal/catalog
request routing adapter → internal/routing → internal/catalog / internal/config / internal/protocol / internal/provider
accounts adapter / login / provider builder → internal/accounts
live-event publishers / SSE adapter → internal/observe/events
forward → internal/guard
forward 分片检测会话窗口 → internal/guard/session → internal/guard
target executor / Fusion / Shadow / Web / CLI → internal/observe/requestlog
forward guard 命中审计 / audit CLI / doctor drift → internal/observe/seclog
stats flusher / admin read ports → internal/observe/stats
budget watcher adapter → internal/observe/budget → internal/observe/stats / internal/observe/events / internal/pricing
应用 / CLI / observe / fusion 等组件的日志调用点 → internal/observe/logx（级别过滤叶子包，serve 启动时 SetLevel 一次）
forward / target executor / cache adapter → internal/cache
target executor / Shadow → internal/transport/bodycapture
config / app / provider / providerbuild / login / pricing / CLI 出站调用 → internal/upstreamproxy（上游代理策略叶子包）
probe 执行（daemon 探测 pass / CLI / Web 测活）→ internal/probe → config / provider
wire verdict / target plan → internal/runtime/wirecap → config / provider（值类型）
schedule / health / resolver / quota adapter → internal/runtime
target plan / target executor → internal/protocol
composition root → internal/config → internal/pricing / internal/protocol
composition root → internal/targetexec → internal/protocol / internal/provider
application → serveAssembly → applicationRuntime → Proxy
```

`internal` 的直接仓库依赖采用闭合 allowlist；标准库与外部 module 不在此表中。
该清单与 `internal/archtest/architecture_dependency_dag_contract_test.go` 的
`internalRepositoryImportPolicy` 互为镜像——两处必须同步修改：

- 叶子包（不得依赖其他 `model-proxy/*` 包）：`archtest`（纯测试包）、`cache`、
  `configedit`、`credstore`、`daemonctl`、`display`、`guard`、`httpx`、
  `observe/counters`、`observe/events`、`observe/logx`、`upstreamproxy`、
  `transport/bodycapture`、`webauth`；
- `accounts → credstore`；
- `guard/session → guard`；
- `app → accounts, admin, appapi, cache, catalog,
  config, configedit, credstore, display, forward, fusion, guard, guard/session, httpx, login, observe/budget,
  observe/counters, observe/events, observe/logx, observe/requestlog, observe/seclog, observe/stats, presets,
  pricing, probe, protocol, provider, providerbuild, routing, runtime, runtime/wirecap, shadow,
  targetexec, transport/bodycapture, upstreamproxy, web, webauth`；
- `admin → accounts, appapi, cache, config, configedit, credstore, fusion, login,
  observe/counters, observe/logx, observe/seclog, observe/stats, presets, pricing, probe,
  provider, routing, runtime, runtime/wirecap`（Web admin 应用服务；不得回依赖 app/web/cli）；
- `appapi → fusion, observe/stats, presets, pricing`；
- `cli → cli/account, cli/admin, cli/audit, cli/config, cli/diag, cli/doctor,
  cli/framework, cli/login, cli/models, cli/presets, cli/stats, cli/status, config, display, takeover,
  observe/logx`（registry + 进程级 shell：调度循环、help、takeover/restore）；
- `catalog → upstreamproxy`；
- `cli/account → accounts, cli/framework, cli/serve, config, display, login, providerbuild`（`logout`）；
- `cli/admin → cli/framework, config, daemonctl, display`（`pin`/`unpin`/`unfreeze`）；
- `cli/audit → cli/framework, config, observe/seclog`（`audit`）；
- `cli/clicommon → appapi, daemonctl, display, provider`；
- `cli/clitest → accounts`（纯测试支撑：子进程 harness 与共享 fixture，生产代码不得依赖）；
- `cli/config → accounts, cli/framework, config, display, provider, routing, takeover`（`config init|print|check`）；
- `cli/diag → cli/framework, cli/models, config, daemonctl, display, observe/requestlog, probe, provider, upstreamproxy`（`wire`/`replay`/`shadow`）；
- `cli/doctor → accounts, appapi, cli/clicommon, cli/framework,
  cli/models, config, credstore, display, routing, takeover, observe/seclog, provider`；
- `cli/framework → accounts, config`；
- `cli/presets → cli/framework, cli/login, cli/serve, config, login, presets`；
- `cli/stats → accounts, cli/framework, config, daemonctl, display, observe/stats, providerbuild`（`stats`/`usage`）；
- `cli/status → appapi, cli/clicommon, cli/framework, config, daemonctl, display, routing`（`serve status`/`schedule`/`routes`）；
- `cli/serve → config, observe/logx`；
- `cli/login → accounts, cli/framework, cli/serve, config, display, login, provider`；
- `cli/models → cli/serve, cli/framework, accounts, catalog, config,
  configedit, display, probe, provider, providerbuild, routing, runtime/wirecap, upstreamproxy`；
- `login → accounts, config, display, provider, observe/logx, upstreamproxy`；
- `config → catalog, pricing, protocol, upstreamproxy`；
- `fusion → config, observe/logx`；
- `forward → cache, catalog, config, fusion, guard, guard/session, observe/counters,
  observe/events, observe/logx, observe/requestlog, observe/seclog, protocol, provider, routing,
  shadow, targetexec`（请求转发管线：guard/cache/schedule/failover/Fusion 编排与 target
  plan/executor 装配；不得回依赖 app/web/cli）；
- `observe/budget → config, observe/events, observe/logx, observe/stats, pricing`（config 是
  budgets 生效值所需的值类型，stats 是 analytics bucket 值类型与分钟对齐 seam）；
- `observe/requestlog → config, observe/logx`（config 是生效值 accessor 所需的值类型）；
- `observe/seclog → observe/logx`；
- `observe/stats → observe/counters, observe/logx`；
- `presets → config, configedit, provider`；
- `probe → config, provider`；
- `protocol → observe/logx`；
- `pricing → upstreamproxy`；
- `provider → credstore, display, upstreamproxy`（display 是终端着色/文本格式化叶子工具包）；
- `providerbuild → accounts, config, display, provider, observe/logx, upstreamproxy`；
- `routing → catalog, config, protocol, provider`（均为值类型消费）；
- `runtime → config, runtime/wirecap, provider, observe/logx`；
- `runtime/wirecap → config, provider`；
- `takeover → catalog, config, routing, observe/logx`；
- `shadow → targetexec, transport/bodycapture`；
- `targetexec → cache, config, protocol, transport/bodycapture, provider, observe/logx`；
- `web → appapi, observe/logx, observe/requestlog, observe/stats, pricing, webauth`。

`internal/display` 拥有终端 stdout/stderr 着色状态与文本格式化工具（`C`/`Green` 等
颜色 helper、`ProgressBar`、`Money`、`Format*`、`Pad`、`Or`、`Truncate`、
`StatusColor`），是纯标准库叶子包，任何层都可 import；`internal/provider` 回归
后端实现 owner，不再持有 display helper。

`internal/takeover` 拥有客户端配置的备份、改写与恢复（claude/opencode/codex/pi），
只消费 config DTO、catalog 元数据与 `routing.DefaultModelMetadata` 保守回落；
route derivation、catalog 加载（`internal/config`）与 source 标记
（`internal/routing.HydrateModels`）由本包的 `ModelFactsFor` 计算并注入
`RunTakeover`（调用方传入 HOME seam 以定位 catalog 缓存），包内不读取
应用运行时。

`internal/upstreamproxy` 拥有全部出站调用的上游代理策略：解析链
`providers.<name>.proxy_url → 顶层 proxy → HTTPS_PROXY/HTTP_PROXY(+NO_PROXY) 环境变量
→ OS 系统代理 → 直连`，`off`/`direct` 在该级强制直连并终止回落；支持
http/https/socks5 URL；自动来源（env NO_PROXY / 系统代理）对 loopback 恒绕过，显式配置的
代理 URL 对 loopback 同样生效；PAC/WPAD 不解析。系统代理
每进程探测一次（`Resolver` 内 sync.Once，改动需重启）；macOS 读 `scutil --proxy`、
Windows 读注册表 Internet Settings、Linux best-effort 读 gsettings。转发/fusion/
shadow 流量由 app 的 `Proxy.clientFor` 按请求快照解析、按生效代理身份池化
transport（`internal/app/proxy_transport.go`）；usage/quota/login/pricing 等维护类
调用走 `AutoTransport()`（config 全局值经 `SetDefaultProxy` 由组合根在启动/reload
发布），不做 per-provider 区分。

`internal/archtest` 是架构契约测试的 owner：纯测试包、仓内零依赖，经 `repoRoot`
（`runtime.Caller` 定位模块根）以模块根相对路径扫描全仓生产文件。其中的
`internal/archtest/architecture_dependency_dag_contract_test.go` 是这份 allowlist 的可执行镜像：它必须发现并
分类全部生产 `internal` package、拒绝未声明边、拒绝环；新增 package 不能靠遗漏目录
绕过检查；仓库内 import 不得使用 dot/blank alias 绕过 owner matcher。扩大依赖前必须
先证明 owner 边界仍成立并更新本节，不能只放宽测试。

模块构造入口是闭合的：`internal/app/runtime.go` 独占 serve process 的
`NewProxy` / `StartRuntimeServices` / `NewWebServer` 装配；`internal/forward/plan.go` 构造
`targetexec.Plan`，`internal/forward/attempt.go` 构造 `targetexec.Attempt`，
`internal/forward/plan.go` 绑定 `targetexec.Executor`；`internal/forward/plan.go`
构造 request `routing.Planner`；`internal/forward/fusion.go` 绑定 `fusion.Engine` 及其 ports；
`internal/app/proxy.go` / `internal/app/proxy_reload.go` 构造启动与 reload generation 的 Shadow
runtime；`internal/app/web_adapter.go` 构造 `internal/web.Server`。其他 `internal/app`/`internal/forward`
文件只能消费这些 seam，
不得建立第二套 owner。owner 符号只能通过已审查的直接形态引用（`pkg.F(...)` /
`pkg.T{...}` / `receiver.Method(...)` / 裸标识符调用）；function value、method value、
type alias 和 method expression 都会被守卫计为新的引用点并判定违规。

禁止：

- `internal/web.Server` 持有 `*Proxy`，或 handler 绕过 `ReadAPI` / `CommandAPI`
  直接访问应用运行态；
- `internal/web` 用裸 `go` 启动绕过其 task owner 的后台任务；
- `internal/admin` 承担 HTTP routing、session/task lifecycle、直接触碰锁或
  reload-owned 状态，或 `internal/app/web_adapter.go` 恢复应用逻辑；
- `internal/fusion` 访问 Proxy、HTTP、runtime/observability owner，或应用层
  `runFusion` 恢复 fan-out/quorum/body/registry 策略副本；
- `internal/shadow` 访问 Proxy、lifecycle、runtime Manager、request log、
  metrics/events 或应用层 `runShadow` 恢复 detached HTTP、
  auth、转换和 bounded capture；
- Fusion/Shadow adapter 复制 provider lookup、协议选择或 target plan 逻辑；
- request、response、SSE 各自维护协议方向 switch；
- `targetexec.Executor` import/持有 `Proxy`、application composition owner 或其他
  runtime aggregate；
- `internal/forward` 重新实现 HTTP、转换、retry 或 Shadow 编排，或回依赖
  `internal/app`/Proxy/`p.mu`；
- 普通 route/Fusion 绕过 `newTargetAttempt` 直接拼装执行器输入；
- `cli_serve.go` / `internal/cli/serve` / reload 绕过 `internal/runtime.Lifecycle` 启动 Proxy 级
  goroutine；
- `main` 函数恢复命令解析、serve/daemon 编排，或根包恢复第二个顶层命令
  分发器；
- 将根 `package main` 的 application/serve runtime 伪装成可 import 的
  callback bag；真实边界是薄 main + `internal/cli` / `internal/app`；
- `internal/config` import `internal/pricing`、`internal/protocol` 以外的
  `model-proxy/*` 包，或在根包恢复配置类型别名、加载 wrapper 等任何配置实现；
- `internal/catalog` 反向依赖 Config、Proxy、Provider、Web/CLI 或任意
  `model-proxy/*` 包；
- `internal/routing` 反向依赖 Proxy、runtime Manager、target executor、Web/CLI
  或 `internal/catalog` / `internal/config` / `internal/provider` 值类型之外
  的仓库包；应用层恢复 request
  profile、capability/context、cross-route 或 cooldown terminal 策略副本；
- `internal/accounts` 读取 HOME、反向依赖 Config、Proxy、Provider、Web/CLI，
  或承担网络验证、Provider 构建、reload 与路由选择；
- `internal/observe/events` 反向依赖 Proxy、HTTP/Web、Config、Provider 或任意
  `model-proxy/*` 包；应用层 SSE adapter 重新声明事件类型或拥有 ring/fan-out 状态；
- `internal/observe/requestlog` 反向依赖 Proxy、RouteTarget、Provider、
  protocol、Web/CLI 或 `config` 值类型之外的 `model-proxy/*` 包；应用层重新声明 Record、writer、
  logger、query heap 或 Shadow 聚合；
- `internal/observe/seclog` 反向依赖 Proxy、Guard、Config、Provider、Web/CLI
  或任意 `model-proxy/*` 包；应用层不得把命中内容/秘密值塞进 Record，
  或在应用层重新声明 Record、writer、logger 或 query heap；
- `internal/cache` 反向依赖 Config、Proxy、Provider、protocol、events
  或任意 `model-proxy/*` 包；应用层重新声明 store、entry 或 recorder；
- `internal/transport/bodycapture` 反向依赖 request log、protocol、Proxy、
  Config、Provider 或任意 `model-proxy/*` 包；
- `internal/runtime/wirecap` 反向依赖 Proxy、HTTP/Web/CLI 或 `config` /
  `provider` 值类型之外的 `model-proxy/*` 包；应用层重新声明 verdict、
  provider/model 级 capabilities map、其锁或 model_caps.json 文件格式；
- `internal/runtime` 依赖 `config`/`provider` 值类型与 `runtime/wirecap` 之外的
  Proxy、HTTP/Web/CLI 或持久化实现；应用层恢复 health/sticky/pin/model-lock/paramBlock/spread/quota
  的第二份 map 或互斥锁；
- `internal/pricing` 反向依赖 `main` 的 YAML 配置、Proxy、Web 或通用 helper；
- `internal/protocol` import 任意 `model-proxy/*`，或反向读取 Config、Provider、
  Proxy、Web/CLI；Fusion 直接 import protocol 绕过 `targetexec.Plan`；
- 将 config generation 内的 map 原地修改。

## 专题文档

- 路由与失败：`routing-and-failure.md`
- 运行态与生命周期：`runtime-state.md`
- Provider pools：`provider-pools.md`
- 请求感知路由：`request-routing.md`
- 协议转换：`protocol-conversion.md`
- Fusion、Shadow、Cache 与观测：`fusion-shadow-cache.md`
- Web/API：`../web-api.md`

架构边界的静态回归位于 `internal/archtest/architecture_*_contract_test.go` 套件（基于 go/ast 检查
`internal/web.Server`、`internal/app` Web composition/application adapter 的精确字段集合与
consumer-owned ports、禁止 transport 访问应用运行态、账号测活的单次 runtime
snapshot、internal 叶子包 import（含
accounts/catalog）、
`internal/observe/events`、`internal/cache` 与各自 `internal/app` adapter 的职责、
`internal/config` 依赖 allowlist、禁止恢复根配置兼容 facade、薄 main 与 application/serve/runtime 的
process-composition owner 边界、`internal/fusion`/
`internal/shadow` import 与 owner 边界，以及 Fusion/Shadow 不绕过
`targetexec.Plan`，不是字符串扫描）。`internal/archtest/architecture_dependency_dag_contract_test.go`
额外检查
internal package 发现全集、闭合 direct-import allowlist 与无环性；交互合同检查应用层构造
入口的精确 owner/callsite、planner 构造值的调用接收者、executor result 的 committed
分支以及同一代 Shadow runtime 的采样/准入/执行数据流，并为 guard helper 保留
正向/反向 control，避免空扫描或仅有同名调用造成假绿。
`Proxy` / `runtime.Manager` 的语义所有权检查
合并 package 内全部生产 Go 声明，不绑定单一物理文件；adapter/facade 的精确形状
约束仍保持 file-scoped。行为与并发验证仍按
`docs/engineering/testing.md` 执行。
