# S1 设计草案：凭据存储加密（OS keychain）

> 状态：**已实现核心（2026-08）**。`internal/credstore` 落地，四个凭据 I/O 面已接入：ApiKeyBase（zhipu/deepseek/kimi-code/qwen-plan 单账号）、accounts.Store 池文件（`<name>_apikeys.json`）、codex OAuth store、aqp SSO/account store。volcengine 遗留 cred 文件与 Web UI 状态展示为后续项。实现与草案的差异见 §8。实现契约落地时应同步进 `docs/architecture/protocol-conversion.md`（凭据部分）与 `internal/provider/AGENTS.md`。
> 关联调研：`docs/research/similar-projects.md` §5-S1。

## 1. 现状与问题

- 凭据以明文 JSON 存于 `~/.model-proxy/<providerName>_<suffix>.json`（`_apikey.json`、`_oauth_auth.json`、SSO cookie 等），写入统一走 `internal/provider/persist.go` 的 `atomicWriteFile`（0600 + 原子替换，防 token 轮换中途 crash 锁号）。
- 0600 只防其他用户；同用户任意进程、云盘/备份同步、误打包提交都能直接带走 OAuth refresh token。竞品（CLIProxyAPI/cc-switch）同样明文——做成加密是可宣传的安全差异。

## 2. 目标 / 非目标

**目标**
1. 默认部署下凭据静态内容不可被同用户无关进程直接读取。
2. 明文文件自动迁移，用户无感。
3. headless 环境（无 keyring 服务）行为不回退：继续用文件并明确告警。

**非目标**
- 不做跨机器同步、不做主密码体系、不加密 config.yaml 本身（config 不含凭据是既有红线）。

## 3. 方案

### 3.1 存储 seam：新增 `internal/credstore`

```go
package credstore

// Blob 是凭据文件的原始字节（JSON），credstore 不理解其内部结构。
type Store interface {
    Load(name string) ([]byte, error)   // ErrNotFound 表示无该条目
    Save(name string, blob []byte) error
    Delete(name string) error
}
```

三个实现：

| 实现 | 后端 | 选择条件 |
|---|---|---|
| `keychainStore` | go-keyring（macOS Keychain / Windows Credential Manager / Linux libsecret） | `MP_CRED_STORE=auto`（默认）且服务可用 |
| `fileStore` | 现 `~/.model-proxy/*.json` 原样 | `MP_CRED_STORE=file`，或 auto 探测失败 |
| `fakeStore`（测试） | 内存 map | 仅 `_test` |

- 条目命名：service 固定 `model-proxy`，account = 文件名（如 `codex_oauth_auth.json`）。文件名语义不变，所有既有路径推导逻辑不动。
- 可用性探测在进程启动时做一次（打开一个 probe entry 或查询服务存在性），结果缓存；避免每次请求付 IPC 成本判断。

### 3.2 Provider 层接入点

Provider struct 目前各自持有 `authFile string` 并调用 `os.ReadFile` / `atomicWriteFile`（`apikey.go`、`auth.go` 等）。改造为：

- `NewXxxProvider(authFile)` 保持签名不变，内部把 `authFile` 包成 `credstore.Ref{Name: filepath.Base(authFile), FallbackPath: authFile}`。
- 读路径：`ref.Load()` → keychain 有 → 返回；keychain 无且 fallback 文件存在 → 读文件并触发迁移（见 3.3）；都没有 → 与今天相同的 not-found 错误。
- 写路径：一律 `ref.Save()`（含 token 轮换的 save 调用点）。**原子性与防锁号语义必须保留**：fileStore 继续走 `atomicWriteFile`；keychainStore 天然单条目替换，无 truncation 风险。
- 删除路径（logout）：`ref.Delete()` 同时清 keychain 条目和残留文件。

### 3.3 迁移策略（lazy，一次成功）

首次 `Load` 命中「keychain 空 + 文件存在」时：
1. 读文件 → 写 keychain；
2. 成功后删除原文件（先 rename 为 `<name>.migrated.bak`，下次启动成功后再清掉——保留一次回滚窗口）；
3. 发一条 observe live event（不含任何凭据内容，仅 "credential migrated to OS keychain: <name>"）。

### 3.4 降级与告警

- `MP_CRED_STORE=auto` 且 keyring 服务不可用（headless Linux 无 dbus/secret service）：静默回落 fileStore，但启动时打一条 warning 日志 + `/api/status` 暴露 `credential_store: file (fallback)`，doctor 同步展示。
- 显式 `MP_CRED_STORE=keychain` 而服务不可用：fail-closed，login/save 直接报错（用户点名要求就不能悄悄降级）。

## 4. 红线核对

- **锁序**：keychain 读写是外部 IPC，等价外部 I/O。现有凭据 I/O 已发生在请求转发路径上（Refresh）；接入后必须确认这些调用点不持 `Proxy.mu` / runtime.Manager 锁——按现状 provider 层不被上层锁包裹，保持不变即可；评审时逐个 callsite 核对。
- **fail-closed**：keychainStore Save/Load 失败返回 error，不得吞错后假装成功（token 轮换丢写 = 锁号事故）。
- **日志**：live event / 日志只出现条目名，永不出现 blob 内容。

## 5. 测试计划

- `internal/credstore`：fakeStore 单测迁移逻辑（lazy migrate、`.bak` 清理、双写失败回滚）。
- 各 provider 包：现有凭据读写测试改为注入 fakeStore，断言「写经 store」「读兼容旧文件」语义不变。
- 架构 guard：archtest 增加「provider/config 包不得直接 import keyring 库」闭合约束（只允许 credstore 引入）。
- 不写真实 HOME、不依赖真实 keyring 服务（CI 用 fake；真机集成测试手动跑）。

## 6. 分阶段落地

1. **P1**：credstore 包 + fileStore 等价替换（纯重构，零行为变化）+ archtest 约束。
2. **P2**：keychainStore + lazy migration + doctor/status 可见性。
3. **P3**：README 安全章节更新；评估 logout/login 全链路清理。

## 7. 风险与实现结论（§8 实现差异）

| 风险 | 结论/缓解 |
|---|---|
| Linux CI/容器无 secret service | auto→file 回退 + 显式 env 强制 |
| 用户备份脚本失效（文件消失） | 迁移 live event 未做；README 已说明 `.migrated.bak` 保留一代回滚 |
| keyring 服务卡死拖慢 Refresh | 本轮未加超时（go-keyring 无 context API）；dbus 挂起场景待观察，必要时后续包一层带超时的调用 |

## 8. 实现记录与草案差异

1. **无需 build tag**：zalando/go-keyring v0.2.8 在 macOS 上 shell out 到 `/usr/bin/security`（纯 Go），CGO_ENABLED=0 全静态构建矩阵不受影响——草案中的 build-tag 方案作废。
2. **测试隔离用 `testing.Testing()`**：测试二进制在 auto 模式下解析为 file 模式（credstore.computeMode），从机制上保证任何包的测试都不会探测/触碰真实 keychain；keychain 语义由 credstore 包内注入 fake ops 的测试覆盖（`useFakeKeychain`）。
3. **Ref 即时派生而非构造期字段**：provider 测试大量使用结构体字面量构造（`&ApiKeyBase{authFile: ...}`），构造期绑定 Ref 会读到零值——改为方法内 `credstore.NewRef(path)` 即时派生，消除对构造路径的依赖。
4. **原子写收敛到一处**：credstore.atomicWriteFile 采用唯一临时名（防并发冲突），成功后顺带清理历史固定名 `<path>.tmp` 残留（accounts.Save 的旧契约有测试保护"保存后无 .tmp 残留"）。
5. **archtest 新增闭合契约**：`architecture_credential_store_contract_test.go` 保证 go-keyring/dbus/wincred 导入只出现在 internal/credstore；DAG 策略表登记 accounts/provider → credstore 依赖。
