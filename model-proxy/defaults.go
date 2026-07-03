package main

// defaultConfigYAML is the template written by `model-proxy config init`; matches the repo's config.yaml.
const defaultConfigYAML = `# model-proxy config — standalone from AIS Switch, portable to Linux.
# Paths support ~ expansion. env:ENV_VAR reads an environment variable.

listen: 127.0.0.1:15721
log_level: info            # debug | info | warn | error
# Runtime log + pid file. Default when unset: /tmp/model-proxy.log (pid: /tmp/model-proxy.pid).
# Uncomment to override:
# log_file: /var/log/model-proxy/model-proxy.log

# Gateway model-list cache + scheduled refresh (used by serve and the models command).
# Default cache file: next to sso_cookie_file (model-proxy-models.json).
# Default refresh interval: 1h. Uncomment to override:
# models_cache_file: ~/.model-proxy/model-proxy-models.json
# models_refresh_interval: 1h

auth:
  sso_cookie_file: ~/.model-proxy/google_oauth_auth.json
  cqp_mint_url: https://compass.llm.shopee.io/api/v1/cqp/ccswitch/api_key/get_or_generate
  codex_auth_file: ~/.model-proxy/codex_oauth_auth.json   # proxy own codex OAuth tokens (via login codex), NOT codex CLI auth.json
  # static_key: "..."

# Layer 1: providers — upstream backend definitions (baseURL + auth + models).
providers:
  compass:
    apiKey: PROXY_MANAGED
    baseURL: https://compass.llm.shopee.io/compass-api/v1
    auth: cqp
    models:
      glm-5.2:           {context: 1048576, output: 4096, modalities: {input: [text], output: [text]}}
      deepseek-v4-pro:   {context: 1048576, output: 4096, modalities: {input: [text], output: [text]}}
      deepseek-v4-flash: {context: 1048576, output: 4096, modalities: {input: [text], output: [text]}}
  codex:
    apiKey: PROXY_MANAGED
    baseURL: https://chatgpt.com/backend-api/codex
    auth: codex_oauth
    models:
      gpt-5.5: {context: 200000, output: 32768, modalities: {input: [text, image], output: [text]}}
  # External provider example (Zhipu BigModel — API key via login zhipu)
  zhipu:
    apiKey: PROXY_MANAGED
    baseURL: https://open.bigmodel.cn/api/paas/v4
    auth: apikey
    usageURL: https://open.bigmodel.cn/api/paas/v4/users/usage
    models:
      glm-4-plus: {context: 128000, output: 4096, modalities: {input: [text], output: [text]}}

# Layer 2: routes — by protocol. Exposed model → provider/realModel.
routes:
  anthropic:
    models:
      claude-opus-4-7: compass/glm-5.2
      claude-opus-4-8: compass/glm-5.2
      opus: compass/glm-5.2
      claude-sonnet-4-6: compass/deepseek-v4-pro
      sonnet: compass/deepseek-v4-pro
      claude-haiku-4-5: compass/deepseek-v4-flash
      glm-5.2: compass/glm-5.2
      deepseek-v4-pro: compass/deepseek-v4-pro
      deepseek-v4-flash: compass/deepseek-v4-flash
  openai:
    models:
      gpt-5.5: codex/gpt-5.5
      glm-5.2: compass/glm-5.2

takeover:
  # proxy_url defaults to http://<listen> when unset — leave it commented so
  # changing the listen port above is enough. Uncomment to override.
  # proxy_url: http://127.0.0.1:15721
  claude_file: ~/.claude/settings.json
  opencode_file: ~/.config/opencode/opencode.json
  codex_file: ~/.codex/config.toml
  pi_file: ~/.pi/agent/models.json
  # provider_id is the single identifier used by takeover for every agent that
  # takes one (opencode, pi, codex, future agents). claude doesn't use it.
  provider_id: model-proxy
`
