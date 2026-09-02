# 客户端 Takeover 契约

## 适用范围

修改 `takeover` / `restore`、客户端配置路径、provider_id 或 baseURL 生成时必读。

实现归属：备份/改写/恢复与五个客户端的 rewrite 归 `internal/takeover`
（`RunTakeover` / `RunRestore` / `ListClients` / `BackupDir`）；`internal/cli`
（`commands.go` 的 `RunTakeover` / `RunRestore` / `takeoverFacts`）
只解析参数、加载 config 并用 `takeoverFacts` 注入 implicit routes 与 models.dev
元数据（catalog 加载与 source 标记留在 CLI 层）。

| 客户端 | baseURL 格式 | 关键差异 |
|---|---|---|
| claude | `http://<proxy>`（不带 `/v1`） | `~/.claude/settings.json` 写 `env.ANTHROPIC_BASE_URL` + `env.ANTHROPIC_AUTH_TOKEN: "PROXY_MANAGED"` 占位；Claude Code 自拼 `/v1/messages` |
| opencode | `http://<proxy>/v1` | `@ai-sdk/anthropic` 拼接 `baseURL + /messages` |
| pi | `http://<proxy>` | pi 自行拼 `/v1/messages`，baseURL 不能再带 `/v1` |
| codex | 按 Responses API 客户端配置 | 不经过 Chat Completions 协议转换 |
| kimi | `http://<proxy>/v1` | 目标客户端是 MoonshotAI/kimi-cli（品牌名 "Kimi Code CLI"）`~/.kimi/config.toml`；`openai_legacy` 是 kimi-cli 对 OpenAI Chat Completions 协议的 provider 类型名（不是"旧版客户端"），kimi-cli 自拼 `/chat/completions`，所以 base_url 带 `/v1`。每个暴露模型写 `[models."<name>"]` 块（点号名必须加引号，否则 TOML 解析成嵌套表），按 kimi-cli 的 LLMModel schema 携带 `provider`/`model`/`max_context_size`；`max_context_size` 为必填，无元数据时回落到保守默认 200000（`routing.DefaultModelMetadata.Context`，takeover 直接引用） |

`provider_id` 默认统一为 `model-proxy`，opencode、pi、codex、kimi 共用。备份位于：

```text
<configDir>/.model-proxy/<client>.bak
```

`takeover:` 配置块可省略；客户端路径和 provider_id 在配置加载器中有默认值，只在覆盖时配置。

- `takeover all` / `restore all` 遇到未安装客户端时跳过并继续；
- 单独指定客户端而文件不存在时返回硬错误；
- takeover 前必须备份，restore 后不得保留代理专属残片；
- restore 成功即结束接管：删除 `<client>.bak` 与 `<client>.bak.meta` 标记，
  drift 检查（以 `.bak` 是否存在作为"已接管"标记）随后报告该客户端未接管，
  再次 takeover 会重新备份而不是沿用陈旧备份；恢复前的 sha256 完整性校验
  （fail-closed）不受影响；
- 所有客户端写回（JSON 改写、codex TOML 改写、restore）一律 temp+fsync+rename 原子写；
  **保留目标文件既有权限位**（这些文件常含真实 API key，硬编码 0644 会把 0600 放宽成全局可读），
  新建文件统一 0600；
- URL 拼接回归需要覆盖 opencode 和 pi 的 `/v1` 差异。

