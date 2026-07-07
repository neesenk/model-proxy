package main

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// format_test.go covers the pure helper functions in main.go / models.go /
// clients.go via table-driven tests. These are 0% in the baseline; each has
// small, well-defined branch sets.

func TestFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		secs int
		want string
	}{
		{-5, "—"},
		{0, "—"},
		{30, "0m"},      // < 1h → minutes only (30s truncates to 0m)
		{90, "1m"},      // 1m30s → 1m
		{3700, "1h1m"},  // 1h1m40s
		{7200, "2h0m"},  // 2h
		{90000, "1d1h"}, // 25h
		{86400, "1d0h"}, // exactly 1 day
		{3 * 86400, "3d0h"},
	} {
		if got := formatDuration(tc.secs); got != tc.want {
			t.Errorf("formatDuration(%d)=%q want %q", tc.secs, got, tc.want)
		}
	}
}

func TestFormatWithCommas(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-1234567, "-1,234,567"},
		{-999, "-999"},
	} {
		if got := formatWithCommas(tc.n); got != tc.want {
			t.Errorf("formatWithCommas(%d)=%q want %q", tc.n, got, tc.want)
		}
	}
}

func TestFormatCredits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"330.258", "330"},
		{"22500", "22,500"},
		{"0.9", "0"},
		{"not-a-number", "0"},
		{"9999999", "9,999,999"},
	} {
		if got := formatCredits(tc.in); got != tc.want {
			t.Errorf("formatCredits(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestMoney(t *testing.T) {
	if got := money(12.5); got != "$12.50" {
		t.Errorf("money(12.5)=%q want $12.50", got)
	}
	if got := money(0); got != "$0.00" {
		t.Errorf("money(0)=%q want $0.00", got)
	}
}

func TestProgressBar(t *testing.T) {
	// Width clamping: width < 4 becomes 4.
	bar := progressBar(50, 2)
	if !strings.Contains(bar, "[") || !strings.Contains(bar, "]") {
		t.Errorf("progressBar(width=2)=%q missing brackets", bar)
	}
	// 0% used → all empty.
	bar = progressBar(0, 10)
	if strings.Count(bar, "█") != 0 {
		t.Errorf("progressBar(0%%)=%q want no filled cells", bar)
	}
	// 100% used → all filled.
	bar = progressBar(100, 10)
	if strings.Count(bar, "█") != 10 {
		t.Errorf("progressBar(100%%)=%q want 10 filled cells", bar)
	}
	// >100% clamps to full.
	bar = progressBar(200, 10)
	if strings.Count(bar, "█") != 10 {
		t.Errorf("progressBar(200%%)=%q want 10 (clamped)", bar)
	}
}

func TestPad(t *testing.T) {
	if got := pad("ab", 5); got != "ab   " {
		t.Errorf("pad(%q,5)=%q want %q", "ab", got, "ab   ")
	}
	if got := pad("abcde", 3); got != "abcde" {
		t.Errorf("pad(%q,3)=%q want %q (no truncation)", "abcde", got, "abcde")
	}
	if got := pad("", 3); got != "   " {
		t.Errorf("pad(%q,3)=%q want %q", "", got, "   ")
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName("glm-5.2"); got != "glm-5.2" {
		t.Errorf("displayName(glm-5.2)=%q want glm-5.2", got)
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
	got := routeNames(cfg)
	want := "deepseek, glm-5.2, gpt-5.5" // sorted
	if got != want {
		t.Errorf("routeNames()=%q want %q (sorted)", got, want)
	}
	// Empty routes → empty string.
	if got := routeNames(&Config{}); got != "" {
		t.Errorf("routeNames(empty)=%q want empty", got)
	}
}

func TestProviderNames(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu":   {},
			"compass": {},
			"codex":   {},
		},
	}
	got := providerNames(cfg)
	// providerNames does NOT sort (unlike routeNames); collect + sort for stable check.
	parts := strings.Split(got, ", ")
	sort.Strings(parts)
	want := []string{"codex", "compass", "zhipu"}
	if len(parts) != len(want) {
		t.Fatalf("providerNames()=%q want 3 providers", got)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("providerNames() parts=%v want %v", parts, want)
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
			if got := positional(tc.args); got != tc.want {
				t.Errorf("positional(%v)=%q want %q", tc.args, got, tc.want)
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
			got := nonFlagArgs(tc.args)
			if !equalSlices(got, tc.want) {
				t.Errorf("nonFlagArgs(%v)=%v want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestTakesProvider(t *testing.T) {
	for _, cmd := range []string{"login", "logout", "usage"} {
		if !takesProvider(cmd) {
			t.Errorf("takesProvider(%q)=false want true", cmd)
		}
	}
	for _, cmd := range []string{"models", "serve", "schedule", "doctor", "config", "takeover", "help", ""} {
		if takesProvider(cmd) {
			t.Errorf("takesProvider(%q)=true want false", cmd)
		}
	}
}

func TestFormatResetAt(t *testing.T) {
	// Today's date → HH:MM only.
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, now.Location())
	got := formatResetAt(today.UnixMilli())
	if !strings.Contains(got, "14:30") || strings.Contains(got, "-") {
		t.Errorf("formatResetAt(today 14:30)=%q want 14:30 (no date)", got)
	}
	// A different date → MM-DD HH:MM.
	other := time.Date(now.Year(), now.Month(), now.Day()+5, 9, 5, 0, 0, now.Location())
	got = formatResetAt(other.UnixMilli())
	if !strings.Contains(got, "09:05") {
		t.Errorf("formatResetAt(other day)=%q want MM-DD 09:05", got)
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
