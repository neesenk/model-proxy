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

## qwen-plan：用量仅控制台、不轮询（有意为之）

千问 Token Plan 个人版的 Credits 用量（5h/7d 窗口）**没有公开 API**（文档「以控制台订阅页用量明细为准」），且平台条款「严禁 API 调用」明确禁止自动化/批量调用（仅允许 Claude Code/Cursor 等交互式工具）。model-proxy 作为交互式开发工具的转发代理，转发本身合规；但**有意不实现**控制台 cookie 抓取或后台配额轮询——`Quota()` 返回 `BillingUnknown`，仅把控制台订阅页 URL（`https://platform.qianwenai.com/home/billing/subscription/token-plan-individual`）附在 CLI `usage` 与 Web UI（`/api/status.quota` → app.js 渲染 `snap.Notes`）里供人工查看。窗口耗尽由 429 `Allocated quota exceeded` 反应式触发冷却与 failover（`failclass.go` 已分类，无需新增代码）。

## 修改要求

- 改变任一决策必须同步更新本文件、实现注释和回归测试。
- 扩大错误字符串匹配范围时必须增加至少一个正例和一个相邻语义负例。
- 新的自动请求改写必须说明作用域、次数上限、never-mutate 字段和可见日志。
