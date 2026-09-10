package stats

import (
	"path/filepath"
	"testing"
	"time"

	obscounters "model-proxy/internal/observe/counters"
)

// TestAgentRestoreOnBoot verifies the agent-dimension boot restore end to end
// through Bootstrap: persisted agent_buckets seed the hot AgentCounter (so the
// Agents card survives restarts like the token counters) AND the flusher's
// agent diff baseline (so the first post-boot flush does not re-count history
// into agent_buckets).
func TestAgentRestoreOnBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	ss, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	// Minute 1 traffic from a "previous process": pi/zhipu/glm + a failure.
	m := obscounters.NewMetricsStore()
	tc := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	f := NewFlusher(ss, m, tc, agents, nil, nil)
	agents.IncRequests("pi", "zhipu", "glm")
	agents.AddTokens("pi", "zhipu", "glm", obscounters.TokenUsage{Input: 100, Output: 40, CacheCreation: 6, CacheRead: 30})
	agents.IncFailure("pi", "zhipu", "glm")
	if !f.Flush(time.Now()) {
		t.Fatal("first flush reported no write")
	}
	if err := ss.Close(); err != nil {
		t.Fatal(err)
	}

	// "Reboot": Bootstrap over the same DB seeds the counters and baselines.
	m2 := obscounters.NewMetricsStore()
	tc2 := obscounters.NewTokenCounter()
	agents2 := obscounters.NewAgentCounter()
	res := Bootstrap(path, 0, t.TempDir(), m2, tc2, agents2)
	if res.Store == nil || res.Flusher == nil {
		t.Fatal("bootstrap failed to open store")
	}
	defer res.Store.Close()

	// Hot counter restored: the Agents read shows the persisted totals.
	snap := agents2.Snapshot()
	got := snap[obscounters.AgentKey{Agent: "pi", Provider: "zhipu", Model: "glm"}]
	if got.Requests != 1 || got.Input != 100 || got.Output != 40 ||
		got.CacheCreation != 6 || got.CacheRead != 30 || got.Failures != 1 {
		t.Fatalf("restored agent counter = %+v, want reqs=1 in=100 out=40 cc=6 cr=30 fail=1", got)
	}

	// Seeding alone must not double-count: an idle flush (no new traffic)
	// writes nothing, and one new request then flushes as exactly one delta.
	if res.Flusher.Flush(time.Now()) {
		t.Fatal("idle flush after boot restore wrote a batch (agent baseline not seeded)")
	}
	agents2.IncRequests("pi", "zhipu", "glm")
	if !res.Flusher.Flush(time.Now().Add(time.Minute)) {
		t.Fatal("flush after new traffic reported no write")
	}
	rows, err := res.Store.QueryAgents(0, time.Now().Add(time.Hour).Unix(), "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var totalReq uint64
	for _, r := range rows {
		totalReq += r.Requests
	}
	if totalReq != 2 {
		t.Fatalf("persisted agent requests = %d, want 2 (1 historical + 1 new, no re-count)", totalReq)
	}
}

// TestAgentCounterSeed pins Seed's exact-baseline semantics (mirrors
// TestSeedRestore for the token counters).
func TestAgentCounterSeed(t *testing.T) {
	a := obscounters.NewAgentCounter()
	k := obscounters.AgentKey{Agent: "codex", Provider: "z", Model: "glm"}
	a.Seed(k, obscounters.AgentCount{Requests: 9, Input: 10, Output: 11, CacheCreation: 2, CacheRead: 5, LatencySum: 12, Failures: 13})
	snap := a.Snapshot()
	got := snap[k]
	if got.Requests != 9 || got.Input != 10 || got.Output != 11 ||
		got.CacheCreation != 2 || got.CacheRead != 5 || got.LatencySum != 12 || got.Failures != 13 {
		t.Fatalf("agent seed = %+v, want exact baseline", got)
	}
	// Post-seed increments accumulate on top of the baseline.
	a.IncRequests(k.Agent, k.Provider, k.Model)
	if got := a.Snapshot()[k]; got.Requests != 10 {
		t.Fatalf("requests after seed+inc = %d, want 10", got.Requests)
	}
}
