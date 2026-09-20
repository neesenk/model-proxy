package provider

import (
	"testing"
	"time"
)

func TestSurplus(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	const dur = 7 * 24 * time.Hour
	ult := func(rem, fLeft float64) QuotaWindow {
		return QuotaWindow{Ultimate: true, Kind: "tokens", RemainingPct: rem, Total: 200,
			Duration: dur, ResetsAt: now.Add(time.Duration(fLeft * float64(dur)))}
	}
	short := QuotaWindow{Short: true, Kind: "tokens", RemainingPct: 0.6, Total: 100} // share = 100/200 = 0.5

	cases := []struct {
		name string
		snap *QuotaSnapshot
		mult float64
		want float64
	}{
		{"nil", nil, 1, 0},
		{"unknown billing", &QuotaSnapshot{Billing: BillingUnknown}, 1, 0},
		{"no ultimate", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{{RemainingPct: 0.5}}}, 1, 0},
		{"on pace", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.5, Windows: []QuotaWindow{ult(0.5, 0.5)}}, 1, 0},
		{"waste risk (last moment)", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.5, Windows: []QuotaWindow{ult(0.5, 0)}}, 1, 0.5},
		{"over pace (early, low remaining)", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.1, Windows: []QuotaWindow{ult(0.1, 0.9)}}, 1, -0.8},
		{"peak burns short window", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.5, Windows: []QuotaWindow{ult(0.5, 0.5), short}}, 2, -0.3},
		{"intermediate window ignored", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.5, Windows: []QuotaWindow{ult(0.5, 0.5), {RemainingPct: 0.1, Total: 999}}}, 1, 0},
		{"multiple shorts sum", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.5, Windows: []QuotaWindow{ult(0.5, 0.5), {Short: true, RemainingPct: 0.6, Total: 100}, {Short: true, RemainingPct: 0.4, Total: 50}}}, 2, -0.4},
		{"no reset -> can't pace", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.5,
			Windows: []QuotaWindow{{Ultimate: true, RemainingPct: 0.5, Total: 200, Duration: dur}}}, 1, 0},
		{"fLeft clamped low (reset in past)", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.2,
			Windows: []QuotaWindow{ult(0.2, -0.1)}}, 1, 0.2}, // fLeft clamped to 0 -> surplus = remaining
		{"fLeft clamped high (reset >1 cycle out)", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.2,
			Windows: []QuotaWindow{ult(0.2, 1.5)}}, 1, -0.8}, // fLeft clamped to 1 -> surplus = 0.2−1
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.Surplus(now, tc.mult); !approxEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func approxEqual(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

func TestUsageWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	// zhipu-shaped weekly plan: reset 3d out on a 7d cycle -> window started 4d ago.
	weekly := QuotaWindow{Ultimate: true, Kind: "tokens", RemainingPct: 0.4, Total: 200,
		Duration: 7 * 24 * time.Hour, ResetsAt: now.Add(3 * 24 * time.Hour)}
	// codex-shaped monthly plan: 30d cycle, reset 10d out -> started 20d ago.
	monthly := QuotaWindow{Ultimate: true, Kind: "money", RemainingPct: 0.7, Total: 100,
		Duration: 30 * 24 * time.Hour, ResetsAt: now.Add(10 * 24 * time.Hour)}
	short5h := QuotaWindow{Short: true, Kind: "tokens", RemainingPct: 0.5, Total: 20,
		Duration: 5 * time.Hour, ResetsAt: now.Add(2 * time.Hour)}

	cases := []struct {
		name string
		snap *QuotaSnapshot
		want time.Time
		ok   bool
	}{
		{"nil snapshot", nil, time.Time{}, false},
		{"non-plan billing", &QuotaSnapshot{Billing: BillingPayG, Windows: []QuotaWindow{weekly}}, time.Time{}, false},
		{"poll error", &QuotaSnapshot{Billing: BillingPlan, Err: "boom", Windows: []QuotaWindow{weekly}}, time.Time{}, false},
		{"no windows", &QuotaSnapshot{Billing: BillingPlan}, time.Time{}, false},
		{"no ultimate", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{short5h}}, time.Time{}, false},
		{"zero reset", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{{Ultimate: true, Duration: 7 * 24 * time.Hour}}}, time.Time{}, false},
		{"zero cycle (e.g. zhipu monthly time)", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{{Ultimate: true, ResetsAt: now.Add(24 * time.Hour)}}}, time.Time{}, false},
		{"reset >1 cycle out (broken data)", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{{Ultimate: true, Duration: 7 * 24 * time.Hour, ResetsAt: now.Add(10 * 24 * time.Hour)}}}, time.Time{}, false},
		{"weekly: shorts ignored, ultimate wins", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{short5h, weekly}}, now.Add(-4 * 24 * time.Hour), true},
		{"monthly 30d cycle", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{monthly}}, now.Add(-20 * 24 * time.Hour), true},
		{"stale snapshot (reset just passed)", &QuotaSnapshot{Billing: BillingPlan, Windows: []QuotaWindow{{Ultimate: true, Duration: 7 * 24 * time.Hour, ResetsAt: now.Add(-time.Hour)}}}, now.Add(-7 * 24 * time.Hour).Add(-time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.snap.UsageWindow(now)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("from = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStaticProviderQuota_Unknown: a static provider (no measurable quota)
// returns BillingUnknown, never panics. Replaces the old TestQuotaOrUnknown
// (QuotaFn/QuotaOrUnknown deleted in Phase 2 - each provider implements Quota()).
func TestStaticProviderQuota_Unknown(t *testing.T) {
	p := &StaticProvider{cfg: &Config{}}
	got, err := p.Quota()
	if err != nil {
		t.Fatalf("static Quota should not error, got %v", err)
	}
	if got.Billing != BillingUnknown {
		t.Errorf("static Quota -> Billing %v, want BillingUnknown", got.Billing)
	}
}
