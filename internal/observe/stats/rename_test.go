package stats

import (
	"path/filepath"
	"testing"
	"time"

	obscounters "model-proxy/internal/observe/counters"
)

// TestRenameProviderKeyMergesBuckets pins the rename's merge semantics: rows
// under the old key merge ADDITIVIVELY into the new key's rows (same bucket
// identity — counter sums, last_request_at keeps the max) in BOTH tables, the
// old key owns nothing afterwards, and a second rename with no source rows is
// a no-op.
func TestRenameProviderKeyMergesBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const minute = int64(1791900000)
	agents := obscounters.NewAgentCounter()
	agentFlush := NewFlusher(store, nil, nil, agents, nil, nil)
	// Bare-key era: provider "p", agent "pi".
	agents.IncRequests("pi", "p", "m")
	agents.AddTokens("pi", "p", "m", obscounters.TokenUsage{Input: 10, Output: 4})
	// New-key era: same agent already has traffic under "p#acc".
	agents.IncRequests("pi", "p#acc", "m")
	agents.AddTokens("pi", "p#acc", "m", obscounters.TokenUsage{Input: 5})
	if !agentFlush.Flush(time.Unix(minute, 0)) {
		t.Fatal("seed flush reported no write")
	}

	if err := store.Flush(minute, map[Key]Counters{
		{Provider: "p", Model: "m"}:     {Requests: 82242, Input: 100, LastRequestAt: 1791900060, TokenRequests: 82242},
		{Provider: "p#acc", Model: "m"}: {Requests: 3, Input: 7, LastRequestAt: 1791900070, TokenRequests: 3},
	}); err != nil {
		t.Fatal(err)
	}

	moved, err := store.RenameProviderKey("p", "p#acc")
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d rows, want 2 (one minute bucket + one agent bucket)", moved)
	}

	rows, err := store.QueryRange(minute, minute, "p#acc", "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 82245 || rows[0].Input != 107 ||
		rows[0].TokenRequests != 82245 || rows[0].LastRequestAt != 1791900070 {
		t.Fatalf("merged minute bucket = %+v, want requests=82245 input=107 tokens=82245 last=1791900070", rows)
	}
	if bare, err := store.QueryRange(minute, minute, "p", "m", 0); err != nil || len(bare) != 0 {
		t.Fatalf("bare-key rows survived the rename: %+v (err=%v)", bare, err)
	}

	// The agent flusher writes the COMPLETED minute (now/60*60 - 60), so the
	// agent rows live one bucket below the store.Flush minute.
	agentRows, err := store.QueryAgents(minute-60, minute, "pi", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var totalReq uint64
	var totalIn uint64
	for _, r := range agentRows {
		totalReq += r.Requests
		totalIn += r.Input
	}
	if totalReq != 2 || totalIn != 15 {
		t.Fatalf("agent rows after rename: req=%d in=%d, want 2 requests / 15 input merged under p#acc", totalReq, totalIn)
	}
	for _, r := range agentRows {
		if r.Provider != "p#acc" {
			t.Fatalf("agent row still keyed %q after rename: %+v", r.Provider, r)
		}
	}

	// Idempotent: no source rows → no-op, zero moved.
	moved, err = store.RenameProviderKey("p", "p#acc")
	if err != nil || moved != 0 {
		t.Fatalf("second rename = (%d, %v), want (0, nil)", moved, err)
	}
}
