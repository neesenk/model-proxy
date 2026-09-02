package routing

import (
	"reflect"
	"testing"

	configdomain "model-proxy/internal/config"
)

// implicit_routes_test.go covers DeriveRoutesFrom / RouteTable /
// BuildExpandedRoutes (config-only route derivation: per-provider priority,
// model aliases, aggregation by exposed name, explicit-route override).

func TestDeriveRoutesFrom_AggregatesByExposedNameWithProviderPriority(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"kimi-code":  {Provider: "kimi-code", Models: []string{"k3", "kimi-for-coding"}, Priority: 1, Alias: map[string]string{"k3": "kimi-k3"}},
			"aqp":        {Provider: "aqp", Models: []string{"kimi-k3", "glm-5.3"}, Priority: 2},
			"volcengine": {Provider: "volcengine", Models: []string{"kimi-k3"}, Priority: 3},
		},
	}
	derived := DeriveRoutesFrom(cfg)

	// kimi-k3 aggregates three providers (kimi-code under its k3 alias); each
	// target keeps the REAL upstream model name and inherits its provider's
	// priority; ordering is (priority, provider).
	want := []configdomain.RouteTarget{
		{Provider: "kimi-code", Model: "k3", Priority: 1},
		{Provider: "aqp", Model: "kimi-k3", Priority: 2},
		{Provider: "volcengine", Model: "kimi-k3", Priority: 3},
	}
	if got := derived["kimi-k3"]; !reflect.DeepEqual(got, want) {
		t.Errorf("derived kimi-k3 = %+v, want %+v", got, want)
	}
	// Non-aliased models keep their own name.
	if got := derived["glm-5.3"]; len(got) != 1 || got[0].Provider != "aqp" || got[0].Model != "glm-5.3" {
		t.Errorf("derived glm-5.3 = %+v, want single aqp target", got)
	}
	if _, aliased := derived["k3"]; aliased {
		t.Error("aliased model k3 must not stay exposed under its real name")
	}
	if _, ok := derived["kimi-for-coding"]; !ok {
		t.Error("kimi-for-coding should be derived under its own name")
	}
}

func TestRouteTable_ExplicitRouteOverridesDerived(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp":   {Provider: "aqp", Models: []string{"foo"}, Priority: 2},
			"zhipu": {Provider: "zhipu", Models: []string{"foo", "bar"}, Priority: 1},
		},
		Routes: map[string][]configdomain.RouteTarget{
			// Explicit override wins wholesale for this name.
			"foo": {{Provider: "zhipu", Model: "foo"}},
			// An explicit name with no derived counterpart is kept as-is.
			"hard": {{Provider: "fusion", Model: "hard-coding"}},
		},
	}
	table := RouteTable(cfg)
	if got := table["foo"]; len(got) != 1 || got[0].Provider != "zhipu" {
		t.Errorf("explicit foo route should override the derived aggregation, got %+v", got)
	}
	// Explicit targets without their own priority inherit the provider's.
	if got := table["foo"]; len(got) != 1 || got[0].Priority != 1 {
		t.Errorf("explicit foo target should inherit zhipu priority 1, got %+v", got)
	}
	if got := table["bar"]; len(got) != 1 || got[0].Provider != "zhipu" || got[0].Priority != 1 {
		t.Errorf("derived bar = %+v, want single zhipu target with priority 1", got)
	}
	if got := table["hard"]; len(got) != 1 || got[0].Provider != "fusion" || got[0].Model != "hard-coding" {
		t.Errorf("explicit fusion route = %+v", got)
	}
}

func TestBuildExpandedRoutes_FansOutDerivedAndExplicit(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp":   {Provider: "aqp", Models: []string{"m1"}, Priority: 2},
			"zhipu": {Provider: "zhipu", Models: []string{"m1"}, Priority: 1},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m2": {{Provider: "aqp", Model: "m1"}},
		},
	}
	expand := func(t configdomain.RouteTarget) []configdomain.RouteTarget {
		if t.Provider == "zhipu" {
			return []configdomain.RouteTarget{{Provider: "zhipu", Model: t.Model, Priority: t.Priority}, {Provider: "zhipu#2", Model: t.Model, Priority: t.Priority}}
		}
		return []configdomain.RouteTarget{t}
	}
	out := BuildExpandedRoutes(cfg, DeriveRoutesFrom(cfg), expand)
	if got := out["m1"]; len(got) != 3 {
		t.Errorf("derived m1 should fan out to 3 targets, got %+v", got)
	}
	if got := out["m2"]; len(got) != 1 || got[0].Provider != "aqp" || got[0].Priority != 2 {
		t.Errorf("explicit m2 = %+v, want aqp target with inherited priority 2", got)
	}
}
