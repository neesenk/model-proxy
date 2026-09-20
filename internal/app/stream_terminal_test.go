package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// The zhipu-incident shape: a same-protocol anthropic passthrough stream whose
// upstream drops the connection mid-thinking — 200 + SSE deltas, then EOF
// without content_block_stop / message_delta / message_stop. The client must
// receive the forwarded bytes plus a synthesized protocol-native error event
// (strict SDKs previously failed on the bare EOF; lenient ones silently
// accepted a truncated answer).
func TestForward_AnthropicPassthroughTruncationSynthesizesErrorEvent(t *testing.T) {
	partial := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"glm-5.3"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"par"}}` + "\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, partial)
		// upstream abandons the generation here: no terminal is ever sent
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"zhipu": {AnthropicBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["zhipu"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/messages",
		strings.NewReader(`{"model":"glm-5.3","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.HasPrefix(string(body), partial) {
		t.Fatalf("forwarded bytes were mutated:\n%s", body)
	}
	want := "event: error\n" +
		`data: {"type":"error","error":{"type":"api_error","message":"upstream stream ended before message_stop"}}` + "\n\n"
	if !strings.HasSuffix(string(body), want) {
		t.Fatalf("stream does not end with the synthesized error event:\ngot  tail: %q\nwant tail: %q",
			string(body)[len(body)-len(want)-40:], want)
	}
	if strings.Contains(string(body), "event: message_stop") {
		t.Fatal("a fake success terminal leaked into the client stream")
	}
}

// Chat-protocol passthrough truncation: deltas without finish_reason and
// without [DONE]. The client must get an error chunk — never a fabricated
// finish_reason or [DONE] (silent truncation).
func TestForward_ChatPassthroughTruncationSynthesizesErrorChunk(t *testing.T) {
	partial := `data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}` + "\n\n" +
		`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{"content":"llo"}}]}` + "\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, partial)
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"gpt-x": {{Provider: "oai", Model: "gpt-x"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.HasPrefix(string(body), partial) {
		t.Fatalf("forwarded bytes were mutated:\n%s", body)
	}
	want := `data: {"error":{"code":null,"message":"upstream stream ended without a terminal finish_reason","param":null,"type":"api_error"}}` + "\n\n"
	if !strings.HasSuffix(string(body), want) {
		t.Fatalf("stream does not end with the synthesized error chunk:\ngot  tail: %q\nwant tail: %q",
			string(body)[len(body)-len(want)-40:], want)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Fatal("a fake [DONE] delimiter leaked into the client stream")
	}
}

// A complete passthrough stream must stay byte-identical: the watcher is a
// scanner, never a rewriter.
func TestForward_AnthropicPassthroughCompleteStreamUntouched(t *testing.T) {
	complete := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"glm-5.3"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, complete)
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"zhipu": {AnthropicBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["zhipu"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/messages",
		strings.NewReader(`{"model":"glm-5.3","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}
	if string(body) != complete {
		t.Fatalf("complete passthrough stream was modified:\n%s", body)
	}
}
