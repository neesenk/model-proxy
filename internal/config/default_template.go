package config

// DefaultConfigYAML is the template written by `model-proxy config init`; matches the repo's config.yaml.
const DefaultConfigYAML = `# model-proxy config — standalone, portable to Linux.
# Paths support ~ expansion. env:ENV_VAR reads an environment variable.
# Order note (every provider's models: list): the wizard's suggested
# "model-proxy test" model is models[0] of the alphabetically-first selected
# provider — append new models at the tail instead of re-sorting, or the
# wizard hint silently shifts.

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
      - claude-fable-5-1
      - claude-opus-5
      - claude-sonnet-5
      - deepseek-v4-flash
      - deepseek-v4-pro
      - glm-5.3
      - glm-5.3-flash
      - gpt-5.6-luna
      - gpt-5.6-sol
      - gpt-5.6-terra
      - gpt-6-astra
      - gpt-6-luna
      - kimi-k3
      - claude-haiku-4-5
      - claude-opus-4-8
      - claude-opus-5-5
      - glm-5.2
      - gpt-5.4
      - gpt-5.5
      - gpt-6-sol
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
      - gpt-5.5
      - gpt-6-luna
      - gpt-6-sol
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
      - glm-4.5
      - glm-4.5-air
      - glm-4.6
      - glm-4.7
      - glm-5
      - glm-5-turbo
      - glm-5.1
      - glm-5.2
      - glm-5.3
      - glm-5.3-flash
      - glm-5.3-flashx
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
      - deepseek-v4-pro

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
    alias:
      deepseek-v4.1-flash: deepseek-flash
    models:
      - deepseek-v4-flash
      - deepseek-v4-pro
      - deepseek-v4.1-flash
      - doubao-embedding-vision
      - doubao-seed-2.1-pro
      - doubao-seed-2.1-turbo
      - doubao-seed-evolving
      - doubao-seedream-5.0-lite
      - glm-5.3
      - glm-5.3-flash
      - kimi-k2.7-code
      - kimi-k2.8-preview
      - kimi-k3
      - minimax-m3

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
    # kimi-for-coding: Moonshot serves "K2.8 Preview" under this STABLE id (the
    # upstream swaps the model behind the id — 'models refresh' prints the
    # upstream display names so the swap is visible); exposed as
    # kimi-k2.8-preview to match what the model actually is.
    alias:
      k3: kimi-k3
      kimi-for-coding: kimi-k2.8-preview
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

  # StepFun Step Plan (阶跃星辰订阅制 Coding Plan — Credit 月池, NOT the
  # pay-as-you-go 开放平台 channel). API key via 'login step-plan' (from
  # https://platform.stepfun.com/interface-key). Two protocol bases: openai_base_url
  # = .../step_plan/v1, anthropic_base_url = .../step_plan (no /v1; proxy keeps
  # /v1/messages). No usage_url — Credit usage is console-only (/v1/accounts reads
  # the SEPARATE pay-as-you-go balance); 'login step-plan' validates the key by
  # probing openai_base_url/models. stepaudio-* voice models use separate audio
  # endpoints and stay out of the chat models: list. Poolable (repeat 'login').
  step-plan:
    provider_id: step-plan
    billing: plan          # 订阅制 Credit 月池（月内消耗、月末清零），非按量余额
    openai_base_url: https://api.stepfun.com/step_plan/v1
    anthropic_base_url: https://api.stepfun.com/step_plan
    models:
      - step-5-preview
      - step-3.7-flash
      - step-3.5-flash
      - step-3.5-flash-2603
      - step-router-v1

  # Xiaomi MiMo (小米 MiMo 开放平台), PAY-AS-YOU-GO only. API key via
  # 'login mimo' (from https://platform.xiaomimimo.com — Console → API Keys).
  # One key serves both protocols; the two endpoints are per-protocol:
  # openai_base_url = the OpenAI-compatible base (/v1), anthropic_base_url =
  # the Anthropic-compatible base (no /v1; proxy keeps the client
  # /v1/messages path). No usage_url: MiMo's balance endpoint is gated on the
  # console's browser SSO cookie, NOT the API key (verified 2026-09), so the
  # key is validated against openai_base_url/models and quota stays
  # console-only (BillingUnknown + console URL, like qwen-plan/step-plan).
  # billing: pay-as-you-go is METADATA (the upstream's payment method) — it
  # gates nothing: quota polling follows the provider implementation (a
  # configured usage_url, else its own console-only snapshot). Scheduling
  # reads the measured snapshot first; only WITHOUT a measurement does this
  # label fill the tier (it never overrides one). Token Plan subscriptions
  # are NOT supported. Poolable (repeat 'login').
  mimo:
    openai_base_url: https://api.xiaomimimo.com/v1
    anthropic_base_url: https://api.xiaomimimo.com/anthropic
    provider_id: mimo
    priority: 3
    billing: pay-as-you-go
    models:
      - mimo-v2.6-flash
      - mimo-v2.6-pro
      - mimo-v2.6-pro-ultraspeed
      - mimo-v2.5-pro
      - mimo-v2.5

  # OpenRouter (https://openrouter.ai — aggregated model gateway, prepaid
  # credits). API key via 'login openrouter' (openrouter.ai/settings/keys).
  # One key (Authorization: Bearer) serves both protocols: openai_base_url =
  # /api/v1 (chat/completions, responses, models, key), anthropic_base_url =
  # /api (proxy keeps the client /v1/messages path — OpenRouter's Anthropic
  # Messages endpoint is /api/v1/messages). Model ids are VENDOR-PREFIXED
  # (anthropic/claude-*, openai/gpt-*, z-ai/glm-*, plus :free/:batch variants
  # — see openrouter.ai/models) and do NOT match models.dev's bare-name
  # metadata; declare capabilities: per model when the request-aware router
  # needs it. usage_url polls GET /key (day/week/month spend + optional
  # per-key credit cap); credit exhaustion surfaces as upstream 402.
  # Chat reasoning shape is the native reasoning:{effort} object. Poolable
  # (repeat 'login').
  openrouter:
    openai_base_url: https://openrouter.ai/api/v1
    anthropic_base_url: https://openrouter.ai/api
    provider_id: openrouter
    priority: 3
    billing: pay-as-you-go
    usage_url: https://openrouter.ai/api/v1/key
    models:
      - stealth/space-bunny-alpha

  # OpenCode Go (https://opencode.ai/go — the OpenCode team's $10/month
  # SUBSCRIPTION for curated open coding models; NOT the pay-as-you-go Zen
  # gateway at /zen). API key via 'login opencode-go' (opencode.ai/auth →
  # sign in → subscribe to Go → copy API key). One key serves both protocols,
  # but the legs read different auth headers (the proxy dual-writes Bearer +
  # x-api-key): openai_base_url = /zen/go/v1 (chat/completions, responses,
  # models — Bearer), anthropic_base_url = /zen/go (proxy keeps the client
  # /v1/messages path — x-api-key only, verified live). Usage limits are
  # per-model monthly dollar amounts ($15/$30/$60 tiers) split into windows
  # (5h=20%, weekly=50%, monthly=100%); console-only (no public usage API;
  # ExtraHeaders mirrors the client session header into x-opencode-session
  # for Go's routing/prompt-cache affinity). /models is public, so login
  # validation cannot reject bad keys (a wrong key surfaces at first request
  # as 401). Poolable (repeat 'login').
  opencode-go:
    openai_base_url: https://opencode.ai/zen/go/v1
    anthropic_base_url: https://opencode.ai/zen/go
    provider_id: opencode-go
    priority: 2
    billing: plan
    models:
      - kimi-k3
      - kimi-k2.7-code
      - glm-5.3
      - glm-5.3-flash
      - glm-5.2
      - deepseek-v4-pro
      - deepseek-v4-flash
      - deepseek-v4.1-flash
      - qwen3.7-max
      - qwen3.7-plus
      - minimax-m3
      - gpt-6-luna
      - gpt-5.6-luna
      - grok-4.7
      - mimo-v2.6-pro
      - longcat-2.0

  # TypeSafe System One decisions API (Jev): a decision model returning typed,
  # calibrated answers (choice/score/noul + probabilities) instead of text —
  # for routing/classification/scoring inside software, not chat. API key via
  # 'login typesafe' (console.typesafe.ai). Pure decisions-protocol provider:
  # decisions_base_url only (no chat protocols); clients call POST
  # /v1/decisions with {model, state, questions}; a chat-protocol client
  # hitting these models fails closed (no conversion exists). jev-latest
  # tracks the newest release; pin jev-1.13.0 (exact version) when tuned
  # thresholds must not drift. Note: /v1/models advertises the SHORT family
  # id (jev-1.13) which /systemone rejects — refresh rewrites it to jev-X.Y.0.
  typesafe:
    provider_id: typesafe
    billing: pay-as-you-go   # 按 token 计价（输入 $0.042/M、输出免费）
    decisions_base_url: https://api.typesafe.ai/v1
    models:
      - jev-latest
      - jev-1.13.0

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
#   stream_keepalive: 15s       # (default 15s; "0" disables) SSE comment heartbeat into client streams after this much upstream silence
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
