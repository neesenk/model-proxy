package config

import (
	"strings"
	"testing"
)

// TestLoadFusion covers the fusion schema end to end: YAML loading and every
// structural validation rule owned by this package.
func TestLoadFusion(t *testing.T) {
	yaml := func(fusion, routes, shadow string) []byte {
		return []byte(`listen: 127.0.0.1:15721
providers:
  a: {openai_base_url: "https://a", provider_id: static}
  b: {openai_base_url: "https://b", provider_id: static}
  s: {openai_base_url: "https://s", provider_id: static}
` + fusion + routes + shadow)
	}
	validFusion := `fusion:
  hard-coding:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    min_panel: 2
`
	validRoutes := `routes:
  hard:
    - {provider: fusion, model: hard-coding, priority: 1}
    - {provider: s, model: ms, priority: 2}
`
	cfg, err := LoadConfigFromBytes("config.yaml", yaml(validFusion, validRoutes, ""))
	if err != nil {
		t.Fatalf("valid fusion config rejected: %v", err)
	}
	recipe := cfg.Fusion["hard-coding"]
	if len(recipe.Panel) != 2 || recipe.Panel[0].Provider != "a" || recipe.Panel[1].Model != "mb" {
		t.Errorf("loaded panel = %+v", recipe.Panel)
	}
	if recipe.Synthesizer.Provider != "s" || recipe.Synthesizer.Model != "ms" {
		t.Errorf("loaded synthesizer = %+v", recipe.Synthesizer)
	}
	if recipe.MinPanel != 2 {
		t.Errorf("loaded min_panel = %d, want 2", recipe.MinPanel)
	}

	cases := []struct {
		name          string
		fusion        string
		routes        string
		shadow        string
		wantErrSubstr string
	}{
		{"panel too small", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
    synthesizer: {provider: s, model: ms}
`, "", "", "2..4"},
		{"panel too big", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: a, model: ma}
      - {provider: a, model: ma}
      - {provider: a, model: ma}
      - {provider: a, model: ma}
    synthesizer: {provider: s, model: ms}
`, "", "", "2..4"},
		{"min_panel exceeds panel", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    min_panel: 3
`, "", "", "min_panel"},
		{"panel member unknown provider", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: ghost, model: mg}
    synthesizer: {provider: s, model: ms}
`, "", "", "not defined"},
		{"nested fusion rejected", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: fusion, model: other}
    synthesizer: {provider: s, model: ms}
`, "", "", "nested"},
		{"synthesizer unknown provider", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: ghost, model: ms}
`, "", "", "not defined"},
		{"member empty model", `fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: ""}
    synthesizer: {provider: s, model: ms}
`, "", "", "model is empty"},
		{"route references missing recipe", validFusion, `routes:
  hard:
    - {provider: fusion, model: no-such-recipe, priority: 1}
`, "", "not defined under fusion:"},
		{"route fusion takes no protocol", validFusion, `routes:
  hard:
    - {provider: fusion, model: hard-coding, priority: 1, protocol: openai}
`, "", "takes no protocol"},
		{"shadow cannot target fusion", validFusion, validRoutes, `shadow:
  hard: {provider: fusion, model: hard-coding}
`, "not a valid shadow target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routes := tc.routes
			if routes == "" {
				routes = `routes:
  hard:
    - {provider: fusion, model: r, priority: 1}
`
			}
			_, err := LoadConfigFromBytes("config.yaml", yaml(tc.fusion, routes, tc.shadow))
			if err == nil {
				t.Fatalf("config accepted, want error containing %q", tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("error %q missing %q", err, tc.wantErrSubstr)
			}
		})
	}
}

// TestLoadFusionObsKnobs guards the optional cost and quality controls and
// their validation rules.
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes("config.yaml", yaml(tc.fusion))
			if err == nil {
				t.Fatalf("config accepted, want error containing %q", tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("error %q missing %q", err, tc.wantErrSubstr)
			}
		})
	}
}
