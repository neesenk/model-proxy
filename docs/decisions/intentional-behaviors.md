# 有意的失败与路由行为

这些行为容易在评审中被误认为 bug。修改前必须同时检查实现中的 `INTENTIONAL` 注释和 `docs/architecture/routing-and-failure.md`。

共同原则：客户端通常是无人值守 agent，明确失败并允许重试优于静默返回不可用结果。

## 决策

1. **空 200 视为模型失败**：LLM 端点没有合法的空成功响应，部分逆向网关会在过载时返回空 200。
2. **404 failover 并锁模型**：proxy 只转发已知 LLM 路径，404 表示模型或上游路径不可用。
3. **400/403 model-denied failover**：只有保守命中模型不可用语义才 failover；普通 4xx 原样 commit。
4. **Unsupported parameter 自动剥离一次**：只处理顶层字段，受 never-strip 白名单保护，并按 `(provider, model)` 学习。
5. **daily/quota 429 使用长冷却**：body reset hint 始终优先；没有 hint 时 daily 到午夜、quota 默认 1h；`unfreeze` 是人工逃生口（no-arg = 全部），`freeze` 是反向人工开关（显式冻结 provider 至 unfreeze，成功/失败记录不解冻；**刻意只接受显式 provider、无 freeze-all**——全冻结=自我断供，批量形式只留在逃生口一侧）。
6. **冻结态恢复需要 config fingerprint**：防止不同配置或测试二进制把同名 provider 的旧状态恢复到当前实例。
7. **最后一个 target commit 上游错误**：保留真实 404/400 状态与错误信息，而不是统一改写成 502；跨协议时只把错误 envelope 翻译为客户端协议，学习到的模型锁仍保留。
8. **Pin 是硬禁 failover**：即使 pinned provider 已熔断，仍返回失败，不自动逃到其他 provider。
9. **请求感知二次 schedule 为空时回落原目标**：宁可尝试可能不匹配的目标，也不允许零尝试 502。
10. **普通显式历史中的孤儿 tool 对原样透传；仅 previous_response_id miss 修复**：完整 `input/messages` 转换不擅自删改不配对工具项（锁定测试 `TestConvertFault_OrphanToolPairs`）。只有 Responses 跨协议请求声明 `previous_response_id` 且本地状态未命中时，才把 orphan output 降级成 user 文本、删除 dangling call/reasoning，避免无服务端状态后端直接 400。
11. **wire verdict unknown 时维持透传**：探测未完成或结论为 unknown（超时/5xx）时，协议选择回落到客户端协议透传，与探测功能引入前的行为一致——探测只能"改善"默认行为，不能在 boot 窗口期改变它。模型级矩阵无条目或相关腿未结论时同样不回退客户端协议，而是**回落 provider 级矩阵**再按本条处理（`runtimewire.ResolveModel` → `Resolve`）。锁定测试：`TestResolve`、`TestResolveModel`（`internal/runtime/wirecap/store_test.go`，决策矩阵覆盖 unknown/超时等价路径）。
12. **provider 级 wire 探测把 404 以外的 4xx 视为端点存在**：provider 级探测分类里 400/401/403/429 都判 yes——400 是请求形状争议而非路由缺失，401/403/429 更是端点存在的直接证据；只有 404 判 no（proxy 只探测已知 LLM 路径）。误判 yes 的兜底是运行时 404 纠正（翻转 verdict + 跳过模型锁），因此探测本身不做更细的形状校验。模型级分类的口径刻意不同，见条目 29。
13. **Responses 跨协议 body 解析失败落 502 而非 400**：responses 客户端走跨协议 target 时 body 先经 `responsesState.expand` 做本地历史展开（proxy.go serveOnce）；body 本身是非法 JSON 时展开报错、当前 target 被跳过，所有 target 都失败后走统一的 all-targets-failed 路径返回 502，而不是 400。转发主路径不整体解析客户端 body（同协议字节透传），调度层无法区分「客户端 body 坏」和「单 target 处理失败」，保持 fail-closed 跳过语义。
14. **唯一可转换 target 冷却中时立即 400 而非等待恢复**：本 pass 记录了 `conversionErr` 且 `tried` 为空时（请求转换被 capability scanner 拒绝的 target 不计入 tried；唯一能安全转换的 target 在冷却、未进本轮调度），直接返回 400 `unsupported_protocol_conversion`，跳过 cooldown wait-retry——不等待冷却恢复。重试中的 agent 下一轮自然会再命中已恢复的 target。
15. **guard known-secret 凭据值进内存扫描器**：代理自身管理的凭据（池 key、OAuth token）以内存值形式进入 `guard.Scanner` 做出站精确匹配——不违反"凭据不进 config/代码/日志/测试输出"红线，因为匹配集永不落盘、不序列化、不进事件/DTO（命中只报 `known_secret` 类型名）。OAuth token 进程内轮转（codex/aqp 原地刷新写回 auth 文件）后，后台节拍（`scheduling.quota_poll_interval`，默认 5m）自动重收 OAuth 文件并按当前代重建换入扫描器，最迟一个周期生效；轮转的间隙里旧值仍被扫描（无害，旧值已失效）。重建只用当前代的池秘密基（reload-owned），跨代混用在代检查处丢弃。同一节拍还收集 provider 经 `provider.SecretReporter` 上报的**只存在于内存**的凭据：aqp 的 managed API key 是运行时经 SSO cookie mint 的，任何文件都不存；codex 的内存 access_token 也可能比文件新。这是 provider 凭据值的首次接口级暴露——例外成立的条件与扫描集相同：只读内存、只为出站扫描、永不序列化/日志/落盘（未 mint/未缓存时上报空集）。
16. **guard.paths 不支持 redact**：敏感路径命中只有 log/block/off——redact 会改写 `.env`、`~/.ssh` 等路径文本，破坏正常编码负载（读 .env 是 agent 的合法工作）；要阻断用 block，默认 log 只要可见性。
17. **guard 命中永不含匹配内容**：live event、计数器、安全审计日志（seclog）只携带模式类型名/路径类别名，匹配到的秘密字节只允许出现在 redact 后的转发 body 里（被替换为 `[REDACTED]`）。审计日志因此可以安全长期保留。
18. **cache key 基于 redact 后的 body**：guard.secrets=redact 先把秘密替换为 `[REDACTED]`，响应缓存再对改写后的 body 取 key——因此两个仅秘密值不同的请求 redact 后共享同一条缓存条目（语义有意：缓存命中的应答本就不依赖被抹掉的秘密，且避免了把秘密派生进缓存 key）。pin/force-provider 仍按既有红线绕过缓存，不受此影响。
19. **seclog 审计日志 reload 换代、换代瞬间允许丢尾记录**：`guard.audit` 开关与 `audit_path` 变更在 reload 时立即生效（off→on 当场开始写、on→off 当场停写、路径变更换新文件）——旧 logger 在换代时先 drain 再关停，forward 只写请求自己快照里的 logger。换代瞬间在途请求若仍持旧快照 enqueue，旧 logger 已 drain 完，这几条尾记录被静默丢弃（Enqueue 是非阻塞 offer，无人再消费）：审计日志是 best-effort 可见性通道，绝不为持久化阻塞或失败请求路径。
20. **guard 分片检测的会话窗口存 redact 前原文**：`guard.session_scan` 的会话窗口（store 由 `internal/guard/session` 拥有；按 `x-claude-code-session-id`，每会话 32KiB 尾窗、LRU 256、≤8MiB）保存的是 redact 之前的请求 body——存 redact 后形态会让后续分片检测失效（被抹掉的分段永远拼不回来）。这是内存敏感性的有意取舍：窗口可能含凭据，因此只活在小锁保护的进程内存里，永不落盘/日志/序列化/API；有界性（截断即重置分片进度、LRU 整体淘汰）是它的暴露上限。它不进 RuntimeSnapshot（跨代运行时观察态，参照 metricsStore），reload 不清空，避免 reload 抹掉在途会话的分片上下文。会话键是**客户端可控**的 `x-claude-code-session-id` 头：省略或轮转该头的 agent 永远不会进入本通道，256 个不同 id 即可挤空整个 LRU——它是对配合客户端的有界启发式，不是对有意规避者的检测保证。
21. **guard 分片命中 redact 降级为 log、block 只拦补齐段**：分片泄露的秘密横跨多条请求，没有任何单个 body 可以被改写——redact 对 `known_secret_fragmented` 有意降级为 log（event/audit 的 action 记为 log），block 则 400 拒绝补齐段所在的请求；此前的分段已放行，因为它们各自是不含完整秘密的干净请求，单请求扫描无从拦截。检测有两条通道（`internal/forward/forward.go` 同处运行，本条是唯一权威定义）：**① 相邻窗口精确拼接**——只在"拼接体命中而尾窗单独不命中、当前 body 单独不命中"时计 fragmented；一个只存在于拼接体的命中必然跨越接缝，而 known-secret needle 最长为 `Scanner.MaxKnownNeedleLen`，因此实现只扫描接缝区（尾窗末尾 + 当前 body 开头各 MaxKnownNeedleLen-1 字节），判定与整段拼接扫描完全等价，但省掉每请求至多 64MiB+32KiB 的拼接拷贝；尾窗单独命中判定来自会话条目里 Add 时增量维护的缓存（截断或换代失效时回退重扫），不再每请求重扫尾窗。真实流量上本通道接近零命中（能转发的 body 都以 `{` 开头，ExtractModel 要求，两条 JSON 请求的分段在拼接处永远不可能字节相邻），但作为 fail-closed 兜底保留。**② 按序最长前缀进度**（`ScanKnownFragment`，每段 ≥8 字节、只覆盖 known-secret 原文）——覆盖分段各自落在单条请求 JSON 内部的真实拆分，是实际起作用的通道。某秘密跨请求补齐（或单请求出现完整值）时扫描器将其进度**刻意归零**并经 `reset` 返回值声明，存储层合并进度时对恰好这些下标取 0 而非元素级 max——否则陈旧进度会让后续 suffix 分段把同一秘密重复报成 fragmented（回归测试：`TestSessionScan_NoRefireAfterCompletion`）。
22. **apikey 池 keychain 模式不可用时 fail-closed，不回落明文**：`credentials: keychain` 是用户显式的安全选择——后端不可达（headless Linux 无 Secret Service、钥匙串被锁）或元数据账户的 keychain 条目丢失时，`accounts.Store` 的读写直接报错（`credstore.ErrUnavailable`/`ErrNotFound`），而不是悄悄退回读取明文池文件（回落会让"秘密已进钥匙串"的预期在故障时无声失效，且明文文件可能正是用户想摆脱的东西）。代价是 keychain 故障期间该 provider 整体不可用，需要修复后端，或把 `credentials:` 改回 `file` 走条目 25 的回迁。删除语义同样偏向不留孤儿：先删 keychain 条目成功才落元数据文件（删除失败则整个保存报错），宁可保留可重试的元数据，也不留"元数据没了、秘密还挂钥匙串"的孤儿条目。锁定测试：`internal/accounts/store_keychain_test.go`（`TestKeychainUnavailableFailsClosed` 等）。
23. **guard.paths strong 只算工具调用侧；weak（正文/工具结果侧提及）完全忽略（不计数、不 block、不发 live event、不落审计记录）**：敏感路径命中按结构位置分 strong/weak（`guard.ScanPathsContext`）——路径出现在工具**调用侧**（anthropic `tool_use.input`、openai `tool_calls[].function.arguments`、responses `function_call.arguments`）是 strong，即"agent 发起读敏感文件动作"的 MCP Tool Poisoning 特征（用户语义：命令行/工具调用引用了这些路径上的文件，才可能是安全问题）。工具**结果侧**（`tool_result.content`、`role:"tool"` content、`function_call_output.output`）一律 weak：结果文本里出现路径只是文档/源码/报错里的"地址提及"（实测主要误报源：agent 读代理自身 README/源码触发全类别 strong 刷屏），不是访问动作；结果里的真实秘密内容仍由 secret 通道精确匹配兜底。普通正文/user 消息提及同样是 weak——只是"发出去一个地址"，本身不是安全问题。weak 若发 live event 会刷屏监控、若 block 会误伤正常负载、若落审计会让 Security 页被良性"地址提及"淹没（用户实测 95%+ 记录是这类误报，训练操作者忽略审计页）；因此 weak **完全忽略**——不计数（`("guard", <类别>_text)` 计数器已随本决策移除：良性提及的计数同样是噪音）、不发 live event、不 block、不落 seclog 审计记录，观测面只对 strong 生效。body 非合法 JSON 或结构识别失败时一律降级 weak（宁低勿高），绝不因识别失败升级为 strong。历史日志中的 action=`log-weak` 与结果侧 strong 记录是本决策调整前写入的。锁定测试：`internal/guard/pathctx_test.go`、`internal/app/guard_runtime_integration_test.go`（`TestGuardPaths_WeakTextNeverBlocks`、`TestGuardAudit_PersistsSecretAndPathRecords`）。
24. **凭据存储单一开关：`credentials:` 驱动池与 OAuth 两侧，env 只覆盖 OAuth**：收敛前池后端走 config `credentials:`、OAuth blob 走 env `MP_CRED_STORE`（默认 auto 探测），两套开关语义割裂。收敛后优先级为 **env `MP_CRED_STORE` 非空 > config `credentials:` > 默认 file**：config 同时应用到 `accounts` 池后端和 credstore OAuth 模式（config 加载点统一调 `accounts.SetProcessCredentialsMode`）；env 保留为已发布的显式 override，只作用于 OAuth 侧（`auto` 收敛为 opt-in 的探测语义，不再是默认）。env 与 config 不一致是唯一可能的分歧，`config check` 与启动/reload 日志各打一行说明两侧生效值与来源。代价是行为变更：此前未设开关、靠 auto 默认进 keychain 的 OAuth blob，升级后默认按 file 读取，需显式设 `credentials: keychain`（或 env）继续读原条目。锁定测试：`internal/credstore/credstore_test.go`（`TestResolveModePriorityMatrix`、`TestConfigMismatchNote`）、`internal/cli/config/config_cmd_test.go`（`TestCLI_ConfigCheckCredentialsSummary`）。
25. **keychain→file 部分回迁：缺条目或身份不一致时保留元数据；显式 logout 跨模式清理**：`credentials:` 切回 `file` 后，纯元数据池按条目从 keychain 读回秘密；只有全部条目成功、凭据推导出的 canonical ID 与原 metadata/keychain namespace 相同才原子重写 0600 明文池。Volcengine 非 API-only 身份必须同时恢复 AK/SK，缺半不得降级。缺条目、后端不可达或非 canonical ID 的账号进入 secretless `Snapshot.ReloginNeeded`，部分回迁不动原文件；普通 Save 还要求覆盖并 canonical 恢复所有原 metadata ID，防止逐个 login 静默丢记录，放弃账号必须显式 remove。完整回迁写明文前先写 0600、无秘密的 `<pool>.keychain-origin` marker；读路径有意保留 keychain 副本，`Store.RemoveAccount` / `RemoveAllAccounts` 才清理。metadata-only 删除以当前被移除 ID 为最低清理集合，marker 只补充历史 provenance，不能因切回 keychain 后变旧而压制新账号删除；失败保留 pool/marker 供重试，纯 file 无 marker 不碰 keychain。混合池仍按 validate 报错。OAuth blob 不自动回迁，切回 file 需重新 login；但 keychain Save/Load 会写无秘密的 `<authfile>.keychain-origin`（legacy keychain-only 首次 Load 补写），`Ref.Delete` 在当前 keychain 模式或 `.keychain-origin` / `.migrated.bak` 表明曾使用 keychain 时一并删 keychain 条目。删除失败保留 origin marker 以便跨模式重试；纯 file 历史无 marker/档案时永不触碰后端。锁定测试：`internal/accounts/store_restore_test.go`、`internal/credstore/credstore_test.go`。
26. **guard 扫描按次预算：对抗性 body 有界降级为漏报，不再无界耗时或内存**：单次扫描的编码通道解码上限 256 次（`maxDecodesPerScan`）、命中声明上限 262144（`maxMatchesPerScan`）；Aho-Corasick phase-1 在 append occurrence offset 时也立即限额，raw known、encoded known、probe 三层各自最多 262144，低优先级 probe 不能挤占 known-secret。只需回答“是否命中”的 `ScanKnown` 不保存 offset，只留 raw/encoded 两个存在位。对抗性构造（长 token run 内以字节间隔重复探针变体 → 滑动 span 每命中一次 8KiB 解码；重复廉价 probe/秘密形状百万次 → phase-1 position 无界堆积或命中声明两两重叠检查）此前可把单请求推到 100GB 级解码量、O(n²) 比较或与 body 命中数同比的额外内存——guard 默认开启且威胁模型内的恶意 agent 正是本地请求方。预算耗尽后对应通道停止收集，方向是**有界漏报**（宁漏勿滥，与 redact 侧的宁滥勿缺相反）；`claimSet` 有序不交区间集 + 流式正则迭代保证正常负载路径线性。锁定测试：`internal/guard/scanner_budget_test.go`。
27. **keychain 条目超限（ErrEntryTooLarge）时懒迁移保留明文服务，不锁死账号**：OS 钥串后端有物理条目上限（darwin 命令行预算 ~3000 原始字节、windows 凭据 blob 2560 字节；codex OAuth 档案典型 2.6-3.6KB，常超限）。`Ref.Load` 的懒迁移写入被拒时**返回明文数据**：尺寸超限是 blob+后端的确定性属性，不是可用性或降级问题，明文权威副本完好、后续每次 Load 都会重试并落回同处；此前一律 fail-closed 会让进程明明读得到的凭据变得不可读。写入路径（`Save`/`KeychainSet`）按平台预检尺寸、提前返回 `ErrEntryTooLarge`（darwin 超限命令会在进程已启动后才被拒、搁置子进程；错误类别与 `ErrUnavailable` 分开，排障与修复路径不同：前者改回 `credentials: file`，后者修后端）。这与条目 22 的"不可用 fail-closed"不冲突：可用性故障仍原样报错。锁定测试：`internal/credstore/credstore_test.go`（`TestRefLoadOversizedBlobKeepsServingPlaintext`、`TestMapKeyringErrPreservesSizeClass`）。
28. **guard scanner 构建失败：启动降级、换入侧一律拒绝（有意不对称）**：`buildGuardScanner` 的错误只可能来自手工构造的未验证 `Config`（`validate` 在加载时已拒绝非法 `guard.extra_patterns`），三类入口对同一错误取两种语义。启动（`NewProxyWithStatePath`）**降级**：先回退内置规则表 + known secrets（warn 一行），再失败则 `guardScanner = nil` 完全关闭出站 guard——这行用 error 级记录（`logx.Errorf`）：安全控制关闭必须在 `log_level: error` 下也可见，是级别过滤（063b2f9）的有意例外。一个拒绝启动的代理比少了 guard 的代理更糟，且该错误在正常加载路径不可达。所有"已有可用 scanner 兜底"的换入侧一律**fail-closed 拒绝新代**：reload（`Proxy.Reload`）整次拒绝保留旧 generation，OAuth 凭据重同步节拍（`guard_wiring.go`）保留旧 scanner 等下一拍。即：无服务可用时宁降级也要启动，有旧代/旧 scanner 兜底时绝不允许降级换入。
29. **模型级探测的分类口径与 provider 级刻意不同**：模型级分类（`runtimewire.ClassifyModelStatus`）里 401/403/429 判 **unknown**（auth/quota 回答证明路由存在，但说明不了该模型是否被服务——与 provider 级判 yes 相反），unknown 腿下一轮探测 pass 重探；400 携带模型拒绝措辞（"model not found"/"does not exist"/"unsupported model"/"invalid model"/"model is not supported"/"model not supported"/"unknown model"/"no such model"/"not supported with this model"/"not supported by this endpoint"（shopee retcode 40403），大小写不敏感，另加正则 "not supported for <model> in <path>" 对齐 aqp 网关措辞）判 **no**；裸的 "not supported for" 子串 **不**判 no——套餐层拒绝（"Streaming is not supported for this plan"）会误伤，而模型级 no 无 TTL、误伤即锁腿，其余 400 仍判 **yes**（形状争议反证该路由服务此模型——与 provider 级同样的刻意粗粒度，由运行时模型粒度 404 纠正兜底：翻转模型级 responses verdict、不锁模型）。裸的 "model" 词不足以判 no——形状争议也会提到模型（"... with this model"）。锁定测试：`TestClassifyModelStatus`（`internal/runtime/wirecap/store_test.go`）。
30. **anthropic 支持由 config 声明，不在 openai base 上探测或透传**：provider 配置 `anthropic_base_url` 即支持 anthropic（声明 = 事实），未配置即定义上不支持——provider 级探测只有 openai base 的 chat/responses 两条腿，绝不在 openai base 上伪造 anthropic 请求体。旧的「对无 anthropic_base_url 的 provider 在 openai base 上探 `/v1/messages`、网关接受 anthropic 则字节透传」分支已删除：anthropic 客户端打无专用 base 的 provider 现在一律按 verdict **转换**（responses 优先，其次 chat），不再把一个未验证的 anthropic body 字节透传给 openai base。`wire record` 的 anthropic 端点在缺省 `anthropic_base_url` 时仍回落 openai base——那是录制工具的显式探测请求，不是转发决策。

30. **跨协议错误体降级不 fail-closed**：`convertErrorResponse` 对无法识别/无法解析的上游错误体（FastAPI `{"detail":...}`、HTML 错误页、空 body）**不**返回错误把 4xx 升级成 "response conversion failed" 502，而是合成携带截断原文（≤500 字节）的目标协议 envelope——客户端必须能看到上游的诊断原文。同协议（如 responses→responses）仍字节透传。锁定测试：`TestConvertErrorResponseDegradesUnrecognizedBodies`。

31. **无 probed-yes 替代腿时的有意透传**：chat/responses 客户端遇模型级当前腿 no 时优先转 probed-yes 替代腿，其次 **unknown** 替代腿（unknown ≠ dead）；三条腿全 no 时**有意维持原协议透传/等效选择**，让上游错误经错误体降级干净浮现，而不是 proxy 侧凭空合成失败。锁定测试：`TestResolveModel` 的 all-no 用例。

32. **学习-重试家族的 fusion 覆盖范围（有意）**：`developer_role`/`thinking_adaptive` 两条改写型课程只在主 executor 路径生效；Fusion BufferedLeg 未接入（fusion 腿不做方言学习重试，失败即按面板语义处理）。如需接入按 executor 的同款结构补 `buffered_leg.go`。

33. **响应 model 归一化以 calledModel 为唯一客户端可见名（含 `provider/model` 前缀形态）**：只要 target 真实模型与请求里的 calledModel 不同，响应字节的 model 一律改写回 calledModel——客户端（尤其把响应 model 记入 session、resume 时按它寻路的 agent）只能看到自己调用过的名字，上游名永不泄漏。这也覆盖 `provider/model` 前缀调用（响应 echo 完整前缀形态）和显式 claude-* 别名路由。Shadow 只进 request log 不回客户端，刻意不改写。契约与路径清单见 `docs/architecture/protocol-conversion.md` 接线要求。锁定测试：`TestForward_AliasResponseModelNormalizationE2E`（`internal/app`）、`TestExecutorNormalizes*`（`internal/targetexec`）。

34. **`/api/security/explain` 是决策 17 的显式例外边界，seclog 红线本身不变**：安全审计记录仍然永不携带命中内容；explain 端点（UI Security 页行内 analyze）按需从 request_log 拉取该请求的 body、用当前代 guard scanner 重扫定位（`guard.Scanner.Locate` + `admin.Ports.LocateGuardHits` 端口，admin 不 import guard）。例外成立的条件：① 不落盘、不进审计日志，只活在本次响应里；② 走与 `/api/requests/<id>` 相同的 admin 鉴权，派生自 admin 本就可完整读取的数据，不产生第二份持久副本；③ secret 命中段在上下文中掩码为头4+…+尾2（<8 字节全掩），且上下文窗口内任何其它 secret 命中同样掩码（path 命中的上下文可能夹带无关密钥）；path 字面量原文展示（路径非凭据，且"哪个文件"是 path 命中的核心信息）；上下文按 pre/hit/post 预切分返回，不暴露字节偏移（前端按 UTF-16 切片，偏移跨编码不可靠）；④ 当前代 scanner 重扫不到的历史 name 标注 `located:false` 并附解释，不静默为空；⑤ `known_secret_fragmented` 是跨请求检测，单条 body 不可重定位，返回 `cross_request` 状态而非伪造定位。锁定测试：`internal/app/security_explain_test.go`、`internal/admin/read_test.go`（`TestSecurityExplain*`）、`internal/web/handlers_read_test.go`（`TestReadSecurityExplain`）。

35. **path 检测只做精确内容匹配能兜住的类别；`proxy_creds` 类别退役**：敏感路径表只保留"路径本身即高置信凭据位置、且内容不在已知凭据扫描集里"的类别（`~/.ssh`、`~/.aws/credentials`、`~/.gnupg`、`~/.kube/config`、`~/.docker/config.json`、`~/.config/gcloud`、`.env`）。`~/.model-proxy` 曾作为 `proxy_creds` 类别在表内，但它持有的正是代理自身管理的凭据（池 key、OAuth token）——这些值由 known-secret 通道按**原文 + base64/hex/url 编码形态精确匹配**（功能定位本来就是"拿我们管理的密钥/token 的内容去精确匹配"），path 字面量只会在良性引用（开发/配置代理时 config、日志、文档天天出现 `~/.model-proxy`）上额外报警，是精确通道之上的纯启发式噪音。退役后该目录的真实凭据泄露仍由 known-secret 兜底；admin_token 等不在扫描集的文件失去路径信号，是有意接受的取舍。历史审计日志中的 `proxy_creds` 记录由 explain 的静态说明表继续解释。锁定测试：`internal/guard/paths_test.go`（类别表即测试夹具）。

36. **guard AI 二次判定（`guard.adjudicate`）：内容外发的显式 opt-in 例外 + 全链 fail-open + 持久化拦截**：pattern 命中（规则表/自定义/编码通道，非 known-secret 精确通道）与 strong 路径命中在 log 档下改为异步判定——命中片段（±`context_bytes` 上下文，窗口内**其它** secret 命中先按 explain 同款掩码）发给 `guard.adjudicate.model` 指定的模型。例外成立条件：① 默认 off，模型由用户显式指定（可以是本地模型——用户接受把片段发给谁由配置决定）；② 判定调用走 provider 直连（probe.Do 配方），**不进 forward 管线**：判定体必含命中模式文本，走管线会自我递归触发 guard，也会污染 request log/cache/stats；③ 发送的是最小片段而非整个 body；④ 模型回复的 reason 经 scrub（控制字符过滤 + 命中片段回显替换为 `[MASKED]`）与 120 字符限长后才进审计/ring。判定语义：**high**＝真实泄露 → 经典审计记录（新 `verdict` 字段）+ live event + `("guard", <规则名>)` 计数器 + `block_session` 时拉黑会话；**low**＝fixture/示例/文档 → 屏蔽（只留 `adjudicated_low` 计数器与 `/api/security/adjudications` ring，不落审计不发事件——观测面对良性命中保持静默正是本通道的目的）；**error/skipped**（调用失败/超时/队列满/单请求上限 4 个 job 之外的溢出/跨规则同内容去重吞掉的名字）→ **fail-open** 回经典立即记录，绝不因判定器不可用而静音 guard（溢出的判定粒度是名字：审计记录只携带名字，一个名字有任意 span 被判定即覆盖，未被判定到的 span 通过 fail-open 记录兜底）。缓存：verdict 按 sha256(kind+rule+model+hit) LRU 缓存并持久化（`guard_verdicts.json`，只存 hash→verdict，无凭据内容）——会话历史回显 120 次只计费 1 次；error 不缓存。判定通道自带 LLM 用量记账（真实模型调用次数与 in/out token 合计，缓存命中与在途去重不计费；判定调用有意不进 forward 统计），经 `/api/security/adjudications` 的 `stats` 字段与 WebUI Security 页 KPI 呈现。会话拉黑：键为客户端可控的 `x-claude-code-session-id`（决策 20 同一限制：无头不拉黑、可轮转头规避；有界启发式非对抗性保证），持久化 `guard_blocks.json` 跨重启保留，**只**经 `model-proxy guard unblock` / WebUI Security 页 / `DELETE /api/security/blocks/<id>` 显式解除；拦截检查在 guard 扫描之前（拉黑会话零扫描成本），且独立于当前 `guard.adjudicate.enabled`（拦截产生后配置关闭不自动放行）。`secrets/paths=block` 档与 known-secret 精确通道不参与判定（同步终态决策不做异步复核）。锁定测试：`internal/adjudicate/adjudicate_test.go`（high/low/error、缓存、inflight、队列满、scrub、持久化）、`internal/forward/guard_adjudication_test.go`（延后/fail-open/拦截 400/strong 路径延后/上限与跨规则去重溢出 fail-open）、`internal/app/guard_adjudication_integration_test.go`（端到端：low 抑制+缓存、high 拉黑+解除、judge 故障 fail-open）、`internal/cli/guard/guard_cli_test.go`、`internal/web/handlers_read_test.go`（`TestSecurityBlocksAndAdjudications`）。

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
