package config

import (
	"strings"
	"testing"
)

// config_extra_test.go covers the validate error branches and Scheduling /
// expandPath / PeakConfig edges not exercised by config_test.go.

// TestRouteTarget_CompactForm: a route target accepts the compact
// "provider/model" scalar (split at the FIRST slash, so models may contain
// "/") alongside the full map form; malformed scalars fail loudly.
func TestRouteTarget_CompactForm(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:1
providers:
  z: {provider_id: zhipu, openai_base_url: "https://x", models: [glm-5.2]}
  o: {provider_id: zhipu, openai_base_url: "https://y", models: ["openai/gpt-5"]}
routes:
  mixed: [z/glm-5.2, "o/openai/gpt-5", {provider: z, model: glm-5.2, priority: 9, protocol: openai}]
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	targets := cfg.Routes["mixed"]
	if len(targets) != 3 {
		t.Fatalf("targets = %v", targets)
	}
	if targets[0] != (RouteTarget{Provider: "z", Model: "glm-5.2"}) {
		t.Errorf("compact target = %+v", targets[0])
	}
	if targets[1].Provider != "o" || targets[1].Model != "openai/gpt-5" {
		t.Errorf("slashed model target = %+v", targets[1])
	}
	if targets[2].Priority != 9 || targets[2].Protocol != "openai" {
		t.Errorf("map form target = %+v", targets[2])
	}

	for _, doc := range []string{
		"z-no-slash", // missing "/"
		"z/",         // empty model
		"/glm-5.2",   // empty provider
		"42",         // non-string scalar
	} {
		bad := `listen: 127.0.0.1:1
providers:
  z: {provider_id: zhipu, openai_base_url: "https://x", models: [glm-5.2]}
routes:
  m: [` + doc + `]
`
		if _, err := LoadConfigFromBytes("test", []byte(bad)); err == nil {
			t.Errorf("malformed target %q: want parse error", doc)
		}
	}
}

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

func TestValidate_Budgets(t *testing.T) {
	base := func() *Config {
		return &Config{
			Listen:    "127.0.0.1:1",
			Providers: map[string]Provider{"x": {OpenAIBaseURL: "https://x", Provider: "zhipu"}},
		}
	}

	cfg := base()
	cfg.Budgets = BudgetsConfig{MonthlyUSD: 20, Providers: map[string]float64{"x": 5}, WebhookURL: "https://hooks.example.com/budget"}
	if err := cfg.validate(); err != nil {
		t.Errorf("valid budgets: want nil, got %v", err)
	}

	cfg = base()
	cfg.Budgets = BudgetsConfig{MonthlyUSD: -1}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "budgets.monthly_usd") {
		t.Errorf("negative monthly_usd: err=%v", err)
	}

	cfg = base()
	cfg.Budgets = BudgetsConfig{Providers: map[string]float64{"x": -1}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), `budgets.providers["x"]`) {
		t.Errorf("negative provider threshold: err=%v", err)
	}

	cfg = base()
	cfg.Budgets = BudgetsConfig{Providers: map[string]float64{"typo": 5}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "not defined under providers:") {
		t.Errorf("unknown budgets provider: err=%v", err)
	}

	cfg = base()
	cfg.Budgets = BudgetsConfig{MonthlyUSD: 1, WebhookURL: "not-a-url"}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "budgets.webhook_url") {
		t.Errorf("bad webhook_url: err=%v", err)
	}
}
