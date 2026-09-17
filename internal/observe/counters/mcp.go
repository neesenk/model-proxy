package counters

import (
	"sync"
	"sync/atomic"
	"time"
)

// MCPStats is the MCP gateway's call counter: per exposed name (server or
// route — they share one namespace) calls/errors/latency. In-memory and
// process-lifetime like the hot metrics counters (restart resets), but
// DELIBERATELY separate from MetricsStore: MCP exchanges carry no tokens and
// must not pollute the LLM dashboards (Status/Analytics/stats.db).
type MCPStats struct {
	mu sync.Mutex // guards m only; increments are atomic after get-or-create
	m  map[string]*mcpStatEntry
}

type mcpStatEntry struct {
	calls      atomic.Uint64
	errors     atomic.Uint64
	latencySum atomic.Uint64 // ms, cumulative over all calls
	lastCallAt atomic.Int64  // unix seconds
}

// MCPStatSnapshot is the JSON-friendly copy of one name's counters.
type MCPStatSnapshot struct {
	Calls        uint64 `json:"calls"`
	Errors       uint64 `json:"errors"`
	AvgLatencyMs uint64 `json:"avg_latency_ms"`
	LastCallAt   int64  `json:"last_call_at,omitempty"`
}

func NewMCPStats() *MCPStats { return &MCPStats{m: map[string]*mcpStatEntry{}} }

// Record one terminal MCP exchange: a call, an error when status >= 400, and
// its latency.
func (s *MCPStats) Record(name string, status int, latencyMs int64) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	e := s.m[name]
	if e == nil {
		e = &mcpStatEntry{}
		s.m[name] = e
	}
	s.mu.Unlock()
	e.calls.Add(1)
	if status >= 400 {
		e.errors.Add(1)
	}
	if latencyMs > 0 {
		e.latencySum.Add(uint64(latencyMs))
	}
	e.lastCallAt.Store(time.Now().Unix())
}

// Snapshot returns a per-name copy (empty map when no calls yet).
func (s *MCPStats) Snapshot() map[string]MCPStatSnapshot {
	if s == nil {
		return map[string]MCPStatSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]MCPStatSnapshot, len(s.m))
	for name, e := range s.m {
		calls := e.calls.Load()
		var avg uint64
		if calls > 0 {
			avg = e.latencySum.Load() / calls
		}
		out[name] = MCPStatSnapshot{
			Calls:        calls,
			Errors:       e.errors.Load(),
			AvgLatencyMs: avg,
			LastCallAt:   e.lastCallAt.Load(),
		}
	}
	return out
}
