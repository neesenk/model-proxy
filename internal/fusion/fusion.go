// Package fusion contains the value-only pieces of Fusion orchestration.
//
// It deliberately does not know about Proxy, HTTP, metrics, or runtime state.
// The composition layer owns executing legs; this package owns the bounded
// registry and deterministic transformations of those leg results.
package fusion

const (
	// RecentRunCap is the number of most recent runs retained by Registry.
	RecentRunCap = 200

	DegradedInsufficientProposers = "insufficient_proposers"
	DegradedToolsUnsupported      = "tools_unsupported"
	DegradedBodyBuildFailed       = "body_build_failed"
	DegradedBudgetExceeded        = "budget_exceeded"
	DegradedMultiTurn             = "multi_turn"

	DefaultSynthesisInstruction = "你是多模型编排的结果汇总模型。基于原对话和下列候选答案，给出最强的最终答案；不要提及候选/编排过程；需要动作时直接输出工具调用。"
	DefaultJudgeInstruction     = "你是多模型编排的评审模型。基于原对话和下列候选答案，分析它们的共识、冲突与遗漏，输出简短的评审报告供结果汇总模型参考；不要回答原问题；不要提及候选/编排过程。"
)

// LegResult is the non-client-visible outcome of one panel or judge call.
// Index identifies its member in the configured panel; judge legs use -1.
type LegResult struct {
	Index     int
	Provider  string
	Model     string
	Text      string
	Usage     Usage
	Status    int
	LatencyMs int64
	Err       error
}

// Usage is Fusion's token accounting value. It retains the cache-token fields
// used by the root token ledger while remaining independent of that package.
type Usage struct {
	Input         uint64
	Output        uint64
	CacheCreation uint64
	CacheRead     uint64
}

// LegObservation is the JSON-safe projection retained with a Run.
type LegObservation struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Kind      string `json:"kind"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Input     uint64 `json:"input"`
	Output    uint64 `json:"output"`
	Err       string `json:"err,omitempty"`
	Cut       bool   `json:"cut,omitempty"`
}

// Run is one client request handled by a Fusion workflow.
type Run struct {
	RunID          string           `json:"run_id"`
	Ts             int64            `json:"ts"`
	Route          string           `json:"route"`
	Workflow       string           `json:"workflow"`
	Agent          string           `json:"agent,omitempty"`
	Proto          string           `json:"proto"`
	Quorum         int              `json:"quorum"`
	DraftsUsed     int              `json:"drafts_used"`
	Degraded       string           `json:"degraded,omitempty"`
	Legs           []LegObservation `json:"legs"`
	JudgeUsed      bool             `json:"judge_used,omitempty"`
	SynthCommitted bool             `json:"synth_committed"`
	SynthStatus    int              `json:"synth_status,omitempty"`
	SynthLatencyMs int64            `json:"synth_latency_ms,omitempty"`
	SynthInput     uint64           `json:"synth_input,omitempty"`
	SynthOutput    uint64           `json:"synth_output,omitempty"`
	// Selector is the decisions-model routing pass record, present only when
	// the recipe configures a selector (shadow records too — the shadow
	// observations are the tuning data for enforce).
	Selector *SelectorObservation `json:"selector,omitempty"`
}

// WorkflowStats is the JSON projection of Registry's cumulative workflow
// aggregate plus today's admitted orchestration count.
type WorkflowStats struct {
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
