package main

import (
	clicmd "model-proxy/internal/cli"
	cliframework "model-proxy/internal/cli/framework"
	climodels "model-proxy/internal/cli/models"
	"model-proxy/internal/takeover"
	"model-proxy/provider"
	"sort"
	"strings"
	"testing"
	"time"
)

// format_test.go covers pure display helpers that remain in the composition
// root. Provider-owned formatting primitives live in provider/display_test.go.

func TestProgressBar(t *testing.T) {
	// Width clamping: width < 4 becomes 4.
	bar := provider.ProgressBar(50, 2)
	if !strings.Contains(bar, "[") || !strings.Contains(bar, "]") {
		t.Errorf("provider.ProgressBar(width=2)=%q missing brackets", bar)
	}
	// 0% used → all empty.
	bar = provider.ProgressBar(0, 10)
	if strings.Count(bar, "█") != 0 {
		t.Errorf("provider.ProgressBar(0%%)=%q want no filled cells", bar)
	}
	// 100% used → all filled.
	bar = provider.ProgressBar(100, 10)
	if strings.Count(bar, "█") != 10 {
		t.Errorf("provider.ProgressBar(100%%)=%q want 10 filled cells", bar)
	}
	// >100% clamps to full.
	bar = provider.ProgressBar(200, 10)
	if strings.Count(bar, "█") != 10 {
		t.Errorf("provider.ProgressBar(200%%)=%q want 10 (clamped)", bar)
	}
}

func TestPad(t *testing.T) {
	if got := provider.Pad("ab", 5); got != "ab   " {
		t.Errorf("provider.Pad(%q,5)=%q want %q", "ab", got, "ab   ")
	}
	if got := provider.Pad("abcde", 3); got != "abcde" {
		t.Errorf("provider.Pad(%q,3)=%q want %q (no truncation)", "abcde", got, "abcde")
	}
	if got := provider.Pad("", 3); got != "   " {
		t.Errorf("provider.Pad(%q,3)=%q want %q", "", got, "   ")
	}
}

func TestDisplayName(t *testing.T) {
	if got := takeover.DisplayName("glm-5.2"); got != "glm-5.2" {
		t.Errorf("takeover.DisplayName(glm-5.2)=%q want glm-5.2", got)
	}
}

func TestRouteNames(t *testing.T) {
	cfg := &Config{
		Routes: map[string][]RouteTarget{
			"glm-5.2":  {{Provider: "a", Model: "glm-5.2"}},
			"gpt-5.5":  {{Provider: "b", Model: "gpt-5.5"}},
			"deepseek": {{Provider: "c", Model: "deepseek"}},
		},
	}
	got := cliframework.RouteNames(cfg)
	want := "deepseek, glm-5.2, gpt-5.5" // sorted
	if got != want {
		t.Errorf("cliframework.RouteNames()=%q want %q (sorted)", got, want)
	}
	// Empty routes → empty string.
	if got := cliframework.RouteNames(&Config{}); got != "" {
		t.Errorf("cliframework.RouteNames(empty)=%q want empty", got)
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
	got := cliframework.ProviderNames(cfg)
	// providerNames does NOT sort (unlike routeNames); collect + sort for stable check.
	parts := strings.Split(got, ", ")
	sort.Strings(parts)
	want := []string{"aqp", "codex", "zhipu"}
	if len(parts) != len(want) {
		t.Fatalf("cliframework.ProviderNames()=%q want 3 providers", got)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("cliframework.ProviderNames() parts=%v want %v", parts, want)
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
			if got := cliframework.Positional(tc.args); got != tc.want {
				t.Errorf("cliframework.Positional(%v)=%q want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestNonFlagArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"simple", []string{"models", "zhipu"}, []string{"models", "zhipu"}},
		{"--config value skipped", []string{"--config", "x.yaml", "models"}, []string{"models"}},
		{"--config= skipped", []string{"--config=x.yaml", "models"}, []string{"models"}},
		{"flags skipped", []string{"--verbose", "models", "--refresh"}, []string{"models"}},
		{"empty", []string{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := climodels.NonFlagArgs(tc.args)
			if !equalSlices(got, tc.want) {
				t.Errorf("climodels.NonFlagArgs(%v)=%v want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestTakesProvider(t *testing.T) {
	for _, cmd := range []string{"login", "logout", "usage"} {
		if !clicmd.TakesProvider(cmd) {
			t.Errorf("clicmd.TakesProvider(%q)=false want true", cmd)
		}
	}
	for _, cmd := range []string{"models", "serve", "schedule", "doctor", "config", "takeover", "help", ""} {
		if clicmd.TakesProvider(cmd) {
			t.Errorf("clicmd.TakesProvider(%q)=true want false", cmd)
		}
	}
}

func TestFormatResetAt(t *testing.T) {
	// Today's date → HH:MM only.
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, now.Location())
	got := provider.FormatResetAt(today.UnixMilli())
	if !strings.Contains(got, "14:30") || strings.Contains(got, "-") {
		t.Errorf("provider.FormatResetAt(today 14:30)=%q want 14:30 (no date)", got)
	}
	// A different date → MM-DD HH:MM.
	other := time.Date(now.Year(), now.Month(), now.Day()+5, 9, 5, 0, 0, now.Location())
	got = provider.FormatResetAt(other.UnixMilli())
	if !strings.Contains(got, "09:05") {
		t.Errorf("provider.FormatResetAt(other day)=%q want MM-DD 09:05", got)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
