package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// pmKey is the (provider, model) key shared by metrics and token counters.
// For pooled providers Provider is the virtual id ("name#<accountID>"); Model is
// the rewrite-target upstream model name (not the exposed/client name).
type pmKey struct {
	Provider string
	Model    string
}

// metricsEvent identifies a counter to bump on the forward hot path.
type metricsEvent string

const (
	evRequests       metricsEvent = "requests"
	evFailovers      metricsEvent = "failovers"
	evRateLimited429 metricsEvent = "rate_limited_429"
	evFailures       metricsEvent = "failures"
	// Fusion observability reuses the generic per-minute buckets under the
	// virtual key ("fusion", <workflow>): an orchestration run counts as a
	// "request", a degraded run (answered directly instead of orchestrated)
	// counts into the failovers column — the closest existing "fell back"
	// semantic. /api/stats and the stats CLI thus get a fusion time series
	// with zero schema change.
	evFusionRuns     metricsEvent = "fusion_runs"
	evFusionDegraded metricsEvent = "fusion_degraded"
)

type providerMetrics struct {
	Requests       atomic.Uint64
	Failovers      atomic.Uint64
	RateLimited429 atomic.Uint64
	Failures       atomic.Uint64
	LastRequestAt  atomic.Int64 // unix seconds
	// LatencySum/TTFTSum are cumulative millisecond sums over COMMITTED (served)
	// responses; avg = sum/requests. Latency = full request wall-clock (send →
	// end of streamed body); TTFT = send → first byte written to the client.
	LatencySum atomic.Uint64
	TTFTSum    atomic.Uint64
}

// providerMetricsSnapshot is the JSON-friendly, lock-acquired copy.
type providerMetricsSnapshot struct {
	Requests       uint64 `json:"requests"`
	Failovers      uint64 `json:"failovers"`
	RateLimited429 uint64 `json:"rate_limited_429"`
	Failures       uint64 `json:"failures"`
	LastRequestAt  int64  `json:"last_request_at"`
	LatencySum     uint64 `json:"latency_ms_sum"`
	TTFTSum        uint64 `json:"ttft_ms_sum"`
}

type metricsStore struct {
	started time.Time
	// mu guards the map only. Increments acquire mu briefly to get-or-create
	// the per-(provider,model) entry, then do an atomic add; snapshots acquire mu
	// to iterate the map.
	mu sync.Mutex
	m  map[pmKey]*providerMetrics
}

func newMetricsStore() *metricsStore {
	return &metricsStore{started: time.Now(), m: map[pmKey]*providerMetrics{}}
}

func (s *metricsStore) startedAt() time.Time { return s.started }

// entry returns the per-(provider,model) metrics, creating it if absent.
func (s *metricsStore) entry(k pmKey) *providerMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm := s.m[k]
	if pm == nil {
		pm = &providerMetrics{}
		s.m[k] = pm
	}
	return pm
}

// inc bumps one counter for a (provider, model) (hot path: one atomic add + map
// lookup). Model is the rewrite-target upstream model (t.Model on the forward
// path), so failovers/429/failures are attributed to the same model the request
// was sent to.
func (s *metricsStore) inc(provider, model string, ev metricsEvent) {
	pm := s.entry(pmKey{Provider: provider, Model: model})
	switch ev {
	case evRequests:
		pm.Requests.Add(1)
		pm.LastRequestAt.Store(time.Now().Unix())
	case evFailovers:
		pm.Failovers.Add(1)
	case evRateLimited429:
		pm.RateLimited429.Add(1)
	case evFailures:
		pm.Failures.Add(1)
	case evFusionRuns:
		pm.Requests.Add(1)
		pm.LastRequestAt.Store(time.Now().Unix())
	case evFusionDegraded:
		pm.Failovers.Add(1)
	}
}

// addLatency records one served response's wall-clock latency and time-to-first-
// token (milliseconds) for a (provider, model), accumulating into the sums the
// flusher diffs. Called once per committed target on the forward hot path.
func (s *metricsStore) addLatency(provider, model string, latencyMs, ttftMs uint64) {
	pm := s.entry(pmKey{Provider: provider, Model: model})
	pm.LatencySum.Add(latencyMs)
	pm.TTFTSum.Add(ttftMs)
}

// snapshot returns a detached per-(provider,model) copy. Callers may read the
// returned map without holding the lock.
func (s *metricsStore) snapshot() map[pmKey]providerMetricsSnapshot {
	s.mu.Lock()
	keys := make([]pmKey, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	out := map[pmKey]providerMetricsSnapshot{}
	for _, k := range keys {
		pm := s.entry(k)
		out[k] = providerMetricsSnapshot{
			Requests:       pm.Requests.Load(),
			Failovers:      pm.Failovers.Load(),
			RateLimited429: pm.RateLimited429.Load(),
			Failures:       pm.Failures.Load(),
			LastRequestAt:  pm.LastRequestAt.Load(),
			LatencySum:     pm.LatencySum.Load(),
			TTFTSum:        pm.TTFTSum.Load(),
		}
	}
	return out
}

// aggregateByProvider collapses the per-(provider,model) counters to per-provider
// totals (summing across models). Used by /api/status so the existing
// provider-keyed "counters" shape (and the Web UI Providers card) is unchanged
// despite the store now being model-aware.
func (s *metricsStore) aggregateByProvider() map[string]providerMetricsSnapshot {
	per := map[string]providerMetricsSnapshot{}
	for k, snap := range s.snapshot() {
		cur := per[k.Provider]
		cur.Requests += snap.Requests
		cur.Failovers += snap.Failovers
		cur.RateLimited429 += snap.RateLimited429
		cur.Failures += snap.Failures
		cur.LatencySum += snap.LatencySum
		cur.TTFTSum += snap.TTFTSum
		if snap.LastRequestAt > cur.LastRequestAt {
			cur.LastRequestAt = snap.LastRequestAt
		}
		per[k.Provider] = cur
	}
	return per
}

// seed sets a (provider,model) entry's counters to a baseline value (used on
// boot to restore cumulative totals persisted in SQLite). Seeds are rare
// (boot-only), so the per-key Lock/entry overhead is fine.
func (s *metricsStore) seed(k pmKey, snap providerMetricsSnapshot) {
	pm := s.entry(k)
	pm.Requests.Store(snap.Requests)
	pm.Failovers.Store(snap.Failovers)
	pm.RateLimited429.Store(snap.RateLimited429)
	pm.Failures.Store(snap.Failures)
	pm.LastRequestAt.Store(snap.LastRequestAt)
	pm.LatencySum.Store(snap.LatencySum)
	pm.TTFTSum.Store(snap.TTFTSum)
}

// reset zeroes every counter (in-memory). The SQLite history is cleared
// separately by statsStore.resetAll; the two are called together by
// Proxy.resetStats so "reset counters" zeroes both.
func (s *metricsStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = map[pmKey]*providerMetrics{}
}
