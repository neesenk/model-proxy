package provider

import (
	"net/http"
	"time"
)

// StaticProvider is for providers with a static key in config (no login/usage).
type StaticProvider struct {
	baseProbe
	cfg *Config
}

func init() {
	Register("static", func(cfg *Config, providerName string) (Provider, error) {
		return &StaticProvider{cfg: cfg}, nil
	})
}

// AuthHeaders injects the static key as Bearer (a static provider has no
// login/refresh; the key comes from config, default empty).
func (p *StaticProvider) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+p.cfg.StaticKey)
	req.Header.Del("x-api-key")
	return nil
}
func (p *StaticProvider) Refresh() error {
	return nil
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

// Quota: static providers have no measurable quota - always BillingUnknown.
func (p *StaticProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{Billing: BillingUnknown}, nil
}
func (p *StaticProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}

var errNotSupported = &notSupportedErr{}

type notSupportedErr struct{}

func (e *notSupportedErr) Error() string { return "not supported for this provider type" }
