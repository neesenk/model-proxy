package runtime

import (
	"math"
	"reflect"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

func scheduleQuota(billing provider.BillingClass, remaining float64, now time.Time) *provider.QuotaSnapshot {
	return &provider.QuotaSnapshot{
		Billing:      billing,
		RemainingPct: remaining,
		AsOf:         now,
		Windows: []provider.QuotaWindow{{
			Ultimate:     true,
			RemainingPct: remaining,
			Total:        100,
			ResetsAt:     now,
			Duration:     time.Hour,
		}},
	}
}

func setScheduleQuota(t *testing.T, m *Manager, name string, snapshot *provider.QuotaSnapshot, generation uint64) {
	t.Helper()
	if !m.SetQuota(name, snapshot, generation) {
		t.Fatalf("SetQuota(%q) rejected generation %d", name, generation)
	}
}

func TestDecideOrderSortsTierPriorityAndSurplus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	setScheduleQuota(t, m, "tier-2", scheduleQuota(provider.BillingPayG, 1, now), 1)
	setScheduleQuota(t, m, "priority-2", scheduleQuota(provider.BillingPlan, 1, now), 1)
	setScheduleQuota(t, m, "surplus-low", scheduleQuota(provider.BillingPlan, .1, now), 1)
	setScheduleQuota(t, m, "surplus-high", scheduleQuota(provider.BillingPlan, .2, now), 1)
	result := m.DecideOrder(ScheduleInput{
		Exposed:     "route",
		Now:         now,
		QuotaMaxAge: time.Hour,
		Targets: []Target{
			{Provider: "tier-2", Priority: 0},
			{Provider: "priority-2", Priority: 2},
			{Provider: "surplus-low", Priority: 1},
			{Provider: "surplus-high", Priority: 1},
		},
	})
	if !reflect.DeepEqual(result.Order, []int{3, 2, 1, 0}) ||
		result.StickyProvider != "surplus-high" {
		t.Fatalf("sorted result = %+v", result)
	}
}

func TestDecideOrderStickyDwellAndSwitchMargin(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 16, 0, 0, 0, time.UTC)
	targets := []Target{
		{Provider: "best", Priority: 1},
		{Provider: "current", Priority: 1},
	}
	base := ScheduleInput{
		Exposed:    "route",
		SessionKey: "session",
		Targets:    targets,
		RouteKeys:  map[string]bool{"route": true},
		Dwell:      time.Hour,
		// SwitchMargin is a FRACTION of remaining quota (config converts
		// percentage points via /100). The quota gap here is 1.0-0.7=0.3:
		// margin 0.5 keeps sticky only under the fractional reading — if the
		// implementation ever compared percentage points (or dropped the /100
		// conversion), 30 >= 0.5 would switch and both keep-cases below fail.
		SwitchMargin: 0.5,
		Now:          now,
		QuotaMaxAge:  time.Hour,
		Generation:   2,
	}

	m := newTestManager(2)
	setScheduleQuota(t, m, "best", scheduleQuota(provider.BillingPlan, 1, now), 2)
	setScheduleQuota(t, m, "current", scheduleQuota(provider.BillingPlan, .7, now), 2)
	m.SetSticky(StickyKey("session", "route"), Sticky{Provider: "current", Since: now.Add(-30 * time.Minute)}, 2)
	result := m.DecideOrder(base)
	if !reflect.DeepEqual(result.Order, []int{1, 0}) ||
		result.StickyProvider != "current" {
		t.Fatalf("sticky dwell result = %+v", result)
	}

	m.SetSticky(StickyKey("session", "route"), Sticky{Provider: "current", Since: now.Add(-2 * time.Hour)}, 2)
	result = m.DecideOrder(base)
	if !reflect.DeepEqual(result.Order, []int{1, 0}) ||
		result.StickyProvider != "current" {
		t.Fatalf("below-margin result = %+v", result)
	}

	// Below the fractional margin (0.2 < gap 0.3): the better plan wins.
	base.SwitchMargin = 0.2
	result = m.DecideOrder(base)
	if !reflect.DeepEqual(result.Order, []int{0, 1}) ||
		result.StickyProvider != "best" {
		t.Fatalf("at-margin result = %+v", result)
	}

	priorityTargets := append([]Target(nil), targets...)
	priorityTargets[1].Priority = 2
	base.Targets = priorityTargets
	base.SwitchMargin = 1000
	if result = m.DecideOrder(base); result.Order[0] != 0 {
		t.Fatalf("better priority did not switch: %+v", result)
	}
	setScheduleQuota(t, m, "current", scheduleQuota(provider.BillingPayG, .7, now), 2)
	base.Targets = targets
	if result = m.DecideOrder(base); result.Order[0] != 0 {
		t.Fatalf("better tier did not switch: %+v", result)
	}

	m.SetSticky("route", Sticky{Provider: "best", Since: now.Add(-2 * time.Hour)}, 2)
	base.SessionKey = ""
	base.Targets = targets
	result = m.DecideOrder(base)
	if result.Order[0] != 0 || result.StickyProvider != "" {
		t.Fatalf("route-level sticky result = %+v", result)
	}
}

func TestDecideOrderSkipsQuotaExhaustedStickyAccount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	m := newTestManager(7)
	targets := []Target{
		{Provider: "fresh", Priority: 1},
		{Provider: "exhausted", Priority: 1},
	}
	setScheduleQuota(t, m, "fresh", scheduleQuota(provider.BillingPlan, .5, now), 7)
	setScheduleQuota(t, m, "exhausted", scheduleQuota(provider.BillingPlan, 0, now), 7)
	m.SetSticky(StickyKey("session", "route"), Sticky{Provider: "exhausted", Since: now}, 7)
	input := ScheduleInput{
		Exposed: "route", SessionKey: "session", Targets: targets,
		RouteKeys: map[string]bool{"route": true},
		Dwell:     time.Hour, SwitchMargin: .15,
		Now: now, QuotaMaxAge: time.Hour, Generation: 7,
	}

	// Sticking to a KNOWN-exhausted plan account just eats a guaranteed 429:
	// sticky is skipped and the scored order takes over.
	result := m.DecideOrder(input)
	if !reflect.DeepEqual(result.Order, []int{0}) || result.StickyProvider != "fresh" {
		t.Fatalf("exhausted sticky must be skipped: %+v", result)
	}

	// The dashboard preview must mirror the real decision exactly.
	preview := m.Dashboard(now).PreviewOrder(input)
	if !reflect.DeepEqual(preview.Order, []int{0}) {
		t.Fatalf("preview did not mirror exhaustion skip: %+v", preview)
	}

	// A STALE exhausted snapshot proves nothing — fail open to the reactive
	// 429 cooldown rather than dropping the sticky account on old data.
	stale := scheduleQuota(provider.BillingPlan, 0, now)
	stale.AsOf = now.Add(-2 * time.Hour)
	setScheduleQuota(t, m, "exhausted", stale, 7)
	result = m.DecideOrder(input)
	if !reflect.DeepEqual(result.Order, []int{1, 0}) || result.StickyProvider != "exhausted" {
		t.Fatalf("stale exhausted snapshot must fail open: %+v", result)
	}

	// PayG has no window to exhaust; RemainingPct==0 must never exclude it.
	setScheduleQuota(t, m, "exhausted", scheduleQuota(provider.BillingPayG, 0, now), 7)
	result = m.DecideOrder(input)
	if result.Order[0] != 1 || result.StickyProvider != "exhausted" {
		t.Fatalf("payg zero snapshot must not exclude: %+v", result)
	}
}

// TestDecideOrderStickyHealthExclusionAndRecoverReturn pins the sticky×health
// interaction matrix for quota mode (load_balance covers the same ground in
// manager_load_balance_test.go): a sticky provider that is circuit-open,
// rate-limited, or model-locked leaves the ordered chain ENTIRELY — the
// availability filter runs before sticky is honored, so a down binding can
// never be resurrected while its down signal lasts. The sticky ENTRY itself
// is deliberately kept (not deleted on failure): once the provider recovers
// inside the dwell window, the session returns to it — recover-and-return is
// the intentional prompt-cache semantics, not a bug.
func TestDecideOrderStickyHealthExclusionAndRecoverReturn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	m := newTestManager(11)
	targets := []Target{
		{Provider: "best", Model: "m1", Priority: 1},
		{Provider: "current", Model: "m1", Priority: 1},
	}
	input := ScheduleInput{
		Exposed: "route", SessionKey: "session", Targets: targets,
		RouteKeys: map[string]bool{"route": true},
		Dwell:     time.Hour, SwitchMargin: .15,
		Now: now, QuotaMaxAge: time.Hour, Generation: 11,
	}
	m.SetSticky(StickyKey("session", "route"), Sticky{Provider: "current", Since: now}, 11)
	// "best" carries a strictly better measured plan, so a dwell-EXPIRED
	// sticky has a decisive reason to switch (an exact tie would legitimately
	// keep the session parked — sub-margin switch prevention).
	setScheduleQuota(t, m, "best", scheduleQuota(provider.BillingPlan, 1, now), 11)
	setScheduleQuota(t, m, "current", scheduleQuota(provider.BillingPlan, .5, now), 11)

	downStates := map[string]*providerHealth{
		"circuit":      {circuitOpenUntil: now.Add(time.Hour)},
		"rate-limited": {rateLimitedUntil: now.Add(time.Hour), rateLimitKind: Quota},
	}
	for name, state := range downStates {
		m.mu.Lock()
		m.health["current"] = state
		m.mu.Unlock()

		result := m.DecideOrder(input)
		if !reflect.DeepEqual(result.Order, []int{0}) || result.StickyProvider != "best" {
			t.Fatalf("%s: down sticky provider must leave the chain entirely: %+v", name, result)
		}
		if preview := m.Dashboard(now).PreviewOrder(input); !reflect.DeepEqual(preview.Order, []int{0}) {
			t.Fatalf("%s: preview did not mirror the exclusion: %+v", name, preview)
		}
		// The sticky entry survives the failure (recover-and-return needs it),
		// keyed on the session — it must NOT be deleted just because the
		// provider is down.
		if got := m.Dashboard(now).Sticky[StickyKey("session", "route")].Provider; got != "current" {
			t.Fatalf("%s: sticky entry deleted on failure (got %q) — recover-and-return impossible", name, got)
		}
	}

	// Model lock is (provider, model)-granular: locking current/m1 excludes
	// only that target, never best/m1.
	m.mu.Lock()
	delete(m.health, "current")
	m.modelLocks[ModelKey{Provider: "current", Model: "m1"}] = &modelLock{failures: 1, lockedUntil: now.Add(time.Hour)}
	m.mu.Unlock()
	if result := m.DecideOrder(input); !reflect.DeepEqual(result.Order, []int{0}) || result.StickyProvider != "best" {
		t.Fatalf("model-locked sticky target must leave the chain: %+v", result)
	}

	// Recovery inside the dwell window: the session returns to the sticky
	// provider (prompt cache), outranking the scored order.
	m.mu.Lock()
	delete(m.modelLocks, ModelKey{Provider: "current", Model: "m1"})
	m.mu.Unlock()
	returned := input
	returned.Now = now.Add(10 * time.Minute)
	result := m.DecideOrder(returned)
	if !reflect.DeepEqual(result.Order, []int{1, 0}) || result.StickyProvider != "current" {
		t.Fatalf("recovered sticky provider must be returned to within dwell: %+v", result)
	}

	// ...but NOT after the dwell expired unused: the idle sweep evicts the
	// entry on the next committed decision and the scored order takes over.
	expired := input
	expired.Now = now.Add(2 * time.Hour)
	result = m.DecideOrder(expired)
	if !reflect.DeepEqual(result.Order, []int{0, 1}) || result.StickyProvider != "best" {
		t.Fatalf("dwell-expired sticky must not pin the session: %+v", result)
	}
}

func TestDecideOrderUsesAtomicQuotaHealthPinAndStickyState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 16, 30, 0, 0, time.UTC)
	m := newTestManager(5)
	targets := []Target{{Provider: "plan", Model: "m"}, {Provider: "pinned", Model: "m"}}
	setScheduleQuota(t, m, "plan", scheduleQuota(provider.BillingPlan, .9, now), 5)

	// Hold the sole manager lock while the decision is queued, then replace every
	// scheduling input family. A decision released afterwards must see only this
	// complete state, never a quota/health/pin/sticky mixture.
	m.mu.Lock()
	m.quotas["pinned"] = scheduleQuota(provider.BillingPayG, .1, now)
	m.health["pinned"] = &providerHealth{rateLimitedUntil: now.Add(time.Hour)}
	m.pins["route"] = Pin{Provider: "pinned"}
	m.sticky["session"] = Sticky{Provider: "pinned", Since: now}
	resultCh := make(chan ScheduleResult, 1)
	go func() {
		resultCh <- m.DecideOrder(ScheduleInput{
			Exposed: "route", SessionKey: "session", Targets: targets,
			Dwell: time.Hour, Now: now, QuotaMaxAge: time.Hour, Generation: 5,
		})
	}()
	m.mu.Unlock()

	result := <-resultCh
	if !reflect.DeepEqual(result.Order, []int{1}) || result.StickyProvider != "pinned" {
		t.Fatalf("atomic decision order = %+v", result)
	}
	if result.Facts[1].Billing != provider.BillingPayG || result.Facts[0].Billing != provider.BillingPlan {
		t.Fatalf("atomic decision facts = %+v", result.Facts)
	}
}

func TestDecideOrderFiltersAndPinOverride(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	m := newTestManager(3)
	targets := []Target{
		{Provider: "rate", Model: "m"},
		{Provider: "circuit", Model: "m"},
		{Provider: "model", Model: "m"},
		{Provider: "healthy", Model: "m"},
	}
	m.mu.Lock()
	m.health["rate"] = &providerHealth{rateLimitedUntil: now.Add(time.Hour)}
	m.health["circuit"] = &providerHealth{circuitOpenUntil: now.Add(time.Hour)}
	m.modelLocks[ModelKey{Provider: "model", Model: "m"}] = &modelLock{
		lockedUntil: now.Add(time.Hour),
	}
	m.mu.Unlock()

	input := ScheduleInput{Exposed: "route", Targets: targets, Now: now}
	result := m.DecideOrder(input)
	if !reflect.DeepEqual(result.Order, []int{3}) ||
		result.StickyProvider != "healthy" {
		t.Fatalf("filtered result = %+v", result)
	}

	m.SetPin("route", Pin{Provider: "rate"})
	result = m.DecideOrder(input)
	if !reflect.DeepEqual(result.Order, []int{0}) ||
		result.StickyProvider != "rate" {
		t.Fatalf("direct pin did not override health: %+v", result)
	}

	poolTargets := []Target{
		{Provider: "pool#b", Parent: "pool", Model: "m"},
		{Provider: "pool#a", Parent: "pool", Model: "m"},
		{Provider: "other", Model: "m"},
	}
	m.SetPin("route", Pin{Provider: "pool"})
	result = m.DecideOrder(ScheduleInput{
		Exposed: "route",
		Targets: poolTargets,
		Now:     now,
	})
	if !reflect.DeepEqual(result.Order, []int{1, 0}) {
		t.Fatalf("parent pin result = %+v, want sorted pool members", result)
	}

	m.SetPin("route", Pin{Provider: "missing"})
	result = m.DecideOrder(input)
	if !reflect.DeepEqual(result.Order, []int{3}) {
		t.Fatalf("unmatched pin changed filtering: %+v", result)
	}
	m.SetPin("route", Pin{Provider: "rate", ExpiresAt: now})
	result = m.DecideOrder(input)
	if !reflect.DeepEqual(result.Order, []int{3}) {
		t.Fatalf("expired pin changed filtering: %+v", result)
	}
}

func TestDecideOrderMarksStaleAndErroredQuotaUnknownAndAppliesPeakMultiplier(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 18, 30, 0, 0, time.UTC)
	m := newTestManager(8)
	peak := scheduleQuota(provider.BillingPlan, .8, now)
	peak.Windows = append(peak.Windows, provider.QuotaWindow{Short: true, RemainingPct: .5, Total: 20})
	stale := scheduleQuota(provider.BillingPlan, .9, now.Add(-2*time.Hour))
	errored := scheduleQuota(provider.BillingPlan, .9, now)
	errored.Err = "quota poll failed"
	setScheduleQuota(t, m, "peak-multiplied", peak, 8)
	setScheduleQuota(t, m, "peak-normal", peak, 8)
	setScheduleQuota(t, m, "stale", stale, 8)
	setScheduleQuota(t, m, "error", errored, 8)

	result := m.DecideOrder(ScheduleInput{
		Exposed: "route", Now: now, QuotaMaxAge: time.Hour,
		Targets: []Target{
			{Provider: "peak-multiplied", PeakMultiplier: 2},
			{Provider: "peak-normal", PeakMultiplier: 0},
			{Provider: "stale"},
			{Provider: "error"},
		},
	})
	if !reflect.DeepEqual(result.Order, []int{1, 0, 2, 3}) {
		t.Fatalf("quota order = %v, want peak-multiplier ranking before unknowns", result.Order)
	}
	if got := result.Facts; got[0].Billing != provider.BillingPlan || math.Abs(got[0].Surplus-.7) > 1e-9 ||
		got[1].Billing != provider.BillingPlan || math.Abs(got[1].Surplus-.8) > 1e-9 ||
		got[2].Billing != provider.BillingUnknown || got[3].Billing != provider.BillingUnknown {
		t.Fatalf("quota facts = %+v", got)
	}
}

func TestDecideOrderComparesSurplusAcrossUltimatePeriods(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 18, 30, 0, 0, time.UTC)
	m := newTestManager(8)
	weekly := scheduleQuota(provider.BillingPlan, .5, now)
	weekly.Windows[0].Duration = 7 * 24 * time.Hour
	weekly.Windows[0].ResetsAt = now.Add(24 * time.Hour)
	monthly := scheduleQuota(provider.BillingPlan, .6, now)
	monthly.Windows[0].Duration = 30 * 24 * time.Hour
	monthly.Windows[0].ResetsAt = now.Add(15 * 24 * time.Hour)
	setScheduleQuota(t, m, "weekly", weekly, 8)
	setScheduleQuota(t, m, "monthly", monthly, 8)

	result := m.DecideOrder(ScheduleInput{
		Exposed: "route", Now: now, QuotaMaxAge: time.Hour,
		Targets: []Target{{Provider: "weekly"}, {Provider: "monthly"}},
	})
	if !reflect.DeepEqual(result.Order, []int{0, 1}) {
		t.Fatalf("cross-period order = %v, want weekly surplus before monthly", result.Order)
	}
	if got := result.Facts; got[0].Billing != provider.BillingPlan ||
		math.Abs(got[0].Surplus-(.5-1.0/7.0)) > 1e-9 ||
		got[1].Billing != provider.BillingPlan || math.Abs(got[1].Surplus-.1) > 1e-9 {
		t.Fatalf("cross-period surplus facts = %+v", got)
	}
}

// The scheduling tier comes from the MEASURED snapshot only. A provider's
// config `billing:` label (payment-method metadata) must NOT reach the
// scheduler: Target carries no billing field anymore, so a pay-as-you-go
// provider whose Quota() measures a plan window is ranked as a plan provider,
// and a `billing: pay-as-you-go` label can neither demote it nor (via the old
// quota-tracker skip gate) hide its console-only snapshot from the UI.
func TestDecideOrderTierComesFromMeasurementNotConfigLabel(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 18, 30, 0, 0, time.UTC)
	m := newTestManager(8)
	setScheduleQuota(t, m, "plan", scheduleQuota(provider.BillingPlan, .5, now), 8)
	// Same measured class + higher surplus: the only inputs the scheduler has.
	setScheduleQuota(t, m, "measured", scheduleQuota(provider.BillingPlan, .9, now), 8)

	result := m.DecideOrder(ScheduleInput{
		Exposed: "route", Now: now, QuotaMaxAge: time.Hour,
		Targets: []Target{
			{Provider: "plan"},
			{Provider: "measured"},
		},
	})
	if !reflect.DeepEqual(result.Order, []int{1, 0}) {
		t.Fatalf("order = %v, want the higher-surplus measured plan first", result.Order)
	}
	for i, want := range []provider.BillingClass{provider.BillingPlan, provider.BillingPlan} {
		if got := result.Facts[i].Billing; got != want {
			t.Fatalf("facts[%d].Billing = %v, want %v (measured, not a config label)", i, got, want)
		}
	}
}

func TestDecideOrderPoolSpreadCommitAndGeneration(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 18, 0, 0, 0, time.UTC)
	m := newTestManager(7)
	targets := []Target{
		{Provider: "pool#b", Parent: "pool"},
		{Provider: "pool#a", Parent: "pool"},
		{Provider: "fallback"},
	}
	input := ScheduleInput{
		Exposed:    "route",
		Targets:    targets,
		Now:        now,
		Commit:     true,
		Generation: 7,
	}
	first := m.DecideOrder(input)
	second := m.DecideOrder(input)
	if !reflect.DeepEqual(first.Order, []int{1, 0, 2}) ||
		!reflect.DeepEqual(second.Order, []int{0, 1, 2}) {
		t.Fatalf("committed pool spread = first %v second %v", first.Order, second.Order)
	}

	input.Commit = false
	peek1 := m.DecideOrder(input)
	peek2 := m.DecideOrder(input)
	if !reflect.DeepEqual(peek1.Order, []int{1, 0, 2}) ||
		!reflect.DeepEqual(peek2.Order, peek1.Order) {
		t.Fatalf("Commit=false advanced spread: %v then %v", peek1.Order, peek2.Order)
	}

	input.Commit = true
	input.Generation = 6
	stale1 := m.DecideOrder(input)
	stale2 := m.DecideOrder(input)
	if !reflect.DeepEqual(stale1.Order, []int{1, 0, 2}) ||
		!reflect.DeepEqual(stale2.Order, stale1.Order) {
		t.Fatalf("stale generation advanced spread: %v then %v", stale1.Order, stale2.Order)
	}
}

func TestDecideOrderPoolSpreadStaysWithinWinningRank(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 18, 15, 0, 0, time.UTC)
	t.Run("lower-ranked pool cannot leapfrog non-pool winner", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(12)
		setScheduleQuota(t, m, "plan", scheduleQuota(provider.BillingPlan, .2, now), 12)
		// The pool loses on the MEASURED tier (a real pay-as-you-go balance
		// snapshot, not a config label): tier 2 vs tier 0 despite the higher
		// surplus and better priority.
		setScheduleQuota(t, m, "pool#a", scheduleQuota(provider.BillingPayG, .9, now), 12)

		result := m.DecideOrder(ScheduleInput{
			Exposed: "route", Now: now, QuotaMaxAge: time.Hour,
			Commit: true, Generation: 12,
			Targets: []Target{
				{Provider: "pool#a", Parent: "pool", Priority: 1},
				{Provider: "plan", Priority: 9},
			},
		})
		if !reflect.DeepEqual(result.Order, []int{1, 0}) {
			t.Fatalf("mixed pool order = %v, want measured plan before payg pool", result.Order)
		}
		if got := m.Dashboard(now).spread["pool"]; got != 0 {
			t.Fatalf("losing pool advanced spread=%d, want 0", got)
		}
	})

	t.Run("pool rotation excludes lower tier and priority siblings", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(13)
		setScheduleQuota(t, m, "pool#a", scheduleQuota(provider.BillingPlan, .3, now), 13)
		setScheduleQuota(t, m, "pool#c", scheduleQuota(provider.BillingPlan, .8, now), 13)

		input := ScheduleInput{
			Exposed: "route", Now: now, QuotaMaxAge: time.Hour,
			Commit: true, Generation: 13,
			Targets: []Target{
				{Provider: "pool#b", Parent: "pool", Priority: 1}, // unknown tier
				{Provider: "pool#c", Parent: "pool", Priority: 2}, // lower priority
				{Provider: "pool#a", Parent: "pool", Priority: 1}, // winning band
			},
		}
		first := m.DecideOrder(input)
		second := m.DecideOrder(input)
		if !reflect.DeepEqual(first.Order, []int{2, 1, 0}) ||
			!reflect.DeepEqual(second.Order, first.Order) {
			t.Fatalf("rank-limited pool spread = first %v second %v, want stable [2 1 0]", first.Order, second.Order)
		}
	})
}

func TestDecideOrderEvictsOnlyCommittedCurrentStaleSticky(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	m := newTestManager(10)
	old := Sticky{Provider: "p", Since: now.Add(-2 * time.Hour)}
	m.SetSticky("old-session", old, 10)
	m.SetSticky("route", old, 10)
	input := ScheduleInput{
		Exposed:    "route",
		Targets:    []Target{{Provider: "p"}},
		RouteKeys:  map[string]bool{"route": true},
		Dwell:      time.Hour,
		Now:        now,
		Generation: 10,
	}

	m.DecideOrder(input)
	if _, ok := m.Sticky("old-session"); !ok {
		t.Fatal("Commit=false evicted stale sticky")
	}
	input.Commit = true
	input.Generation = 9
	m.DecideOrder(input)
	if _, ok := m.Sticky("old-session"); !ok {
		t.Fatal("stale generation evicted sticky")
	}
	input.Generation = 10
	m.DecideOrder(input)
	if _, ok := m.Sticky("old-session"); ok {
		t.Fatal("committed current generation retained stale session sticky")
	}
	if _, ok := m.Sticky("route"); !ok {
		t.Fatal("route sticky was evicted")
	}
	if _, ok := m.Sticky("new"); ok {
		t.Fatal("DecideOrder unexpectedly wrote sticky")
	}
}

func BenchmarkDecideOrder(b *testing.B) {
	now := time.Date(2026, 7, 29, 22, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	for _, name := range []string{"a", "b", "c", "d"} {
		m.SetQuota(name, scheduleQuota(provider.BillingPlan, .5, now), 1)
	}
	input := ScheduleInput{
		Exposed:     "route",
		Now:         now,
		QuotaMaxAge: time.Hour,
		Generation:  1,
		Targets: []Target{
			{Provider: "a", Priority: 1, PeakMultiplier: 1},
			{Provider: "b", Priority: 1, PeakMultiplier: 2},
			{Provider: "c", Priority: 2, PeakMultiplier: 1},
			{Provider: "d", Priority: 2, PeakMultiplier: 1},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m.DecideOrder(input)
	}
}

// TestStickyKey pins the sticky-map key derivation: sessionless traffic parks
// on the bare route key; a session's binding is scoped to (session, route) so
// one model's binding never drags another.
func TestStickyKey(t *testing.T) {
	t.Parallel()
	if got := StickyKey("", "route"); got != "route" {
		t.Errorf("sessionless key = %q, want bare route", got)
	}
	if got := StickyKey("s1", "route"); got != "s1\x1froute" {
		t.Errorf("session key = %q, want s1\\x1froute", got)
	}
}

// TestDecideOrderStickyPerRouteBinding pins the (session, route) binding
// granularity (Bifrost semantics: the binding is recorded against the model
// the caller requested): a session parked on one route's provider starts an
// INDEPENDENT binding when it calls another route — no dragging the previous
// account across models — and each binding dwells and recovers on its own.
func TestDecideOrderStickyPerRouteBinding(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	m := newTestManager(3)
	// Route "alpha" ranks shared first, route "beta" has its own targets.
	setScheduleQuota(t, m, "alpha-a", scheduleQuota(provider.BillingPlan, 1, now), 3)
	setScheduleQuota(t, m, "alpha-b", scheduleQuota(provider.BillingPlan, .5, now), 3)
	setScheduleQuota(t, m, "beta-best", scheduleQuota(provider.BillingPlan, 1, now), 3)
	alpha := []Target{{Provider: "alpha-a", Priority: 1}, {Provider: "alpha-b", Priority: 1}}
	beta := []Target{{Provider: "beta-best", Priority: 1}}
	base := ScheduleInput{
		SessionKey:  "sess",
		RouteKeys:   map[string]bool{"alpha": true, "beta": true},
		Dwell:       time.Hour,
		Now:         now,
		QuotaMaxAge: time.Hour,
		Generation:  3,
	}

	first := base
	first.Exposed, first.Targets = "alpha", alpha
	if result := m.DecideOrder(first); result.StickyProvider != "alpha-a" || result.Source != ScheduleSourceQuota {
		t.Fatalf("alpha first pick = %+v, want alpha-a/quota", result)
	}
	m.SetSticky(StickyKey("sess", "alpha"), Sticky{Provider: "alpha-a", Since: now}, 3)

	// Same session, different route: fresh binding, unrelated provider set.
	second := base
	second.Exposed, second.Targets = "beta", beta
	if result := m.DecideOrder(second); result.StickyProvider != "beta-best" || result.Source != ScheduleSourceQuota {
		t.Fatalf("beta first pick = %+v, want beta-best/quota (no cross-route drag)", result)
	}
	m.SetSticky(StickyKey("sess", "beta"), Sticky{Provider: "beta-best", Since: now}, 3)

	// Both bindings coexist and each route returns to its own within dwell.
	returned := base
	returned.Now = now.Add(10 * time.Minute)
	returned.Exposed, returned.Targets = "alpha", alpha
	if result := m.DecideOrder(returned); result.Order[0] != 0 || result.StickyProvider != "alpha-a" ||
		result.Source != ScheduleSourceSticky {
		t.Fatalf("alpha return = %+v, want alpha-a/sticky", result)
	}
	returned.Exposed, returned.Targets = "beta", beta
	if result := m.DecideOrder(returned); result.Order[0] != 0 || result.StickyProvider != "beta-best" ||
		result.Source != ScheduleSourceSticky {
		t.Fatalf("beta return = %+v, want beta-best/sticky", result)
	}
	if got := CountRouteSessionSticky(m.Dashboard(now.Add(10*time.Minute)).Sticky, "alpha", now.Add(10*time.Minute), time.Hour); got != 1 {
		t.Fatalf("alpha live session bindings = %d, want 1", got)
	}
	if got := CountRouteSessionSticky(m.Dashboard(now.Add(10*time.Minute)).Sticky, "beta", now.Add(10*time.Minute), time.Hour); got != 1 {
		t.Fatalf("beta live session bindings = %d, want 1", got)
	}
	if got := CountRouteSessionSticky(m.Dashboard(now.Add(10*time.Minute)).Sticky, "gamma", now.Add(10*time.Minute), time.Hour); got != 0 {
		t.Fatalf("gamma live session bindings = %d, want 0", got)
	}
}

// TestDecideOrderIgnoreSticky pins the x-mp-sticky: off semantics: the
// existing binding is read as absent (fresh pick), no binding is written or
// refreshed, and the stale entry is left to the dwell sweep.
func TestDecideOrderIgnoreSticky(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	m := newTestManager(5)
	targets := []Target{{Provider: "best", Priority: 1}, {Provider: "parked", Priority: 1}}
	setScheduleQuota(t, m, "best", scheduleQuota(provider.BillingPlan, 1, now), 5)
	setScheduleQuota(t, m, "parked", scheduleQuota(provider.BillingPlan, .5, now), 5)
	m.SetSticky(StickyKey("sess", "route"), Sticky{Provider: "parked", Since: now}, 5)

	input := ScheduleInput{
		Exposed: "route", SessionKey: "sess", Targets: targets,
		RouteKeys: map[string]bool{"route": true},
		Dwell:     time.Hour, Now: now, QuotaMaxAge: time.Hour, Generation: 5,
	}
	result := m.DecideOrder(input)
	if result.Order[0] != 1 || result.StickyProvider != "parked" || result.Source != ScheduleSourceSticky {
		t.Fatalf("affinity baseline = %+v, want parked/sticky", result)
	}

	input.IgnoreSticky = true
	input.Now = now.Add(time.Minute)
	result = m.DecideOrder(input)
	if result.Order[0] != 0 || result.StickyProvider != "" || result.Source != ScheduleSourceQuota {
		t.Fatalf("ignore-sticky result = %+v, want best/quota with no binding report", result)
	}
	// The pre-existing entry survives untouched (same Since), for the sweep.
	if entry, ok := m.Sticky(StickyKey("sess", "route")); !ok || entry.Provider != "parked" || !entry.Since.Equal(now) {
		t.Fatalf("binding after ignore = %+v (ok=%v), want parked untouched at %v", entry, ok, now)
	}
}

// TestDecideOrderSources pins the schedule-source vocabulary: pin dominates
// sticky, an all-blocked chain reports no source.
func TestDecideOrderSources(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	m := newTestManager(9)
	targets := []Target{{Provider: "best", Priority: 1}, {Provider: "parked", Priority: 1}}
	setScheduleQuota(t, m, "best", scheduleQuota(provider.BillingPlan, 1, now), 9)
	setScheduleQuota(t, m, "parked", scheduleQuota(provider.BillingPlan, .5, now), 9)
	input := ScheduleInput{
		Exposed: "route", Targets: targets,
		RouteKeys: map[string]bool{"route": true},
		Dwell:     time.Hour, Now: now, QuotaMaxAge: time.Hour, Generation: 9,
	}
	if result := m.DecideOrder(input); result.Source != ScheduleSourceQuota {
		t.Fatalf("fresh pick source = %q, want quota", result.Source)
	}

	// Pin narrowing: operator intent outranks whatever the engines picked.
	m.SetPin("route", Pin{Provider: "parked"})
	if result := m.DecideOrder(input); result.Order[0] != 1 || result.Source != ScheduleSourcePin {
		t.Fatalf("pinned source = %q (order %v), want pin on parked", result.Source, result.Order)
	}
	m.ClearPin("route")

	// All targets blocked: no schedulable decision, no source.
	m.mu.Lock()
	m.health["best"] = &providerHealth{circuitOpenUntil: now.Add(time.Hour)}
	m.health["parked"] = &providerHealth{circuitOpenUntil: now.Add(time.Hour)}
	m.mu.Unlock()
	if result := m.DecideOrder(input); len(result.Order) != 0 || result.Source != "" || result.StickyProvider != "" {
		t.Fatalf("all-blocked result = %+v, want empty order and no source", result)
	}
}
