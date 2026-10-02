package config

import (
	"strings"
	"testing"
)

// routeStrategyFixture is a minimal valid config shell for route-strategy
// parsing: one provider plus the routes block under test.
const routeStrategyFixture = `
providers:
  a: {provider_id: zhipu, openai_base_url: "https://a.test"}
  b: {provider_id: deepseek, openai_base_url: "https://b.test"}
`

func loadRouteStrategyConfig(t *testing.T, routes string) *Config {
	t.Helper()
	cfg, err := LoadConfigFromBytes("config.yaml", []byte(routeStrategyFixture+routes))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestRouteStrategyListFormDefaultsToQuota(t *testing.T) {
	t.Parallel()

	cfg := loadRouteStrategyConfig(t, `
routes:
  m: [a/m, b/m]
`)
	if len(cfg.RouteStrategies) != 0 {
		t.Fatalf("list form must not declare strategies: %v", cfg.RouteStrategies)
	}
	if got := cfg.RouteStrategyFor("m"); got != RouteStrategyQuota {
		t.Fatalf("RouteStrategyFor(list form) = %q, want %q", got, RouteStrategyQuota)
	}
	if len(cfg.Routes["m"]) != 2 || cfg.Routes["m"][0].Provider != "a" {
		t.Fatalf("targets not parsed: %+v", cfg.Routes["m"])
	}
}

func TestRouteStrategyMapFormParsesStrategyAndTargets(t *testing.T) {
	t.Parallel()

	cfg := loadRouteStrategyConfig(t, `
routes:
  m:
    strategy: load_balance
    targets:
      - provider: a
        model: m
      - b/m
  plain: [a/m]
`)
	if got := cfg.RouteStrategies["m"]; got != RouteStrategyLoadBalance {
		t.Fatalf("RouteStrategies[m] = %q, want %q", got, RouteStrategyLoadBalance)
	}
	if got := cfg.RouteStrategyFor("m"); got != RouteStrategyLoadBalance {
		t.Fatalf("RouteStrategyFor(m) = %q, want %q", got, RouteStrategyLoadBalance)
	}
	if got := cfg.RouteStrategyFor("plain"); got != RouteStrategyQuota {
		t.Fatalf("RouteStrategyFor(plain) = %q, want %q", got, RouteStrategyQuota)
	}
	if len(cfg.Routes["m"]) != 2 || cfg.Routes["m"][1].Provider != "b" {
		t.Fatalf("map-form targets not parsed: %+v", cfg.Routes["m"])
	}
}

func TestRouteStrategyExplicitQuotaAccepted(t *testing.T) {
	t.Parallel()

	cfg := loadRouteStrategyConfig(t, `
routes:
  m:
    strategy: quota
    targets: [a/m]
`)
	if got := cfg.RouteStrategyFor("m"); got != RouteStrategyQuota {
		t.Fatalf("RouteStrategyFor(m) = %q, want %q", got, RouteStrategyQuota)
	}
}

func TestRouteStrategyUnknownValueRejected(t *testing.T) {
	t.Parallel()

	_, err := LoadConfigFromBytes("config.yaml", []byte(routeStrategyFixture+`
routes:
  m:
    strategy: round_robin
    targets: [a/m]
`))
	if err == nil || !strings.Contains(err.Error(), `route "m": unknown strategy "round_robin"`) {
		t.Fatalf("unknown strategy err = %v", err)
	}
}

func TestRouteStrategyMapFormWithoutTargetsRejected(t *testing.T) {
	t.Parallel()

	_, err := LoadConfigFromBytes("config.yaml", []byte(routeStrategyFixture+`
routes:
  m:
    strategy: load_balance
`))
	if err == nil || !strings.Contains(err.Error(), "targets: list") {
		t.Fatalf("strategy-only route err = %v", err)
	}
}

func TestRouteStrategyScalarRouteRejected(t *testing.T) {
	t.Parallel()

	_, err := LoadConfigFromBytes("config.yaml", []byte(routeStrategyFixture+`
routes:
  m: a/m
`))
	if err == nil || !strings.Contains(err.Error(), "target list") {
		t.Fatalf("scalar route err = %v", err)
	}
}

func TestRouteStrategyForStripsPlannerSuffixes(t *testing.T) {
	t.Parallel()

	cfg := loadRouteStrategyConfig(t, `
routes:
  m:
    strategy: load_balance
    targets: [a/m]
`)
	for _, key := range []string{"m#req", "m#ctx"} {
		if got := cfg.RouteStrategyFor(key); got != RouteStrategyLoadBalance {
			t.Fatalf("RouteStrategyFor(%q) = %q, want %q", key, got, RouteStrategyLoadBalance)
		}
	}
	if got := cfg.RouteStrategyFor("other#req"); got != RouteStrategyQuota {
		t.Fatalf("RouteStrategyFor(unknown pool) = %q, want %q", got, RouteStrategyQuota)
	}
}
