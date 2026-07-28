package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"model-proxy/provider"
)

// newTestStatsStore opens a fresh statsStore in a temp dir with no retention.
func newTestStatsStore(t *testing.T) *statsStore {
	t.Helper()
	ss, err := openStatsStore(filepath.Join(t.TempDir(), "stats.db"), 0)
	if err != nil {
		t.Fatalf("openStatsStore: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	return ss
}

func TestDiffCountersClampsAndOmits(t *testing.T) {
	prev := map[pmKey]statsCounters{
		{Provider: "a", Model: "x"}: {Requests: 5, Input: 10, LastRequestAt: 100},
	}
	cur := map[pmKey]statsCounters{
		{Provider: "a", Model: "x"}: {Requests: 8, Input: 10, LastRequestAt: 200}, // reqs +3, input unchanged, LRA advances
		{Provider: "b", Model: "y"}: {Requests: 1, LastRequestAt: 300},            // new key
	}
	d := diffCounters(cur, prev)
	if len(d) != 2 {
		t.Fatalf("diff = %d keys, want 2 (a/x has reqs delta, b/y is new)", len(d))
	}
	ax := d[pmKey{Provider: "a", Model: "x"}]
	if ax.Requests != 3 {
		t.Errorf("a/x reqs delta = %d, want 3", ax.Requests)
	}
	if ax.Input != 0 {
		t.Errorf("a/x input delta = %d, want 0 (unchanged)", ax.Input)
	}
	if ax.LastRequestAt != 200 {
		t.Errorf("a/x last_request_at = %d, want 200 (carried as cumulative max)", ax.LastRequestAt)
	}
	if d[pmKey{Provider: "b", Model: "y"}].Requests != 1 {
		t.Errorf("b/y new-key delta = %+v, want reqs=1", d[pmKey{Provider: "b", Model: "y"}])
	}

	// A reset race (cur < prev) must clamp to 0, never negative.
	neg := diffCounters(
		map[pmKey]statsCounters{{Provider: "a", Model: "x"}: {Requests: 2}},
		map[pmKey]statsCounters{{Provider: "a", Model: "x"}: {Requests: 5}},
	)
	if len(neg) != 0 {
		t.Errorf("negative-delta key should be omitted, got %+v", neg)
	}
}

// TestStatsFlushTwoMinutes verifies per-minute bucketing: two flushes produce two
// distinct buckets whose deltas sum to the cumulative total. Exercises the
// flusher diff + lastBucket monotonic guard.
func TestStatsFlushTwoMinutes(t *testing.T) {
	ss := newTestStatsStore(t)
	m := newMetricsStore()
	tc := newTokenCounter()
	f := newStatsFlusher(ss, m, tc, newAgentCounter(), map[pmKey]statsCounters{})

	// Minute 1: 1 request, 1 failover.
	m.inc("z", "m", evRequests)
	m.inc("z", "m", evFailovers)
	tc.commit(tokenKey{Provider: "z", Model: "m"}, tokenUsage{Input: 10, Output: 5, Requests: 1})
	if !f.flush(time.Now()) {
		t.Fatal("first flush should write deltas")
	}

	// Minute 2: 2 more requests (total 3), more tokens.
	m.inc("z", "m", evRequests)
	m.inc("z", "m", evRequests)
	tc.commit(tokenKey{Provider: "z", Model: "m"}, tokenUsage{Input: 20, Output: 5, Requests: 1})
	if !f.flush(time.Now()) {
		t.Fatal("second flush should write deltas")
	}

	buckets, err := ss.queryRange(0, time.Now().Unix()+3600, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 minute buckets, got %d", len(buckets))
	}
	// Buckets are ordered by minute; the two flushes landed in distinct minutes
	// (the monotonic guard forces the second to M+60 if same minute).
	var sumReqs, sumInput uint64
	for _, b := range buckets {
		if b.Provider != "z" || b.Model != "m" {
			t.Errorf("bucket key = %s/%s, want z/m", b.Provider, b.Model)
		}
		sumReqs += b.Requests
		sumInput += b.Input
	}
	if sumReqs != 3 {
		t.Errorf("sum of bucket requests = %d, want 3", sumReqs)
	}
	if sumInput != 30 {
		t.Errorf("sum of bucket input = %d, want 30", sumInput)
	}
	// First bucket has the minute-1 deltas (1 req, 1 failover, 10 input).
	if buckets[0].Requests != 1 || buckets[0].Failovers != 1 || buckets[0].Input != 10 {
		t.Errorf("first bucket = %+v, want reqs=1 failovers=1 input=10", buckets[0])
	}
}

// TestStatsRestoreOnBoot verifies loadCumulative returns exact all-time totals
// after a reopen (the boot restore path that seeds in-memory counters).
func TestStatsRestoreOnBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	ss, err := openStatsStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := newMetricsStore()
	tc := newTokenCounter()
	f := newStatsFlusher(ss, m, tc, newAgentCounter(), map[pmKey]statsCounters{})
	m.inc("a", "x", evRequests)
	m.inc("a", "x", evRequests)
	m.inc("a", "x", evFailures)
	tc.commit(tokenKey{Provider: "a", Model: "x"}, tokenUsage{Input: 100, Output: 50, CacheRead: 7})
	f.flush(time.Now())
	if err := ss.Close(); err != nil {
		t.Fatal(err)
	}

	ss2, err := openStatsStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ss2.Close()
	base, err := ss2.loadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	got := base[pmKey{Provider: "a", Model: "x"}]
	// commit() bumps Requests by 1 per commit (one commit here) -> TokenRequests=1.
	if got.Requests != 2 || got.Failures != 1 || got.Input != 100 || got.Output != 50 || got.CacheRead != 7 || got.TokenRequests != 1 {
		t.Errorf("restored cumulative = %+v, want reqs=2 fail=1 in=100 out=50 cr=7 treqs=1", got)
	}
}

// TestStatsPrune verifies buckets older than retention are deleted.
func TestStatsPrune(t *testing.T) {
	ss, err := openStatsStore(filepath.Join(t.TempDir(), "stats.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	// Insert a bucket far in the past and one recent.
	old := time.Now().Add(-2*time.Hour).Unix() / 60 * 60
	recent := time.Now().Unix() / 60 * 60
	if err := ss.flushDeltas(old, map[pmKey]statsCounters{
		{Provider: "old", Model: "m"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ss.flushDeltas(recent, map[pmKey]statsCounters{
		{Provider: "new", Model: "m"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ss.prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := ss.queryRange(0, time.Now().Unix()+3600, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != "new" {
		t.Errorf("after prune = %+v, want only new/m", got)
	}
}

// TestStatsResetAll verifies resetAll wipes all buckets.
func TestStatsResetAll(t *testing.T) {
	ss := newTestStatsStore(t)
	if err := ss.flushDeltas(time.Now().Unix()/60*60, map[pmKey]statsCounters{
		{Provider: "z", Model: "m"}: {Requests: 5},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := ss.queryRange(0, time.Now().Unix()+3600, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Requests != 5 {
		t.Fatalf("reset precondition = %+v, want one row with five requests", before)
	}
	if err := ss.resetAll(); err != nil {
		t.Fatal(err)
	}
	got, err := ss.queryRange(0, time.Now().Unix()+3600, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after resetAll = %d rows, want 0", len(got))
	}
}

// TestStatsQueryRangeFilters verifies provider/model filters.
func TestStatsQueryRangeFilters(t *testing.T) {
	ss := newTestStatsStore(t)
	minute := time.Now().Unix() / 60 * 60
	if err := ss.flushDeltas(minute, map[pmKey]statsCounters{
		{Provider: "a", Model: "x"}: {Requests: 1},
		{Provider: "a", Model: "y"}: {Requests: 2},
		{Provider: "b", Model: "x"}: {Requests: 3},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ss.queryRange(0, minute+60, "a", "x", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != "a" || got[0].Model != "x" || got[0].Requests != 1 {
		t.Errorf("filtered query = %+v, want a/x reqs=1 only", got)
	}
}

// TestStatsMigrationFromJSON verifies a legacy token_usage.json is imported once
// into a fresh DB (and not re-imported when the DB already has data).
func TestStatsMigrationFromJSON(t *testing.T) {
	jsonPath := filepath.Join(t.TempDir(), "token_usage.json")
	// The legacy format keys are "provider\x00model".
	flat := map[string]tokenUsage{
		"zhipu\x00glm-5": {Input: 42, Output: 8, CacheRead: 3, Requests: 2},
	}
	data, _ := json.Marshal(flat)
	if err := os.WriteFile(jsonPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(jsonPath, time.Now(), time.Now()) // ensure mtime is recent

	ss := newTestStatsStore(t)
	n, err := ss.migrateFromTokensJSON(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migrated %d entries, want 1", n)
	}
	base, err := ss.loadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	got := base[pmKey{Provider: "zhipu", Model: "glm-5"}]
	if got.Input != 42 || got.Output != 8 || got.CacheRead != 3 || got.TokenRequests != 2 {
		t.Errorf("migrated cumulative = %+v, want in=42 out=8 cr=3 treqs=2", got)
	}

	// Second call must NOT re-import (DB is no longer empty).
	n2, err := ss.migrateFromTokensJSON(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("re-migration imported %d, want 0 (DB not empty)", n2)
	}
}

// TestStatsFlusherResetPrev verifies that after resetStats, a subsequent flush
// writes zero deltas (no negative re-add) and the baseline is the zero snapshot.
func TestStatsFlusherResetPrev(t *testing.T) {
	ss := newTestStatsStore(t)
	m := newMetricsStore()
	tc := newTokenCounter()
	f := newStatsFlusher(ss, m, tc, newAgentCounter(), map[pmKey]statsCounters{})
	m.inc("z", "m", evRequests)
	f.flush(time.Now())

	// Simulate resetStats: zero in-memory + DB + flusher baseline (the prev-reset
	// portion of flusher.resetAll, inlined here since the test has no *Proxy).
	m.reset()
	tc.reset()
	ss.resetAll()
	f.mu.Lock()
	f.prev = f.collect()
	f.agentPrev = nil
	f.lastBucket = 0
	f.mu.Unlock()

	// A flush with no new activity writes nothing.
	if f.flush(time.Now()) {
		t.Error("flush after reset with no activity should write no deltas")
	}
	got, err := ss.queryRange(0, time.Now().Unix()+3600, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after reset+flush, DB has %d rows, want 0", len(got))
	}
}

// TestAPIStatsHandler verifies /api/stats returns persisted buckets as JSON.
func TestAPIStatsHandler(t *testing.T) {
	p := &Proxy{
		metrics: newMetricsStore(),
		tokens:  newTokenCounter(),
		stats:   newTestStatsStore(t),
	}
	minute := time.Now().Unix() / 60 * 60
	if err := p.stats.flushDeltas(minute, map[pmKey]statsCounters{
		{Provider: "zhipu", Model: "glm-5"}: {Requests: 7, Input: 100},
	}); err != nil {
		t.Fatal(err)
	}
	w := newWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"buckets"`, `"zhipu"`, `"glm-5"`, `"requests":7`, `"input":100`, `"bucket":60`} {
		if !strings.Contains(body, want) {
			t.Errorf("stats body missing %s: %s", want, body)
		}
	}

	// ?bucket=10m aggregates: same single row, but bucket reports 600.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/stats?bucket=10m", nil))
	if rec2.Code != 200 {
		t.Fatalf("bucket status=%d want 200", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `"bucket":600`) {
		t.Errorf("bucket=10m should report bucket=600: %s", rec2.Body.String())
	}
}

// TestRenderStatsCLI verifies the `stats` CLI renders the daemon's /api/stats
// response into a terminal table (and --json passes raw JSON through).
func TestRenderStatsCLI(t *testing.T) {
	minute := time.Now().Unix() / 60 * 60
	resp := statsResp{From: minute - 60, To: minute, Bucket: 60, Buckets: []statsBucket{
		{Provider: "zhipu", Model: "glm-5", Minute: minute, Requests: 7, Input: 100, Output: 20},
	}}
	raw, _ := json.Marshal(resp)

	var gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stats" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		io.WriteString(w, string(raw))
	}))
	defer up.Close()

	// renderStats takes "host:port"; derive from the httptest server URL.
	listen := strings.TrimPrefix(up.URL, "http://")
	out, err := renderStats(listen, statsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zhipu", "glm-5", "7", "1m", "lat(ms)", "ttft(ms)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered table missing %q:\n%s", want, out)
		}
	}
	// No --bucket flag -> query string omits bucket (server defaults to 1m).
	if strings.Contains(gotQuery, "bucket=") {
		t.Errorf("default query should omit bucket, got %q", gotQuery)
	}

	// --bucket 10m is forwarded to the daemon's query string.
	if _, err := renderStats(listen, statsOpts{Bucket: "10m"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "bucket=10m") {
		t.Errorf("--bucket 10m not forwarded, query=%q", gotQuery)
	}

	// --json passes the raw body through.
	outJSON, err := renderStats(listen, statsOpts{JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outJSON, `"buckets"`) {
		t.Errorf("json output missing buckets: %s", outJSON)
	}
}

func TestUntilNextMinuteBounded(t *testing.T) {
	d := untilNextMinute(time.Now())
	if d <= 0 || d > 61*time.Second {
		t.Errorf("untilNextMinute = %v, want (0, 61s]", d)
	}
}

// TestSeedRestore verifies the boot-restore seed methods set the in-memory
// counters to a baseline value exactly (the path initStats uses on startup).
func TestSeedRestore(t *testing.T) {
	m := newMetricsStore()
	tc := newTokenCounter()
	k := pmKey{Provider: "z", Model: "m"}
	m.seed(k, providerMetricsSnapshot{Requests: 100, Failovers: 3, Failures: 2, RateLimited429: 1, LastRequestAt: 42})
	tc.seed(k, tokenUsage{Input: 500, Output: 50, CacheCreation: 9, CacheRead: 4, Requests: 7})

	msnap := m.snapshot()[k]
	if msnap.Requests != 100 || msnap.Failovers != 3 || msnap.Failures != 2 || msnap.RateLimited429 != 1 || msnap.LastRequestAt != 42 {
		t.Errorf("metrics seed = %+v, want exact baseline", msnap)
	}
	tsnap := tc.snapshot()[k]
	if tsnap.Input != 500 || tsnap.Output != 50 || tsnap.CacheCreation != 9 || tsnap.CacheRead != 4 || tsnap.Requests != 7 {
		t.Errorf("tokens seed = %+v, want exact baseline", tsnap)
	}
}

// TestLegacyTokensPath sanity-checks the migration source path convention.
func TestLegacyTokensPath(t *testing.T) {
	p := legacyTokensPath()
	if !strings.HasSuffix(p, ".model-proxy/token_usage.json") {
		t.Errorf("legacyTokensPath = %q, want .../.model-proxy/token_usage.json", p)
	}
}

// TestStatsFlushEmptyIsNoop verifies a flush with no activity writes nothing and
// reports false (the idle-proxy path).
func TestStatsFlushEmptyIsNoop(t *testing.T) {
	ss := newTestStatsStore(t)
	f := newStatsFlusher(ss, newMetricsStore(), newTokenCounter(), newAgentCounter(), map[pmKey]statsCounters{})
	if f.flush(time.Now()) {
		t.Error("flush with no deltas should report false")
	}
	got, _ := ss.queryRange(0, time.Now().Unix()+3600, "", "", 60)
	if len(got) != 0 {
		t.Errorf("empty flush wrote %d rows, want 0", len(got))
	}
}

// hasHeapScan reports whether the EXPLAIN QUERY PLAN output contains a heap scan
// (a "SCAN <table>" with no "USING INDEX"), which reads the table rowid-by-rowid
// with no index. An index-ordered "SCAN ... USING INDEX" is NOT a heap scan.
func hasHeapScan(plan string) bool {
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "SCAN minute_buckets") && !strings.Contains(line, "USING") {
			return true
		}
	}
	return false
}

// --- performance / query-plan tests ---

// TestStatsQueryPlan asserts no query plan degrades to a full table scan, which
// would be catastrophic on a table that grows to tens of thousands of rows
// (30d x N keys). SQLite picks the covering autoindex on the primary key
// (provider, model, minute) for provider/model predicates and idx_minute for
// range-only queries; both are index SEARCHes, never a SCAN. This guards
// against a future schema change silently dropping an index.
func TestStatsQueryPlan(t *testing.T) {
	ss := newTestStatsStore(t)
	now := time.Now().Unix() / 60 * 60
	if err := ss.flushDeltas(now, map[pmKey]statsCounters{
		{Provider: "z", Model: "m"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		q    string
		args []any
	}{
		{"range only", `SELECT minute FROM minute_buckets WHERE minute >= ? AND minute <= ?`, []any{now - 60, now + 60}},
		{"range + provider", `SELECT minute FROM minute_buckets WHERE minute >= ? AND minute <= ? AND provider = ?`, []any{now - 60, now + 60, "z"}},
		{"provider + model + range", `SELECT minute FROM minute_buckets WHERE provider = ? AND model = ? AND minute >= ?`, []any{"z", "m", now - 60}},
		{"loadCumulative group-by", `SELECT provider, model, SUM(requests), MAX(last_request_at) FROM minute_buckets GROUP BY provider, model`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, err := ss.db.Query(`EXPLAIN QUERY PLAN `+c.q, c.args...)
			if err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			var plan strings.Builder
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				fmt.Fprintf(&plan, "%d|%d|%s\n", id, parent, detail)
			}
			rows.Close()
			t.Logf("plan:\n%s", plan.String())
			// The only plan we must never see is a heap scan (SCAN without USING
			// INDEX), which reads the raw table with no index help. An index-ordered
			// scan ("SCAN ... USING INDEX") is fine and expected for a full-table
			// GROUP BY aggregate like loadCumulative (it must read every row, and
			// walking the PK in order saves a sort). queryRange-style range/filter
			// queries come back as SEARCH USING COVERING INDEX.
			if hasHeapScan(plan.String()) {
				t.Errorf("plan uses a heap SCAN (want an index SEARCH/scan):\n%s", plan.String())
			}
		})
	}
}

// BenchmarkStatsFlush measures the per-minute flush latency at realistic scale.
// 10 providers x 20 models x 1 minute = 200 rows/bucket upsert per flush, a
// generous upper bound for a single minute. The flush is the only write path
// and runs once/minute, so even 10ms here is negligible.
func BenchmarkStatsFlush(b *testing.B) {
	ss, err := openStatsStore(filepath.Join(b.TempDir(), "stats.db"), 0)
	if err != nil {
		b.Fatal(err)
	}
	defer ss.Close()
	// Build 200 deltas (10 providers x 20 models), each with non-zero counters.
	deltas := make(map[pmKey]statsCounters, 200)
	for p := 0; p < 10; p++ {
		for m := 0; m < 20; m++ {
			k := pmKey{Provider: fmt.Sprintf("prov%d", p), Model: fmt.Sprintf("model%d", m)}
			deltas[k] = statsCounters{Requests: 3, Failovers: 1, Input: 1200, Output: 300}
		}
	}
	minute := time.Now().Unix() / 60 * 60
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Each iteration flushes into a distinct minute so the upsert is an
		// INSERT (not a conflict-update), exercising the cold write path.
		minute += 60
		if err := ss.flushDeltas(minute, deltas); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStatsQueryRange measures the /api/stats query over a 30-day window.
// Seeds 30d x 200 keys = 1440 minutes/day * 30 = 43200 rows * ... actually 200
// keys x (30*1440) minutes is 8.6M rows - too heavy for a benchmark. Use a
// representative slice: 200 keys x 1000 minutes (200k rows), query a 1h (60-min)
// slice. Validates the index keeps range queries sub-millisecond.
func BenchmarkStatsQueryRange(b *testing.B) {
	ss, err := openStatsStore(filepath.Join(b.TempDir(), "stats.db"), 0)
	if err != nil {
		b.Fatal(err)
	}
	defer ss.Close()
	// Seed 200 keys x 1000 minutes of data in bulk (one tx per 50 minutes to
	// keep the seed fast but not memory-heavy).
	const keys, mins = 200, 1000
	base := time.Now().Unix()/60*60 - int64(mins)*60
	for mi := 0; mi < mins; mi++ {
		minute := base + int64(mi)*60
		deltas := make(map[pmKey]statsCounters, keys)
		for p := 0; p < 10; p++ {
			for m := 0; m < 20; m++ {
				k := pmKey{Provider: fmt.Sprintf("prov%d", p), Model: fmt.Sprintf("model%d", m)}
				deltas[k] = statsCounters{Requests: 2, Input: 800}
			}
		}
		if err := ss.flushDeltas(minute, deltas); err != nil {
			b.Fatal(err)
		}
	}
	// Query a 60-minute slice out of the middle.
	from := base + int64(mins/2)*60
	to := from + 60*60
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ss.queryRange(from, to, "", "", 60); err != nil {
			b.Fatal(err)
		}
	}
}

// TestStatsQueryRangeBucket verifies the display-granularity aggregation: raw
// 1-minute rows are SUM'd into wider display buckets, last_request_at via MAX,
// and the bucket minute is the window start. Storage stays 1-minute (verified
// by re-querying at bucket=60). Exact-value assertions per testing-contract.
func TestStatsQueryRangeBucket(t *testing.T) {
	ss := newTestStatsStore(t)
	// Align base to a 10-minute boundary so base/base+60/base+120 fall in one
	// bucket and base+600 in the next (the aggregation key is minute/600*600).
	base := time.Now().Unix() / 600 * 600
	// Three 1-minute rows for (z, m): requests 1/2/3, input 10/20/30, all in the
	// same 10-minute window (base, base+60, base+120). Plus a 4th in the next
	// 10-minute window (base+600) to prove the boundary splits correctly.
	rows := []struct {
		minute int64
		reqs   uint64
		input  uint64
		lra    int64
	}{
		{base, 1, 10, 1000},
		{base + 60, 2, 20, 2000},
		{base + 120, 3, 30, 3000},
		{base + 600, 7, 70, 7000},
	}
	for _, r := range rows {
		if err := ss.flushDeltas(r.minute, map[pmKey]statsCounters{
			{Provider: "z", Model: "m"}: {Requests: r.reqs, Input: r.input, LastRequestAt: r.lra},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// bucket=600 (10m): first 3 rows collapse into one bucket at `base`; the
	// 4th lands in its own bucket at base+600 (floor 600).
	got, err := ss.queryRange(base-60, base+700, "", "", 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("10m bucket = %d rows, want 2 (one per 10m window)", len(got))
	}
	b0 := got[0]
	if b0.Minute != base {
		t.Errorf("first bucket minute = %d, want %d (window start)", b0.Minute, base)
	}
	if b0.Requests != 6 {
		t.Errorf("first bucket requests = %d, want 6 (1+2+3)", b0.Requests)
	}
	if b0.Input != 60 {
		t.Errorf("first bucket input = %d, want 60 (10+20+30)", b0.Input)
	}
	if b0.LastRequestAt != 3000 {
		t.Errorf("first bucket last_request_at = %d, want 3000 (MAX)", b0.LastRequestAt)
	}
	b1 := got[1]
	if b1.Minute != base+600 {
		t.Errorf("second bucket minute = %d, want %d", b1.Minute, base+600)
	}
	if b1.Requests != 7 || b1.Input != 70 {
		t.Errorf("second bucket = %+v, want reqs=7 input=70", b1)
	}

	// Storage is still 1-minute: bucket=60 returns all 4 raw rows.
	raw, err := ss.queryRange(base-60, base+700, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 4 {
		t.Errorf("1m query = %d rows, want 4 (storage lossless)", len(raw))
	}
}

// TestNormalizeBucket covers the granularity-spec parser: durations, bare
// seconds, defaults, clamping, and round-up-to-60-multiple.
func TestNormalizeBucket(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 60},       // default -> raw 1m
		{"0", 60},      // explicit zero -> raw 1m
		{"1m", 60},     // 1 minute
		{"10m", 600},   // 10 minutes
		{"1h", 3600},   // 1 hour
		{"24h", 86400}, // 1 day (as 24h; "d" is not a Go duration unit)
		{"30", 60},     // 30s < 60 -> clamp to 60
		{"90", 120},    // 90s not a 60-multiple -> round up to 120
		{"120", 120},   // exact multiple
		{"garbage", 60},
	}
	for _, c := range cases {
		if got := normalizeBucket(c.in); got != c.want {
			t.Errorf("normalizeBucket(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestBucketLabel covers the CLI column-header rendering.
func TestBucketLabel(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "1m"}, {60, "1m"}, {600, "10m"}, {3600, "1h"}, {86400, "24h"},
	}
	for _, c := range cases {
		if got := bucketLabel(c.secs); got != c.want {
			t.Errorf("bucketLabel(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestParseStatsFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want statsOpts
	}{
		{"empty", nil, statsOpts{}},
		{"space-separated", []string{"--from", "2026-01-01", "--to", "2026-02-01",
			"--provider", "zhipu", "--model", "glm-5.2", "--bucket", "5m", "--json"},
			statsOpts{From: "2026-01-01", To: "2026-02-01", Provider: "zhipu",
				Model: "glm-5.2", Bucket: "5m", JSON: true}},
		{"equals form", []string{"--from=2026-01-01", "--to=2026-02-01",
			"--provider=zhipu", "--model=glm-5.2", "--bucket=5m"},
			statsOpts{From: "2026-01-01", To: "2026-02-01", Provider: "zhipu",
				Model: "glm-5.2", Bucket: "5m"}},
		{"value at end without arg", []string{"--from"}, statsOpts{}},
		{"unknown flag ignored", []string{"--bogus", "x"}, statsOpts{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStatsFlags(tc.args)
			if got != tc.want {
				t.Errorf("parseStatsFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestFormatStatsTable(t *testing.T) {
	// Empty buckets -> "no stats" line.
	out := formatStatsTable(statsResp{From: 1700000000, To: 1700003600, Bucket: 60})
	if !strings.Contains(out, "no stats") || !strings.Contains(out, "bucket 1m") {
		t.Errorf("formatStatsTable empty missing 'no stats':\n%s", out)
	}
	// With buckets -> header + rows.
	resp := statsResp{From: 1700000000, To: 1700003600, Bucket: 3600, Buckets: []statsBucket{
		{Provider: "zhipu", Model: "glm-5.2", Minute: 1700000000, Requests: 100, Failovers: 2, Failures: 1, Input: 5000, Output: 3000},
	}}
	out = formatStatsTable(resp)
	if !strings.Contains(out, "provider") || !strings.Contains(out, "zhipu") || !strings.Contains(out, "glm-5.2") {
		t.Errorf("formatStatsTable rows missing marker:\n%s", out)
	}
}

func TestClearAccount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "acct.json")
	os.WriteFile(p, []byte("{}"), 0o600)
	if err := provider.ClearAqpAccount(p); err != nil {
		t.Fatalf("clearAccount existing: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("clearAccount did not remove the file")
	}
	// Idempotent: missing file is not an error.
	if err := provider.ClearAqpAccount(p); err != nil {
		t.Errorf("clearAccount missing: want nil, got %v", err)
	}
}

func TestContentTypeFor(t *testing.T) {
	cases := map[string]string{
		"page.html": "text/html; charset=utf-8",
		"app.js":    "text/javascript; charset=utf-8",
		"style.css": "text/css; charset=utf-8",
		"logo.png":  "application/octet-stream",
		"":          "application/octet-stream",
	}
	for name, want := range cases {
		if got := contentTypeFor(name); got != want {
			t.Errorf("contentTypeFor(%q)=%q want %q", name, got, want)
		}
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := atomicWrite(p, []byte("hello")); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello" {
		t.Errorf("atomicWrite content=%q want hello", b)
	}
}

func TestListArkAgentPlanModelIDs_DeadProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark","access_key":"AK","secret_key":"SK"}`), 0o600)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := dead.Listener.Addr().String()
	dead.Close()
	t.Setenv("HTTPS_PROXY", "http://"+addr)
	t.Setenv("HTTP_PROXY", "http://"+addr)
	_, err := listArkAgentPlanModelIDs("volcengine")
	if err == nil {
		t.Error("listArkAgentPlanModelIDs (dead proxy): want error, got nil")
	}
}

func TestPublicCookies(t *testing.T) {
	// nil jar -> nil.
	c := newAqpClient("/tmp/nope.json")
	if got := c.PublicCookies(); got != nil {
		t.Errorf("PublicCookies(nil jar)=%v want nil", got)
	}
	// With a cookie set on the jar for c.base.
	u, _ := url.Parse(provider.AqpBase)
	c2 := newAqpClient("/tmp/nope.json")
	c2.Jar.SetCookies(u, []*http.Cookie{{Name: provider.SsoCookieName, Value: "v"}})
	got := c2.PublicCookies()
	if len(got) != 1 || got[0].Name != provider.SsoCookieName {
		t.Errorf("PublicCookies=%+v want [%s=v]", got, provider.SsoCookieName)
	}
}

func TestParseStatsTime(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1700000000", 1700000000, true},
		{"2023-11-14T22:13:20Z", 1700000000, true},
		{"bogus", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseStatsTime(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseStatsTime(%q)=(%d,%v) want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestHealthLabel(t *testing.T) {
	cases := []struct {
		name string
		h    statusHealth
		want string
	}{
		{"open", statusHealth{CircuitState: "open"}, "circuit open"},
		{"half", statusHealth{CircuitState: "half_open"}, "half-open"},
		{"rate", statusHealth{RateLimitedUntil: "x"}, "rate-limited"},
		{"avail", statusHealth{Available: true}, "available"},
		{"unavail", statusHealth{}, "unavailable"},
	}
	for _, c := range cases {
		got, _ := healthLabel(c.h)
		if got != c.want {
			t.Errorf("healthLabel(%s)=%q want %q", c.name, got, c.want)
		}
	}
}

func TestSetChildNode(t *testing.T) {
	s := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	// nil parent is a no-op.
	setChildNode(nil, "k", s("v"))
	// Update an existing key.
	parent := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{s("a"), s("1"), s("b"), s("2")}}
	setChildNode(parent, "a", s("9"))
	if parent.Content[1].Value != "9" {
		t.Errorf("setChildNode update: a=%q want 9", parent.Content[1].Value)
	}
	// Append a missing key.
	setChildNode(parent, "c", s("3"))
	if parent.Content[len(parent.Content)-1].Value != "3" {
		t.Errorf("setChildNode append: last=%q want 3", parent.Content[len(parent.Content)-1].Value)
	}
}

// TestQueryAnalytics_DayBuckets verifies calendar-day grouping: minutes in the
// same local day collapse to one bucket whose start is local midnight; minutes
// spanning a day boundary split; storage stays 1-minute (lossless).
func TestQueryAnalytics_DayBuckets(t *testing.T) {
	ss := newTestStatsStore(t)
	// Anchor to local midnight so the day boundary is deterministic on any host
	// timezone (the SQL uses SQLite 'localtime'). d1m1/d1m2 share a local day;
	// d2m1 is the next local day.
	now := time.Now().In(time.Local)
	twoDaysAgoStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -2)
	d1m1 := twoDaysAgoStart.Add(1*time.Hour).Unix() / 60 * 60
	d1m2 := twoDaysAgoStart.Add(2*time.Hour).Unix() / 60 * 60
	d2m1 := twoDaysAgoStart.Add(25*time.Hour).Unix() / 60 * 60 // next local day
	if err := ss.flushDeltas(d1m1, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 1, Input: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := ss.flushDeltas(d1m2, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 2, Input: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := ss.flushDeltas(d2m1, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 4, Input: 400}}); err != nil {
		t.Fatal(err)
	}

	got, err := ss.queryAnalytics(d1m1-60, d2m1+60, "", "", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 day-buckets, got %d: %+v", len(got), got)
	}
	// Day 1 sums the two minutes (reqs 3, input 300); bucket == local midnight.
	day1 := got[0]
	if day1.Requests != 3 || day1.Input != 300 {
		t.Errorf("day1 sum wrong: %+v", day1)
	}
	if wantStart := twoDaysAgoStart.Unix(); day1.Bucket != wantStart {
		t.Errorf("day1 bucket = %d, want local-midnight %d", day1.Bucket, wantStart)
	}
	// Storage lossless: re-query raw 1-minute rows still see 3 rows.
	raw, _ := ss.queryRange(d1m1-60, d2m1+60, "", "", 60)
	if len(raw) != 3 {
		t.Errorf("storage not lossless: %d raw rows, want 3", len(raw))
	}
}

func TestQueryAnalytics_MonthBuckets(t *testing.T) {
	ss := newTestStatsStore(t)
	now := time.Now().In(time.Local)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	m1 := monthStart.AddDate(0, 0, 2).Unix() / 60 * 60 // same local month
	if err := ss.flushDeltas(m1, map[pmKey]statsCounters{{Provider: "p", Model: "m"}: {Requests: 5, Input: 50}}); err != nil {
		t.Fatal(err)
	}
	got, err := ss.queryAnalytics(m1-60, m1+60, "", "", "month")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Requests != 5 {
		t.Errorf("month bucket wrong: %+v", got)
	}
	if wantStart := monthStart.Unix(); got[0].Bucket != wantStart {
		t.Errorf("month bucket start = %d, want first-of-month %d", got[0].Bucket, wantStart)
	}
}

func TestQueryAnalytics_BadGranularity(t *testing.T) {
	ss := newTestStatsStore(t)
	if _, err := ss.queryAnalytics(0, 1, "", "", "hour"); err == nil {
		t.Error("hour granularity should error")
	}
}

// TestStatsFlags_GranularityCost_Parsed verifies --granularity and --cost parse
// into statsOpts (the new analytics-routing flags).
func TestStatsFlags_GranularityCost_Parsed(t *testing.T) {
	o := parseStatsFlags([]string{"--granularity", "month", "--cost", "--provider", "deepseek"})
	if o.Granularity != "month" || !o.Cost || o.Provider != "deepseek" {
		t.Errorf("parsed = %+v, want granularity=month cost=true provider=deepseek", o)
	}
}

// TestStatsFlags_GranularityCost_DefaultOff verifies the new flags default off
// (the CLI display contract: no behavioral change without flags).
func TestStatsFlags_GranularityCost_DefaultOff(t *testing.T) {
	o := parseStatsFlags([]string{"--from", "1", "--bucket", "1h"})
	if o.Granularity != "" || o.Cost {
		t.Errorf("new flags should default off: %+v", o)
	}
}

// TestStatsFlags_GranularityCost_EqualsForm verifies --granularity=value parses.
func TestStatsFlags_GranularityCost_EqualsForm(t *testing.T) {
	o := parseStatsFlags([]string{"--granularity=day", "--cost"})
	if o.Granularity != "day" || !o.Cost {
		t.Errorf("equals form: parsed = %+v, want granularity=day cost=true", o)
	}
}

// TestRenderStatsCLI_AnalyticsPath verifies --granularity/--cost route to
// /api/analytics (not /api/stats) and the table renders provider/model/cost.
// The table shape comes from formatAnalyticsTable; the cost column appears only
// when --cost is set. The classic /api/stats path is still covered by
// TestRenderStatsCLI above (byte-identity guard).
func TestRenderStatsCLI_AnalyticsPath(t *testing.T) {
	// granularity=month, two series, one with cost and one without (n/a).
	body := `{"granularity":"month","from":1700000000,"to":1700000000,` +
		`"series":[` +
		`{"provider":"deepseek","model":"deepseek-chat","points":[` +
		`{"bucket":1700000000,"requests":5,"input":1000,"output":500,"cost":0.12,"priced":true}]},` +
		`{"provider":"zhipu","model":"glm-5","points":[` +
		`{"bucket":1700000000,"requests":3,"input":200,"output":80,"cost":null,"priced":false}]}` +
		`],"totals":{"input":1200,"output":580,"cost":0.12},` +
		`"price_coverage":{"priced":["deepseek-chat"],"unpriced":["glm-5"]}}`

	var sawAnalytics, sawStats bool
	var lastQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/analytics" {
			sawAnalytics = true
			lastQuery = r.URL.RawQuery
			io.WriteString(w, body)
			return
		}
		if r.URL.Path == "/api/stats" {
			sawStats = true
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")

	// --granularity month --cost: routes to /api/analytics, table has a cost
	// column with the summed cost ($0.12) and n/a for the unpriced series.
	out, err := renderStats(listen, statsOpts{Granularity: "month", Cost: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAnalytics {
		t.Error("expected request to /api/analytics")
	}
	if sawStats {
		t.Error("did not expect request to /api/stats when granularity/cost set")
	}
	if !strings.Contains(lastQuery, "granularity=month") {
		t.Errorf("query missing granularity=month: %q", lastQuery)
	}
	for _, want := range []string{"deepseek", "deepseek-chat", "$0.12", "n/a", "month"} {
		if !strings.Contains(out, want) {
			t.Errorf("analytics table missing %q:\n%s", want, out)
		}
	}

	// --cost only (no granularity): defaults to day in the query string.
	sawAnalytics = false
	if _, err := renderStats(listen, statsOpts{Cost: true}); err != nil {
		t.Fatal(err)
	}
	if !sawAnalytics {
		t.Error("--cost alone should still route to /api/analytics")
	}
	if !strings.Contains(lastQuery, "granularity=day") {
		t.Errorf("--cost alone should default granularity=day in query: %q", lastQuery)
	}
}

// TestRenderStatsCLI_AnalyticsJSON verifies --json passes the analytics body
// through unchanged.
func TestRenderStatsCLI_AnalyticsJSON(t *testing.T) {
	body := `{"granularity":"day","series":[],"price_coverage":{"priced":[],"unpriced":[]}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")
	out, err := renderStats(listen, statsOpts{Granularity: "day", JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != body {
		t.Errorf("analytics --json passthrough mismatch:\ngot:  %s\nwant: %s", out, body)
	}
}

// TestFormatAnalyticsTable verifies the table renderer directly: header label
// reflects granularity, cost column appears only with withCost, and per-series
// cost sums correctly ( priced: $X.XX ; unpriced: n/a ).
func TestFormatAnalyticsTable(t *testing.T) {
	cost := 0.12
	resp := analyticsResp{Granularity: "month"}
	resp.Series = []struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Points   []struct {
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		} `json:"points"`
	}{
		{Provider: "deepseek", Model: "deepseek-chat", Points: []struct {
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		}{
			{Requests: 3, Input: 500, Output: 100, Cost: &cost},
			{Requests: 2, Input: 500, Output: 100, Cost: &cost},
		}},
		{Provider: "zhipu", Model: "glm-5", Points: []struct {
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		}{
			{Requests: 1, Input: 10, Output: 5, Cost: nil},
		}},
	}

	// Without cost: 6-column table, no "cost" header, no $ values.
	out := formatAnalyticsTable(resp, false)
	if !strings.Contains(out, "month") || !strings.Contains(out, "deepseek") {
		t.Errorf("table missing markers:\n%s", out)
	}
	if strings.Contains(out, "cost") || strings.Contains(out, "$") {
		t.Errorf("without --cost, table should not mention cost/$:\n%s", out)
	}
	// Series totals are sums across points.
	if !strings.Contains(out, compactNum(5)) { // 3+2 requests
		t.Errorf("month-1 requests sum missing:\n%s", out)
	}

	// With cost: 7-column table; priced series shows $0.24 (0.12+0.12),
	// unpriced shows n/a.
	out = formatAnalyticsTable(resp, true)
	if !strings.Contains(out, "cost") {
		t.Errorf("with --cost, table should have a cost header:\n%s", out)
	}
	if !strings.Contains(out, "$0.24") {
		t.Errorf("priced series cost should sum to $0.24:\n%s", out)
	}
	if !strings.Contains(out, "n/a") {
		t.Errorf("unpriced series should show n/a:\n%s", out)
	}
}

// TestQueryAgentRange_DefaultBucketKeepsEachMinute (regression #3): the default
// (bucketSecs<=60) agent query must return each stored 1-minute row losslessly.
// It used to always GROUP BY (agent,provider,model) while selecting bare minute
// + counters — SQLite then collapses all minutes for a key into ONE arbitrary
// row, silently losing every other minute's data.
func TestQueryAgentRange_DefaultBucketKeepsEachMinute(t *testing.T) {
	ss := newTestStatsStore(t)
	mk := func(req, in, out, lat, fail uint64) map[agentKey]agentCount {
		return map[agentKey]agentCount{
			{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {Requests: req, Input: in, Output: out, LatencySum: lat, Failures: fail},
		}
	}
	// Two distinct minutes for the SAME (agent,provider,model), distinct counts.
	if err := ss.flushAgentDeltas(0, mk(3, 100, 200, 1500, 0)); err != nil {
		t.Fatal(err)
	}
	if err := ss.flushAgentDeltas(60, mk(5, 400, 800, 6000, 1)); err != nil {
		t.Fatal(err)
	}
	got, err := ss.queryAgentRange(0, 60, "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("queryAgentRange(bucket=60) returned %d rows, want 2 (one per minute; pre-fix collapsed multi-minute data): %+v", len(got), got)
	}
	byMin := map[int64]agentBucket{}
	for _, b := range got {
		byMin[b.Minute] = b
	}
	m0, ok0 := byMin[0]
	m60, ok60 := byMin[60]
	if !ok0 || !ok60 {
		t.Fatalf("missing minute rows; got minutes %v", minuteKeys(byMin))
	}
	if m0.Requests != 3 || m0.Input != 100 || m0.Output != 200 || m0.LatencySum != 1500 || m0.Failures != 0 {
		t.Errorf("minute 0 row = %+v, want Req=3 In=100 Out=200 Lat=1500 Fail=0", m0)
	}
	if m60.Requests != 5 || m60.Input != 400 || m60.Output != 800 || m60.LatencySum != 6000 || m60.Failures != 1 {
		t.Errorf("minute 60 row = %+v, want Req=5 In=400 Out=800 Lat=6000 Fail=1", m60)
	}
}

func minuteKeys(m map[int64]agentBucket) []int64 {
	ks := make([]int64, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestFlush_DefersBaselineOnFlushError (regression #5): on a transient flush
// failure (disk full, SQLite busy), the diff baseline must NOT advance — the
// next flush re-diffs from the old baseline, re-accumulating the lost period.
// Pre-fix the baseline advanced before the SQLite write, permanently dropping
// that period's counters.
func TestFlush_DefersBaselineOnFlushError(t *testing.T) {
	ss := newTestStatsStore(t)
	metrics := newMetricsStore()
	f := newStatsFlusher(ss, metrics, newTokenCounter(), newAgentCounter(), nil)

	addReq := func(n int) {
		for i := 0; i < n; i++ {
			metrics.inc("zhipu", "glm-5", evRequests)
		}
	}

	// Period 1: 10 requests → flush succeeds, baseline advances to 10.
	addReq(10)
	f.flush(time.Unix(60, 0))
	if got := sumRequests(t, ss); got != 10 {
		t.Fatalf("after flush1, DB total = %d, want 10", got)
	}

	// Period 2: +5 (cumulative 15). Force flush to FAIL by swapping in a closed
	// DB handle, then restore it so the next flush + verification can proceed.
	addReq(5)
	goodDB := ss.db
	badDB, _ := sql.Open(statsDriver, ":memory:")
	badDB.Close() // closed pool → Begin() errors
	ss.db = badDB
	f.flush(time.Unix(120, 0)) // fails; pre-fix baseline still advances to 15
	ss.db = goodDB

	// Period 3: +3 (cumulative 18) → flush succeeds.
	addReq(3)
	f.flush(time.Unix(180, 0))

	// Nothing lost: DB total must equal cumulative (18). Pre-fix the failed
	// flush advanced the baseline, so period 2's 5 were dropped (total 13).
	if got := sumRequests(t, ss); got != 18 {
		t.Errorf("DB total = %d, want 18 (period 2's 5 requests lost when baseline advanced before flush success)", got)
	}
}

func sumRequests(t *testing.T, ss *statsStore) uint64 {
	rows, err := ss.db.Query(`SELECT requests FROM minute_buckets`)
	if err != nil {
		t.Fatalf("query minute_buckets: %v", err)
	}
	defer rows.Close()
	var n uint64
	for rows.Next() {
		var r uint64
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n += r
	}
	return n
}

// TestQueryAgentRange_WiderBucketSumsAndFloors (companion to #3): the >60s path
// must SUM the counters across minutes and floor the bucket to the window start.
func TestQueryAgentRange_WiderBucketSumsAndFloors(t *testing.T) {
	ss := newTestStatsStore(t)
	mk := func(req, in, out, lat, fail uint64) map[agentKey]agentCount {
		return map[agentKey]agentCount{
			{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {Requests: req, Input: in, Output: out, LatencySum: lat, Failures: fail},
		}
	}
	if err := ss.flushAgentDeltas(0, mk(3, 100, 200, 1500, 0)); err != nil {
		t.Fatal(err)
	}
	if err := ss.flushAgentDeltas(60, mk(5, 400, 800, 6000, 1)); err != nil {
		t.Fatal(err)
	}
	// 120s bucket spans both minutes (0..119) → one summed row, bucket start = 0.
	got, err := ss.queryAgentRange(0, 60, "", "", "", 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("bucket=120 returned %d rows, want 1 (two minutes summed): %+v", len(got), got)
	}
	b := got[0]
	if b.Minute != 0 {
		t.Errorf("bucket start = %d, want 0 (floor to window start)", b.Minute)
	}
	if b.Requests != 8 || b.Input != 500 || b.Output != 1000 || b.LatencySum != 7500 || b.Failures != 1 {
		t.Errorf("summed row = %+v, want Req=8 In=500 Out=1000 Lat=7500 Fail=1", b)
	}
}
