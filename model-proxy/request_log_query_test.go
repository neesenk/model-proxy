package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeReqLog writes records as JSONL into a requests-*.log file in dir.
func writeReqLog(t *testing.T, dir, name string, recs []requestLogRecord) {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQueryRequestRecords_FiltersAndLimit(t *testing.T) {
	dir := t.TempDir()
	writeReqLog(t, dir, "requests-20260718-100000.log", []requestLogRecord{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "a", Exposed: "glm", CalledModel: "glm", UpstreamModel: "glm-4", Provider: "zhipu", Status: 200, LatencyMs: 120},
		{Ts: "2026-07-18T10:01:00Z", RequestID: "b", Exposed: "glm", CalledModel: "glm", UpstreamModel: "glm-4", Provider: "deepseek", Status: 500, LatencyMs: 30},
		{Ts: "2026-07-18T10:02:00Z", RequestID: "c", Exposed: "codex", CalledModel: "codex-1", UpstreamModel: "gpt-5", Provider: "codex", Status: 200, LatencyMs: 90},
	})

	// No filter → all 3, newest first.
	got, err := queryRequestRecords(dir, recordFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].RequestID != "c" {
		t.Errorf("no-filter = %d rows, first=%s want 3 / c (newest first)", len(got), firstID(got))
	}

	// Filter provider=zhipu → only the zhipu record.
	got, _ = queryRequestRecords(dir, recordFilter{Provider: "zhipu", Limit: 100})
	if len(got) != 1 || got[0].RequestID != "a" {
		t.Errorf("provider filter = %+v want [a]", got)
	}

	// Filter model glm (matches exposed/called/upstream) → 2 records (a, b).
	got, _ = queryRequestRecords(dir, recordFilter{Model: "glm", Limit: 100})
	if len(got) != 2 {
		t.Errorf("model=glm = %d want 2 (a,b)", len(got))
	}

	// Errors only → the 500.
	got, _ = queryRequestRecords(dir, recordFilter{ErrorsOnly: true, Limit: 100})
	if len(got) != 1 || got[0].Status != 500 {
		t.Errorf("errors only = %+v want the 500", got)
	}

	// Exact status.
	got, _ = queryRequestRecords(dir, recordFilter{Status: 200, Limit: 100})
	if len(got) != 2 {
		t.Errorf("status=200 = %d want 2", len(got))
	}

	// RequestID lookup.
	got, _ = queryRequestRecords(dir, recordFilter{RequestID: "b", Limit: 100})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Errorf("request_id=b = %+v want [b]", got)
	}

	// Limit caps (newest first).
	got, _ = queryRequestRecords(dir, recordFilter{Limit: 2})
	if len(got) != 2 || got[0].RequestID != "c" || got[1].RequestID != "b" {
		t.Errorf("limit=2 = %+v want [c,b]", got)
	}
}

func firstID(rs []requestLogRecord) string {
	if len(rs) == 0 {
		return ""
	}
	return rs[0].RequestID
}

// TestRecordFilterMatches_Shadow: the Shadow tri-state filters on the record's
// Shadow bool ("" = all, "only" = shadow only, "exclude" = no shadow).
func TestRecordFilterMatches_Shadow(t *testing.T) {
	shadow := requestLogRecord{Ts: "2026-07-18T10:00:00Z", RequestID: "shadow-a", Shadow: true}
	primary := requestLogRecord{Ts: "2026-07-18T10:00:00Z", RequestID: "a"}
	cases := []struct {
		name   string
		filter string
		rec    requestLogRecord
		want   bool
	}{
		{"empty keeps shadow", "", shadow, true},
		{"empty keeps primary", "", primary, true},
		{"only keeps shadow", "only", shadow, true},
		{"only drops primary", "only", primary, false},
		{"exclude drops shadow", "exclude", shadow, false},
		{"exclude keeps primary", "exclude", primary, true},
	}
	for _, c := range cases {
		if got := (recordFilter{Shadow: c.filter}).matches(c.rec); got != c.want {
			t.Errorf("%s: matches() = %v want %v", c.name, got, c.want)
		}
	}
}

// TestHandleRequests_ListAndDetail: the /api/requests list omits bodies; the
// /api/requests/<id> detail includes them; a missing id 404s; logging-off reports
// enabled=false.
func TestHandleRequests_ListAndDetail(t *testing.T) {
	dir := t.TempDir()
	writeReqLog(t, dir, "requests-20260718-100000.log", []requestLogRecord{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "a", Exposed: "glm", Provider: "zhipu", Status: 200, RequestBody: "SECRET-REQ", ResponseBody: "SECRET-RESP"},
	})

	p := NewProxy(&Config{
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "zhipu", Model: "glm"}}},
	})
	p.reqLog = &requestLogger{dir: dir} // no goroutine; only .directory() is used
	w := newWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.register(mux)

	// List → summaries, NO bodies.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests", nil))
	if rec.Code != 200 {
		t.Fatalf("list status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"request_id":"a"`) {
		t.Errorf("list missing enabled/record: %s", body)
	}
	if strings.Contains(body, "SECRET-REQ") {
		t.Errorf("list must OMIT bodies, got: %s", body)
	}

	// Detail → full record WITH bodies.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/requests/a", nil))
	if rec2.Code != 200 {
		t.Fatalf("detail status=%d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "SECRET-REQ") || !strings.Contains(rec2.Body.String(), "SECRET-RESP") {
		t.Errorf("detail missing bodies: %s", rec2.Body.String())
	}

	// Unknown id → 404.
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/api/requests/ghost", nil))
	if rec3.Code != 404 {
		t.Errorf("unknown id status=%d want 404", rec3.Code)
	}

	// Logging off → enabled=false.
	p2 := NewProxy(&Config{
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "zhipu", Model: "glm"}}},
	})
	w2 := newWebServer(p2, "test-config.yaml")
	mux2 := http.NewServeMux()
	w2.register(mux2)
	rec4 := httptest.NewRecorder()
	mux2.ServeHTTP(rec4, httptest.NewRequest("GET", "/api/requests", nil))
	if !strings.Contains(rec4.Body.String(), `"enabled":false`) {
		t.Errorf("logging off should report enabled=false: %s", rec4.Body.String())
	}
}

// TestHandleRequests_ShadowFilter: /api/requests?shadow=only returns only shadow
// records, shadow=exclude drops them, and summaries carry the shadow flag.
func TestHandleRequests_ShadowFilter(t *testing.T) {
	dir := t.TempDir()
	writeReqLog(t, dir, "requests-20260718-100000.log", []requestLogRecord{
		{Ts: "2026-07-18T10:00:00Z", RequestID: "a", Exposed: "glm", Provider: "zhipu", Status: 200},
		{Ts: "2026-07-18T10:00:01Z", RequestID: "shadow-a", Exposed: "glm", Provider: "deepseek", Status: 200, Shadow: true},
	})

	p := NewProxy(&Config{
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "zhipu", Model: "glm"}}},
	})
	p.reqLog = &requestLogger{dir: dir} // no goroutine; only .directory() is used
	w := newWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.register(mux)

	get := func(query string) string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests?"+query, nil))
		if rec.Code != 200 {
			t.Fatalf("list ?%s status=%d", query, rec.Code)
		}
		return rec.Body.String()
	}

	// No filter → both records; the shadow one carries "shadow":true.
	body := get("")
	if !strings.Contains(body, `"request_id":"a"`) || !strings.Contains(body, `"request_id":"shadow-a"`) {
		t.Errorf("no-filter list missing records: %s", body)
	}
	if !strings.Contains(body, `"shadow":true`) {
		t.Errorf("summary must mark shadow records: %s", body)
	}

	// shadow=only → only the shadow record.
	body = get("shadow=only")
	if !strings.Contains(body, `"request_id":"shadow-a"`) || strings.Contains(body, `"request_id":"a"`) {
		t.Errorf("shadow=only = %s want only the shadow record", body)
	}

	// shadow=exclude → only the primary record.
	body = get("shadow=exclude")
	if !strings.Contains(body, `"request_id":"a"`) || strings.Contains(body, `"request_id":"shadow-a"`) {
		t.Errorf("shadow=exclude = %s want only the primary record", body)
	}

	// Invalid value → ignored (both records).
	body = get("shadow=bogus")
	if !strings.Contains(body, `"request_id":"a"`) || !strings.Contains(body, `"request_id":"shadow-a"`) {
		t.Errorf("shadow=bogus should be ignored (all records): %s", body)
	}
}
