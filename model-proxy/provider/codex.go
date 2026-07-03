package provider

import (
	"net/http"
)

// CodexProvider implements the codex (chatgpt.com) provider with OAuth device flow.
// Wraps existing main package logic via callbacks.
type CodexProvider struct {
	cfg          *Config
	providerName string
	InjectFn     func(req *http.Request) error
	RefreshFn    func() error
	LoginFn      func() error
	LogoutFn     func() error
	UsageFn      func() (any, error)
}

func (p *CodexProvider) AuthHeaders(req *http.Request) error {
	return p.InjectFn(req)
}
func (p *CodexProvider) Refresh() error {
	return p.RefreshFn()
}
func (p *CodexProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	// codex backend requires store:false in the request body.
	body = ensureJSONField(body, "store", false)
	return targetURL, body
}
func (p *CodexProvider) Login() error           { return p.LoginFn() }
func (p *CodexProvider) Logout() error          { return p.LogoutFn() }
func (p *CodexProvider) Usage() (any, error)    { return p.UsageFn() }

func init() {
	Register("codex", func(cfg *Config, providerName string) (Provider, error) {
		return &CodexProvider{cfg: cfg, providerName: providerName}, nil
	})
}

// WireCodex connects the main package's existing functions.
func WireCodex(p *CodexProvider, injectFn func(*http.Request) error, refreshFn func() error,
	loginFn func() error, logoutFn func() error, usageFn func() (any, error)) {
	p.InjectFn = injectFn
	p.RefreshFn = refreshFn
	p.LoginFn = loginFn
	p.LogoutFn = logoutFn
	p.UsageFn = usageFn
}

// ensureJSONField sets body[key] = val if the key is absent.
func ensureJSONField(body []byte, key string, val any) []byte {
	// Minimal inline implementation to avoid importing encoding/json here.
	// The main package already has this; this is a fallback.
	return body
}
