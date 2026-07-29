package main

import (
	"io"
	"log"
	"strconv"
	"time"

	configdomain "model-proxy/internal/config"
	observeevents "model-proxy/internal/observe/events"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
	"model-proxy/internal/transport/bodycapture"
)

// targetExecutionState freezes the reload generation and scheduling values
// captured by one Attempt. The internal executor therefore cannot accidentally
// mutate a newer runtime generation or reach back into root scheduling.
type targetExecutionState struct {
	proxy      *Proxy
	runtime    targetexec.Runtime
	scheduling Scheduling
}

var _ targetexec.State = targetExecutionState{}

func newTargetExecutionState(proxy *Proxy, runtime targetexec.Runtime) targetExecutionState {
	return targetExecutionState{
		proxy:      proxy,
		runtime:    runtime,
		scheduling: runtime.Scheduling,
	}
}

func (state targetExecutionState) ModelLocked(target configdomain.RouteTarget, now time.Time) bool {
	return state.proxy.modelLocked(target.Provider, target.Model, now)
}

func (state targetExecutionState) TakeHalfOpenSlot(provider string) bool {
	return state.proxy.takeHalfOpenSlot(provider, state.runtime.Generation)
}

func (state targetExecutionState) ReleaseHalfOpenSlot(provider string) {
	state.proxy.releaseHalfOpenSlot(provider, state.runtime.Generation)
}

func (state targetExecutionState) RecordSuccess(target configdomain.RouteTarget) {
	state.proxy.recordSuccess(target.Provider, target.Model, state.runtime.Generation)
}

func (state targetExecutionState) RecordFailure(provider string) {
	state.proxy.recordFailure(provider, state.scheduling, state.runtime.Generation)
}

func (state targetExecutionState) RecordModelFailure(target configdomain.RouteTarget) {
	state.proxy.recordModelFailure(target.Provider, target.Model, state.scheduling, state.runtime.Generation)
}

func (state targetExecutionState) RecordRateLimit(
	provider string,
	decision targetexec.RateLimitDecision,
) {
	state.proxy.recordRateLimit(provider, decision.Until, runtimestate.ParseRateLimitKind(string(decision.Kind)), state.runtime.Generation)
}

func (state targetExecutionState) LearnParamBlock(target configdomain.RouteTarget, parameter string) bool {
	return state.proxy.learnParamBlock(target.Provider, target.Model, parameter, state.runtime.Generation)
}

func (state targetExecutionState) ApplyParamBlock(target configdomain.RouteTarget, body []byte) []byte {
	return state.proxy.applyParamBlock(target.Provider, target.Model, body)
}

func (state targetExecutionState) NoteWireResponsesMiss(provider string) {
	state.proxy.noteWireResponsesMiss(provider)
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
		effects.proxy.metrics.inc(target.Provider, target.Model, evFailovers)
	}
}

func (effects targetExecutionEffects) Failure(target configdomain.RouteTarget) {
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.inc(target.Provider, target.Model, evFailures)
	}
}

func (effects targetExecutionEffects) RateLimited(target configdomain.RouteTarget) {
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.inc(target.Provider, target.Model, evRateLimited429)
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
		statusColor(attempt.Response.StatusCode, strconv.Itoa(attempt.Response.StatusCode)),
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
	input := requestLogInput(
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
		completeRequestLog(logger, input, captured, total, truncated)
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
	return newUsageScanner(
		body,
		tokenKey{Provider: target.Provider, Model: target.Model},
		effects.proxy.tokens,
		func(usage tokenUsage) {
			if effects.proxy.agents != nil && agent != "" {
				effects.proxy.agents.addTokens(agent, target.Provider, target.Model, usage)
			}
			observe(targetexec.Usage{Input: usage.Input, Output: usage.Output})
		},
	)
}

func (effects targetExecutionEffects) Committed(attempt targetexec.AttemptDTO) {
	target := attempt.Target
	if effects.proxy.metrics != nil {
		effects.proxy.metrics.inc(target.Provider, target.Model, evRequests)
		effects.proxy.metrics.addLatency(
			target.Provider,
			target.Model,
			uint64(attempt.UpstreamMilliseconds),
			uint64(attempt.TTFTMilliseconds),
		)
	}
	if effects.proxy.agents != nil && attempt.Scope.Agent != "" {
		effects.proxy.agents.incRequests(attempt.Scope.Agent, target.Provider, target.Model)
		effects.proxy.agents.addLatency(
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
		Client:    p.client,
		State:     newTargetExecutionState(p, runtime),
		Effects:   targetExecutionEffects{proxy: p},
		Responses: p.responsesState,
	}
}
