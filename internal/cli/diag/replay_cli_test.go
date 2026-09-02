package diag

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/cli/clitest"
	"model-proxy/internal/observe/requestlog"
)

// TestDoReplay covers the replay core: fetch the stored request, re-send with the
// force-provider header, return the new backend's body. Errors: missing id (404)
// and empty body.
func TestDoReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/requests/ghost":
			http.Error(w, "no record", http.StatusNotFound)
		case "/api/requests/nobody":
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"records": []map[string]any{{"path": "/v1/responses", "request_body": ""}}})
		case "/api/requests/good":
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"records": []map[string]any{{"path": "/v1/responses", "request_body": `{"model":"glm","input":[]}`}}})
		case "/v1/responses":
			// The replay re-sends with the force-provider header.
			if fp := r.Header.Get("x-mp-force-provider"); fp != "zhipu" {
				t.Errorf("force-provider header=%q want zhipu", fp)
			}
			io.WriteString(w, `{"replayed":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	body, err := DoReplay(srv.URL, "good", "zhipu")
	if err != nil {
		t.Fatalf("doReplay good: %v", err)
	}
	if !strings.Contains(string(body), `"replayed":true`) {
		t.Errorf("replay body=%q", string(body))
	}
	if _, err := DoReplay(srv.URL, "ghost", "zhipu"); err == nil || !strings.Contains(err.Error(), "no request log") {
		t.Errorf("ghost err=%v want no request log", err)
	}
	if _, err := DoReplay(srv.URL, "nobody", "zhipu"); err == nil || !strings.Contains(err.Error(), "no captured request body") {
		t.Errorf("nobody err=%v want no captured body", err)
	}
}

// TestDoReplay_TruncatedBody (F6b): a record whose captured request body was
// truncated at max_body_bytes must NOT be replayed verbatim — it would send an
// incomplete request → a misleading upstream 400. doReplay refuses with a clear
// message pointing at max_body_bytes.
func TestDoReplay_TruncatedBody(t *testing.T) {
	logger := requestlog.New(requestlog.Options{MaxBodyBytes: 32})
	record := logger.BuildRecord(requestlog.Input{
		Timestamp:   time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC),
		RequestID:   "trunc",
		Path:        "/v1/responses",
		RequestBody: []byte(`{"model":"glm","input":"` + strings.Repeat("x", 50) + `"}`),
	})
	if !record.RequestBodyTruncated() {
		t.Fatalf("BuildRecord did not mark an over-limit request body as truncated: %+v", record)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clitest.WriteJSON(w, 200, map[string]any{"records": []requestlog.Record{*record}})
	}))
	defer srv.Close()
	_, err := DoReplay(srv.URL, "trunc", "zhipu")
	if err == nil || !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "max_body_bytes") {
		t.Errorf("truncated-body replay err=%v, want a truncated/max_body_bytes refusal", err)
	}
}

// TestDoReplay_RejectsShadowRecord: a shadow evaluation record (request_id
// "shadow-...") is a fire-and-forget log of a candidate backend, not a real
// client request with a route to re-enter — doReplay refuses it (replay_cmd.go
// guard) instead of re-sending it upstream.
func TestDoReplay_RejectsShadowRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"records": []map[string]any{
			{"request_id": "shadow-abc123", "path": "/v1/responses", "request_body": `{"model":"glm","input":[]}`},
		}})
	}))
	defer srv.Close()
	_, err := DoReplay(srv.URL, "shadow-abc123", "zhipu")
	if err == nil || !strings.Contains(err.Error(), "shadow") {
		t.Errorf("shadow-record replay err=%v, want a shadow refusal", err)
	}
}

// TestDoReplay_RejectsNonV1Path: a record whose path is not under /v1/ is not a
// chat-completion path the proxy can forward — doReplay refuses it
// (replay_cmd.go guard) instead of POSTing to a bogus path.
func TestDoReplay_RejectsNonV1Path(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"records": []map[string]any{
			{"request_id": "req-1", "path": "/api/status", "request_body": `{"model":"glm","input":[]}`},
		}})
	}))
	defer srv.Close()
	_, err := DoReplay(srv.URL, "req-1", "zhipu")
	if err == nil || !strings.Contains(err.Error(), "/v1/") {
		t.Errorf("non-/v1/ path replay err=%v, want a /v1/ refusal", err)
	}
}
