package stats

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestRebuildTokenLedgerExcludesShadowRecords: the boot-time rebuild rewrites
// the live minute and agent buckets from the request-log index, and the index
// also holds shadow rows — a shadow leg is real upstream spend and is logged
// like any request (request id "shadow-…", shadow = 1), but the live buckets
// never counted it. The rebuild aggregates must skip them the way they skip
// attempt_only legs:
//
//   - inside the ±10% count guard a shadow row's usage was written into the
//     live bucket (agent_buckets has no guard at all);
//   - outside it the shadow rows inflated the record count, the guard read
//     the mismatch as a log gap and the bucket kept its wrong values;
//   - a post-cutoff bucket's requests repair took the shadow 2xx for live
//     requests.
func TestRebuildTokenLedgerExcludesShadowRecords(t *testing.T) {
	t.Run("shadow rows inside the count guard must not reach the buckets", func(t *testing.T) {
		store, idxPath := openLedgerRebuildFixture(t)
		minute := int64(1791520380) // 2026-10-09T12:33+08:00, pre-cutoff
		seedLedgerBucket(t, store, minute,
			Counters{Requests: 100, Input: 5000, Output: 900, CacheRead: 900, CacheCreation: 900, TokenRequests: 100},
			AgentCounters{Requests: 100, Input: 5000, Output: 900, CacheRead: 900, CacheCreation: 900})
		// 100 live rows (10/5/1/2) and 2 shadow rows (1000/500/7/3), all 2xx
		// with usage in the same minute: the 102 indexed 2xx sit inside the
		// ±10% guard around the 100 committed token_requests. Three failover
		// legs with usage-bearing rows stay out like the shadow rows.
		insertLedgerRecords(t, idxPath, "2026-10-09T12:33:10+08:00", 100,
			ledgerRecordRow{input: 10, output: 5, cacheRead: 1, cacheCreation: 2})
		insertLedgerRecords(t, idxPath, "2026-10-09T12:33:20+08:00", 2,
			ledgerRecordRow{input: 1000, output: 500, cacheRead: 7, cacheCreation: 3, shadow: true})
		insertLedgerRecords(t, idxPath, "2026-10-09T12:33:30+08:00", 3,
			ledgerRecordRow{input: 999, output: 99, cacheRead: 9, cacheCreation: 8, status: 502, attemptOnly: true})

		rebuilt, skipped, err := store.RebuildTokenLedger(idxPath)
		if err != nil || rebuilt != 2 || skipped != 0 {
			t.Fatalf("rebuild = (%d, %d, %v), want (2, 0, nil)", rebuilt, skipped, err)
		}
		assertMinuteLedger(t, store, minute, 100, ledgerTotals{input: 1000, output: 500, cacheRead: 100, cacheCreation: 200})
		assertAgentLedger(t, store, minute, ledgerTotals{input: 1000, output: 500, cacheRead: 100, cacheCreation: 200})
	})

	t.Run("shadow rows must not defeat the count guard", func(t *testing.T) {
		// Three live 200s + one 400 in the index, token_requests 3: one shadow
		// 2xx row pushes the aggregated 2xx count to 4, outside the guard.
		store, idxPath := seedLedgerFixture(t)
		insertLedgerRecords(t, idxPath, "2026-10-09T12:33:40+08:00", 1,
			ledgerRecordRow{input: 700, output: 70, cacheRead: 600, cacheCreation: 50, shadow: true})

		rebuilt, skipped, err := store.RebuildTokenLedger(idxPath)
		if err != nil || rebuilt != 2 || skipped != 0 {
			t.Fatalf("rebuild = (%d, %d, %v), want (2, 0, nil)", rebuilt, skipped, err)
		}
		assertMinuteLedger(t, store, 1791520380, 3, ledgerTotals{input: 300, output: 30, cacheRead: 198, cacheCreation: 15})
		assertAgentLedger(t, store, 1791520380, ledgerTotals{input: 300, output: 30, cacheRead: 198, cacheCreation: 15})
	})

	t.Run("post-cutoff shadow rows must not inflate the requests repair", func(t *testing.T) {
		store, idxPath := openLedgerRebuildFixture(t)
		minute := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix()
		seedLedgerBucket(t, store, minute,
			Counters{Requests: 100, Input: 1000, Output: 500, TokenRequests: 100},
			AgentCounters{Requests: 100, Input: 1000, Output: 500})
		insertLedgerRecords(t, idxPath, "2026-11-20T10:00:10Z", 100, ledgerRecordRow{input: 10, output: 5})
		insertLedgerRecords(t, idxPath, "2026-11-20T10:00:20Z", 10, ledgerRecordRow{input: 10, output: 5, shadow: true})
		// A live 400 counts as a record but not as a successful request.
		insertLedgerRecords(t, idxPath, "2026-11-20T10:00:30Z", 1, ledgerRecordRow{status: 400})

		// The committed requests already equal the live 2xx count: nothing to
		// rewrite in either dimension.
		rebuilt, skipped, err := store.RebuildTokenLedger(idxPath)
		if err != nil || rebuilt != 0 || skipped != 0 {
			t.Fatalf("rebuild = (%d, %d, %v), want (0, 0, nil)", rebuilt, skipped, err)
		}
		assertMinuteLedger(t, store, minute, 100, ledgerTotals{input: 1000, output: 500})
		assertAgentLedger(t, store, minute, ledgerTotals{input: 1000, output: 500})
	})
}

// ledgerRecordRow is one mini-index record: usage, status band and flags.
type ledgerRecordRow struct {
	input, output, cacheRead, cacheCreation int
	status                                  int
	shadow, attemptOnly                     bool
}

// ledgerTotals are the token columns one bucket must carry after the rebuild.
type ledgerTotals struct {
	input, output, cacheRead, cacheCreation uint64
}

// openLedgerRebuildFixture opens a stats store and an empty mini index in one
// temp dir and returns the store and the index path.
func openLedgerRebuildFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(Options{Path: filepath.Join(dir, "stats.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	idxPath := filepath.Join(dir, "index.db")
	idx, err := sql.Open("sqlite", idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Exec(miniIndexSchema); err != nil {
		_ = idx.Close()
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	return store, idxPath
}

// seedLedgerBucket commits one (deepseek, m, minute) minute bucket and its
// claude-code agent row with the given counters.
func seedLedgerBucket(t *testing.T, store *Store, minute int64, c Counters, a AgentCounters) {
	t.Helper()
	key := Key{Provider: "deepseek", Model: "m"}
	if err := store.Flush(minute, map[Key]Counters{key: c}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(minute, map[AgentKey]AgentCounters{
		{Agent: "claude-code", Provider: key.Provider, Model: key.Model}: a,
	}); err != nil {
		t.Fatal(err)
	}
}

// insertLedgerRecords adds n identical mini-index records (provider deepseek,
// upstream model m, agent claude-code, status 200 unless set) stamped at ts.
func insertLedgerRecords(t *testing.T, idxPath, ts string, n int, row ledgerRecordRow) {
	t.Helper()
	status := row.status
	if status == 0 {
		status = 200
	}
	shadow, attemptOnly := 0, 0
	if row.shadow {
		shadow = 1
	}
	if row.attemptOnly {
		attemptOnly = 1
	}
	idx, err := sql.Open("sqlite", idxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for i := 0; i < n; i++ {
		if _, err := idx.Exec(`INSERT INTO records
			(ts, provider, upstream_model, agent, status, input, output, cache_read, cache_creation, shadow, attempt_only, kind)
			VALUES (?, 'deepseek', 'm', 'claude-code', ?, ?, ?, ?, ?, ?, ?, '')`,
			ts, status, row.input, row.output, row.cacheRead, row.cacheCreation, shadow, attemptOnly); err != nil {
			t.Fatal(err)
		}
	}
}

// assertMinuteLedger requires the store to hold exactly one minute bucket for
// (deepseek, m, minute) with the given requests and token columns.
func assertMinuteLedger(t *testing.T, store *Store, minute int64, requests uint64, want ledgerTotals) {
	t.Helper()
	rows, err := store.QueryRange(minute, minute, "deepseek", "m", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("minute rows = %+v, %v; want exactly one for minute %d", rows, err, minute)
	}
	got := rows[0]
	if got.Minute != minute || got.Requests != requests ||
		got.Input != want.input || got.Output != want.output ||
		got.CacheRead != want.cacheRead || got.CacheCreation != want.cacheCreation {
		t.Errorf("minute bucket = {requests %d input %d output %d cache_read %d cache_creation %d}, want {%d %d %d %d %d}",
			got.Requests, got.Input, got.Output, got.CacheRead, got.CacheCreation,
			requests, want.input, want.output, want.cacheRead, want.cacheCreation)
	}
}

// assertAgentLedger requires the store to hold exactly one agent row for
// (claude-code, deepseek, m, minute) with the given token columns.
func assertAgentLedger(t *testing.T, store *Store, minute int64, want ledgerTotals) {
	t.Helper()
	rows, err := store.QueryAgents(minute, minute, "claude-code", "deepseek", "m", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("agent rows = %+v, %v; want exactly one for minute %d", rows, err, minute)
	}
	got := rows[0]
	if got.Minute != minute ||
		got.Input != want.input || got.Output != want.output ||
		got.CacheRead != want.cacheRead || got.CacheCreation != want.cacheCreation {
		t.Errorf("agent bucket = {input %d output %d cache_read %d cache_creation %d}, want {%d %d %d %d}",
			got.Input, got.Output, got.CacheRead, got.CacheCreation,
			want.input, want.output, want.cacheRead, want.cacheCreation)
	}
}
