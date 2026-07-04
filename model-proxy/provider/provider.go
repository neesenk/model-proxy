package provider

import (
	"fmt"
	"net/http"
	"time"
)

// BillingClass tiers providers for scheduling: plan providers are ranked by
// remaining quota, unknown ones by priority, pay-as-you-go is strict last resort.
type BillingClass int

const (
	BillingUnknown BillingClass = iota // can't measure (no AK/SK, not logged in, poll failed, stale)
	BillingPlan                        // Coding/Agent Plan: windowed quotas
	BillingPayG                        // pay-as-you-go: strict last-resort
)

// QuotaDetail is one line of a per-window breakdown (per-model tokens, per-tool time).
type QuotaDetail struct {
	Label string
	Used  float64
}

// QuotaWindow is one normalized quota window (5h / weekly / monthly / spend / balance).
type QuotaWindow struct {
	Label        string        // "5h tokens", "Weekly tokens", "Monthly time", "Spend", "Balance"
	Kind         string        // "tokens" | "time" | "money"
	Used         float64
	Total        float64
	RemainingPct float64       // 0..1; -1 if unmeasured (e.g. balance-only)
	ResetsAt     time.Time     // zero if unknown
	Details      []QuotaDetail
	DetailLabel  string        // breakdown header for display ("By model", "By MCP tool"); "" omits
}

// QuotaSnapshot is the normalized, polled quota for one provider. It carries
// enough detail for both scheduling (Billing, RemainingPct) and the `usage`
// display (Account, Plan, Level, Windows, Notes).
type QuotaSnapshot struct {
	Billing      BillingClass
	RemainingPct float64 // binding min over windows; -1 if unknown
	Account      string
	Plan         string
	Level        string
	Windows      []QuotaWindow
	Notes        []string // provider-specific status lines for display
	AsOf         time.Time
	Err          string
}

// BindingRemaining returns the minimum RemainingPct across windows whose
// RemainingPct >= 0 (the binding constraint). Returns -1 if none measured.
func BindingRemaining(windows []QuotaWindow) float64 {
	min := -1.0
	for _, w := range windows {
		if w.RemainingPct < 0 {
			continue
		}
		if min < 0 || w.RemainingPct < min {
			min = w.RemainingPct
		}
	}
	return min
}

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
	Quota() (*QuotaSnapshot, error)
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
	QuotaFn       func() (*QuotaSnapshot, error) // for Quota (structured quota for scheduling + display)
}

// QuotaOrUnknown returns cfg.QuotaFn()'s snapshot, or a BillingUnknown snapshot
// when QuotaFn is unset (static / not-yet-wired providers) — never panics.
func (c *Config) QuotaOrUnknown() (*QuotaSnapshot, error) {
	if c.QuotaFn == nil {
		return &QuotaSnapshot{Billing: BillingUnknown}, nil
	}
	return c.QuotaFn()
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
