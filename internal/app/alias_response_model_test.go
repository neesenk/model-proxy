package app

import (
	"context"
	"encoding/json"
	"io"
	configdomain "model-proxy/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestForward_AliasResponseModelNormalizationE2E pins the end-to-end contract
// of a provider-alias route (exposed kimi-k3 → upstream k3): the REQUEST is
// rewritten to the upstream model, but the RESPONSE's model field is
// normalized back to the called name on every client-facing path — buffered,
// streamed, and cache-hit replay.
func TestForward_AliasResponseModelNormalizationE2E(t *testing.T) {
	hits := 0
	var upstreamModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		upstreamModel = req.Model
		if req.Stream {
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
			io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"k3","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"kimi-k3": {{Provider: "z", Model: "k3"}}},
		Cache:     configdomain.CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	do := func(body string) (string, http.Header) {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, b)
		}
		return string(b), resp.Header
	}

	body := `{"model":"kimi-k3","messages":[{"role":"user","content":"same"}]}`
	first, _ := do(body)
	var parsed struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(first), &parsed); err != nil {
		t.Fatalf("client body does not parse: %v (%s)", err, first)
	}
	if parsed.Model != "kimi-k3" {
		t.Errorf("buffered response model = %q, want called model kimi-k3 (%s)", parsed.Model, first)
	}
	if upstreamModel != "k3" {
		t.Errorf("upstream request model = %q, want rewritten k3", upstreamModel)
	}

	// Cache hit: the replayed bytes carry the called model too, with no
	// second upstream call.
	waitUntil(t, "cache entry after first request", func() bool {
		return p.cache.Stats().Entries == 1
	})
	second, header := do(body)
	if header.Get("x-mp-cache") != "hit" {
		t.Errorf("second request x-mp-cache = %q, want hit", header.Get("x-mp-cache"))
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (second request served from cache)", hits)
	}
	if second != first {
		t.Errorf("cache-hit body differs:\nfirst:  %s\nsecond: %s", first, second)
	}

	// Streaming passthrough: every chunk's model is the called name.
	streamed, _ := do(`{"model":"kimi-k3","stream":true,"messages":[{"role":"user","content":"same"}]}`)
	if strings.Contains(streamed, `"model":"k3"`) {
		t.Errorf("upstream model leaked into client stream: %s", streamed)
	}
	if got := strings.Count(streamed, `"model":"kimi-k3"`); got != 2 {
		t.Errorf("normalized chunks = %d, want 2: %s", got, streamed)
	}
	if !strings.HasSuffix(streamed, "data: [DONE]\n\n") {
		t.Errorf("stream terminator damaged: %q", streamed)
	}
}
