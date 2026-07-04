package provider

import (
	"net/http"
)

// StaticProvider is for providers with a static key in config (no login/usage).
type StaticProvider struct {
	cfg *Config
}

func init() {
	Register("static", func(cfg *Config, providerName string) (Provider, error) {
		return &StaticProvider{cfg: cfg}, nil
	})
}

func (p *StaticProvider) AuthHeaders(req *http.Request) error {
	return p.cfg.Auth.Inject(req)
}
func (p *StaticProvider) Refresh() error {
	return p.cfg.Auth.Refresh()
}
func (p *StaticProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}
func (p *StaticProvider) Login() error {
	return errNotSupported
}
func (p *StaticProvider) Logout() error {
	return errNotSupported
}
func (p *StaticProvider) Usage() (any, error) {
	return nil, errNotSupported
}
func (p *StaticProvider) FetchModels() ([]string, error) { return nil, errNotSupported }

var errNotSupported = &notSupportedErr{}

type notSupportedErr struct{}

func (e *notSupportedErr) Error() string { return "not supported for this provider type" }
