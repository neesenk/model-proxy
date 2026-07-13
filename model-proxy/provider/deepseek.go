package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DeepSeekProvider implements the DeepSeek API provider. A single API key
// authenticates both protocols; the two endpoints are configured per-protocol:
//   - openai_base_url (OpenAI base, e.g. https://api.deepseek.com) → /chat/completions,
//     /responses, /models, /user/balance — uses Authorization: Bearer.
//   - anthropic_base_url (e.g. https://api.deepseek.com/anthropic) → /v1/messages
//     — uses x-api-key.
//
// The proxy selects the upstream base by protocol (see proxy.forward): anthropic
// requests go to anthropic_base_url, openai requests to openai_base_url. For
// anthropic, the proxy keeps the client's /v1 path (matching the official SDK
// convention: base_url + /v1/messages), so anthropic_base_url should NOT include /v1.
//
// Both auth headers are set on every request so one provider config serves both
// protocols (the OpenAI endpoint ignores x-api-key; the Anthropic endpoint ignores
// anthropic-version/anthropic-beta and reads x-api-key).
type DeepSeekProvider struct {
	*ApiKeyBase
	baseProbe
	cfg *Config
}

func init() {
	Register("deepseek", func(cfg *Config, providerName string) (Provider, error) {
		return &DeepSeekProvider{
			ApiKeyBase: newApiKeyBaseBound(cfg, providerName),
			cfg:        cfg,
		}, nil
	})
}

// AuthHeaders injects the API key as BOTH Authorization: Bearer (OpenAI endpoint)
// and x-api-key (Anthropic endpoint), so one provider config serves both protocols.
// Overrides ApiKeyBase.AuthHeaders (which only sets Bearer + deletes x-api-key).
func (p *DeepSeekProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by protocol
// (anthropic_base_url for /messages, baseURL/openai_base_url otherwise). DeepSeek
// customizes only auth (see AuthHeaders).
func (p *DeepSeekProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *DeepSeekProvider) Login() error  { return p.cfg.LoginFn() }
func (p *DeepSeekProvider) Logout() error { return p.cfg.LogoutFn() }
func (p *DeepSeekProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// Quota GETs /user/balance and parses the per-currency balance windows.
// DeepSeek is pay-as-you-go: no windowed budget (RemainingPct=-1). On any
// failure returns a BillingUnknown snapshot carrying the error.
func (p *DeepSeekProvider) Quota() (*QuotaSnapshot, error) {
	req, _ := http.NewRequest("GET", p.cfg.UsageURL, nil)
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
	return ParseDeepseekQuota(body), nil
}
func (p *DeepSeekProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}

// ParseDeepseekQuota parses DeepSeek /user/balance into a pay-as-you-go snapshot:
// per-currency balance windows (unmeasured, RemainingPct=-1) with granted /
// topped-up breakdown. BillingPayG (no windowed budget -> -1 binding).
func ParseDeepseekQuota(body []byte) *QuotaSnapshot {
	var u struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	s := &QuotaSnapshot{Billing: BillingPayG, RemainingPct: -1, AsOf: time.Now()}
	if err := json.Unmarshal(body, &u); err != nil {
		s.Err = err.Error()
		return s
	}
	if !u.IsAvailable {
		s.Notes = append(s.Notes, "insufficient balance")
	}
	for _, b := range u.BalanceInfos {
		total, _ := strconv.ParseFloat(b.TotalBalance, 64)
		s.Windows = append(s.Windows, QuotaWindow{
			Label: or(b.Currency, "Balance"), Kind: "money",
			Total: total, RemainingPct: -1,
			Details: []QuotaDetail{
				{Label: "granted", Used: atof(b.GrantedBalance)},
				{Label: "topped-up", Used: atof(b.ToppedUpBalance)},
			},
		})
	}
	return s
}
