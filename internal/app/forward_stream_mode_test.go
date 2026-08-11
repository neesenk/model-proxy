package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"claude-x","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
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

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(
		`{"model":"codex-x","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
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
