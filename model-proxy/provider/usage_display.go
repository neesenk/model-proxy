package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		var bar, pctStr string
		if w.RemainingPct < 0 {
			bar = Gray("n/a")
			pctStr = Gray("unmeasured")
		} else {
			pct := int(w.RemainingPct * 100)
			usedPct := 100 - pct
			bar = ProgressBar(usedPct, 16)
			pctStr = UsageRatioColor(w.RemainingPct, 1, fmt.Sprintf("%d%% used", usedPct))
		}
		resetStr := ""
		if !w.ResetsAt.IsZero() {
			dur := FormatDuration(int(time.Until(w.ResetsAt) / time.Second))
			resetStr = Gray(" · resets " + dur + "(at " + FormatResetAt(w.ResetsAt.UnixMilli()) + ")")
		}
		fmt.Printf("%s %s  %s%s\n", Dim(Pad(w.Label+":", 18)), bar, pctStr, resetStr)
		if w.Total > 0 {
			fmt.Printf("%s %.0f used / %.0f total (%.0f remaining)\n",
				Dim(Pad("Usage:", 18)), w.Used, w.Total, w.Total-w.Used)
		}
		if len(w.Details) > 0 && w.DetailLabel != "" {
			parts := make([]string, 0, len(w.Details))
			for _, d := range w.Details {
				parts = append(parts, fmt.Sprintf("%s: %.0f", d.Label, d.Used))
			}
			fmt.Printf("%s %s\n", Dim(Pad(w.DetailLabel+":", 18)), Gray(strings.Join(parts, " · ")))
		}
	}
	for _, n := range s.Notes {
		fmt.Println(Dim(Pad("", 18)) + n)
	}
}

// printAFPWindow renders one Volcengine AFP quota window.
func printAFPWindow(label string, w AfpWindow) {
	remaining := w.Quota - w.Used
	pct := 0
	if w.Quota > 0 {
		pct = int(w.Used / w.Quota * 100)
	}
	bar := ProgressBar(pct, 16)
	pctStr := UsageRatioColor(float64(100-pct), 100, fmt.Sprintf("%d%% used", pct))
	reset := "-"
	if w.ResetTime > 0 {
		dur := FormatDuration(int((w.ResetTime - time.Now().UnixMilli()) / 1000))
		reset = dur + "(at " + FormatResetAt(w.ResetTime) + ")"
	}
	fmt.Printf("%s %s  %s · resets %s  (%.1f used / %.1f quota, %.1f remaining)\n",
		Dim(Pad(label+":", 12)), bar, pctStr, Gray(reset), w.Used, w.Quota, remaining)
}

// aqpUsageLine renders the aqp monthly-usage line (content after "Usage:"):
// a progress bar + "<PCT>% used" prefix, then usage/total + balance/plan/date.
func aqpUsageLine(mu *MonthlyProjectUsage) string {
	pct := 0
	if mu.TotalAmount > 0 {
		pct = int((mu.Usage/mu.TotalAmount)*100 + 0.5)
	}
	bar := ProgressBar(pct, 10)
	pctStr := UsageRatioColor(mu.Balance, mu.TotalAmount, fmt.Sprintf("%d%% used", pct))
	return fmt.Sprintf("%s %s · %s / %s  (%s %s, %s, %d-%02d)",
		bar, pctStr,
		UsageRatioColor(mu.Balance, mu.TotalAmount, Money(mu.Usage)),
		Gray(Money(mu.TotalAmount)),
		Dim("balance"), UsageRatioColor(mu.Balance, mu.TotalAmount, Money(mu.Balance)),
		Magenta(mu.Plan), mu.SelectedYear, mu.SelectedMonth)
}

// listConfigModels prints the config model-id list (fallback when a provider
// can't fetch a structured quota).
func listConfigModels(models []string) {
	ids := append([]string(nil), models...)
	sort.Strings(ids)
	fmt.Printf("%s %d models (from config)\n", Dim("Models:     "), len(ids))
	for _, id := range ids {
		fmt.Printf("  %s\n", Cyan(id))
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
			fmt.Printf("%s%s %s\n", Dim(Pad(k+":", 18)), prefix, Gray(val))
		case float64:
			fmt.Printf("%s%s %s\n", Dim(Pad(k+":", 18)), prefix, Cyan(fmt.Sprintf("%v", val)))
		case bool:
			fmt.Printf("%s%s %s\n", Dim(Pad(k+":", 18)), prefix, Cyan(fmt.Sprintf("%v", val)))
		case map[string]any:
			fmt.Printf("%s%s %s\n", Dim(Pad(k+":", 18)), prefix, Bold(""))
			printUsageFields(val, indent+1)
		default:
			fmt.Printf("%s%s %s\n", Dim(Pad(k+":", 18)), prefix, Gray(fmt.Sprintf("%v", val)))
		}
	}
}

// --- per-provider Usage() ---

func (p *AqpProvider) Usage() (any, error) {
	fmt.Printf("%s %s\n", Dim("Provider:  "), Bold(Blue("aqp")))
	if p.cfg.AqpAccount == nil {
		fmt.Println(Yellow("Not logged in.") + " Run: " + Cyan("model-proxy login aqp"))
		return nil, nil
	}
	email, projectID, storePath, err := p.cfg.AqpAccount()
	if err != nil {
		fmt.Println(Red("Error: " + err.Error()))
		return nil, nil
	}
	if email == "" {
		fmt.Println(Yellow("Not logged in.") + " Run: " + Cyan("model-proxy login aqp"))
		return nil, nil
	}
	fmt.Printf("%s %s\n", Dim("Account:    "), Bold(Cyan(email)))
	fmt.Printf("%s %s\n", Dim("Project ID: "), Gray(projectID))
	if p.cfg.AqpMonthlyUsage != nil {
		mu, err := p.cfg.AqpMonthlyUsage()
		if err != nil {
			fmt.Printf("%s %s\n", Dim("Usage:      "), Red("(unavailable: "+err.Error()+")"))
		} else {
			fmt.Printf("%s %s\n", Dim("Usage:      "), aqpUsageLine(mu))
		}
	}
	fmt.Printf("%s %s\n", Dim("Store:      "), Gray(storePath))
	return nil, nil
}

func (p *CodexProvider) Usage() (any, error) {
	fmt.Printf("%s %s\n", Dim("Provider:  "), Bold(Blue("codex")))
	usageURL := strings.TrimSuffix(p.cfg.OpenAIBaseURL, "/codex") + "/wham/usage"
	req, _ := http.NewRequest("GET", usageURL, nil)
	if err := p.AuthHeaders(req); err != nil {
		fmt.Println(Yellow("Not logged in.") + " Run: " + Cyan("model-proxy login codex"))
		return nil, nil
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(Red("Error: usage request: " + err.Error()))
		return nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", Red("Error:"), resp.StatusCode, Truncate(string(body), 200))
		return nil, nil
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
	fmt.Printf("%s %s\n", Dim("Account:   "), Bold(Cyan(Or(u.Email, "(unknown)"))))
	fmt.Printf("%s %s\n", Dim("Plan:      "), Magenta(Or(u.PlanType, "(unknown)")))
	if u.Credits != nil {
		if u.Credits.Unlimited {
			fmt.Printf("%s %s\n", Dim("Credits:   "), Green("unlimited"))
		} else if u.Credits.HasCredits {
			bal := "available"
			if u.Credits.Balance != nil && *u.Credits.Balance != "" {
				bal = *u.Credits.Balance
			}
			fmt.Printf("%s %s\n", Dim("Credits:   "), Green("has credits ("+bal+")"))
		} else {
			fmt.Printf("%s %s\n", Dim("Credits:   "), Red("none"))
		}
	}
	if u.RateLimit != nil {
		status := Green("allowed")
		if u.RateLimit.LimitReached {
			status = Red("limit reached")
		} else if !u.RateLimit.Allowed {
			status = Yellow("not allowed")
		}
		fmt.Printf("%s %s\n", Dim("Rate Limit:"), status)
		if u.RateLimit.PrimaryWindow != nil {
			pw := u.RateLimit.PrimaryWindow
			fmt.Printf("%s %s\n", Dim("  primary:  "),
				UsageRatioColor(float64(100-pw.UsedPercent), 100, fmt.Sprintf("%d%% used (resets in %s)", pw.UsedPercent, FormatDuration(pw.ResetAfterSecs))))
		}
		if u.RateLimit.SecondaryWindow != nil {
			sw := u.RateLimit.SecondaryWindow
			fmt.Printf("%s %s\n", Dim("  weekly:   "),
				UsageRatioColor(float64(100-sw.UsedPercent), 100, fmt.Sprintf("%d%% used (resets in %s)", sw.UsedPercent, FormatDuration(sw.ResetAfterSecs))))
		}
	}
	if u.SpendControl != nil {
		if u.SpendControl.Reached {
			fmt.Printf("%s %s\n", Dim("Usage:     "), Red("limit reached"))
		} else if u.SpendControl.IndividualLimit != nil {
			il := u.SpendControl.IndividualLimit
			pct := il.UsedPercent
			bar := ProgressBar(pct, 10)
			pctStr := UsageRatioColor(float64(100-pct), 100, fmt.Sprintf("%d%% used", pct))
			resetStr := ""
			if il.ResetAfter > 0 {
				resetStr = Gray(", resets " + FormatDuration(il.ResetAfter))
			}
			fmt.Printf("%s %s %s · %s / %s credits%s\n",
				Dim("Usage:     "),
				bar, pctStr,
				Bold(FormatCredits(il.Used)), Gray(FormatCredits(il.Limit)),
				resetStr)
		}
	}
	return nil, nil
}

func (p *ZhipuProvider) Usage() (any, error) {
	fmt.Printf("%s %s\n", Dim("Provider:  "), Bold(Blue(p.providerName)))
	req, _ := http.NewRequest("GET", p.cfg.UsageURL, nil)
	if err := p.cfg.Auth.Inject(req); err != nil {
		fmt.Println(Yellow("Not logged in.") + " Run: " + Cyan("model-proxy login "+p.providerName))
		return nil, nil
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(Red("Error: usage request: " + err.Error()))
		return nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", Red("Error:"), resp.StatusCode, Truncate(string(body), 200))
		return nil, nil
	}
	if s, _ := ParseZhipuQuota(body, ""); s != nil {
		if s.Level != "" {
			fmt.Printf("%s %s\n", Dim("Level:     "), Magenta(s.Level))
		}
		printQuotaSnapshot(s)
		return nil, nil
	}
	var ml struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &ml) == nil && ml.Object == "list" {
		fmt.Printf("%s %d models available\n", Dim("Models:    "), len(ml.Data))
		for _, m := range ml.Data {
			fmt.Printf("  %s  %s\n", Cyan(Pad(m.ID, 22)), Gray(m.ID))
		}
		return nil, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		fmt.Println(Red("Error: parse usage response: " + err.Error()))
		return nil, nil
	}
	data := raw
	if d, ok := raw["data"].(map[string]any); ok {
		data = d
	}
	printUsageFields(data, 1)
	return nil, nil
}

func (p *DeepSeekProvider) Usage() (any, error) {
	fmt.Printf("%s %s\n", Dim("Provider:  "), Bold(Blue(p.cfg.ProviderName)))
	req, _ := http.NewRequest("GET", p.cfg.UsageURL, nil)
	if err := p.cfg.Auth.Inject(req); err != nil {
		fmt.Println(Yellow("Not logged in.") + " Run: " + Cyan("model-proxy login "+p.cfg.ProviderName))
		return nil, nil
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(Red("Error: usage request: " + err.Error()))
		return nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", Red("Error:"), resp.StatusCode, Truncate(string(body), 200))
		return nil, nil
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
		fmt.Println(Red("Error: parse usage response: " + err.Error()))
		return nil, nil
	}
	if u.IsAvailable {
		fmt.Printf("%s %s\n", Dim("Available:  "), Green("yes"))
	} else {
		fmt.Printf("%s %s\n", Dim("Available:  "), Red("no (insufficient balance)"))
	}
	for _, b := range u.BalanceInfos {
		cur := b.Currency
		if cur == "" {
			cur = "Balance"
		}
		fmt.Printf("%s %s  %s\n",
			Dim(Pad(cur+":", 12)),
			Bold(Cyan(b.TotalBalance)),
			Gray("(granted "+b.GrantedBalance+", topped-up "+b.ToppedUpBalance+")"))
	}
	return nil, nil
}

func (p *VolcengineProvider) Usage() (any, error) {
	fmt.Printf("%s %s\n", Dim("Provider:  "), Bold(Blue(p.cfg.ProviderName)))
	ak, sk, err := p.resolveAKSK()
	if err != nil {
		fmt.Printf("%s Agent Plan 5h/周/月额度需经 GetAFPUsage（火山引擎签名 OpenAPI，AccessKey/SecretKey + V4）。\n", Dim("Note:       "))
		fmt.Printf("%s 用 `model-proxy login %s` 配置 AK/SK（IAM 密钥，非 Ark API Key）后可查询。\n", Dim("            "), p.cfg.ProviderName)
		listConfigModels(p.cfg.Models)
		return nil, nil
	}
	u, err := getAFPUsage(ak, sk)
	if err != nil {
		fmt.Printf("%s GetAFPUsage failed: %v\n", Dim("Error:      "), err)
		listConfigModels(p.cfg.Models)
		return nil, nil
	}
	if u.PlanType != "" {
		fmt.Printf("%s %s\n", Dim("Plan:      "), Magenta(u.PlanType))
	}
	printAFPWindow("5h", u.AFPFiveHour)
	printAFPWindow("Daily", u.AFPDaily)
	printAFPWindow("Weekly", u.AFPWeekly)
	printAFPWindow("Monthly", u.AFPMonthly)
	return nil, nil
}
