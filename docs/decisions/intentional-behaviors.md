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
11. **wire verdict unknown 时维持透传**：探测未完成或结论为 unknown（超时/5xx）时，协议选择回落到客户端协议透传，与探测功能引入前的行为一致——探测只能"改善"默认行为，不能在 boot 窗口期改变它。锁定测试：`TestWireCap_DecisionMatrix`。
12. **wire 探测把 404 以外的 4xx 视为端点存在**：探测分类里 400/401/403/429 都判 yes——400 是请求形状争议而非路由缺失，401/403/429 更是端点存在的直接证据；只有 404 判 no（proxy 只探测已知 LLM 路径）。误判 yes 的兜底是运行时 404 纠正（翻转 verdict + 跳过模型锁），因此探测本身不做更细的形状校验。
13. **Responses 跨协议 body 解析失败落 502 而非 400**：responses 客户端走跨协议 target 时 body 先经 `responsesState.expand` 做本地历史展开（proxy.go serveOnce）；body 本身是非法 JSON 时展开报错、当前 target 被跳过，所有 target 都失败后走统一的 all-targets-failed 路径返回 502，而不是 400。转发主路径不整体解析客户端 body（同协议字节透传），调度层无法区分「客户端 body 坏」和「单 target 处理失败」，保持 fail-closed 跳过语义。
14. **唯一可转换 target 冷却中时立即 400 而非等待恢复**：本 pass 记录了 `conversionErr` 且 `tried` 为空时（请求转换被 capability scanner 拒绝的 target 不计入 tried；唯一能安全转换的 target 在冷却、未进本轮调度），直接返回 400 `unsupported_protocol_conversion`，跳过 cooldown wait-retry——不等待冷却恢复。重试中的 agent 下一轮自然会再命中已恢复的 target。
15. **guard known-secret 凭据值进内存扫描器**：代理自身管理的凭据（池 key、OAuth token）以内存值形式进入 `guard.Scanner` 做出站精确匹配——不违反"凭据不进 config/代码/日志/测试输出"红线，因为匹配集永不落盘、不序列化、不进事件/DTO（命中只报 `known_secret` 类型名）。OAuth token 进程内轮转（codex/aqp 原地刷新写回 auth 文件）后，后台节拍（`scheduling.quota_poll_interval`，默认 5m）自动重收 OAuth 文件并按当前代重建换入扫描器，最迟一个周期生效；轮转的间隙里旧值仍被扫描（无害，旧值已失效）。重建只用当前代的池秘密基（reload-owned），跨代混用在代检查处丢弃。同一节拍还收集 provider 经 `provider.SecretReporter` 上报的**只存在于内存**的凭据：aqp 的 managed API key 是运行时经 SSO cookie mint 的，任何文件都不存；codex 的内存 access_token 也可能比文件新。这是 provider 凭据值的首次接口级暴露——例外成立的条件与扫描集相同：只读内存、只为出站扫描、永不序列化/日志/落盘（未 mint/未缓存时上报空集）。
16. **guard.paths 不支持 redact**：敏感路径命中只有 log/block/off——redact 会改写 `.env`、`~/.ssh` 等路径文本，破坏正常编码负载（读 .env 是 agent 的合法工作）；要阻断用 block，默认 log 只要可见性。
17. **guard 命中永不含匹配内容**：live event、计数器、安全审计日志（seclog）只携带模式类型名/路径类别名，匹配到的秘密字节只允许出现在 redact 后的转发 body 里（被替换为 `[REDACTED]`）。审计日志因此可以安全长期保留。
18. **cache key 基于 redact 后的 body**：guard.secrets=redact 先把秘密替换为 `[REDACTED]`，响应缓存再对改写后的 body 取 key——因此两个仅秘密值不同的请求 redact 后共享同一条缓存条目（语义有意：缓存命中的应答本就不依赖被抹掉的秘密，且避免了把秘密派生进缓存 key）。pin/force-provider 仍按既有红线绕过缓存，不受此影响。
19. **seclog 审计日志 reload 换代、换代瞬间允许丢尾记录**：`guard.audit` 开关与 `audit_path` 变更在 reload 时立即生效（off→on 当场开始写、on→off 当场停写、路径变更换新文件）——旧 logger 在换代时先 drain 再关停，forward 只写请求自己快照里的 logger。换代瞬间在途请求若仍持旧快照 enqueue，旧 logger 已 drain 完，这几条尾记录被静默丢弃（Enqueue 是非阻塞 offer，无人再消费）：审计日志是 best-effort 可见性通道，绝不为持久化阻塞或失败请求路径。
20. **guard 分片检测的会话窗口存 redact 前原文**：`guard.session_scan` 的会话窗口（按 `x-claude-code-session-id`，每会话 32KiB 尾窗、LRU 256、≤8MiB）保存的是 redact 之前的请求 body——存 redact 后形态会让后续分片检测失效（被抹掉的分段永远拼不回来）。这是内存敏感性的有意取舍：窗口可能含凭据，因此只活在小锁保护的进程内存里，永不落盘/日志/序列化/API；有界性（截断即重置分片进度、LRU 整体淘汰）是它的暴露上限。它不进 RuntimeSnapshot（跨代运行时观察态，参照 metricsStore），reload 不清空，避免 reload 抹掉在途会话的分片上下文。
21. **guard 分片命中 redact 降级为 log、block 只拦补齐段**：分片泄露的秘密横跨多条请求，没有任何单个 body 可以被改写——redact 对 `known_secret_fragmented` 有意降级为 log（event/audit 的 action 记为 log），block 则 400 拒绝补齐段所在的请求；此前的分段已放行，因为它们各自是不含完整秘密的干净请求，单请求扫描无从拦截。检测用「按序最长前缀进度」（每段 ≥8 字节、只覆盖 known-secret 原文）而非窗口拼接精确匹配：能转发的 body 都以 `{` 开头（ExtractModel 要求），两条 JSON 请求的分段在窗口拼接处永远不可能字节相邻，精确拼接匹配在真实流量上必然零命中。
22. **apikey 池 keychain 模式不可用时 fail-closed，不回落明文**：`credentials: keychain` 是用户显式的安全选择——后端不可达（headless Linux 无 Secret Service、钥匙串被锁）或元数据账户的 keychain 条目丢失时，`accounts.Store` 的读写直接报错（`credstore.ErrUnavailable`/`ErrNotFound`），而不是悄悄退回读取明文池文件（回落会让"秘密已进钥匙串"的预期在故障时无声失效，且明文文件可能正是用户想摆脱的东西）。代价是 keychain 故障期间该 provider 整体不可用，需要修复后端或把 `credentials:` 改回 `file` 后重新 login。删除语义同样偏向不留孤儿：先删 keychain 条目成功才落元数据文件（删除失败则整个保存报错），宁可保留可重试的元数据，也不留"元数据没了、秘密还挂钥匙串"的孤儿条目。锁定测试：`internal/accounts/store_keychain_test.go`（`TestKeychainUnavailableFailsClosed` 等）。

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
- `observe/events` 订阅后的短窗口内，同一条事件可能既出现在重放的 recent 快照
  又出现在订阅流里（注册与 recent 复制在同一把锁内完成，publish 无需感知订阅时
  点）。仅影响展示端去重，不丢事件。
- Fusion 面板全部以 429 冷却失败时，终局按 hard 处理返回 502 而非 429+
  Retry-After：fusion 伪 provider 无健康状态，`cooldownState` 判不出 allDown；
  代码注释自认 "opaque → hard"。收紧前先给 fusion 伪 provider 建立限频观测。
- 半开单飞（`halfOpenInFlight`）期间，并发请求把该 provider 视为不可用，若其余
  target 也全灭则立即终局 502，不享有 retry_wait 等待——这是单飞探针语义的固有
  代价（让一个请求独占探测权），与"短冷却等待优于立即报错"的整体取向的偏差是
  已知且接受的。

## 修改要求

- 改变任一决策必须同步更新本文件、实现注释和回归测试。
- 扩大错误字符串匹配范围时必须增加至少一个正例和一个相邻语义负例。
- 新的自动请求改写必须说明作用域、次数上限、never-mutate 字段和可见日志。
