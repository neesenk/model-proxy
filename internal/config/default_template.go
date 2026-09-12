package config

// DefaultConfigYAML is the template written by `model-proxy config init`; matches the repo's config.yaml.
const DefaultConfigYAML = `# model-proxy config — standalone, portable to Linux.
# Paths support ~ expansion. env:ENV_VAR reads an environment variable.

listen: 127.0.0.1:15721
log_level: info            # debug | info | warn | error
# Runtime log + pid file. Default when unset: /tmp/model-proxy.log (pid: /tmp/model-proxy.pid).
# Uncomment to override:
# log_file: /var/log/model-proxy/model-proxy.log

# Upstream proxy. Resolution chain per request: providers.<name>.proxy_url ->
# this global setting -> HTTPS_PROXY/HTTP_PROXY env (+ NO_PROXY) -> OS system
# proxy -> direct. Value: http/https/socks5 URL (userinfo allowed), or 'off'
# to force direct and stop the chain. Auto-detected sources (env NO_PROXY /
# system proxy) bypass loopback destinations; an explicitly configured proxy
# URL applies to loopback too (e.g. a local debugging proxy). The OS system
# proxy is detected once per process
# (restart after changing it); PAC/WPAD is not evaluated. Account-maintenance
# calls (usage/quota/login) follow this global chain only — a provider's
# proxy_url applies to its forwarded traffic (forward/fusion/shadow).
# proxy: http://127.0.0.1:7890

# credentials: file         # file (default) | keychain — where credential secret
#                           # VALUES live, for BOTH apikey pools and codex/aqp OAuth
#                           # stores. keychain moves api_key/access_key/secret_key
#                           # into the OS keychain (macOS Keychain / Windows Credential
#                           # Manager / Linux Secret Service) per entry — the pool file
#                           # then keeps metadata only — and stores each OAuth blob as a
#                           # whole keychain entry. Plaintext pools/OAuth files migrate
#                           # lazily on first read. Keychain mode is fail-closed: an
#                           # unreachable backend errors instead of silently reading
#                           # plaintext files. Switching back to file restores pool
#                           # secrets from the keychain (accounts missing their entry
#                           # keep metadata and need a re-login); OAuth blobs are not
#                           # restored — re-login after switching them back. env
#                           # MP_CRED_STORE (file|keychain|auto) remains an explicit
#                           # override on the OAuth side only; 'config check' flags it
#                           # when it diverges from this setting.

# Providers — upstream backends. Token files are auto-managed by login/logout
# at ~/.model-proxy/<provider_name>_<suffix>.json (no config needed).
# Optional per-provider upstream proxy: 'proxy_url: http://...' (or 'off')
# overrides the global proxy chain for that provider's forwarded traffic.
providers:
  aqp:
    openai_base_url: https://compass.llm.shopee.io/compass-api/v1
    anthropic_base_url: https://compass.llm.shopee.io/compass-api  # same base without /v1; proxy keeps the client /v1/messages path
    provider_id: aqp
    priority: 1   # provider-wide route priority (lower = tried first within a tier/quota band)
    aqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
    # peak_hours:                       # multi-segment, per-segment multiplier
    #   - {window: "14:00-18:00", multiplier: 2}
    models:
      - claude-opus-5
      - claude-sonnet-5
      - deepseek-v4-flash
      - deepseek-v4-pro
      - glm-5.3
      - glm-5.3-flash
      - gpt-5.6-luna
      - gpt-5.6-sol
      - gpt-5.6-terra
      - kimi-k3
  codex:
    openai_base_url: https://chatgpt.com/backend-api/codex
    # client_version: "0.144.1"   # optional; auto-detected from codex CLI if omitted
    provider_id: codex
    priority: 1
    # gpt-5.4 omitted: upstream rejects it for ChatGPT-account Codex access
    # ("The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account").
    models:
      - gpt-5.6-luna
      - gpt-5.6-sol
      - gpt-5.6-terra
      - gpt-6-astra
  # External provider (Zhipu BigModel — API key via login zhipu)
  # /models endpoint only lists 8 chat models; multimodal models exist but
  # must be added manually here (they use different API paths). Metadata
  # (context/output/modalities) is auto-sourced from models.dev at runtime.
  zhipu:
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    anthropic_base_url: https://open.bigmodel.cn/api/anthropic
    provider_id: zhipu
    priority: 2
    # capabilities:                    # optional: per-model capability declaration for request-aware
    #   glm-4v-plus: [image, tools]    # routing (values: image, tools). A model listed here is
                                       # authoritative — models.dev metadata is ignored for it.
    usage_url: https://open.bigmodel.cn/api/monitor/usage/quota/limit  # usage zhipu: 5h/weekly/monthly quota + token consumption
    models:
      - glm-5.3
      - glm-5.3-flash
  # DeepSeek (API key via 'login deepseek'). One key serves both protocols; the
  # two endpoints are per-protocol: openai_base_url = OpenAI base, anthropic_base_url =
  # Anthropic base (no /v1; proxy keeps the client /v1/messages path).
  # Listed models are auto-exposed as routes (see routes: below).
  # DeepSeek's anthropic endpoint also auto-maps claude-opus*→deepseek-v4-pro and
  # claude-sonnet*/claude-haiku*→deepseek-v4-flash server-side.
  deepseek:
    openai_base_url: https://api.deepseek.com
    anthropic_base_url: https://api.deepseek.com/anthropic
    provider_id: deepseek
    priority: 3
    billing: pay-as-you-go
    usage_url: https://api.deepseek.com/user/balance
    models:
      - deepseek-flash

  # Volcengine Ark (火山方舟, including 'Agent Plan'). API key via 'login volcengine'.
  # Two protocol bases: openai_base_url = Ark OpenAI base, anthropic_base_url =
  # Ark Anthropic-compatible base (no /v1; proxy keeps /v1/messages). usage_url is the
  # login-time key-validation endpoint (GET /models with Bearer; 401/403 rejects).
  volcengine:
    provider_id: volcengine
    priority: 3
    openai_base_url: https://ark.cn-beijing.volces.com/api/plan/v3
    anthropic_base_url: https://ark.cn-beijing.volces.com/api/plan
    usage_url: https://ark.cn-beijing.volces.com/api/plan/v3/models
    models:
      - deepseek-v4-flash
      - deepseek-v4-pro
      - doubao-seed-evolving
      - glm-5.3
      - glm-5.3-flash
      - kimi-k2.7-code
      - kimi-k3

  # Kimi Code (Moonshot membership coding plan). API key via 'login kimi-code'
  # (from https://www.kimi.com/code/console). Two protocol bases under one
  # gateway: openai_base_url = .../coding/v1, anthropic_base_url = .../coding
  # (no /v1; proxy keeps /v1/messages). usage_url is both the login-time
  # key-validation endpoint and the runtime quota poll target (GET /usages).
  # If you override openai_base_url, update usage_url to match (it does not
  # auto-derive at runtime). Poolable (repeat 'login').
  kimi-code:
    provider_id: kimi-code
    priority: 1
    openai_base_url: https://api.kimi.com/coding/v1
    anthropic_base_url: https://api.kimi.com/coding
    usage_url: https://api.kimi.com/coding/v1/usages
    # alias exposes k3 under the unified third-party name kimi-k3, so providers
    # naming the same model differently aggregate into one route. Requests are
    # rewritten to the upstream name "k3"; the response's model field is
    # normalized back to the called name, so clients only ever see "kimi-k3".
    alias:
      k3: kimi-k3
      kimi-for-coding: kimi-k2.7-code
    models:
      - k3
      - k3-256k
      - kimi-for-coding
      - kimi-for-coding-highspeed

  # Qianwen Token Plan 个人版 (千问 AI Token Plan personal edition). API key via
  # 'login qwen-plan' (from the Token Plan console; key format sk-sp-…). Two
  # protocol bases: openai_base_url = OpenAI-compatible base, anthropic_base_url
  # = Anthropic-compatible base (no /v1; proxy keeps /v1/messages). No usage_url
  # — personal-edition Credits usage (5h/7d windows) is console-only (no public
  # API); 'login qwen-plan' validates the key by probing openai_base_url/models.
  qwen-plan:
    provider_id: qwen-plan
    openai_base_url: https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
    anthropic_base_url: https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic
    models:
      - qwen3.8-max-preview
      - qwen3.7-max
      - qwen3.7-plus
      - qwen3.6-flash
      - glm-5.2
      - deepseek-v4-pro

# routes: AUTO-DERIVED — this block is intentionally omitted. Every provider
# model is exposed under its model name (or its provider-level alias) and all
# providers serving the same name aggregate into one route, ordered by provider
# priority (lower first), then billing tier and quota surplus; the proxy fails
# over to the next target on error. Either protocol may reach any exposed
# model; the protocol (from the request path) selects the upstream path + base
# URL. Set peak_hours on a provider to deprioritize it during its peak window.
# Inspect the derived table: 'model-proxy routes [model]' or the Web UI
# Config tab. An explicit routes: block is only needed for overrides — it
# replaces
# the derived route for that exposed name wholesale. A target is written as a
# compact "provider/model" string; the {provider, model, ...} map form is only
# needed when setting priority or protocol:
#   protocol: anthropic|openai — declare when the target requires cross-protocol
#     conversion, e.g. a Claude Code (anthropic) client hitting codex:
#     gpt-5.5: [{provider: codex, model: gpt-5.5, protocol: openai}]
#   {provider: fusion, model: <workflow>} — reference a fusion: workflow (see
#     the fusion block at the bottom of this file).
# routes:
#   glm-5.2: [zhipu/glm-5.2, aqp/glm-5.2]
#   gpt-5.5: [{provider: codex, model: gpt-5.5, protocol: openai}]

# Scheduling: failover health (circuit breaker, rate-limit skip) + sticky routing.
# Every field has a code default (owned by internal/config), so this entire
# block can be omitted - the values below are listed commented-out for reference.
# Durations are strings (e.g. "10m", "60s"). Uncomment a line to override.
# scheduling:
#   circuit_threshold: 3        # (default 3)  consecutive failover-eligible failures -> open circuit
#   circuit_cooldown: 10m       # (default 10m) circuit open duration, then half-open (1 probe)
#   rate_limit_backoff: 60s     # (default 60s) transient 429 with no reset hint/Retry-After: skip this long, then probe
#   quota_cooldown: 1h          # (default 1h)  429 classified quota-exhausted (balance/plan window) with no reset hint
#   model_lockout: 10m          # (default 10m) model-level failure (404 / model-denied / empty 200): lock (provider,model)
#   retry_wait: 10s             # (default 10s; "0" disables) all targets cooling down: wait ≤ this for the earliest expiry and retry (≤2×)
#   upstream_timeout: 1800s     # (default 1800s) per-upstream-request timeout
#   sticky_dwell: 10m           # (default 10m) min time on the chosen provider before re-evaluating
#   quota_poll_interval: 5m     # (default 5m)  background quota poll cadence
#   quota_switch_margin: 15     # (default 15)  switch provider if another's effective remaining beats current by >= this many pct points
#   quality_error_weight: 100   # (default 100) penalty per unit error-rate EWMA (2m half-life), subtracted from surplus; 0 disables
#   quality_ttft_weight: 20     # (default 20)  penalty per unit normalized TTFT EWMA (10s reference); 0 disables

# takeover: client targets come from templates — embedded presets
# (see 'model-proxy takeover list': claude/opencode/opencode-openai/opencode-responses/pi/pi-openai/
# pi-responses/codex/kimi/gemini-cli) or your own YAML in
# ~/.model-proxy/takeover-templates/<name>.yaml (same name overrides a preset).
# Template fields: file (client config path), format (json|toml|env),
# base_url (bare|v1), provider_id, proxy_url, json.set / toml.top_keys+sections /
# env.set with {{base_url}}/{{token}}/{{provider_id}} placeholders, and an
# optional models: block (per-exposed-model metadata shapes).

# Per-request access log: writes the full request + response body of each
# committed upstream call as one JSONL line to a rotating file under dir, for
# offline analysis (prompt replay, failure debugging, agent behavior). Disabled
# by default (zero hot-path overhead). Restart the daemon after changing these
# (reload is not enough). Uncomment to enable:
# request_log:
#   enabled: true                       # default false
#   dir: ~/.model-proxy/log/requests    # default
#   max_file_size: 1073741824           # 1G per file (rotate on size or day)
#   max_body_bytes: 5242880             # 5MB cap per body (truncates past it)
#   retention: 720h                     # 30d; delete rotated files older than this; 0 = forever
#   # Client session-id header allowlist (ordered; first non-empty wins); drives
#   # the request-log session_id + live /api/events session_id. Default:
#   # [x-claude-code-session-id, x-session-affinity, x-session-id, x-opencode-session]
#   # session_headers: [x-claude-code-session-id, x-session-affinity]

# Web admin UI + JSON API (/ui/ + /api/). Enabled by default. Without the
# optional auth block below, listen must stay loopback (enforced at startup);
# configuring both auth files allows a non-loopback listen. Uncomment to disable:
# web:
#   enabled: false
#   # S2 可选鉴权（默认全部不配 = 回环免鉴权历史行为）。
#   # 配置后管理面（/api /ui /metrics）要求 Bearer/x-api-key 匹配 admin 文件；
#   # 转发面（/v1/* /messages /v1/models）要求匹配 api_keys 文件（每行一个 key，
#   # 支持 # 注释；文件编辑后 ≤10s 生效，无需重启）。
#   # 非 loopback listen 时两者必配，否则 validate 拒绝（fail-closed）。
#   # auth:
#   #   admin_token_file: ~/.model-proxy/admin_token
#   #   api_keys_file: ~/.model-proxy/api_keys
#
# # Prometheus 指标（GET /metrics，文本 exposition；随 web.enabled，
# # 未配 admin auth 时仅回环可信）：model_proxy_{requests,failures,failovers,
# # rate_limited_429}_total 与 latency/ttft 毫秒累计，按 provider 标签聚合。

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

# Monthly budget alerts on the equivalent USD cost of observed token usage
# (same pricing path as /api/analytics; unpriced models contribute nothing).
# Checked once per minute against the current local month's accumulated cost.
# On crossing: a "budget" live event (SSE /api/events) plus an optional
# webhook POST of {scope, month, threshold_usd, actual_usd} (2 retries;
# failures are only logged). Alerts fire once per (scope, month, threshold)
# per process — a restart may re-alert. Enabling this block requires a
# daemon restart (startup-only, like request_log); threshold value changes
# apply on reload.
# budgets:
#   monthly_usd: 20                       # global monthly total threshold; 0/unset = off
#   providers:                            # optional per-provider overrides
#     zhipu: 5                            # replaces the global threshold for the zhipu scope
#   webhook_url: https://hooks.example.com/budget  # optional; unset = live event only

# Exact response cache: SHA-256(method+path+body) byte-exact match replays the
# recorded response (x-mp-cache: hit). Disabled by default. Shields retries and
# duplicate single-shot requests; multi-turn chats change the body every turn,
# so the hit rate there is ≈ 0 (prefix savings belong to upstream prompt caching).
# cache:
#   enabled: true                       # default false
#   ttl: 10m                            # default
#   max_entries: 1000                   # default
#   max_body_bytes: 262144              # default 256KiB; larger responses not cached

# Outbound secret guard (DLP-lite): scans the raw request body BEFORE
# forwarding upstream (request bodies only, never responses) for:
#   - high-confidence secret patterns (PEM private-key headers, AWS/OpenAI/
#     Anthropic/GitHub/Google tokens, plus the built-in rules table),
#   - the exact credential values the proxy itself manages (known_secrets;
#     pool API keys + OAuth tokens, matched in memory only — never written
#     to disk or logs),
#   - base64/hex/url-encoded forms of those patterns (decode),
#   - high-confidence sensitive paths (~/.ssh, ~/.aws/credentials, .env, ...).
# Hits are reported by pattern type name / path category only (live event +
# stats counter + audit log) — matched secret content is never logged.
# guard:
#   secrets: log      # log (default: allow + live event + counter) | redact
#                     # (forward with matches replaced by [REDACTED]) | block
#                     # (reject with 400) | off (no scan)
#   known_secrets: true   # default true; also match the proxy's own credentials
#   decode: true          # default true; catch base64/hex/url-encoded forms
#   paths: log            # log (default) | block | off — sensitive-path signal;
#                         # redact intentionally unsupported (rewriting a path
#                         # would corrupt legitimate coding work)
#   audit: true           # default true; persist security events to the audit log
#   session_scan: true    # default true; detect a known credential split into
#                         # fragments across requests of one session
#                         # (x-claude-code-session-id; bounded in-memory windows,
#                         # reported as known_secret_fragmented — under secrets=redact
#                         # a fragmented hit degrades to log: a cross-request secret
#                         # cannot be rewritten)
#   audit_path: ""        # optional absolute path; default ~/.model-proxy/log/security/security.log
#   extra_patterns:       # user secret formats (gitleaks extend-style)
#     - {name: myvendor_key, regex: '\bmv-[A-Za-z0-9]{32,}', literal: 'mv-'}
#                         # name: ^[a-z0-9_]{1,32}$; literal (optional) must be a
#                         # guaranteed substring of every regex match (pre-filter)
#   extra_paths:          # user sensitive paths, literal body match (~ kept literal)
#     - ~/.company/secrets
#   adjudicate:           # AI second opinion for pattern hits (default off).
#                         # When on, the matched span plus masked context is
#                         # sent (async, cached by content hash) to a model you
#                         # designate, which returns high (real leak: audit
#                         # record + optional session block) or low (benign
#                         # fixture/doc/example: suppressed). Queue overflow,
#                         # the per-request cap, model errors and timeouts fail
#                         # OPEN to the classic immediate record. Enabling opts
#                         # in to sending the matched snippet to that model.
#     enabled: false
#     model: ""           # required when enabled: exposed route name of the
#                         # judging model; its provider needs anthropic_base_url
#     block_session: true # high verdict blocks the session until unblocked
#                         # ('model-proxy guard unblock' or the WebUI)

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
