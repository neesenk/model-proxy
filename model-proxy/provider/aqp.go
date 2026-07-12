package provider

import (
	"net/http"
	"strings"
	"time"
)

// AqpProvider wraps the main package's AQP auth + SSO login + monthly_usage.
type AqpProvider struct {
	baseProbe
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

// ProbeRequest overrides the OpenAI default: aqp speaks the Anthropic messages
// API, so the probe goes to /v1/messages (base does NOT include /v1; the SDK
// appends it) with an anthropic body. Mirrors forward's anthropic path.
func (p *AqpProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   anthropicProbeBody(modelID),
	}
}

// ExtraHeaders sets aqp's per-request headers: anthropic-version + a fresh
// x-compass-request-id UUID. Applied on EVERY upstream request (forward + probe)
// so the two paths share one implementation - no duplicated aqp branch in main.
func (p *AqpProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-compass-request-id", newRequestID())
}
