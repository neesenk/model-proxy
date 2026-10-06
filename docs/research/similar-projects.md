# 同类开源项目调研与方向建议

> 状态：调研报告（非实现契约）。事实基准：2026-08 第一轮（GitHub 元数据快照，star 数仅用于量级判断）+ 2026-09 第二轮增量（各家官网/README/changelog 一手来源；其中的性能与成本数字多为厂商自述，未经第三方验证）+ 2026-10 第三轮增量（GitHub releases/官方 changelog 一手来源，覆盖 2026-09-01 → 2026-10-05 窗口）。
> 本文档只做竞品分析与方向建议；实现契约仍以 `docs/architecture/` 各专题为准。

## 1. 调研范围

用户指定项目 + 业界热门同类/相邻项目：

| 项目 | Stars | 语言 | 一句话定位 |
|---|---|---:|---|
| [QuantumNous/new-api](https://github.com/QuantumNous/new-api) | ~46k | Go | 自托管 LLM 网关 + AI 资产管理（令牌计费、渠道管理、多租户分发），one-api 的增强分支 |
| [songquanpeng/one-api](https://github.com/songquanpeng/one-api) | ~37k | JS(Go) | 最早的国产 LLM API 分发系统，OpenAI 兼容统一入口 |
| [BerriAI/litellm](https://github.com/BerriAI/litellm) | ~57k | Python(+Rust core) | 企业级 LLM Gateway，100+ provider，预算/限流/guardrails |
| [farion1231/cc-switch](https://github.com/farion1231/cc-switch) | ~129k | Rust(Tauri) | 桌面端 All-in-One 客户端配置切换器（Claude Code/Codex/Gemini CLI 等），近年扩展出代理能力 |
| [musistudio/claude-code-router (CCR)](https://github.com/musistudio/claude-code-router) | ~37k | TypeScript | 编码 agent 的本地模型网关/控制面：路由、failover、凭据池、Fusion 能力扩展、桌面 App |
| [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) | ~49k | Go | 把 Codex/Claude Code/Gemini/Antigravity 等 CLI OAuth 账号池包装成 OpenAI/Gemini/Claude 兼容 API |
| [NVIDIA-NeMo/Switchyard](https://github.com/NVIDIA-NeMo/Switchyard) | ~2.4k | Rust | pre-alpha 的 Rust 代理+库：OpenAI↔Anthropic↔Responses 协议互转、可组合路由算法（LLM classifier / stage router / escalation）、Prometheus 指标 |
| [omnilabs-ai/OmniRouter](https://github.com/omnilabs-ai/OmniRouter) | ~40 | Python | 早期实验项目：统一 API + 动态路由，无生产化迹象 |
| [opensquilla/opensquilla](https://github.com/opensquilla/opensquilla) | ~6.7k | Python | 微内核 token 高效 AI agent，内置本地模型路由器（每轮选最便宜能胜任的模型）+ 记忆/沙箱/搜索 |

### 1.2 第二轮新增调研对象（2026-09）

第一轮覆盖的「编码 agent 本地控制面」之外，第二轮补齐了服务端网关、推理侧路由、MCP 网关、评测平台与安全产品：

| 项目 | 类别 | 本轮关注点 |
|---|---|---|
| [Portkey Gateway](https://github.com/Portkey-AI/gateway)、[Helicone](https://github.com/helicone/helicone) | 网关 + 可观测（开源核 / 全开源） | guardrails 编排化、语义缓存阈值调优、边缘缓存、prompt 管理 |
| [Bifrost](https://github.com/maximhq/bifrost)、[TensorZero](https://github.com/tensorzero/tensorzero) | 高性能网关 / 数据飞轮平台 | WASM 插件、虚拟 key；bandits A/B、自动 prompt 优化 |
| [Cloudflare AI Gateway](https://developers.cloudflare.com/ai-gateway/)、[Kong AI Gateway](https://docs.konghq.com/hub/)、[Envoy AI Gateway](https://aigateway.envoyproxy.io/)、[agentgateway](https://agentgateway.dev/) | 基础设施网关 | 美元预算与自动降级、DLP 复用、MCPRoute、A2A |
| [OpenRouter](https://openrouter.ai/docs/)、[RouteLLM](https://github.com/lm-sys/routellm)、[NotDiamond](https://www.notdiamond.ai/)、[Requesty](https://docs.requesty.ai/)、AWS Bedrock IPR、Azure Model Router | 商业/开源智能路由 | Auto 与 cost_tier、阈值校准、session affinity、合规报告 |
| [vLLM production stack](https://docs.vllm.ai/projects/production-stack/)、[llm-d](https://llm-d.ai/)、[Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) | 推理侧路由 | prefix/KV-cache 感知负载均衡、block hash 索引 |
| [Docker MCP Gateway](https://docs.docker.com/ai/sandboxes/mcp-gateway/)、[IBM ContextForge](https://github.com/IBM/mcp-context-forge)、[MetaMCP](https://github.com/metatool-ai/metamcp)、[MCPHub](https://github.com/samanhappy/mcphub)、[mcpo](https://github.com/open-webui/mcpo)、[mcp-proxy](https://github.com/achetronic/mcp-proxy) | MCP 网关生态 | 沙箱+Cedar 策略、多协议联邦、namespace 聚合、用户级 OAuth、限流 |
| [Langfuse](https://github.com/langfuse/langfuse)、[Arize Phoenix](https://github.com/Arize-ai/phoenix)、[Braintrust](https://www.braintrust.dev/docs) | 评测/可观测平台 | trace→eval 自动生成、在线打分、decision-model judge |
| [Guardrails AI](https://github.com/guardrails-ai/guardrails)、[NeMo Guardrails](https://github.com/NVIDIA/NeMo-Guardrails)、[Invariant](https://github.com/invariantlabs-ai/invariant)、[Pangea](https://pangea.cloud/docs/) | Guardrails 产品 | 五 rail 编排、跨工具调用时序规则、三视角注入检测 |
| [GPTCache](https://github.com/zilliztech/gptcache) | 语义缓存库 | 模块化相似度与失效策略 |

## 2. 功能与实现对比

### 2.1 维度矩阵（✅=成熟支持，🟡=部分/基础，❌=无）

| 维度 | new-api | LiteLLM | CCR | CLIProxyAPI | cc-switch | Switchyard | **model-proxy** |
|---|---|---|---|---|---|---|---|
| 部署形态 | Docker 自托管服务端 | 服务端(企业网关) | 本地进程+桌面App | 本地服务+桌面壳 | 桌面App为主 | 本地二进制/嵌入库 | **单二进制本地 daemon** |
| 目标用户 | 团队/二次分销 | 企业/平台 | 个人编码 agent 用户 | 个人 CLI 重度用户 | 个人 CLI 用户 | 开发者(库)/实验 | **个人/小团队本地** |
| 凭据模型 | 上游 API key 渠道池 | API key + 部分 OAuth | key/订阅导入预设 | CLI OAuth 账号池(核心卖点) | 配置文件级切换 | env 变量单 key | **OAuth/SSO/API key 多账号池 + login 流程** |
| 协议对外 | OpenAI 兼容为主 | OpenAI/原生格式 | Anthropic Messages 为中心 | OpenAI/Gemini/Claude 多协议 | 取决于被切客户端 | OpenAI/Anthropic/Responses 三向转换 | **Anthropic/OpenAI 双协议 + 三协议可选转换** |
| 路由调度 | 权重/优先级 | fallback/权重/tag | 规则路由+failover | 轮询账号池 | 无(纯切换) | random/LLM-classifier/stage/escalation 算法 | **surplus 调度+熔断+粘性+请求感知(图片/工具/上下文)** |
| 用量观测 | 计费/额度面板 | 成本追踪最全 | 日志/延迟/token/成本 | 基础日志 | 极简 | Prometheus 指标 | **Web UI+SSE live+TTFT/LAT+budget 告警** |
| 评测工具 | ❌ | ❌ | Fusion 对比 | ❌ | ❌ | A/B split | **Shadow 影子评测+replay+测活**（差异化） |
| 安全 | token 鉴权+分组(有历史 CVE) | guardrails/PII/redact(强) | 弱(本地可信假设) | 弱 | 中 | ❌(pre-alpha) | **guard 秘密扫描(log/redact/block)、强制 loopback、凭据不落 config** |
| 易用性 | Web 控制台 | Python SDK+代理配置 | 桌面App+UI 向导 | 桌面壳(EasyCLIProxyAPI) | 桌面App体验标杆 | cargo 安装+dry-run | **CLI 向导+takeover+Web UI** |
| 可编程性 | 插件少 | SDK/callbacks 最强 | MCP/ToolHub 扩展 | 插件有限 | ❌ | Rust 库可嵌入 | ❌（暂无插件/SDK 面） |

### 2.2 关键实现差异（值得借鉴的点）

1. **new-api / one-api 系**：核心是"渠道(channel)+令牌(token)"两级抽象——上游凭据聚合成渠道，再对下游发放独立 token 并记账。适合多人分发，但安全债明显（公开部署实例频繁出现未授权访问、key 泄露事件）；其"按 token 计费"模型与我们"订阅/OAuth 配额感知"模型是两个生态位。
2. **LiteLLM**：把安全做成产品线——guardrails、PII redaction、secret manager 集成、预算硬限制。证明"安全特性是企业付费点"，也验证了 DLP-lite 方向的价值。
3. **CCR / cc-switch / CLIProxyAPI**（当前增长最快的赛道）：全部押注"编码 agent 本地控制面"。共性打法：
   - 一个稳定本地端点 + takeover/接管客户端配置（我们已有，且更深入）；
   - **桌面 App 化**（Tauri/Electron 壳 + 托盘 + 图形向导），这是它们 star 增速远超纯 CLI 项目的最大原因；
   - provider 预设一键导入（Kimi/K3、Zhipu、DeepSeek 等官方赞助合作，内置 preset）。
4. **Switchyard**（NVIDIA NeMo 出品）：信号在两点上与我们同向——①协议三向转换作为一等公民；②路由算法可组合、类型化（stage_router 不需要额外模型调用，靠对话内信号如工具错误决定升降档）。它的 escalation router（弱模型先答→judge 决定是否升级强模型）是 fusion 之外的另一条"效果-成本"路径。
5. **OpenSquilla**：代表"harness-native routing"叙事——agent 每轮流量本身成为路由决策的数据飞轮；其技术报告宣称多模型 ensemble 路由超越单一最强模型，与我们的 fusion 思路互相印证。

### 2.3 第二轮增量要点（2026-09）

1. **缓存换轨：语义缓存是标配，增量在 prompt-cache/KV-cache 亲和**。Portkey 的语义缓存不开放阈值配置（内部按高置信度判定），官方建议从 0.95 附近起步、以真实流量回测校准；其博客引用 AWS 实测：阈值 0.75–0.99 区间成本节省 15.8%–86.3%、延迟降 88%（[阈值调优](https://portkey.ai/blog/semantic-caching-thresholds)）；LiteLLM 支持 Redis/Qdrant/Valkey 多后端与 `end_user` 作用域隔离，但文档明确警告 agentic 多轮流量会 stale replay（原文：任何实际阈值下每一轮都会命中上一轮的条目、客户端重放过期响应，典型表现是 agent 反复重复同一个工具调用；[semantic caching](https://docs.litellm.ai/docs/proxy/caching_semantic)）——与本仓库对 `cache` 的定位（多轮会话命中≈0，见 `config.yaml` 的 cache 注释）同向。协议级亲和更硬核：LiteLLM 自动为 Anthropic 注入 `cache_control`、为 GPT-5.6+ 转 `prompt_cache_breakpoint`（客户端无感），并用 session/deployment affinity 保护命中（[prompt caching](https://docs.litellm.ai/docs/tutorials/prompt_caching)）；`encrypted_content_affinity` 针对 Responses API 的 `rs_*` reasoning 项只有原 deployment key 能解密这一点，预检查后把后续请求粘回原 deployment（[Responses API session continuity](https://docs.litellm.ai/docs/response_api#load-balancing-with-session-continuity)）。推理侧 vLLM/llm-d 用 ZMQ 订阅 KV 事件 + block hash 全局索引，缓存被逐出时按真实 hit rate 重选而非盲目 sticky（[llm-d](https://llm-d.ai/docs/well-lit-paths/foundations/precise-prefix-cache-routing)）。

2. **智能路由从"事前预测难度"转向"做完再判"**。Switchyard escalation router：每轮先让弱模型答，judge 评判该轮实际产出，连续命中才 latch 到强模型（[escalation](https://github.com/NVIDIA-NeMo/Switchyard/blob/main/docs/routing_algorithms/escalation_router_routing.md)）；stage router 按工具结果历史（错误/探索/写入/测试）选档；composite 用 LLM classifier 供默认 tier 并支持 session 保持。商业侧：OpenRouter Auto Router 按社区任务花费份额选模型 + `cost_tier` 五档（[auto router](https://openrouter.ai/docs/guides/routing/routers/auto-router)）、Azure Model Router 三模式 + session affinity（[model router](https://learn.microsoft.com/en-us/azure/foundry/openai/concepts/model-router)）、RouteLLM 阈值校准（[repo](https://github.com/lm-sys/routellm)）。这是 fusion selector 之外的另一条解法：不引入分类模型，用已完成的工作判档。

3. **Guardrails 从"过滤器"变成"编排器"**。Portkey 命中可触发 fallback/retry，并把命中样本攒成 eval 数据集（[guardrails](https://docs.portkey.ai/docs/product/guardrails)）；NeMo Guardrails 用 Colang 状态机覆盖 input/dialog/retrieval/execution/output 五个 rail，IORails 并行校验以隐藏延迟、`stream_first` 显式权衡首字节与安全检查（[repo](https://github.com/NVIDIA/NeMo-Guardrails)）；Invariant 的规则作用在完整 agent trace 上，可表达"工具 A 返回被注入内容 → 禁止调用工具 B"的跨调用时序约束（[repo](https://github.com/invariantlabs-ai/invariant)）；Pangea 提供 prompt injection 三视角检测（用户输入 / 用户-system 对齐 / assistant-system 对齐，[prompt guard](https://pangea.cloud/docs/prompt-guard)）；LiteLLM 新增 `pre_mcp_call`/`during_mcp_call` 扫 MCP 工具参数（[MCP guardrails](https://docs.litellm.ai/docs/mcp_guardrail)）。反面信号：llm-guard 已归档停维（[repo](https://github.com/laiyer-ai/llm-guard)），自维护规则表比依赖停更分类器可靠。

4. **MCP 网关生态补全了工程细节**。Docker MCP Gateway：sandbox 隔离 + Cedar 治理策略 + 宿主侧统一 OAuth token/secret + SSRF 检查 + 热加载（[docs](https://docs.docker.com/ai/sandboxes/mcp-gateway/)）；IBM ContextForge：多协议联邦（HTTP/JSON-RPC/WS/SSE/stdio/Streamable）并能把 REST/gRPC 虚拟成 MCP server，用户级 OAuth（PKCE、token 加密隔离、自动刷新）（[repo](https://github.com/IBM/mcp-context-forge)）；MetaMCP：namespace 聚合 + 工具改名/描述覆盖 + middleware，给上游预分配 idle session 降冷启动 + endpoint/用户级 token bucket（[repo](https://github.com/metatool-ai/metamcp)）；MCPHub 用向量语义搜索做 tool discovery 并按用户隔离凭据（[repo](https://github.com/samanhappy/mcphub)）；mcpo 把 MCP 转成带 OpenAPI/Swagger 的 REST（[repo](https://github.com/open-webui/mcpo)）。Envoy MCPRoute（toolSelector + CEL+JWT 授权）与 agentgateway（原生 A2A + MCP）说明这层正被基础设施厂商收编。

5. **治理升维：A2A、虚拟 key 生命周期、美元预算**。A2A 成为新数据面：agentgateway 原生支持，LiteLLM 把 A2A agent 与 MCP server 挂同一入口，Portkey Agent Gateway 用 CRUD 管 agent integration/skills 并配 RBAC（[Portkey changelog](https://docs.portkey.ai/docs/changelog/2026/april)）。虚拟 key 生命周期：LiteLLM `auto_rotate` + `grace_period` + 单点 `custom_key_policy` hook（[virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys)）。按美元治理：Cloudflare spend limits 超支后自动降级到便宜模型（[spend limits](https://developers.cloudflare.com/ai-gateway/features/spend-limits/)）；OpenRouter workspace guardrails 把预算/ZDR/模型白名单/注入规则/PII 收在一处（[guardrails](https://openrouter.ai/blog/announcements/guardrails/)）。

6. **评测从"平台"走向"自动闭环"**。TensorZero 把推理+反馈落自有 DB → SFT/RLHF/GEPA 自动 prompt 工程/dynamic ICL/best-of-N，bandits 自适应 A/B，2026 的 Autopilot 自动分析观测数据、建 eval、优化 prompt 再跑 A/B（[repo](https://github.com/tensorzero/tensorzero)）；Braintrust Loop 从生产 trace 自动生成数据集与 scorer，在线打分支持 trace/span/group 三种范围 + 采样 + rewind（[score-online](https://www.braintrust.dev/docs/evaluate/score-online)）；Langfuse 2026-09 上线用 TypeSafe Jev 做 judge（类型化问题 + 概率答案 + 校准置信度，[changelog](https://langfuse.com/changelog/2026-09-22-jev-as-a-judge)）；Arize Phoenix 提供 OpenAI 兼容代理 + `/mcp` 端点让编码 agent 直接查 trace（[repo](https://github.com/Arize-ai/phoenix)）。

### 2.4 第三轮增量要点（2026-10）

窗口 2026-09-01 → 2026-10-05，四路调研（企业网关 / 编码 agent 控制面 / MCP 与基础设施 / 评测与安全），全部一手来源核实。

1. **会话粘滞×熔断的交互语义成为代理层新竞争面**。Bifrost 9 月连发 v2.2.2–2.2.4 专修这块：粘滞绑定在 (provider, key/model) 组合上而非仅 provider；provider 级会话粘滞直接读 Claude Code/Codex/OpenCode 自带 session header（`x-bf-session-id`），只重排当前路由链、**绝不复活已因失败/冷却被排除的账号**，被判失败的绑定主动丢弃；被静默跳过的 fallback 写入 routing log；fallback 引用从 `provider/model` 字符串改为路由时刻解析（修重启/重载后失效）（[releases](https://github.com/maximhq/bifrost/releases)）。两个值得直接借鉴的发现：①Claude Code 的 server-side thread 绑定上游账号，多 key 轮询下无法存活——对这类会话要么粘死同一账号，要么像 Bifrost 一样返回可识别错误（`thread_unsupported_request`）让客户端整轮重发，不要静默失败；②LiteLLM v1.103 在 fallback 目标上复核预算（[release notes](https://docs.litellm.ai/release_notes/)）——failover 选下一账号时必须复核其配额状态，避免打进已耗尽账号。

2. **takeover 写入被做成"引擎"**。cc-switch 4.0.0（10/4）的关键字段写入引擎：只替换 endpoint/凭据/模型/协议字段，其余 key、注释、顺序逐字节保留；staging + 回读比对 + rename；`live-state.json` 记录操作意图、崩溃后恢复；首写字节级备份；live 文件 0600（[CHANGELOG](https://github.com/farion1231/cc-switch/blob/main/CHANGELOG.md)）。同版的 Aggregation（Stack）模式把多 provider 模型以 `ccs-<key>/<model>` 前缀并进同一客户端选择器，且带前缀请求明确定义为绕过路由/熔断——"聚合 ≠ 路由"的取舍可参考。Codex 兼容新坑（cc-switch 3.20.x/4.0 持续跟进）：`auth.json` 不再作为第三方凭据载体、官方路由不能换 `model_provider`（否则会话历史分桶丢失）。另有一条协议转换硬教训：会话中途注入的 system 消息必须原位保留，重排会让上游 radix cache 命中率从 99% 崩到 20%（cc-switch #6941）。

3. **MCP 2026-07-28 无状态 spec 进入落地期**。mcphub 双栈迁移最激进：`X-MCPHub-State-Id` 显式状态句柄（客户端 UUIDv4，绑定凭据+用户+路由，空闲 30 分钟过期，多副本需 sticky）、`ttlMs`/`cacheScope` 保守缓存（TTL = 上游 discovery 有效期 ∩ 网关 5s 预算，不确定则 0）、按每请求 clientCapabilities 门控 MCP Apps、legacy HTTP+SSE 进入 12 个月弃用窗口（[releases](https://github.com/samanhappy/mcphub/releases)）；ContextForge v1.0.11 迁 MCP SDK 2.x 且现代协议协商**默认 legacy、显式开关切 auto**；agentgateway v1.5 用 CEL 派生 session key 做跨副本确定性亲和。安全教训（回归清单级）：mcphub v1.0.37 CRITICAL——禁用工具只在 tools/list 过滤、tools/call 仍可执行（可被注入绕过），**allowlist 必须在执行路径生效**；Docker/agentgateway 的远程 URL 默认 SSRF fail-closed + 显式逃生阀。Cloudflare Workers OAuth Provider v1（10/1）完整支持 MCP 2026-07-28 authorization spec（CIMD、RFC 9728、step-up）。

4. **结构化判定（decision-model as judge）是本月最强跨项目信号**。Langfuse（09-22）、Braintrust JS SDK（09-17）、Phoenix evals 2.6.0（09-22）、GPTCache（09-20，未发版）四家同月接入 TypeSafe Jev：固定选项输出 + 置信度 + 各选项概率，无自由文本。GPTCache 的 JevEvaluation 用它做语义缓存命中判定：任务同一性/上下文/格式/新鲜度/缺失上下文五维检查取最小分、全过阈（0.70）才复用。配套动作：Langfuse evaluator 历史回填（09-07，新 judge 可追溯打历史 observation）与 evaluator 告警（09-02）；Braintrust span customizers 上传前脱敏、customizer 失败则整批扣住（fail-closed）；Phoenix 新增 DECISION span kind（10-01）与 trace 树内评测结果（09-30）；NeMo Guardrails v0.24 的 `/v1/checks`（单独跑 rail、不产生新响应）与 rail manifest 声明式能力描述。

5. **fail-closed 卫生成为标配**。LiteLLM v1.104（10/3）：master key 未设置/为空/公开默认值时拒绝启动；guardrail 逐 choice 扫描流式工具调用参数（`delta.partial_json`）；spend log 剥离请求体凭据；泄露密码检测（[release](https://github.com/BerriAI/litellm/releases/tag/v1.104.0)）。Bifrost v2.2.5（10/2）：首启动 Setup Token（新装实例不再对全网开放）。new-api rc.32 的模型修饰符协议 `model@thinking:on|off|adaptive|<budget>`、`@effort:`、`@temperature:`——按请求覆盖模型参数，右侧优先、未知键 400，但后缀解析限定已知模型家族（保护 `kimi-k2-thinking` 这类真实 ID）；rc.38 的请求策略（请求检查/会话与重试/渠道健康）并在使用日志留策略决策记录。

6. **生态集中化加速，本地细分定位反而更稳**。Helicone 被 Mintlify 收购后进入维护模式（[官方博客](https://www.helicone.ai/blog/joining-mintlify)）；TensorZero 已归档停维（2026-06）；Portkey 并入 Palo Alto（Prisma AIRS）；Invariant 开源部分停更（归 Snyk）；MetaMCP 半年无活动；Envoy AI Gateway 改名 Agent Router 转入 Agentic AI Foundation（[公告](https://theagentrouter.ai/blog/envoy-ai-gateway-is-now-agent-router/)）；LiteLLM 宣布迁 Rust——头部网关都在向高性能原生/单二进制靠拢，与我们的 Go 单二进制路线同向。同赛道对手仍高频迭代：CLIProxyAPI 9 月 40+ 个 release（v8 配置 API、OAuth 渠道按渠道模型覆盖、auth 终态未授权保持不重试、UUIDv7 request ID、管理面暴露凭据冷却快照）、CCR v3.1.x（profile 凭据池、聚合错误带每次尝试详情）、cc-switch 3.20.x–4.0。

7. **路由决策可解释性与基准化**。Switchyard v0.3.0（09-22）：Advisor Gate（执行模型 + 更强 advisor 做 review/approve/redo，带会话审阅预算与停滞检查点）、composite routing（LLM 分类器 + stage router）、decision endpoint（只路由不执行）、路由结果带 `selected_model_ids` + 决策元数据 + OTel evidence（[release](https://github.com/NVIDIA-NeMo/Switchyard/releases/tag/v0.3.0)）。OpenRouter 发布 Model Router Benchmarks（10-02）：横向评测 7 个 router，提出 Router Index（0–10，默认质量 60% / 耗时 20% / 成本 20%，权重可调），并明确指出路由器的固有难点——**切模型要重建 input cache**、prompt 信息不足以判断复杂度（[blog](https://openrouter.ai/blog/announcements/model-router-benchmarks/)）。

## 3. 业界动向总结

1. **赛道分化完成**：「服务端网关」（new-api/LiteLLM/Portkey/Higress，卷计费与企业治理）vs「编码 agent 本地控制面」（CCR/cc-switch/CLIProxyAPI，卷易用性与 agent 覆盖）。我们在后者的深水区（配额感知调度、订阅账号池、评测），前者不做也不该做。
2. **Anthropic Messages 正在成为 agent 侧通用语**：各家都以 Claude Code 兼容为默认入口，同时 OpenAI Responses 格式渗透。协议转换从"加分项"变为"标配"（Switchyard 整个项目就建立在这上面）。我们三协议转换方向正确。
3. **订阅/OAuth 账号池化成为刚需**：CLIProxyAPI 48k star 的核心就是"把订阅当 API 用"；上游厂商（Codex/Claude/Qwen Plan/Kimi Code）订阅制普及让"多账号池+配额感知"从 hack 变成产品能力。这是我们已有的最强差异化之一。
4. **路由智能化**：静态规则 → 请求感知 → 信号驱动/分类器驱动（Switchyard stage_router、OpenSquilla on-device router）。趋势是"用便宜的信号决定贵模型的调用时机"。
5. **易用性竞争 = 桌面化 + 向导化**：纯 CLI 在个人用户市场增长见顶，头部项目全在做图形界面、托盘常驻、一键导入 preset、doctor 类自检。
6. **安全成为分水岭**：公开部署的 one-api/new-api 实例安全事故频发；LiteLLM 把 guardrails 商业化。个人本地场景的安全焦点转向：凭据存储加密、秘密外泄扫描、审计。
7. **上游生态反向收编**：中转商大量赞助这些开源项目换取曝光，部分项目开始内置商业 preset——说明这个入口位置有真实商业价值。
8. **缓存叙事换轨（2026-09 补充）**：语义缓存成为网关标配，但在 agent 多轮场景被自家文档劝退；增量转向 prompt-cache/KV-cache 亲和与协议级粘性（Responses 加密 reasoning 项、Anthropic `cache_control` 自动注入）。
9. **治理面从「模型 + 密钥」扩到「agent + 工具」（2026-09 补充）**：A2A 数据面、MCP 网关的沙箱/策略/用户级 OAuth、工具调用级 guardrail 成为新竞争面。
10. **评测走向自动闭环（2026-09 补充）**：trace → 自动数据集/scorer → 自动优化 prompt 与模型；评测平台与网关的边界在模糊（Phoenix、Braintrust 都推出了自己的网关）。
11. **粘滞×熔断×配额的交互语义成为代理层新竞争面（2026-10 补充）**：Bifrost 三个版本连修粘滞（绑定粒度、失败绑定不复活、跳过进日志、路由时刻解析），说明"会话粘滞 + 账号池 + 熔断"的组合语义是各家共同的痛点；server-side 会话（Claude Code thread、Responses 加密 reasoning）正在把粘滞从优化变成正确性问题。
12. **MCP 进入无状态化迁移期（2026-10 补充）**：2026-07-28 spec 删除协议级会话，生态用显式状态句柄 + 保守缓存 TTL + 每请求能力协商承接；legacy SSE 进入 12 个月弃用窗口。迁移期默认 legacy、显式切新的双栈姿势（ContextForge）是最稳路径。
13. **评测判定结构化 + 独立网关集中化（2026-10 补充）**：decision-model as judge（Jev）同月被 Langfuse/Braintrust/Phoenix/GPTCache 四家采用，结构化判定取代自由文本 judge 成趋势；Helicone/TensorZero/Portkey/Invariant 相继被收购或停维，独立通用网关赛道集中化，本地优先 + 订阅账号池的细分定位验证更稳。

## 4. 我们的定位（不变）

**本地优先的单二进制多 Provider LLM 代理**：面向个人开发者与小团队，以「订阅/OAuth 账号池 + 配额感知调度」为核心能力，「易用性 + 安全性」为长期护城河。不做多人分发/计费平台（new-api 生境），不做企业网关全家桶（LiteLLM 生境）。

对照调研，我们的既有优势：请求感知路由深度（图片/工具/上下文过滤、溢出改道）、shadow/replay 评测（全场唯一）、fusion、guard DLP-lite、强制 loopback 默认安全、takeover 深度。
主要短板：无图形化安装/托盘体验、凭据明文存储、`/api/*` 与 Web UI 无鉴权（靠 loopback 单点保障）、provider 接入依赖手写 YAML、无可观测导出面。

## 5. 下一步建议（收益 + 解决方案）

> **落地进展（2026-08 第二轮）**：S1 已实现（含 volcengine 接入与 doctor/status 可见性）；S2 已实现（`internal/webauth` + validate 门槛 + 双面鉴权）；S3 已实现（CLI + Web 向导 + FetchModels 交集校验）；S4 已实现 kimi（Gemini CLI 因代理不支持 Gemini wire 协议暂缓）；S5′ 已并入 doctor；S6 被 guard 审计提交超越；S7 已实现（`GET /metrics`）。细节见各设计文档的「实现记录」。
>
> **落地进展（2026-09 第三轮）**：S1–S7 状态不变；§2.3 第二轮增量对应的落地候选为 S8–S12（均为建议，未实现）。
>
> **落地进展（2026-10 第三轮增量调研）**：S1–S12 状态不变；§2.4 第三轮增量对应的落地候选为 S13–S16。**S14 已部分落地**（同日）：交互矩阵契约 + 行为回归（`internal/runtime` sticky×熔断/限频/模型锁用例）+ 部分链 `skipped` 可观测（`/api/status.schedule` + Web UI），权威契约见 `docs/architecture/routing-and-failure.md`「粘滞×可用性×配额 交互矩阵」；server-side 会话亲和（`rs_*`/thread）随 S8 继续。**S13 已落地**（次日）：op journal 意图日志 + 中断收敛 warning、写后回读校验（复用 drift 探针）、Codex 兼容不变量回归，契约见 `docs/client-takeover.md`「机制契约」。S15/S16 未实现。

定位不变，按「安全性 → 易用性 → 生态位」排序。

### S1. 凭据存储加密（安全性 P0）

- **现状**：凭据明文 JSON 存于 `~/.model-proxy/*_auth.json`，任何同用户进程/备份同步/误提交都能带走 OAuth refresh token。
- **方案**：新增凭据存储层 seam——macOS Keychain / Linux libsecret / Windows Credential Manager（go-keyring），不可用时回退到 `0600` 明文并启动告警；迁移策略：首次读到旧明文文件即写入 keychain 并删除原文件（保留 `.bak` 一次）。`login`/`runtime` 只经此层读写。
- **收益**：把最大的单点安全风险消掉，形成对 CLIProxyAPI/cc-switch（均明文或半明文）的可宣传安全差异；备份/同步场景不再泄密。

### S2. 可选鉴权层，解锁 LAN/团队部署（安全性+易用性 P1）

- **现状**：validate 强制 loopback 是因为 `/api/*` 无鉴权——这同时封死了"小团队共享一台代理"这一自然需求（目前只能每台机器各跑一套、各自 login）。
- **方案**：保持**默认不变**（loopback + 无鉴权，secure-by-default 不动）；新增 `web.auth: {token_file|api_keys}` 显式开启后才允许非回环 listen。转发端点与 `/api/*` 分开鉴权：下游客户端发 key（复用现有双写 header 语义），管理面单独 admin token（首启生成打印到终端）。红线不受影响：pin/force-provider、cache 绕过等语义原样。
- **收益**：打开"家庭实验室/小团队自托管"增量用户群，且是竞品（CLIProxyAPI/CCR 安全薄弱）最容易被打的差异点；默认姿势零变化，老用户无感。

### S3. Provider 接入向导化 + preset 化（易用性 P0/P1）

- **现状**：接新 provider 要手写 YAML（base_url/models/routes），认知负担是上手流失主因；cc-switch/CCR 全部用"preset 一键导入"解决。
- **方案**：内置常见 provider preset 目录（zhipu/deepseek/volcengine/qwen/kimi/openrouter…，只含公开 endpoint 与默认模型表，不含任何凭据）；`model-proxy add <name>` 交互向导：选 preset → login → 自动生成 routes（含隐式路由歧义消解提示）→ 测活 → 可选立即 takeover。Web UI 加同等向导页。config init 已有交互向导，此为同一模式的延伸。
- **收益**：新 provider 接入从"读文档写 YAML"降到"一条命令"，直接对标 cc-switch 的上手体验；preset 数据结构天然支持社区贡献（后续生态杠杆）。

### S4. Agent 覆盖扩张：takeover 支持更多客户端（易用性 P1）

- **现状**：takeover 支持 claude/opencode/codex/pi；竞品已覆盖 Gemini CLI、Grok CLI、Kimi CLI、Kilo Code 等十余个。
- **方案**：按 takeover 现有抽象逐个增加客户端适配器（每个只需读写对应配置文件+指向本地端点）；优先 Gemini CLI 与 Kimi CLI（用户基数大、配置格式简单）。
- **收益**："换/加一个 agent 就要重新折腾配置"的痛点被持续收割；README 特性表直接对标 CCR 的 supported-agents 列表。

### S5. 配置体检：`doctor` 子命令 + dry-run（易用性 P1）

- **现状**：配置错误多在运行期才暴露；Switchyard 的 `--dry-run` validate 是好实践。
- **方案**：`model-proxy doctor`：校验 config（含 route 可达性静态检查、别名路由歧义——claude_mapping 已废弃、claude-* 别名为显式 routes 且对全协议生效、models.dev 匹配告警汇总）、凭据完整性、端口占用、daemon 健康、takeover 目标漂移检测；`serve --dry-run` 复用同一校验器输出后退出。Web UI Status 页展示同样的检查结果。
- **收益**：降低 issue 排查成本（用户自助率上升），也是"易用性"最便宜的高感知改进。

### S6. Guard 能力产品化（安全性 P2）

- **现状**：guard 已有 log/redact/block 三态与固定高置信模式，但模式集封闭、结果只有 live event。
- **方案**：①自定义正则模式（config 内声明，含测试样例校验）；②guard 命中写入 stats（按模式计数入 SQLite，UI 展示趋势）；③新增 `audit` 模式：命中时记录脱敏摘要（仅模式名+长度+哈希前缀，绝不落原文）到独立审计文件。红线遵守：敏感数据不入日志。
- **收益**：从"个人 DLP-lite"升级为可演示的安全能力矩阵，向 S2 解锁的团队场景延伸价值；成本极低（复用现有 guard owner）。

### S7. 可观测导出：Prometheus `/metrics` + OTel traces（生态 P2）

- **现状**：指标只在自有 UI 内可见。
- **方案**：`metrics.prometheus.enabled` 暴露 `/metrics`（loopback 默认，随 S2 鉴权策略）；请求级 trace 后续接 OTel。注意锁序红线：指标读取不得触碰 Proxy.mu 持有区，用原子计数器聚合。
- **收益**：接入用户既有监控栈（Switchyard/LiteLLM 标配），提升"专业感"与团队场景留存。

### S8. prompt-cache 亲和路由（成本 P1）

- **现状**：`sticky_dwell` + 会话粘滞保证同一会话落在同一账号，但选目标时不看上游 prompt cache 状态；也不做 `cache_control` 之类的协议级注入。
- **方案**：①Responses 协议：请求体携带 `rs_*` encrypted reasoning 项时，把产生它的 provider/账号记为该会话的粘滞目标（预检查后强制粘回，避免 reasoning 解密失败）；②anthropic 目标可选自动注入 `cache_control`（默认关，避免改变计费语义）；③surplus 排序加"本会话缓存热度"权重（可选）。
- **收益**：多轮 agent 会话降低上游输入 token 计费；对齐 LiteLLM 已实证方向（见 §2.3-1，厂商自述 37–69% 额外节省）。
- **边界**：不引入语义缓存（见「暂不建议做的」）。

### S9. Guard 工具调用时序规则（安全性 P1）

- **现状**：guard 扫单请求 body（+ 跨请求分片检测）；`guard.mcp_secrets` 默认 off；没有工具调用序列层面的规则。
- **方案**：①把 MCP tools/call 参数扫描（已实现但默认关）产品化：文档/UI 引导 + 命中进 Security 页；②会话内工具调用轨迹规则：工具结果命中注入/秘密特征后，对该会话后续敏感工具调用施加 log/block，规则表与现有 guard 表同源。
- **收益**：把 DLP-lite 从"秘密外泄"扩到"agent 行为约束"，对标 Invariant 的跨调用时序规则；复用现有会话身份与审计面。

### S10. MCP 网关：上游 OAuth 与端点限流（生态 P2）

- **现状**：MCP server 鉴权只有 provider 凭据池 / 静态头 env 间接 / `auth: none` 三种；无端点级限流、无 idle session 预热。
- **方案**：①上游 OAuth（authorization code + PKCE）支持与 token 加密存储/自动刷新；②`/mcp/<name>` 端点级 token bucket（防单客户端打爆 stdio 子进程）；③可选 idle session 预热。
- **收益**：覆盖只支持 OAuth 的 MCP server（当前无法接入）；多客户端共用时更稳。

### S11. 评测数据飞轮：request_log → 自动 eval（生态 P2）

- **现状**：shadow 影子评测 + replay 已能配对与重答，但评测集需要人工挑选。
- **方案**：按 `turn_key`/session 从 request_log 索引采样自动生成评测集；复用 adjudicate 的调度 seam 跑 judge 打分；报表复用 Analytics 页。
- **收益**：把一次性 shadow 对比升级为持续回归；不引入外部评测平台。

### S12. 预算动作化：越线自动降级（成本 P3，可选）

- **现状**：`budgets`（默认关）只发 live event/webhook，不改变路由。
- **方案**：越线时对指定 scope 生效 route 覆盖（走便宜目标，保留 failover；不用 pin——pin 是硬选择且绕过 cache），月内有效、下月自动解除，显式 opt-in。
- **收益**：对标 Cloudflare spend limits 的自动降级；与既有 route override 机制天然一致。

### S13. takeover 写入原子性强化 + Codex 兼容跟进（易用性/安全性 P1）

- **现状**：takeover 模板已做关键字段写入（JSON dotted path / TOML 行编辑 / YAML 注释保留节点手术）与 `.bak` 备份、restore、漂移检测（见 `docs/client-takeover.md`）；但写入路径没有 staging + 回读比对的原子化流程，崩溃恢复依赖手工 restore。
- **方案**：①写前 staging → 回读比对 → rename 的原子写路径（cc-switch 4.0 关键字段写入引擎同款）；②意图日志（`live-state.json` 模式）记录操作意图，启动时 settle 未完成操作（settle-before-acting）；③Codex 兼容清单进回归测试并持续跟进：`auth.json` 不作第三方凭据载体、官方路由不换 `model_provider`（否则会话历史分桶丢失）、`mcp_servers` 不写 `type` 键。
- **收益**：把"写坏用户配置"的残余风险压到接近零；Codex 升级的高频踩坑点被系统性收割（竞品在这条线上每版本都在修）。
- **落地记录（2026-10）**：已实现——op journal（`<bakDir>/op-journal.json` 意图日志，首个 mutation 前写入、成功才删除，残留中断在下一次 takeover/restore 以 warning 透出并收敛，CLI stderr + Web 警告条双通道）；写后回读校验（复用模板 drift 探针，没落 key 当场硬失败）；Codex 兼容不变量回归（`internal/takeover/codex_compat_test.go`：不写 auth.json、选择器与段名同源 provider_id、`requires_openai_auth`、mcp_servers 无 `type` 键）。契约见 `docs/client-takeover.md`「机制契约」。与 cc-switch 的有意差异：回读校验用语义探针（漂移探针）而非字节比对——我们的模板写入本来就是字段级合并而非整文件替换。

### S14. 粘滞×熔断×配额交互语义矩阵（路由正确性 P1）

- **现状**：会话粘滞优先于一切排序信号（sliding-dwell sticky），pin/force 是硬选择；但"粘滞目标被熔断/冷却后怎么办""failover 是否复核下一账号配额""被跳过的目标是否可观测"这组交互语义没有写成显式契约。Bifrost 在 9 月连修三个版本才把同一组语义修对。
- **方案**：①把交互矩阵写成路由契约 + 行为回归：粘滞绑定粒度到 (provider, account)；失败绑定丢弃、绝不复活已排除账号；被跳过的 fallback 进 routing log；failover 选下一账号时复核其配额状态（LiteLLM v1.103 同款）；fallback 引用路由时刻解析，重启/重载不失效。②server-side 会话的正确性策略：Claude Code thread、Responses `rs_*` 加密 reasoning 这类绑定上游账号的会话，要么粘死原账号，要么返回可识别错误让客户端整轮重发（Bifrost `thread_unsupported_request` 语义），禁止静默失败。
- **收益**：把"优化级"粘滞升级为"正确性级"契约；与 S8 的 `rs_*` 亲和是同一块的正确性半区。
- **落地记录（2026-10）**：矩阵契约 + 行为回归 + `skipped` 可观测已实现（见 routing-and-failure.md「粘滞×可用性×配额 交互矩阵」；注意与其失败绑定即丢弃的有意差异——我们是 dwell 内 recover-and-return）；server-side 会话（`rs_*`/Claude thread 的强制粘回/可识别拒接）待 S8 的 producer 追踪落地后实现，目标契约已写入同一节。

### S15. MCP 无状态化适配（生态 P2，扩展 S10）

- **现状**：MCP 网关用隐式会话粘滞承接有状态工具；MCP 2026-07-28 spec 已删除协议级会话（无 initialize 握手、无 `Mcp-Session-Id`），生态进入迁移期（mcphub 双栈、ContextForge 默认 legacy 显式切新、legacy SSE 进入 12 个月弃用窗口）。
- **方案**：①显式状态句柄（mcphub `X-MCPHub-State-Id` 模式：客户端 UUID，绑定凭据+用户+路由，空闲过期）作为会话粘滞的替代路径；②tools/list 聚合缓存按 `ttlMs`/`cacheScope` 落地，TTL = 上游 discovery 有效期 ∩ 网关预算，不确定则 0；③双栈姿势：默认 legacy、显式开关协商现代协议；④远程 MCP server 接入默认 SSRF fail-closed + 显式逃生阀（Docker/agentgateway 同款）；⑤若做工具禁用/allowlist，检查必须落在 tools/call 执行路径（mcphub v1.0.37 CRITICAL 教训：只在 tools/list 过滤可被注入绕过）。
- **收益**：跟随 spec 迁移而非被动挨打；安全姿势对齐基础设施厂商。

### S16. 结构化判定评测闭环（生态 P2，扩展 S11）

- **现状**：shadow/replay 的 judge 打分无结构化约束；评测只对新流量生效；结果只在报表可见。同期 Langfuse/Braintrust/Phoenix/GPTCache 四家都接入了 decision-model 判定（TypeSafe Jev）。
- **方案**：①judge 改结构化判定：定义问题 + 允许答案 → JSON schema 输出，带置信度与各选项概率，无自由文本；②新 judge 注册时对既有 request_log/shadow 记录做一次性历史回填，judge 版本留分数谱系（Langfuse evaluator backfill 同款）；③分数/成本超阈值发本地告警（live event/webhook）；④guard 加 dry-run 检查端点（NeMo `/v1/checks` 模式）：单独跑规则、不产生新响应，供 replay 回放扫描验证规则变更；⑤guard 判定记结构化 decision 节点进审计（Phoenix DECISION span 思路）。
- **收益**：S11 从"自动采样"升级为完整闭环；guard 从运行时组件变成可独立验证的规则系统。

### 暂不建议做的（明确边界）

- 多租户/令牌计费/充值体系（new-api 生境）：偏离定位且安全责任陡增。
- LLM-classifier/stage 路由算法（Switchyard 方向）：先观察；fusion+请求感知已覆盖主要价值，classifier 引入额外延迟与成本。
- 桌面 App 壳：中期候选（S1–S5 落地后，若增长瓶颈仍在触达，再评估 Tauri 壳包 CLI+UI，复用 web assets）。
- 语义缓存（2026-09 补充）：LiteLLM 自家文档警告 agentic 多轮会 stale replay（见 §2.3-1），与本仓库 `cache` 的定位判断一致；降本走 S8 的前缀/协议级亲和，而不是语义近似。2026-10 补充：GPTCache 未发版的 JevEvaluation 证明若未来重访，"多维检查取最小分 + 全过阈才命中"是唯一可接受的门控设计，单向量相似度不可接受。
- 错峰成本系数与 Router Index 报表口径（2026-10 补充，LiteLLM off-peak / Bifrost `off_peak_cost_multiplier` / OpenRouter Router Index）：观察项——配额调度引入时段系数、shadow 报表采用"质量/耗时/成本加权 0–10 分"都成立，但优先级低于 S13–S16；注意 OpenRouter 基准指出的切模型重建 input cache 成本会侵蚀路由收益。
- A2A 数据面（2026-09 补充）：观察——等编码 agent 生态出现真实的 A2A 客户端需求（当前 agentgateway/Portkey 的服务对象是企业平台）再评估。
- 通用插件机制（WASM/CGO 动态库，Bifrost/CLIProxyAPI 方向）：模块化单体 + 编译期 owner 边界（archtest 闭合契约）是架构红利，插件层会破坏它；确需扩展点优先走 config 声明式能力（如 guard `extra_patterns`）。

## 6. 建议路线图

| 阶段 | 内容 | 主线 |
|---|---|---|
| 近期 (1–2 迭代) | S3 preset 向导、S5 doctor/dry-run、S1 凭据加密启动 | 易用性+安全地基 |
| 中期 | S2 可选鉴权、S4 agent 扩张、S6 guard 产品化 | 打开团队场景 |
| 远期 | S7 metrics/OTel、评估桌面壳、社区 preset 生态 | 生态位巩固 |

第二轮（2026-09，对应 §2.3 增量）：

| 阶段 | 内容 | 主线 |
|---|---|---|
| 近期 | S8 prompt-cache 亲和、S9 guard 工具调用规则 | 成本 + agent 安全 |
| 中期 | S10 MCP 用户级 OAuth/限流、S11 评测数据飞轮 | 生态位巩固 |
| 远期 | S12 预算动作化（可选）、A2A 观察项 | 按需 |

第三轮（2026-10，对应 §2.4 增量）：

| 阶段 | 内容 | 主线 |
|---|---|---|
| 近期 | S14 粘滞×熔断×配额语义矩阵、S13 takeover 原子写 | 路由正确性 + 接管可靠性 |
| 中期 | S16 结构化判定评测闭环、S15 MCP 无状态化适配 | 评测 + MCP 生态 |
| 远期 | 错峰成本系数、Router Index 报表口径（观察项） | 按需 |

每项落地遵循仓库既有约定：行为回归用例进 owner package、跨模块 wiring 进 composition 测试、用户可见变化同步 README/config.yaml、内部契约进对应 `docs/architecture/` 专题。
