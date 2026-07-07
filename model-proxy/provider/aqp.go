package provider

import (
	"net/http"
	"strings"
	"time"
)

// AqpProvider wraps the main package's AQP auth + SSO login + monthly_usage.
type AqpProvider struct {
	cfg *Config
}

func init() {
	Register("aqp", func(cfg *Config, providerName string) (Provider, error) {
		return &AqpProvider{cfg: cfg}, nil
	})
}

func (p *AqpProvider) AuthHeaders(req *http.Request) error {
	return p.cfg.Auth.Inject(req)
}
func (p *AqpProvider) Refresh() error {
	return p.cfg.Auth.Refresh()
}
func (p *AqpProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	if strings.Contains(path, "/messages") && !strings.Contains(targetURL, "beta=") {
		if strings.Contains(targetURL, "?") {
			targetURL += "&beta=true"
		} else {
			targetURL += "?beta=true"
		}
	}
	return targetURL, body
}
func (p *AqpProvider) Login() error                   { return p.cfg.LoginFn() }
func (p *AqpProvider) Logout() error                  { return p.cfg.LogoutFn() }
func (p *AqpProvider) Usage() (any, error)            { return p.cfg.UsageFn() }
func (p *AqpProvider) FetchModels() ([]string, error) { return fetchModelsBearer(p.cfg) }
func (p *AqpProvider) Quota() (*QuotaSnapshot, error) { return p.cfg.QuotaOrUnknown() }
func (p *AqpProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}
