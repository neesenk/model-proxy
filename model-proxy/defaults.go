package main

// defaultConfigYAML is the template written by `model-proxy config init`; matches the repo's config.yaml.
const defaultConfigYAML = `# model-proxy config — standalone, portable to Linux.
# Paths support ~ expansion. env:ENV_VAR reads an environment variable.

listen: 127.0.0.1:15721
log_level: info            # debug | info | warn | error
# Runtime log + pid file. Default when unset: /tmp/model-proxy.log (pid: /tmp/model-proxy.pid).
# Uncomment to override:
# log_file: /var/log/model-proxy/model-proxy.log

# Providers — upstream backends. Token files are auto-managed by login/logout
# at ~/.model-proxy/<provider_name>_<suffix>.json (no config needed).
providers:
  aqp:
    openai_base_url: https://compass.llm.shopee.io/compass-api/v1
    anthropic_base_url: https://compass.llm.shopee.io/compass-api  # same base without /v1; proxy keeps the client /v1/messages path
    provider_id: aqp
    aqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
    # peak_hours:                       # multi-segment, per-segment multiplier
    #   - {window: "14:00-18:00", multiplier: 2}
    models:
      - glm-5.2
      - deepseek-v4-pro
      - deepseek-v4-flash
  codex:
    openai_base_url: https://chatgpt.com/backend-api/codex
    # client_version: "0.144.1"   # optional; auto-detected from codex CLI if omitted
    provider_id: codex
    models:
      - gpt-5.5
  # External provider (Zhipu BigModel — API key via login zhipu)
  # /models endpoint only lists 8 chat models; multimodal models exist but
  # must be added manually here (they use different API paths). Metadata
  # (context/output/modalities) is auto-sourced from models.dev at runtime.
  zhipu:
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    anthropic_base_url: https://open.bigmodel.cn/api/anthropic
    provider_id: zhipu
    usage_url: https://open.bigmodel.cn/api/monitor/usage/quota/limit  # usage zhipu: 5h/weekly/monthly quota + token consumption
    models:
      - glm-5.2
      - glm-5.1
      - glm-5
      - glm-5-turbo
      - glm-4.7
      - glm-4.6
      - glm-4.5
      - glm-4.5-air
      - glm-4v-plus
      - cogview-4-plus
  # DeepSeek (API key via 'login deepseek'). One key serves both protocols; the
  # two endpoints are per-protocol: openai_base_url = OpenAI base, anthropic_base_url =
  # Anthropic base (no /v1; proxy keeps the client /v1/messages path).
  # To expose models, add routes (e.g. anthropic/openai: deepseek-v4-pro: deepseek/deepseek-v4-pro).
  # DeepSeek's anthropic endpoint also auto-maps claude-opus*→deepseek-v4-pro and
  # claude-sonnet*/claude-haiku*→deepseek-v4-flash server-side.
  deepseek:
    openai_base_url: https://api.deepseek.com
    anthropic_base_url: https://api.deepseek.com/anthropic
    provider_id: deepseek
    billing: pay-as-you-go
    usage_url: https://api.deepseek.com/user/balance
    models:
      - deepseek-v4-pro
      - deepseek-v4-flash

  # Volcengine Ark (火山方舟, including 'Agent Plan'). API key via 'login volcengine'.
  # Two protocol bases: openai_base_url = Ark OpenAI base, anthropic_base_url =
  # Ark Anthropic-compatible base (no /v1; proxy keeps /v1/messages).
  volcengine:
    provider_id: volcengine
    openai_base_url: https://ark.cn-beijing.volces.com/api/plan/v3
    anthropic_base_url: https://ark.cn-beijing.volces.com/api/plan
    models:
      - doubao-seed-1-8-251228
      - doubao-seed-2-0-code
      - doubao-seed-1-6-251015
      - doubao-seed-2-0-lite-260428

# claude_mapping: anthropic-only. Translates a claude-* client model name to an
# exposed model name (looked up in routes below) before routing. If a called
# anthropic model isn't listed, the called name is used as the exposed name.
claude_mapping:
  claude-opus-4-7: glm-5.2
  claude-opus-4-8: glm-5.2
  opus: glm-5.2
  claude-sonnet-4-6: deepseek-v4-pro
  sonnet: deepseek-v4-pro
  claude-haiku-4-5: deepseek-v4-flash

# routes: exposed model name → ordered list of provider/model targets. The proxy
# schedules non-peak providers first, then by priority (lower wins), and fails
# over to the next on error. Either protocol may reach any exposed model; the
# protocol (from the request path) only selects the upstream path + base URL.
# Set peak_hours on a provider (see providers above) to deprioritize it during
# its peak window.
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
    - {provider: zhipu,   model: glm-5.2, priority: 2}
  deepseek-v4-pro:
    - {provider: aqp,  model: deepseek-v4-pro, priority: 1}
    - {provider: deepseek, model: deepseek-v4-pro, priority: 2}
  deepseek-v4-flash:
    - {provider: aqp,  model: deepseek-v4-flash, priority: 1}
    - {provider: deepseek, model: deepseek-v4-flash, priority: 2}
  gpt-5.5:
    - {provider: codex, model: gpt-5.5, priority: 1}

# Scheduling: failover health (circuit breaker, rate-limit skip) + sticky routing.
# Durations are strings (e.g. "10m", "60s"). Unset fields use the defaults shown.
scheduling:
  circuit_threshold: 3        # consecutive failover-eligible failures → open circuit
  circuit_cooldown: 10m       # circuit open duration, then half-open (1 probe)
  rate_limit_backoff: 60s     # 429 with no Retry-After: skip this long, then probe
  upstream_timeout: 30s       # per-upstream-request timeout
  sticky_dwell: 10m           # min time on the chosen provider before re-evaluating
  quota_poll_interval: 5m     # background quota poll cadence
  quota_switch_margin: 15     # switch provider if another's effective remaining beats current by ≥ this many pct points

takeover:
  # proxy_url defaults to http://<listen> when unset — leave it commented so
  # changing the listen port above is enough. Uncomment to override.
  # proxy_url: http://127.0.0.1:15721
  claude: ~/.claude/settings.json
  opencode: ~/.config/opencode/opencode.json
  codex: ~/.codex/config.toml
  pi: ~/.pi/agent/models.json
  # provider_id is the single identifier used by takeover for every agent that
  # takes one (opencode, pi, codex, future agents). claude doesn't use it.
  provider_id: model-proxy
`
