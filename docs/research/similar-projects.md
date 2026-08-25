# 同类开源项目调研与方向建议

> 状态：调研报告（非实现契约）。事实基准：2026-08 GitHub 元数据（star 数为快照值，仅用于量级判断）。
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

## 3. 业界动向总结

1. **赛道分化完成**：「服务端网关」（new-api/LiteLLM/Portkey/Higress，卷计费与企业治理）vs「编码 agent 本地控制面」（CCR/cc-switch/CLIProxyAPI，卷易用性与 agent 覆盖）。我们在后者的深水区（配额感知调度、订阅账号池、评测），前者不做也不该做。
2. **Anthropic Messages 正在成为 agent 侧通用语**：各家都以 Claude Code 兼容为默认入口，同时 OpenAI Responses 格式渗透。协议转换从"加分项"变为"标配"（Switchyard 整个项目就建立在这上面）。我们三协议转换方向正确。
3. **订阅/OAuth 账号池化成为刚需**：CLIProxyAPI 48k star 的核心就是"把订阅当 API 用"；上游厂商（Codex/Claude/Qwen Plan/Kimi Code）订阅制普及让"多账号池+配额感知"从 hack 变成产品能力。这是我们已有的最强差异化之一。
4. **路由智能化**：静态规则 → 请求感知 → 信号驱动/分类器驱动（Switchyard stage_router、OpenSquilla on-device router）。趋势是"用便宜的信号决定贵模型的调用时机"。
5. **易用性竞争 = 桌面化 + 向导化**：纯 CLI 在个人用户市场增长见顶，头部项目全在做图形界面、托盘常驻、一键导入 preset、doctor 类自检。
6. **安全成为分水岭**：公开部署的 one-api/new-api 实例安全事故频发；LiteLLM 把 guardrails 商业化。个人本地场景的安全焦点转向：凭据存储加密、秘密外泄扫描、审计。
7. **上游生态反向收编**：中转商大量赞助这些开源项目换取曝光，部分项目开始内置商业 preset——说明这个入口位置有真实商业价值。

## 4. 我们的定位（不变）

**本地优先的单二进制多 Provider LLM 代理**：面向个人开发者与小团队，以「订阅/OAuth 账号池 + 配额感知调度」为核心能力，「易用性 + 安全性」为长期护城河。不做多人分发/计费平台（new-api 生境），不做企业网关全家桶（LiteLLM 生境）。

对照调研，我们的既有优势：请求感知路由深度（图片/工具/上下文过滤、溢出改道）、shadow/replay 评测（全场唯一）、fusion、guard DLP-lite、强制 loopback 默认安全、takeover 深度。
主要短板：无图形化安装/托盘体验、凭据明文存储、`/api/*` 与 Web UI 无鉴权（靠 loopback 单点保障）、provider 接入依赖手写 YAML、无可观测导出面。

## 5. 下一步建议（收益 + 解决方案）

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

### 暂不建议做的（明确边界）

- 多租户/令牌计费/充值体系（new-api 生境）：偏离定位且安全责任陡增。
- LLM-classifier/stage 路由算法（Switchyard 方向）：先观察；fusion+请求感知已覆盖主要价值，classifier 引入额外延迟与成本。
- 桌面 App 壳：中期候选（S1–S5 落地后，若增长瓶颈仍在触达，再评估 Tauri 壳包 CLI+UI，复用 web assets）。

## 6. 建议路线图

| 阶段 | 内容 | 主线 |
|---|---|---|
| 近期 (1–2 迭代) | S3 preset 向导、S5 doctor/dry-run、S1 凭据加密启动 | 易用性+安全地基 |
| 中期 | S2 可选鉴权、S4 agent 扩张、S6 guard 产品化 | 打开团队场景 |
| 远期 | S7 metrics/OTel、评估桌面壳、社区 preset 生态 | 生态位巩固 |

每项落地遵循仓库既有约定：行为回归用例进 owner package、跨模块 wiring 进 composition 测试、用户可见变化同步 README/config.yaml、内部契约进对应 `docs/architecture/` 专题。
