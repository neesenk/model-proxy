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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/protocol"
	runtimestate "model-proxy/internal/runtime"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/transport/bodycapture"
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
	lifecycle        *proxyLifecycle
	mu               sync.RWMutex  // guards cfg/providers across reload (held by handler for the request)
	configGeneration atomic.Uint64 // incremented on every successful reload
	runtimeState     runtimestate.Manager
	cfg              *Config
	providers        map[string]provider.Provider // provider name → Provider (shared)
	client           *http.Client
	quota            *quotaTracker                 // background quota poller; nil only in degenerate tests
	metrics          *metricsStore                 // request counters (atomic); nil only in degenerate tests
	tokens           *tokenCounter                 // SSE-scanned token usage; nil only in degenerate tests
	agents           *agentCounter                 // per-agent (UA) request/token counters; nil only in degenerate tests
	stats            *observestats.Store           // SQLite persistence for per-minute buckets; nil in tests (runtime services open it)
	flusher          *statsFlusher                 // per-minute diff loop; nil in tests (runProxy starts it)
	reqLog           *requestlog.Logger            // per-request access log (full bodies); nil = disabled (default) or init failure
	reqLogStarted    bool                          // lifecycle owns loop/shutdown only when started by startRuntimeServices
	cache            *responsecache.Store          // exact-match response cache (prompt-hash + TTL); nil = disabled
	responsesState   *protocol.ResponsesStateStore // previous_response_id replay for Responses clients bridged to stateless backends
	events           *observeevents.Hub            // live request monitor fan-out hub (SSE /api/events); always non-nil
	fusionReg        *fusionRegistry               // fusion orchestration observability (recent runs + per-workflow aggregates + daily budget); survives reload like events
	catalog          *catalog.Catalog              // models.dev metadata (context window + modalities) for request-aware routing; nil = unavailable, degrade gracefully
	shadow           atomic.Pointer[shadowRuntime] // reload-swappable shadow dispatch state (sample rate, concurrency gate, client); see shadowRuntime
	pricingMu        sync.Mutex                    // guards pricing during refresh (thundering-herd guard on pricing.EnsureFresh)
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
	implicitRoutes map[string]RouteTarget   // exposed model → single target auto-derived from logged-in providers' model lists (for models not in cfg.Routes)
	routeWarnings  []string                 // ambiguity warnings for implicit routes (multi-provider); surfaced in `models` CLI + /api/status

	// Runtime wire capabilities have their own leaf Store. The Store never
	// calls back into Proxy while locked and survives reload generations.
	wireCaps  runtimewire.Store
	wireProbe bool
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
	proto := string(protocol.ForPath(r.URL.Path))
	// Generate the request id ONCE, here at the handler top, so EVERY downstream
	// path — including the unknown-path 502 below, which returns before forward —
	// can publish live events carrying a stable id (the contract: every start/end
	// carries a stable request_id; cache-hit and 400/502 terminals must produce an
	// end). Cheap: one atomic add, no data dependency.
	requestID := nextRequestID()
	if proto == "" {
		p.publishTerminalEvent(requestID, r, "", r.URL.Path, http.StatusBadGateway)
		http.Error(w, fmt.Sprintf("no route for path %s", r.URL.Path), http.StatusBadGateway)
		return
	}
	p.forward(proto, w, r, requestID)
}

func (*Proxy) responsesPreviousID(body []byte) string {
	return protocol.PreviousResponseID(body)
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
// invisible to the live monitor, defeating the feature's core use case. The
// requestID is generated at the handler top and threaded in so these terminal
// events still pair with a stable id (the contract: 400/502 终局也必须产生 end
// 且带稳定 request_id).
func (p *Proxy) publishTerminalEvent(requestID string, r *http.Request, proto, exposed string, status int) {
	p.events.Publish(observeevents.Event{
		Type:      "end",
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     detectAgent(r),
		Protocol:  proto,
		Exposed:   exposed,
		Status:    status,
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
// 401-after-refresh / 5xx / 429, and retries once on a strictly-larger-context
// target when the upstream answers a context-overflow 400 (see
// contextOverflowRetry). The protocol (from the request path) selects the
// upstream path and base URL (anthropic_base_url vs openai_base_url); it does not
// key the route.
func (p *Proxy) forward(proto string, w http.ResponseWriter, r *http.Request, requestID string) {
	// Capture all reload-owned dependencies once. The lock is NOT held during
	// forwarding (which streams for minutes on SSE); the immutable snapshot keeps
	// routing, providers, catalog, and cache on one config generation.
	runtime := p.snapshotRuntime()
	cfg := runtime.cfg
	expanded := runtime.expandedRoutes
	parentOf := runtime.parentOf
	cache := runtime.cache

	origBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	calledModel := extractModel(origBody)
	if calledModel == "" {
		p.publishTerminalEvent(requestID, r, proto, calledModel, http.StatusBadRequest)
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
		p.publishTerminalEvent(requestID, r, proto, exposed, http.StatusBadGateway)
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
	if cache != nil && forceProvider(r) == "" && !force {
		cacheKey = responsecache.Key(r, origBody)
		if e, ok := cache.Lookup(cacheKey, time.Now()); ok {
			// Live monitor (#6): a cache hit skips the normal start/end flow, so
			// emit an end event explicitly — otherwise the live view is blind to
			// these (e.g. a retry-looping agent served from cache stays invisible).
			p.events.Publish(observeevents.Event{
				Type:      "end",
				Ts:        time.Now().UnixMilli(),
				RequestID: requestID,
				Agent:     detectAgent(r),
				Protocol:  proto,
				Exposed:   calledModel,
				Provider:  "(cache)",
				Status:    e.Status(),
			})
			w.Header().Set("x-mp-cache", "hit")
			_ = responsecache.Replay(w, e)
			return
		}
	}

	// One-shot force-provider override (x-mp-force-provider header / force_provider
	// query): narrows this single request's targets to one provider (matches the
	// parent name for pools). Used by `model-proxy replay` to re-answer with a
	// chosen backend without a global pin. When set but the named provider is NOT
	// a target of this route (typo, wrong name), HARD-FAIL (400): falling back to
	// normal scheduling would let another provider answer while `replay --to`
	// still reports the typo'd name, silently polluting comparison conclusions.
	if fp := forceProvider(r); fp != "" {
		narrowed := filterTargetsByProvider(targets, parentOf, fp)
		if len(narrowed) == 0 {
			p.publishTerminalEvent(requestID, r, proto, exposed, http.StatusBadRequest)
			http.Error(w, fmt.Sprintf("force-provider %q is not a target for model %q", fp, exposed), http.StatusBadRequest)
			return
		}
		targets = narrowed
	}

	// Snapshot the models.dev catalog for request-aware routing (#8/#9 unified):
	// a target must support the request's capability (image) and fit its context
	// window; if none in the route fit, fall back cross-route by scheduling policy.
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
	// requestID groups this client request's failover attempts in the per-request
	// access log + live events. Generated once at the handler top (so early
	// terminal/cache-hit paths share it) and passed in here.
	// Detect the calling agent once (from the UA / known headers); attributed to
	// whichever target commits, in the parallel agent-stats pipeline.
	agent := detectAgent(r)

	// Live request monitor (#6): announce the in-flight request so the Web UI's
	// live view sees who is sending + where it routed, before the response lands.
	p.events.Publish(observeevents.Event{
		Type:      "start",
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     agent,
		Protocol:  proto,
		Exposed:   exposed,
	})

	// Cooldown-aware wait-retry (#6): when EVERY target is in a cooldown
	// (rate-limit / circuit) and the earliest expiry is within retry_wait, sleep
	// until it lapses and re-run the whole pass — a short silent wait beats an
	// immediate error the agent would just retry anyway (with a fresh round-trip
	// each time). Bounded: ≤2 retries, each wait ≤ retry_wait; a client
	// disconnect aborts the wait. Skipped for one-shot force-provider overrides
	// (replay wants the answer now).
	retryWait := cfg.Scheduling.RetryWaitDuration()
	var st serveState
	var sawHard, sawCool bool
	execution := serveRequest{
		runtime: runtime,

		proto:       proto,
		upPath:      upPath,
		exposed:     exposed,
		calledModel: calledModel,
		sessionKey:  sessionKey,
		agent:       agent,
		requestID:   requestID,

		targets:   targets,
		routeKeys: routeKeys,
		force:     force,
		cacheKey:  cacheKey,
		origBody:  origBody,

		writer:  w,
		request: r,
	}
	for round := 0; ; round++ {
		res := p.serveOnce(execution, &st)
		if res.committed {
			return
		}
		if res.conversionErr != nil && len(res.tried) == 0 {
			p.publishTerminalEvent(requestID, r, proto, exposed, http.StatusBadRequest)
			writeUnsupportedConversionError(w, protocol.Protocol(proto), res.conversionErr)
			return
		}
		sawHard = sawHard || res.sawHard
		sawCool = sawCool || res.sawCooldown
		// Key cooldown/TOCTOU on the EFFECTIVE targets serveOnce actually
		// considered (after scheduling/request-aware narrowing/context retry),
		// not the original route targets — otherwise a healthy-but-filtered
		// sibling (e.g. a target that doesn't fit the request's capability) makes
		// cooldownState think a servable target exists and skips the wait. Fall
		// back to the original targets when effective is empty (e.g. schedule
		// dropped every target while cooling, then they all recovered — claim 2).
		checkTargets := res.effectiveTargets
		if len(checkTargets) == 0 {
			checkTargets = targets
		}
		now := time.Now()
		allDown, allRateLimited, earliest := p.cooldownState(checkTargets, now)
		if forceProvider(r) == "" && retryWait > 0 && round < 2 {
			if allDown {
				if sleep := earliest.Sub(now); sleep > 0 && sleep <= retryWait {
					log.Printf("[proto=%s model=%s] all targets cooling down; retry %d/2 in %s", proto, exposed, round+1, sleep.Round(time.Millisecond))
					select {
					case <-time.After(sleep):
						continue
					case <-r.Context().Done():
						// Client gave up waiting — close the live event pair (499 =
						// client closed request) and write nothing.
						p.events.Publish(observeevents.Event{
							Type: "end", Ts: time.Now().UnixMilli(), RequestID: requestID,
							Agent: agent, Protocol: proto, Exposed: exposed, Status: 499,
						})
						return
					}
				}
			} else if p.hasRecoveredUntried(checkTargets, res.tried, now) {
				// TOCTOU (P0-5): a target recovered between scheduling and this
				// terminal check but was never tried in the failed pass (its
				// cooldown lapsed mid-pass while a sibling re-failed). Give it an
				// immediate, zero-wait pass — still inside the round budget —
				// instead of erroring out while a servable target exists.
				log.Printf("[proto=%s model=%s] a cooled-down target recovered; retrying immediately (round %d/2)", proto, exposed, round+1)
				continue
			}
		}
		// Terminal: every target failed. The status is honest about the CLASS of
		// failures seen ACROSS ALL PASSES (not a racy health re-read — a target
		// whose cooldown lapsed mid-request without a retry must not flip the
		// verdict): pure rate-limit → 429 + Retry-After (the upstreams' own
		// answer, per RFC 9110); any hard failure → 502. Attribute the failure
		// to the calling agent so failing-only agents stay visible (first-tried
		// target = where the request WAS directed).
		if p.agents != nil && agent != "" && res.firstTried.Provider != "" {
			p.agents.incRequests(agent, res.firstTried.Provider, res.firstTried.Model)
			p.agents.incFailure(agent, res.firstTried.Provider, res.firstTried.Model)
		}
		status := http.StatusBadGateway
		msg := fmt.Sprintf("all targets failed for model %q", exposed)
		if !sawHard && (sawCool || (allDown && allRateLimited)) {
			d := time.Until(earliest)
			if d <= 0 {
				d = cfg.Scheduling.RateBackoff() // horizon already lapsed: use the transient default
			}
			secs := int(d / time.Second)
			if d%time.Second != 0 {
				secs++
			}
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			status = http.StatusTooManyRequests
			msg = fmt.Sprintf("all providers for model %q are rate-limited; retry after %ds", exposed, secs)
		}
		// Live monitor (#6): every target failed → emit an end event so the live
		// view surfaces the failure (a retry-looping agent that always errors is
		// otherwise invisible — only starts, never ends).
		p.events.Publish(observeevents.Event{
			Type:      "end",
			Ts:        time.Now().UnixMilli(),
			RequestID: requestID,
			Agent:     agent,
			Protocol:  proto,
			Exposed:   exposed,
			Status:    status,
		})
		http.Error(w, msg, status)
		return
	}
}

// serveState carries the two per-request pieces of state that must survive a
// cooldown wait-retry round (serveOnce is otherwise re-entrant).
type serveState struct {
	retriedForContext bool // the larger-context retry is one-shot per request
	attempt           int  // monotonic tryTarget index for the request log (ti resets on a context retry)
}

// serveResult is the outcome of one serveOnce pass: where the request was
// first directed, who was actually tried, and the failure CLASS mix — the
// terminal status derives from these (pure cooldown → 429; any hard → 502),
// NOT from a racy health re-read at terminal time.
type serveResult struct {
	committed     bool
	firstTried    RouteTarget
	tried         map[string]bool            // providers actually attempted this pass
	sawHard       bool                       // conn/timeout/5xx/401/build/model-denied-class failure
	sawCooldown   bool                       // at least one 429 this pass
	conversionErr *protocol.UnsupportedError // first client feature no candidate conversion could safely represent
	// effectiveTargets is the target set serveOnce actually considered this pass
	// (after scheduling drops cooling targets, request-aware routing narrows, or a
	// context-overflow retry replaces it) — NOT necessarily the original route
	// targets. forward keys its cooldown/TOCTOU decisions on this so it waits for
	// / re-schedules the targets that were really in play, not a sibling that was
	// filtered out and can't serve. Empty when the pass committed (forward
	// returns immediately) or schedule produced nothing (forward falls back to
	// the original targets).
	effectiveTargets []RouteTarget
}

// serveOnce runs ONE full scheduling + failover pass: schedule → request-aware
// routing → try each target in order (fusion recipes intercepted). forward
// calls it in a wait-retry loop for all-cooldown situations.
func (p *Proxy) serveOnce(req serveRequest, st *serveState) serveResult {
	runtime := req.runtime
	cfg := runtime.cfg
	generation := runtime.generation
	parentOf := runtime.parentOf
	expanded := runtime.expandedRoutes
	cat := runtime.catalog
	proto := req.proto
	upPath := req.upPath
	exposed := req.exposed
	calledModel := req.calledModel
	sessionKey := req.sessionKey
	targets := req.targets
	routeKeys := req.routeKeys
	force := req.force
	cacheKey := req.cacheKey
	w := req.writer
	r := req.request
	agent := req.agent
	requestID := req.requestID
	origBody := req.origBody

	ordered := p.schedule(cfg, parentOf, exposed, sessionKey, targets, routeKeys, generation)
	// `force` (pin) was computed before the cache. A pin is EXCLUSIVE: it
	// overrides request-aware routing (no cross-route reroute away from the pinned
	// provider) and, via the `force` flag into tryTarget, bypasses the circuit
	// breaker — the user explicitly asked for THIS backend, no failover.
	if !force {
		// Request-aware routing (#8 capability + #9 context, unified): keep targets
		// that fit the request (image capability + context window); if none in the
		// route fit, fall back to a cross-route capable+fitting pool ranked by the
		// normal scheduling policy. No-op when everything already fits.
		ordered = p.applyRequestAwareRouting(cfg, parentOf, cat, exposed, sessionKey, ordered, expanded, routeKeys, origBody, generation)
	}
	var firstTried RouteTarget
	if len(ordered) > 0 {
		firstTried = ordered[0]
	}
	res := serveResult{firstTried: firstTried, tried: map[string]bool{}}
	for ti := 0; ti < len(ordered); ti++ {
		t := ordered[ti]
		// Fusion orchestration: {provider: fusion, model: <recipe>} is NOT a
		// provider — intercept before the providerConfig lookup and run the
		// panel→synthesis engine (its synthesizer leg reuses tryTarget). A
		// force-provider override (replay) targets one concrete backend, so it
		// skips fusion entirely.
		if t.Provider == "fusion" && forceProvider(r) == "" {
			recipe, ok := cfg.Fusion[t.Model]
			if !ok {
				log.Printf("[proto=%s model=%s] target %d: fusion recipe %q not defined, skipping", proto, exposed, ti, t.Model)
				continue
			}
			fc := fusionCtx{
				runtime: runtime,
				proto:   proto, calledModel: calledModel, upPath: upPath, agent: agent,
				sessionKey: sessionKey,
				origBody:   origBody,
				flc:        forwardLogCtx{requestID: requestID, attempt: st.attempt, exposed: exposed, origBody: origBody},
			}
			st.attempt++
			res.tried[t.Provider] = true
			if p.runFusion(fc, t.Model, recipe, w, r, cacheKey) {
				res.committed = true
				return res // committed: response written to the client
			}
			res.sawHard = true // a failed fusion run is opaque → treat as hard
			log.Printf("[proto=%s model=%s] target %d (fusion/%s) failed; trying next", proto, exposed, ti, t.Model)
			continue
		}
		plan, err := p.planTarget(targetPlanInput{
			runtime: runtime, target: t, clientProto: proto, clientPath: upPath,
		})
		if err != nil {
			log.Printf("[proto=%s model=%s] target %d: %v, skipping", proto, exposed, ti, err)
			continue
		}

		// Rewrite the body's model to this target's real model (per target), then
		// convert the request to the backend protocol if needed.
		body := plan.rewriteModel(origBody, calledModel)
		var responsesHistory []any
		if proto == "responses" && plan.backendProto != "responses" && p.responsesState != nil {
			expandedBody, history, hit, err := p.responsesState.Expand(body, sessionKey)
			if err != nil {
				log.Printf("[proto=%s model=%s] target %d (%s/%s) responses state expansion failed: %v — skipping",
					proto, exposed, ti, t.Provider, t.Model, err)
				continue
			}
			if protocol.PreviousResponseID(body) != "" && !hit {
				log.Printf("[proto=%s model=%s] previous_response_id cache miss; repaired orphaned continuation items", proto, exposed)
			}
			body = expandedBody
			responsesHistory = history
		}
		body, err = plan.convertBody(body)
		if err != nil {
			if unsupported, ok := protocol.AsUnsupported(err); ok {
				if res.conversionErr == nil {
					res.conversionErr = unsupported
				}
				log.Printf("[proto=%s model=%s] target %d (%s/%s) %s→%s unsupported feature %s — trying another target",
					proto, exposed, ti, t.Provider, t.Model, proto, plan.backendProto, unsupported.Feature)
				continue
			}
			// Fail CLOSED: a conversion failure must NOT send the unconverted
			// body to the backend (that ships an Anthropic body to an OpenAI
			// endpoint, or vice versa). Skip this target and try the next; if
			// none serve, the loop's all-targets-failed path returns a 502.
			log.Printf("[proto=%s model=%s] target %d (%s/%s) %s→%s request convert failed: %v — skipping",
				proto, exposed, ti, t.Provider, t.Model, proto, plan.backendProto, err)
			continue
		}

		flc := forwardLogCtx{requestID: requestID, attempt: st.attempt, exposed: exposed, origBody: origBody}
		st.attempt++
		// One-shot larger-context retry: when this target answers a
		// context-overflow 400, tryTarget calls ctxRetry for a strictly-larger-
		// context replacement list (cross-route pool, scheduled) instead of
		// committing. Nil — no peek, no retry — once the retry is spent, while a
		// pin is in force (exclusive: no cross-route reroute), or without a
		// catalog (same no-op degradation as applyRequestAwareRouting).
		var ctxRetry func() []RouteTarget
		if !st.retriedForContext && !force && cat != nil {
			// Only capture targets ACTUALLY tried so far (through the current
			// index), not the full ordered list — failover targets further down
			// haven't been attempted yet and shouldn't anchor the "strictly larger
			// context" threshold (they might be worth trying as the retry itself).
			alreadyTried := ordered[:ti+1]
			ctxRetry = func() []RouteTarget {
				return p.contextOverflowRetry(cfg, parentOf, cat, exposed, sessionKey, alreadyTried, expanded, routeKeys, origBody, generation)
			}
		}
		attempt := newTargetAttempt(
			runtime,
			plan,
			attemptExchange{
				request: r,
				writer:  w,
				body:    body,
			},
			attemptScope{
				calledModel:      calledModel,
				agent:            agent,
				cacheKey:         cacheKey,
				log:              flc,
				responseContext:  plan.responseContext(origBody),
				responsesHistory: responsesHistory,
				responsesSession: sessionKey,
			},
			attemptPolicy{
				force:        force,
				lastTarget:   ti == len(ordered)-1,
				contextRetry: ctxRetry,
			},
		)
		committed, retried, outcome, commit := p.targetExecutor().execute(attempt)
		res.tried[t.Provider] = true
		switch outcome {
		case tryFailedHard:
			res.sawHard = true
		case tryRateLimited:
			res.sawCooldown = true
		}
		if committed {
			p.dispatchShadowAfterCommit(
				runtime,
				proto,
				string(plan.backendProto),
				calledModel,
				exposed,
				t,
				requestID,
				commit,
			)
			res.committed = true
			return res // committed: response written to the client
		}
		if retried != nil {
			st.retriedForContext = true
			log.Printf("[proto=%s model=%s] target %d (%s/%s) context overflow; retrying with larger-context targets", proto, exposed, ti, t.Provider, t.Model)
			ordered = retried
			ti = -1 // restart at the first replacement target (post-statement ti++ → 0)
			continue
		}
		log.Printf("[proto=%s model=%s] target %d (%s/%s) failed; trying next", proto, exposed, ti, t.Provider, t.Model)
	}
	// Record the target set actually considered this pass (post scheduling /
	// request-aware narrowing / context retry) so forward's cooldown + TOCTOU
	// decisions key on what was really in play, not the original route targets.
	res.effectiveTargets = ordered
	return res
}

// dispatchShadowAfterCommit is orchestration-layer post-processing for a
// successfully delivered normal target. The executor returns only the exact
// upstream request bytes; Shadow policy, sampling, lifecycle admission, and the
// Proxy method call stay here. Fusion synthesis does not pass through this
// normal-route hook and therefore never recursively dispatches Shadow.
func (p *Proxy) dispatchShadowAfterCommit(
	runtime runtimeSnapshot,
	proto string,
	backendProto string,
	calledModel string,
	exposed string,
	primary RouteTarget,
	primaryRequestID string,
	commit *attemptCommit,
) {
	if commit == nil || p.reqLog == nil || len(runtime.cfg.Shadow) == 0 {
		return
	}
	shadow, ok := runtime.cfg.Shadow[exposed]
	if !ok || shadow.Provider == "" || shadow.Provider == primary.Provider {
		return
	}
	shadowRuntime := runtime.shadow
	if shadowRuntime == nil || !shadowRuntime.shouldSample() {
		return
	}
	select {
	case shadowRuntime.sem <- struct{}{}:
		if !p.lifecycle.runBeforeLogDrain(func() {
			defer func() { <-shadowRuntime.sem }()
			p.runShadow(
				runtime,
				shadowRuntime,
				proto,
				backendProto,
				calledModel,
				exposed,
				shadow,
				commit.requestBody,
				primaryRequestID,
			)
		}) {
			<-shadowRuntime.sem
		}
	default:
		// Shadow concurrency cap reached → skip (best-effort).
	}
}

// shadowRuntime is the reload-swappable shadow dispatch state. reload replaces
// the whole bundle via an atomic store; each dispatch loads it once, so in-flight
// goroutines finish on the bundle they started with (same sem/client) while new
// traffic follows the reloaded sample rate / concurrency cap / client timeout.
type shadowRuntime struct {
	sem      chan struct{} // buffered concurrency gate (cap = max concurrent)
	client   *http.Client  // shared HTTP client for shadow requests
	sampRate float64       // 0-1; fraction of requests to shadow (1.0 = all, 0 = off)
}

// newShadowRuntime builds the shadow dispatch bundle from a config (used by both
// NewProxy and reload so the two stay in sync).
func newShadowRuntime(cfg *Config) *shadowRuntime {
	maxConc := cfg.ShadowMaxConcurrent
	if maxConc <= 0 {
		maxConc = 4
	}
	sr := &shadowRuntime{
		sem:      make(chan struct{}, maxConc),
		client:   &http.Client{Timeout: cfg.Scheduling.Timeout()},
		sampRate: 1.0, // default; nil ShadowSampleRate = all requests
	}
	if cfg.ShadowSampleRate != nil {
		sr.sampRate = *cfg.ShadowSampleRate // explicit 0.0 = off
	}
	return sr
}

// shouldSample reports whether this request should be shadow-evaluated, based on
// the configured sample rate (1.0 = all, 0.5 = half, 0 = none). A nil sem means
// shadowing is not configured.
func (sr *shadowRuntime) shouldSample() bool {
	if sr == nil || sr.sem == nil {
		return false
	}
	if sr.sampRate >= 1 {
		return true
	}
	if sr.sampRate <= 0 {
		return false
	}
	return rand.Float64() < sr.sampRate
}

// shouldShadow reports whether this request should be shadow-evaluated, based on
// the currently-loaded shadow runtime's sample rate. Reload-aware: the runtime
// pointer is swapped atomically, so a config change (e.g. sample_rate: 0) takes
// effect immediately without a restart.
func (p *Proxy) shouldShadow() bool {
	return p.shadow.Load().shouldSample()
}

// runShadow sends the same prompt to a candidate backend (shadow evaluation,
// #12): fire-and-forget, the result is logged for offline comparison and NEVER
// returned to the client. It shares targetPlan request preparation but is
// best-effort and bounded — any error is logged and dropped (shadow must never
// affect the live request). Both runtimeSnapshot and shadowRuntime are captured
// by the primary attempt before launching the goroutine, so reload cannot mix
// config/provider generation with a different semaphore/client bundle.
//
// `bodyProto` is the protocol of reqBody (the primary target's backend proto —
// reqBody may already be converted from the client's proto). The shadow backend's
// own protocol is shadow.Protocol (defaulting to bodyProto); runShadow selects the
// shadow base URL + path for THAT protocol and converts the body if it differs.
func (p *Proxy) runShadow(runtime runtimeSnapshot, shadowRuntime *shadowRuntime, proto, bodyProto, calledModel, exposed string, shadow ShadowTarget, reqBody []byte, primaryReqID string) {
	if runtime.cfg == nil {
		// Defensive: runtimeSnapshot is handed around as a plain value — a
		// future call site that forgets to populate it must not nil-deref
		// below (runtime.cfg.Scheduling.Timeout()). Log loudly and skip.
		log.Printf("[shadow] %s: skipped — runtime snapshot has no config (caller bug)", shadow.Provider)
		return
	}
	logger := p.reqLog
	if logger == nil {
		return // nowhere to record → no point shadowing
	}
	// Resolve the shadow target to a runnable virtual via the unified resolver
	// (pooled parent → one healthy account; "" stickyKey → spread/round-robin since
	// shadow is fire-and-forget). A pooled parent name has no runtime instance, so
	// without this shadow silently stopped sampling the moment a second account was
	// added.
	target := RouteTarget{Provider: shadow.Provider, Model: shadow.Model, Protocol: shadow.Protocol}
	picked, ok := newResolver(
		p,
		runtime.providers,
		runtime.poolIndex,
		runtime.generation,
	).Pick(target, "")
	if !ok {
		log.Printf("[shadow] %s: provider not available (no runnable healthy virtual)", shadow.Provider)
		return
	}
	target = picked
	shadow.Provider = target.Provider
	plan, err := p.planTarget(targetPlanInput{
		runtime: runtime, target: target, clientProto: bodyProto, clientPath: protocol.BackendPath(protocol.Protocol(bodyProto)),
	})
	if err != nil {
		log.Printf("[shadow] %s: target plan failed: %v", shadow.Provider, err)
		return
	}
	provCfg := plan.providerCfg
	impl := plan.providerImpl
	if impl == nil {
		log.Printf("[shadow] %s: provider not available", shadow.Provider)
		return
	}
	// Shadow backend protocol: declared, else the provider's ProtocolHint
	// (auto-resolve, e.g. codex→responses), else the wire verdict, else same as
	// the body's. Route + convert accordingly so the shadow gets a request in
	// the protocol IT speaks.
	sbody := plan.rewriteModel(reqBody, calledModel)
	sbody, err = plan.convertBody(sbody)
	if err != nil {
		// Fail CLOSED: don't send the unconverted body to the shadow backend.
		log.Printf("[shadow] %s: %s→%s convert failed: %v — skipping",
			shadow.Provider, bodyProto, plan.backendProto, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtime.cfg.Scheduling.Timeout())
	defer cancel()
	targetURL := strings.TrimRight(plan.baseURL, "/") + plan.upPath
	if impl != nil {
		targetURL, sbody = impl.RewriteRequest(targetURL, sbody, plan.upPath)
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
		impl.ExtraHeaders(sreq, plan.upPath)
	}
	for k, v := range provCfg.Headers {
		sreq.Header.Set(k, v)
	}
	client := shadowRuntime.client
	start := time.Now()
	resp, err := client.Do(sreq)
	if err != nil {
		log.Printf("[shadow] %s/%s upstream error: %v", shadow.Provider, shadow.Model, err)
		return
	}
	// Drain the shadow response into a bounded capture for the log. The reader
	// passes all bytes through (drained to Discard) while teeing a capped copy.
	var captured []byte
	var capturedTotal int64
	var capturedTruncated bool
	cr := bodycapture.New(resp.Body, logger.MaxBodyBytes(), func(body []byte, total int64, truncated bool) {
		captured = append([]byte(nil), body...)
		capturedTotal = total
		capturedTruncated = truncated
	})
	_, _ = io.Copy(io.Discard, cr)
	_ = cr.Close()
	logInput := requestLogInput(
		forwardLogCtx{requestID: "shadow-" + primaryReqID, exposed: exposed},
		sreq,
		proto,
		calledModel,
		RouteTarget{Provider: shadow.Provider, Model: shadow.Model},
		resp,
		start,
		sbody,
	)
	completeRequestLog(logger, logInput, captured, capturedTotal, capturedTruncated)
}
