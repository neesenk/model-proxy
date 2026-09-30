package config

import (
	"strings"
	"testing"
	"time"
)

const routePolicyBase = `
listen: 127.0.0.1:16000
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    models: [glm-5.3, glm-5.3-flash]
  typesafe:
    provider_id: typesafe
    decisions_base_url: https://api.typesafe.ai/v1
    models: [jev-1.13.0]
  anthropic-only:
    provider_id: deepseek
    anthropic_base_url: https://api.example.com/anthropic
    models: [deepseek-v4-pro]
routes:
  tier: [zhipu/glm-5.3, zhipu/glm-5.3-flash]
`

func TestRoutePolicyLoads(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
route_policy:
  tier:
    bands:
      - when: {follow_up: true, estimated_tokens_max: 8000}
        target: zhipu/glm-5.3-flash
      - when: {has_image: true}
        target: {provider: zhipu, model: glm-5.3}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	policy, ok := cfg.RoutePolicies["tier"]
	if !ok {
		t.Fatalf("route_policy tier missing: %+v", cfg.RoutePolicies)
	}
	if len(policy.Bands) != 2 {
		t.Fatalf("bands = %d, want 2", len(policy.Bands))
	}
	first := policy.Bands[0]
	if first.Target.Provider != "zhipu" || first.Target.Model != "glm-5.3-flash" {
		t.Fatalf("band 0 target = %+v", first.Target)
	}
	if first.When.FollowUp == nil || !*first.When.FollowUp {
		t.Fatalf("band 0 follow_up not parsed: %+v", first.When)
	}
	if first.When.EstimatedTokensMax == nil || *first.When.EstimatedTokensMax != 8000 {
		t.Fatalf("band 0 estimated_tokens_max not parsed: %+v", first.When)
	}
	if policy.Bands[1].When.HasImage == nil || !*policy.Bands[1].When.HasImage {
		t.Fatalf("band 1 has_image not parsed: %+v", policy.Bands[1].When)
	}
}

// TestRoutePolicyDerivedRouteKey accepts keys that come from a provider's model
// list (no explicit routes: entry) — routes are derived, and policies key off
// the exposed names either way.
func TestRoutePolicyDerivedRouteKey(t *testing.T) {
	_, err := LoadConfigFromBytes("x", []byte(`
listen: 127.0.0.1:16000
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    models: [glm-5.3, glm-5.3-flash]
route_policy:
  glm-5.3:
    bands:
      - when: {follow_up: true}
        target: zhipu/glm-5.3-flash
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
}

func TestRoutePolicyRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "unknown route key",
			doc:  "route_policy:\n  nope:\n    bands:\n      - when: {follow_up: true}\n        target: zhipu/glm-5.3\n",
			want: `route_policy "nope": route not found`,
		},
		{
			name: "no bands",
			doc:  "route_policy:\n  tier: {}\n",
			want: `route_policy "tier": no bands`,
		},
		{
			name: "empty when",
			doc:  "route_policy:\n  tier:\n    bands:\n      - when: {}\n        target: zhipu/glm-5.3\n",
			want: "when has no conditions",
		},
		{
			name: "min above max",
			doc:  "route_policy:\n  tier:\n    bands:\n      - when: {estimated_tokens_min: 100, estimated_tokens_max: 10}\n        target: zhipu/glm-5.3\n",
			want: "exceeds estimated_tokens_max",
		},
		{
			name: "negative max",
			doc:  "route_policy:\n  tier:\n    bands:\n      - when: {estimated_tokens_max: -1}\n        target: zhipu/glm-5.3\n",
			want: "estimated_tokens_max -1 out of range",
		},
		{
			name: "incomplete target",
			doc:  "route_policy:\n  tier:\n    bands:\n      - when: {follow_up: true}\n        target: {provider: zhipu}\n",
			want: "target must name a provider and a model",
		},
		{
			name: "unknown provider",
			doc:  "route_policy:\n  tier:\n    bands:\n      - when: {follow_up: true}\n        target: nope/glm-5.3\n",
			want: `provider "nope" not defined`,
		},
		{
			name: "protocol on a band target",
			doc:  "route_policy:\n  tier:\n    bands:\n      - when: {follow_up: true}\n        target: {provider: zhipu, model: glm-5.3, protocol: openai}\n",
			want: "declare protocol: on the route target instead",
		},
		{
			name: "missing fusion recipe",
			doc:  "route_policy:\n  tier:\n    bands:\n      - when: {follow_up: false}\n        target: fusion/nope\n",
			want: `fusion recipe "nope" not defined`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes("x", []byte(routePolicyBase+tc.doc))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestRoutePolicyAcceptsFusionBandTarget pins the slice-4 contract: a band may
// name a fusion recipe, which the pipeline dispatches like any other fusion
// route target.
func TestRoutePolicyAcceptsFusionBandTarget(t *testing.T) {
	_, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
fusion:
  hard:
    panel: [zhipu/glm-5.3, zhipu/glm-5.3-flash]
    synthesizer: zhipu/glm-5.3
route_policy:
  tier:
    bands:
      - when: {estimated_tokens_min: 60000}
        target: fusion/hard
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
}

func TestRoutePolicyEscalationLoads(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error]
      consecutive: 2
      target: zhipu/glm-5.3
      dwell: 10m
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	esc := cfg.RoutePolicies["tier"].Escalation
	if esc == nil {
		t.Fatal("escalation block not parsed")
	}
	if len(esc.BadSignals) != 1 || esc.BadSignals[0] != "upstream_error" {
		t.Fatalf("bad_signals = %v, want [upstream_error]", esc.BadSignals)
	}
	if esc.Consecutive != 2 {
		t.Fatalf("consecutive = %d, want 2", esc.Consecutive)
	}
	if esc.Target.Provider != "zhipu" || esc.Target.Model != "glm-5.3" {
		t.Fatalf("target = %+v", esc.Target)
	}
	if d := esc.DwellDuration(); d != 10*time.Minute {
		t.Fatalf("dwell duration = %v, want 10m", d)
	}
}

func TestRoutePolicySelectorLoads(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
      mode: shadow
      confidence: 0.6
      timeout: 800ms
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sel := cfg.RoutePolicies["tier"].Selector
	if sel == nil {
		t.Fatal("selector block not parsed")
	}
	if sel.Target.Provider != "typesafe" || sel.Target.Model != "jev-1.13.0" || sel.Target.Protocol != "decisions" {
		t.Fatalf("target = %+v", sel.Target)
	}
	if sel.SelectorMode() != "shadow" {
		t.Fatalf("mode = %q, want shadow", sel.SelectorMode())
	}
	if sel.ConfidenceThreshold() != 0.6 {
		t.Fatalf("confidence = %v, want 0.6", sel.ConfidenceThreshold())
	}
	if sel.TimeoutDuration() != 800*time.Millisecond {
		t.Fatalf("timeout = %v, want 800ms", sel.TimeoutDuration())
	}
}

func TestRoutePolicySelectorRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "missing protocol",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0}
`,
			want: `target must declare protocol: decisions`,
		},
		{
			name: "non-decisions protocol",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: openai}
`,
			want: `target must declare protocol: decisions`,
		},
		{
			name: "provider without decisions base",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: anthropic-only, model: deepseek-v4-pro, protocol: decisions}
`,
			want: `protocol:decisions but provider "anthropic-only" has neither decisions_base_url nor openai_base_url`,
		},
		{
			name: "invalid mode",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
      mode: magic
`,
			want: `mode "magic" invalid`,
		},
		{
			name: "confidence out of range",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
      confidence: 1.5
`,
			want: `confidence 1.5 out of range [0, 1]`,
		},
		{
			name: "direct_score_max non-zero",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
      direct_score_max: 2
`,
			want: `direct_score_max 2 is fusion-only and must be 0`,
		},
		{
			name: "panel_top_k non-zero",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
      panel_top_k: 1
`,
			want: `panel_top_k 1 is fusion-only and must be 0`,
		},
		{
			name: "invalid timeout",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
      timeout: not-a-duration
`,
			want: `timeout "not-a-duration" is not a valid duration`,
		},
		{
			name: "instruction too long",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
      instruction: "` + strings.Repeat("x", 4001) + `"
`,
			want: `instruction is 4001 runes, max 4000`,
		},
		{
			name: "fusion target rejected",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    selector:
      target: {provider: fusion, model: hard}
`,
			want: `provider "fusion" is not a valid selector target`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes("x", []byte(routePolicyBase+tc.doc))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRoutePolicyEscalationDefaults(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error]
      consecutive: 2
      target: zhipu/glm-5.3
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d := cfg.RoutePolicies["tier"].Escalation.DwellDuration(); d != 30*time.Minute {
		t.Fatalf("default dwell = %v, want 30m", d)
	}
}

func TestRoutePolicyEscalationRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "unknown bad signal",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error, empty_response]
      consecutive: 2
      target: zhipu/glm-5.3
`,
			want: `bad_signals "empty_response" is not supported (must be one of upstream_error, empty_ok, repeat_turn)`,
		},
		{
			name: "consecutive zero",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error]
      consecutive: 0
      target: zhipu/glm-5.3
`,
			want: "consecutive 0 must be >= 1",
		},
		{
			name: "target missing model",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error]
      consecutive: 2
      target: {provider: zhipu}
`,
			want: "escalation: target must name a provider and a model",
		},
		{
			name: "target protocol",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error]
      consecutive: 2
      target: {provider: zhipu, model: glm-5.3, protocol: openai}
`,
			want: "declare protocol: on the route target instead of the escalation target",
		},
		{
			name: "target unknown provider",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error]
      consecutive: 2
      target: nope/glm-5.3
`,
			want: `escalation: provider "nope" not defined`,
		},
		{
			name: "invalid dwell",
			doc: `route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error]
      consecutive: 2
      target: zhipu/glm-5.3
      dwell: not-a-duration
`,
			want: "dwell \"not-a-duration\" is not a valid duration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes("x", []byte(routePolicyBase+tc.doc))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRoutePolicyEscalationAcceptsAllBadSignals(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
route_policy:
  tier:
    bands:
      - when: {has_image: true}
        target: zhipu/glm-5.3
    escalation:
      bad_signals: [upstream_error, empty_ok, repeat_turn]
      consecutive: 2
      target: zhipu/glm-5.3
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	esc := cfg.RoutePolicies["tier"].Escalation
	if len(esc.BadSignals) != 3 {
		t.Fatalf("bad_signals = %v, want 3 entries", esc.BadSignals)
	}
}

func TestRoutePolicyGradesLoad(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
      - when: {estimated_tokens_min: 60000}
        target: zhipu/glm-5.3
    fallback: next_grade
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	policy := cfg.RoutePolicies["tier"]
	if !policy.HasGrades() {
		t.Fatal("expected grades to be loaded")
	}
	if policy.FallbackMode() != "next_grade" {
		t.Fatalf("fallback = %q, want next_grade", policy.FallbackMode())
	}
	if len(policy.Grades) != 2 {
		t.Fatalf("grades = %d, want 2", len(policy.Grades))
	}
	if len(policy.Grades["fast"]) != 1 || policy.Grades["fast"][0].Model != "glm-5.3-flash" {
		t.Fatalf("fast grade = %+v", policy.Grades["fast"])
	}
	if policy.Bands[0].Grade != "fast" {
		t.Fatalf("band 0 grade = %q, want fast", policy.Bands[0].Grade)
	}
}

func TestRoutePolicyGradesRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "grade and target both set",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
    bands:
      - when: {follow_up: true}
        grade: fast
        target: zhipu/glm-5.3
`,
			want: "set either grade or target, not both",
		},
		{
			name: "band references unknown grade",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
    bands:
      - when: {follow_up: true}
        grade: missing
`,
			want: "grade \"missing\" not declared in grades",
		},
		{
			name: "target not in any grade",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
    bands:
      - when: {follow_up: true}
        target: zhipu/glm-5.3
`,
			want: "does not belong to any grade",
		},
		{
			name: "ambiguous target in grades",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        target: zhipu/glm-5.3
`,
			// Rejected at grade validation now (before the band check): one
			// target in two grades makes grade grouping/next_grade/eval
			// pairing nondeterministic.
			want: "target zhipu/glm-5.3 appears in multiple grades",
		},
		{
			name: "target duplicated across grades without band reference",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash, zhipu/glm-5.3]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
`,
			want: "target zhipu/glm-5.3 appears in multiple grades",
		},
		{
			name: "grade target not in route",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-missing]
    bands:
      - when: {follow_up: true}
        grade: fast
`,
			want: "is not a target of route",
		},
		{
			name: "empty grade",
			doc: `route_policy:
  tier:
    grades:
      fast: []
    bands:
      - when: {follow_up: true}
        grade: fast
`,
			want: "grade \"fast\" must contain at least one target",
		},
		{
			name: "invalid fallback",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
    bands:
      - when: {follow_up: true}
        grade: fast
    fallback: all_of_them
`,
			want: "fallback \"all_of_them\" invalid",
		},
		{
			name: "grade used without grades",
			doc: `route_policy:
  tier:
    bands:
      - when: {follow_up: true}
        grade: fast
`,
			want: "grade used but route_policy \"tier\" has no grades declared",
		},
		{
			name: "escalation grade unknown",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
    bands:
      - when: {follow_up: true}
        grade: fast
    escalation:
      bad_signals: [upstream_error]
      consecutive: 2
      grade: missing
`,
			want: "escalation: grade \"missing\" not declared in grades",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes("x", []byte(routePolicyBase+tc.doc))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestRoutePolicyEvalLoads pins the eval block parsing and defaults.
func TestRoutePolicyEvalLoads(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(routePolicyBase+`
route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
    eval:
      sample_rate: 0.1
      pair: opposite
      judge: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	eval := cfg.RoutePolicies["tier"].Eval
	if eval == nil {
		t.Fatal("eval block not parsed")
	}
	if eval.SampleRateValue() != 0.1 {
		t.Fatalf("sample_rate = %v, want 0.1", eval.SampleRateValue())
	}
	if eval.PairMode() != "opposite" {
		t.Fatalf("pair = %q, want opposite", eval.PairMode())
	}
	if eval.Judge.Provider != "typesafe" || eval.Judge.Model != "jev-1.13.0" || eval.Judge.Protocol != "decisions" {
		t.Fatalf("judge = %+v", eval.Judge)
	}
}

// TestRoutePolicyEvalRejectsInvalidShapes covers the eval validation rules.
func TestRoutePolicyEvalRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "eval without grades",
			doc: `route_policy:
  tier:
    bands:
      - when: {follow_up: true}
        target: zhipu/glm-5.3
    eval:
      judge: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
`,
			want: `route_policy "tier" eval: requires grades to be configured`,
		},
		{
			name: "sample_rate too high",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
    eval:
      sample_rate: 0.9
      judge: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
`,
			want: `sample_rate 0.9 out of range [0, 0.5]`,
		},
		{
			name: "judge missing model",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
    eval:
      judge: {provider: typesafe, protocol: decisions}
`,
			want: `judge must name a provider and a model`,
		},
		{
			name: "judge provider unknown",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
    eval:
      judge: {provider: nope, model: jev-1.13.0, protocol: decisions}
`,
			want: `judge provider "nope" not defined`,
		},
		{
			name: "judge not decisions protocol",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
    eval:
      judge: {provider: zhipu, model: glm-5.3, protocol: openai}
`,
			want: `judge protocol must be decisions`,
		},
		{
			name: "pair grade unknown",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
      strong: [zhipu/glm-5.3]
    bands:
      - when: {follow_up: true}
        grade: fast
    eval:
      pair: grade:missing
      judge: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
`,
			want: `pair grade "missing" not declared`,
		},
		{
			name: "pair opposite needs two grades",
			doc: `route_policy:
  tier:
    grades:
      fast: [zhipu/glm-5.3-flash]
    bands:
      - when: {follow_up: true}
        grade: fast
    eval:
      judge: {provider: typesafe, model: jev-1.13.0, protocol: decisions}
`,
			want: `pair=opposite requires at least 2 grades`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes("x", []byte(routePolicyBase+tc.doc))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
