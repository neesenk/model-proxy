package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"model-proxy/internal/observe/counters"
	"net/http"
	"strings"
	"time"

	"model-proxy/internal/fusion"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/routing"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
)

// fusion.go adapts one captured application runtime to internal/fusion.Engine.
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

// fusionGracePeriod is how long draft collection keeps waiting AFTER the quorum
// is met, to absorb nearly-finished stragglers. A var (not a const) so tests
// can shrink it.
var fusionGracePeriod = fusion.DefaultGracePeriod

var (
	errFusionLegUnavailable = errors.New("fusion leg unavailable")
	errFusionEmptyDraft     = errors.New("fusion draft has no text")
)

// fusionCtx bundles the per-request values the engine threads into panel legs
// and the synthesizer call (all snapshotted by forward under p.mu).
type fusionCtx struct {
	runtime     runtimeSnapshot
	proto       string // client protocol ("anthropic"|"openai")
	calledModel string
	upPath      string // client request path (/v1 stripped for openai)
	agent       string
	sessionKey  string // client session id — makes pooled members/synthesizer session-sticky (cache-warm)
	origBody    []byte
	flc         forwardLogCtx // parent request id + exposed route
}

// runFusion executes one fusion recipe for the client request. It returns true
// when the synthesizer leg committed a response to the client (success or a
// committed upstream error); false means "nothing committed — fail over to the
// route's next target". workflow is the recipe name (registry/metrics key).
func (p *Proxy) runFusion(fc fusionCtx, workflow string, recipe FusionConfig, w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	engine := fusion.Engine{Registry: p.fusionReg, GracePeriod: fusionGracePeriod}
	result := engine.Run(r.Context(), fusion.Request{
		Workflow:     workflow,
		RunID:        fc.flc.requestID,
		Route:        fc.flc.exposed,
		Agent:        fc.agent,
		Protocol:     fc.proto,
		OriginalBody: fc.origBody,
		HasTools:     routing.RequestHasTools(fc.origBody),
		Recipe:       recipe,
	}, fusionAdapter{proxy: p, context: fc, writer: w, request: r, cacheKey: cacheKey})
	if p.metrics != nil {
		p.metrics.Inc("fusion", result.Run.Workflow, counters.EvFusionRuns)
		if result.Run.Degraded != "" {
			p.metrics.Inc("fusion", result.Run.Workflow, counters.EvFusionDegraded)
		}
	}
	return result.Committed
}

type fusionAdapter struct {
	proxy    *Proxy
	context  fusionCtx
	writer   http.ResponseWriter
	request  *http.Request
	cacheKey string
}

var _ fusion.Ports = fusionAdapter{}

func (adapter fusionAdapter) SupportsTools(target RouteTarget) bool {
	return adapter.proxy.fusionSynthesizerSupportsTools(adapter.context, target)
}

func (adapter fusionAdapter) CallLeg(ctx context.Context, call fusion.LegCall) fusion.LegResult {
	tag := "fusion-" + call.Kind
	return adapter.proxy.callFusionLeg(ctx, adapter.context, call.Index, tag, call.Target, call.Body)
}

func (adapter fusionAdapter) Synthesize(target RouteTarget, body []byte) fusion.SynthesisResult {
	result := fusion.SynthesisResult{
		Committed: adapter.proxy.callFusionSynthesizer(
			adapter.context,
			target,
			body,
			adapter.writer,
			adapter.request,
			adapter.cacheKey,
		),
	}
	if result.Committed {
		if event, ok := adapter.proxy.events.FindEnd(adapter.context.flc.requestID); ok {
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
func (p *Proxy) fusionSynthesizerSupportsTools(fc fusionCtx, st RouteTarget) bool {
	return routing.Fits(
		fc.runtime.catalog,
		routing.CapabilitiesFor(fc.runtime.cfg, fc.runtime.parentOf, st),
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
func (p *Proxy) callFusionLeg(ctx context.Context, fc fusionCtx, idx int, tag string, m RouteTarget, srcBody []byte) (res fusion.LegResult) {
	// Resolve the member's provider to a runnable virtual via the unified resolver
	// (pooled parent → one healthy account, session-sticky via fc.sessionKey with
	// failover to a sibling). A pooled parent name has no runtime instance, so
	// without this a multi-account member was always dropped as "not available" the
	// moment a second account was added. On !ok (unknown / not logged in / all
	// accounts unhealthy) leave m as-is and let the build gate below report it.
	if picked, ok := newResolver(
		p,
		fc.runtime.providers,
		fc.runtime.poolIndex,
		fc.runtime.generation,
	).Pick(m, fc.sessionKey); ok {
		m = picked
	}
	res = fusion.LegResult{Index: idx, Provider: m.Provider, Model: m.Model}
	legID := tag + "-" + fc.flc.requestID
	if idx >= 0 {
		legID = fmt.Sprintf("%s-%d-%s", tag, idx, fc.flc.requestID)
	}
	marker := tag + ":" + m.Model
	start := time.Now()
	status := http.StatusBadGateway // pre-upstream failures report as 502
	// Live monitor: fusion legs are visible while they run (progressive reveal),
	// marked "<tag>:<model>" to distinguish them from direct targets.
	p.events.Publish(observeevents.Event{
		Type:      "start",
		Ts:        start.UnixMilli(),
		RequestID: legID,
		Agent:     fc.agent,
		Protocol:  fc.proto,
		Exposed:   fc.flc.exposed,
		Provider:  marker,
	})
	defer func() {
		res.Status = status
		res.LatencyMs = time.Since(start).Milliseconds()
		p.events.Publish(observeevents.Event{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     legID,
			Agent:         fc.agent,
			Protocol:      fc.proto,
			Exposed:       fc.flc.exposed,
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
	plan, err := p.planTarget(targetPlanInput{
		runtime: fc.runtime, target: m, clientProto: fc.proto, clientPath: fc.upPath,
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
	// breaker has open. record* below all clear the half-open slot; the deferred
	// release is idempotent and covers the paths that don't record.
	if !p.takeHalfOpenSlot(m.Provider, fc.runtime.generation) {
		res.Err = errFusionLegUnavailable
		return
	}
	defer p.releaseHalfOpenSlot(m.Provider, fc.runtime.generation)
	sched := fc.runtime.cfg.Scheduling

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
	var (
		req           *http.Request
		resp          *http.Response
		respBody      []byte
		refreshedAuth bool
		strippedParam bool
	)
	// Match the normal target pipeline's unsupported-parameter behavior: apply
	// learned blocks before send, then learn/strip/retry one newly reported
	// top-level parameter immediately.
	for {
		targetURL := strings.TrimRight(plan.BaseURL(), "/") + plan.UpstreamPath()
		targetURL, body = impl.RewriteRequest(targetURL, body, plan.UpstreamPath())
		body = p.applyParamBlock(m.Provider, m.Model, body)
		req, err = http.NewRequestWithContext(legCtx, http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			res.Err = err
			return
		}
		req.Header.Set("content-type", "application/json")
		if err := impl.AuthHeaders(req); err != nil {
			res.Err = fmt.Errorf("auth: %w", err)
			return
		}
		plan.ApplyConfiguredHeaders(req.Header)
		impl.ExtraHeaders(req, plan.UpstreamPath())

		resp, err = p.client.Do(req)
		if err != nil {
			// A fusion-level cancel (grace expired / quorum unreachable / client
			// disconnect) is NOT a provider failure — the leg was simply cut.
			if ctx.Err() == context.Canceled {
				res.Err = errFusionLegUnavailable
				return
			}
			p.recordFailure(m.Provider, sched, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.Inc(m.Provider, m.Model, counters.EvFailures)
				p.metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like tryTarget
			}
			res.Err = err
			return
		}
		status = resp.StatusCode
		respBody, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			p.recordFailure(m.Provider, sched, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.Inc(m.Provider, m.Model, counters.EvFailures)
				p.metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like tryTarget
			}
			res.Err = err
			return
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshedAuth {
			refreshedAuth = true
			if refreshErr := impl.Refresh(); refreshErr == nil {
				continue
			}
		}
		if resp.StatusCode == http.StatusBadRequest && !strippedParam {
			if param, found := targetexec.ParseUnsupportedParam(respBody); found {
				p.learnParamBlock(m.Provider, m.Model, param, fc.runtime.generation)
				if stripped, changed := targetexec.StripTopLevelParam(body, param); changed {
					body = stripped
					strippedParam = true
					log.Printf("[fusion provider=%s] 400 unsupported parameter %q — stripped, retrying",
						m.Provider, param)
					continue
				}
			}
		}
		break
	}
	switch {
	case resp.StatusCode == 429:
		peek := respBody
		if len(peek) > 8<<10 {
			peek = peek[:8<<10]
		}
		decision := targetexec.ParseRateLimit(resp, peek, time.Now(), sched)
		p.recordRateLimit(m.Provider, decision.Until, runtimestate.ParseRateLimitKind(string(decision.Kind)), fc.runtime.generation)
		if p.metrics != nil {
			p.metrics.Inc(m.Provider, m.Model, counters.EvRateLimited429)
			p.metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like tryTarget
		}
		res.Err = errFusionLegUnavailable
	case resp.StatusCode >= 500:
		p.recordFailure(m.Provider, sched, fc.runtime.generation)
		if p.metrics != nil {
			p.metrics.Inc(m.Provider, m.Model, counters.EvFailures)
			p.metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like tryTarget
		}
		res.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized:
		p.recordFailure(m.Provider, sched, fc.runtime.generation)
		if p.metrics != nil {
			p.metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // like tryTarget: failover only, no counters.EvFailures
		}
		res.Err = fmt.Errorf("upstream status %d after auth refresh", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || targetexec.IsModelDenied(resp.StatusCode, respBody):
		// Wire-verdict 404 correction (same as tryTarget): this leg was
		// converted to /responses because the probe verdict said the endpoint
		// supports it — a 404 here means the VERDICT was wrong, not the model.
		// Flip the verdict (persisted; later legs use chat) and skip the model
		// lock so the model doesn't take the blame for our protocol choice.
		if plan.ViaResponsesVerdict() && resp.StatusCode == http.StatusNotFound {
			p.noteWireResponsesMiss(m.Provider)
			log.Printf("[fusion provider=%s] /responses 404 after wire verdict — provider responses downgraded to no (model NOT locked)",
				m.Provider)
		} else {
			p.recordModelFailure(m.Provider, m.Model, sched, fc.runtime.generation)
		}
		if p.metrics != nil {
			p.metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like tryTarget's failover
		}
		res.Err = fmt.Errorf("model unavailable (status %d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		// 4xx (non-429): client-class error — no candidate, but the provider is
		// healthy; don't poison the circuit.
		if p.metrics != nil {
			p.metrics.Inc(m.Provider, m.Model, counters.EvFailures)
		}
		res.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	default:
		res.Usage = fusion.ParseUsage(respBody)
		res.Text = fusion.TruncateRunes(plan.ExtractResponseText(respBody), fusionCandidateMaxChars)
		if res.Text == "" {
			res.Err = errFusionEmptyDraft
			p.recordModelFailure(m.Provider, m.Model, sched, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.Inc(m.Provider, m.Model, counters.EvFailovers) // leg abandoned, like tryTarget's empty-200 failover
			}
		} else {
			p.recordSuccess(m.Provider, m.Model, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.Inc(m.Provider, m.Model, counters.EvRequests)
				latencyMs := time.Since(start).Milliseconds()
				p.metrics.AddLatency(m.Provider, m.Model, uint64(latencyMs), uint64(latencyMs))
			}
			// Usage is accounted per leg (internal books stay accurate; the
			// client's own usage comes from the synthesizer, unmodified).
			usage := counters.TokenUsage{
				Input:         res.Usage.Input,
				Output:        res.Usage.Output,
				CacheCreation: res.Usage.CacheCreation,
				CacheRead:     res.Usage.CacheRead,
			}
			if p.tokens != nil {
				p.tokens.Commit(counters.TokenKey{Provider: m.Provider, Model: m.Model}, usage)
			}
			p.agents.AddTokens(fc.agent, m.Provider, m.Model, usage)
		}
	}
	// Request log: each leg records under its own id (fusion-panel-<i>-<parent>
	// / fusion-judge-<parent>) so per-leg detail is filterable by prefix in the
	// log / API.
	if logger := p.reqLog; logger != nil {
		logInput := requestLogInput(
			forwardLogCtx{requestID: legID, exposed: fc.flc.exposed},
			req,
			fc.proto,
			fc.calledModel,
			m,
			resp,
			start,
			body,
		)
		completeRequestLog(logger, logInput, respBody, int64(len(respBody)), false)
	}
	return res
}

// callFusionSynthesizer sends the (possibly synthesis-augmented) body to the
// synthesizer model through the normal targetexec.Executor path — streaming, conversion,
// auth, metrics, latency, live events, request log and cache all apply.
func (p *Proxy) callFusionSynthesizer(fc fusionCtx, st RouteTarget, body []byte, w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	// Resolve to a runnable virtual (pooled parent → one healthy account,
	// session-sticky so a conversation reuses one synthesizer account), same as
	// the panel legs — otherwise a multi-account synthesizer has no impl and fails.
	// FAIL CLOSED on resolver failure: proceeding with the unresolved (pooled
	// parent) name would hand targetexec.Executor a nil impl and fail closed.
	picked, ok := newResolver(
		p,
		fc.runtime.providers,
		fc.runtime.poolIndex,
		fc.runtime.generation,
	).Pick(st, fc.sessionKey)
	if !ok {
		log.Printf("[fusion] %s: synthesizer %s/%s unavailable (unknown provider, not logged in, or no healthy pooled account) — aborting synthesis",
			fc.flc.exposed, st.Provider, st.Model)
		return false
	}
	st = picked
	plan, err := p.planTarget(targetPlanInput{
		runtime: fc.runtime, target: st, clientProto: fc.proto, clientPath: fc.upPath,
	})
	if err != nil {
		log.Printf("[fusion] %s: synthesizer target plan failed: %v", fc.flc.exposed, err)
		return false
	}
	if plan.Provider() == nil {
		log.Printf("[fusion] %s: synthesizer provider %q has no runtime implementation (not logged in)", fc.flc.exposed, st.Provider)
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
		log.Printf("[fusion] synthesizer %s/%s %s→%s convert failed: %v — aborting synthesis",
			st.Provider, st.Model, fc.proto, plan.BackendProtocol(), err)
		return false
	}
	// The log ctx carries NO origBody so the request log stores the actual
	// synthesis body (with the candidate sections), not the client's original.
	flc := forwardLogCtx{requestID: fc.flc.requestID, attempt: fc.flc.attempt, exposed: fc.flc.exposed}
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
				RequestID:    flc.requestID,
				Attempt:      flc.attempt,
				Exposed:      flc.exposed,
				OriginalBody: flc.origBody,
			},
			ResponseContext:  plan.ResponseContext(fc.origBody),
			ResponsesHistory: responsesHistory,
			ResponsesSession: fc.sessionKey,
		},
		targetexec.Policy{LastTarget: true},
	)
	return p.targetExecutor(attempt.Runtime()).Execute(attempt).Committed
}

// expandFusionResponses mirrors forward's responses-state expansion for one
// fusion sub-call body (panel leg / judge / synthesizer): it applies only when
// the client spoke the responses protocol AND this leg's backend is stateless
// (backendProto != responses) — a native-responses backend keeps
// previous_response_id passthrough and its own server-side chain. On expansion
// failure the UNEXPANDED body is sent: a broken chain degrades context but must
// not kill the whole run (forward fails closed per-target; fusion has no
// per-leg target list to fall through). Returns the body to send plus the
// merged history for post-response recording (nil when not applicable).
func (p *Proxy) expandFusionResponses(fc fusionCtx, backendProto string, body []byte) ([]byte, []any) {
	if fc.proto != "responses" || backendProto == "responses" || p.responsesState == nil {
		return body, nil
	}
	expanded, history, hit, err := p.responsesState.Expand(body, fc.sessionKey)
	if err != nil {
		log.Printf("[fusion] %s: responses state expansion failed: %v — sending unexpanded body", fc.flc.exposed, err)
		return body, nil
	}
	if p.responsesPreviousID(body) != "" && !hit {
		log.Printf("[fusion] %s: previous_response_id cache miss; repaired orphaned continuation items", fc.flc.exposed)
	}
	return expanded, history
}
