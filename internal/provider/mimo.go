package provider

import (
	"fmt"
	"net/http"
	"time"

	"model-proxy/internal/display"
)

// MiMoProvider implements the Xiaomi MiMo provider (小米 MiMo 开放平台,
// https://mimo.mi.com), PAY-AS-YOU-GO mode only. One API key serves BOTH
// protocols on two per-protocol bases (the deepseek pattern):
//   - openai_base_url (https://api.xiaomimimo.com/v1) → /chat/completions,
//     /responses, /models — uses Authorization: Bearer (or the vendor's
//     api-key header; both are documented auth methods).
//   - anthropic_base_url (https://api.xiaomimimo.com/anthropic, no /v1) →
//     /v1/messages — Claude Code reaches it via the Anthropic SDK convention
//     (x-api-key / Bearer).
//
// The proxy selects the upstream base by protocol (see proxy.forward); requests
// are byte-level passthrough (RewriteRequest no-op).
//
// Billing: pay-as-you-go only, with NO public balance API. The console's
// balance/usage endpoints (platform.xiaomimimo.com/api/v1/balance and
// .../tokenPlan/usage) are gated on the browser SSO session cookie
// (api-platform serviceToken), NOT on the API key — verified live 2026-09:
// every auth-header spelling answers 401 with a loginUrl redirect, and the
// same paths 404 on the api.xiaomimimo.com inference host. The community
// "GET /api/v1/balance with Bearer" recipe therefore does not work for API
// keys (cc-switch issue #2488 reaches the same conclusion). Quota() therefore
// returns BillingUnknown carrying the console URL in Notes — the
// qwen-plan/step-plan pattern — and exhaustion surfaces reactively as the
// upstream 402 (see the Quota comment). The console is deliberately NOT
// scraped for cookies (same decision as qwen-plan, see
// docs/decisions/intentional-behaviors.md).
//
// ProbeRequest / FilterModelIDs keep the baseProbe defaults, like deepseek:
// probe.Callable forces /v1/messages + the anthropic body when
// anthropic_base_url is set and ProbeModelProtocols probes each leg on its own
// base, so an override would be dead weight. ExtraHeaders IS overridden — see
// the method comment (the forward path drops the client's anthropic-version).
type MiMoProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

// mimoConsoleURL is the MiMo console balance page. Shown in the CLI `usage`
// output and the Web UI (QuotaSnapshot.Notes → app.js renders snap.Notes)
// because the balance has no public API.
const mimoConsoleURL = "https://platform.xiaomimimo.com/#/console/balance"

func init() {
	Register("mimo", func(cfg *Config, providerName string) (Provider, error) {
		return &MiMoProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// AuthHeaders injects the key as ALL documented MiMo auth shapes:
// Authorization: Bearer (OpenAI endpoint — the documented auth for
// /chat/completions — and the login-time /models validation probe), x-api-key
// (Anthropic SDK convention on the anthropic base), and api-key (the vendor's
// OWN primary documented header — MiMo documents `api-key: $MIMO_API_KEY`
// first on BOTH the OpenAI and the Anthropic compatibility pages, unlike
// deepseek which only reads Bearer/x-api-key). Endpoints ignore the shapes
// they don't read, so one config serves every MiMo surface (deepseek
// dual-write pattern plus the vendor's api-key spelling, since a gateway that
// reads only its own documented header would otherwise reject the anthropic
// path).
func (p *MiMoProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	req.Header.Set("api-key", key)
	return nil
}

// RewriteRequest is a no-op: pure passthrough. The proxy selects the upstream
// base URL by protocol in proxy.forward (openai_base_url for OpenAI paths,
// anthropic_base_url for /v1/messages).
func (p *MiMoProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *MiMoProvider) Logout() error { return p.DeleteKey() }

// FetchModels lists models via the OpenAI-compatible /models endpoint. If the
// gateway hides the list, this errors and the caller (models refresh / usage
// display) falls back to the config models: list.
func (p *MiMoProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// ExtraHeaders sets anthropic-version on every upstream request (forward +
// probe), like every other anthropic-compatible provider here (kimi-code,
// qwen-plan, step-plan, volcengine, aqp, zcode) — deepseek is the documented
// exception because its gateway ignores the header. The forward path's
// client→upstream header whitelist (targetexec.upstreamHeaderWhitelist) does
// NOT carry anthropic-version, so without this the proxy would strip the
// version header the Anthropic SDK (Claude Code's MiMo path) always sends, and
// a strict gateway would 400. Harmless where the endpoint ignores it.
func (p *MiMoProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

// Quota returns an unmeasured snapshot: MiMo's balance/usage endpoints are
// console-only (browser SSO cookie, not the API key — see the type comment),
// so there is nothing an API key can poll. The console URL rides in Notes so
// both the CLI `usage` command and the Web UI (app.js renders snap.Notes) can
// link to the real numbers (qwen-plan/step-plan pattern). BillingUnknown →
// the surplus scheduler ranks MiMo by priority (neutral); exhaustion is
// handled reactively: the upstream answers 402 "Insufficient Balance", which
// targetexec's body-proven quota-denied policy already classifies (its marker
// table covers insufficient balance / account balance / 余额不足) → quota
// cooldown + failover, no MiMo-specific code.
func (p *MiMoProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{
		Billing:      BillingUnknown,
		RemainingPct: -1, // the documented "unknown" sentinel — 0 would read as "exhausted"
		Notes: []string{
			"MiMo balance is viewable only in the console (no API-key billing endpoint)",
			"Balance & recharge: " + mimoConsoleURL,
		},
		AsOf: time.Now(),
	}, nil
}

// Usage prints the console pointer and the config model list (qwen-plan/
// step-plan pattern) so `usage mimo` is never mute.
func (p *MiMoProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	fmt.Printf("%s pay-as-you-go (no API-key billing endpoint)\n", display.Dim("Billing:    "))
	s, err := p.Quota()
	if err != nil || s == nil {
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Red("(unavailable)"))
	} else {
		for _, n := range s.Notes {
			fmt.Println(display.Dim(display.Pad("", 18)) + n)
		}
	}
	listConfigModels(p.cfg.Models)
	return nil
}
