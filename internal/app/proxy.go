// proxy.go — Proxy state (generationState/processServices) and its construction, close, and process-lifetime component wiring.
package app

import (
	"fmt"
	"model-proxy/internal/accounts"
	"model-proxy/internal/adjudicate"
	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	"model-proxy/internal/guard"
	guardsession "model-proxy/internal/guard/session"
	"model-proxy/internal/observe/budget"
	"model-proxy/internal/observe/counters"
	obscounters "model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/observe/seclog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/routing"
	runtimestate "model-proxy/internal/runtime"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/shadow"
	"model-proxy/internal/upstreamproxy"
	"model-proxy/internal/webauth"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// generationState holds everything swapped as one reload generation under p.mu
// (proxy_reload.go's locked section is the swap definition). Embedded in Proxy
// so promoted selectors (p.cfg, p.providers, ...) stay unchanged.
type generationState struct {
	cfg       *configdomain.Config
	providers map[string]provider.Provider // provider name → Provider (shared)

	cache             *responsecache.Store   // exact-match response cache (prompt-hash + TTL); nil = disabled
	guardScanner      *guard.Scanner         // reload-owned immutable outbound secret/path scanner; never serialized or logged
	guardPoolSecrets  []providerbuild.Secret // reload-owned: current generation's Build.PoolSecrets (base for OAuth re-syncs)
	guardOAuthSecrets []providerbuild.Secret // reload-owned: OAuth + provider memory-reported known-secret values in guardScanner; refreshed in place on the poll beat
	secLog            *seclog.Logger         // security audit log (guard hit records); reload-owned, swapped by reconcileSecLog; nil = audit off for this generation
	secLogRunning     bool                   // current generation's secLog Run goroutine is live (lifecycle-admitted); guards Close's drain

	// Credential-pool unrolling (buildProviders). For a multi-account parent,
	// poolIndex[parent] = its sorted virtual ids ("name#<id>") and parentOf is
	// the inverse. Single-account / not-logged-in providers appear in neither
	// map (their id == the plain name). Guards: same as generationState —
	// poolIndex and parentOf are rebuilt on reload under p.mu; pool spread is
	// owned by runtimeState.
	poolIndex      map[string][]string        // parent name → sorted virtual ids (only multi-account parents)
	parentOf       map[string]string          // virtual id → parent name
	expandedRoutes map[string][]configdomain. // exposed model → expanded targets (explicit + derived)
			RouteTarget
	routeKeys     map[string]bool            // key set of expandedRoutes; generation-owned, shared by the scheduling hot path
	derivedRoutes map[string][]configdomain. // exposed model → targets auto-aggregated from provider model lists (for names not in cfg.Routes)
			RouteTarget
	routeWarnings []string // routing hazard warnings; surfaced in `models` CLI + /api/status

	shadow atomic.Pointer[shadow.Runtime] // reload-swappable detached Shadow runtime; captured with each request generation

	// adminAuth/apiKeys are the S2 optional-auth sources (webauth), swapped on
	// reload with the config generation. Atomic pointers so the request path
	// reads them without the repository lock.
	adminAuth atomic.Pointer[webauth.Source]
	apiKeys   atomic.Pointer[webauth.Source]
}

// processServices holds process-lifetime services that survive reload.
// Embedded in Proxy so promoted selectors (p.metrics, p.reqLog, ...) stay
// unchanged.
type processServices struct {
	lifecycle          *runtimestate.Lifecycle
	runtimeState       runtimestate.Manager
	client             *http.Client
	quota              *runtimestate.QuotaTracker    // background quota poller; nil only in degenerate tests
	metrics            *obscounters.MetricsStore     // request counters (atomic); nil only in degenerate tests
	tokens             *obscounters.TokenCounter     // SSE-scanned token usage; nil only in degenerate tests
	agents             *obscounters.AgentCounter     // per-agent (UA) request/token counters; nil only in degenerate tests
	stats              *observestats.Store           // SQLite persistence for per-minute buckets; nil in tests (runtime services open it)
	flusher            *observestats.Flusher         // per-minute diff loop; nil in tests (runProxy starts it)
	reqLog             *requestlog.Logger            // per-request access log (full bodies); nil = disabled (default) or init failure
	reqLogStarted      bool                          // lifecycle owns loop/shutdown only when started by startRuntimeServices
	reqLogIndex        *requestlog.Indexer           // tailing SQLite index over the request log (web read path); nil = request log disabled or index open failed (reads fall back to directory scans)
	reqLogIndexStarted bool                          // lifecycle owns the indexer loop/shutdown only when started alongside reqLog
	sessionScan        *guardsession.Store           // split-exfiltration session windows; process-lifetime (survives reload like metrics), never serialized or logged
	adjudication       *adjudicate.Service           // AI second-opinion channel for guard pattern hits; process-lifetime, persisted verdict cache + session blocks
	responsesState     *protocol.ResponsesStateStore // previous_response_id replay for Responses clients bridged to stateless backends
	events             *observeevents.Hub            // live request monitor fan-out hub (SSE /api/events); always non-nil
	fusionReg          *fusion.Registry              // fusion orchestration observability (recent runs + per-workflow aggregates + daily budget); survives reload like events
	catalog            *catalog.Catalog              // models.dev metadata (context window + modalities) for request-aware routing; survives reload; refreshed async best-effort; nil = unavailable, degrade gracefully
	budget             *budget.Watcher               // monthly cost alert loop; nil unless budgets: configures a threshold

	// Upstream proxy resolution (internal/upstreamproxy): the resolver caches
	// OS system-proxy detection once per process; the transport pool is keyed
	// by effective proxy identity and survives reload like the default client.
	proxyResolver *upstreamproxy.Resolver
	transportsMu  sync.Mutex
	transports    map[string]*http.Transport

	// Runtime wire capabilities have their own leaf Store. The Store never
	// calls back into Proxy while locked and survives reload generations.
	wireCaps  runtimewire.Store
	wireProbe bool
	// Model-level protocol capabilities (chat/anthropic/responses per model),
	// persisted in their own model_caps.json (path derived from the quota
	// state path). Same leaf-lock + survives-reload discipline as wireCaps.
	modelCaps     runtimewire.ModelStore
	modelCapsPath string
	// Cache entries are generation-owned. Counters and their serialized disk
	// writer survive reload, including disabled generations.
	cacheStatePath string
	cacheCounters  *responsecache.Counters
	cachePersistMu sync.Mutex // serializes cache snapshot/write and durable reset
}

// Proxy holds the compiled provider instances + the config.
type Proxy struct {
	mu               sync.RWMutex  // guards generationState across reload (held by handler for the request)
	configGeneration atomic.Uint64 // incremented on every successful reload
	generationState
	processServices
	pricingMu sync.Mutex // guards pricing during refresh (thundering-herd guard on pricing.EnsureFresh)
	closeOnce sync.Once
	// pprofEnabled (MP_PPROF=1 at construction) exposes /debug/pprof/ on the
	// proxy handler for live profiling. Opt-in: profiles can carry request
	// data in heap samples, so the endpoint is off by default.
	pprofEnabled bool
}

// applyAuthSources swaps the S2 auth sources to match a config generation.
// Sources cache their token files internally (webauth cacheTTL), so swapping
// here only re-points at (possibly changed) paths. Caller holds p.mu on
// reload; the constructor calls it before serving.
func (p *Proxy) applyAuthSources(cfg *configdomain.Config) {
	p.adminAuth.Store(webauth.NewSource(cfg.Web.Auth.AdminTokenFile))
	p.apiKeys.Store(webauth.NewSource(cfg.Web.Auth.APIKeysFile))
}

func NewProxy(cfg *configdomain.Config) *Proxy {
	home, _ := os.UserHomeDir()
	p := NewProxyWithStatePath(cfg, filepath.Join(home, ".model-proxy", "quota_state.json"))
	// Production only: probe wire capabilities asynchronously at boot (the
	// injectable constructor leaves probing off; tests drive it synchronously).
	p.wireProbe = true
	p.startWireCapProbe()
	return p
}

// NewProxyWithStatePath is the injectable constructor used by tests so every
// Proxy owns an isolated state file before the tracker loads or starts.
func NewProxyWithStatePath(cfg *configdomain.Config, qpath string) *Proxy {
	// Apply the configured credentials mode (`credentials:`) before any pool
	// I/O: the account store and the web/login save paths resolve stores through
	// the accounts process default, and OAuth blob storage follows credstore's
	// process mode. Reload re-applies both per config. A non-empty
	// MP_CRED_STORE overriding only the OAuth side gets one visible line.
	accounts.SetProcessCredentialsMode(cfg.CredentialsMode())
	if note := accounts.CredentialMismatchNote(cfg.Credentials); note != "" {
		logx.Warnf("[startup] ⚠ %s", note)
	}
	// Publish the config-level global proxy for the automatic chain
	// (env → system → direct) used by non-forwarding outbound calls
	// (usage/quota/login/pricing). Reload re-publishes it per generation.
	upstreamproxy.SetDefaultProxy(cfg.Proxy)
	built := providerbuild.BuildProviders(cfg, accounts.NewStore(accounts.HomeDir()), providerbuild.BuildOpts())
	// Same scanner entry point as Reload: startup and reload build identical
	// generations. An error is only reachable with an unvalidated Config
	// (validate rejects bad guard.extra_patterns at load) — degrade to the
	// embedded table + known secrets rather than start without any guard.
	guardScanner, err := buildGuardScanner(cfg, built.Secrets)
	if err != nil {
		logx.Warnf("[startup] guard scanner: %v; falling back to built-in rules + known secrets", err)
		// The fallback can only fail if the embedded rule table itself is
		// broken (custom patterns are nil here, so config cannot be the cause).
		// Degrade to nil — forward skips the guard entirely — rather than run
		// a scanner we no longer trust; the warning makes the loss loud.
		fallback := make([]guard.KnownSecret, 0, len(built.Secrets))
		if cfg.Guard.KnownSecretsEnabled() {
			for _, sec := range built.Secrets {
				fallback = append(fallback, guard.KnownSecret{Value: sec.Value, Label: sec.Label})
			}
		}
		guardScanner, err = guard.NewScannerWithOptions(nil, fallback, cfg.Guard.ExtraPaths, guard.Options{Decode: cfg.Guard.DecodeEnabled()})
		if err != nil {
			// Errorf, not Warnf: this announces a security control is OFF, so
			// it must survive even log_level: error (level filtering, 063b2f9).
			// The sibling warn above keeps a working fallback scanner, so it
			// stays level-filtered.
			logx.Errorf("[startup] guard fallback scanner failed: %v; outbound guard scanning is DISABLED for this process", err)
			guardScanner = nil
		}
	}
	// http.DefaultTransport pools at most 2 idle connections per host; concurrent
	// streams to one upstream would re-dial TLS after the first two close. The
	// proxy fans out to a handful of upstream hosts, so pool generously instead.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 100
	p := &Proxy{
		generationState: generationState{
			cfg:       cfg,
			providers: built.Providers,
			poolIndex: built.PoolIndex,
			parentOf:  built.ParentOf,
		},
		processServices: processServices{
			lifecycle:     runtimestate.NewLifecycle(),
			client:        &http.Client{Timeout: 0, Transport: transport},
			proxyResolver: upstreamproxy.NewResolver(),
			transports:    map[string]*http.Transport{},
		},
		// Read once here (not per request): MP_PPROF=1 turns on /debug/pprof/.
		pprofEnabled: os.Getenv("MP_PPROF") == "1",
	}
	p.runtimeState.ReplaceGeneration(1)
	p.configGeneration.Store(1)
	// Reload-owned: assigned before any request/goroutine can read it, swapped
	// under p.mu on reload.
	p.guardScanner = guardScanner
	p.applyAuthSources(cfg)
	// Split-exfiltration session windows: cross-generation observation state
	// (like p.metrics below), NOT reload-owned — reload must not wipe in-flight
	// session context. Never logged or persisted (see internal/guard/session).
	p.sessionScan = guardsession.NewStore()
	// AI second-opinion channel for guard pattern hits: process-lifetime
	// (workers idle while guard.adjudicate is off; persisted session blocks
	// stay enforced regardless). State derives from the injected state path
	// (same directory as quota/cache state) so every Proxy owns isolated
	// adjudication files. Drained in Close before the audit log.
	p.startAdjudication(adjudicationStateDir(qpath))
	// The OAuth subset is tracked separately so the refresh loop can re-sync it
	// (OAuth tokens rotate in place during serve) without re-running a full
	// BuildProviders pass; the pool subset is the stable base of every rebuild.
	p.guardPoolSecrets = built.PoolSecrets
	p.guardOAuthSecrets = built.OAuthSecrets
	p.derivedRoutes = routing.DeriveRoutesFrom(cfg)
	p.expandedRoutes = p.buildExpandedRoutes()
	p.routeKeys = routeKeySet(p.expandedRoutes)
	// Config-time routing hazards (reasoning-replay models behind conversion,
	// missing protocol: on hint providers): appended to the warnings channel
	// (/api/status + `models` CLI) AND logged — the operator should see them at
	// boot, not only when they open the dashboard.
	if hw := routing.ConfigRoutingWarnings(cfg, p.expandedRoutes); len(hw) > 0 {
		p.routeWarnings = append(p.routeWarnings, hw...)
		for _, w := range hw {
			logx.Warnf("[startup] ⚠ %s", w)
		}
	}
	// The tracker reads cfg/providers asynchronously via the snapshot closures
	// (each takes p.mu.RLock), so reloads are picked up without recreating it.
	p.quota = runtimestate.NewQuotaTracker(qpath,
		func() *configdomain.Config { return p.cfgSnapshot() },
		func() map[string]provider.Provider { return p.providerSnapshot() },
		&p.runtimeState)
	p.quota.Generation = p.configGeneration.Load
	p.quota.FullSnapshot = p.snapshotPersistedState
	p.quota.Start()
	// Guard OAuth known-secret re-sync: codex/aqp providers rotate their tokens
	// in place during serve (rewriting <name>_oauth_auth.json), which would
	// otherwise leave the boot-time scanner matching stale values until the
	// next reload. The loop re-collects the OAuth files on the quota-poll beat
	// and swaps the scanner in place, generation-consistent. Lifecycle-admitted
	// like the other loops: stopped and waited by Close.
	p.lifecycle.Run(p.guardSecretRefreshLoop)
	p.metrics = counters.NewMetricsStore()
	// SSE token counter. Persistence (baseline restore + per-minute flush) is
	// projected by statsFlusher into internal/observe/stats.Store, opened only
	// by lifecycle services so direct-NewProxy tests stay in-memory.
	p.tokens = counters.NewTokenCounter()
	// Per-agent counters (detected from the client UA). Flushed alongside the
	// minute buckets by the same flusher; nil-stats tests keep them in-memory.
	p.agents = counters.NewAgentCounter()
	// Exact-match response cache. nil unless cache.enabled is set in config, so
	// the default (off) path and direct-NewProxy tests pay zero overhead.
	// Seeded from cache_state.json so restarts continue the hit/miss history.
	p.cacheStatePath = CacheStatePath(qpath)
	p.cacheCounters = &responsecache.Counters{}
	state := loadCacheState(p.cacheStatePath)
	models := make([]responsecache.ModelStat, 0, len(state.Models))
	for _, m := range state.Models {
		models = append(models, responsecache.ModelStat{Name: m.Model, Hits: m.Hits, Misses: m.Misses})
	}
	p.cacheCounters.Seed(state.Hits, state.Misses, models)
	p.cache = p.newResponseCache(cfg.Cache)
	p.responsesState = protocol.NewResponsesStateStore(protocol.ResponsesStatePath(qpath))
	p.modelCapsPath = runtimewire.ModelCapsPath(qpath)
	// Live request monitor hub (SSE /api/events). Always on — empty unless a Web
	// UI client subscribes; publish is non-blocking so it never stalls forward.
	p.events = observeevents.NewHub()
	// Fusion orchestration observability registry (recent runs + per-workflow
	// aggregates + daily budget counters). Like the event hub, reload does NOT
	// rebuild it — aggregates and today's budget survive config edits.
	p.fusionReg = fusion.NewRegistry()
	// Detached Shadow runtime (sample rate, concurrency gate, shared client). Stored
	// in an atomic pointer so reload can swap the whole bundle race-free; each
	// dispatch loads it once and uses that snapshot, so in-flight shadow goroutines
	// finish on the old bundle while new traffic follows the reloaded config.
	p.shadow.Store(shadow.NewRuntime(shadow.Options{
		SampleRate:    cfg.ShadowSampleRate,
		MaxConcurrent: cfg.ShadowMaxConcurrent,
		Timeout:       cfg.Scheduling.Timeout(),
	}))
	// Restore the per-route sticky selections persisted before the last restart,
	// so the proxy resumes parking on the same providers (prompt-cache-friendly).
	// Gated like the health restore below: a file with a MISMATCHING config
	// fingerprint belongs to a different config (or a test binary sharing the
	// state file) — its route→provider parks must not leak over (route names
	// like "m1" collide across configs even when providers don't). Legacy
	// files without a fingerprint keep the historical restore behavior.
	fp := healthConfigFingerprint(cfg)
	fpMatch := p.quota.LoadedHealthFP == "" || p.quota.LoadedHealthFP == fp
	if p.quota.LoadedHealthFP != "" && !fpMatch {
		// Quota snapshots are provider/account observations too. A changed provider
		// identity or endpoint must not inherit the old file's scheduling tier.
		p.quota.ClearForGeneration(p.configGeneration.Load())
	}
	if loaded := p.quota.LoadedSticky; len(loaded) > 0 && fpMatch {
		p.runtimeState.RestoreSticky(loaded)
	}
	// Restore frozen health state (rate-limit/circuit cooldowns, operator
	// freezes, model lockouts, learned param blocklist) persisted before the
	// last restart — ONLY when the file's config fingerprint matches the
	// current config: health is keyed by provider name, so without the gate a
	// different config (or a test binary sharing the state file) would inherit
	// cooldowns onto unrelated same-named providers. Expired cooldowns
	// self-heal by being dropped; operator freezes have no expiry and restore
	// as-is. A restored circuit gets a full failure count so its next failure
	// re-opens it immediately (same semantics as before).
	if loaded := p.quota.LoadedHealth; len(loaded) > 0 && p.quota.LoadedHealthFP != "" && p.quota.LoadedHealthFP == fp {
		p.runtimeState.RestoreHealth(loaded, time.Now(), cfg.Scheduling.Threshold())
	}
	// Restore wire capability verdicts (independent of the health fingerprint:
	// capabilities are endpoint properties). A verdict is honored only while
	// its recorded base_url still matches the current config — an endpoint
	// change invalidates it and triggers a re-probe at the next boot probe.
	if loaded := p.quota.LoadedWireCaps; len(loaded) > 0 {
		baseURLs := make(map[string]string, len(cfg.Providers))
		for name, prov := range cfg.Providers {
			baseURLs[name] = prov.OpenAIBaseURL
		}
		p.wireCaps.RestoreMatching(loaded, baseURLs)
	}
	// Restore model-level protocol capabilities from model_caps.json. A
	// provider's entry is honored only while its protocol-relevant config
	// fingerprint (base urls / provider_id / headers) still matches — an
	// unchanged fingerprint means the verdicts are reused with NO re-probe.
	if loaded, err := runtimewire.LoadModelCapsFile(p.modelCapsPath); err != nil {
		logx.Warnf("[modelcaps] %v; starting empty", err)
	} else if len(loaded) > 0 {
		p.modelCaps.Restore(loaded, protocolFingerprints(cfg))
	}
	return p
}

// Close releases every Proxy-owned background component and performs final
// flushes. It is idempotent; lifecycle admission is closed before waiting so a
// concurrent reload cannot add a catalog refresh behind shutdown.
func (p *Proxy) Close() {
	p.closeOnce.Do(p.closeRuntimeServices)
}

// resetStats zeroes all call-statistics state: the in-memory metrics + token
// counters, the persisted SQLite bucket history, and the flusher's diff
// baseline (so the next flush sees zero delta rather than zero-minus-old
// negatives). Drives POST /api/tokens/reset ("reset counters").
func (p *Proxy) resetStats() error {
	// When a flusher exists, delegate to flusher.reset which does the full stats
	// reset (counters + DB + baseline) UNDER the flusher's lock — preventing a
	// concurrent per-minute flush from writing stale deltas to the just-cleared
	// DB (the resetStats vs flush race).
	if p.flusher != nil {
		if err := p.flusher.Reset(); err != nil {
			return fmt.Errorf("reset persisted stats: %w", err)
		}
	} else {
		// Non-flusher path (degenerate tests): clear durable state first. If that
		// fails, keep the in-memory totals so a later restart cannot resurrect a
		// history the user was told had been reset.
		if p.stats != nil {
			if err := p.stats.Reset(); err != nil {
				return fmt.Errorf("reset persisted stats: %w", err)
			}
		}
		if p.metrics != nil {
			p.metrics.Reset()
		}
		if p.tokens != nil {
			p.tokens.Reset()
		}
		if p.agents != nil {
			p.agents.Reset()
		}
	}
	// The response-cache counters are deliberately NOT reset here: the cache
	// hit rate is an operational metric (like the guard usage counters), not
	// call-statistics — coupling it into "reset counters" silently destroyed
	// the accumulated history.
	return nil
}

// newResponseCache rebuilds generation-owned entries around the process-wide
// counter owner. No disk I/O is performed during reload's generation swap.
func (p *Proxy) newResponseCache(config configdomain.CacheConfig) *responsecache.Store {
	return NewResponseCache(config, p.cacheCounters)
}

// NewResponseCache adapts resolved application configuration into the
// repository-leaf cache component.
func NewResponseCache(config configdomain.CacheConfig, counters *responsecache.Counters) *responsecache.Store {
	if !config.IsEnabled() {
		return nil
	}
	return responsecache.New(responsecache.Options{
		Counters:     counters,
		TTL:          config.TTLDuration(),
		MaxEntries:   config.MaxEntriesValue(),
		MaxBodyBytes: config.MaxBodyBytesValue(),
	})
}
