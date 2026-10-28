package stats

import (
	"context"
	"errors"
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

// gatedSink holds provider-bucket flushes back so a flush cycle retains its
// batch in the flusher's pending backlog — the state the live rename must
// re-key (a bare-key delta left pending would resurrect bare-name rows after
// the durable rename).
type gatedSink struct {
	*Store
	fail bool
}

func (g *gatedSink) FlushContext(ctx context.Context, minute int64, deltas map[Key]Counters) error {
	if g.fail {
		return errors.New("sink gated")
	}
	return g.Store.FlushContext(ctx, minute, deltas)
}

// TestRenameProviderKeyLive pins the reload-time contract: after converting a
// bare-key provider to a pool WITHOUT a restart, durable buckets, cumulative
// in-memory counters, and the flusher's diff baseline all land on the virtual
// key — and subsequent flushes neither resurrect the bare key nor double-count
// the adopted history.
func TestRenameProviderKeyLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	const minute = int64(1791900000)

	seed, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	// Bare-key era the process inherited on disk.
	if err := seed.Flush(minute, map[Key]Counters{
		{Provider: "p", Model: "m"}: {Requests: 800, Input: 400, Output: 100, TokenRequests: 800, LastRequestAt: minute + 30},
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed.FlushAgents(minute, map[AgentKey]AgentCounters{
		{Agent: "pi", Provider: "p", Model: "m"}: {Requests: 800, Input: 400},
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	// Production wiring: Bootstrap restores the cumulative baseline into the
	// counters and seeds the flusher's diff baseline from the same rows.
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	result := Bootstrap(path, 0, "", metrics, tokens, agents, nil, "")
	if result.Store == nil || result.Flusher == nil {
		t.Fatal("bootstrap failed")
	}
	store, flusher := result.Store, result.Flusher
	defer store.Close()

	// New-key era: traffic under "p#acc" after the reload swapped keys, with
	// one flush persisting it as its own minute.
	metrics.Inc("p#acc", "m", obscounters.EvRequests)
	metrics.AddLatency("p#acc", "m", 10, 5)
	tokens.Commit(obscounters.PMKey{Provider: "p#acc", Model: "m"}, obscounters.TokenUsage{Input: 7, Output: 4})
	agents.IncRequests("pi", "p#acc", "m")
	agents.AddTokens("pi", "p#acc", "m", obscounters.TokenUsage{Input: 7})
	if !flusher.Flush(time.Unix(minute+180, 0)) {
		t.Fatal("new-era flush reported no write")
	}

	// The conversion event: runtime rename, no restart.
	moved, err := flusher.RenameProviderKeyLive(store, "p", "p#acc")
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d rows, want 2 (one minute bucket + one agent bucket)", moved)
	}

	// Post-rename traffic and one more flush: the adopted history must not be
	// re-flushed (double-count) and the bare key must stay dead.
	metrics.Inc("p#acc", "m", obscounters.EvRequests)
	tokens.Commit(obscounters.PMKey{Provider: "p#acc", Model: "m"}, obscounters.TokenUsage{Input: 1})
	if !flusher.Flush(time.Unix(minute+300, 0)) {
		t.Fatal("post-rename flush reported no write")
	}

	// Durable view: everything under "p#acc", nothing under "p".
	rows, err := store.QueryRange(minute, minute+300, "p#acc", "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	var req, in, out uint64
	for _, r := range rows {
		req += r.Requests
		in += r.Input
		out += r.Output
	}
	if req != 802 || in != 408 || out != 104 {
		t.Fatalf("durable totals = req %d / in %d / out %d, want 802 / 408 / 104 (no double count)", req, in, out)
	}
	if bare, err := store.QueryRange(minute, minute+300, "p", "m", 0); err != nil || len(bare) != 0 {
		t.Fatalf("bare-key rows resurrected after the live rename: %+v (err=%v)", bare, err)
	}
	agentRows, err := store.QueryAgents(minute-60, minute+300, "pi", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var agentReq, agentIn uint64
	for _, r := range agentRows {
		if r.Provider != "p#acc" {
			t.Fatalf("agent row still keyed %q: %+v", r.Provider, r)
		}
		agentReq += r.Requests
		agentIn += r.Input
	}
	if agentReq != 801 || agentIn != 407 {
		t.Fatalf("agent totals = req %d / in %d, want 801 / 407", agentReq, agentIn)
	}

	// In-memory cumulative view agrees with the durable one — the guarantee
	// Bootstrap restores at startup must now hold WITHOUT a restart.
	if got := metrics.Snapshot()[obscounters.PMKey{Provider: "p#acc", Model: "m"}]; got.Requests != 802 {
		t.Fatalf("cumulative metrics = %+v, want 802 requests under p#acc", got)
	}
	if _, bare := metrics.Snapshot()[obscounters.PMKey{Provider: "p", Model: "m"}]; bare {
		t.Error("bare cumulative metrics entry survived the live rename")
	}
	tu := tokens.Snapshot()[obscounters.PMKey{Provider: "p#acc", Model: "m"}]
	if tu.Input != 408 || tu.Output != 104 || tu.Requests != 802 {
		t.Fatalf("cumulative tokens = %+v, want input=408 output=104 requests=802", tu)
	}
	cum, err := store.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	if _, bare := cum[Key{Provider: "p", Model: "m"}]; bare {
		t.Error("bare key still owns durable cumulative history")
	}
	dur := cum[Key{Provider: "p#acc", Model: "m"}]
	if dur.Requests != 802 || dur.Input != 408 || dur.Output != 104 || dur.TokenRequests != 802 {
		t.Fatalf("durable cumulative = %+v, want requests=802 input=408 output=104 tokens=802", dur)
	}

	// Idempotent: a second pass with nothing left under the bare key is a
	// no-op that still leaves both views intact.
	if moved, err := flusher.RenameProviderKeyLive(store, "p", "p#acc"); err != nil || moved != 0 {
		t.Fatalf("second live rename = (%d, %v), want (0, nil)", moved, err)
	}
	if got := metrics.Snapshot()[obscounters.PMKey{Provider: "p#acc", Model: "m"}].Requests; got != 802 {
		t.Fatalf("second pass changed the cumulative view: %d requests, want 802", got)
	}
}

// TestRenameProviderKeyLive_RewritesPendingBacklog covers the retained-batch
// path: a bare-key delta stuck in the pending backlog (Store outage at diff
// time) must flush under the virtual key after the live rename instead of
// resurrecting bare-name rows.
func TestRenameProviderKeyLive_RewritesPendingBacklog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const minute = int64(1791900000)
	gate := &gatedSink{Store: store, fail: true}
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	flusher := NewFlusher(gate, metrics, tokens, nil, nil, nil)

	// Bare-key traffic whose flush is gated → the delta sits in the backlog.
	metrics.Inc("p", "m", obscounters.EvRequests)
	tokens.Commit(obscounters.PMKey{Provider: "p", Model: "m"}, obscounters.TokenUsage{Input: 10})
	flusher.Flush(time.Unix(minute, 0))
	if prov, _ := flusher.PendingCounts(); prov != 1 {
		t.Fatalf("gated flush retained %d batches, want 1", prov)
	}

	// Live rename with the real store, then let the backlog drain.
	if _, err := flusher.RenameProviderKeyLive(store, "p", "p#acc"); err != nil {
		t.Fatal(err)
	}
	gate.fail = false
	if !flusher.Flush(time.Unix(minute+120, 0)) {
		t.Fatal("backlog drain reported no write")
	}
	if prov, _ := flusher.PendingCounts(); prov != 0 {
		t.Fatalf("backlog did not drain: %d batches", prov)
	}

	rows, err := store.QueryRange(minute-60, minute+120, "p#acc", "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 || rows[0].Input != 10 {
		t.Fatalf("backlog flushed = %+v, want one p#acc row with 1 request / 10 input", rows)
	}
	if bare, err := store.QueryRange(minute-60, minute+120, "p", "m", 0); err != nil || len(bare) != 0 {
		t.Fatalf("pending bare delta resurrected bare-name rows: %+v (err=%v)", bare, err)
	}
	if _, bare := metrics.Snapshot()[obscounters.PMKey{Provider: "p", Model: "m"}]; bare {
		t.Error("bare cumulative entry survived the live rename")
	}
}
