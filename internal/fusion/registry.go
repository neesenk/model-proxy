package fusion

import (
	"math"
	"sync"
	"time"
)

type workflowAggregate struct {
	runs, quorumMet                                  uint64
	panelInput, panelOutput, judgeInput, judgeOutput uint64
	synthInput, synthOutput                          uint64
	degraded                                         map[string]uint64
}

// Registry retains process-lifetime Fusion observations and local-day budget
// counters. All methods are nil-receiver-safe for hand-built composition roots.
type Registry struct {
	mu      sync.Mutex
	runs    []Run
	agg     map[string]*workflowAggregate
	day     string
	dayRuns map[string]uint64
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{agg: map[string]*workflowAggregate{}, dayRuns: map[string]uint64{}}
}

// Admit atomically checks and consumes one workflow's local-day orchestration
// budget. A non-positive limit is unlimited. Calls on a nil registry admit.
func (r *Registry) Admit(workflow string, limit int, now time.Time) bool {
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

// Record retains a detached Run and folds it into the cumulative aggregate.
func (r *Registry) Record(run *Run) {
	if r == nil || run == nil {
		return
	}
	copyRun := cloneRun(*run)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, copyRun)
	if len(r.runs) > RecentRunCap {
		r.runs = r.runs[len(r.runs)-RecentRunCap:]
	}
	aggregate := r.agg[copyRun.Workflow]
	if aggregate == nil {
		aggregate = &workflowAggregate{degraded: map[string]uint64{}}
		r.agg[copyRun.Workflow] = aggregate
	}
	aggregate.runs++
	if copyRun.Degraded != "" {
		aggregate.degraded[copyRun.Degraded]++
	}
	if copyRun.Quorum > 0 && copyRun.DraftsUsed >= copyRun.Quorum {
		aggregate.quorumMet++
	}
	for _, leg := range copyRun.Legs {
		if leg.Kind == "judge" {
			aggregate.judgeInput += leg.Input
			aggregate.judgeOutput += leg.Output
		} else {
			aggregate.panelInput += leg.Input
			aggregate.panelOutput += leg.Output
		}
	}
	aggregate.synthInput += copyRun.SynthInput
	aggregate.synthOutput += copyRun.SynthOutput
}

// Snapshot returns detached workflow aggregates and recent runs, newest first.
// An empty workflow selects all workflows.
func (r *Registry) Snapshot(workflow string, now time.Time) (map[string]WorkflowStats, []Run) {
	stats := map[string]WorkflowStats{}
	runs := []Run{}
	if r == nil {
		return stats, runs
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollDayLocked(now)
	for name, aggregate := range r.agg {
		if workflow != "" && name != workflow {
			continue
		}
		stat := WorkflowStats{
			Runs:        aggregate.runs,
			RunsToday:   r.dayRuns[name],
			QuorumMet:   aggregate.quorumMet,
			PanelInput:  aggregate.panelInput,
			PanelOutput: aggregate.panelOutput,
			JudgeInput:  aggregate.judgeInput,
			JudgeOutput: aggregate.judgeOutput,
			SynthInput:  aggregate.synthInput,
			SynthOutput: aggregate.synthOutput,
		}
		if len(aggregate.degraded) != 0 {
			stat.Degraded = cloneCounts(aggregate.degraded)
		}
		if synth := aggregate.synthInput + aggregate.synthOutput; synth > 0 {
			total := aggregate.panelInput + aggregate.panelOutput +
				aggregate.judgeInput + aggregate.judgeOutput + synth
			stat.Amplification = math.Round(float64(total)/float64(synth)*100) / 100
		}
		stats[name] = stat
	}
	for i := len(r.runs) - 1; i >= 0; i-- {
		if workflow == "" || r.runs[i].Workflow == workflow {
			runs = append(runs, cloneRun(r.runs[i]))
		}
	}
	return stats, runs
}

func (r *Registry) rollDayLocked(now time.Time) {
	day := now.Format("2006-01-02")
	if r.day != day {
		r.day = day
		r.dayRuns = map[string]uint64{}
	}
}

func cloneCounts(in map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneRun(run Run) Run {
	if run.Legs != nil {
		run.Legs = append([]LegObservation(nil), run.Legs...)
	}
	if run.Selector != nil {
		observation := *run.Selector
		run.Selector = &observation
	}
	return run
}
