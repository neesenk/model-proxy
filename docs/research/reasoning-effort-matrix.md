# 思考等级（reasoning effort）客户端与厂家对照矩阵

> 状态：调研报告（非实现契约）。事实基准：2026-09-07 快照，客户端源码与厂家 API 文档均为该时点版本，**版本敏感**——档位枚举、默认值、互斥规则随版本频繁变化，引用前须复核来源。
> 本文档是 proxy effort↔budget 转换设计的事实库；实现契约仍以 `docs/architecture/protocol-conversion.md` 为准。

## 1. 背景与用途

model-proxy 需要在 Anthropic Messages / OpenAI Chat Completions / OpenAI Responses 三种协议间转换思考等级设置：客户端发来的形态五花八门（effort 枚举、thinking 开关、数值 budget），各厂家 API 收的形态也各不相同且常有互斥约束。本文分两轮调研整理：第一轮覆盖主流 coding-agent 客户端"发什么"，第二轮覆盖各模型厂家官方 API"收什么"，作为 proxy 统一 effort↔budget 转换设计的对照事实库。

## 2. 规范层：七档 canonical 枚举

proxy 内部统一采用七档枚举，取 **OpenAI Responses API 的 `reasoning.effort` 全集**作为 canonical（它是各方枚举的超集）：

```
none < minimal < low < medium < high < xhigh < max
```

- 各家枚举都是这七档的子集或别名（如 Anthropic 无 minimal/none，DeepSeek 只有 low/high/max）。
- `none` 语义特殊：部分厂家是"关思考"（Anthropic `disabled`、Qwen `enable_thinking=false`），部分厂家不允许关思考（见 §6）。

## 3. 客户端矩阵

| 客户端 | 暴露档位 | 默认 | wire 形态 |
|---|---|---|---|
| Codex CLI | none/minimal/low/medium/high/xhigh（源码另有 max/ultra/persistent + Custom 透传，按服务端 catalog 校验） | medium（catalog 决定） | `reasoning:{effort, summary:"auto"}`；config `model_reasoning_effort` |
| Claude Code | low/medium/high/xhigh/max（无 minimal/ultra；Opus 4.6/Sonnet 4.6 无 xhigh；伪档位 ultracode 不上 wire） | high（Opus 4.7 = xhigh） | 自适应模型 `output_config:{effort}`；旧模型 `thinking:{type:"enabled",budget_tokens}`（MAX_THINKING_TOKENS 控制） |
| opencode | Anthropic 变体 low/medium/high/xhigh/max；OpenAI 按模型 none/minimal/…/xhigh；多数国产模型无变体 | Anthropic=high，gpt-5=medium | 旧 Claude budget（high=16000/max=31999）；4.7+ effort；OpenAI `reasoningEffort`/`reasoning_effort` |
| pi (pi-mono) | off/minimal/low/medium/high/xhigh/max | medium | budget 模型 `budget_tokens` 1024/2048/8192/16384（xhigh/max 钳到 high）；自适应 `output_config:{effort}`；OpenAI `reasoning_effort` / `reasoning:{effort}` |
| Kimi CLI | off/low/medium/high/xhigh/max | off | Kimi 端点只发 `thinking:{type:"enabled"|"disabled"}`（effort 纯客户端状态）；Anthropic 端点旧模型 budget 1024/4096/32000，4.6+ `output_config.effort` |
| ZCode (z.ai) | GLM/Kimi：low/high/max（GLM-5.2 有 nothink）；GPT/Claude：low/medium/high/xhigh | GLM/Kimi=max，GPT/Claude=medium | 闭源未公开；确认 OpenAI 兼容路由发 `thinking` 字段 |
| Roo Code / Cline | budget 数值滑块（min 1024，封顶 80%×max_tokens） | 8192 | `budget_tokens` 数值 |
| Aider | `--reasoning-effort` low/medium/high 或 `--thinking-tokens` | 无默认 | 按后端协议透传 |
| Gemini CLI | 无一等 effort 开关 | 动态（-1） | `thinkingConfig.thinkingBudget` 数值（如 4096） |
| Qwen Code | low/medium/high/xhigh/max | — | `reasoning:{effort}`；Anthropic 协议 `output_config.effort`（max 钳 high） |

要点：

- **effort 枚举与数值 budget 两种形态并存**，且同一客户端对不同上游用不同形态（Claude Code、opencode、pi、Kimi CLI 都是双形态）。
- 客户端默认值严重分裂：medium（Codex/pi/gpt-5）、high（Claude Code/opencode）、xhigh（Opus 4.7）、off（Kimi CLI）。proxy 不能假设"客户端不传 = 某个统一默认"。
- 未知/私有档位真实存在（Codex 的 persistent/ultra/Custom、Claude Code 的 ultracode），proxy 必须有兜底策略（§5）。

来源：
- Codex CLI: https://github.com/openai/codex （codex-rs/protocol/src/openai_models.rs）, https://developers.openai.com/codex/config-reference
- Claude Code: https://code.claude.com/docs/en/model-config , https://platform.claude.com/docs/en/build-with-claude/effort
- opencode: https://github.com/anomalyco/opencode （packages/opencode/src/provider/transform.ts）
- pi: https://github.com/badlogic/pi-mono （packages/ai/src/api/*）
- Kimi CLI: https://github.com/MoonshotAI/kimi-cli （packages/kosong）
- ZCode: https://zcode.z.ai/en/docs/configuration
- Roo Code: https://github.com/RooCodeInc/Roo-Code （src/api/transform/model-params.ts）
- Aider: https://aider.chat/docs/config/reasoning.html
- Gemini CLI: https://geminicli.com/docs
- Qwen Code: https://qwenlm.github.io/qwen-code-docs

## 4. 厂家矩阵

| 厂家 | 字段形态 | 枚举/范围 | 默认 | 约束/冲突 | echo-back 要求 |
|---|---|---|---|---|---|
| OpenAI | `reasoning.effort` | none\|minimal\|low\|medium\|high\|xhigh\|max（无数值预算） | schema 默认 medium，按模型覆盖：gpt-5.1/5.2 默认 none，gpt-5.5/5.6 默认 medium | gpt-5.2-codex 无 none/minimal；GPT-6 Astra + none 明确 400 | — |
| Anthropic | `thinking.type` ∈ enabled\|adaptive\|disabled；`budget_tokens`；`output_config.effort` | effort ∈ low\|medium\|high\|xhigh\|max（无 minimal/none/ultra）；budget ≥1024 且 < max_tokens（interleaved beta 例外），目标非硬上限 | effort 默认 high | 4.7+/5.x 仅 adaptive（发 enabled 必 400）；4.6 并存；4.5- 仅 extended；Opus 5 在 xhigh/max 下拒 disabled | — |
| DeepSeek | `thinking:{type:"enabled"\|"disabled"}` + `reasoning_effort` | low\|high\|max（medium/xhigh→high） | high | — | 带 tools 时历史 `reasoning_content` 必须逐字回传否则 400 |
| Zhipu GLM | `thinking` switch + `reasoning_effort` | 5.3：max\|high\|low；5.2：none\|minimal\|low\|medium\|high\|xhigh\|max（none/minimal=停思考，low/medium→high，xhigh→max） | thinking 默认开；5.3 effort 默认 max | GLM-5.3 发 disabled 报错（只能 enabled + `reasoning_effort:"low"`） | `clear_thinking:false` = 保留思考，需完整回传 |
| Moonshot Kimi | 按模型：kimi-k3 用 `reasoning_effort`；k2.6 `thinking.type` enabled\|disabled | k3：low\|high\|max | k3 默认 max（不要发 thinking）；k2.6 默认 enabled | k2.7-code 仅 enabled | K3/K2.7-code 的 `reasoning_content` 每轮必须逐字回传；`thinking.keep:"all"` |
| 阿里 Qwen / DashScope | `enable_thinking` bool + `thinking_budget`；qwen3.8 新增 `reasoning_effort` | budget 1–32768；effort ∈ xhigh\|medium\|low | budget 默认 4000（按模型变）；qwen3.8 effort 默认 xhigh | qwen3.8 的 reasoning_effort 与 thinking_budget **互斥**；官方换算 low↔4096 / medium↔16384 / xhigh↔262144，max/high→xhigh，none→`enable_thinking=false` | — |
| MiniMax | M3：`thinking:{type:"adaptive"\|"disabled"}` | 无 effort/budget 旋钮 | — | M2.x 无法关思考（disabled 静默忽略）；`reasoning_split` 只是输出格式开关 | — |
| Volcengine Doubao | `thinking.type` ∈ enabled\|disabled\|auto + `reasoning_effort` | effort 全枚举 none…max，但按模型子集（如 seed-2-0-lite：low/medium/high/minimal） | 按模型（seed-2-0-lite 默认 medium） | disabled + 非 none effort → 400 | — |
| 小米 MiMo | `thinking:{type:"enabled"\|"disabled"}` | 无 effort 档 | enabled | — | — |
| Google Gemini | 2.5 系：`thinkingConfig.thinkingBudget`；Gemini 3：`thinkingLevel` | budget：2.5 Pro 128–32768、Flash 0–24576、Flash-Lite 默认关，-1=动态；level ∈ MINIMAL\|LOW\|MEDIUM\|HIGH | 动态 | 2.5 Pro 不可关思考；3 Pro 仅 low/high；level + budget 同发 → 400 | — |
| xAI Grok | `reasoning_effort` | low\|medium\|high\|xhigh | high | 不可关思考；grok-4.5 上 xhigh 静默按 high | — |
| OpenRouter | `reasoning:{effort,max_tokens,exclude,enabled,context,mode}` | effort 全枚举 | — | effort→max_tokens 百分比：max/xhigh≈95% / high≈80% / medium≈50% / low≈20% / minimal≈10%；Claude 上游 budget = max(min(max_tokens×比例, 128000), 1024)，minimal→low，none 被拒；Gemini 3 xhigh→high clamp | — |
| Mistral | `reasoning_effort` | 可调模型仅 high\|none | — | 官网不可达，依据 Wayback 快照 | — |
| Bedrock / Vertex 上的 Claude | 与 Anthropic 第一方一致 | 同 Anthropic | 同 Anthropic | 已知生态问题：部分 Vertex 网关剥 `output_config`（litellm issue，非厂家文档） | — |
| 阶跃星辰 | 官方参数页未取得 | 第三方记录 low/medium/high，xhigh 报错 | — | **未证实**，见 §7 | — |

要点：

- **枚举字段三家三样**：`reasoning.effort`（OpenAI/Qwen Code/xAI）、`reasoning_effort`（DeepSeek/GLM/Kimi-k3/Doubao/Mistral）、`output_config.effort`（Anthropic 系）、`thinkingLevel`（Gemini 3）。proxy 转换必须按厂家选字段名，不能只转值。
- **数值 budget 只有 Anthropic（budget_tokens）、Gemini 2.5（thinkingBudget）、Qwen（thinking_budget）三家收**，且语义边界不同：Anthropic 是"目标非硬上限"且必须 < max_tokens；Qwen 是与 effort 互斥的旧旋钮；Gemini 2.5 Pro 下限 128 且不可关。
- **echo-back（思考内容回传）是 DeepSeek、Kimi、GLM（clear_thinking:false）三类厂家的硬约束**，proxy 做多轮转发时不能只转 effort 字段而丢弃 reasoning_content。
- "默认"普遍按模型细分而非按厂家统一（OpenAI、Doubao、Anthropic 模型矩阵），proxy 的 effort 默认值应交给 route/模型层决定。

来源：
- OpenAI: https://developers.openai.com/api/docs/guides/reasoning , https://github.com/openai/openai-openapi
- Anthropic: https://platform.claude.com/docs/en/build-with-claude/thinking , …/extended-thinking , …/effort , …/thinking-troubleshooting
- DeepSeek: https://api-docs.deepseek.com/guides/thinking_mode
- Zhipu GLM: https://docs.z.ai/guides/capabilities/thinking , …/thinking-mode
- Moonshot Kimi: https://platform.kimi.com/docs/guide/use-thinking-models
- 阿里 Qwen: https://help.aliyun.com/zh/model-studio/deep-thinking
- MiniMax: https://platform.minimax.io/docs/api-reference/text-openai-api
- Volcengine Doubao: https://www.volcengine.com/docs/82379/2662855
- 小米 MiMo: https://mimo.mi.com/docs/en-US/api/chat/openai-api
- Google Gemini: https://ai.google.dev/api/generate-content , https://ai.google.dev/gemini-api/docs/thinking
- xAI: https://docs.x.ai/developers/model-capabilities/text/reasoning
- OpenRouter: https://openrouter.ai/docs/use-cases/reasoning-tokens
- Mistral: https://web.archive.org/web/20260330181523/https://docs.mistral.ai/capabilities/reasoning
- Bedrock: https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-anthropic-claude-messages.html ; Vertex: https://cloud.google.com/vertex-ai/generative-ai/docs/partner-models/use-claude

## 5. 预算聚类分析与 proxy 阶梯选择

真实客户端发出的 budget 数值聚在以下六簇：

| 簇 | 来源实例 |
|---|---|
| 1024 | pi minimal、Kimi CLI low、各家下限（Anthropic/Roo min 1024） |
| 2048 | pi low |
| 4096 | Kimi CLI medium、Gemini CLI 示例、Qwen 默认 4000≈4096 |
| 8192 | pi medium、Roo 默认 8192 |
| 16384 | pi high、opencode 16000≈16384 |
| 31999–32000 | Kimi CLI high=32000、opencode max=31999 |

据此 proxy 的 effort→budget 阶梯取：

```
minimal=1024  low=2048  medium=8192  high=16384  xhigh=32000  max=顶格(max_tokens−1 钳制)
```

理由：与 pi 的四档逐值吻合；直接覆盖 Roo 默认 8192、Kimi high 32000；opencode 的 16000 与 16384 相差 2%，归入同簇。max 没有公认数值，取"顶格"（max_tokens−1，满足 Anthropic budget < max_tokens 约束）。

反向（budget→effort）阈值取阶梯值就近归档：`≥32000→xhigh`、`≥16384→high`、`≥8192→medium`、`≥2048→low`、`>0→minimal`。自有档位互逆；外来 budget 就近向下归档。

已知近似（非 1:1，best-effort）：

- Kimi CLI high=32000 反向会升为 xhigh（+1 档）；Kimi CLI low=1024 反向降为 minimal（−1 档）。
- max 的顶格值反向按阈值归档为 xhigh 而非 max。
- budget↔effort 双向转换本质是有损的，文档与实现均按 best-effort 处理并注明。

未知 effort 档位（如 Codex persistent/Custom）：钳到 high + 告警，与 kimi-cli、Qwen Code 的通行钳制做法一致。

## 6. 冲突与不可关思考清单

发送转换后的请求前，proxy 必须逐厂家检查以下约束：

| 厂家 | 约束 | 后果 |
|---|---|---|
| Moonshot Kimi | kimi-k3 同时发 thinking + reasoning_effort 被拒 | 400；k3 只发 reasoning_effort |
| Volcengine Doubao | `thinking.type:"disabled"` + 非 none 的 reasoning_effort | 400 |
| Google Gemini | thinkingLevel + thinkingBudget 同发 | 400 |
| 阿里 Qwen qwen3.8 | reasoning_effort + thinking_budget 同发 | 互斥，需二选一 |
| Anthropic 4.7+/5.x | 发 `thinking.type:"enabled"`（仅收 adaptive） | 400 |
| Anthropic Opus 5 | xhigh/max 下同时发 `disabled` | 拒绝 |

不可关思考（`none`/`disabled` 无效或报错）的模型：

- Zhipu GLM-5.3（disabled 报错，只能 enabled + effort:"low" 近似关）
- MiniMax M2.x（disabled 静默忽略）
- Moonshot Kimi K2.7-code（仅 enabled）
- Google Gemini 2.5 Pro（budget 下限 128，无 0）
- xAI Grok（无 none 档，不可关）

对不可关思考的模型，proxy 收到 `none` 时应降级为厂家允许的最低档（如 GLM-5.3 的 low），而不是原样转发。

## 7. 版本敏感性与未证实项

版本敏感（快照 2026-09-07，引用前复核）：

- OpenAI 各模型默认 effort（gpt-5.1/5.2=none、gpt-5.5/5.6=medium）随模型发布变化；gpt-5.2-codex 的 none/minimal 缺失是模型级约束。
- Anthropic 模型×thinking 模式矩阵（4.5- 仅 extended / 4.6 并存 / 4.7+ 仅 adaptive）是迁移中的状态，新模型只会更靠 adaptive 侧。
- Zhipu GLM 5.2 与 5.3 的 effort 语义差异极大（5.2 none=停思考 vs 5.3 拒 disabled），升级模型必须重查。
- 各 coding-agent 的档位表和默认值跟随其上游 catalog 更新（Codex 明确按服务端 catalog 校验）。

未证实项（不进入实现契约，仅备忘）：

- **阶跃星辰**：官方参数页未取得，仅第三方记录（low/medium/high，xhigh 报错）。
- **Mistral**：官网文档不可达，依据 Wayback 快照（2026-03-30），可调模型仅 high|none。
- **Vertex 网关剥 `output_config`**：来自 litellm issue 的生态报告，非厂家文档，未在 proxy 环境复现。
