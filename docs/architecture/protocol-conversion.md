# 协议转换契约

## 适用范围

修改 `internal/protocol/*`、`internal/provider/protocol_hint.go`、跨协议
route、流式转换或工具调用映射时必读。

## 边界

目标未声明 `protocol:` 时，上游协议按 `(*Proxy).resolvedBackendProto` 解析：显式 `protocol:` > `ProtocolHint`（codex→responses）> 模型级协议矩阵 > provider 级 wire 探测 verdict（`internal/app/wirecap.go`，boot/reload 时在 openai base 上探测 `/chat/completions` 与 `/responses`；anthropic 支持由 `anthropic_base_url` 声明，不探测）> 客户端协议透传。完整两级决策矩阵与探测分类见 `routing-and-failure.md`。只有解析出的后端协议与客户端不同，才启用转换；verdict unknown 时维持透传（boot 窗口期行为不变），verdict 说 `/responses` 不存在时 anthropic/responses 客户端自动转 chat。

三个协议值：

- `anthropic` = Anthropic Messages（`/v1/messages`，`messages` + content blocks）
- `openai` = OpenAI Chat Completions（`/v1/chat/completions`，`messages` + `tool_calls`）。**不再**表示 Responses API。
- `responses` = OpenAI Responses API（`/v1/responses`，`input` list + `output` items + `response.*` 流式事件）。`/v1/responses` 路径由 `protocol.ForPath` 独立判为 `responses`，不再并入 `openai`。

转换器为直连 pairwise codec，并统一注册在
`internal/protocol/conversion_registry.go`：
一个 client→backend pair 必须同时声明 request、反向 response 和反向 SSE
三个入口。注册表覆盖 3×2 共六组方向，并由结构测试保证完整；`convertRequestFor`、
`convertResponseNS` 和 `convertSSEReaderNS` 不得各自维护方向 switch。
pair-specific codec 保留协议特有语义，不引入最低公分母 IR：hosted tools、
reasoning 方言、namespace restore 等信息无法通过统一 message IR 无损表达。
`needsConversion` 对任意两个不同的已知协议返回 true；未知协议值 fail-safe 不转换。

`internal/protocol` 是仓库依赖叶子，拥有协议解析、转换 registry、请求/响应/SSE
codec、SSE↔JSON 模式桥接、图片缩减和 Responses `previous_response_id` 状态；
生产文件不得 import `model-proxy/*`。应用层 `internal/app` 只通过导出 facade 接线：
`RequestOptions` 接收 `targetexec.Plan` 已解析的 reasoning 方言、Codex shaping
和视觉能力，`ResponseContext` 对 namespace/custom restore 状态保持 opaque。
Provider 映射仍由 `internal/provider.ChatReasoningMode` 定义，具体 HTTP 400 envelope
仍由 transport 层写入，二者都不反向进入协议包。

Codex 后端只接受 Responses API，因此 `ProtocolHint("codex") = "responses"`。转发路径在目标未声明 `protocol:` 时经 `resolvedBackendProto` 自动回退到 `ProtocolHint`（forward/fusion/shadow 共用），所以 anthropic/chat 客户端打 codex 路由会**自动转换**,无需用户写 `protocol: responses`(显式声明仍可,且优先级最高)。Responses 客户端跨协议访问 chat/anthropic 后端时，proxy 为 `previous_response_id` 维护短期本地历史：按 session + response id 索引、TTL 30 分钟、最多 512 条、单条 2 MiB、总量 32 MiB，异步以 0600 写入 quota state 同目录的 `responses_state.json`。命中时展开完整 input/output 历史；未命中时只修复本次缺失历史导致的孤立 output/dangling call，普通显式全历史请求不做全局配对改写。只有 completed 和因 token 上限产生的 incomplete 响应进入 replay state，content_filter/其他中止不缓存。无稳定 session 时仅允许 response id 唯一命中，避免跨会话串线。此外 `protocol.ConvertRequestWithOptions` 在 target plan 注入 `CodexShaping` 且目标协议为 responses 时剥离 `max_output_tokens`/`temperature`/`top_p`——该预剥离只作用于转换路径；客户端本来就讲 responses 的同协议 codex 流量保持字节级透传（`targetexec.Plan.ConvertBody` 短路），其 unsupported parameter 400 靠 paramBlock 学习后预防性剥离自愈（`routing-and-failure.md`）。

跨协议转换前先运行 capability scanner。已知无法无损表达的请求（例如 Chat `n>1`/logprobs/audio、未知 hosted tool、Anthropic MCP server、Responses 24h cache retention → Anthropic、Responses custom/freeform tool → Anthropic）不会进入上游：当前 target 被跳过并继续 failover；若没有兼容 target，按客户端协议返回 HTTP 400、code=`unsupported_protocol_conversion`。同协议透传不受扫描器影响。

## 请求映射（Responses 方向）

- anthropic `system` ↔ responses `instructions`；chat 的**全部** system/developer message 按原顺序合并为 `instructions`（忽略空文本，以 `\n\n` 连接）。**messages 内的 system/developer role 不允许进 Responses input**（codex 400 "System messages are not allowed"）：a→r 把它们折叠进 `instructions`（接在顶层 system 后，`\n\n` 连接，与 chat→r 一致）；r→a 反向把 input 里的 system/developer message item 折叠进 anthropic 顶层 `system`（同样接在 instructions 后，`\n\n` 连接——anthropic messages 只接受 user/assistant，原样发 role:"system" 必 400，降级为 user 会改变指令优先级，cc-switch transform_codex_anthropic 同款）。
- **孤立 reasoning item 丢弃（a→r，cc-switch transform_responses.rs）**：仅含 thinking/redacted_thinking 块的 assistant 消息（incomplete turn 历史常见）转换后没有任何 message/function_call 后继，codex 400 "reasoning item without its required following item"——这类 reasoning item 直接丢弃 + `convertWarn`；同代内有 function_call/文本后继时保留。
- `messages` ↔ `input` items：text block ↔ `{type:message, content:[{input_text|output_text}]}`；image ↔ `{input_image, image_url}`（base64 转 data URL，url-source 原样保留）；Anthropic `document` ↔ Responses `input_file`（base64 data URL/http(s) URL/filename），Chat 使用 `file` part，URL-only Chat 降级为带文件名的文本链接。**file_id 不跨协议透传**：file_id 是 provider 域作用域的引用，跨协议（几乎必然跨 provider）的目标不认识它——六个方向统一处理：仅 file_id 时整个附件降级为可观测的文本注记 + `convertWarn`（`file_id_degraded` 诊断）；与 file_url/file_data 同时出现时保留可传输的 inline base64/URL 源、仅丢弃 file_id 并发同一诊断（r→chat/r→a 同样偏好 URL 源；Switchyard 同款语义，见 `docs/decisions/intentional-behaviors.md`）。**无 type 的 message 简写项**：Responses API 允许 input 里的 message 省略 `type`（`{role, content}` 简写，pi-ai/openai-responses 即此形态）——`responsesInputItems` 统一归一化为 `type:"message"`（有 `role` 且无 `type` 时补键），否则 r→chat/r→a 会把它当未知 item 丢弃、上游收到空消息列表（zhipu 400 "输入不能为空" 1214）；message content 的字符串简写同样合法，r→chat 经 `chatContentText`、r→a 在 `responsesContentToAnthropicBlocks` 塌缩为单个 text 块。
- anthropic `tool_use` / chat `tool_calls` → responses `{type:function_call, name, arguments, call_id}`；tool `id` ↔ `call_id`（`function_call_output` 的 `call_id` 必须回填）。arguments 缺省补 `"{}"`；chat→r 对只带 id 不带 name 的 tool_call（replace-style 客户端重发）按 call_id 从同请求前面的 function_call 回填 name，仍缺则 convertWarn。
- anthropic `tool_result` / chat `role:tool` → responses `{type:function_call_output, call_id, output}`。`is_error:true` 使用 cc-switch 兼容的 `[cc-switch:tool-result-error]` marker 可逆编码，反向恢复 `is_error`。**碰撞后果**：工具输出文本本身恰等于该 marker、或以 marker + `\n` 开头时（`internal/protocol/convert.go` 的 `splitToolResultError`），反向转换会误判 `is_error:true` 并剥掉 marker 前缀——内联编码无法与真实输出区分，属已知取舍。
- **tool_result 媒体改投（cc-switch 剥离-改投，按目标模型视觉能力门控）**：tool_result/function_call_output 里的 image 块不再丢弃——tool/function_call_output 消息只带文本，图片紧随一条合成 user 消息改投：a→chat 为 `{"role":"user","content":[{"type":"text","text":"[image returned by tool]"},{"type":"image_url",...}]}`（base64 转 data URL、url source 保留）；a→r 为 `{type:message, role:user, content:[input_text + input_image]}`；r→chat 的 output parts 数组同样拆 text/image 改投；**r→a 的 function_call_output parts 数组拆成 anthropic 原生块**（text part → tool_result 文本块，image part → image 块，base64/url source 均支持；r→a 侧无视觉门控——anthropic 目标恒接受 image 块）。连续多条带图 tool_result 各自的合成消息跟在各自 tool 消息后；无图时行为不变。**视觉门控**（`imageOKForTarget`，config `capabilities:` > models.dev catalog > 缺省 true）：目标模型无视觉能力时不发合成 user 消息，占位文本 `[image omitted: target model has no vision capability]` 并进 tool 消息文本（文本为空即全部 content）——无视觉上游（如 deepseek）会对 image_url part 400。
- **占位 reasoning_content（thinking 方言限定）**：r→chat 在消息列表构建完成后，对 `ChatReasoningMode == "thinking"` 的 provider，给每条带非空 tool_calls 且 `reasoning_content` 为空/缺失的 assistant 消息注入 `"reasoning_content": "tool call"`（deepseek 400 "reasoning_content must be passed back"，kimi/Moonshot 同样拒绝；codex 的 reasoning item 为空 summary + encrypted_content，附挂后恰为空，由此兜住 codex→deepseek）。其他方言不注入。
- **MCP namespace 与 hosted tools**：namespace function 经 Chat 或 Anthropic target 均按 `namespace__name` 压平（截断到 ≤64 字节时回退到 rune 边界，不产出非法 UTF-8），并在非流式/SSE 响应中按原请求 context 还原，撞名 fail-closed；namespace 容器先展开。Responses `web_search` 在 Anthropic 侧映射为 `web_search_20250305`，在 Chat 侧降级为同名 function；`tool_search` 在两侧降级为带 `{query,limit}` schema 的客户端工具；两个方向的 hosted fallback 名都与用户工具共用撞名检查，冲突 fail-closed。`web_search_call` 非流式与 r→{a,chat} 流式均保留（Anthropic 为 `server_tool_use + web_search_tool_result` pair，Chat 为 function tool call）。`tool_search_output.tools` 是下一轮已加载的真实工具声明：与顶层/additional_tools 合并，namespace 同样展开压平；结果消息列出准确 wire name，failed status 恢复为 Anthropic `is_error` 或 Chat 可逆错误 marker。
- **custom/freeform 工具（cc-switch transform_codex_chat）**：Codex CLI 主力工具（shell/apply_patch）是 `{type:"custom", name}`，call 携带**原始字符串** input 而非 JSON arguments。r→chat：custom 工具包装为单参数 function（`parameters={"input": string}`，required+additionalProperties:false），`custom_tool_call` 历史编码为 `arguments={"input": <raw>}`，`custom_tool_call_output` 同 function_call_output（含媒体改投）。chat→r：响应转换凭从原始请求体重建的 custom 集合（`r2cCtx.custom`，与 ns restore map 同通道 `r2cCtxFor` 传递）把命中名字的 tool_call 还原为 `{type:"custom_tool_call", input}`（arguments JSON 解出 `input`，解析失败原样兜底）；流式合成 `response.custom_tool_call_input.{delta,done}`（item id `ctc_item_<idx>`），arguments 的部分 JSON 做**渐进解包**（吃 `{"input": "` 前缀、反转义内容、尾部不完整转义留存下一 chunk、结尾 `"}` 吃掉）。Anthropic 没有等价的 raw-input tool contract，因此 Responses custom/freeform → Anthropic 由 capability scanner 明确拒绝并尝试下一 target，而不是错误地伪装为 JSON-schema tool。
- **reasoning effort 方言（`internal/provider.ChatReasoningMode` → `protocol.RequestOptions.ReasoningDialect`，cc-switch mapReasoningEffort）**：r→chat 的 `reasoning.effort` 按 provider 渲染——zhipu/volcengine/kimi-code/deepseek → `thinking:{type:"enabled"|"disabled"}`（none/minimal→disabled）；qwen-plan → `enable_thinking: bool`；aqp、shopee（OpenRouter 系）→ 原生 `reasoning:{effort}` 对象；其他 → `reasoning_effort` 原样。无 reasoning 字段时任何模式都不发声。
- anthropic `thinking` ↔ responses `{type:reasoning, summary, encrypted_content}`；`thinking`↔`summary.text`，`signature`↔`encrypted_content`。`redacted_thinking` ↔ `summary` 为空数组但**键必须存在**且仅含 `encrypted_content` 的 reasoning item。Anthropic↔Chat 使用 replay envelope：可见文本走 `reasoning_content`，签名 thinking/redacted block 原样放在 `reasoning_details` 扩展；Chat 客户端回放该扩展时恢复原块并保证位于 tool_use 前（thinking 文本为空的项不回放——空 thinking 块可能被 Anthropic 拒收），unsigned reasoning 不伪造成 Anthropic thinking。**a→r 的 `reasoning` 请求对象恒带 `summary:"auto"`**。**adaptive thinking** 的 effort 取自顶层 `output_config.effort`。codex provider 合并 `include:["reasoning.encrypted_content"]`。**a→r 注入 `prompt_cache_key`**：优先 `metadata.user_id` 的 sha256；否则用 model+instructions+工具的确定性指纹。Anthropic 显式 cache breakpoint 转 Chat 时同样生成稳定 cache key；Chat↔Responses 透传 `prompt_cache_key`/`prompt_cache_retention`，24h retention 到 Anthropic 因无法等价表达而 fail-closed。
- **引用**：Anthropic URL citation → Responses `output_text.annotations[].url_citation` / Chat `message.annotations[].url_citation`，按 Unicode 字符计算输出 offset；Chat↔Responses 可逆，非流式与 SSE 均覆盖。Responses → Anthropic 缺少 Anthropic web citation 必需的 `encrypted_index`，因此不伪造结构化 citation，而在同一文本块追加去重 Markdown 来源链接（流式按 block 去重：同一 URL 的多个 annotation 事件只追加一次；chat→a 的 parts 数组 content 同样把 message 级 annotations 追加进文本块）。file citation 仅在存在 `file_id` 且目标为 Responses 时结构化保留。
- tools：`{type:function, name, parameters}` ↔ anthropic `input_schema` / chat `function.parameters`（`strict` 在 responses↔chat 两侧透传）。进入 Anthropic 的 schema 会去除 transport `encrypted` marker、保证根 `type:object` + `properties`，并展平根 oneOf/anyOf/allOf；字面名为 `encrypted` 的 property 保留。`web_search_*` 按 hosted tool 映射；Anthropic 普通 function tool 的显式默认 type `custom`（及缺省 type）按 name/input_schema 常规映射，不当 hosted tool 拒绝；仍不支持的 `computer_*` 等丢弃 + `convertWarn`。**工具声明合并（codex 0.145 实测）**：codex 把工具放在 input 的 `{type:"additional_tools", role:"developer", tools:[…]}` item 里（顶层可无 `tools`）——r→chat/r→a 统一经 `responsesRequestTools` 合并（顶层在前，additional_tools 按序追加），additional_tools item 是工具声明不是消息，不进消息流、不按 developer 消息折叠；ns restore map 与 custom 工具集合同样从合并后的声明重建。
- `max_tokens` ↔ `max_output_tokens`；chat `max_completion_tokens` 与 `max_tokens` 并存时取前者（chat→a、chat→r 均同）。`thinking.budget_tokens` ↔ `reasoning.effort`（best-effort 近似）；chat `reasoning_effort` ↔ responses `reasoning.effort`（chat→a 也映射到 `thinking`）。r→chat 输出侧只写 `max_tokens`（兼容性最好）。**r→a 缺失 `max_output_tokens`（或为显式 null，codex 客户端常态）时注入与 chat→a 相同的默认值 4096**——anthropic 对缺 max_tokens 必 400。
- chat `response_format` ↔ responses `text.format`（`json_object` 直通；`json_schema` 拆装一层 `json_schema` 包装；**空包装（无 schema）丢弃 + convertWarn**——裸 `{"type":"json_schema"}` 上游必 400；嵌套 `type` 不覆盖判别式）；anthropic 无对应，r→a 丢弃 + `convertWarn`。
- `parallel_tool_calls` ↔ anthropic `disable_parallel_tool_use`（responses 四方向均映射；r→a 在 tool_choice 缺省时合成 `{type:auto}`，无 tools 或 `none` 时不合成）。
- r→a 与 chat→a 一样保证首消息为 user（input 以 function_call 开头时插入占位 user）。
- r→chat 特有：连续 function_call 合并进**一条** assistant 消息（多 tool_calls）；**穿插在 function_call 与其 output 之间的 assistant 消息并入该 assistant tool-call 消息的 `content`（"\n\n" 连接）**，保证 tool_calls→tool 相邻性（严格上游拒绝不相邻的 tool 消息，Switchyard deferred-message 同款目标）；reasoning item 的文本附挂到**相邻 assistant 消息**的 `reasoning_content`（前向附到后续 assistant，尾部回溯附到前一条；DeepSeek 类上游要求带 tool_calls 的 assistant 消息必带 reasoning_content；无 assistant 时退化为独立消息）。**user/system 回合边界处 pending reasoning 立即回溯附挂到上一条 assistant（已有内容时 `\n\n` 追加），禁止跨 user 回合泄漏到下一轮 assistant；无可附挂的 assistant 时丢弃 + convertWarn**（cc-switch transform_codex_chat.rs:1012-1045）；system/developer 消息全部提到头部（保序，MiniMax 类上游拒绝 mid-thread system）；tools 为空（或全部被过滤）时丢弃 `tool_choice` 和 `parallel_tool_calls`（上游会拒绝引用不存在工具的参数，cc-switch #3557）。
- `stop`/`stop_sequences` 无 Responses 对应字段：→r 方向丢弃 + `convertWarn`。

## 流式映射（Responses 方向）

客户端 `stream` 是跨协议输出契约：成功响应若上游模式相反，proxy 在 commit 前把 SSE 聚合成目标协议 JSON，或把目标协议 JSON 合成为合法 SSE；转换/聚合失败返回 502。相同协议透传不做模式改写，保持既有字节和首包行为。

Responses → {anthropic, chat}（`responsesSSETo*`，读 `response.*` 事件）：

- `response.created` → anthropic `message_start` / chat 首 chunk（`role:assistant`）。
- `output_text.delta` → `text_delta` / `delta.content`。
- `function_call_arguments.delta` → anthropic `input_json_delta` / chat `delta.tool_calls[].function.arguments`（`output_item.added` 时发出 tool_use/tool_calls 块头）。无 delta、仅 done 帧带完整 arguments 时回退补发（done-only fallback）；同一 item 的重复 `output_item.added` 忽略不重置块。**done-only tool call 补建**：网关连 `output_item.added` 都省掉、只发 `output_item.done` 时，用 done 帧里的完整 item（name/call_id/arguments）补建整个 tool_use/tool_calls 块，不再整个丢弃（opencodex chat/outbound.ts 同款）。**delta 先于 added 到达**时按 `item_id`（缺省 `output_index`）缓冲，added/done 到达时回放，避免首段参数丢失、arguments JSON 损坏。`response.completed` 事件携带 `status:"failed"` 时按错误处理，不产出干净终态。
- `reasoning_summary_text.delta` → `thinking_delta` / `delta.reasoning_content`；reasoning item 的 `encrypted_content`（done 帧）→ `signature_delta`（无 summary 时整块 → `redacted_thinking`）。
- `response.completed` → `message_delta`(usage) + `message_stop` / chat finish chunk + `[DONE]`；usage 取自 `response.usage`（含 cache/reasoning details，见 Usage 节）。completed 事件携带 `status:"failed"`/`"cancelled"` 或 error 非空时按错误处理，不产出干净终态（与非流式 fail-closed 对齐）。
- `response.incomplete` 读 `incomplete_details.reason`：`content_filter`→`refusal`/`content_filter`，其余→`max_tokens`/`length`；**usage 同样读取**（cc-switch completed/incomplete 统一取 usage——max_tokens 截断恰好最需要计费）。
- `response.refusal.delta` → `text_delta` / `delta.content`（refusal 正文是真实内容；stop 语义由 finish/stop_reason 携带，与 `internal/protocol/convert.go` 方向 refusal→text 对齐）；非流式 `{type:"refusal"}` content part 同样转 text。
- 上游 SSE 必须出现协议终止信号：Responses 为 `response.completed`/`response.incomplete`/`response.failed`，Anthropic 为 `message_stop`（已带 stop_reason 的 `message_delta` 可在 EOF 兜底；方言 `data: [DONE]` 同为显式终止符——干净终态、合成 finish（若 `message_delta` 未发），其后帧不再扫描处理），Chat 为 `[DONE]` 或非空 `finish_reason`。无终止信号 EOF、scanner 错误或未闭合的 function arguments 一律 fail-closed：Anthropic 客户端收到 `error`，Chat 客户端收到 error chunk，Responses 客户端收到 `response.failed`；不得合成 `message_stop`/`[DONE]`/`response.completed`。

方言兼容（真实流量录制发现）：

- r→{a,chat} 的 SSE reader 在无 `event:` 行时回退用 payload 的 `type` 分派（OpenRouter 系网关如 aqp 的 `/responses` 完全不发 event 行；缺失时整流内容会被静默丢弃）。
- `response.reasoning_text.delta`（OpenRouter 方言，reasoning 走 content part 而非 summary）与 `reasoning_summary_text.delta` 同等处理。
- chat 源的 reasoning 方言按 cc-switch codex_chat_common 的穷举顺序提取（非流式 message 与流式 delta 同一 `chatReasoningText`）：`reasoning_content` > `reasoning`（字符串，或 `{content,text,summary}` 对象）> `reasoning_details`（OpenRouter 系数组/对象，取各项 text/content/summary，`\n\n` 连接；encrypted 项自然跳过）。
- **内联 `<think>…</think>` 拆分（chat→r，MiniMax 类上游把 thinking 内联在 content）**：只拆**前导**块（前导空白容忍）——非流式拆成 reasoning item + 去掉 think 块的正文；流式用 detecting/reasoning/text 三态缓冲逐 chunk 判定（标签可跨 chunk），未闭合的前导块整段按 reasoning 收尾；正文中间的 `<think>` 永远原样保留（cc-switch split_leading_think_block / streaming 同款）。
- r→chat 请求侧 `reasoning.context`（codex 发 `"all_turns"`）无 chat 对应：丢弃 + convertWarn。

{anthropic, chat} → Responses（`*ToResponsesSSE`，合成 `response.*`）：

- `message_start`/首 chunk → `response.created`；content 块增量组装回 `output_item.added` + 对应 `*.delta` + `output_item.done`（added/done 按 item id + type 严格配对；从未发过 added 的空 text/thinking 块不发任何 done 帧）。**`output_item.done` 帧与 `response.completed.output` 携带完整 item**（message 带 content、function_call 带 name/call_id/arguments、reasoning 带 summary/encrypted_content、custom_tool_call 带 input，对齐真实上游 framing，cc-switch streaming_codex_chat 同款）。
- 合成 item id：`msg_item_<idx>` / `fc_item_<idx>` / `rs_item_<idx>`。chat 侧 tool_call 的 `output_item.added` 延迟到 name 已知才发（空 name 是协议违规），且按连续 output_index 释放（前面的匿名 call 不跳过）；id/name 后到的碎片不覆盖已有 identity。
- `message_stop`/finish → `response.completed`（带 usage）；status 为 `incomplete` 时事件名为 `response.incomplete` 并带 `incomplete_details.reason`（`max_tokens`/`length`→`max_output_tokens`，`refusal`/`content_filter`→`content_filter`）。**每个合成帧带递增 `sequence_number`（从 0 开始每帧 +1，对齐真实上游 framing）。** **合成的 completed/incomplete 快照带 `created_at`（Unix 秒整数，Responses 线上契约；JSON→SSE 桥接仅在上游体缺失时补齐）**——严格 SDK 解析必填。
- anthropic `redacted_thinking` 块 → 仅含 `encrypted_content` 的 reasoning item。
- 上游 error event / chat error chunk → `response.failed`，绝不合成干净的 `response.completed`；chat 源流显式 `event: error` 行同样判错（payload 无 error 键时从 message/detail 提取，cc-switch extract_chat_sse_error）。

## 请求映射（anthropic ↔ openai-chat，既有不变）

- `tools` / `tool_choice` 双向映射，`any ↔ required`。
- assistant `tool_use` ↔ OpenAI `tool_calls`，arguments 使用 JSON 字符串。
- `stop_sequences` ↔ `stop`（数组）；chat 的单字符串 `stop`（OpenAI 合法形态）
  包装为单元素数组，不再被静默丢弃；chat→r 方向字符串形态同样计入丢弃告警。
- user `tool_result` 转成独立 `role: tool`，其余 text/image 聚合回 user。
- 连续 tool 消息合并到同一 user turn。
- Anthropic role 必须交替；首消息不是 user 时插入占位。
- image ↔ `image_url` data URL；chat→a 方向 data URI 只认 `;base64,` 形态（media_type 取 `;base64,` 前的**裸 MIME**，charset 等参数截断），非 base64 的 percent-encoded data URI 丢弃 + `convertWarn`（无 Anthropic 等价 source 形态）。
- `parallel_tool_calls` ↔ `disable_parallel_tool_use`。
- `tool_choice=none` 不携带 disable_parallel_tool_use。
- chat `max_completion_tokens` 优先于 `max_tokens`；`reasoning_effort` → `thinking`（best-effort budget 阶梯，与 responses 方向共用）。
- 单 part 消息仅当该 part 是 text 时才简化为字符串；单个 image part（无 `text` 键）保留数组，否则 content 坍缩为 nil 静默丢图（user/assistant 两分支同规则，cc-switch 同款）。
- chat→a 的 system/tool 消息 content 为 parts 数组时抽取 text part（块间 "\n" 连接），content 缺失 → 空串（绝不产生字面量 `"null"`）；多条 system 消息以 "\n" 拼接。a→chat 的 system 多文本块同样以 "\n" 连接。
- 响应 refusal：content part `{"type":"refusal","refusal":...}` 与 message 级 `refusal` 字段（含流式 `delta.refusal`）映射为 text block（cc-switch 同款）；stop/finish 语义映射已有。
- 非流式 chat 响应 `choices` 为空按上游失败处理（转换报错 fail-closed，对齐 intentional-behaviors #1「空 200 视为模型失败」），不再合成 `content:[]` + `end_turn` 的合法空消息；**有 choices 但无任何 content/refusal/tool_calls 时合成单个空 text 块**（空 content 数组不是合法 anthropic 消息，与 r→a 同款）。
- 跨协议 HTTP 4xx 不进入成功响应转换器：`convertErrorResponse` 保留 status、message、code/param 与 request_id，并翻译为客户端错误 envelope；无 `error` 键的裸 `{"type":"error",...}` 信封按顶层字段转换（字面 `"error"` 不当错误类型，回退 status 推导）；无法解析或无法识别的错误体在 commit 前转 502。Responses 非流式 `status:"failed"|"cancelled"` 即使缺少 `error` 对象也必须转换失败，不得合成空成功响应。
- 响应 `reasoning_content` ↔ thinking block（非流式置于 text 前；流式 `thinking_delta` 独立 block 生命周期；多个 thinking 块合并为 reasoning_content 时以 `\n\n` 分隔）。stop 语义：`content_filter ↔ refusal`；`pause_turn`→`stop`（best-effort）。

## 流式映射（anthropic ↔ openai-chat，既有不变）

OpenAI → Anthropic：

- 为每个 OpenAI index 分配 block index；
- `input_json_delta` 原样传递；
- 并行工具调用交错时整块缓冲，按顺序完整结束；
- finish 延迟到 trailing usage；
- error chunk 转成 Anthropic error event。

Anthropic → OpenAI：

- 只有当前 block 是 `tool_use` 时才发送 tool argument delta；
- 空 arguments 补 `{}`；
- finished gate 防止重复 finish；
- error event 转成 OpenAI error chunk。

## Tool ID

OpenAI → Anthropic 的 tool use id 必须满足 `^[a-zA-Z0-9_-]+$`。同一调用内 tool_use/tool_result 使用 memo 保持成对映射；纯函数映射必须确定性。**同一请求内两个不同原始 id 清洗后碰撞**（如 `call.a` 与 `call_a` 都得 `call_a`）时，按出现顺序加 `_2`/`_3` 后缀去重，保持 use/result 配对（Switchyard FNV-1a 后缀解决同一问题）。

可选字段（call_id/name/arguments/model/id/stop_reason 等）缺失时**绝不产生字面量 `"null"`**——取值统一用 `strOpt`/`strKey`（`strOf(nil)` 会渲染成 `"null"` 字符串并击穿 `firstNonEmpty` 回退）。缺失时的具体行为：function_call `arguments` 缺省 `"{}"`（空串会让下游 `JSON.parse("")` 失败）；chat→r 请求里 `name` 缺失时按 `call_id` 从同请求较前项回填（replace-style 客户端只回传 id）并 `convertWarn`；流式合成缺失 tool id 时回退到合成的 item id（`fc_item_<idx>`）。

## Usage

- Anthropic → OpenAI 请求注入 `stream_options.include_usage`；r→chat 的流式请求同样注入（kimi/MiniMax 类上游否则流式 usage 全 0）。
- OpenAI → Anthropic：`input = prompt − cached − cache_creation`，clamp 到非负，并设置 cache read/creation。cache_creation 识别直传 `cache_creation_input_tokens` 与 `prompt_tokens_details.cache_write_tokens` 两种拼写（直传优先），cache write 必须从 input 双减，否则在 input 与 cache 桶重复计数。流式路径同语义。
- Anthropic → OpenAI：prompt 包含 input、cache read、cache creation，并输出 `prompt_tokens_details.cached_tokens`。
- Responses 方向同一约定（OpenAI inclusive）：a→r 的 `input_tokens` = input + cache_read + cache_creation，`cache_read` → `input_tokens_details.cached_tokens`；r→a 反向拆分（clamp 非负）——cache write 同样双减：r→a 识别直传 `cache_creation_input_tokens`（优先）与 `input_tokens_details.cache_write_tokens`（兜底），产出 `cache_creation_input_tokens`，`input = input − cached − cache_write`，流式路径同语义；chat↔r 双向透传 `cached_tokens`（`prompt_tokens_details`↔`input_tokens_details`）和 `reasoning_tokens`（`completion_tokens_details`↔`output_tokens_details`）。流式路径同语义（a→r 从 `message_start`/`message_delta` usage 按字段存在性累计，不用缺省值覆盖）。

## 有损字段

SSE 读取按规范折叠多行 `data:`（连续 `data:` 行在分派空行处以 `\n` 拼接为一个 payload，六个流式转换器与 `parseWireSSE` 聚合路径一致；无尾空行的尾帧在 EOF 分派）。LLM 上游实践全部单行，多行仅出现在方言网关。对漏写帧间空行、导致多个完整 JSON payload 被折叠到同一帧的非规范网关，解析器逐 data 行恢复；每个恢复帧使用该 data 行读取时对应的 `event:`，不能把最后一个 event 套给全部 payload，遇到协议终态后不再处理同一折叠帧中的后续数据。

剩余有损项主要是尚未实现降级的 server tools（如 computer）、未知 role/content 和 chat `input_audio`。已知无法表达的请求特性优先由 capability scanner 拒绝；仅响应侧或可安全降级的差异使用 `convertWarn`。document/file、hosted web/tool search、tool_result 图片/错误标记、reasoning replay 和引用均已保留或采用明确降级。跨协议目标为 Anthropic 时，会在 system、最后一个 tool、最后一条 user content 注入 ephemeral cache breakpoint；原协议 cache_control 仍不逐点一一映射。

涉及 responses 的方向（`internal/protocol/convert_responses.go`）：reasoning、citations、document/file、工具与 usage 按上述规则保留。多模态**输出**仍有损：Responses 协议没有通用 image output item，上游返回的图片输出会告警。`stop`/`stop_sequences`→r 和 `text.format`→a 无对应。`top_k`、`seed`、penalties、metadata、service_tier、store 等非核心提示不跨协议；其中 metadata.user_id 仅以哈希形式用于 `prompt_cache_key`。

reasoner/thinking/MiMo 等需要 reasoning replay 的模型在 anthropic↔openai-chat 转换路由上仍会丢 thinking（replay cache 未实现）；经 responses 方向可保 reasoning。`ConfigRoutingWarnings` 只负责警告，不代表已实现 replay cache。

## 结构化转换诊断（diagnostics.go）

请求侧的 convertWarn 站点已迁移为 `warnDiag(d, code, msg)` 双写：日志行为不变，同时按稳定
code 收集进 `RequestOptions.Diag`（`targetexec.Plan` 每次尝试携带一个收集器，经
`Scope.Log.Diagnostics` 落入 request log 的 `diagnostics` 字段）。code 清单：`stop_dropped`、
`response_format_dropped`、`empty_json_schema_dropped`、`unknown_role_dropped`、
`unknown_block`/`unknown_part`/`unknown_item`/`unknown_tool_type`、`non_text_block_dropped`/
`non_text_part_dropped`、`server_tool_dropped`、`block_dropped`、`cache_control_dropped`、
`data_uri_dropped`、`file_id_degraded`、`media_degraded`（目标无视觉时 tool 结果图片塌缩为
占位文本——strict 模式同样拒绝，能力门控不再是无感盲区）、`tool_args_raw`/`tool_args_wrapped`、
`orphan_reasoning_dropped`、`reasoning_dropped`、`reasoning_context_dropped`、`tool_name_missing`。
`StrictLossy` 未配 `Diag` 时 `convertRequestFor` 自动补一个收集器（否则静默失效）。
响应/流式路径与 responses_state 的孤儿修复仍走 convertWarn（Phase 2）。

**strict 模式**（config `conversion.strict_lossy`，默认关）：任一诊断触发即以
`unsupportedConversionError`（`Feature: strict_lossy:<codes>`）拒绝该次转换——完全复用
capability scanner 的 target 跳过与 400 信封通道，proxy 无特判。strict 只作用于请求侧
（提交后的响应转换无法回退，failover 无意义）。拒绝集就是**全部**已收集 code 的集合，
包括语义上无损的项（`cache_control_dropped`、`tool_args_wrapped` 等）：strict 的契约是
“零诊断”而非“零数据损失”，属有意设计；新增诊断 code 默认进入拒绝集。

## 接线要求

- 跨协议请求的后处理(Anthropic cache breakpoint 注入、图片缩减、codex 参数剥离)共享同一次 decode/encode(UseNumber 保留数字字面量,防大整数雪球 id 损坏);树未变更时保留 pair 转换器的原始字节。
- 响应转换必须是最内层 reader；logger、usage scanner、cache 只能看到客户端协议字节。
- 非流式响应在 commit header 前转换，失败返回 502，禁止提交错误协议 body。上游 content-type 缺失/非 `text/event-stream` 时先嗅探帧格式（`event:`/`data:`/`id:`/`retry:` 字段或 `:` comment heartbeat；codex 实测空 CT 流式响应），按流式路径转换；usageScanner 同样吃嗅探结果。嗅探对所有 <300 响应执行（透传路径的 usageScanner/responses-state 也依赖它），但只保证读满首个可用数据块：首块已能定判（如 `: ping` 心跳）就立即按 SSE 处理，不再为填满 16 字节窗口而阻塞；仅当首块是空白或标记被截断（`ev`+`ent:`）时才继续读满窗口。
- 跨协议请求内联图片默认限制为 4 MiB / 4096px；上游返回 413 时仅重试一次，以 1 MiB / 2048px 重新编码 JPEG。非图片字段不改写，无法解码或超过 64 MiB 的图片不在请求路径展开。
- 非流式读取上限 64 MiB。
- SSE 单行上限 8 MiB，超限要记录错误。
- 客户端断开后停止读取上游。

## 回归测试

以下纯 codec/state/framing 测试位于 `internal/protocol/`；名称以
`TestForward_` 开头的 Proxy 接线测试仍位于根包，防止 codec 正确但 transport
接线错误。

- `TestProtocolConversionRegistryIsComplete` 断言六组跨协议 pair 均同时注册
  request、response、stream codec；未知协议与同协议不得命中注册表。
- 同协议逐字节透传（`TestConvertFault_SameProtocolPassthrough`：anthropic/responses 两方向请求与响应均 byte-identical，端到端）。
- tools、并行工具、图片、usage 双向转换（anthropic↔openai-chat）。
- 交错 tool delta 和 trailing usage。
- 转换失败发生在 commit 前。
- logger/cache 捕获客户端协议而非上游协议。
- responses 方向：4 个请求 + 4 个响应 + 文本/工具流式转换（`internal/protocol/convert_responses_test.go`），以及 anthropic/chat 客户端经 `protocol:responses` 目标的端到端（`TestForward_*ToResponses_NonStream`）。
- 流式 6 方向均已覆盖：responses→{a,chat}、{a,chat}→responses 和 anthropic↔chat；`internal/protocol/convert_responses_stream_test.go` 用 `drainSSE`（`internal/protocol/convert_sse_test.go`）断言事件**序列**、合成 item id（`msg_item_/fc_item_/rs_item_`）和 added/done 配对，含并行交错 tool_calls、reasoning 流、EOF fail-closed 与错误事件。跨方向 EOF 矩阵和未闭合 tool arguments 见 `internal/protocol/convert_review_fix_test.go`。
- 空/错误 content-type 的 framing 嗅探见 `internal/protocol/stream_mode_test.go`：除 event/data 外覆盖 id/retry/comment heartbeat、分段 marker、heartbeat 不阻塞，以及 heartbeat 后跨协议端到端流式转换，防止误入非流式 JSON 分支。
- reasoning/encrypted_content 映射（`internal/protocol/convert_reasoning_test.go`）：`thinking`+`signature` ↔ reasoning `summary.text`+`encrypted_content` ↔ chat `reasoning_content`，请求/响应/流式三层 + `budget_tokens`↔`effort`。
- 容错与终态（`internal/protocol/convert_fault_test.go`）：未知事件/畸形 JSON 帧跳过、`response.failed`/`response.incomplete` 分支、重复 `response.completed` 终态唯一、非 JSON/非对象 arguments 保真包装（`{"raw":…}`/`{"value":…}`，`parseToolArgs`）、孤儿 tool 对、末帧无尾换行、8MiB 行上限告警、responses 方向图片映射、未知 block/part/item/event 的 `convertWarn` 全覆盖（J）。
- Switchyard 移植用例（`internal/protocol/convert_borrowed_parity_test.go`）：done 帧全量 arguments 与 delta 流不重复（r→a/r→chat 双向）、裸 `message_stop` 不重置 max_tokens 终态、敌意 tool id 的 use/result 一致清洗、data-URI 变体（charset 参数取裸 MIME、非 base64 丢弃告警）、穿插 assistant 消息保持 tool_calls→tool 相邻、同一逻辑响应的缓冲与流式转换语义等价。
- 回环矩阵（`internal/protocol/convert_roundtrip_matrix_test.go`）：请求 2-hop（a→chat→a、a→r→a）与 3-hop（a→r→chat→a、a→chat→r→a）语义投影守恒（system/model/max_tokens/texts/tool_use/tool_result 有序等价），以及文本+usage 响应的 a→chat→a 回环。
- Switchyard 移植第二波（`internal/protocol/convert_p1_parity_test.go`）：a→chat finish 的 `total_tokens` 重算、anthropic 方言源以 `[DONE]` 终止（含其后垃圾帧不泄漏）、SSE 解析器健壮性（1 字节分片喂入含跨 chunk UTF-8、CRLF 帧、语义级逐帧等价比较）、legacy `function`/未知 role 丢弃必告警、json_schema 边角（空包装丢弃告警、嵌套 type 不覆盖判别式）、空 chat 响应合成空 text 块、tool id 清洗碰撞去重、合成 completed 快照的必填字段完备性（id/object/created_at/status/model/output/usage）、kitchen-sink 未知 part/block/item 三方向不泄漏且已知内容存活。
- 三向审计补齐的映射（`internal/protocol/convert_gap_test.go`）：usage cache/reasoning details 四方向（流式+非流式）、`content_filter`/`refusal`/`incomplete_details.reason` 语义、failed/cancelled fail-closed、hosted web_search 保留与 unsupported server tools 过滤、r→a 首消息占位、`max_completion_tokens`、`response_format`↔`text.format`、responses 四方向 `parallel_tool_calls`；`redacted_thinking` 与 chat→a reasoning 见 `internal/protocol/convert_reasoning_test.go`。
- tool_result 媒体改投（`internal/protocol/convert_media_test.go`）：a→chat / a→r / r→chat 三方向各覆盖纯图、多图（含 url source）、无图回归；视觉门控见 `internal/protocol/convert_fixups_test.go`（占位文本、能力查找顺序、三方向无视觉路径）。
- 占位 reasoning_content 与 sequence_number（`internal/protocol/convert_fixups_test.go`）：thinking 方言注入/不覆盖/不越界注入，两个合成方向 0..N 连续递增。
- MCP namespace 与 hosted fallback（`internal/protocol/convert_namespace_test.go`）：经 Chat/Anthropic target 的压平、历史 call + tool_choice、无 namespace no-op、撞名 fail-closed、restore map 反查、非流式/流式 round-trip、未命中透传、80 字符截断一致、流式 added 帧还原，以及 web_search/tool_search function fallback。
- custom/freeform 工具（`internal/protocol/convert_custom_tool_test.go` + `internal/protocol/convert_parity_test.go`）：定义/历史包装、非流式解包（含 parse 失败兜底）、流式渐进解包（前缀跨 chunk、`\n`/`\"`/`\uXXXX` 跨 chunk 截断）、cc-switch `restores_custom_tool_input_stream_events` 移植、reasoning effort 四方言 + 池虚拟名归一。
- responses 方向 P0 修复：r→a 缺省/显式 null `max_output_tokens` 注入默认 max_tokens、r→chat 流式注入 `stream_options.include_usage`、system/developer role 双向折叠（r→a 顶层 system / a→r instructions）、a→r 孤立 reasoning item 丢弃（`internal/protocol/convert_responses_test.go`）；流式 done-only tool call 补建与 delta 先于 added 的缓冲回放（`internal/protocol/convert_parity_test.go` 的 `TestParity_DoneOnlyToolCallSynthesized` / `TestParity_ArgsDeltaBeforeAdded`）。
- responses 方向 P1 修复：a→r `reasoning.summary:"auto"` + codex `include:["reasoning.encrypted_content"]`（后者在 `internal/provider/provider_usecase_test.go`）、reasoning 方言穷举（reasoning 对象/reasoning_details，流式+非流式）、r→chat reasoning user 边界回溯、合成流 done/completed 帧完整 item + 空块无 done、r→a cache_write 双减、r→a tool 输出 parts 数组拆块、流式 incomplete 读 usage、流式 completed cancelled/error fail-closed、refusal 正文保留、tool_use 缺 input arguments 兜底 `"{}"`、chat→r 流式 `event: error` 判错。
- responses 方向第三批（codex 0.145 抓包驱动）：additional_tools 合并（`TestConvertResponsesRequestTo{OpenAI,Anthropic}_AdditionalTools`）、namespace 容器展开（`TestNSFlatten_NamespaceContainer` / `TestNSRestoreMap_NamespaceContainerInAdditionalTools`，含嵌套 fail-closed 与 field 形同名）、adaptive thinking/output_config.effort（`TestConvertAnthropicRequestToResponses_AdaptiveThinking` 三态+disabled）、内联 `<think>` 拆分（非流式 `TestConvertOpenAIResponseToResponses_InlineThink`、流式 `TestParity_ChatToResponses_InlineThinkStream` 含跨 chunk/未闭合/非前导）、prompt_cache_key 注入与透传（`TestConvertAnthropicRequestToResponses_PromptCacheKey`）、strict 透传（`TestConvertOpenAIRequestToResponses_ToolStrict`）、reasoning.context 丢弃告警（`TestConvertResponsesRequestToOpenAI_ReasoningContextWarns`）。
- codex 路由：`ProtocolHint("codex")=="responses"`,转发路径经 `resolvedBackendProto` 自动回退到该 hint,所以缺 `protocol:` 的显式路由也会自动转换(不再告警、不再生成 Chat Completions hint、不再带 WireProtocolNote)。
- 错误与整形审计回归（`internal/protocol/convert_review_fix_test.go`）：6 个跨协议方向 × 400/403/404 错误 envelope、端到端 4xx status/message、同协议 responses→codex `convertBody` 字节级透传不剥离（转换方向的 codex 预剥离见 `TestConvertGap_CodexStripsUnsupportedParams`）、全部 system/developer 折叠、普通消息 URL 图片、status-only failed/cancelled，以及 6 个流式方向 EOF fail-closed。
- 低危评审修复（同上测试文件 + `internal/protocol/convert_namespace_test.go`/`internal/protocol/convert_test.go`/`internal/protocol/convert_reasoning_test.go`/`internal/protocol/convert_responses_stream_test.go`）：流式 r→a 引用链接按 block 去重、hosted fallback 名两方向撞名 fail-closed、裸 `{"type":"error"}` 错误信封转换、nsFlattenName rune 边界截断、多 thinking 块 `\n\n` 分隔（请求/响应两方向）、chat→a parts content 的 message 级 annotations 追加来源链接、空 thinking 文本的 reasoning_details 项不回放。
- SSE 帧组装与终止符评审修复：`[DONE]` 作为 a→chat 显式终止符（其后已识别事件帧不处理、finish+`[DONE]` 不等上游 EOF、无 `message_delta` 时不追加 "terminated before a terminal event" 错误 chunk，`internal/protocol/convert_p1_parity_test.go`）；`appendSSEData` 空 data 行按 HTML spec 折叠（`data:`+`data: x` → `"\nx"`，帧开启与载荷为空的哨兵分离，`internal/protocol/convert_multiline_sse_test.go`）；responses 状态记录按折叠帧整体解析（多行 `response.output_item.done` 不再逐行丢失，`internal/protocol/responses_state_test.go`）；`convertWarn` 去重集合 1024 上限（防客户端可控内容无限增长，超帽不去重但仍记录，`internal/protocol/convert_polish_test.go`）；chat→r 的 file_id 与 file_url/file_data 并存时保留源、仅丢弃 file_id 并发 `file_id_degraded` 诊断（`internal/protocol/convert_protocol_completeness_test.go`）。
- 无 type 的 message 简写项归一化（`internal/protocol/convert_responses_test.go` 的 `TestConvertResponsesRequest_TypesLessMessageItems`）：`{role, content}` 简写（含字符串 content）在 r→a/r→chat 两方向转为正常消息，不再被当未知 item 丢弃。
