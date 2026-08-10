package app

import (
	"bytes"
	"context"
	"io"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":100,"system":"be nice","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

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
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
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
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
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

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
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

	cfg := &Config{
		Providers: map[string]Provider{"ant": {AnthropicBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "ant", Model: "claude", Protocol: "anthropic"}}},
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
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

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

	cfg := &Config{
		Providers: map[string]Provider{"ant": {AnthropicBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "ant", Model: "claude", Protocol: "anthropic"}}},
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

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
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
	clientBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Client received the Anthropic conversion.
	if !strings.Contains(string(clientBody), `"type":"message"`) || !strings.Contains(string(clientBody), `"text":"hello"`) {
		t.Fatalf("client did not get anthropic body: %s", string(clientBody))
	}
	// The request log must record the SAME anthropic body the client got — NOT the
	// openai `choices` shape, and NOT empty.
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
	fwdCfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: fwdUp.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
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
	revCfg := &Config{
		Providers: map[string]Provider{"ant": {AnthropicBaseURL: revUp.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "ant", Model: "claude", Protocol: "anthropic"}}},
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
