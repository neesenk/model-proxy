package fusion

import (
	"errors"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
)

func selectorRecipe(sel *configdomain.SelectorConfig) configdomain.FusionConfig {
	return configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "a", Model: "ma"},
			{Provider: "b", Model: "mb"},
			{Provider: "c", Model: "mc"},
		},
		Synthesizer: configdomain.RouteTarget{Provider: "s", Model: "ms"},
		Selector:    sel,
	}
}

func selectorRequest(recipe configdomain.FusionConfig) Request {
	return Request{
		Workflow:     "quality",
		RunID:        "request-sel",
		Route:        "hard",
		Protocol:     "anthropic",
		OriginalBody: []byte(`{"messages":[{"role":"user","content":"what is 2+2?"}]}`),
		Recipe:       recipe,
	}
}

func okSelectorPorts() *enginePorts {
	return &enginePorts{
		supportsTools: true,
		legs: map[string]LegResult{
			"a": {Text: "draft A", Status: 200, Usage: Usage{Input: 2, Output: 1}},
			"b": {Text: "draft B", Status: 200, Usage: Usage{Input: 3, Output: 1}},
			"c": {Text: "draft C", Status: 200, Usage: Usage{Input: 4, Output: 1}},
		},
		synthesis: SynthesisResult{Committed: true, Status: 200},
	}
}

func TestSelectorShadowRecordsButRoutesUnchanged(t *testing.T) {
	registry := NewRegistry()
	ports := okSelectorPorts()
	ports.selector = SelectResult{ChoiceID: "c1", Confidence: 0.9, Difficulty: 1.0, LatencyMs: 42}
	sel := &configdomain.SelectorConfig{
		Target: configdomain.RouteTarget{Provider: "typesafe", Model: "jev", Protocol: "decisions"},
		Mode:   "shadow", DirectScoreMax: 1.5,
	}
	result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), selectorRequest(selectorRecipe(sel)), ports)
	if !result.Committed {
		t.Fatalf("run = %+v", result.Run)
	}
	if len(ports.selectCalls) != 1 {
		t.Fatalf("select calls = %d, want 1", len(ports.selectCalls))
	}
	// Shadow must not change routing: full panel fanned out, synthesizer answers.
	if len(ports.calls) != 3 {
		t.Errorf("panel legs = %d, want full static panel of 3", len(ports.calls))
	}
	if ports.synthTarget.Provider != "s" || result.Run.Degraded != "" {
		t.Errorf("synth = %+v degraded = %q", ports.synthTarget, result.Run.Degraded)
	}
	obs := result.Run.Selector
	if obs == nil || obs.Mode != "shadow" || obs.Action != "none" ||
		obs.Choice != "b/mb" || obs.Confidence != 0.9 || obs.Difficulty != 1.0 || obs.LatencyMs != 42 {
		t.Errorf("selector observation = %+v", obs)
	}
}

func TestSelectorDirectSkipsOrchestrationAndBudget(t *testing.T) {
	registry := NewRegistry()
	ports := okSelectorPorts()
	ports.selector = SelectResult{ChoiceID: "c1", Confidence: 0.9, Difficulty: 1.2}
	sel := &configdomain.SelectorConfig{
		Target: configdomain.RouteTarget{Provider: "typesafe", Model: "jev", Protocol: "decisions"},
		Mode:   "enforce", DirectScoreMax: 1.5, Confidence: 0.55,
	}
	recipe := selectorRecipe(sel)
	recipe.MaxRunsPerDay = 1
	result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), selectorRequest(recipe), ports)
	if !result.Committed {
		t.Fatalf("run = %+v", result.Run)
	}
	// Direct: the CHOSEN panel member answers the original body; no fan-out.
	if len(ports.calls) != 0 {
		t.Errorf("panel legs fanned out = %d, want 0 (direct)", len(ports.calls))
	}
	if ports.synthTarget.Provider != "b" || ports.synthTarget.Model != "mb" {
		t.Errorf("direct target = %+v, want b/mb", ports.synthTarget)
	}
	if string(ports.synthBody) != string(`{"messages":[{"role":"user","content":"what is 2+2?"}]}`) {
		t.Errorf("direct body = %s, want the original request body", ports.synthBody)
	}
	if result.Run.Degraded != DegradedSelectorDirect {
		t.Errorf("degraded = %q, want %q", result.Run.Degraded, DegradedSelectorDirect)
	}
	if result.Run.Selector == nil || result.Run.Selector.Action != "direct" {
		t.Errorf("selector observation = %+v", result.Run.Selector)
	}
	// No orchestration ran: the daily budget admission must NOT be consumed.
	stats, _ := registry.Snapshot("quality", time.Now())
	if stats["quality"].RunsToday != 0 {
		t.Errorf("RunsToday = %d, want 0 (selector_direct is not an orchestration)", stats["quality"].RunsToday)
	}
}

func TestSelectorDirectBlockedByToolsGate(t *testing.T) {
	registry := NewRegistry()
	ports := okSelectorPorts()
	ports.supportsTools = false // the chosen candidate cannot serve tools
	ports.selector = SelectResult{ChoiceID: "c0", Confidence: 0.9, Difficulty: 0.5}
	sel := &configdomain.SelectorConfig{
		Target: configdomain.RouteTarget{Provider: "typesafe", Model: "jev", Protocol: "decisions"},
		Mode:   "enforce", DirectScoreMax: 1.5, Confidence: 0.55,
	}
	request := selectorRequest(selectorRecipe(sel))
	request.HasTools = true
	request.OriginalBody = []byte(`{"messages":[{"role":"user","content":"use the tool"}],"tools":[{"name":"x"}]}`)
	result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), request, ports)
	// No direct to a tool-less choice; synthesizer also lacks tools → the
	// ordinary tools gate degrades (as without a selector).
	if ports.synthTarget.Provider == "a" {
		t.Errorf("must not direct to tool-less chosen target")
	}
	if result.Run.Degraded != DegradedToolsUnsupported {
		t.Errorf("degraded = %q, want %q", result.Run.Degraded, DegradedToolsUnsupported)
	}
	if result.Run.Selector == nil || result.Run.Selector.Action == "direct" {
		t.Errorf("selector observation = %+v", result.Run.Selector)
	}
}

func TestSelectorFallbacks(t *testing.T) {
	sel := func() *configdomain.SelectorConfig {
		return &configdomain.SelectorConfig{
			Target: configdomain.RouteTarget{Provider: "typesafe", Model: "jev", Protocol: "decisions"},
			Mode:   "enforce", DirectScoreMax: 1.5, Confidence: 0.55,
		}
	}
	cases := []struct {
		name     string
		result   SelectResult
		wantErr  string
		wantActs string
	}{
		{"decision call failed", SelectResult{Err: errors.New("boom")}, "boom", "fallback"},
		{"low confidence", SelectResult{ChoiceID: "c0", Confidence: 0.4, Difficulty: 0.5}, "", "fallback"},
		{"unknown choice id", SelectResult{ChoiceID: "cX", Confidence: 0.9}, "unknown_choice", "fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry()
			ports := okSelectorPorts()
			ports.selector = tc.result
			result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), selectorRequest(selectorRecipe(sel())), ports)
			if len(ports.calls) != 3 {
				t.Errorf("panel legs = %d, want static full panel", len(ports.calls))
			}
			if ports.synthTarget.Provider != "s" {
				t.Errorf("synth = %+v, want synthesizer", ports.synthTarget)
			}
			obs := result.Run.Selector
			if obs == nil || obs.Action != tc.wantActs {
				t.Errorf("observation = %+v, want action %q", obs, tc.wantActs)
			}
			if tc.wantErr != "" && (obs == nil || !strings.Contains(obs.Err, tc.wantErr)) {
				t.Errorf("observation err = %q, want substring %q", obs.Err, tc.wantErr)
			}
		})
	}
}

func TestSelectorTrimPanelByProbability(t *testing.T) {
	registry := NewRegistry()
	ports := okSelectorPorts()
	ports.selector = SelectResult{
		ChoiceID:   "c1",
		Confidence: 0.9,
		Difficulty: 3.0, // above direct_score_max → trim, not direct
		Probabilities: map[string]float64{
			"c0": 0.1, "c1": 0.7, "c2": 0.15, "c3": 0.05,
		},
	}
	sel := &configdomain.SelectorConfig{
		Target: configdomain.RouteTarget{Provider: "typesafe", Model: "jev", Protocol: "decisions"},
		Mode:   "enforce", DirectScoreMax: 1.5, Confidence: 0.55, PanelTopK: 2,
	}
	result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), selectorRequest(selectorRecipe(sel)), ports)
	if result.Run.Selector == nil || result.Run.Selector.Action != "trim" {
		t.Fatalf("observation = %+v, want trim", result.Run.Selector)
	}
	// Top-2 by probability: c1 (0.7) and c2 (0.15) → providers b and c (fan-out
	// is concurrent; assert the set, not the call order).
	if len(ports.calls) != 2 {
		t.Fatalf("trimmed fan-out = %+v", ports.calls)
	}
	providers := map[string]bool{ports.calls[0].Target.Provider: true, ports.calls[1].Target.Provider: true}
	if !providers["b"] || !providers["c"] {
		t.Errorf("trimmed fan-out = %+v, want providers {b, c}", ports.calls)
	}
	if result.Run.Quorum != 2 {
		t.Errorf("quorum = %d, want 2 (clamp to trimmed panel)", result.Run.Quorum)
	}
	if !result.Committed || result.Run.DraftsUsed != 2 {
		t.Errorf("run = %+v", result.Run)
	}
}

func TestSelectorSkipsImageRequests(t *testing.T) {
	registry := NewRegistry()
	ports := okSelectorPorts()
	sel := &configdomain.SelectorConfig{
		Target: configdomain.RouteTarget{Provider: "typesafe", Model: "jev", Protocol: "decisions"},
		Mode:   "enforce", DirectScoreMax: 1.5,
	}
	request := selectorRequest(selectorRecipe(sel))
	request.Facts.HasImage = true
	result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), request, ports)
	if len(ports.selectCalls) != 0 {
		t.Errorf("select calls = %d, want 0 for image requests", len(ports.selectCalls))
	}
	if len(ports.calls) != 3 {
		t.Errorf("panel legs = %d, want static panel", len(ports.calls))
	}
	if result.Run.Selector == nil || result.Run.Selector.Err != "skipped_image_request" {
		t.Errorf("observation = %+v", result.Run.Selector)
	}
}

func TestBuildSelectorStateExtraction(t *testing.T) {
	candidates := selectorCandidates(configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "a", Model: "ma", Rubric: "cheap generalist"},
			{Provider: "b", Model: "mb"},
		},
		Synthesizer: configdomain.RouteTarget{Provider: "a", Model: "ma"}, // dup → deduped
	})
	if len(candidates) != 2 || candidates[0].ID != "c0" || candidates[1].ID != "c1" {
		t.Fatalf("candidates = %+v", candidates)
	}
	facts := RequestFacts{HasImage: false, EstimatedTokens: 123}

	t.Run("anthropic blocks", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ans"},{"role":"user","content":[{"type":"text","text":"second"},{"type":"image","source":{}}]}]}`)
		state := BuildSelectorState(body, "anthropic", facts, true, candidates)
		if state["task"] != "second" {
			t.Errorf("task = %v", state["task"])
		}
		conv := state["conversation"].(map[string]any)
		if conv["turns"] != 3 || conv["has_tools"] != true || conv["estimated_input_tokens"] != int64(123) {
			t.Errorf("conversation = %+v", conv)
		}
		options := state["candidates"].([]map[string]any)
		if options[0]["rubric"] != "cheap generalist" || options[1]["rubric"] != "b/mb" {
			t.Errorf("options = %+v", options)
		}
	})
	t.Run("openai string content", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
		state := BuildSelectorState(body, "openai", facts, false, candidates)
		if state["task"] != "hello world" {
			t.Errorf("task = %v", state["task"])
		}
	})
	t.Run("responses input_text", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"resp q"}]}]}`)
		state := BuildSelectorState(body, "responses", facts, false, candidates)
		if state["task"] != "resp q" {
			t.Errorf("task = %v", state["task"])
		}
	})
	t.Run("truncation", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", selectorStateMaxRunes+500) + `"}]}`)
		state := BuildSelectorState(body, "openai", facts, false, candidates)
		if got := len([]rune(state["task"].(string))); got != selectorStateMaxRunes {
			t.Errorf("task runes = %d, want cap %d", got, selectorStateMaxRunes)
		}
	})
	t.Run("unparseable body degrades", func(t *testing.T) {
		state := BuildSelectorState([]byte(`nope`), "openai", facts, false, candidates)
		if state["task"] != "" {
			t.Errorf("task = %v", state["task"])
		}
	})
}

func TestTrimSelectorPanelNeverBelowQuorum(t *testing.T) {
	recipe := configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "a", Model: "ma"},
			{Provider: "b", Model: "mb"},
			{Provider: "c", Model: "mc"},
		},
		MinPanel: 2,
	}
	candidates := selectorCandidates(recipe)
	probs := map[string]float64{"c0": 0.05, "c1": 0.9, "c2": 0.05}

	// topK=1 must be raised to the quorum (2): b + one of a/c (stable tie on c0/c2 → a first).
	trimmed := trimSelectorPanel(recipe.Panel, candidates, probs, 1, quorumFor(recipe))
	if len(trimmed) != 2 || trimmed[0].Provider != "a" || trimmed[1].Provider != "b" {
		t.Errorf("trimmed = %+v, want [a b] (quorum floor, configured order)", trimmed)
	}
	// topK >= len(panel) is a no-op.
	if got := trimSelectorPanel(recipe.Panel, candidates, probs, 3, quorumFor(recipe)); len(got) != 3 {
		t.Errorf("len = %d, want unchanged panel", len(got))
	}
}
