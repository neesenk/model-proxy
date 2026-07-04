package provider

import (
	"net/http"
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
	cfg *Config
}

func init() {
	Register("deepseek", func(cfg *Config, providerName string) (Provider, error) {
		return &DeepSeekProvider{
			ApiKeyBase: NewApiKeyBase(providerName),
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

func (p *DeepSeekProvider) Login() error                   { return p.cfg.LoginFn() }
func (p *DeepSeekProvider) Logout() error                  { return p.cfg.LogoutFn() }
func (p *DeepSeekProvider) Usage() (any, error)            { return p.cfg.UsageFn() }
func (p *DeepSeekProvider) FetchModels() ([]string, error) { return fetchModelsBearer(p.cfg) }
func (p *DeepSeekProvider) Quota() (*QuotaSnapshot, error) { return p.cfg.QuotaOrUnknown() }
