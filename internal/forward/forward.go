package forward

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/seclog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

// Serve runs the request-forwarding pipeline: it proxies a request to the
// upstream selected by the route for the requested model. Routing is two-step:
// for anthropic, the called model name is first translated via claude_mapping
// (if the called name is mapped); openai uses the called name directly. The
// (translated) name is then looked up in routes, which maps it to an ordered
// list of provider/model targets. The proxy schedules the route's sticky
// provider first (within its dwell window), else the best available by
// (non-peak, priority); providers with an open circuit or active rate-limit
// are skipped. It fails over to the next on connection error /
// 401-after-refresh / 5xx / 429, and retries once on a
// strictly-larger-context target when the upstream answers a context-overflow
// 400. The protocol (from the request path) selects the upstream path and base
// URL (anthropic_base_url vs openai_base_url); it does not key the route.
//
// snap is the caller's single per-request runtime capture (internal/app takes
// the lock there): the lock is NOT held during forwarding (which streams for
// minutes on SSE); the immutable snapshot keeps routing, providers, catalog,
// and cache on one config generation.
func Serve(svc Services, state RouteState, snap Snapshot, proto string, w http.ResponseWriter, r *http.Request, requestID string) {
	pipeline{svc: svc, state: state}.forward(snap, proto, w, r, requestID)
}

func (p pipeline) forward(runtime Snapshot, proto string, w http.ResponseWriter, r *http.Request, requestID string) {
	cfg := runtime.Cfg
	expanded := runtime.ExpandedRoutes
	parentOf := runtime.ParentOf
	cache := runtime.Cache

	// Bound per-request memory: the body is fully buffered for routing and
	// conversion, so anything over max_request_body_bytes (default 64 MiB) is
	// rejected before any routing work.
	maxBody := cfg.MaxRequestBodyBytesValue()
	origBody, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()
	if int64(len(origBody)) > maxBody {
		p.publishTerminalEvent(requestID, r, proto, "", http.StatusRequestEntityTooLarge)
		http.Error(w, fmt.Sprintf("request body exceeds max_request_body_bytes (%d bytes)", maxBody), http.StatusRequestEntityTooLarge)
		return
	}

	// Detect the calling agent once (UA / known headers); attributed to guard
	// and cache-hit events here and to whichever target commits downstream.
	agent := counters.DetectAgent(r)

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
	forcedProvider := ForcedProviderFromRequest(r)

	// Outbound secret guard (DLP-lite): scan the SHARED request body once, here
	// — after route resolution (so hits are attributable) and before the cache
	// lookup and every forward branch. Cache, Fusion and Shadow all consume the
	// (possibly redacted) origBody from this point on; no branch rescans. Only
	// pattern TYPE NAMES / path CATEGORY NAMES are counted/emitted — matched
	// bytes never leave the body (credential red line). The scanner is this
	// generation's snapshot (runtime.Guard): it carries the embedded rule
	// table, config custom patterns/paths, and the known-secret values of the
	// credential pool. nil only in degenerate hand-built proxies — skip then.
	if sc := runtime.Guard; sc != nil {
		action := cfg.Guard.SecretsAction()
		// preGuardBody is the body as received, before any redact rewrite
		// below; the split-exfiltration window must store THIS form (storing
		// the redacted form would destroy the very fragments that pass exists
		// to reassemble). In-memory only, bounded — see internal/guard/session.
		preGuardBody := origBody
		guardDecision := EvaluateRequestGuard(cfg.Guard, sc, origBody)
		secretNames := guardDecision.Secrets
		if len(secretNames) > 0 {
			if p.svc.Metrics != nil {
				for _, name := range secretNames {
					p.svc.Metrics.Inc("guard", name, counters.EvGuardHits)
				}
			}
			p.svc.Events.Publish(observeevents.Event{
				Type:      "guard",
				Ts:        time.Now().UnixMilli(),
				RequestID: requestID,
				Agent:     agent,
				Protocol:  proto,
				Exposed:   exposed,
				Detail:    "secrets=" + strings.Join(secretNames, ",") + " action=" + action,
			})
			AuditGuardHit(runtime.SecLog, seclog.KindSecret, secretNames, action, requestID, agent, proto, exposed)
		}
		origBody = guardDecision.ForwardBody
		// Sensitive-path signal (S2): an intent-level alert fired before any
		// secret value appears. Paths are never redacted (rewriting a path
		// would corrupt legitimate coding work). Hits are context-split
		// (guard.ScanPathsContext): a path inside a tool-call/tool-result
		// position is STRONG — the structural signature of an agent reading a
		// sensitive file through a tool (MCP Tool Poisoning shape) — and gets
		// the configured guard.paths action: live event, ("guard", cat)
		// counter, audit record, and it is the only kind block can 400. A path
		// in ordinary prose is WEAK (coding agents legitimately discuss .env
		// & friends all the time): it only increments ("guard", cat+"_text")
		// and writes an audit record with action "log-weak" — no live event
		// (would spam the monitor), never blocked (正文提及敏感路径不阻断).
		// This scan runs even when the secrets pass already hit — including
		// secrets=block — so one request carrying both signals gets both
		// counters/events/audit records; only the response action is decided
		// afterwards (below).
		pa := cfg.Guard.PathsAction()
		pathCats := guardDecision.StrongPath
		if pa != "off" {
			if len(pathCats) > 0 {
				if p.svc.Metrics != nil {
					for _, cat := range pathCats {
						p.svc.Metrics.Inc("guard", cat, counters.EvGuardHits)
					}
				}
				p.svc.Events.Publish(observeevents.Event{
					Type:      "guard",
					Ts:        time.Now().UnixMilli(),
					RequestID: requestID,
					Agent:     agent,
					Protocol:  proto,
					Exposed:   exposed,
					Detail:    "paths=" + strings.Join(pathCats, ",") + " action=" + pa,
				})
				AuditGuardHit(runtime.SecLog, seclog.KindPath, pathCats, pa, requestID, agent, proto, exposed)
			}
			if len(guardDecision.WeakPath) > 0 {
				if p.svc.Metrics != nil {
					for _, cat := range guardDecision.WeakPath {
						p.svc.Metrics.Inc("guard", cat+"_text", counters.EvGuardHits)
					}
				}
				AuditGuardHit(runtime.SecLog, seclog.KindPath, guardDecision.WeakPath, "log-weak", requestID, agent, proto, exposed)
			}
		}
		// Split-exfiltration signal (fragmented known secret): a credential
		// smuggled out in pieces — one fragment per request — never hits the
		// per-request scan above. When session_scan is on, this generation's
		// scanner carries known secrets, and the request declares a session
		// (x-claude-code-session-id), two channels run over the session's
		// bounded state (known-secret channel only — rule-table/custom hits
		// were already reported per request):
		//  1. exact reassembly: an occurrence present in tail+body but in
		//     NEITHER alone must span the junction, so only the junction
		//     region (the last MaxKnownNeedleLen-1 bytes of the tail plus the
		//     first MaxKnownNeedleLen-1 bytes of the current PRE-REDACT body)
		//     is scanned through ScanKnown — same verdict as scanning the
		//     whole concatenation without copying up to 64MiB+32KiB per
		//     request. The tail-alone verdict comes from the session entry's
		//     cache (refreshed at Add time; a miss rescans the tail) and
		//     excludes a key fully seen in an earlier request from re-firing
		//     "fragmented" on every later one; the current-body-alone verdict
		//     is the already-computed secretNames (known secrets are claimed
		//     first in the scanner, so a current-body occurrence always lands
		//     there).
		//  2. fragment progress: realistic bodies all start with '{' (see
		//     ExtractModel), so fragments can never sit byte-contiguously at
		//     the junction — guard.ScanKnownFragment instead tracks each
		//     secret's longest prefix seen in order across the session.
		// secrets=off disables this pass together with the secrets channel.
		var fragmented bool
		sessionID := r.Header.Get("x-claude-code-session-id")
		if action != "off" && cfg.Guard.SessionScanEnabled() && sc.HasKnownSecrets() &&
			sessionID != "" && p.svc.SessionScan != nil {
			tail, progress, tailKnown, tailKnownOK := p.svc.SessionScan.Snapshot(sessionID, sc)
			knownInCurrent := false
			for _, n := range secretNames {
				if n == "known_secret" || n == "known_secret_encoded" {
					knownInCurrent = true
					break
				}
			}
			var nextProgress []int
			var progressReset []bool
			if len(tail) > 0 && !knownInCurrent {
				if !tailKnownOK {
					tailKnown = len(sc.ScanKnown(tail)) > 0
				}
				if !tailKnown {
					span := sc.MaxKnownNeedleLen() - 1
					start := max(len(tail)-span, 0)
					end := min(span, len(preGuardBody))
					junction := make([]byte, 0, len(tail)-start+end)
					junction = append(junction, tail[start:]...)
					junction = append(junction, preGuardBody[:end]...)
					if len(sc.ScanKnown(junction)) > 0 {
						fragmented = true
					}
				}
			}
			if !fragmented && !knownInCurrent {
				fragmented, nextProgress, progressReset = sc.ScanKnownFragment(preGuardBody, progress)
			}
			if fragmented {
				fragAction := action
				if fragAction == "redact" {
					// The secret spans requests — no single body can be
					// rewritten; degrade to alert-only (documented in
					// docs/decisions/intentional-behaviors.md).
					fragAction = "log"
				}
				if p.svc.Metrics != nil {
					p.svc.Metrics.Inc("guard", "known_secret_fragmented", counters.EvGuardHits)
				}
				p.svc.Events.Publish(observeevents.Event{
					Type:      "guard",
					Ts:        time.Now().UnixMilli(),
					RequestID: requestID,
					Agent:     agent,
					Protocol:  proto,
					Exposed:   exposed,
					Detail:    "secrets=known_secret_fragmented action=" + fragAction,
				})
				AuditGuardHit(runtime.SecLog, seclog.KindSecret, []string{"known_secret_fragmented"}, fragAction, requestID, agent, proto, exposed)
			}
			// Merge the current body into the session window whether or not
			// anything hit — later fragments depend on earlier ones being
			// retained. The window stores the PRE-REDACT form and lives in
			// memory only (bounded: 256 sessions × 32KiB tail; see
			// internal/guard/session for the red lines).
			p.svc.SessionScan.Add(sessionID, preGuardBody, sc, nextProgress, progressReset, knownInCurrent)
		}
		// Unified action evaluation after BOTH scans: a secrets block outranks
		// a paths block and its message names only the secret patterns — the
		// path hits of the same request are already counted/audited above.
		if len(secretNames) > 0 && action == "block" {
			p.publishTerminalEvent(requestID, r, proto, exposed, http.StatusBadRequest)
			http.Error(w, fmt.Sprintf("blocked: request body contains a secret matching %s (guard.secrets=block)", strings.Join(secretNames, ", ")), http.StatusBadRequest)
			return
		}
		if fragmented && action == "block" {
			p.publishTerminalEvent(requestID, r, proto, exposed, http.StatusBadRequest)
			http.Error(w, "blocked: request completes a secret fragmented across requests matching known_secret_fragmented (guard.secrets=block)", http.StatusBadRequest)
			return
		}
		if len(pathCats) > 0 && pa == "block" {
			p.publishTerminalEvent(requestID, r, proto, exposed, http.StatusBadRequest)
			http.Error(w, fmt.Sprintf("blocked: request body references sensitive path %s (guard.paths=block)", strings.Join(pathCats, ", ")), http.StatusBadRequest)
			return
		}
	}

	// Exact-match response cache (#10): a request byte-identical to a recently
	// served one is replayed from cache with no upstream call. Computed before
	// routing (the key is the raw request), threaded into the target attempt to
	// store on a fresh 2xx commit. SKIPPED entirely when a force-provider
	// override OR a pin is in effect — both mean "send to THIS backend", not a
	// stale cached answer.
	var cacheKey string
	if cache != nil && forcedProvider == "" && !force {
		cacheKey = responsecache.Key(r, origBody)
		if e, ok := cache.Lookup(cacheKey, time.Now()); ok {
			// Live monitor (#6): a cache hit skips the normal start/end flow, so
			// emit an end event explicitly — otherwise the live view is blind to
			// these (e.g. a retry-looping agent served from cache stays invisible).
			p.svc.Events.Publish(observeevents.Event{
				Type:      "end",
				Ts:        time.Now().UnixMilli(),
				RequestID: requestID,
				Agent:     agent,
				Protocol:  proto,
				// Same mapping as the start event above: with claude_mapping
				// the live view must show ONE exposed name per request, not
				// the called name on start and the mapped name on end.
				Exposed:  exposed,
				Provider: "(cache)",
				Status:   e.Status(),
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
	// the scheduler to tell route-name sticky keys (preserve) from session-id
	// keys (evict after dwell). Generation-owned: rebuilt with expandedRoutes.
	routeKeys := runtime.RouteKeys

	// Live request monitor (#6): announce the in-flight request so the Web UI's
	// live view sees who is sending + where it routed, before the response lands.
	p.svc.Events.Publish(observeevents.Event{
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
		if res.clientGone {
			// Client went away before any commit — no terminal status is
			// writable to a dead connection. Close the live event pair as 499
			// (client closed request), same as the cooldown-wait cancel path.
			p.publishTerminalEvent(requestID, r, proto, exposed, statusClientGone)
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
		// Same freshness window the scheduler uses for its quota-exhausted
		// skip — frozen to the poll cadence at quota-tracker Start.
		quotaMaxAge := p.quotaFreshnessMaxAge(cfg)
		allDown, allRateLimited, earliest := p.cooldownState(checkTargets, now, quotaMaxAge)
		bypassWait := forcedProvider != ""
		recoveredUntried := false
		if !bypassWait && retryWait > 0 && round < 2 && !allDown {
			recoveredUntried = p.hasRecoveredUntried(checkTargets, res.tried, now, quotaMaxAge)
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
			logx.Debugf("[proto=%s model=%s] all targets cooling down; retry %d/2 in %s", proto, exposed, round+1, decision.Wait.Round(time.Millisecond))
			select {
			case <-time.After(decision.Wait):
				continue
			case <-r.Context().Done():
				// Client gave up waiting — close the live event pair (499 =
				// client closed request) and write nothing.
				p.publishTerminalEvent(requestID, r, proto, exposed, statusClientGone)
				return
			}
		case routing.FailureRetryNow:
			// TOCTOU: a target recovered between scheduling and this terminal
			// check but was never tried in the failed pass.
			logx.Debugf("[proto=%s model=%s] a cooled-down target recovered; retrying immediately (round %d/2)", proto, exposed, round+1)
			continue
		}
		// Terminal: every target failed across all passes — classify and answer.
		p.writeAllTargetsFailed(w, r, requestID, proto, exposed, agent, res, decision)
		return
	}
}

// statusClientGone marks a request abandoned by the CALLER (client closed the
// connection) before any terminal status was writable to the dead socket. The
// live view uses it to distinguish "we failed" from "they left".
const statusClientGone = 499

// writeAllTargetsFailed answers the terminal response after every failover
// pass failed, closing the live event pair. The status is honest about the
// CLASS of failures seen ACROSS ALL PASSES (not a racy health re-read — a
// target whose cooldown lapsed mid-request without a retry must not flip the
// verdict): pure rate-limit → 429 + Retry-After (the upstreams' own answer,
// per RFC 9110); any hard failure → 502. Attribute the failure to the calling
// agent so failing-only agents stay visible (first-tried target = where the
// request WAS directed).
func (p pipeline) writeAllTargetsFailed(
	w http.ResponseWriter,
	r *http.Request,
	requestID, proto, exposed, agent string,
	res serveResult,
	decision routing.FailureDecision,
) {
	if p.svc.Agents != nil && agent != "" && res.firstTried.Provider != "" {
		p.svc.Agents.IncRequests(agent, res.firstTried.Provider, res.firstTried.Model)
		p.svc.Agents.IncFailure(agent, res.firstTried.Provider, res.firstTried.Model)
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
	p.svc.Events.Publish(observeevents.Event{
		Type:      "end",
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     agent,
		Protocol:  proto,
		Exposed:   exposed,
		Status:    status,
	})
	http.Error(w, msg, status)
}

// serveState carries the two per-request pieces of state that must survive a
// cooldown wait-retry round (serveOnce is otherwise re-entrant).
type serveState struct {
	retriedForContext bool // the larger-context retry is one-shot per request
	attempt           int  // monotonic target-attempt index for the request log (ti resets on a context retry)
	profiled          bool // request profile computed (see requestProfile)
	profile           routing.Profile
}

// requestProfile returns the request's routing profile, computed at most once
// per request: the body is immutable, so the wait-retry rounds and the
// context-overflow retry share the first scan instead of re-walking the body.
func (p pipeline) requestProfile(st *serveState, cat *catalog.Catalog, body []byte) routing.Profile {
	if cat == nil {
		return routing.Profile{}
	}
	if !st.profiled {
		st.profile = routing.ProfileRequest(body)
		st.profiled = true
	}
	return st.profile
}

// serveResult is the outcome of one serveOnce pass: where the request was
// first directed, who was actually tried, and the failure CLASS mix — the
// terminal status derives from these (pure cooldown → 429; any hard → 502),
// NOT from a racy health re-read at terminal time.
type serveResult struct {
	committed   bool
	firstTried  RouteTarget
	tried       map[string]bool // providers actually attempted this pass
	sawHard     bool            // conn/timeout/5xx/401/build/model-denied-class failure
	sawCooldown bool            // at least one 429 this pass
	// clientGone: the caller disconnected before any target committed. The
	// failover loop stops immediately — every remaining target would fail
	// instantly on the dead request context and (pre-fix) each burned a
	// circuit-breaker tick for a provider that never misbehaved.
	clientGone    bool
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
func (p pipeline) serveOnce(req serveRequest, st *serveState) serveResult {
	runtime := req.runtime
	cfg := runtime.Cfg
	generation := runtime.Generation
	parentOf := runtime.ParentOf
	cat := runtime.Catalog
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
	// Routing-decision overhead (scheduling + request-aware planning), observed
	// separately from upstream latency under the virtual ("routing","decision")
	// row — sum/requests there is the mean decision time per request.
	routeStart := time.Now()
	ordered := p.schedule(cfg, parentOf, exposed, sessionKey, targets, routeKeys, generation)
	// `force` (pin) was computed before the cache. A pin is EXCLUSIVE: it
	// overrides request-aware routing (no cross-route reroute away from the pinned
	// provider) and, via the `force` flag into the attempt, bypasses the circuit
	// breaker — the user explicitly asked for THIS backend, no failover.
	if !force && forcedProvider == "" {
		// Request-aware routing (#8 capability + #9 context, unified): keep targets
		// that fit the request (image capability + context window); if none in the
		// route fit, fall back to a cross-route capable+fitting pool ranked by the
		// normal scheduling policy. No-op when everything already fits.
		ordered = planner.ApplyWithProfile(exposed, sessionKey, ordered, p.requestProfile(st, cat, origBody))
	}
	if p.svc.Metrics != nil {
		p.svc.Metrics.Inc("routing", "decision", counters.EvRoutingObserved)
		p.svc.Metrics.AddLatency("routing", "decision", uint64(time.Since(routeStart).Milliseconds()), 0)
	}
	var firstTried RouteTarget
	if len(ordered) > 0 {
		firstTried = ordered[0]
	}
	res := serveResult{firstTried: firstTried, tried: map[string]bool{}}
	for ti := 0; ti < len(ordered); ti++ {
		t := ordered[ti]
		// A cancelled request context means every remaining target would fail
		// instantly in the executor — stop before doing per-target work.
		if r.Context().Err() != nil {
			res.clientGone = true
			break
		}
		// Fusion orchestration: {provider: fusion, model: <recipe>} is NOT a
		// provider — intercept before the providerConfig lookup and run the
		// panel→synthesis engine (its synthesizer leg reuses the target
		// executor). A force-provider override (replay) targets one concrete
		// backend, so it skips fusion entirely.
		if t.Provider == "fusion" && forcedProvider == "" {
			recipe, ok := cfg.Fusion[t.Model]
			if !ok {
				logx.Warnf("[proto=%s model=%s] target %d: fusion recipe %q not defined, skipping", proto, exposed, ti, t.Model)
				continue
			}
			fc := fusionCtx{
				runtime: runtime,
				proto:   proto, calledModel: calledModel, upPath: upPath, agent: agent,
				sessionKey: sessionKey,
				origBody:   origBody,
				flc:        LogCtx{RequestID: requestID, Attempt: st.attempt, Exposed: exposed, OrigBody: origBody},
			}
			st.attempt++
			res.tried[t.Provider] = true
			if p.runFusion(fc, t.Model, recipe, w, r, cacheKey) {
				res.committed = true
				return res // committed: response written to the client
			}
			res.sawHard = true // a failed fusion run is opaque → treat as hard
			logx.Warnf("[proto=%s model=%s] target %d (fusion/%s) failed; trying next", proto, exposed, ti, t.Model)
			continue
		}
		plan, err := p.planTarget(PlanInput{
			Runtime: runtime, Target: t, ClientProto: proto, ClientPath: upPath,
		})
		if err != nil {
			logx.Warnf("[proto=%s model=%s] target %d: %v, skipping", proto, exposed, ti, err)
			continue
		}

		// Rewrite the body's model to this target's real model (per target), then
		// convert the request to the backend protocol if needed.
		body := plan.RewriteModel(origBody, calledModel)
		var responsesHistory []any
		if proto == "responses" && plan.BackendProtocol() != protocol.Responses && p.svc.ResponsesState != nil {
			expandedBody, history, hit, err := p.svc.ResponsesState.Expand(body, sessionKey)
			if err != nil {
				logx.Warnf("[proto=%s model=%s] target %d (%s/%s) responses state expansion failed: %v — skipping",
					proto, exposed, ti, t.Provider, t.Model, err)
				continue
			}
			if protocol.PreviousResponseID(body) != "" && !hit {
				logx.Infof("[proto=%s model=%s] previous_response_id cache miss; repaired orphaned continuation items", proto, exposed)
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
				logx.Warnf("[proto=%s model=%s] target %d (%s/%s) %s→%s unsupported feature %s — trying another target",
					proto, exposed, ti, t.Provider, t.Model, proto, plan.BackendProtocol(), unsupported.Feature)
				continue
			}
			// Fail CLOSED: a conversion failure must NOT send the unconverted
			// body to the backend (that ships an Anthropic body to an OpenAI
			// endpoint, or vice versa). Skip this target and try the next; if
			// none serve, the loop's all-targets-failed path returns a 502.
			logx.Warnf("[proto=%s model=%s] target %d (%s/%s) %s→%s request convert failed: %v — skipping",
				proto, exposed, ti, t.Provider, t.Model, proto, plan.BackendProtocol(), err)
			continue
		}

		// This attempt's conversion diagnostics ride the log context into the
		// request log (empty for same-protocol passthrough).
		var convDiags []targetexec.ConversionDiagnostic
		if diag := plan.ConversionDiag(); diag != nil {
			for _, item := range diag.Items() {
				convDiags = append(convDiags, targetexec.ConversionDiagnostic{Code: item.Code, Detail: item.Detail})
			}
		}
		flc := LogCtx{RequestID: requestID, Attempt: st.attempt, Exposed: exposed, OrigBody: origBody, Diagnostics: convDiags}
		st.attempt++
		// One-shot larger-context retry: when this target answers a
		// context-overflow 400, the executor calls ctxRetry for a strictly-larger-
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
				return planner.ContextOverflowRetryWithProfile(exposed, sessionKey, alreadyTried, p.requestProfile(st, cat, origBody))
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
				CalledModel: calledModel,
				Agent:       agent,
				CacheKey:    cacheKey,
				Log: targetexec.LogContext{
					RequestID:    flc.RequestID,
					Attempt:      flc.Attempt,
					Exposed:      flc.Exposed,
					OriginalBody: flc.OrigBody,
					Diagnostics:  convDiags,
				},
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
		result := p.targetExecutor(attempt.Runtime(), runtime.ParentOf).Execute(attempt)
		res.tried[t.Provider] = true
		switch result.Outcome {
		case targetexec.OutcomeFailedHard:
			res.sawHard = true
		case targetexec.OutcomeRateLimited:
			res.sawCooldown = true
		case targetexec.OutcomeClientGone:
			res.clientGone = true
		}
		if res.clientGone {
			break
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
			logx.Warnf("[proto=%s model=%s] target %d (%s/%s) context overflow; retrying with larger-context targets", proto, exposed, ti, t.Provider, t.Model)
			ordered = result.Retried
			ti = -1 // restart at the first replacement target (post-statement ti++ → 0)
			continue
		}
		logx.Warnf("[proto=%s model=%s] target %d (%s/%s) failed; trying next", proto, exposed, ti, t.Provider, t.Model)
	}
	// Record the target set actually considered this pass (post scheduling /
	// request-aware narrowing / context retry) so forward's cooldown + TOCTOU
	// decisions key on what was really in play, not the original route targets.
	res.effectiveTargets = ordered
	return res
}

// serveRequest is the stable input to one scheduling/failover pass. It groups
// request identity, protocol/body data, routing inputs, and the runtime snapshot
// instead of threading them as an ever-growing positional parameter list.
type serveRequest struct {
	runtime Snapshot

	proto       string
	upPath      string
	exposed     string
	calledModel string
	sessionKey  string
	agent       string
	requestID   string

	targets   []RouteTarget
	routeKeys map[string]bool
	force     bool // active pin: exclusive and bypasses circuit health
	// forcedProvider is the request-scoped replay override. It is exclusive for
	// routing, but unlike a pin it does not bypass circuit health.
	forcedProvider string
	cacheKey       string
	origBody       []byte

	writer  http.ResponseWriter
	request *http.Request
}

// PublishTerminalEvent emits a live "end" event for a request that ends before
// the normal start/commit flow — a malformed body (400) or an unrouted model
// (502). Without it, an agent retry-looping on a missing/removed model is
// invisible to the live monitor, defeating the feature's core use case. The
// requestID is generated at the handler top and threaded in so these terminal
// events still pair with a stable id (the contract: 400/502 终局也必须产生 end
// 且带稳定 request_id).
func PublishTerminalEvent(events *observeevents.Hub, requestID string, r *http.Request, proto, exposed string, status int) {
	events.Publish(observeevents.Event{
		Type:      "end",
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     counters.DetectAgent(r),
		Protocol:  proto,
		Exposed:   exposed,
		Status:    status,
	})
}

// ForcedProviderFromRequest extracts the HTTP boundary value used by replay.
// Header wins over query. The policy package receives only the resulting value.
func ForcedProviderFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	if value := request.Header.Get("x-mp-force-provider"); value != "" {
		return value
	}
	if request.URL == nil {
		return ""
	}
	return request.URL.Query().Get("force_provider")
}

// expandFusionResponses mirrors forward's responses-state expansion for one
// fusion sub-call body (panel leg / judge / synthesizer): it applies only when
// the client spoke the responses protocol AND this leg's backend is stateless
// (backendProto != responses) — a native-responses backend keeps
// previous_response_id passthrough and its own server-side chain. On expansion
// failure the UNEXPANDED body is sent: a broken chain degrades context but must
// not kill the whole run (forward fails closed per-target; fusion has no
// per-leg target list to fall through). Returns the body to send plus the
// merged history for post-response recording (nil when not applicable).
//
// It lives here (not fusion.go) so fusion.go stays free of the protocol
// import: conversion mechanics belong to targetexec.Plan.
func (p pipeline) expandFusionResponses(fc fusionCtx, backendProto string, body []byte) ([]byte, []any) {
	if fc.proto != "responses" || backendProto == "responses" || p.svc.ResponsesState == nil {
		return body, nil
	}
	expanded, history, hit, err := p.svc.ResponsesState.Expand(body, fc.sessionKey)
	if err != nil {
		logx.Warnf("[fusion] %s: responses state expansion failed: %v — sending unexpanded body", fc.flc.Exposed, err)
		return body, nil
	}
	if protocol.PreviousResponseID(body) != "" && !hit {
		logx.Infof("[fusion] %s: previous_response_id cache miss; repaired orphaned continuation items", fc.flc.Exposed)
	}
	return expanded, history
}
