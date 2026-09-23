package cli

import (
	"fmt"
	"io"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"sort"
)

const Usage = `model-proxy — standalone portable multi-provider LLM proxy

Usage: model-proxy <COMMAND> [SUBCOMMAND] [OPTIONS]

Commands:
  serve                Start the proxy server
  serve daemon         Run in background (auto-restart on crash)
  serve stop           Stop a running daemon
  serve reload         Hot-reload config (SIGHUP the running daemon)
  serve status         Show running daemon status (providers/schedule/quota/tokens)
  takeover <client>    Rewrite client config to point at the proxy
  restore <client>     Restore client config from backup
  login <provider>     Login to a provider (aqp | codex | zcode | zhipu | deepseek | kimi-code | qwen-plan | step-plan | volcengine | typesafe)
  add <preset>         Add a provider preset to config.yaml and log in
  presets list         List built-in provider presets
  logout <provider>    Clear provider credentials
  usage [provider]     Show usage / credits (no provider = all configured)
  models               List models from all providers (from config)
  models <provider>    List models for one provider
  models refresh <provider>  Fetch live model list from a provider
  models pull         Force-refresh the models.dev metadata cache
  config init          Generate a config.yaml template
  config print         Print the effective config
  config check         Validate config and print a summary
  routes [model]       Show the derived route table (all exposed models, or one model's targets)
  schedule             Show current per-model provider (queries the running daemon)
  pin <route> <prov>   Temporarily force a route onto one provider (no failover)
  unpin <route>        Remove a pin
  unfreeze [provider]  Clear frozen provider state (circuit/rate-limit/model locks)
  freeze <provider>    Manually freeze a provider (exclude from scheduling until unfreeze)
  stats                Show per-(provider, model) call statistics (queries the daemon)
  cache                Show exact-response cache hit stats (queries the daemon)
  doctor               Offline scheduling diagnostic (config only, no daemon)
  audit                Show the security audit log (offline, no daemon)
  guard                List/unblock adjudicated session blocks; manage content overrides
  test <model>         End-to-end probe of a model's route targets (real upstream calls)
  mcp [list|test]      Manage MCP gateway servers (mcp: config; test runs the handshake)
  replay <id> --to P   Re-answer a logged request with a different backend
  shadow report       Shadow-evaluation aggregation (primary vs shadow compare)
  wire record <prov>  Record raw upstream SSE streams into testdata/wire/
  help                 Print this message

Options:
  --config <PATH>
          Config file path
          Lookup order: explicit path > ~/.model-proxy/config.yaml > ./config.yaml

  --log-file <PATH>
          Log file path (for 'serve': overrides config log_file)

  -h, --help
          Print this message (or per-command help: <command> -h)
`

// cmdHelp returns the short help for a command, or "" if unknown.
var Help = map[string]string{
	"serve": `serve [subcommand] [--config PATH] [--log-file PATH]

  Start the proxy server.

Subcommands:
  (none)    Run in foreground.
  daemon    Run in background (auto-restart on crash).
  stop      Stop a running daemon.
  reload    Hot-reload config (sends SIGHUP to the running daemon).
  status    Show running daemon status (providers, schedule, quota, tokens).

Options:
  --config PATH     Config file (default lookup: ~/.model-proxy/config.yaml > ./config.yaml)
  --log-file PATH   Log file path (overrides config log_file)`,

	"takeover": `takeover <client> [--config PATH] [--mode unified|split|anthropic|openai|responses]

  Rewrite a client's config to point at the proxy (backs up the original).

  <client> is a template name or a client family. Single-protocol agents
  (claude, codex, ...) are written in the one protocol they support. For
  agents supporting several protocols (families with template variants: pi,
  opencode), --mode decides how the config is written:
    unified (default) — ONE entry, the protocol the route providers serve
                        natively for the most models; the rest converts.
    split             — one entry per natively-spoken protocol, models
                        partitioned among them (every model passes through).
    <protocol>        — unified pinned to that protocol where the family has
                        a variant for it; families without one fall back to
                        the unified auto-selection.
  On a TTY with routes spanning several native protocols, takeover asks.
  An exact template name (pi-openai) always pins that variant.

  takeover list shows the available client templates (embedded presets +
  user overrides in ~/.model-proxy/takeover-templates/<name>.yaml), marking
  the auto-selected variant of each family with *.

Clients:
  claude | opencode | pi | codex | kimi | gemini-cli | all
  (variants: opencode-openai | opencode-responses | pi-openai | pi-responses)`,

	"restore": `restore <client> [--config PATH]

  Restore a client's config from the backup created by takeover.
  A client family restores every taken-over variant of that family.

Clients:
  same template names as takeover (see: takeover list) | all`,

	"login": `login <provider> [--config PATH] [--label NAME] [--replace]
                 [--from-env VAR [--from-env-ak VAR --from-env-sk VAR]]
                 [--from-codex]

  Authenticate with a provider. Interactive by default; the import shortcuts
  below avoid pasting secrets into the terminal.

Import flags:
  --from-codex        codex only: import the official codex CLI login from
                      ~/.codex/auth.json (OAuth tokens; the proxy refreshes the
                      access_token on demand while the refresh_token is valid)
  --from-env VAR      apikey-class providers: read the API key from env var VAR
  --from-env-ak VAR   volcengine only: read the Access Key ID from VAR (optional)
  --from-env-sk VAR   volcengine only: read the Secret Access Key from VAR (optional)

  --label/--replace work the same as in interactive login. Imported values are
  never echoed or written to logs; success output shows only a masked account
  id. Missing files/variables and malformed credential files are errors.`,

	"add": `add <preset> [--config PATH] [--label NAME] [--replace]
                  [--api-key-env ENV] [--yes]

  Add a built-in provider preset to config.yaml and log in to it in one step.
  The preset supplies the provider block (endpoints, default models) from the
  built-in template; login then stores credentials. A running daemon is
  hot-reloaded afterwards.

Flags:
  --api-key-env ENV   Read the API key from this environment variable (non-interactive)
  --label NAME        Label the logged-in account
  --replace           Replace an already-logged-in account's key without prompting
  --yes               Skip the interactive ambiguity confirmation (scripts)

See also: presets list`,

	"presets": `presets list [--config PATH]

  List the built-in provider presets available to ` + "`model-proxy add`" + `.`,

	"logout": `logout <provider> [--config PATH]

  Clear stored credentials for a provider.`,

	"usage": `usage [provider] [--config PATH]

  Show usage / credits for a provider. With no provider, shows all
  configured providers.`,

	"models": `models [subcommand] [provider] [--config PATH]

  List or refresh models.

Usage:
  models              List all models from all providers (from config).
  models <provider>   List models for one provider.
  models refresh <provider>  Fetch live model list from a provider's server.
  models pull         Force-refresh the models.dev metadata cache.`,

	"config": `config <subcommand> [--config PATH]

  Manage config files.

Subcommands:
  init      Generate a config.yaml template in the current directory.
  print     Print the effective config.
  check     Validate the config and print a summary.`,

	"routes": `routes [model] [--config PATH]

  Show the derived route table (offline, no daemon): every provider model is
  exposed under its model name (or its provider's alias:) and aggregated per
  exposed name with the provider's priority; explicit routes: entries
  override the derived route for that name.

Usage:
  routes            List all exposed models with their ordered targets.
  routes <model>    One model's targets in detail (provider, upstream model,
                    priority, billing tier; alias/explicit origin).`,

	"schedule": `schedule [--config PATH]

  Query the running daemon's /debug/schedule endpoint and print which provider
  each model is currently scheduled to (first-choice + ordered list + sticky
  state). The daemon (` + "`model-proxy serve`" + `) must be running.`,

	"cache": `cache [--json] [--config PATH]

  Show the exact-response cache counters (GET /api/status): entries, hits,
  misses and the hit rate over all recorded lookups. A disabled cache prints
  a hint instead. The daemon (` + "`model-proxy serve`" + ` + web.enabled) must
  be running.

Flags:
  --json   Dump the raw {enabled,hits,misses,entries} object.`,

	"pin": `pin [<route> <provider>] [--ttl DUR] [--config PATH]

  Temporarily force a route onto ONE provider without editing config.yaml (a
  runtime hot-switch). The pinned route skips failover — only the pinned
  provider is tried, until the TTL expires, you ` + "`unpin`" + `, or the daemon
  restarts (pins are in-memory). Pinning a pooled provider by its parent name
  (e.g. "zhipu") pins every zhipu#<id> virtual. Use /debug/schedule (the
  ` + "`schedule`" + ` command) to see the active pin. With no route/provider
  args, lists active pins.

  --ttl DUR   Go duration (1h, 30m, 2h45m); omit for no expiry.`,

	"unpin": `unpin <route> [--config PATH]

  Remove a manual pin (see ` + "`pin`" + `).`,

	"replay": `replay <id> --to <provider> [--config PATH]

  Re-answer a previously logged request with a DIFFERENT backend, so you can
  compare answers side-by-side. Fetches the stored request (method/path/body)
  from the daemon's /api/requests/<id> (request_log must be enabled), then
  re-sends it to the proxy with a one-shot force-provider override pinning THIS
  request to <provider> (no effect on other traffic). The new backend's response
  is written to stdout. Requires a running daemon + request_log.enabled.`,

	"doctor": `doctor [--live] [--config PATH]

  Offline scheduling diagnostic from config alone (no daemon needed): per-provider
  tier/quota source/peak_hours, per-route dry-run order (no live quota → tier then
  priority), and warnings (route with no plan provider, plan provider that will be
  unknown at runtime).

  --live   Live diagnosis against the running daemon instead — why an agent is
           stuck right now: routes with all targets down (+ earliest recovery
           time), active pins, first-choice quota nearly exhausted, daemon
           warnings, takeover pointer drift, and recent failed requests
           (request_log). Read-only. Each drifted client is also appended to
           the security audit log (kind=drift, hosts only) when guard.audit
           is on.`,

	"audit": `audit [--stats] [--from TIME] [--to TIME] [--kind KIND] [--limit N] [--json] [--config PATH]

  Show the security audit log (secret/path hits and doctor takeover-drift
  findings). Offline: reads the seclog files directly from guard.audit_path
  (default ~/.model-proxy/log/security/security.log) — no daemon needed.

  Flags:
  --from TIME   range start: now, duration ago (1h, 30m, 7d), unix seconds,
                or RFC3339 (default: unbounded)
  --to TIME     range end (same forms; default: unbounded)
  --kind KIND   filter to one kind: secret | path | drift
  --limit N     newest N records (default 50; 0 = all; ignored by --stats)
  --stats       aggregate view instead of raw records: total + counts by kind,
                top 10 hit names / agents, counts by action, over the whole
                filtered set (--limit ignored; internal cap 10000 records)
  --json        raw records JSON for jq; with --stats, the aggregate as JSON`,

	"guard": `guard <blocks|unblock|allowed|disallow> [--json] [--config PATH]

  Manage the AI second-opinion session blocks (guard.adjudicate): pattern
  guard hits a high verdict from the designated model under block_session
  block that client session's requests until explicitly unblocked here or in
  the WebUI Security page. Blocks persist across daemon restarts. Requires
  the running daemon (admin API).

  Unblocking IS the operator's final risk judgment: the verdict's content
  moves to the operator override table (same bytes are never re-intercepted
  or re-judged). Manage those overrides with allowed/disallow.

Subcommands:
  blocks                 list blocked sessions (newest first)
  blocks --json          raw JSON for jq
  unblock <session-id>   re-admit a blocked session immediately (also
                         releases its content — operator override)
  allowed                list operator content overrides (hash-keyed)
  allowed --json         raw JSON for jq
  disallow <hash>        revoke one override — the content returns to
                         fresh adjudication on its next occurrence`,

	"test": `test <model> [--config PATH]

  End-to-end link test: resolve the model's route targets (the derived route
  table — provider models aggregated per exposed name with explicit routes:
  overriding) and probe each
  target once with a real minimal upstream call.
  Exit status is 0 when at least one target answers 2xx, 1 when all fail.`,

	"mcp": `mcp [subcommand] [--config PATH]

  Manage MCP gateway servers (the mcp: config section; the daemon exposes
  them at /mcp/<name>).

Subcommands:
  list              List configured MCP servers (name, enabled, auth, provider, url).
  test <name>       Run the MCP handshake (initialize + tools/list) against one
                    server through its configured credentials.`,

	"stats": `stats [flags] [--config PATH]

  Query the running daemon's /api/stats endpoint and print per-(provider, model)
  call statistics from the SQLite store. The daemon (` + "`model-proxy serve`" + `)
  must be running.

Flags:
  --from TIME    range start (unix seconds or RFC3339; default: 60 min ago)
  --to TIME      range end (default: now)
  --provider P   filter to one provider
  --model M      filter to one model
  --bucket DUR   display granularity (1m/5m/10m/1h/1d; default 1m = raw rows;
                 storage is always 1-minute, so this only widens the view)
  --by-agent    switch to the agent view: per-agent (claude-code/codex/...)
                 request + token totals over the range, from /api/agents
  --json         raw /api/stats JSON for jq`,

	"unfreeze": `unfreeze [provider] [--config PATH]

  Clear frozen runtime provider state via the running daemon: circuit-open
  cooldowns, rate-limit cooldowns (429), and model-level lockouts — the
  provider is retried immediately instead of waiting out the cooldown.
  With no argument, clears ALL providers. A pooled parent name clears all
  its accounts. Sticky routes, pins, and learned parameter blocklists are
  NOT cleared. Requires a running daemon.`,

	"freeze": `freeze <provider> [--config PATH]

  Manually freeze a provider via the running daemon: the provider is
  excluded from scheduling (never selected as a target) until
  ` + "`unfreeze`" + ` clears it. The freeze is explicit — it has no expiry and
  is NOT cleared by success/failure recording — and persists across
  restarts (same config-fingerprint gate as the rest of the health state).
  A pooled parent name freezes all its accounts. Unlike unfreeze, freeze
  always requires an explicit provider (no freeze-all). Sticky routes,
  pins, quotas, and learned parameter blocklists are NOT touched.
  Requires a running daemon.`,

	"wire": `wire record <provider> [--model M] [--prompt P] [--out DIR]

  Record RAW upstream SSE streams for the golden-replay tests: one minimal
  stream=true request per endpoint (responses / chat / anthropic), written
  to <out>/<proto>_<provider>.sse (default: testdata/wire/). A non-2xx
  endpoint lands in a .err file instead and never overwrites a good .sse.
  Credentials come from login. Review recordings for sensitive content
  before committing them.`,

	"shadow": `shadow report [--from TIME] [--to TIME] [--config PATH]

  Shadow-evaluation aggregation from the running daemon's
  /api/shadow-report endpoint: per (route, primary, shadow) samples,
  status-match rate, latency diff and response-size ratio. Requires a
  running daemon with shadow routes configured and request_log enabled.`,
}

// takesProvider reports whether the command requires a <provider> argument
// whose -h help should list the providers defined in config.yaml.
func TakesProvider(cmd string) bool {
	switch cmd {
	case "login", "logout", "usage":
		return true
	}
	return false
}

// PrintConfigProvidersTo loads the config (best-effort) and lists the
// providers defined under providers:, so the user knows what to pass as
// <provider>. Silently skips if no config is available or it has no providers.
func PrintConfigProvidersTo(out io.Writer, args []string) {
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil || len(cfg.Providers) == 0 {
		return
	}
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintln(out, "\nProviders (from config):")
	for _, n := range names {
		p := cfg.Providers[n]
		baseURL := p.OpenAIBaseURL
		if baseURL == "" {
			baseURL = p.DecisionsBaseURL // pure-decisions provider (typesafe)
		}
		if baseURL == "" {
			baseURL = p.AnthropicBaseURL
		}
		fmt.Fprintf(out, "  %s  provider=%s  %s\n", display.Pad(n, 14), display.Pad(p.Provider, 12), baseURL)
	}
}
