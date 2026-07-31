package main

import (
	"fmt"
	"io"
	"log"
	"model-proxy/internal/observe/counters"
	"net/http"
	"strconv"
	"strings"
	"time"

	responsecache "model-proxy/internal/cache"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

// forward proxies a request to the upstream selected by the route for the
// requested model. Routing is two-step: for anthropic, the called model name is
// first translated via claude_mapping (if the called name is mapped); openai
// uses the called name directly. The (translated) name is then looked up in
// routes, which maps it to an ordered list of provider/model targets. The proxy
// schedules the route's sticky provider first (within its dwell window), else the
// best available by (non-peak, priority); providers with an open circuit or active
// rate-limit are skipped. It fails over to the next on connection error /
// 401-after-refresh / 5xx / 429, and retries once on a strictly-larger-context
// target when the upstream answers a context-overflow 400. The protocol (from
// the request path) selects the
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

	calledModel := protocol.ExtractModel(origBody)
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
	forcedProvider := forcedProviderFromRequest(r)

	// Exact-match response cache (#10): a request byte-identical to a recently
	// served one is replayed from cache with no upstream call. Computed before
	// routing (the key is the raw request), threaded into tryTarget to store on
	// a fresh 2xx commit. SKIPPED entirely when a force-provider override OR a pin
	// is in effect — both mean "send to THIS backend", not a stale cached answer.
	var cacheKey string
	if cache != nil && forcedProvider == "" && !force {
		cacheKey = responsecache.Key(r, origBody)
		if e, ok := cache.Lookup(cacheKey, time.Now()); ok {
			// Live monitor (#6): a cache hit skips the normal start/end flow, so
			// emit an end event explicitly — otherwise the live view is blind to
			// these (e.g. a retry-looping agent served from cache stays invisible).
			p.events.Publish(observeevents.Event{
				Type:      "end",
				Ts:        time.Now().UnixMilli(),
				RequestID: requestID,
				Agent:     counters.DetectAgent(r),
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
	if forcedProvider != "" {
		narrowed := routing.FilterTargetsByProvider(targets, parentOf, forcedProvider)
		if len(narrowed) == 0 {
			p.publishTerminalEvent(requestID, r, proto, exposed, http.StatusBadRequest)
			http.Error(w, fmt.Sprintf("force-provider %q is not a target for model %q", forcedProvider, exposed), http.StatusBadRequest)
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
	agent := counters.DetectAgent(r)

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

		targets:        targets,
		routeKeys:      routeKeys,
		force:          force,
		forcedProvider: forcedProvider,
		cacheKey:       cacheKey,
		origBody:       origBody,

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
			protocol.WriteUnsupportedConversionError(w, protocol.Protocol(proto), res.conversionErr)
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
		bypassWait := forcedProvider != ""
		recoveredUntried := false
		if !bypassWait && retryWait > 0 && round < 2 && !allDown {
			recoveredUntried = p.hasRecoveredUntried(checkTargets, res.tried, now)
		}
		decision := routing.DecideFailure(routing.FailureInput{
			SawHard:            sawHard,
			SawRateLimit:       sawCool,
			AllDown:            allDown,
			AllDownRateLimited: allRateLimited,
			RecoveredUntried:   recoveredUntried,
			BypassWait:         bypassWait,
			Round:              round,
			MaxRetryRounds:     2,
			RetryWait:          retryWait,
			RateLimitBackoff:   cfg.Scheduling.RateBackoff(),
			EarliestRecovery:   earliest,
			Now:                now,
		})
		switch decision.Action {
		case routing.FailureWait:
			log.Printf("[proto=%s model=%s] all targets cooling down; retry %d/2 in %s", proto, exposed, round+1, decision.Wait.Round(time.Millisecond))
			select {
			case <-time.After(decision.Wait):
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
		case routing.FailureRetryNow:
			// TOCTOU: a target recovered between scheduling and this terminal
			// check but was never tried in the failed pass.
			log.Printf("[proto=%s model=%s] a cooled-down target recovered; retrying immediately (round %d/2)", proto, exposed, round+1)
			continue
		}
		// Terminal: every target failed. The status is honest about the CLASS of
		// failures seen ACROSS ALL PASSES (not a racy health re-read — a target
		// whose cooldown lapsed mid-request without a retry must not flip the
		// verdict): pure rate-limit → 429 + Retry-After (the upstreams' own
		// answer, per RFC 9110); any hard failure → 502. Attribute the failure
		// to the calling agent so failing-only agents stay visible (first-tried
		// target = where the request WAS directed).
		if p.agents != nil && agent != "" && res.firstTried.Provider != "" {
			p.agents.IncRequests(agent, res.firstTried.Provider, res.firstTried.Model)
			p.agents.IncFailure(agent, res.firstTried.Provider, res.firstTried.Model)
		}
		status := http.StatusBadGateway
		msg := fmt.Sprintf("all targets failed for model %q", exposed)
		if decision.RateLimited {
			w.Header().Set("Retry-After", strconv.Itoa(decision.RetryAfterSeconds))
			status = http.StatusTooManyRequests
			msg = fmt.Sprintf("all providers for model %q are rate-limited; retry after %ds", exposed, decision.RetryAfterSeconds)
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
	cat := runtime.catalog
	proto := req.proto
	upPath := req.upPath
	exposed := req.exposed
	calledModel := req.calledModel
	sessionKey := req.sessionKey
	targets := req.targets
	routeKeys := req.routeKeys
	force := req.force
	forcedProvider := req.forcedProvider
	cacheKey := req.cacheKey
	w := req.writer
	r := req.request
	agent := req.agent
	requestID := req.requestID
	origBody := req.origBody

	planner := requestRoutingPlanner(p, runtime, routeKeys)
	ordered := p.schedule(cfg, parentOf, exposed, sessionKey, targets, routeKeys, generation)
	// `force` (pin) was computed before the cache. A pin is EXCLUSIVE: it
	// overrides request-aware routing (no cross-route reroute away from the pinned
	// provider) and, via the `force` flag into tryTarget, bypasses the circuit
	// breaker — the user explicitly asked for THIS backend, no failover.
	if !force && forcedProvider == "" {
		// Request-aware routing (#8 capability + #9 context, unified): keep targets
		// that fit the request (image capability + context window); if none in the
		// route fit, fall back to a cross-route capable+fitting pool ranked by the
		// normal scheduling policy. No-op when everything already fits.
		ordered = planner.Apply(exposed, sessionKey, ordered, origBody)
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
		if t.Provider == "fusion" && forcedProvider == "" {
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
		body := plan.RewriteModel(origBody, calledModel)
		var responsesHistory []any
		if proto == "responses" && plan.BackendProtocol() != protocol.Responses && p.responsesState != nil {
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
		body, err = plan.ConvertBody(body)
		if err != nil {
			if unsupported, ok := protocol.AsUnsupported(err); ok {
				if res.conversionErr == nil {
					res.conversionErr = unsupported
				}
				log.Printf("[proto=%s model=%s] target %d (%s/%s) %s→%s unsupported feature %s — trying another target",
					proto, exposed, ti, t.Provider, t.Model, proto, plan.BackendProtocol(), unsupported.Feature)
				continue
			}
			// Fail CLOSED: a conversion failure must NOT send the unconverted
			// body to the backend (that ships an Anthropic body to an OpenAI
			// endpoint, or vice versa). Skip this target and try the next; if
			// none serve, the loop's all-targets-failed path returns a 502.
			log.Printf("[proto=%s model=%s] target %d (%s/%s) %s→%s request convert failed: %v — skipping",
				proto, exposed, ti, t.Provider, t.Model, proto, plan.BackendProtocol(), err)
			continue
		}

		flc := forwardLogCtx{requestID: requestID, attempt: st.attempt, exposed: exposed, origBody: origBody}
		st.attempt++
		// One-shot larger-context retry: when this target answers a
		// context-overflow 400, tryTarget calls ctxRetry for a strictly-larger-
		// context replacement list (cross-route pool, scheduled) instead of
		// committing. Nil — no peek, no retry — once the retry is spent, while a
		// pin is in force (exclusive: no cross-route reroute), or without a
		// catalog (same no-op degradation as Planner.Apply).
		var ctxRetry func() []RouteTarget
		if !st.retriedForContext && !force && forcedProvider == "" && cat != nil {
			// Only capture targets ACTUALLY tried so far (through the current
			// index), not the full ordered list — failover targets further down
			// haven't been attempted yet and shouldn't anchor the "strictly larger
			// context" threshold (they might be worth trying as the retry itself).
			alreadyTried := ordered[:ti+1]
			ctxRetry = func() []RouteTarget {
				return planner.ContextOverflowRetry(exposed, sessionKey, alreadyTried, origBody)
			}
		}
		attempt := newTargetAttempt(
			runtime,
			plan,
			targetexec.Exchange{
				Request: r,
				Writer:  w,
				Body:    body,
			},
			targetexec.Scope{
				CalledModel:      calledModel,
				Agent:            agent,
				CacheKey:         cacheKey,
				Log:              targetLogContext(flc),
				ResponseContext:  plan.ResponseContext(origBody),
				ResponsesHistory: responsesHistory,
				ResponsesSession: sessionKey,
			},
			targetexec.Policy{
				Force:        force,
				LastTarget:   ti == len(ordered)-1,
				ContextRetry: ctxRetry,
			},
		)
		result := p.targetExecutor(attempt.Runtime()).Execute(attempt)
		res.tried[t.Provider] = true
		switch result.Outcome {
		case targetexec.OutcomeFailedHard:
			res.sawHard = true
		case targetexec.OutcomeRateLimited:
			res.sawCooldown = true
		}
		if result.Committed {
			p.dispatchShadowAfterCommit(
				runtime,
				proto,
				string(plan.BackendProtocol()),
				calledModel,
				exposed,
				t,
				requestID,
				result.Commit,
			)
			res.committed = true
			return res // committed: response written to the client
		}
		if result.Retried != nil {
			st.retriedForContext = true
			log.Printf("[proto=%s model=%s] target %d (%s/%s) context overflow; retrying with larger-context targets", proto, exposed, ti, t.Provider, t.Model)
			ordered = result.Retried
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
