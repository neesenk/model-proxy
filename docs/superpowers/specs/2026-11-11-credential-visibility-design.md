# 凭据失效可见性：全局横幅 + failover leg 失败证据 — 设计

日期：2026-11-11
状态：已批准（用户裁定：全程不再中途确认，做完并完整测试；CLI 端 warning 取消；leg 的 auth 层失败记观测状态码而非合成 502）

## 背景

2026-11-11 aqp OAuth 会话过期（`mint aqp key: HTTP 401: Session expired`），全部负载 failover 到 shopee，用户感知「性能差」数小时无法定位。两个可观测性缺口：

1. 凭据失效只体现在 serve.log 与 Accounts 页药丸；在 Requests/Config 等任意页工作时完全不可见。
2. 失败腿（failover leg）的 request log 行 `response_body` 为空、status 被合成 502：`internal/targetexec/executor.go` 四条 hard-fail 路径不设 `AttemptBody`，其中 `AuthHeaders` 失败（mint 401 即此类）连 `AttemptStatus` 也没有，`internal/forward/forward.go` 落腿时把 0 合成为 502。真实原因（401 Session expired）无处可看。

## 需求裁定

- Web UI：全局顶部横幅，所有 tab 可见，常驻至凭据恢复（不可手动关闭），无 CLI 端提示。
- 触发口径：`CredentialErrClass(err) ∈ {session-expired, auth}`；`not-logged-in` 不触发（配置了但未登录是显式状态，不告警）。
- leg 失败证据：失败原因文本必须落进腿行 `response_body`；RESULT 区显示错误首行摘要；auth 层失败记认证端点**观测到的**状态码（401），不再一律合成 502。

## 设计

### A. 全局 credential 横幅

**数据通道**：`GET /api/status` 新增 `credential_alerts: [{key, class, err, since}]`
（`key`=配额键如 `aqp#71e0…`，`class`=`session-expired|auth`，`err`=原始错误串，`since`=unix 秒）。

- 生成点：`internal/admin/read.go` 的 Dashboard 组装（`QuotaSyncErrs` 已在同一快照内），过滤 `runtime.CredentialErrClass` 属于横幅集合。横幅集合与 schedule 徽标集合（含 not-logged-in）的差异显式化为两个具名常量，同源分流。
- 同步成功即清除（`syncErrs` 现有契约），横幅自动消失；无需新增运行时状态。
- 不把每请求的 mint 失败写运行时状态（避免请求路径反写 reload-owned 状态的锁序问题）；quota 轮询已在一个周期内捕获。

**前端**：
- `index.html` topbar 下加 `#cred-banner` 容器；`app.js` `renderCredBanner(st)` 由两处既有 `/api/status` 轮询驱动（`refreshConnIndicator` 全局 5s tick + Status tab 自身 tick），有则填充、无则移除。
- 与 `setConn` 同级豁免交互门（只 swap topbar chrome 下一条 DOM，不碰面板）。
- 文案（纯函数进 `pure.js`，`jstests` 覆盖）：`⚠ <parent>#<短id> 会话过期 — 运行 model-proxy login <parent> 重新登录`；`auth` 类 → `key 被拒 — 请更新该账号凭据`。多账号逐行。
- 样式复用 `:root` 语义 token，登记模式注册表。

### B. failover leg 失败证据

**`internal/provider`**：auth 层包装 typed error 携带观测状态码（mint HTTP 失败路径 `HTTPStatusError{Status}`）；`errors.As` 提取。

**`internal/targetexec/executor.go`**：
- `AuthHeaders` 失败：`AttemptStatus` = typed error 的观测码（无则 0，由 forward 合成 502），`AttemptBody = []byte(err.Error())`。
- `ApplyConfiguredHeaders` / `build upstream req` 失败：`AttemptBody = []byte(err.Error())`（status 维持合成 502）。
- 上游 401 分支：失败落定时 `peekResponseBody` 进 `AttemptBody`（refresh-continue 路径不 peek）。

**`internal/forward/forward.go`**：不变（本就存 `result.AttemptBody`，status 0→502 合成保留为「无观测值」回退）。

**前端（Requests 详情）**：RESULT 区当腿行 body 存在时显示首行摘要（≤200 字符，`pure.js` 纯函数）；`raw response body` 折叠面板自然有内容。

### 非目标

- 不为 not-logged-in 弹横幅；不改 schedule 徽标口径；不做 CLI 提示；不改 committed 行为。

## 测试

- `internal/provider`：mint 401 包装出 `HTTPStatusError{401}`。
- `internal/targetexec`：逐失败路径断言 `AttemptBody` 非空含关键子串、auth 失败 `AttemptStatus`=观测码、无观测码路径为 0；401 落定路径 peek 进 body、refresh-continue 不 peek。
- `internal/forward`：腿行落盘带 body 与正确 status。
- `internal/admin` + `internal/web`：`credential_alerts` 过滤（not-logged-in 不进、恢复清空）、JSON 契约。
- `jstests/pure.test.mjs`：横幅文案映射 + 腿摘要纯函数；`jstests/contract.test.mjs` 对 web-api.md 契约。
- UI 行为改动追加 uie2e（横幅出现/消失路径）。

## 文档同步

`docs/web-api.md`（/api/status 字段）、`README.md`（Web UI 横幅一句）、`docs/frontend.md`（横幅与腿摘要行为契约）。
