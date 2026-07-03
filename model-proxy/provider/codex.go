package provider

import (
	"net/http"
)

// CodexProvider wraps the main package's codex OAuth + device flow + wham/usage.
type CodexProvider struct {
	cfg *Config
}

func init() {
	Register("codex", func(cfg *Config, providerName string) (Provider, error) {
		return &CodexProvider{cfg: cfg}, nil
	})
}

func (p *CodexProvider) AuthHeaders(req *http.Request) error {
	return p.cfg.Auth.Inject(req)
}
func (p *CodexProvider) Refresh() error {
	return p.cfg.Auth.Refresh()
}
func (p *CodexProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	// codex backend requires store:false (injected by main package's ensureJSONField).
	// The main package already handles this in forward(); we return body as-is here
	// to avoid double-processing. TODO: move ensureJSONField into provider/.
	return targetURL, body
}
func (p *CodexProvider) Login() error       { return p.cfg.LoginFn() }
func (p *CodexProvider) Logout() error       { return p.cfg.LogoutFn() }
func (p *CodexProvider) Usage() (any, error) { return p.cfg.UsageFn() }
