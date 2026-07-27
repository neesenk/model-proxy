package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestSniffSSEFraming: verdicts for the framing sniff, and the body must be
// fully restored afterwards (the commit path re-reads every peeked byte).
func TestSniffSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"event marker", "event: message_start\ndata: {}\n\n", true},
		{"data marker", `data: {"a":1}` + "\n\n", true},
		{"leading whitespace before marker", "\n  event: x\n", true},
		{"id marker", "id: 7\n", true},
		{"retry marker", "retry: 1000\n", true},
		{"json body", `{"id":"c1","choices":[]}`, false},
		{"sse comment heartbeat", ": ping\n\n", true},
		{"empty body", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(tc.body))}
			if got := sniffSSEFraming(resp); got != tc.want {
				t.Errorf("sniffSSEFraming(%q)=%v want %v", tc.body, got, tc.want)
			}
			restored, _ := io.ReadAll(resp.Body)
			if string(restored) != tc.body {
				t.Errorf("body not restored after sniff: got %q want %q", restored, tc.body)
			}
		})
	}
}

// TestSniffSSEFraming_SplitMarker: a short first read that splits inside the
// "event:" marker leaves the verdict undecided, so the sniff keeps reading to
// the window instead of misjudging a stream as JSON.
func TestSniffSSEFraming_SplitMarker(t *testing.T) {
	full := "event: message_start\ndata: {}\n\n"
	resp := &http.Response{Body: &chunkedReader{chunks: []string{"ev", full[2:]}}}
	if !sniffSSEFraming(resp) {
		t.Error("split event: marker must still sniff as SSE")
	}
	restored, _ := io.ReadAll(resp.Body)
	if string(restored) != full {
		t.Errorf("body not restored: got %q want %q", restored, full)
	}
}

func TestSniffSSEFraming_SplitShortFieldNoBlock(t *testing.T) {
	pr, pw := io.Pipe()
	resp := &http.Response{Body: pr}
	done := make(chan bool, 1)
	go func() { done <- sniffSSEFraming(resp) }()
	if _, err := pw.Write([]byte("i")); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("d: 7\n\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !got {
			t.Error("split id field must sniff as SSE")
		}
	case <-time.After(time.Second):
		t.Error("sniff waited to fill the window after a complete short SSE field")
	}
	pw.Close()
}

// TestSniffSSEFraming_HeartbeatNoBlock (E2): an SSE upstream with an empty
// content-type may send a short heartbeat (": ping\n\n") then idle. The
// heartbeat is itself valid SSE framing, so the sniff must return true after the
// first read — blocking to fill the 16-byte window would delay the stream until
// the next event arrives.
func TestSniffSSEFraming_HeartbeatNoBlock(t *testing.T) {
	pr, pw := io.Pipe()
	resp := &http.Response{Body: pr}
	done := make(chan bool, 1)
	go func() { done <- sniffSSEFraming(resp) }()
	if _, err := pw.Write([]byte(": ping\n\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !got {
			t.Error("heartbeat comment must sniff as SSE framing")
		}
	case <-time.After(2 * time.Second):
		t.Error("sniff blocked waiting to fill the window after a decisive first read")
	}
	// The peeked heartbeat must still reach the commit path. io.Pipe writes
	// block until consumed, so feed the rest from a goroutine while ReadAll
	// drains the restored body.
	go func() {
		pw.Write([]byte("event: x\n\n"))
		pw.Close()
	}()
	restored, _ := io.ReadAll(resp.Body)
	if string(restored) != ": ping\n\nevent: x\n\n" {
		t.Errorf("body not restored after sniff: got %q", restored)
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
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

// chunkedReader returns its chunks one per Read, simulating TCP segmentation
// that splits a marker across reads.
type chunkedReader struct {
	chunks []string
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks[0] = c.chunks[0][n:]
	if c.chunks[0] == "" {
		c.chunks = c.chunks[1:]
	}
	return n, nil
}

func (c *chunkedReader) Close() error { return nil }
