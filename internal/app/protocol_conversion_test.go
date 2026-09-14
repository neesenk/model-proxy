package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---- protocol_conversion_integration_test.go ----

// TestForward_AnthropicToOpenAI_NonStream: an Anthropic client hitting a route
// whose target declares protocol:openai gets its request converted to OpenAI
// chat/completions, and the OpenAI JSON response converted back to an Anthropic
// message — end to end.
func TestForward_AnthropicToOpenAI_NonStream(t *testing.T) {
	var gotOpenAIReq string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("backend path=%q want /chat/completions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotOpenAIReq = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"abc","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2}}`))
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":100,"system":"be nice","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200: %s", resp.StatusCode, body)
	}

	// Backend received an OpenAI-format request (system message, /chat/completions).
	if !strings.Contains(gotOpenAIReq, `"role":"system"`) || !strings.Contains(gotOpenAIReq, `"content":"be nice"`) || !strings.Contains(gotOpenAIReq, `"model":"gpt-x"`) {
		t.Errorf("backend got non-OpenAI request: %s", gotOpenAIReq)
	}
	// Client received an Anthropic-format response.
	bs := string(body)
	for _, want := range []string{`"type":"message"`, `"text":"hello world"`, `"stop_reason":"end_turn"`} {
		if !strings.Contains(bs, want) {
			t.Errorf("client response missing %q: %s", want, bs)
		}
	}
}

// TestForward_ConvertRequestFail_Closed (regression #9, request side): when a
// cross-protocol request's conversion fails, the unconverted body must NOT be
// sent to the backend (it would ship an Anthropic body to an OpenAI endpoint).
// Pre-fix the conversion error was logged and the original body forwarded anyway.
func TestForward_ConvertRequestFail_Closed(t *testing.T) {
	var upstreamHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// extractModel returns "claude-x" (fast path reads 3 tokens, ignores the
	// rest), but the full JSON is malformed so convertRequest fails.
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","messages":[BAD`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if upstreamHits != 0 {
		t.Errorf("upstream hit %d time(s), want 0 (conversion failed → must not send the Anthropic body to the OpenAI endpoint)", upstreamHits)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (fail-closed: no target served)", resp.StatusCode)
	}
}

// TestForward_ConvertResponseFail_Closed (regression #9, response side): when a
// cross-protocol response's conversion fails, the client must get a 502 — NOT the
// backend's body in the wrong protocol behind the upstream's 2xx status. Pre-fix
// the 2xx status was committed before conversion, then the raw body was passed
// through ("best-effort").
func TestForward_ConvertResponseFail_Closed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{not valid json`)) // upstream 2xx, but unparseable body
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (conversion failed → must not return a wrong-protocol 2xx body)", resp.StatusCode)
	}
	if bytes.Contains(body, []byte("not valid json")) {
		t.Errorf("client received the raw backend body (fail-open): %q", body)
	}
}

// TestForward_AnthropicToOpenAI_Stream: streaming conversion — the OpenAI SSE
// chunks are converted to Anthropic message events the client receives.
func TestForward_AnthropicToOpenAI_Stream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/messages", strings.NewReader(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	s := string(body)
	for _, want := range []string{"event: message_start", "event: content_block_delta", `"text":"hi"`, "event: message_stop"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream missing %q:\n%s", want, s)
		}
	}
	events := parseSSE(s)
	if got := sseCount(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop count = %d, want 1: %s", got, s)
	}
	assertNoSSEError(t, events)
}

// TestForward_OpenAIToAnthropic_NonStream: REVERSE direction — an OpenAI client
// (POST /v1/chat/completions) hitting a route whose target declares
// protocol:anthropic. The request is converted to Anthropic /v1/messages and the
// Anthropic response back to an OpenAI chat completion. (Backend only provides
// Anthropic; client accesses it with the OpenAI protocol.)
func TestForward_OpenAIToAnthropic_NonStream(t *testing.T) {
	var gotAnthropicReq string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("backend path=%q want /v1/messages", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotAnthropicReq = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_xyz","type":"message","role":"assistant","model":"claude","stop_reason":"end_turn","content":[{"type":"text","text":"reply"}],"usage":{"input_tokens":4,"output_tokens":1}}`))
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"ant": {AnthropicBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"gpt-x": {{Provider: "ant", Model: "claude", Protocol: "anthropic"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["ant"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// OpenAI client → backend converted to Anthropic (system lifted to top-level,
	// /v1/messages path, max_tokens carried).
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-x","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}],"max_tokens":100}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200: %s", resp.StatusCode, body)
	}

	if !strings.Contains(gotAnthropicReq, `"text":"s"`) || !strings.Contains(gotAnthropicReq, `"model":"claude"`) || !strings.Contains(gotAnthropicReq, `"max_tokens":100`) {
		t.Errorf("backend got non-Anthropic request: %s", gotAnthropicReq)
	}
	bs := string(body)
	for _, want := range []string{`"object":"chat.completion"`, `"content":"reply"`, `"finish_reason":"stop"`, `"prompt_tokens":4`, `"total_tokens":5`} {
		if !strings.Contains(bs, want) {
			t.Errorf("client response missing %q: %s", want, bs)
		}
	}
}

// TestForward_OpenAIToAnthropic_Stream: REVERSE streaming — Anthropic message
// events from the backend converted to OpenAI chat.completion.chunk SSE the
// client receives (incl. the [DONE] terminator).
func TestForward_OpenAIToAnthropic_Stream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"ant": {AnthropicBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"gpt-x": {{Provider: "ant", Model: "claude", Protocol: "anthropic"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["ant"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	s := string(body)
	for _, want := range []string{`"object":"chat.completion.chunk"`, `"content":"hi"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream missing %q:\n%s", want, s)
		}
	}
	events := parseSSE(s)
	if got := sseCount(events, "[DONE]"); got != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %s", got, s)
	}
	assertNoSSEError(t, events)
}

// TestForward_Convert_LogsClientProtocolBody is the F2 regression: when protocol
// conversion AND request logging are both on, the logged bytes must be the
// CLIENT-protocol body (what the client received), not the backend's native
// bytes. Before the fix, newCaptureReader wrapped resp.Body and logged the openai
// JSON (or empty, since resp.Body was already read+closed for non-stream convert).
func TestForward_Convert_LogsClientProtocolBody(t *testing.T) {
	const openaiResp = `{"id":"abc","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(openaiResp))
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p, dir, shutdown := newReqLogProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	defer shutdown()
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	clientBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200: %s", resp.StatusCode, clientBody)
	}

	// Client received the Anthropic conversion.
	if !strings.Contains(string(clientBody), `"type":"message"`) || !strings.Contains(string(clientBody), `"text":"hello"`) {
		t.Fatalf("client did not get anthropic body: %s", string(clientBody))
	}
	// The request log must record the SAME anthropic body the client got — NOT the
	// openai `choices` shape, and NOT empty. The record is enqueued from the
	// handler goroutine after the body copy, so wait for the commit metrics
	// before draining the logger (post() returning does not imply the record
	// was enqueued yet).
	awaitCommitMetrics(t, p, counters.PMKey{Provider: "oai", Model: "gpt-x"})
	shutdown()
	recs := allRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(recs))
	}
	rb := recs[0].ResponseBody
	if !strings.Contains(rb, `"type":"message"`) || !strings.Contains(rb, `"text":"hello"`) {
		t.Errorf("logged body is not the client-protocol body (F2 regression):\n%s", rb)
	}
	if strings.Contains(rb, `"choices"`) {
		t.Errorf("logged body leaked the backend's native openai shape:\n%s", rb)
	}
	if rb == "" {
		t.Error("logged body is empty (resp.Body was read+closed before capture)")
	}
}

// TestForward_Convert_StreamingCountsTokens is the F5 regression: token stats
// must NOT be permanently 0 on converted routes. Forward (anthropic client →
// openai backend) injects stream_options.include_usage and the transformer
// carries completion_tokens into message_delta; reverse (openai client →
// anthropic backend) emits a final chunk carrying usage. The usageScanner sees
// the converted stream and attributes tokens to (provider, model).
func TestForward_Convert_StreamingCountsTokens(t *testing.T) {
	// Forward: openai backend streams a usage-bearing final chunk.
	fwdUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the converted request asked for usage.
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"include_usage":true`) {
			t.Errorf("converted openai request did not request include_usage: %s", string(b))
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `data: {"model":"gpt-x","choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		io.WriteString(w, `data: {"model":"gpt-x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":7}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer fwdUp.Close()
	fwdCfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: fwdUp.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	fp := newTestProxy(t, fwdCfg)
	fp.providers["oai"] = &testProv{key: "k"}
	fpx := httptest.NewServer(http.HandlerFunc(fp.Handler))
	defer fpx.Close()
	req, _ := http.NewRequest(http.MethodPost, fpx.URL+"/v1/messages", strings.NewReader(`{"model":"claude-x","max_tokens":50,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Output (7) AND input (10) must be attributed: the transformer carries the
	// trailing prompt_tokens into message_delta.usage.input_tokens, and the
	// usageScanner reads input_tokens there (openai delivers prompt_tokens at the
	// trailing chunk, after message_start already fired with 0).
	if got := fp.tokens.Snapshot()[counters.PMKey{Provider: "oai", Model: "gpt-x"}]; got.Output != 7 || got.Input != 10 {
		t.Errorf("forward converted tokens: input=%d output=%d want 10/7", got.Input, got.Output)
	}

	// Reverse: anthropic backend streams usage-bearing message_start/delta.
	revUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}`+"\n\n")
		io.WriteString(w, `event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"yo"}}`+"\n\n")
		io.WriteString(w, `event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`+"\n\n")
		io.WriteString(w, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer revUp.Close()
	revCfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"ant": {AnthropicBaseURL: revUp.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"gpt-x": {{Provider: "ant", Model: "claude", Protocol: "anthropic"}}},
	}
	rp := newTestProxy(t, revCfg)
	rp.providers["ant"] = &testProv{key: "k"}
	rpx := httptest.NewServer(http.HandlerFunc(rp.Handler))
	defer rpx.Close()
	req2, _ := http.NewRequest(http.MethodPost, rpx.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if got := rp.tokens.Snapshot()[counters.PMKey{Provider: "ant", Model: "claude"}]; got.Input != 12 || got.Output != 4 {
		t.Errorf("reverse converted tokens: input=%d output=%d want 12/4 (F5: transformer emits usage chunk)", got.Input, got.Output)
	}
}

// ---- protocol_integration_support_test.go ----

func unmarshalMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return value
}

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func strOpt(value any) string {
	result, _ := value.(string)
	return result
}

func strOf(value any) string {
	if result, ok := value.(string); ok {
		return result
	}
	body, _ := json.Marshal(value)
	return strings.Trim(string(body), `"`)
}

func intOf(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	default:
		return 0
	}
}

func mustJSONStr(t *testing.T, value string) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type sseEvent struct {
	event string
	data  string
}

func drainSSE(t *testing.T, reader io.Reader) []sseEvent {
	t.Helper()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("drainSSE read: %v", err)
	}
	return parseSSE(string(raw))
}

func parseSSE(raw string) []sseEvent {
	var events []sseEvent
	pending := ""
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			pending = ""
		case strings.HasPrefix(line, "event:"):
			pending = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			events = append(events, sseEvent{
				event: pending,
				data:  strings.TrimSpace(strings.TrimPrefix(line, "data:")),
			})
			pending = ""
		}
	}
	return events
}

func sseEventType(event sseEvent) string {
	if event.event != "" {
		return event.event
	}
	if event.data == "[DONE]" {
		return "[DONE]"
	}
	var body map[string]any
	if json.Unmarshal([]byte(event.data), &body) == nil {
		if typ := strOpt(body["type"]); typ != "" {
			return typ
		}
		if object := strOpt(body["object"]); object != "" {
			return object
		}
	}
	return event.data
}

func sseEventTypes(events []sseEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, sseEventType(event))
	}
	return types
}

func sseCount(events []sseEvent, typ string) int {
	count := 0
	for _, event := range events {
		if sseEventType(event) == typ {
			count++
		}
	}
	return count
}

func sseFilter(events []sseEvent, typ string) []sseEvent {
	var filtered []sseEvent
	for _, event := range events {
		if sseEventType(event) == typ {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func assertNoSSEError(t *testing.T, events []sseEvent) {
	t.Helper()
	for _, event := range events {
		if event.event == "error" {
			t.Fatalf("unexpected SSE error event: %s", event.data)
		}
		var body map[string]any
		if json.Unmarshal([]byte(event.data), &body) == nil && body["error"] != nil {
			t.Fatalf("unexpected SSE error payload: %s", event.data)
		}
	}
}

func sseDataMap(t *testing.T, event sseEvent) map[string]any {
	t.Helper()
	return unmarshalMap(t, []byte(event.data))
}

func extractCandidateText(body []byte, backendProto string) string {
	wire, ok := protocol.Parse(backendProto)
	if !ok {
		return ""
	}
	return protocol.ExtractResponseText(body, wire)
}

const responsesTextSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-x"}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_0","role":"assistant","content":[]}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hel"}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"lo"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_0","status":"completed"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-x","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

// ---- protocol_capabilities_integration_test.go ----

func TestWriteUnsupportedConversionError_ProtocolEnvelopes(t *testing.T) {
	err := &protocol.UnsupportedError{
		ClientProto: "openai", TargetProto: "anthropic",
		Feature: "audio", Detail: "Chat Completions input_audio content",
	}
	for _, proto := range []protocol.Protocol{protocol.OpenAI, protocol.Responses, protocol.Anthropic} {
		t.Run(string(proto), func(t *testing.T) {
			rec := httptest.NewRecorder()
			protocol.WriteUnsupportedConversionError(rec, proto, err)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("content-type") != "application/json" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
			body := unmarshalMap(t, rec.Body.Bytes())
			if proto == protocol.Anthropic {
				if body["type"] != "error" || asMap(body["error"])["type"] != "invalid_request_error" {
					t.Fatalf("anthropic envelope = %v", body)
				}
			} else if asMap(body["error"])["code"] != "unsupported_protocol_conversion" {
				t.Fatalf("openai envelope = %v", body)
			}
		})
	}
}

func TestForward_UnsupportedConversionReturns400WithoutUpstream(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("unsupported request reached upstream")
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"ant": {
			Provider: testProviderID, AnthropicBaseURL: up.URL, OpenAIBaseURL: up.URL,
		}},
		Routes: map[string][]configdomain.RouteTarget{"m": {{
			Provider: "ant", Model: "claude", Protocol: "anthropic",
		}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["ant"] = &testProv{key: "k"}
	server := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
	if !strings.Contains(string(body), `"code":"unsupported_protocol_conversion"`) ||
		!strings.Contains(string(body), "n=2") {
		t.Fatalf("body=%s", body)
	}
}

func TestForward_UnsupportedTargetFallsThroughToCompatibleProtocol(t *testing.T) {
	var anthropicCalls atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls.Add(1)
		t.Error("unsupported converted target was called")
	}))
	defer anthropic.Close()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer chat.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"ant":  {Provider: testProviderID, AnthropicBaseURL: anthropic.URL, OpenAIBaseURL: anthropic.URL},
			"chat": {Provider: testProviderID, OpenAIBaseURL: chat.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{"m": {
			{Provider: "ant", Model: "claude", Protocol: "anthropic", Priority: 0},
			{Provider: "chat", Model: "gpt", Protocol: "openai", Priority: 1},
		}},
	}
	p := newTestProxy(t, cfg)
	p.providers["ant"] = &testProv{key: "k"}
	p.providers["chat"] = &testProv{key: "k"}
	server := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || anthropicCalls.Load() != 0 || !strings.Contains(string(body), `"content":"ok"`) {
		t.Fatalf("status=%d anthropic_calls=%d body=%s", resp.StatusCode, anthropicCalls.Load(), body)
	}
}

// ---- proxy_protocol_integration_test.go ----

// --- UC1: anthropic request resolved via explicit alias route + forwarded with /v1 kept ---

func TestUC_AnthropicMappingAndPathKept(t *testing.T) {
	var hitPath string
	var hitModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		json.Unmarshal(b, &m)
		hitModel = m.Model
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: "aqp"},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"claude-opus-4-8": {{Provider: "aqp", Model: "glm-5.2"}},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"aqp": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/messages", `{"model":"claude-opus-4-8","messages":[]}`)

	if hitModel != "glm-5.2" {
		t.Errorf("upstream model=%q want glm-5.2 (alias route target model)", hitModel)
	}
	if hitPath != "/v1/messages" {
		t.Errorf("upstream path=%q want /v1/messages (anthropic keeps /v1)", hitPath)
	}
}

// --- UC2: openai request strips the client /v1 prefix ---

func TestUC_OpenAIStripsV1Prefix(t *testing.T) {
	var hitPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"codex": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/chat/completions", `{"model":"gpt-5.5","messages":[]}`)

	if hitPath != "/chat/completions" {
		t.Errorf("upstream path=%q want /chat/completions (openai strips /v1)", hitPath)
	}
}
