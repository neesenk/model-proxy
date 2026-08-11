package counters

import (
	"sync"
	"sync/atomic"
	"time"
)

// PMKey is the (provider, model) key shared by metrics and token counters.
// For pooled providers Provider is the virtual id ("name#<accountID>"); Model is
// the rewrite-target upstream model name (not the exposed/client name).
type PMKey struct {
	Provider string
	Model    string
}

// MetricsEvent identifies a counter to bump on the forward hot path.
type MetricsEvent string

const (
	EvRequests       MetricsEvent = "requests"
	EvFailovers      MetricsEvent = "failovers"
	EvRateLimited429 MetricsEvent = "rate_limited_429"
	EvFailures       MetricsEvent = "failures"
	// Fusion observability reuses the generic per-minute buckets under the
	// virtual key ("fusion", <workflow>): an orchestration run counts as a
	// "request", a degraded run (answered directly instead of orchestrated)
	// counts into the failovers column — the closest existing "fell back"
	// semantic. /api/stats and the stats CLI thus get a fusion time series
	// with zero schema change.
	EvFusionRuns     MetricsEvent = "fusion_runs"
	EvFusionDegraded MetricsEvent = "fusion_degraded"
)

type ProviderMetrics struct {
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

// ProviderMetricsSnapshot is the JSON-friendly, lock-acquired copy.
type ProviderMetricsSnapshot struct {
	Requests       uint64 `json:"requests"`
	Failovers      uint64 `json:"failovers"`
	RateLimited429 uint64 `json:"rate_limited_429"`
	Failures       uint64 `json:"failures"`
	LastRequestAt  int64  `json:"last_request_at"`
	LatencySum     uint64 `json:"latency_ms_sum"`
	TTFTSum        uint64 `json:"ttft_ms_sum"`
}

type MetricsStore struct {
	started time.Time
	// mu guards the map only. Increments acquire mu briefly to get-or-create
	// the per-(provider,model) entry, then do an atomic add; snapshots acquire mu
	// to iterate the map.
	mu sync.Mutex
	m  map[PMKey]*ProviderMetrics
}

func NewMetricsStore() *MetricsStore {
	return &MetricsStore{started: time.Now(), m: map[PMKey]*ProviderMetrics{}}
}

func (s *MetricsStore) StartedAt() time.Time { return s.started }

// entry returns the per-(provider,model) metrics, creating it if absent.
func (s *MetricsStore) Entry(k PMKey) *ProviderMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm := s.m[k]
	if pm == nil {
		pm = &ProviderMetrics{}
		s.m[k] = pm
	}
	return pm
}

// inc bumps one counter for a (provider, model) (hot path: one atomic add + map
// lookup). Model is the rewrite-target upstream model (t.Model on the forward
// path), so failovers/429/failures are attributed to the same model the request
// was sent to.
func (s *MetricsStore) Inc(provider, model string, ev MetricsEvent) {
	pm := s.Entry(PMKey{Provider: provider, Model: model})
	switch ev {
	case EvRequests:
		pm.Requests.Add(1)
		pm.LastRequestAt.Store(time.Now().Unix())
	case EvFailovers:
		pm.Failovers.Add(1)
	case EvRateLimited429:
		pm.RateLimited429.Add(1)
	case EvFailures:
		pm.Failures.Add(1)
	case EvFusionRuns:
		pm.Requests.Add(1)
		pm.LastRequestAt.Store(time.Now().Unix())
	case EvFusionDegraded:
		pm.Failovers.Add(1)
	}
}

// addLatency records one served response's wall-clock latency and time-to-first-
// token (milliseconds) for a (provider, model), accumulating into the sums the
// flusher diffs. Called once per committed target on the forward hot path.
func (s *MetricsStore) AddLatency(provider, model string, latencyMs, ttftMs uint64) {
	pm := s.Entry(PMKey{Provider: provider, Model: model})
	pm.LatencySum.Add(latencyMs)
	pm.TTFTSum.Add(ttftMs)
}

// snapshot returns a detached per-(provider,model) copy. Callers may read the
// returned map without holding the lock.
func (s *MetricsStore) Snapshot() map[PMKey]ProviderMetricsSnapshot {
	s.mu.Lock()
	keys := make([]PMKey, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	out := map[PMKey]ProviderMetricsSnapshot{}
	for _, k := range keys {
		pm := s.Entry(k)
		out[k] = ProviderMetricsSnapshot{
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
func (s *MetricsStore) AggregateByProvider() map[string]ProviderMetricsSnapshot {
	per := map[string]ProviderMetricsSnapshot{}
	for k, snap := range s.Snapshot() {
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
func (s *MetricsStore) Seed(k PMKey, snap ProviderMetricsSnapshot) {
	pm := s.Entry(k)
	pm.Requests.Store(snap.Requests)
	pm.Failovers.Store(snap.Failovers)
	pm.RateLimited429.Store(snap.RateLimited429)
	pm.Failures.Store(snap.Failures)
	pm.LastRequestAt.Store(snap.LastRequestAt)
	pm.LatencySum.Store(snap.LatencySum)
	pm.TTFTSum.Store(snap.TTFTSum)
}

// reset zeroes every counter (in-memory). The SQLite history is cleared
// separately by internal/observe/stats.Store.Reset; statsFlusher coordinates
// both under one application-level lock.
func (s *MetricsStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = map[PMKey]*ProviderMetrics{}
}
