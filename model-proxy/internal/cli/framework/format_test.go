package framework

import (
	"sort"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

func TestRouteNames(t *testing.T) {
	cfg := &configdomain.Config{
		Routes: map[string][]configdomain.RouteTarget{
			"glm-5.2":  {{Provider: "a", Model: "glm-5.2"}},
			"gpt-5.5":  {{Provider: "b", Model: "gpt-5.5"}},
			"deepseek": {{Provider: "c", Model: "deepseek"}},
		},
	}
	got := RouteNames(cfg)
	want := "deepseek, glm-5.2, gpt-5.5" // sorted
	if got != want {
		t.Errorf("RouteNames()=%q want %q (sorted)", got, want)
	}
	if got := RouteNames(&configdomain.Config{}); got != "" {
		t.Errorf("RouteNames(empty)=%q want empty", got)
	}
}

func TestProviderNames(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {},
			"aqp":   {},
			"codex": {},
		},
	}
	got := ProviderNames(cfg)
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

func TestPositional(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"simple", []string{"login", "codex"}, "login"},
		{"after --config value", []string{"--config", "x.yaml", "login"}, "login"},
		{"--config= form", []string{"--config=x.yaml", "login"}, "login"},
		{"-config single dash", []string{"-config", "x.yaml", "usage"}, "usage"},
		{"flag only", []string{"--foo", "--bar"}, ""},
		{"empty", []string{}, ""},
		{"positional after flag", []string{"--verbose", "models", "zhipu"}, "models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Positional(tc.args); got != tc.want {
				t.Errorf("Positional(%v)=%q want %q", tc.args, got, tc.want)
			}
		})
	}
}
