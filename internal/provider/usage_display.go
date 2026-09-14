package provider

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/display"
	"sort"
	"strings"
	"time"
)

// usage_display.go holds the per-provider usage display (Usage() methods) +
// shared display helpers, moved from main.go in Phase 3. Each Usage() prints to
// stdout (side-effecting, returns (nil, nil) on success) and starts with the
// "Provider:  <name>" line (the usage-display contract).

// --- shared display helpers ---

// printQuotaSnapshot renders a QuotaSnapshot for the `usage` CLI. Output mirrors
// the per-provider formatters (bars, percentages, reset strings, detail labels).
func printQuotaSnapshot(s *QuotaSnapshot) {
	for _, w := range s.Windows {
		line := formatQuotaWindowLine(w)
		if w.Ultimate {
			line += exhaustionHint(s.ExhaustionEta, time.Now())
		}
		fmt.Println(line)
		if w.Total > 0 {
			fmt.Printf("%s %.0f used / %.0f total (%.0f remaining)\n",
				display.Dim(display.Pad("Usage:", 18)), w.Used, w.Total, w.Total-w.Used)
		}
		if len(w.Details) > 0 && w.DetailLabel != "" {
			parts := make([]string, 0, len(w.Details))
			for _, d := range w.Details {
				parts = append(parts, fmt.Sprintf("%s: %.0f", d.Label, d.Used))
			}
			fmt.Printf("%s %s\n", display.Dim(display.Pad(w.DetailLabel+":", 18)), display.Gray(strings.Join(parts, " · ")))
		}
	}
	for _, n := range s.Notes {
		fmt.Println(display.Dim(display.Pad("", 18)) + n)
	}
}

// formatQuotaWindowLine renders the shared "<label>: [<bar>] <pct> · resets
// <dur>(at <time>)" line used by every provider's usage display. Returns the
// full line without a trailing newline. Shared so kimi-code (which omits the
// absolute Usage line for its 0-100 token windows) renders the bar identically
// to printQuotaSnapshot.
func formatQuotaWindowLine(w QuotaWindow) string {
	var bar, pctStr string
	if w.RemainingPct < 0 {
		bar = display.Gray("n/a")
		pctStr = display.Gray("unmeasured")
	} else {
		usedPct := (1 - w.RemainingPct) * 100
		bar = display.ProgressBar(int(usedPct), 16)
		pctStr = display.UsageRatioColor(w.RemainingPct, 1, fmt.Sprintf("%.1f%% used", usedPct))
	}
	resetStr := ""
	if !w.ResetsAt.IsZero() {
		dur := display.FormatDuration(int(time.Until(w.ResetsAt) / time.Second))
		resetStr = display.Gray(" · resets " + dur + "(at " + display.FormatResetAt(w.ResetsAt.UnixMilli()) + ")")
	}
	return fmt.Sprintf("%s %s  %s%s", display.Dim(display.Pad(w.Label+":", 18)), bar, pctStr, resetStr)
}

// printAFPWindow renders one Volcengine AFP quota window.
func printAFPWindow(label string, w AfpWindow) {
	remaining := w.Quota - w.Used
	pct := 0.0
	if w.Quota > 0 {
		pct = w.Used / w.Quota * 100
	}
	bar := display.ProgressBar(int(pct), 16)
	pctStr := display.UsageRatioColor(float64(100-pct), 100, fmt.Sprintf("%.1f%% used", pct))
	reset := "-"
	if w.ResetTime > 0 {
		dur := display.FormatDuration(int((w.ResetTime - time.Now().UnixMilli()) / 1000))
		reset = dur + "(at " + display.FormatResetAt(w.ResetTime) + ")"
	}
	fmt.Printf("%s %s  %s · resets %s  (%.1f used / %.1f quota, %.1f remaining)\n",
		display.Dim(display.Pad(label+":", 12)), bar, pctStr, display.Gray(reset), w.Used, w.Quota, remaining)
}

// aqpUsageLine renders the aqp monthly-usage line (content after "Usage:"):
// a progress bar + "<PCT>% used" prefix, then usage/total + balance/plan/date.
func aqpUsageLine(mu *MonthlyProjectUsage) string {
	pct := 0.0
	if mu.TotalAmount > 0 {
		pct = (mu.Usage / mu.TotalAmount) * 100
	}
	bar := display.ProgressBar(int(pct), 10)
	pctStr := display.UsageRatioColor(mu.Balance, mu.TotalAmount, fmt.Sprintf("%.1f%% used", pct))
	return fmt.Sprintf("%s %s · %s / %s  (%s %s, %s, %d-%02d)",
		bar, pctStr,
		display.UsageRatioColor(mu.Balance, mu.TotalAmount, display.Money(mu.Usage)),
		display.Gray(display.Money(mu.TotalAmount)),
		display.Dim("balance"), display.UsageRatioColor(mu.Balance, mu.TotalAmount, display.Money(mu.Balance)),
		display.Magenta(mu.Plan), mu.SelectedYear, mu.SelectedMonth)
}

// listConfigModels prints the config model-id list (fallback when a provider
// can't fetch a structured quota).
func listConfigModels(models []string) {
	ids := append([]string(nil), models...)
	sort.Strings(ids)
	fmt.Printf("%s %d models (from config)\n", display.Dim("Models:     "), len(ids))
	for _, id := range ids {
		fmt.Printf("  %s\n", display.Cyan(id))
	}
}

// printUsageFields recursively prints JSON fields with indentation (last-resort
// raw-JSON display).
func printUsageFields(m map[string]any, indent int) {
	prefix := strings.Repeat("  ", indent)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		switch val := v.(type) {
		case string:
			fmt.Printf("%s%s %s\n", display.Dim(display.Pad(k+":", 18)), prefix, display.Gray(val))
		case float64:
			fmt.Printf("%s%s %s\n", display.Dim(display.Pad(k+":", 18)), prefix, display.Cyan(fmt.Sprintf("%v", val)))
		case bool:
			fmt.Printf("%s%s %s\n", display.Dim(display.Pad(k+":", 18)), prefix, display.Cyan(fmt.Sprintf("%v", val)))
		case map[string]any:
			fmt.Printf("%s%s %s\n", display.Dim(display.Pad(k+":", 18)), prefix, display.Bold(""))
			printUsageFields(val, indent+1)
		default:
			fmt.Printf("%s%s %s\n", display.Dim(display.Pad(k+":", 18)), prefix, display.Gray(fmt.Sprintf("%v", val)))
		}
	}
}

// --- per-provider Usage() ---

func (p *AqpProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue("aqp")))
	a, err := LoadAqpAccount(p.cfg.OAuthAuthFile)
	if err != nil {
		fmt.Println(display.Red("Error: " + err.Error()))
		return nil
	}
	if a == nil || a.Email == "" {
		fmt.Println(display.Yellow("Not logged in.") + " Run: " + display.Cyan("model-proxy login aqp"))
		return nil
	}
	fmt.Printf("%s %s\n", display.Dim("Account:    "), display.Bold(display.Cyan(a.Email)))
	fmt.Printf("%s %s\n", display.Dim("Project ID: "), display.Gray(a.ProjectID))
	mu, err := p.fetchMonthlyUsage()
	if err != nil {
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Red("(unavailable: "+err.Error()+")"))
	} else {
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), aqpUsageLine(mu))
	}
	fmt.Printf("%s %s\n", display.Dim("Store:      "), display.Gray(p.cfg.OAuthAuthFile))
	return nil
}

func (p *CodexProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue("codex")))
	usageURL := strings.TrimSuffix(p.cfg.OpenAIBaseURL, "/codex") + "/wham/usage"
	body, ok := usageGetForDisplay(usageURL, "codex", p.AuthHeaders, nil)
	if !ok {
		return nil
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
	fmt.Printf("%s %s\n", display.Dim("Account:   "), display.Bold(display.Cyan(display.Or(u.Email, "(unknown)"))))
	fmt.Printf("%s %s\n", display.Dim("Plan:      "), display.Magenta(display.Or(u.PlanType, "(unknown)")))
	if u.Credits != nil {
		if u.Credits.Unlimited {
			fmt.Printf("%s %s\n", display.Dim("Credits:   "), display.Green("unlimited"))
		} else if u.Credits.HasCredits {
			bal := "available"
			if u.Credits.Balance != nil && *u.Credits.Balance != "" {
				bal = *u.Credits.Balance
			}
			fmt.Printf("%s %s\n", display.Dim("Credits:   "), display.Green("has credits ("+bal+")"))
		} else {
			fmt.Printf("%s %s\n", display.Dim("Credits:   "), display.Red("none"))
		}
	}
	if u.RateLimit != nil {
		status := display.Green("allowed")
		if u.RateLimit.LimitReached {
			status = display.Red("limit reached")
		} else if !u.RateLimit.Allowed {
			status = display.Yellow("not allowed")
		}
		fmt.Printf("%s %s\n", display.Dim("Rate Limit:"), status)
		if u.RateLimit.PrimaryWindow != nil {
			pw := u.RateLimit.PrimaryWindow
			fmt.Printf("%s %s\n", display.Dim("  primary:  "),
				display.UsageRatioColor(float64(100-pw.UsedPercent), 100, fmt.Sprintf("%.1f%% used (resets in %s)", float64(pw.UsedPercent), display.FormatDuration(pw.ResetAfterSecs))))
		}
		if u.RateLimit.SecondaryWindow != nil {
			sw := u.RateLimit.SecondaryWindow
			fmt.Printf("%s %s\n", display.Dim("  weekly:   "),
				display.UsageRatioColor(float64(100-sw.UsedPercent), 100, fmt.Sprintf("%.1f%% used (resets in %s)", float64(sw.UsedPercent), display.FormatDuration(sw.ResetAfterSecs))))
		}
	}
	if u.SpendControl != nil {
		if u.SpendControl.Reached {
			fmt.Printf("%s %s\n", display.Dim("Usage:     "), display.Red("limit reached"))
		} else if u.SpendControl.IndividualLimit != nil {
			il := u.SpendControl.IndividualLimit
			pct := il.UsedPercent
			bar := display.ProgressBar(pct, 10)
			pctStr := display.UsageRatioColor(float64(100-pct), 100, fmt.Sprintf("%.1f%% used", float64(pct)))
			resetStr := ""
			if il.ResetAfter > 0 {
				resetStr = display.Gray(", resets " + display.FormatDuration(il.ResetAfter))
			}
			fmt.Printf("%s %s %s · %s / %s credits%s\n",
				display.Dim("Usage:     "),
				bar, pctStr,
				display.Bold(display.FormatCredits(il.Used)), display.Gray(display.FormatCredits(il.Limit)),
				resetStr)
		}
	}
	return nil
}

func (p *ZhipuProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	body, ok := usageGetForDisplay(p.cfg.UsageURL, p.providerName, p.AuthHeaders, p.cfg.Headers)
	if !ok {
		return nil
	}
	if s, _ := ParseZhipuQuota(body, ""); s != nil {
		if s.Level != "" {
			fmt.Printf("%s %s\n", display.Dim("Level:     "), display.Magenta(s.Level))
		}
		DecorateExhaustionEta(p.providerName, s)
		printQuotaSnapshot(s)
		return nil
	}
	var ml struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &ml) == nil && ml.Object == "list" {
		fmt.Printf("%s %d models available\n", display.Dim("Models:    "), len(ml.Data))
		for _, m := range ml.Data {
			fmt.Printf("  %s  %s\n", display.Cyan(display.Pad(m.ID, 22)), display.Gray(m.OwnedBy))
		}
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		fmt.Println(display.Red("Error: parse usage response: " + err.Error()))
		return nil
	}
	data := raw
	if d, ok := raw["data"].(map[string]any); ok {
		data = d
	}
	printUsageFields(data, 1)
	return nil
}

func (p *DeepSeekProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.cfg.ProviderName)))
	body, ok := usageGetForDisplay(p.cfg.UsageURL, p.cfg.ProviderName, p.AuthHeaders, nil)
	if !ok {
		return nil
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
		fmt.Println(display.Red("Error: parse usage response: " + err.Error()))
		return nil
	}
	if u.IsAvailable {
		fmt.Printf("%s %s\n", display.Dim("Available:  "), display.Green("yes"))
	} else {
		fmt.Printf("%s %s\n", display.Dim("Available:  "), display.Red("no (insufficient balance)"))
	}
	for _, b := range u.BalanceInfos {
		cur := b.Currency
		if cur == "" {
			cur = "Balance"
		}
		fmt.Printf("%s %s  %s\n",
			display.Dim(display.Pad(cur+":", 12)),
			display.Bold(display.Cyan(b.TotalBalance)),
			display.Gray("(granted "+b.GrantedBalance+", topped-up "+b.ToppedUpBalance+")"))
	}
	return nil
}

func (p *VolcengineProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.cfg.ProviderName)))
	ak, sk, err := p.resolveAKSK()
	if err != nil {
		fmt.Printf("%s Agent Plan 5h/周/月额度需经 GetAFPUsage（火山引擎签名 OpenAPI，AccessKey/SecretKey + V4）。\n", display.Dim("Note:       "))
		fmt.Printf("%s 用 `model-proxy login %s` 配置 AK/SK（IAM 密钥，非 Ark API Key）后可查询。\n", display.Dim("            "), p.cfg.ProviderName)
		listConfigModels(p.cfg.Models)
		return nil
	}
	u, err := getAFPUsage(ak, sk)
	if err != nil {
		fmt.Printf("%s GetAFPUsage failed: %v\n", display.Dim("Error:      "), err)
		listConfigModels(p.cfg.Models)
		return nil
	}
	if u.PlanType != "" {
		fmt.Printf("%s %s\n", display.Dim("Plan:      "), display.Magenta(u.PlanType))
	}
	printAFPWindow("5h", u.AFPFiveHour)
	printAFPWindow("Daily", u.AFPDaily)
	printAFPWindow("Weekly", u.AFPWeekly)
	printAFPWindow("Monthly", u.AFPMonthly)
	return nil
}

func (p *QwenPlanProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	fmt.Printf("%s Credits (5h + 7d windows; Lite/Standard/Pro — either hitting the cap pauses service)\n", display.Dim("Billing:    "))
	fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Gray("(console-only; no public Credits API)"))
	fmt.Printf("%s %s\n", display.Dim("Details:    "), display.Cyan(qwenPlanConsoleURL))
	listConfigModels(p.cfg.Models)
	return nil
}
