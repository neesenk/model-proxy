package provider

import (
	"net/http"
)

// StaticProvider authenticates with a credential-pool API key (the
// <name>_apikeys.json pool written by `login`, bound into cfg.BoundAPIKey at
// build time). It has no login/refresh/usage/quota of its own — no key is ever
// stored in config.yaml.
type StaticProvider struct {
	baseProbe
	cfg *Config
}

func init() {
	Register("static", func(cfg *Config, providerName string) (Provider, error) {
		return &StaticProvider{cfg: cfg}, nil
	})
}

// AuthHeaders injects the pool-bound API key as Bearer. The key comes from the
// credential pool (written by `login`), bound into cfg.BoundAPIKey at build
// time; config.yaml never holds a key.
func (p *StaticProvider) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+p.cfg.BoundAPIKey)
	req.Header.Del("x-api-key")
	return nil
}
func (p *StaticProvider) Refresh() error {
	return nil
}
func (p *StaticProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}
func (p *StaticProvider) Logout() error {
	return errNotSupported
}
func (p *StaticProvider) Usage() error {
	return errNotSupported
}

// FetchModels lists models from the OpenAI-compatible /models endpoint on the
// configured base with the pool-bound Bearer key — most static endpoints
// (OpenAI-compatible gateways) serve it. A gateway that hides the list
// errors, and the callers (models refresh / the Web add-provider preview)
// fall back to their configured/probed sets.
func (p *StaticProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// Quota: static providers have no measurable quota - always BillingUnknown.
// The Note is implementation-owned knowledge (this provider has no usage
// surface at all), so the CLI and Web UI can say so instead of rendering an
// empty unmeasured section. Billing/quota display is driven by what the
// implementation knows, never by the config `billing:` label.
func (p *StaticProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{
		Billing:      BillingUnknown,
		RemainingPct: -1,
		Notes:        []string{"static provider: no usage/quota surface (the key lives in the provider headers)"},
	}, nil
}

var errNotSupported = &notSupportedErr{}

type notSupportedErr struct{}

func (e *notSupportedErr) Error() string { return "not supported for this provider type" }
