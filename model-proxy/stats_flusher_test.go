package main

import (
	"context"
	"fmt"
	obscounters "model-proxy/internal/observe/counters"
	"path/filepath"
	"strings"
	"testing"
	"time"

	observestats "model-proxy/internal/observe/stats"
)

func TestDiffCountersClampsAndOmits(t *testing.T) {
	prev := map[observestats.Key]observestats.Counters{
		{Provider: "a", Model: "x"}: {Requests: 5, Input: 10, LastRequestAt: 100},
	}
	cur := map[observestats.Key]observestats.Counters{
		{Provider: "a", Model: "x"}: {Requests: 8, Input: 10, LastRequestAt: 200}, // reqs +3, input unchanged, LRA advances
		{Provider: "b", Model: "y"}: {Requests: 1, LastRequestAt: 300},            // new key
	}
	d := diffCounters(cur, prev)
	if len(d) != 2 {
		t.Fatalf("diff = %d keys, want 2 (a/x has reqs delta, b/y is new)", len(d))
	}
	ax := d[observestats.Key{Provider: "a", Model: "x"}]
	if ax.Requests != 3 {
		t.Errorf("a/x reqs delta = %d, want 3", ax.Requests)
	}
	if ax.Input != 0 {
		t.Errorf("a/x input delta = %d, want 0 (unchanged)", ax.Input)
	}
	if ax.LastRequestAt != 200 {
		t.Errorf("a/x last_request_at = %d, want 200 (carried as cumulative max)", ax.LastRequestAt)
	}
	if d[observestats.Key{Provider: "b", Model: "y"}].Requests != 1 {
		t.Errorf("b/y new-key delta = %+v, want reqs=1", d[observestats.Key{Provider: "b", Model: "y"}])
	}

	// A reset race (cur < prev) must clamp to 0, never negative.
	neg := diffCounters(
		map[observestats.Key]observestats.Counters{{Provider: "a", Model: "x"}: {Requests: 2}},
		map[observestats.Key]observestats.Counters{{Provider: "a", Model: "x"}: {Requests: 5}},
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
	m := obscounters.NewMetricsStore()
	tc := obscounters.NewTokenCounter()
	f := newStatsFlusher(ss, m, tc, obscounters.NewAgentCounter(), map[observestats.Key]observestats.Counters{})

	// Minute 1: 1 request, 1 failover.
	m.Inc("z", "m", obscounters.EvRequests)
	m.Inc("z", "m", obscounters.EvFailovers)
	tc.Commit(obscounters.TokenKey{Provider: "z", Model: "m"}, obscounters.TokenUsage{Input: 10, Output: 5, Requests: 1})
	if !f.flush(time.Now()) {
		t.Fatal("first flush should write deltas")
	}

	// Minute 2: 2 more requests (total 3), more tokens.
	m.Inc("z", "m", obscounters.EvRequests)
	m.Inc("z", "m", obscounters.EvRequests)
	tc.Commit(obscounters.TokenKey{Provider: "z", Model: "m"}, obscounters.TokenUsage{Input: 20, Output: 5, Requests: 1})
	if !f.flush(time.Now()) {
		t.Fatal("second flush should write deltas")
	}

	buckets, err := ss.QueryRange(0, time.Now().Unix()+3600, "", "", 60)
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
	ss, err := openTestStatsStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := obscounters.NewMetricsStore()
	tc := obscounters.NewTokenCounter()
	f := newStatsFlusher(ss, m, tc, obscounters.NewAgentCounter(), map[observestats.Key]observestats.Counters{})
	m.Inc("a", "x", obscounters.EvRequests)
	m.Inc("a", "x", obscounters.EvRequests)
	m.Inc("a", "x", obscounters.EvFailures)
	tc.Commit(obscounters.TokenKey{Provider: "a", Model: "x"}, obscounters.TokenUsage{Input: 100, Output: 50, CacheRead: 7})
	f.flush(time.Now())
	if err := ss.Close(); err != nil {
		t.Fatal(err)
	}

	ss2, err := openTestStatsStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ss2.Close()
	base, err := ss2.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	got := base[observestats.Key{Provider: "a", Model: "x"}]
	// commit() bumps Requests by 1 per commit (one commit here) -> TokenRequests=1.
	if got.Requests != 2 || got.Failures != 1 || got.Input != 100 || got.Output != 50 || got.CacheRead != 7 || got.TokenRequests != 1 {
		t.Errorf("restored cumulative = %+v, want reqs=2 fail=1 in=100 out=50 cr=7 treqs=1", got)
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
	m := obscounters.NewMetricsStore()
	tc := obscounters.NewTokenCounter()
	k := obscounters.PMKey{Provider: "z", Model: "m"}
	m.Seed(k, obscounters.ProviderMetricsSnapshot{Requests: 100, Failovers: 3, Failures: 2, RateLimited429: 1, LastRequestAt: 42})
	tc.Seed(k, obscounters.TokenUsage{Input: 500, Output: 50, CacheCreation: 9, CacheRead: 4, Requests: 7})

	msnap := m.Snapshot()[k]
	if msnap.Requests != 100 || msnap.Failovers != 3 || msnap.Failures != 2 || msnap.RateLimited429 != 1 || msnap.LastRequestAt != 42 {
		t.Errorf("metrics seed = %+v, want exact baseline", msnap)
	}
	tsnap := tc.Snapshot()[k]
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
	f := newStatsFlusher(ss, obscounters.NewMetricsStore(), obscounters.NewTokenCounter(), obscounters.NewAgentCounter(), map[observestats.Key]observestats.Counters{})
	if f.flush(time.Now()) {
		t.Error("flush with no deltas should report false")
	}
	got, _ := ss.QueryRange(0, time.Now().Unix()+3600, "", "", 60)
	if len(got) != 0 {
		t.Errorf("empty flush wrote %d rows, want 0", len(got))
	}
}

// TestFlush_DefersBaselineOnFlushError (regression #5): a transient Store
// failure must retain the failed minute batch. Recovery persists that batch in
// its original minute before the new minute, without loss or duplication.
func TestFlush_DefersBaselineOnFlushError(t *testing.T) {
	ss := newTestStatsStore(t)
	sink := &failOnceStatsSink{Store: ss}
	metrics := obscounters.NewMetricsStore()
	f := newStatsFlusher(sink, metrics, obscounters.NewTokenCounter(), obscounters.NewAgentCounter(), nil)

	addReq := func(n int) {
		for i := 0; i < n; i++ {
			metrics.Inc("zhipu", "glm-5", obscounters.EvRequests)
		}
	}

	// Period 1: 10 requests → flush succeeds.
	addReq(10)
	f.flush(time.Unix(60, 0))
	if got := sumRequests(t, ss); got != 10 {
		t.Fatalf("after flush1, DB total = %d, want 10", got)
	}

	// Period 2: +5 (cumulative 15). Force one Store-port failure without
	// reaching into SQLite internals.
	addReq(5)
	sink.failNext = true
	if f.flush(time.Unix(120, 0)) {
		t.Error("failed flush reported a durable write")
	}

	// Period 3: +3 (cumulative 18) → flush succeeds.
	addReq(3)
	f.flush(time.Unix(180, 0))

	// Nothing lost: DB total must equal cumulative (18), and the failed period
	// remains attributed to its original completed-minute batch.
	if got := sumRequests(t, ss); got != 18 {
		t.Errorf("DB total = %d, want 18", got)
	}
	rows, err := ss.QueryRange(0, 240, "zhipu", "glm-5", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 ||
		rows[0].Minute != 60 || rows[0].Requests != 10 ||
		rows[1].Minute != 120 || rows[1].Requests != 5 ||
		rows[2].Minute != 180 || rows[2].Requests != 3 {
		t.Errorf("recovered minute batches = %+v, want 60:10, 120:5, 180:3", rows)
	}
}

type alwaysFailStatsSink struct {
	*observestats.Store
}

func (sink *alwaysFailStatsSink) FlushContext(
	context.Context,
	int64,
	map[observestats.Key]observestats.Counters,
) error {
	return fmt.Errorf("injected persistent provider failure")
}

func (sink *alwaysFailStatsSink) FlushAgentsContext(
	context.Context,
	int64,
	map[observestats.AgentKey]observestats.AgentCounters,
) error {
	return fmt.Errorf("injected persistent agent failure")
}

func TestStatsPendingBacklogIsBoundedWithoutLosingTotals(t *testing.T) {
	store := newTestStatsStore(t)
	sink := &alwaysFailStatsSink{Store: store}
	metrics := obscounters.NewMetricsStore()
	agents := obscounters.NewAgentCounter()
	flusher := newStatsFlusher(sink, metrics, obscounters.NewTokenCounter(), agents, nil)
	key := observestats.Key{Provider: "zhipu", Model: "glm-5"}
	agentKey := observestats.AgentKey{
		Agent: "codex", Provider: "zhipu", Model: "glm-5",
	}
	const overflow = 25
	total := maxPendingStatsBatches + overflow

	for index := 0; index < total; index++ {
		metrics.Inc(key.Provider, key.Model, obscounters.EvRequests)
		agents.IncRequests(agentKey.Agent, agentKey.Provider, agentKey.Model)
		flusher.flush(time.Unix(int64(index+2)*60, 0))
	}

	if len(flusher.pending) != maxPendingStatsBatches ||
		len(flusher.agentQueue) != maxPendingStatsBatches {
		t.Fatalf(
			"bounded queues = provider:%d agent:%d, want %d each",
			len(flusher.pending),
			len(flusher.agentQueue),
			maxPendingStatsBatches,
		)
	}
	if flusher.coalesced != overflow || flusher.agentCoalesced != overflow {
		t.Errorf(
			"coalesced counts = provider:%d agent:%d, want %d each",
			flusher.coalesced,
			flusher.agentCoalesced,
			overflow,
		)
	}
	var providerTotal, agentTotal uint64
	for _, batch := range flusher.pending {
		providerTotal += batch.deltas[key].Requests
	}
	for _, batch := range flusher.agentQueue {
		agentTotal += batch.deltas[agentKey].Requests
	}
	if providerTotal != uint64(total) || agentTotal != uint64(total) {
		t.Errorf(
			"queued totals = provider:%d agent:%d, want %d each",
			providerTotal,
			agentTotal,
			total,
		)
	}
	wantFolded := uint64(overflow + 1)
	if got := flusher.pending[0].minute; got != 60 {
		t.Errorf("oldest provider batch minute = %d, want earliest boundary 60", got)
	}
	if got := flusher.pending[0].deltas[key].Requests; got != wantFolded {
		t.Errorf("oldest provider batch = %d requests, want folded total %d", got, wantFolded)
	}
	if got := flusher.agentQueue[0].minute; got != 60 {
		t.Errorf("oldest agent batch minute = %d, want earliest boundary 60", got)
	}
	if got := flusher.agentQueue[0].deltas[agentKey].Requests; got != wantFolded {
		t.Errorf("oldest agent batch = %d requests, want folded total %d", got, wantFolded)
	}
}

func TestStatsPendingDrainIsBoundedPerCycle(t *testing.T) {
	store := newTestStatsStore(t)
	flusher := newStatsFlusher(
		store,
		obscounters.NewMetricsStore(),
		obscounters.NewTokenCounter(),
		obscounters.NewAgentCounter(),
		nil,
	)
	key := observestats.Key{Provider: "p", Model: "m"}
	total := maxStatsBatchesPerFlush + 5
	for index := 0; index < total; index++ {
		flusher.enqueue(statsBatch{
			minute: int64(index+1) * 60,
			deltas: map[observestats.Key]observestats.Counters{
				key: {Requests: 1},
			},
		})
	}

	if !flusher.flushPending(context.Background(), maxStatsBatchesPerFlush) {
		t.Fatal("bounded drain did not persist any batch")
	}
	if got := len(flusher.pending); got != 5 {
		t.Errorf("pending after one drain = %d, want 5", got)
	}
	rows, err := store.QueryRange(0, int64(total+1)*60, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != maxStatsBatchesPerFlush {
		t.Errorf("persisted rows = %d, want per-cycle limit %d",
			len(rows), maxStatsBatchesPerFlush)
	}
}

func TestMergeStatsDeltasPreservesAllCounters(t *testing.T) {
	key := observestats.Key{Provider: "p", Model: "m"}
	destination := map[observestats.Key]observestats.Counters{key: {
		Requests: 1, Failovers: 2, RateLimited429: 3, Failures: 4,
		Input: 5, Output: 6, CacheCreation: 7, CacheRead: 8,
		TokenRequests: 9, LastRequestAt: 200, LatencySum: 10, TTFTSum: 11,
	}}
	mergeStatsDeltas(destination, map[observestats.Key]observestats.Counters{key: {
		Requests: 11, Failovers: 12, RateLimited429: 13, Failures: 14,
		Input: 15, Output: 16, CacheCreation: 17, CacheRead: 18,
		TokenRequests: 19, LastRequestAt: 100, LatencySum: 20, TTFTSum: 21,
	}})
	want := observestats.Counters{
		Requests: 12, Failovers: 14, RateLimited429: 16, Failures: 18,
		Input: 20, Output: 22, CacheCreation: 24, CacheRead: 26,
		TokenRequests: 28, LastRequestAt: 200, LatencySum: 30, TTFTSum: 32,
	}
	if got := destination[key]; got != want {
		t.Errorf("merged counters = %+v, want %+v", got, want)
	}
}

type failOnceStatsSink struct {
	*observestats.Store
	failNext bool
}

func (sink *failOnceStatsSink) FlushContext(
	ctx context.Context,
	minute int64,
	deltas map[observestats.Key]observestats.Counters,
) error {
	if sink.failNext {
		sink.failNext = false
		return fmt.Errorf("injected stats flush failure")
	}
	return sink.Store.FlushContext(ctx, minute, deltas)
}

func sumRequests(t *testing.T, ss *observestats.Store) uint64 {
	t.Helper()
	rows, err := ss.QueryRange(0, time.Now().Add(24*time.Hour).Unix(), "", "", 60)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	var total uint64
	for _, row := range rows {
		total += row.Requests
	}
	return total
}

// TestDiffAgent: per-key deltas clamp at 0 and omit unchanged keys.
func TestDiffAgent(t *testing.T) {
	cur := map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5, Input: 10, Output: 2},
		{Agent: "b", Provider: "z", Model: "m"}: {Requests: 3, Input: 0, Output: 0},
	}
	prev := map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 2, Input: 10, Output: 0}, // reqs +3, output +2; input unchanged
	}
	d := diffAgent(cur, prev)
	ad, ok := d[observestats.AgentKey{Agent: "a", Provider: "z", Model: "m"}]
	if !ok || ad.Requests != 3 || ad.Input != 0 || ad.Output != 2 {
		t.Errorf("a delta = %+v want reqs=3 in=0 out=2", ad)
	}
	bd, ok := d[observestats.AgentKey{Agent: "b", Provider: "z", Model: "m"}]
	if !ok || bd.Requests != 3 {
		t.Errorf("b delta = %+v want reqs=3 (new key)", bd)
	}
	// A key whose counters only decreased (e.g. after reset) clamps to 0 and is
	// omitted when ALL fields are 0.
	d2 := diffAgent(
		map[observestats.AgentKey]observestats.AgentCounters{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 1}},
		map[observestats.AgentKey]observestats.AgentCounters{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5}},
	)
	if _, present := d2[observestats.AgentKey{Agent: "a", Provider: "z", Model: "m"}]; present {
		t.Errorf("all-zero delta should be omitted, got %+v", d2)
	}
}
