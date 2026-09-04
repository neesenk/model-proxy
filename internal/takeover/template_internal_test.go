package takeover

import (
	"strings"
	"testing"
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
