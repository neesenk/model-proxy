# 请求感知与路由推导

## 适用范围

修改 `internal/routing/request.go`、`internal/app/target_pipeline.go`、models.dev
catalog、context overflow retry、route derivation（隐式路由的继任者）或 route warnings 时必读。

## 请求画像

`internal/routing.ProfileRequest` 在每次路由决策内只计算一次，不得在候选目标
循环内重复扫描：

- `HasImage`：扫描已知图片标记；
- `HasTools`：识别 tools；
- `EstimatedTokens`：CJK 每 rune 约 1 token，其他文本约 bytes/4，跳过长度
  ≥100 字符的 base64 run。

## 模块边界

`internal/routing` 是无状态策略包，只允许依赖 `internal/catalog`、
`internal/config` 与 `internal/provider` 值/接口类型，不得依赖 `Proxy`、`internal/runtime`、
`internal/targetexec` 或任何 I/O owner。`routing.NewPlanner(PlannerInput)`
隐藏内部字段；输入来自一次 `RuntimeSnapshot`，generation-owned map 在 reload
时只交换、不原地修改。

`internal/app/target_pipeline.go` 是唯一边界桥：它从 HTTP request 提取
force-provider 字符串；`requestRoutingScheduler` 捕获同一快照的 config、parent
identity、route keys 与 generation，并通过 `Proxy.schedule` 进入
`internal/runtime.Manager`。`serveOnce` 每个 pass 只构造一个 Planner，同时用于
主动 `Apply` 与反应式 `ContextOverflowRetry`，禁止重新读取 Proxy 或构造第二份
generation。

`internal/catalog` 作为无仓库内依赖叶子包拥有 models.dev slim projection、
canonical-owner 去重、HTTP/ETag/TTL 刷新和磁盘缓存。`internal/config/modelscatalog.go` 只注入 HOME
cache path 与 `MP_MODELSDEV_URL`；Config 中的 provider/route 名单遍历与
fallback/source 策略由 `internal/routing/model_metadata.go`（`HydrateModels`）拥有。daemon
启动时同步加载 catalog（最多受 source HTTP timeout 限制），reload 后的刷新才经
lifecycle gate 异步执行；catalog 为 nil 时请求感知路由整体 no-op，不能因此把
所有目标过滤为空。

缓存写入必须使用目标目录内的唯一临时文件再 rename，多个进程不得共享固定
`.tmp`。HTTP 200 的 payload 必须先成功解析才能替换旧 cache；malformed payload、
非 2xx 或 fetch 错误有旧 cache 时告警并回落旧值，无旧 cache 时返回空 catalog
与 non-nil error。304 只有在已有 cache 时有效，并要持久化新的 `fetched_at` 与
可用 ETag。

## 能力判断

provider config 的 `capabilities: {model: [image, tools]}` 优先。某 model 一旦显式声明，image/tools 完全以声明为准，不再查询 catalog。这是 aqp、codex、volcengine 等 catalog 盲区的逃生口。

未声明时查询 models.dev：

- catalog 查不到模型，对 image/tools 保守视为不支持；
- context 查不到时不阻断；
- context window 始终来自 catalog。

池化虚拟 provider 必须通过 parentOf 读取父配置的 capabilities。

## 改道

1. 在当前 route 内过滤满足能力和 context 的目标；
2. 全部不匹配时进入跨 route pool；
3. 跨 route pool 仍使用正常 schedule 排序；
4. 二次 schedule 返回空时回落原 ordered，宁可尝试不匹配目标，也不能零尝试直接 502；
5. pin 和 force-provider 禁止跨 route 改道。

`CollectCrossRoute(expanded, nil)` 明确定义为“保留所有 concrete target”；Fusion
recipe 仍为 route-local，不进入跨 route pool。去重 identity 是
`{provider, model, protocol}`，冲突时保留更低 priority，池化虚拟 provider ID
互不折叠。

## Context overflow retry

普通 4xx commit 前最多 peek 64 KiB。命中保守的 context overflow 错误且尚未重试时，选择 catalog 中严格更大 context 的目标重试一次。

- 没有更大目标时，peeked bytes 通过 MultiReader 原样 commit；
- overflow 不进入熔断；
- 只计 failover，不计 served request/latency；
- effective targets 必须回传 cooldown/retry 层，不能继续按原 route 判定。

## 路由推导（routes 自动化）

路由表完全由 config 推导（`DeriveRoutesFrom` / `RouteTable`，
`internal/routing/implicit_routes.go`）：每个 provider 的 `models:` 按暴露名聚合为
多目标 route —— 暴露名 = 模型名，或 provider 的 `alias:` 改名（如 kimi-code 的
`k3` 暴露为 `kimi-k3`，与其他 provider 的同名模型聚合）；target 保留上游真实
模型名，priority 继承 provider 的 `priority:`（lower wins），并按
`(priority, provider)` 排序保证确定性。

- 推导是纯 config 计算：不读凭据/login 状态（构造与 reload 各恰好一次
  `DeriveRoutesFrom(cfg)`，由 `internal/archtest` 的 owner 契约保护）；CLI
  （`routes`、`models`、`test`、doctor、takeover）与 daemon 用同一 `RouteTable`；
- 显式 `routes:` 条目**整条覆盖**同名推导路由（用于 fusion 目标、`protocol:`
  协议转换声明、特殊排序）；target 省略 priority 时同样继承 provider priority；
- `claude_mapping` / `shadow` 的 key 校验对象是「显式 route key ∪ 推导暴露名」
  （`Config.RouteExposedNames`）；alias 的 key 必须在该 provider 的 `models:`
  中，且一个 provider 内一个暴露名只能映射一个上游模型（validate 报错）；
- route-name sticky 可持久化，session sticky 不持久化；
- codex 的推导 target 经 `ProtocolHint` 填充 `protocol:"responses"`
  （`internal/provider/protocol_hint.go`）。

## 配置风险警告

`ConfigRoutingWarnings`（`internal/routing/route_warnings.go`）只警告、不阻止启动：

- reasoning replay 模型使用协议转换；
- provider wire protocol 无法表达且无法转换（当前无实例：codex Responses 已可
  转换，`provider.WireProtocolNote` 现对所有 provider 返回空，此类为 inert marker）。

warning 同时出现在 daemon log、`/api/status.warnings`、models、doctor 和 config check。

## 回归测试

- capabilities override 权威性和 parentOf 解析。
- nil catalog no-op。
- in-route/cross-route/回落/pin/force。
- force-provider 即使能力不匹配也不得被替换到其他 route。
- force-provider 是代理内部控制参数：`?force_provider=` query 由 executor 从
  上游 URL 剥除（`stripInternalQuery`），不透传给上游 API。
- context overflow 的单次重试与 body 恢复。
- 路由推导：按名聚合、alias 改名、priority 继承、显式 routes 覆盖、fusion 显式路由。
- reasoning/codex protocol warnings。
- catalog fresh/stale/force、ETag 304、malformed 200、非 2xx、损坏 cache 与并发
  原子写。
