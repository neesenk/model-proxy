package requestlog

import (
	"testing"
)

func TestSessionSummariesAggregatesAndCosts(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260801-100000.log", []Record{
		{Ts: "2026-08-01T10:00:00Z", RequestID: "1", SessionID: "sess-a", Exposed: "glm", CalledModel: "glm", UpstreamModel: "glm-4.7", Provider: "zhipu", Status: 200,
			ResponseBody: `{"usage":{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":300,"cache_creation_input_tokens":100}}`},
		{Ts: "2026-08-01T10:01:00Z", RequestID: "2", SessionID: "sess-a", Exposed: "glm", CalledModel: "glm", UpstreamModel: "glm-4.7", Provider: "deepseek", Status: 500,
			ResponseBody: `{"error":"boom"}`},
		{Ts: "2026-08-01T10:02:00Z", RequestID: "3", SessionID: "sess-a", Shadow: true, Exposed: "glm", UpstreamModel: "glm-4.7", Provider: "deepseek", Status: 200,
			ResponseBody: `{"usage":{"input_tokens":50,"output_tokens":10}}`},
		{Ts: "2026-08-01T11:00:00Z", RequestID: "4", SessionID: "sess-b", Exposed: "codex", UpstreamModel: "gpt-5.6", Provider: "codex", Status: 200,
			ResponseBody: `{"usage":{"prompt_tokens":500,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":200}}}`},
		{Ts: "2026-08-01T11:01:00Z", RequestID: "5", SessionID: "sess-b", Exposed: "codex", UpstreamModel: "gpt-5.6", Provider: "codex", Status: 200,
			ResponseBody: `{"id":"r","status":"completed","response":{"usage":{"input_tokens":40,"output_tokens":5,"input_tokens_details":{"cached_tokens":10}}}}`},
		// No session id: excluded from the session view.
		{Ts: "2026-08-01T12:00:00Z", RequestID: "6", Exposed: "glm", UpstreamModel: "glm-4.7", Provider: "zhipu", Status: 200,
			ResponseBody: `{"usage":{"input_tokens":9999,"output_tokens":9999}}`},
	})

	sessions, err := SessionSummaries(dir, 100, 10, func(model string, u Usage) float64 {
		// Deterministic stub: $1 per input token, $2 per output token.
		return float64(u.Input) + 2*float64(u.Output)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2: %+v", len(sessions), sessions)
	}
	// Most recently active first.
	if sessions[0].SessionID != "sess-b" || sessions[1].SessionID != "sess-a" {
		t.Fatalf("order = %s, %s; want sess-b first", sessions[0].SessionID, sessions[1].SessionID)
	}
	b := sessions[0]
	if b.Requests != 2 || b.FirstTs != "2026-08-01T11:00:00Z" || b.LastTs != "2026-08-01T11:01:00Z" {
		t.Fatalf("sess-b summary = %+v", b)
	}
	// 500 chat + 40 responses input; 80 + 5 output.
	if b.Usage.Input != 540 || b.Usage.Output != 85 {
		t.Fatalf("sess-b usage = %+v, want in 540 out 85", b.Usage)
	}
	if b.Usage.CacheRead != 210 {
		t.Fatalf("sess-b cache read = %d, want 210 (200 chat + 10 responses)", b.Usage.CacheRead)
	}
	if b.CostUSD != 540+2*85 {
		t.Fatalf("sess-b cost = %v, want %v", b.CostUSD, 540+2*85)
	}
	a := sessions[1]
	if a.Requests != 3 || a.ShadowRequests != 1 || a.Errors != 1 {
		t.Fatalf("sess-a summary = %+v", a)
	}
	if a.Usage.Input != 1050 || a.Usage.Output != 210 || a.Usage.CacheRead != 300 || a.Usage.CacheCreation != 100 {
		t.Fatalf("sess-a usage = %+v", a.Usage)
	}
	// Providers are listed most-recently-seen first (records newest→oldest).
	if len(a.Providers) != 2 || a.Providers[0] != "deepseek" || a.Providers[1] != "zhipu" {
		t.Fatalf("sess-a providers = %v", a.Providers)
	}

	// Limit cut keeps the most active session.
	limited, err := SessionSummaries(dir, 100, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].SessionID != "sess-b" {
		t.Fatalf("limited = %+v", limited)
	}

	// Session filter on QueryRecords.
	records, err := QueryRecords(dir, Filter{Session: "sess-a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("session filter = %d records, want 3", len(records))
	}
}

func TestExtractUsageShapes(t *testing.T) {
	cases := map[string]Usage{
		`{}`: {},
		`{"usage":{"input_tokens":7,"output_tokens":3}}`:                                                         {Input: 7, Output: 3},
		`{"usage":{"prompt_tokens":9,"completion_tokens":4}}`:                                                    {Input: 9, Output: 4},
		`{"response":{"usage":{"input_tokens":2,"output_tokens":1,"input_tokens_details":{"cached_tokens":5}}}}`: {Input: 2, Output: 1, CacheRead: 5},
	}
	for body, want := range cases {
		if got := ExtractUsage(body); got != want {
			t.Errorf("ExtractUsage(%s) = %+v, want %+v", body, got, want)
		}
	}
}
