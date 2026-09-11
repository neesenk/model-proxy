// Package admin owns the Web admin application service: it implements the
// consumer-owned appapi.ReadAPI / appapi.CommandAPI ports consumed by
// internal/web, projecting detached runtime snapshots into JSON-safe DTOs and
// executing all credential/config mutations. It never imports the composition
// root, the HTTP transport, or the CLI: every Proxy touchpoint reaches the
// package through the narrow copy-by-value Ports below, whose closures capture
// *Proxy and own lock discipline (p.mu / SnapshotRuntime) in internal/app.
package admin

import (
	"net/http"
	"time"

	"model-proxy/internal/appapi"
	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	"model-proxy/internal/login"
	obscounters "model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// Ports are the narrow capabilities the admin service needs from the
// composition root. Closures capture *Proxy in internal/app and take
// p.mu / SnapshotRuntime internally, so this package never touches locks or
// reload-owned state directly. Functions returning maps/slices derived from
// reload-owned state must return copies — never live map references (the same
// copy-by-value contract as internal/observe/budget).
type Ports struct {
	// ConfigFile returns the daemon config path (flag-owned, not
	// reload-owned). A nil func reports the empty path.
	ConfigFile func() string

	// Config returns a shallow copy of the current generation's config; the
	// Providers/Routes maps are generation-immutable and read-only to callers.
	Config func() *configdomain.Config
	// ProviderConfig resolves one provider entry from the current generation.
	ProviderConfig func(name string) (configdomain.Provider, bool)
	// ProviderConfigs returns a fresh map copy of the current generation's
	// provider entries.
	ProviderConfigs func() map[string]configdomain.Provider
	// LogFile returns the current generation's configured log path.
	LogFile func() string

	// DashboardState captures one generation-consistent dashboard snapshot:
	// exactly one runtime Manager dashboard read under one root read lock,
	// with the schedule preview derived from that same capture (the schedule
	// projection is shared with the proxy's /debug/schedule surface and stays
	// composition-root owned).
	DashboardState func(now time.Time) DashboardState

	// RequestLogDirectory reports the request-log directory ("" when the
	// request log is disabled).
	RequestLogDirectory func() string
	// RequestLogIndex returns the process-lifetime tailing SQLite index over
	// the request-log directory (nil when the request log is disabled or the
	// index failed to open). The index is set once at startup and never
	// swapped, so the closure needs no lock. Read paths fall back to directory
	// scans when it is nil.
	RequestLogIndex func() *requestlog.Indexer
	// TokenUsage returns the token counter snapshot (nil when disabled).
	TokenUsage func() map[obscounters.TokenKey]obscounters.TokenUsage
	// AgentUsage returns the agent counter snapshot (nil when disabled) —
	// the cumulative agent-dimension counterpart of TokenUsage.
	AgentUsage func() map[obscounters.AgentKey]obscounters.AgentCount
	// TokenUsageRange/AgentUsageRange aggregate persisted minute buckets with
	// from <= minute <= to (either bound <= 0 unbounded) — the range
	// counterparts backing the /api/tokens time selector (empty maps, nil
	// error, when the store is disabled).
	TokenUsageRange func(from, to int64) (map[observestats.Key]observestats.Counters, error)
	AgentUsageRange func(from, to int64) (map[observestats.AgentKey]observestats.AgentCounters, error)
	// StatsSince returns the oldest persisted bucket minute across both stats
	// tables (unix seconds, 0 when empty) — the anchor for the cumulative
	// usage "Since" label.
	StatsSince func() int64
	// StatsRange/AgentStats/Analytics query the stats store; the closures
	// return empty (non-nil) slices when the store is disabled. Analytics
	// reads the (provider, model) calendar buckets; AnalyticsAgents is the
	// agent-dimension form over agent_buckets.
	StatsRange      func(from, to int64, provider, model string, bucketSecs int64) ([]observestats.Bucket, error)
	AgentStats      func(from, to int64, agent, provider, model string, bucketSecs int64) ([]observestats.AgentBucket, error)
	Analytics       func(from, to int64, provider, model, granularity string) ([]observestats.AnalyticsBucket, error)
	AnalyticsAgents func(from, to int64, agent, provider, model, granularity string) ([]observestats.AnalyticsBucket, error)
	// FusionSnapshot returns the fusion registry projection for one workflow.
	FusionSnapshot func(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run)
	// Pins returns the active pins (expired ones already dropped).
	Pins func() map[string]PinState
	// ModelCapsSnapshot returns the startup protocol probe's detached
	// per-provider model capability matrix (empty, non-nil map when nothing
	// was probed yet). The ModelStore owns its own leaf lock, so the closure
	// does not take p.mu.
	ModelCapsSnapshot func() map[string]runtimewire.ProviderModelCaps
	// Pricing returns the pricing catalog (immutable after publication) plus
	// a detached copy of the configured overrides.
	Pricing func() (catalog *pricing.Catalog, overrides map[string]pricing.Override)

	// ResetStats clears request counters and persisted stats.
	ResetStats func() error
	// ResetHealth clears circuit/model-lock health for one provider (or all
	// when name is empty) and reports the cleared entries.
	ResetHealth func(name string) (cleared []string, locks int)
	// FreezeHealth marks one provider as operator-frozen — excluded from
	// scheduling until ResetHealth — and reports the matched entries. Unlike
	// ResetHealth, freeze always requires an explicit name (pooled parent =
	// all its virtual accounts); an empty name matches nothing. known is the
	// universe of runtime provider keys; a nil known lets the composition root
	// derive it from the current generation (config names + pooled virtual
	// account keys) under the same lock as the parentOf read.
	FreezeHealth func(name string, known []string) (frozen []string)
	// Quota* drive the background quota tracker; QuotaEnabled reports whether
	// the tracker exists at all (degenerate configs run without one).
	QuotaEnabled func() bool
	QuotaPollOne func(name string) bool
	QuotaPollAll func(now time.Time)
	QuotaPersist func() error
	// SetPin pins a route to one provider; ok is false when the route is
	// unknown or the provider is not one of its targets.
	SetPin func(route, provider string, ttl time.Duration) (expiresAt time.Time, ok bool)
	// ClearPin removes a route pin.
	ClearPin func(route string) bool
	// Reload hot-reloads the config file. A *ReloadAppliedWarning error means
	// the new generation is live but runtime-state durability is degraded;
	// any other error means the runtime kept the old generation.
	Reload func(configFile string) error
	// ProbeRuntime captures config and provider implementations from exactly
	// one runtime snapshot, so an account probe can never pair one config
	// generation with another generation's impl.
	ProbeRuntime func() (cfg *configdomain.Config, providers map[string]provider.Provider)
	// LocateGuardHits re-runs guard detection over one persisted request body
	// and returns located, display-ready matches (masked snippets, never raw
	// secret bytes) for the security-explain surface. Implemented by the
	// composition root, which owns the current-generation guard scanner;
	// internal/admin must not import internal/guard (archtest DAG).
	LocateGuardHits func(body []byte, kind string, names []string) ([]appapi.SecurityMatch, error)
	// ModelRefreshRuntime captures config, pooled implementation and probe policy
	// together. ModelCapsReplace only accepts the captured endpoint fingerprint.
	ModelRefreshRuntime func(name string) ModelRefreshRuntime
	ModelCapsReplace    func(name, fingerprint string, models map[string]runtimewire.ModelProtocols) bool

	// Login constructor seams. Production wires the login package defaults;
	// tests point them at stub endpoints after construction.
	NewAqpClient    func(storePath string) *login.AqpClient
	NewCodexOptions func() *login.CodexLoginServerOptions
}

// ModelRefreshRuntime is an immutable, single-generation model refresh input.
type ModelRefreshRuntime struct {
	Config      *configdomain.Config
	Provider    provider.Provider
	Fingerprint string
	Client      *http.Client
}

// DashboardState is one generation-consistent capture behind Dashboard. Every
// field derives from a single root read lock plus the process-lifetime
// counter/cache leaves, so config and runtime state cannot cross generations.
type DashboardState struct {
	Listen        string
	RouteWarnings []string
	// Cache is the current generation's cache store (nil = disabled); the
	// store is concurrency-safe and survives its generation, so reading its
	// stats after capture cannot mix state.
	Cache        *responsecache.Store
	Runtime      runtimestate.DashboardSnapshot
	QuotaEnabled bool
	StartedAt    time.Time
	Counters     map[string]obscounters.ProviderMetricsSnapshot
	// Schedule is the schedule preview JSON derived from Runtime by the
	// composition root (shared with /debug/schedule).
	Schedule []byte
}

// PinState is the detached projection of one active route pin.
type PinState struct {
	Provider  string
	ExpiresAt time.Time
}

// ReloadAppliedWarning marks a reload that applied the new generation but
// degraded runtime-state durability. The composition root converts its own
// applied-warning error into this type at the port boundary; the message
// format is part of the HTTP surface and must stay stable.
type ReloadAppliedWarning struct{ Err error }

func (e *ReloadAppliedWarning) Error() string { return "reload applied with warning: " + e.Err.Error() }
func (e *ReloadAppliedWarning) Unwrap() error { return e.Err }

// Service is the Web admin application service consumed by internal/web
// through appapi.ReadAPI / appapi.CommandAPI. It owns projection from
// root-private runtime values to JSON-safe DTOs and all credential/config
// mutations; it does not own HTTP routing, sessions, or locks.
type Service struct {
	ports Ports
}

// New builds the admin service around the given composition-root ports.
func New(ports Ports) *Service {
	return &Service{ports: ports}
}

func (s *Service) currentConfigFile() string {
	if s == nil || s.ports.ConfigFile == nil {
		return ""
	}
	return s.ports.ConfigFile()
}
