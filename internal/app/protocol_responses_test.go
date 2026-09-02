package app

import (
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ---- protocol_responses_integration_test.go ----

// TestForward_AnthropicToResponses_NonStream: an Anthropic client hitting a
// route whose target declares protocol:responses gets its request converted to
// a Responses body (POST /responses, input list), and the Responses JSON
// response converted back to an Anthropic message.
func TestForward_AnthropicToResponses_NonStream(t *testing.T) {
	var gotReq string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("backend path=%q want /responses", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotReq = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-x","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"cdx": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "cdx", Model: "gpt-x", Protocol: "responses"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["cdx"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
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

	// Backend received a Responses-format request (input list, no `messages`).
	if !strings.Contains(gotReq, `"input"`) || strings.Contains(gotReq, `"messages"`) {
		t.Errorf("backend got non-Responses request: %s", gotReq)
	}
	// Client received an Anthropic-format response.
	bs := string(body)
	for _, want := range []string{`"type":"message"`, `"text":"hello world"`, `"stop_reason":"end_turn"`} {
		if !strings.Contains(bs, want) {
			t.Errorf("client response missing %q: %s", want, bs)
		}
	}
}

// TestForward_AnthropicToResponses_AutoResolve: option B — a codex target
// WITHOUT an explicit protocol: declaration still converts, because the forward
// path auto-resolves the backend protocol via ProtocolHint("codex")="responses".
// No protocol: needed on the route.
func TestForward_AnthropicToResponses_AutoResolve(t *testing.T) {
	var gotReq string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("backend path=%q want /responses", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotReq = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-x","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	cfg := &Config{
		// Provider id "codex" ⇒ ProtocolHint returns "responses" (auto-resolve).
		Providers: map[string]Provider{"cdx": {OpenAIBaseURL: up.URL, Provider: "codex"}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "cdx", Model: "gpt-x"}}}, // no Protocol
	}
	p := newTestProxy(t, cfg)
	p.providers["cdx"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"gpt-x","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
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

	// Backend received a Responses body (input list) — conversion auto-activated.
	if !strings.Contains(gotReq, `"input"`) || strings.Contains(gotReq, `"messages"`) {
		t.Errorf("auto-resolve did not convert to responses; backend got: %s", gotReq)
	}
	if !strings.Contains(string(body), `"type":"message"`) {
		t.Errorf("client did not get an anthropic response: %s", body)
	}
}

func TestForward_OpenAIToResponses_NonStream(t *testing.T) {
	var gotReq string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("backend path=%q want /responses", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotReq = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi back"}]}],"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"cdx": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "cdx", Model: "gpt-x", Protocol: "responses"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["cdx"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`))
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

	if !strings.Contains(gotReq, `"input"`) {
		t.Errorf("backend got non-Responses request: %s", gotReq)
	}
	bs := string(body)
	for _, want := range []string{`"object":"chat.completion"`, `"content":"hi back"`, `"finish_reason":"stop"`} {
		if !strings.Contains(bs, want) {
			t.Errorf("client response missing %q: %s", want, bs)
		}
	}
}

// TestForward_PooledResponsesConversionStreamsTerminalUsage proves the full
// Chat -> Responses route remains correct after a credential-pool parent is
// expanded: a pinned virtual supplies its own key, the request is rewritten to
// the Responses wire shape, and the converted Chat SSE preserves one clean
// terminal and the upstream usage accounting.
func TestForward_PooledResponsesConversionStreamsTerminalUsage(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")

	type upstreamRequest struct {
		path string
		auth string
		body []byte
	}
	var got upstreamRequest
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesTextSSE)
	}))
	defer up.Close()

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: up.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{
			"public-model": {{Provider: "zhipu", Model: "upstream-model", Protocol: "responses"}},
		},
	}
	p := newTestProxy(t, cfg)
	virtuals := p.poolIndex["zhipu"]
	if len(virtuals) != 2 {
		t.Fatalf("pooled virtuals = %v, want two", virtuals)
	}
	selectedVirtual := virtuals[0]
	keyForVirtual := map[string]string{
		"zhipu#" + accounts.AccountID("zhipu", accounts.Credentials{APIKey: "KEY-A"}): "KEY-A",
		"zhipu#" + accounts.AccountID("zhipu", accounts.Credentials{APIKey: "KEY-B"}): "KEY-B",
	}
	selectedKey, ok := keyForVirtual[selectedVirtual]
	if !ok {
		t.Fatalf("selected virtual %q has no configured key mapping", selectedVirtual)
	}
	if _, ok := p.setPin("public-model", selectedVirtual, 0); !ok {
		t.Fatalf("pin pooled virtual %q", selectedVirtual)
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(`{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200: %s", resp.StatusCode, body)
	}
	if gotCT := resp.Header.Get("Content-Type"); gotCT != "text/event-stream" {
		t.Fatalf("client Content-Type = %q, want text/event-stream", gotCT)
	}

	if got.path != "/responses" {
		t.Errorf("upstream path = %q, want /responses", got.path)
	}
	if got.auth != "Bearer "+selectedKey {
		t.Errorf("upstream Authorization = %q, want exact virtual %q key Bearer %s", got.auth, selectedVirtual, selectedKey)
	}
	upstream := unmarshalMap(t, got.body)
	if upstream["model"] != "upstream-model" {
		t.Errorf("upstream model = %v, want upstream-model", upstream["model"])
	}
	if upstream["stream"] != true {
		t.Errorf("upstream stream = %v, want true", upstream["stream"])
	}
	if _, hasMessages := upstream["messages"]; hasMessages {
		t.Errorf("Responses request unexpectedly contains messages: %s", got.body)
	}
	input, ok := upstream["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("Responses input = %v, want one item", upstream["input"])
	}
	inputMessage := asMap(input[0])
	if inputMessage["type"] != "message" || inputMessage["role"] != "user" {
		t.Errorf("Responses input item = %v, want user message", inputMessage)
	}
	parts, ok := inputMessage["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("Responses input content = %v, want one input_text part", inputMessage["content"])
	}
	part := asMap(parts[0])
	if part["type"] != "input_text" || part["text"] != "hello" {
		t.Errorf("Responses input part = %v, want input_text hello", part)
	}

	events := parseSSE(string(body))
	if got := sseCount(events, "[DONE]"); got != 1 {
		t.Errorf("[DONE] count = %d, want 1; events=%v", got, sseEventTypes(events))
	}
	for _, event := range events {
		if event.data == "[DONE]" {
			continue
		}
		frame := sseDataMap(t, event)
		if event.event == "error" || frame["type"] == "error" || frame["error"] != nil {
			t.Fatalf("Chat stream contains an error terminal: event=%q data=%s", event.event, event.data)
		}
	}
	finish := sseFilter(events, "chat.completion.chunk")
	var finishReasons []string
	var usage map[string]any
	for _, event := range finish {
		chunk := sseDataMap(t, event)
		choices, _ := chunk["choices"].([]any)
		for _, choice := range choices {
			if reason, ok := asMap(choice)["finish_reason"].(string); ok && reason != "" {
				finishReasons = append(finishReasons, reason)
			}
		}
		if rawUsage, ok := chunk["usage"].(map[string]any); ok {
			if usage != nil {
				t.Fatalf("multiple Chat usage chunks: %v and %v", usage, rawUsage)
			}
			usage = rawUsage
		}
	}
	if len(finishReasons) != 1 || finishReasons[0] != "stop" {
		t.Errorf("non-empty finish reasons = %v, want exactly [stop]", finishReasons)
	}
	if usage == nil {
		t.Fatal("Chat stream has no usage chunk")
	}
	if usage["prompt_tokens"] != float64(3) || usage["completion_tokens"] != float64(2) || usage["total_tokens"] != float64(5) {
		t.Errorf("Chat stream usage = %v, want prompt=3 completion=2 total=5", usage)
	}
}

// TestForward_ResponsesToAnthropic_NonStream completes the sixth conversion
// direction at the integration layer: a Responses-protocol client against an
// explicit anthropic backend. Instructions lift to the top-level system field,
// the request goes to /v1/messages with the model rewritten, and the client
// receives a Responses-shaped answer.
func TestForward_ResponsesToAnthropic_NonStream(t *testing.T) {
	var gotReq, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotReq, gotPath = string(b), r.URL.Path
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello back"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"ant": {AnthropicBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"r-x": {{Provider: "ant", Model: "claude-x", Protocol: "anthropic"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["ant"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(
		`{"model":"r-x","instructions":"be nice","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
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

	// Backend received an Anthropic request: /v1/messages path (kept for
	// anthropic), instructions lifted into the top-level system block list,
	// model rewritten to the target's upstream model, input list → messages.
	if gotPath != "/v1/messages" {
		t.Errorf("backend path = %q, want /v1/messages", gotPath)
	}
	// (Order-insensitive markers: Go map marshaling shuffles JSON keys, so
	// the system block's key order must not be pinned.)
	for _, want := range []string{`"system":[`, `"text":"be nice"`, `"model":"claude-x"`, `"messages":[`, `"text":"hi"`} {
		if !strings.Contains(gotReq, want) {
			t.Errorf("backend request missing %q: %s", want, gotReq)
		}
	}
	if strings.Contains(gotReq, `"input":[`) {
		t.Errorf("backend request still carries the responses input list: %s", gotReq)
	}

	// Client received a Responses-shaped answer.
	for _, want := range []string{`"object":"response"`, `"text":"hello back"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("client response missing %q: %s", want, body)
		}
	}
}

// ---- protocol_responses_state_integration_test.go ----

func TestForward_ResponsesPreviousIDRestoresChatToolHistory(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch hits.Add(1) {
		case 1:
			if !strings.Contains(string(body), `"content":"weather?"`) {
				t.Errorf("first request = %s", body)
			}
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"chat_1","model":"g","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2:
			var req map[string]any
			json.Unmarshal(body, &req)
			msgs, _ := req["messages"].([]any)
			if len(msgs) != 3 {
				t.Fatalf("restored chat messages = %d, want 3: %s", len(msgs), body)
			}
			if len(asMap(msgs[1])["tool_calls"].([]any)) != 1 {
				t.Fatalf("assistant tool call missing: %s", body)
			}
			if asMap(msgs[2])["role"] != "tool" || asMap(msgs[2])["tool_call_id"] != "c1" {
				t.Fatalf("tool output missing: %s", body)
			}
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"chat_2","model":"g","choices":[{"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`)
		default:
			t.Fatalf("unexpected upstream request %d", hits.Load())
		}
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"g": {{Provider: "p", Model: "g", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	post := func(body string) []byte {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-claude-code-session-id", "sess")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, out)
		}
		return out
	}
	first := post(`{"model":"g","input":"weather?","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`)
	var firstResp map[string]any
	json.Unmarshal(first, &firstResp)
	if firstResp["id"] != "chat_1" {
		t.Fatalf("first response = %s", first)
	}
	second := post(`{"model":"g","previous_response_id":"chat_1","input":[{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`)
	if !strings.Contains(string(second), "sunny") || hits.Load() != 2 {
		t.Fatalf("second response=%s hits=%d", second, hits.Load())
	}
}

// TestForward_ResponsesStreamPreviousIDRestoresChatToolHistory exercises the
// continuation state on the actual streaming conversion path.  The state must
// contain the client-facing Responses terminal (not the upstream Chat bytes),
// so a later previous_response_id request can rebuild the Chat tool turn.
func TestForward_ResponsesStreamPreviousIDRestoresChatToolHistory(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch hits.Add(1) {
		case 1:
			request := unmarshalMap(t, body)
			if request["stream"] != true {
				t.Fatalf("first upstream stream = %v, want true: %s", request["stream"], body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"chat_stream_1","object":"chat.completion.chunk","model":"g","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":null}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"id":"chat_stream_1","object":"chat.completion.chunk","model":"g","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 2:
			request := unmarshalMap(t, body)
			messages, _ := request["messages"].([]any)
			if len(messages) != 3 {
				t.Fatalf("restored Chat messages = %d, want user + tool call + tool output: %s", len(messages), body)
			}
			if messages[0] == nil || asMap(messages[0])["role"] != "user" {
				t.Fatalf("restored first message = %#v, want original user: %s", messages[0], body)
			}
			assistant := asMap(messages[1])
			calls, _ := assistant["tool_calls"].([]any)
			if assistant["role"] != "assistant" || len(calls) != 1 || asMap(calls[0])["id"] != "c1" {
				t.Fatalf("restored assistant tool call = %#v: %s", assistant, body)
			}
			tool := asMap(messages[2])
			if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != "sunny" {
				t.Fatalf("restored tool output = %#v: %s", tool, body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chat_stream_2","model":"g","choices":[{"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`)
		default:
			t.Fatalf("unexpected upstream request %d", hits.Load())
		}
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"g": {{Provider: "p", Model: "g", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	post := func(body string) []byte {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Claude-Code-Session-Id", "stream-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
		}
		return out
	}

	first := post(`{"model":"g","stream":true,"input":"weather?","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`)
	firstEvents := parseSSE(string(first))
	if got := sseCount(firstEvents, "response.completed"); got != 1 {
		t.Fatalf("completed terminal count = %d, want exactly one: %s", got, first)
	}
	completed := sseDataMap(t, sseFilter(firstEvents, "response.completed")[0])
	if response := asMap(completed["response"]); response["id"] != "chat_stream_1" || response["status"] != "completed" {
		t.Fatalf("completed response = %#v, want clean chat_stream_1 terminal", response)
	}

	continuation := []byte(`{"model":"g","previous_response_id":"chat_stream_1","input":[{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`)
	expanded, history, hit, err := p.responsesState.Expand(continuation, "stream-session")
	if err != nil || !hit {
		t.Fatalf("stream response state hit=%v err=%v, want hit", hit, err)
	}
	if len(history) != 3 {
		t.Fatalf("expanded history items = %d, want original input + one function call + current tool output: %#v", len(history), history)
	}
	if asMap(history[0])["role"] != "user" || asMap(history[1])["type"] != "function_call" || asMap(history[1])["call_id"] != "c1" || asMap(history[2])["type"] != "function_call_output" {
		t.Fatalf("expanded history lost or duplicated the streamed tool turn: %#v", history)
	}
	if strings.Contains(string(expanded), `"previous_response_id"`) {
		t.Fatalf("expanded continuation kept previous_response_id: %s", expanded)
	}

	second := post(string(continuation))
	if !strings.Contains(string(second), `"text":"sunny"`) || hits.Load() != 2 {
		t.Fatalf("second response=%s upstream requests=%d", second, hits.Load())
	}
}

// A stream without a clean Responses terminal is observable by the client but
// must never become continuation history.  This keeps a broken or cancelled
// delivery from replaying an assistant tool call on the next turn.
func TestForward_ResponsesTruncatedStreamDoesNotRecordContinuationState(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chat_truncated_1","object":"chat.completion.chunk","model":"g","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`+"\n\n")
		// Deliberately omit the Chat finish chunk and [DONE].  The converted
		// Responses stream has no response.completed terminal to record.
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"g": {{Provider: "p", Model: "g", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"g","stream":true,"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "truncated-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := sseCount(parseSSE(string(body)), "response.completed"); got != 0 {
		t.Fatalf("truncated stream completed terminals = %d, want 0: %s", got, body)
	}

	_, _, hit, err := p.responsesState.Expand([]byte(`{"model":"g","previous_response_id":"chat_truncated_1","input":"continue"}`), "truncated-session")
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("truncated stream unexpectedly created continuation state")
	}
}

// TestForward_ResponsesPreviousIDRestoresAcrossRestart proves the state store
// is a RESTART boundary, not an in-memory cache: the first proxy instance
// records chat_1 and persists it to the shared state path; a second instance
// booted on the same path expands previous_response_id from the PERSISTED
// state — the new upstream call carries the full restored history.
func TestForward_ResponsesPreviousIDRestoresAcrossRestart(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch hits.Add(1) {
		case 1:
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"chat_1","model":"g","choices":[{"message":{"role":"assistant","content":"asked"},"finish_reason":"stop"}]}`)
		case 2:
			var req map[string]any
			json.Unmarshal(body, &req)
			msgs, _ := req["messages"].([]any)
			if len(msgs) != 3 {
				t.Fatalf("restored messages after restart = %d, want 3 (user/assistant/user): %s", len(msgs), body)
			}
			if asMap(msgs[1])["role"] != "assistant" || !strings.Contains(fmt.Sprint(asMap(msgs[1])), "asked") {
				t.Fatalf("restored assistant turn missing: %s", body)
			}
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"chat_2","model":"g","choices":[{"message":{"role":"assistant","content":"restored"},"finish_reason":"stop"}]}`)
		default:
			t.Fatalf("unexpected upstream request %d", hits.Load())
		}
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"g": {{Provider: "p", Model: "g", Protocol: "openai"}}},
	}
	// One shared state path for both instances — the persisted
	// previous_response_id bridge must survive the swap.
	statePath := filepath.Join(t.TempDir(), "quota_state.json")

	p1 := newTestProxyAt(t, cfg, statePath)
	p1.providers["p"] = &testProv{key: "k"}
	px1 := httptest.NewServer(http.HandlerFunc(p1.Handler))
	first, err := http.Post(px1.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"g","input":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != 200 || !strings.Contains(string(firstBody), "chat_1") {
		t.Fatalf("first instance response: %d %s", first.StatusCode, firstBody)
	}
	// Drain instance one completely so instance two boots on a quiescent state
	// file (persist is scheduled asynchronously; Close flushes it).
	px1.Close()
	p1.Close()

	p2 := newTestProxyAt(t, cfg, statePath)
	p2.providers["p"] = &testProv{key: "k"}
	px2 := httptest.NewServer(http.HandlerFunc(p2.Handler))
	defer px2.Close()
	second, err := http.Post(px2.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"g","previous_response_id":"chat_1","input":"pong"}`))
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != 200 {
		t.Fatalf("restart continuation status = %d body=%s", second.StatusCode, secondBody)
	}
	if !strings.Contains(string(secondBody), "restored") || hits.Load() != 2 {
		t.Fatalf("previous_response_id did not restore across restart: body=%s hits=%d", secondBody, hits.Load())
	}
}

// ---- protocol_review_integration_test.go ----

func TestForward_Converted4xxUsesClientErrorEnvelope(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unsupported field","type":"invalid_request_error","code":"bad_request"},"request_id":"req_up"}`)
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
		strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "error" || strOf(asMap(out["error"])["message"]) != "unsupported field" {
		t.Fatalf("body is not an Anthropic error envelope: %s", body)
	}
}

// ---- protocol_reasoning_integration_test.go ----

// Pool virtual names (name#id) normalize to the parent's provider id before
// the dialect lookup (providerConfig resolves via parentOf).
func TestParity_ReasoningDialectPooledProvider(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://x"}},
	}
	parentOf := map[string]string{"zhipu#ab12": "zhipu"}
	prov, ok := configdomain.ProviderConfig(cfg, parentOf, "zhipu#ab12")
	if !ok {
		t.Fatal("configdomain.ProviderConfig did not resolve pooled virtual")
	}
	out, err := protocol.ConvertRequestWithOptions(
		[]byte(`{"model":"g","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		protocol.Responses,
		protocol.OpenAI,
		protocol.RequestOptions{
			ImageOK:          true,
			ReasoningDialect: protocol.ReasoningDialect(provider.ChatReasoningMode(prov.Provider)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strOf(asMap(unmarshalMap(t, out)["thinking"])["type"]); got != "enabled" {
		t.Errorf("pooled zhipu#ab12 → thinking = %v, want enabled (parent id zhipu)", got)
	}
}
