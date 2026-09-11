package runtime

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// snapshotProv is a minimal Provider returning a fixed quota snapshot.
type snapshotProv struct{ rem float64 }

func (s *snapshotProv) AuthHeaders(*http.Request) error                        { return nil }
func (s *snapshotProv) Refresh() error                                         { return nil }
func (s *snapshotProv) RewriteRequest(string, []byte, string) (string, []byte) { return "", nil }
func (s *snapshotProv) Logout() error                                          { return nil }
func (s *snapshotProv) Usage() error                                           { return nil }
func (s *snapshotProv) FetchModels() ([]string, error)                         { return nil, nil }
func (s *snapshotProv) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: s.rem, AsOf: time.Now()}, nil
}
func (s *snapshotProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (s *snapshotProv) ExtraHeaders(*http.Request, string)                   {}
func (s *snapshotProv) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

// flakyProv fails transiently a fixed number of times before succeeding.
type flakyProv struct {
	snapshotProv
	errs  []string
	calls atomic.Int32
}

func (f *flakyProv) Quota() (*provider.QuotaSnapshot, error) {
	call := int(f.calls.Add(1))
	if call <= len(f.errs) {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: f.errs[call-1]}, nil
	}
	return f.snapshotProv.Quota()
}

func TestCurrentGeneration(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, newTestManager(0))
	if got := tr.CurrentGeneration(); got != 0 {
		t.Errorf("nil Generation func = %d, want 0", got)
	}
	tr.Generation = func() uint64 { return 7 }
	if got := tr.CurrentGeneration(); got != 7 {
		t.Errorf("Generation func = %d, want 7", got)
	}
}

func TestPollAllMergesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.json")
	tr := NewQuotaTracker(path,
		func() *configdomain.Config { return &configdomain.Config{} },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": &snapshotProv{rem: 0.5}} },
		newTestManager(0))
	tr.PollAll(time.Now())
	if s := tr.Snapshot("x"); s == nil || s.RemainingPct != 0.5 {
		t.Fatalf("PollAll snapshot = %+v", s)
	}

	// Load restores only keys that exist in the CURRENT provider set (the
	// production order is BuildProviders → Start → Load, so provs() is
	// populated). Quota keys include pool virtual ids: keys from removed
	// accounts/providers must NOT merge back — they used to re-persist
	// forever and revive on every restart.
	fresh := NewQuotaTracker(path,
		func() *configdomain.Config { return &configdomain.Config{} },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": &snapshotProv{rem: 0}} },
		newTestManager(0))
	fresh.Load()
	if s := fresh.Snapshot("x"); s == nil || s.RemainingPct != 0.5 {
		t.Fatalf("Load snapshot = %+v", s)
	}
	if s := fresh.Snapshot("ghost"); s != nil {
		t.Fatalf("ghost key restored = %+v, want filtered", s)
	}
	if err := fresh.Persist(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghost") {
		t.Fatal("ghost key re-persisted — stale quota keys never shrink")
	}
}

func TestPollOneUnknownKey(t *testing.T) {
	tr := NewQuotaTracker("", nil,
		func() map[string]provider.Provider { return nil }, newTestManager(0))
	if tr.PollOne("missing") {
		t.Error("PollOne on unknown key must return false")
	}
}

func TestPollOneCommits(t *testing.T) {
	tr := NewQuotaTracker("", nil,
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": &snapshotProv{rem: 0.9}} },
		newTestManager(0))
	if !tr.PollOne("x") {
		t.Fatal("PollOne returned false for live provider")
	}
	if s := tr.Snapshot("x"); s == nil || s.RemainingPct != 0.9 {
		t.Fatalf("PollOne snapshot = %+v", s)
	}
}

func TestPollAllSkipsPayAsYouGo(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"shopee": {Billing: "pay-as-you-go"},
			"zhipu":  {},
			// Pay-as-you-go WITH a usage endpoint (deepseek's /user/balance):
			// the balance IS its quota window, so it is polled like a plan
			// provider.
			"deepseek": {Billing: "pay-as-you-go", UsageURL: "http://x/user/balance"},
		},
	}
	mgr := newTestManager(0)
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{
				"shopee":   &snapshotProv{rem: 0.5},
				"zhipu":    &snapshotProv{rem: 0.8},
				"deepseek": &snapshotProv{rem: -1},
			}
		}, mgr)
	// Seed a stale pay-as-you-go snapshot to verify it gets dropped.
	tr.SetSnapshot("shopee", &provider.QuotaSnapshot{Billing: provider.BillingUnknown})
	tr.PollAll(time.Now())
	if s := tr.Snapshot("shopee"); s != nil {
		t.Fatalf("pay-as-you-go snapshot must be dropped, got %+v", s)
	}
	if s := tr.Snapshot("zhipu"); s == nil || s.RemainingPct != 0.8 {
		t.Fatalf("plan provider snapshot = %+v, want RemainingPct 0.8", s)
	}
	if s := tr.Snapshot("deepseek"); s == nil {
		t.Fatal("pay-as-you-go provider with usage_url must be polled, got nil snapshot")
	}
}

func TestPollAllSkipsPayAsYouGoPoolVirtual(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"shopee": {Billing: "pay-as-you-go"},
			"zhipu":  {},
		},
	}
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{
				"shopee#acc1": &snapshotProv{rem: 0.5},
				"zhipu#acc1":  &snapshotProv{rem: 0.6},
			}
		}, newTestManager(0))
	tr.PollAll(time.Now())
	if s := tr.Snapshot("shopee#acc1"); s != nil {
		t.Fatalf("pay-as-you-go pool virtual snapshot must be dropped, got %+v", s)
	}
	if s := tr.Snapshot("zhipu#acc1"); s == nil || s.RemainingPct != 0.6 {
		t.Fatalf("plan pool virtual snapshot = %+v, want RemainingPct 0.6", s)
	}
}

func TestPollOneRejectsPayAsYouGo(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"shopee":   {Billing: "pay-as-you-go"},
			"deepseek": {Billing: "pay-as-you-go", UsageURL: "http://x/user/balance"},
		},
	}
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{
				"shopee":   &snapshotProv{rem: 0.5},
				"deepseek": &snapshotProv{rem: -1},
			}
		}, newTestManager(0))
	if tr.PollOne("shopee") {
		t.Fatal("PollOne must return false for pay-as-you-go provider without usage_url")
	}
	if s := tr.Snapshot("shopee"); s != nil {
		t.Fatalf("pay-as-you-go PollOne snapshot = %+v, want nil", s)
	}
	if !tr.PollOne("deepseek") {
		t.Fatal("PollOne must accept a pay-as-you-go provider with usage_url")
	}
	if s := tr.Snapshot("deepseek"); s == nil {
		t.Fatal("PollOne(deepseek) committed no snapshot")
	}
}

func TestLoadSkipsPayAsYouGoSnapshot(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"shopee":   {Billing: "pay-as-you-go"},
			"zhipu":    {},
			"deepseek": {Billing: "pay-as-you-go", UsageURL: "http://x/user/balance"},
		},
	}
	path := filepath.Join(t.TempDir(), "q.json")
	wrap := map[string]any{
		"providers": map[string]PersistedQuotaSnapshot{
			"shopee":   {Billing: provider.BillingUnknown},
			"zhipu":    {Billing: provider.BillingPlan, RemainingPct: 0.7},
			"deepseek": {Billing: provider.BillingPayG, RemainingPct: -1},
		},
	}
	data, err := json.Marshal(wrap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tr := NewQuotaTracker(path, func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{
				"shopee":   &snapshotProv{rem: 0.5},
				"zhipu":    &snapshotProv{rem: 0.7},
				"deepseek": &snapshotProv{rem: -1},
			}
		}, newTestManager(0))
	tr.Load()
	if s := tr.Snapshot("shopee"); s != nil {
		t.Fatalf("loaded pay-as-you-go snapshot = %+v, want nil", s)
	}
	if s := tr.Snapshot("zhipu"); s == nil || s.RemainingPct != 0.7 {
		t.Fatalf("loaded plan snapshot = %+v, want RemainingPct 0.7", s)
	}
	if s := tr.Snapshot("deepseek"); s == nil {
		t.Fatal("loaded pay-as-you-go snapshot with usage_url must be kept, got nil")
	}
}

func TestCommitSnapshotGenerationGate(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, newTestManager(3))
	tr.Generation = func() uint64 { return 3 }
	if tr.CommitSnapshot(2, "x", &provider.QuotaSnapshot{}) {
		t.Error("stale generation commit must be rejected")
	}
	if !tr.CommitSnapshot(3, "x", &provider.QuotaSnapshot{RemainingPct: 1}) {
		t.Error("current generation commit must be accepted")
	}
}

func TestFetchQuotaRetriesTransient(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, newTestManager(0))
	tr.SetRetryBackoff(time.Millisecond)
	p := &flakyProv{snapshotProv: snapshotProv{rem: 0.3}, errs: []string{"timeout", "connection refused"}}
	s := tr.FetchQuota(p, time.Now())
	if s.Err != "" || s.RemainingPct != 0.3 {
		t.Fatalf("FetchQuota = %+v, want recovered 0.3", s)
	}
	if p.calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", p.calls.Load())
	}
}

func TestFetchQuotaNoRetryPermanent(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, newTestManager(0))
	tr.SetRetryBackoff(time.Millisecond)
	p := &flakyProv{errs: []string{"http 401 unauthorized"}}
	s := tr.FetchQuota(p, time.Now())
	if s.Err == "" {
		t.Fatal("permanent error must surface")
	}
	if p.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", p.calls.Load())
	}
}

func TestIsTransientQuotaErr(t *testing.T) {
	permanent := []string{"not logged in", "http 401", "session expired", "ak/sk not configured"}
	for _, e := range permanent {
		if IsTransientQuotaErr(e) {
			t.Errorf("IsTransientQuotaErr(%q) = true, want false", e)
		}
	}
	for _, e := range []string{"timeout", "no such host", "http 502 bad gateway", "weird new failure"} {
		if !IsTransientQuotaErr(e) {
			t.Errorf("IsTransientQuotaErr(%q) = false, want true", e)
		}
	}
}

func TestSetSnapshotAndStopChannel(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, newTestManager(0))
	tr.SetSnapshot("a", &provider.QuotaSnapshot{RemainingPct: 0.1})
	if s := tr.Snapshot("a"); s == nil || s.RemainingPct != 0.1 {
		t.Fatalf("SetSnapshot = %+v", s)
	}
	select {
	case <-tr.StopChannel():
		t.Fatal("stop channel closed before Stop")
	default:
	}
	tr.Stop()
	select {
	case <-tr.StopChannel():
	default:
		t.Fatal("stop channel open after Stop")
	}
	if tr.AdmissionOpen() {
		t.Error("AdmissionOpen true after Stop")
	}
	if tr.Launch(func() {}) {
		t.Error("Launch admitted after Stop")
	}
	if !tr.Stopped() {
		t.Error("Stopped false after Stop")
	}
}

func TestPersistInMemoryNoop(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, newTestManager(0))
	if err := tr.Persist(); err != nil {
		t.Errorf("in-memory Persist = %v, want nil", err)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	tr := NewQuotaTracker(filepath.Join(dir, "absent.json"), nil, nil, newTestManager(0))
	tr.Load() // must not panic

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr2 := NewQuotaTracker(bad, nil, nil, newTestManager(0))
	tr2.Load() // must not panic
}

// TestQuotaTrackerFreshnessMaxAgeFrozenAtStart: the freshness window must be
// frozen to the SAME poll interval the ticker captured at Start. A hot
// quota_poll_interval change used to shrink the freshness window (read live
// per request) while the ticker kept the old cadence — every poll cycle's
// tail flagged fresh snapshots stale, flapping tiers and the
// skip-quota-exhausted fail-open (pitfalls #29: the change needs a restart;
// both sides must honor that).
func TestQuotaTrackerFreshnessMaxAgeFrozenAtStart(t *testing.T) {
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "5m"}}
	tracker := NewQuotaTracker("", func() *configdomain.Config { return cfg }, func() map[string]provider.Provider {
		return nil
	}, newTestManager(0))
	defer tracker.Stop()
	if got := tracker.FreshnessMaxAge(); got != 15*time.Minute {
		t.Fatalf("pre-Start FreshnessMaxAge = %v, want 3×5m (live-config fallback)", got)
	}
	tracker.Start()
	// Hot-shrink the interval: the window must stay frozen at Start's cadence.
	cfg.Scheduling.QuotaPollInterval = "1m"
	if got := tracker.FreshnessMaxAge(); got != 15*time.Minute {
		t.Fatalf("post-Start FreshnessMaxAge = %v, want frozen 15m despite hot change to 1m", got)
	}
}
