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

	metadata, err := query(dir, Filter{Limit: 1}, true)
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
