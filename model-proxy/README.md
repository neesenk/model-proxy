# model-proxy

多 Provider LLM 代理 — 统一管理 Compass/codex/Zhipu 等上游后端，按协议（Anthropic/OpenAI）对外暴露，自动处理鉴权、模型映射、流式转发。

## 架构

```
┌─────────────┐     ┌───────────────────────────────────┐     ┌──────────────┐
│  客户端      │────▶│  model-proxy                      │────▶│  Provider    │
│  claude/     │     │  ┌─────────┐  ┌────────────────┐ │     │  compass     │
│  opencode/   │     │  │ routes  │→│ provider (auth) │ │     │  codex       │
│  codex/      │     │  │ (proto) │  │ RewriteRequest  │ │     │  zhipu       │
│  curl/SDK    │     │  └─────────┘  └────────────────┘ │     │  ...         │
└─────────────┘     └───────────────────────────────────┘     └──────────────┘
```

- **Provider 层**（`provider/` 包）：每个上游后端是一个 Provider 实现，封装鉴权、请求改写、登录、用量查询
- **Routes 层**：按协议（anthropic/openai）对外暴露模型，映射到 `provider/realModel`
- 凭据由 `login <provider>` 管理，存储在 `~/.model-proxy/<name>_<suffix>.json`，不落 config

## 构建

```bash
cd model-proxy
go build -o model-proxy .

# 交叉编译 Linux
GOOS=linux GOARCH=amd64 go build -o model-proxy-linux .
```

## 配置

`config.yaml`（`model-proxy config init` 生成模板）。查找顺序：`--config PATH` > `~/.model-proxy/config.yaml` > `./config.yaml`。

```yaml
listen: 127.0.0.1:15721
log_level: info

providers:
  compass:
    provider_id: compass
    baseURL: https://compass.llm.shopee.io/compass-api/v1
    cqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
    models:
      glm-5.2: {context: 1024000, output: 4096, modalities: {input: [text], output: [text]}}
  codex:
    provider_id: codex
    baseURL: https://chatgpt.com/backend-api/codex
    models:
      gpt-5.5: {context: 200000, output: 32768, modalities: {input: [text, image], output: [text]}}
  zhipu:
    provider_id: zhipu
    baseURL: https://open.bigmodel.cn/api/paas/v4
    usageURL: https://open.bigmodel.cn/api/paas/v4/models
    models:
      glm-5.2: {context: 128000, output: 4096, modalities: {input: [text], output: [text]}}

routes:
  anthropic:
    models:
      claude-opus-4-7: compass/glm-5.2
      glm-5.2: compass/glm-5.2
  openai:
    models:
      gpt-5.5: codex/gpt-5.5
      glm-5.2: compass/glm-5.2

takeover:
  claude_file: ~/.claude/settings.json
  opencode_file: ~/.config/opencode/opencode.json
  codex_file: ~/.codex/config.toml
  pi_file: ~/.pi/agent/models.json
  provider_id: model-proxy
```

## 用法

```bash
# 登录（凭据存储在 ~/.model-proxy/<name>_<suffix>.json）
model-proxy login compass          # Compass SSO 浏览器登录
model-proxy login codex            # codex OAuth device flow
model-proxy login zhipu            # 输入 Zhipu API key

# 启动代理
model-proxy serve                  # 前台
model-proxy serve daemon           # 后台（自动重启）
model-proxy serve stop             # 停止 daemon
model-proxy serve reload           # 热加载配置（SIGHUP）

# 查看用量
model-proxy usage compass          # 月度用量/余额
model-proxy usage codex            # credits/spend/rate limits
model-proxy usage zhipu            # 可用模型列表

# 登出
model-proxy logout compass         # 清除凭据文件

# 模型列表
model-proxy models                 # 所有 provider 的模型（从 config）
model-proxy models compass         # 单个 provider
model-proxy models refresh zhipu   # 从服务端刷新

# 接管客户端配置
model-proxy takeover opencode      # claude|opencode|codex|pi|all
model-proxy restore opencode

# 配置管理
model-proxy config init            # 生成模板
model-proxy config print           # 打印生效配置
model-proxy config check           # 校验配置
```

## Token 文件

凭据由 `login` 管理，按 provider name 派生路径，不落 config：

| Provider | Token 文件 | 内容 |
|---|---|---|
| compass | `~/.model-proxy/compass_oauth_auth.json` | SSO cookie + account data |
| codex | `~/.model-proxy/codex_oauth_auth.json` | OAuth access/refresh/id token |
| zhipu | `~/.model-proxy/zhipu_apikey.json` | API key |

多实例支持：同一 `provider_id` 可有多个不同 name（如 `zhipu-personal` / `zhipu-work`），各自独立凭据文件。

## 协议

代理按 URL 路径前缀路由，对外协议 = 转发协议（不做转换）：

| 协议 | 端点 | 转发到 |
|---|---|---|
| Anthropic | `POST /v1/messages` | provider 的 `/messages` |
| OpenAI | `POST /v1/responses`, `/v1/chat/completions` | provider 的同路径 |
| 模型列表 | `GET /v1/models` | 合并所有 routes 的模型 |

## 添加新 Provider

1. 建 `provider/xxx.go`，实现 Provider 接口（或 embed `ApiKeyBase`）
2. `init()` 里 `Register("xxx", constructor)`
3. config 加 `provider_id: xxx`

不改 proxy/login/logout/usage 的代码。

## Demo

```bash
model-proxy serve
python3 examples/demo.py --port 15721 "hello" glm-5.2
python3 examples/demo.py --port 15721 --protocol codex "hello" gpt-5.5
```

## 加载新 Provider 示例

config.yaml 加一个新 provider（如 DeepSeek）：

```yaml
providers:
  deepseek:
    provider_id: zhipu  # 复用 zhipu 实现（apikey + /models 校验）
    baseURL: https://api.deepseek.com/v1
    usageURL: https://api.deepseek.com/user/balance
    models:
      deepseek-chat: {context: 64000, output: 8192, modalities: {input: [text], output: [text]}}
```

```bash
model-proxy login deepseek        # 输入 DeepSeek API key
model-proxy usage deepseek        # 查余额
```
