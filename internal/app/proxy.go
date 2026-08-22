package app

import (
	obscounters "model-proxy/internal/observe/counters"
	"net/http"
	"sync"
	"sync/atomic"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	"model-proxy/internal/fusion"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/shadow"
)

// Proxy holds the compiled provider instances + the config.
type Proxy struct {
	lifecycle        *runtimestate.Lifecycle
	mu               sync.RWMutex  // guards cfg/providers across reload (held by handler for the request)
	configGeneration atomic.Uint64 // incremented on every successful reload
	runtimeState     runtimestate.Manager
	cfg              *Config
	providers        map[string]provider.Provider // provider name → Provider (shared)
	client           *http.Client
	quota            *runtimestate.QuotaTracker     // background quota poller; nil only in degenerate tests
	metrics          *obscounters.MetricsStore      // request counters (atomic); nil only in degenerate tests
	tokens           *obscounters.TokenCounter      // SSE-scanned token usage; nil only in degenerate tests
	agents           *obscounters.AgentCounter      // per-agent (UA) request/token counters; nil only in degenerate tests
	stats            *observestats.Store            // SQLite persistence for per-minute buckets; nil in tests (runtime services open it)
	flusher          *observestats.Flusher          // per-minute diff loop; nil in tests (runProxy starts it)
	reqLog           *requestlog.Logger             // per-request access log (full bodies); nil = disabled (default) or init failure
	reqLogStarted    bool                           // lifecycle owns loop/shutdown only when started by startRuntimeServices
	cache            *responsecache.Store           // exact-match response cache (prompt-hash + TTL); nil = disabled
	responsesState   *protocol.ResponsesStateStore  // previous_response_id replay for Responses clients bridged to stateless backends
	events           *observeevents.Hub             // live request monitor fan-out hub (SSE /api/events); always non-nil
	fusionReg        *fusion.Registry               // fusion orchestration observability (recent runs + per-workflow aggregates + daily budget); survives reload like events
	catalog          *catalog.Catalog               // models.dev metadata (context window + modalities) for request-aware routing; nil = unavailable, degrade gracefully
	shadow           atomic.Pointer[shadow.Runtime] // reload-swappable detached Shadow runtime; captured with each request generation
	pricingMu        sync.Mutex                     // guards pricing during refresh (thundering-herd guard on pricing.EnsureFresh)
	budget           *budgetWatcher                 // monthly cost alert loop; nil unless budgets: configures a threshold
	closeOnce        sync.Once

	// Credential-pool unrolling (buildProviders). For a multi-account parent,
	// poolIndex[parent] = its sorted virtual ids ("name#<id>") and parentOf is
	// the inverse. Single-account / not-logged-in providers appear in neither
	// map (their id == the plain name). Guards: same as the struct — poolIndex
	// and parentOf are rebuilt on reload under p.mu; pool spread is owned by
	// runtimeState.
	poolIndex      map[string][]string      // parent name → sorted virtual ids (only multi-account parents)
	parentOf       map[string]string        // virtual id → parent name
	expandedRoutes map[string][]RouteTarget // exposed model → expanded targets (explicit + implicit)
	routeKeys      map[string]bool          // key set of expandedRoutes; generation-owned, shared by the scheduling hot path
	implicitRoutes map[string]RouteTarget   // exposed model → single target auto-derived from logged-in providers' model lists (for models not in cfg.Routes)
	routeWarnings  []string                 // ambiguity warnings for implicit routes (multi-provider); surfaced in `models` CLI + /api/status

	// Runtime wire capabilities have their own leaf Store. The Store never
	// calls back into Proxy while locked and survives reload generations.
	wireCaps  runtimewire.Store
	wireProbe bool

	// pprofEnabled (MP_PPROF=1 at construction) exposes /debug/pprof/ on the
	// proxy handler for live profiling. Opt-in: profiles can carry request
	// data in heap samples, so the endpoint is off by default.
	pprofEnabled bool
}
