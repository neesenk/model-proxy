# 客户端 Takeover 契约

## 适用范围

修改 `takeover` / `restore`、客户端配置路径、provider_id 或 baseURL 生成时必读。

实现归属：备份/改写/恢复与四个客户端的 rewrite 归 `internal/takeover`
（`RunTakeover` / `RunRestore` / `ListClients` / `BackupDir`）；`internal/cli`
（`commands.go` 的 `RunTakeover` / `RunRestore` / `takeoverFacts`）
只解析参数、加载 config 并用 `takeoverFacts` 注入 implicit routes 与 models.dev
元数据（catalog 加载与 source 标记留在 CLI 层）。

| 客户端 | baseURL 格式 | 关键差异 |
|---|---|---|
| opencode | `http://<proxy>/v1` | `@ai-sdk/anthropic` 拼接 `baseURL + /messages` |
| pi | `http://<proxy>` | pi 自行拼 `/v1/messages`，baseURL 不能再带 `/v1` |
| codex | 按 Responses API 客户端配置 | 不经过 Chat Completions 协议转换 |

`provider_id` 默认统一为 `model-proxy`，opencode、pi、codex 共用。备份位于：

```text
<configDir>/.model-proxy/<client>.bak
```

`takeover:` 配置块可省略；客户端路径和 provider_id 在配置加载器中有默认值，只在覆盖时配置。

- `takeover all` / `restore all` 遇到未安装客户端时跳过并继续；
- 单独指定客户端而文件不存在时返回硬错误；
- takeover 前必须备份，restore 后不得保留代理专属残片；
- URL 拼接回归需要覆盖 opencode 和 pi 的 `/v1` 差异。

