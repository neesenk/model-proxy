package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// setHome redirects HOME to a temp dir for tests that exercise providers reading
// credential files from ~/.model-proxy. Returns a restore func. Not safe for
// parallel tests (HOME is process-global); these tests run sequentially.
func setHome(t *testing.T, dir string) func() {
	t.Helper()
	old, had := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	return func() {
		if had {
			os.Setenv("HOME", old)
		} else {
			os.Unsetenv("HOME")
		}
	}
}

// newDeepSeekTestProxy wires a real DeepSeekProvider (via NewProxy/buildProviders)
// against two mock upstreams — openaiURL (openai_base_url) and anthropicURL
// (anthropic_base_url) — with a fake key file under a temp HOME. Returns the
// proxy's test server.
func newDeepSeekTestProxy(t *testing.T, openaiURL, anthropicURL string) *httptest.Server {
	t.Helper()
	tmpHome := t.TempDir()
	t.Cleanup(setHome(t, tmpHome))
	keyFile := filepath.Join(tmpHome, ".model-proxy", "deepseek_apikey.json")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(`{"api_key":"sk-test-ds"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Providers: map[string]Provider{
			"deepseek": {OpenAIBaseURL: openaiURL, AnthropicBaseURL: anthropicURL, Provider: "deepseek"},
		},
		Routes: map[string][]RouteTarget{
			"deepseek-v4-pro": {{Provider: "deepseek", Model: "deepseek-v4-pro"}},
		},
	}
	p := NewProxy(cfg)
	return httptest.NewServer(http.HandlerFunc(p.handler))
}

type dsHit struct {
	path string
	auth string
	xkey string
}

// The proxy forwards by protocol: anthropic requests hit anthropic_base_url,
// openai requests hit openai_base_url. Verifies the protocol→endpoint
// routing that openai_base_url/anthropic_base_url config provides.
func TestForward_DeepSeekRoutesByProtocol(t *testing.T) {
	var openaiHit, anthropicHit dsHit
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHit = dsHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), xkey: r.Header.Get("x-api-key")}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer openaiUp.Close()
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHit = dsHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), xkey: r.Header.Get("x-api-key")}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer anthropicUp.Close()

	px := newDeepSeekTestProxy(t, openaiUp.URL, anthropicUp.URL)
	defer px.Close()

	// 1) anthropic POST /v1/messages → anthropic_base_url, NOT the openai base.
	post(t, px.URL+"/v1/messages", `{"model":"deepseek-v4-pro","messages":[]}`)
	if anthropicHit.path == "" {
		t.Error("anthropic request: expected to hit anthropic upstream")
	}
	if openaiHit.path != "" {
		t.Error("anthropic request: should not hit openai upstream")
	}
	if anthropicHit.path != "/v1/messages" {
		t.Errorf("anthropic upstream path: got %q, want /v1/messages", anthropicHit.path)
	}
	if anthropicHit.auth != "Bearer sk-test-ds" {
		t.Errorf("anthropic Authorization: got %q, want Bearer sk-test-ds", anthropicHit.auth)
	}
	if anthropicHit.xkey != "sk-test-ds" {
		t.Errorf("anthropic x-api-key: got %q, want sk-test-ds", anthropicHit.xkey)
	}

	// 2) openai POST /v1/chat/completions → openai_base_url, NOT anthropic_base_url.
	openaiHit, anthropicHit = dsHit{}, dsHit{}
	post(t, px.URL+"/v1/chat/completions", `{"model":"deepseek-v4-pro","messages":[]}`)
	if openaiHit.path == "" {
		t.Error("openai request: expected to hit openai upstream")
	}
	if anthropicHit.path != "" {
		t.Error("openai request: should not hit anthropic upstream")
	}
	if openaiHit.path != "/chat/completions" {
		t.Errorf("openai upstream path: got %q, want /chat/completions", openaiHit.path)
	}
	// P0-1: assert exact auth values on the openai path too
	if openaiHit.auth != "Bearer sk-test-ds" {
		t.Errorf("openai Authorization: got %q, want Bearer sk-test-ds", openaiHit.auth)
	}
	if openaiHit.xkey != "sk-test-ds" {
		t.Errorf("openai x-api-key: got %q, want sk-test-ds", openaiHit.xkey)
	}
}
