package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// The default loopback forward surface carries no credential, so a cross-site
// browser page must not be able to spend quota with a CORS simple request
// (LLM-API CSRF): browser-identified cross-origin posts answer 403 before any
// upstream dispatch, while CLI clients (no browser headers) and same-machine
// browser tools (loopback origin) pass through to the route.
func TestForwardBrowserOriginGuard(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","content":"hello"}`))
	}))
	defer up.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	body := `{"model":"glm","input":[{"role":"user","content":"hi"}]}`
	do := func(origin string) int {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := do("http://evil.example"); got != http.StatusForbidden {
		t.Fatalf("cross-origin browser post = %d, want 403", got)
	}
	if hits != 0 {
		t.Fatalf("cross-origin post reached the upstream (%d calls)", hits)
	}
	if got := do("http://localhost:3000"); got != http.StatusOK {
		t.Fatalf("same-machine browser tool = %d, want 200", got)
	}
	if got := do(""); got != http.StatusOK {
		t.Fatalf("plain CLI client = %d, want 200", got)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (loopback browser + CLI)", hits)
	}
}
