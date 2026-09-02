package config

import (
	"sort"
	"strings"
	"testing"
)

func TestRouteNames(t *testing.T) {
	cfg := &Config{
		Routes: map[string][]RouteTarget{
			"glm-5.2":  {{Provider: "a", Model: "glm-5.2"}},
			"gpt-5.5":  {{Provider: "b", Model: "gpt-5.5"}},
			"deepseek": {{Provider: "c", Model: "deepseek"}},
		},
	}
	got := cfg.RouteNames()
	want := "deepseek, glm-5.2, gpt-5.5" // sorted
	if got != want {
		t.Errorf("RouteNames()=%q want %q (sorted)", got, want)
	}
	if got := (&Config{}).RouteNames(); got != "" {
		t.Errorf("RouteNames(empty)=%q want empty", got)
	}
}

func TestProviderNames(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {},
			"aqp":   {},
			"codex": {},
		},
	}
	got := cfg.ProviderNames()
	parts := strings.Split(got, ", ")
	sort.Strings(parts)
	want := []string{"aqp", "codex", "zhipu"}
	if len(parts) != len(want) {
		t.Fatalf("ProviderNames()=%q want 3 providers", got)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("ProviderNames() parts=%v want %v", parts, want)
		}
	}
}
