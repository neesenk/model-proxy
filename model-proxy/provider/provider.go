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
	Label        string // "5h tokens", "Weekly tokens", "Monthly time", "Spend", "Balance"
	Kind         string // "tokens" | "time" | "money"
	Used         float64
	Total        float64
	RemainingPct float64 // 0..1; -1 if unmeasured (e.g. balance-only)
	ResetsAt     time.Time
	Details      []QuotaDetail
	DetailLabel  string // breakdown header for display ("By model", "By MCP tool"); "" omits
	// Scheduling markers, set by the provider parser:
	Ultimate bool          // total-budget window — scheduling base + pace source
	Short    bool          // immediate rate-cap window — peak-burn numerator
	Duration time.Duration // nominal reset cycle (5h / 24h / 7d / 30d) for pace (f_left)
}

// QuotaSnapshot is the normalized, polled quota for one provider. It carries
// enough detail for both scheduling (Billing, RemainingPct) and the `usage`
// display (Account, Plan, Level, Windows, Notes).
type QuotaSnapshot struct {
	Billing      BillingClass
	RemainingPct float64 // ultimate (total-budget) window remaining; -1 if unknown
	Account      string
	Plan         string
	Level        string
	Windows      []QuotaWindow
	Notes        []string // provider-specific status lines for display
	AsOf         time.Time
	Err          string
}

// Surplus is the scheduling pace-score derived from this snapshot:
//
//	remaining = ultimate.remaining − short.remaining × (short.total / ultimate.total) × (peakMult − 1)
//	fLeft     = clamp((ultimate.reset − now) / ultimate.duration, 0, 1)   // window time-left fraction
//	surplus   = remaining − fLeft
//
// surplus > 0: under pace (budget would be wasted at reset → prioritize);
// surplus < 0: over pace (will exhaust before reset → avoid);
// surplus ≈ 0: on pace. Higher surplus sorts first. Returns 0 (neutral) when the
// snapshot isn't a measured plan, or the ultimate window's reset cycle is unknown.
func (s *QuotaSnapshot) Surplus(now time.Time, peakMult float64) float64 {
	if s == nil || s.Billing != BillingPlan || s.RemainingPct < 0 {
		return 0
	}
	var ult *QuotaWindow
	var shorts []QuotaWindow
	for i := range s.Windows {
		if s.Windows[i].Ultimate {
			ult = &s.Windows[i]
		} else if s.Windows[i].Short {
			shorts = append(shorts, s.Windows[i])
		}
	}
	if ult == nil || ult.RemainingPct < 0 || ult.Duration <= 0 || ult.ResetsAt.IsZero() {
		return 0
	}
	remaining := ult.RemainingPct
	if peakMult > 1 && ult.Total > 0 {
		// Peak burns each short rate-cap window's remaining, scaled to the total
		// budget. Multiple shorts sum (independent rate caps); windows that are
		// neither Ultimate nor Short (e.g. volcengine daily/weekly intermediates)
		// are ignored.
		for _, sh := range shorts {
			if sh.RemainingPct >= 0 && sh.Total > 0 {
				remaining -= sh.RemainingPct * (sh.Total / ult.Total) * (peakMult - 1)
			}
		}
	}
	fLeft := ult.ResetsAt.Sub(now).Seconds() / ult.Duration.Seconds()
	if fLeft < 0 {
		fLeft = 0
	} else if fLeft > 1 {
		fLeft = 1
	}
	return remaining - fLeft
}

// Authenticator is the auth-injection interface (matches main.AuthProvider).
// Main package passes its existing AqpKeyProvider/CodexOAuthProvider/ApiKeyProvider
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
	Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64
}

// Config is the provider-level config data passed to constructors.
type Config struct {
	ProviderID    string
	OpenAIBaseURL string
	Headers       map[string]string
	UsageURL      string
	Models        map[string]any

	// Auth-specific fields (only relevant to certain providers).
	SSOCookieFile string // aqp
	AqpMintURL    string // aqp

	// Callbacks: main package wires its existing functions here so provider/
	// doesn't need to re-implement AQP minting, SSO flow, OAuth, etc.
	Auth          Authenticator                  // for AuthHeaders/Refresh (aqp, codex, apikey)
	LoginFn       func() error                   // for Login (aqp: SSO, codex: device flow, zhipu: prompt)
	LogoutFn      func() error                   // for Logout
	UsageFn       func() (any, error)            // for Usage
	FetchModelsFn func() ([]string, error)       // for FetchModels (volcengine: V4-signed OpenAPI)
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
