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

const usage = `ais-switch-proxy — standalone portable proxy for AIS Switch LLM gateway

USAGE
  ais-switch-proxy <command> [options]

COMMANDS
  serve [--daemon] [--log-file F]    Start the proxy server
    --daemon                           Run in background (auto-restart on crash)
    --log-file F                       Log file path (default: $TMPDIR/ais-switch-proxy.log)

  stop                               Stop a running --daemon

  takeover <client>                  Rewrite client config to point at the proxy
    client: claude | opencode | codex | pi | all

  restore <client>                   Restore client config from backup

  login <provider> [--import]        Login to a provider
    compass                            SSO browser login (or --import from desktop app)
    codex                              OAuth device flow

  logout <provider>                  Clear provider credentials

  usage <provider>                   Show usage / credits for a provider
    compass                            Account, monthly usage, balance
    codex                              Credits, rate limits, spend

  mint-key                           Mint and print a CQP key

  models [--refresh]                 List available models

  import-pricing [--db FILE]         Export pricing table from AIS Switch's DB

  config init                        Generate a config.yaml template
  config print                       Print the effective config

COMMON OPTIONS
  --config PATH                      Config file (default lookup: ~/.ais-switch/config.yaml > ./config.yaml)

ENVIRONMENT
  AIS_SSO_COOKIE                     Override sso_cookie_file
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
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("ais-switch-proxy login compass"))
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
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("ais-switch-proxy login codex"))
		return
	}
	// Usage endpoint is at /backend-api/wham/usage, NOT under the codex base
	// (/backend-api/codex/wham/usage returns 403). Derive the backend-api root
	// from the provider baseURL by stripping the trailing /codex segment.
	usageURL := strings.TrimSuffix(prov.BaseURL, "/codex") + "/wham/usage"
	req, _ := http.NewRequest("GET", usageURL, nil)
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
		Credits  *struct {
			HasCredits bool    `json:"has_credits"`
			Unlimited  bool    `json:"unlimited"`
			Balance    *string `json:"balance"`
		} `json:"credits"`
		RateLimit *struct {
			Allowed       bool `json:"allowed"`
			LimitReached  bool `json:"limit_reached"`
			PrimaryWindow *struct {
				UsedPercent       int   `json:"used_percent"`
				LimitWindowSecs   int   `json:"limit_window_seconds"`
				ResetAfterSecs    int   `json:"reset_after_seconds"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent     int   `json:"used_percent"`
				LimitWindowSecs int   `json:"limit_window_seconds"`
				ResetAfterSecs  int   `json:"reset_after_seconds"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		SpendControl *struct {
			Reached          bool `json:"reached"`
			IndividualLimit  *struct {
				Used         string `json:"used"`
				Limit        string `json:"limit"`
				Remaining    string `json:"remaining"`
				UsedPercent  int    `json:"used_percent"`
				ResetAfter   int    `json:"reset_after_seconds"`
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
	fmt.Printf("%s %s\n", cDim("Provider:  "), cGray("codex (chatgpt.com)"))
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
