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
		{"no reset → can't pace", &QuotaSnapshot{Billing: BillingPlan, RemainingPct: 0.5,
			Windows: []QuotaWindow{{Ultimate: true, RemainingPct: 0.5, Total: 200, Duration: dur}}}, 1, 0},
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

func TestQuotaOrUnknown(t *testing.T) {
	cfg := &Config{}
	got, err := cfg.QuotaOrUnknown()
	if err != nil {
		t.Fatalf("nil QuotaFn should not error, got %v", err)
	}
	if got.Billing != BillingUnknown {
		t.Errorf("nil QuotaFn → Billing %v, want BillingUnknown", got.Billing)
	}
}
