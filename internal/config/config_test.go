package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestConfig_ProviderBaseURLs validates the repository config.yaml's base URLs
// against the two path-construction mistakes the proxy's URL joining can hit:
//   - openai_base_url ending in /v1 (the proxy already strips the client's /v1
//     and would append onto it → double path)
//   - anthropic_base_url ending in /v1 (the proxy keeps /v1/messages → double /v1)
//
// It does NOT validate version-segment presence in general (e.g. zhipu's
// /api/paas/v4 has its own shape) — only the doubling regressions above.
func TestConfig_ProviderBaseURLs(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
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

// TestConfig_RouteTargets verifies the derived route table is non-empty and
// every target references an existing provider with a non-empty model name.
func TestConfig_RouteTargets(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
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
	if cfg.Prices != nil && len(cfg.Prices) != 0 {
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
