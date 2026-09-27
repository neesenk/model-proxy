package routing

import (
	"testing"

	configdomain "model-proxy/internal/config"
)

func ptrI64(v int64) *int64 { return &v }
func ptrBool(v bool) *bool  { return &v }

func band(w configdomain.BandWhen, provider, model string) configdomain.RouteBand {
	return configdomain.RouteBand{
		When:   w,
		Target: configdomain.RouteTarget{Provider: provider, Model: model},
	}
}

func TestPickBandFirstMatchWins(t *testing.T) {
	policy := configdomain.RoutePolicy{Bands: []configdomain.RouteBand{
		band(configdomain.BandWhen{FollowUp: ptrBool(true), EstimatedTokensMax: ptrI64(1000)}, "zhipu", "glm-flash"),
		band(configdomain.BandWhen{FollowUp: ptrBool(true)}, "zhipu", "glm-strong"),
	}}

	got, ok := PickBand(policy, Profile{FollowUp: true, EstimatedTokens: 500})
	if !ok || got.Model != "glm-flash" {
		t.Fatalf("first band should win: got %+v ok=%v", got, ok)
	}
	got, ok = PickBand(policy, Profile{FollowUp: true, EstimatedTokens: 5000})
	if !ok || got.Model != "glm-strong" {
		t.Fatalf("second band should win once the first fails its token cap: got %+v ok=%v", got, ok)
	}
}

func TestPickBandConditions(t *testing.T) {
	cases := []struct {
		name    string
		when    configdomain.BandWhen
		profile Profile
		want    bool
	}{
		{"empty when matches everything", configdomain.BandWhen{}, Profile{}, true},
		{"has_tools match", configdomain.BandWhen{HasTools: ptrBool(true)}, Profile{HasTools: true}, true},
		{"has_tools mismatch", configdomain.BandWhen{HasTools: ptrBool(true)}, Profile{HasTools: false}, false},
		{"has_image match", configdomain.BandWhen{HasImage: ptrBool(true)}, Profile{HasImage: true}, true},
		{"has_image mismatch", configdomain.BandWhen{HasImage: ptrBool(true)}, Profile{HasImage: false}, false},
		{"follow_up false matches a first turn", configdomain.BandWhen{FollowUp: ptrBool(false)}, Profile{FollowUp: false}, true},
		{"follow_up false rejects a later turn", configdomain.BandWhen{FollowUp: ptrBool(false)}, Profile{FollowUp: true}, false},
		{"token min inclusive", configdomain.BandWhen{EstimatedTokensMin: ptrI64(100)}, Profile{EstimatedTokens: 100}, true},
		{"token min below", configdomain.BandWhen{EstimatedTokensMin: ptrI64(100)}, Profile{EstimatedTokens: 99}, false},
		{"token max inclusive", configdomain.BandWhen{EstimatedTokensMax: ptrI64(100)}, Profile{EstimatedTokens: 100}, true},
		{"token max above", configdomain.BandWhen{EstimatedTokensMax: ptrI64(100)}, Profile{EstimatedTokens: 101}, false},
		{"conditions AND", configdomain.BandWhen{HasTools: ptrBool(true), EstimatedTokensMin: ptrI64(10)}, Profile{HasTools: true, EstimatedTokens: 9}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := configdomain.RoutePolicy{Bands: []configdomain.RouteBand{band(tc.when, "zhipu", "m")}}
			_, ok := PickBand(policy, tc.profile)
			if ok != tc.want {
				t.Fatalf("match = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestPickBandNoMatch(t *testing.T) {
	policy := configdomain.RoutePolicy{Bands: []configdomain.RouteBand{
		band(configdomain.BandWhen{FollowUp: ptrBool(true)}, "zhipu", "m"),
	}}
	if _, ok := PickBand(policy, Profile{FollowUp: false}); ok {
		t.Fatal("expected no match for a first-turn request")
	}
	if _, ok := PickBand(configdomain.RoutePolicy{}, Profile{}); ok {
		t.Fatal("expected no match for an empty policy")
	}
}

func TestPreferTargetMovesMatchToFront(t *testing.T) {
	ordered := []configdomain.RouteTarget{
		{Provider: "aqp", Model: "m1"},
		{Provider: "shopee", Model: "m2"},
		{Provider: "zhipu", Model: "m3"},
	}
	got := PreferTarget(ordered, configdomain.RouteTarget{Provider: "zhipu", Model: "m3"}, nil)
	want := []string{"zhipu/m3", "aqp/m1", "shopee/m2"}
	for i, target := range got {
		if target.Provider+"/"+target.Model != want[i] {
			t.Fatalf("order = %+v, want %v", got, want)
		}
	}
	if ordered[0].Provider != "aqp" {
		t.Fatalf("input order must not be mutated in place: %+v", ordered)
	}
}

func TestPreferTargetParentMatchForPooledVirtual(t *testing.T) {
	ordered := []configdomain.RouteTarget{
		{Provider: "aqp", Model: "m"},
		{Provider: "zhipu#0", Model: "m"},
	}
	parentOf := map[string]string{"zhipu#0": "zhipu"}
	got := PreferTarget(ordered, configdomain.RouteTarget{Provider: "zhipu", Model: "m"}, parentOf)
	if got[0].Provider != "zhipu#0" {
		t.Fatalf("pooled virtual target should be preferred by its parent name: %+v", got)
	}
}

func TestPreferTargetModelMustMatch(t *testing.T) {
	ordered := []configdomain.RouteTarget{
		{Provider: "zhipu", Model: "glm-5.3"},
		{Provider: "aqp", Model: "glm-5.3.flash"},
	}
	got := PreferTarget(ordered, configdomain.RouteTarget{Provider: "aqp", Model: "glm-5.3"}, nil)
	if got[0].Provider != "zhipu" {
		t.Fatalf("a same-provider different-model target must not match: %+v", got)
	}
}

func TestPreferTargetAbsentLeavesOrder(t *testing.T) {
	ordered := []configdomain.RouteTarget{{Provider: "aqp", Model: "m"}, {Provider: "zhipu", Model: "m"}}
	got := PreferTarget(ordered, configdomain.RouteTarget{Provider: "deepseek", Model: "m"}, nil)
	if len(got) != len(ordered) || got[0].Provider != "aqp" {
		t.Fatalf("absent target must leave the order unchanged: %+v", got)
	}
	if PreferTarget(nil, configdomain.RouteTarget{Provider: "zhipu", Model: "m"}, nil) != nil {
		t.Fatal("nil order stays nil")
	}
}

func TestSelectGradePrecedence(t *testing.T) {
	grades := map[string][]configdomain.RouteTarget{
		"fast":   {{Provider: "zhipu", Model: "glm-flash"}},
		"strong": {{Provider: "zhipu", Model: "glm-5.3"}},
	}
	policy := configdomain.RoutePolicy{
		Grades: grades,
		Bands: []configdomain.RouteBand{
			{When: configdomain.BandWhen{FollowUp: ptrBool(true)}, Grade: "fast"},
			{When: configdomain.BandWhen{HasImage: ptrBool(true)}, Target: configdomain.RouteTarget{Provider: "zhipu", Model: "glm-5.3"}},
		},
	}

	// latch wins over everything
	if g, ok := SelectGrade(policy, Profile{FollowUp: true}, "strong", "fast"); !ok || g != "strong" {
		t.Fatalf("latch should win: got %q ok=%v", g, ok)
	}
	// selector choice wins over bands
	if g, ok := SelectGrade(policy, Profile{FollowUp: true}, "", "strong"); !ok || g != "strong" {
		t.Fatalf("selector choice should win over band: got %q ok=%v", g, ok)
	}
	// band wins over static/default
	if g, ok := SelectGrade(policy, Profile{FollowUp: true}, "", ""); !ok || g != "fast" {
		t.Fatalf("band should select fast: got %q ok=%v", g, ok)
	}
	// target band resolves to grade
	if g, ok := SelectGrade(policy, Profile{HasImage: true}, "", ""); !ok || g != "strong" {
		t.Fatalf("target band should resolve to strong: got %q ok=%v", g, ok)
	}
	// no match falls back
	if g, ok := SelectGrade(policy, Profile{}, "", ""); ok {
		t.Fatalf("expected no grade selection, got %q", g)
	}
	// unknown latch/choice ignored
	if g, ok := SelectGrade(policy, Profile{FollowUp: true}, "missing", "also-missing"); !ok || g != "fast" {
		t.Fatalf("unknown latch/choice should be ignored, band should win: got %q ok=%v", g, ok)
	}
}

func TestProfileRequestFollowUp(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"first turn", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
		{"anthropic follow-up", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`, true},
		{"spaced json", `{"messages": [{"role": "assistant", "content": "yo"}]}`, true},
		{"responses input", `{"input":[{"role":"assistant","content":"yo"}]}`, true},
		{"empty body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProfileRequest([]byte(tc.body)).FollowUp; got != tc.want {
				t.Fatalf("FollowUp = %v, want %v", got, tc.want)
			}
		})
	}
}
