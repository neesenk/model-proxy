# model-proxy

多 Provider LLM 代理 — 统一管理 AQP/codex/Zhipu 等上游后端，按协议（Anthropic/OpenAI）对外暴露，自动处理鉴权、模型映射、流式转发。

**特性一览**：

- **多上游聚合 + 配额感知调度**：surplus 调度分 / 熔断 / 限频跳过 / 粘性驻留 / 多账号凭据池 + 会话粘性
- **三协议转发 + 可选协议转换**：同协议字节级透传；路由目标声明 `protocol:` 即可在 Anthropic Messages、OpenAI Chat Completions、OpenAI Responses 间转换
- **请求感知路由**：按图片/工具能力过滤目标、超长 prompt 自动改道大上下文模型、上游 400 溢出自动重试一次
- **可观测性**：Web UI 七个标签页、实时请求监视（SSE）、请求日志查询、延迟（LAT/TTFT）与按 agent 维度的统计
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
- **Routes 层**：routes 自动推导 —— 每个 provider 的 models 按模型名聚合为「对外暴露名 → 一组 `provider/model` 目标」（provider 的 `alias` 可把上游模型改名统一暴露，`priority` 继承 provider 的 priority，lower wins）。调度先看非高峰（provider 的 `peak_hours`），再看 `priority`，失败逐一 failover。claude-* 别名就是普通的显式 `routes:` 条目（全协议生效），或在客户端配置里直接写目标模型名（takeover 模板方式）；请求模型名也支持 `provider/model` 前缀（如 `deepseek/deepseek-v4-pro`）按裸模型路由并钉到该 provider；显式 `routes:` 仅用于覆盖（fusion 目标、`protocol:` 协议转换、特殊排序、claude 别名）；调度后还会按请求内容（图片/工具/上下文长度）做请求感知路由。查看：`model-proxy routes [model]` / Web UI Config
- 凭据由 `login <provider>` 管理，不落 config；config `credentials:` 统一选择 apikey 池与 codex/aqp OAuth store 的存储后端（`file` 默认 / `keychain`：秘密值进 OS keychain、池文件只留元数据），env `MP_CRED_STORE` 仅作为 OAuth 侧的显式 override

## 安装

```bash
brew tap neesenk/model-proxy
brew trust neesenk/model-proxy        # 新版 Homebrew 对第三方 tap 的 cask 要求显式信任（首次）
brew install --cask model-proxy

# 首次运行前：二进制暂未做 Apple 公证，Gatekeeper 会拦截（进程挂起或弹窗），
# 需手动移除 quarantine 属性（一次性）：
xattr -d com.apple.quarantine "$(readlink -f "$(which model-proxy)")"
```

也可从 [GitHub Releases](https://github.com/neesenk/model-proxy/releases) 直接下载对应平台的归档（含 checksums.txt；curl 下载不带 quarantine 可直接运行，浏览器下载同样需上面的 `xattr -d`），或 `go install github.com/neesenk/model-proxy@latest` 源码安装。签名公证已列入后续计划，完成后此步骤不再需要。

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
listen: 127.0.0.1:15721    # 默认回环；非回环需配置 web.auth（admin_token_file + api_keys_file），否则 validate 拒绝
log_level: info                # debug|info|warn|error 级别过滤（低于所配级别的日志被丢弃）；非法值启动报错；startup-only，改后需重启
# proxy: http://127.0.0.1:7890   # 全局上游代理：http/https/socks5 URL 或 off（强制直连）。
                               # 留空 = 自动链：环境变量(HTTPS_PROXY/HTTP_PROXY/NO_PROXY) → 系统代理 → 直连。
                               # providers.<name>.proxy_url 可逐 provider 覆盖（作用于转发/fusion/shadow 流量）；
                               # 系统代理由 internal/upstreamproxy 每进程探测一次，自动来源(env/系统)对 loopback 恒绕过，
                               # 显式配置的代理 URL 对 loopback 同样生效；PAC/WPAD 不解析

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

routes:  # claude-* 别名 = 普通显式路由（全协议生效）；也可在客户端配置目标模型名
  claude-opus-4-7: [zhipu/glm-5.2]          # 紧凑形式 "provider/model"
  claude-sonnet-4-6: [zhipu/glm-5.2]

# routes 自动推导：每个 provider 的 models 按暴露名聚合（alias 可改名），
# priority 继承 provider 的 priority（lower wins，失败逐一 failover）。
# 查看：model-proxy routes [model]

# takeover: 接管客户端配置走模板（内置预设见 `model-proxy takeover list`；
# 自定义/覆盖放 ~/.model-proxy/takeover-templates/<name>.yaml），不在 config 配置

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
#                           # 只是"发出一个地址"、本身不是安全问题——只计数，不发 live event、永不
#                           # block、不落审计记录
#   audit: true             # 默认 true：命中持久化到安全审计日志（`model-proxy audit` 查询）
#   session_scan: true      # 默认 true：分片泄露检测——同一 session（x-claude-code-session-id）多条请求
#                           # 拼出一个 known-secret 即命中 known_secret_fragmented（redact 对此降级为 log）
#   audit_path: ""          # 默认派生 <home>/.model-proxy/log/security/security.log；自定义必须是绝对路径（不展开 ~）
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
> **路由推导**：`routes` 块默认完全不用写。每个 provider `models:` 里列出的模型自动按暴露名聚合成多目标路由（多个 provider 提供同名模型即自动 failover 组），`priority` 继承 provider 的 `priority:`（lower wins），provider 的 `alias:` 可把上游模型改名后统一暴露（如 kimi-code 的 `k3` → `kimi-k3`）：请求体 model 改写为上游名，响应里的 model 字段则归一回暴露名，客户端始终只看到自己调用的名字。显式 `routes:` 仅用于覆盖：fusion 目标、`protocol:` 转换声明、特殊排序。查看生效表：`model-proxy routes [model]`、Web UI Config 页或 `GET /api/config`。

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
model-proxy takeover list            # 可用模板（内置预设 + ~/.model-proxy/takeover-templates 自定义覆盖；* 标记各族按 provider 原生协议自动选中的变体）
model-proxy takeover opencode      # 族名 claude|opencode|pi|codex|kimi|gemini-cli|all：多协议 agent 按 provider 原生协议写配置；精确模板名（pi-openai、opencode-openai、pi-responses、opencode-responses）钉住变体
# --mode unified(默认)=每族一个协议项(覆盖最多,其余走转换) | split=每种原生协议一个配置项(模型按协议划分,全部透传) | anthropic|openai|responses=归一到指定协议(族里没有该变体则回退自动选择)；TTY 下横跨多协议时会交互询问
model-proxy restore opencode

# 配置管理
model-proxy config init            # TTY 下是引导式上手向导（探测客户端→选 provider→可选 takeover→打印下一步）；管道/脚本下生成完整注释模板
model-proxy config print           # 打印生效配置
model-proxy config check           # 校验配置
model-proxy routes [model]         # 查看（自动推导的）路由表：全部暴露模型或单个模型的有序目标

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

# 手动冻结 provider：调度跳过它直到 unfreeze（排障/摘除异常上游，不改 config）
model-proxy freeze zhipu                 # 必须指定 provider（池化父名 = 全部账号），无 freeze-all

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
model-proxy guard blocks           # AI 二次判定拉黑的会话列表（guard.adjudicate）
model-proxy guard unblock <sid>    # 解除一个被拉黑的会话
model-proxy audit --kind drift --from 7d --json   # 过滤 + 原始 JSON
model-proxy audit --stats --from 7d  # 聚合视图：by kind/命中名 top10/agent top10/action（--json 出结构化聚合）

# 精确响应缓存统计（需 daemon）
model-proxy cache                  # 条目数 / 命中 / 未命中 / 命中率（--json 出原始对象）
```

## Web UI

代理内置一个管理后台（admin UI），在 `http://127.0.0.1:<listen>/ui/`（如 `listen: 127.0.0.1:15721` → <http://127.0.0.1:15721/ui/>）。UI 为 v2 设计系统版（语义状态徽章、SVG 图标、亮暗双主题、可缩放字阶）。**默认开启；回环 `listen` 下无鉴权（本地可信，非回环需 `web.auth`，见「网络部署鉴权」）**。七个标签页（Config 页含 Add provider preset 向导：选内置预设 → 合并+热重载 → Accounts 加凭据）：

- **Status** — 实时面板：Dashboard 小节复用 Analytics 页的卡片与图表，窗口钉死**最近 1 小时 · 按分钟 · 按模型**（KPI chip 行含环比 Δ%，tokens 为四桶直合 in+out+cache 写读、tok/s 为**输出解码速度**（output÷完整调用时长）；+ metric 可切换的半透明柱趋势图 + 排行榜；数据源 `GET /api/analytics?granularity=minute&by=model`，随 Status 页 tick 刷新但至少间隔 30s，图表原地更新）；原 **Model Health** 小节已合并进排行榜——每行带 status 徽标（latency/ttft/tok-s 三维阈值打分 ok/warn/err），最差维度的颜色也标在对应数值单元格上；另有 uptime / 版本 / listen 地址、每 provider 的熔断/限频状态、配额快照、每路由当前调度选择、请求计数器（含平均延迟）、观测到的 token 用量（按 provider×model）、按 agent 的用量卡片、响应缓存命中率、日志尾部。Models 小节展示启动期协议探测的每 provider×model 三协议能力矩阵（chat/anthropic/responses 的 yes/no/unknown，数据源 `GET /api/models`）。
- **Config** — 原始 YAML 编辑器（GET 返回原文件、POST 经 `validate → backup(<configDir>/.model-proxy/back/<base>.<时间戳>.bak) → atomic write → reload` 流水线落盘 + 热重载）+ 结构化编辑表单（`general` / `scheduling` / `provider` / `route` / `guard`，通过 yaml.Node API **保留注释与键序**）+ **Guard rules 编辑器**（`extra_patterns` 行编辑 name/regex/literal、`extra_paths` 行编辑，整表提交走同一 edit 管线；`guard.adjudicate` 有意只留 YAML——内容外发的显式 opt-in）。
- **Accounts** — 列出每个 provider 的账号（30s 自动刷新；测活/登录等操作进行中自动跳过）（`id` / `label` / `added_at`，aqp/codex 额外显示 email；**响应结构里根本没有 key 字段，secret 不可能被序列化出去**）；apikey 类 provider 可在 UI 添加/删除账号；**每个账号卡片有 Test 按钮**（真实最小请求测活，显示 HTTP 状态 + 延迟），多账号 provider 另有 **Test all**（逐个真实探测出结果矩阵，表头联动启动期三协议探测的模型能力计数，与 Status 页 Models 卡同源）；aqp/codex 走**异步登录**（浏览器完成 SSO / OAuth device flow → UI 轮询直到 `done`/`error`）。
- **Analytics** — 分析视图：KPI 行（requests/tokens/cost/failures，各带等长前窗环比 Δ%；failures chip 悬停给 failover 尝试/429 限频分解）+ **单张 metric 可切换趋势图**（tokens/cost/requests/**failovers/429s**/errors/latency/ttft/cache，全部半透明柱状；failovers/429s 是重试尝试与上游限频计数、agent 维度禁用（该表无此列）；悬停 tooltip；图例超两行收进 "+N more"；x 轴本地 24h 制、右侧留白）+ 页面最底是**一年 token 用量热力图**（GitHub 贡献图式：周列 × Mon..Sun 行、正方形格子按当日 token 总量五档着色并铺满卡片宽、横轴月份标签居中作参考；固定「12 整月 + 当月至今」窗口，不随工具栏时间/粒度/metric 变化；悬停用与会话时间线同款的浮动 tooltip 给日期 + requests/tokens/cost/err%/延迟，空格子也给日期 + no usage）+ 排行榜表格（err%/avg lat/$ 每 1M tok/cost share，err% 悬停给失败/failover/429 分解；默认按当前 metric 排序，**列头可点击固定排序**（再点切方向、无数据行恒最后）；**点击行下钻到 Requests 页**并带上该 series 的 provider/model（agent 维度再加 agent）过滤）。时间选择器与 Status→Token usage 同款（预设+双月日历），粒度 Auto/Minute/Hour/Day/Week/Month 与窗口跨度联动（小时窗口只剩 Minute）；provider/model/agent 三个 datalist 过滤（`agent=` 在两个维度下都收窄，建议来自窗口内 facet）；By Model / By Agent 切维度；价格来自 OpenRouter 目录或 config `prices:`，未定价显示 `n/a` 并在 cost chip 标记。per-(provider,model) 的 token/请求总量在 Status 页 Token Usage，两页不重复。
- **Requests** — 请求日志查询（需 `request_log.enabled`）：按 session/model/provider/状态/时间/影子过滤，表格与 Live 页**同列同渲染器**（time/agent/session/status/model/provider/ms/tokens——含 cache read；bytes 列已移除），点击行展开请求详情：**默认渲染人读对话视图**（角色分栏的消息流——user 提示词、assistant 回复、thinking 与工具结果折叠、工具调用与 JSON 工具结果渲染为可读的 `key: value` 文本而非 JSON 语法、token 用量行；请求侧最近 4 轮直读，更早历史折叠零内容、展开即**平铺完整对话**进滚动框按 25 轮分块加载——千轮会话展开 ~35KB 首块秒开，滚动即续），**原始 request/response body 折叠在下方，首次展开时才渲染且 64KB 分块滚动加载**（未点开零开销，多 MB body 不再卡死页面）；影子评测的记录带 `shadow` 徽标。顶部 **session 下拉**（选项来自 `/api/sessions`）选中后，表格上方渲染与 Live 页**同一实现**的会话视图：汇总 chips（请求数 / input+output / 缓存读写 / 平均延迟 / 错误 / 等价成本 / model、provider）+ 健康度 chips（跨度/活跃时长、p50/p95 延迟、ttft p50、failover 次数、缓存命中率、tok/s、模型分布、shadow 数）+ **Trace 时间线**（条形悬停显示请求摘要与「用户输入 + 模型返回」两段摘录，条形点击**定位到该请求行**——滚动到视野中央、闪烁标记并展开 inline 详情；长会话的表格滚到几百行深时会话视图**悬浮固定**在顶栏下方不跟丢，表头联动钉在悬浮视图正下方）；表格按 `session=` 过滤，仍可与 model/provider/shadow/errors 叠加。**过滤器状态随 URL hash 保持**（`#requests?session=…&agent=…`，只携带非默认值）：刷新页面或分享链接落在同一视图而不是回到全量列表，浏览器前进/后退在过滤器状态间切换。
- **Security** — 安全视图（审计记录需 `guard.audit`；30s 自动刷新，交互中自动让位）：顶部 KPI 行（拉黑会话 / verdict 汇总——计数源自合并流，审计 verdict 记录跨重启存活，不随内存 ring 清零归零；判定通道 LLM 用量——真实模型调用次数与 in/out token，缓存命中与在途去重不计费，通道未配置时显示单个 off tile）；**Rule hits** 卡按规则聚合当前窗口命中数（审计记录 + 被屏蔽的 low），高频 pattern 规则在判定通道关闭时标 "noisy — adjudicate can suppress" 降噪提示，点击规则行下钻 Activity 到该规则（再点/✕ chip 清除）；**Blocked sessions** 卡列出 `guard.adjudicate` 高 verdict 拉黑的会话，逐行解除或一键全部解除（持久化跨重启），session 列点击跳转 Requests 会话视图（hash 下钻，Back 返回）；**Activity** 合并时间线把审计记录（秘密/路径类型、takeover 漂移）与 AI 判定 verdict 放进同一 feed 新到旧展示——fresh verdict 双写去重（同一事件只显一行，judge 归属随行），被屏蔽的 low verdict 只在这里可见；过滤器：kind 与时间窗口（All Time/24h/7d/30d）服务端生效 + Show More 逐级加深（至 1,000 行）、verdict/规则客户端过滤，全部进 URL hash（刷新/分享保留）。全部只展示类型名与路由元数据，匹配内容永不进入 UI。每个客户端会话的 token 等价成本汇总在 `/api/sessions`（Requests 页同源数据）。
- **Live** — 实时请求监视（SSE 推送）：哪个 agent 正在发请求、路由到哪个上游、状态/token/耗时——抓「疯狂重试的 agent」就靠它。顶部 **session 选择器**可选一个客户端会话（选择随 URL hash 保持，`#status/live?session=…`——刷新/分享链接/前进后退还原同一会话视图），看该会话的请求分析：**Trace 时间线**（请求 span 泳道图 + 累计 token 曲线，琥珀点＝发生过 failover 重试，红条＝错误响应，条形悬停显示该请求摘要（时间/agent/model→provider/状态/延迟/token）并懒加载**两段内容摘录**（这轮用户输入 + 模型返回，各 ≤220 字符、无标签前缀、以弱/正常文本区分，与详情行共享缓存；纯工具调用轮显示 `[tool_use: …]` 标记、纯思考轮 `[thinking]`、错误响应显示 error 消息——coding agent 的大量工具轮不再空白），条形可点开详情；**长空闲间隙自动压缩**——2 分钟无请求即断轴并以 ⫽ 分隔，短突发在跨天会话里仍占可读宽度；**横向拖拽缩放**进任意时间窗，zoom 状态跨 SSE 重渲染存活，reset 一键还原；泳道超过 14 条时压缩行高；成本保持会话级汇总——定价归服务端）+ 汇总 chips（请求数 / input+output / 缓存读写 / 平均延迟 / 错误 / 等价成本 / model、provider），实时行与持久化行合并（表格与 Requests 页同列同渲染器、表头同样悬浮跟随），逐行可展开 body（详情与 Requests 页同一人读对话视图，原始 body 折叠），会话视图悬浮固定在顶栏下方、表头联动跟随。会话 id 来自 `request_log.session_headers` 允许列表（Claude Code/OpenCode 默认带；pi 需 takeover 模板写入的 `compat.sendSessionAffinityHeaders`）。

**所有写操作都会即时热重载运行中的 serve（进程内 `p.reload`，无需重启）**：改 config、增删账号、aqp/codex 登录完成 —— 改动立即生效。账号增删虽不改 `config.yaml`，但 reload 会重建 providers（重新读池文件），新加/删除的账号随即（取消）展开成虚拟 provider；reload 还会顺手清空熔断/限频/粘性状态并重建响应缓存，所以 UI 改动也是"给卡住的 provider 复位"的手段。

关闭 UI：

```yaml
web:
  enabled: false
```

JSON 接口在 `/api/*`（`status` / `models` / `logs` / `config` / `accounts`（含 `…/<id>/test`）/ `tokens` / `stats` / `agents` / `analytics` / `sessions` / `requests` / `security` / `shadow-report` / `events` / `pin` / `quota/refresh` / `login/*`）；底层契约（请求/响应 shape、stats 口径）见仓库根目录 `docs/web-api.md`。前端是嵌入式的静态资源（`internal/web/assets/`，`go:embed`），无独立构建步骤。

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

## 网络部署鉴权（可选）与 Prometheus 指标

默认部署（回环 `listen`）保持零配置零鉴权。要在局域网/小团队共享代理时，配置 `web.auth` 后 `listen` 才允许非回环（validate fail-closed，两者必配）：

```yaml
listen: 0.0.0.0:15721          # 非回环——必须配 web.auth
web:
  auth:
    admin_token_file: ~/.model-proxy/admin_token   # 管理数据面（/api /metrics；UI 用它建立会话）
    api_keys_file: ~/.model-proxy/api_keys         # 转发面（客户端 key，每行一个，支持 # 注释）
```

- 两个面独立鉴权：管理 API/metrics 校验 `admin_token_file`；转发端点（`/v1/*`、`/messages`、`/v1/models`）校验 `api_keys_file`。`Authorization: Bearer` 与 `x-api-key` 均接受。`/health` 保持开放（存活探针）。
- 浏览器可直接打开 `http://<LAN-IP>:15721/ui/`（或 config `listen` 指定的同名 hostname:port）：静态 UI 是无秘密 bootstrap，首次访问会要求 admin token，并换取 `HttpOnly`、`SameSite=Strict`、仅限 `/api` 的会话 cookie，Live/EventSource 也可正常使用。其他 DNS Host 仍被拒绝以防 DNS rebinding；反向代理应把上游 Host 固定为配置的 listen host 或 IP/loopback。直接 HTTP 无法防同网段窃听，团队部署优先 VPN、SSH tunnel 或 TLS。
- 密钥比较 constant-time；文件编辑（换 key/撤销）≤10s 生效，无需重启。
- 回环部署也可以单独开启任一面（例如本地也想给转发加 key）。
- **takeover 注意**：开启 api_keys 后客户端配置里的 `PROXY_MANAGED` 占位符会被拒绝——把 takeover 写入的 key 换成 `api_keys_file` 中的一行。

`GET /metrics` 输出 Prometheus 文本指标（随 `web.enabled`）：`model_proxy_{requests,failures,failovers,rate_limited_429}_total` 与 `model_proxy_{latency,ttft}_milliseconds_sum`，按 `provider` 标签聚合（虚拟键 guard/attempts/fusion/routing 与 `/api/stats` 同语义）。

## Token 文件

凭据由 `login` 管理，按 provider name 派生路径，不落 config。两类凭据（apikey 池、codex/aqp OAuth store）共用**一个**后端开关：config 顶层 `credentials:`（`file` 默认 | `keychain`）。env `MP_CRED_STORE`（`file|keychain|auto`）已发布，保留为**仅作用于 OAuth 侧的显式 override**——优先级：env 非空 > config > 默认 file；`auto` 表示按 keychain 可达性探测（收敛前的旧默认行为，现为显式 opt-in）。env 与 config 不一致时（env 只覆盖了 OAuth 侧），`config check` 与启动/reload 日志各给一行提示，说明两侧各自生效值与来源（env/config/default）。注意：收敛前未设任何开关、靠 auto 默认进过 keychain 的用户，升级后 OAuth blob 默认按 file 读取——显式设 `credentials: keychain`（或 env）即可继续读到原 keychain 条目。

- **apikey 池**（`login` 写入的 `<name>_apikeys.json`）：`file`（默认）时秘密值内联在 0600 池 JSON（历史行为）；`keychain` 时秘密值 api_key/access_key/secret_key 逐条存进 OS keychain——macOS Keychain / Windows 凭据管理器 / Linux Secret Service，条目键形如 `<providerName>/<accountId>/api_key`，池文件只留 `{id, label, added_at}` 元数据。file→keychain 明文池与遗留单账号文件在首次读取时懒迁移（池文件被重写为纯元数据，遗留文件改名为 `<path>.migrated.bak` 保留一代回滚）；keychain 不可达时 fail-closed——操作报错，不静默回落明文文件。启用方式：`config.yaml` 加 `credentials: keychain` 后重新 login（或等首次读取自动迁移）。注意：macOS 首次写入可能弹钥匙串授权框；headless Linux 需要 Secret Service（gnome-keyring 或 KWallet）在运行。**keychain→file 切回有自动回迁**：池文件是纯元数据时，file 模式按条目读回秘密；只有全量恢复且账号 ID 与原 keychain namespace 一致才原子写入 0600 明文池。缺条目、不完整 Volcengine AK/SK 或非规范 ID 的账号保留元数据并报告需重新 login；未处理元数据不能被一次普通 Save 静默丢弃。完整回迁会留下仅含 canonical ID 的 0600 `<pool>.keychain-origin`，读取时仍保留 keychain 副本；之后 `logout`/Web 删除按该标记清理，失败可重试。纯 file 历史无标记时不访问 keychain。
- **codex/aqp OAuth store**（下表前两类）经 `internal/credstore` 统一读写，同一个 `credentials:` 选择后端（env `MP_CRED_STORE` 可覆盖）：`keychain` 把整个凭据 blob 存为一条 keychain 记录并懒迁移遗留明文文件（原文件改名为 `<path>.migrated.bak`），同时写无秘密的 0600 `<authfile>.keychain-origin`；`file` 按历史行为存 `0600` 明文文件；显式 `keychain` 而后端不可达时 fail-closed。OAuth blob **不做 keychain→file 自动回迁**（与历史 env 切换行为一致）：切回 file 后需重新 login；但 logout/Web 删除会通过 credstore 清理旧 keychain 条目，失败时保留来源标记供下次重试，纯 file 历史不触碰 keychain。

测试二进制永远不触碰真实 keychain。

| Provider | Token 文件 | 内容 |
|---|---|---|
| aqp | `~/.model-proxy/aqp_oauth_auth.json` | SSO cookie + account data（单账号） |
| codex | `~/.model-proxy/codex_oauth_auth.json` | OAuth access/refresh/id token（单账号） |
| zhipu | `~/.model-proxy/zhipu_apikey.json` | API key（单账号遗留文件，只读回退） |
| deepseek | `~/.model-proxy/deepseek_apikey.json` | API key（同上） |
| volcengine | `~/.model-proxy/volcengine_apikey.json` | `{api_key, access_key, secret_key}`（同上） |

**多账号凭据池**：apikey 类 provider（static/zhipu/zcode/deepseek/volcengine/kimi-code/qwen-plan）重复 `login` 会把账号累积进**池文件** `~/.model-proxy/<name>_apikeys.json`（`{version, accounts:[{id, label, api_key, (access_key, secret_key), added_at}]}`），按账号 id（volcengine 优先 `sha256(access_key)[:16]`，无 AK 时回落 `sha256(api_key)[:16]`；其余为 `sha256(api_key)[:16]`）去重，ID 永不携带原始凭据。运行时每个池被展开成 N 个虚拟 provider（`<name>#<accountId>`），共享父配置但各绑自己的凭据；路由目标命名父 provider 会 fan-out 到全部账号。plural pool 是权威凭据来源：损坏或空 pool 会禁用该 provider，不会降级读取旧 singular key。**路由跨池是会话粘性的**：按请求的 `x-claude-code-session-id` 粘同一个账号（保 prompt cache），新会话 round-robin 分到不同账号（并发散开）；只有 429/熔断才换账号。`usage <provider>` 逐账号展示全部账号。aqp/codex 是单凭据（不入池）。`login --label`/`--replace`、`logout --label`/`--all` 管理池内账号；Web UI Accounts 标签页也能增删。

多实例支持：同一 `provider_id` 可有多个不同 name（如 `zhipu-personal` / `zhipu-work`），各自独立凭据文件/池。

## 协议与协议转换

代理按 URL 路径前缀路由。**默认「对外协议 = 转发协议」，同协议字节级透传**：

| 协议 | 端点 | 转发到 |
|---|---|---|
| Anthropic | `POST /v1/messages` | provider 的 `/messages` |
| OpenAI | `POST /v1/responses`, `/v1/chat/completions` | provider 的同路径 |
| 模型列表 | `GET /v1/models` | 合并显式路由 + 推导路由的模型名 |

**按协议转发到不同 endpoint**：provider 用 `openai_base_url`（默认 base，用于 OpenAI 协议 + `/models` + `usage`）和可选的 `anthropic_base_url`（覆盖 anthropic 协议；不设则用 `openai_base_url`）。如 DeepSeek 的 OpenAI 与 Anthropic 是两个不同 base。两个协议对客户端 `/v1` 前缀的处理相反：OpenAI 协议会剥掉客户端的 `/v1`，故 `openai_base_url` 自带版本段（如 `…/v1`、`…/paas/v4`）；Anthropic 协议保留客户端的 `/v1/messages`，故 `anthropic_base_url` **不带** `/v1`（如 `…/anthropic`、`…/api/plan`）。

**协议转换（opt-in）**：路由目标声明 `protocol:` 且与客户端协议不同时，代理自动做 Anthropic Messages、OpenAI Chat Completions、OpenAI Responses 三种协议的双向转换（请求 + 响应 + 流式，**tools 全链路**：`tools`/`tool_choice`/`tool_use`/`tool_result` 结构映射、流式增量事件互转、usage/cache token 透传）——比如让 Claude Code（Anthropic 协议）直连只有 OpenAI 端点的后端：

```yaml
# 显式 routes 仅在需要覆盖推导时写（如声明跨协议转换）：
routes:
  glm-5.2:
    # 客户端说 anthropic，zhipu 这条走它的 openai 端点 → 自动转换
    - {provider: zhipu, model: glm-5.2, protocol: openai}
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

命中/未命中统计跨 reload 和重启保留，reload 仅清空响应条目。Reset counters 即使在缓存关闭时也清理持久化历史；完整语义见 [缓存统计契约](docs/architecture/fusion-shadow-cache.md#精确响应缓存)。Web 模型 Refresh 支持请求取消，并在并发修改 provider 时返回冲突供重试，见 [模型刷新提交边界](docs/web-api.md#模型刷新提交边界)。

## 出站安全扫描与审计（guard）

针对提示注入（prompt injection）偷凭据的场景：恶意内容诱使 agent 读取 `~/.ssh/id_rsa`、`.env`、API key 后，最常见的漏出通道是把秘密塞进发给 LLM 的请求——这道流量必经 model-proxy，因此代理在**转发前对请求 body 做一次出站扫描**，是凭据出域前的最后一道内容级闸门。（agent 直接 curl/DNS 出网的通道不经过代理，那是客户端沙箱的职责，见各家 CLI 的 sandbox/网络白名单设置。）

五层检测，全部只在命中字面量预过滤后才精读，干净 body 零正则零解码：

- **内置规则表**：53 条高置信秘密模式，其中 46 条精选自 gitleaks v8.28.0 规则集（MIT，溯源见 `internal/guard/rules.json`）——LLM 厂商 key、AWS/GCP/Azure、GitHub/GitLab/Slack/npm/PyPI token、JWT、PEM 私钥头等；上游带熵阈值的规则保留 Shannon 熵后置过滤压误报。
- **known-secret（默认开）**：把代理自己管理的凭据（账号池 API key/AK/SK、codex/aqp OAuth 文件里的 token）加入扫描集，请求体出现这些值的**原文或 base64/hex/url 编码形态**即命中 `known_secret`——零误报，防注入偷代理自身凭据。匹配集只存在于内存，随 login/logout/reload 自动更新，无需任何规则维护；OAuth token 进程内轮转（codex/aqp 原地刷新写回 auth 文件）后由后台节拍（`scheduling.quota_poll_interval`，默认 5m）自动重扫进集，最迟一个周期生效，无需 reload。
- **编码逃逸检测（默认开）**：规则前缀的 base64 三对齐/hex 变体命中后，解码外围 token 再过原规则（含熵过滤），不解码任意 span（不碰 base64 图片等正常负载）。
- **敏感路径信号（默认 log）**：`~/.ssh`、`~/.aws/credentials`、`~/.gnupg`、`~/.kube/config`、`~/.docker/config.json`、`~/.config/gcloud`、`.env` 出现在请求体里即按类别告警（`ssh`/`aws_creds`/…）——在秘密出现之前给出"意图级"信号。命中按出现位置分两级：**strong**（路径在工具**调用侧**——anthropic `tool_use.input`、openai `tool_calls[].function.arguments`、responses `function_call.arguments`，即"agent 发起读敏感文件的动作"的 MCP Tool Poisoning 特征）按 `guard.paths` 配置处理：live event + `("guard", <类别>)` 计数器 + 审计，block 只对 strong 生效；**weak**（正文/user 消息、以及工具**结果侧**内容里提及——tool_result/`role:"tool"`/function_call_output 里的文档、源码、报错文本提到路径只是"发出一个地址"，本身不是安全问题；结果里的真实秘密内容仍由 secret 通道精确兜底）完全忽略——不计数、不发 live event、永不 block、不落审计记录（良性"地址提及"的任何观测面都是噪音）。结构识别是字面量预过滤之后才做的一遍流式 JSON 扫描（干净 body 零成本）；body 非合法 JSON 或结构识别失败时全部按 weak 处理（宁低勿高）。只支持 log/block/off，不支持 redact（改路径会破坏正常编码工作）。
- **分片泄露检测（`guard.session_scan`，默认开）**：单请求扫描挡不住把秘密拆成多段、每次请求带一段的偷法。代理按 `x-claude-code-session-id` 会话头维护有界内存窗口（每会话保留最近请求 body 尾部 32KiB，LRU 上限 256 会话、总量 ≤8MiB，reload 不清、永不落盘/日志），跟踪每个 known-secret 在该会话中**按序出现的最长前缀**（每段 ≥8 字节）；后续请求补齐剩余部分即命中 `known_secret_fragmented`（计数器/live event/审计与单请求命中同通路）。只覆盖 known-secret（池凭据/OAuth token）原文形态；段间隔超过 32KiB 窗口或会话被淘汰后不追溯（有界启发式，非会话录像）；无会话头的请求不聚合（单请求扫描已覆盖）。**redact 对分片命中降级为 log**——秘密横跨多个请求，任何一个 body 都无法改写；block 拒绝补齐段所在请求（400），此前的分段已放行（它们各自是干净请求）。

动作与观测：`guard.secrets` 控制秘密类命中（log/redact/block/off），`guard.paths` 控制路径命中（log/block/off；只作用于 strong，weak 完全忽略，见上）。命中只上报**模式类型名/路径类别名**（live event + `("guard", <名>)` 计数器），匹配内容永不落日志、事件或测试输出。同一请求同时命中两类时两类都计数/审计（secrets=block 不短路 paths 扫描），响应动作 secrets 优先、paths=block 只阻断 strong 命中。命中持久化到安全审计日志（默认 `~/.model-proxy/log/security/security*.log`，0600，与请求日志同一持久化模式：活动文件按天命名、同日重启追加同一文件，超大小归档轮转，30 天保留），用 `model-proxy audit [--kind secret|path|drift] [--from 1h] [--json]` 离线查询；`doctor --live` 检出 takeover 漂移（客户端 BASE_URL 被改离代理——API key 劫持手法）时也会写一条 `drift` 审计记录。

**AI 二次判定（`guard.adjudicate`，默认关；本仓库随附的 `config.yaml` 是显式开启的示例）**：规则表/custom 秘密命中与 strong 路径命中在 `secrets/paths = log` 档下不再立即记录——命中片段（±256B 上下文，窗口内**其它**秘密命中先掩码）异步发给指定模型（`guard.adjudicate.model`，provider 直连 `/v1/messages`，不进转发管线：不重扫 guard、不进 cache/request log/forward 统计）判定：**high**＝真实泄露 → 审计记录（`verdict` 字段）+ 可选拉黑该会话；**low**＝fixture/示例/文档等良性内容 → 屏蔽（只留 `adjudicated_low` 计数器与 WebUI 最近判定 feed，不落审计）。结果按内容 hash 缓存并持久化（`~/.model-proxy/guard_verdicts.json`，只存 hash→verdict）——会话历史回显同一片段只计费一次；判定通道自带 LLM 用量记账（真实调用次数与 in/out token，缓存命中不计费），见 WebUI Security 页 KPI 与 `/api/security/adjudications` 的 `stats` 字段。会话拉黑持久化（`guard_blocks.json`）直到显式解除：`model-proxy guard unblock <session-id>` 或 WebUI Security 页。队列满、单请求判定数超上限（4 个 job）、跨规则同内容去重、模型错误、超时一律 **fail-open** 回到经典立即记录（verdict=error/skipped），绝不因判定器不可用而静音 guard。known-secret 精确通道与 secrets/paths=block 同步拦截不参与判定。注意：开启即表示接受把命中片段发给指定模型（这是"凭据不出机器"红线的显式 opt-in 例外，可用本地模型）。

规则维护：你的凭据免维护（自动派生）；新 key 格式用 `guard.extra_patterns`、敏感路径用 `guard.extra_paths`——Config 页的 Guard rules 编辑器或直接改 config（热 reload 即时生效）；也可以向上游同步内置表（升 `rules.json` 的 upstream pin → 重抽 → review）。哪些规则在产生噪音看 Security 页的 Rule hits 卡。

## 安全功能使用指南

上面是机制，这里是按场景的用法。默认配置（全 log）下**装好即受保护、不打扰**——先跑起来观察，再按需收紧。

### 上手：从零到受保护（5 分钟）

```bash
brew tap neesenk/model-proxy && brew trust neesenk/model-proxy && brew install --cask model-proxy
xattr -d com.apple.quarantine "$(readlink -f "$(which model-proxy)")"   # 首次（未公证，见安装节）

model-proxy add zhipu                      # 预设接入：一条命令完成 config + 登录（或 login codex --from-codex 复用官方登录态）
model-proxy takeover claude                # 接管客户端（写完自动复检漂移，异常会警示并留审计记录）
model-proxy serve daemon                   # 启动
```

此时 guard 已在工作：秘密/路径命中走 log——不阻断、只记录。

### 日常观测：命中了怎么看

- **Web UI**(`http://127.0.0.1:15721/ui/`):Live 页实时看 guard 事件（⚑ 徽标行）;Security 页按 kind/时间翻审计记录。
- **终端**:`model-proxy audit` 翻记录；`model-proxy audit --stats --from 7d` 看聚合（哪类命中多、哪个 agent 在触发）;`stats` 里 `guard` 虚拟 provider 的计数器看趋势。
- **漂移**:`model-proxy doctor --live`——客户端 BASE_URL 被改离代理（key 劫持手法）会有 ⚠ 提示并写 drift 审计。

### 收紧防护（按需，别一上来就开）

```yaml
guard:
  secrets: redact    # 观察期确认误报可接受后：命中内容替换 [REDACTED] 转发（agent 收到脱敏文本，工作不中断）
  # secrets: block   # 最严：直接 400——agent 会报错重试，适合高敏环境；注意 redact/block 对分片命中分别降级为 log/仅拦补齐段
  paths: block       # 只拦"工具调用里读敏感路径"（strong）；正文讨论 .env 永不阻断，可放心开
```

改完 `serve reload` 即生效。block 误伤了正常请求？把动作调回 log、或把该格式加进下一条的自定义排除（用更精确的 extra_patterns 名字区分）。

### 自定义规则

```yaml
guard:
  extra_patterns:    # 公司内部 token 格式，热 reload 生效
    - {name: corp_token, regex: '\bct-[A-Za-z0-9]{32,}', literal: 'ct-'}
  extra_paths: [~/.company/secrets]
```

`config check` 会显示 guard 生效摘要（内置表 + N 条自定义 + 各开关状态），写完规则先跑一下确认加载。

### 凭据放系统钥匙串（可选）

`config.yaml` 加 `credentials: keychain`——apikey 池秘密值进 OS keychain，池文件只留元数据；已有的明文池首次读取自动迁移并擦除。macOS 首次写入可能弹钥匙串授权框；切回 `file` 会自动从钥匙串回迁（个别账号条目缺失会提示重新 login）。keychain 不可用时操作直接报错，不会静默回落明文。

### 边界（什么不归代理管）

agent 被诱导**直接 curl/DNS 出网**偷数据不经过本代理——那是客户端沙箱的职责（Codex 默认关网络、Claude Code 的 sandbox 模式），guard 管的是"秘密混在发给 LLM 的请求里"这条最常见的通道。已知检测限制：分片检测只覆盖秘密原文形态（编码分片不覆盖）；段间隔超 32KiB 不追溯。

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

# fusion 目标通过显式 routes 暴露（推导不含 fusion）：
routes:
  hard-question:
    - {provider: fusion, model: hard-coding}
    - {provider: zhipu, model: glm-5.2}   # 可叠普通 target 兜底
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
- **冻结态持久化 + unfreeze/freeze**：限频/熔断冷却、模型锁、剥参 blocklist 随 `quota_state.json` 落盘，重启后按 config 指纹匹配恢复（防串配置）。异常边界（账号已充值、429 误分类、上游提前重置）用 `model-proxy unfreeze [provider]` 或 Web UI Providers 卡的 unfreeze 按钮立即解冻重试；反向地，`model-proxy freeze <provider>` 或 Providers 卡的 freeze 按钮把 provider 显式冻结（调度排除、无到期、成功/失败记录不解冻，必须指定 provider——无 freeze-all，同样按指纹落盘恢复），直到 unfreeze。
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

交互式协议转换测试工具（基于 pi agent 接口，真实多轮 + 工具调用 + thinking + 图片 + MCP 覆盖
anthropic/chat/responses 三协议 ingress）见 `tools/agenttest/README.md`；
`tools/agenttest/e2e.mjs` 一键串起真实 agent 项目（`projects/` 可插拔）+ matrix
+ 代理后端日志与 `/api/status` 内部数据分析。

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

routes:  # claude 别名 = 普通显式路由（全协议生效），可选
  claude-opus-4-8: [deepseek/deepseek-v4-pro]  # DeepSeek 服务端也会自动映射 claude-opus*→v4-pro

# routes 自动推导：models 里列出的模型即自动暴露为路由（priority 继承
# provider 的 priority；不声明 claude 别名时不需要写 routes 块）。
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
# routes 自动推导：无需 routes 块。
```

```bash
model-proxy login volcengine        # 依次输入：Ark API Key（对话）+ AccessKey/SecretKey（GetAFPUsage 用，IAM 密钥）
model-proxy usage volcengine        # Agent Plan 的 5h/每日/周/月 AFP 额度（GetAFPUsage，需 AK/SK）
```

鉴权双写（`Authorization: Bearer` + `x-api-key`）：OpenAI 端点用 Bearer，Anthropic-compatible 端点用 x-api-key，一个 key 两种协议都能用。

> **用量（GetAFPUsage）**：Agent Plan 的 5h/每日/周/月额度在 `GetAFPUsage`——火山引擎**签名 OpenAPI**（`Action=GetAFPUsage&Version=2024-01-01`，HMAC-SHA256/V4，需 **AccessKey/SecretKey**），Ark API Key（Bearer，仅对话）调不了。`login volcengine` 会同时收 Ark API Key + AK/SK；`usage volcengine` 用 V4 签名调 GetAFPUsage 显示各窗口 Quota/Used/Remaining/ResetTime。未配 AK/SK 时退化为列 config 模型。
