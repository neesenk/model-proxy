package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// metricsEvent identifies a counter to bump on the forward hot path.
type metricsEvent string

const (
	evRequests       metricsEvent = "requests"
	evFailovers      metricsEvent = "failovers"
	evRateLimited429 metricsEvent = "rate_limited_429"
	evFailures       metricsEvent = "failures"
)

type providerMetrics struct {
	Requests       atomic.Uint64
	Failovers      atomic.Uint64
	RateLimited429 atomic.Uint64
	Failures       atomic.Uint64
	LastRequestAt  atomic.Int64 // unix seconds
}

// providerMetricsSnapshot is the JSON-friendly, lock-acquired copy.
type providerMetricsSnapshot struct {
	Requests       uint64 `json:"requests"`
	Failovers      uint64 `json:"failovers"`
	RateLimited429 uint64 `json:"rate_limited_429"`
	Failures       uint64 `json:"failures"`
	LastRequestAt  int64  `json:"last_request_at"`
}

type metricsStore struct {
	started time.Time
	// mu guards the map only. Increments acquire mu briefly to get-or-create
	// the per-provider entry, then do an atomic add; snapshots acquire mu to
	// iterate the map.
	mu sync.Mutex
	m  map[string]*providerMetrics
}

func newMetricsStore() *metricsStore {
	return &metricsStore{started: time.Now(), m: map[string]*providerMetrics{}}
}

func (s *metricsStore) startedAt() time.Time { return s.started }

// entry returns the per-provider metrics, creating it if absent.
func (s *metricsStore) entry(provider string) *providerMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm := s.m[provider]
	if pm == nil {
		pm = &providerMetrics{}
		s.m[provider] = pm
	}
	return pm
}

// inc bumps one counter for a provider (hot path: one atomic add + map lookup).
func (s *metricsStore) inc(provider string, ev metricsEvent) {
	pm := s.entry(provider)
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
	}
}

func (s *metricsStore) snapshot() map[string]providerMetricsSnapshot {
	s.mu.Lock()
	ids := make([]string, 0, len(s.m))
	for k := range s.m {
		ids = append(ids, k)
	}
	s.mu.Unlock()
	out := map[string]providerMetricsSnapshot{}
	for _, id := range ids {
		pm := s.entry(id)
		out[id] = providerMetricsSnapshot{
			Requests:       pm.Requests.Load(),
			Failovers:      pm.Failovers.Load(),
			RateLimited429: pm.RateLimited429.Load(),
			Failures:       pm.Failures.Load(),
			LastRequestAt:  pm.LastRequestAt.Load(),
		}
	}
	return out
}
