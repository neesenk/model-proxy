# CLI 契约（命令行逻辑 + 信息展示契约）

本文档是 model-proxy CLI 的**唯一对外行为契约**：每个子命令做什么、stdout/stderr 的精确格式与文案、退出码。根 `AGENTS.md` 只负责把 CLI 修改路由到本文档，不再维护重复命令表。

> **变更控制**：本文档记录的 stdout/stderr 格式、文案、退出码是**稳定契约**，脚本和用户依赖它们。**修改这些契约前必须先与用户确认**（见 `CLAUDE.md` 的对应指令）。新增字段/列允许追加（向后兼容），但不得改动既有行的格式或删改既有文案。契约改动需同步更新本文档 + 相关测试（`*_test.go` 里的 `strings.Contains` 断言）。

---

## 全局约定

### 实现边界

`main` 函数只把 `os.Args`、`os.Stdin`、`os.Stdout`、`os.Stderr` 绑定到根
`package main` 的 `application.Run`，并把其返回值交给 OS 进程退出。`application`
是实际进程 composition owner，拥有命令表和 `serveAssembly`；其 `Run` 是可测试的
顶层入口：它直接处理无参数、顶层/子命令 help、未知命令及其 exit code，并通过统一
表分发已知命令（唯一 CLI 分发入口）。现阶段已知命令
由兼容 adapter 调用既有 handler；这些 handler 的进程 I/O、`log.Fatal` / `os.Exit`
语义仍保持不变，不代表所有命令都已经改成注入流或返回 exit code。

`serveAssembly` 拥有 `serve` 命令及前台/worker signal、HTTP server 生命周期；
其 `applicationRuntime` 在配置和进程日志准备后构造 `Proxy`、调用
`startRuntimeServices`、装配 mux/Web、投影 reload、交出 transport task，并以
`Close` 结束 Proxy 生命周期。可复用 HTTP drain primitive 留在
`internal/cli/serve/shutdown.go`；daemon/supervisor 的 signal 与 pid/probe 编排位于
`internal/cli/serve/supervisor.go`。child process
detach 属性的平台差异位于 `internal/cli/serve/detach_unix.go` / `internal/cli/serve/detach_windows.go`。

这是内部装配边界的收敛，不改变命令、输出或退出码。

### 退出码

| 码 | 含义 | 触发点 |
|---|---|---|
| `0` | 成功 | 命令正常返回；`serve` worker 收到 SIGINT/SIGTERM 后完成 transport drain 和 final flush（`runProxyProcess`） |
| `1` | 运行时错误 | 顶层无参数/未知命令由 `Application.Run` 返回 1；既有 handler 继续通过 `log.Fatal(...)` 或显式 `os.Exit(1)` 处理 config 无效、daemon 不可达、响应解析失败、未知子命令/参数 |

真实 CLI 路径**不使用** exit 2。

`Application.Run` 自己处理的顶层分支只返回 exit code、不终止进程；已知命令 handler
仍可能按既有契约终止进程。`log.Printf` 写入 stderr；故 stderr 是
诊断/进度/告警/错误的统一流。

### 流约定

- **stdout** = 命令的结果数据（表格、列表、JSON、状态行）。脚本应只读 stdout。
- **stderr** = 进度提示、告警、错误、`log.*` 输出。面向人，不面向脚本。
- 例外：`takeover`/`restore` 的逐客户端进度走 `log.Printf`（stderr）；`models refresh` 的 diff 行（`config: added/removed ...`）和 fallback 通知走 stderr；`config check` 的 `✗ config invalid` 走 **stdout**（`fmt.Println`，见下）。

### 着色

两套独立开关：

- `provider.ColorEnabled`（`internal/provider/display.go`）— stdout 着色；根 CLI 仅保留
  实际使用的 `cGreen`/`cRed`/`cYellow`/`cDim`/`cBold`/`cBlue`/`cCyan`/`cGray`
  等薄包装。
- `logColorEnabled` — stderr/log 着色（`cl(...)`，运行时日志用）。

两者都遵循：`NO_COLOR` 非空 -> 关；`CLICOLOR_FORCE` 非 0/空 -> 开；否则按 fd 是否 tty 自动。**管道/重定向到文件时着色自动关闭**，故脚本看到的 stdout 是纯文本。契约文案里的 `cGreen("✓")` 等表示「着色开启时包 ANSI，关闭时原样」。

### 通用辅助函数

| 函数 | 行为 |
|---|---|
| `mask(id)` | 账号 id 掩码：首 2 + `…` + 末 2（如 `ab…ef`）。账号 id 在 `logout`/`login` 确认行、`usage` 池化块头用。`/api/accounts` 返回**不掩码**的 id（UI 要用它删除）。 |
| `truncate(s, n)` | `s[:n]+"..."`（n 之外截断），用于上游错误体摘录。 |
| `compactNum(n)` | 紧凑数字（千位 K / 百万 M），用于计数器列。 |
| `pad(s, n)` | 左对齐补空格到 n 列；`len(s)>=n` 时原样（不截断）。 |
| `providerNames(cfg)` | 配置里的 provider 名，逗号分隔，**未排序**（map 迭代序）。用于「unknown provider」错误。 |
| `routeNames(cfg)` | 路由名，逗号分隔，**已排序**。用于 `serve` 启动日志。 |

### 全局选项

- `--config <PATH>` — 配置文件路径。查找顺序：显式 > `~/.model-proxy/config.yaml` > `./config.yaml`。所有命令通用。
- `--log-file <PATH>` — 仅 `serve`：覆盖 config 的 `log_file`。
- `-h, --help` — 打印帮助（顶层或 `model-proxy <cmd> -h` 打印 `cmdHelp[cmd]`）。

---

## 1. `serve` — 启动代理

```
serve [daemon|stop|reload|status] [--config PATH] [--log-file PATH]
```

### 子命令分发（`cli_serve.go` 的 `cmdServe`）

| 调用 | 行为 |
|---|---|
| `serve`（无子命令） | 前台运行（`runProxy`）：加载 config、起 runtime services、注册 mux，以显式 `http.Server` 提供服务。 |
| `serve daemon` | `daemonize`：分离一个 supervisor 进程（setsid，stdio -> log 文件），父进程立即返回。 |
| `serve stop` | `cmdStop`：读 pid 文件，SIGTERM 等待 ≤15s，超时 SIGKILL。 |
| `serve reload` | `cmdReload`：读 pid 文件，SIGHUP supervisor（转发给 worker 热重载）。 |
| `serve status` | `cmdServeStatus`：见 §11。 |

### `serve daemon` stdout（成功）

```
model-proxy daemonized: supervisor pid=<PID> log=<LOGFILE> pidfile=<PIDFILE>
  stop with: kill -TERM <PID>  (or kill -TERM $(cat <PIDFILE>))
```

失败：`log.Fatal(err)` -> stderr + exit 1（config 加载失败 / 无 log_file / 启动 supervisor 失败 / **已有 daemon 在运行**）。

已有 daemon 在运行（pid 文件指向活进程）时拒绝启动，避免第二个 supervisor 覆盖 pid 文件后 `serve stop` 停错进程、原 daemon 变孤儿：
```
model-proxy is already running (supervisor pid=<PID>); use `serve stop` first, or `serve status` to inspect
```

前台 `serve`（无子命令）同样不接管已被活进程持有的 pid 文件
（`ClaimForegroundPidFile`）：daemon 在跑时前台进程不覆盖该文件、退出时也不
删除它，避免夺走 daemon 的 `serve stop`/`reload` 入口；退出清理只删除仍指向
本进程的 pid 文件（`ReleasePidFileIfOwned`）。

### `serve stop` 输出

| 场景 | 流 | 文案 |
|---|---|---|
| pid 文件不存在 | stdout | `No daemon running. (pid file not found: <PIDFILE>)`（黄色） |
| 进程已不在（stale pid） | stdout | `Daemon not running. (removed stale pid file <PIDFILE>)`（黄色）；删 stale pid 文件 |
| 正常 | stdout | `Stopping model-proxy daemon (pid=<PID>)...` -> `✓ Stopped.`（绿） |
| 15s 未退出 | stderr | `graceful stop timed out, sending SIGKILL to <PID>` -> stdout `✓ Killed.`（绿） |

失败：`log.Fatal`（config 加载失败 / pid 非法 / 信号发送失败）-> stderr + exit 1。

### `serve reload` 输出

| 场景 | 流 | 文案 |
|---|---|---|
| pid 文件不存在 | stdout | `No daemon running. (pid file not found: <PIDFILE>)`（黄） |
| 进程已不在 | stdout | `Daemon not running. (removed stale pid file)`（黄） |
| 正常 | stdout | `Reloading model-proxy daemon (pid=<PID>)...` -> `✓ Reload signal sent. Check logs for [reload] lines.`（绿） |

reload 结果在 daemon 的 **log 文件**里（`[reload] config reloaded successfully (providers: ..., routes: ...)` 或 `[reload] FAILED: ... (keeping old config)`），不在 CLI stdout。

### 前台 `serve` 启动日志（stderr，`log.Printf`）

- `model-proxy listening on <LISTEN> (routes: <routeNames>)`
- 收到 SIGHUP：`[reload] SIGHUP received, reloading config from <PATH>` + 上述结果行。
- 收到 SIGINT/SIGTERM：停止 SIGHUP reload，取消并等待 Web session GC/异步登录
  轮询，再停止接入并最多等待 8 秒让在途请求完成；已进入凭据落盘 commit 的登录
  会完成 save+reload。随后 final flush request log、Responses state、stats 和
  quota，正常退出。8 秒内未 drain 时强制取消活动连接，并记录
  `[shutdown] HTTP drain exceeded 8s (...)`; handler 完成退栈后继续 final flush。

### 性能剖析（可选）

- 环境变量 `MP_PPROF=1` 启动(前台或 daemon 均继承)后,proxy handler 暴露
  `http://127.0.0.1:<LISTEN>/debug/pprof/`(与 `/debug/schedule` 同一浏览器来源
  防护)。默认关闭:heap profile 可能携带请求数据采样,仅调试时开启,配合
  `go tool pprof` 使用。

---

## 2. `takeover <client>` — 接管客户端配置

```
takeover <client>   # client ∈ {claude, opencode, codex, pi, kimi, all}
```

逻辑（`internal/takeover/takeover.go` 的 `RunTakeover`）：备份每个客户端配置（verbatim + sha256 meta，幂等）到 `<configDir>/.model-proxy/`，再改写指向代理。含隐式路由模型；opencode/pi 额外 hydrate models.dev 元数据。

### 输出

- **stderr**（`log.Printf`，每客户端两行）：
  ```
  takeover <client>: <FILE> (backup -> <BAKDIR>/)
    ✓ <client> done
  ```
  `restore` 对应：`restore <client>: <FILE> (from <BAKDIR>/)` -> `  ✓ <client> restored`。
- **stderr 跳过**（仅 `all`：某客户端配置文件不存在 -> agent 未安装时，跳过该客户端继续其余）：
  ```
    ~ <client> skipped (config not present: <FILE>)
  ```
  `restore all` 对应：`  ~ <client> skipped (no backup in <BAKDIR>/)`。单客户端（`takeover <client>`）缺文件仍是硬错误。
- **stderr 告警**（仅 opencode/pi，某个模型无 models.dev 元数据时，每个模型一行）：
  ```
  warning: model <MODEL> at <PROVIDER>: no models.dev metadata - wrote defaults (ctx=200000 out=16384 text-only)
  ```
- **stderr 漂移警示**（改写成功后立即对本次接管的 client 复检 proxy 指针，复用 `doctor --live` 的漂移检测；仅仍有漂移时每个 client 一行）：
  ```
    ⚠ <client> drift detected right after takeover: <FILE> points to <CURRENT>, want <EXPECTED>
  ```
  `guard.audit` 开启时按 doctor 同款语义追加一条 kind=drift 安全审计记录（agent=takeover，同日同 client 去重，见 §18）；审计追加失败只 stderr 提示。漂移不影响 exit code。`restore` 不做该校验（恢复原状是预期）。

失败：`log.Fatal(err)` -> stderr + exit 1（config 加载失败 / 备份失败 / 改写失败）。`client` 不在集合内由 `listClients` 决定（`all` 展开全部；未知名通常导致空集，静默返回 0）。`takeover:` 块整个可省略--五个 client 路径 + provider_id 有代码默认值，只有覆盖某项才需写。kimi 写 `~/.kimi/config.toml`：注入 `[providers."model-proxy"]`（`type = "openai_legacy"`，base_url 带 `/v1`）+ 每个暴露模型一个 `[models.<name>]` 块；开启 `web.auth.api_keys_file` 后需把 `PROXY_MANAGED` 占位 key 换成文件里的真实 key。

---

## 3. `restore <client>` — 还原客户端配置

```
restore <client>   # client ∈ {claude, opencode, codex, pi, all}
```

逻辑（`internal/takeover/takeover.go` 的 `RunRestore`）：从 `<BAKDIR>/<client>.bak` verbatim 复制回原路径。输出同 §2 的 restore 行。失败：`log.Fatal` -> stderr + exit 1（无备份 -> `no backup for <client> in <BAKDIR>: ...`）。

---

## 4. `login <provider>` — 登录

```
login <provider> [--label <name>] [--replace]
                 [--from-env VAR [--from-env-ak VAR --from-env-sk VAR] | --from-codex]
```

逻辑（`internal/cli/login/login.go` 的 `CmdLogin`）：经 `RunProviderLogin` 按 `provider_id` 分派。aqp=SSO、codex=OAuth device flow、static/zhipu/deepseek/kimi-code/qwen-plan=apikey 池、volcengine=apikey+AK/SK 三元组池、zcode=BigModel Coding Plan（开 bigmodel.cn/login + apikey 池）。成功后 `MaybeReloadDaemon`（热重载运行中的 serve，无 daemon 时静默 no-op）。`add` 命令复用同一分派。

### 凭据存储后端（config `credentials:` 统一开关 + env override）

两类凭据共用一个开关：config 顶层 `credentials: file|keychain`（默认 file），同时驱动 apikey 池与 codex/aqp OAuth store。env `MP_CRED_STORE`（`file|keychain|auto`）保留为**仅 OAuth 侧的显式 override**：优先级 env 非空 > config > 默认 file；`auto` = 按 keychain 可达性探测（收敛前的旧默认，现为 opt-in）。env 与 config 不一致时 `config check` 和启动/reload 日志各打一行，列出两侧生效值与来源（env/config/default）。

codex/aqp OAuth store 经 `credstore.Ref` 读写：`file` = 历史行为（0600 明文文件 + temp+fsync+rename 原子写）；`keychain` = 整 blob 一条 keychain 记录，后端不可达时 fail-closed 报错。keychain 模式下首次读到遗留明文文件会懒迁移进 keychain 并把原文件改名为 `<path>.migrated.bak`（保留一代回滚）。OAuth blob 不做 keychain→file 自动回迁（与历史 env 切换一致）——切回 file 需重新 login。

apikey 池（`<name>_apikeys.json` 与遗留单账号文件）：`keychain` 模式下 `accounts.Store` 把 api_key/access_key/secret_key 逐条写入 OS keychain（经 credstore 的 entry API，service `model-proxy`，键 `<providerName>/<accountId>/<field>`），池文件只留 `{id, label, added_at}` 元数据；明文池与遗留文件在首次读取时懒迁移（池文件原地重写为纯元数据，遗留文件改名 `.migrated.bak`）。保存时先写 keychain 再写元数据；删除账号时先删 keychain 条目成功才落元数据，避免"元数据没了秘密还留钥匙串"的孤儿。keychain 不可达一律 fail-closed 报错，不静默回落明文。**反向回迁**：file 模式读到纯元数据池时按条目从 keychain 读回秘密并原子重写明文池（0600）；条目缺失的账号保留元数据并经 `Snapshot.ReloginNeeded`/启动日志报出需重新 login（部分回迁不整体失败）；回迁成功后 keychain 条目默认保留（防误删，`logout` 是正常删除路径）。模式经 `accounts.SetProcessCredentialsMode` 在 config 加载点（`LoadCmdConfig`/`CmdLogin`/`config check`/serve 构造与 reload）同时应用到池后端与 credstore OAuth 模式，未加载 config 的调用点保持 file 默认。

测试二进制默认解析为 file 模式，绝不触碰真实 keychain；keychain 语义测试用 `keyring.MockInit()` 内存 mock。

### 通用

- 无 provider 参数 -> stdout 打印 usage + `available providers:` 列表（每行 `  <name> (provider=<provider_id>)`），exit 0。
- 未知 provider -> stderr `unknown provider "<NAME>"; available: <providerNames>` + exit 1。
- 任意登录失败 -> stderr `login failed: <err>` + exit 1。

### aqp（SSO，`runLogin`）

stdout（引导浏览器）：
```
Open a browser (or copy the link below into one) to complete your company Google login:
  <LOGIN_URL>

After logging in, the browser will try to redirect back to this machine...
Once you've finished logging in (or the redirect above succeeded), come back here and press ENTER...
```
回调收到后：`[GoogleGateway] Received login-complete signal, checking AQP session` / `[GoogleGateway] Ignoring OAuth callback code/state (AQP Soup SSO flow)`。成功：
```
[GoogleGateway] login complete: <EMAIL> (project=<PROJECT_ID>)
  store: <STORE_PATH>

Login complete. You can now run `model-proxy serve`.
```

### codex（OAuth device flow，`internal/cli/login/codex_login.go` 的 `CmdCodexLogin`）

stdout：
```
Requesting device code from OpenAI...

  Open this URL: <VERIFY_URL>
  Enter code:   <USER_CODE>

Waiting for authorization (15 min timeout)...
Authorized. Exchanging code for tokens...
✓ codex OAuth tokens saved to <AUTHFILE>
  account_id: <ACCOUNT_ID>

You can now use codex-native models (gpt-5.5) through the proxy.
```

### apikey 类（zhipu/deepseek/kimi-code/qwen-plan，`runApiKeyLoginWithInput`）

- stdout 提示：`Enter API key for <PROVNAME>: `（stdin 读 key）。
- stderr（当存在可校验端点时）：`Validating API key...`。校验端点由 `apiKeyValidationURL` 解析：配了 `usage_url` 的用它（zhipu/deepseek/**kimi-code** 均配）；**未配 `usage_url` 的回退 `openai_base_url/models`**（qwen-plan：个人版无公开用量接口，不配 `usage_url`）。校验 = GET 该端点 with `Authorization: Bearer <key>`；**401/403 或网络错误** → `login failed: validation failed: HTTP <N>: <BODY>`（exit 1，**不写池**）；其余状态码（200/404 等）= key 通过（写池）。
- 重复 id 且非 `--replace` -> stdout 提示 `Account "<LABEL>" is already logged in. Replace its key? [y/N] `；答非 y -> `login cancelled`（exit 1）。
- 成功 stdout：`✓ Saved account <MASKED_ID> (<LABEL>)`（绿）。

### zcode（BigModel Coding Plan，`case "zcode"` → `runApiKeyLoginWithInput`）

stdout：
```
Opening BigModel login to fetch a Coding Plan API key…
Enter API key for <PROVNAME>: <stdin>
```
- 开浏览器到 `https://bigmodel.cn/login`（`openBrowser`）；远程/SSH 打不开时 stderr `(could not open browser: <err> — open https://bigmodel.cn/login manually)`。用户登录后到 API Keys 拿 **Coding Plan API key** 粘贴。
- 之后同 apikey 池流程：`Validating API key...`（stderr，配了 `usage_url`）→ `✓ Saved account <MASKED_ID> (<LABEL>)`。
- 失败同 apikey：`login failed: <err>`（exit 1，不写池）。

### volcengine（`runVolcengineLoginWithInput`）

stdout 依次提示（stdin 读三元组）：
```
Ark API Key (对话用，控制台创建): 
Volcengine Access Key ID (GetAFPUsage 用，IAM 密钥): 
Volcengine Secret Access Key: 
```
- stderr（配了 `usage_url` 或填了 AK/SK）：`Validating credentials...`。校验在 `addVolcengineAccount` 内顺序执行：先 GET `/api/plan/v3/models` with `Authorization: Bearer <Ark key>`（**401/403 或网络错误** → `login failed: validation failed: ...`，exit 1，**不落盘**）；通过后，若 AK/SK 都非空，再签名 GetAFPUsage（失败 → 同上 exit 1，不落盘）。
- AK/SK 可缺省（仅 chat 账号），但**必须成对**：只填 AK 不填 SK（或反之）→ `login failed: AccessKey and SecretKey must both be set, or both be empty for a chat-only account`（exit 1，不落盘）。
- 成功同 apikey：`✓ Saved account <MASKED_ID> (<LABEL>)`（绿）。

### 凭据导入（`internal/cli/login/import.go`，免粘贴）

两条非交互导入路径，成功后同样走 `MaybeReloadDaemon` 热重载。**安全约定：导入的 secret 值从不回显、不进日志/错误信息；错误只点名文件/字段/变量名；成功输出只有掩码账号 id。**

- `login codex --from-codex`（`RunCodexImport`）：读取官方 codex CLI 登录态 `~/.codex/auth.json`（格式 `{"OPENAI_API_KEY", "tokens": {id_token, access_token, refresh_token, account_id}, "last_refresh"}`），三个 token 必须非空，经 `provider.WriteCodexAuthFile` 写入本代理的 `<provName>_oauth_auth.json`（`auth_mode=chatgpt`，`account_id` 缺省时从 id_token JWT 解析，`last_refresh` 沿用源文件）。仅对 `provider_id: codex` 的条目有效（其他 provider → `--from-codex is only valid for codex providers ...`，exit 1）；与 `--from-env*` 互斥。
  - stdout：`✓ Imported codex CLI credentials → <AUTHFILE>` + `  account_id: <MASKED>` + 一行提示（导入的 access_token 可能已过期，代理在 refresh_token 有效时按需 refresh）。
  - 文件缺失 → `codex CLI credentials not found at <PATH> (run \`codex login\` first, ...)`；非法 JSON → `<PATH> is not valid JSON (...)`（不引用文件内容）；缺 token → `<PATH> is missing tokens.<FIELD> (...)`；apikey 模式（有 `OPENAI_API_KEY` 无 tokens）→ `<PATH> is an apikey-mode codex CLI login ...`（改用交互 device flow）。均 exit 1、不落盘。
- `login <provider> --from-env VAR`（`runFromEnvLogin`）：apikey 类 provider 从环境变量读 key，之后与交互登录完全同路（校验、去重、`--label`/`--replace`、重复 id 的 stdin 确认）。变量不存在或为空 → `environment variable <VAR> is not set or empty`（exit 1）。codex/aqp 不支持（报错并提示 `--from-codex`/SSO）。
- volcengine 追加 `--from-env-ak VAR` / `--from-env-sk VAR`（可选、成对，语义同交互）：三值全从环境读，**不触发 AK/SK 的 stdin 提示**（`runVolcengineLoginFromEnv`）；单独用 ak/sk flag 而无 `--from-env`、或对非 volcengine 用 ak/sk flag 均报错 exit 1。

---

## 4b. `presets list` / `add <preset>` — 预设目录与一键接入

```
presets list [--config PATH]
add <preset> [--config PATH] [--label NAME] [--replace]
             [--api-key-env ENV] [--yes]
```

逻辑（`internal/cli/presets/presets.go`）：

- 目录**派生自内置注释模板**（`configdomain.DefaultConfigYAML`），过滤到已注册实现的 provider_id；`aqp` 因内网 SSO 端点不进公共目录。模板即权威，无第二份 endpoint/模型知识。
- `add`：① 把模板的 `providers.<preset>` 块经 `configedit` 合并进用户 config.yaml（保结构/注释；块已存在则跳过合并，幂等）；② 合并后先 `LoadConfigFromBytes` 校验再原子写回（fail-closed）；③ 歧义门——preset 模型同时出现在其他已配置 provider 且无显式 `routes:` 条目时，非 TTY 拒绝 exit 1（提示 `--yes`），TTY 询问 y/N；④ 复用 `login.RunProviderLogin` 登录；⑤ `MaybeReloadDaemon` 热重载；⑥ 打印 `model-proxy test <首个模型>` 下一步。
- `--api-key-env ENV`：从环境变量读 API key（脚本化，不落 stdin 历史）。变量为空 -> stderr 报错 exit 1。
- 未知 preset -> stderr 列出全部可用 preset + exit 1。config.yaml 不存在 -> 提示 `config init` + exit 1。

---

## 5. `logout <provider>` — 登出

```
logout <provider> [--label <name>] [--all]
```

逻辑（`cmdLogout`）：aqp/codex/无池文件 -> 单文件 `Logout()`；有池文件 -> 池路径（`--all` 清空 / `--label` 删指定 / 否则交互式列号选择）。

### 输出

| 场景 | 流 | 文案 |
|---|---|---|
| 无 provider 参数 | stdout | `usage: model-proxy logout <provider> [--label <name>] [--all]` + `available providers:` 列表，exit 0 |
| 未知 provider | stderr | `unknown provider "<NAME>"; available: <providerNames>` + exit 1 |
| aqp/codex/单文件成功 | stdout | `✓ Logged out`（绿） |
| 池为空 | stdout | `Not logged in.`（黄） |
| 交互式选择（池） | stdout | `Accounts for <PROVNAME>:` + 每行 `  <N>) <LABEL>  (#<MASKED_ID>  added <ADDED_AT>)` + `Remove which (number)? `（stdin） |
| `--all` 成功 | stdout | `✓ Removed all accounts from <PROVNAME>`（绿） |
| `--label`/选中单号成功 | stdout | `✓ Removed account <MASKED_ID>`（绿） |
| `--label` 未命中 | stderr | `no account labeled "<LABEL>" in <PROVNAME>` + exit 1 |
| 选号非法 | stderr | `invalid selection` + exit 1 |

成功后 `maybeReloadDaemon`。

---

## 6. `usage [provider]` — 用量

```
usage [provider]   # 无参数 = 所有已配置 provider
```

逻辑（`internal/cli/usage.go` 的 `CmdUsage` -> `PrintProviderUsage` -> `provider.Provider.Usage()`）：无参数时
按 provider 名排序逐个打印，块间用 `usageDivider` 分隔。未知 provider -> stderr
`unknown provider ...` + exit 1。

### 分隔符

```
const usageDivider = "────────────────────────────────────────"
```
仅块**之间**打印，不在首块前/末块后。

### 池化 provider（≥2 账号，`printProviderUsage`）

每账号一块，块间 `usageDivider`，每块首行：
```
<LABEL> (<MASKED_ID>)
```
然后为该账号构建绑定凭据的虚拟 provider，并调用它的 `Usage()`；账号间不得共享
凭据或绕过统一 Provider 接口。

### 单账号/非池化（aqp/codex）

`p.Usage()` 失败 -> stdout `  (usage unavailable: <ERR>)`（黄）。

### 每个 provider 展示契约（首行固定）

**所有 provider 的展示函数首行必须是**：
```
Provider:   <PROVNAME>
```
（`cDim("Provider:  ")` + `cBold(cBlue(provName))`，两空格对齐）。这是 `usage`（无参数）多块输出一致性的硬约束（Provider 接口注释规定）。

| provider | 关键行（首行之后） |
|---|---|
| aqp (`AqpProvider.Usage`) | `Account:    <EMAIL>`；`Project ID: <ID>`；`Usage:      [<BAR>] <PCT>% used · $<USAGE> / $<TOTAL>  (balance $<BAL>, <PLAN>, <YEAR>-<MONTH>)`；`Store:      <PATH>` |
| codex (`CodexProvider.Usage`) | `Account:   <EMAIL|"(unknown)">`；`Plan:      <PLAN_TYPE>`；`Credits:   unlimited`/`has credits (<BAL>)`/`none`；`Rate Limit:` 状态；`Usage:` `limit reached` 或 `[<BAR>] <PCT>% used · <USED> / <TOTAL> credits, resets <DUR>` |

> `<PCT>` 为已用百分比，统一保留小数点后一位（如 `56.8%`、`25.0%`）。
| zhipu (`ZhipuProvider.Usage`) | 5h/weekly token 限额 + 月度时间限额（带进度条）；`TIME_LIMIT` 按 MCP 工具（search-prime/web-reader/zread）分解；回退：OpenAI 风格模型列表 |
| deepseek (`DeepSeekProvider.Usage`) | `Available:  no (insufficient balance)`（余额不足时）；各币种 `total/granted/topped-up` 余额 |
| volcengine (`VolcengineProvider.Usage`) | 无 AK/SK：`Note:` 说明 + 列 config 模型；有 AK/SK：`Plan: <PLAN_TYPE>` + `AFPFiveHour/Daily/Weekly/Monthly` 各窗口 Quota/Used/Remaining/ResetTime |
| kimi-code (`KimiCodeProvider.Usage`) | `Plan:       Kimi Code membership`；`Weekly limit`（Ultimate，7d）+ `5h limit`（Short，5h）+ 其他限额窗口（带进度条/重置时间）+ `Extra usage`/`Monthly cap` 钱包窗口（`n/a`）。取自 `/usages`。拉取失败：`Usage:      (unavailable: <ERR>)` + 控制台提示 + 列 config 模型 |

重置时间格式：`<duration>(at <time>)`；`formatResetAt`：今天显示 `HH:MM`，否则 `MM-DD HH:MM`。

---

## 7. `models` — 模型列表

```
models [provider]
models refresh <provider>
models pull
```

### `models [provider]`（`cmdModels`，无 `refresh`/`pull`）

stdout 表格（`printAllModels`），表头：
```
PROVIDER       MODEL ID               NAME                 CTX         OUTPUT    MODALITIES        SRC
```
列宽：PROVIDER 12 / MODEL ID 22 / NAME 20 / CTX 10 / OUTPUT 8 / MODALITIES 16 / SRC 10（`pad`，左对齐）。每 provider 一组模型（config 名 ∪ hydrate 元数据键，排序）。`SRC` = `models.dev` / `default` / 空。

- 未知 provider -> stderr `unknown provider "<NAME>"; available: <providerNames>` + exit 1。
- 末尾（有歧义隐式路由时）stderr：
  ```
  ⚠ implicit-route warnings:
    <WARNING>
  ```

### `models pull`（强制刷新 models.dev 缓存）

stdout：`models.dev catalog refreshed: <N> unique models, etag <ETAG>`。网络错误、
非 2xx、malformed 200 或无 cache 的异常 304：有旧 cache 时 stderr 告警并继续
使用旧值；无可用 cache 时 stderr + exit 1（`log.Fatal`），且不会写入坏响应。

### `models refresh <provider>`（`cmdModels` refresh 分支 + `probeAndWriteModels`）

候选集：`FetchModels()` 成功 -> `mergeModelIDs(existing, fetched)`；**`FetchModels` 失败（无 `/models` 端点 / 未登录 / 网络错）-> fallback**：stderr 通知 + `mergeStringIDs(existing, routeModelsForProvider(cfg, prov))`。然后统一走 `probeAndWriteModels`。

**stderr 通知（fallback 时）**：
```
Refreshing models from <PROVNAME>...
models endpoint unavailable for <PROVNAME> (<ERR>); probing route-configured models instead
```
fallback 且无候选（无 `/models` 且无路由指向它）时追加：
```
no models to probe for <PROVNAME> (no /models endpoint and no routes target it); add routes targeting <PROVNAME> first
```

**stdout（`printKeptModels`，最终保留列表，先于摘要）**：
```
MODEL ID                       NAME                  CTX         OUTPUT    INPUT MODALITIES    SRC
<kept rows>
provider: <PROVNAME>: <N> models
```
列宽：MODEL ID 26 / NAME 20 / CTX 10 / OUTPUT 8 / INPUT MODALITIES 18 / SRC 10。空集 -> stdout `(no models)`（黄）。

**stderr 摘要（`printFilterSummary`，列表之后）**：先空行；有 drop 时：
```
filtered out <N> model(s):
  <MODEL>                       excluded by filter rule          # 策略（regex）drop
  <MODEL>                       not callable on base_url - <REASON>   # 探测 drop
```
全失败（`allProbeFailed`）时表头改为 `filtered out <N> model(s) - probe failed for ALL (likely not logged in / network):`，每行 reason 前缀 `probe failed (login/network?) - `。探测 infra 不可用（`perr != nil`）-> `endpoint probe skipped (<ERR>); list written unvalidated`。

**stderr diff 行（`writeProviderModels` 写盘后，仅当 `kept != existing`）**：
```
config: added <N> -> [<ADDED...>]
config: removed <N> -> [<REMOVED...>]
```
`added`/`removed` 来自 `diffStringSets`，已排序。无变化 -> 不写盘、不打 diff、不 reload。写盘失败 -> stderr `writing models to config: <ERR>` + exit 1（`log.Fatal`）。写盘成功后 `maybeReloadDaemon`。

退出码：成功 0；未知 provider / 写盘失败 -> 1。

---

## 8. `config init|print|check` — 配置管理

```
config init|print|check
```

### `config init`

写 `config.yaml` 到 **CWD**。stdout：`wrote config.yaml`。失败 -> stderr + exit 1。
**CWD 已存在 `config.yaml` 时拒绝覆盖**（保留原文件，stderr + exit 1）——避免一次误操作抹掉现有配置。

**交互式向导**（仅当 stdin 是真实 TTY；管道/重定向/`/dev/null` 保持上面的静态模板行为不变，脚本无感）：依次做四件事——

1. 探测本机已安装的编程客户端（claude/opencode/codex/pi 的 config 路径存在性，复用 takeover 包的路径知识），列出探测结果；
2. 逐个询问启用哪些 provider（清单 = 内置模板 providers ∩ provider 注册表，不硬编码第二份），提示形如 `Enable zhipu (provider=zhipu, 10 models)? [y/N] `（一律默认 N，EOF = N）；
3. 写出**最小 config.yaml**：只含选中 provider 的块，routes 过滤到选中 provider（整路由无存活 target 则删），claude_mapping 只留指向存活路由的别名；落盘前重新 validate（fail-closed）。一个都没选 -> stderr `no providers selected — config.yaml not written` + exit 1；
4. 探测到客户端时询问 `Take over detected client configs now (...)? [y/N] `，确认则当场执行 takeover（幂等备份机制与 `takeover` 命令相同）；最后打印下一步命令：每个选中 provider 的 `model-proxy login <name>`、（未执行 takeover 时）每个探测到客户端的 `model-proxy takeover <client>`、`model-proxy serve`、`model-proxy test <model>`（首个存活路由名；无存活路由时为首个 provider 的首个模型，依赖隐式路由）。

模板里的 `scheduling:` 整块默认是注释掉的（每行带 `(default N)`）：所有字段都有代码默认（`internal/config` 的 accessor），不写即用默认，需覆盖时取消注释对应行。`config check` 的 `scheduling:` 摘要行始终打印**生效值**（已覆盖则显覆盖值，未配则显代码默认）。`--config` 与其他命令一致、位置无关（`config --config X check` 与 `config check --config X` 等价）。

### `config print`

stdout：
```
listen: <LISTEN>
provider <NAME>: openai_base_url=<URL> provider_id=<ID> (<N> models)
...
route <EXPOSED>: <N> targets
...
claude_mapping: <N> aliases      # 仅当 >0
```

### `config check`

- 无效 -> **stdout** `✗ config invalid: <ERR>`（红）+ exit 1。
- 有效 -> stdout `✓ config valid`（绿）+ 缩进摘要：
  ```
    listen:    <LISTEN>
    log_file:  <LOGFILE>
    providers: <N>
      <NAME>: <URL> (<ID>, <N> models)
      ...
    routes:    <N>
      <EXPOSED>: <N> targets
      ...
    claude_mapping: <N>
    scheduling: threshold=<T> cooldown=<D> rate_backoff=<D> timeout=<D> dwell=<D>
    guard: secrets=<ACTION> known_secrets=<BOOL> decode=<BOOL> paths=<ACTION> audit=<BOOL>
      audit_path: <PATH>
      patterns: built-in tables (embedded) + <N> custom (<NAME>, ...)
      extra_paths: <N>
  ```
  guard 段为生效值（load 默认值已应用；`RenderGuardSummary`）：内置规则表嵌入在二进制里，只报「embedded」不报条数——这样 CLI 不需要依赖 internal/guard；自定义扩展（`guard.extra_patterns` 计数 + name 列表、`guard.extra_paths` 计数）来自 config 结构。

### 通用

- 无子命令 -> stdout `usage: model-proxy config [init|print|check]` + exit 1。
- 未知子命令 -> stderr `unknown config subcommand: <SUB>` + exit 1。
- config 加载失败（`print`/`check`）-> `log.Fatal`（stderr）+ exit 1；但 `check` 的「无效 config」走 stdout + exit 1（见上，便于脚本区分）。

### Route target 字段（`routes:` 下每个 target）

```yaml
routes:
  <exposed>:
    - {provider: <name>, model: <upstream-model>, priority: <int>, protocol: <anthropic|openai>}
```

- `provider` / `model`：必填。`model` 是该 provider 的真实上游模型名（请求转发时 rewrite 进 body）。
- `priority`：可选，整数，低 = 优先（同 tier/quota band 内先试）；空 = 0。
- `protocol`：可选，**协议转换** (#11)。声明该后端说的协议；与客户端协议不同时，proxy 双向转换（request+response+streaming，含 tools/tool_use/tool_result/image）。空 = 与客户端同协议（字节透传，默认）。
  - `validate` 会校验：`protocol: anthropic` 的 target 其 provider 必须配 `anthropic_base_url`（`protocol: openai` 同理需 `openai_base_url`），否则报错。
  - 已保留：文档/文件、`tool_result` 图片与 `is_error`、Responses `web_search`/`tool_search`、跨协议到 Anthropic 时自动生成的 cache breakpoint。剩余有损项主要是 anthropic↔chat 的 thinking、尚无降级实现的 server tool（如 computer）和 chat `input_audio`。`doctor` 会列出转换路由与剩余有损项。
  - 纯 anthropic 后端可只配 `anthropic_base_url`（provider 校验已放宽为「至少一个 base URL」）。

### `shadow:` （配置驱动，非 CLI）

```yaml
shadow:
  <exposed>: {provider: <P>, model: <M>, protocol: <anthropic|openai>}
```
路由每次已交付请求**另发一份**相同 prompt 到 `<P>/<M>`（fire-and-forget，只记录不返回，`request_id` 前缀 `shadow-`）。`protocol` 可选：声明影子后端的协议（默认与请求 body 的协议一致）；不同则 shadow 请求会做对应转换。需 `request_log.enabled`。`replay <id> --to <P>` 见 §15。

---

## 9. `schedule` — 调度查询（需 daemon）

```
schedule   # 查询运行中 daemon 的 GET /debug/schedule
```

逻辑（`internal/cli/schedule.go` 的 `CmdSchedule`）：HTTP GET `http://<LISTEN>/debug/schedule`，10s 超时，渲染 `RenderScheduleRoutes`（`internal/cli/status.go`，与 `serve status` 共享）。

### stdout（`renderScheduleRoutes`，每路由一块）

```
<MODEL> -> <FIRST_CHOICE_PROVIDER>
    <PROVIDER> <TIER>  surplus <+0.00>  p<PRIORITY><EXTRA>
    ...
    sticky:  <STICKY_PROVIDER>, <N>s dwell left      # 仅当有 sticky
```
- `<EXTRA>`：不可用时追加 ` (unavailable)`（红）；peak 时追加 ` peak`（黄）。
- 池化 provider 在 ordered 上方多一行：`    pool: <PARENT> (<N> accounts, <M> available)`。
- 每路由块后一空行。
- 无路由 -> stdout `(no routes)` + exit 0。

### 失败（stderr + exit 1）

| 场景 | 文案 |
|---|---|
| 不可达 | `✗ cannot reach daemon at <LISTEN>: <ERR>` + 换行 `is `model-proxy serve` running?` |
| 非 200 | `✗ daemon returned HTTP <CODE>: <BODY_TRUNC_200>` |
| 解析失败 | `✗ parse schedule response: <ERR>` |

---

## 10. `stats` — 调用统计（需 daemon + web.enabled）

```
stats [--from TIME] [--to TIME] [--provider P] [--model M] [--bucket B] [--granularity day|month] [--cost] [--json]
```

逻辑（`internal/cli/stats.go` 的 `CmdStats` -> `RenderStats`）：GET `http://<LISTEN>/api/stats?...`，10s 超时。`--from`/`--to` = unix 秒或 RFC3339；默认 60min 前..now；`--bucket` 仅展示聚合（`1m`/`10m`/`1h`，存储恒为 1 分钟）；`--json` 原样返回。

> `--granularity day|month` 或 `--cost` 任一存在时，改走 `/api/analytics`（按自然日/月聚合，存储恒为 1 分钟），表格头与列由 `formatAnalyticsTable` 渲染（见下）。两者都省略时输出与原 `stats` 完全一致。

### 摘要行（人类可读模式）

所有人类可读（非 `--json`）输出的**第一行**是一句话摘要，由已取回的查询结果直接聚合，不再发第二次请求：

```
今天 214 请求 · 成功率 97.2% · 等价成本 $3.21 · kimi-code 承担 61%
```

- 窗口名：`[from, to]` 落在当天内显示 `今天`，否则 `MM-DD HH:MM ~ MM-DD HH:MM`。
- `成功率` = `(requests − failures − 429) / requests`（`/api/stats`、`/api/agents` 路径；`/api/analytics` 无失败计数，该段省略），下限钳到 0.0%。
- `等价成本`：仅 `/api/analytics` 且窗口内至少一个已定价点时出现（未定价点不计入）。
- `承担 <P>%`：请求数占比最高的 provider（stats/analytics）；`--by-agent` 时为 input+output token 占比最高的 agent（与表格排序一致）。
- 无数据：摘要行为 `<窗口> 暂无数据`（后续仍打印原有的 `(no stats in range ...)` 行）。

`--json` 输出结构不变（原样透传，无摘要行）。

### stdout（表格，`formatStatsTable`）

表头（`bucketLabel`：60->`1m`、3600 整除->`Nh`、否则`<min>m`）：
```
provider         model               <BUCKET>      reqs failover     429     fail     input    output   lat(ms)  ttft(ms)
```
每行：`<PROVIDER(16)> <MODEL(18)> <MM-DD HH:MM(12)> <reqs(8)> <failover(8)> <429(8)> <fail(8)> <input(10)> <output(10)> <lat(ms)(8)> <ttft(ms)(8)>`（`compactNum`）。`lat(ms)`/`ttft(ms)` = 该桶内已交付响应的平均总时延 / 平均首字时延（毫秒，`SUM/requests` 四舍五入；0 表示无已交付请求）。

空结果 -> stdout `(no stats in range <FROM> .. <TO>, bucket <BUCKET>)` + 换行（`FROM`/`TO` = `MM-DD HH:MM`）。

`--json` -> stdout 原始 JSON（`statsResp`）。

### `--granularity` / `--cost`（`renderAnalytics` -> `/api/analytics`）

任一存在时改走 `/api/analytics`：`--granularity day|month`（仅 `--cost` 时默认 `day`）按自然日/月聚合（存储恒为 1 分钟）；`--cost` 额外追加等价成本列（未知价格 = `n/a`）。表格（`formatAnalyticsTable`）：

```
provider         model               <day|month>     reqs      input    output      cost
```

每行 = 一个 (provider, model) 在窗口内的 SUM（reqs/input/output）；`cost` 列仅 `--cost` 时出现，已定价 = `$X.XX`（点相加），未定价 = `n/a`。`--json` -> stdout 原始 `/api/analytics` 响应（`analyticsResp`）。两者都省略 = 走 `/api/stats`，输出与原 `stats` 完全一致。

### `--by-agent`（`renderAgents` -> `/api/agents`）

带 `--by-agent` 时改走 `/api/agents`（agent 维度：哪个客户端发的请求），把窗口内所有 (agent, provider, model, minute) 桶按 agent 折叠成「谁在烧我的配额」汇总表，按 input+output 总 token 降序：

```
agent                reqs        input       output
```

每行 = `<AGENT(16)> <reqs(10)> <input(12)> <output(12)>`（`compactNum`）。`--from`/`--to`/`--bucket` 仍适用；`--provider`/`--model`/`--agent` 过滤在此模式同样生效（作为 query 参数传给 `/api/agents` 由服务端过滤）。`--json` -> stdout 原始 `/api/agents` 响应（`agentResp`）。agent 识别见 `detectAgent`（claude-cli/x-claude-code-session-id -> `claude-code`，`codex` -> `codex`，`opencode` -> `opencode`，`pi/` -> `pi`，无 UA -> `unknown`，其余 -> `other`）。

> 解析失败时，analytics 路径的报错为 `parse analytics response: <ERR>`（与 `/api/stats` 路径的 `parse stats response: <ERR>` 对应，见下节）。

### 失败（stderr `✗ <ERR>` + exit 1）

- 不可达：`cannot reach daemon at <LISTEN>: <ERR>` + 换行 `is `model-proxy serve` running?`
- 404：`web UI endpoints not available - is web.enabled true on the daemon?`
- 非 200：`daemon returned HTTP <CODE>: <BODY_TRUNC_200>`
- 解析失败：`parse stats response: <ERR>`

---

## 11. `serve status` — 终端状态面板（需 daemon + web.enabled）

```
serve status [--logs [N]] [--json] [--config PATH]
```

逻辑（`internal/cli/status.go` 的 `CmdServeStatus` -> `RenderStatus`）：GET `/api/status` + `/api/tokens`（带 `--logs` 再加 `/api/logs?tail=N`，默认 N=20）。`--json` 合并 `{status, tokens[, logs]}` 原样输出。

### stdout（渲染，各段由 `appendSection` 以空行分隔）

**头部**：
```
model-proxy  v<VERSION> · <UPTIME> · <LISTEN>
```

**Providers (N)**（`renderProviders`）— 表头 + 每行：
```
  PROVIDER          HEALTH        REQS  FAILOVERS   429  FAILURES    LAT   TTFT  LAST
  <NAME(16)>  <HEALTH(13)>  <reqs(8)>  <failover(9)>  <429(5)>  <fail(8)>  <lat(6)>  <ttft(6)>  <CLOCK>
```
`HEALTH` ∈ `available`(绿) / `circuit open`(红) / `half-open`(红) / `rate-limited`(黄) / `rl:quota`(黄) / `rl:daily`(黄) / `unavailable`(dim)（`rl:*` 是 429 分类为配额耗尽/日配额的限频，含冷却倒计时语义同 `rate-limited`）。`LAT`/`TTFT` = 平均总时延/首字节时延（`renderAvgMs`，无样本显示 `—`）。`LAST` = `formatClock(LastRequestAt)`。

**Schedule (N route[s])**（`renderSchedule`，2 空格缩进的 `renderScheduleRoutes`）— 同 §9。

**Quota (N)**（`renderQuota`）— 每 provider 一块：
```
  <NAME>[ · <ACCOUNT>][ · <PLAN>]
      <LABEL>[(ultimate)|(short)]  <PCT%|->  <BAR(16)>  resets <RESET_AT>
```
`q.Err` 非空 -> `      no data (<ERR>)`(dim)。`<BAR>` = `progressBar(usedPct, 16)`。`<PCT>` 为已用百分比，保留小数点后一位（如 `40.0%`）。`resets` 仅当 `ResetsAt` 非零（`formatResetAt`）。

**implicit-route warnings**（仅当有 `st.Warnings`）：
```
⚠  implicit-route warnings
  <WARNING>
```

**Tokens (N model[s] · <TOTAL> requests)**（`renderTokens`）— 表头 + 每行：
```
  PROVIDER       MODEL                  INPUT    OUTPUT  CACHE-CR  CACHE-RD  REQUESTS
  <PROV(14)> <MODEL(22)> <input(10)> <output(10)> <ccr(10)> <crd(10)> <reqs(10)>
```

**Logs (last N)**（仅 `--logs`，且 `/api/logs` 200）：
```
Logs (last <N>)
  <LINE>
```

`--json` -> stdout 合并 JSON（`json.MarshalIndent` 2 空格）。

### 失败（stderr `✗ <ERR>` + exit 1）

同 §10 的 4 类（不可达 / 404-web.enabled / 非200 / 解析失败），文案一致。`config` 加载失败 -> `log.Fatal`（stderr）+ exit 1。

---

## 12. `doctor` — 调度诊断（离线 dry-run / `--live` 实时）

```
doctor [--live]
```

逻辑（`cmdDoctor` -> `doctorWithCfg`）：纯 config 离线诊断，无 live quota（全部 unknown -> tier 然后 priority）。

### stdout

```
✓ config valid

Providers
  <NAME(12)> <TIER(13)>  quota=<SOURCE>  peak=<PEAK>[  pool: <N> accounts]

Routes (dry-run: no live quota -> tier then priority)
  <EXPOSED>
    <PROVIDER(12)> <TIER(13)>  p<PRIORITY>
        pool: <N> accounts (round-robin session-sticky; no live quota -> falls back to priority)   # 池化时
        <VIRTUAL_ID>                                                                              # 每个虚拟 id
    ⚠ no plan provider - only pay-as-you-go                                                       # 该路由无 plan provider 时
        ↔ target protocol <P> — converts when client protocol differs; lossy: …                    # target 声明 protocol: 时
        ⚠ reasoning-required model behind openai-chat conversion — … (replay cache not implemented) # reasoning 模型 + openai-chat 转换时（计入 warning 数）
        ⚠ codex speaks the OpenAI Responses API … — only Responses-speaking clients …              # codex target（线协议说明，计入 warning 数）
        ⚠ no protocol: declared, but <ID> speaks <P> — …; add protocol: <P>                        # 缺 protocol: 且 provider 有协议 hint 时（计入 warning 数）

Scheduling
  sticky_dwell=<D>  quota_poll_interval=<D>  quota_switch_margin=<N> pts  circuit=(threshold <T>, cooldown <D>)

<⚠ N warning(s) | ✓ no warnings>
```
- `<TIER>` = `plan` / `pay-as-you-go`。
- `<SOURCE>` = `quotaSourceLabel(provider_id)`：aqp=`monthly_usage`、codex=`wham/usage`、zhipu=`quota/limit`、volcengine=`GetAFPUsage (AK/SK)`、deepseek=`user/balance`、kimi-code=`usages`、其他=`(none -> unknown at runtime)`。
- `<PEAK>` = `peakSummary`：`09:00-12:00(×2), 14:00-18:00(×2)` 或 `-`。
- 末行：`⚠ <N> warning(s)`（黄）或 `✓ no warnings`（绿）。返回值 = warning 数。

### 失败

config 无效 -> **stdout** `✗ config invalid:  <ERR>`（红）+ exit 1（注意：`doctor` 的无效路径走 stdout + exit 1，同 `config check`）。

### `doctor --live` — 实时诊断（需 running daemon + web.enabled）

```
doctor --live [--config PATH]
```

逻辑（`internal/cli/doctor/live.go` 的 `RenderDoctorLive`）：连 daemon `GET /api/status` + `GET /api/requests?errors=1&limit=5`，叠加本地 takeover 漂移检查（`<configDir>/.model-proxy/<client>.bak` 存在 = 已接管，校验该 client 配置里的 proxy 指针是否仍等于 `takeover.proxy_url` 推导值），输出**结论先行**报告，回答「agent 为什么不动了」。对 daemon 纯只读；唯一磁盘副作用：检出漂移的 client 在 `guard.audit` 开启时追加一条安全审计记录（见 §18）。有 `--live` 时离线报告不再输出。

### stdout

```
model-proxy doctor --live · http://<LISTEN>
✓ daemon running (v<VERSION>, uptime <UPTIME>)

Diagnosis
  ✗ route "<R>": <N> targets all unavailable — earliest recovery <HH:MM> (<PROVIDER>, <CAUSE>)
    → [可立即执行] model-proxy unfreeze <PROVIDER>，或等冷却到期自动恢复
    → [需要凭据] model-proxy login <PROVIDER>（账号池 provider 可再加一个账号分担配额）   # CAUSE 为 quota/daily cooldown 且 provider 可池化时
    → [需要改配置] config.yaml 的 routes.<R> 增加备用 target，或等待配额重置            # CAUSE 为 quota/daily cooldown 且 provider 为单账号（aqp/codex）时
  ⚠ route "<R>" pinned to <P> — no failover while pinned (…)        # pin 生效时
    → [可立即执行] model-proxy unpin <R>
  ⚠ route "<R>": <P> quota nearly exhausted (<N>% remaining)        # 首选 provider RemainingPct ≤ 5%
    → [可立即执行] model-proxy pin <R> <ALT>，临时钉到备用 provider（unpin 恢复）        # 存在其他可用 target 时
    → [需要凭据] model-proxy login <P> / [需要改配置] config.yaml 的 routes.<R> …        # 同上按 provider 是否可池化二选一
  ⚠ <daemon warnings 原样透传>
    → [需要改配置] 按提示修改 config.yaml，然后 model-proxy serve reload 生效
  ⚠ takeover drift: <CLIENT> (<FILE> points to <CURRENT>, want <EXPECTED>)
    → [可立即执行] model-proxy takeover <CLIENT>（要还原客户端则 model-proxy restore <CLIENT>）
  ✓ route "<R>" → <P> (<N>% remaining)                              # 健康 route 的当前落点

Schedule (<N> routes)                                               # 与 serve status 的 Schedule 节同一渲染

Recent failures
  <HH:MM:SS>  <ROUTE> → <PROVIDER>  <STATUS>  <LATENCY>ms           # request_log 开启时，最近 ≤5 条 status≥400
  → [可立即执行] model-proxy unfreeze <PROVIDER>，若 <PROVIDER> 仍在 429 冷却          # 有 429 记录时（按 provider 去重）
  → [可立即执行] model-proxy test <ROUTE>，探测该路由各 target 链路                   # 有 5xx 记录时（按 route 去重）

Takeover
  claude ✓  ·  opencode ✗ drift  ·  codex not taken over  ·  pi ✓
```

- 每条诊断发现的修复建议以分级标签开头：`[可立即执行]` = 现成 CLI 命令；`[需要凭据]` = 需要登录/账号的命令；`[需要改配置]` = 指出要改的 config 键。建议里的 provider 一律用池父名（虚拟 id `parent#<account>` 归一到 `parent`）。
- 结论区按严重度排序：✗ route 全灭 -> ⚠（pin / 配额将尽 / warnings / takeover 漂移）-> ✓ 健康 route 落点；无 ✗/⚠ 时首行 `✓ no problems found`。
- route 全灭判定：schedule `ordered` 中 `available=true` 数为 0。daemon 的 decideOrder 只返回当前可调度目标（全灭时 `ordered` 为空），故 target 数与恢复时间候选由 CLI 端从 config routes + 隐式路由 + 池展开推导；`<CAUSE>` = `quota cooldown` / `daily cooldown` / `rate-limit cooldown` / `circuit breaker` / `model lock`，跨目标取最早恢复（模型锁按 target 的 model 精确匹配，数据源为 `/api/status` 的 `model_locks`）。
- `request_log` 未开启时 Recent failures 节是一行 dim 提示（`request_log disabled — …`），不算错误；无任何失败记录时显示 `none recorded`。
- takeover 三态：`not taken over`（无 .bak）/ `✓`（指针相符）/ `✗ drift`（指针不符、文件丢失或不可读；漂移细节进结论区）。各 client 期望值与 takeover 写入完全一致：claude `env.ANTHROPIC_BASE_URL`、opencode `provider[<pid>].options.baseURL`（含 `/v1` 后缀）、codex `model_provider` + `[model_providers."<pid>"]` 的 `base_url`、pi `providers[<pid>].baseUrl`。
- 漂移审计：`guard.audit` 开启（默认）时，每个漂移 client 追加一条 `kind=drift`、`agent=doctor` 的安全审计记录（`seclog.AppendSync`），`detail` 只含 `client=<名> expected=<期望host> actual=<实际host>`——`net/url` 解析取 `Host`，永不含 URL 路径与查询串；无 scheme 的指针（`evil-host:8317/v1` 会被误解析为 scheme）回退取第一个 `/` 前的部分（过滤控制字符），非 URL 占位值（含空格/括号的 `(file missing)` 等）归一为 `(no-url)`。**同一 client 当天已有 drift 记录则不重复追加**（漂移通常持续到用户修复；去重查询失败不阻断追加）。`guard.audit: false` 不写；append 失败只降级为 stderr `⚠ security audit append failed: <ERR>`，doctor 输出与 exit code 不变。

### 失败（stderr `✗ <ERR>` + exit 1）

同 §10 的 4 类（不可达 / 404-web.enabled / 非200 / 解析失败），文案一致。config 无效同离线路径：**stdout** `✗ config invalid:  <ERR>` + exit 1。

---

## 13. `test <model>` — 端到端链路探测（不需 daemon）

```
test <model> [--config PATH]
```

逻辑（`internal/cli/models/test.go` 的 `CmdTest`）：离线解析 `<model>` 的路由目标（claude_mapping 别名先翻译；显式 routes 按 priority 升序；无显式路由则回退隐式路由），对**每个**目标由 `probeRouteTarget` 调用 `internal/probe.Exchange` 发一次真实最小上游请求（复用 `models refresh` 的 per-provider base/path/auth 接线）。不查询/不改动运行态。

### stdout（每目标一行）

成功：`✓ <MODEL> → <PROVIDER> (<UPSTREAM_MODEL>) — HTTP <CODE> (<LATENCY>)`（绿）。失败：`✗ <MODEL> → <PROVIDER> (<UPSTREAM_MODEL>) — HTTP <CODE>: <REASON> (<LATENCY>)`（红，有上游响应时）或 `✗ … — <REASON> (<LATENCY>)`（无上游响应，如 build/auth/网络错误，不显示伪造的 `HTTP 0`）。claude_mapping 命中时先打印 `claude_mapping: <ALIAS> → <EXPOSED>`。

### 退出码

0 = 至少一个目标 2xx；1 = 全部失败（或无路由）。无路由时 stderr `✗ no route for model "<MODEL>"; available routes: <ROUTE,…>` + exit 1。

---

## 14. `pin` / `unpin` — 手动钉住路由 provider（热切换，需 daemon + web.enabled）

```
pin [<route> <provider>] [--ttl DUR] [--config PATH]
unpin <route> [--config PATH]
```

逻辑（`internal/cli/pin.go` 的 `CmdPin`）：不改 yaml，临时把某路由钉到一个 provider。`pin` 经 `POST /api/pin` 由 `internal/web` transport 的 `CommandAPI` 写入 daemon 的 `internal/runtime.Manager`；`Manager.DecideOrder` 在 availability 过滤前对该路由做独占过滤——**只保留被钉 provider 的 target，不故障转移**（池化 provider 按父名钉，如 `zhipu` 钉住所有 `zhipu#<id>` 虚拟）。`--ttl` 到期 / `unpin` / daemon 重启即失效（纯内存）；operator pin 有意跨 config reload 保留。`pin`（无参数）`GET /api/pin` 列出活跃 pin；`unpin` `DELETE /api/pin?route=`。活跃 pin 在 `schedule` / `/debug/schedule` 每路由块标 `pinned: <PROVIDER> (<expires in …>)`。

### stdout

`pin <route> <provider>` 成功：`✓ pinned <ROUTE> → <PROVIDER> (<no expiry … | expires <RFC3339>>)`。`pin`（列表）：表头 `ROUTE PROVIDER EXPIRES` + 每行，或 `(no active pins)`。`unpin`：`✓ unpinned <ROUTE>` 或 `• no pin on <ROUTE>`（灰）。

### 失败（stderr `✗ <ERR>` + exit 1）

- 不可达：`cannot reach daemon at <LISTEN>: <ERR>` + 换行 `is `model-proxy serve` running?`
- 路由/provider 无效（400）：`cannot pin "<ROUTE>" to "<PROVIDER>": no such route, or the route has no target for that provider`
- `--ttl` 非法：`invalid --ttl "<V>" (use a Go duration like 1h, 30m, 2h45m)`

---

## 15. `replay` — 用另一后端重答（需 daemon + request_log）

```
replay <id> --to <provider> [--config PATH]
```

逻辑（`internal/cli/replay.go` 的 `CmdReplay`）：从 daemon 的 `GET /api/requests/<id>` 取回原请求（method/path/body，需 `request_log.enabled`），再以 `x-mp-force-provider: <provider>` 头把同一 body 重发到 proxy（该头对**这一条请求**做一次性 provider 钉死，不动全局 pin），把新后端的响应写 stdout，用于并排对比。

### stdout

新后端的响应原文（raw body，SSE 则为原始事件流）。

### stderr

进度行：`• replay <ID> → <PROVIDER> (HTTP <CODE>)`（灰）。失败：`✗ <REASON>` + exit 1。

### 失败（stderr `✗ <ERR>` + exit 1）

- 不可达：`cannot reach daemon at <LISTEN>: <ERR>` + 换行 `is `model-proxy serve` running?`
- 无日志：`no request log for id <ID> (is request_log.enabled on?)`（404）
- 记录无 body：`record <ID> has no captured request body`
- 上游 ≥400：`✗ <BODY_TRUNC_400>`

> 影子评测（shadow，配置驱动，非 CLI）：`shadow: {<route>: {provider: <P>, model: <M>}}` 时，路由每次已交付请求会**另发一份**相同 prompt 到 `<P>/<M>`（fire-and-forget），只记录（`request_id` 前缀 `shadow-`，provider 为影子 provider）不返回。在 Requests 页按 provider 过滤即可与主后端并排比较；`GET /api/requests` 另有 `shadow=only|exclude` 参数（仅影子 / 排除影子，空=全部，其它值忽略），Requests 页过滤行有对应下拉，影子行 provider 名后带 `shadow` 徽标。需 `request_log.enabled`。

---

## 16. `unfreeze` — 清理冻结的 provider 状态（需 daemon + web.enabled）

```
unfreeze [provider] [--config PATH]
```

逻辑（`internal/cli/unfreeze.go` 的 `CmdUnfreeze`）：经 `POST /api/health/reset`（`internal/web` transport 的 `CommandAPI`）清 daemon 内存里的**冻结运行态**——熔断开路冷却、429 限频冷却（含 quota/daily 类的长冷却）、模型级锁定（model lockout）——目标 provider 下次请求立即重试，不再等冷却到期。不带参数清全部 provider；池化父名清其全部虚拟账号（同 pin 的匹配语义）。**不清** sticky、pin、已学习的剥参 blocklist（请求体知识，非冻结态）。用于异常边界：账号已充值、429 误分类、上游窗口提前重置等。请求体为空=清全部；**畸形 JSON 返回 400（防误清全部）**；清理后**同步落盘成功才返回 200**（否则 500）——持久化在 `quota_state.json` 的冻结态同步被清后状态覆盖，不会在下轮配额落盘前因重启复活。

### stdout

- 有清理：`✓ unfroze <all providers|PROVIDER>: <NAME(, NAME…)> (+<N> model lock(s))`（绿；NAME 为被清的 provider 冷却条目，无冷却仅有锁时显示 `(no provider cooldowns)`）
- 无冻结态：`• no frozen state on <all providers|PROVIDER>`（灰）

### 失败（stderr `✗ <ERR>` + exit 1）

- 不可达：`cannot reach daemon at <LISTEN>: <ERR>` + 换行 `is `model-proxy serve` running?`
- daemon 非 200：响应体截断 200 字符

---

## 17. `wire record` — 录制上游 SSE 黄金流（离线，凭据来自 login）

```
wire record <provider> [--model M] [--prompt P] [--out DIR]
```

逻辑（`internal/cli/wire.go` 的 `CmdWireRecord`）：对 provider 的三个端点各发 `stream=true` 最小请求（`/responses`、`/chat/completions` 走 `openai_base_url`；`/v1/messages` 走 `anthropic_base_url`，缺省回落 `openai_base_url`），把**原始响应字节**写入 `<out>/<proto>_<provider><scenario>.sse`（`--out` 默认 `testdata/wire/`，供 `internal/protocol/convert_golden_test.go` 回放）。每端点录 3 个场景：**text**（无后缀，prompt 一句话）、**`_tool`**（强制工具调用：`get_weather` + `tool_choice` 强制）、**`_thinking`**（开启推理：responses 用 `reasoning.effort:low`、chat 用 `reasoning_effort:low`、anthropic 用 `thinking.budget_tokens`）——后两个覆盖工具调用/思考流这些纯文本流碰不到的转换硬路径，不支持的场景按失败写 `.err`。请求构造与 forward 同序：RewriteRequest → AuthHeaders → 配置 `headers` → ExtraHeaders（`/v1/messages` 预置 `anthropic-version`）；单请求 30s 超时；`--model` 缺省取 provider 首个模型/首个路由目标。responses 请求的两个特殊性：input 用 list 形式（codex 拒绝字符串简写）、不带 `max_output_tokens`（codex 400）。

### stdout / stderr

- 成功：`  ✓ <proto> → <out>/<proto>_<provider>.sse (<N> bytes)`
- 端点无 base url：`  - <proto>: skipped (no base url)`
- 非 2xx：状态码 + body 摘录（≤4KiB）写入 `<proto>_<provider>.err`，stderr `✗ <proto>: HTTP <status> — wrote <ERRFILE> (existing .sse untouched)`；**不覆盖已有 .sse**。任一端点失败则 exit 1。

凭据来自 `login`（providerImplFor）；录制文件只含响应字节，但 prompt/模型输出仍可能敏感——提交前人工审查。

---

## 18. `audit` — 安全审计日志（离线，不需 daemon）

```
audit [--stats] [--from TIME] [--to TIME] [--kind KIND] [--limit N] [--json] [--config PATH]
```

逻辑（`internal/cli/audit.go` 的 `CmdAudit` -> `RenderAudit`）：离线直读 seclog 目录——`guard.audit_path`（默认 `~/.model-proxy/security.log`）取 `filepath.Dir`，扫描其中全部 `security-*.log`（活动 + 轮转文件，daemon 不在也能查，同 `doctor` 离线语义）。config 加载失败 -> `log.Fatal`（stderr）+ exit 1（同 `stats`）。

- `--from` / `--to`：`now`、时长（`1h`/`30m`，表示"多久之前"；另接受整数天数后缀 `7d`）、unix 秒、RFC3339；默认不限（闭区间，毫秒精度）。负时长（如 `-1h`/`-7d`）报错；`--from` 晚于 `--to` -> `✗ --from is after --to (empty window)` + exit 1。
- `--kind`：`secret` | `path` | `drift`；其他值 -> stderr `✗ invalid --kind "<V>": must be secret, path, or drift` + exit 1。
- `--limit N`：只保留最新 N 条（默认 50；`0`/负数 = 全部；`--stats` 下忽略）。非整数 -> stderr `✗ invalid --limit: …` + exit 1。
- `--stats`：聚合视图替代原始记录——对**过滤后的全集**统计（忽略 `--limit`，改用内部上限 10000 条，超出按最新优先截断）：总数 + 按 kind 命中数、命中名（`names` 展开）top 10、agent top 10、按 action 计数。可与 `--from`/`--to`/`--kind` 组合。
- `--json`：stdout 为 records 数组原样 JSON（`seclog.Record`，最新在前；空结果为 `[]`），供 jq。与 `--stats` 组合时输出聚合对象：`{"from":ms,"to":ms,"total":N,"by_kind":{...},"by_action":{...},"top_names":[{"name","count"}],"top_agents":[...]}`（`from`/`to` 为查询窗口，`0`/不限则省略；列表按 count 降序、name 升序）。
- 未识别 flag/位置参数 -> `✗ unknown flag "<A>"` + exit 1；`--from`/`--to`/`--kind`/`--limit` 缺值 -> `✗ <FLAG> requires a value` + exit 1（`--config` 及其值由 configPath 消费，不算未知）。

### stdout（表格，`FormatAuditTable`）

```
time           kind    agent         route             names                 action  detail
<MM-DD HH:MM:SS(14)> <kind(7)> <agent(12)> <exposed(16)> <逗号连接(20)> <action(7)> <detail>
```

记录按时间倒序（最新在前）。空结果 -> `(no security audit records in <DIR>)`；目录不存在 -> `(no security audit records yet — <DIR> does not exist)`（均 exit 0）。扫描中跳过的不可解析行数（含无法打开的日志文件，每个计 1）追加一行 `  (<N> unreadable line(s) skipped)`；文件末尾无换行符的半行是 daemon 写入中的撕裂尾行，直接忽略、不计入 skipped。detail 列渲染前过滤控制字符（`\n`/`\t`/ANSI 转义等 -> 空格），防生产者破坏表格。

### stdout（`--stats` 聚合，`FormatAuditStats`）

```
security audit stats  range: <YYYY-MM-DD HH:MM:SS|-> .. <同上|->  total: <N> record(s)

by kind
  <kind>      <count(右对齐6)>
top names (top 10)
  <name>      <count>
top agents (top 10)
  <agent>     <count>
by action
  <action>    <count>
```

首行 range 是查询窗口（不限显示 `-`，非数据 min/max）。各段按 count 降序、name 升序；空段（如无 action）整段省略。`total` 为 0 时只输出首行 + `(no security audit records in <DIR>)`。

---

## 契约改动清单（改动时须核对）

改契约时，除更新本文档外，还需同步这些测试断言（`strings.Contains` 精确文案）：

- `internal/cli/models_cli_test.go`：`models refresh` 的 `config: added/removed ...`、fallback 通知、`models` 歧义告警、`models pull`；`internal/cli/cli_takeover_test.go`：takeover/restore。
- `internal/cli/models_cli_test.go`：`models refresh` 未知/无参数 provider；`internal/cli/cli_subcommands_test.go`：`config` 子命令。
- `internal/cli/models/models_check_test.go`：`PrintKeptModels` / `PrintFilterSummary` 输出。
- `internal/cli` 的 serve status / stats `render*` 函数均有 httptest 单测锁文案。
- `internal/provider/*_test.go`：`usage` 展示的 `Provider:` 首行 + 配额窗口标记。
- `internal/cli/audit_cli_test.go`：`audit` 表格/`--json` 输出、flag 与时间解析错误文案；`internal/cli/doctor/doctor_drift_audit_test.go`：漂移审计记录（host-only detail、当日去重、audit 关闭）。

新增列/字段允许（追加式，向后兼容）；改动既有列宽、既有文案、退出码、stdout/stderr 归属**需先与用户确认**。
