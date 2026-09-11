# 客户端 Takeover 契约

## 适用范围

修改 `takeover` / `restore`、客户端模板、模板解析或渲染引擎时必读。

实现归属：备份/恢复、模板引擎与内嵌预设归 `internal/takeover`
（`RunTakeover` / `RunRestore` / `ListClients` / `LoadTemplates` / `TemplateByName` /
`BackupDir`；引擎在 `template.go`，预设在 `presets/*.yaml`）；`internal/cli`
（`commands.go` 的 `RunTakeover` / `RunRestore`）只解析参数、加载 config 并注入
模型事实（catalog 加载与 source 标记留在 CLI 层）；doctor 的漂移检测
（`CheckTakeoverDrift`）读模板的 drift 探针。

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

env:                               # format=env:KEY=VALUE 文件(注释/未管键保留)
  set: {GOOGLE_GEMINI_BASE_URL: "{{base_url}}", GEMINI_API_KEY: "{{token}}"}

models:                            # 可选:按暴露模型逐个输出元数据
  shape: opencode | pi | kimi      # 集合渲染器(含元数据默认值)
  json_path: provider.{{provider_id}}.models   # opencode/pi:集合注入点
  toml_section: 'models."{{model.id}}"'        # kimi:每模型段名
  toml_body: |                     # 支持 {{model.id}} {{model.context}} {{model.output}} {{provider_id}}
    provider = "{{provider_id}}"
    model = "{{model.id}}"
    max_context_size = {{model.context}}
  also_remove: 'models.{{model.id}}'           # 可选:写前清理旧段(如未加引号的遗留块)
```

占位符：`{{proxy_url}}` `{{base_url}}` `{{token}}`(= `PROXY_MANAGED`)
`{{provider_id}}` `{{display_name}}`；模型循环内另有 `{{model.id}}`
`{{model.context}}` `{{model.output}}`。

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
| claude | `~/.claude/settings.json` | json | env 注入 `ANTHROPIC_BASE_URL`(bare)+ `ANTHROPIC_AUTH_TOKEN`；Claude Code 自拼 `/v1/messages` |
| opencode | `~/.config/opencode/opencode.json` | json | `@ai-sdk/anthropic`(自拼 `/messages`,base_url 带 /v1)+ 全量模型(opencode 形状) |
| opencode-openai | 同上 | json | `@ai-sdk/openai-compatible` 变体(OpenAI Chat Completions),provider_id `model-proxy-openai` |
| opencode-responses | 同上 | json | `@ai-sdk/openai` 变体(OpenAI Responses `/v1/responses`),provider_id `model-proxy-responses` |
| pi | `~/.pi/agent/models.json` | json | `anthropic-messages`,base_url 裸(pi 自拼 `/v1/messages`)+ 全量模型(pi 形状) |
| pi-openai / pi-responses | 同上 | json | `openai-completions` / `openai-responses` 变体(base_url 带 /v1,独立 provider_id) |
| codex | `~/.codex/config.toml` | toml | `[model_providers."<id>"]`(wire_api=responses)+ 顶层 `model_provider` 选择器 |
| kimi | `~/.kimi/config.toml` | toml | `[providers."<id>"]`(`openai_legacy`,带 /v1)+ 每模型 `[models."<name>"]`(provider/model/max_context_size,点号名必须引号;无元数据回退 `routing.DefaultModelMetadata.Context`) |
| gemini-cli | `~/.gemini/.env` | env | `GOOGLE_GEMINI_BASE_URL`(带 /v1)+ `GEMINI_API_KEY` 占位 |

凭据一律占位符 `PROXY_MANAGED`,真实 key 只在代理侧。

**pi 会话归因**：pi 的 anthropic-messages 与 openai-completions 路径只在
`model.compat.sendSessionAffinityHeaders` 为 true 时才发 `x-session-affinity`
（openai-responses 默认就发）。`piModelsCollection` 给每个模型写入
`compat: {sendSessionAffinityHeaders: true}`,因此 takeover 后的 pi 请求带会话
UUID，代理据此填请求日志 `session_id` 与 live 事件 `session_id`（见
`docs/web-api.md` 的 `/api/events`、Live 会话分析）。

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
