# CLI 契约（命令行逻辑 + 信息展示契约）

本文档是 model-proxy CLI 的**对外行为契约**：每个子命令做什么、stdout/stderr 的精确格式与文案、退出码。`AGENTS.md` 的「CLI 命令」一节是速查表，本文档是精确规范。

> **变更控制**：本文档记录的 stdout/stderr 格式、文案、退出码是**稳定契约**，脚本和用户依赖它们。**修改这些契约前必须先与用户确认**（见 `CLAUDE.md` 的对应指令）。新增字段/列允许追加（向后兼容），但不得改动既有行的格式或删改既有文案。契约改动需同步更新本文档 + 相关测试（`*_test.go` 里的 `strings.Contains` 断言）。

---

## 全局约定

### 退出码

| 码 | 含义 | 触发点 |
|---|---|---|
| `0` | 成功 | 命令正常返回；`serve` worker 收到 SIGINT/SIGTERM 优雅退出（`daemon.go:154`） |
| `1` | 运行时错误 | `log.Fatal(...)`（默认 exit 1）；显式 `os.Exit(1)`：config 无效、daemon 不可达、响应解析失败、未知子命令/参数 |

真实 CLI 路径**不使用** exit 2。（`cli_test.go` 的 `TestHelperProcess` 在未知 `MP_SUBCMD` 时 `os.Exit(2)`，那是测试桩，不是真实路径。）

`log.Fatal` 把消息打到 **stderr** 再 exit 1；`log.Printf` 同样打 stderr。故 stderr 是诊断/进度/告警/错误的统一流。

### 流约定

- **stdout** = 命令的结果数据（表格、列表、JSON、状态行）。脚本应只读 stdout。
- **stderr** = 进度提示、告警、错误、`log.*` 输出。面向人，不面向脚本。
- 例外：`takeover`/`restore` 的逐客户端进度走 `log.Printf`（stderr）；`models refresh` 的 diff 行（`config: added/removed ...`）和 fallback 通知走 stderr；`config check` 的 `✗ config invalid` 走 **stdout**（`fmt.Println`，见下）。

### 着色

两套独立开关（`color.go`）：

- `colorEnabled` — stdout 着色（`cGreen`/`cRed`/`cYellow`/`cDim`/`cBold`/`cBlue`/`cCyan`/`cGray`/`cMagenta`）。
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

### 子命令分发（`daemon.go:64` `cmdServe`）

| 调用 | 行为 |
|---|---|
| `serve`（无子命令） | 前台运行（`runProxy`）：加载 config、起 stats、注册 mux、`http.ListenAndServe`。 |
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

---

## 2. `takeover <client>` — 接管客户端配置

```
takeover <client>   # client ∈ {claude, opencode, codex, pi, all}
```

逻辑（`takeover.go:66` `runTakeover`）：备份每个客户端配置（verbatim + sha256 meta，幂等）到 `<configDir>/.model-proxy/`，再改写指向代理。含隐式路由模型；opencode/pi 额外 hydrate models.dev 元数据。

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

失败：`log.Fatal(err)` -> stderr + exit 1（config 加载失败 / 备份失败 / 改写失败）。`client` 不在集合内由 `listClients` 决定（`all` 展开全部；未知名通常导致空集，静默返回 0）。`takeover:` 块整个可省略--四个 client 路径 + provider_id 有代码默认值，只有覆盖某项才需写。

---

## 3. `restore <client>` — 还原客户端配置

```
restore <client>   # client ∈ {claude, opencode, codex, pi, all}
```

逻辑（`takeover.go:110`）：从 `<BAKDIR>/<client>.bak` verbatim 复制回原路径。输出同 §2 的 restore 行。失败：`log.Fatal` -> stderr + exit 1（无备份 -> `no backup for <client> in <BAKDIR>: ...`）。

---

## 4. `login <provider>` — 登录

```
login <provider> [--label <name>] [--replace]
```

逻辑（`login.go:29` `cmdLogin`）：按 `provider_id` 分派。aqp=SSO、codex=OAuth device flow、zhipu/deepseek/kimi-code=apikey 池、volcengine=apikey+AK/SK 三元组池、zcode=BigModel Coding Plan（开 bigmodel.cn/login + apikey 池）。成功后 `maybeReloadDaemon`（热重载运行中的 serve，无 daemon 时静默 no-op）。

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

### codex（OAuth device flow，`codex_login.go` `cmdCodexLogin`）

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

### apikey 类（zhipu/deepseek/kimi-code，`runApiKeyLoginWithInput`）

- stdout 提示：`Enter API key for <PROVNAME>: `（stdin 读 key）。
- stderr（当 provider 配了 `usage_url`，zhipu/deepseek/**kimi-code** 均配）：`Validating API key...`。校验 = GET `usage_url` with `Authorization: Bearer <key>`；**401/403 或网络错误** → `login failed: validation failed: HTTP <N>: <BODY>`（exit 1，**不写池**）；其余状态码（200/404 等）= key 通过（写池）。
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

---

## 5. `logout <provider>` — 登出

```
logout <provider> [--label <name>] [--all]
```

逻辑（`main.go:287` `cmdLogout`）：aqp/codex/无池文件 -> 单文件 `Logout()`；有池文件 -> 池路径（`--all` 清空 / `--label` 删指定 / 否则交互式列号选择）。

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

逻辑（`main.go:436` `cmdUsage` -> `printProviderUsage`）：无参数时按 provider 名排序逐个打印，块间用 `usageDivider` 分隔。未知 provider -> stderr `unknown provider ...` + exit 1。

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
然后按 `provider_id` 调 `showDeepseekUsage` / `showVolcengineUsage` / `showGenericUsage`。

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
| aqp (`showAqpUsage`) | `Account:    <EMAIL>`；`Project ID: <ID>`；`Usage:      [<BAR>] <PCT>% used · $<USAGE> / $<TOTAL>  (balance $<BAL>, <PLAN>, <YEAR>-<MONTH>)`；`Store:      <PATH>` |
| codex (`showCodexUsage`) | `Account:   <EMAIL|"(unknown)">`；`Plan:      <PLAN_TYPE>`；`Credits:   unlimited`/`has credits (<BAL>)`/`none`；`Rate Limit:` 状态；`Usage:` `limit reached` 或 `[<BAR>] <PCT>% used · <USED> / <TOTAL> credits, resets <DUR>` |

> `<PCT>` 为已用百分比，统一保留小数点后一位（如 `56.8%`、`25.0%`）。
| zhipu (`showGenericUsage`) | 5h/weekly token 限额 + 月度时间限额（带进度条）；`TIME_LIMIT` 按 MCP 工具（search-prime/web-reader/zread）分解；回退：OpenAI 风格模型列表 |
| deepseek (`showDeepseekUsage`) | `Available:  no (insufficient balance)`（余额不足时）；各币种 `total/granted/topped-up` 余额 |
| volcengine (`showVolcengineUsage`) | 无 AK/SK：`Note:` 说明 + 列 config 模型；有 AK/SK：`Plan: <PLAN_TYPE>` + `AFPFiveHour/Daily/Weekly/Monthly` 各窗口 Quota/Used/Remaining/ResetTime |
| kimi-code (`showGenericUsage`) | `Plan:       Kimi Code membership`；`Weekly limit`（Ultimate，7d）+ `5h limit`（Short，5h）+ 其他限额窗口（带进度条/重置时间）+ `Extra usage`/`Monthly cap` 钱包窗口（`n/a`）。取自 `/usages`。拉取失败：`Usage:      (unavailable: <ERR>)` + 控制台提示 + 列 config 模型 |

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

stdout：`models.dev catalog refreshed: <N> unique models, etag <ETAG>`。失败 -> stderr + exit 1（`log.Fatal`）。

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

写 `config.yaml` 到 **CWD**（`os.WriteFile("config.yaml", defaultConfigYAML, 0o644)`）。stdout：`wrote config.yaml`。失败 -> stderr + exit 1。

模板里的 `scheduling:` 整块默认是注释掉的（每行带 `(default N)`）：所有字段都有代码默认（`config.go` 的 accessor），不写即用默认，需覆盖时取消注释对应行。`config check` 的 `scheduling:` 摘要行始终打印**生效值**（已覆盖则显覆盖值，未配则显代码默认）。

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
  ```

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
  - 转换有损项（dropped + warned，不静默）：thinking/redacted_thinking 块、`cache_control` 断点、服务端 tools（web_search/computer/...）、`tool_result` 内的图片。`doctor` 会列出哪些路由发生转换 + 有损清单。
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

逻辑（`main.go:1694` `cmdSchedule`）：HTTP GET `http://<LISTEN>/debug/schedule`，10s 超时，渲染 `renderScheduleRoutes`（`serve_status.go`，与 `serve status` 共享）。

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

逻辑（`cmd_stats.go:75` `cmdStats` -> `renderStats`）：GET `http://<LISTEN>/api/stats?...`，10s 超时。`--from`/`--to` = unix 秒或 RFC3339；默认 60min 前..now；`--bucket` 仅展示聚合（`1m`/`10m`/`1h`，存储恒为 1 分钟）；`--json` 原样返回。

> `--granularity day|month` 或 `--cost` 任一存在时，改走 `/api/analytics`（按自然日/月聚合，存储恒为 1 分钟），表格头与列由 `formatAnalyticsTable` 渲染（见下）。两者都省略时输出与原 `stats` 完全一致。

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

逻辑（`serve_status.go:486` `cmdServeStatus` -> `renderStatus`）：GET `/api/status` + `/api/tokens`（带 `--logs` 再加 `/api/logs?tail=N`，默认 N=20）。`--json` 合并 `{status, tokens[, logs]}` 原样输出。

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
`HEALTH` ∈ `available`(绿) / `circuit open`(红) / `half-open`(红) / `rate-limited`(黄) / `unavailable`(dim)。`LAT`/`TTFT` = 平均总时延/首字节时延（`renderAvgMs`，无样本显示 `—`）。`LAST` = `formatClock(LastRequestAt)`。

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

## 12. `doctor` — 离线调度诊断（不需 daemon）

```
doctor
```

逻辑（`main.go:1729` `cmdDoctor` -> `doctorWithCfg`）：纯 config 离线诊断，无 live quota（全部 unknown -> tier 然后 priority）。

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

---

## 13. `test <model>` — 端到端链路探测（不需 daemon）

```
test <model> [--config PATH]
```

逻辑（`test_cmd.go:18` `cmdTest`）：离线解析 `<model>` 的路由目标（claude_mapping 别名先翻译；显式 routes 按 priority 升序；无显式路由则回退隐式路由），对**每个**目标用 `probeModelCallable` 发一次真实最小上游请求（复用 `models refresh` 的 per-provider base/path/auth 接线，`test_cmd.go:86` `probeRouteTarget`）。不查询/不改动运行态。

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

逻辑（`pin_cmd.go`）：不改 yaml，临时把某路由钉到一个 provider。`pin` 经 `POST /api/pin`（`web.go` `handlePinSet`）写入 daemon 内存 `Proxy.pins`（`healthMu`）；`decideOrder` 末尾对该路由做独占过滤——**只保留被钉 provider 的 target，不故障转移**（池化 provider 按父名钉，如 `zhipu` 钉住所有 `zhipu#<id>` 虚拟）。`--ttl` 到期 / `unpin` / daemon 重启即失效（纯内存）。`pin`（无参数）`GET /api/pin` 列出活跃 pin；`unpin` `DELETE /api/pin?route=`。活跃 pin 在 `schedule` / `/debug/schedule` 每路由块标 `pinned: <PROVIDER> (<expires in …>)`。

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

逻辑（`replay_cmd.go` `cmdReplay`）：从 daemon 的 `GET /api/requests/<id>` 取回原请求（method/path/body，需 `request_log.enabled`），再以 `x-mp-force-provider: <provider>` 头把同一 body 重发到 proxy（该头对**这一条请求**做一次性 provider 钉死，不动全局 pin），把新后端的响应写 stdout，用于并排对比。

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

## 契约改动清单（改动时须核对）

改契约时，除更新本文档外，还需同步这些测试断言（`strings.Contains` 精确文案）：

- `cli_extra2_test.go`：`models refresh` 的 `config: added/removed ...`、fallback 通知、`models` 歧义告警、`models pull`、takeover/restore。
- `cli_test.go`：`models refresh` 未知/无参数 provider、`config` 子命令。
- `models_check_test.go`：`printKeptModels` / `printFilterSummary` 输出。
- `serve_status.go` / `cmd_stats.go` 的 `render*` 函数均有 httptest 单测锁文案。
- `provider/*_test.go`：`usage` 展示的 `Provider:` 首行 + 配额窗口标记。

新增列/字段允许（追加式，向后兼容）；改动既有列宽、既有文案、退出码、stdout/stderr 归属**需先与用户确认**。
