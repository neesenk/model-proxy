package requestlog

import (
	"bytes"
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := indexRowCount(t, indexer); got == total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("index rows = %d, want %d (indexer did not catch up)", indexRowCount(t, indexer), total)
		}
		time.Sleep(2 * time.Millisecond)
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
