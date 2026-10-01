package runtime

import (
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// quota_eta_test.go covers the tracker's exhaustion-prediction wiring: the ETA
// is computed at commit time from the PREVIOUS committed snapshot and attached
// to the new Manager-held snapshot (PollAll/MergeQuotas and CommitSnapshot
// paths). Rate math itself is covered in the provider package.

// etaWindow serves the ETA fixture shape: one ultimate weekly window whose
// Used the test advances between polls (rem = 1 − used/1000).
func etaWindow(used *float64) func() (*provider.QuotaSnapshot, error) {
	return func() (*provider.QuotaSnapshot, error) {
		rem := 1 - *used/1000
		return &provider.QuotaSnapshot{
			Billing: provider.BillingPlan, RemainingPct: rem,
			Windows: []provider.QuotaWindow{{
				Label: "Weekly tokens", Kind: "tokens", Used: *used, Total: 1000,
				RemainingPct: rem, Ultimate: true,
				Duration: 7 * 24 * time.Hour, ResetsAt: time.Now().Add(7 * 24 * time.Hour),
			}},
		}, nil
	}
}

func newEtaTracker(p *quotaFake) *QuotaTracker {
	return NewQuotaTracker("",
		func() *configdomain.Config { return &configdomain.Config{} }, // default 5m poll interval → 15m max gap
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": p} },
		newTestManager(0))
}

func TestPollAll_ComputesExhaustionEta(t *testing.T) {
	used := 100.0
	p := &quotaFake{quota: etaWindow(&used)}
	tr := newEtaTracker(p)
	t0 := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	// First snapshot: no baseline → no prediction.
	tr.PollAll(t0)
	if s := tr.Snapshot("x"); s == nil || !s.ExhaustionEta.IsZero() {
		t.Fatalf("first poll: snapshot = %+v, want zero ExhaustionEta", s)
	}

	// Second poll 5m later, +100 used → rate 100/300s, remaining 800 → +40m.
	used = 200
	t1 := t0.Add(5 * time.Minute)
	tr.PollAll(t1)
	s := tr.Snapshot("x")
	if s == nil || s.ExhaustionEta.IsZero() {
		t.Fatalf("second poll: snapshot = %+v, want ExhaustionEta", s)
	}
	if d := s.ExhaustionEta.Sub(t1.Add(40 * time.Minute)); d < -time.Second || d > time.Second {
		t.Errorf("ExhaustionEta = %v, want ~%v", s.ExhaustionEta, t1.Add(40*time.Minute))
	}

	// Poll gap > 3×poll_interval (15m): stale baseline → no prediction.
	used = 300
	tr.PollAll(t1.Add(20 * time.Minute))
	if s := tr.Snapshot("x"); s == nil || !s.ExhaustionEta.IsZero() {
		t.Errorf("gapped poll: snapshot = %+v, want zero ExhaustionEta", s)
	}

	// Flat usage (rate 0) → no prediction, and a valid in-gap baseline.
	used = 300
	tr.PollAll(t1.Add(25 * time.Minute))
	if s := tr.Snapshot("x"); s == nil || !s.ExhaustionEta.IsZero() {
		t.Errorf("flat poll: snapshot = %+v, want zero ExhaustionEta", s)
	}
}

// CommitSnapshot (PollOne / 429 RefreshOne path) attaches the ETA the same way.
func TestCommitSnapshot_ComputesExhaustionEta(t *testing.T) {
	used := 0.0
	p := &quotaFake{quota: etaWindow(&used)}
	tr := newEtaTracker(p)
	t0 := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	mk := func(u float64, asOf time.Time) *provider.QuotaSnapshot {
		used = u
		s, _ := p.quota()
		s.AsOf = asOf
		return s
	}
	if !tr.CommitSnapshot(0, "x", mk(100, t0)) {
		t.Fatal("first commit rejected")
	}
	if s := tr.Snapshot("x"); !s.ExhaustionEta.IsZero() {
		t.Fatalf("first commit: eta = %v, want zero", s.ExhaustionEta)
	}
	if !tr.CommitSnapshot(0, "x", mk(200, t0.Add(5*time.Minute))) {
		t.Fatal("second commit rejected")
	}
	if s := tr.Snapshot("x"); s.ExhaustionEta.IsZero() {
		t.Fatal("second commit: no ExhaustionEta")
	}
	// Stale generation → rejected, snapshot untouched.
	if tr.CommitSnapshot(7, "x", mk(300, t0.Add(10*time.Minute))) {
		t.Error("stale-generation commit accepted")
	}
	if s := tr.Snapshot("x"); s.Windows[0].Used != 200 {
		t.Errorf("stale commit mutated quota: used = %v, want 200", s.Windows[0].Used)
	}
}
