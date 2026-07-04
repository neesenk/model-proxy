package provider

import (
	"fmt"
	"net/http"
)

// Authenticator is the auth-injection interface (matches main.AuthProvider).
// Main package passes its existing CQPProvider/CodexOAuthProvider/ApiKeyProvider
// via this interface, so provider/ doesn't need to re-implement them.
type Authenticator interface {
	Inject(req *http.Request) error
	Refresh() error
}

// Provider encapsulates all behavior for an upstream backend: auth, request
// rewriting, login, logout, usage queries, and model listing. Each provider
// implementation registers itself via Register() in init().
//
// Conventions for Usage(): the display function MUST print "Provider: <name>"
// as the FIRST line (via the UsageFn callback wired in buildProviders), so
// `usage` (no provider arg) produces consistent output across all providers.
// Other fields (Account, Plan, quota bars, etc.) follow after it.
type Provider interface {
	AuthHeaders(req *http.Request) error
	Refresh() error
	RewriteRequest(targetURL string, body []byte, path string) (string, []byte)
	Login() error
	Logout() error
	Usage() (any, error)
	FetchModels() ([]string, error)
}

// Config is the provider-level config data passed to constructors.
type Config struct {
	ProviderID    string
	OpenAIBaseURL string
	Headers       map[string]string
	UsageURL      string
	Models        map[string]any

	// Auth-specific fields (only relevant to certain providers).
	SSOCookieFile string // compass
	CQPMintURL    string // compass

	// Callbacks: main package wires its existing functions here so provider/
	// doesn't need to re-implement CQP minting, SSO flow, OAuth, etc.
	Auth          Authenticator            // for AuthHeaders/Refresh (compass, codex, apikey)
	LoginFn       func() error             // for Login (compass: SSO, codex: device flow, zhipu: prompt)
	LogoutFn      func() error             // for Logout
	UsageFn       func() (any, error)      // for Usage
	FetchModelsFn func() ([]string, error) // for FetchModels (volcengine: V4-signed OpenAPI)
}

// Constructor builds a Provider instance from config.
type Constructor func(cfg *Config, providerName string) (Provider, error)

var registry = map[string]Constructor{}

func Register(providerID string, fn Constructor) {
	registry[providerID] = fn
}

// New constructs a Provider from config. The main package must set cfg.Auth
// and the callback functions before calling New.
func New(cfg *Config, providerName string) (Provider, error) {
	fn, ok := registry[cfg.ProviderID]
	if !ok {
		return nil, fmt.Errorf("unsupported provider_id %q for provider %q", cfg.ProviderID, providerName)
	}
	return fn(cfg, providerName)
}

// AuthFilePath returns the credential file path for a provider name.
func AuthFilePath(providerName, suffix string) string {
	return fmt.Sprintf("~/.model-proxy/%s_%s.json", providerName, suffix)
}
