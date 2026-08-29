package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
)

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
		"zhipu#" + accounts.AccountID("zhipu", AccountCred{APIKey: "KEY-A"}): "KEY-A",
		"zhipu#" + accounts.AccountID("zhipu", AccountCred{APIKey: "KEY-B"}): "KEY-B",
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
