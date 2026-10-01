package routing

import (
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// TestDerivedRoute_ProtocolHintFilled: derived codex routes auto-declare
// protocol:"responses" (so an anthropic/chat client is converted, not left to
// send a body codex rejects); non-codex providers stay unset.
func TestDerivedRoute_ProtocolHintFilled(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x", Models: []string{"gpt-5.6"}},
		"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://y", Models: []string{"glm-5"}},
	}}
	derived := DeriveRoutesFrom(cfg)
	if got := derived["gpt-5.6"][0].Protocol; got != "responses" {
		t.Errorf("codex derived protocol = %q, want \"responses\"", got)
	}
	if got := derived["glm-5"][0].Protocol; got != "" {
		t.Errorf("zhipu derived protocol = %q, want unset", got)
	}
}

// TestConfigRoutingWarnings: the reasoning-replay marker fires precisely for a
// reasoning model behind a declared protocol conversion. A codex target WITHOUT
// protocol: does NOT warn — the forward path auto-resolves it to responses
// (resolvedBackendProto/ProtocolHint), so it converts without user action.
func TestConfigRoutingWarnings(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x"},
		"aqp":   {Provider: "aqp", OpenAIBaseURL: "https://y"},
	}}
	expanded := map[string][]configdomain.RouteTarget{
		"k2":        {{Provider: "aqp", Model: "kimi-k2-thinking", Protocol: "openai"}},
		"plain":     {{Provider: "aqp", Model: "glm-5", Protocol: "openai"}},
		"no-proto":  {{Provider: "codex", Model: "gpt-5.6"}},         // auto-resolves → no warning
		"reason-ok": {{Provider: "aqp", Model: "deepseek-reasoner"}}, // no conversion → no warning
		// responses targets preserve reasoning, so the "currently dropped"
		// message would be a false positive for them.
		"resp-think": {{Provider: "codex", Model: "kimi-k2-thinking", Protocol: "responses"}},
	}
	warns := ConfigRoutingWarnings(cfg, expanded)
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, `route "k2"`) || !strings.Contains(joined, "reasoning-required") {
		t.Errorf("missing reasoning marker, warns = %v", warns)
	}
	if strings.Contains(joined, `route "no-proto"`) {
		t.Errorf("codex auto-resolves (no protocol: needed) — must NOT warn, warns = %v", warns)
	}
	if strings.Contains(joined, `route "resp-think"`) {
		t.Errorf("responses targets preserve reasoning — must NOT warn, warns = %v", warns)
	}
	if strings.Contains(joined, `route "plain"`) || strings.Contains(joined, `route "reason-ok"`) {
		t.Errorf("false positive, warns = %v", warns)
	}
	if len(warns) != 1 {
		t.Errorf("len(warns) = %d, want 1 (only the k2 reasoning marker; codex auto-resolves): %v", len(warns), warns)
	}
}

// TestConfigRoutingWarnings_GradeBands: grade: references must not produce a
// "target not in route" false positive; target references in graded policies
// must resolve uniquely to a declared grade.
func TestConfigRoutingWarnings_GradeBands(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://x", Models: []string{"glm-5.3-flash", "glm-5.3"}},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"coding": {
				{Provider: "zhipu", Model: "glm-5.3-flash"},
				{Provider: "zhipu", Model: "glm-5.3"},
			},
		},
		RoutePolicies: map[string]configdomain.RoutePolicy{
			"coding": {
				Grades: map[string][]configdomain.RouteTarget{
					"fast":   {{Provider: "zhipu", Model: "glm-5.3-flash"}},
					"strong": {{Provider: "zhipu", Model: "glm-5.3"}},
				},
				Bands: []configdomain.RouteBand{
					{When: configdomain.BandWhen{FollowUp: boolp(true)}, Grade: "fast"},
					{When: configdomain.BandWhen{EstimatedTokensMin: int64p(60000)}, Target: configdomain.RouteTarget{Provider: "zhipu", Model: "glm-5.3"}},
				},
			},
		},
	}
	expanded := map[string][]configdomain.RouteTarget{
		"coding": {
			{Provider: "zhipu", Model: "glm-5.3-flash"},
			{Provider: "zhipu", Model: "glm-5.3"},
		},
	}
	warns := ConfigRoutingWarnings(cfg, expanded)
	joined := strings.Join(warns, "\n")
	if strings.Contains(joined, `band 0`) || strings.Contains(joined, `grade "fast"`) {
		t.Errorf("grade: band must not warn when grade is declared, warns = %v", warns)
	}
	if strings.Contains(joined, `band 1`) {
		t.Errorf("target band that uniquely maps to a grade must not warn, warns = %v", warns)
	}
}

// TestConfigRoutingWarnings_GradeBandUnknownGrade: a grade: reference to an
// undeclared grade produces a warning.
func TestConfigRoutingWarnings_GradeBandUnknownGrade(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://x", Models: []string{"glm-5.3-flash"}},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"coding": {{Provider: "zhipu", Model: "glm-5.3-flash"}},
		},
		RoutePolicies: map[string]configdomain.RoutePolicy{
			"coding": {
				Grades: map[string][]configdomain.RouteTarget{
					"fast": {{Provider: "zhipu", Model: "glm-5.3-flash"}},
				},
				Bands: []configdomain.RouteBand{
					{When: configdomain.BandWhen{FollowUp: boolp(true)}, Grade: "unknown"},
				},
			},
		},
	}
	expanded := map[string][]configdomain.RouteTarget{
		"coding": {{Provider: "zhipu", Model: "glm-5.3-flash"}},
	}
	warns := ConfigRoutingWarnings(cfg, expanded)
	if !containsStr(warns, `grade "unknown" is not declared`) {
		t.Errorf("missing unknown grade warning, warns = %v", warns)
	}
}

// TestConfigRoutingWarnings_NonGradedBandTargetNotServed: in a non-graded
// policy a band target must simply be served by the route — the provider
// existing is NOT enough (config-side band validation only checks provider
// presence), so a typo'd model surfaces at startup as exactly one
// "not served by this route" warning; a correctly served band target stays
// silent.
func TestConfigRoutingWarnings_NonGradedBandTargetNotServed(t *testing.T) {
	newCfg := func(bandTarget configdomain.RouteTarget) *configdomain.Config {
		return &configdomain.Config{
			Providers: map[string]configdomain.Provider{
				"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://x", Models: []string{"glm-5.3", "glm-5.3-air"}},
			},
			Routes: map[string][]configdomain.RouteTarget{
				"coding": {{Provider: "zhipu", Model: "glm-5.3"}},
			},
			RoutePolicies: map[string]configdomain.RoutePolicy{
				"coding": {
					Bands: []configdomain.RouteBand{
						{When: configdomain.BandWhen{FollowUp: boolp(true)}, Target: bandTarget},
					},
				},
			},
		}
	}
	expanded := map[string][]configdomain.RouteTarget{
		"coding": {{Provider: "zhipu", Model: "glm-5.3"}},
	}

	// Existing provider, model not on the route: exactly one band warning
	// naming the route, the band index and the unreachable target.
	warns := ConfigRoutingWarnings(newCfg(configdomain.RouteTarget{Provider: "zhipu", Model: "glm-5.3-air"}), expanded)
	if len(warns) != 1 ||
		!containsStr(warns, `route_policy "coding" band 0: target zhipu/glm-5.3-air is not served by this route`) {
		t.Errorf("warns = %v, want exactly the not-served band warning", warns)
	}

	// Band target the route serves: zero warnings.
	warns = ConfigRoutingWarnings(newCfg(configdomain.RouteTarget{Provider: "zhipu", Model: "glm-5.3"}), expanded)
	if len(warns) != 0 {
		t.Errorf("served band target warns = %v, want none", warns)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func boolp(b bool) *bool    { return &b }
func int64p(i int64) *int64 { return &i }
