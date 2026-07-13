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

// The Parse*Quota parser tests moved to the provider package in Phase 1
// (provider/quota_parse_test.go) so go test ./provider covers the parsers.
// This file keeps the quotaTracker + classifyBilling tests (main-package state).

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
		{"stale->unknown", stale, "", provider.BillingUnknown},
		{"err->unknown", errSnap, "", provider.BillingUnknown},
		{"nil->unknown", nil, "", provider.BillingUnknown},
		{"unknown-snap->unknown", unknownSnap, "", provider.BillingUnknown},
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
func (s *snapshotProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (s *snapshotProv) ExtraHeaders(*http.Request, string)                   {}
func (s *snapshotProv) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

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
	prov := &quotaCallProv{calls: &calls} // pollInterval default 5m -> half 2.5m
	cfg := &Config{Providers: map[string]Provider{"x": {Provider: "zhipu"}}}
	tr := newQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *Config { return cfg },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	tr.refreshOne("x") // calls=1, sets last
	tr.refreshOne("x") // within 2.5m -> debounced
	if got := calls.Load(); got != 1 {
		t.Errorf("sequential refreshOne: Quota() called %d times, want 1 (debounced)", got)
	}
}

// TestQuotaTracker_StickyPersistLoad: persist() writes the sticky map (via
// stickySnapshot) and load() restores it into LoadedSticky - so the proxy
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
