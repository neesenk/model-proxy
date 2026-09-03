package forward

import (
	"context"
	"errors"
	"fmt"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"net/http"
	"time"

	"model-proxy/internal/fusion"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

// fusion.go adapts one captured runtime snapshot to internal/fusion.Engine.
// The engine owns gates, fan-out, quorum/grace, judge/body construction and
// registry recording; this file owns generation-bound leg execution and normal
// target-executor delivery for the client-facing synthesizer.
//
// Error semantics: a failed synthesis leg is a hard endpoint (the executor's
// answer, incl. upstream errors, goes to the client as-is — no re-orchestration;
// a route-level failover target may still follow, as for any target). The
// in-engine degradations all answer directly via the synthesizer with the
// ORIGINAL body (half-finished drafts are discarded): cost gates
// (first_turn_only / max_runs_per_day), the tools gate, quorum-shortfall and
// an unbuildable synthesis body. Every run — orchestrated or degraded — is
// recorded in internal/fusion.Registry.

const (
	// fusionCandidateMaxChars caps each draft fed into the synthesis body, so a
	// runaway max_tokens on a panel member can't blow up the synthesizer prompt.
	fusionCandidateMaxChars = 24000
)

// FusionGracePeriod is how long draft collection keeps waiting AFTER the quorum
// is met, to absorb nearly-finished stragglers. A var (not a const) so tests
// can shrink it.
var FusionGracePeriod = fusion.DefaultGracePeriod

var (
	errFusionLegUnavailable = errors.New("fusion leg unavailable")
	errFusionEmptyDraft     = errors.New("fusion draft has no text")
)

// fusionCtx bundles the per-request values the engine threads into panel legs
// and the synthesizer call (all from the single snapshot captured for the
// request — the single-snapshot red line).
type fusionCtx struct {
	runtime     Snapshot
	proto       string // client protocol ("anthropic"|"openai"|"responses")
	calledModel string
	upPath      string // client request path (/v1 stripped for openai)
	agent       string
	sessionKey  string // client session id — makes pooled members/synthesizer session-sticky (cache-warm)
	origBody    []byte
	flc         LogCtx // parent request id + exposed route
}

// runFusion executes one fusion recipe for the client request. It returns true
// when the synthesizer leg committed a response to the client (success or a
// committed upstream error); false means "nothing committed — fail over to the
// route's next target". workflow is the recipe name (registry/metrics key).
func (p pipeline) runFusion(fc fusionCtx, workflow string, recipe FusionConfig, w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	engine := fusion.Engine{Registry: p.svc.FusionReg, GracePeriod: FusionGracePeriod}
	result := engine.Run(r.Context(), fusion.Request{
		Workflow:     workflow,
		RunID:        fc.flc.RequestID,
		Route:        fc.flc.Exposed,
		Agent:        fc.agent,
		Protocol:     fc.proto,
		OriginalBody: fc.origBody,
		HasTools:     routing.RequestHasTools(fc.origBody),
		Recipe:       recipe,
	}, fusionAdapter{pipe: p, context: fc, writer: w, request: r, cacheKey: cacheKey})
	if p.svc.Metrics != nil {
		p.svc.Metrics.Inc("fusion", result.Run.Workflow, counters.EvFusionRuns)
		if result.Run.Degraded != "" {
			p.svc.Metrics.Inc("fusion", result.Run.Workflow, counters.EvFusionDegraded)
		}
	}
	return result.Committed
}

type fusionAdapter struct {
	pipe     pipeline
	context  fusionCtx
	writer   http.ResponseWriter
	request  *http.Request
	cacheKey string
}

var _ fusion.Ports = fusionAdapter{}

func (adapter fusionAdapter) SupportsTools(target RouteTarget) bool {
	return adapter.pipe.fusionSynthesizerSupportsTools(adapter.context, target)
}

func (adapter fusionAdapter) CallLeg(ctx context.Context, call fusion.LegCall) fusion.LegResult {
	tag := "fusion-" + call.Kind
	return adapter.pipe.callFusionLeg(ctx, adapter.context, call.Index, tag, call.Target, call.Body)
}

func (adapter fusionAdapter) Synthesize(target RouteTarget, body []byte) fusion.SynthesisResult {
	result := fusion.SynthesisResult{
		Committed: adapter.pipe.callFusionSynthesizer(
			adapter.context,
			target,
			body,
			adapter.writer,
			adapter.request,
			adapter.cacheKey,
		),
	}
	if result.Committed {
		if event, ok := adapter.pipe.svc.Events.FindEnd(adapter.context.flc.RequestID); ok {
			result.Status = event.Status
			result.LatencyMs = event.LatencyMs
			result.Input = event.Input
			result.Output = event.Output
		}
	}
	return result
}

// fusionSynthesizerSupportsTools reports whether the synthesizer model handles
// tool calls, judged by the provider's capabilities override first, then the
// models.dev catalog (nil catalog → fits, the graceful default).
func (p pipeline) fusionSynthesizerSupportsTools(fc fusionCtx, st RouteTarget) bool {
	return routing.Fits(
		fc.runtime.Catalog,
		routing.CapabilitiesFor(fc.runtime.Cfg, fc.runtime.ParentOf, st),
		st.Model,
		routing.Profile{HasTools: true},
	)
}

// callFusionLeg runs one non-streaming fusion sub-call: a panel member's
// candidate branch (tag "fusion-panel", idx >= 0) or the judge analysis (tag
// "fusion-judge", idx -1). Pipeline: build-gate (fail closed), circuit gate,
// non-streaming tool-stripped sub-call on srcBody, then health/metrics/usage
// recording and the request log — the leg NEVER touches the client. The tag
// shapes the request-log id ("fusion-panel-<i>-<parent>" /
// "fusion-judge-<parent>") and the live-event provider marker
// ("<tag>:<model>").
func (p pipeline) callFusionLeg(ctx context.Context, fc fusionCtx, idx int, tag string, m RouteTarget, srcBody []byte) (res fusion.LegResult) {
	// Resolve the member's provider to a runnable virtual via the unified resolver
	// (pooled parent → one healthy account, session-sticky via fc.sessionKey with
	// failover to a sibling). A pooled parent name has no runtime instance, so
	// without this a multi-account member was always dropped as "not available" the
	// moment a second account was added. On !ok (unknown / not logged in / all
	// accounts unhealthy) leave m as-is and let the build gate below report it.
	if picked, ok := routing.NewResolver(
		p.svc.ResolverState,
		fc.runtime.Providers,
		fc.runtime.PoolIndex,
		fc.runtime.Generation,
	).Pick(m, fc.sessionKey); ok {
		m = picked
	}
	res = fusion.LegResult{Index: idx, Provider: m.Provider, Model: m.Model}
	legID := tag + "-" + fc.flc.RequestID
	if idx >= 0 {
		legID = fmt.Sprintf("%s-%d-%s", tag, idx, fc.flc.RequestID)
	}
	marker := tag + ":" + m.Model
	start := time.Now()
	status := http.StatusBadGateway // pre-upstream failures report as 502
	// Live monitor: fusion legs are visible while they run (progressive reveal),
	// marked "<tag>:<model>" to distinguish them from direct targets.
	p.svc.Events.Publish(observeevents.Event{
		Type:      "start",
		Ts:        start.UnixMilli(),
		RequestID: legID,
		Agent:     fc.agent,
		Protocol:  fc.proto,
		Exposed:   fc.flc.Exposed,
		Provider:  marker,
	})
	defer func() {
		res.Status = status
		res.LatencyMs = time.Since(start).Milliseconds()
		p.svc.Events.Publish(observeevents.Event{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     legID,
			Agent:         fc.agent,
			Protocol:      fc.proto,
			Exposed:       fc.flc.Exposed,
			Provider:      marker,
			UpstreamModel: m.Model,
			Status:        status,
			LatencyMs:     res.LatencyMs,
			Input:         res.Usage.Input,
			Output:        res.Usage.Output,
		})
	}()

	// Credential/build gate (fail closed): an unbuildable member is dropped
	// BEFORE any upstream call and only counts into the quorum math.
	plan, err := p.planTarget(PlanInput{
		Runtime: fc.runtime, Target: m, ClientProto: fc.proto, ClientPath: fc.upPath,
	})
	if err != nil {
		res.Err = err
		return
	}
	impl := plan.Provider()
	if impl == nil {
		res.Err = fmt.Errorf("provider %s not available", m.Provider)
		return
	}
	// Circuit gate (same availability rule as targetexec.Executor): skip members the
	// breaker has open. Record* below all clear the half-open slot; the deferred
	// release is idempotent and covers the paths that don't record. The gate is
	// bound to the REQUEST snapshot's parent projection (single-snapshot red
	// line): the wire-verdict 404 correction below records under this
	// generation's parent name.
	gate := p.svc.NewHealthGate(fc.runtime.ParentOf)
	if !gate.TakeHalfOpenSlot(m.Provider, fc.runtime.Generation) {
		res.Err = errFusionLegUnavailable
		return
	}
	defer gate.ReleaseHalfOpenSlot(m.Provider, fc.runtime.Generation)
	sched := fc.runtime.Cfg.Scheduling

	// Responses chain expansion (same rule as forward): only when the client
	// spoke responses AND this leg's backend is stateless — a native-responses
	// backend keeps previous_response_id passthrough and its server-side chain.
	srcBody, _ = p.expandFusionResponses(fc, string(plan.BackendProtocol()), srcBody)
	body := plan.RewriteModel(srcBody, fc.calledModel)
	body, err = plan.ConvertBody(body)
	if err != nil {
		res.Err = fmt.Errorf("convert %s→%s: %w", fc.proto, plan.BackendProtocol(), err)
		return
	}
	// Draft legs are non-streaming and tool-free: the draft only produces a
	// text analysis; tools/tool_choice are the synthesizer's job.
	body = fusion.StripDraftFields(body)

	legCtx, cancelLeg := context.WithTimeout(ctx, sched.Timeout())
	defer cancelLeg()
	// The leg transport (URL build, provider rewrite, param-block application,
	// send, one-shot 401 refresh / 400 param learn-strip retries) is owned by
	// targetexec.BufferedLeg — the headless counterpart of the streaming
	// Executor. Effect recording (circuit/metrics/rate-limit) stays here.
	exchange := &targetexec.BufferedLegExchange{}
	legStatus, respBody, err := targetexec.BufferedLeg{
		Client:  p.svc.Client,
		Plan:    plan,
		MaxBody: 64 << 20,
		ApplyParamBlock: func(body []byte) []byte {
			return gate.ApplyParamBlock(m.Provider, m.Model, body)
		},
		LearnParamBlock: func(param string) {
			gate.LearnParamBlock(m.Provider, m.Model, param, fc.runtime.Generation)
		},
		OnStripParam: func(param string) {
			logx.Warnf("[fusion provider=%s] 400 unsupported parameter %q — stripped, retrying",
				m.Provider, param)
		},
		Capture: exchange,
	}.Do(legCtx, body)
	// status only advances on a real upstream response; pre-upstream failures
	// (build/auth/transport with no earlier response) keep the 502 default.
	if legStatus != 0 {
		status = legStatus
	}
	if err != nil {
		// Pre-wire failures (request build / auth header construction) drop the
		// leg without recording a provider failure — the upstream was never
		// contacted.
		var buildErr *targetexec.BufferedLegBuildError
		if errors.As(err, &buildErr) {
			res.Err = err
			return
		}
		// A fusion-level cancel (grace expired / quorum unreachable / client
		// disconnect) is NOT a provider failure — the leg was simply cut. The
		// same rule covers a cancel cutting the leg mid-body: the upstream
		// never got to finish, it was simply abandoned.
		if ctx.Err() == context.Canceled {
			res.Err = errFusionLegUnavailable
			return
		}
		gate.RecordFailure(m.Provider, sched, fc.runtime.Generation)
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailures)
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like a direct target
		}
		res.Err = err
		return
	}
	req := exchange.Request
	resp := exchange.Response
	body = exchange.SentBody
	switch {
	case resp.StatusCode == 429:
		peek := respBody
		if len(peek) > 8<<10 {
			peek = peek[:8<<10]
		}
		decision := targetexec.ParseRateLimit(resp, peek, time.Now(), sched)
		gate.RecordRateLimit(m.Provider, decision.Until, string(decision.Kind), fc.runtime.Generation)
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvRateLimited429)
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like a direct target
		}
		res.Err = errFusionLegUnavailable
	case resp.StatusCode >= 500:
		gate.RecordFailure(m.Provider, sched, fc.runtime.Generation)
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailures)
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like a direct target
		}
		res.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized:
		gate.RecordFailure(m.Provider, sched, fc.runtime.Generation)
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // like a direct target: failover only, no counters.EvFailures
		}
		res.Err = fmt.Errorf("upstream status %d after auth refresh", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || targetexec.IsModelDenied(resp.StatusCode, respBody):
		// Wire-verdict 404 correction (same as a direct target): this leg was
		// converted to /responses because the probe verdict said the endpoint
		// supports it — a 404 here means the VERDICT was wrong, not the model.
		// Flip the verdict (persisted; later legs use chat) and skip the model
		// lock so the model doesn't take the blame for our protocol choice.
		if plan.ViaResponsesVerdict() && resp.StatusCode == http.StatusNotFound {
			// The gate resolves the pool parent from the REQUEST snapshot
			// projection it was bound with (nil-safe): this in-flight leg
			// belongs to fc.runtime's generation (single-snapshot red line).
			gate.NoteWireResponsesMiss(m.Provider, m.Model)
			logx.Warnf("[fusion provider=%s] /responses 404 after wire verdict — provider responses downgraded to no (model NOT locked)",
				m.Provider)
		} else {
			gate.RecordModelFailure(m.Provider, m.Model, sched, fc.runtime.Generation)
		}
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like a direct target's failover
		}
		res.Err = fmt.Errorf("model unavailable (status %d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		// 4xx (non-429): client-class error — no candidate, but the provider is
		// healthy; don't poison the circuit.
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailures)
		}
		res.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	default:
		res.Usage = fusion.ParseUsage(respBody)
		res.Text = fusion.TruncateRunes(plan.ExtractResponseText(respBody), fusionCandidateMaxChars)
		if res.Text == "" {
			res.Err = errFusionEmptyDraft
			gate.RecordModelFailure(m.Provider, m.Model, sched, fc.runtime.Generation)
			if p.svc.Metrics != nil {
				p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like a direct target's empty-200 failover
			}
		} else {
			gate.RecordSuccess(m.Provider, m.Model, fc.runtime.Generation)
			if p.svc.Metrics != nil {
				p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvRequests)
				latencyMs := time.Since(start).Milliseconds()
				p.svc.Metrics.AddLatency(m.Provider, m.Model, uint64(latencyMs), uint64(latencyMs))
			}
			// Usage is accounted per leg (internal books stay accurate; the
			// client's own usage comes from the synthesizer, unmodified).
			usage := counters.TokenUsage{
				Input:         res.Usage.Input,
				Output:        res.Usage.Output,
				CacheCreation: res.Usage.CacheCreation,
				CacheRead:     res.Usage.CacheRead,
			}
			if p.svc.Tokens != nil {
				p.svc.Tokens.Commit(counters.TokenKey{Provider: m.Provider, Model: m.Model}, usage)
			}
			if p.svc.Agents != nil {
				p.svc.Agents.AddTokens(fc.agent, m.Provider, m.Model, usage)
			}
		}
	}
	// Request log: each leg records under its own id (fusion-panel-<i>-<parent>
	// / fusion-judge-<parent>) so per-leg detail is filterable by prefix in the
	// log / API.
	if logger := p.svc.ReqLog; logger != nil {
		logInput := BuildRequestLogInput(
			LogCtx{RequestID: legID, Exposed: fc.flc.Exposed},
			req,
			fc.proto,
			fc.calledModel,
			m,
			resp,
			start,
			body,
		)
		requestlog.Complete(logger, logInput, respBody, int64(len(respBody)), false)
	}
	return res
}

// callFusionSynthesizer sends the (possibly synthesis-augmented) body to the
// synthesizer model through the normal targetexec.Executor path — streaming, conversion,
// auth, metrics, latency, live events, request log and cache all apply.
func (p pipeline) callFusionSynthesizer(fc fusionCtx, st RouteTarget, body []byte, w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	// Resolve to a runnable virtual (pooled parent → one healthy account,
	// session-sticky so a conversation reuses one synthesizer account), same as
	// the panel legs — otherwise a multi-account synthesizer has no impl and fails.
	// FAIL CLOSED on resolver failure: proceeding with the unresolved (pooled
	// parent) name would hand targetexec.Executor a nil impl and fail closed.
	picked, ok := routing.NewResolver(
		p.svc.ResolverState,
		fc.runtime.Providers,
		fc.runtime.PoolIndex,
		fc.runtime.Generation,
	).Pick(st, fc.sessionKey)
	if !ok {
		logx.Warnf("[fusion] %s: synthesizer %s/%s unavailable (unknown provider, not logged in, or no healthy pooled account) — aborting synthesis",
			fc.flc.Exposed, st.Provider, st.Model)
		return false
	}
	st = picked
	plan, err := p.planTarget(PlanInput{
		Runtime: fc.runtime, Target: st, ClientProto: fc.proto, ClientPath: fc.upPath,
	})
	if err != nil {
		logx.Warnf("[fusion] %s: synthesizer target plan failed: %v", fc.flc.Exposed, err)
		return false
	}
	if plan.Provider() == nil {
		logx.Warnf("[fusion] %s: synthesizer provider %q has no runtime implementation (not logged in)", fc.flc.Exposed, st.Provider)
		return false
	}
	// Responses chain expansion (same rule as forward): expand + orphan repair
	// only for a stateless (non-responses) backend (native-responses backends
	// keep previous_response_id passthrough and server-side chaining). TWO
	// expansions on purpose: the SEND body is the synthesis-augmented one, but
	// the RECORDED history is the expansion of the CLIENT-VISIBLE conversation
	// (origBody) — the injected instruction/candidate scaffolding is ephemeral
	// per-turn and must not be replayed into later turns as if the user said it.
	body, _ = p.expandFusionResponses(fc, string(plan.BackendProtocol()), body)
	_, responsesHistory := p.expandFusionResponses(fc, string(plan.BackendProtocol()), fc.origBody)
	body = plan.RewriteModel(body, fc.calledModel)
	body, err = plan.ConvertBody(body)
	if err != nil {
		// Fail CLOSED: a conversion failure must not send the unconverted body
		// to a different backend protocol.
		logx.Warnf("[fusion] synthesizer %s/%s %s→%s convert failed: %v — aborting synthesis",
			st.Provider, st.Model, fc.proto, plan.BackendProtocol(), err)
		return false
	}
	// The log ctx carries NO origBody so the request log stores the actual
	// synthesis body (with the candidate sections), not the client's original.
	flc := LogCtx{RequestID: fc.flc.RequestID, Attempt: fc.flc.Attempt, Exposed: fc.flc.Exposed}
	attempt := newTargetAttempt(
		fc.runtime,
		plan,
		targetexec.Exchange{
			Request: r,
			Writer:  w,
			Body:    body,
		},
		targetexec.Scope{
			CalledModel: fc.calledModel,
			Agent:       fc.agent,
			CacheKey:    cacheKey,
			Log: targetexec.LogContext{
				RequestID:    flc.RequestID,
				Attempt:      flc.Attempt,
				Exposed:      flc.Exposed,
				OriginalBody: flc.OrigBody,
			},
			ResponseContext:  plan.ResponseContext(fc.origBody),
			ResponsesHistory: responsesHistory,
			ResponsesSession: fc.sessionKey,
		},
		targetexec.Policy{LastTarget: true},
	)
	return p.targetExecutor(attempt.Runtime(), fc.runtime.ParentOf).Execute(attempt).Committed
}
