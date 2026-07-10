package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"model-proxy/provider"
)

// Proxy holds the compiled provider instances + the config.
type Proxy struct {
	mu        sync.RWMutex // guards cfg/providers across reload (held by handler for the request)
	healthMu  sync.Mutex   // guards health + sticky maps (runtime circuit/rate-limit/sticky state)
	cfg       *Config
	providers map[string]provider.Provider // provider name → Provider (shared)
	client    *http.Client
	health    map[string]*providerHealth // provider name → circuit/rate-limit state
	sticky    map[string]routeSticky     // exposed model → current provider + since
	quota     *quotaTracker              // background quota poller; nil only in degenerate tests
	metrics   *metricsStore              // request counters (atomic); nil only in degenerate tests
	tokens    *tokenCounter              // SSE-scanned token usage; nil only in degenerate tests

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

// buildProviders creates provider.Provider instances from config, wiring the
// main package's existing AuthProvider/Login/Logout/Usage functions as callbacks.
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
			// no plural pool → legacy singular fallback OR not logged in:
			// cred=nil keeps ApiKeyBase + pcfg.Auth file-backed (pre-pool path).
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

// credOrNil returns a pointer to c when it carries an API key, else nil. Used
// to thread an account credential through the auth + Usage/Quota closures: nil
// means "read from the auth file" (legacy single-account), non-nil means "bound
// to this in-memory key" (credential-pool virtual).
func credOrNil(c accountCred) *accountCred {
	if c.APIKey == "" && c.AccessKey == "" && c.SecretKey == "" {
		return nil
	}
	return &c
}

// buildOne constructs a single provider instance (a real provider for the
// single-account path, or a virtual for one credential-pool entry) bound to
// cred. When cred is non-empty the key is bound in THREE places, all required
// for a correct virtual:
//  1. Embedded ApiKeyBase (the FORWARD path) — via pcfg.BoundAPIKey; the
//     apikey constructors (zhipu/deepseek/volcengine) build a bound base whose
//     LoadKey/AuthHeaders use the in-memory key. This is the load-bearing
//     binding: deepseek/volcengine DEFINE their own AuthHeaders (dual Bearer +
//     x-api-key) sourcing from the embedded ApiKeyBase, NOT from cfg.Auth.
//  2. pcfg.Auth (the FetchModels path) — newAuthProvider with cred produces a
//     bound ApiKeyProvider so fetchModelsBearer (cfg.Auth.Inject) uses the key.
//  3. Usage/Quota closures — cred is passed through so per-account usage/quota
//     queries are scoped to this account.
//
// When cred is empty all three fall back to the legacy file-backed behavior
// (identical to the pre-pool buildProviders).
func buildOne(cfg *Config, name string, prov Provider, cred accountCred) provider.Provider {
	credPtr := credOrNil(cred)
	auth := newAuthProvider(prov.Provider, name, cfg, credPtr)
	pcfg := &provider.Config{
		ProviderID:    prov.Provider,
		OpenAIBaseURL: prov.OpenAIBaseURL,
		Headers:       prov.Headers,
		UsageURL:      prov.UsageURL,
		Auth:          authAdapter{auth},
		BoundAPIKey:   cred.APIKey, // binding point #1 (forward path)
	}
	// Wire callbacks by provider type.
	switch prov.Provider {
	case "aqp":
		pcfg.LoginFn = func() error { return runLogin(cfg) }
		pcfg.LogoutFn = func() error { return clearAccount(authFilePath("aqp", "oauth_auth")) }
		pcfg.UsageFn = func() (any, error) { return showAqpUsageData(cfg) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchAqpQuota(cfg) }
	case "codex":
		pcfg.ClientVersion = resolveCodexClientVersion(prov.ClientVersion, codexCLIVersion, codexCacheVersion)
		pcfg.LoginFn = func() error { return runCodexLogin(cfg) }
		pcfg.LogoutFn = func() error { return clearCodexAuth(cfg) }
		pcfg.UsageFn = func() (any, error) { return showCodexUsageData(cfg, prov) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCodexQuota(cfg, prov) }
	case "zhipu":
		pcfg.LoginFn = func() error { return runApiKeyLoginErr(cfg, name, prov) }
		pcfg.LogoutFn = func() error { return clearApiKey(name) }
		pcfg.UsageFn = func() (any, error) { return showZhipuUsageData(cfg, name, prov, credPtr) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchZhipuQuota(cfg, name, prov, credPtr) }
	case "deepseek":
		pcfg.LoginFn = func() error { return runApiKeyLoginErr(cfg, name, prov) }
		pcfg.LogoutFn = func() error { return clearApiKey(name) }
		pcfg.UsageFn = func() (any, error) { return showDeepseekUsageData(cfg, name, prov, credPtr) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchDeepseekQuota(cfg, name, prov, credPtr) }
	case "volcengine":
		pcfg.LoginFn = func() error { return runVolcengineLoginErr(cfg, name, prov) }
		pcfg.LogoutFn = func() error { return clearApiKey(name) }
		pcfg.UsageFn = func() (any, error) { return showVolcengineUsageData(cfg, name, prov, credPtr) }
		pcfg.FetchModelsFn = func() ([]string, error) { return listArkAgentPlanModelIDs(name) }
		// GetAFPUsage is V4-signed with the virtual's OWN AK/SK (bound via
		// credPtr) so each pooled account queries its own Agent Plan quota.
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchVolcengineQuota(name, credPtr) }
	}
	p, err := provider.New(pcfg, name)
	if err != nil {
		log.Printf("[proxy] failed to build provider %s: %v (using auth-only)", name, err)
		return nil
	}
	return p
}

// authAdapter bridges main.AuthProvider → provider.Authenticator.
type authAdapter struct{ inner AuthProvider }

func (a authAdapter) Inject(req *http.Request) error { return a.inner.Inject(req) }
func (a authAdapter) Refresh() error                 { return a.inner.Refresh() }

func NewProxy(cfg *Config) *Proxy {
	providers, poolIndex, parentOf := buildProviders(cfg)
	p := &Proxy{
		cfg:       cfg,
		providers: providers,
		client:    &http.Client{Timeout: 0},
		health:    map[string]*providerHealth{},
		sticky:    map[string]routeSticky{},
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
	// SSE token counter: load the persisted baseline so usage accrues across
	// restarts. A missing file is not an error (first run). Failure to load
	// only logs — the proxy still works, just without the baseline.
	p.tokens = newTokenCounter(tokenStatePath())
	if err := p.tokens.load(); err != nil {
		log.Printf("[tokens] load baseline failed: %v", err)
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

// cfgSnapshot returns the current config under a brief read lock. Used by the
// quota tracker (which reads cfg asynchronously from its poll goroutine).
func (p *Proxy) cfgSnapshot() *Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
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
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	out := make(map[string]routeSticky, len(p.sticky))
	for k, v := range p.sticky {
		if _, isRoute := routes[k]; !isRoute {
			continue // session-keyed — don't persist
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
	p.mu.Lock()
	p.cfg = cfg
	p.providers = newProviders
	// Rebuild the pool index + expanded routes from the single buildProviders
	// pass. Doing this under the write lock means request readers (which take
	// the read lock) see a consistent cfg/providers/poolIndex/expandedRoutes.
	p.poolIndex = newPoolIndex
	p.parentOf = newParentOf
	p.implicitRoutes, p.routeWarnings = synthesizeImplicitRoutes(cfg)
	p.expandedRoutes = p.buildExpandedRoutes()
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
	provs := p.providers
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
	p.healthMu.Unlock()

	avail := func(name string) bool {
		h := healthCopy[name]
		return h.available(now)
	}
	surplusOf := func(name string) float64 {
		pconf, _ := providerConfig(cfg, parentOf, name)
		peakMult := pconf.peakMultiplier(now)
		if peakMult < 1 {
			peakMult = 1
		}
		snap := qs[name]
		if impl := provs[name]; impl != nil {
			return impl.Surplus(snap, now, peakMult)
		}
		if snap == nil {
			return 0
		}
		return snap.Surplus(now, peakMult)
	}

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
		First    string     `json:"first"`
		Ordered  []provInfo `json:"ordered"`
		Sticky   string     `json:"sticky,omitempty"`
		DwellRem float64    `json:"sticky_dwell_remaining_sec,omitempty"`
		Pools    []poolInfo `json:"pools,omitempty"`
	}

	models := map[string]routeInfo{}
	for exposed, targets := range expanded {
		// commit=false: scheduleStatus is a read-only peek — it must NOT bump the
		// round-robin counter, set sticky, or evict sticky entries. decideOrder
		// gates all sticky mutation on commit, so the peek is side-effect-free.
		ordered, _ := p.decideOrder(cfg, provs, parentOf, exposed, "", targets, now, false)
		ri := routeInfo{}
		if len(ordered) > 0 {
			ri.First = ordered[0].Provider
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
		http.Error(w, `missing or unparseable "model" field in request body`, http.StatusBadRequest)
		return
	}

	// Two-step lookup: anthropic translates claude-* names via claude_mapping
	// (if the called name is mapped); openai uses the called name as-is.
	exposed := calledModel
	if proto == "anthropic" && cfg.ClaudeMapping != nil {
		if mapped, ok := cfg.ClaudeMapping[calledModel]; ok && mapped != "" {
			exposed = mapped
		}
	}
	targets, ok := expanded[exposed]
	if !ok || len(targets) == 0 {
		http.Error(w, fmt.Sprintf("model %q not found in routes", exposed), http.StatusBadGateway)
		return
	}

	// For openai protocol, strip the client's /v1 prefix (provider openai_base_url
	// includes its own version segment, e.g. .../v3, .../paas/v4).
	// For anthropic, keep /v1 — the official anthropic_base_url does NOT include
	// /v1 (the Anthropic SDK appends it: base + /v1/messages), so we pass it through.
	upPath := r.URL.Path
	if proto != "anthropic" && strings.HasPrefix(upPath, "/v1/") {
		upPath = strings.TrimPrefix(upPath, "/v1")
	}

	sessionKey := r.Header.Get("x-claude-code-session-id")
	ordered := p.schedule(cfg, provs, parentOf, exposed, sessionKey, targets)
	if p.scheduleHook != nil {
		p.scheduleHook(sessionKey)
	}

	for ti, t := range ordered {
		if p.metrics != nil {
			p.metrics.inc(t.Provider, evRequests)
		}
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

		// Rewrite the body's model to this target's real model (per target).
		body := origBody
		if t.Model != calledModel {
			body = rewriteModel(origBody, t.Model)
		}

		// Select the upstream base URL for this provider + protocol.
		baseURL := prov.OpenAIBaseURL
		if proto == "anthropic" && prov.AnthropicBaseURL != "" {
			baseURL = prov.AnthropicBaseURL
		}

		if p.tryTarget(cfg, proto, calledModel, t, prov, provImpl, baseURL, upPath, body, w, r) {
			return // committed: response written to the client
		}
		log.Printf("[proto=%s model=%s] target %d (%s/%s) failed; trying next", proto, exposed, ti, t.Provider, t.Model)
	}
	http.Error(w, fmt.Sprintf("all targets failed for model %q", exposed), http.StatusBadGateway)
}

// tryTarget sends the request to one target, with a 401-refresh retry and an
// upstream timeout. It writes the response to w and returns true once committed
// (2xx or non-failover 4xx). Returns false to signal failover (connection error,
// timeout, 401 after refresh, 5xx, 429, or a build/auth error). It updates the
// provider's health on success/failure/rate-limit and enforces half-open
// single-flight. Failover only happens before any bytes are written to w.
func (p *Proxy) tryTarget(cfg *Config, proto, calledModel string, t RouteTarget, prov Provider, provImpl provider.Provider, baseURL, upPath string, body []byte, w http.ResponseWriter, r *http.Request) bool {
	sched := cfg.Scheduling
	// Re-check availability and reserve the half-open probe slot if needed.
	if !p.takeHalfOpenSlot(t.Provider) {
		return false
	}
	// recordSuccess/Failure/RateLimit below release the slot.

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
				p.metrics.inc(t.Provider, evFailovers)
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
					p.metrics.inc(t.Provider, evFailovers)
				}
				return false
			}
		}
		for k, v := range prov.Headers {
			req.Header.Set(k, v)
		}
		if prov.Provider == "aqp" {
			req.Header.Set("anthropic-version", "2023-06-01")
			req.Header.Set("x-compass-request-id", newRequestID())
		}

		start := time.Now()
		resp, err := p.client.Do(req)
		if err != nil {
			log.Printf("[proto=%s provider=%s] upstream error: %v", proto, t.Provider, err)
			p.recordFailure(t.Provider, sched) // connection error / timeout → circuit
			if p.metrics != nil {
				p.metrics.inc(t.Provider, evFailures)
				p.metrics.inc(t.Provider, evFailovers)
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
				p.metrics.inc(t.Provider, evFailovers)
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
				p.metrics.inc(t.Provider, evRateLimited429)
				p.metrics.inc(t.Provider, evFailovers)
			}
			return false
		}
		// Transient upstream errors → circuit + failover.
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			p.recordFailure(t.Provider, sched)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, evFailures)
				p.metrics.inc(t.Provider, evFailovers)
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
		for k, vs := range resp.Header {
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
		body := resp.Body
		if p.tokens != nil && isSSE(resp.Header) {
			body = newUsageScanner(resp.Body, tokenKey{Provider: t.Provider, Model: t.Model}, p.tokens)
		}
		flushCopy(w, body)
		body.Close()
		return true
	}
	// 401-retry exhausted without resolution — release the slot.
	// Defensive guard: unreachable in normal flow (the 401 branch above always
	// returns or continues on attempt 0), kept for safety.
	p.releaseHalfOpenSlot(t.Provider)
	if p.metrics != nil {
		p.metrics.inc(t.Provider, evFailovers)
	}
	return false
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
func (p *Proxy) schedule(cfg *Config, provs map[string]provider.Provider, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget) []RouteTarget {
	now := time.Now()
	ordered, stickyToSet := p.decideOrder(cfg, provs, parentOf, exposed, sessionKey, targets, now, true)
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

// decideOrder computes the try-order for targets and the provider to park sticky
// on ("" = leave the current sticky untouched), WITHOUT mutating p.sticky (the
// counter bump when commit=true is the one exception — it advances the per-parent
// round-robin, not sticky). schedule() commits the sticky on the SESSION key;
// scheduleStatus() (the /debug/schedule endpoint) calls this with commit=false for
// a read-only peek. parentOf resolves pooled virtual ids to their parent's config
// (billing/peak are parent-level, not per-account) AND drives per-parent
// round-robin assignment of new sessions.
func (p *Proxy) decideOrder(cfg *Config, provs map[string]provider.Provider, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, now time.Time, commit bool) (ordered []RouteTarget, stickyToSet string) {
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
			if _, isRoute := cfg.Routes[k]; isRoute {
				continue
			}
			if now.Sub(v.since) > sched.dwell() {
				delete(p.sticky, k)
			}
		}
	}

	avail := func(name string) bool {
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
	surplusOf := func(name string) float64 {
		pconf, _ := providerConfig(cfg, parentOf, name)
		peakMult := pconf.peakMultiplier(now)
		if peakMult < 1 {
			peakMult = 1
		}
		snap := qs[name]
		if impl := provs[name]; impl != nil {
			return impl.Surplus(snap, now, peakMult) // provider-owned (delegates to snap.Surplus)
		}
		if snap == nil {
			return 0
		}
		return snap.Surplus(now, peakMult)
	}

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

func copyHeaderWhitelist(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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
