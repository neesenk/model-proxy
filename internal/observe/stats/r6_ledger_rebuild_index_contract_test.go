package stats_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/observe/stats"
	"model-proxy/internal/testsyn"
)

// TestRebuildTokenLedgerAgainstIndexerBuiltIndex pins the contract between
// Store.RebuildTokenLedger's aggregate SQL and the REAL request-log index
// schema — the schema requestlog.Indexer.migrate creates and insertBatch
// fills. The stats package's own ledger tests run against a hand-written
// stand-in table; a stand-in proves nothing about the real one: the original
// rebuild query named a `model` column the real table never had, the stand-in
// happened to declare it, the tests stayed green and the rebuild failed on
// every production start until the next commit fixed the query.
//
// Here the index is built by the real Indexer from real JSONL records
// (the Record encoding the logger writes), so it carries every column shape
// the rebuild touches: provider and upstream_model (the bucket key — not
// called_model), the status band, the usage projection, attempt_only legs,
// shadow records, MCP exchanges (kind "mcp") and a post-cutoff minute.
func TestRebuildTokenLedgerAgainstIndexerBuiltIndex(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	preCutoff := time.Date(2026, 10, 9, 4, 33, 0, 0, time.UTC).Unix()   // 2026-10-09T12:33+08:00
	postCutoff := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Unix() // 2026-11-20T10:00Z
	usageBody := func(input, output, cacheRead, cacheCreation int) string {
		return `{"id":"msg_1","usage":{"input_tokens":` + strconv.Itoa(input) +
			`,"output_tokens":` + strconv.Itoa(output) +
			`,"cache_read_input_tokens":` + strconv.Itoa(cacheRead) +
			`,"cache_creation_input_tokens":` + strconv.Itoa(cacheCreation) + `}}`
	}
	live := func(id string, ts string, provider, model, agent string, status int, body string) requestlog.Record {
		return requestlog.Record{
			Ts: ts, RequestID: id, Protocol: "anthropic", Method: "POST", Path: "/v1/messages",
			CalledModel: "alias-of-" + model, UpstreamModel: model, Exposed: "exposed", Provider: provider,
			Agent: agent, Attempt: 1, Status: status, RequestSize: 10, ResponseSize: int64(len(body)),
			RequestBody: `{}`, ResponseBody: body,
		}
	}
	leg := func(base requestlog.Record) requestlog.Record {
		base.AttemptOnly = true
		base.Attempt++
		base.Status = 502
		base.RequestBody, base.ResponseBody = "", ""
		return base
	}
	shadow := func(base requestlog.Record) requestlog.Record {
		base.Shadow = true
		base.RequestID = "shadow-" + base.RequestID
		return base
	}
	// mcp builds an MCP exchange record the way proxy_mcp.go logs it while
	// request_log.mcp_split is off: kind "mcp", the server as provider and no
	// upstream model.
	mcp := func(id, server, model, body string) requestlog.Record {
		return requestlog.Record{
			Ts: "2026-10-09T04:33:40Z", RequestID: id, Kind: "mcp", Protocol: "mcp", Method: "POST", Path: "/mcp/" + server,
			Tool: "read_file", Exposed: server, Provider: server, UpstreamModel: model,
			Agent: "claude-code", Attempt: 1, Status: 200, RequestSize: 10, ResponseSize: int64(len(body)),
			RequestBody: `{}`, ResponseBody: body,
		}
	}
	const preTS = "2026-10-09T04:33:10Z"
	const postTS = "2026-11-20T10:00:10Z"
	var records []requestlog.Record
	// Pre-cutoff minute, (deepseek, m): three 200s with usage and one 400.
	for i, ts := range []string{"2026-10-09T04:33:10Z", "2026-10-09T04:33:20Z", "2026-10-09T04:33:30Z"} {
		records = append(records, live("live-a"+strconv.Itoa(i), ts, "deepseek", "m", "claude-code", 200, usageBody(100, 10, 66, 5)))
	}
	records = append(records, live("live-a-400", preTS, "deepseek", "m", "claude-code", 400, `{"error":"bad request"}`))
	// Pre-cutoff minute, (zhipu, glm): two 2xx with usage under another agent.
	for i := 0; i < 2; i++ {
		records = append(records, live("live-b"+strconv.Itoa(i), preTS, "zhipu", "glm", "codex", 201, usageBody(12, 3, 0, 0)))
	}
	// Failover legs carry usage-bearing bodies and must stay out of the ledger.
	for i := 0; i < 2; i++ {
		records = append(records, leg(live("leg"+strconv.Itoa(i), preTS, "deepseek", "m", "claude-code", 200, usageBody(900, 90, 0, 0))))
	}
	// Shadow records under live buckets, both minutes: out of the ledger.
	records = append(records,
		shadow(live("sh-pre", preTS, "deepseek", "m", "claude-code", 200, usageBody(700, 70, 600, 50))),
		shadow(live("sh-post", postTS, "deepseek", "m", "claude-code", 200, usageBody(700, 70, 0, 0))),
	)
	// MCP exchanges share the index when request_log.mcp_split is off. They
	// are not LLM traffic: the first keys its own (server, "") group — a
	// pre-cutoff group with no bucket would be counted as skipped — and the
	// second's keys coincide with a live bucket, so only its kind keeps its
	// usage out of the ledger.
	records = append(records,
		mcp("mcp-1", "filesystem", "", `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`),
		mcp("mcp-2", "deepseek", "m", usageBody(999, 99, 0, 0)),
	)
	// Post-cutoff minute, (deepseek, m): two 200s and one 400.
	records = append(records,
		live("post-1", postTS, "deepseek", "m", "claude-code", 200, usageBody(50, 5, 0, 0)),
		live("post-2", "2026-11-20T10:00:20Z", "deepseek", "m", "claude-code", 200, usageBody(50, 5, 0, 0)),
		live("post-400", "2026-11-20T10:00:30Z", "deepseek", "m", "claude-code", 400, `{"error":"bad request"}`),
	)

	indexPath := indexWithRealIndexer(t, logDir, records)

	store, err := stats.Open(stats.Options{Path: filepath.Join(dir, "stats.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	deepseekM := stats.Key{Provider: "deepseek", Model: "m"}
	zhipuGLM := stats.Key{Provider: "zhipu", Model: "glm"}
	// The committed pre-cutoff buckets carry the historical double count
	// (input inclusive of the cache read); the post-cutoff bucket carries the
	// legacy 4xx-inclusive request count over a corrected token ledger.
	for minute, buckets := range map[int64]map[stats.Key]stats.Counters{
		preCutoff: {
			deepseekM: {Requests: 4, Input: 500, Output: 90, CacheRead: 200, TokenRequests: 3},
			zhipuGLM:  {Requests: 2, Input: 24, Output: 6, TokenRequests: 2},
		},
		postCutoff: {
			deepseekM: {Requests: 3, Input: 100, Output: 10, TokenRequests: 2},
		},
	} {
		if err := store.Flush(minute, buckets); err != nil {
			t.Fatal(err)
		}
	}
	agentClaude := stats.AgentKey{Agent: "claude-code", Provider: "deepseek", Model: "m"}
	agentCodex := stats.AgentKey{Agent: "codex", Provider: "zhipu", Model: "glm"}
	if err := store.FlushAgents(preCutoff, map[stats.AgentKey]stats.AgentCounters{
		agentClaude: {Requests: 4, Input: 500, Output: 90, CacheRead: 200},
		agentCodex:  {Requests: 2, Input: 999, Output: 999},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(postCutoff, map[stats.AgentKey]stats.AgentCounters{
		agentClaude: {Requests: 3, Input: 777, Output: 777},
	}); err != nil {
		t.Fatal(err)
	}

	// 2 pre-cutoff minute buckets, 1 post-cutoff requests repair, 2 agent
	// rows — and no skipped group for the MCP rows.
	rebuilt, skipped, err := store.RebuildTokenLedger(indexPath)
	if err != nil || rebuilt != 5 || skipped != 0 {
		t.Fatalf("rebuild = (%d, %d, %v), want (5, 0, nil): 2 pre-cutoff minute buckets, 1 post-cutoff requests repair, 2 agent rows, no group for the MCP rows", rebuilt, skipped, err)
	}

	minute, err := store.QueryRange(0, postCutoff+60, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantMinute := map[string]stats.Bucket{
		"deepseek/m@" + strconv.Itoa(int(preCutoff)):  {Provider: "deepseek", Model: "m", Minute: preCutoff, Requests: 3, Input: 300, Output: 30, CacheRead: 198, CacheCreation: 15, TokenRequests: 3},
		"zhipu/glm@" + strconv.Itoa(int(preCutoff)):   {Provider: "zhipu", Model: "glm", Minute: preCutoff, Requests: 2, Input: 24, Output: 6, TokenRequests: 2},
		"deepseek/m@" + strconv.Itoa(int(postCutoff)): {Provider: "deepseek", Model: "m", Minute: postCutoff, Requests: 2, Input: 100, Output: 10, TokenRequests: 2},
	}
	if len(minute) != len(wantMinute) {
		t.Fatalf("minute buckets = %+v, want %d", minute, len(wantMinute))
	}
	for _, b := range minute {
		key := b.Provider + "/" + b.Model + "@" + strconv.Itoa(int(b.Minute))
		want, ok := wantMinute[key]
		if !ok {
			t.Errorf("unexpected minute bucket %+v", b)
			continue
		}
		if b.Requests != want.Requests || b.Input != want.Input || b.Output != want.Output ||
			b.CacheRead != want.CacheRead || b.CacheCreation != want.CacheCreation || b.TokenRequests != want.TokenRequests {
			t.Errorf("minute bucket %s = {requests %d input %d output %d cache_read %d cache_creation %d token_requests %d}, want {%d %d %d %d %d %d}",
				key, b.Requests, b.Input, b.Output, b.CacheRead, b.CacheCreation, b.TokenRequests,
				want.Requests, want.Input, want.Output, want.CacheRead, want.CacheCreation, want.TokenRequests)
		}
	}

	agents, err := store.QueryAgents(0, postCutoff+60, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	type tokens struct{ input, output, cacheRead, cacheCreation uint64 }
	wantAgents := map[string]tokens{
		// Pre-cutoff agent rows take the live ledger (the 400, the shadow row
		// and the coinciding MCP row stay out).
		"claude-code/deepseek/m@" + strconv.Itoa(int(preCutoff)): {300, 30, 198, 15},
		"codex/zhipu/glm@" + strconv.Itoa(int(preCutoff)):        {24, 6, 0, 0},
		// Post-cutoff agent rows are not part of the rebuild.
		"claude-code/deepseek/m@" + strconv.Itoa(int(postCutoff)): {777, 777, 0, 0},
	}
	if len(agents) != len(wantAgents) {
		t.Fatalf("agent buckets = %+v, want %d", agents, len(wantAgents))
	}
	for _, a := range agents {
		key := a.Agent + "/" + a.Provider + "/" + a.Model + "@" + strconv.Itoa(int(a.Minute))
		want, ok := wantAgents[key]
		if !ok {
			t.Errorf("unexpected agent bucket %+v", a)
			continue
		}
		if a.Input != want.input || a.Output != want.output || a.CacheRead != want.cacheRead || a.CacheCreation != want.cacheCreation {
			t.Errorf("agent bucket %s = {input %d output %d cache_read %d cache_creation %d}, want {%d %d %d %d}",
				key, a.Input, a.Output, a.CacheRead, a.CacheCreation, want.input, want.output, want.cacheRead, want.cacheCreation)
		}
	}
}

// indexWithRealIndexer writes records as a request-log JSONL file (the Record
// encoding the logger writes), lets the real Indexer tail it in one pass,
// closes the index and returns the index.db path. It fails the test unless
// every record reached the real schema with its flags intact — a harness that
// indexed nothing would let the assertions below pass vacuously.
func indexWithRealIndexer(t *testing.T, dir string, records []requestlog.Record) string {
	t.Helper()
	logPath := filepath.Join(dir, "requests-20261009-043310.log")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for i := range records {
		if err := encoder.Encode(&records[i]); err != nil {
			_ = file.Close()
			t.Fatalf("encode record %d: %v", i, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	// The Indexer's Run loop is the one background owner this test starts;
	// it must be gone again before the test ends.
	goroutines := testsyn.Goroutines()
	indexer, err := requestlog.NewIndexer(dir)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		indexer.Run()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := indexer.WaitReconciled(ctx); err != nil {
		t.Fatalf("first index pass: %v", err)
	}
	indexer.Shutdown()
	<-runDone
	testsyn.LeakCheck(t, goroutines, "indexer Run loop")

	indexPath := filepath.Join(dir, "index.db")
	idx, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	var rows, shadows, legs, mcp int
	if err := idx.QueryRow(`SELECT COUNT(*), SUM(shadow), SUM(attempt_only),
		SUM(CASE WHEN kind = 'mcp' THEN 1 ELSE 0 END) FROM records`).Scan(&rows, &shadows, &legs, &mcp); err != nil {
		t.Fatalf("read the indexer-built index: %v", err)
	}
	if rows != len(records) {
		t.Fatalf("index holds %d rows, want %d — the harness did not index every record", rows, len(records))
	}
	var wantShadow, wantLegs, wantMCP int
	for _, r := range records {
		if r.Shadow {
			wantShadow++
		}
		if r.AttemptOnly {
			wantLegs++
		}
		if r.Kind == "mcp" {
			wantMCP++
		}
	}
	if shadows != wantShadow || legs != wantLegs || mcp != wantMCP {
		t.Fatalf("index flags: shadow rows = %d, attempt_only rows = %d, mcp rows = %d; want %d, %d and %d", shadows, legs, mcp, wantShadow, wantLegs, wantMCP)
	}
	return indexPath
}
