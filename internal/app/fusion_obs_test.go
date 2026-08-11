package app

import (
	"encoding/json"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/fusion"
)

// fusion_obs_test.go covers the application integration around
// internal/fusion: /api/fusion, cost gates, judge behavior, instruction
// override, and doctor output. Pure registry/engine tests live with the package.

// --- /api/fusion ---

// apiFusionGet queries /api/fusion on a rig proxy and decodes the response.
func apiFusionGet(t *testing.T, proxy *Proxy, query string) (struct {
	Workflows map[string]fusion.WorkflowStats `json:"workflows"`
	Runs      []fusion.Run                    `json:"runs"`
}, int) {
	t.Helper()
	w := NewWebServer(proxy, "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/fusion"+query, nil))
	var out struct {
		Workflows map[string]fusion.WorkflowStats `json:"workflows"`
		Runs      []fusion.Run                    `json:"runs"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode /api/fusion: %v (%s)", err, rec.Body.String())
		}
	}
	return out, rec.Code
}

// awaitFusionState polls cond until the proxy's post-commit bookkeeping for
// the just-served request(s) is visible. finishFusion records the run +
// metrics AFTER the response stream completes, and the forwarded upstream
// Content-Length lets the client's ReadAll finish before that bookkeeping
// runs — under scheduler load an immediate read can legitimately miss the
// just-served run (observed flake: "runs = 0, want 1"). Condition-driven, not
// a fixed sleep: locally the first poll already passes.
func awaitFusionState(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAPIFusion: a successful run is recorded with per-leg detail + the
// synthesis leg's outcome read back from the live-event hub; a quorum failure
// is recorded as a degraded run. The workflow filter isolates recipes.
func TestAPIFusion(t *testing.T) {
	t.Run("success run recorded", func(t *testing.T) {
		pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
		recipe := FusionConfig{
			Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
		postAnthropic(t, px, fusionClientBody)

		awaitFusionState(t, "success run recorded", func() bool {
			o, _ := apiFusionGet(t, proxy, "")
			return len(o.Runs) == 1
		})
		out, code := apiFusionGet(t, proxy, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		st := out.Workflows["recipe"]
		if st.Runs != 1 || st.QuorumMet != 1 || len(st.Degraded) != 0 {
			t.Errorf("stats = %+v, want 1 run, quorum met, no degrades", st)
		}
		if st.RunsToday != 1 {
			t.Errorf("runs_today = %d, want 1", st.RunsToday)
		}
		if st.PanelInput != 22 || st.PanelOutput != 14 { // 2 × anthropic draft {11,7}
			t.Errorf("panel tokens = %d/%d, want 22/14", st.PanelInput, st.PanelOutput)
		}
		if st.SynthInput != 50 || st.SynthOutput != 9 { // anthropicSSEResponder usage
			t.Errorf("synth tokens = %d/%d, want 50/9", st.SynthInput, st.SynthOutput)
		}
		if len(out.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(out.Runs))
		}
		run := out.Runs[0]
		if run.Workflow != "recipe" || run.Route != "hard" || run.Degraded != "" || run.DraftsUsed != 2 || run.Quorum != 2 {
			t.Errorf("run = %+v", run)
		}
		if !run.SynthCommitted || run.SynthStatus != 200 || run.SynthLatencyMs < 0 {
			t.Errorf("synth read-back = committed %v status %d, want committed 200", run.SynthCommitted, run.SynthStatus)
		}
		if len(run.Legs) != 2 {
			t.Fatalf("legs = %d, want 2", len(run.Legs))
		}
		for _, l := range run.Legs {
			if l.Kind != "panel" || l.Status != 200 || l.Err != "" || l.Input != 11 || l.Output != 7 {
				t.Errorf("leg = %+v, want clean panel 200 with draft usage", l)
			}
		}
		// Metrics: ("fusion", recipe) run counted, no degrade.
		m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "fusion", Model: "recipe"}]
		if m.Requests != 1 || m.Failovers != 0 {
			t.Errorf("fusion metrics = requests %d failovers %d, want 1/0", m.Requests, m.Failovers)
		}
	})

	t.Run("quorum failure recorded as degraded", func(t *testing.T) {
		// pa is DELAYED so the two failures always land first: the quorum then
		// becomes unreachable and the collector cuts pa (deterministic).
		pa := newFakeUpstream(t, delayedResponder(300*time.Millisecond, anthropicDraftResponder("draft-A")))
		pb := newFakeUpstream(t, statusResponder(500))
		pc := newFakeUpstream(t, statusResponder(500))
		ps := newFakeUpstream(t, anthropicSSEResponder("direct answer"))
		recipe := FusionConfig{
			Panel: []RouteTarget{
				{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}, {Provider: "pc", Model: "mc"},
			},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
		postAnthropic(t, px, fusionClientBody)

		awaitFusionState(t, "degraded run recorded", func() bool {
			o, _ := apiFusionGet(t, proxy, "?workflow=recipe")
			return len(o.Runs) == 1
		})
		out, _ := apiFusionGet(t, proxy, "?workflow=recipe")
		st := out.Workflows["recipe"]
		if st.Runs != 1 || st.QuorumMet != 0 || st.Degraded[fusion.DegradedInsufficientProposers] != 1 {
			t.Errorf("stats = %+v, want 1 degraded run (insufficient_proposers)", st)
		}
		if len(out.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(out.Runs))
		}
		run := out.Runs[0]
		if run.Degraded != fusion.DegradedInsufficientProposers || run.DraftsUsed != 0 {
			t.Errorf("run degraded = %q drafts %d, want insufficient_proposers/0", run.Degraded, run.DraftsUsed)
		}
		// The failures are visible per leg; the delayed member was cut once the
		// quorum became unreachable.
		byProv := map[string]fusion.LegObservation{}
		for _, l := range run.Legs {
			byProv[l.Provider] = l
		}
		if byProv["pb"].Status != 500 || byProv["pc"].Status != 500 {
			t.Errorf("failed leg statuses = %+v", byProv)
		}
		if !byProv["pa"].Cut {
			t.Errorf("pa leg should be cut after the quorum became unreachable: %+v", byProv["pa"])
		}
		// The run fell back to the synthesizer with the original body — still
		// committed, so the synth read-back is present.
		if !run.SynthCommitted || run.SynthStatus != 200 {
			t.Errorf("degraded synth = committed %v status %d", run.SynthCommitted, run.SynthStatus)
		}
		m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "fusion", Model: "recipe"}]
		if m.Requests != 1 || m.Failovers != 1 {
			t.Errorf("fusion metrics = requests %d failovers %d, want 1/1", m.Requests, m.Failovers)
		}
		// Filter to an unknown workflow: empty snapshot.
		out, _ = apiFusionGet(t, proxy, "?workflow=nope")
		if len(out.Workflows) != 0 || len(out.Runs) != 0 {
			t.Errorf("filtered snapshot = %+v, want empty", out)
		}
	})

	t.Run("grace-cut leg recorded", func(t *testing.T) {
		old := fusionGracePeriod
		fusionGracePeriod = 150 * time.Millisecond
		defer func() { fusionGracePeriod = old }()
		pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		pc := newFakeUpstream(t, delayedResponder(600*time.Millisecond, anthropicDraftResponder("draft-C")))
		ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
		recipe := FusionConfig{
			Panel: []RouteTarget{
				{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}, {Provider: "pc", Model: "mc"},
			},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
		postAnthropic(t, px, fusionClientBody)

		awaitFusionState(t, "grace-cut run recorded", func() bool {
			o, _ := apiFusionGet(t, proxy, "")
			return len(o.Runs) == 1
		})
		out, _ := apiFusionGet(t, proxy, "")
		if len(out.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(out.Runs))
		}
		var cutSeen bool
		for _, l := range out.Runs[0].Legs {
			if l.Provider == "pc" {
				cutSeen = l.Cut
			}
		}
		if !cutSeen {
			t.Errorf("straggler leg not marked cut: %+v", out.Runs[0].Legs)
		}
	})
}

// --- cost gates ---

// TestFusion_BudgetExceeded: with max_runs_per_day=1 the first request
// orchestrates and the second degrades to a direct synthesizer call (panel
// untouched), recorded as budget_exceeded.
func TestFusion_BudgetExceeded(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("answer"))
	recipe := FusionConfig{
		Panel:         []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer:   RouteTarget{Provider: "ps", Model: "ms"},
		MaxRunsPerDay: 1,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	postAnthropic(t, px, fusionClientBody)
	if pa.hits() != 1 || pb.hits() != 1 {
		t.Fatalf("first request should orchestrate (hits pa=%d pb=%d)", pa.hits(), pb.hits())
	}
	postAnthropic(t, px, fusionClientBody)
	if pa.hits() != 1 || pb.hits() != 1 {
		t.Errorf("second request should NOT fan out (hits pa=%d pb=%d)", pa.hits(), pb.hits())
	}
	if strings.Contains(ps.lastBody(), "CANDIDATE") {
		t.Errorf("over-budget body should be the original (no candidates): %s", ps.lastBody())
	}
	awaitFusionState(t, "budget-exceeded run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return o.Workflows["recipe"].Runs == 2
	})
	out, _ := apiFusionGet(t, proxy, "")
	st := out.Workflows["recipe"]
	if st.Runs != 2 || st.QuorumMet != 1 || st.Degraded[fusion.DegradedBudgetExceeded] != 1 || st.RunsToday != 1 {
		t.Errorf("stats = %+v, want runs 2, quorum 1, budget_exceeded 1, runs_today 1", st)
	}
}

// TestFusion_FirstTurnOnly: a first_turn_only recipe orchestrates the first
// turn and answers a multi-turn conversation directly (multi_turn), without
// touching the panel.
func TestFusion_FirstTurnOnly(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("answer"))
	recipe := FusionConfig{
		Panel:         []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer:   RouteTarget{Provider: "ps", Model: "ms"},
		FirstTurnOnly: true,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	postAnthropic(t, px, fusionClientBody) // single user message → orchestrated
	if pa.hits() != 1 {
		t.Fatalf("first turn should orchestrate (pa hits %d)", pa.hits())
	}
	multiTurn := `{"model":"hard","max_tokens":100,"stream":true,"messages":[` +
		`{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`
	postAnthropic(t, px, multiTurn)
	if pa.hits() != 1 || pb.hits() != 1 {
		t.Errorf("multi-turn request should NOT fan out (hits pa=%d pb=%d)", pa.hits(), pb.hits())
	}
	if strings.Contains(ps.lastBody(), "CANDIDATE") {
		t.Errorf("multi-turn body should be the original (no candidates): %s", ps.lastBody())
	}
	awaitFusionState(t, "multi-turn degraded run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return o.Workflows["recipe"].Runs == 2
	})
	out, _ := apiFusionGet(t, proxy, "")
	st := out.Workflows["recipe"]
	if st.Runs != 2 || st.Degraded[fusion.DegradedMultiTurn] != 1 || st.RunsToday != 1 {
		t.Errorf("stats = %+v, want runs 2, multi_turn 1, runs_today 1 (degraded runs don't consume budget)", st)
	}
}

// --- quality knobs ---

// TestFusion_JudgeReport: a configured judge reviews the candidates (one
// non-streaming call through the leg pipeline) and its report is injected
// into the synthesis body before the candidate sections.
func TestFusion_JudgeReport(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	pj := newFakeUpstream(t, anthropicDraftResponder("judge: consensus on X"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	judge := RouteTarget{Provider: "pj", Model: "mj"}
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		Judge:       &judge,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pj": pj, "ps": ps})
	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing answer: %s", out)
	}
	// The judge got a non-streaming, model-rewritten review request carrying
	// the judge instruction + both candidates, tools stripped.
	if pj.hits() != 1 {
		t.Fatalf("judge hits = %d, want 1", pj.hits())
	}
	var jb map[string]any
	if err := json.Unmarshal([]byte(pj.lastBody()), &jb); err != nil {
		t.Fatalf("judge body not JSON: %v", err)
	}
	if jb["model"] != "mj" || jb["stream"] != false {
		t.Errorf("judge body model/stream = %v/%v, want mj/false", jb["model"], jb["stream"])
	}
	jsys, _ := jb["system"].(string)
	if !strings.Contains(jsys, "评审模型") || !strings.Contains(jsys, "draft-A") || !strings.Contains(jsys, "draft-B") {
		t.Errorf("judge system missing instruction/candidates: %q", jsys)
	}
	// The synthesis body carries the judge report BEFORE the candidates.
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	ji := strings.Index(synth.System, "<JUDGE_ANALYSIS>")
	ci := strings.Index(synth.System, "<CANDIDATE 1>")
	if ji < 0 || !strings.Contains(synth.System, "judge: consensus on X") {
		t.Errorf("synthesis system missing judge report: %q", synth.System)
	}
	if ci < 0 || ji > ci {
		t.Errorf("judge report should precede candidates (judge %d, candidate %d)", ji, ci)
	}
	// Registry: judge leg observed (kind=judge), report used, judge tokens
	// aggregated separately from the panel.
	awaitFusionState(t, "judge run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return len(o.Runs) == 1
	})
	outAPI, _ := apiFusionGet(t, proxy, "")
	run := outAPI.Runs[0]
	if !run.JudgeUsed {
		t.Errorf("run.JudgeUsed = false, want true")
	}
	var judgeLeg *fusion.LegObservation
	for i := range run.Legs {
		if run.Legs[i].Kind == "judge" {
			judgeLeg = &run.Legs[i]
		}
	}
	if judgeLeg == nil || judgeLeg.Status != 200 || judgeLeg.Err != "" {
		t.Errorf("judge leg = %+v, want clean 200", judgeLeg)
	}
	st := outAPI.Workflows["recipe"]
	if st.JudgeInput != 11 || st.JudgeOutput != 7 { // anthropicDraftResponder usage
		t.Errorf("judge tokens = %d/%d, want 11/7", st.JudgeInput, st.JudgeOutput)
	}
	// The judge call went through the standard per-(provider,model) books.
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pj", Model: "mj"}]; m.Requests != 1 {
		t.Errorf("judge metrics requests = %d, want 1", m.Requests)
	}
}

// TestFusion_JudgeFailure: a failing judge only skips the report — the run
// still synthesizes from the candidates, is not degraded, and records the
// judge leg's error.
func TestFusion_JudgeFailure(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	pj := newFakeUpstream(t, statusResponder(500))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	judge := RouteTarget{Provider: "pj", Model: "mj"}
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		Judge:       &judge,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pj": pj, "ps": ps})
	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing answer: %s", out)
	}
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if strings.Contains(synth.System, "JUDGE_ANALYSIS") {
		t.Errorf("failed judge should leave no report section: %q", synth.System)
	}
	if !strings.Contains(synth.System, "draft-A") {
		t.Errorf("synthesis should still carry candidates: %q", synth.System)
	}
	awaitFusionState(t, "judge-failure run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return len(o.Runs) == 1
	})
	outAPI, _ := apiFusionGet(t, proxy, "")
	run := outAPI.Runs[0]
	if run.Degraded != "" || run.JudgeUsed {
		t.Errorf("run = degraded %q judgeUsed %v, want no degrade, no judge report", run.Degraded, run.JudgeUsed)
	}
	var judgeLeg *fusion.LegObservation
	for i := range run.Legs {
		if run.Legs[i].Kind == "judge" {
			judgeLeg = &run.Legs[i]
		}
	}
	if judgeLeg == nil || judgeLeg.Status != 500 || judgeLeg.Err == "" {
		t.Errorf("judge leg = %+v, want recorded 500 failure", judgeLeg)
	}
}

// TestFusion_InstructionOverride: recipe.Instruction replaces the fixed
// synthesis preamble.
func TestFusion_InstructionOverride(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		Instruction: "自定义汇总指令X",
	}
	_, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	postAnthropic(t, px, fusionClientBody)
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if !strings.Contains(synth.System, "自定义汇总指令X") {
		t.Errorf("synthesis missing custom instruction: %q", synth.System)
	}
	if strings.Contains(synth.System, "结果汇总模型") {
		t.Errorf("default instruction should be replaced: %q", synth.System)
	}
}
