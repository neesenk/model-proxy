package provider

import (
	"net/http"
	"time"
)

// StepPlanProvider implements StepFun Step Plan (阶跃星辰 Step Plan): the
// subscription Credit-月池 coding-plan gateway at api.stepfun.com, distinct
// from the pay-as-you-go 开放平台 channel (https://api.stepfun.com/v1 — a
// different base, a different balance; visiting it never consumes plan
// Credits). One Step API key serves BOTH protocols on two per-protocol bases:
//   - openai_base_url (https://api.stepfun.com/step_plan/v1) → /chat/completions,
//     /models — uses Authorization: Bearer.
//   - anthropic_base_url (https://api.stepfun.com/step_plan, no /v1) →
//     /v1/messages — Claude Code reaches it via ANTHROPIC_AUTH_TOKEN (Bearer).
//
// The proxy selects the upstream base by protocol (proxy.forward); requests
// are byte-level passthrough (RewriteRequest no-op). Quota has NO public API:
// the Credit 月池 (monthly pool + 30d booster packs, 1M Credit = ¥1) is
// console-only — /v1/accounts reads the SEPARATE pay-as-you-go balance — so
// Quota() returns BillingUnknown carrying the console URL in Notes (qwen-plan
// pattern); exhaustion surfaces reactively as 402 quota_exceeded, classified
// by targetexec's body-proven quota-denied policy → cooldown + failover.
type StepPlanProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

// stepPlanConsoleURL is the Step Plan account usage/subscription page (Credit
// 月池 + booster packs; account-overview is where the platform surfaces
// per-account usage). Shown in the CLI `usage` output and the Web UI
// (QuotaSnapshot.Notes → app.js renders snap.Notes) because Credit numbers
// have no public API.
const stepPlanConsoleURL = "https://platform.stepfun.com/account-overview"

func init() {
	Register("step-plan", func(cfg *Config, providerName string) (Provider, error) {
		return &StepPlanProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// AuthHeaders injects the key as BOTH Authorization: Bearer (OpenAI endpoint —
// the documented auth for /chat/completions — and the login-time /models
// validation probe) and x-api-key (Anthropic endpoint), so one config serves
// both protocols (deepseek/qwen-plan pattern). The OpenAI endpoint ignores
// x-api-key; the Anthropic gateway is reached by Claude Code via Bearer, so
// dual-writing covers it whether it prefers Bearer or x-api-key.
func (p *StepPlanProvider) AuthHeaders(req *http.Request) error {
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
func (p *StepPlanProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *StepPlanProvider) Logout() error { return p.DeleteKey() }

// FetchModels lists models via the OpenAI-compatible /models endpoint. If the
// step_plan channel hides the list, this errors and the caller (models
// refresh / usage display) falls back to the config models: list (qwen-plan
// pattern).
func (p *StepPlanProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// ProbeRequest returns the ANTHROPIC probe shape (/v1/messages), not baseProbe's
// OpenAI /chat/completions: probeModelCallable selects the anthropic base URL
// when anthropic_base_url is set (step-plan's primary path for Claude Code),
// and OpenAI's /chat/completions path on the anthropic base would 404 for
// every model (qwen-plan/volcengine lesson).
func (p *StepPlanProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   AnthropicProbeBody(modelID),
	}
}

// ExtraHeaders sets anthropic-version on every upstream request (forward +
// probe). The probe has no client request to inherit it from, and
// anthropic-compatible endpoints expect it (harmless on the OpenAI path).
func (p *StepPlanProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

// Quota returns an unmeasured snapshot (no public Credit API — /v1/accounts
// reads the independent pay-as-you-go balance, NOT the plan 月池). The console
// subscription URL rides in Notes so both the CLI `usage` command and the Web
// UI (app.js renders snap.Notes) can link to the real Credit numbers.
// BillingUnknown → the surplus scheduler ranks step-plan by priority
// (neutral); exhaustion is handled reactively via 402 quota_exceeded →
// quota cooldown → failover.
func (p *StepPlanProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{
		Billing: BillingUnknown,
		Notes: []string{
			"Step Plan Credit usage (月池 + booster packs) is viewable only in the console",
			"Usage & subscription: " + stepPlanConsoleURL,
		},
		AsOf: time.Now(),
	}, nil
}
