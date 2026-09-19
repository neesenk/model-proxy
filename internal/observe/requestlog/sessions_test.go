package requestlog

import (
	"bytes"
	"strings"
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

	sessions, err := SessionSummaries(dir, 100, 10, func(provider, model string, u Usage) float64 {
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
	// 300 chat (500 prompt − 200 cached) + 30 responses (40 − 10 cached)
	// input; 80 + 5 output. Cache is subtracted from input: ComputeCost
	// prices the buckets independently.
	if b.Usage.Input != 330 || b.Usage.Output != 85 {
		t.Fatalf("sess-b usage = %+v, want in 330 out 85", b.Usage)
	}
	if b.Usage.CacheRead != 210 {
		t.Fatalf("sess-b cache read = %d, want 210 (200 chat + 10 responses)", b.Usage.CacheRead)
	}
	if b.CostUSD != 330+2*85 {
		t.Fatalf("sess-b cost = %v, want %v", b.CostUSD, 330+2*85)
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

// TestSessionSummariesAgents pins the per-session agent list that links the
// Requests page's agent and session filters: one label per session in the
// normal case, every observed label when a client-supplied session id is
// reused across agents (most recently seen first, matching Providers), and no
// empty label for agent-less records.
func TestSessionSummariesAgents(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260801-100000.log", []Record{
		{Ts: "2026-08-01T10:00:00Z", RequestID: "1", SessionID: "sess-a", Provider: "zhipu", UpstreamModel: "glm-4.7", Agent: "claude-code", Status: 200},
		{Ts: "2026-08-01T10:01:00Z", RequestID: "2", SessionID: "sess-a", Provider: "zhipu", UpstreamModel: "glm-4.7", Agent: "claude-code", Status: 200},
		{Ts: "2026-08-01T10:02:00Z", RequestID: "3", SessionID: "sess-b", Provider: "codex", UpstreamModel: "gpt-5.6", Agent: "codex", Status: 200},
		// Same session id seen from a second client, and one record with no
		// agent at all (pre-dimension log line).
		{Ts: "2026-08-01T10:03:00Z", RequestID: "4", SessionID: "sess-b", Provider: "codex", UpstreamModel: "gpt-5.6", Agent: "pi", Status: 200},
		{Ts: "2026-08-01T10:04:00Z", RequestID: "5", SessionID: "sess-c", Provider: "zhipu", UpstreamModel: "glm-4.7", Status: 200},
	})

	sessions, err := SessionSummaries(dir, 100, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string][]string{}
	for _, s := range sessions {
		agents[s.SessionID] = s.Agents
	}
	if got := agents["sess-a"]; len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("sess-a agents = %v, want one deduplicated label", got)
	}
	// Records iterate newest-first, so the most recent agent leads.
	if got := agents["sess-b"]; len(got) != 2 || got[0] != "pi" || got[1] != "codex" {
		t.Errorf("sess-b agents = %v, want [pi codex]", got)
	}
	if got := agents["sess-c"]; len(got) != 0 {
		t.Errorf("sess-c agents = %v, want none (no agent recorded, not an empty label)", got)
	}
}

func TestExtractUsageShapes(t *testing.T) {
	cases := map[string]Usage{
		`{}`: {},
		`{"usage":{"input_tokens":7,"output_tokens":3}}`:      {Input: 7, Output: 3},
		`{"usage":{"prompt_tokens":9,"completion_tokens":4}}`: {Input: 9, Output: 4},
		// openai shapes include cached_tokens in prompt/input tokens — the
		// cached count moves to CacheRead and comes off Input so the
		// independent pricing buckets never double-bill it.
		`{"usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5}}}`:         {Input: 4, Output: 4, CacheRead: 5},
		`{"response":{"usage":{"input_tokens":20,"output_tokens":1,"input_tokens_details":{"cached_tokens":5}}}}`: {Input: 15, Output: 1, CacheRead: 5},
		// Pathological cached>input clamps instead of underflowing.
		`{"response":{"usage":{"input_tokens":2,"output_tokens":1,"input_tokens_details":{"cached_tokens":5}}}}`: {Input: 0, Output: 1, CacheRead: 5},
	}
	for body, want := range cases {
		if got := ExtractUsage(body); got != want {
			t.Errorf("ExtractUsage(%s) = %+v, want %+v", body, got, want)
		}
	}
}

// Streaming responses are recorded as SSE text — the dominant traffic shape.
// Usage frames repeat with cumulative counters, so extraction takes the
// per-field maximum across frames.
func TestExtractUsageSSE(t *testing.T) {
	anthropic := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":40,\"cache_creation_input_tokens\":10}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":25}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	if got, want := ExtractUsage(anthropic), (Usage{Input: 100, Output: 25, CacheRead: 40, CacheCreation: 10}); got != want {
		t.Errorf("anthropic SSE = %+v, want %+v", got, want)
	}

	chat := "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":80,\"prompt_tokens_details\":{\"cached_tokens\":200}}}\n\ndata: [DONE]\n\n"
	if got, want := ExtractUsage(chat), (Usage{Input: 300, Output: 80, CacheRead: 200}); got != want {
		t.Errorf("chat SSE = %+v, want %+v", got, want)
	}

	responses := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":40,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":10}}}}\n\n"
	if got, want := ExtractUsage(responses), (Usage{Input: 30, Output: 5, CacheRead: 10}); got != want {
		t.Errorf("responses SSE = %+v, want %+v", got, want)
	}

	// Per-chunk cumulative output (message_delta repeats usage with growing
	// output_tokens): the maximum wins, never the sum.
	cumulative := "data: {\"usage\":{\"output_tokens\":3}}\n\ndata: {\"usage\":{\"output_tokens\":9}}\n\ndata: {\"usage\":{\"output_tokens\":27}}\n\n"
	if got, want := ExtractUsage(cumulative), (Usage{Output: 27}); got != want {
		t.Errorf("cumulative SSE = %+v, want %+v", got, want)
	}

	// Multi-line data frames are joined per the SSE spec before parsing.
	multiline := "data: {\"usage\":{\"input_tokens\":12,\n"
	multiline += "data: \"output_tokens\":6}}\n\n"
	if got, want := ExtractUsage(multiline), (Usage{Input: 12, Output: 6}); got != want {
		t.Errorf("multi-line data SSE = %+v, want %+v", got, want)
	}

	// Stream with no usage object at all (error stream): zero.
	if got := ExtractUsage("data: {\"error\":\"boom\"}\n\n"); got != (Usage{}) {
		t.Errorf("no-usage SSE = %+v, want zero", got)
	}
}

// The UsageOnly projection powers /api/sessions: usage is parsed as records
// are read and bodies are stripped before the top-K heap retains them, so a
// 2000-record scan holds metadata-sized records instead of full bodies.
func TestQueryUsageOnlyStripsBodies(t *testing.T) {
	dir := t.TempDir()
	bigBody := strings.Repeat("a", 1<<16)
	writeRecordFile(t, dir, "requests-20260802-100000.log", []Record{
		{Ts: "2026-08-02T10:00:00Z", RequestID: "1", SessionID: "s", Exposed: "glm", Provider: "zhipu", Status: 200,
			RequestSize:  len(bigBody),
			RequestBody:  bigBody,
			ResponseBody: `{"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5}}`},
		{Ts: "2026-08-02T10:01:00Z", RequestID: "2", SessionID: "s", Exposed: "glm", Provider: "zhipu", Status: 200,
			RequestSize:  2,
			RequestBody:  "{}",
			ResponseBody: "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"},
	})
	records, err := query(dir, filePrefix, Filter{Limit: 10, UsageOnly: true}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	var total Usage
	for _, r := range records {
		if r.RequestBody != "" || r.ResponseBody != "" {
			t.Errorf("record %s kept bodies: req=%d resp=%d bytes", r.RequestID, len(r.RequestBody), len(r.ResponseBody))
		}
		if r.RequestSize == 0 {
			t.Errorf("record %s lost RequestSize", r.RequestID)
		}
		total.Input += r.ParsedUsage.Input
		total.Output += r.ParsedUsage.Output
		total.CacheRead += r.ParsedUsage.CacheRead
	}
	if total != (Usage{Input: 107, Output: 23, CacheRead: 5}) {
		t.Errorf("parsed usage total = %+v, want in=107 out=23 cacheRead=5 (JSON + SSE)", total)
	}
	// The projection never persists: ParsedUsage is absent from the wire
	// encoding (json:"-" and the manual line encoder both skip it).
	line := appendRecordLine(nil, &records[0])
	if bytes.Contains(line, []byte("parsed")) {
		t.Errorf("ParsedUsage leaked into the JSONL encoding: %s", line)
	}
}
