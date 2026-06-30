package main

// defaultConfigYAML is the template written by `ais-switch-proxy config init`; matches the repo's config.yaml.
const defaultConfigYAML = `# ais-switch-proxy config — standalone from AIS Switch, portable to Linux.
# Paths support ~ expansion. env:ENV_VAR reads an environment variable.

listen: 127.0.0.1:15721
log_level: info            # debug | info | warn | error
# Runtime log + pid file. Default when unset: /tmp/ais-switch-proxy.log (pid: /tmp/ais-switch-proxy.pid).
# Uncomment to override:
# log_file: /var/log/ais-switch-proxy/ais-switch-proxy.log

# Gateway model-list cache + scheduled refresh (used by serve and the models command).
# Default cache file: next to sso_cookie_file (ais-switch-proxy-models.json).
# Default refresh interval: 1h. Uncomment to override:
# models_cache_file: ~/.ais-switch/ais-switch-proxy-models.json
# models_refresh_interval: 1h

auth:
  sso_cookie_file: ~/.ais-switch/google_oauth_auth.json
  cqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
  # static_key: "..."
  # gemini_api_key_env: GEMINI_API_KEY

routes:
  claude:
    path_prefixes: ["/v1/messages"]
    upstream: https://compass.llm.shopee.io/compass-api/v1
    upstream_path: ""
    auth: cqp
    model_map:
      claude-opus-4-7: glm-5.2
      claude-opus-4-8: glm-5.2
      opus: glm-5.2
      claude-sonnet-4-6: deepseek-v4-pro
      sonnet: deepseek-v4-pro
      claude-haiku-4-5: deepseek-v4-flash
  codex:
    path_prefixes: ["/v1/chat/completions", "/v1/responses"]
    upstream: https://compass.llm.shopee.io/compass-api/v1
    upstream_path: ""
    auth: cqp
    model_map:
      gpt-5.5: gpt-5.5
  gemini:
    path_prefixes: ["/v1beta"]
    upstream: https://generativelanguage.googleapis.com
    upstream_path: ""
    auth: gemini_key
    model_map: {}

takeover:
  proxy_url: http://127.0.0.1:15721
  claude_file: ~/.claude/settings.json
  opencode_file: ~/.config/opencode/opencode.json
  opencode_provider_id: anthropic
  codex_file: ~/.codex/config.toml
  pi_file: ~/.pi/agent/models.json
  pi_provider_name: ais-switch-proxy
  pi_models: ["claude-opus-4-7", "claude-sonnet-4-6", "claude-haiku-4-5"]
`
