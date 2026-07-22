package provider

import (
	"net/http"
	"time"
)

// QwenPlanProvider implements 千问 AI Token Plan 个人版 (personal edition): a
// dual-protocol OpenAI + Anthropic byte-level passthrough on Alibaba Bailian's
// token-plan maas gateway, authenticated with one subscription API key (sk-sp-…).
//
// There is NO public Credits-usage API (personal-edition usage is console-only),
// so Quota() returns BillingUnknown carrying the console subscription URL in
// Notes; the 5h/7d window exhaustion surfaces reactively via the 429
// "Allocated quota exceeded" body, already classified as rlQuota by failclass.go
// → quota cooldown + failover.
type QwenPlanProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

// qwenPlanConsoleURL is the personal-edition subscription/usage page. Shown in
// the CLI `usage` output and the Web UI (QuotaSnapshot.Notes → app.js renders
// snap.Notes in the Accounts-tab provider detail) because the Credits numbers
// have no public API.
const qwenPlanConsoleURL = "https://platform.qianwenai.com/home/billing/subscription/token-plan-individual"

func init() {
	Register("qwen-plan", func(cfg *Config, providerName string) (Provider, error) {
		return &QwenPlanProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// AuthHeaders injects the key as BOTH Authorization: Bearer (OpenAI endpoint)
// and x-api-key (Anthropic endpoint), so one config serves both protocols
// (deepseek/zcode pattern). The OpenAI endpoint ignores x-api-key; this covers
// the Anthropic gateway whether it prefers Bearer or x-api-key.
//
// NOTE: login-time validation (validateKeyBearerGET) sends Bearer only — it
// probes the OpenAI /models endpoint, which is Bearer. Only the forward path
// (this method) dual-writes, because it must serve both protocols.
func (p *QwenPlanProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// RewriteRequest is a no-op: pure passthrough. The proxy selects the upstream
// base URL by protocol in proxy.forward (openai_base_url for OpenAI paths,
// anthropic_base_url for /v1/messages).
func (p *QwenPlanProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *QwenPlanProvider) Logout() error { return p.DeleteKey() }

// FetchModels lists models via the OpenAI-compatible /models endpoint. If the
// token-plan gateway hides the list for subscription plans (the Coding-Plan FAQ
// warns model lists may not be queryable), this errors and the caller (models
// refresh / usage display) falls back to the config models: list.
func (p *QwenPlanProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// Quota returns an unmeasured snapshot (no public Credits-usage API). The
// console subscription URL rides in Notes so both the CLI `usage` command and
// the Web UI (app.js renders snap.Notes) can link to the real 5h/7d numbers.
// BillingUnknown → the surplus scheduler ranks qwen-plan by priority (neutral);
// window exhaustion is handled reactively via 429 → rlQuota → cooldown/failover.
func (p *QwenPlanProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{
		Billing: BillingUnknown,
		Notes: []string{
			"Credits usage (5h/7d windows) is viewable only in the console",
			"Subscription details: " + qwenPlanConsoleURL,
		},
		AsOf: time.Now(),
	}, nil
}
