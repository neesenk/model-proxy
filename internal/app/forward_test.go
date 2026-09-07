package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- forward_edge_scenario_test.go ----

// TestForward_CommittedSSEStreamMidFailureIsNotRecalled pins the "commit means
// no recall" end state: once an SSE stream has bytes committed to the client,
// an upstream dying mid-stream must NOT trigger failover — the client keeps
// the committed prefix, no fallback request is sent, and the post-commit death
// is not miscounted as a pre-commit hard failure (circuit untouched).
func TestForward_CommittedSSEStreamMidFailureIsNotRecalled(t *testing.T) {
	streamPrefix := "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"tial\"}}]}\n\n"
	var pHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pHits.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, streamPrefix)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // commits the prefix to the proxy (and on to the client)
		}
		// Die mid-stream: httptest recovers the panic and drops the
		// connection, so the proxy sees the stream break after the prefix.
		panic("upstream died mid-stream")
	}))
	defer primary.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{CircuitThreshold: 3},
	}
	p, dir, shutdown := newReqLogProxy(t, cfg)
	defer shutdown()
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(px.URL+"/v1/chat/completions", "application/json",
		stringReader(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("client post: %v", err)
	}
	// The stream broke mid-transfer: the read may end in EOF or an unexpected-
	// EOF error — both are valid observations of the committed-prefix contract.
	got, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(got), streamPrefix) {
		t.Fatalf("client did not receive the committed prefix verbatim (readErr=%v):\n%s", readErr, got)
	}

	// No recall: failover after commit must not send a fallback request.
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits = %d, want 1", got)
	}
	if len(*fallbackSeen) != 0 {
		t.Errorf("fallback was hit %d time(s) after a COMMITTED stream died — commit must mean no recall", len(*fallbackSeen))
	}

	// The request still reaches its terminal state: one committed end event
	// (status 200, the committed response) and a request-log record carrying
	// the partial stream the client saw. Both the end event and the record are
	// emitted from the handler goroutine after the client may have already
	// observed the broken stream, so poll for the end event instead of
	// asserting it immediately.
	deadline := time.Now().Add(2 * time.Second)
	found := false
	endStatus := 0
	for time.Now().Before(deadline) && !found {
		for _, e := range p.events.Snapshot() {
			if e.Type == "end" && e.Provider == "primary" {
				endStatus = e.Status
			}
		}
		for _, r := range allRecords(t, dir) {
			if r.Provider == "primary" && r.Status == 200 && strings.Contains(r.ResponseBody, "tial") {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if endStatus != 200 {
		t.Errorf("end event status = %d, want 200 (committed)", endStatus)
	}
	if !found {
		t.Error("no request-log record for the committed partial stream")
	}

	// Post-commit death is not a pre-commit hard failure: circuit untouched.
	if h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]; ok &&
		(h.ConsecutiveFailures != 0 || h.CircuitOpenUntil.After(time.Now())) {
		t.Errorf("mid-stream death poisoned the circuit: %+v", h)
	}
}

// TestForward_DialRefusedFailsOver covers the transport-level failure class:
// every other failover test injects an HTTP error (5xx/429/401), never a
// connection that cannot be established at all. A dial-refused primary must
// fail over to the sibling target with the client still getting a 200, and the
// network failure must land in health as a hard failure.
func TestForward_DialRefusedFailsOver(t *testing.T) {
	// Reserve a port, then close its listener: dialing it is refused.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: deadURL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{CircuitThreshold: 3, RetryWait: "0"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	code, body := post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if code != 200 || body != `{"ok":true}` {
		t.Fatalf("dial-refused failover: status=%d body=%s, want 200 with the fallback body", code, body)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback seen %d time(s), want 1", len(*fallbackSeen))
	}
	h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
	if !ok {
		t.Fatal("primary health missing after dial refusal")
	}
	if h.ConsecutiveFailures != 1 {
		t.Errorf("dial refusal recorded as %d consecutive failures, want 1 (hard failure)", h.ConsecutiveFailures)
	}
}

// ---- forward_events_test.go ----

// TestForward_EarlyEventsHaveRequestID (bug 7): live events published on the
// EARLY-return paths (missing model 400, unrouted model 502, unknown path 502)
// used to carry an empty request_id — requestID was generated deep in forward
// (at the start-event), after these returns. The contract (fusion-shadow-cache
// "forward 产生 start/end，含稳定 request_id；cache hit、400/502 终局也必须产生
// end") requires every event to carry a stable id so start↔end pairing works.
func TestForward_EarlyEventsHaveRequestID(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
`))
	p := newTestProxy(t, cfg)

	eventCursor := 0
	endEventIDs := func() []string {
		recent := p.events.Snapshot()
		var ids []string
		for _, e := range recent[eventCursor:] {
			if e.Type == "end" {
				ids = append(ids, e.RequestID)
			}
		}
		eventCursor = len(recent)
		return ids
	}
	do := func(body, path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		p.Handler(rec, req)
		return rec.Code
	}

	// Missing model field → 400 terminal end event with a non-empty request_id.
	if code := do(`{"stream":false}`, "/v1/chat/completions"); code != http.StatusBadRequest {
		t.Errorf("missing-model: status=%d, want 400", code)
	}
	ids := endEventIDs()
	if len(ids) == 0 {
		t.Fatalf("missing-model: expected an end event, got none")
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("missing-model end event has empty request_id")
		}
	}

	// Model not in routes → 502 terminal end event with a non-empty request_id.
	if code := do(`{"model":"no-such-route","stream":false}`, "/v1/chat/completions"); code != http.StatusBadGateway {
		t.Errorf("model-not-found: status=%d, want 502", code)
	}
	ids = endEventIDs()
	if len(ids) == 0 {
		t.Fatalf("model-not-found: expected an end event, got none")
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("model-not-found end event has empty request_id")
		}
	}

	// Unknown path (proto=="") → handler emits a terminal end event (502 终局也
	// 必须产生 end) with a non-empty request_id. Previously: no event at all.
	if code := do(`{}`, "/v1/no-such-path"); code != http.StatusBadGateway {
		t.Errorf("unknown-path: status=%d, want 502", code)
	}
	ids = endEventIDs()
	if len(ids) == 0 {
		t.Fatalf("unknown-path: expected a terminal end event, got none")
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("unknown-path end event has empty request_id")
		}
	}
}

// ---- forward_stream_mode_test.go ----

func TestForwardAdaptsUpstreamSSEToClientJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"claude-x","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("content-type"), "application/json") {
		t.Fatalf("status=%d content-type=%q body=%s", resp.StatusCode, resp.Header.Get("content-type"), body)
	}
	if !strings.Contains(string(body), `"type":"message"`) || !strings.Contains(string(body), `"text":"hello"`) {
		t.Fatalf("client did not receive Anthropic JSON: %s", body)
	}
}

func TestForwardAdaptsUpstreamJSONToClientSSE(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"codex-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(
		`{"model":"codex-x","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("content-type"), "text/event-stream") {
		t.Fatalf("status=%d content-type=%q body=%s", resp.StatusCode, resp.Header.Get("content-type"), body)
	}
	if !strings.Contains(string(body), "event: response.completed") || !strings.Contains(string(body), `"text":"hello"`) {
		t.Fatalf("client did not receive Responses SSE: %s", body)
	}
}

// TestForwardConvertsSSEAfterCommentHeartbeat verifies the full routing path,
// not just the framing helper. A cross-protocol upstream with no Content-Type
// may legally start with an SSE comment; it must stay on the streaming converter
// instead of being buffered and parsed as non-stream JSON.
func TestForwardConvertsSSEAfterCommentHeartbeat(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A nil Content-Type value suppresses net/http's automatic sniffing so
		// this exercises the proxy's empty-header fallback.
		w.Header()["Content-Type"] = nil
		io.WriteString(w, ": ping\n\n")
		io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"claude-x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "event: message_start") ||
		!strings.Contains(string(body), `"text":"hello"`) ||
		!strings.Contains(string(body), "event: message_stop") {
		t.Fatalf("client did not receive converted Anthropic SSE: %s", body)
	}
}

// ---- deepseek_forward_test.go ----

// newDeepSeekTestProxy wires a real DeepSeekProvider (via NewProxy/buildProviders)
// against two mock upstreams — openaiURL (openai_base_url) and anthropicURL
// (anthropic_base_url) — with a fake key file under a temp HOME. Returns the
// proxy's test server.
func newDeepSeekTestProxy(t *testing.T, openaiURL, anthropicURL string) *httptest.Server {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	keyFile := filepath.Join(tmpHome, ".model-proxy", "deepseek_apikey.json")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(`{"api_key":"sk-test-ds"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Providers: map[string]Provider{
			"deepseek": {OpenAIBaseURL: openaiURL, AnthropicBaseURL: anthropicURL, Provider: "deepseek"},
		},
		Routes: map[string][]RouteTarget{
			"deepseek-v4-pro": {{Provider: "deepseek", Model: "deepseek-v4-pro"}},
		},
	}
	p := newTestProxy(t, cfg)
	return httptest.NewServer(http.HandlerFunc(p.Handler))
}

type dsHit struct {
	path string
	auth string
	xkey string
}

// The proxy forwards by protocol: anthropic requests hit anthropic_base_url,
// openai requests hit openai_base_url. Verifies the protocol→endpoint
// routing that openai_base_url/anthropic_base_url config provides.
func TestForward_DeepSeekRoutesByProtocol(t *testing.T) {
	var openaiHit, anthropicHit dsHit
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHit = dsHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), xkey: r.Header.Get("x-api-key")}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer openaiUp.Close()
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHit = dsHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), xkey: r.Header.Get("x-api-key")}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer anthropicUp.Close()

	px := newDeepSeekTestProxy(t, openaiUp.URL, anthropicUp.URL)
	defer px.Close()

	// 1) anthropic POST /v1/messages → anthropic_base_url, NOT the openai base.
	postOK(t, px.URL+"/v1/messages", `{"model":"deepseek-v4-pro","messages":[]}`)
	if anthropicHit.path == "" {
		t.Error("anthropic request: expected to hit anthropic upstream")
	}
	if openaiHit.path != "" {
		t.Error("anthropic request: should not hit openai upstream")
	}
	if anthropicHit.path != "/v1/messages" {
		t.Errorf("anthropic upstream path: got %q, want /v1/messages", anthropicHit.path)
	}
	if anthropicHit.auth != "Bearer sk-test-ds" {
		t.Errorf("anthropic Authorization: got %q, want Bearer sk-test-ds", anthropicHit.auth)
	}
	if anthropicHit.xkey != "sk-test-ds" {
		t.Errorf("anthropic x-api-key: got %q, want sk-test-ds", anthropicHit.xkey)
	}

	// 2) openai POST /v1/chat/completions → openai_base_url, NOT anthropic_base_url.
	openaiHit, anthropicHit = dsHit{}, dsHit{}
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"deepseek-v4-pro","messages":[]}`)
	if openaiHit.path == "" {
		t.Error("openai request: expected to hit openai upstream")
	}
	if anthropicHit.path != "" {
		t.Error("openai request: should not hit anthropic upstream")
	}
	if openaiHit.path != "/chat/completions" {
		t.Errorf("openai upstream path: got %q, want /chat/completions", openaiHit.path)
	}
	// P0-1: assert exact auth values on the openai path too
	if openaiHit.auth != "Bearer sk-test-ds" {
		t.Errorf("openai Authorization: got %q, want Bearer sk-test-ds", openaiHit.auth)
	}
	if openaiHit.xkey != "sk-test-ds" {
		t.Errorf("openai x-api-key: got %q, want sk-test-ds", openaiHit.xkey)
	}
}
