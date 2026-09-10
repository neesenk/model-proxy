# AGENTS.md - Web UI 规则

修改本目录前读取 `../../../../docs/web-api.md`。API shape、状态字段和 mutation 语义以该文档为准。

## 边界

- UI 展示后端返回的状态，不在前端重新推导熔断、quota 或 schedule 结论。
- 所有 mutation 成功后刷新对应视图；错误必须显示后端 message，不能静默吞掉。
- SSE 订阅断开后清理 listener/timer，重连不得重复绑定。
- 日志、Raw YAML 和大 JSON 保持页面可滚动，不能通过压缩容器隐藏内容。
- 敏感 request/response body 只在 detail 视图按需加载，列表只使用 metadata。

## 契约变更

新增或修改 `/api/*` 字段时先更新 `docs/web-api.md`，再更新前端。前端不得依赖未文档化字段。

## 样式约定

- 设计基调见 `styles.css` 文件头注释：仪表盘美学、细线边框、等宽展示数据、单一琥珀强调色；颜色只编码语义（ok/warn/err），不做装饰。
- 颜色一律引用 `:root` 的 CSS 变量（`--surface-2`、`--border`、`--text`、`--muted`、`--accent` 等），禁止写死色值——dark 主题靠 `prefers-color-scheme` 重定义同一组变量成立，写死色值会在深色下破损。
- 表单控件必须套用既有的控件样式组之一：整宽表单用 `.field`（input/textarea/select），工具行内联控件用 `.req-input`，路由编辑行用 `.route-target-row`。**裸 `<input>`/`<select>` 会渲染成浏览器原生样式，与主题不符**——新增控件时先归组，不要写一次性 ID 样式。
- 控件视觉语言统一为：`var(--surface-2)` 底 + `1px solid var(--border)` + `var(--r-card)` 圆角 + 13px，focus 态 `outline: 2px solid color-mix(in srgb, var(--accent) 50%, transparent)`；新控件样式沿用这组值。
- 原生控件优先保留语义：能用 `accent-color`（checkbox/radio）就不用 `appearance: none` 全自定义；确需自定义时必须补全勾选标记、focus 环、disabled 态。

## 验证

```bash
node --check internal/web/assets/app.js internal/web/assets/pure.js
node --test internal/web/jstests/pure.test.mjs
go test ./internal/web -count=1
```

`pure.js` 只收零 DOM 依赖的纯函数（esc、格式化、YAML 高度计算等），
`app.js` 从 `./pure.js` import；新增纯逻辑先进 pure.js 并在
`jstests/pure.test.mjs` 加行为用例，locale/时区相关的渲染留在 app.js。

布局改动还需检查窄窗口、短窗口、长日志、长 YAML 和 hover/selection 状态。
