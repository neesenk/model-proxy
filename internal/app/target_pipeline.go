// target_pipeline.go — app-side targetexec adapters: the health/circuit gate and the observability effects bound to one runtime generation, plus the pool resolver aliases.
package app

import (
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"model-proxy/internal/forward"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/provider"
	"model-proxy/internal/routing"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
	"model-proxy/internal/transport/bodycapture"
	"strconv"
	"time"
)

// proxyHealthGate adapts the root Proxy to targetexec.HealthGate (the narrow
// scheduling capability used by GateState). parentOf is the request snapshot's
// pool-virtual→parent projection (RuntimeSnapshot.ParentOf, immutable): the
// wire-verdict 404 correction resolves the parent from it instead of
// re-reading reload-owned state on the request path.
type proxyHealthGate struct {
	proxy    *Proxy
	parentOf map[string]string
}

func (g proxyHealthGate) ModelLocked(provider, model string, now time.Time) bool {
	return g.proxy.modelLocked(provider, model, now)
}
func (g proxyHealthGate) TakeHalfOpenSlot(provider string, generation uint64) bool {
	return g.proxy.takeHalfOpenSlot(provider, generation)
}
func (g proxyHealthGate) ReleaseHalfOpenSlot(provider string, generation uint64) {
	g.proxy.releaseHalfOpenSlot(provider, generation)
}
func (g proxyHealthGate) RecordSuccess(provider, model string, generation uint64) {
	g.proxy.recordSuccess(provider, model, generation)
}
func (g proxyHealthGate) RecordFailure(provider string, scheduling configdomain.Scheduling, generation uint64) {
	g.proxy.recordFailure(provider, scheduling, generation)
}
func (g proxyHealthGate) RecordModelFailure(provider, model string, scheduling configdomain.Scheduling, generation uint64) {
	g.proxy.recordModelFailure(provider, model, scheduling, generation)
}
func (g proxyHealthGate) RecordRateLimit(provider string, until time.Time, kind string, generation uint64) {
	g.proxy.recordRateLimit(provider, until, runtimestate.ParseRateLimitKind(kind), generation)
}
func (g proxyHealthGate) LearnParamBlock(provider, model, parameter string, generation uint64) bool {
	return g.proxy.learnParamBlock(provider, model, parameter, generation)
}
func (g proxyHealthGate) ApplyParamBlock(provider, model string, body []byte) []byte {
	return g.proxy.applyParamBlock(provider, model, body)
}
func (g proxyHealthGate) NoteWireResponsesMiss(provider, model string) {
	// Resolve the pool parent from the REQUEST SNAPSHOT projection (nil-safe),
	// not from live p.parentOf: a pre-reload in-flight request must record the
	// verdict under its own generation's parent name.
	parent := provider
	if par, ok := g.parentOf[provider]; ok {
		parent = par
	}
	g.proxy.noteWireResponsesMiss(parent, model)
}

// targetExecutionEffects maps semantic target-execution observations to the
// application-owned metrics, logging, token, agent, request-log and live-event
// stores. The internal package sees only the targetexec.Effects port.
// generation is the request snapshot's runtime generation: quality samples
// recorded here are generation-gated like the error-rate samples, so a
// pre-reload in-flight commit cannot write into the new generation's state.
type targetExecutionEffects struct {
	proxy      *Proxy
	generation uint64
}

var _ targetexec.Effects = targetExecutionEffects{}

func (effects targetExecutionEffects) Failover(target configdomain.RouteTarget) {
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.Inc(target.Provider, target.Model, counters.EvFailovers)
	}
}

func (effects targetExecutionEffects) Failure(target configdomain.RouteTarget) {
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.Inc(target.Provider, target.Model, counters.EvFailures)
		effects.proxy.metrics.Inc("attempts", "hard", counters.EvAttemptHard)
	}
}

func (effects targetExecutionEffects) RateLimited(target configdomain.RouteTarget) {
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.Inc(target.Provider, target.Model, counters.EvRateLimited429)
		effects.proxy.metrics.Inc("attempts", "rate_limited", counters.EvAttemptRateLimited)
	}
}

func (effects targetExecutionEffects) LogAttempt(attempt targetexec.AttemptDTO) {
	logx.Infof(
		"[proto=%s provider=%s] %s %s model=%s→%s status=%s %dms bytes=%d",
		attempt.Protocol,
		attempt.Target.Provider,
		attempt.Request.Method,
		attempt.Request.URL.Path,
		attempt.Scope.CalledModel,
		attempt.Target.Model,
		display.StatusColor(attempt.Response.StatusCode, strconv.Itoa(attempt.Response.StatusCode)),
		time.Since(attempt.Started).Milliseconds(),
		len(attempt.Body),
	)
}

func (effects targetExecutionEffects) CaptureResponse(
	body io.ReadCloser,
	attempt targetexec.AttemptDTO,
) io.ReadCloser {
	logger := effects.proxy.reqLog
	if logger != nil {
		logContext := forward.LogCtx{
			RequestID: attempt.Scope.Log.RequestID,
			SessionID: attempt.Scope.Log.SessionID,
			Attempt:   attempt.Scope.Log.Attempt,
			Exposed:   attempt.Scope.Log.Exposed,
			Agent:     attempt.Scope.Agent,
			OrigBody:  attempt.Scope.Log.OriginalBody,
		}
		input := forward.BuildRequestLogInput(
			logContext,
			attempt.Request,
			string(attempt.Protocol),
			attempt.Scope.CalledModel,
			attempt.Target,
			attempt.Response,
			attempt.Started,
			attempt.Body,
		)
		// TTFT for the log = the pipeline's first read of the committed
		// body (stamped before the capture callback fires inside Close, so
		// the record built there already carries it). Same goroutine reads
		// then completes — no synchronization needed.
		firstReadMs := int64(0)
		body = &ttftReadCloser{ReadCloser: body, started: attempt.Started, onFirst: func(t int64) { firstReadMs = t }}
		body = bodycapture.New(body, logger.MaxBodyBytes(), func(captured []byte, total int64, truncated bool) {
			in := input
			in.TTFTMilliseconds = firstReadMs
			requestlog.Complete(logger, in, captured, total, truncated)
		})
	}

	// Live response preview: bounded, throttled tap on the committed response
	// body when someone is watching the Live monitor. The tap is zero-cost when
	// no subscribers exist (HasSubscribers is an atomic snapshot read).
	if effects.proxy.events != nil && effects.proxy.events.HasSubscribers() {
		body = observeevents.NewProgressReader(body, observeevents.ProgressMeta{
			RequestID:     attempt.Scope.Log.RequestID,
			Agent:         attempt.Scope.Agent,
			Protocol:      string(attempt.Protocol),
			Exposed:       attempt.Scope.Log.Exposed,
			Provider:      attempt.Target.Provider,
			UpstreamModel: attempt.Target.Model,
		}, func(text string, receivedBytes int64) {
			effects.proxy.events.Publish(observeevents.Event{
				Type:          "progress",
				Ts:            time.Now().UnixMilli(),
				RequestID:     attempt.Scope.Log.RequestID,
				SessionID:     attempt.Scope.Log.SessionID,
				Agent:         attempt.Scope.Agent,
				Protocol:      string(attempt.Protocol),
				Exposed:       attempt.Scope.Log.Exposed,
				Provider:      attempt.Target.Provider,
				UpstreamModel: attempt.Target.Model,
				ReceivedBytes: receivedBytes,
				Text:          text,
			})
		})
	}

	return body
}

// ttftReadCloser stamps the elapsed-since-start time of the first Read on
// its delegate — the request-log TTFT source (see CaptureResponse).
type ttftReadCloser struct {
	io.ReadCloser
	started time.Time
	onFirst func(ms int64)
	seen    bool
}

func (t *ttftReadCloser) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if !t.seen && n > 0 {
		t.seen = true
		t.onFirst(time.Since(t.started).Milliseconds())
	}
	return n, err
}

func (effects targetExecutionEffects) CaptureUsage(
	body io.ReadCloser,
	attempt targetexec.AttemptDTO,
	observe func(targetexec.Usage),
) io.ReadCloser {
	if effects.proxy.tokens == nil {
		return body
	}
	target := attempt.Target
	agent := attempt.Scope.Agent
	return counters.NewUsageScanner(
		body,
		counters.TokenKey{Provider: target.Provider, Model: target.Model},
		effects.proxy.tokens,
		func(usage counters.TokenUsage) {
			if effects.proxy.agents != nil && agent != "" {
				effects.proxy.agents.AddTokens(agent, target.Provider, target.Model, usage)
			}
			observe(targetexec.Usage{Input: usage.Input, Output: usage.Output})
		},
	)
}

func (effects targetExecutionEffects) Committed(attempt targetexec.AttemptDTO) {
	target := attempt.Target
	if attempt.Response.StatusCode < 300 {
		// Successful (2xx) commit: fold TTFT into the scheduling quality EWMA
		// — "committed 2xx only", matching manager_quality and
		// runtime-state.md (a committed 3xx is not a generation result).
		// (Error EWMA already moved via RecordSuccess/RecordFailure upstream.)
		effects.proxy.recordAttemptQuality(
			target.Provider,
			time.Duration(attempt.TTFTMilliseconds)*time.Millisecond,
			effects.generation,
		)
	}
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.Inc(target.Provider, target.Model, counters.EvRequests)
		effects.proxy.metrics.Inc("attempts", "ok", counters.EvAttemptOK)
		effects.proxy.metrics.AddLatency(
			target.Provider,
			target.Model,
			uint64(attempt.UpstreamMilliseconds),
			uint64(attempt.TTFTMilliseconds),
		)
		// Full call wall-clock (send → end of streamed body) — the tok/s
		// denominator; header-arrival latency above deliberately excludes the
		// generation tail.
		effects.proxy.metrics.AddDuration(
			target.Provider,
			target.Model,
			uint64(attempt.TotalMilliseconds),
		)
	}
	if effects.proxy.agents != nil && attempt.Scope.Agent != "" {
		effects.proxy.agents.IncRequests(attempt.Scope.Agent, target.Provider, target.Model)
		effects.proxy.agents.AddLatency(
			attempt.Scope.Agent,
			target.Provider,
			target.Model,
			uint64(attempt.UpstreamMilliseconds),
			uint64(attempt.TTFTMilliseconds),
		)
		effects.proxy.agents.AddDuration(
			attempt.Scope.Agent,
			target.Provider,
			target.Model,
			uint64(attempt.TotalMilliseconds),
		)
	}
	if effects.proxy.events != nil {
		effects.proxy.events.Publish(observeevents.Event{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     attempt.Scope.Log.RequestID,
			SessionID:     attempt.Scope.Log.SessionID,
			Agent:         attempt.Scope.Agent,
			Protocol:      string(attempt.Protocol),
			Exposed:       attempt.Scope.Log.Exposed,
			Provider:      target.Provider,
			UpstreamModel: target.Model,
			Status:        attempt.Response.StatusCode,
			LatencyMs:     attempt.TotalMilliseconds,
			Input:         attempt.Usage.Input,
			Output:        attempt.Usage.Output,
		})
	}
}

// resolver is the root alias for routing.Resolver; resolution logic lives in
// internal/routing. Proxy satisfies routing.ResolverState via the two
// delegation methods below.
type resolver = routing.Resolver

type resolverState = routing.ResolverState

func newResolver(
	state resolverState,
	providers map[string]provider.Provider,
	poolIndex map[string][]string,
	generations ...uint64,
) *resolver {
	return routing.NewResolver(state, providers, poolIndex, generations...)
}

func (p *Proxy) ResolverSpreadStart(parent string, n int, generation uint64) int {
	return p.runtimeState.ResolverSpreadStart(parent, n, generation)
}

func (p *Proxy) TargetHealthy(virtual, model string, now time.Time) bool {
	return p.runtimeState.TargetHealthy(virtual, model, now)
}
