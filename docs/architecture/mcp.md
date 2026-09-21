# MCP 网关

## 适用范围

修改 config `mcp:` / `mcp_routes:` 节、`internal/mcp`、`internal/app/proxy_mcp*.go`、
`internal/cli/mcp`、request log 的 `kind` 字段、`/mcp/` 端点行为、takeover 的 mcp
模板块或 Web MCP 面时必读。设计背景与实测结论见
`docs/research/design-mcp-gateway.md`；Web 字段契约见 `docs/web-api.md`。

## 功能

daemon 把 config `mcp:` 声明的远程 MCP（Model Context Protocol）服务暴露为本地
`/mcp/<name>` 端点（streamable HTTP），客户端（Claude Code `.mcp.json`、opencode、
Codex 等）指向代理而不是直接持有服务商凭据。两类后端：

- **provider 型**（`provider: <name>`）：复用该 provider 的账号池凭据，请求时从当次
  RuntimeSnapshot 的 providers 选择账号注入；多账号池展开为虚拟 provider 参与选择。
- **匿类型**（`auth: none`，或无 provider 的默认）：不注入任何凭据，面向公共端点。

凭据只存在于内存注入路径：不进 config、不进日志、不进事件。自定义鉴权头
（`auth_header`，如火山的 `X-Agent-Plan-Key`）经 `provider.KeyReporter` 窄缝取原始
key（apikey provider 限定；aqp/codex 在 config 校验期即拒绝，`auth: none` 配
`auth_header` 同为校验错误）。`Authorization`（默认）走 `AuthHeaders` 的 Bearer 注入。

## 端点与请求流

`/mcp/` 分支挂在 `Proxy.Handler` 的 `protocol.ForPath` 502 兜底之前，属 forward 面：
走与 LLM 端点相同的 api-keys 门禁（非 admin endpoint），body 上限复用
`max_request_body_bytes`。未知名称、disabled 服务器、嵌套路径答 **404**（不是 LLM 的
502）；非 POST/GET/DELETE 答 405。

- `POST /mcp/<name>`：JSON-RPC 透传。请求 body 全量读入（上限见上）；响应无论
  `application/json` 还是 `text/event-stream` 都**边收边 flush**，不缓冲到 EOF。POST
  交换受 `timeout:`（默认 300s）约束，计时器随响应 body Close 释放。
- `GET /mcp/<name>`：上游 SSE server-stream 透传（上游 405 则 405），不设超时。
- `DELETE /mcp/<name>`：会话终止透传 + 清本地会话条目。

每请求**一次** `RuntimeSnapshot` 捕获：mcp 配置、provider 实现、池身份与上游 client
（`mcpClientFor`：`mcp.<name>.proxy_url` → 所属 provider 的 `proxy_url` → 全局链）全部
取自同一代。header 过界由 `internal/mcp` 的双向窄白名单控制（客户端侧
Accept/Content-Type/MCP-Protocol-Version；响应侧 Content-Type/Cache-Control/
MCP-Protocol-Version + 改写后的 Mcp-Session-Id），凭据与 hop-by-hop 永不过界。
上游客户端（`mcpClientFor` 与 probe 共用策略 `mcp.CheckNoCrossOriginRedirect`）拒绝
**跨域重定向**：凭据头在首次发送前注入，跟随 3xx 到外域会泄 key；同源重定向正常跟随。

## 会话与账号

- 会话表归 `internal/mcp.SessionTable`：进程级跨代 LRU（512 会话、30 分钟空闲 TTL、
  **惰性回收**——无后台 goroutine，不进 lifecycle；先例 `internal/guard/session`）。
  本地 session id 由 proxy 铸造（crypto/rand 128-bit hex），永不序列化/落盘。
- 上游 `initialize` 响应带回 `Mcp-Session-Id` 时建立绑定
  `local → (server, account, upstream id)` 并改写响应头；后续请求按本地 id 粘到
  **同一账号**并还原上游 id。stateless 上游（实测 Firecrawl 无 session id）不铸造会话。
  已建会话上 re-initialize 且上游**轮换**了自己的 id 时，绑定刷新到新 id
  （`SessionTable.RefreshUpstream`），否则后续请求拿陈旧 id 必 404。
- 会话绑定 server：拿着 A server 的会话访问 B server 按未知会话 404。
- 无会话请求：池内 round-robin（进程级计数器）；**401 时一次**账号轮换重试（镜像
  `BufferedLeg` 的一次 401 语义），不写健康状态、不引入完整 failover。带会话的 401
  不轮换（换账号上游会话必失效），原样回客户端。
- reload 换代：条目存 account id；换代后账号仍在池内则继续，已移除则清会话并答
  **410 Gone**（fail-closed，客户端重新 initialize），绝不漂移到其他凭据。

## 观测

request log `Record.Kind`（`json:"kind,omitempty"`）：空 = LLM 流量（历史数据兼容），
`"mcp"` = 网关交换。MCP 记录的投影：`Protocol="mcp"`、`Method`=JSON-RPC method
（GET/DELETE 用 HTTP 动词，批量标注 `(batch)`）、`Path=/mcp/<name>`、`Tool`=tools/call
的客户端视角工具名（`json:"tool,omitempty"`，与 stats 的 tool 维度同一 `ParseToolCallName`
解析；非 call 方法与老记录为空/省略，summary 与尾随索引列同带）、`Exposed`=服务器
名、`Provider`=账号虚拟 id（匿型为空）、status/latency/收发 size 与 body 截断策略与
LLM 记录一致。MCP 交换进入 live events（`protocol="mcp"`，end 事件携带同源 `tool`
——start 发出时 body 尚未解析，见下文「统计、Live 事件与配额冷却」）。

**拆分流（`request_log.mcp_split`，默认关）**：开启时 `kind="mcp"` 记录改写入独立
的 `mcp-YYYYMMDD.log` 流（`mcp_dir`，默认 `~/.model-proxy/log/mcp`，策略值与
request_log 块共享；写入侧 `mcpLogTarget` 选流，截断 cap 取所选流的
`MaxBodyBytes`）。requests- 流与尾随索引回到 LLM-only，`/api/requests?kind=mcp`
经 admin 适配层改读拆分流（目录扫描，无索引），`Detail` 双流 fallthrough——语义
细节见 `docs/architecture/fusion-shadow-cache.md` 的 Request log 节。客户端每
thread 全量重握手（Codex 每 `thread/start` 对全部服务器 initialize +
notifications/initialized + tools/list）是常态，拆分流让这类探测噪音不再混入
LLM 请求日志与 Requests 页。

**来源标识（agent/session_id 投影）**：每条 MCP 记录携带客户端归属——`agent`
优先取 MCP 原生身份：initialize 帧的 `params.clientInfo.name`（`mcp.ParseClientInfo`
提取，会话铸造/重初始化时经 `SessionTable.SetClient` 绑定到本地会话，后续只带
会话 id 的请求也能归属），归一化到 `counters.AgentFromMCPClient` 的封闭标签集
（`codex-mcp-client`→`codex`，与 UA 面同集）；UA（`counters.DetectAgent`）仅作
兜底（GET 探测、无会话的预初始化交换；传输层 UA 如 `go-http-client` 会被
clientInfo 覆盖）。`session_id` 优先取 `request_log.session_headers` 允许列表里
客户端自带的会话头，否则用本地 `Mcp-Session-Id`（同一客户端 MCP 会话的交换
按会话分组）。路由会话同样绑定（proxy 自答 initialize 后 `SetClient`）。

## CLI

`model-proxy mcp list`（配置服务器表 + 路由表）与 `model-proxy mcp test <name>`（握手测活：
initialize + tools/list，经 `internal/mcp.Probe`）为离线诊断，出站走全局代理链
（与 `test` 相同的维护类调用语义）。`mcp test` 不接受路由名（路由是虚拟聚合面，
错误信息引导改测成员 server 或经 daemon 探测路由）。契约见 `CLI.md` §13b。

## 聚合路由（`mcp_routes:`）

路由与 server 共享 `/mcp/<name>` 命名空间（重名是 config 校验错误）。`serveMCP`
在 pinned 查找未命中后落入 `serveMCPRoute`（`internal/app/proxy_mcp_route.go`）。
与 pinned 透传不同，**proxy 拥有客户端会话**：

- `initialize` 由 proxy 自己应答（protocolVersion 回显客户端请求值，serverInfo 为
  `model-proxy route <name>`），铸造路由会话（`SessionTable.PutRoute`）。其余方法必须
  带路由会话：无会话 400、未知/过期/pinned 会话 404；GET 一律 405（聚合面无单一上游可流）。
- **子会话懒建**（`mcpRouteEnsureSub`）：首个需要某后端的调用才对其 initialize +
  notifications/initialized，各自独立选账号（池内 RR）、记录上游 session id 与**该后端
  协商的协议版本**（后续子会话请求以 `protoOverride` 覆盖 `MCP-Protocol-Version`，
  例如智谱实测协商到 2024-11-05）。并发同（session, server）可能重复 initialize，
  最后一次 `RouteSubPut` 生效——HTTP 后端无害（两个上游会话都有效）；stdio spawn 按
  registry key 单飞（`lockSpawn` + 锁内复查，败者不 fork），registry put 覆盖时 Close
  旧 conn 是兜底（锁外 Close，不串行化 registry）。
- `tools/list`：按 target 顺序聚合各后端工具，对外只暴露**规范名**（第一 target 的
  schema 胜出；获取失败的后端跳过，聚合面降级不整体失败），按路由会话缓存
  （`RouteToolsPut`）；工具等价是 config 声明的人工事实，v1 不做 schema/参数推导。
- `tools/call`：规范名 → 候选 target 集（声明该工具且 enabled，按声明顺序）→
  会话粘滞置顶（`RouteStickyGet/Put`，上一成功后端优先）→ `RewriteToolCallName`
  改写 params.name 后透传参数。**failover 只发生在传输/鉴权/会话/限流/服务端故障**
  （`mcp.FailoverStatus`：网络错误、401、404、429、5xx；401/404 同时 drop 子会话强制
  重建）；HTTP <300 的 JSON-RPC **业务错误原样返回、绝不 failover**。全部失败 502。
  候选集为空时区分两种语义：无人声明该工具 → -32602 unknown tool；声明者全部因
  配额窗耗尽被预降权跳过 → -32000 quota-exhausted（而非误报 unknown tool）。
- `DELETE`：向所有已初始化子会话转发 DELETE 后清本地会话；notifications 向已初始化
  子会话广播（fire-and-forget，客户端恒 202）；`ping` 本地应答；其他方法 -32601。
- 会话表路由态（子会话/粘滞/工具缓存）只能经 `SessionTable.RouteXxx` 方法访问
  （锁内拷贝语义），`Get` 永不暴露 `Route` 指针。

## P2 扩展

### legacy `sse` transport（pinned 专用）

`transport: sse` 的服务走 2024-11-05 的 HTTP+SSE 双通道：`GET /mcp/<name>` 打开上游事件
流，`internal/mcp.EndpointRewriter` 把首个 `endpoint` 事件的 POST URL 改写回
`/mcp/<name>?mps=<本地会话>`（逐行流式，绝不填满缓冲——SSE 流可在事件间空挂数分钟）；
会话表项的 UpstreamID 存**完整上游 POST URL**，POST 按 `?mps=` 还原并粘到 GET 时的账号。
GET 流结束即删会话（相关性随流消亡）。路由 target 引用 sse 后端在 config 校验期拒绝。

### stdio 后端（`transport: stdio`）

本地子进程后端（智谱 vision、火山豆包搜索形态）：`command` + `env`（值为
`${account.api_key}` 模板、`env:VAR` 进程环境引用，或良性键的字面量——模式开关类
配置直接写 config；凭据形键名（KEY/TOKEN/SECRET/PASSWORD/AUTH/CREDENTIAL 分段匹配）
的字面量在加载期拒绝，红线 3；基础环境最小化 PATH/HOME/TMPDIR/LANG，凭据注入走
`config.ResolveMCPStdioEnv`，daemon 与 CLI 共用）。`internal/mcp.StdioConn` 拥有子进程：
换行分隔 JSON-RPC、按 id 解复用、Close 杀进程并回收。进程模型是**每客户端会话一进程**
（MCP stdio 语义单客户端），pinned 挂本地会话 id、路由挂 (route session, server) 键；
`mcpStdioRegistry`（processServices，每 server 8 个上限）是 owner：会话表 `OnEvict`
（TTL/LRU/DELETE，锁外触发）联动 kill，`Proxy.Close` 开头 killAll（子进程不写日志）。
stdio 会话无上游 session id 概念；子进程死亡 → 502 + 丢会话让客户端重连。
stdio 交换同样受 `timeout:` 约束：`StdioConn.Call(ctx, ...)` 监听 ctx，挂起的子进程
不再永久阻塞调用方（pinned 转发/握手、路由子会话、probe、CLI 全接线）。
http 专属旋钮（url/headers/auth_header/proxy_url）对 stdio 一律校验拒绝；反之
`command`/`env` 只对 stdio 有效，出现在 streamable/sse 服务器上时校验失败。

### 出站扫描与静态头

- `guard.mcp_secrets`（log|redact|block|off，**默认 off**，与 LLM 面 secrets=log 不同）：
  pinned/route/stdio 的 POST body 在转发前过当代 Scanner 的秘密规则一次；审计只记模式名
  （kind=secret, protocol=mcp），命中内容永不入记录。
- `mcp.<name>.headers`：静态头只允许 `env:VAR` 间接引用（发送时解析，未设置即 fail-closed
  报错），字面量在校验期拒绝——第三方 keyed 服务（Tavily 等）的凭据永远不进 config。

### 统计、Live 事件与配额冷却

- 统计：每个终态交换（pinned/route/stdio 全覆盖）经 `mcpLog` 记录到 MCP 网关自己的
  进程级计数器 `observe/counters.MCPStats`（按 name 计 calls/errors/avg_latency_ms；
  errors = status ≥ 400），再经分钟 flusher 持久化到 `stats.db` 的 `mcp_buckets`
  表（`name, kind, minute, calls, errors, latency_ms_sum, last_call_at`）。
  `tools/call` 交换另记 **tool 维度**：`mcpLog` 对原始客户端 body 跑
  `mcp.ParseToolCallName`（同一次解析同时供给 stats、request log 记录的 `Tool`
  字段与 live end 事件的 `tool`），非空 tool 名时 `RecordTool(name, tool, …)`（route 的
  account 双计规则同 server 级），flusher 经 `DiffMCPTools` 持久化到
  `mcp_tool_buckets`（`name, tool, kind, minute, …`，PK `(name, tool, minute)`）。
  tool 名为**客户端视角**——route 交换记 canonical 名（记录用的是改写前 body），
  不是后端改写名。tool 行只覆盖 `tools/call`：非 call 方法（initialize/tools/list 等）
  没有 tool 名，只能经 server 级 `series` 观测。
  计数归属规则：
  - pinned/直连 server 的交换只记该 server 名；
  - 路由的 `initialize`/`tools/list` 只记路由名；
  - 路由的 `tools/call` 在 `account`（实际后端 server 名）非空且与暴露的 `name`
    不同时，会同时记**路由名**和**后端 server 名**各一次——因此两个命名空间的
    调用数之和会大于实际请求数；
  - 路由全部后端失败（502）只记路由名，不记后端。
  flusher 通过 `WithMCPStats` 选项接入，每分钟对 `MCPStats.RawSnapshot()`/
  `RawToolSnapshot()` 做 diff（`DiffMCP`/`DiffMCPTools`），由 `initStats()` 里的
  config-backed kind 解析器把名字归类为 `server` 或 `route`；解析失败的名字被跳过。
  diff 也处理计数器 reset（`current < previous` 时以 current 为 delta）。持久化遵循与
  LLM stats 相同的 retention/prune 配置（`stats.retention`，prune/reset 同步覆盖两张
  MCP 表）。`POST /api/tokens/reset` 会同时清空 `mcpStats` 及其 flusher 基线。
  - `/api/mcp`（Servers/Routes 标签）仍然只展示**进程生命周期**的内存计数——进程
    重启归零；
  - `/api/mcp/analytics`（Analytics 子标签）读取 `mcp_buckets`/`mcp_tool_buckets`
    持久化聚合，跨重启保留。tool 维度从该能力加入起才开始累积，更早的历史没有
    tool 行（UI 在窗口无 tool 行时回退到 server 级 series）。
  MCP 统计与 LLM `MetricsStore`/token 计数/agent 计数是**完全独立的管线**：不进入
  Status/Analytics 的 provider/model 维度，不参与等价成本计算，也不混入
  `minute_buckets`/`agent_buckets`。记录不依赖 `request_log` 开关。
- MCP 交换（pinned/route/stdio 全覆盖）发布 live start/end（protocol="mcp"）：start 在
  server/route 命中后发出（agent 取 UA 标签），end 由 defer 恰好一次发出，status/provider 经
  `mcpLiveWriter` 捕获（WriteHeader 截获 status、各 commit 点 `mcpSetLiveProvider`；
  Flush 透传不破坏流式）；end 另携带 `mcpLog` 解析回写的 agent（clientInfo/会话绑定
  标签）与 session_id（`mcpIdentity` 指针线程化，defer 在 handler 返回后发布、读到
  回写值）。与 LLM 面同一契约：start/end 必带稳定 request_id 配对。
- 路由 tools/call 候选构建时跳过**共享 MCP 工具时间窗耗尽**的 provider（智谱
  TIME_LIMIT 窗 `Kind=time + DetailLabel="By MCP tool" + RemainingPct==0`，经
  `Manager.Quota` 克隆读取；未轮询到则 fail-open）。这是预降权，call 时 429 failover
  仍是权威兜底；pinned 无候选可换，不做预拦截。

### Web 与 takeover

- `GET /api/mcp`（servers+routes+活会话 gauge）与 `POST /api/mcp/test`（握手探测，daemon
  版 `mcp test`）经 `ReadAPI.MCPSurface` / `CommandAPI.ProbeMCP`；admin 不 import
  internal/mcp，探测由组合根全权实现。Web MCP tab 复用 card/table/badge 既有模式，
  全部渲染为用户触发（无自动刷新 tick）。
- takeover `mcp:` 模板块（`json_path`+`json_entry` / `toml_section`+`toml_body`）把网关面
  写成客户端 MCP 配置：**默认面 = 全部 route + 未被聚合的 server**（被聚合成员不重复
  投影，`include_routed_members: true` 恢复全量；禁用面不投影）；合并语义——指向本代理
  `/mcp/` 的陈旧条目先清（JSON 按 url 前缀、TOML 按段名前缀+URL 内容），用户自有条目
  保留；网关面无条目时不动客户端配置。预设：
  `claude-mcp`（~/.claude.json mcpServers，独立族）、opencode 三变体（`mcp` 节）、codex
  （`[mcp_servers."<name>"]` 段）。
