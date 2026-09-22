package fusion

import (
	"context"
	"model-proxy/internal/observe/logx"
	"sort"
	"time"

	configdomain "model-proxy/internal/config"
)

// DefaultGracePeriod is the amount of time collection waits for nearly
// completed panel legs after quorum is first reached.
const DefaultGracePeriod = 5 * time.Second

// LegCall is one non-client-visible panel or judge execution requested by the
// orchestration engine. The application adapter resolves and executes Target
// against the request's captured runtime generation.
type LegCall struct {
	Index  int
	Kind   string
	Target configdomain.RouteTarget
	Body   []byte
}

// SynthesisResult is the client-facing synthesis outcome returned by the
// application adapter after it delegates delivery to the normal target
// executor.
type SynthesisResult struct {
	Committed bool
	Status    int
	LatencyMs int64
	Input     uint64
	Output    uint64
}

// Ports are the four application capabilities needed by orchestration.
// Implementations must stay bound to the runtime generation captured for the
// parent request.
type Ports interface {
	SupportsTools(configdomain.RouteTarget) bool
	CallLeg(context.Context, LegCall) LegResult
	Synthesize(configdomain.RouteTarget, []byte) SynthesisResult
	// SelectPanel asks a decisions model (e.g. TypeSafe Jev) to judge which
	// candidate best fits the request and how hard the request is. Called at
	// most once per run, synchronously before fan-out, and only when the
	// recipe configures a selector.
	SelectPanel(context.Context, SelectRequest) SelectResult
}

// Request contains the immutable workflow and request values used by Engine.
// HTTP delivery, cache state, and runtime owners remain behind Ports.
type Request struct {
	Workflow     string
	RunID        string
	Route        string
	Agent        string
	Protocol     string
	OriginalBody []byte
	HasTools     bool
	Facts        RequestFacts
	Recipe       configdomain.FusionConfig
}

// Result reports whether synthesis committed and returns the exact observation
// recorded in Registry.
type Result struct {
	Committed bool
	Run       Run
}

// Engine owns Fusion gates, fan-out/quorum collection, optional judging,
// synthesis-body construction, and observation recording. It deliberately has
// no transport, runtime-state, request-log, or client-response dependency.
type Engine struct {
	Registry    *Registry
	GracePeriod time.Duration
}

// Run executes one Fusion workflow through generation-bound application ports.
func (engine Engine) Run(ctx context.Context, request Request, ports Ports) Result {
	run := Run{
		RunID:    request.RunID,
		Ts:       time.Now().UnixMilli(),
		Route:    request.Route,
		Workflow: request.Workflow,
		Agent:    request.Agent,
		Proto:    request.Protocol,
	}
	if request.Recipe.FirstTurnOnly && HasAssistantTurn(request.OriginalBody) {
		run.Degraded = DegradedMultiTurn
		logx.Infof("[fusion] %s: workflow %s is first_turn_only and the conversation is multi-turn; answering directly (fusion_multi_turn)",
			request.Route, request.Workflow)
		return engine.finish(&run, request.Recipe.Synthesizer, request.OriginalBody, ports)
	}

	// Selector gate (before tools/budget): one decisions-model call may answer
	// directly (cheap requests skip orchestration — and its budget) or trim the
	// panel. Every failure falls back to the static panel; selector_direct is
	// deliberately NOT charged against max_runs_per_day (no orchestration ran).
	panel := request.Recipe.Panel
	if sel := request.Recipe.Selector; sel != nil {
		trimmed, direct := engine.applySelector(ctx, &run, request, ports, sel)
		if direct != nil {
			run.Degraded = DegradedSelectorDirect
			logx.Infof("[fusion] %s: selector directs to %s/%s (confidence %.2f, difficulty %.2f); answering directly (fusion_selector_direct)",
				request.Route, direct.Provider, direct.Model, run.Selector.Confidence, run.Selector.Difficulty)
			return engine.finish(&run, *direct, request.OriginalBody, ports)
		}
		panel = trimmed
	}

	if request.HasTools && !ports.SupportsTools(request.Recipe.Synthesizer) {
		run.Degraded = DegradedToolsUnsupported
		logx.Warnf("[fusion] %s: synthesizer %s/%s lacks tool support; answering directly (fusion_tools_unsupported)",
			request.Route, request.Recipe.Synthesizer.Provider, request.Recipe.Synthesizer.Model)
		return engine.finish(&run, request.Recipe.Synthesizer, request.OriginalBody, ports)
	}
	if !engine.Registry.Admit(request.Workflow, request.Recipe.MaxRunsPerDay, time.Now()) {
		run.Degraded = DegradedBudgetExceeded
		logx.Warnf("[fusion] %s: workflow %s daily orchestration budget exhausted (%d/day); answering directly (fusion_budget_exceeded)",
			request.Route, request.Workflow, request.Recipe.MaxRunsPerDay)
		return engine.finish(&run, request.Recipe.Synthesizer, request.OriginalBody, ports)
	}

	quorum := quorumFor(request.Recipe)
	if quorum > len(panel) {
		quorum = len(panel)
	}
	run.Quorum = quorum

	panelContext, cancelPanel := context.WithCancel(ctx)
	defer cancelPanel()
	results := make(chan LegResult, len(panel))
	for index, member := range panel {
		index, member := index, member
		go func() {
			results <- ports.CallLeg(panelContext, LegCall{
				Index:  index,
				Kind:   "panel",
				Target: member,
				Body:   request.OriginalBody,
			})
		}()
	}
	grace := engine.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	successes, received := CollectResults(results, len(panel), quorum, grace, cancelPanel)
	run.DraftsUsed = len(successes)
	run.Legs = ReconcileLegs(panel, received)
	if len(successes) < quorum {
		run.Degraded = DegradedInsufficientProposers
		logx.Warnf("[fusion] %s: fusion_insufficient_proposers (%d/%d drafts, quorum %d); answering directly via synthesizer",
			request.Route, len(successes), len(panel), quorum)
		return engine.finish(&run, request.Recipe.Synthesizer, request.OriginalBody, ports)
	}

	candidates := make([]string, 0, len(successes))
	for _, result := range successes {
		candidates = append(candidates, result.Text)
	}
	judgeReport := ""
	if request.Recipe.Judge != nil {
		judgeBody, ok := BuildJudgeBody(request.OriginalBody, request.Protocol, candidates)
		if !ok {
			run.Legs = append(run.Legs, LegObservation{
				Provider: request.Recipe.Judge.Provider,
				Model:    request.Recipe.Judge.Model,
				Kind:     "judge",
				Err:      "cannot build judge body",
			})
			logx.Warnf("[fusion] %s: cannot build judge body; synthesizing without judge report", request.Route)
		} else {
			judge := ports.CallLeg(ctx, LegCall{
				Index:  -1,
				Kind:   "judge",
				Target: *request.Recipe.Judge,
				Body:   judgeBody,
			})
			run.Legs = append(run.Legs, ObservationFromResult(judge, "judge"))
			if judge.Err != nil {
				logx.Warnf("[fusion] %s: judge %s/%s failed: %v; synthesizing without judge report",
					request.Route, request.Recipe.Judge.Provider, request.Recipe.Judge.Model, judge.Err)
			} else {
				judgeReport = judge.Text
				run.JudgeUsed = judge.Text != ""
			}
		}
	}

	synthesisBody, ok := BuildSynthesisBody(
		request.OriginalBody,
		request.Protocol,
		candidates,
		judgeReport,
		request.Recipe.Instruction,
	)
	if !ok {
		run.Degraded = DegradedBodyBuildFailed
		logx.Warnf("[fusion] %s: cannot build synthesis body; answering directly via synthesizer", request.Route)
		return engine.finish(&run, request.Recipe.Synthesizer, request.OriginalBody, ports)
	}
	logx.Infof("[fusion] %s: synthesizing from %d/%d drafts (quorum %d)",
		request.Route, len(successes), len(request.Recipe.Panel), quorum)
	return engine.finish(&run, request.Recipe.Synthesizer, synthesisBody, ports)
}

// applySelector runs the optional decisions-model routing pass. It returns
// the panel to fan out (possibly trimmed) and, when the decision is a
// confident "answer directly", the chosen target instead. Every unusable
// outcome (skip, error, low confidence, unknown choice) returns the static
// panel — the selector can only ever make routing cheaper, never narrower
// than the configured recipe.
func (engine Engine) applySelector(
	ctx context.Context,
	run *Run,
	request Request,
	ports Ports,
	sel *configdomain.SelectorConfig,
) (panel []configdomain.RouteTarget, direct *configdomain.RouteTarget) {
	observation := &SelectorObservation{Mode: sel.SelectorMode(), Action: selectorActionNone}
	run.Selector = observation
	panel = request.Recipe.Panel

	// Decisions models are text-only: an image request's state would describe
	// the picture only as a flag, which invites confident nonsense. Skip.
	if request.Facts.HasImage {
		observation.Err = "skipped_image_request"
		logx.Infof("[fusion] %s: selector skipped for image request (decisions models are text-only)", request.Route)
		return panel, nil
	}

	candidates := selectorCandidates(request.Recipe)
	choiceInstructions := sel.Instruction
	if choiceInstructions == "" {
		choiceInstructions = DefaultSelectorChoiceInstructions
	}
	difficultyInstructions := sel.DifficultyInstruction
	if difficultyInstructions == "" {
		difficultyInstructions = DefaultSelectorDifficultyInstructions
	}
	result := ports.SelectPanel(ctx, SelectRequest{
		State:                  BuildSelectorState(request.OriginalBody, request.Protocol, request.Facts, request.HasTools, candidates),
		Candidates:             candidates,
		ChoiceInstructions:     choiceInstructions,
		DifficultyInstructions: difficultyInstructions,
		DifficultyLevels:       DefaultSelectorDifficultyLevels,
	})
	observation.LatencyMs = result.LatencyMs
	if result.Err != nil {
		observation.Err = result.Err.Error()
		observation.Action = selectorActionFallback
		logx.Warnf("[fusion] %s: selector decision failed: %v; using static panel", request.Route, result.Err)
		return panel, nil
	}
	chosen := findSelectorCandidate(candidates, result.ChoiceID)
	if chosen == nil {
		observation.Err = "unknown_choice:" + result.ChoiceID
		observation.Action = selectorActionFallback
		logx.Warnf("[fusion] %s: selector picked unknown option %q; using static panel", request.Route, result.ChoiceID)
		return panel, nil
	}
	observation.Choice = chosen.Target.Provider + "/" + chosen.Target.Model
	observation.Confidence = result.Confidence
	observation.Difficulty = result.Difficulty

	if sel.SelectorMode() != "enforce" {
		logx.Infof("[fusion] %s: selector (shadow) picked %s (confidence %.2f, difficulty %.2f); routing unchanged",
			request.Route, observation.Choice, result.Confidence, result.Difficulty)
		return panel, nil
	}
	if result.Confidence < sel.ConfidenceThreshold() {
		observation.Action = selectorActionFallback
		logx.Infof("[fusion] %s: selector confidence %.2f below threshold %.2f; using static panel",
			request.Route, result.Confidence, sel.ConfidenceThreshold())
		return panel, nil
	}

	// Direct: easy enough (per the difficulty score) to skip orchestration.
	// The tools gate is re-checked against the CHOSEN target — a tool-less
	// choice degrades the direct attempt back to panel evaluation.
	if sel.DirectScoreMax > 0 && result.Difficulty <= sel.DirectScoreMax {
		if request.HasTools && !ports.SupportsTools(chosen.Target) {
			logx.Warnf("[fusion] %s: selector direct candidate %s/%s lacks tool support; evaluating panel instead",
				request.Route, chosen.Target.Provider, chosen.Target.Model)
		} else {
			observation.Action = selectorActionDirect
			target := chosen.Target
			return nil, &target
		}
	}

	// Trim: keep the top-K panel members by decision probability (never below
	// quorum, and always including the chosen member when it sits in-panel).
	if sel.PanelTopK > 0 {
		if trimmed := trimSelectorPanel(request.Recipe.Panel, candidates, result.Probabilities, sel.PanelTopK, quorumFor(request.Recipe)); len(trimmed) < len(request.Recipe.Panel) {
			observation.Action = selectorActionTrim
			return trimmed, nil
		}
	}
	return panel, nil
}

// quorumFor resolves the recipe quorum (default 2, clamped to the panel).
func quorumFor(recipe configdomain.FusionConfig) int {
	quorum := recipe.MinPanel
	if quorum <= 0 {
		quorum = 2
	}
	if quorum > len(recipe.Panel) {
		quorum = len(recipe.Panel)
	}
	return quorum
}

// findSelectorCandidate resolves the decisions model's choice id back to its
// candidate; nil means the model answered outside the offered option set.
func findSelectorCandidate(candidates []SelectorCandidate, id string) *SelectorCandidate {
	for i := range candidates {
		if candidates[i].ID == id {
			return &candidates[i]
		}
	}
	return nil
}

// trimSelectorPanel keeps the top-K panel members by decision probability
// (stable for ties and unknown scores). K never goes below the quorum: a trim
// that silently weakened quorum would trade reliability for cost.
func trimSelectorPanel(
	panel []configdomain.RouteTarget,
	candidates []SelectorCandidate,
	probabilities map[string]float64,
	topK int,
	quorum int,
) []configdomain.RouteTarget {
	k := topK
	if k < quorum {
		k = quorum
	}
	if k >= len(panel) {
		return panel
	}
	idOf := make(map[string]string, len(candidates))
	for _, c := range candidates {
		idOf[c.Target.Provider+"/"+c.Target.Model] = c.ID
	}
	indices := make([]int, len(panel))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		return probabilities[idOf[panel[indices[a]].Provider+"/"+panel[indices[a]].Model]] >
			probabilities[idOf[panel[indices[b]].Provider+"/"+panel[indices[b]].Model]]
	})
	keep := indices[:k]
	sort.Ints(keep) // restore configured order within the kept set
	trimmed := make([]configdomain.RouteTarget, 0, k)
	for _, i := range keep {
		trimmed = append(trimmed, panel[i])
	}
	return trimmed
}

func (engine Engine) finish(
	run *Run,
	target configdomain.RouteTarget,
	body []byte,
	ports Ports,
) Result {
	synthesis := ports.Synthesize(target, body)
	run.SynthCommitted = synthesis.Committed
	run.SynthStatus = synthesis.Status
	run.SynthLatencyMs = synthesis.LatencyMs
	run.SynthInput = synthesis.Input
	run.SynthOutput = synthesis.Output
	engine.Registry.Record(run)
	return Result{Committed: synthesis.Committed, Run: *run}
}
