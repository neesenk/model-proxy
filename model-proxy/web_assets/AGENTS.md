# AGENTS.md - Web UI 规则

修改本目录前读取 `../../docs/web-api.md`。API shape、状态字段和 mutation 语义以该文档为准。

## 边界

- UI 展示后端返回的状态，不在前端重新推导熔断、quota 或 schedule 结论。
- 所有 mutation 成功后刷新对应视图；错误必须显示后端 message，不能静默吞掉。
- SSE 订阅断开后清理 listener/timer，重连不得重复绑定。
- 日志、Raw YAML 和大 JSON 保持页面可滚动，不能通过压缩容器隐藏内容。
- 敏感 request/response body 只在 detail 视图按需加载，列表只使用 metadata。

## 契约变更

新增或修改 `/api/*` 字段时先更新 `docs/web-api.md`，再更新前端。前端不得依赖未文档化字段。

## 验证

```bash
node --check app.js
cd ..
go test ./... -count=1
```

布局改动还需检查窄窗口、短窗口、长日志、长 YAML 和 hover/selection 状态。

