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
type providerQuality struct {
	errRate  float64 // 0..1, sample 1 on RecordFailure, 0 on RecordSuccess
	ttftNorm float64 // 0..1, sample min(ttft/ttftReference, 1) on committed success
	updated  time.Time
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
func (q *providerQuality) decayed(now time.Time) (errRate, ttftNorm float64) {
	if q == nil {
		return 0, 0
	}
	return ewma(q.errRate, 0, now.Sub(q.updated)), ewma(q.ttftNorm, 0, now.Sub(q.updated))
}

// recordSample applies one error-rate sample (0 success / 1 failure). Caller
// holds m.mu and has passed the generation check.
func (m *Manager) recordQualityLocked(name string, sample float64, now time.Time) {
	m.ensureLocked()
	q := m.quality[name]
	if q == nil {
		q = &providerQuality{}
		m.quality[name] = q
	}
	if q.updated.IsZero() {
		q.errRate = sample
	} else {
		q.errRate = ewma(q.errRate, sample, now.Sub(q.updated))
	}
	q.updated = now
}

// RecordAttemptQuality folds a committed attempt's TTFT into the provider's
// quality EWMA. Called from the effects layer on committed 2xx responses only
// (terminal errors already flow through RecordFailure); TTFTMilliseconds <= 0
// (no timing captured) updates nothing. Quality is heuristic state, so a
// generation-less call applies to the current generation like other
// GenerationArg-0 callers.
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
	q := m.quality[name]
	if q == nil {
		q = &providerQuality{}
		m.quality[name] = q
	}
	sample := ttft.Seconds() / ttftReference.Seconds()
	if sample > 1 {
		sample = 1
	}
	if q.updated.IsZero() {
		q.ttftNorm = sample
	} else {
		q.ttftNorm = ewma(q.ttftNorm, sample, now.Sub(q.updated))
	}
	q.updated = now
}
