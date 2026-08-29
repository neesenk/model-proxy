package runtime

import (
	"math"
	"time"
)

// qualityHalfLife is the EWMA half-life for per-provider quality signals.
// Two minutes is short enough to react to a degrading account within a coding
// session, long enough that a single hiccup doesn't swing the score. Not
// configurable: only the ordering WEIGHTS are operator knobs (Scheduling);
// the decay constant stays a code default until a real need shows up.
const qualityHalfLife = 2 * time.Minute

// ttftReference normalizes TTFT into 0..1 for the quality penalty: a provider
// whose EWMA TTFT reaches this reference eats the full ttft weight.
const ttftReference = 10 * time.Second

// providerQuality is per-provider quality EWMA state, updated by the same
// Record* funnels that feed the circuit breaker. Deliberately per PROVIDER
// (not per model): circuit health already works at that granularity, and
// model-level faults have their own lock mechanism.
//
// Each signal carries its OWN anchor timestamp: a shared anchor let error
// samples keep the timestamp fresh while an old TTFT penalty stayed frozen
// (and the recovery TTFT sample blended in with dt≈0, alpha≈0). Decay is
// measured from the signal's last observation.
type providerQuality struct {
	errRate  float64 // 0..1, sample 1 on RecordFailure, 0 on RecordSuccess
	errAt    time.Time
	ttftNorm float64 // 0..1, sample min(ttft/ttftReference, 1) on committed success
	ttftAt   time.Time
}

// ewma applies one sample with time-based decay toward the previous value.
// The first sample initializes directly (no cold-start lag from zero).
func ewma(prev, sample float64, dt time.Duration) float64 {
	if dt < 0 {
		dt = 0
	}
	alpha := 1 - math.Pow(0.5, dt.Seconds()/qualityHalfLife.Seconds())
	return prev*(1-alpha) + sample*alpha
}

// decayed returns the signals projected to `now` WITHOUT mutating state: a
// provider that stopped failing an hour ago must not carry a stale penalty
// into the next scheduling decision.
func (q providerQuality) decayed(now time.Time) (errRate, ttftNorm float64) {
	return ewma(q.errRate, 0, now.Sub(q.errAt)), ewma(q.ttftNorm, 0, now.Sub(q.ttftAt))
}

// qualitySnapshot loads the immutable quality state without the mutex (nil
// maps become empty pre-first-record).
func (m *Manager) qualitySnapshot() map[string]providerQuality {
	if p := m.quality.Load(); p != nil {
		return *p
	}
	return nil
}

// storeQualityLocked publishes a copy-on-write quality map. Caller holds m.mu.
func (m *Manager) storeQualityLocked(next map[string]providerQuality) {
	m.quality.Store(&next)
}

// recordSample applies one error-rate sample (0 success / 1 failure). Caller
// holds m.mu and has passed the generation check.
func (m *Manager) recordQualityLocked(name string, sample float64, now time.Time) {
	m.ensureLocked()
	cur := m.qualitySnapshot()
	q := cur[name]
	if q.errAt.IsZero() {
		q.errRate = sample
	} else {
		q.errRate = ewma(q.errRate, sample, now.Sub(q.errAt))
	}
	q.errAt = now
	next := make(map[string]providerQuality, len(cur)+1)
	for k, v := range cur {
		next[k] = v
	}
	next[name] = q
	m.storeQualityLocked(next)
}

// RecordAttemptQuality folds a committed attempt's TTFT into the provider's
// quality EWMA. Called from the effects layer on committed 2xx responses only
// (terminal errors already flow through RecordFailure); TTFTMilliseconds <= 0
// (no timing captured) updates nothing. TTFT samples pass the SAME generation
// gate as the error-rate samples: the effects layer binds the request's
// captured runtime generation, so a pre-reload in-flight commit cannot write
// into the new generation's quality map. Generation-less (0) callers — outside
// the request path — still apply to the current generation.
func (m *Manager) RecordAttemptQuality(name string, ttft time.Duration, generation uint64) {
	if ttft <= 0 {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return
	}
	m.ensureLocked()
	cur := m.qualitySnapshot()
	q := cur[name]
	sample := ttft.Seconds() / ttftReference.Seconds()
	if sample > 1 {
		sample = 1
	}
	if q.ttftAt.IsZero() {
		q.ttftNorm = sample
	} else {
		q.ttftNorm = ewma(q.ttftNorm, sample, now.Sub(q.ttftAt))
	}
	q.ttftAt = now
	next := make(map[string]providerQuality, len(cur)+1)
	for k, v := range cur {
		next[k] = v
	}
	next[name] = q
	m.storeQualityLocked(next)
}
