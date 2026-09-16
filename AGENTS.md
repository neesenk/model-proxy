# AGENTS.md - model-proxy 仓库规则

本文件只保留全仓硬规则和文档入口。精确实现契约在 `docs/` 专题文档；目标目录中更近的 `AGENTS.md` 优先。`CLAUDE.md` 指向这里。

## 定位与边界

model-proxy 是单进程模块化单体：根 `package main` 只负责进程入口，`internal/app` 是组合根，`internal/forward` 拥有请求转发管线（snapshot + HTTP 请求 → 响应 commit 或终态错误），`internal/cli*` 拥有命令和进程生命周期，其余 `internal/` package 各自拥有领域实现，`internal/web` 拥有 Web/API transport。完整 owner 与依赖图见 `docs/architecture/overview.md`。

## 红线

1. 每个请求只捕获一次 `runtimeSnapshot`；Fusion、Shadow 等异步分支只能使用该快照，禁止重读 reload-owned 状态。
2. 跨域锁顺序只有 `Proxy.mu → internal/runtime.Manager`；持锁期间不得回调上层 owner 或执行外部 I/O。后台任务必须有 owner、stop、wait 和 final flush。
3. nil runtime impl、请求/响应转换失败必须 fail-closed；凭据不得进入 config、代码、日志、测试输出或文档示例。
4. pin/force-provider 是硬选择，不得跨 route failover，并且必须绕过响应 cache。
5. 模块边界只进不出；直接仓库依赖和关键 owner/callsite 由 `internal/archtest` 的闭合契约保护，不能只放宽测试来迁就实现。
6. 实现、测试、文档和示例必须同步；用户可见变化更新 `README.md`/`config.yaml`，内部契约更新对应专题文档。

## 文档路由

只读取当前任务需要的文档：

| 范围 | 权威文档 |
|---|---|
| 全局架构、owner、配置边界 | `docs/architecture/overview.md`；配置陷阱另见 `docs/engineering/pitfalls.md` |
| forward、failover、cooldown、runtime、账号池 | `docs/architecture/routing-and-failure.md`、`docs/architecture/runtime-state.md`、`docs/architecture/provider-pools.md` |
| request routing、catalog、implicit routes | `docs/architecture/request-routing.md` |
| 协议转换、Provider、凭据/login | `docs/architecture/protocol-conversion.md`、`internal/provider/AGENTS.md`、`docs/backend-contracts.md` |
| Fusion、Shadow、Cache、request log、live | `docs/architecture/fusion-shadow-cache.md` |
| Web/API/UI | `docs/web-api.md`；前端硬规则另读 `internal/web/assets/AGENTS.md`，页面/组件实现细节见 `docs/frontend.md` |
| CLI、serve、daemon、进程生命周期 | `CLI.md`、`docs/architecture/overview.md`、`docs/engineering/pitfalls.md` |
| 测试、覆盖率、构建、架构 guard | `docs/engineering/testing.md` |
| takeover、反直觉行为 | `docs/client-takeover.md`、`docs/decisions/intentional-behaviors.md` |

`docs/superpowers/` 是历史设计档案，不是现状依据。

## 工作规则

- 先读取上表中的相关专题及目标目录最近的 `AGENTS.md`，不要把领域契约复制回根文件。
- 保留用户和其他任务的现有改动；未经要求不 commit、push、reset 或清理工作区。
- 重启运行中的 `serve` 用 `scripts/restart_serve.sh`（`--build` 构建先行再切换；SIGINT→等端口→拉起一次调用内原子完成），禁止把停/启拆到两次调用——中间的下线窗口会断连把它当 LLM 网关的工具链；陷阱背景见 `docs/engineering/pitfalls.md` 条目 24b，手动等价模板见 `CLI.md`「手动重启」。
- 请求体、响应体、cookie、token、API key 等敏感数据只在必要范围内读取，不得写入日志、测试输出或文档示例。
- 一个事实只保留一个权威定义：实现契约进对应 `docs/architecture/` 专题，跨模块陷阱进 `docs/engineering/pitfalls.md`，有意行为进 `docs/decisions/intentional-behaviors.md`。

## 测试核心约定

- 测试是行为与架构的可执行契约。bug 修复必须有回归用例；禁止靠删除/弱化断言、测试侧默认值、宽泛 fallback 或无条件 `Skip` 变绿。`Skip` 只用于明确的外部前置条件。
- 断言必须覆盖可观察语义：精确检查 status、终态、关键字段、身份和副作用，并优先解析结构化输出。源码/AST 测试只保护无法通过行为观察的 owner/wiring，且必须限定精确符号和调用点。
- race-clean 只是必要条件。并发和生命周期测试使用 channel、barrier、context、fake clock 或包内窄 seam，不用固定 `Sleep` 证明时序；不得遗留 goroutine、写真实 HOME、访问真实凭据/上游或向真实用户进程发信号。
- 纯行为测试跟随 owner package；跨模块 wiring 留在 composition/integration 测试。不要在 fixture 中复制生产实现或暴露可变全局测试 hook。coverage baseline/floor、race、跨平台构建和架构 guard 都是门禁，但不能替代语义断言。

## 验证

所有修改至少执行 `git diff --check`，代码修改执行 `go vet ./...`、`go test ./... -count=1`、`go test -race ./... -count=1`、`gofmt -l .` 和 `scripts/cover.sh`；平台代码追加 Linux/Windows 构建。精确追加矩阵见 `docs/engineering/testing.md`。修改本文件或 `docs/` 时执行该文档末尾的检查清单。
