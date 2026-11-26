package forward

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

var errDecisionLegUnavailable = errors.New("decision leg unavailable")

// decisionsInput parameterizes one decisions-model routing call. The caller
// owns candidate construction, instructions, and observability context.
type decisionsInput struct {
	Runtime     Snapshot
	Target      RouteTarget
	SessionKey  string
	SessionID   string
	RequestID   string
	Kind        string
	Agent       string
	Proto       string
	Exposed     string
	CalledModel string
	Timeout     time.Duration

	State                  map[string]any
	Candidates             []selectorCandidate
	ChoiceInstructions     string
	DifficultyInstructions string
	DifficultyLevels       []string
}

// decisionsOutput is the typed result of a decisions call. Err covers
// transport/parse failures AND unusable answers (empty answers, unknown choice)
// — callers decide how to fall back.
type decisionsOutput struct {
	Model         string
	ChoiceID      string
	Confidence    float64
	Probabilities map[string]float64
	Difficulty    float64
	Usage         counters.TokenUsage
	LatencyMs     int64
	Err           error
}

// callDecisions executes a route selector through generation-bound leg
// machinery: resolver, plan, health gate, BufferedLeg, metrics, request log and
// live events.
func (p pipeline) callDecisions(ctx context.Context, in decisionsInput) (out decisionsOutput) {
	m := in.Target
	if picked, ok := routing.NewResolver(
		p.svc.ResolverState,
		in.Runtime.Providers,
		in.Runtime.PoolIndex,
		in.Runtime.Generation,
	).Pick(m, in.SessionKey); ok {
		m = picked
	}

	legID := in.Kind + "-" + in.RequestID
	marker := in.Kind + ":" + m.Model
	start := time.Now()
	status := http.StatusBadGateway
	p.svc.Events.Publish(observeevents.Event{
		Type:      "start",
		Ts:        start.UnixMilli(),
		RequestID: legID,
		SessionID: in.SessionID,
		Agent:     in.Agent,
		Protocol:  in.Proto,
		Exposed:   in.Exposed,
		Provider:  marker,
	})
	defer func() {
		out.LatencyMs = time.Since(start).Milliseconds()
		p.svc.Events.Publish(observeevents.Event{
			Type:          "end",
			Ts:            time.Now().UnixMilli(),
			RequestID:     legID,
			SessionID:     in.SessionID,
			Agent:         in.Agent,
			Protocol:      in.Proto,
			Exposed:       in.Exposed,
			Provider:      marker,
			UpstreamModel: m.Model,
			Status:        status,
			LatencyMs:     out.LatencyMs,
			Input:         out.Usage.Input,
			Output:        out.Usage.Output,
		})
	}()

	fail := func(err error) decisionsOutput {
		out.Err = err
		return out
	}

	questions := map[string]protocol.SystemOneQuestion{
		"model_choice": {
			Type:         "choice",
			Instructions: in.ChoiceInstructions,
			Criteria:     selectorCriteria(in.Candidates),
		},
		"difficulty": {
			Type:         "score",
			Instructions: in.DifficultyInstructions,
			Criteria:     append([]string(nil), in.DifficultyLevels...),
		},
	}

	body, err := protocol.BuildSystemOneBody(m.Model, in.State, questions)
	if err != nil {
		return fail(err)
	}

	plan, err := PlanTarget(PlanInput{
		Runtime: in.Runtime, Target: m, ClientProto: "decisions", ClientPath: "/v1/decisions",
	})
	if err != nil {
		return fail(err)
	}
	if plan.Provider() == nil {
		return fail(fmt.Errorf("provider %s not available", m.Provider))
	}
	gate := p.svc.NewHealthGate(in.Runtime)
	if !gate.TakeHalfOpenSlot(m.Provider, in.Runtime.Generation) {
		return fail(errDecisionLegUnavailable)
	}
	defer gate.ReleaseHalfOpenSlot(m.Provider, in.Runtime.Generation)
	sched := in.Runtime.Cfg.Scheduling

	body, err = plan.ConvertBody(body)
	if err != nil {
		return fail(fmt.Errorf("convert decisions→%s: %w", plan.BackendProtocol(), err))
	}

	legCtx, cancelLeg := context.WithTimeout(ctx, in.Timeout)
	defer cancelLeg()
	exchange := &targetexec.BufferedLegExchange{}
	legStatus, respBody, err := targetexec.BufferedLeg{
		Client:    p.clientFor(in.Runtime.Cfg, in.Runtime.ParentOf, m.Provider),
		Plan:      plan,
		SessionID: in.SessionID,
		MaxBody:   64 << 20,
		ApplyParamBlock: func(body []byte) []byte {
			return gate.ApplyParamBlock(m.Provider, m.Model, body)
		},
		LearnParamBlock: func(param string) {
			gate.LearnParamBlock(m.Provider, m.Model, param, in.Runtime.Generation)
		},
		OnStripParam: func(param string) {
			logx.Warnf("[%s provider=%s] 400 unsupported parameter %q — stripped, retrying", in.Kind, m.Provider, param)
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
			return fail(errDecisionLegUnavailable)
		}
		if errors.Is(err, context.DeadlineExceeded) &&
			legCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return fail(err)
		}
		gate.RecordFailure(m.Provider, sched, in.Runtime.Generation)
		p.incProviderMetric(m.Provider, m.Model, counters.EvFailures)
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
		gate.RecordRateLimit(m.Provider, decision.Until, string(decision.Kind), in.Runtime.Generation)
		p.incProviderMetric(m.Provider, m.Model, counters.EvRateLimited429)
		out.Err = errDecisionLegUnavailable
	case resp.StatusCode >= 500:
		gate.RecordFailure(m.Provider, sched, in.Runtime.Generation)
		p.incProviderMetric(m.Provider, m.Model, counters.EvFailures)
		out.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized:
		gate.RecordFailure(m.Provider, sched, in.Runtime.Generation)
		out.Err = fmt.Errorf("upstream status %d after auth refresh", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || targetexec.IsModelDenied(resp.StatusCode, respBody):
		gate.RecordModelFailure(m.Provider, m.Model, sched, in.Runtime.Generation)
		out.Err = fmt.Errorf("model unavailable (status %d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		p.incProviderMetric(m.Provider, m.Model, counters.EvFailures)
		out.Err = fmt.Errorf("upstream status %d", resp.StatusCode)
	default:
		model, answers, usage, parseErr := protocol.ParseSystemOneResponse(respBody)
		if parseErr != nil {
			out.Err = parseErr
			gate.RecordModelFailure(m.Provider, m.Model, sched, in.Runtime.Generation)
			break
		}
		choice, ok := answers["model_choice"]
		if !ok || choice.Choice == "" {
			out.Err = fmt.Errorf("decisions response missing model_choice answer")
			gate.RecordModelFailure(m.Provider, m.Model, sched, in.Runtime.Generation)
			break
		}
		out.Model = model
		out.ChoiceID = choice.Choice
		out.Confidence = choice.Confidence
		out.Probabilities = choice.Probabilities
		if difficulty, ok := answers["difficulty"]; ok {
			out.Difficulty = difficulty.Score
		}
		out.Usage = counters.TokenUsage{Input: usage.InputTokens, Output: usage.OutputTokens}
		gate.RecordSuccess(m.Provider, m.Model, in.Runtime.Generation)
		p.incProviderMetric(m.Provider, m.Model, counters.EvRequests)
		latencyMs := time.Since(start).Milliseconds()
		p.addProviderLatency(m.Provider, m.Model, uint64(latencyMs))
		p.commitProviderTokens(in.Agent, m.Provider, m.Model, out.Usage)
	}
	if logger := p.svc.ReqLog; logger != nil {
		logInput := BuildRequestLogInput(
			LogCtx{RequestID: legID, SessionID: in.SessionID, Exposed: in.Exposed, Agent: in.Agent},
			upstreamReq,
			"decisions",
			in.CalledModel,
			m,
			resp,
			start,
			sentBody,
		)
		requestlog.Complete(logger, logInput, respBody, int64(len(respBody)), false)
	}
	return out
}

func (p pipeline) incProviderMetric(provider, model string, ev counters.MetricsEvent) {
	if p.svc.Metrics == nil {
		return
	}
	p.svc.Metrics.Inc(provider, model, ev)
}

func (p pipeline) addProviderLatency(provider, model string, ms uint64) {
	if p.svc.Metrics != nil {
		p.svc.Metrics.AddLatency(provider, model, ms, ms)
	}
}

func (p pipeline) commitProviderTokens(agent, provider, model string, usage counters.TokenUsage) {
	if p.svc.Tokens != nil {
		p.svc.Tokens.Commit(counters.TokenKey{Provider: provider, Model: model}, counters.TokenUsage{
			Input:  usage.Input,
			Output: usage.Output,
		})
	}
	if p.svc.Agents != nil {
		p.svc.Agents.AddTokens(agent, provider, model, counters.TokenUsage{
			Input:  usage.Input,
			Output: usage.Output,
		})
	}
}
