package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `ais-switch-proxy — standalone, portable reimplementation of the AIS Switch local proxy

Usage:
  ais-switch-proxy serve   [--config config.yaml] [--daemon] [--log-file FILE]  Start the local proxy
    --daemon        Run detached (supervisor/worker): supervisor monitors the worker
                    and auto-restarts it on crash. Logs go to the configured log file.
    --log-file FILE Override the log file path (default: log_file from config).
  ais-switch-proxy stop    [--config config.yaml] [--log-file FILE]  Stop a running --daemon (SIGTERM the supervisor)
  ais-switch-proxy takeover [--config config.yaml] [client]  Rewrite client config to point at the proxy
  ais-switch-proxy restore  [--config config.yaml] [client]  Restore client config from backup
  ais-switch-proxy login <provider> [--config config.yaml] [--import]  Login (compass: SSO, codex: device flow)
  ais-switch-proxy logout <provider> [--config config.yaml]  Logout (clear provider credentials)
  ais-switch-proxy usage <provider> [--config config.yaml]  Show usage for a provider (compass | codex)
  ais-switch-proxy mint-key [--config config.yaml]       Mint and print a CQP key (for direct mode)
  ais-switch-proxy models  [--config config.yaml] [--refresh]  List gateway models (cached, refreshes on schedule)
  ais-switch-proxy import-pricing [--config config.yaml] [--db FILE]  Export pricing from AIS Switch's cc-switch.db
  ais-switch-proxy config init                            Generate a config.yaml template
  ais-switch-proxy config print [--config config.yaml]   Print the effective config

client: claude | opencode | codex | pi | all (default all)

Config lookup order: --config PATH > ~/.ais-switch/config.yaml > ./config.yaml

Environment:
  AIS_SSO_COOKIE    Full SSO cookie string (overrides sso_cookie_file, handy on Linux)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "stop":
		cmdStop(os.Args[2:])
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
	case "mint-key":
		cmdMintKey(os.Args[2:])
	case "models":
		cmdModels(os.Args[2:])
	case "import-pricing":
		cmdImportPricing(os.Args[2:])
	case "config":
		cmdConfig(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
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
//	1. --config PATH flag            (explicit)
//	2. ~/.ais-switch/config.yaml   (user-level, shared across CWDs)
//	3. ./config.yaml                 (current directory)
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
	// 2. user-level config under ~/.ais-switch/
	home := homeDirForTest
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home != "" {
		p := filepath.Join(home, ".ais-switch", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 3. current directory
	return "config.yaml"
}

func routeNames(cfg *Config) string {
	out := ""
	first := true
	for proto := range cfg.Routes {
		if !first {
			out += ", "
		}
		first = false
		out += proto
	}
	return out
}

func cmdTakeover(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	which := positional(args)
	if err := runTakeover(cfg, which); err != nil {
		log.Fatal(err)
	}
}

func cmdRestore(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	which := positional(args)
	if err := runRestore(cfg, which); err != nil {
		log.Fatal(err)
	}
}

func cmdMintKey(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	p := newCQPProvider(cfg.Auth)
	key, err := p.key()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(cCyan(key))
}

func cmdLogout(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := positional(args)
	if provName == "" {
		fmt.Println("usage: ais-switch-proxy logout <provider>")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (auth=%s)\n", name, p.Auth)
		}
		return
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	switch prov.Auth {
	case "cqp":
		path := cfg.Auth.SSOCookieFile
		if path == "" {
			log.Fatal("auth.sso_cookie_file not set in config")
		}
		if err := clearAccount(path); err != nil {
			log.Fatal(err)
		}
		fmt.Println(cGreen("Logged out") + " (cleared " + cGray(path) + ").")
	case "codex_oauth":
		path := cfg.Auth.CodexAuthFile
		if path == "" {
			path = "~/.ais-switch/codex_oauth_auth.json"
		}
		path = expandPath(path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Fatal(err)
		}
		fmt.Println(cGreen("Logged out") + " (cleared " + cGray(path) + ").")
	default:
		log.Fatalf("logout not supported for provider %q (auth=%s)", provName, prov.Auth)
	}
}

func cmdUsage(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	// First positional arg is the provider name.
	provName := positional(args)
	if provName == "" {
		// List available providers.
		fmt.Println("usage: ais-switch-proxy usage <provider>")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (auth=%s)\n", name, p.Auth)
		}
		return
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	switch prov.Auth {
	case "cqp":
		showCompassUsage(cfg)
	case "codex_oauth":
		showCodexUsage(cfg, prov)
	default:
		log.Fatalf("usage not supported for provider %q (auth=%s)", provName, prov.Auth)
	}
}

func providerNames(cfg *Config) string {
	names := []string{}
	for n := range cfg.Providers {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

func showCompassUsage(cfg *Config) {
	path := cfg.Auth.SSOCookieFile
	if path == "" {
		log.Fatal("auth.sso_cookie_file not set in config")
	}
	a, err := loadAccount(path)
	if err != nil {
		log.Fatal(err)
	}
	if a == nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("ais-switch-proxy login"))
		return
	}
	c := newCompassClient(path)
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
	authFile := cfg.Auth.CodexAuthFile
	if authFile == "" {
		authFile = "~/.ais-switch/codex_oauth_auth.json"
	}
	p := newCodexOAuthProvider(expandPath(authFile))
	tok, acct, err := p.token()
	if err != nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("ais-switch-proxy codex-login"))
		return
	}
	req, _ := http.NewRequest("GET", prov.BaseURL+"/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("originator", "codex_cli_rs")
	if acct != "" {
		req.Header.Set("ChatGPT-Account-Id", acct)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		log.Fatalf("usage request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Fatalf("usage HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var u struct {
		Email    string `json:"email"`
		PlanType string `json:"plan_type"`
	}
	json.Unmarshal(body, &u)
	fmt.Printf("%s %s\n", cDim("Account:   "), cBold(cCyan(or(u.Email, "(unknown)"))))
	fmt.Printf("%s %s\n", cDim("Plan:      "), cMagenta(or(u.PlanType, "(unknown)")))
	fmt.Printf("%s %s\n", cDim("Provider:  "), cGray("codex (chatgpt.com)"))
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// money formats v as $X.XX
func money(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

func cmdConfig(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: ais-switch-proxy config [init|print]")
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
		fmt.Printf("auth: cqp_mint_url=%s sso_cookie_file=%s\n", cfg.Auth.CQPMintURL, cfg.Auth.SSOCookieFile)
		for name, prov := range cfg.Providers {
			fmt.Printf("provider %s: baseURL=%s auth=%s (%d models)\n", name, prov.BaseURL, prov.Auth, len(prov.Models))
		}
		for proto, route := range cfg.Routes {
			fmt.Printf("route %s: %d model maps\n", proto, len(route.Models))
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand: %s\n", args[0])
		os.Exit(1)
	}
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
