package runtime

import (
	"net/http"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// recoveredSnapshot builds the positive-evidence quota snapshot: plan
// billing, fresh, every window with remaining budget.
func recoveredSnapshot(now time.Time, remaining ...float64) *provider.QuotaSnapshot {
	s := &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
	}
	for _, r := range remaining {
		s.Windows = append(s.Windows, provider.QuotaWindow{RemainingPct: r})
	}
	return s
}

// windowedProv is a minimal Provider serving a fixed windowed quota snapshot
// (the package's snapshotProv carries no windows, and windowless snapshots
// are deliberately NOT recovery evidence).
type windowedProv struct{ snapshot *provider.QuotaSnapshot }

func (w *windowedProv) AuthHeaders(*http.Request) error                        { return nil }
func (w *windowedProv) Refresh() error                                         { return nil }
func (w *windowedProv) RewriteRequest(string, []byte, string) (string, []byte) { return "", nil }
func (w *windowedProv) Logout() error                                          { return nil }
func (w *windowedProv) Usage() error                                           { return nil }
func (w *windowedProv) FetchModels() ([]string, error)                         { return nil, nil }
func (w *windowedProv) Quota() (*provider.QuotaSnapshot, error) {
	s := *w.snapshot
	return &s, nil
}
func (w *windowedProv) ProbeRequest(string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (w *windowedProv) ExtraHeaders(*http.Request, string)                   {}
func (w *windowedProv) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

func TestQuotaRecoveredClearCooldownClearsStale429Prediction(t *testing.T) {
	m := newTestManager(0)
	now := time.Now()
	// A 429 cooldown claiming the provider stays limited for another 80 min…
	m.RecordRateLimit("zhipu", now.Add(80*time.Minute), Transient, 0)
	// …while the 5h window has already reset: fresh plan snapshot, every
	// window with remaining budget (the reported bug).
	m.SetQuota("zhipu", recoveredSnapshot(now, 0.99, 0.57, 1.0), 0)

	cleared := m.QuotaRecoveredClearCooldown([]string{"zhipu"}, now, 3*time.Minute, 0)
	if len(cleared) != 1 || cleared[0] != "zhipu" {
		t.Fatalf("cleared = %v, want [zhipu]", cleared)
	}
	if !m.TargetHealthy("zhipu", "glm-5.3", now) {
		t.Fatal("provider must be schedulable again after the measurement overturned the prediction")
	}
}

func TestQuotaRecoveredClearCooldownRequiresPositiveEvidence(t *testing.T) {
	now := time.Now()
	maxAge := 3 * time.Minute

	cases := map[string]*provider.QuotaSnapshot{
		"exhausted window":    recoveredSnapshot(now, 0.99, 0), // weekly at zero
		"stale snapshot":      recoveredSnapshot(now.Add(-time.Hour), 0.99),
		"errored snapshot":    {Billing: provider.BillingPlan, AsOf: now, Err: "http 503"},
		"unknown billing":     {Billing: 0, AsOf: now, Windows: []provider.QuotaWindow{{RemainingPct: 1}}},
		"windowless snapshot": {Billing: provider.BillingPlan, AsOf: now},
		"no snapshot":         nil,
	}
	for name, snapshot := range cases {
		t.Run(name, func(t *testing.T) {
			m := newTestManager(0)
			m.RecordRateLimit("zhipu", now.Add(80*time.Minute), Quota, 0)
			m.SetQuota("zhipu", snapshot, 0)
			if cleared := m.QuotaRecoveredClearCooldown([]string{"zhipu"}, now, maxAge, 0); len(cleared) != 0 {
				t.Fatalf("cleared = %v, want none: absence of exhaustion proof is not proof of recovery", cleared)
			}
			if m.TargetHealthy("zhipu", "glm-5.3", now) {
				t.Fatal("cooldown must survive without positive recovery evidence")
			}
		})
	}
}

func TestQuotaRecoveredClearCooldownSparesFrozenAndCircuit(t *testing.T) {
	now := time.Now()
	// Operator freeze + a hot circuit stay even with recovered quota: they
	// are budget-independent signals.
	m := newTestManager(0)
	m.FreezeHealth("zhipu", nil, []string{"zhipu"})
	m.RecordFailure("zhipu", 1, time.Hour, 0) // threshold 1 → circuit open
	m.SetQuota("zhipu", recoveredSnapshot(now, 1), 0)

	if cleared := m.QuotaRecoveredClearCooldown([]string{"zhipu"}, now, time.Minute, 0); len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none (no rate-limit cooldown was set)", cleared)
	}
	if m.TargetHealthy("zhipu", "m", now) {
		t.Fatal("operator freeze must survive quota recovery")
	}
	status := m.Dashboard(now).Providers["zhipu"]
	if status.CircuitState != "open" {
		t.Fatalf("circuit cooldown must survive quota recovery, circuit_state=%s", status.CircuitState)
	}
}

func TestQuotaRecoveredClearCooldownOnlyFutureCooldown(t *testing.T) {
	now := time.Now()
	m := newTestManager(0)
	// An already-expired cooldown is not "cleared" (nothing to do) — and a
	// second provider with no cooldown at all is skipped.
	m.RecordRateLimit("zhipu", now.Add(-time.Minute), Transient, 0)
	m.SetQuota("zhipu", recoveredSnapshot(now, 1), 0)

	if cleared := m.QuotaRecoveredClearCooldown([]string{"zhipu", "other"}, now, time.Minute, 0); len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none", cleared)
	}
}

func TestQuotaRecoveredClearCooldownGenerationGate(t *testing.T) {
	now := time.Now()
	m := newTestManager(0)
	m.RecordRateLimit("zhipu", now.Add(time.Hour), Quota, 0)
	m.SetQuota("zhipu", recoveredSnapshot(now, 1), 0)

	if cleared := m.QuotaRecoveredClearCooldown([]string{"zhipu"}, now, time.Minute, 42); len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none for a stale generation", cleared)
	}
	if m.TargetHealthy("zhipu", "m", now) {
		t.Fatal("a stale generation must not mutate health")
	}
}

func TestQuotaTrackerPollOneClearsCooldownWithRecoveredQuota(t *testing.T) {
	// The manual "Refresh usage" path (the reported bug: usage recovered,
	// freeze badge stuck). PollOne commits the snapshot AND lifts the stale
	// cooldown in one motion.
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "60s"}}
	m := newTestManager(0)
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{
				"zhipu": &windowedProv{snapshot: recoveredSnapshot(time.Now(), 0.99, 0.57)},
			}
		}, m)
	m.RecordRateLimit("zhipu", time.Now().Add(80*time.Minute), Transient, 0)

	if !tr.PollOne("zhipu") {
		t.Fatal("PollOne(zhipu) returned false")
	}
	if !m.TargetHealthy("zhipu", "glm-5.3", time.Now()) {
		t.Fatal("PollOne with a recovered snapshot must clear the stale 429 cooldown")
	}
}

func TestQuotaTrackerPollAllClearsCooldownWithRecoveredQuota(t *testing.T) {
	// The periodic poll self-heals the same way (no manual click needed).
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "60s"}}
	m := newTestManager(0)
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{
				"zhipu": &windowedProv{snapshot: recoveredSnapshot(time.Now(), 0.99)},
				"aqp":   &windowedProv{snapshot: recoveredSnapshot(time.Now(), 0)},
			}
		}, m)
	m.RecordRateLimit("zhipu", time.Now().Add(80*time.Minute), Transient, 0)
	m.RecordRateLimit("aqp", time.Now().Add(80*time.Minute), Transient, 0)

	tr.PollAll(time.Now())

	if !m.TargetHealthy("zhipu", "glm-5.3", time.Now()) {
		t.Fatal("PollAll with a recovered zhipu snapshot must clear its stale 429 cooldown")
	}
	if m.TargetHealthy("aqp", "m", time.Now()) {
		t.Fatal("PollAll must NOT clear aqp's cooldown — its window shows an exhausted budget")
	}
}

func TestQuotaTrackerRefreshOneKeepsCooldown(t *testing.T) {
	// The 429-triggered refresh must NOT clear the cooldown it just
	// observed: a genuine per-request rate limit with healthy budget would
	// otherwise fight its own backoff on every poll.
	now := time.Now()
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "60s"}}
	m := newTestManager(0)
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{
				"zhipu": &windowedProv{snapshot: recoveredSnapshot(now, 0.99)},
			}
		}, m)
	m.RecordRateLimit("zhipu", now.Add(80*time.Minute), Transient, 0)

	tr.RefreshOne("zhipu")

	if m.TargetHealthy("zhipu", "glm-5.3", now) {
		t.Fatal("RefreshOne (429-triggered) must not clear the cooldown")
	}
	// ...but the snapshot was still learned (scheduling sees fresh quota).
	if q := m.Quota("zhipu"); q == nil || q.Err != "" {
		t.Fatal("RefreshOne must still commit the fetched snapshot")
	}
}
