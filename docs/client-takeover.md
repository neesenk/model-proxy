# 客户端 Takeover 契约

## 适用范围

修改 `takeover` / `restore`、客户端模板、模板解析或渲染引擎时必读。

实现归属：备份/恢复、模板引擎与内嵌预设归 `internal/takeover`
（`RunTakeover` / `RunRestore` / `RunTakeoverReport` / `RunRestoreReport` /
`CheckDrift` / `ListClients` / `LoadTemplates` / `TemplateByName` / `PresetTemplateYAML` /
`BackupDir`；引擎在 `template.go`，预设在 `presets/*.yaml`）；`internal/cli`
（`commands.go` 的 `RunTakeover` / `RunRestore`）只解析参数、加载 config 并注入
模型事实（catalog 加载与 source 标记留在 CLI 层）；doctor 的漂移检测
（`CheckTakeoverDrift`）是 `takeover.CheckDrift` 的薄包装。WebUI 面（Takeover tab）
经 `/api/takeover` 一族端点消费同一实现（`internal/admin/takeover.go` 只做投影与
目录解析，见 `docs/web-api.md`）。

## 模板机制

每个客户端一个 YAML 模板。解析顺序：内嵌预设（`internal/takeover/presets/`）
← 用户覆盖（`~/.model-proxy/takeover-templates/<name>.yaml`，同名替换预设，
新名新增客户端）。模板名 = 文件名去 `.yaml`。`takeover list` 显示解析结果
（含来源 preset/用户目录）。config.yaml 的 `takeover:` 块已移除（tombstone：
残留即报迁移错误）——改路径/provider_id/proxy_url 一律通过模板。

```yaml
description: 人类可读描述(takeover list 显示)
file: ~/.claude/settings.json     # 客户端配置文件(~ 展开),必填
format: json                       # json | toml | env,必填
client: claude                     # 客户端族(默认 = 模板名,即单变体族)
protocol: anthropic                # anthropic | openai | responses;多变体族必填且互不相同
base_url: bare                     # bare(默认) | v1(追加 /v1)
provider_id: model-proxy           # 默认 model-proxy;写同一文件的变体必须用不同 id
proxy_url: ""                      # 可选;默认 http://<listen>
display_name: model-proxy          # 可选,TOML name = "..." 用

json:                              # format=json:dotted.path → 值(嵌套 map/list 皆可)
  set:
    env.ANTHROPIC_BASE_URL: "{{base_url}}"
    env.ANTHROPIC_AUTH_TOKEN: "{{token}}"
  drift_path: env.ANTHROPIC_BASE_URL   # doctor 漂移探针(JSON 路径,期望值为 base_url)

toml:                              # format=toml:文本行编辑(无 TOML decoder)
  top_keys: {model_provider: '"{{provider_id}}"'}   # 顶层键(值原样写入,字符串自带引号)
  sections:                        # replace-or-append
    - name: 'model_providers."{{provider_id}}"'
      body: |
        name = "{{display_name}}"
        base_url = "{{base_url}}"
        wire_api = "responses"

variants:                          # 多协议 agent 单文档声明(opencode/pi 预设的形态):
  - name: pi                       #   每变体一份协议身份(name/protocol/base_url/
    protocol: anthropic            #   provider_id)与写入块(json/models),展开成
    base_url: bare                 #   独立模板——族选择/split 分区/备份单元/CLI
    json: {…}                      #   `takeover <变体名>` 全部按展开后模板工作
  - name: pi-openai                # 顶层只留共享字段(description/file/format/
    protocol: openai               #   client);mcp 块可顶层共享(渲染与变体无关)
    base_url: v1                   #   顶层出现写入块/协议身份与 variants 互斥(校验拒绝)

env:                               # format=env:KEY=VALUE 文件(注释/未管键保留)
  set: {GOOGLE_GEMINI_BASE_URL: "{{base_url}}", GEMINI_API_KEY: "{{token}}"}

models:                            # 可选:按暴露模型逐个输出元数据
  shape: opencode | pi | kimi      # 集合渲染器(含元数据默认值)
  json_path: provider.{{provider_id}}.models   # opencode/pi:集合注入点
  toml_section: 'models."{{model.id}}"'        # kimi:每模型段名
  toml_body: |                     # 支持 {{model.id}} {{model.context}} {{model.output}} {{provider_id}}
    provider = "{{provider_id}}"               #   及 kimi 能力块 {{model.capabilities}} {{model.efforts}}
    model = "{{model.id}}"
    max_context_size = {{model.context}}
  also_remove: 'models.{{model.id}}'           # 可选:写前清理旧段(如未加引号的遗留块)

mcp:                               # 可选:把网关 mcp:/mcp_routes: 面写成客户端 MCP 配置
  file: ~/.claude.json             # 可选:MCP 存于独立 JSON 文件时(claude/kimi;缺省写主文件,如 opencode/codex)——独立备份单元 <name>-mcp,与主文件格式无关(mcp 块按 JSON 语义校验/渲染)
  json_path: mcpServers            # json:对象注入点(claude 的 ~/.claude.json;opencode: mcp)
  json_entry: {type: http, url: "{{mcp.url}}"}  # 每条目值模板(占位符见下)
  toml_section: 'mcp_servers."{{mcp.name}}"'   # toml:每条目段名
  include_routed_members: false    # 可选(默认 false):被 mcp_routes 聚合的成员 server 不再单独写条目——route 是规范入口,成员直连会造成客户端工具重叠;true 恢复全量投影
  toml_body: |
    url = "{{mcp.url}}"
```

占位符：`{{proxy_url}}` `{{base_url}}` `{{token}}`(= `PROXY_MANAGED`)
`{{provider_id}}` `{{display_name}}`；模型循环内另有 `{{model.id}}`
`{{model.context}}` `{{model.output}}`；mcp 条目循环内另有 `{{mcp.name}}` `{{mcp.url}}`
（url = `<proxy>/mcp/<name>`，按名排序）。**默认面 = 全部 route + 未被任何启用 route
聚合的 server**：被聚合的成员默认不单独投影（route 已覆盖其能力，重复直连会让客户端
看到重叠工具）；`include_routed_members: true` 恢复全量。禁用的 server/route 不投影
（其 `/mcp/` 端点 404，写入即死条目）；禁用的 route 不拥有成员——其成员回退直连投影，
能力保持可达。显式 MCP 子集（CLI `--mcp` / Web 确认对话框 chip）在全量面上选择：点名
被裁剪的成员 = 显式要求直连，优于默认裁剪。kimi 形状另有
`{{model.capabilities}}`（models.dev 元数据派生的 TOML 能力数组，见下节）与
`{{model.efforts}}`（`support_efforts` + `default_effort` 两行块，模型无 effort
档位时渲染为空——写空档位会破坏 kimi-cli 的 effort 选择器）。

mcp 渲染是**合并语义**：先清理指向本代理 `/mcp/` 的陈旧条目（JSON 按 url 前缀、TOML 按
段名前缀+正文 URL 匹配），再写入当前面；用户自有 MCP 条目保留；网关面无条目时不动客户端
配置。env 格式不支持 mcp 块（校验拒绝）。

## 协议感知变体选择

单协议 agent（claude、codex、gemini-cli）只有一种写法，按它支持的协议写。
多协议 agent（pi、opencode）每种协议一个模板变体，用 `client:` 归族、
`protocol:` 标注（pi 族：pi=anthropic / pi-openai=openai / pi-responses=responses；
opencode 族：opencode=anthropic / opencode-openai=openai / opencode-responses=responses）。**takeover 的目标是让 agent 用 provider 原生
协议直连模型**——协议与上游一致时是字节级透传，不一致才走
`internal/protocol` 转换（开销与兼容性边界见
`docs/architecture/protocol-conversion.md`）。`--mode` 决定多协议族怎么写：

- **unified（默认）**：按族选一个变体写入。统计每条暴露路由首选目标
  （priority 最小）的原生协议（`routing.NativeProtocolsWithVerdict`，判定
  顺序：显式 `protocol:` > `ProtocolHint` > daemon 逐模型探测结论
  （`model_caps.json`，fingerprint 校验防陈旧：探测 no 推翻端点声明、yes
  可补出静态拿不到的 responses 腿）> 声明的 `anthropic_base_url`/
  `openai_base_url`（responses 无探测结果时不静态声明）），覆盖最多的协议
  胜出，平手（含完全无信号）回退与族同名的默认变体；覆盖之外的模型走
  协议转换。选择理由与仍需转换的模型会打在日志里。
- **split**：每种原生协议写一个配置项，暴露模型按原生协议划分到各配置项
  （`splitAssignment`：多原生/未知协议模型归默认变体，无模型的变体不产出
  空配置项），每个模型都是透传。共享同一客户端文件的多个变体顺序写入，
  因此 RunTakeover 一律两阶段执行（先全部备份再全部改写），保证每个变体的
  备份都是原始文件而不是上一个变体的改写结果。
- **协议值（anthropic|openai|responses）**：带协议偏好的 unified——族里
  有该协议的变体就钉到它（不看原生覆盖率，其余模型走转换，日志列出）；
  没有该变体的族回退 unified 自动选择（没有就用默认方式）。
- **交互选择**：TTY 下未给 `--mode` 且 split 会写出与 unified 不同的配置项
  集合（`SplitWouldChange`，即路由横跨多种原生协议）时，takeover 提示用户
  二选一；管道/脚本默认 unified。
- 精确模板名（pi-openai）始终钉住该变体，不参与模式与自动选择。
- 多变体族的校验是硬约束：族内每个变体必须声明 `protocol` 且互不相同，
  否则 LoadTemplates 直接报错（fail-closed，不猜）。
- restore 不做协议选择：备份标记属于当初实际接管的变体，配置可能已变，
  族名恢复会展开到族内全部变体、跳过无备份者。

## 内嵌预设

| 模板 | file | format | 要点 |
|---|---|---|---|
| claude | `~/.claude/settings.json` | json | env 注入 `ANTHROPIC_BASE_URL`(bare)+ `ANTHROPIC_AUTH_TOKEN`；Claude Code 自拼 `/v1/messages`。**同一模板还接管 MCP**：`mcp.file: ~/.claude.json`（Claude Code 的 user-scope MCP 存在与主配置不同的文件）——一次 takeover 同时落两个文件，各自独立备份单元（`claude.bak` / `claude-mcp.bak`），restore 一并恢复 |
| opencode(单文档 3 变体) | `~/.config/opencode/opencode.json` | json | 变体 = 协议档位:`@ai-sdk/anthropic`(自拼 `/messages`)/ `@ai-sdk/openai-compatible`(Chat Completions,provider_id `model-proxy-openai`)/ `@ai-sdk/openai`(Responses,provider_id `model-proxy-responses`),base_url 均 /v1 + 全量模型(opencode 形状)+ 顶层共享 mcp 块 |
| pi(单文档 3 变体) | `~/.pi/agent/models.json` | json | 变体 = 协议档位:`anthropic-messages`(base_url 裸,pi 自拼 `/v1/messages`)/ `openai-completions` / `openai-responses`(base_url 带 /v1,独立 provider_id)+ 全量模型(pi 形状) |
| codex | `~/.codex/config.toml` | toml | `[model_providers."<id>"]`(wire_api=responses,base_url 带 /v1——codex 拼 base_url+/responses)+ 顶层 `model_provider` 选择器 + **模型目录**：`~/.codex/model-proxy-models.json`(shape codex,每暴露模型一条 ModelInfo：visibility=list、context/effort 档位/输入模态来自 models.dev)+ 顶层 `model_catalog_json` 指向它——codex 加载后**替换**内置目录，/model 选择器即列出全部代理模型；restore 一并删除目录文件 |
| kimi | `~/.kimi-code/config.toml` | toml | `[providers."<id>"]`(`openai`,带 /v1——kimi-cli 2.x 移除了 `openai_legacy` 运行时类型,模型 wire protocol 从 provider type 解析,缺失即报 `must declare a wire protocol`)+ 每模型 `[models."<name>"]`(provider/model/max_context_size/capabilities,可选 support_efforts+default_effort;点号名必须引号;无元数据回退 `routing.DefaultModelMetadata.Context`)。**同一模板还接管 MCP**:`mcp.file: ~/.kimi-code/mcp.json`(Kimi Code 的 MCP 配置独立于 config.toml,`mcpServers`/`url` 条目)——独立备份单元 `kimi-mcp.bak`,restore 一并恢复 |
| gemini-cli | `~/.gemini/.env` | env | `GOOGLE_GEMINI_BASE_URL`(带 /v1)+ `GEMINI_API_KEY` 占位 |
| （opencode 三变体） | 同上 | json | 附 `mcp` 块：`mcp` 节写 remote 型条目（enabled: true） |
| （codex） | 同上 | toml | 附 `mcp` 块：`[mcp_servers."<name>"]` 段写网关条目 |

凭据一律占位符 `PROXY_MANAGED`,真实 key 只在代理侧。

**pi 会话归因**：pi 的 anthropic-messages 与 openai-completions 路径只在
`model.compat.sendSessionAffinityHeaders` 为 true 时才发 `x-session-affinity`
（openai-responses 默认就发）。`piModelsCollection` 给每个模型写入
`compat: {sendSessionAffinityHeaders: true}`,因此 takeover 后的 pi 请求带会话
UUID，代理据此填请求日志 `session_id` 与 live 事件 `session_id`（见
`docs/web-api.md` 的 `/api/events`、Live 会话分析）。

## 能力元数据（models.dev → 客户端能力声明）

models.dev 是能力的唯一外部事实源；`internal/catalog` 投影 `tool_call`/`reasoning`/
`modalities.input`/`reasoning_options`(effort 档位,`none` 是思考开关不算档位)。
takeover 把它们翻译成各客户端的模型能力声明——**没有元数据就不写该能力**（宁可保守
也不虚构）：

- **kimi**（用户实测报告的回归）：`capabilities` 数组逐项派生——`thinking`+
  `always_thinking` ← reasoning；`image_in`/`video_in` ← 输入模态；`tool_use` ←
  tool_call；`dynamically_loaded_tools` ← tool_call 且有 effort 档位（kimi 官方
  托管配置 4/4 吻合：k3/kimi-for-coding/k3-256k 有、highspeed 无）。
  `support_efforts` ← effort 档位原样；`default_effort` ← 最高档（models.dev 无逐
  模型默认值标记，kimi 官方默认不一致:kimi-for-coding=max、k3=high——takeover 取
  最高档暴露全部能力，用户可在 config.toml 或模板覆盖里改）。无元数据 →
  `capabilities = []`（等价于今天的能力缺失行为，而非虚构能力）。
- **pi**：`reasoning: true` + `input`（text/image，pi schema 只收这两值）+ 窗口
  元数据已有；缺 `reasoning` 会让 pi 把思考模型当纯聊天模型（既有行为已覆盖）。
- **opencode**：模型 schema 的 `reasoning`/`tool_call` 布尔 + `modalities` 照写；
  缺失会让 opencode 隐藏思考/工具能力。
- **claude / codex / gemini-cli**：配置 schema 无逐模型能力声明（claude 只有 env、
  codex 的 `model_reasoning_effort` 是用户偏好而非能力声明、gemini-cli 是 env 键），
  无需也无处可写——复查确认。

`catalog.parse` 同时修正 models.dev 当前 api.json 的 `tool_call` 顶层位置（旧投影
读 `features.tool_call`，实测 7838/7838 个模型都在顶层——旧代码恒 false）；磁盘缓存
`models_cache.json` 旧格式缺 `efforts` 字段，刷新（`models pull` / Web 的 Refresh
Catalog）后生效。

## 机制契约（与模板机制无关的部分不变）

- 备份位于 `<configDir>/.model-proxy/<client>.bak`(+ sha256 meta)；
- `takeover all` / `restore all` 遇到未安装客户端时跳过并继续；单独指定客户端而文件不存在时返回硬错误；未知模板/族名是硬错误（列出可用模板与族）；`takeover all` 每族只应用自动选中的一个变体（见上文协议感知变体选择）；
- takeover 前必须备份，restore 后不得保留代理专属残片；
- restore 成功即结束接管：删除 `<client>.bak` 与 `<client>.bak.meta` 标记，
  drift 检查（以 `.bak` 是否存在作为"已接管"标记）随后报告该客户端未接管，
  再次 takeover 会重新备份而不是沿用陈旧备份；恢复前的 sha256 完整性校验
  （fail-closed）不受影响；
- 所有客户端写回（JSON 改写、TOML 改写、env 改写、restore）一律 temp+fsync+rename 原子写；
  **保留目标文件既有权限位**（这些文件常含真实 API key，硬编码 0644 会把 0600 放宽成全局可读），
  新建文件统一 0600；
- 渲染幂等：重复 takeover 不产生重复段/键（replace-or-append / key set）；
- 漂移探针模板驱动：json 用 `drift_path`，toml 有 `model_provider` top_key 时
  codex 式（选择器 + 段 base_url）否则 kimi 式（首个段 base_url），env 取渲染值
  等于 base_url 的键；无探针信息的自定义模板显示 `(no drift probe)`。
