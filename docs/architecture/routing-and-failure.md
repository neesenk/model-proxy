# 路由、失败分类与恢复

## 适用范围

修改 `internal/forward/forward.go`、`internal/forward/plan.go`、
`internal/targetexec/executor.go`、`internal/targetexec/rate_limit.go`、
`internal/routing/retry.go`、`internal/app/proxy_read_endpoints.go`、
`internal/app/target_pipeline.go`、`internal/app/wirecap.go`、
`internal/app/modelcaps.go`、`internal/probe/`、`internal/app/health_test.go`、
`internal/app/model_lock_test.go` 或 cooldown/retry 行为时必读。

## 实现入口

- `Proxy.forward`（app 薄 shim）→ `internal/forward.Serve` / `serveOnce` / `targetexec.Executor.Execute`
- `internal/app/proxy_read_endpoints.go`：应用执行层到 runtime health/cooldown/param/rate-limit
  状态端口的适配
- `internal/forward/snapshot.go` / `internal/app/dispatch_context.go`：`Snapshot`
  （app 别名 `RuntimeSnapshot`，单次捕获红线在 `Proxy.SnapshotRuntime`）；
  `internal/forward/forward.go` 的 `serveRequest` 与 `internal/forward/attempt.go` 的
  `targetexec.Attempt` 唯一 assembly adapter
- `internal/targetexec.Plan`：已解析目标的不可变 model/body/base URL/path wire
  preparation；`internal/forward/plan.go` 的 `PlanTarget` 只做 snapshot-owned facts 的投影
- `internal/targetexec/executor.go`：完整单目标 I/O、401/参数/图片 retry、
  failure classification、response conversion/capture/cache pipeline
- `internal/app/target_pipeline.go`：captured generation/scheduling 的 `targetexec.State`
  与 metrics/tokens/request-log/events 的 `targetexec.Effects` 应用适配
- `internal/routing.DecideFailure`：跨 pass 失败类别、短 cooldown wait、
  recovered-untried 与 429/502 终局的纯策略
- `internal/runtime.Manager`：health、model lock、paramBlock、sticky、pin、
  spread、quota 与 schedule
- `targetexec.ParseRateLimit`、`targetexec.IsModelDenied`、
  `targetexec.ParseUnsupportedParam`
- `cooldownState`、`hasRecoveredUntried`

## 基本路由语义

一次客户端请求在 `forward` 开始时只捕获一次 `RuntimeSnapshot`。该快照包含
同一 config generation 的 config、provider implementations、pool identity、
expanded routes 与 models.dev catalog；reload 只交换新对象，
不得原地修改快照持有的 map。`serveRequest` 是一次完整 schedule/failover pass
的输入，`internal/targetexec.Attempt` 是单次 resolved target 的强类型执行契约。
普通 route 通过该契约进入 `targetexec.Executor.Execute`，
新增横切能力不得继续扩张 positional 参数列表或以 `any` 夹带 root owner。

`targetexec.Attempt` 固定为五组：`targetexec.Runtime`
（captured scheduling/generation/cache）、`targetexec.Plan`、
`targetexec.Exchange`（request/writer/body）、`targetexec.Scope`（调用模型、
agent、log/Responses 上下文）和 `targetexec.Policy`（force、last
target、context retry）。两条调用链只能经
`newTargetAttempt → targetexec.NewAttempt` 构造；factory 仅投影/装配，不做
model rewrite、state expansion 或协议转换。普通 route/Shadow 不得复制
endpoint、model rewrite 或 conversion-option 逻辑。执行器必须从 typed
Runtime 与 Plan 读取事实，禁止在 scope/log 中复制第二份 generation、嵌入完整
`RuntimeSnapshot`、使用 `any` 或重新查 Proxy。

`targetexec.Executor` 只允许依赖 generation-frozen `targetexec.State` 暴露的
健康、参数学习和 wire 能力，以及 typed `Effects/Responses` 端口；不得 import
或持有完整 `*Proxy`，也不得访问调度、reload、Web、lifecycle 或 Shadow。
`internal/app/target_pipeline.go` 从 `Attempt.Runtime()` 绑定 generation/scheduling，再把
根 metrics/token/request-log/events 映射为语义 effect，不得包含 `client.Do`、
转换、retry 或 failover pipeline；`internal/forward/plan.go` 只组装这些端口。
`internal/forward/forward.go` 只负责编排和调用。
executor commit 后只返回含实际上游请求体的最小 `targetexec.Commit`；Shadow
sampling、semaphore、lifecycle admission 与 dispatch 由 `serveOnce` 在
executor 外完成。

默认“客户端协议 = 上游协议”，同协议请求和响应字节级透传。目标声明 `protocol:` 时才进行协议转换。

后端协议解析优先级（`forward.PlanTarget`）：显式 `protocol:` > `ProtocolHint`（codex→responses）> **模型级协议矩阵**（`runtimewire.ResolveModel`）> **provider 级 wire 探测 verdict**（`runtimewire.Resolve`）> 客户端协议透传。空 `target.Model`（透传 target）跳过模型级查询，直接进入 provider 级。

`Proxy.SnapshotRuntime` 随配置捕获 provider 与 model 两级只读能力视图。请求的转发、重试和路由选择器调用共用这份视图。探测、404 纠正或 reload 更新后的结论从下一次捕获起可见。模型级回写校验请求配置的 fingerprint，provider 级回写校验原端点。

provider 级 wire verdict 由 `probeAllWireCaps` 在 boot/reload 时异步探测并按 parent provider 名缓存。探测只有 openai base 上的两条腿：`/chat/completions` 与 `/responses` 各一个请求，经 `probe.ProbeProviderOpenAILegs` 走与模型级完全相同的探测管线（impl 方言 body 合并、function tool 注入、max_tokens→max_completion_tokens 改名重试；配方见 overview.md 的 `internal/probe` 条目）——**provider 级 verdict 因此同样是 agent 级可用性，不是裸 ping**：网关答裸 ping 但拒 function tools（aqp gpt-5.6 案例）在 provider 级回退路径上也不会再产生假 yes。**anthropic 支持是 config 声明、永不探测**：provider 配置 `anthropic_base_url` 即支持（声明 = 事实），未配置即定义上不支持；旧的「在无 anthropic_base_url 时对 openai base 探 `/v1/messages`、网关接受 anthropic 则字节透传」分支已删除——探测绝不在 openai base 上伪造 anthropic 请求体。分类（`runtimewire.ClassifyProviderStatus`）：404→no；**400 带模型/工具拒绝措辞（与模型级共用 `modelRejectionPhrases`）→no**（工具注入后该措辞证明此腿对 agentic 流量不可用，yes 会重演裸 ping 假阳性；provider 级 no 有 24h negativeTTL 自愈，误伤面有兜底）；其余 2xx–4xx（含 3xx 与 400/401/403/429）→yes；超时/连接错误/5xx→unknown。verdict 条目携带探测语义版本（`Capabilities.ProbeVersion`，当前 2）；恢复时 base_url 匹配**且**版本为当前的条目才采纳，旧语义（v1 裸 ping）条目丢弃重探——provider 级 yes 无限期信任，不 bump 版本旧 yes 会永远支配协议选择。provider 级 verdict 生效的决策矩阵：

| 客户端协议 | 条件 | 后端协议 |
|---|---|---|
| openai(chat) | — | 透传 |
| responses | verdict.responses ≠ no | 透传 |
| responses | verdict.responses == no | 转 chat |
| anthropic | provider 有 anthropic_base_url | 透传（专用 base） |
| anthropic | verdict.responses == yes | 转 responses（reasoning 保留） |
| anthropic | verdict.responses == no | 转 chat |
| anthropic | verdict unknown | 透传（维持现状，探测窗口期不改变行为） |

**模型级协议矩阵**：boot/reload 的探测 pass（`(*Proxy).probeAllModelCaps`，`internal/app/modelcaps.go`，与 `probeAllWireCaps` 同由 `startWireCapProbe` 派发，并发上限 4）对每个 provider 的 model 集合（config models ∪ 显式 route target ∪ derived target）经 `probe.ProbeModelProtocols` 并发探三条腿：chat → POST {openai_base}/chat/completions，responses → POST {openai_base}/responses，anthropic → POST {anthropic_base}/v1/messages；腿的 base 未配置则不探（`LegResult.Probed=false`）。**每条腿的 body 都注入一个最小 function tool 声明（`attachProbeTool`，按腿成形：chat 的 `{type:function,function:{...}}` / anthropic 的 `input_schema` / responses 的扁平 `{type:function,...}`），verdict 因此是"agent 级可用性"而非裸 ping**——真实案例：aqp 的 gpt-5.6 系列裸 ping 三协议皆通，但带工具时 chat/anthropic 被上游拒绝（"Function tools ... are not supported for gpt-5.6-luna in /v1/chat/completions"），只有 responses 可用。impl 的 `ProbeRequest` 路径与腿的 canonical 路径一致时（如 codex 的 /responses 方言），impl 的 body 优先于通用最小 body（工具声明仍会并入）；max_tokens→max_completion_tokens 改名重试按腿适用。ProtocolHint 覆盖的 provider（codex、typesafe）不探，直接按 hint 合成（codex → `{responses:yes, chat:no, anthropic:no}`；typesafe（decisions，无对应探测腿）→ 三腿全 no，decisions 解析永远先走 hint）。分类（`runtimewire.ClassifyModelStatus`）与 provider 级不同：未探腿→no（定义上不支持）；2xx→yes；404→no；400 带模型拒绝措辞（"model not found"/"does not exist"/"unsupported model"/"invalid model"/"model is not supported"/"model not supported"/"not supported by this endpoint"（shopee 40403 形态）/"unknown model"/"no such model"/"not supported with this model"/正则 `not supported for <model> in <path>`（某特性对该模型在该路径不支持，如上面的 gpt-5.6 工具拒绝；v4 起不再匹配裸 "not supported for" 子串——它会误伤套餐层措辞），大小写不敏感）→no，其余 400→yes（形状争议反证该路由服务此模型，刻意粗粒度）；401/403/429→unknown（auth/quota 回答证明路由存在，但说明不了该模型；**例外**：429 携带**套餐权限类专属措辞**（`planPermissionPhrases`，如 BigModel 的 `[1311][当前订阅套餐暂未开放<model>权限]` 套餐权限拒录）→ no——否则永久 `? unknown` 无限重探。429 分支只扫这个专属列表，不复用 400 的通用拒绝措辞表：网关/CDN 的 429 错误页样板可能含 "does not exist" 类字样，而模型级 no 是 fingerprint 门控（无 TTL）的永久结论，一次误判即锁死该腿）；5xx/3xx/网络→unknown（与 provider 级 3xx→yes 不同）。unknown 腿在下一次探测 pass 重探；**新矩阵的 unknown 腿不覆盖同 fingerprint 下已存储的结论**（`MergeOnUnknown`，daemon `Put`/refresh `ReplaceProviderModels`/CLI `persistModelCaps` 三处同一规则）——瞬态 429/超时是「无信息」，不得把已结论的 yes 降级成 `? unknown`；已结论的 model 在 fingerprint 匹配时也不再作为同伴 unknown 腿的殉葬品重探（避免整 provider 重爆触发限流）。pass 是 single-flight（`wireProbeMu` + 至多一个合并后续 pass，`wireProbePending`），跑在 stop-aware 且带 4s wall-clock 预算的 context 上（取消/超预算的腿不落 verdict、下轮重探），单腿超时 30s。**operator disabled 的 model 不探测**（Manager 的 override 投影）：不发包、不参与 eligibility（全 disabled 的 provider 整体跳过）、已存 verdict 冻结不删（disable 是轮换选择不是能力事实；重新启用后按既定规则复用/重探）；`models refresh`（CLI 与 daemon 孪生）同样不探测 disabled model，且它们无条件留在 kept（探全失败的 safety net 只按被探测子集判定，disabled rider 不得掩盖探测中断）。模型级矩阵的生效规则（`runtimewire.ResolveModel`，先于 provider 级）：

| 客户端协议 | 条件 | 后端协议 |
|---|---|---|
| anthropic | 有 anthropic_base_url 且 model.anthropic == yes | 透传 |
| anthropic | model.responses == yes | 转 responses（含「有 anthropic base 但该模型不在其上」） |
| anthropic | 矩阵全部已结论且 responses ≠ yes | 转 chat |
| responses | model.responses == yes | 透传 |
| responses | model.responses == no | 转首个 probed-yes 替代腿（chat，再 anthropic）；替代腿未结论（unknown）优先于已知死腿（unknown ≠ dead）；全 no 维持 chat，让上游错误干净浮现 |
| openai(chat) | model.chat == no | 转首个 probed-yes 替代腿（responses，再 anthropic），其次 unknown 替代腿；全 no 维持透传，让上游错误干净浮现 |
| openai(chat) | 其他 | 透传 |
| 任意 | 模型无条目或相关腿未结论 | 回落 provider 级矩阵 |

**运行时 404 纠正（模型粒度）**：因 verdict 转到 `/responses` 的请求若上游 404，说明 verdict 有误而非模型缺失——`proxyHealthGate.NoteWireResponsesMiss` 经 `targetexec` State/HealthGate 传导（executor 与 decisions selector leg 共用）：请求快照中该 model 在模型级矩阵有条目时**只翻转模型级** responses verdict 并持久化 model_caps.json（provider 级不动）；无模型条目（透传 target）才翻转 provider 级 verdict（legacy 路径）。两种情况都**跳过 recordModelFailure**（这是我们的协议选择失误，不是模型的失败），按正常失败走 failover；后续请求自动转 chat。非 verdict 驱动的 404 行为不变（模型锁）。decisions selector leg 共享同一纠正（`PlanTarget` 已算出 `viaResponsesVerdict`，见 observe.md）。

provider 级 yes 结论永久信任（错误 yes 由上述运行时路径纠正）；**provider 级 no 结论有 24h TTL**（`wireCapNegativeTTL`），到期后下一次 boot/reload 探测 pass 重探——一次性错误 no（上游发布中临时 404 等）不会永久降级该 provider。模型级矩阵**无 TTL**：失效只由 config fingerprint（`providerbuild.ProtocolConfigFingerprint`）触发，fingerprint 匹配即复用结论（持久化格式与恢复门控见 `runtime-state.md`）。fingerprint 不覆盖 models 列表，因此每次探测 pass 还会把当前 config（models: ∪ 路由 target）不再服务的 model 条目从 store 剔除（`ModelStore.PruneModels`）——从 config 删掉的 model 不会留在矩阵和 /api/models 里。

**判定的不对称兜底**：provider 级 `runtime/wirecap.ClassifyProviderStatus` 把 404 与「模型/工具拒绝」400 判 no，其余 4xx（含 401/403/405/429）一律判 yes，而运行时纠正只认 404。对 `/responses` 需要不同鉴权、或对未实现路径返 405 的网关会产生 wrong-yes 且不会被自动翻转——此时只能显式声明 `protocol:` 兜底，绕过 verdict。模型级的 400 措辞嗅探与 401/403/429→unknown 是有意的口径差异，见 `docs/decisions/intentional-behaviors.md`。

运行态健康、模型锁和调度状态统一位于 `internal/runtime.Manager`。Manager 以
单锁和 generation gate 保证健康 mutation、availability 过滤、schedule、
cooldown 与 detached dashboard/persistence snapshot 看到一致状态；reload 只按
`Proxy.mu → runtime.Manager` 进入它：

- `schedule` 跳过熔断、限频和模型锁定目标。
- **调度策略（per-route `strategy:`，默认 `quota`）**：`quota` 按现有排序（tier →
  priority → surplus，含会话 sticky 与质量罚分）；`load_balance` 在同一可用
  性门控之后改为轮转分配——**会话粒度**：新会话从 per-route round-robin
  计数器取下一槽位并粘住（与 quota 同一套 sliding-dwell sticky 机制，保
  prompt cache；空闲超 dwell 逐出后重轮转，provider 不可用时重分配），
  无 session id 的请求逐请求轮转且不写 sticky；不排序、不追逐 surplus，
  但会话粘滞优先于一切排序信号。计数器仅 committed assignment 递增，
  reload 重置；preview 不消耗。
  disabled 过滤、pin 收窄、可用性门控、配额耗尽跳过对两种策略同等生效；
  词汇表与 YAML 形式由 `internal/config`（`RouteStrategy*` 常量 +
  `RouteStrategyFor`，含 `#req`/`#ctx` 跨路由 pool 键的后缀剥离）拥有，
  `internal/runtime.decideOrder` 是唯一排序分叉点。
- timeout、连接错误、5xx、refresh 后仍失败的 401、`error.code` 证明账号停用的 403 与
  message 证明套餐订阅失效的 4xx（见下文「普通转发的 403 归因」）和 2xx HTML 伪成功（见下文）计入 provider 熔断。
- **连接类快失败先重派再计数**（`targetexec.IsFastFailConnError`）：`unexpected EOF`/`EOF`/`ECONNRESET`/`EPIPE`/`ECONNREFUSED` 这类毫秒级返回的错误先在同一 target 上重派一次（新连接）；重派成功记 success、首跳**不**记失败。对外网络（代理链回收、LB 滚动）一次事件杀 N 条在途连接是常态噪音，重派把它从熔断视野里滤掉；超时不重派（时间预算已烧掉，重试等于翻倍等待），客户端断开照旧 `OutcomeClientGone`。重派不占 401 refresh 的 retry slot。**服务端主动 GOAWAY 同属快失败**：HTTP/2 网关按 stream 预算优雅回收连接（实测 compass 周期性 `LastStreamID≈2000, ErrCode=NO_ERROR`），`http.Client.Do` 层面浮出的 GOAWAY 意味着本 stream id 已过 LastStreamID——服务端从未处理该请求，同 target 重派不是重复请求（net/http 的 goAwayError 未导出，按稳定错误文案识别）；已 commit 的响应 body 中途断流不在此列（commit 后无法重派）。decisions selector 调用经 `targetexec.BufferedLeg` 同样重派一次（边界同上；`BufferedLeg.OnRedispatch` 通知调用方记与执行器同文案的日志）。
- **熔断需要 streak 持续**（`circuit_persist`，默认 6s，`"0"` 关闭）：连续失败除了达到 `circuit_threshold`，还必须**跨至少 persist 窗口**才开熔断——"N 连败"从"N 次尝试碰噪音"升级为"N 次新鲜连接确认不可达持续了一段时间"。persist 只延迟**新请求**不再考虑该 provider 的时刻；单个请求的 failover 照旧立即发生，重派后的失败也照常计数。成功重置 streak 起点与计数。
- **客户端取消不计熔断**：调用方断开（`exchange.Request.Context()` 取消，
  含调用方自身 deadline）不是上游判决——不记 `RecordFailure`、不计
  `evFailures`，executor 返回 `OutcomeClientGone`，`serveOnce` 立即终止
  failover 链（不再把剩余 target 在死掉的 ctx 上各烧一次）并以 499 结束
  live 事件。执行器自身 upstream timeout 触发的
  `DeadlineExceeded`（父 ctx 仍存活）照常计入熔断。
- exhausted 401 的观测只记 failover，不记 `evFailures`；账号停用 403 与订阅失效同此（仅普通执行器）。
- **凭据拒绝记录**：上述三处账号归因（刷新后仍 401、账号停用 403、订阅失效）在记账之外各留一条
  凭据拒绝（`targetexec.CredentialDenialKind`：`auth` / `account-disabled` / `plan-inactive`，
  经 `State.RecordCredentialDenied` 上报）——这是凭据横幅、Accounts 标记与 Schedule 徽标在
  配额轮询之外的数据源：控制台型 provider（step-plan / qwen-plan / mimo / static）的
  `Quota()` 永远成功，轮询看不到它们的订阅失效或 key 被吊销。首次命中即记录，该账号下一次
  2xx 清除，reload 按存活账号剪枝；状态归 `internal/runtime`（`runtime-state.md`），字段契约见
  `docs/web-api.md`。其它失败类别不记：模型拒绝、配额冷却、网关拒绝、5xx、派发前跳过，以及
  Auth 层失败（如 aqp mint 失败——由轮询路径报告）。
  熔断状态与请求 metric 是不同语义，普通执行器和 decisions selector leg 必须保持一致。
- 429 进入 provider 限频冷却，不计熔断；**403 且 body 证明配额耗尽**（`targetexec.ParseQuotaDenied`，kimi-code 的 5 小时套餐窗口耗尽即此形态）同路：记限频冷却 + failover，终局按限频类分类。
- 404、model-denied、空 200 只锁 `(provider, model)`。
- 半开状态使用 `halfOpenInFlight` 单飞。
- 普通 4xx commit 必须释放半开槽，但不得清除失败历史。
- runtime provider implementation 缺失时必须 fail-closed，禁止发送无认证请求。

## 粘滞×可用性×配额 交互矩阵

**绑定键 = `StickyKey(sessionKey, exposed)`**：会话粘滞以「(会话, 路由)」为粒度——同一会话在不同路由上的粘滞互相独立（会话粘滞键含路由名；同一会话访问不同模型各粘各的）。会话键异常（空/超长/含非法字符）时按无会话处理（`protocol.HeaderSessionID` 卫生化），粘滞落到路由键（等价于匿名请求的路由粘滞）。

**同 provider 内 retry 不换账号**：executor 的参数级重试（剥参/缩图/role 改写/thinking 自适应、连接快失败重派等 in-place retry）留在原 provider；粘滞在 dwell 内保持指向该 provider。跨 provider 的移动只发生在 fallback（沿 ordered 链）与 wait-retry 重排（整轮重调度）。

以下交互语义是路由正确性契约（回归锚点：`internal/runtime` 的
`TestDecideOrderStickyHealthExclusionAndRecoverReturn`、
`TestDecideOrderSkipsQuotaExhaustedStickyAccount`、
`TestDecideOrderStickyPerRouteBinding`、`TestDecideOrderIgnoreSticky`、
`TestDecideOrderSources`、load_balance 对应用例，以及
`internal/app` 的 `TestScheduleStatus_SkippedReasonsOnPartialChain`、
`TestScheduleSourceLogged`、`TestStickyOffOverrideComposition`、
`TestInvalidSessionHeaderFallsBackToRouteKey`）：

| 粘滞目标的当前状态 | 进 ordered 链 | sticky 是否生效 | 恢复后（dwell 内） |
|---|---|---|---|
| 熔断（circuit open） | 否 | 否，按排序取次优 | 是，会话回归（recover-and-return） |
| 限频冷却（429/quota/daily） | 否 | 否，同上 | 是，同上 |
| 模型锁（仅该 (provider, model)） | 该 target 否 | 否，同上 | 锁过期后回归 |
| 配额已知耗尽（新鲜 plan 快照 ultimate=0） | 否 | 否，同上 | 快照转好/失鲜后回归 |
| 并发占满（已学到上限且在途已满） | 否（operator pin 例外：照常派发并占位） | 否，同上 | 空位腾出后回归 |
| operator disabled | 否（candidate 集整体剔除，先于 pin） | 否 | 重新启用后回归 |
| operator frozen | 否 | 否 | unfreeze 后回归 |
| 健康但 dwell 已过期且次优优势 < margin | 是 | 是（sub-margin 不切换） | — |

- **失败绑定不复活**：availability 过滤（熔断/限频/模型锁/配额已知耗尽/并发占满）先于
  sticky 判定执行，`keepSticky` 只在粘滞目标仍处 available 集时成立——down 信号
  存续期间，任何排序/粘滞逻辑都不得把该目标放回链中。
- **recover-and-return 是有意语义**：sticky 条目在目标失败时**不删除**（仅由
  committed 调用的 dwell 空闲清扫驱逐），目标在 dwell 内恢复则会话回到原账号——
  保上游 prompt cache。这与 Bifrost 的「失败绑定即丢弃」是有意差异：他们的多 key
  轮询场景里绑定即失败源，我们的账号池场景里熔断是瞬态信号。
- **failover 复核配额**：ordered 链整体（而非仅首选）过同一 availability 过滤，
  配额已知耗尽的账号不会出现在链的任何位置——failover 不可能把请求打进已知
  耗尽的账号（LiteLLM「budgets re-checked on fallback targets」的等价语义在此
  是结构性成立，不是事后补丁）。
- **路由时刻解析**：routes 每个 config generation 从 config 重新展开，健康/粘滞/
  模型锁按 generation 门控——reload/重启后不存在「引用已新增 provider 的失效
  fallback 链」这一失效类别（Bifrost 曾修此 bug）；route 目标引用不存在的
  provider 在展开/doctor 静态检查处暴露，不进入调度。
- **跳过可观测**：空链由 `/api/status.schedule` 的 `blocked` 逐目标解释；部分链
  被排除的目标由 `skipped` 透出（同形状同分类，字段契约见 `docs/web-api.md`）——
  被静默跳过的 fallback 必须可与配置错误区分。请求级 failover 链由 request log
  的同 id 多 attempt 行承载（attempt 行语义见下一段）。request-aware 路由
  （planner.ApplyWithProfile 的能力/上下文裁剪与跨路由兜底）**必须留痕**：
  目标集被裁剪或整链被跨路由替换时，请求日志该请求的 `schedule.note`
  （已提交行与全链失败终态行都带）记录每个被裁目标的 fit 判词与替换头（如 `request-aware: dropped
  zhipu/glm-5.3: model metadata lacks image modality → cross-route
  step-plan/step-5-preview`），响应头带同值的 `x-mp-reroute`（写于首个 body 字节
  之前，流式也可见；响应头与 `schedule.note` 描述**最近一轮** pass 的改道——wait-retry
  后续轮未改道时清除）；未变化时不产生任何开销（`DescribeFitReroute` 先做
  集合比对）。跨路由托底池的选择按**阶梯**：先取**本路由 provider** 内
  合适的目标（路由已信任的 provider 上的兄弟模型，无视 tier 领先），组内
  及其余池再按调度序（tier plan→pay → priority → 余量）——`preferRouteProviders`
  对调度结果做稳定分区实现，`#req`（能力/上下文兜底）与 `#ctx`（溢出重试）
  两条路径共用。token 用量聚合（observe/counters 的 UsageScanner）的形态内
  合并规则与请求日志索引侧（`extractSSEUsage`）逐条同构（权威定义见
  `docs/architecture/protocol-conversion.md`）：chat 方言帧的 input/cache 拆分整体
  取自 `prompt_tokens` 最大的一帧、output 取最大值（存在逐 chunk 重复累计值的网关，
  曾把单请求膨胀成上亿 token；也曾把份额帧间增长误读成增量而把 input 重复计一次），
  anthropic 帧按终帧读取，responses 快照逐字段取 MAX；跨形态（anthropic 帧与 openai
  chunk / responses 快照）仍求和——索引侧取最大，这是两侧唯一有意的跨形态差异。
  四桶账本归一化为 anthropic 口径（input 不含 cache）：inclusive 计数器
  （openai `prompt_tokens`、responses `input_tokens` 自带 cached 份额）
  入桶前先扣减该份额（钳零）并记入 cache_read——否则同一份额同时落 input
  与 cache_read，曾把 analytics 命中率压到真实值的一半并双重计费
  （与 requestlog.ExtractUsage 同一账本）。
  同 id 多 attempt 行语义：每个未 commit 的跳（429/5xx/认证/传输错误、模型拒绝、403/订阅失效
  归因、派发前门禁跳过——最后一跳也在内；client 断开的跳不落）各落一行 `attempt_only`
  请求日志——同 request id、各自递增的 attempt 序号、带该跳的观测状态与证据体（上游错误体
  peek／传输错误文本／403 归因与门禁跳过的固定原因），api_key 照记。腿行描述它自己那一跳：
  `latency_ms` 是该跳的墙钟耗时（派发到得出结果——死账号重读 key 后再打一次的两个 401 往返
  是真实耗时，派发前门禁跳过没有网络接触、读数为亚毫秒级），`upstream_model` 是该目标被
  请求的上游模型；请求体不重复落盘（`request_size` 为 0，请求体在同 request id 的已提交行
  或终态行上）。2026-11-26 之前两者恒为 0 与空，`failed_hops[].latency_ms` 这个对外字段
  分不出「真实往返」与「本地跳过」。`attempt_only` 行被
  KeyUsage 与 session 聚合排除（路由遥测，不是客户端流量）。**全链失败终态行**：forward
  以代理自己的应答收尾（纯限频 429 + `Retry-After`，否则 502）时，该应答另落一行普通行
  （`attempt_only` 不置位）：`provider` 为哨兵 `(failover)`——代理自己产生的行，不对应任何
  上游，与缓存命中的 `(cache)` 同类；`upstream_model` 为空；status、response body 与
  response headers 是客户端实际收到的字节（限频时含 `retry-after`）；request body 照常
  落盘（全链失败的请求仍可 replay）；attempt 序号排在最后一跳之后；
  `latency_ms` 自首轮派发起算（跨全部 pass 与冷却等待）；不携带 usage。该请求的
  所有腿折入这一行，所以 `attempt_only` 独立成行的只剩未应答请求（终态写出前 client 断开，
  live 事件 499）；client 断开与「无目标可试的转换错误 400」不落终态行。运行日志侧，forward 的 failover 警告行同源携带该跳状态：`[proto=… model=…] target N (<provider>/<model>) failed (status <code>); trying next`——传输层错误带 502 且其专属的 `upstream error:` 行就在上方，零字节 2xx、模型拒答与 403/订阅失效归因腿带观测到的真实状态；**派发前门禁拒绝**（model lock / 熔断开路 / 并发饱和）打 `skipped (<reason>); trying next`（`OutcomeSkipped`：无网络接触、无熔断计数，attempt 行以 502 + 原因文本落档），serve.log 单文件即可回答"这一跳为何失败/为何没派发"，无需 join 索引。存储语义之外的**查询折叠**：`/api/requests` 列表把已配对腿折入其已提交行的 `failed_hops`（未应答请求的最新一腿独立成行），status/errors 过滤看存活行终态、facets 排除腿——索引与扫描两路逐字段一致（见 `docs/web-api.md`）。
- **绑定粒度 (会话, 路由)**：sticky 键是 `runtime.StickyKey(sessionKey, exposed)`
  ——一个会话在每个路由上持有**独立绑定**（同会话换模型 = 新绑定，不把旧路由选中的
  账号拖带到新路由，与 latchKey/repeat_turn 同粒度；Bifrost「绑定按请求的 model
  分记」同义）。各自独立 dwell、独立 recover-and-return。会话键本身经
  `protocol.HeaderSessionID` 做 opaque-token 卫生（trim、≤256、可打印 ASCII）：
  无效/超长/含控制字节的值视为**无会话**（sticky 落到裸路由键）——宁可不粘也不粘
  错。出口侧同规则：`copyHeaderWhitelist` 跳过含控制字节的值，一个畸变 header
  不得毒化整个上游请求（net/http 会拒绝发送它）。
- **retry 与账号移动**：同一 target 的 in-request retry（401 refresh、413 图片
  缩压、400 剥参、developer-role 改写、thinking 自适应、连接快失败重派）在
  executor 内**原账号**重试，不重排、不换账号（锚点 `TestExecutorRetries401Then200`、
  `TestExecutorRedispatchesFastFailConnThen200`）；账号移动只发生在沿 ordered 链的
  failover（下一 target = 下一账号），wait-retry 是 round 级整轮重排。绑定不因
  retry/failover 删除（recover-and-return）。
- **每请求覆盖 `x-mp-sticky: off`**：本请求退出会话亲和——读绑定时视为无绑定
  （引擎按序新选），不写、不刷新绑定（旧条目留给 dwell 清扫）；同请求的跨路由
  改道（context overflow / capability reroute）同规则。pin/force 与可用性门控
  不受影响；未知取值忽略（打错字不得改变路由）。该 header 不在出站白名单内，
  永不上行。
- **force-provider 不参与亲和**：`x-mp-force-provider` 收窄后的目标集仍经调度器
  排序，但该请求**不读、不写任何 sticky 槽**（与 `x-mp-sticky: off` 同一抑制路径，
  由 forward 层在调度前合成）。force 是"这一笔"的一次性意图，不是引擎决策：若
  写入 sticky，被 force 的提供方会占用共享槽（会话键或无 session 客户端共用的裸
  路由键）整整一个 dwell，把全部匿名流量拖到被 force 的提供方上（实测：force 探测
  落到按量付费官方 API，把整个分析风暴拖离 plan 池）。锚点
  `TestForceProviderDoesNotWriteSticky`。
- **调度决策出处 `schedule.source`**：每条 attempt 的 request log 携带
  `schedule.source` 词表：`quota`（tier→priority→surplus 新选/切换）、`sticky`
  （(会话, 路由) 绑定保持）、`pin`（操作员 pin 收窄）、`load_balance`（轮转分
  配）、`force`（force-provider 收窄——由 forward 层盖章，调度器只看到收窄后的
  目标集）。优先级 force > pin > sticky > 引擎。与 `routing` 字段（route_policy
  层的 band/latch/selector/fallback）**正交**：`schedule` 记录调度器给出的基础
  顺序，`routing` 记录策略层在其上的重排。`/debug/route` 预览同源带出
  `schedule_source`。词表常量权威在 `internal/runtime`（`ScheduleSource*`），
  forward 层的 force 常量与 runtime 的等值由
  `TestScheduleSourceForceVocabulary` 钉住。

### server-side 会话状态（目标契约）

Claude Code server-side thread、Responses 加密 reasoning 项（`rs_*`）这类状态
绑定产生它的上游账号。当前行为：session sticky 把会话粘在同一账号（正常路径
连续性已保证）；粘滞目标中途熔断/限频时照常 failover，跨账号的加密 reasoning 项
在新账号上无法解密，由上游错误（通常 400）浮现并进入正常失败分类。目标契约
（随 prompt-cache 亲和落地，见 `docs/research/similar-projects.md` S8/S14）：

1. 请求携带 `rs_*` 项时，把产生它的账号记为该会话的粘滞目标并强制粘回——
   跨账号解密失败是确定性失败，不应消耗 failover 链；
2. 无法粘回（目标熔断）时返回可识别错误让客户端整轮重发（Bifrost
   `thread_unsupported_request` 语义），不得静默失败；
3. 不得为兼容而静默丢弃/改写加密 reasoning 项。

## 429 分类

429 分为：

| kind | 含义 | 无明确 reset 时的默认值 |
|---|---|---|
| `transient` | 请求速率或短期 token rate | `rate_limit_backoff`，默认 60s |
| `quota` | 账号余额、套餐或总配额耗尽 | `quota_cooldown`，默认 1h |
| `daily` | 当日配额耗尽 | 本地时间下一日 00:00 |
| `concurrency` | 账号的同时在途请求上限已满（step-plan `concurrency reached, current: 11, limit: 10`；另一形态是无数字的并发文案，如 zcode code 1302——该 provider 已于 2026-11-26 移除，判据仍是通用的并发措辞 + 在途数） | 不冷却——空位在一个请求完成时释放，没有时间 horizon；约束是「上限 + 在途数」（见「并发上限（在途门禁）」） |

冷却时间采用顺序：

1. body reset hint；
2. `Retry-After`；
3. 分类默认值。

没有配额/日限证据的 429 再按并发证据细分，优先级从高到低：

1. 上游明说并发上限（body 的 `concurrency … limit N`）→ `concurrency`，上限取 N；
2. body 带重置提示，或响应带可用的 `Retry-After` → 该 horizon 照旧：给自己标注了日期的限制是
   时间窗，与当时还有多少在途无关；
3. 派发时该账号另有在途请求 → `concurrency`，按在途数推断上限；
4. 账号上只有它自己 → `transient`：没有同伴的 429 是限速、套餐窗口或别的消费者，不是并发。

body hint 支持 `retry after N s/m/h/d`、`reset after 2h5m`、`Resets in 164h`（`days?|d|hours?|minutes?|seconds?` 等单位）、reset/retry 关键词邻近的 RFC3339，以及中文
`N 秒/分钟/小时/天 后重试/重置/再试`（`cnResetHintRe`，`个` 可选插入、动词词族收尾——GLM/Qwen/Kimi
的 429/配额 body 实测形态；无数字的「请稍后重试」仍非 hint）。上限 7 天；duration 在乘法前必须 clamp，防止溢出。

非 429 状态码携带配额耗尽证据时走同一分类器：`targetexec.ParseQuotaDenied`（body 命中 quota/daily 措辞即采纳，否则返回 false 保持通用 4xx 路径）复用 `ParseRateLimit` 的 kind/horizon 计算，executor 对 403 调用它并记 `RecordRateLimit` + failover（kimi-code 把 5 小时套餐窗口耗尽答成 403 "You've reached your 5-hour usage limit"，此前它落入普通 4xx commit：不冷却、不 failover，路由继续把流量派进已耗尽的账号）。

分类与 horizon 只由 `internal/targetexec.ParseRateLimitFor` 计算（`ParseRateLimit` 是它
不带在途视角的委托，只供没有在途信息的调用方与测试使用）。三个生产调用点——普通
`targetexec.Executor`、decisions selector leg（`internal/forward/decisions.go`）与 guard 判定的
调度 seam（`internal/app/model_call.go`）——各自恰好调用一次 `ParseRateLimitFor` 并传入自己
占位时的在途数：丢掉在途视角就丢掉了推断未明说上限的唯一证据
（`internal/archtest` 的 `TestRateLimitPolicyArchitecture` 钉住）。三处都把预计算的
`RateLimitDecision` 交给 runtime adapter（并发型交给 `RecordConcurrencyLimit`，其余交给
`RecordRateLimit`）。根包不得恢复 `classify429`、`parseResetHint`、`hintDuration`、
`parseConcurrencyLimit`、`hasTimeHint`、`parseTimedRateLimit` 或 `parseRateLimit` 副本。

同一 provider 收到多个 429 时，只采用更晚的 horizon；`rateLimitKind` 必须跟随胜出的 horizon，短冷却不能覆盖既有长冷却的 kind。

### 冷却被配额测量推翻

429 冷却是预算恢复时间的**预测**（reset hint / Retry-After / 分类默认），周期配额 poll 是**测量**；测量与预测矛盾时预测必须让位。`Manager.QuotaRecoveredClearCooldown`（由周期 `PollAll` 与手动 `PollOne`（Web UI 的 Refresh usage）提交快照后调用）：快照满足正面预算证据（`Err` 为空、`BillingPlan`、在新鲜度窗口内、**每个**窗口 `RemainingPct > 0`）时，清除该 provider 的 `rateLimitedUntil`（任意 kind——zhipu 的 429 文案「并发限制/余额不足」无法可靠分类，实际配额耗尽也会归为 transient（有同伴在途时会被当成并发证据，见「并发上限（在途门禁）」）；transient 默认 backoff 很短，真 RPS 限速被清后代价只是一次重新学习的 429）。不碰 `frozen`（operator 冻结）与 `circuitOpenUntil`（失败计数熔断）——它们是与预算无关的独立信号。缺失证明不是恢复证明：stale/errored/非 plan 快照一律不清。429 触发的 `RefreshOne` **不**参与清除，避免 RPS 限速与配额刷新互搏。

## 并发上限（在途门禁）

账号的同时在途请求数由 `runtime.Manager` 记账（`manager_concurrency.go`）：执行器在派发前
`TakeInflightSlot` 占位，`Execute` 返回时由 defer `ReleaseInflightSlot` 释放——流结束、失败、
客户端离开，panic 也算。一次 `Execute` 算一个在途，其内部的 401 刷新、参数改写重试不重复占位。不经执行器的两处
上游调用同样记账：decisions selector leg 从取位到返回占一个位（占不到即腿不可用、请求按
调度顺序落地），guard 判定 exchange 每次尝试在请求停留于上游期间占一个位（账号饱和则跳过
该尝试）。占位不按 generation 门控：在途是进程事实，不管请求捕获的是哪一代。

**饱和 = 上限已知且在途数已达上限**，按可用性状态处理：

- 调度器（`DecideOrder` 的 `targetAvailable`、`TargetHealthy`、`HasRecoveredUntried`）把饱和账号
  当作不可用——与限频、熔断走同一处过滤，请求不耗一次上游往返；
- 执行器在半开探针门禁之前原子占位，占不到即 `OutcomeSkipped`
  （`skipped (concurrency saturated); trying next`，attempt 行以合成 502 + 原因落档）——
  这是调度与派发之间被别的请求占满的兜底；
- pin / force 是硬选择：照常派发，不等待、不改道，但仍占位（调度器里 pin 本来就绕过可用性
  过滤）；`x-mp-force-provider`（replay）与它「不绕过熔断」一致，不绕过本门禁；
- MCP 网关选账号（`AccountHealthy`）不看并发饱和：工具调用是另一份上游预算。

**上限怎么来**（`NoteConcurrencyLimit`；任何并发型 429 都清零上调 streak）：

| 证据 | 效果 |
|---|---|
| 上游明说 `limit: N` | 上限 = N 且 `stated`：这是事实，只被另一句不同的明说更正，推断既不下调也不上调 |
| 未明说，派发时该账号另有 K（≥1）个在途 | 推断上限降到 K（只降不升） |
| 未明说，派发时没有其他在途 | 不是并发证据，沿用 transient 冷却 |

推断出的上限会被持续**往上探**：连续 20 次「满载成功」（该请求占位时账号已有 ≥ 上限−1 个在途）
后上限 +1；低占用时的成功既不计入也不打断；任何失败或并发型 429 清零 streak；上限封顶 64。
并发型 429 **不**记时间冷却（账号已有的更长冷却保持不变）。依据是 11-22 起的请求日志：
step-plan 两个账号的 429 率在在途数达到文案上限时从 0–2% 跳到 40–44%；zcode（1302，无数字）
的 429 率随在途数平滑上升（4→3%、5→10%、6→20%、9→45%），推断上限会稳定在 5–6。

**全部候选饱和**：`ConcurrencyWaitable` 报告「至少一个目标仅因并发占满而不可用」时，
`routing.DecideFailure` 返回带 `WaitForSlot` 的 `FailureWait`——forward 等空位
（`WaitInflightSlot`：任一目标有请求完成即唤醒，其它账号的完成只会让等待者复核后继续等、不耗等待轮次；单轮上限 `retry_wait`，轮次预算与冷却等待
共享的 ≤2 轮），期间客户端断开按 499 收尾；预算用尽仍饱和则 429 + `Retry-After: 1`
（不是 `rate_limit_backoff`：空位在一个请求的生命周期内就会腾出）。`retry_wait: "0"` 同样
关闭该等待。

**生命周期**：在途计数跨 reload 保留；`ReplaceGeneration` 按存活账号剪枝（存活账号保留上限与
在途请求，被移除账号的在途请求释放时落到空处）；`unfreeze` / `POST /api/health/reset` 清除学到的
上限（明说与推断都清）但不动在途计数；清除上限与 reload 移除账号都会唤醒正在等空位的请求；
不持久化，重启后由第一个并发型 429 重新学。

运行日志（转发执行器路径）：学上限只在值变化时打一行——`429 states a concurrency limit of N — dispatch capped`
或 `429 with K other request(s) in flight — concurrency cap lowered to K`——上调一行
`concurrency cap raised to N — full-occupancy successes probe upward`，等待一行
`every target is at its concurrency limit — waiting up to <retry_wait> for a slot`。decisions
selector 腿只打上调那一行；guard 判定 seam（`internal/app/model_call.go`）学上限与上调都不打行。
每个账号的在途数与上限经 `/api/status` 的 health 条目暴露（`inflight`、`concurrency_cap`、
`concurrency_cap_stated`，见 `docs/web-api.md`）。

## 模型级失败

`modelLocks` 的 key 是 `(provider, model)`，避免一个模型的问题污染同账号的其他模型。

会记录模型锁的情况：

- 任意 404；
- 400/403 且 `targetexec.IsModelDenied` 保守命中（措辞表 `modelDeniedMarkers` 与
  `missingModel`：`does not exist` 只在 `model <名字> does not exist` 形态下算，
  `not supported by this endpoint` 须带 `model ` 前缀——其余同名措辞是普通 4xx）；
- `Content-Length: 0` 的成功响应；
- 流式成功响应在 clean EOF、零字节且客户端仍连接时。

客户端断开、写错误、上游 read error 都不得学习为空响应。最后一个 target 的 404、模型拒绝的 400/403 仍原样 commit 给客户端，但模型锁继续记录，供后续请求跳过。

## 上游伪成功（2xx HTML）

中转网关在凭据过期或被 WAF 拦截时答「200 + HTML 登录页/挑战页」——把它当成功流出，
客户端拿到的是一段 markup 而不是 JSON/SSE。`targetexec.IsFakeAPIReply` 判定后按 502 语义
处理：走 failover、计入 provider 级熔断（`RecordFailure` + failure metric），永不 commit、
永不下沉到模型锁。两条判定臂都不得破坏 pre-commit 语义，并且都在返回前关闭上游 body
（这个 return 跳过了 commit 路径上唯一的关闭点，不关即钉住连接与读循环）：

- **Content-Type `text/html`**：仅凭响应头判定，对流式调用方完全不读 body（慢 TTFT 流的
  SSE keepalive 契约不受影响）；非流式调用方额外 peek 2KiB，只为提取 `<title>` 进诊断；
- **文档前缀 peek**（`<!doctype`/`<html`/`<head`/`<body`/`<title`/`<script>`，大小写不敏感）：
  单次有界读 ≤64B 并原样回放（`peekHead`），**只对非流式调用方**执行——流式调用方在首字节
  前的任何阻塞读都会吃掉心跳窗口（keepalive 用例钉住）。声明 JSON 却以 HTML 开头的 body 也
  被此臂捕获。

attempt 腿的诊断只带状态与页面标题（`upstream answered <status> with an HTML page: <title>`），
不落页面原文；流式调用方不读 body、没有标题，降级为 `html document`。

**普通转发的 403 归因**（只接在普通执行器上；decisions selector 腿不接入）：先沿用配额耗尽和模型拒绝的分类。结构化错误的
`error.code` 为 `account_disabled`、`account_deactivated` 或 `account_suspended` 时，按当前
解析到的账号记录一次认证失败并 failover，熔断门槛同 401、同样不记 failure metric；**末候选
也不例外**——不透传账号错误，按全链失败回 502（单目标路由与 pin/force-provider 同此）。
标准 nginx 系（nginx / openresty / tengine，整页文档结构精确匹配）403 文档或带
`Cf-Mitigated: challenge` 的拒绝响应，有下一候选就释放半开槽并
换目标，腿记录 403 和固定原因；最后一个候选保留原始 403。单次网关拒绝不累计账号或模型
失败。只有 HTML 类型、标题或笼统权限提示时沿用普通 4xx 语义。锁定测试：
`TestForward_Upstream403AttributionE2E`（`internal/app`）、`TestExecutor403FailureScope` 与
`TestGatewayRefusedNginxFamilyDenialPage`（`internal/targetexec`）。

**订阅失效归因**（账号归因的文本形态，同样只接在普通执行器上）：错误 envelope 的
`error.message` 明说账号没有有效套餐订阅时（step-plan 的 `you have no active step plan
subscription`，实测形态是 HTTP 400、`type: request_params_invalid`、`content-type:
text/plain`——通用类型、无 `error.code`，配额冷却与账号停用两条判据都接不住），与账号停用
同一路径：记一次认证失败并 failover、不记 failure metric、**末候选也不例外**（502 终态，
单目标路由与 pin/force-provider 同此），腿带观测到的状态码与上游错误体原文。判据边界：

- **只认 envelope 自身的 message**：同一句话出现在 body 的其它位置（如回显的请求内容）不算
  ——那是对请求的陈述，不是对账号的；
- **账号事实先于模型锁**：判定排在 404/模型拒绝之前，不落 `(provider, model)` 锁；同一体内
  命中配额措辞时配额冷却（402/403 + quota marker）仍然优先；
- **只看措辞不看状态码**，但仅限执行器本来就 peek 错误体的状态（400/402/403/404/422，或
  带 context retry 时任意 4xx）；
- 相邻语义不归因：套餐档位不含某个模型（`your active step plan subscription does not include
  model …`）是对模型的陈述，按普通 4xx 提交。

订阅失效没有可预期的恢复时刻，所以走熔断 + 半开探针（订阅恢复后至多一个 `circuit_cooldown`
自动回归）而不是 `quota_cooldown`。触发时另打一行 Info（`body proves the account has no
active plan subscription — failing over (auth circuit)`），serve.log 单文件可答。decisions selector 腿与 guard 判定 seam 不接入（同 403 归因）。锁定测试：
`TestExecutorLapsedPlanSubscriptionFailsOver`、
`TestExecutorQuotaProofOutranksSubscriptionAttribution`（`internal/targetexec`）与
`TestForward_LapsedPlanSubscriptionFailsOverE2E`（`internal/app`）。

## 内容性拒绝不熔断

5xx body 证明是**内容性拒绝**（厂商安全过滤器/response protection/内容审核措辞，
`targetexec.IsContentRefusal`，保守多词匹配，中英双语）时，失败归属于**请求内容**而非
provider：照常 failover（换一家策略不同的 provider 可能可服务同一请求），但**不计熔断、
不计失败指标**、不触发任何冷却——账号本身是健康的。反面教训：把「unapproved channel」
误判为账号故障曾让 magpie 静默封禁全部账号 5 天。普通 500 错误页绝不许误分类（裸通用词
不进 marker 表）。marker 表另一面的取舍同样有意：个别中文 marker（「安全策略」）不带
上下文限定，网关/WAF 的 5xx 拦截页命中时也按内容拒绝处理——放弃的是该场景的熔断开路
保护（每次请求付全链 failover 延迟），换取的是绝不因请求级拒绝 bench 掉健康账号。
4xx 不在此例：内容无关的普通 4xx 走原样 commit 路径（无冷却无
failover），带证据的 403/400 另按配额、模型拒绝或账号归因（见上文各节）。decisions
selector 腿同此判定（同一 8 KiB 窗口）：decisions 腿本就不记 failover 计数，命中时各
计数全零。锁定测试：`TestR6CallDecisionsContentRefusal5xx`（`internal/forward`）。

## Unsupported parameter 学习

`paramBlock` 按 `(provider, model)` 隔离：

1. **400/422** body 命中已知 unsupported/unknown parameter 形式（deepseek 的 serde 反序列化错误以 422 应答同一类拒绝——"unknown field"/"unknown variant"，两拼写都触发）；
2. 提取违规顶层字段；
3. 当次剥离并重试一次；
4. 后续请求在 `RewriteRequest` 后预防性剥离。

同一学习-重试家族还有两条**改写型**课程（`internal/targetexec/failure.go`，只在本上游真的拒绝后才改写——接受该形状的上游永不被碰）：

- `developer_role`：chat 方言拒绝 OpenAI `developer` role（volcengine "invalid value: \`developer\`"、deepseek "unknown variant"——**400 与 422 两种拼写**、kimi-code "not a valid role"、zhipu "角色信息不正确"）→ 把 `messages[].role` 的 `developer` 改写为 `system` 后重试一次。
- `thinking_adaptive`：anthropic 方言拒绝 budget 式 thinking（shopee claude 网关 400 "thinking.type.enabled is not supported … Use thinking.type.adaptive"）→ 把 `thinking` 改写为 `{"type":"adaptive"}` 后重试一次。

两条课程与剥参一样持久化在 `(provider, model)` 的 paramBlock 里，经 `applyParamBlock` 在后续请求上预先应用。

以下字段永不自动剥离：

`model`、`messages`、`input`、`prompt`、`system`、`stream`、`tools`、`tool_choice`、`response_format`。

剥参重试不占用 401 refresh 的 retry slot。匹配规则必须保守，不能根据裸的通用错误词锁模型或删除语义字段。

## 全冷却等待

`retry_wait` 默认 10s，`"0"` 关闭。`forward` 通过可重入的 `serveOnce` 执行完整调度和 failover：

- 仅当全部实际候选目标都处于冷却且最早恢复时间不超过预算时等待；全部候选都在并发上限上
  时等的是空位而非 horizon（事件驱动，见「并发上限（在途门禁）」）；
- 最多整体重试两次；
- 重试轮共享首轮的 per-request 缓存：请求画像（rawProfile）与 route_policy selector 的
  decisions 结果都按「每请求至多计算一次」缓存在 `serveState`，后续轮复用而非重新付费
  （见 `docs/architecture/request-routing.md` 的 selector 一节）；
- 已恢复但本轮未尝试的目标应零等待重新调度（配额已知耗尽、被 skip 的目标**不算**已恢复）；
- `x-mp-force-provider` 和已断开的客户端不等待；
- 终局分类应基于跨轮实际失败类别：纯限频返回 429 和 `Retry-After`，出现硬失败返回 502。配额已知耗尽（新鲜 plan 快照 ultimate RemainingPct==0）按限频类参与分类——全耗尽 route 终局 429，`Retry-After` 取窗口 reset 与快照失鲜边界的较早者（详见 `runtime-state.md` 的 Sticky/availability 一节）。

### 等待层目标来源

等待层基于 `serveResult.effectiveTargets` —— `serveOnce` 实际考虑的目标集合（schedule 过滤冷却目标、request-aware 改道、context overflow 替换之后），而非原始 route targets。`forward` 用它计算 cooldown / Retry-After / TOCTOU；effective 为空（例如 schedule 因全部冷却而返回空）时回退到原始 targets。这避免「健康但被过滤的兄弟目标」让 `cooldownState` 误判仍有可用目标而跳过等待。

`hasRecoveredUntried` 只依赖「是否存在 available 但本 pass 未尝试的目标」：冷却中途恢复（TOCTOU）或全部目标同时恢复时给予零等待重排，由 forward 的 round budget（≤2）防止无限循环。不再要求「至少仍有一个目标在冷却」。

## 流式保活（SSE keepalive）

`stream_keepalive` 默认 15s，`"0"` 关闭。客户端面向的 SSE 流（`clientStream` 且 status < 300）在上游静默超过该间隔时，`targetexec` 经 `flushCopyHeartbeat` 向客户端写一帧 SSE 注释心跳（`: ping\n\n`，协议级 no-op）。契约边界：

- **只在 commit（上游响应头已收到、`WriteHeader` 已发）之后才开始**——header 之前的连接失败仍完整保留透明 failover；代价是 header 之后上游在 prefill 中途断流无法再 failover，客户端看到断流（有意取舍，慢 TTFT 收益大于该窗口风险）。
- 心跳写在响应链的**客户端写出侧**，不经过 body 读取侧的 CaptureResponse/CaptureUsage——request log 与 usage 永远见不到心跳帧。
- 心跳经 `timingResponseWriter.WriteHeartbeat` 旁路首字节标记——TTFT 只测量上游真实首字节，不被代理 padding 污染。
- 非流式（JSON）响应绝不写注释帧（会损坏 body）；数据流动时每写一帧重置计时器，只在静默期发心跳。

## 回归测试

- half-open 的成功、5xx、429、普通 4xx 生命周期。
- 并发上限（在途门禁）：账本、门禁与两个 seam 见 `docs/engineering/testing.md` 的对应行与
  `docs/decisions/intentional-behaviors.md` 决策 53；分类入口的唯一性由 `internal/archtest` 的
  `TestRateLimitPolicyArchitecture` 钉住（三个生产调用点各恰好一次 `ParseRateLimitFor`，组合根
  不得有解析副本）。
- 2xx-HTML 伪成功：两臂判定、502 语义 failover+熔断、标题进诊断、真实 JSON 提交不受
  peek 影响、流式请求不做 body peek（keepalive 契约）。
- 内容性拒绝 5xx：failover 但零冷却记账（熔断/失败指标/releases 各就各位）、普通 5xx 仍记账。
- transient/quota/daily 及 body/header reset 优先级。
- 普通 target 与 decisions selector leg 的 429 kind/horizon、metric、quota refresh 一致。
- 同 provider 不同 model 的锁和 paramBlock 隔离。
- empty stream 的 EOF、client cancel、upstream read error。
- 上游 403 归因（`internal/app/forward_403_attribution_test.go`、`internal/targetexec`
  的 `TestExecutor403FailureScope`）：账号停用码换候选并记熔断、末候选终态 502；网关拒绝
  换候选且零记账、末候选保留原始 403；通用 HTML 403 原样 commit。Auth 层失败（如 aqp mint
  被网关 403）由 `internal/targetexec` 的 `TestExecutorAuthLayerFailureKeepsObservedStatus` 钉住：
  立即 failover、腿带观测到的 403 与错误文本、不派发上游请求、不计熔断。
- 订阅失效归因（`internal/targetexec` 的 `TestExecutorLapsedPlanSubscriptionFailsOver` 与
  `TestExecutorQuotaProofOutranksSubscriptionAttribution`、`internal/app` 的
  `TestForward_LapsedPlanSubscriptionFailsOverE2E`）：message 证明无有效订阅 → 认证失败记账 +
  failover、末候选终态 502、腿带上游错误体；轮转池里无请求吸收该 400；套餐档位不含该模型、
  请求内容回显措辞、未 peek 的状态码、普通 400 均不归因；配额佐证优先于订阅失效。
- 凭据拒绝记录（`internal/targetexec` 的 `TestExecutorRecordsCredentialDenialOnlyForAccountAttributions`、
  `internal/admin` 的 `TestDashboardForwardCredentialDenials`、`internal/app` 的
  `TestForward_CredentialDenialSurfacesInStatusE2E`）：三处账号归因各上报恰好一条、其它失败
  类别静默；三类经真实 admin 端口出现在 `credential_alerts` / `quota[name].denial` / Schedule
  徽标并在账号恢复后一起消失；轮询的横幅级记录优先、非凭据类同步失败不遮蔽拒绝。
- 全链失败终态行与每跳落腿（`internal/forward/terminal_row_test.go`、`internal/app` 的
  `TestForward_AllTargetsFailedFoldsLegsIntoTerminalRow`）：终态行字段逐一断言（哨兵
  provider、客户端实际字节、请求体、attempt 序号、429 的 `Retry-After`）、最后一跳落腿、
  client 断开不落行、列表折叠为一行终态且不贡献 token。腿行带该跳的墙钟延迟与上游模型
  （`TestServeLegCarriesHopLatencyAndUpstreamModel`：上游跳的延迟 ≥ 夹具保证的下限、门禁
  跳过的低于它；`failed_hops[].latency_ms` 的端到端断言在折叠用例里）。
- 提交后流中止归因（`internal/targetexec/executor_stream_abort_test.go`）：对端写失败先于上游终态在普通与 keepalive 两条拷贝路径都打 client 断开行、不得误判上游截断；上游自带错误终态打 Warn 带摘录；上游已送达终态后的写失败与完整流不打行（契约在 `docs/architecture/protocol-conversion.md`「提交后流中止的归因日志」）。
- SSE keepalive：静默期心跳帧、ttft 不被心跳标记、录制链/缓存无心跳、client gone 后 reader goroutine 释放。
- cooldown 全部恢复、部分恢复、跨轮纯 429、混合硬失败。
- request-aware/context retry 后的实际目标冷却判定。
- usage 账本归一化：inclusive wire（openai `prompt_tokens` / responses
  `input_tokens`）cached 份额扣减入桶、真实录制 wire 形态（含 aqp/kimi-k3
  事故形态）与逐 chunk 重复累计值的合并。
- 交互矩阵：quota 模式 sticky×熔断/限频/模型锁的排除、失败不复活、dwell 内
  recover-and-return、dwell 过期后按 margin 切换；部分链 `skipped` 与空链
  `blocked` 的原因分类（负载均衡策略的同类语义在 load_balance 用例覆盖）。
