package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// config_extra_test.go covers the validate error branches and Scheduling /
// expandPath / PeakConfig edges not exercised by config_test.go.

func TestValidate_NoProviders(t *testing.T) {
	err := (&Config{Listen: "127.0.0.1:1"}).validate()
	if err == nil || !strings.Contains(err.Error(), "no providers") {
		t.Errorf("no-providers: err=%v", err)
	}
}

func TestValidate_UsageURLInvalid(t *testing.T) {
	err := (&Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"x": {OpenAIBaseURL: "https://x", Provider: "zhipu", UsageURL: "not-a-url"},
		},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid URL") {
		t.Errorf("usage_url invalid: err=%v", err)
	}
}

func TestValidate_PeakHoursMalformed(t *testing.T) {
	err := (&Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"x": {OpenAIBaseURL: "https://x", Provider: "zhipu", PeakHours: PeakConfig{{Window: "not-a-range"}}},
		},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("peak malformed: err=%v", err)
	}
}

func TestValidate_PeakHoursZeroWidth(t *testing.T) {
	err := (&Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"x": {OpenAIBaseURL: "https://x", Provider: "zhipu", PeakHours: PeakConfig{{Window: "09:00-09:00"}}},
		},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "zero-width") {
		t.Errorf("peak zero-width: err=%v", err)
	}
}

func TestValidate_PeakHoursNegativeMultiplier(t *testing.T) {
	err := (&Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"x": {OpenAIBaseURL: "https://x", Provider: "zhipu", PeakHours: PeakConfig{{Window: "09:00-18:00", Multiplier: -1}}},
		},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "multiplier") {
		t.Errorf("peak negative multiplier: err=%v", err)
	}
}

func TestValidate_BillingInvalid(t *testing.T) {
	err := (&Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"x": {OpenAIBaseURL: "https://x", Provider: "zhipu", Billing: "free"},
		},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Errorf("billing invalid: err=%v", err)
	}
}

func TestValidate_RouteNoTargets(t *testing.T) {
	err := (&Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"x": {OpenAIBaseURL: "https://x", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"m": {}},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "no targets") {
		t.Errorf("route no targets: err=%v", err)
	}
}

func TestValidate_RouteTargetEmptyProvider(t *testing.T) {
	err := (&Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"x": {OpenAIBaseURL: "https://x", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "", Model: "m"}}},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "provider is empty") {
		t.Errorf("route empty provider: err=%v", err)
	}
}

func TestValidate_RouteTargetEmptyModel(t *testing.T) {
	err := (&Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"x": {OpenAIBaseURL: "https://x", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "x", Model: ""}}},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "model is empty") {
		t.Errorf("route empty model: err=%v", err)
	}
}

func TestValidate_ClaudeMappingEmptyTarget(t *testing.T) {
	err := (&Config{
		Listen:        "127.0.0.1:1",
		Providers:     map[string]Provider{"x": {OpenAIBaseURL: "https://x", Provider: "zhipu"}},
		Routes:        map[string][]RouteTarget{"m": {{Provider: "x", Model: "m"}}},
		ClaudeMapping: map[string]string{"claude-x": ""},
	}).validate()
	if err == nil || !strings.Contains(err.Error(), "target is empty") {
		t.Errorf("claude_mapping empty target: err=%v", err)
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	// A minimal valid config returns nil.
	err := (&Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"x": {OpenAIBaseURL: "https://x", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "x", Model: "m", Priority: 1}}},
	}).validate()
	if err != nil {
		t.Errorf("valid config: want nil, got %v", err)
	}
}

// --- Scheduling default + invalid-duration fallbacks ---

func TestScheduling_DefaultsAndInvalid(t *testing.T) {
	var s Scheduling // all zero
	if s.threshold() != 3 {
		t.Errorf("default threshold=%d want 3", s.threshold())
	}
	if s.cooldown() != 10*time.Minute {
		t.Errorf("default cooldown=%v want 10m", s.cooldown())
	}
	if s.rateBackoff() != 60*time.Second {
		t.Errorf("default rateBackoff=%v want 60s", s.rateBackoff())
	}
	if s.timeout() != 30*time.Second {
		t.Errorf("default timeout=%v want 30s", s.timeout())
	}
	if s.dwell() != 10*time.Minute {
		t.Errorf("default dwell=%v want 10m", s.dwell())
	}
	if s.pollInterval() != 5*time.Minute {
		t.Errorf("default pollInterval=%v want 5m", s.pollInterval())
	}
	if s.switchMargin() != 0.15 {
		t.Errorf("default switchMargin=%v want 0.15", s.switchMargin())
	}

	// Invalid duration strings fall back to defaults.
	bad := Scheduling{CircuitCooldown: "bad", RateLimitBackoff: "bad", UpstreamTimeout: "bad", StickyDwell: "bad", QuotaPollInterval: "bad"}
	if bad.cooldown() != 10*time.Minute {
		t.Errorf("invalid cooldown fallback=%v want 10m", bad.cooldown())
	}
	if bad.rateBackoff() != 60*time.Second {
		t.Errorf("invalid rateBackoff fallback=%v want 60s", bad.rateBackoff())
	}
	if bad.timeout() != 30*time.Second {
		t.Errorf("invalid timeout fallback=%v want 30s", bad.timeout())
	}
	if bad.dwell() != 10*time.Minute {
		t.Errorf("invalid dwell fallback=%v want 10m", bad.dwell())
	}
	if bad.pollInterval() != 5*time.Minute {
		t.Errorf("invalid pollInterval fallback=%v want 5m", bad.pollInterval())
	}

	// Explicit values are honored.
	good := Scheduling{CircuitThreshold: 5, CircuitCooldown: "2m", RateLimitBackoff: "10s", UpstreamTimeout: "5s", StickyDwell: "3m", QuotaPollInterval: "1m", QuotaSwitchMargin: 25}
	if good.threshold() != 5 {
		t.Errorf("explicit threshold=%d want 5", good.threshold())
	}
	if good.cooldown() != 2*time.Minute {
		t.Errorf("explicit cooldown=%v want 2m", good.cooldown())
	}
	if good.switchMargin() != 0.25 {
		t.Errorf("explicit switchMargin=%v want 0.25", good.switchMargin())
	}
}

// --- expandPath: ~, env:, plain, empty ---

func TestExpandPath(t *testing.T) {
	t.Setenv("MP_TEST_PATH", "/from/env")
	if got := expandPath("env:MP_TEST_PATH"); got != "/from/env" {
		t.Errorf("expandPath(env:)=%q want /from/env", got)
	}
	if got := expandPath(""); got != "" {
		t.Errorf("expandPath(empty)=%q want empty", got)
	}
	// Plain path with no prefix passes through.
	if got := expandPath("/abs/path"); got != "/abs/path" {
		t.Errorf("expandPath(/abs/path)=%q want /abs/path", got)
	}
	// ~/ expands to home.
	home := homeDir()
	want := filepath.Join(home, "foo")
	if got := expandPath("~/foo"); got != want {
		t.Errorf("expandPath(~/foo)=%q want %q", got, want)
	}
}

// --- PeakConfig unmarshal: list of strings shape ---

func TestPeakConfig_UnmarshalListOfStrings(t *testing.T) {
	var pc PeakConfig
	yaml.Unmarshal([]byte(`["09:00-12:00", "14:00-18:00"]`), &pc)
	if len(pc) != 2 {
		t.Fatalf("list-of-strings: len=%d want 2", len(pc))
	}
	if pc[0].Window != "09:00-12:00" {
		t.Errorf("pc[0].Window=%q", pc[0].Window)
	}
}

func TestPeakConfig_UnmarshalInvalid(t *testing.T) {
	// A sequence of non-string, non-map elements (integers) must error or
	// produce empty segments — it must NOT silently produce bogus windows.
	var pc PeakConfig
	if err := yaml.Unmarshal([]byte(`[1, 2, 3]`), &pc); err == nil {
		// If no error, every segment must have an empty Window (the int was
		// skipped). If any has a non-empty Window, that's a silent data corruption.
		for i, seg := range pc {
			if seg.Window != "" {
				t.Errorf("segment %d has Window=%q from an integer element (silent corruption)", i, seg.Window)
			}
		}
	}
}

// --- peakMultiplier: wrap-around window (e.g. 22:00-02:00) ---

func TestPeakMultiplier_WrapAround(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{{Window: "22:00-02:00", Multiplier: 3}}}
	// 23:00 is inside the wrap-around window.
	late := timeAt(t, 23, 0)
	if got := p.peakMultiplier(late); got != 3 {
		t.Errorf("wrap 23:00 multiplier=%v want 3", got)
	}
	// 01:00 is also inside (after midnight).
	early := timeAt(t, 1, 0)
	if got := p.peakMultiplier(early); got != 3 {
		t.Errorf("wrap 01:00 multiplier=%v want 3", got)
	}
	// 12:00 is outside.
	noon := timeAt(t, 12, 0)
	if got := p.peakMultiplier(noon); got != 1 {
		t.Errorf("noon multiplier=%v want 1 (no peak)", got)
	}
	// Wrap window edges: start inclusive, end exclusive.
	if got := p.peakMultiplier(timeAt(t, 22, 0)); got != 3 {
		t.Errorf("wrap 22:00 (start, inclusive) multiplier=%v want 3", got)
	}
	if got := p.peakMultiplier(timeAt(t, 2, 0)); got != 1 {
		t.Errorf("wrap 02:00 (end, exclusive) multiplier=%v want 1", got)
	}
}

// --- peakMultiplier: default multiplier when 0/unset on the segment ---

func TestPeakMultiplier_DefaultMultiplier(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{{Window: "09:00-18:00"}}} // Multiplier 0 → default
	inside := timeAt(t, 12, 0)
	if got := p.peakMultiplier(inside); got != defaultPeakMultiplier {
		t.Errorf("default multiplier=%v want %v", got, defaultPeakMultiplier)
	}
}

// --- peakMultiplier: malformed window is skipped (no panic, returns 1) ---

func TestPeakMultiplier_MalformedSkipped(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{{Window: "bad"}, {Window: "10:00-11:00", Multiplier: 2}}}
	if got := p.peakMultiplier(timeAt(t, 10, 30)); got != 2 {
		t.Errorf("malformed-then-valid: multiplier=%v want 2 (malformed skipped)", got)
	}
}

func timeAt(t *testing.T, hour, min int) time.Time {
	t.Helper()
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
}
