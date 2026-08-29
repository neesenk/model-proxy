package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	observeevents "model-proxy/internal/observe/events"
)

// fakeUpstream records every request body + path and answers with a
// configurable responder.
type fakeUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	paths  []string
}

func newFakeUpstream(t *testing.T, respond http.HandlerFunc) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeUpstream) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeUpstream) lastPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) == 0 {
		return ""
	}
	return f.paths[len(f.paths)-1]
}

// --- responders ---

func anthropicDraftResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_d","type":"message","role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":11,"output_tokens":7}}`, text)
	}
}

func openaiDraftResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl_d","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":9}}`, text)
	}
}

// responsesDraftResponder answers a native responses-API leg with the
// responses output[] shape (no choices[]/content[] at the top level).
func responsesDraftResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_d","status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":13,"output_tokens":5}}`, text)
	}
}

func statusResponder(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

func delayedResponder(d time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { time.Sleep(d); next(w, r) }
}

// anthropicSSEResponder streams a plain-text anthropic SSE answer.
func anthropicSSEResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"synth\",\"usage\":{\"input_tokens\":50}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

// anthropicToolUseSSEResponder streams an anthropic SSE answer that IS a tool call.
func anthropicToolUseSSEResponder() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"usage\":{\"input_tokens\":50}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\n")
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":12}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

// newFusionRig builds a Proxy whose only route "hard" is a single fusion target
// referencing recipe "recipe". Every provider named in the recipe must have a
// fake upstream in ups; a member/synthesizer with protocol:openai or
// protocol:responses gets an openai_base_url, everything else an
// anthropic_base_url (client is anthropic).
func newFusionRig(t *testing.T, recipe FusionConfig, ups map[string]*fakeUpstream) (*Proxy, *httptest.Server) {
	t.Helper()
	names := map[string]string{} // provider name → effective protocol
	for _, m := range recipe.Panel {
		names[m.Provider] = m.Protocol
	}
	names[recipe.Synthesizer.Provider] = recipe.Synthesizer.Protocol
	if recipe.Judge != nil {
		names[recipe.Judge.Provider] = recipe.Judge.Protocol
	}
	providers := map[string]Provider{}
	for name, proto := range names {
		up := ups[name]
		if up == nil {
			t.Fatalf("no fake upstream for provider %q", name)
		}
		p := Provider{Provider: testProviderID}
		if proto == "openai" || proto == "responses" {
			p.OpenAIBaseURL = up.srv.URL
		} else {
			p.AnthropicBaseURL = up.srv.URL
		}
		providers[name] = p
	}
	cfg := &Config{
		Listen:    "127.0.0.1:1",
		Providers: providers,
		Routes:    map[string][]RouteTarget{"hard": {{Provider: "fusion", Model: "recipe", Priority: 1}}},
		Fusion:    map[string]FusionConfig{"recipe": recipe},
	}
	proxy := newTestProxy(t, cfg)
	t.Cleanup(proxy.Close) // stop the quota tracker; don't leak a poller past the test
	for name := range names {
		proxy.providers[name] = &testProv{key: name}
	}
	px := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(px.Close)
	return proxy, px
}

// postAnthropic posts to the proxy's anthropic endpoint and returns the full
// response body. Streaming reads must fail fast (client timeout) and surface
// read errors instead of silently returning a truncated prefix.
func postAnthropic(t *testing.T, px *httptest.Server, body string) string {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("postAnthropic: read response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("postAnthropic: close response: %v", closeErr)
	}
	return string(b)
}

func recentLiveEvents(p *Proxy) []observeevents.Event {
	return p.events.Snapshot()
}

const fusionClientBody = `{"model":"hard","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"solve X"}]}`
