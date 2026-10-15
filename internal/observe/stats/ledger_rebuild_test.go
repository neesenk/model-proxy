package stats

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// seedLedgerFixture opens a stats store with one pre-cutoff minute bucket
// carrying the historical double-count ledger (input inclusive of cache_read,
// inflated output) plus a matching request-log index with the correct ledger,
// and returns both handles.
func seedLedgerFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(Options{Path: filepath.Join(dir, "stats.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	minute := int64(1791520380) // epoch minute of 2026-10-09T12:33:0x+08:00 (sqlite strftime conversion), pre-cutoff
	if err := store.Flush(minute, map[Key]Counters{
		{Provider: "deepseek", Model: "m"}: {
			Requests: 3, Input: 500, Output: 90, CacheRead: 200, TokenRequests: 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(minute, map[AgentKey]AgentCounters{
		{Agent: "claude-code", Provider: "deepseek", Model: "m"}: {
			Requests: 3, Input: 500, Output: 90, CacheRead: 200,
		},
	}); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(dir, "index.db")
	idx, err := sql.Open("sqlite", idxPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	schema := `CREATE TABLE records (
		ts TEXT NOT NULL, provider TEXT, upstream_model TEXT, agent TEXT,
		input INTEGER DEFAULT 0, output INTEGER DEFAULT 0,
		cache_read INTEGER DEFAULT 0, cache_creation INTEGER DEFAULT 0,
		attempt_only INTEGER DEFAULT 0)`
	if _, err := idx.Exec(schema); err != nil {
		t.Fatal(err)
	}
	// The correct ledger: input EXCLUDES the cache hit (300), output without
	// the repetition inflation (30). Three records in the same minute.
	for i := 0; i < 3; i++ {
		if _, err := idx.Exec(`INSERT INTO records
			(ts, provider, upstream_model, agent, input, output, cache_read, cache_creation, attempt_only)
			VALUES (?, 'deepseek', 'm', 'claude-code', 100, 10, 66, 5, 0)`,
			"2026-10-09T12:33:"+itoa2(i)+"+08:00"); err != nil {
			t.Fatal(err)
		}
	}
	return store, idxPath
}

func itoa2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func TestRebuildTokenLedgerRewritesPreCutoffBuckets(t *testing.T) {
	store, idxPath := seedLedgerFixture(t)

	rebuilt, skipped, err := store.RebuildTokenLedger(idxPath)
	if err != nil || rebuilt != 2 || skipped != 0 {
		t.Fatalf("rebuild = (%d, %d, %v), want (2, 0, nil)", rebuilt, skipped, err)
	}
	rows, err := store.QueryRange(0, 1791600000, "deepseek", "m", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	got := rows[0]
	if got.Input != 300 || got.Output != 30 || got.CacheRead != 198 || got.CacheCreation != 15 {
		t.Fatalf("rebuilt bucket = input=%d output=%d cr=%d cc=%d, want 300/30/198/15",
			got.Input, got.Output, got.CacheRead, got.CacheCreation)
	}
	agents, err := store.QueryAgents(0, 1791600000, "claude-code", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var aIn, aCR uint64
	for _, a := range agents {
		aIn += a.Input
		aCR += a.CacheRead
	}
	if aIn != 300 || aCR != 198 {
		t.Fatalf("rebuilt agent rows = in=%d cr=%d, want 300/198", aIn, aCR)
	}

	// Idempotent: a second rebuild finds the buckets already equal.
	rebuilt, skipped, err = store.RebuildTokenLedger(idxPath)
	if err != nil || rebuilt != 2 || skipped != 0 {
		t.Fatalf("second rebuild = (%d, %d, %v), want (2, 0, nil)", rebuilt, skipped, err)
	}
}

// TestRebuildTokenLedgerSkipsCountMismatch: a bucket whose request-log record
// count diverges from its committed token_requests (a log gap) is left with
// its committed values — the defensive no-destroy rule.
func TestRebuildTokenLedgerSkipsCountMismatch(t *testing.T) {
	store, idxPath := seedLedgerFixture(t)
	// Halve the committed token_requests so the aggregate count (3) no longer
	// matches within 10%.
	if _, err := store.db.Exec(`UPDATE minute_buckets SET token_requests = 1`); err != nil {
		t.Fatal(err)
	}
	rebuilt, skipped, err := store.RebuildTokenLedger(idxPath)
	if err != nil || rebuilt != 1 || skipped != 1 {
		t.Fatalf("rebuild = (%d, %d, %v), want minute-skips: (1, 1, nil)", rebuilt, skipped, err)
	}
	rows, err := store.QueryRange(0, 1791600000, "deepseek", "m", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	// The mismatched minute bucket keeps its committed (wrong) values.
	if rows[0].Input != 500 || rows[0].CacheRead != 200 {
		t.Fatalf("mismatched bucket must keep committed values, got %+v", rows[0])
	}
}
