# zcode-wire-diff — 差分测试：真实 ZCode CLI vs model-proxy 指纹模拟

ZCode 已于 2026-09-21 开源（`github.com/zai-org/ZCode`，Apache-2.0）。本目录把
"我们的 zcode provider 发出的请求"和"真实 ZCode CLI 发出的请求"做**逐头差分**，
产出 golden fixture 供 `internal/provider/zcode_wire_test.go`
（`TestZCode_WireMatchesRealClientCapture`）在 CI 里比对：

- 确定性指纹头（UA / Referer / X-Title / X-ZCode-* / Release-Channel /
  session-type / anthropic-version）**精确比对**；
- 四个归因 UUID 头（request/session/query/trace）**形状比对**（v4 UUID）；
- 机器/环境派生头（platform / os-* / language / timezone）fixture 只查形状，
  代理侧的值与算法期望比对；
- **头集合 parity**：真实客户端发了而我们没发的、或我们多发的一切头，测试失败
  —— ZCode 换版本改线上形状时，fixture 过期即红，逼着重新捕获并 review。

## 一次性准备

```bash
git clone https://github.com/zai-org/ZCode.git /tmp/ZCode
cd /tmp/ZCode
pnpm install --filter @zcode/cli...   # 只装 CLI 及其依赖（避开 electron）
pnpm --filter @zcode/cli... build     # 产出 apps/zcode-cli/packages/cli/dist/zcode.cjs
```

依赖：node ≥ 24（实测 node 26 也可；注意 UA 的 `runtime/node.js/<major>` 段
会随本地 node 变化，fixture 生成时会记录，测试比对时会把该段归一化掉）、pnpm。

## 捕获 / 刷新 fixture

```bash
scripts/zcode-wire-diff/run.sh            # 捕获并打印与 checked-in fixture 的 diff
scripts/zcode-wire-diff/run.sh --write    # 捕获并覆盖 fixture
```

全程本地：CLI 通过 `provider_config.json`（personal provider，anthropic-messages，
baseUrl 指向 `127.0.0.1:8787`，dummy key）把模型请求发到本机 capture server，
capture server 回一 minimal Anthropic SSE 让 agent loop 跑完一轮。不触碰
BigModel、不需要真 key。`ZCODE_DATA_BASE_DIR` 隔离全部 app 状态。

## 2026-09-23 首次捕获结论（v3.14.0）

真实线上请求（28 个头）与 model-proxy 的指纹**逐头一致**，并修正了两处此前的
源码推断错误：

1. **UA 没有 `ai-sdk/anthropic/<ver>` 段**——`@ai-sdk/anthropic` 的
   `getHeaders()` 确实会追加，但 ZCode 在 per-request header 层
   （`runner-options.ts` `mergeRequestHeaders`）重新应用自己的
   `User-Agent: ZCode/<ver>` 把它覆盖，随后 provider-utils 才追加自己的后缀。
   真实 UA = `ZCode/3.14.0 ai-sdk/provider-utils/4.0.27 runtime/node.js/<major>`。
2. `X-Title: Z Code@cli` 得到实证（apikey coding-plan 路径的载体是独立 CLI；
   桌面 agent-server 才发 `Z Code@electron`）。

同时确认：版本 3.14.0、`X-ZCode-Agent: glm`、`x-zcode-session-type: main`、
五个归因 id 均为裸 v4 UUID、无 `X-Device-Mid`、language/timezone 走 Intl
（`en-US` / `Asia/Shanghai`）。
