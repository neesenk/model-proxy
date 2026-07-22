# Provider 多账号池与统一解析

## 适用范围

修改 `pool.go`、`resolve.go`、`buildProviders`、登录/登出、多账号 quota 或 Fusion/Shadow provider 解析时必读。

## 凭据文件

池化 provider 使用 `~/.model-proxy/<name>_apikeys.json`。旧单数 `<name>_apikey.json` 仅作为只读 fallback，包装成一条账号。

账号 ID：

- volcengine 优先使用 `access_key`，为空时回落到 hash；
- 其他 API key provider 使用 `sha256(api_key)[:16]`。

zhipu、deepseek、volcengine、kimi-code 支持池化；aqp、codex 不池化。login 按 id 去重，支持 label/replace，成功后触发热 reload。

## 构建期展开

多账号 provider 展开成虚拟 provider：

```text
<parent>#<accountID>
```

每个虚拟实例必须绑定自己的：

1. `ApiKeyBase.BoundAPIKey`，供 forward auth；
2. `cfg.Auth`，供 FetchModels；
3. Usage/Quota closure。

单账号池文件也必须 bind；不能因为只有一条记录而退回运行期文件读取。

volcengine 每账号包含 `{api_key, access_key, secret_key}`。绑定凭据存在时，`resolveAKSK` 必须排他使用该账号，不得回落到兄弟账号文件。

## Resolver

所有“配置 provider 名 → 可运行 provider 实例”的路径统一经过 resolver：

- `Expand`：父 provider 展开为全部虚拟账号，用于正常路由和 failover；
- `Pick`：为 Fusion/Shadow 等单目标调用选择一个 built 且 healthy 的虚拟账号。

resolver 负责 identity mapping 和健康预过滤，不负责占用 half-open slot。权威 gate 仍由调用方的 `takeHalfOpenSlot` 完成。

任何 resolver 失败都必须 fail-closed，禁止继续使用池化父名构造无认证请求。

## Session sticky

池内请求按 session sticky：

- 同一个 session 稳定映射到同一账号；
- 无 session 时用共享 spread counter 轮询；
- sticky 账号不可用时扫描兄弟账号 failover；
- 会话中不得仅因 quota surplus 边际变化迁移账号；
- session sticky 不落盘。

## 已知边界

volcengine `FetchModels` 尚未按账号完全绑定；池化时 `models refresh` 复用首个虚拟凭据。探测全失败时保留合并模型集，不写空。

## 回归测试

- 单账号池、多账号池和 legacy fallback。
- BoundAPIKey 不读取虚拟名字对应的不存在文件。
- session 稳定、不同 session 分流、账号冷却 failover。
- Fusion panel/synthesizer 和 Shadow 的 pooled parent 解析。
- resolver !ok 时没有任何上游请求。

