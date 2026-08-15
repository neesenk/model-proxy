package targetexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/transport/bodycapture"
)

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
	LearnParamBlock(configdomain.RouteTarget, string) bool
	ApplyParamBlock(configdomain.RouteTarget, []byte) []byte
	NoteWireResponsesMiss(string)
}

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
	Usage                Usage
	Scope                Scope
}

// Usage is deliberately independent of root metrics/token types.
type Usage struct{ Input, Output uint64 }

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
		log.Printf("[proto=%s provider=%s] no runtime provider implementation (not logged in / unresolved pooled parent) — failing closed",
			plan.ClientProtocol(), target.Provider)
		executor.failover(target)
		return Result{Outcome: OutcomeFailedHard}
	}
	now := time.Now()
	if executor.State != nil && !policy.Force && executor.State.ModelLocked(target, now) {
		return Result{}
	}
	if executor.State != nil && !policy.Force && !executor.State.TakeHalfOpenSlot(target.Provider) {
		return Result{}
	}

	ctx, cancel := context.WithTimeout(exchange.Request.Context(), runtime.Scheduling.Timeout())
	defer cancel()
	body := exchange.Body
	strippedParam, retriedImages := false, false
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
			log.Printf("[proto=%s provider=%s] build upstream req: %v", plan.ClientProtocol(), target.Provider, err)
			executor.release(target.Provider)
			executor.failover(target)
			return Result{Outcome: OutcomeFailedHard}
		}
		copyHeaderWhitelist(req.Header, exchange.Request.Header, "content-type", "accept", "user-agent", "x-session-id", "user_id", "x-claude-code-session-id", "x-interaction-type", "x-interaction-id", "prompt_cache_key", "x-anthropic-billing-header", "anthropic-beta", "accept-language")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		if err := providerImpl.AuthHeaders(req); err != nil {
			log.Printf("[proto=%s provider=%s] auth error: %v", plan.ClientProtocol(), target.Provider, err)
			executor.release(target.Provider)
			executor.failover(target)
			return Result{Outcome: OutcomeFailedHard}
		}
		plan.ApplyConfiguredHeaders(req.Header)
		providerImpl.ExtraHeaders(req, plan.UpstreamPath())
		started := time.Now()
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
				executor.release(target.Provider)
				return Result{Outcome: OutcomeClientGone}
			}
			log.Printf("[proto=%s provider=%s] upstream error: %v", plan.ClientProtocol(), target.Provider, err)
			executor.failure(target, true)
			return Result{Outcome: OutcomeFailedHard}
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
			response.Body.Close()
			if authAttempt == 0 {
				log.Printf("[proto=%s provider=%s] 401, refreshing auth", plan.ClientProtocol(), target.Provider)
				if refreshErr := providerImpl.Refresh(); refreshErr == nil {
					continue
				} else {
					log.Printf("[proto=%s provider=%s] auth refresh failed: %v", plan.ClientProtocol(), target.Provider, refreshErr)
				}
			}
			executor.failure(target, false)
			return Result{Outcome: OutcomeFailedHard}
		}
		if response.StatusCode == http.StatusTooManyRequests {
			peek := peekResponseBody(response, 8<<10)
			response.Body.Close()
			decision := ParseRateLimit(response, peek, time.Now(), runtime.Scheduling)
			if executor.State != nil {
				executor.State.RecordRateLimit(target.Provider, decision)
				if decision.Kind != "" && decision.Kind != RateLimitTransient {
					log.Printf("[proto=%s provider=%s] 429 classified %s — skipped until %s",
						plan.ClientProtocol(), target.Provider, decision.Kind, decision.Until.Format(time.RFC3339))
				}
			}
			executor.rateLimited(target)
			return Result{Outcome: OutcomeRateLimited}
		}
		if response.StatusCode >= 500 {
			response.Body.Close()
			executor.failure(target, true)
			return Result{Outcome: OutcomeFailedHard}
		}
		if response.StatusCode >= 400 {
			var peek []byte
			if policy.ContextRetry != nil || response.StatusCode == 400 || response.StatusCode == 403 || response.StatusCode == 404 {
				peek = peekResponseBody(response, contextOverflowPeek)
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
						executor.State.NoteWireResponsesMiss(target.Provider)
					}
				} else if executor.State != nil {
					executor.State.RecordModelFailure(target)
				}
				if !policy.LastTarget {
					response.Body.Close()
					executor.release(target.Provider)
					executor.failover(target)
					return Result{Outcome: OutcomeFailedHard}
				}
			}
			if response.StatusCode == http.StatusBadRequest && !strippedParam {
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
		}
		if response.StatusCode < 300 && response.Header.Get("Content-Length") == "0" {
			if executor.State != nil {
				executor.State.RecordModelFailure(target)
			}
			if !policy.LastTarget {
				response.Body.Close()
				executor.release(target.Provider)
				executor.failover(target)
				return Result{Outcome: OutcomeFailedHard}
			}
		}
		if response.StatusCode < 300 {
			if executor.State != nil {
				executor.State.RecordSuccess(target)
			}
		} else {
			executor.release(target.Provider)
		}
		return executor.commit(runtime.Cache, plan, exchange, scope, dto, timedWriter, clientWantsStream)
	}
	executor.release(target.Provider)
	executor.failover(target)
	return Result{Outcome: OutcomeFailedHard}
}

func (executor Executor) commit(
	cache *responsecache.Store,
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
	var converted []byte
	if modeMismatch || (convert && !upstreamStream) {
		all, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		response.Body.Close()
		if err != nil {
			log.Printf("[proto=%s provider=%s] %s→%s convert read failed: %v — failing closed",
				plan.ClientProtocol(), dto.Target.Provider, plan.BackendProtocol(), plan.ClientProtocol(), err)
			http.Error(
				exchange.Writer,
				fmt.Sprintf("upstream response read failed during %s→%s conversion", plan.BackendProtocol(), plan.ClientProtocol()),
				http.StatusBadGateway,
			)
			return Result{Committed: true}
		}
		converted, err = convertBuffered(all, response.StatusCode, convert, upstreamStream, clientWantsStream, plan, scope)
		if err != nil {
			log.Printf("[proto=%s provider=%s] %s→%s convert response failed: %v — failing closed (would return wrong-protocol body)",
				plan.ClientProtocol(), dto.Target.Provider, plan.BackendProtocol(), plan.ClientProtocol(), err)
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
		if transformed && (strings.EqualFold(key, "content-length") || strings.EqualFold(key, "transfer-encoding")) {
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
			body = io.NopCloser(protocol.ConvertSSE(body, plan.ClientProtocol(), plan.BackendProtocol(), plan.Target().Model, scope.ResponseContext))
		}
	}
	if plan.ClientProtocol() == protocol.Responses && executor.Responses != nil && len(scope.ResponsesHistory) > 0 && converted == nil && upstreamStream && response.StatusCode < 300 {
		responses := executor.Responses
		body = bodycapture.New(body, protocol.ResponsesStateCaptureLimit, func(captured []byte, _ int64, truncated bool) {
			if !truncated {
				responses.RecordSSE(scope.ResponsesSession, scope.ResponsesHistory, captured)
			}
		})
	}
	// The wrapper order intentionally mirrors the root pipeline: transformed
	// client bytes are captured for continuation, then logging, usage, cache and client.
	if executor.Effects != nil {
		body = executor.Effects.CaptureResponse(body, dto)
		if clientStream {
			body = executor.Effects.CaptureUsage(body, dto, func(usage Usage) {
				dto.Usage = usage
			})
		}
	}
	var recorder *responsecache.Recorder
	if cache != nil && scope.CacheKey != "" && response.StatusCode < 300 {
		recorder = responsecache.NewRecorder(body, cache.MaxBodyBytes())
		body = recorder
	}
	counting := &countingReadCloser{ReadCloser: body}
	end := flushCopy(exchange.Writer, counting)
	body.Close()
	if response.StatusCode < 300 && counting.Count == 0 && end == streamEOF && exchange.Request.Context().Err() == nil {
		if executor.State != nil {
			executor.State.RecordModelFailure(dto.Target)
		}
		log.Printf("[proto=%s provider=%s] 200 with zero-byte body — model locked post-commit; next request fails over",
			plan.ClientProtocol(), dto.Target.Provider)
	}
	if recorder != nil && recorder.Complete() && len(recorder.Body()) > 0 {
		cache.Put(scope.CacheKey, response.StatusCode, responsecache.HeaderForCapturedBody(response.Header, convert, modeMismatch, clientWantsStream), recorder.Body(), time.Now())
	}
	dto.TotalMilliseconds = time.Since(dto.Started).Milliseconds()
	dto.TTFTMilliseconds = dto.TotalMilliseconds
	if timedWriter.hasFirstByte {
		dto.TTFTMilliseconds = timedWriter.firstByte.Sub(dto.Started).Milliseconds()
	}
	if executor.Effects != nil {
		executor.Effects.Committed(dto)
	}
	return Result{Committed: true, Commit: NewCommit(dto.Body)}
}

func convertBuffered(all []byte, status int, convert, upstreamStream, clientWantsStream bool, plan Plan, scope Scope) ([]byte, error) {
	if status >= 400 && convert {
		return protocol.ConvertErrorResponse(all, plan.ClientProtocol(), plan.BackendProtocol(), status)
	}
	if upstreamStream && !clientWantsStream {
		if convert {
			var err error
			all, err = io.ReadAll(protocol.ConvertSSE(bytes.NewReader(all), plan.ClientProtocol(), plan.BackendProtocol(), plan.Target().Model, scope.ResponseContext))
			if err != nil {
				return nil, err
			}
		}
		return protocol.AggregateSSE(all, plan.ClientProtocol())
	}
	if !upstreamStream && clientWantsStream {
		var err error
		if convert {
			all, err = protocol.ConvertResponse(all, plan.ClientProtocol(), plan.BackendProtocol(), scope.ResponseContext)
			if err != nil {
				return nil, err
			}
		}
		return protocol.ResponseToSSE(all, plan.ClientProtocol())
	}
	if convert {
		return protocol.ConvertResponse(all, plan.ClientProtocol(), plan.BackendProtocol(), scope.ResponseContext)
	}
	return all, nil
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
func (executor Executor) rateLimited(target configdomain.RouteTarget) {
	if executor.Effects != nil {
		executor.Effects.RateLimited(target)
		executor.Effects.Failover(target)
	}
}
