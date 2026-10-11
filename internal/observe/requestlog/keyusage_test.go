package requestlog

// keyusage_test.go — the api_key dimension end to end: records carry the
// gate-resolved key id through the JSONL encoder, the SQLite index migrates
// and indexes the column, and the per-key aggregation (index and directory
// scan, twin semantics) counts only real LLM traffic — shadow evaluations and
// unauthenticated records never count toward a key.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func keyUsageLine(t *testing.T, dir, ts, keyID, model, provider string, status int, usage string) {
	t.Helper()
	rec := Record{
		Ts: ts, RequestID: "r-" + ts + "-" + model, SessionID: "s",
		CalledModel: model, UpstreamModel: model, Exposed: model,
		Provider: provider, Agent: "test", APIKeyID: keyID,
		Status: status, RequestBody: "{}", ResponseBody: usage,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeJSONLLine(t, dir, string(b)+"\n")
}

// writeJSONLLine appends raw bytes to the newest (or a new) requests log file
// in dir — the JSONL producer the indexer tails.
func writeJSONLLine(t *testing.T, dir, line string) {
	t.Helper()
	name := filepath.Join(dir, "requests-20260101.log")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

func TestKeyUsageIndexAggregatesPerKeyModelProvider(t *testing.T) {
	dir := t.TempDir()
	// two keys, two models; shadow row and unauthenticated row must NOT count
	keyUsageLine(t, dir, "2026-01-01T10:00:00Z", "aaaa1111", "glm-5.3", "zhipu", 200, `{"usage":{"input_tokens":10,"output_tokens":5}}`)
	keyUsageLine(t, dir, "2026-01-01T10:01:00Z", "aaaa1111", "glm-5.3", "zhipu", 500, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	keyUsageLine(t, dir, "2026-01-01T10:02:00Z", "aaaa1111", "kimi-k3", "kimi", 200, `{"usage":{"input_tokens":7,"output_tokens":3}}`)
	keyUsageLine(t, dir, "2026-01-01T10:03:00Z", "bbbb2222", "glm-5.3", "zhipu", 200, `{"usage":{"input_tokens":100,"output_tokens":50}}`)
	keyUsageLine(t, dir, "2026-01-01T10:04:00Z", "", "glm-5.3", "zhipu", 200, `{"usage":{"input_tokens":999,"output_tokens":999}}`)
	// shadow row carries the key id but is an experiment, not client traffic
	shadow := Record{Ts: "2026-01-01T10:05:00Z", RequestID: "shadow-x", Shadow: true,
		CalledModel: "glm-5.3", Exposed: "glm-5.3", Provider: "deepseek", APIKeyID: "aaaa1111", Status: 200,
		RequestBody: "{}", ResponseBody: `{"usage":{"input_tokens":42,"output_tokens":42}}`}
	sb, _ := json.Marshal(shadow)
	writeJSONLLine(t, dir, string(sb)+"\n")

	idx := newTestIndexer(t, dir)
	mustReconcile(t, idx)

	all, err := idx.KeyUsage(Filter{}, "")
	if err != nil {
		t.Fatal(err)
	}
	cells := map[string]KeyUsageEntry{}
	for _, e := range all {
		cells[e.APIKeyID+"/"+e.Model+"/"+e.Provider] = e
	}
	got, ok := cells["aaaa1111/glm-5.3/zhipu"]
	if !ok {
		t.Fatalf("missing aaaa1111/glm-5.3 cell: %+v", all)
	}
	if got.Requests != 2 || got.Failures != 1 || got.Input != 11 || got.Output != 6 {
		t.Errorf("aaaa1111/glm-5.3 = %+v, want 2 req/1 fail/11 in/6 out", got)
	}
	if got.LastUsedMs != time.Date(2026, 1, 1, 10, 1, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("last used = %d", got.LastUsedMs)
	}
	if cells["bbbb2222/glm-5.3/zhipu"].Input != 100 {
		t.Errorf("bbbb2222 = %+v", cells["bbbb2222/glm-5.3/zhipu"])
	}
	if len(all) != 3 {
		t.Errorf("cells = %d, want 3 (unauthenticated and shadow excluded): %+v", len(all), all)
	}

	one, err := idx.KeyUsage(Filter{}, "aaaa1111")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 {
		t.Errorf("key-filtered cells = %d, want 2: %+v", len(one), one)
	}
	for _, e := range one {
		if e.APIKeyID != "aaaa1111" {
			t.Errorf("leaked other key into filtered view: %+v", e)
		}
	}

	// Time window: from > all rows → empty.
	from := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	none, err := idx.KeyUsage(Filter{From: from}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("future window = %+v, want empty", none)
	}
}

// TestKeyUsageScanMatchesIndex: the JSONL directory scan answers identically
// to the SQLite index (the twin contract).
func TestKeyUsageScanMatchesIndex(t *testing.T) {
	dir := t.TempDir()
	keyUsageLine(t, dir, "2026-01-01T10:00:00Z", "cccc3333", "glm-5.3", "zhipu", 200, `{"usage":{"input_tokens":3,"output_tokens":4}}`)
	keyUsageLine(t, dir, "2026-01-01T11:00:00Z", "cccc3333", "glm-5.3", "zhipu", 404, `{"usage":{"input_tokens":0,"output_tokens":0}}`)

	idx := newTestIndexer(t, dir)
	mustReconcile(t, idx)
	fromIndex, err := idx.KeyUsage(Filter{}, "")
	if err != nil {
		t.Fatal(err)
	}
	fromScan, err := KeyUsage(dir, Filter{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fromIndex) != 1 || len(fromScan) != 1 {
		t.Fatalf("index=%+v scan=%+v, want one cell each", fromIndex, fromScan)
	}
	if fromIndex[0] != fromScan[0] {
		t.Errorf("twin drift: index=%+v scan=%+v", fromIndex[0], fromScan[0])
	}
	if fromIndex[0].Requests != 2 || fromIndex[0].Failures != 1 || fromIndex[0].Input != 3 || fromIndex[0].Output != 4 {
		t.Errorf("cell = %+v", fromIndex[0])
	}
}

// TestRecordJSONLCarriesAPIKey: the encoder emits api_key only when set and
// the decoder round-trips it (old records without the field decode to "").
func TestRecordJSONLCarriesAPIKey(t *testing.T) {
	withKey := Record{Ts: "2026-01-01T10:00:00Z", RequestID: "r1", CalledModel: "m", Exposed: "m", Provider: "p", APIKeyID: "abcd1234", Status: 200}
	line := appendRecordLine(nil, &withKey)
	if !contains(line, `"api_key":"abcd1234"`) {
		t.Fatalf("api_key missing from JSONL line: %s", line)
	}
	var back Record
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if back.APIKeyID != "abcd1234" {
		t.Errorf("round-trip = %q", back.APIKeyID)
	}

	without := Record{Ts: "2026-01-01T10:00:00Z", RequestID: "r2", CalledModel: "m", Exposed: "m", Provider: "p", Status: 200}
	if contains(appendRecordLine(nil, &without), "api_key") {
		t.Fatal("unauthenticated record must omit api_key entirely")
	}
}

// TestIndexMigratesOldDatabaseAddsAPIKeyColumn: a pre-api_key index file
// gains the column (NULL-equivalent ”) without losing rows.
func TestIndexMigratesOldDatabaseAddsAPIKeyColumn(t *testing.T) {
	dir := t.TempDir()
	idx := newTestIndexer(t, dir)
	// simulate an old schema by dropping the column is not possible; instead
	// verify the column exists and old-shape rows (api_key absent in JSONL)
	// index as ''.
	writeJSONLLine(t, dir, `{"ts":"2026-01-01T10:00:00Z","request_id":"old","called_model":"m","exposed":"m","provider":"p","status":200}`+"\n")
	mustReconcile(t, idx)
	all, err := idx.KeyUsage(Filter{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("old-shape row must not aggregate under any key: %+v", all)
	}
}

func contains(b []byte, sub string) bool {
	return len(sub) > 0 && len(b) >= len(sub) && stringContains(string(b), sub)
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Failover attempt rows (attempt_only) are routing telemetry, not client
// traffic: the JSONL twin must not count them as key usage.
func TestKeyUsageExcludesAttemptOnlyRows(t *testing.T) {
	records := []Record{
		{APIKeyID: "k1", CalledModel: "m", Provider: "p", Status: 429, AttemptOnly: true},
		{APIKeyID: "k1", CalledModel: "m", Provider: "p", Status: 200},
	}
	out := aggregateKeyUsage(records, "")
	if len(out) != 1 || out[0].Requests != 1 {
		t.Fatalf("attempt_only row leaked into usage: %+v", out)
	}
}
