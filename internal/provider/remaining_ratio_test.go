package provider

// remainingRatio is the shared fail-closed quota fraction: every provider
// parser routes its remaining/limit computation through it so the volcengine
// ±Inf class (fuzz seed Quota 1e308 / Used -1e308) cannot reappear on another
// provider's formula. These tests pin the helper contract plus one
// parser-level case per call site (over-quota clamp and/or overflow).

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestRemainingRatioContract(t *testing.T) {
	// Runtime evaluation: the seed's subtraction overflows float64 and must
	// not be folded by the compiler as an unrepresentable constant.
	volcSeed := 1e308
	volcSeed -= -1e308
	cases := []struct {
		name             string
		remaining, limit float64
		want             float64
	}{
		{"normal fraction", 30, 100, 0.3},
		{"zero limit is unmeasured", 30, 0, -1},
		{"negative limit is unmeasured", 30, -5, -1},
		{"over-quota clamps to exhausted", -20, 100, 0},
		{"numerator +Inf reads unmeasured", math.Inf(1), 1e308, -1},
		{"NaN reads unmeasured", math.NaN(), 100, -1},
		{"volcengine seed shape 1e308-(-1e308)", volcSeed, 1e308, -1},
		{"finite over-1 stays (upstream negative Used)", 150, 100, 1.5},
	}
	for _, tc := range cases {
		if got := remainingRatio(tc.remaining, tc.limit); got != tc.want {
			t.Errorf("%s: remainingRatio(%v, %v) = %v, want %v", tc.name, tc.remaining, tc.limit, got, tc.want)
		}
	}
}

// TestAqpQuotaOverflowsToUnmeasured: a Balance/TotalAmount ratio that
// overflows the division (huge balance over a tiny positive total) must read
// as -1 (unmeasured), and a negative balance (overspent) clamps to 0
// exhausted — not a negative that downstream reads as "unmeasured", which
// would lose the stronger exhausted signal.
func TestAqpQuotaOverflowsToUnmeasured(t *testing.T) {
	s := ParseAqpQuota(&MonthlyProjectUsage{
		SelectedYear: 2026, SelectedMonth: 7, TotalAmount: 1e-300, Balance: 1e308, Plan: "CQP",
	}, "a@example.com")
	if s.RemainingPct != -1 {
		t.Errorf("overflowing Balance/TotalAmount: RemainingPct = %v, want -1 (unmeasured; pre-fix +Inf flowed into the scheduling base)", s.RemainingPct)
	}
	overspent := ParseAqpQuota(&MonthlyProjectUsage{
		SelectedYear: 2026, SelectedMonth: 7, TotalAmount: 100, Balance: -5, Plan: "CQP",
	}, "a@example.com")
	if overspent.RemainingPct != 0 {
		t.Errorf("negative balance: RemainingPct = %v, want 0 (exhausted, not unmeasured)", overspent.RemainingPct)
	}
}

// TestKimiMonthlyCapOverLimitClampsToExhausted: used beyond the monthly cap
// must read as 0 remaining, not a negative (which display/scheduling treat as
// unmeasured — the exact confusion the -1 convention forbids).
func TestKimiMonthlyCapOverLimitClampsToExhausted(t *testing.T) {
	body := []byte(`{
		"usage": {"name":"Weekly limit","used":400,"limit":1000,"resetAt":"2026-08-01T00:00:00Z"},
		"limits": [
			{"detail":{"used":10,"limit":100,"name":"5h limit","remaining":90,"resetAt":"2026-07-17T15:00:00Z"},"window":{"duration":300,"timeUnit":"MINUTE"}}
		],
		"boosterWallet": {
			"balance":{"type":"BOOSTER","amount":100000000,"amountLeft":80000000},
			"monthlyChargeLimit":{"priceInCents":10000,"currency":"CNY"},
			"monthlyUsed":{"priceInCents":15000,"currency":"CNY"},
			"monthlyChargeLimitEnabled": true
		}
	}`)
	s, err := ParseKimiCodeQuota(body, "")
	if err != nil || s == nil {
		t.Fatalf("ParseKimiCodeQuota: err=%v s=%v", err, s)
	}
	cap := findWindowByLabel(t, s.Windows, "Monthly cap CNY ")
	if cap.RemainingPct != 0 {
		t.Errorf("over-limit monthly cap RemainingPct = %v, want 0 (exhausted)", cap.RemainingPct)
	}
}

// TestOpenRouterFreeModelAndCapOverflow: the two ratio windows (key credit
// cap, free-model daily) must fail closed when the division overflows (huge
// remaining over a tiny positive limit).
func TestOpenRouterFreeModelAndCapOverflow(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	s := ParseOpenRouterKeyQuota([]byte(`{"data":{
		"label":"k",
		"limit": 1e-300, "limit_remaining": 1e308,
		"free_model_daily_requests": {"used": 1, "limit": 1e-300, "remaining": 1e308}
	}}`), now)
	if s.Err != "" {
		t.Fatalf("parse error: %s", s.Err)
	}
	for _, w := range s.Windows {
		if strings.HasPrefix(w.Label, "Key credit cap") || w.Label == "Free-model req/day" {
			if w.RemainingPct != -1 {
				t.Errorf("%s: RemainingPct = %v, want -1 (unmeasured on overflow)", w.Label, w.RemainingPct)
			}
		}
	}
}
