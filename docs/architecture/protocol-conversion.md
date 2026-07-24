# 协议转换契约

## 适用范围

修改 `convert.go`、`provider/protocol_hint.go`、跨协议 route、流式转换或工具调用映射时必读。

## 边界

目标未声明 `protocol:` 时，上游协议等于客户端协议，必须字节级透传。只有目标显式声明的协议（`anthropic` / `openai` / `responses`）与客户端不同，才启用转换。

三个协议值：

- `anthropic` = Anthropic Messages（`/v1/messages`，`messages` + content blocks）
- `openai` = OpenAI Chat Completions（`/v1/chat/completions`，`messages` + `tool_calls`）。**不再**表示 Responses API。
- `responses` = OpenAI Responses API（`/v1/responses`，`input` list + `output` items + `response.*` 流式事件）。`/v1/responses` 路径在 `protocolForPath` 里独立判为 `responses`，不再并入 `openai`。

转换器为直连 pairwise（`convert.go` 的 anthropic↔openai 不动；`convert_responses.go` 新增 4 方向 × {请求, 响应, 流式}）。`needsConversion` 对任意两个不同的已知协议返回 true；未知协议值 fail-safe 不转换。

Codex 后端只接受 Responses API，因此 `ProtocolHint("codex") = "responses"`。转发路径在目标未声明 `protocol:` 时经 `resolvedBackendProto` 自动回退到 `ProtocolHint`（forward/fusion/shadow 共用），所以 anthropic/chat 客户端打 codex 路由会**自动转换**,无需用户写 `protocol: responses`(显式声明仍可,且优先级最高)。codex 不再带 `WireProtocolNote`、不再告警。 Responses 转换无状态：丢弃 `previous_response_id`，历史全部靠 `input` 列表显式携带（等价于 messages）。

## 请求映射（Responses 方向）

- anthropic `system` ↔ responses `instructions`；chat 首条 system/developer message → `instructions`。
- `messages` ↔ `input` items：text block ↔ `{type:message, content:[{input_text|output_text}]}`；image ↔ `{input_image, image_url:data:...}`。
- anthropic `tool_use` / chat `tool_calls` → responses `{type:function_call, name, arguments, call_id}`；tool `id` ↔ `call_id`（`function_call_output` 的 `call_id` 必须回填）。
- anthropic `tool_result` / chat `role:tool` → responses `{type:function_call_output, call_id, output}`。
- anthropic `thinking` ↔ responses `{type:reasoning, summary, encrypted_content}`；`thinking`↔`summary.text`，`signature`↔`encrypted_content`。
- tools：`{type:function, name, parameters}` ↔ anthropic `input_schema` / chat `function.parameters`。
- `max_tokens` ↔ `max_output_tokens`；`thinking.budget_tokens` ↔ `reasoning.effort`（best-effort 近似）；chat `reasoning_effort` ↔ responses `reasoning.effort`。

## 流式映射（Responses 方向）

Responses → {anthropic, chat}（`responsesSSETo*`，读 `response.*` 事件）：

- `response.created` → anthropic `message_start` / chat 首 chunk（`role:assistant`）。
- `output_text.delta` → `text_delta` / `delta.content`。
- `function_call_arguments.delta` → anthropic `input_json_delta` / chat `delta.tool_calls[].function.arguments`（`output_item.added` 时发出 tool_use/tool_calls 块头）。
- `reasoning_summary_text.delta` → `thinking_delta` / `delta.reasoning_content`。
- `response.completed` → `message_delta`(usage) + `message_stop` / chat finish chunk + `[DONE]`；usage 取自 `response.usage`。

{anthropic, chat} → Responses（`*ToResponsesSSE`，合成 `response.*`）：

- `message_start`/首 chunk → `response.created`；content 块增量组装回 `output_item.added` + 对应 `*.delta` + `output_item.done`。
- 合成 item id：`msg_item_<idx>` / `fc_item_<idx>` / `rs_item_<idx>`。
- `message_stop`/finish → `response.completed`（带 usage）。

## 请求映射（anthropic ↔ openai-chat，既有不变）

- `tools` / `tool_choice` 双向映射，`any ↔ required`。
- assistant `tool_use` ↔ OpenAI `tool_calls`，arguments 使用 JSON 字符串。
- user `tool_result` 转成独立 `role: tool`，其余 text/image 聚合回 user。
- 连续 tool 消息合并到同一 user turn。
- Anthropic role 必须交替；首消息不是 user 时插入占位。
- image ↔ `image_url` data URL。
- `parallel_tool_calls` ↔ `disable_parallel_tool_use`。
- `tool_choice=none` 不携带 disable_parallel_tool_use。

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

OpenAI → Anthropic 的 tool use id 必须满足 `^[a-zA-Z0-9_-]+$`。同一调用内 tool_use/tool_result 使用 memo 保持成对映射；纯函数映射必须确定性。

## Usage

- Anthropic → OpenAI 请求注入 `stream_options.include_usage`。
- OpenAI → Anthropic：`input = prompt - cached`，clamp 到非负，并设置 cache read。
- Anthropic → OpenAI：prompt 包含 input、cache read、cache creation，并输出 `prompt_tokens_details.cached_tokens`。

## 有损字段

anthropic↔openai-chat 方向：thinking、cache_control、server tools、tool_result 内图片、logprobs、未知 role 当前会丢弃，并通过 `convertWarn` 每进程每消息类型告警一次。流式 thinking_delta 同理丢弃。

涉及 responses 的方向（`convert_responses.go`）：**reasoning 保留**——responses `reasoning`（summary + encrypted_content）↔ anthropic `thinking`（thinking + signature）↔ chat `reasoning_content`。reasoning.effort 只单向到达 chat（`reasoning_effort`），anthropic 侧靠 `thinking.budget_tokens` best-effort 近似。多模态输出有损（responses 无 image output，用 `[Image]` 占位）。

reasoner/thinking/MiMo 等需要 reasoning replay 的模型在 anthropic↔openai-chat 转换路由上仍会丢 thinking（replay cache 未实现）；经 responses 方向可保 reasoning。`configRoutingWarnings` 只负责警告，不代表已实现 replay cache。

## 接线要求

- 响应转换必须是最内层 reader；logger、usage scanner、cache 只能看到客户端协议字节。
- 非流式响应在 commit header 前转换，失败返回 502，禁止提交错误协议 body。
- 非流式读取上限 64 MiB。
- SSE 单行上限 8 MiB，超限要记录错误。
- 客户端断开后停止读取上游。

## 回归测试

- 同协议逐字节透传。
- tools、并行工具、图片、usage 双向转换（anthropic↔openai-chat）。
- 交错 tool delta 和 trailing usage。
- 转换失败发生在 commit 前。
- logger/cache 捕获客户端协议而非上游协议。
- responses 方向：4 个请求 + 4 个响应 + 文本/工具流式转换（`convert_responses_test.go`），以及 anthropic/chat 客户端经 `protocol:responses` 目标的端到端（`TestForward_*ToResponses_NonStream`）。
- codex 路由：`ProtocolHint("codex")=="responses"`,转发路径经 `resolvedBackendProto` 自动回退到该 hint,所以缺 `protocol:` 的显式路由也会自动转换(不再告警、不再生成 Chat Completions hint、不再带 WireProtocolNote)。

