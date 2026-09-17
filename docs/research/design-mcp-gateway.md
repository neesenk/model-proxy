# 设计草案：MCP 网关与能力路由

> 状态：**P1 + P1.5 + P2 已实现（2026-09）**。P1：`mcp:` pinned 透传网关（`internal/mcp` 叶子包、
> `/mcp/` handler、账号粘滞、401 轮换、换代 fail-closed、request log `kind`、CLI）。P1.5：
> `mcp_routes:` 聚合路由（proxy 自有会话、子会话懒建、tools/list 规范名聚合、call 级
> failover + 会话内目标粘滞、业务错误不 failover）。P2：stdio 子进程后端（每会话一进程、
> registry+OnEvict 联动回收）、legacy `sse` transport（endpoint 改写双通道绑定）、
> `guard.mcp_secrets` 出站扫描、`env:` 静态头、Web 面（`/api/mcp` + `/api/mcp/test` + MCP tab）、
> live start/end 事件、配额预降权、takeover mcp 模板块（claude-mcp/opencode/codex 预设）。
> 与草案的差异：① 火山 Agent 记忆需要独立 OpenViking key（Agent Plan key 401），未接
> （§8 开放问题 1）；② tools/list 聚合缓存按路由会话而非 generation；③ 配额冷却用共享
> TIME_LIMIT 窗的粗信号（usageDetails 只有用量归因、无 per-tool 限额可读），call 时 429
> failover 仍是权威兜底；④ live 事件经 status 捕获 writer + defer 实现，Live 语义从
> 「仅 LLM」扩到 MCP。
> 实现契约：`docs/architecture/mcp.md`；包归属 + DAG：`docs/architecture/overview.md`。
> 关联：探测结论全部来自 2026-09 对真实端点的在线实测（§3）。

## 1. 现状与问题

客户端（Claude Code / Codex / opencode / pi 等）已把 LLM 流量指向 model-proxy，但服务商与公共
MCP 服务（搜索、网页读取、文档检索、代码搜索……）目前只能逐客户端手工配置：

- 凭据分散：每个客户端都要拿到服务商 API key，绕过了 `internal/accounts` 账号池；
- 无统一观测：MCP 调用不进 request log，无法审计、无法排障；
- 无 failover：单个 MCP 服务挂了/限流就是硬失败，而同一能力往往有多个可替代后端。

已有可复用资产：账号池与 per-account 虚拟 provider（`providerbuild`）、per-provider 上游代理链
（`internal/upstreamproxy` + `Proxy.clientFor`）、request log（`internal/observe/requestlog`）、
进程级跨代 LRU 先例（`internal/guard/session`）、api-keys 门禁（`Proxy.Handler` forward 面）。

## 2. 目标 / 非目标

**目标**：

1. model-proxy 作为统一 MCP 网关：本地 `/mcp/<name>` 端点 → 远端/本地 MCP 后端，凭据不出仓；
2. 能力路由（`mcp_routes:`）：同一能力（如 web-search）声明多个后端，tools/call 级 failover；
3. 全部调用进 request log（新增 `kind` 字段），复用现有查询面。

**非目标**（本期不做）：

- stdio 本地子进程后端（智谱 vision、火山豆包搜索）→ P2；
- guard 出站扫描、Web/live 观测面、takeover 自动写客户端 MCP 配置 → P2；
- 协议层 hosted-tools 注入（Anthropic `mcp_servers` / Responses `tools:[{type:"mcp"}]`）→ 独立立项；
- legacy SSE transport（`/sse` 后缀端点）→ 首期只做 streamable HTTP（候选后端全部实测支持）。

## 3. 探测结论（2026-09 在线实测）

### 3.1 服务商后端（复用账号池凭据）

| 名称 | 端点 | 鉴权 | 实测 |
|---|---|---|---|
| 智谱 web_search_prime | `https://open.bigmodel.cn/api/mcp/web_search_prime/mcp` | Bearer（同 LLM key） | initialize + tools/list 全通；SSE 分帧响应；下发 `Mcp-Session-Id` |
| 智谱 web_reader | `https://open.bigmodel.cn/api/mcp/web_reader/mcp` | 同上 | initialize 通 |
| 智谱 zread | `https://open.bigmodel.cn/api/mcp/zread/mcp` | 同上 | initialize 通 |
| 火山 专业数据集 | `https://datapro.hqd.cn-beijing.volces.com/mcp` | **自定义头** `X-Agent-Plan-Key: <Agent Plan key>` | initialize 通（serverInfo「专业数据集查询服务」v1.28.1）；响应为纯 JSON |
| 火山 Agent 记忆 | `https://api.vikingdb.cn-beijing.volces.com/openviking/mcp` | Bearer OpenViking key | 用 Agent Plan key 试 401——**需要独立的 OpenViking key**（控制台另领），首期不接 |

智谱配额已实测：quota 接口 `TIME_LIMIT.usageDetails` 按 `search-prime/web-reader/zread` 分工具计量
（现被排除在绑定额度之外，P2 可作为 MCP 冷却信号）。

### 3.2 零鉴权公共后端（`auth: none`，全部 initialize 实测 200）

| 名称 | 端点 | 工具面 |
|---|---|---|
| Exa | `https://mcp.exa.ai/mcp` | web 搜索 |
| Jina | `https://mcp.jina.ai/` | 网页读取/搜索/grounding |
| Firecrawl | `https://mcp.firecrawl.dev/v2/mcp` | keyless 档 3 工具：search/scrape/parse；**无 `Mcp-Session-Id`（stateless）**；同端点加 Bearer 升级全工具面 |
| Context7 | `https://mcp.context7.com/mcp` | 开源库文档 |
| DeepWiki | `https://mcp.deepwiki.com/mcp` | GitHub 仓库问答 |
| Grep.app | `https://mcp.grep.app`（根路径） | GitHub 代码搜索 |
| Hugging Face | `https://huggingface.co/mcp` | 模型/数据集/论文检索 |

注意：匿名公共服务按出口 IP 限流且无 SLA——挂网关后全部客户端共享出口配额，cache/cooldown 与
failover 从加分项变为必需品。

## 4. 方案

### 4.1 配置面（`internal/config`）

```yaml
mcp:
  # provider 凭据后端：auth 默认 provider 模式，复用该 provider 的账号池
  zhipu-search:
    provider: zhipu
    url: https://open.bigmodel.cn/api/mcp/web_search_prime/mcp
    # auth_header: Authorization   # 默认 Authorization: Bearer <key>；
                                   # 火山 datapro 类配 "X-Agent-Plan-Key"（裸 key 值）
    # timeout: 300s                # 默认 300s（search/scrape 可能分钟级）
    # proxy_url / enabled / headers（静态附加头，值支持 env:VAR 引用）
  ark-datapro:
    provider: volcengine
    url: https://datapro.hqd.cn-beijing.volces.com/mcp
    auth_header: X-Agent-Plan-Key
  exa:
    url: https://mcp.exa.ai/mcp
    auth: none

mcp_routes:
  web-search:                       # 与 mcp: 共享命名空间（禁止重名），暴露 /mcp/web-search
    targets:
      - {mcp: zhipu-search, tools: {web_search: web_search_prime}}
      - {mcp: exa,          tools: {web_search: web_search_exa}}
      - {mcp: jina,         tools: {web_search: jina_search}}
```

校验规则：name 为合法路径段；`provider` 必须存在；`auth: none` 与 `provider` 互斥；
route 的 target 必须引用已定义的 mcp server、tools 映射非空；**凭据一律不进 config**
（红线 3——第三方 keyed 服务后续只许 `env:VAR` 引用，首期不实现该分支）。

### 4.2 端点与请求处理流

`Proxy.Handler` 在 `protocol.ForPath` 502 兜底之前新增 `/mcp/` 前缀分支：

| 方法 | pinned server | routed server |
|---|---|---|
| `POST /mcp/<name>` | JSON-RPC 透传（SSE 分帧响应边收边 flush，不缓冲） | proxy 编排：initialize 聚合、tools/call 路由+failover |
| `GET /mcp/<name>` | 上游 server-stream 透传（上游 405 则 405） | 405（首期无 server 主动消息） |
| `DELETE /mcp/<name>` | 会话终止透传 + 清本地会话条目 | 终止全部子会话 + 清条目 |

- `/mcp/` 属 forward 面：走现有 api-keys 门禁（非 admin endpoint）；body 上限复用
  `max_request_body_bytes`；默认 loopback listen 不变。
- pinned 透传不改写 JSON-RPC 内容与 `MCP-Protocol-Version`（协议版本客户端↔上游自行协商）；
  仅注入鉴权头、改写 `Mcp-Session-Id`（见 4.3）。
- 每请求一次 `RuntimeSnapshot` 捕获（红线 1）：mcp 配置、provider 实现、代理 client 全部取自同一代。

### 4.3 会话粘滞与账号选择

- **会话表**：进程级跨代有界 LRU（512 会话，30 分钟空闲 TTL，**惰性回收**——无后台 goroutine，
  不碰 lifecycle；先例 `internal/guard/session`）。本地 session id 由 proxy 铸造（crypto/rand）。
- pinned：upstream `initialize` 响应带回 `Mcp-Session-Id` 时，记录
  `local → (server, accountID, upstream session id)` 并改写响应头；后续请求按本地 id 映射回
  **同一账号** + 上游 id。stateless 上游（Firecrawl 实测无 session id）不铸造本地会话。
- 无会话请求（tools/list/call 直发、notifications）：池内 round-robin；401 时**一次**账号轮换重试
  （镜像 `BufferedLeg` 的一次 401 refresh 语义），不引入完整 failover、不写健康状态。
- reload 语义：条目存 accountID；换代后账号仍在池内则继续，已移除则 fail-closed 让客户端重新
  initialize。不跨代混用（红线 1 延伸）。
- routed：proxy 拥有客户端会话；对每个后端 lazy 建子会话（首个需要该后端的 call 时 initialize +
  notifications/initialized），子会话同样进会话表、随客户端会话终止。

### 4.4 能力路由（`mcp_routes:`）

- **initialize**：聚合并缓存各后端 tools/list（按 generation 缓存），对外暴露 `tools` 映射表里的
  **规范工具名**；schema 取第一后端的声明（等价关系是配置声明的人工事实，不做自动推导）。
- **tools/call**：规范名 → 当前目标后端 → 参数透传（v1 不做跨 schema 参数转换；声明等价即自认
  schema 兼容）。失败（transport error / 5xx / 429 / 会话失效）换下一 target 重试，每 target 一次；
  JSON-RPC 应用层 error result 属业务错误，**不触发** failover。
- 协议版本：proxy 对客户端 echo 其请求版本；对各后端分别按 initialize 结果记录（智谱实测协商到
  2024-11-05），tools/call body 版本无关，v1 不做版本转换。
- 会话内目标粘滞：同一客户端会话的同一规范工具固定首选同一后端（保持 server 侧状态/缓存
  一致），失败才换；新会话重新从 targets[0] 开始。

### 4.5 包归属与依赖

| 位置 | 职责 |
|---|---|
| 新叶子包 `internal/mcp` | MCP wire 语义：有界 JSON-RPC 帧解析（识别 method/响应配对，只为 session 捕获）、会话表（LRU+TTL+惰性回收）、streamable 传输判定、header 改写规则、tools 聚合与路由解析**纯逻辑**。零仓库内依赖 |
| `internal/config` | `mcp:` / `mcp_routes:` 类型、默认值、校验（不新增仓库依赖） |
| `internal/app/proxy_mcp.go` | 组合根：`/mcp/` handler、snapshot 解析、账号绑定、上游 client（`clientFor`）、request log 投影 |
| `internal/cli/mcp` | `mcp list` / `mcp test <name>`（initialize + tools/list 测活，经 daemon 或直接出站，形态对齐 `cli/models`） |

凭据注入窄缝：`auth_header` 自定义头需要原始 key。经接口断言新增 provider 窄方法（先例：
`provider.SecretReporter` 的接口级凭据暴露），只对 apikey 系 provider 实现；默认
`Authorization: Bearer` 直接复用 `AuthHeaders`。

DAG 同步点（两处镜像必须同改）：`docs/architecture/overview.md` 依赖 allowlist +
`internal/archtest/architecture_dependency_dag_contract_test.go`——新增叶子 `mcp`，新增边
`app → mcp`、`cli/mcp → …`、`cli → cli/mcp`。

### 4.6 观测

- `requestlog.Record` 新增 `kind`（`json:"kind,omitempty"`；空 = llm 兼容旧数据，`"mcp"` 时：
  `Protocol="mcp"`、`Method`=JSON-RPC method、`Path=/mcp/<name>`、status/latency/收发 size/body
  截断同现有策略）。列表/聚合等 metadata API 行为不变；detail/replay 可见完整 body。
- 首期不进 live events（Live 是 LLM 视图，与 handler 现有注释语义一致）；Web/live 面 P2 再议。

### 4.7 安全

- 凭据只存在于内存注入路径：不进 config、不进 request log header 白名单之外、不进事件；
  `headers:` 静态值只允许 `env:VAR` 引用（该分支 P1.5）。
- SSRF：上游 URL 只能来自 config，运行时不接受用户传入 URL；出站走 `clientFor` 代理链
  （provider 后端用 per-provider `proxy_url`，公共后端用全局链）。
- OAuth 型后端（Sentry 等，首期不接）要求 401/`WWW-Authenticate` 原样穿透，接入时再立条目。
- guard 出站扫描（tools/call 参数可能夹带秘密）P2，默认 off 立配置位。

## 5. 分期

| 阶段 | 内容 |
|---|---|
| **P1** | config（`mcp:`）、`internal/mcp` 叶子包、`/mcp/` pinned 透传 handler、会话粘滞、401 轮换、request log `kind`、`mcp list/test` CLI |
| **P1.5** | `mcp_routes:` 聚合路由（tools 合并 + call 级 failover + 会话内目标粘滞） |
| P2 | stdio 子进程后端（lifecycle admission）、guard 扫描、Web 观测面、takeover 写客户端配置、智谱 MCP 配额冷却、`env:` 静态头、legacy `/sse` transport |

P1 首批后端（全部 §3 实测可达）：智谱 3 + 火山 datapro 1 + 公共 7。

## 6. 测试矩阵

- `internal/mcp` 单测：帧解析（SSE 分帧/批量/notification 无 id）、LRU+TTL 惰性回收、header 改写、
  tools 聚合、路由解析；
- `internal/app` 集成（httptest fake 上游）：
  - initialize 回 session id → 后续请求**粘到同一账号**（多账号池断言）；
  - stateless 上游不铸造会话；notifications 202 透传；DELETE 清会话；
  - SSE 分帧响应不缓冲（流式断言）；401 一次轮换；reload 换代 fail-closed；Close 无 goroutine 泄漏；
  - api-keys 门禁对 `/mcp/` 生效；未知 `/mcp/` 名称 404（非 502）；
  - routed：tools/list 聚合、call 按映射选后端、5xx failover 到次后端、业务 error 不 failover；
- `internal/config`：校验错误矩阵（重名、缺 provider、auth 互斥、route 悬空引用）；
- requestlog：`kind` 字段 JSONL round-trip、旧记录无 kind 兼容、metadata API 不受影响；
- archtest：DAG allowlist + 叶子 import 契约；
- 并发：会话表 race-clean（无固定 Sleep；barrier/fake clock）。

门禁：`git diff --check`、`go vet ./...`、`go test ./... -count=1`、`go test -race ./... -count=1`、
`gofmt -l .`、`scripts/cover.sh`。

## 7. 文档与校验同步清单

实现时同 PR 同步：`README.md`（用户可见功能）、`config.yaml`（注释示例）、`CLI.md`（mcp 命令）、
`docs/architecture/overview.md`（新包 owner + allowlist + 镜像测试）、新建
`docs/architecture/mcp.md`（契约），本文状态头改为「已实现」并标注差异。

## 8. 开放问题 / 后续

1. 火山 Agent 记忆需要独立 OpenViking key（实测 Agent Plan key 401）——是否为此扩展 accounts 凭据
   类型（provider 外挂 key 条目），还是接受 `env:` 引用，接入前定。
2. 智谱 MCP 配额（TIME_LIMIT per-tool）→ cooldown 的信号路径：quota poll 已有数据，需要一条
   「per-tool 配额尽 → 该后端在对应 route 降权」的窄投影，P2 设计。
3. stdio 后端的子进程模型：每会话一进程 vs 每 server 一进程多会话复用——P2 设计时结合
   智谱 vision / 火山豆包搜索的实际并发语义定。
4. routed server 的 tools/list 变更（`listChanged`）首期按 generation 缓存不订阅，是否足够。
