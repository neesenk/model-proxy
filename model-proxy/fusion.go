package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	observeevents "model-proxy/internal/observe/events"
)

// fusion.go implements multi-model orchestration (panel → synthesis). A route
// target of {provider: fusion, model: <recipe>} fans the request out to the
// recipe's panel in parallel; each member produces a NON-streaming text draft
// (tools stripped); once a quorum of drafts is in (plus a short grace for
// stragglers), the synthesizer model answers the CLIENT from the original
// conversation + the collected drafts — streamed through the normal target
// path, so the synthesis leg gets auth, conversion, metrics, latency, live
// events, request logging and cache recording for free.
//
// Error semantics: a failed synthesis leg is a hard endpoint (the executor's
// answer, incl. upstream errors, goes to the client as-is — no re-orchestration;
// a route-level failover target may still follow, as for any target). The
// in-engine degradations all answer directly via the synthesizer with the
// ORIGINAL body (half-finished drafts are discarded): cost gates
// (first_turn_only / max_runs_per_day), the tools gate, quorum-shortfall and
// an unbuildable synthesis body. Every run — orchestrated or degraded — is
// recorded in the fusion registry (fusion_obs.go).

const (
	// fusionCandidateMaxChars caps each draft fed into the synthesis body, so a
	// runaway max_tokens on a panel member can't blow up the synthesizer prompt.
	fusionCandidateMaxChars = 24000
	// fusionInstruction is the default synthesizer preamble (do not mention the
	// orchestration; answer directly; emit tool calls when action is needed).
	// FusionConfig.Instruction overrides it.
	fusionInstruction = "你是多模型编排的结果汇总模型。基于原对话和下列候选答案，给出最强的最终答案；不要提及候选/编排过程；需要动作时直接输出工具调用。"
	// fusionJudgeInstruction is the fixed judge preamble: review the candidates
	// (consensus / conflicts / omissions) for the synthesizer — do NOT answer
	// the original question.
	fusionJudgeInstruction = "你是多模型编排的评审模型。基于原对话和下列候选答案，分析它们的共识、冲突与遗漏，输出简短的评审报告供结果汇总模型参考；不要回答原问题；不要提及候选/编排过程。"
)

// fusionGracePeriod is how long draft collection keeps waiting AFTER the quorum
// is met, to absorb nearly-finished stragglers. A var (not a const) so tests
// can shrink it.
var fusionGracePeriod = 5 * time.Second

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

// fusionLegResult is one panel member's outcome: the (truncated) draft text +
// observed usage on success, or the failure reason. status/latencyMs mirror
// what the live end event carries (kept for the fusion registry observation).
type fusionLegResult struct {
	idx       int
	provider  string
	model     string
	text      string
	usage     tokenUsage
	status    int
	latencyMs int64
	err       error
}

// runFusion executes one fusion recipe for the client request. It returns true
// when the synthesizer leg committed a response to the client (success or a
// committed upstream error); false means "nothing committed — fail over to the
// route's next target". workflow is the recipe name (registry/metrics key).
func (p *Proxy) runFusion(fc fusionCtx, workflow string, recipe FusionConfig, w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	run := &fusionRun{
		RunID: fc.flc.requestID, Ts: time.Now().UnixMilli(),
		Route: fc.flc.exposed, Workflow: workflow, Agent: fc.agent, Proto: fc.proto,
	}
	// Cost gate: a first_turn_only recipe only orchestrates the FIRST turn —
	// once the conversation carries an assistant message, answer directly.
	if recipe.FirstTurnOnly && bodyHasAssistantTurn(fc.origBody) {
		run.Degraded = fusionDegradedMultiTurn
		log.Printf("[fusion] %s: workflow %s is first_turn_only and the conversation is multi-turn; answering directly (fusion_multi_turn)",
			fc.flc.exposed, workflow)
		return p.finishFusion(fc, run, recipe.Synthesizer, fc.origBody, w, r, cacheKey)
	}
	// Tool round: drafts answer in plain text (tools stripped), the synthesizer
	// carries the tools and may answer with a tool call directly. When the
	// synthesizer itself can't do tools, orchestration adds nothing — answer
	// directly (degrade, don't fail).
	if requestHasTools(fc.origBody) && !p.fusionSynthesizerSupportsTools(fc, recipe.Synthesizer) {
		run.Degraded = fusionDegradedTools
		log.Printf("[fusion] %s: synthesizer %s/%s lacks tool support; answering directly (fusion_tools_unsupported)",
			fc.flc.exposed, recipe.Synthesizer.Provider, recipe.Synthesizer.Model)
		return p.finishFusion(fc, run, recipe.Synthesizer, fc.origBody, w, r, cacheKey)
	}
	// Budget gate: cap orchestrated runs per local day; an over-budget request
	// degrades to a plain direct call (only admitted runs consume the budget).
	if !p.fusionReg.admit(workflow, recipe.MaxRunsPerDay, time.Now()) {
		run.Degraded = fusionDegradedBudget
		log.Printf("[fusion] %s: workflow %s daily orchestration budget exhausted (%d/day); answering directly (fusion_budget_exceeded)",
			fc.flc.exposed, workflow, recipe.MaxRunsPerDay)
		return p.finishFusion(fc, run, recipe.Synthesizer, fc.origBody, w, r, cacheKey)
	}
	quorum := recipe.MinPanel
	if quorum <= 0 {
		quorum = 2
	}
	if quorum > len(recipe.Panel) {
		quorum = len(recipe.Panel)
	}
	run.Quorum = quorum
	// Fan out: one goroutine per panel member (panel is validate-capped at 4).
	// Legs share the client request's context so a client disconnect cancels
	// them; collectFusionResults cancels the rest on quorum-shortfall/grace.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	results := make(chan fusionLegResult, len(recipe.Panel)) // buffered: late sends never block after cancel
	for i, m := range recipe.Panel {
		go p.callFusionLeg(ctx, fc, i, "fusion-panel", m, fc.origBody, results)
	}
	legs, received := collectFusionResults(results, len(recipe.Panel), quorum, cancel)
	run.DraftsUsed = len(legs)
	run.Legs = reconcileFusionLegs(recipe.Panel, received)
	if len(legs) < quorum {
		run.Degraded = fusionDegradedInsufficient
		log.Printf("[fusion] %s: fusion_insufficient_proposers (%d/%d drafts, quorum %d); answering directly via synthesizer",
			fc.flc.exposed, len(legs), len(recipe.Panel), quorum)
		return p.finishFusion(fc, run, recipe.Synthesizer, fc.origBody, w, r, cacheKey)
	}
	candidates := make([]string, 0, len(legs))
	for _, l := range legs {
		candidates = append(candidates, l.text)
	}
	// Judge (optional): one non-streaming review of the candidates, injected
	// into the synthesis body. Uses a FRESH context — the fan-out ctx above may
	// already be cancelled by the collection. A judge failure only skips the
	// report; it never degrades the run.
	judgeReport := ""
	if recipe.Judge != nil {
		obs, report := p.runFusionJudge(r.Context(), fc, *recipe.Judge, candidates)
		run.Legs = append(run.Legs, obs)
		if report != "" {
			judgeReport = report
			run.JudgeUsed = true
		}
	}
	synthBody, ok := buildSynthesisBody(fc.origBody, fc.proto, candidates, judgeReport, recipe.Instruction)
	if !ok {
		run.Degraded = fusionDegradedBodyBuild
		log.Printf("[fusion] %s: cannot build synthesis body; answering directly via synthesizer", fc.flc.exposed)
		return p.finishFusion(fc, run, recipe.Synthesizer, fc.origBody, w, r, cacheKey)
	}
	log.Printf("[fusion] %s: synthesizing from %d/%d drafts (quorum %d)", fc.flc.exposed, len(legs), len(recipe.Panel), quorum)
	return p.finishFusion(fc, run, recipe.Synthesizer, synthBody, w, r, cacheKey)
}

// finishFusion runs the synthesis leg (direct original body, or the
// synthesis-augmented one) and records the run. The synthesis leg's
// status/latency/tokens are read back from the live-event hub's recent ring —
// attemptExecutor publishes that end event on commit, just before returning, so it
// is already visible here. The run then lands in the fusion registry and the
// ("fusion", <workflow>) metrics counters.
func (p *Proxy) finishFusion(fc fusionCtx, run *fusionRun, st RouteTarget, body []byte, w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	run.SynthCommitted = p.callFusionSynthesizer(fc, st, body, w, r, cacheKey)
	if run.SynthCommitted {
		if ev, ok := p.events.FindEnd(run.RunID); ok {
			run.SynthStatus = ev.Status
			run.SynthLatencyMs = ev.LatencyMs
			run.SynthInput = ev.Input
			run.SynthOutput = ev.Output
		}
	}
	p.fusionReg.record(run)
	if p.metrics != nil {
		p.metrics.inc("fusion", run.Workflow, evFusionRuns)
		if run.Degraded != "" {
			p.metrics.inc("fusion", run.Workflow, evFusionDegraded)
		}
	}
	return run.SynthCommitted
}

// collectFusionResults gathers panel-leg results until the quorum is met (plus
// a grace window for stragglers) or the quorum becomes unreachable. It cancels
// the remaining legs on an early exit and returns the quorum-validated drafts
// (nil when fewer than quorum succeeded — the caller degrades to a direct
// synthesizer call) plus EVERY result received so far (successes + failures),
// so the registry can report per-leg outcomes; legs still in flight at an
// early exit are cut (their late results land in the buffered channel).
func collectFusionResults(results <-chan fusionLegResult, launched, quorum int, cancel context.CancelFunc) (successes, received []fusionLegResult) {
	var graceTimer *time.Timer
	var grace <-chan time.Time
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	inFlight := launched
	for inFlight > 0 {
		// Quorum unreachable even if every in-flight leg succeeds → cut losses.
		if len(successes)+inFlight < quorum {
			cancel()
			return nil, received
		}
		select {
		case res := <-results:
			inFlight--
			received = append(received, res)
			if res.err == nil {
				successes = append(successes, res)
			}
			// Quorum just met with legs still running → start the grace window
			// (once) so a nearly-done straggler still makes the synthesis.
			if len(successes) >= quorum && graceTimer == nil && inFlight > 0 {
				graceTimer = time.NewTimer(fusionGracePeriod)
				grace = graceTimer.C
			}
		case <-grace:
			// Quorum already met (the timer only starts then) — cut stragglers.
			cancel()
			return successes, received
		}
	}
	if len(successes) < quorum {
		return nil, received
	}
	return successes, received
}

// fusionSynthesizerSupportsTools reports whether the synthesizer model handles
// tool calls, judged by the provider's capabilities override first, then the
// models.dev catalog (nil catalog → fits, the graceful default).
func (p *Proxy) fusionSynthesizerSupportsTools(fc fusionCtx, st RouteTarget) bool {
	return modelFits(fc.runtime.catalog, targetCapabilities(fc.runtime.cfg, fc.runtime.parentOf, st), st.Model, requestProfile{hasTools: true})
}

// callFusionLeg runs one non-streaming fusion sub-call: a panel member's
// candidate branch (tag "fusion-panel", idx >= 0) or the judge analysis (tag
// "fusion-judge", idx -1). Pipeline: build-gate (fail closed), circuit gate,
// non-streaming tool-stripped sub-call on srcBody, then health/metrics/usage
// recording and the request log — the leg NEVER touches the client; results
// go to `out`. The tag shapes the request-log id ("fusion-panel-<i>-<parent>"
// / "fusion-judge-<parent>") and the live-event provider marker
// ("<tag>:<model>").
func (p *Proxy) callFusionLeg(ctx context.Context, fc fusionCtx, idx int, tag string, m RouteTarget, srcBody []byte, out chan<- fusionLegResult) {
	// Resolve the member's provider to a runnable virtual via the unified resolver
	// (pooled parent → one healthy account, session-sticky via fc.sessionKey with
	// failover to a sibling). A pooled parent name has no runtime instance, so
	// without this a multi-account member was always dropped as "not available" the
	// moment a second account was added. On !ok (unknown / not logged in / all
	// accounts unhealthy) leave m as-is and let the build gate below report it.
	if picked, ok := newResolver(p, fc.runtime.providers, fc.runtime.poolIndex).Pick(m, fc.sessionKey); ok {
		m = picked
	}
	res := fusionLegResult{idx: idx, provider: m.Provider, model: m.Model}
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
		res.status = status
		res.latencyMs = time.Since(start).Milliseconds()
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
			LatencyMs:     res.latencyMs,
			Input:         res.usage.Input,
			Output:        res.usage.Output,
		})
		out <- res
	}()

	// Credential/build gate (fail closed): an unbuildable member is dropped
	// BEFORE any upstream call and only counts into the quorum math.
	plan, err := p.planTarget(targetPlanInput{
		runtime: fc.runtime, target: m, clientProto: fc.proto, clientPath: fc.upPath,
	})
	if err != nil {
		res.err = err
		return
	}
	provCfg := plan.providerCfg
	impl := plan.providerImpl
	if impl == nil {
		res.err = fmt.Errorf("provider %s not available", m.Provider)
		return
	}
	// Circuit gate (same availability rule as attemptExecutor): skip members the
	// breaker has open. record* below all clear the half-open slot; the deferred
	// release is idempotent and covers the paths that don't record.
	if !p.takeHalfOpenSlot(m.Provider, fc.runtime.generation) {
		res.err = errFusionLegUnavailable
		return
	}
	defer p.releaseHalfOpenSlot(m.Provider, fc.runtime.generation)
	sched := fc.runtime.cfg.Scheduling

	// Responses chain expansion (same rule as forward): only when the client
	// spoke responses AND this leg's backend is stateless — a native-responses
	// backend keeps previous_response_id passthrough and its server-side chain.
	srcBody, _ = p.expandFusionResponses(fc, string(plan.backendProto), srcBody)
	body := plan.rewriteModel(srcBody, fc.calledModel)
	body, err = plan.convertBody(body)
	if err != nil {
		res.err = fmt.Errorf("convert %s→%s: %w", fc.proto, plan.backendProto, err)
		return
	}
	// Draft legs are non-streaming and tool-free: the draft only produces a
	// text analysis; tools/tool_choice are the synthesizer's job.
	body = stripFusionDraftFields(body)

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
		targetURL := strings.TrimRight(plan.baseURL, "/") + plan.upPath
		targetURL, body = impl.RewriteRequest(targetURL, body, plan.upPath)
		body = p.applyParamBlock(m.Provider, m.Model, body)
		req, err = http.NewRequestWithContext(legCtx, http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			res.err = err
			return
		}
		req.Header.Set("content-type", "application/json")
		if err := impl.AuthHeaders(req); err != nil {
			res.err = fmt.Errorf("auth: %w", err)
			return
		}
		impl.ExtraHeaders(req, plan.upPath)
		for k, v := range provCfg.Headers {
			req.Header.Set(k, v)
		}

		resp, err = p.client.Do(req)
		if err != nil {
			// A fusion-level cancel (grace expired / quorum unreachable / client
			// disconnect) is NOT a provider failure — the leg was simply cut.
			if ctx.Err() == context.Canceled {
				res.err = errFusionLegUnavailable
				return
			}
			p.recordFailure(m.Provider, sched, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.inc(m.Provider, m.Model, evFailures)
				p.metrics.inc(m.Provider, m.Model, evFailovers) // leg abandoned, like tryTarget
			}
			res.err = err
			return
		}
		status = resp.StatusCode
		respBody, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			p.recordFailure(m.Provider, sched, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.inc(m.Provider, m.Model, evFailures)
				p.metrics.inc(m.Provider, m.Model, evFailovers) // leg abandoned, like tryTarget
			}
			res.err = err
			return
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshedAuth {
			refreshedAuth = true
			if refreshErr := impl.Refresh(); refreshErr == nil {
				continue
			}
		}
		if resp.StatusCode == http.StatusBadRequest && !strippedParam {
			if param, found := parseUnsupportedParam(respBody); found {
				p.learnParamBlock(m.Provider, m.Model, param, fc.runtime.generation)
				if stripped, changed := stripTopLevelParam(body, param); changed {
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
		until, kind := p.parseRateLimit(resp, peek, time.Now(), sched)
		p.recordRateLimit(m.Provider, until, kind, fc.runtime.generation)
		if p.metrics != nil {
			p.metrics.inc(m.Provider, m.Model, evRateLimited429)
			p.metrics.inc(m.Provider, m.Model, evFailovers) // leg abandoned, like tryTarget
		}
		res.err = errFusionLegUnavailable
	case resp.StatusCode >= 500:
		p.recordFailure(m.Provider, sched, fc.runtime.generation)
		if p.metrics != nil {
			p.metrics.inc(m.Provider, m.Model, evFailures)
			p.metrics.inc(m.Provider, m.Model, evFailovers) // leg abandoned, like tryTarget
		}
		res.err = fmt.Errorf("upstream status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized:
		p.recordFailure(m.Provider, sched, fc.runtime.generation)
		if p.metrics != nil {
			p.metrics.inc(m.Provider, m.Model, evFailovers) // like tryTarget: failover only, no evFailures
		}
		res.err = fmt.Errorf("upstream status %d after auth refresh", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || isModelDenied(resp.StatusCode, respBody):
		// Wire-verdict 404 correction (same as tryTarget): this leg was
		// converted to /responses because the probe verdict said the endpoint
		// supports it — a 404 here means the VERDICT was wrong, not the model.
		// Flip the verdict (persisted; later legs use chat) and skip the model
		// lock so the model doesn't take the blame for our protocol choice.
		if plan.viaResponsesVerdict && resp.StatusCode == http.StatusNotFound {
			p.noteWireResponsesMiss(m.Provider)
			log.Printf("[fusion provider=%s] /responses 404 after wire verdict — provider responses downgraded to no (model NOT locked)",
				m.Provider)
		} else {
			p.recordModelFailure(m.Provider, m.Model, sched, fc.runtime.generation)
		}
		if p.metrics != nil {
			p.metrics.inc(m.Provider, m.Model, evFailovers) // leg abandoned, like tryTarget's failover
		}
		res.err = fmt.Errorf("model unavailable (status %d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		// 4xx (non-429): client-class error — no candidate, but the provider is
		// healthy; don't poison the circuit.
		if p.metrics != nil {
			p.metrics.inc(m.Provider, m.Model, evFailures)
		}
		res.err = fmt.Errorf("upstream status %d", resp.StatusCode)
	default:
		res.usage = parseUsageJSON(respBody)
		res.text = truncateRunes(plan.extractResponseText(respBody), fusionCandidateMaxChars)
		if res.text == "" {
			res.err = errFusionEmptyDraft
			p.recordModelFailure(m.Provider, m.Model, sched, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.inc(m.Provider, m.Model, evFailovers) // leg abandoned, like tryTarget's empty-200 failover
			}
		} else {
			p.recordSuccess(m.Provider, m.Model, fc.runtime.generation)
			if p.metrics != nil {
				p.metrics.inc(m.Provider, m.Model, evRequests)
				latencyMs := time.Since(start).Milliseconds()
				p.metrics.addLatency(m.Provider, m.Model, uint64(latencyMs), uint64(latencyMs))
			}
			// Usage is accounted per leg (internal books stay accurate; the
			// client's own usage comes from the synthesizer, unmodified).
			if p.tokens != nil {
				p.tokens.commit(tokenKey{Provider: m.Provider, Model: m.Model}, res.usage)
			}
			p.agents.addTokens(fc.agent, m.Provider, m.Model, res.usage)
		}
	}
	// Request log: each leg records under its own id (fusion-panel-<i>-<parent>
	// / fusion-judge-<parent>) so per-leg detail is filterable by prefix in the
	// log / API.
	if logger := p.reqLog; logger != nil {
		logger.record(logger.buildRecord(recordInputs{
			flc:         forwardLogCtx{requestID: legID, exposed: fc.flc.exposed},
			r:           req,
			proto:       fc.proto,
			calledModel: fc.calledModel,
			t:           m,
			resp:        resp,
			start:       start,
			requestBody: body,
			captured:    respBody,
			total:       int64(len(respBody)),
		}))
	}
}

// callFusionSynthesizer sends the (possibly synthesis-augmented) body to the
// synthesizer model through the normal attemptExecutor path — streaming, conversion,
// auth, metrics, latency, live events, request log and cache all apply.
func (p *Proxy) callFusionSynthesizer(fc fusionCtx, st RouteTarget, body []byte, w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	// Resolve to a runnable virtual (pooled parent → one healthy account,
	// session-sticky so a conversation reuses one synthesizer account), same as
	// the panel legs — otherwise a multi-account synthesizer has no impl and fails.
	// FAIL CLOSED on resolver failure: proceeding with the unresolved (pooled
	// parent) name would hand attemptExecutor a nil impl and fail closed.
	picked, ok := newResolver(p, fc.runtime.providers, fc.runtime.poolIndex).Pick(st, fc.sessionKey)
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
	if plan.providerImpl == nil {
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
	body, _ = p.expandFusionResponses(fc, string(plan.backendProto), body)
	_, responsesHistory := p.expandFusionResponses(fc, string(plan.backendProto), fc.origBody)
	body = plan.rewriteModel(body, fc.calledModel)
	body, err = plan.convertBody(body)
	if err != nil {
		// Fail CLOSED: a conversion failure must not send the unconverted body
		// to a different backend protocol.
		log.Printf("[fusion] synthesizer %s/%s %s→%s convert failed: %v — aborting synthesis",
			st.Provider, st.Model, fc.proto, plan.backendProto, err)
		return false
	}
	// The log ctx carries NO origBody so the request log stores the actual
	// synthesis body (with the candidate sections), not the client's original.
	flc := forwardLogCtx{requestID: fc.flc.requestID, attempt: fc.flc.attempt, exposed: fc.flc.exposed}
	attempt := newTargetAttempt(
		fc.runtime,
		plan,
		attemptExchange{
			request: r,
			writer:  w,
			body:    body,
		},
		attemptScope{
			calledModel:      fc.calledModel,
			agent:            fc.agent,
			cacheKey:         cacheKey,
			log:              flc,
			responseContext:  plan.responseContext(fc.origBody),
			responsesHistory: responsesHistory,
			responsesSession: fc.sessionKey,
		},
		attemptPolicy{lastTarget: true},
	)
	committed, _, _, _ := p.targetExecutor().execute(attempt)
	return committed
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

// runFusionJudge runs the optional judge leg: one non-streaming review of the
// original conversation + the collected candidates, through the same
// build/circuit/metrics pipeline as the candidate branches (callFusionLeg).
// It returns the leg observation (always recorded on the run) and the report
// text — empty on any failure, in which case the synthesis simply proceeds
// without it (a judge failure never degrades the run).
func (p *Proxy) runFusionJudge(ctx context.Context, fc fusionCtx, jt RouteTarget, candidates []string) (fusionLegObs, string) {
	body, ok := buildFusionJudgeBody(fc.origBody, fc.proto, candidates)
	if !ok {
		log.Printf("[fusion] %s: cannot build judge body; synthesizing without judge report", fc.flc.exposed)
		return fusionLegObs{Provider: jt.Provider, Model: jt.Model, Kind: "judge", Err: "cannot build judge body"}, ""
	}
	out := make(chan fusionLegResult, 1) // synchronous use: one result, never blocks
	p.callFusionLeg(ctx, fc, -1, "fusion-judge", jt, body, out)
	res := <-out
	obs := obsFromLegResult(res, "judge")
	if res.err != nil {
		log.Printf("[fusion] %s: judge %s/%s failed: %v; synthesizing without judge report",
			fc.flc.exposed, jt.Provider, jt.Model, res.err)
		return obs, ""
	}
	return obs, res.text
}

// buildSynthesisBody derives the synthesizer's request body from the original
// client body: the instruction (recipe override, else the fixed template) +
// the optional judge report (before the candidates) + the candidate drafts
// (shuffled to avoid position bias, each capped) are appended as a system
// section (anthropic) or an extra trailing user message (openai), leaving the
// conversation untouched. ok=false when the original body isn't usable JSON
// (caller degrades to direct).
func buildSynthesisBody(origBody []byte, proto string, candidates []string, judgeReport, instruction string) ([]byte, bool) {
	if instruction == "" {
		instruction = fusionInstruction
	}
	var sb strings.Builder
	sb.WriteString(instruction)
	if judgeReport != "" {
		fmt.Fprintf(&sb, "\n\n<JUDGE_ANALYSIS>\n%s\n</JUDGE_ANALYSIS>", judgeReport)
	}
	for i, ci := range rand.Perm(len(candidates)) {
		fmt.Fprintf(&sb, "\n\n<CANDIDATE %d>\n%s\n</CANDIDATE %d>", i+1, candidates[ci], i+1)
	}
	return injectFusionSection(origBody, proto, sb.String())
}

// buildFusionJudgeBody derives the judge's request body from the original
// client body: the fixed judge instruction + the candidates (in collection
// order — the judge only reviews, no position-bias shuffle needed).
func buildFusionJudgeBody(origBody []byte, proto string, candidates []string) ([]byte, bool) {
	var sb strings.Builder
	sb.WriteString(fusionJudgeInstruction)
	for i, c := range candidates {
		fmt.Fprintf(&sb, "\n\n<CANDIDATE %d>\n%s\n</CANDIDATE %d>", i+1, c, i+1)
	}
	return injectFusionSection(origBody, proto, sb.String())
}

// injectFusionSection appends section to the request body's system prompt
// (anthropic), as an extra trailing user message (openai chat), or as a typed
// trailing input message item (responses), leaving the conversation untouched.
// ok=false when the body isn't usable JSON (or has an unexpected system shape).
func injectFusionSection(origBody []byte, proto, section string) ([]byte, bool) {
	var v map[string]any
	if err := json.Unmarshal(origBody, &v); err != nil {
		return nil, false
	}
	if proto == "anthropic" {
		switch sys := v["system"].(type) {
		case nil:
			v["system"] = section
		case string:
			v["system"] = sys + "\n\n" + section
		case []any:
			v["system"] = append(sys, map[string]any{"type": "text", "text": section})
		default:
			return nil, false
		}
	} else if proto == "responses" {
		// Responses bodies use input — never invent a parallel messages key
		// (the r→chat converter reads ONLY input). The injected item must be a
		// TYPED message: the converter switches on item type and drops untyped
		// maps. A string input is normalized to a typed item first.
		msg := map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": section}},
		}
		switch input := v["input"].(type) {
		case []any:
			v["input"] = append(input, msg)
		case string:
			var items []any
			if input != "" {
				items = append(items, map[string]any{
					"type": "message", "role": "user",
					"content": []any{map[string]any{"type": "input_text", "text": input}},
				})
			}
			v["input"] = append(items, msg)
		default:
			v["input"] = []any{msg}
		}
	} else {
		// openai chat/completions: extra trailing user message.
		msg := map[string]any{"role": "user", "content": section}
		if msgs, ok := v["messages"].([]any); ok {
			v["messages"] = append(msgs, msg)
		} else {
			v["messages"] = []any{msg}
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}

// stripFusionDraftFields turns a client request body into a draft-leg body:
// non-streaming, with tools/tool_choice removed (the draft only produces a
// text analysis). Best-effort: an unparseable body passes through unchanged.
func stripFusionDraftFields(body []byte) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	delete(v, "tools")
	delete(v, "tool_choice")
	delete(v, "stream_options")
	v["stream"] = false
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// parseUsageJSON extracts token usage from a non-streaming response body. One
// struct covers both protocol shapes (anthropic input_tokens/output_tokens —
// also the responses-API names — and openai prompt_tokens/completion_tokens)
// since their field names are disjoint.
func parseUsageJSON(body []byte) tokenUsage {
	var v struct {
		Usage struct {
			InputTokens      uint64 `json:"input_tokens"`
			OutputTokens     uint64 `json:"output_tokens"`
			CacheCreation    uint64 `json:"cache_creation_input_tokens"`
			CacheRead        uint64 `json:"cache_read_input_tokens"`
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return tokenUsage{}
	}
	return tokenUsage{
		Input:         v.Usage.InputTokens + v.Usage.PromptTokens,
		Output:        v.Usage.OutputTokens + v.Usage.CompletionTokens,
		CacheCreation: v.Usage.CacheCreation,
		CacheRead:     v.Usage.CacheRead,
	}
}

// truncateRunes caps s at max runes (runes, not bytes, so CJK drafts aren't cut
// mid-character).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
