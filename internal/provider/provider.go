package provider

import (
	"context"
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
	// ExhaustionEta is the predicted ultimate-window exhaustion time, derived
	// from the burn rate (Δused/Δt) between the previous and this snapshot —
	// see EstimateExhaustionEta. Zero when there is no valid prediction (first
	// snapshot, flat/decreasing usage, or a poll gap > 3×poll_interval).
	// Display-only: scheduling never reads it, and it is not persisted.
	ExhaustionEta time.Time
	// UsageFrom is the current billing period's start (UsageWindow's derived
	// ResetsAt−Duration), set by the admin Dashboard projection at request
	// time so consumers can align usage queries with the quota window without
	// re-deriving the reset cycle. Zero when not resolvable (not a measured
	// plan, poll error, no usable ultimate reset/cycle). Runtime state never
	// sets it; scheduling and persistence never read it.
	UsageFrom time.Time
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

// UsageWindow returns the start of the CURRENT billing period: the ultimate
// window's last reset (ResetsAt − Duration). It reads the same ultimate
// window Surplus/fLeft pace against — Duration is the nominal reset cycle
// the provider parsers set (5h/24h/7d/30d), so 7d and 30d plans resolve
// distinctly — giving usage views (Web accounts tab) one authoritative way
// to align their query window with the quota window instead of all-time
// counters. ok is false when the snapshot is not a measured plan account,
// carries a poll error, has no usable ultimate reset/cycle, or the derived
// start is not in the past (broken poll data, e.g. a reset >1 cycle out).
func (s *QuotaSnapshot) UsageWindow(now time.Time) (time.Time, bool) {
	if s == nil || s.Billing != BillingPlan || s.Err != "" {
		return time.Time{}, false
	}
	var ult *QuotaWindow
	for i := range s.Windows {
		if s.Windows[i].Ultimate {
			ult = &s.Windows[i]
			break
		}
	}
	if ult == nil || ult.ResetsAt.IsZero() || ult.Duration <= 0 {
		return time.Time{}, false
	}
	from := ult.ResetsAt.Add(-ult.Duration)
	if !from.Before(now) {
		return time.Time{}, false
	}
	return from, true
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

	// Volcengine signing keys for GetAFPUsage (the Agent Plan quota endpoint,
	// V4-signed, needs AK/SK not the Bearer chat key). Bound per virtual from
	// the credential pool's cred entry in buildOne. Empty = not configured.
	AccessKey string
	SecretKey string
	// VolcengineCredFile is the legacy <name>_apikey.json path, read for AK/SK
	// when AccessKey/SecretKey are empty (the single-account / pre-pool path).
	// The file read stays in the provider package (it's pure file I/O).
	VolcengineCredFile string

	// AqpBaseURL overrides the aqp (compass) backend base URL for the
	// monthly_usage quota fetch (AqpProvider.Quota/Usage). Empty -> AqpBase
	// (production). Tests point it at an httptest mock. The aqp account store
	// path is OAuthAuthFile (read via LoadAqpAccount).
	AqpBaseURL string

	// Models is the config model-id list, used by the usage display's fallback
	// (listConfigModels) when a provider can't fetch a structured quota
	// (e.g. volcengine without AK/SK).
	Models []string

	// Callbacks: main wires volcengine's V4-signed FetchModels here. The aqp
	// monthly_usage fetch is provider-owned (AqpProvider.fetchMonthlyUsage reads
	// the SSO-cookie store + POSTs directly, like the other providers). The
	// interactive login flow is NOT a callback - cmdLogin calls the run* flows
	// directly. Logout is provider-owned (file removal) since Phase 5.
	FetchModelsFn func(context.Context) ([]string, error) // for FetchModels (volcengine: V4-signed OpenAPI)

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

// IsRegistered reports whether providerID has a registered implementation.
// Callers offering a choice of built-in providers (e.g. `config init`'s guided
// setup) use this to skip template entries with no real backend.
func IsRegistered(providerID string) bool {
	_, ok := registry[providerID]
	return ok
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
