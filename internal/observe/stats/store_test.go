package stats

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, retention time.Duration) *Store {
	t.Helper()
	store, err := Open(Options{
		Path:      filepath.Join(t.TempDir(), "state", "stats.db"),
		Retention: retention,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func TestFlushUpsertAndLoadCumulative(t *testing.T) {
	store := newTestStore(t, 0)
	key := Key{Provider: "zhipu", Model: "glm-5"}
	first := Counters{
		Requests:       2,
		Failovers:      1,
		RateLimited429: 1,
		Failures:       1,
		Input:          100,
		Output:         20,
		CacheCreation:  3,
		CacheRead:      4,
		TokenRequests:  2,
		LastRequestAt:  200,
		LatencySum:     1001,
		TTFTSum:        201,
	}
	second := Counters{
		Requests:       3,
		Failovers:      2,
		RateLimited429: 2,
		Failures:       2,
		Input:          200,
		Output:         30,
		CacheCreation:  5,
		CacheRead:      6,
		TokenRequests:  3,
		LastRequestAt:  100,
		LatencySum:     1499,
		TTFTSum:        299,
	}
	if err := store.Flush(60, map[Key]Counters{key: first}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(60, map[Key]Counters{key: second}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(120, map[Key]Counters{key: {Requests: 1, LastRequestAt: 300}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(180, nil); err != nil {
		t.Fatalf("empty Flush: %v", err)
	}

	raw, err := store.QueryRange(0, 180, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("raw rows = %d, want 2: %+v", len(raw), raw)
	}
	got := raw[0]
	if got.Provider != key.Provider || got.Model != key.Model || got.Minute != 60 {
		t.Errorf("first key/minute = %+v, want %s/%s at 60", got, key.Provider, key.Model)
	}
	if got.Requests != 5 || got.Failovers != 3 || got.RateLimited429 != 3 ||
		got.Failures != 3 || got.Input != 300 || got.Output != 50 ||
		got.CacheCreation != 8 || got.CacheRead != 10 || got.TokenRequests != 5 ||
		got.LastRequestAt != 200 || got.LatencySum != 2500 || got.TTFTSum != 500 {
		t.Errorf("upserted counters = %+v", got)
	}
	if got.AvgLatencyMs != 500 || got.AvgTtftMs != 100 {
		t.Errorf("averages = latency %.1f, ttft %.1f; want 500.0, 100.0", got.AvgLatencyMs, got.AvgTtftMs)
	}

	cumulative, err := store.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	total := cumulative[key]
	if total.Requests != 6 || total.Failovers != 3 || total.RateLimited429 != 3 ||
		total.Failures != 3 || total.Input != 300 || total.Output != 50 ||
		total.CacheCreation != 8 || total.CacheRead != 10 || total.TokenRequests != 5 ||
		total.LastRequestAt != 300 || total.LatencySum != 2500 || total.TTFTSum != 500 {
		t.Errorf("cumulative = %+v", total)
	}
}

func TestPruneCoversBothTablesAndZeroRetentionIsNoop(t *testing.T) {
	now := time.Now()
	old := now.Add(-2*time.Hour).Unix() / 60 * 60
	recent := now.Unix() / 60 * 60
	store := newTestStore(t, time.Hour)
	if err := store.Flush(old, map[Key]Counters{{Provider: "old", Model: "m"}: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(recent, map[Key]Counters{{Provider: "new", Model: "m"}: {Requests: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(old, map[AgentKey]AgentCounters{{Agent: "old", Provider: "p", Model: "m"}: {Requests: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(recent, map[AgentKey]AgentCounters{{Agent: "new", Provider: "p", Model: "m"}: {Requests: 4}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	statsRows, err := store.QueryRange(0, recent+60, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(statsRows) != 1 || statsRows[0].Provider != "new" {
		t.Errorf("pruned stats = %+v", statsRows)
	}
	agentRows, err := store.QueryAgents(0, recent+60, "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentRows) != 1 || agentRows[0].Agent != "new" {
		t.Errorf("pruned agents = %+v", agentRows)
	}

	keep := newTestStore(t, 0)
	if err := keep.Flush(old, map[Key]Counters{{Provider: "old", Model: "m"}: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := keep.FlushAgents(old, map[AgentKey]AgentCounters{{Agent: "old", Provider: "p", Model: "m"}: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := keep.Prune(now); err != nil {
		t.Fatal(err)
	}
	keptStats, _ := keep.QueryRange(0, recent+60, "", "", 60)
	keptAgents, _ := keep.QueryAgents(0, recent+60, "", "", "", 60)
	if len(keptStats) != 1 || len(keptAgents) != 1 {
		t.Errorf("zero-retention prune removed rows: stats=%d agents=%d", len(keptStats), len(keptAgents))
	}
}

func TestResetCoversBothTables(t *testing.T) {
	store := newTestStore(t, 0)
	if err := store.Flush(60, map[Key]Counters{{Provider: "p", Model: "m"}: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(60, map[AgentKey]AgentCounters{{Agent: "a", Provider: "p", Model: "m"}: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	statsRows, _ := store.QueryRange(0, 120, "", "", 60)
	agentRows, _ := store.QueryAgents(0, 120, "", "", "", 60)
	if len(statsRows) != 0 || len(agentRows) != 0 {
		t.Errorf("after Reset: stats=%+v agents=%+v", statsRows, agentRows)
	}
}

func TestOpenAndClosedStoreErrors(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Path: filepath.Join(parentFile, "stats.db")}); err == nil ||
		!strings.Contains(err.Error(), "stats db dir") {
		t.Fatalf("Open beneath file error = %v", err)
	}

	store := newTestStore(t, time.Hour)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(60, map[Key]Counters{{Provider: "p"}: {Requests: 1}}); err == nil {
		t.Error("Flush on closed store succeeded")
	}
	if err := store.FlushAgents(60, map[AgentKey]AgentCounters{{Agent: "a"}: {Requests: 1}}); err == nil {
		t.Error("FlushAgents on closed store succeeded")
	}
	if _, err := store.LoadCumulative(); err == nil {
		t.Error("LoadCumulative on closed store succeeded")
	}
	if _, err := store.QueryRange(0, 60, "", "", 60); err == nil {
		t.Error("QueryRange on closed store succeeded")
	}
	if _, err := store.QueryAnalytics(0, 60, "", "", "day"); err == nil {
		t.Error("QueryAnalytics on closed store succeeded")
	}
	if _, err := store.QueryAgents(0, 60, "", "", "", 60); err == nil {
		t.Error("QueryAgents on closed store succeeded")
	}
	if err := store.Prune(time.Now()); err == nil {
		t.Error("Prune on closed store succeeded")
	}
	if err := store.Reset(); err == nil {
		t.Error("Reset on closed store succeeded")
	}
	if _, err := store.ImportLegacyTokens("ignored"); err == nil {
		t.Error("ImportLegacyTokens on closed store succeeded")
	}
	if err := (*Store)(nil).Close(); err != nil {
		t.Errorf("nil Close = %v", err)
	}
}

func TestFlushContextBoundsBusyWriterWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	writer, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	blocked, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocked.Close() })

	transaction, err := writer.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(
		`INSERT INTO minute_buckets (provider, model, minute) VALUES (?, ?, ?)`,
		"lock-holder",
		"model",
		60,
	); err != nil {
		t.Fatalf("acquire writer lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = blocked.FlushContext(ctx, 120, map[Key]Counters{
		{Provider: "blocked", Model: "model"}: {Requests: 1},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("FlushContext succeeded while another transaction held the writer lock")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Errorf("context state = %v, want deadline exceeded; flush error=%v", ctx.Err(), err)
	}
	// modernc's SQLite busy handler is not interrupted mid-wait by context
	// cancellation. The Store therefore uses a short, fixed busy timeout so one
	// locked write remains bounded well below the shutdown retry window.
	if elapsed > time.Second {
		t.Errorf("FlushContext elapsed = %v, want bounded wait below 1s", elapsed)
	}
}
