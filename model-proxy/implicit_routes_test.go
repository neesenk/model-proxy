package main

import (
	"strings"
	"testing"
)

// implicit_routes_test.go covers synthesizeImplicitRoutesFrom (login-aware
// auto-routing for models not in routes). login status is injected directly so
// the pure core is tested without credential files.

func TestSynthesizeImplicitRoutes_SingleProviderAutoRoute(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", Models: []string{"glm-4.6", "glm-5.2"}},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}}, // explicit
		},
	}
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"zhipu": true})

	// glm-4.6: not routed, single logged-in provider → implicit route, no warning.
	got, ok := implicit["glm-4.6"]
	if !ok {
		t.Fatal("glm-4.6 should get an implicit route")
	}
	if got.Provider != "zhipu" || got.Model != "glm-4.6" {
		t.Errorf("implicit glm-4.6 = %+v, want provider=zhipu model=glm-4.6", got)
	}
	// glm-5.2 is explicitly routed → no implicit.
	if _, dup := implicit["glm-5.2"]; dup {
		t.Error("explicitly-routed glm-5.2 should not get an implicit route")
	}
	if len(warnings) != 0 {
		t.Errorf("single-provider implicit route should not warn: %v", warnings)
	}
}

func TestSynthesizeImplicitRoutes_MultipleProvidersWarnsAndPicksFirst(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":   {Provider: "aqp", Models: []string{"foo"}},     // alphabetically first
			"zhipu": {Provider: "zhipu", Models: []string{"foo"}},
		},
	}
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"aqp": true, "zhipu": true})

	got, ok := implicit["foo"]
	if !ok || got.Provider != "aqp" {
		t.Errorf("foo should auto-route to alphabetically-first 'aqp', got %+v ok=%v", got, ok)
	}
	if len(warnings) != 1 {
		t.Fatalf("want 1 ambiguity warning, got %d: %v", len(warnings), warnings)
	}
	w := warnings[0]
	if !strings.Contains(w, "foo") || !strings.Contains(w, "aqp") || !strings.Contains(w, "zhipu") {
		t.Errorf("warning should name model + both providers: %q", w)
	}
}

func TestSynthesizeImplicitRoutes_SkipsNotLoggedIn(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", Models: []string{"glm-4.6"}},
		},
	}
	// zhipu NOT logged in → no implicit route, no warning.
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{})
	if len(implicit) != 0 {
		t.Errorf("no logged-in providers → no implicit routes, got %v", implicit)
	}
	if len(warnings) != 0 {
		t.Errorf("no logged-in providers → no warnings, got %v", warnings)
	}
}

func TestSynthesizeImplicitRoutes_PrefersLoggedInAmongMultiple(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":   {Provider: "aqp", Models: []string{"foo"}},     // not logged in
			"zhipu": {Provider: "zhipu", Models: []string{"foo"}},   // logged in
		},
	}
	// Only zhipu logged in → route to zhipu, single candidate → no warning.
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"zhipu": true})
	got, ok := implicit["foo"]
	if !ok || got.Provider != "zhipu" {
		t.Errorf("foo should route to the only logged-in provider zhipu, got %+v ok=%v", got, ok)
	}
	if len(warnings) != 0 {
		t.Errorf("single logged-in candidate → no warning, got %v", warnings)
	}
}
