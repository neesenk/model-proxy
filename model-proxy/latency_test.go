package main

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMetricsAddLatency: addLatency accumulates into the sums the flusher diffs,
// snapshot returns them, and seed round-trips them (the boot-restore path).
func TestMetricsAddLatency(t *testing.T) {
	m := newMetricsStore()
	m.inc("z", "glm", evRequests)
	m.addLatency("z", "glm", 100, 20)
	m.addLatency("z", "glm", 50, 10)
	snap := m.snapshot()[pmKey{Provider: "z", Model: "glm"}]
	if snap.Requests != 1 {
		t.Errorf("requests=%d want 1", snap.Requests)
	}
	if snap.LatencySum != 150 || snap.TTFTSum != 30 {
		t.Errorf("latency=%d ttft=%d want 150/30", snap.LatencySum, snap.TTFTSum)
	}
	// aggregateByProvider rolls the sums up across models.
	agg := m.aggregateByProvider()["z"]
	if agg.LatencySum != 150 || agg.TTFTSum != 30 {
		t.Errorf("aggregate latency=%d ttft=%d want 150/30", agg.LatencySum, agg.TTFTSum)
	}
	// seed round-trips the sums (boot restore).
	m2 := newMetricsStore()
	m2.seed(pmKey{Provider: "z", Model: "glm"}, snap)
	got := m2.snapshot()[pmKey{Provider: "z", Model: "glm"}]
	if got.LatencySum != 150 || got.TTFTSum != 30 {
		t.Errorf("seed round-trip latency=%d ttft=%d want 150/30", got.LatencySum, got.TTFTSum)
	}
}

// TestStatsLatencyFlushQuery: latency sums are persisted additively and surface
// as exact averages at query time (SUM / requests, rounded to 0.1ms), in both the
// raw 1-minute path and the aggregated (bucket>60) path.
func TestStatsLatencyFlushQuery(t *testing.T) {
	ss := newTestStatsStore(t)
	minute := time.Now().Unix() / 60 * 60
	if err := ss.flushDeltas(minute, map[pmKey]statsCounters{
		{Provider: "z", Model: "glm"}: {Requests: 4, LatencySum: 1000, TTFTSum: 200},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ss.queryRange(minute, minute, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows=%d want 1", len(got))
	}
	b := got[0]
	// Exact stored sums + derived averages (1000/4=250, 200/4=50).
	if b.Requests != 4 || b.LatencySum != 1000 || b.TTFTSum != 200 {
		t.Errorf("stored = reqs=%d lat=%d ttft=%d want 4/1000/200", b.Requests, b.LatencySum, b.TTFTSum)
	}
	if b.AvgLatencyMs != 250 || b.AvgTtftMs != 50 {
		t.Errorf("avg = latency=%v ttft=%v want 250/50", b.AvgLatencyMs, b.AvgTtftMs)
	}

	// Aggregated path (bucket=600) preserves the same sums + averages.
	agg, err := ss.queryRange(minute, minute, "", "", 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(agg) != 1 || agg[0].LatencySum != 1000 || agg[0].AvgLatencyMs != 250 || agg[0].AvgTtftMs != 50 {
		t.Errorf("aggregated = %+v want latency 1000 / avg 250 / ttft avg 50", agg[0])
	}

	// Two flushes into the same minute add (additive counters, not overwrite).
	if err := ss.flushDeltas(minute, map[pmKey]statsCounters{
		{Provider: "z", Model: "glm"}: {Requests: 1, LatencySum: 250, TTFTSum: 50},
	}); err != nil {
		t.Fatal(err)
	}
	got2, _ := ss.queryRange(minute, minute, "", "", 60)
	if got2[0].Requests != 5 || got2[0].LatencySum != 1250 || got2[0].TTFTSum != 250 {
		t.Errorf("second flush not additive: %+v want 5/1250/250", got2[0])
	}
}

// TestStatsLatencyMigration: a DB created with the pre-latency schema (no
// latency_ms_sum / ttft_ms_sum columns) is migrated additively on open, so an
// existing user's accumulated history is preserved and a latency flush works.
func TestStatsLatencyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	dbPath := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open(statsDriver, dbPath) // raw open, no migration
	if err != nil {
		t.Fatal(err)
	}
	// Old schema = the FULL schema as it existed the day before latency shipped
	// (every column except latency_ms_sum / ttft_ms_sum). A real user's DB looks
	// exactly like this; ensureColumns must add the two missing columns.
	if _, err := db.Exec(`CREATE TABLE minute_buckets (
		provider    TEXT NOT NULL,
		model       TEXT NOT NULL,
		minute      INTEGER NOT NULL,
		requests    INTEGER NOT NULL DEFAULT 0,
		failovers   INTEGER NOT NULL DEFAULT 0,
		rate_limited_429 INTEGER NOT NULL DEFAULT 0,
		failures    INTEGER NOT NULL DEFAULT 0,
		input       INTEGER NOT NULL DEFAULT 0,
		output      INTEGER NOT NULL DEFAULT 0,
		cache_creation INTEGER NOT NULL DEFAULT 0,
		cache_read  INTEGER NOT NULL DEFAULT 0,
		token_requests INTEGER NOT NULL DEFAULT 0,
		last_request_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (provider, model, minute)
	); CREATE INDEX IF NOT EXISTS idx_minute ON minute_buckets(minute);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO minute_buckets (provider, model, minute, requests) VALUES ('z','glm',0,3)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Re-open through openStatsStore -> migrate -> ensureColumns adds the columns.
	ss, err := openStatsStore(path, 0)
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	defer ss.Close()
	cols, err := ss.columnSet("minute_buckets")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["latency_ms_sum"] || !cols["ttft_ms_sum"] {
		t.Errorf("migration did not add latency columns; set=%v", cols)
	}
	// Pre-existing row survived (history preserved)...
	got, err := ss.queryRange(0, 60, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Requests != 3 {
		t.Errorf("history not preserved after migration: %+v", got)
	}
	// ...and a latency flush now succeeds (the new columns accept writes).
	if err := ss.flushDeltas(0, map[pmKey]statsCounters{
		{Provider: "z", Model: "glm"}: {Requests: 1, LatencySum: 500, TTFTSum: 100},
	}); err != nil {
		t.Fatalf("flush after migration: %v", err)
	}
	got2, _ := ss.queryRange(0, 60, "", "", 60)
	if len(got2) != 1 || got2[0].Requests != 4 || got2[0].LatencySum != 500 || got2[0].AvgLatencyMs != 125 {
		t.Errorf("post-migration flush/avg wrong: %+v want 4/500/avg125", got2[0])
	}
}

// TestTimingResponseWriter: the first Write stamps firstByte; a writer that never
// receives bytes leaves hasFirstByte false; Flush delegates so SSE still flushes.
func TestTimingResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTimingResponseWriter(rec)
	if tw.hasFirstByte {
		t.Fatal("firstByte should be unset before any Write")
	}
	time.Sleep(2 * time.Millisecond)
	tw.Write([]byte("hello"))
	if !tw.hasFirstByte {
		t.Error("firstByte not stamped on first Write")
	}
	if tw.firstByte.IsZero() {
		t.Error("firstByte is zero after Write")
	}
	// Subsequent writes do not move firstByte.
	fb := tw.firstByte
	time.Sleep(time.Millisecond)
	tw.Write([]byte("world"))
	if tw.firstByte != fb {
		t.Error("firstByte moved on a later Write")
	}
	// Flusher delegation: the recorder implements http.Flusher (no-op), so this
	// must not panic and the underlying writer must still be usable.
	tw.Flush()
	if rec.Body.String() != "helloworld" {
		t.Errorf("body=%q want helloworld (Flush must not corrupt writes)", rec.Body.String())
	}
}

// TestForward_RecordsLatency: a served request records a non-zero total latency
// and TTFT against its (provider, model) in the metrics store on the hot path.
// The upstream deliberately delays before responding so latency is measurable.
func TestForward_RecordsLatency(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(8 * time.Millisecond) // ensure latency >= a few ms
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"z": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "z", Model: "glm-rt"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "z-key"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":[]}`))
	req = req.WithContext(context.Background())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	snap := p.metrics.snapshot()[pmKey{Provider: "z", Model: "glm-rt"}]
	if snap.Requests != 1 {
		t.Errorf("requests=%d want 1", snap.Requests)
	}
	if snap.LatencySum == 0 {
		t.Error("latency not recorded (LatencySum=0) for served request")
	}
	if snap.TTFTSum == 0 {
		t.Error("TTFT not recorded (TTFTSum=0); first-byte stamp should fire on body write")
	}
	// Ordering: the recorded latency is the UPSTREAM response time (send →
	// headers received, see proxy.go), while TTFT runs to the first byte
	// written to the CLIENT — necessarily after the headers arrive. Both share
	// the same `start`, so TTFT can never be SMALLER; the reverse (ttft >
	// latency) is normal whenever the body copy lands in a later millisecond
	// (ms truncation under load), not a bug.
	if snap.TTFTSum < snap.LatencySum {
		t.Errorf("ttft=%d < latency=%d (impossible: first client byte precedes upstream headers)", snap.TTFTSum, snap.LatencySum)
	}
}
