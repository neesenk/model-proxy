package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"model-proxy/provider"
)

// reqIDPrefix is a per-process 8-hex-char nonce (generated once from crypto/rand
// at package init via newRequestID). Combined with an atomic counter, this gives
// each request a unique id with ONE atomic add (no crypto/rand syscall per
// request). Always generated — even when request_log is off — so live start↔end
// event pairing + Live↔Requests cross-page linking work.
var reqIDPrefix = newRequestID()[:8]
var reqIDCounter atomic.Uint64

func nextRequestID() string {
	return fmt.Sprintf("%s-%010d", reqIDPrefix, reqIDCounter.Add(1))
}

// Proxy holds the compiled provider instances + the config.
type Proxy struct {
	mu             sync.RWMutex // guards cfg/providers across reload (held by handler for the request)
	healthMu       sync.Mutex   // guards health + sticky maps (runtime circuit/rate-limit/sticky state)
	cfg            *Config
	providers      map[string]provider.Provider // provider name → Provider (shared)
	client         *http.Client
	health         map[string]*providerHealth // provider name → circuit/rate-limit state
	sticky         map[string]routeSticky     // exposed model → current provider + since
	pins           map[string]pinEntry        // exposed model → manual pin (healthMu); hot-switch, overrides schedule
	quota          *quotaTracker              // background quota poller; nil only in degenerate tests
	metrics        *metricsStore              // request counters (atomic); nil only in degenerate tests
	tokens         *tokenCounter              // SSE-scanned token usage; nil only in degenerate tests
	agents         *agentCounter              // per-agent (UA) request/token counters; nil only in degenerate tests
	stats          *statsStore                // SQLite persistence for per-minute buckets; nil in tests (runProxy opens it)
	flusher        *statsFlusher              // per-minute diff loop; nil in tests (runProxy starts it)
	reqLog         *requestLogger             // per-request access log (full bodies); nil = disabled (default) or init failure
	cache          *responseCache             // exact-match response cache (prompt-hash + TTL); nil = disabled
	events         *eventHub                  // live request monitor fan-out hub (SSE /api/events); always non-nil
	catalog        *modelsDevCatalog          // models.dev metadata (context window + modalities) for request-aware routing; nil = unavailable, degrade gracefully
	shadowSem      chan struct{}              // concurrency gate for shadow goroutines (buffered = max concurrent)
	shadowClient   *http.Client               // shared HTTP client for shadow requests (not per-request)
	shadowSampRate float64                    // 0-1; fraction of requests to shadow (default 1.0)
	pricingMu      sync.Mutex                 // guards pricing during refresh (thundering-herd guard on ensurePricingFresh)

	// Credential-pool unrolling (buildProviders). For a multi-account parent,
	// poolIndex[parent] = its sorted virtual ids ("name#<id>") and parentOf is
	// the inverse. Single-account / not-logged-in providers appear in neither
	// map (their id == the plain name). Guards: same as the struct — poolIndex
	// and parentOf are rebuilt on reload under p.mu; spreadCtr under healthMu.
	poolIndex      map[string][]string      // parent name → sorted virtual ids (only multi-account parents)
	parentOf       map[string]string        // virtual id → parent name
	spreadCtr      map[string]uint64        // parent name → session-assignment round-robin counter (healthMu)
	expandedRoutes map[string][]RouteTarget // exposed model → expanded targets (explicit + implicit)
	implicitRoutes map[string]RouteTarget   // exposed model → single target auto-derived from logged-in providers' model lists (for models not in cfg.Routes)
	routeWarnings  []string                 // ambiguity warnings for implicit routes (multi-provider); surfaced in `models` CLI + /api/status

	// scheduleHook is a test-only hook fired in forward right after schedule(),
	// capturing the threaded sessionKey. Nil in production.
	scheduleHook func(sessionKey string)
}

// providerHealth tracks a provider's circuit-breaker and rate-limit state.
type providerHealth struct {
	consecutiveFailures int
	circuitOpenUntil    time.Time // zero = closed
	rateLimitedUntil    time.Time // zero = not limited
	halfOpenInFlight    bool      // a half-open probe is running
}

// routeSticky records the provider a route is currently parked on + when it was
// chosen (for the sticky_dwell window).
type routeSticky struct {
	provider string
	since    time.Time
}

// pinEntry is a manual route→provider pin (model-proxy pin <route> <provider>
// --ttl). expiresAt zero = no expiry (until unpin). Guarded by healthMu.
type pinEntry struct {
	provider  string
	expiresAt time.Time
}

// active reports whether the pin is still in effect at now (zero expiresAt =
// never expires).
func (e pinEntry) active(now time.Time) bool {
	return e.expiresAt.IsZero() || now.Before(e.expiresAt)
}

// expiresLabel returns "" (no expiry), a "expires <relative>" hint, or "expired".
func (e pinEntry) expiresLabel(now time.Time) string {
	if e.expiresAt.IsZero() {
		return ""
	}
	d := e.expiresAt.Sub(now)
	if d <= 0 {
		return "expired"
	}
	return "expires in " + d.Round(time.Second).String()
}

// buildProviders creates provider.Provider instances from config. Each provider
// owns its auth/usage/quota/logout (Phase 1-5 + aqp-fetch migration); buildOne
// only wires the remaining callback (FetchModelsFn: volcengine's V4-signed
// ListArkAgentPlanModel) + the per-provider config fields.
//
// A provider whose credential pool (loadPool) has ≥2 accounts is UNROLLED into
// one virtual provider per account, keyed "name#<accountID>"; the parent name
// is NOT a key (only the virtuals are). A 1-entry PLURAL pool (the file `login`
// writes) is bound in-memory under the plain name. Only the no-plural-file /
// not-logged-in case stays file-backed (reading the legacy singular
// <name>_apikey.json via loadPool's fallback) — binding the cred there would
// break the embedded ApiKeyBase, which reads the (non-existent) singular file.
//
// It also derives the credential-pool index maps in the SAME pass:
//   - poolIndex[parent] = its sorted virtual ids ("name#<accountID>")
//   - parentOf[vid]    = the parent name
//
// Loading the pool once (rather than separately in buildPoolIndex) closes a
// TOCTOU window across reload and guarantees the index only references virtual
// ids that actually exist in the returned providers map: a virtual whose
// buildOne returned nil (provider.New error) is skipped in BOTH the providers
// map AND the index. Single-account / not-logged-in providers appear in
// neither index map (their id == the plain name).
func buildProviders(cfg *Config) (map[string]provider.Provider, map[string][]string, map[string]string) {
	m := map[string]provider.Provider{}
	poolIndex := map[string][]string{}
	parentOf := map[string]string{}
	for name, prov := range cfg.Providers {
		// Surface a corrupt pool file instead of silently dropping the provider —
		// treat as empty (not-logged-in) but log the diagnostic so a 502 isn't
		// mute. loadPool already wraps the parse error with the file path.
		pool, poolErr := loadPool(name, prov.Provider)
		if poolErr != nil {
			log.Printf("[proxy] pool %s unreadable: %v; treating as not-logged-in", name, poolErr)
			pool = credentialPool{}
		}
		// Distinguish a 1-entry PLURAL pool (written by `login` via savePool) from
		// the legacy singular fallback / not-logged-in case. The plural file
		// carries its own key on disk; the legacy path must stay file-backed
		// (reading <name>_apikey.json) so Login/Logout file semantics are
		// byte-for-byte unchanged. stat-ing the plural path — not loadPool's
		// result — is what tells the two apart: loadPool wraps a legacy singular
		// as a 1-entry pool, which would otherwise mis-route it to the bind path.
		_, statErr := os.Stat(poolPath(name))
		pluralExists := statErr == nil
		// Distinguish "plural pool file exists but is empty" (cleared by
		// `logout --all` or truncated by a crashed/corrupt write) from "provider
		// was never logged in". With atomic savePool (#1) this is near-unreachable
		// in practice, but a distinct diagnostic means a 502 isn't mute if it does
		// happen. Only emitted when the pool read back OK — an unreadable pool is
		// already covered by the "unreadable" log above.
		if pluralExists && len(pool.Accounts) == 0 && poolErr == nil {
			log.Printf("[proxy] pool %s exists but has 0 accounts (cleared or corrupt); treating as not logged in", name)
		}
		if !pluralExists || len(pool.Accounts) == 0 {
			// no plural pool -> legacy singular fallback OR not logged in:
			// cred=nil keeps ApiKeyBase file-backed (pre-pool path).
			if p := buildOne(cfg, name, prov, accountCred{}); p != nil {
				m[name] = p
			}
			continue
		}
		if len(pool.Accounts) == 1 {
			// 1-entry PLURAL pool (from `login`): bind the account's cred under
			// the plain name, no virtuals. The singular file does not exist in
			// this case, so binding in-memory is required for the provider to
			// authenticate at all.
			if p := buildOne(cfg, name, prov, pool.Accounts[0].cred()); p != nil {
				m[name] = p
			}
			continue
		}
		vids := make([]string, 0, len(pool.Accounts))
		for _, a := range pool.Accounts {
			vid := name + "#" + a.ID
			p := buildOne(cfg, name, prov, a.cred())
			if p == nil {
				// provider.New failed for this account — skip it in BOTH the
				// providers map and the index, so poolIndex never lists an id
				// that isn't runnable (which would make expandedRoutes produce
				// a target that forward can't serve).
				continue
			}
			m[vid] = p
			vids = append(vids, vid)
			parentOf[vid] = name
		}
		if len(vids) > 0 {
			sort.Strings(vids)
			poolIndex[name] = vids
		}
	}
	return m, poolIndex, parentOf
}

// buildOne constructs a single provider instance (a real provider for the
// single-account path, or a virtual for one credential-pool entry) bound to
// cred. When cred is non-empty the key is bound via pcfg.BoundAPIKey, which the
// apikey constructors (zhipu/deepseek/volcengine) feed into a bound ApiKeyBase
// whose LoadKey/AuthHeaders use the in-memory key. This covers all three paths
// (forward AuthHeaders, FetchModels via p.AuthHeaders, and Usage/Quota fetches
// which call p.AuthHeaders) - one binding point, no separate cfg.Auth needed.
//
// When cred is empty all three fall back to the legacy file-backed behavior
// (identical to the pre-pool buildProviders).
func buildOne(cfg *Config, name string, prov Provider, cred accountCred) provider.Provider {
	pcfg := &provider.Config{
		ProviderID:    prov.Provider,
		ProviderName:  name,
		OpenAIBaseURL: prov.OpenAIBaseURL,
		Headers:       prov.Headers,
		UsageURL:      prov.UsageURL,
		BoundAPIKey:   cred.APIKey, // binding point #1 (forward path)
		Models:        prov.Models, // for the usage-display fallback (listConfigModels)
		OAuthAuthFile: authFilePath(name, "oauth_auth"),
	}
	// Wire callbacks by provider type. aqp needs none: its Quota/Usage fetch
	// monthly_usage directly (AqpProvider.fetchMonthlyUsage reads the SSO-cookie
	// store at OAuthAuthFile + POSTs), like the other providers.
	switch prov.Provider {
	case "codex":
		pcfg.ClientVersion = resolveCodexClientVersion(prov.ClientVersion, codexCLIVersion, codexCacheVersion)
	case "volcengine":
		pcfg.FetchModelsFn = func() ([]string, error) { return listArkAgentPlanModelIDs(name) }
		// GetAFPUsage is V4-signed with the virtual's own AK/SK (bound here so
		// each pooled account queries its own Agent Plan quota); falls back to
		// the legacy <name>_apikey.json when unbound (single-account path).
		pcfg.AccessKey = cred.AccessKey
		pcfg.SecretKey = cred.SecretKey
		pcfg.VolcengineCredFile = filepath.Join(homeDir(), ".model-proxy", name+"_apikey.json")
	}
	p, err := provider.New(pcfg, name)
	if err != nil {
		log.Printf("[proxy] failed to build provider %s: %v (using auth-only)", name, err)
		return nil
	}
	return p
}
func NewProxy(cfg *Config) *Proxy {
	providers, poolIndex, parentOf := buildProviders(cfg)
	p := &Proxy{
		cfg:       cfg,
		providers: providers,
		client:    &http.Client{Timeout: 0},
		health:    map[string]*providerHealth{},
		sticky:    map[string]routeSticky{},
		pins:      map[string]pinEntry{},
		spreadCtr: map[string]uint64{},
		poolIndex: poolIndex,
		parentOf:  parentOf,
	}
	p.implicitRoutes, p.routeWarnings = synthesizeImplicitRoutes(cfg)
	p.expandedRoutes = p.buildExpandedRoutes()
	// The tracker reads cfg/providers asynchronously via the snapshot closures
	// (each takes p.mu.RLock), so reloads are picked up without recreating it.
	home, _ := os.UserHomeDir()
	qpath := filepath.Join(home, ".model-proxy", "quota_state.json")
	p.quota = newQuotaTracker(qpath,
		func() *Config { return p.cfgSnapshot() },
		func() map[string]provider.Provider { return p.providerSnapshot() })
	p.quota.stickySnapshot = p.snapshotSticky
	p.quota.start()
	p.metrics = newMetricsStore()
	// SSE token counter. Persistence (baseline restore + per-minute flush) is
	// owned by statsStore/flusher, opened in runProxy so direct-NewProxy tests
	// stay in-memory and don't touch ~/.model-proxy/.
	p.tokens = newTokenCounter()
	// Per-agent counters (detected from the client UA). Flushed alongside the
	// minute buckets by the same flusher; nil-stats tests keep them in-memory.
	p.agents = newAgentCounter()
	// Exact-match response cache. nil unless cache.enabled is set in config, so
	// the default (off) path and direct-NewProxy tests pay zero overhead.
	p.cache = newResponseCache(cfg.Cache)
	// Live request monitor hub (SSE /api/events). Always on — empty unless a Web
	// UI client subscribes; publish is non-blocking so it never stalls forward.
	p.events = newEventHub()
	// Shadow concurrency gate + shared client + sample rate.
	maxConc := cfg.ShadowMaxConcurrent
	if maxConc <= 0 {
		maxConc = 4
	}
	p.shadowSem = make(chan struct{}, maxConc)
	p.shadowClient = &http.Client{Timeout: cfg.Scheduling.timeout()}
	p.shadowSampRate = 1.0 // default; nil ShadowSampleRate = all requests
	if cfg.ShadowSampleRate != nil {
		p.shadowSampRate = *cfg.ShadowSampleRate // explicit 0.0 = off
	}
	// Restore the per-route sticky selections persisted before the last restart,
	// so the proxy resumes parking on the same providers (prompt-cache-friendly).
	if loaded := p.quota.LoadedSticky; len(loaded) > 0 {
		p.healthMu.Lock()
		for k, v := range loaded {
			p.sticky[k] = v
		}
		p.healthMu.Unlock()
	}
	return p
}

// resetStats zeroes all call-statistics state: the in-memory metrics + token
// counters, the persisted SQLite bucket history, and the flusher's diff
// baseline (so the next flush sees zero delta rather than zero-minus-old
// negatives). Drives POST /api/tokens/reset ("reset counters").
func (p *Proxy) resetStats() {
	// When a flusher exists, delegate to flusher.resetAll which does the full
	// reset (counters + DB + baseline) UNDER the flusher's lock — preventing a
	// concurrent per-minute flush from writing stale deltas to the just-cleared
	// DB (the resetStats vs flush race).
	if p.flusher != nil {
		p.flusher.resetAll(p)
		return
	}
	// Non-flusher path (degenerate tests): reset directly.
	if p.metrics != nil {
		p.metrics.reset()
	}
	if p.tokens != nil {
		p.tokens.reset()
	}
	if p.agents != nil {
		p.agents.reset()
	}
	if p.cache != nil {
		p.cache.reset()
	}
	if p.stats != nil {
		if err := p.stats.resetAll(); err != nil {
			log.Printf("[stats] resetAll failed: %v", err)
		}
	}
}

// cfgSnapshot returns the current config under a brief read lock. Used by the
// quota tracker (which reads cfg asynchronously from its poll goroutine).
func (p *Proxy) cfgSnapshot() *Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// pricingSnapshot returns a usable price catalog, refreshing the cache when
// stale (best-effort; offline falls back to the stale cache). Nil-safe (including
// a nil cfg, as in degenerate tests) and thundering-herd-safe. Returns nil when
// pricing is disabled.
func (p *Proxy) pricingSnapshot() *pricingCatalog {
	if p == nil {
		return nil
	}
	cfg := p.cfgSnapshot()
	if cfg == nil || !cfg.Pricing.enabled() {
		return nil
	}
	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()
	cat, err := ensurePricingFresh(pricingCachePath(), cfg.Pricing.sourceURL(), realPricingFetch, false, cfg.Pricing.ttl())
	if err != nil || cat == nil {
		return emptyPricingCatalog()
	}
	return cat
}

// priceOverrides returns the current config `prices:` overrides for the handler.
func (p *Proxy) priceOverrides() map[string]PriceConfig {
	cfg := p.cfgSnapshot()
	if cfg == nil {
		return nil
	}
	return cfg.Prices
}

// snapshotConfig returns a shallow copy of the current config under a brief
// read lock. Used by the web account-add flow, which passes the snapshot to the
// add cores (addApikeyAccount/addVolcengineAccount) so they see a consistent cfg
// without holding p.mu during their network validation call (usage_url probe).
// The Providers/Routes maps are shared with the live cfg (shallow copy) — that's
// safe because the cores only READ them; a concurrent reload swaps p.cfg to a
// brand-new *Config, it never mutates the maps in place.
func (p *Proxy) snapshotConfig() *Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c := *p.cfg
	return &c
}

// catalogSnapshot returns the current models.dev catalog under a brief read lock,
// or nil if unavailable (tests, or the best-effort load failed). Request-aware
// routing (context-window fallback, capability routing) degrades to a no-op when
// nil — the proxy forwards unchanged rather than guessing.
func (p *Proxy) catalogSnapshot() *modelsDevCatalog {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.catalog
}

// initCatalog loads the models.dev metadata catalog best-effort (context window
// + modalities for request-aware routing). On failure p.catalog stays nil and
// the proxy runs without request-aware routing (forwards unchanged). Called from
// runProxy only — direct NewProxy callers (tests) stay offline; tests that need
// metadata set p.catalog directly.
func (p *Proxy) initCatalog() {
	cat, err := ensureCatalogFresh(cachePath(), modelsDevEndpoint(), realModelsDevFetch, false)
	if err != nil || cat == nil {
		if err != nil {
			log.Printf("[models] catalog load failed: %v - running without request-aware routing", err)
		}
		return
	}
	p.mu.Lock()
	p.catalog = cat
	p.mu.Unlock()
}

// providerSnapshot returns the current provider map under a brief read lock.
func (p *Proxy) providerSnapshot() map[string]provider.Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.providers
}

// snapshotSticky returns a copy of the per-route sticky map under healthMu, for
// persistence by the quota tracker (restored on boot — see NewProxy). Only
// ROUTE-keyed entries (keys present in cfg.Routes) are persisted: per-session
// entries (keyed by x-claude-code-session-id) matter only within a running
// daemon's dwell window and self-heal on the next request, so writing them to
// quota_state.json would just accumulate client conversation IDs on disk.
func (p *Proxy) snapshotSticky() map[string]routeSticky {
	cfg := p.cfgSnapshot()
	routes := cfg.Routes
	// Implicit-route names aren't in cfg.Routes but should persist too — snapshot
	// them under the same RLock as cfg (they're mutated on reload under p.mu).
	p.mu.RLock()
	implicit := p.implicitRoutes
	p.mu.RUnlock()
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	out := make(map[string]routeSticky, len(p.sticky))
	for k, v := range p.sticky {
		if _, isRoute := routes[k]; !isRoute {
			if _, isImplicit := implicit[k]; !isImplicit {
				continue // session-keyed — don't persist
			}
		}
		out[k] = v
	}
	return out
}

func (p *Proxy) reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	newProviders, newPoolIndex, newParentOf := buildProviders(cfg)
	// synthesizeImplicitRoutes does per-provider loadPool file I/O — compute it
	// BEFORE taking the write lock so in-flight forward handlers (RLock) aren't
	// stalled behind N credential-file reads on every reload.
	newImplicit, newWarnings := synthesizeImplicitRoutes(cfg)
	p.mu.Lock()
	p.cfg = cfg
	p.providers = newProviders
	// Rebuild the pool index + expanded routes from the single buildProviders
	// pass. Doing this under the write lock means request readers (which take
	// the read lock) see a consistent cfg/providers/poolIndex/expandedRoutes.
	p.poolIndex = newPoolIndex
	p.parentOf = newParentOf
	p.implicitRoutes = newImplicit
	p.routeWarnings = newWarnings
	p.expandedRoutes = p.buildExpandedRoutes()
	// Rebuild the cache from the new config (pure in-memory, no goroutine/file
	// lifecycle to drain — safe to swap). cache.enabled toggled via reload now
	// takes effect immediately.
	p.cache = newResponseCache(cfg.Cache)
	p.mu.Unlock()
	// Reset health + sticky state — a reload is the operator's way to clear
	// stuck circuit-open / rate-limited / sticky-dwell state.
	p.healthMu.Lock()
	p.health = map[string]*providerHealth{}
	p.sticky = map[string]routeSticky{}
	p.spreadCtr = map[string]uint64{}
	p.healthMu.Unlock()
	// The tracker reads the new cfg/providers via its snapshot closures, so it
	// is NOT stopped/recreated on reload. Kick an immediate poll so newly added
	// providers show up at once (removed ones simply go stale and age out).
	if p.quota != nil {
		go p.quota.pollAll(time.Now())
	}
	// Refresh the models.dev catalog best-effort so newly configured model names
	// resolve their context window / modalities for request-aware routing. A
	// failure leaves the previous catalog in place (never fails the reload).
	go p.initCatalog()
	// request_log is NOT rebuilt on reload (the logger owns a background
	// goroutine + open file; restarting it mid-flight needs careful drain). So
	// a config that enables/tunes request_log via SIGHUP won't take effect until
	// restart. Warn when the operator clearly expects logging but it isn't
	// active, so this isn't a silent no-op.
	if cfg.RequestLog.Enabled && p.reqLog == nil {
		log.Printf("[reload] request_log.enabled is true but logging is not active (reload cannot start it); restart the daemon to enable request logging")
	}
	return nil
}

// buildExpandedRoutes returns routes with pooled targets fanned out to their
// virtual children: a target whose provider is a pooled parent (key present in
// poolIndex) is replaced by its N virtuals, each with the SAME Model + Priority
// as the original; non-pooled targets pass through unchanged. Routes with no
// pooled targets are returned as-is (same slice contents).
//
// Caller holds p.mu (write) — in NewProxy / reload, after buildProviders has
// populated poolIndex. forward + scheduleStatus read the result via the
// expandedRoutes field instead of cfg.Routes, so the fan-out is transparent to
// the scheduling/circuit code (which operates on provider names).
func (p *Proxy) buildExpandedRoutes() map[string][]RouteTarget {
	out := make(map[string][]RouteTarget, len(p.cfg.Routes))
	for exposed, targets := range p.cfg.Routes {
		var exp []RouteTarget
		for _, t := range targets {
			exp = append(exp, p.expandTarget(t)...)
		}
		out[exposed] = exp
	}
	// Merge implicit routes (auto-derived for unrouted models served by a logged-in
	// provider). Explicit routes win; implicit targets the parent so pool fan-out
	// applies via expandTarget too.
	for exposed, t := range p.implicitRoutes {
		if _, explicit := out[exposed]; explicit {
			continue
		}
		out[exposed] = p.expandTarget(t)
	}
	return out
}

// expandTarget fans a single route target out across a pooled provider's virtuals
// (same Model/Priority); non-pooled targets pass through unchanged.
func (p *Proxy) expandTarget(t RouteTarget) []RouteTarget {
	vids, pooled := p.poolIndex[t.Provider]
	if !pooled {
		return []RouteTarget{t}
	}
	out := make([]RouteTarget, 0, len(vids))
	for _, vid := range vids {
		out = append(out, RouteTarget{Provider: vid, Model: t.Model, Priority: t.Priority})
	}
	return out
}

// loggedInProviders returns the set of provider names (parents) that have ≥1
// stored credential (plural pool OR legacy singular file). Used to decide which
// providers can actually serve an implicit (auto) route. buildProviders builds
// all configured providers (logged-in or not, file-backed), so its result map
// can't answer "logged in?" — this does, via the same loadPool it uses.
func loggedInProviders(cfg *Config) map[string]bool {
	out := map[string]bool{}
	for name, prov := range cfg.Providers {
		if pool, _ := loadPool(name, prov.Provider); len(pool.Accounts) > 0 {
			out[name] = true
		}
	}
	return out
}

// synthesizeImplicitRoutesFrom is the pure, testable core. For each model name
// that is NOT already an explicit route key AND is served by ≥1 logged-in
// provider, it creates a single-target implicit route to the alphabetically-first
// logged-in provider that serves it; if >1 logged-in provider serves it, the
// others are dropped and a warning is emitted. Explicit routes always win.
func synthesizeImplicitRoutesFrom(cfg *Config, loggedIn map[string]bool) (implicit map[string]RouteTarget, warnings []string) {
	// model → sorted list of logged-in providers that serve it
	claims := map[string][]string{}
	for name, prov := range cfg.Providers {
		if !loggedIn[name] {
			continue
		}
		for _, m := range prov.Models {
			claims[m] = append(claims[m], name)
		}
	}
	implicit = map[string]RouteTarget{}
	for model, provs := range claims {
		if _, explicit := cfg.Routes[model]; explicit {
			continue // explicit route wins
		}
		sort.Strings(provs)
		implicit[model] = RouteTarget{Provider: provs[0], Model: model, Priority: 1}
		if len(provs) > 1 {
			warnings = append(warnings, fmt.Sprintf("model %q served by %d logged-in providers (%s); auto-routing to %s — add an explicit route to choose",
				model, len(provs), strings.Join(provs, ", "), provs[0]))
		}
	}
	return implicit, warnings
}

// synthesizeImplicitRoutes derives login status then delegates to the pure core.
func synthesizeImplicitRoutes(cfg *Config) (map[string]RouteTarget, []string) {
	return synthesizeImplicitRoutesFrom(cfg, loggedInProviders(cfg))
}

func (p *Proxy) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health/status" || r.URL.Path == "/health" {
		w.WriteHeader(200)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		p.serveModels(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/debug/schedule" {
		w.Header().Set("content-type", "application/json")
		w.Write(p.scheduleStatus())
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/events" {
		p.serveEvents(w, r)
		return
	}
	proto := protocolForPath(r.URL.Path)
	if proto == "" {
		http.Error(w, fmt.Sprintf("no route for path %s", r.URL.Path), http.StatusBadGateway)
		return
	}
	p.forward(proto, w, r)
}

// scheduleStatus builds a read-only JSON snapshot of what each route would
// schedule right now: the first-choice provider, the full ordered list (with
// tier/surplus/availability/peak per provider), and the current sticky selection
// (+ dwell remaining). Used by the /debug/schedule endpoint. It does NOT mutate
// sticky — it peeks via decideOrder.
//
// Credential pools are surfaced (Task 9 observability): each virtual in `ordered`
// carries its `pool_parent`, and each route whose targets share a pool carries a
// `pools` summary (parent + total accounts + how many are currently available).
// Existing fields are unchanged — consumers that don't read the new fields see
// the same shape as before.
func (p *Proxy) scheduleStatus() []byte {
	now := time.Now()
	p.mu.RLock()
	cfg := p.cfg
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	poolIndex := p.poolIndex
	p.mu.RUnlock()
	var qs map[string]*provider.QuotaSnapshot
	if p.quota != nil {
		qs = p.quota.allSnapshots()
	}

	// Snapshot health + sticky once (per-provider info + sticky display).
	p.healthMu.Lock()
	healthCopy := make(map[string]providerHealth, len(p.health))
	for k, v := range p.health {
		healthCopy[k] = *v
	}
	stickyCopy := make(map[string]routeSticky, len(p.sticky))
	for k, v := range p.sticky {
		stickyCopy[k] = v
	}
	pinsCopy := make(map[string]pinEntry, len(p.pins))
	for k, v := range p.pins {
		pinsCopy[k] = v
	}
	p.healthMu.Unlock()

	avail := func(name string) bool {
		h := healthCopy[name]
		return h.available(now)
	}
	surplusOf := func(name string) float64 { return computeSurplus(cfg, parentOf, qs, name, now) }

	type provInfo struct {
		Provider   string  `json:"provider"`
		PoolParent string  `json:"pool_parent,omitempty"`
		Priority   int     `json:"priority"`
		Tier       string  `json:"tier"`
		Surplus    float64 `json:"surplus"`
		Available  bool    `json:"available"`
		Peak       bool    `json:"peak"`
	}
	type poolInfo struct {
		Parent    string `json:"parent"`
		Accounts  int    `json:"accounts"`
		Available int    `json:"available"`
	}
	type routeInfo struct {
		First      string     `json:"first"`
		Ordered    []provInfo `json:"ordered"`
		Sticky     string     `json:"sticky,omitempty"`
		DwellRem   float64    `json:"sticky_dwell_remaining_sec,omitempty"`
		Pools      []poolInfo `json:"pools,omitempty"`
		Pin        string     `json:"pin,omitempty"`
		PinExpires string     `json:"pin_expires,omitempty"`
	}

	models := map[string]routeInfo{}
	routeKeys := make(map[string]bool, len(expanded))
	for k := range expanded {
		routeKeys[k] = true
	}
	for exposed, targets := range expanded {
		// commit=false: scheduleStatus is a read-only peek — it must NOT bump the
		// round-robin counter, set sticky, or evict sticky entries. decideOrder
		// gates all sticky mutation on commit, so the peek is side-effect-free.
		ordered, _ := p.decideOrder(cfg, parentOf, exposed, "", targets, now, false, routeKeys)
		ri := routeInfo{}
		if len(ordered) > 0 {
			ri.First = ordered[0].Provider
		}
		// Surface an active manual pin (hot-switch) so /debug/schedule shows WHY a
		// route is narrowed to one provider, plus its expiry. The pin's effect on
		// `ordered` is already applied inside decideOrder; this just labels it.
		if pe, ok := pinsCopy[exposed]; ok && pe.active(now) {
			ri.Pin = pe.provider
			ri.PinExpires = pe.expiresLabel(now)
		}
		// Track which parents appear in `ordered` so the route-level `pools`
		// summary can be emitted. A parent may have more accounts in poolIndex
		// than are currently in `ordered` (some unavailable) — Accounts uses
		// poolIndex (total), Available counts only those in `ordered`.
		parentSeen := map[string]bool{}
		for _, t := range ordered {
			pconf, _ := providerConfig(cfg, parentOf, t.Provider)
			parent := parentOf[t.Provider]
			if parent != "" {
				parentSeen[parent] = true
			}
			ri.Ordered = append(ri.Ordered, provInfo{
				Provider:   t.Provider,
				PoolParent: parent,
				Priority:   t.Priority,
				Tier:       billingClassName(p.billingClass(cfg, parentOf, t.Provider, qs)),
				Surplus:    surplusOf(t.Provider),
				Available:  avail(t.Provider),
				Peak:       pconf.peakMultiplier(now) > 1,
			})
		}
		if len(parentSeen) > 0 {
			parents := make([]string, 0, len(parentSeen))
			for pp := range parentSeen {
				parents = append(parents, pp)
			}
			sort.Strings(parents)
			for _, parent := range parents {
				availCount := 0
				for _, t := range ordered {
					if parentOf[t.Provider] == parent && avail(t.Provider) {
						availCount++
					}
				}
				ri.Pools = append(ri.Pools, poolInfo{
					Parent:    parent,
					Accounts:  len(poolIndex[parent]),
					Available: availCount,
				})
			}
		}
		if cur := stickyCopy[exposed]; cur.provider != "" {
			ri.Sticky = cur.provider
			if rem := cfg.Scheduling.dwell() - now.Sub(cur.since); rem > 0 && rem < cfg.Scheduling.dwell() {
				ri.DwellRem = rem.Seconds()
			}
		}
		models[exposed] = ri
	}
	out, _ := json.Marshal(map[string]any{"models": models})
	return out
}

// billingClassName renders a BillingClass for the /debug/schedule + doctor output.
func billingClassName(b provider.BillingClass) string {
	switch b {
	case provider.BillingPlan:
		return "plan"
	case provider.BillingPayG:
		return "pay-as-you-go"
	default:
		return "unknown"
	}
}

// serveModels lists all exposed models (from routes) merged with provider
// metadata (context/output from providers[].models).
func (p *Proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	data := p.exposedModelsJSON()
	writeModels(w, data)
}

// exposedModelsJSON builds an OpenAI-style model list from all exposed model
// names (routes' keys) plus the claude_mapping keys (so anthropic clients can
// discover claude-* aliases too).
func (p *Proxy) exposedModelsJSON() []byte {
	p.mu.RLock()
	cfg := p.cfg
	implicit := p.implicitRoutes
	p.mu.RUnlock()
	type m struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var models []m
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, m{ID: id, Object: "model"})
	}
	for exposed := range cfg.Routes {
		add(exposed)
	}
	for exposed := range implicit { // implicitly-routable models are callable → listable
		add(exposed)
	}
	for claude := range cfg.ClaudeMapping {
		add(claude)
	}
	out, _ := json.Marshal(map[string]any{"object": "list", "data": models})
	return out
}

func writeModels(w http.ResponseWriter, data []byte) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	if len(data) > 0 {
		w.Write(data)
		return
	}
	w.Write([]byte(`{"object":"list","data":[]}`))
}

// publishTerminalEvent emits a live "end" event for a request that ends before
// the normal start/commit flow — a malformed body (400) or an unrouted model
// (502). Without it, an agent retry-looping on a missing/removed model is
// invisible to the live monitor, defeating the feature's core use case.
func (p *Proxy) publishTerminalEvent(r *http.Request, proto, exposed string, status int) {
	p.events.publish(liveEvent{
		Type:     "end",
		Ts:       time.Now().UnixMilli(),
		Agent:    detectAgent(r),
		Protocol: proto,
		Exposed:  exposed,
		Status:   status,
	})
}

// forward proxies a request to the upstream selected by the route for the
// requested model. Routing is two-step: for anthropic, the called model name is
// first translated via claude_mapping (if the called name is mapped); openai
// uses the called name directly. The (translated) name is then looked up in
// routes, which maps it to an ordered list of provider/model targets. The proxy
// schedules the route's sticky provider first (within its dwell window), else the
// best available by (non-peak, priority); providers with an open circuit or active
// rate-limit are skipped. It fails over to the next on connection error /
// 401-after-refresh / 5xx / 429. The protocol (from the request path) selects the
// upstream path and base URL (anthropic_base_url vs openai_base_url); it does not
// key the route.
func (p *Proxy) forward(proto string, w http.ResponseWriter, r *http.Request) {
	// Snapshot cfg + providers under a brief RLock, then release. The lock is NOT
	// held during forwarding (which streams for minutes on SSE) — otherwise hot
	// reload (Proxy.reload takes mu.Lock) blocks until all streams finish.
	p.mu.RLock()
	cfg := p.cfg
	provs := p.providers
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	p.mu.RUnlock()

	origBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	calledModel := extractModel(origBody)
	if calledModel == "" {
		p.publishTerminalEvent(r, proto, calledModel, http.StatusBadRequest)
		http.Error(w, `missing or unparseable "model" field in request body`, http.StatusBadRequest)
		return
	}

	// Two-step lookup: anthropic translates claude-* names via claude_mapping
	// (if the called name is mapped); openai uses the called name as-is. Computed
	// early so the pin check (and cache bypass) can run before any upstream work.
	exposed := calledModel
	if proto == "anthropic" && cfg.ClaudeMapping != nil {
		if mapped, ok := cfg.ClaudeMapping[calledModel]; ok && mapped != "" {
			exposed = mapped
		}
	}
	targets, ok := expanded[exposed]
	if !ok || len(targets) == 0 {
		p.publishTerminalEvent(r, proto, exposed, http.StatusBadGateway)
		http.Error(w, fmt.Sprintf("model %q not found in routes", exposed), http.StatusBadGateway)
		return
	}
	// A pin on this route forces the pinned provider (exclusive) — compute early
	// so the cache can bypass it (a pinned request must reach the pinned backend,
	// not a stale cached answer from another provider — same rationale as the
	// force-provider/replay bypass).
	force := p.pinForces(exposed, targets, parentOf)

	// Exact-match response cache (#10): a request byte-identical to a recently
	// served one is replayed from cache with no upstream call. Computed before
	// routing (the key is the raw request), threaded into tryTarget to store on
	// a fresh 2xx commit. SKIPPED entirely when a force-provider override OR a pin
	// is in effect — both mean "send to THIS backend", not a stale cached answer.
	var cacheKey string
	if p.cache != nil && forceProvider(r) == "" && !force {
		cacheKey = cacheKeyOf(r.Method, r.URL.Path, origBody)
		if e, ok := p.cache.get(cacheKey, time.Now()); ok {
			// Live monitor (#6): a cache hit skips the normal start/end flow, so
			// emit an end event explicitly — otherwise the live view is blind to
			// these (e.g. a retry-looping agent served from cache stays invisible).
			p.events.publish(liveEvent{
				Type:     "end",
				Ts:       time.Now().UnixMilli(),
				Agent:    detectAgent(r),
				Protocol: proto,
				Exposed:  calledModel,
				Provider: "(cache)",
				Status:   e.status,
			})
			w.Header().Set("x-mp-cache", "hit")
			replayCached(w, e)
			return
		}
	}

	// One-shot force-provider override (x-mp-force-provider header / force_provider
	// query): narrows this single request's targets to one provider (matches the
	// parent name for pools). Used by `model-proxy replay` to re-answer with a
	// chosen backend without a global pin. No effect when unset or unmatched.
	if fp := forceProvider(r); fp != "" {
		if narrowed := filterTargetsByProvider(targets, parentOf, fp); len(narrowed) > 0 {
			targets = narrowed
		}
	}

	// Snapshot the models.dev catalog for request-aware routing (#8/#9 unified):
	// a target must support the request's capability (image) and fit its context
	// window; if none in the route fit, fall back cross-route by scheduling policy.
	cat := p.catalogSnapshot()

	// For openai protocol, strip the client's /v1 prefix (provider openai_base_url
	// includes its own version segment, e.g. .../v3, .../paas/v4).
	// For anthropic, keep /v1 — the official anthropic_base_url does NOT include
	// /v1 (the Anthropic SDK appends it: base + /v1/messages), so we pass it through.
	upPath := r.URL.Path
	if proto != "anthropic" && strings.HasPrefix(upPath, "/v1/") {
		upPath = strings.TrimPrefix(upPath, "/v1")
	}

	sessionKey := r.Header.Get("x-claude-code-session-id")
	// routeKeys = all callable route names (explicit ∪ implicit) — used by
	// decideOrder to tell route-name sticky keys (preserve) from session-id keys
	// (evict after dwell). Built from the expanded map so implicit routes count.
	routeKeys := make(map[string]bool, len(expanded))
	for k := range expanded {
		routeKeys[k] = true
	}
	ordered := p.schedule(cfg, parentOf, exposed, sessionKey, targets, routeKeys)
	// `force` (pin) was computed before the cache. A pin is EXCLUSIVE: it
	// overrides request-aware routing (no cross-route reroute away from the pinned
	// provider) and, via the `force` flag into tryTarget, bypasses the circuit
	// breaker — the user explicitly asked for THIS backend, no failover.
	if !force {
		// Request-aware routing (#8 capability + #9 context, unified): keep targets
		// that fit the request (image capability + context window); if none in the
		// route fit, fall back to a cross-route capable+fitting pool ranked by the
		// normal scheduling policy. No-op when everything already fits.
		ordered = p.applyRequestAwareRouting(cfg, parentOf, cat, exposed, sessionKey, ordered, expanded, routeKeys, origBody)
	}
	if p.scheduleHook != nil {
		p.scheduleHook(sessionKey)
	}

	// requestID groups this client request's failover attempts in the per-request
	// access log + live events. Always generated (cheap: one atomic add) so live
	// start↔end pairing works even when request logging is off.
	requestID := nextRequestID()

	// Detect the calling agent once (from the UA / known headers); attributed to
	// whichever target commits, in the parallel agent-stats pipeline.
	agent := detectAgent(r)

	// Live request monitor (#6): announce the in-flight request so the Web UI's
	// live view sees who is sending + where it routed, before the response lands.
	p.events.publish(liveEvent{
		Type:      "start",
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     agent,
		Protocol:  proto,
		Exposed:   exposed,
	})

	for ti, t := range ordered {
		// Resolve the provider CONFIG. For a pooled virtual ("name#<id>") the
		// config lives under the parent name in cfg.Providers; providerConfig
		// resolves it via parentOf. The provider IMPLEMENTATION (provImpl) is
		// keyed by the virtual id in provs.
		prov, ok := providerConfig(cfg, parentOf, t.Provider)
		if !ok {
			log.Printf("[proto=%s model=%s] target %d: unknown provider %q, skipping", proto, exposed, ti, t.Provider)
			continue
		}
		provImpl := provs[t.Provider]

		// Backend protocol (#11 conversion): the target's declared protocol, else
		// the client's. When they differ, convert the request body + route to the
		// backend's protocol; tryTarget converts the response back.
		backendProto := t.Protocol
		if backendProto == "" {
			backendProto = proto
		}
		convert := needsConversion(proto, backendProto)

		// Rewrite the body's model to this target's real model (per target), then
		// convert the request to the backend protocol if needed.
		body := origBody
		if t.Model != calledModel {
			body = rewriteModel(origBody, t.Model)
		}
		if convert {
			if cb, err := convertRequest(body, proto, backendProto); err == nil {
				body = cb
			} else {
				log.Printf("[convert] %s→%s request failed: %v", proto, backendProto, err)
			}
		}

		// Select the upstream base URL + path for the BACKEND protocol.
		baseURL := prov.OpenAIBaseURL
		if backendProto == "anthropic" && prov.AnthropicBaseURL != "" {
			baseURL = prov.AnthropicBaseURL
		}
		effPath := upPath
		if convert {
			effPath = backendPath(backendProto)
		}

		flc := forwardLogCtx{requestID: requestID, attempt: ti, exposed: exposed, origBody: origBody}
		if p.tryTarget(cfg, proto, backendProto, calledModel, t, prov, provImpl, baseURL, effPath, body, w, r, agent, cacheKey, force, flc) {
			return // committed: response written to the client
		}
		log.Printf("[proto=%s model=%s] target %d (%s/%s) failed; trying next", proto, exposed, ti, t.Provider, t.Model)
	}
	// Attribute the failed request to the calling agent so 502-only agents are
	// visible in the Agents view (not just agents whose requests succeed). The
	// first-tried target's provider/model is the attribution (the request WAS
	// directed there — it just failed).
	if p.agents != nil && agent != "" && len(ordered) > 0 {
		p.agents.incRequests(agent, ordered[0].Provider, ordered[0].Model)
	}
	// Live monitor (#6): every target failed → emit an end event so the live view
	// surfaces the 502 (otherwise a retry-looping agent that always 502s is
	// invisible — only starts, never ends).
	p.events.publish(liveEvent{
		Type:      "end",
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     agent,
		Protocol:  proto,
		Exposed:   exposed,
		Status:    http.StatusBadGateway,
	})
	http.Error(w, fmt.Sprintf("all targets failed for model %q", exposed), http.StatusBadGateway)
}

// tryTarget sends the request to one target, with a 401-refresh retry and an
// upstream timeout. It writes the response to w and returns true once committed
// (2xx or non-failover 4xx). Returns false to signal failover (connection error,
// timeout, 401 after refresh, 5xx, 429, or a build/auth error). It updates the
// provider's health on success/failure/rate-limit and enforces half-open
// single-flight. Failover only happens before any bytes are written to w.
func (p *Proxy) tryTarget(cfg *Config, proto, backendProto, calledModel string, t RouteTarget, prov Provider, provImpl provider.Provider, baseURL, upPath string, body []byte, w http.ResponseWriter, r *http.Request, agent, cacheKey string, force bool, flc forwardLogCtx) bool {
	// Wrap the client writer to capture time-to-first-token for latency stats.
	// All writes below go through tw; ttft is read on the commit path.
	tw := newTimingResponseWriter(w)
	w = tw
	sched := cfg.Scheduling
	// Re-check availability and reserve the half-open probe slot if needed. A pin
	// (force) bypasses the circuit breaker — the user explicitly asked for THIS
	// backend, so circuit-open state must not block it (and there's no failover
	// target anyway). recordSuccess on a forced hit reopens the circuit.
	if !force && !p.takeHalfOpenSlot(t.Provider) {
		return false
	}
	// recordSuccess/Failure/RateLimit below release the slot (force never took
	// one, so those releases are harmless no-ops).

	ctx, cancel := context.WithTimeout(r.Context(), sched.timeout())
	defer cancel()

	for attempt := 0; attempt < 2; attempt++ {
		targetURL := strings.TrimRight(baseURL, "/") + upPath
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}
		// Provider-specific URL/body tweaks (store:false, ?beta=true, ...).
		if provImpl != nil {
			targetURL, body = provImpl.RewriteRequest(targetURL, body, upPath)
		}

		req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			log.Printf("[proto=%s provider=%s] build upstream req: %v", proto, t.Provider, err)
			p.releaseHalfOpenSlot(t.Provider)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false
		}
		copyHeaderWhitelist(req.Header, r.Header,
			"content-type", "accept", "user-agent", "x-session-id",
			"user_id", "x-claude-code-session-id", "x-interaction-type", "x-interaction-id",
			"prompt_cache_key", "x-anthropic-billing-header", "anthropic-beta", "accept-language")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if provImpl != nil {
			if err := provImpl.AuthHeaders(req); err != nil {
				log.Printf("[proto=%s provider=%s] auth error: %v", proto, t.Provider, err)
				p.releaseHalfOpenSlot(t.Provider)
				if p.metrics != nil {
					p.metrics.inc(t.Provider, t.Model, evFailovers)
				}
				return false
			}
		}
		for k, v := range prov.Headers {
			req.Header.Set(k, v)
		}
		// Provider-specific per-request headers (aqp: anthropic-version +
		// x-compass-request-id). Same method the probe path calls - one impl,
		// no duplicated aqp branch.
		if provImpl != nil {
			provImpl.ExtraHeaders(req, upPath)
		}

		start := time.Now()
		resp, err := p.client.Do(req)
		upstreamMs := time.Since(start).Milliseconds() // upstream response time (headers received), NOT client-read-inclusive
		if err != nil {
			log.Printf("[proto=%s provider=%s] upstream error: %v", proto, t.Provider, err)
			p.recordFailure(t.Provider, sched) // connection error / timeout → circuit
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailures)
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false
		}

		// 401: refresh + retry once on the same target; still 401 → failure + failover.
		if resp.StatusCode == 401 {
			resp.Body.Close()
			if attempt == 0 && provImpl != nil {
				log.Printf("[proto=%s provider=%s] 401, refreshing auth", proto, t.Provider)
				if rerr := provImpl.Refresh(); rerr != nil {
					log.Printf("[proto=%s provider=%s] auth refresh failed: %v", proto, t.Provider, rerr)
				} else {
					continue
				}
			}
			p.recordFailure(t.Provider, sched)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false
		}
		// Rate limit (429): skip this provider until Retry-After / default backoff.
		// Does not count toward the circuit.
		if resp.StatusCode == 429 {
			until := p.parseRateLimit(resp, time.Now(), sched)
			resp.Body.Close()
			p.recordRateLimit(t.Provider, until)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evRateLimited429)
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false
		}
		// Transient upstream errors → circuit + failover.
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			p.recordFailure(t.Provider, sched)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailures)
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false
		}

		// Commit: stream this response (2xx or non-failover 4xx).
		// recordSuccess only for 2xx — 4xx (400/403/404) are client errors that
		// shouldn't reset the circuit breaker (a persistently-403 provider is broken).
		if resp.StatusCode < 300 {
			p.recordSuccess(t.Provider)
		}
		log.Printf("[proto=%s provider=%s] %s %s model=%s→%s status=%s %dms bytes=%d",
			proto, t.Provider, r.Method, r.URL.Path, calledModel, t.Model,
			statusColor(resp.StatusCode, fmt.Sprintf("%d", resp.StatusCode)),
			time.Since(start).Milliseconds(), len(body))
		// Conversion flag: when the backend protocol differs from the client's,
		// the response body is rewritten (#11), so its length changes — drop the
		// backend's content-length / transfer-encoding (can't forward a length for
		// a body we're about to transform; Go's server would reject the mismatch).
		convert := needsConversion(proto, backendProto)
		for k, vs := range resp.Header {
			if convert && (strings.EqualFold(k, "content-length") || strings.EqualFold(k, "transfer-encoding")) {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		// Wrap SSE 2xx responses in a usageScanner so observed usage events
		// (anthropic message_start/message_delta, openai usage) accrue to the
		// (provider, model) counter. Non-SSE responses pass through unscanned
		// (no overhead). Nil-guard like metrics for degenerate tests.
		//
		// Bind the (possibly wrapped) body to a variable and close THAT: on a
		// client disconnect mid-stream, flushCopy returns after a write error
		// without reaching EOF, so the scanner's Read-err commit path is never
		// hit. Closing the scanner explicitly fires its Close → commit, so
		// usage already observed (notably input_tokens from message_start,
		// which arrives at the START of the stream before any cancel) is not
		// silently dropped. On normal EOF the scanner's Read already set
		// done=true and committed, so Close is a harmless no-op (no double
		// count). Non-SSE: body == resp.Body, equivalent to before.
		//
		// When request logging is enabled, wrap resp.Body in a captureReader
		// (innermost) so the full response body is tee'd to a bounded buffer as
		// it streams to the client; on Close it enqueues a requestLogRecord.
		// The usageScanner wraps the captureReader (pass-through, so it sees the
		// same bytes); scanner.Close -> captureReader.Close -> enqueue, then
		// resp.Body.Close. The token path is unchanged (cumulative counters
		// only); request logging never touches it.
		reqBytes := body // request bytes sent upstream (post-rewrite); capture before body is shadowed
		logger := p.reqLog
		var endTokens tokenUsage // best-effort per-request usage for the live end event
		body := resp.Body
		// Protocol conversion (#11): translate the backend's response into the
		// client's protocol. INNERMOST wrap so the logger/scanner/cache below all
		// see client-protocol bytes. Streaming → stateful SSE transformer; non-
		// streaming → buffer + convert the JSON body (best-effort pass-through on
		// error so a conversion hiccup doesn't drop a successful upstream response).
		if convert {
			if isSSE(resp.Header) {
				body = io.NopCloser(convertSSEReader(body, proto, backendProto, t.Model))
			} else {
				// 64 MiB cap: a non-stream LLM response larger than this is
				// pathological (max_tokens bounds it); the cap prevents a
				// misbehaving backend from OOMing the proxy via the buffered
				// conversion path.
				all, err := io.ReadAll(io.LimitReader(body, 64<<20))
				body.Close()
				if err != nil {
					log.Printf("[convert] %s→%s read response failed: %v", backendProto, proto, err)
				}
				conv, cerr := convertResponse(all, proto, backendProto)
				if cerr != nil {
					log.Printf("[convert] %s→%s response failed: %v", backendProto, proto, cerr)
					conv = all
				}
				body = io.NopCloser(bytes.NewReader(conv))
			}
		}
		// request logging: tee the (possibly converted) body — `body`, NOT
		// resp.Body. Wrapping resp.Body here would log/replay the backend's native
		// bytes (e.g. openai SSE) instead of the client-protocol bytes the client
		// received, and for a converted non-stream response resp.Body is already
		// read+closed → an empty capture. `body` is exactly what flushCopy sends.
		if logger != nil {
			body = newCaptureReader(body, logger.maxBody, func(captured []byte, total int64, truncated bool) {
				logger.record(logger.buildRecord(recordInputs{
					flc:         flc,
					r:           r,
					proto:       proto,
					calledModel: calledModel,
					t:           t,
					resp:        resp,
					start:       start,
					requestBody: reqBytes,
					captured:    captured,
					total:       total,
					truncated:   truncated,
				}))
			})
		}
		if p.tokens != nil && isSSE(resp.Header) {
			// Attribute the same observed usage to the calling agent (parallel
			// agent pipeline) so per-agent token totals reconcile with per-model.
			// Also stash into endTokens for the live-monitor end event (best-effort:
			// non-SSE requests have no scanner → 0 tokens in the live event).
			var agentSink func(tokenUsage)
			ag, prov, mdl := agent, t.Provider, t.Model
			if p.agents != nil && agent != "" {
				agentSink = func(u tokenUsage) { p.agents.addTokens(ag, prov, mdl, u); endTokens = u }
			} else {
				agentSink = func(u tokenUsage) { endTokens = u }
			}
			body = newUsageScanner(body, tokenKey{Provider: t.Provider, Model: t.Model}, p.tokens, agentSink)
		}
		// Exact-match cache capture (#10): tee the streamed bytes into a bounded
		// buffer so a 2xx response can be cached for replay. Placed outermost
		// (pass-through wrappers below it don't alter bytes). Only when the cache
		// is on, this is a cacheable 2xx, and a key was computed in forward.
		var crec *cacheRecorder
		if p.cache != nil && cacheKey != "" && resp.StatusCode < 300 {
			crec = newCacheRecorder(body, p.cache.maxBody)
			body = crec
		}
		flushCopy(w, body)
		body.Close()
		// Record latency for this committed (served) target: total wall-clock from
		// upstream send to end of the streamed body, and TTFT from send to the
		// first byte written to the client (falls back to total when nothing was
		// written, e.g. an empty body).
		latencyMs := time.Since(start).Milliseconds()
		if p.metrics != nil {
			p.metrics.inc(t.Provider, t.Model, evRequests) // commit-only: failed attempts don't dilute latency avg
			ttftMs := latencyMs
			if tw.hasFirstByte {
				ttftMs = tw.firstByte.Sub(start).Milliseconds()
			}
			// Stats use UPSTREAM response time (not client-read-inclusive total)
			// so a slow client doesn't make a fast provider look slow.
			p.metrics.addLatency(t.Provider, t.Model, uint64(upstreamMs), uint64(ttftMs))
		}
		// Attribute this served request to the calling agent (parallel pipeline).
		if p.agents != nil && agent != "" {
			p.agents.incRequests(agent, t.Provider, t.Model)
		}
		// Store the exact-match cache entry for this 2xx response — only when the
		// capture is COMPLETE: not truncated by the size cap, AND sawEOF (the body
		// streamed to a clean end). A client disconnect mid-stream leaves sawEOF
		// false, so a half-read response is never cached as complete.
		if crec != nil && crec.sawEOF && !crec.truncated && len(crec.buf) > 0 {
			// For a converted response the cached body is the CLIENT-protocol body
			// (different length than the backend's), so the backend's
			// Content-Length / Transfer-Encoding must NOT be cached — replaying
			// them with the converted body would corrupt the response. Strip them
			// (same as the live forwarding path does); Go's server re-derives the
			// length on replay.
			hdr := resp.Header.Clone()
			if convert {
				hdr.Del("Content-Length")
				hdr.Del("Transfer-Encoding")
			}
			p.cache.put(cacheKey, &cacheEntry{
				status: resp.StatusCode,
				header: hdr,
				body:   crec.buf,
			}, time.Now())
		}
		// Live request monitor (#6): announce the completed request (agent,
		// route, chosen provider, status, latency, best-effort tokens).
		p.events.publish(liveEvent{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     flc.requestID,
			Agent:         agent,
			Protocol:      proto,
			Exposed:       flc.exposed,
			Provider:      t.Provider,
			UpstreamModel: t.Model,
			Status:        resp.StatusCode,
			LatencyMs:     latencyMs,
			Input:         endTokens.Input,
			Output:        endTokens.Output,
		})
		// Shadow evaluation (#12): for a route with a configured shadow backend,
		// also send the same prompt to the candidate provider (fire-and-forget,
		// best-effort) and log its result for offline comparison — the shadow
		// response is NEVER returned to the client. Requires request logging to
		// record the result; otherwise it's a no-op (nowhere to compare).
		if p.reqLog != nil && len(cfg.Shadow) > 0 {
			if sh, ok := cfg.Shadow[flc.exposed]; ok && sh.Provider != "" && sh.Provider != t.Provider {
				if p.shouldShadow() {
					select {
					case p.shadowSem <- struct{}{}:
						go func() {
							defer func() { <-p.shadowSem }()
							p.runShadow(proto, backendProto, calledModel, flc.exposed, sh, reqBytes)
						}()
					default:
						// shadow concurrency cap reached → skip (best-effort)
					}
				}
			}
		}
		return true
	}
	// 401-retry exhausted without resolution — release the slot.
	// Defensive guard: unreachable in normal flow (the 401 branch above always
	// returns or continues on attempt 0), kept for safety.
	p.releaseHalfOpenSlot(t.Provider)
	if p.metrics != nil {
		p.metrics.inc(t.Provider, t.Model, evFailovers)
	}
	return false
}

// shouldShadow reports whether this request should be shadow-evaluated, based on
// the configured sample rate (1.0 = all, 0.5 = half, 0 = none). Returns false if
// shadow is not configured.
func (p *Proxy) shouldShadow() bool {
	if p.shadowSem == nil {
		return false
	}
	if p.shadowSampRate >= 1 {
		return true
	}
	if p.shadowSampRate <= 0 {
		return false
	}
	return rand.Float64() < p.shadowSampRate
}

// runShadow sends the same prompt to a candidate backend (shadow evaluation,
// #12): fire-and-forget, the result is logged for offline comparison and NEVER
// returned to the client. It mirrors tryTarget's request-building (auth, extra
// headers, model rewrite) but is best-effort and bounded — any error is logged
// and dropped (shadow must never affect the live request). Snapshots cfg/providers
// from p so the goroutine is consistent with the commit that spawned it.
//
// `bodyProto` is the protocol of reqBody (the primary target's backend proto —
// reqBody may already be converted from the client's proto). The shadow backend's
// own protocol is shadow.Protocol (defaulting to bodyProto); runShadow selects the
// shadow base URL + path for THAT protocol and converts the body if it differs.
func (p *Proxy) runShadow(proto, bodyProto, calledModel, exposed string, shadow ShadowTarget, reqBody []byte) {
	p.mu.RLock()
	cfg := p.cfg
	provs := p.providers
	parentOf := p.parentOf
	p.mu.RUnlock()
	logger := p.reqLog
	if logger == nil {
		return // nowhere to record → no point shadowing
	}
	provCfg, ok := providerConfig(cfg, parentOf, shadow.Provider)
	if !ok {
		log.Printf("[shadow] %s: unknown provider", shadow.Provider)
		return
	}
	impl := provs[shadow.Provider]
	if impl == nil {
		log.Printf("[shadow] %s: provider not available", shadow.Provider)
		return
	}
	// Shadow backend protocol: declared, else same as the body's. Route + convert
	// accordingly so the shadow gets a request in the protocol IT speaks.
	shadowProto := shadow.Protocol
	if shadowProto == "" {
		shadowProto = bodyProto
	}
	sbody := reqBody
	if needsConversion(bodyProto, shadowProto) {
		if cb, err := convertRequest(reqBody, bodyProto, shadowProto); err == nil {
			sbody = cb
		} else {
			log.Printf("[shadow] %s: %s→%s convert failed: %v", shadow.Provider, bodyProto, shadowProto, err)
		}
	}
	baseURL := provCfg.OpenAIBaseURL
	if shadowProto == "anthropic" && provCfg.AnthropicBaseURL != "" {
		baseURL = provCfg.AnthropicBaseURL
	}
	upPath := backendPath(shadowProto)
	if shadow.Model != "" && shadow.Model != calledModel {
		sbody = rewriteModel(sbody, shadow.Model)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Scheduling.timeout())
	defer cancel()
	targetURL := strings.TrimRight(baseURL, "/") + upPath
	if impl != nil {
		targetURL, sbody = impl.RewriteRequest(targetURL, sbody, upPath)
	}
	sreq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(sbody))
	if err != nil {
		log.Printf("[shadow] %s: build req: %v", shadow.Provider, err)
		return
	}
	sreq.Header.Set("content-type", "application/json")
	if impl != nil {
		if err := impl.AuthHeaders(sreq); err != nil {
			log.Printf("[shadow] %s: auth: %v", shadow.Provider, err)
			return
		}
		impl.ExtraHeaders(sreq, upPath)
	}
	for k, v := range provCfg.Headers {
		sreq.Header.Set(k, v)
	}
	client := p.shadowClient // shared, not per-request
	start := time.Now()
	resp, err := client.Do(sreq)
	if err != nil {
		log.Printf("[shadow] %s/%s upstream error: %v", shadow.Provider, shadow.Model, err)
		return
	}
	defer resp.Body.Close()
	// Drain the shadow response into a bounded capture for the log. The reader
	// passes all bytes through (drained to Discard) while teeing a capped copy.
	cr := newCaptureReader(resp.Body, logger.maxBody, nil)
	io.Copy(io.Discard, cr)
	rec := logger.buildRecord(recordInputs{
		flc:         forwardLogCtx{requestID: "shadow-" + newRequestID(), attempt: 0, exposed: exposed},
		r:           sreq,
		proto:       proto,
		calledModel: calledModel,
		t:           RouteTarget{Provider: shadow.Provider, Model: shadow.Model},
		resp:        resp,
		start:       start,
		requestBody: sbody,
		captured:    cr.buf.Bytes(),
		total:       cr.total,
		truncated:   cr.truncated,
	})
	logger.record(rec)
}

// tierRank maps a BillingClass to the scheduling tier order: plan(0) < unknown(1) < payg(2).
// (BillingClass iota values are Unknown=0,Plan=1,PayG=2, which is NOT the scheduling order,
// so rank through this map instead of comparing the raw constants.)
func tierRank(b provider.BillingClass) int {
	switch b {
	case provider.BillingPlan:
		return 0
	case provider.BillingPayG:
		return 2
	default:
		return 1 // BillingUnknown or anything else
	}
}

// schedule returns targets in try-order using quota-aware ranking:
//
//	tier: plan < unknown < payg (pay-as-you-go is strict last-resort)
//	within tier: priority asc (config), then surplus desc (breaks priority ties)
//
// Sticky routing keeps the current provider for sticky_dwell (cache-friendly),
// then re-selects the best unless the best's only edge is a sub-margin surplus gain
// (priority beats surplus; surplus only matters at equal priority).
func (p *Proxy) schedule(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool) []RouteTarget {
	now := time.Now()
	ordered, stickyToSet := p.decideOrder(cfg, parentOf, exposed, sessionKey, targets, now, true, routeKeys)
	if stickyToSet != "" {
		// Commit sticky on the SESSION key (fallback to the exposed model for
		// non-session clients), so one conversation parks on one provider and
		// distinct conversations spread across the pool.
		sk := sessionKey
		if sk == "" {
			sk = exposed
		}
		p.healthMu.Lock()
		p.sticky[sk] = routeSticky{provider: stickyToSet, since: now}
		p.healthMu.Unlock()
	}
	return ordered
}

// setPin installs a manual route→provider pin (hot-switch). ttl <= 0 means no
// expiry (pin until clearPin). The provider must be a target of the route (after
// virtual/pool expansion) or setPin returns false — a pin to a provider the route
// can't reach would silently do nothing, so reject it up front with a clear error.
func (p *Proxy) setPin(route, provider string, ttl time.Duration) (pinEntry, bool) {
	now := time.Now()
	p.mu.RLock()
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	p.mu.RUnlock()
	targets, ok := expanded[route]
	if !ok {
		return pinEntry{}, false
	}
	matches := false
	for _, t := range targets {
		if t.Provider == provider || parentOf[t.Provider] == provider {
			matches = true
			break
		}
	}
	if !matches {
		return pinEntry{}, false
	}
	pe := pinEntry{provider: provider}
	if ttl > 0 {
		pe.expiresAt = now.Add(ttl)
	}
	p.healthMu.Lock()
	p.pins[route] = pe
	p.healthMu.Unlock()
	return pe, true
}

// clearPin removes a route's manual pin (no-op if none).
func (p *Proxy) clearPin(route string) bool {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	_, ok := p.pins[route]
	delete(p.pins, route)
	return ok
}

// listPins returns the active pins (provider + expiry), dropping expired ones.
func (p *Proxy) listPins() map[string]pinEntry {
	now := time.Now()
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	out := make(map[string]pinEntry, len(p.pins))
	for k, v := range p.pins {
		if v.active(now) {
			out[k] = v
		}
	}
	return out
}

// pinForces reports whether an active pin for `exposed` is in effect over the
// given ordered targets (i.e. decideOrder narrowed to the pinned provider). When
// true, forward treats the route as pinned-exclusive: request-aware routing is
// skipped (no reroute away from the pin) and tryTarget bypasses the circuit.
func (p *Proxy) pinForces(exposed string, ordered []RouteTarget, parentOf map[string]string) bool {
	p.healthMu.Lock()
	pe, ok := p.pins[exposed]
	p.healthMu.Unlock()
	if !ok || !pe.active(time.Now()) {
		return false
	}
	for _, t := range ordered {
		if t.Provider == pe.provider || parentOf[t.Provider] == pe.provider {
			return true
		}
	}
	return false
}

// decideOrder computes the try-order for targets and the provider to park sticky
// on ("" = leave the current sticky untouched), WITHOUT mutating p.sticky (the
// counter bump when commit=true is the one exception — it advances the per-parent
// round-robin, not sticky). schedule() commits the sticky on the SESSION key;
// scheduleStatus() (the /debug/schedule endpoint) calls this with commit=false for
// a read-only peek. parentOf resolves pooled virtual ids to their parent's config
// (billing/peak are parent-level, not per-account) AND drives per-parent
// round-robin assignment of new sessions.
func (p *Proxy) decideOrder(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, now time.Time, commit bool, routeKeys map[string]bool) (ordered []RouteTarget, stickyToSet string) {
	sched := cfg.Scheduling
	// (a) Re-key sticky on the session. Non-session clients (sessionKey=="")
	// fall back to the exposed model → identical to the pre-session path, so the
	// existing model-keyed sticky tests stay green.
	sk := sessionKey
	if sk == "" {
		sk = exposed
	}
	// Snapshot quota once (brief RLock), to avoid holding quotaMu during the sort
	// or while taking healthMu below.
	var qs map[string]*provider.QuotaSnapshot
	if p.quota != nil {
		qs = p.quota.allSnapshots()
	}

	p.healthMu.Lock()
	defer p.healthMu.Unlock()

	// (b) Evict expired SESSION entries to bound the sticky map. Per-session
	// keying adds one entry per distinct session id; without eviction a
	// long-running daemon would grow without bound between reloads. Only
	// session-id keys (not present in cfg.Routes) are evicted: model/route keys
	// are few (one per route) and MUST be preserved so the non-session path
	// stays byte-identical to pre-Task-6 (the after-dwell margin re-evaluation
	// reads them — evicting at dwell would silently delete the seeded entries
	// those tests rely on and re-pick on every call). Active sessions don't age
	// out: keepSticky refreshes their `since` each call (see below). Eviction is
	// commit-gated (schedule path only) so the /debug/schedule peek
	// (commit=false) never mutates sticky — a genuinely read-only snapshot.
	if commit {
		for k, v := range p.sticky {
			if routeKeys[k] {
				continue // route name (explicit OR implicit) — preserve
			}
			if now.Sub(v.since) > sched.dwell() {
				delete(p.sticky, k)
			}
		}
	}

	// Manual pin (model-proxy pin <route> <provider> --ttl): narrow to the pinned
	// provider's targets UP FRONT and force them past the circuit breaker. A pin
	// is an explicit "send to THIS backend, don't fail over" (debugging / A-B
	// compare) — it must NOT silently fail to another provider when the pinned one
	// is circuit-open. A pin whose provider isn't a route target is a no-op
	// (pt empty → targets unchanged); expired pins are ignored (lazy). The pin's
	// effect is visible in /debug/schedule via pinsCopy.
	pinned := false
	if pe, ok := p.pins[exposed]; ok && pe.active(now) {
		var pt []RouteTarget
		for _, t := range targets {
			if t.Provider == pe.provider || parentOf[t.Provider] == pe.provider {
				pt = append(pt, t)
			}
		}
		if len(pt) > 0 {
			targets = pt
			pinned = true
		}
	}

	avail := func(name string) bool {
		if pinned {
			return true // an active pin forces through circuit/rate-limit state
		}
		h := p.health[name]
		return h == nil || h.available(now)
	}

	var availTargets []RouteTarget
	for _, t := range targets {
		if avail(t.Provider) {
			availTargets = append(availTargets, t)
		}
	}

	billingOf := func(name string) provider.BillingClass { return p.billingClass(cfg, parentOf, name, qs) }
	surplusOf := func(name string) float64 { return computeSurplus(cfg, parentOf, qs, name, now) }

	sort.SliceStable(availTargets, func(i, j int) bool {
		ri, rj := tierRank(billingOf(availTargets[i].Provider)), tierRank(billingOf(availTargets[j].Provider))
		if ri != rj {
			return ri < rj // tier: plan < unknown < payg
		}
		if pi, pj := availTargets[i].Priority, availTargets[j].Priority; pi != pj {
			return pi < pj // priority (config) decides before surplus
		}
		return surplusOf(availTargets[i].Provider) > surplusOf(availTargets[j].Provider) // surplus only breaks priority ties
	})

	margin := sched.switchMargin()
	cur := p.sticky[sk]

	// Find cur's priority + whether it's still in the available set.
	curPrio := 0
	curInAvail := false
	for _, t := range availTargets {
		if t.Provider == cur.provider {
			curInAvail = true
			curPrio = t.Priority
			break
		}
	}

	keepSticky := false
	if cur.provider != "" && curInAvail {
		if now.Sub(cur.since) < sched.dwell() {
			keepSticky = true // within dwell: preserve cache
		} else if len(availTargets) == 0 {
			keepSticky = true
		} else {
			best := availTargets[0]
			if best.Provider == cur.provider {
				keepSticky = true // current is already the best
			} else {
				rb, rc := tierRank(billingOf(best.Provider)), tierRank(billingOf(cur.provider))
				switch {
				case rb < rc:
					keepSticky = false // best has a better billing tier
				// rb > rc is unreachable: best is availTargets[0] (sorted tier→priority→surplus),
				// and cur is in availTargets, so best can never rank worse than cur on tier.
				case best.Priority < curPrio:
					keepSticky = false // same tier; best has better priority → switch (priority beats surplus)
				// best.Priority > curPrio is unreachable for the same reason (best sorts first).
				case surplusOf(best.Provider)-surplusOf(cur.provider) >= margin:
					keepSticky = false // same tier + same priority; best ahead by surplus margin → switch
				default:
					keepSticky = true // same tier + same priority, sub-margin surplus → preserve cache
				}
			}
		}
	}

	if keepSticky {
		// leave p.sticky untouched (stickyToSet stays "" = keep current)
		for _, t := range availTargets {
			if t.Provider == cur.provider {
				ordered = append(ordered, t)
				break
			}
		}
		// For a SESSION key, refresh `since` (return the current provider as
		// stickyToSet so schedule re-writes the entry with now) — an active
		// conversation then never ages out past the eviction horizon and stays
		// on one account (cache-warm) for its whole lifetime. Model-keyed
		// clients (sk == exposed) skip this: leaving the entry untouched keeps
		// the pre-Task-6 after-dwell re-evaluate-every-call behavior, so the
		// existing model-keyed sticky tests stay byte-identical.
		if sk != exposed {
			stickyToSet = cur.provider
		}
	} else if len(availTargets) > 0 {
		// (c) Assign a fresh account. For a pooled route, round-robin over the
		// available band of the pool (id-sorted → deterministic) via the
		// per-parent counter so distinct sessions land on distinct accounts;
		// for a non-pooled route, keep the prior best-first (sorted) pick. The
		// counter advance is commit-gated so the read-only /debug/schedule peek
		// doesn't perturb assignment order for real traffic.
		pick := availTargets[0].Provider
		if parent, pooled := routePoolParent(parentOf, availTargets); pooled {
			band := poolBandByID(parentOf, availTargets, parent)
			// Modulo the uint64 counter BEFORE the int cast: on 32-bit the cast
			// of a counter past ~2³¹ would go negative and index band out of
			// range. Modulo-uint64 keeps start in [0, len(band)).
			start := int(p.spreadCtr[parent] % uint64(len(band)))
			if commit {
				p.spreadCtr[parent]++
			}
			pick = band[start].Provider
			// Move pick to the front of ordered (same move-to-front the
			// keepSticky branch uses for cur) so forward() tries it first.
			for _, t := range availTargets {
				if t.Provider == pick {
					ordered = append(ordered, t)
					break
				}
			}
		}
		stickyToSet = pick
	}
	for _, t := range availTargets {
		if len(ordered) > 0 && t.Provider == ordered[0].Provider {
			continue
		}
		ordered = append(ordered, t)
	}
	return ordered, stickyToSet
}

// routePoolParent reports whether any available target belongs to a credential
// pool, returning that pool's parent name. A route is pooled if at least one
// available target's provider is a virtual id present in parentOf. Used by
// decideOrder to decide whether to round-robin-assign a new session.
func routePoolParent(parentOf map[string]string, avail []RouteTarget) (string, bool) {
	for _, t := range avail {
		if parent, ok := parentOf[t.Provider]; ok {
			return parent, true
		}
	}
	return "", false
}

// poolBandByID returns the available virtuals of `parent`, sorted by virtual id
// (stable account-id order → deterministic round-robin across schedule calls and
// across restarts, since account ids derive from the keys, not insertion order).
func poolBandByID(parentOf map[string]string, avail []RouteTarget, parent string) []RouteTarget {
	var band []RouteTarget
	for _, t := range avail {
		if parentOf[t.Provider] == parent {
			band = append(band, t)
		}
	}
	sort.SliceStable(band, func(i, j int) bool { return band[i].Provider < band[j].Provider })
	return band
}

// providerConfig resolves the Provider config for name, resolving a
// credential-pool virtual id ("name#<accountID>") back to its parent. parentOf
// is the snapshot taken under p.mu alongside cfg; for a non-virtual name (incl.
// single-account providers), parentOf[name] is "" and the config is read
// directly. Returns the zero Provider (ok=false) if neither name nor a parent
// is found — callers treat that as an unknown provider.
func providerConfig(cfg *Config, parentOf map[string]string, name string) (Provider, bool) {
	if parent := parentOf[name]; parent != "" {
		name = parent
	}
	p, ok := cfg.Providers[name]
	return p, ok
}

// billingClass returns the effective scheduling tier, applying the staleness
// guard and the pay-as-you-go config override. A snapshot older than 3× the poll
// interval, or one carrying an error, is treated as Unknown. parentOf resolves
// virtual ids to their parent's billing config (pay-as-you-go / plan is set on
// the parent, not per-account).
func (p *Proxy) billingClass(cfg *Config, parentOf map[string]string, name string, qs map[string]*provider.QuotaSnapshot) provider.BillingClass {
	pconf, _ := providerConfig(cfg, parentOf, name)
	return classifyBilling(qs[name], pconf.Billing, cfg.Scheduling.pollInterval())
}

// computeSurplus is the shared scheduling pace-score for one provider, used by
// both scheduleStatus (the /debug/schedule peek) and decideOrder (the commit
// path). It resolves the provider's peak multiplier, fetches its quota snapshot,
// and delegates to QuotaSnapshot.Surplus. Returns 0 for an unknown/unmeasured
// provider (nil snapshot).
func computeSurplus(cfg *Config, parentOf map[string]string, qs map[string]*provider.QuotaSnapshot, name string, now time.Time) float64 {
	pconf, _ := providerConfig(cfg, parentOf, name)
	peakMult := pconf.peakMultiplier(now)
	if peakMult < 1 {
		peakMult = 1
	}
	snap := qs[name]
	if snap == nil {
		return 0
	}
	return snap.Surplus(now, peakMult)
}

// available reports whether a provider may be tried: not rate-limited, and
// circuit closed or half-open with no probe in flight.
func (h *providerHealth) available(now time.Time) bool {
	if now.Before(h.rateLimitedUntil) {
		return false
	}
	if !h.circuitOpenUntil.IsZero() && now.Before(h.circuitOpenUntil) {
		return false // circuit open
	}
	if !h.circuitOpenUntil.IsZero() && !now.Before(h.circuitOpenUntil) && h.halfOpenInFlight {
		return false // half-open, but a probe is already in flight
	}
	return true
}

// takeHalfOpenSlot re-checks availability and, for a half-open provider, reserves
// the single probe slot. Returns false if the provider should be skipped (circuit
// open, rate-limited, or a half-open probe is already in flight).
func (p *Proxy) takeHalfOpenSlot(name string) bool {
	now := time.Now()
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	h := p.health[name]
	if h == nil {
		return true // no failures recorded → available, no slot needed
	}
	if now.Before(h.rateLimitedUntil) {
		return false
	}
	if h.circuitOpenUntil.IsZero() {
		return true // circuit closed
	}
	if now.Before(h.circuitOpenUntil) {
		return false // circuit open
	}
	// Half-open (cooldown expired): allow one probe at a time.
	if h.halfOpenInFlight {
		return false
	}
	h.halfOpenInFlight = true
	return true
}

func (p *Proxy) releaseHalfOpenSlot(name string) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if h := p.health[name]; h != nil {
		h.halfOpenInFlight = false
	}
}

func (p *Proxy) recordSuccess(name string) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	h := p.health[name]
	if h == nil {
		return
	}
	h.consecutiveFailures = 0
	h.circuitOpenUntil = time.Time{}
	h.halfOpenInFlight = false
}

// recordFailure increments a provider's consecutive failures and opens the
// circuit (for cooldown) once the threshold is reached. Clears any half-open slot.
func (p *Proxy) recordFailure(name string, sched Scheduling) {
	now := time.Now()
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	h := p.health[name]
	if h == nil {
		h = &providerHealth{}
		p.health[name] = h
	}
	h.consecutiveFailures++
	h.halfOpenInFlight = false
	if h.consecutiveFailures >= sched.threshold() {
		h.circuitOpenUntil = now.Add(sched.cooldown())
	}
}

// recordRateLimit marks a provider rate-limited until `until` (extends if later)
// and clears any half-open slot. Does not count toward the circuit. It then
// triggers an async quota refresh of the provider so its snapshot is fresh when
// the rate-limit clears. healthMu is released BEFORE spawning refreshOne —
// refreshOne takes quotaMu internally and we never nest the two locks.
func (p *Proxy) recordRateLimit(name string, until time.Time) {
	p.healthMu.Lock()
	h := p.health[name]
	if h == nil {
		h = &providerHealth{}
		p.health[name] = h
	}
	h.halfOpenInFlight = false
	if until.After(h.rateLimitedUntil) {
		h.rateLimitedUntil = until
	}
	p.healthMu.Unlock()
	if p.quota != nil {
		go p.quota.refreshOne(name)
	}
}

// parseRateLimit derives the rate-limit-until time from a 429 response: the
// Retry-After header (seconds or HTTP-date), else the default backoff.
func (p *Proxy) parseRateLimit(resp *http.Response, now time.Time, sched Scheduling) time.Time {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			if secs < 0 {
				secs = 0
			}
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil {
			if t.Before(now) {
				return now
			}
			return t
		}
	}
	return now.Add(sched.rateBackoff())
}

func parseHHMMRange(s string) (start, end int, ok bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	s1, ok1 := parseHHMM(parts[0])
	s2, ok2 := parseHHMM(parts[1])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return s1, s2, true
}

func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// isSSE reports whether the response is an SSE stream (content-type
// text/event-stream). Only such responses are wrapped by the usage scanner —
// plain JSON / chunked-but-not-SSE bodies pay no scanner overhead.
func isSSE(h http.Header) bool {
	for _, ct := range h.Values("content-type") {
		if strings.Contains(ct, "text/event-stream") {
			return true
		}
	}
	return false
}

// flushCopy reads, writes, and flushes per chunk, supporting SSE streaming.
// Stops immediately if the client disconnects (write error), so the proxy
// doesn't keep pulling the upstream stream after the client is gone.
func flushCopy(w http.ResponseWriter, rc io.ReadCloser) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client disconnected — stop reading upstream.
				break
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

// timingResponseWriter wraps the client ResponseWriter to capture time-to-first-
// token: the instant of the first response body byte written to the client
// (the SSE first event, or the first byte of a buffered body). It delegates
// Write/WriteHeader/Header unchanged and implements http.Flusher so SSE
// flushing (flushCopy's w.(http.Flusher)) keeps working through the wrapper.
// firstByte is zero until the first Write; callers treat a zero value as "no
// byte was written" and fall back to the total latency as the TTFT.
type timingResponseWriter struct {
	http.ResponseWriter
	firstByte    time.Time
	hasFirstByte bool
	flusher      http.Flusher // nil if the underlying writer isn't a Flusher
}

func newTimingResponseWriter(w http.ResponseWriter) *timingResponseWriter {
	fl, _ := w.(http.Flusher)
	return &timingResponseWriter{ResponseWriter: w, flusher: fl}
}

func (t *timingResponseWriter) Write(p []byte) (int, error) {
	if !t.hasFirstByte {
		t.hasFirstByte = true
		t.firstByte = time.Now()
	}
	return t.ResponseWriter.Write(p)
}

// Flush delegates to the underlying writer's Flush so SSE chunk flushing through
// the wrapper is a no-op change (flushCopy asserts http.Flusher; without this
// the assertion would fail and SSE would not flush until the buffer filled).
func (t *timingResponseWriter) Flush() {
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

func copyHeaderWhitelist(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func extractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

func rewriteModel(body []byte, newModel string) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	v["model"] = newModel
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

func ensureJSONField(body []byte, key string, val any) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	if _, ok := v[key]; !ok {
		v[key] = val
		out, err := json.Marshal(v)
		if err != nil {
			return body
		}
		return out
	}
	return body
}
