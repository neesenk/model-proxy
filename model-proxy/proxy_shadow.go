package main

import (
	"context"
	"log"
	"model-proxy/internal/observe/requestlog"

	"model-proxy/internal/protocol"
	shadowexec "model-proxy/internal/shadow"
	"model-proxy/internal/targetexec"
)

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
	commit *targetexec.Commit,
) {
	if commit == nil || p.reqLog == nil || len(runtime.cfg.Shadow) == 0 {
		return
	}
	shadow, ok := runtime.cfg.Shadow[exposed]
	if !ok || shadow.Provider == "" || shadow.Provider == primary.Provider {
		return
	}
	shadowRuntime := runtime.shadow
	if shadowRuntime == nil || !shadowRuntime.ShouldSample() {
		return
	}
	permit := shadowRuntime.TryAcquire()
	if permit == nil {
		return
	}
	if !p.lifecycle.RunBeforeLogDrain(func() {
		defer permit.Release()
		p.runShadow(
			runtime,
			shadowRuntime,
			proto,
			backendProto,
			calledModel,
			exposed,
			shadow,
			commit.RequestBody(),
			primaryRequestID,
		)
	}) {
		permit.Release()
	}
}

// shouldShadow reports whether this request should be shadow-evaluated, based on
// the currently-loaded shadow runtime's sample rate. Reload-aware: the runtime
// pointer is swapped atomically, so a config change (e.g. sample_rate: 0) takes
// effect immediately without a restart.
func (p *Proxy) shouldShadow() bool {
	return p.shadow.Load().ShouldSample()
}

// runShadow sends the same prompt to a candidate backend (shadow evaluation,
// #12): fire-and-forget, the result is logged for offline comparison and NEVER
// returned to the client. It shares targetexec.Plan request preparation but is
// best-effort and bounded — any error is logged and dropped (shadow must never
// affect the live request). Both runtimeSnapshot and shadowRuntime are captured
// by the primary attempt before launching the goroutine, so reload cannot mix
// config/provider generation with a different semaphore/client bundle.
//
// `bodyProto` is the protocol of reqBody (the primary target's backend proto —
// reqBody may already be converted from the client's proto). The shadow backend's
// own protocol is shadowTarget.Protocol (defaulting to bodyProto); runShadow selects the
// shadow base URL + path for THAT protocol and converts the body if it differs.
func (p *Proxy) runShadow(runtime runtimeSnapshot, shadowRuntime *shadowexec.Runtime, proto, bodyProto, calledModel, exposed string, shadowTarget ShadowTarget, reqBody []byte, primaryReqID string) {
	if runtime.cfg == nil {
		// Defensive: runtimeSnapshot is handed around as a plain value — a
		// future call site that forgets to populate it must not nil-deref
		// below (runtime.cfg.Scheduling.Timeout()). Log loudly and skip.
		log.Printf("[shadow] %s: skipped — runtime snapshot has no config (caller bug)", shadowTarget.Provider)
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
	target := RouteTarget{Provider: shadowTarget.Provider, Model: shadowTarget.Model, Protocol: shadowTarget.Protocol}
	picked, ok := newResolver(
		p,
		runtime.providers,
		runtime.poolIndex,
		runtime.generation,
	).Pick(target, "")
	if !ok {
		log.Printf("[shadow] %s: provider not available (no runnable healthy virtual)", shadowTarget.Provider)
		return
	}
	target = picked
	plan, err := p.planTarget(targetPlanInput{
		runtime: runtime, target: target, clientProto: bodyProto, clientPath: protocol.BackendPath(protocol.Protocol(bodyProto)),
	})
	if err != nil {
		log.Printf("[shadow] %s: target plan failed: %v", target.Provider, err)
		return
	}
	result := shadowRuntime.Execute(context.Background(), shadowexec.Job{
		Plan:         plan,
		Body:         reqBody,
		CalledModel:  calledModel,
		MaxBodyBytes: logger.MaxBodyBytes(),
	})
	if result.Err != nil {
		log.Printf("[shadow] %s/%s execution failed: %v", target.Provider, target.Model, result.Err)
		// Once upstream response headers exist, preserve the historical
		// best-effort request-log record even when draining the body times out or
		// is cut short. Preparation/transport failures have no response to log.
		if result.Request == nil || result.Response == nil {
			return
		}
	}
	logInput := buildRequestLogInput(
		forwardLogCtx{requestID: "shadow-" + primaryReqID, exposed: exposed},
		result.Request,
		proto,
		calledModel,
		RouteTarget{Provider: target.Provider, Model: target.Model},
		result.Response,
		result.Started,
		result.RequestBody,
	)
	requestlog.Complete(logger, logInput, result.Capture.Body, result.Capture.Total, result.Capture.Truncated)
}
