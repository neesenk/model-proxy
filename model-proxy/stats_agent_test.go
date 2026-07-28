package main

import (
	"strings"
	"testing"
	"time"
)

// TestDiffAgent: per-key deltas clamp at 0 and omit unchanged keys.
func TestDiffAgent(t *testing.T) {
	cur := map[agentKey]agentCount{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5, Input: 10, Output: 2},
		{Agent: "b", Provider: "z", Model: "m"}: {Requests: 3, Input: 0, Output: 0},
	}
	prev := map[agentKey]agentCount{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 2, Input: 10, Output: 0}, // reqs +3, output +2; input unchanged
	}
	d := diffAgent(cur, prev)
	ad, ok := d[agentKey{Agent: "a", Provider: "z", Model: "m"}]
	if !ok || ad.Requests != 3 || ad.Input != 0 || ad.Output != 2 {
		t.Errorf("a delta = %+v want reqs=3 in=0 out=2", ad)
	}
	bd, ok := d[agentKey{Agent: "b", Provider: "z", Model: "m"}]
	if !ok || bd.Requests != 3 {
		t.Errorf("b delta = %+v want reqs=3 (new key)", bd)
	}
	// A key whose counters only decreased (e.g. after reset) clamps to 0 and is
	// omitted when ALL fields are 0.
	d2 := diffAgent(
		map[agentKey]agentCount{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 1}},
		map[agentKey]agentCount{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5}},
	)
	if _, present := d2[agentKey{Agent: "a", Provider: "z", Model: "m"}]; present {
		t.Errorf("all-zero delta should be omitted, got %+v", d2)
	}
}

// TestPruneAgent: retention deletes old agent buckets; 0 retention is a no-op.
func TestPruneAgent(t *testing.T) {
	ss := newTestStatsStore(t)
	old := time.Now().Add(-2*time.Hour).Unix() / 60 * 60
	if err := ss.flushAgentDeltas(old, map[agentKey]agentCount{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// A store with the default 0 retention (newTestStatsStore uses 0) → no prune.
	if err := ss.pruneAgent(time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := ss.queryAgentRange(0, time.Now().Unix()+3600, "", "", "", 60)
	if len(got) != 1 {
		t.Errorf("0-retention prune deleted rows: %d want 1", len(got))
	}
}

// TestConvertHelpers: finish↔stop maps (all branches), text extraction, backend
// path, and the SSE reader selector.
// TestFormatAgentsTable_Latency: the --by-agent table includes the new latency
// + failure columns.
func TestFormatAgentsTable_Latency(t *testing.T) {
	resp := agentResp{Bucket: 60, Buckets: []agentBucket{
		{Agent: "claude-code", Requests: 10, Input: 100, Output: 50, LatencySum: 2000, Failures: 1},
	}}
	out := formatAgentsTable(resp)
	for _, want := range []string{"claude-code", "lat", "fail", "10", "200", "1"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatAgentsTable missing %q:\n%s", want, out)
		}
	}
}
