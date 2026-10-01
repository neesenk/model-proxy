package runtime

import (
	"os"
	"path/filepath"
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
				"zhipu": fixedQuota(recoveredSnapshot(time.Now(), 0.99, 0.57)),
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
				"zhipu": fixedQuota(recoveredSnapshot(time.Now(), 0.99)),
				"aqp":   fixedQuota(recoveredSnapshot(time.Now(), 0)),
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
				"zhipu": fixedQuota(recoveredSnapshot(now, 0.99)),
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

// TestPollAllSyncFailureKeepsLastSnapshotAndHasNoSideEffects pins the
// backend-sync contract at the poll seam: a FAILED quota sync (usage endpoint
// down) neither mutates the last committed snapshot nor triggers the
// measurement-driven cooldown clear — a no-op sync must have no side effects.
// A later successful sync switches the snapshot over and may lift cooldowns.
func TestPollAllSyncFailureKeepsLastSnapshotAndHasNoSideEffects(t *testing.T) {
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "60s"}}
	m := newTestManager(0)
	snapshot := recoveredSnapshot(time.Now(), 0.8)
	prov := &quotaFake{quota: func() (*provider.QuotaSnapshot, error) { cp := *snapshot; return &cp, nil }}
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{"zhipu": prov}
		}, m)

	tr.PollAll(time.Now())
	seeded := m.Quota("zhipu")
	if seeded == nil || seeded.Err != "" || seeded.Billing != provider.BillingPlan {
		t.Fatalf("seed poll did not commit a good snapshot: %+v", seeded)
	}

	// The upstream starts 429-predicting a cooldown, then its usage endpoint
	// breaks ("http 401" is a permanent fetch error: no retries).
	m.RecordRateLimit("zhipu", time.Now().Add(80*time.Minute), Transient, 0)
	snapshot = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "http 401"}
	tr.PollAll(time.Now())

	if kept := m.Quota("zhipu"); kept == nil || kept.Err != "" ||
		kept.Billing != provider.BillingPlan || !kept.AsOf.Equal(seeded.AsOf) {
		t.Fatalf("failed sync mutated the last snapshot: %+v", kept)
	}
	if m.TargetHealthy("zhipu", "glm-5.3", time.Now()) {
		t.Fatal("failed sync must not clear the 429 cooldown (no side effects)")
	}

	// Recovery: the next successful sync switches over and lifts the cooldown.
	snapshot = recoveredSnapshot(time.Now(), 0.8)
	tr.PollAll(time.Now())
	if !m.TargetHealthy("zhipu", "glm-5.3", time.Now()) {
		t.Fatal("successful sync must clear the stale cooldown again")
	}
	if s := m.Quota("zhipu"); s.Err != "" || s.AsOf.Equal(seeded.AsOf) {
		t.Fatalf("successful sync did not switch the snapshot over: %+v", s)
	}
}

// TestPollOneSyncFailureKeepsLastSnapshot: the manual per-account "Refresh
// usage" path follows the same contract — a failing endpoint keeps the last
// snapshot, leaves cooldowns alone, and reports the refresh as NOT successful
// (the keep is a no-op, not a committed snapshot).
func TestPollOneSyncFailureKeepsLastSnapshot(t *testing.T) {
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "60s"}}
	m := newTestManager(0)
	snapshot := recoveredSnapshot(time.Now(), 0.7)
	prov := &quotaFake{quota: func() (*provider.QuotaSnapshot, error) { cp := *snapshot; return &cp, nil }}
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{"zhipu": prov}
		}, m)

	if !tr.PollOne("zhipu") {
		t.Fatal("seed PollOne rejected")
	}
	seeded := m.Quota("zhipu")

	m.RecordRateLimit("zhipu", time.Now().Add(80*time.Minute), Transient, 0)
	snapshot = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "http 403"}
	if tr.PollOne("zhipu") {
		t.Fatal("failed sync must not report a successful refresh — nothing was committed")
	}
	if kept := m.Quota("zhipu"); kept == nil || kept.Err != "" ||
		kept.Billing != provider.BillingPlan || !kept.AsOf.Equal(seeded.AsOf) {
		t.Fatalf("failed PollOne mutated the last snapshot: %+v", kept)
	}
	if m.TargetHealthy("zhipu", "glm-5.3", time.Now()) {
		t.Fatal("failed PollOne must not clear the 429 cooldown")
	}
}

// TestPollOneSyncFailureDoesNotPersist: a failed PollOne/RefreshOne keeps the
// last-known-good snapshot and must not touch the state file at all — the
// previous bool signal reported the no-op keep as a commit, so every failed
// sync rewrote quota_state.json with identical content.
func TestPollOneSyncFailureDoesNotPersist(t *testing.T) {
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "60s"}}
	m := newTestManager(0)
	snapshot := recoveredSnapshot(time.Now(), 0.7)
	prov := &quotaFake{quota: func() (*provider.QuotaSnapshot, error) { cp := *snapshot; return &cp, nil }}
	path := filepath.Join(t.TempDir(), "quota_state.json")
	tr := NewQuotaTracker(path, func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{"zhipu": prov}
		}, m)

	tr.PollAll(time.Now())
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("seed poll did not persist: %v", err)
	}
	// A rewrite goes through tmp+rename, so it always produces a NEW inode —
	// os.SameFile detects it regardless of the filesystem's mtime granularity.
	assertNotRewritten := func(op string) {
		t.Helper()
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
			t.Fatalf("%s rewrote the state file for a no-op keep", op)
		}
	}

	snapshot = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "http 401"}
	if tr.PollOne("zhipu") {
		t.Fatal("failed PollOne must report false")
	}
	assertNotRewritten("failed PollOne")

	tr.RefreshOne("zhipu")
	assertNotRewritten("failed RefreshOne")
	if kept := m.Quota("zhipu"); kept == nil || kept.Err != "" {
		t.Fatalf("failed sync mutated the last snapshot: %+v", kept)
	}
}

// TestRefreshOneSyncFailureDoesNotDebounce: a failed RefreshOne learns
// nothing, so it must not stamp the debounce marker — the next refresh (the
// upstream recovered) must run immediately instead of being swallowed by the
// half-interval debounce.
func TestRefreshOneSyncFailureDoesNotDebounce(t *testing.T) {
	cfg := &configdomain.Config{Scheduling: configdomain.Scheduling{QuotaPollInterval: "60s"}}
	m := newTestManager(0)
	snapshot := &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "http 401"}
	prov := &quotaFake{quota: func() (*provider.QuotaSnapshot, error) { cp := *snapshot; return &cp, nil }}
	tr := NewQuotaTracker("", func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider {
			return map[string]provider.Provider{"zhipu": prov}
		}, m)

	// Seed a good snapshot, then fail the refresh: the keep is a no-op.
	tr.SetSnapshot("zhipu", recoveredSnapshot(time.Now(), 0.7))
	tr.RefreshOne("zhipu")
	if kept := m.Quota("zhipu"); kept == nil || kept.Err != "" {
		t.Fatalf("failed RefreshOne mutated the last snapshot: %+v", kept)
	}

	// The upstream recovers; the very next refresh (well inside the 30s
	// debounce half-interval) must commit — the failed one did not debounce.
	snapshot = recoveredSnapshot(time.Now(), 0.9)
	tr.RefreshOne("zhipu")
	got := m.Quota("zhipu")
	if got == nil || got.Err != "" || len(got.Windows) == 0 || got.Windows[0].RemainingPct != 0.9 {
		t.Fatalf("refresh after a failed refresh was debounced: %+v", got)
	}
}
