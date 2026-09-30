# 请求感知与路由推导

## 适用范围

修改 `internal/routing/request.go`、`internal/forward/plan.go`、models.dev
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
`internal/config`、`internal/protocol` 与 `internal/provider` 值/接口类型，不得依赖 `Proxy`、`internal/runtime`、
`internal/targetexec` 或任何 I/O owner。`routing.NewPlanner(PlannerInput)`
隐藏内部字段；输入来自一次 `RuntimeSnapshot`，generation-owned map 在 reload
时只交换、不原地修改。

`internal/forward` 是唯一边界桥：`forward.ForcedProviderFromRequest` 从 HTTP request 提取
force-provider 字符串；`internal/forward/plan.go` 的 `requestRoutingScheduler` 捕获同一快照的
config、parent identity、route keys 与 generation，并经注入的 schedule 端口
（app: `Proxy.schedule`）进入 `internal/runtime.Manager`。`serveOnce` 每个 pass 只构造一个
Planner，同时用于
主动 `ApplyWithProfile` 与反应式 `ContextOverflowRetryWithProfile`，禁止重新读取 Proxy 或构造第二份
generation。

`internal/catalog` 作为仅依赖上游代理策略叶子 `internal/upstreamproxy` 的元数据源包拥有 models.dev slim projection、
canonical-owner 去重、HTTP/ETag/TTL 刷新和磁盘缓存。`internal/config/modelscatalog.go` 只注入 HOME
cache path 与 `MP_MODELSDEV_URL`；Config 中的 provider/route 名单遍历与
fallback/source 策略由 `internal/routing/model_metadata.go`（`HydrateModels`）拥有。查找顺序：
provider `catalog_alias[model]` 映射的目录 id 优先，未命中回落 model id 直查（陈旧映射不会遮蔽直接命中），
再落空用保守默认元数据。`catalog_alias` 只改元数据查找，不改路由与对外名（改名用 `alias`）。daemon
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

## 档位策略（route_policy）

`route_policy:` 是 per-route 的声明式档位策略（键 = exposed route 名，与
`shadow:`/`mcp_routes:` 同属「route 作用域的额外行为」）。协议解析见
`internal/config` 的 `RoutePolicy`/`RouteBand`/`BandWhen`，判定与应用在
`internal/routing` 的 `PickBand`/`PreferTarget`/`SelectGrade`（叶子纯函数）与
`internal/forward` 的 `applyRoutePolicy`/`applyRoutePolicyGrades`（编排）。

语义（实现契约）：

1. **信号来自唯一一次画像扫描**（`routing.ProfileRequest`）：`estimated_tokens_min/max`
   对应 `Profile.EstimatedTokens`，`follow_up` 对应 `Profile.FollowUp`（body 含
   assistant 轮次的字节标记判定，即 fusion `first_turn_only` 门对「后续轮」的同一概念），
   另有 `has_tools`/`has_image`。`when` 里列出的条件全部成立才算命中（AND），
   空 `when` 是校验错误。**注意**：能力路由在 catalog 缺失时用空画像 no-op，而 bands 读
   `pipeline.rawProfile`——档位信号是纯 body 事实，不依赖目录，因此目录不可用时仍然生效。
2. **bands 按序求值，首个命中者胜**；无命中 = 保持调度顺序。
3. **应用是「前置」不是「硬选」**：命中目标移到 `ordered` 最前，其余目标保留为 failover；
   命中目标不在（能力过滤已剔除或本就不在该 route）时返回原顺序——策略不得造成零尝试。
4. **硬选择优先**：pin（`force`）或 force-provider 生效时整条策略跳过，与「硬选择不进入
   请求感知改道」同一规则。
5. **响应 cache**：bands 的判定是 `(body, config)` 纯函数，band-only route 的缓存语义不变。
   （后续切片引入的会话 latch / decisions selector 才需要绕过缓存，见各自章节。）
6. **校验分层**：`internal/config` 只校验本地不变量（route 名存在、provider/recipe 存在、
   协议归属、min ≤ max、`when` 非空、band 目标不得自带 `protocol:`——协议要写在 route
   target 上）；「band 目标是否真的被该 route 服务」由 `internal/routing.ConfigRoutingWarnings`
   在启动/doctor/config check 出告警（跨层事实归 routing owner，config 不能反向依赖）。
7. **fusion 目标**：band 可以指向 `{provider: fusion, model: <workflow>}`，管线把它当普通
   fusion target 编排（fusion 内部的降级语义不变）；这是「fusion 从唯一入口变成档位之一」
   的落点。

### 档位分组（grades）

`route_policy.<route>.grades` 把 route 的目标划分成命名模型档，是「**模型选择层 vs 调度层**」
分离的落点：选择层只输出 grade 名，调度层只在选中 grade 内排序。

- **两层顺序**（graded route 的实际管线）：`schedule(全部 targets)`（调度投影）→
  `ApplyWithProfile`（能力/context 过滤，仍保留跨 route 兜底）→ 按 grade 分组并在**档内**
  再过滤一次（`filterGradeTargets`；空档就空着，不跨档兜底）→ `SelectGrade` 选档 →
  `buildGradeOrdered` 按 fallback 拼接最终顺序 → 目标循环。选择层看到的只有 grade 级候选
  与「该档是否可用」，**不含** surplus/配额数值；调度层的排序语义（surplus / peak / 熔断 /
  粘滞 / 池化 spread）不变，作用域收窄到选中档的目标集。
- **分组**：grade 顺序 = 目标在 route 中首次出现的顺序；不在任何 grade 的目标作为
  ungraded 保留在末尾。同一 target 不得声明进多个 grade——配置校验直接拒绝（跨 grade
  重复会让分组、next_grade 与 eval 配对依赖 map 迭代序，每请求不确定）。
- **选择优先级**：`routing.SelectGrade` 按 **active latch > selector choice > band** 返回 grade 名；
  无显式选择时保持自然顺序（各档按原序 + ungraded）——即未配置 grades 的旧行为。
- **fallback 模式**（默认 `any`）：
  - `any`：选中 grade 后置，其余 grade 按 route 顺序追加，最后追加 ungraded。
  - `next_grade`：只追加选中 grade 的下一个 grade，然后 ungraded。
  - `strict`：只尝试选中 grade，不追加任何回退。**注意**：若选中档过滤后为空，本次请求会
    零尝试直接终局——这是显式选择，与「宁可尝试不匹配目标也不零尝试」的默认规则相反；
    接受这种失败模式才用它。这类终局会被 `recordLatchOutcome` 计为坏运行，从而让会话在
    escalation 的 `dwell` 内升到更强的档。
- **与 latch 的协作**：graded route 的 escalation 可用 `grade: <name>`，latch 目标编码为
  `"grade:<name>"`；latch 落在某 grade 时强制选该 grade，即使其过滤后为空也优先。
- **selector 候选**：graded route 下候选为 grade 级别，ID 用 `g0..gn`，rubric 为
  `"grade <name>"`；enforce 模式置信度达标时把选中 grade 前置。
- **响应 cache**：仅有 bands 的 graded route 仍保持纯函数缓存语义；启用 escalation 或
  selector 的 graded route 绕过响应 cache。
- **校验**：grade 名非空、每个 grade 至少一个 target、target 必须属于该 route、同一 target
  不得出现在多个 grade（报错信息含 target 与相关 grade 名）；band/escalation
  在 graded policy 下必须引用已声明 grade，或用能无歧义解析到唯一 grade 的 target。

### 会话内升级（escalation）

`route_policy.<route>.escalation` 是会话级的「坏运行计数器 → 临时升档」机制：

- **状态**：`runtime.Latch{Target, Since, BadRuns}`，按 `(x-claude-code-session-id, route)` 索引
  （与 repeat_turn 滑窗同粒度：一条 route 的坏运行/升级不影响另一条），内存态、
  不持久化、随 config generation 清空——owner 是 `internal/runtime.Manager`（见
  `docs/architecture/runtime-state.md`）。
- **信号闭集**：`bad_signals` 为闭集，取值只能是 `upstream_error`、`empty_ok`、`repeat_turn` 之一或组合；
  未知取值在校验时报错。默认仍只含 `upstream_error`（不配置 escalation 零行为变化）。
  - `upstream_error`：请求终局未 commit 且客户端未断开（收到终局错误）。客户端侧转换失败
    400（请求对全部候选均不可转换、上游从未被联系）**不计入**——与 guard 拦截同口径，
    重发同一不可转换请求不会误升级 latch。规划级失败同口径：planTarget 失败（未知 provider
    配置）、缺 runtime provider implementation（未登录，fail-closed 跳过）、Responses state
    展开失败导致目标在上游接触前被丢弃，且整轮没有任何 target 被真正尝试时，同样不计入坏
    运行（`serveResult.planningErr`）——配置/规划层缺口不是上游判决。
  - `empty_ok`：请求 commit 为 200，但最终写回客户端的响应体字节数为 0。该谓词是保守的：
    只按最终客户端字节计数，不误判带空 `content` 或纯 `tool_use` 的合法 JSON/JSON 帧。
  - `repeat_turn`：同一 `TurnKey`（`requestlog.ComputeTurnKey`，request log 与该信号的唯一权威来源）
    在同一 session+route 的 `escalation.dwell` 滑窗内再次出现，说明客户端在重发同一轮。
    状态由 `runtime.Manager` 持有，generation 门控、内存 only、 bounded。
- **触发**：任一配置的坏信号贡献一次坏运行；多个信号可在同一次 outcome 中叠加。
  连续 `consecutive` 个坏运行（默认读取侧不处理 0；配置校验要求 ≥1）后，`Target` 设为
  `escalation.target/Grade`，`Since` 刷新，`BadRuns` 清零；下一次同会话请求会把该 target/grade
  前置到调度顺序最前。
- **滞回**：已升级会话在 `dwell`（默认 `30m`）内不回退；好运行（commit 且无配置的坏信号触发）
  只清零 `BadRuns`，保留 latch target。
- **与 band 的优先级**：先查 latch，latch 生效时跳过 bands；latch 过期/不存在时才走 bands。
  latch 目标不在当前 ordered 集（能力过滤剔除或 operator disable）时 latch 同样不生效——
  顺序不变、不记录 `source=latch`，照常走 bands/selector。graded 路径 `resolveLatchGrade`
  同一严格度：目标不可解析到档、**或档在当前过滤后集合中无代表**（`gradeGroupHasTargets`
  检查，与非 graded 路径的 TargetIndex 检查对称）均视为不生效，且不抑制 selector。
- **与硬选择的优先级**：pin / `x-mp-force-provider` 生效时整条策略（含 latch）跳过，与 bands 同一规则。
- **响应 cache**：启用 escalation 的 route 必须绕过响应 cache（与 force-provider/pin 同语义），
  否则已升级会话可能命中便宜档缓存。

### 判定选择器（selector）

`route_policy.<route>.selector` 是会话级/请求级的 decisions 模型判定：每次请求把当前
`ordered` 目标集作为候选（`c0..cn`，`rubric` 取自 target 的 `rubric`），调用一个
`protocol: decisions` 模型（如 TypeSafe Jev）拿到最佳选择与难度评分。

- **优先级链**：在 `!force && forcedProvider == ""` 分支内，顺序为
  **latch > selector(enforce 且置信度达标) > bands > 静态顺序**。selector 失败、低置信、
  shadow 或未配置时顺序不变。
- **候选集**：当前 `ordered` 中的目标（能力/上下文过滤后、或 latch/band 已调整后的顺序），
  用 `routing.PreferTarget` 把选中目标前置，其余保留为 failover，不硬选、不清空、不切换
  provider 身份（池化虚拟 ID 保持正确）。
- **调用与超时**：每请求至多一次，timeout 来自 `SelectorConfig.TimeoutDuration()`（默认 800ms）；
  任何失败 fail-open 回退到上一步顺序。all-cooldown 等待重试的后续轮次复用首轮缓存的
  selector 结果（缓存于 `serveState`，与 rawProfile 同一模式），不重复发起付费 decisions
  调用、不重复发 `route-select-<requestID>` live 事件；enforce 的首选目标在新一轮 ordered
  中缺席（如被冷却调度剔除）时保持当轮顺序，该轮 decision 降级为 fallback。graded route
  同一规则：缓存的 enforce 档位在新一轮过滤后没有任何代表目标时，`buildGradeOrdered`
  回落自然顺序，该轮 decision 同样如实降级为 fallback（selector choice 记录但不标
  enforced），不把主响应归因到根本没被服务的 grade。
- **mode**：`shadow`（默认）只记录不行动；`enforce` 在 `confidence ≥ SelectorConfig.ConfidenceThreshold()`
  时前置选中目标。
- **响应 cache**：启用 selector 的 route 必须绕过响应 cache，与 escalation 同一语义。
- **实现**：route selector 与 fusion selector 共用 `internal/forward/decisions.go` 的
  `pipeline.callDecisions` 共享 helper，事件/统计/日志语义与 fusion selector 同形状
  （live event provider marker 为 `route-select:<model>` / `fusion-select:<model>`）。
- **配置限制**：route 级 selector 禁止 `direct_score_max` 与 `panel_top_k`（fusion-only），
  非 0 校验报错；instruction/difficulty_instruction 长度沿用 fusion 的 rune 上限。

### L2 成对评估（eval）

`route_policy.<route>.eval` 为已配置 `grades` 的 route 开启「主响应 vs 配对档响应」的
离线成对评估（L2 strong labels）。eval 在请求 commit 后异步触发，不影响 live 请求。

- **触发条件**：`pin` / `x-mp-force-provider` 等硬选择会跳过整条 route_policy，因此也
  跳过 eval；只有正常经过档位策略且 `routingDecision.Grade != ""` 的请求才可能被评估。
- **采样**：`sample_rate` 控制 eval 采样率，范围 `[0, 0.5]`，默认/缺省取 `0.05`。
  采样使用 `Proxy.evalRand`（测试可注入确定性函数），并在 commit 后才做决策；响应体
  在 `CaptureResponse` 时先无条件缓存到 `evalPrimaryBodies`，commit 后若被采样则取走。
- **配对规则**：`pair` 支持 `opposite`（默认）或 `grade:<name>`。
  `opposite` 使用档位在 route 目标链中首次出现的顺序（与调度 fallback `next_grade` 共用 `forward.GradeOrder`，源为该 route 的 expanded target 顺序，非 grades map 迭代序）：主档的下一档为配对档；最后一档回退到
  前一档。该规则只依赖目标链顺序，不依赖价格数据。
- **影子请求**：eval 使用与 legacy shadow 同一套 `shadow.Runtime` 并发门和生命周期准入；
  影子目标在配对档内按健康/禁用状态挑选一个可运行目标（含池化虚拟 ID 解析）。
- **裁判模型**：`judge` 必须是 `protocol: decisions` 的 provider/model，调用
  `internal/forward/decisions.go` 的 `CallEvalJudge`。裁判 prompt 包含主/影子两个响应正文，
  题型为 `model_choice`，候选 `{primary_better, shadow_better, tie}`。
- **敏感日志**：eval judge 调用标记 `Sensitive`，request log 中只保留元数据
  （provider/model/status/latency/usage），request/response body 置空，避免用户响应正文
  进入日志。
- **verdict 汇总**：shadow 记录的 `diagnostics` 会写入 `eval_shadow_grade` 与 `eval_verdict`；
  `model-proxy routing report` 按 `(route, primary_grade, shadow_grade, verdict)` 聚合展示，
  供离线校准档位策略。

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
`(priority, provider)` 排序保证确定性。请求方向把 body 的 model 改写为上游
真实名；响应方向对称地把客户端可见字节的 model 归一回暴露名（契约见
`protocol-conversion.md` 接线要求的「响应 model 归一化」）。

**配置真相 vs 生效表**：daemon 在推导之上还有一张生效表（`expandedRoutes`，
reload 时重建）：池化 target 展开为虚拟账号，并且**没有 runnable impl 的
provider 整体剔除**（凭据池为 0 账号墓碑、构建失败、static 无 plural 池——
如 zcode 登出后；`Proxy.expandTarget` 过滤，`fusion` 伪 provider 例外，forward
在 impl 查找前拦截它）。全部目标被剔除的 route 从生效表整体消失：不进
schedule 链、不进 `GET /v1/models`、forward 终局 not-found。补上账号（login →
reload 重建）即回归。CLI `routes` 与 Web Config 页的 `GET /api/config.routes`
仍是配置真相（`RouteTable(cfg)`，不含凭据状态），两者刻意分层。takeover 的
模型面在线下镜像同一剔除（`providerbuild.AuthenticatedProviders`：同一
BuildProviders pass + 各 impl 的 `AuthReady`，虚拟账号折回父名，只出布尔值；
`takeover.PruneUnauthenticatedRoutes` 在 `fusion` 例外与空集 abstain 上与
`expandTarget` 同口径——见 `docs/client-takeover.md`「未登录面」）。

- 推导是纯 config 计算：不读凭据/login 状态（构造与 reload 各恰好一次
  `DeriveRoutesFrom(cfg)`，由 `internal/archtest` 的 owner 契约保护）；CLI
  （`routes`、`models`、`test`、doctor）与 daemon 用同一 `RouteTable`，takeover
  在 `RouteTable` 之上叠自己的可服务面投影（chat 可达 / 禁用 / 未登录，见
  `docs/client-takeover.md`）；
- 显式 `routes:` 条目**整条覆盖**同名推导路由（用于 fusion 目标、`protocol:`
  协议转换声明、特殊排序、claude-* 别名）；target 写紧凑形式 `"provider/model"`
  字符串即可，仅当要设 `priority`/`protocol` 时才用 `{provider, model, ...}`
  map 形式；target 省略 priority 时同样继承 provider priority；
- 请求里的模型名也可直接写 `"provider/model"`（如 `deepseek/deepseek-v4-pro`）：
  没有同名精确路由、且前缀是已配置 provider 时，按裸模型名查路由并把目标收窄到
  该 provider，语义等同一次性 force-provider（绕过响应 cache、禁止跨 route 改道）；
  精确同名路由永远优先，前缀不是 provider 时整串按普通模型名处理（兼容
  openrouter 风格带 `/` 的模型名）；
- `shadow` 的 key 校验对象是「显式 route key ∪ 推导暴露名」
  （`Config.RouteExposedNames`）；alias 的 key 必须在该 provider 的 `models:`
  中，且一个 provider 内一个暴露名只能映射一个上游模型（validate 报错）；
- route-name sticky 可持久化，session sticky 不持久化；
- codex 的推导 target 经 `ProtocolHint` 填充 `protocol:"responses"`
  （`internal/provider/protocol_hint.go`）。

## 配置风险警告

`ConfigRoutingWarnings`（`internal/routing/route_warnings.go`）只警告、不阻止启动：

- reasoning replay 模型使用协议转换；
- provider wire protocol 无法表达且无法转换（当前无实例：codex Responses 已可
  转换，`provider.WireProtocolNote` 现对所有 provider 返回空，此类为 inert marker）；
- `route_policy` band 的目标不被该 route 服务（永不命中，多半是名字写错）。

warning 同时出现在 daemon log、`/api/status.warnings`、models、doctor 和 config check。

## 回归测试

- capabilities override 权威性和 parentOf 解析。
- nil catalog no-op。
- in-route/cross-route/回落/pin/force。
- 档位策略：band 首命中前置、无命中/目标缺席保持原序、force-provider 与 pin 跳过策略、
  `follow_up` 标记在三种协议形态下的判定、band 目标不在 route 时的启动告警；grades 的分组/过滤/回退
  （any/next_grade/strict）、latch 强制选 grade、selector 的 grade 级候选、同一 target 跨 grade
  重复的配置拒绝、重试轮 enforce 目标/档位缺席时 decision 如实降级 fallback。
- force-provider 即使能力不匹配也不得被替换到其他 route。
- force-provider 是代理内部控制参数：`?force_provider=` query 由 executor 从
  上游 URL 剥除（`stripInternalQuery`），不透传给上游 API。
- context overflow 的单次重试与 body 恢复。
- 路由推导：按名聚合、alias 改名、priority 继承、显式 routes 覆盖、fusion 显式路由。
- reasoning/codex protocol warnings。
- catalog fresh/stale/force、ETag 304、malformed 200、非 2xx、损坏 cache 与并发
  原子写。
