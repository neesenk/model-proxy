package provider

import (
	"net/http"
	"strings"
	"time"
)

// AqpProvider wraps the AQP auth (key minting from SSO cookie) + SSO login +
// monthly_usage.
type AqpProvider struct {
	baseProbe
	cfg  *Config
	auth authInjector
}

func init() {
	Register("aqp", func(cfg *Config, providerName string) (Provider, error) {
		return &AqpProvider{cfg: cfg, auth: cfg.authOrDefault(NewAqpKeyProvider(cfg.AqpMintURL, cfg.OAuthAuthFile))}, nil
	})
}

func (p *AqpProvider) AuthHeaders(req *http.Request) error {
	return p.auth.Inject(req)
}
func (p *AqpProvider) Refresh() error {
	return p.auth.Refresh()
}
func (p *AqpProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	if strings.Contains(path, "/messages") && !strings.Contains(targetURL, "beta=") {
		if strings.Contains(targetURL, "?") {
			targetURL += "&beta=true"
		} else {
			targetURL += "?beta=true"
		}
	}
	return targetURL, body
}
func (p *AqpProvider) Login() error                   { return p.cfg.LoginFn() }
func (p *AqpProvider) Logout() error                  { return removeAuthFile(p.cfg.OAuthAuthFile) }
func (p *AqpProvider) FetchModels() ([]string, error) { return fetchModelsBearer(p.cfg, p.AuthHeaders) }

// Quota POSTs monthly_usage (via the injected AqpMonthlyUsage fetcher, which
// is the SSO-cookie-authed AqpClient shared with login/web) and parses it into
// a single monthly Ultimate window. On any failure returns BillingUnknown
// carrying the error (never a non-nil error).
func (p *AqpProvider) Quota() (*QuotaSnapshot, error) {
	if p.cfg.AqpMonthlyUsage == nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "monthly usage not configured"}, nil
	}
	mu, err := p.cfg.AqpMonthlyUsage()
	if err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	return ParseAqpQuota(mu, ""), nil
}
func (p *AqpProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}

// ProbeRequest overrides the OpenAI default: aqp speaks the Anthropic messages
// API, so the probe goes to /v1/messages (base does NOT include /v1; the SDK
// appends it) with an anthropic body. Mirrors forward's anthropic path.
func (p *AqpProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   anthropicProbeBody(modelID),
	}
}

// ExtraHeaders sets aqp's per-request headers: anthropic-version + a fresh
// x-compass-request-id UUID. Applied on EVERY upstream request (forward + probe)
// so the two paths share one implementation - no duplicated aqp branch in main.
func (p *AqpProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-compass-request-id", newRequestID())
}

// MonthlyProjectUsage mirrors monthly_usage's data payload (7 fields, matching the
// binary's struct MonthlyProjectUsage). Method POST, requires project_id input.
type MonthlyProjectUsage struct {
	ProjectID     string  `json:"project_id"`
	SelectedYear  int     `json:"selected_year"`
	SelectedMonth int     `json:"selected_month"`
	TotalAmount   float64 `json:"total_amount"`
	Usage         float64 `json:"usage"`
	Balance       float64 `json:"balance"`
	Plan          string  `json:"plan"`
}

// aqpMonthlyReset derives the monthly quota reset time (last second of the
// selected month, local time) and the nominal cycle duration from the
// SelectedYear/SelectedMonth the monthly_usage endpoint returns. Falls back to
// the current month when the API omits them (zero values). The reset time is
// required: without it the surplus guard in QuotaSnapshot.Surplus()
// (ult.ResetsAt.IsZero()) short-circuits to 0, so aqp could never be
// prioritized for being under pace - it would only beat over-pace providers.
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

// ParseAqpQuota converts a MonthlyProjectUsage into a single-window plan snapshot.
// The monthly window is Ultimate (total budget); remaining = balance/total.
func ParseAqpQuota(mu *MonthlyProjectUsage, account string) *QuotaSnapshot {
	s := &QuotaSnapshot{Billing: BillingPlan, Account: account, Plan: mu.Plan, AsOf: time.Now()}
	rem := -1.0
	if mu.TotalAmount > 0 {
		rem = mu.Balance / mu.TotalAmount
	}
	resetsAt, dur := aqpMonthlyReset(mu.SelectedYear, mu.SelectedMonth)
	s.Windows = append(s.Windows, QuotaWindow{
		Label: "Monthly", Kind: "money",
		Used: mu.Usage, Total: mu.TotalAmount, RemainingPct: rem,
		Ultimate: true, Duration: dur, ResetsAt: resetsAt,
	})
	s.RemainingPct = rem
	return s
}
