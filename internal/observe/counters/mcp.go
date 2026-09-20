package counters

import (
	"sync"
	"sync/atomic"
	"time"
)

// MCPStats is the MCP gateway's call counter: per exposed name (server or
// route — they share one namespace) calls/errors/latency, plus a per-tool view
// of the tools/call exchanges under each name. In-memory and process-lifetime
// like the hot metrics counters (restart resets), but DELIBERATELY separate
// from the LLM metrics pipeline: MCP exchanges carry no tokens and must not
// pollute the provider/model dashboards (Status/Analytics minute_buckets/
// agent_buckets). The per-minute flusher persists deltas to the dedicated
// mcp_buckets / mcp_tool_buckets tables in stats.db.
type MCPStats struct {
	mu   sync.Mutex // guards m and tools; increments are atomic after get-or-create
	m    map[string]*mcpStatEntry
	tool map[MCPToolKey]*mcpStatEntry
}

// MCPToolKey is the per-tool counter key: the exposed name (server or route)
// the client addressed, and the client-facing tool name (for route exchanges
// that is the canonical tool name, not the backend's rewritten name).
type MCPToolKey struct {
	Name string
	Tool string
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

func NewMCPStats() *MCPStats {
	return &MCPStats{m: map[string]*mcpStatEntry{}, tool: map[MCPToolKey]*mcpStatEntry{}}
}

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

// RecordTool is Record for one (name, tool) pair: the per-tool dimension of
// the mcp_tool_buckets table. Empty tool names (method without a parseable
// params.name) are skipped — only tools/call carries a tool name.
func (s *MCPStats) RecordTool(name, tool string, status int, latencyMs int64) {
	if s == nil || name == "" || tool == "" {
		return
	}
	s.mu.Lock()
	e := s.tool[MCPToolKey{Name: name, Tool: tool}]
	if e == nil {
		e = &mcpStatEntry{}
		s.tool[MCPToolKey{Name: name, Tool: tool}] = e
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

// RawToolSnapshot returns per-(name, tool) cumulative totals. This is the
// durable-persistence view feeding the flusher's mcp_tool_buckets channel.
func (s *MCPStats) RawToolSnapshot() map[MCPToolKey]MCPStatRaw {
	if s == nil {
		return map[MCPToolKey]MCPStatRaw{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[MCPToolKey]MCPStatRaw, len(s.tool))
	for key, e := range s.tool {
		out[key] = MCPStatRaw{
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
	s.tool = map[MCPToolKey]*mcpStatEntry{}
}
