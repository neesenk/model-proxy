package main

import (
	"net/http"
	"path/filepath"
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
	// binding = min(primary rem 0.7, weekly rem 0.4, spend rem 0.75) = 0.4
	if s.RemainingPct != 0.4 {
		t.Errorf("RemainingPct=%v, want 0.4 (weekly binding)", s.RemainingPct)
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
	// binding = min(5h rem 0.2, weekly rem 0.7, monthly rem 0.9) = 0.2
	if s.RemainingPct != 0.2 {
		t.Errorf("RemainingPct=%v, want 0.2 (5h binding)", s.RemainingPct)
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

func TestQuotaTracker_StaleIsUnknown(t *testing.T) {
	tr := newQuotaTracker(filepath.Join(t.TempDir(), "q.json"), func() *Config { return &Config{} }, func() map[string]provider.Provider { return nil })
	tr.setSnapshot("zhipu", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: time.Now().Add(-30 * time.Minute)})
	if c := tr.effectiveBilling("zhipu", 5*time.Minute); c != provider.BillingUnknown {
		t.Errorf("stale snapshot billing=%v, want Unknown", c)
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
