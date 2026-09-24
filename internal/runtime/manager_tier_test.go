package runtime

import (
	"testing"
	"time"

	"model-proxy/internal/provider"
)

// tierOrderInput builds a DecideOrder input with the given targets at equal
// priority (index order is the tiebreak) and a generous quota freshness age.
func tierOrderInput(targets ...Target) ScheduleInput {
	return ScheduleInput{
		Exposed:     "m",
		RouteKeys:   map[string]bool{"m": true},
		Dwell:       time.Minute,
		Now:         time.Now(),
		QuotaMaxAge: time.Hour,
		Targets:     targets,
	}
}

// TestDecideOrderDeclaredBillingFillsUnmeasuredTier pins the tier rule
// "measurement wins, explicit declaration fills the gap, undeclared stays
// unknown": an unmeasured provider with `billing: plan` competes INSIDE the
// plan tier (ordered by priority/surplus with measured plans) instead of a
// middling unknown tier that only drains after every measured plan is
// exhausted; symmetrically a declared payg ranks with the paygs. A fresh
// measurement is never overridden by the label (the removed BillingOverride
// regression must not return).
func TestDecideOrderDeclaredBillingFillsUnmeasuredTier(t *testing.T) {
	m := &Manager{}
	now := time.Now()
	m.SetQuota("measured-plan", &provider.QuotaSnapshot{Billing: provider.BillingPlan, AsOf: now}, 0)
	m.SetQuota("measured-payg", &provider.QuotaSnapshot{Billing: provider.BillingPayG, AsOf: now}, 0)

	input := tierOrderInput(
		Target{Provider: "declared-payg", Billing: provider.BillingPayG, Priority: 1},
		Target{Provider: "undeclared", Priority: 1},
		Target{Provider: "measured-payg", Priority: 1},
		Target{Provider: "declared-plan", Billing: provider.BillingPlan, Priority: 1},
		Target{Provider: "measured-plan", Priority: 1},
	)
	names := targetNames(input)
	got := m.DecideOrder(input)
	order := make([]string, 0, len(got.Order))
	for _, index := range got.Order {
		order = append(order, names[index])
	}
	// plan tier: declared-plan + measured-plan (equal priority, both
	// surplus 0 → stable input order); unknown tier: undeclared; payg tier:
	// declared-payg + measured-payg. The CONTRACT is the tier grouping —
	// same-class members interleave by priority/surplus, measurability no
	// longer splits them.
	want := []string{"declared-plan", "measured-plan", "undeclared", "declared-payg", "measured-payg"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (tier = measurement ?? explicit declaration ?? unknown)", order, want)
		}
	}
}

// TestDecideOrderDeclaredBillingNeverOverridesMeasurement pins the
// BillingOverride regression: a provider whose measurement says payg stays
// payg even when the config label claims plan; display facts agree with the
// measurement.
func TestDecideOrderDeclaredBillingNeverOverridesMeasurement(t *testing.T) {
	m := &Manager{}
	now := time.Now()
	m.SetQuota("really-payg", &provider.QuotaSnapshot{Billing: provider.BillingPayG, AsOf: now}, 0)

	input := tierOrderInput(
		Target{Provider: "really-payg", Billing: provider.BillingPlan, Priority: 1},
		Target{Provider: "declared-plan", Billing: provider.BillingPlan, Priority: 2},
	)
	got := m.DecideOrder(input)
	if len(got.Order) != 2 || got.Order[0] != 1 {
		t.Fatalf("order = %v, want the declared-plan target first — the label must not override a measured payg", got.Order)
	}
	if got.Facts[0].Billing != provider.BillingPayG {
		t.Fatalf("facts billing = %v, want the measured payg", got.Facts[0].Billing)
	}
}

// TestDecideOrderStaleMeasurementFallsBackToDeclared pins the freshness
// interaction: a stale measurement degrades Facts.Billing to unknown (the
// honest display view) and the tier falls back to the declared class for
// ranking.
func TestDecideOrderStaleMeasurementFallsBackToDeclared(t *testing.T) {
	m := &Manager{}
	now := time.Now()
	m.SetQuota("stale-plan", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now.Add(-2 * time.Hour),
	}, 0)

	input := tierOrderInput(
		Target{Provider: "stale-plan", Billing: provider.BillingPayG, Priority: 1},
		Target{Provider: "undeclared", Priority: 2},
	)
	got := m.DecideOrder(input)
	if got.Facts[0].Billing != provider.BillingUnknown {
		t.Fatalf("stale snapshot facts billing = %v, want unknown (display stays measured-honest)", got.Facts[0].Billing)
	}
	if len(got.Order) != 2 || got.Order[0] != 1 {
		t.Fatalf("order = %v, want the undeclared (unknown tier) target before the stale plan whose declared class is payg", got.Order)
	}
}

func targetNames(input ScheduleInput) []string {
	names := make([]string, len(input.Targets))
	for i, target := range input.Targets {
		names[i] = target.Provider
	}
	return names
}
