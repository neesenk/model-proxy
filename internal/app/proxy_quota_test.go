package app

import (
	"encoding/json"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/provider"
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
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"m1": {
			{Provider: "primary", Model: "m1", Priority: 1},
			{Provider: "fallback", Model: "m1", Priority: 2},
		}},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := newTestProxy(t, cfg)
	refreshed := make(chan string, 2)
	p.providers["primary"] = &quotaCountProv{name: "primary", refreshed: refreshed}
	p.providers["fallback"] = &quotaCountProv{name: "fallback", refreshed: refreshed}
	p.quota.Stop()
	p.quota = runtimestate.NewQuotaTracker(
		"",
		func() *Config { return cfg },
		func() map[string]provider.Provider { return p.providers },
		&p.runtimeState,
	)
	p.quota.Generation = p.configGeneration.Load
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	select {
	case name := <-refreshed:
		if name != "primary" {
			t.Errorf("refreshed provider=%q, want primary (the one that 429'd)", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota refresh did not run after 429")
	}
	p.quota.Stop() // drain every admitted refresh before asserting exact cardinality
	select {
	case name := <-refreshed:
		t.Fatalf("unexpected duplicate quota refresh for %q", name)
	default:
	}
}

// quotaCountProv is a controllable Provider whose real Quota call records its
// own identity. This proves the 429 path refreshes the provider that failed,
// rather than merely proving an arbitrary refresh happened.
type quotaCountProv struct {
	testProv
	name      string
	refreshed chan<- string
	calls     atomic.Int32
}

func (q *quotaCountProv) Quota() (*provider.QuotaSnapshot, error) {
	q.calls.Add(1)
	if q.refreshed != nil {
		q.refreshed <- q.name
	}
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5}, nil
}

// staticSurplus sets a plan snapshot whose single ultimate window has the given
// remaining and time-left fraction (reset = now + fLeft×7d). Its surplus is
// remaining − fLeft. No short window → no peak-burn deduction.
func staticSurplus(p *Proxy, name string, remaining, fLeft float64) {
	now := time.Now()
	const dur = 7 * 24 * time.Hour
	p.quota.SetSnapshot(name, &provider.QuotaSnapshot{
		Billing:      provider.BillingPlan,
		RemainingPct: remaining,
		Windows: []provider.QuotaWindow{{
			Ultimate: true, Kind: "tokens", RemainingPct: remaining, Total: 200,
			Duration: dur, ResetsAt: now.Add(time.Duration(fLeft * float64(dur))),
		}},
		AsOf: now,
	})
}

// newQuotaProxy builds a Proxy wired with a hand-built runtimestate.QuotaTracker (no polling),
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
		poolIndex: map[string][]string{},
		parentOf:  map[string]string{},
	}
	p.expandedRoutes = p.buildExpandedRoutes()
	p.quota = runtimestate.NewQuotaTracker(
		"",
		func() *Config { return cfg },
		func() map[string]provider.Provider { return p.providers },
		&p.runtimeState,
	)
	p.quota.Generation = p.configGeneration.Load
	t.Cleanup(p.Close)
	for name := range provs {
		p.providers[name] = &testProv{key: name}
	}
	return p
}

// firstProvider returns the provider the scheduler tries first for a model.
func firstProvider(p *Proxy, model string) string {
	routeKeys := map[string]bool{}
	for k := range p.cfg.Routes {
		routeKeys[k] = true
	}
	ordered := p.schedule(p.cfg, p.parentOf, model, "", p.cfg.Routes[model], routeKeys)
	if len(ordered) == 0 {
		return ""
	}
	return ordered[0].Provider
}

// TestScheduleAdapter_AppliesPeakMultiplier verifies the root adapter projects
// Provider.PeakHours into runtime.Target.PeakMultiplier. The underlying surplus
// state machine is covered in internal/runtime.
//
// A provider in peak (multiplier 2) with a
// short rate-cap window has its surplus reduced by the peak-burn deduction, so a
// non-peak peer (same ultimate remaining + time-left) ranks ahead.
func TestScheduleAdapter_AppliesPeakMultiplier(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{
			"plain": {},
			"peak":  {PeakHours: PeakConfig{{Window: "00:00-23:59", Multiplier: 2}}},
		},
		// peak listed first: a lost peak-burn deduction leaves both surpluses at
		// 0, and sort.SliceStable would keep config order — the expected winner
		// must not be first.
		map[string][]RouteTarget{"m": {{Provider: "peak"}, {Provider: "plain"}}})
	now := time.Now()
	const dur = 7 * 24 * time.Hour
	ult := provider.QuotaWindow{Ultimate: true, Kind: "tokens", RemainingPct: 0.5, Total: 200,
		Duration: dur, ResetsAt: now.Add(dur / 2)} // fLeft 0.5
	// plain: ultimate only → surplus 0.5 − 0.5 = 0.
	p.quota.SetSnapshot("plain", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5,
		Windows: []provider.QuotaWindow{ult}, AsOf: now})
	// peak: ultimate + short (rem 0.6, total 100 → share 0.5); peak mult 2 →
	// remaining = 0.5 − 0.6×0.5×1 = 0.2 → surplus 0.2 − 0.5 = −0.3.
	p.quota.SetSnapshot("peak", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5,
		Windows: []provider.QuotaWindow{ult, {Short: true, Kind: "tokens", RemainingPct: 0.6, Total: 100}}, AsOf: now})
	if got := firstProvider(p, "m"); got != "plain" {
		t.Errorf("first=%q, want plain (peak provider's surplus reduced by short-window burn)", got)
	}
}

// TestScheduleAdapter_AppliesQualityWeights verifies the root adapter projects
// Scheduling quality weights into the runtime decision: with the default
// weights a below-circuit-threshold failure burst sinks the provider; with the
// error weight explicitly disabled the same failures leave the order intact.
func TestScheduleAdapter_AppliesQualityWeights(t *testing.T) {
	build := func(disableWeight bool) *Proxy {
		p := newQuotaProxy(t,
			map[string]Provider{"flaky": {}, "steady": {}},
			map[string][]RouteTarget{"m": {{Provider: "flaky"}, {Provider: "steady"}}})
		if disableWeight {
			zero := 0
			p.cfg.Scheduling.QualityErrorWeight = &zero
		}
		now := time.Now()
		quota := &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: now}
		p.quota.SetSnapshot("flaky", quota)
		p.quota.SetSnapshot("steady", quota)
		// Two failures: below the circuit threshold (3), so the provider stays
		// available — only the quality penalty can reorder.
		p.runtimeState.RecordFailure("flaky", 3, time.Minute, 0)
		p.runtimeState.RecordFailure("flaky", 3, time.Minute, 0)
		return p
	}

	// Two fresh proxies: schedule() commits sticky to the first winner, which
	// would otherwise carry into the second phase.
	if got := firstProvider(build(false), "m"); got != "steady" {
		t.Errorf("default weights: first=%q, want steady (flaky penalized)", got)
	}
	if got := firstProvider(build(true), "m"); got != "flaky" {
		t.Errorf("disabled weight: first=%q, want flaky (config order kept)", got)
	}
}

func TestScheduleAdapter_ProjectsParentBillingAndMapsTargets(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	targets := []RouteTarget{
		{Provider: "pool#acct", Model: "pool-model", Priority: 1},
		{Provider: "plan", Model: "plan-model", Priority: 9},
	}
	p := newQuotaProxy(t,
		map[string]Provider{
			"pool": {Billing: "pay-as-you-go"},
			"plan": {},
		},
		map[string][]RouteTarget{"m": targets})
	p.parentOf = map[string]string{"pool#acct": "pool"}
	p.quota.SetSnapshot("pool#acct", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
	})
	p.quota.SetSnapshot("plan", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
	})
	if got := configuredBillingOverride(p.cfg.Providers["pool"].Billing); got != provider.BillingPayG {
		t.Fatalf("parent billing override=%v, want payg", got)
	}
	if got := p.quota.Snapshot("pool#acct"); got == nil || got.Billing != provider.BillingPlan {
		t.Fatalf("virtual quota=%+v, want fresh plan snapshot", got)
	}
	if got := p.quota.Snapshot("plan"); got == nil || got.Billing != provider.BillingPlan {
		t.Fatalf("plan quota=%+v, want fresh plan snapshot", got)
	}

	ordered, _ := p.decideOrder(
		p.cfg,
		p.parentOf,
		"m",
		"",
		targets,
		now,
		false,
		map[string]bool{"m": true},
	)
	if len(ordered) != 2 {
		t.Fatalf("ordered=%+v, want two targets", ordered)
	}
	if ordered[0].Provider != "plan" || ordered[0].Model != "plan-model" ||
		ordered[1].Provider != "pool#acct" || ordered[1].Model != "pool-model" {
		t.Fatalf("ordered=%+v, want exact plan then payg virtual target mapping", ordered)
	}
}

func TestScheduleAdapter_DerivesQuotaMaxAgeFromPollInterval(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	targets := []RouteTarget{
		{Provider: "unknown", Model: "unknown-model", Priority: 1},
		{Provider: "candidate", Model: "candidate-model", Priority: 1},
	}
	p := newQuotaProxy(t,
		map[string]Provider{"unknown": {}, "candidate": {}},
		map[string][]RouteTarget{"m": targets})
	p.quota.SetSnapshot("candidate", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now.Add(-5 * time.Minute),
	})

	p.cfg.Scheduling.QuotaPollInterval = "2m" // max age = 3 × 2m = 6m
	fresh, _ := p.decideOrder(
		p.cfg, p.parentOf, "m", "", targets, now, false, map[string]bool{"m": true},
	)
	if len(fresh) != 2 || fresh[0].Provider != "candidate" {
		t.Fatalf("6m max age order=%+v, want fresh plan candidate first", fresh)
	}

	p.cfg.Scheduling.QuotaPollInterval = "1m" // max age = 3m; snapshot is stale
	stale, _ := p.decideOrder(
		p.cfg, p.parentOf, "m", "", targets, now, false, map[string]bool{"m": true},
	)
	if len(stale) != 2 || stale[0].Provider != "unknown" {
		t.Fatalf("3m max age order=%+v, want stable unknown-first order", stale)
	}
}

// TestScheduleStatus: the /debug/schedule payload reports the first-choice
// provider + ordered list with tiers, read-only (no sticky mutation).
func TestScheduleStatus(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a", Priority: 1}, {Provider: "b", Priority: 2}}})
	staticSurplus(p, "a", 0.5, 0)   // surplus +0.5 (waste risk)
	staticSurplus(p, "b", 0.5, 0.5) // surplus 0
	var st struct {
		Models map[string]struct {
			First   string `json:"first"`
			Ordered []struct {
				Provider string `json:"provider"`
				Tier     string `json:"tier"`
			} `json:"ordered"`
		} `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatal(err)
	}
	m, ok := st.Models["m"]
	if !ok {
		t.Fatal("no model m in status")
	}
	if m.First != "a" {
		t.Errorf("first=%q, want a (higher surplus)", m.First)
	}
	if len(m.Ordered) != 2 || m.Ordered[0].Provider != "a" || m.Ordered[1].Provider != "b" {
		t.Errorf("ordered=%+v, want [a, b]", m.Ordered)
	}
	if m.Ordered[0].Tier != "plan" {
		t.Errorf("tier=%q, want plan", m.Ordered[0].Tier)
	}
}
