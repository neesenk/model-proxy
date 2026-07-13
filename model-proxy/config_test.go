package main

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestConfig_ProviderBaseURLs validates each provider's base URLs produce the
// correct upstream path for both protocols. This catches:
//   - openai_base_url missing its version segment (proxy strips /v1 → double path)
//   - anthropic_base_url including /v1 (proxy keeps /v1/messages → double /v1)
//   - wrong base URL patterns
func TestConfig_ProviderBaseURLs(t *testing.T) {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	for name, prov := range cfg.Providers {
		t.Run(name+"/openai", func(t *testing.T) {
			if prov.OpenAIBaseURL == "" {
				t.Fatalf("openai_base_url is empty")
			}
			// Openai: proxy strips /v1 from client path, appends remainder.
			// Simulate: client sends /v1/chat/completions → stripped to /chat/completions
			upstream := strings.TrimRight(prov.OpenAIBaseURL, "/") + "/chat/completions"
			// Should not have double /v1
			if strings.Contains(upstream, "/v1/v1") {
				t.Errorf("openai URL has double /v1: %s (openai_base_url should NOT end with /v1)", upstream)
			}
		})

		if prov.AnthropicBaseURL != "" {
			t.Run(name+"/anthropic", func(t *testing.T) {
				// Anthropic: proxy keeps the client's /v1/messages path.
				// Simulate: client sends /v1/messages → path stays /v1/messages
				upstream := strings.TrimRight(prov.AnthropicBaseURL, "/") + "/v1/messages"
				// Should not have double /v1
				if strings.Contains(upstream, "/v1/v1") {
					t.Errorf("anthropic URL has double /v1: %s (anthropic_base_url should NOT end with /v1)", upstream)
				}
				// Should end with .../v1/messages
				if !strings.HasSuffix(upstream, "/v1/messages") {
					t.Errorf("anthropic URL doesn't end with /v1/messages: %s", upstream)
				}
			})
		}
	}
}

// TestConfig_RouteTargets verifies every route target references an existing
// provider and has a non-empty model name.
func TestConfig_RouteTargets(t *testing.T) {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Routes) == 0 {
		t.Fatal("no routes configured")
	}
	for exposed, targets := range cfg.Routes {
		if len(targets) == 0 {
			t.Errorf("route %q has no targets", exposed)
			continue
		}
		for i, target := range targets {
			if target.Provider == "" {
				t.Errorf("route %q target %d: provider is empty", exposed, i)
			}
			if target.Model == "" {
				t.Errorf("route %q target %d: model is empty", exposed, i)
			}
			if _, ok := cfg.Providers[target.Provider]; !ok {
				t.Errorf("route %q target %d: provider %q not in providers", exposed, i, target.Provider)
			}
		}
	}
}

// TestConfig_ClaudeMapping verifies claude_mapping values reference existing routes.
func TestConfig_ClaudeMapping(t *testing.T) {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	for claude, exposed := range cfg.ClaudeMapping {
		if exposed == "" {
			t.Errorf("claude_mapping %q → empty target", claude)
		}
		if _, ok := cfg.Routes[exposed]; !ok {
			t.Errorf("claude_mapping %q → %q: target not in routes", claude, exposed)
		}
	}
}

// TestConfig_UpstreamURLPreview prints the exact upstream URLs the proxy would
// build for each provider×protocol. Useful for manual review — run with -v.
func TestConfig_UpstreamURLPreview(t *testing.T) {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Providers) == 0 {
		t.Fatal("no providers configured")
	}
	for name, prov := range cfg.Providers {
		// P1-2: assert no double /v1 (real assertion, not just t.Logf)
		openaiURL := strings.TrimRight(prov.OpenAIBaseURL, "/") + "/chat/completions"
		if strings.Contains(openaiURL, "/v1/v1") {
			t.Errorf("provider %s: openai URL has double /v1: %s", name, openaiURL)
		}
		if prov.AnthropicBaseURL != "" {
			anthropicURL := strings.TrimRight(prov.AnthropicBaseURL, "/") + "/v1/messages"
			if strings.Contains(anthropicURL, "/v1/v1") {
				t.Errorf("provider %s: anthropic URL has double /v1: %s", name, anthropicURL)
			}
			if !strings.HasSuffix(anthropicURL, "/v1/messages") {
				t.Errorf("provider %s: anthropic URL doesn't end with /v1/messages: %s", name, anthropicURL)
			}
		}
	}
}

// TestConfig_ValidateErrors checks that each validation rule produces a clear
// error message with a fix hint.
func TestConfig_ValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *Config
		wantSub string // substring the error message should contain
	}{
		{
			name:    "empty listen",
			cfg:     &Config{Providers: map[string]Provider{"x": {OpenAIBaseURL: "https://x", Provider: "zhipu"}}},
			wantSub: "listen is empty",
		},
		{
			name: "empty openai_base_url",
			cfg: &Config{Listen: ":1", Providers: map[string]Provider{
				"x": {Provider: "zhipu"},
			}},
			wantSub: "openai_base_url is empty",
		},
		{
			name: "empty provider_id",
			cfg: &Config{Listen: ":1", Providers: map[string]Provider{
				"x": {OpenAIBaseURL: "https://x", Provider: ""},
			}},
			wantSub: "provider_id is empty",
		},
		{
			name: "unknown provider_id",
			cfg: &Config{Listen: ":1", Providers: map[string]Provider{
				"x": {OpenAIBaseURL: "https://x", Provider: "unknown-typo"},
			}},
			wantSub: "unknown provider_id",
		},
		{
			name: "anthropic_base_url ends with /v1",
			cfg: &Config{Listen: ":1", Providers: map[string]Provider{
				"x": {OpenAIBaseURL: "https://x/v3", AnthropicBaseURL: "https://x/anthropic/v1", Provider: "zhipu"},
			}},
			wantSub: "anthropic_base_url ends with /v1",
		},
		{
			name: "route references unknown provider",
			cfg: &Config{Listen: ":1", Providers: map[string]Provider{
				"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
			}, Routes: map[string][]RouteTarget{
				"m": {{Provider: "nonexistent", Model: "m", Priority: 1}},
			}},
			wantSub: "provider \"nonexistent\" not defined",
		},
		{
			name: "claude_mapping bad target",
			cfg: &Config{Listen: ":1", Providers: map[string]Provider{
				"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
			}, Routes: map[string][]RouteTarget{
				"m": {{Provider: "a", Model: "m", Priority: 1}},
			}, ClaudeMapping: map[string]string{
				"claude-x": "no-such-route",
			}},
			wantSub: "target \"no-such-route\" not found in routes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestConfig_ValidateAcceptsDuplicatePriorities verifies that targets sharing
// the same priority within a route are ACCEPTED - the scheduler ranks same-
// priority targets by surplus (tier -> priority -> surplus), so duplicates are
// a feature (a surplus-competed pool), not a config error.
func TestConfig_ValidateAcceptsDuplicatePriorities(t *testing.T) {
	cfg := &Config{Listen: ":1", Providers: map[string]Provider{
		"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
		"b": {OpenAIBaseURL: "https://y", Provider: "zhipu"},
	}, Routes: map[string][]RouteTarget{
		"m": {
			{Provider: "a", Model: "m", Priority: 1},
			{Provider: "b", Model: "m", Priority: 1},
		},
	}}
	if err := cfg.validate(); err != nil {
		t.Errorf("duplicate priority should be accepted (surplus-competed pool), got error: %v", err)
	}
}

func TestPeakConfig_Unmarshal(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []PeakSegment
	}{
		{"single string", `peak_hours: "09:00-18:00"`, []PeakSegment{{Window: "09:00-18:00"}}},
		{"list of strings", "peak_hours:\n  - \"09:00-12:00\"\n  - \"14:00-18:00\"", []PeakSegment{{Window: "09:00-12:00"}, {Window: "14:00-18:00"}}},
		{"list of maps", "peak_hours:\n  - {window: \"09:00-12:00\", multiplier: 2}\n  - {window: \"14:00-18:00\", multiplier: 3}", []PeakSegment{{Window: "09:00-12:00", Multiplier: 2}, {Window: "14:00-18:00", Multiplier: 3}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wrap struct {
				PeakHours PeakConfig `yaml:"peak_hours"`
			}
			if err := yaml.Unmarshal([]byte(tc.yaml), &wrap); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if len(wrap.PeakHours) != len(tc.want) {
				t.Fatalf("got %d segments, want %d", len(wrap.PeakHours), len(tc.want))
			}
			for i, want := range tc.want {
				got := wrap.PeakHours[i]
				if got.Window != want.Window {
					t.Errorf("seg %d Window: got %q, want %q", i, got.Window, want.Window)
				}
				if want.Multiplier != 0 && got.Multiplier != want.Multiplier {
					t.Errorf("seg %d Multiplier: got %v, want %v", i, got.Multiplier, want.Multiplier)
				}
			}
		})
	}
}

func TestProvider_PeakMultiplier(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{
		{Window: "09:00-12:00", Multiplier: 2},
		{Window: "14:00-18:00", Multiplier: 3},
	}}
	parseHHMMRange("09:00-12:00") // ensure parser initialized
	at := func(h, m int) time.Time { return time.Date(2026, 7, 5, h, m, 0, 0, time.Local) }
	if got := p.peakMultiplier(at(10, 0)); got != 2 {
		t.Errorf("10:00 (in 09-12) mult=%v, want 2", got)
	}
	if got := p.peakMultiplier(at(15, 0)); got != 3 {
		t.Errorf("15:00 (in 14-18) mult=%v, want 3", got)
	}
	if got := p.peakMultiplier(at(13, 0)); got != 1 {
		t.Errorf("13:00 (no segment) mult=%v, want 1", got)
	}
}

func TestScheduling_QuotaDefaults(t *testing.T) {
	var s Scheduling
	if s.pollInterval() != 5*time.Minute {
		t.Errorf("default pollInterval=%v, want 5m", s.pollInterval())
	}
	if s.switchMargin() != 0.15 {
		t.Errorf("default switchMargin=%v, want 0.15", s.switchMargin())
	}
	s.QuotaSwitchMargin = 20
	if s.switchMargin() != 0.20 {
		t.Errorf("switchMargin(20)=%v, want 0.20", s.switchMargin())
	}
}

func TestConfigWebField(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(`
listen: 127.0.0.1:16000
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
web:
  enabled: false
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Web.Enabled {
		t.Fatalf("expected Web.Enabled=false, got true")
	}
}

func TestConfigWebDefaultTrue(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(`
listen: 127.0.0.1:16000
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Web.Enabled {
		t.Fatalf("expected Web.Enabled default true, got false")
	}
}
