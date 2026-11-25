package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- request_log_adapter_test.go ----

// The LogCtx → requestlog.Input mapping unit tests moved to internal/forward
// (attempt_test.go) with the pipeline that owns BuildRequestLogInput.

// ---- request_log_http_test.go ----

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
	proxy := newTestProxy(t, &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"backend": {OpenAIBaseURL: "https://example.invalid", Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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

// ---- request_log_integration_test.go ----

// newReqLogProxy wires a real requestlog.Logger into a test Proxy. Call the
// returned shutdown function before reading the JSONL files so every accepted
// record has been drained to disk.
func newReqLogProxy(t *testing.T, cfg *configdomain.Config) (*Proxy, string, func()) {
	t.Helper()
	proxy := newTestProxy(t, cfg)
	dir := t.TempDir()
	logger := requestlog.New(requestlog.Options{
		Directory:    dir,
		MaxFileSize:  1 << 30,
		MaxBodyBytes: 1 << 20,
	})
	go logger.Run()
	proxy.reqLog = logger
	t.Cleanup(func() {
		// Finite log producers belong to Proxy lifecycle and
		// must finish before the logger drains.
		proxy.Close()
		logger.Shutdown()
	})
	return proxy, dir, logger.Shutdown
}

func allRecords(t *testing.T, dir string) []requestlog.Record {
	t.Helper()
	records, err := requestlog.QueryRecords(dir, requestlog.Filter{Limit: 1000})
	if err != nil {
		t.Fatalf("query request log: %v", err)
	}
	return records
}

func TestForward_RequestLog_CapturesBodies_NonSSE(t *testing.T) {
	const (
		exposedModel  = "client-visible-model"
		upstreamModel = "backend-model"
		responseBody  = `{"id":"resp_1","object":"response","status":"completed","output":[]}`
		requestBody   = `{"model":"client-visible-model","input":[{"role":"user","content":"hello"}]}`
	)

	var upstreamBody []byte
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		upstreamPath = request.URL.Path
		var err error
		upstreamBody, err = io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			exposedModel: {{
				Provider: "backend",
				Model:    upstreamModel,
				Protocol: "responses",
			}},
		},
	}
	proxy, dir, shutdown := newReqLogProxy(t, cfg)
	proxy.providers["backend"] = &testProv{key: "test-token"}
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	code, clientBody := post(t, server.URL+"/v1/responses", requestBody)
	if code != http.StatusOK {
		t.Fatalf("client status = %d, want 200; body=%s", code, clientBody)
	}
	if clientBody != responseBody {
		t.Fatalf("client body = %q, want %q", clientBody, responseBody)
	}

	var upstreamJSON map[string]any
	if err := json.Unmarshal(upstreamBody, &upstreamJSON); err != nil {
		t.Fatalf("upstream request is not JSON: %v; body=%q", err, upstreamBody)
	}
	if got := upstreamJSON["model"]; got != upstreamModel {
		t.Fatalf("upstream model = %v, want %q; body=%s", got, upstreamModel, upstreamBody)
	}
	if bytes.Equal(upstreamBody, []byte(requestBody)) {
		t.Fatalf("upstream body was not rewritten: %s", upstreamBody)
	}
	if upstreamPath != "/responses" {
		t.Fatalf("upstream path = %q, want /responses", upstreamPath)
	}

	// The request-log record is completed at response-body Close inside the
	// post-copy commit path — a Content-Length client can finish reading
	// first. Wait for the commit metrics (recorded right after) so shutdown()
	// below actually drains the record.
	awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "backend", Model: upstreamModel})
	shutdown()
	records := allRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("request log record count = %d, want 1", len(records))
	}
	record := records[0]
	if record.RequestBody != requestBody {
		t.Errorf("logged request body = %q, want original client bytes %q", record.RequestBody, requestBody)
	}
	if record.RequestBody == string(upstreamBody) {
		t.Errorf("logged request body captured rewritten upstream bytes instead of original client bytes: %s", upstreamBody)
	}
	if record.ResponseBody != string(clientBody) {
		t.Errorf("logged response body = %q, want exact client bytes %q", record.ResponseBody, clientBody)
	}
	if record.Exposed != exposedModel || record.CalledModel != exposedModel || record.UpstreamModel != upstreamModel {
		t.Errorf(
			"logged exposed/called/upstream models = %q/%q/%q, want %q/%q/%q",
			record.Exposed,
			record.CalledModel,
			record.UpstreamModel,
			exposedModel,
			exposedModel,
			upstreamModel,
		)
	}
	if record.Provider != "backend" || record.Protocol != "responses" || record.Status != http.StatusOK {
		t.Errorf(
			"logged provider/protocol/status = %q/%q/%d, want backend/responses/200",
			record.Provider,
			record.Protocol,
			record.Status,
		)
	}
	if record.RequestID == "" {
		t.Error("logged request ID is empty")
	}
}

func TestForward_RequestLog_CapturesBodies_SSE(t *testing.T) {
	const (
		backendStream = "data: {\"model\":\"backend-model\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
			"data: {\"model\":\"backend-model\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"
		requestBody = `{"model":"client-visible-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	)

	var upstreamBody []byte
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		upstreamPath = request.URL.Path
		var err error
		upstreamBody, err = io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, backendStream)
	}))
	defer upstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"client-visible-model": {{
				Provider: "backend",
				Model:    "backend-model",
				Protocol: "openai",
			}},
		},
	}
	proxy, dir, shutdown := newReqLogProxy(t, cfg)
	proxy.providers["backend"] = &testProv{key: "test-token"}
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	code, clientBody := post(t, server.URL+"/v1/messages", requestBody)
	if code != http.StatusOK {
		t.Fatalf("client status = %d, want 200; body=%s", code, clientBody)
	}
	if clientBody == backendStream {
		t.Fatalf("client received unconverted backend stream:\n%s", clientBody)
	}
	for _, want := range []string{
		"event: message_start",
		"event: content_block_delta",
		`"text":"hello"`,
		"event: message_stop",
	} {
		if !strings.Contains(clientBody, want) {
			t.Errorf("converted client stream missing %q:\n%s", want, clientBody)
		}
	}
	events := parseSSE(clientBody)
	if got := sseCount(events, "message_stop"); got != 1 {
		t.Fatalf("converted message_stop count = %d, want 1:\n%s", got, clientBody)
	}
	assertNoSSEError(t, events)

	var upstreamJSON map[string]any
	if err := json.Unmarshal(upstreamBody, &upstreamJSON); err != nil {
		t.Fatalf("converted upstream request is not JSON: %v; body=%q", err, upstreamBody)
	}
	if got := upstreamJSON["model"]; got != "backend-model" {
		t.Fatalf("upstream model = %v, want backend-model; body=%s", got, upstreamBody)
	}
	if _, ok := upstreamJSON["messages"]; !ok {
		t.Fatalf("upstream request was not converted to chat messages: %s", upstreamBody)
	}
	if upstreamPath != "/chat/completions" {
		t.Fatalf("upstream path = %q, want /chat/completions", upstreamPath)
	}

	// Same commit-path race as the non-SSE variant: the upstream wrote the
	// stream in one shot (Content-Length), so the client can finish before
	// the handler closes the captured body and enqueues the record.
	awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "backend", Model: "backend-model"})
	shutdown()
	records := allRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("request log record count = %d, want 1", len(records))
	}
	record := records[0]
	if record.RequestBody != requestBody {
		t.Errorf("logged request body = %q, want original Anthropic client bytes %q", record.RequestBody, requestBody)
	}
	if record.ResponseBody != string(clientBody) {
		t.Errorf("logged response body differs from exact client bytes:\nlogged=%q\nclient=%q", record.ResponseBody, clientBody)
	}
	if record.ResponseBody == backendStream {
		t.Errorf("logged response body captured backend Chat SSE instead of converted client SSE:\n%s", record.ResponseBody)
	}
	if record.ResponseSize != int64(len(clientBody)) {
		t.Errorf("logged response size = %d, want client byte count %d", record.ResponseSize, len(clientBody))
	}
	if record.Protocol != "anthropic" || record.UpstreamModel != "backend-model" {
		t.Errorf(
			"logged protocol/upstream model = %q/%q, want anthropic/backend-model",
			record.Protocol,
			record.UpstreamModel,
		)
	}
}

func TestForward_RequestLog_NilLoggerPassThrough(t *testing.T) {
	const responseBody = `{"ok":true}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"client-model": {{Provider: "backend", Model: "client-model", Protocol: "responses"}},
		},
	}
	proxy := newTestProxy(t, cfg)
	proxy.providers["backend"] = &testProv{key: "test-token"}
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	code, body := post(t, server.URL+"/v1/responses", `{"model":"client-model","input":[]}`)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 with request logging disabled", code)
	}
	if body != responseBody {
		t.Errorf("body = %q, want intact pass-through body %q", body, responseBody)
	}
	if proxy.reqLog != nil {
		t.Error("request logger is non-nil even though request logging was not started")
	}
}

// TestForward_RequestLog_CapturesAgent proves the detected calling agent is
// persisted in the request-log record for both recognized and unrecognized UAs.
func TestForward_RequestLog_CapturesAgent(t *testing.T) {
	const responseBody = `{"id":"r","object":"response","status":"completed","output":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"client-model": {{Provider: "backend", Model: "client-model", Protocol: "responses"}},
		},
	}
	proxy, dir, shutdown := newReqLogProxy(t, cfg)
	proxy.providers["backend"] = &testProv{key: "test-token"}
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	postWithUA := func(ua string) {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"client-model","input":[]}`))
		req.Header.Set("user-agent", ua)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	postWithUA("claude-cli/1.0")
	postWithUA("curl/8.0")

	awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "backend", Model: "client-model"})
	// Metrics commit and request-log persistence are SEPARATE async paths:
	// under -race (10-20x slower) the second record can queue after its
	// metrics land, and shutdown's flush would then miss it. Poll the log
	// until both records are durable BEFORE shutting down.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(allRecords(t, dir)) != 2 {
		time.Sleep(10 * time.Millisecond)
	}
	shutdown()
	records := allRecords(t, dir)
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	byAgent := map[string]bool{}
	for _, r := range records {
		byAgent[r.Agent] = true
	}
	if !byAgent["claude-code"] {
		t.Errorf("missing claude-code agent in records: %+v", records)
	}
	if !byAgent["curl"] {
		t.Errorf("missing curl-derived agent in records: %+v", records)
	}
}

// syncLogBuffer is a mutex-guarded log capture: Reload admits async
// goroutines (catalog refresh) that may still be writing warnings while the
// test reads the buffer — a bare bytes.Buffer is a data race there.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestReload_WarnsWhenRequestLogEnabledButInactive(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	useStaticProviderPools(t, "backend")
	var output syncLogBuffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
	}()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := "listen: 127.0.0.1:0\n" +
		"providers:\n" +
		"  backend:\n" +
		"    openai_base_url: http://127.0.0.1:1\n" +
		"    provider_id: static\n" +
		"routes:\n" +
		"  model:\n" +
		"    - {provider: backend, model: model}\n" +
		"request_log:\n" +
		"  enabled: true\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configdomain.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newTestProxy(t, cfg)
	if err := proxy.Reload(configPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	logOutput := output.String()
	if !strings.Contains(logOutput, "request_log.enabled is true") ||
		!strings.Contains(logOutput, "restart the daemon") {
		t.Errorf("reload did not warn about enabled-but-inactive request logging:\n%s", logOutput)
	}
}

func TestReload_NoWarnWhenRequestLogDisabled(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	useStaticProviderPools(t, "backend")
	var output syncLogBuffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
	}()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := "listen: 127.0.0.1:0\n" +
		"providers:\n" +
		"  backend:\n" +
		"    openai_base_url: http://127.0.0.1:1\n" +
		"    provider_id: static\n" +
		"routes:\n" +
		"  model:\n" +
		"    - {provider: backend, model: model}\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configdomain.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newTestProxy(t, cfg)
	if err := proxy.Reload(configPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if strings.Contains(output.String(), "request_log.enabled is true") {
		t.Errorf("reload warned even though request logging is disabled:\n%s", output.String())
	}
}

// TestRequestLog_SmallCapTruncatesBodiesEndToEnd (P1): every other fixture
// uses a 1 MiB cap that never truncates. With a tiny max_body_bytes the
// captured request/response bodies must be cut to the cap and carry the
// truncation marker — the exact flag the CLI replay path refuses to replay —
// while the CLIENT still receives the full response (logging must never
// truncate the live stream).
func TestRequestLog_SmallCapTruncatesBodiesEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"full":"` + strings.Repeat("y", 4096) + `"}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "p", Model: "m"}}},
	}
	proxy := newTestProxy(t, cfg)
	proxy.providers["p"] = &testProv{key: "k"}
	dir := t.TempDir()
	logger := requestlog.New(requestlog.Options{
		Directory:    dir,
		MaxFileSize:  1 << 30,
		MaxBodyBytes: 64,
	})
	go logger.Run()
	proxy.reqLog = logger
	t.Cleanup(func() {
		proxy.Close()
		logger.Shutdown()
	})
	px := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer px.Close()

	bigBody := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}]}`
	code, clientBody := post(t, px.URL+"/v1/chat/completions", bigBody)

	// The live client response is NOT truncated by the log cap.
	if code != 200 || len(clientBody) < 4096 {
		t.Fatalf("client response truncated by request-log cap: status=%d len=%d", code, len(clientBody))
	}

	deadline := time.Now().Add(2 * time.Second)
	var rec *requestlog.Record
	for time.Now().Before(deadline) && rec == nil {
		for _, r := range allRecords(t, dir) {
			if r.Provider == "p" {
				rr := r
				rec = &rr
			}
		}
		if rec == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if rec == nil {
		t.Fatal("no request-log record for the oversized request")
	}
	if !rec.RequestBodyTruncated() {
		t.Errorf("request body not marked truncated (len=%d, cap=64): %q…", len(rec.RequestBody), rec.RequestBody[:min(64, len(rec.RequestBody))])
	}
	if len(rec.RequestBody) != 64+len("...[truncated by model-proxy request_log max_body_bytes]")+1 {
		t.Errorf("truncated request body length = %d, want cap+marker", len(rec.RequestBody))
	}
	if !strings.HasSuffix(rec.ResponseBody, "]") || !strings.Contains(rec.ResponseBody, "truncated by model-proxy request_log") {
		t.Errorf("response body not marker-terminated: %q…", rec.ResponseBody[:min(64, len(rec.ResponseBody))])
	}
}

// TestForward_RequestLog_AggregatesCompletedStreams: with
// request_log.aggregate_streams the composition root wires the protocol
// aggregator into the request logger. The client still receives the stream
// byte-for-byte; the log stores the completed stream as its aggregated chat
// completion (flagged response_aggregated, wire size kept).
func TestForward_RequestLog_AggregatesCompletedStreams(t *testing.T) {
	const backendStream = "data: {\"id\":\"c1\",\"model\":\"backend-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"backend-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, backendStream)
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"backend-model": {{Provider: "backend", Model: "backend-model", Protocol: "openai"}},
		},
	})
	proxy.providers["backend"] = &testProv{key: "test-token"}
	dir := t.TempDir()
	proxy.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: dir, AggregateStreams: true})
	proxy.reqLogStarted = proxy.lifecycle.Run(func(<-chan struct{}) { proxy.reqLog.Run() })
	proxy.reqLogIndexStarted = proxy.lifecycle.Run(func(<-chan struct{}) { proxy.reqLogIndex.Run() })
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	code, clientBody := post(t, server.URL+"/v1/chat/completions",
		`{"model":"backend-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || clientBody != backendStream {
		t.Fatalf("client = (%d, %q), want the upstream stream byte-for-byte", code, clientBody)
	}
	awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "backend", Model: "backend-model"})
	proxy.Close() // drains the request logger

	records := allRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("request log record count = %d, want 1", len(records))
	}
	record := records[0]
	if !record.ResponseAggregated || record.ResponseSize != int64(len(backendStream)) {
		t.Fatalf("record aggregated=%v size=%d, want aggregated with wire size %d", record.ResponseAggregated, record.ResponseSize, len(backendStream))
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(record.ResponseBody), &completion); err != nil {
		t.Fatalf("logged body is not the aggregated JSON: %v (%q)", err, record.ResponseBody)
	}
	if completion.Object != "chat.completion" || len(completion.Choices) != 1 ||
		completion.Choices[0].Message.Content != "hello" || completion.Choices[0].FinishReason != "stop" ||
		completion.Usage.PromptTokens != 5 || completion.Usage.CompletionTokens != 2 {
		t.Errorf("aggregated completion = %+v (%s)", completion, record.ResponseBody)
	}
}

// TestInitRequestLogWiresHotRetention: request_log.hot_retention is resolved
// once and shared by both streams — the default (unset → 168h) and an explicit
// window must reach the LLM logger and, when mcp_split is on, the MCP logger,
// and the announcement line broadcasts the window; "0" keeps every file plain
// and stays silent.
func TestInitRequestLogWiresHotRetention(t *testing.T) {
	var output syncLogBuffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
	}()

	proxy := newTestProxy(t, &configdomain.Config{})
	proxy.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: t.TempDir()})
	if got := proxy.reqLog.HotRetention(); got != 168*time.Hour {
		t.Fatalf("default-wired reqLog hot retention = %v, want 168h", got)
	}
	if !strings.Contains(output.String(), "hot window 168h") {
		t.Errorf("announcement missing the default hot-window note:\n%s", output.String())
	}

	split := newTestProxy(t, &configdomain.Config{})
	split.initRequestLog(configdomain.RequestLogConfig{
		Enabled:      true,
		Dir:          t.TempDir(),
		MCPDir:       t.TempDir(),
		HotRetention: "48h",
		MCPSplit:     true,
	})
	if got := split.reqLog.HotRetention(); got != 48*time.Hour {
		t.Fatalf("reqLog hot retention = %v, want 48h", got)
	}
	if got := split.mcpReqLog.HotRetention(); got != 48*time.Hour {
		t.Fatalf("mcpReqLog hot retention = %v, want 48h (shared window)", got)
	}

	var coldOutput syncLogBuffer
	log.SetOutput(&coldOutput)
	cold := newTestProxy(t, &configdomain.Config{})
	cold.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: t.TempDir(), HotRetention: "0"})
	if got := cold.reqLog.HotRetention(); got != 0 {
		t.Fatalf(`hot retention "0" wired %v, want 0 (archiving disabled)`, got)
	}
	if strings.Contains(coldOutput.String(), "hot window") {
		t.Errorf(`announcement added a hot-window note despite hot_retention "0":\n%s`, coldOutput.String())
	}
}

// TestForward_AllTargetsFailedFoldsLegsIntoTerminalRow: through the real
// handler and logger, a request the proxy answers with its own 502 shows up
// in the list as ONE row — the terminal answer, every failover hop folded
// into its failed_hops — instead of a lone "failover leg" row that made an
// answered request look unanswered. The terminal row carries no usage: it is
// a failure, not a token-bearing answer.
func TestForward_AllTargetsFailedFoldsLegsIntoTerminalRow(t *testing.T) {
	// The first hop is held past hopFloor, giving its folded latency a known
	// lower bound: failed_hops[].latency_ms read 0 for every leg before the
	// leg row carried its own hop clock.
	const hopFloor = 20 * time.Millisecond
	a := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		for deadline := time.Now().Add(hopFloor); time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		staticResponder(500, `{"error":"a exploded"}`)(w, r)
	})
	b := newFakeUpstream(t, staticResponder(503, `{"error":"b unavailable"}`))
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: a.srv.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: b.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"m1": {
			{Provider: "a", Model: "m1", Priority: 1},
			{Provider: "b", Model: "m1", Priority: 2},
		}},
	}
	proxy, dir, shutdown := newReqLogProxy(t, cfg)
	proxy.providers["a"] = &testProv{key: "a"}
	proxy.providers["b"] = &testProv{key: "b"}
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	const requestBody = `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`
	code, clientBody := post(t, server.URL+"/v1/chat/completions", requestBody)
	if code != http.StatusBadGateway {
		t.Fatalf("client status = %d (want the 502 terminal); body=%s", code, clientBody)
	}
	shutdown()

	summaries, err := requestlog.QuerySummaries(dir, requestlog.Filter{})
	if err != nil {
		t.Fatalf("query summaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("list rows = %+v, want exactly the terminal row (legs folded)", summaries)
	}
	row := summaries[0]
	if row.AttemptOnly || row.Status != http.StatusBadGateway || row.Provider != "(failover)" {
		t.Errorf("list row = attempt_only:%v status:%d provider:%q, want the terminal row: 502 from (failover)", row.AttemptOnly, row.Status, row.Provider)
	}
	wantHops := []requestlog.FailedHop{
		{Provider: "a", Status: 500, Attempt: 0},
		{Provider: "b", Status: 503, Attempt: 1},
	}
	if len(row.FailedHops) != len(wantHops) {
		t.Fatalf("failed_hops = %+v, want both hops %+v", row.FailedHops, wantHops)
	}
	for i, want := range wantHops {
		if got := row.FailedHops[i]; got.Provider != want.Provider || got.Status != want.Status || got.Attempt != want.Attempt {
			t.Errorf("failed_hops[%d] = %+v, want %+v", i, got, want)
		}
	}
	if got := row.FailedHops[0].LatencyMs; got < int64(hopFloor/time.Millisecond) {
		t.Errorf("failed_hops[0].latency_ms = %d, want at least the %d ms the hop took", got, hopFloor/time.Millisecond)
	}
	if row.Input != 0 || row.Output != 0 || row.CacheRead != 0 || row.CacheCreation != 0 {
		t.Errorf("terminal row usage = %d/%d/%d/%d, want all zero", row.Input, row.Output, row.CacheRead, row.CacheCreation)
	}

	records := allRecords(t, dir)
	var legs, terminal int
	for _, record := range records {
		if record.AttemptOnly {
			legs++
			continue
		}
		terminal++
		if record.ResponseBody != clientBody || record.RequestBody != requestBody {
			t.Errorf("terminal record bodies = request %q / response %q, want the client request and the exact client response", record.RequestBody, record.ResponseBody)
		}
	}
	if legs != 2 || terminal != 1 {
		t.Errorf("records = %d legs + %d terminal, want 2 + 1", legs, terminal)
	}
}
