package runtime

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

func TestRateLimitKindAndPin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind RateLimitKind
		want string
	}{
		{Transient, "transient"},
		{Quota, "quota"},
		{Daily, "daily"},
		{RateLimitKind(99), "transient"},
	}
	for _, test := range tests {
		if got := test.kind.String(); got != test.want {
			t.Fatalf("String(%d) = %q, want %q", test.kind, got, test.want)
		}
		if test.kind <= Daily {
			if got := ParseRateLimitKind(test.want); got != test.kind {
				t.Fatalf("ParseRateLimitKind(%q) = %d, want %d", test.want, got, test.kind)
			}
		}
	}
	if got := ParseRateLimitKind(" QUOTA "); got != Quota {
		t.Fatalf("case/space tolerant parse = %d, want quota", got)
	}
	if got := ParseRateLimitKind("legacy"); got != Transient {
		t.Fatalf("unknown parse = %d, want transient", got)
	}

	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	permanent := Pin{Provider: "a"}
	if !permanent.Active(now) || permanent.ExpiresLabel(now) != "" {
		t.Fatalf("permanent pin = active %v, label %q", permanent.Active(now), permanent.ExpiresLabel(now))
	}
	future := Pin{Provider: "a", ExpiresAt: now.Add(90 * time.Second)}
	if !future.Active(now) || future.ExpiresLabel(now) != "expires in 1m30s" {
		t.Fatalf("future pin = active %v, label %q", future.Active(now), future.ExpiresLabel(now))
	}
	expired := Pin{Provider: "a", ExpiresAt: now}
	if expired.Active(now) || expired.ExpiresLabel(now) != "expired" {
		t.Fatalf("expired pin = active %v, label %q", expired.Active(now), expired.ExpiresLabel(now))
	}
}

func TestZeroValueAndGenerationReplacement(t *testing.T) {
	t.Parallel()

	var zero Manager
	if zero.Generation() != 0 {
		t.Fatalf("zero generation = %d, want 0", zero.Generation())
	}
	if !zero.SetSticky("route", Sticky{Provider: "a"}, 0) {
		t.Fatal("generation zero must bypass gating")
	}
	if got, ok := zero.Sticky("route"); !ok || got.Provider != "a" {
		t.Fatalf("zero-value sticky = %+v, %v", got, ok)
	}
	if !zero.SetQuota("a", &provider.QuotaSnapshot{Plan: "zero"}, 0) {
		t.Fatal("zero-value quota set failed")
	}

	m := newTestManager(1)
	m.SetPin("route", Pin{Provider: "pinned"})
	if !m.SetSticky("route", Sticky{Provider: "old"}, 1) {
		t.Fatal("current sticky rejected")
	}
	m.RecordFailure("old", 1, time.Hour, 1)
	m.RecordModelFailure("old", "m", time.Hour, 1)
	if !m.LearnParamBlock("old", "m", "temperature", 1) {
		t.Fatal("parameter was not learned")
	}
	if !m.SetQuota("old", &provider.QuotaSnapshot{Plan: "old"}, 1) {
		t.Fatal("quota was not set")
	}
	if got := m.ResolverSpreadStart("pool", 2, 1); got != 0 {
		t.Fatalf("initial spread = %d, want 0", got)
	}

	m.ReplaceGeneration(2)
	if got := m.Generation(); got != 2 {
		t.Fatalf("generation = %d, want 2", got)
	}
	if _, ok := m.Sticky("route"); ok {
		t.Fatal("sticky survived generation replacement")
	}
	if got := m.Quotas(); len(got) != 0 {
		t.Fatalf("quotas survived replacement: %+v", got)
	}
	if got := m.ParamBlock("old", "m"); len(got) != 0 {
		t.Fatalf("param block survived replacement: %v", got)
	}
	dashboard := m.Dashboard(time.Now())
	if len(dashboard.Providers) != 0 || len(dashboard.ModelLocks) != 0 {
		t.Fatalf("health survived replacement: %+v", dashboard)
	}
	if pins := m.Pins(time.Now()); len(pins) != 1 || pins["route"].Provider != "pinned" {
		t.Fatalf("pin did not survive replacement: %+v", pins)
	}
	if got := m.ResolverSpreadStart("pool", 2, 2); got != 0 {
		t.Fatalf("spread did not reset: %d", got)
	}
}

func TestGenerationGatesStaleMutations(t *testing.T) {
	t.Parallel()

	m := newTestManager(8)
	if m.SetSticky("s", Sticky{Provider: "stale"}, 7) {
		t.Fatal("stale sticky mutation committed")
	}
	if m.SetQuota("stale", &provider.QuotaSnapshot{}, 7) {
		t.Fatal("stale quota mutation committed")
	}
	if m.MergeQuotas(map[string]*provider.QuotaSnapshot{"stale": {}}, 7) {
		t.Fatal("stale quota merge committed")
	}
	if m.ClearQuotas(7) {
		t.Fatal("stale quota clear committed")
	}
	if m.LearnParamBlock("stale", "m", "temperature", 7) {
		t.Fatal("stale param mutation committed")
	}
	m.RecordFailure("stale", 1, time.Hour, 7)
	m.RecordModelFailure("stale", "m", time.Hour, 7)
	m.RecordRateLimit("stale", time.Now().Add(time.Hour), Daily, 7)

	// Seed current-generation half-open/model state, then prove stale methods
	// neither reserve/release the slot nor clear the model lock.
	m.mu.Lock()
	m.health["current"] = &providerHealth{
		circuitOpenUntil: time.Now().Add(-time.Second),
	}
	m.modelLocks[ModelKey{Provider: "current", Model: "m"}] = &modelLock{
		failures:    1,
		lockedUntil: time.Now().Add(time.Hour),
	}
	m.mu.Unlock()
	if !m.TakeHalfOpenSlot("current", 7) {
		t.Fatal("stale take must let the old request finish")
	}
	m.mu.Lock()
	if m.health["current"].halfOpenInFlight {
		t.Fatal("stale take mutated half-open state")
	}
	m.health["current"].halfOpenInFlight = true
	m.mu.Unlock()
	m.ReleaseHalfOpenSlot("current", 7)
	m.RecordSuccess("current", "m", 7)

	m.mu.Lock()
	slot := m.health["current"].halfOpenInFlight
	_, locked := m.modelLocks[ModelKey{Provider: "current", Model: "m"}]
	_, staleHealth := m.health["stale"]
	m.mu.Unlock()
	if !slot || !locked || staleHealth {
		t.Fatalf("stale mutation leaked: slot=%v locked=%v staleHealth=%v", slot, locked, staleHealth)
	}

	// Generation zero is the explicit standalone/test bypass.
	if !m.SetQuota("bypass", &provider.QuotaSnapshot{Plan: "ok"}, 0) ||
		!m.SetSticky("bypass", Sticky{Provider: "ok"}, 0) ||
		!m.LearnParamBlock("bypass", "m", "temperature", 0) {
		t.Fatal("generation-zero bypass was rejected")
	}
}

func TestQuotaDetachmentAndGating(t *testing.T) {
	t.Parallel()

	m := newTestManager(3)
	input := &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		Account: "account",
		Plan:    "plan",
		Notes:   []string{"note"},
		Windows: []provider.QuotaWindow{{
			Label:   "weekly",
			Details: []provider.QuotaDetail{{Label: "model", Used: 2}},
		}},
	}
	if !m.SetQuota("a", input, 3) {
		t.Fatal("SetQuota rejected current generation")
	}
	facts := m.DecideOrder(ScheduleInput{
		Exposed: "route",
		Now:     time.Now(),
		Targets: []Target{{Provider: "a"}, {Provider: "missing"}},
	}).Facts
	if len(facts) != 2 || facts[0].Billing != provider.BillingUnknown ||
		facts[0].Surplus != 0 || facts[1].Billing != provider.BillingUnknown {
		t.Fatalf("scheduling quota facts = %+v", facts)
	}
	input.Plan = "mutated"
	input.Notes[0] = "mutated"
	input.Windows[0].Label = "mutated"
	input.Windows[0].Details[0].Label = "mutated"

	got := m.Quota("a")
	if got.Plan != "plan" || got.Notes[0] != "note" ||
		got.Windows[0].Label != "weekly" ||
		got.Windows[0].Details[0].Label != "model" {
		t.Fatalf("stored quota aliases input: %+v", got)
	}
	got.Plan = "output mutation"
	got.Notes[0] = "output mutation"
	got.Windows[0].Details[0].Used = 99
	again := m.Quota("a")
	if again.Plan != "plan" || again.Notes[0] != "note" ||
		again.Windows[0].Details[0].Used != 2 {
		t.Fatalf("Quota result aliases manager state: %+v", again)
	}

	all := m.Quotas()
	delete(all, "a")
	if m.Quota("a") == nil {
		t.Fatal("Quotas map aliases manager state")
	}
	if !m.MergeQuotas(map[string]*provider.QuotaSnapshot{
		"b": nil,
		"c": {Plan: "merged"},
	}, 3) {
		t.Fatal("MergeQuotas rejected current generation")
	}
	if m.Quota("b") != nil || m.Quota("c").Plan != "merged" {
		t.Fatalf("merged quotas incorrect: %+v", m.Quotas())
	}
	if !m.ClearQuotas(3) || len(m.Quotas()) != 0 {
		t.Fatal("ClearQuotas did not clear current state")
	}
}

func TestRestoreAndPersistSnapshotSemantics(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	m := newTestManager(11)
	m.RestoreSticky(map[string]Sticky{
		"route":   {Provider: "a", Since: now.Add(-time.Minute)},
		"session": {Provider: "b", Since: now.Add(-time.Minute)},
	})
	m.RestoreHealth(map[string]PersistedHealth{
		"active": {
			RateLimitedUntil: now.Add(time.Hour),
			RateLimitKind:    "daily",
			CircuitOpenUntil: now.Add(2 * time.Hour),
			ModelLocks: map[string]time.Time{
				"live":    now.Add(time.Hour),
				"expired": now.Add(-time.Hour),
			},
			ParamBlock: map[string][]string{
				"live": {"z", "a", "z"},
			},
		},
		"expired": {
			RateLimitedUntil: now.Add(-time.Hour),
			RateLimitKind:    "quota",
			CircuitOpenUntil: now.Add(-time.Hour),
			ModelLocks: map[string]time.Time{
				"old": now.Add(-time.Second),
			},
		},
		"param-only": {
			ParamBlock: map[string][]string{
				"m": {"temperature", "max_tokens"},
			},
		},
	}, now, 4)
	if !m.SetQuota("active", &provider.QuotaSnapshot{
		Plan:    "detached",
		Windows: []provider.QuotaWindow{{Label: "window"}},
	}, 11) {
		t.Fatal("quota set failed")
	}

	snapshot := m.SnapshotForPersist(map[string]bool{"route": true}, now)
	if snapshot.Generation != 11 {
		t.Fatalf("snapshot generation = %d", snapshot.Generation)
	}
	if len(snapshot.Sticky) != 1 || snapshot.Sticky["route"].Provider != "a" {
		t.Fatalf("persisted sticky = %+v, want route only", snapshot.Sticky)
	}
	active := snapshot.Health["active"]
	if active.RateLimitKind != "daily" ||
		!active.RateLimitedUntil.Equal(now.Add(time.Hour)) ||
		!active.CircuitOpenUntil.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("active health = %+v", active)
	}
	if _, ok := active.ModelLocks["expired"]; ok {
		// Restore drops expired model locks, so they cannot reappear in snapshot.
		t.Fatalf("expired restored model lock persisted: %+v", active.ModelLocks)
	}
	if got := active.ParamBlock["live"]; !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("params = %v, want sorted unique", got)
	}
	if got := snapshot.Health["param-only"].ParamBlock["m"]; !reflect.DeepEqual(got, []string{"max_tokens", "temperature"}) {
		t.Fatalf("param-only provider missing/unsorted: %v", got)
	}
	if _, ok := snapshot.Health["expired"]; ok {
		t.Fatalf("expired-only restore should be dropped: %+v", snapshot.Health["expired"])
	}

	// Only an active horizon carries its rate-limit kind. The expired horizon
	// itself remains durable until a later restore prunes it.
	m.RecordRateLimit("past-kind", now.Add(-time.Minute), Quota, 11)
	past := m.SnapshotForPersist(nil, now).Health["past-kind"]
	if past.RateLimitKind != "" || !past.RateLimitedUntil.Equal(now.Add(-time.Minute)) {
		t.Fatalf("expired rate state = %+v", past)
	}

	snapshot.Quotas["active"].Plan = "mutated"
	snapshot.Health["active"].ParamBlock["live"][0] = "mutated"
	snapshot.Sticky["route"] = Sticky{Provider: "mutated"}
	again := m.SnapshotForPersist(map[string]bool{"route": true}, now)
	if again.Quotas["active"].Plan != "detached" ||
		again.Health["active"].ParamBlock["live"][0] != "a" ||
		again.Sticky["route"].Provider != "a" {
		t.Fatalf("persist snapshot aliases manager state: %+v", again)
	}

	data, err := json.Marshal(PersistedHealth{
		RateLimitKind: "quota",
		ParamBlock:    map[string][]string{"m": {"temperature"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["rate_limit_kind"]; !ok {
		t.Fatalf("missing legacy JSON field: %s", data)
	}
	if _, ok := raw["param_block"]; !ok {
		t.Fatalf("missing legacy JSON field: %s", data)
	}
}

func TestAtomicSnapshotsNeverMixGenerations(t *testing.T) {
	m := newTestManager(1)
	routeKeys := map[string]bool{"route": true}

	const generations = 600
	start := make(chan struct{})
	done := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		<-start
		for generation := uint64(2); generation <= generations; generation++ {
			m.ReplaceGeneration(generation)
			name := generationName(generation)
			m.SetQuota(name, &provider.QuotaSnapshot{Plan: name}, generation)
			m.SetSticky("route", Sticky{Provider: name}, generation)
			m.LearnParamBlock(name, "m", name, generation)
		}
		close(done)
	}()

	close(start)
	for {
		snapshot := m.SnapshotForPersist(routeKeys, time.Now())
		want := generationName(snapshot.Generation)
		for name, quota := range snapshot.Quotas {
			if name != want || quota.Plan != want {
				t.Fatalf("mixed quota generation: generation=%d key=%q quota=%+v", snapshot.Generation, name, quota)
			}
		}
		if sticky, ok := snapshot.Sticky["route"]; ok && sticky.Provider != want {
			t.Fatalf("mixed sticky generation: generation=%d sticky=%+v", snapshot.Generation, sticky)
		}
		for name := range snapshot.Health {
			if name != want {
				t.Fatalf("mixed health generation: generation=%d health=%q", snapshot.Generation, name)
			}
		}
		select {
		case <-done:
			writer.Wait()
			final := m.SnapshotForPersist(routeKeys, time.Now())
			if final.Generation != generations {
				t.Fatalf("final generation = %d, want %d", final.Generation, generations)
			}
			if len(final.Quotas) == 0 || len(final.Sticky) == 0 || len(final.Health) == 0 {
				t.Fatalf(
					"final snapshot missing a state family: quotas=%d sticky=%d health=%d",
					len(final.Quotas),
					len(final.Sticky),
					len(final.Health),
				)
			}
			want := generationName(generations)
			if final.Quotas[want] == nil ||
				final.Sticky["route"].Provider != want ||
				final.Health[want].ParamBlock["m"][0] != want {
				t.Fatalf("final snapshot does not belong to generation %d: %+v", generations, final)
			}
			return
		default:
		}
	}
}

func generationName(generation uint64) string {
	const digits = "0123456789"
	if generation == 0 {
		return "g0"
	}
	var reversed [20]byte
	i := len(reversed)
	for generation > 0 {
		i--
		reversed[i] = digits[generation%10]
		generation /= 10
	}
	return "g" + string(reversed[i:])
}
