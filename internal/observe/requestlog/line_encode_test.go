package requestlog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestAppendRecordLineMatchesJSONMarshal pins the hand-rolled line encoder to
// the reference encoding/json output: the JSONL bytes must stay identical,
// including HTML escaping, control characters, invalid UTF-8 and U+2028/2029.
func TestAppendRecordLineMatchesJSONMarshal(t *testing.T) {
	tricky := []string{
		"",
		"plain ascii",
		"quote \" backslash \\ slash /",
		"<html>&amp;</html>",
		"line\nbreak\rtab\there",
		"null\x00byte\x1fcontrol",
		"valid unicode: héllo 世界",
		"line\u2028separator\u2029here",
		"invalid utf8: \xff\xfe\x80",
		"del \x7f char",
		strings.Repeat("y", 4096),
	}
	var records []*Record
	for i, s := range tricky {
		records = append(records, &Record{
			Ts:              "2026-08-16T10:00:00Z",
			Shadow:          i%2 == 0,
			RequestID:       fmt.Sprintf("rid-%d", i),
			SessionID:       s,
			Protocol:        "anthropic",
			Method:          "POST",
			Path:            "/v1/messages",
			CalledModel:     s,
			UpstreamModel:   "upstream-model",
			Exposed:         "route-model",
			Provider:        "provider-a",
			Attempt:         i,
			Status:          200 + i,
			LatencyMs:       int64(i * 7),
			RequestSize:     len(s),
			ResponseSize:    int64(len(s) * 2),
			RequestBody:     s,
			ResponseBody:    s,
			ResponseHeaders: map[bool]string{true: `{"content-type":"application/json"}`, false: ""}[i%3 == 0],
		})
	}
	for i, rec := range records {
		want, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record %d: %v", i, err)
		}
		want = append(want, '\n')
		if got := appendRecordLine(nil, rec); string(got) != string(want) {
			t.Errorf("record %d:\n got %q\nwant %q", i, got, want)
		}
	}
}

// TestAppendRecordLineReusesBuffer verifies encoding onto a retained buffer
// (the fileWriter steady state) leaves no residue from previous rows.
func TestAppendRecordLineReusesBuffer(t *testing.T) {
	buffer := appendRecordLine(nil, &Record{Ts: "t", RequestID: "long-first-record-id", RequestBody: strings.Repeat("x", 1024)})
	small := &Record{Ts: "t2", RequestID: "r"}
	got := appendRecordLine(buffer[:0], small)
	want, err := json.Marshal(small)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if string(got) != string(want) {
		t.Errorf("reused buffer row = %q, want %q", got, want)
	}
}
