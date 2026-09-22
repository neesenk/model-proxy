package app

import (
	"context"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/logx"
	"net/http"
	"time"

	"model-proxy/internal/observe/requestlog"

	"model-proxy/internal/forward"
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
	runtime RuntimeSnapshot,
	proto string,
	backendProto string,
	calledModel string,
	exposed string,
	primary configdomain.RouteTarget,
	primaryRequestID string,
	primaryAgent string,
	primarySession string,
	commit *targetexec.Commit,
) {
	if commit == nil || p.reqLog == nil || len(runtime.Cfg.Shadow) == 0 {
		return
	}
	shadow, ok := runtime.Cfg.Shadow[exposed]
	if !ok || shadow.Provider == "" || shadow.Provider == primary.Provider {
		return
	}
	shadowRuntime := runtime.Shadow
	if shadowRuntime == nil || !shadowRuntime.ShouldSample() {
		return
	}
	permit := shadowRuntime.TryAcquire()
	if permit == nil {
		// The gate rejection used to be invisible from the outside — the only
		// symptom was shadow records quietly disappearing. The runtime's
		// dropped counter drives the cadence: first rejection, then every
		// 50th.
		if n := shadowRuntime.Dropped(); n == 1 || n%50 == 0 {
			logx.Warnf("[shadow] concurrency gate full — detached dispatches dropped so far: %d (see shadow_max_concurrent)", n)
		}
		return
	}
	if !p.lifecycle.RunBeforeLogDrain(func(stop <-chan struct{}) {
		defer permit.Release()
		p.runShadow(
			runtime,
			shadowRuntime,
			stop,
			proto,
			backendProto,
			calledModel,
			exposed,
			shadow,
			commit.RequestBody(),
			primaryRequestID,
			primaryAgent,
			primarySession,
		)
	}) {
		permit.Release()
	}
}

// shadowShutdownGrace bounds how long Proxy.Close waits for an in-flight
// shadow request after lifecycle stop before canceling it. Fast (normal)
// shadow evaluations finish inside the grace window and their request-log
// records are drained as usual; a hung upstream gets cut well before the
// supervisor's 10s SIGTERM window, so every final flush behind
// WaitBeforeLogDrain still runs.
const shadowShutdownGrace = 2 * time.Second

// shadowTimeoutCap bounds one detached shadow execution's HTTP budget. The
// upstream timeout (scheduling.upstream_timeout, default 1800s) is sized for
// live streaming generations; a shadow evaluation must not pin one of the
// few concurrency slots (shadow_max_concurrent, default 4) for half an hour
// when its upstream hangs — four hung shadows would silently stall the whole
// channel. Five minutes covers any real generation while bounding the stall.
const shadowTimeoutCap = 5 * time.Minute

// shadowTimeoutBudget is the shadow client timeout: the configured upstream
// budget capped at shadowTimeoutCap (never larger than either).
func shadowTimeoutBudget(upstream time.Duration) time.Duration {
	if upstream <= 0 || upstream > shadowTimeoutCap {
		return shadowTimeoutCap
	}
	return upstream
}

// runShadow sends the same prompt to a candidate backend (shadow evaluation,
// #12): fire-and-forget, the result is logged for offline comparison and NEVER
// returned to the client. It shares targetexec.Plan request preparation but is
// best-effort and bounded — any error is logged and dropped (shadow must never
// affect the live request). Both RuntimeSnapshot and shadowRuntime are captured
// by the primary attempt before launching the goroutine, so reload cannot mix
// config/provider generation with a different semaphore/client bundle.
//
// The task observes the lifecycle stop channel with a grace window: shutdown
// lets an in-flight evaluation finish (its record is drained by the logger
// shutdown that follows), but a request still hanging past the grace is
// canceled — WaitBeforeLogDrain must not be pinned for the full client
// timeout, or the supervisor kill would drop every final flush behind it.
//
// `bodyProto` is the protocol of reqBody (the primary target's backend proto —
// reqBody may already be converted from the client's proto). The shadow backend's
// own protocol is shadowTarget.Protocol (defaulting to bodyProto); runShadow selects the
// shadow base URL + path for THAT protocol and converts the body if it differs.
func (p *Proxy) runShadow(runtime RuntimeSnapshot, shadowRuntime *shadowexec.Runtime, stop <-chan struct{}, proto, bodyProto, calledModel, exposed string, shadowTarget configdomain.ShadowTarget, reqBody []byte, primaryReqID, primaryAgent, primarySession string) {
	if runtime.Cfg == nil {
		// Defensive: RuntimeSnapshot is handed around as a plain value — a
		// future call site that forgets to populate it must not nil-deref
		// below (runtime.Cfg.Scheduling.Timeout()). Log loudly and skip.
		logx.Warnf("[shadow] %s: skipped — runtime snapshot has no config (caller bug)", shadowTarget.Provider)
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
	target := configdomain.RouteTarget{Provider: shadowTarget.Provider, Model: shadowTarget.Model, Protocol: shadowTarget.Protocol}
	picked, ok := newResolver(
		p,
		runtime.Providers,
		runtime.PoolIndex,
		runtime.Generation,
	).Pick(target, "")
	if !ok {
		logx.Warnf("[shadow] %s: provider not available (no runnable healthy virtual)", shadowTarget.Provider)
		return
	}
	target = picked
	plan, err := forward.PlanTarget(p.forwardServices(), forward.PlanInput{
		Runtime: runtime, Target: target, ClientProto: bodyProto, ClientPath: protocol.BackendPath(protocol.Protocol(bodyProto)),
	})
	if err != nil {
		logx.Warnf("[shadow] %s: target plan failed: %v", target.Provider, err)
		return
	}
	// Tie the shadow execution to the lifecycle stop channel with a grace
	// window: BeginStop starts the clock, and a request still in flight past
	// the grace is canceled so the final flushes behind WaitBeforeLogDrain
	// still run before the supervisor's kill window closes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			select {
			case <-time.After(shadowShutdownGrace):
				cancel()
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
	}()
	result := shadowRuntime.Execute(ctx, shadowexec.Job{
		Plan:        plan,
		Body:        reqBody,
		CalledModel: calledModel,
		// Per-provider proxy: same resolution chain as the live pipeline, but
		// with shadow's own timeout budget (the pooled clients run Timeout 0).
		Client:       &http.Client{Transport: p.transportFor(runtime.Cfg, runtime.ParentOf, target.Provider), Timeout: shadowTimeoutBudget(runtime.Cfg.Scheduling.Timeout())},
		MaxBodyBytes: logger.MaxBodyBytes(),
	})
	if result.Err != nil {
		logx.Warnf("[shadow] %s/%s execution failed: %v", target.Provider, target.Model, result.Err)
		// Once upstream response headers exist, preserve the historical
		// best-effort request-log record even when draining the body times out or
		// is cut short. Preparation/transport failures have no response to log.
		if result.Request == nil || result.Response == nil {
			return
		}
	}
	// The synthetic upstream request carries neither the client's UA nor its
	// session headers, so agent/session attribution comes from the PRIMARY
	// request (resolved once in serveOnce) — without this every shadow record
	// was agentless and invisible in the sessions view.
	logInput := forward.BuildRequestLogInput(
		forward.LogCtx{RequestID: "shadow-" + primaryReqID, SessionID: primarySession, Exposed: exposed, Agent: primaryAgent},
		result.Request,
		proto,
		calledModel, configdomain.RouteTarget{Provider: target.Provider, Model: target.Model}, result.Response,
		result.Started,
		result.RequestBody,
	)
	requestlog.Complete(logger, logInput, result.Capture.Body, result.Capture.Total, result.Capture.Truncated)
}
