package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"model-proxy/provider"
)

const usage = `model-proxy — standalone portable multi-provider LLM proxy

Usage: model-proxy <COMMAND> [SUBCOMMAND] [OPTIONS]

Commands:
  serve                Start the proxy server
  serve daemon         Run in background (auto-restart on crash)
  serve stop           Stop a running daemon
  serve reload         Hot-reload config (SIGHUP the running daemon)
  takeover <client>    Rewrite client config to point at the proxy
  restore <client>     Restore client config from backup
  login <provider>     Login to a provider (aqp | codex)
  logout <provider>    Clear provider credentials
  usage <provider>     Show usage / credits for a provider
  models               List models from all providers (from config)
  models <provider>    List models for one provider
  models refresh <provider>  Fetch live model list from a provider
  config init          Generate a config.yaml template
  config print         Print the effective config
  config check         Validate config and print a summary
  schedule             Show current per-model provider (queries the running daemon)
  doctor               Offline scheduling diagnostic (config only, no daemon)
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

	"doctor": `doctor [--config PATH]

  Offline scheduling diagnostic from config alone (no daemon needed): per-provider
  tier/quota source/peak_hours, per-route dry-run order (no live quota → tier then
  priority), and warnings (route with no plan provider, plan provider that will be
  unknown at runtime).`,
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
	case "doctor":
		cmdDoctor(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		fmt.Print(usage)
		os.Exit(1)
	}
}

// homeDirForTest is overridden in tests to redirect the user-config lookup.
// In production it's empty, and os.UserHomeDir() is used.
var homeDirForTest = ""

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
	home := homeDirForTest
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
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
		provMap, _, _ := buildProviders(cfg)
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
			if err := savePool(provName, cur); err != nil {
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
		for _, n := range names {
			printProviderUsage(cfg, n)
			fmt.Println()
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
	providerID := prov.Provider
	pool, _ := loadPool(provName, providerID)
	if len(pool.Accounts) >= 2 {
		for _, a := range pool.Accounts {
			fmt.Println(cDim("────────────────────────────────────────"))
			fmt.Printf("%s (%s)\n", cBold(cCyan(a.Label)), mask(a.ID))
			cred := a.cred()
			switch providerID {
			case "deepseek":
				showDeepseekUsage(cfg, provName, prov, &cred)
			case "volcengine":
				showVolcengineUsage(cfg, provName, prov, &cred)
			default:
				// zhipu + future apikey providers use the generic BigModel /
				// OpenAI-style usage display.
				showGenericUsage(cfg, provName, prov, &cred)
			}
		}
		return
	}
	// Single-account / non-pooled / aqp / codex: existing path.
	provMap, _, _ := buildProviders(cfg)
	p := provMap[provName]
	if p == nil {
		return
	}
	fmt.Println(cDim("────────────────────────────────────────"))
	if _, err := p.Usage(); err != nil {
		fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
	}
}

func providerNames(cfg *Config) string {
	names := []string{}
	for n := range cfg.Providers {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

func showAqpUsage(cfg *Config) {
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue("aqp")))
	path := authFilePath("aqp", "oauth_auth")
	a, err := loadAccount(path)
	if err != nil {
		fmt.Println(cRed("Error: " + err.Error()))
		return
	}
	if a == nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("model-proxy login aqp"))
		return
	}
	c := newAqpClient(path)
	fmt.Printf("%s %s\n", cDim("Account:    "), cBold(cCyan(a.Email)))
	fmt.Printf("%s %s\n", cDim("Project ID: "), cGray(a.ProjectID))
	mu, err := c.MonthlyUsage()
	if err != nil {
		fmt.Printf("%s %s\n", cDim("Usage:      "), cRed("(unavailable: "+err.Error()+")"))
	} else {
		fmt.Printf("%s %s / %s  (%s %s, %s, %d-%02d)\n",
			cDim("Usage:      "),
			usageRatioColor(mu.Balance, mu.TotalAmount, money(mu.Usage)),
			cGray(money(mu.TotalAmount)),
			cDim("balance"), usageRatioColor(mu.Balance, mu.TotalAmount, money(mu.Balance)),
			cMagenta(mu.Plan), mu.SelectedYear, mu.SelectedMonth)
	}
	fmt.Printf("%s %s\n", cDim("Store:      "), cGray(path))
}

func showCodexUsage(cfg *Config, prov Provider) {
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue("codex")))
	authFile := authFilePath("codex", "oauth_auth")
	p := newCodexOAuthProvider(authFile)
	tok, acct, err := p.token()
	if err != nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("model-proxy login codex"))
		return
	}
	// Usage endpoint is at /backend-api/wham/usage, NOT under the codex base
	// (/backend-api/codex/wham/usage returns 403). Derive the backend-api root
	// from the provider openai_base_url by stripping the trailing /codex segment.
	usageURL := strings.TrimSuffix(prov.OpenAIBaseURL, "/codex") + "/wham/usage"
	req, _ := http.NewRequest("GET", usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("originator", "codex_cli_rs")
	if acct != "" {
		req.Header.Set("ChatGPT-Account-Id", acct)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(cRed("Error: usage request: " + err.Error()))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", cRed("Error:"), resp.StatusCode, truncate(string(body), 200))
		return
	}
	var u struct {
		Email    string `json:"email"`
		PlanType string `json:"plan_type"`
		Credits  *struct {
			HasCredits bool    `json:"has_credits"`
			Unlimited  bool    `json:"unlimited"`
			Balance    *string `json:"balance"`
		} `json:"credits"`
		RateLimit *struct {
			Allowed       bool `json:"allowed"`
			LimitReached  bool `json:"limit_reached"`
			PrimaryWindow *struct {
				UsedPercent     int `json:"used_percent"`
				LimitWindowSecs int `json:"limit_window_seconds"`
				ResetAfterSecs  int `json:"reset_after_seconds"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent     int `json:"used_percent"`
				LimitWindowSecs int `json:"limit_window_seconds"`
				ResetAfterSecs  int `json:"reset_after_seconds"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		SpendControl *struct {
			Reached         bool `json:"reached"`
			IndividualLimit *struct {
				Used        string `json:"used"`
				Limit       string `json:"limit"`
				Remaining   string `json:"remaining"`
				UsedPercent int    `json:"used_percent"`
				ResetAfter  int    `json:"reset_after_seconds"`
			} `json:"individual_limit"`
		} `json:"spend_control"`
	}
	json.Unmarshal(body, &u)
	fmt.Printf("%s %s\n", cDim("Account:   "), cBold(cCyan(or(u.Email, "(unknown)"))))
	fmt.Printf("%s %s\n", cDim("Plan:      "), cMagenta(or(u.PlanType, "(unknown)")))
	// Credits
	if u.Credits != nil {
		if u.Credits.Unlimited {
			fmt.Printf("%s %s\n", cDim("Credits:   "), cGreen("unlimited"))
		} else if u.Credits.HasCredits {
			bal := "available"
			if u.Credits.Balance != nil && *u.Credits.Balance != "" {
				bal = *u.Credits.Balance
			}
			fmt.Printf("%s %s\n", cDim("Credits:   "), cGreen("has credits ("+bal+")"))
		} else {
			fmt.Printf("%s %s\n", cDim("Credits:   "), cRed("none"))
		}
	}
	// Rate limit windows
	if u.RateLimit != nil {
		status := cGreen("allowed")
		if u.RateLimit.LimitReached {
			status = cRed("limit reached")
		} else if !u.RateLimit.Allowed {
			status = cYellow("not allowed")
		}
		fmt.Printf("%s %s\n", cDim("Rate Limit:"), status)
		if u.RateLimit.PrimaryWindow != nil {
			pw := u.RateLimit.PrimaryWindow
			fmt.Printf("%s %s\n", cDim("  primary:  "),
				usageRatioColor(float64(100-pw.UsedPercent), 100, fmt.Sprintf("%d%% used (resets in %s)", pw.UsedPercent, formatDuration(pw.ResetAfterSecs))))
		}
		if u.RateLimit.SecondaryWindow != nil {
			sw := u.RateLimit.SecondaryWindow
			fmt.Printf("%s %s\n", cDim("  weekly:   "),
				usageRatioColor(float64(100-sw.UsedPercent), 100, fmt.Sprintf("%d%% used (resets in %s)", sw.UsedPercent, formatDuration(sw.ResetAfterSecs))))
		}
	}
	// Spend control — progress bar + numbers
	if u.SpendControl != nil {
		if u.SpendControl.Reached {
			fmt.Printf("%s %s\n", cDim("Spend:     "), cRed("limit reached"))
		} else if u.SpendControl.IndividualLimit != nil {
			il := u.SpendControl.IndividualLimit
			pct := il.UsedPercent
			bar := progressBar(pct, 20)
			pctStr := usageRatioColor(float64(100-pct), 100, fmt.Sprintf("%d%%", pct))
			resetStr := ""
			if il.ResetAfter > 0 {
				resetStr = cGray(" · resets " + formatDuration(il.ResetAfter))
			}
			fmt.Printf("%s %s / %s credits  %s  %s%s\n",
				cDim("Spend:     "),
				cBold(formatCredits(il.Used)), cGray(formatCredits(il.Limit)),
				bar, pctStr, resetStr)
		}
	}
}

// parseCodexQuota parses codex /backend-api/wham/usage into a QuotaSnapshot.
// Windows: primary(5h) + secondary(weekly) + spend(monthly $). Credits/rate-limit
// status go to Notes for display.
// ultimateRemaining returns the RemainingPct of the window marked Ultimate,
// or -1 if there is none (the parser then can't derive a scheduling base).
func ultimateRemaining(windows []provider.QuotaWindow) float64 {
	for _, w := range windows {
		if w.Ultimate {
			return w.RemainingPct
		}
	}
	return -1
}

func parseCodexQuota(body []byte, account, plan string) (*provider.QuotaSnapshot, error) {
	var u struct {
		Email    string `json:"email"`
		PlanType string `json:"plan_type"`
		Credits  *struct {
			HasCredits bool    `json:"has_credits"`
			Unlimited  bool    `json:"unlimited"`
			Balance    *string `json:"balance"`
		} `json:"credits"`
		RateLimit *struct {
			Allowed       bool `json:"allowed"`
			LimitReached  bool `json:"limit_reached"`
			PrimaryWindow *struct {
				UsedPercent     int `json:"used_percent"`
				LimitWindowSecs int `json:"limit_window_seconds"`
				ResetAfterSecs  int `json:"reset_after_seconds"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent     int `json:"used_percent"`
				LimitWindowSecs int `json:"limit_window_seconds"`
				ResetAfterSecs  int `json:"reset_after_seconds"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		SpendControl *struct {
			Reached         bool `json:"reached"`
			IndividualLimit *struct {
				Used        string `json:"used"`
				Limit       string `json:"limit"`
				Remaining   string `json:"remaining"`
				UsedPercent int    `json:"used_percent"`
				ResetAfter  int    `json:"reset_after_seconds"`
			} `json:"individual_limit"`
		} `json:"spend_control"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	s := &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		Account: or(u.Email, account),
		Plan:    or(u.PlanType, plan),
		AsOf:    time.Now(),
	}
	now := time.Now()
	if u.RateLimit != nil {
		status := "allowed"
		if u.RateLimit.LimitReached {
			status = "limit reached"
		} else if !u.RateLimit.Allowed {
			status = "not allowed"
		}
		s.Notes = append(s.Notes, "Rate Limit: "+status)
		if pw := u.RateLimit.PrimaryWindow; pw != nil {
			w := provider.QuotaWindow{
				Label:        "primary (5h)",
				Kind:         "tokens",
				RemainingPct: float64(100-pw.UsedPercent) / 100.0,
			}
			if pw.ResetAfterSecs > 0 {
				w.ResetsAt = now.Add(time.Duration(pw.ResetAfterSecs) * time.Second)
			}
			s.Windows = append(s.Windows, w)
		}
		if sw := u.RateLimit.SecondaryWindow; sw != nil {
			w := provider.QuotaWindow{
				Label:        "weekly",
				Kind:         "tokens",
				RemainingPct: float64(100-sw.UsedPercent) / 100.0,
			}
			if sw.ResetAfterSecs > 0 {
				w.ResetsAt = now.Add(time.Duration(sw.ResetAfterSecs) * time.Second)
			}
			s.Windows = append(s.Windows, w)
		}
	}
	if sc := u.SpendControl; sc != nil && sc.IndividualLimit != nil {
		il := sc.IndividualLimit
		w := provider.QuotaWindow{
			Label:        "Spend",
			Kind:         "money",
			RemainingPct: float64(100-il.UsedPercent) / 100.0,
			Ultimate:     true,
			Duration:     30 * 24 * time.Hour,
		}
		if il.ResetAfter > 0 {
			w.ResetsAt = now.Add(time.Duration(il.ResetAfter) * time.Second)
		}
		s.Windows = append(s.Windows, w)
	}
	// primary(5h)/secondary(weekly) are token rate-caps of a different unit than
	// the $ spend budget, so they're not marked Short (peak-burn share undefined);
	// their exhaustion is handled reactively via 429. Ultimate = monthly spend.
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s, nil
}

// fetchCodexQuota GETs /backend-api/wham/usage with Bearer + originator.
func fetchCodexQuota(cfg *Config, prov Provider) (*provider.QuotaSnapshot, error) {
	authFile := authFilePath("codex", "oauth_auth")
	p := newCodexOAuthProvider(authFile)
	tok, acct, err := p.token()
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	usageURL := strings.TrimSuffix(prov.OpenAIBaseURL, "/codex") + "/wham/usage"
	req, _ := http.NewRequest("GET", usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("originator", "codex_cli_rs")
	if acct != "" {
		req.Header.Set("ChatGPT-Account-Id", acct)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	return parseCodexQuota(body, acct, "")
}

// parseZhipuQuota parses Zhipu BigModel's /api/monitor/usage/quota/limit body into
// a QuotaSnapshot. Returns (nil, nil) if the body isn't the zhipu quota format
// (caller falls back to the OpenAI model-list display). TIME_LIMIT windows are
// included for display but EXCLUDED from the binding RemainingPct (they're MCP
// tool quota, not LLM tokens).
//
// Zhipu's `percentage` field is the USED percentage (0..100): the existing
// pre-refactor display fed it straight to progressBar/`%d%% used`, and the
// quota/limit fixture corroborates (percentage=40 ⇔ currentValue=40000 /
// usage=100000). We convert to RemainingPct = (100 - percentage) / 100.
func parseZhipuQuota(body []byte, account string) (*provider.QuotaSnapshot, error) {
	var z struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Limits []struct {
				Type          string `json:"type"`
				Unit          int    `json:"unit"`
				Number        int    `json:"number"`
				Percentage    int    `json:"percentage"`
				NextResetTime int64  `json:"nextResetTime"`
				Usage         *int   `json:"usage"`
				CurrentValue  *int   `json:"currentValue"`
				Remaining     *int   `json:"remaining"`
				UsageDetails  []struct {
					ModelCode string `json:"modelCode"`
					Usage     int    `json:"usage"`
				} `json:"usageDetails"`
			} `json:"limits"`
			Level string `json:"level"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &z); err != nil {
		return nil, nil
	}
	if !z.Success || len(z.Data.Limits) == 0 {
		return nil, nil
	}
	s := &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		Account: account,
		Level:   z.Data.Level,
		Plan:    z.Data.Level,
		AsOf:    time.Now(),
	}
	for _, l := range z.Data.Limits {
		w := provider.QuotaWindow{
			Label:        zhipuLimitLabel(l.Type, l.Unit),
			RemainingPct: (100.0 - float64(l.Percentage)) / 100.0,
		}
		if l.Type == "TIME_LIMIT" {
			w.Kind = "time"
			w.DetailLabel = "By MCP tool"
		} else {
			w.Kind = "tokens"
			w.DetailLabel = "By model"
			// TOKENS_LIMIT windows: unit 3 = 5h (immediate rate cap), unit 6 = weekly (total budget).
			switch l.Unit {
			case 3:
				w.Short = true
				w.Duration = 5 * time.Hour
			case 6:
				w.Ultimate = true
				w.Duration = 7 * 24 * time.Hour
			}
		}
		if l.CurrentValue != nil && l.Remaining != nil {
			w.Used = float64(*l.CurrentValue)
			w.Total = float64(*l.CurrentValue + *l.Remaining)
		}
		if l.NextResetTime > 0 {
			w.ResetsAt = time.UnixMilli(l.NextResetTime)
		}
		for _, ud := range l.UsageDetails {
			w.Details = append(w.Details, provider.QuotaDetail{Label: ud.ModelCode, Used: float64(ud.Usage)})
		}
		s.Windows = append(s.Windows, w)
	}
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s, nil
}

// fetchZhipuQuota GETs the zhipu usage_url and returns the parsed snapshot.
func fetchZhipuQuota(cfg *Config, name string, prov Provider, cred *accountCred) (*provider.QuotaSnapshot, error) {
	auth := newAuthProvider(prov.Provider, name, cfg, cred)
	req, _ := http.NewRequest("GET", prov.UsageURL, nil)
	if err := auth.Inject(req); err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	s, _ := parseZhipuQuota(body, "")
	if s == nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "not zhipu quota format"}, nil
	}
	return s, nil
}

// printQuotaSnapshot renders a QuotaSnapshot for the `usage` CLI. Output mirrors
// the pre-refactor per-provider formatters (same bars, percentages, reset
// strings, "By model"/"By MCP tool" detail labels).
func printQuotaSnapshot(s *provider.QuotaSnapshot) {
	for _, w := range s.Windows {
		var bar, pctStr string
		if w.RemainingPct < 0 {
			bar = cGray("n/a")
			pctStr = cGray("unmeasured")
		} else {
			pct := int(w.RemainingPct * 100)
			usedPct := 100 - pct
			bar = progressBar(usedPct, 16)
			pctStr = usageRatioColor(w.RemainingPct, 1, fmt.Sprintf("%d%% used", usedPct))
		}
		resetStr := ""
		if !w.ResetsAt.IsZero() {
			dur := formatDuration(int(time.Until(w.ResetsAt) / time.Second))
			resetStr = cGray(" · resets " + dur + "(at " + formatResetAt(w.ResetsAt.UnixMilli()) + ")")
		}
		fmt.Printf("%s %s  %s%s\n", cDim(pad(w.Label+":", 18)), bar, pctStr, resetStr)
		if w.Total > 0 {
			fmt.Printf("%s %.0f used / %.0f total (%.0f remaining)\n",
				cDim(pad("Usage:", 18)), w.Used, w.Total, w.Total-w.Used)
		}
		if len(w.Details) > 0 && w.DetailLabel != "" {
			parts := make([]string, 0, len(w.Details))
			for _, d := range w.Details {
				parts = append(parts, fmt.Sprintf("%s: %.0f", d.Label, d.Used))
			}
			fmt.Printf("%s %s\n", cDim(pad(w.DetailLabel+":", 18)), cGray(strings.Join(parts, " · ")))
		}
	}
	for _, n := range s.Notes {
		fmt.Println(cDim(pad("", 18)) + n)
	}
}

// showGenericUsage fetches and displays usage from a provider's usageURL.
// Handles Zhipu BigModel's /api/monitor/usage/quota/limit format:
//
//	{data:{limits:[{type:"TOKENS_LIMIT",unit,percentage,nextResetTime}, ...], level}}
//
// unit: 3=5h window, 6=weekly window, 5=monthly time limit.
func showGenericUsage(cfg *Config, provName string, prov Provider, cred *accountCred) {
	auth := newAuthProvider(prov.Provider, provName, cfg, cred)
	req, _ := http.NewRequest("GET", prov.UsageURL, nil)
	if err := auth.Inject(req); err != nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("model-proxy login "+provName))
		return
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(cRed("Error: usage request: " + err.Error()))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", cRed("Error:"), resp.StatusCode, truncate(string(body), 200))
		return
	}
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue(provName)))

	// Try Zhipu BigModel quota format (parseZhipuQuota); if not zhipu, fall
	// through to the OpenAI model-list / raw-JSON fallback below.
	if s, _ := parseZhipuQuota(body, ""); s != nil {
		if s.Level != "" {
			fmt.Printf("%s %s\n", cDim("Level:     "), cMagenta(s.Level))
		}
		printQuotaSnapshot(s)
		return
	}

	// Fallback: check if it's a model list (OpenAI-style).
	var ml struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &ml) == nil && ml.Object == "list" {
		fmt.Printf("%s %d models available\n", cDim("Models:    "), len(ml.Data))
		for _, m := range ml.Data {
			name := m.ID
			fmt.Printf("  %s  %s\n", cCyan(pad(m.ID, 22)), cGray(name))
		}
		return
	}

	// Last resort: print raw JSON fields.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		fmt.Println(cRed("Error: parse usage response: " + err.Error()))
		return
	}
	data := raw
	if d, ok := raw["data"].(map[string]any); ok {
		data = d
	}
	printUsageFields(data, 1)
}

// runVolcengineLogin is the thin wrapper retained for the provider-callback path
// (LoginFn → runVolcengineLoginErr). It prompts the triple on stdin. The real
// implementation lives in runVolcengineLoginWithInput, which writes the plural
// credential pool (<name>_apikeys.json) so repeated logins accumulate accounts,
// each carrying its OWN Ark API Key (Bearer chat) + Volcengine AK/SK
// (V4-signed GetAFPUsage). The account id is the AccessKey (account-level).
func runVolcengineLogin(cfg *Config, provName string, prov Provider) error {
	return runVolcengineLoginWithInput(cfg, provName, prov, "", "", "", "", false)
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

	if _, err := addVolcengineAccount(cfg, provName, prov, accountCred{APIKey: apiKey, AccessKey: ak, SecretKey: sk}, label, replace); err != nil {
		return err
	}
	// Print the confirmation line (label resolved from the freshly-saved pool,
	// which may have been re-sorted by savePool).
	pool, _ := loadPool(provName, prov.Provider)
	fmt.Println(cGreen("✓ Saved account ") + cGray(mask(id)+" ("+labelFor(pool, id)+")"))
	return nil
}

// addVolcengineAccount is the non-printing core for volcengine's AK/SK triple:
// it dedups by id (= AccessKey for volcengine) under the cross-process lock and
// writes the pool. Returns the account id. No stdin, no stdout — symmetric with
// addApikeyAccount; reused by the web layer (Task 12). Volcengine has no
// usage_url validation step (the Ark API Key is validated implicitly by the
// first chat request; the AK/SK are validated lazily by GetAFPUsage on the next
// quota poll). replace=false on an existing id returns "login cancelled" without
// modifying the pool.
func addVolcengineAccount(cfg *Config, name string, prov Provider, cred accountCred, label string, replace bool) (string, error) {
	apiKey := strings.TrimSpace(cred.APIKey)
	ak := strings.TrimSpace(cred.AccessKey)
	sk := strings.TrimSpace(cred.SecretKey)
	if apiKey == "" {
		return "", fmt.Errorf("API key is required")
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
		return savePool(name, pool)
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

// afpWindow is one Agent Plan AFP quota window (5h/daily/weekly/monthly).
type afpWindow struct {
	Quota     float64 `json:"Quota"`
	Used      float64 `json:"Used"`
	ResetTime int64   `json:"ResetTime"` // epoch ms
}

type afpUsage struct {
	PlanType    string    `json:"PlanType"`
	AFPFiveHour afpWindow `json:"AFPFiveHour"`
	AFPDaily    afpWindow `json:"AFPDaily"`
	AFPWeekly   afpWindow `json:"AFPWeekly"`
	AFPMonthly  afpWindow `json:"AFPMonthly"`
}

// getAFPUsage calls the Volcengine signed OpenAPI GetAFPUsage and returns the
// 5h/daily/weekly/monthly AFP quota windows.
func getAFPUsage(ak, sk string) (*afpUsage, error) {
	req, err := volcengineGet("GetAFPUsage", "2024-01-01", ak, sk, time.Now(), "")
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetAFPUsage: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GetAFPUsage HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var wrap struct {
		ResponseMetadata json.RawMessage `json:"ResponseMetadata"`
		Result           afpUsage        `json:"Result"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("parse GetAFPUsage: %w", err)
	}
	return &wrap.Result, nil
}

// showVolcengineUsage shows the Agent Plan's 5h/daily/weekly/monthly AFP quota
// via GetAFPUsage (needs AK/SK + V4 signing). When cred is non-nil the virtual's
// own AK/SK are used (per-account); otherwise the legacy file is read. Falls
// back to config models if AK/SK aren't configured or the call fails.
func showVolcengineUsage(cfg *Config, provName string, prov Provider, cred *accountCred) {
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue(provName)))
	ak, sk, err := resolveVolcengineAKSK(provName, cred)
	if err != nil {
		fmt.Printf("%s Agent Plan 5h/周/月额度需经 GetAFPUsage（火山引擎签名 OpenAPI，AccessKey/SecretKey + V4）。\n", cDim("Note:       "))
		fmt.Printf("%s 用 `model-proxy login %s` 配置 AK/SK（IAM 密钥，非 Ark API Key）后可查询。\n", cDim("            "), provName)
		listConfigModels(prov)
		return
	}
	u, err := getAFPUsage(ak, sk)
	if err != nil {
		fmt.Printf("%s GetAFPUsage failed: %v\n", cDim("Error:      "), err)
		listConfigModels(prov)
		return
	}
	if u.PlanType != "" {
		fmt.Printf("%s %s\n", cDim("Plan:      "), cMagenta(u.PlanType))
	}
	printAFPWindow("5h", u.AFPFiveHour)
	printAFPWindow("Daily", u.AFPDaily)
	printAFPWindow("Weekly", u.AFPWeekly)
	printAFPWindow("Monthly", u.AFPMonthly)
}

func printAFPWindow(label string, w afpWindow) {
	remaining := w.Quota - w.Used
	pct := 0
	if w.Quota > 0 {
		pct = int(w.Used / w.Quota * 100)
	}
	bar := progressBar(pct, 16)
	pctStr := usageRatioColor(float64(100-pct), 100, fmt.Sprintf("%d%% used", pct))
	reset := "—"
	if w.ResetTime > 0 {
		dur := formatDuration(int((w.ResetTime - time.Now().UnixMilli()) / 1000))
		reset = dur + "(at " + formatResetAt(w.ResetTime) + ")"
	}
	fmt.Printf("%s %s  %s · resets %s  (%.1f used / %.1f quota, %.1f remaining)\n",
		cDim(pad(label+":", 12)), bar, pctStr, cGray(reset), w.Used, w.Quota, remaining)
}

// parseVolcengineQuota converts the GetAFPUsage result into a QuotaSnapshot.
func parseVolcengineQuota(u *afpUsage) *provider.QuotaSnapshot {
	s := &provider.QuotaSnapshot{Billing: provider.BillingPlan, Plan: u.PlanType, AsOf: time.Now()}
	add := func(label string, w afpWindow, ultimate, short bool, dur time.Duration) {
		rem := -1.0
		if w.Quota > 0 {
			rem = (w.Quota - w.Used) / w.Quota
			if rem < 0 {
				rem = 0 // over-quota → exhausted (0), not a negative that'd read as "unmeasured"
			}
		}
		var reset time.Time
		if w.ResetTime > 0 {
			reset = time.UnixMilli(w.ResetTime)
		}
		s.Windows = append(s.Windows, provider.QuotaWindow{
			Label: label, Kind: "tokens",
			Used: w.Used, Total: w.Quota, RemainingPct: rem, ResetsAt: reset,
			Ultimate: ultimate, Short: short, Duration: dur,
		})
	}
	add("5h", u.AFPFiveHour, false, true, 5*time.Hour)
	add("daily", u.AFPDaily, false, false, 24*time.Hour)
	add("weekly", u.AFPWeekly, false, false, 7*24*time.Hour)
	add("monthly", u.AFPMonthly, true, false, 30*24*time.Hour)
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s
}

// resolveVolcengineAKSK picks the AccessKey/SecretKey to sign GetAFPUsage with.
// When a cred is supplied (the pool-bound path), its AK/SK are used EXCLUSIVELY
// — the on-disk file is never consulted, preserving per-account isolation (a
// sibling virtual's file must not leak into this account's quota call). An
// incomplete cred returns an error rather than falling back to the file. When
// cred is nil (the single-account / pre-pool path), the legacy
// <name>_apikey.json is read for backward compatibility.
func resolveVolcengineAKSK(name string, cred *accountCred) (ak, sk string, err error) {
	if cred != nil {
		if cred.AccessKey != "" && cred.SecretKey != "" {
			return cred.AccessKey, cred.SecretKey, nil
		}
		return "", "", fmt.Errorf("AK/SK not configured")
	}
	c, err := loadVolcengineCreds(name)
	if err != nil || c.AccessKey == "" || c.SecretKey == "" {
		return "", "", fmt.Errorf("AK/SK not configured")
	}
	return c.AccessKey, c.SecretKey, nil
}

// fetchVolcengineQuota calls GetAFPUsage (signed, AK/SK). When cred is non-nil
// the virtual's own AK/SK are used (per-account); otherwise the legacy file is
// read. Returns BillingUnknown if AK/SK aren't configured or the call fails.
func fetchVolcengineQuota(name string, cred *accountCred) (*provider.QuotaSnapshot, error) {
	ak, sk, err := resolveVolcengineAKSK(name, cred)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "AK/SK not configured"}, nil
	}
	u, err := getAFPUsage(ak, sk)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	return parseVolcengineQuota(u), nil
}

// parseDeepseekQuota parses /user/balance. deepseek is pay-as-you-go: no window,
// RemainingPct unmeasured (-1). Balance kept as a single window for display.
func parseDeepseekQuota(body []byte) *provider.QuotaSnapshot {
	var u struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	s := &provider.QuotaSnapshot{Billing: provider.BillingPayG, RemainingPct: -1, AsOf: time.Now()}
	if err := json.Unmarshal(body, &u); err != nil {
		s.Err = err.Error()
		return s
	}
	if !u.IsAvailable {
		s.Notes = append(s.Notes, "insufficient balance")
	}
	for _, b := range u.BalanceInfos {
		total, _ := strconv.ParseFloat(b.TotalBalance, 64)
		s.Windows = append(s.Windows, provider.QuotaWindow{
			Label: or(b.Currency, "Balance"), Kind: "money",
			Total: total, RemainingPct: -1,
			Details: []provider.QuotaDetail{
				{Label: "granted", Used: atof(b.GrantedBalance)},
				{Label: "topped-up", Used: atof(b.ToppedUpBalance)},
			},
		})
	}
	return s
}

func atof(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

// fetchDeepseekQuota GETs /user/balance.
func fetchDeepseekQuota(cfg *Config, name string, prov Provider, cred *accountCred) (*provider.QuotaSnapshot, error) {
	auth := newAuthProvider(prov.Provider, name, cfg, cred)
	req, _ := http.NewRequest("GET", prov.UsageURL, nil)
	if err := auth.Inject(req); err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	return parseDeepseekQuota(body), nil
}

// parseAqpQuota converts monthly_usage into a single-window plan snapshot.
// aqpMonthlyReset derives the monthly quota reset time (last second of the
// selected month, local time) and the nominal cycle duration from the
// SelectedYear/SelectedMonth the monthly_usage endpoint returns. Falls back to
// the current month when the API omits them (zero values). The reset time is
// required: without it the surplus guard in provider.QuotaSnapshot.Surplus()
// (ult.ResetsAt.IsZero()) short-circuits to 0, so aqp could never be
// prioritized for being under pace — it would only beat over-pace providers.
func aqpMonthlyReset(year, month int) (resetsAt time.Time, duration time.Duration) {
	if year == 0 || month == 0 {
		now := time.Now()
		if year == 0 {
			year = now.Year()
		}
		if month == 0 {
			month = int(now.Month())
		}
	}
	cycleStart := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.Local)
	// Day 0 of next month = last day of this month, at 23:59:59 local.
	resetsAt = time.Date(year, time.Month(month)+1, 0, 23, 59, 59, 0, time.Local)
	return resetsAt, resetsAt.Sub(cycleStart)
}

func parseAqpQuota(mu *MonthlyProjectUsage, account string) *provider.QuotaSnapshot {
	s := &provider.QuotaSnapshot{Billing: provider.BillingPlan, Account: account, Plan: mu.Plan, AsOf: time.Now()}
	rem := -1.0
	if mu.TotalAmount > 0 {
		rem = mu.Balance / mu.TotalAmount
	}
	resetsAt, dur := aqpMonthlyReset(mu.SelectedYear, mu.SelectedMonth)
	s.Windows = append(s.Windows, provider.QuotaWindow{
		Label: "Monthly", Kind: "money",
		Used: mu.Usage, Total: mu.TotalAmount, RemainingPct: rem,
		Ultimate: true, Duration: dur, ResetsAt: resetsAt,
	})
	s.RemainingPct = rem
	return s
}

// fetchAqpQuota mints the AQP key + POSTs monthly_usage.
func fetchAqpQuota(cfg *Config) (*provider.QuotaSnapshot, error) {
	path := authFilePath("aqp", "oauth_auth")
	a, _ := loadAccount(path)
	c := newAqpClient(path)
	mu, err := c.MonthlyUsage()
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	acct := ""
	if a != nil {
		acct = a.Email
	}
	return parseAqpQuota(mu, acct), nil
}

func listConfigModels(prov Provider) {
	ids := make([]string, 0, len(prov.Models))
	for id := range prov.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Printf("%s %d models (from config)\n", cDim("Models:     "), len(ids))
	for _, id := range ids {
		fmt.Printf("  %s\n", cCyan(id))
	}
}

// showDeepseekUsage fetches and displays the DeepSeek account balance from
// /user/balance: {is_available, balance_infos:[{currency, total_balance,
// granted_balance, topped_up_balance}]}. Auth is Bearer (the balance endpoint is
// OpenAI-style, under the OpenAI base).
func showDeepseekUsage(cfg *Config, provName string, prov Provider, cred *accountCred) {
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue(provName)))
	auth := newAuthProvider(prov.Provider, provName, cfg, cred)
	req, _ := http.NewRequest("GET", prov.UsageURL, nil)
	if err := auth.Inject(req); err != nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("model-proxy login "+provName))
		return
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(cRed("Error: usage request: " + err.Error()))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", cRed("Error:"), resp.StatusCode, truncate(string(body), 200))
		return
	}
	var u struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		fmt.Println(cRed("Error: parse usage response: " + err.Error()))
		return
	}
	if u.IsAvailable {
		fmt.Printf("%s %s\n", cDim("Available:  "), cGreen("yes"))
	} else {
		fmt.Printf("%s %s\n", cDim("Available:  "), cRed("no (insufficient balance)"))
	}
	for _, b := range u.BalanceInfos {
		cur := b.Currency
		if cur == "" {
			cur = "Balance"
		}
		fmt.Printf("%s %s  %s\n",
			cDim(pad(cur+":", 12)),
			cBold(cCyan(b.TotalBalance)),
			cGray("(granted "+b.GrantedBalance+", topped-up "+b.ToppedUpBalance+")"))
	}
}

// zhipuLimitLabel converts Zhipu's type+unit to a human-readable label.
func zhipuLimitLabel(typ string, unit int) string {
	switch typ {
	case "TOKENS_LIMIT":
		switch unit {
		case 3:
			return "5h tokens"
		case 6:
			return "Weekly tokens"
		}
		return fmt.Sprintf("Tokens (unit=%d)", unit)
	case "TIME_LIMIT":
		switch unit {
		case 5:
			return "Monthly time"
		}
		return fmt.Sprintf("Time (unit=%d)", unit)
	}
	return fmt.Sprintf("%s (unit=%d)", typ, unit)
}

// printUsageFields recursively prints JSON fields with indentation.
func printUsageFields(m map[string]any, indent int) {
	prefix := strings.Repeat("  ", indent)
	// Collect and sort keys for stable output.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		switch val := v.(type) {
		case string:
			fmt.Printf("%s%s %s\n", cDim(pad(k+":", 18)), prefix, cGray(val))
		case float64:
			fmt.Printf("%s%s %s\n", cDim(pad(k+":", 18)), prefix, cCyan(fmt.Sprintf("%v", val)))
		case bool:
			fmt.Printf("%s%s %s\n", cDim(pad(k+":", 18)), prefix, cCyan(fmt.Sprintf("%v", val)))
		case map[string]any:
			fmt.Printf("%s%s %s\n", cDim(pad(k+":", 18)), prefix, cBold(""))
			printUsageFields(val, indent+1)
		default:
			fmt.Printf("%s%s %s\n", cDim(pad(k+":", 18)), prefix, cGray(fmt.Sprintf("%v", val)))
		}
	}
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// formatDuration converts seconds to a compact human-readable string (e.g. "5h", "7d3h").
func formatDuration(secs int) string {
	if secs <= 0 {
		return "—"
	}
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd%dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// formatResetAt formats a reset time (epoch ms) for display: if it falls on
// today's date, only HH:MM; otherwise MM-DD HH:MM.
func formatResetAt(resetMs int64) string {
	t := time.UnixMilli(resetMs).Local()
	if t.Format("20060102") == time.Now().Format("20060102") {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// formatCredits formats a credit amount string (e.g. "330.258..." → "330",
// "22500" → "22,500"). Truncates decimals, adds thousands separators.
func formatCredits(s string) string {
	f := 0.0
	fmt.Sscanf(s, "%f", &f)
	return formatWithCommas(int(f))
}

// formatWithCommas adds thousands separators to an integer.
func formatWithCommas(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return "-" + formatWithCommas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// progressBar renders a [██░░░] bar of given width, colored by remaining ratio.
// pct is the used percentage (0-100). The filled portion uses ratio coloring
// (green < 50%, yellow < 80%, red >= 80%), empty portion is dim.
func progressBar(pct, width int) string {
	if width < 4 {
		width = 4
	}
	filled := pct * width / 100
	if filled > width {
		filled = width
	}
	bar := ""
	for i := 0; i < width; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	return usageRatioColor(float64(100-pct), 100, "["+bar+"]")
}

// money formats v as $X.XX
func money(v float64) string {
	return fmt.Sprintf("$%.2f", v)
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
			s.threshold(), s.cooldown(), s.rateBackoff(), s.timeout(), s.dwell())
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
	resp, err := http.Get("http://" + cfg.Listen + "/debug/schedule")
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

	s := cfg.Scheduling
	fmt.Printf("\n%s\n", cBold("Scheduling"))
	fmt.Printf("  sticky_dwell=%s  quota_poll_interval=%s  quota_switch_margin=%d pts  circuit=(threshold %d, cooldown %s)\n",
		s.dwell(), s.pollInterval(), s.QuotaSwitchMargin, s.threshold(), s.cooldown())
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
			mult = defaultPeakMultiplier
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
