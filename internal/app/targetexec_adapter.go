package app

import (
	"io"
	"log"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/provider"
	"strconv"
	"time"

	configdomain "model-proxy/internal/config"
	observeevents "model-proxy/internal/observe/events"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
	"model-proxy/internal/transport/bodycapture"
)

// proxyHealthGate adapts the root Proxy to targetexec.HealthGate (the narrow
// scheduling capability used by GateState).
type proxyHealthGate struct{ proxy *Proxy }

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
func (g proxyHealthGate) NoteWireResponsesMiss(provider string) {
	g.proxy.noteWireResponsesMiss(provider)
}

// targetExecutionEffects maps semantic target-execution observations to the
// application-owned metrics, logging, token, agent, request-log and live-event
// stores. The internal package sees only the targetexec.Effects port.
type targetExecutionEffects struct {
	proxy *Proxy
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
	}
}

func (effects targetExecutionEffects) RateLimited(target configdomain.RouteTarget) {
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.Inc(target.Provider, target.Model, counters.EvRateLimited429)
	}
}

func (effects targetExecutionEffects) LogAttempt(attempt targetexec.AttemptDTO) {
	log.Printf(
		"[proto=%s provider=%s] %s %s model=%s→%s status=%s %dms bytes=%d",
		attempt.Protocol,
		attempt.Target.Provider,
		attempt.Request.Method,
		attempt.Request.URL.Path,
		attempt.Scope.CalledModel,
		attempt.Target.Model,
		provider.StatusColor(attempt.Response.StatusCode, strconv.Itoa(attempt.Response.StatusCode)),
		time.Since(attempt.Started).Milliseconds(),
		len(attempt.Body),
	)
}

func (effects targetExecutionEffects) CaptureResponse(
	body io.ReadCloser,
	attempt targetexec.AttemptDTO,
) io.ReadCloser {
	logger := effects.proxy.reqLog
	if logger == nil {
		return body
	}
	logContext := forwardLogCtx{
		requestID: attempt.Scope.Log.RequestID,
		attempt:   attempt.Scope.Log.Attempt,
		exposed:   attempt.Scope.Log.Exposed,
		origBody:  attempt.Scope.Log.OriginalBody,
	}
	input := buildRequestLogInput(
		logContext,
		attempt.Request,
		string(attempt.Protocol),
		attempt.Scope.CalledModel,
		attempt.Target,
		attempt.Response,
		attempt.Started,
		attempt.Body,
	)
	return bodycapture.New(body, logger.MaxBodyBytes(), func(captured []byte, total int64, truncated bool) {
		requestlog.Complete(logger, input, captured, total, truncated)
	})
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
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.Inc(target.Provider, target.Model, counters.EvRequests)
		effects.proxy.metrics.AddLatency(
			target.Provider,
			target.Model,
			uint64(attempt.UpstreamMilliseconds),
			uint64(attempt.TTFTMilliseconds),
		)
	}
	if effects.proxy.agents != nil && attempt.Scope.Agent != "" {
		effects.proxy.agents.IncRequests(attempt.Scope.Agent, target.Provider, target.Model)
		effects.proxy.agents.AddLatency(
			attempt.Scope.Agent,
			target.Provider,
			target.Model,
			uint64(attempt.UpstreamMilliseconds),
		)
	}
	if effects.proxy.events != nil {
		effects.proxy.events.Publish(observeevents.Event{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     attempt.Scope.Log.RequestID,
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

func (p *Proxy) targetExecutor(runtime targetexec.Runtime) targetexec.Executor {
	return targetexec.Executor{
		Client: p.client,
		State: targetexec.GateState{
			Gate:       proxyHealthGate{proxy: p},
			Runtime:    runtime,
			Scheduling: runtime.Scheduling,
		},
		Effects:   targetExecutionEffects{proxy: p},
		Responses: p.responsesState,
	}
}
