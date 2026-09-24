package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"model-proxy/internal/display"
)

// OpenRouterProvider implements the OpenRouter aggregator (openrouter.ai):
// prepaid credits, pay-as-you-go. One API key (Authorization: Bearer — the
// only documented auth shape, api reference "Authentication") serves BOTH
// protocols on two per-protocol bases (the deepseek pattern):
//   - openai_base_url (https://openrouter.ai/api/v1) → /chat/completions,
//     /responses, /models, /key (quota) — Bearer.
//   - anthropic_base_url (https://openrouter.ai/api) → /v1/messages —
//     OpenRouter's Anthropic Messages endpoint ("Anthropic Skin"). Bearer is
//     the documented path (the Claude Code cookbook wires ANTHROPIC_AUTH_TOKEN
//     → Authorization: Bearer); x-api-key is also read, but ApiKeyBase's
//     default Bearer-only injection (which strips x-api-key) is the canonical
//     shape, so AuthHeaders keeps the default.
//
// The proxy selects the upstream base by protocol (see proxy.forward); requests
// are byte-level passthrough (RewriteRequest no-op). Model ids are
// VENDOR-PREFIXED ("anthropic/claude-…", "openai/gpt-…", plus ":free"/":batch"
// variants) — request-routing deliberately treats a non-provider "/" prefix as
// part of the model name, so those ids pass through untouched (and do NOT
// match models.dev's bare-name metadata; declare capabilities: per model when
// the request-aware router needs it).
//
// Billing: prepaid credits (BillingPayG). Quota polls GET /key (usage_url):
// per-key credit cap (optional), current UTC day/week/month spend, and the
// free-model daily request counter. No windowed budget → RemainingPct stays
// -1 (deepseek's pay-as-you-go contract); exhaustion surfaces reactively as
// the upstream 402 (targetexec's body-proven quota-denied policy classifies
// openrouter's limit_source-marked 402s) → cooldown + failover.
type OpenRouterProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

// openRouterCreditsURL is the top-up / settings page. Shown in Notes because
// the /key endpoint reports usage but not the account balance itself.
const openRouterCreditsURL = "https://openrouter.ai/settings/credits"

func init() {
	Register("openrouter", func(cfg *Config, providerName string) (Provider, error) {
		return &OpenRouterProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// RewriteRequest is a no-op: pure passthrough. The proxy selects the upstream
// base URL by protocol in proxy.forward (openai_base_url for OpenAI paths,
// anthropic_base_url for /v1/messages).
func (p *OpenRouterProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *OpenRouterProvider) Logout() error { return p.DeleteKey() }

// FetchModels lists the aggregator catalog via the OpenAI-compatible /models
// endpoint (Bearer). The list is large (~450 ids incl. :free/:batch variants);
// `models refresh` probes and filters it down to callable chat models.
func (p *OpenRouterProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// ExtraHeaders sets anthropic-version on every upstream request (forward +
// probe), like every other anthropic-compatible provider here — the forward
// path's client→upstream header whitelist does not carry anthropic-version,
// and OpenRouter's Anthropic Messages endpoint behaves like the real
// Anthropic API. Harmless on the OpenAI paths.
func (p *OpenRouterProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

// Quota GETs /api/v1/key and parses the key-usage envelope (label, optional
// per-key credit cap, day/week/month spend, free-model daily requests). On
// any failure returns a BillingUnknown snapshot carrying the error (never a
// non-nil error) so the poll stays alive.
func (p *OpenRouterProvider) Quota() (*QuotaSnapshot, error) {
	body, fail, ok := usageGet(p.cfg.UsageURL, p.AuthHeaders, nil, nil)
	if !ok {
		return fail, nil
	}
	return ParseOpenRouterKeyQuota(body, time.Now()), nil
}

// openRouterKey is the /api/v1/key response body (api reference "Limits":
// "Checking your limits … GET https://openrouter.ai/api/v1/key"). limit /
// limit_remaining are null when the key has no per-key credit cap.
type openRouterKey struct {
	Data struct {
		Label          string   `json:"label"`
		Limit          *float64 `json:"limit"`
		LimitReset     string   `json:"limit_reset"` // reset TYPE ("daily", …), not a timestamp
		LimitRemaining *float64 `json:"limit_remaining"`
		Usage          float64  `json:"usage"`
		UsageDaily     float64  `json:"usage_daily"`
		UsageWeekly    float64  `json:"usage_weekly"`
		UsageMonthly   float64  `json:"usage_monthly"`
		IsFreeTier     bool     `json:"is_free_tier"`
		FreeModelDaily *struct {
			Used      float64 `json:"used"`
			Limit     float64 `json:"limit"`
			Remaining float64 `json:"remaining"`
		} `json:"free_model_daily_requests"`
	} `json:"data"`
}

// ParseOpenRouterKeyQuota parses /api/v1/key into a pay-as-you-go snapshot.
// Windows are display-only (no Ultimate/Short — there is no windowed budget
// to schedule against): an optional per-key credit cap with a real remaining
// fraction, the current UTC day/week/month spend (counters reset at the UTC
// boundaries OpenRouter documents), and the free-model daily request cap.
// RemainingPct stays -1: the account balance is not part of this payload.
// A malformed body returns a BillingUnknown snapshot carrying the error —
// the same failure contract as every other parser here.
func ParseOpenRouterKeyQuota(body []byte, now time.Time) *QuotaSnapshot {
	var k openRouterKey
	if err := json.Unmarshal(body, &k); err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}
	}
	s := &QuotaSnapshot{
		Billing:      BillingPayG,
		RemainingPct: -1,
		Account:      k.Data.Label,
		Plan:         "credits",
		AsOf:         now,
		Notes:        []string{"Prepaid credits — top up at " + openRouterCreditsURL},
	}
	if k.Data.IsFreeTier {
		s.Notes = append(s.Notes, "free tier: no credits purchased yet")
	}
	if lim := k.Data.Limit; lim != nil && *lim > 0 {
		w := QuotaWindow{
			Label: "Key credit cap", Kind: "money",
			Total: *lim, Used: *lim, RemainingPct: -1,
		}
		if rem := k.Data.LimitRemaining; rem != nil && *rem >= 0 {
			w.Used = *lim - *rem
			w.RemainingPct = *rem / *lim
		}
		if k.Data.LimitReset != "" {
			w.Label = "Key credit cap (" + k.Data.LimitReset + " reset)"
		}
		s.Windows = append(s.Windows, w)
	}
	nextDay, nextWeek, nextMonth := nextUTCPeriodBoundaries(now)
	s.Windows = append(s.Windows,
		QuotaWindow{Label: "Spend (today)", Kind: "money", Used: k.Data.UsageDaily, RemainingPct: -1, ResetsAt: nextDay, Duration: 24 * time.Hour},
		QuotaWindow{Label: "Spend (this week)", Kind: "money", Used: k.Data.UsageWeekly, RemainingPct: -1, ResetsAt: nextWeek, Duration: 7 * 24 * time.Hour},
		QuotaWindow{Label: "Spend (this month)", Kind: "money", Used: k.Data.UsageMonthly, RemainingPct: -1, ResetsAt: nextMonth, Duration: monthLength(now)},
	)
	if f := k.Data.FreeModelDaily; f != nil && f.Limit > 0 {
		s.Windows = append(s.Windows, QuotaWindow{
			Label: "Free-model req/day", Kind: "requests",
			Used: f.Used, Total: f.Limit, RemainingPct: f.Remaining / f.Limit,
			ResetsAt: nextDay, Duration: 24 * time.Hour,
		})
	}
	return s
}

// nextUTCPeriodBoundaries returns the next UTC midnight / Monday-midnight /
// month-start after now — the reset points of OpenRouter's usage_daily /
// usage_weekly (current UTC week starting Monday) / usage_monthly counters.
func nextUTCPeriodBoundaries(now time.Time) (day, week, month time.Time) {
	u := now.UTC()
	todayStart := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	day = todayStart.AddDate(0, 0, 1)
	daysToMonday := (int(time.Monday) - int(u.Weekday()) + 7) % 7
	if daysToMonday == 0 {
		daysToMonday = 7
	}
	week = todayStart.AddDate(0, 0, daysToMonday)
	month = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return day, week, month
}

// monthLength returns the length of now's calendar month (the nominal reset
// cycle of a monthly counter).
func monthLength(now time.Time) time.Duration {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start.AddDate(0, 1, 0).Sub(start)
}

// Usage prints the /key snapshot: account (key label), plan line, and the
// shared quota-window rendering (bars, resets, absolute used lines).
func (p *OpenRouterProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	body, ok := usageGetForDisplay(p.cfg.UsageURL, p.providerName, p.AuthHeaders, nil)
	if !ok {
		return nil
	}
	s := ParseOpenRouterKeyQuota(body, time.Now())
	if s.Err != "" {
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Red("(unavailable: "+s.Err+")"))
		return nil
	}
	if s.Account != "" {
		fmt.Printf("%s %s\n", display.Dim("Account:   "), display.Bold(display.Cyan(s.Account)))
	}
	fmt.Printf("%s prepaid credits (pay-as-you-go)\n", display.Dim("Billing:   "))
	printQuotaSnapshot(s)
	return nil
}
