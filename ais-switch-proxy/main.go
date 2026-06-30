package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

const usage = `ais-switch-proxy — standalone, portable reimplementation of the AIS Switch local proxy

Usage:
  ais-switch-proxy serve   [--config config.yaml] [--daemon] [--log-file FILE]  Start the local proxy
    --daemon        Run detached (supervisor/worker): supervisor monitors the worker
                    and auto-restarts it on crash. Logs go to the configured log file.
    --log-file FILE Override the log file path (default: log_file from config).
  ais-switch-proxy takeover [--config config.yaml] [client]  Rewrite client config to point at the proxy
  ais-switch-proxy restore  [--config config.yaml] [client]  Restore client config from backup
  ais-switch-proxy login    [--config config.yaml]       Compass SSO login, writes sso_cookie_file
  ais-switch-proxy login --import [--config config.yaml] Import SSO cookie from the AIS Switch desktop app
  ais-switch-proxy logout   [--config config.yaml]       Clear sso_cookie_file (log out)
  ais-switch-proxy status   [--config config.yaml]       Show logged-in account / monthly usage
  ais-switch-proxy mint-key [--config config.yaml]       Mint and print a CQP key (for direct mode)
  ais-switch-proxy models  [--config config.yaml] [--refresh]  List gateway models (cached, refreshes on schedule)
  ais-switch-proxy import-pricing [--config config.yaml] [--db FILE]  Export pricing from AIS Switch's cc-switch.db
  ais-switch-proxy config init                            Generate a config.yaml template
  ais-switch-proxy config print [--config config.yaml]   Print the effective config

client: claude | opencode | codex | pi | all (default all)

Environment:
  AIS_SWITCH_PROXY_CONFIG  Config file path (overrides --config)
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
	case "takeover":
		cmdTakeover(os.Args[2:])
	case "restore":
		cmdRestore(os.Args[2:])
	case "login":
		cmdLogin(os.Args[2:])
	case "logout":
		cmdLogout(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
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

// configPath scans --config / -config / --config= manually and ignores other
// flags (e.g. login's --import) so flag.Parse doesn't choke on unknown flags.
// AIS_SWITCH_PROXY_CONFIG overrides --config.
func configPath(args []string) string {
	if env := os.Getenv("AIS_SWITCH_PROXY_CONFIG"); env != "" {
		return env
	}
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
	return "config.yaml"
}

func routeNames(cfg *Config) string {
	out := ""
	for i, r := range cfg.Routes {
		if i > 0 {
			out += ", "
		}
		out += r.Name + "("
		for j, p := range r.PathPrefixes {
			if j > 0 {
				out += "|"
			}
			out += p
		}
		out += ")"
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
	path := cfg.Auth.SSOCookieFile
	if path == "" {
		log.Fatal("auth.sso_cookie_file not set in config")
	}
	if err := clearAccount(path); err != nil {
		log.Fatal(err)
	}
	fmt.Println(cGreen("Logged out") + " (cleared " + cGray(path) + ").")
}

func cmdStatus(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
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
		for _, r := range cfg.Routes {
			fmt.Printf("route %s: %v → %s (auth=%s, %d model maps)\n", r.Name, r.PathPrefixes, r.Upstream, r.Auth, len(r.ModelMap))
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
