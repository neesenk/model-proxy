package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const usage = `model-proxy — standalone portable proxy for AIS Switch LLM gateway

Usage: model-proxy <COMMAND> [SUBCOMMAND] [OPTIONS]

Commands:
  serve                Start the proxy server
  serve daemon         Run in background (auto-restart on crash)
  serve stop           Stop a running daemon
  serve reload         Hot-reload config (SIGHUP the running daemon)
  takeover <client>    Rewrite client config to point at the proxy
  restore <client>     Restore client config from backup
  login <provider>     Login to a provider (compass | codex)
  logout <provider>    Clear provider credentials
  usage <provider>     Show usage / credits for a provider
  models               List models from all providers (from config)
  models <provider>    List models for one provider
  models refresh <provider>  Fetch live model list from a provider
  config init          Generate a config.yaml template
  config print         Print the effective config
  config check         Validate config and print a summary
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

func cmdLogout(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := positional(args)
	if provName == "" {
		fmt.Println("usage: model-proxy logout <provider>")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (provider=%s)\n", name, p.Provider)
		}
		return
	}
	p := buildProviders(cfg)[provName]
	if p == nil {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	if err := p.Logout(); err != nil {
		log.Fatalf("logout failed: %v", err)
	}
	fmt.Println(cGreen("✓ Logged out"))
}

func cmdUsage(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := positional(args)
	if provName == "" {
		// No provider specified → show usage for all logged-in providers.
		pv := buildProviders(cfg)
		names := make([]string, 0, len(cfg.Providers))
		for n := range cfg.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := pv[n]
			if p == nil {
				continue
			}
			fmt.Println(cDim("────────────────────────────────────────"))
			if _, err := p.Usage(); err != nil {
				fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
			}
			fmt.Println()
		}
		return
	}
	p := buildProviders(cfg)[provName]
	if p == nil {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	if _, err := p.Usage(); err != nil {
		log.Fatalf("usage failed: %v", err)
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
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue("compass")))
	path := authFilePath("compass", "oauth_auth")
	a, err := loadAccount(path)
	if err != nil {
		fmt.Println(cRed("Error: " + err.Error()))
		return
	}
	if a == nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("model-proxy login compass"))
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

// showGenericUsage fetches and displays usage from a provider's usageURL.
// Handles Zhipu BigModel's /api/monitor/usage/quota/limit format:
//
//	{data:{limits:[{type:"TOKENS_LIMIT",unit,percentage,nextResetTime}, ...], level}}
//
// unit: 3=5h window, 6=weekly window, 5=monthly time limit.
func showGenericUsage(cfg *Config, provName string, prov Provider) {
	auth := newAuthProvider(prov.Provider, provName, cfg)
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

	// Try Zhipu BigModel quota format (usageURL → /api/monitor/usage/quota/limit):
	//   {code, msg, success, data:{limits:[{type,unit,number,percentage,nextResetTime,
	//     usage(=total), currentValue(=used), remaining, usageDetails:[{modelCode,usage}]}, ...], level}}
	// type: TOKENS_LIMIT | TIME_LIMIT; unit: 3=5h, 6=weekly (tokens), 5=monthly (time).
	var zhipu struct {
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
	if json.Unmarshal(body, &zhipu) == nil && zhipu.Success && len(zhipu.Data.Limits) > 0 {
		if zhipu.Data.Level != "" {
			fmt.Printf("%s %s\n", cDim("Level:     "), cMagenta(zhipu.Data.Level))
		}
		for _, l := range zhipu.Data.Limits {
			label := zhipuLimitLabel(l.Type, l.Unit)
			pct := l.Percentage
			bar := progressBar(pct, 16)
			pctStr := usageRatioColor(float64(100-pct), 100, fmt.Sprintf("%d%% used", pct))
			resetStr := ""
			if l.NextResetTime > 0 {
				dur := formatDuration(int((l.NextResetTime - time.Now().UnixMilli()) / 1000))
				resetStr = cGray(" · resets " + dur + "(at " + formatResetAt(l.NextResetTime) + ")")
			}
			fmt.Printf("%s %s  %s%s\n", cDim(pad(label+":", 18)), bar, pctStr, resetStr)
			// Usage detail: currentValue = used this period, remaining = left,
			// total = currentValue + remaining (== usage).
			// TIME_LIMIT is the tool / value-added-service quota, consumed by MCP tools
			// (search-prime, web-reader, zread, ...) — label it as tool usage. TOKENS_LIMIT
			// is LLM token usage, broken down by model.
			detailLabel := "Usage"
			breakdownLabel := "By model"
			if l.Type == "TIME_LIMIT" {
				detailLabel = "Tool usage"
				breakdownLabel = "By MCP tool"
			}
			if l.CurrentValue != nil && l.Remaining != nil {
				total := *l.CurrentValue + *l.Remaining
				fmt.Printf("%s %d used / %d total (%d remaining)\n",
					cDim(pad(detailLabel+":", 18)), *l.CurrentValue, total, *l.Remaining)
			}
			// Per-item consumption breakdown, when the API provides it.
			if len(l.UsageDetails) > 0 {
				parts := make([]string, 0, len(l.UsageDetails))
				for _, ud := range l.UsageDetails {
					parts = append(parts, fmt.Sprintf("%s: %d", ud.ModelCode, ud.Usage))
				}
				fmt.Printf("%s %s\n", cDim(pad(breakdownLabel+":", 18)), cGray(strings.Join(parts, " · ")))
			}
		}
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

// runVolcengineLogin prompts for the Ark API Key (chat) AND the Volcengine
// AccessKey/SecretKey (for GetAFPUsage), saving all three to the apikey file.
func runVolcengineLogin(cfg *Config, provName string, prov Provider) error {
	fmt.Printf("Ark API Key (对话用，控制台创建): ")
	var apiKey string
	fmt.Scanln(&apiKey)
	fmt.Printf("Volcengine Access Key ID (GetAFPUsage 用，IAM 密钥): ")
	var ak string
	fmt.Scanln(&ak)
	fmt.Printf("Volcengine Secret Access Key: ")
	var sk string
	fmt.Scanln(&sk)
	if apiKey == "" {
		return fmt.Errorf("API key is required")
	}
	data, _ := json.Marshal(map[string]string{
		"api_key":    apiKey,
		"access_key": ak,
		"secret_key": sk,
	})
	path := filepath.Join(homeDir(), ".model-proxy", provName+"_apikey.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
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
// via GetAFPUsage (needs AK/SK + V4 signing). Falls back to config models if
// AK/SK aren't configured or the call fails.
func showVolcengineUsage(cfg *Config, provName string, prov Provider) {
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue(provName)))
	creds, err := loadVolcengineCreds(provName)
	if err != nil || creds.AccessKey == "" || creds.SecretKey == "" {
		fmt.Printf("%s Agent Plan 5h/周/月额度需经 GetAFPUsage（火山引擎签名 OpenAPI，AccessKey/SecretKey + V4）。\n", cDim("Note:       "))
		fmt.Printf("%s 用 `model-proxy login %s` 配置 AK/SK（IAM 密钥，非 Ark API Key）后可查询。\n", cDim("            "), provName)
		listConfigModels(prov)
		return
	}
	u, err := getAFPUsage(creds.AccessKey, creds.SecretKey)
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
func showDeepseekUsage(cfg *Config, provName string, prov Provider) {
	fmt.Printf("%s %s\n", cDim("Provider:  "), cBold(cBlue(provName)))
	auth := newAuthProvider(prov.Provider, provName, cfg)
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
