package requestlog

// Same-second parity between the two read engines. Ts has one-second
// resolution and a failover writes a request's legs milliseconds apart, so
// same-second rows are the norm. The index orders them by write order
// (ORDER BY ts DESC, rowid DESC — a request's leg is written before its next
// row); the directory scan serves the same list when the index cannot open
// and must give the same rows in the same order (docs/web-api.md /api/requests:
// "索引不可用时回落目录扫描，语义逐字段一致"). The index==scan matrix in
// index_test.go gives every row its own second ("timestamps are distinct");
// the fixtures here are the half it skips.

import (
	"fmt"
	"reflect"
	"testing"
)

const sameSecond = "2026-10-27T03:36:30Z"

// sameSecondRow builds an LLM forward row; attemptOnly marks a failover leg.
func sameSecondRow(id, provider string, attempt, status int, attemptOnly bool) Record {
	return Record{
		Ts: sameSecond, RequestID: id, SessionID: "sess-1", Protocol: "chat", Method: "POST",
		Path: "/v1/chat/completions", CalledModel: "deepseek", Provider: provider,
		Attempt: attempt, Status: status, LatencyMs: int64(attempt+1) * 10, AttemptOnly: attemptOnly,
	}
}

// requestRow is sameSecondRow at an explicit second, in the seconds around it.
func requestRow(ts, id, provider string, attempt, status int, attemptOnly bool) Record {
	row := sameSecondRow(id, provider, attempt, status, attemptOnly)
	row.Ts = "2026-10-27T03:36:" + ts + "Z"
	return row
}

func summaryIDs(summaries []Summary) []string {
	ids := make([]string, len(summaries))
	for i, summary := range summaries {
		ids[i] = summary.RequestID
	}
	return ids
}

func recordIDs(records []Record) []string {
	ids := make([]string, len(records))
	for i, record := range records {
		ids[i] = record.RequestID
	}
	return ids
}

// assertIndexScanParity runs one summary filter on both engines and fails
// unless the index and the scan answer identically; it returns the scan rows.
func assertIndexScanParity(t *testing.T, dir string, indexer *Indexer, filter Filter) []Summary {
	t.Helper()
	scan, _, err := QuerySummariesWithFacets(dir, filter)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	indexed, _, err := indexer.SummariesWithFacets(filter)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if !reflect.DeepEqual(indexed, scan) {
		t.Fatalf("index/scan diverge for %+v:\nindex: %v\nscan:  %v\nindex rows: %+v\nscan rows:  %+v",
			filter, summaryIDs(indexed), summaryIDs(scan), indexed, scan)
	}
	return scan
}

// TestScanOrdersSameSecondRowsByWriteOrder: same-second rows come back
// newest-written first — the newer file's rows before an older file's, and
// inside a file the later line first — on the top-K path and the unbounded
// one alike.
func TestScanOrdersSameSecondRowsByWriteOrder(t *testing.T) {
	dir := t.TempDir()
	// Two rows in an older file, then forty in a newer one, all in one
	// second. Forty clears sort.Slice's insertion-sort threshold (12), past
	// which an unstable sort leaves equal keys in arbitrary order.
	writeRecordFile(t, dir, "requests-20261027-033600.log", []Record{
		sameSecondRow("s-a", "zhipu", 0, 200, false),
		sameSecondRow("s-b", "zhipu", 0, 200, false),
	})
	newer := make([]Record, 0, 40)
	for i := range 40 {
		newer = append(newer, sameSecondRow(fmt.Sprintf("s%02d", i), "zhipu", 0, 200, false))
	}
	writeRecordFile(t, dir, "requests-20261027-033700.log", newer)
	want := make([]string, 0, 42)
	for i := 39; i >= 0; i-- {
		want = append(want, fmt.Sprintf("s%02d", i))
	}
	want = append(want, "s-b", "s-a")

	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	for _, limit := range []int{10, 100} {
		if got := summaryIDs(assertIndexScanParity(t, dir, indexer, Filter{Limit: limit})); !reflect.DeepEqual(got, want[:min(limit, len(want))]) {
			t.Errorf("summary limit %d: order = %v, want %v", limit, got, want[:min(limit, len(want))])
		}
	}
	// The full-record list: unbounded (no heap involved) and top-K.
	for _, limit := range []int{0, 10} {
		records, err := QueryRecords(dir, Filter{Limit: limit})
		if err != nil {
			t.Fatalf("QueryRecords limit %d: %v", limit, err)
		}
		bound := want
		if limit > 0 {
			bound = want[:min(limit, len(want))]
		}
		if got := recordIDs(records); !reflect.DeepEqual(got, bound) {
			t.Errorf("QueryRecords limit %d: order = %v, want %v", limit, got, bound)
		}
	}
}

// TestSummaryFoldKeepsNewestSameSecondLeg: an unanswered request keeps its
// newest leg as its only row, the earlier legs riding as FailedHops. With
// both legs in one second "newest" is the write order, whichever attempt
// number was written last — the scan must read it the way the index does.
func TestSummaryFoldKeepsNewestSameSecondLeg(t *testing.T) {
	leg := func(provider string, attempt, status int) Record {
		return sameSecondRow("req-b", provider, attempt, status, true)
	}
	cases := []struct {
		name     string
		rows     []Record // write order
		survivor Record
		hops     []FailedHop
	}{
		{
			name:     "attempt order",
			rows:     []Record{leg("p-a", 0, 500), leg("p-b", 1, 503)},
			survivor: leg("p-b", 1, 503),
			hops:     []FailedHop{{Provider: "p-a", Status: 500, Attempt: 0, LatencyMs: 10}},
		},
		{
			// The same two legs with their insertion order swapped: the
			// membership and the row count are unchanged, and the survivor
			// is still the one written last.
			name:     "insertion order swapped",
			rows:     []Record{leg("p-b", 1, 503), leg("p-a", 0, 500)},
			survivor: leg("p-a", 0, 500),
			hops:     []FailedHop{{Provider: "p-b", Status: 503, Attempt: 1, LatencyMs: 20}},
		},
		{
			name:     "legs in different seconds",
			rows:     []Record{requestRow("29", "req-b", "p-a", 0, 500, true), requestRow("30", "req-b", "p-b", 1, 503, true)},
			survivor: requestRow("30", "req-b", "p-b", 1, 503, true),
			hops:     []FailedHop{{Provider: "p-a", Status: 500, Attempt: 0, LatencyMs: 10}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRecordFile(t, dir, "requests-20261027-033600.log", tc.rows)
			indexer := newTestIndexer(t, dir)
			mustReconcile(t, indexer)

			scan := assertIndexScanParity(t, dir, indexer, Filter{Limit: 10})
			if len(scan) != 1 {
				t.Fatalf("rows = %v, want exactly the request's surviving leg", summaryIDs(scan))
			}
			got := scan[0]
			if !got.AttemptOnly || got.Attempt != tc.survivor.Attempt || got.Provider != tc.survivor.Provider ||
				got.Status != tc.survivor.Status || got.Ts != tc.survivor.Ts {
				t.Errorf("surviving leg = %+v, want the one written last %+v", got, tc.survivor)
			}
			if !reflect.DeepEqual(got.FailedHops, tc.hops) {
				t.Errorf("FailedHops = %+v, want %+v", got.FailedHops, tc.hops)
			}
		})
	}
}

// TestSummaryTopKIsNotConsumedByPairedLegs: a request's paired legs fold
// into its committed row, so they must not take top-K slots — the index's
// fold predicate runs before LIMIT, and a page served by the scan holds the
// same committed rows rather than holes where legs were folded away.
func TestSummaryTopKIsNotConsumedByPairedLegs(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20261027-033600.log", []Record{
		requestRow("31", "quiet", "zhipu", 0, 200, false),
		requestRow("32", "req-a", "shopee-a", 0, 502, true),
		requestRow("33", "req-a", "shopee-b", 1, 429, true),
		requestRow("34", "req-a", "shopee-c", 2, 200, false),
	})
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)

	got := assertIndexScanParity(t, dir, indexer, Filter{Limit: 2})
	if ids := summaryIDs(got); !reflect.DeepEqual(ids, []string{"req-a", "quiet"}) {
		t.Fatalf("top-2 rows = %v, want the committed request and the quiet row", ids)
	}
	wantHops := []FailedHop{
		{Provider: "shopee-a", Status: 502, Attempt: 0, LatencyMs: 10},
		{Provider: "shopee-b", Status: 429, Attempt: 1, LatencyMs: 20},
	}
	if !reflect.DeepEqual(got[0].FailedHops, wantHops) {
		t.Errorf("req-a FailedHops = %+v, want both legs %+v", got[0].FailedHops, wantHops)
	}
}

// TestQueryRecordsRequestIDReturnsWholeRequest: a request id names every row
// of the request — each failover leg and the committed answer — and the
// scan's detail lookup returns all of them newest-first, like the index's
// Detail (docs/web-api.md /api/requests/<id>: 同 id 的 attempt 腿记录与已
// 提交行都在 records 里). The legs are written before the answer, so a scan
// that stops at the first line carrying the id returns only the oldest leg.
func TestQueryRecordsRequestIDReturnsWholeRequest(t *testing.T) {
	leg := func(provider string, attempt, status int, second string) Record {
		return requestRow(second, "req-a", provider, attempt, status, true)
	}
	answer := requestRow("31", "req-a", "shopee-c", 2, 200, false)
	type fixtureFile struct {
		name string
		rows []Record // write order
	}
	cases := []struct {
		name  string
		files []fixtureFile // names sort oldest first
		want  []Record
	}{
		{
			name: "legs and answer in one file",
			files: []fixtureFile{{"requests-20261027-033600.log", []Record{
				requestRow("28", "other-1", "zhipu", 0, 200, false),
				leg("shopee-a", 0, 502, "29"),
				requestRow("30", "other-2", "zhipu", 0, 200, false),
				leg("shopee-b", 1, 429, "30"),
				answer,
				requestRow("32", "other-3", "zhipu", 0, 200, false),
			}}},
			want: []Record{answer, leg("shopee-b", 1, 429, "30"), leg("shopee-a", 0, 502, "29")},
		},
		{
			// Rotation between the legs and the answer: the older file still
			// holds the request's first row, so the scan must reach it.
			name: "legs straddle a rotation",
			files: []fixtureFile{
				{"requests-20261027-033600.log", []Record{leg("shopee-a", 0, 502, "29")}},
				{"requests-20261027-033700.log", []Record{leg("shopee-b", 1, 429, "30"), answer}},
			},
			want: []Record{answer, leg("shopee-b", 1, 429, "30"), leg("shopee-a", 0, 502, "29")},
		},
		{
			name:  "unanswered request",
			files: []fixtureFile{{"requests-20261027-033600.log", []Record{leg("shopee-a", 0, 502, "29"), leg("shopee-b", 1, 429, "30")}}},
			want:  []Record{leg("shopee-b", 1, 429, "30"), leg("shopee-a", 0, 502, "29")},
		},
		{
			name:  "request without legs",
			files: []fixtureFile{{"requests-20261027-033600.log", []Record{answer}}},
			want:  []Record{answer},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, file := range tc.files {
				writeRecordFile(t, dir, file.name, file.rows)
			}
			indexer := newTestIndexer(t, dir)
			mustReconcile(t, indexer)

			scan, err := QueryRecords(dir, Filter{RequestID: "req-a", Limit: 50})
			if err != nil {
				t.Fatalf("scan detail: %v", err)
			}
			indexed, err := indexer.Detail("req-a")
			if err != nil {
				t.Fatalf("index detail: %v", err)
			}
			if !reflect.DeepEqual(scan, tc.want) {
				t.Fatalf("scan detail rows = %+v, want newest-first %+v", scan, tc.want)
			}
			if !reflect.DeepEqual(indexed, scan) {
				t.Fatalf("index/scan detail diverge:\nindex: %+v\nscan:  %+v", indexed, scan)
			}
		})
	}
}

// TestDetailFallbackOrdersSameSecondRowsByWriteOrder: the Detail fallback
// streams the un-indexed tail oldest-first, and rows of one request share a
// second — right after a failover commits, the detail must already have the
// indexed path's order (the answer first), not the file's.
func TestDetailFallbackOrdersSameSecondRowsByWriteOrder(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20261027-033500.log", []Record{
		{Ts: "2026-10-27T03:35:00Z", RequestID: "past", Status: 200},
	})
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	// A file reconcile has never seen: its whole content is the tail.
	first := sameSecondRow("req-a", "shopee-a", 0, 502, true)
	second := sameSecondRow("req-a", "shopee-b", 1, 429, true)
	answer := sameSecondRow("req-a", "shopee-c", 2, 200, false)
	writeRecordFile(t, dir, "requests-20261027-033600.log", []Record{first, second, answer})
	want := []Record{answer, second, first}

	lagging, err := indexer.Detail("req-a")
	if err != nil {
		t.Fatalf("detail before reconcile: %v", err)
	}
	if !reflect.DeepEqual(lagging, want) {
		t.Fatalf("detail before reconcile = %+v, want newest-first %+v", lagging, want)
	}
	mustReconcile(t, indexer)
	indexed, err := indexer.Detail("req-a")
	if err != nil {
		t.Fatalf("detail after reconcile: %v", err)
	}
	if !reflect.DeepEqual(indexed, want) {
		t.Fatalf("detail after reconcile = %+v, want newest-first %+v", indexed, want)
	}
}
