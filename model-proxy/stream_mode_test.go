package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAggregateAndSynthesizeAllProtocolStreams(t *testing.T) {
	cases := []struct {
		proto string
		json  string
		want  string
	}{
		{"openai", `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`, `"content":"hello"`},
		{"anthropic", `{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, `"text":"hello"`},
		{"responses", `{"id":"r1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`, `"text":"hello"`},
	}
	for _, tc := range cases {
		t.Run(tc.proto, func(t *testing.T) {
			stream, err := responseToSSE([]byte(tc.json), tc.proto)
			if err != nil {
				t.Fatal(err)
			}
			aggregated, err := aggregateSSEToResponse(stream, tc.proto)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(aggregated), tc.want) {
				t.Fatalf("round trip missing %s: %s", tc.want, aggregated)
			}
		})
	}
}

func TestForwardAdaptsUpstreamSSEToClientJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"codex-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
