package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// quota_eta_test.go covers EstimateExhaustionEta (rate math + no-prediction
// boundaries), the CLI baseline decoration (DecorateExhaustionEta reading the
// daemon-persisted quota_state.json), and the usage-display hint rendering.

func etaWindow(used, total float64) QuotaWindow {
	return QuotaWindow{
		Label: "Weekly tokens", Kind: "tokens", Used: used, Total: total,
		RemainingPct: 1 - used/total, Ultimate: true,
		Duration: 7 * 24 * time.Hour, ResetsAt: time.Now().Add(7 * 24 * time.Hour),
	}
}

func TestEstimateExhaustionEta(t *testing.T) {
	t0 := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	prev := &QuotaSnapshot{Billing: BillingPlan, AsOf: t0, Windows: []QuotaWindow{etaWindow(100, 1000)}}
	cur := &QuotaSnapshot{Billing: BillingPlan, AsOf: t0.Add(5 * time.Minute), Windows: []QuotaWindow{etaWindow(200, 1000)}}

	// rate = 100/300s; remaining = 800 → eta = cur.AsOf + 2400s.
	got := EstimateExhaustionEta(prev, cur, 15*time.Minute)
	want := cur.AsOf.Add(40 * time.Minute)
	if d := got.Sub(want); d < -time.Second || d > time.Second {
		t.Errorf("eta = %v, want %v (±1s)", got, want)
	}

	cases := []struct {
		name      string
		prev, cur *QuotaSnapshot
		maxGap    time.Duration
	}{
		{"first snapshot, no baseline", nil, cur, 15 * time.Minute},
		{"prev error", &QuotaSnapshot{AsOf: t0, Err: "boom", Windows: prev.Windows}, cur, 15 * time.Minute},
		{"cur error", prev, &QuotaSnapshot{AsOf: cur.AsOf, Err: "boom", Windows: cur.Windows}, 15 * time.Minute},
		{"flat usage (rate 0)", prev, &QuotaSnapshot{AsOf: cur.AsOf, Windows: []QuotaWindow{etaWindow(100, 1000)}}, 15 * time.Minute},
		{"usage dropped (window reset)", prev, &QuotaSnapshot{AsOf: cur.AsOf, Windows: []QuotaWindow{etaWindow(50, 1000)}}, 15 * time.Minute},
		{"poll gap > maxGap", prev, &QuotaSnapshot{AsOf: t0.Add(20 * time.Minute), Windows: []QuotaWindow{etaWindow(200, 1000)}}, 15 * time.Minute},
		{"non-positive dt", &QuotaSnapshot{AsOf: t0, Windows: prev.Windows}, &QuotaSnapshot{AsOf: t0, Windows: []QuotaWindow{etaWindow(200, 1000)}}, 15 * time.Minute},
		{"already exhausted", prev, &QuotaSnapshot{AsOf: cur.AsOf, Windows: []QuotaWindow{etaWindow(1000, 1000)}}, 15 * time.Minute},
		{"no ultimate window", &QuotaSnapshot{AsOf: t0, Windows: []QuotaWindow{{Label: "Balance", Kind: "money", Total: 10, RemainingPct: -1}}},
			&QuotaSnapshot{AsOf: cur.AsOf, Windows: []QuotaWindow{{Label: "Balance", Kind: "money", Total: 10, RemainingPct: -1}}}, 15 * time.Minute},
		{"unmeasured ultimate", prev, &QuotaSnapshot{AsOf: cur.AsOf, Windows: []QuotaWindow{{
			Label: "Weekly", Kind: "tokens", Used: 200, Total: 1000, RemainingPct: -1, Ultimate: true}}}, 15 * time.Minute},
		{"missing AsOf", &QuotaSnapshot{Windows: prev.Windows}, cur, 15 * time.Minute},
	}
	for _, tc := range cases {
		if got := EstimateExhaustionEta(tc.prev, tc.cur, tc.maxGap); !got.IsZero() {
			t.Errorf("%s: eta = %v, want zero (no prediction)", tc.name, got)
		}
	}
}

func TestDecorateExhaustionEta(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	asOf := time.Now().Add(-10 * time.Minute).UTC()
	body := fmt.Sprintf(`{"providers":{"zhipu":{"billing":1,"remaining_pct":0.9,"as_of":%q,"windows":[`+
		`{"Label":"Weekly tokens","Kind":"tokens","Used":100,"Total":1000,"RemainingPct":0.9,"Ultimate":true}`+
		`]}}}`, asOf.Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(stateDir, "quota_state.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Live-fetched CLI snapshot: no AsOf (parsers don't set it), used 300.
	// rate = 200/600s → remaining 700 → eta ≈ now + 35m.
	s := &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{etaWindow(300, 1000)}}
	DecorateExhaustionEta("zhipu", s)
	if s.ExhaustionEta.IsZero() {
		t.Fatal("DecorateExhaustionEta: no prediction from a valid persisted baseline")
	}
	if d := time.Until(s.ExhaustionEta); d < 34*time.Minute || d > 36*time.Minute {
		t.Errorf("eta in %v, want ~35m", d)
	}

	// Unknown provider / missing file / error snapshot → no prediction, no error.
	noBase := &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{etaWindow(300, 1000)}}
	DecorateExhaustionEta("deepseek", noBase)
	if !noBase.ExhaustionEta.IsZero() {
		t.Errorf("unknown provider: eta = %v, want zero", noBase.ExhaustionEta)
	}
	errSnap := &QuotaSnapshot{Err: "not logged in"}
	DecorateExhaustionEta("zhipu", errSnap)
	if !errSnap.ExhaustionEta.IsZero() {
		t.Errorf("error snapshot: eta = %v, want zero", errSnap.ExhaustionEta)
	}

	// Stale persisted baseline (as_of 1h ago > 15m gap) → no prediction.
	stale := &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{etaWindow(300, 1000)}}
	stale.AsOf = time.Now()
	old := EstimateExhaustionEta(&QuotaSnapshot{AsOf: time.Now().Add(-time.Hour), Windows: stale.Windows}, stale, DefaultEtaMaxGap)
	if !old.IsZero() {
		t.Errorf("stale baseline: eta = %v, want zero", old)
	}
}

func TestPrintQuotaSnapshot_ExhaustionHint(t *testing.T) {
	s := &QuotaSnapshot{
		Billing: BillingPlan,
		Windows: []QuotaWindow{
			{Label: "5h tokens", Kind: "tokens", Used: 10, Total: 100, RemainingPct: 0.9, Short: true},
			etaWindow(600, 1000),
		},
		ExhaustionEta: time.Now().Add(40 * time.Minute),
	}
	out := captureProv(t, func() { printQuotaSnapshot(s) })
	if !contains(out, "按当前速率 ~") || !contains(out, "后耗尽") {
		t.Errorf("ultimate window line missing exhaustion hint:\n%s", out)
	}

	// No prediction → no hint.
	s.ExhaustionEta = time.Time{}
	out = captureProv(t, func() { printQuotaSnapshot(s) })
	if contains(out, "按当前速率") {
		t.Errorf("zero eta rendered a hint:\n%s", out)
	}
}
