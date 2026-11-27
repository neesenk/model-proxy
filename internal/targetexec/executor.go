package targetexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"model-proxy/internal/observe/logx"
	"net/http"
	"net/url"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/transport/bodycapture"
)

// authErrHTTPStatus extracts the HTTP status an auth-layer failure actually
// observed (provider auth endpoints signal it by satisfying
// interface{ HTTPStatus() int } — see provider.HTTPStatusError). Behavioral
// errors.As keeps this file free of provider imports (the architecture
// contract pins executor.go to execution leaves). 0 = no observed status.
func authErrHTTPStatus(err error) int {
	var s interface{ HTTPStatus() int }
	if errors.As(err, &s) {
		return s.HTTPStatus()
	}
	return 0
}

// Doer is the subset of http.Client used by one target execution.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// State is the generation-frozen mutable runtime adapter. Its methods never
// accept a generation: the composition root binds that concern once.
type State interface {
	ModelLocked(configdomain.RouteTarget, time.Time) bool
	TakeHalfOpenSlot(string) bool
	ReleaseHalfOpenSlot(string)
	RecordSuccess(configdomain.RouteTarget)
	RecordFailure(string)
	RecordModelFailure(configdomain.RouteTarget)
	RecordRateLimit(string, RateLimitDecision)
	// RecordCredentialDenied reports an account attribution the executor just
	// proved (CredentialDenialKind) with the upstream evidence — the record
	// the operator surfaces read. Called exactly where failure(target, false)
	// accounts an auth-class failure; never for any other failure class.
	RecordCredentialDenied(provider string, kind CredentialDenialKind, evidence []byte)
	// RecordGatewayRefusal counts one gateway refusal (an edge/WAF 403 —
	// provider.GatewayRefused — on the wire or at the auth layer) towards the
	// account's circuit and nothing else: never a credential record, never a
	// failure metric. Called exactly where noteGatewayRefusal accounts it.
	RecordGatewayRefusal(provider string)
	LearnParamBlock(configdomain.RouteTarget, string) bool
	ApplyParamBlock(configdomain.RouteTarget, []byte) []byte
	NoteWireResponsesMiss(configdomain.RouteTarget)
	// TakeInflight registers this request against the account and reports
	// whether it may be dispatched under the account's learned concurrency
	// limit. The slot is taken either way — a caller that dispatches
	// regardless (a pin is a hard choice) really is running against the
	// account — and is released exactly once with ReleaseInflight.
	TakeInflight(provider string) (InflightSlot, bool)
	// ReleaseInflight returns a slot TakeInflight handed out with how the
	// request ended (InflightOutcome). Only an upstream verdict moves an
	// inferred cap's probe-up streak, and raised reports that this release
	// lifted the cap to raisedTo.
	ReleaseInflight(slot InflightSlot, outcome InflightOutcome) (raisedTo int, raised bool)
	// RecordConcurrencyLimit reports the concurrency limit a 429 proved
	// (RateLimitDecision's concurrency fields) and returns the cap in force
	// afterwards, changed when this report moved it. It replaces
	// RecordRateLimit for a RateLimitConcurrency decision: that limit has no
	// time horizon to cool down for.
	RecordConcurrencyLimit(provider string, decision RateLimitDecision) (cap int, changed bool)
}

// InflightSlot identifies one request's occupancy of an account from dispatch
// to completion. Level is how many OTHER requests the account was already
// running when the slot was taken — the request's vantage point on the cap.
type InflightSlot struct {
	Provider string
	Level    int
}

// InflightOutcome is how the request that held a slot ended, as the account's
// concurrency ledger sees it (the shape of runtime.InflightOutcome; the app
// gate maps one onto the other).
type InflightOutcome uint8

const (
	// InflightNoVerdict: the upstream was never asked to admit the request —
	// a pre-dispatch gate refused it, or the caller walked away.
	InflightNoVerdict InflightOutcome = iota
	// InflightFailed: dispatched, and the upstream did not answer 2xx.
	InflightFailed
	// InflightSucceeded: dispatched, and the upstream answered 2xx.
	InflightSucceeded
)

// AttemptDTO is the stable, leaf-owned description passed to optional effects.
type AttemptDTO struct {
	Target               configdomain.RouteTarget
	Protocol             protocol.Protocol
	Request              *http.Request
	Body                 []byte
	Response             *http.Response
	Started              time.Time
	UpstreamMilliseconds int64
	TotalMilliseconds    int64
	TTFTMilliseconds     int64
	// BodyReads counts the distinct body reads served to the client — the
	// streamed-vs-bulk arrival shape for the generation-window classification
	// (stats.GenerationWindowMs).
	BodyReads int
	Usage     Usage
	Scope     Scope
}

// Usage is deliberately independent of root metrics/token types.
type Usage struct{ Input, Output, CacheRead, CacheCreation uint64 }

// Effects bridge application-owned logging, metrics, live events and usage
// observation. Nil Effects is safe and means no application side effects.
type Effects interface {
	Failover(configdomain.RouteTarget)
	Failure(configdomain.RouteTarget)
	RateLimited(configdomain.RouteTarget)
	LogAttempt(AttemptDTO)
	CaptureResponse(io.ReadCloser, AttemptDTO) io.ReadCloser
	CaptureUsage(io.ReadCloser, AttemptDTO, func(Usage)) io.ReadCloser
	Committed(AttemptDTO)
}

// Responses records client-facing Responses API output for continuation.
type Responses interface {
	RecordJSON(session string, history []any, body []byte) bool
	RecordSSE(session string, history []any, body []byte) bool
}

type Executor struct {
	Client    Doer
	State     State
	Effects   Effects
	Responses Responses
}

func (executor Executor) Execute(attempt Attempt) Result {
	runtime, plan, exchange, scope, policy := attempt.Runtime(), attempt.Plan(), attempt.Exchange(), attempt.Scope(), attempt.Policy()
	target, providerImpl := plan.Target(), plan.Provider()
	timedWriter := newTimingResponseWriter(exchange.Writer)
	exchange.Writer = timedWriter
	if providerImpl == nil {
		logx.Warnf("[proto=%s provider=%s] no runtime provider implementation (not logged in / unresolved pooled parent) — failing closed",
			plan.ClientProtocol(), target.Provider)
		executor.failover(target)
		return Result{Outcome: OutcomeFailedHard}
	}
	now := time.Now()
	if executor.State != nil && !policy.Force && executor.State.ModelLocked(target, now) {
		return Result{Outcome: OutcomeSkipped, SkipReason: "model locked", AttemptBody: []byte("skipped before dispatch: model locked (earlier zero-byte 200)")}
	}
	// Occupancy: from here until Execute returns — stream finished, failed or
	// the client left — this is one of the account's in-flight requests. The
	// deferred release runs on every exit path, a panic included; a leaked
	// slot would read as permanent saturation. The gate sits before the
	// half-open one, so a refused request never takes a probe slot it would
	// have to give back. Before this gate the request was simply sent, the
	// upstream answered 429, and the account was benched for a whole
	// transient cooldown although its slots were freeing within seconds.
	// outcome is what the release will report. It stays "no verdict" while
	// nothing has been sent — a refusal by either gate below says nothing
	// about the account's concurrency limit — and becomes a failure the
	// moment the request is dispatched: from there anything but a 2xx is the
	// upstream not admitting it.
	var slot InflightSlot
	outcome := InflightNoVerdict
	if executor.State != nil {
		var free bool
		slot, free = executor.State.TakeInflight(target.Provider)
		defer func() {
			if raisedTo, raised := executor.State.ReleaseInflight(slot, outcome); raised {
				logx.Infof("[proto=%s provider=%s] concurrency cap raised to %d — full-occupancy successes probe upward", plan.ClientProtocol(), target.Provider, raisedTo)
			}
		}()
		if !free && !policy.Force {
			return Result{Outcome: OutcomeSkipped, SkipReason: "concurrency saturated", AttemptBody: []byte("skipped before dispatch: concurrency saturated (learned limit reached)")}
		}
	}
	if executor.State != nil && !policy.Force && !executor.State.TakeHalfOpenSlot(target.Provider) {
		return Result{Outcome: OutcomeSkipped, SkipReason: "circuit open (no half-open probe slot)", AttemptBody: []byte("skipped before dispatch: circuit open (no half-open probe slot)")}
	}

	ctx, cancel := context.WithTimeout(exchange.Request.Context(), runtime.Scheduling.Timeout())
	defer cancel()
	body := exchange.Body
	strippedParam, renamedRole, adaptedThinking, retriedImages, retriedConn := false, false, false, false, false
	clientWantsStream := protocol.WantsStream(body)
	for authAttempt := 0; authAttempt < 2; authAttempt++ {
		targetURL := strings.TrimRight(plan.BaseURL(), "/") + plan.UpstreamPath()
		if exchange.Request.URL.RawQuery != "" {
			targetURL += "?" + stripInternalQuery(exchange.Request.URL.RawQuery)
		}
		targetURL, body = providerImpl.RewriteRequest(targetURL, body, plan.UpstreamPath())
		if executor.State != nil {
			body = executor.State.ApplyParamBlock(target, body)
		}
		req, err := http.NewRequestWithContext(ctx, exchange.Request.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			logx.Warnf("[proto=%s provider=%s] build upstream req: %v", plan.ClientProtocol(), target.Provider, err)
			executor.release(target.Provider)
			executor.failover(target)
			return Result{Outcome: OutcomeFailedHard, AttemptBody: []byte(err.Error())}
		}
		copyHeaderWhitelist(req.Header, exchange.Request.Header, upstreamHeaderWhitelist...)
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		if err := providerImpl.AuthHeaders(req); err != nil {
			// A key mint refused by the gateway (an edge/WAF 403 page in the
			// auth response) is the same network-path refusal as on the wire:
			// counted towards the circuit, never towards the credential.
			// Anything else at the auth layer keeps its meaning (an expired
			// session is the operator's to fix).
			if authErrHTTPStatus(err) == http.StatusForbidden && gatewayRefusalText(err.Error()) {
				logx.Warnf("[proto=%s provider=%s] gateway refused at the auth layer: %v", plan.ClientProtocol(), target.Provider, err)
				executor.noteGatewayRefusal(target)
			} else {
				logx.Warnf("[proto=%s provider=%s] auth error: %v", plan.ClientProtocol(), target.Provider, err)
			}
			executor.failover(target)
			executor.release(target.Provider)
			// Auth-layer failure: no upstream-model response exists, but the
			// auth endpoint's observed status (e.g. aqp mint 401 "Session
			// expired") is extractable — record it so the failover leg shows
			// the real status instead of the synthetic 502, with the error
			// text as the body evidence. 0 (network/parse failure, no
			// observed status) keeps forward's 502 fallback.
			return Result{Outcome: OutcomeFailedHard, AttemptStatus: authErrHTTPStatus(err), AttemptBody: []byte(err.Error())}
		}
		if err := plan.ApplyConfiguredHeaders(req.Header); err != nil {
			logx.Warnf("[proto=%s provider=%s] header resolve error: %v", plan.ClientProtocol(), target.Provider, err)
			executor.release(target.Provider)
			executor.failover(target)
			return Result{Outcome: OutcomeFailedHard, AttemptBody: []byte(err.Error())}
		}
		providerImpl.ExtraHeaders(req, body, scope.Log.SessionID, plan.UpstreamPath())
		started := time.Now()
		outcome = InflightFailed
		response, err := executor.Client.Do(req)
		upstreamMS := time.Since(started).Milliseconds()
		if err != nil {
			// The caller walked away (client disconnect / caller deadline), not
			// the upstream misbehaving: the request context is the caller-owned
			// one, and our own upstream-timeout ctx is a child of it, so a live
			// caller context here means the abort came from the caller side.
			// Client cancellations must not poison the circuit breaker or be
			// retried against the remaining targets.
			if exchange.Request.Context().Err() != nil {
				outcome = InflightNoVerdict // the caller left: the upstream said nothing
				executor.release(target.Provider)
				return Result{Outcome: OutcomeClientGone}
			}
			// Fast-fail connection error (EOF/reset/refused, returned in
			// milliseconds): one same-target redispatch on a fresh connection
			// before counting the failure. External-path hiccups (proxy chain
			// recycle, LB roll) kill existing connections while new ones work —
			// the redispatch absorbs the blip invisibly and, on success, the
			// first attempt never reaches RecordFailure, so the breaker's
			// "consecutive failures" keeps meaning "confirmed unreachable on a
			// fresh connection". Like the param-strip family, the redispatch
			// does not consume the 401 refresh slot. Timeouts never take this
			// path (their time budget is already spent).
			if IsFastFailConnError(err) && !retriedConn {
				retriedConn = true
				logx.Infof("[proto=%s provider=%s] fast-fail connection error (%v) — redispatching once on a fresh connection",
					plan.ClientProtocol(), target.Provider, err)
				authAttempt--
				continue
			}
			logx.Warnf("[proto=%s provider=%s] upstream error: %v", plan.ClientProtocol(), target.Provider, err)
			executor.failure(target, true)
			return Result{Outcome: OutcomeFailedHard, AttemptStatus: http.StatusBadGateway, AttemptBody: []byte(err.Error())}
		}
		dto := AttemptDTO{
			Target:               target,
			Protocol:             plan.ClientProtocol(),
			Request:              exchange.Request,
			Body:                 body,
			Response:             response,
			Started:              started,
			UpstreamMilliseconds: upstreamMS,
			Scope:                scope,
		}
		if response.StatusCode == http.StatusUnauthorized {
			if authAttempt == 0 {
				logx.Debugf("[proto=%s provider=%s] 401, refreshing auth", plan.ClientProtocol(), target.Provider)
				if refreshErr := providerImpl.Refresh(); refreshErr == nil {
					response.Body.Close()
					continue
				} else {
					logx.Warnf("[proto=%s provider=%s] auth refresh failed: %v", plan.ClientProtocol(), target.Provider, refreshErr)
				}
			}
			// The upstream 401 body is the failure evidence (bearer rejected,
			// tenant mismatch, session expired) — same bounded peek discipline
			// as the 429/5xx branches. Without it the failover leg serializes
			// an empty response body and the request log shows a bare 401.
			peek := peekResponseBody(response, 8<<10)
			response.Body.Close()
			executor.credentialDenied(target, DenialAuth, peek)
			return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: peek}
		}
		if response.StatusCode == http.StatusTooManyRequests {
			peek := peekResponseBody(response, 8<<10)
			response.Body.Close()
			decision := ParseRateLimitFor(response, peek, time.Now(), runtime.Scheduling, slot.Level)
			if executor.State != nil {
				executor.noteRateLimit(plan, target, decision)
			}
			executor.rateLimited(target)
			return Result{Outcome: OutcomeRateLimited, AttemptStatus: response.StatusCode, AttemptBody: peek}
		}
		if response.StatusCode >= 500 {
			peek := peekResponseBody(response, 8<<10)
			response.Body.Close()
			// A 5xx whose body proves a CONTENT refusal (safety filter /
			// response protection / moderation) is request-attributable, not a
			// provider fault: fail over to the next target (another provider's
			// policy may serve the same request) but never tick the circuit
			// breaker or failure metric — the account is healthy (magpie's
			// content-refusal bucket; mis-reading it once silently benched all
			// its accounts for days).
			if IsContentRefusal(peek) {
				logx.Infof("[proto=%s provider=%s] %d proved a content refusal — failing over without cooldown accounting",
					plan.ClientProtocol(), target.Provider, response.StatusCode)
				executor.release(target.Provider)
				executor.failover(target)
				return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: peek}
			}
			executor.failure(target, true)
			return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: peek}
		}
		if response.StatusCode >= 400 {
			var peek []byte
			if policy.ContextRetry != nil || response.StatusCode == 400 || response.StatusCode == 402 || response.StatusCode == 403 || response.StatusCode == 404 || response.StatusCode == 422 {
				peek = peekResponseBody(response, contextOverflowPeek)
			}
			// kimi-code answers the exhausted 5-hour coding-plan window with
			// 403 "You've reached your 5-hour usage limit", and step-plan
			// answers the exhausted Credit 月池 with 402 "quota_exceeded" -
			// quota exhaustion proven by the body on a non-429 status must take
			// the same reactive cooldown + failover path as 429, or the route
			// keeps committing the raw upstream denial (no cooldown, no
			// failover); the quota poller only gates scheduling on the ultimate
			// window.
			if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusPaymentRequired {
				if decision, ok := ParseQuotaDenied(response, peek, time.Now(), runtime.Scheduling); ok {
					response.Body.Close()
					if executor.State != nil {
						executor.State.RecordRateLimit(target.Provider, decision)
						logx.Infof("[proto=%s provider=%s] %d body proves %s exhaustion - skipped until %s",
							plan.ClientProtocol(), target.Provider, response.StatusCode, decision.Kind, decision.Until.Format(time.RFC3339))
					}
					executor.rateLimited(target)
					return Result{Outcome: OutcomeRateLimited}
				}
			}
			// A lapsed plan subscription denies the ACCOUNT — every model,
			// every request shape — yet step-plan spells it as a plain 400
			// with a generic error type, so neither a quota marker nor an
			// account code vouches for it (2026-11-25: 11 of these were
			// committed as terminal 400s while both sibling accounts served
			// the same route). Same accounting as an exhausted 401: fail over
			// — the last candidate included, the account error is never
			// passed through — tick the circuit, skip the failure metric, and
			// leave a credential denial for the operator surfaces.
			if accountUnsubscribed(peek) {
				response.Body.Close()
				logx.Infof("[proto=%s provider=%s] %d body proves the account has no active plan subscription — failing over (auth circuit)",
					plan.ClientProtocol(), target.Provider, response.StatusCode)
				executor.credentialDenied(target, DenialPlanInactive, peek)
				return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: peek}
			}
			if response.StatusCode == http.StatusRequestEntityTooLarge && !retriedImages {
				if smaller, changed := protocol.ShrinkImages(body, 1<<20, 2048); changed {
					response.Body.Close()
					body = smaller
					retriedImages = true
					authAttempt--
					continue
				}
			}
			if policy.ContextRetry != nil && IsContextOverflow(response.StatusCode, peek) {
				if larger := policy.ContextRetry(); len(larger) > 0 {
					response.Body.Close()
					executor.release(target.Provider)
					executor.failover(target)
					return Result{Retried: larger}
				}
			}
			if response.StatusCode == http.StatusNotFound || IsModelDenied(response.StatusCode, peek) {
				if plan.ViaResponsesVerdict() && response.StatusCode == http.StatusNotFound {
					if executor.State != nil {
						executor.State.NoteWireResponsesMiss(target)
					}
				} else if executor.State != nil {
					executor.State.RecordModelFailure(target)
				}
				if !policy.LastTarget {
					response.Body.Close()
					executor.release(target.Provider)
					executor.failover(target)
					return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: peek}
				}
			} else if response.StatusCode == http.StatusForbidden {
				if accountDisabled(peek) {
					response.Body.Close()
					executor.credentialDenied(target, DenialAccountDisabled, peek) // Auth failure, like an exhausted 401.
					return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: []byte("upstream denied the account")}
				}
				if gatewayRefused(response.Header, peek) {
					// An edge/WAF refusal is about the path this client is
					// calling from, not the key: no credential record. It
					// counts towards the circuit on its own streak (a run of
					// refusals means the gateway is refusing this client).
					executor.noteGatewayRefusal(target)
					if !policy.LastTarget {
						response.Body.Close()
						executor.release(target.Provider)
						executor.failover(target)
						return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: []byte("upstream gateway refused the request")}
					}
					// The last candidate: the raw 403 stays the client's
					// answer (the ordinary commit below releases the
					// half-open slot).
				}
			}
			if requestShapeStatus(response.StatusCode) && !strippedParam {
				if parameter, ok := ParseUnsupportedParam(peek); ok {
					if executor.State != nil {
						executor.State.LearnParamBlock(target, parameter)
					}
					if shaped, changed := StripTopLevelParam(body, parameter); changed {
						response.Body.Close()
						body = shaped
						strippedParam = true
						authAttempt--
						continue
					}
				}
			}
			// Same learning-retry family, different lesson: an upstream whose
			// chat dialect rejects the developer role (400/422 naming the role
			// or zhipu's generic role wording) learns ParamDeveloperRole and
			// the request retries once with developer renamed to system. Like
			// the param strip, this only fires after THIS upstream actually
			// rejected the role — OpenAI-proper backends that accept developer
			// are never rewritten.
			if requestShapeStatus(response.StatusCode) && !renamedRole && IsDeveloperRoleRejection(response.StatusCode, peek) {
				if shaped, changed := RenameDeveloperRole(body); changed {
					if executor.State != nil {
						executor.State.LearnParamBlock(target, ParamDeveloperRole)
					}
					response.Body.Close()
					body = shaped
					renamedRole = true
					authAttempt--
					continue
				}
			}
			// Budget-based → adaptive thinking, same self-healing shape:
			// shopee's claude gateway 400s thinking.type.enabled on the
			// adaptive-only models; retry once with {"type":"adaptive"}.
			if response.StatusCode == http.StatusBadRequest && !adaptedThinking && IsThinkingDialectRejection(response.StatusCode, peek) {
				if shaped, changed := RewriteThinkingAdaptive(body); changed {
					if executor.State != nil {
						executor.State.LearnParamBlock(target, ParamThinkingAdaptive)
					}
					response.Body.Close()
					body = shaped
					adaptedThinking = true
					authAttempt--
					continue
				}
			}
		}
		// 2xx carrying an HTML page is a relay/WAF fake success (login wall,
		// challenge page) — committing it streams markup to a JSON/SSE client.
		// Reinterpret as 502 so it fails over and cools down like any upstream
		// failure; unlike the zero-byte case this is provider-level (the
		// gateway is answering, just not with an API reply), so it never
		// reaches RecordModelFailure. Two arms, both pre-commit-safe, and both
		// close the body on their way out (this return skips the commit path,
		// the only other closer — a leaked body pins its connection and read
		// loop): Content-Type text/html settles from headers alone, covering
		// streamed requests without touching the body — a blocking read here
		// would defeat the SSE keepalive contract for slow-TTFT streams, so
		// even the 2 KiB title peek is taken for non-stream callers only; the
		// document-prefix peek likewise runs only for non-stream callers,
		// whose body read is about to happen anyway.
		if response.StatusCode < 300 {
			if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") {
				var peek []byte
				if !clientWantsStream {
					peek = peekResponseBody(response, 2<<10)
				}
				response.Body.Close()
				executor.fakeAPIReply(target, plan, peek)
				return Result{Outcome: OutcomeFailedHard, AttemptStatus: http.StatusBadGateway, AttemptBody: fakeAPIReplyBody(response.StatusCode, peek)}
			}
			if !clientWantsStream {
				if head := peekHead(response, 64); IsFakeAPIReply("", head) {
					response.Body.Close()
					executor.fakeAPIReply(target, plan, head)
					return Result{Outcome: OutcomeFailedHard, AttemptStatus: http.StatusBadGateway, AttemptBody: fakeAPIReplyBody(response.StatusCode, head)}
				}
			}
		}
		if response.StatusCode < 300 && response.Header.Get("Content-Length") == "0" {
			if executor.State != nil {
				executor.State.RecordModelFailure(target)
			}
			if !policy.LastTarget {
				response.Body.Close()
				executor.release(target.Provider)
				executor.failover(target)
				return Result{Outcome: OutcomeFailedHard, AttemptStatus: response.StatusCode, AttemptBody: []byte("upstream 2xx carried a zero-length body")}
			}
		}
		if response.StatusCode < 300 {
			// A 2xx is the upstream admitting the request under the load it
			// saw: for the concurrency cap this release counts as a success.
			outcome = InflightSucceeded
			if executor.State != nil {
				executor.State.RecordSuccess(target)
			}
		} else {
			executor.release(target.Provider)
		}
		return executor.commit(ctx, runtime.Scheduling.Keepalive(), plan, exchange, scope, dto, timedWriter, clientWantsStream)
	}
	executor.release(target.Provider)
	executor.failover(target)
	return Result{Outcome: OutcomeFailedHard}
}

func (executor Executor) commit(
	ctx context.Context,
	keepalive time.Duration,
	plan Plan,
	exchange Exchange,
	scope Scope,
	dto AttemptDTO,
	timedWriter *timingResponseWriter,
	clientWantsStream bool,
) Result {
	response := dto.Response
	if executor.Effects != nil {
		executor.Effects.LogAttempt(dto)
	}
	convert := protocol.NeedsConversion(plan.ClientProtocol(), plan.BackendProtocol())
	streamBySniff := response.StatusCode < 300 && !isSSE(response.Header) && sniffSSEFraming(response)
	upstreamStream := response.StatusCode < 300 && (isSSE(response.Header) || streamBySniff)
	modeMismatch := convert && response.StatusCode < 300 && clientWantsStream != upstreamStream
	transformed := convert || modeMismatch
	clientStream := upstreamStream
	if modeMismatch {
		clientStream = clientWantsStream
	}
	// Response model normalization (provider alias): when the target's real
	// upstream model differs from the called name, upstreams echo THEIR model
	// in the response and clients learn an id they never called. The rewrite
	// applies to the final client-facing bytes (before logging/usage);
	// passthrough byte-identical.
	normalizeModel := responseModelNormalization(plan, scope)
	var converted []byte
	if modeMismatch || (convert && !upstreamStream) {
		all, readErr := readCapped(response.Body, maxConvertBufferBytes)
		response.Body.Close()
		if readErr != nil {
			logx.Warnf("[proto=%s provider=%s] %s→%s convert read failed: %v — failing closed",
				plan.ClientProtocol(), dto.Target.Provider, plan.BackendProtocol(), plan.ClientProtocol(), readErr)
			http.Error(
				exchange.Writer,
				fmt.Sprintf("upstream response read failed during %s→%s conversion", plan.BackendProtocol(), plan.ClientProtocol()),
				http.StatusBadGateway,
			)
			return Result{Committed: true}
		}
		converted, readErr = convertBuffered(all, response.StatusCode, convert, upstreamStream, clientWantsStream, plan, scope)
		if readErr != nil {
			logx.Warnf("[proto=%s provider=%s] %s→%s convert response failed: %v — failing closed (would return wrong-protocol body)",
				plan.ClientProtocol(), dto.Target.Provider, plan.BackendProtocol(), plan.ClientProtocol(), readErr)
			http.Error(
				exchange.Writer,
				fmt.Sprintf("response conversion %s→%s failed", plan.BackendProtocol(), plan.ClientProtocol()),
				http.StatusBadGateway,
			)
			return Result{Committed: true}
		}
		if response.StatusCode < 300 && plan.ClientProtocol() == protocol.Responses && executor.Responses != nil && len(scope.ResponsesHistory) > 0 {
			if clientWantsStream {
				executor.Responses.RecordSSE(scope.ResponsesSession, scope.ResponsesHistory, converted)
			} else {
				executor.Responses.RecordJSON(scope.ResponsesSession, scope.ResponsesHistory, converted)
			}
		}
	}
	// Same-protocol non-stream passthrough normally streams the body through
	// untouched; an alias target instead buffers it once so the model field can
	// be normalized before commit (a read failure fails closed, exactly like
	// the buffered conversion above).
	if normalizeModel != "" && !transformed && response.StatusCode < 300 && !upstreamStream {
		all, readErr := readCapped(response.Body, maxConvertBufferBytes)
		response.Body.Close()
		if readErr != nil {
			logx.Warnf("[proto=%s provider=%s] response read for model normalization failed: %v — failing closed",
				plan.ClientProtocol(), dto.Target.Provider, readErr)
			http.Error(
				exchange.Writer,
				"upstream response read failed during model normalization",
				http.StatusBadGateway,
			)
			return Result{Committed: true}
		}
		// The body is already consumed and closed, so it must be served from
		// the buffer either way; only a CHANGED body drops Content-Length.
		normalized := protocol.NormalizeResponseModel(all, plan.ClientProtocol(), normalizeModel)
		if !bytes.Equal(normalized, all) {
			transformed = true
		}
		converted = normalized
	}
	// SSE normalization rewrites frame bytes in flight, so any upstream
	// Content-Length no longer describes the client body — treat the stream as
	// transformed for header purposes (strip length headers here and in the
	// transformed response headers).
	if normalizeModel != "" && response.StatusCode < 300 && upstreamStream {
		transformed = true
	}
	// Heartbeat frames are bytes the upstream body never had, so a forwarded
	// Content-Length would understate the client body; a keepalive stream
	// drops the length header exactly like a transformed one. (clientStream
	// already implies a <300 status.)
	heartbeat := clientStream && keepalive > 0
	// Hop-by-hop headers belong to ONE transport connection, never to the
	// client (RFC 9110 §7.6.1): strip Connection (plus every header it names),
	// Trailer and the other connection-scoped tokens. HTTP/2 upstreams never
	// send them; HTTP/1.1 upstreams sometimes do, and forwarding them to the
	// client corrupts connection handling.
	connectionNamed := map[string]bool{}
	for _, connectionValue := range response.Header.Values("Connection") {
		for _, token := range strings.Split(connectionValue, ",") {
			if named := strings.TrimSpace(token); named != "" {
				connectionNamed[strings.ToLower(named)] = true
			}
		}
	}
	for key, values := range response.Header {
		lower := strings.ToLower(key)
		if hopByHopHeaders[lower] || connectionNamed[lower] {
			continue
		}
		if (transformed || heartbeat) && (strings.EqualFold(key, "content-length") || strings.EqualFold(key, "transfer-encoding")) {
			continue
		}
		if modeMismatch && strings.EqualFold(key, "content-type") {
			continue
		}
		for _, value := range values {
			exchange.Writer.Header().Add(key, value)
		}
	}
	if modeMismatch {
		if clientWantsStream {
			exchange.Writer.Header().Set("content-type", "text/event-stream")
		} else {
			exchange.Writer.Header().Set("content-type", "application/json")
		}
	}
	exchange.Writer.WriteHeader(response.StatusCode)
	body := response.Body
	if convert {
		if converted != nil {
			body = io.NopCloser(bytes.NewReader(converted))
		} else if upstreamStream {
			// The converter only reads; Close must still reach the upstream
			// body (io.NopCloser would drop it — connection reuse lost and the
			// net/http body contract broken).
			body = &convertedStreamBody{reader: protocol.ConvertSSE(response.Body, plan.ClientProtocol(), plan.BackendProtocol(), plan.Target().Model, scope.ResponseContext), source: response.Body}
		}
	} else if converted != nil {
		body = io.NopCloser(bytes.NewReader(converted))
	}
	if normalizeModel != "" && response.StatusCode < 300 && converted == nil && upstreamStream {
		body = protocol.NormalizeSSEModelStream(body, plan.ClientProtocol(), normalizeModel)
	}
	if plan.ClientProtocol() == protocol.Responses && executor.Responses != nil && len(scope.ResponsesHistory) > 0 && converted == nil && upstreamStream && response.StatusCode < 300 {
		responses := executor.Responses
		body = bodycapture.New(body, protocol.ResponsesStateCaptureLimit, func(captured []byte, _ int64, truncated bool) {
			if !truncated {
				responses.RecordSSE(scope.ResponsesSession, scope.ResponsesHistory, captured)
			}
		})
	}
	// Same-protocol passthrough streams bypass the converters, so an upstream
	// that abandons a generation mid-stream used to reach the client as a bare
	// EOF: state-tracking clients (Anthropic SDKs) hard-fail, lenient ones
	// silently accept a truncated answer. The watcher forwards bytes untouched
	// while shadow-scanning for the client protocol's terminal sequence and,
	// when the upstream ends without one, appends a protocol-native terminal
	// error event — the same fail-closed shape the converters already emit
	// (never a fake finish_reason / [DONE] / message_stop). It wraps BELOW the
	// effects chain (request log and usage capture see the
	// client's bytes)
	// and ABOVE the model normalizer and responses-state capture, which must
	// observe the raw upstream stream.
	//
	// A CONVERTED stream is terminated fail-closed by its converter (an error
	// terminal in the client protocol), so it needs nothing appended — but its
	// aborts need the same attribution, and the converters' only log is
	// convertWarn: deduped per process per message, with neither provider nor
	// cause. On 2026-11-24 thirteen converted streams (openai clients reading an
	// anthropic upstream) ended mid-generation and the whole day's serve.log
	// evidence was one deduped line. The observer runs the same shadow scanner
	// over the client-protocol bytes the converter produced and only reports.
	var terminalWatch *protocol.TerminalWatcher
	convertedStream := convert && upstreamStream && converted == nil
	if convertedStream {
		terminalWatch = protocol.ObserveStreamTerminal(body, plan.ClientProtocol())
		body = terminalWatch
	} else if !convert && upstreamStream {
		terminalWatch = protocol.WatchStreamTerminal(body, plan.ClientProtocol())
		body = terminalWatch
	}
	// The wrapper order intentionally mirrors the root pipeline: transformed
	// client bytes are captured for continuation, then logging, usage and client.
	// Usage capture wraps EVERY response body: the scanner counts SSE usage on
	// streamed bodies and falls back to a whole-body JSON usage parse on
	// non-stream ones (counters.UsageScanner) — gating on clientStream used to
	// drop all non-stream token accounting (decisions is 100% non-stream).
	if executor.Effects != nil {
		body = executor.Effects.CaptureResponse(body, dto)
		body = executor.Effects.CaptureUsage(body, dto, func(usage Usage) {
			dto.Usage = usage
		})
	}
	counting := &countingReadCloser{ReadCloser: body}
	// The upstream-timeout context bounds the upstream Read only. A peer that
	// keeps its connection but stops reading parks the copy loop in the
	// client Write, where that context cannot reach it (2026-11-26: two
	// handlers were held for 8h+ until a restart's forced close). Every
	// client write of this response takes the same deadline, plus the tail
	// grace, on the connection — the handler cannot outlive the request's
	// budget.
	if deadline, ok := ctx.Deadline(); ok {
		timedWriter.writeDeadline = clientWriteDeadline(deadline, deadline.Sub(dto.Started))
	}
	// SSE keepalive: for client-facing event streams, write a comment
	// heartbeat after `keepalive` of upstream silence so client/edge idle
	// timeouts don't cut slow-TTFT streams. The heartbeat starts only after
	// the upstream response headers committed above, so the pre-headers
	// failover window stays intact; the frames bypass the recording chain
	// and the ttft marker. Non-stream responses must never see a comment
	// frame (it would corrupt a JSON body).
	var end streamEnd
	if heartbeat {
		// response.Body is the raw source at the bottom of body's wrapper
		// chain — flushCopyHeartbeat closes it to unplug a parked helper Read
		// when the client goes away (see the transport comment).
		end = flushCopyHeartbeat(exchange.Writer, counting, response.Body, keepalive)
	} else {
		end = flushCopy(exchange.Writer, counting)
	}
	body.Close()
	if response.StatusCode < 300 && counting.Count == 0 && end == streamEOF && exchange.Request.Context().Err() == nil {
		if executor.State != nil {
			executor.State.RecordModelFailure(dto.Target)
		}
		logx.Warnf("[proto=%s provider=%s] 200 with zero-byte body — model locked post-commit; next request fails over",
			plan.ClientProtocol(), dto.Target.Provider)
	}
	if end == streamWriteStalled {
		// The peer never closed or reset anything — it stopped reading, and
		// a client write sat on its full buffers until the request's deadline
		// abandoned the stream. Decided ahead of the watcher switch: the
		// reader goroutine was idle while the loop was parked, so the watcher
		// observed no upstream end and none of its branches apply — the old
		// plain "client disconnected" line sent the 2026-11-26 audit to the
		// connection owner instead of the stopped reader.
		logx.Warnf("[proto=%s provider=%s] client stopped reading — a client write blocked past the upstream timeout; stream abandoned; not an upstream fault",
			plan.ClientProtocol(), dto.Target.Provider)
	} else if terminalWatch != nil {
		switch {
		case terminalWatch.Synthesized():
			// Attribution: the watcher only sees that the upstream body ended
			// without a terminal — not WHY. A caller that walks away cancels the
			// caller-owned request context, and that interrupt reaches the parked
			// upstream Read as a context error shaped exactly like a provider
			// abandoning its generation. The 2026-11-16 logs carried 132 such
			// warns, most of them client-side subagent aborts, each claiming an
			// upstream fault (and delivery to a client that was already gone) —
			// misattributed evidence for every later incident investigation.
			switch {
			case end == streamClientGone || exchange.Request.Context().Err() != nil:
				logx.Infof("[proto=%s provider=%s] client disconnected mid-stream — synthesized terminal not delivered; not an upstream fault",
					plan.ClientProtocol(), dto.Target.Provider)
			case ctx.Err() != nil:
				logx.Warnf("[proto=%s provider=%s] scheduling timeout ended the stream before its terminal sequence — synthesized terminal delivered",
					plan.ClientProtocol(), dto.Target.Provider)
			default:
				logx.Warnf("[proto=%s provider=%s] upstream stream ended before its terminal sequence — synthesized a protocol-native error terminal for the client (delivered)",
					plan.ClientProtocol(), dto.Target.Provider)
			}
		case terminalWatch.UpstreamErrorTerminal() && convertedStream:
			// The converter appended its fail-closed terminal: the upstream read
			// failed, the upstream sent an error event, or the request context
			// was canceled under the parked Read. Same three-way attribution as
			// the synthesized case above — a client that walked away is not an
			// upstream fault — which the converters cannot make themselves.
			switch {
			case end == streamClientGone || exchange.Request.Context().Err() != nil:
				logx.Infof("[proto=%s provider=%s] client disconnected mid-stream — converted error terminal not delivered; not an upstream fault",
					plan.ClientProtocol(), dto.Target.Provider)
			case ctx.Err() != nil:
				logx.Warnf("[proto=%s provider=%s] scheduling timeout ended the converted stream — error terminal delivered",
					plan.ClientProtocol(), dto.Target.Provider)
			default:
				logx.Warnf("[proto=%s provider=%s] converted stream (%s→%s) ended with an error terminal — delivered to the client: %s",
					plan.ClientProtocol(), dto.Target.Provider, plan.BackendProtocol(), plan.ClientProtocol(), terminalWatch.UpstreamErrorEvidence())
			}
		case terminalWatch.UpstreamErrorTerminal():
			// The upstream itself failed the generation after commit. Its error
			// terminal is the client's fail-closed signal (forwarded byte-identical,
			// never re-synthesized), but the request log keeps the exchange as a
			// plain status 200. This line is that evidence; the excerpt is bounded
			// and single-line by the watcher. (The live shape on 2026-11-22 —
			// forty streams ending in "upstream stream terminated unexpectedly" —
			// is a CONVERTED stream and lands in the branch above.)
			logx.Warnf("[proto=%s provider=%s] upstream error terminal ended the stream — forwarded to the client as-is, nothing synthesized: %s",
				plan.ClientProtocol(), dto.Target.Provider, terminalWatch.UpstreamErrorEvidence())
		case end == streamClientGone && !terminalWatch.TerminalSeen():
			// The client-side write failed while the upstream was still
			// streaming. The watcher only reports an upstream end it observed
			// (EOF/read error); here the copy loop returned before any arrived —
			// buffered upstream data trips the sticky write error first — so
			// Synthesized() stays false and the branch above never fires. 51 such
			// aborts on 2026-11-22 (35 cut by a restart's drain deadline, 16
			// client disconnects) left no trace in serve.log. A terminal already
			// seen means the upstream HAD finished and only trailing bytes were
			// lost — not a mid-stream cut, and it stays unlogged.
			logx.Infof("[proto=%s provider=%s] client disconnected mid-stream before its terminal — nothing synthesized; not an upstream fault",
				plan.ClientProtocol(), dto.Target.Provider)
		}
	}
	dto.TotalMilliseconds = time.Since(dto.Started).Milliseconds()
	dto.TTFTMilliseconds = dto.TotalMilliseconds
	if timedWriter.hasFirstByte {
		dto.TTFTMilliseconds = timedWriter.firstByte.Sub(dto.Started).Milliseconds()
	}
	dto.BodyReads = counting.Reads
	if executor.Effects != nil {
		executor.Effects.Committed(dto)
	}
	return Result{Committed: true}
}

// maxConvertBufferBytes caps the buffered conversion read. A body strictly
// larger than this fails closed instead of being silently truncated — a
// truncated JSON would convert into a corrupt "successful" response.
const maxConvertBufferBytes = 64 << 20

// readCapped reads at most limit bytes; a longer body is an error (probe with
// limit+1 so truncation is detected, never silently accepted).
func readCapped(reader io.Reader, limit int64) ([]byte, error) {
	all, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(all)) > limit {
		return nil, fmt.Errorf("body exceeds the %d MiB conversion buffer", limit>>20)
	}
	return all, nil
}

// convertedStreamBody pairs a converted client byte stream with the upstream
// body it was derived from: Close releases the upstream connection exactly
// once. The buffered-conversion path (converted != nil) closes the upstream
// body itself right after reading, so only the streaming leg needs this.
type convertedStreamBody struct {
	reader io.Reader
	source io.Closer
}

func (c *convertedStreamBody) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *convertedStreamBody) Close() error {
	if closer, ok := c.reader.(io.Closer); ok {
		_ = closer.Close()
	}
	return c.source.Close()
}

func convertBuffered(all []byte, status int, convert, upstreamStream, clientWantsStream bool, plan Plan, scope Scope) ([]byte, error) {
	if status >= 400 && convert {
		return protocol.ConvertErrorResponse(all, plan.ClientProtocol(), plan.BackendProtocol(), status)
	}
	normalizeModel := ""
	if status < 300 {
		normalizeModel = responseModelNormalization(plan, scope)
	}
	if upstreamStream && !clientWantsStream {
		if convert {
			var err error
			all, err = io.ReadAll(protocol.ConvertSSE(bytes.NewReader(all), plan.ClientProtocol(), plan.BackendProtocol(), plan.Target().Model, scope.ResponseContext))
			if err != nil {
				return nil, err
			}
		}
		aggregated, err := protocol.AggregateSSE(all, plan.ClientProtocol())
		if err != nil {
			return nil, err
		}
		return protocol.NormalizeResponseModel(aggregated, plan.ClientProtocol(), normalizeModel), nil
	}
	if !upstreamStream && clientWantsStream {
		var err error
		if convert {
			all, err = protocol.ConvertResponse(all, plan.ClientProtocol(), plan.BackendProtocol(), scope.ResponseContext)
			if err != nil {
				return nil, err
			}
		}
		// Normalize the JSON BEFORE synthesizing SSE so every stamped frame
		// carries the called model.
		all = protocol.NormalizeResponseModel(all, plan.ClientProtocol(), normalizeModel)
		return protocol.ResponseToSSE(all, plan.ClientProtocol())
	}
	if convert {
		converted, err := protocol.ConvertResponse(all, plan.ClientProtocol(), plan.BackendProtocol(), scope.ResponseContext)
		if err != nil {
			return nil, err
		}
		return protocol.NormalizeResponseModel(converted, plan.ClientProtocol(), normalizeModel), nil
	}
	return all, nil
}

// responseModelNormalization reports the client-facing model name when the
// target's real upstream model differs from the called (exposed) name — the
// provider-alias case. "" means no rewrite: same-name targets keep the
// zero-copy passthrough with zero parsing overhead.
func responseModelNormalization(plan Plan, scope Scope) string {
	called := scope.CalledModel
	if called == "" || plan.Target().Model == "" || plan.Target().Model == called {
		return ""
	}
	return called
}

func (executor Executor) release(provider string) {
	if executor.State != nil {
		executor.State.ReleaseHalfOpenSlot(provider)
	}
}

// internalQueryKeys are proxy control parameters consumed at the front door
// (forcedProviderFromRequest). They address the PROXY, not the upstream API:
// forwarding ?force_provider=x leaks an internal knob to third parties and
// strict-argument upstreams reject the request outright.
var internalQueryKeys = map[string]bool{"force_provider": true}

// upstreamHeaderWhitelist is the client → upstream header pass-through list.
// Everything else (Cookie, Authorization, tracing/idempotency headers, ...) is
// dropped: client credentials and ambient headers must not leak to a different
// provider than the one they were issued for. Per entry:
//
//   - content-type / accept: payload and response-shape negotiation
//   - user-agent: some backends gate on client identity (codex originator checks)
//   - anthropic-beta: multi-valued beta-capability declarations (see
//     copyHeaderWhitelist — values are Add-ed, never collapsed)
//   - x-claude-code-session-id / user_id / x-session-id /
//     x-interaction-type / x-interaction-id: session + agent attribution the
//     backends echo for prompt-cache warmth and abuse accounting
//   - x-opencode-session: the client may set OpenCode Go's session-affinity
//     header itself; forwarding it makes the "explicit client value always
//     wins" rule in OpenCodeGoProvider.ExtraHeaders reachable on the forward
//     path (opaque session id, not a credential)
//   - prompt_cache_key: upstream prompt-cache shard selection
//   - x-anthropic-billing-header: billing-plan attribution on anthropic bases
//   - accept-language: locale-dependent model behavior
var upstreamHeaderWhitelist = []string{
	"content-type",
	"accept",
	"user-agent",
	"x-session-id",
	"user_id",
	"x-claude-code-session-id",
	"x-interaction-type",
	"x-interaction-id",
	"x-opencode-session",
	"prompt_cache_key",
	"x-anthropic-billing-header",
	"anthropic-beta",
	"accept-language",
}

// hopByHopHeaders must never be forwarded from the upstream response to the
// client (RFC 9110 §7.6.1); headers named by the Connection header are
// stripped alongside them.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"upgrade":             true,
}

// stripInternalQuery removes internal control keys from a raw query string.
// When no internal key is present the ORIGINAL bytes pass through untouched —
// same-protocol requests keep byte-transparent queries; only requests that
// actually used an internal knob get re-encoded.
func stripInternalQuery(rawQuery string) string {
	parsed, err := url.ParseQuery(rawQuery)
	if err != nil {
		// Unparseable query: pass through rather than corrupt it. The front
		// door's Query().Get would not have found the key either.
		return rawQuery
	}
	found := false
	for key := range parsed {
		if internalQueryKeys[strings.ToLower(key)] {
			found = true
			parsed.Del(key)
		}
	}
	if !found {
		return rawQuery
	}
	return parsed.Encode()
}
func (executor Executor) failover(target configdomain.RouteTarget) {
	if executor.Effects != nil {
		executor.Effects.Failover(target)
	}
}
func (executor Executor) failure(target configdomain.RouteTarget, observeFailure bool) {
	if executor.State != nil {
		executor.State.RecordFailure(target.Provider)
	}
	if executor.Effects != nil && observeFailure {
		executor.Effects.Failure(target)
	}
	executor.failover(target)
}

// credentialDenied is failure(target, false) for the account attributions:
// the auth-class accounting (circuit tick, failover effect, no failure
// metric) plus the credential denial the operator surfaces read — a dead
// account must not hide behind its siblings carrying the traffic. evidence is
// the upstream's own error body (already bounded by the caller's peek).
func (executor Executor) credentialDenied(target configdomain.RouteTarget, kind CredentialDenialKind, evidence []byte) {
	if executor.State != nil {
		executor.State.RecordCredentialDenied(target.Provider, kind, evidence)
	}
	executor.failure(target, false)
}

// noteGatewayRefusal is the accounting of an edge/WAF refusal (gatewayRefused
// on the wire, gatewayRefusalText at the auth layer): one tick on the
// account's gateway-refusal streak, which opens the circuit like a hard
// failure, only later. Deliberately NOT failure(): no failure metric, no
// quality sample, no hard-failure streak, and never credentialDenied — the
// refusal says nothing about the key. The failover effect stays with the
// caller: a last-candidate refusal is committed, not failed over.
func (executor Executor) noteGatewayRefusal(target configdomain.RouteTarget) {
	if executor.State != nil {
		executor.State.RecordGatewayRefusal(target.Provider)
	}
}

// fakeAPIReply records the provider-level failure for a 2xx-that-was-HTML:
// one circuit-breaker tick + failure metric + failover effect, exactly the
// 5xx accounting, so the gateway stops receiving dispatches while it keeps
// answering with pages.
func (executor Executor) fakeAPIReply(target configdomain.RouteTarget, plan Plan, peek []byte) {
	title := HTMLTitle(peek)
	if title == "" {
		title = "html document"
	}
	logx.Warnf("[proto=%s provider=%s] 2xx answered an HTML page (%q) — reinterpreted as 502 for failover and cooldown",
		plan.ClientProtocol(), target.Provider, title)
	executor.failure(target, true)
}

// fakeAPIReplyBody builds the bounded diagnostic carried on the failover leg
// (request-log attempt row / terminal error): status + page title, never the
// page itself.
func fakeAPIReplyBody(status int, peek []byte) []byte {
	title := HTMLTitle(peek)
	if title == "" {
		title = "html document"
	}
	return []byte(fmt.Sprintf("upstream answered %d with an HTML page: %s", status, title))
}

// noteRateLimit hands a 429's classification to the state port. A concurrency
// limit updates the account's cap instead of starting a cooldown, and the
// move is named in serve.log when it changed something — an unchanged cap is
// the steady state under load and stays quiet.
func (executor Executor) noteRateLimit(plan Plan, target configdomain.RouteTarget, decision RateLimitDecision) {
	if decision.Kind == RateLimitConcurrency {
		cap, changed := executor.State.RecordConcurrencyLimit(target.Provider, decision)
		if !changed {
			return
		}
		if decision.ConcurrencyLimit > 0 {
			logx.Infof("[proto=%s provider=%s] 429 states a concurrency limit of %d — dispatch capped",
				plan.ClientProtocol(), target.Provider, decision.ConcurrencyLimit)
			return
		}
		logx.Infof("[proto=%s provider=%s] 429 with %d other request(s) in flight — concurrency cap lowered to %d",
			plan.ClientProtocol(), target.Provider, decision.ConcurrencyInflight, cap)
		return
	}
	executor.State.RecordRateLimit(target.Provider, decision)
	if decision.Kind != "" && decision.Kind != RateLimitTransient {
		logx.Infof("[proto=%s provider=%s] 429 classified %s — skipped until %s",
			plan.ClientProtocol(), target.Provider, decision.Kind, decision.Until.Format(time.RFC3339))
	}
}

func (executor Executor) rateLimited(target configdomain.RouteTarget) {
	if executor.Effects != nil {
		executor.Effects.RateLimited(target)
		executor.Effects.Failover(target)
	}
}
