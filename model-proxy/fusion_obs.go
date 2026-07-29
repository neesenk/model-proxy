package main

import (
	"encoding/json"
	"math"
	"sync"
	"time"
)

// fusion_obs.go is the observability layer for the fusion orchestration
// engine (fusion.go): every runFusion execution — fully orchestrated or
// degraded to a direct synthesizer call — is recorded as a fusionRun in a
// process-lifetime in-memory registry. The registry keeps a bounded ring of
// recent runs plus per-workflow cumulative aggregates (runs / quorum met /
// degrades by reason / token totals), and owns the per-day orchestration
// budget counters backing FusionConfig.MaxRunsPerDay. It lives on Proxy and
// survives reloads (like the event hub), so config edits don't reset it.
//
// Lock discipline: fusionRegistry.mu is an independent leaf owner — methods
// never call into other subsystems while holding it (record only copies
// caller-built values).

// fusionRecentCap bounds the recent-run ring (same size as the live-event ring).
const fusionRecentCap = 200

// Degrade reasons recorded on fusionRun.Degraded (empty = fully orchestrated).
const (
	fusionDegradedInsufficient = "insufficient_proposers" // quorum unreachable
	fusionDegradedTools        = "tools_unsupported"      // request has tools, synthesizer doesn't
	fusionDegradedBodyBuild    = "body_build_failed"      // original body not usable JSON
	fusionDegradedBudget       = "budget_exceeded"        // max_runs_per_day exhausted
	fusionDegradedMultiTurn    = "multi_turn"             // first_turn_only + assistant turn present
)

// fusionLegObs is the per-leg observation of one fusion sub-call (a panel
// member's candidate branch or the judge analysis): what came back, and
// whether the leg was cut by the quorum/grace collection before answering.
type fusionLegObs struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Kind      string `json:"kind"` // "panel" | "judge"
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Input     uint64 `json:"input"`
	Output    uint64 `json:"output"`
	Err       string `json:"err,omitempty"`
	Cut       bool   `json:"cut,omitempty"` // cancelled by the quorum/grace cutoff
}

// fusionRun is one orchestration run: one client request handled by a fusion
// recipe, from the gates/fan-out to the synthesis leg. The synthesis leg's
// status/latency/tokens are read back from the live-event hub (published by
// tryTarget on commit) — best-effort: all zero when the synthesis leg never
// committed, tokens zero for non-SSE responses (no usage scan).
type fusionRun struct {
	RunID          string         `json:"run_id"` // = parent request id
	Ts             int64          `json:"ts"`     // unix milliseconds
	Route          string         `json:"route"`
	Workflow       string         `json:"workflow"`
	Agent          string         `json:"agent,omitempty"`
	Proto          string         `json:"proto"`
	Quorum         int            `json:"quorum"`
	DraftsUsed     int            `json:"drafts_used"`
	Degraded       string         `json:"degraded,omitempty"` // one of the fusionDegraded* constants
	Legs           []fusionLegObs `json:"legs"`
	JudgeUsed      bool           `json:"judge_used,omitempty"` // a judge report made it into the synthesis body
	SynthCommitted bool           `json:"synth_committed"`
	SynthStatus    int            `json:"synth_status,omitempty"`
	SynthLatencyMs int64          `json:"synth_latency_ms,omitempty"`
	SynthInput     uint64         `json:"synth_input,omitempty"`
	SynthOutput    uint64         `json:"synth_output,omitempty"`
}

// fusionWorkflowAgg is the cumulative per-workflow aggregate (internal; the
// JSON projection is fusionWorkflowStats).
type fusionWorkflowAgg struct {
	runs        uint64
	quorumMet   uint64
	degraded    map[string]uint64
	panelInput  uint64
	panelOutput uint64
	judgeInput  uint64
	judgeOutput uint64
	synthInput  uint64
	synthOutput uint64
}

// fusionWorkflowStats is the JSON projection of fusionWorkflowAgg plus today's
// admitted-run count (the budget counter). Amplification is the orchestration
// token overhead vs a plain direct call: (panel+judge+synth) / synth; 0 while
// no synthesis usage has been observed.
type fusionWorkflowStats struct {
	Runs          uint64            `json:"runs"`
	RunsToday     uint64            `json:"runs_today"`
	QuorumMet     uint64            `json:"quorum_met"`
	Degraded      map[string]uint64 `json:"degraded,omitempty"`
	PanelInput    uint64            `json:"panel_input"`
	PanelOutput   uint64            `json:"panel_output"`
	JudgeInput    uint64            `json:"judge_input"`
	JudgeOutput   uint64            `json:"judge_output"`
	SynthInput    uint64            `json:"synth_input"`
	SynthOutput   uint64            `json:"synth_output"`
	Amplification float64           `json:"amplification"`
}

// fusionRegistry keeps the recent-run ring + per-workflow aggregates + daily
// budget counters. All methods are nil-receiver-safe so a hand-built Proxy
// (degenerate tests) stays no-op.
type fusionRegistry struct {
	mu      sync.Mutex
	runs    []fusionRun // newest last; capped at fusionRecentCap
	agg     map[string]*fusionWorkflowAgg
	day     string            // local day ("2006-01-02") the dayRuns counters belong to
	dayRuns map[string]uint64 // workflow → orchestrated runs admitted today
}

func newFusionRegistry() *fusionRegistry {
	return &fusionRegistry{agg: map[string]*fusionWorkflowAgg{}, dayRuns: map[string]uint64{}}
}

// admit reports whether one more ORCHESTRATED run for workflow fits the daily
// budget (limit <= 0 = unlimited), counting the admission when admitted. Runs
// degraded before fan-out (multi_turn / tools / budget itself) never consume
// budget — they make a single plain call, not an orchestration. The day
// boundary is local time.
func (r *fusionRegistry) admit(workflow string, limit int, now time.Time) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollDayLocked(now)
	if limit > 0 && r.dayRuns[workflow] >= uint64(limit) {
		return false
	}
	r.dayRuns[workflow]++
	return true
}

// record appends run to the recent ring and folds it into the per-workflow
// aggregate.
func (r *fusionRegistry) record(run *fusionRun) {
	if r == nil || run == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, *run)
	if len(r.runs) > fusionRecentCap {
		r.runs = r.runs[len(r.runs)-fusionRecentCap:]
	}
	a := r.agg[run.Workflow]
	if a == nil {
		a = &fusionWorkflowAgg{degraded: map[string]uint64{}}
		r.agg[run.Workflow] = a
	}
	a.runs++
	if run.Degraded != "" {
		a.degraded[run.Degraded]++
	}
	if run.Quorum > 0 && run.DraftsUsed >= run.Quorum {
		a.quorumMet++
	}
	for _, l := range run.Legs {
		if l.Kind == "judge" {
			a.judgeInput += l.Input
			a.judgeOutput += l.Output
		} else {
			a.panelInput += l.Input
			a.panelOutput += l.Output
		}
	}
	a.synthInput += run.SynthInput
	a.synthOutput += run.SynthOutput
}

// snapshot returns the per-workflow aggregates (optionally filtered to one
// workflow) and the matching recent runs, newest first.
func (r *fusionRegistry) snapshot(workflow string, now time.Time) (map[string]fusionWorkflowStats, []fusionRun) {
	stats := map[string]fusionWorkflowStats{}
	runs := []fusionRun{}
	if r == nil {
		return stats, runs
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollDayLocked(now)
	for name, a := range r.agg {
		if workflow != "" && name != workflow {
			continue
		}
		st := fusionWorkflowStats{
			Runs:        a.runs,
			RunsToday:   r.dayRuns[name],
			QuorumMet:   a.quorumMet,
			PanelInput:  a.panelInput,
			PanelOutput: a.panelOutput,
			JudgeInput:  a.judgeInput,
			JudgeOutput: a.judgeOutput,
			SynthInput:  a.synthInput,
			SynthOutput: a.synthOutput,
		}
		if len(a.degraded) > 0 {
			st.Degraded = make(map[string]uint64, len(a.degraded))
			for reason, n := range a.degraded {
				st.Degraded[reason] = n
			}
		}
		if synth := a.synthInput + a.synthOutput; synth > 0 {
			total := a.panelInput + a.panelOutput + a.judgeInput + a.judgeOutput + synth
			st.Amplification = math.Round(float64(total)/float64(synth)*100) / 100
		}
		stats[name] = st
	}
	for i := len(r.runs) - 1; i >= 0; i-- {
		if workflow != "" && r.runs[i].Workflow != workflow {
			continue
		}
		runs = append(runs, r.runs[i])
	}
	return stats, runs
}

// rollDayLocked resets the daily budget counters when the local day changes.
// Caller holds r.mu.
func (r *fusionRegistry) rollDayLocked(now time.Time) {
	day := now.Format("2006-01-02")
	if r.day != day {
		r.day = day
		r.dayRuns = map[string]uint64{}
	}
}

// obsFromLegResult projects a panel/judge leg result into its observation.
func obsFromLegResult(res fusionLegResult, kind string) fusionLegObs {
	obs := fusionLegObs{
		Provider:  res.provider,
		Model:     res.model,
		Kind:      kind,
		Status:    res.status,
		LatencyMs: res.latencyMs,
		Input:     res.usage.Input,
		Output:    res.usage.Output,
	}
	if res.err != nil {
		obs.Err = res.err.Error()
	}
	return obs
}

// reconcileFusionLegs folds the leg results that arrived before collection
// stopped into the per-member observation list. A member with no result was
// cut by the quorum/grace cutoff (its context is cancelled; a late result
// lands in the buffered channel and is discarded).
func reconcileFusionLegs(panel []RouteTarget, received []fusionLegResult) []fusionLegObs {
	byIdx := make(map[int]fusionLegResult, len(received))
	for _, res := range received {
		byIdx[res.idx] = res
	}
	legs := make([]fusionLegObs, 0, len(panel))
	for i, m := range panel {
		if res, ok := byIdx[i]; ok {
			legs = append(legs, obsFromLegResult(res, "panel"))
		} else {
			legs = append(legs, fusionLegObs{
				Provider: m.Provider, Model: m.Model, Kind: "panel",
				Cut: true, Err: "cut by quorum/grace",
			})
		}
	}
	return legs
}

// bodyHasAssistantTurn reports whether the request body already carries an
// assistant message (i.e. a multi-turn conversation) — messages[] for
// anthropic/openai chat, input[] for the responses API. Best-effort: an
// unparseable body reports false (orchestration proceeds).
func bodyHasAssistantTurn(body []byte) bool {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	for _, key := range []string{"messages", "input"} {
		msgs, ok := v[key].([]any)
		if !ok {
			continue
		}
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" {
				return true
			}
		}
	}
	return false
}
