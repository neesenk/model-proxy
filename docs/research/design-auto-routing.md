# 设计：route 级档位策略（auto-routing）

> 状态：设计文档（非实现契约）。实现契约以 `docs/architecture/request-routing.md`（档位策略一节）
> 与 `docs/architecture/runtime-state.md`（档位 latch 一节）为准；本文记录决策背景、取舍与实现顺序。
>
> 落地进展：切片 0–4 均已实现（bands → latch → decisions selector → fusion 档位），
> 实现偏差逐条记在对应小节的「偏差登记」里；行为回归用例在 `internal/routing`、`internal/config`、
> `internal/forward`、`internal/runtime`。
> 反馈与评估闭环 L0–L3 亦已实现（判定持久化 → 离线对账 → 影子配对 judge → bad_signals 扩充）。

## 目标

给 route 增加「按请求画像选档位」的能力：同一条 exposed route 下，便宜的请求走便宜目标、
困难的请求走强目标或 fusion 编排。fusion 从"唯一的多模型入口"退化为"档位之一"。

非目标：每请求默认调用分类器；语义缓存；A2A；把 fusion 设为默认路由；改动 `Routes` 的值类型。

## 决策分层

「自动路由」是四层决策，本设计只补 L3：

| 层 | 决策 | 现状 |
|---|---|---|
| L1 路由名/能力 | 走哪个 exposed route | 请求感知（image/tools/context）+ 跨 route pool |
| L2 目标/账号 | route 内选哪个 provider/账号 | surplus 调度 + sticky + 熔断 + 质量 EWMA |
| L3 档位 | 这次用便宜还是强目标 | **本设计** |
| L4 编排 | 要不要 fan-out + 合成 | fusion（本设计后作为 L3 的一个档位） |

## 配置形状

新增顶层 `route_policy:`（键 = exposed route 名，与 `shadow:`/`mcp_routes:` 同一"route 作用域额外行为"
的既有惯例）：

```yaml
route_policy:
  coding-auto:
    bands:                                  # 按序求值，首个全条件命中者胜
      - when: {estimated_tokens_max: 8000, follow_up: true}
        target: zhipu/glm-5.3-flash         # 便宜档
      - when: {follow_up: false}            # 首轮（大上下文、要探索）
        target: zhipu/glm-5.3
      - when: {estimated_tokens_min: 60000}
        target: fusion/hard-coding          # 峰顶：多模型编排
    escalation:                             # 会话内升级（切片 2）
      bad_signals: [upstream_error]         # 当前闭集只有 upstream_error
      consecutive: 2
      target: zhipu/glm-5.3
      dwell: 30m
    selector:                               # decisions 模型判定（切片 3，默认 shadow）
      target: {provider: typesafe, model: jev-1.13, protocol: decisions}
      mode: shadow
      confidence: 0.55
```

信号闭集（全部来自一次 `routing.ProfileRequest` 扫描，遵循该包惯例的字节标记扫描，
不引入 JSON 解析）：

| 信号 | 来源 | 说明 |
|---|---|---|
| `estimated_tokens_min/max` | `Profile.EstimatedTokens` | 已有 |
| `has_tools` / `has_image` | `Profile.HasTools` / `Profile.HasImage` | 已有 |
| `follow_up` | `Profile.FollowUp`（新增） | body 已带 assistant 轮次 → 是会话的后续轮 |

**与计划的偏差（有意）**：计划里写的是 `turns_min/turns_max`；精确轮数需要 JSON 解析，
与 `internal/routing/request.go` 现有的字节标记扫描惯例冲突且给热路径加分配，故改为
`follow_up` 布尔——它正好覆盖"首轮重、后续轮轻"这一主要用法。

匹配语义：bands 按序求值，首个满足全部条件的 band 胜出；无匹配 → 静态调度顺序（零行为变化）。
band 的 target **只是"前置"**：把它移到 `ordered` 最前，其余 target 保留为 failover。
优先级链是 **latch > selector(enforce 且置信度达标) > bands > 静态顺序**。

**与计划的偏差（有意）**：计划里 fusion 档位写的是"选中即独占、不向普通 target failover"；
实现改为与 bands 一致的"前置 + 保留兜底"——fusion 自身的降级（quorum 不足、超预算、
`selector_direct`、`multi_turn`）已经覆盖了"编排失败仍有答案"的路径，管线只在 recipe
缺失/配置损坏时才落到后面的普通 target。独占语义会让一次配置笔误直接变成零尝试，
与「策略不得造成零尝试」的红线冲突。

## 与既有语义的交互（红线）

1. **硬选择优先**：`force`（`x-mp-force-provider` / `?force_provider=`）或 pin 生效时整条策略跳过——
   与"硬选择不进入请求感知改道"同一条规则。
2. **不产生零尝试**：band 选中的 target 若已被能力/context 过滤掉，回落到原 `ordered`，
   绝不因为策略把目标清空。
3. **响应 cache**：
   - bands 的判定是 `(body, config)` 的纯函数 → cache 语义不变，不绕过；
   - `escalation`（有会话状态）与 `selector`（有模型判定）启用后，**该 route 绕过响应 cache**
     （与 force-provider 绕过 cache 同一语义），否则会把便宜档的缓存回放给已升级的会话。
4. **配置校验 vs 告警**：`internal/config` 与 `internal/routing` 是单向依赖（routing→config），
   config 无法计算"该 route 的生效 target 集"。因此 config 只校验本地不变量（route 名存在、
   provider/协议/recipe 存在、`when` 至少一个条件、min ≤ max），"band target 不在该 route 里"
   由 `internal/routing` 的 `ConfigRoutingWarnings` 出告警（不阻止启动，走 `/api/status.warnings`、
   doctor、config check 同一出口）。
5. **单快照**：策略只消费本次请求的 `runtimeSnapshot`（`Proxy.SnapshotRuntime()` 一次捕获），
   判定与状态读取都在该快照的 generation 内。
6. **锁序**：latch 状态在 `internal/runtime.Manager` 内读写，锁序 `Proxy.mu → runtime.Manager`，
   持锁期间不回调、不做 I/O。

## 会话 latch（切片 2，已实现）

- 状态形状：`runtime.Latch{Target string, Since time.Time, BadRuns int}`，session-keyed
  （`x-claude-code-session-id`），内存 only、不持久化、随 `ReplaceGeneration` 清空——与 session sticky 同待遇。
- **实现偏差**：切片 2 的信号闭集只有 `upstream_error`（终局未 commit 且客户端未断），未实现
  空 200、客户端重发同一 turn、工具错误等信号；`BadRuns` 计数取代设计稿的 `BadSignals`。
- 升级规则：连续 N 个坏运行 → latch 到 `escalation.target`；命中即从下一轮起前置该 target；
  滞回靠 `dwell`（默认 `30m`）与"连续"阈值，避免档位抖动反复打断上游 prompt cache 热度。
- 失败的判定一律 fail-open 到静态顺序。

## 判定调用 seam（切片 3，已实现）

route 级 selector 的模型调用与 fusion selector 共用 `internal/forward/decisions.go` 的
`pipeline.callDecisions` 共享 helper，该 helper 仍基于 `BufferedLegExchange` + 自带 timeout
（默认 800ms），并非收敛到 `internal/app` 的 `modelExchange` seam。融合/路由两种调用方仅
参数化差异（leg id 前缀、live event provider marker、日志上下文、候选来源）。

**偏差登记**：
- seam 统一未实现：route 级与 fusion selector 仍是 `BufferedLegExchange`，未复用 guard.adjudicate
  的 `modelExchange` 排序/预算切片，登记为切片 4 或后续重构项。
- route 级 selector 不实现 `direct_score_max` / `panel_top_k`（强制为 0 并校验报错），只把选中目标
  前置到 `ordered` 最前，不直接回答。

## 观测

- band 命中/未命中：结构化日志 + 现有 counters 虚拟键。
- selector 判定：`SelectorObservation` 同形状（mode/action/choice/confidence/difficulty/latency/err），
  shadow 模式只记录不改路由。
- latch：升级/到期事件进 live events，便于和 prompt cache 命中率变化对照。

## 分层：模型选择 vs 调度（已实现）

**目标**：两条独立的决策层，接口是「模型档」（grade）而不是具体目标。

```
请求 → [模型选择层] --选中的 grade--> [调度层] --ordered targets--> 转发/failover
        形状规则 / latch / 判定模型        配额 surplus / 熔断 / sticky / 池化账号
```

**现状（切片 1–4）违反这条分层，具体四点：**

1. **顺序反了**：`serveOnce` 先 `p.schedule(...)`（调度层排序整条 route 的全部 target），
   再由 `applyRoutePolicy`/`applyRouteLatch`/`applyRouteSelector` 对**已排序列表**做「前置」。
   选择层无法表达"调度层会丢掉的东西"（冷却中、被能力过滤掉的目标）。
2. **候选集被污染**：selector 的候选来自 `ordered`（已按 quota/priority 排好），并且按该顺序
   编号 `c0..cn`——判定模型看到的是"钱包排序"，位置偏差会把调度偏好当成模型偏好。
   同一模型在 4 个 provider 上的候选更是完全没有区分度（差异在配额，不在能力）。
3. **策略目标是 `provider/model` 字符串**：band/escalation 必须写具体 provider，于是要有
   "band 目标不被该 route 服务"这种告警补丁；而调度层的路由推导/能力过滤又在同一个
   target 列表上叠加，两层的关注点混在一个结构里。
4. **failover 跨模型**：目标循环今天会在模型之间连续 failover（`glm-5.3` 挂了直接试
   `deepseek-v4-pro`），这实际上是**在选择层没有决策的情况下偷偷换档**——降级发生了，
   但没有经过策略（没进 latch、没记 selector 观测）。

**目标契约：**

- **grade 声明在策略层，不在 `routes:` 里**：`routes.<name>` 保持"一组 target"的唯一权威定义
  （值类型不动，避免 7+ 处消费方连锁改动）；`route_policy.<route>.grades` 把该 route 的
  target **划分**成若干模型档：

  ```yaml
  routes:
    coding-auto: [zhipu/glm-5.3-flash, volcengine/deepseek-v4-flash, zhipu/glm-5.3, deepseek/deepseek-v4-pro, kimi-code/k3]
  route_policy:
    coding-auto:
      grades:
        flash: [zhipu/glm-5.3-flash, volcengine/deepseek-v4-flash]
        pro:   [zhipu/glm-5.3, deepseek/deepseek-v4-pro]
        k3:    [kimi-code/k3]
      bands:      [{when: {follow_up: true, estimated_tokens_max: 12000}, grade: flash}]
      escalation: {bad_signals: [upstream_error], consecutive: 2, grade: pro, dwell: 30m}
      selector:   {target: {...decisions...}, mode: shadow}
  ```

  没有 `grades:` 的 route（今天全部）= 单一隐式 grade，语义与现状完全一致。
- **选择层只输出 grade**：bands / latch / escalation / selector 的目标写作 grade 名；
  判定模型的候选是 grade（criteria 由 grade 的 rubric 渲染），不再出现 provider。
  为兼容，band/escalation target 仍接受 `provider/model`，含义 = "包含该 target 的 grade"。
- **调度层只在选中的 grade 内排序**：surplus / peak / 熔断 / 池化账号 spread / sticky
  全部不变，作用域缩到该 grade 的 target 集。
- **failover 边界**：grade 内 failover 由调度层负责；跨 grade 的降级只能由**选择层**发起
  （escalation / bands 的兜底规则）。为保证"永不零尝试"，跨 grade 兜底保留为**显式策略项**
  `fallback: any`（默认，等价今天行为：剩余 grade 按配置顺序兜底）/ `next_grade` / `strict`。
- **可用性投影（唯一允许的跨层回流）**：选择层需要知道"这个 grade 现在有没有可用目标"。
  由调度层以**只读**形式提供（候选集里剔除全灭的 grade，或附 `available` 布尔），
  不把 surplus/配额数值泄漏给判定模型。方向是调度 → 选择，同一份快照内。
- **sticky 归属**：wallet/账号粘滞留在调度层（会话 → provider）；**grade 粘滞**属于选择层
  （已有的 latch 就是雏形）。
- **能力过滤的位置前移**：能力/context 过滤作用于 **grade 的候选集**（在判定之前），
  而不是过滤完 target 再让 band 静默失效——消除"band 目标被过滤掉就无声无息"的失败模式。
- **响应 cache**：选择层有状态（latch/selector）时该 route 继续绕过 cache；纯 bands 保持
  纯函数语义（现契约不变）。

**迁移切片（每片可独立验收）：**

1. **配置**：`route_policy.<route>` 增加 `grades: {name: [targets]}`（把该 route 的 target 划分为
   模型档）与 `fallback`（`any` 默认 / `next_grade` / `strict`）；band/escalation/selector 的
   target 改为 `grade: <name>`（**为兼容保留** `provider/model` 写法，含义 = "包含该 target 的 grade"）；
   校验：grade 名唯一且非空、每个 grade 至少一个 target、target 必须属于该 route、
   `provider/model` 必须能唯一落进某个 grade、未配置 `grades` 时出现 `grade:` 引用报错。
2. **选择层**：新增纯函数 `SelectGrade(policy, profile, latch, selectorChoice) → grade, ok`，
   把今天 `applyRoutePolicy`/`applyRouteLatch`/`applyRouteSelector` 的"前置目标"逻辑收敛成
   "选档"；`PreferTarget` 退化为 grade 内的一步（或直接删除）。
3. **管线**：`serveOnce` 顺序改为 profile → grade 候选过滤（+ 可用性投影）→ 选档 →
   `schedule(选中 grade 的 targets)` → 目标循环（grade 内 failover）→ 跨 grade 兜底
   （按 `fallback`，默认 `any` = 今天的鲁棒性）。**未配置 `grades:` 的 route 走原路径，
   行为逐字节不变**——整个切片是 opt-in。
4. **判定模型输入**：候选改为 grade（criteria = profile 渲染 + rubric），`state.candidates[].model`
   填 grade 的代表模型/描述；观测记录增加 `grade` 字段（与今天的 choice/confidence 并存）。
5. **文档与回归**：`request-routing.md` 改写为两层契约；回归用例覆盖"grade 内 failover 不跨档"、
   "全灭 grade 的可用性投影"、"selector 候选是 grade"、"旧配置等价性"。

**明确不做**：不在热路径用运行数据自适应（评判/统计的产物只能固化成配置）；不把
配额/延迟数值喂给判定模型（只给可用性布尔）；不改变调度层内部的排序语义（surplus/peak/
熔断/粘滞原样）。

**实现记录（已落地）**：

- 配置：`RoutePolicy.Grades` / `Fallback`（`any` 默认 / `next_grade` / `strict`），
  `RouteBand.Grade` 与 `EscalationConfig.Grade`（与 `Target` 互斥，`provider/model` 写法按
  "唯一落进某档" 兼容解析）；校验含 grade 名/成员/归属/歧义/闭集。
- 选择：`routing.SelectGrade`（latch > selector > band）+ `GradeForTarget` 反查。
- 管线：`forward.applyRoutePolicyGrades` = 分组 → 档内过滤 → 解析 latch/selector/band →
  `buildGradeOrdered` 按 fallback 拼接；`serveOnce` 对 `HasGrades()` 的 route 走该分支，
  其余走原路径（**零行为变化**）。
- **实现偏差（有意）**：
  1. `ApplyWithProfile`（含跨 route 兜底）仍在分组**之前**执行，档内再过滤一次
     （`filterGradeTargets`）——保留旧的能力兜底链路，代价是同一判定做两遍过滤（幂等、开销可忽略）。
  2. 档内过滤用 `routing.Fits` 直判，不引入第二条跨 route 池路径：空档保持为空，交由
     fallback 决定，避免跨 route 目标污染档语义。
  3. latch 用 `"grade:<name>"` 编码落在档上（`Latch.Target` 字段复用），未改动 runtime 状态类型。
  4. selector 候选为 `g0..gn`（rubric = `grade <name>`，属 grade 级而非 target 级）——
     这正是设计目标：判定模型看到的是模型档，不是钱包。


## 实现顺序与状态

切片 0 文档 → 切片 1 bands（无模型调用、无状态）→ 切片 2 latch → 切片 3 selector → 切片 4 fusion 档位。
每片独立可回滚：配置块缺省即零行为变化。

| 切片 | 状态 | 主要落点 |
|---|---|---|
| 0 设计文档 | 已完成 | 本文 + `request-routing.md` / `runtime-state.md` 契约节 |
| 1 bands | 已完成 | `internal/config`（`RoutePolicy`/`RouteBand`/`BandWhen`）、`internal/routing`（`PickBand`/`PreferTarget`）、`internal/forward`（`applyRoutePolicy`） |
| 2 latch | 已完成 | `internal/runtime.Manager`（`Latch`/`SetLatch`/`LatchValue`）、`internal/forward`（`applyRouteLatch`/`recordLatchOutcome`） |
| 3 selector | 已完成 | `internal/forward/decisions.go`（共享 `callDecisions`）、`applyRouteSelector`、config 校验 |
| 4 fusion 档位 | 已完成 | band/escalation target 允许 `fusion/<recipe>`；语义见上文「与计划的偏差」 |
| 5 模型选择/调度分层 | 已完成 | 见「分层：模型选择 vs 调度」一节——`route_policy.grades` 声明模型档、`SelectGrade` 只输出档名、调度在选中档内排序、`fallback` 决定跨档兜底 |
| 6 反馈闭环 L0 | 已完成 | `Record.Routing` 持久化（`internal/config/routing_decision.go`、`requestlog` 编码/索引、`internal/forward` 决策透传） |
| 7 反馈闭环 L1 | 已完成 | `model-proxy routing report`（`internal/cli/diag/routing_report.go`）：弱标签、混淆矩阵、成本基线、判定开销 |
| 8 反馈闭环 L2 | 已完成 | `route_policy.eval` + eval shadow（`internal/app/proxy_eval.go`、`proxy_shadow.go`、`forward.CallEvalJudge`）、verdict 汇总 |
| 9 反馈闭环 L3 | 已完成 | `bad_signals` 三项闭集（`empty_ok`/`repeat_turn`）、`runtime.CheckRepeatTurn` 滑窗、route_warnings 修复 |

后续项（未做，按需再评估）：selector 收敛到 `internal/app` 的 `modelExchange` seam、
route 级 selector 的 direct 直答。

## 反馈与评估闭环

> 外部依据：`docs/research/research-llm-routing.md`（学术/工业实践对照）。核心结论：
> 分层骨架与主流收敛解一致，短板在「判定结果不可测量」——先建测量手段，再谈改进。

评估要回答三个问题，对应四层方案（成本与标签质量递增）：

| 问题 | 层 | 方案 |
|---|---|---|
| 前置：判定结果不可考 | L0 观测补全 | route selector 判定结果（choice/confidence/difficulty/选中档/决策来源/latch 状态）持久化进 request log |
| 判定器准不准？策略值不值？ | L1 离线对账（弱标签） | 只读 request log 索引的离线报告：混淆矩阵 + 成本基线对比 |
| 弱标签不可信的区域 | L2 影子配对 + judge（强标签） | 抽样把请求影子重放到对立档，judge 对比两个响应 |
| 在线护栏灵不灵？ | L3 信号扩充 | `bad_signals` 补空 200 / 重复 turn；bandit 在线学习**明确暂缓**（样本量不足、依赖 L1/L2 标签体系） |

### L0：判定结果持久化（已实现）

- request log `Record` 新增 `Routing` 字段（JSON 对象，缺省省略）：`{source, grade, target,
  selector: {choice, confidence, difficulty, enforced}, latch}`。
- `source` ∈ `band` / `selector` / `latch` / `fallback`（无策略命中时不写）；shadow 模式下
  `selector.enforced=false` 但判定结果照常记录——这是测量 shadow 准确性的数据基础。
- 判定调用本身已有独立 request log 记录（`route-select-<id>`，`internal/forward/decisions.go`），
  L0 补的是「判定结果 ↔ 业务请求」的关联字段，落在业务请求自己的记录上。
- 索引：`index.db` 加 `routing` 列（`Indexer` reconcile 自动补旧行 NULL），查询侧不解 JSON。

### L1：离线对账 `model-proxy routing report`（已实现）

- 只读 `index.db` 的离线 CLI，不进热路径、不需要 daemon 在线。
- **弱标签**（判定"选中档是否够用"）：200 且响应非空且无错误诊断码；同 `SessionID`/`TurnKey`
  时间窗内无重发；该会话后续未触发 latch 升级。三条件与文献（用户重试 = 最强免费不满意信号）一致。
- 产出：selector 判定 vs 弱标签的混淆矩阵（按 difficulty 分层）、各档弱标签成功率、
  实际成本 vs「全走最贵档」基线估算（复用 `internal/pricing` 价格表）。
- 明确的局限写进输出：弱标签有偏（用户不重试 ≠ 满意），低置信区域需 L2 强标签校准。

### L2：影子配对 + judge（已实现）

- `route_policy.<route>.eval`：`{sample_rate, pair: opposite|grade:<name>, judge: <decisions target>}`。
- 机制复用 post-commit shadow 管线（`internal/app/proxy_shadow.go`）：策略选中档 G 且抽样命中时，
  把同一请求影子重放到配对档（默认 opposite = 相邻的另一档），judge（decisions 协议模型）
  对比两份响应给出 `primary_better / shadow_better / tie` 判定。
- 判定结果写入 shadow 记录的 `Diagnostics`，对账报告（`ShadowReport` 扩展 / `routing report`）
  汇总「便宜档够用率」——这就是 RouteLLM 的 LLM-judge 标签增强路线。
- 成本护栏：只对 selector 低置信判定或按 `sample_rate`（默认 5%）抽样；judge 与 selector 一样
  fail-open，任何一步失败只丢该样本，不影响业务请求。

### L3：`bad_signals` 扩充（已实现）

- 信号闭集从 `upstream_error` 扩到闭集三项：`upstream_error`（终局未 commit 且客户端未断）、
  `empty_ok`（200 commit 但响应体为空/零内容）、`repeat_turn`（同一 `TurnKey` 在短时间内
  再次出现 = 客户端重发同一轮，内存滑窗判定，不读 request log）。
- `repeat_turn` 的滑窗状态放在 `internal/runtime.Manager`（generation 门控、不持久化、
  随 `ReplaceGeneration` 清空，与 latch 同待遇）。
- 明示不做：工具错误信号（需要协议层解析工具结果，超出本次范围）；bandit/在线学习。

### 评估指标与出口

| 维度 | 指标 | 数据来源 |
|---|---|---|
| 成本 | 实际成本 vs 全走最贵档基线 | `/api/analytics` cost + `internal/pricing` |
| 质量 | 各档弱标签成功率、escalation 触发率、latch 驻留时长 | L1 对账 + runtime latch |
| 判定器 | 混淆矩阵、按 difficulty 分层准确率、判定开销占比 | L0 字段 + decisions 记录 |
| 延迟 | selector enforce 的 TTFT 增量 | stats `ttft` 字段 |

Web UI Eval tab 已有 Shadow Report / Fusion 卡片，路由评估面板作为后续 UI 项（不阻塞 CLI 出口）。

### 偏差登记（反馈闭环，有意）

1. **L0**：`RoutingDecision` 类型放在 `internal/config` 而非 `requestlog`——`internal/archtest`
   依赖图禁止 `targetexec` 导入 `requestlog`，`internal/config` 是双方都可导入的最近公共包。
   guard 终局与 cache 命中路径（都发生在策略运行之前）不写 `routing` 字段，与「无策略命中省略」语义一致。
2. **L1**：错误类 diagnostic code 用启发式判定（code 含 `error`/`failed`/`fatal` 视为错误，其余如
   `stop_dropped` 视为信息）——诊断码没有机器可读的"错误/信息"分类，引入分类表属于诊断码体系本身的演进。
   未配置 grades 时成本基线回退为该 route 历史记录中出现过的最贵模型。`archtest` 白名单为
   `cli/diag` 新增 `pricing` 依赖边（新功能的正当依赖，`overview.md` 依赖列表同步更新）。
3. **L2**：judge 复用 decisions 协议的 `model_choice` 闭集题型表达成对判定（候选 =
   `{primary_better, shadow_better, tie}`），未引入新协议题型；judge 调用标记 `Sensitive`，
   request log 中其请求/响应 body 置空（响应对比内容属敏感数据）。`opposite` 的精确定义 =
   grades 声明顺序中主档的下一档，最后一档回退到前一档。顺带修复既有 bug：`BuildRecord`
   此前未拷贝 `Input.Diagnostics`（诊断码在日志记录中丢失）。
4. **L3**：`empty_ok` 谓词 = 200 commit 且最终写回客户端的响应体字节数为 0（不解析协议，
   避免把合法的空 `content`/纯 `tool_use` 误判）。`repeat_turn` 滑窗时长复用 `escalation.dwell`
   （与 latch 同一会话时间尺度），窗口上限 64 条。`TurnKey` 唯一权威来源是
   `requestlog.ComputeTurnKey`（由包内私有函数导出，路由侧不复制实现）。顺带修复两个既有问题：
   `recordLatchOutcome` 在好运行时会凭空创建空 target 的 latch；`route_warnings.go` 对 `grade:`
   写法的 band 误报「target 不在 route 中」。
