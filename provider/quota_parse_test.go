package provider

import (
	"testing"
	"time"
)

// quota_parse_test.go covers the quota parsers + shared helpers, moved from the
// main package in Phase 1 of the provider-impl migration. In package provider,
// the parsers are called directly (ParseXxxQuota) and the DTOs/types need no
// prefix.

func TestUltimateRemaining(t *testing.T) {
	if got := ultimateRemaining(nil); got != -1 {
		t.Errorf("ultimateRemaining(nil)=%v want -1", got)
	}
	ws := []QuotaWindow{
		{Label: "5h", RemainingPct: 0.5},
		{Label: "Monthly", RemainingPct: 0.8, Ultimate: true},
	}
	if got := ultimateRemaining(ws); got != 0.8 {
		t.Errorf("ultimateRemaining=%v want 0.8", got)
	}
	// No ultimate window -> -1.
	ws2 := []QuotaWindow{{Label: "5h", RemainingPct: 0.5}}
	if got := ultimateRemaining(ws2); got != -1 {
		t.Errorf("ultimateRemaining(no ultimate)=%v want -1", got)
	}
}

func TestZhipuLimitLabel(t *testing.T) {
	for _, tc := range []struct {
		typ, want string
		unit      int
	}{
		{"TOKENS_LIMIT", "5h tokens", 3},
		{"TOKENS_LIMIT", "Weekly tokens", 6},
		{"TOKENS_LIMIT", "Tokens (unit=9)", 9},
		{"TIME_LIMIT", "Monthly time", 5},
		{"TIME_LIMIT", "Time (unit=7)", 7},
		{"OTHER", "OTHER (unit=1)", 1},
	} {
		if got := zhipuLimitLabel(tc.typ, tc.unit); got != tc.want {
			t.Errorf("zhipuLimitLabel(%q,%d)=%q want %q", tc.typ, tc.unit, got, tc.want)
		}
	}
}

func TestParseZhipuQuota(t *testing.T) {
	body := []byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[
		{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000,"usageDetails":[{"modelCode":"glm-5.2","usage":30000},{"modelCode":"glm-5.1","usage":10000}]},
		{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1750000000000,"usage":200000,"currentValue":140000,"remaining":60000},
		{"type":"TIME_LIMIT","unit":5,"percentage":10,"nextResetTime":1750000000000,"usage":3600,"currentValue":360,"remaining":3240}
	]}}`)
	s, err := ParseZhipuQuota(body, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.Level != "GLM Coding Plan" {
		t.Errorf("Level=%q", s.Level)
	}
	// binding = min(5h rem 0.6, weekly rem 0.3) - TIME_LIMIT excluded.
	if s.RemainingPct != 0.3 {
		t.Errorf("RemainingPct=%v, want 0.3 (weekly is binding; TIME_LIMIT excluded)", s.RemainingPct)
	}
	if len(s.Windows) != 3 {
		t.Fatalf("got %d windows, want 3", len(s.Windows))
	}
	// 5h window has per-model details.
	var w5h *QuotaWindow
	for i := range s.Windows {
		if s.Windows[i].Label == "5h tokens" {
			w5h = &s.Windows[i]
		}
	}
	if w5h == nil || len(w5h.Details) != 2 {
		t.Errorf("5h window details: %+v", w5h)
	}
	// P0-2: assert Ultimate/Short markers - getting these wrong silently
	// breaks peak-burn, pacing, and sticky-switch.
	if !w5h.Short || w5h.Ultimate {
		t.Errorf("5h window: Short=%v Ultimate=%v, want Short=true Ultimate=false", w5h.Short, w5h.Ultimate)
	}
	if w5h.Duration != 5*time.Hour {
		t.Errorf("5h Duration=%v, want 5h", w5h.Duration)
	}
	if !w5h.ResetsAt.Equal(time.UnixMilli(1750000000000)) {
		t.Errorf("5h ResetsAt=%v, want 1750000000000ms", w5h.ResetsAt)
	}
	var wWeekly *QuotaWindow
	for i := range s.Windows {
		if s.Windows[i].Label == "Weekly tokens" {
			wWeekly = &s.Windows[i]
		}
	}
	if wWeekly == nil {
		t.Fatal("weekly window not found")
	}
	if !wWeekly.Ultimate || wWeekly.Short {
		t.Errorf("weekly: Ultimate=%v Short=%v, want Ultimate=true Short=false", wWeekly.Ultimate, wWeekly.Short)
	}
	if wWeekly.Duration != 7*24*time.Hour {
		t.Errorf("weekly Duration=%v, want 7d", wWeekly.Duration)
	}
}

func TestParseZhipuQuota_NotZhipu(t *testing.T) {
	// Non-zhipu JSON -> returns nil snapshot (caller falls back to model list).
	s, err := ParseZhipuQuota([]byte(`{"object":"list","data":[]}`), "")
	if err == nil && s != nil {
		t.Fatalf("expected nil snapshot for non-zhipu body, got %+v", s)
	}
}

func TestParseCodexQuota(t *testing.T) {
	body := []byte(`{"email":"a@b.com","plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":12000},
		"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_after_seconds":300000}},
		"spend_control":{"reached":false,"individual_limit":{"used":"5","limit":"20","remaining":"15","used_percent":25,"reset_after_seconds":2500000}}}`)
	before := time.Now()
	s, err := ParseCodexQuota(body, "a@b.com", "pro")
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.Account != "a@b.com" || s.Plan != "pro" {
		t.Errorf("Account/Plan=%q/%q", s.Account, s.Plan)
	}
	// ultimate = monthly spend -> RemainingPct = 0.75 (primary/weekly are token rate-caps, not ultimate).
	if s.RemainingPct != 0.75 {
		t.Errorf("RemainingPct=%v, want 0.75 (spend ultimate)", s.RemainingPct)
	}
	// P0-2: assert Ultimate marker on the spend window, and that primary/weekly
	// are NOT Ultimate/Short (different unit - money vs tokens).
	// ResetsAt must come from reset_after_seconds: a dropped assignment leaves it
	// zero, which silently zeroes Surplus() for scheduling.
	resetAfter := map[string]time.Duration{
		"Spend":        2500000 * time.Second,
		"primary (5h)": 12000 * time.Second,
		"weekly":       300000 * time.Second,
	}
	for _, w := range s.Windows {
		switch w.Label {
		case "Spend":
			if !w.Ultimate {
				t.Error("spend window must be Ultimate=true")
			}
			if w.Duration != 30*24*time.Hour {
				t.Errorf("spend Duration=%v, want 30d", w.Duration)
			}
		case "primary (5h)", "weekly":
			if w.Ultimate || w.Short {
				t.Errorf("%s: Ultimate=%v Short=%v - codex token windows must NOT be Ultimate/Short (money ultimate)", w.Label, w.Ultimate, w.Short)
			}
		}
		want, ok := resetAfter[w.Label]
		if !ok {
			t.Errorf("unexpected window label %q", w.Label)
			continue
		}
		if lo, hi := before.Add(want), time.Now().Add(want); w.ResetsAt.Before(lo) || w.ResetsAt.After(hi) {
			t.Errorf("%s: ResetsAt=%v, want within [%v, %v] (reset_after_seconds=%v)", w.Label, w.ResetsAt, lo, hi, want)
		}
	}
}

// TestParseCodexQuota_ZeroResetAfter: when reset_after_seconds is 0 (absent),
// ResetsAt must stay zero (not now), so it isn't misread as "just reset".
func TestParseCodexQuota_ZeroResetAfter(t *testing.T) {
	body := []byte(`{"email":"a@b.com","rate_limit":{"primary_window":{"used_percent":30,"reset_after_seconds":0}},
		"spend_control":{"individual_limit":{"used":"5","limit":"20","used_percent":25,"reset_after_seconds":0}}}`)
	s, err := ParseCodexQuota(body, "a@b.com", "pro")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range s.Windows {
		if !w.ResetsAt.IsZero() {
			t.Errorf("window %q ResetsAt=%v, want zero (reset_after=0 should leave it unset)", w.Label, w.ResetsAt)
		}
	}
}

func TestParseVolcengineQuota(t *testing.T) {
	u := &AfpUsage{
		PlanType:    "agent-plan",
		AFPFiveHour: AfpWindow{Quota: 100, Used: 80, ResetTime: 1750000000000},
		AFPDaily:    AfpWindow{Quota: 100, Used: 50, ResetTime: 1760000000000},
		AFPWeekly:   AfpWindow{Quota: 100, Used: 30, ResetTime: 1750000000000},
		AFPMonthly:  AfpWindow{Quota: 100, Used: 10, ResetTime: 1750000000000},
	}
	s := ParseVolcengineQuota(u)
	if s.Billing != BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	// ultimate = monthly -> RemainingPct = 0.9 (5h is the short rate-cap).
	if s.RemainingPct != 0.9 {
		t.Errorf("RemainingPct=%v, want 0.9 (monthly ultimate)", s.RemainingPct)
	}
	if s.Plan != "agent-plan" {
		t.Errorf("Plan=%q", s.Plan)
	}
	// P0-2: assert Ultimate/Short markers on volcengine windows, and that each
	// window's ResetsAt comes from ITS OWN ResetTime (a dropped assignment
	// leaves it zero, silently zeroing Surplus() for scheduling).
	wantReset := map[string]int64{
		"5h": 1750000000000, "daily": 1760000000000, "weekly": 1750000000000, "monthly": 1750000000000,
	}
	for _, w := range s.Windows {
		switch w.Label {
		case "5h":
			if !w.Short || w.Ultimate {
				t.Errorf("5h: Short=%v Ultimate=%v, want Short=true Ultimate=false", w.Short, w.Ultimate)
			}
			if w.Duration != 5*time.Hour {
				t.Errorf("5h Duration=%v, want 5h", w.Duration)
			}
		case "monthly":
			if !w.Ultimate || w.Short {
				t.Errorf("monthly: Ultimate=%v Short=%v, want Ultimate=true Short=false", w.Ultimate, w.Short)
			}
			if w.Duration != 30*24*time.Hour {
				t.Errorf("monthly Duration=%v, want 30d", w.Duration)
			}
		case "daily", "weekly":
			if w.Ultimate || w.Short {
				t.Errorf("%s: Ultimate=%v Short=%v - intermediate windows must be neither", w.Label, w.Ultimate, w.Short)
			}
		}
		ms, ok := wantReset[w.Label]
		if !ok {
			t.Errorf("unexpected window label %q", w.Label)
			continue
		}
		if want := time.UnixMilli(ms); !w.ResetsAt.Equal(want) {
			t.Errorf("%s: ResetsAt=%v, want %v (from its own ResetTime)", w.Label, w.ResetsAt, want)
		}
	}
}

// TestParseVolcengineQuota_OverQuotaClampsToZero: when Used > Quota (over-quota),
// RemainingPct must clamp to 0 (exhausted), not go negative - a negative would
// read as the "unmeasured" sentinel and give the provider a neutral surplus.
func TestParseVolcengineQuota_OverQuotaClampsToZero(t *testing.T) {
	u := &AfpUsage{
		AFPFiveHour: AfpWindow{Quota: 100, Used: 120, ResetTime: 1750000000000}, // 120% used
		AFPMonthly:  AfpWindow{Quota: 100, Used: 10, ResetTime: 1750000000000},
	}
	s := ParseVolcengineQuota(u)
	found := false
	for _, w := range s.Windows {
		if w.Label == "5h" {
			found = true
			if w.RemainingPct != 0 {
				t.Errorf("5h RemainingPct=%v, want exactly 0", w.RemainingPct)
			}
		}
	}
	if !found {
		t.Fatal("5h quota window not found")
	}
	// Snapshot scheduling follows the Ultimate monthly window, not the short 5h
	// rate cap; this separately pins the final RemainingPct source.
	if s.RemainingPct != 0.9 {
		t.Errorf("snapshot RemainingPct=%v, want monthly ultimate value 0.9", s.RemainingPct)
	}
}

func TestParseDeepseekQuota(t *testing.T) {
	body := []byte(`{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`)
	s := ParseDeepseekQuota(body)
	if s.Billing != BillingPayG {
		t.Errorf("Billing=%v, want PayG", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct=%v, want -1 (balance has no window)", s.RemainingPct)
	}
	if len(s.Windows) != 1 || s.Windows[0].Total != 10.5 {
		t.Errorf("balance window: %+v", s.Windows)
	}
	w := s.Windows[0]
	if w.RemainingPct != -1 {
		t.Errorf("window RemainingPct=%v want -1 (balance has no percentage)", w.RemainingPct)
	}
	// The Web UI must show ONLY the remaining balance, not a granted/topped-up
	// breakdown (it's redundant: total = granted + topped-up). DetailLabel empty
	// + no Details is what suppresses the breakdown section in renderAccountUsage
	// (which gates Details on a non-empty DetailLabel). Assert both stay empty so
	// a regression that re-adds the breakdown turns the test red.
	if w.DetailLabel != "" {
		t.Errorf("DetailLabel=%q want empty (no breakdown in UI)", w.DetailLabel)
	}
	if len(w.Details) != 0 {
		t.Errorf("Details=%+v want none (no breakdown in UI)", w.Details)
	}
}

func TestParseAqpQuota(t *testing.T) {
	mu := &MonthlyProjectUsage{SelectedYear: 2026, SelectedMonth: 7, TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}
	s := ParseAqpQuota(mu, "alice@example.com")
	if s.Billing != BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.RemainingPct != 0.7 {
		t.Errorf("RemainingPct=%v, want 0.7", s.RemainingPct)
	}
	if len(s.Windows) != 1 {
		t.Fatalf("Windows len=%d want 1", len(s.Windows))
	}
	w := s.Windows[0]
	// ResetsAt must be set - without it Surplus() returns 0 (the bug).
	if w.ResetsAt.IsZero() {
		t.Fatal("Ultimate window ResetsAt is zero - surplus would always be 0")
	}
	wantReset := time.Date(2026, 8, 0, 23, 59, 59, 0, time.Local) // day 0 of Aug = Jul 31
	if !w.ResetsAt.Equal(wantReset) {
		t.Errorf("ResetsAt=%v want %v (last second of selected month)", w.ResetsAt, wantReset)
	}
	if w.Duration <= 0 {
		t.Errorf("Duration=%v want >0", w.Duration)
	}
	// With 70% remaining and >30% of the month elapsed, surplus > 0 (under pace).
	// Pre-fix this returned 0 because ResetsAt was zero - the regression guard.
	midMonth := time.Date(2026, 7, 15, 12, 0, 0, 0, time.Local)
	if got := s.Surplus(midMonth, 1); got <= 0 {
		t.Errorf("Surplus at mid-month (70%% remaining) = %v, want >0 (under pace)", got)
	}
}

func TestParseAqpQuota_ZeroMonthFallback(t *testing.T) {
	// When the API omits SelectedYear/SelectedMonth, fall back to the current
	// month so ResetsAt is still non-zero (surplus still works).
	mu := &MonthlyProjectUsage{TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}
	s := ParseAqpQuota(mu, "")
	if s.Windows[0].ResetsAt.IsZero() {
		t.Fatal("fallback ResetsAt is zero - should default to current month end")
	}
	if s.Windows[0].Duration <= 0 {
		t.Errorf("fallback Duration=%v want >0", s.Windows[0].Duration)
	}
}
