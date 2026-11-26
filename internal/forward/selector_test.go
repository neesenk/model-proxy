package forward

import (
	"strings"
	"testing"
)

func TestBuildSelectorStateExtraction(t *testing.T) {
	candidates := []selectorCandidate{
		{Target: RouteTarget{Provider: "a", Model: "ma", Rubric: "cheap generalist"}, ID: "c0", Rubric: "cheap generalist"},
		{Target: RouteTarget{Provider: "b", Model: "mb"}, ID: "c1"},
	}
	facts := selectorRequestFacts{EstimatedTokens: 123}

	t.Run("anthropic blocks", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ans"},{"role":"user","content":[{"type":"text","text":"second"},{"type":"image","source":{}}]}]}`)
		state := buildSelectorState(body, "anthropic", facts, true, candidates)
		if state["task"] != "second" {
			t.Errorf("task = %v", state["task"])
		}
		conversation := state["conversation"].(map[string]any)
		if conversation["turns"] != 3 || conversation["has_tools"] != true || conversation["estimated_input_tokens"] != int64(123) {
			t.Errorf("conversation = %+v", conversation)
		}
		options := state["candidates"].([]map[string]any)
		if options[0]["rubric"] != "cheap generalist" || options[1]["rubric"] != "b/mb" {
			t.Errorf("options = %+v", options)
		}
	})
	t.Run("openai string content", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
		state := buildSelectorState(body, "openai", facts, false, candidates)
		if state["task"] != "hello world" {
			t.Errorf("task = %v", state["task"])
		}
	})
	t.Run("responses input_text", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"resp q"}]}]}`)
		state := buildSelectorState(body, "responses", facts, false, candidates)
		if state["task"] != "resp q" {
			t.Errorf("task = %v", state["task"])
		}
	})
	t.Run("truncation", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", selectorStateMaxRunes+500) + `"}]}`)
		state := buildSelectorState(body, "openai", facts, false, candidates)
		if got := len([]rune(state["task"].(string))); got != selectorStateMaxRunes {
			t.Errorf("task runes = %d, want cap %d", got, selectorStateMaxRunes)
		}
	})
	t.Run("unparseable body degrades", func(t *testing.T) {
		state := buildSelectorState([]byte(`nope`), "openai", facts, false, candidates)
		if state["task"] != "" {
			t.Errorf("task = %v", state["task"])
		}
	})
}

func TestSelectorCriteriaIncludesTargetAndRubric(t *testing.T) {
	criteria := selectorCriteria([]selectorCandidate{
		{Target: RouteTarget{Provider: "a", Model: "ma"}, ID: "c0"},
		{Target: RouteTarget{Provider: "b", Model: "mb"}, ID: "c1", Rubric: "fast"},
	})
	if criteria["c0"] != "a/ma" || criteria["c1"] != "b/mb — fast" {
		t.Fatalf("criteria = %+v", criteria)
	}
}
