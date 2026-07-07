package provider

import (
	"fmt"
	"net/http"
	"time"
)

// VolcengineProvider implements the Volcengine Ark (火山方舟) provider, including
// the "Agent Plan" subscription tier. A single Ark API key authenticates both
// protocols from one config entry:
//   - openai_base_url (e.g. https://ark.cn-beijing.volces.com/api/plan/v3) →
//     /chat/completions, /responses, /models — uses Authorization: Bearer.
//   - anthropic_base_url (e.g. https://ark.cn-beijing.volces.com/api/plan/compatible)
//     → /v1/messages (Anthropic-compatible, for Claude Code) — uses x-api-key.
//
// The proxy selects the upstream base by protocol (see proxy.forward). For
// anthropic, the proxy keeps the client's /v1 path (base_url + /v1/messages),
// so anthropic_base_url should NOT include /v1. Both auth headers are set on
// every request so one config serves both protocols (the OpenAI endpoint
// ignores x-api-key; the Anthropic-compatible endpoint reads x-api-key).
type VolcengineProvider struct {
	*ApiKeyBase
	cfg *Config
}

func init() {
	Register("volcengine", func(cfg *Config, providerName string) (Provider, error) {
		return &VolcengineProvider{
			ApiKeyBase: newApiKeyBaseBound(cfg, providerName),
			cfg:        cfg,
		}, nil
	})
}

// AuthHeaders injects the API key as BOTH Authorization: Bearer (OpenAI endpoint)
// and x-api-key (Anthropic-compatible endpoint), so one provider config serves
// both protocols. Overrides ApiKeyBase.AuthHeaders (Bearer only).
func (p *VolcengineProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by protocol.
func (p *VolcengineProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *VolcengineProvider) Login() error                   { return p.cfg.LoginFn() }
func (p *VolcengineProvider) Logout() error                  { return p.cfg.LogoutFn() }
func (p *VolcengineProvider) Usage() (any, error)            { return p.cfg.UsageFn() }
func (p *VolcengineProvider) Quota() (*QuotaSnapshot, error) { return p.cfg.QuotaOrUnknown() }
func (p *VolcengineProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}
func (p *VolcengineProvider) FetchModels() ([]string, error) {
	if p.cfg.FetchModelsFn != nil {
		return p.cfg.FetchModelsFn()
	}
	return nil, fmt.Errorf("FetchModelsFn not configured")
}
