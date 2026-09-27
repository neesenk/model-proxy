package forward

import (
	"context"
	"errors"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
)

// fusion_select.go adapts internal/fusion's selector port to the decisions
// protocol through the shared pipeline.callDecisions helper. The helper owns
// the transport, health gate, metrics, live events and request log; this file
// only maps fusion's SelectRequest/SelectResult onto the generic decisions call.

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

// callFusionSelector executes the decisions leg for a fusion recipe. It keeps
// the historical contract: start/end live events keyed
// "fusion-select-<requestID>", provider marker "fusion-select:<model>", and
// the same timeout / fallback semantics.
func (p pipeline) callFusionSelector(ctx context.Context, fc fusionCtx, sel configdomain.SelectorConfig, req fusion.SelectRequest) fusion.SelectResult {
	out := p.callDecisions(ctx, decisionsInput{
		Runtime:                fc.runtime,
		Target:                 sel.Target,
		SessionKey:             fc.sessionKey,
		SessionID:              fc.flc.SessionID,
		RequestID:              fc.flc.RequestID,
		Kind:                   "fusion-select",
		Agent:                  fc.agent,
		Proto:                  fc.proto,
		Exposed:                fc.flc.Exposed,
		CalledModel:            fc.calledModel,
		Timeout:                sel.TimeoutDuration(),
		State:                  req.State,
		Candidates:             req.Candidates,
		ChoiceInstructions:     req.ChoiceInstructions,
		DifficultyInstructions: req.DifficultyInstructions,
		DifficultyLevels:       req.DifficultyLevels,
	})
	return fusion.SelectResult{
		ChoiceID:      out.ChoiceID,
		Confidence:    out.Confidence,
		Probabilities: out.Probabilities,
		Difficulty:    out.Difficulty,
		LatencyMs:     out.LatencyMs,
		Model:         out.Model,
		Usage:         out.Usage,
		Err:           out.Err,
	}
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
