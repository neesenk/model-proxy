# S3 设计草案：Provider preset 目录与 `add` 向导

> 状态：**已实现 CLI 核心（2026-08）**。`internal/cli/presets` 落地：`presets list` + `add <preset>`（TTY 编号选择 / `--api-key-env` 脚本化 / 歧义门 fail-closed / 登录后热重载）。与草案的差异见 §8；Web UI 向导页（P2）与社区 preset 贡献路径（P3）未做。实现契约落地时应同步 `docs/architecture/request-routing.md`（隐式路由交互）与 `CLI.md`。
> 关联调研：`docs/research/similar-projects.md` §5-S3。

## 1. 现状与问题

- 接入新 provider = 手写 YAML：`openai_base_url`、`models:`、`usage_url`、routes……知识散在 README 示例和 backend-contracts 里，认知负担是上手流失主因。
- 已有资产可直接复用：`config init` 的 TTY 向导模式（`internal/cli/config_init.go`）、`cli/login` 编排、`configedit`（保结构 YAML node 编辑）、probe/test 测活、takeover。

## 2. 目标 / 非目标

**目标**：常见 provider 从零可用 ≤ 一条命令 + 一次登录；preset 数据结构支持社区扩展。
**非目标**：不做远程 preset 市场/网络拉取（首版内置编译期目录）；不改隐式路由语义（显式 routes 仍优先）。

## 3. 方案

### 3.1 Preset 数据模型：新增 `internal/presets`

```go
type Preset struct {
    ID          string   // "zhipu"、"deepseek"、"kimi-code"…（= provider_id）
    DisplayName string
    LoginKind   string   // "apikey" | "oauth-device" | "sso" | "token-plan" | "volcengine-aksk"
    BaseURL     string            // openai_base_url
    AnthropicURL string           // 可选 anthropic_base_url
    UsageURL    string            // 可选 usage_url
    DefaultModels []string        // 该家当前主力模型名（公开信息）
    ProtocolHint string           // 可选默认 protocol:
    DocsURL     string
    Notes       string            // 计费方式/配额特点一句话
}
```

- 内置目录为 Go 表（或 embed YAML），**只含公开 endpoint 与模型名，绝不内置任何凭据或 mint URL 私有值**（aqp 这类内网 SSO provider 不进公共目录，仍走手配）。
- `model-proxy presets list` 列出全部；`--json` 输出供 Web UI 使用。

### 3.2 `model-proxy add [presetID]` 命令流程

```
add zhipu
 ├─ 1. 解析 preset（无参数则 TTY 列表选择）
 ├─ 2. login <id>            ← 完全复用 cli/login 现有编排（--label/--replace 透传）
 ├─ 3. 生成 providers 块      ← 经 internal/configedit 写入（保注释、保用户其他配置）
 │     models 取 preset.DefaultModels ∩ login 后 FetchModels 实际可见列表
 ├─ 4. 路由建议              ← 单 provider：靠隐式路由即可，提示无需显式 route；
 │     多 provider 同模型冲突：展示歧义告警文案并给出建议显式 routes 片段（询问是否写入）
 ├─ 5. 测活                  ← 复用 test/probe 路径，失败给出结构化错误（401→重新 login 等）
 └─ 6. 询问 takeover         ← 复用 takeover 包；claude_mapping 别名建议一并展示
```

- 非 TTY（脚本化）：`add --preset zhipu --api-key-env ZHIPU_KEY --yes`，跳过交互，第 4 步歧义时拒绝写入并退出非零（fail-closed，不猜）。
- 幂等性：重复 add 同一 preset = 更新 models 列表 + 重登，不产生重复块（configedit 按 key merge）。
- reload 串联：若 daemon 在跑，写完配置后提示/执行 `serve reload`（沿用现有 reload 生命周期约定）。

### 3.3 Web UI 向导页

- 后端复用同一编排核心：把 1–6 步抽成 `internal/cli/add` 下的可编程 step 函数（TTY 只是其中一个 driver），Web 通过 `internal/appapi` 新增端点驱动同一步骤（SSE 进度复用 events 通道）。
- 前端遵循 `internal/web/assets/AGENTS.md`。

### 3.4 与 config init 向导的关系

`config init` 保持「首次最小配置」定位；`add` 是运行期增量接入入口。config init 第 3 步（勾选 provider）内部改调同一 preset 表，消除两份 provider 知识。

## 4. 红线核对

- 凭据只在 login 步骤经 provider 层落盘（S1 落地后经 credstore），向导代码不接触凭据内容。
- pin/force-provider、cache 绕过等路由语义不受影响；add 不修改既有 routes。
- 隐式路由歧义告警逻辑复用现有实现，不另造一份判定。

## 5. 测试计划

- preset 表完整性测试：每个 preset 必须能通过 config validate（生成临时 config 校验）；LoginKind 必须有对应已注册 login 流程。
- add 流程测试：fake login + fake probe，断言生成的 YAML 节点、幂等重复执行、歧义拒写、非 TTY flag 组合校验。
- configedit 合并测试：向已有注释 config 中 add 不丢注释/不重排 key。

## 6. 分阶段落地

1. **P1**：presets 包 + `presets list` + `add`（TTY + 非 TTY），首批 preset：zhipu/deepseek/kimi-code/qwen-plan/volcengine/openrouter。
2. **P2**：Web UI 向导页；config init 收敛到同一表。
3. **P3**：社区贡献路径（PR 加 preset 的模板与 CI 校验），README「支持的 Provider」表由 preset 表自动生成。

## 7. 风险（P2/P3 待办时需处理）

| 风险 | 缓解 |
|---|---|
| 上游改 endpoint/model 导致 preset 过期 | preset 带 DocsURL + doctor 对 base_url 做 probe 校验并报「preset 可能过期」 |
| DefaultModels 与实际账号可见模型不一致 | 以 login 后 FetchModels 结果取交集，差异项提示而非静默写入 |
| 两处向导（config init / add）漂移 | config init 收敛到同一派生逻辑；archtest 限制 provider 知识不得进入 cli 包顶层 |

## 8. 实现记录与草案差异

1. **无独立 presets 数据目录**：目录直接派生自 `configdomain.DefaultConfigYAML`（模板即唯一权威），过滤已注册 provider_id 并排除内网 SSO 的 aqp——草案 §3.1 的独立 Go 表/YAML 方案作废，避免第二份 provider 知识。
2. **登录分派提取为 `login.RunProviderLogin`**：`CmdLogin` 与 `add` 共用同一 provider 分派（aqp/codex/zcode/volcengine/apikey）；codex 流程拆出可返回错误的 `runCodexLoginFlow`（原 `CmdCodexLogin` 保留 AST 守卫要求的路径解析模式）。
3. **命令注册遵循 ProcessCommand 约定**：archtest 要求 app.Commands 每项都是 `ProcessCommand(handler)`，故经 `RunAdd`/`RunPresets` 适配（exit code → 进程退出码）；相关穷举契约测试同步更新。
4. **歧义门语义**：合并后对「其他已配置 provider 同模型且无显式 route」检测；非 TTY 无 `--yes` 时在登录前拒绝（fail-closed，不产生任何凭据副作用），TTY 询问 y/N。
