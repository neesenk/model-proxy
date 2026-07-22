# 协议转换契约

## 适用范围

修改 `convert.go`、`provider/protocol_hint.go`、跨协议 route、流式转换或工具调用映射时必读。

## 边界

目标未声明 `protocol:` 时，上游协议等于客户端协议，必须字节级透传。只有目标显式声明 `anthropic` 或 `openai` 且与客户端不同，才启用转换。

当前 `openai` 表示 Chat Completions，不表示 Responses API。Codex 后端只接受 Responses API 的 `/responses` 和 `input` list，因此：

- 不得给 codex 返回错误的 `ProtocolHint("openai")`；
- 使用 `WireProtocolNote` 明确提示协议缺口；
- 只有原生讲 Responses 的客户端可直接使用 codex；
- 在实现 Responses 转换前，不得把 Anthropic/Chat 请求伪装成可转换。

## 请求映射

- `tools` / `tool_choice` 双向映射，`any ↔ required`。
- assistant `tool_use` ↔ OpenAI `tool_calls`，arguments 使用 JSON 字符串。
- user `tool_result` 转成独立 `role: tool`，其余 text/image 聚合回 user。
- 连续 tool 消息合并到同一 user turn。
- Anthropic role 必须交替；首消息不是 user 时插入占位。
- image ↔ `image_url` data URL。
- `parallel_tool_calls` ↔ `disable_parallel_tool_use`。
- `tool_choice=none` 不携带 disable_parallel_tool_use。

## 流式映射

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

thinking、cache_control、server tools、tool_result 内图片、logprobs、未知 role、流式 thinking_delta 当前会丢弃，并通过 `convertWarn` 每进程每消息类型告警一次。

reasoner/thinking/MiMo 等需要 reasoning replay 的模型不得通过当前转换路由。`configRoutingWarnings` 只负责警告，不代表已实现 replay cache。

## 接线要求

- 响应转换必须是最内层 reader；logger、usage scanner、cache 只能看到客户端协议字节。
- 非流式响应在 commit header 前转换，失败返回 502，禁止提交错误协议 body。
- 非流式读取上限 64 MiB。
- SSE 单行上限 8 MiB，超限要记录错误。
- 客户端断开后停止读取上游。

## 回归测试

- 同协议逐字节透传。
- tools、并行工具、图片、usage 双向转换。
- 交错 tool delta 和 trailing usage。
- 转换失败发生在 commit 前。
- logger/cache 捕获客户端协议而非上游协议。
- Codex route warning，不生成 Chat Completions hint。

