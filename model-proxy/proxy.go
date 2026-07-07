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
func buildProviders(cfg *Config) map[string]provider.Provider {
	m := map[string]provider.Provider{}
	for name, prov := range cfg.Providers {
		auth := newAuthProvider(prov.Provider, name, cfg, nil)
		pcfg := &provider.Config{
			ProviderID:    prov.Provider,
			OpenAIBaseURL: prov.OpenAIBaseURL,
			Headers:       prov.Headers,
			UsageURL:      prov.UsageURL,
			Auth:          authAdapter{auth},
		}
		// Wire callbacks by provider type.
		switch prov.Provider {
		case "aqp":
			pcfg.LoginFn = func() error { return runLogin(cfg) }
			pcfg.LogoutFn = func() error { return clearAccount(authFilePath("aqp", "oauth_auth")) }
			pcfg.UsageFn = func() (any, error) { return showAqpUsageData(cfg) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchAqpQuota(cfg) }
		case "codex":
			pcfg.LoginFn = func() error { return runCodexLogin(cfg) }
			pcfg.LogoutFn = func() error { return clearCodexAuth(cfg) }
			pcfg.UsageFn = func() (any, error) { return showCodexUsageData(cfg, prov) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCodexQuota(cfg, prov) }
		case "zhipu":
			pcfg.LoginFn = func() error { return runApiKeyLoginErr(cfg, name, prov) }
			pcfg.LogoutFn = func() error { return clearApiKey(name) }
			pcfg.UsageFn = func() (any, error) { return showZhipuUsageData(cfg, name, prov, nil) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchZhipuQuota(cfg, name, prov, nil) }
		case "deepseek":
			pcfg.LoginFn = func() error { return runApiKeyLoginErr(cfg, name, prov) }
			pcfg.LogoutFn = func() error { return clearApiKey(name) }
			pcfg.UsageFn = func() (any, error) { return showDeepseekUsageData(cfg, name, prov, nil) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchDeepseekQuota(cfg, name, prov, nil) }
		case "volcengine":
			pcfg.LoginFn = func() error { return runVolcengineLoginErr(cfg, name, prov) }
			pcfg.LogoutFn = func() error { return clearApiKey(name) }
			pcfg.UsageFn = func() (any, error) { return showVolcengineUsageData(cfg, name, prov, nil) }
			pcfg.FetchModelsFn = func() ([]string, error) { return listArkAgentPlanModelIDs(name) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchVolcengineQuota(name) }
		}
		p, err := provider.New(pcfg, name)
		if err != nil {
			log.Printf("[proxy] failed to build provider %s: %v (using auth-only)", name, err)
			continue
		}
		m[name] = p
	}
	return m
}

// authAdapter bridges main.AuthProvider → provider.Authenticator.
type authAdapter struct{ inner AuthProvider }

func (a authAdapter) Inject(req *http.Request) error { return a.inner.Inject(req) }
func (a authAdapter) Refresh() error                 { return a.inner.Refresh() }

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		cfg:       cfg,
		providers: buildProviders(cfg),
		client:    &http.Client{Timeout: 0},
		health:    map[string]*providerHealth{},
		sticky:    map[string]routeSticky{},
	}
	// The tracker reads cfg/providers asynchronously via the snapshot closures
	// (each takes p.mu.RLock), so reloads are picked up without recreating it.
	home, _ := os.UserHomeDir()
	qpath := filepath.Join(home, ".model-proxy", "quota_state.json")
	p.quota = newQuotaTracker(qpath,
		func() *Config { return p.cfgSnapshot() },
		func() map[string]provider.Provider { return p.providerSnapshot() })
	p.quota.stickySnapshot = p.snapshotSticky
	p.quota.start()
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

// providerSnapshot returns the current provider map under a brief read lock.
func (p *Proxy) providerSnapshot() map[string]provider.Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.providers
}

// snapshotSticky returns a copy of the per-route sticky map under healthMu, for
// persistence by the quota tracker (restored on boot — see NewProxy).
func (p *Proxy) snapshotSticky() map[string]routeSticky {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	out := make(map[string]routeSticky, len(p.sticky))
	for k, v := range p.sticky {
		out[k] = v
	}
	return out
}

func (p *Proxy) reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	newProviders := buildProviders(cfg)
	p.mu.Lock()
	p.cfg = cfg
	p.providers = newProviders
	p.mu.Unlock()
	// Reset health + sticky state — a reload is the operator's way to clear
	// stuck circuit-open / rate-limited / sticky-dwell state.
	p.healthMu.Lock()
	p.health = map[string]*providerHealth{}
	p.sticky = map[string]routeSticky{}
	p.healthMu.Unlock()
	// The tracker reads the new cfg/providers via its snapshot closures, so it
	// is NOT stopped/recreated on reload. Kick an immediate poll so newly added
	// providers show up at once (removed ones simply go stale and age out).
	if p.quota != nil {
		go p.quota.pollAll(time.Now())
	}
	return nil
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
func (p *Proxy) scheduleStatus() []byte {
	now := time.Now()
	p.mu.RLock()
	cfg := p.cfg
	provs := p.providers
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
		peakMult := cfg.Providers[name].peakMultiplier(now)
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
		Provider  string  `json:"provider"`
		Priority  int     `json:"priority"`
		Tier      string  `json:"tier"`
		Surplus   float64 `json:"surplus"`
		Available bool    `json:"available"`
		Peak      bool    `json:"peak"`
	}
	type routeInfo struct {
		First    string     `json:"first"`
		Ordered  []provInfo `json:"ordered"`
		Sticky   string     `json:"sticky,omitempty"`
		DwellRem float64    `json:"sticky_dwell_remaining_sec,omitempty"`
	}

	models := map[string]routeInfo{}
	for exposed, targets := range cfg.Routes {
		ordered, _ := p.decideOrder(cfg, provs, exposed, "", targets, now)
		ri := routeInfo{}
		if len(ordered) > 0 {
			ri.First = ordered[0].Provider
		}
		for _, t := range ordered {
			ri.Ordered = append(ri.Ordered, provInfo{
				Provider:  t.Provider,
				Priority:  t.Priority,
				Tier:      billingClassName(p.billingClass(cfg, t.Provider, qs)),
				Surplus:   surplusOf(t.Provider),
				Available: avail(t.Provider),
				Peak:      cfg.Providers[t.Provider].peakMultiplier(now) > 1,
			})
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
	targets, ok := cfg.Routes[exposed]
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
	ordered := p.schedule(cfg, provs, exposed, sessionKey, targets)
	if p.scheduleHook != nil {
		p.scheduleHook(sessionKey)
	}

	for ti, t := range ordered {
		prov, ok := cfg.Providers[t.Provider]
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
			return false
		}
		// Rate limit (429): skip this provider until Retry-After / default backoff.
		// Does not count toward the circuit.
		if resp.StatusCode == 429 {
			until := p.parseRateLimit(resp, time.Now(), sched)
			resp.Body.Close()
			p.recordRateLimit(t.Provider, until)
			return false
		}
		// Transient upstream errors → circuit + failover.
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			p.recordFailure(t.Provider, sched)
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
		flushCopy(w, resp.Body)
		resp.Body.Close()
		return true
	}
	// 401-retry exhausted without resolution — release the slot.
	p.releaseHalfOpenSlot(t.Provider)
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
func (p *Proxy) schedule(cfg *Config, provs map[string]provider.Provider, exposed, sessionKey string, targets []RouteTarget) []RouteTarget {
	now := time.Now()
	ordered, stickyToSet := p.decideOrder(cfg, provs, exposed, sessionKey, targets, now)
	if stickyToSet != "" {
		p.healthMu.Lock()
		p.sticky[exposed] = routeSticky{provider: stickyToSet, since: now}
		p.healthMu.Unlock()
	}
	return ordered
}

// decideOrder computes the try-order for targets and the provider to park sticky
// on ("" = leave the current sticky untouched), WITHOUT mutating p.sticky.
// schedule() commits the sticky; scheduleStatus() (the /debug/schedule endpoint)
// uses this for a read-only peek.
func (p *Proxy) decideOrder(cfg *Config, provs map[string]provider.Provider, exposed, sessionKey string, targets []RouteTarget, now time.Time) (ordered []RouteTarget, stickyToSet string) {
	_ = sessionKey // used in Task 6
	sched := cfg.Scheduling
	// Snapshot quota once (brief RLock), to avoid holding quotaMu during the sort
	// or while taking healthMu below.
	var qs map[string]*provider.QuotaSnapshot
	if p.quota != nil {
		qs = p.quota.allSnapshots()
	}

	p.healthMu.Lock()
	defer p.healthMu.Unlock()

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

	billingOf := func(name string) provider.BillingClass { return p.billingClass(cfg, name, qs) }
	surplusOf := func(name string) float64 {
		peakMult := cfg.Providers[name].peakMultiplier(now)
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
	cur := p.sticky[exposed]

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
	} else if len(availTargets) > 0 {
		stickyToSet = availTargets[0].Provider
	}
	for _, t := range availTargets {
		if len(ordered) > 0 && t.Provider == ordered[0].Provider {
			continue
		}
		ordered = append(ordered, t)
	}
	return ordered, stickyToSet
}

// billingClass returns the effective scheduling tier, applying the staleness
// guard and the pay-as-you-go config override. A snapshot older than 3× the poll
// interval, or one carrying an error, is treated as Unknown.
func (p *Proxy) billingClass(cfg *Config, name string, qs map[string]*provider.QuotaSnapshot) provider.BillingClass {
	return classifyBilling(qs[name], cfg.Providers[name].Billing, cfg.Scheduling.pollInterval())
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
