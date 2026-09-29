# 调研：LLM 路由/级联的学术与工业实践对照

日期：2026-09-27。状态：调研结论，已用于指导 `design-auto-routing.md` 的反馈评估闭环设计。

本文回答一个问题：**在 proxy 层做基于能力的模型调度，我们的做法（grades 档位池 + bands 规则 + selector 判定 + latch 升级，选择层与调度层分离）是否与当前文献和工业实践一致？**

结论：**方向正确，形态与主流收敛解同构**；真正的短板在判定器的模型能力信号、响应质量信号和反馈闭环三处。

## 1. 学术框架

系统综述 [Dynamic Model Routing and Cascading for Efficient LLM Inference: A Survey](https://arxiv.org/html/2603.04445v2)（2026）把跨模型路由的设计空间归纳为三个轴：

| 轴 | 取值 |
|---|---|
| 何时决策 | 生成前（pre-generation）/ 生成后（post-generation）/ 多阶段级联 |
| 用什么信号 | 查询特征 / 模型元数据（成本、延迟、领域特长）/ 响应级信号（质量、置信度）/ 在线反馈 |
| 怎么计算 | 启发式规则 / 监督分类器 / bandit 在线学习 / RL 策略 |

综述的核心结论：**生产系统几乎都是组合式的**——启发式规则 + 分类器 + 级联升级多层叠加，而不是单一范式。我们的 `bands（规则）→ selector（判定）→ escalation/latch（升级）→ grades（档位池）` 正是这种组合。

### 代表性方法

- **RouteLLM**（UC Berkeley, ICLR 2025, [arXiv:2406.18665](https://arxiv.org/pdf/2406.18665v1)）：用 Chatbot Arena 人类偏好 + LLM judge 增强标签训练 strong/weak 二档路由器；四种实现里矩阵分解最便宜且效果有竞争力。报告 MT-Bench 上降 85% 成本同时保持 95% GPT-4 性能。
- **FrugalGPT**（2023/2024）：便宜到贵的级联，质量不达标才升级；缺点是升级路径上的重复计算。
- **AutoMix**：小模型先答 + 自验证，不可靠才升级。
- **Arch-Router**（2025）：把路由策略（domain-action 对）放进 1.5B 模型的上下文做选择，改策略不用重训；代价是判定模型本身的延迟/计算开销，文献明确指出在延迟敏感场景偏贵。
- **IRT-Router / ICL-Router / SCOPE**：给路由器喂**模型侧能力画像**（IRT 能力向量、in-context 能力向量、行为指纹），而不是只靠查询特征。
- **bandit 类**（PILOT、MixLLM）：在线反馈更新路由策略。MixLLM 报告 97.25% GPT-4 质量 @ 24.18% 成本。前提是已有可靠的奖励信号和足够样本量。
- **kNN/嵌入检索**（[ORBIT](https://github.com/LAMDA-Model-Reuse/ORBIT) 复现集）：非参数基线在更低样本量下可追平训练路由器。

## 2. 工业实践：NVIDIA NeMo Switchyard

[NVIDIA NeMo Switchyard](https://developer.nvidia.com/blog/route-ai-agent-workloads-across-models-with-nvidia-nemo-switchyard) 的 [escalation router](https://github.com/headroomlabs-ai/headroom-switchyard/blob/main/docs/routing_algorithms/escalation_router_routing.md) 与我们的 escalation/latch 设计逐点重合：

| Switchyard | model-proxy route_policy |
|---|---|
| `confirmations = 2`（连续 2 次升级判决才 latch） | `escalation.consecutive: 2` |
| latch 后跳过判定直达强档 | latch 优先级高于 selector/bands |
| judge 超时/报错 fail-open，不清空 streak | 判定 fail-open |
| 会话级 streak（`x-switchyard-session-id`） | 会话级 latch（按 `x-claude-code-session-id` 索引）+ dwell |

这是独立团队做出的几乎相同的设计，说明该形状是收敛解。一个值得借鉴的差异：

1. **判定对象是"已完成轮次的输出质量"**（卡住/循环/漂移），而不是只看 infra 错误。综述的级联章节同样强调响应级信号信息量远大于纯查询信号。

## 3. 逐层对照我们的设计

| 我们的层 | 对应文献/实践 | 评价 |
|---|---|---|
| bands（follow_up / estimated_tokens 规则） | difficulty-aware + heuristic 档 | 成本为零的第一层，合理；规则准确率天花板低，文献中普遍被轻量分类器取代 |
| selector（jev 生成前判定） | Arch-Router 式 LLM-as-judge | 逐请求 LLM 判定是**最贵的路由器形态**；先 shadow 验证再 enforce 的顺序正确；远期可考虑 BERT/kNN 级廉价分类器 |
| escalation + latch（连续坏信号→锁档，dwell） | Switchyard escalation router | 收敛解，逐点重合（含会话级粒度）；信号维度是差距（见 §4） |
| grades（flash/pro 档位池，选择/调度分离） | RouteLLM strong/weak 及 N 路推广 | 一致：路由决定能力档，调度解决档内健康与成本 |

## 4. 文献指出的三个真实短板

1. **判定器缺模型能力画像**。IRT-Router/ICL-Router/SCOPE/RouteLLM 都给路由器喂模型侧信息（能力向量、行为指纹、成对胜率）。我们的 selector 只有 candidates 的 rubric 文本，同模型跨 provider 无区分度，限制了判定准确率上限。
2. **升级信号仍以 infra 错误为主**。`bad_signals` 现为闭集 `{upstream_error, empty_ok, repeat_turn}`（"空 200 / 重复 turn"已补）；Switchyard 判的是轨迹质量（卡住/循环/漂移），"会话内连续重试"等更丰富的质量信号仍缺。
3. **没有反馈闭环，准确性不可测**。没有对账数据，selector 的准确率无法测量、只能凭感觉调参。bandit 式在线学习（PILOT/MixLLM）是远期方向，但前提是先建立标签体系和足够样本量。

## 5. 对路线图的结论

分层骨架不变（规则→判定→升级→档位池，选择与调度分离）。按文献方向修正的优先级：

1. **先建反馈评估闭环**（测量手段）：判定结果持久化 → 离线弱标签对账 → 影子配对 + judge 强标签。没有它，其余改动都无法验证。
2. **扩充升级信号**到响应质量层。
3. **判定器喂模型能力数据**（模型运行统计、能力画像）。
4. bandit 在线学习**暂缓**：单用户样本量不足，且依赖 1 的标签体系。

详细方案见 `design-auto-routing.md` 的「反馈与评估闭环」章节。

## 参考

- [Dynamic Model Routing and Cascading for Efficient LLM Inference: A Survey](https://arxiv.org/html/2603.04445v2)（2026 综述，设计空间三轴框架）
- [RouteLLM: Learning to Route LLMs with Preference Data](https://arxiv.org/pdf/2406.18665v1)（ICLR 2025）
- [Awesome LLM Routing & Cascading](https://github.com/ymoslem/awesome-llm-routing-cascading)（文献清单）
- [NVIDIA NeMo Switchyard escalation router 文档](https://github.com/headroomlabs-ai/headroom-switchyard/blob/main/docs/routing_algorithms/escalation_router_routing.md)
- [ORBIT 路由方法复现集](https://github.com/LAMDA-Model-Reuse/ORBIT)（kNN/MLP/SVM/MIRT 基线）
- [LLMRouterBench](https://www.emergentmind.com/topics/llmrouterbench)（路由基准，(query, model, quality, cost) 四元组回放）
