# ais-switch-proxy

AIS Switch 本地代理的**独立、可移植**实现。把 AIS Switch（Mac-only Tauri 应用）内嵌代理的四个能力抽成单二进制程序，可交叉编译到 Linux：

1. **认证** — Compass SSO 登录（`GET /auth/login` 拿登录 URL + `SSO_A` cookie，轮询 `/auth/info` 升级为 `SSO_C`）→ 用 SSO cookie 向 Compass 网关换取 CQP API key
2. **token** — 内存缓存 key + 上游 401 时自动刷新重试
3. **配置更新** — `takeover`/`restore` 改写客户端配置文件指向代理（claude / opencode / codex / pi）
4. **本地路由** — 按 URL 路径路由 + 模型别名映射 + 鉴权注入 + SSE 流式转发

不依赖 AIS Switch 应用、不依赖它的 SQLite DB，纯配置驱动。

## 构建

```bash
cd ais-switch-proxy
go build -o ais-switch-proxy .

# 交叉编译 Linux
GOOS=linux GOARCH=amd64 go build -o ais-switch-proxy-linux .
# 或 arm64: GOOS=linux GOARCH=arm64 go build -o ais-switch-proxy-linux-arm64 .
```

## 配置

`config.yaml`（可用 `ais-switch-proxy config init` 生成模板）。路径支持 `~` 展开与 `env:VAR`。

关键段：
- 顶层: `listen`(监听地址)、`log_level`、`log_file`(运行时日志 + pid 文件;不配则默认 `$TMPDIR/ais-switch-proxy.log`/`/tmp/ais-switch-proxy.log`,pid 同目录 `.pid`。`serve --daemon` 写入此文件,前台配了也会镜像)
- `auth`: SSO cookie 文件路径、CQP 换取端点、可选 `static_key`（跳过换取）、gemini key env
- `routes`: 每条路由 = `path_prefixes` + `upstream` + `auth`(cqp/gemini_key/static/none) + `model_map`(别名→真实模型名)
- `takeover`: `proxy_url` + 各客户端配置文件路径

### Linux 上无 AIS Switch 怎么拿 SSO cookie？

`auth.sso_cookie_file` 指向的 JSON 含 `sso_session_cookie` 字段。Linux 上可：
- 直接 `ais-switch-proxy login` 走 Compass SSO 浏览器登录（本地有浏览器即可；SSH 远程时浏览器跳不回本机，按终端提示按回车也能完成）；或
- 从 Mac 拷贝 `~/.ais-switch/google_oauth_auth.json` 过来，再 `ais-switch-proxy login --import` 导入验证；或
- 设环境变量 `AIS_SSO_COOKIE` 为整串 cookie（含 `SSO_C=` 前缀），并配 `static_key` 或让程序读 env；或
- 直接配 `auth.static_key`（一把已换好的 CQP key），跳过换取。

## 用法

```bash
# 0. 首次登录
./ais-switch-proxy login --config config.yaml            # Compass SSO 浏览器登录，写入 sso_cookie_file
./ais-switch-proxy login --import --config config.yaml   # 或从 AIS Switch 桌面端导入已有 SSO cookie

# 1. 启动代理
./ais-switch-proxy serve --config config.yaml

# 1b. 守护进程模式（父子进程，崩溃自动拉起，日志写文件）
./ais-switch-proxy serve --daemon --config config.yaml
# 停止：
./ais-switch-proxy stop --config config.yaml   # 或 kill -TERM $(cat <log_file 同目录的 .pid>)

# 2. 改写客户端配置指向代理（先自动备份）
./ais-switch-proxy takeover opencode      # 单个: claude|opencode|codex|pi
./ais-switch-proxy takeover all           # 全部

# 3. 用客户端（以 opencode 为例）
opencode run -m anthropic/claude-opus-4-7 "..."

# 4. 还原客户端配置
./ais-switch-proxy restore opencode

# 直连场景：只换 CQP key
./ais-switch-proxy mint-key

# 列出网关模型（缓存到文件，serve 时后台定时刷新）
./ais-switch-proxy models                 # 打印缓存（过期则刷新）
./ais-switch-proxy models --refresh       # 强制刷新后再打印

# 查看登录账号 / 月度用量
./ais-switch-proxy status
# 登出（清除 sso_cookie_file）
./ais-switch-proxy logout
```

## 通过代理使用（客户端配置）

代理监听 `http://127.0.0.1:15721`（`config.yaml` 的 `listen`），按 URL 路径前缀同时支持三种协议：

- **Anthropic `/v1/messages`** —— 实测可用（claude code / opencode 等走这个；下面的 Python demo 也用它）
- **OpenAI 兼容 `/v1/chat/completions`、`/v1/responses`** —— 代理转发 + CQP 注入正常，但网关侧当前对 Bearer key 返回 401（需 SSO cookie 鉴权，见 `AGENTS.md` 的 codex 路由说明）
- **Gemini `/v1beta/*`** —— 需 `GEMINI_API_KEY`

### 快速 demo（Python，零依赖）

`examples/demo.py` 直接调 `/v1/messages`，不依赖任何 SDK：

```bash
# 1. 启动代理
./ais-switch-proxy serve --config config.yaml

# 2. 跑 demo（默认 prompt "reply with exactly: pong"，model 别名 claude-haiku-4-5）
python3 examples/demo.py
python3 examples/demo.py "What is 2+2? one word" claude-haiku-4-5
```

代理把别名 `claude-haiku-4-5` 映射成网关真实模型 `deepseek-v4-flash`，注入真实 CQP key，返回结果。

### Anthropic 接口（实测可用）

客户端把 baseURL 指向 `http://127.0.0.1:15721`，apiKey 任意（代理用 CQP key 替换）：

```bash
curl http://127.0.0.1:15721/v1/messages \
  -H "content-type: application/json" \
  -H "Authorization: Bearer ANY" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-haiku-4-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}'
```

### OpenAI 兼容接口

客户端把 baseURL 指向 `http://127.0.0.1:15721/v1`，apiKey 任意：

```bash
curl http://127.0.0.1:15721/v1/chat/completions \
  -H "content-type: application/json" \
  -H "Authorization: Bearer ANY" \
  -d '{"model":"gpt-5.5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:15721/v1", api_key="ANY")
client.chat.completions.create(model="gpt-5.5", messages=[{"role":"user","content":"hi"}])
```

> ⚠️ OpenAI 路由 `/v1/chat/completions` 当前上游 401（网关需 SSO cookie 鉴权）。若你的网关 account 支持，可配 `auth.static_key` 或调整路由鉴权；否则用上面的 Anthropic `/v1/messages`。

### 三种协议对照

| 客户端协议 | 端点 | 路由 | 上游 | 鉴权 | 状态 |
|---|---|---|---|---|---|
| Anthropic | `/v1/messages` | claude | compass 网关 | CQP key (Bearer) | ✅ 实测可用 |
| OpenAI 兼容 | `/v1/chat/completions`、`/v1/responses` | codex | compass 网关 | CQP key (Bearer) | ⚠️ 上游 401 |
| Gemini | `/v1beta/*` | gemini | googleapis | x-goog-api-key | 需 GEMINI_API_KEY |

- **model 改写**：请求体 `model` 字段按 `config.yaml` 的 `model_map` 改写（如 `claude-haiku-4-5` → `deepseek-v4-flash`）。客户端发的模型名是别名，上游收到的是真实模型名。
- **apiKey 占位**：客户端填任意值（约定 `PROXY_MANAGED`），代理注入真实 CQP key，原占位 token 不会泄漏到上游。
- **可用模型**：`ais-switch-proxy models` 查看网关实际支持的模型列表。

### 一键接管客户端

`takeover` 自动改写客户端配置文件指向代理（先备份）：

```bash
./ais-switch-proxy takeover all        # claude | opencode | codex | pi | all
./ais-switch-proxy restore all         # 还原
```

- **claude**: `~/.claude/settings.json` 设 `env.ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN=PROXY_MANAGED`（走 Anthropic 协议）
- **opencode**: `~/.config/opencode/opencode.json` 的 `provider.anthropic.options.{baseURL,apiKey}`；模型用 anthropic 内置白名单别名
- **codex**: `~/.codex/config.toml` 注入 `[model_providers.ais_switch_proxy]` + 顶层 `model_provider`（走 OpenAI responses 协议）
- **pi**: `~/.pi/agent/models.json` 的 `providers.ais-switch-proxy`，`api: anthropic-messages`，`baseUrl` 指代理 `/v1`

> 自定义客户端（不在 takeover 列表里）：直接把它的 baseURL 指向 `http://127.0.0.1:15721`、apiKey 填任意值即可走 Anthropic 协议。

### `models` —— 网关模型列表（缓存 + 定时刷新）

`models` 列出 Compass 网关的真实模型（经 CQP key 调 `<upstream>/models`）。模型列表**缓存到文件**，避免每次都打网关：

- 缓存文件：`models_cache_file`（默认 `~/.ais-switch/ais-switch-proxy-models.json`，即 `sso_cookie_file` 同目录）
- 刷新间隔：`models_refresh_interval`（默认 `1h`，支持 `30m`/`2h` 等 Go duration）
- `serve` 启动时后台 goroutine **立即刷新一次**（预热缓存），之后按间隔定时刷新
- `models` 命令：缓存新鲜（< 间隔）则直接打印缓存；过期则拉取并更新；`--refresh` 强制刷新

配置（`config.yaml`，均可选）：
```yaml
# models_cache_file: ~/.ais-switch/ais-switch-proxy-models.json
# models_refresh_interval: 1h
```

`models` 输出还合并了**定价元数据**（显示名、输入/输出每百万 token 价格），来自一份内置的定价表（147 条，导出自 AIS Switch 的 `cc-switch.db` `model_pricing` 表）。运行时**不依赖** `cc-switch.db`。

### `import-pricing` —— 刷新定价表

定价数据源自 AIS Switch 桌面端（它在 schema 初始化时从二进制内嵌数据播种 147 条到 `model_pricing` 表，无远程端点）。`import-pricing` 把这张表导出成本地 JSON，运行时优先读取：

```bash
ais-switch-proxy import-pricing                  # 从 ~/.ais-switch/cc-switch.db 导出
ais-switch-proxy import-pricing --db /path/to/cc-switch.db  # 指定 DB
```

- 写入 `~/.ais-switch/models_pricing.json`（运行时自动读取，无需重建二进制）
- 无此文件时退化到二进制内嵌的默认定价表（`data/models_pricing.json`，随仓库分发）
- AIS Switch 升级带新定价后，重跑 `import-pricing` 即可刷新

> 注：定价表里没有的模型（如较新的 `glm-5.2`）在 `models` 输出中显示 `—`。

### `login` —— Compass SSO 登录

复刻 AIS Switch（ais-switch-cli）的 GoogleGateway 登录流程，写入 `sso_cookie_file`（与 AIS Switch 的 `~/.ais-switch/google_oauth_auth.json` 同格式，可互换）。认证逻辑迁移自 `ais-switch-cli/internal/gateway/`。

完整流程：

1. **Bootstrap** — `GET /compass-api/v1/auth/login` → 后端返回 401 + `result` 字段里的 `soup.shopee.io` 登录 URL，同时种下 `SSO_A` 引导 cookie（由 CLI 的 cookie jar 保留）。
2. **Loopback server** — 本地随机端口监听 `/company-gateway/login-complete`，把它的 URL 作为登录 URL 的 `next` 参数；打开浏览器。
3. **等登录完成信号** — 用户在 `soup.shopee.io` 完成 Google 登录。两条路径任一即完成：浏览器跳回本地 loopback（**本地场景**），或用户在终端按回车（**远程/SSH 场景**，浏览器到不了本机端口）。信号本身不携带 cookie —— `SSO_A` 全程留在 CLI 的 jar 里。
4. **轮询会话** — 用 jar 里的 `SSO_A` 每 2s 轮询 `/auth/info`（最长 3 分钟），认证成功后 200 响应种下 `SSO_C` 会话 cookie，由 jar 捕获。
5. **签发托管 key + 补全身份** — `POST /api/v1/cqp/ccswitch/api_key/get_or_generate`（cookie 鉴权）返回完整身份（`api_key`+`project_id`+`employee_*`），用其 `project_id`/`email` 填充账号。
6. **持久化** — 写入 `sso_cookie_file`（`account_id`/`email`/`project_id`/`sso_session_cookie`/时间戳，明文 JSON `0600`）。**托管 CQP key 仅内存缓存，不落盘**（与 AIS Switch 一致）。

> 与旧版的区别：旧版试图直接拼 `accounts.google.com` OAuth URL 并从浏览器跨进程捕获 `SSO_C`，受 soup 的 nonce/session 校验所阻（不可用）。新版改走 Compass 的 bootstrap 端点，靠 cookie jar 内部的 `SSO_A → SSO_C` 升级解决，无需 cookie 跨进程回传。

### `login --import` —— 从 AIS Switch 桌面端导入

读取桌面端 `~/.ais-switch/google_oauth_auth.json`（已完成浏览器 SSO），拷贝其 `sso_session_cookie` 到本工具的 `sso_cookie_file`，并立即调 `get_or_generate` 验证可换取托管 CQP key。适合：有 AIS Switch 的机器上登录后、或把 cookie 拷到 Linux 机器后快速接入。

### `status` / `logout`

- `status` — 读 `sso_cookie_file` 打印账号 / `project_id` / 月度用量（`/monthly_usage`，cookie 鉴权）/ 脱敏 SSO cookie。未登录则提示 `login`。
- `logout` — 删除 `sso_cookie_file`。

环境变量：
- `AIS_SWITCH_PROXY_CONFIG` — 配置文件路径（覆盖 `--config`）
- `AIS_SSO_COOKIE` — SSO cookie 整串（覆盖 `sso_cookie_file`，便于 Linux）

### `serve --daemon` —— 守护进程模式

`--daemon` 让代理以**父子进程**模式后台运行：

- 前台命令启动一个**脱离终端**（setsid）的 **supervisor**（父进程）后立即返回，打印 `supervisor pid` / `log` / `pidfile`。
- supervisor 写 pid 文件，spawn 一个 **worker**（子进程）真正跑 `http.ListenAndServe`。
- supervisor 监控 worker 存活状态：worker 退出（崩溃）即**自动拉起**，指数退避（1s→2s→…→30s 封顶，存活超 30s 重置退避），避免快速死循环打满 CPU。
- 收到 `SIGTERM`/`SIGINT`：supervisor 转发给 worker 优雅停止（10s 超时后 `SIGKILL`），清理 pid 文件后退出。
- **日志写文件**：`log_file`（配置）或 `--log-file`（flag 覆盖）；都不配则默认 `$TMPDIR/ais-switch-proxy.log`（Linux `/tmp/ais-switch-proxy.log`，pid 文件 `/tmp/ais-switch-proxy.pid`）。运行时产物按惯例进 `/tmp`，不污染配置目录。daemon 模式必用文件；前台模式若配了 `log_file` 也会同时镜像到文件。因 stderr 是普通文件，色彩自动关闭 → 文件日志无 ANSI 转义码。

```bash
./ais-switch-proxy serve --daemon --config config.yaml
# ais-switch-proxy daemonized: supervisor pid=12345 log=/tmp/ais-switch-proxy.log pidfile=/tmp/ais-switch-proxy.pid
#   stop with: kill -TERM 12345  (or kill -TERM $(cat /tmp/ais-switch-proxy.pid))
```

配置（`config.yaml`）：默认无需设置（运行时产物进 `/tmp`）。如需自定义：
```yaml
# 默认（不配）：$TMPDIR/ais-switch-proxy.log + $TMPDIR/ais-switch-proxy.pid
log_file: /var/log/ais-switch-proxy/ais-switch-proxy.log   # 自定义日志 + pid 目录
```

> 限制：`setsid` 仅 Unix；Windows 上 supervisor 不脱离控制台（仍可监控/拉起）。`go build ./...` 会写出主二进制，交叉编译后记得用对应平台二进制运行。

### `stop` —— 停止 daemon

```bash
./ais-switch-proxy stop --config config.yaml
```

读 pid 文件（与 `serve --daemon` 同路径），向 supervisor 发 `SIGTERM`：supervisor 转发给 worker 优雅退出、清理 pid 文件。等最多 15s，超时 `SIGKILL` 兜底。无 daemon 运行时友好提示，并清理 stale pid 文件。`--config`/`--log-file` 须与启动时一致（用来定位 pid 文件）。

## 路由与模型映射

代理按 URL 路径前缀（最长匹配）路由：

| 路径前缀 | 路由 | 上游 | 鉴权 |
|---|---|---|---|
| `/v1/messages` | claude | compass 网关 | CQP key (Bearer) |
| `/v1/chat/completions`、`/v1/responses` | codex | compass 网关 | CQP key (Bearer) |
| `/v1beta/*` | gemini | googleapis | x-goog-api-key |

请求体的 `model` 字段按 `model_map` 改写（如 `claude-opus-4-7` → `glm-5.2`），客户端原占位 token 被替换为真实凭据。

## 客户端接管说明

`takeover` 改写的各客户端配置文件细节见上文[通过代理使用 → 一键接管客户端](#一键接管客户端)。备份文件：`<原文件>.ais-switch-proxy.bak`（纯净副本）+ `.ais-switch-proxy.bak.meta`。

## 验证状态

- ✅ claude 路由 + CQP 认证 + 模型映射 + 流式转发：端到端实测（`/v1/messages` + FAKE key → 200 + `model=glm-5.2`）
- ✅ OpenAI 兼容路由 `/v1/chat/completions`：代理转发 + CQP 鉴权注入工作正常（上游对 `gpt-5.5` 返回 401 是网关侧 project scope/鉴权问题，非代理故障）
- ✅ `mint-key`：输出 64 字符 CQP key
- ✅ `login`：完整 SSO 流程（bootstrap 拿登录 URL + SSO_A → loopback/回车信号 → 轮询 auth/info 升级 SSO_C → fetchAPIKey 补全身份）由 mock 后端全流程测试覆盖；`login --import` + `status` + `logout` 用真实后端端到端实测（导入真实 SSO cookie → `get_or_generate` 真实换取 64 字符托管 CQP key）✓。浏览器交互需本地实测。
- ✅ takeover/restore：claude/codex/opencode/pi 配置改写与还原往返正确
- ⚠️ codex/gemini 路由的**上游鉴权**未在本机完全实测（codex 可能需 OAuth/project scope 而非 CQP，gemini 需 `GEMINI_API_KEY`）。鉴权策略可配置，不通时按 config 调 `auth` 字段。

## 与 AIS Switch 代理的关系

两者协议一致（同 mint 端点、同 cookie 字段、同路由前缀、同模型映射）。本程序可在 Linux 上替代 AIS Switch 的代理；在 Mac 上若与 AIS Switch 同时运行，注意 `listen` 端口不要冲突（AIS Switch 默认占 15721）。
