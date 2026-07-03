package provider

import (
	"fmt"
	"net/http"
	"strings"
)

// CompassProvider implements the compass gateway provider (CQP key + SSO login).
// It wraps the existing CQPProvider logic in the main package via callbacks,
// avoiding a large code migration.
type CompassProvider struct {
	cfg          *Config
	providerName string
	// InjectFn is the main package's auth.Inject equivalent.
	InjectFn  func(req *http.Request) error
	RefreshFn func() error
	// LoginFn runs the SSO browser flow.
	LoginFn  func() error
	// LogoutFn clears the SSO cookie file.
	LogoutFn func() error
	// UsageFn queries monthly_usage.
	UsageFn  func() (any, error)
}

func (p *CompassProvider) AuthHeaders(req *http.Request) error {
	return p.InjectFn(req)
}
func (p *CompassProvider) Refresh() error {
	return p.RefreshFn()
}
func (p *CompassProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	// Compass + anthropic /messages needs ?beta=true.
	if strings.Contains(path, "/messages") && !strings.Contains(targetURL, "beta=") {
		if strings.Contains(targetURL, "?") {
			targetURL += "&beta=true"
		} else {
			targetURL += "?beta=true"
		}
	}
	return targetURL, body
}
func (p *CompassProvider) Login() error  { return p.LoginFn() }
func (p *CompassProvider) Logout() error { return p.LogoutFn() }
func (p *CompassProvider) Usage() (any, error) { return p.UsageFn() }

func init() {
	Register("compass", func(cfg *Config, providerName string) (Provider, error) {
		// The actual Inject/Refresh/Login/Logout/Usage functions are wired
		// by the main package via WireCompass().
		return &CompassProvider{cfg: cfg, providerName: providerName}, nil
	})
}

// WireCompass connects the main package's existing functions to the provider.
// Called during initialization (NewProxy or cmdLogin etc).
func WireCompass(p *CompassProvider, injectFn func(*http.Request) error, refreshFn func() error,
	loginFn func() error, logoutFn func() error, usageFn func() (any, error)) {
	p.InjectFn = injectFn
	p.RefreshFn = refreshFn
	p.LoginFn = loginFn
	p.LogoutFn = logoutFn
	p.UsageFn = usageFn
}

// ExtraHeaders for compass: anthropic-version + x-compass-request-id.
func CompassExtraHeaders(req *http.Request, authType string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	if authType == "compass" {
		// x-compass-request-id set by main package's newRequestID
	}
}

// Unused import guard
var _ = fmt.Sprintf
