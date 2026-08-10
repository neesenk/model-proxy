package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/observe/requestlog"
)

// writeReqLog writes public requestlog.Record values as JSONL, matching the
// on-disk contract consumed by the root HTTP integration tests.
func writeReqLog(t *testing.T, dir, name string, records []requestlog.Record) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func requestLogMux(t *testing.T, dir string) *http.ServeMux {
	t.Helper()
	proxy := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"backend": {OpenAIBaseURL: "https://example.invalid", Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"client-model": {{Provider: "backend", Model: "backend-model"}},
		},
	})
	if dir != "" {
		proxy.reqLog = requestlog.New(requestlog.Options{
			Directory:    dir,
			MaxFileSize:  1 << 20,
			MaxBodyBytes: 1 << 10,
		})
	}
	web := NewWebServer(proxy, "test-config.yaml")
	mux := http.NewServeMux()
	web.Register(mux)
	return mux
}

func serveRequestLogList(t *testing.T, mux *http.ServeMux, query string) []map[string]json.RawMessage {
	t.Helper()
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/requests"+query, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/requests%s status = %d, want 200; body=%s", query, recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Enabled bool                         `json:"enabled"`
		Records []map[string]json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list response: %v; body=%s", err, recorder.Body.String())
	}
	if !payload.Enabled {
		t.Fatalf("GET /api/requests%s reported enabled=false", query)
	}
	return payload.Records
}

func requestIDs(t *testing.T, records []map[string]json.RawMessage) []string {
	t.Helper()
	ids := make([]string, 0, len(records))
	for _, record := range records {
		var id string
		if err := json.Unmarshal(record["request_id"], &id); err != nil {
			t.Fatalf("decode request_id: %v; record=%v", err, record)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestHandleRequests_ListAndDetail(t *testing.T) {
	dir := t.TempDir()
	const responseHeaders = `{"content-type":"application/json","x-request-id":"upstream-id"}`
	writeReqLog(t, dir, "requests-20260718-100000.log", []requestlog.Record{{
		Ts:              "2026-07-18T10:00:00Z",
		RequestID:       "request-a",
		Exposed:         "client-model",
		CalledModel:     "client-model",
		UpstreamModel:   "backend-model",
		Provider:        "backend",
		Status:          http.StatusOK,
		RequestBody:     "SECRET-REQUEST-BODY",
		ResponseBody:    "SECRET-RESPONSE-BODY",
		ResponseHeaders: responseHeaders,
	}})
	mux := requestLogMux(t, dir)

	listRecords := serveRequestLogList(t, mux, "")
	if ids := requestIDs(t, listRecords); len(ids) != 1 || ids[0] != "request-a" {
		t.Fatalf("list request IDs = %v, want [request-a]", ids)
	}
	for _, key := range []string{"request_body", "response_body", "response_headers"} {
		if _, ok := listRecords[0][key]; ok {
			t.Errorf("list summary contains sensitive detail field %q: %s", key, listRecords[0][key])
		}
	}
	listJSON, err := json.Marshal(listRecords)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"SECRET-REQUEST-BODY",
		"SECRET-RESPONSE-BODY",
		"upstream-id",
	} {
		if bytes.Contains(listJSON, []byte(secret)) {
			t.Errorf("list summary leaked %q: %s", secret, listJSON)
		}
	}

	detailRecorder := httptest.NewRecorder()
	mux.ServeHTTP(
		detailRecorder,
		httptest.NewRequest(http.MethodGet, "/api/requests/request-a", nil),
	)
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		Records []requestlog.Record `json:"records"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail response: %v; body=%s", err, detailRecorder.Body.String())
	}
	if len(detail.Records) != 1 {
		t.Fatalf("detail record count = %d, want 1", len(detail.Records))
	}
	record := detail.Records[0]
	if record.RequestBody != "SECRET-REQUEST-BODY" ||
		record.ResponseBody != "SECRET-RESPONSE-BODY" ||
		record.ResponseHeaders != responseHeaders {
		t.Errorf(
			"detail did not retain full fields: request=%q response=%q headers=%q",
			record.RequestBody,
			record.ResponseBody,
			record.ResponseHeaders,
		)
	}

	missingRecorder := httptest.NewRecorder()
	mux.ServeHTTP(
		missingRecorder,
		httptest.NewRequest(http.MethodGet, "/api/requests/missing", nil),
	)
	if missingRecorder.Code != http.StatusNotFound {
		t.Errorf("missing detail status = %d, want 404", missingRecorder.Code)
	}

	disabledMux := requestLogMux(t, "")
	disabledRecorder := httptest.NewRecorder()
	disabledMux.ServeHTTP(
		disabledRecorder,
		httptest.NewRequest(http.MethodGet, "/api/requests", nil),
	)
	if disabledRecorder.Code != http.StatusOK {
		t.Fatalf("disabled list status = %d, want 200", disabledRecorder.Code)
	}
	var disabledPayload struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(disabledRecorder.Body.Bytes(), &disabledPayload); err != nil {
		t.Fatalf("decode disabled list: %v", err)
	}
	if disabledPayload.Enabled {
		t.Errorf("request logging disabled response reported enabled=true: %s", disabledRecorder.Body.String())
	}
}

func TestHandleRequests_ShadowFilter(t *testing.T) {
	dir := t.TempDir()
	writeReqLog(t, dir, "requests-20260718-100000.log", []requestlog.Record{
		{
			Ts:        "2026-07-18T10:00:00Z",
			RequestID: "request-a",
			Exposed:   "client-model",
			Provider:  "primary",
			Status:    http.StatusOK,
		},
		{
			Ts:        "2026-07-18T10:00:01Z",
			RequestID: "shadow-request-a",
			Shadow:    true,
			Exposed:   "client-model",
			Provider:  "shadow",
			Status:    http.StatusOK,
		},
	})
	mux := requestLogMux(t, dir)

	all := serveRequestLogList(t, mux, "")
	if ids := requestIDs(t, all); len(ids) != 2 ||
		ids[0] != "shadow-request-a" || ids[1] != "request-a" {
		t.Errorf("unfiltered request IDs = %v, want [shadow-request-a request-a]", ids)
	}
	var shadow bool
	if err := json.Unmarshal(all[0]["shadow"], &shadow); err != nil || !shadow {
		t.Errorf("shadow summary flag = %s (err=%v), want true", all[0]["shadow"], err)
	}

	only := serveRequestLogList(t, mux, "?shadow=only")
	if ids := requestIDs(t, only); len(ids) != 1 || ids[0] != "shadow-request-a" {
		t.Errorf("shadow=only request IDs = %v, want [shadow-request-a]", ids)
	}

	exclude := serveRequestLogList(t, mux, "?shadow=exclude")
	if ids := requestIDs(t, exclude); len(ids) != 1 || ids[0] != "request-a" {
		t.Errorf("shadow=exclude request IDs = %v, want [request-a]", ids)
	}

	invalid := serveRequestLogList(t, mux, "?shadow=invalid")
	if ids := requestIDs(t, invalid); len(ids) != 2 {
		t.Errorf("invalid shadow filter request IDs = %v, want both records", ids)
	}
}
