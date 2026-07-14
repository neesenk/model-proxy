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

// Provider encapsulates all behavior for an upstream backend: auth, request
// rewriting, logout, usage queries, and model listing. Each provider
// implementation registers itself via Register() in init().
//
// Login is NOT on this interface: the interactive login flow (SSO / OAuth /
// stdin) is CLI/IO orchestration owned by main's `login` command (cmdLogin ->
// run*), which never goes through a provider instance.
//
// Conventions for Usage(): the display method MUST print "Provider: <name>"
// as the FIRST line, so `usage` (no provider arg) produces consistent output
// across all providers. Other fields (Account, Plan, quota bars, etc.) follow.
type Provider interface {
	AuthHeaders(req *http.Request) error
	Refresh() error
	RewriteRequest(targetURL string, body []byte, path string) (string, []byte)
	Logout() error
	Usage() error
	FetchModels() ([]string, error)
	Quota() (*QuotaSnapshot, error)

	// ProbeRequest returns the minimal request pieces to probe whether `modelID`
	// is callable on this provider's own endpoint - used by `models refresh`'s
	// endpoint probe. The caller selects the base URL by protocol (anthropic vs
	// openai) and appends Path. Embed baseProbe for the default (OpenAI
	// /chat/completions + a minimal body); override for providers whose probe
	// path/body differ (codex /responses, aqp /v1/messages).
	ProbeRequest(modelID string) ProbeRequest

	// ExtraHeaders sets provider-specific headers that EVERY upstream request
	// needs (forward + probe paths) - e.g. aqp's anthropic-version +
	// x-compass-request-id. Default (baseProbe) is a no-op. Called after
	// AuthHeaders + prov.Headers so providers can layer on top.
	ExtraHeaders(req *http.Request, path string)

	// FilterModelIDs applies provider-specific static policy filters (regex
	// rules) to a candidate model-id list, returning (kept, dropped). This is
	// the "policy" pass in `models refresh` (before the endpoint probe). Default
	// (baseProbe) passes through; volcengine overrides to drop *-latest /
	// doubao-seed-1-* / lite / mini.
	FilterModelIDs(ids []string) (kept, dropped []string)
}

// ProbeRequest is the minimal request pieces for probing one model's
// callability on a provider's endpoint. The caller builds base+Path, sets
// Content-Type/Content-Length/Accept, calls AuthHeaders + ExtraHeaders, then Do.
type ProbeRequest struct {
	Method string // http.Method (default POST)
	Path   string // path relative to the selected base URL, e.g. "/chat/completions"
	Body   []byte // minimal request body for this provider's chat shape
}

// Config is the provider-level config data passed to constructors.
type Config struct {
	ProviderID    string
	ProviderName  string // the config top-level key (for display "Provider: <name>")
	OpenAIBaseURL string
	Headers       map[string]string
	UsageURL      string

	// ClientVersion is the codex /models client_version query param, resolved
	// by the main package (config > codex CLI > ~/.codex cache > constant) and
	// passed in via buildOne. Unused by other providers.
	ClientVersion string

	// BoundAPIKey binds an in-memory API key for apikey providers (zhipu,
	// deepseek, volcengine) when unrolled from a credential-pool entry. When
	// non-empty, the constructor builds an ApiKeyBase bound to this key (file
	// reads/writes skipped) — this is the FORWARD path binding (AuthHeaders
	// reads the key via the embedded ApiKeyBase, NOT via Auth). Empty = legacy
	// file-backed behavior.
	BoundAPIKey string

	// Auth-specific fields (only relevant to certain providers).
	OAuthAuthFile string // aqp/codex: the <name>_oauth_auth.json store path
	AqpMintURL    string // aqp: the api_key/get_or_generate endpoint
	StaticKey     string // static: a config static key (no login)

	// Volcengine signing keys for GetAFPUsage (the Agent Plan quota endpoint,
	// V4-signed, needs AK/SK not the Bearer chat key). Bound per virtual from
	// the credential pool's cred entry in buildOne. Empty = not configured.
	AccessKey string
	SecretKey string
	// VolcengineCredFile is the legacy <name>_apikey.json path, read for AK/SK
	// when AccessKey/SecretKey are empty (the single-account / pre-pool path).
	// The file read stays in the provider package (it's pure file I/O).
	VolcengineCredFile string

	// AqpMonthlyUsage fetches the monthly_usage payload (SSO-cookie POST,
	// project_id-scoped). Wired in buildOne from the main-package AqpClient
	// (shared with the login/web flows). nil for non-aqp providers.
	AqpMonthlyUsage func() (*MonthlyProjectUsage, error)

	// AqpAccount returns the logged-in account's email, project_id, and store
	// file path (for the aqp usage display header). nil/err for non-aqp or when
	// not logged in. Wired in buildOne from the main-package loadAccount.
	AqpAccount func() (email, projectID, storePath string, err error)

	// Models is the config model-id list, used by the usage display's fallback
	// (listConfigModels) when a provider can't fetch a structured quota
	// (e.g. volcengine without AK/SK).
	Models []string

	// Callbacks: main wires volcengine's V4-signed FetchModels here, plus aqp's
	// SSO-cookie monthly_usage fetcher (AqpMonthlyUsage) and account reader
	// (AqpAccount). The interactive login flow is NOT a callback - cmdLogin
	// calls the run* flows directly. Logout is provider-owned (file removal)
	// since Phase 5.
	FetchModelsFn func() ([]string, error) // for FetchModels (volcengine: V4-signed OpenAPI)

	// Auth is an optional auth-injector override (TEST SEAM): when set, the aqp/
	// codex constructors use it instead of building their real AqpKeyProvider /
	// CodexOAuthProvider. Production (buildOne) leaves it nil so the real
	// provider-owned auth is used; forward-path integration tests set a fake.
	Auth authInjector
}

// authOrDefault returns the cfg.Auth override when set (test seam), else the
// real provider-owned auth injector passed in.
func (c *Config) authOrDefault(real authInjector) authInjector {
	if c.Auth != nil {
		return c.Auth
	}
	return real
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
