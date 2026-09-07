# agenttest — model-proxy 协议转换交互式测试工具

基于 pi 的 agent 接口([`@earendil-works/pi-agent-core`](https://www.npmjs.com/package/@earendil-works/pi-agent-core) + `pi-ai`)驱动**真实** agent loop(多轮会话 + 工具调用 + thinking + 流式),从三种 ingress 协议打进运行中的 model-proxy,端到端验证协议转换链路。比 `examples/demo.py` 的单发请求覆盖更真实:tool_use/tool_calls/function_call 的往返、多轮上下文、思考块都会实际经过转换器。

## 前置

1. `model-proxy serve` 在跑(默认 `127.0.0.1:15722`),且目标 provider 已 `login`
2. Node ≥ 20,首次 `npm install`(本目录)

## 用法

```bash
# 交互式 REPL(默认 anthropic 协议 + claude-haiku-4-5)
node agenttest.mjs

# 单发模式
node agenttest.mjs --protocol chat --model glm-5.2 --prompt "hello"

# 批量自检:3 协议 × 2 模型 × 4 场景,任一失败退出码 1
node agenttest.mjs --matrix --protocols anthropic,chat,responses \
  --models claude-haiku-4-5,gpt-5.5 --with-thinking

# 抓原始报文做跨协议 diff
node agenttest.mjs --protocol anthropic --prompt "hi" --dump-dir /tmp/wire
```

## codews — 真实代码实现工程全量回归

`codews.mjs` 复用同一套核心,给每个 provider/model 一个**真实的编码工程任务**:
agent 在独立工程副本里读 `SPEC.md`,用 `read_file`/`write_file` 实现
`src/strutil.js` 三个纯函数,并把 `input.txt` 逐行 slugify 写到 `output.txt`。
判定全部在 harness 侧真实执行(不信模型自评):

1. 在工程副本里跑 `node --test`(node:test 功能测试);
2. `output.txt` 与期望**逐字节**比对(真实读写往返);
3. agent 回复 DONE。

```bash
node codews.mjs                                   # 全 provider/model × 3 协议
node codews.mjs --providers zhipu --models glm-5.3
node codews.mjs --protocols anthropic --concurrency 4
node codews.mjs --clean                           # 仅清理并退出;配合 --providers/--models 清理后连跑
```

- 每组合一次多轮真实请求(读 SPEC → 实现 → 写回),`x-mp-force-provider` 钉死;
  报告落 `codews-last.json`,工程副本保留在 `.codews-work/` 供检查失败实现。
- 失败区分:功能测试未过(模型实现错误或转换问题)、缺 output.txt(未完成
  写回)、上游错误、任务超时。

## compare — 原生协议 vs 转换协议对比

`compare.mjs` 回答"同一模型,provider 原生协议和经 proxy 转换的协议在
实现上是否有差异":从 `model_caps.json` 读每个 (provider, model) 的原生
协议(verdict=yes 的腿),对 codews 首跑中「原生 PASS + 转换 FAIL」的可疑
组合做多轮复测(原生 ×2 对照 + 转换 ×3),消除单次采样的随机性:

```bash
node compare.mjs                          # 自动筛 codews-last.json 的可疑组合
node compare.mjs --providers zhipu --models glm-5.2    # 指定目标跑全部协议
node compare.mjs --repeats 3 --native-repeats 2
```

判定:转换 0 通过而原生有通过 → 强信号;失败分桶 impl-error(模型实现错)/
chain-broken(工具链断裂——没读 SPEC 没写文件,转换层强信号)/upstream/
timeout。强信号组合建议再用 dump 抓 wire 逐轮比对(`runCodeTask` 透传
`dumpDir`),区分「内容保真但模型行为分化」与「转换丢失/畸变」。

## sweep — config 全量回归

`sweep.mjs` 复用同一套核心(`lib/core.mjs`:pi agent loop、场景判定、
force-provider 钉死),对 **config.yaml 配置的全部 provider × model** 按
协议 × thinking 参数矩阵发起真实请求,汇总结果并分析失败原因:

```bash
node sweep.mjs                                    # 全 provider/model × 3 协议,ping 场景
node sweep.mjs --providers zhipu,volcengine       # provider 配置名子串过滤
node sweep.mjs --models glm-5.3,kimi-k3           # model id 子串过滤
node sweep.mjs --scenarios ping,tool,memory,thinking --thinking off,high
```

- 每行请求经 `x-mp-force-provider` 钉死到所属 provider,nonce 撞开响应
  cache,保证真实打到上游转换链路;thinking 非 off 档只对 models.dev 目录
  判定 `reasoning=true` 的模型执行(其余 SKIP;先 `models pull` 刷新目录)。
- 结果逐行打印,汇总表按 provider/model × 协议(+th)给出 pass 比;
  `sweep-last.json` 落全量明细(已 gitignore)。
- 失败分析:所有失败一律列出并按成因聚类(限流/鉴权/模型不存在/role
  拒绝/特性拒绝/图片不支持/超时/thinking 未返回/路由/响应转换);daemon
  探测 verdict(`model_caps.json`、`quota_state.json` 的 `wire_caps`)只作
  诊断上下文——runtime 会按 verdict 把请求转换到可用腿,客户端协议腿的
  no 解释不了任何失败;仅当错误显示请求确实打进了判 no 的腿时标注
  **选择违例**(那意味着协议选择有 bug)。退出码 0 仅当无失败。
- 规模提示:默认只跑 ping(约 3×模型数 次真实请求);全场景 × 双 thinking
  档约为 其 4 倍,先用 `--providers/--models` 缩小范围试跑。

三种 `--protocol` 对应代理的三个 ingress 路径:

| protocol | pi-ai api | 打到代理的路径 |
|---|---|---|
| `anthropic` | `anthropic-messages` | `POST /v1/messages` |
| `chat` | `openai-completions` | `POST /v1/chat/completions` |
| `responses` | `openai-responses` | `POST /v1/responses` |

## REPL 命令

```
/protocol <anthropic|chat|responses>   切换 ingress 协议(重置会话)
/model <id>                            切换模型/路由别名(重置会话)
/thinking <off|minimal|low|medium|high|xhigh|max>
/tools [on|off]                        开关工具(默认 on)
/raw [on|off]                          控制台 dump 出站 payload
/vision [prompt]                       发一条带四象限测试图的消息(默认问左上角颜色)
/state  /reset  /matrix  /help  /quit
```

其他输入直接作为 prompt 发送;工具调用自动执行并流式渲染(thinking 灰色、tool_call/result 青色、末尾 stop/usage/耗时)。Ctrl-C 中断当前请求,再按一次退出。

内置工具刻意覆盖不同参数形状:`get_time`(无参)、`calc`(字符串)、`read_file`(限 cwd 内、8KB 截断,制造多行 tool_result)、`make_test_image`(返回四象限 PNG,制造图片 tool_result)。

## matrix 场景

| 场景 | 断言 |
|---|---|
| `ping` | 回复含 pong(基础连通) |
| `tool` | 模型真实发起 calc 工具调用且最终答案含 42(tool_call → tool_result 往返) |
| `memory` | 两轮会话后仍记得 4242(多轮上下文转换) |
| `thinking` | 响应含 thinking block 且回复 pong(thinking 转换;需 `--with-thinking`) |
| `vision` | 用户消息带四象限 PNG,答出左上角为 red(image block 入站转换;需 `--with-vision`) |
| `vision_tool` | 模型调用 make_test_image 工具,从**图片工具结果**读出 red(tool_result 内 image content 转换;需 `--with-vision`) |

每个场景注入随机 `test-ref` nonce 撞开代理响应 cache,保证真实打到上游转换链路。模型不支持 thinking/vision 时对应场景会 FAIL —— 按需用 `--with-thinking`/`--with-vision` 开启,并用 `--force-provider <P>`(代理 `x-mp-force-provider` 头)把流量钉到具备能力的后端。

> 环境提示:若路由的 model_map 把某协议的模型别名改写到不支持图片的上游(如本仓 config 把 anthropic 协议的 claude-haiku-4-5 映射为 deepseek-v4-flash),该协议的 vision 场景会拿到上游 400 `Model do not support image input` —— 这是配置事实,不是转换缺陷;换协议或钉到多模态后端再验。

## 注意

- REPL/单发模式下重复发**完全相同**的 prompt 可能命中代理响应 cache(表现为 usage=0、秒回),这是代理的正常行为;排查转换问题时换措辞或加随机后缀。
- `--dump-dir` 写的是 pi-ai 出站 payload(客户端侧视角);代理入站/上游的原始字节录制用 `model-proxy wire record`。
- 凭据由代理持有,工具只发占位 key(`getApiKey: "agenttest-placeholder"`),不读任何真实凭据。

## e2e 整合 runner(`e2e.mjs`)

一个命令串起全链路:**真实 agent 项目 × 协议 × 模型 → matrix 批量自检 → 代理后端日志/内部数据分析 → 汇总退出码**。

```bash
node e2e.mjs                                        # 全量(vision 项目自动跳过)
node e2e.mjs --protocols anthropic --skip-matrix    # 快速冒烟
node e2e.mjs --with-vision --force-provider aqp     # 含图片用例,钉多模态后端
node e2e.mjs --projects pi-coding --with-thinking
```

内置项目:

| 项目 | 覆盖 |
|---|---|
| `pi-coding` | 真实 pi coding agent 完成 fizzbuzz 编码任务:write/bash/read 工具链 × 3 协议 |
| `pi-mcp` | pi + pi-mcp-adapter + 本地 MCP stdio server:MCP 工具(directTools 直挂)的调用与文件内容返回 × 3 协议 |
| `pi-vision` | read 工具读 PNG(image tool_result)+ CSV 文件处理;`requires:["vision"]`,需 `--with-vision`,manifest 声明只跑 chat/responses(见 matrix 节的环境提示),models.json 内钉 `x-mp-force-provider: aqp` |

项目 manifest 可声明 `requires`(如 `vision`,未加对应 flag 时 SKIP)和 `protocols`(与 CLI `--protocols` 取交集)。

后端分析内容:

- **运行日志**(解析顺序: `--log-file` > config `log_file` > `$TMPDIR/model-proxy.log`):按字节 offset 只分析窗口内新行,解析每请求行 `[proto=X provider=Y] POST /path model=A→B status=N`,按 proto×status 聚合(只统计本次 `--models` 的流量,其它流量单列计数),status≥400 判 warn;status≥500 的行先挂起,窗口内同协议后续出现 status<400 的行则按成功 failover 的瞬时失败降级为 warn(启发式:日志行无 request id,无法精确关联 attempt),窗口结束仍挂起的才判 FAIL,并扫描 panic/FAILED/error 可疑行。
- **内部运行数据**(`GET /api/status`):diff 窗口前后每 provider 的 `requests/failures/failovers/rate_limited_429` 计数器并全部展示;failures 是 attempt 级计数(成功 failover 也会 +1),增长只判 warn 并注明「可能含成功 failover 的瞬时失败」,429 增长判 warn。
- **request_log**(`GET /api/requests`):检测是否开启;未开启时给出开启提示(config `request_log` 段,需重启 daemon)。

判定:项目运行全过 + matrix 过 + 后端无异常 ⇒ `E2E PASS`(exit 0),否则 exit 1。

## 插入新的测试项目(`projects/<name>/`)

runner 自动发现 `projects/` 下所有含 `project.json` 的目录。新项目放一个目录即可,参照 `projects/pi-coding/`:

```jsonc
// project.json
{
  "name": "my-agent",
  "description": "...",
  "timeoutSec": 300,
  "taskFile": "task.md",          // 或内联 "task": "..."
  "requires": ["vision"],         // 可选:未加 --with-vision 时 SKIP
  "protocols": ["chat", "responses"], // 可选:只跑这些协议(与 CLI --protocols 取交集)
  "command": ["{{node}}", "...", "{{prompt}}"],   // 模板变量见下
  "env": { "SOME_DIR": "{{project}}/agent" },
  "assert": {
    "files": [{ "path": "out.txt", "match": "^ok$" }],  // 在 workspace 内断言
    "stdoutMatch": "DONE"                                  // 可选,正则
  }
}
```

- 每次运行使用独立 workspace:`projects/<name>/workspace/<protocol>--<model>/`,跑前自动清空(`--keep-workspace` 可保留),进程完整输出落 `workspace/.../run.log`。
- 模板变量:`{{node}} {{agenttest}} {{project}} {{workspace}} {{protocol}} {{model}} {{origin}} {{prompt}}`。
- 可选 `setup.mjs`(default export async `setup(ctx)`):每次运行前调用,用于渲染配置等,`ctx = { projectDir, workspace, protocol, model, origin, log }`。pi-coding 用它生成 pi 的 `models.json`(三协议 provider 指向代理)。
- 进程默认 cwd 是 workspace;命令应把代理地址经 `{{origin}}` 注入(注意 anthropic 类客户端 baseUrl 不带 `/v1`,openai 类带 `/v1`,参考 pi-coding/setup.mjs)。
