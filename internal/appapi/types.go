// Package web owns the embedded admin UI, HTTP routing, JSON presentation,
// login-session transport, and Web-scoped background tasks.
//
// Application state and mutations stay behind consumer-owned ReadAPI and
// CommandAPI ports so this package never imports the composition root.
package appapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"model-proxy/internal/fusion"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/presets"
	"model-proxy/internal/pricing"
)

// Metrics is the JSON-safe projection of one provider's process counters.
type Metrics struct {
	Requests       uint64 `json:"requests"`
	Failovers      uint64 `json:"failovers"`
	RateLimited429 uint64 `json:"rate_limited_429"`
	Failures       uint64 `json:"failures"`
	LastRequestAt  int64  `json:"last_request_at"`
	LatencySum     uint64 `json:"latency_ms_sum"`
	TTFTSum        uint64 `json:"ttft_ms_sum"`
}

// Dashboard is one detached, generation-consistent status snapshot.
type Dashboard struct {
	Uptime     string
	Listen     string
	Health     map[string]any
	ModelLocks map[string][]map[string]any
	Quota      map[string]any
	Schedule   json.RawMessage
	Counters   map[string]Metrics
	Cache      map[string]any
	Warnings   []string
	// CredentialStore names the resolved credstore backend ("keychain" or
	// "file") so the Status surface shows where credentials live at rest.
	CredentialStore string
}

// Account is deliberately incapable of carrying a credential.
type Account struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	AddedAt string `json:"added_at"`
	Email   string `json:"email,omitempty"`
}

// ProviderAccounts is one configured provider and its public account metadata.
type ProviderAccounts struct {
	Name       string    `json:"name"`
	ProviderID string    `json:"provider_id"`
	Billing    string    `json:"billing"`
	Accounts   []Account `json:"accounts"`
}

// TokenUsage is one flattened provider/model usage counter.
type TokenUsage struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Requests      uint64 `json:"requests"`
}

// Pin is the public projection of one manual route pin.
type Pin struct {
	Route     string
	Provider  string
	ExpiresAt time.Time
}

// PricingSnapshot contains detached pricing inputs used for analytics
// presentation. Catalog is immutable after publication.
type PricingSnapshot struct {
	Catalog   *pricing.Catalog
	Overrides map[string]pricing.Override
}

// ConfigRouteTarget is the JSON shape returned by GET /api/config.
type ConfigRouteTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Priority int    `json:"priority"`
}

// ConfigSummary is the small structured summary paired with raw YAML.
type ConfigSummary struct {
	Listen        string `json:"listen"`
	ProviderCount int    `json:"provider_count"`
	RouteCount    int    `json:"route_count"`
}

// ConfigProviderMeta is the per-provider routing metadata paired with the
// model lists: the provider-level priority every derived target inherits and
// the model → exposed-name alias map.
type ConfigProviderMeta struct {
	Priority int               `json:"priority"`
	Alias    map[string]string `json:"alias"`
}

// ConfigDocument is the complete transport projection for GET /api/config.
// Routes is the EFFECTIVE table (derived from provider model lists, explicit
// routes: entries overriding per name), so every callable model appears.
type ConfigDocument struct {
	YAML           string                         `json:"yaml"`
	Summary        ConfigSummary                  `json:"summary"`
	ProviderModels map[string][]string            `json:"provider_models"`
	ProviderMeta   map[string]ConfigProviderMeta  `json:"provider_meta"`
	Routes         map[string][]ConfigRouteTarget `json:"routes"`
}

// StatsQuery is the normalized query passed through the read port.
type StatsQuery struct {
	From       int64
	To         int64
	Provider   string
	Model      string
	BucketSecs int64
}

// AgentStatsQuery is the agent-dimension form of StatsQuery.
type AgentStatsQuery struct {
	From       int64
	To         int64
	Agent      string
	Provider   string
	Model      string
	BucketSecs int64
}

// AnalyticsQuery describes one calendar aggregation read.
type AnalyticsQuery struct {
	From        int64
	To          int64
	Provider    string
	Model       string
	Granularity string
}

// SecurityQuery is the normalized audit-log query passed through the read
// port. From/To are unix milliseconds, inclusive; zero means unbounded.
type SecurityQuery struct {
	Kind  string
	From  int64
	To    int64
	Limit int
}

// SecurityRecord is the JSON-safe projection of one security audit record.
// Names carries pattern/path-category names only — matched content never
// enters this DTO (the seclog red line applies to the projection too).
type SecurityRecord struct {
	Ts        int64    `json:"ts"`
	Kind      string   `json:"kind"`
	RequestID string   `json:"request_id,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Protocol  string   `json:"protocol,omitempty"`
	Exposed   string   `json:"exposed,omitempty"`
	Names     []string `json:"names,omitempty"`
	Action    string   `json:"action,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

// SecurityResult is one audit-log query outcome: Enabled reports whether the
// security audit log is persisted (guard.audit on and its directory present),
// Records holds matches newest first, and Skipped counts unreadable lines.
type SecurityResult struct {
	Enabled bool             `json:"enabled"`
	Records []SecurityRecord `json:"records"`
	Skipped int              `json:"skipped"`
}

// ValidationIssue is one config lint finding returned by POST
// /api/config/validate. Line is 1-based; 0 means the problem cannot be pinned
// to a source line (whole-document errors such as "no providers configured").
type ValidationIssue struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// EditRequest is a structured config mutation.
type EditRequest struct {
	Kind string         `json:"kind"`
	Name string         `json:"name"`
	Data map[string]any `json:"data"`
}

// AccountInput is the credential input accepted by the account-add endpoint.
// It must never be logged or returned from this package.
type AccountInput struct {
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Label     string `json:"label"`
	Replace   bool   `json:"replace"`
}

// MutationResult reports a durable account mutation plus any reload warning.
type MutationResult struct {
	ID      string
	Warning string
}

// ProbeResult is one active account probe projection.
type ProbeResult struct {
	OK         bool
	HTTPStatus int
	Reason     string
	Provider   string
	AccountID  string
	Model      string
	Latency    time.Duration
}

// LoginUpdate is a detached async-login session state.
type LoginUpdate struct {
	State   string `json:"state"`
	Detail  string `json:"detail"`
	Result  string `json:"result"`
	Warning string `json:"warning"`
}

// LoginJob completes provider-specific polling and credential persistence.
// Cancellation is honored until the application-defined credential commit.
type LoginJob interface {
	Run(context.Context) LoginUpdate
}

// LoginStart carries the transport-visible bootstrap values and its owned job.
type LoginStart struct {
	Provider  string
	LoginURL  string
	VerifyURL string
	UserCode  string
	Job       LoginJob
}

// HTTPError lets the application port select a stable HTTP status without
// exposing root-private error types to the transport.
type HTTPError struct {
	Status  int
	Message string
}

func (err *HTTPError) Error() string {
	if err == nil {
		return ""
	}
	return err.Message
}

// NewHTTPError constructs a transport-classified application error.
func NewHTTPError(status int, message string) error {
	return &HTTPError{Status: status, Message: message}
}

// ReadAPI is the complete read-only capability consumed by the Web transport.
type ReadAPI interface {
	Dashboard(time.Time) Dashboard
	LogFile() string
	RequestLogDirectory() string
	Accounts() []ProviderAccounts
	Tokens() []TokenUsage
	Stats(StatsQuery) ([]observestats.Bucket, error)
	AgentStats(AgentStatsQuery) ([]observestats.AgentBucket, error)
	Analytics(AnalyticsQuery) ([]observestats.AnalyticsBucket, error)
	Pricing() PricingSnapshot
	Fusion(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run)
	Pins() []Pin
	Security(SecurityQuery) (SecurityResult, error)
	ConfigDocument() (ConfigDocument, error)
	// Presets lists the provider preset catalog (internal/presets) for the
	// web Add-Provider wizard.
	Presets() []presets.Preset
}

// CommandAPI is the complete mutation/active-probe capability consumed by the
// Web transport.
type CommandAPI interface {
	ResetStats() error
	RefreshQuota(provider string) bool
	ResetHealth(provider string) ([]string, int, error)
	SetPin(route, provider string, ttl time.Duration) (Pin, bool)
	ClearPin(route string) bool
	SaveConfig([]byte) error
	// ValidateConfig lints candidate config bytes without persisting anything;
	// an empty result means valid.
	ValidateConfig([]byte) []ValidationIssue
	EditConfig(EditRequest) error
	AddAccount(context.Context, string, AccountInput) (MutationResult, error)
	ProbeAccount(context.Context, string, string) (ProbeResult, error)
	RemoveAccount(string, string) (MutationResult, error)
	BeginLogin(context.Context, string) (LoginStart, error)
	// AddPreset merges a preset's template provider block into the live
	// config (fail-closed on validation) and hot-reloads; warnings are the
	// implicit-routing ambiguity model names for the UI to surface, and a
	// failed reload is reported separately as reloadWarning (the merged
	// block is already persisted). Credentials are added afterwards through
	// AddAccount/BeginLogin as usual.
	AddPreset(name string) (warnings []string, reloadWarning string, err error)
}

// RequirePorts validates that both application ports are present. It is
// exported so the transport package can fail fast during construction.
func RequirePorts(reads ReadAPI, commands CommandAPI) error {
	if reads == nil {
		return fmt.Errorf("appapi ReadAPI is nil")
	}
	if commands == nil {
		return fmt.Errorf("appapi CommandAPI is nil")
	}
	return nil
}
