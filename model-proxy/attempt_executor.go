package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	responsecache "model-proxy/internal/cache"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
)

// attemptState is the narrow mutable-runtime port required by target execution.
// Keeping it separate from Proxy prevents the target pipeline from acquiring
// new scheduler, reload, or Web dependencies by reaching into the god object.
type attemptState interface {
	modelLocked(provider, model string, now time.Time) bool
	takeHalfOpenSlot(name string, generations ...uint64) bool
	releaseHalfOpenSlot(name string, generations ...uint64)
	recordSuccess(name, model string, generations ...uint64)
	recordFailure(name string, sched Scheduling, generations ...uint64)
	recordModelFailure(provider, model string, sched Scheduling, generations ...uint64)
	recordRateLimit(name string, until time.Time, kind rateLimitKind, generations ...uint64)
	parseRateLimit(resp *http.Response, bodyPeek []byte, now time.Time, sched Scheduling) (time.Time, rateLimitKind)
	learnParamBlock(provider, model, param string, generations ...uint64) bool
	applyParamBlock(provider, model string, body []byte) []byte
	noteWireResponsesMiss(name string)
}

var _ attemptState = (*Proxy)(nil)

// attemptExecutor owns the complete one-target I/O pipeline. Its collaborators
// are explicit so request execution no longer has unrestricted access to Proxy.
// The executor is a cheap request-local value; all pointers reference
// concurrency-safe, process-owned components.
type attemptExecutor struct {
	attemptState

	client         *http.Client
	metrics        *metricsStore
	tokens         *tokenCounter
	agents         *agentCounter
	reqLog         *requestLogger
	responsesState *protocol.ResponsesStateStore
	events         *observeevents.Hub
}

func (p *Proxy) targetExecutor() attemptExecutor {
	return attemptExecutor{
		attemptState:   p,
		client:         p.client,
		metrics:        p.metrics,
		tokens:         p.tokens,
		agents:         p.agents,
		reqLog:         p.reqLog,
		responsesState: p.responsesState,
		events:         p.events,
	}
}

// execute sends the request to one target, with a 401-refresh retry and an
// upstream timeout. It writes the response to w and returns committed=true once
// committed (2xx or non-failover 4xx). committed=false signals failover
// (connection error, timeout, 401 after refresh, 5xx, 429, or a build/auth
// error). When ctxRetry is non-nil and the upstream answers a context-overflow
// 400 (see isContextOverflow), execute instead counts a failover and returns
// ctxRetry()'s strictly-larger-context replacement targets as `retried` (nil
// when no larger target exists → the peeked 400 commits unchanged). It updates
// the provider's health on success/failure/rate-limit and enforces half-open
// single-flight. Failover/retarget only happen before any bytes are written
// to w. A final committed response additionally returns attemptCommit so the
// orchestration layer can run post-commit work without an executor back-reference
// to Proxy.
// tryOutcome classifies a failed (non-committed) target attempt, for the
// terminal-status decision in forward: a request whose failures are ALL
// cooldown-flavored ends as 429 (+Retry-After); any hard failure makes it 502.
type tryOutcome int

const (
	tryNone        tryOutcome = iota // not attempted (locked / unavailable / no info)
	tryFailedHard                    // conn error / timeout / 401-after-refresh / 5xx / build / auth / model-denied class
	tryRateLimited                   // 429
)

// attemptCommit exposes the only post-commit datum that orchestration needs:
// the exact upstream request bytes after provider rewrite and retry shaping.
// Shadow policy, lifecycle admission, and dispatch stay outside the executor.
type attemptCommit struct {
	requestBody []byte
}

func (p attemptExecutor) execute(attempt targetAttempt) (committed bool, retried []RouteTarget, outcome tryOutcome, commit *attemptCommit) {
	runtime := attempt.runtime
	plan := attempt.plan
	exchange := attempt.exchange
	scope := attempt.scope
	policy := attempt.policy

	cfg := runtime.cfg
	generation := runtime.generation
	cache := runtime.cache
	proto := plan.clientProto
	backendProto := plan.backendProto
	t := plan.target
	prov := plan.providerCfg
	provImpl := plan.providerImpl
	baseURL := plan.baseURL
	upPath := plan.upPath
	body := exchange.body
	w := exchange.writer
	r := exchange.request
	calledModel := scope.calledModel
	agent := scope.agent
	cacheKey := scope.cacheKey
	flc := scope.log
	ctxRetry := policy.contextRetry
	force := policy.force
	lastTarget := policy.lastTarget
	viaResponsesVerdict := plan.viaResponsesVerdict
	r2c := scope.responseContext
	responsesHistory := scope.responsesHistory
	responsesSession := scope.responsesSession

	// Wrap the client writer to capture time-to-first-token for latency stats.
	// All writes below go through tw; ttft is read on the commit path.
	tw := newTimingResponseWriter(w)
	w = tw
	sched := cfg.Scheduling
	// Fail CLOSED on a missing runtime implementation: nil impl would silently
	// skip AuthHeaders/RewriteRequest and ship an UNAUTHENTICATED request
	// upstream (pooled parent leaking through, provider not logged in). This is
	// a build/config failure, not a provider failure — no circuit, no model lock.
	if provImpl == nil {
		log.Printf("[proto=%s provider=%s] no runtime provider implementation (not logged in / unresolved pooled parent) — failing closed", proto, t.Provider)
		if p.metrics != nil {
			p.metrics.inc(t.Provider, t.Model, evFailovers)
		}
		return false, nil, tryFailedHard, nil
	}
	// Model-level lockout: schedule() already filters locked (provider, model)
	// pairs; this is the race guard for locks recorded after scheduling. Checked
	// BEFORE takeHalfOpenSlot so a locked model never burns the half-open probe.
	// force (pin / x-mp-force-provider) bypasses — the user asked for THIS target.
	if !force && p.modelLocked(t.Provider, t.Model, time.Now()) {
		return false, nil, tryNone, nil
	}
	// Re-check availability and reserve the half-open probe slot if needed. A pin
	// (force) bypasses the circuit breaker — the user explicitly asked for THIS
	// backend, so circuit-open state must not block it (and there's no failover
	// target anyway). recordSuccess on a forced hit reopens the circuit.
	if !force && !p.takeHalfOpenSlot(t.Provider, generation) {
		return false, nil, tryNone, nil
	}
	// recordSuccess/Failure/RateLimit below release the slot (force never took
	// one, so those releases are harmless no-ops).

	ctx, cancel := context.WithTimeout(r.Context(), sched.Timeout())
	defer cancel()

	// Two independent one-shot retries live in this loop: 401 → auth-refresh
	// retry (attempt-gated), and 400 unsupported-parameter → strip-retry
	// (flag-gated below, rewinds `attempt` so it never consumes the 401 slot).
	strippedParam := false
	retriedImages := false
	clientWantsStream := protocol.WantsStream(body)
	for attempt := 0; attempt < 2; attempt++ {
		targetURL := strings.TrimRight(baseURL, "/") + upPath
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}
		// Provider-specific URL/body tweaks (store:false, ?beta=true, ...).
		if provImpl != nil {
			targetURL, body = provImpl.RewriteRequest(targetURL, body, upPath)
		}
		// Strip previously learned unsupported top-level parameters (#4) —
		// after RewriteRequest, before Content-Length is derived from the body.
		body = p.applyParamBlock(t.Provider, t.Model, body)

		req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			log.Printf("[proto=%s provider=%s] build upstream req: %v", proto, t.Provider, err)
			p.releaseHalfOpenSlot(t.Provider, generation)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false, nil, tryFailedHard, nil
		}
		copyHeaderWhitelist(req.Header, r.Header,
			"content-type", "accept", "user-agent", "x-session-id",
			"user_id", "x-claude-code-session-id", "x-interaction-type", "x-interaction-id",
			"prompt_cache_key", "x-anthropic-billing-header", "anthropic-beta", "accept-language")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if provImpl != nil {
			if err := provImpl.AuthHeaders(req); err != nil {
				log.Printf("[proto=%s provider=%s] auth error: %v", proto, t.Provider, err)
				p.releaseHalfOpenSlot(t.Provider, generation)
				if p.metrics != nil {
					p.metrics.inc(t.Provider, t.Model, evFailovers)
				}
				return false, nil, tryFailedHard, nil
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
			p.recordFailure(t.Provider, sched, generation) // connection error / timeout → circuit
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailures)
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false, nil, tryFailedHard, nil
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
			p.recordFailure(t.Provider, sched, generation)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false, nil, tryFailedHard, nil
		}
		// Rate limit (429): skip this provider until the upstream's reset hint /
		// Retry-After / per-class default backoff. Does not count toward the circuit.
		// The body is peeked (≤8KiB) for classification only — 429s never commit.
		if resp.StatusCode == 429 {
			peek := peekResponseBody(resp, 8<<10)
			until, kind := p.parseRateLimit(resp, peek, time.Now(), sched)
			resp.Body.Close()
			p.recordRateLimit(t.Provider, until, kind, generation)
			if kind != rlTransient {
				log.Printf("[proto=%s provider=%s] 429 classified %s — skipped until %s", proto, t.Provider, kind, until.Format(time.RFC3339))
			}
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evRateLimited429)
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false, nil, tryRateLimited, nil
		}
		// Transient upstream errors → circuit + failover.
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			p.recordFailure(t.Provider, sched, generation)
			if p.metrics != nil {
				p.metrics.inc(t.Provider, t.Model, evFailures)
				p.metrics.inc(t.Provider, t.Model, evFailovers)
			}
			return false, nil, tryFailedHard, nil
		}

		// 4xx classification (ONE peek, ≤64KiB, bytes restored transparently):
		// context-overflow → one-shot larger-context retry (ctxRetry); model-level
		// failure → model lockout + failover. Everything else falls through to
		// the normal commit: the client receives the upstream's 4xx unchanged
		// (peeked bytes included).
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			var peek []byte
			if ctxRetry != nil || resp.StatusCode == 400 || resp.StatusCode == 403 || resp.StatusCode == 404 {
				peek = peekResponseBody(resp, contextOverflowPeek)
			}
			if resp.StatusCode == http.StatusRequestEntityTooLarge && !retriedImages {
				if smaller, changed := protocol.ShrinkImages(body, 1<<20, 2048); changed {
					resp.Body.Close()
					body = smaller
					retriedImages = true
					attempt--
					log.Printf("[proto=%s provider=%s] 413 request too large — compressed inline images and retrying once",
						proto, t.Provider)
					continue
				}
			}
			// Context-overflow retry: a 4xx whose body matches an upstream "prompt
			// exceeds the context window" error is NOT committed while this request
			// still has its one retry — count a failover (NO circuit failure: the
			// provider is healthy, the request just doesn't fit the model) and
			// return the larger-context replacement list for forward to retarget.
			if ctxRetry != nil && isContextOverflow(resp.StatusCode, peek) {
				if bigger := ctxRetry(); len(bigger) > 0 {
					resp.Body.Close()
					p.releaseHalfOpenSlot(t.Provider, generation)
					if p.metrics != nil {
						p.metrics.inc(t.Provider, t.Model, evFailovers)
					}
					log.Printf("[proto=%s provider=%s] context overflow (status %d); retrying on a larger-context target",
						proto, t.Provider, resp.StatusCode)
					return false, bigger, tryNone, nil
				}
			}
			// Model-level failure: any 404 (the proxy only forwards known LLM
			// paths, so an upstream 404 means the model/path is gone) or a
			// 400/403 model-denied body. Lock ONLY (provider, model) — the
			// account may serve its other models fine. On the last target the
			// error still commits unchanged (the client deserves the real
			// 404/400, not an opaque 502), but the lock is recorded so the NEXT
			// request fails over / skips immediately.
			// INTENTIONAL — 404/4xx failover is deliberate, see
			// AGENTS.md「会被误认为是 bug 的设计」#2/#3.
			if resp.StatusCode == 404 || isModelDenied(resp.StatusCode, peek) {
				// Wire-verdict 404 correction: this request was converted to
				// /responses because the probe verdict said the endpoint
				// supports it — a 404 here means the VERDICT was wrong, not
				// the model. Flip the verdict (persisted; later requests use
				// chat) and skip recordModelFailure so the model lock doesn't
				// take the blame for our protocol choice.
				if viaResponsesVerdict && resp.StatusCode == 404 {
					p.noteWireResponsesMiss(t.Provider)
					log.Printf("[proto=%s provider=%s] /responses 404 after wire verdict — provider responses downgraded to no (model NOT locked), failing over",
						proto, t.Provider)
				} else {
					p.recordModelFailure(t.Provider, t.Model, sched, generation)
					if !lastTarget {
						log.Printf("[proto=%s provider=%s] model %s unavailable upstream (status %d) — model locked %s, failing over",
							proto, t.Provider, t.Model, resp.StatusCode, sched.ModelLockoutDuration())
					}
				}
				if !lastTarget {
					resp.Body.Close()
					p.releaseHalfOpenSlot(t.Provider, generation)
					if p.metrics != nil {
						p.metrics.inc(t.Provider, t.Model, evFailovers)
					}
					return false, nil, tryFailedHard, nil
				}
			}
			// Unsupported-parameter learning: a 400 naming an offending top-level
			// parameter teaches the provider's blocklist. When THIS request
			// carries the parameter, strip it and retry the same target once
			// immediately (rewinding `attempt` so the retry doesn't consume the
			// 401 slot); otherwise just learn — the next request strips it
			// preemptively via applyParamBlock.
			// INTENTIONAL — mutating the client's request is deliberate and
			// narrowly gated, see AGENTS.md「会被误认为是 bug 的设计」#4.
			if resp.StatusCode == 400 && !strippedParam {
				if param, ok := parseUnsupportedParam(peek); ok {
					isNew := p.learnParamBlock(t.Provider, t.Model, param, generation)
					if nb, did := stripTopLevelParam(body, param); did {
						resp.Body.Close()
						strippedParam = true
						body = nb
						log.Printf("[proto=%s provider=%s] 400 unsupported parameter %q — stripped, retrying",
							proto, t.Provider, param)
						attempt--
						continue
					}
					if isNew {
						log.Printf("[proto=%s provider=%s] learned unsupported parameter %q (stripped on future requests)",
							proto, t.Provider, param)
					}
				}
			}
		}

		// Empty-200 preflight: a 2xx announcing a zero-length body is a broken
		// upstream response (reverse-engineered gateways do this under load), not
		// a client error — committing it would hang the agent's turn. Classified
		// MODEL-level (the account may serve other models fine): fail over when
		// another target remains; on the last target the response still commits.
		// INTENTIONAL — treating success as failure here is deliberate, see
		// AGENTS.md「会被误认为是 bug 的设计」#1.
		if resp.StatusCode < 300 && resp.Header.Get("Content-Length") == "0" {
			p.recordModelFailure(t.Provider, t.Model, sched, generation)
			if !lastTarget {
				resp.Body.Close()
				p.releaseHalfOpenSlot(t.Provider, generation)
				if p.metrics != nil {
					p.metrics.inc(t.Provider, t.Model, evFailovers)
				}
				log.Printf("[proto=%s provider=%s] empty 200 (Content-Length: 0) — model locked %s, failing over",
					proto, t.Provider, sched.ModelLockoutDuration())
				return false, nil, tryFailedHard, nil
			}
		}

		// Commit: stream this response (2xx or non-failover 4xx).
		// recordSuccess only for 2xx — 4xx (400/403/404) are client errors that
		// shouldn't reset the circuit breaker (a persistently-403 provider is broken).
		if resp.StatusCode < 300 {
			p.recordSuccess(t.Provider, t.Model, generation)
		} else {
			// …but a 4xx commit still RELEASES the half-open probe slot
			// (release-neutral: failure history untouched) — otherwise a provider
			// whose half-open probe gets a 4xx keeps halfOpenInFlight=true
			// FOREVER and starves (P0-4).
			p.releaseHalfOpenSlot(t.Provider, generation)
		}
		log.Printf("[proto=%s provider=%s] %s %s model=%s→%s status=%s %dms bytes=%d",
			proto, t.Provider, r.Method, r.URL.Path, calledModel, t.Model,
			statusColor(resp.StatusCode, fmt.Sprintf("%d", resp.StatusCode)),
			time.Since(start).Milliseconds(), len(body))
		// Conversion flag: when the backend protocol differs from the client's,
		// the response body is rewritten (#11), so its length changes — drop the
		// backend's content-length / transfer-encoding (can't forward a length for
		// a body we're about to transform; Go's server would reject the mismatch).
		convert := protocol.NeedsConversion(proto, backendProto)
		// Non-streaming conversion runs BEFORE we commit the status/headers so a
		// conversion failure fails CLOSED (502 to the client) instead of sending
		// the backend's body in the wrong protocol after the upstream's 2xx status.
		// recordSuccess above already released the half-open slot and marked the
		// provider healthy — the upstream DID succeed; a convert failure is ours,
		// not the provider's. The streaming path converts lazily after WriteHeader
		// (below); its errors surface mid-stream and can't be pre-empted.
		//
		// Some upstreams (codex, live-verified) stream SSE with an EMPTY
		// content-type. Sniff the framing before committing to the buffered
		// non-stream path — a JSON body never starts with event:/data:.
		// The sniff also feeds usageScanner / responses-state recording on the
		// passthrough path, so it runs for same-protocol traffic too.
		streamBySniff := resp.StatusCode < 300 && !isSSE(resp.Header) && sniffSSEFraming(resp)
		upstreamIsStream := resp.StatusCode < 300 && (isSSE(resp.Header) || streamBySniff)
		modeMismatch := convert && resp.StatusCode < 300 && clientWantsStream != upstreamIsStream
		transformed := convert || modeMismatch
		clientOutputIsStream := upstreamIsStream
		if modeMismatch {
			clientOutputIsStream = clientWantsStream
		}
		var preconv []byte // converted non-stream body; nil unless pre-converted here
		if modeMismatch || (convert && !upstreamIsStream) {
			// 64 MiB cap: a non-stream LLM response larger than this is pathological
			// (max_tokens bounds it); the cap bounds memory on the buffered convert.
			all, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if rerr != nil {
				log.Printf("[proto=%s provider=%s] %s→%s convert read failed: %v — failing closed",
					proto, t.Provider, backendProto, proto, rerr)
				http.Error(w, fmt.Sprintf("upstream response read failed during %s→%s conversion", backendProto, proto), http.StatusBadGateway)
				return true, nil, tryNone, nil
			}
			conv, cerr := all, error(nil)
			switch {
			case resp.StatusCode >= 400 && convert:
				conv, cerr = protocol.ConvertErrorResponse(all, proto, backendProto, resp.StatusCode)
			case upstreamIsStream && !clientWantsStream:
				if convert {
					convertedSSE, readErr := io.ReadAll(protocol.ConvertSSE(bytes.NewReader(all), proto, backendProto, t.Model, r2c))
					if readErr != nil {
						cerr = readErr
					} else {
						all = convertedSSE
					}
				}
				if cerr == nil {
					conv, cerr = protocol.AggregateSSE(all, proto)
				}
			case !upstreamIsStream && clientWantsStream:
				if convert {
					conv, cerr = protocol.ConvertResponse(all, proto, backendProto, r2c)
				}
				if cerr == nil {
					conv, cerr = protocol.ResponseToSSE(conv, proto)
				}
			case convert:
				conv, cerr = protocol.ConvertResponse(all, proto, backendProto, r2c)
			}
			if cerr != nil {
				log.Printf("[proto=%s provider=%s] %s→%s convert response failed: %v — failing closed (would return wrong-protocol body)",
					proto, t.Provider, backendProto, proto, cerr)
				http.Error(w, fmt.Sprintf("response conversion %s→%s failed", backendProto, proto), http.StatusBadGateway)
				return true, nil, tryNone, nil
			}
			preconv = conv
			if resp.StatusCode < 300 && proto == "responses" && p.responsesState != nil && len(responsesHistory) > 0 {
				if clientWantsStream {
					p.responsesState.RecordSSE(responsesSession, responsesHistory, preconv)
				} else {
					p.responsesState.RecordJSON(responsesSession, responsesHistory, preconv)
				}
			}
		}
		for k, vs := range resp.Header {
			if transformed && (strings.EqualFold(k, "content-length") || strings.EqualFold(k, "transfer-encoding")) {
				continue
			}
			if modeMismatch && strings.EqualFold(k, "content-type") {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if modeMismatch {
			if clientWantsStream {
				w.Header().Set("content-type", "text/event-stream")
			} else {
				w.Header().Set("content-type", "application/json")
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
		// see client-protocol bytes. Non-streaming bodies were already converted
		// above (preconv, before WriteHeader, fail-closed on error); streaming is
		// converted lazily here via a stateful SSE transformer.
		if convert {
			if preconv != nil {
				body = io.NopCloser(bytes.NewReader(preconv))
			} else if isSSE(resp.Header) || streamBySniff {
				body = io.NopCloser(protocol.ConvertSSE(body, proto, backendProto, t.Model, r2c))
			}
		}
		if proto == "responses" && p.responsesState != nil && len(responsesHistory) > 0 &&
			preconv == nil && upstreamIsStream && resp.StatusCode < 300 {
			state := p.responsesState
			session := responsesSession
			history := responsesHistory
			body = newCaptureReader(body, protocol.ResponsesStateCaptureLimit, func(captured []byte, _ int64, truncated bool) {
				if !truncated {
					state.RecordSSE(session, history, captured)
				}
			})
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
					proto:       string(proto),
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
		if p.tokens != nil && clientOutputIsStream {
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
		var cacheCapture *responsecache.Recorder
		if cache != nil && cacheKey != "" && resp.StatusCode < 300 {
			cacheCapture = responsecache.NewRecorder(body, cache.MaxBodyBytes())
			body = cacheCapture
		}
		// Count streamed bytes for the empty-200 postmortem below.
		// INTENTIONAL — recording a failure AFTER a committed 200 is deliberate,
		// see AGENTS.md「会被误认为是 bug 的设计」#1.
		counting := &countingReadCloser{rc: body}
		body = counting
		end := flushCopy(w, body)
		body.Close()
		// Only a CLEAN EOF with zero bytes — client still attached — proves an
		// empty upstream body. A client disconnect (streamClientGone) or an
		// upstream read error (streamUpstreamErr) also yields n==0 but is NOT a
		// model failure (P1-1c).
		if resp.StatusCode < 300 && counting.n == 0 && end == streamEOF && r.Context().Err() == nil {
			p.recordModelFailure(t.Provider, t.Model, sched, generation)
			log.Printf("[proto=%s provider=%s] 200 with zero-byte body — model locked %s (post-commit; next request fails over)",
				proto, t.Provider, sched.ModelLockoutDuration())
		}
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
			p.agents.addLatency(agent, t.Provider, t.Model, uint64(upstreamMs))
		}
		// Store the exact-match cache entry for this 2xx response — only when the
		// capture is COMPLETE: not truncated by the size cap, AND sawEOF (the body
		// streamed to a clean end). A client disconnect mid-stream leaves sawEOF
		// false, so a half-read response is never cached as complete.
		if cacheCapture != nil && cacheCapture.Complete() && len(cacheCapture.Body()) > 0 {
			// The cached body is the CLIENT-protocol body the recorder captured,
			// so the stored header must describe THAT body: a converted response
			// drops the backend's Content-Length/Transfer-Encoding, and a mode
			// mismatch carries the content-type the live path sent (see
			// HeaderForCapturedBody). Replaying the upstream's original values
			// would corrupt the response or mislabel its framing.
			header := responsecache.HeaderForCapturedBody(resp.Header, convert, modeMismatch, clientWantsStream)
			cache.Put(cacheKey, resp.StatusCode, header, cacheCapture.Body(), time.Now())
		}
		// Live request monitor (#6): announce the completed request (agent,
		// route, chosen provider, status, latency, best-effort tokens).
		p.events.Publish(observeevents.Event{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     flc.requestID,
			Agent:         agent,
			Protocol:      string(proto),
			Exposed:       flc.exposed,
			Provider:      t.Provider,
			UpstreamModel: t.Model,
			Status:        resp.StatusCode,
			LatencyMs:     latencyMs,
			Input:         endTokens.Input,
			Output:        endTokens.Output,
		})
		return true, nil, tryNone, &attemptCommit{requestBody: reqBytes}
	}
	// 401-retry exhausted without resolution — release the slot.
	// Defensive guard: unreachable in normal flow (the 401 branch above always
	// returns or continues on attempt 0), kept for safety.
	p.releaseHalfOpenSlot(t.Provider, generation)
	if p.metrics != nil {
		p.metrics.inc(t.Provider, t.Model, evFailovers)
	}
	return false, nil, tryFailedHard, nil
}
