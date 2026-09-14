// Package web owns the embedded admin UI, HTTP routing, JSON presentation,
// login-session transport, and Web-scoped background tasks.
//
// Application state and mutations stay behind consumer-owned ReadAPI and
// CommandAPI ports so this package never imports the composition root.
package appapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"model-proxy/internal/adjudicate"
	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/requestlog"
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
	Name       string `json:"name"`
	ProviderID string `json:"provider_id"`
	Billing    string `json:"billing"`
	// UsageEndpoint reports whether the provider configures a usage_url — the
	// quota tracker polls pay-as-you-go providers only when one exists
	// (deepseek's /user/balance), so the UI's per-account "Refresh usage"
	// button keys off this, not billing alone.
	UsageEndpoint bool      `json:"usage_endpoint"`
	Accounts      []Account `json:"accounts"`
}

// TokenUsage is one flattened provider/model usage counter. Total is the
// display total across all four token buckets (input + output + cache_creation
// + cache_read): the buckets are anthropic-normalized (input excludes cached
// tokens), so the total is their plain sum.
type TokenUsage struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Total         uint64 `json:"total"`
	Requests      uint64 `json:"requests"`
}

// AgentUsage is the agent-dimension counterpart of TokenUsage: one agent's
// cumulative usage, served alongside TokenUsage in GET /api/tokens (same
// in-memory since-daemon-start window, reset together by
// POST /api/tokens/reset). Range-windowed agent queries stay on
// GET /api/agents (persisted agent_buckets). The top-level fields are the
// per-agent totals across every (provider, model) the agent touched; Models
// carries the per-(provider, model) breakdown. Total has the same
// four-bucket-sum definition as TokenUsage.Total, at both levels.
type AgentUsage struct {
	Agent         string            `json:"agent"`
	Requests      uint64            `json:"requests"`
	Input         uint64            `json:"input"`
	Output        uint64            `json:"output"`
	CacheCreation uint64            `json:"cache_creation"`
	CacheRead     uint64            `json:"cache_read"`
	Total         uint64            `json:"total"`
	Models        []AgentModelUsage `json:"models"`
}

// AgentModelUsage is one (provider, model) row of an agent's breakdown.
type AgentModelUsage struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Requests      uint64 `json:"requests"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Total         uint64 `json:"total"`
}

// Pin is the public projection of one manual route pin.
type Pin struct {
	Route     string
	Provider  string
	ExpiresAt time.Time
}

// PricingSnapshot contains detached pricing inputs used for analytics
// presentation. Catalog is immutable after publication. Aliases maps
// pricing.AliasKey(provider, upstreamModel) → exposed alias name, so a
// metered upstream name with no catalog entry (kimi-code's "k3") falls back
// to its exposed name's price ("kimi-k3").
type PricingSnapshot struct {
	Catalog   *pricing.Catalog
	Overrides map[string]pricing.Override
	Aliases   map[string]string
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
	Settings       ConfigSettings                 `json:"settings"`
}

// ConfigSettings is the structured projection of the scalar config blocks the
// Config tab edits through forms instead of the Raw YAML editor. Values are the
// RAW file values (empty/zero = key absent, so the form shows the code default
// as a placeholder); the two exceptions are log_level/log_file, which the loader
// fills with its effective defaults. Optional ints are pointers so "unset" is
// distinguishable from an explicit 0 (quality weights: 0 disables the signal).
// Everything not represented here stays YAML-only.
type ConfigSettings struct {
	LogLevel   string           `json:"log_level"`
	LogFile    string           `json:"log_file"`
	Scheduling ConfigScheduling `json:"scheduling"`
	RequestLog ConfigRequestLog `json:"request_log"`
	Stats      ConfigStats      `json:"stats"`
	Cache      ConfigCache      `json:"cache"`
	Guard      ConfigGuard      `json:"guard"`
}

// ConfigGuard mirrors guard.* for the Config tab's Guard form: the scalar
// switches plus the user-declared rule lists the Guard rules editor edits
// (extra_patterns / extra_paths). adjudicate stays YAML-only — it is an
// explicit opt-in exception (decision 36), not a form toggle.
type ConfigGuard struct {
	Secrets       string               `json:"secrets"`
	Paths         string               `json:"paths"`
	KnownSecrets  bool                 `json:"known_secrets"`
	Decode        bool                 `json:"decode"`
	Audit         bool                 `json:"audit"`
	SessionScan   bool                 `json:"session_scan"`
	AuditPath     string               `json:"audit_path"`
	ExtraPatterns []ConfigGuardPattern `json:"extra_patterns,omitempty"`
	ExtraPaths    []string             `json:"extra_paths,omitempty"`
}

// ConfigGuardPattern is one user-declared secret pattern (gitleaks
// extend-style): Regex plus an optional Literal pre-filter that must be a
// guaranteed substring of every regex match.
type ConfigGuardPattern struct {
	Name    string `json:"name"`
	Regex   string `json:"regex"`
	Literal string `json:"literal,omitempty"`
}

// ConfigScheduling mirrors scheduling.* — every field defaults in code, so an
// empty string / nil pointer means "use the default".
type ConfigScheduling struct {
	CircuitThreshold   *int   `json:"circuit_threshold"`
	CircuitCooldown    string `json:"circuit_cooldown"`
	RateLimitBackoff   string `json:"rate_limit_backoff"`
	QuotaCooldown      string `json:"quota_cooldown"`
	ModelLockout       string `json:"model_lockout"`
	RetryWait          string `json:"retry_wait"`
	UpstreamTimeout    string `json:"upstream_timeout"`
	StickyDwell        string `json:"sticky_dwell"`
	QuotaPollInterval  string `json:"quota_poll_interval"`
	QuotaSwitchMargin  *int   `json:"quota_switch_margin"`
	QualityErrorWeight *int   `json:"quality_error_weight"`
	QualityTTFTWeight  *int   `json:"quality_ttft_weight"`
}

// ConfigRequestLog mirrors request_log.* (whole block is restart-only).
type ConfigRequestLog struct {
	Enabled      bool   `json:"enabled"`
	Dir          string `json:"dir"`
	MaxFileSize  int64  `json:"max_file_size"`
	MaxBodyBytes int    `json:"max_body_bytes"`
	Retention    string `json:"retention"`
}

// ConfigStats mirrors stats.* (db_path/retention are restart-only).
type ConfigStats struct {
	DBPath    string `json:"db_path"`
	Retention string `json:"retention"`
}

// ConfigCache mirrors cache.* (hot-reloadable).
type ConfigCache struct {
	Enabled      bool   `json:"enabled"`
	TTL          string `json:"ttl"`
	MaxEntries   int    `json:"max_entries"`
	MaxBodyBytes int    `json:"max_body_bytes"`
}

// ModelProtocols is one model's three-protocol probe verdict matrix, with each
// leg as the stable verdict string ("yes"/"no"/"unknown").
type ModelProtocols struct {
	Chat      string `json:"chat"`
	Anthropic string `json:"anthropic"`
	Responses string `json:"responses"`
}

// ProviderModelCaps is one provider's probed model-capability projection for
// GET /api/models. Fingerprint invalidates the whole entry on protocol-relevant
// config change; ProbedAt is the provider's latest probe/correction time.
type ProviderModelCaps struct {
	Fingerprint string                    `json:"fingerprint"`
	ProbedAt    time.Time                 `json:"probed_at"`
	Models      map[string]ModelProtocols `json:"models"`
}

// ModelsDocument is the transport projection for GET /api/models: the startup
// protocol probe's verdicts per provider and model. Providers with no probe
// data are omitted; an empty store projects `{"providers":{}}`.
type ModelsDocument struct {
	Providers map[string]ProviderModelCaps `json:"providers"`
}

// ModelsRefreshDrop is one model dropped by the models-refresh endpoint probe,
// with the per-leg reason summary (same rendering as the CLI's drop summary).
type ModelsRefreshDrop struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// ModelsRefreshResult is the outcome of POST /api/models/refresh (the daemon
// twin of `model-proxy models refresh <provider>`): the validated model list,
// the config diff it produced, and a warning when the list was written
// unvalidated (fetch/probe infra down or an all-failed probe).
type ModelsRefreshResult struct {
	Provider      string              `json:"provider"`
	Kept          []string            `json:"kept"`
	Added         []string            `json:"added"`
	Removed       []string            `json:"removed"`
	PolicyDropped []string            `json:"policy_dropped"`
	ProbeDropped  []ModelsRefreshDrop `json:"probe_dropped"`
	Warning       string              `json:"warning,omitempty"`
	ConfigUpdated bool                `json:"config_updated"`
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

// AnalyticsQuery describes one calendar aggregation read. By selects the
// grouping dimension: ""/"model" reads minute_buckets grouped by
// (provider, model); "agent" reads agent_buckets grouped by
// (agent, provider, model) — Agent narrows that view to one agent.
type AnalyticsQuery struct {
	From        int64
	To          int64
	Provider    string
	Model       string
	Agent       string
	Granularity string
	By          string
}

// SecurityQuery is the normalized audit-log query passed through the read
// port. From/To are unix milliseconds, inclusive; zero means unbounded.
type SecurityQuery struct {
	Kind      string
	From, To  int64
	Limit     int
	RequestID string
}

// SecurityRecord is the JSON-safe projection of one security audit record
// (served from the queryable SQLite store; the ignored-tier low verdicts
// never enter the store). Names carries pattern/path-category names only —
// matched content never enters this DTO (the seclog red line applies to the
// projection too); Reason/Evidence/Model are the scrubbed LLM verdict
// attribution.
type SecurityRecord struct {
	Ts        int64    `json:"ts"`
	Kind      string   `json:"kind"`
	RequestID string   `json:"request_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Protocol  string   `json:"protocol,omitempty"`
	Exposed   string   `json:"exposed,omitempty"`
	Names     []string `json:"names,omitempty"`
	Action    string   `json:"action,omitempty"`
	// Verdict is non-empty on records from (or fail-opened out of) the AI
	// second-opinion channel: high | medium | error | skipped. Empty for
	// classic immediate records, including exact-match interceptions.
	Verdict  string `json:"verdict,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Model    string `json:"model,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// SecurityResult is one audit-log query outcome: Enabled reports whether the
// security audit log is persisted (guard.audit on and its directory present),
// Records holds matches newest first, and Skipped counts unreadable lines.
type SecurityResult struct {
	Enabled bool             `json:"enabled"`
	Records []SecurityRecord `json:"records"`
	Skipped int              `json:"skipped"`
	// Counts is the SERVER-side verdict aggregation over the same window as
	// Records (kind/from/to; the KPI tiles read this instead of counting the
	// client-merged feed — that counted the in-memory ring and drifted on
	// restart). Low comes from the adjudication service's cumulative counter
	// (lows never enter the queryable store by design).
	Counts *SecurityVerdictCounts `json:"counts,omitempty"`
}

// SecurityVerdictCounts is the verdict digest of one audit window.
type SecurityVerdictCounts struct {
	High    int64 `json:"high"`
	Medium  int64 `json:"medium"`
	Error   int64 `json:"error"`
	Skipped int64 `json:"skipped"`
	Low     int64 `json:"low"`
}

// Security-explain statuses (SecurityExplainResult.Status).
const (
	SecurityExplainOK                 = "ok"
	SecurityExplainNoRequestLog       = "no_request_log"
	SecurityExplainNotFound           = "not_found"
	SecurityExplainRedacted           = "redacted"
	SecurityExplainCrossRequest       = "cross_request"
	SecurityExplainScannerUnavailable = "scanner_unavailable"
)

// SecurityMatch is one re-located occurrence of a recorded guard-hit name
// inside the persisted request body. The context window around the match is
// pre-split into Pre/Hit/Post (never byte offsets — the web UI slices JS
// UTF-16 strings); secret-kind hits and ANY other secret overlapping the
// window are masked (prefix + … + suffix), path hits are shown verbatim (a
// path is not a credential). Located is false when the name could not be
// re-located on the stored body (rule removed since, body stored
// post-redact, or a cross-request channel like known_secret_fragmented) —
// Regex/Source/Explanation still describe it.
type SecurityMatch struct {
	Name        string `json:"name"`
	Strength    string `json:"strength,omitempty"`
	Regex       string `json:"regex,omitempty"`
	Source      string `json:"source,omitempty"`
	Explanation string `json:"explanation,omitempty"`
	Pre         string `json:"pre,omitempty"`
	Hit         string `json:"hit,omitempty"`
	Post        string `json:"post,omitempty"`
	Located     bool   `json:"located"`
}

// SecurityExplainResult is the on-demand analysis of one audit record: the
// original request body is fetched from the request log and re-scanned with
// the current guard scanner. Nothing is persisted — the explain surface
// derives from data the admin can already read via /api/requests/<id>.
type SecurityExplainResult struct {
	Status    string          `json:"status"`
	RequestID string          `json:"request_id"`
	Kind      string          `json:"kind"`
	Matches   []SecurityMatch `json:"matches"`
	// Adjudications are the AI second-opinion verdicts recorded for THIS
	// request (audit-log verdict records, enriched from the live ring when
	// still resident): the LLM judgment rides the analyze view.
	Adjudications []SecurityExplainAdjudication `json:"adjudications,omitempty"`
}

// SecurityExplainAdjudication is one recorded LLM judgment attached to an
// explain result. Reason is the judgment logic and Evidence the factual basis
// the model cited (both scrubbed before storage).
type SecurityExplainAdjudication struct {
	Rule     string `json:"rule"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Model    string `json:"model,omitempty"`
	Ts       int64  `json:"ts,omitempty"`
	Cached   bool   `json:"cached,omitempty"`
}

// SecurityBlock is one persisted guard-adjudication session block: a high
// verdict under guard.adjudicate.block_session. It survives restarts and
// clears only through the explicit unblock surface (CLI / WebUI). Aliased to
// the adjudicate package's snapshot type (flat JSON via embedding) so the
// field set has one owner.
type SecurityBlock = adjudicate.BlockEntry

// SecurityAdjudicationStats is the LLM usage accounting of the adjudication
// channel: real model calls and their token totals. Cache hits cost nothing
// and are not counted.
type SecurityAdjudicationStats struct {
	Calls        int64 `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// LowVerdicts is the cumulative suppressed-low count (persisted in
	// guard_stats.json, covers cached echoes): rows stay ring-only.
	LowVerdicts int64 `json:"low_verdicts"`
}

// SecurityAdjudicationFeed is the payload of GET /api/security/adjudications:
// the recent-verdict ring plus the channel's LLM usage stats.
type SecurityAdjudicationFeed struct {
	Adjudications []SecurityAdjudication    `json:"adjudications"`
	Stats         SecurityAdjudicationStats `json:"stats"`
	// Enabled reports whether guard.adjudicate is currently on (the current
	// generation's switch) — the leaderboard's "enable to suppress noise"
	// hint keys off it, distinguishing off from merely quiet.
	Enabled bool `json:"enabled"`
}

// SecurityAdjudication is one recent AI second-opinion verdict from the
// bounded in-memory ring (newest first). Verdict is high | medium | low |
// error | skipped; Reason/Evidence are the scrubbed, length-capped model
// judgment logic and factual basis. Aliased to the adjudicate package's
// Result — same owner, same JSON shape.
type SecurityAdjudication = adjudicate.Result

// ErrGuardScannerUnavailable is returned through the admin LocateGuardHits
// port when the current generation has no guard scanner (guard disabled or
// its construction failed). SecurityExplain maps it to the
// scanner_unavailable status instead of a generic error.
var ErrGuardScannerUnavailable = errors.New("guard scanner unavailable in the current generation")

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

// RequestLogQueries is the read port over the request-log store consumed by
// the /api/requests and /api/sessions handlers. The production implementation
// is the tailing SQLite index (*requestlog.Indexer) with a directory-scan
// fallback inside admin; every method carries the exact semantics of the
// requestlog scan function of the same name (filter mapping, newest-first
// top-K, facets collected before the filter, detail including bodies).
type RequestLogQueries interface {
	SummariesWithFacets(requestlog.Filter) ([]requestlog.Summary, requestlog.Facets, error)
	Detail(requestID string) ([]requestlog.Record, error)
	SessionSummaries(scanLimit, limit int, costOf func(provider, model string, usage requestlog.Usage) float64) ([]requestlog.SessionSummary, error)
}

// ReadAPI is the complete read-only capability consumed by the Web transport.
type ReadAPI interface {
	Dashboard(time.Time) Dashboard
	LogFile() string
	RequestLogDirectory() string
	// RequestLogQueries returns the request-log query port; nil means the
	// request log is disabled and handlers answer their {enabled:false} shape.
	RequestLogQueries() RequestLogQueries
	Accounts() []ProviderAccounts
	// Tokens/Agents project the usage counters. from <= 0 && to <= 0 is the
	// all-time cumulative view (hot counters); any bound > 0 aggregates
	// persisted minute buckets with from <= minute <= to (the /api/tokens
	// time-range selector).
	Tokens(from, to int64) ([]TokenUsage, error)
	Agents(from, to int64) ([]AgentUsage, error)
	StatsSince() int64
	Stats(StatsQuery) ([]observestats.Bucket, error)
	AgentStats(AgentStatsQuery) ([]observestats.AgentBucket, error)
	Analytics(AnalyticsQuery) ([]observestats.AnalyticsBucket, error)
	// AnalyticsAgentNames lists the distinct agents with traffic in the
	// query's window (provider/model filtered; the agent filter itself is
	// ignored so the suggestion list keeps offering alternatives). Never nil.
	AnalyticsAgentNames(AnalyticsQuery) []string
	Pricing() PricingSnapshot
	Fusion(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run)
	Pins() []Pin
	Security(SecurityQuery) (SecurityResult, error)
	// SecurityExplain re-locates one audit record's hits inside the persisted
	// request body (on-demand, nothing persisted). kind must be secret or
	// path; names are the audit record's pattern/category names.
	SecurityExplain(requestID, kind string, names []string) (SecurityExplainResult, error)
	SecurityBlocks() []SecurityBlock
	SecurityAdjudications() SecurityAdjudicationFeed
	ConfigDocument() (ConfigDocument, error)
	// ModelsDocument projects the startup protocol probe's per-provider model
	// capability matrix (internal/runtime/wirecap ModelStore snapshot).
	ModelsDocument() ModelsDocument
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
	// FreezeHealth marks one provider as operator-frozen until ResetHealth;
	// unlike ResetHealth it always requires an explicit provider (no
	// freeze-all). Persist-then-return semantics mirror ResetHealth (the
	// error means the in-memory freeze is live but durable state is stale).
	FreezeHealth(provider string) ([]string, error)
	SetPin(route, provider string, ttl time.Duration) (Pin, bool)
	ClearPin(route string) bool
	// SecurityUnblock removes one persisted guard-adjudication session block.
	SecurityUnblock(sessionID string) error
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
	// RefreshModels is the daemon twin of `model-proxy models refresh
	// <provider>`: fetch the provider's live model list, probe every candidate
	// with the 3-protocol matrix, overwrite providers.<name>.models with the
	// callable subset, hot-reload, and replace the provider's cached verdicts
	// with the fresh matrix. Safety nets mirror the CLI — a fetch/probe
	// outage never wipes models:, the list is written unvalidated with a
	// warning instead.
	RefreshModels(ctx context.Context, provider string) (ModelsRefreshResult, error)
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
