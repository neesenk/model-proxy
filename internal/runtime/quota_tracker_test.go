package runtime

import (
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/provider"
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
	tr := NewQuotaTracker("", nil, nil, NewManager(0))
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
		NewManager(0))
	tr.PollAll(time.Now())
	if s := tr.Snapshot("x"); s == nil || s.RemainingPct != 0.5 {
		t.Fatalf("PollAll snapshot = %+v", s)
	}

	fresh := NewQuotaTracker(path,
		func() *configdomain.Config { return &configdomain.Config{} },
		func() map[string]provider.Provider { return nil },
		NewManager(0))
	fresh.Load()
	if s := fresh.Snapshot("x"); s == nil || s.RemainingPct != 0.5 {
		t.Fatalf("Load snapshot = %+v", s)
	}
}

func TestPollOneUnknownKey(t *testing.T) {
	tr := NewQuotaTracker("", nil,
		func() map[string]provider.Provider { return nil }, NewManager(0))
	if tr.PollOne("missing") {
		t.Error("PollOne on unknown key must return false")
	}
}

func TestPollOneCommits(t *testing.T) {
	tr := NewQuotaTracker("", nil,
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": &snapshotProv{rem: 0.9}} },
		NewManager(0))
	if !tr.PollOne("x") {
		t.Fatal("PollOne returned false for live provider")
	}
	if s := tr.Snapshot("x"); s == nil || s.RemainingPct != 0.9 {
		t.Fatalf("PollOne snapshot = %+v", s)
	}
}

func TestCommitSnapshotGenerationGate(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, NewManager(3))
	tr.Generation = func() uint64 { return 3 }
	if tr.CommitSnapshot(2, "x", &provider.QuotaSnapshot{}) {
		t.Error("stale generation commit must be rejected")
	}
	if !tr.CommitSnapshot(3, "x", &provider.QuotaSnapshot{RemainingPct: 1}) {
		t.Error("current generation commit must be accepted")
	}
}

func TestFetchQuotaRetriesTransient(t *testing.T) {
	tr := NewQuotaTracker("", nil, nil, NewManager(0))
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
	tr := NewQuotaTracker("", nil, nil, NewManager(0))
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
	tr := NewQuotaTracker("", nil, nil, NewManager(0))
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
	tr := NewQuotaTracker("", nil, nil, NewManager(0))
	if err := tr.Persist(); err != nil {
		t.Errorf("in-memory Persist = %v, want nil", err)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	tr := NewQuotaTracker(filepath.Join(dir, "absent.json"), nil, nil, NewManager(0))
	tr.Load() // must not panic

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr2 := NewQuotaTracker(bad, nil, nil, NewManager(0))
	tr2.Load() // must not panic
}
