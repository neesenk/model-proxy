package provider

import (
	"net/http"
	"strings"
)

// CompassProvider wraps the main package's CQP auth + SSO login + monthly_usage.
type CompassProvider struct {
	cfg *Config
}

func init() {
	Register("compass", func(cfg *Config, providerName string) (Provider, error) {
		return &CompassProvider{cfg: cfg}, nil
	})
}

func (p *CompassProvider) AuthHeaders(req *http.Request) error {
	return p.cfg.Auth.Inject(req)
}
func (p *CompassProvider) Refresh() error {
	return p.cfg.Auth.Refresh()
}
func (p *CompassProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	if strings.Contains(path, "/messages") && !strings.Contains(targetURL, "beta=") {
		if strings.Contains(targetURL, "?") {
			targetURL += "&beta=true"
		} else {
			targetURL += "?beta=true"
		}
	}
	return targetURL, body
}
func (p *CompassProvider) Login() error                   { return p.cfg.LoginFn() }
func (p *CompassProvider) Logout() error                  { return p.cfg.LogoutFn() }
func (p *CompassProvider) Usage() (any, error)            { return p.cfg.UsageFn() }
func (p *CompassProvider) FetchModels() ([]string, error) { return fetchModelsBearer(p.cfg) }
