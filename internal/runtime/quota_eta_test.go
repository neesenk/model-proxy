package runtime

import (
	"net/http"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// quota_eta_test.go covers the tracker's exhaustion-prediction wiring: the ETA
// is computed at commit time from the PREVIOUS committed snapshot and attached
// to the new Manager-held snapshot (PollAll/MergeQuotas and CommitSnapshot
// paths). Rate math itself is covered in the provider package.

// etaProv returns a plan snapshot with one ultimate window whose Used the test
// advances between polls. AsOf is stamped by FetchQuota from the poll's now.
type etaProv struct {
	used float64
}

func (e *etaProv) AuthHeaders(*http.Request) error                        { return nil }
func (e *etaProv) Refresh() error                                         { return nil }
func (e *etaProv) RewriteRequest(string, []byte, string) (string, []byte) { return "", nil }
func (e *etaProv) Logout() error                                          { return nil }
func (e *etaProv) Usage() error                                           { return nil }
func (e *etaProv) FetchModels() ([]string, error)                         { return nil, nil }
func (e *etaProv) Quota() (*provider.QuotaSnapshot, error) {
	rem := 1 - e.used/1000
	return &provider.QuotaSnapshot{
		Billing: provider.BillingPlan, RemainingPct: rem,
		Windows: []provider.QuotaWindow{{
			Label: "Weekly tokens", Kind: "tokens", Used: e.used, Total: 1000,
			RemainingPct: rem, Ultimate: true,
			Duration: 7 * 24 * time.Hour, ResetsAt: time.Now().Add(7 * 24 * time.Hour),
		}},
	}, nil
}
func (e *etaProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (e *etaProv) ExtraHeaders(*http.Request, string)                   {}
func (e *etaProv) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

func newEtaTracker(p *etaProv) *QuotaTracker {
	return NewQuotaTracker("",
		func() *configdomain.Config { return &configdomain.Config{} }, // default 5m poll interval → 15m max gap
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": p} },
		NewManager(0))
}

func TestPollAll_ComputesExhaustionEta(t *testing.T) {
	p := &etaProv{used: 100}
	tr := newEtaTracker(p)
	t0 := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	// First snapshot: no baseline → no prediction.
	tr.PollAll(t0)
	if s := tr.Snapshot("x"); s == nil || !s.ExhaustionEta.IsZero() {
		t.Fatalf("first poll: snapshot = %+v, want zero ExhaustionEta", s)
	}

	// Second poll 5m later, +100 used → rate 100/300s, remaining 800 → +40m.
	p.used = 200
	t1 := t0.Add(5 * time.Minute)
	tr.PollAll(t1)
	s := tr.Snapshot("x")
	if s == nil || s.ExhaustionEta.IsZero() {
		t.Fatalf("second poll: snapshot = %+v, want ExhaustionEta", s)
	}
	if d := s.ExhaustionEta.Sub(t1.Add(40 * time.Minute)); d < -time.Second || d > time.Second {
		t.Errorf("ExhaustionEta = %v, want ~%v", s.ExhaustionEta, t1.Add(40*time.Minute))
	}

	// Poll gap > 3×poll_interval (15m): stale baseline → no prediction.
	p.used = 300
	tr.PollAll(t1.Add(20 * time.Minute))
	if s := tr.Snapshot("x"); s == nil || !s.ExhaustionEta.IsZero() {
		t.Errorf("gapped poll: snapshot = %+v, want zero ExhaustionEta", s)
	}

	// Flat usage (rate 0) → no prediction, and a valid in-gap baseline.
	p.used = 300
	tr.PollAll(t1.Add(25 * time.Minute))
	if s := tr.Snapshot("x"); s == nil || !s.ExhaustionEta.IsZero() {
		t.Errorf("flat poll: snapshot = %+v, want zero ExhaustionEta", s)
	}
}

// CommitSnapshot (PollOne / 429 RefreshOne path) attaches the ETA the same way.
func TestCommitSnapshot_ComputesExhaustionEta(t *testing.T) {
	p := &etaProv{used: 0}
	tr := newEtaTracker(p)
	t0 := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	mk := func(used float64, asOf time.Time) *provider.QuotaSnapshot {
		p.used = used
		s, _ := p.Quota()
		s.AsOf = asOf
		return s
	}
	if !tr.CommitSnapshot(0, "x", mk(100, t0)) {
		t.Fatal("first commit rejected")
	}
	if s := tr.Snapshot("x"); !s.ExhaustionEta.IsZero() {
		t.Fatalf("first commit: eta = %v, want zero", s.ExhaustionEta)
	}
	if !tr.CommitSnapshot(0, "x", mk(200, t0.Add(5*time.Minute))) {
		t.Fatal("second commit rejected")
	}
	if s := tr.Snapshot("x"); s.ExhaustionEta.IsZero() {
		t.Fatal("second commit: no ExhaustionEta")
	}
	// Stale generation → rejected, snapshot untouched.
	if tr.CommitSnapshot(7, "x", mk(300, t0.Add(10*time.Minute))) {
		t.Error("stale-generation commit accepted")
	}
	if s := tr.Snapshot("x"); s.Windows[0].Used != 200 {
		t.Errorf("stale commit mutated quota: used = %v, want 200", s.Windows[0].Used)
	}
}
