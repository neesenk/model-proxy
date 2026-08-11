package cli

import (
	"fmt"
	"io"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	"os"
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
  login <provider>     Login to a provider (aqp | codex | zcode)
  logout <provider>    Clear provider credentials
  usage <provider>     Show usage / credits for a provider
  models               List models from all providers (from config)
  models <provider>    List models for one provider
  models refresh <provider>  Fetch live model list from a provider
  config init          Generate a config.yaml template
  config print         Print the effective config
  config check         Validate config and print a summary
  schedule             Show current per-model provider (queries the running daemon)
  pin <route> <prov>   Temporarily force a route onto one provider (no failover)
  unpin <route>        Remove a pin
  unfreeze [provider]  Clear frozen provider state (circuit/rate-limit/model locks)
  stats                Show per-(provider, model) call statistics (queries the daemon)
  doctor               Offline scheduling diagnostic (config only, no daemon)
  test <model>         End-to-end probe of a model's route targets (real upstream calls)
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

	"takeover": `takeover <client> [--config PATH]

  Rewrite a client's config to point at the proxy (backs up the original).

Clients:
  claude | opencode | codex | pi | all`,

	"restore": `restore <client> [--config PATH]

  Restore a client's config from the backup created by takeover.

Clients:
  claude | opencode | codex | pi | all`,

	"login": `login <provider> [--config PATH]

  Authenticate with a provider.`,

	"logout": `logout <provider> [--config PATH]

  Clear stored credentials for a provider.`,

	"usage": `usage <provider> [--config PATH]

  Show usage / credits for a provider.`,

	"models": `models [subcommand] [provider] [--config PATH]

  List or refresh models.

Usage:
  models              List all models from all providers (from config).
  models <provider>   List models for one provider.
  models refresh <provider>  Fetch live model list from a provider's server.`,

	"config": `config <subcommand> [--config PATH]

  Manage config files.

Subcommands:
  init      Generate a config.yaml template in the current directory.
  print     Print the effective config.
  check     Validate the config and print a summary.`,

	"schedule": `schedule [--config PATH]

  Query the running daemon's /debug/schedule endpoint and print which provider
  each model is currently scheduled to (first-choice + ordered list + sticky
  state). The daemon (` + "`model-proxy serve`" + `) must be running.`,

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
           (request_log). Read-only.`,

	"test": `test <model> [--config PATH]

  End-to-end link test: resolve the model's route targets (explicit routes,
  then the implicit-route fallback; claude_mapping aliases are translated
  first) and probe each target once with a real minimal upstream call.
  Exit status is 0 when at least one target answers 2xx, 1 when all fail.`,

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

	"wire": `wire record <provider> [--model M] [--prompt P] [--out DIR]

  Record RAW upstream SSE streams for the golden-replay tests: one minimal
  stream=true request per endpoint (responses / chat / anthropic), written
  to <out>/<proto>_<provider>.sse (default: testdata/wire/). A non-2xx
  endpoint lands in a .err file instead and never overwrites a good .sse.
  Credentials come from login. Review recordings for sensitive content
  before committing them.`,
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

// printConfigProviders loads the config (best-effort) and lists the providers
// defined under providers:, so the user knows what to pass as <provider>.
// Silently skips if no config is available or it has no providers.
func PrintConfigProviders(args []string) {
	PrintConfigProvidersTo(os.Stdout, args)
}

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
		fmt.Fprintf(out, "  %s  provider=%s  %s\n", provider.Pad(n, 14), provider.Pad(p.Provider, 12), p.OpenAIBaseURL)
	}
}
