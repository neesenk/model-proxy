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

func TestParseCompassQuota(t *testing.T) {
	mu := &MonthlyProjectUsage{TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}
	s := parseCompassQuota(mu, "alice@example.com")
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.RemainingPct != 0.7 {
		t.Errorf("RemainingPct=%v, want 0.7", s.RemainingPct)
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
