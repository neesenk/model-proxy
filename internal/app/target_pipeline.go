// target_pipeline.go — request-to-target pipeline wiring: routing planner adapter, pool resolver aliases, per-target plan construction, and the targetexec executor/effects adapter.
package app

import (
	"fmt"
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
	"model-proxy/internal/routing"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
	"model-proxy/internal/transport/bodycapture"
	"net/http"
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
func (g proxyHealthGate) NoteWireResponsesMiss(provider string) {
	// Resolve the pool parent from the REQUEST SNAPSHOT projection (nil-safe),
	// not from live p.parentOf: a pre-reload in-flight request must record the
	// verdict under its own generation's parent name.
	parent := provider
	if par, ok := g.parentOf[provider]; ok {
		parent = par
	}
	g.proxy.noteWireResponsesMiss(parent)
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

// targetExecutor assembles the per-attempt executor. parentOf is the request
// snapshot's pool-virtual→parent projection (RuntimeSnapshot.ParentOf): it is
// threaded into the health gate so the wire-verdict 404 correction stays on
// the request's own generation (single-snapshot red line).
func (p *Proxy) targetExecutor(runtime targetexec.Runtime, parentOf map[string]string) targetexec.Executor {
	return targetexec.Executor{
		Client: p.client,
		State: targetexec.GateState{
			Gate:       proxyHealthGate{proxy: p, parentOf: parentOf},
			Runtime:    runtime,
			Scheduling: runtime.Scheduling,
		},
		Effects:   targetExecutionEffects{proxy: p, generation: runtime.Generation},
		Responses: p.responsesState,
	}
}

type targetPlanInput struct {
	runtime     RuntimeSnapshot
	target      RouteTarget
	clientProto string
	clientPath  string
}

// planTarget resolves snapshot-owned provider, protocol, endpoint-capability,
// and runtime implementation facts, then freezes them in targetexec.Plan.
func (p *Proxy) planTarget(input targetPlanInput) (targetexec.Plan, error) {
	providerCfg, ok := configdomain.ProviderConfig(input.runtime.Cfg, input.runtime.ParentOf, input.target.Provider)
	if !ok {
		return targetexec.Plan{}, fmt.Errorf("unknown provider %q", input.target.Provider)
	}
	backendProtoName, viaResponsesVerdict := p.resolvedBackendProto(
		input.target.Protocol,
		input.target.Provider,
		providerCfg,
		input.target.Model,
		input.clientProto,
		input.runtime.ParentOf,
	)
	clientProto := protocol.Protocol(input.clientProto)
	backendProto := protocol.Protocol(backendProtoName)
	imageOK := routing.ImageOKForTarget(
		input.runtime.Cfg,
		input.runtime.ParentOf,
		input.runtime.Catalog,
		input.target,
	)
	return targetexec.NewPlan(targetexec.PlanInput{
		Target:              input.target,
		ProviderConfig:      providerCfg,
		Provider:            input.runtime.Providers[input.target.Provider],
		ClientProtocol:      clientProto,
		BackendProtocol:     backendProto,
		ViaResponsesVerdict: viaResponsesVerdict,
		ClientPath:          input.clientPath,
		ImageOK:             imageOK,
		// One collector per target attempt: the attempt's conversion
		// diagnostics ride the plan into the request log; strict refuses
		// lossy conversions for this target when configured.
		Diag:        protocol.NewDiagnostics(),
		StrictLossy: input.runtime.Cfg.Conversion.StrictLossyValue(),
	}), nil
}

// forcedProviderFromRequest extracts the HTTP boundary value used by replay.
// Header wins over query. The policy package receives only the resulting value.
func forcedProviderFromRequest(request *http.Request) string {
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

// requestRoutingScheduler is the stateful scheduling port used by the
// stateless request-routing planner. Every field belongs to the runtime
// generation captured once at the start of forward; a reload cannot mix new
// config or pool identity into an in-flight cross-route decision.
type requestRoutingScheduler struct {
	proxy      *Proxy
	config     *Config
	parentOf   map[string]string
	routeKeys  map[string]bool
	generation uint64
}

func (scheduler requestRoutingScheduler) Schedule(
	routeName, sessionKey string,
	targets []RouteTarget,
) []RouteTarget {
	return scheduler.proxy.schedule(
		scheduler.config,
		scheduler.parentOf,
		routeName,
		sessionKey,
		targets,
		scheduler.routeKeys,
		scheduler.generation,
	)
}

// requestRoutingPlanner projects one immutable runtime snapshot into the pure
// policy package and binds only the narrow scheduler port that may mutate
// sticky/round-robin state.
func requestRoutingPlanner(
	proxy *Proxy,
	runtime RuntimeSnapshot,
	routeKeys map[string]bool,
) routing.Planner {
	return routing.NewPlanner(routing.PlannerInput{
		Config:         runtime.Cfg,
		ParentOf:       runtime.ParentOf,
		Catalog:        runtime.Catalog,
		ExpandedRoutes: runtime.ExpandedRoutes,
		Scheduler: requestRoutingScheduler{
			proxy:      proxy,
			config:     runtime.Cfg,
			parentOf:   runtime.ParentOf,
			routeKeys:  routeKeys,
			generation: runtime.Generation,
		},
	})
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
