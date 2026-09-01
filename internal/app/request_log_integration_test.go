package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
)

// newReqLogProxy wires a real requestlog.Logger into a test Proxy. Call the
// returned shutdown function before reading the JSONL files so every accepted
// record has been drained to disk.
func newReqLogProxy(t *testing.T, cfg *Config) (*Proxy, string, func()) {
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
		// Shadow and other finite log producers belong to Proxy lifecycle and
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

	cfg := &Config{
		Providers: map[string]Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
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

	response, err := http.Post(
		server.URL+"/v1/responses",
		"application/json",
		strings.NewReader(requestBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	clientBody, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200; body=%s", response.StatusCode, clientBody)
	}
	if string(clientBody) != responseBody {
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

	cfg := &Config{
		Providers: map[string]Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
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

	response, err := http.Post(
		server.URL+"/v1/messages",
		"application/json",
		strings.NewReader(requestBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	clientBody, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200; body=%s", response.StatusCode, clientBody)
	}
	if bytes.Equal(clientBody, []byte(backendStream)) {
		t.Fatalf("client received unconverted backend stream:\n%s", clientBody)
	}
	for _, want := range []string{
		"event: message_start",
		"event: content_block_delta",
		`"text":"hello"`,
		"event: message_stop",
	} {
		if !bytes.Contains(clientBody, []byte(want)) {
			t.Errorf("converted client stream missing %q:\n%s", want, clientBody)
		}
	}
	events := parseSSE(string(clientBody))
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

	cfg := &Config{
		Providers: map[string]Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"client-model": {{Provider: "backend", Model: "client-model", Protocol: "responses"}},
		},
	}
	proxy := newTestProxy(t, cfg)
	proxy.providers["backend"] = &testProv{key: "test-token"}
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	response, err := http.Post(
		server.URL+"/v1/responses",
		"application/json",
		strings.NewReader(`{"model":"client-model","input":[]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with request logging disabled", response.StatusCode)
	}
	if string(body) != responseBody {
		t.Errorf("body = %q, want intact pass-through body %q", body, responseBody)
	}
	if proxy.reqLog != nil {
		t.Error("request logger is non-nil even though request logging was not started")
	}
}

func TestReload_WarnsWhenRequestLogEnabledButInactive(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	useStaticProviderPools(t, "backend")
	var output bytes.Buffer
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
	cfg, err := LoadConfig(configPath)
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
	var output bytes.Buffer
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
	cfg, err := LoadConfig(configPath)
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
	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "p", Model: "m"}}},
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
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(bigBody))
	if err != nil {
		t.Fatal(err)
	}
	clientBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The live client response is NOT truncated by the log cap.
	if resp.StatusCode != 200 || len(clientBody) < 4096 {
		t.Fatalf("client response truncated by request-log cap: status=%d len=%d", resp.StatusCode, len(clientBody))
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
