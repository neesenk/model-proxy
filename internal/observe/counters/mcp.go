package counters

import (
	"sync"
	"sync/atomic"
	"time"
)

// MCPStats is the MCP gateway's call counter: per exposed name (server or
// route — they share one namespace) calls/errors/latency. In-memory and
// process-lifetime like the hot metrics counters (restart resets), but
// DELIBERATELY separate from the LLM metrics pipeline: MCP exchanges carry no
// tokens and must not pollute the provider/model dashboards (Status/Analytics
// minute_buckets/agent_buckets). The per-minute flusher persists deltas to the
// dedicated mcp_buckets table in stats.db.
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

// MCPStatRaw is the non-averaged counter totals used by the stats flusher.
type MCPStatRaw struct {
	Calls      uint64
	Errors     uint64
	LatencySum uint64
	LastCallAt int64
}

// RawSnapshot returns per-name cumulative totals without averaging. This is the
// durable-persistence view of the counter; Snapshot remains the dashboard view.
func (s *MCPStats) RawSnapshot() map[string]MCPStatRaw {
	if s == nil {
		return map[string]MCPStatRaw{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]MCPStatRaw, len(s.m))
	for name, e := range s.m {
		out[name] = MCPStatRaw{
			Calls:      e.calls.Load(),
			Errors:     e.errors.Load(),
			LatencySum: e.latencySum.Load(),
			LastCallAt: e.lastCallAt.Load(),
		}
	}
	return out
}

// Reset clears all per-name counters. It is used by the stats flusher's reset
// path so the next diff baseline starts from zero.
func (s *MCPStats) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = map[string]*mcpStatEntry{}
}
