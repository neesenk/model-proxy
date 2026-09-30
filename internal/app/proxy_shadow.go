package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/forward"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/protocol"
	runtimestate "model-proxy/internal/runtime"
	shadowexec "model-proxy/internal/shadow"
	"model-proxy/internal/targetexec"
)

// dispatchShadowAfterCommit is orchestration-layer post-processing for a
// successfully delivered normal target. The executor returns only the exact
// upstream request bytes; Shadow policy, sampling, lifecycle admission, and the
// Proxy method call stay here. Fusion synthesis does not pass through this
// normal-route hook and therefore never recursively dispatches Shadow.
//
// When the route has route_policy.eval configured, this also drives L2 pairwise
// shadow evaluation: a sampled primary response is shadow-replayed to the paired
// grade and a judge model compares the two responses. Eval failures are always
// fail-open.
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
	routingDecision *configdomain.RoutingDecision,
) {
	if commit == nil || p.reqLog == nil {
		return
	}

	// Legacy shadow dispatch (unchanged behavior).
	if len(runtime.Cfg.Shadow) > 0 {
		p.dispatchLegacyShadow(runtime, proto, backendProto, calledModel, exposed, primary, primaryRequestID, primaryAgent, primarySession, commit, routingDecision)
	}

	// L2 eval shadow dispatch.
	p.dispatchEvalShadow(runtime, proto, backendProto, calledModel, exposed, primary, primaryRequestID, primaryAgent, primarySession, commit, routingDecision)
}

func (p *Proxy) dispatchLegacyShadow(
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
	routingDecision *configdomain.RoutingDecision,
) {
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
			configdomain.RouteTarget{Provider: shadow.Provider, Model: shadow.Model, Protocol: shadow.Protocol},
			commit.RequestBody(),
			primaryRequestID,
			primaryAgent,
			primarySession,
			nil,
			routingDecision,
		)
	}) {
		permit.Release()
	}
}

// evalJudgeTimeout is the per-judge-call budget. It is short: a judge failure
// must never block shadow dispatch for long.
const evalJudgeTimeout = 30 * time.Second

// dispatchEvalShadow implements L2 pairwise evaluation for graded routes.
// It is fail-open at every step and never affects the live request.
func (p *Proxy) dispatchEvalShadow(
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
	routingDecision *configdomain.RoutingDecision,
) {
	if isEvalRecursiveID(primaryRequestID) {
		return
	}
	policy, ok := runtime.Cfg.RoutePolicies[exposed]
	if !ok || policy.Eval == nil || !policy.HasGrades() {
		return
	}
	// Pin/force requests skip route policy and therefore skip eval.
	if routingDecision == nil || routingDecision.Grade == "" {
		return
	}
	primaryGrade := routingDecision.Grade
	cfg := policy.Eval
	// SampleRateValue applies the documented default (0 or omitted → 0.05,
	// per config.yaml's eval block comment); evalRand is injected at
	// construction (production: rand.Float64; tests: deterministic draws) —
	// a request goroutine must never lazily write the shared Proxy field.
	if p.evalRand() >= cfg.SampleRateValue() {
		return
	}
	primaryBody := p.evalPrimaryBodies.Retrieve(primaryRequestID)
	if len(primaryBody) == 0 {
		logx.Debugf("[eval] %s: sampled but primary body unavailable (cache miss or request_log disabled)", primaryRequestID)
		return
	}

	shadowGrade, ok := resolveEvalPairGrade(primaryGrade, cfg.Pair, runtime.ExpandedRoutes[exposed], policy.Grades, runtime.ParentOf)
	if !ok {
		logx.Debugf("[eval] %s: could not resolve pair grade for %s (pair=%s)", primaryRequestID, primaryGrade, cfg.Pair)
		return
	}
	shadowTarget, ok := p.pickEvalShadowTarget(runtime, policy.Grades[shadowGrade])
	if !ok {
		logx.Debugf("[eval] %s: no available target in shadow grade %s", primaryRequestID, shadowGrade)
		return
	}

	shadowRuntime := runtime.Shadow
	if shadowRuntime == nil {
		return
	}
	permit := shadowRuntime.TryAcquire()
	if permit == nil {
		logx.Debugf("[eval] %s: shadow concurrency gate full — dropping eval sample", primaryRequestID)
		return
	}
	if !p.lifecycle.RunBeforeLogDrain(func(stop <-chan struct{}) {
		defer permit.Release()
		p.runEvalShadowPair(
			runtime,
			shadowRuntime,
			stop,
			proto,
			backendProto,
			calledModel,
			exposed,
			primary,
			shadowTarget,
			shadowGrade,
			primaryRequestID,
			primaryAgent,
			primarySession,
			commit.RequestBody(),
			primaryBody,
			routingDecision,
		)
	}) {
		permit.Release()
	}
}

// resolveEvalPairGrade returns the shadow grade for a pairwise evaluation.
// opposite uses the route's effective grade order (derived from the first
// appearance of each grade in the route's expanded target list, via
// forward.GradeOrder — the same order the live scheduler uses). For the last
// grade in that order, "opposite" wraps to the previous grade. This rule is
// written in code and docs; it intentionally does not depend on pricing data.
func resolveEvalPairGrade(primaryGrade, pair string, ordered []configdomain.RouteTarget, grades map[string][]configdomain.RouteTarget, parentOf map[string]string) (string, bool) {
	if strings.HasPrefix(pair, "grade:") {
		g := strings.TrimPrefix(pair, "grade:")
		if _, ok := grades[g]; ok {
			return g, true
		}
		return "", false
	}
	// "opposite" default.
	if pair != "" && pair != "opposite" {
		return "", false
	}
	order := forward.GradeOrder(ordered, grades, parentOf)
	if len(order) < 2 {
		return "", false
	}
	for i, g := range order {
		if g == primaryGrade {
			if i+1 < len(order) {
				return order[i+1], true
			}
			return order[i-1], true
		}
	}
	return "", false
}

// pickEvalShadowTarget returns the first target of the shadow grade that is not
// operator-disabled and not in cooldown. Shadow eval is measurement, not
// service, so it does not run the full scheduler.
func (p *Proxy) pickEvalShadowTarget(runtime RuntimeSnapshot, gradeTargets []configdomain.RouteTarget) (configdomain.RouteTarget, bool) {
	if len(gradeTargets) == 0 {
		return configdomain.RouteTarget{}, false
	}
	now := time.Now()
	quotaMaxAge := p.quotaFreshnessMaxAge(runtime.Cfg)
	for _, t := range gradeTargets {
		if p.runtimeState.ModelDisabled(t.Provider, t.Model) {
			continue
		}
		rt := runtimestate.Target{Provider: t.Provider, Parent: runtime.ParentOf[t.Provider], Model: t.Model}
		allDown, _, _ := p.runtimeState.CooldownState([]runtimestate.Target{rt}, now, quotaMaxAge)
		if allDown {
			continue
		}
		// Resolve a pooled parent to one runnable virtual.
		if picked, ok := newResolver(p, runtime.Providers, runtime.PoolIndex, runtime.Generation).Pick(t, ""); ok {
			return picked, true
		}
	}
	return configdomain.RouteTarget{}, false
}

// runEvalShadowPair executes the shadow half of an eval pair, then calls the
// judge model and logs the shadow record with the verdict diagnostic.
func (p *Proxy) runEvalShadowPair(
	runtime RuntimeSnapshot,
	shadowRuntime *shadowexec.Runtime,
	stop <-chan struct{},
	proto, backendProto, calledModel, exposed string,
	primary, shadowTarget configdomain.RouteTarget,
	shadowGrade string,
	primaryReqID, primaryAgent, primarySession string,
	reqBody, primaryBody []byte,
	routingDecision *configdomain.RoutingDecision,
) {
	result, ok := p.executeShadow(runtime, shadowRuntime, stop, proto, backendProto, calledModel, exposed, shadowTarget, reqBody, primaryReqID, primaryAgent, primarySession)
	if !ok {
		// Log a minimal shadow record with a diagnostic so the operator can see
		// the eval was attempted and why it produced no verdict.
		p.logEvalShadow(
			runtime, proto, calledModel, exposed, result.Target, primaryReqID,
			primaryAgent, primarySession, result.Request, result.Response,
			result.Started, result.RequestBody, result.Capture,
			[]requestlog.ConversionDiagnostic{{Code: "eval_judge_error", Detail: "shadow execution failed"}},
			routingDecision,
		)
		return
	}

	verdict, diags := p.judgeEvalPair(runtime, stop, proto, exposed, calledModel, primary, result.Target, primaryBody, result.Capture.Body, primaryReqID, primaryAgent, primarySession)
	if shadowGrade != "" {
		diags = append(diags, requestlog.ConversionDiagnostic{Code: "eval_shadow_grade", Detail: shadowGrade})
	}
	if verdict != "" {
		diags = append(diags, requestlog.ConversionDiagnostic{Code: "eval_verdict", Detail: verdict})
	}
	p.logEvalShadow(
		runtime, proto, calledModel, exposed, result.Target, primaryReqID,
		primaryAgent, primarySession, result.Request, result.Response,
		result.Started, result.RequestBody, result.Capture,
		diags,
		routingDecision,
	)
}

// judgeEvalPair calls the configured decisions-protocol judge and returns the
// verdict and any diagnostics. Failures return empty verdict and a diagnostic.
// The judge context derives from the lifecycle stop channel (plus the
// evalJudgeTimeout budget): shutdown must not be dragged past the drain
// window by a single in-flight judge call.
func (p *Proxy) judgeEvalPair(
	runtime RuntimeSnapshot,
	stop <-chan struct{},
	proto, exposed, calledModel string,
	primary, shadowTarget configdomain.RouteTarget,
	primaryBody, shadowBody []byte,
	primaryReqID, primaryAgent, primarySession string,
) (string, []requestlog.ConversionDiagnostic) {
	policy, ok := runtime.Cfg.RoutePolicies[exposed]
	if !ok || policy.Eval == nil {
		return "", []requestlog.ConversionDiagnostic{{Code: "eval_judge_error", Detail: "eval config missing"}}
	}
	judge := policy.Eval.Judge
	state := map[string]any{
		"primary_provider": primary.Provider,
		"primary_model":    primary.Model,
		"shadow_provider":  shadowTarget.Provider,
		"shadow_model":     shadowTarget.Model,
		"primary_response": string(primaryBody),
		"shadow_response":  string(shadowBody),
	}
	ctx, cancel := context.WithTimeout(context.Background(), evalJudgeTimeout)
	defer cancel()
	// Bind the judge to lifecycle stop: unlike the shadow leg (which gets
	// shadowShutdownGrace to finish a nearly-done generation), a judge call
	// is a small decisions request that is safe to cut immediately. The
	// watcher goroutine exits with ctx in every path, so nothing leaks.
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	out := forward.CallEvalJudge(p.forwardServices(), ctx, forward.EvalJudgeInput{
		Runtime:     runtime,
		Target:      judge,
		SessionID:   primarySession,
		RequestID:   primaryReqID,
		Agent:       primaryAgent,
		Proto:       proto,
		Exposed:     exposed,
		CalledModel: calledModel,
		Timeout:     evalJudgeTimeout,
		State:       state,
	})
	if out.Err != nil {
		return "", []requestlog.ConversionDiagnostic{{Code: "eval_judge_error", Detail: out.Err.Error()}}
	}
	return out.Verdict, nil
}

// shadowResult bundles the pieces needed to log a shadow record.
type shadowResult struct {
	Target      configdomain.RouteTarget
	Request     *http.Request
	Response    *http.Response
	Started     time.Time
	RequestBody []byte
	Capture     shadowexec.Capture
	Err         error
}

// executeShadow runs one detached shadow leg and returns its result plus a bool
// indicating whether the result is loggable (response existed). It is shared by
// legacy shadow and eval shadow.
func (p *Proxy) executeShadow(
	runtime RuntimeSnapshot,
	shadowRuntime *shadowexec.Runtime,
	stop <-chan struct{},
	proto, backendProto, calledModel, exposed string,
	target configdomain.RouteTarget,
	reqBody []byte,
	primaryReqID, primaryAgent, primarySession string,
) (shadowResult, bool) {
	var out shadowResult
	if runtime.Cfg == nil {
		logx.Warnf("[shadow] %s: skipped — runtime snapshot has no config (caller bug)", target.Provider)
		return out, false
	}
	logger := p.reqLog
	if logger == nil {
		return out, false
	}
	// Resolve a pooled parent to one runnable virtual, matching the live pipeline.
	if picked, ok := newResolver(p, runtime.Providers, runtime.PoolIndex, runtime.Generation).Pick(target, ""); ok {
		target = picked
	}
	plan, err := forward.PlanTarget(p.forwardServices(), forward.PlanInput{
		Runtime: runtime, Target: target, ClientProto: backendProto, ClientPath: protocol.BackendPath(protocol.Protocol(backendProto)),
	})
	if err != nil {
		logx.Warnf("[shadow] %s: target plan failed: %v", target.Provider, err)
		return out, false
	}
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
	res := shadowRuntime.Execute(ctx, shadowexec.Job{
		Plan:         plan,
		Body:         reqBody,
		CalledModel:  calledModel,
		Client:       &http.Client{Transport: p.transportFor(runtime.Cfg, runtime.ParentOf, target.Provider), Timeout: shadowTimeoutBudget(runtime.Cfg.Scheduling.Timeout())},
		MaxBodyBytes: logger.MaxBodyBytes(),
	})
	if res.Err != nil {
		logx.Warnf("[shadow] %s/%s execution failed: %v", target.Provider, target.Model, res.Err)
		if res.Request == nil || res.Response == nil {
			return shadowResult{Err: res.Err}, false
		}
	}
	return shadowResult{
		Target:      target,
		Request:     res.Request,
		Response:    res.Response,
		Started:     res.Started,
		RequestBody: res.RequestBody,
		Capture:     res.Capture,
		Err:         res.Err,
	}, true
}

// logEvalShadow writes an eval shadow record with diagnostics and the primary
// routing decision attached.
func (p *Proxy) logEvalShadow(
	runtime RuntimeSnapshot,
	proto, calledModel, exposed string,
	target configdomain.RouteTarget,
	primaryReqID, primaryAgent, primarySession string,
	req *http.Request,
	resp *http.Response,
	started time.Time,
	reqBody []byte,
	capture shadowexec.Capture,
	diags []requestlog.ConversionDiagnostic,
	routingDecision *configdomain.RoutingDecision,
) {
	logger := p.reqLog
	if logger == nil {
		return
	}
	execDiags := make([]targetexec.ConversionDiagnostic, len(diags))
	for i, d := range diags {
		execDiags[i] = targetexec.ConversionDiagnostic{Code: d.Code, Detail: d.Detail}
	}
	logInput := forward.BuildRequestLogInput(
		forward.LogCtx{
			RequestID:   "shadow-" + primaryReqID,
			SessionID:   primarySession,
			Exposed:     exposed,
			Agent:       primaryAgent,
			Diagnostics: execDiags,
			Routing:     routingDecision,
		},
		req,
		proto,
		calledModel,
		target,
		resp,
		started,
		reqBody,
	)
	requestlog.Complete(logger, logInput, capture.Body, capture.Total, capture.Truncated)
}

// runShadow sends the same prompt to a candidate backend (shadow evaluation,
// #12): fire-and-forget, the result is logged for offline comparison and NEVER
// returned to the client. It shares targetexec.Plan request preparation but is
// best-effort and bounded — any error is logged and dropped (shadow must never
// affect the live request). Both RuntimeSnapshot and shadowRuntime are captured
// by the primary attempt before launching the goroutine, so reload cannot mix
// config/provider generation with a different semaphore/client bundle.
func (p *Proxy) runShadow(runtime RuntimeSnapshot, shadowRuntime *shadowexec.Runtime, stop <-chan struct{}, proto, bodyProto, calledModel, exposed string, target configdomain.RouteTarget, reqBody []byte, primaryReqID, primaryAgent, primarySession string, diags []requestlog.ConversionDiagnostic, routingDecision *configdomain.RoutingDecision) {
	res, ok := p.executeShadow(runtime, shadowRuntime, stop, proto, bodyProto, calledModel, exposed, target, reqBody, primaryReqID, primaryAgent, primarySession)
	if !ok {
		return
	}
	p.logEvalShadow(
		runtime, proto, calledModel, exposed, res.Target, primaryReqID,
		primaryAgent, primarySession, res.Request, res.Response,
		res.Started, res.RequestBody, res.Capture,
		diags,
		routingDecision,
	)
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
// live streaming generations; a shadow evaluation must not pin one of the few
// concurrency slots (shadow_max_concurrent, default 4) for half an hour when
// its upstream hangs. Five minutes covers any real generation while bounding
// the stall.
const shadowTimeoutCap = 5 * time.Minute

// shadowTimeoutBudget is the shadow client timeout: the configured upstream
// budget capped at shadowTimeoutCap (never larger than either).
func shadowTimeoutBudget(upstream time.Duration) time.Duration {
	if upstream <= 0 || upstream > shadowTimeoutCap {
		return shadowTimeoutCap
	}
	return upstream
}
