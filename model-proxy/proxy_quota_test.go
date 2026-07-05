package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

// TestProxy_QuotaRefreshOnRateLimit: a 429 on a provider triggers an async
// quota refresh of that provider.
func TestProxy_QuotaRefreshOnRateLimit(t *testing.T) {
	primary, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 429, `{}`, intHdr("Retry-After", "30"), 0
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{"m1": {
			{Provider: "primary", Model: "m1", Priority: 1},
			{Provider: "fallback", Model: "m1", Priority: 2},
		}},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := NewProxy(cfg)
	p.providers["primary"] = &quotaCountProv{}
	p.providers["fallback"] = &testProv{key: "f"}
	var refreshes atomic.Int32
	p.quota = &quotaTracker{
		state: map[string]*provider.QuotaSnapshot{},
		cfg:   func() *Config { return cfg },
		provs: func() map[string]provider.Provider { return p.providers },
	}
	// refreshHook lets the test count refreshes without running a real poll.
	p.quota.refreshHook = func(name string) { refreshes.Add(1) }
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	// The refresh is launched as a goroutine from recordRateLimit; give it a
	// brief moment to land before asserting (the failover HTTP round-trip to
	// the fallback normally dominates, but poll briefly to stay deterministic).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && refreshes.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := refreshes.Load(); got != 1 {
		t.Errorf("expected 1 quota refresh after 429, got %d", got)
	}
}

// quotaCountProv is a testProv whose Quota() is callable.
type quotaCountProv struct{ testProv }

func (q *quotaCountProv) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5}, nil
}

// staticQuota sets the tracker's snapshot for a provider (plan, given remaining%).
func staticQuota(p *Proxy, name string, rem float64) {
	p.quota.setSnapshot(name, &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: rem, AsOf: time.Now()})
}

// newQuotaProxy builds a Proxy wired with a hand-built quotaTracker (no polling),
// for schedule() unit tests. Providers are backed by testProv. It constructs the
// Proxy directly (not via NewProxy) so no real poller goroutine starts — which
// avoids a data race between that goroutine reading cfg.Scheduling and these
// tests mutating it (e.g. StickyDwell) after construction.
func newQuotaProxy(t *testing.T, provs map[string]Provider, routes map[string][]RouteTarget) *Proxy {
	t.Helper()
	cfg := &Config{
		Providers:  provs,
		Routes:     routes,
		Scheduling: Scheduling{QuotaSwitchMargin: 15},
	}
	p := &Proxy{
		cfg:       cfg,
		providers: map[string]provider.Provider{},
		client:    &http.Client{Timeout: 0},
		health:    map[string]*providerHealth{},
		sticky:    map[string]routeSticky{},
	}
	p.quota = &quotaTracker{state: map[string]*provider.QuotaSnapshot{}, cfg: func() *Config { return cfg }, provs: func() map[string]provider.Provider { return p.providers }}
	for name := range provs {
		p.providers[name] = &testProv{key: name}
	}
	return p
}

// firstProvider returns the provider the scheduler tries first for a model.
func firstProvider(p *Proxy, model string) string {
	ordered := p.schedule(p.cfg, model, p.cfg.Routes[model])
	if len(ordered) == 0 {
		return ""
	}
	return ordered[0].Provider
}

func TestSchedule_PicksMostRemaining(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}, "c": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}, {Provider: "c"}}})
	staticQuota(p, "a", 0.2)
	staticQuota(p, "b", 0.8)
	staticQuota(p, "c", 0.5)
	if got := firstProvider(p, "m"); got != "b" {
		t.Errorf("first=%q, want b (highest remaining)", got)
	}
}

func TestSchedule_StickyHoldsWithinDwell(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	p.cfg.Scheduling.StickyDwell = "10m"
	staticQuota(p, "a", 0.2) // current sticky (low)
	staticQuota(p, "b", 0.9) // much higher
	// seed sticky on 'a'
	p.sticky["m"] = routeSticky{provider: "a", since: time.Now()}
	if got := firstProvider(p, "m"); got != "a" {
		t.Errorf("within dwell: first=%q, want a (sticky despite lower remaining)", got)
	}
}

func TestSchedule_SwitchesAfterDwellByMargin(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	p.cfg.Scheduling.StickyDwell = "1ms"
	staticQuota(p, "a", 0.4)
	staticQuota(p, "b", 0.8) // ahead by 0.4 ≥ 0.15 margin
	p.sticky["m"] = routeSticky{provider: "a", since: time.Now().Add(-time.Second)}
	time.Sleep(2 * time.Millisecond) // dwell expired
	if got := firstProvider(p, "m"); got != "b" {
		t.Errorf("after dwell + margin: first=%q, want b", got)
	}
}

func TestSchedule_NoSwitchBelowMargin(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	p.cfg.Scheduling.StickyDwell = "1ms"
	staticQuota(p, "a", 0.5)
	staticQuota(p, "b", 0.6) // ahead by 0.1 < 0.15 margin
	p.sticky["m"] = routeSticky{provider: "a", since: time.Now().Add(-time.Second)}
	time.Sleep(2 * time.Millisecond)
	if got := firstProvider(p, "m"); got != "a" {
		t.Errorf("below margin: first=%q, want a (stay sticky)", got)
	}
}

func TestSchedule_PayGStrictLastResort(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"plan": {Billing: ""}, "payg": {Billing: "pay-as-you-go"}},
		map[string][]RouteTarget{"m": {{Provider: "plan"}, {Provider: "payg"}}})
	p.quota.setSnapshot("plan", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.05, AsOf: time.Now()})
	p.quota.setSnapshot("payg", &provider.QuotaSnapshot{Billing: provider.BillingPayG, RemainingPct: -1, AsOf: time.Now()})
	if got := firstProvider(p, "m"); got != "plan" {
		t.Errorf("plan@5%% still beats payg: first=%q, want plan", got)
	}
	// now mark plan unavailable (rate-limited) → payg used
	p.healthMu.Lock()
	p.health["plan"] = &providerHealth{rateLimitedUntil: time.Now().Add(time.Hour)}
	p.healthMu.Unlock()
	if got := firstProvider(p, "m"); got != "payg" {
		t.Errorf("when plan unavailable: first=%q, want payg (last resort)", got)
	}
}

// TestSchedule_PlanBeforeUnknown: a measurable Plan provider ranks ahead of an
// unmeasurable Unknown one (tier order: plan < unknown < payg), even when the
// Plan provider's remaining quota is low.
func TestSchedule_PlanBeforeUnknown(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"planprov": {}, "unkprov": {}},
		map[string][]RouteTarget{"m": {{Provider: "planprov"}, {Provider: "unkprov"}}})
	staticQuota(p, "planprov", 0.05) // low but known
	// unkprov: no snapshot → BillingUnknown
	if got := firstProvider(p, "m"); got != "planprov" {
		t.Errorf("first=%q, want planprov (plan tier ranks ahead of unknown)", got)
	}
}

// TestSchedule_PeakDiscountsEffectiveRemaining: at equal raw remaining, a
// provider inside a peak window (multiplier 2) has its effective remaining
// halved, so a non-peak peer ranks ahead. (Peak is folded into effective
// remaining, not a separate sort tier.)
func TestSchedule_PeakDiscountsEffectiveRemaining(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{
			"plain": {},
			"peak":  {PeakHours: PeakConfig{{Window: "00:00-23:59", Multiplier: 2}}},
		},
		map[string][]RouteTarget{"m": {{Provider: "plain"}, {Provider: "peak"}}})
	staticQuota(p, "plain", 0.5)
	staticQuota(p, "peak", 0.5) // same raw remaining, but peak → effective 0.25
	if got := firstProvider(p, "m"); got != "plain" {
		t.Errorf("first=%q, want plain (peak provider's effective remaining is discounted)", got)
	}
}

// TestSchedule_SwitchesOnPriorityAfterDwell: after dwell, with quota equal
// (sub-margin), the best provider wins on priority — the "return to the
// preferred provider" branch (proxy.go keepSticky priority arm).
func TestSchedule_SwitchesOnPriorityAfterDwell(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {
			{Provider: "a", Priority: 1},
			{Provider: "b", Priority: 2},
		}})
	p.cfg.Scheduling.StickyDwell = "1ms"
	staticQuota(p, "a", 0.5)
	staticQuota(p, "b", 0.5) // equal remaining → sub-margin; priority decides
	p.sticky["m"] = routeSticky{provider: "b", since: time.Now().Add(-time.Second)}
	time.Sleep(2 * time.Millisecond) // dwell expired
	if got := firstProvider(p, "m"); got != "a" {
		t.Errorf("after dwell: first=%q, want a (better priority wins on sub-margin quota)", got)
	}
}

// TestSchedule_PayGOrderByPriority: among multiple pay-as-you-go providers
// (same tier, no quota data), priority orders them; the higher-priority one
// serves, and the next serves when it's unavailable.
func TestSchedule_PayGOrderByPriority(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{
			"paygA": {Billing: "pay-as-you-go"},
			"paygB": {Billing: "pay-as-you-go"},
		},
		map[string][]RouteTarget{"m": {
			{Provider: "paygA", Priority: 1},
			{Provider: "paygB", Priority: 2},
		}})
	if got := firstProvider(p, "m"); got != "paygA" {
		t.Errorf("first=%q, want paygA (priority 1 within payg tier)", got)
	}
	p.healthMu.Lock()
	p.health["paygA"] = &providerHealth{rateLimitedUntil: time.Now().Add(time.Hour)}
	p.healthMu.Unlock()
	if got := firstProvider(p, "m"); got != "paygB" {
		t.Errorf("paygA unavailable: first=%q, want paygB", got)
	}
}
