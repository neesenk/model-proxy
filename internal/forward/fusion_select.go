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

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

// fusion_select.go adapts internal/fusion's selector port to the decisions
// protocol: it builds a System One body from the engine's semantic
// SelectRequest, executes it through the same generation-bound leg machinery
// as panel legs (resolver, plan, health gate, BufferedLeg, metrics, request
// log, live events) and projects the typed answers back to fusion.SelectResult.
//
// It lives in its own file because internal/forward/fusion.go is contract-bound
// to reuse targetexec.Plan helpers WITHOUT importing internal/protocol; the
// decisions wire shape (BuildSystemOneBody / ParseSystemOneResponse) is a
// protocol-package concern.

// SelectPanel implements fusion.Ports.SelectPanel: one decisions call picking
// the best candidate and scoring difficulty. The per-call timeout comes from
// the recipe's selector config (default 800ms), NOT from scheduling — a slow
// decision must fail fast into the static-panel fallback.
func (adapter fusionAdapter) SelectPanel(ctx context.Context, req fusion.SelectRequest) fusion.SelectResult {
	sel := adapter.selector
	if sel == nil {
		return fusion.SelectResult{Err: errors.New("selector not configured")}
	}
	return adapter.pipe.callFusionSelector(ctx, adapter.context, *sel, req)
}

// callFusionSelector executes the decisions leg. It mirrors callFusionLeg's
// health/metrics/observation semantics with two deliberate differences: the
// body is built in decisions shape (no StripDraftFields, no tool stripping —
// the questions ARE the payload) and the response is parsed as typed answers
// (an empty-answers 200 is a model failure, like an empty draft).
func (p pipeline) callFusionSelector(ctx context.Context, fc fusionCtx, sel configdomain.SelectorConfig, req fusion.SelectRequest) (res fusion.SelectResult) {
	m := sel.Target
	if picked, ok := routing.NewResolver(
		p.svc.ResolverState,
		fc.runtime.Providers,
		fc.runtime.PoolIndex,
		fc.runtime.Generation,
	).Pick(m, fc.sessionKey); ok {
		m = picked
	}
	legID := "fusion-select-" + fc.flc.RequestID
	marker := "fusion-select:" + m.Model
	start := time.Now()
	status := http.StatusBadGateway // pre-upstream failures report as 502
	p.svc.Events.Publish(observeevents.Event{
		Type:      "start",
		Ts:        start.UnixMilli(),
		RequestID: legID,
		SessionID: fc.flc.SessionID,
		Agent:     fc.agent,
		Protocol:  fc.proto,
		Exposed:   fc.flc.Exposed,
		Provider:  marker,
	})
	defer func() {
		res.LatencyMs = time.Since(start).Milliseconds()
		p.svc.Events.Publish(observeevents.Event{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     legID,
			SessionID:     fc.flc.SessionID,
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

	fail := func(err error) fusion.SelectResult {
		res.Err = err
		return res
	}

	questions := map[string]protocol.SystemOneQuestion{
		"model_choice": {
			Type:         "choice",
			Instructions: req.ChoiceInstructions,
			Criteria:     selectorCriteria(req.Candidates),
		},
		"difficulty": {
			Type:         "score",
			Instructions: req.DifficultyInstructions,
			Criteria:     append([]string(nil), req.DifficultyLevels...),
		},
	}
	body, err := protocol.BuildSystemOneBody(m.Model, req.State, questions)
	if err != nil {
		return fail(err)
	}

	// Credential/build gate (fail closed), same as panel legs.
	plan, err := p.planTarget(PlanInput{
		Runtime: fc.runtime, Target: m, ClientProto: "decisions", ClientPath: "/v1/decisions",
	})
	if err != nil {
		return fail(err)
	}
	if plan.Provider() == nil {
		return fail(fmt.Errorf("provider %s not available", m.Provider))
	}
	gate := p.svc.NewHealthGate(fc.runtime.ParentOf)
	if !gate.TakeHalfOpenSlot(m.Provider, fc.runtime.Generation) {
		return fail(errFusionLegUnavailable)
	}
	defer gate.ReleaseHalfOpenSlot(m.Provider, fc.runtime.Generation)
	sched := fc.runtime.Cfg.Scheduling

	body, err = plan.ConvertBody(body)
	if err != nil {
		return fail(fmt.Errorf("convert decisions→%s: %w", plan.BackendProtocol(), err))
	}

	legCtx, cancelLeg := context.WithTimeout(ctx, sel.TimeoutDuration())
	defer cancelLeg()
	exchange := &targetexec.BufferedLegExchange{}
	legStatus, respBody, err := targetexec.BufferedLeg{
		Client:  p.clientFor(fc.runtime.Cfg, fc.runtime.ParentOf, m.Provider),
		Plan:    plan,
		MaxBody: 64 << 20,
		ApplyParamBlock: func(body []byte) []byte {
			return gate.ApplyParamBlock(m.Provider, m.Model, body)
		},
		LearnParamBlock: func(param string) {
			gate.LearnParamBlock(m.Provider, m.Model, param, fc.runtime.Generation)
		},
		OnStripParam: func(param string) {
			logx.Warnf("[fusion provider=%s] 400 unsupported parameter %q — stripped, retrying", m.Provider, param)
		},
		Capture: exchange,
	}.Do(legCtx, body)
	if legStatus != 0 {
		status = legStatus
	}
	if err != nil {
		var buildErr *targetexec.BufferedLegBuildError
		if errors.As(err, &buildErr) {
			return fail(err)
		}
		if ctx.Err() == context.Canceled {
			return fail(errFusionLegUnavailable)
		}
		// The selector's OWN short deadline (default 800ms) expiring is a
		// policy fallback, not an upstream-health verdict: RecordFailure
		// counts toward the provider circuit breaker and would penalize the
		// chat/panel legs sharing this provider for what is only "the
		// decisions model was slower than the budget". Real transport errors
		// and a parent-context deadline (the genuine request timeout) still
		// count. The selector observation (run.Selector.err) already records
		// the timeout for /api/fusion.
		if errors.Is(err, context.DeadlineExceeded) &&
			legCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return fail(err)
		}
		gate.RecordFailure(m.Provider, sched, fc.runtime.Generation)
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailures)
		}
		return fail(err)
	}
	upstreamReq := exchange.Request
	resp := exchange.Response
	sentBody := exchange.SentBody
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
		}
		res.Err = errFusionLegUnavailable
	case resp.StatusCode >= 500:
		gate.RecordFailure(m.Provider, sched, fc.runtime.Generation)
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailures)
		}
		res.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized:
		gate.RecordFailure(m.Provider, sched, fc.runtime.Generation)
		res.Err = fmt.Errorf("upstream status %d after auth refresh", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || targetexec.IsModelDenied(resp.StatusCode, respBody):
		gate.RecordModelFailure(m.Provider, m.Model, sched, fc.runtime.Generation)
		res.Err = fmt.Errorf("model unavailable (status %d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvFailures)
		}
		res.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	default:
		model, answers, usage, parseErr := protocol.ParseSystemOneResponse(respBody)
		if parseErr != nil {
			res.Err = parseErr
			gate.RecordModelFailure(m.Provider, m.Model, sched, fc.runtime.Generation)
			break
		}
		choice, ok := answers["model_choice"]
		if !ok || choice.Choice == "" {
			res.Err = fmt.Errorf("decisions response missing model_choice answer")
			gate.RecordModelFailure(m.Provider, m.Model, sched, fc.runtime.Generation)
			break
		}
		res.Model = model
		res.ChoiceID = choice.Choice
		res.Confidence = choice.Confidence
		res.Probabilities = choice.Probabilities
		if difficulty, ok := answers["difficulty"]; ok {
			res.Difficulty = difficulty.Score
		}
		res.Usage = fusion.Usage{Input: usage.InputTokens, Output: usage.OutputTokens}
		gate.RecordSuccess(m.Provider, m.Model, fc.runtime.Generation)
		if p.svc.Metrics != nil {
			p.svc.Metrics.Inc(m.Provider, m.Model, counters.EvRequests)
			latencyMs := time.Since(start).Milliseconds()
			p.svc.Metrics.AddLatency(m.Provider, m.Model, uint64(latencyMs), uint64(latencyMs))
		}
		if p.svc.Tokens != nil {
			p.svc.Tokens.Commit(counters.TokenKey{Provider: m.Provider, Model: m.Model}, counters.TokenUsage{
				Input:  res.Usage.Input,
				Output: res.Usage.Output,
			})
		}
		if p.svc.Agents != nil {
			p.svc.Agents.AddTokens(fc.agent, m.Provider, m.Model, counters.TokenUsage{
				Input:  res.Usage.Input,
				Output: res.Usage.Output,
			})
		}
	}
	if logger := p.svc.ReqLog; logger != nil {
		logInput := BuildRequestLogInput(
			LogCtx{RequestID: legID, SessionID: fc.flc.SessionID, Exposed: fc.flc.Exposed, Agent: fc.agent},
			upstreamReq,
			"decisions",
			fc.calledModel,
			m,
			resp,
			start,
			sentBody,
		)
		requestlog.Complete(logger, logInput, respBody, int64(len(respBody)), false)
	}
	return res
}

// selectorCriteria renders the choice options as id → rubric text. The model
// identity is always included (the decisions model reads criteria literally
// and anchors on them); a configured rubric refines it.
func selectorCriteria(candidates []fusion.SelectorCandidate) map[string]string {
	criteria := make(map[string]string, len(candidates))
	for _, c := range candidates {
		label := c.Target.Provider + "/" + c.Target.Model
		if c.Rubric != "" {
			label += " — " + c.Rubric
		}
		criteria[c.ID] = label
	}
	return criteria
}
