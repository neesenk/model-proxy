# model-proxy

多 Provider LLM 代理 — 统一管理 AQP/codex/Zhipu 等上游后端，按协议（Anthropic/OpenAI）对外暴露，自动处理鉴权、模型映射、流式转发。

**特性一览**：

- **多上游聚合 + 配额感知调度**：surplus 调度分 / 熔断 / 限频跳过 / 粘性驻留 / 多账号凭据池 + 会话粘性
- **三协议转发 + 可选协议转换**：同协议字节级透传；路由目标声明 `protocol:` 即可在 Anthropic Messages、OpenAI Chat Completions、OpenAI Responses 间转换
- **请求感知路由**：按图片/工具能力过滤目标、超长 prompt 自动改道大上下文模型、上游 400 溢出自动重试一次
- **可观测性**：Web UI 六个标签页、实时请求监视（SSE）、请求日志查询、延迟（LAT/TTFT）与按 agent 维度的统计
- **评测工具**：影子评测（真实负载双跑对比后端）、一键重放（replay）、端到端测活（`test` / UI 按钮）
- **多模型编排（fusion）**：一条路由 fan-out 到多个后端并行生成候选答案，结果汇总模型融合成最终答案——困难问题要最好效果
- **其他**：精确响应缓存、`pin` 运行期热切换、等价成本分析（OpenRouter 价格）

## 架构

```
┌─────────────┐     ┌───────────────────────────────────┐     ┌──────────────┐
│  客户端      │────▶│  model-proxy                      │────▶│  Provider    │
│  claude/     │     │  ┌─────────┐  ┌────────────────┐ │     │  aqp         │
│  opencode/   │     │  │ routes  │→│ provider (auth) │ │     │  codex       │
│  codex/      │     │  │ (proto) │  │ RewriteRequest  │ │     │  zhipu       │
│  curl/SDK    │     │  └─────────┘  └────────────────┘ │     │  ...         │
└─────────────┘     └───────────────────────────────────┘     └──────────────┘
```

- **Provider 层**（`internal/provider/` 包）：每个上游后端是一个 Provider 实现，封装鉴权、请求改写、登录、用量查询
- **Routes 层**：对外暴露模型名 → 一组 `provider/model` 目标。调度先看非高峰（provider 的 `peak_hours`），再看 `priority`，失败逐一 failover。anthropic 协议先经 `claude_mapping` 把 claude-* 别名翻译成对外模型名，再查路由；目标可声明 `protocol:` 触发协议转换；调度后还会按请求内容（图片/工具/上下文长度）做请求感知路由
- 凭据由 `login <provider>` 管理，不落 config；config `credentials:` 统一选择 apikey 池与 codex/aqp OAuth store 的存储后端（`file` 默认 / `keychain`：秘密值进 OS keychain、池文件只留元数据），env `MP_CRED_STORE` 仅作为 OAuth 侧的显式 override

## 安装

```bash
brew tap neesenk/model-proxy && brew install --cask model-proxy
```

也可从 [GitHub Releases](https://github.com/neesenk/model-proxy/releases) 直接下载对应平台的归档（含 checksums.txt），或 `go install github.com/neesenk/model-proxy@latest` 源码安装。

## 构建

```bash
go build -o model-proxy .          # 原生编译（host）

# 交叉编译（纯 Go，CGO_ENABLED=0，全静态，无需交叉工具链）
scripts/build.sh                   # 编译 host -> dist/ + ./model-proxy
scripts/build.sh linux/amd64       # 交叉编译单个目标 -> dist/
scripts/build.sh --strip all       # 全矩阵（linux/darwin/windows），-s -w 去符号
```

`scripts/build.sh` 从 `git describe --tags --always --dirty` 注入版本号（`-ldflags -X main.version`，覆盖 `version.go` 的 `dev` 默认值，显示在 `serve status` / `/api/status`）。每个目标写 `dist/model-proxy-<goos>-<goarch>`（windows 加 `.exe`）；host 构建额外复制到 `./model-proxy`（可原地运行）。Flag：`--version <v>`、`--out <dir>`（默认 `dist`）、`--strip`（`-s -w`）、`-v`。`dist/` 和 `./model-proxy` 都在 .gitignore 里。

**发布**：打 `v*` tag 推送即触发 `.github/workflows/release.yml`（GoReleaser，配置见 `.goreleaser.yaml`）——同一全静态矩阵 + 归档 + checksums 上 GitHub Releases，并自动更新 `neesenk/homebrew-model-proxy` 的 cask。本地验证：`goreleaser release --snapshot --clean`（不推送）。首次启用需在仓库 Settings 配 `HOMEBREW_TAP_GITHUB_TOKEN`（对 tap 仓库有 contents 写权限）。

## 配置

`config.yaml`（`model-proxy config init` 生成；TTY 下为交互向导——探测已装客户端、勾选 provider、可选立即 takeover，只写最小配置；管道/脚本下输出完整注释模板）。查找顺序：`--config PATH` > `~/.model-proxy/config.yaml` > `./config.yaml`。路径字段支持 `~/` 展开 和 `env:ENV_VAR` 前缀（从环境变量读值，如 `log_file: env:MP_LOG_FILE`）。

```yaml
listen: 127.0.0.1:15721    # 强制回环（0.0.0.0/内网 IP/域名会被 validate 拒绝——/api/* 无鉴权）
log_level: info

providers:
  aqp:
    provider_id: aqp
    openai_base_url: https://compass.llm.shopee.io/compass-api/v1
    aqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
    headers:                      # 可选：每个上游请求（转发+探测）额外带的头，鉴权之后应用
      x-ccswitch-client: "0.2.1"  # 对齐 AIS Switch 的客户端标识头（值=app 裸版本号；AIS Switch 升级后同步改）
    models:                       # 只填模型名；元数据(context/output/modalities/tool_call)运行时从 models.dev 自动补
      - glm-5.2
  codex:
    provider_id: codex
    openai_base_url: https://chatgpt.com/backend-api/codex
    models:
      - gpt-5.5
    # capabilities:               # 可选：手动声明模型能力（models.dev 查不到时的逃生口，声明即权威）
    #   gpt-5.5: [image, tools]
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    usage_url: https://open.bigmodel.cn/api/paas/v4/models
    models:
      - glm-5.2

claude_mapping:
  claude-opus-4-7: glm-5.2          # anthropic-only: claude 别名 → 对外模型名
  claude-sonnet-4-6: deepseek-v4-pro

routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
    - {provider: zhipu,   model: glm-5.2, priority: 2}   # failover 备选
  gpt-5.5:
    - {provider: codex, model: gpt-5.5, priority: 1}

# takeover:              # 接管客户端配置（全部字段有默认值，可省略整块）
#   claude: ~/.claude/settings.json
#   opencode: ~/.config/opencode/opencode.json
#   codex: ~/.codex/config.toml
#   pi: ~/.pi/agent/models.json
#   provider_id: model-proxy

# web:                      # 管理后台（默认开启，仅 loopback，无鉴权）
#   enabled: true
# stats:                    # 调用统计持久化（SQLite，默认开启，30 天保留）
#   db_path: ~/.model-proxy/stats.db
#   retention: 30d          # 0 = 永久
# cache:                    # 精确响应缓存（默认关；逐字节重复的请求直接命中，省上游配额）
#   enabled: true
#   ttl: 10m
#   max_entries: 1000
# shadow:                   # 影子评测：把某路由的请求异步镜像到候选后端做对比（需 request_log 开启）
#   glm-5.2: {provider: kimi-code, sample_rate: 0.1, max_concurrent: 4}
# request_log:              # 请求日志（完整 request/response body，默认关；Requests 页 + replay 的数据源）
#   enabled: true
# guard:                    # 出站安全扫描（DLP，转发前扫描请求 body；详见下文「出站安全扫描与审计」）
#   secrets: log            # 秘密扫描动作：log（默认，放行 + 告警）| redact（替换 [REDACTED] 后放行）
#                           # | block（400 拒绝）| off（不扫）
#   known_secrets: true     # 默认 true：把代理自己管理的凭据（账号池 key、OAuth token）加入扫描集，
#                           # 请求体出现这些值（含 base64/hex/url 编码形态）即命中，零误报
#   decode: true            # 默认 true：检测编码形态的秘密（base64/hex 前缀变体，解码后过原规则）
#   paths: log              # 敏感路径信号：log（默认）| block | off（不支持 redact）。命中按位置分
#                           # 两级：工具调用/工具结果位（strong）按此动作处理；正文提及（weak）
#                           # 只计数+审计（log-weak），不发 live event、永不 block
#   audit: true             # 默认 true：命中持久化到安全审计日志（`model-proxy audit` 查询）
#   session_scan: true      # 默认 true：分片泄露检测——同一 session（x-claude-code-session-id）多条请求
#                           # 拼出一个 known-secret 即命中 known_secret_fragmented（redact 对此降级为 log）
#   audit_path: ""          # 默认派生 <home>/.model-proxy/security.log；自定义必须是绝对路径（不展开 ~）
#   extra_patterns:         # 自定义秘密格式（gitleaks extend 式，热 reload 生效）
#     - {name: myvendor_key, regex: '\bmv-[A-Za-z0-9]{32,}', literal: 'mv-'}
#   extra_paths:            # 自定义敏感路径（字面量）
#     - ~/.company/secrets
# budgets:                  # 月度预算告警（默认关；启用需重启）：按 stats 用量 × 价格目录
#   monthly_usd: 20         # 每分钟核对当月等价成本，越线发 "budget" live event（SSE /api/events），
#   providers: {zhipu: 5}   # 可选 webhook_url 时附带 POST {scope, month, threshold_usd, actual_usd}
                            # （2 次重试，失败只记日志）；providers 条目是该 provider 的独立阈值
                            # （覆盖全局）；每 (scope, 月份, 阈值) 每进程只告警一次，重启可能重复
```

> **模型元数据**：`models:` 只填模型名，`context`/`output`/`modalities`/`tool_call` 在运行时从 [models.dev](https://models.dev) 自动补全（缓存于 `~/.model-proxy/models_cache.json`，24h TTL，ETag `304`-aware；`models pull` 强制刷新）。网络、HTTP 或响应解析失败时已有缓存继续可用且不会被覆盖；无可用缓存时 `models pull` 明确报错，普通列表/刷新与 takeover 按原有 best-effort 语义使用保守默认值。匹配不到的模型会在 `takeover` 时告警。`MP_MODELSDEV_URL` 环境变量可覆盖 models.dev 端点（测试/镜像用）。
>
> **隐式路由**：某个模型即使没在 `routes` 里配，只要某个**已登录** provider 的 `models:` 列了它，代理会自动按模型名路由到（字母序）首个 provider。若多个已登录 provider 都提供且无显式 route，只用首个并在 `models` 命令 / Web UI 发出歧义告警。显式 `routes` 永远优先（要做 failover/优先级控制仍需显式配置）。

## 用法

```bash
# 登录（凭据存储在 ~/.model-proxy/<name>_<suffix>.json；apikey 类重复 login 累积多账号池）
model-proxy login aqp          # AQP SSO 浏览器登录
model-proxy login codex            # codex OAuth device flow
model-proxy login zhipu            # 输入 Zhipu API key（--label NAME 命名；重复 login 加进池）
model-proxy login deepseek         # 输入 DeepSeek API key（可重复 -> 多账号）
model-proxy login volcengine       # Ark API Key + AccessKey/SecretKey（可重复 -> 多账号）
model-proxy login qwen-plan        # 千问 Token Plan 个人版 sk-sp- key（可重复 -> 多账号）
model-proxy login zhipu --label work --replace   # 命名账号 / 覆盖已存在的同 id 账号
# 免粘贴导入（值不回显、不落日志；成功输出只有掩码账号 id）
model-proxy login codex --from-codex             # 复用官方 codex CLI 登录态（~/.codex/auth.json，access_token 过期会自动 refresh）
model-proxy login zhipu --from-env ZHIPU_KEY     # 从环境变量读 API key（等价交互输入，支持 --label/--replace）
model-proxy login volcengine --from-env VOLC_ARK_KEY --from-env-ak VOLC_AK --from-env-sk VOLC_SK   # AK/SK 可选

# 预设接入（一条命令完成：合并 provider 块到 config.yaml + 登录 + 热重载）
model-proxy presets list                          # 内置预设目录（来自内置模板，过滤未实现的 provider）
model-proxy add zhipu                             # 交互式接入（TTY 下可选编号）
model-proxy add deepseek --api-key-env DS_KEY     # 脚本化接入：API key 从环境变量读
model-proxy add kimi-code --label work --replace  # 命名账号 / 覆盖已登录账号
# 模型同时被其他已配置 provider 服务且无显式 route 时会告警；非 TTY 下拒绝执行，加 --yes 放行

# 启动代理
model-proxy serve                  # 前台
model-proxy serve daemon           # 后台（自动重启）
model-proxy serve stop             # 停止 daemon
model-proxy serve reload           # 热加载配置（SIGHUP）
model-proxy serve status           # 运行状态（providers/路由/配额/token/延迟）
# serve 通用 flag：--config <PATH>、--log-file <PATH>（覆盖 config 的 log_file）

# 查看用量
model-proxy usage aqp          # 月度用量/余额
model-proxy usage codex            # credits/spend/rate limits
model-proxy usage zhipu            # 5h/周/月配额 + token 消耗（池化时逐账号展示全部账号）
model-proxy usage deepseek         # 账户余额（is_available + 各币种）
model-proxy usage volcengine       # Agent Plan 5h/日/周/月额度（需 AK/SK；否则列 config 模型）
model-proxy usage qwen-plan        # 个人版 Credits 仅控制台可见（输出订阅页 URL + 列模型）
# 有 daemon 轮询历史时，配额窗口行尾会按当前消耗速率预测耗尽时间（"按当前速率 ~40m 后耗尽"）；
# Web Status 配额卡同样展示。速率 ≤0、无历史基线或轮询断档（>3×quota_poll_interval）时不显示。

# 登出
model-proxy logout aqp         # 清除凭据文件
model-proxy logout zhipu --label work   # 删指定账号（apikey 类池）
model-proxy logout zhipu --all          # 清空整个池

# 模型列表
model-proxy models                 # 所有 provider 的模型（元数据从 models.dev 自动补；SRC 列标来源）
model-proxy models aqp             # 单个 provider
model-proxy models refresh zhipu   # 从服务端拉取模型，逐个探测校验后覆盖写回 config（无 /models 端点时回退探测路由模型）
model-proxy models pull            # 强制刷新 models.dev 元数据缓存

# 端到端测活
model-proxy test glm-5.2           # 探测路由每个 target（路由 → 凭据 → 上游真实请求）；任一通则 exit 0
# Web UI Accounts 页每个账号卡片还有 Test 按钮（POST /api/accounts/<p>/<id>/test），可测池化指定账号

# 接管客户端配置
model-proxy takeover opencode      # claude|opencode|codex|pi|all
model-proxy restore opencode

# 配置管理
model-proxy config init            # TTY 下是引导式上手向导（探测客户端→选 provider→可选 takeover→打印下一步）；管道/脚本下生成完整注释模板
model-proxy config print           # 打印生效配置
model-proxy config check           # 校验配置

# 调度诊断
model-proxy schedule               # 查询运行中的 daemon：每 model 当前调度到哪个 provider（GET /debug/schedule）
model-proxy doctor                 # 离线 config 调度诊断（tier/quota/peak/shadow + dry-run 顺序 + warning）
model-proxy doctor --live          # 实时诊断：连运行中的 daemon 回答“agent 为什么不动”（路由全灭+最早恢复时间 / pin / 配额将尽 / takeover 漂移 / 最近失败）

# 临时钉住路由（排查/对比用，不改 config）
model-proxy pin glm-5.2 zhipu --ttl 1h   # 硬禁 failover：zhipu 挂了就 502，绝不逃别家
model-proxy pin                          # 列出所有 pin
model-proxy unpin glm-5.2

# 清理冻结的 provider 状态（熔断/429 冷却/模型锁），下次请求立即重试
model-proxy unfreeze                     # 全部 provider
model-proxy unfreeze zhipu               # 单个（池化父名 = 全部账号）

# 调用统计（需 daemon + web.enabled）
model-proxy stats                  # 最近 60min 的 per-(provider,model) 调用统计（reqs/failover/429/fail/lat/ttft/input/output）
model-proxy stats --bucket 1h      # 按小时聚合展示
model-proxy stats --from 1h --to now --provider zhipu   # 时间范围 + 过滤
model-proxy stats --by-agent       # 按 agent 维度（哪个客户端在烧配额；可叠 --agent/--provider/--model）
model-proxy stats --json           # 原始 JSON（便于 jq）

# 影子评测与重放（需 request_log.enabled）
model-proxy shadow report          # 影子聚合对比：样本数/状态一致率/延迟差/大小比
model-proxy replay <request_id> --to kimi-code   # 用另一个后端重答历史中任意一条请求

# 安全审计（离线直读审计日志，不需 daemon）
model-proxy audit                  # 最近的 guard 命中（秘密/路径）与 takeover 漂移记录
model-proxy audit --kind drift --from 7d --json   # 过滤 + 原始 JSON
```

## Web UI

代理内置一个管理后台（admin UI），在 `http://127.0.0.1:<listen>/ui/`（如 `listen: 127.0.0.1:15721` → <http://127.0.0.1:15721/ui/>）。**默认开启；`listen` 由 validate 强制回环，无鉴权**（本地可信）。六个标签页：

- **Status** — 实时面板：uptime / 版本 / listen 地址、每 provider 的熔断/限频状态、配额快照、每路由当前调度选择、请求计数器（含平均延迟）、观测到的 token 用量（按 provider×model）、按 agent 的用量卡片、响应缓存命中率、日志尾部。
- **Config** — 原始 YAML 编辑器（GET 返回原文件、POST 经 `validate → backup(<configDir>/.model-proxy/back/<base>.<时间戳>.bak) → atomic write → reload` 流水线落盘 + 热重载）+ 结构化编辑表单（`general` / `scheduling` / `provider` / `route` / `claude_mapping`，通过 yaml.Node API **保留注释与键序**）。
- **Accounts** — 列出每个 provider 的账号（`id` / `label` / `added_at`，aqp/codex 额外显示 email；**响应结构里根本没有 key 字段，secret 不可能被序列化出去**）；apikey 类 provider 可在 UI 添加/删除账号；**每个账号卡片有 Test 按钮**（真实最小请求测活，显示 HTTP 状态 + 延迟）；aqp/codex 走**异步登录**（浏览器完成 SSO / OAuth device flow → UI 轮询直到 `done`/`error`）。
- **Analytics** — token + 等价成本趋势（日历日/月聚合；价格来自 OpenRouter 目录或 config `prices:`，未定价显示 `n/a`）。
- **Requests** — 请求日志查询（需 `request_log.enabled`）：按 model/provider/状态/时间/影子过滤，点击行展开完整 request/response body；影子评测的记录带 `shadow` 徽标。
- **Live** — 实时请求监视（SSE 推送）：哪个 agent 正在发请求、路由到哪个上游、状态/token/耗时——抓「疯狂重试的 agent」就靠它。

**所有写操作都会即时热重载运行中的 serve（进程内 `p.reload`，无需重启）**：改 config、增删账号、aqp/codex 登录完成 —— 改动立即生效。账号增删虽不改 `config.yaml`，但 reload 会重建 providers（重新读池文件），新加/删除的账号随即（取消）展开成虚拟 provider；reload 还会顺手清空熔断/限频/粘性状态并重建响应缓存，所以 UI 改动也是"给卡住的 provider 复位"的手段。

关闭 UI：

```yaml
web:
  enabled: false
```

JSON 接口在 `/api/*`（`status` / `logs` / `config` / `accounts`（含 `…/<id>/test`）/ `tokens` / `stats` / `agents` / `analytics` / `requests` / `shadow-report` / `events` / `pin` / `quota/refresh` / `login/*`）；底层契约（请求/响应 shape、stats 口径）见仓库根目录 `docs/web-api.md`。前端是嵌入式的静态资源（`internal/web/assets/`，`go:embed`），无独立构建步骤。

## `serve status`（终端状态面板）

`model-proxy serve status` 是 Web UI **Status 标签页的终端等价物**——一次性拉取运行中 daemon 的 `/api/status` + `/api/tokens`（带 `--logs` 时再加 `/api/logs`），按终端优化输出（列对齐 / 着色 / 紧凑数字）。需要 daemon 在跑、且 `web.enabled`（默认 true）。

```bash
model-proxy serve status            # 头部 + Providers + Schedule + Quota + Tokens
model-proxy serve status --logs     # ……再加最近 20 行日志
model-proxy serve status --logs 5   # ……最近 5 行
model-proxy serve status --json     # 合并的原始 JSON（{status, tokens[, logs]}，方便 jq）
model-proxy serve status --config /path/to/config.yaml   # 指定 config（从而选 listen 地址）
```

显示内容（与 Status 标签页一致）：

- **头部**：`v<version> · <uptime> · <listen>`
- **Providers**：每个 provider 的健康状态（`available` / `circuit open` / `rate-limited` / `unavailable`）+ 计数器（reqs / failovers / 429 / failures / **LAT / TTFT 平均延迟** / 最后请求时间）
- **Schedule**：每路由首选 provider + ordered 列表（tier / surplus / priority / 可用 / peak）+ sticky 驻留 + pin 状态
- **Quota**：每 provider 的配额窗口（ultimate / short）+ 剩余百分比 + 进度条 + 重置时间
- **Tokens**：按 provider × model 的观测用量（input / output / cache）
- **Logs**（仅 `--logs`）：最近 N 行日志

错误处理：daemon 没在跑 → `✗ cannot reach daemon at <listen>: … is 'model-proxy serve' running?`；`web.enabled: false`（`/api/status` 返 404）→ 提示开启 Web UI。每次请求带 10s 超时，daemon 卡死会快速失败而不是一直挂起。`--json` 适合脚本，如 `model-proxy serve status --json | jq .status.quota`。

## `stats`（调用统计）

`model-proxy stats` 从 daemon 的 `/api/stats` 拉取 SQLite 存储的 per-(provider, model) 调用统计并按终端表格输出。需 daemon 在跑 + `web.enabled`（默认 true）。统计在 hot path 只更新内存计数器，由后台每分钟 flush 到 `~/.model-proxy/stats.db`（重启不丢；SIGINT/SIGTERM 触发最后一次 flush）。

```bash
model-proxy stats                             # 最近 60min（默认），1 分钟桶
model-proxy stats --bucket 10m                # 按 10 分钟桶聚合展示（存储恒为 1 分钟，聚合仅展示）
model-proxy stats --from 2h --to now          # 时间范围（unix 秒或 RFC3339；默认 60min 前..now）
model-proxy stats --provider zhipu --model glm-5.2   # 过滤
model-proxy stats --by-agent                  # 按 agent 维度（claude-code/codex/opencode/pi/…）
model-proxy stats --by-agent --agent codex    # 只看某个 agent（可叠 --provider/--model）
model-proxy stats --granularity day --cost    # 日历日聚合 + 等价成本（走 /api/analytics）
model-proxy stats --json                      # 原始 JSON（便于 jq）
```

输出列：`provider · model · <bucket> · reqs · failover · 429 · fail · lat(ms) · ttft(ms) · input · output`（紧凑数字；lat/ttft 是平均总时延/首字节时延，不含客户端慢读）。`--by-agent` 输出 `agent · provider · model · reqs · fail · lat · input · output`——回答「哪个 agent 在烧配额/哪个在疯狂失败」。空结果 -> `(no stats in range …)`。`POST /api/tokens/reset`（或 Web UI）可清零内存 + SQLite + flush 基线。配置：`config.stats.{db_path, retention}`（默认 `~/.model-proxy/stats.db`，30 天；`0` = 永久）。

## Token 文件

凭据由 `login` 管理，按 provider name 派生路径，不落 config。两类凭据（apikey 池、codex/aqp OAuth store）共用**一个**后端开关：config 顶层 `credentials:`（`file` 默认 | `keychain`）。env `MP_CRED_STORE`（`file|keychain|auto`）已发布，保留为**仅作用于 OAuth 侧的显式 override**——优先级：env 非空 > config > 默认 file；`auto` 表示按 keychain 可达性探测（收敛前的旧默认行为，现为显式 opt-in）。env 与 config 不一致时（env 只覆盖了 OAuth 侧），`config check` 与启动/reload 日志各给一行提示，说明两侧各自生效值与来源（env/config/default）。注意：收敛前未设任何开关、靠 auto 默认进过 keychain 的用户，升级后 OAuth blob 默认按 file 读取——显式设 `credentials: keychain`（或 env）即可继续读到原 keychain 条目。

- **apikey 池**（`login` 写入的 `<name>_apikeys.json`）：`file`（默认）时秘密值内联在 0600 池 JSON（历史行为）；`keychain` 时秘密值 api_key/access_key/secret_key 逐条存进 OS keychain——macOS Keychain / Windows 凭据管理器 / Linux Secret Service，条目键形如 `<providerName>/<accountId>/api_key`，池文件只留 `{id, label, added_at}` 元数据。file→keychain 明文池与遗留单账号文件在首次读取时懒迁移（池文件被重写为纯元数据，遗留文件改名为 `<path>.migrated.bak` 保留一代回滚）；keychain 不可达时 fail-closed——操作报错，不静默回落明文文件。启用方式：`config.yaml` 加 `credentials: keychain` 后重新 login（或等首次读取自动迁移）。注意：macOS 首次写入可能弹钥匙串授权框；headless Linux 需要 Secret Service（gnome-keyring 或 KWallet）在运行。**keychain→file 切回有自动回迁**：池文件是纯元数据时，file 模式读取会按条目从 keychain 读回秘密并原子重写明文池（0600）；keychain 条目缺失的账号保留元数据、在 `Snapshot.ReloginNeeded`/启动日志中报出需重新 login（部分回迁，不整体失败），回迁后 keychain 条目默认保留（防误删，`logout` 是正常删除路径）。
- **codex/aqp OAuth store**（下表前两类）经 `internal/credstore` 统一读写，同一个 `credentials:` 选择后端（env `MP_CRED_STORE` 可覆盖）：`keychain` 把整个凭据 blob 存为一条 keychain 记录并懒迁移遗留明文文件（原文件改名为 `<path>.migrated.bak`）；`file` 按历史行为存 `0600` 明文文件；显式 `keychain` 而后端不可达时 fail-closed。OAuth blob **不做 keychain→file 自动回迁**（与历史 env 切换行为一致）：切回 file 后需重新 login。

测试二进制永远不触碰真实 keychain。

| Provider | Token 文件 | 内容 |
|---|---|---|
| aqp | `~/.model-proxy/aqp_oauth_auth.json` | SSO cookie + account data（单账号） |
| codex | `~/.model-proxy/codex_oauth_auth.json` | OAuth access/refresh/id token（单账号） |
| zhipu | `~/.model-proxy/zhipu_apikey.json` | API key（单账号遗留文件，只读回退） |
| deepseek | `~/.model-proxy/deepseek_apikey.json` | API key（同上） |
| volcengine | `~/.model-proxy/volcengine_apikey.json` | `{api_key, access_key, secret_key}`（同上） |

**多账号凭据池**：apikey 类 provider（static/zhipu/zcode/deepseek/volcengine/kimi-code/qwen-plan）重复 `login` 会把账号累积进**池文件** `~/.model-proxy/<name>_apikeys.json`（`{version, accounts:[{id, label, api_key, (access_key, secret_key), added_at}]}`），按账号 id（volcengine=access_key，其余=sha256(api_key)[:16]）去重。运行时每个池被展开成 N 个虚拟 provider（`<name>#<accountId>`），共享父配置但各绑自己的凭据；路由目标命名父 provider 会 fan-out 到全部账号。plural pool 是权威凭据来源：损坏或空 pool 会禁用该 provider，不会降级读取旧 singular key。**路由跨池是会话粘性的**：按请求的 `x-claude-code-session-id` 粘同一个账号（保 prompt cache），新会话 round-robin 分到不同账号（并发散开）；只有 429/熔断才换账号。`usage <provider>` 逐账号展示全部账号。aqp/codex 是单凭据（不入池）。`login --label`/`--replace`、`logout --label`/`--all` 管理池内账号；Web UI Accounts 标签页也能增删。

多实例支持：同一 `provider_id` 可有多个不同 name（如 `zhipu-personal` / `zhipu-work`），各自独立凭据文件/池。

## 协议与协议转换

代理按 URL 路径前缀路由。**默认「对外协议 = 转发协议」，同协议字节级透传**：

| 协议 | 端点 | 转发到 |
|---|---|---|
| Anthropic | `POST /v1/messages` | provider 的 `/messages` |
| OpenAI | `POST /v1/responses`, `/v1/chat/completions` | provider 的同路径 |
| 模型列表 | `GET /v1/models` | 合并 routes + 隐式路由 + claude_mapping 的模型名 |

**按协议转发到不同 endpoint**：provider 用 `openai_base_url`（默认 base，用于 OpenAI 协议 + `/models` + `usage`）和可选的 `anthropic_base_url`（覆盖 anthropic 协议；不设则用 `openai_base_url`）。如 DeepSeek 的 OpenAI 与 Anthropic 是两个不同 base。两个协议对客户端 `/v1` 前缀的处理相反：OpenAI 协议会剥掉客户端的 `/v1`，故 `openai_base_url` 自带版本段（如 `…/v1`、`…/paas/v4`）；Anthropic 协议保留客户端的 `/v1/messages`，故 `anthropic_base_url` **不带** `/v1`（如 `…/anthropic`、`…/api/plan`）。

**协议转换（opt-in）**：路由目标声明 `protocol:` 且与客户端协议不同时，代理自动做 Anthropic Messages、OpenAI Chat Completions、OpenAI Responses 三种协议的双向转换（请求 + 响应 + 流式，**tools 全链路**：`tools`/`tool_choice`/`tool_use`/`tool_result` 结构映射、流式增量事件互转、usage/cache token 透传）——比如让 Claude Code（Anthropic 协议）直连只有 OpenAI 端点的后端：

```yaml
routes:
  glm-5.2:
    # 客户端说 anthropic，zhipu 这条走它的 openai 端点 → 自动转换
    - {provider: zhipu, model: glm-5.2, priority: 1, protocol: openai}
```

转换是 per-target 的；同协议目标通常保持字节级透传，Codex Responses 例外：会清理其明确拒绝的采样/输出上限参数。Responses 客户端跨协议使用 `previous_response_id` 时，proxy 以 30 分钟短期本地状态展开完整历史（有界、0600 原子持久化；只缓存 completed/token-limit incomplete）。客户端 `stream` 与上游实际模式相反时会聚合 SSE 或合成 SSE。跨协议 4xx 保留 HTTP 状态和错误信息并改写为客户端错误格式；已知无法无损表达的请求先尝试兼容 target，最终按客户端格式返回 400 `unsupported_protocol_conversion`；缺协议终止事件、scanner 失败或工具 arguments JSON 未闭合时 fail-closed。document/input_file/file、tool_result 图片与 `is_error`、hosted web_search/tool_search（含发现工具物化）、citations 和 signed/redacted reasoning replay 均可转换或采用明确可见降级；到 Anthropic 的转换自动生成 prompt-cache breakpoints，Chat↔Responses 保留 cache key/retention。内联图片跨协议时限制为 4 MiB/4096px，413 时仅压缩重试一次（1 MiB/2048px）。MCP namespace 经 Chat/Anthropic 目标均可双向压平与恢复；custom/freeform 工具经 Chat 双向保留，经 Anthropic 时因没有等价 raw-input 契约而明确拒绝。reasoning effort 按 provider 方言渲染。unsupported server tools、audio、多 choice/logprobs 等不可表达特性会被能力扫描器拒绝，不再静默丢弃。

**自动 wire 探测（零配置）**：daemon 启动和 reload 后会异步探测每个 provider 的 `openai_base_url` 是否支持 `/responses`（一个最小请求；404 判为不支持，其余 4xx/2xx 判为支持，超时/5xx 不下结论）；`/v1/messages` 只在 provider **没有** `anthropic_base_url` 时才探（探测地址正是 anthropic 透传会打的 openai base 地址；有 `anthropic_base_url` 时直接向它透传，不会在 openai base 上拼 /v1/messages）。探测结论作为协议选择的**默认值**：没写 `protocol:` 的目标，anthropic 客户端在端点支持 responses 时自动转 responses、不支持时自动转 chat（不再默认把 anthropic body 透传给只懂 chat 的端点）；网关本身接受 anthropic（探测 `/v1/messages` 成功）或有 `anthropic_base_url` 时仍保持透传。探测完成前（unknown）一律维持透传。显式 `protocol:` 永远优先，是关闭自动行为的逃逸口。探测结论持久化在 `quota_state.json` 的 `wire_caps`（换 base_url 自动作废重探）；若探测误报支持而上游对 `/responses` 返回 404，代理会自动把该 provider 降级为 chat 并记录日志（不锁模型）。codex 已由 ProtocolHint 覆盖，不参与探测。

**录制→回放（开发工作流）**：`model-proxy wire record <provider> [--model M] [--prompt P] [--out DIR]` 对 provider 的 `/responses`、`/chat/completions`、`/v1/messages` 各发一个最小 `stream=true` 请求，把**原始上游字节**录成 `testdata/wire/<proto>_<provider>.sse`（凭据来自 `login`，与 forward 相同的请求构造；非 2xx 存 `.err` 且不覆盖已有好文件）。`testdata/wire/` 下的流会被黄金回放测试（`internal/protocol/convert_golden_test.go`）喂给所有同协议转换器做不变量断言（终态唯一、item 配对、无空帧）。录制文件不含凭据，但可能含模型输出的敏感内容——提交前人工审查。

## 请求感知路由

调度之后、转发之前，代理还会按**请求内容**微调目标（数据来自 models.dev 目录，目录不可用时全部跳过）：

- **能力过滤**：请求带图片（`image` block / `image_url`）或带 `tools` 时，剔除不支持该能力的目标；全部不匹配则跨路由找支持的模型兜底。models.dev 查不到的 provider（codex/aqp/volcengine）默认按「不支持」保守处理——可用 provider 级 `capabilities:` 手动声明覆盖（见配置节）。
- **上下文兜底（两道保险）**：① 主动——估算 prompt token（rune 感知，中文按字计、剔除 base64 图片），超过路由最大上下文时自动改道到更大上下文的模型；② 被动——上游返回 context-overflow 类 400 时识别错误形状，用更大上下文的目标**自动重试一次**（找不到更大目标才把 400 原样回给客户端）。

## 响应缓存

`cache.enabled` 开启后，对**逐字节相同**的请求（SHA-256(method+path+body)）直接重放缓存的原始响应字节（SSE 也逐字节一致），不发上游、不烧配额。命中时响应带 `x-mp-cache: hit` 头，`/api/status` 和 Web UI Status 页展示命中率；`x-mp-force-provider`/pin 生效的请求跳过缓存（保证 replay/pin 语义）。客户端断开的半截响应不会入库。改 `cache.*` 配置 reload 即生效。

**定位是「重试/重复请求盾牌」**：多轮对话 body 逐轮变长，正常会话命中率≈0；前缀复用的经济性由上游 prompt caching 覆盖，精确缓存接住的是客户端原地重试、CI/脚本里的重复单发。

## 出站安全扫描与审计（guard）

针对提示注入（prompt injection）偷凭据的场景：恶意内容诱使 agent 读取 `~/.ssh/id_rsa`、`.env`、API key 后，最常见的漏出通道是把秘密塞进发给 LLM 的请求——这道流量必经 model-proxy，因此代理在**转发前对请求 body 做一次出站扫描**，是凭据出域前的最后一道内容级闸门。（agent 直接 curl/DNS 出网的通道不经过代理，那是客户端沙箱的职责，见各家 CLI 的 sandbox/网络白名单设置。）

五层检测，全部只在命中字面量预过滤后才精读，干净 body 零正则零解码：

- **内置规则表**：53 条高置信秘密模式，其中 46 条精选自 gitleaks v8.28.0 规则集（MIT，溯源见 `internal/guard/rules.json`）——LLM 厂商 key、AWS/GCP/Azure、GitHub/GitLab/Slack/npm/PyPI token、JWT、PEM 私钥头等；上游带熵阈值的规则保留 Shannon 熵后置过滤压误报。
- **known-secret（默认开）**：把代理自己管理的凭据（账号池 API key/AK/SK、codex/aqp OAuth 文件里的 token）加入扫描集，请求体出现这些值的**原文或 base64/hex/url 编码形态**即命中 `known_secret`——零误报，防注入偷代理自身凭据。匹配集只存在于内存，随 login/logout/reload 自动更新，无需任何规则维护；OAuth token 进程内轮转（codex/aqp 原地刷新写回 auth 文件）后由后台节拍（`scheduling.quota_poll_interval`，默认 5m）自动重扫进集，最迟一个周期生效，无需 reload。
- **编码逃逸检测（默认开）**：规则前缀的 base64 三对齐/hex 变体命中后，解码外围 token 再过原规则（含熵过滤），不解码任意 span（不碰 base64 图片等正常负载）。
- **敏感路径信号（默认 log）**：`~/.ssh`、`~/.aws/credentials`、`~/.model-proxy`、`~/.gnupg`、`~/.kube/config`、`~/.docker/config.json`、`~/.config/gcloud`、`.env` 出现在请求体里即按类别告警（`ssh`/`aws_creds`/`proxy_creds`/…）——在秘密出现之前给出"意图级"信号。命中按出现位置分两级：**strong**（路径在工具调用/工具结果位——anthropic `tool_use.input`/`tool_result.content`、openai `tool_calls[].function.arguments` 与 `role:"tool"` 消息 content、responses `function_call.arguments`/`function_call_output.output`，即"agent 通过工具读敏感文件"的 MCP Tool Poisoning 特征动作）按 `guard.paths` 配置处理：live event + `("guard", <类别>)` 计数器 + 审计，block 只对 strong 生效；**weak**（正文/user 消息里提及——coding agent 讨论 `.env` 是常态）只计 `("guard", <类别>_text)` 计数器并写 action=`log-weak` 的审计记录，不发 live event（避免刷屏）、永不 block（正文提及敏感路径不阻断）。结构识别是字面量预过滤之后才做的一遍流式 JSON 扫描（干净 body 零成本）；body 非合法 JSON 或结构识别失败时全部按 weak 处理（宁低勿高）。只支持 log/block/off，不支持 redact（改路径会破坏正常编码工作）。
- **分片泄露检测（`guard.session_scan`，默认开）**：单请求扫描挡不住把秘密拆成多段、每次请求带一段的偷法。代理按 `x-claude-code-session-id` 会话头维护有界内存窗口（每会话保留最近请求 body 尾部 32KiB，LRU 上限 256 会话、总量 ≤8MiB，reload 不清、永不落盘/日志），跟踪每个 known-secret 在该会话中**按序出现的最长前缀**（每段 ≥8 字节）；后续请求补齐剩余部分即命中 `known_secret_fragmented`（计数器/live event/审计与单请求命中同通路）。只覆盖 known-secret（池凭据/OAuth token）原文形态；段间隔超过 32KiB 窗口或会话被淘汰后不追溯（有界启发式，非会话录像）；无会话头的请求不聚合（单请求扫描已覆盖）。**redact 对分片命中降级为 log**——秘密横跨多个请求，任何一个 body 都无法改写；block 拒绝补齐段所在请求（400），此前的分段已放行（它们各自是干净请求）。

动作与观测：`guard.secrets` 控制秘密类命中（log/redact/block/off），`guard.paths` 控制路径命中（log/block/off；strong 按配置、weak 恒为计数+审计，见上）。命中只上报**模式类型名/路径类别名**（live event + `("guard", <名>)` 计数器，weak 路径命中例外：不发 live event，计数器名带 `_text` 后缀），匹配内容永不落日志、事件或测试输出。同一请求同时命中两类时两类都计数/审计（secrets=block 不短路 paths 扫描），响应动作 secrets 优先、paths=block 只阻断 strong 命中。命中持久化到安全审计日志（默认 `~/.model-proxy/security*.log`，0600，按大小+按天轮转，30 天保留），用 `model-proxy audit [--kind secret|path|drift] [--from 1h] [--json]` 离线查询；`doctor --live` 检出 takeover 漂移（客户端 BASE_URL 被改离代理——API key 劫持手法）时也会写一条 `drift` 审计记录。

规则维护：你的凭据免维护（自动派生）；新 key 格式用 `guard.extra_patterns`（config 热 reload 即时生效）或向上游同步内置表（升 `rules.json` 的 upstream pin → 重抽 → review）；敏感路径用 `guard.extra_paths`。

## 调试工具：`test` / `pin` / `replay`

```bash
# test —— 端到端链路测活（路由解析 → 凭据 → 上游真实最小请求）
model-proxy test glm-5.2        # 逐 target 打印 ✓/✗ + HTTP 状态 + 原因；任一通 exit 0

# pin —— 运行期把路由钉到某 provider（排查「是不是这家后端的问题」）
model-proxy pin glm-5.2 zhipu --ttl 1h
# pin 是硬独占：被钉的 provider 挂了直接 502 也不 failover；重启即失效
# 单次请求级覆盖：请求头 x-mp-force-provider: <provider>（replay 用的就是这个）

# replay —— 从请求历史里挑一条，用另一个后端重答（需 request_log.enabled）
model-proxy replay 3fa9c2-118 --to kimi-code
# shadow 记录 / 非 /v1 路径 / 被截断的 body 会被明确拒绝
```

配合 Web UI 的 **Requests** 页（找 request_id）和 **Live** 页（实时盯着看），构成完整的本地调试闭环。

## 影子评测

新增后端时不靠猜：`shadow:` 把某路由的真实请求**异步镜像**一份到候选后端，客户端照常收主后端的响应，影子的结果落进请求日志供对比。

```yaml
shadow:
  glm-5.2:
    provider: kimi-code          # 候选后端（可跨协议，自动转换）
    sample_rate: 0.1             # 采样率（0-1；省略=1.0 全量，显式 0=关闭）。影子烧候选方配额，务必控量
    max_concurrent: 4            # 并发闸（省略=4）
```

显式设置 `protocol:` 时，配置加载会同时检查 endpoint：`anthropic` 需要候选
provider 配置 `anthropic_base_url`，`openai`/`responses` 需要
`openai_base_url`；缺失时启动或 reload 直接报错，不会运行期静默跳过。

```bash
model-proxy shadow report        # 近 24h 聚合：每对 (路由, 主, 影子) 的样本数/状态一致率/平均延迟差/响应大小比
model-proxy shadow report --from 7d
```

影子请求不污染生产（不进熔断/统计/粘性）；Web UI Requests 页用「shadow only」过滤后点任意一条，可用 `replay` 继续深挖。评测维度目前是客观指标（状态/延迟/大小），LLM judge 胜率未做。

## 多模型编排（fusion）

OpenRouter Fusion 式编排：把一条路由的请求**并行发给多个后端（panel）各答一遍**，再由**结果汇总模型（synthesizer）融合候选答案**输出最终结果——中端面板打出接近旗舰的质量，成本只有旗舰的一半量级。对客户端完全透明（就是一次普通请求）。

```yaml
fusion:
  hard-coding:                            # 工作流配置名
    panel:                                # 候选答案生成成员（2-4 个，并行调用）
      - {provider: zhipu, model: glm-5.2}
      - {provider: deepseek, model: deepseek-v4-pro}
      - {provider: kimi-code, model: kimi-k2}
    synthesizer: {provider: zhipu, model: glm-5.2}   # 用你手里最强的模型做结果汇总
    min_panel: 2                          # 可选：至少 N 份候选答案才进入汇总（默认 2）
    max_runs_per_day: 50                  # 可选：当日编排次数上限，超出→降级直打 synthesizer
    first_turn_only: true                 # 可选：仅单轮会话（无 assistant 消息）才编排
    judge: {provider: zhipu, model: glm-5.2}   # 可选：汇总前先出「共识/冲突/遗漏」评审报告
    # instruction: "..."                  # 可选：覆盖内置的结果汇总指令模板

routes:
  hard-question:
    - {provider: fusion, model: hard-coding, priority: 1}
    - {provider: zhipu, model: glm-5.2, priority: 2}   # 可叠普通 target 兜底
```

工作机制与语义：

- **quorum + grace**：凑够 `min_panel` 份候选答案就进入结果汇总阶段（再留 5 秒等差点完成的调用分支）；凑不齐就用原始请求直打 synthesizer（降级，不报错）。成员挂掉不影响（该成员自己的熔断/限频语义照常）。
- **工具轮**：带 tools 的请求照常编排——候选答案生成调用只产出文本分析（剥 `tools`/`tool_choice`），结果汇总调用保留 tools，由 synthesizer 输出 tool 调用。
- **跨协议面板**：成员可带 `protocol:`，请求自动转换（转换层的现成能力）。
- **代价（要想清楚再用）**：一次请求 = N+1 次上游调用（panel N 次候选生成 + 1 次结果汇总），延迟 ≈ 候选等待 + 汇总首字节。**只给困难路由用，别当默认路由**；多轮长会话建议配 `first_turn_only`（多轮自动降级直打 synthesizer）和 `max_runs_per_day` 控制成本。每家的消耗在 `stats` / Requests 页（`fusion-panel-*` 标记）里都看得见。
- **编排观测**：`GET /api/fusion?workflow=<名>` 返回按工作流配置的聚合（编排次数/quorum 达成率/按原因的降级计数/候选与汇总 token/放大系数）+ 最近 200 次运行明细（每条调用分支的状态/延迟/token）；`stats --provider fusion` 出时间序列（requests=编排次数，failovers=降级次数）。
- **judge 评审（可选）**：配 `judge:` 后，结果汇总前先对候选答案做一次「共识/冲突/遗漏」分析，报告注入汇总提示词（LLM-Blender「先排后融」）；judge 调用失败不影响编排。
- **验证收益**：给 fusion 路由配一条 `shadow:` 影子（或反过来），用你自己的真实负载对比「编排 vs 单模型」，别信普遍结论。

## 调度与熔断（`scheduling`）

每个对外模型可配多个 `provider/model` 目标。代理按下面的规则选目标、失败逐一 failover，并对持续出错的 provider 熔断，避免每请求都去撞一个挂掉的上游：

- **熔断**（超时 / 5xx / 连接错误 / 401 刷新后仍失败）：连续 `circuit_threshold`（默认 3）次 → 开路 `circuit_cooldown`（默认 10m），后半开放 1 个探针请求，成功关、失败再开。开路期间该 provider 被跳过。
- **限频跳过**（429，三维分类）：先采纳响应里的精确重置时间（body 文本 "reset after 2h5m"/"Resets in 164h"/RFC3339，上限 7 天），其次 `Retry-After` 头；都没有时按分类退避——日配额锁到本地次日 0 点、配额耗尽（余额/套餐窗口）等 `quota_cooldown`（默认 1h）、普通频率限制等 `rate_limit_backoff`（默认 60s）。不计入熔断。分类（transient/quota/daily）在 `serve status`（`rl:quota`/`rl:daily`）和 Web UI 健康 pill 上可见。
- **模型级锁定**（`model_lockout`，默认 10m）：上游 404（模型被移除）、400/403「模型不可用/无权限」、空 200（`Content-Length: 0` 直接 failover；流式零字节本次难免、下次 failover）→ 只锁 `(provider, model)`，**不毒化整个账号**——同账号的其他模型照常服务。单目标路由的末 target 仍把上游原始错误（404/400）原样回给客户端。
- **400 自动剥参**：上游 400 报「Unsupported parameter: 'xxx'」时，自动把该顶层参数记入 per-provider blocklist，当次剥离重试一次、后续请求预防性剥离（`model`/`messages` 等关键字段永不剥）。
- **冻结态持久化 + unfreeze**：限频/熔断冷却、模型锁、剥参 blocklist 随 `quota_state.json` 落盘，重启后按 config 指纹匹配恢复（防串配置）。异常边界（账号已充值、429 误分类、上游提前重置）用 `model-proxy unfreeze [provider]` 或 Web UI Providers 卡的 unfreeze 按钮立即解冻重试。
- **全冷却等待重试**（`retry_wait`，默认 10s，`"0"` 关闭）：当路由的**所有**目标都在冷却（限频/熔断）且最早到期 ≤ 预算时，代理静默等到期后整体重试，最多 2 次——代替立即报错让客户端走自己的重试循环；某目标在调度和终局之间恢复但本轮未被试，则**零等待立即重排**一次（仍在 2 次预算内）；客户端断开立即中止。重试耗尽或冷却超预算时按**跨轮失败类别**给出诚实终局：**纯限频 → 429 + `Retry-After`**，含硬失败/熔断成分 → 502（`x-mp-force-provider` 一次性覆盖不参与等待）。
- **粘性驻留**（`sticky_dwell`，默认 10m）：每个路由「停」在一个 provider 上，在驻留窗口内优先用它（保 prompt cache，不为已恢复的高优先 provider 频繁回切）；只有它熔断/限频或驻留到期才换。10m ≈ 2× 缓存 TTL（~5m）：够保住活跃会话缓存、扛过短暂抖动，又能在有限时间内回到首选 provider。
- **上游超时**（`upstream_timeout`，默认 1800s）：每个上游请求带超时，挂起的上游在超时后失败进入熔断/failover，而不是无限拖住请求；默认 1800s 以容纳长 thinking 流与超长输出，需要更快 failover 可调小。

```yaml
scheduling:
  circuit_threshold: 3
  circuit_cooldown: 10m
  rate_limit_backoff: 60s
  quota_cooldown: 1h          # 429 配额耗尽（无重置提示时）
  model_lockout: 10m          # 模型级失败锁定时长
  retry_wait: 10s             # 全冷却时等待重试预算（"0" 关闭）
  upstream_timeout: 1800s
  sticky_dwell: 10m
  quota_poll_interval: 5m   # 后台 Quota() 轮询周期
  quota_switch_margin: 15   # 切换 provider 的 quota 边际（百分点）
```

## 配额感知调度（quota-aware scheduling）

代理后台轮询每个 provider 的剩余配额（`Provider.Quota()`，每 `quota_poll_interval` 默认 5m 一次），缓存到 `~/.model-proxy/quota_state.json`（启动时作为基线加载，**并携带每路由 `sticky` 选择，重启后恢复 → 保 prompt cache**），并据此排序路由目标：

- **调度分 = surplus**（provider 接口方法 `Provider.Surplus`）：`surplus = (最终窗口.remaining − 短窗口.remaining × (短窗口配额/最终窗口配额) × (peakMult−1)) − 时间剩余比例`。surplus>0 = 落后节奏（不用就浪费 → 优先用）；<0 = 超前（会提前耗尽 → 回避）。最终窗口（总预算）：zhipu=周、volcengine/codex/aqp=月、deepseek=按量（无窗口）。
- **三层 tier**：`plan`（按 surplus 排）< `unknown`（按 priority 排）< `pay-as-you-go`（严格兜底，仅当所有 plan provider 都不可用）。设 `billing: pay-as-you-go` 即兜底（如 DeepSeek）。排序 `(tier, priority asc, surplus desc)` —— **priority 压过 surplus，surplus 只在同 priority 间决定**。
- **peak 只烧短窗口**：`peak_hours` 的 multiplier 只折算 provider 的短 rate-cap 窗口（5h 等）；没有短窗口的 provider（codex/aqp、未轮询的）高峰不打折。`peak_hours` 支持多段、每段独立 multiplier；multiplier=1 关闭。
- **粘性切换**：路由停在一个 provider 至少 `sticky_dwell`（保 prompt cache）；到期后仅当另一 provider 在 **tier / priority / surplus 边际（`quota_switch_margin`，默认 15 pts）** 任一更优时才换 —— 既能短暂抖动后回首选，也能在他人明显领先时切换。
- **质量打分**：每个 provider 的错误率与 TTFT 各维护一条 2m 半衰期 EWMA，折算成罚分从 surplus 中扣除（`quality_error_weight` 默认 100、`quality_ttft_weight` 默认 20，0 关闭）——持续报错/变慢的账号自动下沉，粘性账号恶化时经同一个 switch margin 逃逸；配额快照显示 ultimate 窗口已耗尽的 plan target 在粘性判定之前就被跳过（不再白挨一轮确定性 429）。

```yaml
providers:
  deepseek:
    provider_id: deepseek
    billing: pay-as-you-go          # 严格兜底（仅当所有 plan provider 不可用）
    # ...
  zhipu:
    provider_id: zhipu
    billing: plan                   # 默认；显式写也可
    peak_hours:                     # 三种写法都支持
      - {window: "14:00-18:00", multiplier: 2}
    # ...

scheduling:
  quota_poll_interval: 5m
  quota_switch_margin: 15
```

`Quota()` 来源：zhipu/codex/volcengine/aqp → plan tier；deepseek → pay-as-you-go。volcengine 的 `GetAFPUsage` 需 AccessKey/SecretKey（Ark API Key 调不了），未配时该 provider 退化为 `unknown`。

**查看调度**：`model-proxy schedule` 查询运行中的 daemon，显示每个 model 当前调度到哪个 provider（后台接口 `GET /debug/schedule`，含 ordered 列表/sticky 状态）；`model-proxy doctor` 离线诊断 config 的调度设置（每 provider tier/quota/peak + 每路由 dry-run 顺序 + shadow 配置 + warning，不需 daemon）；agent 卡住时用 `model-proxy doctor --live` 连运行中的 daemon 做实时诊断（结论先行：路由全灭原因与最早恢复时间、pin、配额将尽、takeover 漂移、最近失败请求）。

## 添加新 Provider

1. 建 `internal/provider/xxx.go`，实现 Provider 接口（embed `ApiKeyBase`（文件存 API key）+ `baseProbe`（默认探测/过滤行为））。`baseProbe` 默认：探测走 OpenAI `POST /chat/completions`、无专属请求头、候选模型透传。仅当 provider 与此不符时才 override：
   - `ProbeRequest(modelID)` -- 探测请求的 path/body（如 codex 的 `/responses` + Responses API body、aqp 的 `/v1/messages`）
   - `ExtraHeaders(req, path)` -- 每次请求（转发 + 探测）都要的专属头（如 aqp 的 `anthropic-version` + `x-compass-request-id`）
   - `FilterModelIDs(ids)` -- `models refresh` 的静态策略过滤（如 volcengine 剔除 `*-latest`/lite/mini）
2. `init()` 里 `Register("xxx", constructor)`
3. config 加 `provider_id: xxx`
4. plan 类 provider 还应实现 `Quota()`（在 `buildProviders` 里 wire `QuotaFn`），否则会被当作 `unknown`（按 priority 排）；按量计费的设 `billing: pay-as-you-go`

provider 专属的探测/过滤/请求头知识全部收敛在 `internal/provider/xxx.go`，不写进 main 包的 switch/if。不改 proxy/login/logout/usage/models 的代码。

## Demo

```bash
model-proxy serve
python3 examples/demo.py --port 15721 "hello" glm-5.2
python3 examples/demo.py --port 15721 --protocol codex "hello" gpt-5.5
```

## DeepSeek（内置，双协议）

DeepSeek 已内置（`provider_id: deepseek`），一个 API key 同时服务 OpenAI 与 Anthropic 协议。两个 endpoint 用 `openai_base_url`（OpenAI base）和 `anthropic_base_url`（Anthropic base，**不带 `/v1`**，代理保留客户端的 `/v1/messages`）分别配置；代理按调用协议转发到对应 endpoint。默认 config 含 provider 定义但**不含 routes**——按需添加：

```yaml
providers:
  deepseek:
    provider_id: deepseek
    openai_base_url: https://api.deepseek.com
    anthropic_base_url: https://api.deepseek.com/anthropic/v1
    usage_url: https://api.deepseek.com/user/balance
    models:                 # 只填模型名；元数据从 models.dev 自动补
      - deepseek-v4-pro
      - deepseek-v4-flash

claude_mapping:
  claude-opus-4-8: deepseek-v4-pro   # DeepSeek 服务端也会自动映射 claude-opus*→v4-pro

routes:                     # 可选：不配 routes 时，models 里且已 login 的模型会自动隐式路由
  deepseek-v4-pro:
    - {provider: deepseek, model: deepseek-v4-pro, priority: 1}
```

```bash
model-proxy login deepseek        # 输入 DeepSeek API key
model-proxy usage deepseek        # 查余额（is_available + 各币种 total/granted/topped-up）
```

## 火山方舟 Volcengine（含 Agent Plan，双协议）

火山方舟（Ark）已内置（`provider_id: volcengine`），一个 API key 同时服务 OpenAI 与 Anthropic 协议；两个 endpoint 用 `openai_base_url`（OpenAI base，自带版本段，代理剥客户端 `/v1`）与 `anthropic_base_url`（**不带 `/v1`**，代理保留客户端 `/v1/messages`）分别配置。**Agent Plan** 套餐用独立的 plan base（OpenAI `/api/plan/v3`、Anthropic `/api/plan`）。

```yaml
providers:
  volcengine:
    provider_id: volcengine
    openai_base_url: https://ark.cn-beijing.volces.com/api/plan/v3
    anthropic_base_url: https://ark.cn-beijing.volces.com/api/plan
    usage_url: https://ark.cn-beijing.volces.com/api/plan/v3/models
    models:
      - doubao-seed-1-8-251228

routes:
  doubao-seed-1-8-251228:
    - {provider: volcengine, model: doubao-seed-1-8-251228, priority: 1}
```

```bash
model-proxy login volcengine        # 依次输入：Ark API Key（对话）+ AccessKey/SecretKey（GetAFPUsage 用，IAM 密钥）
model-proxy usage volcengine        # Agent Plan 的 5h/每日/周/月 AFP 额度（GetAFPUsage，需 AK/SK）
```

鉴权双写（`Authorization: Bearer` + `x-api-key`）：OpenAI 端点用 Bearer，Anthropic-compatible 端点用 x-api-key，一个 key 两种协议都能用。

> **用量（GetAFPUsage）**：Agent Plan 的 5h/每日/周/月额度在 `GetAFPUsage`——火山引擎**签名 OpenAPI**（`Action=GetAFPUsage&Version=2024-01-01`，HMAC-SHA256/V4，需 **AccessKey/SecretKey**），Ark API Key（Bearer，仅对话）调不了。`login volcengine` 会同时收 Ark API Key + AK/SK；`usage volcengine` 用 V4 签名调 GetAFPUsage 显示各窗口 Quota/Used/Remaining/ResetTime。未配 AK/SK 时退化为列 config 模型。
