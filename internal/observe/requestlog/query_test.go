package requestlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQueryRecordsFiltersAndLimit(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260718-100000.log", []Record{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "a", Exposed: "glm", CalledModel: "glm", UpstreamModel: "glm-4", Provider: "zhipu", Status: 200, LatencyMs: 120},
		{Ts: "2026-07-18T10:01:00Z", RequestID: "b", Exposed: "glm", CalledModel: "glm", UpstreamModel: "glm-4", Provider: "deepseek", Status: 500, LatencyMs: 30},
		{Ts: "2026-07-18T10:02:00Z", RequestID: "c", Exposed: "codex", CalledModel: "codex-1", UpstreamModel: "gpt-5", Provider: "codex", Status: 200, LatencyMs: 90},
	})

	tests := []struct {
		name    string
		filter  Filter
		wantIDs []string
	}{
		{"all newest first", Filter{Limit: 100}, []string{"c", "b", "a"}},
		{"provider case insensitive", Filter{Provider: "ZHIPU", Limit: 100}, []string{"a"}},
		{"model across fields", Filter{Model: "GLM", Limit: 100}, []string{"b", "a"}},
		{"errors only", Filter{ErrorsOnly: true, Limit: 100}, []string{"b"}},
		{"exact status", Filter{Status: 200, Limit: 100}, []string{"c", "a"}},
		{"request id", Filter{RequestID: "b", Limit: 100}, []string{"b"}},
		{"limit", Filter{Limit: 2}, []string{"c", "b"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			records, err := QueryRecords(dir, test.filter)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(records))
			for i := range records {
				got[i] = records[i].RequestID
			}
			if !reflect.DeepEqual(got, test.wantIDs) {
				t.Errorf("ids = %v, want %v", got, test.wantIDs)
			}
		})
	}
}

func TestFilterShadowModes(t *testing.T) {
	shadow := Record{Ts: "2026-07-18T10:00:00Z", RequestID: "shadow-a", Shadow: true}
	primary := Record{Ts: "2026-07-18T10:00:00Z", RequestID: "a"}
	tests := []struct {
		name   string
		mode   string
		record Record
		want   bool
	}{
		{"empty keeps shadow", "", shadow, true},
		{"empty keeps primary", "", primary, true},
		{"only keeps shadow", "only", shadow, true},
		{"only drops primary", "only", primary, false},
		{"exclude drops shadow", "exclude", shadow, false},
		{"exclude keeps primary", "exclude", primary, true},
		{"unknown keeps shadow", "bogus", shadow, true},
		{"unknown keeps primary", "bogus", primary, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := (Filter{Shadow: test.mode}).matches(test.record); got != test.want {
				t.Errorf("matches = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFilterTimeBoundsAreInclusive(t *testing.T) {
	from := time.Date(2026, 7, 18, 10, 0, 0, 500, time.UTC)
	to := from.Add(time.Minute)
	filter := Filter{From: from, To: to}
	tests := []struct {
		name string
		ts   string
		want bool
	}{
		{"one nanosecond before", from.Add(-time.Nanosecond).Format(time.RFC3339Nano), false},
		{"exact from", from.Format(time.RFC3339Nano), true},
		{"inside", from.Add(30 * time.Second).Format(time.RFC3339Nano), true},
		{"exact to", to.Format(time.RFC3339Nano), true},
		{"one nanosecond after", to.Add(time.Nanosecond).Format(time.RFC3339Nano), false},
		{"malformed timestamp", "not-a-timestamp", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := filter.matches(Record{Ts: test.ts}); got != test.want {
				t.Errorf("matches(%q) = %v, want %v", test.ts, got, test.want)
			}
		})
	}
	if !(Filter{}).matches(Record{Ts: "not-a-timestamp"}) {
		t.Error("timestamp parsing should not be required when no time bound is set")
	}
}

func TestMetadataQueryClearsBodiesAndResponseHeadersBeforeRetention(t *testing.T) {
	dir := t.TempDir()
	source := []Record{
		{
			Ts: "2026-07-18T10:00:00Z", RequestID: "r1", Provider: "zhipu", Status: 200,
			RequestBody: "BIG-REQ-1", ResponseBody: "BIG-RESP-1", ResponseHeaders: `{"x-request-id":"secret-1"}`,
		},
		{
			Ts: "2026-07-18T10:00:01Z", RequestID: "r2", Provider: "zhipu", Status: 200,
			RequestBody: "BIG-REQ-2", ResponseBody: "BIG-RESP-2", ResponseHeaders: `{"x-request-id":"secret-2"}`,
		},
	}
	writeRecordFile(t, dir, "requests-20260718-100000.log", source)

	metadata, err := query(dir, filePrefix, Filter{Limit: 1}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 1 || metadata[0].RequestID != "r2" {
		t.Fatalf("metadata top-1 = %+v, want r2", metadata)
	}
	if metadata[0].RequestBody != "" || metadata[0].ResponseBody != "" || metadata[0].ResponseHeaders != "" {
		t.Errorf("metadata retained body/header fields: %+v", metadata[0])
	}
	summaries, err := QuerySummaries(dir, Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "r2" {
		t.Errorf("QuerySummaries top-1 = %+v, want r2", summaries)
	}

	full, err := QueryRecords(dir, Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 1 ||
		full[0].RequestBody != source[1].RequestBody ||
		full[0].ResponseBody != source[1].ResponseBody ||
		full[0].ResponseHeaders != source[1].ResponseHeaders {
		t.Errorf("full query did not retain body/header fields: %+v", full)
	}
}

func TestSummarizeProjectsEveryMetadataField(t *testing.T) {
	record := Record{
		Ts: "2026-07-18T10:00:00Z", RequestID: "rid", SessionID: "sid",
		Protocol: "responses", Method: "POST", Path: "/v1/responses",
		Exposed: "route", CalledModel: "called", UpstreamModel: "upstream",
		Provider: "provider", Attempt: 3, Status: 429, LatencyMs: 123,
		RequestSize: 456, ResponseSize: 789, Shadow: true,
		RequestBody: "secret request", ResponseBody: "secret response",
		ResponseHeaders: `{"x-request-id":"secret"}`,
	}
	want := Summary{
		Ts: "2026-07-18T10:00:00Z", RequestID: "rid", SessionID: "sid",
		Protocol: "responses", Method: "POST", Path: "/v1/responses",
		Exposed: "route", CalledModel: "called", UpstreamModel: "upstream",
		Provider: "provider", Attempt: 3, Status: 429, LatencyMs: 123,
		RequestSize: 456, ResponseSize: 789, Shadow: true,
	}
	if got := Summarize(record); !reflect.DeepEqual(got, want) {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

func TestQueryRecordsScansAcrossFilenameTimestampDisorder(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260701-000000.log", []Record{{
		Ts: "2026-07-18T10:00:00Z", RequestID: "newer",
	}})
	writeRecordFile(t, dir, "requests-20260718-120000.log", []Record{{
		Ts: "2026-07-01T00:00:00Z", RequestID: "older",
	}})
	records, err := QueryRecords(dir, Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RequestID != "newer" {
		t.Errorf("top-1 = %+v, want newer record from older-named file", records)
	}
}

func TestQueryRecordsLargeLineDoesNotLoseFollowingRecord(t *testing.T) {
	dir := t.TempDir()
	records := []Record{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "oversized", RequestBody: strings.Repeat("x", 9*1024*1024)},
		{Ts: "2026-07-18T10:00:01Z", RequestID: "after", CalledModel: "glm-5"},
	}
	writeRecordFile(t, dir, "requests-20260720-120000.log", records)

	got, err := QueryRecords(dir, Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, record := range got {
		ids[record.RequestID] = true
	}
	if !ids["oversized"] || !ids["after"] {
		t.Errorf("ids = %v, want both oversized and following record", ids)
	}
}

func TestQueryRecordsReturnsNewestBoundedLimit(t *testing.T) {
	dir := t.TempDir()
	timestamp := func(second int64) string {
		return time.Unix(second, 0).UTC().Format(time.RFC3339)
	}
	write := func(name string, count, baseTimestamp int64) {
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		for i := int64(0); i < count; i++ {
			if err := encoder.Encode(Record{RequestID: name, Ts: timestamp(baseTimestamp + i)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), buffer.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("requests-20260701-000000.log", 50, 1000)
	write("requests-20260702-000000.log", 50, 2000)
	write("requests-20260703-000000.log", 5000, 3000)

	const limit = 100
	records, err := QueryRecords(dir, Filter{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != limit {
		t.Fatalf("records = %d, want %d", len(records), limit)
	}
	for i, record := range records {
		if record.RequestID != "requests-20260703-000000.log" {
			t.Errorf("record %d came from %q, want newest file", i, record.RequestID)
		}
		if i > 0 && record.Ts >= records[i-1].Ts {
			t.Errorf("timestamps not strictly descending at %d: %q then %q", i, records[i-1].Ts, record.Ts)
		}
	}
	if records[0].Ts != timestamp(7999) {
		t.Errorf("newest Ts = %q, want %q", records[0].Ts, timestamp(7999))
	}
	if records[len(records)-1].Ts != timestamp(7900) {
		t.Errorf("oldest retained Ts = %q, want %q", records[len(records)-1].Ts, timestamp(7900))
	}
}

func TestQueryRecordsContinuesAfterMalformedAndPartialJSONL(t *testing.T) {
	dir := t.TempDir()
	marshal := func(record Record) string {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	validOne := marshal(Record{Ts: "2026-07-18T10:00:00Z", RequestID: "valid-1"})
	validTwo := marshal(Record{Ts: "2026-07-18T10:00:01Z", RequestID: "valid-2"})
	validThree := marshal(Record{Ts: "2026-07-18T10:00:02Z", RequestID: "valid-no-newline"})
	content := strings.Join([]string{
		validOne,
		`{"request_id":bad-json}`,
		`{"request_id":"truncated"`,
		validTwo,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "requests-20260718-100000.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "requests-20260718-100001.log"),
		[]byte(`{"request_id":"partial-at-eof"`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "requests-20260718-100002.log"),
		[]byte(validThree),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	records, err := QueryRecords(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, record := range records {
		got[record.RequestID] = true
	}
	want := map[string]bool{"valid-1": true, "valid-2": true, "valid-no-newline": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("valid ids after corrupt JSONL = %v, want %v", got, want)
	}
}

func TestQueryRecordsMissingDirectoryReturnsError(t *testing.T) {
	_, err := QueryRecords(filepath.Join(t.TempDir(), "missing"), Filter{})
	if err == nil {
		t.Fatal("QueryRecords returned nil error for a missing directory")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error = %q, want missing path context", err)
	}
}

// peekLastRecordTs bounds a file by its last admissible record; every anomaly
// must report ok=false so the caller falls back to streaming.
func TestPeekLastRecordTs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	marshal := func(record Record) string {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	// Newline-terminated file: the last line's Ts.
	path := write("terminated.log", marshal(Record{Ts: "2026-07-18T10:00:00Z"})+"\n"+marshal(Record{Ts: "2026-07-18T10:01:00Z"})+"\n")
	if ts, ok := peekLastRecordTs(path); !ok || ts != "2026-07-18T10:01:00Z" {
		t.Errorf("terminated file = (%q, %v), want (2026-07-18T10:01:00Z, true)", ts, ok)
	}
	// An unterminated final line is still an admissible record for the
	// streaming reader, so it bounds the file.
	path = write("unterminated.log", marshal(Record{Ts: "2026-07-18T10:00:00Z"})+"\n"+marshal(Record{Ts: "2026-07-18T10:02:00Z"}))
	if ts, ok := peekLastRecordTs(path); !ok || ts != "2026-07-18T10:02:00Z" {
		t.Errorf("unterminated final line = (%q, %v), want (2026-07-18T10:02:00Z, true)", ts, ok)
	}
	// Anomalies → not ok: missing file, empty file, blank-only file, corrupt
	// last line, and a final line longer than the peek chunk.
	if _, ok := peekLastRecordTs(filepath.Join(dir, "missing.log")); ok {
		t.Error("missing file must not peek ok")
	}
	if _, ok := peekLastRecordTs(write("empty.log", "")); ok {
		t.Error("empty file must not peek ok")
	}
	if _, ok := peekLastRecordTs(write("blank.log", "\n\n")); ok {
		t.Error("blank-only file must not peek ok")
	}
	if _, ok := peekLastRecordTs(write("corrupt.log", marshal(Record{Ts: "2026-07-18T10:00:00Z"})+"\n"+`{"ts":`+"\n")); ok {
		t.Error("corrupt last line must not peek ok")
	}
	bigLine := marshal(Record{Ts: "2026-07-18T10:00:00Z", RequestBody: strings.Repeat("x", 200<<10)}) + "\n"
	if _, ok := peekLastRecordTs(write("bigline.log", bigLine)); ok {
		t.Error("final line longer than the peek chunk must not peek ok")
	}
}

// Early termination must return exactly the top-K the full scan would,
// whether the heap fills inside the newest file (older files skipped) or only
// across several files (older files still streamed because the heap is not
// full yet).
func TestQueryRecordsEarlyTerminationEquivalentResults(t *testing.T) {
	dir := t.TempDir()
	timestamp := func(second int64) string {
		return time.Unix(second, 0).UTC().Format(time.RFC3339)
	}
	write := func(name string, count, baseTimestamp int64) {
		t.Helper()
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		for i := int64(0); i < count; i++ {
			if err := encoder.Encode(Record{RequestID: name, Ts: timestamp(baseTimestamp + i)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), buffer.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("requests-20260701-000000.log", 50, 1000)
	write("requests-20260702-000000.log", 50, 2000)
	write("requests-20260703-000000.log", 200, 3000)

	// Heap fills inside the newest file: both older files are skippable.
	records, err := QueryRecords(dir, Filter{Limit: 150})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 150 {
		t.Fatalf("records = %d, want 150", len(records))
	}
	if records[0].Ts != timestamp(3199) || records[len(records)-1].Ts != timestamp(3050) {
		t.Errorf("top-150 span = %q..%q, want %q..%q",
			records[len(records)-1].Ts, records[0].Ts, timestamp(3050), timestamp(3199))
	}

	// Heap does NOT fill within the newest file (200 < 250): the middle
	// file's records still make the cut and must not be skipped away.
	records, err = QueryRecords(dir, Filter{Limit: 250})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 250 {
		t.Fatalf("records = %d, want 250", len(records))
	}
	if records[0].Ts != timestamp(3199) || records[len(records)-1].Ts != timestamp(2000) {
		t.Errorf("top-250 span = %q..%q, want %q..%q",
			records[len(records)-1].Ts, records[0].Ts, timestamp(2000), timestamp(3199))
	}

	// A From bound older than the two oldest files' newest records excludes
	// them entirely.
	from := time.Unix(2500, 0).UTC()
	records, err = QueryRecords(dir, Filter{From: from, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 200 {
		t.Fatalf("From query records = %d, want only the newest file's 200", len(records))
	}
	for _, record := range records {
		if record.Ts < timestamp(3000) {
			t.Errorf("From query returned %q, older than the bound", record.Ts)
		}
	}
}

// A peek anomaly (here: corrupt last line in an older file) must fall back to
// streaming even when the heap is already full — a record newer than the heap
// floor buried in that file (within-file disorder) must still make the top-K,
// exactly as the full scan would return it.
func TestQueryRecordsEarlyTerminationPeekFallback(t *testing.T) {
	dir := t.TempDir()
	marshal := func(record Record) string {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	ts := func(second int64) string {
		return time.Unix(second, 0).UTC().Format(time.RFC3339)
	}
	var newest bytes.Buffer
	for i := 0; i < 30; i++ {
		newest.WriteString(marshal(Record{RequestID: "new", Ts: ts(3000 + int64(i))}) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "requests-20260702-000000.log"), newest.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var older bytes.Buffer
	for i := 0; i < 20; i++ {
		older.WriteString(marshal(Record{RequestID: "old", Ts: ts(1000 + int64(i))}) + "\n")
	}
	// Newer than the heap floor but NOT the file's last record; a corrupt
	// last line then breaks the peek, forcing the streaming fallback.
	older.WriteString(marshal(Record{RequestID: "buried-new", Ts: ts(4000)}) + "\n")
	older.WriteString(`{"ts":`)
	if err := os.WriteFile(filepath.Join(dir, "requests-20260701-000000.log"), older.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := QueryRecords(dir, Filter{Limit: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 30 {
		t.Fatalf("records = %d, want 30", len(records))
	}
	if records[0].RequestID != "buried-new" {
		t.Errorf("top-1 = %+v, want the buried newer record from the peek-broken file", records[0])
	}
	for i, record := range records[1:] {
		if record.RequestID != "new" {
			t.Errorf("record %d = %q, want the 29 newest records of the newest file", i+1, record.RequestID)
		}
	}
}

// TestRawPrefilter pins the cheap byte gate that lets a RequestID/Session query
// skip json.Unmarshal on lines that cannot match.
func TestRawPrefilter(t *testing.T) {
	line := []byte(`{"request_id":"abc","session_id":"sess-1","response_body":"body"}`)
	tests := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"empty filter admits", Filter{}, true},
		{"request id present", Filter{RequestID: "abc"}, true},
		{"request id absent", Filter{RequestID: "zzz"}, false},
		{"session present", Filter{Session: "sess-1"}, true},
		{"session absent", Filter{Session: "nope"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := rawPrefilter(line, test.filter); got != test.want {
				t.Errorf("rawPrefilter = %v, want %v", got, test.want)
			}
		})
	}
}

// TestQueryRecordsRequestIDPrefilterCorrectness covers the byte gate's two
// hazards: a real match in an OLDER file must still be found (the gate must not
// stop the cross-file scan), and a newer record whose body merely CONTAINS the
// id string must not be returned (false positives fall through to the exact
// match check).
func TestQueryRecordsRequestIDPrefilterCorrectness(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260718-100000.log", []Record{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "target", Provider: "zhipu", Status: 200, RequestBody: `{"prompt":"hi"}`},
	})
	writeRecordFile(t, dir, "requests-20260718-110000.log", []Record{
		{Ts: "2026-07-18T11:00:00Z", RequestID: "other", Provider: "zhipu", Status: 200, RequestBody: `{"note":"target appears in this body"}`},
	})

	records, err := QueryRecords(dir, Filter{RequestID: "target", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RequestID != "target" {
		t.Fatalf("records = %+v, want the single older-file target record", records)
	}
}

// TestQueryRecordsRequestIDStopsAtFirstMatch pins the early stop: a request_id
// identifies exactly one record, so the scan must not keep decoding the rest of
// the file (or older files) after the match.
func TestQueryRecordsRequestIDStopsAtFirstMatch(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260718-100000.log", []Record{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "dup", Status: 200},
		{Ts: "2026-07-18T10:01:00Z", RequestID: "dup", Status: 500},
		{Ts: "2026-07-18T10:02:00Z", RequestID: "other", Status: 200},
	})
	records, err := QueryRecords(dir, Filter{RequestID: "dup", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != 200 {
		t.Fatalf("records = %+v, want only the first matching record", records)
	}
}

// TestQueryAgentFilterAndFacets pins the agent dimension of the Requests
// filter: the agent match is EXACT (not the substring match model/provider
// use, so one agent label cannot select another that contains it), the agent
// facet lists the distinct agents observed in the log, records with no agent
// contribute no facet option, and the facet is not narrowed by the request's
// own agent filter (the dropdown must stay reversible).
func TestQueryAgentFilterAndFacets(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260718-100000.log", []Record{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "a", Provider: "zhipu", Exposed: "glm-5.3", CalledModel: "glm-5.3", Agent: "claude-code", Status: 200},
		{Ts: "2026-07-18T10:01:00Z", RequestID: "b", Provider: "zhipu", Exposed: "glm-5.3", CalledModel: "glm-5.3", Agent: "claude-code-router", Status: 200},
		{Ts: "2026-07-18T10:02:00Z", RequestID: "c", Provider: "deepseek", Exposed: "deepseek-v4-pro", CalledModel: "deepseek-v4-pro", Agent: "codex", Status: 200},
		{Ts: "2026-07-18T10:03:00Z", RequestID: "d", Provider: "deepseek", Exposed: "deepseek-v4-pro", CalledModel: "deepseek-v4-pro", Status: 200},
	})

	summaries, facets, err := QuerySummariesWithFacets(dir, Filter{Agent: "claude-code", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "a" {
		t.Fatalf("agent filter = %+v, want only request a (exact match, not the claude-code-router prefix)", summaries)
	}
	if !reflect.DeepEqual(facets.Agents, []string{"claude-code", "claude-code-router", "codex"}) {
		t.Errorf("agents = %v, want all three despite the filter and without an empty label for the agent-less record", facets.Agents)
	}

	// An agent that only appears as a substring of a real label matches nothing.
	none, _, err := QuerySummariesWithFacets(dir, Filter{Agent: "claude", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("agent=claude = %+v, want no rows (exact match)", none)
	}

	// Agent ANDs with the other dimensions rather than replacing them.
	combined, _, err := QuerySummariesWithFacets(dir, Filter{Agent: "codex", Provider: "zhipu", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != 0 {
		t.Errorf("agent+provider = %+v, want no rows (codex never used zhipu)", combined)
	}
}

// TestQuerySummariesWithFacets pins the data-driven filter facets: distinct
// providers/models come from the scanned log, and they are NOT narrowed by the
// request's own model/provider filter (otherwise the UI dropdowns could lock
// the user into the current selection).
func TestQuerySummariesWithFacets(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260718-100000.log", []Record{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "a", Provider: "zhipu", Exposed: "glm-5.3", CalledModel: "glm-5.3", Status: 200},
		{Ts: "2026-07-18T10:01:00Z", RequestID: "b", Provider: "deepseek", Exposed: "deepseek-v4-pro", CalledModel: "deepseek-v4-pro", Status: 200},
		{Ts: "2026-07-18T10:02:00Z", RequestID: "c", Provider: "zhipu", Exposed: "glm-5.3", CalledModel: "glm-5.3", Status: 500},
	})

	summaries, facets, err := QuerySummariesWithFacets(dir, Filter{Provider: "zhipu", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2 (provider filter applied)", len(summaries))
	}
	if !reflect.DeepEqual(facets.Providers, []string{"deepseek", "zhipu"}) {
		t.Errorf("providers = %v, want both despite the filter", facets.Providers)
	}
	if !reflect.DeepEqual(facets.Models, []string{"deepseek-v4-pro", "glm-5.3"}) {
		t.Errorf("models = %v", facets.Models)
	}
	if !reflect.DeepEqual(facets.ProviderModels["zhipu"], []string{"glm-5.3"}) {
		t.Errorf("zhipu models = %v", facets.ProviderModels["zhipu"])
	}
	if !reflect.DeepEqual(facets.ProviderModels["deepseek"], []string{"deepseek-v4-pro"}) {
		t.Errorf("deepseek models = %v", facets.ProviderModels["deepseek"])
	}
}
