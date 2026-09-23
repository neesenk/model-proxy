package provider

import (
	"reflect"
	"testing"
)

// reasoning_effort_test.go — ChatEffortProfile registry: exact enum maps and
// the model gating for each registered provider; everything else stays the
// zero profile (nil Enum = switch only).

func TestChatEffortProfile_Deepseek(t *testing.T) {
	want := map[string]string{
		"minimal": "low", "low": "low", "medium": "high",
		"high": "high", "xhigh": "high", "max": "max",
	}
	p := ChatEffortProfile("deepseek", "deepseek-chat")
	if !reflect.DeepEqual(p.Enum, want) || p.EnumOnly {
		t.Errorf("deepseek profile = %+v, want enum %v (EnumOnly false)", p, want)
	}
	// Model-independent.
	if p := ChatEffortProfile("deepseek", "deepseek-reasoner"); !reflect.DeepEqual(p.Enum, want) {
		t.Errorf("deepseek-reasoner profile = %+v, want same enum", p)
	}
}

func TestChatEffortProfile_Zhipu(t *testing.T) {
	want53 := map[string]string{
		"none": "low", "minimal": "low", "low": "low", "medium": "high",
		"high": "high", "xhigh": "high", "max": "max",
	}
	want52 := map[string]string{
		"minimal": "minimal", "low": "low", "medium": "medium",
		"high": "high", "xhigh": "xhigh", "max": "max",
	}
	if p := ChatEffortProfile("zhipu", "glm-5.3"); !reflect.DeepEqual(p.Enum, want53) || p.EnumOnly {
		t.Errorf("glm-5.3 profile = %+v, want %v", p, want53)
	}
	if p := ChatEffortProfile("zhipu", "glm-5.3-flash"); !reflect.DeepEqual(p.Enum, want53) {
		t.Errorf("glm-5.3-flash profile = %+v, want the 5.3 enum (prefix gate)", p)
	}
	if p := ChatEffortProfile("zhipu", "glm-5.2"); !reflect.DeepEqual(p.Enum, want52) || p.EnumOnly {
		t.Errorf("glm-5.2 profile = %+v, want %v", p, want52)
	}
	// Older models: no level knob.
	if p := ChatEffortProfile("zhipu", "glm-4.6"); p.Enum != nil || p.EnumOnly {
		t.Errorf("glm-4.6 profile = %+v, want zero profile", p)
	}
}

func TestChatEffortProfile_KimiCode(t *testing.T) {
	want := map[string]string{
		"none": "low", "minimal": "low", "low": "low", "medium": "high",
		"high": "high", "xhigh": "high", "max": "max",
	}
	p := ChatEffortProfile("kimi-code", "kimi-k3")
	if !reflect.DeepEqual(p.Enum, want) || !p.EnumOnly {
		t.Errorf("kimi-k3 profile = %+v, want enum %v with EnumOnly", p, want)
	}
	// kimi-k2.x keeps the bare thinking switch.
	if p := ChatEffortProfile("kimi-code", "kimi-k2.6"); p.Enum != nil || p.EnumOnly {
		t.Errorf("kimi-k2.6 profile = %+v, want zero profile", p)
	}
	if p := ChatEffortProfile("kimi-code", "kimi-k2.7-code"); p.Enum != nil {
		t.Errorf("kimi-k2.7-code profile = %+v, want zero profile", p)
	}
}

func TestChatEffortProfile_QwenPlan(t *testing.T) {
	want := map[string]string{
		"minimal": "low", "low": "low", "medium": "medium",
		"high": "xhigh", "xhigh": "xhigh", "max": "xhigh",
	}
	if p := ChatEffortProfile("qwen-plan", "qwen3.8-max-preview"); !reflect.DeepEqual(p.Enum, want) || p.EnumOnly {
		t.Errorf("qwen3.8-max-preview profile = %+v, want %v", p, want)
	}
	// Non-3.8 models: no level knob.
	if p := ChatEffortProfile("qwen-plan", "qwen3.7-max"); p.Enum != nil || p.EnumOnly {
		t.Errorf("qwen3.7-max profile = %+v, want zero profile", p)
	}
}

func TestChatEffortProfile_StepPlan(t *testing.T) {
	// Step Plan's chat endpoint accepts reasoning_effort with the vendor enum
	// low|medium|high — a restricted pass-through field, no thinking switch.
	// Canonical rungs above high clamp down; "off" maps to low (the step
	// reasoning models have no off switch).
	want := map[string]string{
		"none": "low", "minimal": "low", "low": "low", "medium": "medium",
		"high": "high", "xhigh": "high", "max": "high",
	}
	// Model-independent (every step chat model shares the low|medium|high dial).
	for _, model := range []string{"step-5-preview", "step-3.7-flash", "step-3.5-flash", "step-3.5-flash-2603", "step-router-v1"} {
		if p := ChatEffortProfile("step-plan", model); !reflect.DeepEqual(p.Enum, want) || p.EnumOnly {
			t.Errorf("step-plan %s profile = %+v, want enum %v (EnumOnly false)", model, p, want)
		}
	}
}

func TestChatEffortProfile_ZeroProfileDefault(t *testing.T) {
	for _, tc := range []struct{ providerID, model string }{
		{"volcengine", "doubao-seed-2.0"}, // per-model subsets too fragmented
		{"aqp", "claude-sonnet"},          // OpenRouter dialect passes effort through
		{"shopee", "claude-sonnet"},
		{"codex", "gpt-5.5"},
		{"unknown", "whatever"},
	} {
		if p := ChatEffortProfile(tc.providerID, tc.model); p.Enum != nil || p.EnumOnly {
			t.Errorf("ChatEffortProfile(%q, %q) = %+v, want zero profile", tc.providerID, tc.model, p)
		}
	}
}
