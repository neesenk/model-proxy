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
		auth := newAuthProvider(prov.Provider, name, cfg)
		pcfg := &provider.Config{
			ProviderID:    prov.Provider,
			OpenAIBaseURL: prov.OpenAIBaseURL,
			Headers:       prov.Headers,
			UsageURL:      prov.UsageURL,
			Auth:          authAdapter{auth},
		}
		// Wire callbacks by provider type.
		switch prov.Provider {
		case "compass":
			pcfg.LoginFn = func() error { return runLogin(cfg) }
			pcfg.LogoutFn = func() error { return clearAccount(authFilePath("compass", "oauth_auth")) }
			pcfg.UsageFn = func() (any, error) { return showCompassUsageData(cfg) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCompassQuota(cfg) }
		case "codex":
			pcfg.LoginFn = func() error { return runCodexLogin(cfg) }
			pcfg.LogoutFn = func() error { return clearCodexAuth(cfg) }
			pcfg.UsageFn = func() (any, error) { return showCodexUsageData(cfg, prov) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCodexQuota(cfg, prov) }
		case "zhipu":
			pcfg.LoginFn = func() error { return runApiKeyLoginErr(cfg, name, prov) }
			pcfg.LogoutFn = func() error { return clearApiKey(name) }
			pcfg.UsageFn = func() (any, error) { return showZhipuUsageData(cfg, name, prov) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchZhipuQuota(cfg, name, prov) }
		case "deepseek":
			pcfg.LoginFn = func() error { return runApiKeyLoginErr(cfg, name, prov) }
			pcfg.LogoutFn = func() error { return clearApiKey(name) }
			pcfg.UsageFn = func() (any, error) { return showDeepseekUsageData(cfg, name, prov) }
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchDeepseekQuota(cfg, name, prov) }
		case "volcengine":
			pcfg.LoginFn = func() error { return runVolcengineLoginErr(cfg, name, prov) }
			pcfg.LogoutFn = func() error { return clearApiKey(name) }
			pcfg.UsageFn = func() (any, error) { return showVolcengineUsageData(cfg, name, prov) }
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
	return p
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
	proto := protocolForPath(r.URL.Path)
	if proto == "" {
		http.Error(w, fmt.Sprintf("no route for path %s", r.URL.Path), http.StatusBadGateway)
		return
	}
	p.forward(proto, w, r)
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
	for exposed := range p.cfg.Routes {
		add(exposed)
	}
	for claude := range p.cfg.ClaudeMapping {
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

	ordered := p.schedule(exposed, targets)

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

		if p.tryTarget(proto, calledModel, t, prov, provImpl, baseURL, upPath, body, w, r) {
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
func (p *Proxy) tryTarget(proto, calledModel string, t RouteTarget, prov Provider, provImpl provider.Provider, baseURL, upPath string, body []byte, w http.ResponseWriter, r *http.Request) bool {
	sched := p.cfg.Scheduling
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
		if prov.Provider == "compass" {
			req.Header.Set("anthropic-version", "2023-06-01")
			req.Header.Set("x-compass-request-id", newRequestID())
		}

		start := time.Now()
		resp, err := p.client.Do(req)
		if err != nil {
			log.Printf("[proto=%s provider=%s] upstream error: %v", proto, t.Provider, err)
			p.recordFailure(t.Provider) // connection error / timeout → circuit
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
			p.recordFailure(t.Provider)
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
			p.recordFailure(t.Provider)
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

// schedule returns targets in try-order: the route's sticky provider first if
// it's available and within its sticky_dwell window, then the remaining available
// targets by (non-peak, priority). Providers with an open circuit or active
// rate-limit are skipped. When the sticky provider is unavailable or its dwell
// has expired, the best available target becomes the new sticky (dwell resets).
func (p *Proxy) schedule(exposed string, targets []RouteTarget) []RouteTarget {
	now := time.Now()
	sched := p.cfg.Scheduling
	p.healthMu.Lock()
	defer p.healthMu.Unlock()

	avail := func(name string) bool {
		h := p.health[name]
		return h == nil || h.available(now)
	}

	// Available targets, sorted by (non-peak, priority), stable for ties.
	var availTargets []RouteTarget
	for _, t := range targets {
		if avail(t.Provider) {
			availTargets = append(availTargets, t)
		}
	}
	sort.SliceStable(availTargets, func(i, j int) bool {
		ei := availTargets[i].inPeak(p.cfg.Providers, now)
		ej := availTargets[j].inPeak(p.cfg.Providers, now)
		if ei != ej {
			return !ei // non-peak first
		}
		return availTargets[i].Priority < availTargets[j].Priority
	})

	cur := p.sticky[exposed]
	stickyActive := cur.provider != "" && avail(cur.provider) && now.Sub(cur.since) < sched.dwell()

	var ordered []RouteTarget
	if stickyActive {
		for _, t := range availTargets {
			if t.Provider == cur.provider {
				ordered = append(ordered, t)
				break
			}
		}
	} else if len(availTargets) > 0 {
		// Re-select: best available becomes the new sticky. Note: availTargets is
		// already filtered by availability (circuit/rate-limit) and sorted by
		// (non-peak, priority), so the sticky candidate is peak-aware.
		p.sticky[exposed] = routeSticky{provider: availTargets[0].Provider, since: now}
	}
	for _, t := range availTargets {
		if len(ordered) > 0 && t.Provider == ordered[0].Provider {
			continue
		}
		ordered = append(ordered, t)
	}
	return ordered
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
func (p *Proxy) recordFailure(name string) {
	now := time.Now()
	sched := p.cfg.Scheduling
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
// and clears any half-open slot. Does not count toward the circuit.
func (p *Proxy) recordRateLimit(name string, until time.Time) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	h := p.health[name]
	if h == nil {
		h = &providerHealth{}
		p.health[name] = h
	}
	h.halfOpenInFlight = false
	if until.After(h.rateLimitedUntil) {
		h.rateLimitedUntil = until
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

// inPeak reports whether this target's provider is currently in its peak_hours
// window (so the target is deprioritized to the peak group).
func (t RouteTarget) inPeak(providers map[string]Provider, now time.Time) bool {
	p, ok := providers[t.Provider]
	if !ok {
		return false
	}
	return p.inPeak(now)
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
