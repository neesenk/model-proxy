# 有意的失败与路由行为

这些行为容易在评审中被误认为 bug。修改前必须同时检查实现中的 `INTENTIONAL` 注释和 `docs/architecture/routing-and-failure.md`。

共同原则：客户端通常是无人值守 agent，明确失败并允许重试优于静默返回不可用结果。

## 决策

1. **空 200 视为模型失败**：LLM 端点没有合法的空成功响应，部分逆向网关会在过载时返回空 200。
2. **404 failover 并锁模型**：proxy 只转发已知 LLM 路径，404 表示模型或上游路径不可用。
3. **400/403 model-denied failover**：只有保守命中模型不可用语义才 failover；普通 4xx 原样 commit。
4. **Unsupported parameter 自动剥离一次**：只处理顶层字段，受 never-strip 白名单保护，并按 `(provider, model)` 学习。
5. **daily/quota 429 使用长冷却**：body reset hint 始终优先；没有 hint 时 daily 到午夜、quota 默认 1h；`unfreeze` 是人工逃生口。
6. **冻结态恢复需要 config fingerprint**：防止不同配置或测试二进制把同名 provider 的旧状态恢复到当前实例。
7. **最后一个 target commit 上游错误**：保留真实 404/400 状态与错误信息，而不是统一改写成 502；跨协议时只把错误 envelope 翻译为客户端协议，学习到的模型锁仍保留。
8. **Pin 是硬禁 failover**：即使 pinned provider 已熔断，仍返回失败，不自动逃到其他 provider。
9. **请求感知二次 schedule 为空时回落原目标**：宁可尝试可能不匹配的目标，也不允许零尝试 502。
10. **普通显式历史中的孤儿 tool 对原样透传；仅 previous_response_id miss 修复**：完整 `input/messages` 转换不擅自删改不配对工具项（锁定测试 `TestConvertFault_OrphanToolPairs`）。只有 Responses 跨协议请求声明 `previous_response_id` 且本地状态未命中时，才把 orphan output 降级成 user 文本、删除 dangling call/reasoning，避免无服务端状态后端直接 400。
11. **wire verdict unknown 时维持透传**：探测未完成或结论为 unknown（超时/5xx）时，协议选择回落到客户端协议透传，与探测功能引入前的行为一致——探测只能"改善"默认行为，不能在 boot 窗口期改变它。锁定测试：`TestResolve`（`internal/runtime/wirecap/store_test.go`，决策矩阵覆盖 unknown/超时等价路径）。
12. **wire 探测把 404 以外的 4xx 视为端点存在**：探测分类里 400/401/403/429 都判 yes——400 是请求形状争议而非路由缺失，401/403/429 更是端点存在的直接证据；只有 404 判 no（proxy 只探测已知 LLM 路径）。误判 yes 的兜底是运行时 404 纠正（翻转 verdict + 跳过模型锁），因此探测本身不做更细的形状校验。
13. **Responses 跨协议 body 解析失败落 502 而非 400**：responses 客户端走跨协议 target 时 body 先经 `responsesState.expand` 做本地历史展开（proxy.go serveOnce）；body 本身是非法 JSON 时展开报错、当前 target 被跳过，所有 target 都失败后走统一的 all-targets-failed 路径返回 502，而不是 400。转发主路径不整体解析客户端 body（同协议字节透传），调度层无法区分「客户端 body 坏」和「单 target 处理失败」，保持 fail-closed 跳过语义。
14. **唯一可转换 target 冷却中时立即 400 而非等待恢复**：本 pass 记录了 `conversionErr` 且 `tried` 为空时（请求转换被 capability scanner 拒绝的 target 不计入 tried；唯一能安全转换的 target 在冷却、未进本轮调度），直接返回 400 `unsupported_protocol_conversion`，跳过 cooldown wait-retry——不等待冷却恢复。重试中的 agent 下一轮自然会再命中已恢复的 target。
15. **guard known-secret 凭据值进内存扫描器**：代理自身管理的凭据（池 key、OAuth token）以内存值形式进入 `guard.Scanner` 做出站精确匹配——不违反"凭据不进 config/代码/日志/测试输出"红线，因为匹配集永不落盘、不序列化、不进事件/DTO（命中只报 `known_secret` 类型名）。OAuth token 进程内轮转（codex/aqp 原地刷新写回 auth 文件）后，后台节拍（`scheduling.quota_poll_interval`，默认 5m）自动重收 OAuth 文件并按当前代重建换入扫描器，最迟一个周期生效；轮转的间隙里旧值仍被扫描（无害，旧值已失效）。重建只用当前代的池秘密基（reload-owned），跨代混用在代检查处丢弃。同一节拍还收集 provider 经 `provider.SecretReporter` 上报的**只存在于内存**的凭据：aqp 的 managed API key 是运行时经 SSO cookie mint 的，任何文件都不存；codex 的内存 access_token 也可能比文件新。这是 provider 凭据值的首次接口级暴露——例外成立的条件与扫描集相同：只读内存、只为出站扫描、永不序列化/日志/落盘（未 mint/未缓存时上报空集）。
16. **guard.paths 不支持 redact**：敏感路径命中只有 log/block/off——redact 会改写 `.env`、`~/.ssh` 等路径文本，破坏正常编码负载（读 .env 是 agent 的合法工作）；要阻断用 block，默认 log 只要可见性。
17. **guard 命中永不含匹配内容**：live event、计数器、安全审计日志（seclog）只携带模式类型名/路径类别名，匹配到的秘密字节只允许出现在 redact 后的转发 body 里（被替换为 `[REDACTED]`）。审计日志因此可以安全长期保留。
18. **cache key 基于 redact 后的 body**：guard.secrets=redact 先把秘密替换为 `[REDACTED]`，响应缓存再对改写后的 body 取 key——因此两个仅秘密值不同的请求 redact 后共享同一条缓存条目（语义有意：缓存命中的应答本就不依赖被抹掉的秘密，且避免了把秘密派生进缓存 key）。pin/force-provider 仍按既有红线绕过缓存，不受此影响。
19. **seclog 审计日志 reload 换代、换代瞬间允许丢尾记录**：`guard.audit` 开关与 `audit_path` 变更在 reload 时立即生效（off→on 当场开始写、on→off 当场停写、路径变更换新文件）——旧 logger 在换代时先 drain 再关停，forward 只写请求自己快照里的 logger。换代瞬间在途请求若仍持旧快照 enqueue，旧 logger 已 drain 完，这几条尾记录被静默丢弃（Enqueue 是非阻塞 offer，无人再消费）：审计日志是 best-effort 可见性通道，绝不为持久化阻塞或失败请求路径。
20. **guard 分片检测的会话窗口存 redact 前原文**：`guard.session_scan` 的会话窗口（store 由 `internal/guard/session` 拥有；按 `x-claude-code-session-id`，每会话 32KiB 尾窗、LRU 256、≤8MiB）保存的是 redact 之前的请求 body——存 redact 后形态会让后续分片检测失效（被抹掉的分段永远拼不回来）。这是内存敏感性的有意取舍：窗口可能含凭据，因此只活在小锁保护的进程内存里，永不落盘/日志/序列化/API；有界性（截断即重置分片进度、LRU 整体淘汰）是它的暴露上限。它不进 RuntimeSnapshot（跨代运行时观察态，参照 metricsStore），reload 不清空，避免 reload 抹掉在途会话的分片上下文。会话键是**客户端可控**的 `x-claude-code-session-id` 头：省略或轮转该头的 agent 永远不会进入本通道，256 个不同 id 即可挤空整个 LRU——它是对配合客户端的有界启发式，不是对有意规避者的检测保证。
21. **guard 分片命中 redact 降级为 log、block 只拦补齐段**：分片泄露的秘密横跨多条请求，没有任何单个 body 可以被改写——redact 对 `known_secret_fragmented` 有意降级为 log（event/audit 的 action 记为 log），block 则 400 拒绝补齐段所在的请求；此前的分段已放行，因为它们各自是不含完整秘密的干净请求，单请求扫描无从拦截。检测有两条通道（`proxy_forward.go` 同处运行，本条是唯一权威定义）：**① 相邻窗口精确拼接**——只在"拼接体命中而尾窗单独不命中、当前 body 单独不命中"时计 fragmented；一个只存在于拼接体的命中必然跨越接缝，而 known-secret needle 最长为 `Scanner.MaxKnownNeedleLen`，因此实现只扫描接缝区（尾窗末尾 + 当前 body 开头各 MaxKnownNeedleLen-1 字节），判定与整段拼接扫描完全等价，但省掉每请求至多 64MiB+32KiB 的拼接拷贝；尾窗单独命中判定来自会话条目里 Add 时增量维护的缓存（截断或换代失效时回退重扫），不再每请求重扫尾窗。真实流量上本通道接近零命中（能转发的 body 都以 `{` 开头，ExtractModel 要求，两条 JSON 请求的分段在拼接处永远不可能字节相邻），但作为 fail-closed 兜底保留。**② 按序最长前缀进度**（`ScanKnownFragment`，每段 ≥8 字节、只覆盖 known-secret 原文）——覆盖分段各自落在单条请求 JSON 内部的真实拆分，是实际起作用的通道。某秘密跨请求补齐（或单请求出现完整值）时扫描器将其进度**刻意归零**并经 `reset` 返回值声明，存储层合并进度时对恰好这些下标取 0 而非元素级 max——否则陈旧进度会让后续 suffix 分段把同一秘密重复报成 fragmented（回归测试：`TestSessionScan_NoRefireAfterCompletion`）。
22. **apikey 池 keychain 模式不可用时 fail-closed，不回落明文**：`credentials: keychain` 是用户显式的安全选择——后端不可达（headless Linux 无 Secret Service、钥匙串被锁）或元数据账户的 keychain 条目丢失时，`accounts.Store` 的读写直接报错（`credstore.ErrUnavailable`/`ErrNotFound`），而不是悄悄退回读取明文池文件（回落会让"秘密已进钥匙串"的预期在故障时无声失效，且明文文件可能正是用户想摆脱的东西）。代价是 keychain 故障期间该 provider 整体不可用，需要修复后端，或把 `credentials:` 改回 `file` 走条目 25 的回迁。删除语义同样偏向不留孤儿：先删 keychain 条目成功才落元数据文件（删除失败则整个保存报错），宁可保留可重试的元数据，也不留"元数据没了、秘密还挂钥匙串"的孤儿条目。锁定测试：`internal/accounts/store_keychain_test.go`（`TestKeychainUnavailableFailsClosed` 等）。
23. **guard.paths 弱命中（正文提及）不 block、不发 live event**：敏感路径命中按结构位置分 strong/weak（`guard.ScanPathsContext`）——路径出现在工具调用/工具结果位（anthropic `tool_use.input`/`tool_result.content`、openai `tool_calls[].function.arguments` 与 `role:"tool"` content、responses `function_call.arguments`/`function_call_output.output`）是 strong，即"agent 通过工具读敏感文件"的 MCP Tool Poisoning 特征动作；普通正文/user 消息提及是 weak。coding agent 的正常对话大量讨论 `.env` 等路径，weak 若发 live event 会刷屏监控、若 block 会误伤正常负载；因此 weak 只计 `("guard", <类别>_text)` 计数器并写 action=`log-weak` 的审计记录保留可见性，live event 与 block 只对 strong 生效。body 非合法 JSON 或结构识别失败时一律降级 weak（宁低勿高），绝不因识别失败升级为 strong。锁定测试：`internal/guard/pathctx_test.go`、`internal/app/guard_runtime_integration_test.go`（`TestGuardPaths_WeakTextNeverBlocks` 等）。
24. **凭据存储单一开关：`credentials:` 驱动池与 OAuth 两侧，env 只覆盖 OAuth**：收敛前池后端走 config `credentials:`、OAuth blob 走 env `MP_CRED_STORE`（默认 auto 探测），两套开关语义割裂。收敛后优先级为 **env `MP_CRED_STORE` 非空 > config `credentials:` > 默认 file**：config 同时应用到 `accounts` 池后端和 credstore OAuth 模式（config 加载点统一调 `accounts.SetProcessCredentialsMode`）；env 保留为已发布的显式 override，只作用于 OAuth 侧（`auto` 收敛为 opt-in 的探测语义，不再是默认）。env 与 config 不一致是唯一可能的分歧，`config check` 与启动/reload 日志各打一行说明两侧生效值与来源。代价是行为变更：此前未设开关、靠 auto 默认进 keychain 的 OAuth blob，升级后默认按 file 读取，需显式设 `credentials: keychain`（或 env）继续读原条目。锁定测试：`internal/credstore/credstore_test.go`（`TestResolveModePriorityMatrix`、`TestConfigMismatchNote`）、`internal/cli/config/config_cmd_test.go`（`TestCLI_ConfigCheckCredentialsSummary`）。
25. **keychain→file 部分回迁：缺条目或身份不一致时保留元数据；显式 logout 跨模式清理**：`credentials:` 切回 `file` 后，纯元数据池按条目从 keychain 读回秘密；只有全部条目成功、凭据推导出的 canonical ID 与原 metadata/keychain namespace 相同才原子重写 0600 明文池。Volcengine 非 API-only 身份必须同时恢复 AK/SK，缺半不得降级。缺条目、后端不可达或非 canonical ID 的账号进入 secretless `Snapshot.ReloginNeeded`，部分回迁不动原文件；普通 Save 还要求覆盖并 canonical 恢复所有原 metadata ID，防止逐个 login 静默丢记录，放弃账号必须显式 remove。完整回迁写明文前先写 0600、无秘密的 `<pool>.keychain-origin` marker；读路径有意保留 keychain 副本，`Store.RemoveAccount` / `RemoveAllAccounts` 才清理。metadata-only 删除以当前被移除 ID 为最低清理集合，marker 只补充历史 provenance，不能因切回 keychain 后变旧而压制新账号删除；失败保留 pool/marker 供重试，纯 file 无 marker 不碰 keychain。混合池仍按 validate 报错。OAuth blob 不自动回迁，切回 file 需重新 login；但 keychain Save/Load 会写无秘密的 `<authfile>.keychain-origin`（legacy keychain-only 首次 Load 补写），`Ref.Delete` 在当前 keychain 模式或 `.keychain-origin` / `.migrated.bak` 表明曾使用 keychain 时一并删 keychain 条目。删除失败保留 origin marker 以便跨模式重试；纯 file 历史无 marker/档案时永不触碰后端。锁定测试：`internal/accounts/store_restore_test.go`、`internal/credstore/credstore_test.go`。
26. **guard 扫描按次预算：对抗性 body 有界降级为漏报，不再无界耗时或内存**：单次扫描的编码通道解码上限 256 次（`maxDecodesPerScan`）、命中声明上限 262144（`maxMatchesPerScan`）；Aho-Corasick phase-1 在 append occurrence offset 时也立即限额，raw known、encoded known、probe 三层各自最多 262144，低优先级 probe 不能挤占 known-secret。只需回答“是否命中”的 `ScanKnown` 不保存 offset，只留 raw/encoded 两个存在位。对抗性构造（长 token run 内以字节间隔重复探针变体 → 滑动 span 每命中一次 8KiB 解码；重复廉价 probe/秘密形状百万次 → phase-1 position 无界堆积或命中声明两两重叠检查）此前可把单请求推到 100GB 级解码量、O(n²) 比较或与 body 命中数同比的额外内存——guard 默认开启且威胁模型内的恶意 agent 正是本地请求方。预算耗尽后对应通道停止收集，方向是**有界漏报**（宁漏勿滥，与 redact 侧的宁滥勿缺相反）；`claimSet` 有序不交区间集 + 流式正则迭代保证正常负载路径线性。锁定测试：`internal/guard/scanner_budget_test.go`。
27. **keychain 条目超限（ErrEntryTooLarge）时懒迁移保留明文服务，不锁死账号**：OS 钥串后端有物理条目上限（darwin 命令行预算 ~3000 原始字节、windows 凭据 blob 2560 字节；codex OAuth 档案典型 2.6-3.6KB，常超限）。`Ref.Load` 的懒迁移写入被拒时**返回明文数据**：尺寸超限是 blob+后端的确定性属性，不是可用性或降级问题，明文权威副本完好、后续每次 Load 都会重试并落回同处；此前一律 fail-closed 会让进程明明读得到的凭据变得不可读。写入路径（`Save`/`KeychainSet`）按平台预检尺寸、提前返回 `ErrEntryTooLarge`（darwin 超限命令会在进程已启动后才被拒、搁置子进程；错误类别与 `ErrUnavailable` 分开，排障与修复路径不同：前者改回 `credentials: file`，后者修后端）。这与条目 22 的"不可用 fail-closed"不冲突：可用性故障仍原样报错。锁定测试：`internal/credstore/credstore_test.go`（`TestRefLoadOversizedBlobKeepsServingPlaintext`、`TestMapKeyringErrPreservesSizeClass`）。
28. **guard scanner 构建失败：启动降级、换入侧一律拒绝（有意不对称）**：`buildGuardScanner` 的错误只可能来自手工构造的未验证 `Config`（`validate` 在加载时已拒绝非法 `guard.extra_patterns`），三类入口对同一错误取两种语义。启动（`NewProxyWithStatePath`）**降级**：先回退内置规则表 + known secrets（warn 一行），再失败则 `guardScanner = nil` 完全关闭出站 guard——这行用 error 级记录（`logx.Errorf`）：安全控制关闭必须在 `log_level: error` 下也可见，是级别过滤（063b2f9）的有意例外。一个拒绝启动的代理比少了 guard 的代理更糟，且该错误在正常加载路径不可达。所有"已有可用 scanner 兜底"的换入侧一律**fail-closed 拒绝新代**：reload（`Proxy.Reload`）整次拒绝保留旧 generation，OAuth 凭据重同步节拍（`guard_wiring.go`）保留旧 scanner 等下一拍。即：无服务可用时宁降级也要启动，有旧代/旧 scanner 兜底时绝不允许降级换入。

## qwen-plan：用量仅控制台、不轮询（有意为之）

千问 Token Plan 个人版的 Credits 用量（5h/7d 窗口）**没有公开 API**（文档「以控制台订阅页用量明细为准」），且平台条款「严禁 API 调用」明确禁止自动化/批量调用（仅允许 Claude Code/Cursor 等交互式工具）。model-proxy 作为交互式开发工具的转发代理，转发本身合规；但**有意不实现**控制台 cookie 抓取或后台配额轮询——`Quota()` 返回 `BillingUnknown`，仅把控制台订阅页 URL（`https://platform.qianwenai.com/home/billing/subscription/token-plan-individual`）附在 CLI `usage` 与 Web UI（`/api/status.quota` → app.js 渲染 `snap.Notes`）里供人工查看。窗口耗尽由 429 `Allocated quota exceeded` 经 `internal/targetexec.ParseRateLimit` 分类为 `quota` 后反应式触发冷却与 failover，无需新增轮询。

## 有意的测试与观测行为

- 生产包的 `_test.go` 不受 DAG import policy 检查（仅无生产文件的目录例外）：
  测试依赖历来比生产宽（fixture 需要跨包构造），"依赖图闭合"只在生产维度成立。
  若要收紧，先改 `architecture_dependency_dag_contract_test.go` 的分类逻辑并同步
  本文件。
- 登录交互式输入 API key 时终端明文回显：stdin 同时服务管道/脚本输入
  （`echo key | model-proxy login ...`），`term.ReadPassword` 会破坏非终端输入。
  凭据不会进入日志，回显只存在于用户自己的终端缓冲。
- `observe/events` 每条事件对每个订阅者**恰好投递一次**：订阅注册、recent
  快照复制与 publish 的订阅者快照加载全部串行化在同一把锁内——一条事件要么
  出现在订阅者的 recent 快照里，要么出现在订阅流里，不会两者都出现（COW 化
  时曾在 Unlock 后才加载快照，重新打开过双投递窗口）。锁定测试：
  `TestHubPublishSubscribeExactlyOnce`。
- Fusion 面板全部以 429 冷却失败时，终局按 hard 处理返回 502 而非 429+
  Retry-After：fusion 伪 provider 无健康状态，`cooldownState` 判不出 allDown；
  代码注释自认 "opaque → hard"。收紧前先给 fusion 伪 provider 建立限频观测。
- 半开单飞（`halfOpenInFlight`）期间，并发请求把该 provider 视为不可用，若其余
  target 也全灭则立即终局 502，不享有 retry_wait 等待——这是单飞探针语义的固有
  代价（让一个请求独占探测权），与"短冷却等待优于立即报错"的整体取向的偏差是
  已知且接受的。

## 有意的 Web 与 guard 行为

- `AddPreset` 落盘后 reload 失败**不回滚**（保留合并块、返回 `reload_warning`）：
  preset wizard 流程要继续走 login，而 `saveAndReload` 路径 reload 失败会从备份恢复
  原文件。同一 config.yaml 上两条 mutation 路径失败语义相反是有意的——wizard 的
  合并块不含凭据、可幂等重试，回滚反而会丢掉用户刚确认的 provider 配置。
- 分片外传检测（`known_secret_fragmented`）的会话键来自客户端可控的
  `x-claude-code-session-id`：任一客户端可用随机 id 挤出 256 条 LRU 里的其他会话
  窗口，使检测**有界漏检**（与 guard 扫描器各资源上限同向）。这是有界内存的
  固有取舍，不是可修的缺陷。
- admin 会话 cookie 直接承载 admin token 本身（base64，非一次性 session id）：
  cookie 泄露等价于 token 泄露且无法单独吊销。已由 HttpOnly + SameSite=Strict +
  强制 `Authorization: Bearer` 铸造（防 `x-api-key` 意外铸造）+ validate 层非回环
  监听强制鉴权缓解；引入独立 session 存储前该取舍保持不变。

## 修改要求

- 改变任一决策必须同步更新本文件、实现注释和回归测试。
- 扩大错误字符串匹配范围时必须增加至少一个正例和一个相邻语义负例。
- 新的自动请求改写必须说明作用域、次数上限、never-mutate 字段和可见日志。
