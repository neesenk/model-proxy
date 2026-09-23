# 同类开源项目调研与方向建议

> 状态：调研报告（非实现契约）。事实基准：2026-08 第一轮（GitHub 元数据快照，star 数仅用于量级判断）+ 2026-09 第二轮增量（各家官网/README/changelog 一手来源；其中的性能与成本数字多为厂商自述，未经第三方验证）。
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

## 4. 我们的定位（不变）

**本地优先的单二进制多 Provider LLM 代理**：面向个人开发者与小团队，以「订阅/OAuth 账号池 + 配额感知调度」为核心能力，「易用性 + 安全性」为长期护城河。不做多人分发/计费平台（new-api 生境），不做企业网关全家桶（LiteLLM 生境）。

对照调研，我们的既有优势：请求感知路由深度（图片/工具/上下文过滤、溢出改道）、shadow/replay 评测（全场唯一）、fusion、guard DLP-lite、强制 loopback 默认安全、takeover 深度。
主要短板：无图形化安装/托盘体验、凭据明文存储、`/api/*` 与 Web UI 无鉴权（靠 loopback 单点保障）、provider 接入依赖手写 YAML、无可观测导出面。

## 5. 下一步建议（收益 + 解决方案）

> **落地进展（2026-08 第二轮）**：S1 已实现（含 volcengine 接入与 doctor/status 可见性）；S2 已实现（`internal/webauth` + validate 门槛 + 双面鉴权）；S3 已实现（CLI + Web 向导 + FetchModels 交集校验）；S4 已实现 kimi（Gemini CLI 因代理不支持 Gemini wire 协议暂缓）；S5′ 已并入 doctor；S6 被 guard 审计提交超越；S7 已实现（`GET /metrics`）。细节见各设计文档的「实现记录」。
>
> **落地进展（2026-09 第三轮）**：S1–S7 状态不变；§2.3 第二轮增量对应的落地候选为 S8–S12（均为建议，未实现）。

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
- **方案**：`model-proxy doctor`：校验 config（含 route 可达性静态检查、claude_mapping 歧义、models.dev 匹配告警汇总）、凭据完整性、端口占用、daemon 健康、takeover 目标漂移检测；`serve --dry-run` 复用同一校验器输出后退出。Web UI Status 页展示同样的检查结果。
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

### 暂不建议做的（明确边界）

- 多租户/令牌计费/充值体系（new-api 生境）：偏离定位且安全责任陡增。
- LLM-classifier/stage 路由算法（Switchyard 方向）：先观察；fusion+请求感知已覆盖主要价值，classifier 引入额外延迟与成本。
- 桌面 App 壳：中期候选（S1–S5 落地后，若增长瓶颈仍在触达，再评估 Tauri 壳包 CLI+UI，复用 web assets）。
- 语义缓存（2026-09 补充）：LiteLLM 自家文档警告 agentic 多轮会 stale replay（见 §2.3-1），与本仓库 `cache` 的定位判断一致；降本走 S8 的前缀/协议级亲和，而不是语义近似。
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

每项落地遵循仓库既有约定：行为回归用例进 owner package、跨模块 wiring 进 composition 测试、用户可见变化同步 README/config.yaml、内部契约进对应 `docs/architecture/` 专题。
