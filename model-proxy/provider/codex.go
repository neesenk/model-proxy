package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CodexProvider wraps the main package's codex OAuth + device flow + wham/usage.
type CodexProvider struct {
	baseProbe
	cfg *Config
}

func init() {
	Register("codex", func(cfg *Config, providerName string) (Provider, error) {
		return &CodexProvider{cfg: cfg}, nil
	})
}

func (p *CodexProvider) AuthHeaders(req *http.Request) error {
	return p.cfg.Auth.Inject(req)
}
func (p *CodexProvider) Refresh() error {
	return p.cfg.Auth.Refresh()
}
func (p *CodexProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	// codex backend requires store:false in the request body.
	body = ensureJSONField(body, "store", false)
	return targetURL, body
}
func (p *CodexProvider) Login() error        { return p.cfg.LoginFn() }
func (p *CodexProvider) Logout() error       { return p.cfg.LogoutFn() }
func (p *CodexProvider) Usage() (any, error) { return p.cfg.UsageFn() }

// Quota GETs /backend-api/wham/usage (derived from OpenAIBaseURL by stripping
// the trailing /codex) with Bearer + originator + account-id (set by
// AuthHeaders). On any failure returns a BillingUnknown snapshot carrying the
// error string (never a non-nil error) so the scheduler treats codex as
// unmeasured rather than crashing the poll.
func (p *CodexProvider) Quota() (*QuotaSnapshot, error) {
	usageURL := strings.TrimSuffix(p.cfg.OpenAIBaseURL, "/codex") + "/wham/usage"
	req, _ := http.NewRequest("GET", usageURL, nil)
	if err := p.AuthHeaders(req); err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	return ParseCodexQuota(body, "", "")
}
func (p *CodexProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}

// ProbeRequest overrides the OpenAI default: codex's backend speaks the OpenAI
// Responses API (/responses), NOT /chat/completions. The body uses `input` (a
// list, not `messages`), requires stream:true, and rejects max_tokens.
// store:false is injected by RewriteRequest (as on the forward path).
func (p *CodexProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/responses",
		Body:   codexProbeBody(modelID),
	}
}

// FetchModels queries the codex backend's /models endpoint and returns the
// slugs with visibility "list". The endpoint is gated on a client_version
// query param (the backend uses it to decide which models to expose — a stale
// version hides newer models) and returns a custom {"models":[{slug,visibility,
// ...}]} shape rather than OpenAI's {data:[]}, so it cannot reuse
// fetchModelsBearer. client_version is resolved by the main package (config >
// codex CLI > ~/.codex cache > baked constant) and passed in via cfg.ClientVersion.
func (p *CodexProvider) FetchModels() ([]string, error) {
	url := strings.TrimRight(p.cfg.OpenAIBaseURL, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("client_version", p.cfg.ClientVersion)
	req.URL.RawQuery = q.Encode()
	if err := p.cfg.Auth.Inject(req); err != nil {
		return nil, fmt.Errorf("codex models auth: %w", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch codex models: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch codex models: HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}
	var v struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("parse codex models: %w", err)
	}
	ids := make([]string, 0, len(v.Models))
	for _, m := range v.Models {
		if m.Visibility == "list" {
			ids = append(ids, m.Slug)
		}
	}
	return ids, nil
}

// ParseCodexQuota parses codex /backend-api/wham/usage into a QuotaSnapshot.
// Windows: primary(5h) + secondary(weekly) + spend(monthly $). Credits/rate-limit
// status go to Notes for display.
//
// primary(5h)/secondary(weekly) are token rate-caps of a different unit than
// the $ spend budget, so they're not marked Short (peak-burn share undefined);
// their exhaustion is handled reactively via 429. Ultimate = monthly spend.
func ParseCodexQuota(body []byte, account, plan string) (*QuotaSnapshot, error) {
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
	s := &QuotaSnapshot{
		Billing: BillingPlan,
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
			w := QuotaWindow{
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
			w := QuotaWindow{
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
		w := QuotaWindow{
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
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s, nil
}

// ensureJSONField sets body[key] = val if the key is absent.
func ensureJSONField(body []byte, key string, val any) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	if _, ok := v[key]; !ok {
		v[key] = val
		out, err := json.Marshal(v)
		if err != nil {
			return body
		}
		return out
	}
	return body
}
