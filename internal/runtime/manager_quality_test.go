package runtime

import (
	"math"
	"reflect"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

func TestQualityEWMA(t *testing.T) {
	t.Parallel()

	// One half-life blends halfway to the sample; a converged signal is stable.
	if got := ewma(0, 1, qualityHalfLife); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("one half-life blend = %v, want 0.5", got)
	}
	if got := ewma(1, 0, qualityHalfLife); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("one half-life decay = %v, want 0.5", got)
	}
	if got := ewma(0.4, 0.4, 10*time.Minute); math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("converged signal drifted: %v", got)
	}

	// decayed() projects without mutating.
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	q := &providerQuality{errRate: 1, errAt: now.Add(-qualityHalfLife), ttftNorm: 1, ttftAt: now.Add(-qualityHalfLife)}
	errRate, ttft := q.decayed(now)
	if math.Abs(errRate-0.5) > 1e-9 || math.Abs(ttft-0.5) > 1e-9 {
		t.Fatalf("decayed = %v/%v, want 0.5/0.5", errRate, ttft)
	}
	if q.errRate != 1 || q.ttftNorm != 1 {
		t.Fatalf("decayed mutated state: %+v", q)
	}
	if errRate, _ := (providerQuality{}).decayed(now); errRate != 0 {
		t.Fatalf("nil quality = %v, want 0", errRate)
	}
}

func TestDecideOrderAppliesQualityPenalty(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	targets := []Target{
		{Provider: "degrading", Priority: 1},
		{Provider: "healthy", Priority: 1},
	}
	quota := scheduleQuota(provider.BillingPlan, .5, now)
	input := ScheduleInput{
		Exposed: "route", Targets: targets, Now: now, QuotaMaxAge: time.Hour,
		Dwell: time.Hour, SwitchMargin: .15,
		QualityErrWeight: 1.0, QualityTTFTWeight: 0.2,
	}

	m := newTestManager(3)
	setScheduleQuota(t, m, "degrading", quota, 3)
	setScheduleQuota(t, m, "healthy", quota, 3)

	// Zero weights (both zero-value) reproduce the pre-quality order exactly.
	zeroWeight := input
	zeroWeight.QualityErrWeight, zeroWeight.QualityTTFTWeight = 0, 0
	if result := m.DecideOrder(zeroWeight); result.Order[0] != 0 {
		t.Fatalf("zero weights changed order: %+v", result)
	}

	// A degrading provider (fresh 60% error EWMA) sinks below an equally
	// provisioned healthy one. Quality state is published through the same
	// copy-on-write pointer the record paths use.
	m.quality.Store(&map[string]providerQuality{"degrading": {errRate: .6, errAt: now}})
	result := m.DecideOrder(input)
	if !reflect.DeepEqual(result.Order, []int{1, 0}) {
		t.Fatalf("degrading provider did not sink: %+v", result)
	}
	if result.Facts[0].QualityPenalty <= 0 || result.Facts[1].QualityPenalty != 0 {
		t.Fatalf("penalty facts = %+v", result.Facts)
	}

	// The penalty expires with the EWMA: an hour-old failure burst no longer
	// penalizes (decays ~0 at 30 half-lives), restoring the original order.
	m.quality.Store(&map[string]providerQuality{"degrading": {errRate: .6, errAt: now.Add(-time.Hour)}})
	if result = m.DecideOrder(input); result.Order[0] != 0 {
		t.Fatalf("stale penalty did not expire: %+v", result)
	}

	// PreviewOrder (dashboard path) computes the same penalty from the
	// detached snapshot as the live decision at the same state.
	m.quality.Store(&map[string]providerQuality{"degrading": {errRate: .6, errAt: now}})
	live := m.DecideOrder(input)
	preview := m.Dashboard(now).PreviewOrder(input)
	if !reflect.DeepEqual(preview.Order, []int{1, 0}) ||
		preview.Facts[0].QualityPenalty != live.Facts[0].QualityPenalty {
		t.Fatalf("preview diverged from live decision: %+v vs %+v", preview, live)
	}
	if got := m.Dashboard(now).Quality["degrading"].ErrorRate; got <= 0 {
		t.Fatalf("dashboard quality missing: %+v", got)
	}
}

func TestDecideOrderStickyEscapesDegradingAccount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	m := newTestManager(4)
	targets := []Target{
		{Provider: "current", Priority: 1},
		{Provider: "other", Priority: 1},
	}
	quota := scheduleQuota(provider.BillingPlan, .5, now)
	setScheduleQuota(t, m, "current", quota, 4)
	setScheduleQuota(t, m, "other", quota, 4)
	m.SetSticky("session", Sticky{Provider: "current", Since: now.Add(-2 * time.Hour)}, 4)
	input := ScheduleInput{
		Exposed: "route", SessionKey: "session", Targets: targets,
		RouteKeys: map[string]bool{"route": true},
		Dwell:     time.Hour, SwitchMargin: .15, Now: now, QuotaMaxAge: time.Hour,
		QualityErrWeight: 1.0,
		Generation:       4,
	}

	// Equal quotas: below the switch margin, dwell-expired sticky holds.
	if result := m.DecideOrder(input); result.StickyProvider != "current" {
		t.Fatalf("sticky did not hold: %+v", result)
	}
	// The sticky account's fresh 40% error EWMA pushes the score gap (0.4)
	// past the margin (0.15): sticky escapes through the SAME margin gate.
	m.quality.Store(&map[string]providerQuality{"current": {errRate: .4, errAt: now}})
	if result := m.DecideOrder(input); result.StickyProvider != "other" {
		t.Fatalf("degrading sticky did not escape: %+v", result)
	}
}

func TestQualityLifecycle(t *testing.T) {
	t.Parallel()

	m := newTestManager(5)

	// Record* funnels move the error EWMA.
	m.RecordFailure("p", 3, time.Minute, 5)
	m.RecordFailure("p", 3, time.Minute, 5)
	m.RecordSuccess("p", "m", 5)
	if got := m.qualitySnapshot()["p"].errRate; got <= 0 || got >= 1 {
		t.Fatalf("errRate after fail/fail/success = %v", got)
	}

	// Committed TTFT folds in (fresh provider: first sample initializes);
	// non-positive timings are ignored.
	m.RecordAttemptQuality("r", 20*time.Second, 5)
	m.RecordAttemptQuality("r", 0, 5)
	if got := m.qualitySnapshot()["r"].ttftNorm; got != 1 {
		t.Fatalf("ttftNorm = %v, want clamped 1", got)
	}

	// unfreeze clears quality alongside health: the operator's "retry now"
	// must not leave a stale demotion behind.
	m.ResetHealth("p", nil)
	if _, kept := m.qualitySnapshot()["p"]; kept {
		t.Fatalf("ResetHealth kept quality: %+v", m.qualitySnapshot())
	}

	// Generation replace drops quality like the rest of the routing state.
	m.RecordFailure("q", 3, time.Minute, 5)
	m.ReplaceGeneration(6)
	if len(m.qualitySnapshot()) != 0 {
		t.Fatalf("ReplaceGeneration kept quality: %+v", m.qualitySnapshot())
	}
	// Stale-generation mutations are rejected.
	m.RecordFailure("q", 3, time.Minute, 5)
	if len(m.qualitySnapshot()) != 0 {
		t.Fatalf("stale generation mutated quality: %+v", m.qualitySnapshot())
	}
	// TTFT samples pass the same generation gate as the error-rate samples.
	m.RecordAttemptQuality("q", time.Second, 5)
	if len(m.qualitySnapshot()) != 0 {
		t.Fatalf("stale generation mutated quality via TTFT: %+v", m.qualitySnapshot())
	}
}

func TestQualityProjectionRevalidatesAcrossGenerationReplacement(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	old := map[string]providerQuality{
		"old-provider": {errRate: 1, errAt: now},
	}
	m.quality.Store(&old)
	projected := m.projectQualitySnapshot(now)
	if projected.status["old-provider"].ErrorRate == 0 {
		t.Fatal("precondition: detached projection did not contain old quality")
	}

	// Deterministically model the only dangerous interleaving: projection from
	// generation 1, then ReplaceGeneration publishes generation 2 before the
	// reader takes m.mu. Revalidation must discard the old projection.
	m.ReplaceGeneration(2)
	m.mu.Lock()
	m.ensureLocked()
	quality := m.reconcileQualityProjectionLocked(projected, now)
	generation := m.generation
	m.mu.Unlock()
	if generation != 2 {
		t.Fatalf("generation = %d, want 2", generation)
	}
	if len(quality) != 0 {
		t.Fatalf("generation 2 reused generation 1 quality: %+v", quality)
	}
}

// TestQualityTTFTDecaysDuringFailureStretch: a slow-TTFT penalty must decay
// from its OWN last TTFT observation. A shared `updated` anchor let error
// samples (a provider failing continuously) keep the timestamp fresh, so the
// stale TTFT penalty never decayed at decision time and the recovery TTFT
// sample blended in with dt≈0 (alpha≈0) — the freeze the review found.
func TestQualityTTFTDecaysDuringFailureStretch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	m.quality.Store(&map[string]providerQuality{"p": {ttftNorm: 1, ttftAt: now}})
	// Ten minutes of continuous failures, no TTFT samples in between.
	for i := 1; i <= 5; i++ {
		m.recordQualityLocked("p", 1, now.Add(time.Duration(2*i)*time.Minute))
	}
	_, ttft := m.qualitySnapshot()["p"].decayed(now.Add(10 * time.Minute))
	if ttft > 0.1 {
		t.Fatalf("TTFT penalty after a 10-minute failure stretch = %v, want decayed to ≤0.1 (half-life 2m)", ttft)
	}
}
