package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	configdomain "model-proxy/internal/config"
	"model-proxy/provider"
)

const usage = `model-proxy — standalone portable multi-provider LLM proxy

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
var cmdHelp = map[string]string{
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

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	cmd := os.Args[1]
	// Top-level help.
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Print(usage)
		return
	}
	// Per-command help: if any arg is -h/--help, print that command's help.
	if help, ok := cmdHelp[cmd]; ok {
		for _, a := range os.Args[2:] {
			if a == "-h" || a == "--help" {
				fmt.Println(help)
				if takesProvider(cmd) {
					printConfigProviders(os.Args[2:])
				}
				return
			}
		}
	}
	switch cmd {
	case "serve":
		cmdServe(os.Args[2:])
	case "takeover":
		cmdTakeover(os.Args[2:])
	case "restore":
		cmdRestore(os.Args[2:])
	case "login":
		cmdLogin(os.Args[2:])
	case "logout":
		cmdLogout(os.Args[2:])
	case "usage":
		cmdUsage(os.Args[2:])
	case "models":
		cmdModels(os.Args[2:])
	case "config":
		cmdConfig(os.Args[2:])
	case "schedule":
		cmdSchedule(os.Args[2:])
	case "pin":
		cmdPin(os.Args[2:])
	case "unpin":
		cmdUnpin(os.Args[2:])
	case "unfreeze":
		cmdUnfreeze(os.Args[2:])
	case "stats":
		cmdStats(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "test":
		cmdTest(os.Args[2:])
	case "replay":
		cmdReplay(os.Args[2:])
	case "shadow":
		cmdShadow(os.Args[2:])
	case "wire":
		cmdWire(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		fmt.Print(usage)
		os.Exit(1)
	}
}

// configPath resolves the config file path, scanning --config / -config /
// --config= manually (ignoring other flags so flag.Parse doesn't choke on
// unknown ones like login's --import). Lookup order:
//
//  1. --config PATH flag            (explicit)
//  2. ~/.model-proxy/config.yaml   (user-level, shared across CWDs)
//  3. ./config.yaml                 (current directory)
//
// The first existing file wins. If none exists, "./config.yaml" is returned so
// LoadConfig reports a clear "not found" error.
func configPath(args []string) string {
	// 1. explicit flag
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if strings.HasPrefix(a, "--config=") {
			return strings.TrimPrefix(a, "--config=")
		}
	}
	// 2. user-level config under ~/.model-proxy/
	home := ""
	if h, err := os.UserHomeDir(); err == nil {
		home = h
	}
	if home != "" {
		p := filepath.Join(home, ".model-proxy", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 3. current directory
	return "config.yaml"
}

func routeNames(cfg *Config) string {
	names := make([]string, 0, len(cfg.Routes))
	for n := range cfg.Routes {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func cmdTakeover(args []string) {
	cp := configPath(args)
	cfg, err := LoadConfig(cp)
	if err != nil {
		log.Fatal(err)
	}
	which := positional(args)
	if err := runTakeover(cfg, which, backupDir(cp)); err != nil {
		log.Fatal(err)
	}
}

func cmdRestore(args []string) {
	cp := configPath(args)
	cfg, err := LoadConfig(cp)
	if err != nil {
		log.Fatal(err)
	}
	which := positional(args)
	if err := runRestore(cfg, which, backupDir(cp)); err != nil {
		log.Fatal(err)
	}
}

// hasPoolFile reports whether the plural credential pool file
// (<name>_apikeys.json) exists. Used to dispatch logout between the pool-aware
// path (operate on the pool) and the singular path (legacy single-file removal,
// including aqp/codex oauth files).
func hasPoolFile(name string) bool {
	_, err := os.Stat(poolPath(name))
	return err == nil
}

func cmdLogout(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := positional(args)
	if provName == "" {
		fmt.Println("usage: model-proxy logout <provider> [--label <name>] [--all]")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (provider=%s)\n", name, p.Provider)
		}
		return
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	providerID := prov.Provider

	// aqp/codex keep their existing single-file logout (oauth_auth.json). An
	// apikey provider with NO plural pool file but a legacy singular file also
	// goes through the singular removal path (backward compat: the legacy
	// singular <name>_apikey.json is removed by clearApiKey).
	if providerID == "aqp" || providerID == "codex" || !hasPoolFile(provName) {
		provMap := buildProviders(cfg).providers
		p := provMap[provName]
		if p == nil {
			log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
		}
		if err := p.Logout(); err != nil {
			log.Fatalf("logout failed: %v", err)
		}
		fmt.Println(cGreen("✓ Logged out"))
		return
	}

	// Pool-aware path: the plural pool file (<name>_apikeys.json) exists.
	//
	// Locking: the interactive/label/all SELECTION (read pool, list accounts,
	// prompt for a number) runs OUTSIDE the cross-process lock; it captures the
	// selected account's ID (not index) from the displayed list. The mutation
	// (re-load under the lock → remove by id → save / os.Remove) runs INSIDE
	// withPoolLock. Removing by id re-resolved under the lock is correct even if
	// the pool changed between display and lock: a concurrently-removed target is
	// a no-op save; a concurrently-added account is preserved.
	pool, err := loadPool(provName, providerID)
	if err != nil {
		log.Fatalf("logout failed: %v", err)
	}
	if len(pool.Accounts) == 0 {
		// Pool file exists but is empty — remove it and report not-logged-in.
		// Re-resolve under the cross-process lock so a concurrent `login` can't
		// append an account between the unlocked read above and the remove; if
		// the pool is still empty under the lock, drop the file. Mirrors the
		// in-lock remove-by-id path below.
		if err := withPoolLock(provName, func() error {
			cur, err := loadPool(provName, providerID)
			if err != nil {
				return err
			}
			if len(cur.Accounts) == 0 {
				if err := os.Remove(poolPath(provName)); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove pool file: %w", err)
				}
			}
			return nil
		}); err != nil {
			log.Fatalf("logout failed: %v", err)
		}
		fmt.Println(cYellow("Not logged in."))
		return
	}

	label := flagStringValue(args, "--label")
	all := hasFlagValue(args, "--all")
	// removeAll: --all clears every account. rmID: specific account id to drop.
	var rmID string
	removeAll := false
	switch {
	case all:
		removeAll = true
	case label != "":
		idx := -1
		for i, a := range pool.Accounts {
			if a.Label == label {
				idx = i
				break
			}
		}
		if idx < 0 {
			log.Fatalf("no account labeled %q in %s", label, provName)
		}
		rmID = pool.Accounts[idx].ID
	default:
		// Interactive: list + pick a number.
		fmt.Printf("Accounts for %s:\n", provName)
		for i, a := range pool.Accounts {
			fmt.Printf("  %d) %s  (#%s  added %s)\n", i+1, a.Label, mask(a.ID), a.AddedAt)
		}
		fmt.Print("Remove which (number)? ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(pool.Accounts) {
			log.Fatal("invalid selection")
		}
		rmID = pool.Accounts[n-1].ID
	}

	if err := withPoolLock(provName, func() error {
		cur, err := loadPool(provName, providerID)
		if err != nil {
			return err
		}
		if removeAll {
			cur.Accounts = nil
		} else {
			out := make([]poolAccount, 0, len(cur.Accounts))
			for _, a := range cur.Accounts {
				if a.ID == rmID {
					continue // drop the selected id
				}
				out = append(out, a)
			}
			cur.Accounts = out
		}
		if len(cur.Accounts) == 0 {
			if err := os.Remove(poolPath(provName)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove pool file: %w", err)
			}
		} else {
			if err := savePool(provName, providerID, cur); err != nil {
				return fmt.Errorf("save pool: %w", err)
			}
		}
		return nil
	}); err != nil {
		log.Fatalf("logout failed: %v", err)
	}

	if all {
		fmt.Println(cGreen("✓ Removed all accounts from " + provName))
	} else {
		fmt.Println(cGreen("✓ Removed account " + mask(rmID)))
	}
	maybeReloadDaemon(args)
}

func cmdUsage(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := positional(args)
	if provName == "" {
		// No provider specified → show usage for all configured providers.
		// printProviderUsage handles pool iteration for pooled parents (which
		// are absent from the buildProviders map under their plain name).
		names := make([]string, 0, len(cfg.Providers))
		for n := range cfg.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		for i, n := range names {
			if i > 0 {
				fmt.Println(cDim(usageDivider))
			}
			printProviderUsage(cfg, n)
		}
		return
	}
	if _, ok := cfg.Providers[provName]; !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	printProviderUsage(cfg, provName)
}

// printProviderUsage prints the usage for one provider entry, handling the
// credential-pool case (≥2 accounts): each account gets its own divider +
// per-account header (label + masked id) and the per-account usage fetch is
// dispatched with THAT account's cred. Single-account / non-pooled / aqp /
// codex go through the existing path (buildProviders → p.Usage).
//
// This does NOT use NewProxy — that would start the quota tracker (goroutines
// + HTTP polls), wasteful for a one-shot CLI. Pool accounts are iterated
// directly via loadPool, and the bound usage function is called per account.
func printProviderUsage(cfg *Config, provName string) {
	prov, ok := cfg.Providers[provName]
	if !ok {
		return
	}
	pool, _ := loadPool(provName, prov.Provider)
	if len(pool.Accounts) >= 2 {
		for ai, a := range pool.Accounts {
			if ai > 0 {
				fmt.Println(cDim(usageDivider))
			}
			fmt.Printf("%s (%s)\n", cBold(cCyan(a.Label)), mask(a.ID))
			cred := a.Credentials()
			if p := buildOne(cfg, provName, prov, cred); p != nil {
				if err := p.Usage(); err != nil {
					fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
				}
			}
		}
		return
	}
	// Single-account / non-pooled / aqp / codex: build one provider + call Usage.
	provMap := buildProviders(cfg).providers
	p := provMap[provName]
	if p == nil {
		return
	}
	if err := p.Usage(); err != nil {
		fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
	}
}

// usageDivider separates multiple usage blocks (providers in `usage` with no
// arg, or accounts within a pooled provider). Printed between blocks only —
// never before the first or after the last.
const usageDivider = "────────────────────────────────────────"

func providerNames(cfg *Config) string {
	names := []string{}
	for n := range cfg.Providers {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// runVolcengineLoginWithInput performs a pool-aware volcengine login. The
// triple (api_key + access_key + secret_key) may be passed directly (tests) or,
// when any is empty, prompted on stdin. The account is deduped by id
// (accountIDFor → AccessKey for volcengine): a new id appends; an existing id
// with replace=true (or an interactive `y` on stdin when replace=false)
// overwrites the entry's triple/label in place; an existing id without
// confirmation aborts with "login cancelled". The entry's label defaults to the
// id when not supplied. The pool is written to ~/.model-proxy/<name>_apikeys.json
// via savePool — the legacy singular <name>_apikey.json is no longer written.
//
// Locking: ALL stdin (triple prompts + replace confirmation) happens BEFORE the
// cross-process lock — same invariant as runApiKeyLoginWithInput. The replace
// confirmation is resolved with a read-only loadPool; the authoritative
// load→dedup→save then runs under withPoolLock (in addVolcengineAccount).
//
// This is the CLI wrapper: it owns stdin prompting + stdout printing, then
// delegates the dedup→save core to addVolcengineAccount (reused by the web
// layer, Task 12).
func runVolcengineLoginWithInput(cfg *Config, provName string, prov Provider, inKey, inAK, inSK, label string, replace bool) error {
	// === BEFORE LOCK: apikey + AK + SK prompts ===
	apiKey := strings.TrimSpace(inKey)
	if apiKey == "" {
		fmt.Printf("Ark API Key (对话用，控制台创建): ")
		fmt.Scanln(&apiKey)
	}
	ak := strings.TrimSpace(inAK)
	if ak == "" {
		fmt.Printf("Volcengine Access Key ID (GetAFPUsage 用，IAM 密钥): ")
		fmt.Scanln(&ak)
	}
	sk := strings.TrimSpace(inSK)
	if sk == "" {
		fmt.Printf("Volcengine Secret Access Key: ")
		fmt.Scanln(&sk)
	}
	if apiKey == "" {
		return fmt.Errorf("API key is required")
	}

	id := accountIDFor(prov.Provider, accountCred{APIKey: apiKey, AccessKey: ak})

	// Resolve replace confirmation BEFORE the lock (stdin must never block the
	// cross-process lock).
	if !replace {
		existing, err := loadPool(provName, prov.Provider)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		for _, a := range existing.Accounts {
			if a.ID == id {
				fmt.Printf("Account %q is already logged in. Replace its key? [y/N] ", a.Label)
				reader := bufio.NewReader(os.Stdin)
				ans, _ := reader.ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
					return fmt.Errorf("login cancelled")
				}
				break
			}
		}
		replace = true // user confirmed; tell the core to overwrite
	}

	// Announce validation (UX parity with the apikey login's "Validating API
	// key..." line). One line covering whatever addVolcengineAccount will probe
	// (Ark key via usage_url and/or the AK/SK pair) — the AK/SK step is not
	// pre-announced separately because it only runs if the Ark-key probe passes.
	if prov.UsageURL != "" || (ak != "" && sk != "") {
		fmt.Fprintf(os.Stderr, "Validating credentials...\n")
	}

	if _, err := addVolcengineAccount(cfg, provName, prov, accountCred{APIKey: apiKey, AccessKey: ak, SecretKey: sk}, label, replace); err != nil {
		return err
	}
	// Print the confirmation line (label resolved from the freshly-saved pool,
	// which may have been re-sorted by savePool).
	pool, _ := loadPool(provName, prov.Provider)
	fmt.Println(cGreen("✓ Saved account ") + cGray(mask(id)+" ("+labelFor(pool, id)+")"))
	return nil
}

// volcengineAKSKValidator validates a Volcengine AccessKey/SecretKey pair. It
// defaults to provider.ValidateVolcengineAKSK (the signed GetAFPUsage control-
// plane call); login_cmd tests override it to assert addVolcengineAccount's
// decision logic without hitting the real Volcengine API. Package-level var
// (process-wide invariant, not per-provider config).
var volcengineAKSKValidator = provider.ValidateVolcengineAKSK

// addVolcengineAccount is the non-printing core for volcengine's AK/SK triple:
// it validates the Ark API Key (via usage_url, a Bearer GET to /models) and, when
// both AK and SK are present, the AK/SK pair (via the signed GetAFPUsage); then
// dedups by id (= AccessKey for volcengine) under the cross-process lock and
// writes the pool. Returns the account id. No stdin, no stdout — symmetric with
// addApikeyAccount; reused by the web layer (Task 12). Validation happens BEFORE
// the lock (a slow probe must not hold the cross-process lock). AK/SK are
// optional — chat-only accounts skip that check. replace=false on an existing id
// returns "login cancelled" without modifying the pool.
func addVolcengineAccount(cfg *Config, name string, prov Provider, cred accountCred, label string, replace bool) (string, error) {
	apiKey := strings.TrimSpace(cred.APIKey)
	ak := strings.TrimSpace(cred.AccessKey)
	sk := strings.TrimSpace(cred.SecretKey)
	if apiKey == "" {
		return "", fmt.Errorf("API key is required")
	}
	// Validate the Ark API Key via usage_url (GET /models with Bearer), mirroring
	// addApikeyAccount's usage_url gate: 401/403 or a network error = bad key →
	// reject before save. No-op when usage_url is unset.
	if err := validateKeyBearerGET(prov.UsageURL, apiKey); err != nil {
		return "", err
	}
	// AK/SK are optional (chat-only accounts omit them entirely), but they must
	// be BOTH set or BOTH empty: a lone AK or SK can't sign GetAFPUsage
	// (resolveAKSK requires both), yet before this guard it saved silently and
	// degraded to BillingUnknown at runtime with no login-time signal. Reject
	// the partial pair up front.
	if (ak == "") != (sk == "") {
		return "", fmt.Errorf("AccessKey and SecretKey must both be set, or both be empty for a chat-only account")
	}
	if ak != "" && sk != "" {
		if err := volcengineAKSKValidator(ak, sk); err != nil {
			return "", fmt.Errorf("validation failed: %w", err)
		}
	}
	id := accountIDFor(prov.Provider, accountCred{APIKey: apiKey, AccessKey: ak})
	return id, withPoolLock(name, func() error {
		pool, err := loadPool(name, prov.Provider)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		now := nowTS()
		idx := -1
		for i, a := range pool.Accounts {
			if a.ID == id {
				idx = i
				break
			}
		}
		if idx >= 0 {
			if !replace {
				return fmt.Errorf("login cancelled")
			}
			pool.Accounts[idx].APIKey = apiKey
			pool.Accounts[idx].AccessKey = ak
			pool.Accounts[idx].SecretKey = sk
			if label != "" {
				pool.Accounts[idx].Label = label
			}
			pool.Accounts[idx].AddedAt = now
		} else {
			lbl := label
			if lbl == "" {
				lbl = id
			}
			pool.Accounts = append(pool.Accounts, poolAccount{
				ID: id, Label: lbl, APIKey: apiKey, AccessKey: ak, SecretKey: sk, AddedAt: now,
			})
		}
		return savePool(name, prov.Provider, pool)
	})
}

// volcengineCreds is the on-disk format of the volcengine apikey file: the Ark
// API Key (chat) plus the Volcengine AK/SK (GetAFPUsage).
type volcengineCreds struct {
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

func loadVolcengineCreds(provName string) (*volcengineCreds, error) {
	path := filepath.Join(homeDir(), ".model-proxy", provName+"_apikey.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c volcengineCreds
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func cmdConfig(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: model-proxy config [init|print|check]")
		os.Exit(1)
	}
	switch args[0] {
	case "init":
		if err := os.WriteFile("config.yaml", []byte(defaultConfigYAML), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote config.yaml")
	case "print":
		cfg, err := LoadConfig(configPath(args[1:]))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("listen: %s\n", cfg.Listen)
		for name, prov := range cfg.Providers {
			fmt.Printf("provider %s: openai_base_url=%s provider_id=%s (%d models)\n", name, prov.OpenAIBaseURL, prov.Provider, len(prov.Models))
		}
		for exposed, targets := range cfg.Routes {
			fmt.Printf("route %s: %d targets\n", exposed, len(targets))
		}
		if len(cfg.ClaudeMapping) > 0 {
			fmt.Printf("claude_mapping: %d aliases\n", len(cfg.ClaudeMapping))
		}
	case "check":
		cfg, err := LoadConfig(configPath(args[1:]))
		if err != nil {
			fmt.Println(cRed("✗ config invalid") + ": " + err.Error())
			os.Exit(1)
		}
		fmt.Println(cGreen("✓ config valid"))
		fmt.Printf("  listen:    %s\n", cfg.Listen)
		fmt.Printf("  log_file:  %s\n", cfg.LogFile)
		fmt.Printf("  providers: %d\n", len(cfg.Providers))
		for name, prov := range cfg.Providers {
			fmt.Printf("    %s: %s (%s, %d models)\n", name, prov.OpenAIBaseURL, prov.Provider, len(prov.Models))
		}
		fmt.Printf("  routes:    %d\n", len(cfg.Routes))
		for exposed, targets := range cfg.Routes {
			fmt.Printf("    %s: %d targets\n", exposed, len(targets))
		}
		fmt.Printf("  claude_mapping: %d\n", len(cfg.ClaudeMapping))
		s := cfg.Scheduling
		fmt.Printf("  scheduling: threshold=%d cooldown=%s rate_backoff=%s timeout=%s dwell=%s\n",
			s.Threshold(), s.Cooldown(), s.RateBackoff(), s.Timeout(), s.Dwell())
		// Config-time routing hazards (explicit routes only — implicit routes are
		// a daemon-side concept; the daemon logs these at boot/reload).
		for _, w := range configRoutingWarnings(cfg, cfg.Routes) {
			fmt.Println(cYellow("  ⚠ " + w))
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

// cmdSchedule queries the running daemon's /debug/schedule endpoint and prints
// which provider each model is currently scheduled to (first-choice + ordered
// list + sticky state). The daemon (`model-proxy serve`) must be running.
func cmdSchedule(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	resp, err := daemonHTTPClient.Get("http://" + cfg.Listen + "/debug/schedule")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot reach daemon at %s: %v\nis `model-proxy serve` running?\n",
			cRed("✗"), cfg.Listen, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "%s daemon returned HTTP %d: %s\n", cRed("✗"), resp.StatusCode, truncate(string(body), 200))
		os.Exit(1)
	}
	var st statusSchedule
	if err := json.Unmarshal(body, &st); err != nil {
		fmt.Fprintf(os.Stderr, "%s parse schedule response: %v\n", cRed("✗"), err)
		os.Exit(1)
	}
	if len(st.Models) == 0 {
		fmt.Println("(no routes)")
		return
	}
	fmt.Print(renderScheduleRoutes(st.Models, ""))
}

// cmdDoctor runs an OFFLINE diagnostic of the scheduling setup from config (no
// daemon needed): per-provider tier/quota source/peak_hours, per-route dry-run
// order (no live quota → all unknown → tier then priority), and warnings.
// Credential pools (≥2 accounts) are expanded inline: the parent is shown with
// its account count + the per-account virtual ids, plus a note that new sessions
// round-robin across the pool (offline: no live quota → falls back to priority).
func cmdDoctor(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		fmt.Println(cRed("✗ config invalid: ") + err.Error())
		os.Exit(1)
	}
	// --live replaces the offline report with the live daemon diagnosis; the
	// two never print together (the live report re-derives everything from
	// /api/status + local takeover state).
	if doctorLive(args) {
		out, err := renderDoctorLive(cfg, configPath(args))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err.Error())
			os.Exit(1)
		}
		fmt.Print(out)
		return
	}
	doctorWithCfg(cfg)
}

// doctorWithCfg renders the doctor diagnostic for an already-loaded config.
// Extracted from cmdDoctor so tests can drive it in-process with a hand-built
// Config (no temp config file needed). Writes to stdout; returns the warning
// count.
func doctorWithCfg(cfg *Config) int {
	fmt.Println(cGreen("✓ config valid"))

	fmt.Printf("\n%s\n", cBold("Providers"))
	pnames := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		pnames = append(pnames, n)
	}
	sort.Strings(pnames)
	for _, name := range pnames {
		prov := cfg.Providers[name]
		tier := "plan"
		if prov.Billing == "pay-as-you-go" {
			tier = "pay-as-you-go"
		}
		extra := ""
		if vids, pooled := poolVirtuals(cfg, name); pooled {
			extra = fmt.Sprintf("  pool: %d accounts", len(vids))
		}
		fmt.Printf("  %s %s  quota=%s  peak=%s%s\n",
			pad(name, 12), cCyan(pad(tier, 13)), cGray(quotaSourceLabel(prov.Provider)), peakSummary(prov.PeakHours), extra)
	}

	fmt.Printf("\n%s\n", cBold("Routes (dry-run: no live quota → tier then priority)"))
	warns := 0
	rnames := make([]string, 0, len(cfg.Routes))
	for n := range cfg.Routes {
		rnames = append(rnames, n)
	}
	sort.Strings(rnames)
	for _, exposed := range rnames {
		targets := cfg.Routes[exposed]
		fmt.Printf("  %s\n", exposed)
		hasPlan := false
		for _, t := range dryRunOrder(cfg, targets) {
			prov := cfg.Providers[t.Provider]
			tier := "plan"
			if prov.Billing == "pay-as-you-go" {
				tier = "pay-as-you-go"
			}
			if tier == "plan" {
				hasPlan = true
			}
			fmt.Printf("    %s %s  p%d\n", pad(t.Provider, 12), cCyan(pad(tier, 13)), t.Priority)
			// Wire-protocol note: a provider our protocol system can neither
			// passthrough nor convert (none today — codex/Responses is now
			// converted) would get the HONEST marker — which client families
			// can't be served — not a conversion suggestion.
			if note := provider.WireProtocolNote(prov.Provider); note != "" {
				fmt.Printf("        %s %s\n", cYellow("⚠"), note)
				warns++
			} else if t.Protocol != "" {
				// Protocol conversion (#11): a target declaring a backend protocol
				// converts client↔backend when they differ. Surface it + the fixed
				// set of fields conversion drops (so an operator wiring tools/images
				// knows what's lossy before traffic flows).
				fmt.Printf("        %s target protocol %s — converts when client protocol differs; remaining lossy: anthropic↔chat thinking, unsupported server tools, input_audio (cache breakpoints/documents/tool-result media preserved)\n",
					cYellow("↔"), t.Protocol)
				// Reasoning-replay marker (#9): for models that REQUIRE reasoning
				// content echoed back, the dropped thinking/reasoning is fatal to
				// multi-turn tool calls, not just lossy.
				if reasoningReplayModel(t.Model) && t.Protocol == "openai" {
					fmt.Printf("        %s reasoning-required model behind openai-chat conversion — Anthropic thinking is dropped; multi-turn tool calls may 400 upstream (replay cache not implemented)\n",
						cYellow("⚠"))
					warns++
				}
			} else if hint := provider.ProtocolHint(prov.Provider, t.Model); hint != "" {
				fmt.Printf("        %s no protocol: declared, but %s speaks %s — clients of the other protocol will send malformed bodies; add protocol: %s\n",
					cYellow("⚠"), prov.Provider, hint, hint)
				warns++
			}
			// Expand a pooled parent inline: show its account count + the
			// per-account virtual ids. Offline (no live quota) so we can't show
			// per-account surplus — note the session-sticky round-robin so an
			// operator understands how traffic spreads at runtime.
			if vids, pooled := poolVirtuals(cfg, t.Provider); pooled {
				fmt.Printf("        %s %d accounts (round-robin session-sticky; no live quota → falls back to priority)\n",
					cDim("pool:"), len(vids))
				for _, vid := range vids {
					fmt.Printf("        %s\n", cGray(vid))
				}
			}
		}
		if !hasPlan {
			fmt.Printf("    %s no plan provider — only pay-as-you-go\n", cYellow("⚠"))
			warns++
		}
	}

	// Shadow evaluation: each route's candidate backend + the global sampling
	// knobs. Omitted entirely when no shadow is configured.
	if len(cfg.Shadow) > 0 {
		fmt.Printf("\n%s\n", cBold("Shadow"))
		rate := 1.0
		if cfg.ShadowSampleRate != nil {
			rate = *cfg.ShadowSampleRate
		}
		maxConc := cfg.ShadowMaxConcurrent
		if maxConc <= 0 {
			maxConc = 4
		}
		sroutes := make([]string, 0, len(cfg.Shadow))
		for r := range cfg.Shadow {
			sroutes = append(sroutes, r)
		}
		sort.Strings(sroutes)
		for _, route := range sroutes {
			sh := cfg.Shadow[route]
			proto := sh.Protocol
			if proto == "" {
				proto = "same-as-primary"
			}
			fmt.Printf("  %s → %s/%s  protocol=%s  sample_rate=%s  max_concurrent=%d\n",
				pad(route, 12), sh.Provider, sh.Model, proto, strconv.FormatFloat(rate, 'f', -1, 64), maxConc)
		}
	}

	// Fusion orchestration: one line per recipe — panel size/quorum/synthesizer
	// plus the cost knobs (budget, first_turn_only) and quality knobs (judge,
	// custom instruction). Omitted entirely when no fusion recipe is configured.
	if len(cfg.Fusion) > 0 {
		fmt.Printf("\n%s\n", cBold("Fusion"))
		fnames := make([]string, 0, len(cfg.Fusion))
		for n := range cfg.Fusion {
			fnames = append(fnames, n)
		}
		sort.Strings(fnames)
		for _, name := range fnames {
			f := cfg.Fusion[name]
			quorum := f.MinPanel
			if quorum <= 0 {
				quorum = 2
			}
			if quorum > len(f.Panel) {
				quorum = len(f.Panel)
			}
			budget := "unlimited"
			if f.MaxRunsPerDay > 0 {
				budget = strconv.Itoa(f.MaxRunsPerDay) + "/day"
			}
			extra := ""
			if f.FirstTurnOnly {
				extra += "  first_turn_only"
			}
			if f.Judge != nil {
				extra += fmt.Sprintf("  judge=%s/%s", f.Judge.Provider, f.Judge.Model)
			}
			if f.Instruction != "" {
				extra += "  custom instruction"
			}
			fmt.Printf("  %s panel=%d quorum=%d  synthesizer=%s/%s  budget=%s%s\n",
				pad(name, 12), len(f.Panel), quorum, f.Synthesizer.Provider, f.Synthesizer.Model, budget, extra)
		}
	}

	s := cfg.Scheduling
	fmt.Printf("\n%s\n", cBold("Scheduling"))
	fmt.Printf("  sticky_dwell=%s  quota_poll_interval=%s  quota_switch_margin=%d pts  circuit=(threshold %d, cooldown %s)\n",
		s.Dwell(), s.PollInterval(), s.QuotaSwitchMargin, s.Threshold(), s.Cooldown())
	if warns > 0 {
		fmt.Printf("\n%s %d warning(s)\n", cYellow("⚠"), warns)
	} else {
		fmt.Printf("\n%s no warnings\n", cGreen("✓"))
	}
	return warns
}

// quotaSourceLabel returns a short label for where a provider's quota comes from
// (by provider_id), or "(none → unknown at runtime)" for ids without a Quota parser.
func quotaSourceLabel(providerID string) string {
	switch providerID {
	case "aqp":
		return "monthly_usage"
	case "codex":
		return "wham/usage"
	case "zhipu":
		return "quota/limit"
	case "volcengine":
		return "GetAFPUsage (AK/SK)"
	case "deepseek":
		return "user/balance"
	case "kimi-code":
		return "usages"
	case "zcode":
		return "quota/limit"
	default:
		return "(none → unknown at runtime)"
	}
}

// peakSummary renders a PeakConfig as "09:00-12:00(×2), 14:00-18:00(×2)" or "-".
func peakSummary(ph PeakConfig) string {
	if len(ph) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ph))
	for _, seg := range ph {
		mult := seg.Multiplier
		if mult == 0 {
			mult = configdomain.DefaultPeakMultiplier
		}
		parts = append(parts, fmt.Sprintf("%s(×%g)", seg.Window, mult))
	}
	return strings.Join(parts, ", ")
}

// dryRunOrder sorts targets by the offline schedule order: billing tier (plan
// before pay-as-you-go), then priority asc. With no live quota all plan-intent
// providers are equal-surplus, so tier + priority decide.
func dryRunOrder(cfg *Config, targets []RouteTarget) []RouteTarget {
	out := append([]RouteTarget(nil), targets...)
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := 0, 0
		if cfg.Providers[out[i].Provider].Billing == "pay-as-you-go" {
			ti = 1
		}
		if cfg.Providers[out[j].Provider].Billing == "pay-as-you-go" {
			tj = 1
		}
		if ti != tj {
			return ti < tj
		}
		return out[i].Priority < out[j].Priority
	})
	return out
}

// positional returns the first non-flag positional arg (skipping --config xxx).
func positional(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" {
			i++ // skip its value
			continue
		}
		if len(a) > 8 && a[:8] == "--config" {
			continue // --config=xxx
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// takesProvider reports whether the command requires a <provider> argument
// whose -h help should list the providers defined in config.yaml.
func takesProvider(cmd string) bool {
	switch cmd {
	case "login", "logout", "usage":
		return true
	}
	return false
}

// printConfigProviders loads the config (best-effort) and lists the providers
// defined under providers:, so the user knows what to pass as <provider>.
// Silently skips if no config is available or it has no providers.
func printConfigProviders(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil || len(cfg.Providers) == 0 {
		return
	}
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("\nProviders (from config):")
	for _, n := range names {
		p := cfg.Providers[n]
		fmt.Printf("  %s  provider=%s  %s\n", pad(n, 14), pad(p.Provider, 12), p.OpenAIBaseURL)
	}
}
