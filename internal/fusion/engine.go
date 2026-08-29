package fusion

import (
	"context"
	"model-proxy/internal/observe/logx"
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

// Ports are the three application capabilities needed by orchestration.
// Implementations must stay bound to the runtime generation captured for the
// parent request.
type Ports interface {
	SupportsTools(configdomain.RouteTarget) bool
	CallLeg(context.Context, LegCall) LegResult
	Synthesize(configdomain.RouteTarget, []byte) SynthesisResult
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

	quorum := request.Recipe.MinPanel
	if quorum <= 0 {
		quorum = 2
	}
	if quorum > len(request.Recipe.Panel) {
		quorum = len(request.Recipe.Panel)
	}
	run.Quorum = quorum

	panelContext, cancelPanel := context.WithCancel(ctx)
	defer cancelPanel()
	results := make(chan LegResult, len(request.Recipe.Panel))
	for index, member := range request.Recipe.Panel {
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
	successes, received := CollectResults(results, len(request.Recipe.Panel), quorum, grace, cancelPanel)
	run.DraftsUsed = len(successes)
	run.Legs = ReconcileLegs(request.Recipe.Panel, received)
	if len(successes) < quorum {
		run.Degraded = DegradedInsufficientProposers
		logx.Warnf("[fusion] %s: fusion_insufficient_proposers (%d/%d drafts, quorum %d); answering directly via synthesizer",
			request.Route, len(successes), len(request.Recipe.Panel), quorum)
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
