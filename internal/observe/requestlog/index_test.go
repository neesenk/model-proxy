package requestlog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

const indexTestSSEBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n"

// indexFixtureRecords covers the filter dimensions: providers/models (with
// case variants), shadow, status bands, sessions, agents (including records
// with no agent, which must not become a facet option), empty provider/model
// (facet skipping) and both usage body shapes (JSON + SSE). Timestamps are
// distinct so newest-first ordering is deterministic on both the scan and
// index paths.
func indexFixtureRecords() []Record {
	return []Record{
		{Ts: "2026-07-29T12:00:00Z", RequestID: "r1", SessionID: "sess-a", Protocol: "anthropic", Method: "POST", Path: "/v1/messages", CalledModel: "Model-X", UpstreamModel: "up-x", Exposed: "exposed-x", Provider: "Provider-X", Agent: "agent-a", Attempt: 1, Status: 200, LatencyMs: 12, RequestSize: 100, ResponseSize: 50, RequestBody: "req-1", ResponseBody: `{"usage":{"input_tokens":10,"output_tokens":5}}`},
		{Ts: "2026-07-29T12:01:00Z", RequestID: "r2", SessionID: "sess-a", Protocol: "chat", Method: "POST", Path: "/v1/chat/completions", CalledModel: "other", Provider: "provider-y", Status: 500, Shadow: true, RequestBody: "req-2", ResponseBody: `{"error":"boom"}`},
		{Ts: "2026-07-30T01:02:03Z", RequestID: "r3", SessionID: "sess-b", Protocol: "chat", Method: "POST", Path: "/v1/chat/completions", CalledModel: "model-x-twin", UpstreamModel: "up-y", Provider: "provider-y", Agent: "agent-b", Status: 200, ResponseBody: indexTestSSEBody},
		{Ts: "2026-07-30T01:03:03Z", RequestID: "r4", Protocol: "responses", Method: "POST", Path: "/v1/responses", Status: 200},
	}
}

func writeIndexFixture(t *testing.T, dir string) []Record {
	t.Helper()
	records := indexFixtureRecords()
	writeRecordFile(t, dir, "requests-20260729.log", records[:2])
	writeRecordFile(t, dir, "requests-20260730.log", records[2:])
	return records
}

func appendRecordLines(t *testing.T, path string, records ...Record) {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for i := range records {
		if err := encoder.Encode(&records[i]); err != nil {
			t.Fatalf("encode record: %v", err)
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := file.Write(buffer.Bytes()); err != nil {
		_ = file.Close()
		t.Fatalf("append %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// newTestIndexer opens an indexer that never runs its loop; tests drive
// reconcile synchronously. The cleanup closes the database (Shutdown needs a
// running loop, so Run-based tests call Shutdown themselves instead).
func newTestIndexer(t *testing.T, dir string) *Indexer {
	t.Helper()
	indexer, err := NewIndexer(dir)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	t.Cleanup(func() { _ = indexer.db.Close() })
	return indexer
}

func mustReconcile(t *testing.T, indexer *Indexer) {
	t.Helper()
	if err := indexer.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func indexRowCount(t *testing.T, indexer *Indexer) int {
	t.Helper()
	var count int
	if err := indexer.db.QueryRow(`SELECT COUNT(*) FROM records`).Scan(&count); err != nil {
		t.Fatalf("count index rows: %v", err)
	}
	return count
}

// TestIndexSummariesEquivalenceVsScan pins the index query against the file
// scan field-by-field across the filter matrix. Facets are compared only on
// the unbounded filter: the scan's facets are bounded by its file-level early
// termination window, the index answers over the whole retained set (both are
// "collected before the filter"; the divergence only shows with a limit or a
// From bound that lets the scan skip whole files).
func TestIndexSummariesEquivalenceVsScan(t *testing.T) {
	dir := t.TempDir()
	writeIndexFixture(t, dir)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	from, _ := time.Parse(time.RFC3339, "2026-07-30T00:00:00Z")
	to, _ := time.Parse(time.RFC3339, "2026-07-29T12:00:30Z")
	filters := map[string]Filter{
		"baseline usage":        {Limit: 100, UsageOnly: true},
		"baseline no usage":     {Limit: 100},
		"model case-fold":       {Model: "model-x", Limit: 100, UsageOnly: true},
		"model across upstream": {Model: "UP-Y", Limit: 100, UsageOnly: true},
		"model exposed":         {Model: "exposed", Limit: 100, UsageOnly: true},
		"model miss":            {Model: "zzz", Limit: 100, UsageOnly: true},
		"provider substring":    {Provider: "PROVIDER", Limit: 100, UsageOnly: true},
		"provider exact-ish":    {Provider: "provider-x", Limit: 100, UsageOnly: true},
		"agent exact":           {Agent: "agent-a", Limit: 100, UsageOnly: true},
		"agent other":           {Agent: "agent-b", Limit: 100, UsageOnly: true},
		"agent case-sensitive":  {Agent: "AGENT-A", Limit: 100, UsageOnly: true},
		"agent substring miss":  {Agent: "agent", Limit: 100, UsageOnly: true},
		"status":                {Status: 500, Limit: 100, UsageOnly: true},
		"errors only":           {ErrorsOnly: true, Limit: 100, UsageOnly: true},
		"shadow only":           {Shadow: "only", Limit: 100, UsageOnly: true},
		"shadow exclude":        {Shadow: "exclude", Limit: 100, UsageOnly: true},
		"session":               {Session: "sess-a", Limit: 100, UsageOnly: true},
		"request id":            {RequestID: "r3", Limit: 100, UsageOnly: true},
		"limit top-k":           {Limit: 2, UsageOnly: true},
		"combined narrowing":    {Model: "model-x", Provider: "provider-y", Shadow: "exclude", Limit: 100, UsageOnly: true},
		"from bound":            {From: from, Limit: 100, UsageOnly: true},
		"to bound":              {To: to, Limit: 100, UsageOnly: true},
		"from and to":           {From: to, To: from, Limit: 100, UsageOnly: true},
		"time bound impossible": {From: from, To: to, Limit: 100, UsageOnly: true},
		"from equals record ts": {From: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC), Limit: 100, UsageOnly: true},
		"to equals record ts":   {To: time.Date(2026, 7, 29, 12, 1, 0, 0, time.UTC), Limit: 100, UsageOnly: true},
	}
	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			wantSummaries, wantFacets, err := QuerySummariesWithFacets(dir, filter)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			gotSummaries, gotFacets, err := indexer.SummariesWithFacets(filter)
			if err != nil {
				t.Fatalf("index: %v", err)
			}
			if !reflect.DeepEqual(gotSummaries, wantSummaries) {
				t.Errorf("summaries differ:\nindex: %+v\nscan:  %+v", gotSummaries, wantSummaries)
			}
			// Facets: only where the scan cannot narrow its collection — no
			// early-termination pressure (limit heap, From bound) and no
			// rawPrefilter gating (RequestID/Session skip undecoded lines,
			// which also skips their facet contribution on the scan path).
			bounded := (filter.Limit > 0 && filter.Limit < len(indexFixtureRecords())) ||
				!filter.From.IsZero() || filter.RequestID != "" || filter.Session != ""
			if !bounded && !reflect.DeepEqual(gotFacets, wantFacets) {
				t.Errorf("facets differ:\nindex: %+v\nscan:  %+v", gotFacets, wantFacets)
			}
		})
	}
}

// TestIndexShadowReportEquivalenceVsScan pins the index-backed shadow report
// against the directory scan across the window/limit matrix, including the
// unpaired-row skips. The eval page loads this surface on every open; the
// scan streams every JSONL line of the window (multi-MB bodies included, then
// discarded), while the index answers from metadata rows — this test holds
// the two answers identical.
func TestIndexShadowReportEquivalenceVsScan(t *testing.T) {
	dir := t.TempDir()
	primaries := []Record{
		{Ts: "2026-07-19T01:00:00Z", RequestID: "b1", Exposed: "beta", Provider: "primary-b", Status: 200, LatencyMs: 10, ResponseSize: 1},
		{Ts: "2026-07-19T01:00:01Z", RequestID: "b2", Exposed: "beta", Provider: "primary-b", Status: 500, LatencyMs: 20, ResponseSize: 2},
		{Ts: "2026-07-20T01:00:00Z", RequestID: "a1", Exposed: "alpha", Provider: "primary-a", Status: 200, LatencyMs: 100, ResponseSize: 1000},
		{Ts: "2026-07-20T01:00:01Z", RequestID: "lonely-primary", Exposed: "ignored", Provider: "primary", Status: 200},
	}
	shadows := []Record{
		{Ts: "2026-07-19T01:01:00Z", RequestID: "shadow-b1", Provider: "shadow-b", Status: 201, LatencyMs: 40, ResponseSize: 4, Shadow: true},
		{Ts: "2026-07-19T01:01:01Z", RequestID: "shadow-b2", Provider: "shadow-b", Status: 503, LatencyMs: 50, ResponseSize: 5, Shadow: true},
		{Ts: "2026-07-20T01:01:00Z", RequestID: "shadow-a1", Provider: "shadow-a", Status: 500, LatencyMs: 160, ResponseSize: 800, Shadow: true},
		{Ts: "2026-07-20T01:01:01Z", RequestID: "shadow-lonely-shadow", Provider: "shadow", Status: 200, Shadow: true},
	}
	writeRecordFile(t, dir, "requests-20260719.log", primaries[:2])
	writeRecordFile(t, dir, "requests-20260719-2.log", shadows[:2])
	writeRecordFile(t, dir, "requests-20260720.log", primaries[2:])
	writeRecordFile(t, dir, "requests-20260720-2.log", shadows[2:])
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	from19, _ := time.Parse(time.RFC3339, "2026-07-19T00:00:00Z")
	from20, _ := time.Parse(time.RFC3339, "2026-07-20T00:00:00Z")
	to19, _ := time.Parse(time.RFC3339, "2026-07-19T23:59:59Z")
	filters := map[string]Filter{
		"baseline":        {Limit: 100},
		"no limit":        {},
		"day 19 window":   {From: from19, To: to19, Limit: 100},
		"from day 20":     {From: from20, Limit: 100},
		"to mid window":   {To: from20, Limit: 100},
		"limit top-k":     {Limit: 3},
		"shadow-only":     {Shadow: "only", Limit: 100},
		"errors only":     {ErrorsOnly: true, Limit: 100},
		"provider filter": {Provider: "primary-b", Limit: 100},
	}
	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			want, err := ShadowReport(dir, filter)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			got, err := indexer.ShadowReport(filter)
			if err != nil {
				t.Fatalf("index: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("shadow report differs:\nindex: %+v\nscan:  %+v", got, want)
			}
		})
	}

	// Sanity: the baseline window pairs both groups and skips the unpaired.
	got, err := indexer.ShadowReport(Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Route != "beta" || got[0].Samples != 2 || got[1].Route != "alpha" || got[1].Samples != 1 {
		t.Fatalf("index shadow report = %+v", got)
	}
}

// TestIndexShadowReportIgnoresKindFilter verifies that ShadowReport never
// filters by kind: shadow evaluation rows are LLM-stream rows, and a caller-
// supplied kind=mcp filter must not suppress the paired shadow rows.
func TestIndexShadowReportIgnoresKindFilter(t *testing.T) {
	dir := t.TempDir()
	records := []Record{
		{Ts: "2026-07-29T12:00:00Z", RequestID: "r1", Exposed: "m", Provider: "p", Status: 200, LatencyMs: 5},
		{Ts: "2026-07-29T12:00:01Z", RequestID: "shadow-r1", Provider: "sp", Status: 200, LatencyMs: 8, Shadow: true},
		{Ts: "2026-07-29T12:00:02Z", RequestID: "mcp-1", Provider: "mcp", Status: 200, Kind: "mcp"},
	}
	writeRecordFile(t, dir, "requests-20260729.log", records)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	report, err := indexer.ShadowReport(Filter{Kind: "mcp", Limit: 100})
	if err != nil {
		t.Fatalf("ShadowReport: %v", err)
	}
	if len(report) != 1 || report[0].Route != "m" || report[0].PrimaryProvider != "p" || report[0].ShadowProvider != "sp" {
		t.Fatalf("shadow report with kind=mcp = %+v, want one paired row", report)
	}
}

// TestIndexReconcileAppendRotationRetention covers the reconcile state
// machine: appends advance the cursor, a new (rotated-day) file joins, a
// deleted file drops its rows, and a shrunk file is re-indexed from zero.
func TestIndexReconcileAppendRotationRetention(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "requests-20260730.log")
	appendRecordLines(t, active, indexFixtureRecords()[0])
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != 1 {
		t.Fatalf("rows after first reconcile = %d, want 1", got)
	}

	// Append: the cursor tails the new bytes.
	appendRecordLines(t, active, indexFixtureRecords()[1])
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != 2 {
		t.Fatalf("rows after append = %d, want 2", got)
	}

	// Rotation: a new file name joins the directory.
	rotated := filepath.Join(dir, "requests-20260730--010203-1.log")
	appendRecordLines(t, rotated, indexFixtureRecords()[2])
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != 3 {
		t.Fatalf("rows after rotation = %d, want 3", got)
	}

	// Retention: deleting a file drops its rows.
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != 1 {
		t.Fatalf("rows after retention delete = %d, want 1", got)
	}
	summaries, _, err := indexer.SummariesWithFacets(Filter{Limit: 100, UsageOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "r3" {
		t.Fatalf("surviving summaries = %+v, want only r3", summaries)
	}

	// Truncation/replace: a smaller file under an indexed name is re-indexed
	// from zero (old rows for it are gone, the new content is indexed).
	replacement := Record{Ts: "2026-07-30T02:00:00Z", RequestID: "r9", Provider: "p9", Status: 200}
	writeRecordFile(t, dir, "requests-20260730--010203-1.log", []Record{replacement})
	mustReconcile(t, indexer)
	summaries, _, err = indexer.SummariesWithFacets(Filter{Limit: 100, UsageOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "r9" {
		t.Fatalf("summaries after truncation = %+v, want only r9", summaries)
	}
}

// TestIndexReconcilePartialLine: a record still being written (no trailing
// newline) is invisible until the line completes; an unparseable line is
// skipped but still advances the cursor, so later records index normally.
func TestIndexReconcilePartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-20260730.log")
	appendRecordLines(t, path, indexFixtureRecords()[0])
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	// Half a line, unterminated: nothing indexes, the cursor holds.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"ts":"2026-07-30T03:00:00Z","request_id":"half`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != 1 {
		t.Fatalf("rows with partial tail = %d, want 1", got)
	}

	// Complete the line (plus an unparseable line and a valid successor): the
	// completed half and the successor index, the garbage line is consumed.
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`-line","status":200}` + "\n" + "not json at all\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	appendRecordLines(t, path, Record{Ts: "2026-07-30T03:01:00Z", RequestID: "after-garbage", Status: 200})
	mustReconcile(t, indexer)
	summaries, _, err := indexer.SummariesWithFacets(Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, summary := range summaries {
		ids[summary.RequestID] = true
	}
	if len(summaries) != 3 || !ids["r1"] || !ids["half-line"] || !ids["after-garbage"] {
		t.Fatalf("summaries after completing the tail = %+v", summaries)
	}

	// A trailing run of ONLY unparseable lines inserts no rows but must still
	// advance the cursor — otherwise every reconcile re-reads the same garbage.
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("garbage one\ngarbage two\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != 3 {
		t.Fatalf("rows after garbage-only append = %d, want 3", got)
	}
	var cursor int64
	if err := indexer.db.QueryRow(`SELECT size_indexed FROM files WHERE path = ?`, "requests-20260730.log").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != info.Size() {
		t.Fatalf("cursor = %d, want file size %d after consuming garbage lines", cursor, info.Size())
	}
}

// TestIndexRebuild: deleting index.db loses only the cursor state — a fresh
// indexer re-tails every file from zero. A corrupted index.db (unopenable to
// SQLite) self-heals the same way at open.
func TestIndexRebuild(t *testing.T) {
	dir := t.TempDir()
	writeIndexFixture(t, dir)

	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != 4 {
		t.Fatalf("rows = %d, want 4", got)
	}

	// Corrupt the index file of a second indexer on another directory.
	corruptDir := t.TempDir()
	writeIndexFixture(t, corruptDir)
	if err := os.WriteFile(filepath.Join(corruptDir, indexFileName), []byte("this is not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	healed, err := NewIndexer(corruptDir)
	if err != nil {
		t.Fatalf("NewIndexer over corrupted index.db: %v", err)
	}
	defer func() { _ = healed.db.Close() }()
	mustReconcile(t, healed)
	summaries, _, err := healed.SummariesWithFacets(Filter{Limit: 100, UsageOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 4 {
		t.Fatalf("summaries after corrupt-index rebuild = %d, want 4", len(summaries))
	}
}

// TestIndexDetailSeek: indexed detail reads return the full record (bodies,
// headers) by a single (file, offset, length) seek, byte-equal to the scan.
func TestIndexDetailSeek(t *testing.T) {
	dir := t.TempDir()
	writeIndexFixture(t, dir)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		want, err := QueryRecords(dir, Filter{RequestID: id, Limit: 50})
		if err != nil {
			t.Fatalf("scan detail %s: %v", id, err)
		}
		got, err := indexer.Detail(id)
		if err != nil {
			t.Fatalf("index detail %s: %v", id, err)
		}
		if len(got) != 1 || !reflect.DeepEqual(got, want) {
			t.Errorf("detail %s:\nindex: %+v\nscan:  %+v", id, got, want)
		}
		if got[0].RequestBody == "" && id == "r1" {
			t.Errorf("detail %s lost the request body", id)
		}
	}

	// Index miss: a record committed after the last reconcile falls back to
	// the scan, so a just-committed record never reports "not logged".
	appendRecordLines(t, filepath.Join(dir, "requests-20260730.log"), Record{
		Ts: "2026-07-30T04:00:00Z", RequestID: "fresh", Status: 200, ResponseBody: "fresh-body",
	})
	got, err := indexer.Detail("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResponseBody != "fresh-body" {
		t.Fatalf("miss-fallback detail = %+v", got)
	}

	// Truly unknown id: empty, no error (the handler turns it into 404).
	got, err = indexer.Detail("nope")
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown detail = (%+v, %v), want empty", got, err)
	}
}

// TestIndexDetailFallbackScansOnlyUnindexedTail pins the Detail fallback's
// scan domain: only the bytes beyond a file's indexed cursor. A findable id
// inside already-indexed bytes exists only with its index row removed
// (hand-rolled below; in production an indexed row is always seekable), and
// the fallback must miss it — the full-directory QueryRecords fallback it
// replaced streamed every indexed byte for such misses (seconds on a real
// multi-GB log; every never-logged id, e.g. the Live view's client-gone
// 499, paid it on each drill).
func TestIndexDetailFallbackScansOnlyUnindexedTail(t *testing.T) {
	dir := t.TempDir()
	writeIndexFixture(t, dir)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	// Indexed bytes are not rescanned: r1 sits in requests-20260729.log,
	// fully indexed — deleting its index row must turn Detail into a miss.
	if _, err := indexer.db.Exec(`DELETE FROM records WHERE request_id = ?`, "r1"); err != nil {
		t.Fatal(err)
	}
	got, err := indexer.Detail("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("detail over deleted index row = %+v, want empty (only the un-indexed tail is scanned)", got)
	}

	// The lag case the fallback exists for: committed after the last
	// reconcile pass, i.e. appended beyond an indexed file's cursor.
	appendRecordLines(t, filepath.Join(dir, "requests-20260730.log"), Record{
		Ts: "2026-07-30T05:00:00Z", RequestID: "lagging", Status: 200, ResponseBody: "lag-body",
	})
	got, err = indexer.Detail("lagging")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResponseBody != "lag-body" {
		t.Fatalf("lagging detail = %+v, want the tail record", got)
	}

	// A file reconcile has never seen (absent from the files table) has
	// cursor zero: the whole file is the un-indexed tail (index rebuild).
	writeRecordFile(t, dir, "requests-20260731.log", []Record{{
		Ts: "2026-07-31T01:00:00Z", RequestID: "newfile", Status: 200, ResponseBody: "new-body",
	}})
	got, err = indexer.Detail("newfile")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResponseBody != "new-body" {
		t.Fatalf("newfile detail = %+v, want the never-reconciled file's record", got)
	}

	// A decodable final line without its newline is still returned (the scan
	// path parses the unterminated tail too — the newline lands in the same
	// Write, so the content is complete); a genuinely torn write that cannot
	// decode is skipped.
	last := filepath.Join(dir, "requests-20260731.log")
	appendFragment := func(s string) {
		t.Helper()
		file, err := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	appendFragment(`{"request_id":"torn-ok","ts":"2026-07-31T02:00:00Z","status":200,"response_body":"ok"}`)
	got, err = indexer.Detail("torn-ok")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResponseBody != "ok" {
		t.Fatalf("torn-ok detail = %+v, want the unterminated-but-decodable line", got)
	}
	// Terminate the line above, then leave a torn write as the new tail.
	appendFragment("\n" + `{"request_id":"torn-bad","ts":"2026-07-31T03:00:00Z","sta`)
	got, err = indexer.Detail("torn-bad")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("torn-bad detail = %+v, want empty (a torn write is not consumable)", got)
	}
}

// TestIndexUsageParity: the usage columns written at index time equal
// ExtractUsage over the raw bodies, for both the JSON and the SSE shape, and
// flow into summaries only under UsageOnly.
func TestIndexUsageParity(t *testing.T) {
	dir := t.TempDir()
	writeIndexFixture(t, dir)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	summaries, _, err := indexer.SummariesWithFacets(Filter{Limit: 100, UsageOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Summary{}
	for _, summary := range summaries {
		byID[summary.RequestID] = summary
	}
	for _, record := range indexFixtureRecords() {
		want := ExtractUsage(record.ResponseBody)
		got := byID[record.RequestID]
		if got.Input != want.Input || got.Output != want.Output ||
			got.CacheRead != want.CacheRead || got.CacheCreation != want.CacheCreation {
			t.Errorf("usage %s = %+v, want ExtractUsage %+v", record.RequestID, got, want)
		}
	}
	// Sanity: the SSE body actually produced usage (7 in / 9 out).
	if byID["r3"].Input != 7 || byID["r3"].Output != 9 {
		t.Errorf("SSE usage = %+v, want input 7 output 9", byID["r3"])
	}

	// Without UsageOnly the projection zeroes the usage fields (scan parity:
	// QuerySummaries only fills them under UsageOnly).
	plain, _, err := indexer.SummariesWithFacets(Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range plain {
		if summary.Input != 0 || summary.Output != 0 || summary.CacheRead != 0 || summary.CacheCreation != 0 {
			t.Errorf("non-UsageOnly summary carries usage: %+v", summary)
		}
	}
}

// TestIndexSessionSummariesEquivalence: the index-backed session aggregation
// equals the file-scan aggregation row for row, including shadow counting,
// error counting and the cost callback.
func TestIndexSessionSummariesEquivalence(t *testing.T) {
	dir := t.TempDir()
	writeIndexFixture(t, dir)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	costOf := func(provider, model string, usage Usage) float64 {
		return float64(len(provider)+len(model)) + float64(usage.Input+usage.Output)/1000
	}
	want, err := SessionSummaries(dir, 2000, 50, costOf)
	if err != nil {
		t.Fatalf("scan sessions: %v", err)
	}
	got, err := indexer.SessionSummaries(2000, 50, costOf)
	if err != nil {
		t.Fatalf("index sessions: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions differ:\nindex: %+v\nscan:  %+v", got, want)
	}
	// ScanLimit bounds the window on both paths: with scanLimit 1 only the
	// newest record participates (r4, session-less) → no sessions at all.
	got, err = indexer.SessionSummaries(1, 50, costOf)
	if err != nil {
		t.Fatal(err)
	}
	want, err = SessionSummaries(dir, 1, 50, costOf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || len(got) != 0 {
		t.Fatalf("scanLimit=1 sessions: index %+v vs scan %+v", got, want)
	}
	// limit caps the kept sessions (most recently active first).
	got, err = indexer.SessionSummaries(2000, 1, costOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "sess-b" {
		t.Fatalf("limit=1 sessions = %+v, want only sess-b", got)
	}
}

// TestIndexRunLifecycle: the background loop reconciles immediately and once
// more as a final flush on Shutdown, so records written after the last tick
// are still indexed before the database closes.
func TestIndexRunLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-20260730.log")
	appendRecordLines(t, path, indexFixtureRecords()[0])
	indexer, err := NewIndexer(dir)
	if err != nil {
		t.Fatal(err)
	}
	indexer.tick = time.Hour // no tick interference: only the boot pass and the final flush run
	go indexer.Run()
	select {
	case <-indexer.reconciled:
	case <-time.After(5 * time.Second):
		t.Fatal("boot reconcile did not signal")
	}

	// Written after the boot pass: only Shutdown's final flush can index it.
	appendRecordLines(t, path, Record{Ts: "2026-07-30T05:00:00Z", RequestID: "tail", Status: 200})
	indexer.Shutdown()

	reopened, err := NewIndexer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.db.Close() }()
	summaries, _, err := reopened.SummariesWithFacets(Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 || summaries[0].RequestID != "tail" {
		t.Fatalf("summaries after shutdown flush = %+v, want r1 + tail", summaries)
	}
}

// TestIndexConcurrentReconcileAndQueries: appends, reconcile ticks and web
// queries run concurrently; everything is race-clean and the final state
// holds every written record.
func TestIndexConcurrentReconcileAndQueries(t *testing.T) {
	dir := t.TempDir()
	indexer, err := NewIndexer(dir)
	if err != nil {
		t.Fatal(err)
	}
	indexer.tick = 2 * time.Millisecond
	go indexer.Run()

	const total = 120
	path := filepath.Join(dir, "requests-20260730.log")
	writer := sync.WaitGroup{}
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; i < total; i++ {
			appendRecordLines(t, path, Record{
				Ts:        time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339),
				RequestID: "bulk-" + strconv.Itoa(i),
				SessionID: "sess-bulk",
				Provider:  "p", CalledModel: "m", Status: 200,
				ResponseBody: `{"usage":{"input_tokens":1,"output_tokens":2}}`,
			})
		}
	}()

	stop := make(chan struct{})
	readers := sync.WaitGroup{}
	for i := 0; i < 3; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := indexer.SummariesWithFacets(Filter{Limit: 10, UsageOnly: true, Provider: "p"}); err != nil {
					t.Errorf("concurrent summaries: %v", err)
					return
				}
				if _, err := indexer.SessionSummaries(2000, 50, nil); err != nil {
					t.Errorf("concurrent sessions: %v", err)
					return
				}
				if _, err := indexer.Detail("bulk-never-written"); err != nil {
					t.Errorf("concurrent detail: %v", err)
					return
				}
			}
		}()
	}

	writer.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if got := indexRowCount(t, indexer); got == total {
			break
		}
		if err := indexer.WaitReconciled(ctx); err != nil {
			t.Fatalf("index rows = %d, want %d (indexer did not catch up)", indexRowCount(t, indexer), total)
		}
	}
	close(stop)
	readers.Wait()
	indexer.Shutdown()
}

// TestIndexSummariesPreserveTurnKey verifies that the derived index stores the
// turn_key column and that the UsageOnly summary projection carries it through.
func TestIndexSummariesPreserveTurnKey(t *testing.T) {
	dir := t.TempDir()
	records := []Record{
		{Ts: "2026-09-16T10:00:00Z", RequestID: "r-turn", SessionID: "s", Provider: "p", Status: 200, TurnKey: "deadbeefcafe1234"},
		{Ts: "2026-09-16T10:01:00Z", RequestID: "r-none", SessionID: "s", Provider: "p", Status: 200},
	}
	writeRecordFile(t, dir, "requests-20260916.log", records)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	summaries, _, err := indexer.SummariesWithFacets(Filter{Limit: 100, UsageOnly: true})
	if err != nil {
		t.Fatalf("SummariesWithFacets: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("got %d summaries, want 2", len(summaries))
	}
	byID := map[string]Summary{}
	for _, s := range summaries {
		byID[s.RequestID] = s
	}
	if byID["r-turn"].TurnKey != "deadbeefcafe1234" {
		t.Fatalf("r-turn TurnKey = %q, want deadbeefcafe1234", byID["r-turn"].TurnKey)
	}
	if byID["r-none"].TurnKey != "" {
		t.Fatalf("r-none TurnKey = %q, want empty", byID["r-none"].TurnKey)
	}

	// The scan path must agree with the index path.
	want, _, err := QuerySummariesWithFacets(dir, Filter{Limit: 100, UsageOnly: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !reflect.DeepEqual(summaries, want) {
		t.Fatalf("index/scan summaries differ:\nindex: %+v\nscan:  %+v", summaries, want)
	}
}
