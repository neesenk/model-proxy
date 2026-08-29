package runtime

import (
	"reflect"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

func TestDashboardCompleteSortedAndDetached(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	m := newTestManager(12)
	m.mu.Lock()
	m.health["open"] = &providerHealth{
		consecutiveFailures: 3,
		circuitOpenUntil:    now.Add(time.Hour),
		rateLimitedUntil:    now.Add(2 * time.Hour),
		rateLimitKind:       Daily,
	}
	m.health["half"] = &providerHealth{
		circuitOpenUntil: now.Add(-time.Hour),
		halfOpenInFlight: true,
	}
	m.health["closed"] = &providerHealth{}
	m.modelLocks[ModelKey{Provider: "open", Model: "z"}] = &modelLock{
		failures:    2,
		lockedUntil: now.Add(time.Hour),
	}
	m.modelLocks[ModelKey{Provider: "open", Model: "a"}] = &modelLock{
		failures:    1,
		lockedUntil: now.Add(2 * time.Hour),
	}
	m.modelLocks[ModelKey{Provider: "open", Model: "expired"}] = &modelLock{
		failures:    9,
		lockedUntil: now,
	}
	m.mu.Unlock()
	m.SetSticky("route", Sticky{Provider: "open", Since: now.Add(-time.Hour)}, 12)
	m.SetPin("route", Pin{Provider: "open"})
	m.SetPin("expired", Pin{Provider: "closed", ExpiresAt: now})
	m.SetQuota("open", &provider.QuotaSnapshot{
		Plan:    "paid",
		Notes:   []string{"note"},
		Windows: []provider.QuotaWindow{{Label: "weekly"}},
	}, 12)

	snapshot := m.Dashboard(now)
	if snapshot.Generation != 12 {
		t.Fatalf("dashboard generation = %d, want 12", snapshot.Generation)
	}
	open := snapshot.Providers["open"]
	if open.ConsecutiveFailures != 3 || open.CircuitState != "open" ||
		open.Available || open.RateLimitKind != Daily ||
		!open.CircuitOpenUntil.Equal(now.Add(time.Hour)) ||
		!open.RateLimitedUntil.Equal(now.Add(2*time.Hour)) ||
		open.HalfOpenInFlight {
		t.Fatalf("open provider status = %+v", open)
	}
	half := snapshot.Providers["half"]
	if half.CircuitState != "half_open" || half.Available || !half.HalfOpenInFlight {
		t.Fatalf("half-open provider status = %+v", half)
	}
	closed := snapshot.Providers["closed"]
	if closed.CircuitState != "closed" || !closed.Available {
		t.Fatalf("closed provider status = %+v", closed)
	}
	locks := snapshot.ModelLocks["open"]
	if len(locks) != 2 || locks[0].Model != "a" || locks[0].Failures != 1 ||
		locks[1].Model != "z" || locks[1].Failures != 2 {
		t.Fatalf("sorted active model locks = %+v", locks)
	}
	if !reflect.DeepEqual(snapshot.Sticky["route"], Sticky{
		Provider: "open",
		Since:    now.Add(-time.Hour),
	}) {
		t.Fatalf("dashboard sticky = %+v", snapshot.Sticky)
	}
	if len(snapshot.Pins) != 1 || snapshot.Pins["route"].Provider != "open" {
		t.Fatalf("dashboard pins = %+v", snapshot.Pins)
	}
	if snapshot.Quotas["open"].Plan != "paid" ||
		snapshot.Quotas["open"].Notes[0] != "note" ||
		snapshot.Quotas["open"].Windows[0].Label != "weekly" {
		t.Fatalf("dashboard quota = %+v", snapshot.Quotas["open"])
	}

	snapshot.Providers["open"] = ProviderStatus{}
	snapshot.ModelLocks["open"][0].Model = "mutated"
	snapshot.Sticky["route"] = Sticky{Provider: "mutated"}
	snapshot.Pins["route"] = Pin{Provider: "mutated"}
	snapshot.Quotas["open"].Plan = "mutated"
	snapshot.Quotas["open"].Notes[0] = "mutated"
	snapshot.Quotas["open"].Windows[0].Label = "mutated"

	again := m.Dashboard(now)
	if again.Providers["open"].ConsecutiveFailures != 3 ||
		again.ModelLocks["open"][0].Model != "a" ||
		again.Sticky["route"].Provider != "open" ||
		again.Pins["route"].Provider != "open" ||
		again.Quotas["open"].Plan != "paid" ||
		again.Quotas["open"].Notes[0] != "note" ||
		again.Quotas["open"].Windows[0].Label != "weekly" {
		t.Fatalf("dashboard snapshot aliases manager state: %+v", again)
	}
}

func TestDashboardPreviewMatchesNonCommittingDecisionAndIsFullyDetached(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 21, 0, 0, 0, time.UTC)
	m := newTestManager(13)
	quota := &provider.QuotaSnapshot{
		Billing: provider.BillingPlan, AsOf: now, RemainingPct: .8,
		Notes: []string{"quota note"},
		Windows: []provider.QuotaWindow{{
			Ultimate: true, RemainingPct: .8, Total: 100, ResetsAt: now, Duration: time.Hour,
			Details: []provider.QuotaDetail{{Label: "nested", Used: 1}},
		}},
	}
	if !m.SetQuota("pool#a", quota, 13) || !m.SetQuota("pool#b", quota, 13) {
		t.Fatal("failed to seed quotas")
	}
	m.SetSticky("session", Sticky{Provider: "pool#b", Since: now}, 13)
	m.mu.Lock()
	m.spread["pool"] = 7
	m.mu.Unlock()

	input := ScheduleInput{
		Exposed: "route", SessionKey: "session", Now: now.Add(time.Minute),
		QuotaMaxAge: time.Hour, Dwell: time.Hour, Commit: false, Generation: 13,
		Targets: []Target{{Provider: "pool#b", Parent: "pool"}, {Provider: "pool#a", Parent: "pool"}},
	}
	snapshot := m.Dashboard(now)
	preview := snapshot.PreviewOrder(input)
	direct := m.DecideOrder(input)
	if !reflect.DeepEqual(preview, direct) {
		t.Fatalf("preview = %+v, direct non-commit decision = %+v", preview, direct)
	}
	if got := m.Dashboard(now); got.spread["pool"] != 7 || got.Sticky["session"].Provider != "pool#b" {
		t.Fatalf("non-committing schedule mutated manager: spread=%d sticky=%+v", got.spread["pool"], got.Sticky)
	}

	// Mutate every nested and private reference exposed by the detached snapshot.
	snapshot.spread["pool"] = 99
	snapshot.Quotas["pool#a"].Notes[0] = "mutated"
	snapshot.Quotas["pool#a"].Windows[0].Details[0].Label = "mutated"
	snapshot.Quotas["pool#a"].Windows[0].Details[0].Used = 99
	snapshot.Sticky["session"] = Sticky{Provider: "mutated"}

	again := m.Dashboard(now)
	if again.spread["pool"] != 7 || again.Quotas["pool#a"].Notes[0] != "quota note" ||
		again.Quotas["pool#a"].Windows[0].Details[0].Label != "nested" ||
		again.Quotas["pool#a"].Windows[0].Details[0].Used != 1 ||
		again.Sticky["session"].Provider != "pool#b" {
		t.Fatalf("dashboard snapshot aliases manager state: %+v", again)
	}
}
