package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fusion_obs_test.go covers the fusion observability layer (fusion_obs.go):
// the run registry (record / ring eviction / aggregates / daily budget), the
// /api/fusion endpoint, the cost gates (max_runs_per_day, first_turn_only),
// and the quality knobs (judge report injection + failure tolerance,
// instruction override), plus the YAML load/validate of the new recipe fields.

// --- registry unit tests ---

// TestFusionRegistry_AdmitBudget: the daily budget admits up to the limit,
// rejects beyond it, resets on the local-day rollover, and limit<=0 is
// unlimited. Nil-registry calls are no-op permissive.
func TestFusionRegistry_AdmitBudget(t *testing.T) {
	now := time.Now()
	r := newFusionRegistry()
	if !r.admit("w", 2, now) || !r.admit("w", 2, now) {
		t.Fatal("first two admissions should succeed")
	}
	if r.admit("w", 2, now) {
		t.Fatal("third admission should be rejected (budget exhausted)")
	}
	if !r.admit("other", 2, now) {
		t.Fatal("budget is per-workflow — another workflow should admit")
	}
	if !r.admit("w", 2, now.Add(25*time.Hour)) {
		t.Fatal("next-day admission should succeed (day rollover resets)")
	}
	if !r.admit("w", 0, now) || !r.admit("w", -1, now) {
		t.Fatal("limit <= 0 should be unlimited")
	}
	var nilReg *fusionRegistry
	if !nilReg.admit("w", 1, now) {
		t.Fatal("nil registry should admit")
	}
	nilReg.record(&fusionRun{Workflow: "w"}) // must not panic
	if stats, runs := nilReg.snapshot("", now); len(stats) != 0 || len(runs) != 0 {
		t.Fatal("nil registry snapshot should be empty")
	}
}

// TestFusionRegistry_RecordEvictAggregate: runs land in the ring (oldest
// evicted past the cap, newest first on read) and fold correctly into the
// per-workflow cumulative aggregate.
func TestFusionRegistry_RecordEvictAggregate(t *testing.T) {
	now := time.Now()
	r := newFusionRegistry()
	const total = fusionRecentCap + 5
	for i := 0; i < total; i++ {
		run := &fusionRun{
			RunID: fmt.Sprintf("r%d", i), Ts: int64(i),
			Route: "hard", Workflow: "w", Quorum: 2,
			Legs: []fusionLegObs{
				{Kind: "panel", Input: 10, Output: 5},
				{Kind: "judge", Input: 3, Output: 1},
			},
			SynthInput: 100, SynthOutput: 20, SynthCommitted: true,
		}
		if i%2 == 0 {
			run.DraftsUsed = 2 // quorum met
		} else {
			run.DraftsUsed = 1
			run.Degraded = fusionDegradedInsufficient
		}
		r.record(run)
	}
	// A second workflow aggregates independently.
	r3 := newFusionRegistry()
	r3.record(&fusionRun{RunID: "w1", Workflow: "w", Quorum: 2, DraftsUsed: 2})
	r3.record(&fusionRun{RunID: "x", Workflow: "other", Quorum: 2, DraftsUsed: 2})

	stats, runs := r.snapshot("w", now)
	if len(runs) != fusionRecentCap {
		t.Fatalf("ring size = %d, want %d", len(runs), fusionRecentCap)
	}
	if runs[0].RunID != "r204" {
		t.Errorf("newest run = %q, want r204", runs[0].RunID)
	}
	if runs[len(runs)-1].RunID != "r5" {
		t.Errorf("oldest kept run = %q, want r5 (older evicted)", runs[len(runs)-1].RunID)
	}
	st := stats["w"]
	if st.Runs != total {
		t.Errorf("runs = %d, want %d", st.Runs, total)
	}
	if st.QuorumMet != 103 { // even i in [0,205): 0,2,...,204
		t.Errorf("quorum_met = %d, want 103", st.QuorumMet)
	}
	if st.Degraded[fusionDegradedInsufficient] != 102 {
		t.Errorf("degraded[insufficient_proposers] = %d, want 102", st.Degraded[fusionDegradedInsufficient])
	}
	if st.PanelInput != 10*total || st.PanelOutput != 5*total {
		t.Errorf("panel tokens = %d/%d, want %d/%d", st.PanelInput, st.PanelOutput, 10*total, 5*total)
	}
	if st.JudgeInput != 3*total || st.JudgeOutput != 1*total {
		t.Errorf("judge tokens = %d/%d, want %d/%d", st.JudgeInput, st.JudgeOutput, 3*total, 1*total)
	}
	if st.SynthInput != 100*total || st.SynthOutput != 20*total {
		t.Errorf("synth tokens = %d/%d, want %d/%d", st.SynthInput, st.SynthOutput, 100*total, 20*total)
	}
	// amplification = (panel+judge+synth) / synth = (3075+820+24600)/24600.
	if st.Amplification != 1.16 {
		t.Errorf("amplification = %v, want 1.16", st.Amplification)
	}
	// Budget counter surfaced as runs_today.
	r.admit("w", 5, now)
	r.admit("w", 5, now)
	stats, _ = r.snapshot("w", now)
	if stats["w"].RunsToday != 2 {
		t.Errorf("runs_today = %d, want 2", stats["w"].RunsToday)
	}
	// Unfiltered snapshot sees both workflows.
	stats, _ = r3.snapshot("", now)
	if len(stats) != 2 || stats["other"].Runs != 1 || stats["w"].Runs != 1 {
		t.Errorf("unfiltered stats = %+v, want w+other", stats)
	}
}

// TestReconcileFusionLegs: members with a received result project into their
// observation; members without one are marked cut.
func TestReconcileFusionLegs(t *testing.T) {
	panel := []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}}
	received := []fusionLegResult{
		{idx: 0, provider: "pa", model: "ma", text: "d", status: 200, latencyMs: 42},
	}
	legs := reconcileFusionLegs(panel, received)
	if len(legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(legs))
	}
	if legs[0].Status != 200 || legs[0].LatencyMs != 42 || legs[0].Cut || legs[0].Err != "" {
		t.Errorf("leg 0 = %+v, want a clean 200 observation", legs[0])
	}
	if !legs[1].Cut || legs[1].Err == "" || legs[1].Provider != "pb" {
		t.Errorf("leg 1 = %+v, want cut pb observation", legs[1])
	}
}

// TestBodyHasAssistantTurn: the first_turn_only probe sees an assistant message
// in anthropic/openai messages[] and responses input[], and nothing else.
func TestBodyHasAssistantTurn(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"anthropic first turn", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"anthropic multi-turn", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"again"}]}`, true},
		{"openai multi-turn", `{"messages":[{"role":"system","content":"s"},{"role":"assistant","content":"a"}]}`, true},
		{"responses input list", `{"input":[{"role":"assistant","content":"a"}]}`, true},
		{"responses input string", `{"input":"hello"}`, false},
		{"system only", `{"system":"s","messages":[{"role":"user","content":"u"}]}`, false},
		{"garbage", `not json`, false},
	}
	for _, c := range cases {
		if got := bodyHasAssistantTurn([]byte(c.body)); got != c.want {
			t.Errorf("%s: bodyHasAssistantTurn = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- /api/fusion ---

// apiFusionGet queries /api/fusion on a rig proxy and decodes the response.
func apiFusionGet(t *testing.T, proxy *Proxy, query string) (struct {
	Workflows map[string]fusionWorkflowStats `json:"workflows"`
	Runs      []fusionRun                    `json:"runs"`
}, int) {
	t.Helper()
	w := newWebServer(proxy, "test-config.yaml")
	mux := http.NewServeMux()
	w.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/fusion"+query, nil))
	var out struct {
		Workflows map[string]fusionWorkflowStats `json:"workflows"`
		Runs      []fusionRun                    `json:"runs"`
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
		m := proxy.metrics.snapshot()[pmKey{Provider: "fusion", Model: "recipe"}]
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
		if st.Runs != 1 || st.QuorumMet != 0 || st.Degraded[fusionDegradedInsufficient] != 1 {
			t.Errorf("stats = %+v, want 1 degraded run (insufficient_proposers)", st)
		}
		if len(out.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(out.Runs))
		}
		run := out.Runs[0]
		if run.Degraded != fusionDegradedInsufficient || run.DraftsUsed != 0 {
			t.Errorf("run degraded = %q drafts %d, want insufficient_proposers/0", run.Degraded, run.DraftsUsed)
		}
		// The failures are visible per leg; the delayed member was cut once the
		// quorum became unreachable.
		byProv := map[string]fusionLegObs{}
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
		m := proxy.metrics.snapshot()[pmKey{Provider: "fusion", Model: "recipe"}]
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
	if st.Runs != 2 || st.QuorumMet != 1 || st.Degraded[fusionDegradedBudget] != 1 || st.RunsToday != 1 {
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
	if st.Runs != 2 || st.Degraded[fusionDegradedMultiTurn] != 1 || st.RunsToday != 1 {
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
	var judgeLeg *fusionLegObs
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
	if m := proxy.metrics.snapshot()[pmKey{Provider: "pj", Model: "mj"}]; m.Requests != 1 {
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
	var judgeLeg *fusionLegObs
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

// --- config load + validate + doctor ---

// TestLoadFusionObsKnobs guards the new FusionConfig fields: they must load
// from YAML (map-value decode, no rawConfig copy needed — trap #16) and obey
// their validate rules.
func TestLoadFusionObsKnobs(t *testing.T) {
	yaml := func(fusion string) []byte {
		return []byte(`listen: 127.0.0.1:15721
providers:
  a: {openai_base_url: "https://a", provider_id: static}
  b: {openai_base_url: "https://b", provider_id: static}
  s: {openai_base_url: "https://s", provider_id: static}
  j: {openai_base_url: "https://j", provider_id: static}
routes:
  hard:
    - {provider: fusion, model: r, priority: 1}
` + fusion)
	}
	valid := `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    min_panel: 2
    max_runs_per_day: 50
    first_turn_only: true
    judge: {provider: j, model: mj, protocol: openai}
    instruction: "自定义指令"
`
	cfg, err := LoadConfigFromBytes("config.yaml", yaml(valid))
	if err != nil {
		t.Fatalf("valid fusion config rejected: %v", err)
	}
	recipe := cfg.Fusion["r"]
	if recipe.MaxRunsPerDay != 50 || !recipe.FirstTurnOnly {
		t.Errorf("loaded cost knobs = max_runs_per_day %d first_turn_only %v", recipe.MaxRunsPerDay, recipe.FirstTurnOnly)
	}
	if recipe.Judge == nil || recipe.Judge.Provider != "j" || recipe.Judge.Model != "mj" || recipe.Judge.Protocol != "openai" {
		t.Errorf("loaded judge = %+v", recipe.Judge)
	}
	if recipe.Instruction != "自定义指令" {
		t.Errorf("loaded instruction = %q", recipe.Instruction)
	}

	cases := []struct {
		name          string
		fusion        string
		wantErrSubstr string
	}{
		{"negative budget", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    max_runs_per_day: -1
`, "max_runs_per_day"},
		{"instruction too long", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    instruction: "` + strings.Repeat("x", fusionInstructionMaxRunes+1) + `"
`, "instruction"},
		{"judge unknown provider", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    judge: {provider: ghost, model: mg}
`, "not defined"},
		{"judge nested fusion", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    judge: {provider: fusion, model: other}
`, "nested"},
		{"judge protocol without base url", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    judge: {provider: j, model: mj, protocol: anthropic}
`, "no anthropic_base_url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes("config.yaml", yaml(c.fusion))
			if err == nil {
				t.Fatalf("config accepted, want error containing %q", c.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), c.wantErrSubstr) {
				t.Fatalf("error %q missing %q", err, c.wantErrSubstr)
			}
		})
	}
}

// TestDoctorFusion: the doctor diagnostic renders a Fusion section with the
// panel/quorum plus the cost and quality knobs.
func TestDoctorFusion(t *testing.T) {
	setPoolHome(t, t.TempDir())
	cfg, err := LoadConfigFromBytes("config.yaml", []byte(`listen: 127.0.0.1:15721
providers:
  a: {openai_base_url: "https://a", provider_id: static}
  b: {openai_base_url: "https://b", provider_id: static}
  s: {openai_base_url: "https://s", provider_id: static}
  j: {openai_base_url: "https://j", provider_id: static}
fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    max_runs_per_day: 50
    first_turn_only: true
    judge: {provider: j, model: mj}
    instruction: "自定义指令"
routes:
  hard:
    - {provider: fusion, model: r, priority: 1}
`))
	if err != nil {
		t.Fatalf("config rejected: %v", err)
	}
	out := grabStdout(t, func() { doctorWithCfg(cfg) })
	for _, want := range []string{"Fusion", "panel=2", "quorum=2", "synthesizer=s/ms", "budget=50/day", "first_turn_only", "judge=j/mj", "custom instruction"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}
