package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestConfig_ExampleHasNoProviders pins the post-migration normal form of the
// repository example: providers live in ~/.model-proxy/provider.json, so the
// example yaml must NOT carry a providers: section (the boot absorbs any
// straggler entries — a lingering section here means the example drifted).
func TestConfig_ExampleHasNoProviders(t *testing.T) {
	example, rerr := os.ReadFile(filepath.Join("..", "..", "config.yaml"))
	if rerr != nil {
		t.Skipf("repo example unreadable in this checkout: %v", rerr)
	}
	cfg, err := parseConfigBytes("config.yaml", example)
	if err != nil {
		t.Fatalf("parse repo example: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("repo config.yaml carries %d providers (%v) — the example must stay provider-free (provider.json owns them)", len(cfg.Providers), cfg.ProviderNames())
	}
}

// TestConfig_ProviderBaseURLs validates the template's base URLs
// against the two path-construction mistakes the proxy's URL joining can hit:
//   - openai_base_url ending in /v1 (the proxy already strips the client's /v1
//     and would append onto it → double path)
//   - anthropic_base_url ending in /v1 (the proxy keeps /v1/messages → double /v1)
//
// It does NOT validate version-segment presence in general (e.g. zhipu's
// /api/paas/v4 has its own shape) — only the doubling regressions above.
func TestConfig_ProviderBaseURLs(t *testing.T) {
	// Providers moved out of the repo example into the provider.json store
	// (internal/providerstore) — the shipped DEFAULTS the URL guard protects
	// are the `config init` template's. The example file's new invariant
	// (no providers: section) is pinned by TestConfig_ExampleHasNoProviders.
	cfg, err := LoadConfigFromBytes("template", []byte(DefaultConfigYAML))
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}
	if len(cfg.Providers) == 0 {
		t.Fatal("template carries no providers — template drift")
	}

	for name, prov := range cfg.Providers {
		if prov.OpenAIBaseURL == "" && prov.DecisionsBaseURL != "" {
			// Pure decisions provider (typesafe): no chat endpoint to join —
			// check the decisions URL instead (proxy strips /v1 from the client
			// path and appends /systemone).
			t.Run(name+"/decisions", func(t *testing.T) {
				upstream := strings.TrimRight(prov.DecisionsBaseURL, "/") + "/systemone"
				if strings.Contains(upstream, "/v1/v1") {
					t.Errorf("decisions URL has double /v1: %s", upstream)
				}
			})
			continue
		}
		if prov.OpenAIBaseURL == "" {
			// Anthropic-only provider (zcode ultra gateway: the openai leg is
			// pay-per-use, so config keeps it off that protocol): no chat
			// endpoint to join — only the anthropic check below applies. A
			// provider with no protocol base URL at all is a config bug.
			if prov.AnthropicBaseURL == "" {
				t.Errorf("%s has no openai/decisions/anthropic base URL", name)
				continue
			}
		} else {
			t.Run(name+"/openai", func(t *testing.T) {
				// Openai: proxy strips /v1 from client path, appends remainder.
				// Simulate: client sends /v1/chat/completions → stripped to /chat/completions
				upstream := strings.TrimRight(prov.OpenAIBaseURL, "/") + "/chat/completions"
				// Should not have double /v1
				if strings.Contains(upstream, "/v1/v1") {
					t.Errorf("openai URL has double /v1: %s (openai_base_url should NOT end with /v1)", upstream)
				}
			})
		}

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

// TestConfig_RouteTargets verifies the derived route table is non-empty and
// every target references an existing provider with a non-empty model name.
func TestConfig_RouteTargets(t *testing.T) {
	cfg, err := LoadConfigFromBytes("template", []byte(DefaultConfigYAML))
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}
	exposedNames := cfg.RouteExposedNames()
	if len(exposedNames) == 0 {
		t.Fatal("no routes configured")
	}
	derived := cfg.ExposedModelNames()
	for name := range derived {
		found := false
		for _, prov := range cfg.Providers {
			for _, m := range prov.Models {
				if prov.ExposedModelName(m) == name {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("exposed name %q not derivable from any provider model", name)
		}
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

// TestConfig_ClaudeMappingRemoved: the claude_mapping key was removed — a
// config still carrying it must fail to load with a migration hint instead of
// silently dropping the mapping (tombstone in LoadConfigFromBytes).
func TestConfig_ClaudeMappingRemoved(t *testing.T) {
	doc := []byte(`listen: 127.0.0.1:1
providers:
  a: {openai_base_url: "https://x", provider_id: zhipu, models: [glm-5.2]}
claude_mapping:
  claude-haiku-4-5: glm-5.2
`)
	if _, err := LoadConfigFromBytes("config.yaml", doc); err == nil ||
		!strings.Contains(err.Error(), "claude_mapping is no longer supported") {
		t.Errorf("claude_mapping tombstone: want migration error, got %v", err)
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
			name: "no base url at all",
			cfg: &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
				"x": {Provider: "zhipu"},
			}},
			wantSub: "at least one of openai_base_url / anthropic_base_url",
		},
		{
			name: "empty provider_id",
			cfg: &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
				"x": {OpenAIBaseURL: "https://x", Provider: ""},
			}},
			wantSub: "provider_id is empty",
		},
		{
			name: "unknown provider_id",
			cfg: &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
				"x": {OpenAIBaseURL: "https://x", Provider: "unknown-typo"},
			}},
			wantSub: "unknown provider_id",
		},
		{
			name: "anthropic_base_url ends with /v1",
			cfg: &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
				"x": {OpenAIBaseURL: "https://x/v3", AnthropicBaseURL: "https://x/anthropic/v1", Provider: "zhipu"},
			}},
			wantSub: "anthropic_base_url ends with /v1",
		},
		{
			name: "route references unknown provider",
			cfg: &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
				"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
			}, Routes: map[string][]RouteTarget{
				"m": {{Provider: "nonexistent", Model: "m", Priority: 1}},
			}},
			wantSub: "provider \"nonexistent\" not defined",
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

func TestValidate_AcceptsZcodeProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"zcode": {Provider: "zcode", AnthropicBaseURL: "https://open.bigmodel.cn/api/anthropic"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("zcode provider_id rejected: %v", err)
	}
}

func TestValidate_AcceptsQwenPlanProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"qwen-plan": {Provider: "qwen-plan", OpenAIBaseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("qwen-plan provider_id rejected: %v", err)
	}
}

func TestValidate_AcceptsMiMoProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"mimo": {Provider: "mimo", OpenAIBaseURL: "https://api.xiaomimimo.com/v1"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("mimo provider_id rejected: %v", err)
	}
}

func TestValidate_AcceptsOpenRouterProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"openrouter": {Provider: "openrouter", OpenAIBaseURL: "https://openrouter.ai/api/v1"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("openrouter provider_id rejected: %v", err)
	}
}

func TestValidate_AcceptsOpenCodeProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"opencode-go": {Provider: "opencode-go", OpenAIBaseURL: "https://opencode.ai/zen/go/v1"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("opencode-go provider_id rejected: %v", err)
	}
}

func TestValidate_AcceptsStepPlanProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"step-plan": {Provider: "step-plan", OpenAIBaseURL: "https://api.stepfun.com/step_plan/v1"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("step-plan provider_id rejected: %v", err)
	}
}

// TestConfig_ValidateShadowErrors: shadow validation (config.go validate) —
// each shadow entry must reference a real route + provider and a known
// protocol; the global sample rate must be in [0,1] and max_concurrent >= 0.
// Table-driven over a shared valid base config, mirroring
// TestConfig_ValidateErrors.
func TestConfig_ValidateShadowErrors(t *testing.T) {
	rate := func(f float64) *float64 { return &f }
	base := func() *Config {
		return &Config{
			Listen: "127.0.0.1:1",
			Providers: map[string]Provider{
				"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
			},
			Routes: map[string][]RouteTarget{
				"m": {{Provider: "a", Model: "m", Priority: 1}},
			},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string // substring the error message should contain
	}{
		{
			name:    "shadow route missing",
			mutate:  func(c *Config) { c.Shadow = map[string]ShadowTarget{"ghost": {Provider: "a", Model: "m"}} },
			wantSub: `shadow "ghost": route not found`,
		},
		{
			name:    "shadow provider missing",
			mutate:  func(c *Config) { c.Shadow = map[string]ShadowTarget{"m": {Provider: "ghost", Model: "m"}} },
			wantSub: `provider "ghost" not defined`,
		},
		{
			name: "shadow protocol invalid",
			mutate: func(c *Config) {
				c.Shadow = map[string]ShadowTarget{"m": {Provider: "a", Model: "m", Protocol: "grpc"}}
			},
			wantSub: `protocol "grpc" is not`,
		},
		{
			name: "shadow anthropic protocol without anthropic base URL",
			mutate: func(c *Config) {
				c.Shadow = map[string]ShadowTarget{"m": {Provider: "a", Model: "m", Protocol: "anthropic"}}
			},
			wantSub: "anthropic_base_url",
		},
		{
			name: "shadow responses protocol without OpenAI base URL",
			mutate: func(c *Config) {
				c.Providers["a"] = Provider{AnthropicBaseURL: "https://x", Provider: "zhipu"}
				c.Shadow = map[string]ShadowTarget{"m": {Provider: "a", Model: "m", Protocol: "responses"}}
			},
			wantSub: "openai_base_url",
		},
		{
			name:    "sample rate negative",
			mutate:  func(c *Config) { c.ShadowSampleRate = rate(-0.1) },
			wantSub: "shadow_sample_rate -0.1 out of range [0, 1]",
		},
		{
			name:    "sample rate above 1",
			mutate:  func(c *Config) { c.ShadowSampleRate = rate(1.5) },
			wantSub: "shadow_sample_rate 1.5 out of range [0, 1]",
		},
		{
			name:    "max_concurrent negative",
			mutate:  func(c *Config) { c.ShadowMaxConcurrent = -1 },
			wantSub: "shadow_max_concurrent -1 must be >= 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestConfig_ValidateShadowOK: a well-formed shadow block (known route +
// provider, explicit protocol, boundary rates) passes validation.
func TestConfig_ValidateShadowOK(t *testing.T) {
	for _, r := range []float64{0, 0.5, 1} {
		cfg := &Config{
			Listen: "127.0.0.1:1",
			Providers: map[string]Provider{
				"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
			},
			Routes: map[string][]RouteTarget{
				"m": {{Provider: "a", Model: "m", Priority: 1}},
			},
			Shadow:              map[string]ShadowTarget{"m": {Provider: "a", Model: "m2", Protocol: "openai"}},
			ShadowSampleRate:    &r,
			ShadowMaxConcurrent: 8,
		}
		if err := cfg.validate(); err != nil {
			t.Errorf("rate=%v: valid shadow config rejected: %v", r, err)
		}
	}
}

func TestLoadShadowRejectsProtocolWithoutMatchingBaseURL(t *testing.T) {
	_, err := LoadConfigFromBytes("x", []byte(`
listen: 127.0.0.1:1
providers:
  candidate: {provider_id: static, anthropic_base_url: https://example.com}
routes:
  m: [{provider: candidate, model: m}]
shadow:
  m: {provider: candidate, model: m, protocol: responses}
`))
	if err == nil || !strings.Contains(err.Error(), "openai_base_url") {
		t.Fatalf("LoadConfigFromBytes error = %v, want missing openai_base_url", err)
	}
}

// TestConfig_ValidateAcceptsDuplicatePriorities verifies that targets sharing
// the same priority within a route are ACCEPTED - the scheduler ranks same-
// priority targets by surplus (tier -> priority -> surplus), so duplicates are
// a feature (a surplus-competed pool), not a config error.
func TestConfig_ValidateAcceptsDuplicatePriorities(t *testing.T) {
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
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

// TestConfig_ValidateListenLoopback enforces the loopback-only listen rule:
// /api/* and /ui/ are unauthenticated, so binding to anything but loopback
// (including the empty host form ":PORT", which binds all interfaces) is a
// hard config error.
func TestConfig_ValidateListenLoopback(t *testing.T) {
	base := func(listen string) *Config {
		return &Config{Listen: listen, Providers: map[string]Provider{
			"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
		}}
	}
	for _, listen := range []string{"0.0.0.0:15721", ":15721", "[::]:15721", "192.168.1.10:15721", "example.com:15721"} {
		err := base(listen).validate()
		if err == nil || !strings.Contains(err.Error(), "not loopback") {
			t.Errorf("listen %q: want 'not loopback' error, got %v", listen, err)
		}
	}
	if err := base("127.0.0.1").validate(); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("listen without port: want 'invalid' error, got %v", err)
	}
	for _, listen := range []string{"127.0.0.1:15721", "127.0.0.2:15721", "[::1]:15721", "localhost:15721"} {
		if err := base(listen).validate(); err != nil {
			t.Errorf("listen %q: loopback should be accepted, got %v", listen, err)
		}
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
	if got := p.PeakMultiplier(at(10, 0)); got != 2 {
		t.Errorf("10:00 (in 09-12) mult=%v, want 2", got)
	}
	if got := p.PeakMultiplier(at(15, 0)); got != 3 {
		t.Errorf("15:00 (in 14-18) mult=%v, want 3", got)
	}
	if got := p.PeakMultiplier(at(13, 0)); got != 1 {
		t.Errorf("13:00 (no segment) mult=%v, want 1", got)
	}
	// Window edges: start inclusive, end exclusive.
	if got := p.PeakMultiplier(at(9, 0)); got != 2 {
		t.Errorf("09:00 (window start, inclusive) mult=%v, want 2", got)
	}
	if got := p.PeakMultiplier(at(12, 0)); got != 1 {
		t.Errorf("12:00 (window end, exclusive) mult=%v, want 1", got)
	}
	if got := p.PeakMultiplier(at(14, 0)); got != 3 {
		t.Errorf("14:00 (second window start, inclusive) mult=%v, want 3", got)
	}
	if got := p.PeakMultiplier(at(18, 0)); got != 1 {
		t.Errorf("18:00 (second window end, exclusive) mult=%v, want 1", got)
	}
}

func TestScheduling_QuotaDefaults(t *testing.T) {
	var s Scheduling
	if s.PollInterval() != 5*time.Minute {
		t.Errorf("default pollInterval=%v, want 5m", s.PollInterval())
	}
	if s.SwitchMargin() != 0.15 {
		t.Errorf("default switchMargin=%v, want 0.15", s.SwitchMargin())
	}
	s.QuotaSwitchMargin = 20
	if s.SwitchMargin() != 0.20 {
		t.Errorf("switchMargin(20)=%v, want 0.20", s.SwitchMargin())
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

func TestPricingConfigDefaults(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte("listen: 127.0.0.1:1\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://example.com/api/v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Pricing.IsEnabled() {
		t.Error("pricing should default to enabled")
	}
	if cfg.Pricing.TTLDuration() != 24*time.Hour {
		t.Errorf("default ttl = %v, want 24h", cfg.Pricing.TTLDuration())
	}
	if cfg.Pricing.ResolvedSourceURL() != "https://openrouter.ai/api/v1/models" {
		t.Errorf("default source = %q", cfg.Pricing.ResolvedSourceURL())
	}
	if len(cfg.Prices) != 0 {
		t.Errorf("prices should default empty, got %v", cfg.Prices)
	}
}

// TestLoadCacheAndShadow guards F1: cache: and shadow: must load from yaml. The
// rawConfig decode previously omitted them, so production configs got zero values
// (silent no-op) while tests building Config directly stayed green.
func TestLoadCacheAndShadow(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(`
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
routes:
  glm: [{provider: zhipu, model: glm-4}]
cache:
  enabled: true
  ttl: 5m
  max_entries: 50
shadow:
  glm:
    provider: zhipu
    model: glm-4-shadow
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Cache.Enabled {
		t.Error("cache.enabled did not load from yaml")
	}
	if cfg.Cache.TTL != "5m" || cfg.Cache.MaxEntries != 50 {
		t.Errorf("cache fields = %+v want ttl=5m max_entries=50", cfg.Cache)
	}
	if len(cfg.Shadow) != 1 || cfg.Shadow["glm"].Provider != "zhipu" || cfg.Shadow["glm"].Model != "glm-4-shadow" {
		t.Errorf("shadow did not load: %+v", cfg.Shadow)
	}
}

// TestValidate_ConversionBaseURL: a target declaring a backend protocol needs the
// provider's matching base URL, else validate fails (the converted request would
// have no upstream URL and 400 at runtime). protocol:anthropic → needs
// anthropic_base_url; protocol:openai|responses → needs openai_base_url (responses
// reuses the OpenAI base, e.g. codex's /responses endpoint); unknown protocol
// → error; valid → OK.
func TestValidate_ConversionBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		prov    Provider
		proto   string
		wantSub string // empty = expect no error
	}{
		{"anthropic without anthropic_base_url", Provider{OpenAIBaseURL: "https://x", Provider: "static"}, "anthropic", "anthropic_base_url"},
		{"openai without openai_base_url", Provider{AnthropicBaseURL: "https://x", Provider: "static"}, "openai", "openai_base_url"},
		{"responses without openai_base_url", Provider{AnthropicBaseURL: "https://x", Provider: "static"}, "responses", "openai_base_url"},
		{"decisions without decisions/openai base", Provider{AnthropicBaseURL: "https://x", Provider: "static"}, "decisions", "decisions_base_url"},
		{"unknown protocol", Provider{OpenAIBaseURL: "https://x", Provider: "static"}, "weird", `not "anthropic", "openai", "responses", or "decisions"`},
		{"anthropic with anthropic_base_url (valid)", Provider{AnthropicBaseURL: "https://x", Provider: "static"}, "anthropic", ""},
		{"openai with openai_base_url (valid)", Provider{OpenAIBaseURL: "https://x", Provider: "static"}, "openai", ""},
		{"responses with openai_base_url (valid)", Provider{OpenAIBaseURL: "https://x", Provider: "static"}, "responses", ""},
		{"decisions with decisions_base_url (valid)", Provider{DecisionsBaseURL: "https://x/v1", Provider: "static"}, "decisions", ""},
		{"decisions with openai_base_url fallback (valid)", Provider{OpenAIBaseURL: "https://x", Provider: "static"}, "decisions", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := (&Config{
				Listen:    "127.0.0.1:1",
				Providers: map[string]Provider{"z": c.prov},
				Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm", Protocol: c.proto}}},
			}).validate()
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("err=%v, want substring %q", err, c.wantSub)
			}
		})
	}
}

// TestValidate_ProviderBaseURLSchemeHost: the three provider base URLs must
// be absolute http(s) URLs with a host — a schemeless or hostless value would
// only surface at request-build time deep in the forward path. Covers
// decisions_base_url alongside the two long-standing fields.
func TestValidate_ProviderBaseURLSchemeHost(t *testing.T) {
	base := func(mutate func(*Provider)) *Config {
		p := Provider{OpenAIBaseURL: "https://api.example.com/v1", Provider: "static"}
		if mutate != nil {
			mutate(&p)
		}
		return &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"z": p}}
	}
	cases := []struct {
		name    string
		mutate  func(*Provider)
		wantSub string // empty = expect no error
	}{
		{"openai missing scheme", func(p *Provider) { p.OpenAIBaseURL = "api.example.com/v1" }, `openai_base_url "api.example.com/v1" is not a valid http(s) URL`},
		{"openai no host", func(p *Provider) { p.OpenAIBaseURL = "http://" }, `openai_base_url "http://" is not a valid http(s) URL`},
		{"openai ftp scheme", func(p *Provider) { p.OpenAIBaseURL = "ftp://api.example.com/v1" }, `openai_base_url "ftp://api.example.com/v1" is not a valid http(s) URL`},
		{"anthropic missing scheme", func(p *Provider) { p.AnthropicBaseURL = "api.example.com" }, `anthropic_base_url "api.example.com" is not a valid http(s) URL`},
		{"anthropic no host", func(p *Provider) { p.AnthropicBaseURL = "https://" }, `anthropic_base_url "https://" is not a valid http(s) URL`},
		{"decisions missing scheme", func(p *Provider) { p.DecisionsBaseURL = "api.example.com/v1" }, `decisions_base_url "api.example.com/v1" is not a valid http(s) URL`},
		{"decisions no host", func(p *Provider) { p.DecisionsBaseURL = "https://" }, `decisions_base_url "https://" is not a valid http(s) URL`},
		{"http with host and port valid", func(p *Provider) { p.OpenAIBaseURL = "http://127.0.0.1:8080/v1" }, ""},
		{"https anthropic valid", func(p *Provider) { p.AnthropicBaseURL = "https://api.example.com" }, ""},
		{"https decisions valid", func(p *Provider) { p.DecisionsBaseURL = "https://api.example.com/v1" }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.mutate).validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err=%v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidate_DataPathsAbsolute: explicitly set data locations
// (request_log.dir, request_log.mcp_dir, stats.db_path) must be absolute
// after ~ / env: expansion — the daemon may start from any working directory,
// so a relative path would write to a CWD-dependent location. Unset keeps the
// home-derived defaults and must stay valid.
func TestValidate_DataPathsAbsolute(t *testing.T) {
	base := func() *Config {
		return &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
			"a": {OpenAIBaseURL: "https://x", Provider: "zhipu"},
		}}
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string // empty = expect no error
	}{
		{"request_log.dir relative", func(c *Config) { c.RequestLog.Dir = "logs/requests" }, "request_log.dir \"logs/requests\" must be an absolute path"},
		{"request_log.mcp_dir relative", func(c *Config) { c.RequestLog.MCPDir = "mcplogs" }, "request_log.mcp_dir \"mcplogs\" must be an absolute path"},
		{"stats.db_path relative", func(c *Config) { c.Stats.DBPath = "stats.db" }, "stats.db_path \"stats.db\" must be an absolute path"},
		{"request_log.dir tilde expands absolute", func(c *Config) { c.RequestLog.Dir = "~/reqlogs" }, ""},
		{"stats.db_path tilde expands absolute", func(c *Config) { c.Stats.DBPath = "~/stats.db" }, ""},
		{"request_log.dir absolute valid", func(c *Config) { c.RequestLog.Dir = "/var/lib/model-proxy/requests" }, ""},
		{"stats.db_path absolute valid", func(c *Config) { c.Stats.DBPath = "/var/lib/model-proxy/stats.db" }, ""},
		{"request_log.mcp_dir absolute valid", func(c *Config) { c.RequestLog.MCPDir = "/var/lib/model-proxy/mcp" }, ""},
		{"unset keeps defaults valid", func(c *Config) {}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			err := cfg.validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err=%v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidate_ProviderHeadersCredentialGuard: credential-bearing provider
// header names (authorization, cookie, x-api-key, ...) must use env:VAR
// indirection — same rule as mcp: headers, red line 3 (credentials never
// land in config). Benign names keep literal values.
func TestValidate_ProviderHeadersCredentialGuard(t *testing.T) {
	base := func(headers map[string]string) *Config {
		return &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
			"a": {OpenAIBaseURL: "https://x", Provider: "static", Headers: headers},
		}}
	}
	for _, name := range []string{"Authorization", "authorization", "Cookie", "X-Api-Key", "api-key", "Proxy-Authorization", "X-Auth-Token"} {
		if err := base(map[string]string{name: "sk-literal-123"}).validate(); err == nil || !strings.Contains(err.Error(), "env:VAR") {
			t.Errorf("sensitive header %q with literal value: err=%v, want env:VAR rejection", name, err)
		}
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if err := base(map[string]string{name: "env:MY_UPSTREAM_TOKEN"}).validate(); err != nil {
			t.Errorf("sensitive header %q with env: reference rejected: %v", name, err)
		}
	}
	// Benign names keep literal values (client hints, tracing ids).
	if err := base(map[string]string{"x-ccswitch-client": "0.2.7"}).validate(); err != nil {
		t.Errorf("benign literal header rejected: %v", err)
	}
	// A sensitive name with a malformed env reference is still rejected.
	if err := base(map[string]string{"Authorization": "env:bad-var"}).validate(); err == nil || !strings.Contains(err.Error(), "env:VAR") {
		t.Errorf("malformed env: reference: err=%v, want rejection", err)
	}
}

func TestPricesParseAndUnits(t *testing.T) {
	yaml := `prices:
  glm-4.6: {input: 0.9, output: 0.9, cache_read: 0.09}
  doubao-seed-1-8-251228: {input: 0.5, output: 1.5}
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://example.com/api/v1
`
	cfg, err := LoadConfigFromBytes("x", []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Prices) != 2 {
		t.Fatalf("prices = %d entries, want 2", len(cfg.Prices))
	}
	if cfg.Prices["glm-4.6"].Input != 0.9 || cfg.Prices["glm-4.6"].CacheRead != 0.09 {
		t.Errorf("glm-4.6 parsed wrong: %+v", cfg.Prices["glm-4.6"])
	}
}

// TestPricingConfigSourceURL_Precedence pins the pricing endpoint resolution
// order: config `source_url` > MP_PRICING_URL env (mirrors MP_MODELSDEV_URL) >
// OpenRouter default. Each case runs as a subtest so t.Setenv is called exactly
// once per scope.
func TestPricingConfigSourceURL_Precedence(t *testing.T) {
	const def = "https://openrouter.ai/api/v1/models"

	t.Run("config_wins_over_env", func(t *testing.T) {
		t.Setenv("MP_PRICING_URL", "http://env.example/models")
		got := (PricingConfig{SourceURL: "http://cfg.example/m"}).ResolvedSourceURL()
		if got != "http://cfg.example/m" {
			t.Errorf("config should win over env: got %q", got)
		}
	})

	t.Run("env_when_config_unset", func(t *testing.T) {
		t.Setenv("MP_PRICING_URL", "http://env.example/models")
		got := (PricingConfig{}).ResolvedSourceURL()
		if got != "http://env.example/models" {
			t.Errorf("env fallback wrong: got %q, want %q", got, "http://env.example/models")
		}
	})

	t.Run("default_when_neither_set", func(t *testing.T) {
		t.Setenv("MP_PRICING_URL", "")
		got := (PricingConfig{}).ResolvedSourceURL()
		if got != def {
			t.Errorf("default wrong: got %q, want %q", got, def)
		}
	})
}

// --- request_log.hot_retention: hot-window resolver + fail-closed cross-check ---

func TestRequestLogHotRetentionDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 168 * time.Hour},               // default 7d
		{"0", 0},                            // disable archiving
		{"48h", 48 * time.Hour},             // explicit
		{"not-a-duration", 168 * time.Hour}, // malformed falls back
	}
	for _, tc := range cases {
		got := RequestLogConfig{HotRetention: tc.raw}.HotRetentionDuration()
		if got != tc.want {
			t.Errorf("HotRetentionDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestLoadRejectsHotRetentionNotInsideRetention(t *testing.T) {
	// validate() rejects a provider-less document ("no providers configured")
	// before any request_log rule runs, so the fixture carries one provider
	// (the minimal scaffold TestPricingConfigDefaults uses).
	base := "providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://example.com/api/v1\n" +
		"request_log:\n  enabled: true\n  retention: %s\n  hot_retention: %s\n"
	cases := []struct {
		retention, hot, wantErr string
	}{
		{"720h", "168h", ""}, // ok: hot < retention
		{"720h", "720h", "must be < request_log.retention"}, // boundary: equal is outside
		{"720h", "1000h", "must be < request_log.retention"},
		{"48h", "168h", "must be < request_log.retention"}, // default hot outside a short retention
		{"0", "168h", ""}, // keep-forever retention: any hot ok
		{"720h", "0", ""}, // archiving disabled: always ok
	}
	for _, tc := range cases {
		_, err := LoadConfigFromBytes("config.yaml", []byte(fmt.Sprintf(base, tc.retention, tc.hot)))
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("retention=%s hot=%s: unexpected error %v", tc.retention, tc.hot, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("retention=%s hot=%s: error = %v, want containing %q", tc.retention, tc.hot, err, tc.wantErr)
		}
	}
}

// hotRetentionLoadBase is the smallest document that reaches the request_log
// rules: validate() rejects a provider-less document first.
const hotRetentionLoadBase = "providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://example.com/api/v1\n" +
	"request_log:\n  enabled: true\n%s"

// TestLoadHotRetentionUnsetDefaultStillCrossChecked pins the adjudicated
// fail-closed rule: an ABSENT hot_retention resolves to its 168h default and
// that default takes part in the cross-check like a written value, so a
// retention shorter than 7 days is rejected instead of silently shrinking the
// hot window. The user never wrote the failing key, so the error must name the
// default (likewise retention's 720h default when only hot_retention is set).
// The loadable rows double as the yaml decode check (pitfalls #13): yaml.v3
// drops unknown keys silently, so constructing Config directly proves nothing
// about the `hot_retention:` key reaching the field.
func TestLoadHotRetentionUnsetDefaultStillCrossChecked(t *testing.T) {
	cases := []struct {
		name    string
		request string // request_log body appended to hotRetentionLoadBase
		wantRaw string // decoded RequestLog.HotRetention for loadable rows
		wantHot time.Duration
		wantSub string // "" = must load; else a substring of the error
	}{
		{"both unset keep defaults", "", "", 168 * time.Hour, ""},
		{"unset hot sits inside long retention", "  retention: 720h\n", "", 168 * time.Hour, ""},
		{"unset hot with keep-forever retention", `  retention: "0"` + "\n", "", 168 * time.Hour, ""},
		{"explicit hot decodes from yaml", "  retention: 720h\n  hot_retention: 48h\n", "48h", 48 * time.Hour, ""},
		{"explicit zero hot decodes from yaml", "  hot_retention: 0\n", "0", 0, ""},
		{"unset hot outside short retention", "  retention: 48h\n", "", 0,
			"request_log.hot_retention (unset, default 168h) must be < request_log.retention (48h)"},
		{"hot beyond the unset retention default", "  hot_retention: 1000h\n", "", 0,
			"request_log.hot_retention (1000h) must be < request_log.retention (unset, default 720h)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes("config.yaml", []byte(fmt.Sprintf(hotRetentionLoadBase, tc.request)))
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if cfg.RequestLog.HotRetention != tc.wantRaw {
					t.Errorf("RequestLog.HotRetention = %q, want %q", cfg.RequestLog.HotRetention, tc.wantRaw)
				}
				if got := cfg.RequestLog.HotRetentionDuration(); got != tc.wantHot {
					t.Errorf("HotRetentionDuration() = %v, want %v", got, tc.wantHot)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("load err = %v, want containing %q", err, tc.wantSub)
			}
		})
	}
}

// TestLoadEffectiveRejectsHotRetentionNotInsideRetention: the daemon and the
// Web config editor load through LoadConfigEffective (providers come from the
// store, not the yaml), so the cross-check must sit on the validate() path the
// two loaders share — a check private to LoadConfigFromBytes would let a Web
// edit save a hot window outside retention. The conforming control proves the
// rejection is the cross-check, not the provider swap.
func TestLoadEffectiveRejectsHotRetentionNotInsideRetention(t *testing.T) {
	const doc = "listen: 127.0.0.1:15721\nrequest_log:\n  retention: 720h\n  hot_retention: %s\n"
	providers := map[string]Provider{"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://example.com/api/v1"}}
	_, err := LoadConfigEffective("config.yaml", []byte(fmt.Sprintf(doc, "720h")), providers)
	if err == nil || !strings.Contains(err.Error(), "must be < request_log.retention") {
		t.Fatalf("LoadConfigEffective err = %v, want the hot-retention cross-check error", err)
	}
	cfg, err := LoadConfigEffective("config.yaml", []byte(fmt.Sprintf(doc, "168h")), providers)
	if err != nil {
		t.Fatalf("LoadConfigEffective (hot inside retention): %v", err)
	}
	if cfg.RequestLog.HotRetentionDuration() != 168*time.Hour {
		t.Errorf("HotRetentionDuration() = %v, want 168h", cfg.RequestLog.HotRetentionDuration())
	}
}

// TestLoadDurationErrorsPrecedeHotRetentionCrossCheck: the cross-check runs
// after the duration checks on purpose. Both accessors fall back to their
// defaults on a malformed value, so a cross-check that ran first would compare
// against the fallback and blame a valid hot_retention for a retention typo
// ("hot_retention (800h) must be < request_log.retention (30d)": 30d is not a
// Go duration and never became 720h).
func TestLoadDurationErrorsPrecedeHotRetentionCrossCheck(t *testing.T) {
	_, err := LoadConfigFromBytes("config.yaml", []byte(fmt.Sprintf(hotRetentionLoadBase,
		"  retention: 30d\n  hot_retention: 800h\n")))
	if err == nil || !strings.Contains(err.Error(), `request_log.retention: invalid duration "30d"`) {
		t.Fatalf("load err = %v, want the retention duration error", err)
	}
	if strings.Contains(err.Error(), "hot_retention") {
		t.Errorf("load err = %v, want the retention typo reported before the cross-check", err)
	}
}
