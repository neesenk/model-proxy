package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/config"
)

func TestFusionRegistry_AdmitBudget(t *testing.T) {
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.Local)
	r := NewRegistry()
	if !r.Admit("w", 2, now) || !r.Admit("w", 2, now) || r.Admit("w", 2, now) {
		t.Fatal("daily workflow budget did not admit exactly two runs")
	}
	if !r.Admit("other", 1, now) || !r.Admit("w", 2, now.AddDate(0, 0, 1)) {
		t.Fatal("budget should be per-workflow and reset on the local day")
	}
	if !r.Admit("w", 0, now) || !r.Admit("w", -1, now) {
		t.Fatal("non-positive budget should be unlimited")
	}
	var nilRegistry *Registry
	if !nilRegistry.Admit("w", 1, now) {
		t.Fatal("nil registry should be permissive")
	}
	nilRegistry.Record(&Run{Workflow: "w"})
	if stats, runs := nilRegistry.Snapshot("", now); len(stats) != 0 || len(runs) != 0 {
		t.Fatal("nil registry snapshot should be empty")
	}
}

func TestFusionRegistry_RecordEvictAggregate(t *testing.T) {
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.Local)
	r := NewRegistry()
	const total = RecentRunCap + 5
	for i := 0; i < total; i++ {
		run := &Run{
			RunID:    fmt.Sprintf("r%d", i),
			Ts:       int64(i),
			Route:    "hard",
			Workflow: "w",
			Quorum:   2,
			Legs: []LegObservation{
				{Kind: "panel", Input: 10, Output: 5},
				{Kind: "judge", Input: 3, Output: 1},
			},
			SynthInput: 100, SynthOutput: 20, SynthCommitted: true,
		}
		if i%2 == 0 {
			run.DraftsUsed = 2
		} else {
			run.DraftsUsed = 1
			run.Degraded = DegradedInsufficientProposers
		}
		r.Record(run)
		if i == 0 {
			run.Legs[0].Input = 999 // Record must not retain caller-owned storage.
		}
	}

	stats, runs := r.Snapshot("w", now)
	if len(runs) != RecentRunCap {
		t.Fatalf("ring size = %d, want %d", len(runs), RecentRunCap)
	}
	if runs[0].RunID != "r204" || runs[len(runs)-1].RunID != "r5" {
		t.Fatalf("ring bounds = %q..%q, want r204..r5", runs[0].RunID, runs[len(runs)-1].RunID)
	}
	st := stats["w"]
	if st.Runs != total || st.QuorumMet != 103 {
		t.Errorf("run aggregate = runs %d quorum %d, want %d/103", st.Runs, st.QuorumMet, total)
	}
	if st.Degraded[DegradedInsufficientProposers] != 102 {
		t.Errorf("degraded aggregate = %+v, want insufficient_proposers=102", st.Degraded)
	}
	if st.PanelInput != 10*total || st.PanelOutput != 5*total {
		t.Errorf("panel tokens = %d/%d, want %d/%d", st.PanelInput, st.PanelOutput, 10*total, 5*total)
	}
	if st.JudgeInput != 3*total || st.JudgeOutput != total {
		t.Errorf("judge tokens = %d/%d, want %d/%d", st.JudgeInput, st.JudgeOutput, 3*total, total)
	}
	if st.SynthInput != 100*total || st.SynthOutput != 20*total {
		t.Errorf("synthesis tokens = %d/%d, want %d/%d", st.SynthInput, st.SynthOutput, 100*total, 20*total)
	}
	if st.Amplification != 1.16 {
		t.Errorf("amplification = %.2f, want 1.16", st.Amplification)
	}

	stats["w"] = WorkflowStats{Runs: 999}
	runs[0].RunID = "mutated"
	statsAgain, runsAgain := r.Snapshot("w", now)
	if statsAgain["w"].Runs != total || runsAgain[0].RunID != "r204" {
		t.Fatal("Snapshot returned aliases to registry state")
	}

	if !r.Admit("w", 5, now) || !r.Admit("w", 5, now) {
		t.Fatal("daily admissions failed")
	}
	statsAgain, _ = r.Snapshot("w", now)
	if statsAgain["w"].RunsToday != 2 {
		t.Errorf("runs_today = %d, want 2", statsAgain["w"].RunsToday)
	}

	otherRegistry := NewRegistry()
	otherRegistry.Record(&Run{RunID: "w1", Workflow: "w", Quorum: 2, DraftsUsed: 2})
	otherRegistry.Record(&Run{RunID: "x", Workflow: "other", Quorum: 2, DraftsUsed: 2})
	allStats, allRuns := otherRegistry.Snapshot("", now)
	if len(allStats) != 2 || allStats["other"].Runs != 1 || allStats["w"].Runs != 1 {
		t.Errorf("unfiltered stats = %+v, want one run in w and other", allStats)
	}
	if len(allRuns) != 2 || allRuns[0].RunID != "x" || allRuns[1].RunID != "w1" {
		t.Errorf("unfiltered runs = %+v, want newest x then w1", allRuns)
	}
}

func TestCollectResultsQuorumUnreachable(t *testing.T) {
	results := make(chan LegResult, 3)
	results <- LegResult{Index: 0, Err: errors.New("no draft")}
	var cancelled atomic.Int32
	successes, received := CollectResults(results, 3, 3, time.Second, func() { cancelled.Add(1) })
	if successes != nil || len(received) != 1 || received[0].Index != 0 {
		t.Fatalf("collection = successes %#v received %#v", successes, received)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("cancel count = %d, want 1", cancelled.Load())
	}
}

func TestCollectResultsUsesOneGraceWindow(t *testing.T) {
	results := make(chan LegResult, 3)
	results <- LegResult{Index: 0}
	results <- LegResult{Index: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct {
		successes []LegResult
		received  []LegResult
	}, 1)
	go func() {
		successes, received := CollectResults(results, 3, 2, 25*time.Millisecond, cancel)
		done <- struct {
			successes []LegResult
			received  []LegResult
		}{successes, received}
	}()

	select {
	case <-ctx.Done():
		t.Fatal("collection cancelled before the grace window elapsed")
	case <-time.After(5 * time.Millisecond):
	}
	select {
	case got := <-done:
		if len(got.successes) != 2 || len(got.received) != 2 {
			t.Fatalf("collection after grace = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("collection did not finish after its grace window")
	}
	if ctx.Err() == nil {
		t.Fatal("grace expiry did not cancel remaining leg")
	}
}

func TestReconcileFusionLegs(t *testing.T) {
	panel := []config.RouteTarget{{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}}
	legs := ReconcileLegs(panel, []LegResult{{Index: 0, Provider: "a", Model: "ma", Status: 200, LatencyMs: 42, Usage: Usage{Input: 4, Output: 2}}})
	if len(legs) != 2 || legs[0].Input != 4 || legs[0].Cut || !legs[1].Cut || legs[1].Err != "cut by quorum/grace" {
		t.Fatalf("reconciled legs = %+v", legs)
	}
}

func TestBodyHasAssistantTurn(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"messages":[{"role":"assistant"}]}`, true},
		{`{"input":[{"role":"assistant"}]}`, true},
		{`{"input":"hello"}`, false},
		{`not json`, false},
	}
	for _, tc := range cases {
		if got := HasAssistantTurn([]byte(tc.body)); got != tc.want {
			t.Errorf("HasAssistantTurn(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestBuildBodiesAcrossProtocols(t *testing.T) {
	permuter := func(int) []int { return []int{1, 0} }
	t.Run("anthropic", func(t *testing.T) {
		body, ok := BuildSynthesisBodyWithPermuter([]byte(`{"system":"original","messages":[]}`), "anthropic", []string{"first", "second"}, "judge", "instruction", permuter)
		if !ok {
			t.Fatal("body build failed")
		}
		var request struct {
			System string `json:"system"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(request.System, "original\n\ninstruction") || !strings.Contains(request.System, "<JUDGE_ANALYSIS>") || strings.Index(request.System, "second") > strings.Index(request.System, "first") {
			t.Fatalf("anthropic section = %q", request.System)
		}
	})
	t.Run("openai", func(t *testing.T) {
		body, ok := BuildSynthesisBodyWithPermuter([]byte(`{"messages":[{"role":"user","content":"original"}]}`), "openai", []string{"first", "second"}, "", "instruction", permuter)
		if !ok {
			t.Fatal("body build failed")
		}
		var request struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) != 2 || request.Messages[1].Role != "user" || !strings.Contains(request.Messages[1].Content, "second") {
			t.Fatalf("chat injection = %+v", request.Messages)
		}
	})
	t.Run("responses", func(t *testing.T) {
		body, ok := BuildSynthesisBodyWithPermuter([]byte(`{"input":"original"}`), "responses", []string{"first", "second"}, "", "instruction", permuter)
		if !ok {
			t.Fatal("body build failed")
		}
		var request struct {
			Input []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Input) != 2 || request.Input[1].Type != "message" || request.Input[1].Role != "user" || request.Input[1].Content[0].Type != "input_text" || !strings.Contains(request.Input[1].Content[0].Text, "second") {
			t.Fatalf("responses injection = %+v", request.Input)
		}
	})
	if _, ok := BuildSynthesisBody([]byte(`not json`), "anthropic", nil, "", ""); ok {
		t.Fatal("bad JSON must fail synthesis-body construction")
	}
	if _, ok := BuildJudgeBody([]byte(`not json`), "anthropic", nil); ok {
		t.Fatal("bad JSON must fail judge-body construction")
	}
}

func TestStripDraftFieldsAndTruncateRunes(t *testing.T) {
	stripped := StripDraftFields([]byte(`{"tools":[{}],"tool_choice":"auto","stream_options":{},"stream":true,"x":1}`))
	var request map[string]any
	if err := json.Unmarshal(stripped, &request); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tools", "tool_choice", "stream_options"} {
		if _, exists := request[field]; exists {
			t.Errorf("%s remains in draft body", field)
		}
	}
	if request["stream"] != false || string(StripDraftFields([]byte(`not json`))) != "not json" {
		t.Fatalf("strip result = %s", stripped)
	}
	if got := TruncateRunes("你好世界", 3); got != "你好世" {
		t.Fatalf("CJK truncation = %q", got)
	}
	if got := TruncateRunes("abc", 0); got != "" {
		t.Fatalf("zero limit = %q", got)
	}
}

func TestParseUsageJSON(t *testing.T) {
	if got := ParseUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`)); got != (Usage{Input: 10, Output: 5, CacheCreation: 3, CacheRead: 2}) {
		t.Fatalf("anthropic usage = %+v", got)
	}
	if got := ParseUsage([]byte(`{"usage":{"prompt_tokens":20,"completion_tokens":7}}`)); got != (Usage{Input: 20, Output: 7}) {
		t.Fatalf("openai usage = %+v", got)
	}
	// OpenAI chat shape: prompt_tokens is inclusive of the cached share —
	// cached_tokens lands in CacheRead and is deducted from Input once.
	if got := ParseUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}`)); got != (Usage{Input: 40, Output: 7, CacheRead: 60}) {
		t.Fatalf("openai cached usage = %+v", got)
	}
	// Responses shape: same inclusive convention via input_tokens_details.
	if got := ParseUsage([]byte(`{"usage":{"input_tokens":80,"output_tokens":9,"input_tokens_details":{"cached_tokens":50}}}`)); got != (Usage{Input: 30, Output: 9, CacheRead: 50}) {
		t.Fatalf("responses cached usage = %+v", got)
	}
	// Anthropic's input_tokens is already cache-exclusive: no deduction even
	// when a details spelling rides along (merged by max, deducted once).
	if got := ParseUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":90,"prompt_tokens_details":{"cached_tokens":90}}}`)); got != (Usage{Input: 10, Output: 5, CacheRead: 90}) {
		t.Fatalf("anthropic usage with stray details = %+v", got)
	}
	// The deduction clamps at zero for a fully cached inclusive prompt.
	if got := ParseUsage([]byte(`{"usage":{"prompt_tokens":50,"prompt_tokens_details":{"cached_tokens":80}}}`)); got != (Usage{Input: 0, CacheRead: 80}) {
		t.Fatalf("over-cached usage = %+v", got)
	}
	if got := ParseUsage([]byte(`not json`)); got != (Usage{}) {
		t.Fatalf("invalid usage = %+v", got)
	}
}
