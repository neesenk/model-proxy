package main

import (
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

func TestParseZhipuQuota(t *testing.T) {
	body := []byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[
		{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000,"usageDetails":[{"modelCode":"glm-5.2","usage":30000},{"modelCode":"glm-5.1","usage":10000}]},
		{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1750000000000,"usage":200000,"currentValue":140000,"remaining":60000},
		{"type":"TIME_LIMIT","unit":5,"percentage":10,"nextResetTime":1750000000000,"usage":3600,"currentValue":360,"remaining":3240}
	]}}`)
	s, err := parseZhipuQuota(body, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.Level != "GLM Coding Plan" {
		t.Errorf("Level=%q", s.Level)
	}
	// binding = min(5h rem 0.6, weekly rem 0.3) — TIME_LIMIT excluded.
	if s.RemainingPct != 0.3 {
		t.Errorf("RemainingPct=%v, want 0.3 (weekly is binding; TIME_LIMIT excluded)", s.RemainingPct)
	}
	if len(s.Windows) != 3 {
		t.Fatalf("got %d windows, want 3", len(s.Windows))
	}
	// 5h window has per-model details.
	var w5h *provider.QuotaWindow
	for i := range s.Windows {
		if s.Windows[i].Label == "5h tokens" {
			w5h = &s.Windows[i]
		}
	}
	if w5h == nil || len(w5h.Details) != 2 {
		t.Errorf("5h window details: %+v", w5h)
	}
	// P0-2: assert Ultimate/Short markers — getting these wrong silently
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
	var wWeekly *provider.QuotaWindow
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
	// Non-zhipu JSON → returns nil snapshot (caller falls back to model list).
	s, err := parseZhipuQuota([]byte(`{"object":"list","data":[]}`), "")
	if err == nil && s != nil {
		t.Fatalf("expected nil snapshot for non-zhipu body, got %+v", s)
	}
}

func TestParseCodexQuota(t *testing.T) {
	body := []byte(`{"email":"a@b.com","plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":12000},
		"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_after_seconds":300000}},
		"spend_control":{"reached":false,"individual_limit":{"used":"5","limit":"20","remaining":"15","used_percent":25,"reset_after_seconds":2500000}}}`)
	s, err := parseCodexQuota(body, "a@b.com", "pro")
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.Account != "a@b.com" || s.Plan != "pro" {
		t.Errorf("Account/Plan=%q/%q", s.Account, s.Plan)
	}
	// ultimate = monthly spend → RemainingPct = 0.75 (primary/weekly are token rate-caps, not ultimate).
	if s.RemainingPct != 0.75 {
		t.Errorf("RemainingPct=%v, want 0.75 (spend ultimate)", s.RemainingPct)
	}
	// P0-2: assert Ultimate marker on the spend window, and that primary/weekly
	// are NOT Ultimate/Short (different unit — money vs tokens).
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
				t.Errorf("%s: Ultimate=%v Short=%v — codex token windows must NOT be Ultimate/Short (money ultimate)", w.Label, w.Ultimate, w.Short)
			}
		}
	}
}

// TestParseCodexQuota_ZeroResetAfter: when reset_after_seconds is 0 (absent),
// ResetsAt must stay zero (not now), so it isn't misread as "just reset".
func TestParseCodexQuota_ZeroResetAfter(t *testing.T) {
	body := []byte(`{"email":"a@b.com","rate_limit":{"primary_window":{"used_percent":30,"reset_after_seconds":0}},
		"spend_control":{"individual_limit":{"used":"5","limit":"20","used_percent":25,"reset_after_seconds":0}}}`)
	s, err := parseCodexQuota(body, "a@b.com", "pro")
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
	u := &afpUsage{
		PlanType:    "agent-plan",
		AFPFiveHour: afpWindow{Quota: 100, Used: 80, ResetTime: 1750000000000},
		AFPWeekly:   afpWindow{Quota: 100, Used: 30, ResetTime: 1750000000000},
		AFPMonthly:  afpWindow{Quota: 100, Used: 10, ResetTime: 1750000000000},
	}
	s := parseVolcengineQuota(u)
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	// ultimate = monthly → RemainingPct = 0.9 (5h is the short rate-cap).
	if s.RemainingPct != 0.9 {
		t.Errorf("RemainingPct=%v, want 0.9 (monthly ultimate)", s.RemainingPct)
	}
	if s.Plan != "agent-plan" {
		t.Errorf("Plan=%q", s.Plan)
	}
	// P0-2: assert Ultimate/Short markers on volcengine windows.
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
				t.Errorf("%s: Ultimate=%v Short=%v — intermediate windows must be neither", w.Label, w.Ultimate, w.Short)
			}
		}
	}
}

// TestParseVolcengineQuota_OverQuotaClampsToZero: when Used > Quota (over-quota),
// RemainingPct must clamp to 0 (exhausted), not go negative — a negative would
// read as the "unmeasured" sentinel and give the provider a neutral surplus.
func TestParseVolcengineQuota_OverQuotaClampsToZero(t *testing.T) {
	u := &afpUsage{
		AFPFiveHour: afpWindow{Quota: 100, Used: 120, ResetTime: 1750000000000}, // 120% used
		AFPMonthly:  afpWindow{Quota: 100, Used: 10, ResetTime: 1750000000000},
	}
	s := parseVolcengineQuota(u)
	for _, w := range s.Windows {
		if w.Total > 0 && w.RemainingPct < 0 { // only windows with a real quota
			t.Errorf("window %q RemainingPct=%v, want >= 0 (over-quota clamped to 0, not negative)", w.Label, w.RemainingPct)
		}
	}
}

func TestParseDeepseekQuota(t *testing.T) {
	body := []byte(`{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`)
	s := parseDeepseekQuota(body)
	if s.Billing != provider.BillingPayG {
		t.Errorf("Billing=%v, want PayG", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct=%v, want -1 (balance has no window)", s.RemainingPct)
	}
	if len(s.Windows) != 1 || s.Windows[0].Total != 10.5 {
		t.Errorf("balance window: %+v", s.Windows)
	}
}

func TestParseAqpQuota(t *testing.T) {
	mu := &MonthlyProjectUsage{SelectedYear: 2026, SelectedMonth: 7, TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}
	s := parseAqpQuota(mu, "alice@example.com")
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.RemainingPct != 0.7 {
		t.Errorf("RemainingPct=%v, want 0.7", s.RemainingPct)
	}
	if len(s.Windows) != 1 {
		t.Fatalf("Windows len=%d want 1", len(s.Windows))
	}
	w := s.Windows[0]
	// ResetsAt must be set — without it provider.Surplus() returns 0 (the bug).
	if w.ResetsAt.IsZero() {
		t.Fatal("Ultimate window ResetsAt is zero — surplus would always be 0")
	}
	wantReset := time.Date(2026, 8, 0, 23, 59, 59, 0, time.Local) // day 0 of Aug = Jul 31
	if !w.ResetsAt.Equal(wantReset) {
		t.Errorf("ResetsAt=%v want %v (last second of selected month)", w.ResetsAt, wantReset)
	}
	if w.Duration <= 0 {
		t.Errorf("Duration=%v want >0", w.Duration)
	}
	// With 70% remaining and >30% of the month elapsed, surplus > 0 (under pace).
	// Pre-fix this returned 0 because ResetsAt was zero — the regression guard.
	midMonth := time.Date(2026, 7, 15, 12, 0, 0, 0, time.Local)
	if got := s.Surplus(midMonth, 1); got <= 0 {
		t.Errorf("Surplus at mid-month (70%% remaining) = %v, want >0 (under pace)", got)
	}
}

func TestParseAqpQuota_ZeroMonthFallback(t *testing.T) {
	// When the API omits SelectedYear/SelectedMonth, fall back to the current
	// month so ResetsAt is still non-zero (surplus still works).
	mu := &MonthlyProjectUsage{TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}
	s := parseAqpQuota(mu, "")
	if s.Windows[0].ResetsAt.IsZero() {
		t.Fatal("fallback ResetsAt is zero — should default to current month end")
	}
	if s.Windows[0].Duration <= 0 {
		t.Errorf("fallback Duration=%v want >0", s.Windows[0].Duration)
	}
}

func TestQuotaTracker_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota_state.json")
	cfg := func() *Config { return &Config{} }
	provs := func() map[string]provider.Provider { return nil }
	tr := newQuotaTracker(path, cfg, provs)
	tr.setSnapshot("zhipu", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.42, AsOf: time.Now()})
	tr.persist()

	tr2 := newQuotaTracker(path, cfg, provs)
	tr2.load()
	if s := tr2.snapshot("zhipu"); s == nil || s.RemainingPct != 0.42 {
		t.Fatalf("after reload: %+v", s)
	}
}

func TestClassifyBilling(t *testing.T) {
	fresh := &provider.QuotaSnapshot{Billing: provider.BillingPlan, AsOf: time.Now()}
	stale := &provider.QuotaSnapshot{Billing: provider.BillingPlan, AsOf: time.Now().Add(-30 * time.Minute)}
	errSnap := &provider.QuotaSnapshot{Billing: provider.BillingPlan, Err: "boom", AsOf: time.Now()}
	unknownSnap := &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: time.Now()}
	cases := []struct {
		name       string
		snap       *provider.QuotaSnapshot
		billingCfg string
		want       provider.BillingClass
	}{
		{"fresh plan", fresh, "", provider.BillingPlan},
		{"stale→unknown", stale, "", provider.BillingUnknown},
		{"err→unknown", errSnap, "", provider.BillingUnknown},
		{"nil→unknown", nil, "", provider.BillingUnknown},
		{"unknown-snap→unknown", unknownSnap, "", provider.BillingUnknown},
		{"payg override", fresh, "pay-as-you-go", provider.BillingPayG},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyBilling(tc.snap, tc.billingCfg, 5*time.Minute); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaTracker_PollAllCallsQuota(t *testing.T) {
	dir := t.TempDir()
	tr := newQuotaTracker(filepath.Join(dir, "q.json"),
		func() *Config { return &Config{Providers: map[string]Provider{"x": {Provider: "zhipu"}}} },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{"x": &snapshotProv{rem: 0.77}}
		},
	)
	tr.pollAll(time.Now())
	if s := tr.snapshot("x"); s == nil || s.RemainingPct != 0.77 {
		t.Fatalf("pollAll did not populate: %+v", s)
	}
}

// snapshotProv is a test Provider returning a fixed snapshot.
type snapshotProv struct{ rem float64 }

func (s *snapshotProv) AuthHeaders(*http.Request) error                        { return nil }
func (s *snapshotProv) Refresh() error                                         { return nil }
func (s *snapshotProv) RewriteRequest(string, []byte, string) (string, []byte) { return "", nil }
func (s *snapshotProv) Login() error                                           { return nil }
func (s *snapshotProv) Logout() error                                          { return nil }
func (s *snapshotProv) Usage() (any, error)                                    { return nil, nil }
func (s *snapshotProv) FetchModels() ([]string, error)                         { return nil, nil }
func (s *snapshotProv) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: s.rem, AsOf: time.Now()}, nil
}
func (s *snapshotProv) Surplus(snap *provider.QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}

// quotaCallProv wraps snapshotProv, counting Quota() calls (with an optional
// delay so concurrent refreshOne calls overlap and hit the in-flight guard).
type quotaCallProv struct {
	snapshotProv
	calls *atomic.Int32
	delay time.Duration
}

func (q *quotaCallProv) Quota() (*provider.QuotaSnapshot, error) {
	q.calls.Add(1)
	if q.delay > 0 {
		time.Sleep(q.delay)
	}
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: time.Now()}, nil
}

// TestQuotaTracker_RefreshOneCoalescesConcurrent: many concurrent refreshOne
// calls for one provider collapse to a single Quota() call (in-flight guard).
func TestQuotaTracker_RefreshOneCoalescesConcurrent(t *testing.T) {
	var calls atomic.Int32
	prov := &quotaCallProv{calls: &calls, delay: 30 * time.Millisecond}
	cfg := &Config{Providers: map[string]Provider{"x": {Provider: "zhipu"}}}
	tr := newQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *Config { return cfg },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tr.refreshOne("x") }()
	}
	wg.Wait()
	if got := calls.Load(); got > 2 {
		t.Errorf("concurrent refreshOne: Quota() called %d times, want ≤ 2 (coalesced)", got)
	}
}

// TestQuotaTracker_RefreshOneDebouncesSequential: a second refreshOne within
// pollInterval/2 of the first is dropped (debounce).
func TestQuotaTracker_RefreshOneDebouncesSequential(t *testing.T) {
	var calls atomic.Int32
	prov := &quotaCallProv{calls: &calls} // pollInterval default 5m → half 2.5m
	cfg := &Config{Providers: map[string]Provider{"x": {Provider: "zhipu"}}}
	tr := newQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *Config { return cfg },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	tr.refreshOne("x") // calls=1, sets last
	tr.refreshOne("x") // within 2.5m → debounced
	if got := calls.Load(); got != 1 {
		t.Errorf("sequential refreshOne: Quota() called %d times, want 1 (debounced)", got)
	}
}

// TestQuotaTracker_StickyPersistLoad: persist() writes the sticky map (via
// stickySnapshot) and load() restores it into LoadedSticky — so the proxy
// resumes parking on the same providers after a restart.
func TestQuotaTracker_StickyPersistLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota_state.json")
	tr := newQuotaTracker(path, func() *Config { return &Config{} }, func() map[string]provider.Provider { return nil })
	since := time.Unix(123, 0)
	tr.stickySnapshot = func() map[string]routeSticky {
		return map[string]routeSticky{"glm-5.2": {provider: "zhipu", since: since}}
	}
	tr.persist()

	tr2 := newQuotaTracker(path, func() *Config { return &Config{} }, func() map[string]provider.Provider { return nil })
	tr2.load()
	got := tr2.LoadedSticky["glm-5.2"]
	if got.provider != "zhipu" || !got.since.Equal(since) {
		t.Fatalf("sticky not restored: %+v", got)
	}
}
