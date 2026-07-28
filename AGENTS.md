# AGENTS.md - model-proxy 仓库规则

本文件是全仓入口，只保留全局硬规则和文档路由。实现细节按需读取专题文档，不要把专题内容重新复制回本文件。`CLAUDE.md` 指向这里。

## 项目结构

```text
model-proxy/              Go daemon、CLI、核心测试
model-proxy/provider/     上游 Provider 实现
model-proxy/web_assets/   Web UI 静态资源
docs/                     架构、后端和 API 契约
```

文档目录分三类：`docs/architecture/`（现状架构契约）、`docs/decisions/`（容易被误判的有意行为）、`docs/engineering/`（流程与工程规范）；`docs/backend-contracts.md`、`docs/web-api.md` 是后端与 Web/API 的权威契约。

核心架构是 Provider 抽象 + route target 调度：同协议字节级透传，target 显式声明不同 `protocol:` 时才转换；provider 凭据不写入 config，由 login 管理。

## 必读文档路由

只加载当前任务相关文档：

| 修改范围 | 必读 |
|---|---|
| 全局架构、模块边界、跨领域重构 | `docs/architecture/overview.md` |
| `model-proxy/proxy.go`、`failclass.go`、cooldown | `model-proxy/AGENTS.md`、`docs/architecture/routing-and-failure.md` |
| `quota.go`、schedule、sticky、pin、持久化 | `model-proxy/AGENTS.md`、`docs/architecture/runtime-state.md` |
| `pool.go`、`resolve.go`、多账号 | `model-proxy/AGENTS.md`、`docs/architecture/provider-pools.md` |
| 凭据、login/logout、池文件 | `docs/architecture/provider-pools.md`、`docs/backend-contracts.md` |
| `model-proxy/provider/*` | `model-proxy/provider/AGENTS.md`、`docs/backend-contracts.md` |
| `model-proxy/internal/config/*`、`model-proxy/config_compat.go`、配置加载/校验/默认值 | `model-proxy/AGENTS.md`、`docs/architecture/overview.md`；字段同步另读 `docs/engineering/pitfalls.md` |
| `model-proxy/internal/protocol/*`、跨协议 route | `docs/architecture/protocol-conversion.md` |
| `request_routing.go`、implicit routes | `docs/architecture/request-routing.md` |
| `model-proxy/internal/catalog/*`、models.dev catalog、`models refresh` | `docs/architecture/request-routing.md`；pricing/analytics 另读 `docs/web-api.md` |
| `daemon.go`、supervisor、serve 生命周期 | `docs/engineering/pitfalls.md`（进程类陷阱）、`model-proxy/CLI.md`（serve 命令契约） |
| Fusion、Shadow、Cache、request log、live | `docs/architecture/fusion-shadow-cache.md` |
| `web.go`、`web_assets/*`、stats/API | `docs/web-api.md`；前端另读 `model-proxy/web_assets/AGENTS.md` |
| CLI 命令或显示 | `model-proxy/CLI.md` |
| 用户可见功能、配置或使用方式 | `model-proxy/README.md`、`model-proxy/config.yaml`，并按领域读取对应契约 |
| takeover/restore | `docs/client-takeover.md` |
| 测试、覆盖率、跨平台构建 | `docs/engineering/testing.md` |
| 容易误判为 bug 的行为 | `docs/decisions/intentional-behaviors.md` |
| 跨模块历史陷阱 | `docs/engineering/pitfalls.md` |

`docs/superpowers/`（plans/specs）是历史设计档案，**非权威现状**；与上述专题冲突时以专题为准。

## 仓库级工作规则

1. 先按上表加载当前任务所需文档，并遵守目标文件目录中最近的 `AGENTS.md`；domain 规则不在根文件重复定义。
2. 当前 worktree 可能有用户改动；保留无关修改，不擅自 commit、push、reset 或清理文件。
3. 请求、响应、cookie、token、API key 等敏感数据只在任务必要范围内读取，禁止写入日志、测试输出或文档示例。
4. 代码提交必须在同一提交中同步更新受影响的文档和示例：用户可见功能、配置、命令或操作流程更新 `model-proxy/README.md` 和必要的示例配置；内部契约更新对应专题文档；API、CLI、Provider 分别更新各自权威文档。各专题末尾的「修改要求/回归测试」是该专题的权威补充，不是可选项。
5. 一个事实只保留一个权威定义。README 面向用户说明“如何使用”，专题文档记录精确实现契约；其他位置只保留链接或简短导航。新知识的归属：实现契约 → 对应 `docs/architecture/` 专题；跨模块陷阱 → `docs/engineering/pitfalls.md`；容易被误判的反直觉行为 → `docs/decisions/intentional-behaviors.md`；只影响下一步修改者的实现细节 → 代码注释，不进任何文档。
6. 新增专题前先确认现有权威文档无法承载；目录级 `AGENTS.md` 只写该目录必须立即知道的局部约束。

## 验证基线

验证命令、覆盖率阈值、断言标准和跨平台矩阵统一见 `docs/engineering/testing.md`。所有修改至少执行 `git diff --check`；文档修改还需检查引用路径和唯一权威归属。
