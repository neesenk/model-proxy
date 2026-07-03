package provider

import (
	"fmt"
	"net/http"
)

// Provider encapsulates all behavior for an upstream backend: auth, request
// rewriting, login, logout, and usage queries. Each provider implementation
// registers itself via Register() in init().
type Provider interface {
	// AuthHeaders injects auth headers into the upstream request.
	AuthHeaders(req *http.Request) error
	// Refresh re-acquires credentials (called on upstream 401).
	Refresh() error
	// RewriteRequest modifies the target URL and/or request body before
	// forwarding (e.g. codex injects store:false; compass adds ?beta=true).
	// Returns the (possibly modified) URL and body.
	RewriteRequest(targetURL string, body []byte, path string) (string, []byte)
	// Login performs interactive login (prompt, browser flow, device flow, etc.).
	Login() error
	// Logout clears stored credentials.
	Logout() error
	// Usage queries usage/balance/credits from the provider's server.
	// Returns data for the caller to render.
	Usage() (any, error)
}

// Config is the provider-level config data passed to constructors.
// Main package populates this from its own Config type.
type Config struct {
	ProviderID  string
	BaseURL     string
	Headers     map[string]string
	UsageURL    string
	Models      map[string]any

	// Auth-specific fields (only relevant to certain providers).
	SSOCookieFile string // compass
	CQPMintURL    string // compass
}

// Constructor builds a Provider instance from config.
type Constructor func(cfg *Config, providerName string) (Provider, error)

var registry = map[string]Constructor{}

// Register registers a provider implementation under a provider_id.
// Called from init() in each provider file.
func Register(providerID string, fn Constructor) {
	registry[providerID] = fn
}

// New looks up the provider by providerName in cfg, finds the implementation
// via provider_id, and constructs the instance.
func New(cfg *Config, providerName string) (Provider, error) {
	fn, ok := registry[cfg.ProviderID]
	if !ok {
		return nil, fmt.Errorf("unsupported provider_id %q for provider %q", cfg.ProviderID, providerName)
	}
	return fn(cfg, providerName)
}

// AuthFilePath returns the credential file path for a provider name.
// OAuth providers use <name>_oauth_auth.json; apikey providers use <name>_apikey.json.
func AuthFilePath(providerName, suffix string) string {
	return fmt.Sprintf("~/.model-proxy/%s_%s.json", providerName, suffix)
}
