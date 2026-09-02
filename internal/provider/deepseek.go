package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/display"
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

func (p *DeepSeekProvider) Logout() error { return p.DeleteKey() }
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

// ParseDeepseekQuota parses DeepSeek /user/balance into a pay-as-you-go snapshot:
// one unmeasured (RemainingPct=-1) window per currency carrying the remaining
// balance as Total. DeepSeek is pay-as-you-go with no windowed budget, so only
// the remaining balance is surfaced to the Web UI - the granted/topped-up split
// is NOT shown here (it's redundant: total = granted + topped-up, and the user
// only cares about the amount left); the CLI `usage deepseek` still prints it
// compactly via its own struct. BillingPayG (no windowed budget -> -1 binding);
// TotalBalance is the absolute amount left, not a quota ceiling, so the UI
// renders Total directly rather than a used/total percentage.
func ParseDeepseekQuota(body []byte) *QuotaSnapshot {
	var u struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	s := &QuotaSnapshot{Billing: BillingPayG, RemainingPct: -1, AsOf: time.Now()}
	if err := json.Unmarshal(body, &u); err != nil {
		// Same contract as every other failure in Quota(): BillingUnknown with
		// the error — a snapshot that claims PayG while carrying an error
		// contradicts the function's own failure contract.
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}
	}
	if !u.IsAvailable {
		s.Notes = append(s.Notes, "insufficient balance")
	}
	for _, b := range u.BalanceInfos {
		total, _ := strconv.ParseFloat(b.TotalBalance, 64)
		s.Windows = append(s.Windows, QuotaWindow{
			Label: display.Or(b.Currency, "Balance"), Kind: "money",
			Total: total, RemainingPct: -1,
		})
	}
	return s
}
