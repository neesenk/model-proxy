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
    # capabilities:                    # optional: per-model capability declaration for request-aware
    #   glm-4v-plus: [image, tools]    # routing (values: image, tools). A model listed here is
                                       # authoritative — models.dev metadata is ignored for it.
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
  # Ark Anthropic-compatible base (no /v1; proxy keeps /v1/messages). usage_url is the
  # login-time key-validation endpoint (GET /models with Bearer; 401/403 rejects).
  volcengine:
    provider_id: volcengine
    openai_base_url: https://ark.cn-beijing.volces.com/api/plan/v3
    anthropic_base_url: https://ark.cn-beijing.volces.com/api/plan
    usage_url: https://ark.cn-beijing.volces.com/api/plan/v3/models
    models:
      - doubao-seed-1-8-251228
      - doubao-seed-2-0-code
      - doubao-seed-1-6-251015
      - doubao-seed-2-0-lite-260428

  # Kimi Code (Moonshot membership coding plan). API key via 'login kimi-code'
  # (from https://www.kimi.com/code/console). Two protocol bases under one
  # gateway: openai_base_url = .../coding/v1, anthropic_base_url = .../coding
  # (no /v1; proxy keeps /v1/messages). usage_url is both the login-time
  # key-validation endpoint and the runtime quota poll target (GET /usages).
  # If you override openai_base_url, update usage_url to match (it does not
  # auto-derive at runtime). Poolable (repeat 'login').
  kimi-code:
    provider_id: kimi-code
    openai_base_url: https://api.kimi.com/coding/v1
    anthropic_base_url: https://api.kimi.com/coding
    usage_url: https://api.kimi.com/coding/v1/usages
    models:
      - kimi-for-coding
      - kimi-for-coding-highspeed
      - k3

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
# Per-target options:
#   protocol: anthropic|openai — declare when the target's protocol differs from
#     the client's to trigger protocol conversion (same protocol = byte-level
#     passthrough). E.g. let a Claude Code (anthropic) client hit codex:
#     gpt-5.5: [{provider: codex, model: gpt-5.5, priority: 1, protocol: openai}]
#   {provider: fusion, model: <workflow>} — reference a fusion: workflow (see
#     the fusion block at the bottom of this file).
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
# Every field has a code default (the accessors in config.go), so this entire
# block can be omitted - the values below are listed commented-out for reference.
# Durations are strings (e.g. "10m", "60s"). Uncomment a line to override.
# scheduling:
#   circuit_threshold: 3        # (default 3)  consecutive failover-eligible failures -> open circuit
#   circuit_cooldown: 10m       # (default 10m) circuit open duration, then half-open (1 probe)
#   rate_limit_backoff: 60s     # (default 60s) 429 with no Retry-After: skip this long, then probe
#   upstream_timeout: 30s       # (default 30s) per-upstream-request timeout
#   sticky_dwell: 10m           # (default 10m) min time on the chosen provider before re-evaluating
#   quota_poll_interval: 5m     # (default 5m)  background quota poll cadence
#   quota_switch_margin: 15     # (default 15)  switch provider if another's effective remaining beats current by >= this many pct points

takeover:
  # All takeover fields default to standard client config locations when unset
  # (claude/opencode/codex/pi paths + provider_id "model-proxy"), so you can
  # omit this entire block unless overriding one. proxy_url defaults to
  # http://<listen>. Uncomment any line to override.
  # proxy_url: http://127.0.0.1:15721
  # claude: ~/.claude/settings.json
  # opencode: ~/.config/opencode/opencode.json
  # codex: ~/.codex/config.toml
  # pi: ~/.pi/agent/models.json
  # provider_id is the single identifier used by takeover for every agent that
  # takes one (opencode, pi, codex, future agents). claude doesn't use it.
  # provider_id: model-proxy

# Per-request access log: writes the full request + response body of each
# committed upstream call as one JSONL line to a rotating file under dir, for
# offline analysis (prompt replay, failure debugging, agent behavior). Disabled
# by default (zero hot-path overhead). Restart the daemon after changing these
# (reload is not enough). Uncomment to enable:
# request_log:
#   enabled: true                       # default false
#   dir: ~/.model-proxy/requests        # default
#   max_file_size: 1073741824           # 1G per file (rotate on size or day)
#   max_body_bytes: 5242880             # 5MB cap per body (truncates past it)
#   retention: 720h                     # 30d; delete rotated files older than this; 0 = forever

# Web admin UI + JSON API (/ui/ + /api/). Enabled by default; no auth, so listen
# must stay loopback (enforced at startup). Uncomment to disable:
# web:
#   enabled: false

# Call statistics (SQLite; per provider×model×minute buckets incl. latency/TTFT
# and per-agent dims; powers the 'stats' CLI and Web UI Analytics/Requests
# pages). Defaults shown; omit the block to accept them.
# stats:
#   db_path: ~/.model-proxy/stats.db    # default
#   retention: 720h                     # default 30d; 0 = keep forever

# Equivalent-cost analytics: token usage × OpenRouter price catalog (cached).
# pricing:
#   enabled: true                       # default; false → cost shows n/a everywhere
#   ttl: 24h                            # default catalog cache TTL
#   # source_url: https://openrouter.ai/api/v1/models  # default; MP_PRICING_URL env also works
# Per-model price overrides (USD per 1M tokens) for models the catalog misses:
# prices:
#   glm-5.2: {input: 0.60, output: 2.20, cache_read: 0.11}

# Exact response cache: SHA-256(method+path+body) byte-exact match replays the
# recorded response (x-mp-cache: hit). Disabled by default. Shields retries and
# duplicate single-shot requests; multi-turn chats change the body every turn,
# so the hit rate there is ≈ 0 (prefix savings belong to upstream prompt caching).
# cache:
#   enabled: true                       # default false
#   ttl: 10m                            # default
#   max_entries: 1000                   # default
#   max_body_bytes: 262144              # default 256KiB; larger responses not cached

# Shadow evaluation: after the primary response commits, re-send the same request
# to a shadow backend, record-only — never affects the client, circuit breakers,
# stats or sticky routing. Compare with 'model-proxy shadow report' or the Web
# UI (GET /api/shadow-report).
# shadow_sample_rate: 1.0               # global sample rate (default 1.0; 0 disables all)
# shadow_max_concurrent: 4              # global in-flight cap (default 4; excess dropped)
# shadow:
#   glm-5.2: {provider: deepseek, model: deepseek-v4-pro}                # shadow one route…
#   deepseek-v4-pro: {provider: codex, model: gpt-5.5, protocol: openai} # …cross-protocol OK

# Multi-model orchestration (fusion): a route target {provider: fusion, model:
# <workflow>} fans the request out to the panel members in parallel (candidate
# answers), then the synthesizer model merges the candidates into the final
# (streamed) answer. Cost = N+1 upstream calls per request, latency ≈ candidate
# wait + synthesis first-token — use for hard-question routes only, never as a
# default route.
# fusion:
#   hard-coding:                        # workflow name (the route target's model field)
#     panel:                            # 2-4 candidate generators, called in parallel
#       - {provider: zhipu, model: glm-5.2}
#       - {provider: deepseek, model: deepseek-v4-pro}
#       - {provider: codex, model: gpt-5.5, protocol: openai}  # per-member protocol → auto-convert
#     synthesizer: {provider: zhipu, model: glm-5.2}  # merge model — use your strongest
#     min_panel: 2                      # default 2; fewer candidates → original request
#                                       # goes straight to the synthesizer (graceful degrade)
#     max_runs_per_day: 50              # optional: daily orchestration cap; over → degrade
#                                       # to a direct synthesizer call (degraded runs don't
#                                       # consume the budget)
#     first_turn_only: true             # optional: only orchestrate single-turn requests
#                                       # (no assistant message in the body)
#     judge: {provider: zhipu, model: glm-5.2}  # optional: pre-synthesis review pass
#                                       # (consensus/conflicts/omissions) injected into the
#                                       # synthesis prompt; judge failure never fails the run
#     # instruction: "..."              # optional: override the built-in synthesis
#                                       # instruction template (≤ 4000 chars)
# Observe: GET /api/fusion?workflow=<name> (per-workflow aggregates + recent runs);
# stats --provider fusion (requests = orchestrations, failovers = degrades).
# routes:
#   hard-question:
#     - {provider: fusion, model: hard-coding, priority: 1}
`
