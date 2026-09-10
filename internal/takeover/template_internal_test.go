package takeover

import (
	"strings"
	"testing"

	"model-proxy/internal/catalog"
)

// TestSubstituteValueDoesNotMutateSharedTemplate pins the purity fix: the
// decoded t.JSON.Set values belong to a shared *Template, so a second render
// must not see the first render's substitutions baked in.
func TestSubstituteValueDoesNotMutateSharedTemplate(t *testing.T) {
	shared := map[string]any{
		"nested": []any{map[string]any{"url": "{{base_url}}"}},
		"keep":   "{{base_url}}",
	}
	first := substituteValue(shared, func(s string) string {
		return strings.ReplaceAll(s, "{{base_url}}", "https://first.example.com")
	}).(map[string]any)
	if first["nested"].([]any)[0].(map[string]any)["url"] != "https://first.example.com" {
		t.Fatalf("first substitution did not apply: %#v", first)
	}
	if shared["keep"] != "{{base_url}}" {
		t.Fatalf("shared template mutated by first render: %#v", shared)
	}
	second := substituteValue(shared, func(s string) string {
		return strings.ReplaceAll(s, "{{base_url}}", "https://second.example.com")
	}).(map[string]any)
	if got := second["nested"].([]any)[0].(map[string]any)["url"]; got != "https://second.example.com" {
		t.Fatalf("second render reused first render's value: %q", got)
	}
}

// TestTomlTopKeyValueExactKeyMatch pins the prefix-match fix: reading "model"
// must not hit a longer key like "model_provider".
func TestTomlTopKeyValueExactKeyMatch(t *testing.T) {
	text := "model_provider = \"opencode\"\nmodel = \"gpt-x\"\n"
	if v, ok := tomlTopKeyValue(text, "model"); !ok || v != "gpt-x" {
		t.Errorf("tomlTopKeyValue(model) = (%q, %v), want (\"gpt-x\", true)", v, ok)
	}
	if v, ok := tomlTopKeyValue(text, "model_provider"); !ok || v != "opencode" {
		t.Errorf("tomlTopKeyValue(model_provider) = (%q, %v), want (\"opencode\", true)", v, ok)
	}
	if _, ok := tomlTopKeyValue("modelx = \"y\"", "model"); ok {
		t.Error("tomlTopKeyValue(model) matched key modelx")
	}
}

// TestPiInputModalitiesSchemaConformant pins the pi models.json contract:
// pi's ModelDefinitionSchema accepts input values "text" and "image" ONLY —
// catalog modalities like "pdf"/"video" must be dropped, and an all-dropped
// (or empty) list falls back to ["text"]. A regression here writes an invalid
// models.json and pi refuses to start.
func TestPiInputModalitiesSchemaConformant(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"text"}, []string{"text"}},
		{[]string{"text", "image"}, []string{"text", "image"}},
		{[]string{"text", "image", "pdf"}, []string{"text", "image"}},
		{[]string{"text", "image", "video"}, []string{"text", "image"}},
		{[]string{"pdf", "video"}, []string{"text"}},
		{nil, []string{"text"}},
	}
	for _, testCase := range cases {
		got := piInputModalities(testCase.in)
		if len(got) != len(testCase.want) {
			t.Errorf("piInputModalities(%v) = %v, want %v", testCase.in, got, testCase.want)
			continue
		}
		for i := range got {
			if got[i] != testCase.want[i] {
				t.Errorf("piInputModalities(%v) = %v, want %v", testCase.in, got, testCase.want)
				break
			}
		}
	}
}

// TestPiModelsCollectionReasoningAndInput pins the pi models.json contract:
// reasoning-capable catalog models must carry "reasoning": true (without it
// pi never sends reasoning params and thinking is silently disabled), and
// input modalities are projected to pi's text|image schema.
func TestPiModelsCollectionReasoningAndInput(t *testing.T) {
	models := []ExposedModel{
		{Exposed: "thinker", PM: catalog.Model{Reasoning: true, Modalities: catalog.Modalities{Input: []string{"text", "image", "pdf"}}}},
		{Exposed: "plain", PM: catalog.Model{Modalities: catalog.Modalities{Input: []string{"text"}}}},
		{Exposed: "bare"},
	}
	got := piModelsCollection(models)
	byID := map[string]map[string]any{}
	for _, e := range got {
		byID[e["id"].(string)] = e
	}
	if r, ok := byID["thinker"]["reasoning"]; !ok || r != true {
		t.Errorf("thinker reasoning = %v (ok=%v), want true", r, ok)
	}
	if _, ok := byID["plain"]["reasoning"]; ok {
		t.Error("plain model must not carry reasoning (pi defaults false)")
	}
	if in := byID["thinker"]["input"].([]string); len(in) != 2 || in[0] != "text" || in[1] != "image" {
		t.Errorf("thinker input = %v, want [text image]", in)
	}
	if in := byID["bare"]["input"].([]string); len(in) != 1 || in[0] != "text" {
		t.Errorf("bare input = %v, want [text] fallback", in)
	}
	// Every entry opts into pi's session-affinity header so the proxy can
	// attribute requests to a session (request-log session_id / live events).
	for id, e := range byID {
		compat, ok := e["compat"].(map[string]any)
		if !ok || compat["sendSessionAffinityHeaders"] != true {
			t.Errorf("%s compat = %v, want sendSessionAffinityHeaders:true", id, e["compat"])
		}
	}
}
