package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/display"
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

// ReportSecrets implements SecretReporter by delegating to the auth injector
// when it reports in-memory secrets (the real AqpKeyProvider reports its
// minted key; test fakes report nothing). Values feed the guard known-secret
// scanner only — memory only, never serialized or logged.
func (p *AqpProvider) ReportSecrets() []string {
	if r, ok := p.auth.(SecretReporter); ok {
		return r.ReportSecrets()
	}
	return nil
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
func (p *AqpProvider) Logout() error                  { return removeAuthFile(p.cfg.OAuthAuthFile) }
func (p *AqpProvider) FetchModels() ([]string, error) { return fetchModelsBearer(p.cfg, p.AuthHeaders) }

// aqpBaseURL returns the compass backend base URL for the monthly_usage fetch:
// cfg.AqpBaseURL when set (tests), else the production AqpBase constant.
func (p *AqpProvider) aqpBaseURL() string {
	if p.cfg.AqpBaseURL != "" {
		return p.cfg.AqpBaseURL
	}
	return AqpBase
}

// fetchMonthlyUsage POSTs monthly_usage with the persisted SSO cookie +
// project_id (cookie-authed, not the managed key) and parses the
// {retcode, data:{...MonthlyProjectUsage}} envelope. The endpoint is POST and
// requires project_id input (taken from the store's AqpAccountData.ProjectID).
// Owned by the provider since Stage 2 of the aqp-fetch migration (was a main
// callback AqpMonthlyUsage wired to AqpClient.MonthlyUsage).
func (p *AqpProvider) fetchMonthlyUsage() (*MonthlyProjectUsage, error) {
	a, err := LoadAqpAccount(p.cfg.OAuthAuthFile)
	if err != nil || a == nil || a.SSOSessionCookie == "" {
		return nil, fmt.Errorf("not logged in")
	}
	if a.ProjectID == "" {
		return nil, fmt.Errorf("no project_id in store; run `model-proxy login aqp` (or --import) to populate it")
	}
	endpoint := strings.TrimRight(p.aqpBaseURL(), "/") + aqpMonthlyUsagePath
	payload, _ := json.Marshal(map[string]string{"project_id": a.ProjectID})
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	req.Header.Set("Cookie", CookieHeader(a.SSOSessionCookie))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("monthly usage request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// A non-200 (e.g. 401 "Session expired" when the SSO cookie has aged out)
	// returns an envelope with no retcode/data - without this check it falls
	// through to the misleading "missing data" error. Surface the real cause +
	// a re-login hint so both `usage aqp` and the scheduler's Quota() poll
	// report an expired session correctly instead of a confusing "no data".
	if resp.StatusCode != 200 {
		var e struct {
			Message string `json:"message"`
		}
		msg := display.Truncate(string(body), 120)
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if resp.StatusCode == 401 || strings.Contains(strings.ToLower(msg), "session") {
			return nil, fmt.Errorf("session expired (HTTP %d): %s - re-login: `model-proxy login aqp`", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("monthly usage HTTP %d: %s", resp.StatusCode, msg)
	}
	var wrap struct {
		Retcode int             `json:"retcode"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("monthly usage response parse failed: %w", err)
	}
	if wrap.Retcode != 0 {
		return nil, fmt.Errorf("monthly usage retcode=%d message=%s", wrap.Retcode, wrap.Message)
	}
	if len(wrap.Data) == 0 {
		return nil, fmt.Errorf("monthly usage response missing data")
	}
	var mu MonthlyProjectUsage
	if err := json.Unmarshal(wrap.Data, &mu); err != nil {
		return nil, fmt.Errorf("monthly usage data parse failed: %w", err)
	}
	return &mu, nil
}

// Quota fetches monthly_usage (SSO-cookie POST, project_id-scoped) directly and
// parses it into a single monthly Ultimate window. On any failure returns
// BillingUnknown carrying the error (never a non-nil error) so the scheduler
// treats aqp as unmeasured rather than crashing the poll.
func (p *AqpProvider) Quota() (*QuotaSnapshot, error) {
	mu, err := p.fetchMonthlyUsage()
	if err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	return ParseAqpQuota(mu, ""), nil
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
