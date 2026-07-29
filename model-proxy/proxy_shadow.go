package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"model-proxy/internal/protocol"
	"model-proxy/internal/transport/bodycapture"
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
	commit *attemptCommit,
) {
	if commit == nil || p.reqLog == nil || len(runtime.cfg.Shadow) == 0 {
		return
	}
	shadow, ok := runtime.cfg.Shadow[exposed]
	if !ok || shadow.Provider == "" || shadow.Provider == primary.Provider {
		return
	}
	shadowRuntime := runtime.shadow
	if shadowRuntime == nil || !shadowRuntime.shouldSample() {
		return
	}
	select {
	case shadowRuntime.sem <- struct{}{}:
		if !p.lifecycle.runBeforeLogDrain(func() {
			defer func() { <-shadowRuntime.sem }()
			p.runShadow(
				runtime,
				shadowRuntime,
				proto,
				backendProto,
				calledModel,
				exposed,
				shadow,
				commit.requestBody,
				primaryRequestID,
			)
		}) {
			<-shadowRuntime.sem
		}
	default:
		// Shadow concurrency cap reached → skip (best-effort).
	}
}

// shadowRuntime is the reload-swappable shadow dispatch state. reload replaces
// the whole bundle via an atomic store; each dispatch loads it once, so in-flight
// goroutines finish on the bundle they started with (same sem/client) while new
// traffic follows the reloaded sample rate / concurrency cap / client timeout.
type shadowRuntime struct {
	sem      chan struct{} // buffered concurrency gate (cap = max concurrent)
	client   *http.Client  // shared HTTP client for shadow requests
	sampRate float64       // 0-1; fraction of requests to shadow (1.0 = all, 0 = off)
}

// newShadowRuntime builds the shadow dispatch bundle from a config (used by both
// NewProxy and reload so the two stay in sync).
func newShadowRuntime(cfg *Config) *shadowRuntime {
	maxConc := cfg.ShadowMaxConcurrent
	if maxConc <= 0 {
		maxConc = 4
	}
	sr := &shadowRuntime{
		sem:      make(chan struct{}, maxConc),
		client:   &http.Client{Timeout: cfg.Scheduling.Timeout()},
		sampRate: 1.0, // default; nil ShadowSampleRate = all requests
	}
	if cfg.ShadowSampleRate != nil {
		sr.sampRate = *cfg.ShadowSampleRate // explicit 0.0 = off
	}
	return sr
}

// shouldSample reports whether this request should be shadow-evaluated, based on
// the configured sample rate (1.0 = all, 0.5 = half, 0 = none). A nil sem means
// shadowing is not configured.
func (sr *shadowRuntime) shouldSample() bool {
	if sr == nil || sr.sem == nil {
		return false
	}
	if sr.sampRate >= 1 {
		return true
	}
	if sr.sampRate <= 0 {
		return false
	}
	return rand.Float64() < sr.sampRate
}

// shouldShadow reports whether this request should be shadow-evaluated, based on
// the currently-loaded shadow runtime's sample rate. Reload-aware: the runtime
// pointer is swapped atomically, so a config change (e.g. sample_rate: 0) takes
// effect immediately without a restart.
func (p *Proxy) shouldShadow() bool {
	return p.shadow.Load().shouldSample()
}

// runShadow sends the same prompt to a candidate backend (shadow evaluation,
// #12): fire-and-forget, the result is logged for offline comparison and NEVER
// returned to the client. It shares targetPlan request preparation but is
// best-effort and bounded — any error is logged and dropped (shadow must never
// affect the live request). Both runtimeSnapshot and shadowRuntime are captured
// by the primary attempt before launching the goroutine, so reload cannot mix
// config/provider generation with a different semaphore/client bundle.
//
// `bodyProto` is the protocol of reqBody (the primary target's backend proto —
// reqBody may already be converted from the client's proto). The shadow backend's
// own protocol is shadow.Protocol (defaulting to bodyProto); runShadow selects the
// shadow base URL + path for THAT protocol and converts the body if it differs.
func (p *Proxy) runShadow(runtime runtimeSnapshot, shadowRuntime *shadowRuntime, proto, bodyProto, calledModel, exposed string, shadow ShadowTarget, reqBody []byte, primaryReqID string) {
	if runtime.cfg == nil {
		// Defensive: runtimeSnapshot is handed around as a plain value — a
		// future call site that forgets to populate it must not nil-deref
		// below (runtime.cfg.Scheduling.Timeout()). Log loudly and skip.
		log.Printf("[shadow] %s: skipped — runtime snapshot has no config (caller bug)", shadow.Provider)
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
	target := RouteTarget{Provider: shadow.Provider, Model: shadow.Model, Protocol: shadow.Protocol}
	picked, ok := newResolver(
		p,
		runtime.providers,
		runtime.poolIndex,
		runtime.generation,
	).Pick(target, "")
	if !ok {
		log.Printf("[shadow] %s: provider not available (no runnable healthy virtual)", shadow.Provider)
		return
	}
	target = picked
	shadow.Provider = target.Provider
	plan, err := p.planTarget(targetPlanInput{
		runtime: runtime, target: target, clientProto: bodyProto, clientPath: protocol.BackendPath(protocol.Protocol(bodyProto)),
	})
	if err != nil {
		log.Printf("[shadow] %s: target plan failed: %v", shadow.Provider, err)
		return
	}
	provCfg := plan.providerCfg
	impl := plan.providerImpl
	if impl == nil {
		log.Printf("[shadow] %s: provider not available", shadow.Provider)
		return
	}
	// Shadow backend protocol: declared, else the provider's ProtocolHint
	// (auto-resolve, e.g. codex→responses), else the wire verdict, else same as
	// the body's. Route + convert accordingly so the shadow gets a request in
	// the protocol IT speaks.
	sbody := plan.wire.RewriteModel(reqBody, calledModel)
	sbody, err = plan.wire.ConvertBody(sbody)
	if err != nil {
		// Fail CLOSED: don't send the unconverted body to the shadow backend.
		log.Printf("[shadow] %s: %s→%s convert failed: %v — skipping",
			shadow.Provider, bodyProto, plan.backendProto, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtime.cfg.Scheduling.Timeout())
	defer cancel()
	targetURL := strings.TrimRight(plan.baseURL, "/") + plan.upPath
	if impl != nil {
		targetURL, sbody = impl.RewriteRequest(targetURL, sbody, plan.upPath)
	}
	sreq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(sbody))
	if err != nil {
		log.Printf("[shadow] %s: build req: %v", shadow.Provider, err)
		return
	}
	sreq.Header.Set("content-type", "application/json")
	if impl != nil {
		if err := impl.AuthHeaders(sreq); err != nil {
			log.Printf("[shadow] %s: auth: %v", shadow.Provider, err)
			return
		}
		impl.ExtraHeaders(sreq, plan.upPath)
	}
	for k, v := range provCfg.Headers {
		sreq.Header.Set(k, v)
	}
	client := shadowRuntime.client
	start := time.Now()
	resp, err := client.Do(sreq)
	if err != nil {
		log.Printf("[shadow] %s/%s upstream error: %v", shadow.Provider, shadow.Model, err)
		return
	}
	// Drain the shadow response into a bounded capture for the log. The reader
	// passes all bytes through (drained to Discard) while teeing a capped copy.
	var captured []byte
	var capturedTotal int64
	var capturedTruncated bool
	cr := bodycapture.New(resp.Body, logger.MaxBodyBytes(), func(body []byte, total int64, truncated bool) {
		captured = append([]byte(nil), body...)
		capturedTotal = total
		capturedTruncated = truncated
	})
	_, _ = io.Copy(io.Discard, cr)
	_ = cr.Close()
	logInput := requestLogInput(
		forwardLogCtx{requestID: "shadow-" + primaryReqID, exposed: exposed},
		sreq,
		proto,
		calledModel,
		RouteTarget{Provider: shadow.Provider, Model: shadow.Model},
		resp,
		start,
		sbody,
	)
	completeRequestLog(logger, logInput, captured, capturedTotal, capturedTruncated)
}
