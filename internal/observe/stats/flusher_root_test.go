package stats

import (
	"context"
	"fmt"
	obscounters "model-proxy/internal/observe/counters"
	"path/filepath"
	"testing"
	"time"
)

func TestDiffCountersClampsAndOmits(t *testing.T) {
	prev := map[Key]Counters{
		{Provider: "a", Model: "x"}: {Requests: 5, Input: 10, LastRequestAt: 100},
	}
	cur := map[Key]Counters{
		{Provider: "a", Model: "x"}: {Requests: 8, Input: 10, LastRequestAt: 200}, // reqs +3, input unchanged, LRA advances
		{Provider: "b", Model: "y"}: {Requests: 1, LastRequestAt: 300},            // new key
	}
	d := DiffCounters(cur, prev)
	if len(d) != 2 {
		t.Fatalf("diff = %d keys, want 2 (a/x has reqs delta, b/y is new)", len(d))
	}
	ax := d[Key{Provider: "a", Model: "x"}]
	if ax.Requests != 3 {
		t.Errorf("a/x reqs delta = %d, want 3", ax.Requests)
	}
	if ax.Input != 0 {
		t.Errorf("a/x input delta = %d, want 0 (unchanged)", ax.Input)
	}
	if ax.LastRequestAt != 200 {
		t.Errorf("a/x last_request_at = %d, want 200 (carried as cumulative max)", ax.LastRequestAt)
	}
	if d[Key{Provider: "b", Model: "y"}].Requests != 1 {
		t.Errorf("b/y new-key delta = %+v, want reqs=1", d[Key{Provider: "b", Model: "y"}])
	}

	// A reset race (cur < prev) must clamp to 0, never negative.
	neg := DiffCounters(
		map[Key]Counters{{Provider: "a", Model: "x"}: {Requests: 2}},
		map[Key]Counters{{Provider: "a", Model: "x"}: {Requests: 5}},
	)
	if len(neg) != 0 {
		t.Errorf("negative-delta key should be omitted, got %+v", neg)
	}
}

// TestStatsFlushTwoMinutes verifies per-minute bucketing: two flushes produce two
// distinct buckets whose deltas sum to the cumulative total. Exercises the
// flusher diff + lastBucket monotonic guard.
func TestStatsFlushTwoMinutes(t *testing.T) {
	ss := newTestStore(t, 0)
	m := obscounters.NewMetricsStore()
	tc := obscounters.NewTokenCounter()
	f := NewFlusher(ss, m, tc, obscounters.NewAgentCounter(), map[Key]Counters{}, nil)

	// Minute 1: 1 request, 1 failover.
	m.Inc("z", "m", obscounters.EvRequests)
	m.Inc("z", "m", obscounters.EvFailovers)
	tc.Commit(obscounters.TokenKey{Provider: "z", Model: "m"}, obscounters.TokenUsage{Input: 10, Output: 5, Requests: 1})
	if !f.Flush(time.Now()) {
		t.Fatal("first flush should write deltas")
	}

	// Minute 2: 2 more requests (total 3), more tokens.
	m.Inc("z", "m", obscounters.EvRequests)
	m.Inc("z", "m", obscounters.EvRequests)
	tc.Commit(obscounters.TokenKey{Provider: "z", Model: "m"}, obscounters.TokenUsage{Input: 20, Output: 5, Requests: 1})
	if !f.Flush(time.Now()) {
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
	ss, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	m := obscounters.NewMetricsStore()
	tc := obscounters.NewTokenCounter()
	f := NewFlusher(ss, m, tc, obscounters.NewAgentCounter(), map[Key]Counters{}, nil)
	m.Inc("a", "x", obscounters.EvRequests)
	m.Inc("a", "x", obscounters.EvRequests)
	m.Inc("a", "x", obscounters.EvFailures)
	tc.Commit(obscounters.TokenKey{Provider: "a", Model: "x"}, obscounters.TokenUsage{Input: 100, Output: 50, CacheRead: 7})
	f.Flush(time.Now())
	if err := ss.Close(); err != nil {
		t.Fatal(err)
	}

	ss2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer ss2.Close()
	base, err := ss2.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	got := base[Key{Provider: "a", Model: "x"}]
	// commit() bumps Requests by 1 per commit (one commit here) -> TokenRequests=1.
	if got.Requests != 2 || got.Failures != 1 || got.Input != 100 || got.Output != 50 || got.CacheRead != 7 || got.TokenRequests != 1 {
		t.Errorf("restored cumulative = %+v, want reqs=2 fail=1 in=100 out=50 cr=7 treqs=1", got)
	}
}

func TestUntilNextMinuteBounded(t *testing.T) {
	d := UntilNextMinute(time.Now())
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
	if got := LegacyTokensPath("/home/u"); got != "/home/u/.model-proxy/token_usage.json" {
		t.Errorf("LegacyTokensPath = %q, want /home/u/.model-proxy/token_usage.json", got)
	}
}

// TestStatsFlushEmptyIsNoop verifies a flush with no activity writes nothing and
// reports false (the idle-proxy path).
func TestStatsFlushEmptyIsNoop(t *testing.T) {
	ss := newTestStore(t, 0)
	f := NewFlusher(ss, obscounters.NewMetricsStore(), obscounters.NewTokenCounter(), obscounters.NewAgentCounter(), map[Key]Counters{}, nil)
	if f.Flush(time.Now()) {
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
	ss := newTestStore(t, 0)
	sink := &failOnceStatsSink{Store: ss}
	metrics := obscounters.NewMetricsStore()
	f := NewFlusher(sink, metrics, obscounters.NewTokenCounter(), obscounters.NewAgentCounter(), nil, nil)

	addReq := func(n int) {
		for i := 0; i < n; i++ {
			metrics.Inc("zhipu", "glm-5", obscounters.EvRequests)
		}
	}

	// Period 1: 10 requests → flush succeeds.
	addReq(10)
	f.Flush(time.Unix(60, 0))
	if got := sumRequests(t, ss); got != 10 {
		t.Fatalf("after flush1, DB total = %d, want 10", got)
	}

	// Period 2: +5 (cumulative 15). Force one Store-port failure without
	// reaching into SQLite internals.
	addReq(5)
	sink.failNext = true
	if f.Flush(time.Unix(120, 0)) {
		t.Error("failed flush reported a durable write")
	}

	// Period 3: +3 (cumulative 18) → flush succeeds.
	addReq(3)
	f.Flush(time.Unix(180, 0))

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

// failOnceMCPSink wraps a Store and fails the next name-level and tool-level
// MCP flush once each — the transient-failure seam for the MCP retry test
// (test-side only; the production package carries no test hooks).
type failOnceMCPSink struct {
	*Store
	failNameNext bool
	failToolNext bool
}

func (sink *failOnceMCPSink) FlushMCPBucketsContext(
	ctx context.Context,
	minute int64,
	deltas []MCPBucketDelta,
) error {
	if sink.failNameNext {
		sink.failNameNext = false
		return fmt.Errorf("injected MCP flush failure")
	}
	return sink.Store.FlushMCPBucketsContext(ctx, minute, deltas)
}

func (sink *failOnceMCPSink) FlushMCPToolBucketsContext(
	ctx context.Context,
	minute int64,
	deltas []MCPToolBucketDelta,
) error {
	if sink.failToolNext {
		sink.failToolNext = false
		return fmt.Errorf("injected MCP tool flush failure")
	}
	return sink.Store.FlushMCPToolBucketsContext(ctx, minute, deltas)
}

// TestFlush_MCPDefersBaselineOnFlushError is the MCP counterpart of
// TestFlush_DefersBaselineOnFlushError: a transient Store failure on either
// MCP pipeline must retain the failed minute batch; recovery persists each
// batch in its original minute, without loss or duplication.
func TestFlush_MCPDefersBaselineOnFlushError(t *testing.T) {
	ss := newTestStore(t, 0)
	sink := &failOnceMCPSink{Store: ss}
	mcp := obscounters.NewMCPStats()
	kind := func(name string) (MCPKind, bool) {
		if name == "srv" {
			return MCPKindServer, true
		}
		return "", false
	}
	f := NewFlusher(
		sink,
		obscounters.NewMetricsStore(),
		obscounters.NewTokenCounter(),
		obscounters.NewAgentCounter(),
		nil, nil,
		WithMCPStats(mcp, kind),
	)

	record := func(calls int) {
		for i := 0; i < calls; i++ {
			mcp.Record("srv", 200, 100)
			mcp.RecordTool("srv", "search", 200, 100)
		}
	}

	// Period 1: 2 calls → flush succeeds at minute 60.
	record(2)
	if !f.Flush(time.Unix(60, 0)) {
		t.Fatal("first flush should write deltas")
	}

	// Period 2: +3 calls. One Store-port failure on each MCP pipeline.
	record(3)
	sink.failNameNext = true
	sink.failToolNext = true
	if f.Flush(time.Unix(120, 0)) {
		t.Error("failed flush reported a durable write")
	}

	// Period 3: +1 call → flush succeeds and drains the retained batches.
	record(1)
	if !f.Flush(time.Unix(180, 0)) {
		t.Fatal("recovery flush should write deltas")
	}

	rows, err := ss.QueryMCPBuckets(0, 240, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("mcp rows = %d, want 3: %+v", len(rows), rows)
	}
	byMinute := map[int64]MCPBucketRow{}
	for _, row := range rows {
		byMinute[row.Bucket] = row
	}
	for minute, wantCalls := range map[int64]uint64{60: 2, 120: 3, 180: 1} {
		row, ok := byMinute[minute]
		if !ok {
			t.Fatalf("mcp minute %d missing from %+v", minute, rows)
		}
		if row.Name != "srv" || row.Kind != "server" ||
			row.Calls != wantCalls || row.Errors != 0 ||
			row.LatencyMsSum != 100*wantCalls {
			t.Errorf("mcp minute %d = %+v, want srv/server calls=%d errors=0 latency=%d",
				minute, row, wantCalls, 100*wantCalls)
		}
	}

	toolRows, err := ss.QueryMCPToolBuckets(0, 240, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(toolRows) != 3 {
		t.Fatalf("mcp tool rows = %d, want 3: %+v", len(toolRows), toolRows)
	}
	toolByMinute := map[int64]MCPToolBucketRow{}
	for _, row := range toolRows {
		toolByMinute[row.Bucket] = row
	}
	for minute, wantCalls := range map[int64]uint64{60: 2, 120: 3, 180: 1} {
		row, ok := toolByMinute[minute]
		if !ok {
			t.Fatalf("mcp tool minute %d missing from %+v", minute, toolRows)
		}
		if row.Name != "srv" || row.Tool != "search" ||
			row.Calls != wantCalls || row.LatencyMsSum != 100*wantCalls {
			t.Errorf("mcp tool minute %d = %+v, want srv/search calls=%d latency=%d",
				minute, row, wantCalls, 100*wantCalls)
		}
	}
}

type alwaysFailStatsSink struct {
	*Store
}

func (sink *alwaysFailStatsSink) FlushContext(
	context.Context,
	int64,
	map[Key]Counters,
) error {
	return fmt.Errorf("injected persistent provider failure")
}

func (sink *alwaysFailStatsSink) FlushAgentsContext(
	context.Context,
	int64,
	map[AgentKey]AgentCounters,
) error {
	return fmt.Errorf("injected persistent agent failure")
}

func (sink *alwaysFailStatsSink) FlushMCPBucketsContext(
	context.Context,
	int64,
	[]MCPBucketDelta,
) error {
	return fmt.Errorf("injected persistent MCP failure")
}

func (sink *alwaysFailStatsSink) FlushMCPToolBucketsContext(
	context.Context,
	int64,
	[]MCPToolBucketDelta,
) error {
	return fmt.Errorf("injected persistent MCP tool failure")
}

func TestStatsPendingBacklogIsBoundedWithoutLosingTotals(t *testing.T) {
	store := newTestStore(t, 0)
	sink := &alwaysFailStatsSink{Store: store}
	metrics := obscounters.NewMetricsStore()
	agents := obscounters.NewAgentCounter()
	flusher := NewFlusher(sink, metrics, obscounters.NewTokenCounter(), agents, nil, nil)
	key := Key{Provider: "zhipu", Model: "glm-5"}
	agentKey := AgentKey{
		Agent: "codex", Provider: "zhipu", Model: "glm-5",
	}
	const overflow = 25
	total := MaxPendingStatsBatches + overflow

	for index := 0; index < total; index++ {
		metrics.Inc(key.Provider, key.Model, obscounters.EvRequests)
		agents.IncRequests(agentKey.Agent, agentKey.Provider, agentKey.Model)
		flusher.Flush(time.Unix(int64(index+2)*60, 0))
	}

	if len(flusher.pending) != MaxPendingStatsBatches ||
		len(flusher.agentQueue) != MaxPendingStatsBatches {
		t.Fatalf(
			"bounded queues = provider:%d agent:%d, want %d each",
			len(flusher.pending),
			len(flusher.agentQueue),
			MaxPendingStatsBatches,
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
	store := newTestStore(t, 0)
	flusher := NewFlusher(
		store,
		obscounters.NewMetricsStore(),
		obscounters.NewTokenCounter(),
		obscounters.NewAgentCounter(),
		nil, nil)
	key := Key{Provider: "p", Model: "m"}
	total := MaxStatsBatchesPerFlush + 5
	for index := 0; index < total; index++ {
		flusher.enqueue(Batch{
			minute: int64(index+1) * 60,
			deltas: map[Key]Counters{
				key: {Requests: 1},
			},
		})
	}

	if !flusher.flushPending(context.Background(), MaxStatsBatchesPerFlush) {
		t.Fatal("bounded drain did not persist any batch")
	}
	if got := len(flusher.pending); got != 5 {
		t.Errorf("pending after one drain = %d, want 5", got)
	}
	rows, err := store.QueryRange(0, int64(total+1)*60, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != MaxStatsBatchesPerFlush {
		t.Errorf("persisted rows = %d, want per-cycle limit %d",
			len(rows), MaxStatsBatchesPerFlush)
	}
}

// TestStatsPendingMCPBacklogIsBoundedWithoutLosingTotals is the MCP/tool
// counterpart of TestStatsPendingBacklogIsBoundedWithoutLosingTotals: under a
// persistently failing Store port both MCP queues cap at
// MaxPendingStatsBatches, the oldest folded batch keeps the earliest completed
// minute, and coalescing never loses calls/errors/latency totals.
func TestStatsPendingMCPBacklogIsBoundedWithoutLosingTotals(t *testing.T) {
	sink := &alwaysFailStatsSink{Store: newTestStore(t, 0)}
	mcp := obscounters.NewMCPStats()
	kind := func(name string) (MCPKind, bool) {
		switch name {
		case "srv":
			return MCPKindServer, true
		case "rt":
			return MCPKindRoute, true
		default:
			return "", false
		}
	}
	flusher := NewFlusher(
		sink,
		obscounters.NewMetricsStore(),
		obscounters.NewTokenCounter(),
		obscounters.NewAgentCounter(),
		nil, nil,
		WithMCPStats(mcp, kind),
	)

	const overflow = 25
	total := MaxPendingStatsBatches + overflow
	lastCallAt := make([]int64, 0, total)
	for index := 0; index < total; index++ {
		// Per iteration: srv 2 calls / 1 error / 140ms, rt 1 call, one tool call.
		mcp.Record("srv", 200, 100)
		mcp.Record("srv", 500, 40)
		mcp.Record("rt", 200, 10)
		mcp.RecordTool("srv", "search", 200, 30)
		lastCallAt = append(lastCallAt, mcp.RawSnapshot()["srv"].LastCallAt)
		flusher.Flush(time.Unix(int64(index+2)*60, 0))
	}

	if len(flusher.mcpQueue) != MaxPendingStatsBatches ||
		len(flusher.mcpToolQueue) != MaxPendingStatsBatches {
		t.Fatalf(
			"bounded queues = mcp:%d mcpTool:%d, want %d each",
			len(flusher.mcpQueue), len(flusher.mcpToolQueue), MaxPendingStatsBatches,
		)
	}
	if flusher.mcpCoalesced != overflow || flusher.mcpToolCoalesced != overflow {
		t.Errorf(
			"coalesced counts = mcp:%d mcpTool:%d, want %d each",
			flusher.mcpCoalesced, flusher.mcpToolCoalesced, overflow,
		)
	}

	var srvCalls, srvErrors, srvLatency, rtCalls, toolCalls uint64
	for _, batch := range flusher.mcpQueue {
		for _, delta := range batch.deltas {
			switch delta.Name {
			case "srv":
				srvCalls += delta.Calls
				srvErrors += delta.Errors
				srvLatency += delta.LatencyMsSum
			case "rt":
				rtCalls += delta.Calls
			}
		}
	}
	for _, batch := range flusher.mcpToolQueue {
		for _, delta := range batch.deltas {
			toolCalls += delta.Calls
		}
	}
	if srvCalls != 2*uint64(total) || srvErrors != uint64(total) ||
		srvLatency != 140*uint64(total) {
		t.Errorf(
			"queued srv totals = calls:%d errors:%d latency:%d, want %d/%d/%d (lossless coalescing)",
			srvCalls, srvErrors, srvLatency, 2*total, total, 140*total,
		)
	}
	if rtCalls != uint64(total) {
		t.Errorf("queued rt calls = %d, want %d", rtCalls, total)
	}
	if toolCalls != uint64(total) {
		t.Errorf("queued tool calls = %d, want %d", toolCalls, total)
	}

	// The earliest boundary stays at the first completed minute (60) and the
	// folded oldest batch carries exactly overflow+1 iterations of traffic.
	if got := flusher.mcpQueue[0].minute; got != 60 {
		t.Errorf("oldest mcp batch minute = %d, want earliest boundary 60", got)
	}
	if got := flusher.mcpToolQueue[0].minute; got != 60 {
		t.Errorf("oldest mcp tool batch minute = %d, want earliest boundary 60", got)
	}
	wantFolded := uint64(overflow + 1)
	folded := mcpQueueDelta(flusher.mcpQueue[0].deltas, "srv")
	if folded == nil {
		t.Fatal("oldest mcp batch lost the srv delta")
	}
	if folded.Calls != 2*wantFolded || folded.Errors != wantFolded ||
		folded.LatencyMsSum != 140*wantFolded {
		t.Errorf(
			"oldest folded srv delta = %+v, want calls:%d errors:%d latency:%d",
			folded, 2*wantFolded, wantFolded, 140*wantFolded,
		)
	}
	// LastCallAt survives coalescing as the max over the folded window. Record
	// stamps unix seconds, so a fast host produces equal timestamps; the exact
	// max-not-first semantics are pinned in
	// TestStatsMCPCoalescingKeepsOldestMinuteAndMaxLastCallAt below.
	wantLastCallAt := lastCallAt[0]
	for _, ts := range lastCallAt[1 : overflow+1] {
		if ts > wantLastCallAt {
			wantLastCallAt = ts
		}
	}
	if folded.LastCallAt != wantLastCallAt {
		t.Errorf(
			"oldest folded srv last_call_at = %d, want max over the first %d iterations = %d",
			folded.LastCallAt, overflow+1, wantLastCallAt,
		)
	}
}

func mcpQueueDelta(deltas []MCPBucketDelta, name string) *MCPBucketDelta {
	for i := range deltas {
		if deltas[i].Name == name {
			return &deltas[i]
		}
	}
	return nil
}

func mcpToolQueueDelta(deltas []MCPToolBucketDelta, name, tool string) *MCPToolBucketDelta {
	for i := range deltas {
		if deltas[i].Name == name && deltas[i].Tool == tool {
			return &deltas[i]
		}
	}
	return nil
}

// TestStatsMCPCoalescingKeepsOldestMinuteAndMaxLastCallAt pins the enqueue
// merge semantics directly — they differ from the provider queue's
// MergeDeltas: enqueueMCP folds the two oldest batches with per-name map
// accumulation, keeps the OLDEST minute, and takes the MAX last_call_at of
// the folded pair (not the first batch's value). The tool merge accumulates
// per (name, tool) key and never crosses keys. Hand-built batches give each
// minute a distinct last_call_at, which Record's second-granularity clock
// cannot guarantee.
func TestStatsMCPCoalescingKeepsOldestMinuteAndMaxLastCallAt(t *testing.T) {
	flusher := NewFlusher(
		newTestStore(t, 0),
		obscounters.NewMetricsStore(),
		obscounters.NewTokenCounter(),
		obscounters.NewAgentCounter(),
		nil, nil,
	)
	for index := 0; index < MaxPendingStatsBatches; index++ {
		minute := int64(index+1) * 60 // doubles as the delta's last_call_at: strictly increasing
		flusher.enqueueMCP(MCPBatch{minute: minute, deltas: []MCPBucketDelta{
			{Name: "srv", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: minute},
		}})
		flusher.enqueueMCPTool(MCPToolBatch{minute: minute, deltas: []MCPToolBucketDelta{
			{Name: "srv", Tool: "search", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: minute},
			{Name: "srv", Tool: "fetch", Kind: MCPKindServer, Calls: 5, LatencyMsSum: 50, LastCallAt: minute},
		}})
	}
	// One batch past the cap folds the two oldest batches (minute 60 into 120).
	overflowMinute := int64(MaxPendingStatsBatches+1) * 60
	flusher.enqueueMCP(MCPBatch{minute: overflowMinute, deltas: []MCPBucketDelta{
		{Name: "srv", Kind: MCPKindServer, Calls: 7, Errors: 2, LatencyMsSum: 70, LastCallAt: overflowMinute},
	}})
	flusher.enqueueMCPTool(MCPToolBatch{minute: overflowMinute, deltas: []MCPToolBucketDelta{
		{Name: "srv", Tool: "search", Kind: MCPKindServer, Calls: 3, LatencyMsSum: 30, LastCallAt: overflowMinute},
	}})

	if len(flusher.mcpQueue) != MaxPendingStatsBatches || flusher.mcpCoalesced != 1 {
		t.Fatalf(
			"mcp queue = %d batches after %d folds, want %d batches after exactly 1 fold",
			len(flusher.mcpQueue), flusher.mcpCoalesced, MaxPendingStatsBatches,
		)
	}
	if len(flusher.mcpToolQueue) != MaxPendingStatsBatches || flusher.mcpToolCoalesced != 1 {
		t.Fatalf(
			"mcp tool queue = %d batches after %d folds, want %d batches after exactly 1 fold",
			len(flusher.mcpToolQueue), flusher.mcpToolCoalesced, MaxPendingStatsBatches,
		)
	}

	folded := mcpQueueDelta(flusher.mcpQueue[0].deltas, "srv")
	if folded == nil {
		t.Fatal("folded mcp batch lost the srv delta")
	}
	if flusher.mcpQueue[0].minute != 60 {
		t.Errorf("folded mcp batch minute = %d, want the OLDEST boundary 60",
			flusher.mcpQueue[0].minute)
	}
	if folded.Calls != 2 || folded.LatencyMsSum != 20 {
		t.Errorf("folded srv delta = %+v, want calls=2 (1+1) latency=20 (10+10)", folded)
	}
	if folded.LastCallAt != 120 {
		t.Errorf(
			"folded srv last_call_at = %d, want 120 (max of the folded pair 60/120, not the first batch's 60)",
			folded.LastCallAt,
		)
	}
	// The neighbor batch is untouched: its own minute and single call remain.
	if flusher.mcpQueue[1].minute != 180 {
		t.Errorf("neighbor mcp batch minute = %d, want 180", flusher.mcpQueue[1].minute)
	}
	if neighbor := mcpQueueDelta(flusher.mcpQueue[1].deltas, "srv"); neighbor == nil || neighbor.Calls != 1 {
		t.Errorf("neighbor mcp batch srv delta = %+v, want calls=1", neighbor)
	}
	// The overflow batch itself sits at the tail, unmerged.
	tail := flusher.mcpQueue[len(flusher.mcpQueue)-1]
	if tail.minute != overflowMinute {
		t.Errorf("tail mcp batch minute = %d, want %d", tail.minute, overflowMinute)
	}
	if tailDelta := mcpQueueDelta(tail.deltas, "srv"); tailDelta == nil || tailDelta.Calls != 7 {
		t.Errorf("tail mcp batch srv delta = %+v, want calls=7", tailDelta)
	}

	// Tool merge: search folds 1+1 with the max last_call_at; fetch (absent
	// from the overflow batch) stays at its two folded windows' total and
	// never receives search's contribution.
	if flusher.mcpToolQueue[0].minute != 60 {
		t.Errorf("folded tool batch minute = %d, want the OLDEST boundary 60",
			flusher.mcpToolQueue[0].minute)
	}
	if search := mcpToolQueueDelta(flusher.mcpToolQueue[0].deltas, "srv", "search"); search == nil ||
		search.Calls != 2 || search.LatencyMsSum != 20 || search.LastCallAt != 120 {
		t.Errorf("folded search delta = %+v, want calls=2 latency=20 last_call_at=120 (max)", search)
	}
	if fetch := mcpToolQueueDelta(flusher.mcpToolQueue[0].deltas, "srv", "fetch"); fetch == nil ||
		fetch.Calls != 10 || fetch.LatencyMsSum != 100 || fetch.LastCallAt != 120 {
		t.Errorf("folded fetch delta = %+v, want calls=10 latency=100 last_call_at=120 (key-isolated)", fetch)
	}
}

func TestMergeStatsDeltasPreservesAllCounters(t *testing.T) {
	key := Key{Provider: "p", Model: "m"}
	destination := map[Key]Counters{key: {
		Requests: 1, Failovers: 2, RateLimited429: 3, Failures: 4,
		Input: 5, Output: 6, CacheCreation: 7, CacheRead: 8,
		TokenRequests: 9, LastRequestAt: 200, LatencySum: 10, TTFTSum: 11,
	}}
	MergeDeltas(destination, map[Key]Counters{key: {
		Requests: 11, Failovers: 12, RateLimited429: 13, Failures: 14,
		Input: 15, Output: 16, CacheCreation: 17, CacheRead: 18,
		TokenRequests: 19, LastRequestAt: 100, LatencySum: 20, TTFTSum: 21,
	}})
	want := Counters{
		Requests: 12, Failovers: 14, RateLimited429: 16, Failures: 18,
		Input: 20, Output: 22, CacheCreation: 24, CacheRead: 26,
		TokenRequests: 28, LastRequestAt: 200, LatencySum: 30, TTFTSum: 32,
	}
	if got := destination[key]; got != want {
		t.Errorf("merged counters = %+v, want %+v", got, want)
	}
}

type failOnceStatsSink struct {
	*Store
	failNext bool
}

func (sink *failOnceStatsSink) FlushContext(
	ctx context.Context,
	minute int64,
	deltas map[Key]Counters,
) error {
	if sink.failNext {
		sink.failNext = false
		return fmt.Errorf("injected stats flush failure")
	}
	return sink.Store.FlushContext(ctx, minute, deltas)
}

func sumRequests(t *testing.T, ss *Store) uint64 {
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
	cur := map[AgentKey]AgentCounters{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5, Input: 10, Output: 2, CacheRead: 8},
		{Agent: "b", Provider: "z", Model: "m"}: {Requests: 3, Input: 0, Output: 0},
		// Cache-only movement (no requests/input/output delta) must still flush.
		{Agent: "c", Provider: "z", Model: "m"}: {Requests: 1, CacheCreation: 4, CacheRead: 9},
	}
	prev := map[AgentKey]AgentCounters{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 2, Input: 10, Output: 0, CacheRead: 3}, // reqs +3, output +2, cache_read +5; input unchanged
		{Agent: "c", Provider: "z", Model: "m"}: {Requests: 1, CacheRead: 9},
	}
	d := DiffAgent(cur, prev)
	ad, ok := d[AgentKey{Agent: "a", Provider: "z", Model: "m"}]
	if !ok || ad.Requests != 3 || ad.Input != 0 || ad.Output != 2 || ad.CacheRead != 5 {
		t.Errorf("a delta = %+v want reqs=3 in=0 out=2 cr=5", ad)
	}
	bd, ok := d[AgentKey{Agent: "b", Provider: "z", Model: "m"}]
	if !ok || bd.Requests != 3 {
		t.Errorf("b delta = %+v want reqs=3 (new key)", bd)
	}
	cd, ok := d[AgentKey{Agent: "c", Provider: "z", Model: "m"}]
	if !ok || cd.CacheCreation != 4 || cd.CacheRead != 0 || cd.Requests != 0 {
		t.Errorf("c delta = %+v want cache-only delta cc=4", cd)
	}
	// A key whose counters only decreased (e.g. after reset) clamps to 0 and is
	// omitted when ALL fields are 0.
	d2 := DiffAgent(
		map[AgentKey]AgentCounters{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 1}},
		map[AgentKey]AgentCounters{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5}},
	)
	if _, present := d2[AgentKey{Agent: "a", Provider: "z", Model: "m"}]; present {
		t.Errorf("all-zero delta should be omitted, got %+v", d2)
	}
}

// TestMergeAgentDeltas accumulates every agent counter field when backlog
// batches coalesce.
func TestMergeAgentDeltas(t *testing.T) {
	key := AgentKey{Agent: "a", Provider: "z", Model: "m"}
	dst := map[AgentKey]AgentCounters{key: {Requests: 1, Input: 2, Output: 3, CacheCreation: 4, CacheRead: 5, LatencySum: 6, Failures: 7}}
	MergeAgentDeltas(dst, map[AgentKey]AgentCounters{key: {Requests: 10, Input: 20, Output: 30, CacheCreation: 40, CacheRead: 50, LatencySum: 60, Failures: 70}})
	if got := dst[key]; got.Requests != 11 || got.Input != 22 || got.Output != 33 ||
		got.CacheCreation != 44 || got.CacheRead != 55 || got.LatencySum != 66 || got.Failures != 77 {
		t.Errorf("merged agent delta = %+v", got)
	}
}

// TestFlusherResetClearsEverything covers the in-package reset path: durable
// rows, runtime baselines, and pending queues all clear atomically.
func TestFlusherResetClearsEverything(t *testing.T) {
	ss := newTestStore(t, 0)
	m := obscounters.NewMetricsStore()
	tc := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	f := NewFlusher(ss, m, tc, agents, nil, nil)

	m.Inc("a", "x", obscounters.EvRequests)
	tc.Commit(obscounters.PMKey{Provider: "a", Model: "x"}, obscounters.TokenUsage{Input: 3})
	agents.IncRequests("codex", "a", "x")
	if !f.Flush(time.Now()) {
		t.Fatal("flush with activity must write")
	}
	if err := f.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := sumRequests(t, ss); got != 0 {
		t.Errorf("durable rows after Reset = %d, want 0", got)
	}
	if len(m.Snapshot()) != 0 || len(tc.Snapshot()) != 0 || len(agents.Snapshot()) != 0 {
		t.Error("runtime counters must be empty after Reset")
	}
	providerPending, agentPending := f.PendingCounts()
	if providerPending != 0 || agentPending != 0 {
		t.Errorf("pending after Reset = %d/%d, want 0/0", providerPending, agentPending)
	}
}

// TestPendingCountsContext covers the context-aware pending probe.
func TestPendingCountsContext(t *testing.T) {
	ss := newTestStore(t, 0)
	m := obscounters.NewMetricsStore()
	f := NewFlusher(ss, m, obscounters.NewTokenCounter(), obscounters.NewAgentCounter(), nil, nil)
	m.Inc("a", "x", obscounters.EvRequests)
	f.Flush(time.Now())

	provider, agent, locked := f.PendingCountsContext(context.Background())
	if !locked {
		t.Fatal("PendingCountsContext must acquire the lock")
	}
	if provider != 0 || agent != 0 {
		t.Errorf("pending = %d/%d, want 0/0 after successful flush", provider, agent)
	}

	// A cancelled context must report locked=false rather than blocking.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, locked = f.PendingCountsContext(ctx)
	_ = locked // may still win the TryLock race; only assert no deadlock
}

// failOnceSink wraps a Store and fails the next FlushContext once — the
// failure-injection seam for the shutdown/retry tests (test-side only; the
// production package carries no test hooks).
type failOnceSink struct {
	*Store
	failNext bool
}

func (sink *failOnceSink) FlushContext(
	ctx context.Context,
	minute int64,
	deltas map[Key]Counters,
) error {
	if sink.failNext {
		sink.failNext = false
		return fmt.Errorf("injected stats flush failure")
	}
	return sink.Store.FlushContext(ctx, minute, deltas)
}

// TestFlushForShutdownDrainsPending covers the shutdown retry loop: a sink
// that fails once must still be drained within the retry window.
func TestFlushForShutdownDrainsPending(t *testing.T) {
	ss := newTestStore(t, 0)
	sink := &failOnceSink{Store: ss, failNext: true}
	m := obscounters.NewMetricsStore()
	f := NewFlusher(sink, m, obscounters.NewTokenCounter(), obscounters.NewAgentCounter(), nil, nil)
	m.Inc("a", "x", obscounters.EvRequests)

	f.FlushForShutdown(StatsShutdownFlushTimeout)
	providerPending, _ := f.PendingCounts()
	if providerPending != 0 {
		t.Errorf("provider pending after shutdown flush = %d, want 0", providerPending)
	}
	if got := sumRequests(t, ss); got != 1 {
		t.Errorf("durable requests = %d, want 1", got)
	}
}

// TestFlusherMCPPath verifies the flusher diff + persist path for MCP counters,
// including the reset case where a counter's current value is lower than the
// previous snapshot.
func TestFlusherMCPPath(t *testing.T) {
	ss := newTestStore(t, 0)
	mcp := obscounters.NewMCPStats()
	kind := func(name string) (MCPKind, bool) {
		switch name {
		case "srv":
			return MCPKindServer, true
		case "rt":
			return MCPKindRoute, true
		default:
			return "", false
		}
	}
	f := NewFlusher(
		ss,
		obscounters.NewMetricsStore(),
		obscounters.NewTokenCounter(),
		obscounters.NewAgentCounter(),
		nil, nil,
		WithMCPStats(mcp, kind),
	)

	mcp.Record("srv", 200, 100)
	mcp.Record("srv", 500, 50)
	mcp.Record("rt", 200, 30)
	f.Flush(time.Unix(60, 0))

	rows, err := ss.QueryMCPBuckets(0, 120, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("first flush rows = %d, want 2: %+v", len(rows), rows)
	}
	byName := map[string]MCPBucketRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if s := byName["srv"]; s.Calls != 2 || s.Errors != 1 || s.LatencyMsSum != 150 {
		t.Errorf("srv first flush = %+v", s)
	}
	if r := byName["rt"]; r.Calls != 1 || r.Kind != "route" {
		t.Errorf("rt first flush = %+v", r)
	}

	// Second minute: more calls.
	mcp.Record("srv", 200, 70)
	f.Flush(time.Unix(120, 0))

	rows, err = ss.QueryMCPBuckets(0, 180, "minute")
	if err != nil {
		t.Fatal(err)
	}
	var srvTotal uint64
	for _, r := range rows {
		if r.Name == "srv" {
			srvTotal += r.Calls
		}
	}
	if srvTotal != 3 {
		t.Errorf("srv cumulative calls = %d, want 3", srvTotal)
	}

	// Simulate a counter reset: the next snapshot is lower than the previous
	// baseline. The flusher must treat the current value as the delta.
	mcp.Reset()
	mcp.Record("srv", 200, 40)
	mcp.Record("srv", 200, 60)
	f.Flush(time.Unix(180, 0))

	rows, err = ss.QueryMCPBuckets(180, 240, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("reset flush rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Bucket != 180 || rows[0].Calls != 2 || rows[0].LatencyMsSum != 100 {
		t.Errorf("reset flush row = %+v", rows[0])
	}
}

// TestFlusherMCPToolPath verifies the flusher diff + persist path for the
// per-tool MCP counters, including the reset case where a counter's current
// value is lower than the previous snapshot.
func TestFlusherMCPToolPath(t *testing.T) {
	ss := newTestStore(t, 0)
	mcp := obscounters.NewMCPStats()
	kind := func(name string) (MCPKind, bool) {
		switch name {
		case "srv":
			return MCPKindServer, true
		case "rt":
			return MCPKindRoute, true
		default:
			return "", false
		}
	}
	f := NewFlusher(
		ss,
		obscounters.NewMetricsStore(),
		obscounters.NewTokenCounter(),
		obscounters.NewAgentCounter(),
		nil, nil,
		WithMCPStats(mcp, kind),
	)

	mcp.RecordTool("srv", "search", 200, 100)
	mcp.RecordTool("srv", "search", 500, 50)
	mcp.RecordTool("srv", "fetch", 200, 30)
	mcp.RecordTool("rt", "lookup", 200, 10)
	mcp.RecordTool("ghost", "x", 200, 10) // unresolvable name: skipped
	f.Flush(time.Unix(60, 0))

	rows, err := ss.QueryMCPToolBuckets(0, 120, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("first flush rows = %d, want 3: %+v", len(rows), rows)
	}
	byTool := map[[2]string]MCPToolBucketRow{}
	for _, r := range rows {
		byTool[[2]string{r.Name, r.Tool}] = r
	}
	if s := byTool[[2]string{"srv", "search"}]; s.Calls != 2 || s.Errors != 1 || s.LatencyMsSum != 150 || s.Kind != "server" {
		t.Errorf("srv/search first flush = %+v", s)
	}
	if fetch := byTool[[2]string{"srv", "fetch"}]; fetch.Calls != 1 || fetch.Errors != 0 {
		t.Errorf("srv/fetch first flush = %+v", fetch)
	}
	if lookup := byTool[[2]string{"rt", "lookup"}]; lookup.Calls != 1 || lookup.Kind != "route" {
		t.Errorf("rt/lookup first flush = %+v", lookup)
	}

	// Second minute: more calls on one tool.
	mcp.RecordTool("srv", "search", 200, 70)
	f.Flush(time.Unix(120, 0))

	rows, err = ss.QueryMCPToolBuckets(0, 180, "minute")
	if err != nil {
		t.Fatal(err)
	}
	var searchTotal uint64
	for _, r := range rows {
		if r.Name == "srv" && r.Tool == "search" {
			searchTotal += r.Calls
		}
	}
	if searchTotal != 3 {
		t.Errorf("srv/search cumulative calls = %d, want 3", searchTotal)
	}

	// Counter reset: the next snapshot is lower than the previous baseline.
	// The flusher must treat the current value as the delta.
	mcp.Reset()
	mcp.RecordTool("srv", "search", 200, 40)
	mcp.RecordTool("srv", "search", 200, 60)
	f.Flush(time.Unix(180, 0))

	rows, err = ss.QueryMCPToolBuckets(180, 240, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("reset flush rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Bucket != 180 || rows[0].Calls != 2 || rows[0].LatencyMsSum != 100 {
		t.Errorf("reset flush row = %+v", rows[0])
	}

	// Reset must drain the tool channel too.
	if err := f.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	rows, err = ss.QueryMCPToolBuckets(0, 240, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("durable tool rows after Reset = %d, want 0", len(rows))
	}
	if len(mcp.RawToolSnapshot()) != 0 {
		t.Error("runtime tool counters must be empty after Reset")
	}
}
